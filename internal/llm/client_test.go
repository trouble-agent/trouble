package llm

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// The client CONTRACT (SPEC-05 §3.7a): one buffered request/response, a
// configurable endpoint/model/key-ref, and a wall-clock cap that is enforced
// mid-call rather than reported afterwards.

func TestContract_OneBufferedRequestStreamPinnedFalse(t *testing.T) {
	rec := &recorder{}
	urls := servers(t, rec, scripted{status: 200, ctype: "application/json", body: completion("diagnosis text", 11, 7)})
	client := mustClient(t, testConfig(urls[0]))

	resp, err := client.Complete(context.Background(), Request{System: "be terse", Prompt: "explain the wedge"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := rec.count(); got != 1 {
		t.Fatalf("stage sent %d HTTP requests, want exactly 1 (one buffered request per stage)", got)
	}
	got := rec.all()[0]
	if got.Path != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", got.Path)
	}
	// The buffered-only contract has to be visible on the wire: a missing or true
	// `stream` field is the defect this test exists to catch.
	stream, ok := got.Body["stream"]
	if !ok {
		t.Fatalf("request body has no `stream` field; body=%s", got.Raw)
	}
	if stream != false {
		t.Errorf("stream = %v, want false (streaming is a non-goal)", stream)
	}
	if mt, _ := got.Body["max_tokens"].(float64); int(mt) != 256 {
		t.Errorf("max_tokens = %v, want the configured cap 256", got.Body["max_tokens"])
	}
	if got.Body["model"] != "test-model-c1" {
		t.Errorf("model = %v, want the candidate's model", got.Body["model"])
	}
	msgs, _ := got.Body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2 (system + user)", len(msgs))
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "be terse" {
		t.Errorf("messages[0] = %v, want the system message", first)
	}
	second, _ := msgs[1].(map[string]any)
	if second["role"] != "user" || second["content"] != "explain the wedge" {
		t.Errorf("messages[1] = %v, want the prompt", second)
	}
	if !strings.HasPrefix(got.Auth, "Bearer ") {
		t.Errorf("Authorization = %q, want a bearer credential from key_ref", got.Auth)
	}
	if resp.Text != "diagnosis text" {
		t.Errorf("text = %q, want the completion", resp.Text)
	}
	if resp.Candidate != "c1" || resp.Model != "test-model-c1" {
		t.Errorf("serving candidate = %q/%q, want c1/test-model-c1", resp.Candidate, resp.Model)
	}
	if resp.Usage.PromptTokens != 11 || resp.Usage.CompletionTokens != 7 || resp.Usage.TotalTokens != 18 {
		t.Errorf("usage = %+v, want the provider's own counts", resp.Usage)
	}
	if resp.Usage.Estimated {
		t.Error("usage.Estimated = true, want false when the provider reported usage")
	}
	if len(resp.Attempts) != 1 || resp.Attempts[0].Class != "" {
		t.Errorf("attempts = %+v, want one serving attempt with an empty class", resp.Attempts)
	}
}

func TestContract_BaseURLNamingTheRouteIsUsedVerbatim(t *testing.T) {
	rec := &recorder{}
	urls := servers(t, rec, scripted{status: 200, ctype: "application/json", body: completion("ok", 1, 1)})
	cfg := testConfig(urls[0] + "/chat/completions")
	client := mustClient(t, cfg)
	if _, err := client.Complete(context.Background(), Request{Prompt: "x"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := rec.all()[0].Path; got != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions (the base URL already named the route)", got)
	}
}

func TestContract_RefusesEventStreamInsteadOfParsingIt(t *testing.T) {
	rec := &recorder{}
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\ndata: [DONE]\n\n"
	urls := servers(t, rec, scripted{status: 200, ctype: "text/event-stream", body: sse})
	client := mustClient(t, testConfig(urls[0]))

	_, err := client.Complete(context.Background(), Request{Prompt: "x"})
	if err == nil {
		t.Fatal("Complete accepted an event stream; streaming is a non-goal and an SSE body is not a completion")
	}
	if got := ClassOf(err); got != ClassContract {
		t.Errorf("class = %q, want %q (err=%v)", got, ClassContract, err)
	}
	if got := ReasonOf(err); got != ReasonStreamingRefused {
		t.Errorf("reason = %q, want %q", got, ReasonStreamingRefused)
	}
	if strings.Contains(err.Error(), "partial") {
		t.Error("the error quoted streamed content; an SSE body is never interpreted")
	}
}

func TestContract_TimeoutEnforcedMidCall(t *testing.T) {
	rec := &recorder{}
	urls := servers(t, rec, scripted{status: 200, ctype: "application/json", body: completion("late", 1, 1), delay: 400 * time.Millisecond})
	cfg := testConfig(urls[0])
	cfg.Timeout = types.Duration("60ms")
	client := mustClient(t, cfg)

	start := time.Now()
	_, err := client.Complete(context.Background(), Request{Prompt: "x"})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Complete returned a completion for a request that exceeded the wall-clock cap")
	}
	if got := ClassOf(err); got != ClassTimeout {
		t.Errorf("class = %q, want %q (err=%v)", got, ClassTimeout, err)
	}
	if elapsed > 300*time.Millisecond {
		t.Errorf("the cap was not enforced mid-call: elapsed %s for a 60ms cap", elapsed)
	}
	if rec.count() != 1 {
		t.Errorf("HTTP requests = %d, want 1", rec.count())
	}
}

func TestContract_UnparsableAndEmptyEnvelopesAreContractFailures(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"not json", "not json at all"},
		{"no choices", `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			urls := servers(t, rec, scripted{status: 200, ctype: "application/json", body: tc.body})
			client := mustClient(t, testConfig(urls[0]))
			_, err := client.Complete(context.Background(), Request{Prompt: "x"})
			if err == nil {
				t.Fatal("Complete accepted a body it cannot interpret")
			}
			if got := ClassOf(err); got != ClassContract {
				t.Errorf("class = %q, want %q (err=%v)", got, ClassContract, err)
			}
		})
	}
}

func TestContract_UsageEstimatedWhenProviderOmitsIt(t *testing.T) {
	rec := &recorder{}
	urls := servers(t, rec, scripted{status: 200, ctype: "application/json", body: `{"choices":[{"message":{"content":"a small answer"}}]}`})
	client := mustClient(t, testConfig(urls[0]))
	resp, err := client.Complete(context.Background(), Request{Prompt: "x"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !resp.Usage.Estimated {
		t.Error("usage.Estimated = false, want true when the provider reported no usage object")
	}
	if resp.Usage.CompletionTokens != EstimateTokens("a small answer") {
		t.Errorf("completion tokens = %d, want the documented estimate", resp.Usage.CompletionTokens)
	}
}

// The key is named, never carried: key_ref must be an environment-variable name,
// and a resolved value must not surface in a config dump or an error string.

func TestKeyRef_ConfigRefusesALiteralCredential(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Candidates = []Candidate{{
		Name: "c1", BaseURL: "https://example.invalid/v1", Model: "m",
		KeyRef: "sk-live-looks-like-a-key",
	}}
	_, err := New(cfg)
	if err == nil {
		t.Fatal("New accepted a literal credential in key_ref")
	}
	if got := ReasonOf(err); got != ReasonKeyRefShape {
		t.Errorf("reason = %q, want %q (err=%v)", got, ReasonKeyRefShape, err)
	}
}

func TestKeyRef_ResolvedValueNeverAppearsInAnError(t *testing.T) {
	rec := &recorder{}
	urls := servers(t, rec, scripted{status: 401, ctype: "application/json", body: `{"error":"bad key"}`})
	cfg := testConfig(urls[0])
	client := mustClient(t, cfg)

	_, err := client.Complete(context.Background(), Request{Prompt: "x"})
	if err == nil {
		t.Fatal("Complete accepted a 401")
	}
	if got := ClassOf(err); got != ClassCredential {
		t.Errorf("class = %q, want %q (err=%v)", got, ClassCredential, err)
	}
	if strings.Contains(err.Error(), testKeyValue) {
		t.Errorf("the error carries the resolved key value: %v", err)
	}
	for _, c := range client.Config().Candidates {
		if strings.Contains(c.Redacted(), testKeyValue) {
			t.Errorf("the redacted candidate carries the key value: %q", c.Redacted())
		}
	}
}

func TestKeyRef_UnresolvableKeyIsThatCandidateFailure(t *testing.T) {
	rec := &recorder{}
	urls := servers(t, rec, scripted{status: 200, ctype: "application/json", body: completion("served", 1, 1)})
	cfg := testConfig(urls[0])
	// The one candidate names a key that the resolver does not know.
	cfg.Candidates[0].KeyRef = "TROUBLE_TEST_MISSING_KEY"
	client := mustClient(t, cfg)

	_, err := client.Complete(context.Background(), Request{Prompt: "x"})
	if err == nil {
		t.Fatal("Complete answered a request whose key_ref does not resolve")
	}
	if got := ClassOf(err); got != ClassCredential {
		t.Errorf("class = %q, want %q (err=%v)", got, ClassCredential, err)
	}
	if got := ReasonOf(err); got != ReasonKeyRef {
		t.Errorf("reason = %q, want %q", got, ReasonKeyRef)
	}
	if rec.count() != 0 {
		t.Errorf("HTTP requests = %d, want 0: a request is never sent without a credential", rec.count())
	}
}

func TestConfig_StrictDecodeRefusesUnknownKey(t *testing.T) {
	doc := []byte(`
[llm]
max_tokens = 128
max_tokenss = 64
[[llm.candidates]]
name = "c1"
base_url = "https://example.invalid/v1"
model = "m"
key_ref = "TROUBLE_LLM_KEY"
`)
	_, err := LoadConfig(doc)
	if err == nil {
		t.Fatal("LoadConfig accepted an unknown key")
	}
	if !strings.Contains(err.Error(), "max_tokenss") {
		t.Errorf("the refusal does not name the unknown key: %v", err)
	}
}

func TestConfig_LoadsTheChainInConfiguredOrder(t *testing.T) {
	doc := []byte(`
[llm]
max_tokens = 512
timeout = "90s"
fallback_chain = ["backup", "primary"]
compact.enabled = true
compact.budget_tokens = 9000

[[llm.candidates]]
name = "primary"
base_url = "https://a.invalid/v1"
model = "model-a"
key_ref = "TROUBLE_LLM_KEY_A"

[[llm.candidates]]
name = "backup"
base_url = "https://b.invalid/v1"
model = "model-b"
key_ref = "TROUBLE_LLM_KEY_B"
`)
	cfg, err := LoadConfig(doc)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.MaxTokens != 512 || cfg.Timeout.Std().Seconds() != 90 {
		t.Errorf("caps = %d/%s, want 512/90s", cfg.MaxTokens, cfg.Timeout)
	}
	if len(cfg.Candidates) != 2 {
		t.Fatalf("candidates = %d, want 2", len(cfg.Candidates))
	}
	// fallback_chain is the ORDER; the tables are the connection facts.
	if cfg.Candidates[0].Name != "backup" || cfg.Candidates[1].Name != "primary" {
		t.Errorf("chain order = %s,%s, want backup,primary",
			cfg.Candidates[0].Name, cfg.Candidates[1].Name)
	}
	if !cfg.Compact.Enabled || cfg.Compact.BudgetTokens != 9000 {
		t.Errorf("compact = %+v, want enabled with the declared budget", cfg.Compact)
	}
	if cfg.Compact.ChunkTokens != DefaultCompactChunk || cfg.Compact.MaxChunks != DefaultCompactMaxChunks {
		t.Errorf("compact defaults were not inherited: %+v", cfg.Compact)
	}
}

func TestConfig_FallbackChainRefusesAnUnknownName(t *testing.T) {
	doc := []byte(`
[llm]
fallback_chain = ["ghost"]
[[llm.candidates]]
name = "c1"
base_url = "https://a.invalid/v1"
model = "m"
key_ref = "TROUBLE_LLM_KEY"
`)
	_, err := LoadConfig(doc)
	if err == nil {
		t.Fatal("LoadConfig accepted a fallback_chain naming a candidate that does not exist")
	}
	if got := ReasonOf(err); got != ReasonNoCandidate {
		t.Errorf("reason = %q, want %q", got, ReasonNoCandidate)
	}
}

func TestConfig_DefaultsCarryNoEndpoint(t *testing.T) {
	cfg := DefaultConfig()
	if len(cfg.Candidates) != 0 {
		t.Fatalf("the compiled default declares %d candidates, want 0 (nothing fleet-specific is compiled in)", len(cfg.Candidates))
	}
	if _, err := New(cfg); err == nil {
		t.Fatal("New accepted an empty chain: a stage with no candidate would fabricate a completion")
	} else if got := ReasonOf(err); got != ReasonNoCandidate {
		t.Errorf("reason = %q, want %q", got, ReasonNoCandidate)
	}
	// A document with no [llm] table decodes to the same safe default.
	got, err := LoadConfig(nil)
	if err != nil {
		t.Fatalf("LoadConfig(nil): %v", err)
	}
	if len(got.Candidates) != 0 || got.MaxTokens != cfg.MaxTokens {
		t.Errorf("empty document = %+v, want the compiled default", got)
	}
}

func TestConfig_CandidateCapAboveStageCapIsRefused(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxTokens = 100
	cfg.Candidates = []Candidate{{
		Name: "c1", BaseURL: "https://a.invalid/v1", Model: "m",
		KeyRef: testKeyRef, MaxTokens: 500,
	}}
	if _, err := New(cfg); err == nil {
		t.Fatal("New accepted a candidate cap above the stage cap")
	} else if got := ClassOf(err); got != ClassBudget {
		t.Errorf("class = %q, want %q", got, ClassBudget)
	}
}

func TestConfig_RedactedNeverRendersAPathOrCredential(t *testing.T) {
	c := Candidate{Name: "c1", BaseURL: "https://example.invalid:8443/v1", Model: "m", KeyRef: testKeyRef}
	got := c.Redacted()
	if !strings.Contains(got, "example.invalid:8443") || !strings.Contains(got, testKeyRef) {
		t.Errorf("redacted = %q, want the host and the key NAME", got)
	}
	if strings.Contains(got, "/v1") {
		t.Errorf("redacted = %q, want no path", got)
	}
}

func TestErrors_RetryableClassification(t *testing.T) {
	retryable := []string{ClassTransport, ClassTimeout, ClassRateLimited, ClassServerError, ClassCredential}
	for _, c := range retryable {
		if !Retryable(c) {
			t.Errorf("Retryable(%q) = false, want true", c)
		}
	}
	for _, c := range []string{ClassClientError, ClassContract, ClassBudget, ClassCompaction, ""} {
		if Retryable(c) {
			t.Errorf("Retryable(%q) = true, want false", c)
		}
	}
	statuses := map[int]string{
		429: ClassRateLimited, 500: ClassServerError, 503: ClassServerError,
		401: ClassCredential, 403: ClassCredential, 400: ClassClientError, 404: ClassClientError,
	}
	for status, want := range statuses {
		if got := HTTPStatusClass(status); got != want {
			t.Errorf("HTTPStatusClass(%d) = %q, want %q", status, got, want)
		}
	}
}

func TestEstimateTokens_DeterministicMinimum(t *testing.T) {
	if got := EstimateTokens(""); got != 0 {
		t.Errorf("EstimateTokens(\"\") = %d, want 0", got)
	}
	if got := EstimateTokens("ab"); got != 1 {
		t.Errorf("EstimateTokens(short) = %d, want 1", got)
	}
	long := strings.Repeat("x", 400)
	if got := EstimateTokens(long); got != 100 {
		t.Errorf("EstimateTokens(400 bytes) = %d, want 100", got)
	}
}

func TestRequest_TokenCapRefusedBeforeAnyRequest(t *testing.T) {
	rec := &recorder{}
	urls := servers(t, rec, scripted{status: 200, ctype: "application/json", body: completion("never", 1, 1)})
	client := mustClient(t, testConfig(urls[0]))
	_, err := client.Complete(context.Background(), Request{Prompt: "x", MaxTokens: 4096})
	if err == nil {
		t.Fatal("Complete honored a request above the configured cap")
	}
	if got := ClassOf(err); got != ClassBudget {
		t.Errorf("class = %q, want %q", got, ClassBudget)
	}
	if rec.count() != 0 {
		t.Errorf("HTTP requests = %d, want 0: an over-cap request is refused before it is sent", rec.count())
	}
}
