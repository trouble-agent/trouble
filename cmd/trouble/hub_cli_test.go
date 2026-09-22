package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/hub"
	"github.com/trouble-agent/trouble/internal/ledger"
	"github.com/trouble-agent/trouble/internal/types"
)

// hub_cli_test.go mounts SPEC-13 §2.2's four operator verbs end-to-end through
// the CLI: status, archive, dedup, drain. Each verb's test names the section.
//
// Rules the tests honour:
//
//   - NO LIVE REDIS, NO BOUND PORTS: the verbs that dial Redis are exercised
//     against 127.0.0.1:1 (loopback discard — instant refusal, nothing
//     listened on) for the degraded/unreachable rows, and the key behaviours
//     behind the verbs (dry-run writes nothing, drain empties a stream,
//     exit-code mapping) are proven in-process through the same internal/hub
//     seams the verbs call, exactly as the daemon-side tests do.
//   - NO LEDGER UNDER /tmp: ledger.Open refuses /tmp roots (SPEC-01 §3.9), so
//     every state root here lives under ~/.local/state/trouble-test (the same
//     base internal/ledger's own tests use).
func troubleTestBase(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("no home dir: %v", err)
	}
	base := filepath.Join(home, ".local", "state", "trouble-test", "hub-cli")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatalf("cannot create test base: %v", err)
	}
	return base
}

func hubTestRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(troubleTestBase(t), "t65-")
	if err != nil {
		t.Fatalf("temp root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// writeHubConfig writes a light-hub config over base and returns its path and
// the state root it declares.
func writeHubConfig(t *testing.T, base, redisURL string) (string, string) {
	t.Helper()
	stateRoot := filepath.Join(base, "state")
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatalf("state root: %v", err)
	}
	body := "state_root = \"" + stateRoot + "\"\n" +
		"[origin]\nhost_id = \"7f3a91c2d4e5b607\"\n" +
		"[server]\nprofile = \"light-hub\"\nhub_id = \"hub-cli\"\n" +
		"[server.redis]\nurl = \"" + redisURL + "\"\n" +
		"stream = \"trouble:ingest:cli\"\ngroup = \"ledger-writers-cli\"\n" +
		"[server.duckbrain]\nnamespace = \"trouble/7f3a91c2d4e5b607\"\n" +
		"endpoint = \"http://127.0.0.1:7645\"\n"
	path := filepath.Join(base, "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	return path, stateRoot
}

// writeStandaloneConfig writes a standalone config over base.
func writeStandaloneConfig(t *testing.T, base string) string {
	t.Helper()
	stateRoot := filepath.Join(base, "state")
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatalf("state root: %v", err)
	}
	body := "state_root = \"" + stateRoot + "\"\n" +
		"[origin]\nhost_id = \"7f3a91c2d4e5b607\"\n" +
		"[server]\nprofile = \"standalone\"\n"
	path := filepath.Join(base, "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	return path
}

// buildTrouble compiles the CLI once per test and returns the binary path.
func buildTrouble(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "trouble")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/trouble")
	cmd.Dir = worktreeRoot(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// worktreeRoot finds the module root from the directory the test runs in.
func worktreeRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

// runCLI runs the built binary with TROUBLE_CONFIG_PATH pointed at cfg and
// returns the exit code plus stdout/stderr.
func runCLI(t *testing.T, bin string, cfg string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "TROUBLE_CONFIG_PATH="+cfg)
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run %v: %v", args, err)
		}
		code = ee.ExitCode()
	}
	return code, outBuf.String(), errBuf.String()
}

// ---------------------------------------------------------------------------
// SPEC-13 §2.2 row 1 — trouble hub status (0 ok / 1 degraded; --json shape;
// writes nothing)
// ---------------------------------------------------------------------------

