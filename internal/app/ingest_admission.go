package app

// ingest_admission.go — QA-TROUBLE-25: READY means ingest can be admitted.
//
// The defect this file fixes was measured on hosted CI (run 43479181427, head
// f1c8e71, 4-core runner at load_avg 4.84) on a product tree that had just
// passed: the shipped-example daemon reported READY, answered GET /api/1/ with
// 200, and then answered the DOCUMENTED on-ramp POST (SPEC-04 §2.4,
// `?sentry_key=<public_key>`) with
//
//	429 {"causes":[],"detail":"ledger backpressure"}
//
// The body's prose is the sentinel's `ledger_wait` bound expiring
// (internal/sentinel group.go: the append's durable ack did not arrive inside
// 2s). The queue was not full — the ledger's queue cap is 65536 records and a
// fresh boot writes tens — so the writer was BUSY, not saturated: the boot's
// own records were still being flushed under the group-commit window the
// sensors' startup reconcile and the boot records had just queued behind. READY
// was therefore a promise the daemon could not keep: it served a listener whose
// first documented request could only be shed.
//
// The fix is (b) of the row, not (a): the boot does not report READY until the
// ledger has PROVEN it can accept one record inside the same budget ingest
// enforces. Waiting inside the admission path (a) was rejected because the
// sentinel's `ledger_wait` is the saturation ceiling SPEC-04 §3.7 pins, and the
// sentinel package is outside this change's seam; stretching a budget there
// would trade a boot-time refusal for a slower one under real saturation. The
// boot has the information the sentinel cannot have — that it has just written
// a burst and nothing has yet proved the writer caught up — so the boot is where
// the promise is made honest.
//
// Why a measured append and not "the queue is empty": an empty queue is not the
// property ingest needs. The property is "one append completes its durable ack
// inside ledger_wait", and the only way to know that is to pay it. This gate
// appends one lifecycle record through the same Store the boot already writes
// through (the same ledger.Append the sentinel's sink calls), times the ack, and
// requires it inside `ingestAckBudget`. If the writer is still draining, the
// attempt is slow, the gate waits and re-probes — bounded by
// `bootAdmissionBound` and `bootAdmissionAttempts` — and only a boot that never
// meets the budget refuses to serve (the same fail-loud posture the config
// record already has: a daemon that cannot write a record must not serve).
//
// Residual, stated rather than papered over: this proves the ledger writer is
// ingest-capable at the instant READY fires. A 429 is still legitimate under
// real saturation afterwards (a queue cap that is actually reached), and the
// sentinel's own `ledger_wait` bound is untouched by this change.

