package hub

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// Appender is the ledger's write seam: exactly the signature of
// `(*ledger.Ledger).Append` (SPEC-01 §2.2).
//
// It is a one-method interface on purpose. SPEC-01 §2.1 pins the durability
// statement ("A record is durable the moment `Append` returns"), so the hub can
// state the ack-after-fsync rule as `Append` then `XACK` and mean it literally:
// there is no path in this package where an entry is acked without an Append
// having returned for its records.
type Appender interface {
	Append(ctx context.Context, d types.RecordDraft) (types.Record, error)
}

// CompletionFunc is called once per stream entry, after the entry's records are
// durable (or after the consumer decided the entry needs no append). It is how
// the ingestion door learns the ledger's answer for the entry it enqueued; a
// consumer with no door simply has no completion function.
type CompletionFunc func(entryID string, recs []types.Record, err error)

// ConsumeStats is the consumer's counter snapshot (SPEC-13 §3.3 reports these on
// the health surface).
type ConsumeStats struct {
	Acked        int64
	Appended     int64
	DedupSkips   int64
	DecodeErrors int64
	Reclaims     int64
	Stranded     int64
	Batches      int64
	ReadErrors   int64
	Idle         int64
}

// Consumer is the SPEC-13 §3.3 ledger writer: XREADGROUP → (decode) → Append →
// XACK, one entry batch at a time, with XAUTOCLAIM for work stranded by a dead
// consumer.
type Consumer struct {
	c    *Client
	w    Appender
	gate *DedupGate
	cfg  RedisConfig

	complete   CompletionFunc
	onStranded func(rounds, entries int)
	logf       func(format string, args ...any)
	sleep      func(ctx context.Context, d time.Duration) bool

	stats struct {
		acked, appended, dedupSkips, decodeErrors atomic.Int64
		reclaims, stranded, batches, readErrors   atomic.Int64
		idle                                      atomic.Int64
	}

	mu           sync.Mutex
	claimCursor  string
	strandRounds int
}

// NewConsumer builds a consumer over a client, a ledger appender and the gate.
// The gate may be nil: the consumer then appends every delivery, which is the
// correct behaviour for `hub drain`-style one-shot consumption and for tests that
// are not about dedup — and it is never the shipped light-hub configuration
// (Runtime always supplies the gate).
func NewConsumer(c *Client, w Appender, gate *DedupGate, cfg RedisConfig) *Consumer {
	cfg = cfg.WithDefaults()
	if gate == nil {
		gate = NewDedupGate(cfg, nil)
	}
	return &Consumer{
		c:     c,
		w:     w,
		gate:  gate,
		cfg:   cfg,
		sleep: sleepCtx,
		logf:  func(string, ...any) {},
	}
}

// WithCompletion installs the per-entry completion callback.
func (cs *Consumer) WithCompletion(fn CompletionFunc) *Consumer {
	cs.complete = fn
	return cs
}

// WithStrandedHook installs the TROUBLE-HUB-008 hook: it fires once per reclaim
// round that took back entries with no consumer making progress, so the caller
// can write the one lifecycle record §3.3 asks for.
func (cs *Consumer) WithStrandedHook(fn func(rounds, entries int)) *Consumer {
	cs.onStranded = fn
	return cs
}

// WithLogger installs a diagnostic logger (never a record; records are the
// ledger's).
func (cs *Consumer) WithLogger(fn func(format string, args ...any)) *Consumer {
	if fn != nil {
		cs.logf = fn
	}
	return cs
}

// Consume is SPEC-13 §2.3's function: consume the stream into the ledger until
// ctx is cancelled. A nil error means a clean shutdown (ctx cancelled); a
// TROUBLE-HUB-005 means the consumer must not run at all. Transient Redis
// failures are retried inside the loop with backoff, because §4.3 keeps the
// daemon running through a Redis outage and rewires the consumer when Redis
// returns.
func Consume(ctx context.Context, c *Client, w Appender, cfg RedisConfig) error {
	return NewConsumer(c, w, nil, cfg).Run(ctx)
}

