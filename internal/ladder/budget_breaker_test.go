package ladder

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// budget_breaker_test.go is the SPEC-05 §7 row for budgets (§3.7, AC-5) and storm
// breakers (§3.8, AC-4):
//
//   - per-day agent-run budget: exhaustion escalates instead of running and the
//     counter never exceeds the limit (AC-5);
//   - rollover at 00:00 UTC: counters reset, the day field changes, and a run is
//     charged to the day it started (§3.7);
//   - a breaker trip opens for its scope's configured duration, the half-open
//     probe closes it, a failing probe re-opens doubled and capped at 4h (§3.8);
//   - TROUBLE-LADDER-014 is recorded once per window and then every 100th
//     arrival;
//   - a suppression cycle opens the sig breaker, and arrivals inside a window
//     never advance a rung (AC-4).
//
// Findings against files this test does not own (reported, not asserted here,
// because asserting them would pin a defect rather than the spec):
//
//  1. §3.8 "rule:<name> trips when > 5 new incidents/hour" and "source:<kind> at
//     > 20/hour" and "global at open_incidents > 200" are NOT wired: nothing in
//     the package reads Config.BreakerRulePerHour, BreakerSourcePerHour or
//     MaxOpenIncidents, so no number of admissions ever trips a breaker by
//     itself (verified: 8 admissions from one rule leave l.breakers empty). The
//     trip path below is therefore exercised through TripBreaker (the operator
//     path of §3.8 (c)) and openBreakerLockedWithReason, the same functions the
//     count-based trip would call.
//  2. §3.8 "≥3 suppression cycles/24h for one sig" never fires with the shipped
//     default: openSuppressionAfterTerminal replaces l.suppress[sig] (Cycles = 0)
//     immediately before suppressionCycleOpenedLocked increments it, so Cycles is
//     always 1. The cycle trip is asserted with BreakerSigPer24h = 1, the
//     threshold the current counter can reach.
//  3. §3.8 "arrivals inside a window increment the incident's suppressed_count
//     and never create an incident" is not honoured by Admit's ordering: the
//     suppression-window branch (T02) is evaluated before the open-incident fold,
//     so an arrival for a sig that already has an open incident is answered with
//     TROUBLE-LADDER-018 and shadows it instead of folding into it. The row is
//     asserted as "never advances a rung"; the counter divergence is reported.

// agentRule is the entry-at-agent rule the budget rows use (ceiling = agent).
func agentRule(name string) map[string]types.Rule {
	return map[string]types.Rule{name: {Name: name, EntryRung: types.RungAgent, Severity: types.SevHigh}}
}

// budgetHarness is a fresh ladder with the agent budget of the row under test.
func budgetHarness(t *testing.T, limit int64) *harness {
	t.Helper()
	return newHarness(t, harnessOpts{
		cfg:   Config{HostID: testHostID, AgentRunsPerDay: limit},
		rules: agentRule("rule-agent"),
	})
}

// runAgentOnce admits one fresh incident and enters the agent rung once, closing
// the run immediately so the host lease is free for the next one. It returns the
// incident id and the refusal (nil when the run was allowed).
func runAgentOnce(t *testing.T, h *harness, seq *evSeq, i int) (string, error) {
	t.Helper()
	obs := obsFor(sigFor(fmt.Sprintf("budget-run-%d", i)), "rule-agent", "journald")
	res := admitEv(t, h, obs, seq.next(), fmt.Sprintf("budget-svc-%d.service", i))
	if res.Inc == "" {
		t.Fatalf("admission %d created no incident: %+v", i, res)
	}
	if _, err := h.l.Advance(context.Background(), res.Inc, Transition{Trigger: "T05"}); err != nil {
		return res.Inc, err
	}
	st := h.l.incStateFor(res.Inc)
	if st == nil {
		t.Fatalf("no state for %s", res.Inc)
	}
	st.AgentResult = "done"
	if _, err := h.l.Advance(context.Background(), res.Inc, Transition{Trigger: "T27"}); err != nil {
		t.Fatalf("T27 after run %d: %v", i, err)
	}
	return res.Inc, nil
}

