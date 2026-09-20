package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// EnvKeyResolver is the default key_ref resolver: the value of the named
// environment variable. SPEC-12's `[secrets] environment_file` loads that file into
// the process environment at boot, so an environment NAME is the one reference that
// never puts a credential into a file this package reads, a record it writes or an
// error it returns.
func EnvKeyResolver(ref string) (string, error) {
	if !keyRefRE.MatchString(ref) {
		return "", newErr(ClassContract, "", ReasonKeyRefShape, "key_ref %q is not an environment-variable name", ref)
	}
	val := strings.TrimSpace(os.Getenv(ref))
	if val == "" {
		return "", newErr(ClassCredential, "", ReasonKeyRef,
			"key_ref %s is unset or empty in the environment", ref)
	}
	return val, nil
}

// Client is the buffered, budgeted, ordered-chain LLM client.
type Client struct {
	cfg   Config
	httpc *http.Client
}

// New builds the client and validates the configuration. A configuration that
// cannot be honored (no chain, a bad base URL, a literal credential in `key_ref`, a
// per-candidate cap above the stage cap) is a construction error, never a
// boot-time surprise on the first call.
func New(cfg Config) (*Client, error) {
	if cfg.KeyResolver == nil {
		cfg.KeyResolver = EnvKeyResolver
	}
	if cfg.HTTPClient == nil {
		// No client-level timeout: the wall-clock budget is a per-attempt context
		// deadline, so a hung upstream is cancelled at exactly the configured cap
		// and classifies as this package's `timeout`. A client-level timeout would
		// surface as an opaque transport error instead.
		cfg.HTTPClient = &http.Client{}
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if len(cfg.Candidates) == 0 {
		return nil, newErr(ClassContract, "", ReasonNoCandidate,
			"the fallback chain is empty: declare at least one [[llm.candidates]] entry")
	}
	return &Client{cfg: cfg, httpc: cfg.HTTPClient}, nil
}

// Config returns the resolved configuration. It can contain a `key_ref` (a name)
// but never a resolved key.
func (c *Client) Config() Config { return c.cfg }

// Request is one buffered completion request. There is no Stream field: streaming
// is not expressible (SPEC-05 §3.7a).
type Request struct {
	// System is the system message ("" omits it).
	System string
	// Prompt is the user message.
	Prompt string
	// Context is the assembled context, one chunk per element. An over-budget
	// context is compacted before the request is built; it is never truncated.
	Context []string
	// MaxTokens overrides the stage cap for this request (0 = the stage cap). A
	// value above the stage cap is refused before anything is sent.
	MaxTokens int
}

// Response is the buffered completion plus the accounting the ledger records.
type Response struct {
	Text       string
	Candidate  string
	Model      string
	Endpoint   string
	Usage      types.LLMUsage
	Compaction types.LLMCompaction
	Attempts   []types.LLMAttempt
	LatencyMS  int
}

// Complete performs one buffered completion over the ordered chain.
//
// Order of enforcement: the token cap first (an over-cap request is never sent),
// then the context budget (an over-budget context is compacted or refused), then
// each attempt under its own wall-clock deadline.
func (c *Client) Complete(ctx context.Context, req Request) (Response, error) {
	if len(c.cfg.Candidates) == 0 {
		return Response{}, newErr(ClassContract, "", ReasonNoCandidate, "the fallback chain is empty")
	}
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = c.cfg.MaxTokens
	}
	switch {
	case maxTokens < 0:
		return Response{}, newErr(ClassBudget, "", ReasonTokenCap, "max_tokens %d is negative", maxTokens)
	case maxTokens > c.cfg.MaxTokens:
		return Response{}, newErr(ClassBudget, "", ReasonTokenCap,
			"max_tokens %d exceeds the configured cap %d; the request is refused before it is sent", maxTokens, c.cfg.MaxTokens)
	}

	ctxChunks, compaction, err := c.MaybeCompact(ctx, req.Context)
	if err != nil {
		return Response{}, err
	}

	out := Response{Compaction: compaction}
	limit := len(c.cfg.Candidates)
	if c.cfg.MaxAttempts > 0 && c.cfg.MaxAttempts < limit {
		limit = c.cfg.MaxAttempts
	}
	var lastErr error
	for i, cand := range c.cfg.Candidates {
		if i >= limit {
			break
		}
		// The per-attempt cap is the candidate's own cap (or the stage cap), and a
		// request-level cap can only lower it — never raise it past the stage cap,
		// which was checked above.
		cap := cand.EffectiveMaxTokens(c.cfg)
		if req.MaxTokens > 0 && req.MaxTokens < cap {
			cap = req.MaxTokens
		}
		body, err := buildBody(req, ctxChunks, cap, cand.Model)
		if err != nil {
			return out, err
		}
		start := time.Now()
		res, callErr := c.call(ctx, cand, body, cap)
		latency := int(time.Since(start).Milliseconds())
		if callErr == nil {
			out.Text = res.Text
			out.Candidate = cand.Name
			out.Model = cand.Model
			out.Endpoint = endpointHost(cand.BaseURL)
			out.Usage = res.Usage
			out.LatencyMS = latency
			out.Attempts = append(out.Attempts, attempt{
				Candidate: cand.Name, Model: cand.Model, Status: http.StatusOK, LatencyMS: latency,
			})
			return out, nil
		}
		class, reason := ClassOf(callErr), ReasonOf(callErr)
		out.Attempts = append(out.Attempts, attempt{
			Candidate: cand.Name, Model: cand.Model, Status: statusOf(callErr),
			Class: class, Reason: reason, LatencyMS: latency,
		})
		lastErr = callErr
		if !Retryable(class) {
			// The chain stops: either the remaining candidates would fail the same
			// way (a malformed request) or the decision was already this client's
			// (budget, contract, compaction).
			break
		}
	}
	return out, lastErr
}

