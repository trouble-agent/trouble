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
`<config>/rules.d/10-defaults.toml` by `trouble install`". This is the third trap: **boot the daemon without `trouble install` and no rules.d
exists**, so on this build the inotify sensor reported
`TROUBLE-SENSORS-025: watch path .../config/rules.d: no such file or directory`
and `/health.json` said `degraded`. The shipped defaults are compiled in, so the
daemon still detects — but the file-watch sensor is dead. The right way for a
hand-rolled instance: copy `examples/rules/*.toml` into
`<state_root>/../config/rules.d/` before boot (or run `trouble install`, which
does it for you). Verified: with the rules installed the same config reports
`status:"ok"` and all six sensors `degraded=false`.

**That health alarm was itself a defect, and TRBL-022 removed it** (SPEC-03
§3.7b): the absence of `rules.d` is a first-run state, not a capability failure,
so the inotify sensor now stays up with the shipped defaults active, names the
absent path in its `reason` and writes one informational record
(`payload.kind = "sensor_note"`, no `error_code`). `TROUBLE-SENSORS-025` is kept
for a path that EXISTS and cannot be watched. The rest of this section stands:
copying the rule files in is what arms the file-watch trigger.

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
| 3 | `TROUBLE-SENSORS-025: watch path .../config/rules.d: no such file or directory` | `rulesDir = <state_root>/../config/rules.d`, installed only by `trouble install`; on this build the absence degraded `/health.json` (fixed by TRBL-022, SPEC-03 §3.7b — absence is named, not degraded) | copy `examples/rules/*.toml` there to arm the file-watch trigger, or run `trouble install` |
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

---

# 2026-09-25 — compose container quickstart (append)

## What was driven, and how it works

The compose stack (trouble distroless + redis) is the "container host" profile of the
same daemon: one config file bind-mounted read-only, state in a named volume the
distroless uid 65532 owns, a dashboard token minted from a one-shot container INTO the
volume (because every outside write path fails the 0600/uid rules by design). The
ingest zone is decided by the socket peer, not the bind: through the published port
every request is "lan", so only the X-Sentry-Auth header form is admitted — the
query-form refusal is the bind matrix working, not a bug.

## Why the traps exist (learned this run)

- **`degraded` on a healthy container is structural.** The distroless image has no
  systemd, so the timers sensor can never be non-degraded. The health contract reports
  honestly; what is missing is one operator-facing sentence (TRBL-077).
- **Sensor noise in a fresh ledger is the standalone rule set meeting a host with no
  substrate.** Sensors probe and watch; a container gives them an overlay root and no
  journal, so they fire "observation" events that suppression marks noise. The
  substrate self-detection the PSI boot probe already has would kill the noise class
  (TRBL-078).
- **No event-level dedup on the on-ramp** (TRBL-074, still open): the group record is
  the only fold, keyed on first-seen; a retrying reporter multiplies event records.
  The hub profile's idempotency-key gate is the fix direction.

## What held (the value case)

- The redis outage drill: stop redis → health answers, 6 events accepted 200 across
  the window, restart → all events in the ledger, zero loss, groups/incidents formed.
  The SPEC-13 contract the compose file header promises is real.
- Auth is fail-closed on every probed surface (no/bad dashboard token → 401; bad
  ingest secret → 401 + X-Sentry-Error code).
- Dashboard serves the ingested truth in ~1-4ms per page.
- Ingest latency is the durability window: 288ms warm / 545ms cold per event
  (ack-after-fsync, fsync_window_ms=200) — the number that buys the durability
  promise.

## Harness lessons for the next runner

