package sentinel

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// The §3.4a generic-JSON fold: a retrying reporter (any HTTP client on a
// timeout) sends byte-identical bodies with no event_id, so the §6.6 id-dedup
// never sees the retry — every POST minted a fresh event record and folded it
// into the group as a new occurrence (TRBL-074). These tests pin the fold:
// identical sigs inside the window are accepted 200 and folded into the group
// as suppressed retries, different sigs never fold, expiry opens a new window,
// and `0` disables the fold entirely.

// genericFoldBody is one byte-identical generic body posted N times.
const genericFoldBody = `{"message":"first trouble event","level":"error","release":"0.1.0"}`

// postGenericFold posts the fold fixture and fails the test on any non-200.
func postGenericFold(tb testing.TB, ts *testServer, body string) map[string]any {
	tb.Helper()
	resp := ts.postGeneric(tb, body)
	if resp.StatusCode != http.StatusOK {
		tb.Fatalf("status %d (%s)", resp.StatusCode, readBody(tb, resp))
	}
	var out map[string]any
	if err := json.Unmarshal(readBody(tb, resp), &out); err != nil {
		tb.Fatalf("response body is not a JSON object: %v", err)
	}
	return out
}

// groupRecordsByOp splits the sink's group records by their payload op.
func groupRecordsByOp(tb testing.TB, ts *testServer) map[string]int {
	tb.Helper()
	out := map[string]int{}
	for _, rec := range ts.sink.ofKind(types.KGroup) {
		op, _ := rec.Payload["op"].(string)
		out[op]++
	}
	return out
}

// lastGroupCounters returns the counters snapshot of the last group record.
func lastGroupCounters(tb testing.TB, ts *testServer) types.GroupCounters {
	tb.Helper()
	recs := ts.sink.ofKind(types.KGroup)
	if len(recs) == 0 {
		tb.Fatal("no group records")
	}
	rec := recs[len(recs)-1]
	c, ok := asCounters(rec.Payload["counters"])
	if !ok {
		tb.Fatalf("last group record carries no counters: %v", rec.Payload)
	}
	return c
}

// TestGenericRetryFoldsInsideWindow pins §3.4a: three byte-identical generic
// POSTs inside the window produce exactly ONE event record, ONE group (count
// stays 1), and the two folded retries are visible as counters.suppressed = 2
// on group records whose op is update. The clock is injected: the window is
// walked with a fake clock, never a sleep.
func TestGenericRetryFoldsInsideWindow(t *testing.T) {
	cur := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	ts := newTestServer(t, func(c *Config) { c.CanaryProject = "" })
	defer ts.close()
	ts.s.now = func() time.Time { return cur }

	first := postGenericFold(t, ts, genericFoldBody)
	for i := 0; i < 2; i++ {
		cur = cur.Add(30 * time.Second)
		again := postGenericFold(t, ts, genericFoldBody)
		if again["id"] != first["id"] {
			t.Errorf("folded retry answered id %v, want the first occurrence's id %v", again["id"], first["id"])
		}
	}

	if evs := ts.sink.ofKind(types.KEvent); len(evs) != 1 {
		t.Fatalf("%d event records, want 1 (the fold absorbs the retries)", len(evs))
	}
	groups := ts.s.Groups()
	if len(groups) != 1 || groups[0].Count != 1 {
		t.Fatalf("groups = %+v, want one group with count 1", groups)
	}
	ops := groupRecordsByOp(t, ts)
	if ops["create"] != 1 {
		t.Errorf("%d create records, want 1", ops["create"])
	}
	if ops["update"] != 2 {
		t.Errorf("%d update records, want 2 (one per folded retry)", ops["update"])
	}
	if c := lastGroupCounters(t, ts); c.Suppressed != 2 {
		t.Errorf("counters.suppressed = %d, want 2", c.Suppressed)
	}
	if got := ts.s.counters.genericFolds.Get(); got != 2 {
		t.Errorf("generic_folds_total = %d, want 2", got)
	}
	if got := ts.s.counters.events.Get(); got != 1 {
		t.Errorf("events_total = %d, want 1 (a folded retry is not an admitted event)", got)
	}
}

// TestGenericDifferentSigsNeverFold pins the fold key: two different messages
// are two sigs, so both are admitted events in their own groups and no
// suppression counter moves.
func TestGenericDifferentSigsNeverFold(t *testing.T) {
	cur := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	ts := newTestServer(t, func(c *Config) { c.CanaryProject = "" })
	defer ts.close()
	ts.s.now = func() time.Time { return cur }

	postGenericFold(t, ts, `{"message":"first trouble event","level":"error","release":"0.1.0"}`)
	postGenericFold(t, ts, `{"message":"a different bug","level":"error","release":"0.1.0"}`)

	if evs := ts.sink.ofKind(types.KEvent); len(evs) != 2 {
		t.Fatalf("%d event records, want 2 (different sigs are distinct evidence)", len(evs))
	}
	if groups := ts.s.Groups(); len(groups) != 2 {
		t.Fatalf("%d groups, want 2", len(groups))
	}
	if ops := groupRecordsByOp(t, ts); ops["update"] != 0 {
		t.Errorf("%d update records, want 0 (nothing folded)", ops["update"])
	}
	if c := lastGroupCounters(t, ts); c.Suppressed != 0 {
		t.Errorf("counters.suppressed = %d, want 0", c.Suppressed)
	}
	if got := ts.s.counters.genericFolds.Get(); got != 0 {
		t.Errorf("generic_folds_total = %d, want 0", got)
	}
}

