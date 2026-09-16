package types

import "errors"

// ErrorClass is the module/registry error taxonomy (SPEC-TYPES §3.8).
type ErrorClass string

const (
	ErrClassTransient     ErrorClass = "transient"
	ErrClassPermanent     ErrorClass = "permanent"
	ErrClassPolicyRefused ErrorClass = "policy_refused"
)

// Sentinels (SPEC-TYPES §2).
var (
	ErrPolicyRefused = errors.New("policy refused")
	ErrTransient     = errors.New("transient")
	ErrPermanent     = errors.New("permanent")
)

// ErrorCode is a TROUBLE-<AREA>-<NNN> code (SPEC-TYPES §5).
type ErrorCode string

// Codes owned by SPEC-01 (internal/ledger).
const (
	CodeLedger001 ErrorCode = "TROUBLE-LEDGER-001"
	CodeLedger002 ErrorCode = "TROUBLE-LEDGER-002"
	CodeLedger003 ErrorCode = "TROUBLE-LEDGER-003"
	CodeLedger004 ErrorCode = "TROUBLE-LEDGER-004"
	CodeLedger005 ErrorCode = "TROUBLE-LEDGER-005"
	CodeLedger006 ErrorCode = "TROUBLE-LEDGER-006"
	CodeLedger007 ErrorCode = "TROUBLE-LEDGER-007"
	CodeLedger008 ErrorCode = "TROUBLE-LEDGER-008"
	CodeLedger009 ErrorCode = "TROUBLE-LEDGER-009"
	CodeLedger010 ErrorCode = "TROUBLE-LEDGER-010"
	CodeLedger011 ErrorCode = "TROUBLE-LEDGER-011"
	CodeLedger012 ErrorCode = "TROUBLE-LEDGER-012"
)

// SPEC-02 codes (internal/scrub). SPEC-01 §4.2 propagates 006 and 008 unchanged
// from the write-boundary re-scan; the rest are emitted by the engine.
const (
	CodeScrub001 ErrorCode = "TROUBLE-SCRUB-001"
	CodeScrub002 ErrorCode = "TROUBLE-SCRUB-002"
	CodeScrub003 ErrorCode = "TROUBLE-SCRUB-003"
	CodeScrub004 ErrorCode = "TROUBLE-SCRUB-004"
	CodeScrub005 ErrorCode = "TROUBLE-SCRUB-005"
	CodeScrub006 ErrorCode = "TROUBLE-SCRUB-006"
	CodeScrub007 ErrorCode = "TROUBLE-SCRUB-007"
	CodeScrub008 ErrorCode = "TROUBLE-SCRUB-008"
)

// CodeClass is the canonical class of every code this repository emits
// (SPEC-TYPES §5).
var CodeClass = map[ErrorCode]ErrorClass{
	CodeLedger001: ErrClassPermanent,
	CodeLedger002: ErrClassPermanent,
	CodeLedger003: ErrClassTransient,
	CodeLedger004: ErrClassPermanent,
	CodeLedger005: ErrClassPermanent,
	CodeLedger006: ErrClassTransient,
	CodeLedger007: ErrClassTransient,
	CodeLedger008: ErrClassTransient,
	CodeLedger009: ErrClassPermanent,
	CodeLedger010: ErrClassTransient,
	CodeLedger011: ErrClassPermanent,
	CodeLedger012: ErrClassPermanent,
	CodeScrub001:  ErrClassPermanent,
	CodeScrub002:  ErrClassPermanent,
	CodeScrub003:  ErrClassTransient,
	CodeScrub004:  ErrClassPermanent,
	CodeScrub005:  ErrClassTransient,
	CodeScrub006:  ErrClassPermanent,
	CodeScrub007:  ErrClassPermanent,
	CodeScrub008:  ErrClassPermanent,
}

// HasClass reports whether the code's class matches class. (Named HasClass, not
// Is: an error-typed receiver with an `Is` method of a non-error signature trips
// go vet's stdmethods check, and the repo's `make vet` runs it.)
func (e ErrorCode) HasClass(class ErrorClass) bool { return CodeClass[e] == class }

// Error renders the code.
func (e ErrorCode) Error() string { return string(e) }
