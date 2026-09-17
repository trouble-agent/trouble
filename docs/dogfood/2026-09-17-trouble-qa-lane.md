# trouble-qa lane — field test 2026-09-17 (dogfood, real-use depth)

**Target:** scheduler row `trouble-qa` — the QA lane dedicated to this project.
Not an app: a targeted QA row whose body is
`<fleet-home>/scripts/qa-scheduler-tick.sh` → `dagger-role-tick.sh qa trouble` →
`examples/coding-hermes/qa.ts` on the hermes-dagger engine, with the battery in
`<fleet-home>/scripts/bunker-qa.sh`. Its promise: *"you are the verification
layer"* — a real clean-machine battery (fresh JIT bunker agent, fresh install,
CI, chaos probes) whose findings become board rows here, with a ledger row and a
DuckBrain record proving coverage.

**Promise statement (null hypothesis):** a user can ask *"has `trouble` been
QA'd on a clean machine, and will `trouble-qa` audit it now?"* and get
(a) a fresh machine battery, (b) verifiable findings filed on this board, and
(c) a coverage record — autonomously, at the row's normal cadence.

## How it was used (real use, not a test run)

1. **Read the lane's existing output** — the three rows it filed on 2026-09-17
   (`QA-TROUBLE-1..3`), the QA ledger row for `trouble`
   (`<fleet-home>/qa/ledger.jsonl`, 12:28:29Z, server agent-host-3, agent
   96164169) and the DuckBrain `qa` namespace.
2. **Independently reproduced two of its three claims** (the lane's own contract
   is "reproduce at most ONE claimed failure yourself" — this run reproduced two
   because the rows are the only evidence a later reader has):
   * `QA-TROUBLE-3` (chaos-corruption false green): `go run .` in the repo root →
     `no Go files in ~/trouble`, and `bunker-qa.sh:408` sets
     `BIN="go run ."` for any repo with a `go.mod`. **CONFIRMED** — the restart
     probe never started the app; `trouble` main lives in `cmd/trouble` and
     `cmd/troubled`.
   * `QA-TROUBLE-1` (RSS assertion is environment-fragile):
     `go test ./internal/sensors/ -run TestRSSContributionIsBounded -count=1` →
     `--- FAIL: sensors contributed 16429056 bytes of RSS (13262848 -> 29691904),
     budget is 12582912` (load_avg 12.4 at start, 11.6 after). **CONFIRMED** on a
     fourth environment; the claim's "3 fails : 1 pass across envs" reading (it
     is host/GC-dependent, not a deterministic leak) is the right reading.
3. **Ran the lane for real** — production mode, the way the scheduler does it:
   ```bash
   cd <fleet-home>/stand-in/pm/qa
   REAL_MODE=1 CODING_HERMES_PROJECT=trouble-qa CODING_HERMES_TICK=<tick> \
     QA_TARGETS=trouble QA_REPO=~/trouble \
     bash <fleet-home>/scripts/qa-scheduler-tick.sh
   ```
   Targeting resolved correctly (`resolve_targets: {"mode":"targeted","project":
   "trouble","repo":"~/trouble"}`). See "What happened" below — it did
   **not** deliver.
4. **Probed the discovery path** with the picker the lane itself runs:
   `python3 <fleet-home>/scripts/qa_discover.py 2`.
5. **Tried the ephemeral install leg** (fresh bunker machine, documented install
   commands) — the substrate refused to spawn; evidence in the rows.

## What happened (verbatim outcomes)

**The targeted tick launched a battery, then graded the run before it finished.**
Phase A started 17:01:46 local and wrote its launch cell at 22:10:15Z — **8.5
minutes** against a documented 60–120 s budget, with **two** spawn attempts and
two concurrent syncs in the window, and one agent leaked. Then the pipeline ran
its single PHASE-B collect, saw `RUNNING`, and went straight to `interpret` /
`file_findings`:

| Observation | Value |
|---|---|
| `launch` cell | `OK agent=ed361cdd server=agent-host-2 ttl=4h script_bytes=19056` @ 22:10:15Z |
| agents spawned in the tick's window | `cefd6920` (17:02:30, referenced by **no** evidence file → orphan, still running), `ed361cdd` (17:06:20, the launch agent) |
| concurrent syncs | **two** `tar … -C ~/trouble \| ssh` pipelines in flight at once, both truncating the same evidence path |
| payload shipped | 141 MB total (`du --exclude=.git`), **111 MB of it `.worktrees/`** — gitignored, plus `dagger.db` and `bin/` |
| battery state at grading time | running: `toolchain-bootstrap OK (go 1.26.5)`, `fresh-install` in progress (22:10:45Z) |
| verdict | **CLEAN**, `findings: []`, `filed: 0` — summary: *"phase-B collect has not reported results, so no findings are derivable"* |
| what happened to the rest | the battery's later cells were never pulled; no ledger row; the agent left to its 4 h TTL |

