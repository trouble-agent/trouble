package hub

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// dedup_rewarm_test.go pins SPEC-13 §2.1.1 rule 5a: the two recovery
// obligations of rule 5's second half, which had no behavioural consumer until
// TRBL-033.
//
//   - `server.redis.failover_grace` is the window after a (re)wire in which the
//     gate TRUSTS the keys it restored from the ledger's idempotency index. The
//     contract is a BOUNDARY, so it is asserted on both sides of it: inside the
//     window the answer comes from the index (no Redis round trip), after it the
//     claim goes back through Redis and the ledger's own idempotency check is the
//     arbiter of §3.4's priced duplicate.
//   - the (re)wire re-warms the gate from the ledger's idempotency index instead
//     of installing a fresh one, so a duplicate that arrives right after a
//     recovery is still suppressed — bounded by `hub.dedup_lru`, the capacity of
//     the structure the restored keys land in.

// stepClock is a clock a test drives by hand. The failover window is a duration
// on the runtime's own clock (the gate reads the runtime's `Now`), so a test can
// observe both sides of the boundary without sleeping through it — and the
// assertions cannot go stale on a loaded host.
type stepClock struct {
	mu sync.Mutex
	t  time.Time
}

func newStepClock() *stepClock { return &stepClock{t: time.Now()} }

