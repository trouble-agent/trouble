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

// SPEC-04 codes (internal/sentinel). §5 of that spec pins the trigger and the
// HTTP status family of each; the class column here is the SPEC-TYPES §5 value.
const (
	CodeSentinel001 ErrorCode = "TROUBLE-SENTINEL-001" // permanent, 400 envelope framing
	CodeSentinel002 ErrorCode = "TROUBLE-SENTINEL-002" // permanent, 413 compressed/item too large
	CodeSentinel003 ErrorCode = "TROUBLE-SENTINEL-003" // permanent, 413 decompressed cap/ratio
	CodeSentinel004 ErrorCode = "TROUBLE-SENTINEL-004" // permanent, 400 gzip stream invalid
	CodeSentinel005 ErrorCode = "TROUBLE-SENTINEL-005" // permanent, 401 no auth material
	CodeSentinel006 ErrorCode = "TROUBLE-SENTINEL-006" // permanent, 401 auth material invalid
	CodeSentinel007 ErrorCode = "TROUBLE-SENTINEL-007" // permanent, 401 unknown project
	CodeSentinel008 ErrorCode = "TROUBLE-SENTINEL-008" // permanent, 401 project disabled
	CodeSentinel009 ErrorCode = "TROUBLE-SENTINEL-009" // permanent, boot refusal / 400
	CodeSentinel010 ErrorCode = "TROUBLE-SENTINEL-010" // transient, 429 quota / per-IP / concurrency
	CodeSentinel011 ErrorCode = "TROUBLE-SENTINEL-011" // transient, 429 global disk budget
	CodeSentinel012 ErrorCode = "TROUBLE-SENTINEL-012" // permanent, 200 legacy /store/ used
	CodeSentinel013 ErrorCode = "TROUBLE-SENTINEL-013" // permanent, 200 unknown item type dropped
	CodeSentinel014 ErrorCode = "TROUBLE-SENTINEL-014" // permanent, 200 sampled/spooled, 429 dropped
	CodeSentinel015 ErrorCode = "TROUBLE-SENTINEL-015" // permanent, 200 spool budget full
	CodeSentinel016 ErrorCode = "TROUBLE-SENTINEL-016" // permanent, 200 fingerprint fallback
	CodeSentinel017 ErrorCode = "TROUBLE-SENTINEL-017" // permanent, 200 collector parser error
	CodeSentinel018 ErrorCode = "TROUBLE-SENTINEL-018" // permanent, 200 partial assembly
	CodeSentinel019 ErrorCode = "TROUBLE-SENTINEL-019" // permanent, 400 generic JSON invalid
	CodeSentinel020 ErrorCode = "TROUBLE-SENTINEL-020" // transient, 200 spool write failed
	CodeSentinel021 ErrorCode = "TROUBLE-SENTINEL-021" // permanent, 405 wrong method
	CodeSentinel022 ErrorCode = "TROUBLE-SENTINEL-022" // permanent, 415 unsupported media type
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

	CodeSentinel001: ErrClassPermanent,
	CodeSentinel002: ErrClassPermanent,
	CodeSentinel003: ErrClassPermanent,
	CodeSentinel004: ErrClassPermanent,
	CodeSentinel005: ErrClassPermanent,
	CodeSentinel006: ErrClassPermanent,
	CodeSentinel007: ErrClassPermanent,
	CodeSentinel008: ErrClassPermanent,
	CodeSentinel009: ErrClassPermanent,
	CodeSentinel010: ErrClassTransient,
	CodeSentinel011: ErrClassTransient,
	CodeSentinel012: ErrClassPermanent,
	CodeSentinel013: ErrClassPermanent,
	CodeSentinel014: ErrClassPermanent,
	CodeSentinel015: ErrClassPermanent,
	CodeSentinel016: ErrClassPermanent,
	CodeSentinel017: ErrClassPermanent,
	CodeSentinel018: ErrClassPermanent,
	CodeSentinel019: ErrClassPermanent,
	CodeSentinel020: ErrClassTransient,
	CodeSentinel021: ErrClassPermanent,
	CodeSentinel022: ErrClassPermanent,
}

// Is reports whether the code's class matches class.
func (e ErrorCode) Is(class ErrorClass) bool { return CodeClass[e] == class }

// Error renders the code.
func (e ErrorCode) Error() string { return string(e) }
