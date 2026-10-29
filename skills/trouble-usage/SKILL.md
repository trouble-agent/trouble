---
name: trouble-usage
description: >-
  How to actually run and drive the trouble daemon (incident brain): build the
  binaries, boot a working config, mint and use a dashboard token, read the
  ledger, and the four traps that stop a fresh user cold (example config does
  not parse, `~` token path 401s, no project = no ingest listener, no
  quickstart). Load this before touching ~/trouble.
version: 1.4.3
category: software-development
---

# Using `trouble` (agent-facing usage skill)

`trouble` is a self-hosted incident brain for a fleet of hosts: sensors detect,
the ledger records, the ladder decides, the dashboard renders, and (when
configured) a Sentry-compatible ingest plane receives events from applications.

This skill is the shortest path from "I landed in this repo" to "the daemon is
running and I am looking at real incidents". Everything here was verified by
hand on 2026-09-17 (`c363daf`, go1.26.5). See
`docs/dogfood/2026-09-17-integration.md` for the full transcript and
`docs/dogfood/diagnostics.md` for why the traps exist. The 2026-10-20 pass
re-verified every ledger verb at `d081f79`; its traps and evidence are in
`docs/dogfood/2026-10-20-verify-hash-recurrence-and-writer-seat.md` (board rows
TRBL-106-R2/112/113/114). The 2026-10-29 real-use pass at `50866ae` also
verified the end-to-end dashboard and ingest workflow; its timing and install
boundary are in `docs/dogfood/2026-10-29-real-use-integration.md`.

## Entry points

| surface | how | notes |
|---|---|---|
| daemon | `bin/troubled --config <file>` | boots, serves dashboard (+ ingest when projects are configured) |
| operator CLI | `bin/trouble …` | `--version`, `config explain`, `topology`, `check-stall`, `install`, `upgrade`, `escalate`, `dashboard token …`, `ledger status/verify/compact` (SPEC-01 §2.4) |
| dashboard | `http://127.0.0.1:7644` | token auth; `/health.json` is loopback-exempt |
| ingest | `http://127.0.0.1:7643/api/{project_id}/event/` | **only binds if projects are configured — see trap 3** |
| ledger | `<state_root>/ledger/YYYY-MM-DD.jsonl` | plain JSONL, `jq`-able, `seq`/`rec_id`/`kind`/`sig`/`inc`/`payload` |
| specs | `specs/SPEC-INDEX.md` + `SPEC-01..13` | authority; `python3 specs/tools/selfcheck.py` must pass before spec commits |

Build: `make bin` (stamped), `make build`, `make check`, `make smoke-e2e`.
Module path is `github.com/trouble-agent/trouble` (renamed from the
pre-publish placeholder in a single mechanical pass — go.mod plus import
prefixes, SPEC-INDEX §6.3).

## The 4 traps (each one stopped a real run)

### Trap 1 — `examples/config.toml` does not boot as shipped

```
err="TROUBLE-LIFECYCLE-001: key \"verify.zone_windows\": bad zone_windows pair \"{\""
```

The example uses a TOML inline table; the parser wants the SPEC-12 string form.
Fix in your config copy:

```toml
verify.zone_windows = "loopback=10m lan=15m tailnet=20m public=30m"
```

### Trap 2 — the default dashboard token file is a literal `~` → every token 401s

`dashboard.token_file` defaults to `~/.config/trouble/dashboard-tokens.json` and
**is not `~`-expanded**, so the daemon reads "missing file = empty token set":
the CLI mints a valid `tdt_` token and the daemon answers
`401 TROUBLE-DASHBOARD-002`. Always set an absolute path:

```toml
[dashboard]
bind = "127.0.0.1:7644"
token_file = "/home/user/.config/trouble/dashboard-tokens.json"
```

Then `bin/trouble dashboard token create --config config.toml --label me --scopes read,write`
(plaintext on stdout, shown once; `list --json` never leaks it). New tokens are
accepted without restarting the daemon.

