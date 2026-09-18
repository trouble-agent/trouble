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

`internal/sentinel` — the code plane (SPEC-04): the Sentry-SDK-compatible
ingestion listener (envelope + legacy `/store/`), the generic-JSON on-ramp
(`POST /api/{id}/event/`, one curl from any runtime), the log collectors
(`go-panic`, `py-traceback`, `node-reject` over journald units and file tails),
the group index with its rebuild-from-ledger path, release ordering and
regression detection, quotas with the official rate-limit header format, the
three loss policies, the spool, and the per-project canary that makes "no events"
distinguishable from "no observation".

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

`internal/issues` — the issue desk (SPEC-09): the frozen four-method driver contract
(`Name`/`Healthcheck`/`EnsureBySig`/`Comment`/`Close`), the anchor index keyed by
`(driver, sig, project)` — one issue per sig for the life of the state root, with a fold inside the
dedup window and a full recurrence block outside it — the sig/project/global caps that stop a
chatty app or a flappy rule from spraying, the bounded spool (64 MiB / 20 000 entries / 72 h TTL)
with its drop-oldest floor and its replay order, quiet-close with the five gates and the
reopen-or-supersede branch, and two shipped drivers: `github` (search-first create so a lost 201
adopts instead of duplicating, read-back verification, the exact §3.9.4 markers, the §3.9.5
status→code table, a 0600 token file and argv refusal) and `duckbrain` (a KV layout with one key per
comment and a write-then-read-back contract). It owns the `issue` record kind and is one of the five
`gap` emitters. No module here shells out and no page is ever read: two HTTP APIs, one classified
error per call.