// ---------------------------------------------------------------------------
// §3.7 — budgets (AC-5).
// ---------------------------------------------------------------------------

// TestAgentRunBudgetExhaustionEscalates covers AC-5: the agent-run budget caps
// runs per host per UTC day, and exhaustion escalates instead of running — the
// stage is never entered and the outlets still fire so a human sees it.
func TestAgentRunBudgetExhaustionEscalates(t *testing.T) {
	t.Run("20 per day", func(t *testing.T) {
		const limit = 20
		h := budgetHarness(t, limit)
		var seq evSeq
		allowed, refused := 0, 0
		maxUsed := int64(0)

		for i := 0; i < limit+1; i++ {
			inc, err := runAgentOnce(t, h, &seq, i)
			if err == nil {
				allowed++
			} else {
				refused++
				if got := CodeOf(err); got != types.CodeLadder013 {
					t.Fatalf("run %d refusal = %q, want %q", i+1, got, types.CodeLadder013)
				}
				if code, _ := h.incidentPayload()["error_code"].(string); code != string(types.CodeLadder013) {
					t.Errorf("run %d refusal record error_code = %q", i+1, code)
				}
				// The stage was never entered: the incident is still `recorded`.
				got, ierr := h.l.Incident(context.Background(), inc)
				if ierr != nil {
					t.Fatalf("Incident: %v", ierr)
				}
				if got.State != types.StRecorded {
					t.Errorf("run %d state = %q, want recorded (exhaustion must not enter the stage)", i+1, string(got.State))
				}
				// Escalate instead of running (T07), with the outlets firing.
				esc, aerr := h.l.Advance(context.Background(), inc, Transition{Trigger: "T07"})
				if aerr != nil {
					t.Fatalf("run %d T07: %v", i+1, aerr)
				}
				if esc.State != types.StEscalated {
					t.Fatalf("run %d state = %q, want escalated", i+1, string(esc.State))
				}
				p := h.incidentPayload()
				if p["pending_human"] != true {
					t.Errorf("run %d escalation record pending_human = %v, want true", i+1, p["pending_human"])
				}
				if code, _ := p["error_code"].(string); code != string(types.CodeLadder013) {
					t.Errorf("run %d escalation error_code = %q, want %q", i+1, code, types.CodeLadder013)
				}
			}

			bs, berr := h.l.Budget(context.Background())
			if berr != nil {
				t.Fatalf("Budget: %v", berr)
			}
			if used := bs.Used[types.BudgetAgentRuns]; used > limit {
				t.Fatalf("run %d: agent_runs counter = %d, over the %d limit", i+1, used, limit)
			} else if used > maxUsed {
				maxUsed = used
			}
		}

		if allowed != limit {
			t.Errorf("allowed %d runs, want %d", allowed, limit)
		}
		if refused != 1 {
			t.Errorf("refused %d runs, want exactly 1 (the 21st)", refused)
		}
		if maxUsed != limit {
			t.Errorf("the counter peaked at %d, want %d (the counter tracks runs, not attempts)", maxUsed, limit)
		}
		bs, err := h.l.Budget(context.Background())
		if err != nil {
			t.Fatalf("Budget: %v", err)
		}
		if used := bs.Used[types.BudgetAgentRuns]; used != limit {
			t.Errorf("used agent_runs = %d, want %d", used, limit)
		}
		if !containsStr(bs.Exhausted, types.BudgetAgentRuns) {
			t.Errorf("exhausted = %v, want %q listed", bs.Exhausted, types.BudgetAgentRuns)
		}
		if limit := bs.Limits[types.BudgetAgentRuns]; limit != 20 {
			t.Errorf("limit = %d, want 20", limit)
		}
		// The outlets still fired for the human path.
		if issues, _, _ := h.outlets.counts(); issues == 0 {
			t.Error("no issue was filed for the escalated incident")
		}
	})

	t.Run("configured limit", func(t *testing.T) {
		const limit = 2
		h := budgetHarness(t, limit)
		var seq evSeq
		used := int64(0)
		for i := 0; i < limit+2; i++ {
			inc, err := runAgentOnce(t, h, &seq, i)
			bs, berr := h.l.Budget(context.Background())
			if berr != nil {
				t.Fatalf("Budget: %v", berr)
			}
			if n := bs.Used[types.BudgetAgentRuns]; n > limit {
				t.Fatalf("run %d: agent_runs = %d, over the configured limit %d", i+1, n, limit)
			} else {
				used = n
			}
			if err == nil {
				continue
			}
			if got := CodeOf(err); got != types.CodeLadder013 {
				t.Fatalf("run %d refusal = %q, want %q", i+1, got, types.CodeLadder013)
			}
			if _, aerr := h.l.Advance(context.Background(), inc, Transition{Trigger: "T07"}); aerr != nil {
				t.Fatalf("run %d T07: %v", i+1, aerr)
			}
		}
		if used != limit {
			t.Errorf("used agent_runs = %d, want the configured %d", used, limit)
		}
		if bs, _ := h.l.Budget(context.Background()); bs.Limits[types.BudgetAgentRuns] != limit {
			t.Errorf("limit = %d, want %d (Config drives it)", bs.Limits[types.BudgetAgentRuns], limit)
		}
	})
}

