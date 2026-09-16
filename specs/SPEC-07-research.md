# SPEC-07 — research: the Off-by-One rung, class-slug derivation, degrade paths (trouble v0.1)

Spec: SPEC-07
Area prefix: TROUBLE-RESEARCH
Package: internal/research
Consumed types: Record, RecordKind, Sig, SigSource, Origin, Actor, Prefix, Incident, GapRecord, Duration, ClassSlug, DiscoverRequest, DiscoverResponse, SubmitRequest, SubmitResponse, QueueStatus, ResearchOutcome, PlayTask, SentryEvent, Evidence, SpawnRequest, IssueRef, SkillCandidate, ResearchDriver
Local types: Deps, Service (package-owned; §3.0), driverOffByOne, driverNone, driverWebhook, table, slugEntry, taxonomyRule, subjectFacts, corpus, corpusHit, labHealth, researchCost, pollState, wireDiscoverRequest, wireSubmitRequest, wireQueueStatus, wireLabError, wireHealth, wireStats
ACs: AC-17, AC-20
PRD: §05, §11, §12

## 1. Purpose

The research rung asks Off-by-One for a pre-solved answer before an agent is spent. PRD §05 ②½
("forward fingerprint + stack + release + repro → receive hypotheses / similar cases / patch sketch")
describes an API that does not exist. The lab was measured live: it is a **`problem_class`-keyed
verified-answer cache with a strict JSON decoder and an async submit queue**, and it does not accept a
fingerprint at all.

```
POST :8766/api/v1/problems/discover {"problem_class":"svc-crash-loop","fingerprint":"sentinel:9f2c"}
 -> 400 {"error":"invalid_request","message":"invalid JSON: json: unknown field \"fingerprint\""}
GET  :8766/health       -> {"status":"ok","uptime":"3h47m6s"}     (root path; /api/v1/health is 404)
GET  :8766/api/v1/stats -> 1716 problems / 1900 answers / 1872 verified / queue_depth 0 / solver_available true
```

Three consequences are normative here. (1) **Class-slug derivation is the work** (§3.1): the lab is keyed
on a kebab-case class slug, trouble holds signatures and units, and everything between them is code trouble
owns. (2) **There is no webhook push**: results arrive only by polling `/api/v1/queue/{submission_id}`, the
measured mean solve is ~3m and the submit response's own `estimated_time` was `3m30s`. (3) **`found:false`
is not absence**: the lookup key is class + env + lang + version, so a verified answer whose signatures
carry empty environment/language/version is invisible to a probe that supplies those fields — the corpus
grep is a mandatory second probe.

Owned here: the driver contract and its three implementations, class-slug derivation and its config table,
discover → submit → poll, the corpus-grep fallback, the agent prompt with and without a brief, the degrade
paths and their ledger records, per-request cost accounting, the `research` record payload. Not owned: the
ladder's transitions, rung selection, agent budget or kill-switch semantics (SPEC-05); the ledger writer
and record envelope (SPEC-01); scrubbing rules (SPEC-02); execution of anything a brief suggests
(SPEC-06). Research never executes anything — a brief is data and the only hands stay registry tool calls.

**Non-negotiable:** research is best-effort. The lab being unreachable, slow, rejecting, unavailable or
silent never blocks rung ②, rung ③ or escalation. Every failure path in §5 ends with a `ResearchOutcome`
the ladder can proceed past, a ledger record, and — only where a source's coverage is genuinely lost — a
`GapRecord`.

AC coverage: **AC-17** is exercised by the discover→submit→poll path and the brief handed to the agent (§2, §3.5); **AC-20** is exercised by the degrade matrix and the gap records (§3.8, §5). Sections no AC reaches are **design constraints**: the driver interface signatures (§2), the class-slug table and its fallback (§3.2), the strict wire codec (§3.4) and the budget/poll math (§3.7) are obligations of this subsystem rather than user-visible acceptance criteria — the loop records them as constraints per SPEC-INDEX §7 step 2.

## 2. Interface

### 2.1 Go API

```go
type Deps interface {                                    // implemented by the daemon; wired at the composition root
    Append(ctx context.Context, r Record) error           // internal/ledger — the single writer
    Scrub(targets []string, b []byte) ([]byte, int, bool) // internal/scrub — bytes, redactions, truncated
    Clock() time.Time                                     // wall clock (RFC3339 UTC stamping)
    Since(anchor any) time.Duration                       // monotonic clock (poll budgets, cooldowns)
}
type Service struct{ /* unexported: cfg, deps, driver, table, corpus, inflight, cost, cooldown, fence */ }

func New(cfg map[string]any, deps Deps) (*Service, error)   // resolves + validates config (§4.3); never dials
func (s *Service) Run(ctx context.Context, inc Incident, sig Sig, bundle map[string]any) (ResearchOutcome, error)
func (s *Service) Resume(ctx context.Context, out ResearchOutcome) (ResearchOutcome, error) // boot / park resume
func (s *Service) Park(ctx context.Context, out ResearchOutcome) (ResearchOutcome, error)   // SIGTERM / kill-switch
func (s *Service) Snapshot() map[string]any   // {driver, lab_health, cost, inflight, cooldown_until, fence}
func (s *Service) Close() error               // cancels pollers, waits ≤2s, no writes after Close

func DeriveClassSlug(sig Sig, f subjectFacts, t *table) ClassSlug  // total: no error return (§3.1)
func LoadTable(path string) (*table, error)                        // malformed → the config loader refuses it
func BuildAgentPrompt(inc Incident, out ResearchOutcome, evidence map[string]any) (string, error) // §3.7
func Digest(b []byte) string                                       // sha256 hex, first 16 chars

// Adapter surface. The ladder declares its own consumer-side interface (SPEC-05 §2, `ResearchDriver`
// with `Request`/`Poll`); these methods exist so one thin adapter in the wiring layer satisfies it and
// no second research API is invented. `sub` carries the ladder's Subject fields as a map:
// "slug", "description", "cadence", "context" — the same four values as §3.3.
func (s *Service) Request(ctx context.Context, inc Incident, sub map[string]any) (ResearchOutcome, error)
func (s *Service) Poll(ctx context.Context, resID string) (ResearchOutcome, error)
func (s *Service) Outcomes(sig string) []ResearchOutcome  // rung outcomes for a sig, newest first
```

A caller-supplied `sub["slug"]` is used verbatim and derivation is skipped (no code 005); an absent or
empty slug means `DeriveClassSlug` runs (§3.1). `Outcomes` reads the in-memory rung registry, which boot
rebuilds from the ledger, so `internal/dashboard` (SPEC-10) and `internal/skills` (SPEC-11, which takes
`[]ResearchOutcome`) can read outcomes without a second store.

