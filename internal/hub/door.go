package hub

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// Door is the ingestion seam: the one call the composition root mounts in front
// of the sentinel's ledger sink when the profile is light-hub.
//
// It exists because the two halves of SPEC-13 §4.2 have to meet somewhere:
//
//	HTTP /store/ | /envelope/ ─► scrub ─► sig ─► dedup gate ─► [light-hub] XADD ─► consumer ─► ledger.Append
//
// The sentinel hands the hub a `RecordDraft` and reads the appended record back
// (its group watermark is taken from the record's seq, internal/sentinel/group.go
// markFlushed), so the hop cannot be a fire-and-forget XADD: the door enqueues,
// waits for the CONSUMER's append+fsync, and returns the record the LEDGER
// produced. Two properties follow, and both are SPEC-13 requirements rather than
// conveniences:
//
//   - The 200-answer the sender receives is backed by a durable ledger record
//     (§4.2: "An event is never confirmed to a sender before it is either acked
//     into the ledger or explicitly refused").
//   - The response record carries the canonical `rec_id` and the real `seq`,
//     because the ledger minted them — the hub never fabricates identity.
//
// A wait that expires is a REFUSAL (TROUBLE-HUB-004, class transient), never a
// 200: the entry may still land, in which case the sender's retry is answered by
// the dedup gate as an idempotent duplicate.
type Door struct {
	c     *Client
	gate  *DedupGate
	route RouteDecision
	wait  time.Duration

	hostID string
	hubID  string
	ackFn  func() uint64

	mu      sync.Mutex
	waiters map[string]chan offerResult

	Offers     atomic.Int64
	Duplicates atomic.Int64
	Timeouts   atomic.Int64
	Failures   atomic.Int64
}

// DoorConfig carries the facts the door cannot read from RedisConfig.
type DoorConfig struct {
	// Route is the routing decision every offer is stamped with. A hub has no
	// upstream, so hub-side ingestion is Route A (SPEC-04 §3.10a); the field
	// stays explicit because the decision belongs to the transport seam, not to
	// this package.
	Route RouteDecision
	// HostID/HubID are the envelope's host identity (origin.host_id and
	// server.hub_id, "" → host id).
	HostID string
	HubID  string
	// Ack is the ordered cursor stamped into the envelope: the highest ledger
	// seq the sender may forget (SPEC-TYPES §3.14). Nil stamps 0.
	Ack func() uint64
	// Wait bounds the wait for the consumer's append. Zero uses DefaultDoorWait.
	Wait time.Duration
}

type offerResult struct {
	recs []types.Record
	err  error
}

// NewDoor builds the door over a client and its gate.
func NewDoor(c *Client, gate *DedupGate, cfg DoorConfig) *Door {
	if gate == nil {
		gate = NewDedupGate(c.cfg, c)
	}
	if cfg.Wait <= 0 {
		cfg.Wait = DefaultDoorWait
	}
	return &Door{
		c:       c,
		gate:    gate,
		route:   cfg.Route,
		wait:    cfg.Wait,
		hostID:  cfg.HostID,
		hubID:   cfg.HubID,
		ackFn:   cfg.Ack,
		waiters: map[string]chan offerResult{},
	}
}

// Route reports the routing decision the door stamps.
func (d *Door) Route() RouteDecision { return d.route }

// Complete is the consumer's completion callback (mount with
// Consumer.WithCompletion(door.Complete)). It answers the waiting offer, if any.
func (d *Door) Complete(entryID string, recs []types.Record, err error) {
	d.mu.Lock()
	ch, ok := d.waiters[entryID]
	if ok {
		delete(d.waiters, entryID)
	}
	d.mu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- offerResult{recs: recs, err: err}:
	default:
	}
}

// Offer enqueues one draft and returns the ledger's record for it.
func (d *Door) Offer(ctx context.Context, draft types.RecordDraft) (types.Record, error) {
	return d.OfferRoute(ctx, draft, d.route)
}

// OfferRoute is Offer with an explicit routing decision (the transport seam's
// per-event-class decision of SPEC-04 §3.10a travels with the event).
func (d *Door) OfferRoute(ctx context.Context, draft types.RecordDraft, route RouteDecision) (types.Record, error) {
	local := localRecord(draft, d.hostID)

	key, keyErr := d.idemKey(draft)
	if keyErr != nil {
		return types.Record{}, keyErr
	}
	fresh, err := d.gate.Claim(ctx, key)
	if err != nil {
		return types.Record{}, err
	}
	if !fresh {
		// The gate answered "present": the idempotent replay of §3.4. The
		// counter is the audit trail; the answer is the local identity because
		// the ORIGINAL record lives in the ledger, which is where a reader
		// looks for it.
		d.Duplicates.Add(1)
		d.gate.Hit()
		d.c.Counters.DedupHits.Add(1)
		return local, nil
	}
	d.c.Counters.DedupMisses.Add(1)

	env := types.ForwardEnvelope{
		ProtocolVersion: 1,
		IdempotencyKey:  key,
		HostID:          d.hostID,
		HubID:           d.hubID,
		Origin:          local.Origin,
		Records:         []types.Record{local},
	}
	if d.ackFn != nil {
		env.Ack = d.ackFn()
	}
	entryID, err := d.c.EnqueueEntry(ctx, env, route)
	if err != nil {
		// The sender's 200 was never issued, so the claim must not survive: a
		// retry with the same idempotency key has to be a fresh acceptance
		// (SPEC-13 §6.1, "a lost XADD costs nothing").
		d.gate.Release(ctx, key)
		d.Failures.Add(1)
		return types.Record{}, err
	}
	d.Offers.Add(1)

	ch := make(chan offerResult, 1)
	d.mu.Lock()
	d.waiters[entryID] = ch
	d.mu.Unlock()

	timer := time.NewTimer(d.wait)
	defer timer.Stop()
	defer func() {
		d.mu.Lock()
		delete(d.waiters, entryID)
		d.mu.Unlock()
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			return types.Record{}, res.err
		}
		if len(res.recs) == 0 {
			// The consumer skipped the append because the entry was already
			// appended under this claim: the answer is the local identity, and
			// the ledger already holds the record (§3.3's re-delivery case).
			return local, nil
		}
		return res.recs[0], nil
	case <-ctx.Done():
		return types.Record{}, ctx.Err()
	case <-timer.C:
		d.Timeouts.Add(1)
		d.c.Counters.Backpressure.Add(1)
		return types.Record{}, errf(types.CodeHub004, ReasonXAdd,
			"enqueued as %s but the consumer did not append within %s", entryID, d.wait)
	}
}

// idemKey derives the door's key for a draft.
func (d *Door) idemKey(draft types.RecordDraft) (string, error) {
	host := d.hostID
	if host == "" {
		host = draft.Origin.HostID
	}
	return IdemKeyForDraft(draft, host)
}

// IdemKeyForDraft is the door's key derivation, callable without a door so a
// caller (or a test) can compute the key an offer WILL use: the spec's
// incident-scoped derivation when the draft carries a parseable sig, and the
// content-derived fallback when it does not (a housekeeping record has no
// incident identity to scope by).
func IdemKeyForDraft(draft types.RecordDraft, hostID string) (string, error) {
	if key, ok := IdemKeyForSig(draft.Sig, hostID); ok {
		return key, nil
	}
	body, err := canonicalJSON(draft.Payload)
	if err != nil {
		return "", errWrap(types.CodeHub007, ReasonDecode, "payload is not canonical JSON", err)
	}
	return ContentIdemKey(draft.Kind, body), nil
}

// GatewayDedup exposes the gate the door claims on, so the app can report the
// gate in `trouble hub dedup`-style probes.
func (d *Door) Dedup() *DedupGate { return d.gate }

// localRecord mints the record the stream carries: the hub's local identity for
// the event. The ledger replaces rec_id at append time and the local id survives
// in `payload.local_rec_id` (SPEC-13 §3.2 rule 5).
func localRecord(draft types.RecordDraft, hostID string) types.Record {
	origin := draft.Origin
	if origin.HostID == "" {
		origin.HostID = hostID
	}
	actor := draft.Actor
	if actor.Kind == "" || actor.ID == "" {
		actor = types.Actor{Kind: types.ActorDaemon, ID: "hub-door"}
	}
	payload := draft.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	return types.Record{
		RecID:         types.NewID(types.PEv),
		TS:            types.NowUTC(),
		Kind:          draft.Kind,
		SchemaVersion: 1,
		Sig:           draft.Sig,
		Inc:           draft.Inc,
		Origin:        origin,
		Actor:         actor,
		Redactions:    draft.Redactions,
		Payload:       payload,
	}
}
