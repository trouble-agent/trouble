package ladder

import (
	"context"
	"sort"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// The transition table is DATA (SPEC-05 §7): 48 legal edges (T01–T48) and 13
// refused edges (R01–R13), asserted cell-by-cell. A count guard in the tests
// asserts len(legal)==48 && len(refused)==13 so a table edit cannot silently drop
// an edge.

// commonKeys are emitted by every legal edge (§3.2 lists them per row).
var commonKeys = []string{"transition", "from", "to"}

// edge is one legal transition (§3.2).
type edge struct {
	ID      string
	From    []types.LadderState
	To      types.LadderState
	Trigger string
	// Stage marks a stage-entry edge: the kill-switch is evaluated against it
	// first (§3.4).
	Stage bool
	// Emitted lists the payload keys this edge must carry beyond the common set.
	Emitted []string
	// Code is the error the edge records ("" when it records none).
	Code types.ErrorCode
	// Guard returns nil when the edge is legal in the current state; a non-nil
	// return refuses the transition without mutating state.
	Guard func(l *Ladder, st *incState, tr Transition) *Error
	// Apply performs the edge's stage effects and mutates the incident state.
	Apply func(l *Ladder, st *incState, tr Transition) *Error
}

func anyOf(states ...types.LadderState) []types.LadderState { return states }

// legal is the complete legal transition table (§3.2).
var legal = []edge{
	{
		ID: "T01", From: anyOf(types.StDetected), To: types.StRecorded, Trigger: "observation_admitted",
		Emitted: []string{"transition", "from", "to", "entry_rung", "rung", "rule", "inKey", "sigs", "arrival_paths", "severity", "stabilize_degraded"},
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			if st.IllegalTransitions >= 3 {
				return newErr(types.CodeLadder001, "", "admission refused: %d illegal transitions", st.IllegalTransitions)
			}
			return nil
		},
		Apply: func(l *Ladder, st *incState, tr Transition) *Error { return nil },
	},
	{
		ID: "T02", From: anyOf(types.StDetected), To: types.StSuppressed, Trigger: "suppression_open_at_commit",
		Emitted: []string{"transition", "suppress_until", "suppressed_count"},
		Code:    types.CodeLadder018,
		Apply: func(l *Ladder, st *incState, tr Transition) *Error {
			st.SuppressUntil = l.suppress[st.Inc.Sig].Until
			return nil
		},
	},
	{
		ID: "T03", From: anyOf(types.StRecorded), To: types.StPlayDrafted, Trigger: "rung_advance_play",
		Stage:   true,
		Emitted: []string{"transition", "rung", "play_runs"},
		Guard:   guardRungAtLeast(types.RungPlay),
		Apply: func(l *Ladder, st *incState, tr Transition) *Error {
			if l.deps.PlayFor != nil {
				if p, ok := l.deps.PlayFor(st.Rule); ok {
					cp := p
					st.Play = &cp
				}
			}
			st.Inc.Rung = types.RungPlay
			l.chargeLocked(types.BudgetPlayRuns, l.now())
			return nil
		},
	},
	{
		ID: "T04", From: anyOf(types.StRecorded), To: types.StResRequested, Trigger: "rung_advance_research",
		Stage:   true,
		Emitted: []string{"transition", "rung", "research_id"},
		Code:    types.CodeLadder009,
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			if err := guardRungAtLeast(types.RungResearch)(l, st, tr); err != nil {
				return err
			}
			if l.deps.Research == nil {
				return newErr(types.CodeLadder009, reasonCeiling, "research rung requested with no ResearchPort wired")
			}
			if !l.budgetLeftLocked(types.BudgetResearchReqs) {
				return newErr(types.CodeLadder009, reasonBudgetExhausted, "research budget exhausted")
			}
			return nil
		},
		Apply: func(l *Ladder, st *incState, tr Transition) *Error {
			out, err := l.deps.Research.Request(context.Background(), st.Inc, researchSubject(st))
			if err != nil {
				st.ResearchID = ""
				return wrapErr(types.CodeLadder009, reasonCeiling, err)
			}
			st.ResearchID = out.ID
			st.Inc.ResearchID = out.ID
			st.Outcome = &out
			l.chargeLocked(types.BudgetResearchReqs, l.now())
			st.Inc.Rung = types.RungResearch
			return nil
		},
	},
	{
		ID: "T05", From: anyOf(types.StRecorded), To: types.StAgentRunning, Trigger: "rung_advance_agent",
		Stage:   true,
		Emitted: []string{"transition", "rung", "lease"},
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			if err := guardRungAtLeast(types.RungAgent)(l, st, tr); err != nil {
				return err
			}
			return guardAgentEntry(l, st)
		},
		Apply: agentStage,
	},
	{
		ID: "T06", From: anyOf(types.StRecorded), To: types.StVerifying, Trigger: "outlets_complete",
		Emitted: []string{"transition", "rung", "window"},
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			if st.Ceiling != types.RungRecord {
				return newErr(types.CodeLadder001, "", "recorded → verifying is legal only when the ceiling is `record`")
			}
			return nil
		},
		Apply: func(l *Ladder, st *incState, tr Transition) *Error {
			st.Inc.Rung = types.RungOutlets
			return outletsThenWindow(l, st, tr)
		},
	},
	{
		ID: "T07", From: anyOf(types.StRecorded), To: types.StEscalated, Trigger: "budget_exhausted_before_work",
		Emitted: []string{"transition", "pending_human", "budget"},
		Code:    types.CodeLadder013,
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			if l.budgetLeftLocked(types.BudgetPlayRuns) && l.budgetLeftLocked(types.BudgetAgentRuns) {
				return newErr(types.CodeLadder013, reasonBudgetExhausted, "no budget is exhausted: T07 is not the legal edge")
			}
			return nil
		},
		Apply: escalateStage,
	},
	{
		ID: "T08", From: anyOf(types.StPlayDrafted), To: types.StPlayCheck, Trigger: "run_started_check",
		Stage:   true,
		Emitted: []string{"transition", "rung", "task", "tool_call"},
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			if l.gate.KillSwitch {
				return nil
			}
			if st.Play != nil && st.Play.CheckMode == false && st.Ceiling != types.RungPlay {
				return newErr(types.CodeLadder011, reasonNoCheckMode, "play declares check_mode=false")
			}
			return nil
		},
		Apply: playCheckStage,
	},
	{
		ID: "T09", From: anyOf(types.StPlayDrafted), To: types.StPlayFailed, Trigger: "no_runnable_task",
		Emitted: []string{"transition", "reason", "module"},
		Code:    types.CodeLadder011,
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			if st.NoRunnable {
				return nil
			}
			return newErr(types.CodeLadder011, reasonNoCheckMode, "no runnable task under the current gate")
		},
		Apply: func(l *Ladder, st *incState, tr Transition) *Error { return nil },
	},
	{
		ID: "T10", From: anyOf(types.StPlayCheck), To: types.StPlayApplied, Trigger: "apply_decision",
		Stage:   true,
		Emitted: []string{"transition", "changed", "tool_call", "diff_summary"},
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			if st.RunSummary == nil {
				return newErr(types.CodeLadder011, "", "no run summary: the check has not run")
			}
			if st.RunSummary.Changed {
				return nil
			}
			// An empty diff is applied by definition (nothing to do).
			if st.RunSummary.DiffSummary == "" && !st.RunSummary.Changed && st.RunSummary.TasksRun == 0 {
				return nil
			}
			return nil
		},
		Apply: func(l *Ladder, st *incState, tr Transition) *Error {
			if st.RunSummary != nil && st.RunSummary.Changed {
				l.chargeLocked(types.BudgetPlayRuns, l.now())
			}
			return nil
		},
	},
	{
		ID: "T11", From: anyOf(types.StPlayCheck), To: types.StPlayFailed, Trigger: "check_failed",
		Emitted: []string{"transition", "class", "tool_call"},
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			if st.RunSummary != nil && (st.RunSummary.FailClass == "transient" && st.PlayRuns < st.EffectiveMaxRuns) {
				return newErr(types.CodeLadder002, "", "a transient failure with retries left retries the play (T13), it does not fail it")
			}
			return nil
		},
		Apply: func(l *Ladder, st *incState, tr Transition) *Error { return nil },
	},
	{
		ID: "T12", From: anyOf(types.StPlayApplied), To: types.StVerifying, Trigger: "applied_run_complete",
		Emitted: []string{"transition", "window", "play_runs"},
		Apply: func(l *Ladder, st *incState, tr Transition) *Error {
			st.PlayRuns++
			st.Inc.PlayRuns = st.PlayRuns
			st.Inc.Rung = types.RungOutlets
			return outletsThenWindow(l, st, tr)
		},
	},
	{
		ID: "T13", From: anyOf(types.StPlayFailed), To: types.StPlayDrafted, Trigger: "retry_play",
		Stage:   true,
		Emitted: []string{"transition", "rung", "play_runs"},
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			if st.PlayRuns >= st.EffectiveMaxRuns {
				return newErr(types.CodeLadder002, reasonCeiling, "play runs are spent (%d of %d): the rung advances", st.PlayRuns, st.EffectiveMaxRuns)
			}
			if st.RunSummary != nil && st.RunSummary.FailClass != "" && st.RunSummary.FailClass != "transient" {
				return newErr(types.CodeLadder002, reasonCeiling, "failure class %s is never retried", st.RunSummary.FailClass)
			}
			return nil
		},
		Apply: func(l *Ladder, st *incState, tr Transition) *Error {
			st.PlayRuns++
			st.Inc.PlayRuns = st.PlayRuns
			return nil
		},
	},
	{
		ID: "T14", From: anyOf(types.StPlayFailed), To: types.StResRequested, Trigger: "rung_advance_research",
		Stage:   true,
		Emitted: []string{"transition", "rung", "research_id"},
		Code:    types.CodeLadder002,
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			if st.PlayRuns < st.EffectiveMaxRuns && st.RunSummary != nil && st.RunSummary.FailClass == "transient" {
				return newErr(types.CodeLadder002, reasonCeiling, "play runs remain: retry (T13) instead of advancing")
			}
			if st.Ceiling == types.RungPlay {
				return newErr(types.CodeLadder002, reasonCeiling, "the ceiling is play: the rung cannot advance to research")
			}
			if l.deps.Research == nil {
				return newErr(types.CodeLadder002, reasonCeiling, "no ResearchPort wired")
			}
			return nil
		},
		Apply: func(l *Ladder, st *incState, tr Transition) *Error {
			out, err := l.deps.Research.Request(context.Background(), st.Inc, researchSubject(st))
			if err != nil {
				return wrapErr(types.CodeLadder009, reasonCeiling, err)
			}
			st.ResearchID = out.ID
			st.Inc.ResearchID = out.ID
			st.Outcome = &out
			st.Inc.Rung = types.RungResearch
			return nil
		},
	},
	{
		ID: "T15", From: anyOf(types.StPlayFailed), To: types.StResSkipped, Trigger: "research_unavailable",
		Emitted: []string{"transition", "rung", "skipped"},
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			if l.deps.Research != nil {
				return newErr(types.CodeLadder002, "", "a research driver is wired: T14 applies, not T15")
			}
			return nil
		},
		Apply: func(l *Ladder, st *incState, tr Transition) *Error {
			st.Inc.Rung = types.RungResearch
			st.Skipped = true
			return nil
		},
	},
	{
		ID: "T16", From: anyOf(types.StPlayFailed), To: types.StAgentRunning, Trigger: "rung_advance_agent",
		Stage:   true,
		Emitted: []string{"transition", "rung", "lease"},
		Code:    types.CodeLadder002,
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			if st.Ceiling == types.RungPlay {
				return newErr(types.CodeLadder002, reasonCeiling, "the ceiling is play: the agent rung is out of reach")
			}
			return guardAgentEntry(l, st)
		},
		Apply: agentStage,
	},
	{
		ID: "T17", From: anyOf(types.StPlayFailed), To: types.StQuarantined, Trigger: "rollback_failed_twice",
		Emitted: []string{"transition", "quarantine_reason", "worktree"},
		Code:    types.CodeLadder017,
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			if st.RollbackFailures < 2 {
				return newErr(types.CodeLadder008, reasonRollbackFailed, "only %d rollback failure(s) in 24h", st.RollbackFailures)
			}
			return nil
		},
		Apply: func(l *Ladder, st *incState, tr Transition) *Error {
			st.QuarantineReason = reasonRollbackFailed
			return nil
		},
	},
	{
		ID: "T18", From: anyOf(types.StPlayFailed), To: types.StEscalated, Trigger: "ceiling_reached_nothing_applied",
		Emitted: []string{"transition", "pending_human", "play_runs"},
		Code:    types.CodeLadder002,
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			if st.Ceiling != types.RungPlay {
				return newErr(types.CodeLadder002, reasonCeiling, "the ceiling is %s: escalate through the rung path instead", string(st.Ceiling))
			}
			return nil
		},
		Apply: escalateStage,
	},
	{
		ID: "T19", From: anyOf(types.StResRequested), To: types.StResReturned, Trigger: "brief_validated",
		Emitted: []string{"transition", "rung", "research_id", "brief_ref"},
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			if st.Outcome == nil {
				return newErr(types.CodeLadder009, "", "no research outcome: the request has not returned")
			}
			if len(st.Outcome.Brief) == 0 {
				return newErr(types.CodeLadder009, "", "the brief is empty")
			}
			return nil
		},
		Apply: func(l *Ladder, st *incState, tr Transition) *Error { return nil },
	},
	{
		ID: "T20", From: anyOf(types.StResRequested), To: types.StResDegraded, Trigger: "research_degraded",
		Emitted: []string{"transition", "rung", "degraded_reason", "research_id"},
		Code:    types.CodeLadder009,
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			if st.Outcome != nil && st.Outcome.DegradedReason == "" && len(st.Outcome.Brief) > 0 {
				return newErr(types.CodeLadder009, "", "the brief is valid: T19 applies, not T20")
			}
			return nil
		},
		Apply: func(l *Ladder, st *incState, tr Transition) *Error { return nil },
	},
	{
		ID: "T21", From: anyOf(types.StResRequested), To: types.StResSkipped, Trigger: "research_skipped",
		Emitted: []string{"transition", "rung", "skipped"},
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			if l.deps.Research != nil {
				return newErr(types.CodeLadder009, "", "the research driver is enabled: T21 does not apply")
			}
			return nil
		},
		Apply: func(l *Ladder, st *incState, tr Transition) *Error {
			st.Skipped = true
			return nil
		},
	},
	{
		ID: "T22", From: anyOf(types.StResReturned), To: types.StPlayDrafted, Trigger: "research_short_circuit",
		Emitted: []string{"transition", "rung", "research_play", "play_runs"},
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			if st.Outcome == nil || st.Outcome.Brief == nil {
				return newErr(types.CodeLadder002, "", "no brief to draft a play from")
			}
			if !briefHasPlay(st.Outcome.Brief) {
				return newErr(types.CodeLadder002, "", "the brief carries no executable play draft")
			}
			if st.PlayRuns >= st.EffectiveMaxRuns+l.cfg.ResearchPlayExtra {
				return newErr(types.CodeLadder002, reasonCeiling, "play runs (with the research allowance) are spent")
			}
			if st.Ceiling == types.RungRecord {
				return newErr(types.CodeLadder002, reasonCeiling, "the ceiling is record")
			}
			return nil
		},
		Apply: func(l *Ladder, st *incState, tr Transition) *Error {
			st.ResearchPlays++
			st.Inc.Rung = types.RungPlay
			return nil
		},
	},
	{
		ID: "T23", From: anyOf(types.StResReturned), To: types.StAgentRunning, Trigger: "brief_without_play",
		Stage:   true,
		Emitted: []string{"transition", "rung", "brief_ref", "lease"},
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			if st.Outcome != nil && briefHasPlay(st.Outcome.Brief) {
				return newErr(types.CodeLadder002, "", "the brief carries a play draft: T22 applies")
			}
			return guardAgentEntry(l, st)
		},
		Apply: agentStage,
	},
	{
		ID: "T24", From: anyOf(types.StResDegraded), To: types.StAgentRunning, Trigger: "proceed_degraded",
		Stage:   true,
		Emitted: []string{"transition", "rung", "degraded", "lease"},
		Guard:   func(l *Ladder, st *incState, tr Transition) *Error { return guardAgentEntry(l, st) },
		Apply:   agentStage,
	},
	{
		ID: "T25", From: anyOf(types.StResSkipped), To: types.StAgentRunning, Trigger: "proceed_without_research",
		Stage:   true,
		Emitted: []string{"transition", "rung", "lease"},
		Guard:   func(l *Ladder, st *incState, tr Transition) *Error { return guardAgentEntry(l, st) },
		Apply:   agentStage,
	},
	{
		ID: "T26", From: anyOf(types.StResReturned, types.StResDegraded, types.StResSkipped), To: types.StEscalated, Trigger: "research_ceiling_or_budget",
		Emitted: []string{"transition", "pending_human", "budget"},
		Code:    types.CodeLadder002,
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			if st.Ceiling == types.RungResearch {
				return nil
			}
			if !l.budgetLeftLocked(types.BudgetAgentRuns) {
				return nil
			}
			return newErr(types.CodeLadder002, reasonCeiling, "neither the research ceiling nor the agent budget blocks the agent rung")
		},
		Apply: escalateStage,
	},
	{
		ID: "T27", From: anyOf(types.StAgentRunning), To: types.StAgentDone, Trigger: "agent_run_finished",
		Emitted: []string{"transition", "rung", "agent_runs", "strikes"},
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			if st.AgentResult == "" {
				return newErr(types.CodeLadder013, "", "no run result: the agent run has not completed")
			}
			return nil
		},
		Apply: func(l *Ladder, st *incState, tr Transition) *Error {
			st.AgentRuns++
			st.Inc.AgentRuns = st.AgentRuns
			l.lease.release(st.LeaseID(), "completed")
			st.AgentResult = ""
			return nil
		},
	},
	{
		ID: "T28", From: anyOf(types.StAgentRunning), To: types.StAgentFailed, Trigger: "agent_run_failed",
		Emitted: []string{"transition", "rung", "strike", "failure_class"},
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			if st.AgentResult == "" {
				return newErr(types.CodeLadder013, "", "no run result: the agent run has not completed")
			}
			if st.AgentResult == "done" {
				return newErr(types.CodeLadder013, "", "the run finished: T27 applies, not T28")
			}
			return nil
		},
		Apply: func(l *Ladder, st *incState, tr Transition) *Error {
			st.Strikes = append(st.Strikes, l.now())
			st.AgentResult = ""
			l.lease.release(st.LeaseID(), "failed")
			return nil
		},
	},
	{
		ID: "T29", From: anyOf(types.StAgentRunning), To: types.StAgentSuspended, Trigger: "two_strikes",
		Emitted: []string{"transition", "rung", "suspend_until", "strikes"},
		Code:    types.CodeLadder004,
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			if l.strikesInWindow(st) < 2 {
				return newErr(types.CodeLadder004, "", "only %d strike(s) inside the window", l.strikesInWindow(st))
			}
			return nil
		},
		Apply: func(l *Ladder, st *incState, tr Transition) *Error {
			st.SuspendUntil = types.FormatUTC(l.now().Add(l.cfg.AgentStrikesWindow.Std()))
			l.lease.release(st.LeaseID(), "suspended")
			return nil
		},
	},
	{
		ID: "T30", From: anyOf(types.StAgentRunning), To: types.StEscalated, Trigger: "agent_tool_outside_allowlist",
		Emitted: []string{"transition", "pending_human", "reason"},
		Code:    types.CodeLadder013,
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			if st.OutOfScopeTool {
				return nil
			}
			if !l.budgetLeftLocked(types.BudgetAgentCost) {
				return nil
			}
			return newErr(types.CodeLadder013, "", "neither an out-of-allowlist tool nor the cost cap applies")
		},
		Apply: escalateStage,
	},
	{
		ID: "T31", From: anyOf(types.StAgentFailed), To: types.StAgentRunning, Trigger: "retry_agent",
		Stage:   true,
		Emitted: []string{"transition", "rung", "agent_runs"},
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			if l.strikesInWindow(st) >= 2 {
				return newErr(types.CodeLadder004, "", "two strikes inside the window: the agent rung is suspended (T32)")
			}
			return guardAgentEntry(l, st)
		},
		Apply: agentStage,
	},
	{
		ID: "T32", From: anyOf(types.StAgentFailed), To: types.StAgentSuspended, Trigger: "two_strikes_reached",
		Emitted: []string{"transition", "rung", "suspend_until", "strikes"},
		Code:    types.CodeLadder004,
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			if l.strikesInWindow(st) < 2 {
				return newErr(types.CodeLadder004, "", "only %d strike(s) inside the window", l.strikesInWindow(st))
			}
			return nil
		},
		Apply: func(l *Ladder, st *incState, tr Transition) *Error {
			st.SuspendUntil = types.FormatUTC(l.now().Add(l.cfg.AgentStrikesWindow.Std()))
			return nil
		},
	},
	{
		ID: "T33", From: anyOf(types.StAgentFailed), To: types.StEscalated, Trigger: "agent_budget_or_strikes",
		Emitted: []string{"transition", "pending_human", "strikes", "budget"},
		Code:    types.CodeLadder013,
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			if !l.budgetLeftLocked(types.BudgetAgentRuns) {
				return nil
			}
			if l.strikes7d(st) >= 4 {
				return nil
			}
			return newErr(types.CodeLadder013, reasonBudgetExhausted, "budget remains and strikes(7d)=%d < 4", l.strikes7d(st))
		},
		Apply: escalateStage,
	},
	{
		ID: "T34", From: anyOf(types.StAgentSuspended), To: types.StAgentRunning, Trigger: "suspension_elapsed",
		Stage:   true,
		Emitted: []string{"transition", "rung", "strike_reset"},
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			until, err := types.ParseUTC(st.SuspendUntil)
			if err != nil {
				return newErr(types.CodeLadder004, "", "suspension window %q does not parse", st.SuspendUntil)
			}
			if l.now().Before(until) {
				return newErr(types.CodeLadder004, "", "the suspension window has not elapsed")
			}
			// The sig must have stayed quiet for one full verify window.
			w := l.windowFor(st)
			ev, verr := l.verifyLocked(context.Background(), st, &Window{Start: w.Start, End: w.End, S: w.S}, canaryOpts{})
			if verr != nil {
				return verr
			}
			if ev.EventsObserved > 0 {
				return newErr(types.CodeLadder004, reasonRecurrence, "the sig recurred inside the suspension window")
			}
			if !ev.CanarySeen {
				return newErr(types.CodeLadder006, reasonCanaryMissing, "no canary landed: silence is not proof")
			}
			// The suspension elapsing resets the strikes (T34's own record says
			// strike_reset:true), so the agent-entry check runs without them.
			saved := st.Strikes
			st.Strikes = nil
			defer func() { st.Strikes = saved }()
			return guardAgentEntry(l, st)
		},
		Apply: func(l *Ladder, st *incState, tr Transition) *Error {
			st.Strikes = nil
			st.SuspendUntil = ""
			return agentStage(l, st, tr)
		},
	},
	{
		ID: "T35", From: anyOf(types.StAgentSuspended), To: types.StEscalated, Trigger: "third_failure_while_suspended",
		Emitted: []string{"transition", "pending_human", "strikes", "terminal"},
		Code:    types.CodeLadder004,
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			if l.strikesInWindow(st) >= 3 || st.StrikesWhileSuspended >= 3 {
				return nil
			}
			return newErr(types.CodeLadder004, "", "%d failure(s) inside the suspension window", st.StrikesWhileSuspended)
		},
		Apply: escalateStage,
	},
	{
		ID: "T36", From: anyOf(types.StAgentDone), To: types.StVerifying, Trigger: "agent_applied_something",
		Emitted: []string{"transition", "window", "agent_runs"},
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			if st.AgentMutations == 0 && !st.ModuleVerifyOK {
				return newErr(types.CodeLadder011, reasonDiagnosisOnly, "the run mutated nothing: T37 applies")
			}
			return nil
		},
		Apply: func(l *Ladder, st *incState, tr Transition) *Error {
			st.Inc.Rung = types.RungOutlets
			return outletsThenWindow(l, st, tr)
		},
	},
	{
		ID: "T37", From: anyOf(types.StAgentDone), To: types.StEscalated, Trigger: "diagnosis_only",
		Emitted: []string{"transition", "pending_human", "reason"},
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			if st.AgentMutations > 0 || st.ModuleVerifyOK {
				return newErr(types.CodeLadder011, "", "the run mutated something: T36 applies")
			}
			return nil
		},
		Apply: escalateStage,
	},
	{
		ID: "T38", From: anyOf(types.StVerifying), To: types.StResolved, Trigger: "window_passed",
		Emitted: []string{"transition", "state", "resolved_ts", "evidence_ref"},
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			ev := tr.Evidence
			if ev == nil {
				ev = st.Evidence
			}
			if ev == nil {
				return newErr(types.CodeLadder006, "", "no evidence tuple: verification is the only proof of resolution")
			}
			if ev.Result != types.VerifyPassed || !ev.CanarySeen {
				return newErr(types.CodeLadder006, reasonCanaryMissing, "evidence result %s (canary_seen=%t) is not a passing window", string(ev.Result), ev.CanarySeen)
			}
			return nil
		},
		Apply: func(l *Ladder, st *incState, tr Transition) *Error {
			st.Inc.ResolvedTS = types.FormatUTC(l.now())
			st.BreakerClosed = true
			return nil
		},
	},
	{
		ID: "T39", From: anyOf(types.StVerifying), To: types.StPlayDrafted, Trigger: "window_failed",
		Stage:   true,
		Emitted: []string{"transition", "rung", "verify_fail", "evidence_ref"},
		Code:    types.CodeLadder007,
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			ev := tr.Evidence
			if ev == nil {
				ev = st.Evidence
			}
			if ev == nil || ev.Result != types.VerifyFailed {
				return newErr(types.CodeLadder007, "", "the window did not fail: T38/T40 apply")
			}
			if st.PlayRuns >= st.EffectiveMaxRuns+l.cfg.VerifyReopens {
				return newErr(types.CodeLadder007, reasonCeiling, "play runs plus verify_reopens are spent")
			}
			return nil
		},
		Apply: func(l *Ladder, st *incState, tr Transition) *Error {
			st.Inc.Rung = types.RungPlay
			st.VerifyFailures++
			return nil
		},
	},
	{
		ID: "T40", From: anyOf(types.StVerifying), To: types.StEscalated, Trigger: "window_failed_or_invalid_twice",
		Emitted: []string{"transition", "pending_human", "evidence_ref", "invalid_count"},
		Code:    types.CodeLadder007,
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			ev := tr.Evidence
			if ev == nil {
				ev = st.Evidence
			}
			if ev == nil {
				return newErr(types.CodeLadder007, "", "no evidence tuple")
			}
			if ev.Result == types.VerifyFailed && st.PlayRuns >= st.EffectiveMaxRuns+l.cfg.VerifyReopens {
				return nil
			}
			if ev.Result == types.VerifyInvalid && st.InvalidCount >= 2 {
				return nil
			}
			return newErr(types.CodeLadder007, "", "retries or window restarts remain: T39/T41 apply")
		},
		Apply: escalateStage,
	},
	{
		ID: "T41", From: anyOf(types.StVerifying), To: types.StQuarantined, Trigger: "evaluation_impossible",
		Emitted: []string{"transition", "quarantine_reason"},
		Code:    types.CodeLadder005,
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			if st.WindowUnclampable {
				return nil
			}
			if st.SeqStalled {
				return nil
			}
			if st.SnapshotDisagreement {
				return nil
			}
			return newErr(types.CodeLadder005, reasonEvaluationBlocked, "the window can be evaluated")
		},
		Apply: func(l *Ladder, st *incState, tr Transition) *Error {
			st.QuarantineReason = reasonEvaluationBlocked
			return nil
		},
	},
	{
		ID: "T42", From: anyOf(types.StResolved), To: types.StRecorded, Trigger: "recurrence_after_resolve",
		Emitted: []string{"transition", "reopen", "reopen_count", "rung"},
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			if st.Inc.ReopenCount >= l.cfg.ReopenMax {
				return newErr(types.CodeLadder020, "", "the reopen cap (%d) is spent: a new incident opens", l.cfg.ReopenMax)
			}
			return nil
		},
		Apply: func(l *Ladder, st *incState, tr Transition) *Error {
			st.Inc.ReopenCount++
			st.Inc.ResolvedTS = ""
			return nil
		},
	},
	{
		ID: "T43", From: anyOf(types.StEscalated), To: types.StRecorded, Trigger: "recurrence_after_escalation",
		Emitted: []string{"transition", "reopen", "reopen_count"},
		Code:    types.CodeLadder013,
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			if !l.budgetLeftLocked(types.BudgetPlayRuns) {
				return newErr(types.CodeLadder013, reasonBudgetExhausted, "no budget for the next rung: the incident stays escalated")
			}
			return nil
		},
		Apply: func(l *Ladder, st *incState, tr Transition) *Error {
			st.Inc.ReopenCount++
			st.Inc.ResolvedTS = ""
			st.Waiting = false
			return nil
		},
	},
	{
		ID: "T44", From: anyOf(types.StSuppressed), To: types.StRecorded, Trigger: "suppression_window_closed",
		Emitted: []string{"transition", "suppressed_count", "rung"},
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			if st.SuppressedCount > 0 || !l.cfg.QuietClose {
				return nil
			}
			return newErr(types.CodeLadder018, "", "the window was quiet: quiet-close (T45) applies")
		},
		Apply: func(l *Ladder, st *incState, tr Transition) *Error {
			delete(l.suppress, st.Inc.Sig)
			return nil
		},
	},
	{
		ID: "T45", From: anyOf(types.StSuppressed), To: types.StResolved, Trigger: "quiet_close",
		Emitted: []string{"transition", "state", "quiet_close"},
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			if !l.cfg.QuietClose {
				return newErr(types.CodeLadder018, "", "quiet_close is disabled in config")
			}
			if st.SuppressedCount != 0 {
				return newErr(types.CodeLadder018, "", "the window collected %d event(s): it was not quiet", st.SuppressedCount)
			}
			ev := st.Evidence
			if ev == nil || !ev.CanarySeen || ev.Result != types.VerifyPassed {
				return newErr(types.CodeLadder006, reasonCanaryMissing, "quiet is only accepted when it was observed")
			}
			return nil
		},
		Apply: func(l *Ladder, st *incState, tr Transition) *Error {
			st.Inc.ResolvedTS = types.FormatUTC(l.now())
			delete(l.suppress, st.Inc.Sig)
			return nil
		},
	},
	{
		ID: "T46", From: anyOf(types.StDetected, types.StRecorded, types.StPlayDrafted, types.StPlayCheck, types.StPlayApplied, types.StPlayFailed,
			types.StResRequested, types.StResReturned, types.StResDegraded, types.StResSkipped, types.StAgentSuspended),
		To: types.StSuppressed, Trigger: "suppression_declared",
		Emitted: []string{"transition", "suppress_until", "suppress_reason"},
		Code:    types.CodeLadder018,
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			if _, _, ok := l.suppressionWindowLocked(st.Inc.Sig); !ok {
				return newErr(types.CodeLadder018, "", "no suppression window is open for %s", st.Inc.Sig)
			}
			return nil
		},
		Apply: func(l *Ladder, st *incState, tr Transition) *Error {
			sup := l.suppress[st.Inc.Sig]
			st.SuppressUntil = sup.Until
			st.Inc.State = types.StSuppressed
			return nil
		},
	},
	{
		ID: "T47", From: anyOf(types.StDetected, types.StRecorded, types.StPlayDrafted, types.StPlayCheck, types.StPlayApplied, types.StPlayFailed,
			types.StResRequested, types.StResReturned, types.StResDegraded, types.StResSkipped,
			types.StAgentRunning, types.StAgentDone, types.StAgentFailed, types.StAgentSuspended,
			types.StVerifying, types.StResolved, types.StEscalated, types.StSuppressed),
		To: types.StQuarantined, Trigger: "illegal_transitions_or_corrupt_snapshot",
		Emitted: []string{"transition", "quarantine_reason", "illegal_transitions"},
		Code:    types.CodeLadder017,
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			if st.IllegalTransitions >= 3 || st.SnapshotInvalid {
				return nil
			}
			return newErr(types.CodeLadder017, "", "only %d illegal transition(s)", st.IllegalTransitions)
		},
		Apply: func(l *Ladder, st *incState, tr Transition) *Error {
			if st.QuarantineReason == "" {
				st.QuarantineReason = reasonQuarantined
			}
			return nil
		},
	},
	{
		ID: "T48", From: anyOf(types.StDetected, types.StRecorded, types.StPlayDrafted, types.StPlayCheck, types.StPlayApplied, types.StPlayFailed,
			types.StResRequested, types.StResReturned, types.StResDegraded, types.StResSkipped,
			types.StAgentRunning, types.StAgentDone, types.StAgentFailed, types.StAgentSuspended, types.StVerifying),
		To: types.StRecorded, Trigger: "scope_breaker_open_fold",
		Emitted: []string{"transition", "folded", "breaker_scope", "suppressed_count"},
		Code:    types.CodeLadder014,
		Guard: func(l *Ladder, st *incState, tr Transition) *Error {
			if _, open := l.openBreakerLocked(st.Inc.Sig, Observation{Rule: st.Rule}); !open {
				return newErr(types.CodeLadder014, "", "no scope breaker is open for %s", st.Inc.Sig)
			}
			return nil
		},
		Apply: func(l *Ladder, st *incState, tr Transition) *Error {
			st.FoldCount++
			st.SuppressedCount++
			return nil
		},
	},
}

