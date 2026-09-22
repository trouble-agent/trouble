package sentinel

import (
	"context"
	"fmt"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// codeplane_test.go is the SPEC-04 §7 row for AC-31 (§3.9a): bundle assembly
// on a threshold crossing, the release-mismatch discard rule, the one-gap
// memoization, and the accessor's edge cases.

// admitOne admits one grouping event for project 1 at the given release.
func admitOne(t *testing.T, ts *testServer, i int, release string) error {
	t.Helper()
	entry, _ := ts.s.projects.project("1")
	ev := groupEvent(fmt.Sprintf("%032x", i+1), release)
	rec, err := ts.s.admitEvent(context.Background(), entry, ev, "event", "test")
	if err != nil {
		return err
	}
	_ = rec
	return nil
}

// TestCodeplane31AssemblyCarriesEverySentinelField names AC-31: the threshold
// crossing assembles a bundle with every field the sentinel owns, and nothing
// it is not (the sensor half stays empty).
func TestCodeplane31AssemblyCarriesEverySentinelField(t *testing.T) {
	ts := newTestServer(t, func(c *Config) {
		c.CanaryProject = ""
		c.Projects[0].QuotaEPM = 1_000_000
	})
	defer ts.close()

	if err := admitOne(t, ts, 0, "payment-api@2.4.1"); err != nil {
		t.Fatalf("admitEvent: %v", err)
	}
	recs := ts.sink.ofKind(types.KGroup)
	if len(recs) == 0 {
		t.Fatalf("no group record written")
	}
	var b *types.CodeplaneContext
	for _, r := range recs {
		if r.Codeplane != nil {
			b = r.Codeplane
			break
		}
	}
	if b == nil {
		t.Fatalf("no group record carries a bundle")
	}
	if b.Side != "sentinel" {
		t.Fatalf("Side = %q, want sentinel", b.Side)
	}
	if b.Sig == "" || b.GroupID == "" || b.Project != "1" {
		t.Fatalf("bundle identity incomplete: sig=%q grp=%q project=%q", b.Sig, b.GroupID, b.Project)
	}
	if b.Release != "payment-api@2.4.1" {
		t.Fatalf("Release = %q, want payment-api@2.4.1", b.Release)
	}
	if len(b.Recent) == 0 || b.Recent[0].Count != 1 {
		t.Fatalf("Recent must carry the group's counter, got %+v", b.Recent)
	}
	if b.Recent[0].Sig != b.Sig {
		t.Fatalf("Recent[0].Sig = %q, want the group's sig", b.Recent[0].Sig)
	}
	if b.TS == "" {
		t.Fatalf("TS must be set")
	}
	// The sensor half is never written here (disjoint planes).
	if b.RuleID != "" || len(b.Readings) != 0 {
		t.Fatalf("the sentinel half must stay empty of sensor fields: %+v", b)
	}
}

// TestCodeplane31ReleaseMismatchDiscardsAndGaps names AC-31: a bundle whose
// release disagrees with the running release is discarded at assembly —
// exactly one gap record for the pair, and the admission proceeds unchanged
// (0 HTTP responses of its own).
func TestCodeplane31ReleaseMismatchDiscardsAndGaps(t *testing.T) {
	ts := newTestServer(t, func(c *Config) {
		c.CanaryProject = ""
		c.Projects[0].QuotaEPM = 1_000_000
	})
	defer ts.close()

	// The group opens at release 2.4.1 and a SECOND group (a different stack)
	// moves the project's running release to 2.4.2, so the first group's
	// bundled release becomes stale while its own digest stays untouched.
	if err := admitOne(t, ts, 0, "payment-api@2.4.1"); err != nil {
		t.Fatalf("admitEvent: %v", err)
	}
	var firstDigest string
	for _, g := range ts.s.Groups() {
		firstDigest = g.Digest
	}
	// A structurally different stack → a different digest → a second group.
	other := groupEvent(fmt.Sprintf("%032x", 2), "payment-api@2.4.2")
	other.Message = "invoice generation exploded: tenant loop timeout"
	other.Frames = []frame{
		{File: "invoice.py", Function: "render", InApp: true, ContextLine: "item = billing.render(timeout=1)"},
	}
	entry, _ := ts.s.projects.project("1")
	if _, err := ts.s.admitEvent(context.Background(), entry, other, "event", "test"); err != nil && err.Code != "" {
		t.Fatalf("admitEvent 2: %v", err)
	}

	// Directly assemble the stale-release bundle the way the rule sees it:
	// the live group still describes 2.4.1 while the project's running
	// release is 2.4.2.
	if firstDigest == "" {
		t.Fatalf("no live group")
	}
	st, ok := ts.s.groups.state(firstDigest)
	if !ok {
		t.Fatalf("no live group state")
	}
	b, keep := ts.s.assembleCodeplane(firstDigest, st, "1")
	if keep {
		t.Fatalf("stale-release bundle must be refused, got %+v", b)
	}
	// One gap record only, and re-assembling inside the window writes no more.
	ts.s.noteCodeplaneMismatch(st.grp.Sig, st.grp.ID, "1", "payment-api@2.4.1")
	var n int
	for _, g := range ts.sink.ofKind(types.KGap) {
		if c, _ := g.Payload["cause"].(string); c == "codeplane_release_mismatch" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d codeplane_release_mismatch gap records, want exactly 1", n)
	}
}

// TestCodeplane31EmptyReleaseIsKept names AC-31 edge case: an absent release
// is an absent fact, never a mismatch — the bundle is kept with Release "".
func TestCodeplane31EmptyReleaseIsKept(t *testing.T) {
	ts := newTestServer(t, func(c *Config) {
		c.CanaryProject = ""
		c.Projects[0].QuotaEPM = 1_000_000
	})
	defer ts.close()
	if err := admitOne(t, ts, 0, ""); err != nil {
		t.Fatalf("admitEvent: %v", err)
	}
	recs := ts.sink.ofKind(types.KGroup)
	kept := false
	for _, r := range recs {
		if r.Codeplane != nil {
			kept = true
			if r.Codeplane.Release != "" {
				t.Fatalf("Release = %q, want empty", r.Codeplane.Release)
			}
		}
	}
	if !kept {
		t.Fatalf("a release-less group keeps its bundle")
	}
}

// TestCodeplaneForNoLinkMisses pins the stale-entry rule: a sig with no live
// link returns ok=false and the sensor-born admission proceeds with its own
// rule context only.
func TestCodeplaneForNoLinkMisses(t *testing.T) {
	ts := newTestServer(t, func(c *Config) { c.CanaryProject = "" })
	defer ts.close()
	sig, err := types.ParseSig("sentinel:sha256v1:9f2c1d3e4b5a6c7d")
	if err != nil {
		t.Fatalf("parse sig: %v", err)
	}
	if _, ok := ts.s.CodeplaneFor(sig); ok {
		t.Fatalf("a sig without a link must miss")
	}
}
