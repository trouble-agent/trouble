package registry

import (
	"errors"
	"fmt"

	"github.com/trouble-agent/trouble/internal/types"
)

// Error is a refusal or failure raised by a registry stage. Every code comes
// from the SPEC-06 §5 catalog and its class comes from the same catalog: the
// ladder keys its retry policy on the class, never on the code (SPEC-06 §5).
type Error struct {
	Code   types.ErrorCode
	Class  types.ErrorClass
	Stage  string
	Reason string
	Detail string
	Err    error
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("%s (%s) at stage %s", e.Code, e.Class, e.Stage)
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

// reason names the payload.reason value of a TROUBLE-REGISTRY-006 refusal.
const (
	reasonKillSwitch            = "kill_switch"
	reasonPolicyMissing         = "polkit_missing_policy"
	reasonSkillAllowlist        = "skill_allowlist"
	reasonDoNotTouchWeaken      = "do_not_touch_weaken_refused"
	reasonDoNotTouchPath        = "do_not_touch_path"
	reasonDoNotTouchUnit        = "do_not_touch_unit"
	reasonDoNotTouchScope       = "do_not_touch_scope"
	reasonAllowRootEscape       = "allow_root_escape"
	reasonNoCheckMode           = "no_check_mode"
	reasonScopeNotGranted       = "scope_not_granted"
	reasonPONRWithoutGrant      = "ponr_without_grant"
	reasonReservedCapability    = "reserved_capability_probe"
	reasonTargetLockContention  = "target_lock_contention"
	reasonModuleUnavailable     = "module_unavailable"
	reasonAuditAppendFailed     = "audit_append_failed"
	reasonServeUnitNotAllowed   = "service_unit_not_allowed"
	reasonRollbackUnsupported   = "rollback_unsupported"
	reasonIdempotencyViolation  = "idempotency_violation"
	reasonDescriptorInvalid     = "descriptor_invalid"
	reasonSchemaDrift           = "schema_drift"
	reasonPlayInvalid           = "play_invalid"
	reasonWhenInvalid           = "when_expression_invalid"
	reasonDoNotTouchUnavailable = "do_not_touch_unavailable"
)

func newErr(code types.ErrorCode, stage, reason, format string, args ...any) *Error {
	class, ok := types.CodeClass[code]
	if !ok {
		class = types.ErrClassPermanent
	}
	detail := ""
	if format != "" {
		detail = fmt.Sprintf(format, args...)
	}
	return &Error{Code: code, Class: class, Stage: stage, Reason: reason, Detail: detail}
}

func wrapErr(code types.ErrorCode, stage, reason string, err error) *Error {
	e := newErr(code, stage, reason, "")
	e.Err = err
	return e
}

// CodeOf returns the SPEC-06 code carried by err, or "" when err is not a
// registry error.
func CodeOf(err error) types.ErrorCode {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// ClassOf returns the error class carried by err. An unrecognised error is
// permanent: an unknown failure is never retried (SPEC-06 §5).
func ClassOf(err error) types.ErrorClass {
	var e *Error
	if errors.As(err, &e) {
		return e.Class
	}
	switch {
	case errors.Is(err, types.ErrPolicyRefused):
		return types.ErrClassPolicyRefused
	case errors.Is(err, types.ErrTransient):
		return types.ErrClassTransient
	}
	return types.ErrClassPermanent
}

// ReasonOf returns the payload.reason value of a registry error ("" when unset).
func ReasonOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason
	}
	return ""
}

// classOfModuleError maps a module's returned sentinel onto a class. Any
// unmatched error maps to permanent (SPEC-06 §5): an unknown failure is never
// retried.
func classOfModuleError(err error) types.ErrorClass {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, types.ErrPolicyRefused):
		return types.ErrClassPolicyRefused
	case errors.Is(err, types.ErrTransient):
		return types.ErrClassTransient
	case errors.Is(err, types.ErrPermanent):
		return types.ErrClassPermanent
	}
	return types.ErrClassPermanent
}

// checkCodeForStage attaches the code by stage: 003/011 for Check, 004/010/011
// for Apply, 005 for Verify (SPEC-06 §5).
func checkCodeForStage(stage string, class types.ErrorClass) types.ErrorCode {
	switch stage {
	case types.StageDryRun:
		if class == types.ErrClassPermanent {
			return types.CodeRegistry011
		}
		return types.CodeRegistry003
	case types.StageApply:
		switch class {
		case types.ErrClassPermanent:
			return types.CodeRegistry011
		case types.ErrClassPolicyRefused:
			return types.CodeRegistry011
		default:
			return types.CodeRegistry010
		}
	case types.StageVerify:
		return types.CodeRegistry005
	}
	return types.CodeRegistry011
}