// refusal is one refused edge (§3.3). A refusal never mutates state.
type refusal struct {
	ID    string
	From  []types.LadderState
	To    []types.LadderState
	Why   string
	Wild  bool // matches by rule rather than by a state list
	Match func(from, to types.LadderState) bool
}

var refused = []refusal{
	{ID: "R01", To: anyOf(types.StDetected), Why: "detected is entry-only; re-observing a live incident is a merge",
		Match: func(from, to types.LadderState) bool { return to == types.StDetected }},
	{ID: "R02", From: anyOf(types.StDetected), To: anyOf(types.StPlayDrafted, types.StResRequested, types.StAgentRunning, types.StVerifying),
		Why: "recorded is the only successor of detected",
		Match: func(from, to types.LadderState) bool {
			switch to {
			case types.StPlayDrafted, types.StResRequested, types.StAgentRunning, types.StVerifying:
				return from == types.StDetected
			}
			return false
		}},
	{ID: "R03", From: anyOf(types.StRecorded), To: anyOf(types.StVerifying), Why: "the outlets-only rule (T06) is the single legitimate path",
		Match: func(from, to types.LadderState) bool {
			if from != types.StRecorded || to != types.StVerifying {
				return false
			}
			return true
		}},
	{ID: "R04", From: anyOf(types.StPlayDrafted), To: anyOf(types.StPlayApplied), Why: "play:check_only is mandatory for every mutating apply",
		Match: func(from, to types.LadderState) bool { return from == types.StPlayDrafted && to == types.StPlayApplied }},
	{ID: "R05", From: anyOf(types.StPlayCheck), To: anyOf(types.StVerifying), Why: "play:applied or play:failed is the only exit",
		Match: func(from, to types.LadderState) bool { return from == types.StPlayCheck && to == types.StVerifying }},
	{ID: "R06", From: anyOf(types.StPlayDrafted, types.StPlayCheck, types.StPlayApplied, types.StPlayFailed), To: anyOf(types.StPlayDrafted),
		Why:   "a retry past the cap must become a rung advance",
		Match: func(from, to types.LadderState) bool { return from == types.StPlayFailed && to == types.StPlayDrafted }},
	{ID: "R07", From: anyOf(types.StResRequested), To: anyOf(types.StPlayDrafted), Why: "the short-circuit exists only from research:returned",
		Match: func(from, to types.LadderState) bool {
			return from == types.StResRequested && to == types.StPlayDrafted
		}},
	{ID: "R08", From: anyOf(types.StAgentRunning, types.StAgentDone, types.StAgentFailed, types.StAgentSuspended),
		To:    anyOf(types.StPlayDrafted, types.StPlayCheck, types.StPlayApplied, types.StPlayFailed, types.StResRequested, types.StResReturned, types.StResDegraded, types.StResSkipped),
		Why:   "no backward rungs; the agent's fix leaves via agent:done → verifying",
		Match: func(from, to types.LadderState) bool { return isAgentState(from) && isPlayOrResearch(to) }},
	{ID: "R09", From: anyOf(types.StVerifying), To: anyOf(types.StDetected, types.StRecorded),
		Why: "an in-window recurrence is a verification failure, never a re-admission",
		Match: func(from, to types.LadderState) bool {
			return from == types.StVerifying && (to == types.StDetected || to == types.StRecorded)
		}},
	{ID: "R10", From: anyOf(types.StResolved, types.StEscalated), To: anyOf(types.StVerifying),
		Why: "a reopen re-enters at recorded", Wild: true,
		Match: func(from, to types.LadderState) bool {
			return (from == types.StResolved || from == types.StEscalated) && to == types.StVerifying
		}},
	{ID: "R11", From: anyOf(types.StQuarantined), To: anyOf(types.StDetected, types.StRecorded, types.StPlayDrafted, types.StPlayCheck, types.StPlayApplied, types.StPlayFailed,
		types.StResRequested, types.StResReturned, types.StResDegraded, types.StResSkipped, types.StAgentRunning, types.StAgentDone, types.StAgentFailed,
		types.StAgentSuspended, types.StVerifying, types.StResolved, types.StEscalated, types.StSuppressed),
		Why:   "quarantined is terminal",
		Match: func(from, to types.LadderState) bool { return from == types.StQuarantined }},
	{ID: "R12", From: anyOf(types.StSuppressed), To: anyOf(types.StVerifying), Why: "a suppressed incident collects no evidence window",
		Match: func(from, to types.LadderState) bool { return from == types.StSuppressed && to == types.StVerifying }},
	{ID: "R13", From: anyOf(), To: anyOf(), Why: "any stage entry while the kill-switch is set is refused and recorded as pending",
		Wild:  true,
		Match: func(from, to types.LadderState) bool { return false }},
}

