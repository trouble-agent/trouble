# SPEC-13 — server profiles: standalone and light-hub (Redis ingestion buffer + DuckBrain archival) (trouble v0.1.1)

Spec: SPEC-13
Area prefix: TROUBLE-HUB
Package: internal/hub
Consumed types: ProfileConfig, HubStatus, RedisStreamOffsets, LedgerArchiveMarker, Record, RecordDraft, Origin, Actor, RecordKind, Sig, GapRecord, ForwardEnvelope, SpoolEntry, ConfigValue, Duration, HealthResponse, SourceLiveness, RuntimeWatermarks, Topology, TopologyDecision, Evidence, Incident, Group, Breaker, AutonomyGates, SensorHealth
Local types: profileGate, redisClient, streamReader, consumerGroup, dedupGate, dedupKey, archivePlan, archiveJob, archiveQueue, generationFile, hubState, degradation
ACs: AC-29
PRD: §03, §11, §12

## 1. Purpose

`internal/hub` owns the **server profile**: the single configuration decision that says how a daemon
receives events and where its closed ledger generations live once they leave the hot host. Two profiles
exist and both are first-class:

| Profile | Ingestion plumbing | Archival | External dependencies |
|---|---|---|---|
| **standalone** (default) | in-process path: HTTP or local sensor → scrub → sig → ledger `Append` (group-commit) | the ledger's own rotation/compaction/retention (SPEC-01 §3.7) | **zero** |
| **light-hub** | HTTP or local sensor → scrub → sig → **Redis stream** (bounded) → consumer-group ledger writer → ledger `Append` (same group-commit) | after a generation closes: export to the **DuckBrain** namespace, then the hot host may drop it (SPEC-01 §3.7a) | Redis (queue + dedup gate), DuckBrain (archival tier) |

Four properties are design constraints, not features:

1. **The ledger file stays the only durable record.** Redis is a QUEUE and a dedup gate, never a store of
   record. An entry that exists in Redis and not in the ledger is an un-acked entry, not evidence: the
   writer `XACK`s only **after** the ledger's group-commit fsync returns (SPEC-01 §2.1). Redis
   persistence settings therefore cannot change the durability story, and a Redis data loss costs at
   most a re-delivery of entries the satellite's spool still holds.
2. **The profile changes plumbing only.** The rule engine, the ladder, dedup core semantics,
   verification, the dashboard, autonomy gates and the kill-switch are byte-identical in both profiles.
   A conformance test asserts the same incident/verify sequence for the same event stream under both
   (§7).
3. **One writer.** The Redis consumer group has exactly one consumer per state root (the daemon's own
   `host_id`), so the single-writer invariant (TROUBLE-LEDGER-005) is preserved: parallelism is in the
   queue, never in the writer.
4. **Degradation is a designed state, never a silent success.** Redis down, DuckBrain down, and
   `require_redis=true` all have a named behaviour, a distinct code and a `/health.json` reason (§4.3).
   The standalone profile is the degradation target of the light-hub profile, which is why the profile
   switch is a config field and not a second binary.

The profile is orthogonal to the SPEC-12 §3.7 topology table: a T1 laptop and a T5 regional hub each run
`standalone` by default, and a hub that must absorb bursts from many satellites runs `light-hub`
regardless of T-level. `hub.mode=satellite` daemons run `standalone` (their durable queue IS the spool,
SPEC-12 §3.7) and refuse `light-hub` at preflight with TROUBLE-HUB-001.

## 2. Interface

### 2.1 Config surface (`[server]`, precedence `flag > env > file > default`, SPEC-12 §3.1)

```toml
[server]
profile = "standalone"                  # standalone | light-hub
hub_id = ""                             # "" → origin.host_id; set when this daemon fans in satellites

[server.redis]
url = "redis://127.0.0.1:6379/0"        # required when profile = light-hub
password_env = "TROUBLE_REDIS_PASSWORD" # read from the 0600 EnvironmentFile, never argv (SPEC-12 §3.2)
stream = "trouble:ingest"
group = "ledger-writers"
consumer = ""                           # "" → origin.host_id
maxlen = 1000000                        # XADD MAXLEN ~ : approx trim, un-acked entries are never trimmed
dedup_prefix = "trouble:dedup:"
dedup_ttl = "24h"
batch_records = 256
batch_bytes = 524288                    # same 512KB ceiling as hub.forward_batch_bytes (SPEC-12 §3.7)
block_ms = 1000
claim_min_idle = "60s"                  # XAUTOCLAIM floor for a restarted consumer
dial_timeout = "2s"
read_timeout = "5s"
write_timeout = "2s"
require_redis = false                   # true → no degradation: refuse instead of serving without the queue

[server.duckbrain]
enabled = false                         # true → archival on; implied true when profile = light-hub
namespace = ""                          # required when enabled: e.g. "trouble/<host_id>"
endpoint = ""                           # the duckbrain driver endpoint (SPEC-09 driver, same config block)
archive_interval = "1h"
archive_batch_files = 8
keep_local_generations = 2              # generations retained on the hot host after a verified export
verify_after_write = true               # read back the exported object and compare sha256 before the drop
gzip = true                             # generations are exported gzipped (≈350B/line measured)
```

