package sentinel

import (
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// Auth form names (SPEC-04 §2.4). They are the values written into
// `payload.auth_form` and into `Project.AuthForms`.
const (
	fmtXSentryAuth  = "x_sentry_auth"
	fmtEnvelopeDSN  = "envelope_dsn"
	fmtQueryKey     = "query_sentry_key"
	fmtGenericQuery = "generic_json_query"
)

// authMaterial is one auth form found on a request.
type authMaterial struct {
	form      string
	key       string
	secret    string
	projectID string // set only by the envelope-dsn form
	hasKey    bool
	hasSecret bool
}

// authOutcome is the resolved identity of an authenticated request.
type authOutcome struct {
	entry   *projectEntry
	form    string
	client  net.IP
	zone    string
	proxied bool
}

// authenticate resolves the request's auth material against the project index,
// applying the §2.4 precedence rules and the §3.7 bind matrix.
//
// envHeader is the envelope header map (nil outside the envelope route): its
// `dsn` key is the only place the envelope_dsn form can come from.
func (s *Server) authenticate(r *http.Request, pathProject string, envHeader map[string]any, now time.Time) (authOutcome, *Error) {
	var out authOutcome
	out.client, out.zone = s.clientIP(r.RemoteAddr, r.Header.Get("X-Forwarded-For"))
	out.proxied = s.cfg.ProxyTrust == proxyTrustExplicit && s.cfg.trustsPeer(parseIP(r.RemoteAddr))

	mats := make([]authMaterial, 0, 3)
	if h := r.Header.Get("X-Sentry-Auth"); h != "" {
		m, err := parseXSentryAuth(h)
		if err != nil {
			return out, err
		}
		if m.hasKey {
			mats = append(mats, m)
		}
	}
	if envHeader != nil {
		if raw, ok := envHeader["dsn"].(string); ok && raw != "" {
			d, err := ParseDSN(s.cfg, raw)
			if err != nil {
				return out, err
			}
			mats = append(mats, authMaterial{
				form: fmtEnvelopeDSN, key: d.PublicKey, secret: d.Secret,
				projectID: d.ProjectID, hasKey: true, hasSecret: d.Secret != "",
			})
		}
	}
	if q := r.URL.Query(); q.Has("sentry_key") || q.Has("sentry_secret") {
		form := fmtQueryKey
		if r.URL.Path != "" && strings.HasSuffix(r.URL.Path, "/event/") {
			form = fmtGenericQuery
		}
		m := authMaterial{form: form, key: q.Get("sentry_key"), secret: q.Get("sentry_secret")}
		m.hasKey = m.key != ""
		m.hasSecret = m.secret != ""
		if m.hasKey || m.hasSecret {
			mats = append(mats, m)
		}
	}

	if len(mats) == 0 {
		return out, errf(types.CodeSentinel005, "no auth material on any accepted form", causeNoAuth)
	}

	// Precedence: x_sentry_auth > envelope_dsn > query. Two forms that resolve
	// to different projects are a 006, never a silent pick.
	primary := mats[0]
	for _, m := range mats[1:] {
		if rank(m.form) < rank(primary.form) {
			primary = m
		}
	}
	entry, err := s.resolveMaterial(primary, pathProject, out.zone, out.proxied, now)
	if err != nil {
		return out, err
	}
	for _, m := range mats {
		if m.form == primary.form {
			continue
		}
		other, oerr := s.resolveMaterial(m, pathProject, out.zone, out.proxied, now)
		if oerr != nil {
			return out, oerr
		}
		if other.proj.ID != entry.proj.ID {
			return out, errf(types.CodeSentinel006, "auth forms resolve to different projects", causeConflictingAuth)
		}
	}
	out.entry = entry
	out.form = primary.form
	return out, nil
}

func rank(form string) int {
	switch form {
	case fmtXSentryAuth:
		return 0
	case fmtEnvelopeDSN:
		return 1
	default:
		return 2
	}
}

// resolveMaterial validates one form and returns the project it names.
func (s *Server) resolveMaterial(m authMaterial, pathProject, zone string, proxied bool, now time.Time) (*projectEntry, *Error) {
	if !m.hasKey {
		return nil, errf(types.CodeSentinel005, "auth material carries no key", causeNoAuth)
	}
	if !validPubKey(m.key) {
		return nil, errf(types.CodeSentinel006, "public key must be exactly 32 lowercase hex", causeAuth)
	}
	if m.hasSecret && m.secret != "" && !validSecretKey(m.secret) {
		return nil, errf(types.CodeSentinel006, "secret must be exactly 32 lowercase hex (16-hex is refused)", causeSecretLength)
	}
	if m.projectID != "" && pathProject != "" && m.projectID != pathProject {
		return nil, errf(types.CodeSentinel006, "the dsn project id differs from the request path", causeDSNProjectMism)
	}
	entry, err := s.projects.byPublicKey(m.key, now)
	if err != nil {
		return nil, err
	}
	if !entry.proj.Enabled {
		return nil, errf(types.CodeSentinel008, "project is disabled", causeProjectDisabled)
	}

	// A key in a query string lands in proxy logs; it is refused on a
	// non-loopback request unless the operator opted in.
	if (m.form == fmtQueryKey || m.form == fmtGenericQuery) && zone != zoneLoopback && !s.cfg.AllowQueryKeyNonLoopback {
		return nil, errf(types.CodeSentinel006, "query-string key refused on a non-loopback request", causeQueryKeyRemote)
	}

	// Bind matrix: off loopback a bare public key needs the project token (or a
	// trusted proxy in front of us).
	if zone != zoneLoopback && !proxied {
		if m.secret == "" || entry.proj.SecretKey == "" || m.secret != entry.proj.SecretKey {
			return nil, errf(types.CodeSentinel006, "off-loopback requests require the project ingest token", causeZoneNotPermitted, causeTokenRequired)
		}
	}
	if s.cfg.RequireSecret {
		if entry.proj.SecretKey == "" || m.secret != entry.proj.SecretKey {
			return nil, errf(types.CodeSentinel006, "require_secret = true and the secret is missing or wrong", causeAuth)
		}
	}
	return entry, nil
}

// parseXSentryAuth parses
// `X-Sentry-Auth: Sentry sentry_version=7, sentry_key=…, sentry_secret=…, …`.
// The scheme token is required; unknown keys are ignored (§2.4).
func parseXSentryAuth(h string) (authMaterial, *Error) {
	m := authMaterial{form: fmtXSentryAuth}
	h = strings.TrimSpace(h)
	sp := strings.IndexAny(h, " \t")
	if sp <= 0 {
		return m, errf(types.CodeSentinel006, "X-Sentry-Auth has no scheme token", causeAuth)
	}
	if !strings.EqualFold(h[:sp], "sentry") {
		return m, errf(types.CodeSentinel006, "X-Sentry-Auth scheme token must be Sentry", causeAuth)
	}
	for _, part := range strings.Split(h[sp+1:], ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		eq := strings.Index(part, "=")
		if eq <= 0 {
			continue
		}
		k := strings.ToLower(strings.TrimSpace(part[:eq]))
		v := strings.TrimSpace(part[eq+1:])
		v = strings.Trim(v, `"`)
		switch k {
		case "sentry_key":
			m.key, m.hasKey = v, v != ""
		case "sentry_secret":
			m.secret, m.hasSecret = v, v != ""
		}
	}
	if !m.hasKey {
		return m, errf(types.CodeSentinel005, "X-Sentry-Auth carries no sentry_key", causeNoAuth)
	}
	return m, nil
}
