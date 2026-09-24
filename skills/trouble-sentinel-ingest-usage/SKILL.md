---
name: trouble-sentinel-ingest-usage
description: How to actually use trouble's sentinel ingest path as a real reporter — boot, auth, POST events, read the ledger, and the pitfalls (no event dedup on the standalone on-ramp, idle amplification, where state lives).
version: 1.0.0
---

# Using trouble's sentinel ingest (real-reporter pattern)

## What this covers

The sentinel on-ramp: boot the daemon, POST events like a Sentry-style reporter would, and read
back what the ledger recorded. This is the surface a downstream producer of error events touches.
The SPEC-13 light-hub profile has its own usage skill (`trouble-light-hub-usage`).

## Boot and drive it

```bash
# 1. Build (needs Go 1.26+ — see pitfall 1)
make bin

# 2. Boot with a scratch state dir; daemon listens on 127.0.0.1:7643
~/dgstate/trouble-state/  # state root used in the 09-24 run: ledger/ day-files live here
# start the daemon with the state pointed at a scratch dir, e.g. /tmp/dg-trouble-state

# 3. POST an event the way a reporter does (generic-JSON on-ramp, sentry_key auth):
curl -X POST 'http://127.0.0.1:7643/api/1/event/?sentry_key=0123456789abcdef0123456789abcdef' \
  -H 'Content-Type: application/json' \
  -d '{"message":"first trouble event","level":"error","release":"0.1.0"}'
# → 200 with {"id":"…"} — the record id

# 4. Read the ledger:
jq -c . /tmp/dg-trouble-state/ledger/$(date +%F).jsonl | less
# kinds: event, group. The group carries payload.op, payload.count, counters.suppressed.
```

## Right-way patterns

- **Measure idle cost by ledger diff, not by feeling**: snapshot `wc -l ledger/<today>.jsonl`,
  wait N idle minutes, diff. The 09-24 numbers: 54 records/~63KB per 2 idle min ≈ 10MB/day.
- **Check dedup by POSTing the same payload 3x and grepping the sig**:
  `grep -c 'sentinel:sha256v1:<sig>' ledger/*.jsonl` — events vs groups tell you whether
  duplicates folded. As of 09-24 they DO NOT fold on the standalone on-ramp (TRBL-074) — retrying
  reporters produce one event record per attempt, group count stays 1.
- **Fresh-machine install**: the documented quickstart works on a bare Debian user after Go is
  present; `make bin` ~43s, binary stamps the git HEAD. A no-.git transfer produces an UNSTAMPED
  `0.0.0-dev` pair (known, TRBL-066).

## Pitfalls

1. **`make bin` fails `go: not found` on a bare box.** README states Go 1.26+ under "Getting the
   source", but the quickstart command block never mentions it. Install Go first.
2. **The standalone on-ramp has NO event-level dedup** (as of HEAD 73fecd0). Identical-sig POSTs
   are all recorded. Don't build retry-on-timeout without idempotency expectations.
3. **Idle amplification is real but improved**: one D-Bus sig re-emits sub-second despite
   `sensors.merge_window=5s` (~54 records/2min idle). Expect a growing ledger on an idle host.
4. **Load-test residue lives in the tree**: `internal/sentinel/.sentinel-load-*/` (~1.6GB,
   gitignored but on disk). Never tar the working checkout as a source-transfer install path —
   you ship gigabytes of test state. Tar a fresh clone, or purge first.
5. **State root matters**: the ledger is a set of day-files under `<state>/ledger/`. Point the
   daemon at a scratch dir when experimenting so real state is never touched.
