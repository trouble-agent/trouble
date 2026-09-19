package scrub

import "bytes"

// The rule-9 path-shaped-value exemption (SPEC-02 §3.3 rule 9, SPEC-12 §2.5a).
//
// cli_flag_secret is name-driven: it fires on the flag NAME, so `--hub-token`,
// `--api-key` and `--dashboard-token_file` are one shape to the rule and the
// value that follows is read as a secret whatever it holds. That is the right
// direction for a token — and the wrong one for a path: a token STORE PATH is
// not a secret, and while the refusal stands the daemon cannot be pointed at a
// different store from argv at all (SPEC-12 §2.5a).
//
// The exemption is deliberately an allowlist of EXPLICIT path shapes and not a
// "does this look like a secret" test. A test for the absence of a secret shape
// would accept `--token=hunter2` — exactly the value the mandatory rule exists
// to refuse — so it cannot be the gate. Only a value that starts with `/`,
// `~/`, `./` or `../` AND continues in the path alphabet is left unchanged;
// every other value (a bare `tokens.json`, a relative name, a base64 run, a
// JWT, a token) is redacted exactly as before.

// pathValuePrefixes are the explicit path prefixes a token-store value carries.
// They are the shapes an operator types and a shell expands into a path; a bare
// file name is deliberately absent because it is indistinguishable from a value.
var pathValuePrefixes = [...]string{"/", "~/", "./", "../"}

// isPathValueByte reports whether c may continue a path-shaped value.
//
// The alphabet is deliberately narrower than POSIX allows: `+`, `=`, `:`, `$`,
// `%`, whitespace and every shell metacharacter are absent, so a credential that
// happens to begin with a slash (standard base64 uses `/`) is still redacted
// unless it is also path-clean.
func isPathValueByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '.' || c == '_' || c == '-' || c == '/':
		return true
	}
	return false
}

// pathAlphabet reports whether every byte of b is a path-value byte.
func pathAlphabet(b []byte) bool {
	for _, c := range b {
		if !isPathValueByte(c) {
			return false
		}
	}
	return true
}

// pathShapedValue reports whether tok is explicitly path-shaped: one of the
// prefixes of pathValuePrefixes followed by at least one byte, all of them in
// the path alphabet.
//
// An ABSOLUTE value must also carry path STRUCTURE — a separator or an extension
// after the leading slash (`/srv/tokens.json`, `/tokens.json`). `/` is in the
// credential alphabet of standard base64 (`[A-Za-z0-9+/=]`), so a slash-prefixed
// word with neither (`/tokens`, `/9j4K`) is a credential shape that happens to
// begin with a slash and stays in scope for the rule; the structure requirement
// is what keeps the exemption from admitting it. The shell path idioms `~/`,
// `./` and `../` need no such guard — no credential alphabet of this table
// begins with `~` or with a leading dot — so their remainder only has to stay
// inside the path alphabet. A lone `/` is not a path either way and stays in
// scope for the rule.
func pathShapedValue(tok []byte) bool {
	for _, p := range pathValuePrefixes {
		rest, ok := bytes.CutPrefix(tok, []byte(p))
		if !ok || len(rest) == 0 || !pathAlphabet(rest) {
			continue
		}
		if p == "/" && !bytes.ContainsAny(rest, "/.") {
			continue
		}
		return true
	}
	return false
}

// filterPathExemptSpans drops the spans whose captured value is path-shaped and
// reports how many were dropped. It is the rule-9 counterpart of
// filterExemptSpans (rules 15/16): the value is left unchanged and counted as
// exempt, never redacted.
//
// The quoted alternative of the value class keeps its quotes, so they are
// stripped before the shape test — `--token_file "/srv/tokens.json"` is the same
// value as the bare form.
func filterPathExemptSpans(b []byte, spans []span) ([]span, int) {
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
		if len(tok) >= 2 && (tok[0] == '"' || tok[0] == '\'') && tok[len(tok)-1] == tok[0] {
			tok = tok[1 : len(tok)-1]
		}
		if pathShapedValue(tok) {
			skipped++
			continue
		}
		out = append(out, s)
	}
	return out, skipped
}