### Trap 3 — no projects configured ⇒ no ingest listener, silently

Boot writes `subsystem_not_built` lifecycle records for `sentinel`
("no sentinel projects configured"), `issues` and `skills`. Consequences:
nothing binds `:7643`, `POST /api/{id}/event/` is refused, and `/health.json`
still says `degraded` with a *sensor* detail. v0.1 has no config surface or CLI
verb that supplies a project set, so the ingest plane cannot be enabled by an
operator — plan around it (use sensors + dashboard, or wire projects in code).

### Trap 4 — no quickstart in `README.md`

README documents build+test only. Run instructions live in `docs/cmd.md`,
deploy/install in `deploy/README.md`, and the ingestion surface in
`docs/sentinel-compat.md`. Read those three before concluding something is
missing.

### Trap 5 — `ok` does not mean "complete": read the ledger

`/health.json`'s `status` aggregates the sensor rows AND the subsystem rows, so a
`status:"ok"` proves only that neither raised. Since TRBL-022 (SPEC-03 §3.7b) an
absent rules directory is **not** a failure: a hand-rolled boot without
`trouble install` keeps the inotify sensor up with the shipped defaults active
and the absent path named in its `reason` (`rules dir <path> absent; the shipped
defaults are active`) — it no longer reports `TROUBLE-SENSORS-025` or `degraded`.
A sensor that IS degraded (a rules path that exists but cannot be watched, for
one) still lifts the status, but so does a **refused subsystem**
(`sentinel`/`issues`/`skills` without projects), which the sensor rows cannot
show. Copy `examples/rules/*.toml` into `<state_root>/../config/rules.d/` when you
want the file-watch trigger (`trouble install` does it for you) and treat the
ledger as the whole truth:

```bash
jq -r 'select(.kind=="lifecycle" and .payload.stage=="subsystem_not_built") | .payload.name, .payload.detail' state_root/ledger/*.jsonl
```

Treat that query as part of "is the daemon healthy", not as a debugging step.

## Working config (throwaway, verified)

```toml
state_root = "/home/user/dogfood-trouble/state"        # NOT under /tmp: TROUBLE-LIFECYCLE-004 refuses it
config_path = "/home/user/dogfood-trouble/config.toml"
[secrets]
environment_file = "/home/user/dogfood-trouble/trouble.env"   # mode 0600, or boot refuses
[stall]
max_seq_age = "300s"
[checker]
interval = "60s"
confirm_runs = 2
[ingest]
bind = "127.0.0.1:7643"
[dashboard]
bind = "127.0.0.1:7644"
token_file = "/home/user/.config/trouble/dashboard-tokens.json"
[verify]
zone_windows = "loopback=10m lan=15m tailnet=20m public=30m"
[escalate]
channels = []
```

* `state_root` must be `0700`, the env file `0600`; all files land `0600`.
* Never bind 8642/8643/9090/3000 (other fleet services) and never 7643/7644 on
  a shared host without checking `ss -ltn` first.

## Driving it (verified commands)

```bash
bin/troubled --config config.toml &            # boot; expect "dashboard listening"
curl -s localhost:7644/health.json | jq '.status, .runtime_watermarks, .sensors[].sensor'
T=$(bin/trouble dashboard token create --config config.toml --label api --scopes read,write 2>/dev/null)
curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $T" localhost:7644/incidents   # 200
curl -s -H "Authorization: Bearer $T" localhost:7644/partials/budget                              # runtime numbers
bin/trouble check-stall --config config.toml; echo $?          # 0 live / 8 liveness unreadable / 9 seq stall
bin/trouble config explain --json | jq '.[] | select(.key|startswith("dashboard"))'
```

Auth rules that bite: anonymous page = 401; a token in the URL (`?token=`) = 400;
a POST with a read-scope token = 403 `TROUBLE-DASHBOARD-003`; every POST needs
the CSRF value from `<meta name="trouble-csrf">`. After ~10 failed auth attempts
from one IP the daemon returns `429 TROUBLE-DASHBOARD-012 auth_failure_throttle`
for the whole 60 s window — **even for a valid token** — so slow down rather
than retry-looping.

