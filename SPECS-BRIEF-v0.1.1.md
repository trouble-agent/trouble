# SPECS-BRIEF v0.1.1 — server-profile hardening (the maintainer directives, 2026-09-16)

You are the spec writer for trouble v0.1.1. The v0.1 code is COMPLETE on main (15/15 test
packages green). This round AMENDS the spec suite and implements the deltas. Read
`~/trouble/SPECS-BRIEF.md` for house conventions, then this brief. All decisions
below are binding (the maintainer's orders) — encode, do not re-argue.

## Directive 1 — Sensor transport: TWO proxy routes (amends SPEC-04 §4 + SPEC-12 §3.7)

The in-code sensor (trouble-sensor SDK shim / the agent's own sensor library) writes to its
trouble agent; the agent routes via the system. Two routes, both first-class, selected per
event-class or per config:

- **Route A — direct:** sensor → local trouble daemon over loopback (the existing envelope
  endpoint on :7643). Zero network hops, zero new auth (loopback bind matrix).
- **Route B — proxied:** sensor → local trouble → HUB trouble (the satellite forward path
  that already exists per SPEC-12 §3.7: same sentinel wire format + version header +
  idempotency key). B is the default when `[hub] endpoint` is configured; A is the default
  otherwise. Per-event-class override: e.g. crash-loop signatures may demand Route A
  (local reflex wins), bulk debug events may demand Route B.

SPEC deltas: (a) sentinel gains a `routes` config block {default: auto|direct|proxy,
per_class: {sig-prefix → route}}; (b) the routing decision is recorded in the ledger
record's origin stanza (route: A|B) — dashboards can show local-vs-relayed; (c) Route B
failure = the existing bounded spool (256MB drop-oldest), never event loss; (d) sensor SDK
surface documents both routes and the auto rule. Acceptance tests: route decision matrix,
B-failure spool replay, per-class override, ledger origin.route assertion.

## Directive 2 — Server modes: standalone | light-hub (DuckBrain + Redis) (amends SPEC-01 §3, new SPEC-13)

Two server profiles. **standalone** (existing, default): one binary, JSONL ledger, zero
external deps — unchanged. **light-hub** (new): keeps the daemon light under hub duty by
offloading to two proven services:

- **Redis** = the ingestion buffer + dedup gate: events land in a bounded Redis stream
  (consumer-group = the ledger writer), dedup via `SET NX` on the ForwardEnvelope
  idempotency key (sig+norm_version+host_id) with a 24h TTL; the ledger writer consumes
  the stream in batches (same group-commit path, same single-writer rule — Redis is the
  QUEUE, the ledger file remains the only durable record). Redis outage = Route A/B
  senders see backpressure (429 + Retry-After), local spools hold — the hub degrades to
  standalone behavior when Redis is down (config flag `hub.require_redis: false`).
- **DuckBrain** = the storage backend for the issue desk (duckbrain driver already ships)
  and the *archival* tier of the ledger: closed ledger generation files are exported to
  the DuckBrain namespace (existing `duckbrain s3 sync`/namespace layout) after compaction,
  so retention drops on the hot host are free and history lives in DuckBrain. DuckBrain
  outage = archival pauses with a gap record; ingestion unaffected.

Config: `[server] profile = "standalone" | "light-hub"`; light-hub requires redis url +
duckbrain namespace. The profile changes ONLY transport/archival plumbing — the rule
engine, ladder, dashboard are identical in both modes. Redis client: pure-Go
`github.com/redis/go-redis/v9` (adds ~2-3MB binary — acceptable, documented). Acceptance:
dedup kills duplicate idempotency keys across a Redis failover; Redis-down degradation;
DuckBrain archival + hot-host drop; profile switch does not change ladder behavior.

## Directive 3 — Ledger pagination: database-style page tokens (amends SPEC-01 §6 + SPEC-12 retention)

The ledger is already partitioned (daily generation files). Add an explicit pagination
contract so "drop old files" stays O(1) and every query is bounded:

- **Page tokens:** every ledger/dashboard/list query accepts `page_token` +
  `page_size` (default 500, max 5000). Token format: `{generation_file}::{byte_offset}::{seq}`
  — opaque, stable within a generation's lifetime, invalidated cleanly when a generation
  is dropped (API returns a `reset` hint + newest-first restart, never an error loop).
- **Per-file footer index:** each generation file closes with a sidecar `{file}.idx`
  (count, byte offsets every N records, seq range, min/max ts) built at rotation —
  pagination and time-window scans touch the sidecar + only the pages they read.
- **Retention = drop generation:** the retention sweep deletes whole generation files
  (+ their .idx + their DuckBrain archive marker) — never in-place record deletion.
  Dashboard "groups ranked by rate" reads only the in-memory index (already built at
  boot) — unaffected.
- API surface: ledger query + dashboard `/incidents`, `/groups` gain `page_token`
  params and return `next_page_token`. Acceptance: 10M-record corpus, page-size stability,
  drop-oldest-generation while paginating (no 500s, clean reset hint), sidecar rebuild.

## Deliverables (this round)

1. Amend `specs/SPEC-04-sentinel.md` (routes), `specs/SPEC-01-ledger.md` (pagination),
   `specs/SPEC-12-lifecycle.md` (profiles) — targeted §-edits via the letter-suffix rule
   (no renumbering), plus a NEW `specs/SPEC-13-server-profiles.md` (light-hub: Redis
   buffer/dedup + DuckBrain archival, both §1-8).
2. Update SPEC-INDEX (new spec row, AC additions: AC-28 route matrix, AC-29 light-hub
   degradation, AC-30 pagination drop-test), bump SPEC-TYPES (RouteDecision, PageToken,
   ProfileConfig, RedisStreamOffsets if needed) — selfcheck.py MUST PASS.
3. Run selfcheck; commit everything as ONE commit:
   `specs: v0.1.1 — dual sensor routes, light-hub profile (Redis+DuckBrain), ledger page tokens`
   with the co-author trailer. Do NOT touch internal/ code (implementation is the next
   board wave).
4. Report: files changed + sizes + new types + new ACs.