So the run did not silently lie about evidence — it honestly said it had none —
but it still **closed as CLEAN**, one minute into a 7–15 minute battery. That is
`QA-LANE-6`, and it is the same premature-completion pattern this skill exists to
catch, sitting inside the verification lane. The lane's own report card, verbatim:

```
🟢 · QA · trouble · 10m46s

1 audited · 0 findings (0)
• trouble — CLEAN: Only the launch cell is recorded (OK, agent=ed361cdd on agent-host-2,
•   ttl=4h, battery detached); no FAIL or INFO cells exist, and phase-B collect has not reported
•   results, so no findings are derivable.
```

A green check and `CLEAN` for the same battery whose `ci-pass` cell FAILed
(below). Also observed: **no coverage row appeared in `<fleet-home>/qa/ledger.jsonl`
for this run** — the newest `trouble` entry is still the 12:28:29Z one from the
morning cycle, so a run that produced nothing also left no trace that it ran.
(This run then did what the lane did not: waited for the battery and collected it
— see the section below.)

**Discovery is auditing empty stubs.** `qa_discover.py` filters `-pm`, `-sync`,
dogfood and sync names but **not `-qa`**. Live dry-run:

```json
{"project":"<project>-qa","repo":"<fleet-home>/stand-in/pm/<project>","reason":"never-qa"}
{"project":"hermes-canopy-qa","repo":"<fleet-home>/stand-in/pm/hermes-canopy","reason":"never-qa"}
```

`<fleet-home>/stand-in/pm/<project>` contains only `.coding-hermes/board` (a symlink
to the real board) — no source, not a git repo (12 such rows are enabled). The
pipeline's own checkpoint proves the outcome for a stub target:

```json
{"project":"terminal-jail-qa","repo":"<fleet-home>/stand-in/pm/terminal-jail"}
run_battery raw: "ERROR: <fleet-home>/stand-in/pm/terminal-jail is not a git repo"
launched: true          <-- claims a launch that never happened
```

…and its interpreter converted that into a P1 finding about the *harness*, filed
on the **real** project board (`QA-TERMINAL-JAIL-QA-1` in
`~/terminal-jail/.coding-hermes/board/tasks.jsonl`). That row is on disk
but **uncommitted**: the node reported `FILED=1`, `write_failed:false`, and in the
same output `fatal: pathspec '.coding-hermes/board/tasks.jsonl' is beyond a
symbolic link`.

**The install leg could not run.** `bunker spawn` returned
`deadline_exceeded: context deadline exceeded` (~300 s) on **all three** boxes
tried — `agent-host-3` (the box the dogfood skill names), `agent-host-2`
(the QA default) and `agent-host-4`. agent-3 is wedged: 8 bunker users against
`max_agents: 8`, all their `bunker-docker-*` units inactive, one broken user
(`bunker-b97e8e8c` has a home dir and no passwd entry), and the registry reports
**0** agents — so it can never spawn again without manual cleanup. agent-2 has
32 users on disk against 6 registered agents.

## What the lane left on the table — the battery nobody read

This run did the step the pipeline skipped: polled until `~/qa-run.finished` and
ran the sanctioned collector
(`bunker-qa.sh collect --evidence /tmp/bunker-qa-evidence-1789682506423-1.jsonl`
→ *"pulled 11 evidence row(s), merge-deduped by ts; temp agent destroyed"*).
**Battery wall time: 13.9 minutes.** Verbatim cells for `trouble` @ 28c3c01 on a
fresh box (go 1.26.5 bootstrapped from scratch):

| Cell | Status | Verbatim |
|---|---|---|
| `toolchain-bootstrap` | OK | `go 1.26.5 installed (~/tools/go)` |
| `fresh-install` | OK | `go: downloading golang.org/x/sys v0.48.0` |
| `ci-pass` | **FAIL** | `native rc=1: FAIL …/internal/sensors 74.846s` → `--- FAIL: TestRuleDirMTimesDetectsChange (0.00s) | reload_test.go:310: the mtime fingerprint did not change after an edit` |
| `upgrade` | N/A | `no release tags in repo` |
| `docker-deploy` | N/A | `no compose file` |
| `ui-probe` | N/A | `no web UI detected` ← the repo ships a server-rendered dashboard |
| `chaos-disconnect` | FAIL (artifact) | `HANGS when network is cut (timeout 120s)` — same cell shows `ok …/internal/app 100.799s` |
| `chaos-shutdown` | N/A | `no compose file` |
| `chaos-corruption` | OK (**false green**) | `clean error on the next start after truncating ./dagger.db (rc=1, not referenced by any source file): no Go files in <agent-home>/qa-trouble` |
| `chaos-resource` | INFO | `OOM under cap (expected for heavy suites)` |
| `chaos-errorpath` | OK | `clean error on missing config (rc=1)` |

