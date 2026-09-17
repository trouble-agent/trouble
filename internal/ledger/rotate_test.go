package ledger

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// TestMidnightRotation: seq stays strictly increasing across the boundary and
// each day gets its own file.
func TestMidnightRotation(t *testing.T) {
	day1 := time.Date(2026, 9, 16, 23, 59, 50, 0, time.UTC)
	clk := newFakeClock(day1)
	l := testLedger(t, clk)
	r1 := mustAppend(t, l, eventDraft("psi", "psi:sha256v1:a", "01", 0))
	if got := l.Status().File; got != "2026-09-16.jsonl" {
		t.Fatalf("day-1 file = %s", got)
	}
	clk.Set(day1.Add(20 * time.Second)) // past 00:00:00Z
	r2 := mustAppend(t, l, eventDraft("psi", "psi:sha256v1:b", "02", 0))
	if r2.Seq <= r1.Seq {
		t.Errorf("seq across the boundary: %d then %d, want strictly increasing", r1.Seq, r2.Seq)
	}
	st := l.Status()
	if st.Day != "2026-09-17" || st.File != "2026-09-17.jsonl" {
		t.Errorf("after midnight the file is %s (day %s), want 2026-09-17.jsonl", st.File, st.Day)
	}
	if _, err := os.Stat(filepath.Join(l.Root(), "2026-09-16.jsonl")); err != nil {
		t.Errorf("the day-1 file disappeared: %v", err)
	}
	// one file per day: no line of day 1 carries a day-2 ts and vice versa
	for _, f := range []string{"2026-09-16.jsonl", "2026-09-17.jsonl"} {
		for i, ln := range readFileLines(t, l.Root(), f) {
			rec, err := decodeRecord(ln, true)
			if err != nil {
				t.Fatalf("%s line %d: %v", f, i, err)
			}
			if want := f[:10]; rec.TS[:10] != want {
				t.Errorf("%s carries a record with ts %s", f, rec.TS)
			}
		}
	}
}

// TestBatchNeverSpansFiles: a batch's records all land in the file open at
// batch start, so per-file seq is strictly increasing and no line is split.
func TestBatchNeverSpansFiles(t *testing.T) {
	day1 := time.Date(2026, 9, 16, 23, 59, 59, 0, time.UTC)
	clk := newFakeClock(day1)
	l := testLedger(t, clk, func(o *Options) { o.Rotation.MaxBatchRecords = 8 })
	// one batch: 4 concurrent producers
	el := runAppends(t, l, 4, 4)
	_ = el
	clk.Advance(time.Second)
	runAppends(t, l, 4, 4)

	var prevLast uint64
	for _, f := range []string{"2026-09-16.jsonl", "2026-09-17.jsonl"} {
		var last uint64
		for i, ln := range readFileLines(t, l.Root(), f) {
			if len(ln) == 0 || ln[len(ln)-1] == '\n' {
				t.Fatalf("%s line %d is not a complete line", f, i)
			}
			rec, err := decodeRecord(ln, true)
			if err != nil {
				t.Fatalf("%s line %d: %v", f, i, err)
			}
			if rec.Seq <= last {
				t.Errorf("%s: seq %d after %d: per-file seq must increase", f, rec.Seq, last)
			}
			last = rec.Seq
			if prevLast != 0 && rec.Seq <= prevLast {
				t.Errorf("%s: seq %d does not continue from the previous file (last %d)", f, rec.Seq, prevLast)
			}
		}
		prevLast = last
	}
}

// TestPartRollover: rotate_max_bytes small enough to force D.p02.jsonl, and the
// part order is preserved on read.
func TestPartRollover(t *testing.T) {
	clk := newFakeClock(testNow())
	l := testLedger(t, clk, func(o *Options) { o.Rotation.MaxBytes = 900 })
	const n = 12
	for i := 0; i < n; i++ {
		mustAppend(t, l, eventDraft("psi", fmt.Sprintf("psi:sha256v1:%04d", i),
			fmt.Sprintf("%064x", i), 0))
	}
	d := testNow().Format(dayLayout)
	if _, err := os.Stat(filepath.Join(l.Root(), d+".p02.jsonl")); err != nil {
		t.Fatalf("no part 02 after exceeding rotate_max_bytes: %v", err)
	}
	// part order preserved: seqs strictly increase when walking the day
	var last uint64
	count := 0
	if err := l.ScanFrom(0, func(rec types.Record) bool {
		if rec.Seq <= last {
			t.Errorf("ScanFrom yielded seq %d after %d", rec.Seq, last)
		}
		last = rec.Seq
		count++
		return true
	}); err != nil {
		t.Fatal(err)
	}
	// n appends + the boot record + one rotate note per part rollover
	if count < n+1 {
		t.Errorf("ScanFrom walked %d records, want >= %d", count, n+1)
	}
}

