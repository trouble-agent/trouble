# SPEC-01 — ledger: append-only audit ledger, durability and query index (trouble v0.1)

Spec: SPEC-01
Area prefix: TROUBLE-LEDGER
Package: `internal/ledger`
Consumed types: Record, RecordDraft, Origin, Actor, Sig, SigSource, Prefix, RecordKind, Incident, Group, GroupCounters, GapRecord, Evidence, Duration, IndexStats, QueryInfo, LedgerStatus, GroupStat, SourceAge, EvidenceBundle, RotationPolicy, RetentionPolicy, CompactionResult, PageToken, GenerationIndex, LedgerArchiveMarker
Local types: batch, part, dayEntry, seqOffset, groupRing, writerState, headFile, reader, ref, groupSort, pageWalk, sidecarRow
ACs: AC-6, AC-22, AC-24, AC-26, AC-30
PRD: §03, §04a, §06b, §11

## 1. Purpose

`internal/ledger` is the single durable store of trouble: one append-only JSONL ledger per host, written by
one process, indexed in memory, queried through one read API. It exists to make four promises true:

1. **AC-6 — auditability.** Every detection, play, research call, agent run, tool call, issue, board row,
   spawn, verify and skill change is a record; `seq` is strictly increasing with no silent holes, and no
   record is ever rewritten.
2. **Durability with a stated cost.** Group-commit fsync at ≤ 200 ms with an amortized cost of 1.94 µs/record
   (measured: 515k rec/s amortized vs 512 rec/s per-line fsync on the reference host), and one normative
   crash-loss sentence that every doc carries verbatim (§2.1).
3. **Bounded answers.** Top-N groups by rate/trend/count, incident lookup, group lookup by sig/digest,
   evidence assembly, per-source last-event-age and `ledger_last_seq` are answered from a bounded in-memory
   index in O(1) or O(log n) — never by scanning the file. A full scan of a 1 GiB ledger costs ~1.8 s at the
   measured 545 MB/s substring rate, so scanning cannot meet the dashboard's 50 ms render budget (AC-19,
   SPEC-10); the index is the mechanism, and it is rebuilt at boot inside a stated budget (§3.6).
4. **A single writer, forever.** Exactly one process appends to `ledger/**` (§3.4 LOCK). Satellites forward
   and keep a local↔hub id map; the hub is the only canonical writer (SPEC-12 §3.7, brief P).

**Non-goals.** The ledger is not a message queue, not a config store, not an analytics database, and not the
owner of ladder state: `Incident`/`Group` lifecycle decisions belong to SPEC-05 and SPEC-04 and arrive here
as records to be stored and indexed. The ledger never sees unscrubbed bytes (§4.2).

**Measured basis for every number in this spec** (reference host: kernel 7.0.0-30-generic, Go 1.26.5, ext4):

| Measurement | Value | Used for |
|---|---|---|
| fsync per record (p50 2.0 ms / p95 2.5 ms / max 5.0 ms) | 512 rec/s | why group-commit is mandatory |
| group-commit batched fsync | 1.94 µs/rec amortized → 515k rec/s | the durability design + regression floor |
| sentinel ingest, 100-line group-commit vs fsync-per-line | 6,199 req/s vs 1,972 req/s | budget headroom for the write boundary |
| substring scan of JSONL | 545 MB/s (22.5 MB in 40 ms) | index mandatory, not optional |
| binary size stdlib+godbus / +modernc sqlite | 8.36 MB / 12.05 MB (+3.69 MB) | §3.8 storage-tier decision |
| comparable fleet daemon (schedulerd, sqlite+) | 22.4 MB binary / 35.9 MB RSS | §3.8 RSS ceiling comparison |
| representative event | 1,064 B raw / 350 B gzip (3.0×) | retention/disk-budget arithmetic |
| volume at 1 M events/month | 1.1 GB raw / 0.3 GB gzip | §3.7 disk budget defaults |

## 2. Interface

### 2.1 Durability contract (normative wording — copy verbatim)

The following sentence is the **crash-loss window statement**. It is repeated verbatim in `README.md`,
`docs/operations.md`, the `trouble --help` ledger section, the em-dash block of `trouble config explain
ledger.*`, and it is exposed as `ledger_loss_window_ms` in `GET /health.json` and in `trouble ledger status
--json`. A docs test (`internal/ledger/docs_test.go`) greps the four files for the exact string; CI fails if
any copy drifts.

> **A record is durable the moment `Append` returns.** Records already written to the ledger file but not yet
> fsynced are lost on **power loss or kernel panic**; the maximum loss window is `ledger.fsync_window_ms`,
> default **200 ms**, measured from the last successful fsync completion. A process crash (SIGKILL), a daemon
> restart or a warm OS reboot loses nothing: those records are already in the running kernel's page cache.

### 2.2 Writer API (`internal/ledger`, public)

```go
package ledger

type Options struct {
    Root       string           // ledger dir; resolved from the state root (§3.9). 0700, never /tmp
    Rotation   RotationPolicy
    Retention  RetentionPolicy
    Index      IndexOptions
    Writer     types.Actor      // stamped into every record this process writes
    MaxSchema  int              // highest schema_version this binary understands (1)
    Now        func() time.Time // injectable clock; production = time.Now
}

func Open(ctx context.Context, o Options) (*Ledger, error)          // LOCK, recover, index — all three or fail
func (l *Ledger) Append(ctx context.Context, d types.RecordDraft) (types.Record, error) // durable on return
func (l *Ledger) Close(ctx context.Context) error                   // drain + fdatasync + HEAD + release LOCK
func (l *Ledger) Query() Query
func (l *Ledger) Status() LedgerStatus                              // O(1), no I/O, no lock
func (l *Ledger) IndexStats() IndexStats                            // O(1), snapshot
func (l *Ledger) Rotate(now time.Time) (part, error)                // between batches only (§3.5)
func (l *Ledger) Compact(ctx context.Context, day string, rp RetentionPolicy) (CompactionResult, error)
func (l *Ledger) Verify(ctx context.Context, day string) (VerifyReport, error) // read-only, no lock
```

`Append` fills `Seq`, `RecID`, `TS` and `SchemaVersion`; a draft that supplies any of them is refused
(TROUBLE-LEDGER-001, `payload.reason=validation`). The ledger is the only seq allocator (§3.5).

`VerifyReport` is package-local (returned by the CLI verb, never an HTTP body):

```go
type VerifyReport struct {
    Day string `json:"day"`; Files []string `json:"files"`; Lines int64 `json:"lines"`
    SeqHoles []struct{ From, To uint64 } `json:"seq_holes"`
    TornLines, CorruptLines, NewerSchema int `json:"torn_lines,corrupt_lines,newer_schema"`
    PayloadHashMismatches int `json:"payload_hash_mismatches"`
    TombstoneMismatches int `json:"tombstone_mismatches"`; Counts GroupCounters `json:"counts"`
    OK bool `json:"ok"`; Errors []string `json:"errors"`
}
```

### 2.3 Read API — the query surface (mandate d: each answer O(1)/O(log n), never O(file))

```go
type Query interface {
    TopGroups(n int, window Duration, sort groupSort) ([]GroupStat, QueryInfo, error) // count|rate|trend
    Incident(id string) (*types.Incident, QueryInfo, error)      // by inc_ id
    IncidentBySig(sig string) (*types.Incident, QueryInfo, error) // open incident for a sig (dedup/reopen)
    GroupBySig(sig string) (*types.Group, QueryInfo, error)
    GroupByDigest(hex32 string) (*types.Group, QueryInfo, error)
    Evidence(inc string, maxRecords int) (EvidenceBundle, error)  // one incident's audit chain + gaps
    Sources() ([]SourceAge, QueryInfo, error)                     // per-source last-event-age (verification)
    CounterDeltas(since, until string) (map[string]int64, QueryInfo, error) // per-source counter deltas
    Gaps(since string, limit int) ([]types.GapRecord, QueryInfo, error)
    RecordsInSeqRange(from, to uint64, limit int) ([]types.Record, error)
    ScanFrom(seq uint64, yield func(types.Record) bool) error     // bounded streaming, stops on false
}
```

| Call | Answers | Index structure | Complexity |
|---|---|---|---|
| `TopGroups` | groups ranked by count / rate_1m / rate_5m / trend | `groups map[digestHex]*GroupStat` + per-group 60×1-min ring with incrementally maintained `rate1m/rate5m/rate60m`; top-N via a bounded min-heap sieve | O(G·log n), G = live groups ≤ `index.max_groups` |
| `Incident` | incident by id | `incidents map[incID]*Incident` | O(1) |
| `IncidentBySig` | the open incident for a sig (reopen path, AC-22) | `openBySig map[sigShort]incID` | O(1) |
| `GroupBySig` | group by sig string | `sigToDigest map[sigShort]digestHex` → `groups` | O(1) |
| `GroupByDigest` | group by the full dedup digest | `groups` (key = 64-hex digest) | O(1) |
| `Evidence` | ordered audit chain for one incident + its gaps | `refsByInc map[incID][]ref` (bounded, `index.inc_per_incident`) + `Resolve(seq)` per element | O(k·(log D + log S + 512)) |
| `Sources` | per-source last-event-age, counters, canary, zone | `sources map[hostID":"source]*SourceAge` | O(S), S ≤ 5,000 |
| `CounterDeltas` | per-source deltas for an evidence-tuple window | two counter snapshots taken at window start/end over `sources` | O(S) |
| `Gaps` | gap records (kind `gap`) since a ts | `gapsBySource` + bounded global gap ring (4,096 entries) | O(log + k) |
| `LastSeq`/`Status` | `ledger_last_seq` for the stall checker (O) | scalar watermark updated after each fsync | O(1), no I/O |
| `RecordsInSeqRange`/`ScanFrom` | readers-by-offset over seq | day index + sparse `seqOffset` samples every 512 lines | O(log D + log S + lines) |

`D` = indexed days, `S` = sparse samples per file, `k` = refs. Every query returns a `QueryInfo`; when the
answer is outside the indexed window the **cold-read path** applies (§3.6) and `QueryInfo.Partial=true` with
the scanned byte/line counts — an answer is never silently truncated and never an unbounded scan.

### 2.3a Pagination contract — page tokens, `next_page_token`, and bounded walks

Every ledger list query accepts `page_token` + `page_size` and returns `next_page_token` (SPEC-10's
`/incidents` and `/groups` take the same two parameters — §2.3a is the one pagination dialect in the
suite). A walk is bounded, resumable and O(page): never O(file), never O(corpus), and never an unbounded
scan when a page is the wrong size.

| Parameter | Default | Max | Rule |
|---|---|---|---|
| `page_size` | 500 | 5000 | records per page; absent or `0` means the default; above the max it is **clamped**, not refused — a page size is a display choice, not a policy |
| `page_token` | `""` | — | `""` starts a newest-first walk; any other value must parse as the token grammar below |

