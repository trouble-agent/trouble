# Diagnostic trail — the trouble-qa lane, how it is built and where it bites

Explained, not raw logs. Written from a field test on 2026-09-17 that ran the
lane for real and reproduced two of its open findings. Read this before
believing any `*-qa` output, and before changing anything in the chain.

## 1. The chain, end to end

```
scheduler row "trouble-qa"
  └─ <fleet-home>/scripts/qa-scheduler-tick.sh        (resolves the target)
       └─ <fleet-home>/scripts/dagger-role-tick.sh qa trouble
            ├─ fresh `dagger serve` on a scanned free port, fresh role.db, fresh DAGGER_API_TOKEN
            └─ dagger run examples/coding-hermes/qa.ts   (in the hermes-dagger repo)
                 ├─ resolve_targets   targeted: QA_TARGETS + QA_REPO  |  random: qa_discover.py
                 ├─ run_battery       PHASE A: `bunker-qa.sh launch <repo>` (spawn + sync + start detached)
                 ├─ collect/parse     PHASE B: `bunker-qa.sh collect --evidence ...` (poll → pull → destroy)
                 ├─ interpret         an LLM pass over the cells → findings
                 └─ file_findings / duck_record / ledger row
```

Two properties matter: the pipeline is a **wrapper** (the interesting failures
are in the wrapper, not the idea), and the battery is **split into phases** on
purpose — a fresh-machine battery (spawn, sync, toolchain bootstrap, act/native
suite, compose, 4 chaos cells) takes 7–15 min while a single Hermes terminal call
is killed at ~420 s, so PHASE A returns in ~60–120 s and the pipeline polls in
PHASE B. If PHASE A does not return quickly, nothing downstream can happen.

### Why the target name matters twice

`qa-scheduler-tick.sh` maps the row name to a project: `*-qa` → strip the suffix
→ targeted at that project, with `QA_REPO=~/<project>`. `qa-audit`
(no suffix) → random discovery. That mapping is why a **targeted** row audits the
real repo while a **discovered** pick can audit the stand-in stub: discovery
selects scheduler *rows* (`<project>-qa`), and a `-qa` row's `workdir` in
scheduler.db is the stand-in stub, not the repo.

## 2. The traps, why they exist, and the right way

### Trap 1 — "a battery that never ran" is indistinguishable from "a battery that passed"

`launch()` truncates the evidence file at entry (`: > "$EVIDENCE"`) and only
writes cells as it goes. A 0-byte evidence file therefore means *nothing
happened*, while the pipeline's printed summary still reads as a completed node.
**Right way:** before believing a QA verdict, check the three sinks — the
`.meta` launch record next to the evidence file, the evidence file size, and the
DuckBrain/ledger row. `sinks-empty + green summary` = no audit.

### Trap 2 — the launch phase runs long, and PHASE B does not wait for the battery

`bunker-qa.sh launch` spawns an agent, syncs the tree, and starts the battery.
Three things turn that into a long haul:

* `sync_repo` tars the **whole workdir** with a hardcoded exclude list that never
  consults `.gitignore`. For `trouble` that is 141 MB, of which 111 MB is
  `.worktrees/` (stale worker worktrees carrying their own `.git` files) —
  plus the untracked `dagger.db`. On the agent the remote side then runs
  `git init && git add -A && git commit` over that payload inside a 1800 s ssh
  budget on a ~600 KB/s pipe. Measured phase A: **8.5 min** (17:01:46 → launch
  cell 22:10:15Z) against a documented 60–120 s.
* Each attempt re-spawns an agent and re-truncates the evidence file, so a
  retrying launch leaks agents and destroys the previous attempt's cells
  (`QA-LANE-2`).