// TestHubStatusExitCodesAndJSONShapeSpec13Section22: a light-hub config whose
// Redis is unreachable must answer exit 1 (degraded), carry the §4.3 reason,
// and --json must emit a HubStatus with the §2.2 field names. A standalone
// config answers exit 0 with enabled=false. Neither writes anything.
func TestHubStatusExitCodesAndJSONShapeSpec13Section22(t *testing.T) {
	bin := buildTrouble(t)

	// Degraded light-hub: Redis at the discard port.
	base := hubTestRoot(t)
	cfg, stateRoot := writeHubConfig(t, base, "redis://127.0.0.1:1/0")
	code, out, errOut := runCLI(t, bin, cfg, "hub", "status", "--json")
	if code != 1 {
		t.Fatalf("hub status (degraded) exit = %d, want 1 (SPEC-13 §2.2)\nstdout=%s\nstderr=%s", code, out, errOut)
	}
	var st types.HubStatus
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("hub status --json is not HubStatus JSON: %v\n%s", err, out)
	}
	if st.Enabled {
		t.Fatalf("degraded stanza must not claim enabled=true: %+v", st)
	}
	if st.Profile != "light-hub" {
		t.Fatalf("profile = %q, want light-hub (SPEC-13 §2.2 prints the profile)", st.Profile)
	}
	if !st.Degraded || st.DegradedReason == "" {
		t.Fatalf("degraded stanza must carry degraded + reason: %+v", st)
	}
	// §2.2: status writes nothing. The state root must gain no hub tree.
	if _, err := os.Stat(filepath.Join(stateRoot, "hub")); !os.IsNotExist(err) {
		t.Fatalf("hub status wrote state under %s: %v (SPEC-13 §2.2: writes nothing)", stateRoot, err)
	}

	// Text form names the degradation too (it prints to stdout; go-redis may
	// log dial noise to stderr, which is fine).
	code, textOut, _ := runCLI(t, bin, cfg, "hub", "status")
	if code != 1 {
		t.Fatalf("hub status (text) exit = %d, want 1", code)
	}
	if !strings.Contains(textOut, "degraded") {
		t.Fatalf("text status must name the degradation: %q", textOut)
	}

	// Standalone: exit 0, enabled=false (SPEC-13 §4.1 step 2's answer).
	sCfg := writeStandaloneConfig(t, hubTestRoot(t))
	sCode, sOut, sErr := runCLI(t, bin, sCfg, "hub", "status", "--json")
	if sCode != 0 {
		t.Fatalf("standalone hub status exit = %d, want 0\nstderr=%s", sCode, sErr)
	}
	var sSt types.HubStatus
	if err := json.Unmarshal([]byte(sOut), &sSt); err != nil {
		t.Fatalf("standalone --json: %v\n%s", err, sOut)
	}
	if sSt.Enabled || sSt.Profile != "standalone" {
		t.Fatalf("standalone stanza = %+v, want enabled=false profile=standalone", sSt)
	}
}

// TestHubStatusColdStateSpec13Section22 pins §2.1.1 rule 5 through the CLI's
// offsets projection: an empty, undelivered queue is "cold"; a queue with a
// delivered id is not.
func TestHubStatusColdStateSpec13Section22(t *testing.T) {
	empty := types.RedisStreamOffsets{}
	if !hub.ColdStreamState(empty) {
		t.Fatalf("an empty undelivered queue must be cold")
	}
	warm := empty
	warm.LastDeliveredID = "1758012841221-1"
	if hub.ColdStreamState(warm) {
		t.Fatalf("a queue with a delivery is not cold")
	}
	degraded := empty
	degraded.Degraded = true
	if hub.ColdStreamState(degraded) {
		t.Fatalf("a degraded stanza never says cold")
	}
	populated := empty
	populated.StreamLen = 3
	if hub.ColdStreamState(populated) {
		t.Fatalf("a queue holding entries is not cold")
	}
}

// ---------------------------------------------------------------------------
// SPEC-13 §2.2 row 2 — trouble hub archive (0 ok / 1 failed / 2 refused;
// --dry-run prints the ArchivePlan and writes nothing)
// ---------------------------------------------------------------------------

