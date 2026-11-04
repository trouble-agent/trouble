package ledger

// backpressure_gap_test.go — refusals at the write boundary must be RECORDED
// (SPEC-01 §3.5b, TRBL-125).
//
// The defect this pins: `enqueue` / `enqueueBatch` refused a producer with
// TROUBLE-LEDGER-001 reason=backpressure while writing no record at all, so an
// operator reading the ledger could not tell that data had been lost. The fix
// tallies refusals on the writer (no re-entry into the refusing queue) and folds
// them into ONE `gap{cause:"backpressure"}` record per drain window from the
// flush goroutine.
//
// The tests below provoke REAL refusals — a capped queue plus an expired
// `max_enqueue_wait` — and then assert the ledger's own accounting:
//
//	status.backpressure_total == refusals observed by the producers
//	sum(gap.est_lost)         == those same refusals
//
// Nothing here fabricates the record: the only seam used is the writer's own
// flush goroutine, parked by handing it a rotate reply nobody is waiting for.
// That is exactly what a stalled writer looks like to a producer (the queue
// fills up and stops draining), so the refusals come out of the shipping code
// path.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// wedgeWriter parks the writer's flush goroutine inside a rotate-reply send and
// returns the unwedge closure. Once parked the writer takes nothing off the
// queue, so a producer meets the real `max_enqueue_wait` expiry.
//
// Determinism: rotate requests are served in FIFO order, so the first request —
// whose reply channel nobody reads — parks the goroutine, and the probe that
// follows can never be served while it is parked. That is the signal we wait
// for. Load can only make the wait longer; it cannot turn a parked writer into
// an un-parked one.
func wedgeWriter(t *testing.T, w *writer) func() {
	t.Helper()
	sentinel := make(chan rotateReply) // unbuffered and never read: the reply send parks the goroutine
	w.rotateReq <- sentinel

	probe := make(chan rotateReply, 1)
	deadline := time.Now().Add(30 * time.Second)
	for {
		w.rotateReq <- probe
		time.Sleep(10 * time.Millisecond)
		if len(w.rotateReq) > 0 {
			// Parked on the sentinel's reply: our probe is still queued
			// behind it and cannot have been served.
			return func() { <-sentinel } // completes the parked reply send
		}
		// Not parked yet: the probe was served, so take its reply and retry.
		select {
		case <-probe:
		case <-time.After(time.Second):
			t.Fatalf("flush goroutine serviced a rotate request but never answered it")
		}
		if time.Now().After(deadline) {
			t.Fatalf("could not park the flush goroutine within 30s")
		}
	}
}

