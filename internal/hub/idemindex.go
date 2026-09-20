package hub

import (
	"fmt"

	"github.com/trouble-agent/trouble/internal/types"
)

// idemindex.go is the ledger half of SPEC-13 §2.1.1's recovery posture: "the
// dedup gate re-warms from the ledger's idempotency index". The gate is a
// Redis-side optimisation over the record (§3.4 says so outright — the dedup
// gate is NOT the exactly-once guarantee); the LEDGER is the record, so the index
// a re-warm reads is what the hub WROTE with each record it claimed.

// LedgerIndex is the READ half of the ledger as the re-warm needs it: the
// sequence the ledger runs to, and SPEC-01's one supported bulk walk. `Appender`
// (consume.go) is the write half; the composition root's sink implements both
// because it is the same ledger, and passing it here is what turns "the gate
// re-warms from the ledger's idempotency index" into code. A nil LedgerIndex is
// legal and means "no index": the gate then reports restored_keys=0 instead of
// pretending the window is warm.
type LedgerIndex interface {
	// ScanFrom walks the ledger's records in sequence order from `from`,
	// stopping as soon as yield returns false.
	ScanFrom(from uint64, yield func(types.Record) bool) error
	// LastSeq is the ledger's current last sequence (0 for an empty ledger).
	LastSeq() uint64
}

// localRecIDField is the payload field the stream→ledger conversion adds
// (consume.go draftFromRecord, SPEC-13 §3.2 rule 5: "the local id stays in
// payload.local_rec_id"). It is the hub's own artifact of the append, kept
// documented here because the two hub-added identity fields are what the ledger
// carries beyond the producer's payload.
const localRecIDField = "local_rec_id"

// IdemKeyForRecord reads back the dedup key of a record that is ALREADY in the
// ledger.
//
// The content component is READ from the record (`payload.idem_digest`, written
// by the door with the claim it took) and never recomputed. That is not a
// shortcut, it is the only exact option, and it was measured: the door hashes the
// producer's IN-MEMORY payload, the sentinel puts a Go STRUCT in
// `payload["event"]`, and a payload that has been through JSON holds that object
// as a map. Re-marshalling a decoded record therefore produces a DIFFERENT digest
// (live run: `content:sha256v1:20fd49f31373b3da` re-derived vs
// `content:sha256v1:17b5c28aebcdd8a8` claimed) — a lookalike key that matches no
// claim and would make a warm-looking index protect nothing. The rest of the key
// IS reconstructive because the record carries it verbatim: the sig (the scope),
// the kind, and the daemon's host id.
//
// ok=false means the record carries no claim: a record this hub did not ingest
// through the door (a lifecycle record it writes itself, a record written before
// this identity existed, an entry enqueued by a forward path with an
// envelope-scoped key). Such a record is simply absent from the index — the
// window is never larger than the evidence for it — and a key the index misses
// costs the one duplicate §3.4 prices, never a suppressed event.
func IdemKeyForRecord(rec types.Record, hostID string) (string, bool) {
	content, _ := rec.Payload[IdemDigestField].(string)
	if content == "" {
		return "", false
	}
	host := hostID
	if host == "" {
		// The key's host component is the DAEMON's own host id — it is what the
		// door stamped — so a caller with no host id falls back to the record's
		// origin, which is the same fallback the door's derivation takes.
		host = rec.Origin.HostID
	}
	if scope, ok := IdemKeyForSig(rec.Sig, host); ok {
		return scope + "|" + string(rec.Kind) + "|" + content, true
	}
	return content, true
}

// ledgerIdemKeys walks the ledger's TAIL and returns the idempotency keys its
// records carry — the index a (re)wire seeds the gate with (SPEC-13 §2.1.1 rule
// 5a).
//
// The bound is the gate's own capacity: the walk starts at
// `last_seq − limit + 1` and stops after `limit` keys, so a re-warm costs one
// bounded scan of at most `limit` records and the restored index can never be
// larger than the LRU it lands in (`hub.dedup_lru`, 65536 by default). Without
// the bound, restoring a 30 MB ledger on every Redis return would be the
// recovery mechanism's own outage.
func ledgerIdemKeys(idx LedgerIndex, hostID string, limit int) ([]string, error) {
	if idx == nil || limit <= 0 {
		return nil, nil
	}
	from := uint64(1)
	if last := idx.LastSeq(); last > uint64(limit) {
		from = last - uint64(limit) + 1
	}
	keys := make([]string, 0, limit)
	err := idx.ScanFrom(from, func(rec types.Record) bool {
		key, ok := IdemKeyForRecord(rec, hostID)
		if !ok {
			return true
		}
		keys = append(keys, key)
		return len(keys) < limit
	})
	if err != nil {
		return keys, fmt.Errorf("ledger idempotency index: %w", err)
	}
	return keys, nil
}
