# Dogfood integration report — 2026-09-24 — sentinel live ingest + fresh-machine install

**Target:** trouble at HEAD `73fecd0`. **Angle:** this is the third dogfood run on the repo.
2026-09-17 swept the stock CLI/dashboard and the QA lane; 2026-09-20 swept the SPEC-13 light-hub
profile and the install leg (superseding TRBL-011). The surface neither run touched live is the
**sentinel on-ramp under real event load** — POSTing events like an actual reporter — plus a
repeat of the install leg on a bare box now that the 0.1.4 bunker CLI spawns on first try.

## What was driven (real use, not the test suite)

1. Live daemon with state under `~/dgstate/trouble-state/`, listening on `127.0.0.1:7643`.
2. Generic-JSON on-ramp driven like a retrying reporter:
   `curl -X POST 'http://127.0.0.1:7643/api/1/event/?sentry_key=…' -d '{"message":"first trouble event","level":"error","release":"0.1.0"}'`
   sent THREE byte-identical times.
3. Idle-amplification window: 2 minutes of no interaction, ledger diffed before/after.
4. Fresh-machine install on an ephemeral bunker (las-bunker-03, bare Debian 13 user): documented
   quickstart verbatim from a source transfer (the repo has no clone credential in the agent).

## What held up

- The daemon ingests the generic-JSON on-ramp end-to-end: each POST returns 200 with a record id
  (`{id:67f432210c…}` shape on the fresh box), events land in the day-file ledger
  (`~/dgstate/trouble-state/ledger/2026-09-24.jsonl`), and the signature/digest machinery works —
  three byte-identical POSTs all produce the SAME `sig` (`sentinel:sha256v1:837a01c50fddf0d8`),
  exactly as the README promises ("hosts and arrival paths that describe the same bug produce the
  same digest").
- Idle amplification is 20x better than the 09-17 baseline: 54 records / ~63KB in 2 idle minutes
  (~10MB/day projected) vs ~200MB/day measured on 09-17 (the TRBL-009 direction). Real improvement.
- Fresh install works after the Go prerequisite is satisfied: `make bin` 43s, binary stamped
  `[73fecd0]`, daemon boots, first event 200, ledger written (34 lines), clean SIGTERM stop.
- The bunker CLI 0.1.4 install leg is now routine: spawn on first try, source transfer, quickstart,
  smoke, destroy. The 09-17 `deadline_exceeded` wedge (QA-LANE-5) is gone.

## What broke (rows filed, not fixed — dogfood never fixes code)

- **TRBL-074 (P1)** — no event-level dedup on the generic-JSON on-ramp. Three identical POSTs
  produced three separate event records; the group stayed `op=create count=1`,
  `counters.suppressed=0`. A retrying HTTP client duplicates evidence and the group counters lie.
  The hub profile has the phase-aware dedup gate (SPEC-13 §2.3); the standalone on-ramp has none.
  Repro command is in the row.
- **TRBL-075 (P3)** — one D-Bus sensor sig (`dbus:sha256v1:4cba8d44e663d78f`) re-emitted 15x in 2
  minutes with sub-second timestamps (18:41:55.669…18:41:56.843) despite
  `sensors.merge_window=5s` — the merge window is not catching whatever re-emits per poll tick.
  Residual ~10MB/day idle cost.
- **TRBL-076 (P2)** — the quickstart's first command (`make bin`) fails `go: not found` on a bare
  box; the Go 1.26 prerequisite exists in the README but not in the quickstart's command path.
  Docs friction, not a broken install. Also: a 73s curl of the 67MB Go tarball hit a transient
  partial-write once, and no doc mentions download resumption.
- **TRBL-073 (P2)** — install-leg hazard: `internal/sentinel/.sentinel-load-*/` holds ~1.6-1.8GB of
  untracked load-test residue ON DISK in the working tree (gitignored, but still shipped by the
  documented tar/source-transfer install path — a tar of this workdir is ~50x bigger than a fresh
  clone).

## Measured cost

| Operation | Time |
|---|---|
| `make bin` (fresh box, after Go present) | 43s |
| Go bootstrap (tarball, when a shared /tmp tarball was not used) | ~73s download (one transient partial-write failure) |
| Daemon boot → first event 200 (fresh box) | seconds (single-digit) |
| Idle ledger cost | 54 records / ~63KB / 2 min ≈ 10MB/day |
| Tar/source transfer to bunker | >5 min, 293MB+ remote tree (residue-driven; TRBL-073) |

## Honest gaps

- The `~1.8GB` residue figure in TRBL-073 is the 09-24 measurement; a re-`du` on the continuation
  pass showed ~1.57GB across the visible dirs — same order, same finding; the load test keeps
  writing to them.
- Ledger grep for the TRBL-074 sig returned 6 lines (3 events + group + repeat-probe lines from the
  same session) — the repro command in the row re-derives the exact count on a fresh ledger.
- Nothing was verified about hub-profile dedup (that gate is known-good from 09-20); this run's
  finding is scoped to the standalone on-ramp only.

## Verdict

🟡 PROMISING-BUT-ROUGH — the ingest path genuinely works and the 09-17 amplification problem is
20x better, but a retrying reporter duplicates evidence (P1) and the idle floor is still ~10MB/day
instead of flat. The fresh-machine install is real and fast once Go is present.
