package ledger

// unmeasured_status_test.go — the §3.10b rule that a measurement a status path
// cannot take is ABSENT (and says why), never a zero, and the §2.2 rule that a
// clean verification says "clean" with an empty array rather than `null`
// (TRBL-130).
//
// The two defects these pin were both "a value that reads as a fact":
//
//   - `trouble ledger status --json` answered `fsync_calls: 0` for a ledger that
//     was actively fdatasync-ing, because the CLI takes the lock-free read-only
//     walk and that walk has no writer counter to report. 0 is a MEASUREMENT;
//     the truth was that the path never measured anything.
//   - `trouble ledger verify --json` answered `seq_holes: null` for a healthy
//     ledger, because a nil slice marshals to null — so "clean" and "not
//     computed" produced the same document.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestReadStatusOmitsWriterSeatCounters pins the read-only side: the counters
// that live in the writer process are absent from the read-only status and named
// in `unmeasured` with a reason a caller can act on, while the measurements the
// walk CAN take are present and real.
func TestReadStatusOmitsWriterSeatCounters(t *testing.T) {
	clk := newFakeClock(testNow())
	l := testLedger(t, clk)
	for i := 0; i < 2; i++ {
		mustAppend(t, l, eventDraft("psi", "psi:sha256v1:unmeasured000"+string(rune('a'+i)), "deadbeef", 0))
	}

	// The live writer seat measures its own counters — that is what makes their
	// absence on the other path a statement about the PATH, not the ledger.
	live := l.Status()
	if got, ok := live.FsyncCount(); !ok || got == 0 {
		t.Fatalf("live fsync_calls = (%d, measured=%t), want a measured non-zero count after 2 durable appends", got, ok)
	}
	if _, ok := live.FsyncRate(); !ok {
		t.Errorf("live fsync_per_record is absent on a ledger with %d records; the writer seat measures both counters", live.Records)
	}

	ro, err := ReadStatus(l.Root())
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	if ro.Records == 0 {
		t.Fatalf("read-only status reported no records: the walk did not read the day file, so the rest of this test is vacuous")
	}
	for name, measured := range map[string]bool{
		"fsync_calls":        ro.FsyncCalls != nil,
		"fsync_per_record":   ro.FsyncPerRecord != nil,
		"queue_depth":        ro.QueueDepth != nil,
		"last_seq_pending":   ro.LastSeqPending != nil,
		"backpressure_total": ro.BackpressureTotal != nil,
	} {
		if measured {
			t.Errorf("read-only status reported %s as a value; it is a writer-seat counter and this path cannot measure it", name)
		}
		if got := ro.Unmeasured[name]; got != "in_process_counter" {
			t.Errorf("unmeasured[%s] = %q, want %q: an absent measurement must carry its reason", name, got, "in_process_counter")
		}
	}

	// The measurements the walk DOES take stay real: the stall is last_ts
	// against the clock, and the refusal folds are a property of the files.
	if _, ok := ro.StallSeconds(); !ok {
		t.Errorf("read-only stall_s is absent on a ledger with a watermark; it is derivable and must be measured, not dropped")
	}
	if ro.Records != live.Records {
		t.Errorf("read-only records = %d, live = %d: both paths must agree on what the ledger holds", ro.Records, live.Records)
	}

	// The JSON is the contract: the absent keys are ABSENT, and the reasons are
	// readable, so a script cannot mistake "unmeasured" for 0.
	roJSON := mustMarshal(t, ro)
	roKeys := jsonKeys(t, roJSON)
	for _, key := range []string{"fsync_calls", "fsync_per_record", "queue_depth", "last_seq_pending", "backpressure_total"} {
		if raw, present := roKeys[key]; present {
			t.Errorf("read-only status JSON carries %s=%s: an unmeasured counter must be omitted, not reported as a value", key, raw)
		}
	}
	if !strings.Contains(roJSON, `"unmeasured"`) || !strings.Contains(roJSON, `"in_process_counter"`) {
		t.Errorf("read-only status JSON does not publish the unmeasured reasons:\n%s", roJSON)
	}
	if _, present := roKeys["stall_s"]; !present {
		t.Errorf("read-only status JSON dropped the measured stall:\n%s", roJSON)
	}
	// The one rejected substitute for absence: a fabricated zero that happens to
	// be spelled the same way as a real measurement.
	if strings.Contains(roJSON, `"fsync_calls":0`) {
		t.Errorf("read-only status JSON still reports a zero fsync count:\n%s", roJSON)
	}

	// The live path keeps the keys: measuring everything is what "the writer
	// seat" means, and a caller must be able to tell the two documents apart.
	liveJSON := mustMarshal(t, live)
	liveKeys := jsonKeys(t, liveJSON)
	for _, key := range []string{"fsync_calls", "fsync_per_record", "queue_depth", "last_seq_pending", "backpressure_total"} {
		if _, present := liveKeys[key]; !present {
			t.Errorf("live status JSON omitted %s, which the writer seat does measure\n%s", key, liveJSON)
		}
	}
	if strings.Contains(liveJSON, `"unmeasured"`) {
		t.Errorf("live status JSON names unmeasured members, but the writer seat measured everything:\n%s", liveJSON)
	}
}

