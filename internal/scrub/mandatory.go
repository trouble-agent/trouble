// Package scrub is the single safety gate between the outside world and every
// byte trouble persists (SPEC-02).
//
// SPEC-02 owns this package and ships the full engine (`Engine`, rule sets,
// per-target scrubbing, counters). SPEC-01 §4.2 requires exactly one entry
// point from the ledger — `MandatoryScan` — used as the write-boundary
// defence-in-depth re-scan, and SPEC-INDEX §4.1 fixes the build order
// SPEC-01 → SPEC-02. This file therefore provides the SPEC-01-named seam with
// the mandatory rule set (brief C: env-var assignments, bearer/token shapes,
// private keys, DSN secret parts, connection strings) and nothing else: the
// ledger must not acquire a policy dependency. SPEC-02 replaces the
// implementation behind the same signature (its `Engine.Verify` is the same
// check with the project's configured rules) without touching call sites.
package scrub

import (
	"context"
	"fmt"
	"regexp"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// RulesVersion is reported with every scan result so a rules change is
// observable in the ledger (SPEC-02 §3.9).
const RulesVersion = 1

type rule struct {
	name    string
	pattern string
	re      *regexp.Regexp
}

// mandatoryRules is the rule set that cannot be disabled by configuration and
// that is re-applied at the persistence boundary. Every pattern is RE2 (linear
// time, no catastrophic backtracking) so the scan budget of ≤50 µs per 4 KiB
// payload is structurally met.
var mandatoryRules = []rule{
	{name: "env_assign", pattern: `(?i)\b[A-Z0-9_]*(?:PASS(?:WORD|WD)?|SECRET|TOKEN|APIKEY|API_KEY|CREDENTIAL|PRIVATE_KEY)[A-Z0-9_]*\s*[=:]\s*[^\s"']{4,}`},
	{name: "bearer_token", pattern: `(?i)\bbearer\s+[A-Za-z0-9._\-]{8,}`},
	{name: "private_key", pattern: `-----BEGIN [A-Z ]*PRIVATE KEY-----`},
	{name: "dsn_secret", pattern: `(?i)(?:sentry|dsn|https?)://[0-9a-f]{32}:[0-9a-f]{16,}@`},
	{name: "conn_string", pattern: `(?i)\b(?:postgres(?:ql)?|mysql|mariadb|mongodb(?:\+srv)?|redis|amqp|amqps)://[^\s:/@]+:[^\s@/]{3,}@`},
}

func init() {
	for i := range mandatoryRules {
		re, err := regexp.Compile(mandatoryRules[i].pattern)
		if err != nil {
			panic(fmt.Sprintf("scrub: mandatory rule %s does not compile: %v", mandatoryRules[i].name, err))
		}
		mandatoryRules[i].re = re
	}
}

// MandatoryRuleNames lists the mandatory rule names, in evaluation order.
func MandatoryRuleNames() []string {
	out := make([]string, 0, len(mandatoryRules))
	for _, r := range mandatoryRules {
		out = append(out, r.name)
	}
	return out
}

// ScanError is returned by MandatoryScan when a mandatory pattern is still
// present in bytes that are about to be persisted.
type ScanError struct {
	Code types.ErrorCode
	Rule string
}

func (e *ScanError) Error() string {
	return fmt.Sprintf("%s: persistence-boundary re-scan hit %q", e.Code, e.Rule)
}

// prefilterLiterals are the ASCII literals that every mandatory pattern must
// contain (case-insensitively). The prefilter is a pure performance gate: the
// compiled rules remain the authority, so a hit costs one regex pass and a
// clean payload costs a handful of byte scans instead of five RE2 programs.
// The measured ingest budget (SPEC-01 §4.2: ≤50 µs per 4 KiB payload) is met
// with a factor to spare, which is what makes the boundary affordable on every
// append.
var prefilterLiterals = []string{
	"pass", "secret", "token", "api", "credential", "bearer", "private key", "private_key", "://",
}

// probablyContainsSecret reports whether any mandatory pattern could match.
func probablyContainsSecret(b []byte) bool {
	for _, kw := range prefilterLiterals {
		if containsFoldASCII(b, kw) {
			return true
		}
	}
	return false
}

// containsFoldASCII is an ASCII case-insensitive substring search.
func containsFoldASCII(b []byte, kw string) bool {
	n := len(kw)
	if n == 0 || len(b) < n {
		return false
	}
	lo := kw[0] | 0x20
	up := kw[0] - 32
	for i := 0; i+n <= len(b); i++ {
		c := b[i]
		if c != lo && c != up {
			continue
		}
		if equalFoldASCII(b[i:i+n], kw) {
			return true
		}
	}
	return false
}

func equalFoldASCII(b []byte, kw string) bool {
	for i := 0; i < len(kw); i++ {
		if foldASCII(b[i]) != foldASCII(kw[i]) {
			return false
		}
	}
	return true
}

func foldASCII(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 32
	}
	return c
}

// PrefilterAllows reports whether the performance prefilter would let bytes
// through to the mandatory rules. It exists so a test can prove the prefilter
// never disables a rule; production code never calls it.
func PrefilterAllows(b []byte) bool { return probablyContainsSecret(b) }

// MandatoryScan re-scans bytes that are about to be persisted (SPEC-01 §4.2).
// A hit means the record is refused: it is not written, the ledger propagates
// this error unchanged, and no byte containing the value is stored anywhere.
// TROUBLE-SCRUB-006 is returned when the mandatory rule set is incomplete
// (fail closed); TROUBLE-SCRUB-008 when a mandatory pattern matched.
func MandatoryScan(ctx context.Context, b []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(mandatoryRules) == 0 {
		return &ScanError{Code: types.CodeScrub006}
	}
	if !probablyContainsSecret(b) {
		return nil
	}
	for _, r := range mandatoryRules {
		if r.re.Match(b) {
			return &ScanError{Code: types.CodeScrub008, Rule: r.name}
		}
	}
	return nil
}
