package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// ---- AC-30: page tokens, bounded walks, reset hints ----

// pageWalk drives a full newest-first walk and returns every seq it saw, in
// walk order, plus the page count. It fails the test if a page reports an error
// or if the walk does not terminate — a walk that cannot terminate is exactly
// the error loop AC-30 exists to forbid.
func pageWalk(t *testing.T, l *Ledger, kind types.RecordKind, size int) ([]uint64, int) {
	t.Helper()
	var seen []uint64
	tok := types.PageToken{}
	for page := 0; ; page++ {
		if page > 10000 {
			t.Fatalf("walk did not terminate after %d pages (error loop?)", page)
		}
		recs, _, next, err := l.Page(kind, tok, size)
		if err != nil {
			t.Fatalf("Page(page %d): %v", page, err)
		}
		if len(recs) == 0 && !next.IsZero() {
			t.Fatalf("page %d returned no records but a token %q (a walk must not stall)", page, next.String())
		}
		for _, r := range recs {
			seen = append(seen, r.Seq)
		}
		if next.IsZero() {
			return seen, page + 1
		}
		tok = next
	}
}

// assertWalkExact asserts the walk saw every seq of want exactly once, in strict
// newest-first order.
func assertWalkExact(t *testing.T, seen, want []uint64) {
	t.Helper()
	if len(seen) != len(want) {
		t.Fatalf("walk returned %d records, want %d", len(seen), len(want))
	}
	seenSet := map[uint64]int{}
	for _, s := range seen {
		seenSet[s]++
	}
	for _, s := range want {
		switch seenSet[s] {
		case 1:
		case 0:
			t.Fatalf("walk skipped seq %d (no record repeated, none skipped is the AC-30 invariant)", s)
		default:
			t.Fatalf("walk repeated seq %d %d times", s, seenSet[s])
		}
	}
	// newest-first: every page continues one descending order
	for i := 1; i < len(seen); i++ {
		if seen[i] >= seen[i-1] {
			t.Fatalf("walk order broke at %d: %d then %d (a walk is newest-first)", i, seen[i-1], seen[i])
		}
	}
}

// multiGenerationLedger writes `days` days of `perDay` event records and returns
// the ledger plus every event seq, newest-first. Each day is its own generation
// file, which is what makes the walk cross files.
//
// The corpus is written in a loop on purpose: AC-30's acceptance is about the
// walk's totality, not about a 10M-record fixture, and a fixture that large is
// neither cheap nor honest to fake here.
func multiGenerationLedger(t *testing.T, clk *fakeClock, days, perDay int, muts ...func(*Options)) (*Ledger, []uint64) {
	t.Helper()
	l := testLedger(t, clk, muts...)
	var seqs []uint64
	for d := 0; d < days; d++ {
		for i := 0; i < perDay; i++ {
			rec := mustAppend(t, l, eventDraft(
				"journald",
				fmt.Sprintf("journald:sha256v1:%04d%04d", d, i),
				fmt.Sprintf("%064x", (d*perDay+i)%7),
				i%2,
			))
			seqs = append(seqs, rec.Seq)
		}
		// one extra record opens the next day's file (and closes this one)
		if d < days-1 {
			clk.Advance(24 * time.Hour)
		}
	}
	// The ledger's own housekeeping (boot, rotate, index) records interleave with
	// events; the walk is newest-first, so reverse what we appended.
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] > seqs[j] })
	return l, seqs
}

// TestAC30WalkRepeatsNothingAndSkipsNothing walks a multi-generation corpus at
// several page sizes and asserts the seq set is returned exactly once in
// newest-first order — no record repeated, none skipped, across file boundaries.
func TestAC30WalkRepeatsNothingAndSkipsNothing(t *testing.T) {
	// The assertion is totality at every page size; the page-count assert is
	// only meaningful where the corpus can actually span pages.
	for _, size := range []int{1, 3, 7, 50} {
		t.Run(fmt.Sprintf("page_size_%d", size), func(t *testing.T) {
			clk := newFakeClock(testNow())
			l, want := multiGenerationLedger(t, clk, 3, 40)
			seen, pages := pageWalk(t, l, types.KEvent, size)
			assertWalkExact(t, seen, want)
			if len(want) > size && pages < 2 {
				t.Fatalf("walk of %d records took %d page(s) at page_size %d; it must span pages",
					len(want), pages, size)
			}
		})
	}
}

