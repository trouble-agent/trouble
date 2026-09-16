package types

// SPEC-07 error codes (TROUBLE-RESEARCH-001..010) and their classes, per SPEC-07 §5.
// Declared in their own file so the research area never edits another area's
// declaration block; the classes merge into CodeClass at init.

const (
	CodeResearch001 ErrorCode = "TROUBLE-RESEARCH-001" // transient: lab unreachable
	CodeResearch002 ErrorCode = "TROUBLE-RESEARCH-002" // permanent: strict decoder 400
	CodeResearch003 ErrorCode = "TROUBLE-RESEARCH-003" // permanent: 409 duplicate → queued
	CodeResearch004 ErrorCode = "TROUBLE-RESEARCH-004" // transient: solver unavailable / queue failed
	CodeResearch005 ErrorCode = "TROUBLE-RESEARCH-005" // permanent: slug fell back to unknown
	CodeResearch006 ErrorCode = "TROUBLE-RESEARCH-006" // transient: poll budget exhausted
	CodeResearch007 ErrorCode = "TROUBLE-RESEARCH-007" // permanent: corpus grep found nothing
	CodeResearch008 ErrorCode = "TROUBLE-RESEARCH-008" // permanent: driver = none
	CodeResearch009 ErrorCode = "TROUBLE-RESEARCH-009" // permanent: returned brief failed validation
	CodeResearch010 ErrorCode = "TROUBLE-RESEARCH-010" // permanent: response carried no submission_id
)

// ResearchCodeClass is the SPEC-07 §5 class column.
var ResearchCodeClass = map[ErrorCode]ErrorClass{
	CodeResearch001: ErrClassTransient,
	CodeResearch002: ErrClassPermanent,
	CodeResearch003: ErrClassPermanent,
	CodeResearch004: ErrClassTransient,
	CodeResearch005: ErrClassPermanent,
	CodeResearch006: ErrClassTransient,
	CodeResearch007: ErrClassPermanent,
	CodeResearch008: ErrClassPermanent,
	CodeResearch009: ErrClassPermanent,
	CodeResearch010: ErrClassPermanent,
}

func init() {
	for code, class := range ResearchCodeClass {
		CodeClass[code] = class
	}
}
