package scrub

import (
	"bytes"
	"regexp"
	"strings"
)

// pathTokenRE is the shared path token grammar of rules 17 and 18 (SPEC-02
// §3.3): exactly one capturing group, the path itself.
//
// The spec writes the trailing segments as `(?:/seg{1,255}){0,15}`, which Go's
// RE2 rejects: regexp/syntax caps the product of nested repeat counts at 1000
// (255×15 = 3825). The equivalent unrolled form below accepts the same language
// — one leading segment plus up to 15 optional `/seg` groups — and compiles.
var pathTokenRE = regexp.MustCompile(pathTokenPattern())

// pathTokenPattern builds the spec's token grammar with the trailing segments
// unrolled (Go's RE2 caps the product of nested repeat counts at 1000).
func pathTokenPattern() string {
	const seg = `[A-Za-z0-9._\-]{1,255}`
	const ctx = "(?:^|[\\s\"'\\x60(=:,])"
	return ctx + "(/" + seg + strings.Repeat("(?:/"+seg+")?", 15) + ")"
}

// pathSpans implements rules 17 (path_home_root) and 18 (path_disclosure).
//
// The token's leading segments are matched against the configured home roots
// ("/home/*", "/Users/*", "/root", "/var/home/*"); a match is replaced by the
// marker and the remainder of the path is emitted verbatim — the only rule in
// the table that keeps part of the original text (SPEC-02 §8).
//
// path_disclosure replaces a whole path token of ≥3 segments whose prefix is
// not in the configured allowlist. It is enabled only when path_mode = "redact"
// (rule 18 defaults off).
func pathSpans(b []byte, mode string, allowlist, homeRoots []string) []span {
	ms := pathTokenRE.FindAllSubmatchIndex(b, -1)
	if len(ms) == 0 {
		return nil
	}
	var out []span
	for _, m := range ms {
		if len(m) < 4 || m[2] < 0 || m[3] < 0 {
			continue
		}
		tok := b[m[2]:m[3]]
		switch mode {
		case pathModeHomeRoot:
			if pre := homeRootPrefix(tok, homeRoots); pre > 0 {
				out = append(out, span{start: m[2], end: m[2] + pre})
			}
		default: // chainDisclosure
			if !pathAllowed(tok, allowlist) && len(pathSegments(tok)) >= 3 {
				out = append(out, span{start: m[2], end: m[3]})
			}
		}
	}
	return out
}

const (
	pathModeHomeRoot   = "home_root"
	pathModeDisclosure = "disclosure"
)

// DefaultPathAllowlist is the §3.2 default of path_allowlist.
var DefaultPathAllowlist = []string{"/srv/src", "/opt/apps", "/usr/lib", "/var/log"}

// DefaultHomeRoots is the §3.2 default of home_roots.
var DefaultHomeRoots = []string{"/home/*", "/Users/*", "/root", "/var/home/*"}

// pathSegments splits "/a/b/c" into ["a","b","c"] (no empty segments).
func pathSegments(p []byte) []string {
	if len(p) == 0 || p[0] != '/' {
		return nil
	}
	return strings.Split(strings.Trim(string(p), "/"), "/")
}

// homeRootPrefix returns the byte length of the home-root prefix of tok that
// must be replaced, or 0 when tok is not under a configured home root. A "*"
// in a home root stands for exactly one segment (the user name).
func homeRootPrefix(tok []byte, roots []string) int {
	best := 0
	for _, r := range roots {
		r = strings.TrimRight(r, "/")
		if r == "" {
			continue
		}
		if strings.HasSuffix(r, "/*") {
			base := strings.TrimSuffix(r, "*") // keeps the trailing '/'
			if !bytes.HasPrefix(tok, []byte(base)) {
				continue
			}
			rest := tok[len(base):]
			if len(rest) == 0 {
				continue
			}
			seg := rest
			if i := bytes.IndexByte(rest, '/'); i >= 0 {
				seg = rest[:i]
			}
			if len(seg) == 0 {
				continue
			}
			if n := len(base) + len(seg); n > best {
				best = n
			}
			continue
		}
		if !bytes.Equal(tok, []byte(r)) && !bytes.HasPrefix(tok, []byte(r+"/")) {
			continue
		}
		if len(r) > best {
			best = len(r)
		}
	}
	return best
}

// pathAllowed reports whether tok is under one of the allowlisted prefixes.
func pathAllowed(tok []byte, allowlist []string) bool {
	for _, a := range allowlist {
		a = strings.TrimRight(a, "/")
		if a == "" {
			continue
		}
		if bytes.Equal(tok, []byte(a)) || bytes.HasPrefix(tok, []byte(a+"/")) {
			return true
		}
	}
	return false
}
