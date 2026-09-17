# SPEC-09 — issue desk: driver contract, github + duckbrain drivers (trouble v0.1)

Spec: SPEC-09
Area prefix: TROUBLE-ISSUES
Package: internal/issues
Consumed types: IssueDriver, IssueRef, EnsureBySigRequest, EnsureBySigResponse, DriverHealth, SpoolEntry, IssueDeskConfig, IssueCaps, IssueDriverConfig, IssueCapState, IssueAttempt, CodeplaneContext, Incident, Group, Evidence, GapRecord, Record, RecordKind, Severity, Duration, Prefix, Origin, Actor, Sig, LadderState, Rung, ScrubResult, ErrorClass
Local types: ghIssueWire, ghCommentWire, ghSearchWire, ghRateLimitWire, dbDocument, dbCommentDoc, anchorEntry, attemptBudget, spoolQueue
ACs: AC-8, AC-22, AC-31
PRD: §07, §11

## 1. Purpose

`internal/issues` is the issue desk: the outlet that turns a sig-keyed incident into a tracked issue in an
external tracker, folds every recurrence into that same issue, and closes it when the sig has been quiet.
PRD §07 fixes the shape — driver architecture, `github` + `duckbrain` ship in v0.1, and the four-method
contract keeps any further driver to one adapter each (interface seam only in v0.1, §2.4, `(v1.0 hand-off)`),
while PRD §11's guardrail fixes the bound:
*dedup + issue caps + quiet-close, so neither a chatty app nor a flappy rule can spray anything*.

This spec pins, normatively:

1. The four-method driver contract (`EnsureBySig`, `Comment`, `Close`, `Healthcheck`) plus `Name()`, with
   the semantics of every input and output field, and the retry class of every failure.
2. Per-method idempotency: `EnsureBySig` is idempotent on sig inside the dedup window and returns
   `Created=false, Commented=true` instead of a duplicate; `Comment` appends once per distinct trigger and
   never twice for the same ledger record; `Close` is idempotent.
3. The dedup window: default `30m`, per-driver configurable, clamp `1m..24h`, and what happens on expiry.
4. Rate-limit + backoff policy per driver: exponential with ±25 % jitter, `max_attempts=5`, a 45 s
   per-operation deadline, and one ledger record per attempt that mattered.
5. Spool-and-replay when a driver is down: the `SpoolEntry` shape, bounded spool (64 MB / 20 000 entries),
   drop-oldest with a 1 h floor, FIFO-within-sig replay order, an idempotency key that makes replay
   incapable of duplicating, and a `gap` record for everything dropped.
6. Healthcheck cadence and the ladder consequence of a failed healthcheck: driver failed → the issues rung
   degrades, **the incident continues**, `TROUBLE-ISSUES-009` + a gap record.
7. Issue caps at sig / project / global level — the anti-spray rule.
8. Quiet-close: a resolved incident closes its issue after a quiet period, with the reopen-on-recurrence path.
9. Sig linkage: issues link to board tasks and research briefs via sig automatically (AC-22).

The desk is not a shell and owns no mutating capability: it calls two HTTP APIs with a token that lives in a
0600 file, and it never reads a page, never pages a human, never touches git. It emits exactly two ledger
record kinds — `issue` and `gap` (SPEC-INDEX §3.4).

AC coverage: **AC-8** is exercised by the four driver methods, their ledger records and the healthcheck (§2, §3.5); **AC-22** is exercised by sig-keyed identity and the comment-not-duplicate rule (§3.2, §3.3). Sections no AC reaches are **design constraints**: the driver seam signatures (§2), the spool/replay contract (§3.6), the cap arithmetic (§3.4) and the quiet-close gate (§3.7) are obligations of this subsystem rather than user-visible acceptance criteria — the loop records them as constraints per SPEC-INDEX §7 step 2.

## 2. Interface

### 2.1 The driver contract (frozen; declared in SPEC-TYPES §3.11)

```go
// internal/types — the contract every issue driver implements. Nothing else is required of a driver.
type IssueDriver interface {
    // Name is the driver identifier: [a-z][a-z0-9_-]{0,31}. Pure, no I/O, no context. It MUST equal the
    // `driver` key in config and the `driver` field of every IssueRef this driver returns.
    Name() string

    // Healthcheck is a cheap read-only probe. It MUST NOT create, comment, close or mutate anything.
    // Bounded by cfg.timeout (15s github / 10s duckbrain). OK=false + nil error is legal and means
    // "reachable but degraded" (e.g. rate-limited); a non-nil error is classified by §5.
    Healthcheck(ctx context.Context) (DriverHealth, error)

    // EnsureBySig guarantees at most ONE issue per (driver, sig, project) for the lifetime of the state
    // root. Inside the dedup window a repeat call folds the recurrence into the existing issue and returns
    // Created=false, Commented=true. It never creates a second issue for a sig whose issue is open.
    EnsureBySig(ctx context.Context, req EnsureBySigRequest) (EnsureBySigResponse, error)

    // Comment appends exactly one comment per distinct trigger key. It is a no-op that returns the
    // unchanged ref when a comment with the same IdemKey was already appended.
    Comment(ctx context.Context, ref IssueRef, body string) (IssueRef, error)

    // Close is idempotent: closing an already-closed issue is success, not an error.
    Close(ctx context.Context, ref IssueRef, reason string) (IssueRef, error)
}
```

Input/output field semantics:

| Type. field | Semantics | Contract |
|---|---|---|
| `EnsureBySigRequest.Sig` | canonical sig string `<source>:<algo>v<n>:<hex16>` (SPEC-TYPES §6.3) | non-empty; a malformed sig is rejected before any call |
| `EnsureBySigRequest.Title` | issue title, `<prefix> <severity>: <summary> (<sig>)`, ≤256 chars | truncated by the desk, never by the driver |
| `EnsureBySigRequest.Body` | already-scrubbed evidence bundle (§3.9.3); ≤`body_max_bytes` (60 000) | the driver MUST NOT add raw payload; the desk scrubs before the call (SPEC-02) |
| `EnsureBySigRequest.Labels` | resolved label set (§3.9.2) | advisory only, never a dedup key |
| `EnsureBySigRequest.DedupWindow` | Go duration, `30m` default | clamp `1m..24h` enforced at config load |
| `EnsureBySigRequest.Severity` | `critical\|high\|medium\|low\|info` | drives the severity label only |
| `EnsureBySigResponse.Ref` | the one issue for this sig, fully populated | `ExternalID` non-empty on success |
| `EnsureBySigResponse.Created` | `true` only when this call filed a new issue | `false` on fold, cap, or suppression |
| `EnsureBySigResponse.Commented` | `true` when this call appended or folded a recurrence | `Created=true` implies `Commented=false` |
| `IssueRef.State` | `open \| closed` | the driver reads it back from the API; never inferred |
| `IssueRef.Comments` | comment count observed at the driver | monotonic; a lower value means a human deleted comments (edge case §6.11) |
| `IssueRef.TaskID` / `ResearchID` | `tsk_` / `res_` ULIDs from the sig linkage (§3.11) | set by the desk, never by the driver |
| `DriverHealth.RateLimitRemaining` | remaining quota from response headers; `-1` when the driver has no quota | `github`: `X-RateLimit-Remaining` of the core bucket |
| `DriverHealth.RateLimitResetTS` | RFC3339 UTC from `X-RateLimit-Reset` (unix → UTC) | `""` when unknown |
| `DriverHealth.Detail` | one scrubbed human line, e.g. `core 4821/5000, search 27/30` | never contains a token, key, or URL with a query string |

### 2.2 The desk

```go
// internal/issues
type Desk struct{ /* unexported: cfg, ledger, scrub, drivers, anchors, caps, spool, clock */ }

func New(cfg types.IssueDeskConfig, w ledger.Writer, s scrub.Scrubber,
         clk lifecycle.Clock) (*Desk, error)                          // builds drivers, replays caps, rebuilds caps
func (d *Desk) EnsureBySig(ctx context.Context, inc types.Incident,
        bundle Evidence) (types.IssueRef, error)                      // ladder entry point (AC-8)
func (d *Desk) Comment(ctx context.Context, ref types.IssueRef, trigger string,
        body string) (types.IssueRef, error)                          // trigger = ledger rec_id
func (d *Desk) Close(ctx context.Context, ref types.IssueRef, reason string) (types.IssueRef, error)
func (d *Desk) Link(ctx context.Context, ref types.IssueRef, taskID, researchID string) (types.IssueRef, error)
func (d *Desk) Health(ctx context.Context) []types.DriverHealth       // SPEC-12 §3 health aggregator
func (d *Desk) CapState() types.IssueCapState                         // `trouble issues health --json`
func (d *Desk) Replay(ctx context.Context, budget int) (int, error)   // spool drain (called by the desk loop)
func (d *Desk) SweepQuietClose(ctx context.Context, now time.Time) (int, error)
func (d *Desk) Ack(ctx context.Context, ref types.IssueRef, until time.Duration) (types.IssueRef, error)
func (d *Desk) Anchor(sig string) (types.IssueRef, bool)              // CLI lookup: `issues list --sig`

// Registration seam: the two shipped drivers register here; a driver name that is not registered is a
// boot-time config rejection inside internal/lifecycle (SPEC-12 §5) — internal/issues never sees it.
type DriverFactory func(cfg types.IssueDriverConfig, d *Desk, hc *http.Client) (types.IssueDriver, error)
func Register(name string, f DriverFactory)
```

