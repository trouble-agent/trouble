package hub

import (
	"context"
	"errors"
	"fmt"

	"github.com/trouble-agent/trouble/internal/types"
)

// Reason values carried alongside a TROUBLE-HUB code. They are stable tokens,
// never prose: a record, a log line and a health reason can all name the same
// failure without re-parsing an English sentence.
const (
	ReasonProfile      = "profile"        // 001: the profile itself is unusable
	ReasonRedisDial    = "redis_dial"     // 003: the URL could not be dialled
	ReasonRedisAuth    = "redis_auth"     // 002: the connection or credentials were rejected
	ReasonRedisTopo    = "redis_topology" // 015: cluster/multi-key topology refused
	ReasonRedisPersist = "persistence"    // 016: appendonly=no under require_persistence
	ReasonXAdd         = "xadd"           // 004: the stream write failed while serving
	ReasonGroup        = "group"          // 005: consumer-group operation failed
	ReasonDecode       = "decode"         // 007: an entry does not decode as ForwardEnvelope
	ReasonStranded     = "stranded"       // 008: entries stranded past claim_min_idle × 3
	ReasonDedupGate    = "dedup_gate"     // 006: the gate is unavailable → bounded LRU
	ReasonArchiveCfg   = "archive_config" // 009: the archival target is unusable
	ReasonDuckBrain    = "duckbrain"      // 010: the target was unreachable during export
	ReasonUnclosed     = "not_closed"     // 011: the generation is not closed / changed under the plan
	ReasonVerify       = "verify"         // 012: read-back verification failed
	ReasonDrop         = "drop_gate"      // 014: retention tried to drop an unverified generation
	ReasonProfileSwap  = "profile_switch" // 013: a live profile switch was requested
	ReasonPlain        = "error"

	// ReasonRefusingCode is the error code of require_redis=true's runtime
	// refusal. SPEC-13 §4.3 gives the runtime-loss row a DIFFERENT sender
	// answer when the profile refuses instead of degrades — a hard 503 rather
	// than the overload 429 — and the queue's error values have to carry that
	// distinction so the sentinel can answer it without re-deriving the
	// profile from configuration on every request.
	ReasonRefusingCode = types.CodeRedisRefusing
)

// RedisRefusing is the §4.3 runtime-refusal code: Redis was lost at RUNTIME
// while require_redis=true, and the ingestion surface answers a hard 503
// (unavailable), never the overload 429. It is deliberately not a new
// TROUBLE-HUB-0xx number: the failure class is 004's, and the spec pins the
// distinction on the STATUS, not on a second error code.
const RedisRefusing = types.CodeRedisRefusing

// Error is a hub failure carrying a TROUBLE-HUB code and a stable reason.
//
// The class is read from types.CodeClass (the SPEC-TYPES §5 catalog), so this
// package never keeps a second copy of the class table: SPEC-13 §5 says every
// code here is transient or permanent and that the vocabulary is shared.
type Error struct {
	Code   types.ErrorCode
	Reason string
	Msg    string
	Err    error
}

func (e *Error) Error() string {
	if e.Msg == "" {
		if e.Err != nil {
			return fmt.Sprintf("%s (%s): %v", e.Code, e.Reason, e.Err)
		}
		return fmt.Sprintf("%s (%s)", e.Code, e.Reason)
	}
	if e.Err != nil {
		return fmt.Sprintf("%s (%s): %s: %v", e.Code, e.Reason, e.Msg, e.Err)
	}
	return fmt.Sprintf("%s (%s): %s", e.Code, e.Reason, e.Msg)
}

func (e *Error) Unwrap() error { return e.Err }

// Class returns the SPEC-13 §5 class of the failure.
func (e *Error) Class() types.ErrorClass {
	if c, ok := types.CodeClass[e.Code]; ok {
		return c
	}
	return types.ErrClassPermanent
}

// Transient reports whether the failure is retryable (SPEC-13 §5 class column).
func (e *Error) Transient() bool { return e.Class() == types.ErrClassTransient }

