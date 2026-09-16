package types

// SPEC-06 error codes (TROUBLE-REGISTRY-001..018) and their classes, per SPEC-06 §5.
// Two of them are deliberately NOT permanent: 006 and 007 are policy_refused, so a
// do-not-touch or polkit refusal never enters the ladder's permanent-error branch.

const (
	CodeRegistry001 ErrorCode = "TROUBLE-REGISTRY-001" // permanent: module not found
	CodeRegistry002 ErrorCode = "TROUBLE-REGISTRY-002" // permanent: args violate the schema
	CodeRegistry003 ErrorCode = "TROUBLE-REGISTRY-003" // transient: Check failed
	CodeRegistry004 ErrorCode = "TROUBLE-REGISTRY-004" // transient: Apply failed
	CodeRegistry005 ErrorCode = "TROUBLE-REGISTRY-005" // permanent: Verify not ok
	CodeRegistry006 ErrorCode = "TROUBLE-REGISTRY-006" // policy_refused: capability/kill-switch/skill allowlist/allow-root
	CodeRegistry007 ErrorCode = "TROUBLE-REGISTRY-007" // policy_refused: do-not-touch hit
	CodeRegistry008 ErrorCode = "TROUBLE-REGISTRY-008" // permanent: required scope not granted
	CodeRegistry009 ErrorCode = "TROUBLE-REGISTRY-009" // transient: call exceeded the effective timeout
	CodeRegistry010 ErrorCode = "TROUBLE-REGISTRY-010" // transient: module returned a transient error
	CodeRegistry011 ErrorCode = "TROUBLE-REGISTRY-011" // permanent: module returned a permanent error
	CodeRegistry012 ErrorCode = "TROUBLE-REGISTRY-012" // permanent: idempotency violation
	CodeRegistry013 ErrorCode = "TROUBLE-REGISTRY-013" // permanent: conformance harness failure
	CodeRegistry014 ErrorCode = "TROUBLE-REGISTRY-014" // permanent: descriptor invalid / schema drift
	CodeRegistry015 ErrorCode = "TROUBLE-REGISTRY-015" // permanent: PONR without an explicit module-name grant
	CodeRegistry016 ErrorCode = "TROUBLE-REGISTRY-016" // permanent: play or do-not-touch TOML schema invalid
	CodeRegistry017 ErrorCode = "TROUBLE-REGISTRY-017" // permanent: play when: expression invalid
	CodeRegistry018 ErrorCode = "TROUBLE-REGISTRY-018" // permanent: args failed JSON decoding
)

// RegistryCodeClass is the SPEC-06 §5 class column.
var RegistryCodeClass = map[ErrorCode]ErrorClass{
	CodeRegistry001: ErrClassPermanent,
	CodeRegistry002: ErrClassPermanent,
	CodeRegistry003: ErrClassTransient,
	CodeRegistry004: ErrClassTransient,
	CodeRegistry005: ErrClassPermanent,
	CodeRegistry006: ErrClassPolicyRefused,
	CodeRegistry007: ErrClassPolicyRefused,
	CodeRegistry008: ErrClassPermanent,
	CodeRegistry009: ErrClassTransient,
	CodeRegistry010: ErrClassTransient,
	CodeRegistry011: ErrClassPermanent,
	CodeRegistry012: ErrClassPermanent,
	CodeRegistry013: ErrClassPermanent,
	CodeRegistry014: ErrClassPermanent,
	CodeRegistry015: ErrClassPermanent,
	CodeRegistry016: ErrClassPermanent,
	CodeRegistry017: ErrClassPermanent,
	CodeRegistry018: ErrClassPermanent,
}

func init() {
	for code, class := range RegistryCodeClass {
		CodeClass[code] = class
	}
}
