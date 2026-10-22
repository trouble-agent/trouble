package ledger

import (
	"context"
	"fmt"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// liveGroupDraft is the shape the dedup core writes for a live group snapshot:
// `aggregate: true` plus a counters block whose `events` counts the events that
// belong to the group. A folded retry leaves counters.events = 1 while writing
// NO event record (the retry is suppressed), which is exactly the combination
// that made `isCompactionAggregate` declare events nothing could account for
// (TRBL-116).
func liveGroupDraft(sig, digest string, events, suppressed int) types.RecordDraft {
	return types.RecordDraft{
		Kind:   types.KGroup,
		Sig:    sig,
		Origin: types.Origin{HostID: "7f3a91c2d4e5b607", Source: "journald"},
		Actor:  testActor(),
		Payload: map[string]any{
			"op":        "update",
			"aggregate": true,
			"digest":    digest,
			"title":     "unit failed",
			"counters": map[string]any{
				"events":     events,
				"suppressed": suppressed,
			},
		},
	}
}

// compactionAggregateDraft is the shape the COMPACTOR writes (compact.go): the
// record the verify walk is allowed to treat as a declaration of folded events.
func compactionAggregateDraft(sig, digest string, first, last, count int) types.RecordDraft {
	return types.RecordDraft{
		Kind:   types.KGroup,
		Sig:    sig,
		Origin: types.Origin{HostID: "7f3a91c2d4e5b607", Source: "journald"},
		Actor:  testActor(),
		Payload: map[string]any{
			"op":                "compaction_aggregate",
			"aggregate":         true,
			"digest":            digest,
			"title":             "unit failed",
			"events_aggregated": count,
			"first_seq":         first,
			"last_seq":          last,
			"agg_seqs": map[string]any{
				"first": first, "last": last, "count": count, "exceptions": []any{},
			},
			"tombstone_counts_kept": true,
			"counters": map[string]any{
				"events": count, "suppressed": 0, "redacted_values": 0,
				"dropped_events": 0, "sample_rate": 1,
			},
		},
	}
}

// TestVerifyDoesNotCountLiveGroupFoldsAsDeclared: a healthy ledger whose group
// received ordinary folded retries must verify clean. Before TRBL-116 the verify
// walks counted every live group snapshot's counters.events as a
// compaction-declared event count, so 12 folds against 1 surviving event record
// produced a tombstone mismatch and `trouble ledger verify` exited 1.
func TestVerifyDoesNotCountLiveGroupFoldsAsDeclared(t *testing.T) {
	clk := newFakeClock(testNow())
	l := testLedger(t, clk)
	sig := "journald:sha256v1:fold0116"
	digest := fmt.Sprintf("%064x", 0x1160f01d)

	// The one event that survived the fold window.
	mustAppend(t, l, eventDraft("journald", sig, digest, 0))
	// Twelve duplicate retries folded into the same group: no event records.
	for i := 0; i < 12; i++ {
		mustAppend(t, l, liveGroupDraft(sig, digest, 1, i+1))
	}

	ctx := context.Background()
	rep, err := l.Verify(ctx, "")
	if err != nil {
		t.Fatalf("Ledger.Verify: %v", err)
	}
	if !rep.OK {
		t.Fatalf("Ledger.Verify on a healthy folded ledger: OK=false, tombstone_mismatches=%d seq_holes=%v errors=%v",
			rep.TombstoneMismatches, rep.SeqHoles, rep.Errors)
	}
	if rep.TombstoneMismatches != 0 {
		t.Fatalf("Ledger.Verify: tombstone_mismatches = %d, want 0 (live folds declare nothing)", rep.TombstoneMismatches)
	}
	if len(rep.SeqHoles) != 0 || rep.PayloadHashMismatches != 0 || rep.Lines == 0 {
		t.Fatalf("Ledger.Verify: seq_holes=%v payload_hash_mismatches=%d lines=%d",
			rep.SeqHoles, rep.PayloadHashMismatches, rep.Lines)
	}

	// The CLI path (ReadVerify) must agree with the library path.
	ro, err := ReadVerify(l.Root(), "")
	if err != nil {
		t.Fatalf("ReadVerify: %v", err)
	}
	if !ro.OK || ro.TombstoneMismatches != 0 {
		t.Fatalf("ReadVerify on a healthy folded ledger: OK=%v tombstone_mismatches=%d errors=%v",
			ro.OK, ro.TombstoneMismatches, ro.Errors)
	}
}

// TestVerifyStillCatchesUndeclaredAggregateLoss is the guard against blinding
// the gate: a REAL compaction aggregate declares 8 folded events while only 1
// event record exists for the group, so the verifier must still report the loss.
func TestVerifyStillCatchesUndeclaredAggregateLoss(t *testing.T) {
	clk := newFakeClock(testNow())
	l := testLedger(t, clk)
	sig := "journald:sha256v1:agg0117"
	digest := fmt.Sprintf("%064x", 0x117a99)

	// One surviving event record for the group...
	mustAppend(t, l, eventDraft("journald", sig, digest, 0))
	// ...against a compaction aggregate that claims 8 were folded into it.
	mustAppend(t, l, compactionAggregateDraft(sig, digest, 1, 9, 8))

	ctx := context.Background()
	rep, err := l.Verify(ctx, "")
	if err != nil {
		t.Fatalf("Ledger.Verify: %v", err)
	}
	if rep.TombstoneMismatches == 0 {
		t.Fatalf("Ledger.Verify missed a real loss: declared 8 folded events, 1 event record, tombstone_mismatches=0 (OK=%v)", rep.OK)
	}
	if rep.OK {
		t.Fatalf("Ledger.Verify reported OK=true on a generation that lost 7 events")
	}

	ro, err := ReadVerify(l.Root(), "")
	if err != nil {
		t.Fatalf("ReadVerify: %v", err)
	}
	if ro.TombstoneMismatches == 0 || ro.OK {
		t.Fatalf("ReadVerify missed a real loss: OK=%v tombstone_mismatches=%d", ro.OK, ro.TombstoneMismatches)
	}
}
