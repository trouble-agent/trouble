// Package hub owns the SPEC-13 server profile at runtime: the light-hub
// ingestion buffer (Redis stream + consumer group), the dedup gate, and the
// DuckBrain archival tier. The profile decision itself is resolved by
// internal/lifecycle (SPEC-12 §3.1, SPEC-13 §2.1); this package consumes it.
//
// Four SPEC-13 §1 properties are design constraints, not features, and they are
// the reason every seam below is shaped the way it is:
//
//  1. The ledger file stays the only durable record. Redis is a QUEUE and a dedup
//     gate, never a store of record. The consumer XACKs an entry only AFTER the
//     ledger's group-commit fsync returned (SPEC-01 §2.1), which is exactly the
//     `Append` returning: internal/ledger's Append does not return until the
//     batch holding the record is fdatasync'd (SPEC-01 §2.1). An entry present in
//     Redis and absent from the ledger is un-acked work, not evidence.
//  2. The profile changes plumbing only. Nothing in this package branches on the
//     rule engine, the ladder, verification, the dashboard or the kill-switch;
//     the same event stream produces the same incident/verify sequence under both
//     profiles because the only difference is the hop between `Append` and its
//     caller.
//  3. One writer. The consumer group has exactly one consumer per state root
//     (`consumer = "" → origin.host_id`), so TROUBLE-LEDGER-005's single-writer
//     invariant is preserved: parallelism lives in the queue, never in the
//     writer.
//  4. Degradation is a designed state, never a silent success. Redis down,
//     DuckBrain down and `require_redis=true` each have a named behaviour, a
//     code (TROUBLE-HUB-001..016, SPEC-13 §5) and a reason on the one health
//     surface (§4.3). The standalone profile is the light-hub profile's
//     degradation target, which is why the switch is a config field and not a
//     second binary.
//
// Boundaries stated once, so the next reader does not have to re-derive them:
//
//   - internal/hub imports internal/types and internal/lifecycle (the resolved
//     config read). It does not import internal/ledger: the ledger enters as the
//     narrow `Appender` seam (`Append(ctx, RecordDraft) (Record, error)`, the
//     ledger's own signature), so this package cannot reach around the writer
//     and the import graph stays acyclic (SPEC-13 §8).
//   - The Redis client enters as the `Streams` seam. Production uses
//     github.com/redis/go-redis/v9 (SPEC-13 §8, pure Go, CGO_ENABLED=0 clean);
//     tests use an in-memory implementation of the same seam, so no test needs a
//     live Redis.
//   - The archival target enters as the `Target` seam; the shipped
//     implementation speaks the DuckBrain KV contract internal/issues already
//     uses (`PUT/GET /v1/kv/<key>` with a `{"value": …}` JSON body), so the
//     object store is a real HTTP dependency and not an invented one.
//
// What this package does not do, stated plainly rather than implied: the
// sentinel's own listener and route selection are untouched. The composition
// root mounts `Runtime.Ingest` in front of the sentinel's ledger sink when the
// profile is light-hub (internal/app/hub.go, `hubSink`), which is the ONLY
// change the profile makes to the ingestion path: every other box of SPEC-13
// §4.2 — scrub, sig, dedup core semantics, the ladder hand-off — is the same
// code in both profiles, and the record the door returns is the one the LEDGER
// wrote, so the sentinel's group-watermark arithmetic sees no difference.
package hub