`profile = "light-hub"` without `server.redis.url` or without `server.duckbrain.namespace` fails the
boot preflight with TROUBLE-HUB-001 (exit 13). `require_redis = true` together with a Redis URL that
cannot be dialled at boot fails with TROUBLE-HUB-003 (exit 13); with `require_redis = false` the daemon
starts degraded and says so (§4.3). Every key above appears in `trouble config explain` with its
provenance like any other key — the profile is not a second config system.

### 2.1.1 Redis deployment options (normative for the light-hub profile)

The daemon works against any Redis ≥ 6.2 (streams + consumer groups + `XAUTOCLAIM`). The options
below are the supported deployment shapes; anything outside them is unsupported by the degradation
matrix of §4.3:

1. **Persistence: `appendonly yes` (`everysec`), `save ""` off.** Redis is a queue and a dedup gate,
   never the record (§1 rule 1) — durability comes from the ledger's ack-after-fsync seam (§3.3).
   AOF everysec bounds warm-restart loss to ≤1s of *queue* (entries re-derivable from senders'
   spools), while RDB snapshots invite a false sense of durability. Noeviction is REQUIRED: the
   bound is the daemon's lag gate + `maxlen` (§3.2 rule 3), never Redis-side eviction of a queue.
2. **Topology: single primary, replication optional, no cluster mode.** Cluster mode is refused at
   preflight (TROUBLE-HUB-015) — the stream key and the dedup key space must live on one primary
   for the consumer-group and `SET NX` semantics to hold. A replica MAY follow for warm restarts;
   with `replica-read-only` defaults a failed-over replica is exactly the degradation case §3.4
   already prices (one duplicate, caught by the ledger idempotency check).
3. **Memory: `maxmemory` sized to `maxlen × avg-entry-bytes × 2` minimum, with `maxmemory-policy
   noeviction`.** The queue's ceiling is enforced by trouble, not by Redis eviction.
4. **Keyspace: dedicated logical DB or instance.** `trouble:*` keys must not share a DB with
   eviction-prone caches; preflight `INFO` checks report `evicted_keys`, `maxmemory_policy`,
   `aof_enabled` and `cluster_enabled` into `/health.json` (fields `redis.aof`, `redis.policy`,
   `redis.evicted_keys`) so a misconfigured server is visible, not assumed.
5. **Recovery posture: on a cold (empty) Redis after failover or flush, the daemon re-creates the
   group with `MKSTREAM $` and serves from the ledger — senders' spools replay what Redis lost,
   the dedup gate re-warms from the ledger's idempotency index (boot-time restore, §3.1
   `dedup.state`), and `trouble hub status` reports `redis.state = "cold"` until the first
   acknowledged entry.**

| Key (new) | Default | Meaning |
|---|---|---|
| `server.redis.require_persistence` | `true` | preflight fails (TROUBLE-HUB-016, exit 13) when `appendonly=no`, unless set `false` deliberately |
| `server.redis.check_policy` | `true` | preflight warns (health `redis.policy` ≠ `noeviction`) and `/health.json` flags it |
| `server.redis.failover_grace` | `30s` | window after a reconnect in which the gate trusts restored keys before re-claiming via ledger replay |

New codes: **TROUBLE-HUB-015** (permanent; cluster mode or multi-key-topology Redis refused at
boot, exit 13) and **TROUBLE-HUB-016** (permanent; `appendonly=no` with
`require_persistence=true`, exit 13). Both are preflight refusals: misconfiguration is a boot
failure, never a runtime surprise.

### 2.2 CLI surface

| Command | Contract | Exit codes |
|---|---|---|
| `trouble hub status [--json]` | prints `HubStatus` (profile, Redis stream length/lag/pending, dedup hit/miss counters, archive queue depth, last verified export, degradation reason). Reads Redis + the state root; writes nothing. | 0 ok, 1 degraded |
| `trouble hub archive [--dry-run] [--file FILE] [--force]` | the archival job for one generation (default: every closed generation without an `exported` marker). `--dry-run` prints the `ArchivePlan` (file, bytes, gzip bytes, marker id, target namespace) and writes nothing. | 0 ok, 1 failed, 2 refused |
| `trouble hub dedup --key KEY [--json]` | read-only probe of the dedup gate for one idempotency key: present/absent + TTL. No mutation, no reveal of anything but presence. | 0 present, 1 absent, 2 Redis unreachable |
| `trouble hub drain [--timeout D]` | consume-and-ack the stream to empty (or the timeout), then stop. Used before a profile switch or a host migration (SPEC-12 §3.6 upgrade path). | 0 drained, 1 timeout with pending entries |

`trouble hub drain` is the only mutating verb; it moves entries from Redis into the ledger and never
deletes an un-acked entry.

### 2.3 Go surface

