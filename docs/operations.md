# trouble operations

Operating notes for what is implemented today: the append-only ledger (`internal/ledger`,
SPEC-01). Everything here is derived from the spec, not from tribal knowledge.

## 1. Durability and the crash-loss window

A record is durable the moment `Append` returns. Records already written to the ledger file but not yet fsynced are lost on **power loss or kernel panic**; the maximum loss window is `ledger.fsync_window_ms`, default **200 ms**, measured from the last successful fsync completion. A process crash (SIGKILL), a daemon restart or a warm OS reboot loses nothing: those records are already in the running kernel's page cache.

Read it the other way when you are on call: a SIGKILL of `troubled` is never a data-loss incident.
A host power-cut is a data-loss incident of at most one fsync window. If you need a smaller window,
lower `ledger.fsync_window_ms` and pay for it in throughput — 512 records/s per-line against
515k records/s amortized.

`ledger_loss_window_ms` in `GET /health.json` and `trouble ledger status --json` always reports the
configured value, and `docs_test.go` fails CI if this paragraph drifts from that value's meaning.

## 2. Daily checks

| Check | Command | What "wrong" looks like |
|---|---|---|
| sequence is advancing | `trouble ledger status --json` | `stall_s` growing while the daemon is up: the writer is stuck, not idle |
| no holes or corruption | `trouble ledger verify` | non-zero exit; the report names the files |
| disk inside budget | `trouble ledger status --json` | `disk_bytes` approaching `disk_budget_bytes` (default 2 GiB) |
| index healthy | `trouble ledger status --json` | `index.degraded=true` with a reason, or cold evictions climbing |

`trouble ledger verify` opens **no** lock: it is always safe to run against a live ledger. It reads
the same files the writer is appending to, excludes a torn final line, and reports rather than
repairs.

## 3. Recovery

* **Torn last line** (process killed mid-write, ENOSPC during a batch write): the fragment is
  excluded from the index, counted, and the file gets the missing newline before the next record, so
  exactly one line is lost. A `lifecycle{op:"recover_torn"}` record lands in the trail at boot.
* **Hole in seq**: reported as `TROUBLE-LEDGER-002` on the next read and written into the trail by
  the next successful batch as `lifecycle{op:"hole", from, to}`. The counter never goes backwards and
  seq is never re-used.
* **Interrupted compaction** (kill between rename and unlink): both generations exist; boot prefers
  the higher generation, unlinks the lower one and writes `lifecycle{op:"compaction_resume"}`.
  `trouble ledger verify` accepts that state — it is a legal, self-healing intermediate.
* **Corrupt HEAD**: HEAD is a hint, never a source of truth. `seq_alloc = max(HEAD.last_seq, highest
  seq in the highest-generation file) + 1`; a corrupt or missing HEAD costs one bounded tail read.
* **Second writer**: `Open` fails with `TROUBLE-LEDGER-005`, logs the holding `LOCK`'s pid/version/
  git_sha, and exits non-zero. Never override it — one writer per ledger, forever.

## 4. Compaction and retention

`trouble ledger compact --day D [--dry-run]` rewrites one day into a new generation file
(`D.N.gen.jsonl`). It is never in place: the previous inode may be unlinked but never modified, so a
reader holding it finishes on a stable image. What compaction may do:

* fold every `event`/`canary` line into one aggregate per group per day (`op=compaction_aggregate`)
  carrying the counters, first/last ts and first/last seq — **counts are never dropped**;
* expire `event`/`canary` payloads older than `payload_ttl` (`payload_ttl_expired=true`), keeping
  identity, sig, origin, actor, ts, seq and the redaction count, so the record stays countable;
* copy every audit-spine record (`incident, group, verify, play_run, agent_run, tool_call, research,
  issue, flow, spawn, skill, breaker, config, lifecycle, gap, canary`) verbatim and forever.

If the only compaction candidates are spine records whose counts must be kept, compaction refuses
(`TROUBLE-LEDGER-009`) rather than sacrificing a count. Raise `disk_budget_bytes` or lower
`retention_compacted` instead.

## 5. Disk budget escalation

When `ledger/` exceeds `disk_budget_bytes`, escalation is ordered and each step is recorded:

1. compact the oldest eligible day;
2. expire payloads on the oldest generations, keeping all counts;
3. degraded payload mode — new `event`/`canary` records keep
   `{seq, ts, kind, sig, inc, origin, actor, redactions}` and their payload becomes
   `{"payload_dropped":true,"reason":"disk_budget"}`, with counters preserved
   (`lifecycle{op:"budget_exceeded"}`, `TROUBLE-LEDGER-010`);
4. only if even that cannot be written (ENOSPC) do producers get `TROUBLE-LEDGER-001`
   `reason=backpressure`.

Records are still written at every step: the audit chain never develops a hole.

## 6. Single-writer and satellites

One process appends to `ledger/**`: the hub. Satellites forward records and keep a local↔hub id map
in `idmap.jsonl`; they never mint canonical ids and are never canonical writers. Local ids are never
used as canonical keys off the satellite.

## 7. Storage tier

JSONL is canonical and the index is in memory. `modernc.org/sqlite` is **not** linked in v0.1; the
decision flips only when one of the four measured triggers in SPEC-01 §3.8 fires, and those
thresholds are code constants in `internal/ledger/tier.go` printed beside their live values by
`trouble ledger status --json`.
