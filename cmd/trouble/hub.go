package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/trouble-agent/trouble/internal/hub"
	"github.com/trouble-agent/trouble/internal/ledger"
	"github.com/trouble-agent/trouble/internal/lifecycle"
	"github.com/trouble-agent/trouble/internal/types"
)

// cmdHub mounts SPEC-13 §2.2's operator surface on the seams the daemon
// already runs: status reads, archive exports, dedup probes, drain consumes.
// The CLI is a SEPARATE PROCESS from the daemon — it never reaches into the
// daemon's memory — so every verb re-resolves config the way every other verb
// does and then reads Redis + the state root directly through internal/hub.
//
// Exit codes are the §2.2 contract, per verb:
//
//	status : 0 ok, 1 degraded
//	archive: 0 ok, 1 failed, 2 refused
//	dedup  : 0 present, 1 absent, 2 Redis unreachable
//	drain  : 0 drained, 1 timeout with pending entries
//
// and the CLI's own usage/config failures keep the suite-wide codes
// (2 usage, 13 refusable condition). `drain` is the ONLY mutating verb: it
// consumes the stream through the ledger and acks strictly after the append;
// `status` and `dedup` write nothing at all, and `archive` writes only when it
// is not a --dry-run.
func cmdHub(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, hubUsage)
		return 2
	}
	switch args[0] {
	case "status":
		return cmdHubStatus(args[1:])
	case "archive":
		return cmdHubArchive(args[1:])
	case "dedup":
		return cmdHubDedup(args[1:])
	case "drain":
		return cmdHubDrain(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown hub verb %q\n\n%s", args[0], hubUsage)
		return 2
	}
}

const hubUsage = `usage:
  trouble hub status [--json]
  trouble hub archive [--dry-run] [--file FILE] [--force]
  trouble hub dedup --key KEY [--json]
  trouble hub drain [--timeout DURATION]

SPEC-13 §2.2. status/dedup are read-only; archive exports closed ledger
generations to the DuckBrain namespace; drain consumes the stream into the
ledger (the pre-migration drain of SPEC-12 §3.6) and never deletes an un-acked
entry.
`

// hubContext is what every hub verb resolves before it acts: the resolved
// config, the §4.1 profile gate and the two local roots the hub's seams need.
type hubContext struct {
	res       lifecycle.Resolved
	gate      hub.ProfileGate
	stateRoot string
	now       time.Time
}

// resolveHubContext resolves config + gate + paths, printing the suite's
// config-refusal error and returning exit 13 on failure. stateRoot is the
// RESOLVED state root (SPEC-12 §3.2), or "" when it is not usable on this
// host — a status verb may still degrade honestly without one.
func resolveHubContext(args []string) (hubContext, int) {
	res, code := resolve(args)
	if code != 0 {
		return hubContext{}, code
	}
	gate, err := hub.Gate(res.Config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hub: %v\n", err)
		return hubContext{}, 13
	}
	// The state root carries the same SPEC-12 §3.2 rules the daemon boot
	// enforces; the verbs that only read Redis survive without it and the
	// verbs that touch state refuse. A missing root degrades: it is what a
	// not-yet-booted host looks like.
	var stateRoot string
	if root, err := lifecycle.CheckStateRoot(res.Config); err == nil {
		stateRoot = root.Path
	}
	return hubContext{res: res, gate: gate, stateRoot: stateRoot, now: time.Now()}, 0
}

// ---------------------------------------------------------------------------
// trouble hub status — SPEC-13 §2.2 row 1 (0 ok, 1 degraded; writes nothing)
// ---------------------------------------------------------------------------

func cmdHubStatus(args []string) int {
	fs := flag.NewFlagSet("hub status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	asJSON := fs.Bool("json", false, "machine-readable HubStatus")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	hc, code := resolveHubContext(fs.Args())
	if code != 0 {
		return code
	}
	st, degraded := hubStatusSnapshot(hc, hc.now)
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(st); err != nil {
			fmt.Fprintf(os.Stderr, "hub status: %v\n", err)
			return 13
		}
	} else {
		printHubStatus(st)
	}
	if degraded {
		return 1
	}
	return 0
}

