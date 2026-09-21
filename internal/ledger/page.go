package ledger

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// DefaultPageSize and DefaultPageSizeMax are the §2.3a clamp bounds (SPEC-01
// §2.5 keys `page_size_default` / `page_size_max`). A page size is a display
// choice, not a policy: above the maximum it is clamped, never refused.
const (
	DefaultPageSize    = 500
	DefaultPageSizeMax = 5000
)

// The §2.3a reset vocabulary, aliased from SPEC-TYPES so a caller of this
// package does not have to reach into types for it.
const (
	ResetReasonGenerationDropped = types.ResetGenerationDropped
	ResetReasonTokenInvalid      = types.ResetTokenInvalid
	ResetReasonTokenForeign      = types.ResetTokenForeign
)

// sampleStride is the in-memory sparse-sample interval the boot rebuild writes
// ("sparse `seqOffset` samples every 512 lines", SPEC-01 §3.6). The page walk
// uses it to bound how far it has to read back.
const sampleStride = 512

// PageFromTokenString is the API boundary's entry point (SPEC-10's
// `/incidents`, `/groups` and the §2.4 CLI seams all carry the token as a
// string). It is the one place the §2.3a rule "an unparsable token is a reset
// hint, never a 5xx" is applied: "" is the newest-first start, anything that
// does not parse comes back as Reset=true / ResetReason=token_invalid with the
// newest-first first page.
func (l *Ledger) PageFromTokenString(kind types.RecordKind, raw string, size int) ([]types.Record, types.QueryInfo, string, error) {
	tok, err := types.ParsePageToken(raw)
	if err != nil {
		recs, info, next, perr := l.Page(kind, types.PageToken{}, size)
		info.Reset = true
		info.ResetReason = types.ResetTokenInvalid
		info.Indexed = false
		info.Partial = true
		return recs, info, next.String(), perr
	}
	recs, info, next, perr := l.Page(kind, tok, size)
	return recs, info, next.String(), perr
}

// pageSize resolves the §2.3a clamp for one call. Absent or 0 means the default;
// above the maximum it is clamped. A negative value is a 400 at the API boundary
// (SPEC-10 owns that boundary); the ledger clamps so that no caller can produce
// an unbounded read.
func (l *Ledger) pageSize(size int) int {
	def, max := l.opts.PageSizeDefault, l.opts.PageSizeMax
	if def <= 0 {
		def = DefaultPageSize
	}
	if max <= 0 {
		max = DefaultPageSizeMax
	}
	if max < def {
		max = def
	}
	switch {
	case size <= 0:
		return def
	case size > max:
		return max
	}
	return size
}

// pagePart is one physical generation file as the walk sees it: the
// authoritative byte size plus whichever sparse index it can seek with — the
// in-memory `seqOffset` samples (seq + offset) or the §3.7a sidecar's offsets.
type pagePart struct {
	file    string
	size    int64
	samples []seqOffset // in-memory samples: every sampleStride records
	offsets []int64     // sidecar offsets: every OffsetStride records
	records int64       // sidecar record count (density of the offsets)
	indexed bool        // the index carries this file's records
}

