package sentinel

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// DSN is the parsed form of
// `{scheme}://{pubkey}[:{secret}]@{host}[:{port}]/{project_id}` (§2.3).
//
// The DSN is the one artifact every deployed app carries, so parsing is strict
// and generation refuses to mint one that §2.3 forbids.
type DSN struct {
	Scheme    string
	PublicKey string
	Secret    string
	Host      string
	Port      string
	ProjectID string
}

// String renders the DSN back to wire form. The port is omitted when it equals
// the scheme default.
func (d DSN) String() string {
	var b strings.Builder
	b.WriteString(d.Scheme)
	b.WriteString("://")
	b.WriteString(d.PublicKey)
	if d.Secret != "" {
		b.WriteString(":")
		b.WriteString(d.Secret)
	}
	b.WriteString("@")
	b.WriteString(d.Host)
	if d.Port != "" && d.Port != defaultPortFor(d.Scheme) {
		b.WriteString(":")
		b.WriteString(d.Port)
	}
	b.WriteString("/")
	b.WriteString(d.ProjectID)
	return b.String()
}

// PortOr returns the effective port: the explicit one, else the scheme default.
func (d DSN) PortOr() string {
	if d.Port != "" {
		return d.Port
	}
	return defaultPortFor(d.Scheme)
}

// HostPort returns "host:port" for dialing.
func (d DSN) HostPort() string {
	return net.JoinHostPort(d.Host, d.PortOr())
}

// APIBase returns "<scheme>://<host>:<port>/api/<project_id>/" — what an SDK
// appends its route to. §2.3: the DSN path ends at the base, no /api.
func (d DSN) APIBase() string {
	return fmt.Sprintf("%s://%s/api/%s/", d.Scheme, d.HostPort(), d.ProjectID)
}

func defaultPortFor(scheme string) string {
	if scheme == "https" {
		return "443"
	}
	return "80"
}

// GenerateDSN mints a DSN for a project (§2.3). It refuses a host outside the
// advertised set and a non-32-hex key, because both failures are silent in the
// field and loud here (TROUBLE-SENTINEL-009/006).
func GenerateDSN(cfg Config, p types.Project) (string, error) {
	cfg.applyDefaults()
	if _, err := projectIDAtoi(p.ID); err != nil {
		return "", errf(types.CodeSentinel007, "project id is not numeric-as-string 1..2^31-1", causeProjectUnknown)
	}
	if !validPubKey(p.PublicKey) {
		return "", errf(types.CodeSentinel006, "public_key must be exactly 32 lowercase hex", causeSecretLength)
	}
	if p.SecretKey != "" && !validSecretKey(p.SecretKey) {
		return "", errf(types.CodeSentinel006, "secret_key must be exactly 32 lowercase hex (16-hex is refused)", causeSecretLength)
	}
	if net.ParseIP(cfg.AdvertisedHost) != nil || isBindAddress(cfg.AdvertisedHost) {
		return "", errf(types.CodeSentinel009, "advertised_host must be a name, never an IP or bind address", causeAdvertisedHost)
	}
	if cfg.ProxyTrust == proxyTrustNone && cfg.Scheme == "https" {
		return "", errf(types.CodeSentinel009, "https requires a TLS terminator (proxy_trust != none)", causeHTTPSNoTerminator)
	}
	d := DSN{
		Scheme:    cfg.Scheme,
		PublicKey: p.PublicKey,
		Host:      strings.ToLower(cfg.AdvertisedHost),
		ProjectID: p.ID,
	}
	if cfg.RequireSecret {
		if p.SecretKey == "" {
			return "", errf(types.CodeSentinel006, "require_secret = true but the project has no secret_key", causeSecretLength)
		}
		d.Secret = p.SecretKey
	}
	host, _ := parseHostPort(cfg.Bind)
	if host != "" && !isWildcardHost(host) && strings.EqualFold(host, d.Host) {
		d.Host = host
	}
	if p, ok := parseBindPort(cfg.Bind); ok {
		d.Port = p
	}
	return d.String(), nil
}

func isWildcardHost(h string) bool {
	switch h {
	case "0.0.0.0", "::", "":
		return true
	}
	return false
}

