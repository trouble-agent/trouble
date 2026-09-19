package hub

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// redis_live_test.go is the OPT-IN live probe: it runs the hub's queue against a
// REAL Redis (≥ 6.2, streams + consumer groups + XAUTOCLAIM) and is skipped
// unless TROUBLE_REDIS_TEST_URL names one.
//
//	redis-server --port 6399 --appendonly yes --maxmemory-policy noeviction
//	TROUBLE_REDIS_TEST_URL=redis://127.0.0.1:6399/0 go test ./internal/hub/ -run Live -count=1 -v
//
// Everything else in this package is proven through the Streams seam; what only a
// live server can add is the wire truth: the INFO preflight facts, BUSYGROUP from
// a second XGROUP CREATE, the XADD-assigned ids, XPENDING after the ack, and the
// fact that two records of one request really do occupy two entries (the defect a
// live run found — see IdemKeyForDraft).
func liveRedisCfg(t *testing.T) RedisConfig {
	t.Helper()
	url := strings.TrimSpace(os.Getenv("TROUBLE_REDIS_TEST_URL"))
	if url == "" {
		t.Skip("TROUBLE_REDIS_TEST_URL is not set: the live Redis probe is opt-in")
	}
	stream := os.Getenv("TROUBLE_REDIS_TEST_STREAM")
	if stream == "" {
		stream = "trouble:live:" + types.NewID(types.PEv)
	}
	group := os.Getenv("TROUBLE_REDIS_TEST_GROUP")
	if group == "" {
		group = "ledger-writers"
	}
	return RedisConfig{
		URL:                url,
		Stream:             stream,
		Group:              group,
		HostID:             "7f3a91c2d4e5b607",
		MaxLen:             1000000,
		ClaimIdle:          time.Second,
		RequirePersistence: false, // a dev Redis is often appendonly=off; the refusal is proven by TestPreflightRefusesPersistenceLive
		CheckPolicy:        true,
		DoorWait:           5 * time.Second,
	}.WithDefaults()
}

