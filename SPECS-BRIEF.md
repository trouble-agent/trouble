# SPECS-BRIEF — trouble v0.1 (the vertical slice)

You are the spec writer. Read this brief fully, then `~/trouble/docs/prd-v2.3.html`
(background) and the three quorum reviews in `~/trouble/docs/trouble-judge-*.txt`
(they are AUTHORITATIVE — their measured findings are law). Write the SPEC suite exactly as
specified below. Do NOT modify any src/ (none exists yet). Commit specs + PRD amendments.

## 0. Product non-negotiables (the maintainer's orders — do not revisit)
1. Open-source-first: zero fleet hardcoding; every path/port/board/repo is config; fleet
   material lives in examples/ only; MIT.
2. Sentinel = embedded Sentry-class error-tracking service (Sentry-SDK-compatible ingestion,
   fingerprint grouping, releases/regressions) + local collector for SDK-less apps.
3. Managed hands: the agent's ONLY capabilities are registry tool calls (schema'd, dry-run,
   verify, audit) — never a shell.
4. Issue desk: driver architecture — github + duckbrain ship; 4-method driver contract.
5. Flow control: direct board write + task-router drivers; review_mode setting; hot-fix
   mode with worktree foreman spawn (via the scheduler's admission path), verify window,
   promotion.
6. Sensors (PSI/inotify/journald/D-Bus/disk/timers) + sentinel feed ONE ladder:
   record → playbook(play) → research(off-by-one) → agent(budgeted) → outlets.
7. Dedup core: one signature space across sensors/sentinel/issues/board.
8. Dashboard UI: embedded, html/template + vendored htmx, token-authed, read-mostly.
9. Skill loop: incidents → candidate skills → review → skills repo; pull down for next time.
10. Ansible-grade module semantics: idempotency, check_mode, plays-as-data.
11. Autonomy: shadow (default) | assisted | full + kill-switch.
12. Language-agnostic, works T1 laptop → T5 multi-cloud via ONE hub+satellite shape.

## 1. QUORUM AMENDMENTS — already decided, encode them (do not re-argue)
These are the merged verdicts of sol/k3/glm-5.3 (3× BUILD WITH CHANGES) + Kara's
first-hand dispute resolution. Every one is binding:

A. **PSI (measured 3×, dispute resolved first-hand):** trigger writes MUST be
   NUL-terminated; unprivileged windows must be multiples of 2s (≥2s); sampling of
   avg10/60/300 is the SOURCE OF TRUTH for rules (triggers cannot express avg10 — they
   are an accelerator only); every wake must re-read counters (wake ≠ threshold crossed;
   rate-limited 1/window); NEVER put an unarmed PSI fd in an epoll set (always-ready
   spin: 347k hits/200ms measured); one dedicated goroutine + own epoll per trigger +
   eventfd/self-pipe shutdown; PSI fd is NOT Go-netpoller-compatible (no SetDeadline);
   treat EBUSY (already armed) and EINVAL (bad grammar) as distinct errors; runtime probe
   grammar/limits at startup; kernel floor 5.15+ (UAF fix); POLLERR = source gone.
B. **Ledger & durability (measured):** JSONL audit ledger + bounded in-memory index
   rebuilt at boot (no full-scan-per-render); group-commit fsync (batched: 515k rec/s
   amortized) vs per-line (512 rec/s) — choose group-commit with window ≤ 200ms and
   STATE the crash-loss window in docs; single-writer-per-ledger (hub only, ever);
   every record: {seq, ts RFC3339 UTC, kind, schema_version, sig, origin{host_id, hub_id,
   source}, payload, actor}; seq monotonic per file, writer is the only allocator;
   torn-last-line recovery (tolerant read, fleet jq-law); rotation daily + retention
   config + compaction = new-generation-file rewrite (never in-place), tombstone counts
   kept, payload TTL; unit tests with the measured numbers as regressions.
C. **Scrubbing subsystem (SPEC-02, SAFETY, before any persistence):** pipeline is
   ingest → scrub → ledger; named rules: env-var assignments, bearer/token shapes,
   private keys, DSN secret parts, connection strings, per-project redaction config;
   "what was redacted" counter per event (never the value); the ONLY unredacted
   credential in the system is a DSN public key; scrubber applies to bundles (journal
   tails, SDK payloads, config snapshots) and to skills/issues/board rows derived from
   them; redaction tests are conformance tests.
D. **Verification = positive evidence, not absence:** per-source liveness expectation +
   last-event-age tracking; gap records on every collector/journal/PSI dropout; canary
   event per project through the real ingestion path every N minutes — verification is
   INVALID (not passed) if the canary didn't land; ingest client_report envelope items
   (SDK self-reported drops are visible); verification result = evidence tuple
   {events_observed, canary_seen, counter_deltas, window_bounds, sources_expected/alive},
   never a boolean; multi-host verify states WHICH sources must be quiet.
E. **Sentinel ingestion (measured 6,199 req/s group-commit):** routes = POST
   /api/{project_id}/envelope/ (primary) + POST /api/{project_id}/store/ (legacy compat)
   + GET /api/{project_id}/ (DSN probe); success = 200 {"id": event_id}; errors = 400 +
   X-Sentry-Error; auth = X-Sentry-Auth header | ?sentry_key | envelope-header dsn
   (record which form each project uses); whole-envelope Content-Encoding gzip; 200KB
   compressed / 1MB decompressed caps + gzip-bomb guard; unknown envelope item types =
   200-and-drop with counter (SDKs send mixed items; client_report is parsed, not dropped);
   429 + Retry-After + X-Sentry-Rate-Limits (also on 200s for proactive backoff); quota
   units = events/min per project + global disk budget; LOSS POLICY is a config decision:
   {sample | drop-with-counter | spool-if-light} — document that quota breach destroys
   evidence (SDKs MUST discard on 429); grouping = SDK fingerprint override wins, else
   canonical stack hash; DSN grammar pinned to Sentry's
   {scheme}://{pubkey}[:{secret}]@{host}:{port}/{project_id} — path ends at base (SDKs
   append /api/...), host must be an ADVERTISED STABLE NAME (never an IP/bind addr —
   init generates the DSN from a configured public hostname).
