# Dogfood 2026-10-01 — fresh-machine install + incident/dashboard loop (bunker leg)

Tick: trouble-dogfood-2026-10-01-17-02-42 · HEAD `e2b5b6d` · verdict: **SHIPPABLE (install leg RAN; one new P1)**

## Angle

Prior runs swept sentinel ingest (09-24), compose (09-25), operator CLI + token lifecycle
(09-25b), and a bare local daemon boot (09-28, which SKIPPED the bunker install leg —
TRBL-089). This run took two surfaces at once:

1. **The skipped install leg on a fresh machine** — closing TRBL-089.
2. **The ladder → incident → dashboard loop** as a first-time operator on that fresh
   machine, which no prior run had exercised on a clean host.

## Install leg (fresh machine, las-bunker-03, agent 4812a03c — destroyed after)

Transfer: the repo is private and the agent has no credential, so per the skill's
unattended rule the tree moved as a **git bundle** (20.8 MB, size-verified both sides),
cloned inside the agent, `origin` set to the public URL form. Honest caveat: this is a
bundle-based clone, not a network fetch.

Steps, all from the canonical quickstart (`deploy/README.md`):

| Step | Seconds | Notes |
|---|---|---|
| Go 1.26.5 tarball (README-documented prerequisite) | 31 | `go.dev/dl` per README |
| `make bin` (stamped build) | 39 | `[stamped]`, sha e2b5b6d |
| config/env/state setup + edits | 0 | sed edits of the copied examples |
| daemon → healthy | <1 | `/health.json` 200, status ok |

Smoke: first event POST 200 (**cold 291 ms**), ledger proof (1 event record, sig
grouped, 4 group records on one sig), bad-key 401 (fail-closed), clean SIGTERM-ish stop
("dashboard stopped" logged), restart → ledger continued (seq 193+), pre-restart token
still authenticated. **PASSED.**

Friction notes (minor, mostly fresh-agent artifacts, not product defects):

- `/tmp` is root-owned on the bunker agent — first build log write failed with
  `Permission denied` until redirected to `$HOME`. Not a trouble bug.
- `head`/`tail` absent on the bare agent (the quickstart only needs curl+jq, correctly).

## The incident loop on a clean host — what a first-time operator actually sees

Boot on a headless agent immediately produces a **noise storm**: the dbus sensor emitted
36 events in the first ~8 s; the ladder opened 22 incidents in ~5 s; the
`rule:dbus_unit_failed` storm breaker tripped within the first minute ("21 incidents
inside 5m0s"). The breaker worked exactly as designed — but every fresh install therefore
starts with an open breaker and ~20 noise incidents as the first thing the dashboard
shows. Filed as **TRBL-094 (P2)** (incident-level consequence of TRBL-078's sensor-noise
row, one rung up).

The deliberate first POST grouped correctly: 4 identical events → **1 event record + 4
group records on one sig** (event-level dedup at the group layer; the ledger-side event
count stays 1). TRBL-074 (no event-level dedup at ingest) was NOT re-tested with a repeat
burst at the ack layer; the group upsert deduped everything downstream.

Dashboard: incidents/groups/rules/breakers all 200, **0.9–4.3 ms**, read token scope
enforced, no-auth 401, URL-token refusal per docs. The groups page rendered our test
event alongside the boot noise with real counts and rates — the read-mostly face works
on a clean machine with zero operator config.

## The P1: mint-while-daemon-down token is dead on arrival

Filed as **TRBL-093 (P1)**. A token minted while the daemon was NOT running answers 401
`TROUBLE-DASHBOARD-002` forever after boot, even though the store is well-formed (0600,
hash verified equal to `sha256(token)[:64]`). Tokens minted while the daemon is up work
immediately, survive restarts, and keep working. One narrow broken arm; needs source
triage of the CLI mint write vs `internal/dashboard/tokens.go` load/refresh.

Secondary friction recorded in the same row: the plaintext is printed exactly once with
no retrieval path; scripted mints fumble it (observed twice this run: a heredoc ate
`$TOK` literally; a grep truncation clipped a token containing `-`).

## Perf (Step 2b)

- First event **cold 291 ms, warm 85–206 ms** — the ack-after-fsync window by design;
  nothing user-noticeable slow.
- Dashboard pages 0.9–4.3 ms. Health <20 ms. **No PERF rows** — nothing a user would
  feel. (Install: Go download 31 s + build 39 s are the honest fresh-machine price and
  match prior runs' numbers.)

## What was left behind

- This report: `docs/dogfood/2026-10-01-fresh-install-incident-loop-integration.md`
- `docs/dogfood/diagnostics.md` — appended run section
- `skills/trouble-usage/SKILL.md` — updated with the fresh-machine path + token gotchas
- Board: TRBL-093 (P1), TRBL-094 (P2); TRBL-089 closed with install-leg evidence
- `.coding-hermes/dogfood-log.md` — run entry
- Scratch state: agent destroyed (`bunker destroy` verified — no agents remain on the
  server), no repo visibility/permission touched, tokens were agent-local and died with
  the agent.