// RunAgent is the ladder's agent-stage port (SPEC-05 §2a): one buffered completion
// that reports which candidate served it, or why none did.
//
// It returns the outcome AND the error. The outcome is the ledger-facing report
// even on failure — the attempt list is what explains a failover — so a caller that
// must record the stage never has to reconstruct it from an error string.
func (c *Client) RunAgent(ctx context.Context, prompt string) (types.AgentOutcome, error) {
	resp, err := c.Complete(ctx, Request{Prompt: prompt})
	out := types.AgentOutcome{
		Text:       resp.Text,
		Candidate:  resp.Candidate,
		Model:      resp.Model,
		Endpoint:   resp.Endpoint,
		Usage:      resp.Usage,
		Compaction: resp.Compaction,
		Attempts:   resp.Attempts,
	}
	if err != nil {
		out.FailureClass = ClassOf(err)
		if out.FailureClass == "" {
			out.FailureClass = ClassContract
		}
		return out, err
	}
	return out, nil
}

// callResult is one successful attempt's decoded payload.
type callResult struct {
	Text  string
	Usage types.LLMUsage
}

// call performs one attempt against one candidate. It never retries: the chain is
// the retry mechanism, and an attempt is the unit the wall clock is measured on.
func (c *Client) call(ctx context.Context, cand Candidate, body []byte, maxTokens int) (callResult, error) {
	if ctx.Err() != nil {
		return callResult{}, newErr(ClassTimeout, cand.Name, ReasonDeadline, "the caller's context is already done")
	}
	key, err := c.cfg.KeyResolver(cand.KeyRef)
	if err != nil {
		if ctx.Err() != nil {
			return callResult{}, newErr(ClassTimeout, cand.Name, ReasonDeadline, "the caller's context is done")
		}
		// This candidate's credential, not the request's problem: the chain moves
		// on. The resolver carries the specific reason when it has one; a bare
		// resolver error still records the class's own token.
		reason := ReasonOf(err)
		if reason == "" {
			reason = ReasonKeyRef
		}
		return callResult{}, newErr(ClassCredential, cand.Name, reason, "key_ref %s", cand.KeyRef)
	}

	attemptCtx, cancel := context.WithTimeout(ctx, cand.EffectiveTimeout(c.cfg).Std())
	defer cancel()

	httpReq, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, completionURL(cand.BaseURL), bytes.NewReader(body))
	if err != nil {
		return callResult{}, newErr(ClassContract, cand.Name, ReasonRequestBuild, "%v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+key)

	httpResp, err := c.httpc.Do(httpReq)
	if err != nil {
		// The caller's deadline is not this candidate's fault and must not be
		// retried against the next one: the stage is out of wall clock.
		if ctx.Err() != nil {
			return callResult{}, newErr(ClassTimeout, cand.Name, ReasonDeadline, "the caller's context is done")
		}
		if attemptCtx.Err() != nil {
			return callResult{}, newErr(ClassTimeout, cand.Name, ReasonTimeout,
				"the attempt exceeded its %s wall-clock cap", cand.EffectiveTimeout(c.cfg))
		}
		e := newErr(ClassTransport, cand.Name, ReasonTransport, "%v", err)
		e.Err = err
		return callResult{}, e
	}
	defer func() { _ = httpResp.Body.Close() }()

	if httpResp.StatusCode != http.StatusOK {
		class := HTTPStatusClass(httpResp.StatusCode)
		if class == "" {
			class = ClassContract
		}
		e := newErr(class, cand.Name, ReasonStatus, "upstream answered %s%s",
			httpResp.Status, briefBody(httpResp.Body, c.cfg.MaxResponseBytes))
		e.Status = httpResp.StatusCode
		return callResult{}, e
	}
	if ct := httpResp.Header.Get("Content-Type"); strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "text/event-stream") {
		return callResult{}, newErr(ClassContract, cand.Name, ReasonStreamingRefused,
			"upstream answered an event stream (%s); this client reads buffered completions only", ct)
	}

	raw, err := io.ReadAll(io.LimitReader(httpResp.Body, c.cfg.MaxResponseBytes+1))
	if err != nil {
		if attemptCtx.Err() != nil {
			return callResult{}, newErr(ClassTimeout, cand.Name, ReasonTimeout, "reading the body exceeded the wall-clock cap")
		}
		return callResult{}, newErr(ClassTransport, cand.Name, ReasonTransport, "read body: %v", err)
	}
	if int64(len(raw)) > c.cfg.MaxResponseBytes {
		return callResult{}, newErr(ClassContract, cand.Name, ReasonResponseTooLarge,
			"response exceeded max_response_bytes %d", c.cfg.MaxResponseBytes)
	}

	text, usage, err := decodeCompletion(raw, maxTokens)
	if err != nil {
		var e *Error
		if errors.As(err, &e) {
			e.Candidate = cand.Name
			return callResult{}, e
		}
		return callResult{}, newErr(ClassContract, cand.Name, ReasonBodyUnparsable, "%v", err)
	}
	return callResult{Text: text, Usage: usage}, nil
}