func isAgentState(s types.LadderState) bool {
	switch s {
	case types.StAgentRunning, types.StAgentDone, types.StAgentFailed, types.StAgentSuspended:
		return true
	}
	return false
}

func isPlayOrResearch(s types.LadderState) bool {
	switch s {
	case types.StPlayDrafted, types.StPlayCheck, types.StPlayApplied, types.StPlayFailed,
		types.StResRequested, types.StResReturned, types.StResDegraded, types.StResSkipped:
		return true
	}
	return false
}

// LegalEdgeCount and RefusedEdgeCount are the count guards of §7.
func LegalEdgeCount() int   { return len(legal) }
func RefusedEdgeCount() int { return len(refused) }

// EdgeIDs lists the legal edge ids in table order (used by the tests' count guard
// and by the dashboard's static view).
func EdgeIDs() []string {
	out := make([]string, 0, len(legal))
	for _, e := range legal {
		out = append(out, e.ID)
	}
	return out
}

// RefusalIDs lists the refused edge ids in table order.
func RefusalIDs() []string {
	out := make([]string, 0, len(refused))
	for _, r := range refused {
		out = append(out, r.ID)
	}
	return out
}

// FindEdge resolves an edge by id.
func FindEdge(id string) (edge, bool) {
	for _, e := range legal {
		if e.ID == id {
			return e, true
		}
	}
	return edge{}, false
}

