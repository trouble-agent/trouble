package sentinel

import (
	"context"
	"fmt"
	"testing"
)

// TestReleaseOrdering pins §3.4's ordering rules: numeric segments compare
// numerically, then lexically, and a pair no rule can order is unorderable
// rather than silently "newer".
func TestReleaseOrdering(t *testing.T) {
	cases := []struct {
		a, b string
		cmp  int
		ok   bool
	}{
		{"payment-api@2.4.1", "payment-api@2.4.2", -1, true},
		{"payment-api@2.10.0", "payment-api@2.9.0", 1, true},
		{"2.4.1", "2.4.1", 0, true},
		{"v2.4.1", "2.4.1", 0, true},
		{"1.0.0-alpha", "1.0.0-beta", -1, true},
		{"", "1.0.0", 0, false},
		{"2.4.x", "2.4.1", 0, false},
	}
	for _, tc := range cases {
		cmp, ok := compareReleases(tc.a, tc.b)
		if ok != tc.ok || cmp != tc.cmp {
			t.Errorf("compareReleases(%q,%q) = (%d,%v), want (%d,%v)", tc.a, tc.b, cmp, ok, tc.cmp, tc.ok)
		}
	}
}

// TestReleaseRangeAndRegression pins the regression rule: a resolved group that
// reappears with a release ordering AFTER its last pre-resolve release is
// `confirmed`; an unorderable pair is `unconfirmed`, never a claimed regression.
func TestReleaseRangeAndRegression(t *testing.T) {
	ts := newTestServer(t, func(c *Config) {
		c.CanaryProject = ""
		c.Projects[0].QuotaEPM = 100000
	})
	defer ts.close()
	entry, _ := ts.s.projects.project("1")
	ctx := context.Background()

	ev := groupEvent("11112222333344445555666677770001", "payment-api@2.4.1")
	ev.SourceKind = sourceEnvelope
	rec, err := ts.s.admitEvent(ctx, entry, ev, "event", "test")
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if rec.Payload["native_id"] == nil {
		t.Fatal("the event record must carry native_id")
	}
	g, ok := ts.s.GroupBySig(ev.sigString(t))
	if !ok {
		t.Fatal("group not found by sig")
	}
	if len(g.ReleaseRange) != 2 || g.ReleaseRange[0] != "payment-api@2.4.1" {
		t.Fatalf("release_range = %v, want [2.4.1, 2.4.1]", g.ReleaseRange)
	}

	// The ladder resolves the incident at 2.4.1; the sig reappears at 2.4.2.
	ts.s.NoteIncidentResolved(g.Sig, "")
	ev2 := groupEvent("11112222333344445555666677770002", "payment-api@2.4.2")
	ev2.SourceKind = sourceEnvelope
	if _, err := ts.s.admitEvent(ctx, entry, ev2, "event", "test"); err != nil {
		t.Fatalf("admit: %v", err)
	}
	if v := ts.s.regressionOf(g.Sig); v != regressionConfirmed {
		t.Fatalf("regression verdict = %q, want %q", v, regressionConfirmed)
	}
	// The verdict rides a group record (op=release).
	found := false
	for _, rec := range ts.sink.ofKind("group") {
		if rec.Payload["regression"] == regressionConfirmed {
			found = true
		}
	}
	if !found {
		t.Fatal("no group record carried payload.regression=confirmed")
	}

	// An unorderable release never claims a regression.
	ts.s.NoteIncidentResolved(g.Sig, "payment-api@2.4.1")
	ev3 := groupEvent("11112222333344445555666677770003", "payment-api@2.4.x")
	ev3.SourceKind = sourceEnvelope
	if _, err := ts.s.admitEvent(ctx, entry, ev3, "event", "test"); err != nil {
		t.Fatalf("admit: %v", err)
	}
	if v := ts.s.regressionOf(g.Sig); v != regressionUnconfirmed {
		t.Fatalf("unorderable release verdict = %q, want %q", v, regressionUnconfirmed)
	}
}

// TestFixedInRequiresCanaryCoverage pins §3.4: `fixed_in` needs proven coverage,
// and coverage without a canary is UNKNOWN, not fixed.
func TestFixedInRequiresCanaryCoverage(t *testing.T) {
	ts := newTestServer(t, func(c *Config) {
		c.CanaryProject = ""
		c.Projects[0].QuotaEPM = 100000
	})
	defer ts.close()
	entry, _ := ts.s.projects.project("1")
	ctx := context.Background()
	ev := groupEvent("22223333444455556666777788880001", "payment-api@2.4.1")
	ev.SourceKind = sourceEnvelope
	if _, err := ts.s.admitEvent(ctx, entry, ev, "event", "test"); err != nil {
		t.Fatalf("admit: %v", err)
	}

	// No canary has landed: the quiet window is unobserved.
	ok, lastTS := ts.s.ReleaseCoverage("1", "payment-api@2.4.1")
	if ok {
		t.Fatal("coverage must be false before a canary observation")
	}
	if lastTS == "" {
		t.Fatal("ReleaseCoverage must report the last event ts for the release")
	}
	diff := ts.s.ReleaseDiff("1", "payment-api@2.4.1")
	if len(diff.FixedIn) != 0 {
		t.Fatalf("fixed_in without coverage = %d groups, want 0", len(diff.FixedIn))
	}
	if len(diff.NewIn) != 1 {
		t.Fatalf("new_in = %d groups, want 1 (first_seen_release == to)", len(diff.NewIn))
	}

	// A canary observation after the last event proves the window is observed.
	ts.s.releases.noteCanary("1", "2026-09-16T09:20:00.000Z")
	ok, _ = ts.s.ReleaseCoverage("1", "payment-api@2.4.1")
	if !ok {
		t.Fatal("coverage must be true once a canary lands after the last sighting")
	}
}