F. **journald:** subprocess `journalctl -f -o json --after-cursor=<cursor>` supervised
   child (restart w/ backoff + persisted cursor); malformed cursor → fallback
   --since=<last-ts> + rescan + gap record (never treat seek-failure as "no logs");
   dedupe by __CURSOR equality (cursors are not orderable); bounded queue + explicit
   drop/gap accounting; unit-scoped follows by default (--output-fields, per-entry size
   cap, non-UTF8 handling); install handles adm/systemd-journal membership (effective
   after restart); sdjournal cgo variant reserved behind build tag — do NOT ship in v0.1.
G. **D-Bus:** watch BOTH system manager AND configured user managers (XDG_RUNTIME_DIR/bus)
   — this fleet's units are user units; system-only = ALL-GREEN lie (measured). No
   "unit failed" signal exists: PropertiesChanged(SubState) + JobRemoved + STARTUP
   ListUnits reconcile (failed units before boot are invisible otherwise); re-Subscribe +
   resync on NameOwnerChanged (daemon-reexec); D-Bus down = explicit sensor-degraded
   ledger record; unit object path escaping (− → _2d); THREE arrival paths for one
   incident (PropertiesChanged + JobRemoved + journald) merge via dedup core — spec the
   merge rule; **polkit: service.* actions are gated (manage-units = auth_admin on this
   box) — SPEC an install-time polkit policy file (.rules) as a shipped artifact, and the
   tool call fails with a distinct POLICY-REFUSED error class when missing**;
   systemd-oomd is masked here → capability probe + documented no-op.
H. **Ports & memory (measured collisions):** defaults move: ingestion :7643, dashboard
   :7644 (8642/8643/8644 are fleet-occupied; config-driven anyway) + startup bind
   preflight that FAILS LOUD on collision; memory: MemoryHigh=192M (soft) +
   MemoryMax=256M (hard) — the old MemoryMax=64M + OOMScoreAdjust=-500 pairing is a
   contradiction (cgroup OOM ignores the score); keep OOMScoreAdjust=-500 for
   host-contention only; RSS budget: binary 8-15MB (measured 7.99-12.05MB), steady RSS
   ≤ 80MB, load ≤ 192MB (compare: schedulerd 35.9MB RSS); memory pressure on the host is
   itself an incident the daemon can file about itself.