`internal/skills` — the skill loop (SPEC-11): a strict artifact schema with no field in which
arbitrary code could be written (an unknown key, a `[stats]` table or an `exec`/`shell` key is a
refusal, not a warning), a canonical signed byte form (`trouble.skill.v1` + a deterministic JSON
projection + the play payload digest) verified against a config-listed ed25519 signer set, the
version/floor/module/canary/approve gate chain, a pull-only distribution path (argv-only git reads:
no push, no commit, no gc), the local candidate loop (draft → review → promote, signed with the
host's own key), and the resolver that answers a recurrence with an exact sig match and refuses
ambiguity. It owns the `skill` record kind.



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

## The ingestion contract

Four properties are design constraints in `internal/sentinel`, not features:

* **SDK wire compatibility is a contract.** `docs/sentinel-compat.md` is the
  matrix — routes, item types, encodings, size caps, auth forms, the 16-hex
  secret divergence and every place the shipped code cannot follow the SPEC-04
  text literally. It is rendered from the same tables the server uses
  (`TestCompatMatrix` fails if the two drift).
* **A quota breach destroys evidence**, because official SDK behaviour on 429 is
  *discard*. So every drop writes a ledger record with its sig and disposition,
  the loss policy is a configuration decision with three pinned behaviours, and
  verification for a sig that lost its own events in the window is `invalid`,
  never `passed`.
* **Sentinel never observes itself into a green lie.** A per-project canary is
  injected through the real HTTP path every `canary_interval`; a canary that does
  not land makes verification `invalid` and emits `gap{cause=canary_missing}`.
  `client_report` items are parsed, so SDK-side drops are data rather than
  silence.
* **The ingestion listener is an abuse surface.** Size caps, a gzip-bomb guard
  (limited reader + a 100:1 ratio guard), a concurrency cap, per-IP and
  per-project rates, read/write timeouts, an X-Forwarded-For trust policy and a
  bind-matrix auth rule are all pinned and tested — a DSN public key is a public
  key.

Sentinel emits exactly four record kinds — `event`, `group`, `gap`, `canary` —
and mints no cross-plane identity: it computes a `Sig` and hands it to the
ledger, the dedup core and the ladder, which own incident identity.

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

## The research rung and the flow subsystem

`internal/research` (SPEC-07) is the Off-by-One rung: class-slug derivation from a
signature plus its facts, discover → corpus grep → submit → poll, the agent prompt
with and without a brief, and the degrade matrix that guarantees the ladder is
never held. `internal/flow` (SPEC-08) files one board row per `(sig, board)` —
append-only, never a rewrite — proves the target project is ticked by the
scheduler before writing a byte, and requests a foreman spawn inside a git
worktree through the scheduler's own admission path, with a durable queue behind
it. Both are configuration-only clients: no lab layout, board path, repo list or
model pin is compiled in.

Operational detail (what to look at when one of them misbehaves, and the runbook
facts that surprise people) lives in `docs/operations.md` §12 and §13.

## Run it: the on-ramp, end to end

`POST /api/{id}/event/` is trouble's own on-ramp (SPEC-04 §3.6) — one `curl` from any runtime, no SDK, no
agent. The shipped example already declares the one project the ingest plane needs, so a stock boot serves it.

```
cp examples/config.toml ~/.config/trouble/config.toml
$EDITOR ~/.config/trouble/config.toml      # state_root, ingest.advertised_host, ingest.bind, dashboard.bind
make bin
bin/trouble install                        # or run it in the foreground: bin/troubled --config ~/.config/trouble/config.toml
```

The declaration is a `[[projects]]` table (SPEC-12 §3.1a); `id` and `public_key` are enough:

```toml
[[projects]]
id = "1"                                         # numeric; the DSN path element
slug = "trouble-dev"
public_key = "0123456789abcdef0123456789abcdef"  # 32 lowercase hex — replace it
```

With `ingest.auth.loopback_dsn = true` (the default) a loopback request authenticates with that public key
alone, which makes the query form the documented one for `/api/{id}/event/`:

```
curl -sS -X POST 'http://127.0.0.1:7643/api/1/event/?sentry_key=0123456789abcdef0123456789abcdef' \
  -H 'Content-Type: application/json' \
  -d '{"message":"hello from the on-ramp","level":"error","release":"0.1.0"}'
{"id":"8a0f0f6c…"}
```

One accepted event writes two records — the `event` and the fingerprint `group` — into
`<state_root>/ledger/YYYY-MM-DD.jsonl`:

```
jq -r 'select(.kind=="event" or .kind=="group") | "\(.kind) \(.sig) \(.payload.op // "")"' \
  ~/.local/state/trouble/ledger/*.jsonl
```

`/health.json` then reports the subsystem block (SPEC-12 §3.3a):

```
curl -sS localhost:7644/health.json | jq '.status, .subsystems'
"ok"
[{"name":"sentinel","built":true,"refused":false},                # ← the ingest plane is up
 {"name":"issues","built":true,"refused":false},                  # ← OFF, built and idle (SPEC-09 §3.4a)
 {"name":"research","built":true,"refused":false},
 {"name":"flow","built":true,"refused":false},
 {"name":"skills","built":true,"refused":false}]                  # ← OFF, built and idle (SPEC-11 §2a)
```

`status` is `degraded`, never `ok`, while any subsystem is refused: the block names which one, its code
and its reason, and `detail.subsystem_refused` carries the code for an alarm line. The two optional
subsystems ship **OFF** — the issue desk because nothing is compiled in for it to file into (no
`owner`/`repo`, no token) and the skill loop because no distribution channel is compiled in — so a stock
boot BUILDS both and reads `ok`, instead of refusing two subsystems the operator never configured
(`SPEC-09 §3.4a`, `SPEC-11 §2a`). `examples/config.toml` states both opt-outs and names the keys that turn
each one on. Declaring no project at all is also a valid config: the ingest port stays closed, the
refusal is recorded, and `/health.json` says so instead of pretending.

## Read the specs

* `specs/SPEC-INDEX.md` — suite map, the AC-to-spec matrix and the frozen v0.1 cut line.
* `specs/SPEC-01-ledger.md` — the ledger, in full.
* `specs/SPEC-03-sensors.md` — the detection plane, in full.
* `specs/SPEC-09-issues.md` — the issue desk, in full.
* `specs/SPEC-11-skills.md` — the skill loop, in full.
* `specs/SPEC-TYPES.md` — every shared type and the canonical error-code catalog.

## Build and test

```
go build ./...
go test -count=1 ./internal/...              # the CI gate
go test -count=1 -short ./internal/...        # skips the large fixtures and the 60s load test
go test -count=1 -run TestLoadIngestThroughput ./internal/sentinel/   # SPEC-04 §7's load test
```

`-short` keeps the whole tree safe to run in parallel (the load test degrades to a
correctness smoke: one request in flight per worker and no throughput/latency/RSS
assertions). The full 60s load test saturates the host for its duration, so run
the tree with `-p 1` when it is in the same run — otherwise it can push
`internal/ledger`'s fsync-window and `internal/scrub`'s µs/KiB assertions over
their host-measured bounds. Measured numbers, and the reason the §7 latency budget
is asserted at a host factor, are in `docs/operations.md` §9.

MIT licensed.
