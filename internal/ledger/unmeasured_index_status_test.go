package ledger

import (
	"context"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// TestReadStatusMarksUnbuiltIndexExplicitly proves that the read-only status
// path does not project its record walk as an empty in-memory index. The index
// is not built by ReadStatus, so populated and empty ledgers use the same
// explicit contract: measured=false with a reason.
func TestReadStatusMarksUnbuiltIndexExplicitly(t *testing.T) {
	t.Run("populated", func(t *testing.T) {
		clk := newFakeClock(testNow())
		o := baseTestOptions(t, clk)
		l, err := Open(context.Background(), o)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		mustAppend(t, l, types.RecordDraft{
			Kind:   types.KIncident,
			Sig:    "psi:sha256v1:unmeasured-index-populated",
			Inc:    types.NewID(types.PInc),
			Origin: types.Origin{HostID: "7f3a91c2d4e5b607", Source: "psi"},
			Actor:  testActor(),
			Payload: map[string]any{
				"state": "detected",
			},
		})

		live := l.Status()
		if !live.Index.Measured || live.Index.Incidents == 0 {
			t.Fatalf("live status index = %+v, want a measured populated index", live.Index)
		}
		readOnly, err := ReadStatus(l.Root())
		if err != nil {
			t.Fatalf("ReadStatus: %v", err)
		}
		assertReadOnlyIndexUnmeasured(t, readOnly)
		if readOnly.Records == 0 {
			t.Fatal("read-only status did not observe the populated ledger")
		}
	})

	t.Run("empty", func(t *testing.T) {
		readOnly, err := ReadStatus(testRoot(t))
		if err != nil {
			t.Fatalf("ReadStatus: %v", err)
		}
		assertReadOnlyIndexUnmeasured(t, readOnly)
		if readOnly.Records != 0 {
			t.Fatalf("empty read-only status records = %d, want 0", readOnly.Records)
		}
	})
}

func assertReadOnlyIndexUnmeasured(t *testing.T, st types.LedgerStatus) {
	t.Helper()
	if st.Index.Measured {
		t.Fatalf("read-only index claims measured=true: %+v", st.Index)
	}
	if st.Index.UnmeasuredReason != "read_only_path_did_not_build_index" {
		t.Fatalf("read-only index reason = %q, want read_only_path_did_not_build_index", st.Index.UnmeasuredReason)
	}
	if st.Index.Degraded {
		t.Fatalf("unmeasured index claims degraded=true: %+v", st.Index)
	}
}

// ch:trace row=TRBL-130-R2 spec=specs/SPEC-01-ledger.md#3.10b wave=none:serial test=internal/ledger/unmeasured_index_status_test.go::TestReadStatusMarksUnbuiltIndexExplicitly doc=none evidence=internal/ledger/unmeasured_index_status_test.go witness=none:ephemeral-worker-verification verdict=none:foreman-judge commit=none:foreman-integration memory=none:foreman-write
// STATUS: complete
// TIER: T1
// EVIDENCE: internal/ledger/unmeasured_index_status_test.go