I. **Hot-fix spawn authority:** trouble NEVER hand-runs `git worktree add` on a repo the
   scheduler owns. Spawn = router_spawn path (trouble is a CLIENT with a priority class +
   budget accounting + one-fix-per-sig lease); worktree created by the component that
   owns the repo; adopt existing fleet convention (<repo>/.worktrees/<task-id>), never a
   4th convention; per-repo worktree mutex (config.lock contention measured), max
   concurrent + min-free-disk gate + exempt list for huge repos (duckbrain = 8.9GB
   checkout measured); never /tmp as worktree base; spawn-failure semantics = durable
   queue + bounded retry + spawn_pending record (spawn fails exactly when the fleet is
   sick — never silent); no git fetch/gc/prune from foreman worktrees; prune on boot.
J. **Off-by-One research (measured live: 400 on fingerprint field):** the lab is a
   problem_class-keyed verified-answer cache with strict decoder — NOT a hypothesis
   engine. Contract: (1) class-slug derivation = the real work: sig → {source, unit/app
   kind, error taxonomy} → kebab-case slug table maintained in config (sentinel fingerprint
   alone is NOT enough; derivation function + fallback slug "unknown" + never block on
   derivation); (2) discover first: POST /api/v1/problems/discover {problem_class, env,
   lang, version} → cached solution (corpus grep fallback: found:false ≠ absent);
   (3) miss → POST /api/v1/problems/submit {problem_class, description, cadence, context{}}
   ONLY these fields (strict decoder — no fingerprint at top level; context carries the
   fingerprint + stack); duplicate → 409 (treat as queued); solver unavailable → 503 →
   research-degraded ledger record; (4) poll /api/v1/queue/{submission_id} (~3m solve);
   NO webhooks; (5) interface stays pluggable (off-by-one | none | webhook).
K. **Shadow mode resolved (contradiction fix):** shadow (default) = full detection +
   research + PLAY DRAFTING, but mutating tool calls run check_mode ONLY (diff returned,
   nothing applied); read-only tools execute; hot-fix lane drafts the board row but does
   not spawn; PRs stay unmerged. assisted = per-rule execute grants. full = merge-by-policy.
L. **Skills (contradiction fix + trust model):** SKILL.toml is IMMUTABLE in the repo —
   {name, version(int, monotonic), sigs[], play_ref, guards, provenance, min_daemon_version,
   allowed_modules[], signature}; [stats] moves to the LOCAL ledger (never the artifact);
   distribution: signature-verified (ed25519, signer key in config) + canary host first +
   per-host approve policy + min_daemon_version gate; a pulled skill = typed tool calls
   ONLY within its allowed_modules allowlist; two skills match one sig → highest version
   wins, conflict recorded; the skills repo is a RELEASE CHANNEL (protected branch, PR
   review, tags) not a live-write share.
M. **Module SDK (frozen at v1):** Go interface: Descriptor() (name, version, JSON schema,
   required scopes, idempotency class, check_mode support, timeout), Check(ctx, args)
   (Diff, error), Apply(ctx, args) (Result, error), Verify(ctx, args) (VerifyResult, error);
   error taxonomy {transient, permanent, policy_refused}; diff = {path, before, after} list;
   descriptor = Go struct + generated JSON schema; conformance harness = testkit pkg
   (idempotency double-apply, check-vs-apply equivalence, schema violation rejection) —
   every shipped module passes; v0.1 ships: config.{get,set,list}, service.{status,reload,
   restart}, file.{read,patch}, proc.{top,connections}, flow.*.
N. **Ladder state machine (pinned):** states: detected → recorded → play{drafted|
   check_only|applied|failed} → research{requested|returned|degraded|skipped} →
   agent{running|done|failed|suspended} → verifying → resolved | escalated | suppressed |
   quarantined; entry rung per rule; playbook max_runs=2 then next rung; agent two-strikes
   = per-sig per-24h; verify window per rule (default 10m); recurrence after resolved =
   reopen SAME incident (sig-keyed); stabilization = rule's for= measured from last
   qualifying event; in-flight play on daemon restart = park + resume from ledger
   (never re-execute an applied mutating call — idempotency makes re-check safe);
   global agent lease per host; SIGTERM drain = flush ledger + park plays.
