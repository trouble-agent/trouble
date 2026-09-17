# Dogfood log — trouble

One line per dogfood/field-test run. Newest last. Fields:
`date | lane | verdict | time-to-first-success | frictions | install leg | notes (paths)`.

2026-09-17 | dogfood (cron, target row trouble-sync → real repo ~/trouble) | 🟡 PROMISING-BUT-ROUGH | binaries 1.2s; daemon serving /health.json ~12m (after repairing the example config); first authenticated dashboard page ~27m; first event via the documented on-ramp NEVER (not configurable) | 10 (4 first-success blockers) | install_seconds=N/A — SKIPPED-install-bunker (bunker spawn deadline_exceeded ×2, TRBL-011) | build c363daf; findings TRBL-005..TRBL-011; artifacts docs/dogfood/2026-09-17-integration.md, docs/dogfood/diagnostics.md, skills/trouble-usage/SKILL.md

Promise vs reality: README promises sensors + Sentry-compatible sentinel + dedup
ladder + ledger + issue desk + flow + dashboard + skill loop ("one curl from any
runtime"). Reality on the shipped config: sensors + ledger + ladder + dashboard
work on live host data (74 incidents, 778 records / 6 min); sentinel (ingest),
issues and skills are silently not built; the health surface reports `ok`.

Top 3 findings (by severity):

1. TRBL-005 (P0) — `examples/config.toml` cannot boot: `verify.zone_windows`
   inline table vs the SPEC-12 string form → `TROUBLE-LIFECYCLE-001`, exit 13,
   no listener. First key a fresh user meets; the documented copy-edit path dies
   there.
2. TRBL-006 (P0) — the default `dashboard.token_file` is a literal `~` path and
   is never expanded, so a stock config's token store loads as an *empty* set:
   every CLI-minted `tdt_` token 401s (`TROUBLE-DASHBOARD-002`) silently.
   Adding an absolute `token_file` flips the same token to 200.
3. TRBL-007 (P0) — sentinel/issues/skills are refused on every stock boot
   (`subsystem_not_built` ledger records) and nothing operator-facing says so:
   the ingest plane never binds (the README's "one curl from any runtime" is
   unreachable), and once the rules directory exists `/health.json` reports
   `status:"ok"` for an instance missing three of five subsystems.

Also: TRBL-008 (no quickstart + `dsh_`/`tdt_` and `sha256[:32]` doc drift),
TRBL-009 (idle-host amplification: 74 incidents from 21 sigs; 137 event records
for one psi sig in 6 min; ~200 MB/day ledger projection), TRBL-010 (dashboard
first-paint/footer/heartbeat zeros + IP-keyed auth throttle 429s a valid token),
TRBL-011 (explicit SKIPPED-install-bunker).

Left behind in the repo: `docs/dogfood/2026-09-17-integration.md` (integration
report + reproduction script), `docs/dogfood/diagnostics.md` (how the system is
built, why the traps exist, the right way for each error, and what could not be
verified), `skills/trouble-usage/SKILL.md` (agent-facing usage skill). No source
code was modified: findings became board rows (the foreman fixes).

Fleet/infra note (not a trouble finding): the dogfood picker selected
`trouble-sync`, a `*-sync` shell row whose own README states "gap-push and
dogfood pickers should skip `*-sync` rows or read this file first";
`<fleet-home>/scripts/standin-pick.py:250` implements that skip (SCHED-GAP-087)
while `<fleet-home>/scripts/dogfood-pick.py` does not. This tick therefore ran
against the repo behind the sync row (`local:~/trouble`) rather than a
project with its own board — harmless here, but a wasted slot for a sync row
with no product.

2026-09-17 | dogfood (cron, target row trouble-qa → QA lane for ~/trouble) | 🟡 PROMISING-BUT-ROUGH | existing lane output audited in ~25m (2 claims reproduced); fresh targeted tick launched a battery in 8.5m but graded the run CLEAN one minute in | 11 (6 filed QA-LANE-1..6, 1 TRBL-012, 4 log-only) | install_seconds=N/A — SKIPPED-install-bunker (spawn deadline_exceeded on agent-host-3 AND agent-2 AND agent-4 at ~300s each; QA-LANE-5) | build c363daf; findings QA-LANE-1..6 + TRBL-012; artifacts docs/dogfood/2026-09-17-trouble-qa-lane.md, docs/dogfood/2026-09-17-trouble-qa-diagnostics.md, skills/trouble-qa-lane-usage/SKILL.md

Target = the QA lane itself (scheduler row `trouble-qa`), not a second pass over
the repo: the lane's own contracts ("reproduce at most ONE claimed failure";
"evidence is board rows + ledger + DuckBrain record") are what was tested. Two of
three open QA-TROUBLE rows reproduced exactly as written, so the lane's judgement
is real. Its plumbing is not: phase A took 8.5 min against a documented 60-120s
(two spawn attempts, first attempt's agent cefd6920 leaked and still running, two
concurrent syncs sharing one evidence path), the sync ships 141 MB of workdir
(111 MB gitignored `.worktrees/`), discovery picks `*-qa` stub workdirs (no
source, not a git repo) and files harness findings on real project boards, a
`FILED=1` was observed next to `fatal: pathspec … beyond a symbolic link` (row on
disk, uncommitted), and — the headline — the pipeline graded the run CLEAN with
filed:0 one minute into a 7-15 min battery, so the real cells were never pulled.
This run then collected that battery by hand (cells recorded in the integration
report). Neither the QA battery nor this run's install leg could spawn a fresh
agent on any of three bunker boxes beyond the one that was already up.