// TestReaderAcrossRotation: a reader following by seq crosses the boundary.
func TestReaderAcrossRotation(t *testing.T) {
	day1 := time.Date(2026, 9, 16, 23, 59, 59, 0, time.UTC)
	clk := newFakeClock(day1)
	l := testLedger(t, clk)
	mustAppend(t, l, eventDraft("psi", "psi:sha256v1:1", "01", 0))
	mustAppend(t, l, eventDraft("psi", "psi:sha256v1:2", "02", 0))

	f, err := os.Open(filepath.Join(l.Root(), "2026-09-16.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	clk.Set(day1.Add(time.Minute))
	mustAppend(t, l, eventDraft("psi", "psi:sha256v1:3", "03", 0))

	// the held fd reads to EOF without error
	rd := newLineReader(f)
	lines := 0
	for {
		line, _, complete, rerr := rd.next()
		if rerr != nil {
			t.Fatalf("held fd read: %v", rerr)
		}
		if len(line) == 0 && !complete {
			break
		}
		lines++
	}
	if lines == 0 {
		t.Fatalf("held fd read nothing")
	}
	// a new reader resolves the newest seq in the new file
	st := l.Status()
	name, _, err := l.Resolve(st.LastSeq)
	if err != nil {
		t.Fatalf("Resolve(%d): %v", st.LastSeq, err)
	}
	if name != "2026-09-17.jsonl" {
		t.Errorf("Resolve(%d) = %s, want 2026-09-17.jsonl", st.LastSeq, name)
	}
	var seen []uint64
	if err := l.ScanFrom(1, func(rec types.Record) bool {
		seen = append(seen, rec.Seq)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != int(st.LastSeq) {
		t.Errorf("ScanFrom(1) saw %d records, want %d (the whole ledger)", len(seen), st.LastSeq)
	}
}

// TestReaderAcrossCompaction: a reader holding the gen-0 fd reads a stable
// image while compaction unlinks it, and a new reader resolves gen 1.
func TestReaderAcrossCompaction(t *testing.T) {
	day1 := time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)
	clk := newFakeClock(day1)
	l := testLedger(t, clk)
	for i := 0; i < 5; i++ {
		mustAppend(t, l, eventDraft("journald", fmt.Sprintf("journald:sha256v1:%04d", i),
			fmt.Sprintf("%064x", i), 0))
	}
	gen0 := filepath.Join(l.Root(), "2026-09-16.jsonl")
	before, err := os.ReadFile(gen0)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(gen0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// move the writer to the next day so the day is compactable
	clk.Advance(24 * time.Hour)
	mustAppend(t, l, eventDraft("psi", "psi:sha256v1:next", "aa", 0))

	rp := DefaultRetentionPolicy()
	rp.RawKeep = "1s" // a 1-day-old fixture day is past retention_raw
	res, err := l.Compact(context.Background(), "2026-09-16", rp)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if res.ToFile != "2026-09-16.1.gen.jsonl" {
		t.Fatalf("ToFile = %s", res.ToFile)
	}
	// the unlinked inode is still readable, byte-identical to what was there
	held, err := os.ReadFile(filepath.Join("/proc/self/fd", fmt.Sprint(f.Fd())))
	if err != nil {
		t.Skipf("cannot read the held fd: %v", err)
	}
	if !bytes.Equal(held, before) {
		t.Errorf("the held generation-0 image changed under a live reader")
	}
	if _, err := os.Stat(gen0); !os.IsNotExist(err) {
		t.Errorf("generation 0 must be unlinked after the new generation is fsynced (err=%v)", err)
	}
	// a new reader resolves against gen 1
	name, _, err := l.Resolve(2)
	if err != nil {
		t.Fatalf("Resolve(2) after compaction: %v", err)
	}
	if name != "2026-09-16.1.gen.jsonl" {
		t.Errorf("Resolve(2) = %s, want 2026-09-16.1.gen.jsonl", name)
	}
}
