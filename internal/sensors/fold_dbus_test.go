package sensors

import (
	"context"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// dbusOutcomeEvent builds the event shape emitDBusOutcome produces
// (internal/sensors/dbus.go): one merge-tracker arrival per unit sighted. The
// foldable_repeat marker is what TRBL-075 added, so the caller decides whether
// this arrival is a counted-only repeat (foldable) or one that opened,
// attached or reopened an incident (never foldable).
func dbusOutcomeEvent(scope, substate string, counted, opened bool) types.SensorEvent {
	d := map[string]any{
		"unit_key":          scope,
		"unit_active_state": "failed",
		"unit_substate":     substate,
		"failure_class":     "failed",
		"crash_loop":        false,
		"arrival_path":      string(arrivalReconcile),
		"manager":           "",
		"exit_code":         0,
		"count":             float64(1),
		"merge_arrivals":    1,
		"merged":            false,
		"reopen_count":      0,
		"severity":          "high",
		"sensors_arrivals":  1,
		"sensors_merges":    1,
		"value":             float64(1),
		"unit_field":        "",
		"msg":               "",
		"substr":            "",
		"window_s":          0,
		"age_s":             0,
	}
	if counted && !opened {
		d["foldable_repeat"] = true
	}
	return types.SensorEvent{
		ID:     types.NewID(types.PEv),
		TS:     types.FormatUTC(time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)),
		Sensor: types.SenDBus,
		Scope:  scope,
		Value:  float64(1),
		Unit:   "",
		Detail: d,
		Sig:    sigFor(types.SrcDBus, "", scope, "failed"),
	}
}

// TestFoldAbsorbsCountedOnlyDBusRepeats is the TRBL-075 claim: a D-Bus arrival
// the merge tracker merely counted into an already-open story (Counted, no
// Opened/Attached/Reopened) with an unchanged signature is absorbed by the
// §3.8a fold instead of writing its own record — one D-Bus signature repeating
// 15x inside the merge window wrote 15 ledger records before the marker.
func TestFoldAbsorbsCountedOnlyDBusRepeats(t *testing.T) {
	h := newHarness(t, types.ConfigValue{Key: "sensors.sample_fold_window", Value: "5m"})
	ctx := context.Background()
	ev := dbusOutcomeEvent("unit-a", "auto-restart", true, false)
	sig := ev.Sig.String()
	start := h.now()

	// The idle-host observation TRBL-075 measured: 15 sub-second repeats of
	// the same signature in two minutes, all inside one fold window.
	const n = 15
	for i := 0; i < n; i++ {
		if i > 0 {
			h.advance(8 * time.Second)
		}
		h.s.handleEvent(ctx, ev)
	}
	if got := len(recordsForSig(h, sig)); got != 0 {
		t.Fatalf("counted-only dbus repeats wrote %d records inside the window, want 0", got)
	}
	if got := h.s.fold.openCount(); got != 1 {
		t.Fatalf("open folds = %d, want 1 for one dbus signature", got)
	}

	// The window-past arrival closes the fold and writes exactly one record:
	// 15 repeats × 8s span 112s, so one more full window-step puts the next
	// arrival past firstTS + 5m.
	h.advance(5 * time.Minute)
	h.s.handleEvent(ctx, ev)
	recs := recordsForSig(h, sig)
	if len(recs) != 1 {
		t.Fatalf("%d records for %d counted-only dbus arrivals, want exactly 1", len(recs), n+1)
	}
	rec := recs[0]
	if got := foldCount(t, rec.Payload); got < 2 {
		t.Fatalf("fold record count = %d, want >= 2", got)
	}
	if rec.Payload["fold"] != true {
		t.Fatalf("fold record carries fold=%v, want true", rec.Payload["fold"])
	}
	if got := rec.Payload["last_ts"]; got == "" || got == start.Format("2006-01-02T15:04:05Z") {
		t.Fatalf("fold record last_ts = %v, want the last observation's time", got)
	}
}

