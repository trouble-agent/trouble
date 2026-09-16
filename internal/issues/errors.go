// Package issues is the issue desk (SPEC-09): the outlet that turns a sig-keyed
// incident into a tracked issue in an external tracker, folds every recurrence
// into that same issue, and closes it when the sig has been quiet.
//
// The desk is not a shell and owns no mutating capability: it calls two HTTP APIs
// with a token that lives in a 0600 file, never reads a page, never pages a human
// and never touches git. It emits exactly two ledger record kinds — `issue` and
// `gap` (SPEC-INDEX §3.4).
package issues

import (
	"errors"
	"fmt"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// Reasons attached to a refusal. A reason is a stable token, never prose: the
// ledger payload carries it so a refusal is greppable.
const (
	ReasonTransient      = "transient"
	ReasonTimeout        = "timeout"
	ReasonRateLimited    = "rate_limited"
	ReasonUnauthorized   = "unauthorized"
	ReasonTokenMode      = "token_file_mode"
	ReasonTokenArgv      = "token_on_argv"
	ReasonCap            = "cap"
	ReasonSpoolFull      = "spool_full"
	ReasonSpoolWrite     = "spool_write_failed"
	ReasonReplayFailed   = "replay_failed"
	ReasonAnchorLost     = "anchor_lost"
	ReasonCloseRefused   = "close_refused"
	ReasonHealthcheck    = "healthcheck_failed"
	ReasonDuplicateIdem  = "duplicate_idem_key"
	ReasonValidation     = "validation"
	ReasonValidationFail = "driver_validation"
	ReasonReadback       = "readback_mismatch"
	ReasonDegraded       = "degraded"
	ReasonConfig         = "config_invalid"
	ReasonUnknownDriver  = "unknown_driver"
	ReasonNote           = "note"
)

// Error is a classified desk/driver failure. Every outbound call path returns
// exactly one of these, so SPEC-INDEX §5 rule 3 holds without log reading: the
// code, the class and the retryable flag travel with the error.
type Error struct {
	Code       types.ErrorCode
	Class      types.ErrorClass
	Reason     string
	Retryable  bool
	HTTPStatus int
	ResetTS    string // rate-limit reset, when the driver learned one
	Err        error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	msg := string(e.Code)
	if e.Reason != "" {
		msg += ": " + e.Reason
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// newErr builds a classified error; the class is read from the SPEC-TYPES §5
// catalog, so a code can never be given the wrong class by a call site.
func newErr(code types.ErrorCode, reason string, httpStatus int, retryable bool, format string, args ...any) *Error {
	e := &Error{
		Code:       code,
		Class:      types.CodeClass[code],
		Reason:     reason,
		Retryable:  retryable,
		HTTPStatus: httpStatus,
	}
	if format != "" {
		e.Err = fmt.Errorf(format, args...)
	}
	return e
}

// wrapErr is newErr for an existing transport error.
func wrapErr(code types.ErrorCode, reason string, httpStatus int, retryable bool, cause error) *Error {
	e := newErr(code, reason, httpStatus, retryable, "")
	e.Err = cause
	return e
}

// CodeOf reports the TROUBLE-<AREA>-<NNN> code of err, or "" when err carries
// none (an unclassified error is a bug, and callers treat it as permanent).
func CodeOf(err error) types.ErrorCode {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// ClassOf reports the class of err.
func ClassOf(err error) types.ErrorClass {
	var e *Error
	if errors.As(err, &e) {
		return e.Class
	}
	return types.ErrClassPermanent
}

// ReasonOf reports the reason token of err.
func ReasonOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason
	}
	return ""
}

// RetryableOf reports whether err may enter the spool. Only retryable=true
// failures are ever spooled, so a permanent instance cannot be replayed.
func RetryableOf(err error) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.Retryable
	}
	return false
}

// StatusOf reports the HTTP status the driver saw, or 0.
func StatusOf(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.HTTPStatus
	}
	return 0
}

// ResetTSOf reports the rate-limit reset the error carries, or "".
func ResetTSOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.ResetTS
	}
	return ""
}
