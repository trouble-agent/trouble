package ledger

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// compactFixture builds one day of records (events + an audit-spine set) and
// moves the writer to the next day so the day is compactable.
func compactFixture(t *testing.T, clk *fakeClock, day time.Time, events int, spine bool, muts ...func(*Options)) *Ledger {
	t.Helper()
	l := testLedger(t, clk, muts...)
	for i := 0; i < events; i++ {
		mustAppend(t, l, eventDraft("journald", fmt.Sprintf("journald:sha256v1:%04d", i),
			fmt.Sprintf("%064x", i%3), i%2))
	}
	if spine {
		inc := types.NewID(types.PInc)
		mustAppend(t, l, types.RecordDraft{
			Kind: types.KIncident, Sig: "journald:sha256v1:0001", Inc: inc,
			Origin: types.Origin{HostID: "7f3a91c2d4e5b607", Source: "journald"},
			Actor:  testActor(),
			Payload: map[string]any{
				"state": "recorded", "entry_rung": "play", "op": "transition",
				"evidence_marker": "SPINE-INC-PAYLOAD",
			},
		})
		mustAppend(t, l, types.RecordDraft{
			Kind: types.KVerify, Sig: "journald:sha256v1:0001", Inc: inc,
			Origin: types.Origin{HostID: "7f3a91c2d4e5b607", Source: "journald"},
			Actor:  testActor(),
			Payload: map[string]any{
				"result": "passed", "events_observed": 0, "marker": "SPINE-VERIFY-PAYLOAD",
			},
		})
		mustAppend(t, l, types.RecordDraft{
			Kind: types.KToolCall, Sig: "journald:sha256v1:0001", Inc: inc,
			Origin:  types.Origin{HostID: "7f3a91c2d4e5b607", Source: "journald"},
			Actor:   testActor(),
			Payload: map[string]any{"module": "service.reload", "marker": "SPINE-TOOLCALL-PAYLOAD"},
		})
	}
	// move the writer forward so the fixture day is not the live file
	clk.Advance(24 * time.Hour)
	mustAppend(t, l, eventDraft("psi", "psi:sha256v1:next-day", "ff", 0))
	return l
}

// TestCompactionIsANewGeneration: D.jsonl is never modified in place, D.1.gen
// appears, and gen 0 is unlinked only after the new generation is fsynced.
func TestCompactionIsANewGeneration(t *testing.T) {
	day := time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)
	clk := newFakeClock(day)
	l := compactFixture(t, clk, day, 6, true)
	gen0 := filepath.Join(l.Root(), "2026-09-16.jsonl")
	before, err := os.ReadFile(gen0)
	if err != nil {
		t.Fatal(err)
	}
	fi0, err := os.Stat(gen0)
	if err != nil {
		t.Fatal(err)
	}

	rp := DefaultRetentionPolicy()
	rp.RawKeep = "1s"
	res, err := l.Compact(context.Background(), "2026-09-16", rp)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if res.FromGen != 0 || res.ToGen != 1 || res.ToFile != "2026-09-16.1.gen.jsonl" {
		t.Errorf("result = %+v, want gen 0 → 1", res)
	}
	if !res.TombstoneCountsKept {
		t.Errorf("TombstoneCountsKept must be true")
	}
	gen1, err := os.ReadFile(filepath.Join(l.Root(), res.ToFile))
	if err != nil {
		t.Fatalf("generation 1: %v", err)
	}
	if bytesEqual(gen1, before) {
		t.Errorf("the generation rewrite must actually change the bytes")
	}
	// the audit spine survived verbatim and payloads intact
	for _, marker := range []string{"SPINE-INC-PAYLOAD", "SPINE-VERIFY-PAYLOAD", "SPINE-TOOLCALL-PAYLOAD"} {
		if !strings.Contains(string(gen1), marker) {
			t.Errorf("spine payload %s was lost in compaction", marker)
		}
	}
	if _, err := os.Stat(gen0); !os.IsNotExist(err) {
		t.Errorf("generation 0 must be unlinked after the new generation is fsynced (err=%v)", err)
	}
	if fi0.Size() == 0 {
		t.Errorf("generation 0 was empty")
	}
}

// TestCountsPreserved: Σ group counters before == after, aggregates == distinct
// (group, day).
func TestCountsPreserved(t *testing.T) {
	day := time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)
	clk := newFakeClock(day)
	l := compactFixture(t, clk, day, 9, true)
	before := sumEventCounters(t, l, "2026-09-16")
	if before != 9 {
		t.Fatalf("fixture events before = %d, want 9", before)
	}
	rp := DefaultRetentionPolicy()
	rp.RawKeep = "1s"
	res, err := l.Compact(context.Background(), "2026-09-16", rp)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if res.Aggregates != 3 {
		t.Errorf("Aggregates = %d, want 3 (distinct digests in the fixture)", res.Aggregates)
	}
	after := sumEventCounters(t, l, "2026-09-16")
	if after != before {
		t.Errorf("event counters changed: %d → %d (counts are never dropped)", before, after)
	}
	if !res.TombstoneCountsKept {
		t.Errorf("TombstoneCountsKept = false")
	}
}