- Kill leftover native `troubled` before compose (ports 7643/7644 collide; the 09-24
  run's daemon lived 19h past its "clean stop" — TRBL-079).
- Bunker docker = rootless on a bunker-specific socket; DOCKER_HOST must point at
  /run/bunker/<agent>/docker.sock and `systemctl --user start docker` cannot work
  (TRBL-080 has the full recipe + numbers: clone→build 120s→up 7s→healthy→proof).
- A board append's row-count assert must ignore pre-existing blank lines: 97 physical
  lines were 96 real rows here; count JSON rows, not lines.

## 2026-09-25 run 2 — operator CLI, token lifecycle, dashboard (tick 16-12-39)

**Why this run exists:** the angle rule. Prior runs swept the ingest plane and the
compose path; nobody had used `trouble` (the operator CLI) or the token lifecycle
as an operator does. See `2026-09-25-operator-cli-integration.md` for the full
transcript; here are the lessons that generalize.

- **A CLI/daemon pair sharing a store path is an API contract nobody tests.**
  docs/cmd.md advertises `--dashboard-token_file` as an argv-addressable store;
  the token CLI has no counterpart flag and always mints into the compiled
  default. Two contracts that each pass their own tests diverge silently, and
  the user sees only 401s (TRBL-081). The check that would catch it: mint a
  token with the daemon on a non-default store and assert a 200 — one
  integration test, zero unit-test coverage anywhere in the pair.
- **rc=8 from `check-stall` is a verdict, not a crash.** The watchdog probe
  reports TROUBLE-LIFECYCLE-008 heartbeat_stale with the honest seq_age and
  exits nonzero. Scripting against this CLI means treating any nonzero rc that
  carries a named TROUBLE-* code as a report, not a failure to retry.
- **Revoked tokens stay in `token list` with `revoked:true`** — tombstones for
  audit. Before assuming a listing bug, check the flag; the hash-only 0600
  store plus tombstones is the designed shape.
- **Bare-Debian harness lessons (bunker leg, agent 5c261866):** an agent-local
  `/tmp` write inside a piped script can fail (curl error 23) where the same
  write to `$HOME` succeeds — download throwaway artifacts into `$HOME`;
  and re-confirm the `setsid`-in-own-statement launch pattern: `nohup … &`
  inside ssh one-liners backgrounds the whole chain and yields an empty log.
- **What held:** the scratch-instance promise (flags-only boot, zero config
  edits), named-error-code discipline on every refusal (004 forbidden root,
  001 unknown flag/table-key, 016 install gate — all self-explanatory), live
  token rotation/revocation, hash-only token store, 7.5ms dashboard pages,
  and the checkout-free `go install` path end-to-end from the public module.
- **Checkout-free `go install` smoke that actually proves something:**
  `--version` prints the UNSTAMPED degraded posture (expected — no git
  history), unknown-flag refusal returns rc=13, `config explain --key
  ingest.bind --json` emits provenance, `topology` renders. Four commands, no
  daemon, all from the public module path.

## 2026-10-01 run — fresh-machine install + incident/dashboard loop (tick 17-02-42)

### What was driven, and how it works

The install leg prior tick 09-28 skipped (TRBL-089) ran for real on las-bunker-03
agent 4812a03c: bundle-transfer (private repo, no credential — the per-file loop
was replaced by ONE `git bundle` file, size-verified both sides), clone inside the
agent, Go 1.26.5 tarball 31 s (README documents this prerequisite first — the
quickstart passed the top-down-order test), `make bin` 39 s stamped e2b5b6d,
canonical config edits, healthy <1 s, first event 200 cold 291 ms, ledger proof,
auth fail-closed, clean stop, restart persistence verified.

Then the ladder surface no prior run had touched on a CLEAN host: boot noise
(dbus 36 events/8 s → 22 incidents/5 s → rule:dbus_unit_failed breaker open within
the first minute), the groups page rendering the deliberate first POST grouped
correctly (4 identical events → 1 event record + 4 group records, one sig), and
dashboard pages at 0.9–4.3 ms with read-scope enforcement.

### Why the traps exist (learned this run)

- **Mint-while-daemon-down tokens 401 forever (TRBL-093, P1).** The CLI mint
  writes the store; the daemon (on next boot) rejects the token. Contrast arms:
  mint-while-up works immediately AND survives restart. The broken arm is narrow
  and needs source triage in cmd/trouble vs internal/dashboard/tokens.go.
- **A first boot is a guaranteed breaker trip.** Storm breakers keyed on
  "N incidents in 5m" fire on a clean headless host's own session noise. Design
  tension, not a bug: the breaker is correct; the first-minute noise is the
  problem (TRBL-094, P2, consequence of TRBL-078 one rung up).
- **Bundle > per-file loop for private repos on unattended surfaces.** One 20 MB
  file vs hundreds of ssh cats; `git clone ~/x.bundle` gives real fetch semantics;
  remember `git remote set-url origin <real-url>` afterward, and say "bundle-based"
  in the report — it is not a network-fetch proof.

### What held (the value case)

Fresh machine, zero operator config beyond the documented edits: daemon healthy in
under a second, ingest fail-closed on bad key, the ladder reacting to real host
noise within seconds, the dashboard rendering real counts/rates immediately, ledger
and tokens persisting across restart. The product promise — "a self-hosted incident
brain that detects, records, and decides" — held end-to-end on a machine that had
never seen it.

### Harness lessons for the next runner

- Bunker agent `/tmp` is root-owned: write build logs to `$HOME` (hit twice across
  two runs — it is now a known agent property, not a fluke).
- `head`/`tail` are absent on the bare agent; the quickstart's own curl+jq
  dependency list is exactly right.
- Two token-fumbling modes in one run: a heredoc that passes `$TOK` literally
  (single-quote the heredoc delimiter AND verify `tok_len`), and grep truncation of
  tokens containing `-`. The one-shot plaintext design makes script mistakes
  permanent — mint interactively or capture with a strict full-line regex.
- Verify token auth with `tok_len` + one immediately-following authed request in
  the SAME ssh session; cross-session env assumptions cost three false 401s here.

## 2026-10-16 — why the ledger CLI verbs disagree with their docs (TRBL-104..108)

The 2026-10-13 dogfood found the promised `trouble ledger` verbs missing
(DF-LEDGER-1); TRBL-100 wired them on 10-15 and the 10-16 re-proof confirms they
work and honor the SPEC-01 §2.4 exit contract. Real use of the new surface then
exposed WHY the docs and the verbs disagree — two design facts worth recording:

1. **Writer-mode Open is the only Open.** Every ledger entry point — daemon,
   `hub drain`, and now the CLI verbs — calls `ledger.Open` with a writer actor
   and takes `flock(LOCK_EX|LOCK_NB)`. There is no read-only constructor in this
   build. Consequences: the CLI verbs cannot run against a live daemon
   (TROUBLE-LEDGER-005, correct per SPEC-01 §3.4 rule 6, but it makes ops.md §2's
   "verify opens no lock" promise unimplementable as written), and even a
   "read-only" status boot writes records (its own `boot` lifecycle op, HEAD
   refresh, sidecar rewrite) because recovery is part of Open. TRBL-104/105 file
   the user-visible half; the fix direction is a true read-only walk entry
   point, not doc polish alone.

2. **Two canonical forms of one payload.** The writer serializes payloads from
   Go structs (field order preserved: `"key","value","source",...`); verify
   re-parses the stored bytes into `map[string]any` and re-marshals, and Go
   sorts map keys (`"key","redacted","source",...`). `payload_sha256` is
   computed over the first form and checked against the second, so any
   struct-ordered payload (config snapshots, lifecycle records — the audit
   spine) fails the hash check deterministically: 1000 re-marshals of a real
   seq-2 config payload produced zero byte-identical forms. Events with
   map-shaped payloads agree, which is why the false positive stays rare
   (~1 record per boot) and looks like noise. TRBL-106; fix is to hash/verify
   the raw stored bytes (or canonicalize both sides through the same path).

3. **Crash shape that beats the torn classifier.** A torn fragment followed
   immediately by an intact record with no newline between them (two batches,
   crash between their writes) reads as ONE line whose JSON decode succeeds on
   the first value; `tornShaped` (which requires io.ErrUnexpectedEOF) then
   returns false and the line is filed as 011 corrupt — the intact record
   sharing the line is swallowed. Proven by injecting exactly that shape
   (Go probe on the real bytes, tornShaped=false) and watching the boot heal
   skip it. TRBL-107; the classifier needs the decode-OK-with-trailing-bytes
   branch and a cut-at-first-value recovery.

Fresh-machine leg (bunker-mvp agent 9dee92cf, evidence
/tmp/bunker-qa-evidence-20261016T115442Z-2269145.jsonl): fresh-install OK,
chaos-corruption OK, ci-pass/chaos-resource FAIL on the known load-budget class
(TestLoadIngestThroughput / TestArtifactParseBudget / TestBudgetDashboardRSS)
plus TRBL-100's own TestLedgerVerifyExitCodesSpec01Section24 joining it under
the 3G cap — the new surface shipped with a test that is load-sensitive on
4-vCPU agents, same lineage as QA-TROUBLE-17.


## 2026-10-20 — how the verify false positive came back, and where the writer seat leaks (tick trouble-dogfood-2026-10-20-00-18-40)

1. **A fix that only ever existed on a branch reads exactly like a fix.** `98ba880` (TRBL-106)
   added `verifyPayloadHashLine`/`payloadBytesWithoutHash` and a 487-line regression test, and the
   row was closed with "verified fix on wt/TRBL-106". At HEAD `d081f79` the symbols do not exist in
   any non-test file, the test file is absent, and `internal/ledger/recover.go`'s `Verify` still
   runs the old block (`cp := map[string]any{}` … `payloadHash(cp) != h`). The right way to check a
   closed fix is one command away and must be part of every closure:
   `git merge-base --is-ancestor <fix-sha> origin/main` (or a content grep against `origin/main`).
   Grading the branch tip grades a proxy; the branch tip is exactly what the closing commit cites.

2. **The way to tell an honest payload hash from a verifier bug — without trusting either side.**
   Take the stored line, locate the raw `payload` bytes, splice out the reserved `payload_sha256`
   member at the byte level, and hash what remains (`hex(sha256)[:16]`). If that equals the stored
   `payload_sha256`, the record is intact and the verifier's recomputation is wrong; if it does not,
   the record is damaged. On this run's virgin ledger the raw-bytes hash matched exactly
   (`318d8e8e57a48537`) while the recursive sorted-key re-marshal produced `571d2947a9469af3` — which
   is why `verify` reports `seq 2: payload hash mismatch` on a ledger the daemon wrote itself, once
   per boot config record, and why `docs/operations.md` section 2's daily check fails forever on a
   healthy deployment.

3. **A refusal is not a no-op: check the state root after every "it just refused" leg.** A `--dry-run`
   `compact` that refuses (TROUBLE-LEDGER-007, rc=1) still opened the ledger and left a `LOCK` file
   naming the CLI's own now-dead pid (`cmd/trouble/ledger.go:301` opens before the decision). The
   read-only status path then reports that pid verbatim because `internal/ledger/readonly.go:176`
   parses `LOCK` without the kill-0 liveness check its own sibling `CheckWriterHeld` (lines 44–62)
   applies. The way to see it: run `ledger status --json` with no daemon (`writer_pid=-1`), run the
   refusing verb, then run `ledger status --json` again (`writer_pid=<dead pid>`) and `kill -0` that
   pid. Any verb that reports "the writer" must answer "is it alive" from the process, not from a
   file the previous process left behind.

4. **Stale hints are by design; silent gaps were not (TRBL-110 resolved).** SPEC-01 section 3.4 rule 7
   defines `seq_alloc = max(HEAD.last_seq, highest seq in the highest-generation file) + 1`, so a
   `HEAD` hint above the file's tail *is* the spec. What the 2026-10-16 run correctly caught was the
   gap riding silently; the trail now records `lifecycle{op:hole,from,to}` and `seq_recovered`, which
   is the honest half. Do not re-file the allocation as a defect — check the spec line first.


## 2026-10-20 (run 2) — one defect, two verifiers: why a merged fix can still leave the user broken

5. **A verifier fix must be re-driven through the CLI, not just through the package that was patched.**
   This repo now has TWO payload-hash checks with the same error string: `internal/ledger/recover.go:548`
   (`Ledger.Verify`, patched by the merged TRBL-106 fix at `:538`, helper at `:617`/`:641`) and
   `internal/ledger/readonly.go:307` (`ReadVerify`, added when TRBL-104/105 made the verbs read-only).
   `trouble ledger verify` calls `ReadVerify` (`cmd/trouble/ledger.go:232`), so the fix landed on the
   path only the in-process tests use. The tell was cheap: `grep -rn 'payload hash mismatch' --include='*.go' internal/`
   returns two producers — fix one, and the other keeps emitting. Whenever a verdict string exists in
   more than one walk, treat it as a family and assert on the USER surface (a CLI run against a
   freshly written ledger), not on the helper's unit test.
   Bisect recipe that separates "helper wrong" from "helper unused": replicate the merged helper in a
   throwaway Go program OUTSIDE the repo (unmarshal the raw line's payload into
   `map[string]json.RawMessage`, delete the reserved member, `Encoder(SetEscapeHTML(false))`,
   `sha256[:16]`) and compare its digest with the stored `payload_sha256`. Exact match = the algorithm
   is right and the wiring is wrong; mismatch = the algorithm is wrong. Here it matched to the byte.