**Token grammar**: `{generation_file}::{byte_offset}::{seq}` — e.g.
`2026-09-16.1.gen.jsonl::1835008::41207`. A token is **opaque** to clients (they never construct one),
**stable within a generation's lifetime** (the same token returns the same page while that file exists),
and **invalidated cleanly when a generation is dropped** (§3.7a): the answer is a `reset` hint plus a
newest-first restart, never an error loop. `GenerationIndex` (§3.7a) is what makes a token cheap to honour
— `byte_offset` is a real offset into a real file, and `seq` is the resume point if the file changed under
the walk (a part rollover, a compaction).

```go
// Query additions; PageToken is defined in SPEC-TYPES §3.15.1 and QueryInfo carries the page outcome.
Page(kind types.RecordKind, token types.PageToken, size int) ([]types.Record, types.QueryInfo, types.PageToken, error)
PageIncidents(token types.PageToken, size int) ([]types.Incident, types.QueryInfo, types.PageToken, error)
```

| Situation | Answer | `QueryInfo` |
|---|---|---|
| normal page | `page_size` records (fewer only at the end of the walk) plus `next_page_token` | `Indexed=true`, `Reset=false` |
| walk reached the oldest record | the final page with `next_page_token=""` — finished, not truncated | `Indexed=true` |
| token names a dropped generation | newest-first first page + `reset` hint | `Reset=true`, `ResetReason="generation_dropped"` |
| token is unparsable, or names another host's generation | newest-first first page + `reset` hint | `Reset=true`, `ResetReason="token_invalid"` / `"token_foreign"` |
| the page comes from the cold-read path (§3.6) | the page is served from the sidecar + the file | `Partial=true` with scanned byte/line counts, still bounded by `page_size` |

**What pagination is not.** It is not a cursor into a live mutation stream — `since=<seq>` on the dashboard
partials keeps that job (SPEC-10 §2.6) — and it is not an ordering contract: a walk is newest-first, and a
token continues that order even across a compaction. `/groups` ranked by rate reads only the in-memory
index (SPEC-10 §2.9's no-scan rule) and uses a token to continue a long list, never to re-rank.

### 2.4 CLI seams (read-only by default; every mutating verb has `--dry-run`)

| Command | Behaviour | Exit codes |
|---|---|---|
| `trouble ledger status --json` | prints `LedgerStatus` (durable watermark, queue depth, index stats, loss window) | 0 ok, 1 degraded |
| `trouble ledger verify [--day D]` | read-only walk: seq continuity, line parse, payload hash, tombstone totals; opens **no** lock | 0 ok, 1 findings, 2 unreadable |
| `trouble ledger compact --day D [--dry-run]` | dry-run prints a `CompactionResult` with `ToFile` and byte deltas and writes nothing | 0 ok, 1 failed |
| `trouble ledger reindex [--rebuild-store]` | re-runs the boot index build; `--rebuild-store` is meaningful only under the SQLite tier (§3.8) and is a no-op otherwise | 0 ok, 1 failed |

### 2.5 Config surface (`[ledger]`, precedence `flag > env > file > default`, SPEC-12 §3.4)

```toml
[ledger]
dir                  = ""              # "" → <state_root>/ledger ; must be 0700, refuses /tmp
fsync_window_ms      = 200             # ≤200ms crash-loss window (stated, §2.1)
max_batch_records    = 4096
queue_cap_records    = 65536
max_enqueue_wait     = "5s"            # exceed → TROUBLE-LEDGER-001 reason=backpressure
fdatasync            = true            # fdatasync is sufficient for an append-only file; fsync fallback
max_record_bytes     = 262144
rotate               = "daily"         # only value in v0.1
rotate_max_bytes     = 536870912       # intra-day part rollover (p02, p03, …)
index_hot_days       = 7
index_build_budget_ms   = 9000
index_build_budget_bytes = 536870912
index_min_scan_rate  = "60MiB/s"       # projection decode floor; the build fails over the budget, not below the floor
index_max_groups     = 50000
index_max_incidents  = 50000
index_max_sources    = 5000
index_inc_per_incident = 512
index_incident_ring  = 4096
index_max_bytes      = 67108864        # 64 MiB in-memory index ceiling (within the 80MB RSS budget)
cold_read_max_bytes  = 67108864
cold_read_max_lines  = 200000
payload_ttl          = "720h"
retention_raw        = "72h"
retention_compacted  = "8760h"
page_size_default    = 500             # §2.3a
page_size_max        = 5000            # above this a page size is clamped, never refused
offset_stride        = 256             # sidecar sample interval: a page reads ≤ stride + page_size lines
compaction_interval  = "24h"
compaction_min_age   = "24h"
compaction_min_bytes = 8388608
disk_budget_bytes    = 2147483648
disk_warn_pct        = 80
assert_scrubbed      = true
```

## 3. Data model

### 3.1 The record (mandate: the exact field list)

The wire shape is `Record` in SPEC-TYPES §3.2 — it is the single definition and is not restated here. The
ledger's obligations on it:

| Field | Ledger rule |
|---|---|
| `seq` | allocated here, strictly increasing, never reused (§3.5) |
| `rec_id` | `ev_` + ULID minted here; on a satellite, the hub mints the canonical `rec_id` and the local one is kept in `idmap.jsonl` (§3.2) |
| `ts` | `types.NowUTC()` at append; RFC3339 UTC, millisecond precision, always `Z`; never rewritten, never re-derived from `seq` |
| `kind` | one of the 17 `RecordKind` values; unknown value → refused (TROUBLE-LEDGER-001) |
| `schema_version` | `1` in v0.1; a record whose version exceeds `Options.MaxSchema` is **skipped and counted** (`IndexStats.SkippedNewerSchema`), never fatal (TROUBLE-LEDGER-004) |
| `sig` | `""` for non-sig-keyed kinds; may not be `""` for `event`, `incident`, `verify`, `play_run` |
| `inc` | `inc_` + ULID when the record belongs to an incident; drives `refsByInc` |
| `origin` | **never empty** (`host_id` and `source` required) — enforced at the write boundary from day one (brief P) |
| `actor` | never empty; carries binary `version`/`git_sha`/`build_time` (judge D3) |
| `redactions` | copied from the scrubber's count; **count only, never the value** (brief C). A record with `redactions > 0` is never re-scrubbed on read |
| `payload` | `map[string]any`, schema owned per kind by the emitting spec (§4.1) |

**Common payload envelope keys** (reserved; a kind-specific schema may not redefine them):
`op` (ledger/compaction/internal operation name), `error_code` (the SPEC-INDEX §5.3 ledger mirror),
`merge_key` (§3.3), `subject`, `zone`, `text`, `truncated`, `truncated_bytes`,
`payload_sha256` (mandatory when the canonical payload JSON is ≥ 4096 bytes: `hex(sha256)[:16]`),
`payload_ttl_expired`, `payload_dropped`, `local_id`, `alias_sig` (the sig that produced this sig, e.g.
journald→sentinel merge), `replaced_invalid_utf8`.

**Serialization rules** (all four are conformance-tested): one JSON object per line, `\n`-terminated, struct
field order, `json.Encoder` with `SetEscapeHTML(false)` (Go's default `\u003c` escaping would corrupt stack
traces in the audit artifact and inflate lines by up to 6×); invalid UTF-8 bytes replaced with U+FFFD and
flagged (SPEC-02 owns the rule, TROUBLE-SCRUB-007); newlines inside string values are `\n`-escaped by the
encoder so a record is always exactly one line.

**Size caps.** A canonical payload larger than `max_record_bytes` is **refused** (TROUBLE-LEDGER-001) for the
audit-spine kinds `incident, group, verify, play_run, agent_run, tool_call, research, issue, flow, spawn,
skill, breaker, config, lifecycle` — the producer splits or summarises, so the audit chain cannot be quietly
mutilated. For the high-volume kinds `event, canary, gap` the payload is **truncated** to the cap with
`payload.truncated=true` and `payload.truncated_bytes=N`.

### 3.2 Identity: prefixes, ULID allocation, canonical-id authority (mandate g, brief P)

Prefixes are `types.Prefix` (SPEC-TYPES §3.1) and are frozen: `inc_ grp_ iss_ tsk_ sk_ ev_ res_ sp_`.
`ev_` is the ledger's own (`rec_id`); the ledger mints no other prefix — `inc_/grp_/iss_/tsk_/sk_/res_/sp_`
arrive from SPEC-05/SPEC-04/SPEC-09/SPEC-08/SPEC-11/SPEC-07 through `RecordDraft`.

`types.NewID(prefix)` allocation rules (pinned here because this spec owns identifiers):

1. ULID = 48-bit millisecond timestamp + 80-bit randomness, Crockford base32, 26 characters, uppercase.
2. **Monotonic per process.** If the current millisecond is ≤ the last used millisecond, the timestamp part
   is held and the 80-bit random part is incremented as a big-endian integer; on overflow the millisecond is
   advanced by one and randomness restarts at zero. Therefore ids minted by one process are strictly
   increasing in string order and already sortable by mint time.
3. **Never reused.** Randomness is seeded from `crypto/rand` at process start and every allocation is unique;
   the monotonic counter is in-memory only, so a restart cannot replay an id. An id observed twice is
   corruption (TROUBLE-LEDGER-011).
4. Allocation happens before serialization; an id is never re-issued for a retried record (the same
   `RecordDraft` retried keeps its id only if the caller holds the returned record — otherwise a retry is a
   new record, which is correct for an audit log).
5. Interning: ids are stored exactly once, as the string form, in `rec_id`/`inc`/etc. No id is ever
   embedded inside a payload as an untyped string except under the reserved `local_id` key.

**Canonical-id authority (T5-enabler).** The hub mints canonical ids. A satellite mints *local* ids for its
own spool and records, forwards records through `ForwardEnvelope` with `payload.local_id`, and appends the
hub's answer `[{local_id, hub_id, ack_seq}]` to `<state-root>/ledger/idmap.jsonl` (append-only, same
group-commit durability, same torn-line recovery). Local ids are never used as canonical keys off the
satellite; the canonical identity of any record in the federation is the tuple
`(host_id, hub-implicit seq, hub-minted rec_id)`. A satellite re-sends idempotently on the key
`sig | norm_version | host_id` (SPEC-TYPES `ForwardEnvelope.IdempotencyKey`), so a hub restart cannot
double-count.

### 3.3 Signature normalization, test vectors, and the cross-source merge key (mandate f)

**Canonical sig string** (SPEC-TYPES §6.3): `<source>:<algo>v<norm_version>:<hex16>` where
`algo = "sha256"`, `norm_version = 1` in v0.1, and `hex16 = hex(sha256(normalized_bytes))[:16]`. The full
32-byte digest is the grouping/dedup truth; the 16-hex form is for display, the ledger `sig` field, leases
and dedup keys. Changing any normalization function below **requires** `norm_version = 2`; two norm versions
are two signature spaces and are never merged (SPEC-TYPES §6.3).

