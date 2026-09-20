# Dogfood diagnostics — light-hub path, and how to run this leg without the mistakes I made

2026-09-20, `trouble-dogfood` lane. Not a log dump: it explains how the light-hub subsystem is
wired, why each trap exists, the errors I hit, and the right way to do the thing. Read together with
`docs/dogfood/2026-09-20-light-hub-integration.md` (what I ran and measured). The 2026-09-17 run's
trail lives in `docs/dogfood/diagnostics.md` (stock-config/CLI/dashboard surface) — this file is the
light-hub + install-leg companion and deliberately does not repeat it.

## 1. How the light-hub path is assembled (so you can boot one without guessing)

`server.profile = "light-hub"` is validated at **boot step 6c, BEFORE the bind preflight** — a bad
profile produces one `boot_refused` ledger record and zero HTTP responses, so "the daemon is dead"
and "the config was refused" look identical from outside. The composition root then opens the Redis
runtime (queue + consumer group) and mounts `hubSink.Ingest` **in front of** the sentinel's ledger
sink, which is why an event posted to the on-ramp reaches the ledger only after the consumer appends
and acks (ack-after-fsync). Readiness has two stages: the listener can be BOUND before `Serve` /
`hub_boot` completes. **Poll `/health.json` for the `hub` stanza before posting** — an early POST
otherwise lands on a dead request context and produces a bogus `redis_lost` record with an empty
reason.

State that matters: `redis.stream`/`redis.group` are registry defaults (`trouble:ingest` /
`ledger-writers`); `require_persistence`/`check_policy` make `appendonly=no` a boot refusal
(`TROUBLE-HUB-016`) and a non-`noeviction` policy a flagged misconfiguration (`TROUBLE-HUB-015`).
The daemon READS those via INFO and never sets them — they belong to the redis service, which is why
a bare `redis:7-alpine` without those two flags is the wrong thing to test against.

## 2. Errors I hit, and the right way for each

**2.1 `--rm` + `docker stop` silently deletes your Redis.**
I started redis with `docker run -d --rm ...`. `--rm` removes the container on stop, so
`docker start <name>` had nothing to start — and my script swallowed the error. I spent a round
concluding "recovery does not work after 60s" from a redis that did not exist. **Right way:** never
put `--rm` on a container you intend to stop/start; confirm with
`docker ps -a --filter name=<c> --format '{{.Names}} {{.Status}}'` before reading a failure as
product behaviour. Harness bug class — check it first.

**2.2 Port collisions on the scratch redis port turn a test into a false PASS.**
A second scratch redis failed with `Bind for 127.0.0.1:7699 failed: port is already allocated`, and
the script kept talking to the FIRST redis — yielding an all-green "outage test" in which nothing was
ever down (`stream_len` kept climbing, `redis_lost` never appeared). **Right way:** assert identity
before the test — `docker ps -a --format '{{.Names}} | {{.Ports}}'` plus a ping on the exact port
`server.redis.url` names. A test whose premise never happened is worse than no test.

**2.3 The daemon pid is not the pid you launched.**
A background launch returns the harness's wrapper pid; the listening process is a child. Killing the
wrapper left the daemon up and the ports held. **Right way:** `ss -tlnp | grep <port>` and kill the
listener pid, or read `pid` from `<state_root>/heartbeat.json`. Avoid `pkill -f <pattern>` — it can
match your own shell.

**2.4 A state root cannot live under `/tmp`.**
Two independent layers refuse it: `lifecycle.CheckStateRoot` against `fs.forbidden_state_roots`
(default `/tmp`, `/var/tmp`, overridable from the file) and `internal/ledger`'s HARD-CODED
`validateRoot` refusal (`TROUBLE-LEDGER-012`, no config key). Use a `$HOME` state root, mode 0700.

**2.5 A container config is container-shaped: flag-override every path-like key on a host boot.**
`deploy/container/config.light-hub.toml` carries `/data/state/...` paths and `0.0.0.0` binds. It boots
on a host only because per-key flags win over the file — pass `--state_root`, `--config_path`,
`--secrets-environment_file`, `--dashboard-token_file`, `--ingest-bind`, `--dashboard-bind`,
`--ingest-advertised_host`. Omit `--dashboard-token_file` and the daemon resolves
`dashboard.token_file = /data/state/dashboard.token` (which cannot exist outside the image), comes up
with an empty token store, logs `dashboard token store is empty ... reason=missing`, and 401s every
authenticated route. That is finding TRBL-063.

