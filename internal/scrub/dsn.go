package scrub

import (
	"bytes"
	"strings"
)

// DSN handling is rules 3 and 4 (SPEC-02 §3.3, §3.5). Both are kind = dsn_part
// parsers, not patterns: the DSN grammar is
//
//	{scheme}://{pubkey}[:{secret}]@{host}[:{port}]/{project_id}
//
// and the path ends at the base because SDKs append /api/{project_id}/envelope/.
//
// dsn_secret (rule 3) replaces the secret half only, so the surrounding URI
// keeps its diagnostic value; dsn_any (rule 4) normalises everything else:
// query strings, fragments, extra path segments and any userinfo component that
// is not a 32-hex public key are dropped, and a userinfo with no 32-hex public
// key at all becomes [REDACTED:dsn_any].
//
// Two readings of §3.3/§3.5 are fixed here and documented in the handoff:
//
//   - A URI is DSN-shaped for rule 4 when its userinfo's first component is a
//     32-hex public key (or the shield placeholder standing for one), or the
//     scheme is literally "sentry". Without that guard rule 4 would normalise
//     the ordinary basic-auth URL of P10 before rule 6 could attribute it to
//     url_basic_auth, and the normative vector table (P10) could not hold.
//   - A userinfo component that is already a [REDACTED:…] marker is a fixed
//     point: rule 4 leaves the URI alone. That is what makes P11's expected
//     output (scheme://pubkey:[REDACTED:dsn_secret]@host:port/project) a fixed
//     point and keeps hub-side re-scrubbing byte-identical (§6).
func dsnSecretSpans(b []byte, sh *shield) []span {
	var out []span
	forEachURI(b, func(u uri) bool {
		comps := splitUserinfo(u.userinfo(b))
		if len(comps) != 2 {
			return true
		}
		if !isPubKey(comps[0], sh) {
			return true
		}
		if !isHex32(comps[1]) {
			return true
		}
		if isMarker(comps[1]) {
			return true
		}
		out = append(out, span{start: u.userinfoStart + len(comps[0]) + 1, end: u.userinfoStart + len(comps[0]) + 1 + len(comps[1])})
		return true
	})
	return out
}

func dsnAnySpans(b []byte, sh *shield) []span {
	var out []span
	forEachURI(b, func(u uri) bool {
		ui := u.userinfo(b)
		comps := splitUserinfo(ui)
		if len(comps) == 0 || len(ui) == 0 {
			return true
		}
		for _, c := range comps {
			if isMarker(c) {
				return true // fixed point: rule 3 (or a previous pass) already acted
			}
		}
		pub := isPubKey(comps[0], sh)
		if !pub && !strings.EqualFold(u.scheme(b), "sentry") {
			return true // not a DSN: rules 5/6 own ordinary basic-auth URLs
		}
		canon := canonicalDSN(b, u, comps, pub)
		if canon == nil {
			return true
		}
		out = append(out, span{start: u.start, end: u.end, repl: canon})
		return true
	})
	return out
}

// canonicalDSN rebuilds the URI as {scheme}://{pubkey}@{host}[:{port}]/{project}.
// A userinfo without a usable public key becomes the marker.
func canonicalDSN(b []byte, u uri, comps [][]byte, pub bool) []byte {
	var out []byte
	out = append(out, b[u.start:u.schemeEnd]...) // scheme://
	if pub {
		out = append(out, comps[0]...)
	} else {
		out = append(out, "[REDACTED:dsn_any]"...)
	}
	out = append(out, '@')
	out = append(out, b[u.hostStart:u.hostEnd]...)
	if seg := firstPathSegment(b, u); seg != nil {
		out = append(out, '/')
		out = append(out, seg...)
	}
	if bytes.Equal(out, b[u.start:u.end]) {
		return nil // already canonical: byte-identical output, no counter
	}
	return out
}

// uri is one scheme://… token found in the buffer.
type uri struct {
	start, end    int // whole URI
	schemeEnd     int // index one past "://"
	userinfoStart int
	atPos         int // index of '@'
	hostStart     int
	hostEnd       int // exclusive; excludes any path, query or fragment
}

func (u uri) scheme(b []byte) string { return string(b[u.start : u.schemeEnd-3]) }

func (u uri) userinfo(b []byte) []byte { return b[u.userinfoStart:u.atPos] }

// uriStops end a URI token: whitespace, quotes, brackets, separators.
const uriStops = " \t\n\r\"'<>`;,)[]}"

// forEachURI walks every scheme:// token in b. fn receives the parsed token and
// returns whether scanning should continue.
func forEachURI(b []byte, fn func(u uri) bool) {
	for i := 0; i+3 < len(b); i++ {
		if b[i] != ':' || b[i+1] != '/' || b[i+2] != '/' {
			continue
		}
		// scheme start: walk back over the scheme alphabet
		s := i
		for s > 0 && isSchemeChar(b[s-1]) {
			s--
		}
		if s == i || !isSchemeStart(b[s]) {
			continue
		}
		// the URI token runs to the first stop byte
		end := i + 3
		for end < len(b) && !strings.ContainsRune(uriStops, rune(b[end])) {
			end++
		}
		// authority = up to the first '/' after "://"
		authEnd := end
		if j := bytes.IndexByte(b[i+3:end], '/'); j >= 0 {
			authEnd = i + 3 + j
		}
		atRel := bytes.LastIndexByte(b[i+3:authEnd], '@')
		if atRel < 0 {
			continue
		}
		at := i + 3 + atRel
		hostEnd := authEnd
		u := uri{
			start: s, end: end, schemeEnd: i + 3,
			userinfoStart: i + 3, atPos: at,
			hostStart: at + 1, hostEnd: hostEnd,
		}
		if u.hostStart >= u.hostEnd {
			continue
		}
		if !fn(u) {
			return
		}
		i = end - 1
	}
}

func firstPathSegment(b []byte, u uri) []byte {
	if u.hostEnd >= u.end {
		return nil
	}
	p := b[u.hostEnd:u.end]
	if len(p) == 0 || p[0] != '/' {
		return nil
	}
	p = p[1:]
	if j := bytes.IndexAny(p, "/?#"); j >= 0 {
		p = p[:j]
	}
	if len(p) == 0 {
		return nil
	}
	return p
}

func splitUserinfo(ui []byte) [][]byte {
	if len(ui) == 0 {
		return nil
	}
	return bytes.Split(ui, []byte(":"))
}

func isSchemeStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isSchemeChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '+', c == '-', c == '.':
		return true
	}
	return false
}

// isHex32 reports whether tok is exactly 32 lower-case hex characters.
func isHex32(tok []byte) bool {
	if len(tok) != 32 {
		return false
	}
	for _, c := range tok {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// isPubKey reports whether tok is a 32-hex public key or the shield placeholder
// standing for one.
func isPubKey(tok []byte, sh *shield) bool {
	if isHex32(tok) {
		return true
	}
	if _, ok := sh.indexOfPlaceholder(tok); ok {
		return true
	}
	return false
}
