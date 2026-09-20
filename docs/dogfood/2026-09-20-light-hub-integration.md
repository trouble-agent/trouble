# Dogfood integration report — trouble, light-hub + fresh-install angle (2026-09-20)

Lane: `trouble-dogfood` (skill `coding-hermes-dogfood`). Target: `~/trouble`, HEAD `43fd5ca`,
binaries rebuilt stamped (1.0s). Verdict: **🟡 PROMISING-BUT-ROUGH** — the light-hub runtime is
real and its headline promise reproduces end to end; the operator surface around it (docs,
CLI verbs, container quickstart auth) is where the defects are.

## Why this angle

Two prior dogfood runs are already in `.coding-hermes/dogfood-log.md`: 2026-09-17 walked the stock
config + CLI + dashboard surface, and a second 09-17 run targeted the QA lane. Both had to file
`SKIPPED-install-bunker` because `bunker spawn` returned `deadline_exceeded`. So this run took the
surfaces those two did NOT touch:

1. the **light-hub profile** (SPEC-13 — the v0.1.1 flagship: Redis stream, consumer group, dedup
   gate, degradation/recovery) — never driven by a dogfood run before;
2. the **install leg** — never actually executed before (bunker CLI is now 0.1.4 and `bunker spawn`
   worked on the first try against `agent-host-3`).

## The promise (null hypothesis)

From README.md + deploy/README.md: *a user can clone/transfer trouble, `make bin`, boot it against
`deploy/container/config.light-hub.toml`, and get a Sentry-compatible incident brain whose ingest
plane survives a Redis outage — "degrades to standalone ingestion, no event loss, no restart" —
then recover without a restart.*

## What I actually ran

### A. Light-hub runtime on the control host (real use)

```
docker run -d --name trbl-df-redis2 -p 127.0.0.1:7699:6379 redis:7.4-alpine \
  redis-server --appendonly yes --maxmemory-policy noeviction
cp deploy/container/config.light-hub.toml /tmp/dogfood-trouble/config.light-hub.toml
sed -i 's|redis://redis:6379/0|redis://127.0.0.1:7699/0|' .../config.light-hub.toml
./bin/troubled --config /tmp/dogfood-trouble/config.light-hub.toml \
  --config_path /tmp/dogfood-trouble/config.light-hub.toml \
  --state_root ~/.local/state/trouble-df-hub \
  --secrets-environment_file /tmp/dogfood-trouble/trouble.env \
  --dashboard-token_file ~/.local/state/trouble-df-hub/dashboard.token \
  --ingest-bind 127.0.0.1:7661 --dashboard-bind 127.0.0.1:7662 \
  --ingest-advertised_host trouble.example.net -v
```

Every container-shaped path in that file was overridden **by flag** (the documented §2.5a form);
nothing in the repo was edited.

**Result — the hub is live, not a stub.** `/health.json` carries a full stanza:

```
hub.profile=light-hub  enabled=true
hub.redis: stream=trouble:ingest group=ledger-writers aof=true policy=noeviction
           dedup_window=redis stream_len/pending/lag
hub.archive_queue=0  hub.degraded=false
subsystems: sentinel/issues/research/flow/skills all built=true refused=false
```

**Real workflow, not a test suite.** One on-ramp POST
`POST /api/1/event/?sentry_key=<public_key>` with `{"message":...,"level":"error"}` returned
`200 {"id":"..."}`, and the ledger gained `event` + `group` + `incident` records **on one sig**
(`sentinel:sha256v1:7834cf2f02e630b8`); the Redis stream was acked (`XPENDING 0`). Posting the same
message twice collapsed into ONE `group` (fingerprint dedup at the group level), while two `event`
records persisted — correct per SPEC-04 (events are never dropped), and automatic `ForwardEnvelope`
idempotency is a stated boundary, not a defect.

**SPEC-13 §4.3 degradation + recovery — reproduced, measured:**

| step | observed |
|---|---|
| `docker stop` the wired redis | `/health.json` → `status=degraded`, `hub.degraded_reason=redis_unavailable`, `hub.redis.dedup_window` flips `redis`→`lru`, one `redis_lost` record, `TROUBLE-HUB-004` |
| POST while down | `429` with `Retry-After: 1`, **zero** ledger records — exactly the §4.3 `require_redis=false` row ("senders see 429; local spools hold") |
| `docker start` (no daemon restart) | recovered in **t=4s**, `status=ok`, `dedup_window=redis`, one lifecycle `redis_restored`, next POST `200`, stream acked (`XLEN 12`, `pending 0`) |
| daemon liveness across the outage | kept serving `/health.json` 200 throughout; never restarted |

**Dashboard as a real user.** `trouble dashboard token create` minted a 47-char `tdt_` token;
with the token the `/incidents` page returned 200 and rendered real incident ids (`inc_01M2ZHN...`).

