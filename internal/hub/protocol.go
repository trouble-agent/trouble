package hub

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// RouteDecision is the routing decision of SPEC-TYPES §3.15.3 ("A" direct
// loopback, "B" proxied through a hub) as consumed by SPEC-04 §3.10a.
//
// It is declared here rather than imported because internal/types does not carry
// it in this tree (types.Origin has no Route field either); declaring the token
// type in the package that WRITES the stream entry keeps the wire format honest
// instead of fabricating a record field that the shared type does not have.
type RouteDecision string

const (
	// RouteA is the direct path: sensor → local daemon over loopback. A hub has
	// no upstream, so hub-side sensors always take A (SPEC-04 §3.10a).
	RouteA RouteDecision = "A"
	// RouteB is the proxied path: satellite → hub (the ForwardEnvelope path).
	RouteB RouteDecision = "B"
)

// Valid reports whether r is one of the two pinned routes (or the empty value,
// which a non-sensor path legitimately carries).
func (r RouteDecision) Valid() bool {
	return r == "" || r == RouteA || r == RouteB
}

// StreamEnvelope is the payload of one stream entry: the ForwardEnvelope of
// SPEC-TYPES §3.14 exactly as the satellite→hub path defines it (embedded, so
// the JSON keys are the shared type's, unmarshalled unchanged), plus the two
// facts SPEC-13 §3.2 adds to the queue's own copy of the event:
//
//   - Route: the routing decision the entry was enqueued under (§2.4: "the
//     Route A/B decision is taken per event class, then the same Enqueue call").
//   - EnqueuedTS: when the hub accepted it. It is the arrival order's timestamp
//     witness and is never used for ordering (XADD order is the order).
type StreamEnvelope struct {
	types.ForwardEnvelope
	Route      RouteDecision `json:"route"`
	EnqueuedTS string        `json:"enqueued_ts"`
}

// StreamField is the single field name every entry carries (SPEC-13 §3.2: "One
// stream field per entry, `env`").
const StreamField = "env"

// EncodeFields renders the entry's Redis field map for XADD.
func (e StreamEnvelope) EncodeFields() (map[string]any, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return nil, errWrap(types.CodeHub007, ReasonDecode, "cannot encode envelope", err)
	}
	return map[string]any{StreamField: string(b)}, nil
}

// EnvelopeBytes is the marshalled size of the entry as it lands on the wire.
func (e StreamEnvelope) EnvelopeBytes() int {
	b, err := json.Marshal(e)
	if err != nil {
		return 0
	}
	return len(b)
}

// DecodeEntry decodes one stream entry.
//
// Every refusal is TROUBLE-HUB-007 (class permanent, SPEC-13 §5): an entry that
// does not decode as a ForwardEnvelope — bad JSON, an unknown protocol version,
// or an over-cap batch — is dead-lettered with a reason and never acked as
// success. The batch cap is applied to the DECOMPRESSED form the hub sees, which
// is the JSON length of `records` (§3.2 rule 1: `batch_bytes` bounds decompressed
// bytes).
func DecodeEntry(fields map[string]string, cfg RedisConfig) (StreamEnvelope, error) {
	raw, ok := fields[StreamField]
	if !ok || raw == "" {
		return StreamEnvelope{}, errf(types.CodeHub007, ReasonDecode, "entry carries no %q field", StreamField)
	}
	var env StreamEnvelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return StreamEnvelope{}, errWrap(types.CodeHub007, ReasonDecode, "entry is not a ForwardEnvelope", err)
	}
	if env.ProtocolVersion != 1 {
		return StreamEnvelope{}, errf(types.CodeHub007, ReasonDecode,
			"protocol_version %d is not supported (want 1)", env.ProtocolVersion)
	}
	if len(env.Records) > cfg.BatchRecs {
		return StreamEnvelope{}, errf(types.CodeHub007, ReasonDecode,
			"batch carries %d records, cap is %d", len(env.Records), cfg.BatchRecs)
	}
	rb, err := json.Marshal(env.Records)
	if err != nil {
		return StreamEnvelope{}, errWrap(types.CodeHub007, ReasonDecode, "records are not encodable", err)
	}
	if len(rb) > cfg.BatchBytes {
		return StreamEnvelope{}, errf(types.CodeHub007, ReasonDecode,
			"batch is %d bytes, cap is %d", len(rb), cfg.BatchBytes)
	}
	if !env.Route.Valid() {
		return StreamEnvelope{}, errf(types.CodeHub007, ReasonDecode, "route %q is not A|B", env.Route)
	}
	return env, nil
}

// DeadLetterStream is the stream an undecodable entry is moved to (SPEC-13 §5,
// TROUBLE-HUB-007): the entry keeps its reason instead of being silently acked.
func DeadLetterStream(stream string) string { return stream + ":dead" }

// ---- idempotency-key derivation (one derivation, two consumers) ----

// IdemKey is the shared derivation of SPEC-12 §3.7 / SPEC-13 §3.4:
// `sig + "|" + itoa(norm_version) + "|" + host_id`.
func IdemKey(sig string, normVersion int, hostID string) string {
	return sig + "|" + strconv.Itoa(normVersion) + "|" + hostID
}

// DedupKey is the Redis key of the dedup gate: `<prefix> + IdemKey`.
func DedupKey(prefix, sig string, normVersion int, hostID string) string {
	return prefix + IdemKey(sig, normVersion, hostID)
}

// IdemKeyForSig derives the idempotency key of a record that carries a sig.
// A sig that does not parse yields ok=false: the caller must then use the
// content-derived key rather than inventing a norm_version, because a wrong
// version in the key is a wrong claim about which normalization produced it.
func IdemKeyForSig(sig, hostID string) (string, bool) {
	if sig == "" {
		return "", false
	}
	s, err := types.ParseSig(sig)
	if err != nil {
		return "", false
	}
	norm := s.NormVersion
	if norm == 0 {
		norm = types.NormVersionV1
	}
	return IdemKey(sig, norm, hostID), true
}

// ContentIdemKey derives a key for a record with no usable sig (a housekeeping
// `gap`/`canary` record, or a sig that does not parse). It is hub-local on
// purpose: the spec's derivation is incident-scoped and cannot cover a record
// with no incident identity, so the fallback is content-derived — the same bytes
// replay to the same key, which is the only property the gate needs to make a
// replay a hit rather than a second record.
func ContentIdemKey(kind types.RecordKind, canonicalPayload []byte) string {
	h := sha256.New()
	h.Write([]byte("trouble.hub.idem.v1\x1f"))
	h.Write([]byte(kind))
	h.Write([]byte{0x1f})
	h.Write(canonicalPayload)
	h.Write([]byte{0x1e})
	return "content:sha256v1:" + hex.EncodeToString(h.Sum(nil))[:16]
}

// EntryIDParts splits a Redis stream id ("<ms>-<seq>") into its two numbers.
func EntryIDParts(id string) (ms uint64, seq uint64, err error) {
	var a, b string
	for i := 0; i < len(id); i++ {
		if id[i] == '-' {
			a, b = id[:i], id[i+1:]
			break
		}
	}
	if a == "" {
		return 0, 0, fmt.Errorf("stream id %q is not <ms>-<seq>", id)
	}
	ms, err = strconv.ParseUint(a, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("stream id %q: %w", id, err)
	}
	if b != "" {
		seq, err = strconv.ParseUint(b, 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("stream id %q: %w", id, err)
		}
	}
	return ms, seq, nil
}
