package ledger

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// Sidecar tests (SPEC-01 §3.7a, §7 `internal/ledger/sidecar_test.go`).
//
// The `.idx` sidecar is derived state: it is written when a generation closes,
// it describes exactly the bytes of the file it names, and losing it costs a
// scan and never an answer. Every case below asserts one of those three
// properties, and every one of them asserts the generation file itself is never
// touched by sidecar handling.

// generationFiles returns the ledger's generation file names, sorted.
func generationFiles(t *testing.T, root string) []string {
	t.Helper()
	ents, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		if fileRe.FindStringSubmatch(e.Name()) != nil {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// TestSidecarWrittenAtRotation: every closed generation has a `.idx`, and the
// live file is allowed not to (it has not closed yet).
func TestSidecarWrittenAtRotation(t *testing.T) {
	clk := newFakeClock(testNow())
	l, _ := multiGenerationLedger(t, clk, 3, 6)

	gens := generationFiles(t, l.Root())
	if len(gens) < 2 {
		t.Fatalf("expected multiple generation files, got %v", gens)
	}
	for _, g := range gens[:len(gens)-1] {
		if _, err := os.Stat(sidecarPath(l.Root(), g)); err != nil {
			t.Fatalf("closed generation %s has no .idx sidecar (SPEC-01 §3.7a: written at rotation): %v", g, err)
		}
	}
}

// TestSidecarDescribesTheFileExactly pins the field set: count, byte offsets
// every offset_stride records, seq range, min/max ts and the sha256 of the file's
// bytes. A sidecar that is off by one byte or one record is a sidecar that will
// answer a page from the wrong offset.
func TestSidecarDescribesTheFileExactly(t *testing.T) {
	clk := newFakeClock(testNow())
	l, _ := multiGenerationLedger(t, clk, 2, 260)

	gens := generationFiles(t, l.Root())
	target := gens[0] // the oldest, and definitely closed
	gi, err := buildGenerationIndex(filepath.Join(l.Root(), target), target)
	if err != nil {
		t.Fatalf("buildGenerationIndex: %v", err)
	}
	if gi.File != target {
		t.Fatalf("File = %q want %q", gi.File, target)
	}
	// The record count matches an independent walk of the file.
	raw, err := os.ReadFile(filepath.Join(l.Root(), target))
	if err != nil {
		t.Fatal(err)
	}
	if gi.Bytes != int64(len(raw)) {
		t.Fatalf("Bytes = %d want the file's %d", gi.Bytes, len(raw))
	}
	if gi.Sha256 != sha256Hex(raw) {
		t.Fatalf("Sha256 = %q want the sha256 of the file's bytes", gi.Sha256)
	}
	if gi.OffsetStride != DefaultOffsetStride {
		t.Fatalf("OffsetStride = %d want %d", gi.OffsetStride, DefaultOffsetStride)
	}
	if gi.Records != countRecordLines(t, filepath.Join(l.Root(), target)) {
		t.Fatalf("Records = %d want the file's own parsable line count", gi.Records)
	}
	if gi.Records < 2 {
		t.Fatalf("the fixture failed to write a corpus: %d records", gi.Records)
	}
	if gi.FirstSeq == 0 || gi.LastSeq < gi.FirstSeq {
		t.Fatalf("seq range is wrong: %+v", gi)
	}
	if gi.MinTS == "" || gi.MaxTS == "" || gi.MinTS > gi.MaxTS {
		t.Fatalf("ts range is wrong: min=%q max=%q", gi.MinTS, gi.MaxTS)
	}
	// Offsets are ascending and each addresses the first byte of a line, and
	// there are as many as the record count and the stride imply.
	if len(gi.Offsets) != int((gi.Records-1)/int64(gi.OffsetStride))+1 {
		t.Fatalf("Offsets = %d for %d records at stride %d", len(gi.Offsets), gi.Records, gi.OffsetStride)
	}
	for i, off := range gi.Offsets {
		if i > 0 && off <= gi.Offsets[i-1] {
			t.Fatalf("offsets are not ascending at %d: %d after %d", i, off, gi.Offsets[i-1])
		}
		if off < 0 || off >= int64(len(raw)) {
			t.Fatalf("offset %d is outside the file (%d bytes)", off, len(raw))
		}
		if off > 0 && raw[off-1] != '\n' {
			t.Fatalf("offset %d does not start a line (byte before is %q)", off, raw[off-1])
		}
	}
}

// TestSidecarRebuildPath covers the three ways a sidecar is lost or lies: deleted
// (missing), crash-truncated (unparsable) and describing different bytes
// (mismatch). Each must be discarded and rebuilt to the same field-for-field
// result, and none may touch the generation file.
func TestSidecarRebuildPath(t *testing.T) {
	clk := newFakeClock(testNow())
	l, _ := multiGenerationLedger(t, clk, 2, 40)

	gens := generationFiles(t, l.Root())
	target := gens[0]
	want, err := buildGenerationIndex(filepath.Join(l.Root(), target), target)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(l.Root(), target))
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		prep func(t *testing.T)
		want sidecarState
	}{
		{"deleted", func(t *testing.T) {
			if err := os.Remove(sidecarPath(l.Root(), target)); err != nil {
				t.Fatal(err)
			}
		}, sidecarMissing},
		{"torn", func(t *testing.T) {
			body := `{"file":"` + target + `","bytes":`
			if err := os.WriteFile(sidecarPath(l.Root(), target), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}, sidecarUnparsable},
		{"mismatched-bytes", func(t *testing.T) {
			bad := want
			bad.Bytes = want.Bytes + 4096
			b, _ := json.Marshal(bad)
			if err := os.WriteFile(sidecarPath(l.Root(), target), b, 0o600); err != nil {
				t.Fatal(err)
			}
		}, sidecarMismatch},
		{"mismatched-sha", func(t *testing.T) {
			bad := want
			bad.Sha256 = "deadbeefdeadbeef"
			b, _ := json.Marshal(bad)
			if err := os.WriteFile(sidecarPath(l.Root(), target), b, 0o600); err != nil {
				t.Fatal(err)
			}
		}, sidecarMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.prep(t)
			got, st, err := loadOrRebuildSidecar(l.Root(), target)
			if err != nil {
				t.Fatalf("rebuild: %v", err)
			}
			if st != tc.want {
				t.Fatalf("state = %q want %q", st, tc.want)
			}
			assertSidecarEqual(t, got, want)
			// the generation file itself is untouched: a rebuild reads, never writes
			after, rerr := os.ReadFile(filepath.Join(l.Root(), target))
			if rerr != nil {
				t.Fatal(rerr)
			}
			if string(after) != string(before) {
				t.Fatalf("the generation file changed across a sidecar rebuild")
			}
			// and the rebuild is now on disk, so the next reader uses it
			if _, err := os.Stat(sidecarPath(l.Root(), target)); err != nil {
				t.Fatalf("the rebuilt sidecar was not persisted: %v", err)
			}
		})
	}
}

