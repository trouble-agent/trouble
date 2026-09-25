# Dogfood integration report — operator CLI, token lifecycle and dashboard (2026-09-25)

Tick: trouble-dogfood 2026-09-25-16-12-39. Surface chosen by the "change the angle" rule:
prior runs swept the sentinel ingest plane (09-24), the compose container path (09-25 morning)
and the native quickstart (09-17/09-24). This run used the surfaces those runs never touched:
the **operator CLI** (`trouble`), the **dashboard token lifecycle**, and the **dashboard pages**
as a token-carrying operator sees them. All on HEAD 92061f1, rebuilt binaries (build 2.4s warm).

## What was used, for real

1. `make bin` from a dirty tree → `92061f1-dirty 92061f1 [stamped]` in 2.4s (warm).
2. The documented scratch-instance form from docs/cmd.md — flags only, no config edit:
   `troubled --state_root ... --ingest-bind 127.0.0.1:7645 --dashboard-bind 127.0.0.1:7646
   --dashboard-token_file ... --secrets-environment_file ... --ingest-advertised_host localhost`.
   It booted first try. One self-inflicted refusal: I first used a `/tmp` state root and got
   `TROUBLE-LIFECYCLE-004 ... resolves under forbidden root "/tmp"` — deploy/README.md had
   already told me not to do that. Docs were right; I had skipped reading them.
3. Health: `status=degraded`, reason spelled out in `detail.subsystem_refused` — I booted with
   no `[projects]` (a table key, argv-refused by design), so the sentinel plane is refused.
   `subsystems[]` shows per-plane built/refused codes. Six sensors green.
4. Operator CLI: `config explain --key K` (defaults with provenance), `topology` (the
   zone-ladder with its governing keys), `check-stall` (on a live daemon: rc=8
   TROUBLE-LIFECYCLE-008 heartbeat_stale, seq_age honest).
5. Token lifecycle: create → use (Bearer 200) → rotate (old 401 immediately, "no grace
   window") → revoke (liveness verified: 200 before, 401 within the same second after).
   Revoked tokens stay listed as tombstones with `revoked:true` — audit trail, hashes only
   in the store (0600, plaintext shown once).
6. Dashboard pages with Bearer: /, /incidents, /groups, /rules, /breakers → 200, 2.8–8.3 KB,
   0.8–1.3 ms each. Real data: 12 open incidents from live sensors.
7. Security contract: wrong token 401, no token 401, token-as-URL-param 400 (session/cookie
   never URL tokens — T2→T3 ladder), `trouble install --dry-run` fails closed rc=13
   (TROUBLE-LIFECYCLE-016: empty escalate.channels = watchdog with no alarm channel).
8. README's `go install .../cmd/trouble@latest` + `cmd/troubled@latest` — proven on a bare
   Debian bunker agent (install leg, below).

## The finding (TRBL-081)

docs/cmd.md advertises `--dashboard-token_file` as an argv-addressable token STORE for scratch
daemons. The token CLI (`dashboard token create/list/rotate/revoke`) has no counterpart flag:
it always mints into the compiled default path. Boot the daemon on a different store (the exact
pattern the docs showcase), mint a token, and every request 401s with zero indication that the
CLI and the daemon are reading different stores. Silent divergence between two documented
contracts. Filed as TRBL-081 (P2); not fixed — dogfood files rows, the foreman works them.

## Numbers (Step 2b)

| operation | warm | cold/one-shot |
|---|---|---|
| dashboard overview page (Bearer) | 7.5 ms ± 0.7 (20 runs) | first hit 7.5 ms |
| `trouble config explain --json` | 7.1 ms ± 0.6 | — |
| `trouble check-stall` (live health) | 20 ms | rc=8 as expected |
| `make bin` (warm, dirty tree) | 2.4 s | — |
| bunker agent `go install cmd/troubled@latest` | — | 46 s (after Go bootstrap) |
| bunker Go bootstrap (tarball, agent-local) | — | ~33 s download+extract |

Nothing here is slow enough for a user to notice; no PERF row filed (a win nobody can feel is
not a finding). The headline operations are all sub-30ms.

## Install leg (ephemeral bunker, las-bunker-03, agent 5c261866, destroyed)

New angle vs. the 09-25 morning run (which proved clone+compose): this leg proved the README's
**checkout-free** path. Bare Debian (no Go, no toolchains): Go 1.26.5 tarball →
`go install github.com/trouble-agent/trouble/cmd/troubled@latest` (46s) →
`go install .../cmd/trouble@latest` → smoke: `--help` OK, unknown flag refused rc=13,
`config explain --key ingest.bind --json` emits provenance, `topology` OK,
`--version` = `0.0.0-dev unknown [UNSTAMPED (degraded; ...)]` — exactly the documented posture
for a checkout-free install (there is no git history to describe). Both binaries from the
public module path with zero checkout. No repo visibility or permission was touched.

Friction notes on this leg (documented-trap class, not new rows): the agent-local `/tmp`
write inside a piped script failed while an interactive curl to `$HOME` worked — use
agent-local `$HOME` for throwaway downloads; and my first launch used `nohup ... &` inside
ssh, which backgrounds the whole `&&` chain and produces an empty log. `setsid` in its own
statement, per the established pattern.

## Verdict

🟢 SHIPPABLE on this surface. The CLI is genuinely operator-shaped: every subcommand I tried
either worked first try or failed with a named error code that said what to fix. The one real
gap is the token-store divergence (TRBL-081) — it bites exactly the user the scratch-instance
docs target.
