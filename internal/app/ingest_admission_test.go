package app

// ingest_admission_test.go — QA-TROUBLE-25's regression pin.
//
// The row's defect: a freshly-READY shipped-example daemon answered the
// documented SPEC-04 §2.4 on-ramp POST with 429 `ledger backpressure` after GET
// /api/1/ had already answered 200 (hosted CI run 43479181427). READY was a
// promise the boot had not proved: the ledger was still draining the boot's own
// records, and the sentinel's `ledger_wait` expired on an append that would
// have landed a moment later.
//
// The gate that fixes it is internal/app/ingest_admission.go. What is pinned
// here:
//
//   - the retry/bound logic of the gate itself (deterministic, no host needed):
//     a probe that fits on a later attempt is admitted, a probe that never fits
//     refuses with the sentinel's budget named, a probe that ERRORS is reported
//     as the append failure it is, and a cancelled boot reports cancellation
//     instead of a verdict on the ledger;
//   - the wiring: a real boot writes the `ingest_admission` lifecycle record
//     carrying its measured ack, and it is written before READY is signalled.
//     That last assertion is the one that fails on the pre-fix boot — there is
//     no such record, because the boot served without proving anything.
//
// The user-visible half of the row (a fresh daemon that answers GET /api/1/ with
// 200 accepts the documented on-ramp POST) stays asserted by
// TestShippedExampleConfigBootsToServe, unmodified: this fix must not need that
// test weakened, and it does not.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// admissionTestBudget is a policy shaped like the production one with
// millisecond numbers, so the retry logic is exercised in microseconds.
func admissionTestBudget(ack time.Duration, attempts int) admissionBudget {
	return admissionBudget{Ack: ack, Bound: 2 * time.Second, Pause: time.Millisecond, Attempts: attempts}
}

// TestAdmissionGateAdmitsWhenALaterAttemptFits proves the gate keeps probing
// while the writer is draining and admits as soon as one append fits the budget
// — the exact shape of the measured CI failure (the first append loses, a later
// one wins).
func TestAdmissionGateAdmitsWhenALaterAttemptFits(t *testing.T) {
	acks := []time.Duration{30 * time.Millisecond, 20 * time.Millisecond, 5 * time.Millisecond}
	var calls int
	probe := func(context.Context) (time.Duration, error) {
		took := acks[calls]
		calls++
		return took, nil
	}
	var recorded []map[string]any
	record := func(payload map[string]any) { recorded = append(recorded, payload) }

	if err := runAdmissionGate(context.Background(), admissionTestBudget(10*time.Millisecond, 5), probe, record); err != nil {
		t.Fatalf("runAdmissionGate = %v, want nil: a later attempt met the %s budget", err, 10*time.Millisecond)
	}
	if calls != 3 {
		t.Fatalf("probe called %d times, want 3 (it must keep probing while the writer drains)", calls)
	}
	if len(recorded) != 1 {
		t.Fatalf("gate recorded %d admission records, want exactly 1: %v", len(recorded), recorded)
	}
	got := recorded[0]
	if admitted, _ := got["admitted"].(bool); !admitted {
		t.Errorf("admission record admitted = %v, want true", got["admitted"])
	}
	if ack, _ := got["ack_ms"].(int64); ack != 5 {
		t.Errorf("admission record ack_ms = %v, want the MEASURED 5ms of the attempt that fitted", got["ack_ms"])
	}
	if attempts, _ := got["attempts"].(int); attempts != 3 {
		t.Errorf("admission record attempts = %v, want 3", got["attempts"])
	}
	if budget, _ := got["budget_ms"].(int64); budget != 10 {
		t.Errorf("admission record budget_ms = %v, want the enforced 10ms", got["budget_ms"])
	}
}

// TestAdmissionGateRefusesWhenNoAttemptFits proves the bound: a writer that
// never lands an append inside the ingest budget withholds READY (the caller
// refuses the boot) and the error names the sentinel's budget, so the refusal
// reads as the ingest promise it is.
func TestAdmissionGateRefusesWhenNoAttemptFits(t *testing.T) {
	var calls int
	probe := func(context.Context) (time.Duration, error) {
		calls++
		return 40 * time.Millisecond, nil
	}
	var recorded int
	record := func(map[string]any) { recorded++ }

	err := runAdmissionGate(context.Background(), admissionTestBudget(10*time.Millisecond, 3), probe, record)
	if err == nil {
		t.Fatalf("runAdmissionGate = nil, want a refusal: no attempt met the 10ms budget")
	}
	if calls != 3 {
		t.Errorf("probe called %d times, want the 3-attempt cap", calls)
	}
	if recorded != 0 {
		t.Errorf("gate wrote %d admission records on a refused boot, want 0 (admission was never proven)", recorded)
	}
	msg := err.Error()
	for _, want := range []string{"ingest admission not proven", "ledger_wait 10ms", "40ms", "3 attempt"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q does not mention %q", msg, want)
		}
	}
}

