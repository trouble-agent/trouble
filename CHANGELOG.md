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
- **Packaging and deployment**: the `make release` multi-platform matrix
  (linux/amd64, linux/arm64, darwin/arm64, windows/amd64) with a stamped
  `dist/manifest.json`, a multi-stage `Dockerfile` (static, CGO disabled), a
  compose stack with a light-hub profile, and the `examples/` config set.
- **Docs**: the README contracts, `docs/operations.md` runbook,
  `docs/sentinel-compat.md`, `docs/cmd.md`, and the shipped usage skills under
  `skills/`.

### Notes

- The repo publishes its own `guard/` connectors and a Python guard client used
  by the quality harness.
- Host-measured performance gates (throughput floors, p99 latency, RSS) pass,
  fail or explicitly SKIP behind `internal/loadfence.FenceLoadAvg`; a SKIP on a
  loaded machine is not a failure.