// hubStatusSnapshot assembles the HubStatus WITHOUT a runtime: it reads the
// live Redis (OpenRedis → the §2.1.1 preflight) and the state root directly,
// through the same readers the daemon's health surface uses. The profile state
// file is only READ here; a verb that wrote hub state would break §2.2's
// "writes nothing" and, worse, fight a running daemon's writer.
//
// The degraded answer is not an error path: a hub that cannot see Redis still
// reports what it knows (its profile, its archival files) plus the §4.3
// reason, and the exit code is the machine-readable half of that statement.
func hubStatusSnapshot(hc hubContext, now time.Time) (types.HubStatus, bool) {
	profile := hc.gate.Profile
	if profile == "" {
		profile = hub.ProfileStandalone
	}
	since := types.FormatUTC(now)
	if ps, err := hub.ReadProfileState(hc.stateRoot); err == nil && ps.Since != "" {
		since = ps.Since
	}
	st := hub.StandaloneStatus(profile, since)
	if !hc.gate.Enabled {
		return st, false
	}
	// Archival facts are file facts: they are readable whether or not Redis
	// answers, and they are what an operator planning a drain needs.
	if hc.stateRoot != "" {
		if q, err := hub.ArchiveQueue(hc.stateRoot); err == nil {
			st.ArchiveQueue = q.Depth()
			st.ArchiveLastTS = q.LastTS()
		}
		if mk := hub.NewMarkerStore(hc.stateRoot); mk != nil {
			_, pending, exported, verified, _, _ := mk.Summarize()
			st.MarkerPending = pending
			st.ArchivedFiles = exported + verified
		}
		if droppable, err := hub.DroppableGenerationsIn(
			hub.LedgerRootFromState(hc.stateRoot), hc.stateRoot, "", hc.gate.Archive.KeepLocalGens); err == nil {
			st.DroppableGens = len(droppable)
		}
	}
	client, err := openHubRedis(hc)
	if err != nil {
		// Degrade: the queue is unreachable from HERE. The reason is the §4.3
		// token for the code (002 auth, 003 unreachable, 004 write-refused).
		code := hub.CodeOf(err)
		if code == "" {
			if hub.IsAuthError(err) {
				code = types.CodeHub002
			} else {
				code = types.CodeHub003
			}
		}
		st.Degraded = true
		st.DegradedReason = hub.DegradedReasonFor(code, hc.gate.Redis.RequireRedis)
		st.Redis.Degraded = true
		st.Redis.DegradedReason = st.DegradedReason
		st.Redis.Stream = hc.gate.Redis.Stream
		st.Redis.Group = hc.gate.Redis.Group
		return st, true
	}
	defer client.Close()
	st.Redis = hubOffsets(context.Background(), client, hc.gate.Redis)
	st.Enabled = true
	if st.Redis.Degraded {
		st.Degraded = true
		st.DegradedReason = st.Redis.DegradedReason
	}
	return st, st.Degraded
}

// hubOffsets is the read-only offsets projection: the same fields the runtime
// stamps into /health.json, read fresh by the CLI process. It asks the client
// for the stream length, the group row and the pending summary; any read that
// fails degrades the field set and the stanza says so.
func hubOffsets(ctx context.Context, client *hub.Client, cfg hub.RedisConfig) types.RedisStreamOffsets {
	out := types.RedisStreamOffsets{
		Stream:   cfg.Stream,
		Group:    cfg.Group,
		Consumer: cfg.Consumer,
		Since:    types.FormatUTC(time.Now()),
	}
	info := client.ServerInfo()
	out.OptionsChecked = info.OptionsChecked
	out.AOF = info.AOFEnabled
	out.Policy = info.Policy
	out.EvictedKeys = info.EvictedKeys
	if n, err := client.StreamLen(ctx); err == nil {
		out.StreamLen = n
	}
	if gi, err := client.GroupInfo(ctx); err == nil {
		out.Pending = gi.Pending
		out.Lag = gi.Lag
		out.LastDeliveredID = gi.LastDeliveredID
		if out.LastDeliveredID == "" && out.StreamLen == 0 && out.Pending == 0 {
			// §2.1.1 rule 5: an empty queue with no deliveries is the COLD
			// state the status verb owes the operator ("redis.state = cold").
			out.State = "cold"
		}
	}
	if pending, err := client.Pending(ctx); err == nil {
		out.Pending = pending.Count
	}
	return out
}

