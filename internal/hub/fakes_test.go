package hub

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// fakes_test.go carries the two test doubles the queue's semantics are proven
// against: an in-memory Redis stream/consumer-group (with a PEL, delivery
// counts and the gate's SET NX/XX) and a fake ledger.
//
// No test in this package needs a live Redis or a live DuckBrain: the seams in
// production code exist for exactly this, and a test that only asserted
// constants would prove nothing about the ack-after-fsync rule, the re-delivery
// case or the drop gate.

// eventLog is the shared, ordered log both doubles write to. The
// ack-after-fsync test reads it; ordering is the property, so the log is
// appended by the doubles themselves, not reconstructed afterwards.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(e string) {
	l.mu.Lock()
	l.events = append(l.events, e)
	l.mu.Unlock()
}

func (l *eventLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

func (l *eventLog) index(prefix string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, e := range l.events {
		if strings.HasPrefix(e, prefix) {
			return i
		}
	}
	return -1
}

type fakeEntry struct {
	id     string
	fields map[string]string
	// owner is the consumer holding the entry in the PEL ("" when unread).
	owner     string
	delivered int64
	idleSince time.Time
}

type fakeStream struct {
	id  string
	seq int
}

type fakeGroup struct {
	name            string
	lastDeliveredID string
	entriesRead     int64
	pel             map[string]*fakeEntry
	order           []string
}

// fakeStreams is a faithful-enough Redis for the hub's command surface.
type fakeStreams struct {
	mu      sync.Mutex
	streams map[string][]*fakeEntry
	groups  map[string]map[string]*fakeGroup // stream → group → group
	kv      map[string]kvEntry

	info    ServerInfo
	infoErr error
	pingErr error

	failXAdd          error
	failXReadGroup    error
	failXAck          error
	failSetNX         error
	failSetXX         error
	failGet           error
	failXAutoClaim    error
	failXGroupCreate  error
	xaddFailuresLeft  int
	groupCreateCalled int
	groupName         string

	// postXAdd, when set, runs inside XAdd after the entry exists and its id
	// is known but BEFORE the call returns — the exact window in which a
	// consumer completion can beat the door's waiter registration. The door
	// regression test uses it to park the fake mid-XAdd deterministically.
	postXAdd func(entryID string)

	ms  int64
	seq int
	log *eventLog

	// block is how long XReadGroup "blocks" before answering empty.
	block time.Duration
}

type kvEntry struct {
	value     string
	expiresAt time.Time
}

func newFakeStreams() *fakeStreams {
	return &fakeStreams{
		streams: map[string][]*fakeEntry{},
		groups:  map[string]map[string]*fakeGroup{},
		kv:      map[string]kvEntry{},
		log:     &eventLog{},
		ms:      1758012841221,
	}
}

// Ping, ServerInfo and the read block are the fake's AVAILABILITY surface: a
// test may change it while the runtime is running (a Redis that is lost and
// comes back), so they are read under the same lock the writers take. Blocking
// is done OUTSIDE the lock, or a test's write would wait for the block.
func (f *fakeStreams) Ping(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pingErr
}

func (f *fakeStreams) ServerInfo(ctx context.Context) (ServerInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.info, f.infoErr
}

func (f *fakeStreams) XAdd(ctx context.Context, stream string, maxLen int64, values map[string]any) (string, error) {
	f.mu.Lock()
	if f.failXAdd != nil && f.xaddFailuresLeft == 0 {
		err := f.failXAdd
		f.mu.Unlock()
		return "", err
	}
	if f.xaddFailuresLeft > 0 {
		f.xaddFailuresLeft--
		err := f.failXAdd
		f.mu.Unlock()
		return "", err
	}
	f.seq++
	ent := &fakeEntry{id: fmt.Sprintf("%d-%d", f.ms, f.seq), fields: map[string]string{}}
	for k, v := range values {
		ent.fields[k] = fmt.Sprint(v)
	}
	f.streams[stream] = append(f.streams[stream], ent)
	f.log.add("xadd:" + stream + ":" + ent.id)
	entryID := ent.id
	post := f.postXAdd
	f.mu.Unlock()
	// The hook runs OUTSIDE the lock (explicit unlocks above: one deferred
	// Unlock cannot span a hook that may take the lock again): it is the seam
	// a test uses to act in the window between the entry existing and the
	// caller being handed its id — the door's registration-race window.
	if post != nil {
		post(entryID)
	}
	return entryID, nil
}

func (f *fakeStreams) XLen(ctx context.Context, stream string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int64(len(f.streams[stream])), nil
}

func (f *fakeStreams) XGroupCreate(ctx context.Context, stream, group, start string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.groupCreateCalled++
	if f.failXGroupCreate != nil {
		return f.failXGroupCreate
	}
	if _, ok := f.groups[stream]; !ok {
		f.groups[stream] = map[string]*fakeGroup{}
	}
	if _, ok := f.groups[stream][group]; ok {
		return errors.New("BUSYGROUP Consumer Group name already exists")
	}
	g := &fakeGroup{name: group, pel: map[string]*fakeEntry{}}
	f.groupName = group
	if start == "$" {
		if list := f.streams[stream]; len(list) > 0 {
			g.lastDeliveredID = list[len(list)-1].id
		}
	}
	f.groups[stream][group] = g
	return nil
}

func (f *fakeStreams) XReadGroup(ctx context.Context, req ReadRequest) ([]StreamEntry, error) {
	f.mu.Lock()
	block := f.block
	f.mu.Unlock()
	if block > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(block):
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failXReadGroup != nil {
		return nil, f.failXReadGroup
	}
	g := f.groups[req.Stream][req.Group]
	if g == nil {
		return nil, errors.New("NOGROUP No such key or consumer group")
	}
	var out []StreamEntry
	if req.Start == "" || req.Start == ">" {
		for _, ent := range f.streams[req.Stream] {
			if len(out) >= req.Count {
				break
			}
			if g.lastDeliveredID != "" && ent.id <= g.lastDeliveredID {
				continue
			}
			ent.owner = req.Consumer
			ent.delivered++
			ent.idleSince = time.Now()
			g.pel[ent.id] = ent
			g.order = append(g.order, ent.id)
			g.lastDeliveredID = ent.id
			g.entriesRead++
			out = append(out, StreamEntry{ID: ent.id, Fields: cloneFields(ent.fields), DeliveryCount: ent.delivered})
		}
		return out, nil
	}
	for _, id := range g.order {
		if len(out) >= req.Count {
			break
		}
		if id < req.Start {
			continue
		}
		ent, ok := g.pel[id]
		if !ok {
			continue
		}
		ent.delivered++
		out = append(out, StreamEntry{ID: ent.id, Fields: cloneFields(ent.fields), DeliveryCount: ent.delivered})
	}
	return out, nil
}

func (f *fakeStreams) XAck(ctx context.Context, stream, group string, ids ...string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failXAck != nil {
		return 0, f.failXAck
	}
	g := f.groups[stream][group]
	if g == nil {
		return 0, errors.New("NOGROUP")
	}
	var n int64
	for _, id := range ids {
		if _, ok := g.pel[id]; ok {
			delete(g.pel, id)
			n++
			f.log.add("xack:" + stream + ":" + id)
		}
	}
	return n, nil
}

func (f *fakeStreams) XAutoClaim(ctx context.Context, req ClaimRequest) (ClaimResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failXAutoClaim != nil {
		return ClaimResult{}, f.failXAutoClaim
	}
	g := f.groups[req.Stream][req.Group]
	if g == nil {
		return ClaimResult{}, errors.New("NOGROUP")
	}
	res := ClaimResult{Next: "0-0"}
	for _, id := range g.order {
		if len(res.Entries) >= req.Count {
			break
		}
		ent, ok := g.pel[id]
		if !ok {
			continue
		}
		if time.Since(ent.idleSince) < req.MinIdle {
			continue
		}
		ent.owner = req.Consumer
		ent.delivered++
		ent.idleSince = time.Now()
		res.Entries = append(res.Entries, StreamEntry{
			ID: ent.id, Fields: cloneFields(ent.fields), DeliveryCount: ent.delivered,
		})
	}
	if len(res.Entries) > 0 {
		f.log.add("xautoclaim:" + req.Stream + ":" + strconv.Itoa(len(res.Entries)))
	}
	return res, nil
}

func (f *fakeStreams) XPending(ctx context.Context, stream, group string) (Pending, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g := f.groups[stream][group]
	if g == nil {
		return Pending{}, errors.New("NOGROUP")
	}
	p := Pending{Count: int64(len(g.pel))}
	first := true
	for id := range g.pel {
		if first {
			p.MinID, p.MaxID = id, id
			first = false
			continue
		}
		if id < p.MinID {
			p.MinID = id
		}
		if id > p.MaxID {
			p.MaxID = id
		}
	}
	return p, nil
}

func (f *fakeStreams) XInfoGroups(ctx context.Context, stream string) (GroupInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g := f.groups[stream][f.groupName]
	if g == nil {
		return GroupInfo{}, errors.New("NOGROUP")
	}
	n := int64(len(f.streams[stream]))
	return GroupInfo{
		Name:            g.name,
		Consumers:       1,
		Pending:         int64(len(g.pel)),
		LastDeliveredID: g.lastDeliveredID,
		EntriesRead:     g.entriesRead,
		Lag:             n - g.entriesRead,
	}, nil
}

func (f *fakeStreams) SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSetNX != nil {
		return false, f.failSetNX
	}
	if e, ok := f.kv[key]; ok && time.Now().Before(e.expiresAt) {
		return false, nil
	}
	f.kv[key] = kvEntry{value: value, expiresAt: time.Now().Add(ttl)}
	return true, nil
}

