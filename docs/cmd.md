# The composition root: `internal/app`, `cmd/troubled`, `cmd/trouble`

This document describes the two shipped binaries and the adapter layer between
them and the subsystem packages. It is the operator-facing complement to
SPEC-12 §2 (the CLI surface) and §4 (boot/drain), and it is the only place that
describes how the packages meet.

## The three layers

```
cmd/troubled   the daemon: boot sequence, loops, drain        (SPEC-12 §4.1/§4.2)
cmd/trouble    the operator CLI: install, explain, checks     (SPEC-12 §2.1)
internal/app   adapters: ledger↔ladder, ladder↔registry, sensors↔ladder
```

Every subsystem package imports `internal/types` and nothing else from its
siblings. `internal/app` is where two contracts that were designed separately are
joined, and `cmd/*` is where the daemon's own lifetime lives. That is the whole
reason the graph stays acyclic: `internal/ledger` never imports `internal/ladder`
(the `Actor` triple is injected), `internal/ladder` never imports
`internal/registry` (the `PlayRunner` view is an interface), and neither of them
knows that `internal/dashboard` exists.

## The daemon's argv

`cmd/troubled` parses four argv forms for itself and forwards everything else to
`lifecycle.Resolve`, whose registry is the only whitelist that reads a key flag
(SPEC-12 §2.5, §2.5a):

```
troubled [--config <path>] [-v] [--version] [--<key> <value>]...
```

| argv | Meaning |
|---|---|
| `--config <path>` / `--config=<path>` | the config file resolution reads |
| `--config_path <path>` | the same selection in the mechanical §2.5 spelling; it also stays a resolved row, so `trouble config explain` shows a flag chose the path |
| `-v` | debug logging |
| `--version` | the version triple, exit 0 |
| `-h` / `--help` | the daemon's real surface, exit 0 |
| `--<key> <value>`, `--<key>=<value>`, bare `--<key>` | any registered key: `a.b_c` → `--a-b-c` (`state_root` → `--state_root`, `secrets.environment_file` → `--secrets-environment_file`). A flag beats env, file and default. |

A scratch or second instance therefore needs no edit to the shipped file:

```
troubled --state_root ~/.local/state/trouble-scratch \
         --ingest-bind 127.0.0.1:7645 --dashboard-bind 127.0.0.1:7646
```

Three behaviours are load-bearing:

1. **An unknown key is refused by name.** `--no-such-key` reaches the resolver,
   which exits TROUBLE-LIFECYCLE-001 `unknown flag "--no-such-key"`; the daemon
   maps a boot refusal to exit 13. It is never ignored and never read as a
   positional; a one-dash token that is not `-v`/`-h` is a usage error (exit 2).
2. **Seven keys cannot be set from argv.** `projects`, `issues`, `skills` and
   `llm` are tables and a flag value is a scalar (refused by name, 001).
   `dashboard.token_file`, `hub.token` and `server.redis.password_env` are
   refused by the argv secret scan at boot
   (`cli_flag_secret`, TROUBLE-LIFECYCLE-013): their flag NAMES match a mandatory
   rule, so the value that follows is read as a secret no matter what it holds —
   a token-store path, a real token and the NAME of an environment variable are
   the same shape to that rule. Set them from the file or from
   `TROUBLE_DASHBOARD_TOKEN_FILE` / `TROUBLE_HUB_TOKEN` /
   `TROUBLE_SERVER_REDIS_PASSWORD_ENV`, which is what the shipped unit's
   `EnvironmentFile=` is for.
3. **The daemon's own surface prints its real shape.** `--help` lists its four
   argv forms plus the per-key form and points at `trouble config explain`; it
   never prints Go's flag-package automessage, which could only name the daemon's
   own flags and so contradicted the documented surface (TRBL-018).

`cmd/troubled/main_test.go` pins all three against the registry itself: it
enumerates the keys from a default resolve and drives each one's flag spelling
through the same split function the daemon uses.

## What each adapter is for

