# Changelog

All notable changes to this project are documented in this file. The format is
based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project intends to follow semantic versioning once release tags exist.

Note on versions: no release tag is cut yet. A build from this tree is
*designed* to report the unstamped triple `0.0.0-dev` (`SPEC-12` §3.4), so
`trouble --version` printing `0.0.0-dev` means "built from a tagless tree", not
"broken build".

## [Unreleased]

### Added

- **The spec set is the contract**: `SPECS-BRIEF.md`,
  `SPECS-BRIEF-v0.1.1.md`, `specs/SPEC-01..13`, `SPEC-TYPES`, `SPEC-INDEX`
  (AC matrix) plus `specs/tools/selfcheck.py`, which fails when the matrix and
  the spec metadata drift apart.
- **`internal/ledger`** — the append-only JSONL audit ledger (SPEC-01): record
  schema, seq allocation, group-commit durability, torn-line recovery, daily
  rotation and part rollover, compaction into generation files, the bounded
  in-memory index with its degradation ladder, the query surface, and the
  storage-tier decision.
- **`internal/scrub`** — the single safety gate between the outside world and
  every persisted byte (SPEC-02): the compiled-in 18-rule table (13 mandatory,
  5 optional), per-target byte budgets, the DSN public-key shield,
  per-project overrides, the bundle/record/envelope entry points, and the
  persistence-boundary re-scan (`Verify` / `MandatoryScan`) the ledger runs on
  every serialized line.
- **`internal/sentinel`** — the code plane (SPEC-04): Sentry-SDK-compatible
  ingestion (envelope + legacy `/store/`), the generic-JSON on-ramp, the log
  collectors (`go-panic`, `py-traceback`, `node-reject`), the group index with
  rebuild-from-ledger, release ordering and regression detection, quotas with
  the official rate-limit header format, the three loss policies, the spool,
  and the per-project canary.
- **`internal/sensors`** — host and app sensors with normalize/diff semantics
  and the explicit refuse-to-assume rules (SPEC-03).
- **`internal/registry`** — the daemon's action surface (SPEC-06): the frozen
  v1 module SDK, the six-stage call contract
  (authorize → validate → dry-run → apply → verify → audit) with its
  intent/outcome audit pair, generated draft 2020-12 schemas and their closed
  validator, plays as data with the shared `when:` language, the compiled-in
  do-not-touch floor, the polkit install-time contract, the `testkit`
  conformance harness, and the shipped `config.*` / `service.*` / `file.*`
  modules.
- **`internal/ladder`, `internal/llm`, `internal/research`, `internal/skills`**
  — the play ladder with its research rung and budgeted agent stage
  (SPEC-05/07/11), including the non-streaming LLM client with a
  config-ordered fallback chain and a capped context-compaction hook.
- **`internal/flow`, `internal/issues`** — the flow lane (SPEC-08) and the
  issue desk with its pluggable drivers (SPEC-09).
- **`internal/hub`** — the light-hub profile (SPEC-13): Redis queue, drain,
  archive planning and dedup, with the documented degradation path when Redis
  is absent.
- **`internal/dashboard`** — the operator dashboard (SPEC-10) with the token /
  loopback / tailnet / proxy-header identity seam, CSRF handling and the
  bind-matrix refusals.
- **`internal/lifecycle`** — install, upgrade, park/resume and host checks
  (SPEC-12), with systemd unit templates, the cross-version upgrade path and
  mode checks that refuse a token file wider than `0600`.
- **CLI**: `cmd/trouble` (operator surface: `install`, `upgrade`,
  `config explain`, `topology`, `check-stall`, `escalate`, `dashboard token`,
  `hub status|archive|dedup|drain`, `--version`) and `cmd/troubled` (the
  daemon).
- **Ledger CLI verbs (TRBL-100)**: `trouble ledger status|verify|compact`
  wire the SPEC-01 §2.4 operator surface — the library machinery existed but
  the verbs answered `unknown command "ledger"`. Each verb takes a per-verb
  FlagSet with a `--json` machine form plus a human stanza, follows the
  documented exit-code contract (ok / degraded / findings / refusable
  section-5 codes such as `TROUBLE-LEDGER-007` no files and
  `TROUBLE-LEDGER-009` retention blocked), and `compact` supports `--dry-run`
  reporting the generation file and byte deltas without writing anything.
