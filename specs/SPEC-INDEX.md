# SPEC-INDEX — suite map, AC matrix and the frozen v0.1 cut line (trouble v0.1)

Spec: SPEC-INDEX
Area prefix: (none — this file allocates error-code AREAS, it does not define codes; codes live in SPEC-TYPES §5)
Package: (none — repository meta-document)
Consumed types: ErrorCode, Record, RecordKind, Prefix, Sig (ownership map only)
Local types: — 
ACs: AC-1..AC-33 (matrix owner; every AC appears exactly once in the matrix, with its spec mapping)
PRD: §10, §11, §12

## 1. Purpose

This file is the binding index for the trouble spec suite. It answers three questions only:

1. **Which spec owns which acceptance criterion** (§3.2) — the AC-to-spec matrix is the contract; a spec
   that changes AC coverage must change this file in the same commit.
2. **What is in v0.1 and what is not** (§3.3) — the cut line is frozen from the quorum verdict and is
   normative, not advisory.
3. **Which area owns which error range, record kinds, packages and shared types** (§3.4–§3.6) — so thirteen
   specs written in parallel cannot collide.

## 2. Interface

Every spec file (SPEC-01…SPEC-13, SPEC-TYPES) MUST open with this metadata block immediately after the H1,
verbatim in shape, so the consistency loop (§7) can parse it mechanically:

```
Spec: SPEC-NN
Area prefix: TROUBLE-<AREA>
Package: internal/<area>
Consumed types: <comma-separated type names, all present in SPEC-TYPES.md>
Local types: <comma-separated package-private type names, or —>
ACs: <comma-separated AC list>
PRD: <comma-separated PRD section ids>
```

Every spec then carries exactly these eight numbered-level-2 sections, in this order:

1. Purpose · 2. Interface · 3. Data model · 4. Wiring · 5. Errors · 6. Edge cases · 7. Testing ·
8. hilo impact

A section with no error codes writes `5. Errors` → `None. This area emits no error codes of its own.`
That is legal. `TBD`, `Phase 2`, `later`, `consider` and `TODO` are forbidden strings in every spec file
(§7 step 6).

`Status` values used in §3.1: `frozen` (v0.1 binding), `partial` (v0.1 subset specified; remainder in the
deferred table), `deferred` (specified at interface level only; see §3.3).

## 3. Data model

### 3.1 Suite map

| Spec | Title | PRD sections | ACs covered | Status |
|---|---|---|---|---|
| SPEC-INDEX | suite map, AC matrix, cut line | §10, §11, §12 | all (index) | frozen |
| SPEC-TYPES | shared types + error catalog | §03, §06, §06b, §06c, §07, §08, §11 | all (type substrate) | frozen |
| SPEC-01 | ledger: record schema, sig format, ids, durability | §03, §04a, §06b, §11 | AC-6, AC-22, AC-24, AC-26, AC-30 | frozen |
| SPEC-02 | scrubbing subsystem (safety, pre-persistence) | §11 (guardrails), §04b | AC-18, AC-22 | frozen |
| SPEC-03 | sensors: PSI, journald, D-Bus, disk, timers, inotify, rules | §04a, §11 | AC-1, AC-2, AC-4, AC-19 | frozen |
| SPEC-04 | sentinel: ingestion contract, grouping, releases, collectors, sensor routes | §04b, §10, §11 | AC-10..AC-15, AC-18, AC-19, AC-22, AC-28, AC-31 | frozen |
| SPEC-05 | ladder: state machine, verification, autonomy gates | §05, §06b, §11, §12 | AC-1, AC-2, AC-3, AC-4, AC-5, AC-11, AC-20, AC-21, AC-22, AC-26, AC-31 | frozen |
| SPEC-06 | registry: module SDK v1, plays, do-not-touch, polkit | §06, §11 | AC-7, AC-23 | frozen |
| SPEC-07 | research: Off-by-One contract, class-slug derivation | §05, §12 | AC-3, AC-17, AC-20, AC-31 | frozen |
| SPEC-08 | flow: board-jsonl row, task-router, hot-fix spawn | §08, §11, §12 | AC-9, AC-19, AC-21, AC-22, AC-26, AC-32, AC-33 | frozen |
| SPEC-09 | issues: driver contract, github + duckbrain | §07, §11 | AC-8, AC-22, AC-31 | frozen |
| SPEC-10 | dashboard: routes, auth, scopes, CSRF, live updates, paging | §04c, §11 | AC-16, AC-19, AC-26, AC-30 | frozen |
| SPEC-11 | skills: artifact schema, local promote loop, pull distribution | §06c, §12 | AC-24 (partial), AC-25, AC-26 | partial |
| SPEC-12 | lifecycle: unit, watchdog, upgrades, config, topology, server profiles | §09, §11, §12 | AC-14, AC-18, AC-25 (partial), AC-26, AC-27, AC-28, AC-29, AC-33 | partial |
| SPEC-13 | server profiles: standalone | light-hub (Redis buffer/dedup + DuckBrain archival) | §03, §11, §12 | AC-29 | frozen |

