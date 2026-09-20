package ladder

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/trouble-agent/trouble/internal/llm"
	"github.com/trouble-agent/trouble/internal/types"
)

// agent_test.go is SPEC-05 §7's battery for §2a/§3.7a/§3.12a: the agent stage runs
// through the LLM port and records what served it, and every refusal is a named
// code rather than an empty diagnosis.

// The port's contract is satisfied by the shipped client: a signature drift is a
// compile error here, not a runtime surprise in the daemon.
var _ AgentPort = (*llm.Client)(nil)

// fakeAgent is a scripted AgentPort.
type fakeAgent struct {
	mu      sync.Mutex
	calls   int
	prompts []string
	outcome types.AgentOutcome
	err     error
}

func (f *fakeAgent) RunAgent(ctx context.Context, prompt string) (types.AgentOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.prompts = append(f.prompts, prompt)
	return f.outcome, f.err
}

func (f *fakeAgent) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// serving is a successful outcome for candidate c1.
func serving() types.AgentOutcome {
	return types.AgentOutcome{
		Text:      "the queue wedges because the socket backlog fills; reload the unit",
		Candidate: "c1",
		Model:     "model-a",
		Endpoint:  "llm.example.invalid",
		Usage:     types.LLMUsage{PromptTokens: 120, CompletionTokens: 40, TotalTokens: 160},
		Compaction: types.LLMCompaction{
			Applied: true, Chunks: 2, InTokens: 41000, OutTokens: 3200,
			MaxChunks: 8, Candidate: "c2", Model: "model-b",
		},
		Attempts: []types.LLMAttempt{
			{Candidate: "c0", Model: "model-z", Status: 429, Class: llm.ClassRateLimited, Reason: llm.ReasonStatus, LatencyMS: 12},
			{Candidate: "c1", Model: "model-a", Status: 200, LatencyMS: 240},
		},
	}
}

// agentRunRecords returns the agent_run records the harness ledger holds.
func (h *harness) agentRunRecords() []ledgerRec { return h.ledger.byKind(types.KAgentRun) }

func TestRunAgentStage_RecordsTheServingCandidate(t *testing.T) {
	port := &fakeAgent{outcome: serving()}
	h := newHarness(t, harnessOpts{agent: port, rules: agentRule("rule-agent")})
	inc := agentRunning(t, h, "agent-serve")

	out, err := h.l.RunAgentStage(context.Background(), inc, "diagnose the wedge")
	if err != nil {
		t.Fatalf("RunAgentStage: %v", err)
	}
	if out.Candidate != "c1" || out.Text != serving().Text {
		t.Errorf("outcome = %+v, want the port's outcome returned unchanged", out)
	}
	if port.callCount() != 1 {
		t.Fatalf("port calls = %d, want exactly 1 (one buffered call per stage)", port.callCount())
	}
	if len(port.prompts) != 1 || port.prompts[0] != "diagnose the wedge" {
		t.Errorf("prompts = %v, want the caller's prompt verbatim", port.prompts)
	}

	recs := h.agentRunRecords()
	if len(recs) != 1 {
		t.Fatalf("agent_run records = %d, want exactly 1 per run", len(recs))
	}
	rec := recs[0]
	if rec.Sig == "" || rec.Inc != inc {
		t.Errorf("record sig/inc = %q/%q, want the incident's (%q)", rec.Sig, rec.Inc, inc)
	}
	// §3.12a: the stage record answers "which candidate served, at what cost, over
	// what context".
	for key, want := range map[string]any{
		"stage":             "agent",
		"outcome":           "done",
		"serving_candidate": "c1",
		"model":             "model-a",
		"endpoint":          "llm.example.invalid",
	} {
		if got := rec.Payload[key]; got != want {
			t.Errorf("payload[%q] = %v, want %v", key, got, want)
		}
	}
	usage, ok := rec.Payload["usage"].(types.LLMUsage)
	if !ok || usage.CompletionTokens != 40 {
		t.Errorf("payload[usage] = %#v, want the outcome's accounting", rec.Payload["usage"])
	}
	comp, ok := rec.Payload["compaction"].(types.LLMCompaction)
	if !ok || !comp.Applied || comp.InTokens != 41000 || comp.OutTokens != 3200 {
		t.Errorf("payload[compaction] = %#v, want the pass's in/out token counts", rec.Payload["compaction"])
	}
	attempts, ok := rec.Payload["attempts"].([]types.LLMAttempt)
	if !ok || len(attempts) != 2 || attempts[0].Class != llm.ClassRateLimited {
		t.Errorf("payload[attempts] = %#v, want the ordered failover evidence", rec.Payload["attempts"])
	}
	if rec.Payload["prompt_digest"] == "" {
		t.Error("payload[prompt_digest] is empty; SPEC-07 §3.7 joins on it")
	}
	if rec.Payload["error_code"] != "" || rec.Payload["failure_class"] != "" {
		t.Errorf("a serving run carries error_code=%v failure_class=%v, want both empty",
			rec.Payload["error_code"], rec.Payload["failure_class"])
	}
	if budget, ok := rec.Payload["budget"].(map[string]any); !ok || len(budget) == 0 {
		t.Errorf("payload[budget] = %#v, want the day's counters", rec.Payload["budget"])
	}
	// T27's guard is satisfied by evidence, not by a caller's assertion.
	if st := h.l.incStateFor(inc); st.AgentResult != "done" {
		t.Errorf("AgentResult = %q, want \"done\"", st.AgentResult)
	}
}