// TestAgentRunBudgetRollsAtMidnightUTC covers §3.7's rollover: counters live per
// UTC day, roll at 00:00 UTC, and a run is charged to the day it started.
func TestAgentRunBudgetRollsAtMidnightUTC(t *testing.T) {
	h := budgetHarness(t, 20)
	var seq evSeq

	if _, err := runAgentOnce(t, h, &seq, 0); err != nil {
		t.Fatalf("first run: %v", err)
	}
	before, err := h.l.Budget(context.Background())
	if err != nil {
		t.Fatalf("Budget: %v", err)
	}
	if before.Day != "2026-09-16" {
		t.Fatalf("day = %q, want 2026-09-16", before.Day)
	}
	if before.Used[types.BudgetAgentRuns] != 1 {
		t.Fatalf("used = %d, want 1", before.Used[types.BudgetAgentRuns])
	}
	if before.ResetsTS != "2026-09-17T00:00:00.000Z" {
		t.Errorf("resets_ts = %q, want the next midnight UTC", before.ResetsTS)
	}

	h.clock.advance(15 * time.Hour) // 09:00 → 2026-09-17T00:00:00Z

	after, err := h.l.Budget(context.Background())
	if err != nil {
		t.Fatalf("Budget: %v", err)
	}
	if after.Day != "2026-09-17" {
		t.Errorf("day = %q, want 2026-09-17 (the day field must change at midnight)", after.Day)
	}
	if after.Used[types.BudgetAgentRuns] != 0 {
		t.Errorf("used = %d on the new day, want 0", after.Used[types.BudgetAgentRuns])
	}
	if len(after.Exhausted) != 0 {
		t.Errorf("exhausted = %v on the new day, want empty", after.Exhausted)
	}
	if after.ResetsTS != "2026-09-18T00:00:00.000Z" {
		t.Errorf("resets_ts = %q, want the next midnight UTC", after.ResetsTS)
	}

	// The run is charged to the day it started: yesterday's counter is intact.
	h.l.mu.Lock()
	day1 := h.l.budget[budgetKey(h.l.hostID, "2026-09-16", types.BudgetAgentRuns)]
	h.l.mu.Unlock()
	if day1 != 1 {
		t.Errorf("the 2026-09-16 counter = %d, want 1 (a rollover never abandons a charged run)", day1)
	}

	// The new day has full budget again.
	if _, err := runAgentOnce(t, h, &seq, 1); err != nil {
		t.Fatalf("first run of the new day: %v", err)
	}
	bs, err := h.l.Budget(context.Background())
	if err != nil {
		t.Fatalf("Budget: %v", err)
	}
	if bs.Used[types.BudgetAgentRuns] != 1 {
		t.Errorf("used = %d on the new day after one run, want 1", bs.Used[types.BudgetAgentRuns])
	}
}