// ---------------------------------------------------------------------------
// Advance: the driving entry point.
// ---------------------------------------------------------------------------

// Advance applies one transition. It is idempotent per (inc, transition_seq): a
// repeated call with the same Transition.Seq is a no-op (INV-5). A refused
// transition never mutates state (INV-6).
func (l *Ladder) Advance(ctx context.Context, incID string, tr Transition) (types.Incident, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.incs[incID]
	if !ok {
		return types.Incident{}, newErr(types.CodeLadder012, "", "incident %s is not in the index", incID)
	}
	if tr.Seq != 0 && tr.Seq == st.LastSeq {
		return st.Inc, nil
	}

	e, err := l.resolveEdge(st, tr)
	if err != nil {
		return l.refuseLocked(ctx, st, tr, err)
	}

	// INV-3: the kill-switch is evaluated first at every stage entry.
	if e.Stage && l.gate.KillSwitch {
		ref := newErr(types.CodeLadder010, reasonKillSwitch, "stage entry %s refused while the kill-switch is set", e.ID)
		l.pending[incID] = PendingItem{Inc: incID, From: st.Inc.State, To: e.To, SinceTS: types.FormatUTC(l.now()), Reason: reasonKillSwitch}
		_ = l.appendIncident(ctx, st, map[string]any{
			"transition":  e.ID,
			"from":        string(st.Inc.State),
			"to":          string(st.Inc.State),
			"refused":     true,
			"pending":     true,
			"resume_from": string(st.Inc.State),
			"error_code":  string(types.CodeLadder010),
			"rung":        string(st.Inc.Rung),
			"sigs":        st.Sigs,
		})
		return st.Inc, ref
	}

	if e.Guard != nil {
		if gerr := e.Guard(l, st, tr); gerr != nil {
			return l.refuseLocked(ctx, st, tr, gerr)
		}
	}

	// The stage effect runs before the state is persisted, but the record is
	// appended BEFORE the in-memory index is updated (INV-2).
	if e.Apply != nil {
		if aerr := e.Apply(l, st, tr); aerr != nil {
			return l.refuseLocked(ctx, st, tr, aerr)
		}
	}

	from := st.Inc.State
	payload := l.edgePayload(st, e, tr, from)
	payload["transition"] = e.ID
	payload["from"] = string(from)
	payload["to"] = string(e.To)
	payload["rung"] = string(st.Inc.Rung)
	if e.Code != "" {
		payload["error_code"] = string(e.Code)
	}
	// Records are appended before the state moves (INV-2): the ledger is
	// authoritative, the index is a cache.
	if err := l.appendIncident(ctx, st, payload); err != nil {
		return st.Inc, wrapErr(types.CodeLadder015, "", err)
	}
	// The seq is consumed only by an ACCEPTED transition: a refused attempt must
	// stay retryable with the same seq (INV-5 dedups timers, not refusals).
	if tr.Seq != 0 {
		st.LastSeq = tr.Seq
	}
	st.Inc.State = e.To
	st.LastEdge = e.ID
	st.Inc.UpdatedTS = types.FormatUTC(l.now())
	st.LastTransitionTS = st.Inc.UpdatedTS
	st.Waiting = false
	switch e.To {
	case types.StResolved:
		l.observeBreakerClosuresLocked(ctx, st)
		delete(l.openBySig, st.Inc.Sig)
		if st.InKey != "" {
			delete(l.openByInKey, st.InKey)
		}
		if e.To != types.StQuarantined {
			l.openSuppressionAfterTerminal(ctx, st, e.To)
		}
	case types.StEscalated, types.StQuarantined:
		delete(l.openBySig, st.Inc.Sig)
		if st.InKey != "" {
			delete(l.openByInKey, st.InKey)
		}
		if e.To != types.StQuarantined {
			l.openSuppressionAfterTerminal(ctx, st, e.To)
		}
	}
	return st.Inc, nil
}

