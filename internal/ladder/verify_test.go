package ladder

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// verify_test.go is the SPEC-05 §7 row for internal/ladder/verify.go:
//
//   - the Evidence fixture from SPEC-TYPES §3.7 verbatim, round-tripped through
//     types.Evidence so the tuple cannot drift;
//   - canary absent → invalid + TROUBLE-LADDER-006, with a guard sweep asserting
//     that no code path yields `passed` with canary_seen=false (amendment D);
//   - one expected source missing → invalid, `sources_quiet` exactly the
//     quiet-but-alive sources;
//   - a gap overlapping the window → invalid, `gap_refs` non-empty, and zero
//     `gap`-kind records from this package (ownership assertion);
//   - recurrence at 9m59s of a 10m window → failed + TROUBLE-LADDER-007 → T39;
//   - a canary outside the window → invalid;
//   - window repair 0 → 10m, 30s → 1m, 48h → 10m, each with -005;
//   - quiet-close: suppressed, canary landed, zero events → resolved (T45);
//   - the research degraded path (AC-20).
//
// AC mapping: AC-21/AC-26 (a window is what gates promotion and the zero-human
// loop, so the evidence tuple has to be trustworthy), AC-20 (the degraded
// research rung).
//
// Two facts about this build shaped the harness code below; both are findings
// against files this test does not own and are reported in the run report:
//
//  1. types.NewID is not unique in this environment (10,020 calls produced 5
//     distinct ids) and admission dedups on Observation.EventID, so every
//     arrival a test drives carries an explicit event id (evSeq.next).
//  2. Liveness is computed from one per-sig event count (IndexReader.
//     EventsInWindow is not per-source), so `sources_quiet` and
//     `sources_missing` can never both be non-empty; the two subcases below pin
//     the quiet list in the all-quiet window and the missing list in the
//     canary-less window.

const testHostID = "hostp"

// evSeq mints explicit, distinct event ids. Admission dedups on the event id
// (types.NewID is not unique in this build), and a collision would silently turn
// a real arrival into a replay — one record, one counter increment, no fold.
type evSeq struct{ n int }

func (e *evSeq) next() string {
	e.n++
	return fmt.Sprintf("ev_verify_test_%06d", e.n)
}

// admitEv admits one observation with an explicit event id and subject. The
// subject fixes the cross-plane inKey (§3.9), which is what decides
// merge-vs-new-incident for two arrivals from different sources.
func admitEv(t *testing.T, h *harness, obs Observation, evID, subject string) AdmitResult {
	t.Helper()
	obs.EventID = evID
	if subject != "" {
		obs.Subject = subject
	}
	return h.admit(t, obs)
}

// evalWindow assembles the evidence tuple under the state lock, exactly as
// Verify and the quiet-close checkpoint do (verifyLocked is the `…Locked` form).
func evalWindow(t *testing.T, h *harness, incID string, opts canaryOpts) types.Evidence {
	t.Helper()
	h.l.mu.Lock()
	defer h.l.mu.Unlock()
	st, ok := h.l.incs[incID]
	if !ok {
		t.Fatalf("no incident state for %s", incID)
	}
	w := *st.Window
	ev, err := h.l.verifyLocked(context.Background(), st, &w, opts)
	if err != nil {
		t.Fatalf("verifyLocked: %v", err)
	}
	return ev
}

// lastVerify returns the newest `verify`-kind record and its evidence tuple.
func lastVerify(t *testing.T, h *harness) (ledgerRec, types.Evidence) {
	t.Helper()
	recs := h.ledger.byKind(types.KVerify)
	if len(recs) == 0 {
		t.Fatalf("no verify record was written")
	}
	rec := recs[len(recs)-1]
	ev, ok := rec.Payload["evidence"].(types.Evidence)
	if !ok {
		t.Fatalf("verify record payload evidence is %T, want types.Evidence", rec.Payload["evidence"])
	}
	return rec, ev
}

// lastIncidentRecord returns the newest `incident`-kind record.
func lastIncidentRecord(t *testing.T, h *harness) ledgerRec {
	t.Helper()
	recs := h.ledger.byKind(types.KIncident)
	if len(recs) == 0 {
		t.Fatalf("no incident record was written")
	}
	return recs[len(recs)-1]
}

// verdictHarness is the fresh harness the verify rows drive.
func verdictHarness(t *testing.T, rules map[string]types.Rule) *harness {
	t.Helper()
	return newHarness(t, harnessOpts{
		cfg:   Config{HostID: testHostID},
		rules: rules,
	})
}

func playRule(name string) map[string]types.Rule {
	return map[string]types.Rule{name: {Name: name, EntryRung: types.RungPlay, Severity: types.SevHigh}}
}

// ---------------------------------------------------------------------------
// SPEC-TYPES §3.7 — the Evidence fixture, verbatim.
// ---------------------------------------------------------------------------