**2.6 The distroless image has no shell.** `docker exec <c> sh -c ...` fails with
`exec: "sh": executable file not found in $PATH`. Read files out with
`docker cp <container>:/data/state/... <local>`; mint with the documented
`docker compose run --rm --entrypoint /usr/local/bin/trouble ...` form.

**2.7 Compose project-name drift bites teardown.** With the compose file copied outside the repo the
project name comes from the file's `name:` (I used `trouble-df`) while the containers stay
`container_name: trouble` / `trouble-redis`. `docker compose -f <copy> down -v` then removes the wrong
project's volumes and leaves the containers up. **Right way after a scratch compose run:**
`docker rm -f trouble trouble-redis` + `docker volume rm <project>_trouble-state <project>_redis-data`,
then assert `docker ps -a | grep -i trouble` and `ss -tln | grep -E ':(7643|7644)'` both come back
empty.

## 3. Two product-level traps that cost the most time

**3.1 A 429 during a Redis outage is CORRECT, and two docs read like it is not.**
SPEC-13 §4.3 is explicit: with `require_redis=false`, a runtime Redis loss means senders see
`429 + Retry-After` and **local spools hold**; the daemon keeps running, the ledger stays intact, code
004. `deploy/container/config.light-hub.toml`'s header says instead "with Redis unavailable the hub
degrades to standalone ingestion and the daemon keeps serving — no event loss, no restart", and
`docs/operations.md` §14 repeats "degrade to standalone ingestion, no event loss". Both read as
"ingestion keeps returning 200", which is not the contract. Read the §4.3 table row, not the prose.
Corollary for a verification run: assert the §4.3 row (429, `Retry-After`, intact ledger, clean
recovery) — **not** "the event was accepted during the outage".

**3.2 Health `status: degraded` on a stock boot is often honest, not a defect.** On the fresh bunker
box the canonical quickstart booted, accepted an event and answered 200 with `status: "degraded"`,
`detail.reason: "unstamped_build"` (a transferred, no-`.git` tree cannot be stamped — see TRBL-066).
On the container build it was `detail: {"sensor_degraded": "psi"}` instead. Read `detail.reason`
before filing: "degraded" alone is not a finding.

## 4. The right way to run this leg next time

1. `make bin` first (stamped — an unstamped `bin/` reds smoke checks for the wrong reason).
2. Scratch redis WITHOUT `--rm`, on a free port, `appendonly yes --maxmemory-policy noeviction`;
   assert the exact port the config names is the one you are about to stop.
3. Boot on 766x ports with a `$HOME` state root and the full flag set (§2.5).
4. Wait for the `hub` stanza in `/health.json`, then post one event and confirm `event` + `group` on
   ONE sig in the ledger.
5. Outage: `docker stop` the WIRED container, check 429 + `Retry-After` + degraded reason + zero
   ledger records, `docker start`, poll to `status=ok`.
6. Tear down both the daemon (by listener pid) and the redis; assert the ports are free.
7. Install leg: `bunker spawn --server agent-host-3 --ttl 2h`, transfer with `git archive`,
   bootstrap Go (the README prerequisite), `make bin`, run the documented quickstart verbatim, then
   **destroy the agent**.

## 5. What this run could NOT verify (honest gaps)

- The blob-level `ForwardEnvelope` duplicate-key collapse (SPEC-13 AC29 "1 record for the duplicate
  key"): the generic-JSON on-ramp carries no idempotency key, so the Redis `SET NX` window never
  engages for it (`dedup_hits=0, dedup_misses=3`). What IS proven is the group-level collapse
  (2 identical events → 1 group, 1 incident).
- Archival against a live DuckBrain service: `server.duckbrain.endpoint` was left empty on purpose,
  so the tier parked with `TROUBLE-HUB-009` while ingestion served (the designed split).
- `require_redis=true` at runtime loss (the spec says 503 rather than 429): not exercised; the
  boundary is already recorded in the TRBL-029 close list.
- The four `trouble hub ...` verbs could not be exercised because they do not exist (TRBL-065).
- The compose leg's ingest on-ramp could not be exercised end-to-end for the documented reason in
  TRBL-062 (the 401); the container health path and the seed step WERE exercised and pass.