// resolveEdge maps a requested transition onto a table edge.
func (l *Ladder) resolveEdge(st *incState, tr Transition) (edge, *Error) {
	from := st.Inc.State
	if tr.From != "" {
		from = tr.From
	}
	if tr.Trigger != "" {
		e, ok := FindEdge(tr.Trigger)
		if !ok {
			return edge{}, newErr(types.CodeLadder001, "", "unknown transition %q", tr.Trigger)
		}
		if !statesContain(e.From, st.Inc.State) {
			return edge{}, newErr(types.CodeLadder001, "", "%s is not legal from %s", e.ID, string(st.Inc.State))
		}
		return e, nil
	}
	var matches []edge
	for _, e := range legal {
		if !statesContain(e.From, from) {
			continue
		}
		if tr.To != "" && e.To != tr.To {
			continue
		}
		if e.Guard != nil {
			if gerr := e.Guard(l, st, tr); gerr != nil {
				continue
			}
		}
		matches = append(matches, e)
	}
	if len(matches) == 0 {
		return edge{}, newErr(types.CodeLadder001, "", "no legal edge from %s to %q", string(from), string(tr.To))
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].ID < matches[j].ID })
	return matches[0], nil
}

// refuseLocked records a refused transition: the state is untouched and
// illegal_transitions increments; the third refusal quarantines (T47).
func (l *Ladder) refuseLocked(ctx context.Context, st *incState, tr Transition, cause *Error) (types.Incident, error) {
	isIllegal := cause.Code == types.CodeLadder001 || cause.Code == types.CodeLadder017
	if isIllegal {
		st.IllegalTransitions++
	}
	payload := map[string]any{
		"transition":          tr.Trigger,
		"from":                string(st.Inc.State),
		"to":                  string(tr.To),
		"refused":             true,
		"illegal":             isIllegal,
		"error_code":          string(cause.Code),
		"illegal_transitions": st.IllegalTransitions,
		"rung":                string(st.Inc.Rung),
		"sigs":                st.Sigs,
	}
	if cause.Reason != "" {
		payload["reason"] = cause.Reason
	}
	_ = l.appendIncident(ctx, st, payload)
	if isIllegal && st.IllegalTransitions >= 3 && st.Inc.State != types.StQuarantined {
		from := st.Inc.State
		st.Inc.State = types.StQuarantined
		st.QuarantineReason = reasonQuarantined
		_ = l.appendIncident(ctx, st, map[string]any{
			"transition":          "T47",
			"from":                string(from),
			"to":                  string(types.StQuarantined),
			"quarantine_reason":   reasonQuarantined,
			"illegal_transitions": st.IllegalTransitions,
			"error_code":          string(types.CodeLadder017),
			"sigs":                st.Sigs,
		})
		if l.deps.Notify != nil {
			_ = l.deps.Notify.Emit(ctx, st.Inc, "quarantined", map[string]any{"reason": reasonQuarantined})
		}
		return st.Inc, newErr(types.CodeLadder017, reasonQuarantined, "%d illegal transitions", st.IllegalTransitions)
	}
	return st.Inc, cause
}

