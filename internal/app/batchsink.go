package app

// TRBL-084: the sentinel's single-event ingest (internal/sentinel
// appendRecordBatch) asks the sink for AppendBatch — an optional capability
// asserted by type, never a required method — so one admitted event's records
// (the event and the group create/release/flush record it emits) share ONE
// group-commit window (SPEC-01 §3.5a). The first cut of the AppendBatch path
// (0de49e8) never reached production because these composition-root sinks
// carried only Append: appendRecordBatch's capability check answered ok=false
// on every real ingest and the server fell back to one window per record.
// The per-batch durability contract of §3.5a rule 2 — "durable-on-return per
// BATCH in place of per-record" — is accepted here exactly as the sensors'
// reconcile accepted it (SPEC-03 §3.3a): the records are unchanged, the seq
// space stays contiguous, and the window count is one, not three.

import (
	"context"

	"github.com/trouble-agent/trouble/internal/ledger"
	"github.com/trouble-agent/trouble/internal/types"
)

// sentinelSinkBatch is the batch member of the sentinel's sink seam, kept in
// sync with internal/sentinel's ledgerBatchSink by the assertion the sentinel
// package runs on its own side (its compile-checked adapter) and by the
// production-wiring test here. It cannot be named from this package: the
// sentinel's seam is unexported, and the compiler checks the shape through
// the production-wiring test's interface literal.
type sentinelSinkBatch interface {
	AppendBatch(ctx context.Context, drafts []types.RecordDraft) ([]types.Record, error)
}

// AppendBatch writes the whole batch through the ledger in ONE group-commit
// window (SPEC-01 §3.5a; ledger.Ledger.AppendBatch), then performs the side
// effects the per-record path performs per record — the group-sample note and
// the SPEC-04→SPEC-05 ladder hand-off — for each durable record, in record
// order. A batch error is returned as the ledger produced it: the sentinel's
// own error mapping (appendRecordBatch) classifies hub and overload shapes
// from the sink's error, so the batch member must not re-wrap.
func (s sentinelSink) AppendBatch(ctx context.Context, drafts []types.RecordDraft) ([]types.Record, error) {
	recs, err := s.L.AppendBatch(ctx, drafts)
	if err != nil {
		return nil, err
	}
	for i, rec := range recs {
		if rec.Kind == types.KEvent && rec.RecID != "" {
			if dg, _ := drafts[i].Payload["digest"].(string); dg != "" {
				s.noteSample(dg, rec.RecID)
			}
		}
		sentinelToLadder(s.d, rec)
	}
	return recs, nil
}

// hubSinkBatch is the batch member of the hub-mounted sink. Satisfied by the
// embedded sentinelSink — the point of the embedding: the queue is a hop in
// front of the writer, never a second writer (hub.go), so its batch member
// rides the same embedded struct. The sentinel sees it through the type
// assertion in appendRecordBatch; the wiring test pins the satisfaction.
type hubSinkBatch = sentinelSinkBatch

// AppendBatch routes the whole batch through the profile's plumbing: one
// offer per record, in order, inside the consumer's fsync boundary (SPEC-13
// §2.3). The batch stays a loop over Runtime.Ingest because the door → stream
// → consumer pipeline IS the profile's durability mechanism, not a writer to
// be bypassed — each record carries its own idempotency key, and the consumer
// coalesces whole entry batches into one ledger group commit. The sentinel's
// LedgerWait bound still applies to the batch as a whole: the first error
// returns and the rest of the batch is abandoned, matching the per-record
// path's abandon-on-first-error semantics (appendRecordDraft fails the
// request on the first failed append).
func (h hubSink) AppendBatch(ctx context.Context, drafts []types.RecordDraft) ([]types.Record, error) {
	recs := make([]types.Record, 0, len(drafts))
	for _, d := range drafts {
		rec, err := h.rt.Ingest(ctx, d)
		if err != nil {
			return nil, err
		}
		recs = append(recs, rec)
	}
	return recs, nil
}

// The ledger carries the real batch path the sinks route into; this var keeps
// the import honest and the §3.5a reference greppable from this file.
var _ = ledger.LossWindowStatement
