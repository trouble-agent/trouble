package scrub

import (
	"bytes"
	"regexp"
	"strconv"
)

// span is a half-open byte range of the working buffer that a rule replaces.
// repl, when non-empty, overrides the rule's marker for this one span: the
// dsn_part parsers need it because rule 4 normalises a URI in place rather than
// replacing it with a single marker (SPEC-02 §3.5).
type span struct {
	start, end int
	repl       []byte
}

// markerRE recognises a replacement marker, optionally wrapped in one pair of
// quotes (rules 2/7/8/9/16 capture the quoted value together with its quotes).
// Markers are fixed points of the whole rule table (SPEC-02 §6): a rule that
// would "match" its own output must not count it.
var markerRE = regexp.MustCompile(`^\[REDACTED:[a-z][a-z0-9_]{2,31}\]$|^\[SCRUB-TRUNCATED:[0-9]{1,12}\]$`)

func isMarker(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	if len(b) >= 2 {
		if (b[0] == '"' && b[len(b)-1] == '"') || (b[0] == '\'' && b[len(b)-1] == '\'') {
			b = b[1 : len(b)-1]
		}
	}
	return markerRE.Match(b)
}

// growMarkerSpan grows a span whose text begins a marker but stops short of the
// marker's closing bracket. Rules 8, 9 and 16 exclude `]` and `}` from their
// value class (SPEC-02 §3.3), so an already-redacted value such as
// {"token": [REDACTED:kv_secret_assign]} matches as "[REDACTED:kv_secret_assign"
// — without this the rule would "match" its own output and break the fixed-point
// property of §6 (the second pass must report Redactions == 0).
func growMarkerSpan(b []byte, s span) span {
	x := b[s.start:s.end]
	q := byte(0)
	if len(x) > 0 && (x[0] == '"' || x[0] == '\'') {
		q = x[0]
		x = x[1:]
	}
	if !bytes.HasPrefix(x, []byte("[REDACTED:")) && !bytes.HasPrefix(x, []byte("[SCRUB-TRUNCATED:")) {
		return s
	}
	if bytes.IndexByte(x, ']') >= 0 {
		return s
	}
	end := s.end
	for i := 0; i < 64 && end < len(b); i++ {
		c := b[end]
		end++
		if c == ']' {
			if q != 0 && end < len(b) && b[end] == q {
				end++
			}
			return span{start: s.start, end: end}
		}
	}
	return s
}

// containsMarkerText reports whether x contains a replacement marker.
func containsMarkerText(x []byte) bool {
	return bytes.Contains(x, []byte("[REDACTED:")) || bytes.Contains(x, []byte("[SCRUB-TRUNCATED:"))
}

// withinMarker reports whether the span lies inside a [REDACTED:…] marker
// without covering the whole token: a rule whose value class splits a marker
// (rules 5/6 read `[REDACTED:dsn_any]` as user "…REDACTED" + secret "dsn_any]")
// must not rewrite it, or the fixed-point property of §6 breaks on the second
// pass. A value that merely sits inside brackets ([abc]) is not a marker and is
// still redacted.
func withinMarker(b []byte, s span) bool {
	const window = 64
	start := s.start
	for start > 0 && s.start-start < window {
		c := b[start-1]
		if c == '[' {
			break
		}
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			return false
		}
		start--
	}
	if start == 0 || b[start-1] != '[' {
		return false
	}
	if b[s.end-1] == ']' && markerRE.Match(b[start-1:s.end]) {
		return true
	}
	for j := s.end; j < len(b) && j-s.end < window; j++ {
		switch b[j] {
		case ']':
			return markerRE.Match(b[start-1 : j+1])
		case ' ', '\t', '\n', '\r', '[', '"', '\'':
			return false
		}
	}
	return false
}

// applySpans replaces the given (sorted, non-overlapping) spans with repl (or
// with the span's own replacement when it carries one).
func applySpans(buf []byte, spans []span, repl []byte) []byte {
	if len(spans) == 0 {
		return buf
	}
	out := make([]byte, 0, len(buf)+len(spans)*(len(repl)+1))
	prev := 0
	for _, s := range spans {
		if s.start < prev || s.end > len(buf) || s.start >= s.end {
			continue
		}
		rep := repl
		if len(s.repl) > 0 {
			rep = s.repl
		}
		out = append(out, buf[prev:s.start]...)
		out = append(out, rep...)
		prev = s.end
	}
	out = append(out, buf[prev:]...)
	return out
}