func (f *fakeStreams) SetXX(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSetXX != nil {
		return false, f.failSetXX
	}
	if e, ok := f.kv[key]; !ok || !time.Now().Before(e.expiresAt) {
		return false, nil
	}
	f.kv[key] = kvEntry{value: value, expiresAt: time.Now().Add(ttl)}
	return true, nil
}

func (f *fakeStreams) Get(ctx context.Context, key string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failGet != nil {
		return "", false, f.failGet
	}
	e, ok := f.kv[key]
	if !ok || !time.Now().Before(e.expiresAt) {
		return "", false, nil
	}
	return e.value, true, nil
}

// TTL is the §2.2 dedup probe's TTL fact over the fake: the remaining window
// for a live claim, 0 when the key is absent or released.
func (f *fakeStreams) TTL(ctx context.Context, key string) (time.Duration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failGet != nil {
		return 0, f.failGet
	}
	e, ok := f.kv[key]
	if !ok {
		return 0, nil
	}
	rest := time.Until(e.expiresAt)
	if rest <= 0 {
		return 0, nil
	}
	return rest, nil
}

func (f *fakeStreams) Del(ctx context.Context, keys ...string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, k := range keys {
		if _, ok := f.kv[k]; ok {
			delete(f.kv, k)
			n++
		}
	}
	return n, nil
}