// TestFoldNeverAbsorbsAnArrivalThatChangedIncidentState pins the other half:
// an arrival that OPENED, ATTACHED or REOPENED an incident decided something
// outside the fold, carries no marker, and is written immediately — with the
// fold for its signature closed first (handleEvent's closeSig path), so the
// ledger keeps the chronology.
func TestFoldNeverAbsorbsAnArrivalThatChangedIncidentState(t *testing.T) {
	h := newHarness(t, types.ConfigValue{Key: "sensors.sample_fold_window", Value: "5m"})
	ctx := context.Background()

	// Counted-only repeats first: they open the fold for this signature.
	counted := dbusOutcomeEvent("unit-b", "auto-restart", true, false)
	sig := counted.Sig.String()
	h.s.handleEvent(ctx, counted)
	h.advance(8 * time.Second)
	h.s.handleEvent(ctx, counted)
	if got := len(recordsForSig(h, sig)); got != 0 {
		t.Fatalf("setup: counted repeats wrote %d records, want 0 (still folded)", got)
	}

	// An opened arrival with the SAME signature: never folded, and the open
	// fold for the signature is closed before its record is written.
	opened := dbusOutcomeEvent("unit-b", "failed", false, true)
	h.s.handleEvent(ctx, opened)
	recs := recordsForSig(h, sig)
	if len(recs) != 2 {
		t.Fatalf("%d records after an opening arrival, want 2 (the closed fold + the immediate record)", len(recs))
	}
	foldedRec, openRec := recs[0], recs[1]
	if foldedRec.Payload["fold"] != true {
		t.Fatalf("first record fold=%v, want true (the fold the arrival closed)", foldedRec.Payload["fold"])
	}
	if openRec.Payload["fold"] != nil {
		t.Fatalf("opening arrival was folded (fold=%v), want an immediate record", openRec.Payload["fold"])
	}
	if got := h.s.fold.openCount(); got != 0 {
		t.Fatalf("open folds = %d after the closeSig path, want 0", got)
	}

	// The same shape attached/reopened: still no marker, still immediate.
	attached := dbusOutcomeEvent("unit-b", "auto-restart", false, false)
	attached.Detail["merged"] = true
	h.s.handleEvent(ctx, attached)
	recs = recordsForSig(h, sig)
	if len(recs) != 3 {
		t.Fatalf("%d records after an attaching arrival, want 3", len(recs))
	}
	if recs[2].Payload["fold"] != nil {
		t.Fatalf("attaching arrival was folded (fold=%v), want an immediate record", recs[2].Payload["fold"])
	}
}

// TestFoldableObservationKeepsPSIAndRejectsUnmarkedDBus is the predicate-level
// pin: the psi path's sample_backed gate is unchanged, and a D-Bus outcome
// without the foldable_repeat marker (the Opened/Attached/Reopened shapes, and
// every non-dbus non-sampled sensor) stays non-foldable.
func TestFoldableObservationKeepsPSIAndRejectsUnmarkedDBus(t *testing.T) {
	draft := types.RecordDraft{}
	backed := sampledEvent("cpu", 12.5)
	if !foldableObservation(backed, draft) {
		t.Fatal("a sample_backed observation must stay foldable (psi path unchanged)")
	}
	unmarked := dbusOutcomeEvent("unit-c", "failed", false, true)
	if foldableObservation(unmarked, draft) {
		t.Fatal("an unmarked dbus outcome (opened/attached/reopened) must not be foldable")
	}
	marked := dbusOutcomeEvent("unit-c", "auto-restart", true, false)
	if !foldableObservation(marked, draft) {
		t.Fatal("a counted-only dbus outcome (foldable_repeat) must be foldable")
	}
	wake := sampledEvent("cpu", 12.5)
	wake.Wake = true
	if foldableObservation(wake, draft) {
		t.Fatal("a wake must never be foldable")
	}
}
