# PRD v2.4 amendments — trouble

Status: **binding**, issued 2026-09-16. This file amends `docs/prd-v2.3.html`; it is not a replacement
document and no new HTML is produced (per the spec brief, §1.S). Where a passage below contradicts v2.3,
**this file wins**, and the corresponding SPEC-NN encodes the amended rule.

Source: three independent quorum reviews (`docs/trouble-judge-{sol,k3,glm53}.txt`, all **BUILD WITH
CHANGES**) folded with the first-hand dispute resolution of their one measured conflict (PSI privilege).
Every amendment is a decision, not an open question.

## 1. Changes at a glance

| # | v2.3 says | v2.4 says | Lands in |
|---|---|---|---|
| 1 | PSI triggers are the rule mechanism; "epoll, 0% idle" | Triggers are an **accelerator only**; sampling of `avg10/60/300` is the source of truth. NUL-terminated writes; unprivileged windows multiples of 2 s (≥2 s); every wake re-reads counters; unarmed fds never enter an epoll set (347,378 hits/200 ms measured); one goroutine + own epoll per trigger + self-pipe; PSI fd is not Go-netpoller-compatible; EBUSY ≠ EINVAL; runtime probe of grammar/limits at startup; kernel floor 5.15 (UAF fix); POLLERR = source gone | SPEC-03 §3.1–§3.3 |
| 2 | "append-only JSONL" (schema, durability, retention unspecified) | One record schema, ULID ids, single-writer-per-ledger (hub only), **group-commit fsync with a ≤200 ms stated crash-loss window** (measured 515k rec/s amortized vs 512 rec/s per-line), daily rotation, retention config, compaction = new-generation rewrite (never in-place) with tombstone counts, torn-last-line recovery, bounded in-memory index rebuilt at boot, no full-scan-per-render | SPEC-01 |
| 3 | "secrets never in the ledger (DSN public keys excepted)" — a promise with no mechanism | **New subsystem:** ingest → scrub → ledger. Named mandatory rules (env assigns, bearer/token shapes, private keys, DSN secret parts, connection strings, per-project redaction), a redaction counter per event (never the value), a persistence-boundary re-scan, and the hard rule that the only unredacted credential in the system is a DSN public key | SPEC-02 |
| 4 | Verification = "sig quiet + group flat + probes green" | Verification is an **evidence tuple**, never a boolean: `{window bounds, events observed, canary seen, counter deltas, sources expected/alive/quiet/missing}`. A per-project canary goes through the real ingestion path every N minutes; if it did not land, verification is **invalid** (not passed). Multi-host verification names which sources must be quiet. Gap records on every dropout | SPEC-05 §3.5, SPEC-04 §3.8 |
| 5 | `POST /api/{project}/event/`; store and envelope equal weight; unknown envelope items rejected | Envelope is primary (`POST /api/{project_id}/envelope/`), store is legacy compat, `GET /api/{project_id}/` is the DSN probe; three auth forms recorded per project; unknown item types = **200-and-drop with a counter** (never reject the envelope); `client_report` is parsed; 429 + `Retry-After` + `X-Sentry-Rate-Limits` (also on 200s) | SPEC-04 §2, §3.5 |
| 6 | "it floods only itself"; quota is a detail | A quota breach **destroys evidence** (SDKs MUST discard on 429). The loss policy is a config decision: `sample | drop-with-counter | spool-if-light`, each counted and visible | SPEC-04 §3.7 |
| 7 | journald watching is implied | Supervised `journalctl -f -o json --after-cursor=<c>` subprocess with backoff and a persisted cursor; a malformed cursor is a hard failure → `--since` + rescan + gap record (never "no logs"); dedupe by `__CURSOR` equality; bounded queue with explicit drop accounting; unit-scoped follows by default; install handles `adm`/`systemd-journal`; `sdjournal` cgo variant reserved, not shipped | SPEC-03 §3.5 |
| 8 | D-Bus: watch the system manager | Watch BOTH the system manager and each configured **user** manager (this class of host runs its units per-user; system-only is a measured ALL-GREEN lie). No "unit failed" signal exists → `PropertiesChanged(SubState)` + `JobRemoved` + a startup `ListUnits` reconcile; re-subscribe + resync on `NameOwnerChanged`; path escaping (`-` → `_2d`); three arrival paths merge through the dedup core; a shipped polkit `.rules` artifact and a distinct **POLICY-REFUSED** class when it is missing; masked `systemd-oomd` = capability probe + documented no-op | SPEC-03 §3.6, SPEC-06 §4 |
| 9 | ingestion `:8643`, dashboard `:8644`; `MemoryMax=64MB` + `OOMScoreAdjust=-500` | Ports move to **ingestion 7643 / dashboard 7644** (8642/8643/8644 are occupied on the reference host) with a startup bind preflight that fails loud. Memory becomes **`MemoryHigh=192M` (soft) + `MemoryMax=256M` (hard)**; `OOMScoreAdjust=-500` is kept for host-contention only (a cgroup limit is enforced by the cgroup OOM killer regardless of the score). RSS budget: binary 8–15 MB measured, steady ≤ 80 MB, load ≤ 192 MB. Host memory pressure is itself an incident the daemon may file about itself | SPEC-12 §3.1, SPEC-03 §3.8 |
| 10 | hot-fix "kicks a worktree foreman immediately" (mechanism unspecified) | trouble NEVER hand-runs `git worktree add`. Spawn = the router/scheduler admission path with a priority class, budget accounting and a one-fix-per-sig lease; the worktree is created by the component that owns the repo, using the adopted fleet convention `<repo>/.worktrees/<task-id>` (never a fourth convention), never under `/tmp`; per-repo mutex, max-concurrent + min-free-disk gate, exempt list for huge checkouts, prune on boot, no `fetch/gc/prune` from a foreman worktree; spawn failure = durable queue + bounded retry + a `spawn_pending` record (spawn fails exactly when the fleet is sick) | SPEC-08 §3.6–§3.9 |
| 11 | research rung = "fingerprint + stack → brief (hypotheses, patch sketch)" | **Rewrite against the live lab.** Off-by-One is a `problem_class`-keyed verified-answer cache with a strict decoder: `POST /api/v1/problems/discover` first; on a miss `POST /api/v1/problems/submit {problem_class, description, cadence, context{}}` **only**; duplicate → 409 (queued); solver unavailable → 503 → research-degraded; results by **polling** `/api/v1/queue/{submission_id}` (~3 m), no webhooks; corpus-grep fallback because `found:false` ≠ absent. The finger print + stack live in `context`, and the class-slug derivation table is the real work. Live regression: `unknown field "fingerprint"` → 400 | SPEC-07 |
| 12 | shadow (default) = "everything runs … even hot-fix foremen, but PRs stay unmerged" — not safe as written | **Redefinition:** shadow = full detection + research + play drafting, but every mutating tool call runs `check_mode` only (diff returned, nothing applied); read-only tools execute; the hot-fix lane drafts the board row and does not spawn; PRs stay unmerged; promotions and skill accepts wait. assisted = per-rule execute grants. full = merge-by-policy | SPEC-05 §3.7 |
| 13 | `SKILL.toml` carries live `[stats]` and ships through git (also: the printed example is not valid TOML) | **Artifact fix:** `SKILL.toml` is immutable in the repo — `{name, version(int, monotonic), sigs[], play_ref, guards, provenance, min_daemon_version, allowed_modules[], signature}`; `[stats]` moves to the LOCAL ledger. Distribution is signature-verified (ed25519, signer key list in config) + canary host first + per-host approve policy + `min_daemon_version` gate; a pulled skill may make **typed tool calls only** inside its `allowed_modules` allowlist; two skills matching one sig → highest version wins and the conflict is recorded; the skills repo is a RELEASE CHANNEL (protected branch, review, tags), not a live-write share | SPEC-11 |
| 14 | module semantics described as a 6-stage sequence | **Frozen interface (v1):** `Descriptor()` + `Check` + `Apply` + `Verify`; error taxonomy `{transient, permanent, policy_refused}`; diff = `{path, before, after}`; descriptor = Go struct + generated JSON schema; conformance harness `testkit` (double-apply idempotency, check-vs-apply equivalence, schema-violation rejection) that every shipped module must pass and a deliberately non-idempotent test module fails in CI | SPEC-06 §2, §7 |
| 15 | ladder described in prose | Pinned state machine: `detected → recorded → play{drafted|check_only|applied|failed} → research{requested|returned|degraded|skipped} → agent{running|done|failed|suspended} → verifying → resolved | escalated | suppressed | quarantined`; entry rung per rule; play `max_runs=2` then next rung; agent two-strikes per sig per 24 h; global agent lease per host; verification window per rule (default 10 m); recurrence after resolve reopens the SAME incident (sig-keyed); in-flight play on restart = park + resume, never a re-execute of an applied mutating call; SIGTERM = flush + park | SPEC-05 §3.1–§3.4 |
| 16 | §11 "storm mode, OOM immunity, rename-over upgrades — carried unchanged"; no self-watchdog | Named lifecycle: `Type=notify` + `sd_notify` READY + `WatchdogSec=60` + `Restart=always` + `StartLimitIntervalSec=0` + `OnFailure=trouble-escalate@%n` (escalates through a path that does not depend on trouble); heartbeat every 30 s; an **external** checker that alarms on LEDGER-SEQUENCE STALL (not process liveness); `/health.json` with version/git-sha/build-time/uptime/`ledger_last_seq`/per-sensor ages; ldflags version stamping in every binary and every record's actor; config precedence `flag > env > file > default` with per-value provenance (`trouble config explain`); state root `~/.local/state/trouble` (0700, never `/tmp`); dashboard token never in a URL; secrets in a 0600 EnvironmentFile, never argv | SPEC-12 |
| 17 | T1..T5 named in prose only | One protocol, one identity model, one decision per transition — table in §4 below | SPEC-12 §3.7, SPEC-INDEX §3.3 |
| 18 | dashboard "views" | Pinned v0.1 route set, scopes, CSRF, partial polling at 2 s, htmx + `go:embed`, no external CDN, token scopes read/write/autonomy, identity seam (token impl now, tailnet/proxy later) | SPEC-10 |
| 19 | 27 ACs across four products | **v0.1 cut line frozen**, with three ACs formally downgraded (see §5) | SPEC-INDEX §3.3, §6.1 |

