package llm

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// The FALLBACK CHAIN (SPEC-05 §3.7a): candidates are tried in config order, a
// retryable failure moves to the next candidate, a non-retryable one stops, and
// the candidate that served is reported to the caller (which is what the ledger
// records as `serving_candidate`).

func TestFallback_OrderedFailoverOn429Then5xx(t *testing.T) {
	rec := &recorder{}
	urls := servers(t, rec,
		scripted{status: 429, ctype: "application/json", body: `{"error":"slow down"}`, headers: map[string]string{"Retry-After": "1"}},
		scripted{status: 503, ctype: "application/json", body: `{"error":"unavailable"}`},
		scripted{status: 200, ctype: "application/json", body: completion("recovered", 5, 3)},
	)
	client := mustClient(t, testConfig(urls...))

	resp, err := client.Complete(context.Background(), Request{Prompt: "diagnose"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Candidate != "c3" {
		t.Errorf("serving candidate = %q, want c3 (the third candidate is the first healthy one)", resp.Candidate)
	}
	if resp.Text != "recovered" {
		t.Errorf("text = %q, want the third candidate's completion", resp.Text)
	}
	got := rec.all()
	if len(got) != 3 {
		t.Fatalf("HTTP requests = %d, want 3 (one per candidate)", len(got))
	}
	wantClasses := []string{ClassRateLimited, ClassServerError, ""}
	if len(resp.Attempts) != 3 {
		t.Fatalf("attempts = %d, want 3 (%+v)", len(resp.Attempts), resp.Attempts)
	}
	for i, want := range wantClasses {
		if resp.Attempts[i].Class != want {
			t.Errorf("attempts[%d].Class = %q, want %q", i, resp.Attempts[i].Class, want)
		}
		if resp.Attempts[i].Candidate != "c"+itoa(i+1) {
			t.Errorf("attempts[%d].Candidate = %q, want c%d (the configured order)", i, resp.Attempts[i].Candidate, i+1)
		}
	}
	if resp.Attempts[0].Status != 429 || resp.Attempts[1].Status != 503 {
		t.Errorf("attempt statuses = %d/%d, want 429/503", resp.Attempts[0].Status, resp.Attempts[1].Status)
	}
	if resp.Attempts[2].Class != "" || resp.Attempts[2].Status != 200 {
		t.Errorf("the serving attempt = %+v, want an empty class and status 200", resp.Attempts[2])
	}
	if resp.Attempts[0].Reason != ReasonStatus {
		t.Errorf("attempts[0].Reason = %q, want %q", resp.Attempts[0].Reason, ReasonStatus)
	}
}

func TestFallback_TransportFailureMovesOn(t *testing.T) {
	rec := &recorder{}
	urls := servers(t, rec, scripted{status: 200, ctype: "application/json", body: completion("second served", 2, 2)})
	cfg := testConfig("http://127.0.0.1:1/v1", urls[0])
	client := mustClient(t, cfg)

	resp, err := client.Complete(context.Background(), Request{Prompt: "x"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Candidate != "c2" {
		t.Errorf("serving candidate = %q, want c2", resp.Candidate)
	}
	if len(resp.Attempts) != 2 || resp.Attempts[0].Class != ClassTransport {
		t.Fatalf("attempts = %+v, want a transport failure first", resp.Attempts)
	}
	if resp.Attempts[0].Status != 0 {
		t.Errorf("attempts[0].Status = %d, want 0 (no response arrived)", resp.Attempts[0].Status)
	}
}

func TestFallback_PermanentClientErrorStopsTheChain(t *testing.T) {
	rec := &recorder{}
	urls := servers(t, rec,
		scripted{status: 400, ctype: "application/json", body: `{"error":"bad request"}`},
		scripted{status: 200, ctype: "application/json", body: completion("should never be reached", 1, 1)},
	)
	client := mustClient(t, testConfig(urls...))

	_, err := client.Complete(context.Background(), Request{Prompt: "x"})
	if err == nil {
		t.Fatal("Complete succeeded after a 400; a malformed request fails on every candidate")
	}
	if got := ClassOf(err); got != ClassClientError {
		t.Errorf("class = %q, want %q", got, ClassClientError)
	}
	if rec.count() != 1 {
		t.Errorf("HTTP requests = %d, want 1: a 4xx that is not 429/401/403 is not retried across the chain", rec.count())
	}
	if strings.Contains(err.Error(), "should never be reached") {
		t.Error("the second candidate was called after a non-retryable failure")
	}
}

func TestFallback_CredentialFailureMovesToTheNextCandidate(t *testing.T) {
	rec := &recorder{}
	urls := servers(t, rec, scripted{status: 200, ctype: "application/json", body: completion("served by c2", 1, 1)})
	cfg := testConfig(urls[0])
	cfg.Candidates = append([]Candidate{{
		Name: "c0", BaseURL: urls[0], Model: "m0", KeyRef: "TROUBLE_TEST_UNSET_KEY",
	}}, cfg.Candidates...)
	client := mustClient(t, cfg)

	resp, err := client.Complete(context.Background(), Request{Prompt: "x"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Candidate != "c1" {
		t.Errorf("serving candidate = %q, want c1", resp.Candidate)
	}
	if len(resp.Attempts) != 2 || resp.Attempts[0].Class != ClassCredential {
		t.Fatalf("attempts = %+v, want a credential failure on the first candidate", resp.Attempts)
	}
	if rec.count() != 1 {
		t.Errorf("HTTP requests = %d, want 1: no request is sent for an unresolvable key_ref", rec.count())
	}
}

func TestFallback_MaxAttemptsBoundsTheFailover(t *testing.T) {
	rec := &recorder{}
	urls := servers(t, rec,
		scripted{status: 500, ctype: "application/json", body: `{}`},
		scripted{status: 500, ctype: "application/json", body: `{}`},
		scripted{status: 200, ctype: "application/json", body: completion("third", 1, 1)},
	)
	cfg := testConfig(urls...)
	cfg.MaxAttempts = 2
	client := mustClient(t, cfg)

	resp, err := client.Complete(context.Background(), Request{Prompt: "x"})
	if err == nil {
		t.Fatal("Complete went past max_attempts and served from the third candidate")
	}
	if got := ClassOf(err); got != ClassServerError {
		t.Errorf("class = %q, want %q", got, ClassServerError)
	}
	if rec.count() != 2 {
		t.Errorf("HTTP requests = %d, want 2 (max_attempts)", rec.count())
	}
	if len(resp.Attempts) != 2 {
		t.Errorf("attempts = %+v, want exactly 2", resp.Attempts)
	}
}

func TestFallback_ExhaustedChainReportsTheLastClassAndNoCandidate(t *testing.T) {
	rec := &recorder{}
	urls := servers(t, rec,
		scripted{status: 429, ctype: "application/json", body: `{}`},
		scripted{status: 500, ctype: "application/json", body: `{}`},
	)
	client := mustClient(t, testConfig(urls...))

	resp, err := client.Complete(context.Background(), Request{Prompt: "x"})
	if err == nil {
		t.Fatal("Complete reported success with a fully broken chain")
	}
	if got := ClassOf(err); got != ClassServerError {
		t.Errorf("class = %q, want the LAST attempt's class %q", got, ClassServerError)
	}
	if resp.Candidate != "" {
		t.Errorf("serving candidate = %q, want empty (nothing served)", resp.Candidate)
	}
	if resp.Text != "" {
		t.Errorf("text = %q, want empty", resp.Text)
	}
	if len(resp.Attempts) != 2 {
		t.Errorf("attempts = %+v, want both candidates recorded", resp.Attempts)
	}
}

func TestFallback_RunAgentReportsTheOutcomeOnFailure(t *testing.T) {
	rec := &recorder{}
	urls := servers(t, rec,
		scripted{status: 429, ctype: "application/json", body: `{}`},
		scripted{status: 200, ctype: "application/json", body: completion("done", 4, 2)},
	)
	client := mustClient(t, testConfig(urls...))

	out, err := client.RunAgent(context.Background(), "diagnose the wedge")
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if out.Candidate != "c2" || out.Text != "done" || out.FailureClass != "" {
		t.Errorf("outcome = %+v, want the second candidate to have served", out)
	}
	if out.Usage.CompletionTokens != 2 {
		t.Errorf("outcome usage = %+v, want the provider's count", out.Usage)
	}
	if out.Endpoint != endpointHost(urls[1]) {
		t.Errorf("outcome endpoint = %q, want the credential-free host %q", out.Endpoint, endpointHost(urls[1]))
	}
	if len(out.Attempts) != 2 || out.Attempts[0].Class != ClassRateLimited {
		t.Errorf("outcome attempts = %+v, want the failover recorded", out.Attempts)
	}

	// The failure path still reports the attempt list, so a caller that must
	// record the stage never has to parse an error string.
	broken := mustClient(t, testConfig("http://127.0.0.1:1/v1"))
	out, err = broken.RunAgent(context.Background(), "x")
	if err == nil {
		t.Fatal("RunAgent succeeded with no reachable candidate")
	}
	if out.FailureClass != ClassTransport {
		t.Errorf("failure class = %q, want %q", out.FailureClass, ClassTransport)
	}
	if out.Candidate != "" {
		t.Errorf("serving candidate = %q, want empty on failure", out.Candidate)
	}
	if len(out.Attempts) != 1 {
		t.Errorf("attempts = %+v, want the failed attempt recorded", out.Attempts)
	}
}

func TestFallback_ServingCandidateIsRecordedForTheLedger(t *testing.T) {
	rec := &recorder{}
	urls := servers(t, rec,
		scripted{status: 500, ctype: "application/json", body: `{}`},
		scripted{status: 200, ctype: "application/json", body: completion("served", 3, 2)},
	)
	cfg := testConfig(urls...)
	cfg.Candidates[1].Model = "second-model"
	client := mustClient(t, cfg)
	resp, err := client.Complete(context.Background(), Request{Prompt: "x"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// These two fields are exactly what the agent_run payload carries.
	if resp.Candidate != "c2" || resp.Model != "second-model" {
		t.Errorf("serving candidate/model = %q/%q, want c2/second-model", resp.Candidate, resp.Model)
	}
}

func TestBudget_CandidateCapIsSentOnTheWire(t *testing.T) {
	rec := &recorder{}
	urls := servers(t, rec, scripted{status: 200, ctype: "application/json", body: completion("ok", 1, 1)})
	cfg := testConfig(urls[0])
	cfg.MaxTokens = 4096
	cfg.Candidates[0].MaxTokens = 128
	client := mustClient(t, cfg)

	if _, err := client.Complete(context.Background(), Request{Prompt: "x"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	mt, _ := rec.all()[0].Body["max_tokens"].(float64)
	if int(mt) != 128 {
		t.Errorf("wire max_tokens = %v, want the candidate's own cap 128", mt)
	}
}

func TestBudget_CompletionOverCapIsRefused(t *testing.T) {
	rec := &recorder{}
	urls := servers(t, rec, scripted{status: 200, ctype: "application/json", body: completion("too long", 10, 9999)})
	cfg := testConfig(urls[0])
	cfg.MaxTokens = 256
	client := mustClient(t, cfg)

	_, err := client.Complete(context.Background(), Request{Prompt: "x"})
	if err == nil {
		t.Fatal("Complete accepted a completion above the token cap")
	}
	if got := ClassOf(err); got != ClassBudget {
		t.Errorf("class = %q, want %q", got, ClassBudget)
	}
	if got := ReasonOf(err); got != ReasonCompletionCap {
		t.Errorf("reason = %q, want %q", got, ReasonCompletionCap)
	}
}

func TestBudget_WallClockCapFailsOverToTheNextCandidate(t *testing.T) {
	rec := &recorder{}
	urls := servers(t, rec,
		scripted{status: 200, ctype: "application/json", body: completion("late", 1, 1), delay: 300 * time.Millisecond},
		scripted{status: 200, ctype: "application/json", body: completion("fast", 1, 1)},
	)
	cfg := testConfig(urls...)
	cfg.Timeout = types.Duration("60ms")
	client := mustClient(t, cfg)

	start := time.Now()
	resp, err := client.Complete(context.Background(), Request{Prompt: "x"})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Candidate != "c2" {
		t.Errorf("serving candidate = %q, want c2 after the first candidate's wall-clock cap expired", resp.Candidate)
	}
	if resp.Attempts[0].Class != ClassTimeout {
		t.Errorf("attempts[0] = %+v, want a timeout class", resp.Attempts[0])
	}
	if elapsed > 250*time.Millisecond {
		t.Errorf("elapsed %s for a 60ms cap plus a fast second candidate: the cap was not enforced", elapsed)
	}
}

func TestBudget_CallerDeadlineIsNotRetriedAcrossTheChain(t *testing.T) {
	rec := &recorder{}
	urls := servers(t, rec,
		scripted{status: 200, ctype: "application/json", body: completion("late", 1, 1), delay: 300 * time.Millisecond},
		scripted{status: 200, ctype: "application/json", body: completion("fast", 1, 1)},
	)
	cfg := testConfig(urls...)
	cfg.Timeout = types.Duration("5s")
	client := mustClient(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	resp, err := client.Complete(ctx, Request{Prompt: "x"})
	if err == nil {
		t.Fatal("Complete answered after the caller's deadline")
	}
	if got := ClassOf(err); got != ClassTimeout {
		t.Errorf("class = %q, want %q", got, ClassTimeout)
	}
	if got := ReasonOf(err); got != ReasonDeadline {
		t.Errorf("reason = %q, want %q (the caller's deadline, not the attempt's)", got, ReasonDeadline)
	}
	if resp.Candidate != "" {
		t.Errorf("serving candidate = %q, want empty", resp.Candidate)
	}
	if rec.count() != 1 {
		t.Errorf("HTTP requests = %d, want 1: an expired caller deadline must not spend the rest of the chain", rec.count())
	}
}

func TestBudget_CompactCapIsBoundedByTheStageCap(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxTokens = 512
	cfg.Compact.Enabled = true
	cfg.Compact.MaxTokens = 800 // larger than the stage cap: bounded downward
	if got := cfg.CompactCap(); got != 512 {
		t.Errorf("CompactCap() = %d, want the stage cap 512", got)
	}
	cfg.Compact.MaxTokens = 200
	if got := cfg.CompactCap(); got != 200 {
		t.Errorf("CompactCap() = %d, want the declared 200", got)
	}
}
