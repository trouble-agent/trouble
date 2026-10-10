# Dogfood run — trouble — 2026-10-10 (tick 2026-10-10-00-29-24)

Lane: trouble-dogfood (read-only mutation contract — findings reported here, NOT filed to board; foreman files).

## Angle (why this run differs from 10-01)
Last run proved fresh-install + incident loop (TRBL-089/093/094). Since then product changed:
QA-TROUBLE-19 (dashboard refuses its data plane when token store missing, commit c12f64a) is NEW
and was never exercised as a user. This run took that surface + the operator token CLI.

## Install leg (ephemeral bunker — bunker-mvp; bunker3/4 unreachable, mvp answered)
- bunker3 (100.69.3.13) ssh connect timeout; bunker2 silent timeout; bunker4 timeout; bunker-mvp HOST_OK/bunkerd active → substituted per skill (sibling host IS the leg).
- bunker spawn --server bunker-mvp → agent 098fdeec (0.2.0 CLI, spawn OK ~5s).
- Clone from public origin https://github.com/trouble-agent/trouble: 2s, HEAD 129da13 — anonymous https clone works (public repo).
- Documented quickstart build: NO preinstalled make issue (agent box had go 1.26 already — note: a bare fresh user must install Go first, docs cover this explicitly).
- BUILD_SECONDS=81, stamped `129da13 129da13 [stamped]`.
- Quickstart config + 0700 state root + boot: /health.json 200 in <2s.

## Real use — the new refusal surface (QA-TROUBLE-19)
- Fresh boot with NO token store: dashboard answers **503 TROUBLE-DASHBOARD-013 "dashboard refused at boot: token store missing"** on every page, health reports `status=degraded, detail={subsystem: dashboard, subsystem_refused: TROUBLE-LIFECYCLE-001}`, ingest keeps serving. Structured, actionable — the feature works exactly as merged.
- Friction finding (DF-1): `trouble dashboard token create --config <file> --output-env <path>` prints usage and does NOTHING (no store created, no token, exit non-silent). The operator must know the daemon's resolved default path is `<state_root>/dashboard-tokens.json` (HOME-independent of config state_root? — here it landed under state root). Workaround: omit --config, or pass --token-file matching the daemon's resolved path. Cost ~3 failed attempts + reading --help. Docs don't mention the mint-after-boot ordering.
- Friction finding (DF-2): minting a token AFTER boot does not lift the refusal (refusal is evaluated at boot) — daemon must be restarted after first mint. Undocumented ordering: docs walk config→boot→verify but the first dashboard visit on a fresh install requires boot→mint→restart. A first-time user sees 503 with no hint that a restart is the cure.
- Friction finding (DF-3): the boot-time LOCK refusal (`TROUBLE-LEDGER-005 another writer holds ... LOCK`) fired when I restarted while the old process still drained — error text is excellent (names holder pid), but a kill-then-immediate-restart race is a normal operator path and costs a second attempt.
- Otherwise: full loop green. POST event → `{"id":"2d0b03bd..."}`; ledger carries event seq 536 with sig; auth triad on /incidents: 503(refused)/503→503 pre-restart; after restart-with-store: no-token 200? (health public), valid read token 200, groups 200; bad ingest key 401; SIGINT drain clean ("dashboard stopped", port freed, verified 000); restart → health ok, seq persisted (1596), my event still present, token still valid. `config explain` proves provenance (file row for sensors.sample_fold_window).

## Performance (Step 2b)
- Ingest warm: 0.17–0.40s per POST (incl. first), dashboard /incidents 1.1–1.4ms, /health <5ms.
- Cold quickstart loop on the bunker: clone 2s + build 81s + boot ~2s + first event 200 ~0.29s.
- CLI (control host): `trouble config explain` 6.8ms±0.5, `--version` 6.5ms±0.5 (hyperfine 20 runs).
- VERDICT: nothing slow enough for a user to notice → NO PERF row (per skill: a win nobody can feel is not a finding). Build 81s is the only wait and is expected for a Go build.

## Verdict
🟢 SHIPPABLE. Promise (self-hosted incident brain: sensors + Sentry-compatible ingest + ledger + dashboard) holds end-to-end on a fresh anonymous clone. Friction = 3 documented-ordering gaps (DF-1..3 above), all P2-class.

## Recommendation to foreman (file as rows)
- TRBL-NEW-A (P2): `dashboard token create --config X --output-env Y` silently no-ops; should either mint into the resolved store or error naming the store path it would use.
- TRBL-NEW-B (P2): QA-TROUBLE-19 refusal needs a remediation hint: 503 body/health detail should say "mint a token (`trouble dashboard token create`) and restart" — first-run user currently must infer the restart.
- TRBL-NEW-C (P3): deploy/README quickstart should add the post-boot mint + restart step (or document mint-before-boot), so the first dashboard visit is not a 503.

## Evidence pointers
- Agent 098fdeec destroyed (verified: destroy confirmed). No repo/board/permission changes made (read-only lane).
- HEAD at run start: 129da13.