### 3.2 AC matrix (binding)

Legend: **B** = built in v0.1 · **P** = partial (v0.1 subset; the AC cannot fully pass until the deferred
item ships — see §6.1) · **D** = deferred to v1.0 (AC text retained; v0.1 claims no coverage).

| AC | Text (verbatim where prd-v2.3.html states it) | Spec(s) | Status |
|---|---|---|---|
| AC-1 | `[carried]` event triggers fire and open an incident inside the rule's window | SPEC-03, SPEC-05 | B |
| AC-2 | `[carried]` stabilization (`for=`) suppresses flapping; recurrence after resolve reopens | SPEC-03, SPEC-05 | B |
| AC-3 | `[carried]` ladder walks record → play → (research) → agent → outlets per the rule's entry rung | SPEC-05, SPEC-07 | B |
| AC-4 | `[carried]` storm breakers + per-rule caps stop a flappy source spraying work | SPEC-03, SPEC-05 | B |
| AC-5 | `[carried]` budgets cap agent runs per host/day; exhaustion escalates instead of running | SPEC-05 | B |
| AC-6 | `[carried]` append-only ledger with monotonic seq; every step auditable | SPEC-01 | B |
| AC-7 | `[carried]` registry tool call runs authorize → validate → dry-run → apply → verify → audit | SPEC-06 | B |
| AC-8 | `[carried]` issue desk files/links/closes an issue through a driver contract | SPEC-09 | B |
| AC-9 | `[carried]` flow files a board row (direct write) or dispatches via task-router | SPEC-08 | B |
| AC-10 | `[carried]` sentinel ingests a real Sentry SDK event (envelope + legacy store, DSN auth) | SPEC-04 | B |
| AC-11 | `[carried]` sentinel groups by fingerprint; group counters and rate are correct | SPEC-04, SPEC-05 | B |
| AC-12 | `[carried]` release-aware grouping detects a regression across releases | SPEC-04 | B |
| AC-13 | `[carried]` quotas return 429 + Retry-After + X-Sentry-Rate-Limits in the official format | SPEC-04 | B |
| AC-14 | `[carried]` remote ingestion over LAN/Tailscale with DSN auth | SPEC-04, SPEC-12 | B |
| AC-15 | `[carried]` generic JSON endpoint + collector parsers cover SDK-less apps | SPEC-04 | B |
| AC-16 | `[carried]` dashboard renders incidents, groups, issues, breakers | SPEC-10 | B |
| AC-17 | `[carried]` research lane forwards an unknown class and the agent consumes the brief | SPEC-07 | B |
| AC-18 | AC-18 Remote + language matrix: services on two different hosts — one Go (Sentry SDK), one bash (curl JSON) — report the same bug class; both land in their project groups on the trouble host over Tailscale; dashboard shows both; quotas hold when one floods. | SPEC-04, SPEC-12, SPEC-02 | B |
| AC-19 | AC-19 Dashboard: from a phone browser, token-authed: live incident appears within 2s of trigger; sentinel release-diff view shows the regression; hot-fix timeline shows filed→foreman→verify→promote with PR link; all read-only actions work without write grants. | SPEC-10, SPEC-03, SPEC-04, SPEC-08 | B |
| AC-20 | AC-20 Research lane: unknown fingerprint + off-by-one enabled → brief forwarded, agent run consumes the returned brief (prompt shows it), incident ledger links brief↔agent↔fix; off-by-one forced-unreachable → ladder proceeds unchanged, gap noted in ledger. | SPEC-07, SPEC-05 | B |
| AC-21 | AC-21 Hot-fix immediacy: enabled host + scripted bad line in an allowed repo → direct board row + worktree foreman spawned within 60s of trigger; patch lands in the worktree only (main checkout untouched); verify window passes → promotion prompt; recurrence → rollback + reopen. | SPEC-08, SPEC-05 | B |
| AC-22 | AC-22 Dedup totality: same underlying bug detected via sensor AND sentinel AND collector paths → one incident, one group, one issue, one board row (comments on recurrence), one dashboard story; zero duplicates anywhere. | SPEC-01, SPEC-02 (dedup survives scrubbing), SPEC-04, SPEC-05, SPEC-08, SPEC-09 | B |
| AC-23 | AC-23 Module conformance: every shipped module passes the conformance suite — idempotency (double-apply = ok/ok, one change), check_mode returns the true diff without applying, schema violations rejected at validate; a deliberately non-idempotent test module fails CI. | SPEC-06 | B |
| AC-24 | AC-24 Skill distribution: host A's agent fix → candidate PR → review-merged → hosts B and C pull → synthetic recurrence on B runs as ② at zero tokens with provenance intact (inc_8812 visible in B's ledger). | SPEC-11 (local candidate/review/promote/pull), SPEC-01 (provenance) | P |
| AC-25 | AC-25 Light-mode offload: N100-class host runs light agent → incident forwarded up to hub → hub researches/fixes → updated skill pulls down → next local occurrence handled locally, offline-tolerant (queued forward). | SPEC-12 (forward + spool + pull-down), SPEC-11 | P |
| AC-26 | AC-26 Zero-human loop: autonomy=full host + scripted service bug: detection → research → play → hot-fix foreman → PR merged by policy → verify window → promote → skill candidate auto-accepted (threshold met) — ledger + dashboard show every step, zero human actions; kill-switch stops the loop within one stage. | SPEC-01 (ledger shows every step), SPEC-05 (gates), SPEC-08 (merge/promote), SPEC-10 (visibility), SPEC-11 (auto-accept), SPEC-12 (kill-switch persistence) | B |
| AC-27 | AC-27 Proxy relay: app → proxy (customer edge) → hub: grouping, quotas, and DSN auth intact through the relay; proxy offline → local spool → flush on reconnect, no loss. | SPEC-12 (forward/spool mechanism only) | D |
| AC-28 | AC-28 Sensor route matrix: with a hub endpoint configured, an event class routes to B (proxied) by default and to A (direct) when `routes.per_class` overrides it; with no hub endpoint every class takes A; a hub outage mid-run sends Route B traffic to the bounded spool and every spooled event replays exactly once after the hub returns; each landed record's `origin.route` equals the route that carried it and the dashboard shows the local-vs-relayed split. | SPEC-04 (route decision, config, spool replay), SPEC-12 (forward path, ack, zone) | B |
| AC-29 | AC-29 Light-hub degradation: `[server] profile="light-hub"` with Redis + a DuckBrain namespace — duplicate `ForwardEnvelope` idempotency keys replayed across a Redis failover land exactly one ledger record; stopping Redis mid-burst makes senders see 429 + `Retry-After` while the local spool holds and the daemon keeps serving (no event loss, hub degrades to standalone ingestion); a closed ledger generation is exported to DuckBrain and only then dropped from the hot host, and with DuckBrain unreachable archival pauses with a gap record while ingestion continues; the ladder produces identical incident/verify sequences in both profiles for the same event stream. | SPEC-13 (profile, queue, dedup gate, archival), SPEC-12 (config, health, topology) | B |
| AC-30 | AC-30 Ledger pagination: a 10M-record corpus is walked with `page_token` + `page_size` — page-size stability holds (no record repeated, none skipped across the walk, `next_page_token` empty exactly at the end), the retention sweep deletes whole generation files (+ `.idx` + archive marker) while a walk is in flight, and a token pointing at a dropped generation returns a `reset` hint + newest-first restart instead of an error loop; a missing or mismatched `{file}.idx` is rebuilt from the generation file. | SPEC-01 (tokens, sidecar, drop-generation retention), SPEC-10 (`/incidents`, `/groups` paging) | B |
| AC-31 | AC-31 Cross-plane context: a sentinel `Admit` populates `Observation.Codeplane` (sig, group, release, regression, top-5 recent) and the bundle lands on the incident record; a sensor `Admit` whose convergence map links an open sentinel group arrives at the agent rung with BOTH planes' context; the research request and the issue body carry the bundle verbatim; a bundle whose release disagrees with the running release is discarded with a gap record, never shown as fact. | SPEC-05 §3.13a, SPEC-04 §3.9a, SPEC-07 §3.10a, SPEC-09 §3.13a | B |
| AC-32 | AC-32 Flow-owned dispatch durability: a hot-fix dispatch that cannot be delivered is EITHER durably queued on the flow's own store — `<state_root>/spool/flow/spawn/<ev_ULID>.json`, one JSON entry per file, mode `0600` under a `0700` directory, drained by `Flow.Run` — and replayed through the §3.6 attempt sequence with the SAME `idem_key`, at most one in-flight dispatch per `idem_key` and a `409`/duplicate answer accepted as success, so the entry is deleted and no second spawn is created for work the first attempt already placed, and the entry survives a restart to be listed and replayed by a second store instance over the same root; OR refused as `dispatch_state="unspooled"` or `"spool_failed"` with a `reason` and a `coupling` field naming `SPEC-08 §3.6/§3.9 × SPEC-09 §3.6/§3.7` — a sink or store the subsystem cannot itself replay never yields `dispatch_state="spooled"`, and with the issue desk OFF (the shipped posture) the composition root still hands the flow a queue it can replay, while a store that cannot be built leaves the flow reporting no queue instead of claiming durability. | SPEC-08 | B |
| AC-33 | AC-33 Flow queue bounds, resolved and recorded: every bound on the §3.9a dispatch queue is the `flow.*` registry key in force on BOTH halves — the store and the loop that drains it — so a host that declares no `[flow]` table runs the compiled defaults (256 entries, 72h, 5 attempts, 5s, 100 per drain), while `flow.spool_max_entries=3` and `flow.spool_ttl=1h` are the bounds actually in force and a key that is not strictly positive (zero, negative, `0s`, unparsable) is refused at resolution with TROUBLE-LIFECYCLE-001 naming the key; every entry a bound costs is recorded rather than silent — at the entry bound the OLDEST entry is evicted and never the incoming one with one `drop_reason:"overflow"` record per eviction, and an entry past its TTL, past `max_attempts` or undecodable drops with `drop_reason` `"ttl"`/`"attempts"`/`"corrupt"` as a `flow` + `spawn` record pair naming incident, sig and task_id, with no `gap` record for any of them. | SPEC-08, SPEC-12 §3.1d | B |