**Framing** (identical for every source, applied to post-scrub values):
`normalized_bytes = source + "\x1f" + field₁ + "\x1f" + field₂ … + "\x1e"` — UTF-8, fields joined by U+001F
(unit separator), terminated by a single U+001E (record separator), no trailing newline. Every field value
is pre-cleaned: `\x00`, `\x1f` and `\x1e` inside a value are replaced by one space, and the value is trimmed.
Empty trailing fields are dropped. `norm_version = 1` also pins `message_norm_max_lines = 3` and the 512-byte
message budget.

**`message_norm` (norm_version 1)**, applied to an already-scrubbed, already-assembled message (SPEC-04 owns
multi-line assembly): split on `\n`; drop blank lines; keep the first 3; per line, in this order —
(a) `\b0x[0-9a-fA-F]+\b` → `<hex>`; (b) UUID `[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}` → `<uuid>`;
(c) `[0-9a-fA-F]{8,}` → `<h>`; (d) `[0-9]+` → `#`; (e) whitespace runs → one space, trim. Join the kept lines
with `\n` and truncate to 512 bytes at a UTF-8 rune boundary. Steps are ordered a→e and are re-entrant
(the output of one pass is a fixed point of the next).

**Per-source field lists (norm_version 1).** Every source SPEC-03/SPEC-04 references:

| Source | Fields (joined by `\x1f`) | Subject used for `merge_key` | `class` |
|---|---|---|---|
| `sentinel` | `project_slug`, `culprit_norm`, `fingerprint_or_stackhash` (SDK fingerprint strings joined by `\x1f`; when absent, the canonical stack hash hex16) | `project_slug:culprit_norm` | taxonomy from the sentinel fingerprint/culprit table |
| `journald` | `unit_or_ident` (`_SYSTEMD_UNIT`, else `SYSLOG_IDENTIFIER`, else `_COMM`), `message_norm` | `unit_or_ident` | taxonomy from the journald priority+message table |
| `dbus` | `manager` (`system` \| `user:<uid>`), `unit` (real unit name, **not** the escaped object path), `state` (`SubState` for PropertiesChanged, `job:<Result>` for JobRemoved) | `unit` | `crash_loop` for `failed`/`auto-restart`, `unit_degraded` otherwise |
| `psi` | `scope` (`cpu`\|`memory`\|`io`), `signal` (`some`\|`full`), `bucket` (`b1`..`b4`) | `scope` | `resource_exhaustion` |
| `disk` | `mount`, `kind` (`free_pct`\|`inode_pct`\|`readonly`\|`io_error`) | `mount` | `disk_full` for `free_pct`/`inode_pct`, `io_stall` for `io_error`, `config_error` for `readonly` |
| `timers` | `timer_unit`, `kind` (`missed`\|`skew`\|`inactive`) | `timer_unit` | `timer_missed` |
| `inotify` | `path_norm` (absolute, cleaned), `class` (`create`\|`modify`\|`delete`\|`move`\|`attrib`) | `path_norm` | `file_flap` |
| `collector` | `collector_name` (`go-panic`\|`py-traceback`\|`node-reject`), `parser_class`, `message_norm` | `parser_class` | `crash_loop` |
| `generic` | `project_slug`, `app_slug`, `message_norm` | `app_slug` | `unknown` |
| `unknown` | `raw_hash16` = `hex(sha256(first 512 bytes of the raw body))[:16]` | `raw_hash16` | `unknown` |

PSI `bucket` boundaries (norm_version 1, evaluated on the **sampled** `avg10` — sampling is the source of
truth, triggers are only an accelerator): `b1 < 10.00`, `b2 ∈ [10.00, 25.00)`, `b3 ∈ [25.00, 50.00)`,
`b4 ≥ 50.00`. The wake path re-reads counters before bucket selection, so a wake that crosses no boundary
produces the same sig as the sample it agrees with.

**Published test vectors — bytes → sha256 → 32-byte digest → 16-hex short.** `\x1f`/`\x1e` are shown
literally; trailing `\x1e` is part of the hashed bytes.

```
input bytes (between the quotes)                                                    -> full digest                                                        -> short
"psi\x1fcpu\x1fsome\x1fb3\x1e"                                                      -> d8d1d8cc6f8c431d1b99197c3a52134a0a7d037e8ab9aa3bce221bbf5ddc7296 -> d8d1d8cc6f8c431d
"journald\x1fpayment-worker\x1fqueue wedge: pool exhausted, retry # in # ms\x1e"     -> 8721b2ea46c6cf26b09fe04f73f41b05fe3660e5c21ddefdbae6686708a13e4a -> 8721b2ea46c6cf26
"dbus\x1fuser:1000\x1fpayment-worker.service\x1fauto-restart\x1e"                    -> 2b180920160f1ea8a68a3cd63856212c1e78d01abc9e18572b5d0be76f7cea3e -> 2b180920160f1ea8
"disk\x1f/\x1ffree_pct\x1e"                                                         -> ca5ae505ad8384448d929b5e4a8d7414f8d77db1973115723736c5ce25138936 -> ca5ae505ad838444
"timers\x1fbackup.timer\x1fmissed\x1e"                                              -> e529b7377e31655e90f5987023d0d8bb626616b41605939cd6b2e6429877dcc5 -> e529b7377e31655e
"inotify\x1f/etc/payment/config.toml\x1fmodify\x1e"                                 -> 7ebd19985bf29592ac9e990c0703028cb3ad0fa4f32a983cc53fb47c62939c2f -> 7ebd19985bf29592
"sentinel\x1f1\x1fworker.claim\x1fqueue-wedge\x1e"                                  -> 18b12eea8a3929236822338d1b8d04055a2a670104b97782356e1559fc83f385 -> 18b12eea8a392923
"sentinel\x1f1\x1fworker.claim\x1fb7e2d91c4a3f0e58\x1e"                             -> 3cd6cf21bc4259ea71157d42b218c8505134432d7384436a35ca0c907af22eff -> 3cd6cf21bc4259ea
"collector\x1fgo-panic\x1fpanic\x1fpanic: runtime error: invalid memory address or nil pointer dereference\n[signal SIGSEGV: segmentation violation code=<hex> addr=<hex> pc=<hex>]\x1e"
                                                                                    -> bce03e7abb085b0f963ca3b6fc4348112ebe7d93c316c4f7abd255dee97f498e -> bce03e7abb085b0f
"generic\x1f1\x1flegacy-cron\x1fconnection refused to db host #.#.#.#:#\x1e"         -> f464cc58cfa080a66693c8cf2ae5898d76a74a1d9fbc65f62f685ba67fca616b -> f464cc58cfa080a6
"unknown\x1f17d04f75658547e6\x1e"  (inner = hex16 of sha256('{"level":"error","msg":"boom"}'))
                                                                                    -> b52b9311988a6ef45221311ebebc52a0ed24016b9089bfeab674baae7a084b2e -> b52b9311988a6ef4
"trouble.merge.v1\x1fcrash_loop\x1fpayment-worker.service\x1e"                      -> bcc48494f2190f4f5a4408fe3358445cd887856d386d72fec70ef73039f196f5 -> bcc48494f2190f4f
"trouble.merge.v1\x1fresource_exhaustion\x1fio\x1e"                                 -> a11a6c52eab58b3ae2fc3d2518da3c0ba990dd1352338e4502b7a58430751576 -> a11a6c52eab58b3a
"trouble.merge.v1\x1fdisk_full\x1f/\x1e"                                            -> 8b3c2119f8410f15eacfff1f4b4ef775ac69a91e896c3a183518b46e1a0a1980 -> 8b3c2119f8410f15
```

