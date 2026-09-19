// batchappend_test.go — the §3.5a batch append contract (TRBL-044).
//
// SPEC-12 §7b measured that the daemon's boot pays ONE group-commit window
// per ledger record because its writers are sequential (Append waits for the
// durable ack before the next record is drafted), so a serial producer gets
// no coalescing at all. The §3.5a decision (TRBL-044) adds the batch shape:
// K records, one group commit, durable-on-return per BATCH. These tests pin
// the ledger half of that contract: one durable commit for the whole batch,
// contiguous ordered seqs, and the single-record paths untouched beside it.
package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// distinctDrafts builds n event drafts with distinct sigs (the core dedups on
// the sig, so a repeated sig would not be n records).
func distinctDrafts(n int) []types.RecordDraft {
	out := make([]types.RecordDraft, 0, n)
	for i := 0; i < n; i++ {
		sig := types.NewSig(types.SigSource("psi"), "sha256", 1, []byte(fmt.Sprintf("batch-digest-%06d", i))).String()
		out = append(out, eventDraft("psi", sig, fmt.Sprintf("%064x", i), 0))
	}
	return out
}

// TestAppendBatchCostsOneGroupCommitWindow is §3.5a's headline: a batch of K
// records is ONE durable commit, not K — while the SAME records appended one
// at a time cost K (the §7 appendcost invariant, asserted here on the same
// ledger so the contrast is measured, not assumed).
func TestAppendBatchCostsOneGroupCommitWindow(t *testing.T) {
	const (
		k      = 10
		window = 40 * time.Millisecond
	)
	clk := newFakeClock(time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC))
	l := testLedger(t, clk, mutOpts(func(o *Options) { o.Rotation.FsyncWindowMS = int(window / time.Millisecond) }))

	before := l.Status()
	start := time.Now()
	recs, err := l.AppendBatch(context.Background(), distinctDrafts(k))
	elapsed := time.Since(start)
	after := l.Status()
	if err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if len(recs) != k {
		t.Fatalf("AppendBatch returned %d records, want %d", len(recs), k)
	}
	if got := after.FsyncCalls - before.FsyncCalls; got != 1 {
		t.Fatalf("one batch of %d records produced %d durable commits, want 1: the §3.5a batch is not one group commit", k, got)
	}
	if got := after.Records - before.Records; got != k {
		t.Fatalf("one batch advanced the record count by %d, want %d", got, k)
	}
	if elapsed >= window {
		// The assertion above is the commit count, which is the structural
		// fact; the wall time is only context. Under fleet I/O the single
		// group commit itself can take longer than the whole window (the
		// device's sync latency is the §7b mechanism), so a slow landing is
		// logged, not failed.
		t.Logf("measured: batch of %d landed in %s — one commit, but slower than the bare %s window (host I/O at load)", k, elapsed.Round(time.Millisecond), window)
	} else {
		t.Logf("measured: batch of %d landed in %s (< one %s window)", k, elapsed.Round(time.Millisecond), window)
	}

	// The control arm: the same count appended sequentially costs K commits
	// (the §7 model, unchanged beside the batch path).
	before = l.Status()
	for i := 0; i < k; i++ {
		mustAppend(t, l, eventDraft("psi",
			types.NewSig(types.SigSource("psi"), "sha256", 1, []byte(fmt.Sprintf("solo-digest-%06d", i))).String(),
			fmt.Sprintf("%064x", i), 0))
	}
	after = l.Status()
	if got := after.FsyncCalls - before.FsyncCalls; got != k {
		t.Fatalf("the control arm produced %d commits for %d sequential appends, want %d: the §7 serial cost model changed", got, k, k)
	}
}

// TestAppendBatchSeqsAreContiguousAndOrdered: seq is allocated inside the
// batch in draft order (§3.5's allocator, unchanged) — no reordering, no gaps.
func TestAppendBatchSeqsAreContiguousAndOrdered(t *testing.T) {
	clk := newFakeClock(testNow())
	l := testLedger(t, clk)

	const k = 25
	recs, err := l.AppendBatch(context.Background(), distinctDrafts(k))
	if err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if len(recs) != k {
		t.Fatalf("got %d records, want %d", len(recs), k)
	}
	for i := 1; i < k; i++ {
		if recs[i].Seq != recs[i-1].Seq+1 {
			t.Fatalf("seq %d follows %d: a batch's seqs must be contiguous and in draft order", recs[i].Seq, recs[i-1].Seq)
		}
		if recs[i].RecID == recs[i-1].RecID {
			t.Fatalf("records %d and %d share one rec_id", i-1, i)
		}
	}
	// Every per-record field the single path stamps is stamped here too.
	for _, r := range recs {
		if r.Seq == 0 || r.RecID == "" || r.TS == "" || r.SchemaVersion != SchemaVersionV1 {
			t.Fatalf("batch member not fully stamped: %+v", r)
		}
	}
}

