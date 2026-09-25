---
name: trouble-operator-cli-usage
description: >-
  How to drive the trouble operator CLI and dashboard token lifecycle without
  burning an hour: the token-store divergence trap (TRBL-081), the checked
  contracts that fail closed with named codes, and the fastest proofs that a
  daemon is healthy. Load when operating ~/trouble, minting dashboard tokens,
  or verifying a trouble daemon's health/auth.
version: 1.0.0
category: software-development
---

# Using trouble — operator CLI, tokens and dashboard (verified 2026-09-25, HEAD 92061f1)

The daemon (`bin/troubled`) serves; the operator CLI (`bin/trouble`) controls.
This skill covers the CLI + token + dashboard surface and the traps on it.
Companion skills: `trouble-usage` (boot), `trouble-sentinel-ingest-usage`
(ingest), `trouble-light-hub-usage` (hub), `trouble-compose-usage` (container).

## Checked contracts (all verified live, use these as smoke)

| command | expected | meaning |
|---|---|---|
| `trouble --version` | `VER SHA TIME [stamped]` | unstamped `[UNSTAMPED ...]` = no git stamp; install refuses without `--force` |
| `trouble config explain --key K [--json]` | value + winning source (`flag/env/file/default`) | cheapest live proof a key exists, binds nothing |
| `trouble topology` | zone ladder T1→T5 with governing keys | no daemon needed |
| `trouble check-stall --health-url URL --state-root DIR` | rc=0 fresh, **rc=8** TROUBLE-LIFECYCLE-008 on stale | the watchdog's own probe; rc=8 is its honest verdict, not a crash |
| `trouble dashboard token create --label L --scopes read[,write][,autonomy]` | plaintext `tdt_...` printed ONCE | store is hash-only, 0600 |
| `trouble dashboard token rotate --label L` | old dead immediately ("no grace window") | verify: old 401, new 200 |
| `trouble dashboard token revoke --label L` | dead within the same second | liveness-check it with curl |
| `trouble dashboard token list --json` | includes revoked tombstones (`revoked:true`) | audit trail by design, not a leak |
| `trouble install --dry-run` | fails closed rc=13 if `escalate.channels` empty | TROUBLE-LIFECYCLE-016: watchdog with no alarm channel refuses |

## The token-store trap (TRBL-081, open)

`troubled --dashboard-token_file <path>` boots the daemon reading THAT store (docs/cmd.md
showcases exactly this for scratch daemons), but `trouble dashboard token create` has no
store flag — it mints into the compiled default `~/.config/trouble/dashboard-tokens.json`.
Result: a valid-looking token that 401s forever, silently. **The working pattern:** keep the
daemon on the default store (pass `--dashboard-token_file /home/you/.config/trouble/dashboard-tokens.json`
or configure the same path in the config), mint, then curl:

```sh
curl -H "Authorization: Bearer $TROUBLE_DASHBOARD_TOKEN" http://127.0.0.1:7646/
```

Auth: Bearer on the API; `?token=` in a URL is refused (400) by design — tokens never ride URLs.

## Scratch daemon without editing the shipped config

```sh
troubled --state_root $HOME/.local/state/trouble-scratch \
         --ingest-bind 127.0.0.1:7645 --dashboard-bind 127.0.0.1:7646 \
         --secrets-environment_file /path/trouble.env
```

- State root CANNOT be under `/tmp` or `/var/tmp` (TROUBLE-LIFECYCLE-004 refuses; use
  `$HOME/.local/state/...`). This is documented in deploy/README.md — read it first.
- No `[projects]` in the config ⇒ sentinel plane refused ⇒ `/health.json` reports
  `status=degraded` with `detail.subsystem_refused=TROUBLE-LIFECYCLE-001`. That is the
  designed posture, not a bug; `subsystems[]` in health shows per-plane built/refused.
- Unknown `--key` flags are refused by name (rc=13, TROUBLE-LIFECYCLE-001). Table keys
  (`projects`, `issues`, `skills`, `llm`) cannot be set from argv at all.

## Checkout-free install (fresh machine)

```sh
go install github.com/trouble-agent/trouble/cmd/trouble@latest    # operator CLI
go install github.com/trouble-agent/trouble/cmd/troubled@latest   # daemon
```

Go ≥ 1.26 required. Result is `[UNSTAMPED (degraded; ...)]` by design — no git history to
stamp. Proven on a bare Debian box: Go tarball ~33s, each `go install` ~46s, then
`--help`, unknown-flag refusal (rc=13), `config explain --json`, `topology` all work.

## Perf reference (2026-09-25, warm)

Dashboard overview 7.5ms ± 0.7 (Bearer), `config explain` 7.1ms, `check-stall` 20ms,
`make bin` 2.4s warm. Nothing user-noticeable slow on this surface.
