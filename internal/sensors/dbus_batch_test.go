package sensors

// dbus_batch_test.go — the §3.3a batch reconcile contract (TRBL-044).
//
// SPEC-12 §7b measured the boot's READY latency as linear in the ledger
// records the startup reconcile writes, because a SERIAL producer pays one
// group-commit window per record. The §3.3a decision (TRBL-044) keeps every
// failed unit's own record — SPEC-03 §3.3's per-unit incident model is
// untouched — but writes the whole sweep through ONE durable batch.
//
// These tests pin the sensors half: all decisions still happen per arrival
// (merge rule, rules, stabilization, breakers), the writes are ONE batch
// through the batch boundary, nothing reaches the ladder before the batch is
// durable, and a batch failure drops the whole sweep with a named error and a
// drop count — never a partial sweep, never silence.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// batchCalls records what the installed batch boundary received.
type batchCalls struct {
	drafts [][]types.RecordDraft
	recs   [][]types.Record
	err    error
}

// batchHarness is a sensors harness with the §3.3a batch boundary installed
// and its calls recorded. The merge state is primed the way startDBus would
// (newMergeTracker) so reconcile's batch path is reachable without a bus.
func batchHarness(t *testing.T) (*harness, *batchCalls) {
	t.Helper()
	h := newHarness(t)
	h.s.dbState.merge = newMergeTracker(30*time.Second, h.now)
	calls := &batchCalls{}
	batchBoundary := func(ctx context.Context, drafts []types.RecordDraft) ([]types.Record, error) {
		if calls.err != nil {
			return nil, calls.err
		}
		out := make([]types.Record, 0, len(drafts))
		for _, d := range drafts {
			rec, err := h.emit(ctx, d)
			if err != nil {
				return out, err
			}
			out = append(out, rec)
		}
		calls.drafts = append(calls.drafts, drafts)
		calls.recs = append(calls.recs, out)
		return out, nil
	}
	h.s.SetBatchWriter(batchBoundary)
	return h, calls
}

// failedUnits renders n synthetic failed units the way listUnits would.
func failedUnits(n int) []unitStatus {
	out := make([]unitStatus, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, unitStatus{
			Name:        fmt.Sprintf("batch%03d.service", i),
			LoadState:   "loaded",
			ActiveState: "failed",
			SubState:    "failed",
		})
	}
	return out
}

// TestStartupReconcileWritesOneBatchForNSweeps is §3.3a's headline: a sweep
// over N failed units writes exactly ONE batch carrying N drafts — not N
// single-record emits.
func TestStartupReconcileWritesOneBatchForNUnits(t *testing.T) {
	h, calls := batchHarness(t)
	ctx := context.Background()

	const n = 50
	drafts := h.s.evaluateEveryUnit(ctx, "system", failedUnits(n), h.now())
	if len(drafts) != n {
		t.Fatalf("decide phase produced %d drafts, want %d (one per failed unit — the per-unit record model)", len(drafts), n)
	}
	before := len(h.records)
	h.s.emitBatchOutcome(ctx, drafts)
	if len(calls.drafts) != 1 {
		t.Fatalf("batch boundary called %d times for one sweep, want 1", len(calls.drafts))
	}
	if got := len(calls.drafts[0]); got != n {
		t.Fatalf("the one batch carried %d drafts, want %d", got, n)
	}
	if len(h.records)-before != n {
		t.Fatalf("sweep wrote %d records, want %d", len(h.records)-before, n)
	}
	// The records still carry the reconcile arrival path and the per-unit
	// merge accounting: the batch changed the write shape, not the record.
	// (The accounting block lives at payload.detail, as eventPayload nests it.)
	for _, r := range h.records[before:] {
		detail, _ := r.Payload["detail"].(map[string]any)
		if detail["arrival_path"] != string(arrivalReconcile) {
			t.Fatalf("record detail.arrival_path = %v, want reconcile", detail["arrival_path"])
		}
		if detail["unit_key"] == "" {
			t.Fatal("a batch member lost its per-unit merge key")
		}
	}
}

// TestReconcileBatchFailureDropsTheWholeSweepLoudly: a failed batch is ONE
// named error plus a drop count for the WHOLE sweep — no partial sweep, no
// silence (§3.8: drops are never silent).
func TestReconcileBatchFailureDropsTheWholeSweepLoudly(t *testing.T) {
	h, calls := batchHarness(t)
	calls.err = errors.New("disk on fire")
	ctx := context.Background()

	sweeps := failedUnits(12)
	before := len(h.records)
	h.s.emitBatchOutcome(ctx, h.s.evaluateEveryUnit(ctx, "system", sweeps, h.now()))
	wrote := h.records[before:]
	// No sweep record may land: the batch members are the only writers of the
	// reconcile arrival path, so any surviving one would be a partial sweep.
	// (recError's own failure record is an event-kind row too, which is why
	// the discriminator is the accounting detail, not the kind.)
	for _, r := range wrote {
		detail, _ := r.Payload["detail"].(map[string]any)
		if detail["arrival_path"] == string(arrivalReconcile) {
			t.Fatalf("a failed batch wrote a sweep record (sig %s): never a partial sweep", r.Sig)
		}
	}
	if rt := h.s.rt[types.SenDBus]; rt == nil || rt.drops.Load() != 12 {
		t.Fatalf("drops = %d, want 12 (the whole sweep accounted for)", rt.drops.Load())
	}
	// The batch write failure is named in an error record (recError), which is
	// how the sweep's failure is loud — the drop count carries the accounting.
	found := false
	for _, r := range wrote {
		if d, _ := r.Payload["detail"].(string); contains(d, "reconcile batch write") {
			found = true
		}
	}
	if !found {
		t.Fatal("a failed batch must be named (reconcile batch write) in a record, not silent")
	}
}

