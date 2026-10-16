# Dogfood run — 2026-10-16 — ledger-ops re-proof + verdict-composition on the TRBL-100/101/103 delta

Target: `trouble` at HEAD `6bf0ade` (delta since the 2026-10-13 ledger-ops run `c190bdb`:
TRBL-100 wires `trouble ledger status|verify|compact`, TRBL-101 heals mid-file torn lines,
TRBL-103 quarantines malformed token-store entries, TRBL-REVIEW-CLOSE-ERR surfaces the
ladder's close refusal on the dashboard API).

Scope law (skill §Step 0): re-proof scoped to the delta. Surfaces the delta cannot touch
(skill loop, sensors, hub) were not re-swept.

## Verdict: 🟡 PROMISING-BUT-ROUGH (ledger-ops surface)

The run-2 findings DF-LEDGER-1 (verbs absent) and DF-LEDGER-2 (torn heal unreportable) are
FIXED at HEAD — but real use of the NEW surface immediately surfaced four fresh defects
(filed as TRBL-104..108): the status verb is not read-only, the docs' live-ledger contract
contradicts the shipped verbs, verify false-positives on the audit spine, and the
documented hole-trail never fires. Core engine remains trustworthy (ingest, scrub,
dashboard, TRBL-103 quarantine all behaved correctly).

## The re-proofs (delta scope)

1. **DF-LEDGER-1 re-proof — VERBS EXIST and honor the exit contract.**
   `trouble ledger status/verify/compact --help` work; the help block states the exit
   contract per SPEC-01 §2.4 (status 0/1, verify 0/1/2, compact 0/1). Driven end-to-end on
   a scratch state root (`~/.local/state/trouble-df1016`, fresh key, HEAD-stamped binaries
   `make bin` 6bf0ade): happy-path status = rich LedgerStatus JSON (seq, index health,
   torn/corrupt counts, writer identity); verify = full report (lines, holes, hash
   mismatches, per-file errors); compact --dry-run renders the refusal/dry-run contract.
   Live-writer legs: with the daemon holding LOCK, status→rc=1 degraded, verify→rc=2
   unreadable, compact→rc=1 failed, all carrying `degraded_reason` naming the LOCK holder
   — the committed contract holds.

2. **DF-LEDGER-2 re-proof — mid-file torn heal WORKS.** Injected two torn fragments
   (60%-truncated complete records, realistic crash-mid-write shape) into the middle of a
   live scratch ledger, booted the daemon: `lifecycle{op:"recover_torn",
   error_code:"TROUBLE-LEDGER-003", lines_lost:1}` written at boot, both fragments removed
   from the file, file remains jq-clean, subsequent appends healthy. The fragment is now
   detected, counted, healed, and auditable — the exact gap run-2 filed.

3. **TRBL-103 verified live.** Injected a malformed token-store entry (invalid hash shape)
   alongside a good token, daemon load: `WARN dashboard token store: skipping malformed
   token entry index=1 id=probe-b-badhash reason="malformed entry (empty id or bad hash
   shape)"`, the good token still authenticates (HTTP 200 on /incidents with Bearer), the
   dashboard boots healthy. The fail-closed regression run-2 feared is fixed. (Polish
   notes: the malformed entry stays in the store on disk and the skip WARN logs twice for
   the same entry — two P4-grade observations, not filed.)

4. **Install leg RAN (bunker-mvp, agent 9dee92cf, evidence
   /tmp/bunker-qa-evidence-20261016T115442Z-2269145.jsonl, 20 rows).** fresh-install OK
   (toolchain bootstrap go1.26.5, repo sync, build), chaos-corruption OK (recovered,
   heartbeat rewritten within 3s), upgrade N/A (no tags), docker-deploy/chaos-shutdown
   SKIPPED by the documented live-host safety gate, ui-probe N/A (no web UI), ci-pass FAIL
   and chaos-resource FAIL (see findings below — both land on existing/pending rows).

5. **chaos-errorpath re-driven locally (the battery graded it UNVERIFIED on a harness
   timeout):** `troubled` with a missing config + /tmp state root refuses rc=13
   TROUBLE-LIFECYCLE-004 (forbidden root) — the daemon's fail-closed boot contract works;
   the rc=124 in the battery was the harness wrapper, not a daemon hang.

## Fresh findings (filed as rows TRBL-104..108 on the trouble board)

- **TRBL-104 (P1)** `trouble ledger status` is NOT read-only despite its help text: it
  takes LOCK_EX, appends `lifecycle{op:"boot"}` as actor `trouble-ledger-cli`, rewrites
  HEAD and the `.idx` sidecar, and reports ITSELF as `writer_pid`. Evidence: strace
  (flock LOCK_EX + O_WRONLY|O_APPEND on the day file + .HEAD.tmp/.idx.tmp writes) and the
  ledger itself (11 boot-op records in a one-day scratch ledger, none from the daemon).
- **TRBL-105 (P1)** ops.md §2 promises "verify opens no lock: always safe to run against
  a live ledger" + a daily-checks table that presumes a live daemon; shipped verbs refuse
  005 the moment the daemon is up (driven live: status rc=1, verify rc=2, compact rc=1).
  The documented daily workflow is unexecutable as shipped; §2 contradicts §3's own 005
  semantics.
- **TRBL-106 (P1)** verify reports DETERMINISTIC payload_hash_mismatch false-positives on
  struct-ordered payloads (config/lifecycle records): the writer serializes typed structs
  (field order), verify re-canonicalizes through a parsed map[string]any whose
  json.Marshal sorts keys — 1000 re-marshals of the seq-2 payload produced ZERO
  byte-identical forms; a brand-new compliant ledger flags its own seq 2. The audit spine
  is exactly the affected payload class.
- **TRBL-107 (P2)** ops.md §3 promises `lifecycle{op:"hole",from,to}` when verify sees a
  seq hole — never written (live 395→628 hole rode through 166 later appends, grep
  count 0); recover_torn omits the healed seq; and a torn write GLUED to the following
  record (no newline between them — the realistic crash-between-batches shape) parses as
  one line, tornShaped()=false, classified 011 corrupt, the intact record sharing the
  line unrecoverable.
- **TRBL-108 (P2)** the `<day>.jsonl.idx` sidecar carries internally inconsistent
  metadata (records:566, first_seq:1, last_seq:800 for a file holding 1-394 + 629-799)
  and has no documented consumer — silent-misgrading trap for the first future reader.

## Performance (Step 2b, hyperfine n=20 warm / drop-caches cold)

`trouble ledger status --json` and `ledger verify --json` on the 663 KB scratch ledger:
warm 395 ms ± 71 ms (status), verify similar; cold ~0.30 s; user+sys ≈ 36 ms of the wall
— an I/O+startup floor, not compute. Ingest 200s were sub-300 ms in prior runs and the
dashboard rendered in single-digit ms. Nothing a user would feel; **no PERF row** (per
the perf law: a win nobody can feel is not a finding). The one 4.4 s outlier during the
run coincided with the parallel bunker battery clone (host contention; user 23 ms ⇒
blocked, not slow) and does not reproduce.

## Fresh-machine leg details (evidence)

- bunker-mvp reachable (bunker3/bunker4 down, same as 2026-10-10); agent 9dee92cf spawned
  by `bunker-qa.sh launch` (phase A), collected with phase B (rows=20), destroyed.
- ci-pass FAIL members: TestLoadIngestThroughput (the QA-TROUBLE-17 recurring member),
  TestArtifactParseBudget (same row's 10-03 member) — premise of QA-TROUBLE-17 re-proven
  at HEAD 6bf0ade.
- chaos-resource FAIL: TestBudgetDashboardRSS (known cascade member) PLUS
  TestLedgerVerifyExitCodesSpec01Section24 — TRBL-100's own new test failing under the
  3G cap: the fresh surface shipped with a load-sensitive test, matching the
  QA-TROUBLE-17 class ("perf-budget tests red on slow hosts"). Evidence path and agent id
  above; left for the foreman to attach to the QA-TROUBLE-17/22 lineage.

## Left behind in the repo

- Board rows TRBL-104..108 (this run's findings, created_by-tagged).
- `docs/dogfood/2026-10-16-ledger-ops-reproof.md` (this file).
- `skills/trouble-usage/SKILL.md` updated to v1.2.0: the ledger verb family added to the
  entry-point table, the live-writer refusal + read-only-verb caveats, and the
  status-writes-records trap.
- `docs/dogfood/diagnostics.md` appended: the writer-vs-reader canonicalization split
  behind TRBL-106 and the glued-fragment classification gap behind TRBL-107.
- Dogfood log entry appended to `.coding-hermes/dogfood-log.md`.

## Cleanup

Scratch state removed: `~/.config/trouble-df1016`, `~/.local/state/trouble-df1016`,
/tmp probes (binaries, probe sources in cmd/probe-vt + cmd/probe-hash remain untracked in
the tree — probe-vt predates this tick; probe-hash is this run's hash-localization probe,
both left for the foreman's reference and safe to delete). No repo visibility, permission,
or scheduler state touched. Board verified by id-census after append (jq): exactly
TRBL-104..108 added, no duplicates.
