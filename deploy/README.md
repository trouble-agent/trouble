# Deploy notes for trouble v0.1

## First foreground run (canonical quickstart)

Use this path before installing a systemd unit. It proves the checkout, config,
listeners, generic JSON ingestion and ledger on the same host without modifying a
system unit. This repository intentionally has no remote configured: start from
an existing checkout or from a checkout supplied by your own source-transfer or
release process, then run the commands from that checkout.

Prerequisites:

- Go 1.26 or newer (`go.mod` declares `go 1.26.0`) — install it from
  <https://go.dev/dl/> if `go version` is not already on `PATH`; `make` and a
  POSIX shell.
- `curl` for the readiness and first-event requests; `jq` for the ledger check.
- A writable local filesystem for the state root. Do not use `/tmp`, `/var/tmp`,
  or a network mount.

Install the Go toolchain before anything else.

A bare host — a fresh container, a minimal VM, an ephemeral bunker agent — has no
Go at all, and the build below stops with `/bin/sh: 1: go: not found`. Download
the Go 1.26+ archive for your OS and architecture from the canonical page
<https://go.dev/dl/>, unpack it, and add its `bin` directory to `PATH` (a
per-user unpack under `$HOME` needs no root), then confirm before continuing:

```sh
go version      # → go version go1.26.x <os>/<arch>
```

Create the files and directories the foreground daemon validates. The copied
configuration and environment file are mode `0600`; the state root is mode
`0700`.

```sh
cd /path/to/your/trouble-checkout

CONFIG_DIR="$HOME/.config/trouble"
STATE_ROOT="$HOME/.local/state/trouble"
mkdir -p "$CONFIG_DIR" "$STATE_ROOT"
cp examples/config.toml "$CONFIG_DIR/config.toml"
cp examples/trouble.env "$CONFIG_DIR/trouble.env"
chmod 0600 "$CONFIG_DIR/config.toml" "$CONFIG_DIR/trouble.env"
chmod 0700 "$STATE_ROOT"
```

Edit `"$CONFIG_DIR/config.toml"` before boot. Replace these stock values with
literal absolute paths and host-specific values (do not write `$HOME` or shell
variables into TOML):

```toml
state_root = "/home/you/.local/state/trouble"
config_path = "/home/you/.config/trouble/config.toml"

[secrets]
environment_file = "/home/you/.config/trouble/trouble.env"

[ingest]
advertised_host = "trouble.your-domain.example"

[[projects]]
id = "1"
public_key = "replace-with-a-new-32-lowercase-hex-character-key"
```

Keep `ingest.bind = "127.0.0.1:7643"`, `dashboard.bind = "127.0.0.1:7644"`,
and `ingest.auth.loopback_dsn = true` for this local first run. Do not use an IP
literal or either bind address for `ingest.advertised_host`: it is the name
reporters place in a DSN. On this loopback bind you may leave the key unset
instead — `localhost` is then derived for you and is the one spelling of the
loopback name the sentinel accepts (SPEC-04 §2.3a); the same name is refused the
moment the bind leaves loopback, so moving to a LAN/tailnet address means
declaring a real name here. Keep the
shipped `[issues]` and `[skills]` tables disabled unless their complete external
configuration and credentials are ready. The stock project key is valid only as
a bootable placeholder; replace it before accepting reports from anything other
than this local proof.

Build the stamped binaries and start the daemon in the foreground. Leave this
terminal running until the shutdown step.

```sh
# Requires Go 1.26+ on PATH (a distro Go with GOTOOLCHAIN=auto also works) —
# install it first from https://go.dev/dl/ (see above); without it this line
# stops at `/bin/sh: 1: go: not found`. make is OPTIONAL: the stamped build is
# two go build lines (see README.md "Build and test") — the direct equivalent
# of `make bin`, used here:
VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
GIT_SHA=$(git rev-parse --short=7 HEAD)          # no .git? pass the source's short sha
BUILD_TIME=$(date -u +%Y-%m-%dT%H:%M:%S.000Z)
LDFLAGS="-s -w -X github.com/trouble-agent/trouble/internal/lifecycle.Version=$VERSION \
  -X github.com/trouble-agent/trouble/internal/lifecycle.GitSHA=$GIT_SHA \
  -X github.com/trouble-agent/trouble/internal/lifecycle.BuildTime=$BUILD_TIME"
CGO_ENABLED=0 go build -trimpath -ldflags="$LDFLAGS" -o bin/troubled ./cmd/troubled
CGO_ENABLED=0 go build -trimpath -ldflags="$LDFLAGS" -o bin/trouble ./cmd/trouble
bin/troubled --config "$CONFIG_DIR/config.toml"
```

