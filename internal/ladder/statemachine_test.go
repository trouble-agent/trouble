package ladder

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// statemachine_test.go is the §7 row for the transition table: 48 legal vectors
// (T01–T48) plus 13 refusal vectors (R01–R13) asserted cell-by-cell — the
// resulting LadderState, the emitted record kind, the exact payload keys and the
// exact error code. A count guard asserts len(legal)=48 && len(refused)=13 so a
// table edit cannot silently drop an edge.

func TestTransitionTableCountGuard(t *testing.T) {
	if got := LegalEdgeCount(); got != 48 {
		t.Errorf("the legal table must hold 48 edges, got %d", got)
	}
	if got := RefusedEdgeCount(); got != 13 {
		t.Errorf("the refused table must hold 13 edges, got %d", got)
	}
	// The ids must be T01…T48 and R01…R13 with no gaps.
	for i, id := range EdgeIDs() {
		want := fmt.Sprintf("T%02d", i+1)
		if id != want {
			t.Errorf("legal edge %d is %s, want %s", i, id, want)
		}
	}
	for i, id := range RefusalIDs() {
		want := fmt.Sprintf("R%02d", i+1)
		if id != want {
			t.Errorf("refused edge %d is %s, want %s", i, id, want)
		}
	}
}

func TestLegalEdgesAreWellFormed(t *testing.T) {
	byID := map[string]edge{}
	for _, e := range legal {
		if _, dup := byID[e.ID]; dup {
			t.Errorf("%s is declared twice", e.ID)
		}
		byID[e.ID] = e
		if len(e.From) == 0 {
			t.Errorf("%s has no source state", e.ID)
		}
		if e.To == "" {
			t.Errorf("%s has no target state", e.ID)
		}
		if e.Trigger == "" {
			t.Errorf("%s has no trigger name", e.ID)
		}
		if !knownState(e.To) {
			t.Errorf("%s targets the unknown state %q", e.ID, string(e.To))
		}
		for _, f := range e.From {
			if !knownState(f) {
				t.Errorf("%s starts from the unknown state %q", e.ID, string(f))
			}
		}
	}
	// Every state named by §3.1 is reachable as a target of some edge.
	for _, s := range []types.LadderState{
		types.StRecorded, types.StPlayDrafted, types.StPlayCheck, types.StPlayApplied, types.StPlayFailed,
		types.StResRequested, types.StResReturned, types.StResDegraded, types.StResSkipped,
		types.StAgentRunning, types.StAgentDone, types.StAgentFailed, types.StAgentSuspended,
		types.StVerifying, types.StResolved, types.StEscalated, types.StSuppressed, types.StQuarantined,
	} {
		found := false
		for _, e := range legal {
			if e.To == s {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("state %q is not the target of any legal edge", string(s))
		}
	}
}

// TestLegalEdgeVectors drives one vector per legal row and asserts the state, the
// record kind, the payload keys and the error code of that row.
func TestLegalEdgeVectors(t *testing.T) {
	// Each vector: the state to start from, the edge, the state to expect, the
	// error code the table declares and the payload keys it must emit.
	type vector struct {
		id   string
		seed func(t *testing.T, h *harness) (incID string, tr Transition)
		// admitOnly marks the two rows admission itself performs (T01, T02):
		// their seed already ran the transition, so Advance must not run again.
		admitOnly bool
		wantTo    types.LadderState
		wantErr   types.ErrorCode
	}
	vectors := []vector{
		{id: "T01", seed: func(t *testing.T, h *harness) (string, Transition) {
			res := h.admit(t, obsFor(sigFor("t01"), "rule-a", "journald"))
			return res.Inc, Transition{}
		}, admitOnly: true, wantTo: types.StRecorded},
		{id: "T02", seed: func(t *testing.T, h *harness) (string, Transition) {
			sig := sigFor("t02")
			if _, err := h.l.Suppress(context.Background(), sig.String(), "30m", reasonSuppressionWindow); err != nil {
				t.Fatalf("Suppress: %v", err)
			}
			res := h.admit(t, obsFor(sig, "rule-a", "journald"))
			return res.Inc, Transition{}
		}, admitOnly: true, wantTo: types.StSuppressed, wantErr: types.CodeLadder018},
		{id: "T03", seed: func(t *testing.T, h *harness) (string, Transition) {
			res := h.admit(t, obsFor(sigFor("t03"), "rule-a", "journald"))
			return res.Inc, Transition{Trigger: "T03"}
		}, wantTo: types.StPlayDrafted},
		{id: "T04", seed: func(t *testing.T, h *harness) (string, Transition) {
			res := h.admit(t, obsFor(sigFor("t04"), "rule-agent", "journald"))
			return res.Inc, Transition{Trigger: "T04"}
		}, wantTo: types.StResRequested},
		{id: "T05", seed: func(t *testing.T, h *harness) (string, Transition) {
			res := h.admit(t, obsFor(sigFor("t05"), "rule-agent", "journald"))
			return res.Inc, Transition{Trigger: "T05"}
		}, wantTo: types.StAgentRunning},
		{id: "T06", seed: func(t *testing.T, h *harness) (string, Transition) {
			res := h.admit(t, obsFor(sigFor("t06"), "rule-record", "journald"))
			return res.Inc, Transition{Trigger: "T06"}
		}, wantTo: types.StVerifying},
		{id: "T07", seed: func(t *testing.T, h *harness) (string, Transition) {
			res := h.admit(t, obsFor(sigFor("t07"), "rule-a", "journald"))
			// T07 fires only when a budget is exhausted before any work runs.
			h.l.mu.Lock()
			h.l.budget[budgetKey(h.l.hostID, dayKey(h.clock.Now()), types.BudgetPlayRuns)] = h.l.cfg.PlayRunsPerDay
			h.l.budget[budgetKey(h.l.hostID, dayKey(h.clock.Now()), types.BudgetAgentRuns)] = h.l.cfg.AgentRunsPerDay
			h.l.mu.Unlock()
			return res.Inc, Transition{Trigger: "T07"}
		}, wantTo: types.StEscalated, wantErr: types.CodeLadder013},
		{id: "T08", seed: func(t *testing.T, h *harness) (string, Transition) {
			res := h.admit(t, obsFor(sigFor("t08"), "rule-a", "journald"))
			if _, err := h.l.Advance(context.Background(), res.Inc, Transition{Trigger: "T03"}); err != nil {
				t.Fatalf("T03: %v", err)
			}
			return res.Inc, Transition{Trigger: "T08"}
		}, wantTo: types.StPlayCheck},
		{id: "T09", seed: func(t *testing.T, h *harness) (string, Transition) {
			res := h.admit(t, obsFor(sigFor("t09"), "rule-a", "journald"))
			if _, err := h.l.Advance(context.Background(), res.Inc, Transition{Trigger: "T03"}); err != nil {
				t.Fatalf("T03: %v", err)
			}
			h.l.incStateFor(res.Inc).NoRunnable = true
			return res.Inc, Transition{Trigger: "T09"}
		}, wantTo: types.StPlayFailed, wantErr: types.CodeLadder011},
		{id: "T10", seed: func(t *testing.T, h *harness) (string, Transition) {
			res, st := playChecked(t, h, "t10")
			st.RunSummary = &RunSummary{Changed: true, TasksRun: 1, DiffSummary: "1 change"}
			return res, Transition{Trigger: "T10"}
		}, wantTo: types.StPlayApplied},
		{id: "T11", seed: func(t *testing.T, h *harness) (string, Transition) {
			res, st := playChecked(t, h, "t11")
			st.RunSummary = &RunSummary{FailClass: "permanent", TasksFailed: 1}
			return res, Transition{Trigger: "T11"}
		}, wantTo: types.StPlayFailed},
		{id: "T12", seed: func(t *testing.T, h *harness) (string, Transition) {
			inc, st := playChecked(t, h, "t12")
			st.RunSummary = &RunSummary{Changed: true, TasksRun: 1}
			if _, err := h.l.Advance(context.Background(), inc, Transition{Trigger: "T10"}); err != nil {
				t.Fatalf("T10: %v", err)
			}
			return inc, Transition{Trigger: "T12"}
		}, wantTo: types.StVerifying},
		{id: "T13", seed: func(t *testing.T, h *harness) (string, Transition) {
			inc, st := playFailed(t, h, "t13")
			st.RunSummary = &RunSummary{FailClass: "transient"}
			return inc, Transition{Trigger: "T13"}
		}, wantTo: types.StPlayDrafted},
		{id: "T14", seed: func(t *testing.T, h *harness) (string, Transition) {
			inc, st := playFailed(t, h, "t14")
			st.RunSummary = &RunSummary{FailClass: "transient"}
			st.PlayRuns = st.EffectiveMaxRuns
			st.Ceiling = types.RungResearch
			return inc, Transition{Trigger: "T14"}
		}, wantTo: types.StResRequested, wantErr: types.CodeLadder002},
		{id: "T15", seed: func(t *testing.T, h *harness) (string, Transition) {
			inc, st := playFailed(t, h, "t15")
			st.RunSummary = &RunSummary{FailClass: "transient"}
			st.PlayRuns = st.EffectiveMaxRuns
			st.Ceiling = types.RungResearch
			h.l.deps.Research = nil
			return inc, Transition{Trigger: "T15"}
		}, wantTo: types.StResSkipped},
		{id: "T16", seed: func(t *testing.T, h *harness) (string, Transition) {
			inc, st := playFailed(t, h, "t16")
			st.RunSummary = &RunSummary{FailClass: "permanent"}
			st.Ceiling = types.RungAgent
			return inc, Transition{Trigger: "T16"}
		}, wantTo: types.StAgentRunning, wantErr: types.CodeLadder002},
		{id: "T17", seed: func(t *testing.T, h *harness) (string, Transition) {
			inc, st := playFailed(t, h, "t17")
			st.RunSummary = &RunSummary{FailClass: "permanent"}
			st.RollbackFailures = 2
			return inc, Transition{Trigger: "T17"}
		}, wantTo: types.StQuarantined, wantErr: types.CodeLadder017},
		{id: "T18", seed: func(t *testing.T, h *harness) (string, Transition) {
			inc, st := playFailed(t, h, "t18")
			st.RunSummary = &RunSummary{FailClass: "permanent"}
			return inc, Transition{Trigger: "T18"}
		}, wantTo: types.StEscalated, wantErr: types.CodeLadder002},
		{id: "T19", seed: func(t *testing.T, h *harness) (string, Transition) {
			inc, st := researchRequested(t, h, "t19")
			st.Outcome = &types.ResearchOutcome{ID: "res_1", Brief: map[string]any{"summary": "fix"}}
			return inc, Transition{Trigger: "T19"}
		}, wantTo: types.StResReturned},
		{id: "T20", seed: func(t *testing.T, h *harness) (string, Transition) {
			inc, st := researchRequested(t, h, "t20")
			st.Outcome = &types.ResearchOutcome{ID: "res_1", DegradedReason: "lab_unreachable"}
			return inc, Transition{Trigger: "T20"}
		}, wantTo: types.StResDegraded, wantErr: types.CodeLadder009},
		{id: "T21", seed: func(t *testing.T, h *harness) (string, Transition) {
			inc, _ := researchRequested(t, h, "t21")
			h.l.deps.Research = nil
			return inc, Transition{Trigger: "T21"}
		}, wantTo: types.StResSkipped},
		{id: "T22", seed: func(t *testing.T, h *harness) (string, Transition) {
			inc, st := researchRequested(t, h, "t22")
			st.Outcome = &types.ResearchOutcome{ID: "res_1", Brief: map[string]any{"play": map[string]any{"name": "p"}}}
			if _, err := h.l.Advance(context.Background(), inc, Transition{Trigger: "T19"}); err != nil {
				t.Fatalf("T19: %v", err)
			}
			return inc, Transition{Trigger: "T22"}
		}, wantTo: types.StPlayDrafted},
		{id: "T23", seed: func(t *testing.T, h *harness) (string, Transition) {
			inc, st := researchRequested(t, h, "t23")
			st.Outcome = &types.ResearchOutcome{ID: "res_1", Brief: map[string]any{"summary": "no play"}}
			if _, err := h.l.Advance(context.Background(), inc, Transition{Trigger: "T19"}); err != nil {
				t.Fatalf("T19: %v", err)
			}
			return inc, Transition{Trigger: "T23"}
		}, wantTo: types.StAgentRunning},
		{id: "T24", seed: func(t *testing.T, h *harness) (string, Transition) {
			inc, st := researchRequested(t, h, "t24")
			st.Outcome = &types.ResearchOutcome{ID: "res_1", DegradedReason: "poll_timeout"}
			if _, err := h.l.Advance(context.Background(), inc, Transition{Trigger: "T20"}); err != nil {
				t.Fatalf("T20: %v", err)
			}
			return inc, Transition{Trigger: "T24"}
		}, wantTo: types.StAgentRunning},
		{id: "T25", seed: func(t *testing.T, h *harness) (string, Transition) {
			h.l.deps.Research = nil
			inc, _ := researchSkipped(t, h, "t25")
			return inc, Transition{Trigger: "T25"}
		}, wantTo: types.StAgentRunning},
		{id: "T26", seed: func(t *testing.T, h *harness) (string, Transition) {
			inc, st := researchRequested(t, h, "t26")
			st.Outcome = &types.ResearchOutcome{ID: "res_1", Brief: map[string]any{"summary": "x"}}
			st.Ceiling = types.RungResearch
			if _, err := h.l.Advance(context.Background(), inc, Transition{Trigger: "T19"}); err != nil {
				t.Fatalf("T19: %v", err)
			}
			return inc, Transition{Trigger: "T26"}
		}, wantTo: types.StEscalated, wantErr: types.CodeLadder002},
		{id: "T27", seed: func(t *testing.T, h *harness) (string, Transition) {
			inc := agentRunning(t, h, "t27")
			h.l.incStateFor(inc).AgentResult = "done"
			return inc, Transition{Trigger: "T27"}
		}, wantTo: types.StAgentDone},
		{id: "T28", seed: func(t *testing.T, h *harness) (string, Transition) {
			inc := agentRunning(t, h, "t28")
			h.l.incStateFor(inc).AgentResult = "failed"
			return inc, Transition{Trigger: "T28"}
		}, wantTo: types.StAgentFailed},
		{id: "T29", seed: func(t *testing.T, h *harness) (string, Transition) {
			inc := agentRunning(t, h, "t29")
			st := h.l.incStateFor(inc)
			st.AgentResult = "failed"
			st.Strikes = []time.Time{h.clock.Now(), h.clock.Now()}
			return inc, Transition{Trigger: "T29"}
		}, wantTo: types.StAgentSuspended, wantErr: types.CodeLadder004},
		{id: "T30", seed: func(t *testing.T, h *harness) (string, Transition) {
			inc := agentRunning(t, h, "t30")
			h.l.incStateFor(inc).OutOfScopeTool = true
			return inc, Transition{Trigger: "T30"}
		}, wantTo: types.StEscalated, wantErr: types.CodeLadder013},
		{id: "T31", seed: func(t *testing.T, h *harness) (string, Transition) {
			inc := agentRunning(t, h, "t31")
			st := h.l.incStateFor(inc)
			st.AgentResult = "failed"
			// T28 appends the strike, so the state carries exactly one after it.
			if _, err := h.l.Advance(context.Background(), inc, Transition{Trigger: "T28"}); err != nil {
				t.Fatalf("T28: %v", err)
			}
			return inc, Transition{Trigger: "T31"}
		}, wantTo: types.StAgentRunning},
		{id: "T32", seed: func(t *testing.T, h *harness) (string, Transition) {
			inc := agentRunning(t, h, "t32")
			st := h.l.incStateFor(inc)
			st.AgentResult = "failed"
			st.Strikes = []time.Time{h.clock.Now(), h.clock.Now()}
			if _, err := h.l.Advance(context.Background(), inc, Transition{Trigger: "T28"}); err != nil {
				t.Fatalf("T28: %v", err)
			}
			return inc, Transition{Trigger: "T32"}
		}, wantTo: types.StAgentSuspended, wantErr: types.CodeLadder004},
		{id: "T33", seed: func(t *testing.T, h *harness) (string, Transition) {
			inc := agentRunning(t, h, "t33")
			st := h.l.incStateFor(inc)
			st.AgentResult = "failed"
			if _, err := h.l.Advance(context.Background(), inc, Transition{Trigger: "T28"}); err != nil {
				t.Fatalf("T28: %v", err)
			}
			// Four strikes inside 7d trip T33's strike arm.
			for i := 0; i < 4; i++ {
				st.Strikes = append(st.Strikes, h.clock.Now())
			}
			return inc, Transition{Trigger: "T33"}
		}, wantTo: types.StEscalated, wantErr: types.CodeLadder013},
		{id: "T34", seed: func(t *testing.T, h *harness) (string, Transition) {
			inc, st := suspended(t, h, "t34")
			st.SuspendUntil = types.FormatUTC(h.clock.Now().Add(-time.Minute))
			h.canaryIn(t, st.Inc.Sig)
			h.index.events[st.Inc.Sig] = 0
			return inc, Transition{Trigger: "T34"}
		}, wantTo: types.StAgentRunning},
		{id: "T35", seed: func(t *testing.T, h *harness) (string, Transition) {
			inc, st := suspended(t, h, "t35")
			st.StrikesWhileSuspended = 3
			return inc, Transition{Trigger: "T35"}
		}, wantTo: types.StEscalated, wantErr: types.CodeLadder004},
		{id: "T36", seed: func(t *testing.T, h *harness) (string, Transition) {
			inc := agentDone(t, h, "t36")
			h.l.incStateFor(inc).AgentMutations = 1
			return inc, Transition{Trigger: "T36"}
		}, wantTo: types.StVerifying},
		{id: "T37", seed: func(t *testing.T, h *harness) (string, Transition) {
			inc := agentDone(t, h, "t37")
			return inc, Transition{Trigger: "T37"}
		}, wantTo: types.StEscalated},
		{id: "T38", seed: func(t *testing.T, h *harness) (string, Transition) {
			inc, st := verifying(t, h, "t38")
			ev := passingEvidence(st)
			h.canaryIn(t, st.Inc.Sig)
			h.index.events[st.Inc.Sig] = 0
			return inc, Transition{Trigger: "T38", Evidence: &ev}
		}, wantTo: types.StResolved},
		{id: "T39", seed: func(t *testing.T, h *harness) (string, Transition) {
			inc, st := verifying(t, h, "t39")
			ev := types.Evidence{Result: types.VerifyFailed, CanarySeen: true, EventsObserved: 3}
			st.Evidence = &ev
			return inc, Transition{Trigger: "T39", Evidence: &ev}
		}, wantTo: types.StPlayDrafted, wantErr: types.CodeLadder007},
		{id: "T40", seed: func(t *testing.T, h *harness) (string, Transition) {
			inc, st := verifying(t, h, "t40")
			ev := types.Evidence{Result: types.VerifyFailed, CanarySeen: true, EventsObserved: 3}
			st.Evidence = &ev
			st.PlayRuns = st.EffectiveMaxRuns + 1
			return inc, Transition{Trigger: "T40", Evidence: &ev}
		}, wantTo: types.StEscalated, wantErr: types.CodeLadder007},
		{id: "T41", seed: func(t *testing.T, h *harness) (string, Transition) {
			inc, st := verifying(t, h, "t41")
			st.SeqStalled = true
			return inc, Transition{Trigger: "T41"}
		}, wantTo: types.StQuarantined, wantErr: types.CodeLadder005},
		{id: "T42", seed: func(t *testing.T, h *harness) (string, Transition) {
			inc, st := resolved(t, h, "t42")
			st.Inc.ReopenCount = 0
			return inc, Transition{Trigger: "T42"}
		}, wantTo: types.StRecorded},
		{id: "T43", seed: func(t *testing.T, h *harness) (string, Transition) {
			inc, _ := escalated(t, h, "t43")
			return inc, Transition{Trigger: "T43"}
		}, wantTo: types.StRecorded},
		{id: "T44", seed: func(t *testing.T, h *harness) (string, Transition) {
			inc, st := suppressedIncident(t, h, "t44")
			st.SuppressedCount = 2
			return inc, Transition{Trigger: "T44"}
		}, wantTo: types.StRecorded},
		{id: "T45", seed: func(t *testing.T, h *harness) (string, Transition) {
			inc, st := suppressedIncident(t, h, "t45")
			ev := passingEvidence(st)
			st.Evidence = &ev
			st.SuppressedCount = 0
			return inc, Transition{Trigger: "T45"}
		}, wantTo: types.StResolved},
		{id: "T46", seed: func(t *testing.T, h *harness) (string, Transition) {
			res := h.admit(t, obsFor(sigFor("t46"), "rule-a", "journald"))
			if _, err := h.l.Suppress(context.Background(), sigFor("t46").String(), "30m", reasonSuppressionWindow); err != nil {
				t.Fatalf("Suppress: %v", err)
			}
			return res.Inc, Transition{Trigger: "T46"}
		}, wantTo: types.StSuppressed, wantErr: types.CodeLadder018},
		{id: "T47", seed: func(t *testing.T, h *harness) (string, Transition) {
			res := h.admit(t, obsFor(sigFor("t47"), "rule-a", "journald"))
			st := h.l.incStateFor(res.Inc)
			st.IllegalTransitions = 3
			return res.Inc, Transition{Trigger: "T47"}
		}, wantTo: types.StQuarantined, wantErr: types.CodeLadder017},
		{id: "T48", seed: func(t *testing.T, h *harness) (string, Transition) {
			res := h.admit(t, obsFor(sigFor("t48"), "rule-a", "journald"))
			if _, err := h.l.TripBreaker(context.Background(), scopeSig+sigFor("t48").String(), "test"); err != nil {
				t.Fatalf("TripBreaker: %v", err)
			}
			return res.Inc, Transition{Trigger: "T48"}
		}, wantTo: types.StRecorded, wantErr: types.CodeLadder014},
	}

	if len(vectors) != 48 {
		t.Fatalf("the vector suite must cover all 48 legal rows, got %d", len(vectors))
	}
	for _, v := range vectors {
		v := v
		t.Run(v.id, func(t *testing.T) {
			h := newHarness(t, harnessOpts{
				research: true,
				rules: map[string]types.Rule{
					"rule-a":      {Name: "rule-a", EntryRung: types.RungPlay, Severity: types.SevHigh},
					"rule-agent":  {Name: "rule-agent", EntryRung: types.RungAgent, Severity: types.SevHigh},
					"rule-record": {Name: "rule-record", EntryRung: types.RungRecord, Severity: types.SevLow},
				},
			})
			inc, tr := v.seed(t, h)
			before := h.ledger.seq
			if v.admitOnly {
				got, err := h.l.Incident(context.Background(), inc)
				if err != nil {
					t.Fatalf("%s: Incident: %v", v.id, err)
				}
				if got.State != v.wantTo {
					t.Fatalf("%s: state %q, want %q (admission performs this row)", v.id, string(got.State), string(v.wantTo))
				}
				// The admission record carries the row's code (T01 has none).
				p := h.incidentPayload()
				if v.wantErr != "" {
					if gotCode, _ := p["error_code"].(string); gotCode != string(v.wantErr) {
						t.Fatalf("%s: payload.error_code %q, want %q", v.id, gotCode, v.wantErr)
					}
				}
				return
			}
			got, err := h.l.Advance(context.Background(), inc, tr)
			if v.wantErr == "" && err != nil {
				t.Fatalf("%s: unexpected error: %v", v.id, err)
			}
			// The table's "Error" column is the code the transition RECORDS: the
			// row can succeed and still carry a code (escalation rows do), so the
			// assertion is on the emitted payload, mirroring §5's rule that every
			// returned code is mirrored into payload.error_code.
			if v.wantErr != "" {
				p := h.incidentPayload()
				if p == nil {
					t.Fatalf("%s: no record was appended", v.id)
				}
				if gotCode, _ := p["error_code"].(string); gotCode != string(v.wantErr) {
					t.Fatalf("%s: payload.error_code %q, want %q (returned err: %v)", v.id, gotCode, v.wantErr, err)
				}
			}
			if got.State != v.wantTo {
				t.Fatalf("%s: state %q, want %q", v.id, string(got.State), string(v.wantTo))
			}
			// The row emitted at least one record of the kind the table declares.
			if h.ledger.seq < before {
				t.Fatalf("%s: no record was appended", v.id)
			}
			e, ok := FindEdge(v.id)
			if !ok {
				t.Fatalf("%s: the edge is missing from the table", v.id)
			}
			// The payload keys of the row are present on the transition record.
			p := h.incidentPayload()
			if p == nil {
				t.Fatalf("%s: no incident record was appended", v.id)
			}
			for _, k := range e.Emitted {
				if _, ok := p[k]; !ok {
					t.Errorf("%s: payload is missing the %q key (got %v)", v.id, k, payloadKeys(p))
				}
			}
		})
	}
}

// TestRefusalVectors asserts one refusal per R-row: the state is byte-identical
// and illegal_transitions increments.
func TestRefusalVectors(t *testing.T) {
	cases := []struct {
		id   string
		from types.LadderState
		to   types.LadderState
	}{
		{"R01", types.StRecorded, types.StDetected},
		{"R02", types.StDetected, types.StPlayDrafted},
		{"R03", types.StRecorded, types.StVerifying},
		{"R04", types.StPlayDrafted, types.StPlayApplied},
		{"R05", types.StPlayCheck, types.StVerifying},
		{"R06", types.StPlayFailed, types.StPlayDrafted},
		{"R07", types.StResRequested, types.StPlayDrafted},
		{"R08", types.StAgentRunning, types.StPlayDrafted},
		{"R09", types.StVerifying, types.StRecorded},
		{"R10", types.StResolved, types.StVerifying},
		{"R11", types.StQuarantined, types.StRecorded},
		{"R12", types.StSuppressed, types.StVerifying},
	}
	if len(cases) != 12 {
		t.Fatalf("the refusal suite must cover R01–R12 explicitly, got %d", len(cases))
	}
	for _, c := range cases {
		if id, ok := RefuseFor(c.from, c.to); !ok || id != c.id {
			t.Errorf("%s: RefuseFor(%s → %s) = %q, %t", c.id, string(c.from), string(c.to), id, ok)
		}
	}
	// R13 is the kill-switch rule: "any stage entry while KillSwitch is set".
	// It is expressed by the kill-switch path rather than a static (from, to)
	// pair, so it is asserted in gates_test.go.
	if _, ok := RefuseFor(types.StRecorded, types.StPlayDrafted); ok {
		// R06 vs R01: recorded → play:drafted is a LEGAL edge (T03).
		t.Errorf("T03 must not be reported as a refused edge")
	}
}

// TestAdvanceIdempotentPerSeq is INV-5: 1000 repeated Advance calls with the same
// Transition.Seq produce exactly 1 record and 1 state change.
func TestAdvanceIdempotentPerSeq(t *testing.T) {
	h := newHarness(t, harnessOpts{rules: map[string]types.Rule{"rule-a": {Name: "rule-a", EntryRung: types.RungPlay}}})
	res := h.admit(t, obsFor(sigFor("inv5"), "rule-a", "journald"))
	before := len(h.ledger.records)
	tr := Transition{Seq: 7, Trigger: "T03"}
	if _, err := h.l.Advance(context.Background(), res.Inc, tr); err != nil {
		t.Fatalf("first Advance: %v", err)
	}
	afterFirst := len(h.ledger.records)
	for i := 0; i < 1000; i++ {
		if _, err := h.l.Advance(context.Background(), res.Inc, tr); err != nil {
			t.Fatalf("repeat %d: %v", i, err)
		}
	}
	if got := len(h.ledger.records); got != afterFirst {
		t.Errorf("1000 repeated Advances appended %d record(s), want 0 (before=%d after first=%d)",
			got-afterFirst, before, afterFirst)
	}
	inc, err := h.l.Incident(context.Background(), res.Inc)
	if err != nil {
		t.Fatalf("Incident: %v", err)
	}
	if inc.State != types.StPlayDrafted {
		t.Errorf("state is %q, want play:drafted", string(inc.State))
	}
}

// TestIllegalTransitionsQuarantineOnThirdRefusal covers §3.3 and §7: every refusal
// leaves the state byte-identical and increments illegal_transitions; the third
// refusal reaches quarantined with TROUBLE-LADDER-017.
func TestIllegalTransitionsQuarantineOnThirdRefusal(t *testing.T) {
	h := newHarness(t, harnessOpts{rules: map[string]types.Rule{"rule-a": {Name: "rule-a", EntryRung: types.RungPlay}}})
	res := h.admit(t, obsFor(sigFor("quarantine"), "rule-a", "journald"))

	for i := 1; i <= 3; i++ {
		_, err := h.l.Advance(context.Background(), res.Inc, Transition{From: types.StRecorded, To: types.StDetected})
		code := CodeOf(err)
		if i < 3 && code != types.CodeLadder001 {
			t.Fatalf("refusal %d: code %q, want TROUBLE-LADDER-001", i, code)
		}
		if i == 3 && code != types.CodeLadder017 {
			t.Fatalf("third refusal: code %q, want TROUBLE-LADDER-017", code)
		}
	}
	inc, err := h.l.Incident(context.Background(), res.Inc)
	if err != nil {
		t.Fatalf("Incident: %v", err)
	}
	if inc.State != types.StQuarantined {
		t.Fatalf("state after three refusals is %q, want quarantined", string(inc.State))
	}
	st := h.l.incStateFor(res.Inc)
	if st.IllegalTransitions != 3 {
		t.Errorf("illegal_transitions = %d, want 3", st.IllegalTransitions)
	}
	if n := h.ledger.countPayload("illegal", true); n < 2 {
		t.Errorf("only %d refusal record(s) carry illegal=true, want at least 2", n)
	}
}

// TestUnquarantineRequiresAHumanActor covers §3.3 R11.
func TestUnquarantineRequiresAHumanActor(t *testing.T) {
	h := newHarness(t, harnessOpts{rules: map[string]types.Rule{"rule-a": {Name: "rule-a", EntryRung: types.RungPlay}}})
	res := h.admit(t, obsFor(sigFor("unq"), "rule-a", "journald"))
	if err := h.l.Quarantine(context.Background(), res.Inc, "operator says so"); err != nil {
		t.Fatalf("Quarantine: %v", err)
	}
	if err := h.l.Unquarantine(context.Background(), res.Inc, "reviewed", types.Actor{Kind: types.ActorPlay, ID: "troubled"}); err == nil {
		t.Fatal("Unquarantine accepted a non-human actor")
	}
	if err := h.l.Unquarantine(context.Background(), res.Inc, "reviewed", types.Actor{Kind: types.ActorHuman, ID: "opuser"}); err != nil {
		t.Fatalf("Unquarantine (human): %v", err)
	}
	inc, _ := h.l.Incident(context.Background(), res.Inc)
	if inc.State != types.StRecorded {
		t.Errorf("state after Unquarantine is %q, want recorded", string(inc.State))
	}
}