// edgePayload assembles the payload keys the table declares for an edge.
func (l *Ladder) edgePayload(st *incState, e edge, tr Transition, from types.LadderState) map[string]any {
	p := map[string]any{}
	for _, k := range append(append([]string{}, commonKeys...), e.Emitted...) {
		switch k {
		case "transition", "from", "to", "rung":
			// filled by the caller
		case "entry_rung":
			p[k] = string(st.Inc.EntryRung)
		case "rule":
			p[k] = st.Rule
		case "inKey":
			p[k] = st.InKey
		case "sigs":
			p[k] = st.Sigs
		case "arrival_paths":
			p[k] = st.ArrivalPaths
		case "severity":
			p[k] = string(st.Inc.Severity)
		case "stabilize_degraded":
			p[k] = st.Stabilization.Degraded
		case "suppress_until":
			p[k] = st.SuppressUntil
		case "suppress_reason":
			p[k] = l.suppress[st.Inc.Sig].Reason
		case "suppressed_count":
			p[k] = st.SuppressedCount
		case "play_runs":
			p[k] = st.PlayRuns
		case "agent_runs":
			p[k] = st.AgentRuns
		case "strikes":
			p[k] = l.strikesInWindow(st)
		case "strike":
			p[k] = len(st.Strikes)
		case "strike_reset":
			p[k] = true
		case "failure_class":
			p[k] = st.FailureClass
		case "suspend_until":
			p[k] = st.SuspendUntil
		case "terminal":
			p[k] = true
		case "lease":
			p[k] = l.lease.payload(st.LeaseID())
		case "budget":
			p[k] = l.budgetPayload()
		case "research_id":
			p[k] = st.ResearchID
		case "brief_ref":
			p[k] = st.ResearchID
		case "degraded_reason":
			if st.Outcome != nil {
				p[k] = st.Outcome.DegradedReason
			} else {
				p[k] = ""
			}
		case "degraded":
			p[k] = st.Outcome != nil && st.Outcome.DegradedReason != ""
		case "skipped":
			p[k] = true
		case "research_play":
			p[k] = true
		case "window":
			p[k] = l.windowPayload(st.Window)
		case "task":
			p[k] = 0
		case "tool_call":
			if st.RunSummary != nil {
				p[k] = st.RunSummary.LastTool.ID
			} else {
				p[k] = ""
			}
		case "changed":
			p[k] = st.RunSummary != nil && st.RunSummary.Changed
		case "diff_summary":
			if st.RunSummary != nil {
				p[k] = st.RunSummary.DiffSummary
			} else {
				p[k] = ""
			}
		case "class":
			if st.RunSummary != nil {
				p[k] = st.RunSummary.FailClass
			} else {
				p[k] = ""
			}
		case "reason":
			if st.QuarantineReason != "" {
				p[k] = st.QuarantineReason
			} else {
				p[k] = e.Trigger
			}
		case "module":
			if st.RunSummary != nil {
				p[k] = st.RunSummary.LastTool.Module
			} else {
				p[k] = ""
			}
		case "pending_human":
			p[k] = true
		case "quarantine_reason":
			p[k] = st.QuarantineReason
		case "worktree":
			p[k] = st.Worktree
		case "state":
			p[k] = string(e.To)
		case "resolved_ts":
			p[k] = st.Inc.ResolvedTS
		case "evidence_ref":
			p[k] = st.Inc.ID
		case "verify_fail":
			p[k] = true
		case "invalid_count":
			p[k] = st.InvalidCount
		case "reopen":
			p[k] = true
		case "reopen_count":
			p[k] = st.Inc.ReopenCount
		case "quiet_close":
			p[k] = l.cfg.QuietClose
		case "folded":
			p[k] = true
		case "breaker_scope":
			p[k] = st.BreakerScope
		case "illegal_transitions":
			p[k] = st.IllegalTransitions
		default:
			if _, ok := p[k]; !ok {
				p[k] = nil
			}
		}
	}
	return p
}

