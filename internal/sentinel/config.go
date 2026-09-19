package sentinel

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"strconv"
	"strings"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// ProxyTrust values (§2.2).
const (
	proxyTrustLoopback = "loopback"
	proxyTrustNone     = "none"
	proxyTrustExplicit = "explicit-list"
)

// Zones of the bind matrix (§3.7).
const (
	zoneLoopback = "loopback"
	zoneLAN      = "lan"
	zonePublic   = "public"
)

// Defaults from §2.2. A zero Config is completed by applyDefaults, so a caller
// that only wants "the spec defaults" can pass Config{}.
const (
	defaultBind            = "127.0.0.1:7643"
	defaultMaxCompressed   = 204800
	defaultMaxDecompressed = 1048576
	defaultMaxItemBytes    = 262144
	defaultMaxLineBytes    = 65536
	defaultMaxConcurrent   = 64
	defaultPerIPRate       = "600/min, burst 60"
	defaultGroupFlush      = "5s"
	defaultCanaryInterval  = "10m"
	defaultSpoolBudget     = 268435456
	defaultDiskBudget      = 2147483648
	defaultLedgerWait      = "2s"
	defaultReadHeaderTO    = "5s"
	defaultReadTO          = "10s"
	defaultWriteTO         = "10s"
	defaultIdleTO          = "60s"
)

// Config is the resolved [sentinel] block. SPEC-12 §2 owns precedence
// (flag > env > file > default); sentinel never reads the environment or the
// config file itself.
//
// Four fields are additions to the struct sketched in SPEC-04 §2.2, each marked
// [SPEC-12-resolved] below: the spec's Config has no state-root, host or actor
// because SPEC-12 owns those values, yet sentinel cannot write a record without
// them. They are documented as deviations in docs/sentinel-compat.md §5.
type Config struct {
	Bind                     string   // default "127.0.0.1:7643"
	AdvertisedHost           string   // DSN host; a name, never an IP literal or a wildcard. "localhost" only while Bind is loopback (§2.3a)
	AdvertisedHosts          []string // extra names accepted as a valid DSN host
	Scheme                   string   // "http" | "https"; "https" requires ProxyTrust != "none"
	RequireSecret            bool     // default false
	MaxEnvelopeCompressed    int64    // default 204800 (200KB)
	MaxEnvelopeDecompressed  int64    // default 1048576 (1MB)
	MaxItemBytes             int64    // default 262144 (256KB)
	MaxLineBytes             int      // default 65536
	MaxConcurrent            int      // default 64
	ReadTimeout              types.Duration
	WriteTimeout             types.Duration
	IdleTimeout              types.Duration
	ReadHeaderTimeout        types.Duration
	PerIPRate                string         // default "600/min, burst 60"
	GroupFlush               types.Duration // default "5s"
	CanaryInterval           types.Duration // default "10m"
	CanaryProject            string         // project id that carries canaries
	SpoolBudgetBytes         int64          // default 268435456
	DiskBudgetBytes          int64          // default 2147483648
	LossPolicy               types.LossPolicy
	ProxyTrust               string         // "loopback" | "none" | "explicit-list"
	TrustedProxies           []string       // CIDRs, used only when ProxyTrust == "explicit-list"
	AllowQueryKeyNonLoopback bool           // default false
	LedgerWait               types.Duration // default "2s"; ingest backpressure ceiling
	Projects                 []types.Project
	Collectors               CollectorConfig

	// [SPEC-12-resolved] State root for the spool and the collector offsets
	// (§3.5, §3.9 place both inside the pinned state root of SPEC-01 §6.1).
	SpoolDir string
	// [SPEC-12-resolved] Origin identity of the records sentinel writes.
	HostID string
	// [SPEC-12-resolved] Actor stamped on every record sentinel writes.
	Actor types.Actor
	// [SPEC-12-resolved] Zone of the listening socket, used for SourceLiveness
	// and the bind matrix when the peer address is unavailable (e.g. loopback
	// listener behind a proxy).
	Zone string
}

