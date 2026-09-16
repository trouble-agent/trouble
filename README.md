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

## Read the specs

* `specs/SPEC-INDEX.md` — suite map, the AC-to-spec matrix and the frozen v0.1 cut line.
* `specs/SPEC-01-ledger.md` — this package, in full.
* `specs/SPEC-TYPES.md` — every shared type and the canonical error-code catalog.

## Build and test

```
go build ./...
go test -count=1 ./internal/...              # the CI gate
go test -count=1 -short ./internal/...        # skips the large fixtures and the 60s load test
go test -count=1 -run TestLoadIngestThroughput ./internal/sentinel/   # SPEC-04 §7's load test
```

The load test saturates the host for its duration (60s, 5s under `-short`), so
run the whole tree with `-p 1` (or `-short`) when other timing-sensitive packages
are in the same run: it can otherwise push `internal/ledger`'s fsync-window
assertions over their bounds on a shared machine. Measured numbers and the reason
the §7 latency budget is asserted at a host factor are in `docs/operations.md`
§9.

MIT licensed.