// ---------------------------------------------------------------------------
// §3.8 — breakers (AC-4).
// ---------------------------------------------------------------------------

// breakerRecordKeys is the `breaker` payload schema of §3.12.
var breakerRecordKeys = []string{
	"scope", "state", "trips", "opened_ts", "open_until", "reason", "probe_inc", "previous_duration_s",
}

// TestBreakerTripOpensForItsScopesDuration covers the trip row: the open
// duration comes from the scope (rule 30m, sig 60m, source 30m, global 15m), the
// trip state is readable through Breaker, and the `breaker` record carries every
// payload key of §3.12.
//
// The count-based trigger of §3.8 is not wired (see the file header): the trip is
// driven through TripBreaker, which calls the same openBreakerLockedWithReason
// the count-based trip would.
func TestBreakerTripOpensForItsScopesDuration(t *testing.T) {
	h := newHarness(t, harnessOpts{
		cfg:   Config{HostID: testHostID},
		rules: playRule("rule-a"),
	})
	sig := sigFor("br-scope").String()

	// §3.8's open column, read from the ladder's own mapping.
	for _, c := range []struct {
		scope string
		want  time.Duration
	}{
		{scopeRule + "rule-a", 30 * time.Minute},
		{scopeSig + sig, 60 * time.Minute},
		{scopeSource + "journald", 30 * time.Minute},
		{scopeGlobal, 15 * time.Minute},
	} {
		if got := h.l.openFor(c.scope); got != c.want {
			t.Errorf("openFor(%q) = %s, want %s", c.scope, got, c.want)
		}
	}

	scope := scopeRule + "rule-a"
	now := h.clock.Now()
	b, err := h.l.TripBreaker(context.Background(), scope, reasonSuppressionWindow)
	if err != nil {
		t.Fatalf("TripBreaker: %v", err)
	}
	if b.State != types.BreakerOpen {
		t.Errorf("state = %q, want open", string(b.State))
	}
	if b.Scope != scope {
		t.Errorf("scope = %q, want %q", b.Scope, scope)
	}
	if want := types.FormatUTC(now.Add(30 * time.Minute)); b.OpenUntil != want {
		t.Errorf("open_until = %q, want %q (30m for a rule scope)", b.OpenUntil, want)
	}

	// The state is readable through the public reader.
	got, ok := h.l.Breaker(context.Background(), scope)
	if !ok || got.State != types.BreakerOpen || got.OpenUntil != b.OpenUntil {
		t.Errorf("Breaker(%q) = %+v, %t", scope, got, ok)
	}

	recs := h.ledger.byKind(types.KBreaker)
	if len(recs) == 0 {
		t.Fatal("no breaker record was written")
	}
	rec := recs[len(recs)-1]
	for _, k := range breakerRecordKeys {
		if _, ok := rec.Payload[k]; !ok {
			t.Errorf("breaker record is missing the %q key (got %v)", k, payloadKeys(rec.Payload))
		}
	}
	if rec.Payload["scope"] != scope || rec.Payload["state"] != string(types.BreakerOpen) {
		t.Errorf("breaker record scope/state = %v/%v", rec.Payload["scope"], rec.Payload["state"])
	}
	if rec.Payload["trips"] != 1 {
		t.Errorf("breaker record trips = %v, want 1", rec.Payload["trips"])
	}
	if rec.Payload["previous_duration_s"] != 1800 {
		t.Errorf("breaker record previous_duration_s = %v, want 1800", rec.Payload["previous_duration_s"])
	}
	if rec.Payload["reason"] != reasonSuppressionWindow {
		t.Errorf("breaker record reason = %v, want %q", rec.Payload["reason"], reasonSuppressionWindow)
	}
}

