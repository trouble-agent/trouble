package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/dashboard"
	"github.com/totalwindupflightsystems/trouble/internal/ladder"
	"github.com/totalwindupflightsystems/trouble/internal/ledger"
	"github.com/totalwindupflightsystems/trouble/internal/lifecycle"
	"github.com/totalwindupflightsystems/trouble/internal/registry"
	"github.com/totalwindupflightsystems/trouble/internal/scrub"
	"github.com/totalwindupflightsystems/trouble/internal/sensors"
	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// daemon.go is the boot sequence of SPEC-12 §4.1 and the drain of §4.2, in code.
// It is the only file that knows how to assemble every subsystem, and it is the
// only place a listener, a goroutine and a ledger writer come into existence
// together. A failure at any step before `bind preflight` leaves the state root
// untouched except for the auditable record the spec requires.

// BootOptions is everything the daemon needs from the process around it.
type BootOptions struct {
	// Args and Env are the raw process argv (minus argv[0]) and environ, passed
	// through lifecycle.Resolve so precedence is decided in exactly one place.
	Args []string
	Env  []string

	// Log receives operator-facing lines; nil silences them.
	Log *slog.Logger

	// HostID overrides the origin host id (tests pin it); empty uses the
	// resolved config's origin.host_id, then the hostname.
	HostID string

	// OnReady is called once the daemon serves (the binary sends sd_notify
	// READY=1 here); tests use it to proceed deterministically.
	OnReady func(d *Daemon)
}

// Daemon is the assembled process.
type Daemon struct {
	Cfg        lifecycle.Config
	Resolved   lifecycle.Resolved
	Ledger     *ledger.Ledger
	Store      *Store
	Scrubber   *scrub.Engine
	Sensors    *sensors.Sensors
	Ladder     *ladder.Ladder
	Registry   *registry.Registry
	Spool      *lifecycle.Spool
	DashConfig dashboard.Config
	HealthURL  string

	log     *slog.Logger
	notify  *notifier
	started time.Time
	ready   chan struct{}
	stop    chan struct{}

	rulesOnce   sync.Once
	rules       map[string]types.Rule
	plays       map[string]types.Play
	playsByName map[string]types.Play
	gen         uint64

	drainErr error
}