O. **Lifecycle/self-watchdog (all three judges):** trouble.service: Type=notify +
   sd_notify READY + WatchdogSec=60 + Restart=always + StartLimitIntervalSec=0;
   OnFailure=trouble-escalate@%n (escalates through a path that does not depend on
   trouble); heartbeat file written every 30s; external checker (fleet cron for v0.1)
   alarms on LEDGER-SEQUENCE STALL (not process liveness); /health = {status, version,
   git_sha, build_time, uptime, ledger_last_seq, per-sensor last-success ages}; version
   stamping via ldflags in every binary + /health + every record's actor field;
   config precedence: flag > env > file > default, resolved-config dump with per-value
   provenance (trouble config explain); state root ~/.local/state/trouble (0700, never
   /tmp) with {ledger/, spool/, worktrees-meta/, skills-local/, backups/}; dashboard
   token never in URLs (cookie/Bearer only); secrets in 0600 EnvironmentFile, never argv.
P. **Topology decisions made once now:** T1 = hub with zero satellites (same code paths);
   satellite→hub forwarding reuses THE SENTINEL WIRE FORMAT + version header +
   idempotency key (sig + sig_norm_version + source host_id) — ONE protocol, not two;
   spool = bounded disk queue (default 256MB) drop-oldest-with-ledger-note; hub mints
   canonical ids, satellites keep local↔hub mapping; bind-matrix auth: loopback =
   pubkey-DSN ok, non-loopback = per-project token OR proxy (config), public = reverse
   proxy mandated; dashboard auth = identity seam interface (token impl v0.1,
   tailscale-identity impl later); every record carries origin{host_id, hub_id, source}
   from day one (this is the T5-enabler); verification windows are zone-aware.
Q. **Dashboard (v0.1 minimum route set):** GET / (overview), /incidents,
   /incidents/{id}, /groups, /groups/{id}, /rules, /breakers, /health.json (also for the
   watchdog), POST /api/incidents/{id}/ack, POST /api/incidents/{id}/close,
   POST /api/autonomy (kill-switch, write-scoped token); live updates = 2s polling of
   partials (SSE reserved); html/template + vendored htmx.min.js (go:embed), token =
   Bearer/cookie, CSRF on POSTs, no tokens in URLs, read vs write token scopes.
R. **v0.1 CUT LINE (k3's vertical slice, binding):** IN = sensors (PSI sampling+triggers,
   journald follow, D-Bus both managers, disk, timers) + sentinel (envelope+store+generic
   JSON + collector go-panic/py-traceback/node-reject) + ledger + dedup core + registry
   (modules in M) + ladder with research rung (off-by-one driver per J) + issue desk
   (github + duckbrain drivers) + flow (board-jsonl direct + task-router; hot-fix per I)
   + dashboard (Q) + skills local loop (candidate→review→promote; distribution = pull
   from git repo v1.0) + shadow/assisted/full + kill-switch + lifecycle (O).
   OUT of v0.1 (no ACs reference them): plays LIBRARY beyond the shipped modules' defaults,
   module SDK as a PUBLIC extension surface (interface is frozen but third-party SDK
   packaging is v1.0), light-mode offload binary, sentinel proxy binary, skill PUSH
   (PR-creating) automation — pull-only in v0.1, ansible-bridge, OTel, SSE.
S. **PRD v2.4 amendments (write docs/prd-amendments-v2.4.md, not a new HTML):** the
   corrections above + port/memory fixes + shadow-mode redefinition + skill artifact
   schema fix + research-rung rewrite + scrubbing subsystem + verification redefinition
   + spawn-authority note + the five-topology decision table (T1..T5 with the one
   decision-per-transition). Keep it tight (~150 lines).

## 2. THE SUITE — write these files in specs/
- SPEC-INDEX.md — suite map: spec → PRD section → ACs covered → status. The binding
  AC-to-spec matrix. Include the frozen v0.1 cut line (R) and the "no AC references
  the deferred column" rule.
- SPEC-01-ledger.md — record schema, sig format {source}:{algo-hex with
  sig_norm_version}, id formats (inc_, grp_, iss_, tsk_, sk_ + ULID), single-writer,
  group-commit durability + stated loss window, rotation/retention/compaction,
  torn-line recovery, query/index design, state-root layout.