`Run` is the only entry point the ladder calls; it returns a `ResearchOutcome` whose `State` is
`requested | returned | degraded | skipped`, and the next transition is SPEC-05's.

### 2.2 Driver interface and its three implementations

```go
type ResearchDriver interface {
    Name() string                                                              // off-by-one | none | webhook
    Discover(ctx context.Context, req DiscoverRequest) (DiscoverResponse, error)
    Submit(ctx context.Context, req SubmitRequest) (SubmitResponse, error)
    Poll(ctx context.Context, submissionID string) (QueueStatus, error)
    Health(ctx context.Context) (labHealth, error)
    Stats(ctx context.Context) (labHealth, error)
}
```

| Driver | config value | Behaviour |
|---|---|---|
| `driverOffByOne` | `off-by-one` (default) | Full protocol: discover → corpus grep → submit → poll. The only driver that queues work. |
| `driverNone` | `none` | Every method returns `ErrDriverDisabled`; `Run` writes one `research` record with `state:"skipped"`, `skip_reason:"driver_none"`, code TROUBLE-RESEARCH-008. Zero HTTP, no gap. |
| `driverWebhook` | `webhook` | The same shapes relayed to `research.webhook_url` (POST) and `research.webhook_poll_url` (GET `?submission_id=`), for operators running their own lab behind a gateway. Identical outcome semantics and degrade paths. |

All three ship in v0.1 (the driver seam is a PRD §12 named seam, not a cut-line item). An unknown `driver`
value fails startup config validation, never a rung.

### 2.3 The four lab surfaces — request/response schema

Field names below are the **lab's wire names**; the codec in §3.4 maps trouble's struct tags onto them.
Bodies are built in a buffer and sent as `Content-Type: application/json`.

**A. `POST /api/v1/problems/discover` — cache lookup (read-only; never queues work)**

| Request field | Type | Req | Notes |
|---|---|---|---|
| `problem_class` | string | yes | kebab-case slug from §3.1 |
| `environment`, `language`, `version` | string | no | sent only on the narrowing probe D2 (§3.2) |
| `include_related` | bool | no | trouble always sends `false` (smaller body) |

Responses: 200 `{"found":true,"answer":{"id":1210,"problem_class":"…","env":"…","solution":"…","evidence":{…},"signatures":{…}}}` → build the brief (§3.7) · 200 `{"found":false,…}` → **not absence**, so D3 corpus grep (§3.2) then submit · 404 `{"error":"not_found","message":"problem class not found"}` → miss, submit · 400 `{"error":"invalid_request","message":"invalid JSON: json: unknown field \"<name>\""}` → code 002, the decoder names only the FIRST unknown field · 200 with `Content-Type: text/html` (the SPA catch-all) → protocol mismatch, never a decode panic: code 001, `note:"non_json_200"`.

**B. `POST /api/v1/problems/submit` — queue on a miss (the only write path; `POST /api/v1/answers` is 404)**

| Request field | Type | Req | Notes |
|---|---|---|---|
| `problem_class` | string | yes | the same slug as discover |
| `description` | string | yes | free text, scrubbed, ≤8KB; carries a discovered-but-unverified solution |
| `cadence` | string | yes | enum; exactly one value from `cadence_ladder` (§3.3) |
| `context` | object | yes | MUST be a JSON object; fingerprint + stack + release + repro + provenance (§3.3) |

Exactly these four top-level fields. The lab also accepts `environment`, `language`, `version`,
`error_message`, `stack_trace` and `required_tools`, and rejects everything else with the same
`unknown field` 400 — trouble sends none of them at top level, because each extra top-level field is an
avoidable 400 surface (the decoder reports one unknown field per attempt) while `context` is the field the
contract reserves for provenance. Adding a top-level field is a code change plus an `/openapi.json`
re-read, never a runtime heuristic.

Responses: 200/201 `{"submission_id":"sub_87ee13","problem_class":"…","status":"queued","position":7,"estimated_time":"3m30s","existing_solutions":20}` → `requested`, poll it · 409 duplicate already queued for the class → code 003, **treated as queued**, not a failure · 400 unknown field or `{"error":"invalid_request","message":"ingest: invalid cadence"}` → code 002, and a cadence 400 walks the cadence ladder (§3.3) with ≤2 retries · 503 `{"error":"solver_unavailable"}` → code 004, degraded, discover + corpus stay usable.

**C. `GET /api/v1/queue/{submission_id}` — the only result channel**

Fields: `status` (`pending|queued|solving|solved|failed`) · `stage` (`queued`, `solving`, `storing`; `failed` on terminal failure) · `position` (recorded, never used for control flow) · `estimated_time` (optimistic: `30s` was observed for a solve that took 7.8s, `3m30s` typical) · `started_at`/`completed_at` on terminal states · `answer` when solved, same shape as discover's `answer`.

| Lab state | `QueueStatus.State` |
|---|---|
| `pending`/`queued`, `solving` | `"queued"` / `"solving"` (`stage` wins when both are present) |
| `solved`, or `answer` present | `"solved"` → build the brief |
| `failed` | `"failed"` → code 004, `degraded_reason:"solver_unavailable"` |
| 404 for a submission trouble submitted | code 004, `note:"queue_not_found"`, then one D1 discover |
| unknown `status` string | counted in `payload.unknown_status`, treated as `solving`; the poll budget still ends it |

**D. `GET /health` (root) and `GET /api/v1/stats` — liveness, capability, budget pre-check**

`/health` → `{"status":"ok","uptime":"3h47m6s"}`. `/api/v1/stats` → snake_case keys `total_problems`,
`total_answers`, `verified_answers`, `queue_depth`, `hit_rate`, `coverage`, `solver_available` (measured
at A7: 1716 / 1900 / 1872 / 0 / true). Used for the startup capability probe, the cooldown half-open probe
and `Snapshot()`. The body is decoded into a typed struct — never a `jq` filter (filters on
`problems`/`answers` return null).

### 2.4 Startup capability probe

At boot and every `capability_probe_interval` (default `1h`): `GET /openapi.json` plus `GET /api/v1/stats`,
asserting that `paths` contains `/api/v1/problems/discover`, `/api/v1/problems/submit` and
`/api/v1/queue/{submission_id}`. All three present → full mode. `submit` or `queue` missing →
**discover-only mode**: D1/D2 + corpus grep run, submit never does (`state:"skipped"`,
`skip_reason:"driver_capability_missing"`, no error code — a detected capability, not a failure), recorded
once per probe change rather than per incident. `/health` unreachable → cooldown (§5.2) with the probe
retried every `health_probe_interval` (default `60s`).