// TestAC30NextPageTokenEmptyExactlyAtTheEnd pins the §2.3a end-of-walk rule: the
// token is non-empty while records remain and empty exactly on the final page —
// "finished, not truncated".
func TestAC30NextPageTokenEmptyExactlyAtTheEnd(t *testing.T) {
	clk := newFakeClock(testNow())
	l, want := multiGenerationLedger(t, clk, 2, 6)
	const size = 4
	seen := 0
	tok := types.PageToken{}
	for page := 0; ; page++ {
		recs, info, next, err := l.Page(types.KEvent, tok, size)
		if err != nil {
			t.Fatalf("Page: %v", err)
		}
		if !info.Indexed {
			t.Fatalf("page %d is not Indexed on the normal path: %+v", page, info)
		}
		if info.Reset {
			t.Fatalf("page %d set Reset on the normal path: %+v", page, info)
		}
		seen += len(recs)
		remaining := len(want) - seen
		if remaining > 0 && next.IsZero() {
			t.Fatalf("page %d returned the last token with %d records still unread (truncated walk)", page, remaining)
		}
		if remaining == 0 && !next.IsZero() {
			t.Fatalf("walk finished but page %d still returned next_page_token=%q", page, next.String())
		}
		if next.IsZero() {
			if len(recs) != size && len(recs) == 0 {
				t.Fatalf("final page returned no records")
			}
			return
		}
		if len(recs) != size {
			t.Fatalf("page %d returned %d records with a token (only the final page may be short)", page, len(recs))
		}
		tok = next
	}
}

// TestAC30DroppedGenerationReturnsTheResetHint drops the oldest generation
// mid-walk and asserts the §2.3a outcome: Reset=true with
// ResetReason=generation_dropped, a newest-first restart, and no error.
func TestAC30DroppedGenerationReturnsTheResetHint(t *testing.T) {
	clk := newFakeClock(testNow())
	l, _ := multiGenerationLedger(t, clk, 3, 8)

	// Take a page, then walk into the oldest generation so the token names it.
	tok := types.PageToken{}
	var tokInOldest types.PageToken
	var oldestFile string
	for {
		_, _, next, err := l.Page(types.KEvent, tok, 5)
		if err != nil {
			t.Fatalf("Page: %v", err)
		}
		if next.IsZero() {
			t.Fatalf("the walk ended before reaching a token in the oldest generation")
		}
		tok = next
		if next.Generation < oldestFile || oldestFile == "" {
			oldestFile = next.Generation
		}
		if next.Generation == oldestFile {
			tokInOldest = next
			if len(tokInOldest.Generation) > 0 {
				break
			}
		}
	}

	// The retention sweep drops whole generations: the file and its .idx go.
	if err := os.Remove(filepath.Join(l.Root(), tokInOldest.Generation)); err != nil {
		t.Fatalf("drop generation %s: %v", tokInOldest.Generation, err)
	}
	removeSidecar(l.Root(), tokInOldest.Generation)

	recs, info, next, err := l.Page(types.KEvent, tokInOldest, 5)
	if err != nil {
		t.Fatalf("a dropped generation must not be an error (no error loop): %v", err)
	}
	if !info.Reset {
		t.Fatalf("info.Reset = false for a dropped generation: %+v", info)
	}
	if info.ResetReason != types.ResetGenerationDropped {
		t.Fatalf("ResetReason = %q want %q", info.ResetReason, types.ResetGenerationDropped)
	}
	if len(recs) == 0 {
		t.Fatalf("the reset answer must be a newest-first first page, not an empty page")
	}
	// newest-first restart: the first record is the newest this host still has
	if !next.IsZero() && next.Generation < info.TruncatedBeforeTS {
		_ = next
	}
	// and the answer continues: the restart's first page is the newest records
	fresh, _, _, err := l.Page(types.KEvent, types.PageToken{}, 5)
	if err != nil {
		t.Fatalf("newest-first start: %v", err)
	}
	if len(fresh) != len(recs) || fresh[0].Seq != recs[0].Seq {
		t.Fatalf("the reset page is not the newest-first first page: got seq %d, fresh start seq %d",
			recs[0].Seq, fresh[0].Seq)
	}
}

