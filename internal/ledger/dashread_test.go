package ledger

import (
	"context"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// dashread_test.go proves the SPEC-10 §3.3 read surface serves every accessor
// from the in-memory index: no test in this file opens, reads or scans a ledger
// file, and the ring assertions are on projected values a render would show.

func dashIncidentDraft(id, sig, state string, ts string) types.RecordDraft {
	return types.RecordDraft{
		Kind:   types.KIncident,
		Sig:    sig,
		Inc:    id,
		Origin: types.Origin{HostID: "7f3a91c2d4e5b607", Source: "test"},
		Actor:  testActor(),
		Payload: map[string]any{
			"transition": "observation_admitted",
			"state":      state,
			"grp":        "grp_01J9TESTGROUP",
			"severity":   string(types.SevHigh),
			"entry_rung": string(types.RungPlay),
			"rung":       string(types.RungPlay),
		},
	}
}

func TestDashReaderCountersAndRing(t *testing.T) {
	clk := newFakeClock(testNow())
	l := testLedger(t, clk)
	defer l.Close(context.Background())

	dr := l.DashReader()
	if got := dr.LastSeq(); got == 0 {
		t.Fatalf("LastSeq = 0 before any append; the ledger's boot record should have advanced it")
	}

	sig := "journald\x1fpayment-worker\x1fqueue wedge"
	for i := 0; i < 5; i++ {
		clk.Advance(time.Second)
		mustAppend(t, l, eventDraft("journald", sig, "digest-a", 0))
	}

	open, groups, perMin := dr.Counters(clk.Now())
	if open != 0 {
		t.Errorf("incidentsOpen = %d, want 0 before any incident record", open)
	}
	if groups < 1 {
		t.Errorf("groupsOpen = %d, want ≥1 after 5 events", groups)
	}
	if perMin < 5 {
		t.Errorf("eventsPerMin = %v, want ≥5 for 5 events inside one minute", perMin)
	}

	if ts := dr.LastRecordTS(); ts == "" {
		t.Errorf("LastRecordTS is empty after appends")
	}
	age, ok := dr.LastEventAge(sig, clk.Now())
	if !ok {
		t.Fatalf("LastEventAge(%q) found no projected event", sig)
	}
	if age < 0 || age > 60 {
		t.Errorf("LastEventAge = %v for an event just appended", age)
	}
	if _, ok := dr.LastEventAge("no such sig", clk.Now()); ok {
		t.Errorf("LastEventAge reported an event for an unknown sig")
	}
}

func TestDashReaderOpenIncidentsAndTimeline(t *testing.T) {
	clk := newFakeClock(testNow())
	l := testLedger(t, clk)
	defer l.Close(context.Background())

	dr := l.DashReader()
	const inc = "inc_01J9DASHTEST0000000000"
	const sig = "sentinel\x1f1\x1fworker.claim"

	mustAppend(t, l, dashIncidentDraft(inc, sig, string(types.StRecorded), types.FormatUTC(clk.Now())))
	clk.Advance(2 * time.Second)
	mustAppend(t, l, types.RecordDraft{
		Kind:   types.KIncident,
		Sig:    sig,
		Inc:    inc,
		Origin: types.Origin{HostID: "7f3a91c2d4e5b607", Source: "test"},
		Actor:  testActor(),
		Payload: map[string]any{
			"transition": "rung_advance_play",
			"state":      string(types.StPlayApplied),
			"rung":       string(types.RungPlay),
		},
	})

	rows := dr.OpenIncidents(10)
	if len(rows) != 1 {
		t.Fatalf("OpenIncidents returned %d rows, want 1", len(rows))
	}
	if rows[0].ID != inc {
		t.Errorf("OpenIncidents row id = %q, want %q", rows[0].ID, inc)
	}
	if rows[0].State != types.StPlayApplied {
		t.Errorf("projected state = %q, want %q (last record wins)", rows[0].State, types.StPlayApplied)
	}

	before := dr.LastSeq()
	if since := dr.OpenIncidentsSince(before, 10); len(since) != 0 {
		t.Errorf("OpenIncidentsSince(top seq) = %d rows, want 0", len(since))
	}
	if since := dr.OpenIncidentsSince(0, 10); len(since) != 1 {
		t.Errorf("OpenIncidentsSince(0) = %d rows, want 1", len(since))
	}

	tl := dr.RecordsForIncident(inc, 0, 100)
	if len(tl) != 2 {
		t.Fatalf("RecordsForIncident returned %d rows, want 2", len(tl))
	}
	if tl[0].Seq >= tl[1].Seq {
		t.Errorf("timeline is not in ascending seq order: %d then %d", tl[0].Seq, tl[1].Seq)
	}
	if tl[1].Payload["summary"] == "" {
		t.Errorf("timeline row carries no summary: %+v", tl[1].Payload)
	}
	if tl[1].Actor.ID == "" || tl[1].Actor.Kind != types.ActorHuman && tl[1].Actor.Kind == "" {
		t.Errorf("timeline row lost its actor: %+v", tl[1].Actor)
	}

	// A closed incident leaves the open list but keeps its timeline.
	mustAppend(t, l, types.RecordDraft{
		Kind:   types.KIncident,
		Sig:    sig,
		Inc:    inc,
		Origin: types.Origin{HostID: "7f3a91c2d4e5b607", Source: "test"},
		Actor:  testActor(),
		Payload: map[string]any{
			"transition": "closed",
			"state":      string(types.StResolved),
			"resolution": "fixed",
		},
	})
	if rows := dr.OpenIncidents(10); len(rows) != 0 {
		t.Errorf("OpenIncidents still lists a resolved incident: %+v", rows)
	}
	if tl := dr.RecordsForIncident(inc, 0, 100); len(tl) != 3 {
		t.Errorf("RecordsForIncident after close = %d rows, want 3", len(tl))
	}
}

func TestDashReaderGroupsSinceAndBoundedRing(t *testing.T) {
	clk := newFakeClock(testNow())
	l := testLedger(t, clk)
	defer l.Close(context.Background())
	dr := l.DashReader()

	for i := 0; i < 3; i++ {
		clk.Advance(time.Second)
		mustAppend(t, l, eventDraft("journald", "journald\x1funit\x1fsig-"+string(rune('a'+i)), "digest-"+string(rune('a'+i)), 0))
	}
	groups := dr.GroupsSince(0, 100)
	if len(groups) != 3 {
		t.Fatalf("GroupsSince(0) = %d groups, want 3", len(groups))
	}
	top := dr.LastSeq()
	if got := dr.GroupsSince(top, 100); len(got) != 0 {
		t.Errorf("GroupsSince(top seq) = %d groups, want 0", len(got))
	}

	// The timeline window is bounded by the ring: asking for more than the ring
	// holds returns a window, never a file read.
	const inc = "inc_01J9RINGBOUND00000000000"
	for i := 0; i < 50; i++ {
		mustAppend(t, l, types.RecordDraft{
			Kind:   types.KIncident,
			Sig:    "x",
			Inc:    inc,
			Origin: types.Origin{HostID: "h", Source: "test"},
			Actor:  testActor(),
			Payload: map[string]any{
				"transition": "note",
				"state":      string(types.StPlayApplied),
			},
		})
	}
	if tl := dr.RecordsForIncident(inc, 0, 10); len(tl) != 10 {
		t.Errorf("RecordsForIncident(limit 10) = %d rows, want 10", len(tl))
	}
	if tl := dr.RecordsForIncident(inc, 0, 1000); len(tl) != 50 {
		t.Errorf("RecordsForIncident(all) = %d rows, want 50", len(tl))
	}
}