```go
// internal/hub — package hub

// profile gate (§4.1)
func Gate(cfg lifecyclepkg.Config) (profileGate, error)          // 001 on an invalid/incomplete profile
func TopologyImpact(p profileGate) []types.TopologyDecision       // the SPEC-12 §3.7 rows + the profile row

// ingestion buffer (§3.2)
func OpenRedis(ctx context.Context, cfg RedisConfig) (*redisClient, error) // 002/003
func EnsureGroup(ctx context.Context, c *redisClient, stream, group string) error // BUSYGROUP = success (005 is a hard failure only)
func Enqueue(ctx context.Context, c *redisClient, env types.ForwardEnvelope, route types.RouteDecision) (uint64, error) // XADD; 004 on failure
func Consume(ctx context.Context, c *redisClient, w ledger.Writer, cfg RedisConfig) error // XREADGROUP → scrub → Append → XACK (§3.3)

// dedup gate (§3.4)
func Dedup(ctx context.Context, c *redisClient, key types.Sig, normVersion int, hostID string) (fresh bool, err error) // SET NX EX

// archival (§3.5)
func PlanArchive(stateRoot string, cfg ArchiveConfig) ([]archivePlan, error)     // 011 on an unclosed generation
func Archive(ctx context.Context, cfg ArchiveConfig, plan archivePlan) (types.LedgerArchiveMarker, error) // 009/010/012
func ArchiveQueue(stateRoot string) (archiveQueue, error)
func DroppableGenerations(stateRoot string, keep int) ([]generationFile, error)  // 014 when an export is unverified
func HubStatus(ctx context.Context, cfg Config) (types.HubStatus, error)
```

`internal/hub` imports `internal/types`, `internal/ledger` and `internal/scrub`; it is wired by the app
and mounted by nothing else. The dashboard reads `HubStatus` through `HealthResponse.Hub`; no other
package imports `internal/hub`.

### 2.4 HTTP surface (no new listener, no new route)

The profile adds **no** route. Three existing surfaces change behaviour in `light-hub`:

| Surface | Change |
|---|---|
| `POST /api/{project_id}/envelope/` and `/store/` (SPEC-04 §2.1) | success is now "enqueued" (`200 {"id": …}` after XADD returns); overload returns the existing `429` + `Retry-After` + `X-Sentry-Rate-Limits` shape with reason `overloaded` (SPEC-04 §3.9 header grammar, unchanged) |
| the local sensor/system path (SPEC-04 §3.10a) | Route A/B decision is taken per event class, then the same `Enqueue` call |
| `GET /health.json` (SPEC-12 §2.2, SPEC-10 §2.9) | the body carries the additional `hub` stanza (`HubStatus`); `status=degraded` with `detail.reason ∈ {redis_unavailable, redis_refusing, archive_paused}` when the profile is degraded |

There is exactly one health surface (SPEC-12 §2.2). The profile reports through it; it does not open a
second one.

## 3. Data model

### 3.1 Profile state (one file, one truth)

```
<state_root>/hub/
├── profile.json          0600  {profile, since, hub_id, config_hash} — written at every boot
├── archive/              0700
│   ├── queue.jsonl       0600  append-only archiveJob records (pending → exported → verified → dropped)
│   └── markers.jsonl     0600  append-only LedgerArchiveMarker records (the durable export ledger)
└── dedup.state           0600  {lru_size, restored_keys, degraded_windows} (boot-time restore of the fallback LRU)
```

`markers.jsonl` is the only durable statement about what has been exported. The DuckBrain namespace holds
the bytes; the marker file holds the claim, the sha256 and the verification result. A marker is never
rewritten and never deleted — dropping a generation writes a `dropped` marker, not a deletion.

### 3.2 The stream entry (THE sentinel wire format, one protocol)

One stream field per entry, `env`, carrying the JSON of `ForwardEnvelope` (SPEC-TYPES §3.14) exactly as
the satellite→hub forward path already defines it, plus the routing decision:

```json
{"protocol_version":1,"idempotency_key":"sentinel:sha256v1:9f2c1d3e4b5a6c7d|1|7f3a91c2d4e5b607","host_id":"7f3a91c2d4e5b607","hub_id":"","origin":{"host_id":"7f3a91c2d4e5b607","hub_id":"","source":"sentinel:payment-worker","route":"B"},"records":[],"ack":41207,"route":"B","enqueued_ts":"2026-09-16T09:14:03.221Z"}
```

Rules:

1. **Batching**: one entry carries up to `batch_records` records and `batch_bytes` decompressed bytes —
   the same 200-record/512KB composition rule as `hub.forward_batch_*`, so the queue can never hold a
   batch that violates the ingestion caps.
2. **Ordering**: `XADD` is the only append; per-stream order is the arrival order. The ledger writer
   preserves entry order within a batch, so per-host seq order survives the queue.
3. **Approximate trim is a memory ceiling, and it is NOT ack-aware.** Redis `XADD … MAXLEN ~ maxlen`
   evicts the OLDEST entries whenever the stream exceeds the bound, regardless of consumer-group
   pending (PEL) state — a trimmed-but-unacked entry would only ever surface as a min-idle
   `XAUTOCLAIM` of an id the stream no longer holds. The no-loss guarantee therefore does NOT come
   from the trim. It comes from (a) the **lag gate**: entries are enqueued only while
   `XLEN − (acked position)` stays under `maxlen`; beyond that, §4.2 backpressure (429 +
   `Retry-After`) engages and senders' local spools hold; and (b) the ack-after-fsync rule of §3.3,
   which bounds any trimmed-entry exposure to batches already fsynced into the ledger. Sizing rule:
   `maxlen` ≥ 60 s of peak burst at `batch_records` per entry, so the lag gate — not eviction — is
   what stops the world.
