package flow

// spool_test.go — SPEC-08 §3.9a: the flow's OWN durable dispatch queue and the
// honest refusal when it is not wired.
//
// The defect these tests pin: before §3.9a a failed dispatch wrote
// `dispatch_state="spooled"` for an entry that landed in another subsystem's
// store (SPEC-09's desk spool), whose replay walks the configured DRIVER names
// and whose decode expects the desk's own payload shape — so no loop ever listed
// the entry and, had one, it would have been dropped as corrupt. The desk's own
// shipped posture (`issues.enabled = false`, SPEC-09 §3.4a) refuses the enqueue
// outright. Either way the flow's durable queue did not exist, in any posture,
// and nothing said so.
//
// Suite ACs: AC-32 (a dispatch that cannot be delivered is durably queued on a
// queue this subsystem replays, or honestly refused — never a `spooled` claim a
// loop cannot honour) and AC-33 (the queue's bounds, and every entry a bound
// costs). The AC-1/AC-2 labels below are the ticket-local halves of the same
// rule (TRBL-023), not suite AC-1/AC-2.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// withQueue swaps the fixture's durable queue for a REAL flow-owned store and
// returns it, so the durability assertions run against the shipped store rather
// than a double. The bounds travel with it: the flow owns the drain policy.
func (f *fixture) withQueue(t *testing.T, root string, b SpoolBounds) *Spool {
	t.Helper()
	sp, err := NewSpool(root, b, func() time.Time { return f.clock.Now() })
	if err != nil {
		t.Fatalf("NewSpool(%s): %v", root, err)
	}
	f.flow.bounds = b.WithDefaults()
	d := f.flow.deps
	d.Spool = sp
	f.flow.SetDeps(d)
	if !f.flow.SpoolWired() {
		t.Fatalf("the flow did not adopt its own queue: SpoolWired() = false")
	}
	return sp
}

// withSink swaps the fixture's durable queue for an arbitrary sink.
func (f *fixture) withSink(s spoolSink) {
	d := f.flow.deps
	d.Spool = s
	f.flow.SetDeps(d)
}

// ---------------------------------------------------------------------------
// AC1 / AC2 — the two outcomes, and only ever one of them
// ---------------------------------------------------------------------------

// TestDispatchIsRefusedHonestlyWhenTheSinkIsNotReplayable is the refusal half of
// AC1: a sink that can be written but never replayed (the shape the desk adapter
// has) must produce a refusal that NAMES the coupling, and the flow must never
// record `dispatch_state="spooled"` for it.
func TestDispatchIsRefusedHonestlyWhenTheSinkIsNotReplayable(t *testing.T) {
	// AC-32: a sink that can be written but never replayed yields `unspooled`
	// (with the coupling), and never `spooled`.
	ctx := context.Background()
	fx := newFixture(t, nil)
	sink := &enqueueOnlySink{}
	fx.withSink(sink)
	if fx.flow.SpoolWired() {
		t.Fatalf("an Enqueue-only sink must not be adopted as a replayable queue")
	}
	fx.spawn.err = fmt.Errorf("router_spawn refused the connection")

	if _, err := fx.flow.Spawn(ctx, fx.spawnReq("")); err != nil {
		t.Fatalf("a failed admission must still return an outcome: %v", err)
	}
	if len(sink.got) != 0 {
		t.Fatalf("sink received %d entries: a non-replayable sink must not be written to by the durable path", len(sink.got))
	}

	var unspooled, spooled int
	for _, d := range fx.rec.ofKind(types.KFlow) {
		switch d.Payload["dispatch_state"] {
		case "unspooled":
			unspooled++
			if d.Payload["coupling"] != spoolCoupling {
				t.Fatalf("the refusal must name the coupling in the record: %v", d.Payload)
			}
			if d.Payload["task_id"] != "tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ" {
				t.Fatalf("the refusal must carry the task id: %v", d.Payload)
			}
		case "spooled":
			spooled++
		}
	}
	// EXACTLY ONE of the two outcomes (AC1): an honest refusal, and no
	// durability claim anywhere in the same run.
	if unspooled == 0 {
		t.Fatalf("no honest refusal recorded for a sink that cannot be replayed")
	}
	if spooled != 0 {
		t.Fatalf("the flow claimed dispatch_state=spooled %d times with no replayable queue", spooled)
	}
}

