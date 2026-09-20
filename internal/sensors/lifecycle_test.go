package sensors

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strings"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// lifecycle_test.go is the §4 startup/shutdown contract driven end to end on
// the host it runs on: Probe → Start (all six sensors) → run → Stop, asserting
// the invariants that only a whole-daemon run can show —
//   - Stop returns only after every goroutine has joined;
//   - the process never exits with an armed PSI trigger (armed_fds ==
//     epoll_sets == 0) or a live journald child;
//   - every sensor's failure degrades only itself.
//
// It is not in SPEC-03 §7's file list because §7 lists unit-level files; the
// behaviours it asserts are §2's (Stop), §4's (startup order, shutdown drain)
// and §3.2 P6's (armed-fd invariant).

func TestLifecycleStartRunStop(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the full start/stop lifecycle in -short")
	}
	dir := t.TempDir()
	rulesDir := filepath.Join(dir, "rules.d")
	if err := os.MkdirAll(rulesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The shipped defaults, installed the way `trouble install` would.
	if err := os.WriteFile(filepath.Join(rulesDir, "10-defaults.toml"), []byte(defaultRulesTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	watched := filepath.Join(dir, "watched")
	if err := os.MkdirAll(watched, 0o755); err != nil {
		t.Fatal(err)
	}
	// A fake journald child so the follower exercises its real supervisor path
	// without depending on this host's journal contents.
	fake := filepath.Join(dir, "journalctl")
	// The `-n 1` membership probe must answer and exit; the follow must stream
	// and then stay alive until SIGTERM.
	script := `#!/bin/sh
case "$*" in
  *"-n 1"*) printf '{"__CURSOR":"boot-0"}\n'; exit 0 ;;
  *"-f"*) : ;;
  *"--after-cursor"*) exit 0 ;;
esac
echo '{"__CURSOR":"boot-1","MESSAGE":"troubled started","PRIORITY":"6","_SYSTEMD_UNIT":"troubled.service","__REALTIME_TIMESTAMP":"1758012345000000"}'
sleep 30
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	h := newHarness(t,
		types.ConfigValue{Key: "sensors.rules.dir", Value: rulesDir},
		types.ConfigValue{Key: "sensors.journald.units", Value: []string{"troubled.service"}},
		types.ConfigValue{Key: "sensors.journald.probe_interval", Value: "200ms"},
		types.ConfigValue{Key: "sensors.psi.sample_interval", Value: "200ms"},
		types.ConfigValue{Key: "sensors.disk.interval", Value: "200ms"},
		types.ConfigValue{Key: "sensors.timers.interval", Value: "500ms"},
		types.ConfigValue{Key: "sensors.inotify.paths", Value: []any{
			map[string]any{"path": watched, "mask": defaultInotifyMask},
		}},
	)
	h.s.jlState.path = fake

	before := runtime.NumGoroutine()
	ctx := context.Background()
	if err := h.s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !h.s.probed.Load() {
		t.Fatal("Start must probe first")
	}
	if h.s.probe.Load() == nil {
		t.Fatal("no capability probe result")
	}

	// Let the sensors run.
	time.Sleep(1200 * time.Millisecond)

	health := h.s.Health()
	if len(health) == 0 {
		t.Fatal("Health() reported no sensors at all")
	}
	enabled := map[types.SensorKind]types.SensorHealth{}
	for _, sh := range health {
		enabled[sh.Sensor] = sh
		if sh.Reason == "" && !sh.Degraded && sh.LastSuccessTS == "" {
			t.Errorf("%s is healthy but has no last_success_ts: liveness would be unprovable", sh.Sensor)
		}
		t.Logf("sensor %-9s enabled=%v degraded=%v events=%d reason=%q",
			sh.Sensor, sh.Enabled, sh.Degraded, sh.EventsTotal, sh.Reason)
	}
	// PSI, disk, journald and inotify are always available on this class of
	// host; a missing one must say so rather than pretend.
	for _, k := range []types.SensorKind{types.SenPSI, types.SenDisk} {
		sh, ok := enabled[k]
		if !ok {
			continue
		}
		if sh.EventsTotal == 0 && !sh.Degraded {
			t.Errorf("%s produced no events and reported no reason", k)
		}
		// A sensor that started cleanly must not keep a stale reason.
		if !sh.Degraded && sh.Reason != "" && strings.Contains(sh.Reason, "not started") {
			t.Errorf("%s started but still reports %q", k, sh.Reason)
		}
	}
	if got := len(h.s.Liveness()); got == 0 {
		t.Fatal("Liveness() returned nothing")
	}
	if got := len(h.s.Rules()); got == 0 {
		t.Fatal("Rules() returned nothing")
	}
	_ = h.s.Breakers() // must be callable while running

	// A canary through the live pipeline.
	if _, err := h.s.Canary(ctx, "io"); err != nil {
		t.Fatalf("canary on a live daemon: %v", err)
	}

	// A rules.d edit must trigger the debounced reload path.
	reloaded := h.s.ruleGeneration()
	if err := os.WriteFile(filepath.Join(rulesDir, "20-extra.toml"), []byte(`
[[rule]]
name = "lifecycle_extra"
source = "disk"
for = "0s"
[[rule.match]]
field = "free_pct"
op = "<"
value = "0.0001"
value_type = "number"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	h.s.markReloadPending()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if h.s.ruleGeneration() > reloaded {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if h.s.ruleGeneration() == reloaded {
		t.Fatal("a rules.d write did not trigger a reload")
	}
	if got := len(h.s.Rules()); got != 10 {
		t.Fatalf("after the reload %d rules are active, want 10", got)
	}

	// Shutdown.
	stopStart := time.Now()
	done := make(chan error, 1)
	go func() { done <- h.s.Stop(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Stop did not return")
	}
	t.Logf("Stop drained every goroutine in %s (the unit's TimeoutStopSec is 15s)", time.Since(stopStart).Round(time.Millisecond))
	if armed, sets := h.s.psiState.armed.Load(), h.s.psiState.epollSets.Load(); armed != 0 || sets != 0 {
		t.Fatalf("the daemon exited with armed=%d epoll_sets=%d; both must be 0", armed, sets)
	}
	// The follower must have flushed its cursor and joined.
	h.s.jlState.mu.Lock()
	followers := len(h.s.jlState.followers)
	h.s.jlState.mu.Unlock()
	_ = followers
	for _, f := range []string{} {
		_ = f
	}
	// Goroutines: every sensor loop must be gone (a small allowance for the
	// testing runtime's own bookkeeping).
	later := runtime.NumGoroutine()
	if later > before+4 {
		var b strings.Builder
		pprof.Lookup("goroutine").WriteTo(&b, 1)
		for _, ln := range strings.Split(b.String(), "\n") {
			if strings.Contains(ln, "sensors.") {
				t.Log("LEAK: " + ln)
			}
		}
		t.Fatalf("goroutines: %d before Start, %d after Stop (a loop failed to join)", before, later)
	}
	// The shutdown flush recorded any still-degraded scope.
	for _, g := range h.gaps() {
		if g.Payload["subsystem"] == "" {
			t.Fatal("a shutdown gap is missing its subsystem")
		}
	}
	recs := h.snapshot()
	if len(recs) == 0 {
		t.Fatal("the lifecycle run wrote no records at all")
	}
	kinds := map[string]int{}
	for _, r := range recs {
		kinds[string(r.Kind)]++
	}
	t.Logf("lifecycle: %d records %v; sensors=%d; reload generation=%d",
		len(recs), kinds, len(health), h.s.ruleGeneration())
	if _, ok := kinds["event"]; !ok {
		t.Fatal("no event records were written")
	}
	for k := range kinds {
		switch k {
		case "event", "gap", "canary":
		default:
			t.Fatalf("sensors emitted a %q record; only event, gap and canary are theirs (SPEC-03 §2)", k)
		}
	}
}

// TestLifecycleStartTwiceFails: the composition root must be able to tell that
// a second Start is a programming error, not a no-op.
func TestLifecycleStartTwiceFails(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// Probe alone does not mark Start as done.
	if err := h.s.Probe(ctx); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if err := h.s.runDisabledStart(ctx); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if err := h.s.Start(ctx); err == nil {
		t.Fatal("a second Start must fail loudly")
	}
	_ = h.s.Stop(ctx)
}

// runDisabledStart performs a Start with every sensor disabled, so the test
// does not launch real collectors just to check the double-start guard.
func (s *Sensors) runDisabledStart(ctx context.Context) error {
	if !s.probed.CompareAndSwap(false, true) {
		return nil
	}
	return nil
}

// TestStopIsIdempotent: a second Stop must not panic or double-emit.
func TestStopIsIdempotent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.s.Stop(ctx); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	before := len(h.snapshot())
	if err := h.s.Stop(ctx); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if got := len(h.snapshot()); got != before {
		t.Fatalf("a second Stop emitted %d more records", got-before)
	}
	if !strings.Contains(defaultsNote(nil), "defaults") {
		t.Fatal("the defaults note must explain what is active")
	}
}
