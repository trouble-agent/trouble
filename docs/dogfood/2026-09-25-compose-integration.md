# Dogfood integration report — trouble compose stack (2026-09-25)

Tick: trouble-dogfood 2026-09-25-08-46-05 · build c978da4-dirty · verdict 🟢 SHIPPABLE (with open P1 TRBL-074)

## The promise tested

deploy/README.md "Container quickstart (compose)": a container host operator edits three
placeholder values in ONE file (`deploy/container/config.toml`), runs
`GIT_SHA=$(git rev-parse --short=7 HEAD) docker compose build && docker compose up -d`,
seeds a dashboard token with one documented `compose run` mint, watches
`docker compose ps` flip to healthy, and sends a first event through the published
port with the `X-Sentry-Auth` header form — the ledger proving event+group durability
inside the volume.

This surface had NEVER been driven by a dogfood run before (09-17 foreground, 09-20
light-hub, 09-24 sentinel on-ramp).

## The working run (what to type)

```sh
# 1. one file, two edits (the doc says three; secret_key is already shipped dev-grade)
$EDITOR deploy/container/config.toml
#    ingest.advertised_host = "trouble.your-host.example"   (a NAME, never IP/localhost)
#    dashboard.public_origin = "https://trouble.your-host.example"

# 2. build + boot (measured: build 72s warm-layer, up 7s to 'health: starting')
GIT_SHA=$(git rev-parse --short=7 HEAD) docker compose build
docker compose up -d

# 3. seed the token EXACTLY as documented (the -e flags are load-bearing)
docker compose run --rm \
  -e TROUBLE_DASHBOARD_TOKEN_FILE=/data/state/dashboard.token \
  -e TROUBLE_STATE_ROOT=/data/state \
  --entrypoint /usr/local/bin/trouble trouble \
  dashboard token create --label compose --scopes read \
  --output-env /data/state/trouble.env

# 4. healthy after ~40s (start_period 15s + interval 30s)
docker compose ps

# 5. health + first event (the header form; query-form is refused off-loopback BY DESIGN)
TOK=tdt_...   # printed once by the mint
curl -s -H "Authorization: Bearer $TOK" http://127.0.0.1:7644/health.json
curl -fsS -X POST "http://127.0.0.1:7643/api/1/event/" \
  -H 'Content-Type: application/json' \
  -H 'X-Sentry-Auth: Sentry sentry_version=7, sentry_key=0123456789abcdef0123456789abcdef, sentry_secret=fedcba9876543210fedcba9876543210' \
  --data '{"message":"first trouble event","level":"error","release":"0.1.0"}'
#    -> {"id":"321cdb45..."} in 0.5s cold / 0.2-0.3s warm

# 6. the durable proof (this is the acceptance, not the HTTP 200)
docker run --rm -v trouble_trouble-state:/data alpine:3 \
  sh -c 'grep -hE "\"kind\":\"(event|group)\"" /data/state/ledger/*.jsonl' \
  | jq -r 'select(.kind == "event" or .kind == "group") | [.kind, .sig] | @tsv'
```

## What a real user actually hits (measured this run)

1. **Health says `degraded` when compose says `healthy`.** First API call:
   `{"status":"degraded","detail":{"sensor_degraded":"timers"}}` — a distroless box has
   no timers to watch. Expected, but zero operator-facing doc says so on the container
   path → TRBL-077.
2. **The ledger ships 8 sensor noise events per fresh boot** (journald on an image with
   no journal, disk on `overlay:/`, inotify, dbus, psi; two disk sigs double-fire at
   boot). All `suppressed=true, fire=false` — noise, not incidents — but every fresh
   install's audit ledger starts polluted → TRBL-078.
3. **Five byte-identical `outage probe B` POSTs produced five event records and the
   group never folded** (`op=create count=1 suppressed=0` on all three groups). The
   known open P1 TRBL-074 reproduces identically on the compose path (previously only
   proven on the standalone foreground path).
4. **The redis outage contract HOLDS on compose.** `docker compose stop redis` → daemon
   kept serving: health answered, 6 events (1+5) POSTed 200 across the outage window,
   restart → all 10 sentinel events in the ledger, zero loss, groups+incidents formed
   per sig (3 groups / 3 incidents on 3 sigs).
5. **Dashboard is fast and shows the data.** `/` 200 in 1.6ms (8.4KB) rendering all
   three event titles; `/incidents` 3.6ms with the incident present. Auth fail-closed:
   no token 401, bad token 401, bad secret on ingest 401 + `X-Sentry-Error:
   TROUBLE-SENTINEL-006`.

## Perf (Step 2b, one command each)

- ingest POST (headline): warm 287.6ms ± 58.5ms (hyperfine, 20 runs, -N); cold first
  event 545ms; curl user+sys ~6ms → the latency is the server's ack-after-fsync
  group-commit window (`ledger.fsync_window_ms=200`, SPEC-01 durability contract), not
  CPU. A one-off reporter waits ~0.3s per event; that is the price of the durability
  promise and is fine for the product's purpose. **No PERF row filed — nothing here is
  slow enough that a user would flag it as a defect; the number is the finding.**
- build (cold, bunker agent, 2 vCPU-class): 120s. up: 7s. healthy: ~40s from up.
- dashboard pages: ~1-4ms server time.

## Errors hit, and their fixes

| error | cause | fix |
|---|---|---|
| `query-string key refused on a non-loopback request` (documented) | published port = non-loopback zone | use the `X-Sentry-Auth` header form (docs cover it) |
| ports 7643/7644 busy at boot | 09-24 dogfood's foreground daemon still running (19h) | killed it (TRBL-079 residue note) |
| bunker: `permission denied ... /var/run/docker.sock` | default DOCKER_HOST is root's socket | `DOCKER_HOST=unix:///run/bunker/<agent>/docker.sock` |
| bunker: `Unit docker.service not found` on `systemctl --user start` | bunkerd manages the daemon out-of-band | the daemon is already running on the bunker socket; just point DOCKER_HOST at it |

## Verdict rationale

Every documented acceptance of the container quickstart passed verbatim on a fresh
machine (bunker leg) AND on the dev host, the redis outage contract held with zero
loss, auth is fail-closed everywhere probed, and the dashboard renders the ingested
data. The two P2s (TRBL-077/078) are polish/trust issues a first-time operator meets
in their first five minutes; the P1 (TRBL-074, duplicate evidence on retry) predates
this run and reproduces here unchanged.
