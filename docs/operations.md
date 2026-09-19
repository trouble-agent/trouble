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

## 12. The dashboard (SPEC-10)

`internal/dashboard` is the read-mostly face of the ledger: one embedded HTTP server in the same
binary as the daemon, serving server-rendered pages and htmx fragments from the SPEC-01 in-memory
index. It writes nothing to the ledger. Reads default to `127.0.0.1:7644`.

**The route table is the whole surface.** Twenty registrable `(method, path)` rows in §2.1 plus one
rule for everything else: a pair that is not a row answers **404** with the `Allow` header naming the
methods the path does accept — never 405, and the body never echoes the requested path. A browser
gets the embedded static `404.html`; an API or htmx client gets JSON. `TROUBLE-DASHBOARD-009`.

**A token in a URL is refused before authentication.** Every route inspects the raw query string for
the §2.2 parameter names (`token`, `access_token`, `auth`, `apikey`, `api_key`, `key`,
`trouble_token`, `tdt` — case-insensitive) and the path for any segment matching
`^tdt_[A-Za-z0-9_-]{43}$`, and answers **400 + `TROUBLE-DASHBOARD-005`** even when the request would
otherwise authenticate. The offending value is never logged (parameter name and length only). The
status is 400 because §2.2 and the §5 catalog say so; §7's test table calls the same case "answers
404 for a query-string token" — read as "the route is not served".

**Tokens live in a 0600 file, outside the state root.** `dashboard.token_file` owes nothing to the
ledger's home, because a ledger backup must never carry credentials. Plaintext is `tdt_` + 43
base64url characters, printed once by the CLI and stored only as `sha256(token)[:32]` hex. The store
is stat'ed per request, so a CLI rotation reaches the running daemon without a restart; a parse or IO
failure is fail-closed — every route except the loopback `/health.json` answers 503 +
`TROUBLE-DASHBOARD-013`, with no last-known-good fallback. A wrong file mode refuses the boot
(`TROUBLE-LIFECYCLE-013`), and a stored hash equal to a configured project key refuses it too
(`TROUBLE-DASHBOARD-002`, `detail=equals_ingestion_key`). `LastUsedTS` is written at most once per
60 s per token: a crash loses ≤60 s of usage, never a credential.

**Auth is Bearer or the `trouble_dash` cookie, and nothing else.** `autonomy ⇒ write ⇒ read`
expands once at token load. Under-scoped requests answer **403 + `TROUBLE-DASHBOARD-003`** with
`X-Trouble-Required-Scope`, so a UI can explain the refusal without a second round trip. A
read-scope session still renders every control — disabled, with `data-requires` and an inline reason,
never hidden — and the server re-checks every POST, so a forged request from a read session is still
refused. Ten auth failures for one presented credential inside 60 s throttle that credential for 60 s
(**429 + `Retry-After`**) — a request that presents no grammar-valid credential is counted and throttled per
client IP instead, so a mistyped or rotated token never locks out a valid one on the same phone or behind
the same NAT (SPEC-10 §2.8a); the read bucket is keyed by token *and* client IP (20/s, burst 60) and the
write bucket by token (5/s, burst 10).

**CSRF on every POST, four checks in order** (§2.3): a Bearer *and* cookie mixture is refused
outright; `Origin` must byte-equal `dashboard.public_origin` (or, on loopback, the request's own
`scheme://Host`) and a browser POST without `Origin`/`Referer` is refused; both cookies are
`SameSite=Lax`; and the `X-Trouble-CSRF` header must equal the `trouble_csrf` cookie *and*
`b64url(hmac_sha256(k_csrf, token_id + "|" + yyyymmddhh))` for the authenticated token. `k_csrf` is
32 random bytes generated at process start, never persisted: a restart invalidates every outstanding
value, the current and previous hour are accepted, and a Bearer-only `curl` POST is legal because the
browser-only cookie half is absent.

**Live updates are 2 s polls with a self-describing guard.** The `#health-strip` polls every 1 s and
is the accelerator: when its `data-seq` changes, `app.js` dispatches `troubleSeq`, which the content
partials listen for (`hx-trigger="every 2s, troubleSeq from:body"`). Every polled element sets
`hx-sync="this:replace"` so a slow response is dropped instead of queued, and polling suspends while
the tab is hidden for ≥10 s (one forced refresh on return). The stale banner is advisory: three
consecutive identical seqs *and* `data-stall-s ≥ dashboard.stall_alert_s`, or any 429/5xx, or no
successful poll for 5× the interval. The authoritative stall alarm remains SPEC-12's external checker
on `/health.json`.