// Run is the consumer loop.
func (cs *Consumer) Run(ctx context.Context) error {
	backoff := 50 * time.Millisecond
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		if err := cs.reclaim(ctx); err != nil {
			if CodeOf(err) == types.CodeHub005 {
				return err // the group is unusable: never serve a queue with no drainer
			}
			cs.stats.readErrors.Add(1)
			cs.logf("hub: reclaim failed: %v", err)
			if !cs.sleep(ctx, backoff) {
				return nil
			}
			backoff = nextBackoff(backoff)
			continue
		}
		entries, err := cs.c.s.XReadGroup(ctx, ReadRequest{
			Stream:   cs.cfg.Stream,
			Group:    cs.cfg.Group,
			Consumer: cs.c.ConsumerName(),
			Count:    cs.cfg.BatchRecs,
			Block:    time.Duration(cs.cfg.BlockMS) * time.Millisecond,
			Start:    ">",
		})
		if err != nil {
			if IsAuthError(err) {
				// An auth loss is transient in the matrix (002 says "fix the URL";
				// the daemon keeps running and says so rather than dying with the
				// queue intact but undrained, §4.3 Redis ACL row).
				cs.stats.readErrors.Add(1)
				cs.logf("hub: XREADGROUP refused: %v", err)
				if !cs.sleep(ctx, backoff) {
					return nil
				}
				backoff = nextBackoff(backoff)
				continue
			}
			cs.stats.readErrors.Add(1)
			cs.logf("hub: XREADGROUP failed: %v", err)
			if !cs.sleep(ctx, backoff) {
				return nil
			}
			backoff = nextBackoff(backoff)
			continue
		}
		backoff = 50 * time.Millisecond
		if len(entries) == 0 {
			cs.stats.idle.Add(1)
			continue
		}
		cs.stats.batches.Add(1)
		if err := cs.handleBatch(ctx, entries); err != nil {
			// A failed batch is NOT acked: the entries stay pending and are
			// re-delivered (or reclaimed), and the dedup gate decides what the
			// re-delivery means (SPEC-13 §3.3).
			cs.logf("hub: batch not acked: %v", err)
			if CodeOf(err) == types.CodeHub005 {
				return err
			}
			if !cs.sleep(ctx, backoff) {
				return nil
			}
			backoff = nextBackoff(backoff)
		}
	}
}

// handleBatch processes one delivery batch and acks exactly the entries whose
// records reached the ledger. The XACK is one call for the whole batch
// (SPEC-13 §3.3: "one `XACK` per batch"), and it happens strictly after every
// Append in the batch returned.
func (cs *Consumer) handleBatch(ctx context.Context, entries []StreamEntry) error {
	acked := make([]string, 0, len(entries))
	for _, ent := range entries {
		if _, err := cs.handleEntry(ctx, ent); err != nil {
			if len(acked) > 0 {
				// Acknowledge what is durable; the rest is re-delivered. Partial
				// acks are safe precisely because the gate's `appended` phase
				// makes a re-delivery a no-op.
				if _, aerr := cs.c.s.XAck(ctx, cs.cfg.Stream, cs.cfg.Group, acked...); aerr != nil {
					return classifyRedis("XACK", aerr)
				}
				cs.stats.acked.Add(int64(len(acked)))
				cs.c.SetLastAcked(acked[len(acked)-1])
			}
			return err
		}
		acked = append(acked, ent.ID)
	}
	if len(acked) == 0 {
		return nil
	}
	if _, err := cs.c.s.XAck(ctx, cs.cfg.Stream, cs.cfg.Group, acked...); err != nil {
		return classifyRedis("XACK", err)
	}
	cs.stats.acked.Add(int64(len(acked)))
	cs.c.SetLastAcked(acked[len(acked)-1])
	// Progress resets the stranded counter: TROUBLE-HUB-008 is about rounds that
	// made NO progress (§3.3), so one acked batch proves the consumer is alive.
	cs.mu.Lock()
	cs.strandRounds = 0
	cs.mu.Unlock()
	return nil
}

// handleEntry brings ONE stream entry to its durable state.
//
// Order is the whole point, and it is asserted by consume_test.go:
//
//	decode → dedup state → Append (fsync) → mark appended → XACK (by the caller)
//
// Nothing is acked before the append returned, and nothing is appended twice for
// one claim.
func (cs *Consumer) handleEntry(ctx context.Context, ent StreamEntry) ([]types.Record, error) {
	env, err := DecodeEntry(ent.Fields, cs.cfg)
	if err != nil {
		return nil, cs.deadLetter(ctx, ent, err)
	}
	key := cs.idemKey(env)
	state, gerr := cs.gate.State(ctx, key)
	if gerr != nil {
		// An unreadable gate is the LRU fallback's job (006); it never blocks the
		// ledger write path, because the ledger is the record and the gate is an
		// optimisation over it (SPEC-13 §1 rule 1).
		cs.logf("hub: dedup gate unreadable: %v", gerr)
	}
	if state == DedupAppended {
		// The previous attempt got the records in and died before the XACK.
		// Re-delivery lands zero new records (§3.3), which is the exactly-once
		// property stated as a counter.
		cs.gate.Hit()
		cs.stats.dedupSkips.Add(1)
		cs.completeEntry(ent.ID, nil, nil)
		return nil, nil
	}

	recs := make([]types.Record, 0, len(env.Records))
	for _, rec := range env.Records {
		out, err := cs.w.Append(ctx, draftFromRecord(rec))
		if err != nil {
			return recs, errWrap(types.CodeHub004, ReasonXAdd, "ledger append failed for stream entry "+ent.ID, err)
		}
		recs = append(recs, out)
	}
	// The ledger's Append has returned: every record of this entry is durable
	// (SPEC-01 §2.1). Only now may the claim move to `appended`.
	cs.gate.MarkAppended(ctx, key)
	if state == DedupAbsent && len(recs) > 0 {
		// The claim was lost between the door and here (failover, flush, or a
		// TTL that closed under the entry). §3.4 prices this as exactly one
		// duplicate entry; the gate counts it so the tier's behaviour is
		// visible instead of assumed.
		cs.gate.Conflict()
		cs.c.Counters.DedupConflicts.Add(1)
	}
	cs.stats.appended.Add(int64(len(recs)))
	cs.c.Counters.Appended.Add(int64(len(recs)))
	cs.completeEntry(ent.ID, recs, nil)
	return recs, nil
}

