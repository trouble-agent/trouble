---
name: trouble-usage
description: >-
  How to actually run and drive the trouble daemon (incident brain): build the
  binaries, boot a working config, mint and use a dashboard token, read the
  ledger, and the four traps that stop a fresh user cold (example config does
  not parse, `~` token path 401s, no project = no ingest listener, no
  quickstart). Load this before touching ~/trouble.
version: 1.0.0
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
`docs/dogfood/diagnostics.md` for why the traps exist.

## Entry points

| surface | how | notes |
|---|---|---|
| daemon | `bin/troubled --config <file>` | boots, serves dashboard (+ ingest when projects are configured) |
| operator CLI | `bin/trouble …` | `--version`, `config explain`, `topology`, `check-stall`, `install`, `upgrade`, `escalate`, `dashboard token …` |
| dashboard | `http://127.0.0.1:7644` | token auth; `/health.json` is loopback-exempt |
| ingest | `http://127.0.0.1:7643/api/{project_id}/event/` | **only binds if projects are configured — see trap 3** |
| ledger | `<state_root>/ledger/YYYY-MM-DD.jsonl` | plain JSONL, `jq`-able, `seq`/`rec_id`/`kind`/`sig`/`inc`/`payload` |
| specs | `specs/SPEC-INDEX.md` + `SPEC-01..13` | authority; `python3 specs/tools/selfcheck.py` must pass before spec commits |

Build: `make bin` (stamped), `make build`, `make check`, `make smoke-e2e`.
Module path is frozen (`github.com/totalwindupflightsystems/trouble`) — never
rename it (see the project prompt in the scheduler).

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
token_file = "~/.config/trouble/dashboard-tokens.json"
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

### Trap 5 — `degraded` means "a sensor", `ok` does not mean "complete"

The rules directory is `<state_root>/../config/rules.d/`, installed only by
`trouble install`. Boot the daemon by hand without it and the inotify sensor
reports `TROUBLE-SENSORS-025` and `/health.json` says `degraded` — install the
rules (`cp examples/rules/*.toml <state_root>/../config/rules.d/`) and the same
config reports `status:"ok"` **even though sentinel/issues/skills were refused
in the same boot**. The only surface that tells the whole truth is the ledger:

```bash
jq -r 'select(.kind=="lifecycle" and .payload.stage=="subsystem_not_built") | .payload.name, .payload.detail' state_root/ledger/*.jsonl
```

Treat that query as part of "is the daemon healthy", not as a debugging step.

## Working config (throwaway, verified)

```toml
state_root = "~/dogfood-trouble/state"        # NOT under /tmp: TROUBLE-LIFECYCLE-004 refuses it
config_path = "~/dogfood-trouble/config.toml"
[secrets]
environment_file = "~/dogfood-trouble/trouble.env"   # mode 0600, or boot refuses
[stall]
max_seq_age = "300s"
[checker]
interval = "60s"
confirm_runs = 2
[ingest]
bind = "127.0.0.1:7643"
[dashboard]
bind = "127.0.0.1:7644"
token_file = "~/.config/trouble/dashboard-tokens.json"
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

## Where the knowledge lives

* `docs/dogfood/2026-09-17-integration.md` — the integration report (what works,
  the four blockers, measured volume, reproduction script).
* `docs/dogfood/diagnostics.md` — how the system is put together and why the
  traps exist.
* `docs/cmd.md`, `docs/operations.md`, `docs/sentinel-compat.md` — operator and
  compat references.
