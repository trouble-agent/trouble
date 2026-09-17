package sentinel

import (
	"context"
	"sync/atomic"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// discardSink is a ledgerSink that only counts: it lets a test measure the
// ingestion path's own cost (throughput, latency, memory) without the ledger's
// filesystem work, and it is what the memory test uses.
type discardSink struct {
	seq atomic.Uint64
	rec atomic.Uint64
}

func (d *discardSink) Append(ctx context.Context, r types.RecordDraft) (types.Record, error) {
	d.rec.Add(1)
	return types.Record{Seq: d.seq.Add(1), Kind: r.Kind, Sig: r.Sig, Payload: r.Payload}, nil
}

func (d *discardSink) LastSeq() uint64 { return d.seq.Load() }
