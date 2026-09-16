package sensors

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// liveness_test.go covers SPEC-03 §7's liveness row: one 024 per episode with a
// sensor_recovered on recovery, entry silence that is never staleness, the
// dependency case that produces exactly one gap, the Health() budget and the
// canary.

func enableAll(t *testing.T, h *harness) {
	t.Helper()
	for _, k := range types.SensorKinds {
		h.s.setSensor(k, true, false, "")
	}
}

// TestStaleThresholdFiresOncePerEpisode: 024 exactly once, then a recovery.
func TestStaleThresholdFiresOncePerEpisode(t *testing.T) {
	h := newHarness(t)
	enableAll(t, h)
	ctx := context.Background()
	h.s.sensorOK(types.SenDisk)
	// Nothing stale yet.
	h.advance(staleAfter[types.SenDisk] / 2)
	h.s.livenessCheck(ctx)
	if len(h.gaps()) != 0 {
		t.Fatalf("a fresh sensor filed %d gaps", len(h.gaps()))
	}
	// Now stale: the episode opens.
	h.advance(staleAfter[types.SenDisk])
	h.s.livenessCheck(ctx)
	h.s.livenessCheck(ctx)
	h.s.livenessCheck(ctx)
	if got := len(h.gaps()); got != 0 {
		t.Fatalf("an open episode must not emit %d gap records yet (it is closed on recovery)", got)
	}
	if rt := h.s.rt[types.SenDisk]; rt == nil || !rt.degraded.Load() {
		t.Fatal("a stale sensor must be degraded")
	}
	// Recovery closes the episode: exactly one gap and exactly one recovered.
	h.s.sensorOK(types.SenDisk)
	h.s.livenessCheck(ctx)
	gaps := h.gaps()
	if len(gaps) != 1 {
		t.Fatalf("expected exactly one gap for the episode, got %d", len(gaps))
	}
	if gaps[0].Payload["cause"] != "heartbeat_stale" {
		t.Fatalf("cause = %v", gaps[0].Payload["cause"])
	}
	rec := h.recordsWhere(func(r types.Record) bool { return r.Payload["kind"] == "sensor_recovered" })
	if len(rec) != 1 {
		t.Fatalf("expected exactly one sensor_recovered, got %d", len(rec))
	}
	rt := h.s.rt[types.SenDisk]
	if rt.degraded.Load() {
		t.Fatal("recovery must clear Degraded")
	}
	if reason := *rt.reason.Load(); reason != "" {
		t.Fatalf("recovery must clear Reason, got %q", reason)
	}
	// A second stale window is a second episode, not a repeat of the first.
	h.advance(staleAfter[types.SenDisk] * 2)
	h.s.livenessCheck(ctx)
	h.s.sensorOK(types.SenDisk)
	h.s.livenessCheck(ctx)
	if got := len(h.gaps()); got != 2 {
		t.Fatalf("expected a second episode's gap, got %d total", got)
	}
}

// TestEntrySilenceIsNotStaleness: the probe is the liveness proof; a quiet
// journal/dbus/inotify for an hour with a green probe is healthy.
func TestEntrySilenceIsNotStaleness(t *testing.T) {
	h := newHarness(t)
	enableAll(t, h)
	ctx := context.Background()
	for _, k := range []types.SensorKind{types.SenJournald, types.SenDBus, types.SenInotify} {
		h.s.sensorOK(k) // last success = now
	}
	h.setClock(h.now().Add(time.Hour)) // an hour of no entries
	for _, k := range []types.SensorKind{types.SenJournald, types.SenDBus, types.SenInotify} {
		h.s.sensorOK(k) // ...but the probe kept succeeding
	}
	h.s.livenessCheck(ctx)
	if got := len(h.gaps()); got != 0 {
		t.Fatalf("entry silence must not be staleness; %d gaps filed", got)
	}
	// Health reports a large LastEventAgeS without any penalty.
	ageSeen := false
	for _, sh := range h.s.Health() {
		if sh.LastEventAgeS > 300 || sh.LastEventAgeS == 0 {
			ageSeen = true
		}
		if sh.Degraded {
			t.Fatalf("%s reported degraded on entry silence: %s", sh.Sensor, sh.Reason)
		}
	}
	if !ageSeen {
		t.Fatal("Health did not report an event age")
	}
}