func statesContain(list []types.LadderState, want types.LadderState) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// RefuseFor reports whether a (from, to) pair is a refused edge (SPEC-05 §3.3).
func RefuseFor(from, to types.LadderState) (string, bool) {
	for _, r := range refused {
		if r.Match != nil && r.Match(from, to) {
			return r.ID, true
		}
	}
	return "", false
}

// ---- guards and stage helpers ----

func guardRungAtLeast(rung types.Rung) func(*Ladder, *incState, Transition) *Error {
	return func(l *Ladder, st *incState, _ Transition) *Error {
		if rungRank(st.Ceiling) < rungRank(rung) {
			return newErr(types.CodeLadder002, reasonCeiling, "the ceiling is %s: %s is out of reach", string(st.Ceiling), string(rung))
		}
		scope := scopeSig + st.Inc.Sig
		if b, ok := l.breakers[scope]; ok && b.Breaker.State == types.BreakerOpen {
			return newErr(types.CodeLadder014, reasonSuppressionWindow, "the %s breaker is open", scope)
		}
		return nil
	}
}

func rungRank(r types.Rung) int {
	switch r {
	case types.RungRecord:
		return 0
	case types.RungPlay:
		return 1
	case types.RungResearch:
		return 2
	case types.RungAgent:
		return 3
	case types.RungOutlets:
		return 4
	}
	return 0
}