// overlaps reports whether a and b share at least one byte.
func overlaps(a span, spans []span) bool {
	for _, s := range spans {
		if a.start < s.end && s.start < a.end {
			return true
		}
	}
	return false
}

// extendMode says how a rule's value span is grown to cover a value longer than
// the repetition ceiling of Go's RE2 implementation.
//
// Go's regexp caps a repetition count at 1000, so the normative patterns of
// SPEC-02 §3.3 ({1,8192} value classes and friends) cannot be compiled verbatim;
// they are written with {1,1000} and the span is then extended over the same
// alphabet the value class describes. Extension is a no-op for every value
// shorter than the ceiling, which is why the vector suite is unaffected, and it
// is what keeps a 4 KiB password from being half-redacted.
type extendMode uint8

const (
	extNone extendMode = iota
	extEnvValue
	extKVValue
	extFlagValue
	extBearer
	extJWT
	extToken
	extURLPass
	extInline
	extEntropy
)

const (
	setEnvStop   = " \t\r\n;,&"
	setKVStop    = " \t\r\n,}]&;"
	setFlagStop  = " \t\r\n\""
	setBearer    = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~+/="
	setJWT       = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-."
	setToken     = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-"
	setInline    = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/=_-"
	setEntropy   = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-+/"
	setURLPassNo = "@/\t\r\n \\"
)

func inByteSet(c byte, set string) bool {
	for i := 0; i < len(set); i++ {
		if set[i] == c {
			return true
		}
	}
	return false
}

// extendSpan grows s over the alphabet of mode. A span that starts with a quote
// is grown to the closing quote instead (the quoted alternative of the rule).
func extendSpan(b []byte, s span, mode extendMode) span {
	if mode == extNone || s.start >= s.end || s.end > len(b) {
		return s
	}
	x := b[s.start:s.end]
	if q := x[0]; q == '"' || q == '\'' {
		if x[len(x)-1] == q {
			return s
		}
		for i := s.end; i < len(b); i++ {
			if b[i] == q {
				return span{start: s.start, end: i + 1}
			}
			if b[i] == '\n' {
				return span{start: s.start, end: i}
			}
		}
		return span{start: s.start, end: len(b)}
	}
	end := s.end
	switch mode {
	case extBearer:
		for end < len(b) && inByteSet(b[end], setBearer) {
			end++
		}
	case extJWT:
		for end < len(b) && inByteSet(b[end], setJWT) {
			end++
		}
	case extToken:
		for end < len(b) && inByteSet(b[end], setToken) {
			end++
		}
	case extInline:
		for end < len(b) && inByteSet(b[end], setInline) {
			end++
		}
	case extEntropy:
		for end < len(b) && inByteSet(b[end], setEntropy) {
			end++
		}
	case extURLPass:
		for end < len(b) && !inByteSet(b[end], setURLPassNo) {
			end++
		}
	case extFlagValue:
		for end < len(b) && !inByteSet(b[end], setFlagStop) {
			end++
		}
	case extKVValue:
		for end < len(b) && !inByteSet(b[end], setKVStop) {
			end++
		}
	case extEnvValue:
		for end < len(b) && !inByteSet(b[end], setEnvStop) {
			end++
		}
	}
	return span{start: s.start, end: end}
}

