package sensors

// dbus_boot_test.go — TRBL-094, the fresh-boot D-Bus incident storm.
//
// The measured shape (headless agent host): the boot reconcile's first
// ListUnits returns ~21 pre-existing failed USER units — session units that
// cannot start where no real session exists, expected headless noise. Every
// one arrives as a FIRST arrival: merge.Opened, stabilization passes
// (`for = 0s`), the per-(rule,sig) cooldown is fresh for each distinct
// signature and the breakers are closed at boot. 21 incidents opened within
// the first seconds of daemon life, and the rule:dbus_unit_failed scope
// tripped (>20 incidents in 300s, SPEC-03 §3.8) on a healthy host.
//
// The regressions pinned here, all on the boot-stabilization gate
// (internal/sensors/dbus_boot.go):
//
//   - the 21-unit boot burst opens ZERO incidents in the boot window (every
//     unit recorded, fire suppressed, Suppressed counted, drops 0) and never
//     trips rule:dbus_unit_failed;
//   - the backlog drains under the named pacing cap (5 per 100s), each
//     admitted unit opens exactly its one real incident, re-sights of an
//     admitted unit do NOT re-fire (the story is open), recovery clears the
//     gate, and the total is exactly 21 — deferral is not a mask;
//   - a genuinely new failing unit after the window, and a live
//     PropertiesChanged failure inside the window, fire exactly as before —
//     the gate holds only boot inventory.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// dbusUnitFailedRule is the shipped default rule verbatim (rules.go
// defaultRulesTOML), installed explicitly so the test does not depend on an
// empty rules dir falling back to the defaults.
const dbusUnitFailedRule = `
[[rule]]
name = "dbus_unit_failed"
enabled = true
source = "dbus"
for = "0s"
entry_rung = "play"
severity = "high"
cooldown = "5m"
max_runs = 2
verify_window = "10m"
auto_grants = ["service.reload"]
hotfix = false

[[rule.match]]
field = "unit_substate"
op = "=="
value = "failed"
value_type = "string"
`

// bootStormHarness builds a harness with the shipped dbus_unit_failed rule
// live, a merge tracker wired the way startDBus leaves it, and the boot gate
// armed the way startDBus arms it.
func bootStormHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.writeRules("10.toml", dbusUnitFailedRule)
	h.mustReload()
	h.s.dbState.merge = newMergeTracker(30*time.Second, h.s.now)
	h.s.dbState.boot = newBootStabilizer(h.now(), bootStabilizeWindow, bootDrainInterval)
	return h
}

// bootFires returns the fired records carrying one rule name.
func bootFires(h *harness, rule string) []types.Record { return firedFor(h, rule) }

// breakerTripped reports whether any scope tripped. Snapshot reports a scope
// only when it tripped at least once or is not closed, so the scope's
// absence means the breaker never engaged.
func breakerTripped(h *harness, scope string) bool {
	for _, b := range h.s.Breakers() {
		if scope == "" || b.Scope == scope {
			return true
		}
	}
	return false
}

// reconcileSweep runs one evaluate+batch cycle for the given failed units and
// returns the fired records produced during it.
func reconcileSweep(t *testing.T, h *harness, units []unitStatus) []types.Record {
	t.Helper()
	ctx := context.Background()
	before := len(h.snapshot())
	drafts := h.s.evaluateEveryUnit(ctx, "system", units, h.now())
	h.s.emitBatchOutcome(ctx, drafts)
	after := h.snapshot()
	out := make([]types.Record, 0, 4)
	for _, r := range after[before:] {
		if fire, _ := r.Payload["fire"].(bool); fire {
			out = append(out, r)
		}
	}
	return out
}

// TestFreshBootFailedUnitStormDoesNotTripTheRuleBreaker is the TRBL-094
// regression: 21 distinct units, all already failed at the boot reconcile
// (inside ~5s of daemon life), must open far fewer than 21 incidents in that
// window — zero, in fact — and rule:dbus_unit_failed must never trip.
func TestFreshBootFailedUnitStormDoesNotTripTheRuleBreaker(t *testing.T) {
	h := bootStormHarness(t)

	const n = 21
	units := failedUnits(n) // batch000.service .. batch020.service, all failed

	fired := reconcileSweep(t, h, units)
	if len(fired) != 0 {
		t.Fatalf("the boot sweep opened %d incidents, want 0: the boot burst must not fire the rule 21 times in one window", len(fired))
	}
	// Nothing was dropped: every unit is RECORDED at boot, fire suppressed,
	// the stabilization disposition named on the record.
	recs := h.snapshot()
	if len(recs) == 0 || len(recs) > n {
		t.Fatalf("the boot sweep wrote %d records, want <= %d reconcile records", len(recs), n)
	}
	bootMarked := 0
	for _, r := range recs {
		if fire, _ := r.Payload["fire"].(bool); fire {
			t.Fatalf("record sig %s fired at boot; boot inventory must be deferred, not fired", r.Sig)
		}
		if v, _ := r.Payload["boot_stabilized"].(bool); !v {
			continue
		}
		bootMarked++
		if sup, _ := r.Payload["suppressed"].(bool); !sup {
			t.Fatalf("record sig %s lacks suppressed=true: the ladder-gate suppression must be declared", r.Sig)
		}
		detail, _ := r.Payload["detail"].(map[string]any)
		if detail["arrival_path"] != string(arrivalReconcile) {
			t.Fatalf("record sig %s arrival_path = %v, want reconcile", r.Sig, detail["arrival_path"])
		}
	}
	if bootMarked != n {
		t.Fatalf("%d records carry boot_stabilized, want %d (every boot observation recorded)", bootMarked, n)
	}
	if got := h.s.suppressed.Load(); got != n {
		t.Fatalf("Suppressed = %d, want %d: the suppression counter must stay honest", got, n)
	}
	if rt := h.s.rt[types.SenDBus]; rt != nil && rt.drops.Load() != 0 {
		t.Fatalf("drops = %d, want 0: a deferred observation is recorded, never dropped", rt.drops.Load())
	}
	if breakerTripped(h, "rule:dbus_unit_failed") || breakerTripped(h, "global") || breakerTripped(h, "source:dbus") {
		t.Fatal("a breaker tripped on the boot burst: the §3.8 trip table is normative and must never engage here")
	}
}