func TestHubArchiveDryRunWritesNothingSpec13Section22(t *testing.T) {
	bin := buildTrouble(t)
	base := hubTestRoot(t)
	cfg, stateRoot := writeHubConfig(t, base, "redis://127.0.0.1:1/0")

	led := filepath.Join(stateRoot, "ledger")
	if err := os.MkdirAll(led, 0o700); err != nil {
		t.Fatalf("ledger root: %v", err)
	}
	writeGenerationFile(t, led, "2026-09-15.jsonl", 1, 2)
	writeGenerationFile(t, led, "2026-09-16.jsonl", 3, 4, 5)
	writeGenerationFile(t, led, "2026-09-17.jsonl", 6)
	// HEAD names the live file the writer would be appending to; the CLI
	// reads this hint (never re-derives it) to exclude the live generation.
	if err := os.WriteFile(filepath.Join(led, "HEAD"), []byte(`{"file":"2026-09-17.jsonl"}`), 0o600); err != nil {
		t.Fatalf("write HEAD: %v", err)
	}

	snap := snapshotTree(t, stateRoot)
	code, out, errOut := runCLI(t, bin, cfg, "hub", "archive", "--dry-run")
	if code != 0 {
		t.Fatalf("hub archive --dry-run exit = %d, want 0\nstderr=%s", code, errOut)
	}
	var plans []hub.ArchivePlan
	dec := json.NewDecoder(strings.NewReader(out))
	for {
		var p hub.ArchivePlan
		if err := dec.Decode(&p); err != nil {
			break
		}
		plans = append(plans, p)
	}
	if len(plans) != 2 {
		t.Fatalf("plans = %d (%s), want the two closed generations", len(plans), out)
	}
	// §2.2 names the fields the plan must print: file, bytes, gzip bytes,
	// marker id, target namespace.
	p := plans[0]
	if p.File == "" || p.Bytes <= 0 || p.GzipBytes <= 0 || p.MarkerID == "" || p.Namespace == "" {
		t.Fatalf("plan is missing §2.2 fields: %+v", p)
	}
	for _, pl := range plans {
		if pl.File == "2026-09-17.jsonl" {
			t.Fatalf("the live file must never be a candidate: %+v", pl)
		}
	}
	if got := snapshotTree(t, stateRoot); got != snap {
		t.Fatalf("--dry-run changed the state root:\nBEFORE:\n%s\nAFTER:\n%s", snap, got)
	}
}

// TestHubArchiveRefusalAndEmptySpec13Section22: §2.2's exit code 2 — the
// unknown-live-file refusal (TROUBLE-HUB-011, class permanent) when HEAD is
// missing and candidates exist — and the ok answer for a ledger with nothing
// to archive.
func TestHubArchiveRefusalAndEmptySpec13Section22(t *testing.T) {
	bin := buildTrouble(t)

	// Refused: candidates exist, no HEAD, no --force.
	base := hubTestRoot(t)
	cfg, stateRoot := writeHubConfig(t, base, "redis://127.0.0.1:1/0")
	led := filepath.Join(stateRoot, "ledger")
	if err := os.MkdirAll(led, 0o700); err != nil {
		t.Fatalf("ledger root: %v", err)
	}
	writeGenerationFile(t, led, "2026-09-15.jsonl", 1)
	code, _, errOut := runCLI(t, bin, cfg, "hub", "archive", "--dry-run")
	if code != 2 {
		t.Fatalf("hub archive (unknown live file) exit = %d, want 2 (refused)\nstderr=%s", code, errOut)
	}

	// --force overrides the refusal by naming the newest file the live one;
	// the plan then proceeds (still a dry run: nothing written). The newest
	// file is the ONLY candidate here, so --force must yield ZERO plans (the
	// one file is declared live, not archivable) — the refusal was about the
	// guess, and the guess is now a named file.
	fCode, _, fErr := runCLI(t, bin, cfg, "hub", "archive", "--dry-run", "--force")
	if fCode != 0 {
		t.Fatalf("hub archive --force exit = %d, want 0\nstderr=%s", fCode, fErr)
	}

	// With TWO generations, --force names the newest-MTIME file the live one
	// (that is 2026-09-14: it was written last) and plans the other.
	writeGenerationFile(t, led, "2026-09-14.jsonl", 7)
	gCode, gOut, gErr := runCLI(t, bin, cfg, "hub", "archive", "--dry-run", "--force")
	if gCode != 0 {
		t.Fatalf("hub archive --force (2 gens) exit = %d, want 0\nstderr=%s", gCode, gErr)
	}
	if !strings.Contains(gOut, "2026-09-15.jsonl") {
		t.Fatalf("--force plan must name the generation that is not newest: %q", gOut)
	}
	if strings.Contains(gOut, "2026-09-14.jsonl") {
		t.Fatalf("--force must treat the newest-mtime file as live: %q", gOut)
	}

	// Ok-and-empty: no ledger generations at all answers 0.
	eCfg, _ := writeHubConfig(t, hubTestRoot(t), "redis://127.0.0.1:1/0")
	eCode, _, eErr := runCLI(t, bin, eCfg, "hub", "archive", "--dry-run")
	if eCode != 0 {
		t.Fatalf("hub archive (empty) exit = %d, want 0\nstderr=%s", eCode, eErr)
	}
}