## 3. Data model

### 3.0 Local type rule

`internal/research` exports `Deps` and `Service` (exported names, unexported fields) as in-process
plumbing only: **no local type in this spec appears in an HTTP body, a TOML schema, a ledger payload, a
config file or another spec.** Everything crossing a spec boundary is a SPEC-TYPES type; the `wire*`
structs are unexported precisely because the wire is the lab's, not trouble's.

### 3.1 Class-slug derivation — the real work

`func DeriveClassSlug(sig Sig, f subjectFacts, t *table) ClassSlug` — pure: no network, no filesystem.
A caller-supplied slug (`sub["slug"]` / a `Subject` that already carries one) wins and derivation is
skipped; derivation runs when the caller has none, which is the sensor-sourced path where nothing but the
sig and its facts exist.

| Input | Source | Normalization |
|---|---|---|
| `sig.Source` | `Sig.Source` | one of `journald`, `dbus`, `psi`, `disk`, `timers`, `inotify`, `sentinel`, `collector`, `generic` |
| `subject` | journald: `_SYSTEMD_UNIT` or the `Origin.Source` suffix after `:`; dbus: the unit in the object path (`_2d` → `-`); sentinel: project slug; collector: the configured app name; disk: mount point; inotify: watched-path basename; timers: timer unit; psi: the scope (`cpu`/`memory`/`io`); generic: `Origin.Source` suffix | `kebab()`: lowercase, `[^a-z0-9]+` → `-`, collapse runs, trim `-`, strip `.service`/`.timer`/`.socket`/`.scope`, `name@1234` → `name@`, truncate at a `-` boundary to 40 chars |
| `app_kind` | `research.unit_kind.<subject>` → else `container_markers` (default `docker`,`podman`,`containerd`) over `Origin.Source` → else `.service`-shaped ⇒ `unit` | `unit \| container \| script \| unknown` |
| `taxonomy` | the ordered rule list below, first match wins, matched over the event message + sig + the top stack frame | a bucket, or `error` when nothing matches |

Default taxonomy rules (built in, overridable from the table file; ordered, first match wins):

```
oom|out of memory|cannot allocate memory|memory cgroup                  => resource_exhaustion
no space left|disk full|quota exceeded|inode                            => resource_exhaustion
start request repeated too quickly|start-limit-hit|restart(ed|ing)? (loop|too quickly)
                                                                       => crash_loop
panic:|fatal:|segmentation fault|exit status 1|2|13[0-9]               => crash_loop
permission denied|EACCES|EPERM|auth_admin|polkit                       => permission_denied
connection refused|timed out|timeout|ETIMEDOUT|dial tcp|no route to host => network_timeout
no such file|unknown field|invalid (config|yaml|json)|parse error|unmarshal => config_error
deadlock|queue (wedge|full)|pool exhausted|backpressure|blocked on      => queue_wedge
corrupt|checksum mismatch|bad magic|torn|truncated                     => data_corruption
module not found|cannot find module|ImportError|dependency              => dependency_failure
```

Resolution order — the first step that yields a slug wins: (1) **exact entry** — `(source, subject,
taxonomy)` all equal; (2) **source wildcard** — entry with `subject = "*"`; (3) **taxonomy wildcard** —
entry with `taxonomy = "*"`; (4) **composition** — `kebab(subject) + "-" + kebab(taxonomy)`, with an empty
subject falling back to `kebab(taxonomy)`, an empty taxonomy to `"error"`, truncated to 64 chars at a `-`
boundary; (5) **fallback** — `research.fallback_slug` (default `"unknown"`) with `ClassSlug.Fallback =
true`. `AppKind` and `Taxonomy` are populated even on a fallback: they are the only useful content of a
fallback brief.

**Derivation never blocks the ladder**: the function is total (no error return), the table is loaded at
construction and swapped by the config hot-reload path, and a table that fails validation leaves the active
table in place with the config loader (SPEC-12) reporting it — a rung is never failed by a table. A
fallback slug sets `Fallback=true` and the caller records TROUBLE-RESEARCH-005.

Table format (`research.table`, default: the built-in entries; a path loads and merges them):

```toml
# research-classes.toml — match fields support "*" (matches anything); template is optional.
[[class_slug]]
slug     = "host-io-pressure"
match    = { source = "psi", subject = "io", taxonomy = "resource_exhaustion" }
note     = "PSI io.full on the host itself"
[[class_slug]]
slug     = "service-crash-loop"
match    = { source = "dbus", subject = "*", taxonomy = "crash_loop" }
template = "{subject}-crash-loop"     # {subject} and {taxonomy} placeholders
[[class_slug]]
slug     = "app-unhandled-exception"
match    = { source = "sentinel", subject = "*", taxonomy = "crash_loop" }
template = "{subject}-unhandled-exception"
```

Built-in entries, one per shipped source, generic by construction (no fleet value in any default; fleet
mappings live in `examples/research-classes.example.toml`):

| slug / template | match (source, subject, taxonomy) |
|---|---|
| `host-cpu-pressure`, `host-memory-pressure`, `host-io-pressure` | psi, cpu/memory/io, resource_exhaustion |
| `{subject}-crash-loop` | journald, `*`, crash_loop · dbus, `*`, crash_loop |
| `{subject}-unhandled-exception` | sentinel, `*`, crash_loop · collector, `*`, crash_loop |
| `{subject}-permission-denied` | journald, `*`, permission_denied · dbus, `*`, permission_denied |
| `{subject}-config-error` | journald, `*`, config_error |
| `{subject}-network-timeout`, `{subject}-queue-wedge`, `{subject}-data-corruption`, `{subject}-dependency-failure` | `*`, `*`, the matching taxonomy |

Worked example: sig `journald:sha256v1:2ab4c6d8e0f1a3b5`, unit `payment-worker.service`, message `start
request repeated too quickly` → `{"slug":"payment-worker-crash-loop","source":"journald","app_kind":
"unit","taxonomy":"crash_loop","fallback":false}`.

### 3.2 The discover sequence (D1 → D2 → D3) and the exact corpus fallback

```
D1  POST /api/v1/problems/discover {problem_class, include_related:false}   # broadest key
      found:true -> cached brief (state returned, corpus_grep_hit false) | found:false or 404 -> D2
D2  POST /api/v1/problems/discover {problem_class, environment, language, version}
      only when the incident actually carries env/lang/release facts; runs at most once
      found:true -> cached brief | found:false or 404 -> D3
D3  corpus grep (below): hit -> corpus brief (state returned, corpus_grep_hit true) | miss -> submit
```