// regexSpans extracts the replacement spans of a regex rule, applying the
// capture-group convention of SPEC-02 §2 (exactly one group ⇒ that group's span
// is replaced; zero or ≥2 groups ⇒ the whole match is replaced), the
// marker-skip rule of §6, and the reserved-span deferral that makes rules 7/8/16
// yield to the structured rules 9/10 (§3.6 rules 5 and 9).
func (r *compiledRule) regexSpans(b []byte, reserved []span) []span {
	if r.re == nil {
		return nil
	}
	ms := r.re.FindAllSubmatchIndex(b, -1)
	if len(ms) == 0 {
		return nil
	}
	out := make([]span, 0, len(ms))
	for _, m := range ms {
		var s span
		if r.groups == 1 && len(m) >= 4 && m[2] >= 0 && m[3] >= m[2] {
			s = span{start: m[2], end: m[3]}
		} else {
			s = span{start: m[0], end: m[1]}
		}
		if s.start >= s.end { // zero-length match: nothing to replace
			continue
		}
		s = growMarkerSpan(b, s)
		if isMarker(b[s.start:s.end]) || withinMarker(b, s) {
			continue
		}
		if containsMarkerText(b[s.start:s.end]) {
			// a more specific rule already redacted inside this span: a
			// name-driven value rule must not swallow it (that would destroy the
			// attribution and the diagnostic remainder, §3.6 rule 9's intent)
			continue
		}
		s = extendSpan(b, s, r.extend)
		if len(reserved) > 0 && overlaps(s, reserved) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// prefixSpans finds private-key blocks (rule 1). The span starts at the literal
// BEGIN marker and ends at the first following line whose first 9 bytes are
// "-----END "; a PuTTY block ends at the first blank line; with neither, the
// span runs to the end of the input (SPEC-02 §3.3 rule 1).
func prefixSpans(b []byte, begins []string) []span {
	var out []span
	pos := 0
	for pos < len(b) {
		idx := -1
		marker := ""
		for _, m := range begins {
			if i := bytes.Index(b[pos:], []byte(m)); i >= 0 {
				at := pos + i
				if idx < 0 || at < idx || (at == idx && len(m) > len(marker)) {
					idx, marker = at, m
				}
			}
		}
		if idx < 0 {
			break
		}
		end := blockEnd(b, idx, marker)
		if end <= idx {
			end = len(b)
		}
		out = append(out, span{start: idx, end: end})
		pos = end
	}
	return out
}

// blockEnd returns the exclusive end of the private-key block starting at idx.
func blockEnd(b []byte, idx int, marker string) int {
	if marker == "PuTTY-User-Key-File-" {
		// a PuTTY block ends at the first blank line
		for i := idx; i < len(b); {
			nl := bytes.IndexByte(b[i:], '\n')
			lineEnd := len(b)
			if nl >= 0 {
				lineEnd = i + nl + 1
			}
			line := b[i:lineEnd]
			if isBlankLine(line) {
				if lineEnd > i && b[lineEnd-1] == '\n' {
					return lineEnd - 1
				}
				return lineEnd
			}
			if nl < 0 {
				return len(b)
			}
			i = lineEnd
		}
		return len(b)
	}
	for i := idx; i < len(b); {
		nl := bytes.IndexByte(b[i:], '\n')
		lineEnd := len(b)
		if nl >= 0 {
			lineEnd = i + nl + 1
		}
		if bytes.HasPrefix(b[i:lineEnd], []byte("-----END ")) {
			// the END line's newline stays outside the span so the marker does
			// not glue the following line onto this one
			if lineEnd > i && b[lineEnd-1] == '\n' {
				return lineEnd - 1
			}
			return lineEnd
		}
		if nl < 0 {
			return len(b)
		}
		i = lineEnd
	}
	return len(b)
}

func isBlankLine(line []byte) bool {
	for _, c := range line {
		if c != '\n' && c != '\r' && c != ' ' && c != '\t' {
			return false
		}
	}
	return true
}

// truncationMarker renders "[SCRUB-TRUNCATED:<dropped_bytes>]" (SPEC-02 §3.1).
func truncationMarker(dropped int) []byte {
	b := make([]byte, 0, 28)
	b = append(b, "[SCRUB-TRUNCATED:"...)
	b = strconv.AppendInt(b, int64(dropped), 10)
	b = append(b, ']')
	return b
}

// lastNewlineWithin returns the index one past the last '\n' inside b[:limit],
// or 0 when there is none: the tail is then a partial line and is dropped in
// full (SPEC-02 §3.1).
func lastNewlineWithin(b []byte, limit int) int {
	if limit > len(b) {
		limit = len(b)
	}
	if i := bytes.LastIndexByte(b[:limit], '\n'); i >= 0 {
		return i + 1
	}
	return 0
}
