---
name: trouble-compose-usage
description: Use when driving the trouble compose container stack — quickstart, seeding, health reading, outage drills, bunker installs.
---

# trouble — compose stack usage (agent-facing)

What this is: the container quickstart of `trouble` (incident brain), `deploy/README.md
§Container quickstart (compose)` — trouble (distroless) + redis (appendonly, noeviction).

## Entry points

- Stack: `docker-compose.yml` (trouble + redis; publishes ONLY 127.0.0.1:7643 ingest,
  127.0.0.1:7644 dashboard). Config: `deploy/container/config.toml` — compose bind-mounts
  it read-only; the shipped file IS the file the stack runs.
- The only edits a user makes: `ingest.advertised_host` and `dashboard.public_origin`
  (a NAME, never an IP or `localhost`). The shipped `secret_key` is a dev placeholder.

## The documented sequence (all load-bearing)

```sh
GIT_SHA=$(git rev-parse --short=7 HEAD) docker compose build   # stamp; worktree/shallow → -dirty suffix
docker compose up -d
docker compose run --rm \
  -e TROUBLE_DASHBOARD_TOKEN_FILE=/data/state/dashboard.token \
  -e TROUBLE_STATE_ROOT=/data/state \
  --entrypoint /usr/local/bin/trouble trouble \
  dashboard token create --label compose --scopes read \
  --output-env /data/state/trouble.env
docker compose ps        # healthy after ~40s (start_period 15s + interval 30s)
```

- The mint's two `-e` flags are NOT optional: without them the token lands in
  `~/.config/trouble/` on the host while the daemon reads `/data/state/` — silent 401s.
- Ingest off-loopback (the published port is ALWAYS a non-loopback zone) requires the
  `X-Sentry-Auth` header form; `?sentry_key=` is refused by design (measured, both docs
  and live).
- Ledger proof (the real acceptance, not the HTTP 200):
  `docker run --rm -v trouble_trouble-state:/data alpine:3 sh -c 'grep -hE "\"kind\":\"(event|group)\"" /data/state/ledger/*.jsonl'`
  → one `group` with `op=create count=1` per unique sig.

## Pitfalls (all measured 2026-09-25)

- compose ps says `healthy` while `/health.json` says `status=degraded`
  `detail.sensor_degraded=timers` — distroless has no systemd timers; EXPECTED on the
  container profile, do not debug it as a fault (TRBL-077 asks for a doc sentence).
- Fresh boot writes ~8 sensor noise events (journald/disk-overlay/inotify/dbus/psi,
  `suppressed=true fire=false`) into the ledger before any real event — ignore them
  when grepping the ledger; two disk sigs double-fire at boot (TRBL-078).
- Duplicate POSTs do NOT fold at the group: 5 identical events → 5 event records,
  group count stays 1, `suppressed=0` (open P1 TRBL-074, reproduces on compose).
- Ports 7643/7644: a native foreground `troubled` from a previous dogfood/boot collides
  here. `ss -tlnp | grep 764` before `compose up`.
- Redis stop/start is a DESIGNED drill (SPEC-13): `docker compose stop redis` → the
  daemon keeps serving (429+Retry-After only under sustained load); restart → zero loss.
- Scratch files the stack/container tooling writes to /tmp can collide between agents
  on shared bunker hosts — use $HOME.

## On a bunker agent (fresh-machine install test)

- Clone works from the public origin: `git clone --depth 1
  https://github.com/trouble-agent/trouble ~/app`.
- The agent's docker is rootless with a bunker-specific socket. Use
  `export DOCKER_HOST=unix:///run/bunker/<agent>/docker.sock` (NOT
  /var/run/docker.sock, NOT /run/user/<uid>/docker.sock). `systemctl --user start
  docker` says "Unit not found" — bunkerd runs the daemon out-of-band; it is already
  running, just point DOCKER_HOST at it.
- Measured full leg: build 120s, up 7s, healthy ~40s later, mint→health→event→ledger
  all pass. `/tmp` on bunker hosts is shared across agents — logs go in $HOME.
