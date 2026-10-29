# Dogfood evidence 2026-10-29

Tick: trouble-dogfood-2026-10-29-11-40-58
Target: trouble
HEAD: 50866ae4a232969fee2a354927ce7e298e5955be (origin/main equal)
Canonical checkout: /home/kara/trouble; stand-in board resolves there.

Promise: trouble provides a self-hosted incident brain with sensors, a Sentry-compatible ingest endpoint, append-only ledger, ladder, dashboard, and operator CLI. A user should be able to build stamped binaries, boot the documented config, inspect health, submit an event, and read the durable event/group/incident trail.

Real use:
- Clean detached worktree at HEAD built with `make bin`.
- `bin/trouble --version` printed `50866ae 50866ae 2026-10-29T11:48:33.000Z [stamped]`.
- Isolated state root under ~/.local/state, explicit config/env/token paths, ports 17643/17644.
- Daemon health: HTTP 200, status=ok, version/git_sha=50866ae, all five subsystems built=true/refused=false, ledger_last_seq=28.
- Authenticated dashboard paths: /incidents 200/26ms/2870 bytes; /groups 200/13ms/7618 bytes; /rules 200/12ms/4801 bytes; /breakers 200/10ms/2824 bytes; /partials/budget 200/10ms/531 bytes.
- Three documented generic JSON on-ramp POSTs: all HTTP 200; 290ms, 426ms, 430ms; each returned an event id.
- Ledger after the workflow: config=1, event=35, group=3, incident=3, lifecycle=1; event/group/incident records persisted, and event/group share each digest.
- Clean SIGTERM shutdown observed for daemon PID 35664; wrapper and daemon were same process in this launch.
- CLI config explain and topology both returned JSON rc=0. `check-stall` rc=8 against a stopped daemon, which is the expected stale/unreadable class for that invocation. Ledger status/verify were not re-run after the daemon stopped because this checkout's config resolver flags differed from the docs invocation and the earlier probe was rc=8.

Fresh-machine install leg:
- Required `bunker-qa.sh launch /home/kara/trouble` was run against bunker-mvp, which was ONLINE, bunkerd active, Docker 29.1.3, disk 68%, 2 existing agents plus the run-owned agent.
- Launch reached agent ae53cc5e and printed fresh-install start, but the foreground launch was terminated by the 30s tool timeout before the launch cell was recorded. Agent-side qa-evidence.jsonl was 0 bytes and no agent logs were available.
- The run-owned agent was destroyed; `bunker info ae53cc5e` returned not_found and a fresh `bunker list` showed it absent. The install leg is PARTIAL/UNPROVEN, not PASS and not SKIPPED-install-bunker: the host and spawn worked, but no install/smoke evidence closed.
- The collected helper file /tmp/bunker-qa-evidence-20261029T115558Z-53774.jsonl says launch terminated by SIGTERM before launch cell was recorded.

Perf:
- User-visible daemon startup to health: 1411ms cold on this run.
- Dashboard warm timings: 10-26ms across the exercised routes.
- Event cold/warm timings: 290ms, 426ms, 430ms. The ~0.4s single-event latency is noticeable but already documented in repo history; no new PERF row was filed because this run did not isolate a new regression and nothing crossed the skill's filing threshold beyond the existing known group-commit behavior.
- No profile generated; the measured result did not justify profiling a new hot path.

No code or board state was changed by this lane. Product checkout had pre-existing unrelated untracked files: .gitreins/tasks.yaml.corrupt-963c8d6c853e, resolved, trouble. Scratch worktree and runtime state are outside the repo.
