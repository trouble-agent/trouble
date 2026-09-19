# SPEC-03 — sensors: PSI, journald, D-Bus, disk, timers, inotify, rules (trouble v0.1)

Spec: SPEC-03
Area prefix: TROUBLE-SENSORS
Package: internal/sensors
Consumed types: Record, RecordKind, Origin, Actor, Sig, SigSource, SensorKind, SensorEvent, SensorHealth, SourceLiveness, Rule, Condition, Breaker, Severity, Rung, Duration, GapRecord, ConfigValue
Local types: sensorConfig, psiMode, psiTrigger, ruleSet, compiledRule, exprProgram, evalScope, conditionAST, journalFollower, dbusWatch, sensorRuntime, breakerKey
ACs: AC-1, AC-2, AC-4, AC-19
PRD: §04a, §11

## 1. Purpose

`internal/sensors` is the detection plane: six sensor kinds (§3.1) feed **one** ladder (SPEC-05) through
**one** signature space (SPEC-01 §3.3 dedup). It owns four things and nothing else:

1. **Observation** — PSI counters, journald entries, systemd D-Bus state, filesystem capacity, timer
   runs, inotify events — normalized into `SensorEvent` and emitted as `event` ledger records.
2. **Rule evaluation** — the rule TOML schema (§3.5) and the **shared condition language** (§3.4),
   defined once here and reused verbatim by SPEC-06 (`when:` in plays) and SPEC-05 (rule gates).
3. **Storm control** — breakers and per-rule/per-sig/source caps (AC-4, §3.8).
4. **Liveness** — per-sensor heartbeat expectations, gap records on every dropout, and the
   `SensorHealth` / `SourceLiveness` inputs the dashboard and verification read (AC-19, §3.9).

Binding constraints encoded here: **sampling is the source of truth; PSI triggers are an accelerator
only** (§3.2); a seek failure is never "no logs" (§3.3); this fleet's units are **user** units, so
system-only D-Bus watching is an ALL-GREEN lie (§3.3); every wake re-reads its counters (§3.2); an
unarmed PSI fd is never placed in an epoll set (§3.2); memory pressure on the host is itself an
incident the daemon files about itself (§3.5, rule `self_memory_pressure`); a hot-reload failure keeps
the previous rule set (§3.7).

Sensors are **detection-only**: they never call the registry, never hold a tool handle, never spawn
anything. Mutating capability lives in the ladder (SPEC-05) and the registry (SPEC-06). v0.1 ships
sampling + triggers + the five other sensors + rules; the cgo `sdjournal` variant is an interface seam
only (§3.3, tagged v1.0 hand-off).

AC / design-constraint map (SPEC-INDEX §7 step 2): **AC-1** → §3.4, §3.5, §3.2, §3.6, §7 · **AC-2** →
§3.5 (`for=`), §3.6 (M2 reopen), §7 · **AC-4** → §3.8, §7 · **AC-19** → §3.3, §3.9, §7.
Everything else in this file is a **design constraint** (no AC of its own): §2 (composition seam),
§3.1/§3.3 (normalization + collectors that AC-1/AC-4 consume), §3.7 (reload atomicity, no AC — the
ladder's ACs assume it), §3.10, §4, §5, §6, §8.

## 2. Interface

```go
package sensors

// Narrow dependencies: the composition root (cmd/trouble) passes ledger.Writer.Append and
// scrubber.Scrub. Only shared types (SPEC-TYPES) cross this boundary.
func New(vals []types.ConfigValue,
         emit func(context.Context, types.Record) error,
         redact func([]byte, string) (types.ScrubResult, error),
         now func() time.Time) (*Sensors, error)

func (s *Sensors) Probe(ctx context.Context) error      // capability + PSI grammar probe; writes one capability_probe event
func (s *Sensors) Start(ctx context.Context) error      // starts enabled sensors; Probe() first when not yet probed
func (s *Sensors) Stop(ctx context.Context) error       // SIGTERM drain: current sample flushed, children reaped, triggers closed
func (s *Sensors) Reload(ctx context.Context) error     // atomic rule-set swap (SIGHUP / inotify), §3.7
func (s *Sensors) Health() []types.SensorHealth         // /health.json sensors[] (SPEC-10), non-blocking, ≤50ms
func (s *Sensors) Liveness() []types.SourceLiveness     // per host_id:source liveness for verification (SPEC-05)
func (s *Sensors) Breakers() []types.Breaker            // dashboard /breakers + ladder gate input
func (s *Sensors) Rules() []types.Rule                  // dashboard /rules (resolved, post-validation)
func (s *Sensors) Canary(ctx context.Context, scope string) (string, error) // injects a canary event through the real pipeline
```

Rules for this interface:

- **No HTTP routes.** This area owns no route; SPEC-10 renders `Health()`, `Liveness()`, `Breakers()`,
  `Rules()` on `/health.json`, `/breakers`, `/rules` and the overview.
- **No exported structs.** Configuration arrives as resolved `ConfigValue`s (SPEC-12 owns precedence:
  flag > env > file > default) and is decoded into the package-private `sensorConfig`; every runtime
  type in §3.10 stays unexported so no spec can depend on its shape.
- **CLI verbs** wired by `cmd/trouble` to these methods: `trouble sensors probe` (runs `Probe`, prints
  the `capability_probe` record it wrote), `trouble rules validate [dir]` (loads + validates, prints
  per-rule verdicts, exit 2 on any TROUBLE-SENSORS-017/018), `trouble rules reload` (`Reload`).
- **Emitted record kinds** (SPEC-INDEX §3.4, disjoint): `event`, `gap`, `canary`. `breaker` records are
  emitted by `internal/ladder` (SPEC-05) at each transition; sensors own breaker *state and
  evaluation* and publish it through `Breakers()`.

## 3. Data model

### 3.1 Sensor inventory, scopes and normalization

| Sensor (`SigSource`) | Scope vocabulary | Mechanism | Cadence | `value`/`unit` |
|---|---|---|---|---|
| `psi` | `cpu`, `memory`, `io` | read `/proc/pressure/{cpu,memory,io}`; optional trigger wake | sample `2s`; wake rate-limited 1/window | stall pct / `pct` |
| `journald` | unit name or `_COMM`-derived app key | `journalctl -f -o json --after-cursor=<c>` child | stream + 30s seek probe | match count / `count` |
| `dbus` | visible unit name | `org.freedesktop.systemd1` signals on system **and** user managers | stream + 30s ping + 300s reconcile | state code / `""` |
| `disk` | mount path (visible, never `/dev/*`) | `statfs(2)` per configured mount | 60s | free pct / `pct` |
| `timers` | timer unit name | D-Bus `ListTimers` + unit state + journald corroboration | 60s | missed runs / `count` |
| `inotify` | watched path | inotify watches (rules dir always; configured paths) | event stream + 15m recheck | event count / `count` |

`SensorEvent.Scope` is exactly one of the values above; `SensorEvent.Detail` carries the raw counters
that rules read (§3.4) and `SensorEvent.Wake` is `true` only for trigger-wake production.

**Sig normalization recipes** (the field set SPEC-01 §3.3 carries for these six sources; this spec is
their authoring source). Normalized bytes are lowercase, `\n`-joined `k=v` lines, values masked:

| Source | Normalized fields (in this order) |
|---|---|
| `psi` | `scope`, `metric` (`some`\|`full`), `band` (`avg10`\|`avg60`\|`avg300`), `rule` |
| `journald` | `unit` or `comm`, `priority`, `masked_message` (mask spec below) |
| `dbus` | `unit_key` (visible name), `failure_class` (§3.6 M1), `crash_loop` (`0`\|`1`) |
| `disk` | `mount`, `kind` (`space`\|`inode`) |
| `timers` | `timer_unit`, `missed_bucket` (`1`,`2-4`,`5-9`,`10+`) |
| `inotify` | `path`, `mask_class` (`create`\|`modify`\|`delete`\|`attrib`\|`overflow`) |

`masked_message` masks (applied before hashing; `norm_version = 1`): RFC3339 timestamps, `\b\d+\b`
runs of ≥6 digits, hex ≥8, UUIDs, absolute paths' basename-resolved prefix (`/home/<user>` →
`/home/<user>`), PIDs, ports, and any substring a SPEC-02 scrub rule removed. Rule name is part of the
`psi` sig so two rules over one resource are two signatures, never one.

### 3.2 PSI — measured contract (AD-1 loaded verbatim)

**Measured facts this section encodes:** `/proc/pressure/*` are mode 0666 and readable by everyone;
trigger writes are `"<some|full> <stall_us> <window_us>"`; **a write without a terminator byte returns
EINVAL** (the kernel overwrites the last byte with NUL, so a naive 20-byte string loses its last digit
and looks like a rejected API); a NUL-terminated write is accepted; unprivileged windows must be a
**multiple of 2 s and ≥2 s** (1 s and 5 s → EINVAL; 2 s and 4 s accepted); a second write on an armed fd
→ **EBUSY**; notifications are rate-limited to **1 per window**; an **unarmed** PSI fd polled with a
zero timeout returned **347,378 hits in 200 ms** (always ready — a 100% CPU busy loop) and a naive
reader can mistake that `POLLERR` for "pressure"; **POLLERR means the source is gone**; wakeups are
coarse (one epoll run returned at ~2.04 s despite a 500 ms timeout); `20 s` windows are accepted on this
kernel although the docs state a 10 s maximum (docs are stale → probe, never hard-code).

Normative rules:

- **P1 Terminator.** The only legal trigger write is one `Write` of
  `[]byte(fmt.Sprintf("some %d %d\n", stallUS, windowUS) + "\x00")`. Golden fixture (21 bytes):
  `"some 150000 2000000\n\x00"`. No other construction may reach `write(2)`; the builder is the only
  producer of trigger bytes and a unit test asserts the trailing `\n\x00`.
- **P2 Window.** `window_us` must be a multiple of 2 s and ≥2 000 000 µs; `stall_us` must be > 0 and
  < `window_us`. Config default `sensors.psi.window = "2s"`; accepted maximum discovered by probe (P9).
- **P3 Sampling is truth.** `avg10/avg60/avg300` sampling every `sensors.psi.sample_interval`
  (default `2s`, min `1s`, max `10s`, and always ≤ `window`) is the only source that can evaluate
  rules. Triggers exist to shorten latency for rules whose metric is expressible as "X µs of stall in a
  window"; a trigger can never express `avg10`, so a rule referencing `avg*` is evaluated from samples
  and from the post-wake re-read only.