**Cross-source merge key (AC-22's mechanism).** Sources produce source-scoped sigs by construction, so a
group cannot be keyed on the sig alone when one bug arrives through a sensor, the sentinel and a collector.
The shared function is:

```go
func MergeKey(class, subject string) string  // hex16(sha256("trouble.merge.v1\x1f" + class + "\x1f" + subject + "\x1e"))
```

`subject` resolution order (first hit wins, never blocks): (1) `payload.subject` supplied by the producer;
(2) the `[dedup.subjects]` config table (source-scoped pattern → subject, e.g. project slug → unit name);
(3) fallback = the source-scoped sig short, which yields a single-source group by definition. `class` comes
from the table above. SPEC-05's dedup core groups on `merge_key` when present and on `sig` otherwise, which
is how three arrival paths for one unit condition (PropertiesChanged + JobRemoved + journald) collapse into
one group and one incident. The ledger stores `merge_key` in the payload envelope and indexes it, so the
merge decision is auditable after the fact.

### 3.4 File set, naming, generations, LOCK and HEAD (mandate b)

```
<state-root>/ledger/                     0700, never /tmp
├── LOCK                                 flock(LOCK_EX|LOCK_NB) held for the daemon's life; contents {"pid","version","git_sha","boot_ts","root"}; 0600
├── HEAD                                 seq hint written tmp+rename+fsync(dir); {"last_seq","day","gen","part","bytes","ts","version","git_sha"}; 0600
├── YYYY-MM-DD.jsonl                     generation 0 = the day file, part 01 (implicit part number 1)
├── YYYY-MM-DD.pNN.jsonl                 part NN (02..) when a day exceeded rotate_max_bytes
├── YYYY-MM-DD.N.gen.jsonl               generation N (N ≥ 1) produced by compaction; authoritative for that day
├── idmap.jsonl                          satellite only: local→hub id mapping; same durability + recovery; 0600
└── quarantine/                          files refused by the version gate or failed verification; 0700
```

Naming rules, all regex-checked at boot (`^(\d{4}-\d{2}-\d{2})(?:\.p(\d{2}))?(?:\.(\d+)\.gen)?\.jsonl$`):

1. Days are UTC. Rotation happens at `00:00:00Z` (`rotate = "daily"`, the only cadence in v0.1) **between
   batches** — a batch never spans two files (§3.5).
2. A day that exceeds `rotate_max_bytes` continues in `.pNN` parts, in part order. Part numbers are
   two-digit, ascending, and never reused.
3. **One authoritative content per day**: for day `D`, the highest existing generation wins. Generation `N`
   is written as `D.N.gen.jsonl` after the parts are folded in; parts and lower generations are unlinked
   only after the new generation is fsynced and the directory fsynced. Boot that finds `D.jsonl` and
   `D.1.gen.jsonl` together concludes an interrupted compaction commit, prefers generation 1, unlinks
   generation 0, and emits `lifecycle{op:"compaction_resume"}` (TROUBLE-LEDGER-007 class, no data change).
4. Plain JSONL only — no compression. The ledger is the audit artifact and must stay `jq`/`grep`/`tail`-able
   (the fleet's jq-law); compression lives in `backups/` (SPEC-12) and never in `ledger/`.
5. `quarantine/` receives a file moved aside only by an operator command or by the version gate's explicit
   `--quarantine` action; the daemon itself never moves data files.
6. `LOCK` is the single-writer mechanism (brief B): a second `Open` on the same directory gets
   `EWOULDBLOCK` → TROUBLE-LEDGER-005 and the process exits non-zero. `trouble ledger verify` opens
   read-only and takes no lock. Two daemons sharing one state root are rejected at boot (SPEC-INDEX §6.4).
7. `HEAD` is a **hint, never a source of truth**: recovered `seq_alloc = max(HEAD.last_seq, highest seq in
   the highest-generation file) + 1`. A corrupt or missing HEAD costs one bounded tail read, not a rebuild.

### 3.5 seq allocation and readers-by-offset semantics (mandate c)

**Allocation.** `seq` is one global counter per ledger (not per file), so it is strictly increasing within
each file *and* across every rotation and compaction — which is what makes it usable as the stall checker's
signal (brief O) and as a permanent locator. The writer goroutine is the only allocator: it holds the
counter, assigns `seq = last + 1` for each record of the batch, then serializes, writes, fdatasyncs, and only
then publishes `LastSeq`. Consequences, pinned:

- A record is written with its final seq; seq is never renumbered and never rewritten.
- Allocation happens inside the batch, so a batch's seqs are contiguous and a `\n`-terminated line order
  matches seq order exactly.
- A permanent write failure after allocation leaves a hole. The counter never goes backwards; the hole is
  reported (TROUBLE-LEDGER-002 on the next read) and a `lifecycle{op:"hole", from, to}` record is written
  with the next successful batch. Every hole therefore appears in the audit trail.
- `LastSeq` = the highest **fsynced** seq (the durable watermark). `LastSeqPending` = the highest allocated
  seq. The stall checker alarms on `LastSeq`, so "seq advanced" implies "durable".

**Readers by offset.** A reader is `(day, gen, part, byteOffset)` resolved at open, plus seq as the stable
locator. Rules:

1. **seq is the stable locator; byte offsets are not stable across a generation rewrite.** Every long-lived
   reader stores a seq (or a wall-clock ts, which it maps to seq through the day index), never a raw offset.
2. `Resolve(seq)` → (file, offset) is O(log D + log S): binary-search the day index, then the sparse
   `seqOffset` samples (one per 512 lines), then scan forward at most 512 lines. It resolves *across* files,
   so following is seamless at rotation.
3. Readers take no locks and may read a file the writer is appending to. A reader that meets a torn final
   line skips it and re-reads that offset after the next append, when the line is complete.
4. **Rotation**: the writer fdatasyncs and closes the old file before creating the new one; a reader holding
   the old fd reads to EOF, then re-`Resolve(seq)` and continues in the new file. Rotation is legal only
   between batches.
5. **Compaction**: the pre-compaction inode is unlinked, never modified in place, so a reader holding that fd
   completes its read on a stable, consistent image. A reader that must continue afterwards re-`Resolve(seq)`
   against the new generation. Because offsets shift in a rewrite, rule 1 is what keeps such a reader correct.
6. `ScanFrom(seq, yield)` is the only supported bulk walk; it is bounded by `cold_read_max_bytes` /
   `cold_read_max_lines` and returns `QueryInfo.Partial=true` plus the scanned counts when it hits them.
   There is no API that reads "the whole ledger" into memory — 1 GiB at the measured 545 MB/s scan rate is
   1.8 s and would blow the 80 MB RSS budget.

### 3.6 The in-memory index: structures, budget, degradation (mandate e)

Rebuilt at boot by one sequential pass over the authoritative file set in seq order. Every structure is
bounded; there is no unbounded structure in the design.

| Structure | Key | Bound | Purpose |
|---|---|---|---|
| `groups` | 64-hex digest | `index_max_groups` (50,000) | counts, rates, trend, counters (SPEC-TYPES `Group`/`GroupCounters`) |
| `groupRing` | digest | 60 × uint32 per group | 1-minute buckets; `rate1m/5m/60m` maintained incrementally, trend = current vs previous 5m |
| `sigToDigest` | sig short string | `index_max_groups` | `GroupBySig` in O(1) |
| `incidents` | `inc_` id | `index_max_incidents` (50,000) | incident projection (SPEC-05's decision, stored) |
| `openBySig`, `openByMergeKey` | sig / merge_key | ≤ open incidents | dedup + reopen (AC-22) |
| `incidentRing` | `UpdatedTS` desc | `index_incident_ring` (4,096) | dashboard recent-first list without sorting everything |
| `refsByInc` | `inc_` id | `index_inc_per_incident` (512) + `Overflow` counter | evidence assembly |
| `sources` | `host_id:source` | `index_max_sources` (5,000) | last-event-age, counters, canary, zone |
| `gapsBySource` + global gap ring | source / ts | 4,096 | `Gaps()`, gap accounting for verification |
| `days` | day | one entry per day (≤ 400) | `Resolve(seq)`, disk budget, retention |
| `seqOffset` samples | day file | 1 per 512 lines | `Resolve(seq)` in O(log S + 512) |
| counters | — | scalars | `LastSeq`, `LastSeqPending`, per-source totals, `FsyncCalls`, queue depth |

**Boot rebuild budget.** `index_build_budget_bytes = 512 MiB` and `index_build_budget_ms = 9000`, with a
required projection-decode floor of `index_min_scan_rate = 60 MiB/s` (pure-Go token walk that decodes only
the fields the index needs and skips `payload` bytes). 512 MiB ÷ 60 MiB/s = 8.5 s, so the two limits agree;
the typical boot is far cheaper because compaction has collapsed every day older than `compaction_min_age`
into group-aggregate lines, leaving at most `retention_raw` (72 h) of raw events to decode — at the typical
33 MB/day that is ~100 MB of raw plus tens of KB per compacted day. `index_max_bytes = 64 MiB` caps the
resident index (within the 80 MB steady-RSS budget, brief H).

**Degradation ladder — never an unbounded scan** (ordered, first applicable rung wins; every rung sets
`IndexStats.Degraded=true` with `DegradedReason`, writes a `lifecycle` record, surfaces in `/health.json`,
and increments a counter):

1. **Full rebuild** when projected bytes ≤ `index_build_budget_bytes` and the projection rate holds.
2. **Tiered rebuild** when projected bytes exceed the byte budget: index the hot window
   (`index_hot_days`) fully; for older days index only the non-`event` kinds (`group, incident, verify, gap,
   canary, lifecycle, config`), skipping `payload`. Reason `budget_exceeded`.
3. **Bounded cold index** when rung 2 still exceeds the budget: index the newest `index_hot_days` of
   summaries only, set `TruncatedBeforeTS` to the oldest indexed ts, and answer out-of-window queries through
   the cold-read path. Reason `budget_exceeded`.
4. **Group eviction**: when `groups` reaches `index_max_groups`, the least-recently-seen group with no open
   incident goes *cold* — its row is replaced by a per-day aggregate with counts preserved, `Cold=true`, and
   `ColdEvictionsPerMin` is exposed. Cold groups are still queryable through the cold read.
5. **I/O or parse failure during the build** (rung applies at any level): keep the partial index, mark the
   affected files and `DegradedReason=io_error`, and raise TROUBLE-LEDGER-008. The daemon still starts: a
   degraded index is visible, a failed start is not.

**Cold read.** A query whose answer is outside the indexed window reads the file set with a hard cap of
`cold_read_max_bytes` (64 MiB) and `cold_read_max_lines` (200,000), in seq order, from the newest day
backwards. On cap it returns the partial answer with `QueryInfo.Partial=true`, `Reason` and
`ScannedBytes`/`ScannedLines`. No call site can request more.

### 3.7 Rotation, retention, compaction, TTL, tombstones, disk budget (mandate h)

**What compaction is.** For a day older than `compaction_min_age` and at least `compaction_min_bytes`,
compaction writes **one new generation file** `D.N.gen.jsonl` (never in place) and unlinks the parts and the
previous generation after fsync. Content rules:

1. **Counts are never dropped.** Every `event` line is folded into one aggregate line per group per day
   (kind `group`, `payload.op="compaction_aggregate"`, `payload.aggregate=true`) carrying
   `GroupCounters{events, suppressed, redacted_values, dropped_events, sample_rate}` plus the day's first/last
   ts and the group's first/last seq. `tombstone_counts_kept = true` is asserted by test (§7) and reported in
   `CompactionResult`.
2. **Audit-spine records are copied verbatim** and never expire:
   `incident, group, verify, play_run, agent_run, tool_call, research, issue, flow, spawn, skill, breaker,
   config, lifecycle, gap, canary`. Their payloads stay, so the AC-6 chain survives compaction and
   `Evidence(inc)` keeps working on any age of the ledger.
3. **Payload TTL** applies to `event` and `canary` only: a line older than `payload_ttl` is rewritten with
   its payload replaced by `{"payload_ttl_expired":true,"counters":{…}}` — identity, sig, origin, actor,
   ts, seq, redaction count and counters are preserved, so the record stays countable and auditable.
   `CompactionResult.PayloadsExpired` counts them.
4. Under disk pressure only, payload dropping extends to `event` payloads younger than `payload_ttl`
   (`payload_dropped=true`, `CompactionResult.PayloadsDropped`), and never to the spine kinds.
5. Compaction never changes `seq`, `ts`, `rec_id`, `sig`, `origin`, `actor` or `redactions`; it may only
   aggregate `event`/`canary` lines (which consume the aggregate's `seq` list in `payload.agg_seqs`:
   `[first..last]` plus explicit exceptions) and expire payloads. `trouble ledger verify` re-derives the
   totals from the generation file and compares them with the pre-compaction totals.
6. **Retention**: `retention_raw = 72h` (days kept uncompacted; compaction runs on anything older),
   `retention_compacted = 8760h` (generation files kept for a year), enforced at boot and on a
   `compaction_interval = 24h` timer. Expiry of a compacted day happens only after its group/incident
   aggregates have been rolled into the incident's own records — an incident's `Evidence` never loses its
   spine. Deletion is **always whole-generation** (file + `.idx` + archive marker state, §3.7a): a day is
   dropped, never edited, and under the `light-hub` profile the drop waits for a verified DuckBrain export
   (SPEC-13 §3.5, TROUBLE-HUB-014).

**Disk budget.** `disk_budget_bytes = 2147483648` (2 GiB) for `ledger/`, warn at `disk_warn_pct = 80`.
Escalation order when exceeded: (1) compact the oldest uncompacted eligible day; (2) force
`payload_ttl → 0` on the oldest generations (rule 3/4), keeping all counts; (3) if the budget still holds,
enter **degraded payload mode**: new `event`/`canary` records keep `{seq, ts, kind, sig, inc, origin, actor,
redactions}` and their payload becomes `{"payload_dropped":true,"reason":"disk_budget"}`, with counters and
gauges preserved — records are still written, so AC-6 holds and the Rung-1 audit chain never develops a hole.
(4) If even that cannot be written (ENOSPC), raise TROUBLE-LEDGER-010 and refuse new work at the producer
boundary via the backpressure path. Arithmetic for the defaults: 1 M events/month ≈ 33 MB/day raw → 72 h of
raw ≈ 100 MB, plus ≤ 50 MB of compacted aggregates for a year; a 10× chatty host reaches ~1 GB, still inside
the 2 GiB budget.

### 3.7a Generation sidecar index (`.idx`), pagination, and drop-generation retention

**Per-file footer index.** Every closed generation file gets a sidecar `{file}.idx`, written at rotation
(and for the new generation at compaction) by the writer that owns the file, fsynced **before** the file is
announced as authoritative:

```go
type GenerationIndex struct {          // SPEC-TYPES §3.15.1 — one JSON object per sidecar, mode 0600
    File         string   // "2026-09-16.1.gen.jsonl"
    Records      int64
    Bytes        int64
    FirstSeq     uint64
    LastSeq      uint64
    MinTS        string
    MaxTS        string
    Offsets      []int64  // byte offset every OffsetStride records
    OffsetStride int      // default 256 (ledger.offset_stride)
    TornLines    int
    Sha256       string   // hex of the file's bytes at close; the archive marker id derives from it
}
```

- `Offsets` + `FirstSeq/LastSeq` are what make a `{file}::{byte_offset}::{seq}` token resolvable in
  O(log n): a page seeks to the nearest recorded offset ≤ the token, then reads at most
  `offset_stride + page_size` lines. That bound is the pagination performance contract (SPEC-01 §7).
- A missing or unparsable `.idx` is **rebuilt from the generation file** at boot or on first touch (count,
  offsets, seq range, min/max ts, sha256). The sidecar is an accelerator: losing it costs a scan, never an
  answer. A sidecar whose `Bytes`/`Sha256` disagree with the file it names is discarded, rebuilt, and the
  file is flagged in `IndexStats` — an `.idx` never gets to describe a file it does not match.
- **Time-window scans** consult the sidecar first: a window entirely outside `[MinTS, MaxTS]` is answered
  with zero pages and zero reads, which is what makes "nothing happened overnight" a free question.

**Retention = drop whole generations.** The sweep of §3.7 deletes at generation granularity and nothing
finer: the generation file, its `.idx`, and — under the `light-hub` profile (SPEC-13 §3.5) — only after its
DuckBrain archive marker is `exported` and verified (TROUBLE-HUB-014 refuses the drop otherwise; the
generation stays on the hot host until the archive is real). In-place record deletion stays forbidden: it
would invalidate offsets, break the monotonic-seq promise of AC-6, and turn every outstanding page token
into a landmine. A dropped generation leaves three cheap traces — the sweep's own
`lifecycle{op:"retention_drop"}` record, the `dropped` archive marker when archival is on, and the
`TruncatedBeforeTS` watermark every cold-read answer carries — so a `reset` hint is an explained boundary,
never a silent hole.

**The dashboard is unaffected by drops**: `/groups` ranked by rate reads the in-memory index built at boot
(§3.6), and dropping files never changes that answer — which is exactly why retention can be O(1) in file
operations while the dashboard stays O(1) in reads.

### 3.8 Storage tier: JSONL + in-memory index by default, and the exact thresholds that flip it (mandate a)

**Decision: JSONL is canonical, the index is in memory, and `modernc.org/sqlite` is not linked in v0.1.**
The mandate permits SQLite only with justification; the justification is four measured/structural facts:

1. **Binary and RSS.** SQLite costs +3.69 MB on the same toolchain (12.05 MB vs 8.36 MB measured with
   net/http+html/template+embed+godbus) and the fleet's sqlite daemon comparable runs 22.4 MB on disk and
   35.9 MB RSS. trouble's budget is a 8–15 MB binary with steady RSS ≤ 80 MB and `MemoryHigh=192M`; three
   megabytes and a second pager/cache are spent for nothing, because the queries this spec must answer need
   aggregates over a *hot window*, not a general query engine.
2. **Two writers, two durability stories.** SQLite would add a second durability surface (WAL + its own
   fsync policy) beside the ledger's group-commit. Two crash-recovery stories is a strictly worse position
   than one, and the audit artifact must remain the JSONL as written.
3. **The audit artifact must stay jq-readable.** The ledger is grep/tail/jq surface in the fleet's own law;
   making SQLite canonical breaks that, and making it derived duplicates the index this spec already needs
   in memory for the O(1)/O(log n) guarantees of §2.3.
4. **The index problem does not go away.** Even with SQLite, the top-N/rate/trend/last-event-age answers
   over the in-flight window are served from resident state; SQLite would add a second copy of the truth and
   a reindex-on-schema migration path.

**FAILURE THRESHOLD — any one of these flips the decision** to `modernc.org/sqlite` as a *derived,
disposable* index store at `<state-root>/ledger/index.sqlite` (JSONL stays canonical; `schema_version` and
every wire format are unchanged; the store is deletable and rebuildable by `trouble ledger reindex
--rebuild-store`):

| # | Trigger (measured at run time, reported in `LedgerStatus`/`IndexStats`) | Threshold |
|---|---|---|
| 1 | Index thrash — resident index at the ceilings with continuous eviction | `ColdEvictionsPerMin > 100` for 5 consecutive minutes, **and** `IndexBytes > 20 MiB` (25% of the steady-RSS budget) |
| 2 | Boot cost — compaction cannot shrink the rebuild scan | `IndexStats.BuildMS > index_build_budget_ms` (9,000 ms) at p50 on the reference host for 7 consecutive days |
| 3 | Query latency at the ceilings | p99 `TopGroups(50)` > 50 ms **or** p99 `Incident(id)` > 5 ms with 500,000 groups and 500,000 incidents |
| 4 | Capacity ceiling | sustained ingest > 50,000 records/min (833 rec/s — the amortized fsync ceiling is 515k rec/s, so the limit is the index update path, not the disk) **or** a site that must serve > 1 M group lookups/day with a working set > 20 MiB |

Until one of those fires, the tier is JSONL + memory; the trigger thresholds are asserted as code constants
in `internal/ledger/tier.go` so the decision is data, not opinion, and `trouble ledger status --json` prints
each trigger's current value beside its threshold.

### 3.9 State-root layout

```
~/.local/state/trouble/                 0700  (SPEC-12 owns the root; this spec owns ledger/)
├── ledger/                             0700  files 0600
│   ├── LOCK  HEAD                      (§3.4)
│   ├── YYYY-MM-DD[.pNN][.N.gen].jsonl  (§3.4)
│   ├── idmap.jsonl                     satellite local↔hub ids
│   ├── index.sqlite                    only under the §3.8 flip; derived, deletable
│   └── quarantine/                     0700
├── spool/                              0700  bounded 256 MB (SPEC-12 §3.7)
├── worktrees-meta/                     0700  (SPEC-08)
├── skills-local/                       0700  (SPEC-11)
└── backups/                            0700  (SPEC-06 file.patch, SPEC-12 upgrades)
```

The ledger directory is created with 0700 and every file with 0600 at open; a wrong mode or an unwritable
directory is TROUBLE-LEDGER-012 at boot (the state-root-level codes TROUBLE-LIFECYCLE-004/005 are SPEC-12's
view of the same condition; the split is deliberate — SPEC-12 reports the root, SPEC-01 reports `ledger/`).
The state root is never `/tmp`: `/tmp` on the reference host is world-writable, holds 245,222 entries and is
not a separate mount, so an unconfigured ledger there is a disk-fill waiting for a flood.

### 3.10 Types added to SPEC-TYPES by this spec

```go
type RecordDraft struct {
    Kind       RecordKind     `json:"kind"`
    Sig        string         `json:"sig"`
    Inc        string         `json:"inc"`
    Origin     Origin         `json:"origin"`
    Actor      Actor          `json:"actor"`
    Redactions int            `json:"redactions"`
    Payload    map[string]any `json:"payload"`
}

type QueryInfo struct {
    Indexed           bool   `json:"indexed"`
    Partial           bool   `json:"partial"`
    Degraded          bool   `json:"degraded"`
    Reason            string `json:"reason"`             // "" | outside_index_window | cold_read_cap | ledger_degraded
    ScannedBytes      int64  `json:"scanned_bytes"`
    ScannedLines      int64  `json:"scanned_lines"`
    TruncatedBeforeTS string `json:"truncated_before_ts"`
    ElapsedMS         int    `json:"elapsed_ms"`
}

type IndexStats struct {
    Entries            int     `json:"entries"`
    Groups             int     `json:"groups"`
    GroupsCold         int     `json:"groups_cold"`
    Incidents          int     `json:"incidents"`
    Sources            int     `json:"sources"`
    Days               int     `json:"days"`
    FilesScanned       int     `json:"files_scanned"`
    BytesScanned       int64   `json:"bytes_scanned"`
    LinesScanned       int64   `json:"lines_scanned"`
    TornLines          int     `json:"torn_lines"`
    CorruptLines       int     `json:"corrupt_lines"`
    SkippedNewerSchema int     `json:"skipped_newer_schema"`
    BuildMS            int     `json:"build_ms"`
    BudgetMS           int     `json:"budget_ms"`
    BudgetBytes        int64   `json:"budget_bytes"`
    IndexBytes         int64   `json:"index_bytes"`
    ScanRateMiBs       float64 `json:"scan_rate_mibs"`
    Degraded           bool    `json:"degraded"`
    DegradedReason     string  `json:"degraded_reason"`   // "" | budget_exceeded | io_error | version_gate
    TruncatedBeforeTS  string  `json:"truncated_before_ts"`
    ColdEvictionsPerMin float64 `json:"cold_evictions_per_min"`
}

type LedgerStatus struct {
    LastSeq           uint64    `json:"last_seq"`           // durable watermark (fsynced)
    LastSeqPending    uint64    `json:"last_seq_pending"`   // allocated, not yet fsynced
    LastTS            string    `json:"last_ts"`
    StallS            float64   `json:"stall_s"`
    LossWindowMS      int       `json:"loss_window_ms"`     // §2.1, = fsync_window_ms
    Day               string    `json:"day"`
    Gen               int       `json:"gen"`
    Part              int       `json:"part"`
    File              string    `json:"file"`
    Bytes             int64     `json:"bytes"`
    Records           int64     `json:"records"`
    FsyncCalls        int64     `json:"fsync_calls"`
    FsyncPerRecord    float64   `json:"fsync_per_record"`
    QueueDepth        int       `json:"queue_depth"`
    QueueCap          int       `json:"queue_cap"`
    BackpressureTotal uint64    `json:"backpressure_total"`
    WriterPID         int       `json:"writer_pid"`
    WriterVersion     string    `json:"writer_version"`
    DiskBytes         int64     `json:"disk_bytes"`
    DiskBudgetBytes   int64     `json:"disk_budget_bytes"`
    Index             IndexStats `json:"index"`
}

type GroupStat struct {
    GroupID     string        `json:"group_id"`
    Sig         string        `json:"sig"`
    Digest      string        `json:"digest"`
    MergeKey    string        `json:"merge_key"`
    Source      string        `json:"source"`
    Title       string        `json:"title"`
    Count       uint64        `json:"count"`
    Rate1m      float64       `json:"rate_1m"`
    Rate5m      float64       `json:"rate_5m"`
    Rate60m     float64       `json:"rate_60m"`
    Trend       float64       `json:"trend"`       // rate_5m / previous 5m − 1
    FirstSeenTS string        `json:"first_seen_ts"`
    LastSeenTS  string        `json:"last_seen_ts"`
    IncidentID  string        `json:"incident_id"`
    Counters    GroupCounters `json:"counters"`
    Cold        bool          `json:"cold"`
}

type SourceAge struct {
    HostID        string  `json:"host_id"`
    Source        string  `json:"source"`
    Zone          string  `json:"zone"`         // loopback | lan | tailnet | public
    LastEventTS   string  `json:"last_event_ts"`
    LastEventAgeS float64 `json:"last_event_age_s"`
    LastSeq       uint64  `json:"last_seq"`
    EventsTotal   uint64  `json:"events_total"`
    Events24h     uint64  `json:"events_24h"`
    Gaps24h       int     `json:"gaps_24h"`
    Dropped24h    uint64  `json:"dropped_24h"`
    Redactions24h uint64  `json:"redactions_24h"`
    CanarySeen    bool    `json:"canary_seen"`
    CanaryLastTS  string  `json:"canary_last_ts"`
}

type EvidenceBundle struct {
    Inc        string           `json:"inc"`
    Sig        string           `json:"sig"`
    Digest     string           `json:"digest"`
    MergeKey   string           `json:"merge_key"`
    GroupID    string           `json:"group_id"`
    FirstSeq   uint64           `json:"first_seq"`
    LastSeq    uint64           `json:"last_seq"`
    Records    []Record         `json:"records"`
    KindCounts map[string]int   `json:"kind_counts"`
    RefCount   int              `json:"ref_count"`
    Overflow   int              `json:"overflow"`   // refs dropped by index_inc_per_incident
    Gaps       []GapRecord      `json:"gaps"`
    Partial    bool             `json:"partial"`
    Info       QueryInfo        `json:"info"`
}

type RotationPolicy struct {
    Cadence         string `json:"cadence"`          // "daily"
    AtUTC           string `json:"at_utc"`           // "00:00:00Z"
    MaxBytes        int64  `json:"max_bytes"`        // intra-day part rollover
    PartSuffix      string `json:"part_suffix"`      // ".p%02d"
    FsyncWindowMS   int    `json:"fsync_window_ms"`
    MaxBatchRecords int    `json:"max_batch_records"`
    QueueCapRecords int    `json:"queue_cap_records"`
    MaxEnqueueWait  string `json:"max_enqueue_wait"`
    Fdatasync       bool   `json:"fdatasync"`
    MaxRecordBytes  int64  `json:"max_record_bytes"`
}

type RetentionPolicy struct {
    RawKeep            Duration `json:"raw_keep"`
    CompactedKeep      Duration `json:"compacted_keep"`
    PayloadTTL         Duration `json:"payload_ttl"`
    TombstonesKeep     bool     `json:"tombstones_keep"`     // always true
    CompactionInterval Duration `json:"compaction_interval"`
    CompactionMinAge   Duration `json:"compaction_min_age"`
    CompactionMinBytes int64    `json:"compaction_min_bytes"`
    DiskBudgetBytes    int64    `json:"disk_budget_bytes"`
    DiskWarnPct        int      `json:"disk_warn_pct"`
    SpineKinds         []string `json:"spine_kinds"`
}

type CompactionResult struct {
    Day                 string   `json:"day"`
    FromFiles           []string `json:"from_files"`
    ToFile              string   `json:"to_file"`
    FromGen             int      `json:"from_gen"`
    ToGen               int      `json:"to_gen"`
    LinesIn             int64    `json:"lines_in"`
    LinesOut            int64    `json:"lines_out"`
    BytesIn             int64    `json:"bytes_in"`
    BytesOut            int64    `json:"bytes_out"`
    RecordsKept         int64    `json:"records_kept"`
    Aggregates          int64    `json:"aggregates"`
    PayloadsExpired     int64    `json:"payloads_expired"`
    PayloadsDropped     int64    `json:"payloads_dropped"`
    TombstoneCountsKept bool     `json:"tombstone_counts_kept"`
    ElapsedMS           int      `json:"elapsed_ms"`
    DryRun              bool     `json:"dry_run"`
    ErrorCode           string   `json:"error_code"`
}
```

```json
{"last_seq":41207,"last_seq_pending":41209,"last_ts":"2026-09-16T09:14:03.221Z","stall_s":1.2,"loss_window_ms":200,"day":"2026-09-16","gen":0,"part":1,"file":"2026-09-16.jsonl","bytes":30408704,"records":41207,"fsync_calls":11,"fsync_per_record":0.000267,"queue_depth":0,"queue_cap":65536,"backpressure_total":0,"writer_pid":2028599,"writer_version":"0.1.0","disk_bytes":30408704,"disk_budget_bytes":2147483648,"index":{"entries":41207,"groups":7,"groups_cold":0,"incidents":2,"sources":3,"days":1,"files_scanned":1,"bytes_scanned":30408704,"lines_scanned":41207,"torn_lines":1,"corrupt_lines":0,"skipped_newer_schema":0,"build_ms":412,"budget_ms":9000,"budget_bytes":536870912,"index_bytes":4194304,"scan_rate_mibs":148.2,"degraded":false,"degraded_reason":"","truncated_before_ts":"","cold_evictions_per_min":0}}
{"host_id":"7f3a91c2d4e5b607","source":"sentinel:payment-worker","zone":"loopback","last_event_ts":"2026-09-16T09:14:03.221Z","last_event_age_s":0.2,"last_seq":41207,"events_total":6401,"events_24h":6401,"gaps_24h":0,"dropped_24h":0,"redactions_24h":3,"canary_seen":true,"canary_last_ts":"2026-09-16T09:13:00.000Z"}
{"group_id":"grp_01J9Z6Q0M2X4T8V1K7B3N5R8WH","sig":"sentinel:sha256v1:9f2c1d3e4b5a6c7d","digest":"9f2c1d3e4b5a6c7d0123456789abcdef0123456789abcdef0123456789abcdef","merge_key":"bcc48494f2190f4f","source":"sentinel","title":"queue wedge: pool exhausted","count":6401,"rate_1m":18.4,"rate_5m":12.1,"rate_60m":6.7,"trend":0.52,"first_seen_ts":"2026-09-16T06:00:00.000Z","last_seen_ts":"2026-09-16T09:14:03.221Z","incident_id":"inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE","counters":{"events":6401,"suppressed":0,"redacted_values":3,"dropped_events":0,"sample_rate":1},"cold":false}
```

## 4. Wiring

### 4.1 Producers and the single write path

Every subsystem appends through `Ledger.Append`; nothing else opens a file in `ledger/`. The ledger is the
only writer of every kind (SPEC-INDEX §3.4, "all kinds"). Kind-specific payload schemas are owned by the
emitting spec and are indexed generically here:

| Producer (spec) | Kinds | Payload schema owner |
|---|---|---|
| SPEC-03 sensors | event, gap, breaker, canary | SPEC-03 §3.5 |
| SPEC-04 sentinel | event, group, gap, canary | SPEC-04 §3.6 |
| SPEC-05 ladder | incident, verify, breaker | SPEC-05 §3.7 |
| SPEC-06 registry | play_run, tool_call | SPEC-06 §3.4 |
| SPEC-07 research | research | SPEC-07 §3.6 |
| SPEC-08 flow | flow, spawn | SPEC-08 §3.3 |
| SPEC-09 issues | issue | SPEC-09 §3.4 |
| SPEC-11 skills | skill | SPEC-11 §3.2 |
| SPEC-12 lifecycle | config, lifecycle | SPEC-12 §3.5 |
| SPEC-01 ledger (housekeeping only) | lifecycle (`op` = rotate, recover_torn, hole, compaction_start, compaction_resume, compaction_done, index_degraded, budget_exceeded, seq_recovered, scrub_refusal), group (`op=compaction_aggregate`) | this spec, §3.7 |

Housekeeping records are the only records the ledger authors itself, and they are marked by
`payload.op` + `actor.kind="daemon"`; live `group` records remain SPEC-04's. The audit mirror rule of
SPEC-INDEX §5.3 is enforced here: any record describing a failure carries `payload.error_code` with the
TROUBLE-&lt;AREA&gt;-&lt;NNN&gt; code the ladder saw, so the trail reads without logs.

### 4.2 The scrub boundary (brief C: ingest → scrub → ledger)

The ledger never sees unscrubbed bytes, enforced two ways:

1. **Compile-time shape**: producers pass a `RecordDraft` whose payload is built from a `ScrubResult`
   (`Value`, `Redactions`, `ByRule`) — the counts travel into `Record.Redactions`, and only counts ever
   travel. The ledger has no raw-bytes ingest API and no parser imports.
2. **Write-boundary re-scan** (defence in depth): when `assert_scrubbed = true` (default), every serialized
   payload is re-scanned with SPEC-02's mandatory rule set via `scrub.MandatoryScan`. A hit writes **no
   record** and propagates SPEC-02's error (TROUBLE-SCRUB-002 / TROUBLE-SCRUB-006) unchanged, increments the
   `scrub_refusal_total` counter, and emits nothing containing the value. Go's `regexp` is RE2 (linear, no
   catastrophic backtracking) and the scan budget is ≤ 50 µs per 4 KiB payload, against a measured ingest
   path of 161 µs/request (6,199 req/s) — the boundary costs under a third of the headroom and buys the
   guarantee that the append-only, git-distributed, dashboard-rendered and issue-filed ledger cannot contain
   a secret. This creates one new dependency edge: `internal/ledger → internal/scrub` (scrub imports only
   `internal/types`, so there is no cycle; it is the SPEC-INDEX §4.1 SPEC-01 → SPEC-02 build order).

### 4.3 Boot order and lifecycle

`lifecycle.Open` (state root, 0700) → `ledger.Open` (LOCK, HEAD/seq recovery, torn-line recovery, index
build; TROUBLE-LEDGER-005/012/008 surface here) → scrub rules load → sensors/sentinel start → `sd_notify
READY`. `SIGTERM` → stop producers → drain the queue → fdatasync → write HEAD → release LOCK → exit. Because
`READY` is only sent after the index exists, systemd's `WatchdogSec=60` (SPEC-12 §3.2) covers the 9,000 ms
build budget with 6× margin.

### 4.4 What the ledger feeds

- **`/health.json`** (SPEC-10): `ledger_last_seq`, `ledger_last_ts`, `ledger_stall_s`,
  `runtime_watermarks.ledger_bytes`, `sources[]`, `ledger_loss_window_ms`; the external stall checker (brief
  O) alarms on `LastSeq` not advancing, which is why it is read from memory at O(1) and only after fsync.
- **Verification (brief D)**: every field of the `Evidence` tuple except `ZonesQuiet` joining is produced by
  a §2.3 call — `events_observed` from `TopGroups`/`ScanFrom` over the window, `canary_seen`/`canary_id` from
  the canary records and `SourceAge.CanarySeen`, `counter_deltas` from `CounterDeltas`, `window_bounds` from
  the caller's UTC bounds, `sources_expected`/`alive`/`missing` from `Sources()` joined with SPEC-05's
  expectations. Zones come from `SourceAge.Zone` (loopback | lan | tailnet | public) and windows are always
  evaluated in UTC (SPEC-INDEX §6.5: monotonic clock for local durations, wall-clock UTC across hosts).
- **SPEC-05 dedup/reopen**: `IncidentBySig` and the merge-key index are the mechanism behind AC-22's "one
  incident, one group" across sensor, sentinel and collector paths.

### 4.5 Config and CLI

Every §2.5 key is resolvable by `trouble config explain` with per-value provenance (SPEC-12 §3.4).
`trouble ledger status --json` prints `LedgerStatus` and the §3.8 trigger values beside their thresholds.

## 5. Errors

Codes emitted by this area: **TROUBLE-LEDGER-001 .. TROUBLE-LEDGER-012** (SPEC-TYPES §5, SPEC-INDEX §3.5).
The ledger also propagates SPEC-02 codes unchanged (§4.2) and references SPEC-12's `TROUBLE-LIFECYCLE-009`
(stall alarm) and `TROUBLE-LIFECYCLE-004/005` (state root) as the callers' view; it emits none of them.

| Code | Class | Trigger (exact) | Behaviour |
|---|---|---|---|
| 001 | permanent | append refused (empty origin/actor/sig where required, unknown kind, seq/rec_id supplied by the caller, payload over `max_record_bytes` on a spine kind, `max_enqueue_wait` exceeded) or the write/fdatasync itself failed | `payload.reason` ∈ `validation\|backpressure\|write\|fsync\|head`; producers block up to `max_enqueue_wait` first; the ladder refuses new budgeted work on `backpressure` |
| 002 | permanent | seq hole, duplicate seq or non-monotonic seq seen on read | hole recorded and returned in `VerifyReport.SeqHoles`; the daemon keeps serving; the hole is written to the trail by the next batch as `lifecycle{op:"hole"}` |
| 003 | transient | torn last line (no terminating `\n`, or an unparsable tail) | the torn fragment is excluded from the index, counted (`IndexStats.TornLines`), the file gets a `\n` before the next record so exactly one line is dropped, and `lifecycle{op:"recover_torn"}` is written |
| 004 | permanent | a record's `schema_version` > `Options.MaxSchema` | forward-compat: the line is skipped, counted (`SkippedNewerSchema`), the file is flagged `version_gate`; the daemon stays up and `/health.json` shows the gap |
| 005 | permanent | `flock(LOCK_EX\|LOCK_NB)` on `ledger/LOCK` fails — a second writer | the process logs `LOCK`'s pid/version/git_sha, emits the error and exits non-zero; never retried, never overridden |
| 006 | transient | rotation failed (new day/part file uncreatable, ENOSPC on create) | the writer keeps appending to the current file, retries with backoff, records `lifecycle{op:"rotate_failed"}`; no record is lost |
| 007 | transient | compaction failed (temp file, rename, unlink or fsync of the directory failed), or an interrupted compaction commit is found at boot | the current generation stays authoritative; retained generations are untouched; retried at the next interval; boot case resolves by highest generation (§3.4 rule 3) |
| 008 | transient | index rebuild failed or exceeded the budget / scan-rate floor | the degraded ladder of §3.6 applies, `IndexStats.Degraded=true`, the cold-read path answers, and `lifecycle{op:"index_degraded"}` is written; the daemon still starts |
| 009 | permanent | retention blocked: the only compaction candidates are spine records whose counts must be kept | compaction is refused, `IndexStats`/`LedgerStatus` carry the condition, the operator must raise `disk_budget_bytes` or lower `retention_compacted`; counts are never sacrificed |
| 010 | transient | disk budget exceeded and compaction cannot reclaim enough | escalated payload mode (§3.7 rule 3/4): records keep identity/counters with `payload_dropped=true`, then producers are refused with 001/`backpressure`; `lifecycle{op:"budget_exceeded"}` |
| 011 | permanent | corruption: unparsable interior line, duplicate `rec_id`, or a `payload_sha256` mismatch on a ≥ 4 KiB payload | the line is excluded, counted (`CorruptLines`), the file is marked partial in `VerifyReport`, and the affected incident's evidence is served with `Partial=true`; the daemon does not fail |
| 012 | permanent | `ledger/` missing, unwritable, or mode ≠ 0700 | boot fails with the exact path, mode and expected mode; the daemon exits non-zero (state-root-level reporting is SPEC-12's TROUBLE-LIFECYCLE-004/005) |

No new codes are required by this spec: all twelve slots are used, `policy_refused` does not apply to storage
(SPEC-INDEX §5.2 reserves it for the polkit and do-not-touch surfaces).

**Pagination and the sidecar mint no code of their own, deliberately.** A stale `page_token` is not an
error: it is a `reset` hint in `QueryInfo` (`Reset=true` plus `ResetReason`, §2.3a) followed by a
newest-first restart, because a client whose page fell off the retention cliff has done nothing wrong, and
an error loop would make "drop old files" expensive in exactly the way this design exists to avoid. A
missing, torn or mismatched `.idx` is a rebuild (§3.7a), counted in `IndexStats`, not a failure code — the
sidecar is an accelerator, and the only durable artifact stays the generation file itself. A rebuild that
cannot complete (an unreadable file) surfaces as 011 corruption, which is the honest existing code for it.

## 6. Edge cases

1. **Midnight inside a batch.** Rotation happens between batches only; a batch's records all land in the
   file open at batch start, so per-file seq stays strictly increasing, no line is split across files, and a
   reader following by seq crosses the boundary without a special case.
2. **Clock jumped backwards.** `ts` is never rewritten. Rotation uses a monotonic guard: it triggers when
   the UTC date advances or `now ≥ lastRotation + 24h`, whichever occurs first, so a backwards jump cannot
   produce a file named for an earlier day that already has an authoritative generation. Cross-host skew is
   SPEC-12's `TROUBLE-LIFECYCLE-017`.
3. **SIGKILL between rename and unlink in compaction.** Both generations exist; boot prefers the higher
   generation, unlinks the lower and writes `lifecycle{op:"compaction_resume"}`. `trouble ledger verify`
   accepts the state (it is a legal, self-healing intermediate).
4. **Torn line that is a valid JSON prefix.** Exactly one line is lost: the fragment is excluded, `\n` is
   appended before the next write, and `seq` continuity is preserved because the fragment was never counted
   as a record. If the fragment *did* contain only complete earlier lines, they are indexed (the reader
   keeps every complete line and drops only the tail).
5. **Interior garbage (a line that does not parse but has a `\n`).** Counted, skipped, reported (011); the
   seq hole it creates is reported as 002 on the gap check. Records after it are still indexed.
6. **ENOSPC exactly during a batch write.** The partial line is left torn (recovered by rule 4); the batch's
   seqs are not re-issued, the ack is withheld, and the escalation of §3.7 runs; if it cannot free space the
   producer path returns 010, then 001/`backpressure`.
7. **Queue saturation.** 65,536 queued records, producers block up to `max_enqueue_wait` (5 s), then get
   001/`backpressure`; `BackpressureTotal` increments and the ladder stops admitting budgeted work
   (SPEC-05 design constraint). Ledger records are never dropped to relieve pressure — that would put a hole
   in the audit chain that AC-6 forbids.
8. **A record with `sig=""`.** Legal for non-sig-keyed kinds; it is indexed by seq/ts/kind/inc only and
   belongs to no group. A sig-required kind with an empty sig is refused (001/validation).
9. **Two groups claim one merge key.** Not a ledger error: the ledger stores both, indexes both and exposes
   `GroupStat.MergeKey`; the conflict is SPEC-05's dedup decision, recorded as an `incident` record.
10. **Index capped mid-window.** `index_inc_per_incident` overflow keeps the *oldest* 512 refs plus a
    running count, so an incident's origin is never lost to the cap; `EvidenceBundle.Overflow` makes the
    truncation explicit.
11. **A whole day unreadable (I/O error at boot).** The day is skipped, `DegradedReason="io_error"`, the
    remaining days index normally, and queries that need the missing day return `Partial=true` with the
    scanned counts instead of an empty answer — the verification layer must be able to tell "quiet" from
    "unavailable" (brief D).
12. **A very long single line (1 MB payload).** Refused for spine kinds, truncated for event/canary kinds at
    `max_record_bytes`; the truncation is flagged and counted, so an oversized stack trace cannot silently
    evict evidence.
13. **Read during the index rebuild.** `Open` completes the build before `READY`, so no query is served
    against a half-built index; a query issued while a *cold* index is degraded returns `QueryInfo.Degraded`
    and the cold-read answer.
14. **Two records with the same `rec_id`** (a minted-id replay or a copied file). The second is reported as
    011 corruption and excluded from the index; `seq` (not `rec_id`) remains the locator, so the ledger stays
    queryable.
15. **A page token whose generation was dropped mid-walk.** The sweep deletes whole files (§3.7a), so the
    token's file is gone but the walk is not broken: the answer is a newest-first first page with
    `Reset=true`, `ResetReason="generation_dropped"` and `TruncatedBeforeTS` set to the boundary — the client
    resumes at the top and sees its own position in `QueryInfo`, never a 500 and never an empty page that
    looks like "no records".
16. **A token from another host, or a token that does not parse.** Same treatment as 15 with
    `ResetReason="token_invalid"` / `"token_foreign"`: the token grammar is namespaced by the generation file
    name, so a token minted on a satellite cannot silently address the hub's file of the same day.
17. **Rotation and compaction inside a walk.** A part rollover (`2026-09-16.jsonl` → `.p02`) is transparent
    because the token names its file and the offset is per-file; a compaction of the walk's day serves the
    remainder from the generation file's sidecar with `Partial=true` and the scanned counts, and the walk
    still terminates with `next_page_token=""`.
18. **A `.idx` written by a crash-truncated rotation** (sidecar shorter than a full JSON object). The file is
    discarded and rebuilt from the generation file (§3.7a), counted, and the answers are unchanged — the
    sidecar is derived state, so its corruption is a latency event, not a correctness event.
19. **`page_size` above the maximum, or negative.** Above the maximum it is clamped to `page_size_max`;
    negative and non-numeric values are a 400 at the API boundary. Neither case can produce an unbounded
    read, which is the property the clamp exists to guarantee.

## 7. Testing

Test files under `internal/ledger/`, all with the measured numbers as regression floors. `go test
-race -count=1 ./internal/ledger/...` is the CI gate. Every test that touches time injects `Options.Now`.

| File | Cases | Numeric thresholds / regression numbers |
|---|---|---|
| `durability_test.go` | `TestFsyncCountIsStructural` — 100k appends, batch 4096: assert `FsyncCalls == ceil(records/max_batch_records)` (the group-commit invariant, not a timing proxy); `TestAmortizedThroughput` — ≥ 100,000 rec/s amortized on the reference host (measured 515k rec/s, so a 5× margin); `TestFsyncWindowBound` — a producer appending 1 rec/10 ms with `fsync_window_ms=200`: p99 ack latency ≤ 210 ms and ack count == record count; `TestAckImpliesDurable` — SIGKILL a child writer mid-batch: every acked record is present after restart, unacked records may be absent, and `LastSeq == count(acked)`; `TestPerLineRegression` — a test-only per-line mode must be measurably slower (measured 512 rec/s) so the amortized mode's advantage cannot be optimised away silently | ≥100k rec/s amortized; p99 ≤ window + 10 ms; fsync/record ≤ 1/4096 |
| `recover_test.go` | `TestTornLastLine` (no `\n`) → 003 + one line dropped + `\n` inserted; `TestTornPrefixKeepsCompleteLines`; `TestInteriorCorruption` → 011 + skip + seq hole reported as 002; `TestNewerSchemaSkipped` → 004 + `SkippedNewerSchema==1` + daemon up; `TestSeqRecoveryFromHeadHint` (HEAD deleted / HEAD stale / HEAD ahead of the file — the max rule wins); `TestCompactionResume` (gen 0 + gen 1 present) → gen 1 authoritative + `compaction_resume` | 0 records lost except the torn fragment; `VerifyReport.OK==false` iff findings |
| `index_test.go` | `TestRebuildBudget` — 512 MiB synthetic ledger: `BuildMS ≤ 9000`, `IndexBytes ≤ 67108864`, `ScanRateMiBs ≥ 60`; `TestDegradationLadder` — 1.2 GiB synthetic: rung 2 then rung 3 selected, `Degraded=true`, `DegradedReason=="budget_exceeded"`, `TruncatedBeforeTS` set; `TestGroupEviction` — `index_max_groups+1` groups: `GroupsCold==1`, counts equal before/after, `ColdEvictionsPerMin>0`; `TestQueryLatency` — 500k groups/500k incidents: `TopGroups(10)` ≤ 5 ms, `TopGroups(50)` ≤ 50 ms, `Incident(id)` ≤ 50 µs, `GroupBySig` ≤ 50 µs, `Sources()` (5,000) ≤ 200 µs; `TestColdReadIsCapped` — the cold path never exceeds `cold_read_max_bytes`/`cold_read_max_lines` and returns `Partial=true` | as listed; no query may exceed 5× the §3.8 trigger-3 thresholds without failing CI |
| `sig_test.go` | `TestVectorTable` — every row of §3.3 (14 rows) asserted byte-for-byte, full 32-byte digest **and** 16-hex short; `TestMessageNormIdempotent` (re-normalizing a normalized message is a fixed point); `TestFramingEscapes` — a field containing `\x00`/`\x1f`/`\x1e` is cleaned, and `norm_version` is part of the string; `TestMergeKeyVectors` (3 rows) and `TestMergeKeySubjectResolution` (payload → config table → sig-short fallback); `TestNormVersionMismatchNeverMerges` | all 17 vectors exact; any diff fails CI |
| `rotate_test.go` | `TestMidnightRotation` — seq strictly increasing across the boundary, one file per day; `TestBatchNeverSpansFiles`; `TestPartRollover` — `rotate_max_bytes` small → `D.p02.jsonl`, part order preserved on read; `TestReaderAcrossRotation` — a reader following by seq crosses the boundary without re-open errors; `TestReaderAcrossCompaction` — a reader holding the gen-0 fd reads to EOF while compaction unlinks it (no error, stable image) and a new reader resolves gen 1 | 0 lost records; 0 duplicate reads |
| `compact_test.go` | `TestCompactionIsANewGeneration` — `D.jsonl` byte-identical after compaction (mtime/inode/byte compare), `D.1.gen.jsonl` created, gen 0 unlinked only after fsync; `TestCountsPreserved` — Σ group counters before == after, `TombstoneCountsKept==true`, `Aggregates == distinct(groups,day)`; `TestPayloadTTL` — payloads older than the TTL dropped with `payload_ttl_expired=true` while the spine record count is unchanged; `TestSpineNeverTTL` — a 400-day-old incident/verify/tool_call pair still returns its payloads; `TestRetentionBlocked` → 009; `TestDiskBudgetEscalation` → order (1)(2)(3) and 010 emitted once | counters equal exactly; spine retention 100% |
| `scrub_boundary_test.go` | `TestMandatoryRescanRefuses` — a payload containing a mandatory-rule match is refused, **no** record is written, the propagated code is TROUBLE-SCRUB-006, `scrub_refusal_total==1`, and the value appears nowhere in the ledger bytes; `TestScanBudget` — ≤ 50 µs per 4 KiB payload; `TestRedactionCountOnly` — `Record.Redactions` equals the scrubber count and no `ByRule` value ever reaches the ledger | refusal count exact; ledger bytes contain no match |
| `docs_test.go` | `TestLossWindowStatement` — the §2.1 sentence appears verbatim in `README.md`, `docs/operations.md`, `trouble --help` text and `GET /health.json` (`ledger_loss_window_ms`); `TestSqliteNotLinked` — `go list -deps ./internal/ledger` contains no `modernc.org/sqlite` while §3.8's triggers are un-tripped | 4/4 copies exact; 0 sqlite deps |
| `ac_test.go` | **AC-6**: 1 M synthetic appends → seq has no holes, every line parses, every record has non-empty `origin.host_id`/`origin.source` and a `rec_id`, and no byte of a written file changes; **AC-22**: three records (journald, sentinel, collector) sharing one `merge_key` and three different sigs → `IncidentBySig`/merge-key index resolve to one incident and one group, while two distinct merge keys stay two; **AC-26**: a 20-step scripted ladder fixture (SPEC-05's harness) + a kill-switch flip inside it → `Evidence(inc)` returns ≥ 1 record per ladder state in seq order with no gap between the incident's first and last seq, and the kill-switch checkpoint is present | AC-6: 0 holes, 0 header violations; AC-22: 1 group / 1 incident, 0 duplicates; AC-26: 20/20 steps present, 0 missing seq |
| `internal/ledger/page_test.go` | **AC-30**: token grammar round-trip (opaque, stable within a generation); a 10 M-record corpus walked at `page_size` 500 and 5000: no record repeated, no record skipped, `next_page_token=""` exactly at the end; drop-oldest-generation mid-walk → `Reset=true` + newest-first restart with **0 HTTP 500s**; tokens that are dropped/foreign/unparsable → the matching `ResetReason`; `page_size` clamp and the negative-value 400; a cold-read page stays `Partial=true` and bounded | every record exactly once per walk; p99 page latency ≤ 25 ms on the sidecar path with 10 M records; `ScannedLines ≤ offset_stride + page_size` for every page (0 unbounded scans) |
| `internal/ledger/sidecar_test.go` | `.idx` written at rotation and at compaction with exact count/offsets/seq range/min-max ts; rebuild with the `.idx` deleted; rebuild from a torn `.idx`; a sidecar whose `Bytes`/`Sha256` mismatch the file → discard + rebuild + `IndexStats` flag; a time window outside `[MinTS, MaxTS]` → 0 bytes read; a window inside it reads only the straddling pages | rebuilt sidecar equals the original field-for-field; 0 bytes read for an out-of-range window; a mismatched sidecar never answers a page |

Every threshold above is also printed by `trouble ledger status --json` as a live value beside its limit, so
a regression is visible in production and not only in CI.

## 8. hilo impact

**Packages/files created by this spec** (greenfield repository `~/trouble`; no fleet repository is
touched — the only repository in scope is the new one):

```
internal/ledger/ledger.go      Open/Close/Append/Status, Options, the writer goroutine
internal/ledger/writer.go      batch/part/dayEntry/writerState, group commit, fdatasync, HEAD
internal/ledger/recover.go     LOCK, seq recovery, torn-line recovery, VerifyReport
internal/ledger/index.go       the bounded index, boot rebuild, degradation ladder, cold read
internal/ledger/query.go       the Query interface implementation, QueryInfo
internal/ledger/sig.go         normalization, framing, message_norm, MergeKey, the vector table
internal/ledger/rotate.go      daily rotation, part rollover, Resolve(seq), ScanFrom
internal/ledger/sidecar.go     the .idx writer/reader: offsets, seq range, min/max ts, rebuild-on-mismatch
internal/ledger/page.go        the §2.3a page-token walk: parse/format, page_size clamp, reset hints
internal/ledger/compact.go     generation rewrite, payload TTL, tombstone totals, disk escalation
internal/ledger/tier.go        the §3.8 trigger constants + live trigger evaluation
internal/ledger/*_test.go      §7
```

**Fan-out (what it imports)**: `internal/types` (all shared types), `internal/scrub` (one function,
`MandatoryScan`, §4.2) — nothing else, by design, so the ledger cannot acquire a policy dependency.
**Fan-in (what imports it)**: every subsystem — `sensors, sentinel, ladder, registry, research, flow,
issues, dashboard, skills, lifecycle` call `Append` and/or `Query`. In the hilo graph this makes
`internal/ledger` the second-highest fan-in node after `internal/types`, and the single choke point for
durability.

**Blast radius.** Any change to the `Record` field set, the `seq` semantics, the file naming, or a
normalization function is blast-radius-maximal: a `Record` change requires `schema_version = 2` plus the
forward-compat read rule of §3.1 (004), and a normalization change requires `norm_version = 2` and re-keys
every signature space (SPEC-TYPES §6.3). Both are one-file changes *in the ledger* that every other spec
must be told about, which is why §3.3's vectors and §6.3's version rule are normative and CI-enforced. Within
this repository the blast radius of the ledger is maximal by construction; outside it, zero: no fleet path,
port, unit name or repo is referenced by this spec, and no fleet repository is modified.