// TestLiveRedisQueueRoundTrip walks the whole light-hub hop against a live
// server: dial + preflight, group creation (twice: BUSYGROUP), one draft through
// the door (XADD → consumer → ledger → XACK), the offsets the stanza reports, and
// a REPLAY of the same record landing zero new ledger records.
func TestLiveRedisQueueRoundTrip(t *testing.T) {
	cfg := liveRedisCfg(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c, err := OpenRedis(ctx, cfg)
	if err != nil {
		t.Fatalf("OpenRedis: %v", err)
	}
	defer c.Close()
	t.Logf("preflight: aof=%v policy=%q evicted=%d options_checked=%v",
		c.ServerInfo().AOFEnabled, c.ServerInfo().Policy, c.ServerInfo().EvictedKeys, c.ServerInfo().OptionsChecked)
	for _, w := range c.PolicyWarnings() {
		t.Logf("preflight warning: %s", w)
	}
	if err := c.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if err := c.EnsureGroup(ctx); err != nil {
		t.Fatalf("second EnsureGroup must treat BUSYGROUP as success: %v", err)
	}

	events := &eventLog{}
	ledger := newFakeLedger(events)
	gate := NewDedupGate(c.Config(), c)
	cs := NewConsumer(c, ledger, gate, c.Config())
	door := NewDoor(c, gate, DoorConfig{Route: RouteA, HostID: cfg.HostID, Wait: 5 * time.Second})
	cs.WithCompletion(door.Complete)
	go func() { _ = cs.Run(ctx) }()

	sig := "sentinel:sha256v1:9f2c1d3e4b5a6c7d"
	event := draftFor(types.KEvent, sig, "sentinel:live", map[string]any{"item_type": "event", "probe": types.NewID(types.PEv)})
	groupRec := draftFor(types.KGroup, sig, "sentinel:live", map[string]any{"op": "create", "group_id": "grp_live", "probe": types.NewID(types.PEv)})

	for _, d := range []types.RecordDraft{event, groupRec} {
		rec, err := door.Offer(ctx, d)
		if err != nil {
			t.Fatalf("offer %s: %v", d.Kind, err)
		}
		if rec.Seq == 0 || rec.RecID == "" {
			t.Fatalf("the door returned a record with no ledger identity: %+v", rec)
		}
	}
	if ledger.Count() != 2 {
		t.Fatalf("ledger records = %d want 2 (two distinct records of one sig must both be queued)", ledger.Count())
	}

	// The offsets the stanza reports come from the live server.
	n, err := c.StreamLen(ctx)
	if err != nil {
		t.Fatalf("XLEN: %v", err)
	}
	if n < 2 {
		t.Fatalf("stream length = %d want at least 2", n)
	}
	pending, err := c.Pending(ctx)
	if err != nil {
		t.Fatalf("XPENDING: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for pending.Count != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		pending, _ = c.Pending(ctx)
	}
	if pending.Count != 0 {
		t.Fatalf("pending = %d want 0: every entry must be acked after its append", pending.Count)
	}
	gi, err := c.GroupInfo(ctx)
	if err != nil {
		t.Fatalf("XINFO GROUPS: %v", err)
	}
	if gi.LastDeliveredID == "" {
		t.Fatalf("the group delivered nothing: %+v", gi)
	}
	acked, _ := c.Watermarks()
	if acked == "" {
		t.Fatalf("the runtime did not record an acked id")
	}

	// The replay: the same draft, the same idempotency key → no new record.
	if _, err := door.Offer(ctx, groupRec); err != nil {
		t.Fatalf("replay offer: %v", err)
	}
	if ledger.Count() != 2 {
		t.Fatalf("a replayed record appended again: %d records", ledger.Count())
	}
	if door.Duplicates.Load() != 1 {
		t.Fatalf("duplicates = %d want 1", door.Duplicates.Load())
	}
	// Failover cost (SPEC-13 §3.4): an entry whose claim is GONE is appended
	// once more, and the gate COUNTS the duplicate instead of hiding it. The
	// claim-less entry is enqueued directly (bypassing the door, which always
	// claims) because that is exactly the state a replication loss leaves behind.
	orphanKey, err := IdemKeyForDraft(
		draftFor(types.KEvent, sig, "sentinel:live", map[string]any{"item_type": "event", "orphan": "1"}), cfg.HostID)
	if err != nil {
		t.Fatalf("orphan key: %v", err)
	}
	gate.Release(ctx, orphanKey) // make sure no claim exists for it
	orphan := localRecord(draftFor(types.KEvent, sig, "sentinel:live", map[string]any{"item_type": "event", "orphan": "1"}), cfg.HostID, "")
	if _, err := c.EnqueueEntry(ctx, types.ForwardEnvelope{
		ProtocolVersion: 1,
		IdempotencyKey:  orphanKey,
		HostID:          cfg.HostID,
		Origin:          orphan.Origin,
		Records:         []types.Record{orphan},
	}, RouteA); err != nil {
		t.Fatalf("enqueue the claim-less entry: %v", err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for ledger.Count() < 3 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if ledger.Count() != 3 {
		t.Fatalf("ledger records after a lost claim = %d want 3", ledger.Count())
	}
	if _, _, conflicts, _ := gate.Counters(); conflicts != 1 {
		t.Fatalf("dedup_conflicts = %d want 1", conflicts)
	}
	// The claim is now in the appended phase, so a second delivery of the same
	// bytes is a skip rather than a fourth record.
	if _, err := c.EnqueueEntry(ctx, types.ForwardEnvelope{
		ProtocolVersion: 1,
		IdempotencyKey:  orphanKey,
		HostID:          cfg.HostID,
		Origin:          orphan.Origin,
		Records:         []types.Record{orphan},
	}, RouteA); err != nil {
		t.Fatalf("re-enqueue the same entry: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if ledger.Count() != 3 {
		t.Fatalf("a re-delivery after the appended claim appended again: %d", ledger.Count())
	}
}

func mustLiveKey(t *testing.T, d types.RecordDraft, host string) string {
	t.Helper()
	key, err := IdemKeyForDraft(d, host)
	if err != nil {
		t.Fatalf("IdemKeyForDraft: %v", err)
	}
	return key
}

// TestLiveRedisPreflightRefusesPersistenceLessServer pins 016 against a live
// server configured with appendonly=no and require_persistence=true.
func TestLiveRedisPreflightRefusesPersistenceLessServer(t *testing.T) {
	cfg := liveRedisCfg(t)
	if os.Getenv("TROUBLE_REDIS_TEST_APPENDONLY_OFF") == "" {
		t.Skip("set TROUBLE_REDIS_TEST_APPENDONLY_OFF=1 against a Redis with appendonly=no to run this")
	}
	cfg.RequirePersistence = true
	if _, err := OpenRedis(context.Background(), cfg); CodeOf(err) != types.CodeHub016 {
		t.Fatalf("code = %q want TROUBLE-HUB-016 (%v)", CodeOf(err), err)
	}
}

// TestLiveRedisRewarmServesDuplicateAfterClaimLoss is the re-warm of SPEC-13
// §2.1.1 rule 5a against a live server: a ledger produced by THIS run's own
// door → stream → consumer → ledger round trip is read back through the SAME
// walk a (re)wire uses (`ledgerIdemKeys`), the gate is restored from it, and a
// duplicate whose claim is then GONE from Redis (the failover shape — what a
// flush or a lost dataset does to one claim, done here with `Del` so the
// scratch server other probes share is not flushed) is answered by the index
// with zero new records and zero queue traffic. The dedicated fake-server twin
// of this test is TestRewireRewarmsTheGateFromTheLedgerIndex; this one adds the
// wire: a real XADD'd entry, a real SET NX claim, a real deletion, a real
// read-back.
func TestLiveRedisRewarmServesDuplicateAfterClaimLoss(t *testing.T) {
	cfg := liveRedisCfg(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c, err := OpenRedis(ctx, cfg)
	if err != nil {
		t.Fatalf("OpenRedis: %v", err)
	}
	defer c.Close()
	if err := c.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}

	events := &eventLog{}
	ledger := newFakeLedger(events)
	gate := NewDedupGate(c.Config(), c)
	cs := NewConsumer(c, ledger, gate, c.Config())
	door := NewDoor(c, gate, DoorConfig{Route: RouteA, HostID: cfg.HostID, Wait: 5 * time.Second})
	cs.WithCompletion(door.Complete)
	go func() { _ = cs.Run(ctx) }()

	sig := "sentinel:sha256v1:9f2c1d3e4b5a6c7d"
	draft := draftFor(types.KEvent, sig, "sentinel:live", map[string]any{"item_type": "event", "probe": types.NewID(types.PEv)})
	if _, err := door.Offer(ctx, draft); err != nil {
		t.Fatalf("offer: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for ledger.Count() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if ledger.Count() != 1 {
		t.Fatalf("ledger records = %d want 1", ledger.Count())
	}

	// The index: the ledger's tail through the same walk a (re)wire runs. The
	// read-back key must be the door's key EXACTLY — on this wire it is the
	// struct-payload lookalike trap, which is why the digest is read from the
	// record rather than re-derived.
	keys, err := ledgerIdemKeys(fakeLedgerIndex{ledger}, cfg.HostID, c.Config().DedupLRU)
	if err != nil {
		t.Fatalf("ledgerIdemKeys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("index keys = %d want 1: %+v", len(keys), keys)
	}
	wantKey := mustLiveKey(t, draft, cfg.HostID)
	if keys[0] != wantKey {
		t.Fatalf("read-back key = %q want %q (the key the door claimed on the live server)", keys[0], wantKey)
	}
	gate.Restore(keys)
	if n, open := gate.Restored(); n != 1 || !open {
		t.Fatalf("restored = %d open=%v want 1 true", n, open)
	}

	// THE FAILOVER SHAPE on the live server: the claim is gone. Asserted as a
	// premise — with the claim present, `SET NX` would answer the duplicate and
	// the index would prove nothing.
	key := keys[0]
	if n, err := c.Streams().Del(ctx, key); err != nil || n != 1 {
		t.Fatalf("premise: deleting the claim returned n=%d err=%v, want 1 nil", n, err)
	}

	// THE DUPLICATE: answered from the ledger index — no new record, no new
	// entry, and no conflict counter (the gate answered, so the ledger never
	// had to arbitrate).
	if _, err := door.Offer(ctx, draft); err != nil {
		t.Fatalf("duplicate offer: %v", err)
	}
	if door.Duplicates.Load() != 1 {
		t.Fatalf("duplicates = %d want 1 (the key the ledger holds must be answered as a replay)", door.Duplicates.Load())
	}
	if n, _ := c.StreamLen(ctx); n != 1 {
		t.Fatalf("stream length = %d want 1: the duplicate was enqueued although the restored index held its key", n)
	}
	if ledger.Count() != 1 {
		t.Fatalf("ledger records = %d want 1: the duplicate was re-appended after the re-warm", ledger.Count())
	}
	if _, _, conflicts, _ := gate.Counters(); conflicts != 0 {
		t.Fatalf("dedup_conflicts = %d want 0 (the gate answered from the index)", conflicts)
	}
	n, _ := gate.Restored()
	t.Logf("restored_keys=%d — the duplicate was served by the ledger's idempotency index with no queue traffic", n)
}