`[carried]` ACs: see §6.2 — prd-v2.3.html references AC-1..AC-17 as "carry" but does not restate their
text; the scope keys above are derived from PRD §02 (directive map) and §10 (MVP bullets) and MUST be
re-checked if a v2.4 HTML is ever rendered. The mapping is what v0.1 builds against.

### 3.3 The frozen v0.1 cut line (quorum R — normative)

**IN v0.1:** sensors (PSI sampling + triggers, journald follow, D-Bus both managers, disk, timers) +
sentinel (envelope + store + generic JSON + collector parsers go-panic/py-traceback/node-reject) +
ledger + dedup core + registry (the modules in SPEC-06 §6) + ladder with the research rung (off-by-one
driver per SPEC-07) + issue desk (github + duckbrain) + flow (board-jsonl direct + task-router; hot-fix
per SPEC-08) + dashboard (SPEC-10) + skills local loop (candidate → review → promote; distribution =
pull from a git repo) + shadow/assisted/full + kill-switch + lifecycle (SPEC-12) + dual sensor transport
routes with `origin.route` (SPEC-04 §3.10a) + ledger page tokens and the per-generation `.idx` sidecar
(SPEC-01 §2.3a/§3.7a) + server profiles, with `light-hub` (Redis ingestion buffer + dedup gate, DuckBrain
archival tier) shipping alongside `standalone` (SPEC-13).

