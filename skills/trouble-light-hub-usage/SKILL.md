---
name: trouble-light-hub-usage
description: >-
  How to boot, operate and verify the trouble light-hub profile (SPEC-13) on a host or in the
  container stack — the operator commands, the health fields that matter, the outage/recovery
  contract, and the traps that waste an hour. Load when working on the trouble repo, running a
  light-hub instance, or verifying SPEC-13 behaviour.
version: 1.0.0
---

# Using trouble — light-hub profile

`trouble` is an incident brain: sensors + a Sentry-compatible sentinel feed an append-only ledger
through a dedup ladder. The **light-hub** profile puts a Redis stream + consumer group in front of
that same ledger writer so a fleet of satellite hosts can queue into one writer. Everything below is
measured on HEAD `43fd5ca` against redis 7.4.11.

## Entry points

| what | how |
|---|---|
| build both binaries | `make bin` (stamped). NEVER a bare `go build` — an unstamped `bin/` reds the smoke paths for the wrong reason |
| daemon | `bin/troubled --config <file> --state_root <dir> ...` (boots AND serves) |
| operator CLI | `bin/trouble config explain [--key K] [--json]` — resolves and prints provenance without binding |
| health | `GET /health.json` (dashboard bind; `status`, `subsystems[]`, `hub` stanza) |
| ingest on-ramp | `POST /api/{project_id}/event/?sentry_key=<public_key>` (generic JSON dialect) |
| container stack | `GIT_SHA=$(git rev-parse --short=7 HEAD) docker compose up -d` |

`trouble config explain` is the cheapest live proof that a registry key exists — `cmd/troubled` takes
only `--config/-v/--version` plus per-key flags and will HANG in the foreground because it serves.

## Boot a light-hub instance on a host (the recipe that works)

The shipped `deploy/container/config.light-hub.toml` is CONTAINER-shaped (`/data/state/...`,
`0.0.0.0`). It boots on a host only if every path-like key is overridden by flag:

```sh
docker run -d --name trbl-redis -p 127.0.0.1:7699:6379 redis:7.4-alpine \
  redis-server --appendonly yes --maxmemory-policy noeviction     # NO --rm
cp deploy/container/config.light-hub.toml /tmp/hub.toml
sed -i 's|redis://redis:6379/0|redis://127.0.0.1:7699/0|' /tmp/hub.toml
mkdir -p ~/.local/state/hub-scratch && chmod 0700 ~/.local/state/hub-scratch

bin/troubled --config /tmp/hub.toml --config_path /tmp/hub.toml \
  --state_root ~/.local/state/hub-scratch \
  --secrets-environment_file /tmp/hub.env \
  --dashboard-token_file ~/.local/state/hub-scratch/dashboard.token \
  --ingest-bind 127.0.0.1:7661 --dashboard-bind 127.0.0.1:7662 \
  --ingest-advertised_host trouble.example.net -v
```

Rules that are not optional: the state root must be under `$HOME` (**`/tmp` is refused twice** —
`TROUBLE-LIFECYCLE-004` and the hard-coded `TROUBLE-LEDGER-012`) and mode 0700; ports 7643/7644
belong to the real instance, use 766x for scratch; a container config's `dashboard.token_file`
otherwise resolves to a path that cannot exist and you get a silent empty token store.

## Mint and use a dashboard token

```sh
TROUBLE_DASHBOARD_TOKEN_FILE=<the same path the daemon resolves> \
  bin/trouble dashboard token create --label me --scopes read --output-env /tmp/dash.env
```

The `tdt_` token is shown ONCE. `TROUBLE_DASHBOARD_TOKEN_FILE` must agree with the daemon's
`dashboard.token_file` or the token 401s with no explanation (TRBL-063). On a stock host config
(both sides unset) they agree by default and the mint just works.

## The health fields that matter

- `status` — `ok` only when every subsystem reports built; `degraded` carries `detail.reason`.
  A perfectly working fresh box can read `degraded` with `unstamped_build` or `sensor_degraded: psi`.
  **Read `detail.reason` before treating `degraded` as a defect.**