// RunDaemon performs the full boot, serves until ctx is cancelled, then drains.
func RunDaemon(ctx context.Context, o BootOptions) (*Daemon, error) {
	log := o.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	started := time.Now()

	// 1. config resolve (flag > env > file > default; 001/002).
	//
	// `--config <path>` is the operator's spelling of the `config_path` key; it is
	// consumed here because the path is an INPUT to resolution, not a key
	// resolution can already have read. The mechanical `--config-path` form works
	// too (SPEC-12 §2.5) and is what Resolve itself would accept.
	args, cfgPath := splitConfigFlag(o.Args, defaultConfigPath())
	res, err := lifecycle.Resolve(args, o.Env, cfgPath)
	if err != nil {
		return nil, err
	}
	cfg := res.Config
	// origin.host_id is the stable host identity SPEC-12 §3.7 requires (a derived
	// id, never the hostname): the whole cross-host dedup path keys on it, so a
	// hostname that changes with DHCP must not be able to create a second identity
	// for the same machine. T1 is a hub with zero satellites, so hub_id defaults
	// to the host id.
	if cfg.Origin.HostID == "" {
		cfg.Origin.HostID = stableHostID()
	}
	if cfg.Origin.HubID == "" {
		cfg.Origin.HubID = cfg.Origin.HostID
	}
	res.Config = cfg
	// The subsystems read the resolved key list, not the struct: a value derived
	// after resolution (the host identity) has to be published into the list or
	// the sensors will refuse to start for a missing key the daemon just computed.
	res.Values = setResolved(res.Values, "origin.host_id", cfg.Origin.HostID, "derived", "derived:machine-id")
	res.Values = setResolved(res.Values, "origin.hub_id", cfg.Origin.HubID, "derived", "derived:t1-zero-satellites")

	// 2. state root + modes (004/005).
	root, err := lifecycle.CheckStateRoot(cfg)
	if err != nil {
		return nil, err
	}
	// 3. secret-file modes + the argv secret scan (013).
	if _, err := lifecycle.CheckSecretFiles(cfg); err != nil {
		return nil, err
	}
	if err := lifecycle.ScanProcCmdline(); err != nil {
		return nil, err
	}
	// 4. schema_version compatibility (012).
	if err := lifecycle.CheckSchemaCompat(root.Path, ledger.SchemaVersionV1); err != nil {
		return nil, err
	}

	// 5. ledger open + index rebuild (008). The Actor triple is injected here so
	// internal/ledger never has to import internal/lifecycle (SPEC-12 §4.5).
	actor := lifecycle.Actor(types.ActorDaemon, "troubled")
	hostID := o.HostID
	if hostID == "" {
		hostID = cfg.Origin.HostID
	}
	if hostID == "" {
		hostID = hostnameOr("unknown-host")
	}
	l, err := ledger.Open(ctx, ledger.Options{
		Root:           filepath.Join(root.Path, "ledger"),
		Rotation:       ledger.DefaultRotationPolicy(),
		Retention:      ledger.DefaultRetentionPolicy(),
		Index:          ledger.DefaultIndexOptions(),
		Writer:         actor,
		MaxSchema:      ledger.SchemaVersionV1,
		Now:            time.Now,
		HostID:         hostID,
		Zone:           lifecycle.ZoneOf(cfg.Dashboard.Bind, "daemon", lifecycle.AuthLoopback),
		AssertScrubbed: true,
	})
	if err != nil {
		return nil, err
	}
	d := &Daemon{
		Cfg:      cfg,
		Resolved: res,
		Ledger:   l,
		started:  started,
		log:      log,
		notify:   newNotifier(os.Getenv),
		ready:    make(chan struct{}),
		stop:     make(chan struct{}),
	}
	d.Store = NewStore(l, actor, hostID)

	// 6. the config record: the full redacted dump, one per boot/RELOAD.
	if err := lifecycle.WriteConfigRecord(DraftWriter{d.Store}, res); err != nil {
		_ = l.Close(ctx)
		return nil, err
	}

	// 7. bind preflight (003) — listeners are held, never closed and reopened.
	probes, err := lifecycle.PreflightBinds(cfg)
	if err != nil {
		// The refusal is auditable: one lifecycle record, zero HTTP responses.
		_, _ = d.Store.Append(ctx, types.KLifecycle, "", "", map[string]any{
			"stage":      "boot_refused",
			"error_code": string(types.CodeLifecycle003),
			"detail":     err.Error(),
		})
		_ = l.Close(ctx)
		return nil, err
	}
	var dashLn net.Listener
	for _, p := range probes {
		if p.Name == "dashboard" {
			dashLn = p.Listener
		}
	}
	if dashLn == nil {
		_ = l.Close(ctx)
		return nil, fmt.Errorf("bind preflight returned no dashboard listener")
	}
	d.HealthURL = "http://" + dashLn.Addr().String() + "/health.json"

	// 8. the scrubber (SPEC-02) is a shared boundary, not a per-call construction.
	if eng, err := scrub.New(nil, nil); err == nil {
		d.Scrubber = eng
	}

	// 9. sensors start (SPEC-03). The emit path is where an event becomes a
	// ledger record AND, when a rule fired, a ladder observation.
	sn, err := sensors.New(res.Values, d.emit, d.redact, time.Now)
	if err != nil {
		d.bootFailure(ctx, types.CodeLifecycle003, err)
		return nil, err
	}
	d.Sensors = sn

	// 10. registry (SPEC-06) then ladder (SPEC-05): the ladder is the only caller
	// of the registry's six-stage contract, and the registry's module deps come
	// from the resolved config.
	reg, err := registry.New(registry.RegistryDeps{
		Append: func(rec types.Record) (types.Record, error) {
			return l.Append(ctx, types.RecordDraft{
				Kind:    rec.Kind,
				Sig:     rec.Sig,
				Inc:     rec.Inc,
				Origin:  rec.Origin,
				Actor:   rec.Actor,
				Payload: rec.Payload,
			})
		},
		Scrub: func(target string, in []byte) (types.ScrubResult, error) {
			if d.Scrubber == nil {
				return types.ScrubResult{BytesIn: len(in)}, nil
			}
			_, sr, err := d.Scrubber.ScrubBytes(ctx, types.ScrubTarget(target), "", in)
			return sr, err
		},
		Gates:  func() types.AutonomyGates { return ladderCfgGates(d) },
		Config: res.Values,
		HostID: hostID,
		Actor:  actor,
		Now:    time.Now,
	})
	if err != nil {
		d.bootFailure(ctx, types.CodeLifecycle003, err)
		return nil, err
	}
	d.Registry = reg
	_ = reg

	d.Ladder, err = ladder.New(ladder.Deps{
		Ledger:   d.Store,
		Index:    d.Store,
		Registry: NewPlayEngine(reg),
		Clock:    NewClock(started),
		Eval:     NewEvaluator(),
		Cfg:      ladder.DefaultConfig(),
		Notify:   Notifier{Store: d.Store, Log: log},
		Rules:    d.ruleLookup,
		PlayFor:  d.playLookup,
		PIDAlive: pidAlive,
	})
	if err != nil {
		d.bootFailure(ctx, types.CodeLifecycle003, err)
		return nil, err
	}

	// 11. sensors run only after the ladder exists: the first fired rule must
	// find an admission path.
	if err := d.Sensors.Probe(ctx); err != nil {
		log.Warn("sensors probe", "err", err)
	}

	// 12. checker.alarm mirror (SPEC-12 §3.3): the daemon is the only ledger
	// writer, so alarms the checker wrote while it was down become records now.
	d.mirrorCheckerAlarms(ctx)

	// 13. the dashboard, on the listener the preflight already holds.
	d.DashConfig = dashboardConfig(cfg)
	if err := dashboard.ValidateConfig(d.DashConfig); err != nil {
		d.bootFailure(ctx, types.CodeLifecycle001, err)
		return nil, err
	}
	if err := d.Sensors.Start(ctx); err != nil {
		d.bootFailure(ctx, types.CodeLifecycle003, err)
		return nil, err
	}
	// The preflight's listener, not a second bind: SPEC-12 §3.2 keeps the fd so
	// the check and the serve cannot disagree (dashboard.ServeOn).
	go func() {
		if err := dashboard.ServeOn(ctx, d.DashConfig, d.dashboardDeps(dashLn), dashLn); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("dashboard", "err", err)
		}
	}()

	// 14. heartbeat + watchdog, then READY.
	go func() {
		if err := lifecycle.HeartbeatLoop(ctx, cfg, DraftWriter{d.Store}, d.sensorTimes); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("heartbeat", "err", err)
		}
	}()
	go d.notify.watchdogLoop(d.stop, cfg.Lifecycle.WatchdogSec.Std()/2, d.statusLine)
	if cfg.Hub.Mode == "satellite" {
		go func() {
			if err := lifecycle.ForwardLoop(ctx, cfg); err != nil && !errors.Is(err, context.Canceled) {
				log.Error("forward", "err", err)
			}
		}()
	}

	close(d.ready)
	d.notify.ready(d.statusLine())
	if o.OnReady != nil {
		o.OnReady(d)
	}

	<-ctx.Done()
	return d, d.drain()
}