// ---------------------------------------------------------------------------
// trouble hub archive — SPEC-13 §2.2 row 2 (0 ok, 1 failed, 2 refused)
// ---------------------------------------------------------------------------

func cmdHubArchive(args []string) int {
	fs := flag.NewFlagSet("hub archive", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dryRun := fs.Bool("dry-run", false, "print the ArchivePlan and write nothing")
	one := fs.String("file", "", "archive this generation only (default: every closed generation without an exported marker)")
	force := fs.Bool("force", false, "export even when the archival target is not configured for verification")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	hc, code := resolveHubContext(fs.Args())
	if code != 0 {
		return code
	}
	if hc.stateRoot == "" {
		fmt.Fprintln(os.Stderr, "hub archive: no usable state root; refusing to write archival state (run on the host whose state root this is)")
		return 2
	}
	cfg := hc.gate.Archive
	cfg.StateRoot = hc.stateRoot
	cfg.LedgerRoot = hub.LedgerRootFromState(hc.stateRoot)
	// The live file is the daemon writer's fact, not the CLI's: HEAD is the
	// writer's own hint (it survives a STOPPED daemon, which is exactly the
	// pre-migration state this verb runs in). With no HEAD and candidates
	// present, PlanArchive refuses (011) rather than guessing — --force
	// overrides that by asserting the NEWEST file is the live one.
	cfg.LiveFile = liveFileFromHead(cfg.LedgerRoot)
	if cfg.LiveFile == "" && *force {
		if newest := newestLedgerFile(cfg.LedgerRoot); newest != "" {
			cfg.LiveFile = newest
		}
	}
	// A state root whose ledger dir does not exist yet (the daemon never
	// booted, or the hub tree was created by hand) has nothing to archive —
	// that is the ok-and-empty row, not a failure.
	if fi, err := os.Stat(cfg.LedgerRoot); err != nil || !fi.IsDir() {
		fmt.Fprintln(os.Stderr, "hub archive: nothing to archive (no ledger generations under the state root)")
		return 0
	}
	plans, err := hub.PlanArchive(hc.stateRoot, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hub archive: %v\n", err)
		if hub.CodeOf(err) == types.CodeHub011 {
			return 2
		}
		return 1
	}
	if name := *one; name != "" {
		base := filepath.Base(name)
		var kept []hub.ArchivePlan
		for _, p := range plans {
			if p.File == base {
				kept = append(kept, p)
			}
		}
		plans = kept
	}
	if len(plans) == 0 {
		fmt.Fprintln(os.Stderr, "hub archive: nothing to archive (every closed generation has an exported marker)")
		return 0
	}
	if *dryRun {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		for _, p := range plans {
			if err := enc.Encode(p); err != nil {
				fmt.Fprintf(os.Stderr, "hub archive: %v\n", err)
				return 13
			}
		}
		return 0
	}
	// The real export goes through hub.Archive, which builds its own target
	// from the resolved config (009/010/012 are its vocabulary).
	failed := 0
	for _, p := range plans {
		mk, err := hub.Archive(context.Background(), cfg, p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hub archive: %s: %v\n", p.File, err)
			failed++
			continue
		}
		fmt.Fprintf(os.Stdout, "%s -> %s (%s, %d bytes)\n", p.File, mk.ObjectKey, mk.State, mk.Bytes)
	}
	if failed > 0 {
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------------
// trouble hub dedup — SPEC-13 §2.2 row 3 (0 present, 1 absent, 2 unreachable)
// ---------------------------------------------------------------------------

func cmdHubDedup(args []string) int {
	fs := flag.NewFlagSet("hub dedup", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	key := fs.String("key", "", "the idempotency key to probe (required)")
	asJSON := fs.Bool("json", false, "machine-readable {key, state, ttl_seconds}")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*key) == "" {
		fmt.Fprint(os.Stderr, "usage: trouble hub dedup --key KEY [--json]\n")
		return 2
	}
	hc, code := resolveHubContext(fs.Args())
	if code != 0 {
		return code
	}
	client, err := openHubRedis(hc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hub dedup: redis is unreachable: %v\n", err)
		return 2
	}
	defer client.Close()
	// The probe is the gate's own read path (§3.4): the same key grammar, the
	// same GET, and TTL as the only extra fact. Nothing is claimed or
	// revealed but presence (+TTL) — no values cross this process.
	gate := hub.NewDedupGate(hc.gate.Redis, client)
	state, err := gate.State(context.Background(), *key)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hub dedup: redis is unreachable: %v\n", err)
		return 2
	}
	ttl := hub.RemainingTTL(context.Background(), client.Streams(), *key)
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{
			"key":         *key,
			"state":       state.String(),
			"ttl_seconds": int64(ttl.Seconds()),
		})
	} else {
		switch state {
		case hub.DedupAbsent:
			fmt.Printf("%s absent\n", *key)
		case hub.DedupEnqueued:
			fmt.Printf("%s present (enqueued, ttl %s)\n", *key, ttl.Round(time.Second))
		case hub.DedupAppended:
			fmt.Printf("%s present (appended, ttl %s)\n", *key, ttl.Round(time.Second))
		}
	}
	if state == hub.DedupAbsent {
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------------
// trouble hub drain — SPEC-13 §2.2 row 4 (0 drained, 1 timeout with pending)
// ---------------------------------------------------------------------------

func cmdHubDrain(args []string) int {
	fs := flag.NewFlagSet("hub drain", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	timeout := fs.Duration("timeout", 30*time.Second, "give up after this long with entries still pending")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	hc, code := resolveHubContext(fs.Args())
	if code != 0 {
		return code
	}
	if hc.stateRoot == "" {
		fmt.Fprintln(os.Stderr, "hub drain: no usable state root; refusing (SPEC-12 §3.2)")
		return 2
	}
	// The ledger is opened in WRITER mode: drain is the one mutating verb and
	// it moves entries INTO the ledger (append → fsync → ack, §3.3). Sharing
	// the ledger with a running daemon is impossible by construction (the
	// LOCK file) — which is exactly the safety the pre-migration drain wants.
	hostID := hc.res.Config.Origin.HostID
	if hostID == "" {
		hostID = "unknown-host"
	}
	led, err := ledger.Open(context.Background(), ledger.Options{
		Root:           filepath.Join(hc.stateRoot, "ledger"),
		Rotation:       ledger.DefaultRotationPolicy(),
		Retention:      ledger.DefaultRetentionPolicy(),
		Index:          ledger.DefaultIndexOptions(),
		Writer:         lifecycle.Actor(types.ActorDaemon, "trouble-hub-drain"),
		MaxSchema:      ledger.SchemaVersionV1,
		Now:            time.Now,
		HostID:         hostID,
		Zone:           "loopback",
		AssertScrubbed: true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hub drain: the ledger could not be opened (is the daemon running?): %v\n", err)
		return 13
	}
	defer led.Close(context.Background())
	client, err := openHubRedis(hc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hub drain: redis is unreachable: %v\n", err)
		return 2
	}
	defer client.Close()
	if err := client.EnsureGroup(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "hub drain: %v\n", err)
		return 2
	}
	cfg := client.Config()
	// A one-shot drain carries no dedup gate of its own: the consumer appends
	// every delivery, and the LEDGER's idempotency check is the arbiter (the
	// same configuration Consume's doc pins for drain-style consumption).
	cs := hub.NewConsumer(client, led, nil, cfg)
	stats, err := cs.Drain(context.Background(), *timeout)
	if err != nil {
		// §2.2: the timeout row — pending entries remain, exit 1. The daemon
		// (or a re-run) re-delivers them; nothing was acked un-appended.
		fmt.Fprintf(os.Stderr, "hub drain: timed out with pending entries: %v\n", err)
		fmt.Fprintf(os.Stderr, "  acked=%d appended=%d dedup_skips=%d decode_errors=%d reclaims=%d\n",
			stats.Acked, stats.Appended, stats.DedupSkips, stats.DecodeErrors, stats.Reclaims)
		return 1
	}
	fmt.Fprintf(os.Stdout, "drained: acked=%d appended=%d dedup_skips=%d decode_errors=%d reclaims=%d\n",
		stats.Acked, stats.Appended, stats.DedupSkips, stats.DecodeErrors, stats.Reclaims)
	return 0
}

// openHubRedis dials the resolved Redis for a hub verb. It is the CLI's own
// boot: the same preflight the daemon runs, without the consumer group.
func openHubRedis(hc hubContext) (*hub.Client, error) {
	cfg := hc.gate.Redis
	if cfg.URL == "" {
		return nil, fmt.Errorf("server.redis.url is not configured (profile %q)", hc.gate.Profile)
	}
	return hub.OpenRedis(context.Background(), cfg)
}

// liveFileFromHead reads the ledger writer's HEAD hint (SPEC-01 §3.4: a hint,
// never a source of truth) and returns the file name it points at. The CLI is
// never the writer, so this hint is how a STOPPED daemon's live file stays
// excluded from the archival plan; a missing or torn HEAD yields "" and
// PlanArchive then refuses rather than guessing.
func liveFileFromHead(ledgerRoot string) string {
	b, err := os.ReadFile(filepath.Join(ledgerRoot, "HEAD"))
	if err != nil {
		return ""
	}
	var h struct {
		File string `json:"file"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(b))), &h); err != nil {
		return ""
	}
	return h.File
}

// newestLedgerFile names the newest-modified ledger generation, for --force
// only: the operator's assertion "that newest file is the live one".
func newestLedgerFile(ledgerRoot string) string {
	entries, err := os.ReadDir(ledgerRoot)
	if err != nil {
		return ""
	}
	var (
		newest    string
		newestMod time.Time
	)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if newest == "" || info.ModTime().After(newestMod) {
			newest, newestMod = e.Name(), info.ModTime()
		}
	}
	return newest
}

// printHubStatus renders the stanza for humans. The JSON form is the machine
// contract; the text form names the same fields in the same §2.2 order.
func printHubStatus(st types.HubStatus) {
	fmt.Printf("profile: %s (enabled=%t) since=%s\n", st.Profile, st.Enabled, st.Since)
	fmt.Printf("redis:   stream=%s group=%s consumer=%s len=%d pending=%d lag=%d last_delivered=%s state=%s\n",
		st.Redis.Stream, st.Redis.Group, st.Redis.Consumer,
		st.Redis.StreamLen, st.Redis.Pending, st.Redis.Lag, st.Redis.LastDeliveredID,
		redisStateLabel(st.Redis))
	fmt.Printf("dedup:   hits=%d misses=%d conflicts=%d restored=%d window=%s\n",
		st.Redis.DedupHits, st.Redis.DedupMisses, st.Redis.DedupConflicts, st.Redis.DedupRestored, st.Redis.DedupWindow)
	fmt.Printf("archive: queue=%d last_ts=%s files=%d droppable=%d marker_pending=%d\n",
		st.ArchiveQueue, st.ArchiveLastTS, st.ArchivedFiles, st.DroppableGens, st.MarkerPending)
	if st.Degraded {
		fmt.Printf("degraded: %s\n", st.DegradedReason)
	}
}

// redisStateLabel is the §2.1.1 rule-5 posture for the text output: "cold"
// while the queue is empty and undelivered, "degraded" when the stanza says
// so, otherwise "warm".
func redisStateLabel(st types.RedisStreamOffsets) string {
	if st.Degraded {
		return "degraded"
	}
	if st.State == "cold" || hub.ColdStreamState(st) {
		return "cold"
	}
	return "warm"
}