- **P4 Wake ≠ crossing.** Every wake re-reads the counters before any rule evaluation; an event
  produced by a wake carries `Wake: true` and the re-read values. If the re-read does not satisfy the
  rule's match, no incident state changes — the wake is counted as `psi.spurious_wakes` and the rule's
  `for=` timer is untouched. Wake events alone never open an incident.
- **P5 One goroutine, own epoll, per trigger.** Each armed trigger gets one dedicated goroutine with
  its **own** `epoll` set (`golang.org/x/sys/unix`), the PSI fd, and an `eventfd` for shutdown. PSI fds
  are **not** Go-netpoller compatible: `os.File.SetDeadline` has no effect, so the trigger fd is never
  registered with the runtime netpoller and never wrapped in an `os.File` used for deadlines.
- **P6 Never arm-in-epoll-less, never epoll-an-unarmed fd.** `epoll_ctl(EPOLL_CTL_ADD)` is called only
  after a successful arm, by the same goroutine, and the fd is removed and closed on every exit path.
  A guard function panics in tests if an fd is added before arming (this is the 347k-hits/200 ms
  regression) and `psi.armed_fds` is asserted `== len(epoll_sets)` at shutdown.
- **P7 Errors are distinct.** EBUSY (already armed) → TROUBLE-SENSORS-003; EINVAL (grammar/shape or
  privilege) → TROUBLE-SENSORS-002 (root) or TROUBLE-SENSORS-005 (non-root); EPERM/EACCES →
  TROUBLE-SENSORS-005; POLLERR on an armed fd → TROUBLE-SENSORS-004 (source gone), and a source that
  stays gone trips the PSI heartbeat (TROUBLE-SENSORS-024). Each class has its own record and its own
  `SensorHealth.Reason` string; they are never collapsed.
