package sentinel

import (
	"context"
	"fmt"
	"testing"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// groupEvent builds an event that groups to one digest (the vector-B stack).
func groupEvent(id, release string) *rawEvent {
	return &rawEvent{
		ID:          id,
		TS:          "2026-09-16T09:14:03.221Z",
		Level:       "error",
		Culprit:     "worker.claim",
		Message:     "queue wedge: pool exhausted depth=912",
		Release:     release,
		SourceKind:  sourceGeneric,
		AuthForm:    fmtGenericQuery,
		Frames: []frame{
			{File: "worker.py", Function: "claim", InApp: true, ContextLine: "item = pool.get(timeout=1)"},
			{File: "queue.py", Function: "get", InApp: true, ContextLine: "raise PoolExhausted(depth=912)"},
		},
	}
}

// Test6400EventsOneGroup pins §7's group_test headline: 6,400 events collapse to
// exactly one group with count == 6400, and the counters stay exact after a
// flush.
func Test6400EventsOneGroup(t *testing.T) {
	ts := newTestServer(t, func(c *Config) {
		c.CanaryProject = ""
		c.Projects[0].QuotaEPM = 1_000_000
	})
	defer ts.close()
	entry, _ := ts.s.projects.project("1")
	ctx := context.Background()
	const n = 6400
	for i := 0; i < n; i++ {
		ev := groupEvent(fmt.Sprintf("%032x", i+1), "payment-api@2.4.1")
		if _, err := ts.s.admitEvent(ctx, entry, ev, "event", "test"); err != nil {
			t.Fatalf("admitEvent %d: %v", i, err)
		}
	}
	groups := ts.s.Groups()
	if len(groups) != 1 {
		t.Fatalf("%d groups, want exactly 1", len(groups))
	}
	if groups[0].Count != n {
		t.Fatalf("group count = %d, want %d", groups[0].Count, n)
	}
	if groups[0].Counters.Events != n {
		t.Fatalf("counters.events = %d, want %d", groups[0].Counters.Events, n)
	}
	if n > groupFlushEvents && len(ts.sink.ofKind(types.KGroup)) < 2 {
		t.Fatalf("only %d group records for %d events: the 100-event flush trigger never fired",
			len(ts.sink.ofKind(types.KGroup)), n)
	}
	// Flush on demand and read the absolute snapshot back.
	if ferr := ts.s.flushGroups(ctx, "test"); ferr != nil {
		t.Fatalf("flushGroups: %v", ferr)
	}
	recs := ts.sink.ofKind(types.KGroup)
	last := recs[len(recs)-1]
	if op, _ := last.Payload["op"].(string); op != "flush" {
		t.Fatalf("last group record op = %q, want flush", op)
	}
	if c, ok := last.Payload["count"].(uint64); !ok || c != n {
		t.Fatalf("flush count = %v, want %d", last.Payload["count"], n)
	}
}

// TestIndexRebuildEqualsLiveCounts pins §3.3's rebuild: fold the last `group`
// record per digest, then replay `event` records after its `events_upper_seq` —
// the rebuilt index must equal the live one exactly (delta 0).
func TestIndexRebuildEqualsLiveCounts(t *testing.T) {
	ts := newTestServer(t, func(c *Config) {
		c.CanaryProject = ""
		c.Projects[0].QuotaEPM = 1_000_000
	})
	defer ts.close()
	entry, _ := ts.s.projects.project("1")
	ctx := context.Background()
	for i := 0; i < 350; i++ {
		ev := groupEvent(fmt.Sprintf("%032x", i+1), "payment-api@2.4.1")
		if _, err := ts.s.admitEvent(ctx, entry, ev, "event", "test"); err != nil {
			t.Fatalf("admitEvent: %v", err)
		}
	}
	if ferr := ts.s.flushGroups(ctx, "test"); ferr != nil {
		t.Fatalf("flushGroups: %v", ferr)
	}
	// A mid-flight release change gives the rebuild a second group record to
	// fold (the release op).
	for i := 0; i < 30; i++ {
		ev := groupEvent(fmt.Sprintf("%032x", 1000+i), "payment-api@2.4.2")
		if _, err := ts.s.admitEvent(ctx, entry, ev, "event", "test"); err != nil {
			t.Fatalf("admitEvent: %v", err)
		}
	}
	live := map[string]types.Group{}
	for _, g := range ts.s.Groups() {
		live[g.Digest] = g
	}

	// Rebuild from the ledger alone.
	reb := newTestServer(t, func(c *Config) {
		c.CanaryProject = ""
		c.Projects[0].QuotaEPM = 1_000_000
	})
	defer reb.close()
	rd, ok := interface{}(ts.sink).(ledgerReader)
	if !ok {
		t.Fatal("the test sink must be a ledgerReader for the rebuild path")
	}
	if err := reb.s.rebuild(rd); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	rebuilt := map[string]types.Group{}
	for _, g := range reb.s.Groups() {
		rebuilt[g.Digest] = g
	}
	if len(rebuilt) != len(live) {
		t.Fatalf("rebuilt %d groups, live has %d", len(rebuilt), len(live))
	}
	for digest, want := range live {
		got, ok := rebuilt[digest]
		if !ok {
			t.Fatalf("digest %s is missing after rebuild", digest)
		}
		if got.Count != want.Count {
			t.Errorf("digest %s: rebuilt count %d, live %d (delta %d)", digest, got.Count, want.Count, got.Count-want.Count)
		}
		if got.Counters.Events != want.Counters.Events {
			t.Errorf("digest %s: rebuilt counters.events %d, live %d", digest, got.Counters.Events, want.Counters.Events)
		}
		if len(got.ReleaseRange) > 1 && len(want.ReleaseRange) > 1 && got.ReleaseRange[1] != want.ReleaseRange[1] {
			t.Errorf("digest %s: rebuilt last release %q, live %q", digest, got.ReleaseRange[1], want.ReleaseRange[1])
		}
	}
}

// TestAuthFormsUnion pins §2.4: the project's AuthForms is the deduped union
// observed over the ledger and rebuilt at boot.
func TestAuthFormsUnion(t *testing.T) {
	ts := newTestServer(t, func(c *Config) { c.CanaryProject = "" })
	defer ts.close()
	entry, _ := ts.s.projects.project("1")
	ctx := context.Background()
	for i, form := range []string{fmtXSentryAuth, fmtQueryKey, fmtEnvelopeDSN, fmtXSentryAuth} {
		ev := groupEvent(fmt.Sprintf("%032x", 5000+i), "")
		ev.AuthForm = form
		ev.SourceKind = sourceEnvelope
		if _, err := ts.s.admitEvent(ctx, entry, ev, "event", "test"); err != nil {
			t.Fatalf("admitEvent: %v", err)
		}
	}
	rt, ok := ts.s.ProjectRuntime("1")
	if !ok {
		t.Fatal("ProjectRuntime(1) not found")
	}
	if len(rt.AuthForms) != 3 {
		t.Fatalf("AuthForms = %v, want the deduped union of 3 forms", rt.AuthForms)
	}
	// Rebuild folds them from the event records.
	reb := newTestServer(t, func(c *Config) { c.CanaryProject = "" })
	defer reb.close()
	if err := reb.s.rebuild(interface{}(ts.sink).(ledgerReader)); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	rt2, _ := reb.s.ProjectRuntime("1")
	if len(rt2.AuthForms) != 3 {
		t.Fatalf("rebuilt AuthForms = %v, want 3", rt2.AuthForms)
	}
}

// TestPerItemTypeCounters pins §3.2's accounting for dropped item types.
func TestPerItemTypeCounters(t *testing.T) {
	ts := newTestServer(t, nil)
	defer ts.close()
	ev := eventJSON(t, nil)
	body := envelopeBytes(t, map[string]any{"event_id": "11112222333344445555666677778888"},
		envelopeFixtureItem{Type: "attachment", Body: []byte("binary"), Length: true},
		envelopeFixtureItem{Type: "transaction", Body: []byte(`{"transaction":"GET /"}`), Length: true},
		envelopeFixtureItem{Type: "check_in", Body: []byte(`{}`), Length: true},
		envelopeFixtureItem{Type: "event", Body: ev, Length: true},
	)
	resp := ts.post(t, "/api/1/envelope/", map[string]string{"X-Sentry-Auth": ts.authHeader("1")}, body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d (%s)", resp.StatusCode, readBody(t, resp))
	}
	_ = readBody(t, resp)
	if got := ts.s.counters.itemsDropped.Get(); got != 2 {
		t.Errorf("items_dropped_total = %d, want 2 (attachment + transaction)", got)
	}
	if got := ts.s.counters.unknownItems.Get(); got != 1 {
		t.Errorf("unknown_items_total = %d, want 1 (check_in)", got)
	}
	for _, typ := range []string{"attachment", "transaction", "check_in"} {
		if ts.s.counters.byItemType[typ] != 1 {
			t.Errorf("items_dropped_total{%s} = %d, want 1", typ, ts.s.counters.byItemType[typ])
		}
	}
	// Raw attachment bytes never reach the ledger.
	for _, rec := range ts.sink.recs {
		if fmt.Sprint(rec.Payload) == "binary" {
			t.Fatal("raw attachment bytes reached the ledger")
		}
	}
}

// TestDuplicateEventIDIsNotDoubleCounted pins §6.6: an SDK retry inside the
// 10-minute window returns the same id and does not double count.
func TestDuplicateEventIDIsNotDoubleCounted(t *testing.T) {
	ts := newTestServer(t, func(c *Config) { c.CanaryProject = "" })
	defer ts.close()
	entry, _ := ts.s.projects.project("1")
	ctx := context.Background()
	ev := groupEvent("9f2c1d3e4b5a6c7d8e9f0a1b2c3d4e5f", "payment-api@2.4.1")
	ev.SourceKind = sourceEnvelope
	if _, err := ts.s.admitEvent(ctx, entry, ev, "event", "test"); err != nil {
		t.Fatalf("first admit: %v", err)
	}
	again := groupEvent("9f2c1d3e4b5a6c7d8e9f0a1b2c3d4e5f", "payment-api@2.4.1")
	again.SourceKind = sourceEnvelope
	if _, err := ts.s.admitEvent(ctx, entry, again, "event", "test"); err != nil {
		t.Fatalf("duplicate admit: %v", err)
	}
	if got := ts.s.counters.duplicateEvents.Get(); got != 1 {
		t.Fatalf("duplicate_events_total = %d, want 1", got)
	}
	groups := ts.s.Groups()
	if len(groups) != 1 || groups[0].Count != 1 {
		t.Fatalf("a retry must not double count: %+v", groups)
	}
}

// TestGroupRecordOps pins the group record op vocabulary and that the create
// record carries norm_version.
func TestGroupRecordOps(t *testing.T) {
	ts := newTestServer(t, func(c *Config) { c.CanaryProject = "" })
	defer ts.close()
	entry, _ := ts.s.projects.project("1")
	ctx := context.Background()
	ev := groupEvent("aaaa1111bbbb2222cccc3333dddd4444", "payment-api@2.4.1")
	ev.SourceKind = sourceEnvelope
	if _, err := ts.s.admitEvent(ctx, entry, ev, "event", "test"); err != nil {
		t.Fatalf("admit: %v", err)
	}
	ev2 := groupEvent("aaaa1111bbbb2222cccc3333dddd5555", "payment-api@2.4.2")
	ev2.SourceKind = sourceEnvelope
	if _, err := ts.s.admitEvent(ctx, entry, ev2, "event", "test"); err != nil {
		t.Fatalf("admit: %v", err)
	}
	ops := []string{}
	for _, rec := range ts.sink.ofKind(types.KGroup) {
		op, _ := rec.Payload["op"].(string)
		ops = append(ops, op)
		if rec.Payload["norm_version"] == nil {
			t.Errorf("group record %s has no norm_version", op)
		}
	}
	if len(ops) < 2 || ops[0] != "create" || ops[1] != "release" {
		t.Fatalf("group record ops = %v, want create then release", ops)
	}
}