func TestRunAgentStage_FailedRunIsRecordedWithItsClass(t *testing.T) {
	failure := llm.ClassTransport
	port := &fakeAgent{
		outcome: types.AgentOutcome{
			FailureClass: failure,
			Attempts: []types.LLMAttempt{
				{Candidate: "c1", Model: "model-a", Class: failure, Reason: llm.ReasonTransport},
			},
		},
		err: &llm.Error{Class: failure, Candidate: "c1", Reason: llm.ReasonTransport},
	}
	h := newHarness(t, harnessOpts{agent: port, rules: agentRule("rule-agent")})
	inc := agentRunning(t, h, "agent-fail")

	out, err := h.l.RunAgentStage(context.Background(), inc, "diagnose")
	if err == nil {
		t.Fatal("RunAgentStage returned no error for a failed run")
	}
	if got := CodeOf(err); got != types.CodeLadder021 {
		t.Errorf("code = %q, want TROUBLE-LADDER-021 (err=%v)", got, err)
	}
	if out.FailureClass != failure {
		t.Errorf("outcome.FailureClass = %q, want %q", out.FailureClass, failure)
	}
	recs := h.agentRunRecords()
	if len(recs) != 1 {
		t.Fatalf("agent_run records = %d, want exactly 1 (a failure is recorded too)", len(recs))
	}
	rec := recs[0]
	if rec.Payload["outcome"] != "failed" {
		t.Errorf("payload[outcome] = %v, want \"failed\"", rec.Payload["outcome"])
	}
	if rec.Payload["serving_candidate"] != "" {
		t.Errorf("payload[serving_candidate] = %v, want empty: a failed run never claims a candidate", rec.Payload["serving_candidate"])
	}
	if rec.Payload["failure_class"] != failure {
		t.Errorf("payload[failure_class] = %v, want %q", rec.Payload["failure_class"], failure)
	}
	if rec.Payload["error_code"] != string(types.CodeLadder021) {
		t.Errorf("payload[error_code] = %v, want the mirrored TROUBLE-LADDER-021", rec.Payload["error_code"])
	}
	if rec.Payload["reason"] != llm.ReasonTransport {
		t.Errorf("payload[reason] = %v, want the port's reason token %q", rec.Payload["reason"], llm.ReasonTransport)
	}
	if st := h.l.incStateFor(inc); st.AgentResult != failure {
		t.Errorf("AgentResult = %q, want the failure class so T28's guard sees evidence", st.AgentResult)
	}
	// A failure is still a run: T28 can fire, so the incident is not stuck.
	port.outcome = serving()
	port.err = nil
	if _, err := h.l.Advance(context.Background(), inc, Transition{Trigger: "T28"}); err != nil {
		t.Fatalf("T28 after a failed run: %v", err)
	}
}

func TestRunAgentStage_NoPortWiredRefusesAndRecordsNoRun(t *testing.T) {
	h := newHarness(t, harnessOpts{rules: agentRule("rule-agent")})
	inc := agentRunning(t, h, "agent-noport")

	_, err := h.l.RunAgentStage(context.Background(), inc, "diagnose")
	if err == nil {
		t.Fatal("RunAgentStage succeeded with no port wired")
	}
	if got := CodeOf(err); got != types.CodeLadder021 {
		t.Errorf("code = %q, want TROUBLE-LADDER-021", got)
	}
	if !strings.Contains(err.Error(), "no_agent_port") {
		t.Errorf("error = %v, want the reason token no_agent_port", err)
	}
	if recs := h.agentRunRecords(); len(recs) != 0 {
		t.Errorf("agent_run records = %d, want 0: a stage that cannot call a model must not look like one that did", len(recs))
	}
	// The refusal itself is recorded on the incident, so the reason is auditable.
	if p := h.incidentPayload(); p == nil || p["reason"] != "no_agent_port" {
		t.Errorf("incident payload = %#v, want a refused row naming no_agent_port", p)
	}
	if st := h.l.incStateFor(inc); st.AgentResult != "" {
		t.Errorf("AgentResult = %q, want empty: no run happened", st.AgentResult)
	}
}

