// Package ladder is the only component that decides what work a detection gets,
// and the only one that declares a problem fixed (SPEC-05 §1). It owns the
// `incident`, `verify` and `breaker` record kinds and is the sole writer of the
// incident index (sig → open incident).
package ladder

import (
	"errors"
	"fmt"

	"github.com/totalwindupflightsystems/trouble/internal/types"
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
