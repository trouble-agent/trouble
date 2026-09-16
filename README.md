# trouble

`trouble` is an open-source, self-hosted incident brain for a fleet of hosts and apps: sensors,
an embedded Sentry-compatible sentinel, a dedup core, an append-only audit ledger, a ladder that
plays and researches fixes, an issue desk, a flow lane, a dashboard and a skill loop.

This repository is built spec-first: `SPECS-BRIEF.md` and `specs/SPEC-01..12 + SPEC-TYPES +
SPEC-INDEX` are the authority, and code lands against them.

## What exists today

`internal/ledger` — the append-only JSONL audit ledger (SPEC-01): record schema, seq allocation,
group-commit durability, torn-line recovery, daily rotation and part rollover, compaction into
generation files, the bounded in-memory index with its degradation ladder, the query surface and
the storage-tier decision.

`internal/scrub` — the single safety gate between the outside world and every byte trouble persists
(SPEC-02): the compiled-in 18-rule table (13 mandatory, 5 optional), per-target byte budgets, the
shield that keeps DSN public keys verbatim while redacting everything else, per-project overrides,
the bundle/record/envelope entry points, and the persistence-boundary re-scan
(`Verify` / `MandatoryScan`) that `internal/ledger` runs on every serialized line.

`internal/types` is the shared type source of truth.

`internal/registry` — the daemon's entire action surface (SPEC-06): the frozen v1 module SDK
(`Descriptor`/`Check`/`Apply`/`Verify`), the six-stage call contract
(authorize → validate → dry-run → apply → verify → audit) with its intent/outcome audit pair, the
generated draft 2020-12 schemas and their closed validator, plays as data with the shared `when:`
condition language, the compiled-in do-not-touch floor (with additive merge and weakening refusal),
the polkit install-time contract, the `testkit` conformance harness, and the thirteen shipped
modules `config.{get,set,list}`, `service.{status,reload,restart}`, `file.{read,patch}`,
`proc.{top,connections}` and `flow.{file_issue,create_task,comment}`. No module shells out: there is
no `os/exec` anywhere in the package, and a module that accepts a `command`/`argv`/`shell` args key
cannot register.

`internal/ladder` — the state machine that decides what work a detection gets and is the only
component allowed to declare a problem fixed (SPEC-05): the 19 pinned states, the 48-edge transition
table plus 13 refused edges as data, the dedup/merge upsert keyed by `sig` and `inKey` (AC-22),
verification as an `Evidence` tuple where a canary that did not land can never produce `passed`,
budgets, storm breakers and suppression windows, the host agent lease, and park/re-adopt for daemon
restarts. It owns the `incident`, `verify` and `breaker` record kinds.


`internal/dashboard` — the read-mostly face of the ledger (SPEC-10): the §2.1 route table (20 rows
plus one deterministic 404 rule), token/scope auth with CSRF on every POST, the seven htmx polling
fragments with their stale-render guard, server-rendered pages for incidents, groups, rules and
breakers, and the §2.9 budget discipline — no handler opens a ledger file, fragments fit an 8 KB cap,
and compression concurrency is bounded. Import rule: stdlib + `internal/types`, with every subsystem
arriving through `Deps`.

## The scrubbing contract

The pipeline is **ingest → scrub → ledger**, and it is a safety invariant: no subsystem writes to
disk, to the ledger, to the spool, to the skills-local directory, to an issue driver or to a board
row before its content has passed through `internal/scrub`. Every signature, fingerprint and dedup
key is computed over the *scrubbed* bytes, so two hosts and three arrival paths that describe the
same bug produce the same digest.

Two properties carry the design:

* **Idempotence.** `Scrub(Scrub(x)) == Scrub(x)` byte-for-byte, and the second call reports zero
  redactions. That is what lets a satellite scrub locally and the hub re-scrub the same record on
  receipt without changing the signature.
* **Fail closed.** Rule failure, a rule that exceeds `scrub.rule_timeout`, invalid UTF-8 on a text
  target or a persistence-boundary hit means the payload is **not persisted** — in whole or in part
  — and the caller emits a `gap`. Availability loses to leakage, deliberately: the ledger is
  append-only, git-distributed and auto-filed, so a secret that reaches it cannot be removed.

The `trouble` rule table is the authority for what a "secret" is, and its redaction tests are
conformance tests: `internal/scrub/vectors_test.go` (14 positive and 12 negative vectors, exact
bytes and exact `by_rule`), `fixedpoint_test.go`, `shield_test.go`, `config_test.go`,
`conformance_test.go`, `limits_test.go`, `dsn_test.go`, `e2e_ledger_test.go`, `corpus_test.go`,
`bench_test.go` and `bench_ingest_test.go`.