**OUT of v0.1 (no spec section may describe these as in-scope):**

| Deferred item | Appears only as | Re-enters at |
|---|---|---|
| plays LIBRARY beyond the shipped modules' defaults | SPEC-06 §6 default plays | v1.0 |
| module SDK as a PUBLIC extension surface (interface frozen at v1; third-party packaging deferred) | SPEC-06 §2 note | v1.0 |
| light-mode offload binary | SPEC-12 §3.7 forward path only | v1.0 |
| sentinel proxy binary | SPEC-12 §3.7 spool/replay only | v1.0 |
| skill PUSH (PR-creating) automation — pull-only in v0.1 | SPEC-11 §3.4 (candidate stays local) | v1.0 |
| ansible-bridge | not specified | v1.0 |
| OTel receiver | not specified | v1.0 |
| SSE | SPEC-10 §2 (2s polling chosen; SSE reserved as a named future route) | v1.0 |
| Sentry sessions/tracing/replays/cron-monitors/profiles | SPEC-04 §3.5 drop-with-counter | v1.0 |
| jira/linear/gitlab issue drivers | SPEC-09 §3.2 (interface only) | v1.0 |
| eBPF probes, security mode, multi-host console | not specified | v1.0 |

**Rule (normative):** *no AC may reference a deferred item as its passing condition.* Where a shipped AC
does reference one (AC-24, AC-25, AC-27), the AC is downgraded to `P` (partial) or `D` (deferred) in
§3.2 and the reconciliation is recorded in §6.1. Deferred material appears ONLY in the table above and in
SPEC-INDEX §6.1 — never inside a numbered spec section.