// CollectorConfig is the [sentinel.collector] block (§2.5).
type CollectorConfig struct {
	JournalUnits []string
	FileTails    []string
	Parsers      []types.CollectorParser
}

// Parser returns the configured parser by name.
func (c CollectorConfig) Parser(name string) (types.CollectorParser, bool) {
	for _, p := range c.Parsers {
		if p.Name == name {
			return p, true
		}
	}
	return types.CollectorParser{}, false
}

// applyDefaults fills every zero field with the §2.2 default.
func (c *Config) applyDefaults() {
	if c.Bind == "" {
		c.Bind = defaultBind
	}
	if c.Scheme == "" {
		c.Scheme = "http"
	}
	if c.MaxEnvelopeCompressed == 0 {
		c.MaxEnvelopeCompressed = defaultMaxCompressed
	}
	if c.MaxEnvelopeDecompressed == 0 {
		c.MaxEnvelopeDecompressed = defaultMaxDecompressed
	}
	if c.MaxItemBytes == 0 {
		c.MaxItemBytes = defaultMaxItemBytes
	}
	if c.MaxLineBytes == 0 {
		c.MaxLineBytes = defaultMaxLineBytes
	}
	if c.MaxConcurrent == 0 {
		c.MaxConcurrent = defaultMaxConcurrent
	}
	if c.ReadHeaderTimeout == "" {
		c.ReadHeaderTimeout = defaultReadHeaderTO
	}
	if c.ReadTimeout == "" {
		c.ReadTimeout = defaultReadTO
	}
	if c.WriteTimeout == "" {
		c.WriteTimeout = defaultWriteTO
	}
	if c.IdleTimeout == "" {
		c.IdleTimeout = defaultIdleTO
	}
	if c.PerIPRate == "" {
		c.PerIPRate = defaultPerIPRate
	}
	if c.GroupFlush == "" {
		c.GroupFlush = defaultGroupFlush
	}
	if c.CanaryInterval == "" {
		c.CanaryInterval = defaultCanaryInterval
	}
	if c.SpoolBudgetBytes == 0 {
		c.SpoolBudgetBytes = defaultSpoolBudget
	}
	if c.DiskBudgetBytes == 0 {
		c.DiskBudgetBytes = defaultDiskBudget
	}
	if c.LedgerWait == "" {
		c.LedgerWait = defaultLedgerWait
	}
	if c.ProxyTrust == "" {
		c.ProxyTrust = proxyTrustLoopback
	}
	if c.Zone == "" {
		c.Zone = zoneOf(parseIP(c.Bind))
	}
}