// TestDispatchIsDurablyQueuedOnTheFlowOwnedStore is the other half of AC1/AC2:
// with the flow's own store wired, the parked spawn is ON DISK, and the record
// says `spooled`.
func TestDispatchIsDurablyQueuedOnTheFlowOwnedStore(t *testing.T) {
	// AC-32: with the flow's own store wired, the parked spawn is on disk (one
	// 0600 file) and the record says `spooled`.
	ctx := context.Background()
	fx := newFixture(t, nil)
	root := filepath.Join(t.TempDir(), "spool", "flow", "spawn")
	sp := fx.withQueue(t, root, SpoolBounds{})
	fx.spawn.err = fmt.Errorf("router_spawn refused the connection")

	if _, err := fx.flow.Spawn(ctx, fx.spawnReq("")); err != nil {
		t.Fatalf("a failed admission must still return an outcome: %v", err)
	}
	if n := sp.Count(); n != 1 {
		t.Fatalf("durable queue depth = %d, want 1", n)
	}
	entries, err := sp.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].Kind != types.SpoolSpawn {
		t.Fatalf("entries = %+v, want one kind=%s entry", entries, types.SpoolSpawn)
	}
	if entries[0].IdemKey != "tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ" {
		t.Fatalf("idem key = %q, want the task id (replay idempotency is the router's dedup on it)", entries[0].IdemKey)
	}
	// The entry is one file, mode 0600, under the §3.9a path.
	des, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(des) != 1 {
		t.Fatalf("files under the queue root = %d, want 1 (one file per entry)", len(des))
	}
	fi, err := des[0].Info()
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("entry mode = %v, want 0600", fi.Mode().Perm())
	}
	p := fx.rec.last(types.KFlow)
	if p["dispatch_state"] != "spooled" {
		t.Fatalf("last flow record dispatch_state = %v, want spooled", p["dispatch_state"])
	}
	// No `gap` record: SPEC-INDEX §3.4 does not give SPEC-08 that kind.
	if gaps := len(fx.rec.ofKind(types.KGap)); gaps != 0 {
		t.Fatalf("flow wrote %d `gap` records; SPEC-INDEX §3.4 gives `gap` to SPEC-03/04/07/09/12/13", gaps)
	}
	// NOT one of the two outcomes twice: nothing was refused in this run.
	for _, d := range fx.rec.ofKind(types.KFlow) {
		if d.Payload["dispatch_state"] == "unspooled" {
			t.Fatalf("an entry landed on disk AND was refused: %v", d.Payload)
		}
	}
}

// TestFlowDoesNotAdoptTheDeskSpool proves the wiring rule itself: the desk
// adapter (Enqueue only) leaves the flow without a queue, while the flow-owned
// store is adopted.
func TestFlowDoesNotAdoptTheDeskSpool(t *testing.T) {
	// AC-32: adoption requires a REPLAYABLE queue; an Enqueue-only sink is not
	// one, and the flow-owned store is.
	fx := newFixture(t, nil)
	fx.withSink(&enqueueOnlySink{})
	if fx.flow.SpoolWired() {
		t.Fatalf("SpoolWired() = true for an Enqueue-only sink")
	}
	fx.withQueue(t, filepath.Join(t.TempDir(), "q"), SpoolBounds{})
	if !fx.flow.SpoolWired() {
		t.Fatalf("SpoolWired() = false for the flow-owned store")
	}
}

// TestFlowDoesNotAdoptATypedNilStore pins the typed-nil trap: a nil *Spool put
// behind Deps.Spool satisfies the replay assertion (the interface is non-nil)
// and would panic on the first dispatch. Adopting it is a crash, not durability,
// so the flow must answer "no queue".
func TestFlowDoesNotAdoptATypedNilStore(t *testing.T) {
	// AC-32: a typed-nil store is refused as "no queue" rather than adopted and
	// panicking on the first dispatch.
	fx := newFixture(t, nil)
	var nilStore *Spool
	d := fx.flow.deps
	d.Spool = nilStore
	fx.flow.SetDeps(d)
	if fx.flow.SpoolWired() {
		t.Fatalf("SpoolWired() = true for a typed-nil store")
	}
	if _, err := fx.flow.Replay(context.Background(), 0); err == nil {
		t.Fatalf("Replay with a typed-nil store must refuse, not report success")
	}
	// And the refusal path must not panic on the way out.
	fx.spawn.err = fmt.Errorf("router_spawn refused the connection")
	if _, err := fx.flow.Spawn(context.Background(), fx.spawnReq("")); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
}