- SPEC-02-scrubbing.md — the safety subsystem (C above), rule list, counters, tests.
- SPEC-03-sensors.md — PSI (A), journald (F), D-Bus (G), disk/timers, inotify, rule TOML
  schema + condition language (same expression language as plays' when:), hot-reload
  atomicity, sensor liveness heartbeats.
- SPEC-04-sentinel.md — ingestion contract (E), fingerprinting/grouping, releases,
  quotas/loss policy, collector parsers (multi-line assembly rules: what starts an event,
  continuation, timeout), generic JSON endpoint.
- SPEC-05-ladder.md — state machine (N), stabilization, retries, leases, park/resume,
  evidence-tuple verification (D), canary, autonomy gate matrix per stage × mode,
  kill-switch checkpoint semantics.
- SPEC-06-registry.md — module SDK (M), 6-stage call contract, do-not-touch list format,
  polkit policy artifact + POLICY_REFUSED class, shipped module specs.
- SPEC-07-research.md — off-by-one contract (J), class-slug derivation, degrade paths.
- SPEC-08-flow.md — board-jsonl row schema (id allocation, severity→priority mapping,
  model/provider/capability fields — copy the fleet's real row shape), task-router
  driver, hot-fix spawn via router_spawn (I), one-fix-per-sig lease, verify window,
  promotion, spawn_pending.
- SPEC-09-issues.md — driver contract types (EnsureBySig/Comment/Close/Healthcheck
  signatures, idempotency, backoff, spool-and-replay on driver-down), github + duckbrain
  drivers.
- SPEC-10-dashboard.md — routes (Q), auth/scopes/CSRF, partials, live-update choice,
  budget note (binary/RSS measured).
- SPEC-11-skills.md — artifact schema (L), local promote loop, pull-based distribution,
  signature verification, min_daemon_version gate, conflict rules, [stats]→ledger.
- SPEC-12-lifecycle.md — unit files (O), watchdog chain, heartbeat + external stall
  checker, upgrades (rename-over + park/resume), config precedence + explain, topology
  table (P) with per-transition decisions.
- SPEC-TYPES.md — the shared types file (Go): every struct used across specs (Record,
  Sig, Incident, Group, Evidence tuple, Descriptor, Diff, Result, VerifyResult, Skill,
  Play, ForwardEnvelope...) — single source of truth, every type with a complete JSON
  example, cross-spec references resolve.

## 3. Conventions
- Go 1.26, stdlib-first, CGO_ENABLED=0 static binaries; deps allowed: golang.org/x/sys,
  godbus/dbus/v5, modernc.org/sqlite (only if SPEC-01 justifies the index in SQLite —
  default is in-memory index + JSONL), ed25519 (stdlib).
- Naming: package per spec area (internal/{ledger,scrub,sensors,sentinel,ladder,registry,
  research,flow,issues,dashboard,skills,lifecycle}); error codes TROUBLE-<AREA>-<N>.
- Every spec section 1-8 per the house template (purpose/interface/data model/wiring/
  errors/edge cases/testing/hilo impact). No TBD, no "Phase 2", no "consider".
- Ports: ingestion 7643, dashboard 7644 defaults; state root ~/.local/state/trouble.
- IDs: ULID with prefixes (inc_, grp_, iss_, tsk_, sk_, ev_).
- Timestamps RFC3339 UTC everywhere.

## 4. Self-consistency loop BEFORE commit (mandatory)
1. Every type in every spec resolves in SPEC-TYPES.md; no orphan types; no type defined
   twice with different shapes.
2. Every AC in SPEC-INDEX maps to ≥1 spec section; no spec section without an AC or an
   explicit "design constraint" tag.
3. Every interface signature compiles-by-inspection (matching SPEC-TYPES).
4. Every error code in error catalogs matches the TROUBLE-<AREA>-<N> scheme and is unique.
5. Cross-references (spec↔spec, spec↔PRD section) resolve.
6. The v0.1 cut line is respected: no spec section describes a deferred feature as
   in-scope; deferred items appear only in SPEC-INDEX's deferred table.
7. Report at the end: files written + sizes + section counts + types count + AC coverage
   table. Commit: "specs: SPEC-01..12 + SPEC-TYPES + INDEX (quorum-folded v2.4 brief)".
   This repo is headed for a public org: commits are authored by the AI identity
   only — totalwindupflightsystems <totalwindupflightsystems@gmail.com> — with no
   co-author trailers and no personal names or personal addresses.