func (f *fakeStreams) Close() error { return nil }

// helpers used by the tests ------------------------------------------------

func (f *fakeStreams) entries(stream string) []*fakeEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*fakeEntry(nil), f.streams[stream]...)
}

func (f *fakeStreams) pelSize(stream, group string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	g := f.groups[stream][group]
	if g == nil {
		return 0
	}
	return len(g.pel)
}

func (f *fakeStreams) expireKey(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e, ok := f.kv[key]; ok {
		e.expiresAt = time.Now().Add(-time.Second)
		f.kv[key] = e
	}
}

func (f *fakeStreams) has(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.kv[key]
	return ok && time.Now().Before(e.expiresAt)
}

// dropClaims forgets every dedup claim: what FLUSHALL, a dataset-less failover
// or a replacement container does to the gate's key space, and the reason the
// ledger's idempotency index (SPEC-13 §2.1.1 rule 5a) is load-bearing at all.
// `flush` alone leaves the claims behind, so a test that wants the cold-server
// shape has to say both.
func (f *fakeStreams) dropClaims() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kv = map[string]kvEntry{}
}

func cloneFields(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// fakeLedger is the Appender seam: it mints a canonical rec_id (as
// internal/ledger does) and records the append in the shared order log, which is
// what makes "the XACK came after the append" a measurable fact.
type fakeLedger struct {
	mu     sync.Mutex
	recs   []types.Record
	seq    uint64
	fail   error
	events *eventLog
	delay  time.Duration
}

func newFakeLedger(events *eventLog) *fakeLedger {
	return &fakeLedger{events: events}
}

func (l *fakeLedger) Append(ctx context.Context, d types.RecordDraft) (types.Record, error) {
	if l.delay > 0 {
		select {
		case <-ctx.Done():
			return types.Record{}, ctx.Err()
		case <-time.After(l.delay):
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fail != nil {
		return types.Record{}, l.fail
	}
	l.seq++
	rec := types.Record{
		RecID:         types.NewID(types.PEv),
		Seq:           l.seq,
		TS:            types.NowUTC(),
		Kind:          d.Kind,
		SchemaVersion: 1,
		Sig:           d.Sig,
		Inc:           d.Inc,
		Origin:        d.Origin,
		Actor:         d.Actor,
		Redactions:    d.Redactions,
		Payload:       d.Payload,
	}
	l.recs = append(l.recs, rec)
	if l.events != nil {
		local, _ := d.Payload["local_rec_id"].(string)
		if local == "" {
			local = string(d.Kind)
		}
		l.events.add("append:" + local)
	}
	return rec, nil
}

func (l *fakeLedger) Count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.recs)
}

func (l *fakeLedger) Records() []types.Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]types.Record(nil), l.recs...)
}