// bootFailure records a lifecycle boot_refused record and closes the ledger.
func (d *Daemon) bootFailure(ctx context.Context, code types.ErrorCode, err error) {
	_, _ = d.Store.Append(ctx, types.KLifecycle, "", "", map[string]any{
		"stage":      "boot_refused",
		"error_code": string(code),
		"detail":     err.Error(),
	})
	if d.Ledger != nil {
		_ = d.Ledger.Close(ctx)
	}
	if d.notify != nil {
		d.notify.status("boot refused: " + err.Error())
	}
}

// Ready reports whether the daemon reached READY.
func (d *Daemon) Ready() <-chan struct{} { return d.ready }

// HealthURLFor returns the health URL the daemon serves (and the stall checker
// parses): one health surface, no second shape (SPEC-12 §2.2).
func (d *Daemon) HealthURLFor() string { return d.HealthURL }

// drain is SPEC-12 §4.2: stop accepting, park, flush, final heartbeat, close.
func (d *Daemon) drain() error {
	d.notify.stopping("parking")
	ctx, cancel := context.WithTimeout(context.Background(), d.Cfg.Lifecycle.DrainTimeout.Std())
	defer cancel()

	if d.Sensors != nil {
		_ = d.Sensors.Stop(ctx)
	}
	if d.Ladder != nil {
		if _, err := d.Ladder.Park(ctx, types.ParkSIGTERM); err != nil {
			d.log.Warn("park", "err", err)
			d.drainErr = err
		}
	}
	if d.Store != nil {
		if err := d.Store.Flush(ctx); err != nil {
			d.log.Warn("flush", "err", err)
		}
	}
	if d.Spool != nil {
		_ = d.Spool.SaveState()
	}
	// A final heartbeat with stage="shutdown" so the checker never sees a fresh
	// heartbeat on a dead daemon.
	_ = lifecycle.WriteShutdownHeartbeat(d.Cfg, DraftWriter{d.Store})
	d.notify.stopping("stopped")
	close(d.stop)
	if d.Ledger != nil {
		if err := d.Ledger.Close(ctx); err != nil {
			d.log.Warn("ledger close", "err", err)
			return err
		}
	}
	return d.drainErr
}

