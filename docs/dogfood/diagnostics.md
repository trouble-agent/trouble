# trouble — diagnostic trail (dogfood run, 2026-09-17)

This is the "how is this thing put together, why is it like that, what went
wrong, and what is the right way" record from a real dogfood run on build
`c363daf`. It is written to be read *after* the fact — by an agent landing in
the repo, or by a person asking "is this project worth anything and does it
actually work?". Findings and their board rows are in
`docs/dogfood/2026-09-17-integration.md`; this file explains the machinery
behind them.

Nothing here is a raw log dump. Each error is followed by the mechanism that
produced it and the right way to do it.

---

## 1. Shape of the system (three layers, one direction)

```
cmd/troubled     the daemon: boot sequence, loops, drain            (SPEC-12 §4)
cmd/trouble      the operator CLI: install, explain, checks         (SPEC-12 §2)
internal/app     the composition root: adapters + subsystem wiring
internal/<subsystem>   ledger, scrub, sentinel, sensors, ladder,
                       registry, research, flow, issues, skills, dashboard
```

The dependency rule is the reason this code is comprehensible: every subsystem
imports `internal/types` and nothing else from its siblings, and `internal/app`
is the only place two contracts meet (`app.Store` joins ledger↔ladder,
`app.PlayEngine` joins registry↔ladder, `app.Evaluator` joins the sensors'
condition language to the ladder). `internal/dashboard` is deliberately a leaf:
it receives everything through `Deps` and opens no ledger file.

Boot order (verified in the log lines a real boot emits):

```
config resolve → state root + modes → secret-file modes → schema_version
→ ledger open + index rebuild → config record → bind preflight (listeners held)
→ sensors start → checker alarm → dashboard listening → serve
```

Drain is the mirror image and works: `SIGTERM` → `STOPPED`/park → ledger flush
→ a final heartbeat with `stage:"shutdown"` → exit 0. Observed three times on
throwaway state roots with no torn ledger lines.

## 2. Why the config traps exist (and the right way)

### 2.1 Config resolution is generic, typed and per-key

`internal/lifecycle/config.go` holds a `registry()` of every known key with a
typed setter, which is what `trouble config explain` dumps with provenance
(each row's `source_ref` is the config path, the `--flag` spelling or the
`TROUBLE_*` env name that won). Unknown *file* keys are fatal
(`TROUBLE-LIFECYCLE-001`), unknown *env* keys are ignored with a near-miss hint.
Precedence: flag > env > file > default. A value that differs from its compiled
default is ordinary configuration and records nothing; `TROUBLE-LIFECYCLE-002`
is reserved for two *operator* sources disagreeing (SPEC-12 §3.1f).

That design is good and is why `config explain` is genuinely useful. Two
consequences explain the first two traps:

* **`verify.zone_windows`** — the setter `applyToMapStringDuration` accepts a
  map or a plain string, and the string branch is the one SPEC-12 pins
  (`loopback=10m lan=15m tailnet=20m public=30m`). TOML inline-table syntax in
  the shipped example reaches the string branch as a scalar starting with `{`,
  so the first `k=v` split yields the literal pair `"{"`. The right way in a
  config file is the **string form**; the *better* fix is to make the example
  and the parser agree (TRBL-005).
* **`dashboard.token_file`** — the dashboard's own `DefaultConfig()` carries
  `~/.config/trouble/dashboard-tokens.json` as a literal string, and
  `internal/app/dashdeps.go` only copies the lifecycle value when the operator
  set one. Nothing in the dashboard package expands `~`. `os.Stat` then fails
  and `readFileIntoSet` documents its behaviour explicitly: *"A missing file
  yields a valid empty set"* — fail-closed, no error, no log, no health
  signal. The right way today is to write an **absolute** `dashboard.token_file`
  (TRBL-006); the right fix is to resolve the path once, in one place, for both
  the CLI and the daemon.

### 2.2 `state_root` outside the usual places is refused - and that is correct

```
TROUBLE-LIFECYCLE-004: state_root "/tmp/..." resolves under forbidden root "/tmp"
```