A checkout supplied by a source transfer (an exported tarball, `git archive`,
a release archive — anything without `.git`) gives the build nothing to
derive the version stamp from, and the pair comes out unstamped:
`trouble --version` prints
`0.0.0-dev unknown <build-time> [UNSTAMPED (degraded; trouble install
refuses to enable the unit without --force)]` and the health request below
returns `status="degraded"` with `detail.reason="unstamped_build"`. That is
the designed posture for a build git could not stamp, not a broken build —
`trouble install` merely refuses to enable the unit without `--force`. To
stamp it anyway, pass the short sha of the commit your transfer was made
from — the same `GIT_SHA` the container path passes as a build arg (step 2
of the compose quickstart below); on the command line it overrides the
Makefile default whose fallback is the `unknown` sentinel:

```sh
# No .git — same direct build with the sha of the transferred source:
GIT_SHA=<short-sha-of-the-transferred-source>  # then run the go build block above
bin/trouble --version   # → 0.0.0-dev <sha> <build-time> [stamped]
```

`VERSION` still reports `0.0.0-dev` — with no git history there is no tag to
describe — but the stamped/unstamped verdict keys on the sha alone, so an
explicit `GIT_SHA` reports `[stamped]` and installs without `--force`. The
`nogit00` worktree placeholder and the degraded-by-design rationale are
covered in `docs/operations.md` §19.

In a second terminal, verify both listener contracts and send the first event.
Replace `PUBLIC_KEY` with the 32-character lowercase hexadecimal `public_key`
you placed in the config; this is the documented loopback `sentry_key` auth form,
not a dashboard token.

```sh
curl -fsS http://127.0.0.1:7644/health.json | jq '{status, ledger_last_seq, subsystems}'

PUBLIC_KEY='replace-with-the-project-public-key'
curl -fsS -X POST "http://127.0.0.1:7643/api/1/event/?sentry_key=$PUBLIC_KEY" \
  -H 'Content-Type: application/json' \
  --data '{"message":"first trouble event","level":"error","release":"0.1.0"}'

jq -r 'select(.kind == "event" or .kind == "group") | [.kind, .sig] | @tsv' \
  "$STATE_ROOT"/ledger/*.jsonl
```

The health request must return JSON and the event request must return
`{"id":"<event_id>"}`. The ledger command must show the submitted `event` and
its fingerprint `group`; this is the durable proof, not merely an HTTP success.

Cleanly stop the foreground daemon with `Ctrl-C`. `troubled` handles `SIGINT`
and `SIGTERM` by draining: it stops new work, flushes the ledger and spool, closes
listeners, and exits.

## Container quickstart (compose)

The same two binaries also ship as a distroless image with a compose stack
(trouble + redis). This is the copy-edit path for a container host; the measured
refusals each step avoids are documented in `docs/operations.md` §19.

Prerequisites: Docker with the compose plugin. No ports are touched besides
7643/7644 published on `127.0.0.1` only.