// TestReconcileWithoutBatchBoundaryKeepsPerRecordShape: without the boot's
// batch writer the sweep degrades to the per-record emit — the §3.8 shape,
// durable-on-return per record (the compatibility rule).
func TestReconcileWithoutBatchBoundaryKeepsPerRecordShape(t *testing.T) {
	h := newHarness(t)
	h.s.dbState.merge = newMergeTracker(30*time.Second, h.now)
	ctx := context.Background()

	const n = 5
	drafts := h.s.evaluateEveryUnit(ctx, "system", failedUnits(n), h.now())
	if len(drafts) != n {
		t.Fatalf("decide phase produced %d drafts, want %d", len(drafts), n)
	}
	before := len(h.records)
	h.s.emitBatchOutcome(ctx, drafts)
	if len(h.records)-before != n {
		t.Fatalf("fallback wrote %d records, want %d", len(h.records)-before, n)
	}
}

// TestBatchSweepFiresTheLadderAfterDurability pins the §3.3a ladder bridge: a
// firing batch member admits AFTER the batch boundary returned (durable), and
// with the durable record's identity — the same ordering the per-record emit
// gave (Append returns, then Admit).
func TestBatchSweepFiresTheLadderAfterDurability(t *testing.T) {
	h, calls := batchHarness(t)
	ctx := context.Background()

	order := []string{}
	bridgeErr := func(ctx context.Context, rec types.Record, payload map[string]any) error {
		fire, _ := payload["fire"].(bool)
		if !fire {
			return nil
		}
		order = append(order, "bridge:"+fmt.Sprint(rec.RecID != ""))
		return nil
	}
	h.s.SetLadderBridge(bridgeErr)

	h.writeRules("10.toml", `
[[rule]]
name = "batch_reconcile_rule"
source = "dbus"
for = "0s"
cooldown = "5m"
severity = "critical"
entry_rung = "play"
[[rule.match]]
field = "unit_substate"
op = "=="
value = "failed"
value_type = "string"
`)
	h.mustReload()

	sweeps := failedUnits(3)
	drafts := h.s.evaluateEveryUnit(ctx, "system", sweeps, h.now())
	if len(drafts) != 3 {
		t.Fatalf("decide phase produced %d drafts, want 3", len(drafts))
	}
	if len(order) != 0 {
		t.Fatal("nothing may reach the ladder before the write")
	}
	h.s.emitBatchOutcome(ctx, drafts)
	fired := 0
	for _, d := range drafts {
		if fire, _ := d.Payload["fire"].(bool); fire {
			fired++
		}
	}
	if fired == 0 {
		t.Fatal("the test needs a firing rule to pin the ordering; the rule above must fire on a failed unit")
	}
	if len(order) != fired {
		t.Fatalf("bridge called %d times, want %d (once per firing batch member, after durability)", len(order), fired)
	}
	for _, o := range order {
		if o != "bridge:true" {
			t.Fatalf("bridge received %q; the durable record (real rec_id) must reach the ladder, not a draft", o)
		}
	}
	if len(calls.drafts) != 1 {
		t.Fatalf("batch boundary called %d times, want 1", len(calls.drafts))
	}
}

// TestSecondReconcileSweepBatchesAgainAndDoesNotDuplicate: the merge rule's
// dedup decides membership, not the batch — an already-open incident inside
// the merge window attaches (counted), which is still a record; a resolved
// unit produces nothing. The batch carries exactly what the merge state says.
func TestSecondReconcileSweepBatchesWhatTheMergeRuleSays(t *testing.T) {
	h, calls := batchHarness(t)
	ctx := context.Background()

	units := failedUnits(4)
	first := h.s.evaluateEveryUnit(ctx, "system", units, h.now())
	if len(first) != 4 {
		t.Fatalf("first sweep drafted %d, want 4 (all open)", len(first))
	}
	h.s.emitBatchOutcome(ctx, first)
	if n := len(calls.drafts); n != 1 {
		t.Fatalf("first sweep made %d batch calls, want 1", n)
	}

	// A second sweep 10s later (inside the 30s merge window): every unit
	// attaches into its open incident — still one record per unit, still one
	// batch. The count carried is now 2 (the accounting block lives at
	// payload.detail, as eventPayload nests it).
	h.advance(10 * time.Second)
	second := h.s.evaluateEveryUnit(ctx, "system", units, h.now())
	if len(second) != 4 {
		t.Fatalf("second sweep drafted %d, want 4 (attach, per the §3.6 merge rule)", len(second))
	}
	for _, d := range second {
		if c, _ := d.Payload["detail"].(map[string]any)["count"].(float64); c != 2 {
			t.Fatalf("attached record count = %v, want 2", d.Payload["detail"])
		}
	}
	h.s.emitBatchOutcome(ctx, second)
	if n := len(calls.drafts); n != 2 {
		t.Fatalf("two sweeps made %d batch calls, want 2 (one per sweep)", n)
	}
}
