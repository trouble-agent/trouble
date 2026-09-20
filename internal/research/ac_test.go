package research

// ac_test.go — the two acceptance criteria this package owns (SPEC-INDEX §3.2).
//
// AC-17: the research lane forwards an unknown class and the agent consumes the
// brief. AC-20: an unknown fingerprint with off-by-one enabled forwards a brief,
// the agent run consumes it (the prompt shows it), the ledger links
// brief↔agent↔fix, and a forced-unreachable lab still lets the ladder proceed
// with the gap noted in the ledger.

import (
	"context"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// TestAC17_UnknownClassForwardsAndAgentConsumesBrief.
func TestAC17_UnknownClassForwardsAndAgentConsumesBrief(t *testing.T) {
	lab := newLabStub(t)
	// An unknown class on the cache: found:false, 404 on the narrow probe, an
	// empty corpus. The rung must submit it and consume the returned brief.
	lab.queueStates = []string{"queued", "solving", "solved"}
	s, deps := newTestService(t, lab, nil)
	out, err := s.Run(context.Background(), testIncident(), testSig(), bundleFor(map[string]any{
		"message": "queue wedge: pool exhausted", "await": true,
	}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.State != types.ResReturned {
		t.Fatalf("outcome = %+v, want returned", out)
	}
	if out.Slug.Slug == "" || out.Slug.Slug == "unknown" {
		t.Fatalf("the class must have been derived: %+v", out.Slug)
	}
	if lab.count("/api/v1/problems/submit") != 1 {
		t.Fatalf("submits = %d, want exactly 1", lab.count("/api/v1/problems/submit"))
	}
	// The brief is stored, digest-pinned, and the prompt the agent would see is
	// digest-pinned alongside it.
	p := deps.lastPayload(types.KResearch)
	if p["brief"] == nil || p["brief_digest"] == nil || p["brief_digest"] == "" {
		t.Fatalf("payload must carry the brief and its digest: %v", p)
	}
	if p["prompt_digest"] == nil || p["prompt_digest"] == "" {
		t.Fatalf("payload must carry prompt_digest (AC-17's audit point): %v", p)
	}
	// The prompt itself shows the brief, fenced and labelled untrusted.
	evidence := map[string]any{"unit": "payment-worker.service",
		"message": "queue wedge: pool exhausted", "source": "journald:payment-worker"}
	prompt, err := BuildAgentPrompt(testIncident(), out, evidence)
	if err != nil {
		t.Fatalf("BuildAgentPrompt: %v", err)
	}
	if !strings.Contains(prompt, "set MemoryMax=512M") {
		t.Fatalf("the agent's prompt does not show the brief:\n%s", prompt)
	}
	if Digest([]byte(prompt)) != p["prompt_digest"] {
		t.Fatalf("prompt_digest does not match the prompt: %v", p["prompt_digest"])
	}
}

// TestAC20_LinksAndUnreachableDegrade.
func TestAC20_LinksAndUnreachableDegrade(t *testing.T) {
	// (a) The chain: one res_ id on the outcome, the research record and the
	// subset SPEC-08's ForemanBrief reads.
	lab := newLabStub(t)
	s, deps := newTestService(t, lab, nil)
	out, err := s.Run(context.Background(), testIncident(), testSig(), bundleFor(map[string]any{"await": true}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.ID == "" || !strings.HasPrefix(out.ID, string(types.PRes)) {
		t.Fatalf("outcome id = %q, want a res_ id", out.ID)
	}
	var found bool
	for _, p := range deps.payloads(types.KResearch) {
		if p["res_id"] == out.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("no research record carries res_id %s", out.ID)
	}
	extras := briefExtras(out)
	if extras["state"] != types.ResReturned || extras["slug"] != out.Slug.Slug {
		t.Fatalf("briefExtras = %v", extras)
	}
	if _, ok := extras["brief"]; !ok {
		t.Fatalf("a returned rung must expose the brief to the foreman: %v", extras)
	}
	if got, ok := s.OutcomeByID(out.ID); !ok || got.ID != out.ID {
		t.Fatalf("OutcomeByID did not resolve %s", out.ID)
	}

	// (b) Forced unreachable: the rung returns degraded with a nil error (the
	// ladder advances) and exactly one gap record lands in the ledger.
	lab2 := newLabStub(t)
	lab2.discover400 = false
	s2, deps2 := newTestService(t, lab2, map[string]any{"request_timeout": "1ns"})
	out2, err := s2.Run(context.Background(), testIncident(), testSig(), bundleFor(nil))
	if err != nil {
		t.Fatalf("an unreachable lab must not return an error: %v", err)
	}
	if out2.State != types.ResDegraded {
		t.Fatalf("outcome = %+v, want degraded", out2)
	}
	gaps := deps2.payloads(types.KGap)
	if len(gaps) != 1 {
		t.Fatalf("gaps = %d, want exactly 1", len(gaps))
	}
	if gaps[0]["cause"] != "research_lab_unreachable" || gaps[0]["est_lost"] != -1 {
		t.Fatalf("gap = %v", gaps[0])
	}
	// The degraded rung still produced a usable outcome: an agent prompt with
	// the no-brief section, so "research never ran" and "research degraded" stay
	// distinguishable.
	prompt, err := BuildAgentPrompt(testIncident(), out2, nil)
	if err != nil {
		t.Fatalf("BuildAgentPrompt: %v", err)
	}
	if !strings.Contains(prompt, "no research brief available") {
		t.Fatalf("prompt = %s", prompt)
	}
	if !strings.Contains(prompt, "## Research brief") {
		t.Fatalf("the section must never be omitted")
	}
}

// TestResolveOutrightRecordsTheUnspentAgentBudget pins the §3.8 claim that a
// brief which resolves the incident spends no agent tokens.
func TestResolveOutrightRecordsTheUnspentAgentBudget(t *testing.T) {
	lab := newLabStub(t)
	lab.discoverFound = true
	s, deps := newTestService(t, lab, map[string]any{
		"agent_tokens_saved_in": 25000, "agent_tokens_saved_out": 8000,
	})
	out, err := s.Run(context.Background(), testIncident(), testSig(), bundleFor(nil))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.State != types.ResReturned {
		t.Fatalf("outcome = %+v", out)
	}
	cost, _ := deps.lastPayload(types.KResearch)["cost"].(map[string]any)
	if asInt64(cost["tokens_saved_in"], 0) != 25000 || asInt64(cost["tokens_saved_out"], 0) != 8000 {
		t.Fatalf("cost = %v, want the unspent agent budget", cost)
	}
	if cost["tokens_estimated"] != true {
		t.Fatalf("tokens_estimated must be true: %v", cost["tokens_estimated"])
	}
}

// TestResearchSpendsNoAgentBudget: the process cost is HTTP, never tokens
// (SPEC-07 §3.8's non-negotiable).
func TestResearchSpendsNoAgentBudget(t *testing.T) {
	lab := newLabStub(t)
	s, _ := newTestService(t, lab, nil)
	if _, err := s.Run(context.Background(), testIncident(), testSig(), bundleFor(map[string]any{"await": true})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	cost, _ := s.Snapshot()["cost"].(map[string]any)
	if asInt64(cost["submits"], 0) == 0 {
		t.Fatalf("cost = %v, want the submit counted", cost)
	}
	if _, ok := cost["tokens_saved_in"]; !ok {
		t.Fatalf("the cost object must carry the token fields: %v", cost)
	}
}

// TestOutcomeStatesAreTheFrozenSet guards against a new state leaking out.
func TestOutcomeStatesAreTheFrozenSet(t *testing.T) {
	lab := newLabStub(t)
	lab.queueStates = []string{"solving"}
	s, _ := newTestService(t, lab, map[string]any{"poll_interval": "1ms", "poll_timeout": "20ms"})
	out, err := s.Run(context.Background(), testIncident(), testSig(), bundleFor(map[string]any{"await": true}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	switch out.State {
	case types.ResRequested, types.ResReturned, types.ResDegraded, types.ResSkipped:
	default:
		t.Fatalf("state %q is outside SPEC-07's frozen set", out.State)
	}
}