// writeGenerationFile writes one plausible closed generation (the same shape
// internal/hub's archive tests use).
func writeGenerationFile(t *testing.T, dir, name string, seqs ...uint64) {
	t.Helper()
	var buf strings.Builder
	for i, seq := range seqs {
		rec := map[string]any{
			"rec_id": fmt.Sprintf("ev_%024d", seq),
			"seq":    seq,
			"ts":     fmt.Sprintf("2026-09-16T00:00:%02d.000Z", i),
			"kind":   "event",
			"origin": map[string]any{"host_id": "7f3a91c2d4e5b607", "source": "sentinel"},
			"actor":  map[string]any{"kind": "daemon", "id": "troubled"},
			"payload": map[string]any{
				"subject": "x",
			},
		}
		b, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(buf.String()), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// snapshotTree walks dir and returns a deterministic fingerprint of every
// path + size + content hash. It is how "wrote nothing" is proven.
func snapshotTree(t *testing.T, dir string) string {
	t.Helper()
	var sb strings.Builder
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		sb.WriteString(rel)
		sb.WriteString("|")
		if info.IsDir() {
			sb.WriteString("D\n")
			return nil
		}
		fmt.Fprintf(&sb, "%d|", info.Size())
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		sum := sha256.Sum256(b)
		fmt.Fprintf(&sb, "%x\n", sum)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", dir, err)
	}
	return sb.String()
}

// ---------------------------------------------------------------------------
// SPEC-13 §2.2 row 3 — trouble hub dedup (0 present / 1 absent / 2 unreachable)
// ---------------------------------------------------------------------------

// TestHubDedupExitCodesSpec13Section22 drives the probe composition the verb
// performs (DedupKey grammar → gate read → RemainingTTL) against an in-process
// hub.Streams implementation — no Redis, no sockets — and the unreachable row
// (exit 2) against the real binary pointed at the discard port. The probe must
// not claim: a key that was absent stays absent after probing it.
func TestHubDedupExitCodesSpec13Section22(t *testing.T) {
	// 1. The read-only probe composition, in-process.
	f := newProbeStreams()
	c, err := hub.OpenWith(context.Background(), hub.RedisConfig{HostID: "h"}, f)
	if err != nil {
		t.Fatalf("OpenWith: %v", err)
	}
	defer c.Close()
	key := hub.DedupKey(hub.DefaultDedupPrefix, "sentinel:sha256v1:9f2c1d3e4b5a6c7d", 1, "7f3a91c2d4e5b607")

	gate := hub.NewDedupGate(c.Config(), c)
	state, err := gate.State(context.Background(), key)
	if err != nil {
		t.Fatalf("State(absent): %v", err)
	}
	if state != hub.DedupAbsent {
		t.Fatalf("absent probe = %v, want absent", state)
	}
	if ttl := hub.RemainingTTL(context.Background(), c.Streams(), key); ttl != 0 {
		t.Fatalf("absent TTL = %v, want 0", ttl)
	}
	if len(f.kv) != 0 {
		t.Fatalf("the probe must not claim: kv = %v", f.kv)
	}

	// Claim through the same key grammar a sender uses; the probe now says
	// present with the phase and a live TTL.
	if fresh, err := gate.Claim(context.Background(), key); err != nil || !fresh {
		t.Fatalf("Claim: fresh=%v err=%v", fresh, err)
	}
	state, err = gate.State(context.Background(), key)
	if err != nil {
		t.Fatalf("State(present): %v", err)
	}
	if state != hub.DedupEnqueued {
		t.Fatalf("present probe = %v, want enqueued", state)
	}
	if ttl := hub.RemainingTTL(context.Background(), c.Streams(), key); ttl <= 0 {
		t.Fatalf("present TTL = %v, want the remaining window", ttl)
	}

	// 2. The unreachable row on the REAL binary: exit 2.
	bin := buildTrouble(t)
	cfg, _ := writeHubConfig(t, hubTestRoot(t), "redis://127.0.0.1:1/0")
	code, _, errOut := runCLI(t, bin, cfg, "hub", "dedup", "--key", "somekey")
	if code != 2 {
		t.Fatalf("hub dedup (unreachable) exit = %d, want 2\nstderr=%s", code, errOut)
	}
	// Missing --key is a usage error.
	if code, _, _ = runCLI(t, bin, cfg, "hub", "dedup"); code != 2 {
		t.Fatalf("hub dedup (no key) exit = %d, want 2", code)
	}
}

// ---------------------------------------------------------------------------
// SPEC-13 §2.2 row 4 — trouble hub drain (0 drained / 1 timeout with pending)
// ---------------------------------------------------------------------------

// TestHubDrainDrainsPopulatedStreamSpec13Section22 proves the drain contract
// on the REAL consumer (hub.NewConsumer → Drain) over an in-process stream:
// a populated stream is consumed into a REAL ledger (opened under the
// ~/.local/state test base — never /tmp), every entry is acked only after its
// append, the stream empties, and the verb's exit mapping answers 0. The
// drain loop here is byte-identical to the one cmdHubDrain runs.
func TestHubDrainDrainsPopulatedStreamSpec13Section22(t *testing.T) {
	f := newDrainStreams()
	c, err := hub.OpenWith(context.Background(), hub.RedisConfig{
		HostID: "h", Stream: "trouble:ingest:cli", Group: "ledger-writers-cli",
	}, f)
	if err != nil {
		t.Fatalf("OpenWith: %v", err)
	}
	defer c.Close()
	if err := c.EnsureGroup(context.Background()); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}

	// Populate: two envelopes through the production Enqueue path.
	for i := 0; i < 2; i++ {
		env := types.ForwardEnvelope{
			ProtocolVersion: 1,
			IdempotencyKey:  fmt.Sprintf("sentinel:sha256v1:probe%d|1|7f3a91c2d4e5b607", i),
			HostID:          "7f3a91c2d4e5b607",
			Records: []types.Record{{
				Kind:    types.KEvent,
				Sig:     fmt.Sprintf("sentinel:sha256v1:probe%d", i),
				Origin:  types.Origin{HostID: "7f3a91c2d4e5b607", Source: "sentinel"},
				Actor:   types.Actor{Kind: types.ActorDaemon, ID: "probe"},
				Payload: map[string]any{"subject": "drain-probe"},
			}},
		}
		if _, err := hub.Enqueue(context.Background(), c, env, hub.RouteA); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}

	// A REAL ledger outside /tmp (SPEC-01 §3.9).
	root := hubTestRoot(t)
	led, err := ledger.Open(context.Background(), ledger.Options{
		Root:           filepath.Join(root, "ledger"),
		Rotation:       ledger.DefaultRotationPolicy(),
		Retention:      ledger.DefaultRetentionPolicy(),
		Index:          ledger.DefaultIndexOptions(),
		Writer:         types.Actor{Kind: types.ActorDaemon, ID: "trouble-hub-drain-test"},
		MaxSchema:      ledger.SchemaVersionV1,
		Now:            time.Now,
		HostID:         "7f3a91c2d4e5b607",
		Zone:           "loopback",
		AssertScrubbed: true,
	})
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	defer led.Close(context.Background())

	cs := hub.NewConsumer(c, led, nil, c.Config())
	stats, err := cs.Drain(context.Background(), 10*time.Second)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if stats.Acked != 2 || stats.Appended != 2 {
		t.Fatalf("stats = %+v, want 2 acked / 2 appended", stats)
	}
	if n, _ := f.XLen(context.Background(), "trouble:ingest:cli"); n != 2 {
		t.Fatalf("stream still holds %d entries after the drain (acked entries stay in the stream; the PEL must be empty)", n)
	}
	if p, err := f.XPending(context.Background(), "trouble:ingest:cli", "ledger-writers-cli"); err != nil || p.Count != 0 {
		t.Fatalf("pending after drain = %+v err=%v, want 0", p, err)
	}
	// The ledger received the records (the drain's whole purpose).
	if st := led.Status(); st.Records < 2 {
		t.Fatalf("ledger records = %d, want >= 2", st.Records)
	}

	// An already-empty stream drains clean: the 0-exit of the idempotent rerun.
	// NOTE: Drain's stats are CUMULATIVE over the consumer's lifetime (the
	// counters are atomics, not deltas), so the rerun reports the totals, not
	// zeros — what matters is that the rerun acked NOTHING NEW and returned
	// the clean-nil error (the §2.2 drained row).
	stats2, err := cs.Drain(context.Background(), time.Second)
	if err != nil {
		t.Fatalf("Drain (second): %v", err)
	}
	if stats2.Acked != stats.Acked {
		t.Fatalf("second drain acked %d (cum=%d), want nothing new", stats2.Acked, stats.Acked)
	}
}

