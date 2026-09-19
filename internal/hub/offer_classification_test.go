package hub

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// offer_classification_test.go is TRBL-032. §4.3's rows are statements about
// REDIS ("Redis lost at runtime" → 429/503, remembers the code 004, moves the
// state machine, and — now that the recovery supervisor exists — buys a rewire).
// A failed door offer is not automatically that fact.
//
// The incident this file pins down, measured live by a previous tick: a POST that
// arrived during the boot window was served with an already-dead request context.
// The entry was already in the stream — the wait for the consumer's append is
// where the context died — so the door answered `context.Canceled` and the serve
// path recorded a Redis failure that never happened:
//
//	hub: redis lost at runtime (): context canceled
//	{"op":"redis_lost","error_code":"","reason":""}
//
// A record naming a Redis loss with no code and no reason. Two harms, not one:
// the bogus ledger row, and — because the loss signal tears the consumer down —
// a healthy consumer killed mid-append (stranding the entry the sender had
// already enqueued, until claim_min_idle) plus a spurious rewire.
//
// The tests below separate the three things a failed offer can mean (the
// sender's request ended / the draft was refused on its own content / the queue
// itself failed) and assert which one may move the state machine.

// ---- helpers ---------------------------------------------------------------

// lifecycleRecords returns the payload of every lifecycle record for one op.
func lifecycleRecords(l *fakeLedger, op string) []map[string]any {
	var out []map[string]any
	for _, r := range l.Records() {
		if r.Kind != types.KLifecycle {
			continue
		}
		if got, _ := r.Payload["op"].(string); got == op {
			out = append(out, r.Payload)
		}
	}
	return out
}

// findLog returns the first captured log line containing substr ("" when none
// does) — the daemon's own words about what it decided.
func findLog(lines []string, substr string) string {
	for _, l := range lines {
		if strings.Contains(l, substr) {
			return l
		}
	}
	return ""
}

func logLines(lines []string) string {
	return strings.Join(lines, " | ")
}

// logSink captures the runtime's diagnostics (RuntimeConfig.Log).
type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (s *logSink) log(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, fmt.Sprintf(format, args...))
}

func (s *logSink) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lines...)
}

func (s *logSink) find(substr string) string { return findLog(s.all(), substr) }

// ---- the classification itself ---------------------------------------------

// TestOfferFailureClassification pins the predicate table the serve path and the
// supervisor both decide on. Every row is a shape a real door/probe error takes,
// including the two traps: a deadline is only the caller's when the caller's own
// context says so, and an error nobody can attribute is never a queue failure.
func TestOfferFailureClassification(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()

	queueErr := errWrap(types.CodeHub004, ReasonXAdd, "XADD trouble:ingest failed",
		errors.New("dial tcp 127.0.0.1:6379: connect: connection refused"))
	authErr := errWrap(types.CodeHub002, ReasonRedisAuth, "redis refused the connection", errors.New("WRONGPASS"))
	refused := errWrap(types.CodeHub007, ReasonDecode, "payload is not canonical JSON", errors.New("json: unsupported value: NaN"))
	noGroup := errors.New("NOGROUP No such key or consumer group")
	unattributable := errors.New("something nobody can attribute to redis")

	cases := []struct {
		name       string
		ctx        context.Context
		err        error
		requestEnd bool
		queueFail  bool
	}{
		{"nil", context.Background(), nil, false, false},
		{"the request context was cancelled", expired, canceled.Err(), true, false},
		{"the request died mid-offer", context.Background(), fmt.Errorf("offer: %w", context.Canceled), true, false},
		{"the request ran out of time", expired, expired.Err(), true, false},
		{"a deadline with a live request context", context.Background(), context.DeadlineExceeded, false, false},
		{"a cancellation wrapped in a hub error", context.Background(), errWrap(types.CodeHub004, ReasonXAdd, "XADD failed", context.Canceled), true, false},
		{"a redis transport failure", context.Background(), queueErr, false, true},
		{"a redis auth refusal", context.Background(), authErr, false, true},
		{"a NOGROUP reply", context.Background(), noGroup, false, true},
		{"a draft refused on its own content", context.Background(), refused, false, false},
		{"an unattributable error", context.Background(), unattributable, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRequestEnd(tc.ctx, tc.err); got != tc.requestEnd {
				t.Errorf("IsRequestEnd = %v want %v", got, tc.requestEnd)
			}
			if got := IsQueueFailure(tc.err); got != tc.queueFail {
				t.Errorf("IsQueueFailure = %v want %v", got, tc.queueFail)
			}
		})
	}
}