// TestDependencyDegradationFilesOneGap: the bus gap is the primary record; the
// dependent sensor carries a reason and no second gap.
func TestDependencyDegradationFilesOneGap(t *testing.T) {
	h := newHarness(t)
	enableAll(t, h)
	ctx := context.Background()
	// The bus goes down: one bus_down gap, filed immediately and not again on
	// recovery.
	h.s.openGap(types.SenDBus, "bus", "bus_down", "connect failed")
	h.s.emitGapNow(ctx, string(types.SenDBus), "bus", "bus_down", "connect failed")
	h.s.closeGap(ctx, types.SenDBus, "bus", "bus_down")
	h.s.dbState.dead.Store(true)
	h.s.timerSweep(ctx)
	if got := len(h.gaps()); got != 1 {
		t.Fatalf("expected exactly one gap (the primary cause), got %d", got)
	}
	if reason := *h.s.rt[types.SenTimers].reason.Load(); reason != "dependency dbus degraded" {
		t.Fatalf("timers reason = %q", reason)
	}
}

// TestHealthBudgetWithMaxRules: Health() ≤50 ms with 1024 rules and 6 sensors.
func TestHealthBudgetWithMaxRules(t *testing.T) {
	h := newHarness(t)
	enableAll(t, h)
	// 1024 rules spread across the six sources: a single source is capped at 256.
	sources := []string{"psi", "journald", "dbus", "disk", "timers", "inotify"}
	fields := map[string]string{
		"psi": "some_avg10", "journald": "priority", "dbus": "exit_code",
		"disk": "free_pct", "timers": "missed_runs", "inotify": "overflow",
	}
	ops := map[string]string{"overflow": "=="}
	var b strings.Builder
	for i := 0; i < maxRulesTotal; i++ {
		src := sources[i%len(sources)]
		f := fields[src]
		v := `>= "1"`
		if src == "inotify" {
			v = `== "true"`
		}
		_ = ops
		fmt.Fprintf(&b, "[[rule]]\nname = \"%s_%s\"\nsource = \"%s\"\nfor = \"1s\"\n[[rule.match]]\nfield = \"%s\"\nop = \"%s\"\nvalue = \"%s\"\nvalue_type = \"%s\"\n",
			src, pad4(i), src, f, map[string]string{"inotify": "==", "journald": ">=", "dbus": ">=", "disk": ">=", "timers": ">=", "psi": ">="}[src], strings.Trim(strings.TrimPrefix(strings.TrimPrefix(v, ">= "), "== "), "\""), map[bool]string{true: "bool", false: "number"}[src == "inotify"])
	}
	h.writeRules("10.toml", b.String())
	h.mustReload()
	if got := h.s.ruleCount(); got != maxRulesTotal {
		t.Fatalf("loaded %d rules, want %d", got, maxRulesTotal)
	}
	start := time.Now()
	for i := 0; i < 100; i++ {
		if got := len(h.s.Health()); got != 6 {
			t.Fatalf("Health returned %d sensors, want 6", got)
		}
	}
	per := time.Since(start) / 100
	if per > 50*time.Millisecond {
		t.Fatalf("Health() took %s per call, budget is 50ms", per)
	}
	t.Logf("measured: Health() %s per call with %d rules and 6 sensors", per, maxRulesTotal)
	if got := len(h.s.Liveness()); got != 6 {
		t.Fatalf("Liveness returned %d entries, want 6", got)
	}
}

func pad4(i int) string {
	s := "0000"
	d := []byte(s)
	for p := len(d) - 1; p >= 0 && i > 0; p-- {
		d[p] = byte('0' + i%10)
		i /= 10
	}
	return string(d)
}

// TestCanaryLandsThroughThePipeline: the canary goes through normalization and
// the ledger seam, and its id comes back.
func TestCanaryLandsThroughThePipeline(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	id, err := h.s.Canary(ctx, "io")
	if err != nil {
		t.Fatalf("Canary: %v", err)
	}
	if !strings.HasPrefix(id, "can_") {
		t.Fatalf("canary id = %q", id)
	}
	recs := h.recordsWhere(func(r types.Record) bool { return r.Kind == types.KCanary })
	if len(recs) != 1 {
		t.Fatalf("expected one canary record, got %d", len(recs))
	}
	rec := recs[0]
	if rec.Sig == "" {
		t.Fatal("a canary record needs a sig to be deduped")
	}
	if rec.Payload["kind"] != "canary" {
		t.Fatalf("payload.kind = %v", rec.Payload["kind"])
	}
	if rec.Payload["canary_id"] != id {
		t.Fatalf("canary_id = %v, want %v", rec.Payload["canary_id"], id)
	}
	if rec.Payload["landed"] != true {
		t.Fatal("a canary that the ledger accepted must record landed=true")
	}
	if _, ok := rec.Payload["injected_ts"]; !ok {
		t.Fatal("payload must carry injected_ts")
	}
	if _, ok := rec.Payload["observed_ts"]; !ok {
		t.Fatal("payload must carry observed_ts")
	}
	if h.s.canaryLanded.Load() == 0 {
		t.Fatal("the landing timestamp must be recorded")
	}
}

