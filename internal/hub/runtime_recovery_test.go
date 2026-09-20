package hub

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// runtime_recovery_test.go is the TRBL-031 half of SPEC-13 §4.3's matrix: the
// rows that describe a daemon which BOOTED UP and then lost its queue.
//
//	| Redis lost at runtime | false | 429 + Retry-After | keeps running | 004  |
//	| Redis returns         | —     | 200               | rewires the consumer | lifecycle `redis_restored` |
//
// and §2.1.1 rule 5, which puts a cold (empty) server in the same family: "the
// daemon re-creates the group with MKSTREAM $".
//
// Nothing in these tests calls reconnect, attach or startConsumer. The only
// thing that may bring the queue back is the supervisor the runtime started
// itself, which is the property the ticket is about.
//
// A note on the oracle for "the stale consumer is gone": the counter oracle used
// below is validated INSIDE the same test — the consumer's read_errors are first
// shown to grow while it is looping on NOGROUP, and the same reading is then
// shown frozen after the rewire. A frozen counter therefore means "no goroutine
// is running it", never "the counter happens to be quiet".

// waitFor polls cond until it holds or the deadline passes; the timeout message
// is the failure the ticket's RED run prints.
func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", within, what)
}

// lifecycleOps counts the lifecycle records the ledger holds for one op.
func lifecycleOps(l *fakeLedger, op string) int {
	n := 0
	for _, r := range l.Records() {
		if r.Kind != types.KLifecycle {
			continue
		}
		if got, _ := r.Payload["op"].(string); got == op {
			n++
		}
	}
	return n
}

// groupExists reports whether the fake server holds the group (a cold server
// holds neither the stream nor the group).
func (f *fakeStreams) groupExists(stream, group string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.groups[stream][group]
	return ok
}

// goCold takes the fake server away: it stops answering AND it comes back
// without the stream and the group. That is one model for both halves of §4.3
// and §2.1.1 rule 5 — the daemon's next dial must re-create the group.
func (f *fakeStreams) goCold(stream, group string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pingErr = errors.New("dial tcp 127.0.0.1:6379: connect: connection refused")
	f.failXAdd = errors.New("dial tcp 127.0.0.1:6379: connect: connection refused")
	delete(f.groups[stream], group)
	f.streams[stream] = nil
}

// comeBack makes the fake server answer again, still empty (the cold server).
func (f *fakeStreams) comeBack() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pingErr = nil
	f.failXAdd = nil
}

// flush wipes the queue state of a server that keeps answering: the shape of
// `FLUSHALL`/failover, where nothing in the serve path can notice.
func (f *fakeStreams) flush(stream, group string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.groups[stream], group)
	f.streams[stream] = nil
}

// TestStandaloneStartStartsNothing pins the other half of the standalone
// invariant (A4 of TRBL-031): the recovery supervisor runs for an ENABLED
// runtime only. A standalone boot must stay a no-op — no lifetime context, no
// supervisor goroutine, no consumer registration, no state tree — because the
// supervisor is exactly the machinery this ticket added.
func TestStandaloneStartStartsNothing(t *testing.T) {
	root := t.TempDir()
	rt, err := Open(context.Background(), RuntimeConfig{
		Profile:   types.ProfileConfig{Profile: ProfileStandalone, Valid: true},
		Redis:     RedisConfig{URL: "redis://127.0.0.1:1/0"}, // would fail to dial if it were used
		StateRoot: root,
		HostID:    "h1",
	})
	if err != nil {
		t.Fatalf("Open(standalone): %v", err)
	}
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start(standalone): %v", err)
	}
	rt.mu.Lock()
	cancel, lifeCtx, consumerCancel := rt.cancel, rt.lifeCtx, rt.consumerCancel
	rt.mu.Unlock()
	if cancel != nil || lifeCtx != nil || consumerCancel != nil {
		t.Fatalf("standalone Start started something: cancel=%v lifeCtx=%v consumerCancel=%v", cancel != nil, lifeCtx != nil, consumerCancel != nil)
	}
	if _, err := os.Stat(StateDir(root)); !os.IsNotExist(err) {
		t.Fatalf("standalone Start created the hub state tree (%v)", err)
	}
	if err := rt.Close(); err != nil {
		t.Fatalf("Close(standalone): %v", err)
	}
}