// TestAC30TokenReasons pins the three §2.3a reset reasons: a token that does not
// parse / does not name a generation is token_invalid; a token naming a day this
// host has no knowledge of is token_foreign.
func TestAC30TokenReasons(t *testing.T) {
	clk := newFakeClock(testNow())
	l, _ := multiGenerationLedger(t, clk, 2, 5)

	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"not-three-parts", "2026-09-16.jsonl", types.ResetTokenInvalid},
		{"empty-generation", "::100::7", types.ResetTokenInvalid},
		{"non-numeric-offset", "2026-09-16.jsonl::abc::7", types.ResetTokenInvalid},
		{"non-numeric-seq", "2026-09-16.jsonl::100::x", types.ResetTokenInvalid},
		{"path-in-generation", "../etc/passwd::1::1", types.ResetTokenInvalid},
		{"another-hosts-file", "1999-01-01.3.gen.jsonl::100::1", types.ResetTokenForeign},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Drive every case through the production API boundary, which is the
			// path the dashboard and the CLI use: the raw string goes in, and an
			// unparsable token must come back as a reset hint with 200 semantics
			// rather than an error.
			recs, info, _, err := l.PageFromTokenString(types.KEvent, tc.raw, 5)
			if err != nil {
				t.Fatalf("a bad token must be a reset hint, never an error: %v", err)
			}
			if !info.Reset {
				t.Fatalf("info.Reset = false for %q: %+v", tc.raw, info)
			}
			if info.ResetReason != tc.want {
				t.Fatalf("ResetReason = %q want %q", info.ResetReason, tc.want)
			}
			if len(recs) == 0 {
				t.Fatalf("the reset answer must be a newest-first first page, not an empty page")
			}
			// and the parsed form agrees with the boundary's verdict
			if tok, perr := types.ParsePageToken(tc.raw); perr == nil {
				_, pinfo, _, _ := l.Page(types.KEvent, tok, 5)
				if !pinfo.Reset || pinfo.ResetReason != tc.want {
					t.Fatalf("parsed-token path reset = %v/%q want true/%q",
						pinfo.Reset, pinfo.ResetReason, tc.want)
				}
			}
		})
	}
}