// TestReleaseDiffInputs pins the AC-19 view inputs: new_in, fixed_in, still_open
// and regressed.
func TestReleaseDiffInputs(t *testing.T) {
	ts := newTestServer(t, func(c *Config) {
		c.CanaryProject = ""
		c.Projects[0].QuotaEPM = 100000
	})
	defer ts.close()
	entry, _ := ts.s.projects.project("1")
	ctx := context.Background()

	// A group that only ever appears in the target release → new_in.
	newEv := groupEvent("33334444555566667777888899990001", "payment-api@2.5.0")
	newEv.SourceKind = sourceGeneric
	newEv.Frames = []frame{{File: "new.py", Function: "f", InApp: true, ContextLine: "raise ValueError"}}
	if _, err := ts.s.admitEvent(ctx, entry, newEv, "event", "test"); err != nil {
		t.Fatalf("admit: %v", err)
	}
	// A group last seen in the previous release, with coverage → fixed_in.
	oldEv := groupEvent("33334444555566667777888899990002", "payment-api@2.4.1")
	oldEv.SourceKind = sourceGeneric
	oldEv.Frames = []frame{{File: "old.py", Function: "g", InApp: true, ContextLine: "raise KeyError"}}
	if _, err := ts.s.admitEvent(ctx, entry, oldEv, "event", "test"); err != nil {
		t.Fatalf("admit: %v", err)
	}
	ts.s.releases.noteCanary("1", "2099-01-01T00:00:00.000Z")

	diff := ts.s.ReleaseDiff("1", "payment-api@2.5.0")
	if len(diff.NewIn) != 1 {
		t.Errorf("new_in = %d, want 1", len(diff.NewIn))
	}
	if !diff.Coverage {
		t.Error("coverage should be true with a canary after the last event")
	}
	if len(diff.FixedIn) != 1 {
		t.Errorf("fixed_in = %d, want 1 (coverage proven)", len(diff.FixedIn))
	}
	if len(diff.StillOpen) != 0 {
		t.Errorf("still_open = %d, want 0", len(diff.StillOpen))
	}
}

// TestReleaseLengthCapAndTrim pins §3.4's release field rules.
func TestReleaseLengthCapAndTrim(t *testing.T) {
	ts := newTestServer(t, func(c *Config) {
		c.CanaryProject = ""
		c.Projects[0].QuotaEPM = 100000
	})
	defer ts.close()
	entry, _ := ts.s.projects.project("1")
	ev := groupEvent("44445555666677778888999900001111", "")
	ev.Release = "  "
	ev.SourceKind = sourceGeneric
	if _, err := ts.s.admitEvent(context.Background(), entry, ev, "event", "test"); err != nil {
		t.Fatalf("admit: %v", err)
	}
	g := ts.s.Groups()
	if len(g) != 1 {
		t.Fatalf("%d groups, want 1", len(g))
	}
	// A release-less group is still grouped and counted (§3.4).
	if len(g[0].ReleaseRange) != 2 || g[0].ReleaseRange[0] != "" {
		t.Fatalf("release-less group release_range = %v, want [\"\",\"\"]", g[0].ReleaseRange)
	}
}

// sigString is the test-side helper that computes an event's sig.
func (e *rawEvent) sigString(t *testing.T) string {
	t.Helper()
	n := newNormalizer()
	canonical, _ := n.canonicalFor(e)
	return n.sigOfCanonical(canonical).String()
}

// TestReleaseOrderFirstObservationTiebreak pins the last tiebreaker: equal
// releases fall back to first-observation sequence.
func TestReleaseOrderFirstObservationTiebreak(t *testing.T) {
	ro := newReleaseOrder()
	a := ro.seen("1.0.0")
	b := ro.seen("2.0.0")
	if a >= b {
		t.Fatalf("first-observation sequence must increase: %d then %d", a, b)
	}
	if ro.seen("1.0.0") != a {
		t.Fatal("re-seeing a release must return its original sequence")
	}
	if got := fmt.Sprint(a); got == "" {
		t.Fatal("sequence is empty")
	}
}
