# Dogfood run 2026-10-13 — the SPEC-11 skill loop, driven as a real user

Tick: `trouble-dogfood-2026-10-13-10-02-10` · HEAD at run start: `42e1a51` ·
read-only lane: no board rows filed, no code changed. Findings are reported here
for the foreman to convert into rows.

## Angle

Every prior dogfood tick (09-17/20/24/25/28, 10-01, 10-10) exercised ingest,
scrub, ledger, dashboard, operator CLI, compose, fresh install and the QA
refusal surface. None touched **SPEC-11, the skill loop** — the feature the PRD
calls the reason the product exists (AC-25/AC-26). This run drove that surface
end to end the way the docs tell an operator to.

## Promise under test

"Set `[skills] enabled = true` with exactly one source key in the daemon config
(docs/operations.md §17, examples/config.toml comment block); the daemon pulls
the channel every `pull_interval` and installs signed artifacts that pass every
gate. `trouble skills status` shows installed/pending rows; `trouble skills
approve <name>@<v>` promotes a pending artifact."

## What I did (fresh-machine leg first)

Ephemeral bunker agent `dogf-trouble-1013` on **bunker-mvp** (bunker3/2/4 all
unreachable this tick — connect timeouts; mvp answered). Destroyed after.

- spawn 27s; source tar transfer 10s (51 MB); documented two-line stamped build
  from deploy/README ran verbatim: **85s total** (troubled 84s cold incl. the
  go1.26.9 toolchain auto-download from the agent's stock go1.22.2 — the
  GOTOOLCHAIN path the docs promise; trouble 1s warm).
- `bin/trouble --version` → `0.0.0-dev 31c8236 … [stamped]` (the tarball had no
  `.git`; per the docs I passed `GIT_SHA` explicitly — the documented
  source-transfer path works exactly as written).
- Config per quickstart; then enabled the loop exactly as docs/operations.md
  §17 and the examples/config.toml comment teach:

```toml
[skills]
enabled = true
source_path = "/home/<agent>/skills-release"   # a local git dir, one tag v1
require_signature = false                      # the documented local-channel form
approve = "review"
pull_interval = "1m"
pull_jitter_pct = 0
```

- Booted, waited >2 minutes of uptime (2 pull intervals), then inspected every
  place a pull could leave evidence.

## Result: the loop never pulls

Runtime evidence, all captured on the agent:

| Expectation (from the docs) | Observed |
|---|---|
| pull on boot (`pull_on_boot` default true) | repo/ cache dir **empty**, no `.git` inside |
| `pull-state.json` maintained | **file absent** — `markPull`/`fail` never ran |
| `skill` records in the ledger | **zero** (5412 daemon, 5404 event, 0 skill) |
| pending/ or installed/ artifacts | both **empty** |
| `pull_failures` / refusal records | none — no pull attempt was ever made |
| `/health.json` skills row | `built=true, refused=false` |
| boot log | zero lines mentioning skill or pull |

The subsystem **is constructed** and the `skills-local/` tree (candidates,
installed, pending, quarantine, repo, staging) is created at the documented
state-root path — but nothing ever schedules a pull. Static confirmation in the
working tree at `42e1a51`:

- `internal/skills.Skills.Run` / `Skills.PullOnce` have **zero non-test
  callers** anywhere (`grep -rn 'Skills\.Run(\|PullOnce(' --include=*.go` minus
  tests: nothing). `internal/app/daemon.go`'s background_loops phase starts
  Research, Issues and Flow — there is no `go d.Subsystems.Skills.Run(ctx)`.
- The ladder's skills stage (`Ladder.RunSkillStep`, `Ladder.SkillPlays`) and the
  `Resolver.Match`/`Hold` recurrence path likewise have zero non-test callers:
  even a pulled, installed, approved skill would never resolve against a
  recurrence and never play.
- `Outlet.Promote` and `Outlet.RequestHotfix` are declared on the ladder's
  Outlet interface and implemented by `deskOutlet`, but the state machine only
  ever calls `EnsureIssue`, `EnsureBoardRow` and `Comment` — the SPEC-08
  promote→skills-candidate chain is unreachable from the daemon.

## The operator surface half

`trouble skills status` (SPEC-11 §3.7; docs/operations.md §17) does not exist:

```
$ ./bin/trouble skills status
unknown command "skills"      → exit 2
```

`cmd/trouble` implements install/upgrade/config/topology/check-stall/escalate/
dashboard/hub/sensors — no `skills` verb. The approval flow the docs teach
(`approve = "review"` parks pulls until `trouble skills approve <name>@<v>`)
therefore has no entry point even if pulls ran.

## Findings (for the foreman to file as rows)

1. **P0 — the skill loop is dead code in the wired daemon.** The documented
   opt-in (`[skills] enabled=true` + one source) builds the subsystem, reports
   `built=true refused=false` on /health.json, and then does nothing: no pull
   on boot, no interval pull, no ledger record, no pull state. Time-to-first-
   success for the product's headline learning loop: **unbounded** (this run:
   never). Fix direction: start `Skills.Run(ctx)` in the daemon's
   background_loops phase next to Issues/Flow.
2. **P1 — the documented `trouble skills` CLI verbs are absent.** status/list/
   approve/reject/refusals/explain all exit 2 as unknown commands. With the
   default `approve=review` policy an installed artifact could only ever sit in
   `pending/`. Fix direction: implement the §3.7 verb family in cmd/trouble.
3. **P1 — the consume side is also unwired.** `RunSkillStep`/`SkillPlays`
   (ladder→library), `Resolver.Match/Hold` (recurrence resolution) and
   `Outlet.Promote`/`RequestHotfix` (flow→skills candidate sink) all have zero
   production callers. Wiring the puller alone would not close the loop; the
   ladder needs a skills stage edge and the state machine needs the promote
   call, or installed skills can never affect an incident.

Health honesty note folded into finding 1: `built=true` is truthful per AC-34's
pin (constructed, not refused) — but it means the operator's only signal that
the loop "works" is green while the loop is inert. If §3.3a ever grows a
"functional" dimension, this is the motivating case.

## What did work (credit where due)

- Fresh-machine install: verbatim quickstart build on a bare agent, stamped,
  booting, `/health.json` degraded only by the already-filed QA-TROUBLE-19
  token-store refusal. The daemon core remains solid (prior ticks agree).
- Config schema, refusal-on-invalid-source semantics (TROUBLE-SKILLS-001 boot
  refusal class), and the skills-local directory layout all behaved as specced.

## Performance

Headline operation this tick was "enable the loop and receive a skill" — it
never completes, so there is nothing to time but the wait itself (unbounded;
that is finding 1, not a PERF row). Everything measured on prior ticks
(ingest 85-291ms, dashboard <5ms, config explain 6.8ms) re-confirmed nothing
user-noticeable on this boot: boot-to-ready with the skills loop enabled was
indistinguishable from the 10-01/10-10 boots (<2s). No PERF rows filed — a win
nobody can feel is not a finding.

## Install leg record

`install_seconds=85 build (spawn 27 + transfer 10)` · `bunker=mvp agent=dogf-trouble-1013 destroyed` · `smoke=ok`
(health 200 + stamped --version; no SKIPPED row needed this tick)