// emit is the sensors→ledger path and the sensors→ladder bridge: every event is
// recorded, and an event whose rule fired opens (or folds into) an incident. The
// bridge lives here rather than inside either package because neither may import
// the other.
func (d *Daemon) emit(ctx context.Context, draft types.RecordDraft) (types.Record, error) {
	rec, err := d.Ledger.Append(ctx, draft)
	if err != nil {
		return rec, err
	}
	if draft.Kind != types.KEvent || d.Ladder == nil {
		return rec, nil
	}
	fire, _ := draft.Payload["fire"].(bool)
	if !fire {
		return rec, nil
	}
	obs, ok := observationFrom(rec, draft.Payload)
	if !ok {
		return rec, nil
	}
	// Admission is synchronous in the caller's goroutine: the event record is
	// already durable (Append returned), and the ladder decides whether a new
	// incident opens. Refusals are recorded by the ladder itself.
	if _, err := d.Ladder.Admit(ctx, obs); err != nil {
		d.log.Warn("admit refused", "sig", rec.Sig, "err", err)
	}
	return rec, nil
}

// redact is the SPEC-02 boundary injected into the sensors.
func (d *Daemon) redact(b []byte, target string) (types.ScrubResult, error) {
	if d.Scrubber == nil {
		return types.ScrubResult{BytesIn: len(b)}, nil
	}
	_, res, err := d.Scrubber.ScrubBytes(context.Background(), types.ScrubTarget(target), "", b)
	return res, err
}

// observationFrom turns a fired event record into the ladder's admission input.
func observationFrom(rec types.Record, payload map[string]any) (ladder.Observation, bool) {
	sig, err := types.ParseSig(rec.Sig)
	if err != nil {
		return ladder.Observation{}, false
	}
	str := func(k string) string {
		s, _ := payload[k].(string)
		return s
	}
	detail := make(map[string]any, len(payload))
	for k, v := range payload {
		detail[k] = v
	}
	return ladder.Observation{
		EventID:  rec.RecID,
		TS:       rec.TS,
		Sig:      sig,
		Rule:     str("rule"),
		Source:   ladder.SourcePath(str("source")),
		Subject:  str("subject"),
		Taxonomy: str("taxonomy"),
		AppKind:  str("app_kind"),
		Severity: types.Severity(str("severity")),
		Detail:   detail,
	}, true
}

// sensorTimes is the heartbeat's sensors map: sensor → last-success RFC3339.
func (d *Daemon) sensorTimes() map[string]string {
	out := map[string]string{}
	if d.Sensors == nil {
		return out
	}
	for _, h := range d.Sensors.Health() {
		out[string(h.Sensor)] = h.LastSuccessTS
	}
	return out
}

// statusLine is the sd_notify STATUS= text: version, seq and sensor health.
func (d *Daemon) statusLine() string {
	seq := uint64(0)
	if d.Ledger != nil {
		seq = d.Ledger.DashReader().LastSeq()
	}
	v, sha, _, unstamped := lifecycle.VersionInfo()
	label := v + " " + sha
	if unstamped {
		label = v + " UNSTAMPED"
	}
	ok, total := 0, 0
	if d.Sensors != nil {
		for _, h := range d.Sensors.Health() {
			total++
			if !h.Degraded {
				ok++
			}
		}
	}
	return fmt.Sprintf("%s seq=%d sensors=%d/%d", label, seq, ok, total)
}

// mirrorCheckerAlarms reads the external checker's append-only alarm file and
// writes one `lifecycle` record per line (SPEC-12 §3.3). The file is the only
// fact available from the window in which this process was not running.
func (d *Daemon) mirrorCheckerAlarms(ctx context.Context) {
	path := d.Cfg.Checker.AlarmFile
	if path == "" {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(line), &payload); err != nil {
			payload = map[string]any{"raw_size": len(line), "parse": "failed"}
		}
		payload["stage"] = "checker_alarm_mirror"
		if _, err := d.Store.Append(ctx, types.KLifecycle, "", "", payload); err != nil {
			d.log.Warn("checker alarm mirror", "err", err)
			return
		}
	}
}

