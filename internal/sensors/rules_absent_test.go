package sensors

// rules_absent_test.go is TRBL-022's sensor-level proof of SPEC-03 §3.7b: an
// ABSENT rules directory is not a failure — the inotify sensor stays ENABLED and
// NOT degraded, the shipped defaults are active, and the absence is NAMED (one
// informational record + a non-empty SensorHealth Reason) instead of being
// reported as TROUBLE-SENSORS-025 and lifting /health.json to `degraded`.
//
// The other half is what keeps this from being "the check was deleted": a path
// that EXISTS and cannot be watched still degrades with TROUBLE-SENSORS-025, and
// a rules.d that exists but is malformed still refuses through the unchanged
// §3.5/§3.7 path (pinned by TestRuleDirLoadsSortedAndFallsBackToDefaults and
// TestReloadInvalidFileKeepsPreviousSet).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// inotifyRow returns the inotify row of Health(), and fails when it is missing —
// a sensor that is not in the health surface cannot be asserted on.
func inotifyRow(t *testing.T, h *harness) types.SensorHealth {
	t.Helper()
	for _, r := range h.s.Health() {
		if r.Sensor == types.SenInotify {
			return r
		}
	}
	t.Fatalf("the inotify sensor is missing from Health(): %+v", h.s.Health())
	return types.SensorHealth{}
}

func notes(t *testing.T, h *harness) []types.Record {
	t.Helper()
	return h.recordsWhere(func(r types.Record) bool {
		k, _ := r.Payload["kind"].(string)
		return k == "sensor_note"
	})
}

// TestMissingRulesDirLeavesInotifyHealthy: the AC1 half — a rules directory that
// does not exist keeps every sensor up, the shipped defaults active, and the
// condition named without a refusal code.
func TestMissingRulesDirLeavesInotifyHealthy(t *testing.T) {
	h := newHarness(t)
	rulesDir := filepath.Join(h.dir, "rules.d")
	if _, err := os.Stat(rulesDir); err == nil {
		t.Fatalf("premise: %s exists, so the absent case is not being exercised", rulesDir)
	}

	if err := h.s.startInotify(context.Background()); err != nil {
		t.Fatalf("startInotify: %v", err)
	}
	defer h.s.stopInotify()

	row := inotifyRow(t, h)
	if !row.Enabled {
		t.Errorf("inotify Enabled = false for an absent rules dir: the sensor has nothing to WATCH, not nothing to do (SPEC-03 §3.7b)")
	}
	if row.Degraded {
		t.Errorf("inotify Degraded = true for an absent rules dir, reason %q: absence is a first-run state, not a capability failure", row.Reason)
	}
	if !strings.Contains(row.Reason, rulesDir) {
		t.Errorf("the inotify Reason must NAME the absent directory, got %q", row.Reason)
	}
	if strings.Contains(row.Reason, "TROUBLE-SENSORS-") {
		t.Errorf("the absence must not be reported as a TROUBLE-SENSORS-* refusal, got %q", row.Reason)
	}

	// No TROUBLE-SENSORS-* code may be emitted for the absence — the health
	// status is aggregated from these codes elsewhere, so a stray 025 here is the
	// TRBL-022 bug in a different costume.
	for _, r := range h.snapshot() {
		if code, _ := r.Payload["error_code"].(string); code != "" {
			t.Errorf("an absent rules dir emitted error_code %q (detail %v)", code, r.Payload["detail"])
		}
	}

	// The condition is still visible: exactly one informational record, no code,
	// naming the path.
	got := notes(t, h)
	if len(got) != 1 {
		t.Fatalf("expected exactly one informational sensor_note record, got %d: %+v", len(got), got)
	}
	if code, _ := got[0].Payload["error_code"].(string); code != "" {
		t.Errorf("the informational record carries error_code %q, want none", code)
	}
	if p, _ := got[0].Payload["path"].(string); p != rulesDir {
		t.Errorf("the informational record names path %q, want %q", p, rulesDir)
	}
	if detail, _ := got[0].Payload["detail"].(string); !strings.Contains(detail, rulesDir) {
		t.Errorf("the informational record's detail does not name the directory: %q", detail)
	}
	if fire, _ := got[0].Payload["fire"].(bool); fire {
		t.Error("the informational record must never fire a rule (fire = true)")
	}

	// The shipped defaults are the live set.
	if n := len(h.s.Rules()); n != 9 {
		t.Errorf("the live rule set has %d rules, want the 9 shipped defaults", n)
	}

	// The 15m recheck re-derives the condition, so a re-check must not duplicate
	// the record (no record storm from a directory that stays absent).
	h.s.ensureRulesWatch()
	if again := notes(t, h); len(again) != 1 {
		t.Errorf("a re-check emitted %d informational records, want the original 1", len(again))
	}
}