Two things follow.

1. **The lane dropped a real red**: `ci-pass` FAILed on the clean machine — a test
   that passes on the control host (`go test ./internal/sensors/ -run
   TestRuleDirMTimesDetectsChange -count=1` → `ok 0.006s`, load_avg 3.08). Filed
   as `TRBL-013`. The verdict the lane recorded for that same run was **CLEAN**.
2. **The known harness defects reproduced verbatim** in the same battery:
   `chaos-corruption` false green (`QA-TROUBLE-3`), `chaos-disconnect` 120 s
   window misreading a 100 s app suite (`QA-TROUBLE-2`), and a probe gap the
   harness has now missed twice (`ui-probe` on a Go-native dashboard). Filed as
   `QA-LANE-7` (plus the misleading `act rc=1` label on a repo with no
   `.github/`).

## Verdict

**🟡 PROMISING-BUT-ROUGH.**

The lane's *judgement* is good and its findings are **real**: two of three
`QA-TROUBLE` rows reproduced independently, and the lane visibly self-corrects
(`QA-TROUBLE-2` downgrades its own "HANGS" verdict to a harness artifact after an
offline control run — that is exactly the behaviour this skill exists to
enforce). The lane is also the only thing in the fleet that runs a fresh-machine
battery on `trouble` at all: it is the reason `QA-TROUBLE-1..3` exist.

The *plumbing* is where it fails: on this run the target audit did not execute,
the discovery half spends its slots on empty stub directories, findings land
uncommitted while the tool reports success, and the spawn substrate that both the
QA lane and the dogfood install leg depend on is out of capacity. Rough, not
worthless — the fixes are all in the wrapper, none in the idea.

Time-to-first-success for the *existing* lane output: **~25 minutes** (read the
rows, reproduce two claims, walk the ledger/DuckBrain sinks). Time-to-first-success
for a *fresh* targeted tick: launch+battery are real (8.5 min to launch cell,
battery ran) but the run **closed CLEAN having measured nothing** and its cells
were never pulled — a fresh clean-machine verdict for `trouble` is still not
obtainable from the lane.

Friction count: **12** distinct blockers/frictions (7 filed as `QA-LANE-1..7`,
2 as `TRBL-012..013`, 3 log-only).

## Top findings (filed as board rows)

| ID | Sev | Finding |
|---|---|---|
| `QA-LANE-1` | P1 | Discovery audits non-repo `*-qa` stub workdirs → wasted batteries, findings about the harness filed on real boards, real project stays "never-qa" |
| `QA-LANE-2` | P1 | Launch retry ladder leaks live agents (`cefd6920` orphan) and truncates the evidence file per attempt |
| `QA-LANE-3` | P1 | Sync ships the whole workdir (141 MB, 111 MB gitignored `.worktrees`) → phase A 8.5 min against a 60–120 s budget |
| `QA-LANE-4` | P1 | `FILED=1 / write_failed:false` while the commit died (`beyond a symbolic link`) → rows land uncommitted |
| `QA-LANE-6` | P1 | Verdict CLEAN / `filed:0` one minute into a 13.9 min battery: single collect attempt, the recovered cells included a real CI FAIL |
| `QA-LANE-5` | P2 | Bunker substrate degraded: agent-3 wedged, agent-2/04 refusing spawns; install verification unverifiable |
| `QA-LANE-7` | P2 | Harness reporting recurrences: `act` label with no `.github/`, `ui-probe` misses the Go dashboard, chaos-window artifacts, chaos-corruption false green |
| `TRBL-012` | P2 | No git remote → a fresh user cannot obtain this repo; Go version and fetch path undocumented |
| `TRBL-013` | P1 | Clean-machine native suite RED: `TestRuleDirMTimesDetectsChange` fails on a fresh box, passes on the control host |

## What a user should take from this

* Trust `QA-TROUBLE-*` rows as evidence — they survived reproduction.
* Do **not** trust the lane's *green* runs. A battery that never launched, a
  battery that is still running, and a battery that passed all print the same
  way. The tells: the evidence file (0 bytes / only a `launch` cell), the
  `.meta` record, and whether `interpret` ran within a minute of the launch cell
  — if it did, the run closed before it measured anything (`QA-LANE-6`).
* Targeted rows (`<project>-qa`) resolve to the real repo; discovery does not —
  until `QA-LANE-1` is fixed, treat discovery output as coverage theatre.

Diagnostic trail (how the lane is built, why each trap exists, the right way):
`docs/dogfood/2026-09-17-trouble-qa-diagnostics.md`.
Agent-facing usage skill: `skills/trouble-qa-lane-usage/SKILL.md`.
