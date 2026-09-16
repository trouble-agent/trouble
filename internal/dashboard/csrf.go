package dashboard

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// CSRF — SPEC-10 §2.3. Four checks, evaluated in order on every POST; the
// first failure returns 403 + TROUBLE-DASHBOARD-004 and no state change.
//
//  1. Ambient-credential rule: a POST carrying Authorization: Bearer AND any
//     Cookie header is refused outright (§2.3.1).
//  2. Origin binding: when Origin is present it must byte-equal
//     public_origin or, for a loopback bind, the request's scheme://Host. An
//     absent Origin + absent Referer is accepted only for the Bearer path.
//  3. Same-Site cookie policy: auth and CSRF cookies are SameSite=Lax (belt
//     to the Origin braces; asserted in tests by header inspection).
//  4. Double-submit + binding: header X-Trouble-CSRF must equal cookie
//     trouble_csrf and equal b64url(hmac_sha256(k_csrf, token_id + "|" +
//     yyyymmddhh)) for that token ID. k_csrf is 32 random bytes generated at
//     process start, held only in memory — a restart invalidates every
//     outstanding value. Current or previous hour accepted.
//
// A Bearer-only POST needs only checks 1–2 (no cookie → no ambient
// authority); the double-submit rule holds vacuously because the browser-only
// cookie half is absent.

const (
	csrfCookieName   = "trouble_csrf"
	csrfHeaderName   = "X-Trouble-CSRF"
	csrfHourLayout   = "2006010215"
	csrfCookieMaxAge = 3600
)

// csrfValue is a derived value plus the hour window it belongs to (§3.1).
type csrfValue struct {
	Value string
	Hour  string
}

// csrfEngine owns k_csrf: 32 random bytes, generated at process start, never
// persisted (§2.3.4).
type csrfEngine struct {
	key [32]byte
}

func newCSRFEngine() (*csrfEngine, error) {
	e := &csrfEngine{}
	if _, err := rand.Read(e.key[:]); err != nil {
		return nil, err
	}
	return e, nil
}

// value derives b64url(hmac_sha256(k_csrf, token_id + "|" + yyyymmddhh)).
func (e *csrfEngine) value(tokenID string, t time.Time) csrfValue {
	h := hmac.New(sha256.New, e.key[:])
	hour := t.UTC().Format(csrfHourLayout)
	h.Write([]byte(tokenID + "|" + hour))
	return csrfValue{Value: base64.RawURLEncoding.EncodeToString(h.Sum(nil)), Hour: hour}
}

// csrfFail is the single refusal builder for §2.3 (detail names the failed
// check so the UI can explain without a second round trip).
func csrfFail(detail string) *dashError {
	return &dashError{Code: types.CodeDashboard004, HTTP: 403, Message: "CSRF check failed", Detail: detail}
}

// check runs the four §2.3 checks in order. p is the authenticated principal
// (its ID is the token label the CSRF value binds to). Returns nil when the
// POST passes. The SameSite policy is enforced by the cookie attributes the
// pages set (check 3 is asserted by header inspection in csrf_test).
func (s *server) csrfCheck(r *http.Request, p principal, now time.Time) *dashError {
	// 1. Ambient-credential rule.
	if bearerToken(r) != "" && len(r.Cookies()) > 0 {
		return csrfFail("mixed_credentials")
	}

	// 2. Origin binding.
	origin := r.Header.Get("Origin")
	referer := r.Header.Get("Referer")
	if origin != "" {
		want := s.cfg.PublicOrigin
		if want == "" && s.loopback {
			want = schemeFor(r) + "://" + r.Host
		}
		if want == "" || !strings.EqualFold(origin, want) {
			return csrfFail("origin_mismatch")
		}
	} else if referer == "" {
		// No Origin, no Referer: accepted only without cookies (the Bearer
		// path; non-browser clients). A browser POST without either header is
		// refused.
		if len(r.Cookies()) > 0 {
			return csrfFail("missing_origin")
		}
	}

	// 3. SameSite — cookie attributes, asserted in tests. No server-side
	// action needed at request time.

	// 4. Double-submit + binding.
	cookieVal := ""
	if c, err := r.Cookie(csrfCookieName); err == nil {
		cookieVal = c.Value
	}
	if cookieVal == "" {
		// Bearer-only path: no cookie half → vacuous pass (§2.3).
		if bearerToken(r) != "" {
			return nil
		}
		return csrfFail("missing_csrf")
	}
	header := r.Header.Get(csrfHeaderName)
	if header == "" {
		return csrfFail("missing_header")
	}
	if !strings.EqualFold(header, cookieVal) {
		return csrfFail("cookie_mismatch")
	}
	// Values from the current or the previous hour are accepted, so a value is
	// valid ≤2 h and a leaked one expires without an operator action (§2.3.4).
	// The binding is to *this* token ID: a value derived for another label
	// matches neither candidate.
	current := s.csrf.value(p.ID, now)
	previous := s.csrf.value(p.ID, now.Add(-time.Hour))
	if !strings.EqualFold(header, current.Value) && !strings.EqualFold(header, previous.Value) {
		return csrfFail("token_binding")
	}
	return nil
}

// csrfCookie is the CSRF cookie the pages set alongside every page render
// (§2.3.4): HttpOnly=false (the template must read it), Secure whenever the
// auth cookie is, Max-Age=3600, no Domain, SameSite=Lax.
func (s *server) csrfCookie(value string, secure bool, now time.Time) *http.Cookie {
	return &http.Cookie{
		Name:     csrfCookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   csrfCookieMaxAge,
		HttpOnly: false,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	}
}

// authCookie is the §2.2 bearer cookie a client presents: Path=/, HttpOnly,
// SameSite=Lax, Secure when the effective scheme is https or the bind is
// non-loopback, Max-Age=43200, no Domain. The dashboard reads it; nothing in
// v0.1 sets it (token creation is CLI-only, §3.2).
func authCookie(value string, secure bool) *http.Cookie {
	return &http.Cookie{
		Name:     cookieAuthName,
		Value:    value,
		Path:     "/",
		MaxAge:   authCookieMaxAge,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	}
}

// schemeFor returns the effective scheme of a request (https when TLS is
// terminated at the listener; the caller decides the loopback fallback).
func schemeFor(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}
