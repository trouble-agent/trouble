package dashboard

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/trouble-agent/trouble/internal/types"
)

// Identity seam — SPEC-10 §2.5. The interface is unexported by construction:
// no third party can implement it in v0.1. The "tailnet"/"proxy-header"
// providers are the named seam — no v0.1 implementation ships, and selecting
// one returns 503 + TROUBLE-DASHBOARD-013 at the login boundary. The seam
// exists so T4's real answer to "auth from a phone" (tailnet identity) can
// land as a second Identify implementation without touching the route table,
// the CSRF rules or the actor mapping. (v1.0 hand-off.)

// identityProvider identifies the caller of a request.
type identityProvider interface {
	Name() string // "token" (v0.1) | "tailscale" | "proxy-header"
	Identify(r *http.Request) (principal, error)
}

// principal is an authenticated caller: the token label (which becomes
// Actor.ID on writes, §4.2) and the expanded scope set.
type principal struct {
	ID     string
	Scopes scopeSet
}

// Identity error classes. The error → dashError mapping is the only place the
// §5 codes are attached, so every refusal path builds one dashError.
var (
	// errNoAuthMap: no Bearer header and no trouble_dash cookie (→ 401 + 001).
	errNoAuthMaterial = errors.New("no auth material")
	// errBadAuthMap: material present but unknown/revoked/malformed/wrong
	// length, different values on two carriers, or equal to an ingestion key
	// (→ 401 + 002, counted by the throttle).
	errBadAuth = errors.New("auth material invalid")
	// errIdentityAbsent: the configured identity provider is the named seam
	// with no v0.1 implementation (→ 503 + 013, detail impl_absent).
	errIdentityAbsent = errors.New("identity provider not implemented")
	// errStoreInvalidSignal: fail-closed token store (→ 503 + 013,
	// detail token_store_invalid). Computed per request from the store state.
)

const (
	cookieAuthName   = "trouble_dash" // §2.2 carrier 2; host-only, Lax
	authCookieMaxAge = 43200          // Max-Age seconds (§2.2)
)

// tokenIdentity is the v0.1 identityProvider: the 0600 token store (§3.2).
type tokenIdentity struct {
	store *TokenStore
}

func (t *tokenIdentity) Name() string { return "token" }

// Identify extracts the token from its two carriers, hashes it and looks it
// up constant-time against the store. Never returns the plaintext.
func (t *tokenIdentity) Identify(r *http.Request) (principal, error) {
	bearer := bearerToken(r)
	cookie := ""
	if c, err := r.Cookie(cookieAuthName); err == nil {
		cookie = c.Value
	}
	switch {
	case bearer == "" && cookie == "":
		return principal{}, errNoAuthMaterial
	case bearer != "" && cookie != "" && bearer != cookie:
		// Two carriers with different values → 002 (§2.2).
		return principal{}, errBadAuth
	}
	plaintext := bearer
	if plaintext == "" {
		plaintext = cookie
	}
	if !tokenGrammar.MatchString(plaintext) || len(plaintext) != tokenPlainLen {
		return principal{}, errBadAuth
	}
	entry, ok := t.store.lookup(plaintext)
	if !ok || entry.Revoked {
		return principal{}, errBadAuth
	}
	// v0.1 tokens carry no project dimension (SPEC-INDEX §6.4): no per-token
	// project check here. §2.4's project_scope rule is configuration-time.
	return principal{ID: entry.ID, Scopes: entry.scopes}, nil
}

// bearerToken returns the Authorization: Bearer value, "" when absent.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
}

// throttleKey is the identity the §2.8a auth-failure throttle counts against:
// the credential THIS request presents when it carries exactly one value that
// satisfies the token grammar, and the client IP otherwise (no material at all,
// a value that cannot be a token, or two carriers that disagree).
//
// Two consequences are load-bearing. A credential is throttled only by its own
// failures, so a valid token is never refused by failures it did not make —
// the defect TRBL-010 fixed, where a mistyped or rotated token throttled the
// whole client IP. And a request that presents no grammar-valid credential
// still counts against its client IP, so an unauthenticated flood stays
// throttled instead of being handed to the identity seam.
//
// The key is a truncated sha256 of the presented value: the plaintext is never
// held in the map, never logged and never rendered, exactly like the store's
// own token hashes.
func throttleKey(r *http.Request, ipKey string) string {
	bearer := bearerToken(r)
	cookie := ""
	if c, err := r.Cookie(cookieAuthName); err == nil {
		cookie = c.Value
	}
	switch {
	case bearer == "" && cookie == "":
		return "ip:" + ipKey
	case bearer != "" && cookie != "" && bearer != cookie:
		// Two carriers, two values (§2.2): the failure is the malformed
		// carrier pair, not one named credential.
		return "ip:" + ipKey
	}
	value := bearer
	if value == "" {
		value = cookie
	}
	if !tokenGrammar.MatchString(value) || len(value) != tokenPlainLen {
		return "ip:" + ipKey
	}
	sum := sha256.Sum256([]byte(value))
	return "cred:" + hex.EncodeToString(sum[:8])
}

// identityFor selects the provider per cfg.Identity (§2.5). Selecting the
// named-but-absent providers is not a boot error: they fail 503 + 013 at the
// login boundary.
func identityFor(cfg Config, store *TokenStore) identityProvider {
	switch cfg.Identity {
	case "token", "":
		return &tokenIdentity{store: store}
	case "tailscale", "proxy-header":
		return &absentIdentity{name: cfg.Identity}
	default:
		return &absentIdentity{name: cfg.Identity}
	}
}

// absentIdentity is the named seam with no v0.1 implementation: every
// Identify fails 503 + TROUBLE-DASHBOARD-013 (detail impl_absent, §2.5).
type absentIdentity struct{ name string }

func (a *absentIdentity) Name() string { return a.name }

func (a *absentIdentity) Identify(r *http.Request) (principal, error) {
	return principal{}, errIdentityAbsent
}

// clientIP resolves the peer identity used by the rate limiter, the throttle
// and every log line (§2.4): X-Forwarded-For is honored only when
// proxy_trusted=true and the peer address is inside proxy_cidrs; an untrusted
// client can never forge its own bucket.
func (s *server) clientIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil
	}
	if s.cfg.ProxyTrusted && len(s.cfg.ProxyCIDRs) > 0 && cidrsContain(s.cfg.ProxyCIDRs, ip) {
		if xff := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0]); xff != "" {
			if fip := net.ParseIP(xff); fip != nil {
				return fip
			}
		}
	}
	return ip
}

func cidrsContain(cidrs []string, ip net.IP) bool {
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			continue
		}
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// loopbackRequest reports whether the request's peer is on the loopback
// interface (after the trusted-proxy XFF resolution).
func (s *server) loopbackRequest(r *http.Request) bool {
	return ipIsLoopback(s.clientIP(r))
}

// errIdentity maps the identity errors onto their §5 dashErrors.
func (s *server) errIdentity(err error) *dashError {
	switch {
	case errors.Is(err, errNoAuthMaterial):
		return &dashError{Code: types.CodeDashboard001, HTTP: 401, Message: "authentication required"}
	case errors.Is(err, errBadAuth):
		return &dashError{Code: types.CodeDashboard002, HTTP: 401, Message: "authentication failed"}
	case errors.Is(err, errIdentityAbsent):
		return &dashError{Code: types.CodeDashboard013, HTTP: 503, Message: "identity provider unavailable", Detail: "impl_absent"}
	default:
		return &dashError{Code: types.CodeDashboard013, HTTP: 503, Message: "token store unavailable", Detail: "token_store_invalid"}
	}
}
