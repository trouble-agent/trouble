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
the storage-tier decision. `internal/types` is the shared type source of truth; `internal/scrub`
carries the persistence-boundary re-scan entry point (the full scrubbing engine is SPEC-02).

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
go test -race -count=1 ./internal/...      # the CI gate for this package
go test -race -count=1 -short ./internal/...  # skips the large synthetic-fixture budgets
```

MIT licensed.