// ruleLookup answers the ladder's rule metadata question from the LIVE sensors
// rule set (sensors.Rules), so the rung decision and the match decision can never
// disagree about what a rule says. One dialect, one source (SPEC-INDEX §4.2).
func (d *Daemon) ruleLookup(name string) (types.Rule, bool) {
	if d.Sensors == nil {
		return types.Rule{}, false
	}
	d.rulesOnce.Do(func() {
		d.rules = map[string]types.Rule{}
		d.gen = d.Sensors.RulesGeneration()
		for _, r := range d.Sensors.Rules() {
			d.rules[r.Name] = r
		}
		if len(d.rules) > 0 {
			d.log.Info("rule set loaded from the sensors", "rules", len(d.rules), "gen", d.gen)
		}
	})
	if d.Sensors.RulesGeneration() != d.gen {
		// The set reloaded (SIGHUP): rebuild the cache rather than serving a rule
		// the sensors no longer evaluate.
		d.rulesOnce = sync.Once{}
		return d.ruleLookup(name)
	}
	r, ok := d.rules[name]
	return r, ok
}

// playLookup resolves the play a rule drafts from the state root's plays
// directory (SPEC-06 §3.7: `registry.plays_dir` defaults to <state_root>/plays,
// files are `plays/<name>@<version>.toml`). The registry owns the parser
// (registry.LoadPlay), so this is a directory lookup, not a second dialect.
//
// A rule with no play on disk drafts none, and the ladder degrades through its
// own T09 edge — that is a visible, recorded outcome, not a silent skip.
func (d *Daemon) playLookup(rule string) (types.Play, bool) {
	d.rulesOnce.Do(func() { d.loadPlays() })
	if p, ok := d.plays[rule]; ok {
		return p, true
	}
	if p, ok := d.playsByName[rule]; ok {
		return p, true
	}
	return types.Play{}, false
}

func (d *Daemon) loadPlays() {
	d.plays = map[string]types.Play{}
	d.playsByName = map[string]types.Play{}
	dir := filepath.Join(d.Cfg.StateRoot, "plays")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".toml")
		if i := strings.LastIndex(name, "@"); i > 0 {
			name = name[:i]
		}
		play, err := registry.LoadPlay(filepath.Join(dir, e.Name()))
		if err != nil {
			d.log.Warn("play skipped", "file", e.Name(), "err", err)
			continue
		}
		d.plays[name] = play
		d.playsByName[play.Name] = play
	}
	if len(d.plays) > 0 {
		d.log.Info("plays loaded", "count", len(d.plays), "dir", dir)
	}
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 asks the kernel "does this pid exist and may I signal it" without
	// sending anything (SPEC-05 §3.6 liveness).
	return p.Signal(syscall.Signal(0)) == nil
}

// setResolved publishes one resolved value into the ConfigValue list, replacing
// the row for that key when it exists and appending one when it does not. The
// provenance says where the value really came from, so `trouble config explain`
// can show that the host identity was derived rather than set.
func setResolved(vals []types.ConfigValue, key, value, source, sourceRef string) []types.ConfigValue {
	row := types.ConfigValue{Key: key, Value: value, Source: source, SourceRef: sourceRef}
	for i := range vals {
		if vals[i].Key == key {
			vals[i] = row
			return vals
		}
	}
	return append(vals, row)
}

// splitConfigFlag pulls `--config <path>` out of argv and returns the remaining
// arguments plus the config path to resolve against.
func splitConfigFlag(args []string, def string) ([]string, string) {
	out := make([]string, 0, len(args))
	path := def
	for i := 0; i < len(args); i++ {
		if args[i] == "--config" && i+1 < len(args) {
			path = args[i+1]
			i++
			continue
		}
		if v, ok := strings.CutPrefix(args[i], "--config="); ok {
			path = v
			continue
		}
		out = append(out, args[i])
	}
	return out, path
}

func defaultConfigPath() string {
	if v := os.Getenv("TROUBLE_CONFIG_PATH"); v != "" {
		return v
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "trouble", "config.toml")
}

// ladderCfgGates reports the live autonomy gates to the registry's authorize
// stage. Until the ladder exists the boot-time default (shadow, no grants) is
// the only safe answer.
func ladderCfgGates(d *Daemon) types.AutonomyGates {
	if d == nil || d.Ladder == nil {
		return types.AutonomyGates{Mode: types.AutoShadow, AllowDetect: true, AllowResearch: true, AllowAgent: true, AllowSpawn: true}
	}
	return d.Ladder.Gates()
}