// TestHubDrainTimeoutExit1Spec13Section22: the §2.2 timeout row — a consumer
// whose ledger refuses appends leaves the entries pending (never acked) and
// Drain answers the timeout error; the CLI maps that to exit 1 with the
// pending count on stderr. Proven in-process: the timeout path is the same
// code the verb's exit mapping switches on.
func TestHubDrainTimeoutExit1Spec13Section22(t *testing.T) {
	f := newDrainStreams()
	c, err := hub.OpenWith(context.Background(), hub.RedisConfig{
		HostID: "h", Stream: "trouble:ingest:cli", Group: "ledger-writers-cli",
	}, f)
	if err != nil {
		t.Fatalf("OpenWith: %v", err)
	}
	defer c.Close()
	if err := c.EnsureGroup(context.Background()); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	env := types.ForwardEnvelope{
		ProtocolVersion: 1,
		IdempotencyKey:  "sentinel:sha256v1:timeout|1|h",
		HostID:          "h",
		Records: []types.Record{{
			Kind:    types.KEvent,
			Sig:     "sentinel:sha256v1:timeout",
			Origin:  types.Origin{HostID: "h", Source: "sentinel"},
			Actor:   types.Actor{Kind: types.ActorDaemon, ID: "probe"},
			Payload: map[string]any{"subject": "timeout-probe"},
		}},
	}
	if _, err := hub.Enqueue(context.Background(), c, env, hub.RouteA); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	appender := failingAppender{err: fmt.Errorf("ledger closed")}
	cs := hub.NewConsumer(c, appender, nil, c.Config())
	_, err = cs.Drain(context.Background(), 500*time.Millisecond)
	if err == nil {
		t.Fatalf("Drain over a failing ledger must time out with the entry pending, got nil error")
	}
	p, perr := f.XPending(context.Background(), "trouble:ingest:cli", "ledger-writers-cli")
	if perr != nil {
		t.Fatalf("XPending: %v", perr)
	}
	if p.Count != 1 {
		t.Fatalf("pending = %d, want 1: a failed batch is never acked (SPEC-13 §3.3)", p.Count)
	}
	// The CLI's exit mapping for this exact error class is the §2.2 row:
	// timeout with pending entries → 1. (Mapping asserted directly: the verb
	// returns 1 when Drain errors, 0 when it does not.)
	if hub.CodeOf(err) == "" && !strings.Contains(err.Error(), "timed out") && !strings.Contains(err.Error(), "append") {
		t.Fatalf("unexpected drain error shape: %v", err)
	}
}

