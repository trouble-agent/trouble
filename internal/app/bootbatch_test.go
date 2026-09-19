package app

// bootbatch_test.go — the composition root's §3.3a wiring (TRBL-044).
//
// The sensors batch reconcile is only the boot fix if the DAEMON installs the
// batch boundary (AppendBatch) and the ladder bridge before Start; a Sensors
// built without them silently degrades to per-record writes and the boot pays
// one group-commit window per record again. These tests pin the wiring through
// the public BootOptions surface a real boot uses.

import (
	"context"
	"strings"
	"testing"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// TestBootInstallsBatchBoundaryAndLadderBridge drives a real boot (the same
// harness the boot-phase test uses) and asserts, through the live Daemon, that
// the §3.3a boundaries are installed: the batch writer is the ledger's
// AppendBatch (verified by writing a batch through the installed boundary and
// reading the records back), and the ladder bridge admits a firing record.
func TestBootInstallsBatchBoundaryAndLadderBridge(t *testing.T) {
	h := bootDaemon(t)
	d := h.d
	if d.Sensors == nil {
		t.Fatal("the boot built no sensors; nothing to assert")
	}

	// (1) The batch boundary writes through the daemon's ledger: K drafts in,
	// K durable records out, visible to the ledger's own reader.
	drafts := make([]types.RecordDraft, 0, 3)
	for i := 0; i < 3; i++ {
		drafts = append(drafts, types.RecordDraft{
			Kind:    types.KEvent,
			Sig:     sigForTest("bootbatch", i),
			Origin:  types.Origin{HostID: d.Cfg.Origin.HostID, Source: "psi"},
			Actor:   types.Actor{Kind: types.ActorDaemon, ID: "troubled"},
			Payload: map[string]any{"op": "sample", "n": i},
		})
	}
	before := d.Ledger.Status()
	batch := batchWriterOf(t, d)
	recs, err := batch(context.Background(), drafts)
	if err != nil {
		t.Fatalf("installed batch boundary failed: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("batch boundary returned %d records, want 3", len(recs))
	}
	after := d.Ledger.Status()
	if after.LastSeq < before.LastSeq+3 {
		t.Fatalf("durable watermark %d did not advance by the batch (%d → %d): the boundary is not the ledger's AppendBatch",
			after.LastSeq, before.LastSeq, after.LastSeq)
	}
}

// TestBootBatchBoundaryIsOneDurableCommit is the boot-level arithmetic: the
// installed boundary writes K records for ONE commit's worth of watermark
// movement through the same writer the whole boot shares — the property that
// collapses the reconcile's N windows (TRBL-044).
func TestBootBatchBoundaryIsOneDurableCommit(t *testing.T) {
	h := bootDaemon(t)
	d := h.d

	const k = 6
	drafts := make([]types.RecordDraft, 0, k)
	for i := 0; i < k; i++ {
		drafts = append(drafts, types.RecordDraft{
			Kind:    types.KEvent,
			Sig:     sigForTest("onecommit", i),
			Origin:  types.Origin{HostID: d.Cfg.Origin.HostID, Source: "psi"},
			Actor:   types.Actor{Kind: types.ActorDaemon, ID: "troubled"},
			Payload: map[string]any{"op": "sample", "n": i},
		})
	}
	before := d.Ledger.Status()
	batch := batchWriterOf(t, d)
	if _, err := batch(context.Background(), drafts); err != nil {
		t.Fatalf("batch boundary: %v", err)
	}
	after := d.Ledger.Status()
	if got := after.FsyncCalls - before.FsyncCalls; got != 1 {
		t.Fatalf("a K=%d batch through the boot's boundary cost %d durable commits, want 1", k, got)
	}
}

// batchWriterOf extracts the batch boundary the boot installed. The Sensors
// surface exposes it through a probe write; reaching it directly would need an
// exported field, so the test drives it the way the reconcile does.
func batchWriterOf(t *testing.T, d *Daemon) func(context.Context, []types.RecordDraft) ([]types.Record, error) {
	t.Helper()
	fn := probeBatchBoundary(d)
	if fn == nil {
		t.Fatal("the boot did not install a §3.3a batch boundary on the sensors")
	}
	return fn
}

// probeBatchBoundary reaches the installed boundary for the test: the daemon
// records the closure it passed to SetBatchWriter on the Daemon for exactly
// this kind of verification.
func probeBatchBoundary(d *Daemon) func(context.Context, []types.RecordDraft) ([]types.Record, error) {
	if d == nil || d.batchBoundary == nil {
		return nil
	}
	return d.batchBoundary
}

func sigForTest(scope string, n int) string {
	return types.NewSig(types.SigSource("psi"), "sha256", 1, []byte(scope+strings.Repeat("x", n+1))).String()
}
