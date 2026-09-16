package sentinel

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"os"
	"testing"
)

// TestSpoolRoundTrip pins the spool frame: append, peek, drain, and the CRC32C +
// length prefix that makes a torn write detectable.
func TestSpoolRoundTrip(t *testing.T) {
	dir := t.TempDir()
	sp, err := OpenSpool(dir, 1<<20)
	if err != nil {
		t.Fatalf("OpenSpool: %v", err)
	}
	defer sp.Close()

	for i := 0; i < 5; i++ {
		if _, err := sp.Append(SpoolEntry{
			Project: "1", SourceKind: sourceEnvelope, AuthForm: fmtXSentryAuth,
			TS: "2026-09-16T09:14:03.221Z", ItemType: "event",
			Raw: []byte(fmt.Sprintf(`{"event_id":"%032x","message":"m%d"}`, i, i)),
		}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if sp.Len() != 5 {
		t.Fatalf("Len = %d, want 5", sp.Len())
	}
	peeked, err := sp.Peek(2)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(peeked) != 2 || sp.Len() != 5 {
		t.Fatalf("Peek(2) returned %d entries and left %d (Peek must not consume)", len(peeked), sp.Len())
	}
	if peeked[0].Seq != 1 || peeked[1].Seq != 2 {
		t.Fatalf("sequence numbers = %d,%d, want 1,2 (oldest first)", peeked[0].Seq, peeked[1].Seq)
	}
	drained, err := sp.Drain(3)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(drained) != 3 || sp.Len() != 2 {
		t.Fatalf("Drain(3) returned %d and left %d", len(drained), sp.Len())
	}
	// Re-opening re-indexes the survivors, so a restart resumes the spool.
	if err := sp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	sp2, err := OpenSpool(dir, 1<<20)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer sp2.Close()
	if sp2.Len() != 2 {
		t.Fatalf("reopened spool holds %d entries, want 2", sp2.Len())
	}
	rest, err := sp2.Peek(0)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if rest[0].Seq != 4 {
		t.Fatalf("first surviving entry seq = %d, want 4", rest[0].Seq)
	}
}

// TestSpoolTornEntryIsDetected pins §6.12: a crash mid-write is detectable, the
// entry is discarded, and nothing else is lost.
func TestSpoolTornEntryIsDetected(t *testing.T) {
	dir := t.TempDir()
	sp, err := OpenSpool(dir, 1<<20)
	if err != nil {
		t.Fatalf("OpenSpool: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := sp.Append(SpoolEntry{Project: "1", Raw: []byte(fmt.Sprintf(`{"n":%d}`, i))}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	path := sp.Path()
	if err := sp.Close(); err != nil {
		t.Fatal(err)
	}
	// Truncate the file in the middle of the last frame.
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, st.Size()-5); err != nil {
		t.Fatal(err)
	}
	sp2, err := OpenSpool(dir, 1<<20)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer sp2.Close()
	_, _, entries, torn, _ := sp2.Stats()
	if torn == 0 {
		t.Fatal("a truncated frame must be counted as torn")
	}
	if entries != 2 {
		t.Fatalf("indexed %d entries, want the 2 intact ones", entries)
	}
	drained, err := sp2.Drain(0)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(drained) != 2 {
		t.Fatalf("Drain returned %d entries, want 2 (torn entries are discarded)", len(drained))
	}
}

// TestSpoolCRCMismatchIsDetected pins the CRC half of the frame: a corrupted body
// is dropped rather than replayed.
func TestSpoolCRCMismatchIsDetected(t *testing.T) {
	dir := t.TempDir()
	sp, err := OpenSpool(dir, 1<<20)
	if err != nil {
		t.Fatalf("OpenSpool: %v", err)
	}
	if _, err := sp.Append(SpoolEntry{Project: "1", Raw: []byte(`{"n":1}`)}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	path := sp.Path()
	if err := sp.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Flip a byte inside the body, leaving the length and CRC as they were.
	raw[len(raw)-2] ^= 0xff
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	sp2, err := OpenSpool(dir, 1<<20)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer sp2.Close()
	_, _, entries, torn, _ := sp2.Stats()
	if entries != 0 || torn == 0 {
		t.Fatalf("entries=%d torn=%d, want the corrupted frame dropped (0 entries, 1 torn)", entries, torn)
	}
}

// TestSpoolDropOldest pins §3.9's drop-oldest behaviour and its accounting.
func TestSpoolDropOldest(t *testing.T) {
	dir := t.TempDir()
	// Room for roughly three entries.
	entry := SpoolEntry{Project: "1", Raw: []byte(`{"event_id":"0123456789abcdef0123456789abcdef","message":"a"}`)}
	encoded := int64(len(mustJSON(t, entry))) + spoolEntryHeader
	sp, err := OpenSpool(dir, encoded*3)
	if err != nil {
		t.Fatalf("OpenSpool: %v", err)
	}
	defer sp.Close()
	totalDropped := 0
	for i := 0; i < 8; i++ {
		e := entry
		e.Raw = []byte(fmt.Sprintf(`{"event_id":"%032x","message":"a"}`, i+1))
		dropped, err := sp.Append(e)
		if err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
		totalDropped += dropped
	}
	if totalDropped == 0 {
		t.Fatal("the spool never dropped an oldest entry while over budget")
	}
	bytes, budget, entries, _, dropped := sp.Stats()
	if bytes > budget {
		t.Fatalf("spool holds %d bytes over a %d budget", bytes, budget)
	}
	if dropped != uint64(totalDropped) {
		t.Fatalf("dropped counter = %d, want %d", dropped, totalDropped)
	}
	if entries == 0 {
		t.Fatal("drop-oldest removed everything")
	}
	// The survivors are the newest ones.
	kept, err := sp.Peek(0)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if kept[len(kept)-1].Seq != 8 {
		t.Fatalf("last surviving entry seq = %d, want 8 (the newest)", kept[len(kept)-1].Seq)
	}
}

// TestSpoolBudgetAccounting pins `spool_bytes <= budget` and the SpoolStats view.
func TestSpoolBudgetAccounting(t *testing.T) {
	ts := newTestServer(t, func(c *Config) {
		c.CanaryProject = ""
		c.SpoolBudgetBytes = 4096
	})
	defer ts.close()
	st := ts.s.SpoolStats()
	if st.BudgetBytes != 4096 {
		t.Fatalf("budget = %d, want 4096", st.BudgetBytes)
	}
	if st.Bytes > st.BudgetBytes {
		t.Fatalf("spool bytes %d exceed the budget %d", st.Bytes, st.BudgetBytes)
	}
	if _, err := ts.s.spool.Append(SpoolEntry{Project: "1", Raw: []byte(`{"n":1}`)}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	st = ts.s.SpoolStats()
	if st.Entries != 1 || st.Bytes == 0 {
		t.Fatalf("SpoolStats = %+v, want one entry with bytes accounted", st)
	}
}

// TestSpoolEntryFrameLayout pins the on-disk frame so a future reader can parse
// what this writer produced (seq, length, crc32c, body — all little-endian).
func TestSpoolEntryFrameLayout(t *testing.T) {
	dir := t.TempDir()
	sp, err := OpenSpool(dir, 1<<20)
	if err != nil {
		t.Fatalf("OpenSpool: %v", err)
	}
	entry := SpoolEntry{Project: "7", SourceKind: sourceEnvelope, AuthForm: fmtQueryKey, Raw: []byte(`{"n":1}`)}
	if _, err := sp.Append(entry); err != nil {
		t.Fatalf("Append: %v", err)
	}
	path := sp.Path()
	if err := sp.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < spoolEntryHeader {
		t.Fatal("frame is shorter than its header")
	}
	seq := binary.LittleEndian.Uint64(raw[0:8])
	length := binary.LittleEndian.Uint32(raw[8:12])
	crc := binary.LittleEndian.Uint32(raw[12:16])
	if seq != 1 {
		t.Errorf("seq = %d, want 1", seq)
	}
	if int(length) != len(raw)-spoolEntryHeader {
		t.Errorf("length prefix = %d, want %d", length, len(raw)-spoolEntryHeader)
	}
	if crc != crc32.Checksum(raw[spoolEntryHeader:], crcTable) {
		t.Error("CRC32C does not cover the body")
	}
	// The body is the entry's JSON, so a reader needs no second format.
	var back SpoolEntry
	if err := jsonUnmarshal(raw[spoolEntryHeader:], &back); err != nil {
		t.Fatalf("body is not the entry JSON: %v", err)
	}
	if back.Project != "7" || back.AuthForm != fmtQueryKey {
		t.Fatalf("round-tripped entry = %+v", back)
	}
}

// mustJSON marshals a test value.
func mustJSON(tb testing.TB, v any) []byte {
	tb.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		tb.Fatalf("marshal: %v", err)
	}
	return b
}

// jsonUnmarshal unmarshals a test value.
func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