// validate implements the boot validation of §4.2 step 1: advertised host is a
// name, https implies a terminator, every project's keys are 32 hex, quota_epm
// > 0. Failures are loud (TROUBLE-SENTINEL-007/008/009).
func (c *Config) validate() *Error {
	switch c.ProxyTrust {
	case proxyTrustLoopback, proxyTrustNone, proxyTrustExplicit:
	default:
		return errf(types.CodeSentinel009, "proxy_trust must be loopback|none|explicit-list", causeAdvertisedHost)
	}
	switch c.Scheme {
	case "http":
	case "https":
		if c.ProxyTrust == proxyTrustNone {
			return errf(types.CodeSentinel009,
				"https DSN requires a TLS terminator (proxy_trust = none refuses it)", causeHTTPSNoTerminator)
		}
	default:
		return errf(types.CodeSentinel009, "scheme must be http or https", causeAdvertisedHost)
	}
	if err := c.advertisedHostRefusal(c.AdvertisedHost); err != nil {
		return err
	}
	// The accepted set is the union of the two keys (§2.3), so an entry of
	// `advertised_hosts` is a DSN host like any other and meets the same rule:
	// an IP literal, a wildcard and the loopback name outside a loopback bind
	// are refused here too, or the §2.3a precondition is bypassed by the list.
	for _, h := range c.AdvertisedHosts {
		if c.advertisedHostRefusal(h) != nil {
			return errf(types.CodeSentinel009,
				"advertised_hosts entries must be names the reporters resolve: an IP literal, a wildcard address and \"localhost\" outside a loopback bind are refused (SPEC-04 §2.3a)",
				causeAdvertisedHost)
		}
	}
	for _, p := range c.Projects {
		if _, err := projectIDAtoi(p.ID); err != nil {
			return errf(types.CodeSentinel007, "project id must be numeric-as-string 1..2^31-1", causeProjectUnknown)
		}
		if !validPubKey(p.PublicKey) {
			return errf(types.CodeSentinel006, "project public_key must be exactly 32 lowercase hex", causeSecretLength)
		}
		if p.SecretKey != "" && !validSecretKey(p.SecretKey) {
			return errf(types.CodeSentinel006, "project secret_key must be exactly 32 lowercase hex", causeSecretLength)
		}
		if p.QuotaEPM <= 0 {
			return errf(types.CodeSentinel007, "project quota_epm must be > 0", causeProjectUnknown)
		}
		if p.LossPolicy != "" && !p.LossPolicy.Valid() {
			return errf(types.CodeSentinel009, "project loss_policy is not one of sample|drop-with-counter|spool-if-light", causeAdvertisedHost)
		}
	}
	for _, id := range []string{c.CanaryProject} {
		if id == "" {
			continue
		}
		if _, err := projectIDAtoi(id); err != nil {
			return errf(types.CodeSentinel007, "canary_project must be a numeric project id", causeProjectUnknown)
		}
	}
	if c.RequireSecret {
		for _, p := range c.Projects {
			if p.SecretKey == "" {
				return errf(types.CodeSentinel006, "require_secret = true but a project has no secret_key", causeSecretLength)
			}
		}
	}
	if _, _, err := parsePerIPRate(c.PerIPRate); err != nil {
		return errf(types.CodeSentinel009, "per_ip_rate is not \"<n>/min, burst <n>\"", causeAdvertisedHost)
	}
	return nil
}

// isWildcardAddress reports whether host is a wildcard/any-address form a DSN
// must never carry: 0.0.0.0, ::, [::], or the empty string. None of them names
// the listener from a reporter, at any bind.
func isWildcardAddress(host string) bool {
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "0.0.0.0", "::", "[::]", "":
		return true
	}
	return false
}

// isLocalhostName reports whether host is the loopback NAME. It is stated on the
// same normalization acceptsHost uses, so "LocalHost." is the loopback name here
// too and cannot side-step the §2.3a precondition.
func isLocalhostName(host string) bool {
	return normalizeHostName(host) == "localhost"
}

// loopbackBind reports whether the listening address is itself loopback. It is
// the PRECONDITION of §2.3a: a loopback listener is reachable from this host
// only, so every reporter that can reach it resolves the loopback name to it.
func (c Config) loopbackBind() bool {
	host, _ := parseHostPort(c.Bind)
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "localhost" || h == "ip6-localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// advertisedHostRefusal is the ONE rule for a candidate DSN host, consulted at
// config load (validate) and at generation (GenerateDSN, §2.3), so the two
// surfaces cannot drift. It returns nil when the host is acceptable for the
// bind this config listens on:
//
//   - empty, an IP literal, and a wildcard are refused at every bind;
//   - `localhost` is refused unless the bind is loopback (§2.3a): with any
//     other bind a reporter on another host resolves it to itself and reports
//     nowhere — the silent-no-report §2.3 exists to stop — so the refusal names
//     that fix instead of restating the rule.
func (c Config) advertisedHostRefusal(host string) *Error {
	switch {
	case strings.TrimSpace(host) == "":
		return errf(types.CodeSentinel009, "advertised_host is required: a DSN is baked into every app", causeAdvertisedHost)
	case isWildcardAddress(host):
		return errf(types.CodeSentinel009, "advertised_host must be a name, never a wildcard address (0.0.0.0/::)", causeAdvertisedHost)
	case net.ParseIP(host) != nil:
		return errf(types.CodeSentinel009, "advertised_host must be a name, never an IP literal", causeAdvertisedHost)
	case isLocalhostName(host) && !c.loopbackBind():
		bind := c.Bind
		if bind == "" {
			bind = defaultBind
		}
		return errf(types.CodeSentinel009,
			"advertised_host \"localhost\" is refused while ingest.bind "+strconv.Quote(bind)+
				" is not loopback: a reporter on another host resolves localhost to itself and reports nowhere — "+
				"declare a name the reporters resolve, or bind ingest to a loopback address to keep the loopback DSN form (SPEC-04 §2.3a)",
			causeAdvertisedHost)
	}
	return nil
}

// normalizeHostName is the one normalization for a DSN host and for the hosts the
// config advertises: lowercase, no surrounding space, no trailing root dot. Both
// sides go through it (or a DSN minted from `localhost.` would parse to a host
// the same config then refuses), and §2.3a's precondition is stated on it, so
// the dot spelling cannot walk around the loopback rule.
func normalizeHostName(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}

// acceptsHost reports whether host is inside the advertised set.
func (c *Config) acceptsHost(host string) bool {
	h := normalizeHostName(host)
	if h == normalizeHostName(c.AdvertisedHost) {
		return true
	}
	for _, a := range c.AdvertisedHosts {
		if h == normalizeHostName(a) {
			return true
		}
	}
	return false
}

// trustsPeer reports whether the immediate peer may speak for XFF.
func (c *Config) trustsPeer(ip net.IP) bool {
	switch c.ProxyTrust {
	case proxyTrustNone:
		return false
	case proxyTrustLoopback:
		return ip != nil && ip.IsLoopback()
	case proxyTrustExplicit:
		if ip != nil && ip.IsLoopback() {
			return true
		}
		return c.trustsProxy(ip)
	}
	return false
}

// trustsProxy reports whether ip is inside trusted_proxies.
func (c *Config) trustsProxy(ip net.IP) bool {
	if ip == nil {
		return false
	}
	for _, cidr := range c.TrustedProxies {
		_, netw, err := net.ParseCIDR(cidr)
		if err != nil {
			if single := net.ParseIP(strings.TrimSpace(cidr)); single != nil && single.Equal(ip) {
				return true
			}
			continue
		}
		if netw.Contains(ip) {
			return true
		}
	}
	return false
}

// parsePerIPRate parses "600/min, burst 60" (the §2.2 default form) into a rate
// per minute and a burst.
func parsePerIPRate(s string) (rate, burst float64, err error) {
	rate, burst = 600, 60
	s = strings.TrimSpace(s)
	if s == "" {
		return rate, burst, nil
	}
	parts := strings.Split(s, ",")
	for _, p := range parts {
		p = strings.TrimSpace(strings.ToLower(p))
		switch {
		case strings.HasPrefix(p, "burst"):
			v := strings.TrimSpace(strings.TrimPrefix(p, "burst"))
			n, e := strconv.ParseFloat(v, 64)
			if e != nil {
				return 0, 0, e
			}
			burst = n
		case strings.HasSuffix(p, "/min"):
			v := strings.TrimSpace(strings.TrimSuffix(p, "/min"))
			n, e := strconv.ParseFloat(v, 64)
			if e != nil {
				return 0, 0, e
			}
			rate = n
		default:
			n, e := strconv.ParseFloat(p, 64)
			if e != nil {
				return 0, 0, e
			}
			rate = n
		}
	}
	return rate, burst, nil
}

// validPubKey reports whether s is exactly 32 lowercase hex.
func validPubKey(s string) bool { return validHex(s, 32) }

// validSecretKey reports whether s is exactly 32 lowercase hex (16-hex secrets
// are refused: TROUBLE-SENTINEL-006 cause secret_length, §2.3).
func validSecretKey(s string) bool { return validHex(s, 32) }

func validHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// projectIDAtoi parses a project id: numeric-as-string, 1..2^31-1.
func projectIDAtoi(s string) (int64, error) {
	if s == "" || len(s) > 10 {
		return 0, types.ErrBadID
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, types.ErrBadID
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 1 || n > 2147483647 {
		return 0, types.ErrBadID
	}
	return n, nil
}

// NewKey mints a 32-hex key pair member from crypto/rand (§2.3 generation).
func NewKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