// chatRequest is the wire body. `stream` is a constant false and stays in the
// struct so the buffered-only contract is visible on the wire — and asserted there
// by the contract test.
type chatRequest struct {
	Model     string        `json:"model"`
	Messages  []chatMessage `json:"messages"`
	MaxTokens int           `json:"max_tokens"`
	Stream    bool          `json:"stream"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatResponse is the subset of the OpenAI-compatible envelope this client reads.
type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// buildBody renders the request body for ONE candidate: one buffered request with
// the candidate's model and `stream` pinned false.
func buildBody(req Request, ctxChunks []string, maxTokens int, model string) ([]byte, error) {
	msgs := make([]chatMessage, 0, 2)
	if strings.TrimSpace(req.System) != "" {
		msgs = append(msgs, chatMessage{Role: "system", Content: req.System})
	}
	user := req.Prompt
	if len(ctxChunks) > 0 {
		user = strings.Join(append([]string{req.Prompt}, ctxChunks...), "\n\n")
	}
	msgs = append(msgs, chatMessage{Role: "user", Content: user})
	body, err := json.Marshal(chatRequest{Model: model, Messages: msgs, MaxTokens: maxTokens, Stream: false})
	if err != nil {
		return nil, newErr(ClassContract, "", ReasonRequestBuild, "encode request: %v", err)
	}
	return body, nil
}

// decodeCompletion parses one buffered completion. Every shape it refuses is a
// ClassContract failure: this client does not guess what an upstream meant.
func decodeCompletion(raw []byte, maxTokens int) (string, types.LLMUsage, error) {
	var env chatResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		e := newErr(ClassContract, "", ReasonBodyUnparsable, "decode response: %v", err)
		e.Err = err
		return "", types.LLMUsage{}, e
	}
	if len(env.Choices) == 0 {
		return "", types.LLMUsage{}, newErr(ClassContract, "", ReasonEnvelope,
			"response has no choices[0]; this client does not guess an empty completion")
	}
	usage := types.LLMUsage{
		PromptTokens:     env.Usage.PromptTokens,
		CompletionTokens: env.Usage.CompletionTokens,
		TotalTokens:      env.Usage.TotalTokens,
	}
	if usage.PromptTokens == 0 && usage.CompletionTokens == 0 {
		// No usage object: estimate from the bytes and SAY SO (`estimated:true`).
		usage = types.LLMUsage{
			PromptTokens:     EstimateTokens(string(raw)),
			CompletionTokens: EstimateTokens(env.Choices[0].Message.Content),
			Estimated:        true,
		}
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	if usage.CompletionTokens > maxTokens {
		return "", usage, newErr(ClassBudget, "", ReasonCompletionCap,
			"the completion reported %d tokens, above the cap %d", usage.CompletionTokens, maxTokens)
	}
	return env.Choices[0].Message.Content, usage, nil
}

// completionURL joins a candidate's base URL with the one route this client uses. A
// base URL that already names the route is used verbatim, so both `/v1` and
// `/v1/chat/completions` forms work.
func completionURL(base string) string {
	trimmed := strings.TrimRight(base, "/")
	if strings.HasSuffix(trimmed, "/chat/completions") {
		return trimmed
	}
	return trimmed + "/chat/completions"
}

// endpointHost is the recorded endpoint: the credential-free host only.
func endpointHost(base string) string {
	if i := strings.Index(base, "://"); i >= 0 {
		base = base[i+3:]
	}
	if i := strings.IndexAny(base, "/?#"); i >= 0 {
		base = base[:i]
	}
	return base
}

// briefBody quotes at most 200 bytes of an error body, so an upstream's prose
// cannot flood a record (and a request echo cannot ride along).
func briefBody(r io.Reader, max int64) string {
	limit := int64(200)
	if max > 0 && max < limit {
		limit = max
	}
	raw, err := io.ReadAll(io.LimitReader(r, limit))
	if err != nil || len(raw) == 0 {
		return ""
	}
	return ": " + strings.TrimSpace(string(raw))
}

// statusOf reports the HTTP status carried by a classified attempt error.
func statusOf(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}
