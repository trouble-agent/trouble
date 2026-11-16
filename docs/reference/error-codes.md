# Error-code reference

Every `TROUBLE-*` code the binary can emit, generated from the `ErrorCode`
declarations in `internal/types`. The declarations are authoritative: this page
is derived from them, and a Go test fails when the two drift apart.

* **Measured code count at HEAD (generator run on this tree):** 208
* **Shaped `TROUBLE-<FAMILY>-<NNN>` codes:** 207 across 13 families
* **Codes outside that shape:** 1
* **Regenerate with:** `python3 specs/tools/generate_error_codes.py`
* **Drift gate:** `go test ./internal/types/ -run TestErrorCodeReference` — fails
  when a declared code has no entry here, when this page carries a code that is
  no longer declared, or when a class column disagrees with the class map.

Re-running the regeneration command on an unchanged tree produces no diff.

**Family histogram:** `HUB` 16 · `SENSORS` 26 · `LIFECYCLE` 17 · `FLOW` 19 · `REGISTRY` 18 · `SENTINEL` 23 · `LADDER` 21 · `LEDGER` 12 · `ISSUES` 10 · `RESEARCH` 10 · `SCRUB` 8 · `SKILLS` 14 · `DASHBOARD` 13.

**Class histogram:** `permanent` 144 · `policy_refused` 3 · `transient` 61.

## How this file is produced, and why the count can be trusted

The page is written by `specs/tools/generate_error_codes.py`, a stdlib-only script that
walks every non-test `.go` file in `internal/types` and keeps each
`const <Name> ErrorCode = "TROUBLE-..."` spec. The class and the meaning come from
that declaration: the trailing `// <class>: <meaning>` comment, or the family's
`map[ErrorCode]ErrorClass` for the class. The script fails rather than guesses — a
declaration with no class or no one-line meaning stops it with the code and the file
named.