// sumEventCounters re-derives the day's event totals from the file set.
func sumEventCounters(t *testing.T, l *Ledger, day string) uint64 {
	t.Helper()
	var total uint64
	parts, _, err := l.authoritativeFiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range parts {
		if p.Day != day {
			continue
		}
		b, err := os.ReadFile(filepath.Join(l.Root(), p.File))
		if err != nil {
			t.Fatal(err)
		}
		for _, ln := range splitLines(b) {
			if len(strings.TrimSpace(string(ln))) == 0 {
				continue
			}
			rec, derr := decodeRecord(ln, true)
			if derr != nil || rec == nil {
				continue
			}
			switch {
			case rec.Kind == types.KEvent || rec.Kind == types.KCanary:
				total++
			case rec.Kind == types.KGroup && isCompactionAggregate(rec):
				if c, ok := rec.Payload["counters"].(map[string]any); ok {
					total += uint64(numOf(c["events"]))
				}
			}
		}
	}
	return total
}

// TestPayloadTTL: an event line older than payload_ttl is rewritten with its
// payload replaced by counters while the spine record count is unchanged; a
// young event keeps its payload.
func TestPayloadTTL(t *testing.T) {
	t.Run("expired", func(t *testing.T) {
		day := time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)
		clk := newFakeClock(day)
		l := compactFixture(t, clk, day, 5, true)
		rp := DefaultRetentionPolicy()
		rp.PayloadTTL = "1s" // the fixture is now the next day: everything is older
		res, err := l.Compact(context.Background(), "2026-09-16", rp)
		if err != nil {
			t.Fatalf("Compact: %v", err)
		}
		if res.PayloadsExpired != 5 {
			t.Errorf("PayloadsExpired = %d, want 5", res.PayloadsExpired)
		}
		b, err := os.ReadFile(filepath.Join(l.Root(), res.ToFile))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), `"payload_ttl_expired":true`) {
			t.Errorf("no payload_ttl_expired marker in the generation file")
		}
		if strings.Contains(string(b), `"op":"sample"`) {
			t.Errorf("an expired event payload survived compaction")
		}
		if !strings.Contains(string(b), `"tombstone_counts_kept":true`) {
			t.Errorf("the aggregate must assert tombstone_counts_kept")
		}
	})
	t.Run("young", func(t *testing.T) {
		// A day younger than both retention_raw (72h) and payload_ttl (720h) has
		// nothing legal to reclaim: compaction must refuse (009) and leave every
		// byte alone rather than fold a payload that is still inside its TTL.
		day := time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)
		clk := newFakeClock(day)
		l := testLedger(t, clk)
		for i := 0; i < 3; i++ {
			mustAppend(t, l, eventDraft("journald", fmt.Sprintf("journald:sha256v1:%04d", i),
				fmt.Sprintf("%064x", i), 0))
		}
		clk.Advance(24 * time.Hour)
		mustAppend(t, l, eventDraft("psi", "psi:sha256v1:next", "aa", 0))
		before, rerr := os.ReadFile(filepath.Join(l.Root(), "2026-09-16.jsonl"))
		if rerr != nil {
			t.Fatal(rerr)
		}
		res, err := l.Compact(context.Background(), "2026-09-16", DefaultRetentionPolicy())
		if CodeOf(err) != types.CodeLedger009 {
			t.Fatalf("Compact of a day inside both windows: err = %v (%s), want %s (refuse, never reclaim a live payload)",
				err, CodeOf(err), types.CodeLedger009)
		}
		after, rerr := os.ReadFile(filepath.Join(l.Root(), "2026-09-16.jsonl"))
		if rerr != nil {
			t.Fatal(rerr)
		}
		if string(before) != string(after) {
			t.Errorf("a refused compaction modified the day file")
		}
		if res.PayloadsExpired != 0 {
			t.Errorf("PayloadsExpired = %d, want 0", res.PayloadsExpired)
		}
	})
}