// TestReplayRefusesHonestlyWithNoQueue pins the loop half of the same rule: a
// drain with no queue is an error naming the coupling, not a silent no-op.
func TestReplayRefusesHonestlyWithNoQueue(t *testing.T) {
	// AC-32: a drain with no queue refuses with the coupling named, never a
	// silent no-op.
	ctx := context.Background()
	fx := newFixture(t, nil)
	fx.withSink(&enqueueOnlySink{})
	n, err := fx.flow.Replay(ctx, 0)
	if err == nil || n != 0 {
		t.Fatalf("Replay with no queue = (%d, %v), want (0, a refusal naming the coupling)", n, err)
	}
	if !contains(err.Error(), spoolCoupling) {
		t.Fatalf("refusal %q does not name the coupling %q", err.Error(), spoolCoupling)
	}
}

// TestReplayDueIsFalseOnAnEmptyOrBackedOffQueue keeps the drain cheap: nothing
// due means the loop does no I/O beyond one listing.
func TestReplayDueIsFalseOnAnEmptyOrBackedOffQueue(t *testing.T) {
	// AC-33: the drain tick costs one listing while nothing is due.
	fx := newFixture(t, nil)
	sp := fx.withQueue(t, filepath.Join(t.TempDir(), "q"), SpoolBounds{})
	if fx.flow.ReplayDue() {
		t.Fatalf("ReplayDue() = true on an empty queue")
	}
	if _, err := sp.Put(types.SpoolEntry{
		ID: types.NewID(types.PEv), TS: types.FormatUTC(fx.clock.Now()), Kind: types.SpoolSpawn,
		Payload: []byte(`{}`), IdemKey: "tsk_future",
		NextTryTS: types.FormatUTC(fx.clock.Now().Add(time.Hour)),
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if fx.flow.ReplayDue() {
		t.Fatalf("ReplayDue() = true for an entry that is not due")
	}
	fx.clock.Advance(2 * time.Hour)
	if !fx.flow.ReplayDue() {
		t.Fatalf("ReplayDue() = false for an entry that is past its next_try_ts")
	}
}

// ---------------------------------------------------------------------------
// The replay itself
// ---------------------------------------------------------------------------

// TestFlowOwnedSpoolReplayRedispatchesWithTheSameIdemKey is AC2's desk-ON half
// driven to a real endpoint: the parked spawn is re-dispatched with the SAME idem
// key (so the router's own dedup makes the replay idempotent), the entry is
// deleted on success, a second drain does nothing, and the queue is empty after a
// simulated restart.
func TestFlowOwnedSpoolReplayRedispatchesWithTheSameIdemKey(t *testing.T) {
	// AC-32: a replay delivers the SAME idem_key, deletes the entry on success,
	// and a second drain dispatches nothing.
	ctx := context.Background()
	var mu sync.Mutex
	status := 500
	var keys []string
	srv := schedulerStub(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var p map[string]any
		_ = json.Unmarshal(b, &p)
		mu.Lock()
		keys = append(keys, fmt.Sprint(p["idem_key"]))
		st := status
		mu.Unlock()
		w.WriteHeader(st)
	})
	defer srv.Close()

	fx := newFixture(t, func(c *types.FlowConfig) {
		c.Router = types.RouterConfig{Mode: "http", Endpoint: srv.URL, DispatchPath: "/dispatch", Retries: 0}
	})
	root := filepath.Join(t.TempDir(), "spool", "flow", "spawn")
	sp := fx.withQueue(t, root, SpoolBounds{})
	fx.spawn.err = fmt.Errorf("router_spawn refused the connection")

	if _, err := fx.flow.Spawn(ctx, fx.spawnReq("")); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if sp.Count() != 1 {
		t.Fatalf("queue depth = %d, want 1", sp.Count())
	}

	// The router recovers; the next_try_ts has elapsed.
	mu.Lock()
	status = 200
	mu.Unlock()
	fx.clock.Advance(6 * time.Second)

	n, err := fx.flow.Replay(ctx, 0)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if n != 1 {
		t.Fatalf("replayed = %d, want 1", n)
	}
	if sp.Count() != 0 {
		t.Fatalf("queue depth after a successful replay = %d, want 0 (the entry is deleted on success)", sp.Count())
	}
	mu.Lock()
	got := append([]string(nil), keys...)
	mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("router saw %d dispatches, want exactly 1: %v", len(got), got)
	}
	if got[0] != "tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ" {
		t.Fatalf("replay idem key = %q, want the original task id", got[0])
	}
	last := fx.rec.last(types.KFlow)
	if last["dispatch_state"] != "replayed" {
		t.Fatalf("last flow record dispatch_state = %v, want replayed", last["dispatch_state"])
	}
	if last["idem_key"] != got[0] {
		t.Fatalf("the replay record's idem_key = %v, want %q", last["idem_key"], got[0])
	}

	// Idempotent by construction: a second drain has nothing to do — no second
	// dispatch of the same task.
	n2, err := fx.flow.Replay(ctx, 0)
	if err != nil {
		t.Fatalf("second Replay: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("second Replay replayed = %d, want 0", n2)
	}
	mu.Lock()
	after := len(keys)
	mu.Unlock()
	if after != 1 {
		t.Fatalf("router saw %d dispatches after the second drain, want 1", after)
	}

	// A restart: a brand-new store over the same root still sees an empty queue,
	// which is the property the pre-§3.9a code could not have.
	restarted, err := NewSpool(root, SpoolBounds{}, func() time.Time { return fx.clock.Now() })
	if err != nil {
		t.Fatalf("NewSpool (restart): %v", err)
	}
	if restarted.Count() != 0 {
		t.Fatalf("queue after restart = %d, want 0", restarted.Count())
	}
}

// TestSpoolSurvivesRestartAndReplaysAfterIt is the durability claim itself: an
// entry written by one store instance is listed, decoded and replayed by a
// second one — the restart loss the in-memory queue could not avoid.
func TestSpoolSurvivesRestartAndReplaysAfterIt(t *testing.T) {
	// AC-32: the entry survives a restart — a second store instance over the
	// same root lists it and replays it with the surviving task id.
	ctx := context.Background()
	var mu sync.Mutex
	var keys []string
	srv := schedulerStub(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var p map[string]any
		_ = json.Unmarshal(b, &p)
		mu.Lock()
		keys = append(keys, fmt.Sprint(p["idem_key"]))
		mu.Unlock()
		w.WriteHeader(200)
	})
	defer srv.Close()

	fx := newFixture(t, func(c *types.FlowConfig) {
		c.Router = types.RouterConfig{Mode: "http", Endpoint: srv.URL, DispatchPath: "/dispatch", Retries: 0}
	})
	root := filepath.Join(t.TempDir(), "spool", "flow", "spawn")

	// Instance 1 parks the spawn with the router unreachable.
	first := fx.withQueue(t, root, SpoolBounds{})
	fx.spawn.err = fmt.Errorf("router_spawn refused the connection")
	if _, err := fx.flow.Spawn(ctx, fx.spawnReq("")); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if first.Count() != 1 {
		t.Fatalf("instance 1 queue depth = %d, want 1", first.Count())
	}

	// Instance 2 (a fresh process) adopts the same root and drains it.
	second, err := NewSpool(root, SpoolBounds{}, func() time.Time { return fx.clock.Now() })
	if err != nil {
		t.Fatalf("NewSpool (instance 2): %v", err)
	}
	if second.Count() != 1 {
		t.Fatalf("instance 2 queue depth = %d, want 1 — the entry did not survive the restart", second.Count())
	}
	d := fx.flow.deps
	d.Spool = second
	fx.flow.SetDeps(d)
	fx.clock.Advance(6 * time.Second)

	n, err := fx.flow.Replay(ctx, 0)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if n != 1 {
		t.Fatalf("replayed after restart = %d, want 1", n)
	}
	if second.Count() != 0 {
		t.Fatalf("queue after replay = %d, want 0", second.Count())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 1 || keys[0] != "tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ" {
		t.Fatalf("router keys = %v, want the surviving task id", keys)
	}
}

// TestReplayDropsAfterTheAttemptBudgetLeavesNothingSilent pins the drop pair:
// exhaustion removes the entry AND records the cause naming incident, sig and
// task id.
func TestReplayDropsAfterTheAttemptBudgetLeavesNothingSilent(t *testing.T) {
	// AC-33: the attempt budget drops the entry with `attempts` + the coupling,
	// and the failed replay is counted against the entry (the queue does not grow).
	ctx := context.Background()
	srv := schedulerStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) })
	defer srv.Close()

	fx := newFixture(t, func(c *types.FlowConfig) {
		c.Router = types.RouterConfig{Mode: "http", Endpoint: srv.URL, DispatchPath: "/dispatch", Retries: 1}
	})
	sp := fx.withQueue(t, filepath.Join(t.TempDir(), "q"), SpoolBounds{MaxAttempts: 2})
	fx.spawn.err = fmt.Errorf("router_spawn refused the connection")
	if _, err := fx.flow.Spawn(ctx, fx.spawnReq("")); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	fx.clock.Advance(6 * time.Second)
	if _, err := fx.flow.Replay(ctx, 0); err != nil {
		t.Fatalf("Replay 1: %v", err)
	}
	if sp.Count() != 1 {
		t.Fatalf("queue depth after attempt 1 = %d, want 1 (attempts 1 < 2)", sp.Count())
	}
	entries, _ := sp.List()
	if entries[0].Attempts != 1 {
		t.Fatalf("attempts after one failed replay = %d, want 1", entries[0].Attempts)
	}
	failed := fx.rec.last(types.KFlow)
	if failed["dispatch_state"] != "replay_failed" {
		t.Fatalf("the failed replay must be recorded: %v", failed)
	}

	fx.clock.Advance(30 * time.Second)
	if _, err := fx.flow.Replay(ctx, 0); err != nil {
		t.Fatalf("Replay 2: %v", err)
	}
	if sp.Count() != 0 {
		t.Fatalf("queue depth after the attempt budget = %d, want 0", sp.Count())
	}
	last := fx.rec.last(types.KFlow)
	if last["dispatch_state"] != "dropped" || last["drop_reason"] != "attempts" {
		t.Fatalf("the drop must be recorded with its cause: %v", last)
	}
	if last["task_id"] != "tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ" || last["coupling"] != spoolCoupling {
		t.Fatalf("the drop record must carry the task id and the coupling: %v", last)
	}
}