Sensor-sourced sigs (`psi`, `disk`, `timers`, `inotify`) always send D1 with empty `environment`,
`language`, `version` and skip D2: narrowing on facts the sensor never had is exactly the measured way to
hide a verified answer. `version` is the observed app release for sentinel/collector sigs
(`SentryEvent.Release`), the empty string otherwise.

**Corpus grep, exactly.** `corpus` is an interface so the lab's on-disk layout is configuration:
`type corpus interface { Grep(ctx context.Context, class string, terms []string) ([]corpusHit, error) }`.

1. Roots = `corpus_roots` resolved against `lab_data_dir` (absolute allowed; defaults `["answers"]` against
   `"./data"`). A missing or unreadable root emits a `GapRecord` cause `research_corpus_unreadable` and the
   grep continues with the remaining roots — never an abort.
2. Walk newest-first by mtime, ≤`corpus_max_files` (5000) files matching `corpus_glob` (default `*.json`),
   skipping files over `corpus_max_bytes_per_file` (1 MiB), under a `corpus_timeout` (3s) wall cap; on the
   cap set `corpus_timeout:true` and treat the grep as a miss (no gap).
3. Terms = the class slug verbatim, the `problem_class` body verbatim, the taxonomy bucket, the sig's
   16-hex short form, ≤3 message tokens of ≥4 chars (stopwords dropped), the top stack-frame symbol.
   Case-insensitive substring match; slug-verbatim match = **primary** hit, ≥2 other terms = **secondary**.
4. Parse primary hits then secondary hits, newest first. Accept an answer when `answers[].status` is
   `verified` or `ci_passed` **and** `signatures.result != "failed"` (mirrors the lab's own verified
   aggregation). Malformed JSON is counted in `payload.corpus_parse_errors` and skipped.
5. First accepted answer wins, with its path and mtime. None → `corpus_grep_hit:false` plus
   TROUBLE-RESEARCH-007 (a diagnostic, not a stop).

A corpus or discover hit may **resolve the incident outright and skip the agent**: the brief carries
`resolve_outright:true` when the answer is `verified`/`ci_passed`, every module it names exists in the
registry (SPEC-06), and the incident's rule scopes permit them. When `research.apply_brief_as_play` is
`true` (default `false`) and the answer maps to registry-executable tasks, the brief additionally carries
`play_draft` as an array of `PlayTask`-shaped maps (`name`, `tool`, `args`, `when`, `register`,
`retries`, `on_fail`) so the ladder's research short-circuit (SPEC-05 T22, guarded by
`ladder.research_play_extra`) can draft a play from the answer with no agent run. Research only sets the
flag and the draft; applying it is SPEC-05's transition, and a draft whose tools are outside the registry
or the rule's scopes is dropped from `play_draft` (the brief text still carries the answer).

### 3.3 Submit construction on a miss

```json
{"problem_class":"payment-worker-crash-loop",
 "description":"payment-worker restart loops; 14 restarts in 10m; last exit status 137 (OOM-killed by cgroup)",
 "cadence":"end-of-day",
 "context":{"fingerprint":"journald:sha256v1:2ab4c6d8e0f1a3b5","stack":"worker.py:118 claim\n  queue.py:44 get",
   "release":"payment-api@2.4.1","unit":"payment-worker.service","repro":["systemctl --user status payment-worker"],
   "evidence":{"events":14,"window_s":600,"value":41.7,"unit":"pct"},
   "origin":{"host_id":"7f3a91c2d4e5b607","hub_id":"","source":"journald:payment-worker"},
   "incident_id":"inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE","daemon_version":"0.1.0"}}
```

`description` is free text ≤8KB, scrubbed; when a discover hit exists but is unverified its solution text
is folded in (there is no answer-write endpoint — `POST /api/v1/answers` is 404 — so a submission carrying
the working fix is the only way to contribute one). `context` is always a JSON object (a bare string is a
400) and always scrubbed before egress (§4.5). `cadence` starts at `research.cadence` (default
`end-of-day`) and a 400 `invalid cadence` walks the next `cadence_ladder` value, ≤2 retries per submit
(probe cost ≤3 requests); the accepted value is pinned in memory and in `payload.cadence_accepted` so
subsequent submits for that class start there. The ladder encodes measured drift: `end-of-day` accepted
throughout, `pre-phase` rejected on 2026-08-06 and accepted again on 2026-08-15 — the enum is data, not a
constant. **Never** at top level: `fingerprint`, `stack`, `release`, `solution`, `answer`, `evidence`,
`signatures`, `title`, `repo`, `task_id`, `commit`, `required_tools`.

### 3.4 Wire codec (unexported) and the field-name mapping

The only deltas between trouble's struct tags and the lab's wire names: `DiscoverRequest.Env` →
`environment`, `DiscoverRequest.Lang` → `language`, `DiscoverRequest.Version` → `version`,
`include_related` is sent as `false`, `DiscoverResponse.Solution` ← `answer.solution`,
`SubmitResponse.SubmissionID` ← `submission_id`, and `QueueStatus.State` ← `status` + `stage` with
`QueueStatus.Solution` ← `answer`. `problem_class`, `description`, `cadence` and `context` are identical on
both sides. `SubmitResponse.Duplicate` is set by the codec from the HTTP status (409 → `true`), never
parsed from a body. `DiscoverResponse.CachedTS` is the lab's `answer.created_at` when present, else the
response time. `wireLabError` decodes `error`/`message` for 400/404/409/503; `wireHealth`/`wireStats`
decode `status`/`uptime` and the snake_case stats keys.

### 3.5 Queue polling

One poll loop per submission, owned by the research worker, cancelled with the rung context.

| Parameter | Default | Reason |
|---|---|---|
| `poll_interval` | `15s` | ~12 polls inside the measured ~3m mean solve, at ≈1KB per poll |
| `poll_jitter_pct` | `0.20` | deterministic per submission: `interval × (1 + (hash16(submission_id) mod 41 − 20)/100)`; de-synchronises simultaneous pollers |
| backoff | interval `×1.5` after 2m, capped at `60s` | long solves hold fewer requests |
| `poll_timeout` / `poll_max_requests` | `12m` / `48` | 4× the measured ~3m mean solve and ≈3.4× the lab's own `estimated_time` (`3m30s`); the budget, not the estimate, ends the loop |
| terminal | `solved` → brief · `failed`/404 → code 004 + one D1 · timeout → code 006 | §5 |