| Adapter | Joins | Why it exists |
|---|---|---|
| `app.Store` | `ledger.Ledger` → `ladder.LedgerWriter`, `ladder.IndexReader` | SPEC-01 owns the only writer and the bounded index; SPEC-05 asks for six read methods and one write method. |
| `app.PlayEngine` | `registry.Registry` → `ladder.PlayRunner` | SPEC-06 owns the six-stage call contract and the play runner; SPEC-05's ladder calls `Run/Check/Apply/Rollback` and nothing else. |
| `app.Evaluator` | `sensors` condition language → `ladder.RuleEvaluator` | SPEC-INDEX §4.2 fixes one dialect: whoever evaluates a rule's `match` table must use `internal/sensors`' compilers, never a second parser. |
| `app.Notifier` | ladder escalation → ledger record + journal | The escalation *channels* belong to SPEC-12; the in-daemon half is a record and a WARN line so the incident story shows it. |
| `app.Clock` | `time` → `ladder.Clock` | The ladder's windows and cooldowns are monotonic (SPEC-INDEX §6.5). |

### Two honest limits

1. **`OpenIncidentByInKey`.** SPEC-01's index keys incidents by signature, not by
   the inKey that SPEC-05 folds arrival paths on. The adapter therefore resolves
   the key from (a) a live map it maintains as it writes incident records and (b)
   a bounded fallback that reads the head of each open incident's timeline from
   the index's recent-record ring. Both are memory reads; neither walks a ledger
   file. A dedup fold for a record older than the ring window falls back to the
   signature path, which is the conservative direction (a new incident, not a
   silently merged one).
2. **`Flush` is a no-op.** `ledger.Append` does not return until the record's
   batch has been written and fsynced (SPEC-01 §3.5 group commit), so by the time
   the ladder calls `Flush` there is nothing buffered. The method is declared
   rather than omitted so that a dropped flush cannot look like a successful one.

## Daemon boot order

`cmd/troubled` performs SPEC-12 §4.1's sequence and refuses to serve on any
failure:

```
config resolve (flag > env > file > default, TROUBLE-LIFECYCLE-001/002)
→ state root + modes (004/005) → secret-file modes + /proc/self/cmdline scan (013)
→ schema_version compatibility (012) → ledger open + index rebuild (008)
→ config record → bind preflight, listeners held (003) → sensors start
→ checker.alarm mirrored into the ledger → sd_notify READY=1 → serve
```

The drain path (SIGTERM) is the mirror image: `STOPPING=1`, stop accepting, park
in-flight plays through the ladder, flush the ledger and the spool, write a final
heartbeat with `stage="shutdown"`, close the listeners, exit 0 within
`lifecycle.drain_timeout`.

## Operator CLI

```
trouble --version                       version git_sha build_time (and whether the build is stamped)
trouble config explain [--key K] [--json]   every resolved key with its winning source
trouble topology [--json]               the T1..T5 decisions for this configuration
trouble check-stall [--json]            the external stall checker: exit 0 / 8 / 9
trouble install [--check] [--dry-run] [--root DIR] [--force]
trouble upgrade [--to PATH|VERSION] [--rollback] [--wait DURATION]
trouble escalate --unit NAME            invoked by trouble-escalate@.service only
trouble dashboard token create|rotate|revoke|list
```

Exit codes are contract: `0` ok · `8` liveness surface stale/unreadable
(TROUBLE-LIFECYCLE-008) · `9` ledger-sequence stall (TROUBLE-LIFECYCLE-009) ·
`13` a refusable condition (config, bind, state root, units).

The dashboard token plaintext is printed exactly once, by
`trouble dashboard token create` / `rotate`, on stdout; it is unrecoverable
afterwards because only `sha256(token)[:32]` is stored (SPEC-10 §3.2). There is no
token-management HTTP route in v0.1, by design.

## Building

```
make build     # compile everything
make bin       # the two binaries, version-stamped (SPEC-12 §3.4 ldflags) into ./bin
make test      # the full suite
make check     # the suite's self-consistency loop + schema + vet + internal tests
make smoke     # version, config explain, topology against the real binaries
make smoke-e2e # the live operator smoke: 24 assertions against bin/trouble + bin/troubled
make ac-matrix # every AC in the SPEC-INDEX matrix against the evidence in the tree
```

An unstamped build is visibly degraded rather than silently fine: `/health.json`
reports `status="degraded"` with `detail.reason="unstamped_build"`, and
`trouble install` refuses to enable the unit unless `--force` — which
`tests/e2e/cli_smoke.sh` proves by building an unstamped binary and watching both
answers.