// TestAppendBatchDurableOnReturnAndQueriable: after the batch returns nil the
// records are on disk in order with a matching durable watermark.
func TestAppendBatchDurableOnReturnAndQueriable(t *testing.T) {
	clk := newFakeClock(testNow())
	l := testLedger(t, clk)
	root := l.Root()

	const k = 8
	recs, err := l.AppendBatch(context.Background(), distinctDrafts(k))
	if err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if got := l.Status().LastSeq; got != recs[k-1].Seq {
		t.Fatalf("durable watermark %d, want the batch's last seq %d (durable-on-return per batch)", got, recs[k-1].Seq)
	}
	lines := readFileLines(t, root, l.Status().File)
	if len(lines) < k {
		t.Fatalf("ledger file holds %d lines, want at least the batch's %d", len(lines), k)
	}
	// The batch's lines are the file's LAST k lines and carry the batch's seqs.
	for i := 0; i < k; i++ {
		var got types.Record
		if err := json.Unmarshal(lines[len(lines)-k+i], &got); err != nil {
			t.Fatalf("line %d is not a record: %v", len(lines)-k+i, err)
		}
		if got.Seq != recs[i].Seq {
			t.Fatalf("persisted seq %d at batch position %d, want %d (write order = seq order)", got.Seq, i, recs[i].Seq)
		}
	}
}

// TestAppendBatchEmptyAndBadDraft: an empty batch writes nothing; one bad
// draft fails the call with nothing written (the batch is refused before any
// part of it reaches the queue — never a half batch).
func TestAppendBatchEmptyAndBadDraft(t *testing.T) {
	clk := newFakeClock(testNow())
	l := testLedger(t, clk)

	before := l.Status()
	recs, err := l.AppendBatch(context.Background(), nil)
	if err != nil || recs != nil {
		t.Fatalf("AppendBatch(nil) = %v, %v; want nil, nil", recs, err)
	}
	recs, err = l.AppendBatch(context.Background(), []types.RecordDraft{})
	if err != nil || recs != nil {
		t.Fatalf("AppendBatch(empty) = %v, %v; want nil, nil", recs, err)
	}
	bad := distinctDrafts(3)
	bad = append(bad, types.RecordDraft{Kind: types.RecordKind("nonsense")}) // unknown kind → refused in prepare
	if _, err := l.AppendBatch(context.Background(), bad); err == nil {
		t.Fatal("a batch containing a bad draft must be refused, not silently trimmed")
	}
	if got := l.Status().Records - before.Records; got != 0 {
		t.Fatalf("refused batches wrote %d records, want 0 (never a half batch)", got)
	}
}

// TestAppendBatchClampedByMaxBatchRecords: a batch larger than
// ledger.max_batch_records is split into ceil(K/cap) group commits — the cap
// is a flush boundary and a batch must never span one (§3.5a, §3.5 rule 4).
func TestAppendBatchClampedByMaxBatchRecords(t *testing.T) {
	const (
		k      = 7
		cap    = 3
		window = 40 * time.Millisecond
	)
	clk := newFakeClock(time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC))
	l := testLedger(t, clk, mutOpts(func(o *Options) {
		o.Rotation.FsyncWindowMS = int(window / time.Millisecond)
		o.Rotation.MaxBatchRecords = cap
	}))

	before := l.Status()
	recs, err := l.AppendBatch(context.Background(), distinctDrafts(k))
	if err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	after := l.Status()
	if len(recs) != k {
		t.Fatalf("got %d records, want %d", len(recs), k)
	}
	wantCommits := (k + cap - 1) / cap // ceil(K/cap)
	if got := after.FsyncCalls - before.FsyncCalls; got != int64(wantCommits) {
		t.Fatalf("%d records at cap %d produced %d commits, want %d (ceil)", k, cap, got, wantCommits)
	}
	// The split is invisible to the seq space: still contiguous and ordered.
	for i := 1; i < k; i++ {
		if recs[i].Seq != recs[i-1].Seq+1 {
			t.Fatalf("seq %d follows %d across a split boundary: the clamped batches must tile one contiguous seq range", recs[i].Seq, recs[i-1].Seq)
		}
	}
}
