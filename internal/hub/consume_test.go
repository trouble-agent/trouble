package hub

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// consume_test.go is the heart of SPEC-13 §7's consume row: the batch bound, the
// append-before-ack ordering with its re-delivery consequence, reclaim after
// claim_min_idle, order preserved inside a batch, and the dead-letter path.

func consumerFixture(t *testing.T, mutate func(*RedisConfig)) (*fakeStreams, *fakeLedger, *Consumer, *Client, *DedupGate) {
	t.Helper()
	f := newFakeStreams()
	f.info = ServerInfo{AOFEnabled: true, Policy: "noeviction", OptionsChecked: true}
	f.block = 0
	events := &eventLog{}
	ledger := newFakeLedger(events)
	// The fake Redis and the fake ledger share ONE ordered log: that is what
	// makes "the XACK came after the append" a measurable fact rather than a
	// claim about two independent traces.
	f.log = events
	c := openFake(t, f, mutate)
	gate := NewDedupGate(c.Config(), c)
	cs := NewConsumer(c, ledger, gate, c.Config()).WithCompletion(nil)
	return f, ledger, cs, c, gate
}

// TestConsumeAcksOnlyAfterTheAppend: the ordering proof. The shared event log has
// the ledger's own append event and the fake Redis's XACK event; the append must
// come first, and there must be exactly one of each.
func TestConsumeAcksOnlyAfterTheAppend(t *testing.T) {
	f, ledger, cs, c, _ := consumerFixture(t, nil)
	ctx := context.Background()
	if err := c.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	draft := draftFor(types.KEvent, "sentinel:sha256v1:9f2c1d3e4b5a6c7d", "sentinel:payment-worker", nil)
	entryFor(t, f, draft, RouteA, "7f3a91c2d4e5b607")

	// One read+handle pass, exactly as Run does it.
	entries, err := c.s.XReadGroup(ctx, ReadRequest{
		Stream: c.Config().Stream, Group: c.Config().Group, Consumer: c.ConsumerName(),
		Count: 10, Block: 0, Start: ">",
	})
	if err != nil {
		t.Fatalf("XReadGroup: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("delivered %d entries want 1", len(entries))
	}
	if err := cs.handleBatch(ctx, entries); err != nil {
		t.Fatalf("handleBatch: %v", err)
	}
	events := ledger.events.all()
	appendIdx, ackIdx := -1, -1
	for i, e := range events {
		if strings.HasPrefix(e, "append:") {
			appendIdx = i
		}
		if strings.HasPrefix(e, "xack:") {
			ackIdx = i
		}
	}
	if appendIdx < 0 {
		t.Fatalf("no append event: %v", events)
	}
	if ackIdx < 0 {
		t.Fatalf("no xack event: %v", events)
	}
	if appendIdx > ackIdx {
		t.Fatalf("XACK happened BEFORE the append (events=%v): the entry was acked without being durable", events)
	}
	if ledger.Count() != 1 {
		t.Fatalf("ledger records = %d want 1", ledger.Count())
	}
	if n := f.pelSize(c.Config().Stream, c.Config().Group); n != 0 {
		t.Fatalf("PEL holds %d entries after the ack, want 0", n)
	}
	stats := cs.Stats()
	if stats.Acked != 1 || stats.Appended != 1 {
		t.Fatalf("stats = %+v want acked=1 appended=1", stats)
	}
}

// TestConsumeAppendFailureDoesNotAck is the other half of the ordering rule: a
// failed append leaves the entry PENDING, so it is re-delivered rather than lost.
func TestConsumeAppendFailureDoesNotAck(t *testing.T) {
	f, ledger, cs, c, _ := consumerFixture(t, nil)
	ctx := context.Background()
	if err := c.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	entryFor(t, f, draftFor(types.KEvent, "sig", "sentinel", nil), RouteA, "h1")
	ledger.fail = errors.New("TROUBLE-LEDGER-001 (write): disk on fire")
	entries, err := c.s.XReadGroup(ctx, ReadRequest{
		Stream: c.Config().Stream, Group: c.Config().Group, Consumer: c.ConsumerName(), Count: 10, Start: ">",
	})
	if err != nil {
		t.Fatalf("XReadGroup: %v", err)
	}
	if err := cs.handleBatch(ctx, entries); err == nil {
		t.Fatalf("handleBatch must report the append failure")
	}
	if n := f.pelSize(c.Config().Stream, c.Config().Group); n != 1 {
		t.Fatalf("PEL holds %d entries want 1 (an un-acked entry is the retry)", n)
	}
	if cs.Stats().Acked != 0 {
		t.Fatalf("nothing may be acked when the append failed")
	}
}

// TestConsumeRedeliveryLandsZeroNewRecords is SPEC-13 §3.3's exactly-once seam:
// the same entry handled twice (crash between Append and XACK) appends once.
func TestConsumeRedeliveryLandsZeroNewRecords(t *testing.T) {
	f, ledger, cs, c, gate := consumerFixture(t, nil)
	ctx := context.Background()
	if err := c.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	entryFor(t, f, draftFor(types.KEvent, "sentinel:sha256v1:9f2c1d3e4b5a6c7d", "sentinel", nil), RouteA, "h1")
	ent := f.entries(c.Config().Stream)[0]
	// The door claims first (it accepted the event): the claim is phase
	// `enqueued` when the consumer picks the entry up.
	key := claimForEntry(t, ent, c.Config())
	if fresh, err := gate.Claim(ctx, key); err != nil || !fresh {
		t.Fatalf("door claim: fresh=%v err=%v", fresh, err)
	}
	// …the consumer appends and marks, then dies before the XACK.
	if _, err := cs.handleEntry(ctx, StreamEntry{ID: ent.id, Fields: ent.fields}); err != nil {
		t.Fatalf("first handleEntry: %v", err)
	}
	if ledger.Count() != 1 {
		t.Fatalf("first attempt appended %d records want 1", ledger.Count())
	}
	if state, _ := gate.State(ctx, key); state != DedupAppended {
		t.Fatalf("the claim is not in the appended phase after the append: %v", state)
	}
	// Re-delivery: the same bytes, the same idempotency key.
	if _, err := cs.handleEntry(ctx, StreamEntry{ID: ent.id, Fields: cloneFields(ent.fields)}); err != nil {
		t.Fatalf("redelivery handleEntry: %v", err)
	}
	if ledger.Count() != 1 {
		t.Fatalf("re-delivery appended again: %d records (want exactly 1, SPEC-13 §3.3)", ledger.Count())
	}
	if cs.Stats().DedupSkips != 1 {
		t.Fatalf("dedup skips = %d want 1", cs.Stats().DedupSkips)
	}
}

// TestConsumePreservesOrderInsideOneEnvelope: the queue never re-orders a batch's
// records (SPEC-13 §3.3: "per-host seq order survives the queue").
func TestConsumePreservesOrderInsideOneEnvelope(t *testing.T) {
	f, ledger, cs, c, gate := consumerFixture(t, nil)
	ctx := context.Background()
	if err := c.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	drafts := []types.RecordDraft{
		draftFor(types.KEvent, "sig-a", "sentinel", map[string]any{"n": 1}),
		draftFor(types.KEvent, "sig-b", "sentinel", map[string]any{"n": 2}),
		draftFor(types.KEvent, "sig-c", "sentinel", map[string]any{"n": 3}),
	}
	env := StreamEnvelope{
		ForwardEnvelope: types.ForwardEnvelope{
			ProtocolVersion: 1,
			IdempotencyKey:  "batch-1",
			HostID:          "h1",
			Origin:          types.Origin{HostID: "h1", Source: "sentinel"},
		},
		Route: RouteA,
	}
	for _, d := range drafts {
		rec := localRecord(d, "h1", "")
		rec.Kind = d.Kind
		rec.Sig = d.Sig
		rec.Payload = d.Payload
		env.Records = append(env.Records, rec)
	}
	fields, err := env.EncodeFields()
	if err != nil {
		t.Fatalf("EncodeFields: %v", err)
	}
	f.mu.Lock()
	f.seq++
	id := "1758012841221-999"
	f.streams[c.Config().Stream] = append(f.streams[c.Config().Stream], &fakeEntry{id: id, fields: map[string]string{"env": fields["env"].(string)}})
	f.mu.Unlock()
	if _, err := gate.Claim(ctx, "batch-1"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	entries, err := c.s.XReadGroup(ctx, ReadRequest{
		Stream: c.Config().Stream, Group: c.Config().Group, Consumer: c.ConsumerName(), Count: 10, Start: ">",
	})
	if err != nil {
		t.Fatalf("XReadGroup: %v", err)
	}
	if err := cs.handleBatch(ctx, entries); err != nil {
		t.Fatalf("handleBatch: %v", err)
	}
	var sigs []string
	for _, r := range ledger.Records() {
		sigs = append(sigs, r.Sig)
	}
	if strings.Join(sigs, ",") != "sig-a,sig-b,sig-c" {
		t.Fatalf("order = %v want [sig-a sig-b sig-c]", sigs)
	}
	// The local id survives in the payload (SPEC-13 §3.2 rule 5).
	for _, r := range ledger.Records() {
		if _, ok := r.Payload["local_rec_id"]; !ok {
			t.Fatalf("local_rec_id missing from the appended record: %+v", r.Payload)
		}
	}
}

// TestConsumeDeadLettersUndecodableEntries: 007 — a bad entry is moved to
// `<stream>:dead` with a reason, a `gap` record names it, and the original is
// acked only because its copy exists.
func TestConsumeDeadLettersUndecodableEntries(t *testing.T) {
	f, ledger, cs, c, _ := consumerFixture(t, nil)
	ctx := context.Background()
	if err := c.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	f.addRawEntry(c.Config().Stream, "1758012841221-1", map[string]string{StreamField: "{not json"})
	f.addRawEntry(c.Config().Stream, "1758012841221-2", map[string]string{StreamField: `{"protocol_version":9,"idempotency_key":"k","records":[]}`})
	entries, err := c.s.XReadGroup(ctx, ReadRequest{
		Stream: c.Config().Stream, Group: c.Config().Group, Consumer: c.ConsumerName(), Count: 10, Start: ">",
	})
	if err != nil {
		t.Fatalf("XReadGroup: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("delivered %d entries want 2", len(entries))
	}
	if err := cs.handleBatch(ctx, entries); err != nil {
		t.Fatalf("handleBatch: %v", err)
	}
	dead := f.entries(DeadLetterStream(c.Config().Stream))
	if len(dead) != 2 {
		t.Fatalf("dead stream holds %d entries want 2", len(dead))
	}
	for _, e := range dead {
		if e.fields["error_code"] != string(types.CodeHub007) {
			t.Fatalf("dead-letter entry does not carry TROUBLE-HUB-007: %v", e.fields)
		}
		if e.fields["reason"] == "" || e.fields["entry_id"] == "" {
			t.Fatalf("dead-letter entry does not name its reason/entry: %v", e.fields)
		}
	}
	if cs.Stats().DecodeErrors != 2 {
		t.Fatalf("decode errors = %d want 2", cs.Stats().DecodeErrors)
	}
	// One `gap` record per dead-lettered entry, and no `event` record at all.
	var gaps int
	for _, r := range ledger.Records() {
		if r.Kind == types.KGap {
			gaps++
		}
		if r.Kind == types.KEvent {
			t.Fatalf("an undecodable entry produced an event record")
		}
	}
	if gaps != 2 {
		t.Fatalf("gap records = %d want 2", gaps)
	}
	if n := f.pelSize(c.Config().Stream, c.Config().Group); n != 0 {
		t.Fatalf("PEL holds %d entries want 0 (a dead-lettered entry is acked)", n)
	}
}

// TestDecodeEntryEnforcesTheIngestionCaps: 007 on an over-cap batch (§3.2 rule 1)
// and on a missing field.
func TestDecodeEntryEnforcesTheIngestionCaps(t *testing.T) {
	cfg := testRedisCfg()
	cfg.BatchRecs = 2
	cfg.BatchBytes = 200
	cases := []struct {
		name  string
		field map[string]string
		want  string
	}{
		{"missing field", map[string]string{"other": "x"}, "carries no"},
		{"bad json", map[string]string{StreamField: "{"}, "not a ForwardEnvelope"},
		{"version", map[string]string{StreamField: `{"protocol_version":2,"records":[]}`}, "protocol_version"},
		{"too many records", map[string]string{StreamField: `{"protocol_version":1,"records":[{"kind":"event"},{"kind":"event"},{"kind":"event"}]}`}, "cap is 2"},
		{"too many bytes", map[string]string{StreamField: `{"protocol_version":1,"records":[{"kind":"event","payload":{"pad":"` + strings.Repeat("x", 300) + `"}}]}`}, "cap is 200"},
		{"bad route", map[string]string{StreamField: `{"protocol_version":1,"route":"C","records":[]}`}, "route"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeEntry(tc.field, cfg)
			if CodeOf(err) != types.CodeHub007 {
				t.Fatalf("code = %q want TROUBLE-HUB-007 (%v)", CodeOf(err), err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name %q", err.Error(), tc.want)
			}
		})
	}
}

// TestConsumeReclaimsStrandedEntries: XAUTOCLAIM takes back entries whose
// consumer died, they are re-processed through the same path, and after
// DefaultStrandedRounds rounds with no progress the 008 hook fires.
func TestConsumeReclaimsStrandedEntries(t *testing.T) {
	f, ledger, cs, c, gate := consumerFixture(t, func(cfg *RedisConfig) { cfg.ClaimIdle = time.Millisecond })
	ctx := context.Background()
	if err := c.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	draft := draftFor(types.KEvent, "sentinel:sha256v1:9f2c1d3e4b5a6c7d", "sentinel", nil)
	entryFor(t, f, draft, RouteA, "h1")
	if _, err := gate.Claim(ctx, idemKeyOf(t, draft, "h1")); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	// A consumer that reads and dies before acking.
	entries, err := c.s.XReadGroup(ctx, ReadRequest{
		Stream: c.Config().Stream, Group: c.Config().Group, Consumer: "dead-consumer", Count: 10, Start: ">",
	})
	if err != nil {
		t.Fatalf("XReadGroup: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("delivered %d entries want 1", len(entries))
	}
	time.Sleep(5 * time.Millisecond)
	if err := cs.reclaim(ctx); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if ledger.Count() != 1 {
		t.Fatalf("the reclaimed entry was not appended (records=%d)", ledger.Count())
	}
	if cs.Stats().Reclaims != 1 {
		t.Fatalf("reclaims = %d want 1", cs.Stats().Reclaims)
	}
	if n := f.pelSize(c.Config().Stream, c.Config().Group); n != 0 {
		t.Fatalf("PEL holds %d entries after the reclaim+ack, want 0", n)
	}

	// Stranded work: the append keeps failing, so the reclaim rounds make no
	// progress and the 008 hook fires on the third round.
	var rounds, seen int
	cs.WithStrandedHook(func(r, n int) { rounds = r; seen = n })
	ledger.fail = errors.New("TROUBLE-LEDGER-001 (write)")
	entryFor(t, f, draftFor(types.KEvent, "sig-stranded", "sentinel", nil), RouteA, "h1")
	if _, err := c.s.XReadGroup(ctx, ReadRequest{
		Stream: c.Config().Stream, Group: c.Config().Group, Consumer: "dead-consumer", Count: 10, Start: ">",
	}); err != nil {
		t.Fatalf("XReadGroup: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	for i := 0; i < DefaultStrandedRounds; i++ {
		// Each round needs its own min-idle window: XAUTOCLAIM only takes entries
		// that have been idle for at least claim_min_idle (SPEC-13 §3.3).
		time.Sleep(5 * time.Millisecond)
		_ = cs.reclaim(ctx)
	}
	if rounds != DefaultStrandedRounds || seen != 1 {
		t.Fatalf("stranded hook fired with rounds=%d entries=%d want %d/1", rounds, seen, DefaultStrandedRounds)
	}
	if cs.Stats().Stranded != 1 {
		t.Fatalf("stranded counter = %d want 1 (one record per reclaim round that made no progress)", cs.Stats().Stranded)
	}
}

// TestConsumerRunDrainsAndStops: the loop answers a cancelled context cleanly and
// keeps state on a quiet queue.
func TestConsumerRunDrainsAndStops(t *testing.T) {
	f, ledger, cs, c, _ := consumerFixture(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	if err := c.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	entryFor(t, f, draftFor(types.KEvent, "sentinel:sha256v1:9f2c1d3e4b5a6c7d", "sentinel", nil), RouteA, "h1")
	done := make(chan error, 1)
	go func() { done <- cs.Run(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for ledger.Count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if ledger.Count() != 1 {
		t.Fatalf("the consumer loop did not append the entry (records=%d)", ledger.Count())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v on a cancelled context, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not stop after its context was cancelled")
	}
}

// TestDoorReturnsTheLedgerRecordAndDedupsReplays pins the ingestion seam: the
// caller gets the record the LEDGER wrote (real seq, canonical rec_id), and a
// second offer of the same draft is answered without a second record.
func TestDoorReturnsTheLedgerRecordAndDedupsReplays(t *testing.T) {
	_, ledger, cs, c, gate := consumerFixture(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := c.EnsureGroup(ctx); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	door := NewDoor(c, gate, DoorConfig{Route: RouteA, HostID: "7f3a91c2d4e5b607", Wait: 2 * time.Second})
	cs.WithCompletion(door.Complete)
	go func() { _ = cs.Run(ctx) }()

	draft := draftFor(types.KEvent, "sentinel:sha256v1:9f2c1d3e4b5a6c7d", "sentinel:payment-worker", nil)
	rec, err := door.Offer(ctx, draft)
	if err != nil {
		t.Fatalf("Offer: %v", err)
	}
	if rec.Seq == 0 || rec.RecID == "" {
		t.Fatalf("the door returned a record with no identity: %+v", rec)
	}
	if rec.RecID == "" || strings.HasPrefix(rec.RecID, "ev_") == false {
		t.Fatalf("expected a canonical ev_ rec_id, got %q", rec.RecID)
	}
	if ledger.Count() != 1 {
		t.Fatalf("ledger records = %d want 1", ledger.Count())
	}
	if _, ok := ledger.Records()[0].Payload["local_rec_id"]; !ok {
		t.Fatalf("local_rec_id was not preserved on the appended record")
	}

	// The replay: same draft, same idempotency key → one more ledger record at
	// most, and the door reports the duplicate.
	rec2, err := door.Offer(ctx, draft)
	if err != nil {
		t.Fatalf("second Offer: %v", err)
	}
	if door.Duplicates.Load() != 1 {
		t.Fatalf("duplicates = %d want 1", door.Duplicates.Load())
	}
	if ledger.Count() != 1 {
		t.Fatalf("a replay appended a second record (%d)", ledger.Count())
	}
	if rec2.RecID == "" {
		t.Fatalf("the duplicate answer carries no identity")
	}
}

// TestDoorReleasesTheClaimWhenTheQueueWriteFails is SPEC-13 §6.1: a lost XADD
// costs nothing, because the retry re-claims.
func TestDoorReleasesTheClaimWhenTheQueueWriteFails(t *testing.T) {
	f, _, _, c, gate := consumerFixture(t, nil)
	ctx := context.Background()
	door := NewDoor(c, gate, DoorConfig{Route: RouteA, HostID: "h1", Wait: time.Second})
	draft := draftFor(types.KEvent, "sentinel:sha256v1:9f2c1d3e4b5a6c7d", "sentinel", nil)
	key := idemKeyOf(t, draft, "h1")

	f.failXAdd = errors.New("READONLY replica")
	if _, err := door.Offer(ctx, draft); CodeOf(err) != types.CodeHub004 {
		t.Fatalf("offer over a failing XADD: code=%q err=%v", CodeOf(err), err)
	}
	if f.has(key) {
		t.Fatalf("the claim survived a failed XADD: the sender's retry would be answered as a duplicate")
	}
	// The retry is a NEW acceptance, not an idempotent duplicate: it reaches the
	// stream and only the door's wait (no consumer is running here) expires.
	f.failXAdd = nil
	if _, err := door.Offer(ctx, draft); CodeOf(err) != types.CodeHub004 {
		t.Fatalf("retry offer: code=%q err=%v", CodeOf(err), err)
	}
	if door.Duplicates.Load() != 0 {
		t.Fatalf("the retry was answered as a duplicate: duplicates=%d", door.Duplicates.Load())
	}
	if door.Offers.Load() != 1 {
		t.Fatalf("the retry did not reach the queue: offers=%d", door.Offers.Load())
	}
	if got := len(f.entries(c.Config().Stream)); got != 1 {
		t.Fatalf("stream holds %d entries want 1 (the retried one)", got)
	}
}

func (f *fakeStreams) addRawEntry(stream, id string, fields map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.streams[stream] = append(f.streams[stream], &fakeEntry{id: id, fields: fields})
}

func claimForEntry(t *testing.T, ent *fakeEntry, cfg RedisConfig) string {
	t.Helper()
	var env StreamEnvelope
	if err := json.Unmarshal([]byte(ent.fields[StreamField]), &env); err != nil {
		t.Fatalf("decode entry: %v", err)
	}
	return env.IdempotencyKey
}
