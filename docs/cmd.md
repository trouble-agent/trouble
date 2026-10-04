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
knows that `internal/dashboard` exists. The agent stage's LLM client,
`internal/llm`, is documented in [docs/llm.md](llm.md).

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
| `--<key> <value>`, `--<key>=<value>`, bare `--<key>` | any registered key: `a.b_c` → `--a-b_c` (`state_root` → `--state_root`, `secrets.environment_file` → `--secrets-environment_file`). A flag beats env, file and default. |

A scratch or second instance therefore needs no edit to the shipped file:

```
troubled --state_root ~/.local/state/trouble-scratch \
         --ingest-bind 127.0.0.1:7645 --dashboard-bind 127.0.0.1:7646
```

Four behaviours are load-bearing:

1. **An unknown key is refused by name.** `--no-such-key` reaches the resolver,
   which exits TROUBLE-LIFECYCLE-001 `unknown flag "--no-such-key"`; the daemon
   maps a boot refusal to exit 13. It is never ignored and never read as a
   positional; a one-dash token that is not `-v`/`-h` is a usage error (exit 2).
2. **Six keys cannot be set from argv.** `projects`, `issues`, `skills` and
   `llm` are tables and a flag value is a scalar (refused by name, 001).
   `server.redis.password_env` is refused by the argv secret scan at boot
   (`cli_flag_secret`, TROUBLE-LIFECYCLE-013): its value is the NAME of an
   environment variable, and a name is not path-shaped, so the rule keeps
   reading it as a secret. Set it from the file or from
   `TROUBLE_SERVER_REDIS_PASSWORD_ENV`, which is what the shipped unit's
   `EnvironmentFile=` is for.
3. **The two token-STORE keys do ride argv — with a path.** `dashboard.token_file`
   and `hub.token` are addressable, because the mandatory `cli_flag_secret` rule
   leaves an explicitly path-shaped VALUE alone (SPEC-02 §3.3 rule 9): a value
   that starts with `/`, `~/`, `./` or `../` and continues in the path alphabet
   (an absolute value also needs a separator or an extension after the slash —
   `/srv/tokens.json`, `/tokens.json`) is a store locator, not a secret, and it
   resolves with `source=flag`:

```
troubled --state_root ~/.local/state/trouble-scratch \
         --dashboard-token_file /home/user/.config/trouble/dashboard-tokens.json \
         --hub-token /srv/trouble/hub.token
```

   Anything that is not an explicit path is still refused with 013, exactly as
   before: a pasted token (`--hub-token abcDEF…`), a bare file name
   (`--token_file tokens.json`), a slash-prefixed word with no path structure
   (`--token_file /tokens`) and a base64 run (`--token_file /9j4K+fg==`). The two
   spellings differ for a name that ENDS at the sensitive word: `--dashboard-token_file=<path>`
   is the rule-9 flag form and resolves, while `--hub-token=<path>` is
   `NAME=value` first (rule 7 `env_assign`) and stays refused — the assignment
   form has no path exemption in any context, because an environment or config
   dump is written in the same shape. `hub.token` therefore rides argv in the
   space spelling, the file or the environment.
4. **The daemon's own surface prints its real shape.** `--help` lists its four
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
trouble check-stall [--health-url URL] [--state-root DIR] [--json]   the external stall checker: exit 0 / 8 / 9
trouble install [--check] [--dry-run] [--root DIR] [--force]
trouble upgrade [--to PATH] [--rollback] [--wait DURATION]
trouble escalate --unit NAME            invoked by trouble-escalate@.service only
trouble dashboard token create --label LABEL --scopes read[,write][,autonomy]
                               [--output-env ENVFILE]