// TestGenericWindowExpiryCreatesFreshEvidence pins the window bound: the same
// body after the window has expired is a NEW occurrence — one more admitted
// event record and a group count that reflects both occurrences, with the
// earlier folds still carried as suppressed and no fresh suppression.
func TestGenericWindowExpiryCreatesFreshEvidence(t *testing.T) {
	cur := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	ts := newTestServer(t, func(c *Config) { c.CanaryProject = "" })
	defer ts.close()
	ts.s.now = func() time.Time { return cur }

	postGenericFold(t, ts, genericFoldBody)
	cur = cur.Add(30 * time.Second)
	postGenericFold(t, ts, genericFoldBody) // folded
	cur = cur.Add(10 * time.Minute)         // the window is gone
	postGenericFold(t, ts, genericFoldBody)

	if evs := ts.sink.ofKind(types.KEvent); len(evs) != 2 {
		t.Fatalf("%d event records, want 2 (the expired window admits the retry)", len(evs))
	}
	groups := ts.s.Groups()
	if len(groups) != 1 || groups[0].Count != 2 {
		t.Fatalf("groups = %+v, want one group with count 2", groups)
	}
	if c := lastGroupCounters(t, ts); c.Suppressed != 1 {
		t.Errorf("counters.suppressed = %d, want 1 (only the in-window retry folded)", c.Suppressed)
	}
	if got := ts.s.counters.genericFolds.Get(); got != 1 {
		t.Errorf("generic_folds_total = %d, want 1", got)
	}
}

// TestGenericFoldWindowZeroDisables pins the OFF posture: window 0 reproduces
// the pre-fix one-record-per-POST behaviour (§3.4a "0 = off").
func TestGenericFoldWindowZeroDisables(t *testing.T) {
	cur := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	ts := newTestServer(t, func(c *Config) {
		c.CanaryProject = ""
		c.GenericDedupWindow = "0"
	})
	defer ts.close()
	ts.s.now = func() time.Time { return cur }

	for i := 0; i < 3; i++ {
		postGenericFold(t, ts, genericFoldBody)
	}
	if evs := ts.sink.ofKind(types.KEvent); len(evs) != 3 {
		t.Fatalf("%d event records, want 3 (window 0 is the documented OFF)", len(evs))
	}
	if ops := groupRecordsByOp(t, ts); ops["update"] != 0 {
		t.Errorf("%d update records, want 0", ops["update"])
	}
}

// TestGenericExplicitEventIDRetryStillUsesIDDedup pins that §6.6 is untouched:
// a body carrying an explicit event_id retries through the existing id-dedup —
// no second event record, no suppression counter, no update record.
func TestGenericExplicitEventIDRetryStillUsesIDDedup(t *testing.T) {
	cur := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	ts := newTestServer(t, func(c *Config) { c.CanaryProject = "" })
	defer ts.close()
	ts.s.now = func() time.Time { return cur }

	body := `{"event_id":"9f2c1d3e4b5a6c7d8e9f0a1b2c3d4e5f","message":"first trouble event","level":"error","release":"0.1.0"}`
	postGenericFold(t, ts, body)
	postGenericFold(t, ts, body)

	if evs := ts.sink.ofKind(types.KEvent); len(evs) != 1 {
		t.Fatalf("%d event records, want 1 (the id-dedup answer)", len(evs))
	}
	if ops := groupRecordsByOp(t, ts); ops["update"] != 0 {
		t.Errorf("%d update records, want 0 (the id-dedup path never folds)", ops["update"])
	}
	if c := lastGroupCounters(t, ts); c.Suppressed != 0 {
		t.Errorf("counters.suppressed = %d, want 0", c.Suppressed)
	}
	if got := ts.s.counters.duplicateEvents.Get(); got != 1 {
		t.Errorf("duplicate_events_total = %d, want 1", got)
	}
}

// TestEnvelopeSigsNeverFold pins the fold's scope: the envelope and store
// on-ramps carry real event ids, so §6.6 is their dedup — two distinct-id
// envelope events of one sig are two admitted events, never folded.
func TestEnvelopeSigsNeverFold(t *testing.T) {
	cur := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	ts := newTestServer(t, func(c *Config) { c.CanaryProject = "" })
	defer ts.close()
	ts.s.now = func() time.Time { return cur }

	entry, _ := ts.s.projects.project("1")
	ctx := context.Background()
	first := groupEvent("9f2c1d3e4b5a6c7d8e9f0a1b2c3d4e5f", "payment-api@2.4.1")
	second := groupEvent("0a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d", "payment-api@2.4.1")
	for _, ev := range []*rawEvent{first, second} {
		ev.SourceKind = sourceEnvelope
		if _, err := ts.s.admitEvent(ctx, entry, ev, "event", "test"); err != nil {
			t.Fatalf("admit: %v", err)
		}
	}
	if evs := ts.sink.ofKind(types.KEvent); len(evs) != 2 {
		t.Fatalf("%d event records, want 2", len(evs))
	}
	if ops := groupRecordsByOp(t, ts); ops["update"] != 0 {
		t.Errorf("%d update records, want 0 (the fold is generic-on-ramp only)", ops["update"])
	}
}