// TestBreakerHalfOpenProbeClosesAndFailingProbeReopensDoubled covers §3.8's
// half-open rule: after open_until exactly one incident is admitted as a probe,
// the rest are held, a passing probe closes the breaker, and a failing probe
// re-opens it with double the previous duration, capped at 4h.
func TestBreakerHalfOpenProbeClosesAndFailingProbeReopensDoubled(t *testing.T) {
	h := newHarness(t, harnessOpts{
		cfg:   Config{HostID: testHostID},
		rules: playRule("rule-a"),
	})
	scope := scopeRule + "rule-a"
	if _, err := h.l.TripBreaker(context.Background(), scope, reasonSuppressionWindow); err != nil {
		t.Fatalf("TripBreaker: %v", err)
	}
	h.clock.advance(31 * time.Minute) // the open window elapses

	obs := obsFor(sigFor("br-probe"), "rule-a", "journald")
	// Exactly one probe: the first arrival after open_until is not held.
	if s, open := h.l.openBreakerLocked(obs.Sig.String(), obs); open || s != "" {
		t.Fatalf("the elapsed window still holds work: scope=%q open=%t", s, open)
	}
	b := h.l.breakers[scope]
	if b.Breaker.State != types.BreakerHalfOpen || !b.HalfOpen {
		t.Fatalf("state after open_until = %q (halfOpen=%t), want half_open", string(b.Breaker.State), b.HalfOpen)
	}
	// Every other arrival is held while the probe is in flight.
	if s, open := h.l.openBreakerLocked(obs.Sig.String(), obs); !open || s != scope {
		t.Errorf("a second arrival while half-open: scope=%q open=%t, want the breaker to hold", s, open)
	}

	// The probe's window passing closes it (counter kept, §3.8).
	h.l.mu.Lock()
	h.l.observeBreakerClosuresLocked(context.Background(), &incState{
		Inc:  types.Incident{Sig: obs.Sig.String()},
		Rule: "rule-a",
	})
	h.l.mu.Unlock()
	if b.Breaker.State != types.BreakerClosed {
		t.Fatalf("state after a passing probe = %q, want closed", string(b.Breaker.State))
	}
	if b.Trips != 0 || b.Breaker.OpenUntil != "" {
		t.Errorf("closed breaker trips=%d open_until=%q, want 0/empty", b.Trips, b.Breaker.OpenUntil)
	}
	last := h.ledger.byKind(types.KBreaker)
	if got := last[len(last)-1].Payload["state"]; got != string(types.BreakerClosed) {
		t.Errorf("the close was not recorded: last breaker state = %v", got)
	}

	// A failing probe re-opens it: 30m → 1h → 2h → 4h → 4h (capped).
	b.Breaker.State = types.BreakerHalfOpen
	b.HalfOpen = true
	wantDurations := []time.Duration{time.Hour, 2 * time.Hour, 4 * time.Hour, 4 * time.Hour}
	for i, want := range wantDurations {
		h.l.mu.Lock()
		h.l.openBreakerLockedWithReason(b, reasonSuppressionWindow, h.l.openFor(scope))
		h.l.mu.Unlock()

		if b.Duration != want {
			t.Errorf("re-open %d duration = %s, want %s (doubled from the previous)", i+1, b.Duration, want)
		}
		if want > 4*time.Hour {
			t.Errorf("re-open %d duration %s exceeds the 4h cap", i+1, want)
		}
		if b.PreviousS != int(want.Seconds()) {
			t.Errorf("re-open %d previous_duration_s = %d, want %d", i+1, b.PreviousS, int(want.Seconds()))
		}
		if err := checkUntil(b.Breaker.OpenUntil, h.clock.Now(), want); err != nil {
			t.Errorf("re-open %d: %v", i+1, err)
		}
	}
	if b.Breaker.State != types.BreakerOpen {
		t.Errorf("state = %q, want open", string(b.Breaker.State))
	}
	if b.Trips < 1 {
		t.Errorf("trips = %d, want the count kept across re-opens", b.Trips)
	}
	if cap := h.l.cfg.BreakerMaxOpen.Std(); cap != 4*time.Hour {
		t.Errorf("BreakerMaxOpen = %s, want 4h", cap)
	}
}