This is a deliberate, well-messaged refusal (the ledger is append-only and
auto-filed; a world-writable parent is the wrong place for it). Keep it: a
throwing directory must live under the operator's home, `0700`, with files
`0600`. It only feels like friction because scratch work conventionally starts
in `/tmp` — put dogfood state roots under `~/dogfood-<name>/state` instead.

### 2.3 Rules live in `<state_root>/../config/rules.d/`

`internal/sensors/config.go:144` derives `rulesDir` as
`filepath.Join(filepath.Dir(stateRoot), "config", "rules.d")`, and
`internal/sensors/rules.go:118` notes the directory is "Installed to
`<config>/rules.d/10-defaults.toml` by `trouble install`". This is the third
trap: **boot the daemon without `trouble install` and no rules.d exists**, so the
inotify sensor reports
`TROUBLE-SENSORS-025: watch path .../config/rules.d: no such file or directory`
and `/health.json` says `degraded`. The shipped defaults are compiled in, so the
daemon still detects — but the file-watch sensor is dead. The right way for a
hand-rolled instance: copy `examples/rules/*.toml` into
`<state_root>/../config/rules.d/` before boot (or run `trouble install`, which
does it for you). Verified: with the rules installed the same config reports
`status:"ok"` and all six sensors `degraded=false`.

### 2.4 Subsystems are best-effort by design — which is how the product goes missing

