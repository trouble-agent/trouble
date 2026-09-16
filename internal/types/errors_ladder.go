package types

// SPEC-05 error codes (TROUBLE-LADDER-001..020) and their classes, per SPEC-05 §5.
// Declared in their own file so the ladder area and the sensor/scrub/ledger areas
// never edit the same declaration block; the classes merge into CodeClass at init.

const (
	CodeLadder001 ErrorCode = "TROUBLE-LADDER-001" // permanent: illegal transition
	CodeLadder002 ErrorCode = "TROUBLE-LADDER-002" // permanent: play max_runs exhausted / ceiling
	CodeLadder003 ErrorCode = "TROUBLE-LADDER-003" // transient: host agent lease held elsewhere
	CodeLadder004 ErrorCode = "TROUBLE-LADDER-004" // permanent: two strikes per sig per 24h
	CodeLadder005 ErrorCode = "TROUBLE-LADDER-005" // permanent: verify window invalid
	CodeLadder006 ErrorCode = "TROUBLE-LADDER-006" // permanent: canary not seen
	CodeLadder007 ErrorCode = "TROUBLE-LADDER-007" // permanent: recurrence inside the window
	CodeLadder008 ErrorCode = "TROUBLE-LADDER-008" // transient: rollback of an applied call failed
	CodeLadder009 ErrorCode = "TROUBLE-LADDER-009" // transient: research unavailable
	CodeLadder010 ErrorCode = "TROUBLE-LADDER-010" // permanent: kill-switch active at a checkpoint
	CodeLadder011 ErrorCode = "TROUBLE-LADDER-011" // permanent: autonomy gate denied for this stage
	CodeLadder012 ErrorCode = "TROUBLE-LADDER-012" // permanent: incident id not found in the index
	CodeLadder013 ErrorCode = "TROUBLE-LADDER-013" // permanent: per-day budget exhausted
	CodeLadder014 ErrorCode = "TROUBLE-LADDER-014" // permanent: breaker open for the scope
	CodeLadder015 ErrorCode = "TROUBLE-LADDER-015" // transient: park failed
	CodeLadder016 ErrorCode = "TROUBLE-LADDER-016" // transient: resume failed
	CodeLadder017 ErrorCode = "TROUBLE-LADDER-017" // permanent: quarantined
	CodeLadder018 ErrorCode = "TROUBLE-LADDER-018" // permanent: suppression window active
	CodeLadder019 ErrorCode = "TROUBLE-LADDER-019" // permanent: stabilization window invalid
	CodeLadder020 ErrorCode = "TROUBLE-LADDER-020" // permanent: reopen found a mismatched open incident
)

// LadderCodeClass is the SPEC-05 §5 class column.
var LadderCodeClass = map[ErrorCode]ErrorClass{
	CodeLadder001: ErrClassPermanent,
	CodeLadder002: ErrClassPermanent,
	CodeLadder003: ErrClassTransient,
	CodeLadder004: ErrClassPermanent,
	CodeLadder005: ErrClassPermanent,
	CodeLadder006: ErrClassPermanent,
	CodeLadder007: ErrClassPermanent,
	CodeLadder008: ErrClassTransient,
	CodeLadder009: ErrClassTransient,
	CodeLadder010: ErrClassPermanent,
	CodeLadder011: ErrClassPermanent,
	CodeLadder012: ErrClassPermanent,
	CodeLadder013: ErrClassPermanent,
	CodeLadder014: ErrClassPermanent,
	CodeLadder015: ErrClassTransient,
	CodeLadder016: ErrClassTransient,
	CodeLadder017: ErrClassPermanent,
	CodeLadder018: ErrClassPermanent,
	CodeLadder019: ErrClassPermanent,
	CodeLadder020: ErrClassPermanent,
}

func init() {
	for code, class := range LadderCodeClass {
		CodeClass[code] = class
	}
}