// evidenceFixtureJSON is the `evidence` object of SPEC-TYPES §3.7's Incident
// example, copied character for character.
const evidenceFixtureJSON = `{"ts_window_start":"2026-09-16T09:15:41.009Z","ts_window_end":"2026-09-16T09:25:41.009Z","window_s":600,"events_observed":0,"canary_seen":true,"canary_id":"can_01J9Z6","counter_deltas":{"sentinel:payment-worker":3,"journald:payment-worker":0},"sources_expected":["7f3a91c2d4e5b607:sentinel:payment-worker","7f3a91c2d4e5b607:journald:payment-worker"],"sources_alive":["7f3a91c2d4e5b607:sentinel:payment-worker","7f3a91c2d4e5b607:journald:payment-worker"],"sources_quiet":["7f3a91c2d4e5b607:sentinel:payment-worker"],"sources_missing":[],"zone":"loopback","result":"passed"}`

// evidenceJSONKeys is the frozen tuple: the 13 fields of SPEC-TYPES §3.7.
var evidenceJSONKeys = []string{
	"canary_id", "canary_seen", "counter_deltas", "events_observed", "result",
	"sources_alive", "sources_expected", "sources_missing", "sources_quiet",
	"ts_window_end", "ts_window_start", "window_s", "zone",
}

// TestEvidenceFixtureRoundTrip pins the SPEC-TYPES §3.7 fixture through
// types.Evidence: field-for-field, then structurally both ways, so a rename, a
// dropped field or a JSON-tag drift fails here instead of in production.
func TestEvidenceFixtureRoundTrip(t *testing.T) {
	var ev types.Evidence
	if err := json.Unmarshal([]byte(evidenceFixtureJSON), &ev); err != nil {
		t.Fatalf("unmarshal the §3.7 fixture: %v", err)
	}

	// Field-for-field (§3.7 is the contract; the fixture is its witness).
	if ev.TSWindowStart != "2026-09-16T09:15:41.009Z" {
		t.Errorf("ts_window_start = %q", ev.TSWindowStart)
	}
	if ev.TSWindowEnd != "2026-09-16T09:25:41.009Z" {
		t.Errorf("ts_window_end = %q", ev.TSWindowEnd)
	}
	if ev.WindowS != 600 {
		t.Errorf("window_s = %v, want 600 (10m)", ev.WindowS)
	}
	if ev.EventsObserved != 0 {
		t.Errorf("events_observed = %d, want 0", ev.EventsObserved)
	}
	if !ev.CanarySeen {
		t.Error("canary_seen = false, want true")
	}
	if ev.CanaryID != "can_01J9Z6" {
		t.Errorf("canary_id = %q", ev.CanaryID)
	}
	wantDeltas := map[string]int64{"sentinel:payment-worker": 3, "journald:payment-worker": 0}
	if !reflect.DeepEqual(ev.CounterDeltas, wantDeltas) {
		t.Errorf("counter_deltas = %v, want %v (a zero delta is a reading, not an absence)", ev.CounterDeltas, wantDeltas)
	}
	wantSources := []string{"7f3a91c2d4e5b607:sentinel:payment-worker", "7f3a91c2d4e5b607:journald:payment-worker"}
	if !reflect.DeepEqual(ev.SourcesExpected, wantSources) {
		t.Errorf("sources_expected = %v, want %v", ev.SourcesExpected, wantSources)
	}
	if !reflect.DeepEqual(ev.SourcesAlive, wantSources) {
		t.Errorf("sources_alive = %v, want %v", ev.SourcesAlive, wantSources)
	}
	if !reflect.DeepEqual(ev.SourcesQuiet, []string{"7f3a91c2d4e5b607:sentinel:payment-worker"}) {
		t.Errorf("sources_quiet = %v", ev.SourcesQuiet)
	}
	if len(ev.SourcesMissing) != 0 {
		t.Errorf("sources_missing = %v, want empty", ev.SourcesMissing)
	}
	if ev.Zone != "loopback" {
		t.Errorf("zone = %q, want loopback", ev.Zone)
	}
	if ev.Result != types.VerifyPassed {
		t.Errorf("result = %q, want passed", string(ev.Result))
	}

	// The fixture is honest about its own verdict: zero events, a landed canary
	// and no missing source is exactly §3.8/§3.10's `passed` row.
	if !(ev.EventsObserved == 0 && ev.CanarySeen && len(ev.SourcesMissing) == 0) {
		t.Errorf("the fixture's tuple does not satisfy the `passed` row it claims")
	}

	// Round trip: marshal → unmarshal → identical tuple (counter_deltas and
	// result included), then structural equality with the raw fixture so key
	// order in the document cannot make the comparison lie.
	blob, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back types.Evidence
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if !reflect.DeepEqual(ev, back) {
		t.Errorf("round trip drifted:\n got %+v\nwant %+v", back, ev)
	}
	var gotMap, wantMap map[string]any
	if err := json.Unmarshal(blob, &gotMap); err != nil {
		t.Fatalf("unmarshal re-marshalled: %v", err)
	}
	if err := json.Unmarshal([]byte(evidenceFixtureJSON), &wantMap); err != nil {
		t.Fatalf("unmarshal fixture map: %v", err)
	}
	if !reflect.DeepEqual(gotMap, wantMap) {
		t.Errorf("re-marshalled tuple != fixture:\n got %s\nwant %s", blob, evidenceFixtureJSON)
	}

	// The fixture's key set is exactly the tuple's: a field added to
	// types.Evidence cannot silently escape the shared fixture (§7).
	keys := make([]string, 0, 13)
	for i := 0; i < reflect.TypeOf(types.Evidence{}).NumField(); i++ {
		tag := reflect.TypeOf(types.Evidence{}).Field(i).Tag.Get("json")
		keys = append(keys, tag)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, evidenceJSONKeys) {
		t.Errorf("types.Evidence JSON tags = %v, want %v", keys, evidenceJSONKeys)
	}
	fixtureKeys := make([]string, 0, len(wantMap))
	for k := range wantMap {
		fixtureKeys = append(fixtureKeys, k)
	}
	sort.Strings(fixtureKeys)
	if !reflect.DeepEqual(fixtureKeys, evidenceJSONKeys) {
		t.Errorf("the §3.7 fixture carries keys %v, want %v", fixtureKeys, evidenceJSONKeys)
	}
}