## Reading the ledger (the honest source of truth)

```bash
jq -r 'select(.kind=="incident") | [.ts, .sig, .inc, .payload.rule] | @tsv' state/ledger/*.jsonl | tail
jq -r 'select(.kind=="lifecycle" and .payload.stage=="subsystem_not_built") | .payload.name, .payload.detail' state/ledger/*.jsonl
jq -r 'select(.kind=="event") | .sig' state/ledger/*.jsonl | sort | uniq -c | sort -rn | head
```

Per-record fields worth knowing: `seq` (monotonic), `rec_id`, `sig` (identity),
`inc` (incident id, incidents only), `origin{host_id,hub_id,source}`,
`actor{kind,id,version,git_sha,build_time}`, `redactions` (scrub count),
`payload`.

## Pitfalls recorded from real use

* **Do not trust `status:"degraded"` alone** — it names one degraded sensor
  while whole subsystems may be absent. Grep the ledger for
  `subsystem_not_built` after any boot.
* **Idle hosts still write a lot**: ~778 records / 6 min / 21 sigs observed, with
  repeat incidents per sig. Budget disk for the ledger and check
  `runtime_watermarks.ledger_bytes` before long runs.
* **A stale `stage:"shutdown"` heartbeat** can sit in `state_root` after a
  restart; `check-stall` uses the ledger sequence, so it still returns 0 — do
  not "fix" the heartbeat by hand.
* **Do not edit specs without `selfcheck.py`** and the letter-suffix rule (no
  renumbering). Specs are authority before the wave.
* **Never bind the daemon on a port another trouble instance uses**; run
  throwaway state roots next to the live one, with distinct ports.

* **Never mint a dashboard token while the daemon is down (TRBL-093, P1).** A
  token minted between a stop and the next boot answers 401 forever, even though
  the store looks right. Mint with the daemon running; the token then survives
  restarts. And capture the one-shot plaintext with a strict full-line regex
  (`grep -o 'tdt_[A-Za-z0-9_-]*'`) — a loose grep truncates tokens containing
  `-` and a heredoc passes `$TOK` literally; the plaintext is unrecoverable.
* **A fresh headless boot opens its first-minute breaker by design** (dbus
  session noise → 20+ incidents in seconds → `rule:dbus_unit_failed` open, see
  TRBL-094). Do not treat an open breaker on a brand-new install as a fault;
  look for the real signal under it.
* **`trouble ledger status` IS read-only now — verified with a live writer**
  (TRBL-104 fixed, re-proven 2026-10-20 at d081f79): with the daemon running,
  `status`/`verify` write nothing at all (no `trouble-ledger-cli` records, no
  HEAD/`.idx` rewrite) and `writer_pid` is the LOCK holder, not the CLI's own
  pid. `compact` is the one that still takes the writer seat — it refuses
  `writer-live` while the daemon runs, but it also opens the ledger on the
  refusal path (see the LOCK-residue pitfall below).
* **Status/verify are safe against a LIVE ledger; the runbook's older wording is
  now the truth again** (TRBL-105 fixed; re-proven 2026-10-20 at d081f79): with
  the daemon up, `status`/`verify` walk the files read-only, `compact` refuses
  with `degraded: writer-live` (rc=1, naming the LOCK holder), and the daily
  checks in `docs/operations.md` §2 run against a running daemon. Note the
  status exit code: a *degraded* ledger makes `ledger status` exit 1, so read
  the JSON rather than the exit code when the ledger has holes you injected
  yourself.
