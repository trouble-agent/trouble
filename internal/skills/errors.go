// Package skills is SPEC-11: it turns a verified incident fix into a signed,
// immutable skill artifact, distributes skills down into hosts over a pull-only
// release channel, and decides — on every recurrence — whether a skill may be
// applied to a signature.
//
// A pulled skill is executable material, so this package is a trust boundary, not
// a file-format spec: every artifact is verified (ed25519 over a canonical byte
// form), gated (signer set, version floor, module allowlist, canary, approve
// policy) and recorded on refusal. Nothing here shells out except an argv-only
// `git` read of the channel.
package skills

import (
	"errors"
	"fmt"

	"github.com/trouble-agent/trouble/internal/types"
)

// Reason tokens attached to a refusal. A reason is a stable token, never prose.
const (
	ReasonUnknownKey       = "unknown_key"
	ReasonUnknownTable     = "unknown_table"
	ReasonStatsInArtifact  = "stats_in_artifact"
	ReasonSigGrammar       = "sig_grammar"
	ReasonFieldRule        = "field_rule"
	ReasonVersionRule      = "version_rule"
	ReasonPlayMissing      = "play_missing"
	ReasonSignature        = "signature_invalid"
	ReasonSignerUnknown    = "signer_unknown"
	ReasonSignerDisabled   = "signer_disabled"
	ReasonSignerTrust      = "signer_trust"
	ReasonFloor            = "floor"
	ReasonUnstampedBinary  = "unstamped_binary"
	ReasonMissingModule    = "missing_module"
	ReasonAllowlist        = "allowlist"
	ReasonVersionAmbiguity = "version_ambiguity"
	ReasonNameOwnedRelease = "name_owned_by_release"
	ReasonCanaryFailed     = "canary_failed"
	ReasonCanaryNeedsApply = "canary_needs_apply"
	ReasonCanaryWrongVer   = "canary_wrong_version"
	ReasonApproveNever     = "approve_never"
	ReasonHoldExpired      = "hold_expired"
	ReasonMaxRuns          = "max_runs"
	ReasonDemoted          = "demoted"
	ReasonRejected         = "rejected"
	ReasonDowngrade        = "downgrade"
	ReasonPullRef          = "pull_ref"
	ReasonPullOffline      = "pull_offline"
	ReasonPullSize         = "pull_size"
	ReasonPullTimeout      = "pull_timeout"
	ReasonProvenance       = "provenance"
	ReasonReadback         = "readback_mismatch"
	ReasonValidation       = "validation"
	ReasonConfig           = "config_invalid"
	ReasonConflict         = "conflict"
	ReasonStatsWrite       = "stats_write"
	ReasonTampered         = "tampered"
	ReasonUnknownSource    = "unknown_source"
)

// Error is a classified skills failure, mirroring internal/issues' shape so the
// ledger payload can carry code + class + reason on every refusal.
type Error struct {
	Code   types.ErrorCode
	Class  types.ErrorClass
	Reason string
	Err    error
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

func newErr(code types.ErrorCode, reason string, format string, args ...any) *Error {
	e := &Error{Code: code, Class: types.CodeClass[code], Reason: reason}
	if format != "" {
		e.Err = fmt.Errorf(format, args...)
	}
	return e
}

// CodeOf reports the code of err, or "" when err carries none.
func CodeOf(err error) types.ErrorCode {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// ReasonOf reports the reason token of err.
func ReasonOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason
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