// ---------------------------------------------------------------------------
// amendment D / AC-26 — a canary that did not land can never produce `passed`.
// ---------------------------------------------------------------------------

// TestVerifyCanaryAbsentIsInvalidWithLadder006 drives the public Verify on a
// closed window whose canary never landed: the tuple is `invalid`, and the
// ladder cannot resolve the incident on it — T38 refuses with
// TROUBLE-LADDER-006 and the state stays `verifying`, so absence is never read
// as a fix (§3.10, §5).
func TestVerifyCanaryAbsentIsInvalidWithLadder006(t *testing.T) {
	h := verdictHarness(t, playRule("rule-a"))
	inc, st := verifying(t, h, "vr-no-canary")
	h.index.events[st.Inc.Sig] = 0 // quiet, but nothing observed it

	ev, err := h.l.Verify(context.Background(), inc, *st.Window)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if ev.Result != types.VerifyInvalid {
		t.Fatalf("result = %q, want invalid (a canary that did not land can never pass)", string(ev.Result))
	}
	if ev.CanarySeen {
		t.Error("canary_seen = true on a window with no canary")
	}
	if ev.EventsObserved != 0 {
		t.Errorf("events_observed = %d, want 0", ev.EventsObserved)
	}

	// The verify record carries the incomplete evidence and no gap reference:
	// the ladder's verdict code for this tuple is TROUBLE-LADDER-006 (§5), which
	// invalidCode (the tuple→code mapping in verify.go) states.
	rec, wrote := lastVerify(t, h)
	if rec.Kind != types.KVerify {
		t.Fatalf("newest record kind = %q", string(rec.Kind))
	}
	if wrote.Result != types.VerifyInvalid || wrote.CanarySeen {
		t.Errorf("verify record evidence = result %q canary_seen=%t", string(wrote.Result), wrote.CanarySeen)
	}
	// NOTE: appendVerify does not (yet) fold this code into payload.error_code —
	// a gap reported in the run report, not asserted here.
	if got := invalidCode(ev, 0, len(ev.SourcesMissing)); got != types.CodeLadder006 {
		t.Errorf("invalidCode(no-canary tuple) = %q, want %q", got, types.CodeLadder006)
	}

	// And the resolution path refuses: -006 is on the ledger for this window.
	if _, err := h.l.Advance(context.Background(), inc, Transition{Trigger: "T38", Evidence: &ev}); err == nil {
		t.Fatal("T38 resolved an incident on a canary-less window")
	} else if got := CodeOf(err); got != types.CodeLadder006 {
		t.Errorf("T38 refusal code = %q, want %q", got, types.CodeLadder006)
	}
	after, err := h.l.Incident(context.Background(), inc)
	if err != nil {
		t.Fatalf("Incident: %v", err)
	}
	if after.State != types.StVerifying {
		t.Errorf("state = %q, want verifying (a refusal never mutates state)", string(after.State))
	}
	if got, _ := h.incidentPayload()["error_code"].(string); got != string(types.CodeLadder006) {
		t.Errorf("T38 refusal record error_code = %q, want %q", got, types.CodeLadder006)
	}
}