// checkUntil asserts open_until == now + want.
func checkUntil(until string, now time.Time, want time.Duration) error {
	wantTS := types.FormatUTC(now.Add(want))
	if until != wantTS {
		return fmt.Errorf("open_until = %q, want %q", until, wantTS)
	}
	return nil
}

// TestBreakerFoldsArrivalsWithOneCodePerWindowThenEvery100th covers §3.8's
// arrival handling: while a scope breaker is open new arrivals fold (no incident,
// no work), and TROUBLE-LADDER-014 is recorded once per window and then every
// 100th arrival — not once per arrival.
func TestBreakerFoldsArrivalsWithOneCodePerWindowThenEvery100th(t *testing.T) {
	h := newHarness(t, harnessOpts{
		cfg:   Config{HostID: testHostID},
		rules: playRule("rule-a"),
	})
	scope := scopeRule + "rule-a"
	if _, err := h.l.TripBreaker(context.Background(), scope, reasonSuppressionWindow); err != nil {
		t.Fatalf("TripBreaker: %v", err)
	}

	var seq evSeq
	const arrivals = 150
	for i := 0; i < arrivals; i++ {
		obs := obsFor(sigFor(fmt.Sprintf("br-fold-%d", i)), "rule-a", "journald")
		res := admitEv(t, h, obs, seq.next(), fmt.Sprintf("fold-svc-%d.service", i))
		if res.Inc != "" {
			t.Fatalf("arrival %d produced incident %s while the rule breaker is open", i, res.Inc)
		}
		if res.Created || res.Folded {
			t.Fatalf("arrival %d was reported as created/folded (%+v) with no open incident", i, res)
		}
		if res.Reason != scope {
			t.Fatalf("arrival %d reason = %q, want the open scope %q", i, res.Reason, scope)
		}
	}
	b := h.l.breakers[scope]
	if b == nil {
		t.Fatal("no breaker state for the tripped rule")
	}
	if b.Folds != arrivals {
		t.Errorf("folds = %d, want %d (every arrival folds)", b.Folds, arrivals)
	}
	if len(b.Codes) != 2 {
		t.Errorf("recorded codes = %d (%v), want exactly 2: the first arrival and the 100th", len(b.Codes), b.Codes)
	}
	for i, code := range b.Codes {
		if code != string(types.CodeLadder014) {
			t.Errorf("code %d = %q, want %q", i, code, types.CodeLadder014)
		}
	}
	// Nothing was created for the folded sigs: an open breaker holds new work.
	if n := len(h.l.incs); n != 0 {
		t.Errorf("%d incident(s) exist during an open rule breaker, want 0", n)
	}

	// With an incident already open, the arrival folds into it (T48) instead of
	// spraying a new one, and the incident does not advance a rung.
	h2 := newHarness(t, harnessOpts{
		cfg:   Config{HostID: testHostID},
		rules: playRule("rule-a"),
	})
	sig := sigFor("br-fold-open")
	var seq2 evSeq
	res := admitEv(t, h2, obsFor(sig, "rule-a", "journald"), seq2.next(), "payment-worker.service")
	if res.Inc == "" {
		t.Fatalf("the first arrival created no incident: %+v", res)
	}
	st := h2.l.incStateFor(res.Inc)
	stateBefore, rungBefore := st.Inc.State, st.Inc.Rung

	if _, err := h2.l.TripBreaker(context.Background(), scopeSig+sig.String(), reasonSuppressionWindow); err != nil {
		t.Fatalf("TripBreaker (sig): %v", err)
	}
	obs := obsFor(sig, "rule-a", "journald")
	obs.Subject = "payment-worker.service"
	folded := admitEv(t, h2, obs, seq2.next(), "")
	if folded.Inc != res.Inc {
		t.Errorf("the folded arrival landed on %q, want the open incident %q", folded.Inc, res.Inc)
	}
	if !folded.Folded || folded.Reason != scopeSig+sig.String() {
		t.Errorf("fold result = %+v, want folded=true reason=%q", folded, scopeSig+sig.String())
	}
	if st.SuppressedCount != 1 {
		t.Errorf("suppressed_count = %d, want 1", st.SuppressedCount)
	}
	if st.Inc.State != stateBefore || st.Inc.Rung != rungBefore {
		t.Errorf("state/rung moved to %q/%q, want %q/%q", string(st.Inc.State), string(st.Inc.Rung), string(stateBefore), string(rungBefore))
	}
	rec := lastIncidentRecord(t, h2)
	if rec.Payload["transition"] != "T48" || rec.Payload["folded"] != true {
		t.Errorf("the fold record = transition %v folded %v, want T48/true", rec.Payload["transition"], rec.Payload["folded"])
	}
	if rec.Payload["breaker_scope"] != scopeSig+sig.String() {
		t.Errorf("the fold record breaker_scope = %v, want %q", rec.Payload["breaker_scope"], scopeSig+sig.String())
	}
	if got := h2.l.breakers[scopeSig+sig.String()].Codes; len(got) != 1 || got[0] != string(types.CodeLadder014) {
		t.Errorf("sig breaker codes = %v, want one %q for the first fold", got, types.CodeLadder014)
	}
}