// TestBootBacklogDrainsUnderTheNamedCapAndNeverReForms is the drain half: the
// 21 held units admit 5 per 100s pacing interval, each admitted unit opens
// exactly its one real incident, later re-sights of an admitted unit do NOT
// re-fire (its story is open — re-fire chorus pinned), and the total across
// the drain is exactly 21. Deferral must not become a mask, and the storm
// must not re-form at t+20m when every admitted unit's cooldown expires.
func TestBootBacklogDrainsUnderTheNamedCapAndNeverReForms(t *testing.T) {
	h := bootStormHarness(t)

	const n = 21
	units := failedUnits(n)
	if fired := reconcileSweep(t, h, units); len(fired) != 0 {
		t.Fatalf("precondition: the boot sweep fired %d, want 0", len(fired))
	}

	sweeps := 0
	total := 0
	maxPerSweep := 0
	for total < n && sweeps < 12 {
		h.advance(60 * time.Second)
		sweeps++
		fired := reconcileSweep(t, h, units)
		total += len(fired)
		if len(fired) > maxPerSweep {
			maxPerSweep = len(fired)
		}
		if len(fired) > bootAdmitPerSweep {
			t.Fatalf("sweep %d admitted %d incidents, cap is %d per drain interval", sweeps, len(fired), bootAdmitPerSweep)
		}
		if breakerTripped(h, "rule:dbus_unit_failed") || breakerTripped(h, "global") {
			t.Fatal("a breaker tripped during the drain: the storm re-formed")
		}
	}
	if total != n {
		t.Fatalf("%d of %d units got their incident after %d sweeps: deferral must not become a silent mask", total, n, sweeps)
	}
	if maxPerSweep > bootAdmitPerSweep {
		t.Fatalf("max incidents in one sweep = %d, want <= %d (the named cap)", maxPerSweep, bootAdmitSweepGuard(t))
	}
	// Keep resighting the persistent failures well past every cooldown
	// expiry (5m): re-fires would re-form the storm and trip the breaker.
	for i := 0; i < 6; i++ {
		h.advance(5 * time.Minute)
		if fired := reconcileSweep(t, h, units); len(fired) != 0 {
			t.Fatalf("a re-sight %d re-fired %d incidents: the boot storm re-formed after admission", i, len(fired))
		}
		if breakerTripped(h, "") {
			t.Fatal("a breaker tripped on the post-drain re-sight stream")
		}
	}
}

// bootAdmitSweepGuard keeps the cap name referenced from the drain test's
// failure message without a second constant.
func bootAdmitSweepGuard(t *testing.T) int {
	t.Helper()
	return bootAdmitPerSweep
}

// TestBootStabilizationDoesNotMaskANewFailure is the other direction (AC-B):
// a genuinely new unit failing after the stabilization window still opens its
// incident and fires the rule in the very sweep it first appears — the gate
// holds only boot inventory, never live state.
func TestBootStabilizationDoesNotMaskANewFailure(t *testing.T) {
	h := bootStormHarness(t)

	// Boot posture: one pre-existing failed unit, deferred by the gate.
	if fired := reconcileSweep(t, h, failedUnits(1)); len(fired) != 0 {
		t.Fatalf("boot inventory fired %d incidents, want 0 (deferred)", len(fired))
	}

	// Past the stabilization window a NEW unit is first seen failing, beside
	// the still-failed boot unit.
	h.advance(300 * time.Second)
	units := append(failedUnits(1), unitStatus{
		Name:        "new-worker.service",
		LoadState:   "loaded",
		ActiveState: "failed",
		SubState:    "failed",
	})
	fired := reconcileSweep(t, h, units)
	var newFired bool
	for _, r := range fired {
		if r.Payload["subject"] == "new-worker.service" {
			newFired = true
			if v, _ := r.Payload["boot_stabilized"].(bool); v {
				t.Fatal("a post-window failure must not carry boot_stabilized")
			}
		}
	}
	if !newFired {
		t.Fatalf("a genuinely new failure after boot did not fire (fired=%d): the gate must hold only boot inventory", len(fired))
	}
	// The drained boot unit fired too (its real incident, under the cap) —
	// the new failure's fire is on TOP of the drain, not instead of it.
	if got := len(bootFires(h, "dbus_unit_failed")); got != 2 {
		t.Fatalf("fired = %d, want 2 (one drained boot unit + one genuinely new failure)", got)
	}
}

