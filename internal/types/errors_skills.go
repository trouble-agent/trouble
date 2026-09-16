package types

// SPEC-11 error codes (TROUBLE-SKILLS-001..014) and their classes, per SPEC-11 §5
// and SPEC-TYPES §5. 001/002/003 are the trust-boundary refusals: an artifact
// that fails any of them is never installed and is never applied.

const (
	CodeSkills001 ErrorCode = "TROUBLE-SKILLS-001" // permanent: artifact parse/schema invalid (unknown key/table, stats, sig grammar, field rule)
	CodeSkills002 ErrorCode = "TROUBLE-SKILLS-002" // permanent: signature verification failed (base64/length/ed25519/play_sha256/boot re-verify)
	CodeSkills003 ErrorCode = "TROUBLE-SKILLS-003" // permanent: signer_key_id unknown, disabled, or wrong trust class for the origin
	CodeSkills004 ErrorCode = "TROUBLE-SKILLS-004" // permanent: min_daemon_version floor not satisfied, or unstamped build
	CodeSkills005 ErrorCode = "TROUBLE-SKILLS-005" // permanent: play references a module outside allowed_modules
	CodeSkills006 ErrorCode = "TROUBLE-SKILLS-006" // permanent: two skills match one sig, or same version with different canonical bytes
	CodeSkills007 ErrorCode = "TROUBLE-SKILLS-007" // permanent: version lower than the highest installed for that name
	CodeSkills008 ErrorCode = "TROUBLE-SKILLS-008" // permanent: provenance missing at draft time
	CodeSkills009 ErrorCode = "TROUBLE-SKILLS-009" // permanent: candidate rejected at review
	CodeSkills010 ErrorCode = "TROUBLE-SKILLS-010" // transient: pull failed against a reachable source (ref/checkout/size/corrupt cache)
	CodeSkills011 ErrorCode = "TROUBLE-SKILLS-011" // transient: skills repo unreachable or pull exceeded pull_timeout
	CodeSkills012 ErrorCode = "TROUBLE-SKILLS-012" // permanent: canary host has no green result for this version inside validity
	CodeSkills013 ErrorCode = "TROUBLE-SKILLS-013" // permanent: skill refused on this host (reason names the cause)
	CodeSkills014 ErrorCode = "TROUBLE-SKILLS-014" // transient: local stats write failed
)

// SkillsCodeClass is the SPEC-11 §5 class column.
var SkillsCodeClass = map[ErrorCode]ErrorClass{
	CodeSkills001: ErrClassPermanent,
	CodeSkills002: ErrClassPermanent,
	CodeSkills003: ErrClassPermanent,
	CodeSkills004: ErrClassPermanent,
	CodeSkills005: ErrClassPermanent,
	CodeSkills006: ErrClassPermanent,
	CodeSkills007: ErrClassPermanent,
	CodeSkills008: ErrClassPermanent,
	CodeSkills009: ErrClassPermanent,
	CodeSkills010: ErrClassTransient,
	CodeSkills011: ErrClassTransient,
	CodeSkills012: ErrClassPermanent,
	CodeSkills013: ErrClassPermanent,
	CodeSkills014: ErrClassTransient,
}

func init() {
	for code, class := range SkillsCodeClass {
		CodeClass[code] = class
	}
}