On `poll_timeout` the submission is **not** cancelled (no cancel endpoint is measured and none is
invented): the record keeps `submission_id`, the lab's cache grows anyway, and the next incident for that
class finds the answer on D1 — the cheap path. A parked rung keeps its `res_` id and resumes polling with
a fresh budget if the incident is still open.

### 3.6 Ledger records: kind `research` and kind `gap`

`internal/research` emits exactly two kinds (SPEC-INDEX §3.4). Payload schema for `kind: "research"`
(`schema_version: 1`), on the SPEC-01 envelope:

| Key | Type | Notes |
|---|---|---|
| `res_id` | string | `res_<ULID>`, allocated at rung entry so a degraded or skipped rung still has a join key |
| `state` / `driver` | string | `requested \| returned \| degraded \| skipped` · `off-by-one \| none \| webhook` |
| `slug`, `slug_fallback`, `source`, `app_kind`, `taxonomy` | string/bool | the `ClassSlug` fields, flattened |
| `idem_key` | string | §3.10 |
| `submission_id`, `duplicate` | string/bool | `""` until submit returns one; `duplicate` true on 409 |
| `cadence_attempted`, `cadence_accepted` | string | §3.3 |
| `http_status` / `error_code` | int / string | last lab status (0 when no request was made); `""` on the clean path |
| `degraded_reason` / `skip_reason` | string | the pinned value sets below |
| `corpus_grep_hit`, `corpus_path`, `corpus_parse_errors`, `corpus_timeout` | bool/string/int | §3.2 |
| `brief_digest`, `prompt_digest`, `brief`, `brief_truncated` | string/object/bool | the brief stored scrubbed, capped at `brief_max_bytes` |
| `poll_count`, `elapsed_ms`, `queue_depth_at_submit`, `unknown_status` | int | observed behaviour |
| `cost` | object | §3.8 |

Pinned reason values (the SPEC-TYPES §3.9 list, extended): a `skipped` state carries `skip_reason` and
never a `degraded_reason`. `degraded` → `lab_unreachable`, `solver_unavailable`, `strict_decoder_reject`,
`poll_timeout`. `skipped` → `driver_none`, `driver_capability_missing`, `class_unknown`, `brief_invalid`,
`budget_exhausted`, `kill_switch`.

A `GapRecord` is emitted for these five triggers and no others — a recorded absence of a source, which is
SPEC-05's verification input. `est_lost` is `-1` (unknowable) in all five: research degradation loses an
enrichment, never evidence — the incident, its events and its verification tuple are produced without the
lab.

For consumers: a brief is **open** for a sig while its newest `research` record has `state` in
`{requested, returned}`; `degraded` and `skipped` records are closed — which is the gate SPEC-09's
quiet-close sweep and SPEC-08's `ForemanBrief.ResearchBrief` read (that subset is
`{submission_id, slug, state, brief}` from this record).

| Trigger | `sensor` | `cause` |
|---|---|---|
| lab unreachable / non-JSON 200 / cooldown active | `research` | `research_lab_unreachable` |
| strict-decoder 400 (a code defect worth an alarm) | `research` | `research_strict_decoder_reject` |
| 503 solver unavailable | `research` | `research_solver_unavailable` |
| poll budget exhausted | `research` | `research_poll_timeout` |
| corpus root missing or unreadable | `research` | `research_corpus_unreadable` |

### 3.7 The agent prompt, with and without a brief

`BuildAgentPrompt(inc, out, evidence)` renders a deterministic five-section template;
`research.prompt_template` may override section text but never the order or the machine-readable header
(the digest is compared in tests and in the ledger).

1. `## Incident` — `inc` id, sig, severity, entry rung, current state, opened ts, reopen count.
2. `## Evidence` — source liveness expectations, event counts and PSI values from `bundle`, the scrubbed
   event message, the canonical stack.
3. `## Research brief` — **with a brief**: a fenced section holding the lab answer verbatim (scrubbed,
   capped at `prompt_brief_max_bytes` 16KB inside the prompt; the full brief stays in the ledger under
   `res_id`), preceded by `research_id: res_…  driver: off-by-one  state: returned  brief_digest: <hex16>
   corpus_grep_hit: <bool>  resolve_outright: <bool>  submission_id: sub_…`. **Without a brief** (degraded
   or skipped): exactly one line `research_id: res_…  driver: <driver>  state: <degraded|skipped>
   reason: <reason>` then `no research brief available — diagnose from the evidence above`. The section is
   never omitted, so "no brief" is always distinguishable from "rung never entered".
4. `## Tool contract` — registry-only typed tool calls, `check_mode` in shadow, the do-not-touch list, the
   one-fix-per-sig lease reference.
5. `## Output schema` — `{diagnosis, tool_calls[], issue_draft?, confidence}` (PRD §05 ③).