// newestFirstParts returns the authoritative file set in newest-first order
// (day desc, generation desc, part desc) — the order a §2.3a walk returns.
func (l *Ledger) newestFirstParts() ([]pagePart, error) {
	parts, _, err := l.authoritativeFiles()
	if err != nil {
		return nil, err
	}
	infos := l.idx.partIndex()
	out := make([]pagePart, 0, len(parts))
	for _, p := range parts {
		pp := pagePart{file: p.File, size: p.Bytes, samples: p.Samples}
		ixRecords := int64(-1)
		if ix, ok := infos[p.File]; ok {
			// A part discovered after boot, or one the rebuild scanned in a
			// projected mode, may carry no samples on this copy: prefer the
			// index's, which is the one the rebuild filled.
			if len(ix.Samples) > 0 {
				pp.samples = ix.Samples
			}
			if ix.Bytes > 0 {
				pp.size = ix.Bytes
			}
			ixRecords = ix.Records + ix.Torn
		}
		// The sidecar is the seek table of last resort: it is what §3.7a
		// provides precisely so a page does not have to scan from the front. A
		// closed generation has one on disk; the live file's is derived on demand
		// and cached by size, so a page never re-hashes a file that is not
		// growing under it.
		if gi, ok := l.cachedSidecar(p.File, pp.size); ok {
			pp.offsets, pp.records = gi.Offsets, gi.Records
			// "Indexed" means the resident index carries ALL of this file's
			// records. A file the build scanned in a bounded/tiered mode has its
			// events skipped (§3.6), so a page for it is served from the sidecar
			// and the file — the cold-read path §2.3a's Partial row describes.
			// The sidecar's count is the honest denominator: it is derived from
			// the file itself.
			pp.indexed = gi.Records > 0 && ixRecords >= gi.Records
		} else {
			pp.indexed = ixRecords > 0
		}
		out = append(out, pp)
	}
	sortPartsNewestFirst(out)
	return out, nil
}

// sortPartsNewestFirst orders file names newest-first: day desc, then generation
// desc, then part desc.
func sortPartsNewestFirst(ps []pagePart) {
	sort.SliceStable(ps, func(i, j int) bool {
		di, gi, pi, oki := parsePartName(ps[i].file)
		dj, gj, pj, okj := parsePartName(ps[j].file)
		if !oki || !okj {
			return ps[i].file > ps[j].file
		}
		if di != dj {
			return di > dj
		}
		if gi != gj {
			return gi > gj
		}
		return pi > pj
	})
}

// Page walks the ledger newest-first under the §2.3a contract. The walk is
// bounded, resumable and O(page): the token names the file it points into, the
// page seeks to a sparse sample roughly `size` records below the token and reads
// forward from there — never O(file), never O(corpus).
//
// The returned types.PageToken is `next_page_token`: empty exactly when the walk
// reached the oldest record (finished, not truncated).
func (l *Ledger) Page(kind types.RecordKind, token types.PageToken, size int) ([]types.Record, types.QueryInfo, types.PageToken, error) {
	start := time.Now()
	want := l.pageSize(size)
	info := types.QueryInfo{}
	parts, err := l.newestFirstParts()
	if err != nil {
		info.ElapsedMS = int(time.Since(start).Milliseconds())
		return nil, info, types.PageToken{}, err
	}
	cur, rinfo := l.resolvePageStart(parts, token)
	info.Reset, info.ResetReason = rinfo.Reset, rinfo.ResetReason

	out := make([]pageRec, 0, want)
	var scannedBytes, scannedLines int64
	partial := false
	more := false
	for i := cur.partIndex; i < len(parts) && len(out) < want; i++ {
		p := parts[i]
		endOff, endSeq := p.size, uint64(0)
		if i == cur.partIndex {
			endOff, endSeq = cur.endOffset, cur.endSeq
		}
		if endOff <= 0 {
			continue
		}
		ch, rerr := l.readPageFromFile(p, endOff, endSeq, want-len(out), kind)
		scannedBytes += ch.scannedBytes
		scannedLines += ch.scannedLines
		if rerr != nil {
			info.ElapsedMS = int(time.Since(start).Milliseconds())
			return nil, info, types.PageToken{}, rerr
		}
		// A file the index did not carry (a bounded build's cold day) is served
		// from the sidecar + the file: a cold-read page, so Partial is set.
		if !p.indexed {
			partial = true
		}
		out = append(out, ch.recs...)
		if len(out) >= want {
			// The page filled inside this file. The walk continues only if a
			// matching record remains below the oldest one returned, or an older
			// generation might still hold one — that is what makes
			// `next_page_token` empty EXACTLY at the end of the walk, never one
			// page early and never one page late.
			more = ch.more || i+1 < len(parts)
			break
		}
	}

	records := make([]types.Record, 0, len(out))
	for _, pr := range out {
		records = append(records, pr.rec)
	}
	// `next_page_token` is empty exactly at the end of the walk: a page that
	// filled with more to come has a resume point, a page that ran out has none
	// (finished, not truncated).
	var next types.PageToken
	if len(out) == want && more {
		oldest := out[len(out)-1]
		next = types.PageToken{Generation: oldest.file, ByteOffset: oldest.off, Seq: oldest.rec.Seq}
	}
	info.Indexed = !partial
	if info.Reset {
		// A reset is a newest-first restart, never an error loop (§2.3a), and the
		// page is not an indexed continuation.
		info.Indexed = false
		info.Partial = true
		info.Reason = "outside_index_window"
	} else if partial {
		// A page served from a file the resident index did not carry is the
		// §2.3a cold-read row: Partial with the scanned counts, Indexed false.
		info.Partial = true
		info.Reason = "outside_index_window"
	}
	info.ScannedBytes = scannedBytes
	info.ScannedLines = scannedLines
	info.ElapsedMS = int(time.Since(start).Milliseconds())
	return records, info, next, nil
}