Ports: the three constructor arguments are declared by the packages that implement them and are **not**
re-declared here (no duplicate definitions in the suite): `ledger.Writer` (SPEC-01 §2, the single-writer
append port), `scrub.Scrubber` (SPEC-02 §2), `lifecycle.Clock` (SPEC-12 §2 — the dual clock: monotonic for
every window in this spec, wall for persisted timestamps, per SPEC-INDEX §6.5). The spool is internal to this
package (`internal/issues/spool.go` writing under the state root's `spool/issues/` subtree); no external spool
port exists, and the stdlib `*http.Client` is the only transport type in the factory signature.

The desk runs one goroutine per enabled driver (≤2 in v0.1): healthcheck timer, replay timer, quiet-close
sweep. Total goroutines for the package ≤ 8; no goroutine takes the ladder's path — every call from
SPEC-05 is bounded by `op_deadline` (45 s) and returns either a ref or a classified error.

### 2.3 Surfaces

| Surface | Owner | Notes |
|---|---|---|
| `EnsureBySig` / `Comment` / `Close` from the ladder | SPEC-05 §3.6 | the only automatic path; runs under the incident's autonomy gate |
| `flow.file_issue` registry module | module set owned by SPEC-06 §6 (registered at boot there) | the agent's only way to cause a filing; args `{sig,title,body,labels,severity}`, `check_mode` returns the rendered body without calling the driver, `scopes=["flow:write"]`, `idempotency=convergent` |
| CLI `trouble issues list\|ack\|close\|link\|health\|replay` | this spec | `--json` on every verb; `ack <iss_id> [--until 24h]` suppresses fold comments for the sig, never the incident |
| Dashboard rendering of issue state | SPEC-10 §2 | the `IssueRef` rows render inside the incident story (`GET /incidents/{id}`, route 3) from the ledger's `issue` records; v0.1 has no standalone `/issues` index, and the dashboard makes no driver call |
| `GET /health.json` | SPEC-12 §3, SPEC-10 §2 route 8 | the desk's `Health()` feeds the per-driver health lines of the aggregator |

Outbound calls (this spec owns the call shapes; no inbound route is added by this spec):

| Driver | Calls |
|---|---|
| github | `POST /repos/{owner}/{repo}/issues`, `POST /repos/{owner}/{repo}/issues/{n}/comments`, `PATCH /repos/{owner}/{repo}/issues/{n}`, `GET /repos/{owner}/{repo}/issues/{n}`, `GET /search/issues`, `GET /rate_limit`, `GET /repos/{owner}/{repo}` |
| duckbrain | `GET /health`, `GET /v1/kv/{key}`, `PUT /v1/kv/{key}`, `GET /v1/kv?prefix={ns_prefix}.` |

Shared request envelope for both drivers: `User-Agent: trouble/<version>` (version from ldflags, SPEC-12
§3.6), `Accept: application/json`, connect timeout 5 s, whole-call timeout `timeout` (15 s github), no
retry inside the transport — retries live in the desk so every attempt is a ledger record (§3.5).

### 2.4 Driver seam (v1.0 hand-off)

`jira`, `linear` and `gitlab` implement the same `IssueDriver` interface and add their own
`[issues.drivers.<name>]` block `(v1.0 hand-off)`: no adapter code ships in v0.1 and no section of this spec
describes one as in scope. The seam is the four methods above plus `Register(name, factory)`; an additional
driver needs no change to the desk, the ledger schema, the caps, the spool, or the quiet-close path.

## 3. Data model

### 3.1 Idempotency per method

| Method | Key | Repeat behaviour | On uncertain outcome (timeout / 5xx after send) |
|---|---|---|---|
| `EnsureBySig` | `issue_ensure\|<driver>\|<sig>\|<inc>\|<rec_id>` | same key → `TROUBLE-ISSUES-010`, `Created=false`, `Commented=false`, **no driver call** | read-back first: `GET /search/issues` for the sig marker (≤5 queries); created-found → adopt; zero-found → create; search unavailable → spool as `ensure_unverified` and never create inline |
| `Comment` | `issue_comment\|<iss_id>\|<rec_id>` | same key → returns the unchanged ref, no call | read-back the issue's comment list; a comment whose first line carries the idem marker is adopted |
| `Close` | `issue_close\|<iss_id>\|<reason>` | already closed → success (`State=closed`), no error | read-back `state`; `closed` → success; else `TROUBLE-ISSUES-008` |
| `Healthcheck` | n/a (read-only) | idempotent by construction | counts as one failed probe; two consecutive failures mark the driver failed (§3.6) |

Every comment written by the desk ends with a machine-readable marker line so an uncertain-outcome read-back
is exact: `<!-- trouble:idem=issue_comment|iss_01J9Z6Q0M2X4T8V1K7B3N5R8WK|ev_01J9Z6Q0M2X4T8V1K7B3N5R8WM -->`.
The marker is 1 of the 2 marker lines a driver must never strip (the other is the sig marker, §3.9.3).

### 3.2 The anchor rule and the dedup window

* **Anchor** = the single issue per `(driver, sig, project)`. It is held in the in-memory `anchorIndex`
  (sig → `anchorEntry{ref, idem, created_ts, updated_ts, manual}`) and is rebuilt at boot from the `issue`
  ledger records inside the retention window, then reconciled by search once per driver at boot.
* **Issue identity is permanent.** A second issue for a sig is created only when the anchor is gone at the
  driver (`TROUBLE-ISSUES-007`) or the previous issue was closed by a human and the driver refuses reopen
  (`TROUBLE-ISSUES-008`, §3.10). Reopening the incident reopens the same issue — never a new one (AC-22).
* **`dedup_window` (default `30m`, per driver, clamp `1m..24h`)** governs the *fold shape*, not identity:
  * inside the window → a **one-line fold comment** (`recurrence 2026-09-16T09:41:02.114Z · inc_… · occurrences 43`)
    and `Commented=true`;
  * outside the window, anchor still open → a **full recurrence block** (counters and release range since
    the previous comment) and `Commented=true`;
  * either way `Created=false`, at most 1 comment per sig per `comment_min_interval` (10 m), and any surplus
    is recorded as `TROUBLE-ISSUES-010` with `payload.result="suppressed"`.
* `dedup_window` is deliberately ≥ 3× the ladder's default verify window (10 m, amendment N) so a
  recurrence *inside* verification folds instead of filing.
* Expiry of the window never permits a duplicate: only the two anchor-loss paths above do.
* The anchor rule is written against `(driver, sig, project)`, never against a driver's own identifier space
  — which is what makes the driver seam of §2.4 interface-only: an added driver inherits the whole anchor,
  window, cap, spool and quiet-close behaviour `(v1.0 hand-off)`.

### 3.3 Ledger payload schemas (`kind=issue`, `kind=gap`)

`internal/issues` is the only emitter of `issue` and one of the five emitters of `gap` (SPEC-INDEX §3.4).
One record per attempt that mattered (attempt > 1) and one per operation that changed state.

`kind=issue`, `payload.op` ∈ `ensure | comment | close | reopen | link | ack | healthcheck | replay | drop | cap`:

```json
{"op":"ensure","driver":"github","sig":"sentinel:sha256v1:9f2c1d3e4b5a6c7d","inc":"inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE","idem_key":"issue_ensure|github|sentinel:sha256v1:9f2c1d3e4b5a6c7d|inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE|ev_01J9Z6Q0M2X4T8V1K7B3N5R8WM","result":"created","created":true,"commented":false,"external_id":"42","url":"https://github.com/acme/payment-api/issues/42","attempt":1,"max_attempts":5,"http_status":201,"error_code":"","retryable":false,"dedup_window":"30m","anchor_hit":false,"body_bytes":4821,"truncated":false}
```

```json
{"op":"comment","driver":"github","sig":"sentinel:sha256v1:9f2c1d3e4b5a6c7d","inc":"inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE","idem_key":"issue_comment|iss_01J9Z6Q0M2X4T8V1K7B3N5R8WK|ev_01J9Z6Q0M2X4T8V1K7B3N5R8WM","result":"folded","created":false,"commented":true,"external_id":"42","attempt":1,"http_status":201,"error_code":"","comment_kind":"fold","window_expired":false}
```

```json
{"op":"close","driver":"github","sig":"sentinel:sha256v1:9f2c1d3e4b5a6c7d","inc":"inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE","idem_key":"issue_close|iss_01J9Z6Q0M2X4T8V1K7B3N5R8WK|resolved","result":"closed","state":"closed","reason":"resolved","reason_comment_id":"198","attempt":1,"http_status":200,"error_code":""}
```

```json
{"op":"healthcheck","driver":"duckbrain","ok":false,"detail":"GET /health: connection refused","rate_limit_remaining":-1,"rate_limit_reset_ts":"","consecutive_failures":2,"error_code":"TROUBLE-ISSUES-009"}
```

```json
{"op":"drop","driver":"github","result":"dropped","count":37,"reason":"budget","oldest_ts":"2026-09-16T04:10:00.000Z","error_code":"TROUBLE-ISSUES-005"}
```

```json
{"op":"cap","driver":"github","sig":"sentinel:sha256v1:9f2c1d3e4b5a6c7d","result":"capped","cap":"per_sig_creates","window":"24h","count_in_window":1,"error_code":"TROUBLE-ISSUES-004"}
```

`kind=gap` for a driver outage or a spool drop:

```json
{"id":"ev_01J9Z6Q0M2X4T8V1K7B3N5R8WN","sensor":"issues","scope":"github","from_ts":"2026-09-16T09:16:00.000Z","to_ts":"2026-09-16T09:17:00.000Z","est_lost":3,"cause":"driver_down"}
```

`cause` uses the token `driver_down` (an addition to the `GapRecord.Cause` set in SPEC-TYPES §3.7, reported
as TYPES-GAP at the end of this spec) for healthcheck-driven outages, and `queue_overflow` for spool drops.
`est_lost` is `-1` when the desk cannot count the folded recurrences.

### 3.4 Config schema

```toml
[issues]
enabled              = true
primary_driver       = "github"      # must name an enabled driver
dedup_window         = "30m"         # clamp 1m..24h
quiet_close          = "24h"
quiet_close_sweep    = "5m"
op_deadline          = "45s"         # whole-operation budget incl. backoff sleeps
body_max_bytes       = 60000         # < github's 65536 body limit
title_max_chars      = 256
replay_interval      = "5s"
replay_batch         = 100
healthcheck_interval = "60s"         # driver has open issues or a non-empty spool
healthcheck_idle     = "5m"          # no open issues and an empty spool
fail_after_probes    = 2             # consecutive failed probes → driver failed
spool_budget_bytes   = 67108864      # 64 MiB share of the 256 MiB state-root spool
spool_max_entries    = 20000
spool_ttl            = "72h"
spool_min_retention  = "1h"          # entries younger than this are never evicted
max_attempts_per_op  = 20            # lifetime attempts for a spooled op
ack_default          = "24h"

[issues.caps]
per_sig_creates        = 1
per_sig_window         = "24h"
per_sig_comments       = 6
comment_min_interval   = "10m"
per_project_creates_h  = 20
per_project_creates_d  = 200
per_project_comments_h = 200
global_creates_h       = 100
global_creates_d       = 1000
global_comments_h      = 600

[issues.drivers.github]
enabled          = true
owner            = "acme"                       # required
repo             = "payment-api"                # required
api_base         = "https://api.github.com"     # GitHub Enterprise: the instance's /api/v3 base
token_file       = "~/.config/trouble/github.token"   # 0600, mode checked at boot
token_env        = "TROUBLE_GITHUB_TOKEN"       # env beats file (flag > env > file > default)
labels           = ["trouble", "auto-filed"]
labels_extra     = []
severity_labels  = { critical = "sev:critical", high = "sev:high", medium = "sev:medium", low = "sev:low", info = "sev:info" }
source_labels    = true                         # adds src:sentinel | src:sensor | src:collector | src:generic
max_attempts     = 5
base_backoff     = "1s"
max_backoff      = "60s"
jitter           = 0.25
min_remaining    = 100                          # pause the driver below this core-quota headroom
search_min_interval = "6s"                      # ≤10 search calls/min (api limit 30/min)
timeout          = "15s"

[issues.drivers.duckbrain]
enabled          = false                        # local-first backend; enable when one is reachable
base_url         = "http://127.0.0.1:7645"
api_key_file     = "~/.config/trouble/duckbrain.key"  # 0600
api_key_env      = "TROUBLE_DUCKBRAIN_KEY"
api_key_header   = "Authorization"              # the header NAME is configuration; never hardcoded
api_key_scheme   = "Bearer"
ns_prefix        = "trouble.issues"
max_attempts     = 5
base_backoff     = "500ms"
max_backoff      = "5s"
jitter           = 0.25
timeout          = "10s"
```

Config rules: `owner`/`repo` empty → the driver is refused at boot (config validation, SPEC-12 §5).
`primary_driver` must be enabled and healthy-capable at boot; if it is not, the desk starts degraded, records
`healthcheck ok=false`, and the ladder's issues rung degrades instead of failing the boot. `trouble config
explain` prints `token_file`/`api_key_file` with `Redacted=true` and prints `api_key_header` as the header
**name** only — no key value is ever printed, logged, or placed in the ledger (SPEC-02 mandatory rules).

### 3.5 Rate limits, backoff, and the per-attempt ledger record

| Driver | max_attempts | base | growth | max_backoff | jitter | per-attempt timeout | pause trigger |
|---|---|---|---|---|---|---|---|
| github | 5 | 1s | ×2 | 60s | ±25 %, multiplicative | 15s | `remaining ≤ min_remaining` (100) or 429/403 with rate-limit headers |
| duckbrain | 5 | 500ms | ×2 | 5s | ±25 % | 10s | 429 with `Retry-After` |

Delay for attempt *n* (1-based) is `min(max_backoff, base × 2^(n-1)) × (1 + U(-jitter, +jitter))`; the sum of
in-operation sleeps is capped by `op_deadline` (45 s). GitHub worst case: 1+2+4+8+16 = 31 s of sleep, under
the cap. When the cap or `max_attempts` is reached the operation is spooled, never retried inline.

Rate-limit handling (github): every response's `X-RateLimit-Remaining` / `X-RateLimit-Reset` is read; when
`remaining ≤ min_remaining` the driver pauses until `reset` (bounded at 15 m, then a fresh healthcheck) and the
operation is spooled; a `403` carrying `Retry-After < 60s` backs off in-call, `≥ 60s` spools; `401`, or a `403`
with no rate-limit headers, is `TROUBLE-ISSUES-003` (permanent, no retry, driver marked failed). The search
budget is separate: ≤1 search call per `search_min_interval` (6 s) → ≤10/min against GitHub's 30/min.

The per-attempt ledger record (`types.IssueAttempt`) is written when `attempt > 1` or the operation failed —
never for a clean first attempt (that writes the op record of §3.3 only), so a healthy desk adds exactly one
record per state change:

```json
{"op":"ensure","driver":"github","idem_key":"issue_ensure|github|sentinel:sha256v1:9f2c1d3e4b5a6c7d|inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE|ev_01J9Z6Q0M2X4T8V1K7B3N5R8WM","attempt":3,"max_attempts":5,"outcome":"retry","http_status":502,"error_code":"TROUBLE-ISSUES-001","retryable":true,"backoff_ms":4200,"next_try_ts":"2026-09-16T09:17:31.004Z","remaining":4821,"reset_ts":"2026-09-16T09:36:00.000Z"}
```

### 3.6 Healthcheck cadence and the ladder consequence

Cadence: every `healthcheck_interval` (60 s) while the driver has an open issue or a non-empty spool;
`healthcheck_idle` (5 m) otherwise. `fail_after_probes` (2) consecutive failures → the driver state becomes
`failed`. The health transition is recorded once (ok→fail, fail→ok) plus once per 15 m while continuously
failing, so an outage cannot fill the ledger.

A failed healthcheck does exactly this, in order:

1. `kind=issue` record `{op:"healthcheck", ok:false, …, error_code:"TROUBLE-ISSUES-009"}`.
2. `kind=gap` record `{sensor:"issues", scope:"<driver>", cause:"driver_down", est_lost:-1}`, closed when the
   driver recovers and the spool drains (the closing record carries `est_lost = <replayed + dropped>`).
3. **The issues rung degrades and the incident continues**: the ladder (SPEC-05 §3.6) treats a transient
   outlet failure as non-fatal — the incident stays at rung `outlets` with its state (`recorded` or
   `verifying`) unchanged, the verify window is unaffected, the quiet-close clock is unaffected, no escalation
   is triggered (a driver outage is not an incident failure), no agent run is started, and the pending
   operation is spooled. The rung is marked `issues:degraded` in the incident payload.
4. The local anchor stays authoritative: `Incident.IssueID` is `""` until a driver confirms a ref, and the
   dashboard renders the sig as `issue: pending (driver down)` from the ledger's `result:"spooled"` record.
5. On recovery (`fail→ok`) the desk drains the spool (§3.7) before accepting new work, and the ladder is not
   re-entered — the drained operations themselves set `IssueID` via a follow-up `kind=issue` record with
   `result:"replayed"`.

### 3.7 Spool and replay

Storage: `~/.local/state/trouble/spool/issues/<driver>/<ev_ULID>.json`, one JSON object per file, mode 0600,
written atomically (temp file → `fsync` → `rename` → `fsync` dir), so a torn entry can never be replayed.
`SpoolEntry` (SPEC-TYPES §3.14) is the on-disk and in-memory shape, with `Kind="issue"`:

```json
{"id":"ev_01J9Z6Q0M2X4T8V1K7B3N5R8WP","ts":"2026-09-16T09:16:04.221Z","kind":"issue","payload":"eyJvcCI6ImVuc3VyZSIsImRyaXZlciI6ImdpdGh1YiIsInNpZyI6InNlbnRpbmVsOnNoYTI1NnYxOjlmMmMxZDNlNGI1YTZjN2QifQ==","attempts":2,"idem_key":"issue_ensure|github|sentinel:sha256v1:9f2c1d3e4b5a6c7d|inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE|ev_01J9Z6Q0M2X4T8V1K7B3N5R8WM","next_try_ts":"2026-09-16T09:16:34.221Z"}
```

`payload` is the base64 of the marshalled `EnsureBySigRequest` (or the comment/close argument struct). It is
already scrubbed (SPEC-02) before marshalling; the spool is not a leak surface, and no token, key or header
value is ever inside it.

Bounds and drop policy:

| Bound | Default | Behaviour at the bound |
|---|---|---|
| `spool_budget_bytes` (per driver tree) | 64 MiB | evict oldest entries while `age > spool_min_retention` (1 h) and re-check the budget |
| `spool_max_entries` | 20 000 | same eviction; ~20 000 × 3 KB ≈ 60 MiB, consistent with the byte budget |
| all entries younger than `spool_min_retention` | — | refuse the new entry with `TROUBLE-ISSUES-005` (spool full); the operations stay counted in the group's `suppressed` counter so nothing is lost silently |
| `spool_ttl` | 72 h | drop the entry, `kind=issue {op:"drop", reason:"ttl"}` + gap record |
| `max_attempts_per_op` | 20 | drop the entry, `kind=issue {op:"drop", reason:"attempts"}` + gap record |

Every drop writes a `gap` record (`cause:"queue_overflow"`, `est_lost` = number of recurrences folded into the
dropped entry) **and** an `issue` record `{op:"drop", count, reason, oldest_ts}`, so the loss is visible in the
ledger and on the dashboard rather than inferred.

Replay order and safety:

1. Ordering key `(next_try_ts asc, ts asc, id asc)` within a driver; one in-flight operation per sig (per-sig
   lock) so comments for a signal can never reorder.
2. `replay_batch` (100) entries per `replay_interval` (5 s) → 1 200 operations/min, and ≤ `op_deadline` wall
   time per entry; a rate-limit signal stops the batch immediately and shifts the rest to the next tick.
3. Every replayed operation carries its original `IdemKey`; because `EnsureBySig`/`Comment`/`Close` are keyed
   on it (§3.1), replay cannot create a duplicate issue or comment.
4. An `ensure_unverified` entry re-runs the read-back search first: found → adopt the existing issue; not
   found → create. Adoption and creation both write `result:"replayed"` records.
5. Replay failure → `TROUBLE-ISSUES-006`, `attempts++`, `next_try_ts = now + backoff(attempts)`; the entry is
   dropped only by TTL or `max_attempts_per_op`.

### 3.8 Issue caps (the anti-spray rule)

Counters live in the in-memory `IssueCapState` and are **rebuilt from the ledger at boot** inside the window,
so a restart never lifts a cap. Windows are fixed; the desk uses the monotonic clock for the window and the
persisted RFC3339 timestamps for the rebuild (SPEC-INDEX §6.5). Counters are kept per driver and globally.

| Level | Counter | Default | Window | On exceed |
|---|---|---|---|---|
| sig | issue creates | 1 | 24 h | `TROUBLE-ISSUES-004` (`cap:"per_sig_creates"`), fold comment only if `comment_min_interval` allows |
| sig | comments | 6 | 1 h | `TROUBLE-ISSUES-010` (`result:"suppressed"`), group `suppressed` counter++ |
| sig | minimum comment spacing | 10 m | — | fold deferred; the next allowed fold comment carries the accumulated counters |
| project (sentinel `project_id`, or `host:<host_id>` for sensor findings) | issue creates | 20 | 1 h | `TROUBLE-ISSUES-004` (`cap:"per_project_creates_h"`) |
| project | issue creates | 200 | 24 h | `TROUBLE-ISSUES-004` (`cap:"per_project_creates_d"`) |
| project | comments | 200 | 1 h | `TROUBLE-ISSUES-010`, folded |
| global | issue creates | 100 | 1 h | `TROUBLE-ISSUES-004` (`cap:"global_creates_h"`) |
| global | issue creates | 1 000 | 24 h | `TROUBLE-ISSUES-004` (`cap:"global_creates_d"`) |
| global | comments | 600 | 1 h | `TROUBLE-ISSUES-010`, folded |

Capped operations are **never spooled** (a cap is a policy decision; a retry cannot change it) — they are
recorded as `kind=issue {op:"cap", …}` with the `error_code`, and they increment the group's `suppressed`
counter (SPEC-TYPES `GroupCounters`). The ladder is unaffected: the incident continues, the escalation outlet
is the path for a capped critical incident, and the dashboard shows suppressed counts per sig.

### 3.9 GitHub driver

#### 3.9.1 Auth and configuration

Token resolution order: `--issues-github-token` flag (rejected with `TROUBLE-ISSUES-003` if a token is passed
this way — **never argv**), `token_env` (`TROUBLE_GITHUB_TOKEN`), `token_file` (`~/.config/trouble/github.token`),
then nothing → the driver is refused at boot with `TROUBLE-ISSUES-003`. The token file must be a regular file,
mode 0600, owned by the daemon uid; any other mode → `TROUBLE-ISSUES-003` and the driver is marked failed with
no outbound call attempted. The token value is never logged, never stored in the ledger, never present in
`DriverHealth.Detail`, and never rendered by `trouble config explain` (`Redacted=true`).

Requests: `Authorization: Bearer <token from file>`, `Accept: application/vnd.github+json`,
`X-GitHub-Api-Version: 2022-11-28`, `User-Agent: trouble/<version>`. `owner`, `repo` and `api_base` come from
config with the defaults of §3.4; the repo is probed once at boot (`GET /repos/{owner}/{repo}`) to confirm
reachability and `issues:write` scope.

#### 3.9.2 Labels (default set + severity mapping)

Always: `trouble`, `auto-filed`. Plus one severity label from `severity_labels`, plus one source label when
`source_labels=true` (`src:sentinel|src:sensor|src:collector|src:generic`), plus `labels_extra`. Labels are
advisory and are never used for dedup, lookup or closure; a human may rename or delete them without affecting
the desk. Severity → priority alignment with SPEC-08's `severity→priority` mapping is the same table, exposed
here only as the label tier: `critical→sev:critical`, `high→sev:high`, `medium→sev:medium`, `low→sev:low`,
`info→sev:info`. An issue filed by the desk never auto-closes because of a label change.

#### 3.9.3 Title, body and the sig marker

Title: `[trouble] <severity>: <summary> (<sig>)`, truncated to `title_max_chars` (256) on a rune boundary,
e.g. `[trouble] high: queue wedge in payment-worker (sentinel:sha256v1:9f2c1d3e4b5a6c7d)`.

Body — the exact template, byte-rendered in this order; the two HTML-comment markers are mandatory and are
the mechanism that makes a sig findable in issue bodies by search:

````markdown
<!-- trouble:sig=sentinel:sha256v1:9f2c1d3e4b5a6c7d -->
<!-- trouble:inc=inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE driver=github ver=1 -->

**Summary** — queue wedge in payment-worker — high, from `sentinel:payment-worker`

| field | value |
|---|---|
| sig | `sentinel:sha256v1:9f2c1d3e4b5a6c7d` |
| incident | `inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE` (group `grp_01J9Z6Q0M2X4T8V1K7B3N5R8WH`) |
| first seen | 2026-09-16T09:14:03.221Z |
| last seen | 2026-09-16T09:15:41.009Z |
| occurrences | 42 |
| release range | `payment-api@2.4.1` → `payment-api@2.4.3` (12 events) |
| origin | host `7f3a91c2d4e5b607`, source `sentinel:payment-worker` |
| ladder | detected → recorded → play:applied → verifying |
| redactions applied | 3 |

**Evidence bundle (scrubbed)**

```
worker.py:118 claim
  queue.py:44 get
journal: payment-worker[8841]: pool exhausted (retry 3/5)
config: pool.max = 200 (configured) → 250 (proposed, check_mode)
```

**Recurrences**

- 2026-09-16T09:15:41.009Z — inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE — fold (verify window)

<sub>filed by trouble 0.1.0 (9c1f0ab) at 2026-09-16T09:15:41.009Z · driver github · idem 4821b90f</sub>
````

Field sources: `summary` = `Group.Title`; `occurrences` = `Group.Count`; `release range` = `Group.ReleaseRange`
(`[first_seen_release, last_seen_release]`, count from the sentinel group counters inside that range); `ladder`
= the incident's recorded state path; `redactions applied` = the sum of `Record.Redactions` for the bundle
(never the redacted values — SPEC-02). The body is assembled from the already-scrubbed bundle and is
re-scrubbed as one blob before the call (defence in depth for a user-supplied project name). A body over
`body_max_bytes` (60 000) is truncated on a rune boundary with `\n[truncated by trouble: <n> bytes elided]`,
`payload.truncated=true`; truncation is applied to the evidence section first, never to the sig marker.

#### 3.9.4 Search-by-sig dedup

Search is the **fallback**, never the hot path: the in-memory anchor index (single writer, hub only) answers
every dedup question; search is used at boot/rebuild, for `ensure_unverified` resolution, and when the local
anchor is missing. Exact queries (URL-encoded; `{sig}` and `{owner}/{repo}` substituted literally):

| Purpose | Query |
|---|---|
| find the anchor, any state | `repo:{owner}/{repo} is:issue in:body "trouble:sig={sig}"` + `&sort=created&order=asc&per_page=1` |
| find an open anchor | `repo:{owner}/{repo} is:issue is:open in:body "trouble:sig={sig}"` + `&per_page=1` |
| find the anchor by incident (diagnostic) | `repo:{owner}/{repo} is:issue in:body "trouble:inc={inc}"` + `&per_page=1` |

GitHub search is eventually consistent (index lag is minutes) and costs a separate, smaller budget
(30/min authenticated), so it is never the correctness mechanism for at-most-once creation inside a run: that
is the per-sig mutex plus the anchor index. Search results are cached for `dedup_window`; the search cache is
invalidated by any create/close the desk performs.

#### 3.9.5 Close, and failure classes → error codes

Close = comment first (`POST /repos/{owner}/{repo}/issues/{n}/comments` with the reason text of §3.10's
template) then `PATCH /repos/{owner}/{repo}/issues/{n}` with `{"state":"closed","state_reason":"completed"}`,
then a read-back `GET`. Reopen = the same `PATCH` with `{"state":"open","state_reason":"reopened"}`.

| HTTP / condition | Code | Class | Retry | Desk effect |
|---|---|---|---|---|
| 2xx, read-back agrees | — | — | — | op record, `IssueRef` updated |
| 5xx, connection reset, TLS error, timeout | `TROUBLE-ISSUES-001` | transient | ≤5 attempts, then spool | driver stays healthy |
| 429, or 403 with rate-limit headers, or `remaining ≤ min_remaining` | `TROUBLE-ISSUES-002` | transient | sleep to reset (≤15 m), then spool | driver `degraded` |
| 401, 403 without rate-limit headers, bad token-file mode | `TROUBLE-ISSUES-003` | permanent | none | driver `failed` → `TROUBLE-ISSUES-009` + gap |
| any cap in §3.8 | `TROUBLE-ISSUES-004` | permanent | none | op recorded `capped`, never spooled |
| spool write/budget refusal | `TROUBLE-ISSUES-005` | transient | next tick | rung degrades; group `suppressed`++ |
| replay attempt failed | `TROUBLE-ISSUES-006` | transient | `attempts++`, backoff | entry kept until TTL |
| 404/410 on comment/patch, or read-back 404 | `TROUBLE-ISSUES-007` | permanent | none | anchor invalidated; next trigger re-creates (new external id, same sig) |
| 2xx but read-back shows `state != closed`, or 423/422 "locked"/"already closed by another actor" | `TROUBLE-ISSUES-008` | permanent | 3 attempts, then leave open + ledger note | issue stays open, dashboard marks `close pending` |
| 422 validation on create (bad label, forbidden body shape) | `TROUBLE-ISSUES-001` with `retryable:false` | transient code, non-retryable instance | none | driver `degraded`; gap record; rung degrades |
| repeat of an operation whose `IdemKey` was already handled | `TROUBLE-ISSUES-010` | permanent | none | no driver call |
| 2 consecutive failed healthchecks | `TROUBLE-ISSUES-009` | transient | next interval | rung degrades, incident continues (§3.6) |

The `retryable` field is written on every record that carries a code, and only `retryable=true` entries are
spooled — so a permanent instance can never enter the replay queue.

### 3.10 Duckbrain driver

Local-first backend: the driver speaks a key/value document API, so an incident's issue lives in the local
trouble-adjacent store even when no internet, no GitHub and no fleet is present.

* Auth: `api_key_file` (0600, mode checked at boot) or `api_key_env` (`TROUBLE_DUCKBRAIN_KEY`). The header
  **name** is configuration (`api_key_header`, default `Authorization`) with an optional scheme
  (`api_key_scheme`, default `Bearer`); the key value is never hardcoded, never logged, never written to the
  ledger, and never printed — `trouble config explain` shows `api_key_header = "Authorization"` and the file
  path with `Redacted=true`.
* Namespace/key layout (`ns_prefix` = `trouble.issues`, `sig16` = `hex(Digest)[:16]`):
  * anchor: `trouble.issues.<project>.<sig16>` — one document per sig (§3.2's anchor rule on a KV store)
  * comments: `trouble.issues.<project>.<sig16>.c.<ULID>` — one key per comment, so two writers never clobber
  * index: `trouble.issues.<project>.<sig16>.i` — `{iss, external_id, updated_ts}` pointer for a fast boot rebuild
  * project = sentinel `project_id`, or `host:<host_id>` for sensor findings.

```json
{"iss":"iss_01J9Z6Q0M2X4T8V1K7B3N5R8WK","sig":"sentinel:sha256v1:9f2c1d3e4b5a6c7d","project":"1","driver":"duckbrain","state":"open","title":"queue wedge in payment-worker","severity":"high","first_seen_ts":"2026-09-16T09:14:03.221Z","last_seen_ts":"2026-09-16T09:15:41.009Z","occurrences":42,"release_range":["payment-api@2.4.1","payment-api@2.4.3"],"inc":"inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE","task_id":"","research_id":"","comments":1,"manual":false,"idem":"issue_ensure|duckbrain|sentinel:sha256v1:9f2c1d3e4b5a6c7d|inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE|ev_01J9Z6Q0M2X4T8V1K7B3N5R8WM","updated_ts":"2026-09-16T09:15:41.009Z","redactions":3}
```

* Idempotent write semantics: `PUT` the comment key first (ULID-keyed, therefore inherently idempotent), then
  `PUT` the anchor with `IdemKey`, then **always `GET` the anchor back and compare `idem`, `comments` and
  `updated_ts`**. The write response is never trusted; a read-back mismatch is `TROUBLE-ISSUES-006` and the
  operation is retried with the same idempotency key. If the backend supports conditional writes
  (`If-None-Match: *` on create), the driver uses them; if not, `GET`-then-`PUT` with the `idem` compare is the
  contract (last-writer-wins on the anchor is safe because the anchor is keyed by sig and written by one
  process).
* `EnsureBySig` → `Created=true` only when the anchor key was absent (or `state=closed` and the reopen path
  was refused); otherwise it appends a comment key and returns `Created=false, Commented=true`. `Close` writes
  `state=closed` and returns `State=closed`; a second close is a no-op success. `Healthcheck` is
  `GET /health` + a header-only `GET /v1/kv?prefix=trouble.issues.&limit=1`.
* Degradation is identical to github's contract: healthcheck failure → `TROUBLE-ISSUES-009` + gap record
  (`cause:"driver_down"`) → issues rung degrades, incident continues, operations spool to
  `spool/issues/duckbrain/` and replay through the same keyed operations. Because the backend is local-first,
  the ledger remains the source of truth while it is down and the dashboard shows the local issue state.

### 3.11 Quiet-close and reopen

Quiet-close: a `resolved` incident closes its issue after `quiet_close` (24 h) measured from
`Incident.ResolvedTS`, swept every `quiet_close_sweep` (5 m). All of these must hold:

1. no event with the sig in the quiet period (the group's `LastSeenTS` is older than `ResolvedTS`);
2. `Incident.ReopenCount` unchanged since resolution and the newest **verification** evidence has
   `result=passed` (SPEC-05 §3.6) — a resolution without a passing evidence tuple is never closed;
3. no open board row with the same sig (SPEC-08) and no open research brief with the same sig (SPEC-07);
4. the incident is not `escalated`, and the anchor's `manual=true` (a human-filed issue adopted into the desk)
   is never auto-closed;
5. the driver is healthy; if not, the close is **deferred** (recorded, re-tried on the next sweep after
   recovery) — never dropped.

Close = one reason comment + `Close()` + read-back; the reason comment is:

```
Closed by trouble: sig quiet for 24h after resolution.
- incident: inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE
- evidence: events_observed=0 canary_seen=true window=10m sources_alive=2/2 result=passed
- resolved: 2026-09-16T09:25:41.009Z   quiet period: 24h
- ledger: last seq 41207   <!-- trouble:idem=issue_close|iss_01J9Z6Q0M2X4T8V1K7B3N5R8WK|resolved -->
```

Reopen-on-recurrence: a recurrence after close reopens the **same** incident (sig-keyed, SPEC-05 amendment N)
and the desk reopens the **same** issue — `Comment` (fold, with the new first/last-seen and the reopen reason)
then `PATCH state=open` within `dedup_window` of the recurrence. If the driver refuses the reopen
(`TROUBLE-ISSUES-008`, locked or already closed by another actor) the desk retries 3 times with backoff and
then files a **new** issue whose body carries `supersedes: <old external id>` plus the same sig marker, so
exactly one issue is open per sig in every branch. Both branches write a `kind=issue {op:"reopen"}` record with
`error_code` when applicable.

### 3.12 Sig linkage (AC-22)

Issues link to board tasks and research briefs through the sig, automatically, with no manual step:

| Link | Trigger | Mechanism |
|---|---|---|
| issue ↔ board row | SPEC-08 files a row carrying the same sig (`BoardRow.Sig`) | desk appends a cross-ref comment `linked: tsk_01J9Z6… (board-jsonl)` and sets `IssueRef.TaskID`; record `{op:"link", task_id}` |
| issue ↔ research brief | SPEC-07 returns `ResearchOutcome` for the same sig | desk appends `research: res_01J9Z6… (<slug>)` and sets `IssueRef.ResearchID`; record `{op:"link", research_id}` |
| board row field ← issue refs | SPEC-08 builds a row for the sig | SPEC-08 fills `BoardRow.IssueRefs[]` with the `iss_` ids the desk returned for that sig (SPEC-08 §3.1, `issue_refs`); `Desk.Anchor(sig)` is the lookup used when the row is written before the issue (ordering race), and it never invents an id |

One issue per `(driver, sig, project)` is the invariant. With `mirror=true` on a secondary driver the desk
files the same sig in both stores — each with the sig marker — and the dashboard joins them into one story on
the sig. With the default config (one enabled driver) "one issue" is literal. `Incident.IssueID`,
`BoardRow.IssueRefs[]` and `ResearchOutcome` never invent ids: a value exists only after a driver returned a
ref.

### 3.13 Types added to SPEC-TYPES by this spec

```go
type IssueDeskConfig struct {
    Enabled           bool               `json:"enabled"`
    PrimaryDriver     string             `json:"primary_driver"`
    DedupWindow       Duration           `json:"dedup_window"`
    QuietClose        Duration           `json:"quiet_close"`
    QuietCloseSweep   Duration           `json:"quiet_close_sweep"`
    OpDeadline        Duration           `json:"op_deadline"`
    BodyMaxBytes      int                `json:"body_max_bytes"`
    TitleMaxChars     int                `json:"title_max_chars"`
    ReplayInterval    Duration           `json:"replay_interval"`
    ReplayBatch       int                `json:"replay_batch"`
    HealthcheckEvery  Duration           `json:"healthcheck_interval"`
    HealthcheckIdle   Duration           `json:"healthcheck_idle"`
    FailAfterProbes   int                `json:"fail_after_probes"`
    SpoolBudgetBytes  int64              `json:"spool_budget_bytes"`
    SpoolMaxEntries   int                `json:"spool_max_entries"`
    SpoolTTL          Duration           `json:"spool_ttl"`
    SpoolMinRetention Duration           `json:"spool_min_retention"`
    MaxAttemptsPerOp  int                `json:"max_attempts_per_op"`
    AckDefault        Duration           `json:"ack_default"`
    Caps              IssueCaps          `json:"caps"`
    Drivers           []IssueDriverConfig `json:"drivers"`
}

type IssueCaps struct {
    PerSigCreates      int      `json:"per_sig_creates"`
    PerSigWindow       Duration `json:"per_sig_window"`
    PerSigComments     int      `json:"per_sig_comments"`
    CommentMinInterval Duration `json:"comment_min_interval"`
    PerProjectCreatesH int      `json:"per_project_creates_h"`
    PerProjectCreatesD int      `json:"per_project_creates_d"`
    PerProjectCommentsH int     `json:"per_project_comments_h"`
    GlobalCreatesH     int      `json:"global_creates_h"`
    GlobalCreatesD     int      `json:"global_creates_d"`
    GlobalCommentsH    int      `json:"global_comments_h"`
}

type IssueDriverConfig struct {
    Name            string            `json:"name"`              // github | duckbrain
    Enabled         bool              `json:"enabled"`
    Primary         bool              `json:"primary"`
    Mirror          bool              `json:"mirror"`            // secondary driver for the same sig
    DedupWindow     Duration          `json:"dedup_window"`      // "" → IssueDeskConfig.DedupWindow
    MaxAttempts     int               `json:"max_attempts"`
    BaseBackoff     Duration          `json:"base_backoff"`
    MaxBackoff      Duration          `json:"max_backoff"`
    Jitter          float64           `json:"jitter"`
    Timeout         Duration          `json:"timeout"`
    MinRemaining    int               `json:"min_remaining"`     // 0 → no quota pause (duckbrain)
    SearchMinInterval Duration        `json:"search_min_interval"`
    Owner           string            `json:"owner,omitempty"`
    Repo            string            `json:"repo,omitempty"`
    APIBase         string            `json:"api_base,omitempty"`
    TokenFile       string            `json:"token_file,omitempty"`
    TokenEnv        string            `json:"token_env,omitempty"`
    Labels          []string          `json:"labels,omitempty"`
    LabelsExtra     []string          `json:"labels_extra,omitempty"`
    SeverityLabels  map[string]string `json:"severity_labels,omitempty"`
    SourceLabels    bool              `json:"source_labels,omitempty"`
    BaseURL         string            `json:"base_url,omitempty"`
    APIKeyFile      string            `json:"api_key_file,omitempty"`
    APIKeyEnv       string            `json:"api_key_env,omitempty"`
    APIKeyHeader    string            `json:"api_key_header,omitempty"` // header NAME only; never a value
    APIKeyScheme    string            `json:"api_key_scheme,omitempty"`
    NSPrefix        string            `json:"ns_prefix,omitempty"`
}

type IssueCapState struct {                      // runtime counters; rebuilt from the ledger at boot
    Driver          string         `json:"driver"`
    WindowTS        string         `json:"window_ts"`          // RFC3339 UTC of the oldest counted record
    SigCreates      map[string]int `json:"sig_creates"`        // sig → creates inside PerSigWindow
    SigComments     map[string]int `json:"sig_comments"`       // sig → comments inside 1h
    LastCommentTS   map[string]string `json:"last_comment_ts"` // sig → RFC3339 UTC
    ProjectCreatesH map[string]int `json:"project_creates_h"`
    ProjectCreatesD map[string]int `json:"project_creates_d"`
    ProjectCommentsH map[string]int `json:"project_comments_h"`
    GlobalCreatesH  int            `json:"global_creates_h"`
    GlobalCreatesD  int            `json:"global_creates_d"`
    GlobalCommentsH int            `json:"global_comments_h"`
    Suppressed      int            `json:"suppressed"`          // operations folded or capped since boot
}

type IssueAttempt struct {
    Op           string `json:"op"`              // ensure | comment | close | reopen | link | healthcheck
    Driver       string `json:"driver"`
    IdemKey      string `json:"idem_key"`
    Attempt      int    `json:"attempt"`
    MaxAttempts  int    `json:"max_attempts"`
    Outcome      string `json:"outcome"`         // ok | retry | give_up | spooled
    HTTPStatus   int    `json:"http_status"`
    ErrorCode    string `json:"error_code"`
    Retryable    bool   `json:"retryable"`
    BackoffMS    int    `json:"backoff_ms"`
    NextTryTS    string `json:"next_try_ts"`
    Remaining    int    `json:"rate_limit_remaining"`
    ResetTS      string `json:"rate_limit_reset_ts"`
}
```

```json
{"enabled":true,"primary_driver":"github","dedup_window":"30m","quiet_close":"24h","quiet_close_sweep":"5m","op_deadline":"45s","body_max_bytes":60000,"title_max_chars":256,"replay_interval":"5s","replay_batch":100,"healthcheck_interval":"60s","healthcheck_idle":"5m","fail_after_probes":2,"spool_budget_bytes":67108864,"spool_max_entries":20000,"spool_ttl":"72h","spool_min_retention":"1h","max_attempts_per_op":20,"ack_default":"24h","caps":{"per_sig_creates":1,"per_sig_window":"24h","per_sig_comments":6,"comment_min_interval":"10m","per_project_creates_h":20,"per_project_creates_d":200,"per_project_comments_h":200,"global_creates_h":100,"global_creates_d":1000,"global_comments_h":600},"drivers":[{"name":"github","enabled":true,"primary":true,"mirror":false,"dedup_window":"30m","max_attempts":5,"base_backoff":"1s","max_backoff":"60s","jitter":0.25,"timeout":"15s","min_remaining":100,"search_min_interval":"6s","owner":"acme","repo":"payment-api","api_base":"https://api.github.com","token_file":"~/.config/trouble/github.token","token_env":"TROUBLE_GITHUB_TOKEN","labels":["trouble","auto-filed"],"labels_extra":[],"severity_labels":{"critical":"sev:critical","high":"sev:high","medium":"sev:medium","low":"sev:low","info":"sev:info"},"source_labels":true}]}
```

```json
{"driver":"github","window_ts":"2026-09-16T09:00:00.000Z","sig_creates":{"sentinel:sha256v1:9f2c1d3e4b5a6c7d":1},"sig_comments":{"sentinel:sha256v1:9f2c1d3e4b5a6c7d":2},"last_comment_ts":{"sentinel:sha256v1:9f2c1d3e4b5a6c7d":"2026-09-16T09:15:41.009Z"},"project_creates_h":{"1":3},"project_creates_d":{"1":9},"project_comments_h":{"1":3},"global_creates_h":3,"global_creates_d":9,"global_comments_h":3,"suppressed":4}
```

```json
{"op":"ensure","driver":"github","idem_key":"issue_ensure|github|sentinel:sha256v1:9f2c1d3e4b5a6c7d|inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE|ev_01J9Z6Q0M2X4T8V1K7B3N5R8WM","attempt":1,"max_attempts":5,"outcome":"ok","http_status":201,"error_code":"","retryable":false,"backoff_ms":0,"next_try_ts":"","rate_limit_remaining":4821,"rate_limit_reset_ts":"2026-09-16T09:36:00.000Z"}
```

`GapRecord.Cause` gains the value `driver_down` (§3.3) — reported as a TYPES-GAP line so SPEC-TYPES §3.7's
cause set stays the single source of truth.

### 3.13a The codeplane bundle on the issue body (v0.1.1a)

An issue that a human opens must carry the same cross-plane context the agent and the lab saw: a stack trace
with its release and its recent-signature counts is a work item, a stack trace alone is a puzzle. SPEC-05
§3.13a owns the bundle; this section owns the rendering obligation.

- **Position.** When `Incident.Codeplane` is non-nil the body of §3.9.3 carries one extra section, placed
  between the field table and `**Evidence bundle (scrubbed)**`, in exactly this shape:

````markdown
**Codeplane bundle**

```json
{"side":"sentinel","sig":"sentinel:sha256v1:9f2c1d3e4b5a6c7d","grp":"grp_01J9Z6Q0M2X4T8V1K7B3N5R8WH","project":"7","release":"payment-api@2.4.1","regressed":true,"recent":[{"sig":"sentinel:sha256v1:9f2c1d3e4b5a6c7d","count":412,"first_ts":"2026-09-16T09:14:03.221Z","last_ts":"2026-09-16T09:15:41.009Z"}],"ts":"2026-09-16T09:15:41.009Z"}
```
````

- **Verbatim.** The bytes inside the fence are `json.Marshal(*inc.Codeplane)` (SPEC-TYPES §3.15.12) unchanged:
  no prose re-rendering, no field renaming, no re-indenting, no dropping of empty fields. A block a reader
  cannot diff against the incident record is not the same evidence, so byte-equality is the test (§7).
- **Absent bundle.** `Codeplane == nil` → no heading, no empty fence, and **zero other byte deltas**: for a
  pure-system incident the body stays byte-identical to the §3.9.3 golden, which is what keeps that golden
  test meaningful and every already-published body stable.
- **Never the anchor.** §3.2's anchor rule and the two marker lines of §3.9.3 (`<!-- trouble:sig=… -->` and
  `<!-- trouble:inc=… driver=… ver=1 -->`) remain the identity of the issue: the bundle is additive context,
  never the thing a read-back search matches on, and never a substitute for the incident link. A body that
  carries a bundle and not both markers is a rendering bug, and the desk never sends one.
- **Truncation order.** §3.9.3's order gains one step, markers always last: evidence bundle → codeplane
  block → markers. A body over `body_max_bytes` (60 000) loses the fence before it loses a marker, and
  `payload.truncated=true` is set as today.
- **Scrubbing.** The marshalled bundle is assembled from the already-scrubbed incident and the whole body is
  re-scrubbed as one blob before the call (§3.9.3 and the cross-spec contract table of §4): no bundle field
  is exempt. `redactions applied` counts that pass exactly as it does today.

## 4. Wiring

| Producer | Consumes | Emits kinds |
|---|---|---|
| internal/issues | IssueDeskConfig, Incident, CodeplaneContext, Evidence, ScrubResult, SpoolEntry | `issue`, `gap` |

Data flow (one incident, default config):

```
SPEC-05 ladder reaches rung=outlets, state=verifying
  → Desk.EnsureBySig(inc, bundle)                     [op_deadline 45s]
      ↳ anchorIndex[sig] hit?  ── no ──► cap check ──► driver.EnsureBySig ──► IssueRef → ledger(issue)
      ↳ hit inside dedup_window ───────► cap check ──► driver.Comment(fold) ─► ledger(issue)
      ↳ hit outside window ────────────► cap check ──► driver.Comment(block) ─► ledger(issue)
  → any transient failure  → SpoolStore.Put(SpoolEntry{Kind:"issue", IdemKey}) → ledger(issue result=spooled)
  → any healthcheck failure → ledger(issue healthcheck) + ledger(gap cause=driver_down); INCIDENT CONTINUES
  → SPEC-08 files a row with the same sig → Desk.Link(task_id) → comment + ledger(issue op=link)
  → SPEC-07 returns a brief for the sig  → Desk.Link(research_id) → comment + ledger(issue op=link)
  → Incident.Codeplane non-nil           → body renders the bundle block verbatim (§3.13a) → 0 extra calls
  → quiet-close sweep (5m): resolved + quiet 24h + evidence passed + no open row/brief + driver ok
                                          → driver.Close → comment + ledger(issue op=close)
```

Dependency direction (one-way, enforced by SPEC-INDEX §3.6): `internal/issues` imports `internal/types`,
`internal/ledger`, `internal/scrub`, `internal/lifecycle` (config + version). Nothing imports back. The ladder
calls the desk; the desk never calls the ladder. SPEC-10 renders `IssueRef` rows from the ledger inside the
incident story (SPEC-10 §2 route 3) and makes no driver call; SPEC-12 §3's health aggregator reads `Health()`
and the CLI reads `CapState()`. The desk writes to the ledger through the single writer (hub only) and never opens a ledger
file itself.

Cross-spec contracts this package depends on:

| Contract | Owner | Use |
|---|---|---|
| sig string + digest | SPEC-01, SPEC-TYPES §6.3 | identity of an issue; never re-derived here |
| scrubbing before persistence, `Redactions` counter | SPEC-02 | bundle and body are scrubbed pre-call and pre-ledger |
| ladder rung degradation semantics | SPEC-05 §3.6 | transient outlet failure is non-fatal; `issues:degraded` |
| verification evidence tuple | SPEC-05, SPEC-TYPES §3.7 | pre-condition for quiet-close (`result=passed`) |
| `BoardRow.Sig`, `BoardRow.IssueRefs[]`, board row open/closed state | SPEC-08 | linkage + the "no open row" close gate |
| `ResearchOutcome` for a sig | SPEC-07 | linkage + the "no open brief" close gate |
| `Incident.Codeplane`, the persisted cross-plane bundle | SPEC-05 §3.13a, SPEC-TYPES §3.15.12 | body rendering only (§3.13a); never re-derived here, never a substitute for the §3.2 anchor |
| `GapRecord`, gap emission rights | SPEC-TYPES §3.7, SPEC-INDEX §3.4 | outage and drop accounting |
| token/key files 0600, "never in argv" | SPEC-12 §3.6, SPEC-02 | config explain + boot validation |

## 5. Errors

All ten codes of the area range are used; every one is in SPEC-TYPES §5 with the class shown here.

| Code | Class | Raised when | Retry policy | Ledger record |
|---|---|---|---|---|
| `TROUBLE-ISSUES-001` | transient | driver API error: 5xx, connection reset, TLS failure, timeout, or a 422 create rejection (`retryable:false`) | `retryable` decides; 5xx ≤5 attempts then spool; 422 never | `kind=issue` op record + `IssueAttempt` per retried attempt |
| `TROUBLE-ISSUES-002` | transient | rate-limited: 429, 403 with rate-limit headers, or `remaining ≤ min_remaining` | sleep to `reset` (≤15 m), then spool | `IssueAttempt{outcome:"retry"}` + healthcheck `ok=false` |
| `TROUBLE-ISSUES-003` | permanent | auth failed: 401, 403 without rate-limit headers, missing/insecure token file, token on argv | none; driver → `failed` | `kind=issue` op record; healthcheck failure follows |
| `TROUBLE-ISSUES-004` | permanent | issue cap reached (§3.8): per sig, per project, or global | none; never spooled | `kind=issue {op:"cap", cap, window}` + group `suppressed`++ |
| `TROUBLE-ISSUES-005` | transient | driver spool full: budget/exhausted with all entries inside `spool_min_retention`, or the spool write failed | next tick | `kind=issue {op:"drop"}` when eviction occurs; refusal record otherwise |
| `TROUBLE-ISSUES-006` | transient | spool replay failed (or a read-back mismatch on duckbrain) | `attempts++`, backoff, drop at TTL/20 attempts | `kind=issue {op:"replay"}` |
| `TROUBLE-ISSUES-007` | permanent | referenced issue not found at the driver (404/410 on comment or patch) | none; anchor invalidated | `kind=issue` op record with `result:"anchor_lost"` |
| `TROUBLE-ISSUES-008` | permanent | close refused (423 locked, or read-back still open) / reopen refused | 3 attempts, then leave open + note (close) or file a superseding issue (reopen) | `kind=issue {op:"close"\|"reopen"}` |
| `TROUBLE-ISSUES-009` | transient | healthcheck failed `fail_after_probes` (2) times consecutively | next interval; recovery drains the spool | `kind=issue {op:"healthcheck", ok:false}` + `kind=gap cause:"driver_down"` |
| `TROUBLE-ISSUES-010` | permanent | duplicate suppressed inside the dedup window (repeat `IdemKey`, or a fold beyond the comment cap) | none; no driver call | `kind=issue` op record with `result:"suppressed"` |

Every code raised to the ladder is mirrored into `Record.payload.error_code` (SPEC-INDEX §5 rule 3), so the
audit trail needs no log file. `policy_refused` is not used by this area: the desk's refusals are policy
decisions expressed as caps (`TROUBLE-ISSUES-004`), not polkit/do-not-touch refusals.

**The codeplane bundle adds no codes.** Rendering it is part of the existing `EnsureBySig`/`Comment` bodies
and raises none of `TROUBLE-ISSUES-001..010` on its own: a nil bundle renders nothing and writes no record,
and a bundle present or absent changes no driver call, no cap and no spool decision.

## 6. Edge cases

1. **Create committed but the response was lost.** The desk never re-creates blindly: it searches for the sig
   marker (≤5 queries, ≥`search_min_interval` apart). Found → adopt that issue as the anchor; search
   unavailable → spool as `ensure_unverified` and resolve on replay. Result: at most one issue even under a
   lost 201.
2. **Convergence: sensor + sentinel + collector fire inside one window.** All three arrive with the same sig;
   the per-sig mutex serialises them, the first creates, the other two fold. One issue, three fold comments at
   most, bounded by `comment_min_interval`.
3. **A human deletes the issue.** The next comment/close returns 404 → `TROUBLE-ISSUES-007`, the anchor is
   invalidated with a record, and the next trigger re-creates a fresh issue with the same sig (new external
   id). The ledger shows both external ids for the sig.
4. **A human closes the issue while the incident is open.** The fold path detects `state=closed` on read-back,
   reopens it (comment + `PATCH state=open`) and records `anchor_reopened` — the incident is not closed and no
   new issue appears.
5. **A label is renamed or deleted by an admin.** Labels are advisory; the sig marker is the only key. The
   desk re-adds its labels on the next write and records nothing new.
6. **Token file mode is 0644.** Boot marks the driver `failed` with `TROUBLE-ISSUES-003` and makes no outbound
   call. A relative `~` in the path is expanded before the mode check.
7. **Body over 60 000 bytes.** Truncated on a rune boundary in the evidence section with an elision marker,
   `payload.truncated=true`; the sig marker and the metadata table are never truncated.
8. **Boot with a populated spool from a previous run.** The first successful healthcheck triggers `Replay`
   before new work; `ensure_unverified` entries resolve through search first. Nothing is filed twice.
9. **Spool directory unwritable or the disk is full.** In-memory queue up to 1 000 entries (≤8 MB) absorbs the
   burst; beyond that `TROUBLE-ISSUES-005` is returned, the rung degrades, and the group's `suppressed` counter
   keeps the count. Nothing is silently dropped (SPEC-02's principle applied to issues).
10. **Rate-limit reset in the past or a backwards clock jump.** `reset < now` is treated as "probe again in
    60 s" — no infinite sleep, no negative backoff. Windows themselves use the monotonic clock.
11. **A human deletes comments.** `IssueRef.Comments` lowers; the desk takes the max of observed and recorded,
    records the discrepancy once, and continues (fold cadence is unaffected).
12. **Restart under an active cap.** `IssueCapState` is rebuilt from the ledger for the open windows, so a
    restart cannot lift a cap; the rebuild is bounded by the 24 h window and reads only `issue` records.
13. **Quiet-close races a recurrence in the same second.** A qualifying recurrence with `ts ≥ ResolvedTS`
    cancels the sweep (the sweep re-reads `Group.LastSeenTS` immediately before the close call) and the sweep is
    idempotent: two sweeps produce one close.
14. **Ack active during a recurrence.** The incident and the anchor still update; the fold comment is
    suppressed and one summary comment is posted when the ack expires.
15. **Both drivers enabled and healthy.** The primary files; `mirror=true` files the same sig in the second
    store with the same marker; the ledger records both refs and the dashboard joins them on the sig — the
    story stays single (AC-22).

## 7. Testing

| Test file | Cases | Numeric pass thresholds |
|---|---|---|
| `internal/issues/contract_conformance_test.go` | the 14-case battery run table-driven against **both** drivers over `httptest` servers: create, fold-inside-window, fold-outside-window, repeat-idem-key, comment-once-per-trigger, close-idempotent, close-refused, reopen, healthcheck ok/degraded/failed, read-back adopt, 404 anchor loss, rate-limit pause | 14/14 pass for each driver; zero divergence between the two drivers' observable behaviour |
| `internal/issues/github_driver_test.go` | label set + severity mapping, golden title/body (byte-exact against the §3.9.3 template), **golden search query string**, sig-marker round-trip, 401/403/404/410/422/429/5xx mapping, `Retry-After`, `remaining ≤ min_remaining`, read-back create verification, token-file mode matrix (0600 ok; 0644/0640/missing/symlink → `TROUBLE-ISSUES-003`), argv rejection, 15 s timeout, body truncation at 60 000 on a rune boundary | body golden byte-identical; query string equality; token value absent from every log line and ledger line (grep assertion: 0 hits) |
| `internal/issues/duckbrain_driver_test.go` | ns/key golden strings, one-key-per-comment, `idem` read-back mismatch → `TROUBLE-ISSUES-006`, conditional-write and GET-then-PUT paths, backend-down spooling, header-name-only config rendering | key layout equality; 0 key-value bytes in stdout/stderr/ledger; recovery ≤ 2 healthcheck intervals |
| `internal/issues/caps_test.go` | 1 000 ensures for 1 sig in 1 h → exactly 1 create and ≤6 comments with ≥994 recorded as `TROUBLE-ISSUES-010`/`004`; per-project and global caps; cap survival across a restart (index rebuild); capped ops never spooled | exactly 1 issue row per sig; comments ≤6; spool contains 0 capped ops |
| `internal/issues/spool_test.go` | bounds (64 MiB / 20 000), drop-oldest with the 1 h floor, refusal with `TROUBLE-ISSUES-005` when all entries are young, gap record per drop, TTL and `max_attempts_per_op` drops, ordering `(next_try_ts, ts, id)`, per-sig single flight, 10 000-entry drain | 10 000 entries drain in < 600 s with 0 duplicate issues and 0 duplicate comments; every drop has exactly 1 `gap` + 1 `issue{op:"drop"}` record |
| `internal/issues/quietclose_test.go` | 24 h quiet → close with reason comment; recurrence at 23 h 59 m → no close; evidence not `passed` → no close; open board row → no close; open research brief → no close; `manual=true` → never closed; driver down → deferred then closed after recovery; close-refused → left open with note; reopen after close → same external id | 1 close call per incident, 0 new issues on reopen; deferral re-tried within 2 sweeps of recovery |
| `internal/issues/link_test.go` | AC-22 end to end: one bug through sensor + sentinel + collector, with a fake SPEC-08 row writer and a fake SPEC-07 brief → 1 issue, `TaskID` and `ResearchID` set, cross-ref comments, 0 duplicates; board row `IssueRefs[]` filled both directions | 1 incident, 1 group, 1 issue, 1 board row, 0 duplicates across 500 synthetic recurrences |
| `internal/issues/ledger_payload_test.go` | every §3.3 payload schema round-trips against its golden JSON fixture; every emitted code is in `TROUBLE-ISSUES-001..010`; `retryable` present on every coded record; `seq` monotonic via the ledger test double | 100 % schema matches; 0 codes outside the range; `actor.version`/`git_sha` present on every record |
| `internal/issues/ac8_test.go` (AC-8) | file → comment on recurrence → close after quiet, against a fake driver, asserting the ladder-visible `IssueRef` at each step | issue filed within 1 `op_deadline`; recurrence folded ≤2 s p95 on a local fake; close after exactly 24 h of quiet |
| `internal/issues/codeplane_test.go` (AC-31) | the body golden **with** a bundle: the block byte-exact and in the §3.13a position (after the field table, before the evidence bundle); the body golden **without** a bundle: byte-identical to the §3.9.3 template (0 deltas); a fixture missing either marker line is rejected; truncation at 60 000 drops the fence and keeps both markers; `redactions applied` unchanged by the bundle | both goldens byte-identical; 0 bodies emitted without the two marker lines; truncation removes the fence bytes only |

Conformance battery detail (the 14 cases, because this battery is the contract's executable form): the same
table is run against `github` and `duckbrain` with a fake transport, and the fake counts calls — a case that
passes with the wrong call count fails the battery (e.g. a fold that issues a create, or a repeat idem key that
issues any call at all).

Budget assertions: `internal/issues` adds ≤8 goroutines and ≤6 MB RSS to the steady 80 MB budget; the healthcheck
and replay timers are single `time.Ticker`s per driver (no per-entry goroutines).

## 8. hilo impact

Packages and files created (greenfield repo `~/trouble`):

| File | Purpose |
|---|---|
| `internal/issues/desk.go` | `Desk`, anchor index, caps, the ladder entry points |
| `internal/issues/driver.go` | `IssueDriver`, `DriverFactory`, `Register`, the shared transport + backoff |
| `internal/issues/github.go` | github driver: auth, labels, template, search, close, mapping |
| `internal/issues/duckbrain.go` | duckbrain driver: KV layout, idem read-back, health |
| `internal/issues/spool.go` | bounded spool: atomic write, eviction, replay ordering |
| `internal/issues/quietclose.go` | quiet-close sweep + reopen path |
| `internal/issues/link.go` | sig linkage with SPEC-07/SPEC-08 |
| `internal/issues/config.go` | `IssueDeskConfig` load, clamps, token-file mode checks |
| `internal/issues/*_test.go` | the battery in §7 |
| `cmd/trouble/issues.go` | the `issues` CLI verbs (thin wrapper over `Desk`) |

Fan-out (what this package imports): `internal/types` (highest fan-in node in the repo), `internal/ledger`
(writer interface only), `internal/scrub` (pre-call scrubbing), `internal/lifecycle` (config precedence +
version stamping). Fan-in (what imports it): `internal/ladder` (outlet rung), `internal/dashboard` (health +
cap counters + ledger reads), `internal/registry` (the `flow.file_issue` module implementation, SPEC-08/SPEC-06),
`cmd/trouble`. hilo graph edges added: `issues → {types, ledger, scrub, lifecycle}` and
`{ladder, dashboard, registry, cmd} → issues`; no cycle is possible because the ladder's dependency on issues
is through a function call with `internal/types` values only.

Blast radius: zero on the fleet — trouble is greenfield at `~/trouble`, no fleet repo is touched by
this spec, and no fleet path, host, token or board file is referenced (every such value is a config key with a
neutral default, per non-negotiable #1). The only external surfaces are two outbound HTTP APIs spoken through
configuration.
