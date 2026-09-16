// Package sentinel implements SPEC-04: the SDK-compatible ingestion service
// (envelope + legacy store), the generic-JSON on-ramp, the log collectors, the
// group index, releases, quotas and the canary.
//
// It is the code plane: every accepted event becomes `event` + `group` records
// in the ledger, every dropout becomes a `gap`, and every canary injection and
// observation becomes a `canary` record. Nothing here mints an incident, an
// issue or a board row (SPEC-INDEX §3.4).
//
// Two package-private consumer interfaces keep the dependency direction
// one-way (SPEC-TYPES §4): ledgerSink (*ledger.Ledger through LedgerSink) and
// scrubber (*scrub.Engine). Both are documented deviations from SPEC-04 §2.2 in
// docs/sentinel-compat.md §5, because the shipped SPEC-01/SPEC-02 surfaces have
// different signatures than the spec's sketches.
package sentinel

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// Error is a sentinel failure with its code, its HTTP status family and the
// machine-readable `causes` list that rides the error body (SPEC-04 §5).
type Error struct {
	Code   types.ErrorCode
	Status int
	Causes []string
	Msg    string
	Err    error
	// RetryAfterS and Header carry the pinned rate-limit response fields for a
	// 429 (§3.9); they are empty for every other status.
	RetryAfterS int
	Header      string
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	s := string(e.Code)
	if e.Msg != "" {
		s += ": " + e.Msg
	}
	if len(e.Causes) > 0 {
		s += " (" + strings.Join(e.Causes, ",") + ")"
	}
	return s
}

// Unwrap exposes the underlying cause for errors.Is / errors.As.
func (e *Error) Unwrap() error { return e.Err }

// errf builds a sentinel Error. The status is looked up from the code so a code
// can never be emitted with the wrong status family (§5's table).
func errf(code types.ErrorCode, msg string, causes ...string) *Error {
	return &Error{Code: code, Status: statusFor(code, causes), Causes: causes, Msg: msg}
}

// statusFor maps a code to its HTTP status. The default failure status of §5 is
// 400; the exceptions are the pinned ones.
func statusFor(code types.ErrorCode, causes []string) int {
	switch code {
	case types.CodeSentinel001, types.CodeSentinel004, types.CodeSentinel009,
		types.CodeSentinel019:
		return 400
	case types.CodeSentinel002, types.CodeSentinel003:
		return 413
	case types.CodeSentinel005, types.CodeSentinel006, types.CodeSentinel007,
		types.CodeSentinel008:
		return 401
	case types.CodeSentinel010, types.CodeSentinel011:
		return 429
	case types.CodeSentinel021:
		return 405
	case types.CodeSentinel022:
		return 415
	case types.CodeSentinel014:
		// 200 for sample/spool, 429 for drop-with-counter; the caller picks the
		// cause string, so the status follows the cause.
		for _, c := range causes {
			if c == causeDropped {
				return 429
			}
		}
		return 200
	}
	// 012, 013, 015, 016, 017, 018, 020 are 200 by §5: they are accounted, not
	// refused.
	return 200
}

// cause strings used in `causes` (SPEC-04 §3/§5 pin the ones that appear in the
// spec text verbatim; the rest are this package's vocabulary and are listed in
// docs/sentinel-compat.md §4).
const (
	causeLengthRequired    = "length_required"
	causeUnsupportedEnc    = "unsupported_encoding"
	causeItemTooLarge      = "item_too_large"
	causeDecompressedCap   = "decompressed_cap"
	causeCompressionRatio  = "compression_ratio"
	causeSecretLength      = "secret_length"
	causeDSNProjectMism    = "dsn_project_mismatch"
	causeConflictingAuth   = "conflicting_auth_forms"
	causeZoneNotPermitted  = "zone_not_permitted"
	causeTokenRequired     = "token_required"
	causeQueryKeyRemote    = "query_key_remote"
	causeProjectUnknown    = "project_unknown"
	causeProjectDisabled   = "project_disabled"
	causeNoAuth            = "no_auth"
	causeUnknownKey        = "unknown_key"
	causeKeyExpired        = "key_expired"
	causeFraming           = "framing"
	causeTooLarge          = "too_large"
	causeDecompressed      = "decompressed"
	causeGzip              = "gzip"
	causeAuth              = "auth"
	causeMethod            = "method"
	causeMediaType         = "media_type"
	causeGenericJSON       = "generic_json"
	causeQuota             = "quota_epm"
	causeDiskBudget        = "disk_budget"
	causeOverloaded        = "overloaded"
	causeHTTPSNoTerminator = "https_without_terminator"
	causeAdvertisedHost    = "advertised_host"
	causeDropped           = "dropped"
)

// retryAfter is the Retry-After value for a 429 (minimum 1).
func (e *Error) retryAfter() int {
	if e == nil || e.RetryAfterS <= 0 {
		return 1
	}
	return e.RetryAfterS
}

// isCode reports whether err carries code.
func isCode(err error, code types.ErrorCode) bool {
	var se *Error
	if errors.As(err, &se) {
		return se.Code == code
	}
	return false
}

// causeOf returns the first cause of err, or "".
func causeOf(err error) string {
	var se *Error
	if errors.As(err, &se) && len(se.Causes) > 0 {
		return se.Causes[0]
	}
	return ""
}

// rejectReason maps a sentinel Error to the `reject_total{reason=…}` label of
// §5's ledger/measurement column.
func rejectReason(err error) string {
	var se *Error
	if !errors.As(err, &se) {
		return ""
	}
	switch se.Code {
	case types.CodeSentinel001:
		return causeFraming
	case types.CodeSentinel002:
		return causeTooLarge
	case types.CodeSentinel003:
		return causeDecompressed
	case types.CodeSentinel004:
		return causeGzip
	case types.CodeSentinel005:
		return causeNoAuth
	case types.CodeSentinel006:
		return causeAuth
	case types.CodeSentinel007:
		return causeProjectUnknown
	case types.CodeSentinel008:
		return causeProjectDisabled
	case types.CodeSentinel019:
		return causeGenericJSON
	case types.CodeSentinel021:
		return causeMethod
	case types.CodeSentinel022:
		return causeMediaType
	}
	return ""
}

// sortedCauses returns a deterministic copy of a cause list.
func sortedCauses(c []string) []string {
	out := append([]string(nil), c...)
	sort.Strings(out)
	return out
}

// errBody is the pinned error body of §5: {"detail","causes"}.
func errBody(err error) map[string]any {
	var se *Error
	if !errors.As(err, &se) {
		return map[string]any{"detail": err.Error(), "causes": []string{}}
	}
	causes := se.Causes
	if causes == nil {
		causes = []string{}
	}
	return map[string]any{"detail": errString(se), "causes": causes}
}

func errString(e *Error) string {
	if e.Msg != "" {
		return e.Msg
	}
	return string(e.Code)
}

// wrapErr adds context to a sentinel error while preserving its code.
func wrapErr(err error, format string, args ...any) *Error {
	var se *Error
	if errors.As(err, &se) {
		cp := *se
		cp.Msg = fmt.Sprintf("%s: %s", fmt.Sprintf(format, args...), errString(&cp))
		return &cp
	}
	return &Error{Code: types.CodeSentinel001, Status: 400, Msg: fmt.Sprintf(format, args...) + ": " + err.Error(), Err: err}
}