1. Edit the shipped container config — compose bind-mounts it read-only, so the
   shipped file IS the file the stack runs; the three placeholder values are
   the only edits:

   ```sh
   $EDITOR deploy/container/config.toml
   ```

   Set `ingest.advertised_host` to a NAME reporting apps resolve (never an IP
   literal or `localhost`), and set `dashboard.public_origin` to the origin
   users reach the dashboard on. The other off-loopback keys the container
   needs are already in the file, each with the boot refusal it answers:
   `0.0.0.0` binds (published ports), `[ingest.auth] nonloopback_mode =
   "proxy"` (TROUBLE-LIFECYCLE-003), `dashboard.mandate = "proxy"`
   (TROUBLE-DASHBOARD-006). The token and env files must live in the state
   volume (`/data/state/…`): every location outside it is refused
   (TROUBLE-LIFECYCLE-005/013), and the shipped comment in the file's
   `[secrets]` table shows how the env file gets there.

   The compose stack's own first event needs one line on its project: a
   `secret_key` (32 lowercase hex) plus the `X-Sentry-Auth` header — a request
   through the published port arrives from a non-loopback ZONE (its TCP peer is
   docker's bridge gateway, a private 172.x address), including one sent from
   this host, and the bind matrix refuses a bare public key there
   (`query-string key refused on a non-loopback request` for the query form,
   then `off-loopback requests require the project ingest token` for a header
   form with no secret; both measured live on a built stack). The shipped
   file declares the dev placeholder `secret_key`; rotate it before anything
   real reports here.

2. Build, boot, seed, verify — in order; the container starts `unhealthy` by
   design until the seed exists:

   ```sh
   GIT_SHA=$(git rev-parse --short=7 HEAD) docker compose build
   docker compose up -d
   docker compose run --rm \
     -e TROUBLE_DASHBOARD_TOKEN_FILE=/data/state/dashboard.token \
     -e TROUBLE_STATE_ROOT=/data/state \
     --entrypoint /usr/local/bin/trouble trouble \
     dashboard token create --label compose --scopes read \
     --output-env /data/state/trouble.env
   docker compose ps      # trouble: healthy once the checker can authenticate
   curl -s -H "Authorization: Bearer <plaintext from the seed>" \
     http://127.0.0.1:7644/health.json | jq '{status, git_sha, ledger_last_seq}'
   ```

   The seed's `--output-env` flag is load-bearing, not sugar: the image is
   distroless (no shell, no editor), and every way of writing the env file from
   outside lands the wrong uid or mode — the mint writes the 0600 file itself,
   as the container's own uid, inside the volume (measured: trap 4 in
   `docs/operations.md` §19).

   `-e TROUBLE_DASHBOARD_TOKEN_FILE=/data/state/dashboard.token` is equally
   load-bearing, and for the same class of reason: the container configs declare
   `dashboard.token_file` under `/data/state`, which does not exist on a host, so
   a mint run WITHOUT it would write your `~/.config/trouble/dashboard-tokens.json`
   while the daemon reads the `/data/state` path — the token then 401s with
   nothing naming the two paths. The mint now prints the store it wrote and warns
   when the resolved config declares a different one, so the mismatch is visible
   instead of silent.

   The stack can be healthy at the process level (`docker compose ps`) while the
   health request of step 2 reports `status=degraded`. On this image that is the
   expected posture, **not a fault**: a container has no host systemd/D-Bus
   surface to watch and cannot arm PSI triggers, so its sensor batch degrades by
   design on every boot. Measured on this tree's compose stack, with
   `docker compose ps` reading `healthy`:

   ```json
   {"status":"degraded","detail":{"reason":"sensors_container","sensor_degraded":"timers"}}
   ```

   Read the block, not the status word — the same request as step 2, with the
   projection widened to the whole `detail` block and the degraded `sensors[]`
   rows:

   ```sh
   curl -s -H "<step 2's auth header>" http://127.0.0.1:7644/health.json \
     | jq '{status, detail, degraded_sensors: [.sensors[] | select(.degraded) | {sensor, reason}]}'
   ```

   Three things answer it. `detail.sensor_degraded=<sensor>` names one degraded
   sensor (one cause per alarm, exactly as `subsystems[]` reports one refused
   plane) — on this stack `timers`, and that sensor's own `sensors[]` row carries
   the `reason` behind it, `dependency dbus degraded`: there is no host D-Bus for
   it to watch. A read in the first minute can name `psi` instead
   (`mode=sampling-only`: the trigger write was refused) before the first timer
   sweep runs; either name is the same posture. `detail.reason=sensors_container`
   is the piece that identifies the cause as this container environment rather
   than a fault on the host sensor plane. The posture is stable — every boot of
   this image reports it on a working stack — so a change in it is the signal,
   never the `degraded` word on its own.

   What IS worth stopping for, from the same block: `status=stalled` (the ledger
   writer is not advancing), a `detail.reason` other than `sensors_container`
   (`unstamped_build` for a build git could not stamp), `detail.subsystem_refused`
   (a plane of the daemon never built), `rss_over_budget`, or a
   `hub.degraded_reason` on the light-hub profile. The step-2 fields stay the
   positive proof that this stack is fine: `git_sha` stamped (never `unknown`)
   and `ledger_last_seq` advancing as events arrive.