func parseBindPort(bind string) (string, bool) {
	_, port := parseHostPort(bind)
	if port == "" {
		return "", false
	}
	if _, err := strconv.Atoi(port); err != nil {
		return "", false
	}
	return port, true
}

// ParseDSN parses a DSN and validates it against the config's advertised host
// set. It is the gate every auth form that carries a full DSN goes through.
func ParseDSN(cfg Config, raw string) (DSN, *Error) {
	cfg.applyDefaults()
	var d DSN
	rest := strings.TrimSpace(raw)
	i := strings.Index(rest, "://")
	if i <= 0 {
		return d, errf(types.CodeSentinel006, "dsn has no scheme", causeAuth)
	}
	d.Scheme = strings.ToLower(rest[:i])
	switch d.Scheme {
	case "http", "https":
	default:
		return d, errf(types.CodeSentinel006, "dsn scheme must be http or https", causeAuth)
	}
	if d.Scheme == "https" && cfg.ProxyTrust == proxyTrustNone {
		return d, errf(types.CodeSentinel009, "https dsn requires a TLS terminator (proxy_trust = none)", causeHTTPSNoTerminator)
	}
	rest = rest[i+3:]
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		return d, errf(types.CodeSentinel006, "dsn has no @host", causeAuth)
	}
	creds := rest[:at]
	rest = rest[at+1:]
	if c := strings.Index(creds, ":"); c >= 0 {
		d.PublicKey = creds[:c]
		d.Secret = creds[c+1:]
	} else {
		d.PublicKey = creds
	}
	if d.Secret != "" && !validSecretKey(d.Secret) {
		return d, errf(types.CodeSentinel006, "dsn secret must be exactly 32 lowercase hex", causeSecretLength)
	}
	if !validPubKey(d.PublicKey) {
		return d, errf(types.CodeSentinel006, "dsn public key must be exactly 32 lowercase hex", causeAuth)
	}
	slash := strings.Index(rest, "/")
	if slash < 0 {
		return d, errf(types.CodeSentinel006, "dsn has no project path", causeAuth)
	}
	authority := rest[:slash]
	path := rest[slash+1:]
	if strings.ContainsAny(path, "?/") {
		return d, errf(types.CodeSentinel006, "dsn path must be exactly /{project_id}: no /api, no trailing slash, no query", causeAuth)
	}
	d.ProjectID = path
	if _, err := projectIDAtoi(d.ProjectID); err != nil {
		return d, errf(types.CodeSentinel006, "dsn project id must be numeric-as-string 1..2^31-1", causeDSNProjectMism)
	}
	if h, p, err := net.SplitHostPort(authority); err == nil {
		d.Host, d.Port = strings.ToLower(h), p
	} else {
		d.Host = strings.ToLower(authority)
	}
	if d.Host == "" {
		return d, errf(types.CodeSentinel006, "dsn host is empty", causeAuth)
	}
	if !cfg.acceptsHost(d.Host) {
		return d, errf(types.CodeSentinel009, "dsn host is not an advertised host", causeAdvertisedHost)
	}
	if d.Port != "" {
		if n, err := strconv.Atoi(d.Port); err != nil || n < 1 || n > 65535 {
			return d, errf(types.CodeSentinel006, "dsn port is invalid", causeAuth)
		}
	}
	return d, nil
}

// DSNForProject renders the canonical DSN of a project from the config; it is
// what `trouble config explain` prints and what `InjectCanary` posts to.
func DSNForProject(cfg Config, p types.Project) (DSN, *Error) {
	raw, err := GenerateDSN(cfg, p)
	if err != nil {
		return DSN{}, wrapErr(err, "generate dsn")
	}
	return ParseDSN(cfg, raw)
}

// ValidKeyPair reports whether both halves of a project's key material are
// well-formed; rotation installs a second public key with a valid_until, and
// §2.3 requires both keys to be 32 hex.
func ValidKeyPair(publicKey, secret string) bool {
	if !validPubKey(publicKey) {
		return false
	}
	return secret == "" || validSecretKey(secret)
}