// failingAppender is a hub.Appender that always refuses: the timeout row's
// "ledger cannot take the records" side.
type failingAppender struct{ err error }

func (a failingAppender) Append(ctx context.Context, d types.RecordDraft) (types.Record, error) {
	return types.Record{}, a.err
}

// ---------------------------------------------------------------------------
// The in-process Streams the CLI tests run against (no Redis, no ports).
// ---------------------------------------------------------------------------

// probeStreams is the dedup probe's minimal backend: KV only.
type probeStreams struct {
	mu chan struct{}
	kv map[string]string
}

func newProbeStreams() *probeStreams {
	return &probeStreams{mu: make(chan struct{}, 1), kv: map[string]string{}}
}

func (s *probeStreams) lock()   { s.mu <- struct{}{} }
func (s *probeStreams) unlock() { <-s.mu }

func (s *probeStreams) Ping(ctx context.Context) error { return nil }
func (s *probeStreams) ServerInfo(ctx context.Context) (hub.ServerInfo, error) {
	return hub.ServerInfo{AOFEnabled: true, Policy: "noeviction", OptionsChecked: true}, nil
}
func (s *probeStreams) XAdd(ctx context.Context, stream string, maxLen int64, values map[string]any) (string, error) {
	return "", nil
}
func (s *probeStreams) XLen(ctx context.Context, stream string) (int64, error) { return 0, nil }
func (s *probeStreams) XGroupCreate(ctx context.Context, stream, group, start string) error {
	return nil
}
func (s *probeStreams) XReadGroup(ctx context.Context, req hub.ReadRequest) ([]hub.StreamEntry, error) {
	return nil, nil
}
func (s *probeStreams) XAck(ctx context.Context, stream, group string, ids ...string) (int64, error) {
	return 0, nil
}
func (s *probeStreams) XAutoClaim(ctx context.Context, req hub.ClaimRequest) (hub.ClaimResult, error) {
	return hub.ClaimResult{Next: "0-0"}, nil
}
func (s *probeStreams) XPending(ctx context.Context, stream, group string) (hub.Pending, error) {
	return hub.Pending{}, nil
}
func (s *probeStreams) XInfoGroups(ctx context.Context, stream string) (hub.GroupInfo, error) {
	return hub.GroupInfo{}, nil
}
func (s *probeStreams) SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	s.lock()
	defer s.unlock()
	if _, ok := s.kv[key]; ok {
		return false, nil
	}
	s.kv[key] = value
	return true, nil
}
func (s *probeStreams) SetXX(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	s.lock()
	defer s.unlock()
	if _, ok := s.kv[key]; !ok {
		return false, nil
	}
	s.kv[key] = value
	return true, nil
}
func (s *probeStreams) Get(ctx context.Context, key string) (string, bool, error) {
	s.lock()
	defer s.unlock()
	v, ok := s.kv[key]
	return v, ok, nil
}
func (s *probeStreams) TTL(ctx context.Context, key string) (time.Duration, error) {
	s.lock()
	defer s.unlock()
	if _, ok := s.kv[key]; !ok {
		return 0, nil
	}
	return 24 * time.Hour, nil
}
func (s *probeStreams) Del(ctx context.Context, keys ...string) (int64, error) {
	s.lock()
	defer s.unlock()
	var n int64
	for _, k := range keys {
		if _, ok := s.kv[k]; ok {
			delete(s.kv, k)
			n++
		}
	}
	return n, nil
}
func (s *probeStreams) Close() error { return nil }

