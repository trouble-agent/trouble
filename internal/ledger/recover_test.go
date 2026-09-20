package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// testLedgerAt opens a ledger over an existing root.
func testLedgerAt(t *testing.T, root string, clk *fakeClock, muts ...func(*Options)) *Ledger {
	t.Helper()
	o := baseTestOptionsAt(t, root, clk)
	for _, m := range muts {
		m(&o)
	}
	l, err := Open(context.Background(), o)
	if err != nil {
		t.Fatalf("Open(%s): %v", root, err)
	}
	t.Cleanup(func() { _ = l.Close(context.Background()) })
	return l
}

func baseTestOptionsAt(t *testing.T, root string, clk *fakeClock) Options {
	o := baseTestOptions(t, clk)
	o.Root = root
	return o
}

func writeLines(t *testing.T, root, name string, lines [][]byte) {
	t.Helper()
	var buf []byte
	for _, ln := range lines {
		buf = append(buf, ln...)
		buf = append(buf, '\n')
	}
	if err := os.WriteFile(filepath.Join(root, name), buf, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	if err := os.Chmod(filepath.Join(root, name), 0o600); err != nil {
		t.Fatalf("chmod %s: %v", name, err)
	}
}

func magicRecord(t *testing.T, seq uint64, day time.Time) []byte {
	t.Helper()
	rec := types.Record{
		Seq: seq, RecID: types.NewID(types.PEv), TS: types.FormatUTC(day), Kind: types.KEvent,
		SchemaVersion: SchemaVersionV1, Sig: fmt.Sprintf("psi:sha256v1:%016x", seq),
		Origin:  types.Origin{HostID: "7f3a91c2d4e5b607", Source: "psi"},
		Actor:   testActor(),
		Payload: map[string]any{"digest": fmt.Sprintf("%064x", seq)},
	}
	b, err := marshalRecordLine(&rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b[:len(b)-1] // strip the trailing newline: the caller writes it
}

// TestTornLastLine: a tail without a terminating \n is excluded, counted, and
// the file gets its \n before the next record (SPEC-01 §5 code 003).
func TestTornLastLine(t *testing.T) {
	root := testRoot(t)
	day := testNow()
	name := day.Format(dayLayout) + ".jsonl"
	writeLines(t, root, name, [][]byte{magicRecord(t, 1, day), magicRecord(t, 2, day)})
	// append a torn fragment: a valid JSON prefix with no newline
	f, err := os.OpenFile(filepath.Join(root, name), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"seq":3,"rec_id":"ev_01J9Z6Q0M2X4T8V1K7B3N5R8`); err != nil {
		t.Fatal(err)
	}
	f.Sync()
	f.Close()

	clk := newFakeClock(day)
	l := testLedgerAt(t, root, clk)
	st := l.IndexStats()
	if st.TornLines != 1 {
		t.Errorf("TornLines = %d, want 1", st.TornLines)
	}
	body, rerr := os.ReadFile(filepath.Join(root, name))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(body) == 0 || body[len(body)-1] != '\n' {
		t.Errorf("the file must end with \\n so the next record starts on a fresh line")
	}
	// exactly the fragment is lost: the two complete records are indexed and the
	// only records after them are the ledger's own boot + recover_torn notes
	if got := l.IndexStats().Entries; got < 2 {
		t.Errorf("Entries = %d, want >= 2 (the complete lines are indexed)", got)
	}
	rec := mustAppend(t, l, eventDraft("psi", "psi:sha256v1:after-torn", "e1", 0))
	if rec.Seq <= 2 {
		t.Errorf("next seq = %d, want > 2: the fragment's seq was never counted", rec.Seq)
	}
	// and the recovery is in the trail
	if !fileContains(t, root, name, `"recover_torn"`) {
		t.Errorf("no lifecycle{op:recover_torn} record was written")
	}
	// A later read sees the healed fragment as interior garbage: it is no longer
	// the tail, so 011 (corrupt) is the correct classification then — the boot
	// that healed it reported 003 (torn). Both are honest; the spec's "exactly
	// one line is dropped" holds either way.
	if rep, err := l.Verify(context.Background(), day.Format(dayLayout)); err != nil {
		t.Fatal(err)
	} else if rep.CorruptLines+rep.TornLines != 1 {
		t.Errorf("Verify flagged %d corrupt + %d torn lines, want exactly 1 lost line",
			rep.CorruptLines, rep.TornLines)
	}
}

// TestTornPrefixKeepsCompleteLines: complete lines before the torn tail survive.
func TestTornPrefixKeepsCompleteLines(t *testing.T) {
	root := testRoot(t)
	day := testNow()
	name := day.Format(dayLayout) + ".jsonl"
	writeLines(t, root, name, [][]byte{magicRecord(t, 1, day), magicRecord(t, 2, day)})
	f, _ := os.OpenFile(filepath.Join(root, name), os.O_WRONLY|os.O_APPEND, 0o600)
	f.WriteString(`{"seq":3,"rec_id"`)
	f.Sync()
	f.Close()

	clk := newFakeClock(day)
	l := testLedgerAt(t, root, clk)
	if got := l.IndexStats().Entries; got < 2 {
		t.Errorf("Entries = %d, want >= 2: complete lines must be indexed", got)
	}
	if got := l.IndexStats().TornLines; got != 1 {
		t.Errorf("TornLines = %d, want 1", got)
	}
	var last uint64
	if err := l.ScanFrom(0, func(rec types.Record) bool {
		if rec.Seq <= last {
			t.Errorf("seq %d after %d", rec.Seq, last)
		}
		last = rec.Seq
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if last < 2 {
		t.Errorf("last seq = %d, want >= 2", last)
	}
}

// TestInteriorCorruption: a line that does not parse but has a \n is counted,
// skipped and reported (011); the seq hole it creates is reported (002).
func TestInteriorCorruption(t *testing.T) {
	root := testRoot(t)
	day := testNow()
	name := day.Format(dayLayout) + ".jsonl"
	writeLines(t, root, name, [][]byte{
		magicRecord(t, 1, day),
		magicRecord(t, 2, day),
		[]byte("{this is not json at all"),
		magicRecord(t, 4, day),
		magicRecord(t, 5, day),
	})
	clk := newFakeClock(day)
	l := testLedgerAt(t, root, clk)
	st := l.IndexStats()
	if st.CorruptLines != 1 {
		t.Errorf("CorruptLines = %d, want 1", st.CorruptLines)
	}
	if got := l.IndexStats().Entries; got != 5 {
		t.Errorf("Entries = %d, want 5: records after the garbage still index", got)
	}
	rep, err := l.Verify(context.Background(), day.Format(dayLayout))
	if err != nil {
		t.Fatal(err)
	}
	if rep.CorruptLines != 1 {
		t.Errorf("Verify CorruptLines = %d, want 1", rep.CorruptLines)
	}
	if len(rep.SeqHoles) != 1 || rep.SeqHoles[0].From != 3 || rep.SeqHoles[0].To != 3 {
		t.Errorf("SeqHoles = %+v, want one hole {3,3} (TROUBLE-LEDGER-002)", rep.SeqHoles)
	}
	if rep.OK {
		t.Errorf("VerifyReport.OK must be false when findings exist")
	}
}

// TestNewerSchemaSkipped: schema_version above MaxSchema is skipped and counted,
// never fatal (004).
func TestNewerSchemaSkipped(t *testing.T) {
	root := testRoot(t)
	day := testNow()
	name := day.Format(dayLayout) + ".jsonl"
	rec := types.Record{
		Seq: 2, RecID: types.NewID(types.PEv), TS: types.FormatUTC(day), Kind: types.KEvent,
		SchemaVersion: 2, Sig: "psi:sha256v1:ffff",
		Origin:  types.Origin{HostID: "h", Source: "psi"},
		Actor:   testActor(),
		Payload: map[string]any{"digest": "aa"},
	}
	b, _ := marshalRecordLine(&rec)
	writeLines(t, root, name, [][]byte{magicRecord(t, 1, day), b[:len(b)-1]})

	clk := newFakeClock(day)
	l := testLedgerAt(t, root, clk)
	if got := l.IndexStats().SkippedNewerSchema; got != 1 {
		t.Errorf("SkippedNewerSchema = %d, want 1", got)
	}
	// the daemon stays up and keeps writing
	if _, err := l.Append(context.Background(), eventDraft("psi", "psi:sha256v1:ok", "bb", 0)); err != nil {
		t.Fatalf("Append after a newer-schema line: %v", err)
	}
}

// TestSeqRecoveryFromHeadHint: HEAD is a hint; the max rule wins.
func TestSeqRecoveryFromHeadHint(t *testing.T) {
	day := testNow()
	names := day.Format(dayLayout) + ".jsonl"
	cases := []struct {
		name    string
		head    string
		wantMin uint64
	}{
		{"missing HEAD", "", 3},
		{"stale HEAD", `{"last_seq":1,"day":"` + day.Format(dayLayout) + `","gen":0,"part":1}`, 3},
		{"HEAD ahead", `{"last_seq":99,"day":"` + day.Format(dayLayout) + `","gen":0,"part":1}`, 99},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := testRoot(t)
			writeLines(t, root, names, [][]byte{
				magicRecord(t, 1, day), magicRecord(t, 2, day), magicRecord(t, 3, day),
			})
			if c.head != "" {
				if err := os.WriteFile(filepath.Join(root, "HEAD"), []byte(c.head+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			clk := newFakeClock(day)
			l := testLedgerAt(t, root, clk)
			st := l.Status()
			if st.LastSeqPending < c.wantMin {
				t.Errorf("LastSeqPending = %d, want >= %d (max(HEAD, file) rule)", st.LastSeqPending, c.wantMin)
			}
			rec := mustAppend(t, l, eventDraft("psi", "psi:sha256v1:after", "cc", 0))
			if rec.Seq <= c.wantMin {
				t.Errorf("next seq = %d, want > %d: the allocator never goes backwards", rec.Seq, c.wantMin)
			}
			if rec.Seq != st.LastSeqPending+1 {
				t.Errorf("next seq = %d, want LastSeqPending+1 = %d", rec.Seq, st.LastSeqPending+1)
			}
		})
	}
}

// TestCompactionResume: gen 0 and gen 1 present together is an interrupted
// compaction commit; the higher generation wins and the resume is recorded.
func TestCompactionResume(t *testing.T) {
	root := testRoot(t)
	day := testNow()
	d := day.Format(dayLayout)
	// gen 1 is authoritative: three records; gen 0 is the leftover with two
	writeLines(t, root, d+".jsonl", [][]byte{magicRecord(t, 1, day), magicRecord(t, 2, day)})
	writeLines(t, root, d+".1.gen.jsonl", [][]byte{
		magicRecord(t, 1, day), magicRecord(t, 2, day), magicRecord(t, 3, day),
	})
	clk := newFakeClock(day)
	l := testLedgerAt(t, root, clk)

	if _, err := os.Stat(filepath.Join(root, d+".jsonl")); !os.IsNotExist(err) {
		t.Errorf("the lower generation must be unlinked at boot (err=%v)", err)
	}
	if got := l.Status().LastSeq; got < 3 {
		t.Errorf("LastSeq = %d, want >= 3 (gen 1 is authoritative)", got)
	}
	body, _ := os.ReadFile(filepath.Join(root, d+".1.gen.jsonl"))
	if !containsFold(string(body), "compaction_resume") {
		// the resume record is written to the live file, not the generation
		live, _ := os.ReadFile(filepath.Join(root, d+".1.gen.jsonl"))
		_ = live
	}
	found := false
	for _, de := range l.idx.dayEntries() {
		for _, p := range de.Parts {
			b, _ := os.ReadFile(filepath.Join(root, p.File))
			if containsFold(string(b), "compaction_resume") {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("no lifecycle{op:compaction_resume} record was written")
	}
}

func fileContains(t *testing.T, root, name, needle string) bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		return false
	}
	return containsFold(string(b), needle)
}

func containsFold(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) > 0 && indexOf(haystack, needle) >= 0
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}

// TestHeadIsWrittenAndParsable keeps the HEAD hint honest.
func TestHeadIsWrittenAndParsable(t *testing.T) {
	clk := newFakeClock(testNow())
	l := testLedger(t, clk)
	appended := mustAppend(t, l, eventDraft("psi", "psi:sha256v1:head", "dd", 0))
	// HEAD is a hint refreshed at most once per second (it must not sit on the
	// per-batch latency path), so the watermark may lag the record by a moment.
	deadline := time.Now().Add(3 * time.Second)
	var h headFile
	for {
		b, err := os.ReadFile(filepath.Join(l.Root(), "HEAD"))
		if err != nil {
			t.Fatalf("HEAD: %v", err)
		}
		if err := json.Unmarshal(b, &h); err != nil {
			t.Fatalf("HEAD is not valid JSON: %v", err)
		}
		if h.LastSeq == appended.Seq || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if h.LastSeq != appended.Seq {
		t.Errorf("HEAD.last_seq = %d, want %d (the durable watermark)", h.LastSeq, appended.Seq)
	}
	if h.Day != testNow().Format(dayLayout) {
		t.Errorf("HEAD.day = %s, want %s", h.Day, testNow().Format(dayLayout))
	}
}

// TestLockIsSingleWriter: a second Open on the same root is refused (005).
func TestLockIsSingleWriter(t *testing.T) {
	clk := newFakeClock(testNow())
	l := testLedger(t, clk)
	o := baseTestOptionsAt(t, l.Root(), clk)
	if _, err := Open(context.Background(), o); err == nil {
		t.Fatalf("second Open succeeded; the single-writer lock is not enforced")
	} else if CodeOf(err) != types.CodeLedger005 {
		t.Fatalf("second Open error = %v (%s), want %s", err, CodeOf(err), types.CodeLedger005)
	}
}

// TestRootModeAndTmpRefusal: 012 for /tmp and for a non-0700 root.
func TestRootModeAndTmpRefusal(t *testing.T) {
	clk := newFakeClock(testNow())
	o := baseTestOptions(t, clk)
	o.Root = "/tmp/trouble-ledger-should-refuse"
	if _, err := Open(context.Background(), o); CodeOf(err) != types.CodeLedger012 {
		t.Errorf("Open on /tmp: err = %v (%s), want %s", err, CodeOf(err), types.CodeLedger012)
	}
	root := testRoot(t)
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	o2 := baseTestOptionsAt(t, root, clk)
	if _, err := Open(context.Background(), o2); CodeOf(err) != types.CodeLedger012 {
		t.Errorf("Open on mode 0755: err = %v (%s), want %s", err, CodeOf(err), types.CodeLedger012)
	}
}

// TestAppendValidationRefusals pins the 001/validation reasons.
func TestAppendValidationRefusals(t *testing.T) {
	clk := newFakeClock(testNow())
	l := testLedger(t, clk)
	cases := []struct {
		name  string
		draft types.RecordDraft
		why   string
	}{
		{"unknown kind", types.RecordDraft{
			Kind: "nope", Origin: types.Origin{HostID: "h", Source: "s"}, Actor: testActor(),
		}, "unknown kind"},
		{"empty origin", types.RecordDraft{
			Kind: types.KEvent, Sig: "psi:sha256v1:x", Actor: testActor(),
		}, "empty origin"},
		{"empty actor", types.RecordDraft{
			Kind: types.KEvent, Sig: "psi:sha256v1:x", Origin: types.Origin{HostID: "h", Source: "s"},
		}, "empty actor"},
		{"sig required", types.RecordDraft{
			Kind: types.KEvent, Origin: types.Origin{HostID: "h", Source: "s"}, Actor: testActor(),
		}, "sig required"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := l.Append(context.Background(), c.draft)
			if CodeOf(err) != types.CodeLedger001 {
				t.Fatalf("err = %v (%s), want %s", err, CodeOf(err), types.CodeLedger001)
			}
			if ReasonOf(err) != ReasonValidation {
				t.Errorf("reason = %q, want %q", ReasonOf(err), ReasonValidation)
			}
		})
	}
}

// TestOversizePayloadRefusedOrTruncated: the spine is refused, the high-volume
// kinds are truncated with flags (§3.1).
func TestOversizePayloadRefusedOrTruncated(t *testing.T) {
	clk := newFakeClock(testNow())
	l := testLedger(t, clk, func(o *Options) { o.Rotation.MaxRecordBytes = 512 })
	big := map[string]any{}
	for i := 0; i < 100; i++ {
		big[fmt.Sprintf("k%02d", i)] = "0123456789"
	}
	_, err := l.Append(context.Background(), types.RecordDraft{
		Kind: types.KIncident, Sig: "psi:sha256v1:x", Inc: "inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE",
		Origin: types.Origin{HostID: "h", Source: "s"}, Actor: testActor(), Payload: big,
	})
	if CodeOf(err) != types.CodeLedger001 || ReasonOf(err) != ReasonValidation {
		t.Fatalf("spine oversize: err = %v (%s/%s), want 001/validation", err, CodeOf(err), ReasonOf(err))
	}
	rec := mustAppend(t, l, types.RecordDraft{
		Kind: types.KEvent, Sig: "psi:sha256v1:y",
		Origin: types.Origin{HostID: "h", Source: "s"}, Actor: testActor(), Payload: big,
	})
	if v, _ := rec.Payload["truncated"].(bool); !v {
		t.Errorf("event payload was not truncated: %+v", rec.Payload)
	}
	if _, ok := rec.Payload["truncated_bytes"]; !ok {
		t.Errorf("truncation must be counted in payload.truncated_bytes")
	}
}

// TestPayloadHashRecordedForLargePayloads: payload_sha256 is mandatory at ≥4 KiB.
func TestPayloadHashRecordedForLargePayloads(t *testing.T) {
	clk := newFakeClock(testNow())
	l := testLedger(t, clk, func(o *Options) { o.Rotation.MaxRecordBytes = 1 << 20 })
	pad := make([]byte, 5000)
	for i := range pad {
		pad[i] = 'q'
	}
	rec := mustAppend(t, l, types.RecordDraft{
		Kind: types.KEvent, Sig: "psi:sha256v1:big",
		Origin: types.Origin{HostID: "h", Source: "s"}, Actor: testActor(),
		Payload: map[string]any{"pad": string(pad)},
	})
	want := payloadHash(map[string]any{"pad": string(pad)})
	if got, _ := rec.Payload["payload_sha256"].(string); got != want {
		t.Errorf("payload_sha256 = %q, want %q", got, want)
	}
	rep, err := l.Verify(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.PayloadHashMismatches != 0 {
		t.Errorf("PayloadHashMismatches = %d, want 0", rep.PayloadHashMismatches)
	}
}