Measured on this host (`go test -count=1 ./internal/dashboard/`): a real trigger reaches the first
fragment carrying the incident in **p50 529 ms / p100 990 ms** with the accelerator running
(AC-19's budget: p50 ≤1100 ms, p100 ≤2000 ms), and **p95 1.89 s / p100 2.01 s** with it stopped
(budget: p95 ≤2000 ms) — the 2 s sampling interval is the p100 limit, which is why the accelerator
exists and why the spec asserts p95 for the stopped case.

**The §2.9 budget, and how it is measured.** The budget row (`budget_test.go`) runs the dashboard in
a child process with 10,000 groups / 50,000 records and drives 100 rps from the parent over real
TCP, so the client's allocations are not counted as the server's. Three windows: the dashboard, a
control handler in the same process at the same rate (to subtract process-level drift), and a real
100-concurrent-render burst. Measured here: **live heap 4.12 MB peak / 1.72 MB steady** above the
boot baseline (ceilings 12 MB / 6 MB), **0.45 MB** under 100 concurrent renders, **1.5 MB retained**
after the load stops (the dashboard holds no per-request or ledger-derived state), in-process render
**p99 6.8 ms** (≤20 ms), and **zero regular-file descriptors** opened during renders — every read
goes through the injected index. Raw RSS is logged beside those numbers: it tracks the Go runtime's
arena growth, which the sampler's own forced collections amplify in proportion to the fixture's live
set, and the control window cannot exercise it because it allocates nothing.

Two consequences shaped the code. Fragments are fitted to the §2.1.2 8 KB cap: an over-cap table
renders as many complete rows as fit and reports the remainder in `data-truncated` plus a marker row,
because §2.1.2's "one row per incident" and its cap cannot both hold when a required incident row is
~129 B (200 rows ≈ 25 KB). And compression is bounded: a fresh `flate` writer is ~814 KB of hash
tables, so the pool holds four writers (≈3.3 MB, warmed before the boot baseline) and a request that
finds the pool busy is served identity rather than queueing — content identical, encoding different,
and 100 concurrent renders cannot hold 81 MB against a 12 MB ceiling.

**What the dashboard does not have.** No routes beyond §2.1: no `/issues` index (the issue desk lives
on the incident story panel), no token-management route (creation is CLI-only), no SSE. `identity`
selects the §2.5 seam; `token` is the only v0.1 implementation and selecting `tailscale` or
`proxy-header` answers 503 + `TROUBLE-DASHBOARD-013` (`detail=impl_absent`) at the login boundary.

**Known gaps, stated.** (1) `/groups/{id}` renders the group projection's counters; the wave-contract
`Index` interface exposes no `EventsBySig` accessor, so the per-sig window rate is not shown.
(2) The budget partial renders the runtime watermarks, version and git SHA; the §2.1.2 "agent/play
burn" counters have no injected source in the `Deps` contract, so that row is not rendered rather
than rendered with a fabricated zero. (3) A malformed write body has no code of its own in the closed
§5 catalog; it is refused 400 + `TROUBLE-DASHBOARD-010` with `detail=invalid_body_<field>`.
(4) `internal/dashboard/integration/e2e_dashboard_test.go` is not written here: it needs the
composition root and a mock ladder, and the composition root's own e2e is the Hermes lane's.


## 9. Sentinel (SPEC-04)

The listener, what it accepts, and where it deliberately differs from upstream
Sentry is `docs/sentinel-compat.md`; this section is the operating half.

### The compat document's provenance

`docs/sentinel-compat.md` is **hand-maintained**: this repository ships no build
layer (there is no `Makefile` anywhere in the tree), so §7's "regenerated from the
same tables by `make compat-matrix`" names a target that cannot run here. What
keeps the document honest is the drift-check test set instead —
`TestCompatMatrixRoutes`, `TestCompatMatrixItemTypes`,
`TestCompatMatrixEncodings`, `TestCompatMatrixAuthForms` and
`TestCompatMatrixDivergences` each render their table's values from the same
tables the server uses and fail CI when the document and the code disagree.
`TestCompatDocProvenance` pins this paragraph, that header and the absence of a
root `Makefile` against each other, so the claim "there is no generator" cannot
outlive a `Makefile` that appears; compat §5 records the divergence. The missing
build layer is repo-wide (SPEC-06's `make conformance` / `make schema`, SPEC-12's
`Makefile` `build` target) and is not this package's to invent: it is flagged for
the v0.1 `SPEC-INDEX` review together with §7's numeric load thresholds below.

### What to watch

| Check | Read | What "wrong" looks like |
|---|---|---|
| the ingress path still works end to end | `ProjectRuntime(id).CanaryLastTS` / `Sources()` | `canary_last_ok=false`, or `sentinel` with `alive=false`: the canary did not land, so **every verification in the window is `invalid`, never passed** |
| SDKs are actually reporting | `ProjectRuntime(id).AuthForms`, `LastEventTS` | an empty `auth_forms` list on a project that should be live, or a stale `last_event_ts` |
| quota is not eating evidence | `ProjectRuntime(id).EventsWindow` vs `QuotaEPM`, `DroppedTotal`, `SampledTotal` | `dropped_total` climbing: SDKs are discarding on 429 while the dashboard looks quiet |
| SDK-side attrition | `ProjectRuntime(id).ClientReportDiscards` | `queue_overflow`/`network_error` counts rising: the app is dropping events before they ever reach us |
| collectors are observing | `Sources()` entries for `journal:<unit>` / `file:<path>`, `SpoolStats()` | a source with `alive=false`, or `torn`/`dropped` spool counters climbing |
| the ledger is keeping up | `reject_total{reason=overloaded}` | 429 `overloaded`: the append wait (`ledger_wait`, 2s) was exceeded, the event was counted dropped and a record was written for it |
| no 400 storm | `gap{cause=ingest_reject_storm}` | an unread 400 storm from a misconfigured SDK: the gap is the only thing that makes it visible |

### Quota, loss policy and the official behaviour

A quota breach destroys evidence, because the official SDK behaviour on 429 is to
**discard**. Three policies, all recorded:

* `drop-with-counter` (429 + `Retry-After` + `X-Sentry-Rate-Limits`) — the SDK
  throws the event away. Every drop writes an `event` record with
  `disposition=dropped_quota`, so the loss is auditable, and verification for a
  sig whose own events were dropped in the window is `invalid`, never `passed`.
* `sample` — deterministic 1/N, 200 with the client's own sample rate unchanged;
  the drop is recorded and `GroupCounters.SampleRate` carries 1/N.
* `spool-if-light` — the only policy that keeps the data; the event is written to
  the spool and replayed when the quota allows. The spool is drop-oldest with a
  `gap{cause=spool_drop_oldest}` note, then `TROUBLE-SENTINEL-015` when not even an
  empty spool can hold an entry.

The proactive path matters more than the 429: at 95% of quota the response is
**200 with the same rate-limit header**, which is what turns a flood into an
orderly slowdown. `X-Sentry-Rate-Limits` is `retry:categories:scope:reason:` — the
exact string the ledger and the dashboard show was emitted.

At most one envelope's worth of items is admitted per window at the quota
boundary: an envelope larger than `quota_epm` is admitted to `quota_epm` items and
the remainder follows the loss policy, so one big envelope can never be silently
swallowed whole.

### Canary

One synthetic envelope per `canary_interval` (default 10m) for `canary_project`,
posted over loopback through the real listener: routing → auth → envelope → scrub
→ sig → group → ledger. It is exempt from quota, loss policies and breakers,
because a flood is exactly when evidence matters. The canary sig is in the
reserved set and never opens an incident. No observation within
`2 × canary_interval` → `gap{cause=canary_missing}`, `ProjectRuntime.CanaryLastOK
= false` and a dead `sentinel` source. A window whose canary did not land is
`invalid`; so is a window in which a sig's own events were dropped by quota even
though the canary landed.

The read that carries the canary into verification is the ledger's per-source
index row, not a sentinel-local flag: once an observation lands, the ledger's
`Sources()` reports the `sentinel` source — the `origin.source` every
sentinel-written record carries — with `canary_seen=true` and a `canary_last_ts`
inside `canary_interval`. That row is the antecedent SPEC-05's
`Evidence.CanarySeen` is derived from (§4.1). `TestCanarySeenInTheLedgerSourceIndex`
holds it against the real on-disk chain, together with the §3.8 record pair (one
`phase=injected`, one `phase=observed`) the flag comes from, so §7's canary row is
pinned on data and not only on the process-side projection
(`CanaryLastOK`/`Sources()` liveness).

### Collectors

Sources are `journal:<unit>` (a supervised `journalctl -f -o json` child with a
persisted cursor) and `file:<path>` (a 250ms poll with `(dev, ino, size)`
rotation detection). Both hand raw lines to the same assembler, which is keyed by
`(source, parser)`, so two sources can never share a partial. Parsers:
`go-panic`, `py-traceback`, `node-reject`, first match wins per line.

Operational facts worth knowing:

* the collector scope sets are **disjoint** from SPEC-03's sensor scopes by boot
  validation — the same unit in both is a config conflict, because two consumers
  following one journald cursor would double-count;
* offsets and cursors live under the state root (`spool/collectors/`, mode 0600)
  and are persisted per batch; a malformed cursor is a `gap{cause=cursor_invalid}`
  with `est_lost=-1`, never "no errors";
* rotation, truncation and removal each emit a `gap` and flush the assembled
  partial with `partial=true`, so the head of a traceback is not lost;
* a line longer than `max_line_bytes` is truncated and flagged; non-UTF-8 bytes
  become U+FFFD and the event is never dropped for encoding;
* collector events are attributed to `canary_project` (or the first configured
  project) — journald and file sources are host-level and carry no project id.

### Memory and the dedup window

The duplicate-event window (§6.6) is bounded at 65,536 ids, oldest evicted: at a
sustained rate above ~109 events/s the oldest ids are forgotten before the
10-minute mark, and an SDK retry of such an id is counted twice
(`duplicate_events_total`). That bound exists so a hostile client cannot drive the
resident set; it is a documented divergence in `docs/sentinel-compat.md` §5.

### Envelope size refusals

The decompressed cap is enforced **at the cap**, not at the reader's memory
bound: a body of 1,048,576 decompressed bytes is admitted and 1,048,577 is
`413` + `TROUBLE-SENTINEL-003` (cause `decompressed_cap`), on `identity` and
`gzip` alike, for any ratio under 100:1. `cap + 64KB` is headroom the limited
reader may hold so "over the cap" is detectable mid-stream (§6.1) and is never
accepted as payload; `TestDecompressedCapBoundary` pins the boundary against the
number in `docs/sentinel-compat.md` §3. §6.1's phrase "a limited reader capped at
1MB + 64KB" is read as that memory bound — the reading §3.1's caps table and
§3.7's own bomb row require — and is worth pinning in the spec text at the next
SPEC-04 revision so it cannot be read as an allowance.

### Client report timestamps

A `client_report` item timestamps itself in either of the two forms the SDK
contract allows: an ISO DateTime string, or a UNIX timestamp in seconds with the
fraction that sentry-javascript sends (`1642153010.09`). The decoder reads both
(`clientReportTS`); a missing, `null` or unrecognized value leaves the report on
the server clock. Before that fix the field was a typed string, so the numeric
form failed to decode and the item took the malformed path — a `200` on the wire
with no `event` record, no merged `ClientReportDiscards` and no
`client_reports_total`, i.e. the SDK's own attrition became silence, which is the
outcome §1.3 exists to prevent. `TestClientReportTimestampForms` drives every
shape through the HTTP path, `TestClientReportTSParsing` pins the fallbacks, and
`docs/sentinel-compat.md` §2 records both accepted forms.

The fraction is decoded from the digits rather than through a float, so
`1642153010.09` is exactly 90,000,000 ns; a `float64` subtraction hands back
89,999,914 ns.

### Load test

`internal/sentinel/load_test.go` runs the §7 load test: 8 workers, ~4KB gzip'd
envelopes, one project, 60s (5s under `-short`), against the **real** ledger with
the 100-record/5ms group-commit policy the spec's reference numbers were measured
with. MEASURED here (16-core host carrying sibling fleet work, load_avg 6-24,
sandbox filesystem, across a 5s run and several 60s runs): 1,768-4,490 req/s
(spec reference 6,199), p99 61-526 ms, p999 89-780 ms, 5xx 0, steady-state RSS
growth 12-29MiB, peak RSS growth up to 157MiB. Throughput and latency both track
host load.

The test asserts the host-supported bounds in the table below (`MB` is the
constants' binary megabyte, `1MB = 1<<20` bytes); the right-hand column is §7's
own pass threshold, which this host does not meet. The latency floor is the
ledger's group-commit cycle, not sentinel: the ledger alone measures **6,182
records/s** with the same policy on this filesystem, i.e. the batch write + fsync
cycle bounds a request's latency at tens of milliseconds here, against the 25 ms
p99 the spec assumes on its reference host.

| Bound the test asserts | This host | §7's pass threshold |
|---|---|---|
| throughput floor | 1000 req/s | 2000 req/s |
| throughput reference (logged, not asserted) | — | 6199 req/s |
| p99 latency | 1s | 25ms |
| p999 latency | 2s | 100ms |
| steady-state RSS growth | 48MB | 8MB |
| peak RSS growth | 192MB | — |

`TestLoadBoundsMatchOperationsDoc` parses that table and fails if a value
disagrees with the constants in `load_test.go`, so prose and code cannot drift
apart again. §7's numeric threshold still has no automated assertion — the
left-hand column is what CI trips on. At the in-flight shape §7's own reference
implies (256 × 1 ÷ 6,199 req/s = 41 ms mean) the 25 ms p99 budget is
arithmetically unreachable here, so the gap stays a v0.1 `SPEC-INDEX` review item
for the spec owner rather than a bound asserted in this package.

`-short` degrades the load test to a correctness smoke (one request in flight per
worker, no throughput/latency/RSS assertions), which keeps `go test -short
./internal/...` safe to run in parallel. The full 60s run saturates the host, so
run the tree with `go test -p 1 ./internal/...` when it is included — otherwise it
can push `internal/ledger`'s timing assertions (`TestFsyncWindowBound`,
`TestPerLineRegression`) and `internal/scrub`'s µs/KiB budgets over their
host-measured bounds on a shared machine.

### Steady resident set (§7's Memory paragraph)

§7's Memory sentence — "`TestMain` asserts steady RSS ≤ 80MB after 1,000,000
events (measured trivial path 7.0 → 15.6MB) and ≤ 192MB under the load test" — is
shipped at the spec's scale. `TestSteadyRSSAfterManyEvents` (`load_test.go`) drives
`steadyRSSEvents` (1,000,000) events through the admission path with a discard
sink, so the measurement isolates sentinel from the ledger's own footprint, and
asserts steady RSS after `runtime.GC()` + `debug.FreeOSMemory()`; the load test
asserts the sentence's other half (peak growth ≤ 192MB) beside its own row above.
MEASURED here, 16-core host under sibling fleet load, across two runs:
**1,000,000 events in 32.4-33.2s, steady RSS 30.4-31.4MB** (15.5-15.8MB before the
run, growth 15.0-15.7MB) against the 80MB bound — the package's full run is
115-139s on this host depending on how the tree is run, and this measurement is
roughly a third of it, which is why it is a named test and not a `TestMain`.

There is no `TestMain` in this package: §7's sentence names one, but a `TestMain`
would pay this measurement on every invocation, including the `-race` build (where
a resident-set number is skewed by instrumentation and says nothing) and `-short`
(which exists so a busy host can run the whole tree in parallel). The named test
is what the sentence refers to; `TestMemoryBoundsMatchOperationsDoc` ties the table
below to the constants, so the count and the bounds cannot drift from this prose.
Unlike the load test it is single-goroutine CPU work with no ledger fsync in the
path (~1 core for ~32s), so it runs under `-short` too and cannot push a sibling
package's host-measured budgets the way the 8-worker load test can.

| Measurement | Shipped and asserted | §7's Memory paragraph |
|---|---|---|
| steady-RSS event count | 1,000,000 events | 1,000,000 events |
| steady RSS bound | 80MB | 80MB |
| peak RSS bound under the load test | 192MB | 192MB |

### Substrate fix found by this work: `types.NewID` same-millisecond collisions

Building the sentinel surfaced a real defect in `internal/types/id.go` (SPEC-01's
identity helper): `encodeULID` copied only 8 of the 10 randomness bytes and walked
the 130-bit encoding space backwards, so the leading characters carried the
randomness and the trailing ones the timestamp — and `incRand`, which exists to
keep ids distinct inside one millisecond, incremented the *dropped* bytes. Two
`NewID` calls in the same millisecond returned the **same** id.

The user-visible effect: sentinel's `event_id` generation produced identical ids
inside one millisecond, so SDK-retry deduplication (§6.6) silently swallowed
genuine events under load — a whole load run collapsed into a single event.

Fixed in `internal/types/id.go` (48-bit timestamp in the leading characters,
80-bit randomness in the trailing ones, so `incRand`'s increment is visible) and
pinned by `internal/types/id_test.go` (`TestNewIDIsUniqueAndIncreasing` — 512 ids
minted in one call, all distinct, strictly increasing;
`TestEncodeULIDLayout`). Any package that mints ids (ledger `rec_id`, sentinel
`event_id`, group `grp_`) inherits the fix.

## 12. The research rung (SPEC-07)

The research rung asks Off-by-One for a pre-solved answer before an agent is
spent. It is **best effort by contract**: the lab being unreachable, slow,
rejecting, unavailable or silent never blocks the rung above it. Every failure
path returns an outcome the ladder can proceed past, plus a ledger record; five
conditions additionally write a `gap` record with `est_lost = -1` (research
loses an enrichment, never evidence).

What to look at:

| Signal | Where | What it means |
|---|---|---|
| `state` | `research` record | `requested` (queued, a poller owns it) · `returned` · `degraded` · `skipped` |
| `error_code` | `research` record | `TROUBLE-RESEARCH-001..010`; `""` on the clean path and on every budget decision |
| `degraded_reason` | `research` record | `lab_unreachable`, `solver_unavailable`, `strict_decoder_reject`, `poll_timeout` |
| `skip_reason` | `research` record | `driver_none`, `driver_capability_missing`, `class_unknown`, `brief_invalid`, `budget_exhausted`, `kill_switch` |
| `corpus_grep_hit` / `corpus_path` | `research` record | the answer came from the corpus, not from discover — the `found:false` trap |
| `cost` | `research` record | requests by kind, bytes, polls, submits, corpus greps, tokens saved |
| `fence` | `Snapshot()` / dashboard | `submit_disabled`: three consecutive strict-decoder rejects opened the fuse |
| `cooldown_until` | `Snapshot()` / dashboard | three consecutive transport failures closed the lab conversation |
| `capability` | `Snapshot()` | `full` or `discover_only` (a route is missing from `/openapi.json`) |

Three runbook facts that surprise people:

1. **`found:false` is not absence.** The lab's lookup key is class + env + lang +
   version, so a verified answer whose signatures carry empty
   environment/language/version is invisible to a narrowed probe. The corpus grep
   (D3) is mandatory for exactly this reason, and `corpus_grep_hit:true` records
   which probe found it.
2. **A submitted class is never cancelled.** There is no cancel endpoint; a poll
   timeout leaves the submission queued and the next incident for that class finds
   the answer on the cheap D1 path. `poll_timeout` is a budget decision, not a
   leak.
3. **The daily counter survives a restart.** It is rebuilt at boot by replaying
   today's `research` records (`Service.Replay`), so a crash cannot reset the
   request budget. The driver's `requests_per_day` (200) is deliberately above the
   ladder's `research_per_day` (30), so the policy gate is always the binding one.

Operating it:

```
trouble research status            # driver, capability, cooldown, fence, queue depth
trouble research outcomes <sig>    # the rung history for a signature, newest first
trouble research prompt <res_id>   # the prompt the agent would have seen (and its digest)
```

The class table (`research.table`) is configuration. A malformed table is a
**config-load** refusal, never a rung failure: if a hot reload is rejected, the
active table stays in place and SPEC-12 reports it. A fallback slug
(`Fallback=true`, `TROUBLE-RESEARCH-005`) still runs the corpus grep but does not
submit unless `allow_unknown_class_submit=true` — submitting under `unknown`
pollutes a cache shared with every other trouble host.

## 13. The flow subsystem (SPEC-08)

`internal/flow` turns a finding into work: one board row per `(sig, board)`
through the `board-jsonl` or `task-router` driver, and — for a confirmed code bug
on an enabled host — a foreman inside an isolated git worktree.

Invariants worth memorising, because each one is a fleet failure class:

1. **trouble is a create-only writer of a board.** It appends one row + one
   `task_created` event per `(sig, board)` and never mutates a row afterwards. A
   recurrence is a `task_comment` event on the existing row, never a second row.
   The `boardctl`-style full-file `update` path is deliberately unused: a second
   writer with a full-file rewrite over a git-tracked JSONL board is the
   documented dirty-board class.
2. **Never file into an unregistered project.** The registration proof is a probe
   against the scheduler (`GET {flow.scheduler_endpoint}/projects`), and filing
   refuses with `TROUBLE-FLOW-018` and **zero bytes written** when the project is
   absent, disabled, points at another board path, or the proof went stale past
   `registration_stale_max`. A row the scheduler does not tick is a row nobody
   works.
3. **One fix per signature, host-wide.** The sig lease (`lease_ttl`, default 30m)
   is the only authority for a second attempt; a second incident gets
   `TROUBLE-FLOW-009` plus a cross-ref comment on the existing row. Two foremen on
   one sig are two patches for one bug and two chances to corrupt a repo.
4. **The repo's owner creates the worktree.** trouble sends the repo, the base and
   the task id; `router_spawn` performs `git worktree add` and the untracked-input
   copy. trouble never runs git against a repo the scheduler owns, and a foreman
   worktree carries a hard ban on `git fetch`/`gc`/`prune`/`worktree` mutation.
5. **A spawn failure is never silent.** `requested` is written before the router
   call; no acknowledgement becomes `spawn_pending` plus a durable-queue entry;
   the reconciled pending count is the first number of the dashboard's budget
   panel. Retries are bounded (5s/15s/45s/2m/5m); a permanent refusal is terminal
   with its reason recorded.
6. **The 60s trigger→spawn budget is measured per spawn** (`trig_to_spawn_ms`,
   `budget_exceeded`), not asserted once. Over budget, the run still completes and
   the miss is recorded with `stage="budget"`.

What to look at:

| Signal | Where | What it means |
|---|---|---|
| `decision` | `flow` record | `created` · `commented` · `dispatched` · `skipped` · `drafted` · `failed` · `promoted` · `discarded` · `pending_human` |
| `stage` | `flow` record | `file`, `comment`, `dispatch`, `gate`, `budget`, `promote`, `rollback`, `reap` |
| `validate_rc` / `validate_findings` | `flow` record | the board's own validator: 0 clean, 1 with our id = `TROUBLE-FLOW-003` (quarantine the id), 1 without our id = a pre-existing board finding |
| `state` | `spawn` record | `requested` → `accepted`/`spawn_pending` → `leased` → `promoted`/`discarded` |
| `worktree_mode` | `spawn` record | `owner_created`, or `owner_serialized` for a `worktree_exempt` repo |
| `brief_sha256` | `spawn` record | proves what the foreman was told |
| `QueueDepth()` | dashboard budget panel | spawn requests still pending on the durable queue |

Operating it:

```
trouble flow reconcile             # worktrees-meta vs the router registry vs the boards
trouble flow hotfix status --json  # lanes, leases, queue depth, budget hits
trouble flow explain <sig>         # which rows, leases and dispatches this sig produced
trouble flow promote <sp_id>       # the human decision: merges through the repo's own PR
```

`flow.hotfix.enabled` defaults to **false** and the default is printed by
`trouble config explain`. Turning it on is a per-host decision, and `flow.hotfix.
allowed_repos` is the list it may touch. Promotion in `human` mode is the default
in every autonomy mode including `full`; `auto-after-verify` additionally requires
`AutonomyGates.AllowPromote`, so an assisted host cannot auto-merge by
configuration alone.

## 12. The issue desk (SPEC-09)

**The desk ships OFF.** `enabled = false` is the compiled default (SPEC-09 §3.4a): the desk is built and
idle — no driver is constructed, no credential is read, no outbound call is made — because no deployment
value is compiled in. The `[issues]` table is a live config key: `examples/config.toml` ships it with
`enabled = false` and the composition root hands it to the desk's own loader (SPEC-12 §3.1b), so editing
that one line is the whole opt-in. Turning it on names a driver: `[issues] enabled = true` with
`[issues.drivers.github] owner`/`repo` and a token, or the local-first duckbrain block. Enabling it
without that pair is refused at boot with `TROUBLE-ISSUES-003: driver github needs owner and repo`, and the
`/health.json` issues row says `refused` — the declaration is never silently replaced by the default.

**One issue per sig, forever.** Identity is `(driver, sig, project)`, held in the in-memory anchor
index and rebuilt at boot from the `issue` records inside the retention window. The dedup window
(default `30m`, per driver, clamped `1m..24h`) only chooses the *shape* of a recurrence: a one-line
fold inside it, a full block (counters and release range since the previous comment) outside it.
Neither path ever creates a second issue; only two things do — the anchor being gone at the driver
(`TROUBLE-ISSUES-007`, a human deleted it) or a reopen the driver refuses (`TROUBLE-ISSUES-008`, in
which case the desk files a superseding issue carrying `supersedes: <old id>` and the same sig
marker).

**Caps are the anti-spray rule.** `[issues.caps]` bounds creates and comments per sig, per project
and globally, and `comment_min_interval` spaces the folds. Counters are rebuilt from the ledger at
boot, so a restart cannot lift a cap. A capped operation is recorded (`op=cap`) and **never
spooled**: a cap is a policy decision a retry cannot change. The incident itself is unaffected — a
capped critical incident escalates through the escalation outlet.

**A driver outage degrades the rung; it never fails the incident.** Two consecutive failed probes
(`fail_after_probes`) mark a driver failed, write one `healthcheck` record and one `gap`
(`cause=driver_down`), and the ladder continues at rung `outlets` with its state unchanged. The
pending operations spool; on recovery the spool drains before new work and each drained operation
sets the anchor through a `result=replayed` record. Everything dropped (TTL, attempt count, budget)
writes **both** a `gap` (`cause=queue_overflow`) and an `issue{op:drop}` record, so the loss is
visible in the ledger rather than inferred.

**Day one checks.** `trouble issues health --json` (cap counters per driver, spool depth, driver
health). A `TROUBLE-ISSUES-003` means the token file's mode or owner is wrong — the desk made no
outbound call, so nothing was filed and nothing was lost. A `TROUBLE-ISSUES-005` on a full spool is
the one case where work is refused rather than queued: entries younger than
`spool_min_retention` are never evicted, by design.

**The token never appears anywhere but the wire.** It is read from a 0600 file (or the configured
env var), refused on argv, and it is never logged, never in a ledger payload, never in
`DriverHealth.Detail`, and never printed by `trouble config explain` (which shows the file path with
`Redacted=true` and, for the KV backend, the header **name** only).

## 13. The skill loop (SPEC-11)

**The loop ships OFF.** `enabled = false` is the compiled default (SPEC-11 §2a): the daemon pulls nothing
and no skill is ever applied, because no distribution channel is compiled in. The `[skills]` table is a live
config key like the desk's (SPEC-12 §3.1b): `examples/config.toml` ships it OFF and editing it is the whole
opt-in. Turning it on names exactly one source — `[skills] enabled = true` with `source_path` (a local git
dir) or `source_url` (an https channel). Enabling it with neither, or with both, is refused at boot with
`TROUBLE-SKILLS-001: exactly one of source_path or source_url must be set`, and the `/health.json` skills
row says `refused`.

**The artifact cannot express code.** `SKILL.toml` has a frozen key set; an unknown key, an unknown
table, a `[stats]` table, a glob in `allowed_modules` or a free-text sig is a refusal
(`TROUBLE-SKILLS-001`) with a reason naming the field. A skill can do exactly one thing: cause typed
registry tool calls that its `allowed_modules` allowlist names and this build already ships.

**What is signed is not the file.** TOML has no canonical form, so the signature covers
`trouble.skill.v1` + a deterministic JSON projection + `play_sha256=<digest of the play bytes>`. A
reformat does not invalidate an artifact; a one-byte play edit does, even though `SKILL.toml` is
untouched. The generation prefix inside the signed bytes is the artifact's schema version.

**Local stats never travel.** `applied/success/last_used` live in
`<state root>/skills-local/stats.json` (flushed within 5 s, crash-loss window stated), never in the
artifact — that is the fix for the PRD's own contradiction, and the reason the distribution path
stays unidirectional. The same file carries the per-day `max_runs` counter.

**Pull only, and only reads.** The channel is read with `git ls-remote`/`fetch`/`rev-parse`/
`checkout --detach` as argv arrays, never through a shell, and never pushed to. Tag mode selects the
highest semver tag (a `source_ref` with no wildcard is an exact tag name); branch mode is legal but
recorded as `ref_mode=branch` so an operator can see a mutable head was trusted. A resolved sha equal
to the last one writes **no** ledger record.

**Refusals are recorded, never silent.** Every gate failure writes a `refused` record deduplicated to
one per 24 h per `(name, version, code, reason)` with the accumulated count — a permanently refused
artifact on every pull interval cannot flood the ledger, and nothing is dropped. A below-floor
satellite records `floor_blocked` (refusal `013` whose cause is `004`), which is how the hub learns
the host is not running the version.

**Boot re-verification is the tamper detector.** Every boot re-reads each installed artifact and its
play and compares the canonical digests; a mismatch moves the tree to `quarantine/`, marks the row
`refused`, records `TROUBLE-SKILLS-002` and never executes it. The process stays green on `/health`
while `trouble skills status` reports it.

**Canary first, then the per-host approve policy.** With `canary_host_id` set, a non-canary host
holds a version (`TROUBLE-SKILLS-012`) until a green canary from that host exists inside
`canary_validity`; a **failed** canary is terminal for the version. `approve=review` (the default)
parks a pulled version in `pending/` until `trouble skills approve <name>@<v>`; `approve=never`
refuses everything pulled; `approve=auto` installs, and installation is still not authority — in
`shadow` an `auto` host runs mutating skill plays as `check_mode` downstream.

## 13. Lifecycle, config and the watchdog chain (SPEC-12)

`internal/lifecycle` owns whether the daemon is allowed to run and replaceable
without losing work.

**Config precedence.** flag > env > file > default. `trouble config explain` prints
one row per key with its source and source_ref; secret-class keys are replaced by
`[REDACTED:config]`. There is no reveal flag. Unknown keys in the config file are
fatal (TROUBLE-LIFECYCLE-001); stray `TROUBLE_*` environment variables are ignored
with a near-miss hint when the name is close to a real key.

**State root.** The daemon refuses to start if `state_root` resolves under `/tmp`
or `/var/tmp`, is on a remote filesystem, or is not `0700`. Secret-bearing files
must be `0600`; `CheckSecretFiles` reports every offender in one pass.

**Bind preflight.** `PreflightBinds` resolves both listeners, validates the bind
matrix, and keeps the sockets open so a live listener is detected as `EADDRINUSE`.
A public `ingest.bind` without proxy mode is refused before any HTTP response is
served.

**Watchdog chain.** Four independent links: systemd `WatchdogSec=60`, the atomic
`heartbeat.json`, the ledger sequence (made unconditional by the idle-tick rule),
and the external `trouble-stall.service`. The checker alarms on ledger-sequence
stall (TROUBLE-LIFECYCLE-009), not process liveness, because a wedged ledger
writer can keep heartbeating.

**Upgrades.** `trouble upgrade` stages `<bin>.new`, self-checks, parks in-flight
plays, hardlinks the previous binary into `backups/bin/`, then `rename()`s over
the live path. A park failure aborts with zero renames. The ETXTBSY trap is
avoided by never opening the live binary for writing.

**Satellite forwarding.** Records are queued in `spool/forward/NNNNNNNNNN.fwd`
segments with a CRC footer, batched (≤200 records, ≤512 KiB decompressed), gzip'd,
and forwarded as a Sentry-shaped envelope. Acks trim sealed segments; budget
overflow drops the oldest whole segment and writes an exact `gap` record, leaving
the 2 MiB gap reserve untouched.


## 14. Deployment (binaries and containers)

Two ways to run the same two binaries: from the release matrix on a host, or from
the container image with the compose stack. Both stamp the build the same way, so
`/health.json` reads the same `version` / `git_sha` / `build_time` either way.

**The binary matrix (SPEC-12 §3.4; `make bin`, `make release`).** `make bin` writes
the two stamped binaries to `bin/`; `make release` writes the same pair for each
target below plus `manifest.json` (checksums), and exits non-zero unless every
target built — a partial manifest reads like a release, so it is never written.

| goos/goarch | `troubled` (daemon) | `trouble` (CLI) | archive |
|---|---|---|---|
| linux/amd64 | `dist/linux-amd64/troubled` | `dist/linux-amd64/trouble` | (placeholder — your packaging step) |
| linux/arm64 | `dist/linux-arm64/troubled` | `dist/linux-arm64/trouble` | (placeholder) |
| darwin/arm64 | `dist/darwin-arm64/troubled` | `dist/darwin-arm64/trouble` | (placeholder) |
| windows/amd64 | `dist/windows-amd64/troubled.exe` | `dist/windows-amd64/trouble.exe` | (placeholder) |

Every one is built `CGO_ENABLED=0` with `-trimpath` and `-ldflags "-s -w"` plus the
three `lifecycle` stamps. A build that cannot determine a sha stamps the literal
placeholder `nogit00` — never `unknown`, which is the sentinel `/health.json`
reports as `status="degraded" detail.reason="unstamped_build"` and `trouble install`
refuses without `--force`.

**The image.** `Dockerfile` is multi-stage: `golang:1.26-bookworm` builds both
binaries (module download layer cached ahead of the source `COPY`),
`gcr.io/distroless/static-debian12:nonroot` is the runtime — no shell, no package
manager, no curl. `distroless` rather than `scratch` because it still carries an
`/etc/passwd` with uid 65532, a CA bundle and tzdata, so the daemon runs
unprivileged and can still do TLS. Only the two binaries are copied in.

```
docker build --build-arg GIT_SHA=$(git rev-parse --short=7 HEAD) -t trouble:local .
docker run -d --name trouble \
  -p 127.0.0.1:7643:7643 -p 127.0.0.1:7644:7644 \
  -v trouble-state:/data \
  -v "$PWD/deploy/container/config.toml:/etc/trouble/config.toml:ro" \
  trouble:local --config /etc/trouble/config.toml
```

`ARG VERSION/GIT_SHA/BUILD_TIME` are all overridable. With no `GIT_SHA` the build
derives it with `git rev-parse --short=7 HEAD` from the build context (`.git` is
deliberately not in `.dockerignore`, for exactly this reason); with no usable git
it stamps `nogit00` and says so loudly. A build that reports `git_sha` other than
`unknown` is stamped; anything else is degraded by design.

**The compose stack.** `docker-compose.yml` runs `trouble` and `redis:7-alpine`.
Redis carries the SPEC-13 preflight posture — `--appendonly yes`,
`--maxmemory-policy noeviction`, cluster mode never enabled — because the hub's
dedup window is a correctness surface, not a cache: an evicted key is a lost event.

```
docker compose up -d                 # build + start trouble and redis
docker compose logs -f trouble       # "dashboard listening" = the boot gate passed
```

The healthcheck authenticates against `/health.json` (off loopback there is no
exemption), so a first boot reports `health: starting` and then `unhealthy`
until the token seed below exists — that is the expected order, not a daemon
fault. Seed next; `--output-env` makes the mint write the 0600 env file itself,
inside the volume, as the container's own uid. This is load-bearing on the
distroless image: there is no shell to author the file with, and every path
that writes it from outside fails a shipped mode/ownership rule (trap 4 below,
measured):

```
docker compose run --rm \
  -e TROUBLE_DASHBOARD_TOKEN_FILE=/data/state/dashboard.token \
  -e TROUBLE_STATE_ROOT=/data/state \
  --entrypoint /usr/local/bin/trouble trouble \
  dashboard token create --label compose --scopes read \
  --output-env /data/state/trouble.env
```

That prints the plaintext once and writes it into the env file (replacing any
previous `TROUBLE_DASHBOARD_TOKEN` line; other lines are preserved). The
container `HEALTHCHECK` — `trouble check-stall`, the SPEC-05 external checker —
reads the token from `[secrets] environment_file` on its next run:

```
docker compose ps                    # trouble: healthy once the checker exits 0
curl -s -H "Authorization: Bearer <printed plaintext>" http://127.0.0.1:7644/health.json
```

Day-2 commands:

```
docker compose logs -f trouble
docker compose stop redis            # SPEC-13 degradation check (see below)
docker compose down                  # stop, keep volumes; `-v` also drops state
```

**Ports.** `7643` ingest, `7644` dashboard; both published on `127.0.0.1` only
(`127.0.0.1:7643:7643`, `127.0.0.1:7644:7644`). This stack never binds `3000`,
`8642`/`8643` (Hermes gateway) or `9090` (coding-hermes scheduler).

**Paths and modes.** State volume `trouble-state` at `/data`; `state_root` is
`/data/state` and the token and env files are `/data/state/dashboard.token` and
`/data/state/trouble.env`. Three mode/ownership traps, all measured:

1. `/data` itself cannot be the state root. A fresh named volume is created
   `root:root 0755` by the volume driver — only the image directory's *contents*
   are copied in — so it is neither `0700` nor writable by uid 65532, and the
   daemon refuses: `TROUBLE-LIFECYCLE-005: state_root "/data" mode is 0755, want
   0700`. The image therefore ships `/data/state` as `0700` owned by `65532`.
2. A host bind-mounted token file is owned by the invoking user (uid 1000, `0600`)
   and is unreadable for the daemon → `TROUBLE-LIFECYCLE-013 (token_file_mode):
   token store unstatable`.
3. A compose `secrets:` entry mounts `0444` root-owned and trips the store's own
   check → `TROUBLE-LIFECYCLE-013: token file mode not 0600`.
4. The env file cannot be authored from outside the container at all. `docker
   cp` lands the file owned by the HOST uid (1000 on a typical workstation), so
   the container uid 65532 cannot read it and the healthcheck stays exit 8
   (`TROUBLE-LIFECYCLE-008`) against a serving daemon — measured, live. A host
   bind mount of a `0600` file keeps the invoking uid and cannot even be read
   for seeding; a compose `secrets:` entry is trap 3. The distroless image has
   no shell, so the file cannot be authored inside either. The mint therefore
   writes the env file itself: `dashboard token create --output-env
   /data/state/trouble.env` (the one step the compose quickstart adds).

Secrets therefore live inside the state volume, which is the one place the daemon
can both read and write at the mode it enforces.

**Every off-loopback key the container needs** (each added because the daemon
refused the boot, not by guesswork — all four are in
`deploy/container/config.toml` next to the refusal they answer):

| key | refusal it answers |
|---|---|
| `ingest.bind`/`dashboard.bind` `0.0.0.0` | the published port must reach the listener |
| `[ingest.auth] nonloopback_mode = "proxy"` | `TROUBLE-LIFECYCLE-003: public ingest.bind "0.0.0.0:7643" requires proxy mode` |
| `dashboard.mandate = "proxy"` | `TROUBLE-DASHBOARD-006 (mandate_required): non-loopback bind requires a mandate` |
| `dashboard.public_origin` | `TROUBLE-LIFECYCLE-001 (public_origin_required): public_origin required off loopback` |

`proxy` is the honest declaration for a container whose only ingress is the
host's published port; `tailnet` is the other accepted mandate.

**HEALTHCHECK.** Distroless has neither shell nor curl, so the probe is the CLI
itself — `/usr/local/bin/trouble check-stall --health-url
http://127.0.0.1:7644/health.json --json` — which is the SPEC-05 external stall
checker: it reads the ledger sequence out of `/health.json` and alarms on stall,
not on process liveness. It authenticates from `TROUBLE_DASHBOARD_TOKEN` in
`secrets.environment_file`, so that file must exist and be `0600` or the
container reports unhealthy while the daemon is fine. Pass the container config
explicitly (`--config /etc/trouble/config.toml`): without it the CLI resolves
builtin defaults, looks for the health/heartbeat surfaces at the default paths
and answers exit 8 `TROUBLE-LIFECYCLE-008 "health and heartbeat unreadable"`
against a daemon that is serving perfectly. Without a token file the daemon still
serves and logs `dashboard token store is empty ... reason=missing`, and every
request fails closed with `401`.

**Redis degradation (SPEC-13).** With the standalone profile the daemon does not
consult Redis at all, so `docker compose stop redis` changes nothing: `trouble`
stays `Up`, `/health.json` keeps answering `200`, and `ledger_last_seq` keeps
advancing (measured: 89 → 104 across the stop). When the light-hub profile lands,
its documented behaviour — degrade to standalone ingestion, no event loss, no
restart — must be re-proved against this same before/after sequence.

`deploy/container/config.light-hub.toml` carries the wire-up shape for that
profile (`server.profile = "light-hub"`, `server.redis.url =
redis://redis:6379/0`, `server.duckbrain.namespace`, and the
`require_persistence`/`check_policy` preflight knobs of SPEC-13 §2.1.1). The
`[server]` surface is registered as ordinary config keys, so that file RESOLVES
and the light-hub profile passes the boot gate — measured: `trouble config
explain --config deploy/container/config.light-hub.toml --json` lists all 29
`server.*` rows with their source, and a scratch boot of the same file reaches
`/health.json` with `server.profile=light-hub` recorded in the boot `config`
record. It is still **not** the compose default, because the profile's runtime
(the Redis stream + consumer group, the dedup gate and the DuckBrain archival
tier, SPEC-13 §2.3) is `internal/hub`'s and is not built in this tree: a
light-hub boot serves on the standalone in-process path (the daemon logs a
warning saying so) and `/health.json` carries no `hub` stanza. Switch the
compose volume source to this file when that package lands; do not delete the
keys to make it boot.

**Building from a git worktree.** A worktree checkout's `.git` is a one-line
gitfile pointing at the main clone's `.git/worktrees/<name>` — not a directory
— so the build context carries a 14-byte pointer while the object store it
points at never rides along: the Dockerfile's `git rev-parse --short=7 HEAD`
fails inside the builder and the image stamps the literal `nogit00` (measured:
`trouble --version` in the image printed `nogit00 …` for a worktree build with
no `GIT_SHA`, while the same context built with
`GIT_SHA=$(git rev-parse --short=7 HEAD)` printed the worktree's own sha).
That is degraded by design, not a failure, and the build prints a loud WARN
when it falls back — but pass the sha explicitly and the stamp is identical to
a main-clone build:

```
GIT_SHA=$(git rev-parse --short=7 HEAD) docker compose build
```

`nogit00` is the honest placeholder — never confuse it with `unknown`, which is
the sentinel `/health.json` reports as `status="degraded"
detail.reason="unstamped_build"` and `trouble install` refuses without
`--force`. An image stamped `nogit00` serves stamped values; treat it as
"built from a context without git history", not as an unstamped build.

**Seed cheat-sheet.** The quickstart above in one block — build, seed, watch it
go healthy. `--output-env` is what makes this a copy-edit path on a distroless
image (no shell to author the env file with; see trap 4):

```
GIT_SHA=$(git rev-parse --short=7 HEAD) docker compose build
docker compose up -d                        # trouble starts "unhealthy": the token seed below is still missing
docker compose run --rm \
  -e TROUBLE_DASHBOARD_TOKEN_FILE=/data/state/dashboard.token \
  -e TROUBLE_STATE_ROOT=/data/state \
  --entrypoint /usr/local/bin/trouble trouble \
  dashboard token create --label compose --scopes read \
  --output-env /data/state/trouble.env      # prints the plaintext once; writes the 0600 env file itself
docker compose ps                           # trouble turns healthy on the checker's next run (interval 30s)
docker compose down                         # tear down; `-v` also drops the state volume
```
## 15. The GitReins guard: its scan surface and its test window (TRBL-014)

The Tier 1 guard is only a signal if a clean tree is green. Two independent defects made every judge
run report `tier1 FAIL` regardless of the diff, which meant the guard detected nothing:

* **the secrets step scanned things that are not the commit** — the harness runs
  `gitleaks detect --no-git`, which walks the whole working tree and does **not** honour
  `.gitignore`, so the gitignored `.worktrees/` artifact tree and a dozen tracked corpus files were
  graded as if they were credentials;
* **the tests step never ran the configured command** — `.gitreins/config.yaml` wrapped its settings
  in a top-level `gitreins:` mapping. GitReins reads them with a bare `config.get("guards", {})`
  (`engine/guard_manager.py::_load_guard_config`, `engine/pipeline.py::load_pipeline_config`), so the
  whole block was parsed and then ignored: the judge's generated Tier 1 plan ran a lint step the
  config had switched off and the language-default `go test ./...` (the Go default in
  `engine/lang_detect.py`) with a 120s window. Every guard setting in this repo was inert until the
  shape was flattened to what `gitreins init` writes.

### What the guard grades now

| Lane | Command | Owner | Host requirement |
|---|---|---|---|
| Tier 1 `secrets` | `gitleaks detect --no-git` + the built-in cross-check, scoped by `.gitleaks.toml` | every judge run | none |
| Tier 1 `tests` | `guards.test_command` (see below), window `guards.test_timeout: 900` | every judge run | must finish on a loaded box |
| Go compile / vet | `guards.go.build` / `guards.go.lint` (`go build ./...`, `go vet ./...`) | `gitreins guard` | none |
| **Full suite** | **`go test ./... -count=1`** | **QA / E2E** | **quiet host** |

`guards.go.tests` is **off**: its command is hardcoded inside GitReins
(`go test -count=1 -short ./...`, `engine/guards.py::check_go_tests`) with no scope knob, and that
command is the red run in the table below. Go tests are not untested by this — they are gated on
every evaluation by `guards.test_command`, and the host-measured harnesses that command excludes are
owned by the QA/E2E lane. Nothing here is silently untested: the exclusions are enumerated in the
config, by test name, each with the reason.

### The tests window, measured

Four runs against the same tree, 2026-09-19, `load_avg` 22–49 on 16 cores with sibling workers live:

| Command | Wall | Result |
|---|---|---|
| `go test ./... -count=1` (the full suite) | 10m04s | EXIT=1 — 5 packages red |
| `go test ./... -count=1 -short` | 10m01s | EXIT=1 — 4 packages red, 1 go-test panic |
| `test_command`, before `TestAckImpliesDurable` joined the skip list | 9m55s | EXIT=1 — `internal/ledger` red: the child hit its own 3m timeout (panic), the parent got 322 acks of 500 |
| `guards.test_command` (the guard's lane, final skip list) | 5m46s | EXIT=0 |

A bigger window cannot fix the first two: their reds are host-measured budgets, not logic — the
ledger group-commit and per-line throughput floors, the scrub ingest floor (spec 5000 req/s on a
quiet host), the sentinel live-load and E2E budgets, the dashboard p99/RSS budgets. `-short` alone is
not enough either, because several of those harnesses are not gated on `testing.Short()`. So the
guard's job is scoped to the deterministic lane and the rest is named:

* **daemon boot/drain harnesses** (`internal/app`) — they wait on a budget that scales with host
  load (`readybudget_test.go`: 30s READY + 20s drain × `bootBudgetScale`, clamped at 4×), so their
  wall time is unbounded on a loaded box; the same package measured 600s-plus with a go-test panic
  before the exclusions and 191s after. They boot the daemon end-to-end: E2E work.
* **`TestAckImpliesDurable`** (`internal/ledger`) — SIGKILLs a child writer and demands 500 acks
  within the child's fixed 180s window (`-test.timeout=180s`); that is a host-throughput
  assumption, not a ledger property, and at load 44 the child was cut off at 322 acks (the panic
  row in the table above). The durability property itself is graded by the parent half
  (ack → record present after restart, LastSeq ≥ max acked) on a quiet host. The repo's own
  loadfence doctrine (`internal/loadfence`, fence 45) would be the in-test fix; that is a code
  change this row deliberately does not make — the exclusion is named here instead, and the full
  suite still owns the test.
* **the `TestE2E` chains** — the sentinel E2E suite boots the real ledger, and under parallel load
  it answers `429 ledger backpressure` (`TestE2EStoreAndEnvelopeSameDigest` at load 48, measured
  this tick). The predecessor list named three of these by full name; two runs produced two
  different reds inside the same family, so the skip list now ends with the bare `TestE2E` prefix
  — an exact class boundary: `func TestE2E` matches exactly the nine E2E harness entries
  (scrub, sentinel ×5, daemon subsystems ×2 — verified by grep) and nothing else. The in-test fix
  is the same loadfence doctrine as above; until then the full suite owns them on a quiet host.
* **throughput/latency floors** — asserted against a quiet-host number while printing the observed
  load; the same class as the above, not yet routed through `internal/loadfence` (which is what
  makes the sensors and dashboard budget gates SKIP under load instead of going red).

`guards.test_timeout` is 900s and the overall guard budget `hook_timeout` is 1200s — the second
number must stay above the first, because a guard run that exceeds `hook_timeout` fails OPEN
(GR-064e: remaining checks skipped, commit allowed, a warning printed). A 300s default there would
have made every real run of this lane meaningless. The window remains a bound — a hung test is
still cut.

### How to read a guard failure

* `Tier 1 Guards: FAIL` + exit 1 — a check ran and failed. Read the named step: `secrets` means a
  finding outside the allowlisted fixture paths (fix the commit, do not widen `.gitleaks.toml`
  without the evidence rule at the top of that file); `go_build`/`go_lint` mean compile/vet
  breakage; `tests` means the scoped lane went red — check whether the failure is one of the named
  harness classes (its name will show in the skip list of `test_command` if it should not have run)
  or a real regression.
* `Tier 1: DEGRADED PASS` + exit 2 — a substantive gate did no work this run (the skips are listed
  on the line). Not evidence the tree passes; stage files or widen the grade. Exit 0 on a degraded
  run requires `guards.allow_skips: true`, which this repo does NOT set on purpose.
* `⚠ Guard timed out after Ns (hook_timeout) … commit allowed to proceed (fail-open)` — the run
  blew the overall budget and skipped the remaining checks. Treat as UNKNOWN, not PASS: re-run, and
  if it recurs raise `hook_timeout` above the real `test_command` wall time instead of shrugging.
* A `~` line in the summary marks a step that did no work — never read it as a green check.
* The persisted run log (printed as `guard log: …`) carries the untruncated output; the console
  caps findings at a few lines.

### The secrets allowlist is a file list, not a blanket

`.gitleaks.toml` names the gitignored `.worktrees/` tree and the exact tracked files that must carry
secret-shaped text (the scrub rule table, the scrub test vectors, SPEC-02/SPEC-08/SPEC-TYPES
examples, the generated specs review page, and the tracked fixture tests). No directory glob, no
generic regex pattern. It stays a gate: a credential committed to a path that is **not** on the list
is still caught, by both scanners. Verified 2026-09-19 by planting four secret-shaped markers (an
AWS access-key id, a 40-char provider secret, a GitHub PAT, and an OpenSSH private-key header with
base64 body) in a scratch file under `internal/ledger/` (a non-allowlisted tracked directory), then
running the guard with the file staged:

* `gitreins guard` → `Tier 1 Guards: FAIL`, `secrets — FAIL (gitleaks: 2 findings; builtin
  cross-check: 4 findings)`, exit 1. go_build and go_lint still ran and passed — the secrets lane
  fails the run, it does not mask the others.
* The judge-mode tree scan (`gitleaks detect --no-git` through the generated config) went 0 → 2
  findings (the GitHub PAT and the provider-secret assignment), exit 1.

The split is the honest result and the reason GitReins runs two scanners: gitleaks' default ruleset
matched two of the four markers and missed the bare AWS access-key id and the PEM header; the
built-in cross-check caught all four. Neither scanner is trusted alone. Deleting the scratch file
and un-staging it returned both to clean (0 findings, exit 0).