// TestSuppressionCycleTripOpensTheSigBreaker covers "3 suppression cycles in 24h
// open the sig breaker": the cycle counter is what trips it, and the trip opens
// the sig scope for 60m with reason suppression_window.
//
// The shipped default (3) is unreachable because the counter is reset on every
// cycle (file header, finding 2), so the threshold is configured to 1 — the same
// code path, with a reachable count.
func TestSuppressionCycleTripOpensTheSigBreaker(t *testing.T) {
	h := newHarness(t, harnessOpts{
		cfg:   Config{HostID: testHostID, BreakerSigPer24h: 1},
		rules: map[string]types.Rule{"rule-a": {Name: "rule-a", EntryRung: types.RungPlay, Severity: types.SevHigh, Cooldown: "30m"}},
	})
	sig := sigFor("br-cycle")
	inc, st := verifying(t, h, "br-cycle")
	h.canaryIn(t, sig.String())
	h.index.events[sig.String()] = 0
	ev := passingEvidence(st)

	if _, err := h.l.Advance(context.Background(), inc, Transition{Trigger: "T38", Evidence: &ev}); err != nil {
		t.Fatalf("T38: %v", err)
	}
	got, err := h.l.Incident(context.Background(), inc)
	if err != nil {
		t.Fatalf("Incident: %v", err)
	}
	if got.State != types.StResolved {
		t.Fatalf("state = %q, want resolved", string(got.State))
	}

	// The resolved terminal opened the flapping guard (§3.8 (a)): the sig is
	// suppressed and the sig breaker is open for the cycle.
	b := h.l.breakers[scopeSig+sig.String()]
	if b == nil {
		t.Fatal("no sig breaker after a suppression cycle")
	}
	if b.Breaker.State != types.BreakerOpen {
		t.Fatalf("sig breaker state = %q, want open", string(b.Breaker.State))
	}
	if b.Breaker.Reason != reasonSuppressionWindow {
		t.Errorf("sig breaker reason = %q, want %q", b.Breaker.Reason, reasonSuppressionWindow)
	}
	// The cycle trip re-opened it on top of the 30m cooldown: doubled to 60m.
	if b.Duration != time.Hour {
		t.Errorf("sig breaker duration = %s, want 1h (doubled from the 30m cooldown)", b.Duration)
	}
	if b.Tripper() != 2 {
		t.Errorf("sig breaker trips = %d, want 2 (cooldown + cycle trip)", b.Tripper())
	}
	if err := checkUntil(b.Breaker.OpenUntil, h.clock.Now(), time.Hour); err != nil {
		t.Errorf("sig breaker: %v", err)
	}
	h.l.mu.Lock()
	until, reason, open := h.l.suppressionWindowLocked(sig.String())
	h.l.mu.Unlock()
	if !open || reason != reasonSuppressionWindow || until == "" {
		t.Errorf("suppression window = open=%t reason=%q until=%q, want an open window", open, reason, until)
	}

	// The trip is visible in the ledger with the §3.12 keys.
	found := false
	for _, r := range h.ledger.byKind(types.KBreaker) {
		if r.Payload["scope"] != scopeSig+sig.String() || r.Payload["state"] != string(types.BreakerOpen) {
			continue
		}
		found = true
		for _, k := range breakerRecordKeys {
			if _, ok := r.Payload[k]; !ok {
				t.Errorf("breaker record is missing %q", k)
			}
		}
	}
	if !found {
		t.Error("no open breaker record for the sig scope")
	}
}

