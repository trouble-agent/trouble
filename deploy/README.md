# Deploy notes for trouble v0.1

## systemd units

`internal/lifecycle` embeds four unit templates:

- `trouble.service` — the daemon (`Type=notify`, watchdog, restart always).
- `trouble-escalate@.service` — out-of-band escalation, no dependency on trouble.
- `trouble-stall.service` — external stall checker (oneshot).
- `trouble-stall.timer` — fires the checker every `checker.interval`.

Install with `trouble install [--scope user|system] [--root DIR] [--check] [--dry-run]`.

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

All directories are `0700`, all files `0600`.

## Configuration

See `examples/config.toml` and `examples/trouble.env`. Precedence is
flag > env > file > default. Unknown file keys are fatal (TROUBLE-LIFECYCLE-001);
unknown env keys are ignored with a near-miss hint.