// TestBootWindowDoesNotHoldLiveSignals: a PropertiesChanged transition to
// failed during the boot window is a live edge, not boot inventory — the
// signal path must fire immediately, exactly as before TRBL-094.
func TestBootWindowDoesNotHoldLiveSignals(t *testing.T) {
	h := bootStormHarness(t)
	ctx := context.Background()

	res := h.s.dbState.merge.arrival("system", "live-worker.service", arrivalProperties, "failed", "", "", h.now())
	if !res.Opened {
		t.Fatal("precondition: the live arrival must open its merge story")
	}
	h.s.emitDBusOutcome(ctx, res, "system", "failed", "failed", arrivalProperties)

	recs := bootFires(h, "dbus_unit_failed")
	if len(recs) != 1 {
		t.Fatalf("a live PropertiesChanged failure inside the boot window fired %d incidents, want 1 (the gate is reconcile-only)", len(recs))
	}
	if v, _ := recs[0].Payload["boot_stabilized"].(bool); v {
		t.Fatal("a live signal must not be marked boot_stabilized")
	}
}

// TestBootRecoveryClearsTheGate: a held boot unit that RECOVERS before its
// admission must open nothing, and a genuine later failure of the same unit
// must fire fresh — recovery is the exit, not a perma-suppression.
func TestBootRecoveryClearsTheGate(t *testing.T) {
	h := bootStormHarness(t)

	// Two boot units; batch001 recovers before the first drain.
	units := failedUnits(2)
	if fired := reconcileSweep(t, h, units); len(fired) != 0 {
		t.Fatalf("boot sweep fired %d, want 0", len(fired))
	}
	h.advance(100 * time.Second)
	only000 := failedUnits(1) // batch001 left the failed set
	fired := reconcileSweep(t, h, only000)
	if len(fired) != 1 || fired[0].Payload["subject"] != "batch000.service" {
		t.Fatalf("the first drain admitted %v, want exactly batch000.service (FIFO) — batch001 recovered and must not fire", fired)
	}
	// batch000's story is now open: its re-sights do not re-fire.
	h.advance(60 * time.Second)
	if fired := reconcileSweep(t, h, only000); len(fired) != 0 {
		t.Fatalf("the admitted unit's re-sight re-fired %d incidents, want 0", len(fired))
	}
	// batch001 recovered during the boot window. A genuine later failure
	// (after the window) must fire fresh — no perma-suppression.
	h.advance(400 * time.Second)
	newFail := []unitStatus{units[1]}
	fired = reconcileSweep(t, h, newFail)
	if len(fired) != 1 || fired[0].Payload["subject"] != "batch001.service" {
		t.Fatalf("a genuine post-boot failure of a recovered unit did not fire fresh: fired=%d", len(fired))
	}
}

// TestBootAdmissionReholdsOnBatchWriteFailure: a failed batch write reholds
// the released slice, so the admission retries on the next sweep instead of
// the units being silently masked.
func TestBootAdmissionReholdsOnBatchWriteFailure(t *testing.T) {
	h, calls := batchHarness(t)
	h.writeRules("10.toml", dbusUnitFailedRule)
	h.mustReload()
	h.s.dbState.boot = newBootStabilizer(h.now(), bootStabilizeWindow, bootDrainInterval)

	units := failedUnits(2)
	if fired := reconcileSweep(t, h, units); len(fired) != 0 {
		t.Fatalf("boot sweep fired %d, want 0", len(fired))
	}
	// The drain's batch write fails: no admission may land, and the released
	// slice must be reheld for the next sweep instead of being masked.
	h.advance(100 * time.Second)
	calls.err = errors.New("disk on fire")
	fired := reconcileSweep(t, h, units)
	if len(fired) != 0 {
		t.Fatalf("a failed drain batch admitted %d incidents, want 0", len(fired))
	}
	if h.s.dbState.boot.heldCount() != 2 {
		t.Fatalf("held = %d, want 2: the released slice must be reheld for the next sweep", h.s.dbState.boot.heldCount())
	}
	// The next sweep's batch write succeeds: both units admit, in order.
	calls.err = nil
	h.advance(60 * time.Second)
	fired = reconcileSweep(t, h, units)
	if len(fired) != 2 {
		t.Fatalf("the retry admitted %d, want 2", len(fired))
	}
}
