package hub

import (
	"errors"
	"fmt"

	"github.com/totalwindupflightsystems/trouble/internal/types"
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
)

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
