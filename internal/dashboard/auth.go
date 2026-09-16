package dashboard

import (
	"context"
	"net"
	"net/http"
	"strings"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// Authentication — SPEC-10 §2.2/§2.4. The middleware chain per request:
//
//  1. URL-borne token refusal happens in the router wrapper, before anything
//     here (§2.2: evaluated before authentication).
//  2. Live bind re-check: a non-loopback request on a loopback listener is
//     refused 503 + 006 (§2.4).
//  3. Auth-failure throttle: an IP inside its 60s throttle answers 429 + 012.
//  4. Identify via the §2.5 seam; the /health.json loopback exemption skips
//     all of this.
//  5. Scope check → 403 + 003 with X-Trouble-Required-Scope.
//  6. Rate limit (read: token+IP; write: token) → 429 + 012.
//  7. Write routes additionally run the §2.3 CSRF checks (see csrf.go).
//
// Token scope expansion (autonomy ⇒ write ⇒ read) happens once at token load
// in tokens.go.

// authenticate runs steps 2–6 and returns the principal. required == ""
// means the route carries no scope (the loopback /health.json exemption,
// §2.1 row 8 note); authentication is then skipped entirely.
func (s *server) authenticate(r *http.Request, required types.Scope) (principal, *dashError) {
	// Step 2: live bind re-check (§2.4). A listener handed over by a proxy
	// must not silently change its trust zone.
	if s.loopback && !s.loopbackRequest(r) {
		return principal{}, &dashError{Code: types.CodeDashboard006, HTTP: 503, Message: "bind zone mismatch", Detail: "non_loopback_request"}
	}

	ip := s.clientIP(r)
	ipKey := ipString(ip)
	now := s.now()

	// Step 3: auth-failure throttle (§2.8).
	if s.throttle.blocked(ipKey, now) {
		s.counters.rl.Add(1)
		return principal{}, &dashError{Code: types.CodeDashboard012, HTTP: 429, Message: "rate limited", Detail: "auth_failure_throttle"}
	}

	// Loopback /health.json exemption: the one route that runs without auth
	// (§2.1 row 8 note — the read scope on that row is the non-exempt case).
	if s.healthExempt(r) {
		return principal{}, nil
	}

	// Step 4: identify. The store is stat'ed per request; an mtime/size change
	// re-reads it into a new immutable set before this request looks anything
	// up (§3.2, edge case 3: a rotation cannot half-apply within one request).
	s.store.refresh(now)
	if err := s.store.invalid(); err != nil {
		s.counters.denied.Add(1)
		return principal{}, &dashError{Code: types.CodeDashboard013, HTTP: 503, Message: "token store unavailable", Detail: "token_store_invalid"}
	}
	p, err := s.identity.Identify(r)
	if err != nil {
		de := s.errIdentity(err)
		if de.Code == types.CodeDashboard001 || de.Code == types.CodeDashboard002 {
			s.counters.denied.Add(1)
			if s.throttle.fail(ipKey, now) {
				// The failure that crosses the limit throttles the IP.
				s.counters.rl.Add(1)
				return principal{}, &dashError{Code: types.CodeDashboard012, HTTP: 429, Message: "rate limited", Detail: "auth_failure_throttle"}
			}
		}
		return principal{}, de
	}
	s.throttle.success(ipKey)

	// Step 5: scope.
	if required != "" && !p.Scopes.has(required) {
		s.counters.denied.Add(1)
		return principal{}, &dashError{Code: types.CodeDashboard003, HTTP: 403, Message: "insufficient scope"}
	}

	// Step 5b: CSRF on every POST (§2.3, routes 9–11 without exception). This
	// runs after authentication/scope (the checks need the principal the CSRF
	// value is bound to) and before the write bucket: a forged cross-site POST
	// must not consume the operator's write budget.
	if isWriteRoute(r) {
		if de := s.csrfCheck(r, p, now); de != nil {
			s.counters.csrf.Add(1)
			return principal{}, de
		}
	}

	// Step 6: rate limit (only when a limiter is configured).
	if s.limiter != nil {
		var ok bool
		var ra int
		if isWriteRoute(r) {
			ok, ra = s.limiter.writeTake(p.ID, now)
		} else {
			ok, ra = s.limiter.readTake(p.ID, ipKey, now)
		}
		if !ok {
			s.counters.rl.Add(1)
			return principal{}, &dashError{Code: types.CodeDashboard012, HTTP: 429, Message: "rate limited", Detail: "bucket_empty", RetryAfter: ra}
		}
	}

	// Per-request usage stat: LastUsedTS written at most once per 60s (§3.2).
	s.store.markUsed(p.ID, now)
	return p, nil
}

// healthExempt reports whether the request is the loopback /health.json
// exemption (§2.1 row 8 note): bound to loopback, health_loopback_exempt=true,
// and the peer itself on loopback. On a non-loopback bind the exemption is
// forced off and read scope is required.
func (s *server) healthExempt(r *http.Request) bool {
	if !s.cfg.HealthLoopbackExempt || !s.loopback {
		return false
	}
	if r.URL.Path != "/health.json" {
		return false
	}
	return s.loopbackRequest(r)
}

// isWriteRoute reports whether the request targets one of the three POST
// routes (rows 9–11), selecting the write rate bucket.
func isWriteRoute(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	if r.URL.Path == "/api/autonomy" {
		return true
	}
	prefix := "/api/incidents/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		return false
	}
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	n := strings.IndexByte(rest, '/')
	if n < 0 {
		return false
	}
	switch rest[n:] {
	case "/ack", "/close":
		return true
	}
	return false
}

// ipString renders an IP for bucket/throttle keys and log lines.
func ipString(ip net.IP) string {
	if ip == nil {
		return ""
	}
	return ip.String()
}

// ctxKey is the context key the router stamps the principal into so write
// handlers can build the §4.2 Actor without re-authenticating.
type ctxKey struct{}

func withPrincipal(ctx context.Context, p principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

func principalFrom(ctx context.Context) (principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(principal)
	return p, ok
}