### 3.4 Record-kind ownership

| Kind | Emitted by (only) |
|---|---|
| event, canary | SPEC-03, SPEC-04 |
| group | SPEC-04 |
| `incident`, `verify`, `breaker`, `agent_run` | SPEC-05 |
| play_run, tool_call | SPEC-06 |
| research | SPEC-07 |
| flow, spawn | SPEC-08 |
| issue | SPEC-09 |
| skill | SPEC-11 |
| gap | SPEC-03, SPEC-04, SPEC-07, SPEC-09, SPEC-12, SPEC-13 |
| config, lifecycle | SPEC-12, SPEC-13 (profile boot gate, degradation and archival operations) |
| (all kinds) | SPEC-01 — the ledger is the only writer |

### 3.5 Error-area allocation (disjoint by construction)

| Area token | Spec | Range used |
|---|---|---|
| TROUBLE-LEDGER-0NN | SPEC-01 | 001–012 |
| TROUBLE-SCRUB-0NN | SPEC-02 | 001–008 |
| TROUBLE-SENSORS-0NN | SPEC-03 | 001–025 |
| TROUBLE-SENTINEL-0NN | SPEC-04 | 001–023 |
| TROUBLE-LADDER-0NN | SPEC-05 | 001–021 |
| TROUBLE-REGISTRY-0NN | SPEC-06 | 001–018 |
| TROUBLE-RESEARCH-0NN | SPEC-07 | 001–010 |
| TROUBLE-FLOW-0NN | SPEC-08 | 001–019 |
| TROUBLE-ISSUES-0NN | SPEC-09 | 001–010 |
| TROUBLE-DASHBOARD-0NN | SPEC-10 | 001–013 |
| TROUBLE-SKILLS-0NN | SPEC-11 | 001–014 |
| TROUBLE-LIFECYCLE-0NN | SPEC-12 | 001–017 |
| TROUBLE-HUB-0NN | SPEC-13 | 001–016 |

The authoritative catalog with meanings and classes is SPEC-TYPES §5. A spec MAY add a new code inside
its own range only if the code is also added to SPEC-TYPES §5 in the same commit. Because the ranges are
disjoint, two specs can never mint the same code.

### 3.6 Package / type ownership