// TestRulesWatchIsEstablishedOnceTheDirectoryExists: the watch is DEFERRED, not
// dropped — the always-on §3.7 hot-reload trigger is armed as soon as rules.d
// exists.
func TestRulesWatchIsEstablishedOnceTheDirectoryExists(t *testing.T) {
	h := newHarness(t)
	rulesDir := filepath.Join(h.dir, "rules.d")

	if err := h.s.startInotify(context.Background()); err != nil {
		t.Fatalf("startInotify: %v", err)
	}
	defer h.s.stopInotify()

	if h.s.rulesWatchArmed() {
		t.Fatal("premise: no watch can be armed for a directory that does not exist")
	}
	if err := os.MkdirAll(rulesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// This is what the 15m recheck calls on every tick.
	h.s.ensureRulesWatch()
	if !h.s.rulesWatchArmed() {
		t.Fatal("rules.d exists but the always-on watch was not established: the hot-reload trigger would stay lost for the life of the process")
	}
	h.s.inoState.mu.Lock()
	isRules := false
	for _, w := range h.s.inoState.watches {
		if w.path == rulesDir {
			isRules = w.isRules
		}
	}
	h.s.inoState.mu.Unlock()
	if !isRules {
		t.Error("the rules directory watch must carry isRules=true, or a write in it never marks a reload pending")
	}
	// Idempotent: a second pass must not stack a second watch.
	h.s.ensureRulesWatch()
	h.s.inoState.mu.Lock()
	n := 0
	for _, w := range h.s.inoState.watches {
		if w.path == rulesDir {
			n++
		}
	}
	h.s.inoState.mu.Unlock()
	if n != 1 {
		t.Errorf("the rules directory carries %d watches, want exactly 1", n)
	}
}

// TestUnwatchableRulesPathStillDegrades: the other half of TRBL-022 — the check
// was NARROWED (absence is not a failure), not disabled. A rules path that exists
// and cannot be watched keeps TROUBLE-SENSORS-025 and the degraded sensor.
func TestUnwatchableRulesPathStillDegrades(t *testing.T) {
	t.Run("unreadable directory", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root: an unreadable directory is still watchable, so this failure cannot be produced")
		}
		h := newHarness(t)
		rulesDir := filepath.Join(h.dir, "rules.d")
		if err := os.MkdirAll(rulesDir, 0o000); err != nil {
			t.Fatal(err)
		}
		// Registered AFTER t.TempDir()'s cleanup, so it runs (LIFO) before the
		// framework removes the tree.
		t.Cleanup(func() { _ = os.Chmod(rulesDir, 0o755) })

		if err := h.s.startInotify(context.Background()); err != nil {
			t.Fatalf("startInotify: %v", err)
		}
		defer h.s.stopInotify()
		assertDegradedWith025(t, h, rulesDir)
	})

	// A symlink loop is a present path that cannot be stat'd at all. It produces
	// the same failure on any uid, so the "exists and is unusable" branch is
	// pinned even where the unreadable case must be skipped.
	t.Run("symlink loop", func(t *testing.T) {
		h := newHarness(t)
		loop := filepath.Join(h.dir, "rules.d")
		if err := os.Symlink(loop, loop); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := h.s.startInotify(context.Background()); err != nil {
			t.Fatalf("startInotify: %v", err)
		}
		defer h.s.stopInotify()
		assertDegradedWith025(t, h, loop)
	})
}

