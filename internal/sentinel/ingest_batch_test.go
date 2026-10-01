package sentinel

// TRBL-084: one group-commit window per single-event ingest. A first-occurrence
// event that opens a group used to pay one ledger window for the event record
// and a second for the group create/release record (plus a third on flush).
// These tests pin the window count on the admit path and the no-batcher
// fallback.

import (
	"context"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

func trbl084Config(c *Config) {
	c.CanaryProject = ""
	c.Projects[0].QuotaEPM = 1_000_000
	c.GroupFlush = types.Duration("1h")
}

// TestTRBL084FirstOccurrenceOneWindow pins the batch: admitting one
// first-occurrence event that opens a group emits its records through ONE
// AppendBatch call (one fsync window), never through per-record Append.
func TestTRBL084FirstOccurrenceOneWindow(t *testing.T) {
	ts := newTestServer(t, trbl084Config)
	defer ts.close()

	entry, _ := ts.s.projects.project("1")
	ev := groupEvent("trbl084-window", "pay-api@1.0.0")
	if _, err := ts.s.admitEvent(context.Background(), entry, ev, "event", "test"); err != nil {
		t.Fatalf("admitEvent: %v", err)
	}

	batches, singles := ts.sink.windowCounts()
	if batches != 1 || singles != 0 {
		t.Fatalf("windows: got %d batch call(s) + %d single Append call(s), want 1 batch + 0 single", batches, singles)
	}
	if got := len(ts.sink.ofKind(types.KEvent)); got != 1 {
		t.Fatalf("event records: got %d, want 1", got)
	}
	if got := len(ts.sink.ofKind(types.KGroup)); got != 1 {
		t.Fatalf("group records: got %d, want 1", got)
	}
}

// TestTRBL084MarkFlushedSeesEventSeq pins requirement 4: the group index
// watermark still carries the EVENT record's seq, not the batch's last member.
func TestTRBL084MarkFlushedSeesEventSeq(t *testing.T) {
	ts := newTestServer(t, trbl084Config)
	defer ts.close()

	entry, _ := ts.s.projects.project("1")
	ev := groupEvent("trbl084-seq", "pay-api@1.0.0")
	rec, err := ts.s.admitEvent(context.Background(), entry, ev, "event", "test")
	if err != nil {
		t.Fatalf("admitEvent: %v", err)
	}
	if rec.Kind != types.KEvent {
		t.Fatalf("returned record kind %q, want event", rec.Kind)
	}
	digest, _ := rec.Payload["digest"].(string)
	if digest == "" {
		t.Fatalf("event record payload carries no digest")
	}
	st, ok := ts.s.groups.state(digest)
	if !ok {
		t.Fatalf("group state missing for digest %s", digest)
	}
	if st.eventsUpperSeq != rec.Seq {
		t.Fatalf("group watermark seq %d, want event seq %d", st.eventsUpperSeq, rec.Seq)
	}
}

// batchlessSink is a sink WITHOUT AppendBatch: the fallback path must keep
// working with sequential per-record appends (requirement 5).
type batchlessSink struct{ m *memSink }

func (b batchlessSink) Append(ctx context.Context, d types.RecordDraft) (types.Record, error) {
	return b.m.Append(ctx, d)
}
func (b batchlessSink) LastSeq() uint64 { return b.m.LastSeq() }

func TestTRBL084NoBatcherFallback(t *testing.T) {
	t.Setenv("TZ", "UTC")
	cfg := testConfig(t, trbl084Config)
	sink := &memSink{}
	s, err := NewServer(cfg, batchlessSink{sink}, newTestScrubber(t, cfg.Projects))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	entry, _ := s.projects.project("1")
	ev := groupEvent("trbl084-fallback", "pay-api@1.0.0")
	if _, err := s.admitEvent(context.Background(), entry, ev, "event", "test"); err != nil {
		t.Fatalf("admitEvent: %v", err)
	}
	if got := len(sink.ofKind(types.KEvent)); got != 1 {
		t.Fatalf("event records: got %d, want 1", got)
	}
	if got := len(sink.ofKind(types.KGroup)); got != 1 {
		t.Fatalf("group records: got %d, want 1", got)
	}
}