// TestAdmissionGateReportsTheAppendFailure proves a failing append is reported
// as itself (the ledger error a boot must show), never as a budget verdict.
func TestAdmissionGateReportsTheAppendFailure(t *testing.T) {
	appendErr := errors.New("TROUBLE-LEDGER-012: cannot open ledger part")
	probe := func(context.Context) (time.Duration, error) { return time.Millisecond, appendErr }

	err := runAdmissionGate(context.Background(), admissionTestBudget(10*time.Millisecond, 2), probe, nil)
	if err == nil {
		t.Fatal("runAdmissionGate = nil, want the append failure")
	}
	if !strings.Contains(err.Error(), appendErr.Error()) {
		t.Errorf("refusal %q does not carry the append error %q", err, appendErr)
	}
}

// TestAdmissionGateReportsCancellation proves a boot cancelled underneath the
// gate reports cancellation, not a ledger verdict: an operator's SIGTERM must
// never look like a wedged writer.
func TestAdmissionGateReportsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	probe := func(context.Context) (time.Duration, error) {
		cancel()
		return 40 * time.Millisecond, nil
	}
	err := runAdmissionGate(ctx, admissionTestBudget(10*time.Millisecond, 3), probe, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runAdmissionGate = %v, want context.Canceled", err)
	}
}

// TestBootProvesIngestAdmissionBeforeReady is the wiring half: a real boot
// records the admission it proved, with the measurement, and it does so before
// READY (the phase table in bootphase_test.go pins the ordering; this pins the
// evidence the phase produced).
//
// Pre-fix this fails at the first assertion: the boot reported READY having
// proved nothing, so the ledger carries no `ingest_admission` record at all.
func TestBootProvesIngestAdmissionBeforeReady(t *testing.T) {
	h := bootDaemon(t)

	var admission, probes []types.Record
	for _, rec := range allRecords(t, h) {
		if rec.Kind != types.KLifecycle {
			continue
		}
		switch stage, _ := rec.Payload["stage"].(string); stage {
		case "ingest_admission":
			admission = append(admission, rec)
		case "ingest_admission_probe":
			probes = append(probes, rec)
		}
	}
	if len(admission) != 1 {
		t.Fatalf("boot carries %d `ingest_admission` records, want exactly 1: the boot reported READY "+
			"without proving the ledger can land one append inside the sentinel's ledger_wait (QA-TROUBLE-25)",
			len(admission))
	}
	if len(probes) == 0 {
		t.Fatalf("boot carries no `ingest_admission_probe` record: the admission was asserted, not measured")
	}
	rec := admission[0]
	admitted, _ := rec.Payload["admitted"].(bool)
	if !admitted {
		t.Errorf("admission record admitted = %v, want true (the boot only reaches READY when it was)", rec.Payload["admitted"])
	}
	var ackMS float64
	switch v := rec.Payload["ack_ms"].(type) {
	case float64:
		ackMS = v
	case int64:
		ackMS = float64(v)
	case int:
		ackMS = float64(v)
	default:
		t.Fatalf("admission record carries no numeric ack_ms: %v", rec.Payload)
	}
	if ackMS > float64(ingestAckBudget/time.Millisecond) {
		t.Errorf("measured ack %.1fms exceeds the sentinel's ledger_wait %s — READY must not have been signalled",
			ackMS, ingestAckBudget)
	}
	var budgetMS float64
	switch v := rec.Payload["budget_ms"].(type) {
	case float64:
		budgetMS = v
	case int64:
		budgetMS = float64(v)
	case int:
		budgetMS = float64(v)
	}
	if want := float64(ingestAckBudget / time.Millisecond); budgetMS != want {
		t.Errorf("admission record budget_ms = %v, want %v (the gate must enforce the sentinel's own budget)", budgetMS, want)
	}
	// The measurement belongs to a record the boot wrote before it reported
	// READY: the probe it timed is in the same ledger, ahead of the summary.
	if probes[0].Seq > rec.Seq {
		t.Errorf("probe seq %d is AFTER the admission record seq %d; the measurement was not a boot probe",
			probes[0].Seq, rec.Seq)
	}
	t.Logf("boot proved ingest admission: ack %.0fms of %s budget over %v attempt(s), probe seq %d, admission seq %d",
		ackMS, ingestAckBudget, rec.Payload["attempts"], probes[0].Seq, rec.Seq)
}
