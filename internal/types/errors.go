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

// SPEC-02 codes the ledger propagates unchanged (SPEC-01 §4.2).
const (
	CodeScrub006 ErrorCode = "TROUBLE-SCRUB-006"
	CodeScrub008 ErrorCode = "TROUBLE-SCRUB-008"
)

// CodeClass is the canonical class of every code this repository emits from
// SPEC-01 (SPEC-TYPES §5). Codes not listed here are not emitted by the ledger.
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
	CodeScrub006:  ErrClassPermanent,
	CodeScrub008:  ErrClassPermanent,
}

// Is reports whether the code's class matches class.
func (e ErrorCode) Is(class ErrorClass) bool { return CodeClass[e] == class }

// Error renders the code.
func (e ErrorCode) Error() string { return string(e) }