4. **Payload scrubbing happens before enqueue.** The scrubber (SPEC-02) runs on the ingestion path in
   both profiles, so the queue never holds an un-redacted value and the log-redaction counters are the
   same numbers in both profiles.
5. **The hub still mints canonical ids** (SPEC-12 §3.7): a record that arrives with a local id gets its
   canonical `rec_id` at ledger-append time, and the local id stays in `payload.local_rec_id`.

### 3.3 The consumer group and the exactly-once seam

```
XGROUP CREATE trouble:ingest ledger-writers $ MKSTREAM      # BUSYGROUP is success, not an error
XREADGROUP GROUP ledger-writers <host_id> COUNT <batch_records> BLOCK <block_ms> STREAMS trouble:ingest >
  → scrub-check → ledger.Append(batch)        # group-commit, one fsync per batch, ≤200ms window
  → ledger fsync returns                       # THE durable point
  → XACK trouble:ingest <ids> (one call, all ids of the batch)
```

- **At-least-once in, exactly-once out.** Redis delivery is at-least-once; the ack-after-fsync rule plus
  the dedup gate (§3.4) is what makes the ledger view exactly-once. A crash between `Append` and `XACK`
  re-delivers the batch, and the re-delivery lands zero new records because the dedup key is already
  `SET NX`-claimed.
- **One consumer per state root**: `consumer = host_id`, so `XPENDING` is always attributable and two
  daemons on one state root are impossible (the LOCK of SPEC-01 §3.4 refuses first).
- **Reclaim after restart**: `XAUTOCLAIM … MIN-IDLE-TIME claim_min_idle` takes back entries whose
  consumer died. A reclaimed batch is re-processed through the same path, so the dedup gate decides, not
  the reclaim.
- **Bounded work per iteration**: one batch per `ReadGroup` call, one `Append` per batch, one `XACK` per
  batch — the writer never holds two batches, so the memory ceiling stays the batch, not the stream.
- **The consumer stalls are visible**: `lag = XLEN − (last acked id position)`, `pending`, and
  `last_delivered_id` are reported in `RedisStreamOffsets` and in `/health.json` every heartbeat.

### 3.4 The dedup gate

```go
Dedup(ctx, c, sig, normVersion, hostID) // SET trouble:dedup:{sig}|{norm_version}|{host_id} 1 NX EX <dedup_ttl>
```