- `subsystems[]` — `built`/`refused` per subsystem. Five rows; a `built=false` forces `status != ok`.
- `hub` — the light-hub stanza: `profile`, `enabled`, `degraded`, `degraded_reason`,
  `archive_queue`, `route_counters{A,B}`.
- `hub.redis` — `stream`, `group`, `consumer`, `stream_len`, `pending`, `lag`, `dedup_*`,
  `aof`, `policy`, `evicted_keys`, `dedup_window` (`redis`|`lru`).
  **A missing `hub` stanza on a `light-hub` boot means the listening socket exists but the hub
  runtime has not started yet — poll before posting.**

## The outage contract (SPEC-13 §4.3) — do not misread it

| event | senders see | daemon | ledger | code |
|---|---|---|---|---|
| Redis lost at runtime (`require_redis=false`) | **429 + `Retry-After`**; local spools hold | keeps running, drains in-flight batch | intact | 004 |
| same, `require_redis=true` | 503 + `Retry-After` | keeps running | intact | 004 |
| Redis returns | 200; spool flushes through the same path | rewires the consumer | intact | lifecycle `redis_restored` |
| DuckBrain unreachable | unchanged (ingestion is never gated on archival) | archive queue grows | intact | 010 |

A 429 during an outage is the CONTRACT, not a bug — the "no event loss" guarantee belongs to the
sender's spool. Measured: stop → `degraded/redis_unavailable`, `dedup_window` flips `redis`→`lru`,
one `redis_lost`; restart → `status=ok` in ~4s, `redis_restored`, next POST 200, stream acked.
Verify recovery by polling `/health.json` to `status=ok`, never by re-issuing a POST immediately.

## Verifying that ingestion actually worked (the proof, not the HTTP code)

A 200 is not the proof. Check the LEDGER for the pair on ONE sig:

```sh
jq -r 'select(.kind=="event" or .kind=="group") | [.kind, .sig] | @tsv' <state_root>/ledger/*.jsonl
docker exec <redis> redis-cli XLEN trouble:ingest
docker exec <redis> redis-cli XPENDING trouble:ingest ledger-writers   # 0 = acked
```

Posting the same message twice collapses into ONE `group` + ONE `incident` with two `event` records —
that is the designed posture (events are never dropped), not duplication. The automatic
`ForwardEnvelope` idempotency path is NOT reached by the generic-JSON on-ramp (no idempotency key is
carried), so `dedup_hits` stays 0 for that dialect.

## Traps (each one cost a real run)

- Rebuilding redis with `--rm` deletes it on `stop`, so a later `docker start` silently fails and you
  "prove" broken recovery. Never `--rm` a container you intend to restart.
- A second scratch redis that failed to start leaves the script talking to the OLD one — an all-green
  outage test in which nothing went down. Assert the container/port identity before stopping it.
- The background wrapper's pid is not the daemon; kill the listener pid from `ss -tlnp` or
  `heartbeat.json`, never `pkill -f <pattern>` (it matches your own shell).
- The image is distroless: no `sh`. Use `docker cp` to read state out, and the documented
  `docker compose run --entrypoint /usr/local/bin/trouble ...` form to mint inside.
- After a scratch compose run, `docker compose -f <copy> down -v` may target the wrong project name
  while the containers keep running — `docker rm -f` them by name and verify the ports are free.
- Flag spelling: `a.b_c` → `--a-b_c` (dot becomes a dash, underscore kept).
- On a published (non-loopback) ingest bind the query-string `sentry_key` is REFUSED
  (`query_key_remote`) even for a client on the same host; that path needs `secret_key` +
  `X-Sentry-Auth` (TRBL-062).

## Known gaps at this revision

SPEC-13 §2.2's four operator verbs (`trouble hub status|archive|dedup|drain`) are NOT mounted — the
CLI answers `unknown command "hub"` (TRBL-065), so a pre-migration drain has no supported path.
`docs/operations.md` §14 still describes the hub runtime as unbuilt (TRBL-064).