func guardAgentEntry(l *Ladder, st *incState) *Error {
	if !l.budgetLeftLocked(types.BudgetAgentRuns) {
		return newErr(types.CodeLadder013, reasonBudgetExhausted, "the agent-run budget is exhausted")
	}
	if l.strikesInWindow(st) >= 2 {
		return newErr(types.CodeLadder004, "", "two strikes inside the window")
	}
	lease, err := l.lease.acquire(l, st)
	if err != nil {
		return err
	}
	st.AcquiredLease = lease
	return nil
}

func agentStage(l *Ladder, st *incState, tr Transition) *Error {
	if st.AcquiredLease.LeaseID == "" {
		return newErr(types.CodeLadder003, reasonLeaseHeld, "no lease was acquired for the agent rung")
	}
	st.Inc.Rung = types.RungAgent
	st.Inc.LeaseID = st.AcquiredLease.LeaseID
	l.chargeLocked(types.BudgetAgentRuns, l.now())
	return nil
}

func outletsThenWindow(l *Ladder, st *incState, tr Transition) *Error {
	if l.deps.Outlets != nil {
		if id, err := l.deps.Outlets.EnsureIssue(context.Background(), st.Inc); err == nil && id != "" {
			st.IssueID = id
			st.Inc.IssueID = id
		}
		if id, err := l.deps.Outlets.EnsureBoardRow(context.Background(), st.Inc); err == nil && id != "" {
			st.TaskID = id
			st.Inc.TaskID = id
		}
	}
	w := l.windowFor(st)
	st.Window = w
	return nil
}

func escalateStage(l *Ladder, st *incState, tr Transition) *Error {
	if l.deps.Outlets != nil {
		if id, err := l.deps.Outlets.EnsureIssue(context.Background(), st.Inc); err == nil && id != "" {
			st.IssueID = id
			st.Inc.IssueID = id
		}
		if id, err := l.deps.Outlets.EnsureBoardRow(context.Background(), st.Inc); err == nil && id != "" {
			st.TaskID = id
			st.Inc.TaskID = id
		}
	}
	if l.deps.Notify != nil {
		_ = l.deps.Notify.Emit(context.Background(), st.Inc, "escalated", map[string]any{"reason": tr.Trigger})
	}
	return nil
}

func playCheckStage(l *Ladder, st *incState, tr Transition) *Error {
	if l.deps.Registry == nil || st.Play == nil {
		return nil
	}
	mode := types.ModeApply
	if !l.gate.AllowPlayMutate || l.gate.Mode == types.AutoShadow {
		mode = types.ModeCheck
	}
	sum, err := l.deps.Registry.Run(context.Background(), st.Inc, *st.Play, mode)
	if err != nil {
		st.RunSummary = &RunSummary{FailClass: string(ClassOf(err)), PlayRun: st.PlayRuns + 1}
		st.NoRunnable = true
		return nil
	}
	st.RunSummary = &sum
	if sum.TasksRun == 0 {
		st.NoRunnable = true
	}
	return nil
}

func briefHasPlay(brief map[string]any) bool {
	if brief == nil {
		return false
	}
	if v, ok := brief["play"].(map[string]any); ok && len(v) > 0 {
		return true
	}
	if v, ok := brief["play_draft"].(string); ok && v != "" {
		return true
	}
	return false
}

// openSuppressionAfterTerminal opens the flapping guard after resolved/escalated
// (§3.8 (a): a rule's Cooldown after resolved/escalated).
func (l *Ladder) openSuppressionAfterTerminal(ctx context.Context, st *incState, to types.LadderState) {
	cooldown := types.Duration("")
	if r, ok := l.rule(st.Rule); ok {
		cooldown = r.Cooldown
	}
	if cooldown == "" {
		return
	}
	scope := scopeSig + st.Inc.Sig
	b := l.breakerLocked(scope)
	l.openBreakerLockedWithReason(b, reasonSuppressionWindow, cooldown.Std())
	l.suppress[st.Inc.Sig] = suppression{Until: b.Breaker.OpenUntil, Reason: reasonSuppressionWindow, Opened: types.FormatUTC(l.now())}
	l.suppressionCycleOpenedLocked(ctx, st.Inc.Sig)
	_ = l.appendBreaker(ctx, b, st.Inc.ID)
}

// strikesInWindow counts agent failures inside the strikes window.
func (l *Ladder) strikesInWindow(st *incState) int {
	cutoff := l.now().Add(-l.cfg.AgentStrikesWindow.Std())
	n := 0
	for _, t := range st.Strikes {
		if t.After(cutoff) {
			n++
		}
	}
	return n
}

// strikes7d counts agent failures in the last seven days (T33).
func (l *Ladder) strikes7d(st *incState) int {
	cutoff := l.now().Add(-7 * 24 * time.Hour)
	n := 0
	for _, t := range st.Strikes {
		if t.After(cutoff) {
			n++
		}
	}
	return n
}

func (l *Ladder) budgetPayload() map[string]any {
	out := map[string]any{}
	day := dayKey(l.now())
	for _, class := range []string{types.BudgetAgentRuns, types.BudgetPlayRuns, types.BudgetResearchReqs, types.BudgetSpawns} {
		out[class] = map[string]any{
			"limit": l.limitFor(class),
			"used":  l.budget[budgetKey(l.hostID, day, class)],
		}
	}
	return out
}
