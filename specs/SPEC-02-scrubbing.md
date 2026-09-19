# SPEC-02 — scrubbing subsystem (trouble v0.1)

Spec: SPEC-02
Area prefix: TROUBLE-SCRUB
Package: internal/scrub
Consumed types: ScrubRule, ScrubResult, ScrubTarget, ScrubStats, Record, GroupCounters, Project, ForwardEnvelope
Local types: engine, compiledRule, shield, passStats
ACs: AC-18, AC-22
PRD: §11, §04b

## 1. Purpose

`internal/scrub` is the single safety gate between the outside world and every byte trouble persists.
The pipeline is **ingest → scrub → ledger**: no subsystem writes to disk, to the ledger, to the spool, to
the skills-local directory, to an issue driver, or to a board row before its content has passed through
this package. Both quorum judges called the absence of this subsystem the highest-liability omission in
the PRD for one reason: the ledger is append-only, git-distributed and auto-filed to GitHub, so a secret
that reaches it is **unauditable after the fact** — it cannot be removed, only tombstoned, and it will
already have been copied into a git object, an issue body and a board row. `prd-v2.3.html` asserts
"secrets never in the ledger (DSN public keys excepted — designed public)" as a promise; this spec is the
mechanism.

Normative invariants (each is a conformance test in §7):

1. **Scrub before sig.** Every signature, fingerprint, dedup key and group key is computed over the
   *scrubbed* bytes, never the raw bytes. One normalization point means two hosts and three arrival paths
   that describe the same bug produce the same digest (AC-22), and a secret never enters a digest input.
2. **Idempotence.** `Scrub(Scrub(x)) == Scrub(x)` byte-for-byte, and the second call reports
   `Redactions == 0` for the built-in rules. This is what lets a satellite scrub locally and the hub
   re-scrub the same record on receipt without changing the sig (AC-18).
3. **The only unredacted credential in the system is a DSN public key** (`Project.PublicKey`, 32 hex). No
   other credential — DSN secret half, bearer token, API key, private key, connection-string password,
   dashboard token, cookie — may appear in any persisted byte, in any counter, in any error message, or
   in any log line.
4. **Counts, never values.** Counters are emitted per rule (`by_rule`) and per record (`Record.Redactions`).
   The redacted original is never returned, buffered, logged, counted by value, or written to a debug file.
5. **Fail closed.** A rule that errors or times out, input that is not valid UTF-8 on a text target, or a
   persistence-boundary re-scan hit ⇒ the payload is **not persisted** (in whole or in part), the event is
   counted as dropped, and the caller emits a `gap` record. Availability loses to leakage, deliberately.
6. **Redaction tests are conformance tests.** The vector suite and the seeded-secret end-to-end test run
   in CI and are CI-fatal (a failing redaction test blocks merge the same way TROUBLE-REGISTRY-013 does).

Scrub applies to **bundles** (journal tails, SDK payloads, config snapshots) and to every artifact
**derived** from them (skill candidates, issue bodies, board rows, research context, spool bytes, forward
envelopes). A derived artifact that skips the scrubber is a defect, not a choice: §4 names every call site
and the ledger writer independently refuses non-conforming records.

## 2. Interface

**No HTTP routes.** This package serves no port and owns no route; the route tables of SPEC-04 and SPEC-10
are unaffected. Every other area's HTTP surface passes bytes *through* these functions.

```go
package scrub

// Engine is immutable after New. Safe for concurrent use by any number of goroutines.
type Engine struct { /* compiled rule table, per-project overrides, shield table, atomic counters */ }

// New compiles the built-in rule table (18 rules, order = §3.3), strict-decodes the resolved [scrub]
// config subtree, resolves per-project redaction overrides, and registers every project DSN public key
// in the shield table. Returns TROUBLE-SCRUB-001 (rule failed to compile), TROUBLE-SCRUB-002 (config
// invalid: unknown key, attempt to modify a built-in mandatory rule, duplicate rule name, >64 rules) or
// TROUBLE-SCRUB-006 (a mandatory rule is absent from the compiled set). The daemon treats all three as
// fatal at boot: it exits non-zero BEFORE the ingestion port bind preflight.
func New(cfgTOML []byte, projects []types.Project) (*Engine, error)

// Rules returns the effective ordered rule set (global when projectID == ""). Read-only copy.
func (e *Engine) Rules(projectID string) []types.ScrubRule

// The three value entry points. Redactions/ByRule/Truncated/BytesIn are reported for the call.
func (e *Engine) ScrubString(ctx context.Context, target types.ScrubTarget, projectID, s string) (string, types.ScrubResult, error)
func (e *Engine) ScrubBytes(ctx context.Context, target types.ScrubTarget, projectID string, b []byte) ([]byte, types.ScrubResult, error)

// ScrubFields scrubs a bundle (payload field name -> target class) in ONE pass, sharing one budget and
// one counter set; the returned map is the persisted form. Used for SDK payloads, journal tails and
// config snapshots (SDK payloads carry event_msg + stack + header + env together).
func (e *Engine) ScrubFields(ctx context.Context, projectID string, fields map[string][]byte, targets map[string]types.ScrubTarget) (map[string][]byte, types.ScrubResult, error)

// ScrubRecord rewrites the payload fields named in targets in place, sets r.Redactions to the call total,
// and writes the reserved payload key "scrub" (§3.7). It never changes Seq, RecID, TS, Kind, Sig, Origin.
func (e *Engine) ScrubRecord(ctx context.Context, r *types.Record, targets map[string]types.ScrubTarget) (types.ScrubResult, error)

// ScrubEnvelope scrubs every Record in a ForwardEnvelope (satellite send path and hub receive path).
func (e *Engine) ScrubEnvelope(ctx context.Context, env *types.ForwardEnvelope, targets map[string]types.ScrubTarget) (types.ScrubResult, error)

// Verify is the persistence-boundary re-scan: ONE coalesced RE2 alternation carrying the 13 mandatory
// patterns (the entropy rule is deliberately excluded — it is the only heuristic). Boolean result only;
// it never rewrites and never attributes a rule. Returns TROUBLE-SCRUB-008 on a hit.
func (e *Engine) Verify(ctx context.Context, b []byte) error

func (e *Engine) Stats() types.ScrubStats   // cumulative process counters (§3.7)

// AllowedPublicKeys is the shield source. Exported so `trouble init` and the conformance tests can assert
// that exactly the configured project public keys are exempt from redaction.
func AllowedPublicKeys(projects []types.Project) []string
```

