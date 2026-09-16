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

## 8. Scrubbing (SPEC-02)

`internal/scrub` is the only path bytes take on their way to persistence. Operationally it shows up
in four places.

**Refused records.** A record that trips the persistence-boundary re-scan is refused with
`TROUBLE-SCRUB-008`; the caller writes a `gap` and a `lifecycle` note instead of the payload, and the
ledger's `scrub_refusal_total` counter moves (surfaced by `trouble ledger status --json`). A refusal
is either (a) a producer that forgot to scrub — fix the caller, do not weaken the rule table — or
(b) a payload whose content legitimately looks like a credential (see the false-positive notes
below). The offending bytes are never stored, so a refusal is not diagnosable from the ledger: read
the caller's log line for the record's `sig` and `kind`.

**Counts, never values.** Every scrubbed record carries `redactions` and, in
`payload["scrub"]`, the `by_rule` map, `bytes_in/bytes_out`, `truncated`, `rules_version` and the
engine (`re2`). `Engine.Stats()` (process-cumulative) adds `calls`, `refused_bytes`, `invalid_utf8`,
`timeouts`, `fail_closed` and `boundary_refusals`. A rising `timeouts`/`fail_closed` rate means
payloads are being dropped, not redacted: check host load against `scrub.rule_timeout` (250 ms
default) before anything else.

**Knobs that are safe to use.** `scrub.pii_mode = "keep"` and per-project `pii_mode` are the
documented escape hatches for PII rules (`pii_email_ip`, `pii_identity_kv`); `scrub.path_mode` /
`path_allowlist` / `home_roots` tune the path rules; `max_bytes` and `on_over` are per-target
(only `event_msg`, `stack` and `journal_tail` truncate — everything else refuses, because a
truncated issue body or board row corrupts a durable artifact a human reads). Knobs that are **not**
available: the 13 mandatory rules, `boundary_verify`, and any attempt to configure a rule named
after a built-in. All of those are `TROUBLE-SCRUB-002` at boot, before the ingestion port binds.

**Cost.** The prefilter is what keeps the ingress path cheap: a clean payload costs single-digit
microseconds because no RE2 program runs. A payload that *does* carry a secret runs a full rule pass,
which is dominated by the optional PII rules (`pii_identity_kv` is case-insensitive with an eleven-way
name alternation). MEASURED, on the reference host at load ~2-9: prefilter 3.8 µs/KiB, boundary
re-scan 4.0-5.0 µs/KiB, mandatory pass 4.3 µs/KiB (clean), full set 89 µs/KiB (clean, PII enabled),
26 ms per 256 KiB (clean), ~640 µs for a 1.5 KiB payload carrying four secrets, and 7,500 req/s
through the ingestion harness with the scrubber in the path (12,700 req/s without it). SPEC-02 §3.9
budgets 3 µs/KiB for the prefilter, 25 µs/KiB for a mandatory pass and 60 µs/KiB for the full set:
the first three are met, the PII-bearing numbers are 1.5x the budget and are a documented deviation —
Go's RE2 is linear but its NFA simulation costs 30-90 ns/byte on these patterns. `TestScrubBudget`
asserts within 4x of the spec number and logs the measured value on every run.

### False positives worth knowing

* `cloud_key_shape` matches any `sk-`, `ghp_`, `hf_`, `SG.` … prefix followed by 20+ token
  characters, so prose like `risk-management-framework-2026` is redacted. It is a mandatory rule;
  the fix is the wording, not the table.
* `pii_email_ip`/`pii_identity_kv` leave loopback and RFC1918 addresses alone on purpose
  (`127.0.0.0/8`, `10/8`, `172.16/12`, `192.168/16`, `169.254/16`, `0.0.0.0`) — they identify
  nobody outside the host.
* The reserved `payload["scrub"]` note is exempt from the boundary re-scan: its keys are rule names
  (`"dsn_secret":1`), which a name-driven pattern would otherwise read as an assignment and refuse.
  Everything outside the note is still checked.