`internal/app/subsystems.go:buildSubsystems` never fails the boot: a subsystem
that cannot be constructed is skipped and one `lifecycle` record is written
(`recordSubsystemRefusal` → `stage:"subsystem_not_built"`, `name`, `detail`).
The intent is operator-friendliness ("never fail the boot for a subsystem the
operator can live without"), and the dashboard's rule is that absence is shown
by *no record*.

The failure mode this creates is exactly what the dogfood run hit: from a stock
config, `buildSentinel` returns `nil, "no sentinel projects configured"` because
the project set only ever arrives in-process
(`SubsystemOptions.SentinelProjects`) — there is no config key and no CLI verb
that supplies one. So:

* `ingest.bind` never binds (nothing listens on 7643 → `POST /api/{id}/event/`
  is connection-refused);
* `issues` refuses (`TROUBLE-ISSUES-003: driver github needs owner and repo`);
* `skills` refuses (`TROUBLE-SKILLS-001: exactly one of source_path or source_url`).

And nothing operator-facing says so: `/health.json` has no `subsystems` block,
and once the rules directory exists the status is `ok`. The right way to see the
truth today is the ledger:

```bash
jq -r 'select(.kind=="lifecycle" and .payload.stage=="subsystem_not_built")
       | "\(.ts)\t\(.payload.name)\t\(.payload.detail)"' state_root/ledger/*.jsonl
```

That is the diagnostic this run would have given anything for, and it is why
TRBL-007 asks for a `subsystems` block in the health surface.

## 3. The pieces that worked (and why they are worth trusting)

### 3.1 Ledger (`internal/ledger`, SPEC-01)

Plain JSONL, generation/part rollover, `LOCK`, `HEAD` as a hint (never a source
of truth), `idmap.jsonl` for satellite id mapping, `quarantine/` for files the
version gate refuses. A record is durable when `Append` returns: group commit
fsyncs a batch, so the loss window on power loss is `ledger.fsync_window_ms`
(default 200 ms) and a SIGKILL loses nothing. Observed: four boots over one state
root, no torn lines, sequences monotonic, `jq` and `grep` both work (that is the
point of not compressing `ledger/`).

Records carry `seq`, `rec_id`, `kind`, `sig`, `inc` (incidents), `origin
{host_id, hub_id, source}`, `actor {kind, id, version, git_sha, build_time}`,
`redactions` and `payload`. The stamped actor triple on every record is a nice
touch: you can always tell which binary wrote a line.

### 3.2 Scrub (SPEC-02)

The pipeline is ingest → scrub → ledger and it is a *safety invariant*, not a
convention: the persistence boundary re-scans every serialized line, rule
failure means "not persisted" plus a `gap` record, and idempotence
(`Scrub(Scrub(x)) == Scrub(x)`) is what lets a satellite scrub locally and the
hub re-scrub on receipt. This run did not exercise the rule table (no ingest
plane available), so treat scrubbing as *spec-verified, not use-verified*.

### 3.3 Sensors (`internal/sensors`, SPEC-03)

Six sensors ran for real and produced real observations on the test host (psi
407, dbus 259, disk 10, inotify 4, timers 4, journald 0 events in six minutes).
The PSI capability probe is the most interesting thing in the repo: rather than
assuming kernel behaviour, the daemon arms one real fd, sweeps the window ceiling
`20s → 10s → 2s`, requires a `stall=0` probe to be refused, writes the result as
a `capability_probe` record and repeats it verbatim in `/health.json`
(`mode=triggers+sampling probe=ok max_window_us=10000000` observed). That is the
"I measured it on this host instead of documenting it" discipline the README
claims, and it is real.

Two behaviours that surprise operators and are documented here so nobody
rediscovers them: journald units on this class of host live in the **user**
manager (watching only the system manager is an all-green lie), and a journal
seek failure is never "no logs" (`TROUBLE-SENSORS-007` + `--since` fallback +
one gap record).

### 3.4 Ladder (`internal/ladder`, SPEC-05)

It owns incident identity and is the only component allowed to declare something
fixed. Observed working: 74 incidents from `dbus_unit_failed` + psi rules, states
advancing (`to:"recorded"`, `entry_rung:"play"`), a storm breaker opening with a
reason (`rule:dbus_unit_failed: 21 incidents inside 5m0s`) and a bounded
`open_until`. What is *not* working is the fold implied by "dedup/merge upsert
keyed by `sig` and `inKey`": the same sig **and** the same inKey produced 3–4
distinct incidents minutes apart, and one psi sig produced 137 event records in
six minutes (TRBL-009). The breaker masks the symptom; the identity layer is
where to look.

### 3.5 Dashboard (`internal/dashboard`, SPEC-10)

Read-mostly, stdlib + `internal/types`, every subsystem arriving through `Deps`.
Verified live: 20-route table plus a deterministic 404 rule; anonymous page 401;
URL-borne token **400** (refused by design); a read token on a POST 403
`TROUBLE-DASHBOARD-003 insufficient scope`; CSRF value exposed in
`<meta name="trouble-csrf">` and required on every POST; token store keeping
only `sha256` hex (no plaintext anywhere, `list` never leaks it); the
stale-render guard visible in the markup (`data-seq`, `data-rendered-ts`,
`data-stall-s`, `#stale-banner`). New tokens are picked up by a running daemon
without a restart (the store re-reads its file).

Two operational facts worth knowing:

* the budget/health **fragments** carry the truth; the first server-rendered
  paint of the budget panel after a boot can show blanks/zeros until the first
  poll (TRBL-010);
* the auth-failure throttle is keyed on the client IP (10 failures / 60 s), and
  it throttles *valid* requests too — spamming a wrong token locks you out of the
  dashboard for the window, so a debugging loop should be slow rather than
  fast. Response: `429 TROUBLE-DASHBOARD-012 auth_failure_throttle`.

### 3.6 Build stamping (`internal/lifecycle`, SPEC-12 §3.4)

`make bin` injects `-X ...lifecycle.Version/GitSHA/BuildTime`. An unstamped
build is *visibly* degraded: `--version` says `UNSTAMPED` and
`trouble install --check` refuses it with `TROUBLE-LIFECYCLE-006` (exit 13)
unless `--force` — which `tests/e2e/cli_smoke.sh` proves by building an
unstamped binary on purpose. Verified in the run: `--version` reports the
stamped triple, `/health.json` echoes `version`/`git_sha`/`build_time`, and the
ledger's `actor` carries the same values.

## 4. Errors hit, and the right way for each

| # | symptom (verbatim) | mechanism | right way |
|---|---|---|---|
| 1 | `TROUBLE-LIFECYCLE-001: key "verify.zone_windows": bad zone_windows pair "{"` | inline-table TOML in the example vs the SPEC-12 string form | use `zone_windows = "loopback=10m lan=15m tailnet=20m public=30m"` (TRBL-005) |
| 2 | `TROUBLE-LIFECYCLE-004: state_root ".../tmp" resolves under forbidden root "/tmp"` | deliberate refusal of world-writable parents | keep the state root under `$HOME`, `0700` |
| 3 | `TROUBLE-SENSORS-025: watch path .../config/rules.d: no such file or directory` | `rulesDir = <state_root>/../config/rules.d`, installed only by `trouble install` | copy `examples/rules/*.toml` there, or run `trouble install` |
| 4 | `401 {"code":"TROUBLE-DASHBOARD-002","message":"authentication failed"}` with a token the CLI just minted | literal `~` in the default token path → missing file → empty (fail-closed) token set | set an absolute `dashboard.token_file` (TRBL-006) |
| 5 | `POST http://127.0.0.1:7643/api/1/event/` → connection refused | sentinel not built: no projects configured, and no operator surface to configure them | read the `subsystem_not_built` ledger records; enabling ingest is currently a code change (TRBL-007) |
| 6 | `429 {"code":"TROUBLE-DASHBOARD-012","detail":"auth_failure_throttle"}` on a **valid** token | IP-keyed failure throttle (10 / 60 s) | space out retries; do not loop on auth failures (TRBL-010) |
| 7 | footer renders `trouble v ()` while health says `c363daf` | footer render path reads an empty version | trust `/health.json` + `/partials/budget` for the build identity (TRBL-010) |

## 5. How this run was verified (so the next one can repeat it)

```bash
make bin                                          # stamped binaries
bin/trouble --version                             # stamped triple
bin/troubled --config <throwaway>.toml            # boot; expect "rule set loaded" then "dashboard listening"
curl -s :7644/health.json | jq .status,.sensors    # sensor/capabilities truth
T=$(bin/trouble dashboard token create --config <cfg> --label x --scopes read,write 2>/dev/null)
curl -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $T" :7644/incidents
jq -r 'select(.kind=="lifecycle") | .payload.stage' <state_root>/ledger/*.jsonl | sort | uniq -c
bin/trouble check-stall --config <cfg>; echo $?    # 0 live
kill -TERM <daemon-pid>                            # drain; heartbeat stage=shutdown, ledger intact
```

Deliberately checked and **not** a finding: `check-stall` reads the ledger
sequence, not the heartbeat, so a stale `stage:"shutdown"` heartbeat left behind
by a previous instance does not produce a false exit 8 right after a restart. The
opposite assumption would have been a plausible-looking bug report.

Boot-to-serving latency measured 4.6 s – 15 s across four boots on a 16-core box
at loadavg ~7 (no breakdown captured — measure the PSI probe and the index
rebuild separately if startup latency ever matters). Not filed as a task; noted
here as a number.

## 6. What this run could NOT verify (honest gaps)

* **Ingest end-to-end** (envelope/store/generic-JSON, auth forms, quotas,
  rate-limit headers, the canary): the listener never bound, so the entire
  SPEC-04 wire contract is *spec-verified only*. The next run should verify it
  by constructing a project set in code (or when TRBL-007 lands a config
  surface).
* **Scrub rule table** behaviour on live payloads (redaction counts are present
  in the record schema; no event path existed to exercise them).
* **Issues, skills, flow, research**: refused (issues/skills) or unconfigured
  (flow/research), so nothing was exercised. Several `subsystem_not_built` rows
  exist for them; a future run should drive them from a config that builds them.
* **Install-from-scratch on a clean machine**: the ephemeral bunker spawn failed
  twice (`deadline_exceeded`), recorded as TRBL-011. The repo also has no git
  remote, so even a successful spawn could not have cloned it — a fresh install
  starts from an archive today.