* **`payload hash mismatch` is a false positive on the CLI verb — the fix landed but is wired to
  the other verifier** (TRBL-106 / TRBL-106-R2 / TRBL-106-R3, run 2 of 2026-10-20 at `a4362ff`):
  `trouble ledger verify` calls `ledger.ReadVerify`, whose walk still recomputes the hash through the
  decoded-map path (`internal/ledger/readonly.go:297-308`), while the merged fix lives in
  `Ledger.Verify` (`internal/ledger/recover.go:538`, helpers `:617`/`:641`) — so a virgin ledger still
  reports `seq 2: payload hash mismatch`, rc=1, one per boot config record, and the package's own
  regression test passes because it exercises the library, not the verb. Treat it as noise; holes,
  torn/corrupt counts and ts continuity stay trustworthy. To prove the DATA is intact without
  trusting either side: splice the reserved `payload_sha256` member out of the raw stored payload
  bytes and hash what remains — a virgin seq-2 record reproduces its stored digest exactly
  (`e983344ae3793f1f`) while the decoded-map form gives `c8ea23335044558b`, which is what verify
  prints. General rule: when a verdict string exists in more than one walk
  (`grep -rn 'payload hash mismatch' --include='*.go' internal/` → two producers), the fix is only
  proven when you re-drive the USER surface, not the helper's unit test.
* **A torn write glued to the next record (no newline) classifies as one 011
  corrupt line** (TRBL-107): the intact record sharing the line is
  unrecoverable. If verify reports corrupt_lines on a ledger that crashed
  mid-write, inspect the flagged line by hand before writing it off.
* **A REFUSED `compact` is not a no-op — it leaves a LOCK naming its own dead
  pid, and `status` then reports that dead pid as the writer** (TRBL-112,
  2026-10-20): with no daemon up, `ledger status --json` honestly reports
  `writer_pid=-1`; run `ledger compact --day <the ACTIVE day> --dry-run` and it
  refuses (rc=1, `TROUBLE-LEDGER-007 … refusing to compact the file the writer
  is appending to`) *after* creating `<state_root>/ledger/LOCK` with its own
  pid, which stays on disk. The next `status` reports that dead pid
  (`internal/ledger/readonly.go:176` parses LOCK without the kill-0 check its
  sibling `CheckWriterHeld` applies), so a monitor reads "writer alive" on a
  stopped host. The active day is *never* compactable — compact an older day
  (verified: 437 lines → 428 kept + 9 aggregates, 12 ms) — and the refusal's
  code (007 = "compaction failed" per SPEC-TYPES) is not the one ops.md §4
  documents (009). After any refused compact, check the state root and `kill -0`
  the reported pid before believing `writer_pid`.
* **The quickstart's key placeholder trap is FIXED — do not pre-patch the key
  (TRBL-109 fixed, re-proven 2026-10-20 on a fresh bunker agent)**: `deploy/README.md`
  now names the placeholder `examples/config.toml` actually ships and says the
  first-event command reads the key back out of the file, so the two cannot
  disagree. Following it verbatim on a box that had never seen the repo: boot →
  first event **200** in 0.352 s with the shipped key, no edit. Still derive the
  key from the config file itself (`grep public_key`) instead of retyping it —
  that part of the old pitfall stands.
* **The seq allocator seeds from the HEAD hint — that is the SPEC, not a bug
  (TRBL-110 resolved 2026-10-20)**: SPEC-01 §3.4 rule 7 itself defines
  `seq_alloc = max(HEAD.last_seq, highest seq in the highest-generation file) + 1`
  and `docs/operations.md` §3 repeats it, so a high/stale HEAD (restore of an old
  HEAD, crash between refresh and fsync) makes the next Append allocate from the
  hint and **burns the seq numbers in between** — measured at d081f79: file max
  2839 + HEAD hint 9999 → the next records are `10000…10002` and the gap is now
  recorded honestly as `lifecycle{op:"hole",from:2843,to:9999}` +
  `seq_recovered`. It is not data loss, but `verify` will report that hole on
  every pass, so after any restore/repair rewrite HEAD to the file's true max seq
  BEFORE booting to avoid spending the range.
