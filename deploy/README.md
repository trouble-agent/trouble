# Deploy notes for trouble v0.1

## First foreground run (canonical quickstart)

Use this path before installing a systemd unit. It proves the checkout, config,
listeners, generic JSON ingestion and ledger on the same host without modifying a
system unit. This repository intentionally has no remote configured: start from
an existing checkout or from a checkout supplied by your own source-transfer or
release process, then run the commands from that checkout.

Prerequisites:

- Go 1.26 or newer (`go.mod` declares `go 1.26.0`); `make` and a POSIX shell.
- `curl` for the readiness and first-event requests; `jq` for the ledger check.
- A writable local filesystem for the state root. Do not use `/tmp`, `/var/tmp`,
  or a network mount.

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
literal, `localhost`, or either bind address for `ingest.advertised_host`: it is
the name reporters place in a DSN. Keep the
shipped `[issues]` and `[skills]` tables disabled unless their complete external
configuration and credentials are ready. The stock project key is valid only as
a bootable placeholder; replace it before accepting reports from anything other
than this local proof.

Build the stamped binaries and start the daemon in the foreground. Leave this
terminal running until the shutdown step.

```sh
make bin
bin/troubled --config "$CONFIG_DIR/config.toml"
```

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

## systemd units

`internal/lifecycle` embeds four unit templates:

- `trouble.service` — the daemon (`Type=notify`, watchdog, restart always).
- `trouble-escalate@.service` — out-of-band escalation, no dependency on trouble.
- `trouble-stall.service` — external stall checker (oneshot).
- `trouble-stall.timer` — fires the checker every `checker.interval`.

`trouble install [--scope user|system] [--root DIR] [--check] [--dry-run] [--force]`
is not the first-run path. Run it only after the foreground quickstart succeeds,
a stamped `make bin` build is available, the target systemd scope is usable, and
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