// TestReplayDropsAnExpiredEntry pins the TTL half of the same rule.
func TestReplayDropsAnExpiredEntry(t *testing.T) {
	// AC-33: an entry past flow.spool_ttl drops with drop_reason ttl.
	ctx := context.Background()
	fx := newFixture(t, nil)
	sp := fx.withQueue(t, filepath.Join(t.TempDir(), "q"), SpoolBounds{TTL: time.Minute})
	if _, err := sp.Put(types.SpoolEntry{
		ID: types.NewID(types.PEv), TS: types.FormatUTC(fx.clock.Now()), Kind: types.SpoolSpawn,
		Payload: mustMarshalSpawn(t, fx.spawnReq("")), IdemKey: "tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ",
		NextTryTS: types.FormatUTC(fx.clock.Now()),
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	fx.clock.Advance(2 * time.Minute)
	if _, err := fx.flow.Replay(ctx, 0); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if sp.Count() != 0 {
		t.Fatalf("queue depth = %d, want 0 (the entry is past its TTL)", sp.Count())
	}
	last := fx.rec.last(types.KFlow)
	if last["dispatch_state"] != "dropped" || last["drop_reason"] != "ttl" {
		t.Fatalf("the TTL drop must be recorded: %v", last)
	}
}

// TestReplayDropsACorruptEntryInsteadOfLoopingForever pins the decode branch: an
// entry this build cannot read is dropped with a cause rather than retried until
// the TTL. (The pre-§3.9a desk decode dropped the flow's own payload shape as
// `corrupt` — which is exactly why the flow now owns its own shape.)
func TestReplayDropsACorruptEntryInsteadOfLoopingForever(t *testing.T) {
	// AC-33: an undecodable payload drops with drop_reason corrupt rather than
	// being retried until the TTL.
	ctx := context.Background()
	fx := newFixture(t, nil)
	sp := fx.withQueue(t, filepath.Join(t.TempDir(), "q"), SpoolBounds{})
	if _, err := sp.Put(types.SpoolEntry{
		ID: types.NewID(types.PEv), TS: types.FormatUTC(fx.clock.Now()), Kind: types.SpoolSpawn,
		Payload: []byte("not json at all"), IdemKey: "tsk_corrupt",
		NextTryTS: types.FormatUTC(fx.clock.Now()),
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := fx.flow.Replay(ctx, 0); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if sp.Count() != 0 {
		t.Fatalf("queue depth = %d, want 0 (a corrupt entry is dropped, not looped)", sp.Count())
	}
	last := fx.rec.last(types.KFlow)
	if last["drop_reason"] != "corrupt" {
		t.Fatalf("drop reason = %v, want corrupt", last["drop_reason"])
	}
}

// TestFlowSpoolBoundsDropOldestWithALedgerNote pins the bound: the queue shrinks
// under pressure by evicting the OLDEST entry — never the incoming one — and each
// eviction is a pair of records. The writes go through the flow's own durable
// path, so the drop accounting under test is the shipped one.
func TestFlowSpoolBoundsDropOldestWithALedgerNote(t *testing.T) {
	// AC-33: overflow evicts the OLDEST entry, never the incoming one, with one
	// `overflow` drop record per eviction (LIFECYCLE-015) and no `gap` record.
	ctx := context.Background()
	fx := newFixture(t, nil)
	sp := fx.withQueue(t, filepath.Join(t.TempDir(), "q"), SpoolBounds{MaxEntries: 2})
	// TS ordering is the eviction order, so the sequence is explicit: each write
	// lands one clock second after the previous one.
	put := func() {
		fx.clock.Advance(time.Second)
		req := fx.spawnReq("")
		req.TaskID = "tsk_" + types.FormatUTC(fx.clock.Now())
		inc := types.Incident{ID: req.Inc, Sig: req.Sig}
		if ok := fx.flow.spoolPut(ctx, "dispatch", "spool_not_replayable", req, inc); !ok {
			t.Fatalf("spoolPut refused an entry it must accept")
		}
	}
	put()
	put()
	put()
	put()

	if n := sp.Count(); n != 2 {
		t.Fatalf("queue depth = %d, want 2 (the bound)", n)
	}
	entries, _ := sp.List()
	if len(entries) != 2 {
		t.Fatalf("List returned %d entries, want 2", len(entries))
	}
	// The OLDEST two were evicted; the newest two survive, in write order.
	if entries[0].ID == "" || entries[1].ID == "" {
		t.Fatalf("entries lack ids: %+v", entries)
	}
	for i := 1; i < len(entries); i++ {
		if entries[i-1].TS >= entries[i].TS {
			t.Fatalf("surviving entries are not in write order: %v then %v", entries[i-1].TS, entries[i].TS)
		}
	}
	var drops int
	for _, d := range fx.rec.ofKind(types.KFlow) {
		if d.Payload["dispatch_state"] == "dropped" {
			drops++
			if d.Payload["drop_reason"] != "overflow" {
				t.Fatalf("drop reason = %v, want overflow", d.Payload["drop_reason"])
			}
			if d.Payload["error_code"] != string(types.CodeLifecycle015) {
				t.Fatalf("the overflow drop error code = %v, want %s", d.Payload["error_code"], types.CodeLifecycle015)
			}
			if d.Payload["coupling"] != spoolCoupling {
				t.Fatalf("the drop record must name the coupling: %v", d.Payload)
			}
		}
	}
	if drops != 2 {
		t.Fatalf("overflow drop records = %d, want 2 (one per eviction)", drops)
	}
	if len(fx.rec.ofKind(types.KGap)) != 0 {
		t.Fatalf("flow wrote a `gap` record; SPEC-INDEX §3.4 does not give SPEC-08 that kind")
	}
}

// TestFlowSpoolSkipsForeignAndTornFiles keeps a scratch file (or a half-written
// temp file) from being decoded as an entry.
func TestFlowSpoolSkipsForeignAndTornFiles(t *testing.T) {
	// AC-32: a foreign scratch file and a torn entry are not decoded as entries.
	fx := newFixture(t, nil)
	root := filepath.Join(t.TempDir(), "q")
	sp := fx.withQueue(t, root, SpoolBounds{})
	if err := os.WriteFile(filepath.Join(root, "operator-notes.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatalf("write scratch: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".tmp-12345"), []byte("{torn"), 0o600); err != nil {
		t.Fatalf("write torn temp: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "ev_01DDDDDDDDDDDDDDDDDDDDDDDD.json"), []byte("{torn"), 0o600); err != nil {
		t.Fatalf("write torn entry: %v", err)
	}
	entries, err := sp.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("List returned %d entries from foreign/torn files, want 0", len(entries))
	}
}

// TestFlowRunDrainsTheQueueAndProbes is the loop's own test: Run is the half the
// daemon's comment claimed existed while `Flow.Start` ran neither. The deadline is
// generous because this test is wall-clock driven by design (it drives the real
// ticker); the assertion is "the entry is gone well inside the deadline", not a
// latency budget.
func TestFlowRunDrainsTheQueueAndProbes(t *testing.T) {
	// AC-33: `Flow.Run` owns the drain cadence (flow.replay_every) beside the
	// registration probe.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := schedulerStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	defer srv.Close()

	fx := newFixture(t, func(c *types.FlowConfig) {
		c.Router = types.RouterConfig{Mode: "http", Endpoint: srv.URL, DispatchPath: "/dispatch", Retries: 0}
	})
	sp := fx.withQueue(t, filepath.Join(t.TempDir(), "q"), SpoolBounds{ReplayEvery: 10 * time.Millisecond})
	if _, err := sp.Put(types.SpoolEntry{
		ID: types.NewID(types.PEv), TS: types.FormatUTC(fx.clock.Now()), Kind: types.SpoolSpawn,
		Payload: mustMarshalSpawn(t, fx.spawnReq("")), IdemKey: "tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ",
		NextTryTS: types.FormatUTC(fx.clock.Now()),
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	go fx.flow.Run(ctx)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if sp.Count() == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("Run did not drain the queue within the deadline (depth still %d)", sp.Count())
}

// ---------------------------------------------------------------------------
// The AC-32 / AC-33 branches the section states that had no test at all
// ---------------------------------------------------------------------------

// TestDispatchIsRecordedAsSpoolFailedWhenTheStoreRefusesTheWrite is AC-32's
// third refusal branch: a store whose tree was proved writable when it was
// built and then REFUSED the write must be recorded as `spool_failed` — with
// the reason and the coupling — never as `spooled`. "The entry landed on a
// queue this subsystem replays" is a claim about the write that happened, not
// about the wiring that was intended.
func TestDispatchIsRecordedAsSpoolFailedWhenTheStoreRefusesTheWrite(t *testing.T) {
	// AC-32: a refused write is spool_failed with a reason and a coupling, and
	// no durability claim anywhere in the run.
	ctx := context.Background()
	fx := newFixture(t, nil)
	root := filepath.Join(t.TempDir(), "spool", "flow", "spawn")
	fx.withQueue(t, root, SpoolBounds{})
	// The store proved its tree writable at build time. Now make the path
	// unusable — a FILE where the entry directory belongs, so the write's
	// MkdirAll fails with ENOTDIR and Put reports the refusal.
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("remove the queue root: %v", err)
	}
	if err := os.WriteFile(root, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("block the queue root: %v", err)
	}
	fx.spawn.err = fmt.Errorf("router_spawn refused the connection")
	if _, err := fx.flow.Spawn(ctx, fx.spawnReq("")); err != nil {
		t.Fatalf("a failed admission must still return an outcome: %v", err)
	}

	var spooled int
	for _, d := range fx.rec.ofKind(types.KFlow) {
		if d.Payload["dispatch_state"] == "spooled" {
			spooled++
		}
	}
	if spooled != 0 {
		t.Fatalf("the flow claimed dispatch_state=spooled %d times for a write the store refused", spooled)
	}
	last := fx.rec.last(types.KFlow)
	if last["dispatch_state"] != "spool_failed" {
		t.Fatalf("dispatch_state = %v, want spool_failed", last["dispatch_state"])
	}
	if reason := fmt.Sprint(last["reason"]); !strings.HasPrefix(reason, "spool_write") {
		t.Fatalf("reason = %q, want the refused write named (spool_write: ...)", reason)
	}
	if last["coupling"] != spoolCoupling {
		t.Fatalf("the refusal must name the coupling in the record: %v", last)
	}
	if last["task_id"] != "tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ" {
		t.Fatalf("the refusal must carry the task id: %v", last)
	}
	if last["error_code"] != string(types.CodeFlow005) {
		t.Fatalf("error_code = %v, want %s", last["error_code"], types.CodeFlow005)
	}
	// The `spawn` record is written only after the durable write succeeded, so
	// no spawn record may carry a durability claim either.
	for _, d := range fx.rec.ofKind(types.KSpawn) {
		if d.Payload["dispatch_state"] == "spooled" {
			t.Fatalf("a spawn record claims dispatch_state=spooled for a refused write: %v", d.Payload)
		}
	}
}

// TestReplayHoldsOneInFlightDispatchPerIdemKey is AC-32's single-flight rule:
// while a replayed dispatch is in flight, a second drain of the same entry
// dispatches NOTHING. One in-flight dispatch per idem key is what keeps a
// replay from racing the original attempt (the router's own task_id dedup is
// the second line of defence, not the first).
func TestReplayHoldsOneInFlightDispatchPerIdemKey(t *testing.T) {
	// AC-32: at most one in-flight dispatch per idem_key.
	ctx := context.Background()
	release := make(chan struct{})
	var once sync.Once
	releaseAll := func() { once.Do(func() { close(release) }) }
	// releaseAll must be deferred BEFORE the stub: the test's deferred calls run
	// before the stub's t.Cleanup(srv.Close), and Close waits for the handler,
	// which waits on this channel. (No `defer srv.Close()` here for the same
	// reason: it would order itself in front of releaseAll.)
	defer releaseAll()
	seen := make(chan struct{}, 1)
	var mu sync.Mutex
	var keys []string
	srv := schedulerStub(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var p map[string]any
		_ = json.Unmarshal(b, &p)
		mu.Lock()
		keys = append(keys, fmt.Sprint(p["idem_key"]))
		first := len(keys) == 1
		mu.Unlock()
		if first {
			select {
			case seen <- struct{}{}:
			default:
			}
			<-release // hold the first dispatch in flight
		}
		w.WriteHeader(200)
	})

	fx := newFixture(t, func(c *types.FlowConfig) {
		c.Router = types.RouterConfig{Mode: "http", Endpoint: srv.URL, DispatchPath: "/dispatch", Retries: 0}
	})
	sp := fx.withQueue(t, filepath.Join(t.TempDir(), "q"), SpoolBounds{})
	fx.spawn.err = fmt.Errorf("router_spawn refused the connection")
	if _, err := fx.flow.Spawn(ctx, fx.spawnReq("")); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if sp.Count() != 1 {
		t.Fatalf("queue depth = %d, want 1", sp.Count())
	}
	fx.clock.Advance(6 * time.Second)

	type drainResult struct {
		n   int
		err error
	}
	done := make(chan drainResult, 1)
	go func() {
		n, err := fx.flow.Replay(ctx, 0)
		done <- drainResult{n: n, err: err}
	}()
	select {
	case <-seen:
	case <-time.After(10 * time.Second):
		t.Fatalf("the first drain never reached the router")
	}

	// The entry is in flight now. A second drain must find the idem key claimed
	// and dispatch nothing — it may neither re-send the entry nor delete it.
	n2, err2 := fx.flow.Replay(ctx, 0)
	if err2 != nil {
		t.Fatalf("second Replay: %v", err2)
	}
	if n2 != 0 {
		t.Fatalf("a concurrent second drain replayed %d entries, want 0 (one in-flight dispatch per idem_key)", n2)
	}
	mu.Lock()
	during := len(keys)
	mu.Unlock()
	if during != 1 {
		t.Fatalf("the router saw %d dispatches while the first was in flight, want 1", during)
	}

	releaseAll()
	r := <-done
	if r.err != nil {
		t.Fatalf("first Replay: %v", r.err)
	}
	if r.n != 1 {
		t.Fatalf("first Replay replayed %d, want 1", r.n)
	}
	if sp.Count() != 0 {
		t.Fatalf("queue depth = %d, want 0 (the entry is deleted once the dispatch succeeds)", sp.Count())
	}
	mu.Lock()
	total := len(keys)
	mu.Unlock()
	if total != 1 {
		t.Fatalf("the router saw %d dispatches in total, want exactly 1 for one entry", total)
	}
}

// TestReplayAcceptsA409DuplicateOnReplay is the idempotency guard §3.9a names:
// the router's own task_id dedup. A replay the router answers with 409 (its
// `duplicate:true` shape) is ACCEPTED — the entry is deleted and the record
// says `replayed` — so a replay can neither loop on work the original attempt
// already placed nor drop an entry whose work is already queued.
func TestReplayAcceptsA409DuplicateOnReplay(t *testing.T) {
	// AC-32: 409/duplicate:true is an accepted dispatch, so a replay is
	// idempotent.
	ctx := context.Background()
	var mu sync.Mutex
	var keys []string
	srv := schedulerStub(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var p map[string]any
		_ = json.Unmarshal(b, &p)
		mu.Lock()
		keys = append(keys, fmt.Sprint(p["idem_key"]))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"duplicate":true}`))
	})
	defer srv.Close()

	fx := newFixture(t, func(c *types.FlowConfig) {
		c.Router = types.RouterConfig{Mode: "http", Endpoint: srv.URL, DispatchPath: "/dispatch", Retries: 0}
	})
	sp := fx.withQueue(t, filepath.Join(t.TempDir(), "q"), SpoolBounds{})
	fx.spawn.err = fmt.Errorf("router_spawn refused the connection")
	if _, err := fx.flow.Spawn(ctx, fx.spawnReq("")); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	fx.clock.Advance(6 * time.Second)

	n, err := fx.flow.Replay(ctx, 0)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if n != 1 {
		t.Fatalf("replayed = %d, want 1: a 409 duplicate is an accepted dispatch, not a failure", n)
	}
	if sp.Count() != 0 {
		t.Fatalf("queue depth after a 409 replay = %d, want 0", sp.Count())
	}
	mu.Lock()
	got := append([]string(nil), keys...)
	mu.Unlock()
	if len(got) != 1 || got[0] != "tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ" {
		t.Fatalf("router keys = %v, want exactly the original task id", got)
	}
	last := fx.rec.last(types.KFlow)
	if last["dispatch_state"] != "replayed" {
		t.Fatalf("dispatch_state = %v, want replayed", last["dispatch_state"])
	}
	for _, d := range fx.rec.ofKind(types.KFlow) {
		if d.Payload["dispatch_state"] == "replay_failed" {
			t.Fatalf("a 409 duplicate was recorded as a failed replay: %v", d.Payload)
		}
	}
}

func mustMarshalSpawn(t *testing.T, req types.SpawnRequest) []byte {
	t.Helper()
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal spawn: %v", err)
	}
	return b
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }
