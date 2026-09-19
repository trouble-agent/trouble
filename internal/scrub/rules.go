// Package scrub is the single safety gate between the outside world and every
// byte trouble persists (SPEC-02).
//
// The package is pure: its only dependencies are internal/types and the
// standard library plus a TOML decoder for the [scrub] config subtree. It owns
// the ordered rule table, the per-target byte budgets, the shield for DSN
// public keys and the persistence-boundary re-scan (`Verify`, and the
// SPEC-01-named `MandatoryScan` seam).
package scrub

import (
	"fmt"
	"regexp"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// RulesVersion is the rule-table identity shipped by this binary (SPEC-02 §4).
// It is bumped ONLY when the mandatory set changes; a bump requires a
// ForwardEnvelope.protocol_version bump (SPEC-12 §3.7).
const RulesVersion = 1

// kinds used by the built-in table.
const (
	kindRegex         = types.KindRegex
	kindPrefix        = types.KindPrefix
	kindPathAllowlist = types.KindPathAllowlist
	kindDSNPart       = types.KindDSNPart
	kindEntropy       = types.KindEntropy
)

// The 13 mandatory rule names, in evaluation order (SPEC-02 §3.3). The set is
// frozen: no configuration, flag or build tag removes one of these.
var mandatoryNames = []string{
	"private_key_block", "private_key_inline", "dsn_secret", "dsn_any",
	"conn_string_password", "url_basic_auth", "env_assign", "kv_secret_assign",
	"cli_flag_secret", "auth_header", "bearer_token", "jwt", "cloud_key_shape",
}

// The 5 optional rule names, in evaluation order (SPEC-02 §3.3). Optional
// rules default enabled and may be disabled per host or per project.
var optionalNames = []string{
	"entropy_token", "pii_email_ip", "pii_identity_kv", "path_home_root", "path_disclosure",
}

// RuleTableSize bounds the effective rule set: 13 mandatory + 5 optional + ≤46
// project rules (SPEC-02 §3.2).
const RuleTableSize = 64

// MaxProjectRules is the §3.2 project-rule budget: the table is bounded at 64
// names counting all 5 optional rules, whether or not this host enables them.
const MaxProjectRules = RuleTableSize - 18

// MaxProjectRescanRules caps how many project rules may set rescan = true
// (SPEC-02 §3.6 rule 9).
const MaxProjectRescanRules = 8

// builtinRule is a row of the rule table as compiled from the binary.
type builtinRule struct {
	name       string
	kind       types.RuleKind
	pattern    string // "builtin" for the kind-specific scanners
	replace    string
	mandatory  bool
	targets    []types.ScrubTarget
	begins     []string   // kind = prefix: the literal BEGIN markers
	anchors    []string   // prefilter keywords this rule needs (§3.6 rule 10)
	signals    signalMask // non-literal trigger families of the rule
	extend     extendMode // how a value span longer than RE2's 1000 ceiling is grown
	exemptIP   bool       // loopback/RFC1918 exemption on the matched value (§3.3)
	exemptPath bool       // explicit-path-value exemption on the matched value (§3.3 rule 9)
	strictQuad bool       // spans that look like a dotted quad must be a valid IPv4

	// test-only knobs, reachable only from an in-package injected table (never
	// from configuration: kindTestScrub is not one of the five §3.2 rule kinds).
	testSleep    time.Duration
	testOverflow bool
	testFail     error
}

// privateKeyBeginMarkers are the literal BEGIN markers of rule 1. Case
// sensitive, in SPEC-02 §3.3 order (longest-first inside a family so a match is
// unambiguous).
var privateKeyBeginMarkers = []string{
	"-----BEGIN ENCRYPTED PRIVATE KEY-----",
	"-----BEGIN OPENSSH PRIVATE KEY-----",
	"-----BEGIN PGP PRIVATE KEY BLOCK-----",
	"-----BEGIN RSA PRIVATE KEY-----",
	"-----BEGIN DSA PRIVATE KEY-----",
	"-----BEGIN EC PRIVATE KEY-----",
	"-----BEGIN PRIVATE KEY-----",
	"PuTTY-User-Key-File-",
}

// allTargets is the §3.3 "ALL" target list: the 11 targets of §3.1.
func allTargets() []types.ScrubTarget {
	out := make([]types.ScrubTarget, len(types.ScrubTargets))
	copy(out, types.ScrubTargets)
	return out
}

// optionalTargets returns the target list of an optional rule: the 11 targets
// minus the ones the rule is not defined for.
func optionalTargets(without ...types.ScrubTarget) []types.ScrubTarget {
	out := make([]types.ScrubTarget, 0, len(types.ScrubTargets))
	for _, t := range types.ScrubTargets {
		skip := false
		for _, w := range without {
			if t == w {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, t)
		}
	}
	return out
}

// patterns holds the exact RE2 sources of the regex rules (SPEC-02 §3.3). They
// are written as the spec writes them: exactly one capturing group means "this
// group's span is replaced", zero or ≥2 groups means "the whole match is
// replaced" (§2 capture-group convention).
const (
	patPrivateKeyInline = `(?i)(?:^|[^A-Za-z0-9])(?:private[_-]?key|secret[_-]?key|ssh[_-]?key|key[_-]?material|keystore[_-]?(?:password|pass))\s*[:=]\s*("[^"\n]{16,}"|'[^'\n]{16,}'|[A-Za-z0-9+/=_-]{16,})`

	patConnStringPassword = `(?i)\b(?:postgresql|postgres|mysql|mariadb|mongodb\+srv|mongodb|rediss|redis|amqps|amqp|clickhouse|kafka|mssql|ftps|ftp|ldaps|ldap|memcached)://(?:[^:@/\s]{1,64}):([^@/\s]+)@`

	patURLBasicAuth = `(?i)\b[a-z][a-z0-9+.\-]{1,15}://(?:[^:@/\s]{1,64}):([^@/\s]+)@`

	patEnvAssign = `(?i)(?:^|[^A-Za-z0-9])(?:PASSPHRASE|PASSWORD|PASSWD|PWD|SECRET_KEY|SECRET|API[_-]?KEY|ACCESS[_-]?KEY|PRIVATE[_-]?KEY|CLIENT[_-]?SECRET|SESSION[_-]?KEY|ENCRYPTION[_-]?KEY|SIGNING[_-]?KEY|CONNECTION[_-]?STRING|CONN[_-]?STR|CREDENTIALS|CREDENTIAL|AUTHORIZATION|AUTH|TOKEN|DSN|BEARER)\s*=\s*("[^"\n]*"|'[^'\n]*'|[^\s;,&]+)`

	patKVSecretAssign = `(?i)(?:^|[^A-Za-z0-9])(?:passphrase|password|passwd|pwd|secret_key|secret|api[_-]?key|access[_-]?key|private[_-]?key|client[_-]?secret|session[_-]?key|encryption[_-]?key|signing[_-]?key|token|dsn|connection[_-]?string|conn[_-]?str|credentials?|authorization|auth[_-]?token)["']?\s*[:=]\s*("[^"\n]*"|'[^'\n]*'|[^\s,}\]&;]+)`

	// patCLIFlagSecret is name-driven by construction (SPEC-02 §3.3 rule 9): the
	// flag NAME carries the sensitive word, so the rule sees a path, a token and
	// an environment-variable name as one thing. The value class below is the
	// rule the boundary re-scan of §3.4 uses, and the explicit-path exemption
	// that keeps a token-store path storable is a post-match filter
	// (filterPathExemptSpans, exemptPath) rather than a change to this pattern.
	//
	// The spec writes the leading flag-name fragment as [a-z][a-z0-9_-]{0,31}
	// (mandatory), which cannot match a flag whose name IS the sensitive word
	// (--api-key=…, vector P6): the class consumes the 'a' of "api" and the name
	// alternation then has nothing to match. The fragment is therefore optional
	// here, which accepts the spec's language plus the name-is-the-flag form — a
	// strict superset, and P6 is normative.
	patCLIFlagSecret = `(?i)(?:^|\s)--?(?:[a-z][a-z0-9_\-]{0,29})?(?:password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key)[a-z0-9_\-]{0,8}(?:=|\s+)("[^"\n]*"|[^\s"]+)`

	patAuthHeader = `(?im)^[ \t]*(?:authorization|proxy-authorization|x-api-key|x-auth-token|x-sentry-auth|x-amz-security-token|api-key|cookie|set-cookie)[ \t]*:[ \t]*(.+)$`

	patBearerToken = `(?i)(?:^|[^A-Za-z0-9])bearer\s+([A-Za-z0-9\-._~+/]{8,}={0,2})`

	patJWT = `\beyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]*`

	patCloudKeyShape = `(?i)(?:AKIA[0-9A-Z]{16}|ASIA[0-9A-Z]{16}|gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{22,}|glpat-[A-Za-z0-9_\-]{20,}|xox[baprs]-[A-Za-z0-9\-]{10,}|sk-(?:proj-)?[A-Za-z0-9_\-]{20,}|AIza[0-9A-Za-z_\-]{35}|ya29\.[A-Za-z0-9_\-]{20,}|npm_[A-Za-z0-9]{36,}|pypi-AgEIcHlwaS5vcmc[A-Za-z0-9_\-]{20,}|dckr_pat_[A-Za-z0-9_\-]{20,}|SG\.[A-Za-z0-9_\-]{20,}\.[A-Za-z0-9_\-]{20,}|hf_[A-Za-z0-9]{30,}|tvly-[A-Za-z0-9]{20,}|shpat_[0-9a-f]{32}|eyJr[A-Za-z0-9_\-]{20,})`

	// patPIIEmailIP is a strict superset of the §3.3 pattern: the local part, the
	// label count and the TLD length are unbounded, and the IPv4 alternative is
	// written \d{1,3} per octet. The bounded forms (… {1,64} … {1,8} … and the
	// six-branch octet alternation) unroll to hundreds of RE2 instructions, which
	// measured at 136 µs per KiB; the superset form is a single-digit-µs pass.
	// Invalid octets are dropped post-match (strictQuad), so the accepted language
	// is the spec's.
	patPIIEmailIP = `(?:[A-Za-z0-9._%+\-]+@(?:[A-Za-z0-9\-]+\.)+[A-Za-z]{2,})|(?:\b\d{1,3}(?:\.\d{1,3}){3}\b)`

	patPIIIdentityKV = `(?i)(?:^|[^A-Za-z0-9])(?:user|username|user_name|email|e_mail|ip_address|client_ip|remote_addr|cookie|session_id)["']?\s*[:=]\s*("[^"\n]*"|'[^'\n]*'|[^\s,}\]&;]+)`
)

// builtinTable is the ordered rule table (SPEC-02 §3.3). Order is part of the
// artifact: the ordering semantics of §3.6 depend on it.
//
// `anchors` are the literal keywords the performance prefilter looks for: every
// match of the rule contains at least one of them, which is what makes the
// prefilter a pure fast path (§3.6 rule 10). `signals` cover the trigger
// families that are not literals (an entropy run, an email/IP shape, a path
// token).
var builtinTable = []builtinRule{
	{
		name: "private_key_block", kind: kindPrefix, pattern: "builtin",
		replace: "[REDACTED:private_key_block]", mandatory: true, targets: allTargets(),
		begins:  privateKeyBeginMarkers,
		anchors: []string{"-----BEGIN", "PuTTY-User-Key-File-"},
	},
	{
		name: "private_key_inline", kind: kindRegex, pattern: patPrivateKeyInline,
		replace: "[REDACTED:private_key_inline]", mandatory: true, targets: allTargets(), extend: extInline,
		anchors: []string{"key", "keystore"},
	},
	{
		name: "dsn_secret", kind: kindDSNPart, pattern: "builtin",
		replace: "[REDACTED:dsn_secret]", mandatory: true, targets: allTargets(),
		begins:  []string{"secret"},
		anchors: []string{"://"},
	},
	{
		name: "dsn_any", kind: kindDSNPart, pattern: "builtin",
		replace: "[REDACTED:dsn_any]", mandatory: true, targets: allTargets(),
		begins:  []string{"any"},
		anchors: []string{"://"},
	},
	{
		name: "conn_string_password", kind: kindRegex, pattern: patConnStringPassword,
		replace: "[REDACTED:conn_string_password]", mandatory: true, targets: allTargets(), extend: extURLPass,
		anchors: []string{"://"},
	},
	{
		name: "url_basic_auth", kind: kindRegex, pattern: patURLBasicAuth,
		replace: "[REDACTED:url_basic_auth]", mandatory: true, targets: allTargets(), extend: extURLPass,
		anchors: []string{"://"},
	},
	{
		name: "env_assign", kind: kindRegex, pattern: patEnvAssign,
		replace: "[REDACTED:env_assign]", mandatory: true, targets: allTargets(), extend: extEnvValue,
		anchors: []string{"pass", "pwd", "secret", "key", "auth", "credential", "conn", "token", "dsn", "bearer"},
	},
	{
		name: "kv_secret_assign", kind: kindRegex, pattern: patKVSecretAssign,
		replace: "[REDACTED:kv_secret_assign]", mandatory: true, targets: allTargets(), extend: extKVValue,
		anchors: []string{"pass", "pwd", "secret", "key", "auth", "credential", "conn", "token", "dsn"},
	},
	{
		// The rule that makes a pasted token on argv a hard refusal, and the one
		// that used to refuse a token-store PATH with it: exemptPath is the §3.3
		// rule-9 exemption — a value that is explicitly path-shaped (`/…`, `~/…`,
		// `./…`, `../…`, an absolute one carrying path structure as well) is left
		// unchanged and counted as exempt (SPEC-12 §2.5a; flagvalue.go).
		name: "cli_flag_secret", kind: kindRegex, pattern: patCLIFlagSecret,
		replace: "[REDACTED:cli_flag_secret]", mandatory: true, targets: allTargets(), extend: extFlagValue,
		exemptPath: true,
		anchors:    []string{"pass", "pwd", "secret", "token", "key", "api", "access"},
	},
	{
		name: "auth_header", kind: kindRegex, pattern: patAuthHeader,
		replace: "[REDACTED:auth_header]", mandatory: true, targets: allTargets(),
		anchors: []string{"authorization", "proxy-authorization", "x-api-key", "x-auth-token", "x-sentry-auth", "x-amz-security-token", "api-key", "cookie", "set-cookie"},
	},
	{
		name: "bearer_token", kind: kindRegex, pattern: patBearerToken,
		replace: "[REDACTED:bearer_token]", mandatory: true, targets: allTargets(), extend: extBearer,
		anchors: []string{"bearer"},
	},
	{
		name: "jwt", kind: kindRegex, pattern: patJWT,
		replace: "[REDACTED:jwt]", mandatory: true, targets: allTargets(), extend: extJWT,
		anchors: []string{"eyJ"},
	},
	{
		name: "cloud_key_shape", kind: kindRegex, pattern: patCloudKeyShape,
		replace: "[REDACTED:cloud_key_shape]", mandatory: true, targets: allTargets(), extend: extToken,
		anchors: []string{"AKIA", "ASIA", "ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_", "glpat-", "xox", "sk-", "AIza", "ya29.", "npm_", "pypi-", "dckr_pat_", "SG.", "hf_", "tvly-", "shpat_", "eyJr"},
	},
	{
		name: "entropy_token", kind: kindEntropy, pattern: "builtin",
		replace: "[REDACTED:entropy_token]", mandatory: false,
		targets: optionalTargets(types.TgHeader, types.TgEnv, types.TgDSN),
		extend:  extEntropy,
		signals: sigEntropyRun,
	},
	{
		name: "pii_email_ip", kind: kindRegex, pattern: patPIIEmailIP,
		replace: "[REDACTED:pii_email_ip]", mandatory: false,
		targets:    optionalTargets(types.TgHeader, types.TgEnv, types.TgDSN),
		signals:    sigEmailAt | sigIPv4,
		exemptIP:   true,
		strictQuad: true,
	},
	{
		name: "pii_identity_kv", kind: kindRegex, pattern: patPIIIdentityKV,
		replace: "[REDACTED:pii_identity_kv]", mandatory: false,
		targets:  optionalTargets(types.TgHeader, types.TgDSN),
		extend:   extKVValue,
		exemptIP: true,
		anchors:  []string{"user", "username", "user_name", "email", "e_mail", "ip_address", "client_ip", "remote_addr", "cookie", "session_id"},
	},
	{
		name: "path_home_root", kind: kindPathAllowlist, pattern: "builtin",
		replace: "[REDACTED:path_home_root]", mandatory: false,
		targets: optionalTargets(types.TgHeader, types.TgDSN),
		begins:  []string{"home_root"},
		signals: sigPathSegments,
	},
	{
		name: "path_disclosure", kind: kindPathAllowlist, pattern: "builtin",
		replace: "[REDACTED:path_disclosure]", mandatory: false,
		targets: optionalTargets(types.TgHeader, types.TgDSN),
		begins:  []string{"disclosure"},
		signals: sigPathSegments,
	},
}

// validRuleName: the marker grammar [REDACTED:<name>] must stay unambiguous
// (SPEC-02 §3.2).
var validRuleName = regexp.MustCompile(`^[a-z][a-z0-9_]{2,31}$`)

// compiledRule is one rule of an effective table.
type compiledRule struct {
	index      int // position in the effective table
	name       string
	kind       types.RuleKind
	pattern    string
	replace    []byte
	replaceS   string
	mandatory  bool
	targets    targetMask
	re         *regexp.Regexp
	groups     int
	begins     []string   // kind = prefix markers / path mode
	anchors    []string   // prefilter keywords (union source for a rule table)
	gate       []uint64   // per-rule trigger gate (§3.6 rule 10); nil = always run
	signals    signalMask // non-literal trigger families of the rule
	exemptIP   bool       // loopback/RFC1918 exemption on the matched value (§3.3)
	exemptPath bool       // explicit-path-value exemption on the matched value (§3.3 rule 9)
	strictQuad bool       // spans that look like a dotted quad must be a valid IPv4
	extend     extendMode // value-span extension (RE2 caps repetition at 1000)
	rescan     bool
	builtin    bool

	// test-only hooks (never reachable from configuration).
	testSleep    time.Duration
	testOverflow bool
	testFail     error
}

// targetMask is a bitset over types.ScrubTargets.
type targetMask uint16

func (m targetMask) has(t types.ScrubTarget) bool {
	for i, v := range types.ScrubTargets {
		if v == t {
			return m&(1<<uint(i)) != 0
		}
	}
	return false
}

func maskOf(targets []types.ScrubTarget) targetMask {
	var m targetMask
	for _, t := range targets {
		for i, v := range types.ScrubTargets {
			if v == t {
				m |= 1 << uint(i)
			}
		}
	}
	return m
}

// compileBuiltin turns a builtinRule into a compiledRule (SPEC-02 §3.10: all
// rules compile at New; a compile failure is TROUBLE-SCRUB-001, fatal at boot).
func compileBuiltin(r builtinRule, index int) (*compiledRule, error) {
	c := &compiledRule{
		index:      index,
		name:       r.name,
		kind:       r.kind,
		pattern:    r.pattern,
		replaceS:   r.replace,
		replace:    []byte(r.replace),
		mandatory:  r.mandatory,
		targets:    maskOf(r.targets),
		begins:     append([]string(nil), r.begins...),
		extend:     r.extend,
		anchors:    append([]string(nil), r.anchors...),
		signals:    r.signals,
		exemptIP:   r.exemptIP,
		exemptPath: r.exemptPath,
		strictQuad: r.strictQuad,
		builtin:    true,
	}
	if c.replaceS == "" {
		c.replaceS = "[REDACTED:" + c.name + "]"
		c.replace = []byte(c.replaceS)
	}
	// The per-rule gate is assigned by the engine once the keyword universe is
	// known (assignGates). A rule with no gate always runs.
	switch r.kind {
	case kindRegex:
		re, err := regexp.Compile(r.pattern)
		if err != nil {
			return nil, &Error{Code: types.CodeScrub001, Reason: ReasonRuleCompile,
				Key: r.name, Err: err}
		}
		c.re, c.groups = re, re.NumSubexp()
	case kindTestScrub:
		// test-only: carries its injected behaviour, never compiled from config
		c.testSleep, c.testOverflow, c.testFail = r.testSleep, r.testOverflow, r.testFail
	case kindPrefix, kindEntropy, kindDSNPart, kindPathAllowlist:
		// kind-specific scanners; Pattern is the literal "builtin".
	default:
		return nil, &Error{Code: types.CodeScrub001, Reason: ReasonRuleCompile,
			Key: r.name, Err: fmt.Errorf("unknown rule kind %q", r.kind)}
	}
	return c, nil
}

// mandatorySetComplete reports the first mandatory rule missing from cs
// (SPEC-02 §3.4 point 1).
func mandatorySetComplete(cs []*compiledRule) (string, bool) {
	have := make(map[string]bool, len(cs))
	for _, c := range cs {
		have[c.name] = true
	}
	for _, n := range mandatoryNames {
		if !have[n] {
			return n, false
		}
	}
	return "", true
}