Pinned: the prompt is built only from the **scrubbed** brief (`Scrub` targets `event_msg` and `stack`;
SPEC-02's target set is closed in v0.1, so no new target is introduced here); the brief is **untrusted
data, never instructions** — fenced, never interpreted as tool calls, and a solution naming a module
outside the registry stays text; `prompt_digest = Digest(prompt)` and `brief_digest =
Digest(canonical_json(brief))` both land in the `research` record and in the `agent_run` record written by
SPEC-05, which is what makes "the prompt shows the brief" auditable (AC-20).

### 3.8 Cost and token accounting; budget interaction with SPEC-05

| Path | HTTP requests | Bytes in / out | Wall |
|---|---|---|---|
| discover hit (D1) / discover + narrowing miss (D1+D2) | 1 / 2 | ≈4 KB / 0.2 KB · ≈8 KB / 0.4 KB | <50ms / <100ms |
| corpus grep (D3) | 0 | 0 | ≤3s |
| miss → submit (with the cadence ladder) | +1…3 | 0.6 KB / 1 KB | ≤10s |
| poll to solve | ≤48 | ≤48 KB / ≤1 KB | ≤12m |
| health/stats probe | ≤2 | ≈1 KB / 0.1 KB | <50ms |
| **worst case per rung** | **≤54** | **≤62 KB / ≤40 KB** | **≤12m05s, off the ladder's critical path** |
| driver `none` | 0 | 0 | 0 |

Recorded per rung in `payload.cost`: `{requests, discover_requests, submit_requests, poll_requests,
probe_requests, bytes_in, bytes_out, wall_ms, cache_hits, submits, polls, corpus_greps, cache_hit,
tokens_saved_in, tokens_saved_out, tokens_estimated}`.

Research spends **zero agent tokens** — the lab's solve burns the lab's compute, never trouble's agent
budget. When a brief resolves the incident without an agent run, the rung records `tokens_saved_in` /
`tokens_saved_out` from `agent_tokens_saved_in` (25000) and `agent_tokens_saved_out` (8000) — the PRD's
agent budget — flagged `tokens_estimated:true`. Research never debits that budget: the
25k/8k/120s/one-at-a-time accounting is SPEC-05's, and a research hit only means it goes unspent.

Research has its own budget: `requests_per_day` (200, per host per UTC day) debited on **submit only** —
discover, corpus grep and probes are free. The counter is rebuilt at boot by summing today's `research`
records (`payload.submits`), so a restart cannot reset it (SPEC-01's bounded index). Exhaustion →
`state:"skipped"`, `skip_reason:"budget_exhausted"`, **no error code** (a budget decision is not a
failure), and the ladder makes its own agent-budget decision.

Two gates, one effective ceiling: the ladder's own `ladder.research_per_day` (30/day, SPEC-05 §3.7) gates
rung **entry** and is checked first, so this counter is the driver's inner safety ceiling and its default
(200) is deliberately above the ladder's — the binding limiter is therefore always the ladder, and the
driver never silently becomes a smaller cap than the policy gate. Both exhaustion paths end in
`research:skipped`; only this one writes `skip_reason:"budget_exhausted"`.

### 3.9 Provenance linkage (one join key per incident)

`res_` is allocated at rung entry, so the chain exists for degraded and skipped outcomes too:
`Incident.ResearchID` → `research` record (`Rec.inc`, `Rec.sig`, `payload.res_id`) → `agent_run` payload
`research_id` + `brief_digest` + `prompt_digest` (SPEC-05) → `SpawnRequest.ResearchBriefID` (SPEC-08) and
`IssueRef.ResearchID` (SPEC-09) and `SkillCandidate.ResearchID` (SPEC-11); `ResearchOutcome.ID` is the same
`res_` value everywhere. `internal/research` writes none of those foreign records: it writes its own
`research` record and hands the `res_` id and the two digests to the caller.

### 3.10 Idempotency

`idem_key = Digest([]byte(sig + "|" + slug + "|" + version))` (16 hex chars), stored in the record and in
the worker's `submitted` map, rebuilt at boot from the ledger.

1. At most one in-flight submission per `idem_key` per host; a second request for the same key while one
   is in flight attaches to the existing `submission_id` instead of submitting.
2. A re-submit is permitted only when the previous attempt returned no `submission_id`, or reached
   terminal `failed`, or the incident reopened. A reopen reuses the same `idem_key` deliberately: the
   lab's 409 is authoritative and is treated as queued.
3. A 409 body carrying a `submission_id` → adopt and poll it. A 409 without one → the answer is arriving
   in the cache, so the rung re-probes the **class** with D1 every `duplicate_recheck_interval` (3m)
   inside the same poll budget instead of polling an id it does not have.

## 4. Wiring

### 4.1 Connections

| Direction | Party | Contract |
|---|---|---|
| consumed by | `internal/ladder` (SPEC-05) | `Service.Run`, `Resume`, `Park`; consumes `ResearchOutcome` + the two digests |
| calls | `internal/ledger` (SPEC-01) · `internal/scrub` (SPEC-02) · `internal/types` | `Deps.Append` (`research` + `gap` only) · `Deps.Scrub` on every outbound body and inbound brief · `Sig`, `ClassSlug`, `DiscoverRequest`, `SubmitRequest`, `QueueStatus`, `ResearchOutcome` |
| read by | `internal/flow` (SPEC-08) · `internal/issues` (SPEC-09) · `internal/skills` (SPEC-11) · `internal/dashboard` (SPEC-10) | the `res_` id via `SpawnRequest.ResearchBriefID`, `IssueRef.ResearchID`, `SkillCandidate.ResearchID`, `ForemanBrief.ResearchBrief`; outcome values via `Service.Outcomes(sig)` (`Promoter.Draft` takes `[]ResearchOutcome`) |
| surfaced by | `internal/dashboard` (SPEC-10) | reads `research` records from the ledger (no new shared struct) |
| configured by | `internal/lifecycle` (SPEC-12) | the `[research]` config table + resolved-config explain |

### 4.2 Concurrency, restart, park

One research worker goroutine per incident, capped at `max_inflight` (4); the worker owns its poll loop and
its single `context`. It never blocks the ladder: `Run` returns as soon as the submission is accepted when
the ladder is still at the play rung, and the ladder reads the outcome at the agent rung. Cancellation:
incident terminal state, kill-switch, or `Close` (`Close` waits ≤2s then abandons pollers whose state is
already in the ledger). In-flight state is rebuildable from the ledger alone: boot replays today's
`research` records and rebuilds `submitted{}`, the daily counter, the learned cadence per class and the
cooldown/fence flags; a record with `state:"requested"` is resumed by `Resume` (fresh poll budget on its
recorded `submission_id`, or a re-submit under §3.10.2 when it has none). SIGTERM and kill-switch: `Park`
writes the current state, cancels the poller and writes nothing further; at the kill-switch checkpoint the
state is written as `skipped` / `skip_reason:"kill_switch"`, and the `res_` id survives so the link chain
stays unbroken.

### 4.3 Config keys (every one defaulted; no fleet value in any default)

`research.enabled=true` · `driver="off-by-one"` · `lab_url="http://127.0.0.1:8766"` ·
`lab_data_dir="./data"` · `corpus_roots=["answers"]` · `corpus_glob="*.json"` · `corpus_max_files=5000` ·
`corpus_max_bytes_per_file=1048576` · `corpus_timeout="3s"` · `connect_timeout="2s"` ·
`request_timeout="5s"` · `submit_timeout="10s"` · `poll_interval="15s"` · `poll_jitter_pct=0.20` ·
`poll_timeout="12m"` · `poll_max_requests=48` · `duplicate_recheck_interval="3m"` · `queue_depth_skip=25` ·
`max_inflight=4` · `cooldown_failures=3` · `cooldown="5m"` · `health_probe_interval="60s"` ·
`capability_probe_interval="1h"` · `submit_fuse_400s=3` · `brief_max_bytes=65536` ·
`prompt_brief_max_bytes=16384` · `prompt_max_bytes=32768` · `requests_per_day=200` ·
`cadence="end-of-day"` · `cadence_ladder=["end-of-day","post-debug","pre-phase"]` ·
`fallback_slug="unknown"` · `allow_unknown_class_submit=false` · `apply_brief_as_play=false` · `table="research-classes.toml"` ·
`unit_kind={}` · `container_markers=["docker","podman","containerd"]` · `agent_tokens_saved_in=25000` ·
`agent_tokens_saved_out=8000` · `webhook_url=""` · `webhook_poll_url=""`.

### 4.4 Egress scrubbing (symmetric with SPEC-02)

Outbound `SubmitRequest.context.stack` and the description pass `Deps.Scrub` with target `stack`, and the
free-text fields with `event_msg` (the SPEC-02 consumer table's row for `internal/research`): the lab never
receives a value the ledger would refuse to store. Inbound briefs are scrubbed with the same two targets
before they reach the ledger or the prompt, and `Record.redactions` counts both directions.

## 5. Errors

| Code | Class | Trigger | State / reason | Gap | Rung effect |
|---|---|---|---|---|---|
| TROUBLE-RESEARCH-001 | transient | connect/read timeout, non-JSON 200, cooldown active | `degraded` / `lab_unreachable` | yes | returns immediately; ② and ③ never wait |
| TROUBLE-RESEARCH-002 | permanent | 400 strict decoder (unknown field, or bad cadence past the ladder) | `degraded` / `strict_decoder_reject` | yes | rung returns; the submit fuse (§5.2) protects the lab |
| TROUBLE-RESEARCH-003 | permanent | 409 duplicate submission | `requested` (not a failure) | no | treated as queued (§3.10.3) |
| TROUBLE-RESEARCH-004 | transient | 503 solver unavailable, or a `failed`/404 queue state | `degraded` / `solver_unavailable` | yes | rung returns; discover + corpus stay usable |
| TROUBLE-RESEARCH-005 | permanent | table, rules and composition all missed → fallback slug | `skipped` / `class_unknown` | no | corpus grep still runs; submit only under `allow_unknown_class_submit=true` |
| TROUBLE-RESEARCH-006 | transient | poll budget exhausted (`poll_timeout` or `poll_max_requests`) | `degraded` / `poll_timeout` | yes | the submission stays queued; D1 on the next incident for that class finds the answer |
| TROUBLE-RESEARCH-007 | permanent | corpus grep found no cached answer | the miss path | no | proceeds to submit |
| TROUBLE-RESEARCH-008 | permanent | `driver = "none"` | `skipped` / `driver_none` | no | zero HTTP; the ladder proceeds |
| TROUBLE-RESEARCH-009 | permanent | returned brief failed local validation (not an object, no solution text, unusable after truncation) | `skipped` / `brief_invalid` | no | the agent runs without a brief (§3.7) |
| TROUBLE-RESEARCH-010 | permanent | discover/submit response carried no `submission_id` | `degraded` / `solver_unavailable` | yes | nothing to poll; D1 retried at the duplicate recheck interval |

Every code above appears in `payload.error_code` of the `research` record it describes (SPEC-INDEX §5 rule
3). The ladder's own view of a degraded research rung is its own record and code (SPEC-05).

### 5.1 Timeouts and the ladder path

`connect_timeout` 2s, `request_timeout` 5s (discover/queue/stats), `submit_timeout` 10s, `corpus_timeout`
3s, `poll_timeout` 12m. Worst-case research wall time is 12m05s on the worker goroutine only: the rung's
contract with the ladder is "return an outcome or return `degraded`, and never hold a transition". Every
timeout produces a record; the five conditions in §3.6 additionally produce a `GapRecord`.

### 5.2 Failure fence and cooldown

Three consecutive transport failures (001) → cooldown for `cooldown` (5m): no requests are made, every
`Run` returns `degraded` / `lab_unreachable` immediately, and one `/health` probe every
`health_probe_interval` (60s) closes the cooldown early. Three consecutive strict-decoder rejects (002) →
the **submit fuse** opens for the rest of the process lifetime (`Snapshot().fence = "submit_disabled"`):
discover and corpus grep continue, no further submit is attempted, and the condition is visible in the
ledger and on the dashboard. Both are local state, not `Breaker` records — `breaker` is emitted only by
SPEC-03 and SPEC-05 (SPEC-INDEX §3.4).

## 6. Edge cases

1. **`found:false` with a corpus hit** — the measured trap (a verified answer whose signatures carry empty
   env/lang/version). Covered by the mandatory D3; `corpus_grep_hit` and the corpus path are recorded so
   the next reader sees which probe found it.
2. **Two slugs for one bug** — the corpus key is the exact slug. On a D3 miss the grep widens to mechanism
   terms (error string, source, taxonomy); a second guess-slug is never submitted automatically.
3. **Fallback slug** — `Fallback=true`, `state:"skipped"`, code 005, corpus grep still runs. Submitting
   under `unknown` would pollute a shared cache, so `allow_unknown_class_submit` defaults to `false` and
   requires an explicit operator decision.
4. **Lab reachable but answering HTML** — the catch-all serves the SPA with HTTP 200 for unknown paths
   (measured on `GET /discover`). Any 200 whose `Content-Type` is not JSON becomes code 001 with
   `note:"non_json_200"`; trouble never decodes a body it did not expect. A 404 on `/health` itself means a
   misconfigured `lab_url` (code 001, `note:"health_404"`) — health is at the root, `/api/v1/health` is 404.
5. **Cadence enum drift** — the ladder value set is walked in order, the accepted value is pinned per
   class, and the enum is never assumed stable (it changed twice in the measured record).
6. **Queue id lost by a lab restart** — 404 → code 004 with `note:"queue_not_found"`, then one D1.
7. **Answer larger than `brief_max_bytes`** — truncated with `payload.brief_truncated:true`; the prompt
   says so and tells the agent to re-derive missing detail with read-only tool calls (SPEC-06).
8. **Answer statuses other than verified** — `pending`/`failed` answers are not briefs; only
   `verified`/`ci_passed` with `signatures.result != "failed"` are accepted, on both the discover and the
   corpus paths; anything else falls through to submit.
9. **A brief naming tools outside the registry** — retained as text only. No path leads from a brief to a
   tool call that skipped the registry's authorize → validate → dry-run → apply → verify → audit chain
   (SPEC-06). This is also the prompt-injection rule for untrusted text (§3.7).
10. **Unit-name shapes** — `.service`/`.timer`/`.socket`/`.scope` stripped, `name@1234` → `name@`, dbus
    `_2d` unescaped, container ids normalised to the container name (a 64-hex id never enters a slug),
    paths reduced to basenames, everything `kebab()`-ed.
11. **Concurrent incidents of one class** — one in-flight submission per `idem_key`; followers attach to
    the same `submission_id` and each write their own `research` record.
12. **Clock** — poll budgets, cooldowns and the jitter anchor use the monotonic clock; every persisted
    timestamp is RFC3339 UTC with millisecond precision, always `Z` (SPEC-INDEX §6.5).
13. **Restart mid-poll and deep lab queue** — `Resume` re-adopts; re-submission is idempotent and the 409
    path is exercised by every restart by design. When `queue_depth` ≥ `queue_depth_skip` (default 25) the
    submit still happens (the answer is cached for the future), the poll budget does not extend, and
    `payload.queue_depth_at_submit` records why a timeout was likely.

## 7. Testing

| File | Cases and thresholds |
|---|---|
| `internal/research/driver_offbyone_test.go` | `TestStrictDecoderFingerprintRejected` — the live regression: an `httptest` lab mirroring the strict decoder returns 400 `{"error":"invalid_request","message":"invalid JSON: json: unknown field \"fingerprint\""}` for a body carrying a top-level `fingerprint`; assert trouble's codec can never produce that body, that a recorded 400 maps to TROUBLE-RESEARCH-002 + `degraded` + `strict_decoder_reject`, and that exactly one `research` record **and** one `gap` record are written. `TestGoldenWireBodies` — the four request bodies plus recorded response fixtures (health `uptime 3h47m6s`; stats `1716/1900/1872/0/true`; submit `{"submission_id":"sub_87ee13","status":"queued","position":7,"estimated_time":"3m30s","existing_solutions":20}`; discover `{"found":true,"answer":{"id":1210,…}}`; 404 `not_found`; 409; 503 `solver_unavailable`) byte-for-byte. `TestNonJSON200IsProtocolError`. |
| `internal/research/slug_test.go` | Table-driven, ≥20 vectors across every `SigSource`, unit names with dashes/`@`/suffixes, container ids, missing subject, missing taxonomy, the 64-char truncation boundary, and the §3.1 worked example. `TestDeriveNeverBlocks` — 10k synthetic sigs: every call returns a non-empty slug, no panic, ≤1µs/call, and a stub ladder advances every rung within 1ms. The fallback asserts `Fallback=true` + code 005 + `state:"skipped"`. |
| `internal/research/corpus_test.go` | `TestFoundFalseCorpusHit` — `found:false` plus a corpus file with `status:"verified"` → `returned` + `corpus_grep_hit:true`; `signatures.result:"failed"` and `status:"pending"` rejected; malformed JSON counted and skipped; a missing root → `gap` cause `research_corpus_unreadable`; the 3s cap honoured (fake clock) with `corpus_timeout:true`; ≤5000 files scanned per grep. |
| `internal/research/poll_test.go` | Fake clock: intervals inside 15s ±20% and deterministic per `submission_id`; backoff `×1.5` capped at 60s; `poll_max_requests=48`; the 12m timeout → TROUBLE-RESEARCH-006 + `degraded` + `gap` cause `research_poll_timeout`; a never-answering lab still lets a stub ladder advance within `poll_timeout + 2s` (the AC-20 "never blocks" assertion). |
| `internal/research/prompt_test.go` | Golden prompts **with** a brief (brief text present, `brief_digest` matching, `resolve_outright` surfaced) and **without** (exactly the one-line header plus the "no research brief available" sentence); the brief never escapes its fence; a brief naming an unregistered module yields text only; `prompt_digest == Digest(prompt)` on both fixtures; prompt ≤32KB. |
| `internal/research/degrade_matrix_test.go` | The 7-row matrix (driver `none`, unreachable, 400, 409, 503, poll timeout, brief invalid): each row asserts state, reason, `error_code`, gap presence and that the ladder proceeds. Plus: 0 HTTP requests with driver `none`; ≤54 requests and ≤62KB in / ≤40KB out for the worst-case rung; the daily counter reads 200 after 200 submits across a simulated restart; the submit fuse opens after 3 consecutive 400s while discover keeps working. |
| `internal/research/adapter_test.go` | `Request`/`Poll` driven through a stub ladder adapter: a supplied `sub["slug"]` is used verbatim (0 derivation calls) and an absent one derives; `Outcomes(sig)` returns records newest-first after a simulated boot replay; with `apply_brief_as_play=true` and a registry-valid answer the brief carries `play_draft` and `resolve_outright:true`, and with a module outside the registry the draft is dropped while the brief text survives. |
| `internal/research/ac_test.go` | `TestAC17_UnknownClassForwardsAndAgentConsumesBrief` — an unknown class with `off-by-one` enabled produces one submit, one brief, and an agent prompt whose digest is recorded. `TestAC20_LinksAndUnreachableDegrade` — the `res_` id appears on the incident, the `research` record, the `agent_run` payload and the spawn/issue/skill stub records; with the lab forced unreachable the ladder advances while exactly one `gap` record (`research_lab_unreachable`) lands in the ledger. |

Gates: ≤54 requests and ≤12m05s per rung; 0 HTTP requests when the driver is `none`; ≥1 `gap` record for
each of the five gap conditions and none on the clean path; the package's tests run without network access
(`httptest` only) and without the real lab.

## 8. hilo impact

- **New package** `internal/research` (greenfield): `service.go`, `slug.go`, `table.go`, `taxonomy.go`,
  `driver_offbyone.go`, `driver_none.go`, `driver_webhook.go`, `corpus.go`, `poll.go`, `prompt.go`,
  `cost.go`, `wire.go`, the six test files in §7, and `examples/research-classes.example.toml`.
- **Fan-out (imports)**: `internal/types` (shared types), `internal/ledger` (append only, via `Deps`),
  `internal/scrub` (via `Deps`), stdlib (`net/http`, `encoding/json`, `crypto/sha256`, `context`, `time`).
  Four edges, all one-way.
- **Fan-in (imported by)**: `internal/ladder` (SPEC-05) is the only importer of the `Service` API.
  `internal/flow`, `internal/issues` and `internal/skills` read the `res_` id from the ledger instead of
  importing this package, keeping the dependency direction one-way (`internal/types` at the base,
  subsystems above it, nothing importing back).
- **Blast radius**: the greenfield repository `~/trouble` — no fleet repository is touched, and
  the lab itself is never modified: trouble is a pure client, with `lab_url`, `lab_data_dir` and
  `corpus_roots` as config, so nothing about a particular lab is compiled in.
- **hilo-relevant invariants**: SPEC-10's dashboard reads the `research` and `gap` payload schemas, and the
  `res_` id is a join key across four specs, so a change to `ResearchOutcome.ID` semantics is
  blast-radius-maximal for provenance and is a versioned schema change, not an edit.