// TestLiveStatusMarksRateAsNoRecords pins the one absence the WRITER seat has:
// on a ledger with no records the per-record fsync rate has no denominator, so it
// is absent with a reason instead of a 0 that reads as "no fsyncs". An empty
// ledger also has no watermark to be stalled since, on either path.
func TestLiveStatusMarksRateAsNoRecords(t *testing.T) {
	clk := newFakeClock(testNow())
	l := testLedger(t, clk)
	// Open always writes one boot lifecycle record, so reach the "no records"
	// writer state the way the backpressure tests reach a fresh queue: retire the
	// boot writer and install an empty one (the ledger's structured state — root,
	// options, index, recovery stamp — is all a writer needs).
	if err := l.w.stop(); err != nil {
		t.Fatalf("stopping the boot writer: %v", err)
	}
	l.w = newWriter(l)
	l.w.start()

	st := l.Status()
	if st.Records != 0 {
		t.Fatalf("fresh writer reports %d records, want 0 (this test needs an empty ledger)", st.Records)
	}
	if _, ok := st.FsyncRate(); ok {
		t.Errorf("fsync_per_record = %v on a ledger with no records: the rate has no denominator, so it must be absent", *st.FsyncPerRecord)
	}
	if got := st.Unmeasured["fsync_per_record"]; got != "no_records" {
		t.Errorf("unmeasured[fsync_per_record] = %q, want %q", got, "no_records")
	}
	// The counter itself IS measured here (the writer counts its own fsyncs), so
	// its absence would be a different bug.
	if _, ok := st.FsyncCount(); !ok {
		t.Errorf("live fsync_calls is absent on an empty ledger; the writer always counts its own fsyncs")
	}
	if _, ok := st.StallSeconds(); ok {
		t.Errorf("stall_s is present on a ledger with no watermark (no_records is the honest answer)")
	}
	if got := st.Unmeasured["stall_s"]; got != "no_records" {
		t.Errorf("unmeasured[stall_s] = %q, want %q", got, "no_records")
	}

	// The same two absences hold on the read-only path for a root holding no
	// records, which is where a fresh state directory really lands.
	ro, err := ReadStatus(testRoot(t))
	if err != nil {
		t.Fatalf("ReadStatus on an empty root: %v", err)
	}
	if _, ok := ro.StallSeconds(); ok {
		t.Errorf("read-only stall_s is present on an empty ledger")
	}
	if got := ro.Unmeasured["stall_s"]; got != "no_records" {
		t.Errorf("read-only unmeasured[stall_s] = %q, want %q", got, "no_records")
	}
}

// TestVerifyReportCollectionsAreAlwaysArrays pins the second defect on BOTH
// verify paths: a clean walk marshals `seq_holes` (and the other collections) as
// an empty ARRAY, never null, and a walk with findings still marshals an array.
func TestVerifyReportCollectionsAreAlwaysArrays(t *testing.T) {
	clk := newFakeClock(testNow())
	l := testLedger(t, clk)
	for i := 0; i < 3; i++ {
		mustAppend(t, l, eventDraft("psi", "psi:sha256v1:cleanarray000"+string(rune('a'+i)), "deadbeef", 0))
	}

	t.Run("read-only clean", func(t *testing.T) {
		rep, err := ReadVerify(l.Root(), "")
		if err != nil {
			t.Fatalf("ReadVerify: %v", err)
		}
		assertCollectionsAreArrays(t, rep, 0)
	})

	t.Run("writer clean", func(t *testing.T) {
		rep, err := l.Verify(context.Background(), "")
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		assertCollectionsAreArrays(t, rep, 0)
	})

	t.Run("read-only with a hole", func(t *testing.T) {
		// Two VALID records with a gap in the seq space (TRBL-INT-CI-2: a torn
		// fixture is healed by recovery, which would make this cell vacuous).
		root := testRoot(t)
		day := testNow()
		names := day.Format(dayLayout) + ".jsonl"
		writeLines(t, root, names, [][]byte{
			magicRecord(t, 1, day),
			magicRecord(t, 3, day),
		})
		rep, err := ReadVerify(root, "")
		if err != nil {
			t.Fatalf("ReadVerify: %v", err)
		}
		assertCollectionsAreArrays(t, rep, 1)
	})
}

// assertCollectionsAreArrays checks the shape rule on one report: every
// collection member is non-nil, the hole count is what the walk found, and the
// JSON says `[]` where there is nothing to say.
func assertCollectionsAreArrays(t *testing.T, rep VerifyReport, wantHoles int) {
	t.Helper()
	if rep.SeqHoles == nil {
		t.Fatalf("SeqHoles is nil: a nil slice marshals to null, so 'clean' and 'not computed' read alike")
	}
	if rep.Files == nil {
		t.Fatalf("Files is nil on a walk that read at least one file")
	}
	if rep.Errors == nil {
		t.Fatalf("Errors is nil: a clean walk must say [] rather than null")
	}
	if len(rep.SeqHoles) != wantHoles {
		t.Fatalf("SeqHoles = %+v, want %d hole(s)", rep.SeqHoles, wantHoles)
	}
	out := mustMarshal(t, rep)
	if strings.Contains(out, `"seq_holes":null`) || strings.Contains(out, `"errors":null`) || strings.Contains(out, `"files":null`) {
		t.Fatalf("verify report carries a null collection:\n%s", out)
	}
	if wantHoles == 0 && !strings.Contains(out, `"seq_holes":[]`) {
		t.Fatalf("clean verify report does not say \"seq_holes\": []:\n%s", out)
	}
	if wantHoles > 0 && !strings.Contains(out, `"seq_holes":[{`) {
		t.Fatalf("verify report with a hole does not carry a seq_holes array:\n%s", out)
	}
}

// mustMarshal is the wire form under test: the same encoding/json the CLI uses.
func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	return string(b)
}

// jsonKeys returns the TOP-LEVEL members of a JSON object. It exists so a test
// can say "this key is absent" without tripping over the same word appearing as a
// value elsewhere in the document (the `unmeasured` reasons name the fields).
func jsonKeys(t *testing.T, body string) map[string]json.RawMessage {
	t.Helper()
	var keys map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &keys); err != nil {
		t.Fatalf("unmarshal status JSON: %v\n%s", err, body)
	}
	return keys
}