// PageIncidents is the §2.3a walk over incident records: the same token grammar,
// the same page-size clamp, the same reset hints, answering []types.Incident.
func (l *Ledger) PageIncidents(token types.PageToken, size int) ([]types.Incident, types.QueryInfo, types.PageToken, error) {
	recs, info, next, err := l.Page(types.KIncident, token, size)
	if err != nil {
		return nil, info, types.PageToken{}, err
	}
	out := make([]types.Incident, 0, len(recs))
	for i := range recs {
		if inc := l.incidentOfRecord(recs[i]); inc != nil {
			out = append(out, *inc)
		}
	}
	return out, info, next, nil
}

// incidentOfRecord runs one record through the index's incident projection, so a
// paged incident is the same object the index would report — one field list, one
// code path.
func (l *Ledger) incidentOfRecord(rec types.Record) *types.Incident {
	tmp := newIndex(l.opts.Index, l.root, l.opts.Zone, l.opts.MaxSchema)
	tmp.applyLocked(&rec)
	var best *types.Incident
	for _, in := range tmp.incidents {
		if best == nil || in.UpdatedTS >= best.UpdatedTS {
			cp := *in
			best = &cp
		}
	}
	return best
}

// pageStart is the resolved resume position of one page.
type pageStart struct {
	partIndex int    // index into the newest-first part list
	endOffset int64  // exclusive upper byte bound inside that part
	endSeq    uint64 // 0 = no seq constraint
	exhausted bool   // nothing left to serve
}