func errf(code types.ErrorCode, reason, format string, args ...any) *Error {
	return &Error{Code: code, Reason: reason, Msg: fmt.Sprintf(format, args...)}
}

func errWrap(code types.ErrorCode, reason, msg string, err error) *Error {
	return &Error{Code: code, Reason: reason, Msg: msg, Err: err}
}

// CodeOf returns the TROUBLE-HUB (or carried) code of err, or "".
func CodeOf(err error) types.ErrorCode {
	var he *Error
	if errors.As(err, &he) {
		return he.Code
	}
	return ""
}

// ReasonOf returns the stable reason token of err, or "".
func ReasonOf(err error) string {
	var he *Error
	if errors.As(err, &he) {
		return he.Reason
	}
	return ""
}

// ErrorCodeString is the plain string form used in payloads (SPEC-INDEX §5.3:
// every failure code is mirrored into the ledger record that describes it).
func ErrorCodeString(err error) string { return string(CodeOf(err)) }

// RefusalError builds the require_redis=true runtime-refusal error: the same
// transient failure class as 004 (the remedy is the same: restore Redis), a
// distinct code so the sentinel's answer can be 503 rather than the overload
// 429 (SPEC-13 §4.3's runtime-loss rows; RuntimeRefusalHTTPStatus is the one
// mapping back).
func RefusalError(code types.ErrorCode, msg string) error {
	return errf(code, ReasonXAdd, "%s", msg)
}

// RuntimeRefusalHTTPStatus maps a hub queue failure to the HTTP status the
// SPEC-13 §4.3 matrix promises the sender. It is THE mapping (§4.3's two
// runtime-loss rows): every other queue code keeps the sentinel's own overload
// answer (429 + Retry-After), while the require_redis=true runtime refusal —
// carried by TROUBLE-REDIS-REFUSING — is the hard unavailability answer, 503 +
// Retry-After: a caller that trusts a 200 can trust durability, so the
// unavailability is named by the status, not disguised as rate limiting.
//
// The CLI does not answer HTTP; this exists so the daemon's one ingestion
// surface and any embedder answer §4.3 identically without each re-deriving
// the profile from configuration.
func RuntimeRefusalHTTPStatus(err error) int {
	if CodeOf(err) == types.CodeRedisRefusing {
		return 503
	}
	return 429
}

// IsRequestEnd reports whether err is the CALLER's request ending — its context
// cancelled, or its deadline passing — rather than a failure of the queue behind
// it.
//
// It is structural, never string matching, because SPEC-13 §4.3's rows are
// statements about REDIS and a caller that went away is not one of them:
//
//   - context.Canceled has exactly one producer: a cancelled context. A Redis
//     server cannot answer with it, and a client that lost its connection is
//     reported as `redis: client is closed` or a net error, so an error chain
//     carrying it is the caller's own lifetime ending.
//   - context.DeadlineExceeded is ambiguous — a read timeout reads the same — so
//     it counts as the caller's only when the caller's own context says so.
func IsRequestEnd(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ctx != nil && errors.Is(ctx.Err(), context.DeadlineExceeded)
	}
	return false
}

// IsQueueFailure reports whether err is evidence ABOUT THE QUEUE: a Redis
// refusal, an unreachable server, a lost consumer group, a transport failure.
//
// An error that cannot be attributed to the queue must not move a state machine
// that describes the queue, so the unattributable case fails CLOSED here and the
// supervisor's own liveness probe (§2.1.1 rule 5) is what decides it — on its
// next pass, with its own attributable error.
func IsQueueFailure(err error) bool {
	if err == nil {
		return false
	}
	if IsRequestEnd(nil, err) {
		return false
	}
	switch CodeOf(err) {
	case types.CodeHub002, types.CodeHub003, types.CodeHub004:
		return true
	}
	// The server REPLIED and what it said is a queue fact: it does not know the
	// group (the cold-server case of §2.1.1 rule 5) or it refused the
	// credentials. TROUBLE-HUB-005 is deliberately absent: an unusable group is
	// refused by attach (ModeRefusing) before a door exists to report it.
	return IsNoGroup(err) || IsAuthError(err)
}