Completeness is a test, not a promise:
`go test ./internal/types/ -run TestErrorCodeReference` re-reads the declarations with
`go/ast` and fails when a declared code is missing from this page, when a code on this
page is no longer declared, when a class column disagrees with `CodeClass` (the map the
daemon's retry and refusal decisions actually read), or when the measured count above
disagrees with the declarations.

The declaration walk counts 208 `ErrorCode` constants — 207 shaped
`TROUBLE-<FAMILY>-<NNN>` codes plus the unshaped `TROUBLE-REDIS-REFUSING`. A repo-wide
regex walk (`TROUBLE-[A-Z][A-Z0-9]*-[0-9]{3}`) also returns 208 tokens, but a different
208: it counts the `TROUBLE-A-000` sentinel described below and misses the unshaped
code. The two walks agree on the thing that matters — no declared code is absent here.

## How to read an entry

`class` is the class the declaration records, not a judgement call: `permanent`
means retrying unchanged reproduces the failure, `transient` means the named
dependency recovers and the operation can be retried, and `policy_refused` means
a policy rather than a fault answered the request. The operator action is the
first response only — the subsystem sections in
[../operations.md](../operations.md) carry the full runbook.

## A length sentinel that is not a code

`TROUBLE-A-000` is **not** an emitted error code. It appears in exactly one place,
`internal/app/subsystems.go:737`, as the shortest string a `TROUBLE-` prefixed
token must be before the parser treats it as a code at all:

```go
if len(code) < len("TROUBLE-A-000") {
        return ""
}
```

A grep for `TROUBLE-A-000` therefore finds a length bound, never a refusal a
reader has to handle. It is deliberately absent from the tables below.

## Known irregularities

- `TROUBLE-REDIS-REFUSING` (`spec13.go:73`) is a declared, classed and emitted code that does not follow the `TROUBLE-<FAMILY>-<NNN>` shape; it keeps the `TROUBLE-` prefix so it can ride the same `ErrorCode` plumbing, and is listed in its own table below rather than inside a family.

Numbers are not reused after a code is retired: a gap means a number that no
longer has a declaration, and the contract for that number is whatever the
spec that owned it says, not this page.

## Codes by family

### TROUBLE-HUB

16 code(s). Inspect the Redis endpoint, the stream/consumer-group state and the archival profile in `trouble hub status`.

| Code | Class | Meaning (from the declaration) | Operator action |
|---|---|---|---|
| `TROUBLE-HUB-001` | `permanent` | invalid/incomplete server profile | Inspect the Redis endpoint, the stream/consumer-group state and the archival profile in `trouble hub status`. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-HUB-002` | `permanent` | Redis connection or credentials rejected | Inspect the Redis endpoint, the stream/consumer-group state and the archival profile in `trouble hub status`. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-HUB-003` | `transient` | Redis unreachable at boot | Inspect the Redis endpoint, the stream/consumer-group state and the archival profile in `trouble hub status`. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-HUB-004` | `transient` | stream write failed (429/503 to senders) | Inspect the Redis endpoint, the stream/consumer-group state and the archival profile in `trouble hub status`. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-HUB-005` | `permanent` | consumer-group operation failed | Inspect the Redis endpoint, the stream/consumer-group state and the archival profile in `trouble hub status`. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-HUB-006` | `transient` | dedup gate unavailable → bounded LRU fallback | Inspect the Redis endpoint, the stream/consumer-group state and the archival profile in `trouble hub status`. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-HUB-007` | `permanent` | stream entry does not decode as ForwardEnvelope | Inspect the Redis endpoint, the stream/consumer-group state and the archival profile in `trouble hub status`. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-HUB-008` | `transient` | entries stranded past claim_min_idle × 3 | Inspect the Redis endpoint, the stream/consumer-group state and the archival profile in `trouble hub status`. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-HUB-009` | `permanent` | archival target unusable | Inspect the Redis endpoint, the stream/consumer-group state and the archival profile in `trouble hub status`. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-HUB-010` | `transient` | DuckBrain unreachable during export | Inspect the Redis endpoint, the stream/consumer-group state and the archival profile in `trouble hub status`. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-HUB-011` | `permanent` | export refused (generation not closed / bytes changed) | Inspect the Redis endpoint, the stream/consumer-group state and the archival profile in `trouble hub status`. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-HUB-012` | `transient` | export verification failed | Inspect the Redis endpoint, the stream/consumer-group state and the archival profile in `trouble hub status`. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-HUB-013` | `permanent` | live profile switch requested via reload | Inspect the Redis endpoint, the stream/consumer-group state and the archival profile in `trouble hub status`. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-HUB-014` | `permanent` | retention tried to drop an unverified generation | Inspect the Redis endpoint, the stream/consumer-group state and the archival profile in `trouble hub status`. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-HUB-015` | `permanent` | Redis deployment topology refused at preflight | Inspect the Redis endpoint, the stream/consumer-group state and the archival profile in `trouble hub status`. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-HUB-016` | `permanent` | appendonly=no with require_persistence=true | Inspect the Redis endpoint, the stream/consumer-group state and the archival profile in `trouble hub status`. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |

### TROUBLE-SENSORS

26 code(s). Inspect the collector named in the record, its config block and the host capability probe.

| Code | Class | Meaning (from the declaration) | Operator action |
|---|---|---|---|
| `TROUBLE-SENSORS-001` | `permanent` | kernel < 5.15 | Inspect the collector named in the record, its config block and the host capability probe. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SENSORS-002` | `permanent` | root + EINVAL on a legal trigger write | Inspect the collector named in the record, its config block and the host capability probe. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SENSORS-003` | `permanent` | EBUSY (already armed) | Inspect the collector named in the record, its config block and the host capability probe. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SENSORS-004` | `permanent` | POLLERR on an armed fd (source gone) | Inspect the collector named in the record, its config block and the host capability probe. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SENSORS-005` | `permanent` | arming denied for this uid | Inspect the collector named in the record, its config block and the host capability probe. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SENSORS-006` | `permanent` | journalctl not found | Inspect the collector named in the record, its config block and the host capability probe. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SENSORS-007` | `transient` | cursor invalid | Inspect the collector named in the record, its config block and the host capability probe. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-SENSORS-008` | `transient` | journal child exited | Inspect the collector named in the record, its config block and the host capability probe. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-SENSORS-009` | `permanent` | no journal read access | Inspect the collector named in the record, its config block and the host capability probe. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SENSORS-010` | `transient` | bounded journal queue overflow | Inspect the collector named in the record, its config block and the host capability probe. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-SENSORS-011` | `transient` | D-Bus connect failed | Inspect the collector named in the record, its config block and the host capability probe. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-SENSORS-012` | `policy_refused` | polkit refused manage-units | Inspect the collector named in the record, its config block and the host capability probe. A policy, not a fault, refused the request: change the config or the requested scope. |
| `TROUBLE-SENSORS-013` | `permanent` | configured manager not watched | Inspect the collector named in the record, its config block and the host capability probe. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SENSORS-014` | `transient` | NameOwnerChanged on systemd1 | Inspect the collector named in the record, its config block and the host capability probe. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-SENSORS-015` | `transient` | Subscribe/AddMatch failed on a reachable bus | Inspect the collector named in the record, its config block and the host capability probe. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-SENSORS-016` | `permanent` | systemd-oomd absent/masked (no-op) | Inspect the collector named in the record, its config block and the host capability probe. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SENSORS-017` | `permanent` | rule file schema invalid | Inspect the collector named in the record, its config block and the host capability probe. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SENSORS-018` | `permanent` | condition expression invalid | Inspect the collector named in the record, its config block and the host capability probe. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SENSORS-019` | `transient` | hot-reload failed or exceeded 1s | Inspect the collector named in the record, its config block and the host capability probe. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-SENSORS-020` | `transient` | inotify watch limit / ENOSPC | Inspect the collector named in the record, its config block and the host capability probe. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-SENSORS-021` | `transient` | IN_Q_OVERFLOW | Inspect the collector named in the record, its config block and the host capability probe. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-SENSORS-022` | `permanent` | statfs failed for a configured mount | Inspect the collector named in the record, its config block and the host capability probe. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SENSORS-023` | `permanent` | ListTimers reply could not be parsed | Inspect the collector named in the record, its config block and the host capability probe. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SENSORS-024` | `transient` | sensor last-success older than its stale threshold | Inspect the collector named in the record, its config block and the host capability probe. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-SENSORS-025` | `permanent` | sensor disabled, host capability absent | Inspect the collector named in the record, its config block and the host capability probe. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SENSORS-026` | `permanent` | container substrate — sensor facility absent (documented posture; TRBL-078) | Inspect the collector named in the record, its config block and the host capability probe. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |

### TROUBLE-LIFECYCLE

17 code(s). Inspect the state root, the resolved config (`trouble config explain`) and the boot preflight that refused.

| Code | Class | Meaning (from the declaration) | Operator action |
|---|---|---|---|
| `TROUBLE-LIFECYCLE-001` | `permanent` | config file invalid | Inspect the state root, the resolved config (`trouble config explain`) and the boot preflight that refused. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-LIFECYCLE-002` | `permanent` | precedence conflict (non-fatal) | Inspect the state root, the resolved config (`trouble config explain`) and the boot preflight that refused. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-LIFECYCLE-003` | `permanent` | bind preflight refused | Inspect the state root, the resolved config (`trouble config explain`) and the boot preflight that refused. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-LIFECYCLE-004` | `permanent` | state root unusable | Inspect the state root, the resolved config (`trouble config explain`) and the boot preflight that refused. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-LIFECYCLE-005` | `permanent` | state dir mode wrong (not 0700) | Inspect the state root, the resolved config (`trouble config explain`) and the boot preflight that refused. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-LIFECYCLE-006` | `transient` | unit install/repair failed | Inspect the state root, the resolved config (`trouble config explain`) and the boot preflight that refused. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-LIFECYCLE-007` | `transient` | watchdog ping missed | Inspect the state root, the resolved config (`trouble config explain`) and the boot preflight that refused. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-LIFECYCLE-008` | `transient` | heartbeat stale / liveness surface unreadable | Inspect the state root, the resolved config (`trouble config explain`) and the boot preflight that refused. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-LIFECYCLE-009` | `permanent` | ledger sequence stall | Inspect the state root, the resolved config (`trouble config explain`) and the boot preflight that refused. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-LIFECYCLE-010` | `transient` | escalation hook failed on every channel | Inspect the state root, the resolved config (`trouble config explain`) and the boot preflight that refused. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-LIFECYCLE-011` | `transient` | upgrade park failed | Inspect the state root, the resolved config (`trouble config explain`) and the boot preflight that refused. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-LIFECYCLE-012` | `permanent` | schema downgrade refused | Inspect the state root, the resolved config (`trouble config explain`) and the boot preflight that refused. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-LIFECYCLE-013` | `permanent` | secret-bearing file mode wrong / argv secret | `chmod 0600` the secret-bearing file named in the refusal; a wrong mode refuses the boot. |
| `TROUBLE-LIFECYCLE-014` | `permanent` | forward protocol version unsupported | Inspect the state root, the resolved config (`trouble config explain`) and the boot preflight that refused. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-LIFECYCLE-015` | `transient` | spool budget exceeded → drop-oldest | Inspect the state root, the resolved config (`trouble config explain`) and the boot preflight that refused. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-LIFECYCLE-016` | `permanent` | unit missing escalation wiring | Inspect the state root, the resolved config (`trouble config explain`) and the boot preflight that refused. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-LIFECYCLE-017` | `transient` | clock skew beyond tolerance | Inspect the state root, the resolved config (`trouble config explain`) and the boot preflight that refused. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |

### TROUBLE-FLOW

19 code(s). Inspect the flow definition and its run records under the state root.

| Code | Class | Meaning (from the declaration) | Operator action |
|---|---|---|---|
| `TROUBLE-FLOW-001` | `permanent` | flow driver disabled by config | Inspect the flow definition and its run records under the state root. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-FLOW-002` | `transient` | board row append failed | Inspect the flow definition and its run records under the state root. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-FLOW-003` | `permanent` | post-append validation failed | Inspect the flow definition and its run records under the state root. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-FLOW-004` | `permanent` | board id conflict | Inspect the flow definition and its run records under the state root. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-FLOW-005` | `transient` | task-router dispatch failed | Inspect the flow definition and its run records under the state root. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-FLOW-006` | `permanent` | hot-fix lane disabled | Inspect the flow definition and its run records under the state root. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-FLOW-007` | `permanent` | repo not allowed | Inspect the flow definition and its run records under the state root. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-FLOW-008` | `permanent` | below the hot-fix threshold | Inspect the flow definition and its run records under the state root. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-FLOW-009` | `permanent` | one-fix-per-sig lease held | Inspect the flow definition and its run records under the state root. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-FLOW-010` | `transient` | router_spawn call failed | Inspect the flow definition and its run records under the state root. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-FLOW-011` | `transient` | spawn pending after the retry budget | Inspect the flow definition and its run records under the state root. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-FLOW-012` | `permanent` | disk gate refused | Inspect the flow definition and its run records under the state root. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-FLOW-013` | `transient` | worktree mutex timed out | Inspect the flow definition and its run records under the state root. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-FLOW-014` | `permanent` | repo is worktree-exempt | Inspect the flow definition and its run records under the state root. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-FLOW-015` | `permanent` | verify window failed | Inspect the flow definition and its run records under the state root. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-FLOW-016` | `permanent` | promotion denied by policy | Inspect the flow definition and its run records under the state root. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-FLOW-017` | `permanent` | rollback refused | Inspect the flow definition and its run records under the state root. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-FLOW-018` | `permanent` | project not registered/enabled | Inspect the flow definition and its run records under the state root. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-FLOW-019` | `permanent` | comment/cross-ref conflict | Inspect the flow definition and its run records under the state root. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |

### TROUBLE-REGISTRY

18 code(s). Inspect the registry entry and the store it was read from.

| Code | Class | Meaning (from the declaration) | Operator action |
|---|---|---|---|
| `TROUBLE-REGISTRY-001` | `permanent` | module not found | Inspect the registry entry and the store it was read from. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-REGISTRY-002` | `permanent` | args violate the schema | Inspect the registry entry and the store it was read from. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-REGISTRY-003` | `transient` | Check failed | Inspect the registry entry and the store it was read from. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-REGISTRY-004` | `transient` | Apply failed | Inspect the registry entry and the store it was read from. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-REGISTRY-005` | `permanent` | Verify not ok | Inspect the registry entry and the store it was read from. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-REGISTRY-006` | `policy_refused` | capability/kill-switch/skill allowlist/allow-root | Inspect the registry entry and the store it was read from. A policy, not a fault, refused the request: change the config or the requested scope. |
| `TROUBLE-REGISTRY-007` | `policy_refused` | do-not-touch hit | Inspect the registry entry and the store it was read from. A policy, not a fault, refused the request: change the config or the requested scope. |
| `TROUBLE-REGISTRY-008` | `permanent` | required scope not granted | Inspect the registry entry and the store it was read from. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-REGISTRY-009` | `transient` | call exceeded the effective timeout | Inspect the registry entry and the store it was read from. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-REGISTRY-010` | `transient` | module returned a transient error | Inspect the registry entry and the store it was read from. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-REGISTRY-011` | `permanent` | module returned a permanent error | Inspect the registry entry and the store it was read from. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-REGISTRY-012` | `permanent` | idempotency violation | Inspect the registry entry and the store it was read from. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-REGISTRY-013` | `permanent` | conformance harness failure | Inspect the registry entry and the store it was read from. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-REGISTRY-014` | `permanent` | descriptor invalid / schema drift | Inspect the registry entry and the store it was read from. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-REGISTRY-015` | `permanent` | PONR without an explicit module-name grant | Inspect the registry entry and the store it was read from. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-REGISTRY-016` | `permanent` | play or do-not-touch TOML schema invalid | Inspect the registry entry and the store it was read from. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-REGISTRY-017` | `permanent` | play when: expression invalid | Inspect the registry entry and the store it was read from. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-REGISTRY-018` | `permanent` | args failed JSON decoding | Inspect the registry entry and the store it was read from. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |

### TROUBLE-SENTINEL

23 code(s). Inspect the sentinel evaluation, its envelope and the dedup gate.

| Code | Class | Meaning (from the declaration) | Operator action |
|---|---|---|---|
| `TROUBLE-SENTINEL-001` | `permanent` | permanent, 400 envelope framing | Inspect the sentinel evaluation, its envelope and the dedup gate. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SENTINEL-002` | `permanent` | permanent, 413 compressed/item too large | Inspect the sentinel evaluation, its envelope and the dedup gate. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SENTINEL-003` | `permanent` | permanent, 413 decompressed cap/ratio | Inspect the sentinel evaluation, its envelope and the dedup gate. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SENTINEL-004` | `permanent` | permanent, 400 gzip stream invalid | Inspect the sentinel evaluation, its envelope and the dedup gate. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SENTINEL-005` | `permanent` | permanent, 401 no auth material | Inspect the sentinel evaluation, its envelope and the dedup gate. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SENTINEL-006` | `permanent` | permanent, 401 auth material invalid | Inspect the sentinel evaluation, its envelope and the dedup gate. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SENTINEL-007` | `permanent` | permanent, 401 unknown project | Inspect the sentinel evaluation, its envelope and the dedup gate. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SENTINEL-008` | `permanent` | permanent, 401 project disabled | Inspect the sentinel evaluation, its envelope and the dedup gate. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SENTINEL-009` | `permanent` | permanent, boot refusal / 400 | Inspect the sentinel evaluation, its envelope and the dedup gate. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SENTINEL-010` | `transient` | transient, 429 quota / per-IP / concurrency | Inspect the sentinel evaluation, its envelope and the dedup gate. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-SENTINEL-011` | `transient` | transient, 429 global disk budget | Inspect the sentinel evaluation, its envelope and the dedup gate. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-SENTINEL-012` | `permanent` | permanent, 200 legacy /store/ used | Inspect the sentinel evaluation, its envelope and the dedup gate. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SENTINEL-013` | `permanent` | permanent, 200 unknown item type dropped | Inspect the sentinel evaluation, its envelope and the dedup gate. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SENTINEL-014` | `permanent` | permanent, 200 sampled/spooled, 429 dropped | Inspect the sentinel evaluation, its envelope and the dedup gate. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SENTINEL-015` | `permanent` | permanent, 200 spool budget full | Inspect the sentinel evaluation, its envelope and the dedup gate. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SENTINEL-016` | `permanent` | permanent, 200 fingerprint fallback | Inspect the sentinel evaluation, its envelope and the dedup gate. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SENTINEL-017` | `permanent` | permanent, 200 collector parser error | Inspect the sentinel evaluation, its envelope and the dedup gate. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SENTINEL-018` | `permanent` | permanent, 200 partial assembly | Inspect the sentinel evaluation, its envelope and the dedup gate. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SENTINEL-019` | `permanent` | permanent, 400 generic JSON invalid | Inspect the sentinel evaluation, its envelope and the dedup gate. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SENTINEL-020` | `transient` | transient, 200 spool write failed | Inspect the sentinel evaluation, its envelope and the dedup gate. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-SENTINEL-021` | `permanent` | permanent, 405 wrong method | Inspect the sentinel evaluation, its envelope and the dedup gate. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SENTINEL-022` | `permanent` | permanent, 415 unsupported media type | Inspect the sentinel evaluation, its envelope and the dedup gate. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SENTINEL-023` | `permanent` | permanent, boot refusal (exit 13) | Inspect the sentinel evaluation, its envelope and the dedup gate. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |

### TROUBLE-LADDER

21 code(s). Inspect the ladder run and the evidence it recorded for the rung.

| Code | Class | Meaning (from the declaration) | Operator action |
|---|---|---|---|
| `TROUBLE-LADDER-001` | `permanent` | illegal transition | Inspect the ladder run and the evidence it recorded for the rung. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-LADDER-002` | `permanent` | play max_runs exhausted / ceiling | Inspect the ladder run and the evidence it recorded for the rung. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-LADDER-003` | `transient` | host agent lease held elsewhere | Inspect the ladder run and the evidence it recorded for the rung. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-LADDER-004` | `permanent` | two strikes per sig per 24h | Inspect the ladder run and the evidence it recorded for the rung. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-LADDER-005` | `permanent` | verify window invalid | Inspect the ladder run and the evidence it recorded for the rung. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-LADDER-006` | `permanent` | canary not seen | Inspect the ladder run and the evidence it recorded for the rung. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-LADDER-007` | `permanent` | recurrence inside the window | Inspect the ladder run and the evidence it recorded for the rung. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-LADDER-008` | `transient` | rollback of an applied call failed | Inspect the ladder run and the evidence it recorded for the rung. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-LADDER-009` | `transient` | research unavailable | Inspect the ladder run and the evidence it recorded for the rung. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-LADDER-010` | `permanent` | kill-switch active at a checkpoint | Inspect the ladder run and the evidence it recorded for the rung. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-LADDER-011` | `permanent` | autonomy gate denied for this stage | Inspect the ladder run and the evidence it recorded for the rung. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-LADDER-012` | `permanent` | incident id not found in the index | Inspect the ladder run and the evidence it recorded for the rung. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-LADDER-013` | `permanent` | per-day budget exhausted | Inspect the ladder run and the evidence it recorded for the rung. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-LADDER-014` | `permanent` | breaker open for the scope | Inspect the ladder run and the evidence it recorded for the rung. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-LADDER-015` | `transient` | park failed | Inspect the ladder run and the evidence it recorded for the rung. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-LADDER-016` | `transient` | resume failed | Inspect the ladder run and the evidence it recorded for the rung. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-LADDER-017` | `permanent` | quarantined | Inspect the ladder run and the evidence it recorded for the rung. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-LADDER-018` | `permanent` | suppression window active | Inspect the ladder run and the evidence it recorded for the rung. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-LADDER-019` | `permanent` | stabilization window invalid | Inspect the ladder run and the evidence it recorded for the rung. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-LADDER-020` | `permanent` | reopen found a mismatched open incident | Inspect the ladder run and the evidence it recorded for the rung. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-LADDER-021` | `permanent` | agent-stage LLM call failed or exceeded its budgeted cap | Inspect the ladder run and the evidence it recorded for the rung. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |

### TROUBLE-LEDGER

12 code(s). Inspect the ledger files and the writer seat (`trouble ledger status`).

| Code | Class | Meaning (from the declaration) | Operator action |
|---|---|---|---|
| `TROUBLE-LEDGER-001` | `permanent` | append/write to the ledger file failed (source: specs/SPEC-TYPES.md catalog) | Read `reason`: `validation` means the draft supplied a ledger-owned field; `backpressure` means the queue or the disk budget is exhausted. |
| `TROUBLE-LEDGER-002` | `permanent` | sequence gap detected on read (non-monotonic or missing seq) (source: specs/SPEC-TYPES.md catalog) | Inspect the ledger files and the writer seat (`trouble ledger status`). Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-LEDGER-003` | `transient` | torn last line detected; truncated on read, recorded (source: specs/SPEC-TYPES.md catalog) | Inspect the ledger files and the writer seat (`trouble ledger status`). Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-LEDGER-004` | `permanent` | record schema_version newer than this binary supports (source: specs/SPEC-TYPES.md catalog) | Inspect the ledger files and the writer seat (`trouble ledger status`). Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-LEDGER-005` | `permanent` | second writer attempted a ledger file (single-writer violation) (source: specs/SPEC-TYPES.md catalog) | Stop the other writer and let the lock holder drain; never delete or override `ledger/LOCK`. |
| `TROUBLE-LEDGER-006` | `transient` | rotation failed (target file uncreatable) (source: specs/SPEC-TYPES.md catalog) | Inspect the ledger files and the writer seat (`trouble ledger status`). Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-LEDGER-007` | `transient` | compaction (generation rewrite) failed, or an active-day compaction request was refused before the writer seat was taken (§2.4a) (source: specs/SPEC-TYPES.md catalog) | If the request named the current UTC day, stop the daemon or specify a prior day — an active-day refusal is not a compaction rewrite failure. |
| `TROUBLE-LEDGER-008` | `transient` | in-memory index rebuild failed at boot (source: specs/SPEC-TYPES.md catalog) | Inspect the ledger files and the writer seat (`trouble ledger status`). Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-LEDGER-009` | `permanent` | retention blocked: compaction cannot free space without violating tombstone counts (source: specs/SPEC-TYPES.md catalog) | Raise `disk_budget_bytes` or lower `retention_compacted`; compaction will not sacrifice mandatory tombstone counts to free space. |
| `TROUBLE-LEDGER-010` | `transient` | disk budget exceeded (rotation/compaction cannot reclaim enough) (source: specs/SPEC-TYPES.md catalog) | Inspect the ledger files and the writer seat (`trouble ledger status`). Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-LEDGER-011` | `permanent` | corruption detected (payload hash mismatch / unparsable interior line) (source: specs/SPEC-TYPES.md catalog) | Inspect the ledger files and the writer seat (`trouble ledger status`). Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-LEDGER-012` | `permanent` | state root missing/unwritable/wrong mode (source: specs/SPEC-TYPES.md catalog) | Move `state_root` under `$HOME` and `chmod 0700`; `/tmp`, `/var/tmp`, a remote filesystem and a non-0700 root are all refused. |

### TROUBLE-ISSUES

10 code(s). Inspect the issue record and the tracker binding it references.

| Code | Class | Meaning (from the declaration) | Operator action |
|---|---|---|---|
| `TROUBLE-ISSUES-001` | `transient` | driver API error (5xx/reset/TLS/timeout; 422 is a non-retryable instance) | Inspect the issue record and the tracker binding it references. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-ISSUES-002` | `transient` | rate-limited (429, 403 with rate-limit headers, remaining <= min_remaining) | Inspect the issue record and the tracker binding it references. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-ISSUES-003` | `permanent` | auth failed / missing or insecure token file / token on argv | Inspect the issue record and the tracker binding it references. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-ISSUES-004` | `permanent` | issue cap reached (sig/project/global) | Inspect the issue record and the tracker binding it references. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-ISSUES-005` | `transient` | driver spool full or spool write failed | Inspect the issue record and the tracker binding it references. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-ISSUES-006` | `transient` | spool replay failed / read-back mismatch | Inspect the issue record and the tracker binding it references. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-ISSUES-007` | `permanent` | referenced issue not found at the driver | Inspect the issue record and the tracker binding it references. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-ISSUES-008` | `permanent` | close refused (locked / read-back still open) or reopen refused | Inspect the issue record and the tracker binding it references. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-ISSUES-009` | `transient` | healthcheck failed fail_after_probes times | Inspect the issue record and the tracker binding it references. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-ISSUES-010` | `permanent` | duplicate suppressed inside the dedup window | Inspect the issue record and the tracker binding it references. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |

### TROUBLE-RESEARCH

10 code(s). Inspect the research run and its source list.

| Code | Class | Meaning (from the declaration) | Operator action |
|---|---|---|---|
| `TROUBLE-RESEARCH-001` | `transient` | lab unreachable | Inspect the research run and its source list. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-RESEARCH-002` | `permanent` | strict decoder 400 | Inspect the research run and its source list. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-RESEARCH-003` | `permanent` | 409 duplicate → queued | Inspect the research run and its source list. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-RESEARCH-004` | `transient` | solver unavailable / queue failed | Inspect the research run and its source list. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-RESEARCH-005` | `permanent` | slug fell back to unknown | Inspect the research run and its source list. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-RESEARCH-006` | `transient` | poll budget exhausted | Inspect the research run and its source list. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-RESEARCH-007` | `permanent` | corpus grep found nothing | Inspect the research run and its source list. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-RESEARCH-008` | `permanent` | driver = none | Inspect the research run and its source list. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-RESEARCH-009` | `permanent` | returned brief failed validation | Inspect the research run and its source list. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-RESEARCH-010` | `permanent` | response carried no submission_id | Inspect the research run and its source list. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |

### TROUBLE-SCRUB

8 code(s). Inspect the compiled rule set and the record that tripped it.

| Code | Class | Meaning (from the declaration) | Operator action |
|---|---|---|---|
| `TROUBLE-SCRUB-001` | `permanent` | scrub rule failed to compile (source: specs/SPEC-TYPES.md catalog) | Inspect the compiled rule set and the record that tripped it. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SCRUB-002` | `permanent` | scrub config invalid (mandatory rule disabled or malformed) (source: specs/SPEC-TYPES.md catalog) | Inspect the compiled rule set and the record that tripped it. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SCRUB-003` | `transient` | scrub input exceeded the scrubbing byte budget → truncated + flagged (source: specs/SPEC-TYPES.md catalog) | Inspect the compiled rule set and the record that tripped it. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-SCRUB-004` | `permanent` | redaction counter overflow (rule fired > 2^31 times on one value) (source: specs/SPEC-TYPES.md catalog) | Inspect the compiled rule set and the record that tripped it. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SCRUB-005` | `transient` | scrub rule evaluation exceeded its timeout (source: specs/SPEC-TYPES.md catalog) | Inspect the compiled rule set and the record that tripped it. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-SCRUB-006` | `permanent` | mandatory rule missing from the active rule set (refuse to persist) (source: specs/SPEC-TYPES.md catalog) | Inspect the compiled rule set and the record that tripped it. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SCRUB-007` | `permanent` | input is not valid UTF-8 and is not on a binary-allowed target (source: specs/SPEC-TYPES.md catalog) | Reject or lossy-replace non-UTF-8 bytes before the scrubber sees them; the input is refused, never partially scrubbed. |
| `TROUBLE-SCRUB-008` | `permanent` | persistence-boundary re-scan hit: a serialized record still matches a mandatory pattern → the record is refused by the ledger (source: specs/SPEC-TYPES.md catalog) | Fix the caller that skipped scrubbing; do not retry the same bytes — the record was refused at the persistence boundary, not rewritten. |

### TROUBLE-SKILLS

14 code(s). Inspect the skill file and its front-matter.

| Code | Class | Meaning (from the declaration) | Operator action |
|---|---|---|---|
| `TROUBLE-SKILLS-001` | `permanent` | artifact parse/schema invalid (unknown key/table, stats, sig grammar, field rule) | Inspect the skill file and its front-matter. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SKILLS-002` | `permanent` | signature verification failed (base64/length/ed25519/play_sha256/boot re-verify) | Inspect the skill file and its front-matter. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SKILLS-003` | `permanent` | signer_key_id unknown, disabled, or wrong trust class for the origin | Inspect the skill file and its front-matter. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SKILLS-004` | `permanent` | min_daemon_version floor not satisfied, or unstamped build | Inspect the skill file and its front-matter. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SKILLS-005` | `permanent` | play references a module outside allowed_modules | Inspect the skill file and its front-matter. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SKILLS-006` | `permanent` | two skills match one sig, or same version with different canonical bytes | Inspect the skill file and its front-matter. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SKILLS-007` | `permanent` | version lower than the highest installed for that name | Inspect the skill file and its front-matter. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SKILLS-008` | `permanent` | provenance missing at draft time | Inspect the skill file and its front-matter. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SKILLS-009` | `permanent` | candidate rejected at review | Inspect the skill file and its front-matter. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SKILLS-010` | `transient` | pull failed against a reachable source (ref/checkout/size/corrupt cache) | Inspect the skill file and its front-matter. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-SKILLS-011` | `transient` | skills repo unreachable or pull exceeded pull_timeout | Inspect the skill file and its front-matter. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-SKILLS-012` | `permanent` | canary host has no green result for this version inside validity | Inspect the skill file and its front-matter. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SKILLS-013` | `permanent` | skill refused on this host (reason names the cause) | Inspect the skill file and its front-matter. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-SKILLS-014` | `transient` | local stats write failed | Inspect the skill file and its front-matter. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |

### TROUBLE-DASHBOARD

13 code(s). Inspect the dashboard config, the token store and the listener.

| Code | Class | Meaning (from the declaration) | Operator action |
|---|---|---|---|
| `TROUBLE-DASHBOARD-001` | `permanent` | no auth material | Inspect the dashboard config, the token store and the listener. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-DASHBOARD-002` | `permanent` | auth material invalid | Inspect the dashboard config, the token store and the listener. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-DASHBOARD-003` | `permanent` | under-scoped | Inspect the dashboard config, the token store and the listener. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-DASHBOARD-004` | `permanent` | CSRF failed | Inspect the dashboard config, the token store and the listener. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-DASHBOARD-005` | `permanent` | token in a URL | Remove the token from the URL: a query-string or path token is refused before authentication, so a valid credential does not help. |
| `TROUBLE-DASHBOARD-006` | `permanent` | non-loopback bind without a mandate | Inspect the dashboard config, the token store and the listener. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-DASHBOARD-007` | `transient` | template render failed | Inspect the dashboard config, the token store and the listener. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-DASHBOARD-008` | `permanent` | partial/view not found | Inspect the dashboard config, the token store and the listener. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-DASHBOARD-009` | `permanent` | route not found | Inspect the dashboard config, the token store and the listener. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-DASHBOARD-010` | `permanent` | write action refused / stale view | Inspect the dashboard config, the token store and the listener. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-DASHBOARD-011` | `permanent` | body too large | Inspect the dashboard config, the token store and the listener. Retrying unchanged reproduces it: fix the condition, then restart the affected process. |
| `TROUBLE-DASHBOARD-012` | `transient` | rate limited | Inspect the dashboard config, the token store and the listener. Retry once the named dependency recovers; if it recurs, treat it as a chronic fault rather than a blip. |
| `TROUBLE-DASHBOARD-013` | `permanent` | identity provider unavailable | Check that `dashboard.token_file` exists, parses and is mode 0600, then restart; there is no last-known-good fallback. |

### Declared codes outside the family-number shape

| Code | Class | Meaning (from the declaration) | Operator action |
|---|---|---|---|
| `TROUBLE-REDIS-REFUSING` | `transient` | Redis lost at runtime under require_redis=true (hard 503, not the overload 429) (source: internal/types/spec13.go declaration comment + internal/hub/errors.go RuntimeRefusalHTTPStatus) | Answer 503 + `Retry-After` and name the unavailability; this is not rate limiting, so a caller must not treat it as a retry-with-backoff hint. |
