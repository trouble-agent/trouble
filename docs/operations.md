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
## 9. The sensors: what to look at, and what "wrong" looks like

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

### 9.1 Daily checks

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

### 9.2 The measured traps

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

### 9.3 Where the state lives

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

## 10. The ladder (SPEC-05)

`internal/ladder` is the only component that decides what work a detection gets. Operationally it
shows up in five places.

**One incident per signature.** Admission is an idempotent upsert: an open incident for the same
`sig` collects arrivals (a fold, never a second incident), two signatures that share an `inKey`
share one incident across planes, and a recurrence after `resolved` reopens the *same* incident id
up to `ladder.reopen_max` times. `trouble incidents` (SPEC-12's CLI) therefore shows one row per
problem, not one per arrival path.

**Verification is evidence, not absence.** A window is only `passed` when a canary landed, every
expected source was alive inside the window, no gap record overlaps it and zero events were
observed. A canary that did not land is `invalid` (`TROUBLE-LADDER-006`) — a flat graph because
nothing is arriving must never read as fixed. This is the single most important operational rule in
the daemon: if you see `invalid` with an empty `sources_missing`, check the canary cadence and the
source liveness table before believing any resolution.

**Budgets escalate instead of running.** Per host, per UTC day: `agent_runs` 20, `play_runs` 50,
`research_requests` 30, `spawns` 5. Exhaustion escalates the incident with
`TROUBLE-LADDER-013` and still fires the outlets, so a human always sees it. A rollover at midnight
never aborts a run: the run is charged to the day it started.

**Storm breakers and suppression windows.** Breakers are per `rule:`, `sig:`, `source:` and
`global`, with half-open probes: after the window elapses exactly one incident is admitted as a
probe; its window passing closes the breaker, its failure re-opens it with the doubled duration
capped at 4h. A suppression window *is* an open `sig:` breaker with `reason=suppression_window`;
arrivals inside it fold and never advance a rung. Quiet-close (`quiet_close` default true) resolves
a suppressed incident only when the whole window was observed quiet **and** a canary landed.

**Restarts.** `Park` writes one park record per in-flight play or agent run before exit (it never
changes the incident's state), and `ReAdopt` resumes them oldest-first with at most one run in
flight. An already-applied mutating call is never re-applied: the re-adopter re-runs the *check*,
which is what makes resume safe. A lost agent run (dead pid, no completion record) becomes
`agent:failed` with `failure_class=daemon_restart_lost` and does **not** consume a strike — a restart
storm must not starve the agent rung.

**The kill-switch** stops every stage entry and records each refusal as pending work; detection,
scrubbing, ledger writes, canaries, liveness and verification keep running, so an outage is never
blind. Clearing it resumes from the persisted state — no command or tool call is ever replayed.

## 11. The registry (SPEC-06)

`internal/registry` is the daemon's entire action surface: every state-changing act trouble performs
is one typed tool call. Three operational facts matter.

**Nothing shells out.** `internal/registry/**` contains zero `os/exec` references, enforced by
`TestNoExecInRegistry` (a `go/parser` import scan, not a text grep), and no registered module may
declare a `command`, `cmd`, `argv`, `shell` or `script` args key
(`TestNoArbitraryCommandModule`). The registry reaches the outside world through exactly four typed
channels: file IO inside an allow root, `/proc` reads, the systemd D-Bus API (mediated by polkit)
and the in-process flow/issue subsystem.

**Scripts-as-data are plays.** A play is TOML in `<state_root>/plays` (or a skill's
`plays/`), resolved skill-local → local → embedded, with `retries` (0–3, transient only,
1s/2s/4s backoff), `register` (forward-only), `on_fail` (`abort`/`continue`/`rollback`) and a
`when:` expression evaluated by the **same** condition language the sensors use — there is no second
dialect. A protected play never loads: a task whose literal args name a do-not-touch path or unit is
refused at load time with `TROUBLE-REGISTRY-007`.

**Do-not-touch cannot be weakened.** The compiled-in floor (`/etc/shadow`, `/etc/sudoers`,
`/etc/ssh/**`, `/etc/polkit-1/**`, `/boot/**`, `/usr/**`, the state root, the daemon's own unit, …)
is always applied; a file that sets `mandatory = false` or carries `remove = [...]` is refused with
`TROUBLE-REGISTRY-006` (`reason=do_not_touch_weaken_refused`) and the daemon continues with maximum
enforcement. Configuration can widen the deny set, never narrow it.

**The polkit artifact is an install-time contract.** `contrib/polkit/49-trouble.rules` grants
`org.freedesktop.systemd1.manage-units` for the `reload` and `restart` verbs, for the daemon user,
on the configured unit allowlist (`registry.service_units`), from a local session. Without it the
probe reports `capability=policy_refused` and `service.reload`/`service.restart` refuse at authorize
with `TROUBLE-REGISTRY-006` (`reason=polkit_missing_policy`) — never a retry, always an escalation
carrying the install command. `trouble registry policy` prints the probe's verdict and
`contrib/polkit`'s install state.

**Conformance gates every module.** `make conformance` runs the §2.4 commands: the whole package
with `-race`, plus the three named tests that prove the harness *rejects* a deliberately
non-idempotent module and a mutating module without a real dry run, and that every generated schema
stays inside the closed draft 2020-12 keyword subset. A descriptor edit without a regenerated
`internal/registry/schema/*.json` (`make schema`) fails the boot check with
`TROUBLE-REGISTRY-014`.
