package scrub

import (
	"math"
	"regexp"
)

// entropyRunRE is the run grammar of rule 14: [A-Za-z0-9_\-+/]{40,1000}.
var entropyRunRE = regexp.MustCompile(`[A-Za-z0-9_\-+/]{40,1000}`)

// EntropyMinBitsPerChar is the admission floor of rule 14.
const EntropyMinBitsPerChar = 3.5

// entropySpans implements rule 14 (SPEC-02 §3.3): a run of ≥40 token
// characters is redacted when it mixes cases and digits and carries at least
// 3.5 bits of Shannon entropy per character. Pure hex runs (git SHAs, md5 and
// sha256 digests, hex ids) are rejected, and a 32-hex value is below the
// 40-character floor — so no DSN public key and no Sentry event id is ever in
// scope for this rule, which is why the public-key exemption holds structurally
// rather than by exception.
func entropySpans(b []byte, mode extendMode) []span {
	ms := entropyRunRE.FindAllIndex(b, -1)
	if len(ms) == 0 {
		return nil
	}
	var out []span
	for _, m := range ms {
		s := extendSpan(b, span{start: m[0], end: m[1]}, mode)
		run := b[s.start:s.end]
		if pureHex(run) {
			continue
		}
		if !entropyAdmits(run) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// pureHex reports whether the run is all lower-case hex or all upper-case hex.
// The spec's parenthetical ([0-9a-f]+ or [0-9A-F]+) is read literally: a
// mixed-case hex string is not "pure hex" and therefore falls through to the
// admission test.
func pureHex(run []byte) bool {
	var lower, upper bool
	for _, c := range run {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
			lower = true
		case c >= 'A' && c <= 'F':
			upper = true
		default:
			return false
		}
	}
	return !(lower && upper)
}

// entropyAdmits reports whether a run satisfies hasLower && hasUpper &&
// hasDigit && shannonBitsPerChar(run) >= 3.5.
func entropyAdmits(run []byte) bool {
	var hasLower, hasUpper, hasDigit bool
	for _, c := range run {
		switch {
		case c >= 'a' && c <= 'z':
			hasLower = true
		case c >= 'A' && c <= 'Z':
			hasUpper = true
		case c >= '0' && c <= '9':
			hasDigit = true
		}
	}
	if !hasLower || !hasUpper || !hasDigit {
		return false
	}
	return shannonBitsPerChar(run) >= EntropyMinBitsPerChar
}

// shannonBitsPerChar is the Shannon entropy of the run in bits per character.
func shannonBitsPerChar(b []byte) float64 {
	if len(b) == 0 {
		return 0
	}
	var counts [256]int
	for _, c := range b {
		counts[c]++
	}
	n := float64(len(b))
	h := 0.0
	for _, c := range counts {
		if c == 0 {
			continue
		}
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h
}