* PHASE B is **one** collect attempt at the top of `parse_cells` — by design
  ("PHASE A: LAUNCH, never wait … if nothing collects it, the agent's TTL
  (default 4h) reclaims it", `qa.ts` ~L150-190). The battery needs 7–15 min, so
  in practice collect almost always sees `RUNNING`, and the pipeline grades the
  run from a single `launch` cell: verdict CLEAN, `filed:0`, real cells never
  pulled (`QA-LANE-6`). Right way: poll collect to completion inside a bounded
  window before interpreting, and never close a still-running battery as CLEAN.

**Right way (sync):** sync tracked files only (`git archive` / `git ls-files`),
add a size guard with a visible FAIL cell, and give the launch phase a wall-clock
deadline that fails fast.

### Trap 3 — the harness's own guesses become the project's findings

The battery auto-detects commands (`bunker-qa.sh:395-430`): `go.mod` present →
`BIN="go run ."`, `ci_cmd = native_cmd` when there is no workflow. For a Go repo
whose main packages live in `cmd/`, the chaos cells then "restart the app" with a
command that cannot work, receive `no Go files in <dir>` (rc=1), and report a
**clean error** — a false green. Two live consequences: `QA-OFF-BY-ONE-15` and
`QA-TROUBLE-3` (this run reproduced the underlying `go run .` failure directly).
It also picked the untracked `dagger.db` as the "state file" to corrupt, so the
state-restart property was never tested. **Right way:** detect entry points by
package layout (`cmd/*`, `main.go` at depth, `package.json` `bin`), prefer a
documented smoke (`make smoke`) when one exists, and treat "no start command was
detected" as UNVERIFIED, never as OK.

### Trap 4 — findings that are filed but not committed

`file_findings` appends the row through the stand-in symlink and then commits;
git refuses `beyond a symbolic link`, so for every stub-symlinked target the row
exists on disk and is **not** in the repo. The node still reports
`FILED=1 / write_failed:false`. **Right way:** resolve the board path with
`readlink -f` before add/commit and treat a git failure as `filed=false`.

### Trap 5 — the substrate

The battery needs a fresh JIT bunker agent. `bunker spawn` has a ~300 s client
deadline; the server's own `request_timeout` is also 300 s, so a client timeout
and a server-side success are indistinguishable from the caller's side — and
users/agents accumulate: `agent-host-2` showed 32 agent users against 6
registered agents, `agent-host-3` was at 8/8 users with 0 registered and could
never spawn again, `agent-host-4` refused too. **Right way:** a reaper for agent
users with no registry entry, destroy-before-retry, and one authoritative live
server list (the dogfood skill still says `agent-3`, `bunker-qa.sh`'s comment says
agent-3 was removed while `~/.bunker/config.yaml` registers it).

## 3. How this project's QA rows are organised

`QA-TROUBLE-N` = filed by the lane. Prefix scan is `QA-<PROJECT>` (upper-cased),
max id + 1, so a new filing never restarts at 1. Coverage is recorded in
`<fleet-home>/qa/ledger.jsonl` (one JSON row per run: `cells`, `findings`, `server`,
`agent`) and in DuckBrain `qa` namespace at `/qa/<date>/<project>`. The
`cells` map is the most useful field: `fresh-install`, `ci-pass`,
`docker-deploy`, `ui-probe`, `upgrade`, `chaos-*`.

Row IDs here: `QA-LANE-1..5` (lane defects found by the 2026-09-17 field test) and
`TRBL-012` (repo-level install gap) — deliberately not `QA-TROUBLE-*`, so the
lane's own id counter is untouched.

## 4. What was verified, and what was not

**Verified by direct execution:** `go run .` at repo root fails with `no Go files
in ~/trouble`; `go test ./internal/sensors/ -run
TestRSSContributionIsBounded -count=1` fails (16.4 MB vs 12.6 MB budget) at
load_avg ~12; `qa_discover.py` picks stub targets live; the stub-target
checkpoint (`launched:true` + `not a git repo`); the uncommitted row;
the three spawn failures; the 141 MB / 111 MB sync payload.

**Not verified (honest gaps):** no fresh-machine install of `trouble` was
completed with the documented commands by this run — agent-3/agent-2/agent-4 all
refused to spawn, so the install leg is `SKIPPED-install-bunker` (see `TRBL-011`
for the same class on 2026-09-17 and `QA-LANE-5` for today's evidence). The
chaos-corruption restart property remains UNVERIFIED, exactly as `QA-TROUBLE-3`
says. The lane's own targeted tick closed CLEAN one minute into its battery
(`QA-LANE-6`); this run then collected that battery by hand and the cells it
recovered are recorded in `2026-09-17-trouble-qa-lane.md`. The 12:28:29Z ledger
row from the lane's morning cycle remains the most recent *lane-produced* complete
coverage for `trouble`.