// resolvePageStart turns a token into a resume position, or into the §2.3a reset
// outcome. The reset vocabulary is exactly the spec's three reasons (§2.3a
// outcome table):
//
//   - token_invalid — the grammar does not parse, or the generation is not
//     ledger file naming at all, so it addresses nothing this host can serve.
//   - generation_dropped — the generation is no longer authoritative and this
//     host knows the day: the retention sweep dropped it (§3.7a) or a compaction
//     superseded it. The answer is a newest-first first page.
//   - token_foreign — the generation names a day this host has never indexed
//     (another host's file). The token grammar is namespaced by the generation
//     file name, which is what stops a satellite's token from silently
//     addressing the hub's file of the same day.
func (l *Ledger) resolvePageStart(parts []pagePart, token types.PageToken) (pageStart, types.QueryInfo) {
	info := types.QueryInfo{}
	if token.IsZero() {
		if len(parts) == 0 {
			return pageStart{partIndex: 0, exhausted: true}, info
		}
		return pageStart{partIndex: 0, endOffset: parts[0].size}, info
	}
	for i, p := range parts {
		if p.file == token.Generation {
			end := token.ByteOffset
			if end <= 0 || end > p.size {
				// The file changed under the walk (a part rollover, a
				// compaction): seq is what makes the resume safe (§2.3a), so the
				// byte bound is relaxed to the whole file and the seq guard in
				// readPageFromFile keeps the page honest.
				end = p.size
			}
			return pageStart{partIndex: i, endOffset: end, endSeq: token.Seq}, info
		}
	}
	switch {
	case !fileRe.MatchString(token.Generation):
		info.Reset, info.ResetReason = true, types.ResetTokenInvalid
	case tokenDayBeforeFloor(token.Generation, l.oldestKnownDay()):
		// Older than anything this ledger has ever indexed: not a generation of
		// ours that was swept, but a generation we never had.
		info.Reset, info.ResetReason = true, types.ResetTokenForeign
	default:
		info.Reset, info.ResetReason = true, types.ResetGenerationDropped
	}
	if len(parts) == 0 {
		return pageStart{partIndex: 0, exhausted: true}, info
	}
	// The reset answer is the newest-first first page.
	return pageStart{partIndex: 0, endOffset: parts[0].size}, info
}

// knowsDay reports whether the index holds any file for the day a generation
// name belongs to.
func (l *Ledger) knowsDay(name string) bool {
	day, _, _, ok := parsePartName(name)
	if !ok {
		return false
	}
	return l.idx.day(day) != nil
}

// oldestKnownDay is the oldest day this ledger still indexes — the retention
// boundary the sweep drops behind (§3.7a).
func (l *Ledger) oldestKnownDay() string {
	oldest := ""
	for _, de := range l.idx.dayEntries() {
		if oldest == "" || de.Day < oldest {
			oldest = de.Day
		}
	}
	return oldest
}

// tokenDayBeforeFloor reports whether a generation name's day is strictly older
// than the floor day. A generation older than everything this host has ever
// indexed was never this host's generation, which is what separates a
// token_foreign answer from a token the sweep dropped.
//
// The floor is the oldest indexed day when one exists. A ledger that indexed
// nothing yet has no floor, so nothing is foreign to it and everything answers
// with the (equally non-error) dropped reason — both are reset hints, and the
// reason is diagnostic, not a behavioural difference.
func tokenDayBeforeFloor(gen string, floor string) bool {
	day, _, _, ok := parsePartName(gen)
	if !ok || floor == "" {
		return false
	}
	return day < floor
}

// pageRec is one record with the provenance the next token needs.
type pageRec struct {
	rec  types.Record
	file string
	off  int64
}

// pageChunk is what one file contributed to a page: its records (newest-first),
// the bytes/lines read, and whether a matching record still remains below the
// oldest record returned — the signal that decides an empty `next_page_token`.
type pageChunk struct {
	recs         []pageRec
	scannedBytes int64
	scannedLines int64
	more         bool
}