**Capture-group convention (the engine's replacement law).** `kind = regex` rules: if the pattern contains
**exactly one capturing group**, that group's span is replaced with `Replace`; if it contains zero or two
or more capturing groups, the whole match is replaced. RE2 has no lookbehind, so context-preserving rules
(`PASSWORD=[REDACTED:env_assign]`, `Authorization: [REDACTED:auth_header]`) are written as
non-capturing context + exactly one value group. `kind = prefix`, `kind = dsn_part`, `kind = entropy` and
`kind = path_allowlist` are implemented by kind-specific scanners (§3.5) and ignore `Pattern`'s regex
semantics; their `Pattern` field carries the literal `"builtin"`.

Design constraint (no AC): the signatures above are build-order constraints; AC-18 and AC-22 exercise
them end-to-end through the sentinel and satellite paths (§7).

## 3. Data model

### 3.1 Target vocabulary and byte budgets

11 targets. A target is a *content class*, not a component: the same class arrives from several
components (a stack arrives from the sentinel SDK, from a collector parser and from the research context).

| Target | Content class | `max_bytes` default | Over-budget behaviour | Producers |
|---|---|---|---|---|
| `event_msg` | exception message / log message | 262144 | tail-drop + marker (SCRUB-003) | SPEC-04 ingest + collectors, SPEC-03 journald |
| `stack` | stack frames, traceback, research context stack | 262144 | tail-drop + marker (SCRUB-003) | SPEC-04, SPEC-07 |
| `header` | HTTP header block (Sentry auth forms, cookies) | 8192 | refuse (SCRUB-003) | SPEC-04 |
| `env` | environment dumps inside SDK payloads | 65536 | refuse (SCRUB-003) | SPEC-04 |
| `journal_tail` | journald entry fields + trailing lines | 65536 | tail-drop + marker (SCRUB-003) | SPEC-03, SPEC-04 |
| `config_snapshot` | resolved-config snapshot (`KConfig` payload) | 262144 | refuse (SCRUB-003) | SPEC-12 |
| `skill` | skill candidate artifact: play TOML, provenance, brief | 131072 | refuse (SCRUB-003) | SPEC-11 |
| `issue` | issue title + body | 65536 | refuse (SCRUB-003) | SPEC-09 |
| `board` | board row title, reasoning, brief | 32768 | refuse (SCRUB-003) | SPEC-08 |
| `dsn` | any DSN or project record echoed into a record | 512 | refuse (SCRUB-003) | SPEC-04, SPEC-12 |
| `spool` | serialized `ForwardEnvelope` before it reaches disk | 4194304 | refuse (SCRUB-003) | SPEC-12 |

Only `event_msg`, `stack` and `journal_tail` truncate. A truncated issue body or board row would corrupt
a durable artifact a human reads, so those targets refuse instead — the caller drops the artifact and
counts it. Truncation is never silent: the tail is replaced by the literal marker
`[SCRUB-TRUNCATED:<dropped_bytes>]`, `ScrubResult.Truncated = true`, and a partial final line is dropped
entirely (never persisted half a line). Truncation happens **before** the rule pass, so the retained head
is fully scrubbed; the cut is moved back to the last `\n` inside the budget.

### 3.2 Config schema (TOML, strict-decoded by the package)

`internal/lifecycle` resolves config precedence (flag > env > file > default) generically and hands the
`[scrub]` subtree's bytes to `New`; `trouble config explain` prints provenance for every dotted key using
its generic flattening, so this package owns its schema without the precedence engine knowing it.

```toml
[scrub]
rules_version     = 1              # bumped ONLY when the mandatory set changes (§4)
max_bytes_default = 262144
rule_timeout      = "250ms"
prefilter         = true           # anchored alternation fast path
boundary_verify   = true           # fixed; any other value is SCRUB-002
entropy           = true           # optional rule 14
pii_mode          = "redact"       # redact | keep
path_mode         = "keep"         # keep | redact
path_allowlist    = ["/srv/src", "/opt/apps", "/usr/lib", "/var/log"]
home_roots        = ["/home/*", "/Users/*", "/root", "/var/home/*"]

[scrub.targets.journal_tail]       # per-target override of max_bytes and of the over-budget behaviour
max_bytes = 65536
on_over   = "truncate"             # truncate | refuse

[scrub.rules.entropy_token]        # built-in optional rules: the ONLY editable key is `enabled`
enabled = true

[scrub.projects."1"]               # key = Project.ID (numeric-as-string)
disable        = ["entropy_token"] # optional built-ins only
pii_mode       = "keep"
path_allowlist = ["/srv/customer-x"]

[[scrub.projects."1".rules]]       # project rules append AFTER rule 18
name      = "customer_account_id"
kind      = "regex"
pattern   = "(?i)(?:^|[^A-Za-z0-9])(?:acct|customer)[_-]?id\\s*[:=]\\s*([0-9]{6,12})"
replace   = "[REDACTED:customer_account_id]"
mandatory = false
targets   = ["event_msg", "stack", "issue"]
rescan    = false                  # true = this one rule re-runs once at the end of the pass
```

Config validation (all failures are TROUBLE-SCRUB-002, all fatal at boot):

| Rule | Reason |
|---|---|
| Unknown key anywhere under `[scrub]` | a typo must not silently disable a knob; strict decode |
| `boundary_verify` other than `true`, or `[scrub.rules.<mandatory-name>]` present at all | the boundary check and the mandatory set are not configurable |
| Any key other than `enabled` on a built-in rule | built-in patterns are compiled from the binary, not from disk |
| Rule name outside `^[a-z][a-z0-9_]{2,31}$` | the marker grammar `[REDACTED:<name>]` must stay unambiguous |
| Duplicate rule name (global vs project, or two project rules) | one name = one counter = one meaning |
| More than 64 rules in the effective set (13 mandatory + 5 optional + ≤46 project) | bounds `by_rule` cardinality and per-call cost |
| Projects keyed by a `Project.ID` that does not exist | a silent per-project override is a silent hole |
| Config-authored `kind = "dsn_part"` or `kind = "path_allowlist"` | those kinds are engine parser paths, not patterns |

### 3.3 The ordered rule table (normative; order is part of the artifact)

Thirteen mandatory rules (M) and five optional rules (O). `M` rules cannot be disabled, renamed,
re-targeted or re-patterned by any config on any host; `O` rules default enabled and may be disabled
per host or per project. `ALL` = the 11 targets of §3.1.

```
#   RULE                    KIND            M/O  TARGETS
1   private_key_block       prefix          M    ALL
2   private_key_inline      regex           M    ALL
3   dsn_secret              dsn_part        M    ALL
4   dsn_any                 dsn_part        M    ALL
5   conn_string_password    regex           M    ALL
6   url_basic_auth          regex           M    ALL
7   env_assign              regex           M    ALL
8   kv_secret_assign        regex           M    ALL
9   cli_flag_secret         regex           M    ALL
10  auth_header             regex           M    ALL
11  bearer_token            regex           M    ALL
12  jwt                     regex           M    ALL
13  cloud_key_shape         regex           M    ALL
14  entropy_token           entropy         O    event_msg stack journal_tail config_snapshot skill issue board spool
15  pii_email_ip            regex           O    event_msg stack journal_tail config_snapshot skill issue board spool
16  pii_identity_kv         regex           O    event_msg stack journal_tail config_snapshot skill issue board env spool
17  path_home_root          path_allowlist  O    event_msg stack journal_tail config_snapshot skill issue board env spool
18  path_disclosure         path_allowlist  O    event_msg stack journal_tail config_snapshot skill issue board spool
```

Exact patterns and replacement markers. `Replace` is always `[REDACTED:<rule-name>]` except
`path_home_root`, which keeps the remainder of the path after the marker (§3.5).

```
1  private_key_block        kind=prefix, replace [REDACTED:private_key_block]
   BEGIN markers (literal, case-sensitive, multi-line span to the end of the block):
     "-----BEGIN PRIVATE KEY-----"          "-----BEGIN RSA PRIVATE KEY-----"
     "-----BEGIN DSA PRIVATE KEY-----"      "-----BEGIN EC PRIVATE KEY-----"
     "-----BEGIN OPENSSH PRIVATE KEY-----"  "-----BEGIN ENCRYPTED PRIVATE KEY-----"
     "-----BEGIN PGP PRIVATE KEY BLOCK-----"  "PuTTY-User-Key-File-"
   END: the first following line whose first 9 bytes are "-----END "; EOF ⇒ span runs to end of input;
        a PuTTY block ends at the first blank line.

2  private_key_inline
   (?i)(?:^|[^A-Za-z0-9])(?:private[_-]?key|secret[_-]?key|ssh[_-]?key|key[_-]?material|keystore[_-]?(?:password|pass))
   \s*[:=]\s*("[^"\n]{16,8192}"|'[^'\n]{16,8192}'|[A-Za-z0-9+/=_-]{16,8192})           [1 group: value]

3  dsn_secret               kind=dsn_part, replace [REDACTED:dsn_secret]
   parse {scheme}://{pubkey32hex}:{secret32hex}@{host}  ⇒ scheme and pubkey emitted verbatim,
   group 3 (the secret) replaced ⇒ http://<pubkey>:[REDACTED:dsn_secret]@host:port/project

4  dsn_any                  kind=dsn_part, replace [REDACTED:dsn_any]
   parser rewrites ANY DSN-shaped URI to {scheme}://{pubkey}@{host}[:{port}]/{project_id}: query string,
   fragment, extra path segments and any non-public credential in the userinfo are dropped. If no
   32-hex public key survives, the whole userinfo becomes [REDACTED:dsn_any].

5  conn_string_password
   (?i)\b(?:postgres|postgresql|mysql|mariadb|mongodb|mongodb\+srv|redis|rediss|amqp|amqps|clickhouse|
   kafka|mssql|ftp|ftps|ldap|ldaps|memcached)://(?:[^:@/\s]{1,64}):([^@/\s]{1,4096})@              [1 group]

6  url_basic_auth
   (?i)\b[a-z][a-z0-9+.\-]{1,15}://(?:[^:@/\s]{1,64}):([^@/\s]{1,4096})@                           [1 group]

7  env_assign
   (?i)(?:^|[^A-Za-z0-9])(?:PASSPHRASE|PASSWORD|PASSWD|PWD|SECRET_KEY|SECRET|API[_-]?KEY|ACCESS[_-]?KEY|
   PRIVATE[_-]?KEY|CLIENT[_-]?SECRET|SESSION[_-]?KEY|ENCRYPTION[_-]?KEY|SIGNING[_-]?KEY|
   CONNECTION[_-]?STRING|CONN[_-]?STR|AUTHORIZATION|AUTH|CREDENTIALS|CREDENTIAL|TOKEN|DSN|BEARER)
   \s*=\s*("[^"\n]{0,8192}"|'[^'\n]{0,8192}'|[^\s;,&]{1,8192})                                    [1 group]
   The separator class is [^A-Za-z0-9] so MY_SECRET= and API_KEY= and AUTH_TOKEN= match, while
   MYSECRET= does not. URL and URI are deliberately ABSENT from the name list: a URL is not a secret,
   and its embedded credentials are handled by rules 5 and 6 (redacting by association destroys
   diagnostic value).

8  kv_secret_assign
   (?i)(?:^|[^A-Za-z0-9])(?:passphrase|password|passwd|pwd|secret_key|secret|api[_-]?key|access[_-]?key|
   private[_-]?key|client[_-]?secret|session[_-]?key|encryption[_-]?key|signing[_-]?key|token|dsn|
   connection[_-]?string|conn[_-]?str|credentials?|authorization|auth[_-]?token)["']?\s*[:=]\s*
   ("[^"\n]{0,8192}"|'[^'\n]{0,8192}'|[^\s,}\]&;]{1,8192})                                        [1 group]

9  cli_flag_secret
   (?i)(?:^|\s)--?[a-z][a-z0-9_\-]{0,31}(?:password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key)
   [a-z0-9_\-]{0,8}(?:=|\s+)("[^"\n]{0,8192}"|[^\s"]{1,8192})                                     [1 group]
   post-match exemption (left unchanged, counted as exempt, not redacted): a captured value that is
   explicitly path-shaped — one of the prefixes `/`, `~/`, `./`, `../` followed by at least one byte,
   every byte of it in the path alphabet `[A-Za-z0-9._/-]` — because a token STORE PATH is a locator,
   not a credential, and the rule is name-driven: the flag NAME is what matched, so a path, a token and
   an environment-variable name are one shape to it. An ABSOLUTE value additionally carries path
   STRUCTURE (a separator or an extension after the leading slash: `/srv/tokens.json`, `/tokens.json`);
   `/` is in the standard-base64 credential alphabet, so a slash-prefixed word with neither is a
   credential shape that happens to begin with a slash and stays in scope. The exemption is a post-match
   filter on the captured value, like the loopback exemption of rules 15/16 — never a hedge on the
   pattern: `--hub-token <token>`, `--api-key=abcdef123456` (P6) and a bare file name are still matched.
   It holds at the persistence boundary too (§3.4 point 3), so the daemon's argv control does not refuse
   the value the scrub pass left in place (SPEC-12 §2.5a, TRBL-026).

10 auth_header
   (?im)^[ \t]*(?:authorization|proxy-authorization|x-api-key|x-auth-token|x-sentry-auth|
   x-amz-security-token|api-key|cookie|set-cookie)[ \t]*:[ \t]*(.+)$                              [1 group]

11 bearer_token
   (?i)(?:^|[^A-Za-z0-9])bearer\s+([A-Za-z0-9\-._~+/]{8,8192}={0,2})                              [1 group]

12 jwt
   \beyJ[A-Za-z0-9_\-]{8,4096}\.[A-Za-z0-9_\-]{8,4096}\.[A-Za-z0-9_\-]{0,4096}                    [0 groups]

13 cloud_key_shape
   (?i)(?:AKIA[0-9A-Z]{16}|ASIA[0-9A-Z]{16}|gh[pousr]_[A-Za-z0-9]{36,255}|github_pat_[A-Za-z0-9_]{22,255}
   |glpat-[A-Za-z0-9_\-]{20,64}|xox[baprs]-[A-Za-z0-9\-]{10,255}|sk-(?:proj-)?[A-Za-z0-9_\-]{20,255}
   |AIza[0-9A-Za-z_\-]{35}|ya29\.[A-Za-z0-9_\-]{20,255}|npm_[A-Za-z0-9]{36,255}
   |pypi-AgEIcHlwaS5vcmc[A-Za-z0-9_\-]{20,255}|dckr_pat_[A-Za-z0-9_\-]{20,255}
   |SG\.[A-Za-z0-9_\-]{20,64}\.[A-Za-z0-9_\-]{20,64}|hf_[A-Za-z0-9]{30,255}|tvly-[A-Za-z0-9]{20,64}
   |shpat_[0-9a-f]{32}|eyJr[A-Za-z0-9_\-]{20,})                                                  [0 groups]

14 entropy_token            kind=entropy, replace [REDACTED:entropy_token]
   run:    [A-Za-z0-9_\-+/]{40,4096}
   reject: the run is pure hex ([0-9a-f]+ or [0-9A-F]+) ⇒ git SHAs, md5/sha256 digests, hex ids survive
   admit:  hasLower && hasUpper && hasDigit && shannonBitsPerChar(run) >= 3.5
   A 32-hex value is below the 40-char floor, so no DSN public key and no Sentry event_id is ever in
   scope for this rule — the pubkey exemption holds structurally, not by exception.

15 pii_email_ip
   (?:[A-Za-z0-9._%+\-]{1,64}@(?:[A-Za-z0-9\-]{1,63}\.){1,8}[A-Za-z]{2,24})
   |(?:\b(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(?:\.(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3}\b)      [0 groups]
   post-match exemptions (left unchanged, counted as exempt, not redacted): 127.0.0.0/8, ::1, 0.0.0.0,
   10/8, 172.16/12, 192.168/16, 169.254/16 — loopback and RFC1918 identify nobody outside this host.

16 pii_identity_kv
   (?i)(?:^|[^A-Za-z0-9])(?:user|username|user_name|email|e_mail|ip_address|client_ip|remote_addr|cookie|
   session_id)["']?\s*[:=]\s*("[^"\n]{0,4096}"|'[^'\n]{0,4096}'|[^\s,}\]&;]{1,4096})              [1 group]
   same loopback/RFC1918 exemption on the captured value.

17 path_home_root            kind=path_allowlist, replace [REDACTED:path_home_root]
   path token: (?:^|[\s"'`(=:,])(/[A-Za-z0-9._\-]{1,255}(?:/[A-Za-z0-9._\-]{1,255}){0,15})
   if the token's leading 1–3 segments match a configured home_root (/home/*, /Users/*, /root,
   /var/home/*) ⇒ that prefix is replaced, the remainder is emitted verbatim:
   /home/<user>/projects/trouble/internal/scrub/engine.go
     ⇒ [REDACTED:path_home_root]/projects/trouble/internal/scrub/engine.go

18 path_disclosure            kind=path_allowlist, replace [REDACTED:path_disclosure]   (default enabled
   only when path_mode="redact")                                                              = false)
   any absolute path token of >=3 segments whose prefix is not in path_allowlist ⇒ whole token replaced.
```

`sendDefaultPii` semantics (pinned): trouble cannot trust a client's SDK settings, so `pii_email_ip` and
`pii_identity_kv` run server-side regardless of what the SDK declared; an SDK that sets
`sendDefaultPii=false` simply sends fewer fields, and one that sends `user`, `email`, `ip_address` or
`Cookie` gets those values redacted at ingest. The escape hatch is explicit and per project:
`pii_mode = "keep"`, which is a config change recorded by SPEC-12's config record, not a silent default.

### 3.4 Mandatory-rule enforcement

The mandatory set is those 13 names. Three enforcement points, in order:

1. **Compile time (`New`).** The built-in table is compiled from the binary. If any of the 13 names is
   absent or failed to compile, `New` returns TROUBLE-SCRUB-006 (absent) / TROUBLE-SCRUB-001 (uncompilable)
   and the daemon exits non-zero before binding :7643 or :7644. A build that loses a mandatory rule cannot
   run.
2. **Config time.** Any config that disables, renames, re-patterns, re-targets or shadows a mandatory rule
   is TROUBLE-SCRUB-002 (§3.2). There is no precedence chain, environment variable, flag or per-project
   key that reaches the mandatory set; `trouble config explain` prints each mandatory rule as
   `builtin (source: builtin)`.
3. **Persistence time.** `internal/ledger` calls `Verify` on every serialized record line before
   `write`. A hit is TROUBLE-SCRUB-008 and the record is **refused** (not written, not tombstoned, not
   replaced by a partial). This is the backstop for a caller that forgot to scrub; it is the mechanism
   that turns invariant 3 from a promise into an enforced property.

### 3.5 DSN handling — the single allowed credential

DSN grammar (SPEC-TYPES §6.1): `{scheme}://{pubkey}[:{secret}]@{host}[:{port}]/{project_id}`; the path
ends at the base because SDKs append `/api/{project_id}/envelope/`.

| Case | Behaviour |
|---|---|
| DSN **with** a secret half in a message, stack, header, journal tail, config snapshot, skill, issue, board or spool | `dsn_secret` replaces the secret with `[REDACTED:dsn_secret]`, then `dsn_any` normalizes the remainder to `scheme://pubkey@host[:port]/project_id`. The secret is gone from the byte stream before any subsequent rule runs. |
| DSN **without** a secret half | `dsn_any` rewrites the URI to its canonical public-key-only form; the pubkey, host, port and project id are preserved byte-for-byte. |
| Public key appearing **alone** (a log line, a diagnostic, `pubkey=…`) | Not redacted. It is registered in the shield and restored verbatim. It is designed public. |
| `Project.SecretKey` echoed into a record or a dashboard response | Scrubbed with target `dsn`; if a caller misses it, the boundary re-scan's DSN alternation catches it (SCRUB-008). |
| A DSN embedded in a stack trace or exception message | Same as case 1/2: the message keeps `scheme://pubkey@host/project`, which is enough to identify the reporting project and leaks nothing. |
| A DSN whose host is an IP or a bind address | Not this subsystem's problem to refuse (SPEC-04 rejects it at project creation), but the rule still normalizes it; the public key exemption is host-independent. |

**Shield mechanism.** `New` builds the shield table from `Project.PublicKey` for every enabled project.
Before rule 1, each known public key occurrence is replaced by the placeholder `\x00S<index>\x00` (NUL is
valid UTF-8 and appears in no rule's alphabet, so no rule can match a placeholder); after rule 18 the
placeholders are restored. If the pass fails closed (§3.8) the placeholder form is never returned —
`Scrub` returns the error with no value, so a placeholder can never reach disk. A conformance test asserts
that no returned value contains `\x00` and that every seeded public key is present verbatim in the output.

### 3.6 Ordering semantics

1. **Effective set construction** (before any matching): built-in rules 1–13 in table order, then built-in
   optional rules 14–18 in table order minus the ones the project disabled, then the project's own rules in
   their declared order. Disabling is set-subtraction; it never reorders the survivors.
2. **Private keys before token and shape rules.** Rules 1–2 precede every token rule because a PEM body is
   base64 that rule 14 would otherwise match in fragments: the earlier rule consumes the whole span, so the
   output has one marker instead of a partially rewritten key.
3. **Secret before normalization inside the DSN family.** `dsn_secret` (3) before `dsn_any` (4) so the
   secret is removed even if the surrounding URI cannot be fully normalized.
4. **Scheme-aware before generic inside the connection-string family.** `conn_string_password` (5) before
   `url_basic_auth` (6) so a database URL is attributed to the more diagnostic rule name.
5. **Structured header before bare token shape.** `auth_header` (10) before `bearer_token` (11) so an
   `Authorization:` line is replaced once, at the header level.
6. **Name-driven before heuristic.** All 13 mandatory rules precede `entropy_token` (14): a value already
   consumed by a name rule cannot be re-attributed to the heuristic, which keeps `by_rule` interpretable.
7. **Longest match first, within a rule.** Matches come from RE2 leftmost-first `FindAll*` semantics and
   never overlap; a rule that could match two spans at one position is written so the longer alternative
   is listed first (rule 7's name list is longest-first for this reason).
8. **Project overrides after global rules.** Project rules run after rule 18, in declared order, over the
   already-marker-bearing buffer. They can only add redactions to what the global set produced; they cannot
   undo, re-target or rename a global rule.
9. **Single pass, not multi-pass.** Each rule scans the working buffer exactly once. A second pass over the
   built-in set is provably a no-op: every `Replace` value is a marker, and no built-in pattern matches
   `[REDACTED`, `[SCRUB-TRUNCATED` or `~` (asserted by the fixed-point test in §7) — so a multi-pass design
   would burn a second full pass to produce identical bytes and would make `by_rule` order-dependent.
   `rescan = true` on a *project* rule is the one escape hatch: such a rule re-runs once at the end of the
   pass, for a maximum of two evaluations per call, capped at 8 rescan rules per project. The reason is
   that a project rule may rewrite a span into new text (a custom scheme, a customer id format), where a
   bounded re-run is deterministic and a second global pass is not.
10. **Prefilter fast path.** One coalesced RE2 alternation of cheap anchors
    (`(?i)(bearer |-----BEGIN|private key|secret|token|password|passwd|passphrase|api[_-]?key|://|authorization:|cookie:|sentry_key|eyJ|AKIA)`)
    runs first. If it misses, the buffer is returned unchanged with `Redactions = 0` and no per-rule pass.
    Cost is ≤3 µs/KiB, and it is what keeps the ingress path inside budget (§3.9).

### 3.7 Counters — counts, never values

| Landing point | Field | Rule |
|---|---|---|
| `ScrubResult` | `Redactions`, `ByRule`, `Truncated`, `BytesIn` | per call: `Redactions = Σ ByRule`; `ByRule[rule]` counts *replacements*, so one header line redacted by rule 10 counts as 1 even though it contained a JWT |
| `Record` (ledger) | `redactions` (int) | set by `ScrubRecord`/the caller from the call total; written on every record whose payload passed through the scrubber, including `0` (the field has no `omitempty`) |
| `Record.payload["scrub"]` | `{"by_rule":{...},"bytes_in":N,"bytes_out":N,"truncated":false,"rules_version":1,"engine":"re2"}` | reserved key written by `ScrubRecord`; `by_rule` is bounded to ≤64 names by §3.2, so payload size and index cardinality are bounded |
| `Group` counters | `GroupCounters.Redacted` (`redacted_values`) | SPEC-04 increments the owning group by `Record.Redactions` for every event record with that `Sig`; a group's counter is therefore the exact count of values scrubbed in that group |
| Process | `ScrubStats` via `Engine.Stats()` | cumulative totals: `calls bytes_in bytes_out redactions by_rule truncated refused_bytes invalid_utf8 timeouts fail_closed boundary_refusals rules_version`; consumed read-only by the dashboard (SPEC-10) and by the conformance tests |
| Config snapshot | `ConfigValue.Redacted` | true when the value was produced by the scrubber (§4) |

Never in a counter or an error: the value, a prefix of the value, a hash of the value, or a length of the
value. `ByRule` keys are rule names from the closed set of §3.3 plus validated project rule names.

### 3.8 Limits, non-UTF-8 and fail-closed

| Condition | Code | Behaviour |
|---|---|---|
| Input exceeds the target's `max_bytes`, target truncates | TROUBLE-SCRUB-003 (transient, counted) | head kept to the last `\n` inside the budget, `Truncated=true`, marker appended, call **succeeds** |
| Input exceeds `max_bytes`, target refuses | TROUBLE-SCRUB-003 (transient, counted) | no output, no partial output, caller treats the payload as unavailable and emits a `gap` record |
| A rule's evaluation exceeds `rule_timeout` (250 ms default) | TROUBLE-SCRUB-005 (transient) | fail closed: `Value=nil`, `Redactions=0`, `fail_closed` incremented, caller emits a `gap` with cause `scrub_failed` |
| A rule returns an engine error (bad state, counter overflow) | TROUBLE-SCRUB-004 (permanent) / TROUBLE-SCRUB-005 | fail closed, same as above |
| Non-UTF-8 bytes, any target except `spool` | TROUBLE-SCRUB-007 (permanent) | fail closed. RE2 operates on invalid UTF-8 as bytes, so case-folding and `.` semantics degrade and a secret adjacent to an invalid byte can slip a boundary; refusing is the only safe answer |
| Non-UTF-8 bytes after decompression on `spool` | TROUBLE-SCRUB-007 (permanent) | fail closed; the spool payload is always reconstructed as valid UTF-8 JSON before it is written |
| Boundary re-scan hit | TROUBLE-SCRUB-008 (permanent) | record refused by `internal/ledger`; counted in `boundary_refusals` |
| A mandatory rule absent from the compiled set | TROUBLE-SCRUB-006 (permanent) | boot refusal; if it could ever occur at runtime, the ledger writer refuses to append |

Fail-closed is the whole point: on any of the above the caller persists **metadata only** — no message,
no stack, no header, no journal bytes — plus a `gap` record whose cause is `scrub_failed` or
`scrub_invalid_utf8` (this extends the documented `GapRecord.Cause` vocabulary; the field is a free-form
string, so no type changes with it). The cost is explicit and accepted: a 1 MiB payload that times out is
evidence lost. The alternative — persisting unscrubbed bytes into an append-only, git-distributed ledger —
is unauditable, which is exactly the liability the quorum named.

### 3.9 Performance budget (measured basis, normative targets)

Basis: sentinel ingestion measured **6,199 req/s** with 100-line group-commit vs **1,972 req/s**
fsync-per-line; group-commit amortizes to **1.94 µs/rec** (515k rec/s) vs **512 rec/s** per-line fsync; a
representative event is 1,064 B raw / 350 B gzip; a ledger line is 74 B. The scrubber must not become the
ingestion bottleneck, so:

| Stage | Budget | Notes |
|---|---|---|
| Prefilter (coalesced anchors) | ≤3 µs/KiB | one RE2 alternation, no allocation when it misses |
| Full mandatory pass (rules 1–13) | ≤25 µs/KiB | 13 RE2 patterns, linear time, no backtracking |
| Full set incl. optional (1–18) | ≤60 µs/KiB | the shipped 256 KiB worst case ⇒ ≤15 ms |
| Boundary re-scan (`Verify`) | ≤10 µs/KiB | one coalesced alternation of the 13 mandatory patterns, boolean only |
| Typical 1,064 B envelope, end to end | ≤45 µs | 6,199 req/s × 45 µs = **0.28 core** of scrubbing at the measured peak |
| Ingestion floor with scrubbing ON | ≥5,000 req/s | ≤20% throughput cost vs the 6,199 req/s no-scrub baseline; regression-barred in CI |
| Transient memory | ≤4 × input, ≤8 MiB at 8 concurrent 256 KiB calls | stays inside the ≤80 MB steady RSS budget |
| `by_rule` cardinality | ≤64 names | bounds payload and index growth |

**Regex engine: Go stdlib `regexp` (RE2).** Linear time in input, no backtracking, therefore no ReDoS
surface on attacker-controlled SDK payloads, and no cgo — `CGO_ENABLED=0` static builds stay intact.
Rejected: PCRE/Oniguruma through cgo (breaks the static build and reintroduces catastrophic backtracking),
`regexp2`/backtracking engines (unbounded worst case on hostile input), and hand-rolled matchers for the
whole table (the two `dsn_part` rules and the two `path_allowlist` rules use kind-specific scanners
because they need parsing, not matching; `private_key_block` uses a literal marker scan because Go's RE2
cannot express a matched END marker — no backreferences). All rules compile at `New`; a compile failure is
TROUBLE-SCRUB-001 and fatal at boot, never at first event.

### 3.10 Types added to SPEC-TYPES by this spec

```go
type ScrubTarget string
const (
    TgEventMsg       ScrubTarget = "event_msg"
    TgStack          ScrubTarget = "stack"
    TgHeader         ScrubTarget = "header"
    TgEnv            ScrubTarget = "env"
    TgJournalTail    ScrubTarget = "journal_tail"
    TgConfigSnapshot ScrubTarget = "config_snapshot"
    TgSkill          ScrubTarget = "skill"
    TgIssue          ScrubTarget = "issue"
    TgBoard          ScrubTarget = "board"
    TgDSN            ScrubTarget = "dsn"
    TgSpool          ScrubTarget = "spool"
)

func (t ScrubTarget) Valid() bool

type ScrubStats struct {
    Calls            uint64            `json:"calls"`
    BytesIn          uint64            `json:"bytes_in"`
    BytesOut         uint64            `json:"bytes_out"`
    Redactions       uint64            `json:"redactions"`
    ByRule           map[string]uint64 `json:"by_rule"`
    Truncated        uint64            `json:"truncated"`
    RefusedBytes     uint64            `json:"refused_bytes"`
    InvalidUTF8      uint64            `json:"invalid_utf8"`
    Timeouts         uint64            `json:"timeouts"`
    FailClosed       uint64            `json:"fail_closed"`
    BoundaryRefusals uint64            `json:"boundary_refusals"`
    RulesVersion     int               `json:"rules_version"`
}
```

```json
"event_msg"
{"calls":41207,"bytes_in":43819008,"bytes_out":43819008,"redactions":912,"by_rule":{"env_assign":611,"bearer_token":188,"dsn_secret":113},"truncated":2,"refused_bytes":0,"invalid_utf8":0,"timeouts":0,"fail_closed":0,"boundary_refusals":0,"rules_version":1}
```

`ScrubRule` and `ScrubResult` already exist in SPEC-TYPES §3.3 and are used unchanged; `ScrubTarget` and
`ScrubStats` are reported as TYPES-GAP for the index owner to fold in.

## 4. Wiring

Call order is part of the contract: **parse → UTF-8 check → scrub → sig/fingerprint → dedup/group → ladder
→ ledger**. Computing a sig before scrubbing is a defect (the raw bytes would key a group and the scrubbed
payload would key a different one, splitting AC-22's single incident into two).

| Caller | Targets | Where it must run | Why |
|---|---|---|---|
| `internal/sentinel` ingestion (`POST /api/{id}/envelope/`, `/store/`, generic JSON) | `event_msg`, `stack`, `header`, `env` | after envelope decode + gzip, before fingerprint and before quota accounting writes anything | the SDK payload is the richest secret carrier (auth headers, DSN in an exception message, env dumps) |
| `internal/sentinel` collector parsers (go-panic, py-traceback, node-reject, journal tails) | `journal_tail`, `event_msg`, `stack` | after multi-line assembly, before sig | a traceback carries the connection string that caused the crash |
| `internal/sensors` journald follower | `journal_tail` | per entry, after `--output-fields` projection, before sig | raw journal lines carry tokens |
| `internal/sensors` PSI / disk / timers / D-Bus / inotify | — (no call) | — | payloads are numeric plus unit/mount/path names from configuration, validated at construction by SPEC-03; no free text, so no scrub call and no budget cost |
| `internal/research` | `stack` | `SubmitRequest.context.stack` before the POST | the outbound body leaves the host |
| `internal/skills` | `skill` | candidate play TOML, provenance and brief before the candidate record; every pulled artifact before it is written under `skills-local/` | a skill is executable content and its provenance quotes incidents |
| `internal/issues` | `issue` | title and body before `EnsureBySig`, and before every `Comment` | the issue leaves the host (GitHub) or is git-distributed (DuckBrain) |
| `internal/flow` | `board` | title, reasoning and brief before the board append and before `router_spawn` | the board row is copied into a worker brief |
| `internal/lifecycle` config snapshot | `config_snapshot`, `dsn` | before the `KConfig` record; `ConfigValue.Redacted` set from the result | the snapshot is the most concentrated credential surface in the system |
| `internal/lifecycle` spool writer | `spool` | on the serialized `ForwardEnvelope` **before the file write**; `Verify` on read-back before send | **spool bytes on disk are always scrubbed** — a spool file is a plain file on a portable disk, and a satellite may be a laptop |
| `internal/lifecycle` satellite forward | `spool` (send) + all targets (receive) | satellite scrubs before enqueueing; the hub re-scrubs on receipt | the hub's rule table is the authority: the satellite may run an older table, and the hub must not trust it. The pass is idempotent, so the hub sees the same `Redactions` and the same `sig` — this is what makes AC-18 hold across the wire |
| `internal/ledger` writer | — (`Verify` only) | every serialized record line, immediately before `write` | the persistence-boundary backstop (§3.4 point 3) |
| `internal/dashboard` | — (no call) | — | render-time scrubbing is refused: the ledger is the trust boundary, payloads are scrubbed at write, markers render verbatim, and a second pass would double-count and hide a write-path defect. A dashboard token is never echoed into a record at all (SPEC-10) |
| `trouble init` | `dsn` | when printing the generated DSN | the printed DSN carries the public key only; the secret half, if generated, is written to the 0600 EnvironmentFile and never to stdout |

`rules_version` (config key, default 1) is the rule-table identity. It is bumped **only** when the
mandatory set changes; a bump requires a `ForwardEnvelope.protocol_version` bump (SPEC-12 §3.7) because a
satellite and a hub that disagree on the mandatory set could disagree on what a scrubbed record means.
Additive optional rules and per-project rules do not bump it. `payload["scrub"]["rules_version"]` records
which table scrubbed each ledger line, so a forensic read never has to guess.

## 5. Errors

| Code | Class | Trigger | Caller behaviour | Ledger mirror |
|---|---|---|---|---|
| TROUBLE-SCRUB-001 | permanent | a rule pattern failed to compile | fatal at boot (`New`); at runtime: fail closed | `payload.error_code` on the caller's `gap` record |
| TROUBLE-SCRUB-002 | permanent | config invalid: unknown key, attempt to modify a built-in mandatory rule, duplicate rule name, >64 rules, bad rule name, unknown project, config-authored `dsn_part`/`path_allowlist` rule | fatal at boot; `trouble config explain` prints the offending dotted key | boot failure, no record |
| TROUBLE-SCRUB-003 | transient | input exceeded the target's `max_bytes` | truncate-and-mark, or refuse per §3.1; never partial output | `payload.error_code` + `truncated` flag on the record; `gap` on refuse |
| TROUBLE-SCRUB-004 | permanent | a single rule produced more than 2^31-1 replacements on one value | fail closed; `fail_closed` incremented | `payload.error_code` on the caller's `gap` record |
| TROUBLE-SCRUB-005 | transient | rule evaluation exceeded `rule_timeout` (250 ms) | fail closed; `timeouts` incremented | caller's `gap` record, cause `scrub_failed` |
| TROUBLE-SCRUB-006 | permanent | a mandatory rule is absent from the active rule set | boot refusal; the ledger writer refuses to append if it could ever appear at runtime | boot failure; `payload.error_code` on the refusal path |
| TROUBLE-SCRUB-007 | permanent | input is not valid UTF-8 and the target is not the binary-allowed one (`spool`, post-decompression) | fail closed; `invalid_utf8` incremented | caller's `gap` record, cause `scrub_invalid_utf8` |
| TROUBLE-SCRUB-008 | permanent | persistence-boundary re-scan matched a mandatory rule pattern | the record is refused by `internal/ledger`; `boundary_refusals` incremented; a `lifecycle`-kind ledger note is written by SPEC-12 (the refusal itself is recorded, never the offending bytes) | the refusal note's `payload.error_code` |

`TROUBLE-SCRUB-008` is new and belongs to this area's range; it is reported as ERROR-ADD for SPEC-TYPES §5
per SPEC-INDEX §3.5.

## 6. Edge cases

- **Marker in the input.** A payload that already contains `[REDACTED:env_assign]` (a re-read spool line, a
  quoted previous report, a test fixture) is returned unchanged: no built-in pattern matches a marker, and
  the fixed-point test asserts `Scrub(x) == x` for every one of the 18 markers. This is why hub-side
  re-scrubbing a satellite's records is safe and why AC-22 dedup survives the round trip.
- **A hostile rule name.** A project rule named `x]` or `redacted` would create an ambiguous marker or a
  self-referential one; §3.2's `^[a-z][a-z0-9_]{2,31}$` and the duplicate-name check make both
  TROUBLE-SCRUB-002.
- **Overlapping spans.** Rule 5 and rule 6 can both match one URI: the earlier rule consumes it and the
  subsequent one sees the marker, so the count is 1 and the attribution is the database-scheme rule. Two rules
  matching the *same* span never both fire.
- **Public key inside a private-key block.** If a project's public key string somehow appears inside a PEM
  body, the shield restores it and the block is still redacted around it. The restored value is the
  designed-public credential, so nothing is leaked, and the case is asserted in the vector suite.
- **DSN with no secret half.** Byte-identical output (a negative vector). Shielding plus the 40-character
  entropy floor make this structural rather than exceptional.
- **Percent-encoded DSN secret or userinfo.** `dsn_secret` matches the unescaped 32-hex form; the
  percent-encoded form is caught by `dsn_any`, which drops everything between `://` and `@` that is not a
  32-hex public key.
- **Zero-length input, whitespace-only input, a single marker.** All return the input with
  `Redactions=0`, `BytesIn=0/len`, no error; `ScrubBytes(nil)` returns an empty non-nil slice so callers can
  distinguish "scrubbed empty" from "failed" (failure is the error return).
- **A secret split by truncation.** Truncation cuts back to the last `\n` inside the budget and drops the
  partial final line, so no rule ever sees a half-value and no half-value is persisted.
- **A sensitive name as a JSON key rather than a value.** JSON keys are rewritten only when the key token
  itself matches a positive shape (`cloud_key_shape`, `jwt`, `entropy_token`); a sensitive *name* redacts
  its *value* (rules 7/8/16). A redacted key would break a schema the reader has to parse.
- **IPv6 literals and hostnames.** `pii_email_ip` covers IPv4 and email; IPv6 literals are left intact
  because the grammar's false-positive rate on `::`-bearing strings (timestamps, C++ scopes, TOML) is high
  and the operators' exemption list is unenforceable there. IPv6 identifiers are redacted when they arrive
  under a sensitive name (rule 16).
- **Concurrency.** `Engine` holds immutable compiled rules and an immutable shield table; only `Stats`
  mutates, through atomics. N ingestion goroutines share one engine with no lock on the hot path.
- **Empty rule delta.** A project with no overrides and no disabled optional rules produces an effective
  set identical to the global one; `Rules("")` and `Rules("<project>")` return equal slices (deep-equal
  test in §7).
- **A payload field with no declared target.** `ScrubFields` accepts only fields present in `targets`; an
  undeclared field is returned untouched and counted as `unmapped` in the call result. The callers in §4
  declare every field that reaches disk, and the boundary re-scan is the backstop for a missed declaration.
- **Rule table disabled entirely (`entropy=false`, all optional off).** The 13 mandatory rules still run;
  there is no configuration, flag, build tag or per-project key that yields an empty mandatory pass.

## 7. Testing

Vector and conformance files are CI-fatal. Names are exact so the selfcheck and CI can reference them.

| File | Content | Pass threshold |
|---|---|---|
| `internal/scrub/vectors_test.go` | 14 positive vectors (one per rule family plus boundary cases) and 12 negative vectors, each asserting exact output bytes and the exact `ByRule` map | 100% exact-byte equality, 0 tolerance |
| `internal/scrub/fixedpoint_test.go` | for each of the 18 markers: `Scrub(Scrub(x)) == Scrub(x)` and `Redactions(second) == 0`; plus a property test over 10,000 generated payloads | 0 failures; idempotence is AC-22's mechanism |
| `internal/scrub/shield_test.go` | every configured public key survives verbatim in the output; no returned value contains `\x00` | 0 leaked placeholders, 0 redacted public keys |
| `internal/scrub/config_test.go` | one case per §3.2 validation row, asserting the exact TROUBLE-SCRUB-002 path | 100% of rows |
| `internal/scrub/conformance_test.go` | one positive vector per mandatory rule proving each is compiled, present, and fires on its vector; a deliberately missing mandatory rule (built via a test-only table) returns TROUBLE-SCRUB-006 | 13/13 mandatory, 5/5 optional |
| `internal/scrub/limits_test.go` | 256 KiB truncation boundary (cut at the last `\n`, marker exact, partial line dropped), refuse-mode targets return SCRUB-003 with no partial output, non-UTF-8 on a text target returns SCRUB-007, timeout injection returns SCRUB-005 with `Value==nil` | 100% |
| `internal/scrub/dsn_test.go` | DSN with secret, without secret, percent-encoded, embedded in a traceback, `Project.SecretKey` echo | secret bytes absent in all 5 |
| `internal/scrub/flagvalue_test.go` | rule 9's explicit-path exemption in both directions: a token-store path value after `--dashboard-token_file`/`--hub-token` is left unchanged and counted exempt (space and `=` spellings, the quoted form, `/home/…`, `/tokens.json` and the shell idioms `~/`, `./`, `../`), and the exemption is a fixed point whose output the boundary accepts; against it, a token value, a bare file name, a slash-prefixed word with no path structure (`/tokens`, `/abcDEF…`), a credential-alphabet run (`/9j4K+fg==`), the lone `/` and every `NAME=value` assignment (including `HUB_TOKEN=/srv/hub.token` and `--hub-token=/srv/hub.token`) are still redacted or refused | 100% exact-byte equality; 0 non-path values exempted; every refusal stays TROUBLE-SCRUB-008 at the boundary |
| `internal/scrub/bench_test.go` | `BenchmarkPrefilter1KiB` ≤3 µs, `BenchmarkMandatory1KiB` ≤25 µs, `BenchmarkFull1KiB` ≤60 µs, `BenchmarkScrub256KiB` ≤15 ms, `BenchmarkVerify1KiB` ≤10 µs | budget table in §3.9, regression-barred |
| `internal/scrub/e2e_ledger_test.go` | **the "no unredacted secret reached the ledger" test** | see below |
| `internal/scrub/corpus_test.go` | 10,000-line corpus of real-shaped journal lines containing no rule-shaped span (verified independently with `grep -E`) | 0 redactions with the mandatory set; ≤0.5% of lines touched by `entropy_token` |
| `internal/scrub/bench_ingest_test.go` | re-runs the ingestion harness that measured 6,199 req/s, with the scrubber in the path | ≥5,000 req/s; ≤20% cost |

**Vectors (exact, and the `by_rule` they must produce).**

| # | Input | Expected output | by_rule |
|---|---|---|---|
| P1 | `PASSWORD=hunter2` | `PASSWORD=[REDACTED:env_assign]` | `env_assign:1` |
| P2 | `MY_API_KEY=abcdef1234567890` | `MY_API_KEY=[REDACTED:env_assign]` | `env_assign:1` |
| P3 | `{"token": "abc123xyz"}` | `{"token": [REDACTED:kv_secret_assign]}` | `kv_secret_assign:1` |
| P4 | `Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.sigpart` | `Authorization: [REDACTED:auth_header]` | `auth_header:1` |
| P5 | `Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.sigpart` in a message body | `Bearer [REDACTED:bearer_token]` | `bearer_token:1` |
| P6 | `--api-key=abcdef123456` | `--api-key=[REDACTED:cli_flag_secret]` | `cli_flag_secret:1` |
| P7 | 3-line `-----BEGIN RSA PRIVATE KEY-----` block | `[REDACTED:private_key_block]` (one marker) | `private_key_block:1` |
| P8 | `secret_key: "aVeryLongBase64LookingValue12345678"` | `secret_key: [REDACTED:private_key_inline]` | `private_key_inline:1` |
| P9 | `DATABASE_URL=postgres://app:s3cr3t@db.internal:5432/app` | `DATABASE_URL=postgres://app:[REDACTED:conn_string_password]@db.internal:5432/app` | `conn_string_password:1` |
| P10 | `https://deploy:hunter2@registry.internal/v2/` | `https://deploy:[REDACTED:url_basic_auth]@registry.internal/v2/` | `url_basic_auth:1` |
| P11 | `http://a1b2c3d4e5f60718293a4b5c6d7e8f90:0123456789abcdef0123456789abcdef@hooks.example:7643/7` | `http://a1b2c3d4e5f60718293a4b5c6d7e8f90:[REDACTED:dsn_secret]@hooks.example:7643/7` | `dsn_secret:1` |
| P12 | `AKIAIOSFODNN7EXAMPLE` in a log line | `[REDACTED:cloud_key_shape]` | `cloud_key_shape:1` |
| P13 | `~/projects/trouble/internal/scrub/engine.go:118` | `[REDACTED:path_home_root]/projects/trouble/internal/scrub/engine.go:118` | `path_home_root:1` |
| P14 | a 48-char mixed base64 blob `Qw9zXk2Lm7Tp4Rv8Bn1Yh6Jd3Fg0Sa5Ce2Ui9Ol4Wq7` | `[REDACTED:entropy_token]` | `entropy_token:1` |

**Negative vectors — the output must be byte-identical and `Redactions` must be `0`.**

| # | Input | Why it must survive |
|---|---|---|
| N1 | `{"event_id":"9f2c1d3e4b5a6c7d8e9f0a1b2c3d4e5f"}` | 32 hex is a Sentry event id, not a token: below the entropy floor and not name-driven |
| N2 | `registered project a1b2c3d4e5f60718293a4b5c6d7e8f90 for hooks.example` | a DSN public key is the one allowed credential |
| N3 | `build 9c1f0ab` and 40-hex `d1e8f0a9b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2` | git SHAs are pure hex; the entropy rule rejects hex-only runs |
| N4 | `ev_01J9Z6Q0M2X4T8V1K7B3N5R8WD` | a ULID is an identifier; Crockford base32 has no lowercase, so the entropy rule rejects it |
| N5 | `POSTGRES_MAX_CONNECTIONS=20` | not a sensitive name; redacting by association destroys diagnostics |
| N6 | `password reset required for user alice` | no assignment operator and no value shape; a word is not a secret |
| N7 | `127.0.0.1:7643` and `192.168.1.14` and `[::1]:7644` | loopback and RFC1918 identify nobody outside this host |
| N8 | `[REDACTED:env_assign]` and `[SCRUB-TRUNCATED:4096]` | markers are fixed points |
| N9 | `token bucket refilled at 200/s` | no value, no name-driven assignment |
| N10 | 64-hex `c3ab8ff13720e8ad9047dd39466b3c8974e592c2fa383d4a3960714caef0c4f2` | a sha256 digest is not a credential |
| N11 | `verify_window = "10m"` (a TOML line) | a duration is not a secret; `path_disclosure` is off by default |
| N12 | `https://hooks.example:7643/api/1/envelope/` | a plain URL without userinfo is not a secret |

**The end-to-end seeded-secret test (`e2e_ledger_test.go`).** Boot the daemon in-process against a temp
state root with a temp config and a fake project, then push 12 synthetic secrets through the **real** paths:
a sentinel envelope POST carrying a bearer header, a DSN with a secret in the message, an env dump and a
conn string in the stack; a collector journal tail with a private key block; a config snapshot echoing
`Project.SecretKey`; a skill candidate whose play TOML embeds a CLI flag secret; an issue body with an auth
header; a board row whose brief embeds an email and a public IP; a research context with a JWT; a spool
enqueue and a satellite forward carrying the same payloads. Then:

1. `grep -rF` each of the 12 secret literals across every file under the state root (`ledger/`, `spool/`,
   `skills-local/`, `backups/`): **0 hits required** (this is the guardrail "secrets never in the ledger
   except DSN public keys").
2. `grep -rF` the DSN public key and the Sentry event id: **≥1 hit each required** (the allowed credential
   and the identifier must be present, or the test is proving the wrong thing).
3. Count `[REDACTED:` occurrences per ledger file: **>0** in every file that carried a scrubbed payload, and
   `Σ Record.redactions > 0`; the `by_rule` map over the whole run must contain all 12 firing rule names.
4. Re-run the daemon's read path over the finished ledger with `Verify`: **0 hits** (a second, independent
   check that the persisted bytes are scrubbed, not merely that the seed strings are absent).
5. Assert the fail-closed path: inject a rule that times out and confirm the ledger receives a metadata-only
   record plus a `gap` with cause `scrub_failed`, and that no partial payload appears.

**AC-derived tests.**

- **AC-18 (remote + language matrix).** Two `*Engine` instances built from the same config bytes with
  different `host_id`s; one host posts a Go-SDK envelope whose exception message embeds a DSN with a secret,
  the other curls a JSON body embedding a conn string. Assert: both land in the same project group on the
  hub; the canonical `sig` computed on both paths is equal; the hub's re-scrub reports `Redactions == 0`
  (idempotence across the wire); and a `grep -F` of both seeded secrets over the hub's ledger plus spool is
  empty. Quota behaviour under flood is SPEC-04's test; this test covers the scrubbing half of AC-18.
- **AC-22 (dedup totality).** The same underlying bug is described three ways — a PSI/disk sensor event, a
  sentinel SDK event, and a collector journal line — each including the same secret-bearing message. After
  normalization and scrubbing, assert the three scrubbed payloads are byte-identical where they must be, the
  three digests are equal, and exactly one group and one incident exist. The scrubber's contribution is the
  determinism that makes "same bug" mean "same scrubbed bytes"; the one-group/one-issue assertion is SPEC-05's.

## 8. hilo impact

- **Created:** `internal/scrub/` — `engine.go` (New, entry points, pass driver), `rules.go` (the built-in
  table, compile + validate), `dsn.go` (the `dsn_part` parser), `paths.go` (the `path_allowlist` scanner),
  `entropy.go` (the entropy + hex-rejection scanner), `shield.go`, `stats.go`, `config.go` (strict decode),
  plus `internal/scrub/testdata/{vectors,corpus}/`.
- **Fan-out:** one dependency only — `internal/types` (ScrubRule, ScrubResult, ScrubTarget, ScrubStats,
  Record, Project, ForwardEnvelope) — plus the standard library (`regexp`, `unicode/utf8`, `context`,
  `errors`, `strings`, `sort`, `sync/atomic`). No `internal/ledger`, no driver, no network, no filesystem:
  the package is pure, which is what makes it testable as a vector suite and safe to call from any goroutine.
- **Fan-in:** 8 packages call it — `internal/sentinel`, `internal/sensors`, `internal/lifecycle`,
  `internal/issues`, `internal/flow`, `internal/skills`, `internal/research`, and `internal/ledger`
  (boundary `Verify` only). hilo graph: `internal/scrub` is the highest fan-in leaf after `internal/types`;
  nothing imports back into it.
- **Blast radius:** greenfield repository `~/trouble` — no fleet repo, path, unit or service is
  touched, and every fleet-specific value in this spec appears only inside marked examples. The production
  risk is asymmetric and stated: a **false negative** here leaks into an append-only, git-distributed
  ledger (unauditable), while a **false positive** at the persistence boundary refuses a record (available
  again on the next event of the same sig). The mandatory set is therefore biased toward refusal, the only
  heuristic rule (`entropy_token`) is excluded from the boundary check, and `path_home_root` is the only
  rule whose output keeps part of the original text. Because this package is the last gate before
  persistence, its rule table is the highest-value code in the repository: the index owner records it in
  SPEC-INDEX §4's ownership table and every change to that table must ship with a vector and a corpus re-run.