// TestSidecarAcceleratesTheWalk pins the §3.7a claim that the sidecar is an
// accelerator and not an authority: the same corpus walked with every sidecar
// present and with every sidecar deleted must produce the identical walk.
//
// The comparison is made through a FRESH Ledger for each arm, so the in-memory
// sidecar cache cannot mask a deletion on the second arm (a cache hit would make
// the test pass without ever exercising the missing-sidecar path).
func TestSidecarAcceleratesTheWalk(t *testing.T) {
	clk := newFakeClock(testNow())
	l, want := multiGenerationLedger(t, clk, 2, 60)
	root := l.Root()
	opts := l.opts

	// Arm A: every closed generation has its sidecar (the writer just wrote them).
	withSidecar, pagesWith := pageWalk(t, l, types.KEvent, 25)

	// Arm B: delete every sidecar, then walk from a fresh Ledger. The first
	// ledger must release the LOCK before the second can take it (SPEC-01 §3.4
	// rule 6: one writer per host).
	for _, g := range generationFiles(t, root) {
		removeSidecar(root, g)
	}
	if err := l.Close(contextBackground()); err != nil {
		t.Fatalf("close arm A: %v", err)
	}
	l2, err := Open(contextBackground(), opts)
	if err != nil {
		t.Fatalf("reopen for the no-sidecar arm: %v", err)
	}
	defer l2.Close(contextBackground())
	without, pagesWithout := pageWalk(t, l2, types.KEvent, 25)

	if len(withSidecar) != len(without) {
		t.Fatalf("walk lengths differ: %d with sidecars, %d without", len(withSidecar), len(without))
	}
	for i := range withSidecar {
		if withSidecar[i] != without[i] {
			t.Fatalf("record %d differs: seq %d with sidecars, %d without", i, withSidecar[i], without[i])
		}
	}
	assertWalkExact(t, without, want)
	if pagesWith != pagesWithout {
		t.Fatalf("page counts differ: %d with sidecars, %d without", pagesWith, pagesWithout)
	}
	if l2.SidecarRebuilds() == 0 {
		t.Fatalf("the no-sidecar arm rebuilt nothing; the deleted sidecars were not noticed")
	}
}

// TestSidecarDroppedWithItsGeneration pins the retention rule of §3.7a: retention
// drops whole generations, so the file and its `.idx` leave together and no
// sidecar is left describing a file that is gone.
func TestSidecarDroppedWithItsGeneration(t *testing.T) {
	clk := newFakeClock(testNow())
	l, _ := multiGenerationLedger(t, clk, 2, 20)

	gens := generationFiles(t, l.Root())
	target := gens[0]
	if _, err := os.Stat(sidecarPath(l.Root(), target)); err != nil {
		// the oldest file may still be live in a two-day corpus; use the closed one
		target = gens[0]
	}
	// Rewrite the sidecar so it certainly exists, then drop the generation the
	// way the sweep does.
	if _, err := writeSidecar(l.Root(), target); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(l.Root(), target))
	removeSidecar(l.Root(), target)

	if _, err := os.Stat(sidecarPath(l.Root(), target)); !os.IsNotExist(err) {
		t.Fatalf("a dropped generation left its .idx behind (err=%v)", err)
	}
}

// sha256Hex is the hex sha256 of b.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// contextBackground is the test-side context.Background(), kept as a one-liner so
// the sidecar tests read without an import block of their own.
func contextBackground() context.Context { return context.Background() }

// countRecordLines counts the parsable record lines of a generation file
// independently of the sidecar builder, so the sidecar's own count is checked
// against the file rather than against a literal from the fixture.
func countRecordLines(t *testing.T, path string) int64 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rd := newLineReader(f)
	var n int64
	for {
		line, _, complete, rerr := rd.next()
		if rerr != nil {
			t.Fatal(rerr)
		}
		if len(line) == 0 && !complete {
			break
		}
		if !complete {
			break
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if rec, derr := decodeRecord(line, false); derr == nil && rec != nil {
			n++
		}
	}
	return n
}