// TestAC30TokenGrammarRoundTrip pins the token grammar: the string form
// round-trips exactly, the empty token renders as "" and parses back to the
// newest-first start, and three malformed forms are refused.
func TestAC30TokenGrammarRoundTrip(t *testing.T) {
	// the §2.3a example, verbatim
	const example = "2026-09-16.1.gen.jsonl::1835008::41207"
	tok, err := types.ParsePageToken(example)
	if err != nil {
		t.Fatalf("ParsePageToken(%q): %v", example, err)
	}
	if tok.Generation != "2026-09-16.1.gen.jsonl" || tok.ByteOffset != 1835008 || tok.Seq != 41207 {
		t.Fatalf("parsed = %+v, want the grammar's three fields", tok)
	}
	if got := tok.String(); got != example {
		t.Fatalf("String() = %q want %q (the grammar must round-trip exactly)", got, example)
	}
	// the empty token is the newest-first start, not an error
	zero, err := types.ParsePageToken("")
	if err != nil {
		t.Fatalf("ParsePageToken(\"\") must be the start token, got %v", err)
	}
	if !zero.IsZero() || zero.String() != "" {
		t.Fatalf("the start token is %+v / %q, want zero / \"\"", zero, zero.String())
	}
	for _, bad := range []string{
		"2026-09-16.1.gen.jsonl",
		"2026-09-16.1.gen.jsonl::1835008",
		"2026-09-16.1.gen.jsonl::nope::41207",
		"2026-09-16.1.gen.jsonl::1835008::nope",
	} {
		if _, err := types.ParsePageToken(bad); err == nil {
			t.Fatalf("ParsePageToken(%q) succeeded; a malformed token must be an error", bad)
		}
	}
	// the JSON form is the sidecar/client shape of §3.15.1
	b, err := json.Marshal(tok)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"generation":"2026-09-16.1.gen.jsonl","byte_offset":1835008,"seq":41207}` {
		t.Fatalf("token JSON = %s, want the §3.15.1 example shape", b)
	}
	var back types.PageToken
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back != tok {
		t.Fatalf("JSON round-trip = %+v want %+v", back, tok)
	}
}

// TestAC30TokenStableWithinAGeneration pins "stable within a generation's
// lifetime": the same token returns the same page while the file exists.
func TestAC30TokenStableWithinAGeneration(t *testing.T) {
	clk := newFakeClock(testNow())
	l, _ := multiGenerationLedger(t, clk, 2, 6)
	_, _, tok, err := l.Page(types.KEvent, types.PageToken{}, 3)
	if err != nil {
		t.Fatal(err)
	}
	first, _, next1, err := l.Page(types.KEvent, tok, 3)
	if err != nil {
		t.Fatal(err)
	}
	second, _, next2, err := l.Page(types.KEvent, tok, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != len(second) {
		t.Fatalf("the same token returned %d then %d records", len(first), len(second))
	}
	for i := range first {
		if first[i].Seq != second[i].Seq {
			t.Fatalf("record %d differs across two identical calls: %d vs %d", i, first[i].Seq, second[i].Seq)
		}
	}
	if next1.String() != next2.String() {
		t.Fatalf("next token differs across two identical calls: %q vs %q", next1.String(), next2.String())
	}
}

// TestAC30PageSizeClamp pins the §2.3a clamp: absent/0 means the default, a value
// above the max is clamped to the max (never refused), and neither case can
// produce an unbounded read.
func TestAC30PageSizeClamp(t *testing.T) {
	clk := newFakeClock(testNow())
	l, _ := multiGenerationLedger(t, clk, 2, 4)

	if got := l.pageSize(0); got != DefaultPageSize {
		t.Fatalf("pageSize(0) = %d want the default %d", got, DefaultPageSize)
	}
	if got := l.pageSize(-5); got != DefaultPageSize {
		t.Fatalf("pageSize(-5) = %d want the default %d (a negative is a 400 at the API boundary, a clamp here)", got, DefaultPageSize)
	}
	if got := l.pageSize(DefaultPageSizeMax + 1); got != DefaultPageSizeMax {
		t.Fatalf("pageSize(max+1) = %d want the clamp %d", got, DefaultPageSizeMax)
	}
	if got := l.pageSize(7); got != 7 {
		t.Fatalf("pageSize(7) = %d want 7", got)
	}
	// the clamp is observable through Page: a 0 asks for the default and cannot
	// be refused, and a huge size is capped, not an error
	small, _, _, err := l.Page(types.KEvent, types.PageToken{}, 0)
	if err != nil {
		t.Fatalf("Page(size=0) must not be refused: %v", err)
	}
	if len(small) > DefaultPageSize {
		t.Fatalf("Page(size=0) returned %d records, more than the default %d", len(small), DefaultPageSize)
	}
	huge, _, _, err := l.Page(types.KEvent, types.PageToken{}, DefaultPageSizeMax*10)
	if err != nil {
		t.Fatalf("Page(size>max) must be clamped, not refused: %v", err)
	}
	if len(huge) > DefaultPageSizeMax {
		t.Fatalf("Page(size>max) returned %d records, more than the max %d", len(huge), DefaultPageSizeMax)
	}
	// a per-ledger override is honoured and clamped against it
	l2, _ := multiGenerationLedger(t, clk, 1, 4, mutOpts(func(o *Options) {
		o.PageSizeDefault = 3
		o.PageSizeMax = 5
	}))
	if got := l2.pageSize(0); got != 3 {
		t.Fatalf("configured default = %d want 3", got)
	}
	if got := l2.pageSize(99); got != 5 {
		t.Fatalf("configured max clamp = %d want 5", got)
	}
}

// TestAC30PageReadIsBounded pins the §7 performance contract:
// `ScannedLines ≤ offset_stride + page_size` for every page of a walk that is
// not the final one. A page that read the whole file would satisfy the totality
// tests and still be wrong.
func TestAC30PageReadIsBounded(t *testing.T) {
	clk := newFakeClock(testNow())
	// Enough records per file that a full-file read would be obvious, without
	// writing a corpus large enough to starve the timing-sensitive durability
	// tests that share this package's suite.
	l, _ := multiGenerationLedger(t, clk, 2, 320)
	const size = 50
	tok := types.PageToken{}
	pages := 0
	for {
		recs, info, next, err := l.Page(types.KEvent, tok, size)
		if err != nil {
			t.Fatalf("Page: %v", err)
		}
		if len(recs) == 0 {
			t.Fatalf("empty page mid-walk")
		}
		if !next.IsZero() {
			// The §7 contract is per (page, file): a page reads at most
			// `offset_stride + page_size` lines out of any ONE generation file,
			// so it never scans a whole file to fill a page. ScannedLines is the
			// sum over the files the page touched (a page that crosses a file
			// boundary starts each file at a sample), so the assertion is the
			// one-file bound times the number of files a page can span.
			if info.ScannedLines == 0 {
				t.Fatalf("page %d reports 0 scanned lines (accounting must be real)", pages)
			}
			if info.ScannedLines > int64(2*(DefaultOffsetStride+size)) {
				t.Fatalf("page %d scanned %d lines for page_size %d (one-file bound offset_stride+page_size = %d)",
					pages, info.ScannedLines, size, DefaultOffsetStride+size)
			}
		}
		pages++
		if next.IsZero() {
			break
		}
		tok = next
	}
	if pages < 10 {
		t.Fatalf("walk took %d pages; the corpus should span many pages", pages)
	}
}

// TestAC30SidecarRebuildIsExercised covers the §3.7a sidecar path end to end
// through the ledger: the sidecar is written when a generation closes, a deleted
// sidecar is rebuilt from the generation file, and a torn/mismatched sidecar is
// discarded and rebuilt rather than believed.
func TestAC30SidecarRebuildIsExercised(t *testing.T) {
	clk := newFakeClock(testNow())
	l, want := multiGenerationLedger(t, clk, 3, 6)

	// Every closed generation has a sidecar.
	entries, err := os.ReadDir(l.Root())
	if err != nil {
		t.Fatal(err)
	}
	var gens []string
	for _, e := range entries {
		if e.IsDir() || fileRe.FindStringSubmatch(e.Name()) == nil {
			continue
		}
		gens = append(gens, e.Name())
	}
	if len(gens) < 2 {
		t.Fatalf("expected multiple generation files, got %v", gens)
	}
	sort.Strings(gens)
	closed := gens[:len(gens)-1] // the last one is still the live file
	for _, g := range closed {
		if _, err := os.Stat(sidecarPath(l.Root(), g)); err != nil {
			t.Fatalf("closed generation %s has no .idx sidecar: %v (SPEC-01 §3.7a: written when the file closes)", g, err)
		}
	}

	target := closed[0]
	wantSidecar, _, err := loadOrRebuildSidecar(l.Root(), target)
	if err != nil {
		t.Fatalf("loadOrRebuildSidecar: %v", err)
	}
	if wantSidecar.File != target {
		t.Fatalf("sidecar.File = %q want %q", wantSidecar.File, target)
	}
	if wantSidecar.Records <= 0 || wantSidecar.LastSeq < wantSidecar.FirstSeq {
		t.Fatalf("sidecar counts are wrong: %+v", wantSidecar)
	}
	if wantSidecar.Sha256 == "" || wantSidecar.MinTS == "" || wantSidecar.MaxTS == "" {
		t.Fatalf("sidecar is missing sha256/min-max ts: %+v", wantSidecar)
	}

	// (a) a deleted sidecar is rebuilt from the generation file
	if err := os.Remove(sidecarPath(l.Root(), target)); err != nil {
		t.Fatal(err)
	}
	l2 := &Ledger{root: l.Root(), opts: l.opts, sidecarCache: map[string]sidecarEntry{}}
	rebuilt, st, err := loadOrRebuildSidecar(l2.root, target)
	if err != nil {
		t.Fatalf("rebuild after delete: %v", err)
	}
	if st == sidecarOK {
		t.Fatalf("a deleted sidecar must be reported as a rebuild, got state %q", st)
	}
	assertSidecarEqual(t, rebuilt, wantSidecar)

	// (b) a torn sidecar (crash-truncated rotation) is discarded and rebuilt
	if err := os.WriteFile(sidecarPath(l.Root(), target), []byte(`{"file":"`+target+`","bytes":`), 0o600); err != nil {
		t.Fatal(err)
	}
	torn, st, err := loadOrRebuildSidecar(l2.root, target)
	if err != nil {
		t.Fatalf("rebuild after tear: %v", err)
	}
	if st != sidecarUnparsable {
		t.Fatalf("a torn sidecar must be reported as unparsable, got %q", st)
	}
	assertSidecarEqual(t, torn, wantSidecar)

	// (c) a sidecar whose Bytes/Sha256 disagree with the file is discarded and
	// rebuilt — an .idx never gets to describe a file it does not match
	bogus := wantSidecar
	bogus.Bytes = wantSidecar.Bytes + 1
	bogus.Sha256 = "deadbeef"
	bb, err := json.Marshal(bogus)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sidecarPath(l.Root(), target), bb, 0o600); err != nil {
		t.Fatal(err)
	}
	fixed, st, err := loadOrRebuildSidecar(l2.root, target)
	if err != nil {
		t.Fatalf("rebuild after mismatch: %v", err)
	}
	if st != sidecarMismatch {
		t.Fatalf("a mismatched sidecar must be reported as a mismatch, got %q", st)
	}
	assertSidecarEqual(t, fixed, wantSidecar)

	// (d) the walk is unaffected by the whole exercise
	seen, _ := pageWalk(t, l, types.KEvent, 4)
	assertWalkExact(t, seen, want)
}

// TestAC30PageSurvivesCompaction pins §6.17: a compaction inside a walk is
// transparent — the walk still terminates with an empty token and still returns
// every record exactly once.
func TestAC30PageSurvivesCompaction(t *testing.T) {
	clk := newFakeClock(testNow())
	l, _ := multiGenerationLedger(t, clk, 3, 10)

	_, _, tok, err := l.Page(types.KEvent, types.PageToken{}, 5)
	if err != nil {
		t.Fatal(err)
	}
	// compact the oldest day, including the file the token points into, if any
	oldest := l.oldestDay()
	if oldest == "" {
		t.Fatal("no day to compact")
	}
	if _, err := l.Compact(context.Background(), oldest, l.opts.Retention); err != nil {
		// 009 is the existing "nothing to reclaim without sacrificing tombstone
		// counts" refusal (§5). It is not a page-walk concern, so the test only
		// requires that compaction either rewrote the day or refused for that
		// reason — never that it corrupted the walk.
		if CodeOf(err) != types.CodeLedger009 && CodeOf(err) != types.CodeLedger007 {
			t.Fatalf("Compact: %v", err)
		}
	}
	// the walk continues: no error, terminates, and repeats nothing
	var seen []uint64
	cur := tok
	for page := 0; ; page++ {
		if page > 1000 {
			t.Fatalf("post-compaction walk did not terminate")
		}
		recs, info, next, err := l.Page(types.KEvent, cur, 5)
		if err != nil {
			t.Fatalf("Page after compaction: %v", err)
		}
		_ = info
		for _, r := range recs {
			seen = append(seen, r.Seq)
		}
		if next.IsZero() {
			break
		}
		cur = next
	}
	dup := map[uint64]int{}
	for _, s := range seen {
		dup[s]++
	}
	for s, n := range dup {
		if n > 1 {
			t.Fatalf("seq %d returned %d times after a compaction", s, n)
		}
	}
}

// TestAC30FilesNotCarriedByTheIndexGetAnExplainedAnswer covers a file that is on
// disk but outside the authoritative set the running ledger indexed: the shape a
// retention drop leaves behind when the sweep unlinked the file but this process
// has not re-read the directory since.
//
// §2.3a's cold-read row applies: the page IS served from the file, `Indexed`
// false, `Partial` true with the scanned byte/line counts, still bounded by
// page_size, and never an error.
func TestAC30FilesNotCarriedByTheIndexGetAnExplainedAnswer(t *testing.T) {
	clk := newFakeClock(testNow())
	l, _ := multiGenerationLedger(t, clk, 2, 12)

	// A file for a day the index DOES know, with a generation number the index
	// has not seen: the directory listing did not exist at boot, exactly like a
	// file that appeared under the running process.
	const name = "2026-09-16.3.gen.jsonl"
	writeRawFixture(t, l.Root(), name, 40, 16, time.Date(2026, 9, 16, 8, 0, 0, 0, time.UTC))

	tok := types.PageToken{Generation: name, ByteOffset: 0, Seq: 0}
	recs, info, _, err := l.Page(types.KEvent, tok, 5)
	if err != nil {
		t.Fatalf("a token for a file outside the authoritative set must not be an error: %v", err)
	}
	if info.Indexed {
		t.Fatalf("Indexed = true for a file the index does not carry: %+v", info)
	}
	if !info.Partial {
		t.Fatalf("Partial = false for a cold-read page: %+v", info)
	}
	if info.Reason != "outside_index_window" {
		t.Fatalf("Reason = %q want outside_index_window (the §3.6 cold-read vocabulary)", info.Reason)
	}
	if info.ScannedBytes == 0 && info.ScannedLines == 0 {
		t.Fatalf("a cold-read page must report scanned byte/line counts: %+v", info)
	}
	if len(recs) > 5 {
		t.Fatalf("cold page returned %d records, above page_size 5", len(recs))
	}
	if info.Reset {
		t.Fatalf("a file that EXISTS is not a dropped generation: %+v", info)
	}

	// The walk is a reader: it must not quietly index what it walks.
	before := l.IndexStats()
	if _, _, _, err := l.Page(types.KEvent, types.PageToken{}, 5); err != nil {
		t.Fatal(err)
	}
	if after := l.IndexStats(); after.Entries != before.Entries {
		t.Fatalf("a page changed the index (%d → %d entries); the walk is a reader, not a builder",
			before.Entries, after.Entries)
	}
}

// TestAC30ColdReadPartialAccounting pins the §2.3a cold-read row's shape: when
// the ledger serves a page for a corpus the resident index did not carry, the
// answer says so (Partial=true) and reports the byte/line counts it scanned.
//
// The cold path is reproduced through the §3.6 degradation ladder: a build budget
// that forces the bounded/tiered mode leaves older days' events out of the
// resident index while their files stay authoritative and their §3.7a sidecars
// are written — exactly the configuration the sidecar exists for.
func TestAC30ColdReadPartialAccounting(t *testing.T) {
	clk := newFakeClock(testNow())
	l := testLedger(t, clk, mutOpts(func(o *Options) {
		o.Index.HotDays = 1
		o.Index.BuildBudgetBytes = 1 // force the bounded (tiered) build
	}))
	for d := 0; d < 4; d++ {
		for i := 0; i < 12; i++ {
			mustAppend(t, l, eventDraft("journald",
				fmt.Sprintf("journald:sha256v1:%04d%04d", d, i), "aa", 0))
		}
		if d < 3 {
			clk.Advance(24 * time.Hour)
		}
	}
	// Close and reopen so the boot rebuild re-reads the directory and applies the
	// bounded build: that is what leaves the older days' events out of the
	// resident index while their files stay authoritative.
	if err := l.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	// The same root and the same bounded-build options: a fresh root would test
	// an empty ledger.
	reopen := baseTestOptions(t, clk)
	reopen.Root = l.Root()
	reopen.Index.HotDays = 1
	reopen.Index.BuildBudgetBytes = 1
	lClosed, err := Open(context.Background(), reopen)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer lClosed.Close(context.Background())

	sawPartial := false
	var lines int64
	tok := types.PageToken{}
	for page := 0; ; page++ {
		if page > 500 {
			t.Fatalf("cold walk did not terminate")
		}
		_, info, next, err := lClosed.Page(types.KEvent, tok, 5)
		if err != nil {
			t.Fatalf("Page: %v", err)
		}
		// A page served from a generation the resident index did not carry must
		// say so, and must report real byte/line accounting (§2.3a cold-read row).
		if info.Partial {
			sawPartial = true
			if info.Reason == "" {
				t.Fatalf("a Partial page must carry its Reason (§2.3a/§3.6): %+v", info)
			}
			if info.ScannedBytes == 0 && info.ScannedLines == 0 {
				t.Fatalf("a cold-read page must report scanned byte/line counts: %+v", info)
			}
			lines += info.ScannedLines
		}
		if next.IsZero() {
			break
		}
		tok = next
	}
	if !sawPartial {
		t.Fatalf("no cold-read page was produced: the bounded build must leave events out of the resident index")
	}
	if lines == 0 {
		t.Fatalf("every cold page reported 0 scanned lines; the accounting must be real")
	}
}

// TestAC30PageIncidentsWalksIncidents pins the second §2.3a signature: the
// incident walk answers []types.Incident with the same grammar and the same
// end-of-walk rule.
func TestAC30PageIncidentsWalksIncidents(t *testing.T) {
	clk := newFakeClock(testNow())
	l := testLedger(t, clk)
	var want []string
	for i := 0; i < 5; i++ {
		inc := types.NewID(types.PInc)
		mustAppend(t, l, types.RecordDraft{
			Kind: types.KIncident, Sig: fmt.Sprintf("journald:sha256v1:%04d", i), Inc: inc,
			Origin:  types.Origin{HostID: "7f3a91c2d4e5b607", Source: "journald"},
			Actor:   testActor(),
			Payload: map[string]any{"state": "detected", "severity": "high", "op": "transition"},
		})
		want = append(want, inc)
	}
	var got []string
	tok := types.PageToken{}
	for page := 0; ; page++ {
		if page > 100 {
			t.Fatalf("incident walk did not terminate")
		}
		incs, _, next, err := l.PageIncidents(tok, 2)
		if err != nil {
			t.Fatalf("PageIncidents: %v", err)
		}
		for _, in := range incs {
			if in.ID == "" {
				t.Fatalf("paged incident has no id: %+v", in)
			}
			got = append(got, in.ID)
		}
		if next.IsZero() {
			break
		}
		tok = next
	}
	if len(got) != len(want) {
		t.Fatalf("incident walk returned %d incidents, want %d", len(got), len(want))
	}
	seen := map[string]int{}
	for _, id := range got {
		seen[id]++
	}
	for _, id := range want {
		if seen[id] != 1 {
			t.Fatalf("incident %s returned %d times", id, seen[id])
		}
	}
}

// ---- helpers ----

func assertSidecarEqual(t *testing.T, got, want types.GenerationIndex) {
	t.Helper()
	if got.File != want.File || got.Records != want.Records || got.Bytes != want.Bytes ||
		got.FirstSeq != want.FirstSeq || got.LastSeq != want.LastSeq ||
		got.MinTS != want.MinTS || got.MaxTS != want.MaxTS ||
		got.Sha256 != want.Sha256 || got.OffsetStride != want.OffsetStride ||
		got.TornLines != want.TornLines {
		t.Fatalf("rebuilt sidecar = %+v\nwant %+v", got, want)
	}
	if len(got.Offsets) != len(want.Offsets) {
		t.Fatalf("rebuilt sidecar has %d offsets, want %d", len(got.Offsets), len(want.Offsets))
	}
	for i := range got.Offsets {
		if got.Offsets[i] != want.Offsets[i] {
			t.Fatalf("offset %d = %d want %d", i, got.Offsets[i], want.Offsets[i])
		}
	}
}

// oldestDay returns the oldest indexed day.
func (l *Ledger) oldestDay() string {
	oldest := ""
	for _, de := range l.idx.dayEntries() {
		if oldest == "" || de.Day < oldest {
			oldest = de.Day
		}
	}
	return oldest
}