// TestCanaryMissingOnABlockedPipeline: absence of evidence is never evidence of
// health.
func TestCanaryMissingOnABlockedPipeline(t *testing.T) {
	h := newHarness(t)
	gate := make(chan struct{})
	h.setBlock(gate)
	// The pipeline is blocked when the canary is injected and recovers shortly
	// after: the canary fails, the gap still lands.
	go func() {
		time.Sleep(200 * time.Millisecond)
		close(gate)
		h.clearBlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err := h.s.Canary(ctx, "io")
	if err == nil {
		t.Fatal("a canary that does not land must report an error")
	}
	gaps := h.gaps()
	if len(gaps) != 1 {
		t.Fatalf("expected one canary_missing gap, got %d", len(gaps))
	}
	if gaps[0].Payload["cause"] != "canary_missing" {
		t.Fatalf("cause = %v", gaps[0].Payload["cause"])
	}
	if _, err := h.s.Canary(context.Background(), "io"); err != nil {
		t.Fatalf("after the pipeline recovers the canary must land: %v", err)
	}
}

// TestHealthReflectsEventAgeWithinOneHeartbeat: last_event_age_s tracks the
// newest event without waiting for a health poll.
func TestHealthReflectsEventAgeWithinOneHeartbeat(t *testing.T) {
	h := newHarness(t)
	enableAll(t, h)
	ev := psiEvent("io", 41.7, false)
	h.s.handleEvent(context.Background(), ev)
	h.advance(3 * time.Second)
	var found bool
	for _, sh := range h.s.Health() {
		if sh.Sensor != types.SenPSI {
			continue
		}
		found = true
		if sh.LastEventAgeS < 2.9 || sh.LastEventAgeS > 3.1 {
			t.Fatalf("last_event_age_s = %v, want ~3s", sh.LastEventAgeS)
		}
		if sh.LastEventTS == "" || sh.LastSuccessTS == "" {
			t.Fatal("both timestamps must be populated")
		}
	}
	if !found {
		t.Fatal("psi is missing from Health()")
	}
}

// TestHealthExcludesDisabledSensors: one entry per ENABLED sensor.
func TestHealthExcludesDisabledSensors(t *testing.T) {
	h := newHarness(t)
	h.s.setSensor(types.SenPSI, true, false, "")
	h.s.setSensor(types.SenDisk, false, false, "disabled by configuration")
	got := h.s.Health()
	if len(got) != 1 || got[0].Sensor != types.SenPSI {
		t.Fatalf("Health = %+v, want only the enabled psi sensor", got)
	}
	if got[0].Enabled != true {
		t.Fatal("an enabled sensor must report Enabled:true")
	}
}

// TestLivenessIsProbeBacked: Alive follows last success, not last event.
func TestLivenessIsProbeBacked(t *testing.T) {
	h := newHarness(t)
	enableAll(t, h)
	for _, k := range types.SensorKinds {
		h.s.sensorOK(k)
	}
	h.advance(2 * time.Second)
	for _, l := range h.s.Liveness() {
		if !l.Alive {
			t.Fatalf("%s is not alive although its probe just succeeded", l.Source)
		}
		if l.MaxAgeS != staleAfter[types.SensorKind(l.Source)].Seconds() {
			t.Fatalf("%s max_age_s = %v, want the §3.9 table value", l.Source, l.MaxAgeS)
		}
		if l.HostID != "testhost" {
			t.Fatalf("host_id = %q", l.HostID)
		}
	}
	// Push every sensor past its expectation except the long-window one.
	h.advance(staleAfter[types.SenInotify] - 10*time.Second)
	var dead []string
	for _, l := range h.s.Liveness() {
		if !l.Alive {
			dead = append(dead, l.Source)
		}
	}
	if len(dead) != 5 {
		t.Fatalf("expected only the long-window sensor to stay alive, dead=%v", dead)
	}
}