3. Send the first event. A request through the published port arrives from a
   non-loopback zone, so the foreground quickstart's `?sentry_key=`-only form is
   refused here; use the header form with the project's `secret_key` (step 1,
   or the shipped dev placeholder):

   ```sh
   curl -fsS -X POST "http://127.0.0.1:7643/api/1/event/" \
     -H 'Content-Type: application/json' \
     -H 'X-Sentry-Auth: Sentry sentry_version=7, sentry_key=<32-hex public_key>, sentry_secret=<32-hex secret_key>' \
     --data '{"message":"first trouble event","level":"error","release":"0.1.0"}'

   docker run --rm -v trouble_trouble-state:/data alpine:3 \
     sh -c 'grep -hE "\"kind\":\"(event|group)\"" /data/state/ledger/*.jsonl' \
     | jq -r 'select(.kind == "event" or .kind == "group") | [.kind, .sig] | @tsv'
   ```

   The event request must return `{"id":"<event_id>"}` and the ledger command
   must show the `event` and its fingerprint `group` on one sig — the same
   acceptance the foreground quickstart proves.

   The ledger is at `state_root/ledger`, and this file's `state_root` is
   `/data/state`, so inside the volume it is `state/ledger/` — NOT the volume
   root. On the host side that path is 0700 owned by the daemon's uid (65532),
   so reading it as your own user fails with `Permission denied`; the throwaway
   container above reads it as root instead. To resolve the host path yourself,
   `docker volume inspect trouble_trouble-state` and append `/state/ledger`
   (measured: `/var/lib/docker/volumes/<project>_trouble-state/_data/state/ledger/`).
   The container name is prefixed by the compose project name, so it is
   `trouble_trouble-state` only when the project is still named `trouble`.

4. Tear down: `docker compose down` keeps the state volume; `-v` drops it.

Builds from a git **worktree** stamp `nogit00` unless `GIT_SHA` is passed as in
step 2 — the worktree's `.git` is a pointer file the build context cannot
resolve. `nogit00` is a placeholder, not the unstamped sentinel (`unknown`);
§19 explains the difference.

## systemd units

`internal/lifecycle` embeds four unit templates:

- `trouble.service` — the daemon (`Type=notify`, watchdog, restart always).
- `trouble-escalate@.service` — out-of-band escalation, no dependency on trouble.
- `trouble-stall.service` — external stall checker (oneshot).
- `trouble-stall.timer` — fires the checker every `checker.interval`.

`trouble install [--scope user|system] [--root DIR] [--check] [--dry-run] [--force]`
is not the first-run path. Run it only after the foreground quickstart succeeds,
a stamped build is available (`make bin` or the direct `go build` equivalent —
make is OPTIONAL), the target systemd scope is usable, and
`[escalate] channels` is configured. `trouble install --check` validates the
installed units and escalation wiring without writing them.

## State layout

```
~/.local/state/trouble/
├── ledger/          # SPEC-01
├── spool/forward/   # satellite queue (SPEC-12)
├── backups/bin/     # rollback binaries
├── heartbeat.json
├── checker.state.json
├── checker.alarm
└── escalate.log
```

All directories are `0700`, all files are `0600`.

## Configuration

See `examples/config.toml` and `examples/trouble.env`. Precedence is
flag > env > file > default. Unknown file keys are fatal (TROUBLE-LIFECYCLE-001);
unknown env keys are ignored with a near-miss hint.
