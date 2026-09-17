package scrub

import (
	"bytes"
	"strconv"
)

// The loopback/RFC1918 exemption of rules 15 and 16 (SPEC-02 §3.3): those
// addresses identify nobody outside this host, so they are left unchanged and
// counted as exempt rather than redacted.
//
//	127.0.0.0/8, 0.0.0.0, 10/8, 172.16/12, 192.168/16, 169.254/16

// parseQuad parses a quoted or bare dotted quad; ok is false when tok is not
// four decimal octets in 0..255.
func parseQuad(tok []byte) ([4]int, bool) {
	tok = bytes.Trim(tok, `"'`)
	parts := bytes.Split(tok, []byte("."))
	if len(parts) != 4 {
		return [4]int{}, false
	}
	var oct [4]int
	for i, p := range parts {
		if len(p) == 0 || len(p) > 3 {
			return [4]int{}, false
		}
		n, err := strconv.Atoi(string(p))
		if err != nil || n < 0 || n > 255 {
			return [4]int{}, false
		}
		oct[i] = n
	}
	return oct, true
}

// exemptAddr reports whether tok is an address the operators exempted.
func exemptAddr(tok []byte) bool {
	oct, ok := parseQuad(tok)
	if !ok {
		return false
	}
	switch {
	case oct[0] == 127, oct[0] == 10, oct[0] == 0:
		return true
	case oct[0] == 172 && oct[1] >= 16 && oct[1] <= 31:
		return true
	case oct[0] == 192 && oct[1] == 168:
		return true
	case oct[0] == 169 && oct[1] == 254:
		return true
	}
	return false
}

// looksLikeQuad reports whether tok consists only of digits and dots, i.e. it is
// a candidate for the IPv4 alternative rather than an email address.
func looksLikeQuad(tok []byte) bool {
	if len(tok) == 0 {
		return false
	}
	hasDot := false
	for _, c := range tok {
		switch {
		case c >= '0' && c <= '9':
		case c == '.':
			hasDot = true
		default:
			return false
		}
	}
	return hasDot
}

// filterExemptSpans drops the spans that must not be redacted and reports how
// many were dropped: an exempt address always, and — for the rule whose pattern
// accepts a \d{1,3} superset (rule 15) — any dotted-quad candidate that is not a
// valid IPv4, which the §3.3 pattern would never have matched.
func filterExemptSpans(b []byte, spans []span, strictQuad bool) ([]span, int) {
	if len(spans) == 0 {
		return spans, 0
	}
	out := spans[:0]
	skipped := 0
	for _, s := range spans {
		if s.end > len(b) {
			continue
		}
		tok := b[s.start:s.end]
		if strictQuad && looksLikeQuad(tok) {
			if _, ok := parseQuad(tok); !ok {
				skipped++
				continue
			}
		}
		if exemptAddr(tok) {
			skipped++
			continue
		}
		out = append(out, s)
	}
	return out, skipped
}
