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

## 5. The sensors: what to look at, and what "wrong" looks like

`internal/sensors` (SPEC-03) is the detection plane. It owns observation, rule evaluation, storm
control and liveness, and nothing else: it never calls the registry, never holds a tool handle, and
never spawns anything except `journalctl`.

Start with the capability probe. `trouble sensors probe` writes exactly one `capability_probe`
record and repeats the verdict in `/health.json` as a machine-parseable reason
(`SensorHealth.Reason`), so the daemon's own claims about what it can do are auditable:

| Probe field | Meaning | What "wrong" looks like |
|---|---|---|
| `psi_mode` | `triggers+sampling`, `sampling-only` or `disabled` | `sampling-only` with `TROUBLE-SENSORS-005`: arming is denied for this uid. PSI still samples, rules on `avg*` still work, only latency support is lost |
| `trigger_arm` | `ok`, `EINVAL`, `EBUSY`, `EPERM` | `EBUSY` means something else holds the trigger; the daemon degrades rather than retrying in a loop |
| `max_window_us` | the ceiling discovered by probing downward from 20 s | `0` while `trigger_arm=ok` means the ceiling sweep failed: treat the trigger path as unproven |
| `stall_zero_rejected` | a `stall=0` write was refused, as it must be | `false` on an armed host means the grammar is not being enforced — the daemon's own probe is lying |
| `journald.{binary,cursor_ok,member_adm}` | `journalctl` exists, its cursor grammar works, and the uid can read the journal | `member_adm=false` is `TROUBLE-SENSORS-009`: the group grant only takes effect after a restart, and the install step prints the `usermod -aG` line |
| `dbus_managers[]` | every manager the daemon intended to watch, with connect/subscribe outcomes | a configured manager with `subscribe=false` is `TROUBLE-SENSORS-013` and its identity is in the reason — never a silent skip |
| `oomd_available` | systemd-oomd present | `false` is a documented no-op (`TROUBLE-SENSORS-016`), re-probed every 10 m; the memory-pressure rules cover the gap |

### 5.1 Daily checks

| Check | Command | What "wrong" looks like |
|---|---|---|
| a sensor is degraded | `GET /health.json` → `sensors[]` | `degraded=true` with a **non-empty** `reason`. A degraded sensor keeps its reason until it recovers: an empty reason on a degraded sensor is itself a bug |
| a sensor went quiet | `GET /health.json` → `sensors[]` | `last_success_ts` older than the expectation below. **Entry silence is not staleness**: a quiet journal for an hour with a green seek probe is healthy, and `last_event_age_s` grows without penalty |
| a dropout is documented | `trouble ledger query --kind gap` | every dropout is one gap record with `cause`, `from_ts`/`to_ts` and `subsystem`; `est_lost=-1` means "genuinely unknown", never zero |
| the events are reaching the ledger | `GET /health.json` → `sources[]` | `alive=false` for an expected source. Liveness is decided by the probe, never by arrivals |
| storm control is engaged | `GET /breakers` | a scope in `open` with its trip count and `open_until`. Events are still recorded while a scope is open: suppression happens at the ladder gate, and `Suppressed` counts it |
| rules are the ones you edited | `GET /rules` | a reload that failed keeps the **previous** set and records `TROUBLE-SENSORS-019` with the file and line. After 3 consecutive failures the inotify trigger is disabled; `SIGHUP` and the 60 s mtime sweep continue |

Sensor expectations (`LastSuccessTS` decides health, `LastEventAgeS` is information only):

| Sensor | Proven by | Stale after |
|---|---|---|
| `psi` | a completed sampler tick (default 2 s) | 10 s |
| `journald` | the seek probe (`journalctl -n 0 --after-cursor`) every 30 s | 90 s |
| `dbus` | `Peer.Ping` per manager every 30 s + a `ListUnits` reconcile every 300 s | 90 s |
| `disk` | a statfs sweep of every configured mount every 60 s | 180 s |
| `timers` | `ListTimers` + unit-state sweep every 60 s | 180 s |
| `inotify` | a watch-set recheck every 900 s | 900 s |

### 5.2 The measured traps

Three of these are the difference between a working daemon and a plausible-looking one, and each is
regression-tested against the measurement rather than against a description:

1. **A PSI trigger write must end in a NUL byte.** The only legal payload is
   `"some 150000 2000000\n\x00"` (21 bytes). The kernel overwrites the last byte with NUL, so a
   20-byte write silently loses its last digit and reads back as a rejected API. `triggerBytes` is
   the only producer of trigger bytes and its golden test asserts the terminator.
2. **An unarmed PSI fd is always ready.** Polled with a zero timeout it reported **338,711 hits in
   200 ms** on this host — a 100 % CPU busy loop that a naive reader mistakes for pressure. The fd
   is therefore added to an epoll set only *after* a successful arm, by the same goroutine, and the
   guard panics otherwise. `psi.armed_fds == len(epoll_sets) == 0` must hold at shutdown.
3. **A journal seek failure is never "no logs".** `journalctl -n 0 -o json --after-cursor=<bad>`
   exits rc=1 with zero stdout and `Failed to seek to cursor: Invalid argument`. That is
   `TROUBLE-SENSORS-007`, a `--since=<last_entry_ts - 1s>` rescan, and one gap record. If the
   fallback timestamp is outside the journal's retention the entries are gone: `est_lost` stays `-1`
   and `cause_detail` says `retention_window_exceeded`.

### 5.3 Where the state lives

```
<state-root>/spool/journald/<scope>.cursor    0600, one per follow scope, written every 25
                                              entries or 5 s; the resume point after a restart
<config>/rules.d/*.toml                       the rule set, sorted by filename; a broken file
                                              never disarms the daemon
```

`Stop` (SIGTERM, bounded by the unit's `TimeoutStopSec=15`) drains in order: canary/heartbeat →
inotify watches → disk/timers sweeps → D-Bus connections → `journalctl` (SIGTERM to the child's
process group, join with a 5 s bound, then SIGKILL) → PSI triggers → a final gap for any scope still
degraded. It returns only after every goroutine has joined; the process never exits with an armed
trigger or a live child. Measured on this host: **36 ms**.