### B. Fresh-machine install leg (ephemeral bunker, first time it ever ran)

`bunker spawn --server agent-host-3 --ttl 2h` → agent `215f3511` (destroyed at the end).
Source transferred with `git archive HEAD` (2.8 MB) — the repo has no remote, so this is the
documented "checkout supplied by your own source-transfer process" case.

```
go.mod requires go 1.26.0 ; box had NO Go (README prerequisite, not provisioned)
curl go1.26.5.linux-amd64.tar.gz ; GO_SETUP_SECONDS=27
make bin   -> MAKE_BIN_EXIT=0, INSTALL_SECONDS=25
   bin/trouble 7.0M   bin/troubled 21M   (both UNSTAMPED: 0.0.0-dev unknown)
```

Then the deploy/README.md **canonical quickstart, verbatim** (copy `examples/config.toml`, the
three documented edits, boot, curl):

```
READY_AFTER=3s   health.json 200
first event  -> {"id":"b2381471be20fcfdeb4cfbe29222b5d0"}  HTTP=200
documented jq proof ->  event  sentinel:sha256v1:837a01c50fddf0d8
                        group  sentinel:sha256v1:837a01c50fddf0d8     <- the acceptance
SIGINT -> stopped cleanly (dashboard stopped; drain worked)
```

Dashboard on the fresh box: the CLI default store and the daemon default agree there, so the
documented mint+read path works (`health 200`, `/incidents` 200).

### C. Documented container quickstart (compose)

`GIT_SHA=43fd5ca docker compose build` = 34s on a scratch copy of the compose file; `up -d` brought
`trouble` + `trouble-redis` up; the documented seed step printed
`wrote TROUBLE_DASHBOARD_TOKEN to /data/state/trouble.env (0600)` and the container healthcheck
went healthy — so the documented health command works (`200`, `git_sha=43fd5ca`, token extracted
via `docker cp` because the image is distroless). **The documented first-event POST failed:**
`401 {"causes":["query_key_remote"],"detail":"query-string key refused on a non-loopback request"}`
from the host itself (see TRBL-062).

## Findings filed (board rows, not fixed here)

| id | prio | finding |
|---|---|---|
| TRBL-062 | P1 | container quickstart's documented first event cannot authenticate: a published-port bind refuses the query-string key, and the shipped container project has no `secret_key`; README's "reporters OUTSIDE this host" is wrong for a `0.0.0.0` bind |
| TRBL-063 | P2 | minting against a config that declares `dashboard.token_file` silently writes the CLI's default store → token 401s with no feedback; `token create` never prints the store path |
| TRBL-064 | P1 | `docs/operations.md` §14 still says `internal/hub` is NOT built and a light-hub boot has no hub stanza — contradicted by the same tree |
| TRBL-065 | P2 | SPEC-13 §2.2's four operator verbs (`trouble hub status|archive|dedup|drain`) do not exist (`unknown command "hub"`) |
| TRBL-066 | P2 | a no-`.git` transfer makes `make bin` produce an UNSTAMPED pair and the fresh-machine path never says so |

Install leg: **RAN** (not SKIPPED) — `install_seconds≈25` (build) + 27s Go bootstrap, smoke **passed**
on the fresh box. A previous `SKIPPED-install-bunker` row (TRBL-011) is now superseded by this run:
the blocker was `bunker spawn` `deadline_exceeded` on 0.1.3; bunker 0.1.4 spawned on the first try.

## Honest boundaries of this run

- The blob-level `ForwardEnvelope` duplicate-key collapse (SPEC-13 AC29's "1 record for the
  duplicate key") was **not** exercised: the generic-JSON on-ramp carries no idempotency key, so
  the Redis `SET NX` dedup window never engages for it (`dedup_hits=0, dedup_misses=3`). The
  claim I verified is the GROUP-level collapse (2 identical events → 1 group, 1 incident).
- `server.duckbrain.endpoint` was left empty on purpose, so the archival tier parked with
  `TROUBLE-HUB-009` while ingestion served — the designed split, not a failure. Archival against a
  live DuckBrain service remains unverified here (consistent with the TRBL-029 boundary list).
- Two of my own probe rounds were invalidated by harness mistakes (`--rm` deleted the redis on
  `stop`; a second redis failed to start on an allocated port). Both were discarded and the final
  outage test was run against the redis the daemon was actually wired to. Recorded in
  `diagnostics.md` so the next runner does not repeat them.

## Reproduction script

`docs/dogfood/2026-09-20-repro-light-hub.sh` — boots redis + the light-hub instance on 766x ports,
posts one event, stops/starts redis and prints the same health fields this report quotes, then tears
everything down. It writes only under `~/.local/state/trouble-dogfood-repro` and never touches the
repo or ports 7643/7644.