func assertDegradedWith025(t *testing.T, h *harness, path string) {
	t.Helper()
	row := inotifyRow(t, h)
	if !row.Degraded {
		t.Fatalf("a rules path that exists and cannot be watched (%s) did not degrade the sensor: %+v", path, row)
	}
	if !strings.Contains(row.Reason, string(types.CodeSensors025)) {
		t.Errorf("the degradation reason must carry %s, got %q", types.CodeSensors025, row.Reason)
	}
	if !strings.Contains(row.Reason, path) {
		t.Errorf("the degradation reason must NAME the path, got %q", row.Reason)
	}
	found := false
	for _, r := range h.snapshot() {
		if code, _ := r.Payload["error_code"].(string); code == string(types.CodeSensors025) {
			found = true
		}
	}
	if !found {
		t.Errorf("no record carried error_code %s for %s", types.CodeSensors025, path)
	}
	if len(notes(t, h)) != 0 {
		t.Error("a path that exists must not be reported as an informational absence")
	}
}

// TestReloadAbsentRulesDirIsNotARefusal: absence is not a failure at the reload
// surface either (SPEC-03 §3.7b). A no-op reload must not mint a
// TROUBLE-SENSORS-017/019 refusal and must not strike the loop guard — three
// strikes disable the inotify hot-reload trigger, and on a first-run host there
// is no rules directory to fix.
func TestReloadAbsentRulesDirIsNotARefusal(t *testing.T) {
	h := newHarness(t)
	h.writeRules("10.toml", ruleBody("io_pressure", ">=", "35", "0s"))
	h.mustReload()
	before := h.s.ruleCount()
	if before != 1 {
		t.Fatalf("setup: %d rules", before)
	}
	rulesDir := filepath.Join(h.dir, "rules.d")
	if err := os.RemoveAll(rulesDir); err != nil {
		t.Fatal(err)
	}

	if err := h.s.Reload(context.Background()); err != nil {
		t.Fatalf("a reload with no rules directory must not be a refusal: %v", err)
	}
	if got := h.s.ruleCount(); got != before {
		t.Errorf("the live set changed on an absent-directory reload: %d -> %d", before, got)
	}
	if got := h.s.strikes.Load(); got != 0 {
		t.Errorf("an absent rules directory struck the loop guard %d time(s); three would disable the inotify trigger", got)
	}
	if h.s.inotifyOff.Load() {
		t.Error("an absent rules directory disabled the inotify reload trigger")
	}
	if recs := h.recordsWhere(func(r types.Record) bool {
		k, _ := r.Payload["kind"].(string)
		return k == "rule_reload"
	}); len(recs) != 0 {
		t.Errorf("an absent rules directory produced %d rule_reload refusal record(s): %+v", len(recs), recs)
	}
	got := notes(t, h)
	if len(got) != 1 {
		t.Fatalf("expected exactly one informational record naming the absent directory, got %d", len(got))
	}
	if code, _ := got[0].Payload["error_code"].(string); code != "" {
		t.Errorf("the informational record carries error_code %q, want none", code)
	}
	if !strings.Contains(fmtDetail(got[0]), rulesDir) {
		t.Errorf("the informational record does not name %s: %q", rulesDir, fmtDetail(got[0]))
	}
}

// fmtDetail reads the note's detail field for the assertion messages.
func fmtDetail(r types.Record) string {
	d, _ := r.Payload["detail"].(string)
	return d
}
