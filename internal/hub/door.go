package hub

import (
	"context"
	"strings"
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
	// pending holds an offer's waiter while its stream entry does not exist
	// yet: it is registered BEFORE the XADD, because a consumer can read the
	// entry and call Complete in the window between the XADD returning and the
	// offer registering `waiters[entryID]` — an answer delivered there used to
	// be dropped on the floor, and the offer stalled out its whole wait to
	// answer a spurious TROUBLE-HUB-004 timeout (measured as a -race flake).
	pending map[string]chan offerResult
	// entryAnswer parks a completion that fired before the offer could
	// re-register its waiter under the entry id. The registration consumes
	// it; one parked after a timed-out or cancelled offer is dropped with the
	// door itself (a rewire builds a new one), so no unbounded state accrues.
	entryAnswer map[string]offerResult

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
		c:           c,
		gate:        gate,
		route:       cfg.Route,
		wait:        cfg.Wait,
		hostID:      cfg.HostID,
		hubID:       cfg.HubID,
		ackFn:       cfg.Ack,
		waiters:     map[string]chan offerResult{},
		pending:     map[string]chan offerResult{},
		entryAnswer: map[string]offerResult{},
	}
}

// Route reports the routing decision the door stamps.
func (d *Door) Route() RouteDecision { return d.route }

// Complete is the consumer's completion callback (mount with
// Consumer.WithCompletion(door.Complete)). It answers the waiting offer, if any.
//
// An entry may complete before its offer has registered a waiter under the
// entry id (the consumer needs no round trip through the sender), so a
// completion that finds no waiter parks its answer for the registration to
// pick up — the answer is the offer's only one, and dropping it cost the
// sender a full door wait for an entry that was already in the ledger.
func (d *Door) Complete(entryID string, recs []types.Record, err error) {
	res := offerResult{recs: recs, err: err}
	d.mu.Lock()
	ch, ok := d.waiters[entryID]
	if ok {
		delete(d.waiters, entryID)
	} else {
		// The offer has not registered yet: park the answer where its
		// registration (or its timeout cleanup) will find it.
		d.entryAnswer[entryID] = res
	}
	d.mu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- res:
	default:
	}
}

// Offer enqueues one draft and returns the ledger's record for it.
func (d *Door) Offer(ctx context.Context, draft types.RecordDraft) (types.Record, error) {
	return d.OfferRoute(ctx, draft, d.route)
}