func (l *fakeLedger) localIDs() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.recs))
	for _, r := range l.recs {
		id, _ := r.Payload["local_rec_id"].(string)
		out = append(out, id)
	}
	return out
}

// fakeLedgerIndex is the READ half of the fake ledger (SPEC-13 §2.1.1 rule 5a's
// LedgerIndex): a re-warm in a test reads exactly the records that test's own
// ingest appended, which is what the production composition root does with the
// real ledger.
type fakeLedgerIndex struct{ l *fakeLedger }

func (i fakeLedgerIndex) ScanFrom(from uint64, yield func(types.Record) bool) error {
	for _, rec := range i.l.Records() {
		if rec.Seq < from {
			continue
		}
		if !yield(rec) {
			return nil
		}
	}
	return nil
}

func (i fakeLedgerIndex) LastSeq() uint64 {
	recs := i.l.Records()
	if len(recs) == 0 {
		return 0
	}
	return recs[len(recs)-1].Seq
}

// entryFor builds a stream entry for a draft as the door would enqueue it.
func entryFor(t interface{ Fatalf(string, ...any) }, f *fakeStreams, draft types.RecordDraft, route RouteDecision, hostID string) string {
	key, err := IdemKeyForDraft(draft, hostID)
	if err != nil {
		t.Fatalf("idemKey: %v", err)
	}
	// The door records the claim with the record (SPEC-13 §2.1.1 rule 5a), so the
	// fixture does too: an entry built without it would not be the entry the door
	// enqueues.
	rec := localRecord(draft, hostID, key)
	env := StreamEnvelope{
		ForwardEnvelope: types.ForwardEnvelope{
			ProtocolVersion: 1,
			IdempotencyKey:  key,
			HostID:          hostID,
			Origin:          rec.Origin,
			Records:         []types.Record{rec},
		},
		Route:      route,
		EnqueuedTS: types.NowUTC(),
	}
	fields, err := env.EncodeFields()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	f.mu.Lock()
	f.seq++
	id := fmt.Sprintf("%d-%d", f.ms, f.seq)
	f.mu.Unlock()
	ent := &fakeEntry{id: id, fields: map[string]string{}}
	for k, v := range fields {
		ent.fields[k] = fmt.Sprint(v)
	}
	f.mu.Lock()
	f.streams["trouble:ingest"] = append(f.streams["trouble:ingest"], ent)
	f.mu.Unlock()
	return id
}

func draftFor(kind types.RecordKind, sig, source string, payload map[string]any) types.RecordDraft {
	if payload == nil {
		payload = map[string]any{"subject": "x"}
	}
	return types.RecordDraft{
		Kind:    kind,
		Sig:     sig,
		Origin:  types.Origin{HostID: "7f3a91c2d4e5b607", Source: source},
		Actor:   types.Actor{Kind: types.ActorDaemon, ID: "troubled"},
		Payload: payload,
	}
}