trouble dashboard token rotate --label LABEL [--output-env ENVFILE]
trouble dashboard token revoke --label LABEL
trouble dashboard token list [--json]
trouble hub status [--json]             the light-hub profile: stream, dedup and archive state
trouble hub archive [--dry-run] [--file FILE] [--force]   export closed generations to DuckBrain
trouble hub dedup --key KEY [--json]    probe the dedup gate for one idempotency key
trouble hub drain [--timeout DURATION]  consume the stream into the ledger, then stop
```

`trouble upgrade --to` names the PATH of the binary to stage, which is copied over
the running one: a version string is not resolved to an artifact, so `--to v0.1.1`
stages a path that does not exist. `--rollback` ignores `--to` and restores the
newest `backups/bin/` generation instead; one of the two is required.

Exit codes are contract: `0` ok · `8` liveness surface stale/unreadable
(TROUBLE-LIFECYCLE-008) · `9` ledger-sequence stall (TROUBLE-LIFECYCLE-009) ·
`13` a refusable condition (config, bind, state root, units).

`check-stall` carries two overrides for the case where the config is not the
thing to change: `--health-url URL` overrides the health URL (default:
`dashboard.bind` + `/health.json`) and `--state-root DIR` overrides the state
root — the pair a container `HEALTHCHECK` and a second-instance probe need.
`--json` prints the machine-readable verdict.

The dashboard token plaintext is printed exactly once, by
`trouble dashboard token create` / `rotate`, on stdout; it is unrecoverable
afterwards because only `sha256(token)[:32]` is stored (SPEC-10 §3.2). There is no
token-management HTTP route in v0.1, by design.

### The `trouble hub` verbs

The four `hub` verbs are SPEC-13 §2.2's operator surface for the light-hub
profile. The CLI is a separate process from the daemon: every verb re-resolves
config the way every other verb does and then reads Redis + the state root
directly, so each verb carries its own exit codes rather than the suite-wide
ones above.

| Verb | Meaning | Exit codes |
|---|---|---|
| `trouble hub status [--json]` | the `HubStatus`: hub profile, Redis stream length/lag/pending, dedup hit/miss counters, archive queue depth and last verified export. Reads Redis + the state root; writes nothing | `0` ok, `1` degraded |
| `trouble hub archive [--dry-run] [--file FILE] [--force]` | the archival job for one generation (default: every closed generation without an `exported` marker); `--dry-run` prints the `ArchivePlan` (file, bytes, gzip bytes, marker id, target namespace) and writes nothing | `0` ok, `1` failed, `2` refused |
| `trouble hub dedup --key KEY [--json]` | read-only probe of the dedup gate for one idempotency key: present/absent plus TTL. No mutation, and nothing but presence is revealed | `0` present, `1` absent, `2` Redis unreachable |
| `trouble hub drain [--timeout DURATION]` | consume-and-ack the stream to empty (or the timeout), then stop — the pre-migration drain of SPEC-12 §3.6 | `0` drained, `1` timeout with pending entries |

`drain` is the only mutating verb: it moves entries from Redis into the ledger
and never deletes an un-acked entry. `status` and `dedup` write nothing, and
`archive` writes only when it is not a `--dry-run`. Beyond the §2.2 rows, every
hub verb keeps the suite-wide codes: `2` for a usage error (an unknown verb, a
missing `--key`) and `13` when a refusable condition stops it (a config or
profile refusal, or `drain` finding the ledger already held by a running
daemon).
`--output-env ENVFILE` is the other half of the same mint, and the path a
shell-less image has to take (TRBL-002: no shell, no editor, no `cp`). `create`
and `rotate` write `TROUBLE_DASHBOARD_TOKEN=<plaintext>` into ENVFILE — the line
the operator would otherwise place by hand in the daemon's
`secrets.environment_file` — while still printing the plaintext on stdout;
`revoke` and `list` parse the flag and ignore it. The write is atomic and 0600 by
construction (temp file in ENVFILE's own directory, chmod 0600, rename), so
ENVFILE is created 0600 when it is absent, an existing ENVFILE must already be a
regular file whose mode is exactly 0600 (`env file <path> mode is 0644, want
0600`, exit 13), its other lines are preserved, and an existing
`TROUBLE_DASHBOARD_TOKEN=` line is replaced in place rather than appended. The
refusal comes after the token itself is stored, so a 0644 ENVFILE costs a
rotation, not a lost token. Omitting the flag (the default) keeps the v0.1
contract: stdout only, the operator places the line.

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