func TestRunAgentStage_KillSwitchRefusesAndRecordsPending(t *testing.T) {
	port := &fakeAgent{outcome: serving()}
	h := newHarness(t, harnessOpts{agent: port, rules: agentRule("rule-agent")})
	inc := agentRunning(t, h, "agent-kill")
	if _, err := h.l.SetKillSwitch(context.Background(), true, types.Actor{Kind: types.ActorHuman, ID: "op"}); err != nil {
		t.Fatalf("SetKillSwitch: %v", err)
	}

	_, err := h.l.RunAgentStage(context.Background(), inc, "diagnose")
	if got := CodeOf(err); got != types.CodeLadder010 {
		t.Fatalf("code = %q, want TROUBLE-LADDER-010 (err=%v)", got, err)
	}
	if port.callCount() != 0 {
		t.Errorf("port calls = %d, want 0: a refused stage entry runs nothing", port.callCount())
	}
	p := h.incidentPayload()
	if p["refused"] != true || p["pending"] != true {
		t.Errorf("refusal payload = %#v, want refused+pending", p)
	}
	if p["resume_from"] != string(types.StAgentRunning) {
		t.Errorf("resume_from = %v, want %s", p["resume_from"], types.StAgentRunning)
	}
	if recs := h.agentRunRecords(); len(recs) != 0 {
		t.Errorf("agent_run records = %d, want 0", len(recs))
	}
}

func TestRunAgentStage_DayBudgetExhaustionRefusesBeforeThePort(t *testing.T) {
	port := &fakeAgent{outcome: serving()}
	cfg := DefaultConfig()
	cfg.AgentRunsPerDay = 1
	h := newHarness(t, harnessOpts{cfg: cfg, agent: port, rules: agentRule("rule-agent")})
	// T05 charges the one available agent run, so the stage is already at its cap.
	inc := agentRunning(t, h, "agent-budget")

	_, err := h.l.RunAgentStage(context.Background(), inc, "diagnose")
	if got := CodeOf(err); got != types.CodeLadder013 {
		t.Fatalf("code = %q, want TROUBLE-LADDER-013 (err=%v)", got, err)
	}
	if port.callCount() != 0 {
		t.Errorf("port calls = %d, want 0: exhaustion escalates instead of running (§3.7)", port.callCount())
	}
	p := h.incidentPayload()
	if p["reason"] != reasonBudgetExhausted {
		t.Errorf("incident payload reason = %v, want %q", p["reason"], reasonBudgetExhausted)
	}
	if recs := h.agentRunRecords(); len(recs) != 0 {
		t.Errorf("agent_run records = %d, want 0", len(recs))
	}
	// The day counter is unmoved: a refused stage entry charges nothing.
	if state, err := h.l.Budget(context.Background()); err != nil {
		t.Fatalf("Budget: %v", err)
	} else if used := state.Used[types.BudgetAgentRuns]; used != 1 {
		t.Errorf("agent_runs used = %d, want 1 (the entry charge only)", used)
	}
}

func TestRunAgentStage_RefusesWhenTheIncidentIsNotRunning(t *testing.T) {
	port := &fakeAgent{outcome: serving()}
	h := newHarness(t, harnessOpts{agent: port, rules: agentRule("rule-agent")})
	inc, _ := playChecked(t, h, "agent-wrongstate")

	_, err := h.l.RunAgentStage(context.Background(), inc, "diagnose")
	if got := CodeOf(err); got != types.CodeLadder001 {
		t.Fatalf("code = %q, want TROUBLE-LADDER-001 (err=%v)", got, err)
	}
	if port.callCount() != 0 {
		t.Errorf("port calls = %d, want 0", port.callCount())
	}
	if _, err := h.l.RunAgentStage(context.Background(), "inc_missing", "x"); CodeOf(err) != types.CodeLadder012 {
		t.Errorf("unknown incident code = %q, want TROUBLE-LADDER-012", CodeOf(err))
	}
}

func TestRunAgentStage_LedgerFailureLeavesNoCompletedRun(t *testing.T) {
	port := &fakeAgent{outcome: serving()}
	h := newHarness(t, harnessOpts{agent: port, rules: agentRule("rule-agent")})
	inc := agentRunning(t, h, "agent-ledgerfail")
	h.ledger.mu.Lock()
	h.ledger.failNext = errors.New("ledger closed")
	h.ledger.mu.Unlock()

	_, err := h.l.RunAgentStage(context.Background(), inc, "diagnose")
	if err == nil {
		t.Fatal("RunAgentStage reported a completed run whose record could not be written")
	}
	if got := CodeOf(err); got != types.CodeLadder021 {
		t.Errorf("code = %q, want TROUBLE-LADDER-021", got)
	}
	if st := h.l.incStateFor(inc); st.AgentResult != "" {
		t.Errorf("AgentResult = %q, want empty: an unrecorded run is not a completed run (INV-2)", st.AgentResult)
	}
	if _, err := h.l.Advance(context.Background(), inc, Transition{Trigger: "T27"}); err == nil {
		t.Error("T27 advanced for a run that was never recorded")
	}
}