- **P8 Kernel floor.** Kernel < 5.15 → TROUBLE-SENSORS-001 (the 5.15/5.16 fix for "trigger destroyed
  while being polled"); PSI is disabled entirely, the other five sensors start normally, and `/health`
  reports `status=degraded` with the reason.
- **P9 Probe, never assume.** At startup (and on `trouble sensors probe`) the daemon attempts **one
  real arm** with the P1 shape and `2s` window, then probes the accepted window ceiling downward from
  `20s → 10s → 2s`, and probes `stall=0` (expected EINVAL). The result selects one of two modes and is
  recorded in the ledger as one `event` record (`{seq, rec_id, ts, kind, schema_version, sig:"",
  origin{host_id, hub_id, source:"psi"}, actor, redactions, payload}` — the record is not sig-keyed,
  so `sig` is `""` per SPEC-TYPES §3.2) whose `payload` is:

```json
{"kind":"capability_probe","probe_ts":"2026-09-16T09:00:01.004Z","kernel":"7.0.0-30-generic","psi_read":true,"trigger_arm":"ok","psi_mode":"triggers+sampling","max_window_us":20000000,"stall_zero_rejected":true,"pressure_files_writable":true,"journald":{"binary":true,"cursor_ok":true,"member_adm":true},"dbus_managers":[{"bus":"system","connect":true,"subscribe":true},{"bus":"user:1000","connect":true,"subscribe":true}],"oomd_available":false,"inotify":{"max_user_watches":65536,"max_queued_events":16384},"modes":{"disk":"thresholds","timers":"list+journald","inotify":"rules_dir+configured"},"codes":["TROUBLE-SENSORS-016"]}
```

  The same mode + code appear in `/health` as a machine-parseable reason:
  `SensorHealth{Sensor:"psi", Enabled:true, Degraded:<bool>, Reason:"mode=triggers+sampling probe=ok at 2026-09-16T09:00:01.004Z"}`.
  In `sampling-only` mode the daemon: arms nothing, keeps **no** fd in any epoll set, samples counters
  at `sample_interval`, emits TROUBLE-SENSORS-005 (non-root) or TROUBLE-SENSORS-002 (root) once per
  boot, and marks `Degraded: true`. This resolves the measured disagreement (one seat measured
  unprivileged arming succeeding with NUL termination, another measured EINVAL for **every**
  unprivileged write on kernel 7.0.0-30): the spec assumes neither outcome, probes the host at boot,
  and degrades loudly in the second case. Trigger arming is never attempted speculatively again after
  the probe until the next boot or explicit `trouble sensors probe`.
- **P10 Busy-spin and CPU bounds.** A PSI sampler tick is one file read of ≤4 KB; with `sample_interval
  = 2s` and 3 resources the sensor's steady CPU is ≤0.3% of one core (regression-tested). A wake storm
  cannot raise CPU: notification rate limiting (1/window) caps wake processing at ≤1 per 2 s per armed
  fd.

### 3.3 Collector contracts

**journald.** `exec.CommandContext(journalctl, "-f", "-o", "json", "--after-cursor", cur, "-u", unit…,
"--output-fields", "__CURSOR,MESSAGE,PRIORITY,_SYSTEMD_UNIT,SYSLOG_IDENTIFIER,__REALTIME_TIMESTAMP,_PID,_HOSTNAME", "-n", "0")`
run as a supervised child; `SIGTERM` on shutdown, `Wait` reaped, stderr captured to the log ring.

- Cursor persistence: one file per follow scope at `<state_root>/spool/journald/<scope>.cursor`,
  written after every 25 entries or 5 s, whichever first; `<scope>` is the unit name (or `all`).
- Child death → TROUBLE-SENSORS-008, restart with backoff `250ms, 500ms, 1s, 2s, 4s, 8s, 16s, 30s` (cap)
  with ≤10% jitter, resume from the persisted cursor, and a `gap` record (`cause = child_died`) for the
  downtime window.
- **Malformed/invalid cursor is a hard failure, never "no logs".** Measured: rc=1 with zero stdout and
  `Failed to seek to cursor: Invalid argument`. Any of {non-zero exit, stderr containing
  `Failed to seek`} → TROUBLE-SENSORS-007 → restart with `--since=<last_entry_ts>` (minus `1s` overlap)
  + rescan + one `gap` record (`cause = cursor_invalid`, `est_lost = -1`). If the fallback timestamp is
  outside the journal's retention, entries are gone: keep `est_lost = -1` and record
  `cause_detail = "retention_window_exceeded"`.
- **Dedupe by `__CURSOR` equality only.** Cursors are not orderable (documented kernel/systemd
  behaviour) and a rescan re-delivers entries; the follower holds a ring of the last `4096` seen cursors
  plus `last_entry_ts` and drops exact repeats. No ordering comparison is ever made.
- Bounded queue: `sensors.journald.queue = 8192` entries / `32MB`, whichever first; overflow →
  drop-oldest, `SensorHealth.Dropped++`, TROUBLE-SENSORS-010, one `gap` record per overflow episode
  (`cause = queue_overflow`, `est_lost = dropped count`). Drops are never silent and never block the
  child (a blocked child would stall journald attribution).
- Per-entry cap `sensors.journald.max_entry = 65536` bytes; longer `MESSAGE` is truncated at the cap
  with `detail.truncated = true` and the byte delta in `detail.dropped_bytes`.
- Non-UTF-8 `MESSAGE`: bytes → `strings.ToValidUTF8(msg, "\uFFFD")`, `detail.non_utf8 = true`, entry
  kept; the raw bytes are never written to the ledger.
- **Unit-scoped by default.** `sensors.journald.units = []` (config); an empty list means the daemon
  follows only its own unit, never the whole journal. `sensors.journald.follow_all = false` exists and
  is refused on hosts whose journal exceeds `sensors.journald.follow_all_max_entries = 2000000`
  (`journalctl --disk-usage` / `-n` count probe) — following a busy journal is a self-inflicted load
  incident.
- Membership: unprivileged read requires membership in `adm` or `systemd-journal`; the group grant is
  **effective only after a restart** of the daemon. The daemon therefore never trusts its own group
  list: it verifies by executing the follow with `-n 1` at boot and every `30s` thereafter. Failure →
  TROUBLE-SENSORS-009 (permanent), sensor `Enabled: true, Degraded: true`,
  `Reason: "no journal read access: uid=<n> not in adm/systemd-journal (restart required after grant)"`,
  and a `gap` record with `est_lost = -1`. Install prints the exact `usermod -aG` line it needs; the
  daemon never attempts a privileged self-repair.
- `journalctl` missing → TROUBLE-SENSORS-006, journald sensor disabled, other sensors unaffected.
- Liveness for journald is the **seek probe**, not entry arrival: a quiet journal is healthy. Every
  `30s` the follower runs `journalctl -n 0 -o json --after-cursor=<cursor>`; rc=0 means the cursor is
  still valid and the journal is readable. Entry silence for any duration is **not** staleness.
- cgo `sdjournal` variant: compiled only under `//go:build journald_sdjournal`, same interface, off in
  every v0.1 artifact — the documented no-subprocess seam (v1.0 hand-off).

**D-Bus.** `github.com/godbus/dbus/v5`, no cgo. Watched managers are configuration, resolved at boot:
the **system** manager (`org.freedesktop.systemd1` on `/var/run/dbus/system_bus_socket`) plus every
user manager listed in `sensors.dbus.user_managers` (resolved as `$XDG_RUNTIME_DIR/bus` with
`/run/user/<uid>/bus` fallback, at most 16 uids). Default `sensors.dbus.user_managers = ["self"]`
(`self` = the daemon's own uid). Watching only the system manager is explicitly rejected as an
ALL-GREEN lie: this class of host keeps its application units in **user** managers, where
`Manager.GetUnit` on the system bus returns `Unit <name>.service not loaded.`

- Subscriptions (per manager): `Manager.Subscribe`, then match rules for
  `type='signal',sender='org.freedesktop.systemd1',interface='org.freedesktop.systemd1.Manager',member='JobNew'|'JobRemoved'|'UnitNew'|'UnitRemoved'`
  on path `/org/freedesktop/systemd1`, and `PropertiesChanged` for
  `org.freedesktop.systemd1.Unit` per watched unit object path. Subscribe **before** any action is
  taken, or the resulting signal is raced away.
- Unit object path escaping (measured: `coding-hermes-scheduler.service` →
  `coding_2dhermes_2dscheduler_2eservice`): every byte outside `[A-Za-z0-9]` becomes `_` + two
  lowercase hex digits, `_` (0x5f) included as `_5f` so the transform is round-trippable. Escaping is
  used **only** to address objects; dedup keys use the visible unit name.
- **No "unit failed" signal exists.** Failure detection is the union of three arrival paths: (1)
  `PropertiesChanged` `ActiveState`/`SubState` (crash/restart signal), (2) `JobRemoved` filtered on the
  job object returned by the manager for the same unit (`Result` = `done|failed|timeout|dependency|
  canceled`), (3) the **startup `ListUnits` reconcile** plus a `300s` periodic reconcile — a unit that
  failed before this daemon started emits no signal at all and is invisible without the reconcile.
- `NameOwnerChanged` for `org.freedesktop.systemd1` (daemon-reexec/reload) → stop trusting the
  connection, TROUBLE-SENSORS-014, re-`Subscribe` + full `ListUnits` resync + re-add unit matches;
  the downtime emits a `gap` record (`cause = bus_down`).
- Bus unreachable/lost → TROUBLE-SENSORS-011, `dbus` sensor degrades with reason, reconnect with
  backoff `1s → 60s`, and a `gap` record (`cause = bus_down`). Silence is never the outcome.
- A configured manager that is expected but not watched (no credential to `/run/user/<uid>/bus`, no
  such bus, `Subscribe` refused) → TROUBLE-SENSORS-013 with the manager identity in the reason; the
  sensor reports `Degraded: true` and the probe record lists the unwatched manager explicitly.
- Unit collisions with systemd's own `Subscribe` gating: systemd only emits `PropertiesChanged` for
  units the private subscription covers, so unit-set churn is handled by a `300s` re-subscribe sweep
  driven by `ListUnits` (add matches for new units, drop matches for removed ones).
- **polkit.** `service.*` calls (`ManageUnit` → `org.freedesktop.systemd1.manage-units`) are
  `auth_admin` on hosts like this one, so an unprivileged daemon is refused. SPEC-03 ships the install
  artifact `packaging/polkit/10-trouble-manage-units.rules` (installed to
  `/etc/polkit-1/rules.d/10-trouble-manage-units.rules`, mode 0644, root-owned) which authorizes
  **only** the trouble uid, **only** for `manage-units`, and **only** for units in the allowlist that
  SPEC-06's do-not-touch list has already narrowed. Missing/refused at call time →
  TROUBLE-SENSORS-012, the distinct POLICY-REFUSED surface (catalog class `permanent`; at the Go level
  it wraps `types.ErrPolicyRefused` and maps to `TROUBLE-REGISTRY-006` for the registry's own audit
  record). The install step verifies the rule is present and loaded and fails loudly if not; the daemon
  never probes polkit by attempting a mutating call.
- **systemd-oomd** is masked on this class of host: the `org.freedesktop.oom1` name is probed at boot
  (`busctl --list`/`GetManagedOOMSwap`) and re-probed every `10m`. Absent → TROUBLE-SENSORS-016,
  documented no-op (no watcher is registered, `oomd_available: false` in the probe record, `/health`
  capabilities card shows it), and the PSI memory rules carry memory-pressure detection instead
  (§3.5, `self_memory_pressure`). If oomd becomes available, the next 10-minute probe starts the
  watcher without a restart and the no-op record is superseded.

**disk.** `unix.Statfs` per mount in `sensors.disk.mounts` (default `["/"]`); cadence `60s`; fields
`free_pct = 100*Bavail/Blocks`, `free_bytes`, `inode_free_pct = 100*Ffree/Files`,
`total_bytes`, `read_only` (`ST_RDONLY`), `fstype`. Scope is the visible mount path (never a device
node). statfs failure (unmounted, permission) → TROUBLE-SENSORS-022 with the path, one per mount per
hour (a still-listed-but-unmounted path is a configuration state, not a storm), and the sensor keeps
reporting the remaining mounts. A node with no `Files` data (`Files == 0`) reports
`inode_free_pct = -1` and inode rules treat `-1` as `false` (never as 0% free).

**timers.** `Manager.ListTimers` on every watched manager plus `ListUnits` state for the activated
unit and journald corroboration. Emitted per timer: `next_elapse`, `last_trigger`, `missed_runs`.
A run is **missed** when `next_elapse` is in the past by more than `1` interval and neither a unit
state transition nor a journald entry for the activated unit exists inside that interval;
`missed_runs` counts consecutive misses and resets on the next observed run. `ListTimers` decode
failure → TROUBLE-SENSORS-023 (permanent) with the raw reply size recorded; the sensor stays enabled
and retries on the next tick. Timers depend on D-Bus: a dead bus marks `timers` degraded with
`Reason: "dependency dbus degraded"` and produces **no** second gap record (one cause, one record).

**inotify.** The rules directory is always watched (this is the hot-reload trigger, §3.7); additional
paths come from `sensors.inotify.paths` (`{path, mask, recursive, max_depth, rule}`), `recursive`
bounded by `max_depth` default `8`. Masks: `IN_CLOSE_WRITE|IN_MOVED_TO|IN_CREATE|IN_DELETE|IN_ATTRIB|
IN_Q_OVERFLOW`. Watch capacity is checked at boot and every `15m` against
`/proc/sys/fs/inotify/max_user_watches`: `used/limit > 0.8` → TROUBLE-SENSORS-020 with both numbers;
`add_watch` returning ENOSPC → TROUBLE-SENSORS-020, the watch is dropped and **named** in the record
(never a silent partial watch set). `IN_Q_OVERFLOW` → re-add every watch, one `gap` record
(`cause = queue_overflow`, `est_lost = -1`) and TROUBLE-SENSORS-021; the queue depth read is
`/proc/sys/fs/inotify/max_queued_events` (16384 default) so overflow is reported against a real
number. With `sensors.inotify.paths = []` the sensor still runs for the rules directory; with
`enabled = false` it reports `Enabled: false`.

### 3.3a The startup reconcile's batch write path (TRBL-044)

The §3.3 startup `ListUnits` reconcile decides one record per failed unit — the §3.3 incident model —
but it does not have to PAY one group-commit window per record (SPEC-12 §7b measured that cost as ~92 %
of a loaded boot). The write half of the reconcile is the batch path:

- **Decide everything, then write once.** The sweep runs the §3.6 merge rule per failed unit and the
  full §3.4/§3.5/§3.8 rule pipeline per outcome (stabilization, cooldowns, breakers, incident opens —
  exactly as the per-record path decides them), collects the drafts, and writes them through ONE
  durable batch (SPEC-01 §3.5a `AppendBatch`). N failed units cost `ceil(N/ledger.max_batch_records)`
  group commits instead of N.
- **The record is unchanged.** A batch member carries the same kind (`event`), the same sig
  (norm_version 1, `(manager, unit, state)`), the same `arrival_path = reconcile` and the same merge
  accounting payload the per-record path wrote. Nothing that queries, dedups or ladders the record can
  tell the write shapes apart.
- **The ladder bridge is preserved in order.** A firing batch member is admitted AFTER the batch is
  durable, with the durable record's identity — the same ordering the per-record emit gave (`Append`
  returned before `Admit`). The composition root re-arms this bridge on the sensors at boot (the
  per-record bridge lives inside `emit`, which the batch path bypasses).
- **Crash loss (the §3.5a conflict rule, applied).** A SIGKILL between the sweep's first unit and the
  batch's durable ack can lose the whole sweep's batch — up to N records naming already-failed units.
  The loss is bounded by `ledger.max_batch_records` and self-healing: failed units stay failed, so the
  next startup reconcile or the 300s sweep re-observes them and re-emits into the same incidents
  (§3.6 M1–M4 counts the re-observation). The worst case is delayed detection, never a silently
  retired failure. A batch write failure (device error) is louder still: no partial sweep, a
  TROUBLE-SENSORS-015 record naming the batch, and a drop count for the whole sweep.
- **Compatibility rule.** Without the composition root's batch boundary the reconcile falls back to the
  per-record emit — the §3.8 shape, durable-on-return per record — so a `sensors.New`-built plane
  behaves exactly as before. The batch is a boot wiring decision (the daemon installs `AppendBatch` as
  the boundary), never a config key: no operator can accidentally batch (or unbatch) the reconcile.

### 3.4 Condition language (the single expression dialect)

One language, defined once here, used by: rule `match` (§3.5), play task `when:` (SPEC-06), and rule
gates in the ladder (SPEC-05). No second dialect, no per-caller extensions. A play `when:` and a rule
`match` entry are the same AST, the same evaluator, and the same error codes; SPEC-06 references this
section and may only add variables to the closed namespace table below.

```
expr       := orExpr
orExpr     := andExpr ( "or" andExpr )*
andExpr    := notExpr ( "and" notExpr )*
notExpr    := "not" notExpr | primary
primary    := "(" expr ")" | comparison
comparison := operand op operand
op         := "==" | "!=" | ">" | ">=" | "<" | "<=" | "~" | "in"
operand    := number | string | bool | var | list
var        := ident ( "." ident )*
list       := "[" ( operand ( "," operand )* )? "]"
string     := '"' … '"' | "'" … "'"
number     := [+-]? digits [ "." digits ] [ ("e"|"E") [+-]? digits ]
```

Semantics, pinned:

| Rule | Decision |
|---|---|
| Types | `number` (float64), `string`, `bool`, list literals of homogeneous `number` or `string`. No null, no map. |
| Coercion | Integer→float widening only. Every other type mismatch is a **compile-time** error (TROUBLE-SENSORS-018): `value >= "35"` where `value` is a number does not coerce, it refuses. String ordering with `>`,`<`,`>=`,`<=` is a compile error. |
| `==` `!=` | Same-type comparison, exact. `number` compares float64 with `±1e-9` relative tolerance so `0.1+0.2`-style detail values behave. |
| `~` | RE2 regex (`regexp.Compile` at load), **unanchored** (`MatchString` semantics), pattern ≤512 bytes, compiled once per rule. Invalid pattern → TROUBLE-SENSORS-018. No PCRE features, no backtracking risk. |
| `in` | Right operand must be a list literal; `string in [..strings]` and `number in [..numbers]` only. Encoded in a `Condition.Value` string as a JSON array literal, e.g. `value = "[\"io\",\"memory\"]"` with `value_type = "string"` (the element type). Empty list compiles and is always `false`. |
| Truthiness | A bare variable is legal only when its type is `bool` (`enabled`, `crash_loop`). A bare number or string is a compile error — no implicit truthiness. |
| Precedence | `not` > `and` > `or`; parentheses always available and required for mixed intent. |
| Arithmetic / functions | **Not in the language.** No `+ - * /`, no function calls, no ternaries. A threshold that needs a derived number is emitted by the sensor as a field (`free_pct`, `stall_ratio`). Reason: one dialect, no precedence bugs, a ~300-line evaluator, and no way for a rule to compute something the sensor cannot re-verify. |
| Field namespace | Closed table per source (below). A rule referencing a field outside its source/scope row is TROUBLE-SENSORS-018 **at load** — a typo can never become a silently never-firing rule (the inert-feature class). |
| Missing field at runtime | Statically validated fields cannot be missing; a field that is conditionally present (`full_*` on non-memory resources) evaluates to `false` and increments `rule.field_absent`. |
| Bounds | Expression ≤4096 bytes, ≤32 `match` entries per rule, AST depth ≤16, ≤64 variables. Exceeding any bound → TROUBLE-SENSORS-018. |
| Missing variable in a play | An unknown `result.output.<k>` in `when:` is `false` + `rule.field_absent`, never an error (SPEC-06 references this row). |

Variable namespace (closed; every name resolvable for the listed source):

| Namespace | Fields |
|---|---|
| any sensor event | `value`, `unit`, `scope`, `sensor`, `source`, `count`, `window_s`, `age_s`, `severity`, `substr`, `msg` |
| `psi` | `metric` (`some`\|`full`), `band`, `some_avg10`, `some_avg60`, `some_avg300`, `full_avg10`, `full_avg60`, `full_avg300`, `some_total`, `full_total`, `stall_us`, `wake` |
| `journald` | `unit`, `comm`, `priority`, `priority_name`, `non_utf8`, `truncated` |
| `dbus` | `unit_key`, `unit_active_state`, `unit_substate`, `unit_result`, `exit_code`, `crash_loop`, `arrival_path` (`properties_changed`\|`job_removed`\|`journald`\|`reconcile`) |
| `disk` | `mount`, `fstype`, `free_pct`, `free_bytes`, `total_bytes`, `inode_free_pct`, `read_only` |
| `timers` | `timer_unit`, `missed_runs`, `next_elapse_in_s`, `last_run_age_s` |
| `inotify` | `path`, `mask_class`, `overflow` |
| sentinel (SPEC-04) | `level`, `release`, `env`, `culprit`, `release_diff` |
| play task (SPEC-06) | `result.changed`, `result.output.<key>`, `registered.<name>`, `item`, `run.attempt`, `inc.severity`, `inc.state`, `autonomy.mode` |

Test vectors (executed in `expr_test.go`, both directions):

| Expression + vars | Result |
|---|---|
| `some_avg10 >= 35` (41.7) | true |
| `some_avg10 >= 35 and scope in ["io","memory"]` (41.7, io) | true |
| `free_pct < 10 or inode_free_pct < 10` (22.0, 30.0) | false |
| `unit_substate == "failed"` | true |
| `msg ~ "(?i)panic|segfault"` | true |
| `not (value > 100)` (41.7) | true |
| `count in [1,2,3]` (3) | true |
| `enabled` (true) | true |
| `full_avg60 >= 5` on a `cpu` event | compile error TROUBLE-SENSORS-018 (cpu has no `full`) |
| `value` (number, bare) | compile error TROUBLE-SENSORS-018 |
| `release > "1.0"` | compile error TROUBLE-SENSORS-018 |
| `some_avg10 >= "35"` | compile error TROUBLE-SENSORS-018 |
| `some_avg10 >= 35 and` | compile error TROUBLE-SENSORS-018 |
| `msg ~ "([unclosed"` | compile error TROUBLE-SENSORS-018 |

### 3.5 Rule TOML schema

Rules live in `rules.d/*.toml` (config dir, sorted by filename; `rules.d` default =
`<state_root>/../config/rules.d`, overridable). Each file decodes into `[]types.Rule` — the shared type
is the schema, field-for-field, so there is no second rule shape. Duplicate `name` across files →
TROUBLE-SENSORS-017 for the whole new set.

```toml
# examples/rules/10-defaults.toml — shipped defaults; no host-specific values
[[rule]]
name = "psi_io_some_avg10_high"
enabled = true
source = "psi"
for = "30s"
entry_rung = "play"
severity = "high"
cooldown = "10m"
max_runs = 2
verify_window = "10m"
auto_grants = ["proc.top", "proc.connections"]
hotfix = false

[[rule.match]]
field = "scope"
op = "in"
value = "[\"io\"]"
value_type = "string"

[[rule.match]]
field = "some_avg10"
op = ">="
value = "35"
value_type = "number"
```

| Field | Type | Default | Meaning |
|---|---|---|---|
| `name` | string, `[a-z0-9_]{3,64}`, unique | — | rule identity; survives reload (stabilization/cooldown/breaker state is keyed by it) |
| `enabled` | bool | `true` | disabled rules are validated, never evaluated |
| `source` | `psi\|journald\|dbus\|disk\|timers\|inotify` | — | selects the sensor; a rule never spans sources |
| `match` | array of `Condition` (array-of-tables canonical; inline tables accepted) | `[]` | ALL entries must hold (implicit AND); `[]` matches every event of `source` |
| `for` | duration | `0s` | stabilization: the match must hold **continuously** for this long before the ladder is told (AC-2). `0s` fires on the first qualifying event |
| `entry_rung` | `record\|play\|research\|agent` | `record` | where the ladder enters (SPEC-05); `outlets` is not a legal entry rung |
| `severity` | `critical\|high\|medium\|low\|info` | `medium` | incident severity and the flow/board priority mapping (SPEC-08) |
| `cooldown` | duration | `5m` | after a fire, this rule cannot fire again for the same sig until it elapses |
| `max_runs` | int ≥1 | `2` | play attempts before the next rung (SPEC-05 N) |
| `verify_window` | duration >0, ≤`1h` | `10m` | passed to the incident; invalid → TROUBLE-SENSORS-017 |
| `auto_grants` | []string | `[]` | module/scope names auto-granted in `assisted` mode; every entry must exist in the registry or the rule fails validation (TROUBLE-SENSORS-017) |
| `hotfix` | bool | `false` | whether the incident is eligible for the hot-fix lane (SPEC-08) |

Shipped default set (examples only, nothing fleet-specific): `psi_cpu_some_avg10_high`
(`some_avg10 ≥ 25`, `for=60s`, medium), `psi_io_some_avg10_high` (as above), `self_memory_pressure`
(`source=psi`, `scope in ["memory"]`, `some_avg10 ≥ 8`, `for=30s`, `severity=high`,
`entry_rung=play`, `auto_grants=["proc.top"]`, `hotfix=false`) — the daemon's own incident **about
itself** (memory pressure is a first-class incident, AD-H), `dbus_unit_failed`
(`unit_substate == "failed"`, `for=0s`, high, `entry_rung=play`,
`auto_grants=["service.reload"]`), `dbus_crash_loop` (`crash_loop == true`, critical,
`entry_rung=research`), `disk_space_low` (`free_pct < 10`, `for=5m`, high),
`disk_inode_low` (`inode_free_pct >= 0 and inode_free_pct < 10`, `for=5m`, medium),
`timers_run_missed` (`missed_runs >= 2`, `for=0s`, medium), `inotify_queue_overflow`
(`overflow == true`, `for=0s`, low). Rule limits: ≤256 rules per source, ≤1024 rules total; exceeding
either → TROUBLE-SENSORS-017.

### 3.6 Merge rule for the three arrival paths (normative; SPEC-05 consumes it verbatim)

One underlying unit failure produces up to three arrivals: `PropertiesChanged`(SubState),
`JobRemoved`(Result) and journald entry. They must become **one** incident, one group, one story.

- **`unit_key`** = the visible unit name (`payment-worker.service`). Escaped paths are never keys.
- **M1 — derive the sig inside the merge window.** The unit path's `failure_class` is
  `JobRemoved.Result` when a `JobRemoved` for that unit arrives within `merge_window` (default `5s`) of
  the first arrival, otherwise the observed `SubState`. Sig normalization fields: `unit_key`,
  `failure_class`, `crash_loop` (§3.1). Arrival path is **not** part of the sig — that is what makes the
  merge possible.
- **M2 — one incident per (unit_key, failure_class) per storm window.** First arrival opens; further
  arrivals inside `merge_window` increment the group count and attach evidence; arrivals after
  `merge_window` with the same `(unit_key, failure_class)` while the incident is open increment the
  count, and while it is resolved reopen the **same** incident (SPEC-05, `reopen_count++`). No second
  incident is ever opened for the same key inside `3600s` unless `crash_loop` flips (M4).
- **M3 — journald enriches, it does not duplicate.** A journald entry whose `_SYSTEMD_UNIT` matches a
  `unit_key` with an open incident, or one opened inside `merge_window`, is attached to that incident
  as evidence (`detail.attach = true`, `detail.arrival_path = "journald"`, `journal_tail_ref` to the
  scrubbed tail) and opens nothing. journald opens its own incident (source `journald`) only when no
  unit incident is open for that unit **and** a `source = "journald"` rule matched the entry.
- **M4 — crash loop.** ≥3 failures of one `unit_key` inside `300s`: raise the incident to `critical`,
  set `detail.crash_loop = true`, and re-derive the sig with `crash_loop=1` so the crash-loop class is
  its own signature (the `dbus_crash_loop` rule promotes the rung to `research`). The crash-loop
  incident absorbs further failures of that unit for `1800s`.
- Merge accounting is observable: `sensors_dbus_merges_total` and `sensors_dbus_arrivals_total{path}`
  exist so a "3 arrivals → 3 incidents" regression is caught by numbers, not by reading dashboards.

### 3.7 Hot-reload atomicity

Triggers: `SIGHUP`, inotify `IN_CLOSE_WRITE|IN_MOVED_TO|IN_DELETE` on `rules.d` (debounced `500ms`),
`trouble rules reload`, and a `60s` mtime sweep as a backstop.

Sequence: read all files → decode (§3.5) → compile every expression (§3.4) → run the full validation
(§3.5 table, source/scope field table, `auto_grants` existence) → if **any** file fails, keep the
previous rule set and emit TROUBLE-SENSORS-019 (one `event` record, `payload.kind = "rule_reload"`,
`payload.error_code`, file + rule name + line) and set
`SensorHealth{Reason: "rule reload failed at <ts>: TROUBLE-SENSORS-018 (rules.d/20-app.toml:14)"}` for
the affected sensors. The previous set stays authoritative — a broken edit never disarms the daemon.

- **Swap.** The live set is held in `atomic.Pointer[ruleSet]`; a successful reload installs one new
  immutable set in a single pointer store. There is no lock and no partially-visible set.
- **In-flight evaluations** finish against the snapshot they started with (an evaluation is bounded by
  the `2ms` budget below, and swapping cannot be observed mid-evaluation by construction). Events
  already queued are evaluated against the **new** set when their turn comes; no event is evaluated
  twice.
- **Stabilization timers** are keyed by `(rule.name, sig)`. On reload: rule present with byte-identical
  `match` + `for` → timer and its elapsed window are carried over; rule present with a changed `match`
  or `for` → the timer resets and `detail.rule_state_reset = true` is recorded; rule removed → the
  timer is dropped. Cooldown and breaker state are keyed the same way and always survive.
- **No retroactive evaluation.** New or changed rules apply to events observed after the swap; the
  daemon never replays the ledger through a new rule (that would mint incidents from history). The
  loadable history window for dashboards is unaffected.
- **No incident retraction.** A reload never closes, reopens or deletes an incident; incident lifecycle
  belongs to the ladder (SPEC-05).
- **Budgets.** Reload of 256 rules ≤50ms (CI regression asserts <250ms on the reference box); a reload
  exceeding `1s` is abandoned mid-way, the previous set is kept, and TROUBLE-SENSORS-019 records
  `reason = "reload_timeout"`.
- **Loop guard.** 3 consecutive failed reloads → the inotify trigger is disabled (recorded), the
  `60s` mtime sweep continues, and only `SIGHUP` retries immediately. This prevents an editor's
  half-written file from producing an error storm.

### 3.7a Content-derived reload backstop fingerprint

The `60s` sweep (§3.7) needs a detector that answers one question: "would a reload produce a
different rule set?" The detector is derived from the path, the size, the mtime **and a content
digest** (SHA-256 of a rule file's bytes) of every `*.toml` file under `rules.d`, folded into one
fingerprint.

- **Why the digest is load-bearing.** Path + size + mtime alone is blind to a rewrite that lands
  inside one filesystem timestamp tick. A coarse-granularity filesystem, a tar-synced or restored
  tree, and an editor that preserves mtime all produce a real content change with an identical size
  and an identical mtime; without the digest the backstop reports "nothing changed" and the daemon
  keeps evaluating a stale rule set while `rules.d` says otherwise. With the digest, a same-size,
  same-mtime rewrite is a change like any other.
- **Stability (no reload storm).** An identical file set, identical bytes and identical stat data
  produce an identical fingerprint — for a one-file directory and for a many-file directory — so a
  quiet `rules.d` never triggers a reload on the sweep's tick.
- **Change set.** A moved fingerprint means any of: content edit, size change, mtime change (a bare
  `touch` with unchanged content is still a change, matching `SIGHUP` semantics), file added, file
  removed. A vanished or unreadable file is skipped rather than failing the sweep; its absence from
  the fingerprint is itself a change, so the next tick triggers a reload that reports the read
  problem through the unchanged §3.5 refusal path.
- **The sweep stays a backstop.** `SIGHUP`, the inotify watcher and `trouble rules reload` remain
  the primary triggers of §3.7; §3.7a only makes the last-resort trigger correct on filesystems
  whose timestamps cannot witness an edit.
- **No codes of its own.** §3.7a introduces no new `TROUBLE-SENSORS-*` code. A reload the sweep
  triggers travels the unchanged §3.7 path, including TROUBLE-SENSORS-019 for a refused set.
- **Wiring.** The detector is consumed by `runReloadSweep` at each tick, and by the reload path's
  initial snapshot; no new wiring, no new package surface (§3.10).
- **Test.** `internal/sensors/reload_test.go` `TestRuleDirMTimesDetectsChange` asserts the four
  halves deterministically on any filesystem: a content-only change with size and mtime pinned to a
  frozen instant (fingerprint must change), a natural edit detected by polling with a `3s` deadline
  (long enough for a `1s`-granularity filesystem), two consecutive fingerprints identical with
  nothing changed (one-file and multi-file directories), and add/remove of a rule file each moving
  the fingerprint. No sleep shorter than one poll interval decides the verdict.

### 3.7b An absent rules directory is not a failure

`rules.d` defaults to `<state_root>/../config/rules.d` (§4) and is created by `trouble install` or by the
operator, so on a first boot it does not exist. Absence is a normal state, not a capability failure:

- **Absence ≠ refusal.** A `rules.d` that does not exist leaves the shipped defaults active (§4 step 3)
  and the `inotify` sensor **enabled and not degraded**: `/health.json` keeps `status = "ok"`, and the
  condition is NAMED twice — one informational `event` record (`payload.kind = "sensor_note"`, with
  `path`, `detail`, `error_code = ""`, `fire = false`, at most one per path per boot) and a non-empty
  `SensorHealth.Reason` (`rules dir <path> absent; the shipped defaults are active`) that holds while
  the directory stays absent.
- **TROUBLE-SENSORS-025 is reserved for a path that EXISTS and cannot be watched** — `EACCES` on
  `stat`/`inotify_add_watch`, a symlink loop, an unparseable configured mask, or no inotify in the
  kernel. That is a capability or configuration failure and still degrades the sensor with the path
  named. A `rules.d` that exists but is malformed is unchanged: it refuses through §3.5/§3.7
  (TROUBLE-SENSORS-017/018) and the previous set stays authoritative.
- **The watch is deferred, not dropped.** The rules directory stays the always-on trigger (§3.3): the
  watch is (re)established by the `15m` recheck the moment the directory exists, and until then the
  `60s` content fingerprint sweep (§3.7a) is what notices its creation — a directory that appears is a
  changed fingerprint, which triggers the unchanged §3.7 reload.
- **Reload follows the same rule.** `trouble rules reload` and `SIGHUP` against an absent directory keep
  the current set, emit the same informational record, and **do not** strike the §3.7 loop guard: a
  first-run host has no rule directory to fix, and three strikes there must not disable the inotify
  trigger.
- **No new code.** §3.7b introduces no `TROUBLE-SENSORS-*` code: absence is informational, and every
  failure it does not cover travels the existing refusal path.
- **Test.** `internal/sensors/rules_absent_test.go` pins both sides (absent → enabled, not degraded,
  shipped defaults installed, path named, no code emitted; an unreadable directory and a symlink loop →
  TROUBLE-SENSORS-025 with `Degraded: true`) and `internal/app/missing_rules_dir_test.go` pins the boot:
  a state root with no sibling `config/rules.d` serves `/health.json` with `status = "ok"`, the shipped
  defaults as the live rule set, and no TROUBLE-SENSORS-025 record in the boot's ledger.

### 3.8 Storm breakers and caps (AC-4)

Scopes are the shared `Breaker.Scope` strings: `rule:<name>`, `sig:<sig>`, `source:<kind>`, `global`.
States: `closed → open → half_open → closed`. Sensors evaluate and hold the state; `internal/ladder`
emits the `breaker` ledger record at each transition (SPEC-INDEX §3.4) and suppresses ladder work while
a scope is open (TROUBLE-LADDER-014).

| Scope | Trip condition (measured/normative input) | Cap |
|---|---|---|
| `rule:<name>` | events matching this rule > 120/min sustained 2 min, **or** > 20 incidents opened in 300s | evaluations capped at 120/min; excess is counted, not evaluated |
| `sig:<sig>` | >5 reopens of the same incident in 3600s (flap) | reopens 5/3600s, then suppression |
| `source:<kind>` | sensor event rate > 600/min, **or** sensor error-rate (gap records + code emissions) > 20% of events in 300s | 600 events/min evaluated |
| `global` | > 1200 events/min, **or** > 25 incidents opened in 300s | 1200 events/min; over-cap events are recorded then suppressed |

- **Open duration** = `60s` doubled per consecutive trip, capped at `30m`
  (`open_until = opened_ts + backoff`).
- **Half-open** = after `open_until`, exactly **one** probe event per scope is evaluated; if it would
  trip again the scope returns to `open` with doubled backoff, otherwise it closes and an
  `event` record notes `breaker_closed` with the trip count.
- **Events are never dropped by a breaker.** They are recorded (subject to the bounded queues in
  §3.3) and suppressed at the ladder gate; `Group.Counters.Suppressed` and `SensorHealth.Dropped`
  keep the numbers honest.
- **Per-rule cooldown** (§3.5) is a second, independent limiter: the same rule cannot fire twice for
  the same sig inside `cooldown`.
- Breakers are per host (a single daemon's protection). Their state is exposed in `/health.json` and
  `/breakers` so an open breaker is never invisible.

### 3.8a Sampled-event fold (count-preserving persistence)

P3 makes the sampler the source of truth and §3.8 says a breaker never drops an event, so
`handleEvent` persists one `event` record per observation — one per `sensors.psi.sample_interval` for
a condition that has not changed. That posture IS the amplifier measured on an idle host (TRBL-009,
tick trouble-2026-09-19-08-24-56): one PSI signature wrote 122 event records in 259s, 503 event
records in the window, ~199 MB/day extrapolated from a host with no incident — and every record sat
under the global cap, so no breaker engaged. §3.8a keeps the observations and removes the cardinality:
repeated identical sampled observations become ONE record that carries the count of what it stands
for.

- **What is folded.** One observation qualifies when all three hold: the sampler produced it
  (`detail.sample_backed = true`), it is not a trigger wake (§3.2 P4/P5), and no rule fired for it. A
  firing observation is the ladder's input and is written immediately; a wake is an edge the sampler
  did not produce. Nothing upstream changes: rules (§3.4, §3.5), stabilization, the breakers and the
  per-rule cooldown (§3.8) still see EVERY observation, so `Suppressed`, `evaluations`, `fired`,
  `events_total`, `last_success_ts` and `last_event_age_s` are exactly what they were, and "events are
  never dropped by a breaker" holds unchanged — the `count` on the folded record is the proof of how
  many observations it stands for.
- **The window.** A fold opens on its first observation and covers `[first_ts, first_ts + window)`.
  An observation at or after `first_ts + window` closes the fold it replaces — writing it — and opens
  the next one. Every arrival also closes any fold whose window has already passed, so a signature
  that stops repeating is written by the next unrelated arrival instead of waiting for that signature
  to come back. A non-foldable record for the same signature (a fire, a wake) closes that signature's
  fold first, so the ledger keeps the chronology. `Stop` writes every open fold: an observation that
  already happened is never lost to a shutdown.
- **The record.** Same `kind = event`, same `sig` (the ladder's identity in §3.3 is untouched) and the
  same `origin` as the observations it stands for, carrying the payload of the FIRST folded
  observation plus the fold's own accounting at the payload top level: `count` = the number of
  identical sampled observations the record stands for, `first_ts` / `last_ts` = the window they fall
  in, `fold = true`, and `fold_window_s`. `detail` is carried verbatim from the first observation, so
  the detail shape a consumer already reads does not change.
- **Config.** `sensors.sample_fold_window` (a registered key, SPEC-12 §3.1e, like every other key of the
  surface §4 lists), default `5m`. `0`
  disables the fold and reproduces the per-cycle posture exactly: one record per observation, no fold
  fields. A negative window is refused when the configuration is decoded (§2's loud-failure rule). The
  default equals the per-rule cooldown default (§3.5), so a continuing condition persists records no
  faster than the ladder can act on it: 288 records/day per continuing signature at the 2s default
  sampling interval against 43 200 observations — a 150:1 fold — and 22 464 records/day for the
  78-signature idle-host shape the probe measured (175 583/day), 7.8x fewer.
- **Bound.** At most 4096 folds are held at once, one per distinct signature inside a window. At the
  bound the OLDEST fold is closed — written — rather than discarded: the bound costs record
  compression, never an observation.
- **No codes of its own.** §3.8a introduces no `TROUBLE-SENSORS-*` code. A fold whose record the emit
  path refuses counts as `Dropped` on the producing sensor (§3.9), the same accounting as any other
  emit failure.
- **Test.** `internal/sensors/fold_test.go` asserts it deterministically on the injected clock with no
  wall-clock waits: 150 observations inside a 5m window become one record carrying `count = 150`,
  `first_ts`, `last_ts` and `fold = true`; the boundary observation closes that window and opens the
  next (two windows, two records, 30 + 30); `0` writes one record per observation with no fold fields;
  a fire and a wake are written immediately and the fold they end precedes them; `Stop` flushes an
  open fold; the bound closes a fold instead of dropping one; and one simulated day of a repeating
  signature writes exactly 288 records whose counts add back to 43 200 observations.
  `internal/sensors/dedup_pin_test.go` pins TRBL-009's refuted half: 12 observations inside one 5m
  cooldown produce 12 records (the cooldown gates FIRING, not RECORDING), exactly one of them fires
  under one rule+sig identity, and no breaker opens on that stream.

### 3.9 Sensor liveness, heartbeats and `/health`

| Sensor | Heartbeat / last-success definition | Stale after | On stale |
|---|---|---|---|
| `psi` | sampler tick every `2s` (`sensors.psi.sample_interval`); last success = a completed counter read or wake re-read | `max(5×interval, 10s)` = `10s` | TROUBLE-SENSORS-024 + `gap` + `Degraded` |
| `journald` | seek probe (`journalctl -n 0 -o json --after-cursor=<c>`) every `30s`; last success = rc=0 | `90s` | TROUBLE-SENSORS-024 + `gap` (`cause = child_died` when the child is gone) |
| `dbus` | `Peer.Ping` on each manager every `30s` + a `ListUnits` reconcile every `300s` | `90s` | TROUBLE-SENSORS-024 (a connect failure is TROUBLE-SENSORS-011) |
| `disk` | statfs sweep of every configured mount every `60s` | `180s` | TROUBLE-SENSORS-024 + `gap` |
| `timers` | `ListTimers` + unit-state sweep every `60s` | `180s` | TROUBLE-SENSORS-024 + `gap` |
| `inotify` | watch-set recheck every `900s` (`/proc` counters + fd sanity read); last success = recheck ok | `900s` | TROUBLE-SENSORS-024 + `gap` |

- **Entry silence is not staleness.** journald, D-Bus and inotify may legitimately produce no events
  for hours; liveness is proven by the probe, never by arrivals. `LastEventAgeS` grows without penalty;
  only `LastSuccessTS` decides health.
- **Gaps on every dropout.** Any condition that stops observation emits exactly one `gap` record with
  `GapRecord` fields at the payload top level plus `cause_detail` and `subsystem`:
  `{"id":"ev_…","sensor":"journald","scope":"payment-worker.service","from_ts":"…","to_ts":"…",
  "est_lost":-1,"cause":"child_died","cause_detail":"exit status 1: Failed to seek to cursor","subsystem":"journald-follower:payment-worker.service"}`
  (SPEC-04/07/09/12 reuse this payload schema for their own gaps.)
- **One cause, one record.** A dependency failure (D-Bus down → timers stale) produces the primary
  gap record and marks the dependent sensor degraded with `Reason: "dependency dbus degraded"`, not a
  second gap.
- **Recovery is explicit.** Recovery emits an `event` with `payload.kind = "sensor_recovered"`,
  `detail.downtime_s`, and clears `Degraded`/`Reason`; a degraded sensor that never recovers keeps a
  non-empty `Reason` forever (no silent clearing).
- **Canary.** Every `10m` the daemon injects one canary event through the real pipeline
  (`Canary(ctx, scope)` → normalization → dedup → ledger, `KCanary`) and asserts it lands within `30s`.
  Payload schema (SPEC-04 reuses it with `origin.source = "sentinel:<project>"`):
  `{"kind":"canary","canary_id":"can_01J9Z6","sensor":"psi","scope":"io","injected_ts":"…","observed_ts":"…","latency_ms":12,"landed":true}`.
  A canary that does not land is a `gap` (`cause = canary_missing`) and makes verification `invalid`
  (SPEC-05) — absence of evidence is never evidence of health.
- `/health.json` surface: `Health()` fills `HealthResponse.sensors[]` (`SensorHealth`, one entry per
  enabled sensor, including the PSI mode reason string of §3.2 P9), and `Liveness()` fills
  `HealthResponse.sources[]` with `host_id:source` entries (`expected`, `alive`, `last_event_age_s`,
  `max_age_s` = the table above). Aggregated `status` is `degraded` when any sensor is degraded; the
  dashboard performs that aggregation from these inputs (SPEC-10). Response time ≤50ms and
  allocation-free: `Health()` reads atomics only.
- **Self-observation.** The sensor subsystem's own footprint is an input to the `self_memory_pressure`
  rule: the sampler never increases its cadence under pressure, and when the event pipeline is
  backed up (>75% queue depth) the disk/timers/inotify cadences drop to their configured maximum
  intervals, `detail.load_shed = true` is recorded, and PSI sampling keeps its interval (dropping
  samples under pressure is exactly when they matter).

### 3.10 Package-private types

Never exported, never serialized, never named in another spec; they exist so no exported signature in
§2 leaks an implementation shape.

```go
type sensorConfig struct { psi psiConf; journald journalConf; dbus dbusConf; disk diskConf; timers timerConf; inotify inotifyConf; rulesDir string; sampleFoldWindow time.Duration }

type psiMode string // "triggers+sampling" | "sampling-only" | "disabled"
type psiTrigger struct { resource string; metric string; stallUS, windowUS int64; fd int; epfd int; shutdownFD int }

type ruleSet struct { gen uint64; bySource map[types.SigSource][]compiledRule; loadedTS time.Time }
type compiledRule struct { rule types.Rule; prog []exprProgram; fields []string }
type exprProgram struct { src string; ast conditionAST }
type conditionAST struct { op string; left, right operand; children []conditionAST } // operand stays unexported
type evalScope struct { vars map[string]value; missing uint64 }                    // value stays unexported

type journalFollower struct { scope string; cmd *exec.Cmd; cursor string; lastTS time.Time; ring []string; ringAt int }
type dbusWatch struct { bus string; conn *dbus.Conn; units map[string]struct{}; sub int }
type sensorRuntime struct { kind types.SensorKind; enabled, degraded bool; reason string; lastOK, lastEvent time.Time; events, drops uint64; gaps int }
type breakerKey struct { scope string; name string }
type eventFold struct { window time.Duration; open map[string]*foldEntry; observations, records, overflow, emitFailures uint64 } // §3.8a
type foldEntry struct { key string; sensor types.SensorKind; draft types.RecordDraft; firstTS, lastTS time.Time; count uint64 }  // §3.8a
```

## 4. Wiring

| Producer/consumer | Interface |
|---|---|
| `internal/sensors` imports | `internal/types`, `golang.org/x/sys/unix`, `github.com/godbus/dbus/v5` |
| receives from composition root | `emit` (`ledger.Writer.Append`), `redact` (`scrub.Scrubber.Scrub`), resolved `[]ConfigValue` |
| emits record kinds | `event`, `gap`, `canary` (only) |
| consumed by | `internal/ladder` (event/gap/canary records via the ledger index; `Breakers()` for the gate), `internal/dashboard` (reads `Health()`, `Liveness()`, `Breakers()`, `Rules()`), `internal/lifecycle` (heartbeat file, `/health`) |
| never | sensors never call the registry, never hold a module handle, never spawn a process other than `journalctl` |

Config keys. Every key below is a REGISTERED key of SPEC-12 §3.1 — the sensor
surface is enumerated key by key in SPEC-12 §3.1e — so each one resolves with the
ordinary precedence (flag > env > file > default), carries its own provenance and
appears in `trouble config explain`. The registry mirrors the `case` labels of
`internal/sensors`' own config switch, and a test derives the key list from that
switch, so this list, the registry and the decoder cannot drift apart. All keys
have defaults and no host-specific value is compiled in anywhere:

`…psi.enabled`, `…psi.sample_interval`, `…psi.window`, `…journald.enabled`,
`…journald.units[]`, `…journald.follow_all`, `…journald.follow_all_max_entries`,
`…journald.queue`, `…journald.queue_bytes`, `…journald.max_entry`,
`…journald.probe_interval`, `…dbus.enabled`, `…dbus.user_managers[]`,
`…dbus.ping_interval`, `…dbus.reconcile_interval`, `…dbus.oomd_probe_interval`,
`…disk.enabled`, `…disk.mounts[]`, `…disk.interval`, `…timers.enabled`,
`…timers.interval`, `…inotify.enabled`,
`…inotify.paths[].{path,mask,recursive,max_depth,rule}`, `…inotify.recheck_interval`,
`…inotify.max_depth`, `…rules.dir`, `…rules.reload_debounce`,
`…limits.{rule_per_min,source_per_min,global_per_min,incidents_per_5m}`,
`…merge_window`, `…sample_fold_window` (§3.8a; `0` = off).

Two properties of that surface are pinned by SPEC-12 §3.1e and are part of this
section's contract, because they decide what a config file may say:

- **`sensors.inotify.paths` is a declaration, not a scalar.** Its value is a list
  of tables, one per watched path, written `[[sensors.inotify.paths]]`; the file
  is the source that sets it, and a flag or an environment variable — a scalar —
  is refused by name (TROUBLE-LIFECYCLE-001) rather than flattened into a second
  syntax. The rows are validated here, by §4's own startup step 1: a row without
  a `path` fails the decode.
- **`sensors.rules.dir` defaults to `<state_root>/../config/rules.d`** and is
  derived from the RESOLVED state root, so moving the state root moves the
  directory this plane watches and reloads (§3.7).

Startup order (each step's failure degrades only its own sensor): (1) decode `sensorConfig` from
`ConfigValue`s and fail loudly on an unknown `sensors.*` key; (2) `Probe` — kernel floor, PSI arm
probe + window ceiling, `journalctl` presence, journal read access, D-Bus managers,
`oomd_available`, inotify limits — one `capability_probe` event; (3) load + validate `rules.d`
(TROUBLE-SENSORS-017/018 on failure keeps the shipped defaults); (4) start PSI sampler (+ triggers in
`triggers+sampling`); (5) start journald child at the persisted cursor; (6) connect D-Bus managers,
`Subscribe`, then the **startup `ListUnits` reconcile**; (7) disk sweep; (8) timers sweep; (9) inotify
watches including `rules.d`; (10) start the heartbeat ticker and the canary timer.

Shutdown (SIGTERM, bounded by the unit's `TimeoutStopSec=15`): stop canary/heartbeat → remove inotify
watches → stop disk/timers sweeps → close D-Bus connections → SIGTERM the journald child and flush the
cursor → close every PSI trigger fd (de-registration is by close) and its eventfd → final `gap` for any
scope still degraded. `Stop` returns only after every goroutine has joined; the process never exits
with an armed trigger or a live child.

Flow: `raw source → normalize (§3.1) → scrub (SPEC-02) → SensorEvent → rules (§3.4/§3.5) →
stabilization `for=` → dedup core (SPEC-01 index) → ladder (SPEC-05) → outlets`. Sensors stop at
`SensorEvent` + record emission; they never decide an incident's fate beyond opening it.

## 5. Errors

| Code | Class | Trigger (this spec's surface) | Record + recovery |
|---|---|---|---|
| TROUBLE-SENSORS-001 | permanent | kernel < 5.15 (P8) | boot event + `Reason`; PSI disabled, other sensors start |
| TROUBLE-SENSORS-002 | permanent | root + EINVAL on a legal trigger write (P7/P9) | probe event; `sampling-only`, `Degraded: true` |
| TROUBLE-SENSORS-003 | permanent | EBUSY: fd already armed | close fd, re-open, arm once more; second EBUSY → `sampling-only` |
| TROUBLE-SENSORS-004 | permanent | POLLERR on an armed fd (source gone) | close + re-open + re-arm once; second time → `sampling-only` + gap |
| TROUBLE-SENSORS-005 | permanent | arming denied for this uid (EPERM/EACCES/EINVAL as non-root) | probe event; `sampling-only`; `/health` reason `mode=sampling-only probe=TROUBLE-SENSORS-005` |
| TROUBLE-SENSORS-006 | permanent | `journalctl` not found in `PATH` | journald disabled, `Reason` set, `Enabled: false` |
| TROUBLE-SENSORS-007 | transient | cursor invalid (rc≠0 / `Failed to seek`) | TROUBLE-SENSORS-007 → `--since` + rescan + `gap{cause:cursor_invalid,est_lost:-1}` |
| TROUBLE-SENSORS-008 | transient | journal child exited while following | restart with backoff, `gap{cause:child_died}` for the downtime |
| TROUBLE-SENSORS-009 | permanent | uid not in `adm`/`systemd-journal` (effective after restart) | journald `Degraded: true` + `Reason`; when the follow child exits non-zero because of the denial, one `gap{cause:child_died,est_lost:-1}`; install prints the `usermod -aG` line |
| TROUBLE-SENSORS-010 | transient | bounded journal queue overflow | drop-oldest, `Dropped++`, `gap{cause:queue_overflow,est_lost:N}` per episode |
| TROUBLE-SENSORS-011 | transient | D-Bus connect failed for a configured manager | `dbus` degraded + `gap{cause:bus_down}`; reconnect backoff 1s→60s |
| TROUBLE-SENSORS-012 | permanent (POLICY-REFUSED surface) | polkit refused `manage-units`; `.rules` artifact missing/not loaded | tool-call path returns `ErrPolicyRefused`; SPEC-06 mirrors `TROUBLE-REGISTRY-006`; install verifies the artifact |
| TROUBLE-SENSORS-013 | permanent | a configured manager (system/user for uid N) is not watched | probe record lists it; `dbus` degraded with the manager identity |
| TROUBLE-SENSORS-014 | transient | `NameOwnerChanged` on `org.freedesktop.systemd1` | re-subscribe + full `ListUnits` resync + `gap` for the gap window |
| TROUBLE-SENSORS-015 | transient | `Subscribe`/`AddMatch` failed on a reachable bus | retry ×3, then degraded; never silence |
| TROUBLE-SENSORS-016 | permanent | systemd-oomd absent/masked | documented no-op: no watcher, `oomd_available:false`, re-probed every 10m |
| TROUBLE-SENSORS-017 | permanent | rule file schema invalid / duplicate name / bad `auto_grants` / limits exceeded | reload refused, previous set kept, file+line in the record |
| TROUBLE-SENSORS-018 | permanent | condition expression invalid (grammar, type, field, bounds) | reload refused, previous set kept, rule+line in the record |
| TROUBLE-SENSORS-019 | transient | hot-reload failed or exceeded 1s | previous rule set stays active; `Reason` on affected sensors; after 3 strikes inotify reload disabled |
| TROUBLE-SENSORS-020 | transient | inotify watch limit reached / ENOSPC on `add_watch` | named watch dropped, counts recorded, sensor stays enabled |
| TROUBLE-SENSORS-021 | transient | `IN_Q_OVERFLOW` | re-add all watches + `gap{cause:queue_overflow,est_lost:-1}` |
| TROUBLE-SENSORS-022 | permanent | `statfs` failed for a configured mount | per-mount, at most one record per hour; other mounts continue |
| TROUBLE-SENSORS-023 | permanent | `ListTimers` reply could not be parsed | per-tick retry, raw reply size recorded, no invented timer list |
| TROUBLE-SENSORS-024 | transient | sensor last-success older than its stale threshold | `gap` for the window + `Degraded: true` + `Reason`; `sensor_recovered` on recovery |
| TROUBLE-SENSORS-025 | permanent | sensor disabled: host capability absent (no `/proc/pressure`, no D-Bus, no inotify) | documented no-op, `Enabled:false`, probe record lists the capability |

Every code a sensor returns upward also appears in the ledger record that describes the failure
(`Record.payload.error_code`, SPEC-INDEX §5 rule 3). Codes are emitted at most once per boot per sensor for
the `permanent` capability class (no error storms), and every `transient` code is emitted once per
episode with the ending condition in a matching recovery record.

## 6. Edge cases

1. **Kernel below 5.15** → TROUBLE-SENSORS-001, PSI off, other five sensors normal, `status=degraded`.
2. **Unprivileged arming fails (measured on kernel 7.0.0-30 by one seat, succeeds by another)** →
   probe decides; `sampling-only` mode keeps zero fds in epoll sets and never retries arming
   speculatively. The probe result and the mode are in the ledger and in `/health` — never assumed.
3. **`/proc/pressure/cpu` has no `full` line** → `full_*` fields do not exist for `scope=cpu`; a rule
   referencing them is refused at load (TROUBLE-SENSORS-018) instead of never firing.
4. **Wake without a crossing** (rate-limited, coarse wakeups measured at ~2.04s) → re-read, count as
   `spurious_wakes`, change nothing (P4).
5. **Two windows in one interval** → the sampler reads once per `sample_interval`; `avg10` remains the
   authority and the trigger only shortens latency. A rule whose metric is `avg*` is never evaluated
   from a trigger's stall value.
6. **`journalctl` missing** → TROUBLE-SENSORS-006; journald off, everything else runs.
7. **Journal rotation/vacuum invalidates the cursor's file** → `--since` fallback; if the fallback
   predates the journal's oldest entry, entries are gone: `est_lost = -1`,
   `cause_detail = "retention_window_exceeded"`.
8. **Non-UTF-8 `MESSAGE`** → U+FFFD replacement, `detail.non_utf8 = true`, entry kept, raw bytes never
   persisted.
9. **Entry larger than the cap** → truncated at 65536 bytes, `detail.truncated`, byte delta recorded.
10. **Queue overflow** → drop-oldest + counted + gap, never a blocked child.
11. **D-Bus down at boot** → TROUBLE-SENSORS-011, retries with backoff; the *first* successful connect
    runs the `ListUnits` reconcile, so failures from before boot are still detected.
12. **Another uid's user manager is unreachable** (no read access to `/run/user/<uid>/bus`) →
    TROUBLE-SENSORS-013 naming the manager; a configured-but-unwatched manager is never skipped
    silently. No uid is hard-coded; the list is configuration.
13. **daemon-reexec during a storm** → TROUBLE-SENSORS-014, re-subscribe, resync, one gap record.
14. **Unit churn between subscriptions** → `300s` sweep realigns the match set from `ListUnits`.
15. **Timer whose activated unit fails** → both the `dbus` path (unit failure) and the `timers` path
    (missed run) fire; they are distinct signatures (`dbus` unit failure vs `timers` missed bucket),
    so the dedup core keeps them as two incidents with one cross-reference each — one cause is never
    counted as one bug twice, and the timer's own failure is not hidden behind the unit's.
16. **Missed runs with no journald access** → `missed_runs` is computed from `ListTimers` +
    `ListUnits` only; journald corroboration is optional and its absence is recorded as
    `detail.corroborated = false`.
17. **Inode exhaustion before space exhaustion** → `inode_free_pct` is a first-class field and has its
    own shipped rule; `Files == 0` reports `-1`, never `0%`.
18. **Rules referencing an unknown field / wrong type** → load-time refusal (TROUBLE-SENSORS-018).
19. **Hot reload with a half-written file** → TROUBLE-SENSORS-019, previous set active, debounce
    absorbs the editor's second write; 3 strikes disable the inotify trigger only.
20. **Rule deleted while its stabilization timer is running** → the timer is dropped; the incident (if
    any) stays open and remains the ladder's responsibility.
21. **Clock jump backwards** → stabilization, cooldown and breaker windows use the monotonic clock
    (SPEC-INDEX §6.5); persisted timestamps stay RFC3339 UTC wall clock.
22. **Host memory pressure** → the `self_memory_pressure` rule files an incident about the daemon's own
    host; the daemon sheds non-PSI cadence and never raises its sampling rate (H).
23. **Kill-switch active** → detection continues; only the ladder's stages are gated (SPEC-05). Sensors
    do not observe the kill-switch, so turning it on never blinds the ledger.
24. **`Stop` with an in-flight child** → SIGTERM to `journalctl`, join with a `5s` bound, then kill;
    the cursor file is written before the child is signalled so no entries are re-delivered.

## 7. Testing

`CGO_ENABLED=0 go test -count=1 ./internal/sensors/...` — every test below is fresh-run (`-count=1`),
no cached results; kernel-dependent cases are gated by `TROUBLE_TEST_PSI_TRIGGERS=1` and `t.Skip` with a
printed reason otherwise.

| File | Cases and numeric thresholds |
|---|---|
| `psi_test.go` | Trigger byte builder golden: `"some 150000 2000000\n\x00"` = 21 bytes ending `\n\x00`; a builder that omits the terminator is asserted to be unreachable (the only constructor is the golden one). Grammar matrix over 8 measured shapes: 2s/4s/20s accepted, 1s/5s/500ms accepted→refused, `0` threshold refused, `stall > window` refused. Arm/epoll guard: adding a fd to epoll before a successful arm panics in the test double; `psi.armed_fds == len(epoll_sets)` after shutdown. Busy-spin regression: the guard blocks the 347,378-hits/200 ms shape (asserted by counting poll iterations in a 200 ms window with an unarmed fd — must be 0 in the guarded path). Wake handling: 1 per window; `spurious_wakes` increments when the re-read fails the match. CPU budget: 3 resources × 2s interval ⇒ ≤0.3% of one core over a 30s run. Probe: modes `triggers+sampling`/`sampling-only`, ceiling discovery `20s→10s→2s`, `oomd_available` recorded. |
| `expr_test.go` | The 14 vectors of §3.4 both directions plus 26 more (short-circuit, `in` with one element, `not` precedence, 4096-byte boundary, depth-16 boundary, depth-17 refusal, >512-byte regex refusal). Every refusal asserts TROUBLE-SENSORS-018 and names the rule+line. |
| `rules_test.go` | TOML decode parity with `types.Rule` for all 12 fields; both `[[rule.match]]` and inline-table forms; duplicate names refused (017); limit 257 rules refused (017); the 9 shipped default rules load and validate; `auto_grants` naming an unknown module refused (017). |
| `reload_test.go` | Swap atomicity: 100k evaluations during 200 reloads — zero evaluations observe a mixed set; in-flight evaluation keeps its snapshot; `match`-identical rule carries its `for=` timer (elapsed preserved ±1ms), changed `match` resets it (`rule_state_reset` recorded); deleted rule drops its timer and leaves its incident open; invalid file → previous set still firing + one TROUBLE-SENSORS-019; 256 rules reload ≤50ms (CI asserts <250ms on the reference box); >1s reload abandoned with the previous set; 3 strikes disable the inotify trigger. The `60s` backstop fingerprint (§3.7a) is covered without depending on timestamp granularity: a content-only rewrite with size and mtime pinned to a frozen instant is detected, a natural edit is detected within a polled `3s` deadline, a quiet directory yields two identical fingerprints (one file and several), and adding/removing a rule file each moves the fingerprint. |
| `journald_test.go` | Fake `journalctl` script: cursor round-trip; malformed cursor → rc=1 zero stdout → exactly one `gap{cursor_invalid}` + `--since` fallback (never "no logs"); child killed 5× → backoff sequence `0.25,0.5,1,2,4s` ±10% jitter; 100k entries into an 8192 queue → `Dropped == 100000-8192` exactly and one gap per episode; dedupe: replaying 4096 identical cursors produces 0 duplicate records; 64KiB+ entry truncated with `dropped_bytes` exact; non-UTF-8 entry kept with `non_utf8=true`; `--since` outside retention → `est_lost=-1` + `retention_window_exceeded`. |
| `dbus_test.go` | Path escaping golden: `coding-hermes-scheduler.service` → `coding_2dhermes_2dscheduler_2eservice` and round-trip for 10k generated unit names; merge rule M1–M4 with synthetic signal sequences: {PropertiesChanged, JobRemoved, journald} in 6 orderings ⇒ exactly 1 incident, 3 attaches, 0 duplicates; journald alone ⇒ 1 journald incident; 3 failures in 300s ⇒ `crash_loop=true`, severity critical, sig re-derived; `NameOwnerChanged` ⇒ re-subscribe + resync + 1 gap; manager unreachable ⇒ 013 naming it. |
| `dbus_batch_test.go` | §3.3a batch reconcile (TRBL-044): 50 failed units in one sweep ⇒ exactly 1 batch boundary call carrying 50 drafts, one `arrival_path = reconcile` record per unit with its per-unit merge key; a batch write failure ⇒ 0 sweep records written, drop count = the whole sweep, one TROUBLE-SENSORS-015 record naming `reconcile batch write` (no partial sweep, no silence); no batch boundary installed ⇒ per-record emit fallback (the §3.8 shape); a firing member reaches the ladder bridge only after the batch returns, carrying the durable record's real rec_id; a second sweep inside the merge window attaches (count=2) in its own single batch. |
| `merge_rule_test.go` | Property test (10k randomized arrival sequences, 5s window boundary at ±1ms) asserting §3.6 M1–M4 invariants: one incident per key per window, `reopen_count` matches the post-resolve arrivals, and no arrival is lost (`arrivals == attaches + opens + reopens`). |
| `disk_timer_inotify_test.go` | statfs on a `tmpfs` fixture: `free_pct` ±0.1%; `Files==0` ⇒ `inode_free_pct == -1` and inode rules false; unmounted path ⇒ one 022 per hour; `ListTimers` garbage reply ⇒ 023 with the raw size; missed-run detection with a synthetic timer 2 intervals stale ⇒ `missed_runs` increments and resets on the next run; `IN_Q_OVERFLOW` ⇒ all watches re-added + 1 gap; watch cap `used/limit=0.81` ⇒ 020; `add_watch` ENOSPC ⇒ watch name in the record. |
| `liveness_test.go` | Each sensor's stale threshold fires 024 exactly once per episode and recovery emits `sensor_recovered`; entry silence (0 events for 1h, probe green) never marks anything stale; dependency case (bus down ⇒ timers degraded with `dependency dbus degraded`) emits exactly one gap; `Health()` ≤50ms with 1024 rules and 6 sensors; canary lands ≤30s through the real pipeline and a blocked pipeline emits `gap{canary_missing}`. |
| `sensors_ac_test.go` | AC-derived: **AC-1** a match at `for=2s` opens exactly one incident inside `2s ±250ms` of the qualifying stream; **AC-2** 10 crossings in 10s with `for=30s` ⇒ 0 incidents, one continuous 30s ⇒ 1, resolve+recurrence ⇒ same incident `reopen_count=1`; **AC-4** 10 000 rule-matching events in 60s ⇒ evaluations ≤120/min, breaker opens within 120s, `Suppressed == events - cap`, ladder gate invoked at most `cap` times; **AC-19** an event is queryable through the index ≤1s (p95) after emission and `Health()` reflects its `last_event_age_s` within one heartbeat. |
| `fold_test.go` | §3.8a on an injected clock, no wall-clock wait: 150 identical sampled observations inside a 5m window ⇒ 1 record with `count=150`, `first_ts`/`last_ts`, `fold=true`, same sig and the first observation's `detail`; the boundary observation closes that window and opens the next (30 + 30 across two windows); a quiet signature's fold is written by the next unrelated arrival; `0` ⇒ one record per observation with no fold fields; a fire and a wake are written immediately with the fold they end written first; `Stop` flushes an open fold; the 4096-fold bound closes (writes) a fold rather than discarding one; the budget is derived and then measured — one simulated day of a repeating signature writes exactly 288 records whose counts sum to the 43 200 observations; a 600-observation over-cap sampled burst still trips the rule breaker, still caps the ladder at 120/min, and its records still account for every observation. |
| `dedup_pin_test.go` | TRBL-009 AC1 pinned as satisfied (the refuted half, so a future change cannot reintroduce it): 12 sampled observations inside one 5m cooldown ⇒ 12 event records, one sig, one fired event, `Suppressed == 11`, no breaker; crossing the cooldown fires once more under the SAME signature (the ladder folds the recurrence into the same incident, AC-22). |
| `perf_test.go` | 256 rules × 6 sensors: rule evaluation ≤2ms/event (measured p99 on the reference box); ≥5000 events/min sustained through normalize→scrub→dedup→ledger with group-commit; sensors' RSS contribution ≤12MB steady (reference: stdlib+godbus+x/sys binary 8.36MB measured; sensors add no sqlite). |

## 8. hilo impact

- **Packages/files created** (greenfield repo `~/trouble`):
  `internal/sensors/{sensors.go, psi.go, journald.go, dbus.go, disk.go, timers.go, inotify.go, rules.go,
  expr.go, normalize.go, breaker.go, liveness.go, fold.go, config.go}` plus the `*_test.go` files
  above;
  shipped artifacts `packaging/polkit/10-trouble-manage-units.rules`,
  `examples/rules/10-defaults.toml`, `examples/config/10-sensors.toml`.
- **Fan-out (imports):** `internal/types` (shared types only), `golang.org/x/sys/unix` (epoll,
  statfs, inotify, eventfd), `github.com/godbus/dbus/v5`. No import of `internal/ledger`,
  `internal/scrub`, or any other `internal/*` package: the ledger and scrubber are injected as
  function values (§2), which is what keeps this package testable without a daemon and keeps the
  dependency graph acyclic against SPEC-INDEX §4.1.
- **Fan-in (imported by):** `internal/ladder` (incident open/reopen per §3.6, breaker gate),
  `internal/dashboard` (read-only accessors), `internal/lifecycle` (heartbeat + `/health` assembly),
  `cmd/trouble` (composition root). Nothing imports back into this package's internals — the §3.10
  types are unexported precisely so this stays true.
- **Blast radius:** zero on the fleet. trouble is greenfield; nothing under `~/trouble`
  consumes these files, and no fleet repo, unit, board or state root is touched. Within the repo the
  blast radius is bounded to `internal/ladder` (consumes §3.6 verbatim), `internal/dashboard`
  (renders `SensorHealth`/`SourceLiveness`/`Breaker`), `internal/sentinel` (reuses the canary and gap
  payload schemas of §3.3/§3.9) and `internal/registry` (§3.4's language is reused for `when:`);
  those three are the only cross-spec contacts, and each is a named reference rather than an import of
  this package.