func TestRuntimeRecoversFromARuntimeLossThroughTheSupervisor(t *testing.T) {
	f := newFakeStreams()
	f.info = hubInfoOK()
	// A short BLOCK keeps the consumer's loop responsive and the test cheap; the
	// supervisor's own cadence is the production one.
	f.block = 5 * time.Millisecond
	events := &eventLog{}
	ledger := newFakeLedger(events)
	standalone := newFakeLedger(events)
	cfg := runtimeCfg(t, f, ledger, standalone, func(c *RuntimeConfig) {
		c.Redis.DoorWait = 200 * time.Millisecond
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

	// A live queue: the first event is 200-answered and reaches the ledger.
	sig := "sentinel:sha256v1:9f2c1d3e4b5a6c7d"
	if _, err := rt.Ingest(ctx, draftFor(types.KEvent, sig, "sentinel", map[string]any{"item_type": "event", "n": "1"})); err != nil {
		t.Fatalf("first ingest on a live queue: %v", err)
	}
	waitFor(t, 2*time.Second, "the first event in the ledger", func() bool { return eventRecords(ledger) == 1 })
	stale := rt.Consumer()
	if stale == nil {
		t.Fatal("no consumer after a clean boot")
	}

	// Redis goes away AND comes back cold: the connection is refused and the
	// stream and group are gone (they are re-created by the rewire, not by the
	// consumer, which cannot create them).
	f.goCold("trouble:ingest", "ledger-writers")

	// FIRST-HAND ANSWER to the ticket's question: the consumer goroutine does
	// NOT die on this path. It loops on NOGROUP (XPENDING → 004 → backoff), which
	// is measurable: read_errors grows while nothing can be appended.
	before := stale.Stats()
	time.Sleep(300 * time.Millisecond)
	looping := stale.Stats()
	if looping.ReadErrors <= before.ReadErrors {
		t.Fatalf("the consumer did not loop on the missing group: %+v then %+v", before, looping)
	}

	// A sender is refused (§4.3 row "Redis lost at runtime") and the state
	// machine moves with it.
	if _, err := rt.Ingest(ctx, draftFor(types.KEvent, sig, "sentinel", map[string]any{"item_type": "event", "n": "2"})); CodeOf(err) != types.CodeHub004 {
		t.Fatalf("code while the queue is lost = %q want TROUBLE-HUB-004 (%v)", CodeOf(err), err)
	}
	if rt.RuntimeMode() != ModeUnavailable {
		t.Fatalf("mode = %q want unavailable (require_redis=false)", rt.RuntimeMode())
	}
	if n := lifecycleOps(ledger, "redis_lost"); n != 1 {
		t.Fatalf("redis_lost records = %d want 1", n)
	}

	// Redis returns. NOTHING here rewires: the supervisor the runtime started
	// for itself is the only thing that may.
	f.comeBack()
	waitFor(t, 8*time.Second, "the supervisor to rewire the queue", func() bool {
		return rt.RuntimeMode() == ModeUp
	})

	// The cold server got its stream and group back (MKSTREAM: the second
	// XGROUP CREATE of the test's lifetime is the re-creation).
	f.mu.Lock()
	creates := f.groupCreateCalled
	f.mu.Unlock()
	if creates < 2 {
		t.Fatalf("XGROUP CREATE ran %d time(s): the cold server never got its group back", creates)
	}
	if !f.groupExists("trouble:ingest", "ledger-writers") {
		t.Fatal("the group does not exist after the rewire")
	}
	// Exactly ONE recovery record.
	if n := lifecycleOps(ledger, "redis_restored"); n != 1 {
		t.Fatalf("redis_restored records = %d want exactly 1", n)
	}
	// The runtime reports `up` honestly: not degraded, no reason, the Redis
	// window back in force (§4.3 "Redis returns" → /health.json `ok`).
	if st := rt.Status(ctx); st.Degraded || st.DegradedReason != DegradedNone || st.Redis.DedupWindow != "redis" {
		t.Fatalf("status after the rewire = %+v", st)
	}
	// Exactly ONE consumer: a fresh one, and the stale one is gone (its counters
	// are frozen — the same reading that was shown GROWING above).
	if now := rt.Consumer(); now == nil || now == stale {
		t.Fatalf("the rewire did not install a new consumer: stale=%p now=%p", stale, now)
	}
	frozen := stale.Stats()
	time.Sleep(300 * time.Millisecond)
	if again := stale.Stats(); again != frozen {
		t.Fatalf("the stale consumer is still running after the rewire: %+v then %+v", frozen, again)
	}
	// And the queue serves again: the next event is appended, and the XACK
	// followed the append (XPENDING 0).
	rec, err := rt.Ingest(ctx, draftFor(types.KEvent, sig, "sentinel", map[string]any{"item_type": "event", "n": "3"}))
	if err != nil {
		t.Fatalf("ingest after the rewire: %v", err)
	}
	if rec.Seq == 0 {
		t.Fatalf("the queue answered with a record that has no ledger identity: %+v", rec)
	}
	waitFor(t, 2*time.Second, "the event after the rewire in the ledger", func() bool { return eventRecords(ledger) == 2 })
	waitFor(t, 2*time.Second, "XPENDING back to zero", func() bool {
		return f.pelSize("trouble:ingest", "ledger-writers") == 0
	})
	if standalone.Count() != 0 {
		t.Fatalf("the standalone fallback was used: %d records", standalone.Count())
	}
}

func TestRuntimeColdRedisIsRewiredWithoutASenderFailure(t *testing.T) {
	f := newFakeStreams()
	f.info = hubInfoOK()
	f.block = 5 * time.Millisecond
	events := &eventLog{}
	ledger := newFakeLedger(events)
	cfg := runtimeCfg(t, f, ledger, newFakeLedger(events), func(c *RuntimeConfig) {
		c.Redis.DoorWait = 200 * time.Millisecond
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

	sig := "sentinel:sha256v1:9f2c1d3e4b5a6c7d"
	if _, err := rt.Ingest(ctx, draftFor(types.KEvent, sig, "sentinel", map[string]any{"item_type": "event", "n": "1"})); err != nil {
		t.Fatalf("first ingest on a live queue: %v", err)
	}
	waitFor(t, 2*time.Second, "the first event in the ledger", func() bool { return eventRecords(ledger) == 1 })

	// The server is flushed while it keeps answering: §2.1.1 rule 5's "cold
	// (empty) Redis after failover or flush". No sender has failed, the state
	// machine still says `up`, and XADD would happily create the key again — so
	// the health surface is NOT the thing that notices. The supervisor is.
	f.flush("trouble:ingest", "ledger-writers")
	if rt.RuntimeMode() != ModeUp {
		t.Fatalf("mode = %q: a flush is not something the serve path can see", rt.RuntimeMode())
	}

	waitFor(t, 8*time.Second, "the supervisor to re-create the group on the cold server", func() bool {
		return f.groupExists("trouble:ingest", "ledger-writers")
	})
	if n := lifecycleOps(ledger, "redis_restored"); n != 1 {
		t.Fatalf("redis_restored records = %d want exactly 1", n)
	}
	if st := rt.Status(ctx); st.Degraded || st.Redis.DedupWindow != "redis" {
		t.Fatalf("status after the cold rewire = %+v", st)
	}

	// Ingestion is answered again, through the re-created group.
	rec, err := rt.Ingest(ctx, draftFor(types.KEvent, sig, "sentinel", map[string]any{"item_type": "event", "n": "2"}))
	if err != nil {
		t.Fatalf("ingest after the cold rewire: %v", err)
	}
	if rec.Seq == 0 {
		t.Fatalf("no ledger identity after the cold rewire: %+v", rec)
	}
	waitFor(t, 2*time.Second, "the event after the cold rewire in the ledger", func() bool { return eventRecords(ledger) == 2 })
	waitFor(t, 2*time.Second, "XPENDING back to zero", func() bool {
		return f.pelSize("trouble:ingest", "ledger-writers") == 0
	})
}

func TestRuntimeRestartsAConsumerThatExited(t *testing.T) {
	f := newFakeStreams()
	f.info = hubInfoOK()
	f.block = 5 * time.Millisecond
	events := &eventLog{}
	ledger := newFakeLedger(events)
	cfg := runtimeCfg(t, f, ledger, newFakeLedger(events), func(c *RuntimeConfig) {
		c.Redis.DoorWait = 200 * time.Millisecond
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

	// registered reports whether a consumer goroutine is on the queue.
	registered := func() bool {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		return rt.consumerCancel != nil
	}
	if !registered() {
		t.Fatal("no consumer after a clean boot")
	}

	// The consumer EXITS (its own cancellation — the same observable shape as
	// the TROUBLE-HUB-005 return of §5). The Redis client is untouched, the
	// group is intact, and the state machine still says `up`.
	rt.mu.Lock()
	cancelConsumer := rt.consumerCancel
	rt.mu.Unlock()
	if cancelConsumer == nil {
		t.Fatal("the boot did not register a consumer")
	}
	cancelConsumer()
	waitFor(t, 2*time.Second, "the consumer goroutine to exit", func() bool { return !registered() })

	// A runtime that says `up` with no drainer is the silent success SPEC-13 §1
	// rule 4 forbids: the supervisor must put a consumer back.
	waitFor(t, 8*time.Second, "the supervisor to restart the consumer", registered)

	sig := "sentinel:sha256v1:9f2c1d3e4b5a6c7d"
	rec, err := rt.Ingest(ctx, draftFor(types.KEvent, sig, "sentinel", map[string]any{"item_type": "event", "n": "1"}))
	if err != nil {
		t.Fatalf("ingest with the restarted consumer: %v", err)
	}
	if rec.Seq == 0 {
		t.Fatalf("no ledger identity: %+v", rec)
	}
	waitFor(t, 2*time.Second, "the restarted consumer in the ledger", func() bool { return eventRecords(ledger) == 1 })
	waitFor(t, 2*time.Second, "XPENDING back to zero", func() bool {
		return f.pelSize("trouble:ingest", "ledger-writers") == 0
	})
}