// OfferRoute is Offer with an explicit routing decision (the transport seam's
// per-event decision of SPEC-04 §3.10a travels with the event).
func (d *Door) OfferRoute(ctx context.Context, draft types.RecordDraft, route RouteDecision) (types.Record, error) {
	key, keyErr := d.idemKey(draft)
	if keyErr != nil {
		return types.Record{}, keyErr
	}
	// The claim travels WITH the record: `localRecord` records the content
	// component of `key` in the payload (IdemDigestField), which is what makes the
	// ledger readable as the idempotency index a later (re)wire re-warms from
	// (SPEC-13 §2.1.1 rule 5a). It cannot be RE-DERIVED later: the key is hashed
	// over the producer's in-memory payload, and a decoded record loses what a struct
	// keeps (field order), so a digest recomputed from the record is a lookalike.
	local := localRecord(draft, d.hostID, key)

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
	ch := make(chan offerResult, 1)
	// The waiter for this offer exists from BEFORE the XADD to the moment the
	// answer arrives: a consumer can drain the entry and call Complete the
	// instant the XADD lands, so the registration must never lag the enqueue.
	// The entry id is not known yet, so the waiter sits under the offer's
	// idempotency key; Complete parks an early answer under the ENTRY id, and
	// the SAME critical section below (which now owns the entry id) moves the
	// waiter under `waiters[entryID]` and consumes any parked answer — no
	// window is left in which a completion is dropped.
	d.mu.Lock()
	d.pending[key] = ch
	d.mu.Unlock()

	entryID, err := d.c.EnqueueEntry(ctx, env, route)
	if err != nil {
		d.mu.Lock()
		delete(d.pending, key)
		d.mu.Unlock()
		// The sender's 200 was never issued, so the claim must not survive: a
		// retry with the same idempotency key has to be a fresh acceptance
		// (SPEC-13 §6.1, "a lost XADD costs nothing").
		d.gate.Release(ctx, key)
		d.Failures.Add(1)
		return types.Record{}, err
	}
	d.mu.Lock()
	if answer, ok := d.entryAnswer[entryID]; ok {
		// The consumer completed the entry while it was being registered:
		// hand the parked answer to the buffered waiter (the select below
		// reads it without another trip through the lock).
		delete(d.entryAnswer, entryID)
		ch <- answer
	}
	d.waiters[entryID] = ch
	delete(d.pending, key)
	d.mu.Unlock()
	d.Offers.Add(1)

	timer := time.NewTimer(d.wait)
	defer timer.Stop()
	defer func() {
		d.mu.Lock()
		delete(d.waiters, entryID)
		// EnqueueEntry may never have returned (the caller's context ended
		// first): the pre-enqueue registration is this offer's to remove.
		delete(d.pending, key)
		// The offer is gone; an answer parked for it after this point is
		// dropped with the door itself (a rewire builds a new one), so the
		// parked map stays bounded by the entries completing without a live
		// offer — never by the stream's history.
		delete(d.entryAnswer, entryID)
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

// IdemKeyForDraft is the door's key derivation for one LOCAL record, and it is
// the reason the door cannot simply reuse the envelope derivation.
//
// SPEC-13 §3.4's key (`sig|norm_version|host_id`) is the ForwardEnvelope's
// idempotency key: it identifies the BATCH a sender chose to send, which is why
// the forward path dedups whole envelopes. The local ingestion path is not that:
// one request produces several DISTINCT records under the SAME sig (an `event`
// record and the `group` record the sentinel folds it into, then the next event
// of that group), so keying the gate on the sig alone would drop every record
// after the first — a live run proved exactly that (one entry in the stream for
// a request that wrote two records, with the second answered as an idempotent
// duplicate).
//
// The local derivation therefore keeps the spec's scope as its prefix and adds
// what identifies the record itself: its kind and a digest of its canonical
// payload.
//
//	<sig>|<norm_version>|<host_id>|<kind>|content:sha256v1:<16>
//
// Two consequences, both wanted: a retry of the SAME record (the sender re-sends
// the same event id and body) is the same key and is deduped, while two records
// that merely share an incident are distinct. A draft with no parseable sig keeps
// the content-only key (ContentIdemKey), because there is no incident scope to
// prefix.
func IdemKeyForDraft(draft types.RecordDraft, hostID string) (string, error) {
	body, err := canonicalJSON(draft.Payload)
	if err != nil {
		return "", errWrap(types.CodeHub007, ReasonDecode, "payload is not canonical JSON", err)
	}
	content := ContentIdemKey(draft.Kind, body)
	if scope, ok := IdemKeyForSig(draft.Sig, hostID); ok {
		return scope + "|" + string(draft.Kind) + "|" + content, nil
	}
	return content, nil
}

// GatewayDedup exposes the gate the door claims on, so the app can report the
// gate in `trouble hub dedup`-style probes.
func (d *Door) Dedup() *DedupGate { return d.gate }

// IdemDigestField is the payload field that carries the CONTENT component of the
// dedup key the door claimed for this record — `content:sha256v1:<16>` (SPEC-13
// §2.1.1 rule 5a, §3.2 rule 5).
//
// It is recorded rather than recomputed because the key is hashed over the
// producer's IN-MEMORY payload: the sentinel puts a Go struct in
// `payload["event"]`, Go marshals a struct in field order, and a record that has
// been through JSON (the stream entry, then the ledger) holds that object as a
// map, which marshals sorted. The two digests differ, so a digest re-derived from
// a decoded record is a lookalike that matches no claim — measured live on a real
// Redis run, which is why the index READS this field instead of re-deriving it.
//
// The rest of the key is not stored because the record already carries it
// verbatim: the sig (scope), the kind, and the daemon's host id.
const IdemDigestField = "idem_digest"

// contentComponent returns the content-derived part of a dedup key: the last
// `|`-separated component, which is the only part not implied by the record's own
// fields. A content-only key (a record with no parseable sig) returns itself.
func contentComponent(key string) string {
	if i := strings.LastIndex(key, "|"); i >= 0 {
		return key[i+1:]
	}
	return key
}

// localRecord mints the record the stream carries: the hub's local identity for
// the event. The ledger replaces rec_id at append time and the local id survives
// in `payload.local_rec_id` (SPEC-13 §3.2 rule 5).
//
// `claim` is the dedup key the door took for this record; a non-empty claim is
// recorded as IdemDigestField so the ledger can answer "was this key already
// claimed?" without re-deriving anything. The payload is COPIED when the claim is
// recorded (and when it is nil): the draft's map belongs to the producer, and the
// hub does not add a field behind its back.
func localRecord(draft types.RecordDraft, hostID, claim string) types.Record {
	origin := draft.Origin
	if origin.HostID == "" {
		origin.HostID = hostID
	}
	actor := draft.Actor
	if actor.Kind == "" || actor.ID == "" {
		actor = types.Actor{Kind: types.ActorDaemon, ID: "hub-door"}
	}
	payload := draft.Payload
	if claim != "" || payload == nil {
		next := make(map[string]any, len(payload)+1)
		for k, v := range payload {
			next[k] = v
		}
		payload = next
	}
	if claim != "" {
		payload[IdemDigestField] = contentComponent(claim)
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
