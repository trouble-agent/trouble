# trouble real-use integration — 2026-10-29

Tick: trouble-dogfood-2026-10-29-11-40-58
Target revision: 50866ae4a232969fee2a354927ce7e298e5955be
Canonical checkout: /home/kara/trouble

## Promise

trouble claims that an operator can build the stamped daemon and CLI, boot a configured incident brain, send Sentry-compatible events, inspect incidents/groups/rules/breakers through the dashboard, and retain the records in its append-only ledger.

## Real workflow

The probe used a detached clean worktree at the target revision and a state root under /home/kara/.local/state/trouble-dogfood-20261029-114058. It created a 0600 environment file and token store, set explicit absolute paths, built with `make bin`, started `bin/troubled`, waited for `/health.json`, authenticated the dashboard with the minted token, sent three Sentry-compatible generic JSON events, counted the ledger records, and stopped the daemon with SIGTERM.

Observed:

- `make bin`: exit 0, 0.75–1.09 seconds across two runs; binary reported `50866ae 50866ae ... [stamped]`.
- Daemon readiness: 1.41 seconds in the first probe; the corrected probe returned health status `ok`, version and git_sha `50866ae`, and a non-zero ledger sequence.
- Dashboard: `/incidents`, `/groups`, `/rules`, `/breakers`, and `/partials/budget` all returned HTTP 200. Measured response times were 14–33 ms, with non-empty bodies.
- Ingest: all three POSTs returned HTTP 200 and an incident id. Measured times were 537 ms, 1,897 ms, and 1,203 ms. The spread is material, but this short run does not separate product contention from host load, so no PERF row is filed.
- Ledger: 55 JSONL records were present after the three-event workflow, including 47 event, 3 group, 3 incident, 1 config, and 1 lifecycle record. This proves the writes reached the ledger; it does not claim that every record was independently verified by the CLI.
- Shutdown: the daemon process was signalled and the port was released; the daemon log ended with dashboard stopped and the expected closed-network listener message.

The first local attempt used an invalid `/tmp` state root and was correctly refused with `TROUBLE-LIFECYCLE-004`; it was discarded as harness/input evidence. The corrected run moved the state root under the operator home before classifying product behavior.

## Value judgment

Verdict: PROMISING-BUT-ROUGH.

The core boot → authenticated dashboard → ingest → ledger workflow works at this revision and is useful for a real operator. The main friction is operational: the documented surface is broad, the default/example configuration requires careful path and credential setup, and the CLI verification command was not exercised successfully in this probe because the attempted global `--state-root` spelling is rejected by the CLI. The existing usage skill and board rows already document the historical ledger-verification and configuration traps; this run does not create a duplicate finding.

Performance is acceptable for dashboard reads and build/startup. Ingest is sub-second to roughly two seconds in this short sample; the slowest request is user-visible, but the sample is too small and host-sensitive to attribute it to a new regression.

## Install leg

Status: PARTIAL/UNPROVEN, not PASS and not SKIPPED.

`bash ~/.hermes/scripts/bunker-qa.sh launch /home/kara/trouble` was invoked against `bunker-mvp`. The helper launched agent `ae53cc5e` and recorded it in `/tmp/trouble-bunker-launch-20261029.log`, but the launch transcript stopped at `CELL fresh-install: sync repo → ~/qa-trouble`; no controller evidence file was produced, and `bunker info ae53cc5e --server bunker-mvp` later returned `not_found`. A fresh `bunker list --server bunker-mvp --status all` showed no `ae53cc5e`. Because the launch path did run but no install/smoke cell became gradeable, this is not an install PASS and not a fleet-wide SKIPPED-install-bunker condition. The next install run must collect per-cell evidence before teardown.

A different live agent (`d2da6646`) was present but belonged to a chimera-v2 battery and was not touched.

## Reusable operation

Use an absolute state root below the operator home, an explicit absolute environment file, and an explicit dashboard token file. Use `make bin` for the stamped artifacts. Treat a 200 response as only one part of the proof: inspect the health identity and ledger JSONL after ingest, and keep dashboard token creation and daemon startup on the same resolved token path.

ch:trace tick=trouble-dogfood-2026-10-29-11-40-58 spec=docs/qa/use-cases.md#UC-001,docs/qa/use-cases.md#UC-005,docs/qa/use-cases.md#UC-006 test=/tmp/df-trouble-run-20261029.sh evidence=/tmp/df-trouble-real-20261029-114058 commit=50866ae4a232969fee2a354927ce7e298e5955be verdict=PROMISING-BUT-ROUGH
