package ledger

import (
	"crypto/sha256"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// ---- the footprint guard (QA-TROUBLE-6) ----
//
// The ledger suite must survive a 3 GiB cap. That is not a property of the
// fixtures' SIZE alone — it is a property of their SHAPE: the old AC-6 image
// held the whole ledger (a 472 MiB file at 1M records) plus its concatenated
// copy plus a per-line header slice, so peak live grew with the ledger and blew
// a 3 GiB cgroup during the decode. The fix streams the image, which makes peak
// live O(1) in LEDGER BYTES and O(records) only in the seq accumulator the hole
// check genuinely needs.
//
// This test pins both halves of that claim so a revert cannot pass:
//
//   - the STREAMING arm must stay flat (its growth is decoupled from the file's
//     size, and specifically must not carry the file image), and
//   - the BUFFERED arm must visibly scale with the file, which proves the
//     measurement can SEE retention — without that control arm, a broken
//     instrument (e.g. one sampling after a GC that already collected the
//     evidence) would report ~0 for both and the guard would be vacuous.
//
// The numbers below are MEASURED (see the t.Logf lines) and each bound is stated
// with headroom over them; TestLedgerFootprintIsDocumented ties the streaming
// bound to the footprint recorded in docs/operations.md.

const (
	// footprintFixtureRecords × footprintFixturePayload ≈ 80 MiB of ledger.
	footprintFixtureRecords = 20000
	footprintFixturePayload = 4096
	// footprintStreamingBound is the live-set ceiling for the streaming decode,
	// in bytes. The fixture is ~87 MiB and the measured peak is 2.6-5.0 MiB
	// across runs (GC churn moves it), so the bound is ~2.4x the worst reading
	// while staying under a sixth of the fixture: the streaming arm must not
	// carry the image at all.
	footprintStreamingBound = 12 << 20
)

// peakHeapDelta runs fn and returns the peak growth of HeapInuse over the
// baseline sampled immediately before the call, plus the value fn returns
// (bytes processed, by convention). HeapInuse rather than HeapAlloc so bytes
// returned to the runtime but not yet reused cannot hide growth.
func peakHeapDelta(fn func() int64) (peak, processed int64) {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	baseline := int64(ms.HeapInuse)
	processed = fn()
	runtime.ReadMemStats(&ms)
	if cur := int64(ms.HeapInuse); cur > baseline {
		peak = cur - baseline
	}
	return peak, processed
}

// TestDecodeFootprintIsBounded is the mechanical guard: the streaming decode's
// live set must be decoupled from the ledger's byte size, and the buffered shape
// it replaced must be demonstrably NOT decoupled (the control arm).
func TestDecodeFootprintIsBounded(t *testing.T) {
	root := testRoot(t)
	day := testNow().Format(dayLayout)
	name := day + ".jsonl"
	size := writeRawFixture(t, root, name, footprintFixtureRecords, footprintFixturePayload, testNow())
	path := filepath.Join(root, name)
	t.Logf("fixture: %s = %d bytes (%.1f MiB), %d records",
		name, size, float64(size)/(1<<20), footprintFixtureRecords)

	// Arm A — the shipped shape: hash + decode by streaming, retaining only the
	// seqs the hole check needs.
	var streamSeqs []uint64
	streamPeak, streamProcessed := peakHeapDelta(func() int64 {
		var h hash.Hash = sha256.New()
		n, err := hashFileBytes(h, path, -1)
		if err != nil {
			t.Fatal(err)
		}
		decodeLedgerFile(t, path, &streamSeqs)
		return n
	})
	t.Logf("STREAMING decode: peak HeapInuse growth = %d bytes (%.1f MiB) for %d bytes processed (%.2f%% of fixture)",
		streamPeak, float64(streamPeak)/(1<<20), streamProcessed, 100*float64(streamPeak)/float64(size))

	// Arm B — the shape that OOMed: read the whole file, concatenate it, and
	// hold a header slice per line. This arm exists to prove the instrument sees
	// retention; it is not a code path the product uses.
	bufferedPeak, _ := peakHeapDelta(func() int64 {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		image := make([]byte, 0, len(b))
		image = append(image, b...)
		lines := splitLines(image)
		if len(lines) != footprintFixtureRecords {
			t.Fatalf("control arm split %d lines, want %d", len(lines), footprintFixtureRecords)
		}
		return int64(len(image) + len(lines)*24)
	})
	t.Logf("BUFFERED control:  peak HeapInuse growth = %d bytes (%.1f MiB) for the same fixture",
		bufferedPeak, float64(bufferedPeak)/(1<<20))

	if len(streamSeqs) != footprintFixtureRecords {
		t.Fatalf("streaming arm decoded %d records, want %d", len(streamSeqs), footprintFixtureRecords)
	}
	// The control arm must scale with the image, or this guard proves nothing.
	if bufferedPeak < size {
		t.Errorf("control arm peaked at %d bytes for a %d-byte fixture: this measurement cannot see retention, so the bound below is vacuous",
			bufferedPeak, size)
	}
	if streamPeak >= footprintStreamingBound {
		t.Errorf("streaming decode peaked at %d bytes (%.1f MiB), want < %d bytes: the decode path is retaining the ledger image again",
			streamPeak, float64(streamPeak)/(1<<20), footprintStreamingBound)
	}
	if streamPeak >= bufferedPeak/4 {
		t.Errorf("streaming decode (%d bytes) is not decoupled from the buffered shape (%d bytes)", streamPeak, bufferedPeak)
	}
}

// TestLedgerFootprintIsDocumented ties the streaming bound above to the
// footprint section of docs/operations.md, so the requirement cannot drift from
// the prose the way §7's numbers cannot drift from load_test.go.
func TestLedgerFootprintIsDocumented(t *testing.T) {
	body, err := readWholeFile(filepath.Join("..", "..", "docs", "operations.md"))
	if err != nil {
		t.Fatalf("read docs/operations.md: %v", err)
	}
	doc := normalizeWS(body)
	section := docBetween(doc, "### The ledger test suite's footprint and its cap", "## 13. The research rung")
	if section == "" {
		t.Fatalf("docs/operations.md is missing the ledger-footprint section")
	}
	bound := docMB(footprintStreamingBound)
	if !strings.Contains(section, bound) {
		t.Errorf("the footprint section does not carry the streaming bound %s", bound)
	}
	if !strings.Contains(section, docCount(footprintFixtureRecords)+" records") {
		t.Errorf("the footprint section does not carry the fixture size %d records", footprintFixtureRecords)
	}
	// The cap the suite must survive, and the failure it was measured against.
	for _, want := range []string{"3 GiB", "decodeRecord"} {
		if !strings.Contains(section, want) {
			t.Errorf("the footprint section does not name %q", want)
		}
	}
}

// docMB renders a byte bound the way the operations-doc tables state it (the
// constants' binary megabyte: `1MB = 1<<20` bytes).
func docMB(n int64) string { return fmt.Sprintf("%dMB", n>>20) }

// docCount renders a record count with thousands separators, the shape the
// operations-doc tables use.
func docCount(n int) string {
	s := strconv.Itoa(n)
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// docBetween returns the normalized text from start (inclusive) to end
// (exclusive), or "" when either marker is absent.
func docBetween(doc, start, end string) string {
	i := strings.Index(doc, start)
	if i < 0 {
		return ""
	}
	rest := doc[i:]
	if j := strings.Index(rest, end); j >= 0 {
		rest = rest[:j]
	}
	return rest
}
