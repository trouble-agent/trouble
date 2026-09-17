# SPEC-04 — sentinel: ingestion contract, grouping, releases, collectors (trouble v0.1)

Spec: SPEC-04
Area prefix: TROUBLE-SENTINEL
Package: internal/sentinel
Consumed types: Project, SentryEvent, ClientReport, DiscardCount, RateLimitDecision, Group, GroupCounters, LossPolicy, Record, RecordKind, Origin, Actor, Sig, SigSource, GapRecord, Evidence, SourceLiveness, ScrubResult, ProjectRuntime, CollectorParser, RouteMode, RouteDecision, RouteConfig, CodeplaneContext
Local types: dsn, envelopeHeader, envelopeItem, itemPolicy, authMaterial, quotaWindow, ledgerSink, scrubber, lineSource, logLine, assembleState, tailState, releaseOrder, routeTable, routeMatch
ACs: AC-10, AC-11, AC-12, AC-13, AC-14, AC-15, AC-18, AC-19, AC-22, AC-28, AC-31
PRD: §04b, §10, §11

## 1. Purpose

`internal/sentinel` is trouble's code plane: an embedded, Sentry-SDK-compatible ingestion service
plus the SDK-less on-ramps (generic JSON, log collectors) that all converge on **one signature
space** (non-negotiable #7) shared with the sensors and the issue desk.

Four properties are design constraints, not features:

1. **SDK wire compatibility is a contract, not an approximation.** Modern SDKs speak *envelopes
   only*; `/store/` is deprecated upstream and is accepted here for legacy clients with a counter
   (TROUBLE-SENTINEL-012). The route table, success body, error shape, compression, auth forms and
   rate-limit headers are pinned in §2/§5 and enforced by golden-string tests.
2. **A quota breach destroys evidence.** Official SDK behavior on 429 is *discard*, so refusing
   events silently converts "over quota" into "the group is quiet, therefore fixed". The loss
   policy is therefore a configuration decision with three pinned behaviours (§3.9), every drop
   writes a ledger record, and verification for a sig whose own events were dropped in the window
   is `invalid`, never `passed` (brief §1.D).
3. **Sentinel never observes itself into a green lie.** A per-project canary is injected through the
   real HTTP ingestion path every `canary_interval`; a canary that does not land makes verification
   INVALID (SPEC-05 §3.5) and emits a `gap`/`canary_missing`. `client_report` envelope items are
   parsed, so SDK-side drops are visible data rather than silence.
4. **The ingestion listener is an abuse surface.** Size caps, a gzip-bomb guard, a concurrency cap,
   per-IP and per-project rates, read/write timeouts, an X-Forwarded-For trust policy and a
   bind-matrix auth rule are all pinned here (§3.7), because a DSN public key is a public key.

Sentinel emits exactly four ledger record kinds — `event`, `group`, `gap`, `canary` (SPEC-INDEX
§3.4) — and mints no cross-plane identities: it computes a `Sig` and hands it to the ledger, the
dedup core and the ladder (SPEC-05), which own incident identity.

## 2. Interface

### 2.1 Routes (the complete route set of the ingestion listener; port 7643, config-driven)

| Method | Path | Purpose | Accepted auth | Success | Status family on failure |
|---|---|---|---|---|---|
| POST | `/api/{project_id}/envelope/` | **primary** — Sentry envelope (newline-framed headers + items) | X-Sentry-Auth, envelope `dsn` header, `?sentry_key=` | `200 {"id":"<event_id>"}` | 400 default; 401 auth; 429 quota |
| POST | `/api/{project_id}/store/` | **legacy compat**, deprecated upstream: accepted, counter + TROUBLE-SENTINEL-012, `X-Sentry-Deprecated: store` | same three forms | `200 {"id":"<event_id>"}` + deprecation header | same as envelope |
| GET | `/api/{project_id}/` | DSN probe (reachability + project state, used by `trouble init`, `trouble config explain` and ops) | `?sentry_key=` or X-Sentry-Auth | `200 {"id","slug","enabled","status","canary_last_ts"}` | 401 when auth is present but invalid |
| POST | `/api/{project_id}/event/` | **generic JSON** — trouble's own dialect (PRD §04b), NOT a Sentry route; one curl from any runtime | `?sentry_key=` or X-Sentry-Auth | `200 {"id":"<event_id>"}` | 400 default; 401 auth; 429 quota |

`{project_id}` is the numeric project id as a string (1..2^31-1). Any other method on these four
paths → 405 + TROUBLE-SENTINEL-021. Any other path on the listener → 404 with an empty body and the
`ingest_404_total` counter (never a JSON error body: probe traffic must not be able to write ledger
records). `GET /health.json` lives on the dashboard listener (SPEC-10 §2), never here.

### 2.2 Go surface

```go
package sentinel

// Config is the resolved [sentinel] block (SPEC-12 §2 owns precedence; sentinel never reads the
// environment or the config file itself).
type Config struct {
    Bind                     string   // default "127.0.0.1:7643"
    AdvertisedHost           string   // DSN host; an IP literal or a bind address is refused
    AdvertisedHosts          []string // extra names accepted as a valid DSN host
    Scheme                   string   // "http" | "https"; "https" requires ProxyTrust != "none"
    RequireSecret            bool     // default false
    MaxEnvelopeCompressed    int64    // default 204800 (200KB)
    MaxEnvelopeDecompressed  int64    // default 1048576 (1MB)
    MaxItemBytes             int64    // default 262144 (256KB)
    MaxLineBytes             int      // default 65536
    MaxConcurrent            int      // default 64
    ReadTimeout, WriteTimeout, IdleTimeout, ReadHeaderTimeout types.Duration
    PerIPRate                string   // default "600/min, burst 60"
    GroupFlush               types.Duration // default "5s"
    CanaryInterval           types.Duration // default "10m"
    CanaryProject            string         // project id that carries canaries
    SpoolBudgetBytes         int64          // default 268435456
    DiskBudgetBytes          int64          // default 2147483648
    LossPolicy               types.LossPolicy
    ProxyTrust               string   // "loopback" | "none" | "explicit-list"
    TrustedProxies           []string // CIDRs, used only when ProxyTrust == "explicit-list"
    AllowQueryKeyNonLoopback bool     // default false
    LedgerWait               types.Duration // default "2s"; ingest backpressure ceiling
    Projects                 []types.Project
    Collectors               CollectorConfig
}

type Server struct{ /* http.Server, project index, group index, quota windows, spool, collectors */ }

func NewServer(cfg Config, w ledgerSink, sc scrubber) (*Server, error)
func (s *Server) Handler() http.Handler                 // mounted on the ingestion listener by SPEC-12
func (s *Server) Start(ctx context.Context) error       // starts collectors + canary loop + group flusher
func (s *Server) Drain(ctx context.Context) error       // SIGTERM: stop accepting, flush groups, close spool
func (s *Server) ProjectRuntime(projectID string) (types.ProjectRuntime, bool) // SPEC-10 quota/canary view
func (s *Server) Sources() []types.SourceLiveness       // collector + sentinel liveness for HealthResponse
func (s *Server) ReleaseCoverage(projectID, release string) (canaryOK bool, lastEventTS string)
func (s *Server) InjectCanary(projectID string) (eventID string, err error)
func (s *Server) ObserveRecord(rec types.Record) error  // hub-side group index update for forwarded records
func (s *Server) Groups() []types.Group                 // read view for SPEC-10; never mutates
func (s *Server) SigOf(ev types.SentryEvent) (types.Sig, error)
func (s *Server) CodeplaneFor(sig types.Sig) (types.CodeplaneContext, bool) // cross-plane bundle for an open group (§3.9a; SPEC-05 §3.13a)

type ledgerSink interface { Append(types.Record) error; LastSeq() uint64 }            // *ledger.Writer
type scrubber interface {
    Scrub(b []byte, targets []string) (types.ScrubResult, error)                       // *scrub.Scrubber
}

type lineSource interface { // implemented in-package: journalSource, fileTailSource
    Name() string
    Lines(ctx context.Context) (<-chan logLine, <-chan error, error)
    Close() error
}
type CollectorConfig struct { JournalUnits []string; FileTails []string; Parsers []types.CollectorParser }
```

`ledgerSink` and `scrubber` are package-private consumer-side interfaces satisfied by concrete
types in `internal/ledger` and `internal/scrub`; only shared types (`Record`, `ScrubResult`, derived
from `ScrubResult`) cross the boundary. Dependency direction stays one-way (SPEC-TYPES §4).

### 2.3 DSN — grammar, generation, rotation, revocation

```
{scheme}://{pubkey}[:{secret}]@{host}[:{port}]/{project_id}
```

| Element | Pin |
|---|---|
| `scheme` | `http` or `https`; `https` requires `proxy_trust != none` (a TLS terminator in front) or startup fails with TROUBLE-SENTINEL-009, causes `["https_without_terminator"]` |
| `pubkey` | exactly 32 lowercase hex; the ONLY unredacted credential in the system (brief §1.C) |
| `secret` | exactly 32 lowercase hex, emitted only when `require_secret = true`; 16-hex secrets are refused (TROUBLE-SENTINEL-006, causes `["secret_length"]`) and listed in `docs/sentinel-compat.md` as a divergence from Sentry's older 16-hex secret |
| `host` | must be in `{advertised_host}` ∪ `advertised_hosts`. An IP literal, `0.0.0.0`, `localhost`, or the bind host of another service is refused at generation and at config load (TROUBLE-SENTINEL-009) — a DSN is baked into every deployed app, so a bind address here is a fleet-wide silent-no-report |
| `port` | optional; omitted when it equals the scheme default, else `7643` |
| `path` | **ends at the base**: `/{project_id}` exactly — no `/api`, no trailing slash, no query. SDKs append `/api/{project_id}/envelope/` themselves |
| `project_id` | numeric-as-string, 1..2^31-1; a DSN whose project id differs from the request path → TROUBLE-SENTINEL-006, causes `["dsn_project_mismatch"]` |

Generation: `trouble init` mints `crypto/rand` 32-hex keys per project and refuses to write a DSN
whose host is not an advertised name. Rotation: `trouble sentinel rotate-key <project>` installs a
second pubkey with `valid_until = now + 24h` so reporters roll without a blind window; both keys
resolve to one project inside the overlap. Revocation: `trouble sentinel revoke-key` sets
`valid_until = now`; subsequent requests fail TROUBLE-SENTINEL-006. Key material changes are configuration
mutations and are recorded by SPEC-12 as `config` records — sentinel emits only its own four kinds.

### 2.4 Auth forms — and the ledger must know which one each project uses

| Form | Where | Notes |
|---|---|---|
| `x_sentry_auth` | `X-Sentry-Auth: Sentry sentry_version=7, sentry_key=<32hex>, sentry_secret=<32hex>, sentry_client=<str>` | scheme token `Sentry` required; comma-separated `k=v`; unknown keys ignored |
| `envelope_dsn` | envelope header line key `dsn` | carries the full DSN; used by browser/mobile SDKs that cannot set headers |
| `query_sentry_key` | `?sentry_key=<32hex>&sentry_secret=<32hex>` | refused on a non-loopback request when `allow_query_key_non_loopback = false` (TROUBLE-SENTINEL-006, causes `["query_key_remote"]`) — a key in a query string lands in proxy logs |
| `generic_json_query` | same query form on `/event/` | the practical form for `curl`, so it is permitted on non-loopback only with `allow_query_key_non_loopback` |

Precedence when several are present: `x_sentry_auth` > `envelope_dsn` > query. Two forms that
resolve to different projects → TROUBLE-SENTINEL-006, causes `["conflicting_auth_forms"]`.

Every accepted event record carries `payload.auth_form`, and `Project.AuthForms` is the deduped union
observed over the ledger (rebuilt at boot, persisted as the project index). Recording the form turns
an SDK interop gap into data instead of a mystery (brief §1.E; judge D6).

### 2.5 TOML shape (all keys config-driven, defaults above; fleet values live in `examples/` only)

```toml
[sentinel]
bind = "127.0.0.1:7643"
advertised_host = "trouble.example.net"      # never an IP, never a bind address
scheme = "http"
loss_policy = "drop-with-counter"            # sample | drop-with-counter | spool-if-light
proxy_trust = "loopback"                     # loopback | none | explicit-list
trusted_proxies = ["127.0.0.1/8", "::1/128"]
canary_project = "1"
canary_interval = "10m"
group_flush = "5s"

[[sentinel.project]]
id = "7"; slug = "payment-api"
public_key = "<32 hex>"; secret_key = ""     # secret only when require_secret = true
quota_epm = 600; disk_budget_bytes = 2147483648
loss_policy = "spool-if-light"; enabled = true

[sentinel.routes]
default = "auto"                             # auto | direct | proxy — the sensor transport routes (§3.10a)
per_class = { "psi:io_pressure" = "direct",  # sig-prefix → route; longest prefix wins
              "sentinel:sha256v1" = "proxy" }

[sentinel.collector]
journal_units = ["legacy-daemon"]
file_tails = ["/var/log/legacy/err.log"]
parse = ["go-panic", "py-traceback", "node-reject"]

[sentinel.collector.parser.node-reject]
flush_timeout = "150ms"
```

## 3. Data model

### 3.1 Envelope framing (pinned)

```
<header line>            JSON object, one line, max 8KB
<item header line>       JSON object, one line, max 8KB
[<item body>]            exactly `length` bytes when `length` is present
<item header line>
...
[optional trailing LF]
```

| Rule | Pin |
|---|---|
| Header keys | `event_id` (32 hex), `dsn`, `sentry_client`, `sentry_version`, `content_type`; unknown keys ignored; a non-object or unparsable first line → TROUBLE-SENTINEL-001 |
| Item header keys | `type` (required, ≤64 chars), `length` (optional int ≥0), `content_type`, `filename`, `attachment_type` |
| `length` present | read exactly `length` bytes, then exactly one LF must follow; a different byte → TROUBLE-SENTINEL-001 |
| `length` absent | the item body ends at EOF, or before the next line that parses as an item header (≤8KB lookahead over at most 64 lines). SDKs emit compact single-line JSON for `event`/`client_report`/`session`; a multi-line body without `length` is truncated at the first line that parses as an item header |
| Binary item types | `attachment`, `minidump`, `profile`, `replay` REQUIRE `length`; absent → TROUBLE-SENTINEL-001, causes `["length_required"]` |
| Zero items | legal → `200 {"id":"<header event_id or synthesized 32hex>"}` + `empty_envelopes` counter; never 400 |
| Compression | whole-envelope `Content-Encoding: gzip` (SDK default). `identity` and `gzip` accepted; anything else → TROUBLE-SENTINEL-022. `deflate`/`br`/`zstd` → TROUBLE-SENTINEL-022, causes `["unsupported_encoding"]` |
| Size caps | compressed ≤200KB (TROUBLE-SENTINEL-002), decompressed ≤1MB (TROUBLE-SENTINEL-003), single item ≤`max_item_bytes` (002, causes `["item_too_large"]`), header/body line ≤8KB/64KB (001) |
| Store route body | the event JSON directly (optionally gzip'd); `application/json` or `application/x-www-form-urlencoded` with a `sentry_data` field (pre-2020 SDKs) → the JSON is extracted from the field. Any other content type → TROUBLE-SENTINEL-022 |

### 3.2 Per-item-type policy — never reject the envelope, always account

| Item `type` | Policy | Ledger effect | Counter |
|---|---|---|---|
| `event` | parse → scrub → sig → group | `event` record (full scrubbed `SentryEvent`) + `group` record per §3.3 | `events_total` |
| `client_report` | **parsed**, never dropped | `event` record, `payload.item_type = "client_report"`, `payload.client_report` = `ClientReport`; merged into `ProjectRuntime.ClientReportDiscards` | `client_reports_total`, per `DiscardCount.Reason` |
| `session`, `transaction`, `profile`, `replay` | accepted and dropped with counter (deferred planes; SDKs send them blind) | counter only, 200 | `items_dropped_total{type}` |
| `attachment`, `minidump` | accepted and dropped with counter; the body is consumed and discarded, never buffered beyond `max_item_bytes` | counter only, 200 | `items_dropped_total{type}` |
| `check_in`, any other string | 200-and-drop + TROUBLE-SENTINEL-013 | counter only | `unknown_items_total` |

Rule: an unsupported item type NEVER fails the envelope. `SentryEvent.ItemTypes` records every type
seen, including dropped ones, so an SDK that starts sending a new plane is visible in the dashboard
before its data is needed. Raw attachment bytes never touch the ledger.

### 3.3 Signature, grouping and counters

`Sig` is `sentinel:sha256v1:<hex16>`; the full 32-byte digest is the grouping truth
(SPEC-TYPES §6.3). Grouping key = `hex(digest)`.

**Fingerprint resolution order**

1. **SDK override wins.** `SentryEvent.Fingerprint` non-empty → each string is normalized
   (`§3.3 masking`) and `{{ default }}` is replaced *in place* by the default frame list joined with
   `0x1F`. Repeated `{{ default }}` expands repeatedly; non-string or empty entries are dropped; an
   override that reduces to nothing falls through to path 2.
2. **Canonical stack hash.** Up to the first 8 frames of the top exception value, each normalized to
   `in_app|module|function|file_base|LINE|context_line`, prefixed with `level`/`logger`/`culprit`;
   frames beyond 8 collapse to `frames_over=+N`.
3. **Message-derived fallback** when the event carries no stack and no culprit: one synthetic frame
   `msg|<masked message>`. A failure to compute any of the three (panic in the normalizer, an event
   that is not a JSON object after decode) emits TROUBLE-SENTINEL-016 and falls back to the
   message-derived sig.

**Canonical bytes** (LF-terminated lines, including the final LF, hashed with SHA-256):

```
sentinel/sha256v1
level=<level>
logger=<logger_norm>
culprit=<culprit_norm>
frame=<frame_norm>          # 0..8 lines
frames_over=+<N>            # only when more than 8 frames exist
```

with the SDK-override form instead reading:

```
sentinel/sha256v1
fp=<entry_1>0x1F<entry_2>0x1F...      # 0x1F = the single US byte; written \x1f in docs
```

**Masking (`norm_version = 1`), applied in this exact order**: (1) UTF-8 lossy replace; (2) CRLF→LF,
strip trailing whitespace per line, trim each context line; (3) UUID → `UUID`; (4) RFC3339/ISO
timestamp → `TS`; (5) `[\w./\-]+\.(go|py|js|ts|java|rb|rs|php|cs|c|cpp|h):\d+(:\d+)?` → keep the
file stem + `LINE` (column dropped); (6) `0x[0-9a-fA-F]{4,}` → `0xADDR`; (7) `goroutine \d+` →
`goroutine N`; (8) `(?i)\bpid[=: ]\s*\d+` → `pid=PID`; (9) `\b[0-9a-f]{8,}\b` → `HEXID`;
(10) `\b\d+(\.\d+)?(ns|µs|us|ms|s|m|h)\b` → `DUR`; (11) `\b\d{3,}\b` → `N`; (12) `/(?:tmp|var/tmp)/\S*\d+`
→ `/TMP`; (13) `:(6[0-9]{4}|[1-9][0-9]{3,4})\b` → `:PORT`; (14) collapse runs of whitespace to one
space; (15) truncate each line at 512 bytes. Hex addresses, line numbers, PIDs, goroutine ids, hex
ids, timestamps and 3+-digit literals are masked; exit statuses and 1–2 digit values survive as
discriminators. Ports are masked after `file:LINE` so a port can never be mistaken for a line number.

**Grouped test vectors** (canonical bytes → digest = SHA-256(canonical), `short` = hex16; digests were
produced by the reference normalizer and are checked into `testdata/sig_golden.json`):

| Vector | Input | Canonical bytes | `sig` |
|---|---|---|---|
| A — override | `fingerprint=["queue-wedge","{{ default }}"]`, culprit `worker.claim`, 2 frames (`pool.get(timeout=1)`, `raise PoolExhausted(depth=912)`) | `sentinel/sha256v1\nfp=queue-wedge\x1fapp\|worker\|claim\|worker.py\|LINE\|item = pool.get(timeout=1)\x1fapp\|queue\|get\|queue.py\|LINE\|raise PoolExhausted(depth=N)\n` (149 bytes) | `sentinel:sha256v1:d3e1e8197f4da958` |
| B — default | same stack and culprit, no fingerprint | `sentinel/sha256v1\nlevel=error\nlogger=\nculprit=worker.claim\nframe=app\|worker\|claim\|worker.py\|LINE\|item = pool.get(timeout=1)\nframe=app\|queue\|get\|queue.py\|LINE\|raise PoolExhausted(depth=N)\n` (187 bytes) | `sentinel:sha256v1:af7e89fe750191f9` |
| C — message fallback | `message="queue wedge: pool exhausted depth=912"`, no stack | `sentinel/sha256v1\nlevel=error\nlogger=\nculprit=\nframe=msg\|queue wedge: pool exhausted depth=N\n` (93 bytes) | `sentinel:sha256v1:198e825fb0db5e73` |

**`norm_version` bump rule (ties to SPEC-01 §3.3):** any change to the masking set, its order, the
frame shape, the separator, or the header lines changes every digest in the world, so it REQUIRES
`norm_version++`, a re-key of `testdata/sig_golden.json` in the same commit, and a `group` record
carrying `norm_version` on the first group created under the new version. Two `norm_version` values
are two signature spaces and are never merged (SPEC-TYPES §6.3); groups collide across versions only
in the dashboard's "related" list, never in the ladder.

**Group counters and index maintenance**

| Event | Index action | Ledger action |
|---|---|---|
| first event for a digest | create `Group` (grp_ + ULID), `count=1`, `first_seen_ts`, `release_range[0]` | `group` record, payload `op=create` |
| subsequent event | `count++`, `last_seen_ts`, `counters.events++` | no record per event (the `event` record is the audit trail) |
| counter flush | every `group_flush` (5s) or every 100 events per group, whichever first | `group` record, payload `op=flush`, absolute `counters` snapshot + `events_upper_seq` |
| new/last release change | update `release_range`, evaluate regression (§3.4) | `group` record, payload `op=release` |
| dropped/sampled/suppressed event | `counters.dropped`/`suppressed`, `sample_rate` | `group` record at the next flush + the per-event drop record |
| redaction | `counters.redacted_values += Record.Redactions` | carried by the `event` record |

Index rebuild at boot: fold the last `group` record per digest (absolute counters), then replay
`event` records with `seq > events_upper_seq` for that digest. Counts are exact without a full-file
scan, and the ledger stays the only store (no SQLite; SPEC-01 decision). Group reads for SPEC-10 come
from this index via SPEC-01 §3.4's query API.

### 3.4 Releases and regression detection

| Field | Rule |
|---|---|
| `release` | ≤200 chars, trimmed, no whitespace inside; absent → `""` and the group is release-less (still grouped and counted, excluded from the release views) |
| `ReleaseRange` | `[first_seen_release, last_seen_release]`; updated only when the observed release differs from `last_seen_release` |
| ordering | dot/dash-separated numeric segments compare numerically, then lexically, then by prerelease suffix, then by first-observation sequence (`releaseOrder`); strings that no rule can order are `regressed_unconfirmed`, never claimed as newer |
| **regression** | a group whose incident state is `resolved`/closed reappears with a release that orders AFTER its last pre-resolve release → `group` record `op=release`, `payload.regression="confirmed"` (or `"unconfirmed"`), and the sig is handed to SPEC-05, which reopens the SAME incident (brief §1.N) |
| `fixed_in` (release diff) | requires `last_seen_release == from`, no sighting inside the `to` window, AND proven coverage: a canary landed inside the window (`ReleaseCoverage`). Coverage without a canary is UNKNOWN, not fixed |
| release-diff view inputs for AC-19 | `new_in` = groups with `first_seen_release == to`; `fixed_in` = the rule above; `still_open` = groups seen in both; `regressed` = `regression=confirmed` whose release is in `to`. SPEC-10 composes the view from `Group.ReleaseRange`, `Group.FirstSeenTS/LastSeenTS` and `ReleaseCoverage` — sentinel exposes the inputs, the dashboard renders them |

### 3.5 Collector parsers (v0.1: `go-panic`, `py-traceback`, `node-reject`)

Sources: the collector journald child (sentinel-owned, one supervised `journalctl -f -o json
--after-cursor=<c>` per configured scope, cursor persisted at
`spool/collectors/journal-<sha16(scope)>.cursor`, malformed cursor → `--since=<last-ts>` + rescan +
`gap` record per brief §1.F) and sentinel's own file tailer. Sentinel does NOT import a follower from
`internal/sensors`: SPEC-03 keeps its follower type unexported and its scopes serve the sensor rules,
so the two never share a type — and the two scope sets are DISJOINT by boot validation (a scope listed
in both `sensors.journald.units` and `sentinel.collector.journal_units` is a config conflict refused by
SPEC-12 as TROUBLE-LIFECYCLE-001). One journald child per consumer, no shared state, no exported struct.
Both sources hand raw UTF-8-lossy lines to the assembler through the package-private `lineSource`
interface, so nothing new crosses a package boundary.

| | `go-panic` | `py-traceback` | `node-reject` |
|---|---|---|---|
| START line | `^panic: `, `^\s+panic: `, `^fatal error: ` | `^Traceback \(most recent call last\):` or `^[A-Za-z_.]*\d*(Error|Exception|Warning|Exit|Fault): ` | `^UnhandledPromiseRejection`, `^[A-Za-z_$][\w$]*Error: `, `^Error: `, `^node:internal/` |
| CONTINUATION | `^\s+\S` (indented), `^\S+\.go:\d+`, `^\t`, `^\S+\(0x`, `^\s*created by ` | `^  ` (indented), `^File "`, `^\s+\^`, `^raise `, `^During handling`, `^The above exception`, `^  \|` | `^    at `, `^\s*\[cause\]: `, `^Caused by: `, `^\s+code: `, `^\s+at `, `^\s*\^` |
| END / flush | a non-continuation line that is not `^goroutine \d+`; a `goroutine \d+ [` block flushes the event and starts the dump | first non-continuation, non-blank line → flush immediately | first non-continuation line |
| flush timeout | 250ms of silence, cap 1s total | 200ms | 150ms |
| level | `fatal` (`fatal error:`/`panic:`), else `error` | `error` | `error` |
| message | first line, masked | exception line (`ExcClass: text`) | first line |
| culprit | `goroutine 1` frame immediately above `panic:` | last `File "..."` frame's function | first `at` frame's function/`<module>` |
| stack order | source order, panicking frame last | source order (oldest first), raised frame last | source order |
| sig seed (`SigFields`) | `message`, `frame[0..2]`, panicking function | exception class, `frame[last].function`, `frame[last].file` | `message`, `frame[0].function`, `frame[0].file` |

**False-grouping guards (all pinned, all tested)**

1. One line starts at most one event: parsers are tried in configured order, first match wins; the
   loser never sees the line (`parser_ambiguous_total` incremented).
2. A START line arriving mid-assembly flushes the partial (`TROUBLE-SENTINEL-018`, `partial:true`,
   `payload.flush_reason="superseded"`) and begins a new event — two interleaved tracebacks on one
   stream produce two events, never one merged event.
3. Assembly state is keyed by `(source, parser)`; two sources can never share a partial.
4. A line longer than `max_line_bytes` (64KB) is truncated, flagged `truncated:true`, and the event
   size is capped at 1MB (`TROUBLE-SENTINEL-017` on a parse failure, `018` on the size cap).
5. Non-UTF-8 bytes → U+FFFD replacement + `non_utf8_total`; the event is never dropped for encoding.
6. The assembler uses the journald `__REALTIME_TIMESTAMP` when present, else the receive time; log
   text timestamps never become the event `TS`; skew beyond 5m is recorded as
   `payload.clock_skew_s` (SPEC-12 §6.5).
7. A partial flushed by timeout carries `partial:true` and never claims a complete stack; its own sig
   is stable because the framing marker is masked before hashing.

**File-tail rotation and truncation handling**: each batch re-stats `(dev, ino, size)`. An ino change
→ reopen from offset 0 and emit `gap` cause `file_rotated`; a size smaller than the last offset →
seek 0 and emit `gap` cause `file_truncated`. The tailer keeps a 256KB ring of the pre-offset bytes
so a rotation mid-event flushes the assembled partial instead of losing the head of a traceback.
Offsets persist at `spool/collectors/<parser>-<sha16(path)>.offset` (0600), inside the pinned state
root layout (SPEC-01 §6.1) — no fifth state directory is minted. A source removed or rotated away
entirely emits `gap` cause `file_removed` and marks the parser's `SourceLiveness` entry dead.

### 3.6 Generic JSON endpoint (`POST /api/{project_id}/event/`)

One curl from any runtime (AC-15, AC-18). PRD §04b names this route; it is trouble's dialect and is
deliberately NOT a Sentry path (a real SDK never calls it), and it is never written into a DSN.

| Field | Required | Type / limit | Notes |
|---|---|---|---|
| `message` | one of `message`/`exception` | string ≤16KB | masked and scrubbed like any event message |
| `exception` | one of | `{type, value, stack:[]frame}` | `frame = {file, function, line, in_app, context_line}` |
| `level` | no | enum, default `error` | unrecognized level → `error` + `invalid_level_total` |
| `culprit`, `logger`, `platform` | no | string ≤256 | `platform` defaults to `generic` |
| `release`, `env` | no | string ≤200 | feed §3.4 unchanged |
| `fingerprint` | no | array of strings ≤32 | same resolution order as §3.3 |
| `tags` | no | map[string]string, ≤32 pairs | keys/values ≤128; scrubbed |
| `extra` | no | object ≤8KB serialized | scrubbed; `extra.fingerprint` is ignored (the top-level field wins) |
| `timestamp` | no | RFC3339 | default = receive time |
| `event_id` | no | 32 hex | default = generated; used as the 200 body id and the idempotency key |

`sig` derivation is the identical pipeline: the body becomes a canonical `SentryEvent`
(`platform="generic"`), then §3.3 runs unchanged — so a bash script and a Go SDK reporting the same
bug class land in the SAME group (AC-18, AC-22). Auth is the DSN public key in any of the §2.4 forms;
`?sentry_key=` is the documented curl form. Body cap, gzip handling, quota, loss policy, canary
exemption and error shape are byte-identical to the envelope path. A body that is not a JSON object,
exceeds a limit, or carries none of `message`/`exception` → 400 + TROUBLE-SENTINEL-019 with
`causes` naming each field.

### 3.7 Request, connection and trust limits

| Limit | Pin | Failure |
|---|---|---|
| `MaxHeaderBytes` | 8KB | connection closed |
| `read_header_timeout` / `read_timeout` / `write_timeout` / `idle_timeout` | 5s / 10s / 10s / 60s | TROUBLE-SENTINEL-001 (truncated body) or 408 on the header phase |
| `max_concurrent` | 64 in-flight requests | over cap → queued ≤1s, then 429 reason `overloaded` (see §5 note) |
| per-IP | 600 req/min, burst 60 | 429 `1:error:ip:overloaded:` |
| per-project | `quota_epm` events/min | §3.9 |
| envelope | 200KB compressed / 1MB decompressed / 256KB per item | 413 / 413 / 413 |
| gzip-bomb guard | the decompressed cap is enforced by a limited reader (memory bound = cap + 64KB); additionally a ratio guard refuses compressed:decompressed > 100:1 once output passes 100KB | TROUBLE-SENTINEL-003, causes `["decompressed_cap"]` / `["compression_ratio"]` |

**Proxy trust and X-Forwarded-For.** XFF is honored only when the immediate peer is inside
`trusted_proxies` (rightmost-untrusted-hop algorithm: walk XFF from the right, skip hops in
`trusted_proxies`, the first non-trusted address is the client IP). With `proxy_trust = "none"` XFF is
ignored entirely and the peer address is the client IP. The client IP is used for per-IP limits and
stored only as `client_ip_hash = sha256(ip)[:16]` — a public ingestion surface must never write
attacker IPs into a git-distributed ledger.

**Bind-matrix auth (brief §1.P).**

| Bind / zone | Accepted | Refused |
|---|---|---|
| loopback (`127.0.0.1`, `::1`) | pubkey DSN in any form; secret optional | — |
| non-loopback LAN/tailnet IP | per-project `ingest_token` in `X-Sentry-Auth` as `sentry_secret`, OR a trusted proxy in front | bare public-key DSN → 401 TROUBLE-SENTINEL-006, causes `["zone_not_permitted","token_required"]` |
| public (behind a reverse proxy) | reverse proxy MANDATED: `proxy_trust = "explicit-list"`, `scheme = "https"`, proxy injects the client IP; pubkey DSN is accepted from behind the proxy (Sentry's model) | direct bind without a proxy → same refusal as above |

TLS termination is out of scope for the binary in v0.1: `sentinel` serves plain HTTP and a reverse
proxy provides TLS. A `https` DSN with `proxy_trust = "none"` is refused at startup (009, causes
`["https_without_terminator"]`) because the SDK would otherwise verify a certificate nobody presents.

### 3.8 Canary (brief §1.D)

| Aspect | Pin |
|---|---|
| Mechanism | one synthetic envelope POSTed over loopback to the daemon's own listener (`InjectCanary` → real HTTP path: routing → auth → envelope → scrub → sig → group → ledger) every `canary_interval` (default 10m) for the `canary_project` |
| Content | `level=info`, `message="trouble canary <ULID>"`, `fingerprint=["trouble-canary"]`, `extra.iteration` |
| Ledger | one `canary` record per injection (`payload.phase="injected"`) and one per observation (`payload.phase="observed"`, `sig`, `event_id`) — the observation is what verification reads |
| Exemptions | the canary is exempt from quota, loss policies and breakers: it MUST land during a flood, because that is exactly when evidence matters |
| Exclusion | the canary sig is in the reserved set; it never opens an incident and never reaches the ladder |
| Missing canary | no observation within `2 × canary_interval` → `gap` record cause `canary_missing`, `SourceLiveness{sentinel, alive=false, max_age_s=2×interval}`, and `ProjectRuntime.CanaryLastOK=false` |
| Rule | verification for a window is **INVALID, not passed**, when `Evidence.CanarySeen == false`; and a sig whose own events were dropped by quota inside the window is invalid even when the canary landed |

`ProjectRuntime.CanaryLastTS`/`CanaryLastOK` and `ReleaseCoverage` are the two reads that make the
rule cheap for SPEC-05 and SPEC-10.

### 3.9 Quotas, the official rate-limit header, and the loss policy

Quota units: **event items admitted per project per rolling window**, plus a **global disk budget**
(`sentinel.disk_budget_bytes`, default 2GiB = ledger bytes attributed to sentinel-sourced records +
spool bytes, sampled every 60s). Window = fixed 60s tumbling window aligned to boot, previous window
retained for one window for the verification inputs. Rejected/sampled/spooled events do NOT consume
quota (otherwise a breach would be self-perpetuating).

| Loss policy | At breach | SDK-visible | Accounting |
|---|---|---|---|
| `sample` | deterministic 1/N sampling, `N = ceil(used / quota_epm)`; keep when `sha256(event_id)[:4] mod N == 0` | 200 (SDK keeps sending; the client-side sample rate it believes is unchanged) | per-event record `disposition=sampled`, `error_code=TROUBLE-SENTINEL-014`; `GroupCounters.SampleRate = 1/N` |
| `drop-with-counter` | refuse the items, `429` + `Retry-After` + `X-Sentry-Rate-Limits` + `X-Sentry-Error: TROUBLE-SENTINEL-010` | the SDK MUST discard (official) | per-event record `disposition=dropped_quota`, `TROUBLE-SENTINEL-014`; `counters.dropped` |
| `spool-if-light` | write a compact spool entry (CRC32C + length prefix) and replay when under quota; spool full (256MB default) → drop-oldest-with-ledger-note, then `TROUBLE-SENTINEL-015` | 200 | `disposition=spooled`; `spooled_total`, `spool_bytes` |

**A quota breach destroys evidence, deliberately.** The mandated client behaviour is discard, so the
group goes quiet for reasons that have nothing to do with a fix. The spec's answers, all mandatory:
the drop itself is a ledger record (with sig and disposition) so the loss is auditable; the loss is
counted in `GroupCounters.Dropped` and `ProjectRuntime.DroppedTotal`; the dashboard shows quota usage
per project (`ProjectRuntime`); and verification for a sig that lost its own events in the window is
`invalid` (SPEC-05 §3.5), never `passed`. `spool-if-light` is the only policy that keeps the data — it
is the default for projects whose errors are the point, and it costs disk.

**Header format** `retry:categories:scope:reason:namespaces` (empty namespaces = all categories):

| Trigger | `X-Sentry-Rate-Limits` | `Retry-After` | Status | Code |
|---|---|---|---|---|
| project quota, `drop-with-counter` | `60:error:project:quota_epm:` | `60 - elapsed_in_window` (min 1) | 429 | 010 |
| project quota, ≥95% used but not breached | same value (proactive backoff) | same value | 200 | — |
| global disk budget | `300:error:global:disk_budget:` | 300 | 429 | 011 |
| per-IP | `1:error:ip:overloaded:` | 1 | 429 | 010 |
| concurrency cap | `1:error:global:overloaded:` | 1 | 429 | 010 |
| breaker / kill-switch on the ladder | `120:error:project:breaker:` / `3600:error:global:kill_switch:` | 120 / 3600 | (ladder-side; header shape pinned for reuse) | — |

The proactive (200 + header) path is required for SDK backoff to converge; it fires at 95% of quota
with the same `retry_after` value, which is what turns a flood into an orderly slowdown instead of a
429 cliff. `RateLimitDecision.Header` carries the exact string emitted, so the ledger and the
dashboard can show what the server told the SDK.

### 3.9a The codeplane bundle on admission, and the convergence map (v0.1.1a)

`internal/sentinel` is the only subsystem that knows what the *code* plane was doing at the moment an
incident opens. The `Admit` call therefore carries that knowledge on the `Observation` itself, and the same
map lets a sensor-born admission inherit it (AC-31). SPEC-05 §3.13a owns what the ladder does with the
bundle; this section owns how it is assembled and when it is refused.

**The convergence map (sentinel-owned, read-only).** The sentinel keeps `convergence map[Sig]GroupID` — the
open groups whose subject a sensor rule can also observe (a unit, a path, a container, a project). It is
derived from the group store, never from config, and it is rebuilt at boot from the last 15 minutes of
`group` records (`convergence_window`, a constant in v0.1: the map is a correlation aid, not a store). It
holds at most 4096 entries (`convergence_max_entries`) and evicts least-recently-seen first; an entry leaves
when its group closes or its last event falls out of the window. One accessor is published:

```go
func (s *Server) CodeplaneFor(sig types.Sig) (types.CodeplaneContext, bool) // open-group facts, or ok=false
```

The ladder calls it on a sensor-born admission (SPEC-05 §3.13a) to fill the sentinel half of that bundle.
The sensor plane never reads the group store directly and never writes the sentinel half of a bundle.

**What the sentinel contributes on `Admit`.** Every admission from this package fills `Observation.Codeplane`
(SPEC-TYPES §3.15.12; the ladder persists it as `Incident.Codeplane`) with the facts the sentinel is the
authority for, and nothing it is not:

| Bundle field | Source in this spec | Populated when |
|---|---|---|
| `Side` | constant `"sentinel"` | always |
| `Sig`, `GroupID`, `Project` | the group that crossed the threshold (§3.3) | always on a sentinel admission |
| `Release` | the group's last-seen release (§3.4, `Group.ReleaseRange[1]`) | a release has been seen for the group |
| `Regressed` | the open regression flag for the release pair (§3.4) | a regression is open |
| `Recent` | the group's signature counters, top-5 by count | always on a sentinel admission |
| `Sample` | the `rec_id` of the representative event already attached to the group | the group has a sample |
| `TS` | assembly time, RFC3339 ms UTC | always |

The sensor half (`Side="sensor"`, `RuleID`, `Readings`) is never written here: the two planes write disjoint
fields, so a bundle is never a merge decision.

**The release-mismatch rule (normative).** Assembly compares the bundle's `Release` with the project's
*running release* — the newest release seen for that project under §3.4's `releaseOrder`, i.e. the release
the sentinel believes is deployed. Both values non-empty and unequal (older or newer) means the group
describes code that is not the code running, so the bundle is **discarded at assembly**:

1. `Observation.Codeplane` is left `nil`. The mismatched bundle never reaches the ladder, and therefore
   reaches neither the agent rung nor the research request (SPEC-07 §3.10a) nor the issue body
   (SPEC-09 §3.13a).
2. Exactly one `gap` record is written: `cause="codeplane_release_mismatch"`, `sensor="sentinel"`,
   `scope=<sig>`, `est_lost=0`, `payload {sig, grp, bundle_release, running_release}`. The pair
   `(sig, bundle_release)` is memoized for `convergence_window`, so a burst of stale-release admissions
   writes one record rather than one per event.
3. The admission proceeds unchanged: the ingestion status was committed by §2.1 before assembly, the
   discard path serves **0 HTTP responses of its own**, and the incident opens exactly as it would with no
   bundle at all.

A bundle is context, never evidence: it changes no rung, no gate and no verification input (SPEC-05 §3.10,
§3.13a).

Edge cases owned by this section:

- **Group closed between assembly and the ladder call** — the bundle still describes the observed moment
  (`TS` is the ordering authority); the ladder does not re-read the store.
- **Empty `Release`** — an absent fact, never a mismatch: the field stays empty and the bundle is kept (§3.4).
- **A stale convergence entry** — `CodeplaneFor` returns `ok=false` after the group closed or aged out, and
  the sensor-born admission proceeds with its own rule context only.
- **Restart between assembly and consumption** — the persisted `Incident.Codeplane` is the copy of record
  (SPEC-05 §3.13a); the map is not consulted a second time.

`GapRecord.Cause` gains the value `codeplane_release_mismatch` (§3.9a) — reported as a TYPES-GAP line so
SPEC-TYPES §3.7's cause set stays the single source of truth.

### 3.10a Sensor transport routes — Route A (direct) and Route B (proxied)

The in-code sensor (the trouble-sensor SDK shim, or the agent's own sensor library) writes to *its*
trouble agent, and that agent decides how the event reaches the system. Two routes exist, both
first-class, both configured, neither a fallback of the other:

| Route | Path | Auth | What it is for |
|---|---|---|---|
| **A — direct** | sensor → local daemon over loopback: the envelope endpoint on `ingest.bind` (default `127.0.0.1:7643`) | the loopback bind matrix (§2.4, §3.7): pubkey-DSN alone accepted on loopback | zero network hops, zero new auth: the local reflex path |
| **B — proxied** | sensor → local daemon → **hub** daemon: the satellite forward path (`ForwardEnvelope`, `protocol_version=1`, idempotency key, bounded spool — SPEC-12 §3.7) | local DSN on the first hop; `hub.token` + the reserved forward project on the second | evidence that must reach the canonical ledger (fleet history, hub-owned research, multi-host grouping) |

**The auto rule (one rule, two inputs).** `routes.default = "auto"` resolves to **B when this daemon has
a configured hub endpoint** (`hub.url`, the `[hub] endpoint` key of SPEC-12 §3.7) **and `hub.mode =
satellite`**, else **A**. A hub has no upstream, so hub-side sensors always take A; a satellite relays by
default and pays one hop for the canonical ledger. Resolution happens **once per event**, at the transport
seam of this package — the sensor never selects a route itself, so sensor SDKs stay thin and the decision
lives in exactly one auditable place.

| `routes.default` | `hub.url` | `hub.mode` | `per_class` entry for the class | Resolved | Note |
|---|---|---|---|---|---|
| `auto` | `""` | `hub` | — | **A** | no upstream exists |
| `auto` | set | `satellite` | — | **B** | relay by default |
| `auto` | set | `satellite` | `"direct"` | **A** | the per-class override wins over auto |
| `auto` | `""` | `hub` | `"proxy"` | **refused at boot** | a `proxy` route with no hub endpoint is an unachievable claim → TROUBLE-SENTINEL-023 |
| `direct` | set | `satellite` | — | **A** | whole-daemon direct |
| `proxy` | set | `satellite` | `"direct"` | **A** for that class, **B** for the rest | longest sig-prefix match decides per event class |

```toml
[sentinel.routes]
default = "auto"                              # auto | direct | proxy
per_class = { "psi:io_pressure" = "direct",   # sig-prefix → route
              "collector:go-panic" = "proxy" }
```

Matching is **longest sig-prefix wins** over the canonical sig string (`<source>:<algo>v<n>:<hex16>`,
SPEC-TYPES §6.3): `psi:io_pressure` matches every `psi:io_pressure*` sig, `sentinel:sha256v1` matches every
sentinel sig, and a class with no matching prefix takes `default`. The table is validated at boot
(non-empty prefixes, no prefix mapped to two routes, no `proxy` entry without a hub endpoint) →
TROUBLE-SENTINEL-023 with exit 13 **before the listener binds**: a routing policy that cannot be honoured
is found at boot, not under load. Class examples that drive this table: crash-loop and PSI pressure
signatures demand **A** (a local reflex must not queue behind a network hop), while bulk debug/collector
evidence demands **B** (it is archival-grade and belongs in the canonical ledger).

**The decision is recorded, not inferred.** Every record written by the sensor/sentinel path carries
`Origin.Route` (`"A"` | `"B"`, SPEC-TYPES §3.1) — including the A case, where *no relay* is as much a
fact as a relay. The ledger therefore answers "local or relayed?" without a join:
`jq 'select(.origin.route=="B")' ledger/YYYY-MM-DD.jsonl` is the relay feed, and SPEC-10's group and
incident views render the local-vs-relayed split per group. The auto rule, the per-class table and the
two counters (`route_a_total`, `route_b_total`) are printed by `trouble hub status` and in
`/health.json`, so the decision is visible without reading config.

**Route B failure = the existing bounded spool, never event loss.** A satellite that cannot reach its hub
writes the batch to the SPEC-12 §3.7 spool (`spool/forward/NNNNNNNNNN.fwd`, `spool.budget_bytes` 256MB,
drop-oldest with a `gap` note, `spool.gap_reserve_bytes` reserved so the loss notices themselves survive),
retries with exponential backoff, and replays on reconnect. Replay is idempotent by the
`ForwardEnvelope` idempotency key (`sig | norm_version | host_id`), so a batch that was delivered but never
acked lands exactly once. Route B mints no code of its own: spool exhaustion and eviction are
TROUBLE-LIFECYCLE-015/014 (SPEC-12 §5) and satellite-side backpressure is §3.9's 429 shape unchanged.

**Route A failure** is the local listener being down or refusing (§3.9, TROUBLE-SENTINEL-010/011). A sensor
on the same host has no spool of its own: it retries in-process with the same bounded backoff and reports
its drops through `client_report` (§3.3), because a local sensor that quietly buffers would be a second
queue — the one structure this suite refuses to have.

**Sensor SDK surface (three facts, nothing else).** The SDK documents the endpoint (`ingest.bind`), the
DSN form for its bind zone (§2.3), and that **the daemon chooses the route**; a client that picked its own
route would reintroduce the drift this section exists to remove.

### 3.10 Types added to SPEC-TYPES by this spec

```go
type ProjectRuntime struct {          // per-project runtime state for SPEC-10 (quota usage, canary, loss)
    Project              string         `json:"project"`
    QuotaEPM             int            `json:"quota_epm"`
    WindowS              float64        `json:"window_s"`
    EventsWindow         int            `json:"events_window"`
    Remaining            int            `json:"remaining"`
    RejectedTotal        uint64         `json:"rejected_total"`
    DroppedTotal         uint64         `json:"dropped_total"`
    SpooledTotal         uint64         `json:"spooled_total"`
    SampledTotal         uint64         `json:"sampled_total"`
    LegacyStoreTotal     uint64         `json:"legacy_store_total"`
    UnknownItemsTotal    uint64         `json:"unknown_items_total"`
    ClientReportDiscards map[string]int `json:"client_report_discards"`
    AuthForms            []string       `json:"auth_forms"`
    LastEventTS          string         `json:"last_event_ts"`
    CanaryLastTS         string         `json:"canary_last_ts"`
    CanaryLastOK         bool           `json:"canary_last_ok"`
    DiskBytes            int64          `json:"disk_bytes"`
    DiskBudgetBytes      int64          `json:"disk_budget_bytes"`
}

type CollectorParser struct {         // config + health surface for the log collectors (SPEC-04 §3.5)
    Name          string     `json:"name"`           // "go-panic" | "py-traceback" | "node-reject"
    Enabled       bool       `json:"enabled"`
    Kind          string     `json:"kind"`           // multiline
    StartPattern  string     `json:"start_pattern"`  // RE2
    Continuation  []string   `json:"continuation"`   // RE2 list
    FlushTimeout  Duration   `json:"flush_timeout"`
    MaxEventBytes int        `json:"max_event_bytes"`
    Level         string     `json:"level"`
    SigFields     []string   `json:"sig_fields"`
    Sources       []string   `json:"sources"`        // "journal:<unit>" | "file:<path>"
}
```

```json
{"project":"7","quota_epm":600,"window_s":60,"events_window":41,"remaining":559,"rejected_total":3,"dropped_total":3,"spooled_total":0,"sampled_total":0,"legacy_store_total":1,"unknown_items_total":12,"client_report_discards":{"queue_overflow":2,"network_error":5},"auth_forms":["x_sentry_auth","query_sentry_key"],"last_event_ts":"2026-09-16T09:14:03.221Z","canary_last_ts":"2026-09-16T09:10:00.004Z","canary_last_ok":true,"disk_bytes":1048576,"disk_budget_bytes":2147483648}
{"name":"py-traceback","enabled":true,"kind":"multiline","start_pattern":"^Traceback \\(most recent call last\\):","continuation":["^  ","^File \"","^\\s+\\^","^raise ","^During handling","^The above exception"],"flush_timeout":"200ms","max_event_bytes":1048576,"level":"error","sig_fields":["exception_class","frame_last.function","frame_last.file"],"sources":["journal:legacy-daemon","file:/var/log/legacy/err.log"]}
```

## 4. Wiring

### 4.1 Producer / consumer table

| Producer | Consumes | Emits |
|---|---|---|
| `internal/sentinel` | `internal/types` (Project, SentryEvent, ClientReport, Group, Record, Sig, GapRecord), `internal/ledger` (`ledgerSink`), `internal/scrub` (`scrubber`); its own `journalctl` child and file tailer for collectors (no `internal/sensors` dependency) | `event`, `group`, `gap`, `canary` records |
| `internal/lifecycle` | `sentinel.Config` + `sentinel.Server.Handler()` | mounts the ingestion listener on :7643 with a bind preflight (SPEC-12 §2) |
| `internal/ladder` | `Group`, `Sig`, `Evidence.CanarySeen`/`CanaryID` | `incident`, `verify`, `breaker` (never writes sentinel state) |
| `internal/sentinel` -> `internal/ladder` | `Observation.Codeplane` (§3.9a) — the cross-plane bundle assembled on admission, `nil` when the bundle was refused; the ladder persists it as `Incident.Codeplane` (SPEC-05 §3.13a) and reads it back through `CodeplaneFor` on a sensor-born admission | `gap` cause `codeplane_release_mismatch` (mismatch path only) |
| `internal/dashboard` | `ProjectRuntime`, `Groups()`, `Sources()`, `ReleaseCoverage` | reads only |

### 4.2 Boot and drain order

1. Config resolved by SPEC-12 (flag > env > file > default) → sentinel validates it: advertised host
   is a name, `https` implies a terminator, every project's pubkey is 32 hex, `quota_epm > 0`
   (failures are TROUBLE-SENTINEL-007/008/009 at boot, loud).
2. Bind preflight on :7643 fails loud on collision (TROUBLE-LIFECYCLE-003, SPEC-12).
3. Group index rebuilt from the ledger (§3.3); `AuthForms` folded from `event` records.
4. Collectors start (journald follower from SPEC-03, file tailers from persisted offsets) → each
   emits a `gap` record if it cannot attach.
5. Canary loop and group flusher start; `Server.Start` returns only after a first canary observation
   OR after `canary_interval` (whichever first), so boot never claims readiness untested.
6. Drain (SIGTERM): stop accepting, finish in-flight requests within `write_timeout`, flush all group
   counters, fsync + close the spool, close collectors with a final `gap`-free offset write, stop the
   canary loop. Drain is bounded by 5s; the ledger's own crash-loss window stays ≤200ms (SPEC-01).

### 4.3 Remote ingestion and the forward path (AC-14, AC-18)

A remote app reports straight to the hub over LAN/Tailscale with a DSN
(`advertised_host` = the tailnet/LAN name, never an IP); the bind matrix (§3.7) applies. A satellite
that ingests locally and forwards uses THE sentinel wire format — `ForwardEnvelope` with
`protocol_version = 1` and `idempotency_key = sig + "|" + norm_version + "|" + host_id` (SPEC-12
§3.7, brief §1.P). The hub's forward receiver decodes the records, appends them to the ledger, and
calls `ObserveRecord` so the hub's group index advances without a second ingestion path; duplicate
`idempotency_key` values inside the dedup window are accepted-and-ignored (no double count). Sentinel
exposes no forward route of its own: one protocol, one receiver.

### 4.4 Dedup totality (AC-22)

Sentinel contributes exactly one artifact to the shared signature space: a `sentinel:sha256v1:*` sig.
A collector-sourced event uses `SigSource = collector` for the *record* origin but the identical
canonical stack bytes, so a go-panic line collected from journald and the same panic reported by
sentry-go produce the same digest and therefore one `Group` (a group keyed by digest, not by source).
Merging with sensor and issue paths is the dedup core's job (SPEC-01 §3.3, SPEC-05 §3.2): sentinel
never mints an incident, issue or board row.

### 4.5 Sensor transport wiring and the route decision point (AC-28)

```
in-code sensor / SDK-less collector
        │  (1) event → sig → scrub
        ▼
route resolution   ── routes.default + per_class + hub.url + hub.mode ──► A | B   [Origin.Route stamped]
        │
   A ───┴──► local listener (loopback, DSN auth) ─► group ─► ledger
   B ──────► satellite forward path (SPEC-12 §3.7) ─► hub listener ─► group ─► ledger
                  └── hub unreachable ─► spool (256MB, drop-oldest, gap note) ─► replay on reconnect
```

Three properties of this wiring are normative:

1. **The decision point is inside `internal/sentinel`**, at the transport seam, never in the sensor. One
   decision point makes the matrix testable without an SDK (§7 `routes_test.go`) and makes it impossible
   for two clients to disagree about where a class belongs.
2. **Both routes converge on the same downstream code**: scrub → sig → group → ledger. Routes differ in
   hops, never in what a record *is*, which is why a relayed event and a local event of one class produce
   one group (AC-22) and the ladder cannot tell them apart.
3. **The resolved route is stamped before the transport is attempted**, so a record that ends up in the
   spool still carries `origin.route="B"`: the record states the decision, the spool and its ack/gap
   records state the outcome, and the two reconcile — a relayed batch that never landed is visible as a
   gap with a route, not as silence.

## 5. Errors

Status policy: the default failure status is **400 + `X-Sentry-Error` + `{"detail","causes"}`** on
every ingestion route; the table pins the exceptions (401 auth, 405 method, 413 size, 415 media type,
429 rate/quota). `X-Sentry-Error` always carries the code; `causes` is a list of short machine strings.
Client-side protocol failures are counted, and a reject rate above 50% for a project over 5 minutes
emits `gap` cause `ingest_reject_storm` — an unread 400 storm is otherwise invisible (the ALL-GREEN
class).

| Code | Class | HTTP | Trigger | Ledger / measurement |
|---|---|---|---|---|
| TROUBLE-SENTINEL-001 | permanent | 400 | envelope framing invalid: bad header line, length overrun, missing LF after a body, truncated body, oversized line | `reject_total{reason=framing}` |
| TROUBLE-SENTINEL-002 | permanent | 413 | compressed body > 200KB, or a single item > `max_item_bytes` | `reject_total{reason=too_large}` |
| TROUBLE-SENTINEL-003 | permanent | 413 | decompressed > 1MB or ratio > 100:1 (gzip bomb) | `reject_total{reason=decompressed}` |
| TROUBLE-SENTINEL-004 | permanent | 400 | gzip stream invalid (bad magic, CRC mismatch, truncation) | `reject_total{reason=gzip}` |
| TROUBLE-SENTINEL-005 | permanent | 401 | no auth material on any form | `reject_total{reason=no_auth}` |
| TROUBLE-SENTINEL-006 | permanent | 401 | auth material invalid: unknown key, expired/revoked key, 16-hex secret, conflicting forms, DSN/path project mismatch, zone not permitted | `reject_total{reason=auth}`, `ProjectRuntime.AuthForms` unchanged |
| TROUBLE-SENTINEL-007 | permanent | 401 | unknown `project_id` (route or DSN) | `reject_total{reason=project_unknown}` |
| TROUBLE-SENTINEL-008 | permanent | 401 | project disabled (`enabled=false`); the GET probe still answers 200 with `enabled:false` | `reject_total{reason=project_disabled}` |
| TROUBLE-SENTINEL-009 | permanent | boot-time refusal, else 400 | advertised DSN/route unreachable as configured: host is an IP/bind address, host not in the advertised set, or `https` without a terminator | `config` record (SPEC-12) + loud startup failure |
| TROUBLE-SENTINEL-010 | transient | 429 | project quota exceeded, or per-IP/concurrency cap | per-event `event` record `disposition=dropped_quota`; `RateLimitDecision` |
| TROUBLE-SENTINEL-011 | transient | 429 | global disk budget exceeded → loss policy applies | per-event record + `spool_bytes` vs budget |
| TROUBLE-SENTINEL-012 | permanent | 200 | legacy `/store/` route used: accepted, counted, deprecated | `ProjectRuntime.LegacyStoreTotal` |
| TROUBLE-SENTINEL-013 | permanent | 200 | unknown envelope item type dropped | `items_dropped_total{type}` |
| TROUBLE-SENTINEL-014 | permanent | 200 (sample/spool) or 429 (drop-with-counter) | an event was dropped/sampled by the configured loss policy | per-event `event` record `error_code=014`, `counters.dropped`/`sample_rate` |
| TROUBLE-SENTINEL-015 | permanent | 200 | `spool-if-light` dropped an event because the spool budget was full | per-event record + drop-oldest ledger note |
| TROUBLE-SENTINEL-016 | permanent | 200 | fingerprint computation failed → message-derived sig used | `event` record, `payload.fingerprint_fallback=true` |
| TROUBLE-SENTINEL-017 | permanent | 200 | a collector parser failed on a matched line | `gap` cause `parser_error` + `parser_error_total{parser}` |
| TROUBLE-SENTINEL-018 | permanent | 200 | multi-line assembly timed out or was superseded; partial event emitted with `partial:true` | `event` record + `partial_events_total{parser}` |
| TROUBLE-SENTINEL-019 | permanent | 400 | generic JSON body invalid (not an object, missing `message`/`exception`, over a limit) | `reject_total{reason=generic_json}` |
| TROUBLE-SENTINEL-020 | transient | 200 | spool write failed (disk full, permission, torn write) | `gap` cause `spool_write_failed` + loud log; the event is counted `dropped` |
| TROUBLE-SENTINEL-021 | permanent | 405 | wrong method on an ingestion route | `reject_total{reason=method}` |
| TROUBLE-SENTINEL-022 | permanent | 415 | unsupported `Content-Type`/`Content-Encoding` | `reject_total{reason=media_type}` |
| TROUBLE-SENTINEL-023 | permanent | boot-time refusal (exit 13) | `[sentinel.routes]` invalid: unknown route name, empty sig-prefix key, one prefix mapped to two routes, or `proxy` for a class while no hub endpoint is configured (§3.10a) | `config` record (SPEC-12) naming the offending entry; **0 HTTP responses served** |

Every code a subsystem returns to the ladder also appears in the ledger record describing the failure
(`payload.error_code`, SPEC-INDEX §5.3). `RateLimitDecision.Reason` uses the four catalogued values
plus `overloaded` for the per-IP/concurrency paths — a comment update for SPEC-TYPES §3.6, not a new
type. 023 is the one code this round adds to the SENTINEL range (001–023, SPEC-INDEX §3.5): the routes
block is validated before the listener binds, so an unhonourable routing policy is a boot refusal, never a
runtime surprise on one class of traffic.

**The codeplane bundle adds no codes.** Refusing a mismatched bundle (§3.9a) is accounted by a `gap`
record with cause `codeplane_release_mismatch` and by nothing else: the discard is post-admission, so no
HTTP status changes and the SENTINEL range 001-023 is untouched by this round; no ladder error is raised,
because context loss never degrades an incident.

## 6. Edge cases

1. **gzip bomb**: the decompressor is a limited reader capped at 1MB + 64KB, plus the 100:1 ratio
   guard past 100KB of output → TROUBLE-SENTINEL-003; peak memory for one request is bounded by the
   cap, which is what keeps steady RSS sane (§2.2 `MaxEnvelopeDecompressed`).
2. **No `Content-Length` / chunked**: the compressed cap is enforced by counting bytes read; exceeding
   it mid-stream → 413 + 002 without buffering the rest.
3. **Client aborts mid-body** (declared length never arrives): read timeout 10s → 001, logged, and the
   partial is never written to the ledger.
4. **Item `length` shorter than the JSON body**: the trailing bytes are discarded, the item parses,
   and `item_overrun_total` increments — a lying length is data, not a rejection.
5. **Empty envelope / envelope with only `client_report`**: 200 with the synthesized id; a
   `client_report`-only envelope is the SDK saying it dropped events, so it updates
   `ClientReportDiscards` and writes no group.
6. **Duplicate `event_id` inside 10m**: 200 with the same id, no double count, `duplicate_events_total`
   — SDK retry after a flaky network is the normal case, not an attack.
7. **Event with no frames and no message**: the message-derived path covers it; all-empty events
   collapse to one stable sig per `(level, logger, culprit)`, which is the honest grouping.
8. **Non-UTF-8 in a message or stack**: lossy replacement before the scrubber sees the bytes so the
   persistence rule (TROUBLE-SCRUB-007) cannot refuse the record.
9. **Unorderable release strings**: marked `regressed_unconfirmed`, and the reopen decision goes to the
   ladder as an ordinary sig-keyed recurrence (SPEC-05 §3.2) rather than a claimed regression.
10. **Quota exactly at the limit** (`used + n > quota`): the next item breaches; `used == quota` leaves
    zero allowance. A single envelope larger than `quota_epm` is admitted to `quota_epm` items and the
    remainder follows the loss policy, so one big envelope cannot be silently swallowed whole.
11. **Canary during a flood**: the canary is exempt and must land; a sig whose own events were dropped
    in the window makes its verification `invalid` regardless.
12. **Torn spool line** (crash mid-write): the CRC32C + length prefix makes truncation detectable; the
    entry is discarded with a `gap` note (SPEC-01 torn-line discipline, applied to the spool).
13. **Ledger backpressure**: `Append` blocks at most `ledger_wait` (2s) inside the group-commit window;
    past that the request returns 429 `overloaded` and the event is counted dropped-with-record —
    accepted events are never lost silently, and the ≤200ms crash-loss window is stated in every doc.
14. **File removed while an event is assembling**: flush the partial (`partial:true`, 018) + `gap`
    cause `file_removed`; the offset file is kept so a recreate does not replay the world.
15. **Two parsers match one line**: config order decides, the loser never sees it, `parser_ambiguous_total`
    increments. Ambiguity is a configuration signal, not a fork in the data.
16. **Journald cursor gap** (§3.5 cursor fallback): the collector emits `gap` with `est_lost` and keeps
    going; a cursor failure never means "no errors".
17. **Clock skew**: the SDK timestamp is kept as the event `TS`, the ledger record `ts` stays
    receive-time, skew > 5m is recorded in the payload (SPEC-12 tracks cross-host skew as TROUBLE-LIFECYCLE-017).
18. **Kill switch active**: ingestion is NEVER gated by autonomy mode — detection must keep working
    while the ladder is stopped, otherwise a kill-switch looks like a healthy quiet system (brief §1.K).
19. **Hub endpoint configured after the daemon is running** (config reload, SPEC-03 hot-reload discipline):
    `routes.default="auto"` re-resolves on the next event and the class table is swapped atomically.
    Records already stamped keep their stamp, so a group spanning the switch shows both routes — which is
    the honest view of a transport change.
20. **A `per_class` prefix no event ever matches**: legal and inert. The table is routing policy, not a
    registry; `trouble hub status` reports each prefix's match count, so dead policy is visible instead of
    accumulating.
21. **A class that demands `proxy` on a daemon with no hub** (a laptop, or a hub whose sensors were copied
    from a satellite): refused at boot (§3.10a, TROUBLE-SENTINEL-023). Overrides are validated against the
    configured topology, so a copied config cannot silently route evidence into nowhere.

## 7. Testing

| File | Cases | Pass threshold / regression number |
|---|---|---|
| `internal/sentinel/envelope_test.go` | framing: length present/absent, LF enforcement, zero items, trailing LF, 8KB header cap, item overrun, binary-without-length, urlencoded `sentry_data` store body | every fixture in `testdata/envelopes/` (captured sentry-go, sentry-python, @sentry/node shapes) decodes; 0 panics; each malformed case returns its exact code |
| `internal/sentinel/fingerprint_test.go` | vectors A/B/C byte-for-byte canonical output + digest; masking order (16 rules); 9-frame `frames_over` collapse; override interpolation and repeated `{{ default }}`; fallback path; un-bumped change detection | canonical bytes and `sig` match §3.3 exactly; `testdata/sig_golden.json` mismatch fails CI and demands a `norm_version` bump |
| `internal/sentinel/group_test.go` | 6,400 events → 1 group; counters exact after flush; index rebuild equals live counts; `AuthForms` union; per-item-type counters | exactly 1 group, `count == 6400`, rebuild delta 0 |
| `internal/sentinel/release_test.go` | first/last release, ordering rules, regression confirmed/unconfirmed, `fixed_in` gated by canary coverage | regression fires for a newer release; unorderable strings never claim newer |
| `internal/sentinel/quota_test.go` | 429 + `Retry-After` + exact `X-Sentry-Rate-Limits` strings; proactive header at 95%; window roll; three loss policies; spool full → 015 | golden header strings byte-equal; `events_window` never exceeds `quota_epm` |
| `internal/sentinel/limits_test.go` | gzip bomb, ratio guard, no-Content-Length, abort mid-body, concurrency cap, per-IP, XFF trusted/untrusted, bind-matrix refusals | memory peak ≤ cap + 64KB per request; refusals use the pinned codes |
| `internal/sentinel/collector_*_test.go` | per-parser start/continuation/flush vectors in `testdata/logs/{go-panic,py-traceback,node-reject}/`; timeout flush; supersede; interleave; rotation; truncation; non-UTF-8; 64KB line | every fixture yields the expected event count, level, culprit and sig; no cross-parser merge |
| `internal/sentinel/genericjson_test.go` | required/optional fields, limits, `extra.fingerprint` ignored, auth via `?sentry_key=`, sha256 of a bash-reported body equals the SDK-reported body for the same class | same `sig` as vector B when the stack matches |
| `internal/sentinel/canary_test.go` | canary lands through the real HTTP path; miss → `gap`/`canary_missing`; canary survives quota exhaustion; canary never opens an incident | canary observation inside `canary_interval`; `Evidence.CanarySeen` true |
| `internal/sentinel/spool_test.go` | spool write/read, torn entry, drop-oldest, replay under quota, budget accounting | 0 undetected torn entries; `spool_bytes ≤ budget` |
| `internal/sentinel/e2e_test.go` | `httptest` server: envelope + store + generic JSON + collector line → one group → one `event`/`group` record set; two projects on two listeners with different zones (AC-18 lab shape) | one digest per bug class across all three paths (AC-22) |
| `internal/sentinel/load_test.go` | 8 workers, gzip'd 4KB envelopes, one project, 60s | **target 2,000 req/s sustained, p99 ≤ 25ms, p999 ≤ 100ms, 5xx = 0, RSS growth ≤ 8MB**; measured reference: 6,199 req/s with 100-line group-commit and 1,972 req/s with fsync-per-line — the target proves group-commit is actually in use and leaves 3.1× headroom |
| `internal/sentinel/routes_test.go` | the route matrix of §3.10a (6 rows): auto under `hub.url` set/empty × `hub.mode` hub/satellite; per-class override via longest sig-prefix; unmatched class → `default`; boot refusal on `proxy` with no hub endpoint (023) and on a prefix mapped twice; `origin.route` stamped on A and on B | every matrix row resolves to its pinned route; 100% of written records carry a non-empty `origin.route`; the refusal case is exit 13 with **0 HTTP responses served** |
| `internal/sentinel/routespool_test.go` | Route B with the hub killed mid-batch: spool segment written, backoff retries, replay after the hub returns; torn spool segment; drop-oldest with exact footer-derived `EstLost`; duplicate replay of one idempotency key | 0 events lost while `spool_bytes ≤ budget`; every spooled event lands **exactly once**; `origin.route="B"` survives the spool round-trip |
| `internal/sentinel/codeplane_test.go` | bundle assembly (§3.9a): a threshold crossing carries every field the sentinel owns and nothing else; a regression flags `Regressed`; a release-less group leaves `Release` empty and keeps the bundle; the convergence map rebuilds from 15m of `group` records after a restart and evicts least-recently-seen at 4096; a stale-release bundle is discarded -> `Observation.Codeplane == nil` + exactly 1 `gap` cause `codeplane_release_mismatch`; 100 stale-release admissions of one pair write 1 record; `CodeplaneFor` returns `ok=false` for a closed group | every listed field asserted non-empty on the fixture and the sensor half asserted empty; `Codeplane == nil` on the mismatch and **0 HTTP responses served** by the discard path; 0 second gap records for the memoized pair |

AC-derived tests: AC-10 `e2e_test.go` (real SDK envelope shapes + legacy store + DSN auth forms);
AC-11 `group_test.go` (counters and rate); AC-12 `release_test.go`; AC-13 `quota_test.go` (official
header format); AC-14 `limits_test.go` + `e2e_test.go` (non-loopback bind, token/proxy rule);
AC-15 `collector_*_test.go` + `genericjson_test.go`; AC-18 `e2e_test.go` two-project lab procedure
(Go SDK on host A, `curl` on host B, both landing in the same digest, quota held while one floods);
AC-19 `release_test.go` release-diff inputs; AC-22 `e2e_test.go` (sensor/collector/sentinel → one
digest → one group); AC-28 `routes_test.go` (route matrix + override + refusal) + `routespool_test.go`
(B-failure spool replay, exactly-once) + the `origin.route` assertion in `group_test.go`; AC-31
`codeplane_test.go` (bundle assembly, convergence map, mismatch discard, 0 HTTP responses served).

Memory: `TestMain` asserts steady RSS ≤ 80MB after 1,000,000 events (measured trivial path
7.0 → 15.6MB) and ≤ 192MB under the load test; the binary stays inside the 8–15MB budget because
sentinel adds no dependency beyond stdlib (SPEC-TYPES §6.1).

Shipped artifact: `docs/sentinel-compat.md` — the compatibility matrix (supported SDK versions,
the 16-hex-secret divergence, unsupported item types, unsupported encodings) and the "known
divergences" list required by the judge review, regenerated from the same tables by
`make compat-matrix`.

## 8. hilo impact

New package `internal/sentinel` with these files:

```
server.go routes.go routetransport.go envelope.go auth.go dsn.go project.go fingerprint.go group.go release.go
quota.go loss.go spool.go canary.go genericjson.go collectors.go parser_go_panic.go
parser_py_traceback.go parser_node_reject.go tail.go journal.go limits.go errors.go
+ *_test.go and testdata/{envelopes,logs,sig_golden.json}
```

Fan-out (what it imports): `internal/types` (shared types only), `internal/ledger` and
`internal/scrub` behind package-private interfaces, stdlib (`net/http`, `compress/gzip`,
`crypto/sha256`, `encoding/json`, `hash/crc32`, `os/exec` for the collector `journalctl`
child). Fan-in (who imports it): `internal/lifecycle` (mounts the listener, resolves config), `internal/dashboard`
(quota/canary/release reads), `internal/ladder` (indirectly, via `Group`/`Sig` records only). Nothing
in `internal/sentinel` imports `internal/{ladder,flow,issues,registry,dashboard,skills}`, so the
dependency graph stays acyclic and one-way into `internal/types`.

Blast radius: greenfield repository `~/trouble`; no fleet repository, unit, service or path
is touched, and every path, port, hostname, key and quota in this spec is configuration with a default
(non-negotiable #1). Internally, the highest-risk surface is the fingerprint normalizer: changing it
re-keys every sig in the system, which is why it is versioned, golden-file-tested and called out as a
`norm_version` bump in §3.3. The second is `Config.MaxEnvelopeDecompressed`, which is the memory bound
for the whole listener.
