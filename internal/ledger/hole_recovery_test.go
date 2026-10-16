package ledger

// TRBL-110: a HEAD hint above the file's highest seq seeds a real allocation
// gap at boot (SPEC-01 §3.4 rule 7 max rule: seq_alloc = max(HEAD, file)+1).
// The gap is a recovery-created hole and must reach the audit trail as one
// lifecycle{op:"hole", from, to} record with the next successful batch
// (SPEC-01 §3.5: "Every hole therefore appears in the audit trail"). The
// allocation semantics themselves stay pinned by TestSeqRecoveryFromHeadHint —
// seqs below a high HEAD are never reissued.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// countHoleRecords decodes every record of the ledger's live day file and
// returns the lifecycle{op:"hole"} records it finds (count, from, to, firstSeq).
func countHoleRecords(t *testing.T, path string) (int, uint64, uint64, uint64) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	n := 0
	var from, to, firstSeq uint64
	for _, ln := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		var r types.Record
		if err := json.Unmarshal([]byte(ln), &r); err != nil {
			continue
		}
		if r.Kind != types.KLifecycle {
			continue
		}
		if op, _ := r.Payload["op"].(string); op != "hole" {
			continue
		}
		n++
		f, _ := r.Payload["from"].(float64)
		t2, _ := r.Payload["to"].(float64)
		if n == 1 {
			from, to, firstSeq = uint64(f), uint64(t2), r.Seq
		}
	}
	return n, from, to, firstSeq
}

// TestRecoveryHighHeadRecordsHoleInTrail: file max seq 4, HEAD last_seq 628
// (the dogfood repro shape). Boot continues allocation FROM THE HINT (629) and
// the skipped range {5, 628} must be recorded in the trail by the next
// successful batch as lifecycle{op:"hole", from:5, to:628}.
func TestRecoveryHighHeadRecordsHoleInTrail(t *testing.T) {
	day := testNow()
	root := testRoot(t)
	name := day.Format(dayLayout) + ".jsonl"
	writeLines(t, root, name, [][]byte{
		magicRecord(t, 1, day), magicRecord(t, 2, day), magicRecord(t, 3, day), magicRecord(t, 4, day),
	})
	head := `{"last_seq":628,"day":"` + day.Format(dayLayout) + `","gen":0,"part":1}`
	if err := os.WriteFile(filepath.Join(root, "HEAD"), []byte(head+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	clk := newFakeClock(day)
	l := testLedgerAt(t, root, clk)

	// (a) existing semantics: the allocator continues from the hint. The trail's
	// first new records are the recovery hole + the boot notes, so the hole
	// record's own seq pins that nothing below 629 was ever allocated.
	path := filepath.Join(root, name)
	n, from, to, holeSeq := countHoleRecords(t, path)
	if holeSeq < 629 {
		t.Errorf("hole record seq = %d, want >= 629 = max(HEAD.last_seq=628, file max=4) + 1: a seq inside the gap was reused", holeSeq)
	}
	rec := mustAppend(t, l, eventDraft("psi", "psi:sha256v1:after-hint", "hh", 0))
	if rec.Seq <= 628 {
		t.Errorf("next seq = %d, want > 628: the allocator never goes backwards", rec.Seq)
	}
	if holeSeq >= rec.Seq {
		t.Errorf("hole record seq %d must land with the first batch, before the later append seq %d", holeSeq, rec.Seq)
	}

	// (b) the recovery-created gap is in the audit trail
	if n != 1 {
		t.Errorf("lifecycle{op:\"hole\"} records = %d, want exactly 1 (SPEC-01 §3.5: every hole appears in the audit trail)", n)
	}
	if from != 5 || to != 628 {
		t.Errorf("hole record payload = {from:%d, to:%d}, want {from:5, to:628} = (file max 4 + 1, HEAD.last_seq)", from, to)
	}

	// the seq_recovered boot note (health surface) stays in the trail
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "seq_recovered") {
		t.Errorf("the lifecycle{op:seq_recovered} boot note must remain in the trail")
	}
}

// TestRecoveryNoFalseHoleWhenHeadMatchesFiles: HEAD at the file's watermark is
// the healthy boot — no hole record may be fabricated.
func TestRecoveryNoFalseHoleWhenHeadMatchesFiles(t *testing.T) {
	day := testNow()
	root := testRoot(t)
	name := day.Format(dayLayout) + ".jsonl"
	writeLines(t, root, name, [][]byte{
		magicRecord(t, 1, day), magicRecord(t, 2, day), magicRecord(t, 3, day), magicRecord(t, 4, day),
	})
	head := `{"last_seq":4,"day":"` + day.Format(dayLayout) + `","gen":0,"part":1}`
	if err := os.WriteFile(filepath.Join(root, "HEAD"), []byte(head+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	clk := newFakeClock(day)
	l := testLedgerAt(t, root, clk)
	rec := mustAppend(t, l, eventDraft("psi", "psi:sha256v1:healthy", "h0", 0))
	if rec.Seq <= 4 {
		t.Errorf("next seq = %d, want > 4 = max(HEAD.last_seq=4, file max=4)", rec.Seq)
	}
	if n, _, _, _ := countHoleRecords(t, filepath.Join(root, name)); n != 0 {
		t.Errorf("lifecycle{op:\"hole\"} records = %d, want 0: a healthy boot fabricates no hole", n)
	}
	if _, err := l.Verify(context.Background(), day.Format(dayLayout)); err != nil {
		t.Fatal(err)
	}
}