- **Key = the ForwardEnvelope idempotency key** (SPEC-12 §3.7, incident-scoped derivation):
  `sig + "|" + itoa(norm_version) + "|" + host_id`, prefixed. One key space, one derivation, two
  consumers (the hub's intake LRU and the Redis gate).
- **Fresh** ⇒ `ENQUEUE` proceeds; **present** ⇒ the entry is dropped with a counter
  (`dedup_hits_total`) and the response is the idempotent `200 {"id": <original …>}` shape — a replay
  after a Redis failover is a hit, not a duplicate record.
- **TTL = 24h** (`dedup_ttl`). The window is stated, not implied: a replay older than 24h re-enters, and
  the ledger's own `rec_id`/`local_seq` mapping (SPEC-12 §3.7) keeps it from double-counting a group.
- **Redis down** ⇒ the gate falls back to the bounded in-memory LRU (`hub.dedup_lru` = 65536, restored
  at boot from the ledger's origin fields) and records TROUBLE-HUB-006 once per degradation window. The
  fallback window is smaller than 24h; that fact is printed in `/health.json` as
  `hub.dedup_window = "lru"`, so a smaller window is never mistaken for the full one.
- **Failover**: when a replica is promoted, the `SET NX` claim made before the promotion either survived
  (replication) or did not (asynchronous replication loss). The dedup gate is therefore **not** the
  exactly-once guarantee; the ack-after-fsync rule of §3.3 is. A failover that loses a claim costs one
  duplicate entry, which the ledger's idempotency-key check on `ForwardEnvelope` rejects with
  `dedup_conflict_total` — the same behaviour SPEC-12 §3.7 pins for the hub LRU.

### 3.5 Archival: closed generations to DuckBrain

A generation is **closed** when rotation or compaction has fsynced it and written its `.idx` sidecar
(SPEC-01 §3.7a). Closed generations are the archival unit:

| Step | Action | Failure |
|---|---|---|
| 1 plan | `PlanArchive` lists closed generations with no `exported` marker → `ArchivePlan{file, bytes, gzip_bytes, records, first_seq, last_seq, min_ts, max_ts, marker_id, namespace, object_key}` | 011 when a generation is not closed (no `.idx`), or when the file changed under the plan (size/mtime/sha256 re-check at export) |
| 2 export | gzip (when `gzip=true`) → write one object per generation into the DuckBrain namespace: `<namespace>/ledger/<generation-file>.jsonl.gz` + a sidecar object `<…>.idx.json` carrying the SPEC-01 §3.7a sidecar verbatim | 009 when the namespace is unconfigured/unresolvable; 010 when the driver is unreachable (pause) |
| 3 verify | with `verify_after_write=true`: read the object back, compare `sha256(exported bytes)` with the marker's `sha256` and the byte count | 012 partial write: the marker stays `pending`, the export is retried with the SAME `marker_id` (idempotent overwrite by key), never a second object |
| 4 mark | append one `LedgerArchiveMarker{state:"exported"}` and one `lifecycle{op:"archive_exported"}` record to the ledger | the marker file is append-only; a torn last line is truncated on read (SPEC-01 tolerant-reader discipline) and the marker is re-derived from DuckBrain by `marker_id` |
| 5 drop | only now may the retention sweep delete the generation + its `.idx` (SPEC-01 §3.7a), which appends a `dropped` marker | 014 refuses the drop while any marker for that generation is not `exported` and verified — history outlives the hot host or it does not leave |

`marker_id = hex(sha256(file bytes))[:16]` — content-derived, so a re-export of the same file is the same
marker and the same object key, and a re-export of a compacted generation is a new marker (different
bytes). Idempotency here is by construction, not by bookkeeping.

### 3.6 Types contributed to SPEC-TYPES by this spec

`ProfileConfig`, `HubStatus`, `RedisStreamOffsets`, `LedgerArchiveMarker` — defined in SPEC-TYPES
§3.15.11 with their JSON examples. Two shared types gain one field each, defined in SPEC-TYPES first:
`Origin.Route` (the routing decision, SPEC-04 §3.10a) and `HealthResponse.Hub`.

## 4. Wiring

### 4.1 Boot sequence (profile-gated)

1. SPEC-12 config resolution runs first (`flag > env > file > default`), then `hub.Gate`:
   `profile` ∈ {standalone, light-hub}; light-hub requires `server.redis.url` and
   `server.duckbrain.namespace`; `hub.mode=satellite` refuses light-hub. One `config` record carries the
   resolved profile and its provenance (SPEC-12 §3.1); a refusal exits 13 with TROUBLE-HUB-001.
2. `standalone` ⇒ nothing further happens in this package; the existing in-process ingestion path is
   unchanged and `HubStatus.Profile == "standalone"` with `Enabled == false`.
3. `light-hub` ⇒ `OpenRedis` (dial + `PING` + `INFO server`) → `EnsureGroup` (BUSYGROUP = success) →
   `XAUTOCLAIM` any entries stranded by a previous consumer → the consumer goroutine starts **before**
   the ingestion listener accepts traffic, so no request can be 200-acked into a queue nobody drains.
4. `lifecycle` writes the ledger's `config`/`lifecycle` records for boot (SPEC-12 §4.1);
   `hub.Status` is registered into `HealthResponse.Hub` and printed by `trouble hub status`.
5. The archive timer starts on `archive_interval` and runs one pass at boot; a boot pass with a
   DuckBrain outage is a paused queue, not a failed boot (010), so an archival outage never blocks
   ingestion — the two dependencies fail independently by construction.

### 4.2 Ingestion path (the only write path, both profiles)

```
HTTP /store/ | /envelope/ | generic JSON  ─┐
local sensor (Route A direct)              ─┼─► scrub ─► sig ─► dedup gate ─► [light-hub] XADD ─► consumer ─► ledger.Append
local sensor (Route B proxied)             ─┘                                   [standalone] ─────────────► ledger.Append
```

- The two profiles differ by exactly the bracketed hop; every other box is the same code, which is what
  AC-29's ladder-invariance assertion measures.
- **Backpressure (light-hub)**: when the un-acked stream length exceeds `maxlen`, or `XADD` fails, or the
  consumer's lag exceeds `maxlen/2`, the ingestion path answers `429` with `Retry-After` and
  `X-Sentry-Rate-Limits` (reason `overloaded`, SPEC-04 §3.9 grammar) and counts
  `backpressure_total`. SDKs discard on 429 by contract; local spools hold; the satellite spool holds by
  design (SPEC-12 §3.7). **An event is never confirmed to a sender before it is either acked into the
  ledger or explicitly refused.**
- **`require_redis = true`** replaces the 429-degradation with a hard refusal: a request that cannot be
  enqueued gets `503` + `Retry-After` and is never 200-answered, so a caller that trusts a 200 can trust
  durability. Operators who prefer availability set `false` (the default).

### 4.3 Degradation matrix (normative)

| Condition | `require_redis` | Senders see | Daemon | Ledger | Code | `/health.json` |
|---|---|---|---|---|---|---|
| Redis unreachable at boot | false | (not serving yet) | starts | intact | 003 | `degraded`, `detail.reason="redis_unavailable"` |
| Redis unreachable at boot | true | 503 + `Retry-After` (until Redis returns) | exits 13 | intact | 003 | — (refused to start) |
| Redis lost at runtime | false | 429 + `Retry-After` + rate-limit header; local spools hold | keeps running, drains the in-flight batch | intact | 004 | `degraded`, `detail.reason="redis_unavailable"` |
| Redis lost at runtime | true | 503 + `Retry-After` | keeps running, drains the in-flight batch | intact | 004 | `degraded`, `detail.reason="redis_refusing"` |
| Redis returns | — | 200; the spool flushes through the same path | rewires the consumer | intact | lifecycle `redis_restored` | `ok` |
| DuckBrain unreachable | — | **unchanged** (ingestion is never gated on archival) | archive queue depth grows, visible | intact | 010 | `degraded`, `detail.reason="archive_paused"` |
| Redis ACL / auth rejected | — | as unreachable, reason `redis_refusing` | keeps running | intact | 002 | `degraded` with `detail.reason="redis_auth"` |
| profile changed in config | — | one restart required | refuses a live switch | intact | 013 | `degraded` until restart |

Two rules make the matrix safe: **ingestion never depends on DuckBrain**, and **archival never depends
on ingestion**. A hub with a broken archive tier keeps detecting the fleet's bugs; a hub with a broken
queue stops accepting without losing what it already accepted.

### 4.4 What the profile does not change

Rule loading, PSI/journald/D-Bus sensing, the ladder state machine, research, plays, the issue desk, flow
and spawning, skills, autonomy gates, the kill-switch, dashboard routes, the ledger record schema, and
every verification rule. `trouble hub status` prints the profile; nothing else in the suite branches on
it.

## 5. Errors

Codes: **TROUBLE-HUB-001 .. TROUBLE-HUB-016** (SPEC-TYPES §5, SPEC-INDEX §3.5). Class vocabulary is the
shared one (`transient | permanent | policy_refused`); no code here is `policy_refused`, because a
profile refusal is a configuration error, not an authorization decision.

| Code | Class | Trigger (exact) | Behaviour | Operator remedy |
|---|---|---|---|---|
| TROUBLE-HUB-001 | permanent | invalid/incomplete profile: unknown `server.profile`, light-hub without `server.redis.url` or `server.duckbrain.namespace`, or light-hub with `hub.mode=satellite` | boot preflight fails, exit 13, nothing served | set the missing key(s); `trouble config explain --key server.profile` names the source that won |
| TROUBLE-HUB-002 | permanent | Redis rejected the connection or credentials: bad URL scheme, unsupported protocol version, `NOAUTH`/`WRONGPASS`, or a selected DB the account cannot use | light-hub starts degraded (`require_redis=false`) or exits 13 (`true`); nothing is served that cannot be queued | fix `server.redis.url`; the password comes from the EnvironmentFile key, never argv (SPEC-12 §3.2) |
| TROUBLE-HUB-003 | transient | Redis unreachable at boot (dial error/timeout) | `require_redis=false`: degraded start with the standalone path active; `true`: exit 13 | start Redis, or set `require_redis=false` deliberately |
| TROUBLE-HUB-004 | transient | `XADD` failed while serving (Redis lost, OOM, READONLY replica) | senders get 429 (`false`) or 503 (`true`) + `Retry-After`; the in-flight batch is not lost; local spools hold | restore Redis; the spool flushes through the same path |
| TROUBLE-HUB-005 | permanent | consumer-group operation failed in a way BUSYGROUP does not explain: `XGROUP CREATE` refused, mistyped `XAUTOCLAIM`, group on a non-stream key | the consumer does not start; the daemon serves nothing (a queue with no drainer is worse than a stopped daemon) | inspect the key type/name; `trouble hub status` prints the group and key |
| TROUBLE-HUB-006 | transient | dedup gate unavailable (Redis up but the gate errors, e.g. `SET NX` on a read-only replica) | dedup falls back to the bounded LRU for the window; `dedup_window="lru"` in `/health.json`; counted once per window | restore write access to the primary; the window's size is stated in `/health.json` |
| TROUBLE-HUB-007 | permanent | a stream entry does not decode as `ForwardEnvelope` (bad JSON, unknown protocol version, over-cap batch) | the entry is dead-lettered (moved to `trouble:ingest:dead` with a reason) and counted; never acked as success; the ledger gets a `gap` record with the entry id | inspect the dead-letter entry; a version skew is fixed by upgrading one side (SPEC-12 §3.7) |
| TROUBLE-HUB-008 | transient | pending entries older than `claim_min_idle` × 3 with no active consumer (stranded work) | `XAUTOCLAIM` re-takes them each pass; one record per reclaim round | check for a replacement daemon on the same state root; the LOCK (SPEC-01 §3.4) is the arbiter |
| TROUBLE-HUB-009 | permanent | archival target unusable: `server.duckbrain.namespace` empty/unresolvable, or the driver config is invalid | archival does not start; the archive queue retains every pending generation; ingestion unaffected | configure the namespace/endpoint; `trouble hub archive --dry-run` prints the plan without writing |
| TROUBLE-HUB-010 | transient | DuckBrain unreachable or erroring during export | the job stays `pending`; the queue depth is visible; retried at `archive_interval` with the same `marker_id` | restore DuckBrain; no re-planning is needed (marker ids are content-derived) |
| TROUBLE-HUB-011 | permanent | export refused: the generation is not closed (no `.idx`), or its bytes changed between plan and export | the job is refused and re-planned on the next pass; no partial object is written | let rotation/compaction finish (SPEC-01 §3.7/§3.7a); re-run `trouble hub archive` |
| TROUBLE-HUB-012 | transient | export verification failed (read-back sha256/byte mismatch, truncated object) | the marker stays `pending`; the export is retried with the same key (idempotent overwrite); the hot generation is NOT droppable | inspect the driver's write path; a mismatch means the tier is not trustworthy for that object |
| TROUBLE-HUB-013 | permanent | live profile switch requested (SIGHUP config reload changed `server.profile`) | refused; the running profile stays in force; `/health.json` reports the pending switch until restart | restart the daemon in a maintenance window (SPEC-12 §3.6 upgrade path) |
| TROUBLE-HUB-014 | permanent | retention tried to drop a generation whose markers are not `exported` and verified | the drop is refused; the generation stays on the hot host; a `lifecycle{op:"retention_deferred"}` record names the file and the marker state | restore archival, then let the sweep run; raising `keep_local_generations` buys time but does not create history |

Foreign codes referenced, not minted here: `TROUBLE-LEDGER-*` for append/rotation/compaction failures,
`TROUBLE-LIFECYCLE-014` for a forward protocol-version refusal, `TROUBLE-LIFECYCLE-015` for a spool
eviction, and `TROUBLE-SENTINEL-010/011` for the quota and disk-budget 429s the ingestion route keeps
emitting unchanged. Every code in this table is mirrored into the ledger record that describes the
failure (`Record.payload.error_code`, SPEC-INDEX §5.3).

## 6. Edge cases

1. **Redis failover between XADD and the dedup claim.** Order of operations is dedup-then-XADD, so a
   lost claim costs one duplicate entry, which the hub rejects by the same idempotency key; a lost XADD
   costs nothing (the sender's 200 was never issued). Exactly-once is a property of the ack-after-fsync
   rule, never of the Redis topology.
2. **Replica promoted with an async-replication gap.** Dedup keys and stream entries can both be older
   than the promotion point. The consumer drains what the promoted node has; a gap is discovered by the
   satellite's ack-based trimming (its `local_seq` outruns the hub's `X-Trouble-Ack`) and becomes a
   `satellite_seq_gap` gap record (SPEC-12 §3.7) — visible loss, not silent loss.
3. **Stream trimmed under pending entries.** `MAXLEN ~` never removes an un-acked entry; a manual
   `XTRIM` that does is detected (`XPENDING` id missing from `XRANGE`) and recorded as a `gap` with the
   exact id range, because the ledger's seq continuity is the only thing that must not lie.
4. **Duplicate delivery of the same batch** (consumer crash after `Append`, before `XACK`): the batch is
   re-processed; the dedup gate answers "present" for the incident-scoped key and the ledger appends
   nothing new. A batch whose records are record-scoped (`rec:…`) is deduped by `rec_id` at the ledger
   boundary.
5. **A stream entry containing records for two projects.** Legal: the batch is a set of records from one
   host, and per-project quota accounting happened at ingestion. The consumer never re-attributes a
   record's project.
6. **Entry larger than `batch_bytes`.** Decoding refuses it (007), dead-letters it and records the gap;
   the sender's 200 for the original request was already backed by the spool/refusal path, so an
   over-cap batch cannot exist in a healthy producer.
7. **Redis `maxmemory` eviction policy set to `allkeys-lru`.** Eviction of a stream key is data loss in
   the queue; the spec requires `noeviction` (documented in `deploy/README.md`) and the daemon detects a
   stream-length drop larger than the un-acked count, recording a `gap` with the difference.
8. **Archival of a generation that a page token still references.** The drop is legal: SPEC-01 §3.7a
   requires the API to answer a dropped-generation token with a `reset` hint and a newest-first restart,
   and the DuckBrain copy remains readable through the archive marker. Pagination never 500s on a
   dropped generation.
9. **Archive marker file torn** (crash mid-append): the tolerant reader truncates the fragment, and
   `marker_id` re-derivation from DuckBrain restores the state; a marker is content-addressed, so the
   bookkeeping is reconstructible from the archive itself.
10. **A generation re-compacted after export.** Compaction of an already-archived day produces new bytes
    → new `marker_id` → a second object and a second marker; the `dropped` marker for the old
    generation names both ids, so the history chain is unambiguous.
11. **DuckBrain namespace write conflict** (two hosts sharing one namespace): the object key carries the
    generation file name, which carries the date; a same-name object with different content is detected
    by the read-back verification (012) and refused rather than overwritten silently. One namespace per
    host is the documented default (`trouble/<host_id>`).
12. **Burst larger than the queue with SDK senders.** SDKs discard on 429 (SPEC-04 §3.9, deliberate); the
    loss is counted per event and per group; the profile does not soften that contract, because a
    200-answer the daemon cannot honour is the failure mode this profile exists to prevent.
13. **Kill-switch active.** Ingestion, queueing and archival continue in both profiles: a stopped ladder
    must still see its fleet (SPEC-04 §6.18).
14. **`trouble hub drain` while traffic is live.** Draining does not stop ingestion (that would make the
    command unsafe to run); it consumes and acks what is present, and the live path keeps appending. The
    command's exit code states what was left, not what was lost.
15. **Config reload (`SIGHUP`) flipping `require_redis` at runtime.** Accepted: it only changes whether a
    failure degrades or refuses. Flipping `server.profile` needs a restart (013).

## 7. Testing

| File | Cases | Pass threshold / regression number |
|---|---|---|
| `internal/hub/gate_test.go` | profile matrix: standalone, light-hub complete, light-hub without redis url, without namespace, unknown profile, light-hub + `hub.mode=satellite` | 001 selected for each incomplete case; **0 HTTP responses served** before the refusal; standalone starts with `Enabled=false` |
| `internal/hub/redis_test.go` | `EnsureGroup` twice (BUSYGROUP = success); wrong key type → 005; auth failure → 002; dial timeout with `require_redis` false/true → 003 + degraded start / exit 13 | group exists after both calls; 0 panics; the degraded start serves and reports `detail.reason="redis_unavailable"` |
| `internal/hub/dedup_test.go` | fresh → present; TTL expiry re-opens the window; Redis-idle fallback to the LRU; failover that loses the claim → 1 duplicate entry + `dedup_conflict_total` | exactly 1 ledger record for a replayed batch under failover (AC-29); dedup hits/misses counted |
| `internal/hub/consume_test.go` | batch bound 256/512KB; append-before-ack ordering (kill between `Append` and `XACK` → re-delivery → 0 new records); reclaim after `claim_min_idle`; ordering preserved inside a batch; backpressure above `maxlen` | 0 duplicate records; ack count == fsync count; `lag` never exceeds `maxlen` |
| `internal/hub/archive_test.go` | plan/export/verify/mark/drop cycle; read-back mismatch → 012 + not droppable; unclosed generation → 011; DuckBrain down → 010 + queue depth; re-export of the same file is one object (idempotent by `marker_id`); drop refused while unverified → 014 | 1 object per generation; sha256 verified before every drop; 0 generations dropped without a verified marker |
| `internal/hub/profile_invariance_test.go` | one scripted event stream replayed through both profiles (standalone and light-hub with Redis + a DuckBrain stub) | **identical incident and verify sequences** (same count, same states, same order); AC-29 ladder-invariance assertion |
| `internal/hub/status_test.go` | `HubStatus` fields from a live Redis + a populated state root; degraded reason selection; `/health.json` carries the `hub` stanza | every field present and non-default; degraded reason matches the injected failure |
| `tests/e2e/ac29_light_hub.sh` | Redis + DuckBrain stub on a test network; duplicate `ForwardEnvelope` keys across a Redis restart → 1 record; stop Redis mid-burst → 429 + `Retry-After`, spool holds, restart → replay; archive a generation then drop it → DuckBrain holds the bytes, the hot host does not; DuckBrain down → archival paused with 010 while ingestion keeps returning 200 | 0 records lost across the Redis outage; 1 record for the duplicate key; the dropped generation is readable from the archive; ingestion unaffected by the archival outage |

AC-29 is the profile's acceptance test and lives in `profile_invariance_test.go` +
`tests/e2e/ac29_light_hub.sh`; the degradation matrix of §4.3 has one row per test case above. Memory:
the Redis client adds ≈2–3 MB to the binary and one batch (≤512KB) plus the dedup LRU (65536 keys ≈
6 MB worst case) to RSS, which keeps the binary inside the 8–15 MB budget (SPEC-TYPES §6.1) and steady RSS
under the 80 MB ceiling.

## 8. hilo impact

**Created** (`internal/hub`, one new package; nothing else in the tree is modified by this spec):

| Path | Fan-out | Fan-in |
|---|---|---|
| `internal/hub/gate.go`, `status.go` | `internal/types`, `internal/lifecycle` (config read) | app wiring, `cmd/trouble hub` |
| `internal/hub/redis.go`, `consume.go` | `internal/types`, `internal/ledger` (writer), `internal/scrub`, `github.com/redis/go-redis/v9` | app wiring (light-hub only) |
| `internal/hub/dedup.go` | `internal/types`, `github.com/redis/go-redis/v9` | `redis.go` (ingestion path) |
| `internal/hub/archive.go`, `markers.go` | `internal/types`, `internal/ledger` (read: closed generations) | `cmd/trouble hub archive`, app wiring |
| `deploy/README.md` (Redis `noeviction` + DuckBrain namespace notes), `examples/config.toml` (a commented light-hub block) | — | operator docs; fleet values live only here |

**Dependency direction:** `internal/hub` → `internal/types` + `internal/ledger` + `internal/scrub`; the
dashboard reads `HubStatus` through `HealthResponse` only, so no package imports `internal/hub` back and
the graph stays acyclic. `internal/types` is untouched by this spec except for the four contributed
types and two added fields (documented there first).

**New dependency:** `github.com/redis/go-redis/v9`, pure Go, `CGO_ENABLED=0`-clean; it is the only
addition to `go.mod` in this round. Binary growth is ≈2–3 MB and is stated here so the 8–15 MB budget
verdict (SPEC-TYPES §6.1) is a measurement, not an assumption.

**Blast radius:** ingestion plumbing (a queue hop), archival (new tier), one new CLI group, one new
config block. Greenfield repository `~/trouble`: no fleet repository, board or database is
touched, and no test writes outside its temp state root.

**Highest-risk change points for the consistency loop:** the ack-after-fsync ordering (§3.3) — the one
rule that makes Redis a queue instead of a store; the drop-gate of §3.5 step 5 — the one rule that keeps
history from being deleted before it is archived; and the profile-invariance claim of §1.2, which is
asserted by test and must never become an assumption.
