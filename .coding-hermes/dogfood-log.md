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

2026-09-20 | dogfood (cron, target row trouble-dogfood → repo ~/trouble) | 🟡 PROMISING-BUT-ROUGH | light-hub boot serving + full hub stanza 1s after first poll; documented on-ramp event → ledger event+group on one sig ~30s; SPEC-13 stop/start contract reproduced in ~2 min; fresh-box quickstart READY 3s | 6 (3 harness-mine, 3 product/docs) | install leg RAN on agent-host-3 (agent 215f3511, destroyed): Go bootstrap 27s + `make bin` 25s + documented quickstart smoke PASSED | HEAD 43fd5ca; findings TRBL-062..066; artifacts docs/dogfood/2026-09-20-light-hub-integration.md, docs/dogfood/2026-09-20-light-hub-diagnostics.md, docs/dogfood/2026-09-20-repro-light-hub.sh, skills/trouble-light-hub-usage/SKILL.md

**Angle (skill rule: change the angle, not the depth).** The two 2026-09-17 runs swept the stock
config + CLI + dashboard surface and the QA lane, and BOTH had to file SKIPPED-install-bunker because
`bunker spawn` answered `deadline_exceeded`. This run took the two untouched surfaces: the SPEC-13
light-hub profile (never driven by a dogfood run) and the install leg itself (bunker CLI 0.1.4 —
spawned on the FIRST try).

**Verdict rationale.** The flagship light-hub promise HOLDS: one on-ramp POST produced `event` +
`group` + `incident` on one sig with the stream acked; a real `docker stop` of the wired redis gave
`degraded`/`redis_unavailable` + `Retry-After: 1` + zero ledger records (exactly SPEC-13 §4.3), and
`docker start` recovered to `status=ok` in 4s with `redis_restored` and no daemon restart. The fresh
box ran the documented quickstart verbatim and passed. It is 🟡 rather than ✅ because the OPERATOR
surface around that runtime is missing or wrong: the docs still say the runtime is unbuilt, the four
SPEC-13 §2.2 CLI verbs do not exist, a token mint against a container-shaped config 401s silently,
and the container quickstart's documented first event cannot authenticate at all.

**Findings (rows the foreman works — no code was changed by this run):**

1. TRBL-062 (P1) — the container quickstart's documented first event 401s `query_key_remote` even
   from the host the stack runs on; the shipped container project has no `secret_key`, and
   deploy/README.md's "Reporters OUTSIDE this host need one more line" is wrong for a `0.0.0.0` bind.
2. TRBL-063 (P2) — `dashboard token create` against a config declaring `dashboard.token_file`
   silently mints into the CLI's default store; the token 401s and nothing names the path.
3. TRBL-064 (P1) — `docs/operations.md` §14 still asserts `internal/hub` is not built and a
   light-hub boot carries no hub stanza; the same tree disproves both.
4. TRBL-065 (P2) — SPEC-13 §2.2's `trouble hub status|archive|dedup|drain` are unmounted
   (`unknown command "hub"`), so the documented pre-migration drain has no supported path.
5. TRBL-066 (P2) — a no-`.git` transfer makes `make bin` produce an UNSTAMPED pair
   (`0.0.0-dev unknown`) and the fresh-machine path never says so.

Install leg: **RAN** (install_seconds≈25 build + 27s Go bootstrap), smoke **passed**; TRBL-011's
SKIPPED row is superseded — its blocker was the 0.1.3 spawn wedge. No repo visibility or permission
was touched anywhere in this run.

**Honest gaps** (also in the diagnostics trail): the blob-level `ForwardEnvelope` duplicate-key
collapse is unreachable via the generic-JSON on-ramp (no idempotency key → `dedup_hits=0`); what is
proven is the group-level collapse. DuckBrain archival stayed parked by design (`TROUBLE-HUB-009`,
empty endpoint). Two of my own probe rounds were invalidated by harness mistakes (a `--rm` redis
deleted on `stop`; a second scratch redis that never started) — both discarded, and the final outage
test ran against the redis the daemon was actually wired to. Those traps are written down so the next
runner does not repeat them.

2026-09-24 | dogfood (target trouble-dogfood → repo ~/trouble, tick 2026-09-24-18-35-05) | 🟡 PROMISING-BUT-ROUGH | sentinel on-ramp driven live as a retrying reporter: 3 identical POSTs → 3 event records, sig identical, group count stayed 1 (no event-level dedup); 2-min idle window cost 54 records/~63KB (~10MB/day, 20x better than 09-17); fresh-machine install on las-bunker-03 (bare Debian): quickstart works after Go present — make bin 43s, stamped [73fecd0], first event 200, clean stop | 4 (TRBL-073 P2 residue-in-tree, TRBL-074 P1 no event dedup, TRBL-075 P3 idle dbus re-emit, TRBL-076 P2 quickstart assumes Go) | install_seconds=43 build + ~73s Go download (one transient partial-write; SKIPPED-install-bunker NOT filed — leg RAN, agent destroyed) | HEAD 73fecd0; artifacts docs/dogfood/2026-09-24-sentinel-ingest-integration.md + skills/trouble-sentinel-ingest-usage/SKILL.md; session interrupted (gateway drain_timeout) mid-writing artifacts — this log entry and the artifacts were completed by the continuation pass; board rows verified on disk before commit

2026-09-24 (nudge2) | closure verification of tick 2026-09-24-18-35-05 | previous continuation's work verified landed (HEAD 4f9a6d0, push parity 0, rows TRBL-073..076 + artifacts on disk, install leg RAN) | CI confirmed red twice on the load-calibrated timing family; INT-CI-001 amended with run 36047043173's superset failure set (TestAmortizedThroughput, TestPerLineRegression, TestDeriveNeverBlocks, TestScrubBudget, TestIngestHarnessThroughput); no new failure class, no new rows needed | no code touched, no repo visibility/permission changed