// deadLetter moves an undecodable entry to `<stream>:dead` with its reason and
// records a `gap` for it (SPEC-13 §5, TROUBLE-HUB-007). The original entry is
// acked only once the copy exists, so the dead letter is the entry's new home
// rather than a second silent drop.
func (cs *Consumer) deadLetter(ctx context.Context, ent StreamEntry, cause error) error {
	cs.stats.decodeErrors.Add(1)
	cs.c.Counters.DecodeErrors.Add(1)
	values := map[string]any{
		"entry_id":   ent.ID,
		"stream":     cs.cfg.Stream,
		"reason":     ReasonOf(cause),
		"error_code": string(types.CodeHub007),
		"detail":     cause.Error(),
		"ts":         types.NowUTC(),
	}
	for k, v := range ent.Fields {
		values["raw_"+k] = v
	}
	if _, err := cs.c.s.XAdd(ctx, DeadLetterStream(cs.cfg.Stream), 0, values); err != nil {
		return classifyRedis("XADD dead-letter", err)
	}
	if cs.w != nil {
		_, _ = cs.w.Append(ctx, types.RecordDraft{
			Kind:   types.KGap,
			Origin: types.Origin{HostID: cs.cfg.HostID, Source: "hub"},
			Actor:  types.Actor{Kind: types.ActorDaemon, ID: "hub-consumer"},
			Payload: map[string]any{
				"cause":      "hub_entry_undecodable",
				"scope":      ent.ID,
				"est_lost":   len(ent.Fields),
				"error_code": string(types.CodeHub007),
				"detail":     cause.Error(),
			},
		})
	}
	cs.completeEntry(ent.ID, nil, cause)
	return nil
}

// reclaim takes back entries stranded by a consumer that died (SPEC-13 §3.3) and
// records the stranded case (TROUBLE-HUB-008) once per reclaim round.
func (cs *Consumer) reclaim(ctx context.Context) error {
	pending, err := cs.c.Pending(ctx)
	if err != nil {
		return err
	}
	if pending.Count == 0 {
		cs.mu.Lock()
		cs.strandRounds = 0
		cs.claimCursor = ""
		cs.mu.Unlock()
		return nil
	}
	cs.mu.Lock()
	start := cs.claimCursor
	if start == "" {
		start = "0-0"
	}
	cs.mu.Unlock()
	res, err := cs.c.s.XAutoClaim(ctx, ClaimRequest{
		Stream:   cs.cfg.Stream,
		Group:    cs.cfg.Group,
		Consumer: cs.c.ConsumerName(),
		MinIdle:  cs.cfg.ClaimIdle,
		Start:    start,
		Count:    cs.cfg.BatchRecs,
	})
	if err != nil {
		if IsAuthError(err) {
			return classifyRedis("XAUTOCLAIM", err)
		}
		// A mistyped or illegal XAUTOCLAIM is 005 (permanent): the consumer must
		// not pretend to drain a queue it cannot reclaim.
		return errWrap(types.CodeHub005, ReasonGroup, "XAUTOCLAIM failed", err)
	}
	cs.mu.Lock()
	cs.claimCursor = res.Next
	cs.mu.Unlock()
	if len(res.Entries) == 0 {
		return nil
	}
	cs.c.Counters.Reclaims.Add(1)
	cs.stats.reclaims.Add(1)
	cs.mu.Lock()
	cs.strandRounds++
	rounds := cs.strandRounds
	cs.mu.Unlock()
	if rounds >= DefaultStrandedRounds {
		cs.stats.stranded.Add(1)
		cs.logf("hub: %d entries stranded past %s (round %d): TROUBLE-HUB-008",
			len(res.Entries), cs.cfg.ClaimIdle, rounds)
		if cs.onStranded != nil {
			cs.onStranded(rounds, len(res.Entries))
		}
	}
	return cs.handleBatch(ctx, res.Entries)
}

