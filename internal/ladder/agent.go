package ladder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/trouble-agent/trouble/internal/types"
)

// AgentPort is the agent stage's only path to a model (SPEC-05 §2a). One buffered
// request/response per call; a stream is not expressible. It is implemented by
// internal/llm's Client and by this package's tests.
type AgentPort interface {
	// RunAgent performs one buffered completion. The outcome is returned WITH the
	// error: it is the ledger-facing report even when no candidate served.
	RunAgent(ctx context.Context, prompt string) (types.AgentOutcome, error)
}

// portFailure is the optional interface a port error implements to hand the ladder
// a stable class and reason token. The ladder records tokens, never prose, and
// never imports the client package.
type portFailure interface {
	FailureClass() string
	FailureReason() string
}

// reason tokens this file adds (the rest live in errors.go).
const (
	reasonNoAgentPort  = "no_agent_port"
	reasonRecordFailed = "record_write_failed"
	reasonBadStage     = "stage_not_running"
)

// RunAgentStage runs one agent-stage completion and records it (SPEC-05 §2a,
// §3.12a). It is the ladder's only caller of Deps.Agent and the only writer of an
// `agent_run` record.
//
// Gate order is the stage-entry order of §3.4/§3.11: the kill-switch refuses with
// TROUBLE-LADDER-010 and records the entry as pending; the autonomy gate refuses
// with TROUBLE-LADDER-011; the per-day agent budget refuses with
// TROUBLE-LADDER-013 before the port is reached. Only then does the run happen —
// with NO ladder lock held, because the call is a network round trip — and the
// outcome is recorded once, success or failure.
//
// The incident's `AgentResult` is set to the run's outcome (`done`, or the port's
// failure class) so T27's/T28's guards are satisfied by evidence: an agent run is
// finishable through the state machine without the caller re-deriving the result.
func (l *Ladder) RunAgentStage(ctx context.Context, incID, prompt string) (types.AgentOutcome, error) {
	l.mu.Lock()
	st := l.incs[incID]
	if st == nil {
		l.mu.Unlock()
		return types.AgentOutcome{}, newErr(types.CodeLadder012, "", "incident %s is not in the index", incID)
	}
	state := st.Inc.State
	if state != types.StAgentRunning {
		l.mu.Unlock()
		return types.AgentOutcome{}, newErr(types.CodeLadder001, reasonBadStage,
			"the agent stage runs for an incident in %s, not %s", types.StAgentRunning, state)
	}
	if l.gate.KillSwitch {
		err := l.appendIncident(ctx, st, map[string]any{
			"stage":       "agent",
			"refused":     true,
			"pending":     true,
			"resume_from": string(state),
			"reason":      reasonKillSwitch,
			"error_code":  string(types.CodeLadder010),
		})
		l.mu.Unlock()
		_ = err
		return types.AgentOutcome{}, newErr(types.CodeLadder010, reasonKillSwitch,
			"the kill-switch is set: the agent-stage entry is refused and recorded as pending")
	}
	if !l.gate.AllowAgent {
		_ = l.appendIncident(ctx, st, map[string]any{
			"stage":      "agent",
			"refused":    true,
			"pending":    false,
			"reason":     reasonGateDenied,
			"error_code": string(types.CodeLadder011),
		})
		mode := l.gate.Mode
		l.mu.Unlock()
		return types.AgentOutcome{}, newErr(types.CodeLadder011, reasonGateDenied,
			"the autonomy gate denies the agent stage in mode %q", mode)
	}
	if !l.budgetLeftLocked(types.BudgetAgentRuns) {
		budget := l.budgetPayload()
		_ = l.appendIncident(ctx, st, map[string]any{
			"stage":      "agent",
			"refused":    true,
			"pending":    false,
			"reason":     reasonBudgetExhausted,
			"budget":     budget,
			"error_code": string(types.CodeLadder013),
		})
		l.mu.Unlock()
		return types.AgentOutcome{}, newErr(types.CodeLadder013, reasonBudgetExhausted,
			"the per-day agent-run budget is exhausted: the stage is not entered")
	}
	port := l.deps.Agent
	// Everything the record needs is copied under the lock; the port call below
	// holds none of it.
	inc := st.Inc
	researchID := st.Inc.ResearchID
	if researchID == "" {
		researchID = st.ResearchID
	}
	briefDigest := briefDigestOf(st.Outcome)
	nextRun := st.AgentRuns + 1
	if port == nil {
		_ = l.appendIncident(ctx, st, map[string]any{
			"stage":      "agent",
			"refused":    true,
			"pending":    false,
			"reason":     reasonNoAgentPort,
			"error_code": string(types.CodeLadder021),
		})
		l.mu.Unlock()
		return types.AgentOutcome{}, newErr(types.CodeLadder021, reasonNoAgentPort,
			"no LLM port is wired for the agent stage: declare an [llm] table (SPEC-05 §4.3a)")
	}
	l.mu.Unlock()

	outcome, runErr := port.RunAgent(ctx, prompt)

	// One record per run, written before the stage returns (SPEC-05 §3.12a).
	failureClass := outcome.FailureClass
	if failureClass == "" && runErr != nil {
		failureClass = classOfPortError(runErr)
	}
	outcome.FailureClass = failureClass
	status := runStatusDone
	if failureClass != "" || runErr != nil {
		status = runStatusFailed
		if runErr != nil {
			// A failed run never claims a candidate: there is no "unknown" value.
			outcome.Candidate = ""
			outcome.Model = ""
			outcome.Endpoint = ""
		}
	}
	payload := map[string]any{
		"stage":             "agent",
		"outcome":           status,
		"serving_candidate": outcome.Candidate,
		"model":             outcome.Model,
		"endpoint":          outcome.Endpoint,
		"usage":             outcome.Usage,
		"compaction":        outcome.Compaction,
		"attempts":          outcome.Attempts,
		"failure_class":     failureClass,
		"error_code":        "",
		"reason":            "",
		"prompt_digest":     shortDigest([]byte(prompt)),
		"research_id":       researchID,
		"brief_digest":      briefDigest,
		"incident_state":    string(state),
		"agent_run_number":  nextRun,
	}
	var cause *Error
	if runErr != nil {
		reason := reasonOfPortError(runErr)
		if reason == "" {
			reason = failureClass
		}
		payload["error_code"] = string(types.CodeLadder021)
		payload["reason"] = reason
		cause = newErr(types.CodeLadder021, reason, "the agent-stage run failed: %s", failureClass)
		cause.Err = runErr
	}

	l.mu.Lock()
	payload["budget"] = l.budgetPayload()
	recorded := true
	if _, err := l.deps.Ledger.Append(ctx, types.KAgentRun, inc.Sig, inc.ID, payload); err != nil {
		// A run whose outcome cannot be recorded is not a completed run (INV-2):
		// the incident keeps no result, so no transition can treat it as finished.
		st.AgentResult = ""
		recorded = false
		l.mu.Unlock()
		if cause != nil {
			return outcome, cause
		}
		fail := newErr(types.CodeLadder021, reasonRecordFailed,
			"the agent run could not be recorded, so it is not a completed run")
		fail.Err = err
		return outcome, fail
	}
	if recorded {
		if status == runStatusDone {
			st.AgentResult = runStatusDone
		} else {
			st.AgentResult = failureClass
			if st.AgentResult == "" {
				st.AgentResult = runStatusFailed
			}
		}
	}
	l.mu.Unlock()

	if cause == nil {
		return outcome, nil
	}
	return outcome, cause
}

// run outcomes recorded in the agent_run payload and in AgentResult.
const (
	runStatusDone   = "done"
	runStatusFailed = "failed"
)

// classOfPortError reads the failure class off a port error ("" when it carries
// none).
func classOfPortError(err error) string {
	var pf portFailure
	if errors.As(err, &pf) {
		return pf.FailureClass()
	}
	return ""
}

// reasonOfPortError reads the stable reason token off a port error ("").
func reasonOfPortError(err error) string {
	var pf portFailure
	if errors.As(err, &pf) {
		return pf.FailureReason()
	}
	return ""
}

// shortDigest is the ledger's short digest form (SPEC-01 §3.2): hex(sha256)[:16].
func shortDigest(raw []byte) string {
	return types.DigestShort(types.SigDigest(raw))
}

// briefDigestOf is the research brief's ledger digest (SPEC-07 §3.9): the short
// digest of the brief as stored, encoded without HTML escaping so it matches the
// ledger's own canonical encoding.
func briefDigestOf(out *types.ResearchOutcome) string {
	if out == nil || len(out.Brief) == 0 {
		return ""
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out.Brief); err != nil {
		return ""
	}
	return shortDigest(buf.Bytes())
}