import (
	"context"
	"fmt"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

const (
	// ingestAckBudget is the sentinel's `ledger_wait` (internal/sentinel's
	// defaultLedgerWait, SPEC-04 §3.7): the budget one append gets for its
	// durable ack before the documented on-ramp POST is answered 429. The gate
	// enforces the SAME number, so "READY" and "ingest-admissible" cannot drift.
	ingestAckBudget = 2 * time.Second

	// bootAdmissionBound is how long the boot keeps re-probing before it
	// refuses to report READY. It is deliberately much larger than
	// ingestAckBudget: a single slow window is expected on a loaded host, a
	// writer that cannot land ONE record inside this bound is wedged.
	bootAdmissionBound = 15 * time.Second

	// bootAdmissionPause is the gap between attempts, so a re-probe does not
	// queue another record inside the window that just failed.
	bootAdmissionPause = 100 * time.Millisecond

	// bootAdmissionAttempts caps the probe records one boot may write. On a
	// healthy boot exactly one probe and one summary record are written; the cap
	// keeps a pathological boot from flooding the audit trail it is failing.
	bootAdmissionAttempts = 8
)

// admissionBudget is the gate's policy. It is a value rather than three
// constants so the retry/bound logic below is exercisable without a host that
// happens to be slow.
type admissionBudget struct {
	// Ack is the durable-ack budget one append must meet (the sentinel's
	// ledger_wait).
	Ack time.Duration
	// Bound is the total time the boot will keep re-probing.
	Bound time.Duration
	// Pause is the wait between attempts.
	Pause time.Duration
	// Attempts caps how many probes one boot may write.
	Attempts int
}

// bootAdmissionBudget is the production policy (the constants above).
func bootAdmissionBudget() admissionBudget {
	return admissionBudget{
		Ack:      ingestAckBudget,
		Bound:    bootAdmissionBound,
		Pause:    bootAdmissionPause,
		Attempts: bootAdmissionAttempts,
	}
}

// ingestAdmissionProbe performs one append through the door ingest uses and
// reports how long its durable ack took. It is a seam only so the retry logic
// can be driven deterministically; production passes the daemon's own Store.
type ingestAdmissionProbe func(ctx context.Context) (time.Duration, error)

// admissionRecorder writes the gate's own lifecycle records.
type admissionRecorder func(payload map[string]any)

// awaitIngestAdmission is the boot gate (§4.1 phase `ingest_admission`, between
// the background loops and READY). See the file comment for why the boot owns
// this check.
func (d *Daemon) awaitIngestAdmission(ctx context.Context) error {
	if d == nil || d.Store == nil {
		return nil
	}
	probe := func(pctx context.Context) (time.Duration, error) {
		start := time.Now()
		_, err := d.Store.Append(pctx, types.KLifecycle, "", "", map[string]any{
			"stage":     "ingest_admission_probe",
			"budget_ms": ingestAckBudget.Milliseconds(),
			"detail":    "boot probe: one ledger append, timed against the sentinel's ledger_wait",
		})
		return time.Since(start), err
	}
	record := func(payload map[string]any) {
		_, _ = d.Store.Append(ctx, types.KLifecycle, "", "", payload)
	}
	return runAdmissionGate(ctx, bootAdmissionBudget(), probe, record)
}

// runAdmissionGate probes until one append's durable ack fits in b.Ack, then
// records the measured admission and returns nil. It returns an error naming the
// attempts and the last measurement when the budget is never met, and the
// context error when the boot is cancelled underneath it.
func runAdmissionGate(ctx context.Context, b admissionBudget, probe ingestAdmissionProbe, record admissionRecorder) error {
	if probe == nil {
		return nil
	}
	if b.Attempts < 1 {
		b.Attempts = 1
	}
	if b.Ack <= 0 {
		b.Ack = ingestAckBudget
	}
	deadline := time.Now().Add(b.Bound)
	var lastAck time.Duration
	var lastErr error
	for attempt := 1; attempt <= b.Attempts; attempt++ {
		took, err := probe(ctx)
		if cerr := ctx.Err(); cerr != nil {
			// Cancellation is not a verdict on the ledger: report it as itself.
			return cerr
		}
		lastAck, lastErr = took, err
		if err == nil && took <= b.Ack {
			if record != nil {
				record(map[string]any{
					"stage":     "ingest_admission",
					"admitted":  true,
					"ack_ms":    took.Milliseconds(),
					"budget_ms": b.Ack.Milliseconds(),
					"attempts":  attempt,
					"detail": fmt.Sprintf("one ingest append was durably acked in %s (ledger_wait %s) before READY",
						took.Round(time.Millisecond), b.Ack),
				})
			}
			return nil
		}
		if attempt == b.Attempts {
			break
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		pause := b.Pause
		if remaining < pause {
			pause = remaining
		}
		if pause > 0 {
			select {
			case <-time.After(pause):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	measured := lastAck.Round(time.Millisecond).String()
	if lastErr != nil {
		measured += fmt.Sprintf(" (%v)", lastErr)
	}
	return fmt.Errorf("ingest admission not proven at boot: %d attempt(s), last durable ack %s, ledger_wait %s — "+
		"READY is withheld rather than serving an on-ramp the daemon must shed (QA-TROUBLE-25)",
		b.Attempts, measured, b.Ack)
}