## 2. Port and memory fixes (measured on the reference host)

- `:8642` (default gateway), `:8643` (uncensored-profile gateway), `:8644`, `:9090` (scheduler), `:3000`
  (memory backend) are **occupied**. trouble's defaults are therefore `:7643` (ingestion) and `:7644`
  (dashboard), both config-driven, with a bind preflight at startup that refuses a taken port
  (`TROUBLE-LIFECYCLE-003`) rather than silently binding an alternative.
- `MemoryMax=64MB` is below a measured comparable daemon (35.9 MB RSS idle, 22.4 MB binary) and leaves no
  room for gzip buffers, the group index and a flood backlog. v2.4: `MemoryHigh=192M` (throttle),
  `MemoryMax=256M` (kill), `OOMScoreAdjust=-500` for host-contention only, plus admission control on the
  ingestion path and a disk spool. Host memory pressure is a first-class incident.
- Budget the binary at 8–15 MB (measured build variants: 7.99 MB stdlib+`html/template`+embed, 8.36 MB
  with godbus + `x/sys`, 12.05 MB adding pure-Go SQLite) and steady RSS ≤ 80 MB, load ≤ 192 MB.

## 3. New subsystem: scrubbing (safety, before any persistence)

The v2.3 ledger is append-only, git-distributable, dashboard-rendered and auto-filed to issue trackers —
so an unredacted secret is unauditable after the fact. v2.4 introduces SPEC-02, positioned **between
ingestion and the ledger** and applied to every derived artifact (bundles: journal tails, SDK payloads,
config snapshots; and the skills, issue and board rows derived from them):