func (c *stepClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// advance moves the clock past a window; nothing else in the runtime depends on
// this clock's wall-clock agreement (the supervisors keep their own timers).
func (c *stepClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// TestFailoverGraceTrustsRestoredKeysThenReclaimsThroughRedis is
// obligation (a): the config key has a reader, and the reader has a BOUNDARY.
func TestFailoverGraceTrustsRestoredKeysThenReclaimsThroughRedis(t *testing.T) {
	f := newFakeStreams()
	f.info = hubInfoOK()
	c := openFake(t, f, nil)
	gate := NewDedupGate(c.Config(), c)
	clock := newStepClock()
	gate.now = clock.now
	ctx := context.Background()

	draft := draftFor(types.KEvent, "sentinel:sha256v1:9f2c1d3e4b5a6c7d", "sentinel",
		map[string]any{"item_type": "event", "n": "1"})
	key := idemKeyOf(t, draft, "7f3a91c2d4e5b607")

	// The index the (re)wire seeds the gate with: the ledger already holds this
	// key. Restore is the seam (SPEC-13 §3.1 dedup.state's boot-time restore).
	gate.Restore([]string{key})

	// PREMISE: Redis does NOT hold the claim. A failover/flush that lost the
	// dataset is the only case in which the index is load-bearing — with the
	// claim present, `SET NX` would answer the duplicate anyway and this test
	// would prove nothing.
	if f.has(key) {
		t.Fatalf("premise: the claim is present in Redis, so the answers below would come from Redis")
	}

	// INSIDE the window: the gate trusts the restored key. No Redis round trip,
	// and the answer is §3.4's idempotent duplicate.
	fresh, err := gate.Claim(ctx, key)
	if err != nil {
		t.Fatalf("Claim inside the window: %v", err)
	}
	if fresh {
		t.Fatalf("a key the ledger already holds was claimed FRESH inside server.redis.failover_grace: " +
			"the window trusts no restored key (TRBL-033: the key has no behavioural consumer)")
	}
	if f.has(key) {
		t.Fatalf("the trusted answer wrote a claim into Redis: the window must be answered from the index")
	}
	state, err := gate.State(ctx, key)
	if err != nil || state != DedupAppended {
		t.Fatalf("state inside the window = %v err=%v want appended (the ledger is the authority the index was read from)", state, err)
	}

	// AFTER the window: the trust expires, so the claim is re-claimed through
	// Redis and the ledger's own idempotency check becomes the arbiter — the
	// priced duplicate of §3.4, counted as dedup_conflicts by the consumer.
	clock.advance(c.Config().FailoverGrace + time.Second)
	fresh, err = gate.Claim(ctx, key)
	if err != nil {
		t.Fatalf("Claim after the window: %v", err)
	}
	if !fresh {
		t.Fatalf("after server.redis.failover_grace the key must be re-claimed through Redis, not trusted")
	}
	if !f.has(key) {
		t.Fatalf("the re-claim after the window never reached Redis")
	}
	state, err = gate.State(ctx, key)
	if err != nil || state != DedupEnqueued {
		t.Fatalf("state after the window = %v err=%v want enqueued (the door's claim, read from Redis)", state, err)
	}
}

// TestIdemKeyForRecordReadsBackTheClaimedKey is the derivation half of obligation
// (b): an index is only an index if the keys it holds are the keys the tally was
// taken on. The record is produced by the REAL consumer path (door → stream entry
// → decode → ledger append), so the read-back is measured against what this
// daemon actually writes.
func TestIdemKeyForRecordReadsBackTheClaimedKey(t *testing.T) {
	f, ledger, cs, _, _ := consumerFixture(t, nil)
	ctx := context.Background()
	draft := draftFor(types.KEvent, "sentinel:sha256v1:9f2c1d3e4b5a6c7d", "sentinel",
		map[string]any{"item_type": "event", "n": "1"})
	want := idemKeyOf(t, draft, "7f3a91c2d4e5b607")
	entryFor(t, f, draft, RouteA, "7f3a91c2d4e5b607")
	ent := f.entries("trouble:ingest")[0]
	if _, err := cs.handleEntry(ctx, StreamEntry{ID: ent.id, Fields: ent.fields}); err != nil {
		t.Fatalf("handleEntry: %v", err)
	}
	recs := ledger.Records()
	if len(recs) != 1 {
		t.Fatalf("ledger records = %d want 1", len(recs))
	}
	rec := recs[0]
	// PREMISE, asserted rather than assumed: the claim travels with the record.
	if got, _ := rec.Payload[IdemDigestField].(string); got == "" {
		t.Fatalf("the record carries no %s: the ledger cannot answer what was claimed", IdemDigestField)
	}
	got, ok := IdemKeyForRecord(rec, "7f3a91c2d4e5b607")
	if !ok {
		t.Fatalf("IdemKeyForRecord refused a record the hub itself wrote")
	}
	if got != want {
		t.Fatalf("read-back key = %q want %q (the key the door claimed)", got, want)
	}
	// A sig-less record keeps the content-only key, exactly as the write paths do
	// (a record with no incident scope to prefix).
	bare := types.Record{
		Seq: 1, Kind: types.KLifecycle,
		Payload: map[string]any{IdemDigestField: "content:sha256v1:0123456789abcdef"},
	}
	if k, ok := IdemKeyForRecord(bare, "h1"); !ok || k != "content:sha256v1:0123456789abcdef" {
		t.Fatalf("sig-less key = %q ok=%v", k, ok)
	}
	// A record that carries no claim is ABSENT from the index rather than guessed
	// at: the window is never larger than the evidence for it.
	if k, ok := IdemKeyForRecord(types.Record{Seq: 2, Kind: types.KEvent, Sig: "sentinel:sha256v1:9f2c1d3e4b5a6c7d"}, "h1"); ok {
		t.Fatalf("a record with no recorded claim produced the key %q", k)
	}
}

// TestIdemKeyForRecordIsExactForPayloadsCarryingStructs pins the defect a LIVE
// Redis run found while this ticket was being proved, and it is the reason the
// key is READ rather than recomputed.
//
// The door hashes the payload the PRODUCER holds in memory, and the sentinel puts
// a Go struct in `payload["event"]` (types.SentryEvent). Go marshals a struct in
// FIELD ORDER; a record that has been through JSON holds that object as a map,
// which marshals SORTED. So a digest re-derived from the decoded record is a
// lookalike — the live run measured `content:sha256v1:20fd49f31373b3da` re-derived
// against `content:sha256v1:17b5c28aebcdd8a8` claimed. The test asserts both
// halves: the recorded claim reproduces the door's key EXACTLY, and the
// re-derived digest does not (which is the premise that makes the first half mean
// something).
func TestIdemKeyForRecordIsExactForPayloadsCarryingStructs(t *testing.T) {
	f, ledger, cs, _, _ := consumerFixture(t, nil)
	ctx := context.Background()
	draft := draftFor(types.KEvent, "sentinel:sha256v1:9f2c1d3e4b5a6c7d", "sentinel", map[string]any{
		"item_type": "event",
		"native_id": "472ecdc09f48f912d57401e7ba0a08f2",
		// The sentinel's real shape: a struct, not a map.
		"event": types.SentryEvent{
			ID:        "472ecdc09f48f912d57401e7ba0a08f2",
			Level:     "error",
			Message:   "TRBL-033 struct payload",
			ItemTypes: []string{"event"},
		},
	})
	want := idemKeyOf(t, draft, "7f3a91c2d4e5b607")
	entryFor(t, f, draft, RouteA, "7f3a91c2d4e5b607")
	ent := f.entries("trouble:ingest")[0]
	if _, err := cs.handleEntry(ctx, StreamEntry{ID: ent.id, Fields: ent.fields}); err != nil {
		t.Fatalf("handleEntry: %v", err)
	}
	rec := ledger.Records()[0]

	// (1) The premise: a digest re-derived from the RECORD is not the claimed one.
	// `local_rec_id` is stripped too, so this is the best a derivation could do.
	decoded := make(map[string]any, len(rec.Payload))
	for k, v := range rec.Payload {
		if k == IdemDigestField || k == localRecIDField {
			continue
		}
		decoded[k] = v
	}
	body, err := canonicalJSON(decoded)
	if err != nil {
		t.Fatalf("canonicalJSON: %v", err)
	}
	naive := ContentIdemKey(rec.Kind, body)
	recorded, _ := rec.Payload[IdemDigestField].(string)
	if naive == recorded {
		t.Fatalf("premise: the decoded payload re-derives %q, the very digest the door claimed — "+
			"this payload is reconstructible, so the test proves nothing about the struct case", naive)
	}

	// (2) The claim the door recorded reproduces its key exactly.
	got, ok := IdemKeyForRecord(rec, "7f3a91c2d4e5b607")
	if !ok {
		t.Fatalf("IdemKeyForRecord refused a record the hub itself wrote")
	}
	if got != want {
		t.Fatalf("read-back key = %q want %q (the key the door claimed)", got, want)
	}
	if !strings.HasSuffix(got, recorded) {
		t.Fatalf("the read-back key does not carry the recorded claim: %q vs %q", got, recorded)
	}
}

// recordingIndex is a LedgerIndex over in-memory records that remembers the walk
// it was asked for: the BOUND of the re-warm is a property of that walk, so it is
// measured, not asserted in a comment.
type recordingIndex struct {
	recs  []types.Record
	last  uint64
	from  uint64
	walks int
}

func (r *recordingIndex) ScanFrom(from uint64, yield func(types.Record) bool) error {
	r.from = from
	r.walks++
	for _, rec := range r.recs {
		if rec.Seq < from {
			continue
		}
		if !yield(rec) {
			return nil
		}
	}
	return nil
}

func (r *recordingIndex) LastSeq() uint64 { return r.last }

// TestLedgerIdemKeysIsBoundedToTheGateCapacity: the re-warm is bounded, and it
// says what the bound IS. The walk starts at `last_seq − hub.dedup_lru + 1` and
// stops at `hub.dedup_lru` keys, so a (re)wire never walks a 30 MB ledger and the
// restored index can never be larger than the LRU it lands in.
func TestLedgerIdemKeysIsBoundedToTheGateCapacity(t *testing.T) {
	recs := make([]types.Record, 0, 10)
	for i := 1; i <= 10; i++ {
		recs = append(recs, types.Record{
			Seq:    uint64(i),
			Kind:   types.KEvent,
			Sig:    "sentinel:sha256v1:9f2c1d3e4b5a6c7d",
			Origin: types.Origin{HostID: "h1"},
			// Every record carries the claim the door took when it wrote it; that
			// is what the walk reads (and what bounds the window).
			Payload: map[string]any{
				"n":             i,
				IdemDigestField: fmt.Sprintf("content:sha256v1:%016x", i),
			},
		})
	}
	idx := &recordingIndex{recs: recs, last: 10}
	keys, err := ledgerIdemKeys(idx, "h1", 4)
	if err != nil {
		t.Fatalf("ledgerIdemKeys: %v", err)
	}
	if len(keys) != 4 {
		t.Fatalf("restored keys = %d want 4 (bounded by the caller's limit)", len(keys))
	}
	if idx.from != 7 {
		t.Fatalf("walk started at seq %d want 7 (last_seq − limit + 1): the tail, not the ledger", idx.from)
	}
	if idx.walks != 1 {
		t.Fatalf("walks = %d want 1", idx.walks)
	}
	// The keys are the TAIL's, in ledger order, and each one is the key of the
	// record it was derived from.
	for i, rec := range recs[6:] {
		want, ok := IdemKeyForRecord(rec, "h1")
		if !ok {
			t.Fatalf("record %d: no key", rec.Seq)
		}
		if keys[i] != want {
			t.Fatalf("key[%d] = %q want %q (record seq %d)", i, keys[i], want, rec.Seq)
		}
	}

	// A ledger shorter than the bound: the whole ledger is the tail, and the walk
	// still starts at the beginning.
	short := &recordingIndex{recs: recs[:3], last: 3}
	keys, err = ledgerIdemKeys(short, "h1", 4)
	if err != nil || len(keys) != 3 || short.from != 1 {
		t.Fatalf("short ledger: keys=%d from=%d err=%v want 3 keys from seq 1", len(keys), short.from, err)
	}

	// No index, or no capacity: nothing is restored, and nothing is walked.
	none := &recordingIndex{recs: recs, last: 10}
	if keys, err := ledgerIdemKeys(nil, "h1", 4); len(keys) != 0 || err != nil {
		t.Fatalf("nil index: keys=%v err=%v want empty", keys, err)
	}
	if keys, err := ledgerIdemKeys(none, "h1", 0); len(keys) != 0 || err != nil {
		t.Fatalf("zero capacity: keys=%v err=%v want empty", keys, err)
	}
	if none.walks != 0 {
		t.Fatalf("a zero-capacity re-warm walked the ledger %d time(s)", none.walks)
	}
}

// TestRewireRewarmsTheGateFromTheLedgerIndex is obligation (b) end to end, and
// it is the whole point of the recovery posture: after a failover that emptied
// Redis — the stream, the group AND the claims — the gate a rewire installs is
// the LEDGER's index, so the duplicate that arrives right after the recovery is
// suppressed instead of re-appended. Then the window closes and the same
// duplicate is the priced case of §3.4: re-claimed, appended, counted.
func TestRewireRewarmsTheGateFromTheLedgerIndex(t *testing.T) {
	f := newFakeStreams()
	f.info = hubInfoOK()
	f.block = 5 * time.Millisecond
	events := &eventLog{}
	ledger := newFakeLedger(events)
	clock := newStepClock()
	cfg := runtimeCfg(t, f, ledger, newFakeLedger(events), func(c *RuntimeConfig) {
		c.LedgerIndex = fakeLedgerIndex{ledger}
		c.Now = clock.now
		// A window this test can step over in one call: the SIZE of the window is
		// the config key's business (dedup_rewarm_test.go's sibling test asserts
		// the boundary against the resolved default).
		c.Redis.FailoverGrace = 250 * time.Millisecond
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer rt.Close()

	draft := draftFor(types.KEvent, "sentinel:sha256v1:9f2c1d3e4b5a6c7d", "sentinel",
		map[string]any{"item_type": "event", "n": "1"})
	if _, err := rt.Ingest(ctx, draft); err != nil {
		t.Fatalf("first ingest on a live queue: %v", err)
	}
	waitFor(t, 3*time.Second, "the first event in the ledger", func() bool { return eventRecords(ledger) == 1 })

	key := idemKeyOf(t, draft, cfg.HostID)
	before := rt.Dedup()
	if n, _ := before.Restored(); n != 0 {
		t.Fatalf("the booted gate already holds %d restored keys: this test needs a gate that has not been re-warmed yet", n)
	}

	// THE FAILOVER: the server lost its dataset (stream, group and claims) while
	// it kept answering, so the state machine says `up` and only the supervisor
	// can see it. Both halves matter — `flush` alone leaves the dedup claims
	// behind, and then Redis would answer the duplicate and this test would prove
	// nothing about the index.
	f.flush("trouble:ingest", "ledger-writers")
	f.dropClaims()
	if f.has(key) {
		t.Fatalf("premise: the claim survived the failover")
	}
	// The rewire the supervisor performs (called directly so the test is
	// deterministic: TRBL-031's own tests drive it through the supervisor).
	if !rt.reconnect(ctx) {
		t.Fatalf("the reconnect was refused")
	}
	after := rt.Dedup()
	if after == before {
		t.Fatalf("the rewire did not replace the gate")
	}
	if f.has(key) {
		t.Fatalf("premise: the re-warmed gate wrote a claim, so the answers below would come from Redis")
	}
	n, windowOpen := after.Restored()
	if n == 0 {
		t.Fatalf("the gate installed by the rewire holds no keys from the ledger's idempotency index: " +
			"restored_keys=0 (TRBL-033: a rewire installs a FRESH gate)")
	}
	if !windowOpen {
		t.Fatalf("the re-warmed gate is not inside its failover_grace window")
	}
	if st, err := after.State(ctx, key); err != nil || st != DedupAppended {
		t.Fatalf("the re-warmed gate does not read the key as appended: state=%v err=%v", st, err)
	}
	// The index is reported where an operator looks for it (§3.1 dedup.state in
	// the health surface).
	if st := rt.Status(ctx); st.Redis.DedupRestored != n {
		t.Fatalf("redis.dedup_restored = %d want %d", st.Redis.DedupRestored, n)
	}

	// THE DUPLICATE, right after the rewire: suppressed by the index. The oracle
	// is the EVENT records (the runtime's own lifecycle records share this
	// ledger, and the rewire wrote one).
	if _, err := rt.Ingest(ctx, draft); err != nil {
		t.Fatalf("the duplicate offer: %v", err)
	}
	if got := eventRecords(ledger); got != 1 {
		t.Fatalf("event records = %d want 1: the duplicate was re-appended although the ledger index held its key", got)
	}
	if len(f.entries("trouble:ingest")) != 0 {
		t.Fatalf("the duplicate was enqueued into the stream instead of being answered as a replay")
	}
	if _, _, conflicts, _ := after.Counters(); conflicts != 0 {
		t.Fatalf("dedup_conflicts = %d want 0 (the gate answered, so the ledger never had to)", conflicts)
	}
	door := rt.Door()
	if door == nil || door.Duplicates.Load() != 1 {
		t.Fatalf("the door did not answer the duplicate as one: %+v", door)
	}

	// AFTER the window: the trust is gone, so the claim is re-claimed through
	// Redis — which lost it, so it is FRESH — and the duplicate reaches the
	// ledger, where the idempotency check is the arbiter. That is exactly what
	// the window is FOR: §3.4 prices a lost claim as ONE duplicate entry, and the
	// window is what keeps a recovery burst from paying that price with every
	// retry that arrives while the tier is still settling.
	clock.advance(cfg.Redis.FailoverGrace + time.Second)
	if st, err := after.State(ctx, key); err != nil || st != DedupAbsent {
		t.Fatalf("state after the window = %v err=%v want absent: the index stops answering when the window closes and Redis lost the claim", st, err)
	}
	if _, err := rt.Ingest(ctx, draft); err != nil {
		t.Fatalf("the offer after the window: %v", err)
	}
	if got := len(f.entries("trouble:ingest")); got != 1 {
		t.Fatalf("stream entries after the window = %d want 1: the key was NOT re-claimed through Redis, so the index is still answering", got)
	}
	if !f.has(key) {
		t.Fatalf("the re-claim after the window never reached Redis")
	}
	waitFor(t, 3*time.Second, "the re-claimed duplicate in the ledger", func() bool { return eventRecords(ledger) == 2 })
	// The counter's own contract: `dedup_conflicts` is an append whose claim was
	// LOST. Here the door re-claimed the key before enqueueing, so the consumer
	// saw a live `enqueued` phase and the duplicate lands as the record §3.4
	// prices, not as a conflict. Pinned, not ignored: a change that made this
	// count would be a change to what the counter means.
	if _, _, conflicts, _ := after.Counters(); conflicts != 0 {
		t.Fatalf("dedup_conflicts = %d want 0 (the door re-claimed the key, so no claim was lost)", conflicts)
	}
}