`internal/sensors` — the detection plane (SPEC-03): six sensors (PSI, journald, D-Bus, disk, timers,
inotify), the shared condition language, the rule TOML schema and its atomic hot reload, storm
breakers with per-scope caps, and liveness. Sensors are detection-only: they normalize an
observation, evaluate rules against it, and emit `event`/`gap`/`canary` records through the ledger's
`Append`. They never call the registry, never hold a tool handle and never spawn anything except
`journalctl`.

## The durability contract

A record is durable the moment `Append` returns. Records already written to the ledger file but not yet fsynced are lost on **power loss or kernel panic**; the maximum loss window is `ledger.fsync_window_ms`, default **200 ms**, measured from the last successful fsync completion. A process crash (SIGKILL), a daemon restart or a warm OS reboot loses nothing: those records are already in the running kernel's page cache.

Group commit is not an optimization we may quietly give up: the measured amortized cost is 1.94 µs
per record (515k records/s) against 512 records/s for fsync-per-line on the reference host. That is
why `internal/ledger/durability_test.go` asserts the fsync count is *structural*
(`ceil(records / max_batch_records)`) rather than a timing proxy, and why the test-only per-line mode
exists at all — so the amortized mode's advantage cannot be optimised away silently.

## Ledger file layout

```
<state-root>/ledger/                 0700, files 0600, never /tmp
├── LOCK                             flock(LOCK_EX|LOCK_NB) held for the daemon's life
├── HEAD                             seq hint; tmp+rename+fsync(dir); never a source of truth
├── YYYY-MM-DD.jsonl                 generation 0, part 01
├── YYYY-MM-DD.pNN.jsonl             part NN after an intra-day part rollover
├── YYYY-MM-DD.N.gen.jsonl           generation N produced by compaction; authoritative
├── idmap.jsonl                      satellite local→hub id mapping (same durability)
└── quarantine/                      files refused by the version gate
```

Plain JSONL only — the ledger is the audit artifact and must stay `jq`/`grep`/`tail`-able.
Compression lives in `backups/`, never in `ledger/`.

## What the sensors measure, and what they refuse to assume

The PSI contract is measured, not documented, and every number below was re-measured on the host
that runs the tests (kernel 7.0.0-30-generic, uid in `adm`):

| Fact | Measured |
|---|---|
| only legal trigger write | `"some 150000 2000000\n\x00"` — 21 bytes, terminated with NUL |
| write without the terminator | `EINVAL` (the kernel overwrites the last byte, so a 20-byte write loses its last digit) |
| window grammar | multiples of 2 s ≥ 2 s: 2 s/4 s/6 s/8 s/10 s accepted, 1 s/5 s/12 s/20 s refused |
| second write on an armed fd | `EBUSY` |
| notification rate limit | 1 per window (wakeups observed at 1.95 s and 4.00 s) |
| an **unarmed** fd polled with a zero timeout | 338,711 hits in 200 ms — a 100 % CPU busy loop, which is why an unarmed fd is never placed in an epoll set |
| window ceiling on this kernel | 10 s (the spec's "20 s is accepted" note does not hold here) |

Because the ceiling and the privilege answer differ per host, the daemon **probes** them at boot
(`trouble sensors probe`): one real arm, a downward ceiling sweep `20s → 10s → 2s`, and a `stall=0`
probe that must be refused. The result selects `triggers+sampling` or `sampling-only`, is written to
the ledger as one `capability_probe` record, and is repeated verbatim in `/health.json`. Trigger
arming is never attempted speculatively again after the probe.

Two more rules the code enforces rather than documents: a journal seek failure is never "no logs"
(`Failed to seek to cursor: Invalid argument` → `TROUBLE-SENSORS-007` + a `--since` fallback + one
gap record), and this class of host keeps its application units in **user** managers — watching only
the system manager is an ALL-GREEN lie, so the user manager is resolved and watched too.

## Read the specs

* `specs/SPEC-INDEX.md` — suite map, the AC-to-spec matrix and the frozen v0.1 cut line.
* `specs/SPEC-01-ledger.md` — the ledger, in full.
* `specs/SPEC-03-sensors.md` — the detection plane, in full.
* `specs/SPEC-TYPES.md` — every shared type and the canonical error-code catalog.

## Build and test

```
go build ./...
go test -race -count=1 ./internal/...      # the CI gate for this package
go test -race -count=1 -short ./internal/...  # skips the large synthetic-fixture budgets
```

MIT licensed.