* **A missing `--config` now refuses — it used to fall back to the DEFAULT
  root and write boot notes there** (TRBL-111, run 2; FIXED by f59b30e,
  2026-10-17): a `--config` path that does not exist exits 13 with
  `ledger: config not found: <path>` and creates nothing (verified at HEAD
  43e78ba; the scratch HOME stayed empty). Pre-f59b30e binaries still fall
  back, so on an older build always pass an existing config path. Also:
  `status`/`verify` are read-only since TRBL-104 (7e2a176) and write nothing,
  but `compact` still opens the ledger for write and every open appends one
  `lifecycle{op:"boot"}` record — budget for it when auditing any trail
  `compact` has touched.
* **A closed row does not mean a landed fix — assert the commit is an ancestor
  of `origin/main` before trusting it** (TRBL-113, 2026-10-20): TRBL-106 read
  `complete` with "verified fix on wt/TRBL-106" while `98ba880` was never merged
  (`git merge-base --is-ancestor 98ba880 HEAD` → rc=1; `git branch -r --contains
  98ba880` → `origin/wt/TRBL-106`), so the defect shipped for three more days.
  The same census showed `origin/wt/TRBL-109` unmerged *and* harmless — its fix
  content is on main via `d64b496` — which is why the check must grade the fix's
  content or ancestry, never `git branch --contains`. Before re-proving a closed
  finding, run the ancestry check first; before closing one, put its output in
  the row.
* **The dogfood/QA catalog `docs/qa/use-cases.md` does not exist** (TRBL-114,
  2026-10-20): there are no `UC-`/`REG-` ids anywhere in the repo, so coverage is
  re-derived from prose every tick and no fixed defect has a durable regression
  case. Run history lives in `.coding-hermes/dogfood-log.md` plus the per-run
  reports under `docs/dogfood/` — read the last two entries before choosing a
  surface, otherwise you will re-sweep a covered one.

* **Ingest caps, the gzip guard and the quota ladder: where the contract actually rides**
  (verified live 2026-10-20 at `a4362ff`): every refusal carries the documented code in the
  **`X-Sentry-Error` HEADER** (002 compressed >200KB cause `too_large`; 003 ratio guard cause
  `compression_ratio`; 003 decompressed cap cause `decompressed_cap` at exactly 1,048,577; 019
  generic body invalid; 022 `415` unsupported encoding; 010 on a quota 429) — dump full headers
  (`curl -D -`), never judge an ingest error from the body alone. Testing transport caps through the
  documented generic-JSON on-ramp hits the dialect's OWN field cap first: `message` is capped at
  16KB (`internal/sentinel/genericjson.go:15`), so a 1MiB body answers `400 + 019 +
  message_too_long` even though the transport would have accepted it (TRBL-115). To drive the §3.9
  loss policy, lower `quota_epm` in a scratch config (3 or 5) — the window counts every admitted
  request including your baseline, so reset or count it before calling a threshold drift — and read
  the audit trail afterwards: refusals land as event records with `disposition=dropped_quota` and
  `payload.error_code=TROUBLE-SENTINEL-014`, landed ones as `disposition=admitted`.

## Where the knowledge lives

* `docs/dogfood/2026-09-17-integration.md` — the integration report (what works,
  the four blockers, measured volume, reproduction script).
* `docs/dogfood/diagnostics.md` — how the system is put together and why the
  traps exist.
* `docs/cmd.md`, `docs/operations.md`, `docs/sentinel-compat.md` — operator and
  compat references.


## Latest real-use checkpoint (2026-10-29)

At HEAD `50866ae`, build with `make bin` so the binaries carry the commit stamp. Use an isolated state root, config, environment file, and dashboard token file; passing only `state_root` does not isolate token storage. The documented workflow completed on loopback: health 200 with all five subsystems built, authenticated dashboard reads 200, and three generic JSON event posts 200 with durable event/group/incident records. The cold boot-to-health measurement was 1411ms; dashboard reads were 10–26ms and event posts were 290–430ms.

The 2026-10-29 fresh-bunker attempt is not install proof: `bunker-qa.sh launch` spawned agent `ae53cc5e` but the launcher timed out before its launch cell, agent evidence was empty, and the agent was destroyed and verified absent.