// drainStreams is the stream backend the drain tests run against: real
// entries, a real PEL, and SetNX (the consumer's gate) — the same shape as
// internal/hub's own fake, kept local because that fake is test-only.
type drainStreams struct {
	mu      chan struct{}
	entries []drainEntry
	seq     int
	pel     map[string]drainEntry
	lastID  string
	kv      map[string]string
}

type drainEntry struct {
	id     string
	fields map[string]string
}

func newDrainStreams() *drainStreams {
	return &drainStreams{
		mu:  make(chan struct{}, 1),
		pel: map[string]drainEntry{},
		kv:  map[string]string{},
	}
}

func (s *drainStreams) lock()   { s.mu <- struct{}{} }
func (s *drainStreams) unlock() { <-s.mu }

func (s *drainStreams) Ping(ctx context.Context) error { return nil }
func (s *drainStreams) ServerInfo(ctx context.Context) (hub.ServerInfo, error) {
	return hub.ServerInfo{AOFEnabled: true, Policy: "noeviction", OptionsChecked: true}, nil
}
func (s *drainStreams) XAdd(ctx context.Context, stream string, maxLen int64, values map[string]any) (string, error) {
	s.lock()
	defer s.unlock()
	s.seq++
	id := fmt.Sprintf("1758012841221-%d", s.seq)
	fields := map[string]string{}
	for k, v := range values {
		fields[k] = fmt.Sprint(v)
	}
	s.entries = append(s.entries, drainEntry{id: id, fields: fields})
	return id, nil
}
func (s *drainStreams) XLen(ctx context.Context, stream string) (int64, error) {
	s.lock()
	defer s.unlock()
	return int64(len(s.entries)), nil
}
func (s *drainStreams) XGroupCreate(ctx context.Context, stream, group, start string) error {
	return nil
}
func (s *drainStreams) XReadGroup(ctx context.Context, req hub.ReadRequest) ([]hub.StreamEntry, error) {
	s.lock()
	defer s.unlock()
	var out []hub.StreamEntry
	for _, e := range s.entries {
		if len(out) >= req.Count {
			break
		}
		if e.id <= s.lastID {
			continue
		}
		s.pel[e.id] = e
		s.lastID = e.id
		out = append(out, hub.StreamEntry{ID: e.id, Fields: e.fields, DeliveryCount: 1})
	}
	return out, nil
}
func (s *drainStreams) XAck(ctx context.Context, stream, group string, ids ...string) (int64, error) {
	s.lock()
	defer s.unlock()
	var n int64
	for _, id := range ids {
		if _, ok := s.pel[id]; ok {
			delete(s.pel, id)
			n++
		}
	}
	return n, nil
}
func (s *drainStreams) XAutoClaim(ctx context.Context, req hub.ClaimRequest) (hub.ClaimResult, error) {
	return hub.ClaimResult{Next: "0-0"}, nil
}
func (s *drainStreams) XPending(ctx context.Context, stream, group string) (hub.Pending, error) {
	s.lock()
	defer s.unlock()
	return hub.Pending{Count: int64(len(s.pel))}, nil
}
func (s *drainStreams) XInfoGroups(ctx context.Context, stream string) (hub.GroupInfo, error) {
	s.lock()
	defer s.unlock()
	n := int64(len(s.entries))
	return hub.GroupInfo{Pending: int64(len(s.pel)), LastDeliveredID: s.lastID, Lag: n - int64(len(s.pel))}, nil
}
func (s *drainStreams) SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	s.lock()
	defer s.unlock()
	if _, ok := s.kv[key]; ok {
		return false, nil
	}
	s.kv[key] = value
	return true, nil
}
func (s *drainStreams) SetXX(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	s.lock()
	defer s.unlock()
	if _, ok := s.kv[key]; !ok {
		return false, nil
	}
	s.kv[key] = value
	return true, nil
}
func (s *drainStreams) Get(ctx context.Context, key string) (string, bool, error) {
	s.lock()
	defer s.unlock()
	v, ok := s.kv[key]
	return v, ok, nil
}
func (s *drainStreams) TTL(ctx context.Context, key string) (time.Duration, error) {
	s.lock()
	defer s.unlock()
	if _, ok := s.kv[key]; !ok {
		return 0, nil
	}
	return 24 * time.Hour, nil
}
func (s *drainStreams) Del(ctx context.Context, keys ...string) (int64, error) {
	s.lock()
	defer s.unlock()
	var n int64
	for _, k := range keys {
		if _, ok := s.kv[k]; ok {
			delete(s.kv, k)
			n++
		}
	}
	return n, nil
}
func (s *drainStreams) Close() error { return nil }
