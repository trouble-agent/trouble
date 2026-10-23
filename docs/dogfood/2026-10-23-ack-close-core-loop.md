# Dogfood run — trouble — 2026-10-23 (tick trouble-dogfood-2026-10-23-04-13-16)

Verdict: CONDITIONAL-PASS for the surfaces exercised; core-loop blocker (ack->close)
live-reproduced; SKIPPED-install-bunker (see TRBL-121 note below).

Build: HEAD b0faa21, fresh build (`make build`) with Version/GitSHA/BuildDate pinned in
`bin/troubled` (version output proves the commit). Scratch instance `df1023` under the
stand-in workdir (the documented refuser keeps /tmp scratch out of boot).

## What was exercised (the real-use pass)
- Fresh daemon boot against a hand-written scratch config with a `[[projects]]` table
  (loopback-only :7643 ingest, :7644 dashboard).
- First event through the sentinel's documented curl on-ramp: 200 in 0.39s cold, dedup
  keys (`sentinel:sha256v1:...`) appear in the ledger. VERIFIED.
- Dashboard token mint via `trouble dashboard token create`, restart with store present,
  authenticated GET /incidents: 200, page renders, CSRF meta present, ack form present.
- Ack via the form's CSRF/hidden-value contract: 200, `acknowledged` transition record in
  the ledger.
- Close after ack: NEVER succeeds. -> TRBL-118 (P1) — see rows.jsonl.
- Mint-while-refused (boot without token store): mint OK in the CLI, data plane stays
  503/013 -> requires a full daemon restart. -> TRBL-119 (P2). Reproduced again on an
  AS-SHIPPED examples/config.toml boot where the verbatim quickstart first-event also
  returned 200 (placeholder key).
- `trouble ledger verify` (read-only) against the live writer: exit 0 at 815 lines/day
  across 2 files (active day + a rotated-copy compaction round-trip), 0 seq_holes,
  0 torn/corrupt. TRBL-117 fix re-proven at HEAD on the USER surface. VERIFIED.
- `trouble ledger compact` on a NON-active day: 405 lines -> 1 gen-1 file, verify-after-
  compact: 0 holes. TRBL-116 fix re-proven at HEAD. VERIFIED.
- TRBL-112 (open, status `failed`) re-proof: refused/dry-run compact STILL leaves a
  LOCK file naming its own dead pid and `trouble dashboard status` still reports that
  dead pid as `writer`. -> TRBL-120 (P2, re-proof). STILL LIVE at HEAD.

## Install leg (bunker)
- `bunker-qa.sh status` ran: server bunker-mvp alive, bunkerd active, docker yes, uidmap
  yes (evidence in the transcript). The leg was NOT COMPLETED this tick — the pre-tick
  deconfliction check (board tail + events.jsonl + tasks.jsonl `created_by`) came back
  EMPTY for this tick's label, the executor's session DB was then consulted, and the
  remaining budget went to the freshly-reproduced rows + their write-back. The bunker
  agents observed on the host (21e7fc4e / 0e8aec07) belong to OTHER running workers and
  were left alone.

## Findings filed this tick
| id | pri | one-line |
|---|---|---|
| TRBL-118 | P1 | ack/close core-loop blocker: ack is record-only, the ladder never leaves `detected`, every close from a freshly-acked incident is refused (400 no_close_edge / 409 stale_view); the dashboard still renders the close form |
| TRBL-119 | P2 | a token minted while the dashboard boots refused stays 503-forever until a daemon restart (QA-TROUBLE-19 un-refuse contract regressed under the c12f64a fix) |
| TRBL-120 | P2 | re-proof of open TRBL-112: refused dry-run compact leaves a dead-pid LOCK; status reports the dead pid as `writer` |

(Install-leg SKIP is recorded in the run log and this report; it does not mint a new
row id this tick because TRBL-089/QA-LANE-5 already own the skip-row pattern and the
leg merely was not REACHED — no separate defensible defect row.)

## Perf (Step 2b)
Nothing user-noticeable: ingest cold first-event 0.39s; warm daemon ops ~0.3-0.5ms;
ledger compaction of the scratch round-trip 437 -> 428 lines 12ms; verify 0.7s over
~405 lines 815-line two-file day set. `time -p` wall numbers in the transcript.
Cold daemon boot to a serving dashboard ~2s (boot.log timestamps 04:20:08 -> 04:20:10).
NO perf row.

## Evidence (in-repo)
- This file: `docs/dogfood/2026-10-23-ack-close-core-loop.md`
- `/tmp/df-close-evidence.json` — full stale_view body + refreshed fragment
- `/tmp/df-ack-close-trail.txt` — the seq-276..383 ack/close probe trail
- `/tmp/df-rows.jsonl` — the 3 boarded rows (byte-identical copies of the board's lines)
- run record + key: DuckBrain namespace `trouble`, key `/dogfood/2026-10-23/tick-04-13-16`

## What was NOT exercised
- The ephemeral-bunker install leg (see above).
- hub profile (Redis), issue desk drivers, sensor modules beyond the shipped set.