1. a compiled, ordered rule set with mandatory rules that no config, flag or environment can disable;
2. a per-event redaction count (counts only, never values) carried on the ledger record;
3. a persistence-boundary re-scan that refuses a record which still matches a mandatory pattern;
4. the single-credential rule: the only unredacted credential anywhere in the system is a DSN public key;
5. redaction conformance tests (positive and negative vectors) as a release gate.

## 4. The five topologies — one decision per transition

| Topology | Shape | The one decision made now | Config |
|---|---|---|---|
| T1 | one box, hub with **zero** satellites (identical code paths) | single-writer ledger + state root `~/.local/state/trouble`, own unit with its own restart/watchdog policy (never a child of another service's cgroup) | `topology = "T1"` |
| → T2 | one cloud VM, public service + edge apps | **bind-address-scoped auth**: loopback = DSN public key; non-loopback = per-project token or a proxy; public = reverse proxy mandated. Stable advertised DSN hostname, TLS or proxy. Per-project read scoping before any multi-tenant exposure | `sentinel.bind`, `sentinel.advertised_host`, `sentinel.auth_mode` |
| → T3 | 2–4 boxes, hub + light satellites | **one forwarding protocol = the sentinel wire format** (+ protocol version + idempotency key), hub mints canonical ids, satellites keep a local↔hub mapping, hub is the only ledger writer | `forward.*`, `spool.*` |
| → T4 | home lab over a tailnet, phone dashboard | **queue bounds + token session model**: bounded spool with drop-oldest + ledger note; dashboard auth as Bearer/cookie with an identity seam (token now, tailnet/proxy-header provider later); least-scope credentials per device | `spool.budget_bytes`, `dashboard.identity` |
| → T5 | many machines, regional hubs, customer-edge proxies, fleet foremen | **origin fields in every record from record #1** (`origin{host_id, hub_id, source}`) + a proxy key class distinct from project DSN keys; skills repo as a release channel (protected branch, signed artifacts, canary, per-host pin) | `origin.*`, `forward.proxy_key_class` |

The decision that buys all of T1→T5 at once: treat every deployment as "hub + N satellites", version the
ingest and skill paths from record #1, and give every record a globally stable identity plus an origin —
then every topology transition is configuration, not surgery.

## 5. Acceptance-criteria reconciliation

Three shipped ACs reference material the v0.1 cut line defers. They are recorded rather than quietly
dropped (`SPEC-INDEX §6.1`):

- **AC-24** (skill distribution) → **partial**: v0.1 ships the local candidate → review → promote loop,
  signed artifacts, provenance and pull-from-git. Candidate-PR push automation is v1.0.
- **AC-25** (light-mode offload) → **partial**: v0.1 ships the hub/satellite split in configuration, the
  forward path, the spool and skill pull-down. The separate light binary is v1.0.
- **AC-27** (proxy relay) → **deferred**: v0.1 specifies the forward/spool/replay mechanism the proxy
  would use, and ships no proxy binary.
- **AC-19** (2 s live incident) → resolved in SPEC-10: 2 s polling of server-rendered partials; SSE is
  named as the reserved v1.0 route and is not specified as in-scope.

Additionally, `prd-v2.3.html` states only AC-18..AC-27 in full; AC-1..AC-9 (v1) and AC-10..AC-17 (v2.1)
are referenced as "carry" without their text. The suite maps them by scope key derived from PRD §02 and
§10 and marks them `[carried]`; if the v1/v2.1 documents are recovered, the mapping must be diffed against
the real text.

## 6. v2.3 passages effectively superseded

| v2.3 location | Superseded by |
|---|---|
| §04a PSI trigger rules | amendment 1 (sampling is the source of truth; trigger caveats normative) |
| §04b endpoint list and quota parenthetical | amendments 5, 6 |
| §04c dashboard ports, "views" | amendments 18, 9 |
| §05 research rung (forward/return shape) and open question #1 | amendment 11 |
| §06 module semantics | amendment 14 |
| §06c `SKILL.toml` example incl. `[stats]`, push/pull distribution | amendment 13 |
| §08 hot-fix spawn description | amendment 10 |
| §10 MVP bullets for light mode, proxy, skill push | §5 above (cut line) |
| §11 guardrails: OOM line, "carried unchanged" upgrade/sandbox line | amendments 9, 16 |
| §11 acceptance criteria AC-19/24/25/27 | §5 above |
| §12 autonomy paragraph (shadow = everything runs) | amendment 12 |

Unchanged and still binding from v2.3: open-source-first posture (§09), MIT, the four-outlet loop, the
one-signature-space dedup core, managed hands (registry tool calls only — never a shell), and the
timing-authority exception being the hot-fix lane alone.
