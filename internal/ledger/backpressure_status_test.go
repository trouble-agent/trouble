package ledger

// backpressure_status_test.go — the RECORDED drop surface on both status paths
// (SPEC-01 §3.5b, TRBL-125).
//
// `backpressure_total` answers the in-process question ("how many producers were
// refused?"). It cannot answer the operator's question ("how much of that loss
// does the ledger actually carry?"), because an in-memory counter dies with the
// process while the evidence does not. `gap_records` + `est_lost_total` are that
// second answer, and the two status paths must agree: `trouble ledger status`
// takes the read-only path (no writer seat), while the daemon's health surface
// takes the live one. A written fold only the live writer can see would be a
// report about a process, not about the ledger.
//
// The stop() test is the shutdown half of §3.5b: a fold still open when the
// ledger closes must be flushed, not swallowed.

import (
	"context"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// TestBackpressureDropSurfaceAgreesAcrossStatusPaths pins the invariant and the
// agreement: est_lost_total equals the refusals the producers saw, the query
// surface carries the same total, and the read-only status (the CLI's path)
// reports exactly what the live writer reports.
func TestBackpressureDropSurfaceAgreesAcrossStatusPaths(t *testing.T) {
	clk := newFakeClock(testNow())
	l := testLedger(t, clk, func(o *Options) {
		o.Rotation.QueueCapRecords = 1
		o.Rotation.MaxEnqueueWait = "40ms"
	})

	unwedge := wedgeWriter(t, l.w)
	refused, drain := burstRefusals(t, l, 5, 2, "status")
	unwedge()
	drain()
	forceBoundary(t, l)

	live := l.Status()
	if got, want := live.BackpressureTotal, uint64(refused); got != want {
		t.Fatalf("live backpressure_total = %d, want %d", got, want)
	}
	if live.GapRecords != 1 {
		t.Fatalf("live gap_records = %d, want 1 (one fold for one drain window) — refusals were not recorded", live.GapRecords)
	}
	if live.EstLostTotal != int64(refused) {
		t.Fatalf("live est_lost_total = %d, want %d (the refusals that provoked the fold)", live.EstLostTotal, refused)
	}

	ro, err := ReadStatus(l.Root())
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	if ro.GapRecords != live.GapRecords || ro.EstLostTotal != live.EstLostTotal {
		t.Fatalf("read-only status disagrees with the live writer: read-only gap_records=%d est_lost_total=%d, live gap_records=%d est_lost_total=%d",
			ro.GapRecords, ro.EstLostTotal, live.GapRecords, live.EstLostTotal)
	}
	if ro.BackpressureTotal != 0 {
		t.Errorf("read-only backpressure_total = %d, want 0: it is an in-process counter and a read-only status must not invent one", ro.BackpressureTotal)
	}

	// The records themselves must carry the same total the status reports, so
	// the surface is derived from the ledger rather than from a parallel tally.
	var sum int
	for _, g := range backpressureGaps(t, l) {
		sum += g.EstLost
	}
	if int64(sum) != ro.EstLostTotal {
		t.Fatalf("sum(est_lost) over gap records = %d, status est_lost_total = %d", sum, ro.EstLostTotal)
	}
}

// TestBackpressureFoldFlushedOnStop pins §3.5b rule 3: refusals that happen in a
// drain window with no batch boundary left are still written when the ledger
// closes. Without the stop() flush the tally is silently dropped at shutdown —
// the same defect, just in a narrower window.
//
// The writer under test owns the queue but has no flush goroutine yet and no
// queued work, so the close path is the only thing that can record the fold.
func TestBackpressureFoldFlushedOnStop(t *testing.T) {
	clk := newFakeClock(testNow())
	o := baseTestOptions(t, clk)
	o.Rotation.QueueCapRecords = 1
	o.Rotation.MaxEnqueueWait = "30ms"

	l, err := Open(context.Background(), o)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = l.Close(context.Background()) })
	// Retire the opened writer so this test's writer is the only one appending
	// to the root, and install it as the ledger's seat — Append routes through
	// l.w, so this is what makes the refusals below come from the writer under
	// test. The ledger's structured state (root, options, index, recovered
	// stamp) is all a writer needs.
	if serr := l.w.stop(); serr != nil {
		t.Fatalf("stopping the opened writer: %v", serr)
	}
	l.w = newWriter(l)
	w := l.w

	// Occupy the single queue slot with a housekeeping record nothing is waiting
	// on, so every producer below meets the expiry instead of queueing.
	w.ch <- w.housePending("probe", nil)

	const refusals = 4
	for i := 0; i < refusals; i++ {
		_, aerr := l.Append(context.Background(), types.RecordDraft{
			Kind:    types.KEvent,
			Sig:     "psi:sha256v1:stopsurface0",
			Origin:  types.Origin{HostID: "7f3a91c2d4e5b607", Source: "backpressure-test"},
			Actor:   testActor(),
			Payload: map[string]any{"op": "sample", "i": i},
		})
		if aerr == nil {
			t.Fatalf("append %d was accepted, want a backpressure refusal (the queue slot is occupied)", i)
		}
		if code := CodeOf(aerr); code != types.CodeLedger001 {
			t.Fatalf("append %d refused with %s, want %s", i, code, types.CodeLedger001)
		}
	}
	if got := int(w.backpressure.Load()); got != refusals {
		t.Fatalf("backpressure tally = %d, want %d", got, refusals)
	}

	// Empty the queue so the close path is the only place the fold can be
	// written, then run the writer and close it.
	<-w.ch
	w.start()
	if serr := w.stop(); serr != nil {
		t.Fatalf("writer stop: %v", serr)
	}

	st, err := ReadStatus(l.Root())
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	if st.GapRecords != 1 {
		t.Fatalf("gap_records = %d after a close with %d refusals pending, want 1: the shutdown swallowed the fold", st.GapRecords, refusals)
	}
	if st.EstLostTotal != refusals {
		t.Fatalf("est_lost_total = %d after close, want %d", st.EstLostTotal, refusals)
	}
}