// ---- the incident ----------------------------------------------------------

// TestCancelledRequestIsNotARedisLoss is the ticket's case, end to end: the
// sender disconnects while its offer waits for the consumer's append.
func TestCancelledRequestIsNotARedisLoss(t *testing.T) {
	f := newFakeStreams()
	f.info = hubInfoOK()
	f.block = 5 * time.Millisecond
	events := &eventLog{}
	ledger := newFakeLedger(events)
	// The ledger holds every append longer than the cancellation that follows, so
	// the sender's offer is unambiguously IN FLIGHT when its context dies: the
	// entry is already in the stream and the consumer is appending it. That is the
	// boot-window shape the ticket measured, made deterministic.
	ledger.delay = 700 * time.Millisecond
	logs := &logSink{}
	cfg := runtimeCfg(t, f, ledger, newFakeLedger(events), func(c *RuntimeConfig) {
		c.Redis.DoorWait = 3 * time.Second // the door's own wait must not be the thing that answers
		c.Log = logs.log
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rt.Close()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// A live queue, one warm-up event, and the consumer that owns it.
	sig := "sentinel:sha256v1:9f2c1d3e4b5a6c7d"
	if _, err := rt.Ingest(ctx, draftFor(types.KEvent, sig, "sentinel", map[string]any{"item_type": "event", "n": "1"})); err != nil {
		t.Fatalf("warm-up ingest: %v", err)
	}
	waitFor(t, 3*time.Second, "the warm-up event in the ledger", func() bool { return eventRecords(ledger) == 1 })
	stale := rt.Consumer()
	if stale == nil {
		t.Fatal("no consumer after a clean boot")
	}

	// The sender's request dies while its offer is in flight.
	reqCtx, reqCancel := context.WithCancel(ctx)
	go func() {
		time.Sleep(50 * time.Millisecond)
		reqCancel()
	}()
	_, err = rt.Ingest(reqCtx, draftFor(types.KEvent, sig, "sentinel", map[string]any{"item_type": "event", "n": "2"}))
	reqCancel()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("the door answered %v want context.Canceled: the test is not exercising the mid-offer disconnect", err)
	}
	// Premise: the offer REACHED the stream, so the cancellation happened during
	// the wait for the consumer's append (not before the XADD).
	if got := len(f.entries("trouble:ingest")); got != 2 {
		t.Fatalf("stream entries = %d want 2 (the disconnecting sender's offer never reached the stream)", got)
	}

	// Let any loss-signal-driven rewire happen before the negative assertions.
	time.Sleep(500 * time.Millisecond)

	if recs := lifecycleRecords(ledger, "redis_lost"); len(recs) != 0 {
		t.Fatalf("redis_lost records = %d want 0: a sender that disconnected is not a redis loss (%+v)", len(recs), recs)
	}
	if n := lifecycleOps(ledger, "redis_restored"); n != 0 {
		t.Fatalf("redis_restored records = %d want 0: the queue was never lost (%s)", n, logLines(logs.all()))
	}
	if now := rt.Consumer(); now != stale {
		t.Fatalf("the consumer was replaced (%p → %p): a cancelled request bought a rewire (%s)", stale, now, logLines(logs.all()))
	}
	if mode := rt.RuntimeMode(); mode != ModeUp {
		t.Fatalf("mode = %q want up: the queue was never lost (%s)", mode, logLines(logs.all()))
	}
	if st := rt.Status(ctx); st.Degraded || st.DegradedReason != DegradedNone {
		t.Fatalf("status = degraded=%v reason=%q want a live queue", st.Degraded, st.DegradedReason)
	}
	f.mu.Lock()
	creates := f.groupCreateCalled
	f.mu.Unlock()
	if creates != 1 {
		t.Fatalf("XGROUP CREATE ran %d time(s) want 1: the disconnecting sender re-created the group", creates)
	}
	if line := logs.find("redis lost"); line != "" {
		t.Fatalf("the daemon logged a redis loss for a sender that disconnected: %q", line)
	}
	if line := logs.find("request ended"); line == "" {
		t.Fatalf("the classification was not logged: %s", logLines(logs.all()))
	}

	// The entry the disconnected sender had already enqueued is still drained:
	// nothing was torn down, so nothing is stranded for claim_min_idle.
	waitFor(t, 5*time.Second, "the disconnecting sender's event in the ledger", func() bool { return eventRecords(ledger) == 2 })
	waitFor(t, 5*time.Second, "XPENDING back to zero", func() bool { return f.pelSize("trouble:ingest", "ledger-writers") == 0 })
}

// TestRefusedDraftIsNotARedisLoss is the same classification seen from the other
// end: the door also refuses a draft on its own content (TROUBLE-HUB-007, the
// payload is not canonical JSON). That is a fact about the REQUEST, and it must
// not degrade a queue that is answering.
func TestRefusedDraftIsNotARedisLoss(t *testing.T) {
	f := newFakeStreams()
	f.info = hubInfoOK()
	f.block = 5 * time.Millisecond
	events := &eventLog{}
	ledger := newFakeLedger(events)
	logs := &logSink{}
	cfg := runtimeCfg(t, f, ledger, newFakeLedger(events), func(c *RuntimeConfig) {
		c.Log = logs.log
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rt.Close()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	stale := rt.Consumer()

	_, err = rt.Ingest(ctx, draftFor(types.KEvent, "sentinel:sha256v1:9f2c1d3e4b5a6c7d", "sentinel", map[string]any{"bad": math.NaN()}))
	// Premise: the refusal is the door's DECODE-class refusal, not a queue
	// failure — asserted on the code/reason, not on the message text.
	if CodeOf(err) != types.CodeHub007 || ReasonOf(err) != ReasonDecode {
		t.Fatalf("refusal = %q/%q (%v) want TROUBLE-HUB-007/decode", CodeOf(err), ReasonOf(err), err)
	}
	if got := len(f.entries("trouble:ingest")); got != 0 {
		t.Fatalf("stream entries = %d want 0: a refused draft must not be enqueued", got)
	}

	time.Sleep(200 * time.Millisecond)

	if recs := lifecycleRecords(ledger, "redis_lost"); len(recs) != 0 {
		t.Fatalf("redis_lost records = %d want 0: a refused draft is not a redis loss (%+v)", len(recs), recs)
	}
	if now := rt.Consumer(); now != stale {
		t.Fatalf("the consumer was replaced (%p → %p): a refused draft bought a rewire", stale, now)
	}
	if mode := rt.RuntimeMode(); mode != ModeUp {
		t.Fatalf("mode = %q want up: a refused draft must not degrade the queue", mode)
	}
	if st := rt.Status(ctx); st.Degraded {
		t.Fatalf("status = degraded (%q) after a refused draft", st.DegradedReason)
	}
	if line := logs.find("redis lost"); line != "" {
		t.Fatalf("the daemon logged a redis loss for a refused draft: %q", line)
	}
}

// TestTransportCancellationIsNotARedisLoss is the same classification where it is
// easiest to get wrong: the transport call itself was aborted because a context
// was cancelled, and the door wraps that in a genuine TROUBLE-HUB-004 (the code
// §4.3 names for a lost queue). Classifying on the CODE alone would still call
// this a Redis loss; the error is only attributable once the chain is asked, and
// the answer is "a context was cancelled", not "Redis failed".
func TestTransportCancellationIsNotARedisLoss(t *testing.T) {
	f := newFakeStreams()
	f.info = hubInfoOK()
	f.block = 5 * time.Millisecond
	events := &eventLog{}
	ledger := newFakeLedger(events)
	logs := &logSink{}
	cfg := runtimeCfg(t, f, ledger, newFakeLedger(events), func(c *RuntimeConfig) {
		c.Log = logs.log
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rt.Close()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	stale := rt.Consumer()

	f.mu.Lock()
	f.failXAdd = context.Canceled
	f.mu.Unlock()

	_, err = rt.Ingest(ctx, draftFor(types.KEvent, "sentinel:sha256v1:9f2c1d3e4b5a6c7d", "sentinel", map[string]any{"item_type": "event", "n": "1"}))
	// Premise: the error LOOKS like a lost queue (code 004, the one §4.3 names)
	// while the chain says a context was cancelled — the case a code-only
	// classification gets wrong.
	if CodeOf(err) != types.CodeHub004 || !errors.Is(err, context.Canceled) {
		t.Fatalf("offer error = %q/%v want a TROUBLE-HUB-004 wrapping context.Canceled", CodeOf(err), err)
	}

	time.Sleep(200 * time.Millisecond)

	if recs := lifecycleRecords(ledger, "redis_lost"); len(recs) != 0 {
		t.Fatalf("redis_lost records = %d want 0: a cancelled context is not a redis loss (%+v)", len(recs), recs)
	}
	if now := rt.Consumer(); now != stale {
		t.Fatalf("the consumer was replaced (%p → %p): a cancelled context bought a rewire", stale, now)
	}
	if mode := rt.RuntimeMode(); mode != ModeUp {
		t.Fatalf("mode = %q want up: a cancelled context must not degrade the queue", mode)
	}
	if line := logs.find("redis lost"); line != "" {
		t.Fatalf("the daemon logged a redis loss for a cancelled context: %q", line)
	}
	if line := logs.find("request ended"); line == "" {
		t.Fatalf("the classification was not logged: %s", logLines(logs.all()))
	}
}

// TestGenuineRedisFailureIsStillARedisLoss is the other half of the ticket: the
// fix must not swallow real losses. Redis refuses the write AND the rewire cannot
// reach it (the failover shape of §4.3), so the daemon stays degraded
// deterministically — then Redis returns and ONLY the supervisor rewires it.
func TestGenuineRedisFailureIsStillARedisLoss(t *testing.T) {
	f := newFakeStreams()
	f.info = hubInfoOK()
	f.block = 5 * time.Millisecond
	events := &eventLog{}
	ledger := newFakeLedger(events)
	logs := &logSink{}
	cfg := runtimeCfg(t, f, ledger, newFakeLedger(events), func(c *RuntimeConfig) {
		c.Redis.DoorWait = 200 * time.Millisecond
		c.Log = logs.log
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rt.Close()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	sig := "sentinel:sha256v1:9f2c1d3e4b5a6c7d"
	if _, err := rt.Ingest(ctx, draftFor(types.KEvent, sig, "sentinel", map[string]any{"item_type": "event", "n": "1"})); err != nil {
		t.Fatalf("warm-up ingest: %v", err)
	}
	waitFor(t, 3*time.Second, "the warm-up event in the ledger", func() bool { return eventRecords(ledger) == 1 })
	stale := rt.Consumer()

	// Redis refuses the write; the rewire cannot reach it either.
	f.mu.Lock()
	f.failXAdd = errors.New("READONLY You can't write against a read only replica")
	f.pingErr = errors.New("dial tcp 127.0.0.1:6379: connect: connection refused")
	f.mu.Unlock()

	if _, err := rt.Ingest(ctx, draftFor(types.KEvent, sig, "sentinel", map[string]any{"item_type": "event", "n": "2"})); CodeOf(err) != types.CodeHub004 {
		t.Fatalf("code = %q want TROUBLE-HUB-004 (%v)", CodeOf(err), err)
	}
	if n := lifecycleOps(ledger, "redis_lost"); n != 1 {
		t.Fatalf("redis_lost records = %d want exactly 1 (%+v)", n, lifecycleRecords(ledger, "redis_lost"))
	}
	recs := lifecycleRecords(ledger, "redis_lost")
	code, _ := recs[0]["error_code"].(string)
	reason, _ := recs[0]["reason"].(string)
	if code == "" || reason == "" {
		t.Fatalf("the loss record names a redis loss with no code and no reason: %+v", recs[0])
	}
	if code != string(types.CodeHub004) || reason != DegradedRedisDown {
		t.Fatalf("loss record = %+v want TROUBLE-HUB-004/%s", recs[0], DegradedRedisDown)
	}
	if mode := rt.RuntimeMode(); mode != ModeUnavailable {
		t.Fatalf("mode = %q want unavailable (require_redis=false)", mode)
	}
	if st := rt.Status(ctx); !st.Degraded || st.DegradedReason != DegradedRedisDown {
		t.Fatalf("status = degraded=%v reason=%q want degraded/%s", st.Degraded, st.DegradedReason, DegradedRedisDown)
	}
	if line := logs.find("redis lost"); line == "" {
		t.Fatalf("the loss was not logged: %s", logLines(logs.all()))
	}

	// Redis returns. NOTHING here rewires: the supervisor the runtime started for
	// itself is the only thing that may.
	f.comeBack()
	waitFor(t, 8*time.Second, "the supervisor to rewire the queue", func() bool { return rt.Consumer() != stale })
	if n := lifecycleOps(ledger, "redis_restored"); n != 1 {
		t.Fatalf("redis_restored records = %d want exactly 1", n)
	}
	if mode := rt.RuntimeMode(); mode != ModeUp {
		t.Fatalf("mode after the rewire = %q want up", mode)
	}
	rec, err := rt.Ingest(ctx, draftFor(types.KEvent, sig, "sentinel", map[string]any{"item_type": "event", "n": "3"}))
	if err != nil {
		t.Fatalf("ingest after the rewire: %v", err)
	}
	if rec.Seq == 0 {
		t.Fatalf("no ledger identity after the rewire: %+v", rec)
	}
	waitFor(t, 3*time.Second, "the event after the rewire in the ledger", func() bool { return eventRecords(ledger) == 2 })
}

// TestMidAckFailureStrandsNothingAndWritesNoFalseLoss is the stranded-entry path
// the ticket asks to be considered, live: the entry reaches the ledger (so the
// sender's 200 is honest) but the XACK is refused. That is a real mid-ack failure,
// and the state it leaves must stay truthful — the event record IS durable, the
// entry IS pending (not acked, not lost), no `redis_lost` is written for it, the
// consumer is not torn down, and the re-delivery appends nothing a second time.
func TestMidAckFailureStrandsNothingAndWritesNoFalseLoss(t *testing.T) {
	f := newFakeStreams()
	f.info = hubInfoOK()
	f.block = 5 * time.Millisecond
	events := &eventLog{}
	ledger := newFakeLedger(events)
	logs := &logSink{}
	cfg := runtimeCfg(t, f, ledger, newFakeLedger(events), func(c *RuntimeConfig) {
		c.Redis.DoorWait = 2 * time.Second
		c.Log = logs.log
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rt.Close()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	stale := rt.Consumer()

	f.mu.Lock()
	f.failXAck = errors.New("READONLY You can't write against a read only replica")
	f.mu.Unlock()

	// The sender is answered: the record reached the ledger BEFORE the ack was
	// attempted (that ordering is the ack-after-fsync rule of §3.3).
	if _, err := rt.Ingest(ctx, draftFor(types.KEvent, "sentinel:sha256v1:9f2c1d3e4b5a6c7d", "sentinel", map[string]any{"item_type": "event", "n": "1"})); err != nil {
		t.Fatalf("ingest while XACK is refused: %v", err)
	}
	waitFor(t, 3*time.Second, "the durable event record", func() bool { return eventRecords(ledger) == 1 })
	waitFor(t, 3*time.Second, "the un-acked entry to show up as pending", func() bool {
		return f.pelSize("trouble:ingest", "ledger-writers") == 1
	})
	if logs.find("batch not acked") == "" {
		t.Fatalf("the un-acked batch was not logged: %s", logLines(logs.all()))
	}
	if recs := lifecycleRecords(ledger, "redis_lost"); len(recs) != 0 {
		t.Fatalf("redis_lost records = %d want 0: an un-acked batch is not a lost queue (%+v)", len(recs), recs)
	}
	if mode := rt.RuntimeMode(); mode != ModeUp {
		t.Fatalf("mode = %q want up: the ledger is fine", mode)
	}
	if now := rt.Consumer(); now != stale {
		t.Fatalf("the consumer was replaced (%p → %p) for an un-acked batch", stale, now)
	}

	// The ack path recovers: the entry is re-delivered, the gate says its records
	// are already appended, and the ledger is untouched.
	f.mu.Lock()
	f.failXAck = nil
	f.mu.Unlock()
	waitFor(t, 5*time.Second, "XPENDING back to zero", func() bool {
		return f.pelSize("trouble:ingest", "ledger-writers") == 0
	})
	if n := eventRecords(ledger); n != 1 {
		t.Fatalf("event records = %d want 1: the re-delivery appended a duplicate", n)
	}
}

// TestMarkLostRefusesAnUnnameableLoss pins the invariant the ticket asks for
// third: a `redis_lost` record with an empty `error_code` and an empty `reason`
// names no failure at all, so it is never written — the daemon logs what it
// refused to claim and leaves the queue as it is (the supervisor's liveness probe
// is what decides those cases, with its own attributable error).
//
// Three shapes are driven straight at the funnel, because the door cannot produce
// them: an error nobody can attribute, the caller's own cancellation, and a
// code-less Redis reply (a bare NOAUTH — every Redis call in this package maps
// that to code 002, so what reaches here unclassified is logged, not recorded).
func TestMarkLostRefusesAnUnnameableLoss(t *testing.T) {
	f := newFakeStreams()
	f.info = hubInfoOK()
	f.block = 5 * time.Millisecond
	events := &eventLog{}
	ledger := newFakeLedger(events)
	logs := &logSink{}
	cfg := runtimeCfg(t, f, ledger, newFakeLedger(events), func(c *RuntimeConfig) {
		c.Log = logs.log
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rt.Close()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	stale := rt.Consumer()

	rt.markLost(ctx, errors.New("something nobody can attribute to redis"))
	rt.markLost(ctx, context.Canceled)
	rt.markLost(ctx, errors.New("NOAUTH Authentication required."))

	time.Sleep(200 * time.Millisecond)

	if recs := lifecycleRecords(ledger, "redis_lost"); len(recs) != 0 {
		t.Fatalf("redis_lost records = %d want 0: %+v", len(recs), recs)
	}
	if mode := rt.RuntimeMode(); mode != ModeUp {
		t.Fatalf("mode = %q want up: no classified loss ever happened", mode)
	}
	if now := rt.Consumer(); now != stale {
		t.Fatalf("the consumer was replaced (%p → %p) for an unnameable error", stale, now)
	}
	if line := logs.find("redis lost"); line != "" {
		t.Fatalf("the daemon logged a redis loss it could not name: %q", line)
	}
	for _, want := range []string{"not the queue", "request ended", "unattributable"} {
		if logs.find(want) == "" {
			t.Fatalf("%q was not logged: %s", want, logLines(logs.all()))
		}
	}
}