// Stats is the counter snapshot.
func (cs *Consumer) Stats() ConsumeStats {
	return ConsumeStats{
		Acked:        cs.stats.acked.Load(),
		Appended:     cs.stats.appended.Load(),
		DedupSkips:   cs.stats.dedupSkips.Load(),
		DecodeErrors: cs.stats.decodeErrors.Load(),
		Reclaims:     cs.stats.reclaims.Load(),
		Stranded:     cs.stats.stranded.Load(),
		Batches:      cs.stats.batches.Load(),
		ReadErrors:   cs.stats.readErrors.Load(),
		Idle:         cs.stats.idle.Load(),
	}
}

// Drain consumes and acks what the stream holds right now (or until the timeout)
// and then stops — the `trouble hub drain` verb of SPEC-13 §2.2. It never
// deletes an un-acked entry and never stops the live path (§6.14).
func (cs *Consumer) Drain(ctx context.Context, timeout time.Duration) (ConsumeStats, error) {
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			return cs.Stats(), errf(types.CodeHub008, ReasonStranded,
				"drain timed out with pending entries")
		}
		entries, err := cs.c.s.XReadGroup(ctx, ReadRequest{
			Stream:   cs.cfg.Stream,
			Group:    cs.cfg.Group,
			Consumer: cs.c.ConsumerName(),
			Count:    cs.cfg.BatchRecs,
			Block:    100 * time.Millisecond,
			Start:    ">",
		})
		if err != nil {
			return cs.Stats(), classifyRedis("XREADGROUP", err)
		}
		if len(entries) == 0 {
			pending, perr := cs.c.Pending(ctx)
			if perr == nil && pending.Count == 0 {
				return cs.Stats(), nil
			}
			continue
		}
		if err := cs.handleBatch(ctx, entries); err != nil {
			return cs.Stats(), err
		}
	}
}

// completeEntry notifies the door (when one is attached).
func (cs *Consumer) completeEntry(entryID string, recs []types.Record, err error) {
	if cs.complete != nil {
		cs.complete(entryID, recs, err)
	}
}

// idemKey derives the entry's idempotency key. The envelope's own key wins when
// it is present (it is the satellite's claim and the one the door used); the
// content-derived fallback keeps the derivation total for an envelope that
// carries no key at all.
func (cs *Consumer) idemKey(env StreamEnvelope) string {
	if env.IdempotencyKey != "" {
		return env.IdempotencyKey
	}
	payload, err := canonicalJSON(env.Records)
	if err != nil {
		return ContentIdemKey(types.KEvent, []byte(env.EnqueuedTS))
	}
	kind := types.KEvent
	if len(env.Records) > 0 {
		kind = env.Records[0].Kind
	}
	return ContentIdemKey(kind, payload)
}

// draftFromRecord converts a wire record into a ledger draft. The ledger's
// Append owns rec_id/seq/ts (SPEC-01 §3.1), so the wire record's local id is
// preserved in `payload.local_rec_id` — which is exactly what SPEC-13 §3.2 rule
// 5 pins ("a record that arrives with a local id gets its canonical `rec_id` at
// ledger-append time, and the local id stays in `payload.local_rec_id`").
func draftFromRecord(rec types.Record) types.RecordDraft {
	payload := rec.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	if rec.RecID != "" {
		if _, exists := payload["local_rec_id"]; !exists {
			payload["local_rec_id"] = rec.RecID
		}
	}
	d := types.RecordDraft{
		Kind:       rec.Kind,
		Sig:        rec.Sig,
		Inc:        rec.Inc,
		Origin:     rec.Origin,
		Actor:      rec.Actor,
		Redactions: rec.Redactions,
		Payload:    payload,
	}
	if d.Actor.Kind == "" || d.Actor.ID == "" {
		// The ledger refuses a draft with no actor; the hub is the writer here
		// and says so instead of fabricating a human.
		d.Actor = types.Actor{Kind: types.ActorDaemon, ID: "hub"}
	}
	if d.Origin.HostID == "" {
		d.Origin.HostID = "unknown-host"
	}
	if d.Origin.Source == "" {
		d.Origin.Source = "hub"
	}
	return d
}

func classifyRedis(op string, err error) *Error {
	if err == nil {
		return nil
	}
	if IsAuthError(err) {
		return errWrap(types.CodeHub002, ReasonRedisAuth, op+" refused by redis credentials", err)
	}
	return errWrap(types.CodeHub004, ReasonXAdd, op+" failed", err)
}

func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > 2*time.Second {
		d = 2 * time.Second
	}
	// Jitter keeps several consumers on one host from retrying in lockstep.
	return d/2 + time.Duration(rand.Int63n(int64(d/2)+1))
}

// sleepCtx sleeps or reports false when ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// ErrConsumerStopped is returned by Drain when ctx ended mid-drain.
var ErrConsumerStopped = errors.New("hub: consumer stopped")
