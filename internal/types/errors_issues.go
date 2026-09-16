package types

// SPEC-09 error codes (TROUBLE-ISSUES-001..010) and their classes, per SPEC-09 §5
// and SPEC-TYPES §5. The area contains no policy_refused code: the desk's
// refusals are policy decisions expressed as caps (004), not polkit/do-not-touch
// refusals.

const (
	CodeIssues001 ErrorCode = "TROUBLE-ISSUES-001" // transient: driver API error (5xx/reset/TLS/timeout; 422 is a non-retryable instance)
	CodeIssues002 ErrorCode = "TROUBLE-ISSUES-002" // transient: rate-limited (429, 403 with rate-limit headers, remaining <= min_remaining)
	CodeIssues003 ErrorCode = "TROUBLE-ISSUES-003" // permanent: auth failed / missing or insecure token file / token on argv
	CodeIssues004 ErrorCode = "TROUBLE-ISSUES-004" // permanent: issue cap reached (sig/project/global)
	CodeIssues005 ErrorCode = "TROUBLE-ISSUES-005" // transient: driver spool full or spool write failed
	CodeIssues006 ErrorCode = "TROUBLE-ISSUES-006" // transient: spool replay failed / read-back mismatch
	CodeIssues007 ErrorCode = "TROUBLE-ISSUES-007" // permanent: referenced issue not found at the driver
	CodeIssues008 ErrorCode = "TROUBLE-ISSUES-008" // permanent: close refused (locked / read-back still open) or reopen refused
	CodeIssues009 ErrorCode = "TROUBLE-ISSUES-009" // transient: healthcheck failed fail_after_probes times
	CodeIssues010 ErrorCode = "TROUBLE-ISSUES-010" // permanent: duplicate suppressed inside the dedup window
)

// IssuesCodeClass is the SPEC-09 §5 class column.
var IssuesCodeClass = map[ErrorCode]ErrorClass{
	CodeIssues001: ErrClassTransient,
	CodeIssues002: ErrClassTransient,
	CodeIssues003: ErrClassPermanent,
	CodeIssues004: ErrClassPermanent,
	CodeIssues005: ErrClassTransient,
	CodeIssues006: ErrClassTransient,
	CodeIssues007: ErrClassPermanent,
	CodeIssues008: ErrClassPermanent,
	CodeIssues009: ErrClassTransient,
	CodeIssues010: ErrClassPermanent,
}

func init() {
	for code, class := range IssuesCodeClass {
		CodeClass[code] = class
	}
}