// TestVerifyNoPathProducesPassedWithoutACanary is the §7 grep-style guard on the
// result constructor: across every combination the evidence assembler can see,
// `passed` implies `canary_seen` (AC-26).
func TestVerifyNoPathProducesPassedWithoutACanary(t *testing.T) {
	cases := []struct {
		name   string
		canary bool
		events int
		gap    bool
		want   types.VerifyResultKind
	}{
		{"canary+quiet", true, 0, false, types.VerifyPassed},
		{"canary+recurrence", true, 3, false, types.VerifyFailed},
		{"canary+gap", true, 0, true, types.VerifyInvalid},
		{"no-canary+quiet", false, 0, false, types.VerifyInvalid},
		{"no-canary+recurrence", false, 3, false, types.VerifyInvalid},
		{"no-canary+gap", false, 0, true, types.VerifyInvalid},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			h := verdictHarness(t, playRule("rule-a"))
			inc, st := verifying(t, h, "vr-guard-"+c.name)
			if c.canary {
				h.canaryIn(t, st.Inc.Sig)
			}
			h.index.events[st.Inc.Sig] = c.events
			if c.gap {
				h.index.gaps = []types.GapRecord{{
					ID: "gap_guard", Sensor: "sentinel", Scope: "payment-worker.service",
					FromTS:  types.FormatUTC(h.clock.Now()),
					ToTS:    types.FormatUTC(h.clock.Now().Add(time.Minute)),
					EstLost: 1, Cause: "bus_down",
				}}
			}
			ev := evalWindow(t, h, inc, canaryOpts{})
			if ev.Result != c.want {
				t.Errorf("result = %q, want %q (canary=%t events=%d gap=%t)",
					string(ev.Result), string(c.want), c.canary, c.events, c.gap)
			}
			// The invariant, stated directly.
			if ev.Result == types.VerifyPassed && !ev.CanarySeen {
				t.Errorf("INVARIANT VIOLATED: result=%q with canary_seen=false", string(ev.Result))
			}
			if ev.Result == types.VerifyPassed && !c.canary {
				t.Errorf("result=%q without a canary in the window", string(ev.Result))
			}
			// The tuple is never a boolean: the three values are the only ones.
			switch ev.Result {
			case types.VerifyPassed, types.VerifyFailed, types.VerifyInvalid:
			default:
				t.Errorf("result %q is not one of the three kinds", string(ev.Result))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// §3.10 — expected sources, liveness and gaps.
// ---------------------------------------------------------------------------

// TestVerifyExpectedSourceMissingIsInvalid covers "an expected source that
// cannot prove liveness makes the result invalid — the evidence is incomplete,
// not negative": the group's canary landed, the sig's own source did not.
func TestVerifyExpectedSourceMissingIsInvalid(t *testing.T) {
	h := verdictHarness(t, playRule("rule-a"))
	inc, st := verifying(t, h, "vr-src-missing")
	// Only the group canary lands: an incident's expected sources are the ones
	// this package must hear from, and the canary is the liveness proof for them
	// (amendment D: silence is not proof of anything).
	if err := h.l.CanaryObserved(context.Background(), st.Inc.GroupID, "can_grp_01", "sentinel"); err != nil {
		t.Fatalf("CanaryObserved: %v", err)
	}
	h.index.events[st.Inc.Sig] = 0

	ev, err := h.l.Verify(context.Background(), inc, *st.Window)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if ev.Result != types.VerifyInvalid {
		t.Fatalf("result = %q, want invalid (an expected source is missing)", string(ev.Result))
	}
	if !ev.CanarySeen {
		t.Error("canary_seen = false: the group canary landed")
	}
	if want := []string{testHostID + ":journald"}; !reflect.DeepEqual(ev.SourcesMissing, want) {
		t.Errorf("sources_missing = %v, want exactly %v", ev.SourcesMissing, want)
	}
	if len(ev.SourcesQuiet) != 0 {
		t.Errorf("sources_quiet = %v, want empty (no expected source reported liveness)", ev.SourcesQuiet)
	}
	// `sources_quiet` is always a subset of `sources_alive`: quiet means
	// observed-and-silent, never missing.
	if !reflect.DeepEqual(ev.SourcesExpected, []string{testHostID + ":journald"}) {
		t.Errorf("sources_expected = %v", ev.SourcesExpected)
	}
	for _, q := range ev.SourcesQuiet {
		if !containsStr(ev.SourcesAlive, q) {
			t.Errorf("source %q is listed quiet but not alive", q)
		}
	}
	if !containsStr(ev.SourcesAlive, testHostID+":sentinel") {
		t.Errorf("sources_alive = %v, want the canary's own path to prove liveness", ev.SourcesAlive)
	}

	// A dropout the ladder can see with no gap record to point at raises the
	// flag rather than passing (§3.10: "a missing gap is a defect of the source
	// subsystem, not a licence to pass").
	rec, _ := lastVerify(t, h)
	if got := rec.Payload["gap_missing"]; got != true {
		t.Errorf("verify record gap_missing = %v, want true", got)
	}
	if refs, _ := rec.Payload["gap_refs"].([]string); len(refs) != 0 {
		t.Errorf("gap_refs = %v, want empty (no gap record exists)", refs)
	}
}

// TestVerifyQuietListsExactlyTheQuietButAliveSources covers the other half of the
// row: every expected source is quiet and alive (the canary landed), so the quiet
// list is exactly the expected set and nothing is missing.
func TestVerifyQuietListsExactlyTheQuietButAliveSources(t *testing.T) {
	h := verdictHarness(t, playRule("rule-a"))
	sig := sigFor("vr-quiet")
	inc, st := verifying(t, h, "vr-quiet")
	var arrivals evSeq

	// A second arrival path for the same sig (§3.9): both are expected sources.
	if res := admitEv(t, h, obsFor(sig, "rule-a", "collector"), arrivals.next(), "collector-svc"); res.Inc == "" {
		t.Fatalf("the second arrival did not reach the incident: %+v", res)
	}
	if got := len(st.ArrivalPaths); got != 2 {
		t.Fatalf("arrival paths = %v, want two sources", st.ArrivalPaths)
	}

	h.canaryIn(t, sig.String())
	h.index.events[sig.String()] = 0
	h.index.counters = map[string]int64{"sentinel:payment-worker": 7}

	ev, err := h.l.Verify(context.Background(), inc, *st.Window)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	wantQuiet := []string{testHostID + ":collector", testHostID + ":journald"}
	if !reflect.DeepEqual(ev.SourcesQuiet, wantQuiet) {
		t.Errorf("sources_quiet = %v, want exactly %v", ev.SourcesQuiet, wantQuiet)
	}
	if ev.Result != types.VerifyPassed {
		t.Errorf("result = %q, want passed", string(ev.Result))
	}
	if len(ev.SourcesMissing) != 0 {
		t.Errorf("sources_missing = %v, want empty", ev.SourcesMissing)
	}
	for _, want := range wantQuiet {
		if !containsStr(ev.SourcesAlive, want) {
			t.Errorf("sources_alive = %v, want %q (quiet is alive)", ev.SourcesAlive, want)
		}
	}
	// Counter deltas are read from the index, never inferred (§3.10).
	if got := ev.CounterDeltas["sentinel:payment-worker"]; got != 7 {
		t.Errorf("counter_deltas = %v, want the index reading 7", ev.CounterDeltas)
	}
}

// TestVerifyGapOverlappingWindowIsInvalidAndOwnsNoGapRecords covers the
// ownership rule: this package consumes gap records, references them in
// gap_refs, invalidates the window — and never writes a `gap`-kind record.
func TestVerifyGapOverlappingWindowIsInvalidAndOwnsNoGapRecords(t *testing.T) {
	h := verdictHarness(t, playRule("rule-a"))
	inc, st := verifying(t, h, "vr-gap")
	h.canaryIn(t, st.Inc.Sig)
	h.index.events[st.Inc.Sig] = 0
	h.index.gaps = []types.GapRecord{{
		ID: "gap_01J9Z6Q0M2", Sensor: "sentinel", Scope: "payment-worker.service",
		FromTS:  types.FormatUTC(h.clock.Now()),
		ToTS:    types.FormatUTC(h.clock.Now().Add(2 * time.Minute)),
		EstLost: 12, Cause: "bus_down",
	}}

	ev, err := h.l.Verify(context.Background(), inc, *st.Window)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if ev.Result != types.VerifyInvalid {
		t.Fatalf("result = %q, want invalid (a gap makes the evidence incomplete)", string(ev.Result))
	}
	rec, _ := lastVerify(t, h)
	refs, ok := rec.Payload["gap_refs"].([]string)
	if !ok {
		t.Fatalf("gap_refs is %T, want []string", rec.Payload["gap_refs"])
	}
	if len(refs) == 0 || !containsStr(refs, "gap_01J9Z6Q0M2") {
		t.Errorf("gap_refs = %v, want the overlapping gap id", refs)
	}
	if got := rec.Payload["gap_missing"]; got != nil {
		t.Errorf("gap_missing = %v, want unset when a gap record exists", got)
	}

	// Ownership: across the whole ledger this package emitted zero gap records.
	for _, r := range h.ledger.byKind(types.KGap) {
		t.Errorf("this package emitted a %q record: %+v (SPEC-INDEX §3.4: gap records belong to the source owners)", string(r.Kind), r)
	}
	if n := len(h.ledger.byKind(types.KGap)); n != 0 {
		t.Fatalf("%d gap-kind record(s) in the ledger, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// §3.10 — recurrence, window boundary, window repair.
// ---------------------------------------------------------------------------

// TestVerifyRecurrenceAt9m59sFailsAndT39Reruns covers AC-21's recurrence leg: an
// event 9m59s into a 10m window is inside it, so the window fails with
// TROUBLE-LADDER-007 and the ladder re-runs the play (T39) while
// play_runs < effective_max_runs + verify_reopens.
func TestVerifyRecurrenceAt9m59sFailsAndT39Reruns(t *testing.T) {
	h := verdictHarness(t, playRule("rule-a"))
	inc, st := verifying(t, h, "vr-recur")
	if st.Window.S != "10m" {
		t.Fatalf("window = %s, want the 10m default", string(st.Window.S))
	}

	h.clock.advance(5 * time.Minute)
	h.canaryIn(t, st.Inc.Sig) // the canary lands inside the window
	h.clock.advance(4*time.Minute + 59*time.Second)
	h.index.events[st.Inc.Sig] = 1 // the recurrence at 9m59s

	ev, err := h.l.Verify(context.Background(), inc, *st.Window)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if ev.Result != types.VerifyFailed {
		t.Fatalf("result = %q, want failed (a recurrence inside the window)", string(ev.Result))
	}
	if !ev.CanarySeen || ev.EventsObserved != 1 {
		t.Errorf("canary_seen=%t events_observed=%d, want true/1", ev.CanarySeen, ev.EventsObserved)
	}
	if ev.WindowS != 600 {
		t.Errorf("window_s = %v, want 600", ev.WindowS)
	}
	if ev.TSWindowStart != types.FormatUTC(h.clock.Now().Add(-(9*time.Minute + 59*time.Second))) {
		t.Errorf("ts_window_start = %q", ev.TSWindowStart)
	}
	if got := invalidCode(ev, 0, 0); got != types.CodeLadder007 {
		t.Errorf("invalidCode(failed tuple) = %q, want %q", got, types.CodeLadder007)
	}

	// T39: the play re-runs and the row records TROUBLE-LADDER-007.
	got, err := h.l.Advance(context.Background(), inc, Transition{Trigger: "T39", Evidence: &ev})
	if err != nil {
		t.Fatalf("T39: %v", err)
	}
	if got.State != types.StPlayDrafted {
		t.Fatalf("state after T39 = %q, want play:drafted", string(got.State))
	}
	p := h.incidentPayload()
	if code, _ := p["error_code"].(string); code != string(types.CodeLadder007) {
		t.Errorf("T39 record error_code = %q, want %q", code, types.CodeLadder007)
	}
	if p["verify_fail"] != true {
		t.Errorf("T39 record verify_fail = %v, want true", p["verify_fail"])
	}
	if st.PlayRuns >= st.EffectiveMaxRuns+h.l.cfg.VerifyReopens {
		t.Fatalf("play_runs=%d is at the cap: T39 should not be reachable", st.PlayRuns)
	}
	// The re-run is a real path: the play rung accepts the drafted play again.
	if rerun, err := h.l.Advance(context.Background(), inc, Transition{Trigger: "T08"}); err != nil {
		t.Fatalf("T08 after the verify re-run: %v", err)
	} else if rerun.State != types.StPlayCheck {
		t.Errorf("state after the re-run = %q, want play:check_only", string(rerun.State))
	}
}

// TestVerifyCanaryOutsideWindowIsInvalid is the boundary row: the canary lands
// inside the window → the window can pass; the same canary one second after the
// window closes → invalid, because the window's quiet was never observed.
func TestVerifyCanaryOutsideWindowIsInvalid(t *testing.T) {
	t.Run("inside", func(t *testing.T) {
		h := verdictHarness(t, playRule("rule-a"))
		inc, st := verifying(t, h, "vr-in")
		h.clock.advance(9*time.Minute + 59*time.Second)
		h.canaryIn(t, st.Inc.Sig)
		h.index.events[st.Inc.Sig] = 0
		ev, err := h.l.Verify(context.Background(), inc, *st.Window)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if ev.Result != types.VerifyPassed || !ev.CanarySeen {
			t.Errorf("canary at 9m59s: result=%q canary_seen=%t, want passed/true", string(ev.Result), ev.CanarySeen)
		}
	})
	t.Run("outside", func(t *testing.T) {
		h := verdictHarness(t, playRule("rule-a"))
		inc, st := verifying(t, h, "vr-out")
		h.clock.advance(10*time.Minute + time.Second) // 10m01s: past the window
		h.canaryIn(t, st.Inc.Sig)
		h.index.events[st.Inc.Sig] = 0
		ev, err := h.l.Verify(context.Background(), inc, *st.Window)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if ev.Result != types.VerifyInvalid {
			t.Fatalf("result = %q, want invalid (the canary landed after the window closed)", string(ev.Result))
		}
		if ev.CanarySeen {
			t.Error("canary_seen = true for a canary outside the window")
		}
		if got := invalidCode(ev, 0, len(ev.SourcesMissing)); got != types.CodeLadder006 {
			t.Errorf("invalidCode = %q, want %q", got, types.CodeLadder006)
		}
	})
}

// TestClampWindowRepairs covers §3.10's window validity: 0 → the default, below
// the 1m floor → 1m, above the 24h ceiling → the default, every repair carrying
// TROUBLE-LADDER-005 with the original value.
func TestClampWindowRepairs(t *testing.T) {
	cases := []struct {
		in       types.Duration
		want     types.Duration
		wantCode types.ErrorCode
	}{
		{"0", "10m", types.CodeLadder005},
		{"30s", "1m", types.CodeLadder005},
		{"48h", "10m", types.CodeLadder005},
		{"10m", "10m", ""},
		{"1m", "1m", ""},
		{"24h", "24h", ""},
	}
	for _, c := range cases {
		got, cerr := clampWindow(c.in, "10m")
		if got != c.want {
			t.Errorf("clampWindow(%q) = %q, want %q", string(c.in), string(got), string(c.want))
		}
		if c.wantCode == "" {
			if cerr != nil {
				t.Errorf("clampWindow(%q) repaired a valid window: %v", string(c.in), cerr)
			}
			continue
		}
		if cerr == nil {
			t.Errorf("clampWindow(%q) recorded no code, want %q", string(c.in), c.wantCode)
			continue
		}
		if cerr.Code != c.wantCode {
			t.Errorf("clampWindow(%q) code = %q, want %q", string(c.in), cerr.Code, c.wantCode)
		}
		if cerr.Reason != reasonEvaluationBlocked {
			t.Errorf("clampWindow(%q) reason = %q, want %q", string(c.in), cerr.Reason, reasonEvaluationBlocked)
		}
	}

	// The repair reaches the incident: a rule's invalid window is replaced by the
	// default before the window exists, so no verification ever runs on 0s.
	h := verdictHarness(t, map[string]types.Rule{
		"rule-a": {Name: "rule-a", EntryRung: types.RungPlay, VerifyWin: "30s"},
	})
	res := h.admit(t, obsFor(sigFor("vr-clamp"), "rule-a", "journald"))
	inc, err := h.l.Incident(context.Background(), res.Inc)
	if err != nil {
		t.Fatalf("Incident: %v", err)
	}
	if inc.VerifyWin != "1m" {
		t.Errorf("incident verify_window = %q, want the 1m floor", string(inc.VerifyWin))
	}
}

// ---------------------------------------------------------------------------
// §3.8 — quiet-close (T45).
// ---------------------------------------------------------------------------

// TestQuietCloseResolvesWithPassedTuple covers T45: a suppressed incident whose
// window was silent and whose canary landed resolves on a `passed` tuple.
// Quiet is only accepted when it was observed.
func TestQuietCloseResolvesWithPassedTuple(t *testing.T) {
	h := verdictHarness(t, map[string]types.Rule{
		"rule-a": {Name: "rule-a", EntryRung: types.RungPlay, Cooldown: "30m"},
	})
	inc, st := suppressedIncident(t, h, "vr-quiet-close")
	h.canaryIn(t, st.Inc.Sig) // the canary landed inside the window
	h.index.events[st.Inc.Sig] = 0
	h.clock.advance(31 * time.Minute)

	h.l.mu.Lock()
	h.l.quietCloseLocked(context.Background(), h.l.incs[inc])
	h.l.mu.Unlock()

	got, err := h.l.Incident(context.Background(), inc)
	if err != nil {
		t.Fatalf("Incident: %v", err)
	}
	if got.State != types.StResolved {
		t.Fatalf("state = %q, want resolved", string(got.State))
	}
	if got.ResolvedTS == "" || got.Evidence == nil {
		t.Errorf("resolved_ts=%q evidence=%v, want both set", got.ResolvedTS, got.Evidence)
	}

	rec, ev := lastVerify(t, h)
	if ev.Result != types.VerifyPassed {
		t.Errorf("quiet-close evidence result = %q, want passed", string(ev.Result))
	}
	if !ev.CanarySeen || ev.EventsObserved != 0 {
		t.Errorf("canary_seen=%t events_observed=%d, want true/0", ev.CanarySeen, ev.EventsObserved)
	}
	if got, ok := rec.Payload["canary"].(map[string]any); !ok || got["id"] != "can_test" {
		t.Errorf("verify record canary = %v, want the landed canary", rec.Payload["canary"])
	}

	var t45 map[string]any
	for _, r := range h.ledger.byKind(types.KIncident) {
		if tr, _ := r.Payload["transition"].(string); tr == "T45" {
			t45 = r.Payload
		}
	}
	if t45 == nil {
		t.Fatal("no T45 record was written")
	}
	if t45["quiet_close"] != true || t45["suppressed_count"] != 0 {
		t.Errorf("T45 record quiet_close=%v suppressed_count=%v", t45["quiet_close"], t45["suppressed_count"])
	}
	if t45["to"] != string(types.StResolved) {
		t.Errorf("T45 record to = %v", t45["to"])
	}
	// The window is spent: no suppression window is left open for the sig.
	h.l.mu.Lock()
	_, _, open := h.l.suppressionWindowLocked(st.Inc.Sig)
	h.l.mu.Unlock()
	if open {
		t.Error("the suppression window is still open after a quiet-close")
	}

	// Control: a window that collected arrivals is NOT quiet, and returns to
	// `recorded` on T44 without any passing tuple.
	h2 := verdictHarness(t, map[string]types.Rule{
		"rule-a": {Name: "rule-a", EntryRung: types.RungPlay, Cooldown: "30m"},
	})
	inc2, st2 := suppressedIncident(t, h2, "vr-not-quiet")
	h2.canaryIn(t, st2.Inc.Sig)
	h2.index.events[st2.Inc.Sig] = 0
	st2.SuppressedCount = 2 // arrivals were folded into the window
	h2.clock.advance(31 * time.Minute)

	h2.l.mu.Lock()
	h2.l.quietCloseLocked(context.Background(), h2.l.incs[inc2])
	h2.l.mu.Unlock()

	got2, err := h2.l.Incident(context.Background(), inc2)
	if err != nil {
		t.Fatalf("Incident: %v", err)
	}
	if got2.State != types.StRecorded {
		t.Errorf("state = %q, want recorded (a window with arrivals is not quiet)", string(got2.State))
	}
	if n := len(h2.ledger.byKind(types.KVerify)); n != 0 {
		t.Errorf("%d verify record(s) written for a non-quiet window, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// AC-20 — the research degraded path still runs the agent rung.
// ---------------------------------------------------------------------------

// TestResearchDegradedRunsTheAgentRung covers AC-20's degrade leg: a research
// driver that cannot reach the lab degrades the rung (T20, TROUBLE-LADDER-009),
// the ladder proceeds unchanged to the agent rung (T24), and the ledger holds the
// degraded reason and the request id so the gap is visible rather than silent.
func TestResearchDegradedRunsTheAgentRung(t *testing.T) {
	h := newHarness(t, harnessOpts{
		cfg:      Config{HostID: testHostID},
		research: true,
		rules:    map[string]types.Rule{"rule-agent": {Name: "rule-agent", EntryRung: types.RungAgent, Severity: types.SevHigh}},
	})
	// The fake driver's answer: the lab was unreachable.
	h.research.outcome = types.ResearchOutcome{
		ID: "res_01J9Z6Q0M2X4T8V1K7B3N5R8WZ", State: types.ResDegraded,
		Driver: "off-by-one", DegradedReason: "lab_unreachable: 503",
	}
	const degraded = "lab_unreachable: 503"

	inc, st := researchRequested(t, h, "vr-degraded")
	if st.ResearchID == "" {
		t.Fatal("the research request id is empty: the request never reached the driver")
	}
	if st.Outcome == nil || st.Outcome.DegradedReason != degraded {
		t.Fatalf("outcome = %+v, want the driver's degraded reason", st.Outcome)
	}

	got, err := h.l.Advance(context.Background(), inc, Transition{Trigger: "T20"})
	if err != nil {
		t.Fatalf("T20: %v", err)
	}
	if got.State != types.StResDegraded {
		t.Fatalf("state = %q, want research:degraded", string(got.State))
	}
	p := h.incidentPayload()
	if code, _ := p["error_code"].(string); code != string(types.CodeLadder009) {
		t.Errorf("T20 record error_code = %q, want %q", code, types.CodeLadder009)
	}
	if p["degraded_reason"] != degraded || p["research_id"] != st.ResearchID {
		t.Errorf("T20 record degraded_reason=%v research_id=%v", p["degraded_reason"], p["research_id"])
	}

	// The agent rung still runs: no brief, no play, but the incident proceeds.
	got, err = h.l.Advance(context.Background(), inc, Transition{Trigger: "T24"})
	if err != nil {
		t.Fatalf("T24: %v", err)
	}
	if got.State != types.StAgentRunning {
		t.Fatalf("state = %q, want agent:running (the degraded rung must not block the agent)", string(got.State))
	}
	p = h.incidentPayload()
	if p["degraded"] != true {
		t.Errorf("T24 record degraded = %v, want true", p["degraded"])
	}
	if lease, ok := p["lease"].(map[string]any); !ok || lease["lease_id"] == nil {
		t.Errorf("T24 record lease = %v, want the granted lease", p["lease"])
	}

	// The degraded reason stays in the ledger, attached to the same incident.
	found := 0
	for _, r := range h.ledger.byKind(types.KIncident) {
		if r.Inc != inc {
			continue
		}
		if r.Payload["degraded_reason"] == degraded {
			found++
		}
	}
	if found == 0 {
		t.Error("no incident record in the ledger carries the degraded reason")
	}
	// And the agent rung completes: run → done → verifying (the fix path).
	st.AgentResult = "done"
	if got, err := h.l.Advance(context.Background(), inc, Transition{Trigger: "T27"}); err != nil {
		t.Fatalf("T27: %v", err)
	} else if got.State != types.StAgentDone {
		t.Errorf("state = %q, want agent:done", string(got.State))
	}
	st.AgentMutations = 1
	if got, err := h.l.Advance(context.Background(), inc, Transition{Trigger: "T36"}); err != nil {
		t.Fatalf("T36: %v", err)
	} else if got.State != types.StVerifying {
		t.Errorf("state = %q, want verifying", string(got.State))
	}
}

func containsStr(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
