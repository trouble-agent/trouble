---
name: trouble-qa-lane-usage
description: >-
  How to run, audit and believe the QA lane for this repo (scheduler row
  trouble-qa / qa-scheduler-tick.sh / bunker-qa.sh): targeted vs discovery
  targets, the three sinks that prove a run happened, and the five traps that
  make a green QA summary mean nothing. Load before trusting any QA-TROUBLE row
  or dispatching a QA tick.
version: 1.0.0
category: software-development
---

# trouble-qa — the QA lane (usage skill)

`trouble-qa` is not code in this repo. It is a **scheduler row** that runs a real
clean-machine battery against `trouble` (fresh JIT bunker agent → fresh install →
CI → chaos probes → findings filed as `QA-TROUBLE-*` board rows + a ledger row +
a DuckBrain record). Written from a field test on 2026-09-17 that reproduced two
open findings and watched a targeted tick fail to launch at all.

## When to use this

* You are about to trust, quote or close a `QA-TROUBLE-*` row.
* You want a fresh-machine audit of this repo (ask for it, or check whether the
  lane already produced one).
* A QA tick "passed" and you need to know whether anything actually ran.

## How to run it (targeted — the only mode that audits this repo)

```bash
cd <fleet-home>/stand-in/pm/qa
REAL_MODE=1 CODING_HERMES_PROJECT=trouble-qa CODING_HERMES_TICK=<unique-tick> \
  QA_TARGETS=trouble QA_REPO=~/trouble \
  bash <fleet-home>/scripts/qa-scheduler-tick.sh
```

* `REAL_MODE=0` = report-only (no board rows, no DuckBrain write) — use it for
  observation, `REAL_MODE=1` is what the scheduler runs.
* Expect **15–30 min** per target end-to-end *when the substrate is healthy*:
  spawn 20–90 s, sync (≈4 min for this repo at ~600 KB/s), battery 7–15 min,
  collect. PHASE A alone is documented at 60–120 s.
* Never set `CODING_HERMES_PROJECT` to nothing: an unset project name makes the
  tick resolve 0 targets and exit green having done nothing.

## Verify a tick actually delivered — never trust the summary

The pipeline prints a green summary for runs that launched nothing. Check all
three sinks:

```bash
# 1. launch record + evidence (0 bytes or no .meta = NO battery ran)
ls -la /tmp/bunker-qa-evidence-*.jsonl{,.meta} | tail -3
# 2. what the interpreter decided (payloads live in the run DB, not run.jsonl)
sqlite3 /tmp/dagger-role-qa-<id>-*/role.db \
  "SELECT node_id,status,substr(output,1,300) FROM checkpoints ORDER BY rowid;"
# 3. sinks: coverage row + memory record
tail -3 <fleet-home>/qa/ledger.jsonl
grep -o '"project":"trouble[^,]*' ~/duckbrain/namespaces/qa/event/*/current.jsonl | tail -3
# and the rows themselves
python3 -c "import json;print([json.loads(l)['id'] for l in open('.coding-hermes/board/tasks.jsonl') if l.strip()])"
```

A complete run shows: `launch` cell with an agent id → cells for
`fresh-install`/`ci-pass`/`chaos-*` → `interpret` findings → `filed:N` → a ledger
row with the same `ts` as the DuckBrain key `/qa/<date>/trouble`.

## Reading the cells

| Cell | What a non-OK value means |
|---|---|
| `fresh-install` | the documented install path failed on a clean box (highest-value cell) |
| `ci-pass` | act (or the native suite when there is no workflow) failed |
| `docker-deploy` / `upgrade` | `N/A` is often a *detection gap*, not a pass (no compose file / tags not detected) |
| `chaos-corruption` | **be suspicious of OK**: the harness restarts the app with `go run .`, which cannot work for this repo (`cmd/` layout) — see `QA-TROUBLE-3` |
| `chaos-disconnect` | a 120 s cell window against a ~2.5 min suite reads as a HANG when it is not — see `QA-TROUBLE-2` |

## Pitfalls (all proven in real runs)

1. **Green ≠ ran.** No `.meta`, empty evidence, or a `launched:true` next to
   `not a git repo` all mean nothing executed.
2. **Discovery does not audit this repo.** `qa_discover.py` still picks `*-qa`
   stub rows whose "repo" is an empty stand-in dir; only the targeted
   `trouble-qa` row resolves `~/trouble`. `QA-LANE-1`.
3. **Filing can lie.** `filed:1 / write_failed:false` has been observed together
   with `fatal: pathspec … beyond a symbolic link`, i.e. the row exists on disk
   and is uncommitted. Check `git status` on the board file. `QA-LANE-4`.
4. **Spawn is the fragile dependency.** `bunker spawn` timing out
   (`deadline_exceeded`, ~300 s) means no fresh-machine audit; a timeout can also
   leave an orphan agent behind (seen: agent `cefd6920`, referenced by nothing).
   `QA-LANE-2`, `QA-LANE-5`.
5. **The sync is not a clean checkout.** The battery ships the whole workdir,
   gitignored files included (141 MB here, 111 MB of it `.worktrees/`), and then
   `git init && add -A && commit`s it on the agent. Expect long syncs, and do not
   read chaos-cells about "the repo state" as being about tracked state.
   `QA-LANE-3`.
6. **Rows are claims, not evidence.** The lane's own contract: reproduce at most
   one claim per cycle yourself. Two of three `QA-TROUBLE-*` rows reproduced
   exactly as written — which is the reason to take the rest seriously.

## Verification recipe (copy/paste)

```bash
cd ~/trouble
go run . ; echo "rc=$?"                                   # expect: no Go files (QA-TROUBLE-3)
go test ./internal/sensors/ -run TestRSSContributionIsBounded -count=1   # expect FAIL under load (QA-TROUBLE-1)
```