// readPageFromFile reads up to want records of kind from one file, newest first,
// at or below the byte bound endOff.
//
// The read walks BACKWARD in disjoint chunks: it seeks to a sparse sample roughly
// `want` records below the bound, reads forward to the bound, then steps the bound
// down to that sample and repeats. Each record is therefore read exactly once, a
// page never reads a file end to end unless it is the last page, and a kind
// filter that matches few records in one chunk cannot silently skip the earlier
// ones (the bug a single-chunk window would have).
func (l *Ledger) readPageFromFile(p pagePart, endOff int64, endSeq uint64, want int, kind types.RecordKind) (pageChunk, error) {
	var ch pageChunk
	if want <= 0 || endOff <= 0 {
		return ch, nil
	}
	if endOff > p.size {
		endOff = p.size
	}
	f, err := os.Open(filepath.Join(l.root, p.file))
	if err != nil {
		return ch, ledgerErr(types.CodeLedger012, ReasonIO,
			fmt.Sprintf("cannot open %s", p.file), err)
	}
	defer f.Close()

	acc := make([]pageRec, 0, want) // oldest-first accumulation
	end := endOff
	floor := pageFloorOffset(p, endSeq)
	readTotal := 0
	for {
		start := pageSeekOffset(p, end, endSeq, want, kind)
		if start < floor {
			start = floor
		}
		if start >= end {
			break
		}
		recs, b, lines, rerr := l.readRange(f, p.file, start, end, endSeq, kind)
		ch.scannedBytes += b
		ch.scannedLines += lines
		if rerr != nil {
			return ch, rerr
		}
		readTotal += len(recs)
		// recs is oldest-first and older than everything accumulated so far.
		acc = append(recs, acc...)
		if len(acc) >= want {
			break
		}
		if start <= floor {
			break
		}
		end = start
	}
	if len(acc) == 0 {
		return ch, nil
	}
	// The file is seq-ascending, so keep the newest `want` and reverse into the
	// walk's newest-first order.
	if len(acc) > want {
		acc = acc[len(acc)-want:]
	}
	ch.recs = make([]pageRec, 0, len(acc))
	for i := len(acc) - 1; i >= 0; i-- {
		ch.recs = append(ch.recs, acc[i])
	}
	// Is there anything left below the oldest record this page returns? Reading
	// more records than the page kept answers it for free; otherwise one extra
	// bounded range read answers it, and that read is small by construction: if
	// it finds nothing in its window there is nothing between the window and the
	// floor either. The probe only runs when the page landed exactly on a
	// boundary, which is what keeps a normal page inside the §3.7a bound.
	if want <= len(ch.recs) {
		switch {
		case readTotal > len(ch.recs):
			ch.more = true
		default:
			oldestOff := acc[0].off
			if oldestOff > floor {
				b0 := pageSeekOffsetBack(p, oldestOff)
				if b0 < floor {
					b0 = floor
				}
				recs, b, lines, rerr := l.readRange(f, p.file, b0, oldestOff, endSeq, kind)
				ch.scannedBytes += b
				ch.scannedLines += lines
				if rerr != nil {
					return ch, rerr
				}
				ch.more = len(recs) > 0
			}
		}
	}
	return ch, nil
}

// pageFloorOffset is the byte offset below which this walk's records cannot
// start, for a file and a resume seq: the newest sample at or below endSeq. The
// walk must not step past it, because the records below it are already outside
// the walk's window (they were returned by an earlier page).
func pageFloorOffset(p pagePart, endSeq uint64) int64 {
	if endSeq == 0 {
		return 0
	}
	// newest sample with Seq <= endSeq
	k := sort.Search(len(p.samples), func(i int) bool { return p.samples[i].Seq > endSeq }) - 1
	if k < 0 {
		return 0
	}
	return p.samples[k].Offset
}

// pageSeekOffsetBack returns a byte offset a bounded distance below off, used to
// probe whether a matching record remains below the page's oldest record. It
// prefers the sidecar's stride and falls back to the in-memory samples.
func pageSeekOffsetBack(p pagePart, off int64) int64 {
	if len(p.offsets) > 1 && p.records > 1 && p.size > 0 {
		per := float64(p.size) / float64(p.records)
		target := off - int64(float64(DefaultOffsetStride)*per)
		if target <= 0 {
			return 0
		}
		k := sort.Search(len(p.offsets), func(i int) bool { return p.offsets[i] > target }) - 1
		if k < 0 {
			return 0
		}
		return p.offsets[k]
	}
	if len(p.samples) > 0 {
		k := sort.Search(len(p.samples), func(i int) bool { return p.samples[i].Offset >= off }) - 1
		if k < 0 {
			return 0
		}
		if k-samplesPerProbe >= 0 {
			return p.samples[k-samplesPerProbe].Offset
		}
		return p.samples[0].Offset
	}
	return 0
}

