// Package ladder is the only component that decides what work a detection gets,
// and the only one that declares a problem fixed (SPEC-05 §1). It owns the
// `incident`, `verify` and `breaker` record kinds and is the sole writer of the
// incident index (sig → open incident).
package ladder

import (
	"errors"
	"fmt"
	"strings"

	"github.com/trouble-agent/trouble/internal/types"
)

// Error is a ladder refusal or failure. The class comes from the SPEC-05 §5
// catalog; the ladder's callers key their behaviour on the code.
type Error struct {
	Code   types.ErrorCode
	Class  types.ErrorClass
	Reason string
	Detail string
	Err    error
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("%s (%s)", e.Code, e.Class)
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

func newErr(code types.ErrorCode, reason, format string, args ...any) *Error {
	class, ok := types.CodeClass[code]
	if !ok {
		class = types.ErrClassPermanent
	}
	detail := ""
	if format != "" {
		detail = fmt.Sprintf(format, args...)
	}
	return &Error{Code: code, Class: class, Reason: reason, Detail: detail}
}

func wrapErr(code types.ErrorCode, reason string, err error) *Error {
	e := newErr(code, reason, "")
	e.Err = err
	return e
}

// CodeOf returns the ladder code carried by err ("" when err is not a ladder error).
func CodeOf(err error) types.ErrorCode {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// ClassOf returns the class carried by err, or "" when err is nil.
func ClassOf(err error) types.ErrorClass {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Class
	}
	return types.ErrClassPermanent
}

// TRBL-144: *Error carries the write-seam shape the dashboard maps
// (Error.ErrorCode/ErrorDetail/ErrorReason, SPEC-10 §5/§7.6) itself. The
// interfaces live in internal/dashboard, but they are structural: no ladder
// error can be un-adapted on any seam path, so a refusal never collapses into
// the generic 500 action_failed again. The compile-time assertion pins the
// shape; the class semantics mirror the ladder's own catalog (§5): a
// permanent refusal is a refused state transition (409), a transient one is
// 503, and everything else the dashboard maps as 4xx.
var _ interface {
	ErrorCode() types.ErrorCode
	ErrorDetail() string
	ErrorReason() string
	RefusedTransition() bool
} = (*Error)(nil)

// ErrorCode is the seam code the dashboard's mapActionError answers with.
func (e *Error) ErrorCode() types.ErrorCode { return e.Code }

// ErrorReason is the human cause: the ladder's reason, or — where a guard
// refusal carries only a formatted detail (newErr's format string) — the
// detail itself.
func (e *Error) ErrorReason() string {
	if e.Reason != "" {
		return e.Reason
	}
	return e.Detail
}

// ErrorDetail is the machine-readable refusal name: the ladder's code on the
// ladder_<suffix> vocabulary the wire uses for ladder-carried refusals, only
// when the ladder actually filled a detail.
func (e *Error) ErrorDetail() string {
	if e.Detail == "" {
		return ""
	}
	return "ladder_" + strings.ToLower(strings.TrimPrefix(string(e.Code), "TROUBLE-LADDER-"))
}

// RefusedTransition marks a permanent-class ladder refusal as a refused
// state transition that rejected a write: the API answers 409 Conflict; a
// transient class keeps its 503 via the code-class mapping.
func (e *Error) RefusedTransition() bool {
	return types.CodeClass[e.Code] == types.ErrClassPermanent
}

// reason constants (SPEC-05 §3; the string lands in payload.reason).
const (
	reasonSuppressionWindow = "suppression_window"
	reasonNoCheckMode       = "no_check_mode"
	reasonDiagnosisOnly     = "diagnosis_only"
	reasonEvaluationBlocked = "evaluation_impossible"
	reasonRollbackFailed    = "rollback_failed"
	reasonCeiling           = "ceiling_reached"
	reasonBudgetExhausted   = "budget_exhausted"
	reasonDaemonRestartLost = "daemon_restart_lost"
	reasonOrphanWorktree    = "orphan_worktree"
	reasonIndexMismatch     = "index_mismatch"
	reasonKillSwitch        = "kill_switch"
	reasonGateDenied        = "autonomy_gate_denied"
	reasonLeaseHeld         = "lease_held"
	reasonStabilizeInvalid  = "stabilize_window_invalid"
	reasonQuarantined       = "quarantined"
	reasonParkFailed        = "park_failed"
	reasonResumeFailed      = "resume_failed"
	reasonWindowStall       = "ledger_stall"
	reasonSourceRegistered  = "source_registered_mid_window"
	reasonGapMissing        = "gap_missing"
	reasonCanaryMissing     = "canary_missing"
	reasonSourceMissing     = "source_missing"
	reasonRecurrence        = "recurrence_in_window"
	reasonNoRunnableTask    = "no_runnable_task"
)