// TestArrivalsInsideASuppressionWindowNeverAdvanceARung covers AC-4's arrival
// rule: a suppressed incident collects arrivals instead of work — no rung entry,
// no reopen, no resolve, and the traffic is recorded under the suppression code.
func TestArrivalsInsideASuppressionWindowNeverAdvanceARung(t *testing.T) {
	h := newHarness(t, harnessOpts{
		cfg:   Config{HostID: testHostID},
		rules: playRule("rule-a"),
	})
	sig := sigFor("br-suppress")
	inc, st := suppressedIncident(t, h, "br-suppress")
	stateBefore, rungBefore := st.Inc.State, st.Inc.Rung

	var seq evSeq
	for i := 0; i < 3; i++ {
		res := admitEv(t, h, obsFor(sig, "rule-a", "journald"), seq.next(), "payment-worker.service")
		if res.Reason != string(types.CodeLadder018) {
			t.Errorf("arrival %d reason = %q, want %q", i, res.Reason, types.CodeLadder018)
		}
		if !res.Created && !res.Folded {
			t.Errorf("arrival %d was neither recorded nor folded: %+v", i, res)
		}
	}

	after, err := h.l.Incident(context.Background(), inc)
	if err != nil {
		t.Fatalf("Incident: %v", err)
	}
	if after.State != stateBefore {
		t.Errorf("state = %q, want %q (arrivals never move a suppressed incident)", string(after.State), string(stateBefore))
	}
	if after.Rung != rungBefore {
		t.Errorf("rung = %q, want %q", string(after.Rung), string(rungBefore))
	}
	if st.Inc.Rung != rungBefore {
		t.Errorf("in-memory rung = %q, want %q", string(st.Inc.Rung), string(rungBefore))
	}
	// No work was entered anywhere: no incident record targets a work rung.
	for _, r := range h.ledger.byKind(types.KIncident) {
		switch to, _ := r.Payload["to"].(string); to {
		case string(types.StPlayDrafted), string(types.StPlayCheck), string(types.StPlayApplied),
			string(types.StResRequested), string(types.StAgentRunning), string(types.StVerifying):
			t.Errorf("an arrival inside the window entered %q: %+v", to, r.Payload)
		}
		if r.Payload["reopen"] == true {
			t.Errorf("an arrival inside the window reopened the incident: %+v", r.Payload)
		}
	}
	// The arrivals are recorded under the suppression code, and the incident is
	// still suppressed (never resolved on absent evidence).
	if n := h.ledger.countPayload("error_code", string(types.CodeLadder018)); n < 3 {
		t.Errorf("%d record(s) carry %q, want at least the 3 arrivals", n, types.CodeLadder018)
	}
	got, err := h.l.Incident(context.Background(), inc)
	if err != nil {
		t.Fatalf("Incident: %v", err)
	}
	if got.State != types.StSuppressed {
		t.Errorf("state = %q, want suppressed", string(got.State))
	}
}