// samplesPerProbe is how many in-memory samples the end probe steps back: one
// sample, so the probe window is small and still guaranteed non-vacuous when the
// page sits deep in a file.
const samplesPerProbe = 1

// readRange reads the half-open byte range [start, end) and returns the records
// of kind in ascending seq order. endSeq is applied as the resume guard: every
// record with seq >= endSeq was already returned by an earlier page, because the
// file is seq-ascending.
//
// Offsets are made absolute here: lineReader counts from the seek position, so
// its start is relative to `start`, and a page token built from a relative
// offset would address the wrong byte in the file.
func (l *Ledger) readRange(f *os.File, file string, start, end int64, endSeq uint64, kind types.RecordKind) ([]pageRec, int64, int64, error) {
	if _, err := f.Seek(start, 0); err != nil {
		return nil, 0, 0, ledgerErr(types.CodeLedger012, ReasonIO, "seek failed", err)
	}
	rd := newLineReader(f)
	var out []pageRec
	var scannedBytes, scannedLines int64
	for {
		line, rel, complete, rerr := rd.next()
		if rerr != nil {
			return nil, scannedBytes, scannedLines,
				ledgerErr(types.CodeLedger012, ReasonIO, "read failed", rerr)
		}
		if len(line) == 0 && !complete {
			break
		}
		off := start + rel
		if !complete {
			// a torn tail is never a record (§3.1); it ends the readable range
			break
		}
		lineEnd := off + int64(len(line)) + 1
		if lineEnd > end {
			break
		}
		scannedBytes += int64(len(line)) + 1
		scannedLines++
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		rec, derr := decodeRecord(line, true)
		if derr != nil || rec == nil || rec.SchemaVersion > l.opts.MaxSchema {
			continue
		}
		if endSeq != 0 && rec.Seq >= endSeq {
			continue
		}
		if kind != "" && rec.Kind != kind {
			continue
		}
		out = append(out, pageRec{rec: *rec, file: file, off: off})
	}
	return out, scannedBytes, scannedLines, nil
}

// pageSeekOffset picks the byte offset a page starts reading from.
//
// The §3.7a sidecar is the preferred seek table — its `Offsets` are one sample
// every `offset_stride` records, which is exactly what makes the spec's bound
// (`offset_stride + page_size` lines per page) true. The in-memory `seqOffset`
// samples (every `sampleStride` records, §3.6) are the fallback: a seq anchor
// when the walk knows the seq it resumes from, and whole-interval back-off
// otherwise. Both paths round UP the interval count, so the window is never
// smaller than the records the page still has to yield.
func pageSeekOffset(p pagePart, endOff int64, endSeq uint64, want int, kind types.RecordKind) int64 {
	if len(p.offsets) > 1 && p.records > 1 && p.size > 0 {
		per := float64(p.size) / float64(p.records)
		// Seek `want` records below the token, then take the nearest recorded
		// offset at or below that target: the distance from the sample to the
		// target is at most one `offset_stride` interval, so the page reads at
		// most `offset_stride + page_size` records — the §3.7a contract.
		target := endOff - int64(float64(want)*per)
		if target <= 0 {
			return 0
		}
		k := sort.Search(len(p.offsets), func(i int) bool { return p.offsets[i] > target }) - 1
		if k < 0 {
			return 0
		}
		if p.offsets[k] < endOff {
			return p.offsets[k]
		}
	}
	if len(p.samples) > 0 {
		anchor := endSeq
		if anchor == 0 {
			anchor = p.samples[len(p.samples)-1].Seq
		}
		if anchor > 0 {
			target := uint64(0)
			if anchor > uint64(want) {
				target = anchor - uint64(want)
			}
			k := sort.Search(len(p.samples), func(i int) bool { return p.samples[i].Seq > target }) - 1
			if k >= 0 && p.samples[k].Offset < endOff {
				return p.samples[k].Offset
			}
			return 0
		}
	}
	return 0
}