// TestSpineNeverTTL: a 400-day-old incident/verify/tool_call still returns its
// payloads.
func TestSpineNeverTTL(t *testing.T) {
	day := time.Date(2025, 8, 12, 9, 0, 0, 0, time.UTC) // ~400 days before the clock
	clk := newFakeClock(day)
	l := compactFixture(t, clk, day, 4, true)
	rp := DefaultRetentionPolicy()
	rp.PayloadTTL = "1s"
	rp.RawKeep = "1s"
	res, err := l.Compact(context.Background(), "2025-08-12", rp)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(l.Root(), res.ToFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"SPINE-INC-PAYLOAD", "SPINE-VERIFY-PAYLOAD", "SPINE-TOOLCALL-PAYLOAD"} {
		if !strings.Contains(string(b), marker) {
			t.Errorf("spine payload %s expired: the AC-6 chain must survive compaction", marker)
		}
	}
	// and Evidence() still resolves on that age of ledger
	var inc string
	err = l.ScanFrom(0, func(rec types.Record) bool {
		if rec.Kind == types.KIncident {
			inc = rec.Inc
			return false
		}
		return true
	})
	if err != nil || inc == "" {
		t.Fatalf("could not find the incident (inc=%q err=%v)", inc, err)
	}
	bundle, err := l.Query().Evidence(inc, 64)
	if err != nil {
		t.Fatalf("Evidence: %v", err)
	}
	if len(bundle.Records) == 0 {
		t.Errorf("Evidence returned no records after compaction")
	}
}

// TestRetentionBlocked: only spine candidates → 009, counts are never
// sacrificed.
func TestRetentionBlocked(t *testing.T) {
	day := time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)
	clk := newFakeClock(day)
	l := testLedger(t, clk)
	inc := types.NewID(types.PInc)
	mustAppend(t, l, types.RecordDraft{
		Kind: types.KIncident, Sig: "journald:sha256v1:0001", Inc: inc,
		Origin: types.Origin{HostID: "7f3a91c2d4e5b607", Source: "journald"},
		Actor:  testActor(), Payload: map[string]any{"state": "recorded"},
	})
	clk.Advance(24 * time.Hour)
	mustAppend(t, l, eventDraft("psi", "psi:sha256v1:next", "aa", 0))

	res, err := l.Compact(context.Background(), "2026-09-16", DefaultRetentionPolicy())
	if err == nil {
		t.Fatalf("compaction of a spine-only day must be refused, got %+v", res)
	}
	if CodeOf(err) != types.CodeLedger009 {
		t.Errorf("err = %v (%s), want %s", err, CodeOf(err), types.CodeLedger009)
	}
	if res.ErrorCode != string(types.CodeLedger009) {
		t.Errorf("result.ErrorCode = %q, want %s", res.ErrorCode, types.CodeLedger009)
	}
}

// TestDiskBudgetEscalation: the escalation still writes records (AC-6), drops
// event payloads, and emits exactly one budget_exceeded record.
func TestDiskBudgetEscalation(t *testing.T) {
	clk := newFakeClock(testNow())
	l := testLedger(t, clk, func(o *Options) {
		o.Retention.DiskBudgetBytes = 1 // any byte on disk is over budget
	})
	mustAppend(t, l, eventDraft("psi", "psi:sha256v1:warm", "01", 0))
	deadline := time.Now().Add(5 * time.Second)
	for !l.w.payloadDegraded() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !l.w.payloadDegraded() {
		t.Fatalf("degraded payload mode never engaged")
	}
	rec := mustAppend(t, l, eventDraft("psi", "psi:sha256v1:after", "02", 0))
	if v, _ := rec.Payload["payload_dropped"].(bool); !v {
		t.Errorf("event payload was not dropped under disk pressure: %+v", rec.Payload)
	}
	if got := rec.Payload["reason"]; got != ReasonDiskBudget {
		t.Errorf("payload.reason = %v, want %s", got, ReasonDiskBudget)
	}
	if rec.Seq == 0 || rec.RecID == "" || rec.Origin.HostID == "" {
		t.Errorf("the record lost its identity under disk pressure: %+v", rec)
	}
	st := l.Status()
	if st.LastSeq < 2 {
		t.Errorf("records must keep being written (LastSeq = %d)", st.LastSeq)
	}
	if n := countOccurrences(t, l, "budget_exceeded"); n != 1 {
		t.Errorf("budget_exceeded records = %d, want exactly 1", n)
	}
	// step (1) of the escalation is compaction of the oldest eligible day
	results, err := l.ApplyRetention(context.Background())
	if err != nil {
		t.Fatalf("ApplyRetention: %v", err)
	}
	_ = results
}

func countOccurrences(t *testing.T, l *Ledger, needle string) int {
	t.Helper()
	parts, _, err := l.authoritativeFiles()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, p := range parts {
		b, err := os.ReadFile(filepath.Join(l.Root(), p.File))
		if err != nil {
			t.Fatal(err)
		}
		n += strings.Count(string(b), needle)
	}
	return n
}

func bytesEqual(a, b []byte) bool { return string(a) == string(b) }