// burstRefusals drives producers at the capped queue until at least want of them
// have been refused, and returns how many were refused plus a drain closure.
//
// Two properties matter:
//   - every refusal must carry the shipping refusal code, or the test provoked a
//     different failure and proves nothing;
//   - the refusal count must be FINAL before the caller unwedges: with the flush
//     goroutine parked, exactly one producer can be in flight — it won the single
//     queue slot and is blocked on its durable ack — so waiting until every other
//     producer has returned fixes the count. drain() then waits for that last
//     producer AFTER the unwedge, when its ack can finally land.
func burstRefusals(t *testing.T, l *Ledger, n, want int, tag string) (refused int, drain func()) {
	t.Helper()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	accepted := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := l.Append(context.Background(), types.RecordDraft{
				Kind:   types.KEvent,
				Sig:    fmt.Sprintf("psi:sha256v1:%s%012d", tag, i),
				Origin: types.Origin{HostID: "7f3a91c2d4e5b607", Source: "backpressure-test"},
				Actor:  testActor(),
				Payload: map[string]any{
					"op": "sample", "burst": tag, "i": i,
				},
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			accepted++
		}(i)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		mu.Lock()
		refused = len(errs)
		settled := len(errs) + accepted
		mu.Unlock()
		if refused >= want && settled >= n-1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("burst %s: %d refusal(s), %d settled of %d attempt(s) after 60s (want at least %d refusals and all but the queued producer settled): the queue was never saturated, so the test proves nothing",
				tag, refused, settled, n, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	for _, err := range errs {
		if code := CodeOf(err); code != types.CodeLedger001 {
			t.Errorf("burst %s refusal carried %s, want %s: %v", tag, code, types.CodeLedger001, err)
			continue
		}
		if !strings.Contains(err.Error(), ReasonBackpressure) {
			t.Errorf("burst %s refusal is not backpressure: %v", tag, err)
		}
	}
	mu.Unlock()
	return refused, func() {
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatalf("burst %s producers did not finish after the writer was unwedged", tag)
		}
	}
}

// forceBoundary makes the writer cross a batch boundary and returns after the
// housekeeping batch it produces is durable. It goes through the rotate channel
// — never through the queue — so it cannot itself be refused and cannot perturb
// the refusal tally.
func forceBoundary(t *testing.T, l *Ledger) {
	t.Helper()
	if _, err := l.w.requestRotate(time.Now()); err != nil {
		t.Fatalf("requestRotate: %v", err)
	}
}

// backpressureGaps returns the recorded refusal folds, straight from the ledger
// query surface (the same surface ladder.Verify consumes).
func backpressureGaps(t *testing.T, l *Ledger) []types.GapRecord {
	t.Helper()
	gaps, _, err := l.Query().Gaps("", 100)
	if err != nil {
		t.Fatalf("Query().Gaps: %v", err)
	}
	var out []types.GapRecord
	for _, g := range gaps {
		if g.Cause == ReasonBackpressure {
			out = append(out, g)
		}
	}
	return out
}

// recordsByKind returns the ledger's records of one kind, read back from the day
// files — a decode path independent of the writer's own counters.
func recordsByKind(t *testing.T, l *Ledger, kind types.RecordKind) []types.Record {
	t.Helper()
	files, _, err := authoritativeFilesRoot(l.Root())
	if err != nil {
		t.Fatalf("authoritativeFilesRoot: %v", err)
	}
	var out []types.Record
	for _, p := range files {
		for _, line := range readFileLines(t, l.Root(), p.File) {
			rec, derr := decodeRecord(line, true)
			if derr != nil || rec == nil {
				continue
			}
			if rec.Kind == kind {
				out = append(out, *rec)
			}
		}
	}
	return out
}

// TestBackpressureRefusalsAreRecordedAsGap is the regression test for TRBL-125:
// two saturated bursts on one ledger must produce two folded gap records whose
// est_lost sum equals the refusals the producers saw. Against the pre-fix writer
// it fails on the first assertion — zero records for real refusals.
//
// It deliberately uses only the pre-existing API surface (Append, Status,
// Query().Gaps), so it also COMPILES against the pre-fix tree. That is what makes
// it the mutation proof.
func TestBackpressureRefusalsAreRecordedAsGap(t *testing.T) {
	clk := newFakeClock(testNow())
	l := testLedger(t, clk, func(o *Options) {
		// A one-record queue makes saturation deterministic: with the flush
		// goroutine parked, exactly one producer can be queued at a time.
		o.Rotation.QueueCapRecords = 1
		o.Rotation.MaxEnqueueWait = "40ms"
	})

	const perBurst = 6 // one queues, the rest meet the expiry
	var totalRefused int

	// ---- burst 1: saturate, unwedge, cross a boundary.
	unwedge := wedgeWriter(t, l.w)
	refused1, drain1 := burstRefusals(t, l, perBurst, 3, "b1")
	unwedge()
	drain1()
	if got, want := refusalCount(t, l.Status()), uint64(refused1); got != want {
		t.Fatalf("backpressure_total = %d, want %d (one per refused call)", got, want)
	}
	totalRefused += refused1
	forceBoundary(t, l)

	// ---- burst 2: a second drain window folds separately, never into the first.
	unwedge = wedgeWriter(t, l.w)
	refused2, drain2 := burstRefusals(t, l, perBurst, 3, "b2")
	unwedge()
	drain2()
	totalRefused += refused2
	forceBoundary(t, l)

	gaps := backpressureGaps(t, l)
	if len(gaps) == 0 {
		t.Fatalf("0 backpressure gap record(s) after %d refusal(s): the ledger lost data and recorded nothing (TRBL-125)", totalRefused)
	}
	var estLost int
	for _, g := range gaps {
		if g.Scope != "ledger:queue" {
			t.Errorf("gap %s scope = %q, want ledger:queue", g.ID, g.Scope)
		}
		if g.FromTS == "" || g.ToTS == "" {
			t.Errorf("gap %s has an empty window: from_ts=%q to_ts=%q", g.ID, g.FromTS, g.ToTS)
		}
		if g.ToTS < g.FromTS {
			t.Errorf("gap %s window is inverted: from_ts=%s to_ts=%s", g.ID, g.FromTS, g.ToTS)
		}
		if g.EstLost <= 0 {
			t.Errorf("gap %s est_lost = %d, want the folded refusal count (>0)", g.ID, g.EstLost)
		}
		estLost += g.EstLost
	}
	if estLost != totalRefused {
		t.Fatalf("sum(est_lost) = %d across %d backpressure gap record(s), want %d (the refusals that provoked them)",
			estLost, len(gaps), totalRefused)
	}
	if len(gaps) != 2 {
		t.Errorf("%d backpressure gap record(s) for 2 drain windows, want 2 (one fold per window, never one per refusal)", len(gaps))
	}
	// The live refusal counter must agree with the records the query returns.
	// (The recorded-surface fields of status are pinned separately, in
	// backpressure_status_test.go.)
	st := l.Status()
	if want := uint64(totalRefused); refusalCount(t, st) != want {
		t.Errorf("backpressure_total = %d, want %d", refusalCount(t, st), want)
	}
	// The fold carries the diagnostic an operator needs: the queue cap and the
	// wait that expired, in the record itself.
	recs := recordsByKind(t, l, types.KGap)
	if len(recs) == 0 {
		t.Fatalf("no gap record on disk")
	}
	detail, _ := recs[0].Payload["cause_detail"].(string)
	if !strings.Contains(detail, "queue cap 1") || !strings.Contains(detail, "40ms") {
		t.Errorf("cause_detail = %q, want it to name the queue cap and max_enqueue_wait", detail)
	}
}