| Package | Spec | Shared types it may use (from SPEC-TYPES) |
|---|---|---|
| internal/ledger | SPEC-01 | Record, Origin, Actor, Sig, Prefix, RecordKind |
| internal/scrub | SPEC-02 | ScrubRule, ScrubResult |
| internal/sensors | SPEC-03 | SensorEvent, SensorHealth, Rule, Condition, Breaker, GapRecord, Event* |
| internal/sentinel | SPEC-04 | Project, SentryEvent, ClientReport, RateLimitDecision, Group, LossPolicy, Evidence |
| internal/ladder | SPEC-05 | Incident, LadderState, Rung, Evidence, GapRecord, AutonomyGates, VerifyResultKind, Breaker, Severity |
| internal/registry | SPEC-06 | Module, Descriptor, Diff, DiffEntry, Result, VerifyResult, RollbackHint, ToolCall, CallStage, Play, PlayTask, DoNotTouch, IdempotencyClass, ErrorClass |
| internal/research | SPEC-07 | ClassSlug, DiscoverRequest/Response, SubmitRequest/Response, QueueStatus, ResearchOutcome |
| internal/flow | SPEC-08 | BoardRow, SpawnRequest, HotfixLease, Promotion, FlowConfig, HotfixConfig |
| internal/issues | SPEC-09 | IssueRef, IssueDriver, EnsureBySigRequest/Response, DriverHealth, SpoolEntry |
| internal/dashboard | SPEC-10 | HealthResponse, SourceLiveness, RuntimeWatermarks, Token, Scope, AutonomyGates |
| internal/skills | SPEC-11 | Skill, SkillGuards, Provenance, SkillStats, SkillCandidate, Play |
| internal/lifecycle | SPEC-12 | ConfigValue, Heartbeat, ForwardEnvelope, SpoolEntry, Topology, TopologyDecision, Duration |
| internal/hub | SPEC-13 | ProfileConfig, HubStatus, RedisStreamOffsets, LedgerArchiveMarker, ForwardEnvelope, SpoolEntry, Record, Origin, GapRecord, RouteDecision |
| internal/types | SPEC-TYPES | (defines all of the above) |

## 4. Wiring

### 4.1 Spec dependency order (build sequence)

```
SPEC-TYPES ─┬─► SPEC-01 (ledger) ─┬─► SPEC-02 (scrub) ─► SPEC-04 (sentinel) ─┬─► SPEC-05 (ladder)
            │                     │                                          │
            │                     ├─► SPEC-03 (sensors) ─────────────────────┤
            │                     │                                          ├─► SPEC-06 (registry) ─► SPEC-11 (skills)
            │                     ├─► SPEC-07 (research) ─────────────────────┤
            │                     ├─► SPEC-08 (flow)   ◄── SPEC-06 ───────────┤
            │                     ├─► SPEC-09 (issues) ◄── SPEC-05 ───────────┘
            │                     ├─► SPEC-12 (lifecycle) ◄── SPEC-01..11 (config + version + state root) ─► SPEC-13 (hub: profile, queue, archival)
            └─► SPEC-10 (dashboard) reads everything, writes only ack/close/autonomy
```
Implementation order for v0.1: TYPES → LEDGER → SCRUB → SENSORS → SENTINEL → LADDER → REGISTRY →
RESEARCH → FLOW → ISSUES → DASHBOARD → SKILLS → LIFECYCLE. v0.1.1 adds one node after LIFECYCLE:
**HUB** (SPEC-13) — the profile, the Redis queue/consumer and the archival tier — because it consumes the
resolved config, the ledger writer and the spool, and nothing consumes it in reverse.

### 4.2 Cross-reference resolution rules

- A spec section that names another spec MUST use the file stem (`SPEC-04`) and, where it depends on a
  specific behaviour, a section reference (`SPEC-04 §3.5`).
- A spec section that names a PRD section MUST use the PRD's own ids (`§04b`, `§06c`, `§11`).
- Shared types are referenced by bare type name; the reader resolves them in SPEC-TYPES §3.
- The condition/expression language is defined once (SPEC-03 §3.4) and reused verbatim by SPEC-06
  (`when:` in plays) and SPEC-05 (rule gates). No second dialect.

## 5. Errors

The scheme is `TROUBLE-<AREA>-<NNN>` with the AREA tokens allocated in §3.5 and the catalog in SPEC-TYPES
§5. This file additionally records the four cross-area rules:

1. **Uniqueness** — a code belongs to exactly one area; areas are disjoint, so codes are globally unique.
2. **Class** — every code carries one class: `transient | permanent | policy_refused`
   (`policy_refused` is reserved for TROUBLE-SENSORS-012 and TROUBLE-REGISTRY-006, the two
   POLICY-REFUSED surfaces — polkit for `service.*` and the do-not-touch/capability refusals).
3. **Ledger mirror** — any code a subsystem returns to the ladder MUST also appear in the ledger record
   that describes the failure (`Record.payload.error_code`), so the audit trail is complete without
   reading logs.
4. **No ad-hoc codes** — a subsystem that needs a new condition adds the code to SPEC-TYPES §5 first.

## 6. Edge cases

### 6.1 Cut-line vs AC reconciliation (the honest table)

| AC | Conflict | Resolution |
|---|---|---|
| AC-24 | "candidate PR → review-merged" needs skill PUSH, which the cut line defers (pull-only in v0.1) | Status `P`: v0.1 ships local candidate → review → promote → pull-from-git. The candidate artifact and provenance chain are complete and signed; only the PR-creation automation is deferred. |
| AC-25 | "light agent" binary is deferred | Status `P`: v0.1 ships the hub/satellite split in configuration (same code paths, T1 = hub with zero satellites), the forward path (SPEC-12 §3.7), the spool, and skill pull-down. The separate light binary is v1.0. |
| AC-27 | "proxy" binary is deferred | Status `D`: v0.1 specifies the forward/spool/replay mechanism the proxy would use (SPEC-12 §3.7) but ships no proxy binary. No v0.1 spec section claims proxy coverage. |
| AC-19 | "live incident appears within 2s" needs a live-update choice; SSE is deferred | Resolved in SPEC-10 §2: 2s polling of htmx partials is the v0.1 mechanism; SSE is named as the reserved v1.0 route but is not specified as in-scope. |
| AC-28, AC-29, AC-30, AC-31 (added in v0.1.1) | none — no deferred item is a passing condition here: dual sensor routes, the light-hub profile, ledger page tokens and the cross-plane codeplane bundle are all inside the §3.3 cut line. The deferred light-mode offload binary and sentinel proxy binary are *different* artifacts from the `light-hub` server profile (SPEC-13 §1: a profile selects plumbing inside the one daemon; the deferred binaries are separate processes) | Status `B` for all four. AC-31 carries the same reasoning as AC-28..AC-30: the bundle is a field on records the shipped `Admit`, research and issue paths already carry (`Observation.Codeplane` → `Incident.Codeplane` → the research request payload and the issue body), it adds no deferred process and no deferred artifact, and refusing a mismatched bundle is a `gap` record inside the shipped sentinel. The hand-off note in SPEC-12 §3.7 and SPEC-11 §3.4 is unchanged, and no section of this round describes a separate light or proxy process. |
| AC-32, AC-33 (allocated for SPEC-08 §3.9a) | none — the flow-owned dispatch queue, the honest refusal, the replay loop and the five bounds are shipped behaviour (`internal/flow/spool.go` + `replay.go`, `internal/app` wiring), all inside the §3.3 cut line, and no deferred item is a passing condition | Status `B` for both: the queue the subsystem replays, the refusal that names the coupling, the same-`idem_key` replay and the resolved `flow.*` bounds with their recorded drops are existing behaviour with named enforcing tests (SPEC-08 §7 names them; `spool_test.go` added the three branches that had none — `spool_failed`, single-flight per `idem_key`, `409` accepted). SPEC-12 §3.1d owns the key registry the same AC resolves against. |

### 6.2 AC-1..AC-17 text absence

prd-v2.3.html states only AC-18..AC-27 in full; it asserts AC-1..AC-9 (v1) and AC-10..AC-17 (v2.1)
"carry" without restating their text. §3.2 therefore maps them by **scope key** derived from PRD §02 and
§10, and marks each `[carried]`. This is a documentation gap in the PRD (not in the suite): if the v1/v2.1
PRD HTMLs are recovered, the scope keys must be diffed against the real AC text and this table corrected
in a follow-up commit. Until then the suite is built against the scope keys above.

### 6.3 Module path rename

If the publish org/name differs from `github.com/totalwindupflightsystems/trouble`, the migration is a
single mechanical pass: `go.mod` module line plus the import prefixes in `internal/**` and `cmd/**`. No
spec pins the import path in a wire format, a ledger record, or a schema — the module path appears in Go
source only. This is deliberate.