- **Packaging and deployment**: the `make release` multi-platform matrix
  (linux/amd64, linux/arm64, darwin/arm64, windows/amd64) with a stamped
  `dist/manifest.json`, a multi-stage `Dockerfile` (static, CGO disabled), a
  compose stack with a light-hub profile, and the `examples/` config set.
- **Docs**: the README contracts, `docs/operations.md` runbook,
  `docs/sentinel-compat.md`, `docs/cmd.md`, and the shipped usage skills under
  `skills/`.

### Changed

- **A missing dashboard token store refuses the data plane (QA-TROUBLE-19)**:
  a boot whose `dashboard.token_file` is missing no longer serves the §2.1
  surface behind a WARN. `/health.json` stays up with `status:"degraded"` and
  a refused `dashboard` row (`TROUBLE-LIFECYCLE-001`) in `subsystems[]`, the
  ledger records `subsystem_not_built` for the dashboard, and every other
  route answers `503 + TROUBLE-DASHBOARD-013 (detail:dashboard_refused)`.
  Nothing is regenerated; the remedy is `trouble dashboard token create` plus
  a restart. SPEC-12 §3.3a-d.

### Fixed

- **Ledger `status`/`verify` are read-only (TRBL-104, TRBL-105)**: the verbs
  previously opened the ledger through the `Writer` path, taking the
  exclusive lock, appending a `lifecycle{op:boot}` record and reporting the
  CLI process itself as `writer_pid` — a "read-only" inspection that
  mutated the audited surface it inspected, and one that bailed with exit
  1/2 instead of reading at all when a live writer held the LOCK. Both
  verbs now walk the day files without locking (the LOCK file is read, not
  flocked): real writer identity, correct writer-held rows, no writes.
  `compact` remains a write operation and is unchanged.
- **Ledger hole / audit-trail fixes (TRBL-107, TRBL-110)**: every seq
  discontinuity found by the index scan is now queued and drained into
  `lifecycle{op:"hole"}` records by the next successful batch, torn-line
  recovery boot notes carry `from_seq`/`to_seq`, and a crash-glued pair of
  complete records on one line is extracted and indexed instead of being
  silently swallowed. Recovery-created gaps — a `HEAD` hint above the
  files' highest seq permanently skipped the in-between range with no
  trail record — are now registered as a hole and written to the audit
  trail on the next batch (recorded, never healed: seqs below a high HEAD
  are never reissued); registration is gated on a non-degraded index so
  degraded boots fabricate no holes.
- **Ledger verbs refuse a missing `--config` file (TRBL-111)**: pointing
  `trouble ledger ... --config` at a nonexistent path silently fell back
  to the default state root and wrote boot notes into a ledger the
  operator did not point at; it now answers `ledger: config not found:`
  and exits 13.
- **Token store quarantine instead of store-wide fail-closed (TRBL-103)**:
  one malformed entry in `dashboard-tokens.json` marked the entire token
  set invalid, so every valid token answered `503 +
  TROUBLE-DASHBOARD-013 (token_store_invalid)` until the line was fixed by
  hand. Fail-closed stays scoped to the file (unreadable, unstatable,
  wrong envelope, unparsable JSON): a malformed *entry* is now quarantined
  — skipped with a WARN naming path, index, id and reason (never token
  material), never enters the lookup maps so it can never authenticate,
  and is preserved verbatim on rewrite so a later save never deletes it.
  Unknown tokens still answer `401 + TROUBLE-DASHBOARD-002`.
- **Dashboard close refusals are surfaced, not masked (TRBL-REVIEW-CLOSE-ERR)**:
  a human close of a non-closable incident answered the generic
  `500 + action_failed`, losing the refusal reason. The bridge's close
  refusal now carries the seam shape, so the API answers
  `409 + TROUBLE-LADDER-012 (detail: no_close_edge)` with the human
  reason; `RefusedTransition` errors map to 409 and plain coded errors to
  400, while truly unknown errors keep the generic 500. Along the way the
  close pre-check refuses states with neither a close trigger nor a
  refusal rule (previously recorded as illegal transitions, quarantining
  the incident on the third) and edges resolve by ID, so legitimate
  closes through the dashboard no longer fail with `unknown transition`.

### Notes

- The repo publishes its own `guard/` connectors and a Python guard client used
  by the quality harness.
- Host-measured performance gates (throughput floors, p99 latency, RSS) pass,
  fail or explicitly SKIP behind `internal/loadfence.FenceLoadAvg`; a SKIP on a
  loaded machine is not a failure.
