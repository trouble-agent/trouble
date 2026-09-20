// Package llm is SPEC-05 §3.7a's lightweight, buffered LLM client for the
// ladder's budgeted agent stage.
//
// Design constraints, in the order they decide the code:
//
//   - STREAMING IS A NON-GOAL. One buffered request/response per stage: every
//     request body carries `"stream": false`, there is no API that can ask for a
//     stream, and an upstream that answers with an event stream is refused as a
//     contract failure instead of being parsed. A hung stream is the failure mode
//     this package exists to refuse.
//   - STDLIB ONLY. The transport is net/http; nothing here needs an SDK.
//   - BUDGETS ARE ENFORCED, NOT REPORTED. `max_tokens` is checked before the
//     request is sent and again against the provider's own usage; the wall-clock
//     timeout is a context deadline that cancels the call mid-flight. An
//     over-budget call fails as a budget failure — never as a runaway.
//   - THE CHAIN IS ORDERED AND HONEST. Candidates are tried in config order and
//     each failure is classified; the candidate that served is reported to the
//     caller so the ladder can put it in the `agent_run` record.
//   - NO SILENT CONTEXT TRUNCATION. An over-budget context is compacted by a
//     capped, single-shot, per-chunk summarisation pass that records its in/out
//     token counts; when the pass cannot be capped it refuses.
//
// The key itself never enters the config, the ledger, an error message or a log
// line: a candidate names a `key_ref` (an environment-variable name) and the
// value is resolved at call time.
package llm

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/trouble-agent/trouble/internal/types"
)

// Failure classes. Every attempt is classified with exactly one of these; the
// serving candidate or the class of the last attempt is what the ladder records
// in the `agent_run` payload (SPEC-05 §3.12a).
const (
	// ClassTransport is a request that never got an HTTP response: DNS, connect,
	// TLS, reset, or a body that could not be read.
	ClassTransport = "transport"
	// ClassTimeout is the wall-clock cap of the attempt (never the caller's own
	// deadline): the request was cancelled mid-flight and no response was used.
	ClassTimeout = "timeout"
	// ClassRateLimited is HTTP 429.
	ClassRateLimited = "rate_limited"
	// ClassServerError is any 5xx.
	ClassServerError = "server_error"
	// ClassCredential is a candidate whose key_ref does not resolve, or an
	// upstream that answers 401/403. It is per-candidate, not per-chain: another
	// candidate may hold a working credential.
	ClassCredential = "credential"
	// ClassClientError is a 4xx that is not 429/401/403: a request every
	// candidate would reject, so the chain stops here instead of spending the
	// remaining candidates on the same malformed request.
	ClassClientError = "client_error"
	// ClassContract is a response this client refuses to interpret: a non-JSON
	// body, an event stream, a missing choices[0], an unparsable envelope.
	ClassContract = "contract"
	// ClassBudget is a token cap: the request asked for more than the configured
	// cap (nothing was sent), or the provider reported a completion above the cap.
	ClassBudget = "budget"
	// ClassCompaction is a context-compaction refusal: the pass cannot be capped
	// without dropping content, so nothing is sent.
	ClassCompaction = "compaction"
)

// Retryable reports whether the chain may move to the next candidate after a
// failure of this class.
//
// The SPEC-05 §3.7a rule is "failover on transport, 429 and 5xx". Two classes are
// deliberately included with it, because for THIS chain they are the same kind of
// per-candidate availability failure rather than a statement about the request:
// `timeout` (the attempt's own wall-clock cap expired — the candidate is too slow
// right now) and `credential` (this candidate's key_ref does not resolve, or it
// answered 401/403 — one unset environment variable must not take down a chain
// whose second candidate is healthy). A `client_error` is NOT retryable: a
// malformed request is malformed for every candidate, and spending the rest of the
// chain on it is waste. Budget, contract and compaction failures are decisions
// this client already made and are reported, never retried.
func Retryable(class string) bool {
	switch class {
	case ClassTransport, ClassTimeout, ClassRateLimited, ClassServerError, ClassCredential:
		return true
	}
	return false
}

// Error is one classified attempt failure.
type Error struct {
	Class     string
	Candidate string // candidate name, "" when the failure happened before a candidate was chosen
	Status    int    // HTTP status, 0 when there was no response
	Reason    string // stable token (payload.reason), never prose
	Detail    string
	Err       error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	msg := e.Class
	if e.Candidate != "" {
		msg = e.Candidate + ": " + msg
	}
	if e.Status != 0 {
		msg = fmt.Sprintf("%s (HTTP %d)", msg, e.Status)
	}
	if e.Reason != "" {
		msg += ": reason=" + e.Reason
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

func newErr(class, candidate, reason, format string, args ...any) *Error {
	e := &Error{Class: class, Candidate: candidate, Reason: reason}
	if format != "" {
		e.Detail = fmt.Sprintf(format, args...)
	}
	return e
}

// ClassOf reports the failure class carried by err ("" when err carries none).
func ClassOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Class
	}
	return ""
}

// FailureClass is ClassOf as a method, so a caller that holds only an error value
// can read the class through an interface (internal/ladder's portFailure) without
// importing this package.
func (e *Error) FailureClass() string {
	if e == nil {
		return ""
	}
	return e.Class
}

// FailureReason is the stable reason token as a method (see FailureClass).
func (e *Error) FailureReason() string {
	if e == nil {
		return ""
	}
	return e.Reason
}

// ReasonOf reports the stable reason token carried by err ("").
func ReasonOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason
	}
	return ""
}

// Reason tokens. A reason is a stable token the ledger mirrors; it is never a
// sentence and never carries a response body.
const (
	ReasonNoCandidate      = "no_candidate"
	ReasonKeyRef           = "key_ref_unresolved"
	ReasonKeyRefShape      = "key_ref_not_a_name"
	ReasonBadBaseURL       = "base_url_invalid"
	ReasonRequestBuild     = "request_build"
	ReasonTransport        = "transport"
	ReasonStatus           = "upstream_status"
	ReasonTimeout          = "attempt_timeout"
	ReasonDeadline         = "caller_deadline"
	ReasonStreamingRefused = "streaming_refused"
	ReasonBodyUnparsable   = "body_unparsable"
	ReasonEnvelope         = "envelope_invalid"
	ReasonTokenCap         = "token_cap_exceeded"
	ReasonCompletionCap    = "completion_over_cap"
	ReasonCompactionCapped = "compaction_not_cappable"
	ReasonResponseTooLarge = "response_too_large"
	ReasonNoPort           = "no_agent_port"
)

// HTTPStatusClass classifies an HTTP status into a failure class.
func HTTPStatusClass(status int) string {
	switch {
	case status == http.StatusTooManyRequests:
		return ClassRateLimited
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return ClassCredential
	case status >= 500:
		return ClassServerError
	case status >= 400:
		return ClassClientError
	}
	return ""
}

// attempt is one candidate's try, kept for the ledger payload.
type attempt = types.LLMAttempt