### 6.4 Single-tenant per host, always

Even in T5, **one daemon process owns one state root**. Two daemons sharing a state root is a
configuration error rejected at boot (TROUBLE-LIFECYCLE-004/005) because it breaks the single-writer
invariant (TROUBLE-LEDGER-005). Multi-host scale is achieved with satellites forwarding to one hub, never
by co-located daemons.

### 6.5 Clock policy

Every comparison that must not be fooled by wall-clock jumps (verify windows, cooldowns, leases,
stabilization) uses the monotonic clock (`time.Since`); every persisted timestamp and every cross-host
comparison is RFC3339 UTC from the wall clock. Clock skew beyond tolerance across hosts emits
TROUBLE-LIFECYCLE-017 and marks verification `invalid` for the affected zone.

## 7. Testing — the mandatory self-consistency loop

Run BEFORE the commit; every step must pass with a recorded command and output.

1. **Types resolve** — for every spec (SPEC-01..SPEC-13), parse the `Consumed types:` line; every name must appear as a Go
   type definition in SPEC-TYPES.md §3. No orphans. No type defined twice with different shapes
   (enforced by "defined exactly once in SPEC-TYPES.md").
2. **AC coverage both ways** — every AC in §3.2 maps to ≥1 spec section; every spec section is either
   covered by an AC or explicitly tagged `design constraint` in its heading text. The AC universe checked
   here is derived from the matrix itself: adding `AC-nn` to §3.2 needs no edit to the tool, and a hole in
   the sequence (a missing `AC-nn` below the highest row) is a hard failure.
2b. **Matrix → metadata** — for every AC row, every spec named in that row's spec column MUST declare that
   AC in its own `ACs:` metadata line, failing as
   `AC-31: matrix assigns SPEC-09 but SPEC-09 does not declare it`. This is the direction that let the
   AC-31 row name four owning specs while none of them declared it and the loop still printed PASS.
3. **Interfaces compile by inspection** — every Go signature in a spec's §2 matches the types and method
   sets in SPEC-TYPES; every HTTP route in a spec's §2 appears in that area's route table exactly once.
4. **Error codes** — every `TROUBLE-<AREA>-<NNN>` in every spec exists in SPEC-TYPES §5, is unique, and
   its class matches; no code outside the §3.5 range of the spec that uses it.
5. **Cross-references resolve** — every `SPEC-NN` reference names an existing file; every `§nnX` PRD
   reference names a section that exists in prd-v2.3.html (ids: `§03 §04a §04b §04c §05 §06 §06b §06c
   §07 §08 §09 §10 §11 §12`).
6. **Cut line respected** — no forbidden string (`TBD`, `Phase 2`, `TODO`, `consider`, lowercase
   `later`) in any spec; no deferred item from §3.3 described as in-scope inside a numbered spec section
   (deferred items may appear only as a v1.0 hand-off note inside SPEC-12 §3.7's forward path and SPEC-11
   §3.4's candidate path).
7. **Report** — files written, byte sizes, section counts, type count, AC coverage table (see the commit
   report emitted with this suite).

Mechanical commands used by the loop (recorded in the commit report):

```
python3 specs/tools/selfcheck.py            # steps 1, 4, 5, 6 and section/type counts
git grep -nE 'TBD|Phase 2|TODO|consider' -- specs/   # step 6 (manual review of hits)
```

## 8. hilo impact

- This file creates no package. Its blast radius is process-level: it is the file that must change
  whenever a spec's AC coverage changes.
- Every spec file is a new node under `~/trouble/specs/`; `specs/tools/selfcheck.py` is the
  only executable artifact in the suite (stdlib-only Python, no deps) and is the CI entry point for the
  consistency loop. Since v0.1.1 it also validates `SPEC-13` (file set 01..13) and the AC range —
  `AC-1..AC-33` as of the AC-32/AC-33 allocation for SPEC-08 §3.9a, derived from §3.2 rather than
  hardcoded — plus the matrix→metadata declaration check of §7 step 2b, which is the mechanical half of
  keeping this index honest.
- No fleet repository is modified: trouble is greenfield at `~/trouble`.
