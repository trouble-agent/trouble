package scrub

import (
	"fmt"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// Reasons carried by scrub errors. They are a closed vocabulary: an error
// message never contains a value, a prefix of a value or a length of a value
// (SPEC-02 §3.7).
const (
	ReasonConfigInvalid     = "config_invalid"
	ReasonUnknownTarget     = "unknown_target"
	ReasonRuleCompile       = "rule_compile"
	ReasonMandatoryMissing  = "mandatory_rule_missing"
	ReasonTooLarge          = "too_large"
	ReasonInvalidUTF8       = "invalid_utf8"
	ReasonRuleTimeout       = "rule_timeout"
	ReasonRuleError         = "rule_error"
	ReasonReplacementLimit  = "replacement_limit"
	ReasonBoundaryRescanHit = "boundary_rescan_hit"
	ReasonNotUTF8           = "not_utf8"
)

// Error is a scrub error carrying a TROUBLE-SCRUB code (SPEC-02 §5).
type Error struct {
	Code   types.ErrorCode
	Reason string
	// Key is the offending dotted config key or rule name. It never carries a
	// value from the scrubbed text.
	Key string
	Msg string
	Err error
}

func (e *Error) Error() string {
	head := fmt.Sprintf("%s (%s)", e.Code, e.Reason)
	switch {
	case e.Key != "" && e.Msg != "":
		return fmt.Sprintf("%s: %s: %s", head, e.Key, e.Msg)
	case e.Key != "":
		return fmt.Sprintf("%s: key %s", head, e.Key)
	default:
		return head
	}
}

func (e *Error) Unwrap() error { return e.Err }

func errf(code types.ErrorCode, reason, key, msg string, err error) *Error {
	return &Error{Code: code, Reason: reason, Key: key, Msg: msg, Err: err}
}

// configError is TROUBLE-SCRUB-002 carrying the offending dotted key (SPEC-02
// §3.2: every config failure names the key so `trouble config explain` can
// print it).
func configError(key, format string, args ...any) *Error {
	return &Error{
		Code:   types.CodeScrub002,
		Reason: ReasonConfigInvalid,
		Key:    key,
		Msg:    fmt.Sprintf(format, args...),
	}
}

// ScanError is returned when the persistence-boundary re-scan hits a mandatory
// pattern (SPEC-02 §3.4 point 3). internal/ledger propagates it unchanged
// (SPEC-01 §4.2).
type ScanError struct {
	Code types.ErrorCode
	// Rule is diagnostic only: the boundary check never attributes a rule to a
	// counter and never returns the offending bytes.
	Rule string
}

func (e *ScanError) Error() string {
	if e.Rule == "" {
		return fmt.Sprintf("%s: persistence-boundary re-scan hit a mandatory pattern", e.Code)
	}
	return fmt.Sprintf("%s: persistence-boundary re-scan hit %q", e.Code, e.Rule)
}

func boundaryHit(rule string) error { return &ScanError{Code: types.CodeScrub008, Rule: rule} }

// mandatoryMissing is TROUBLE-SCRUB-006: a mandatory rule is absent from the
// compiled set, so the binary must not run (SPEC-02 §3.4 point 1).
func mandatoryMissing(name string) *Error {
	return &Error{
		Code:   types.CodeScrub006,
		Reason: ReasonMandatoryMissing,
		Key:    name,
		Msg:    "mandatory rule is absent from the compiled rule set",
	}
}

// CodeOf returns the TROUBLE code carried by err, or "".
func CodeOf(err error) types.ErrorCode {
	var e *Error
	if e2, ok := err.(*Error); ok {
		e = e2
	}
	if e != nil {
		return e.Code
	}
	if se, ok := err.(*ScanError); ok {
		return se.Code
	}
	return ""
}
