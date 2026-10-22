package ledger

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/trouble-agent/trouble/internal/types"
)

// recoveredState is what a successful recovery hands to the writer.
type recoveredState struct {
	day     string
	gen     int
	part    int
	bytes   int64
	records int64
	lastSeq uint64
	lastTS  string
	// recoverHoles is the set of seq gaps discovered during recovery scanning.
	// It includes gaps from HEAD.last_seq > maxSeq (§3.4 rule 7) and gaps
	// detected mid-scan (seq jumps between consecutive records). The writer's
	// beforeBatch drains this into one lifecycle{op:"hole"} record per gap
	// with the next successful batch (§3.5: every hole appears in the audit trail).
	recoverHoles []holeRange
}

// lineReader yields \n-terminated lines without buffering the whole file, and
// reports whether the final line was terminated (SPEC-01 §3.1: a record is
// always exactly one line).
type lineReader struct {
	r   *bufio.Reader
	off int64
}

func newLineReader(f *os.File) *lineReader {
	return &lineReader{r: bufio.NewReaderSize(f, 1<<20)}
}

// next returns the next line, its start offset, and whether it was terminated.
func (lr *lineReader) next() (line []byte, start int64, complete bool, err error) {
	start = lr.off
	b, rerr := lr.r.ReadBytes('\n')
	lr.off += int64(len(b))
	switch {
	case rerr == nil:
		return bytes.TrimRight(b, "\n"), start, true, nil
	case rerr == io.EOF && len(b) > 0:
		return b, start, false, nil
	case rerr == io.EOF:
		return nil, start, false, nil
	default:
		return nil, start, false, rerr
	}
}

// walkLines streams a file's lines to fn with the tail flag set on the final
// line. A final line that does not terminate in \n, or that does not parse, is
// the tail of a crash (SPEC-01 §5 code 003) rather than interior corruption
// (011) — that distinction is why the lookahead exists.
//
// An INTERIOR line with the byte shape of a torn fragment (a truncated JSON
// value: the mark a crash leaves mid-write, §6.4 rule 4) is also code 003, not
// interior garbage: walkLines hands it to fn with isTail=true and keeps
// walking, so every complete line around it is delivered in file order with
// absolute offsets and one pass sees the whole file. Interior garbage (rule 5)
// keeps the old path: a counted, unhealed 011 line.
func walkLines(f *os.File, fn func(line []byte, start int64, isTail bool) error) error {
	rd := newLineReader(f)
	var pending []byte
	var pendingStart int64
	have := false
	for {
		line, start, complete, err := rd.next()
		if err != nil {
			return err
		}
		if len(line) == 0 && !complete {
			// EOF: whatever is pending is the tail
			if have {
				if err := fn(pending, pendingStart, true); err != nil {
					return err
				}
			}
			return nil
		}
		if !complete {
			// the tail has no terminating newline at all
			if have {
				if err := fn(pending, pendingStart, false); err != nil {
					return err
				}
			}
			return fn(line, start, true)
		}
		if len(bytes.TrimSpace(line)) > 0 && tornShaped(line) {
			// a fragment wherever it sits: flush the pending complete line,
			// deliver the fragment as torn, keep walking — one continuous
			// pass, so every offset fn sees stays absolute
			if have {
				if err := fn(pending, pendingStart, false); err != nil {
					return err
				}
				have = false
			}
			if err := fn(line, start, true); err != nil {
				return err
			}
			continue
		}
		if have {
			if err := fn(pending, pendingStart, false); err != nil {
				return err
			}
		}
		pending, pendingStart, have = line, start, true
	}
}

// tornShaped reports whether an unparsable line is a truncated JSON value —
// the byte shape a crash leaves mid-write (SPEC-01 §6.4 rule 4) — rather than
// complete content that never parsed (§6.4 rule 5: interior corruption).
// json.Valid keeps the common case (a line that parses) to one allocation-free
// scan; only invalid lines pay the streaming decoder.
func tornShaped(line []byte) bool {
	if json.Valid(line) {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	var v any
	err := dec.Decode(&v)
	return errors.Is(err, io.ErrUnexpectedEOF)
}

// acquireLock takes flock(LOCK_EX|LOCK_NB) on ledger/LOCK (SPEC-01 §3.4 rule 6).
func acquireLock(root string, actor types.Actor) (*os.File, error) {
	path := filepath.Join(root, "LOCK")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, ledgerErr(types.CodeLedger012, ReasonIO,
			fmt.Sprintf("cannot open %s", path), err)
	}
	if err := flockAcquire(f); err != nil {
		body, _ := io.ReadAll(f)
		f.Close()
		return nil, ledgerErr(types.CodeLedger005, ReasonWrite,
			fmt.Sprintf("another writer holds %s: %s", path, string(bytes.TrimSpace(body))), err)
	}
	if err := f.Truncate(0); err == nil {
		_, _ = f.Seek(0, io.SeekStart)
		payload, _ := json.Marshal(lockFile{
			PID:     os.Getpid(),
			Version: actor.Version,
			GitSHA:  actor.GitSHA,
			BootTS:  types.NowUTC(),
			Root:    root,
		})
		_, _ = f.Write(append(payload, '\n'))
		_ = f.Sync()
	}
	return f, nil
}

func releaseLock(f *os.File) error {
	if f == nil {
		return nil
	}
	_ = flockRelease(f)
	return f.Close()
}

// readHead reads the HEAD hint (never a source of truth).
func (l *Ledger) readHead() (headFile, bool) {
	b, err := os.ReadFile(filepath.Join(l.root, "HEAD"))
	if err != nil {
		return headFile{}, false
	}
	var h headFile
	if err := json.Unmarshal(bytes.TrimSpace(b), &h); err != nil {
		return headFile{}, false
	}
	return h, true
}

// recover takes the authoritative file set, resolves an interrupted compaction,
// indexes everything, heals a torn last line and recovers the seq allocator
// (SPEC-01 §3.4 rule 3/7, §3.5).
func (l *Ledger) recover(ctx context.Context) error {
	parts, stale, err := l.authoritativeFiles()
	if err != nil {
		return err
	}
	for _, f := range stale {
		// Interrupted compaction commit: the higher generation wins, the lower
		// one is unlinked and the interruption is recorded (TROUBLE-LEDGER-007).
		if rmErr := os.Remove(filepath.Join(l.root, f.File)); rmErr != nil {
			l.idx.markDegraded(ReasonIO)
		}
		l.bootNotes = append(l.bootNotes, map[string]any{
			"op":            ReasonCompactionRes,
			"day":           f.Day,
			"gen":           f.Gen,
			"file":          f.File,
			"authoritative": partName(f.Day, f.Gen, f.Part),
			"error_code":    string(types.CodeLedger007),
		})
	}
	if err := l.idx.build(parts); err != nil {
		// A degraded index is visible, a failed start is not (code 008).
		l.bootNotes = append(l.bootNotes, map[string]any{
			"op":         ReasonIndexDegraded,
			"reason":     ReasonIO,
			"error_code": string(types.CodeLedger008),
			"detail":     err.Error(),
		})
	}
	st := recoveredState{}
	// Merge seq gaps detected during the index scan into the recovery state.
	// Every discontinuity the scan found (seq jump between consecutive records)
	// must appear in the audit trail as lifecycle{op:"hole"}, per §3.5.
	st.recoverHoles = make([]holeRange, 0)
	for _, p := range parts {
		st.recoverHoles = append(st.recoverHoles, p.Gaps...)
	}
	// Seed recoverHoles with the HEAD-max rule gap.
	head, haveHead := l.readHead()
	if haveHead {
		st.lastSeq = head.LastSeq
	}
	var highest *partInfo
	maxSeq := uint64(0)
	for _, p := range parts {
		if p.LastSeq > maxSeq {
			maxSeq = p.LastSeq
		}
	}
	if maxSeq > st.lastSeq {
		st.lastSeq = maxSeq
	}
	if haveHead && head.LastSeq > maxSeq {
		l.bootNotes = append(l.bootNotes, map[string]any{
			"op": "seq_recovered", "head_last_seq": head.LastSeq, "file_max_seq": maxSeq,
		})
		l.idx.mu.Lock()
		clean := !l.idx.buildStats.Degraded
		l.idx.mu.Unlock()
		if clean {
			st.recoverHoles = append(st.recoverHoles, holeRange{from: maxSeq + 1, to: head.LastSeq})
		}
	}
	if len(parts) > 0 {
		highest = parts[len(parts)-1]
	}
	// torn-line recovery: exactly one line is dropped and the file gets a \n
	// before the next record (SPEC-01 §6.4). A tail fragment is healed by
	// appending the missing \n; an INTERIOR fragment (a crash mid-write, §6.4
	// rule 4) is healed by blanking its bytes in place, so the byte layout
	// after the fragment never moves and every recorded offset stays valid.
	torn := 0
	for _, p := range parts {
		if p.Torn == 0 {
			continue
		}
		torn += int(p.Torn)
		if interior := p.Torn - p.TailTorn; interior > 0 {
			if err := healInteriorTorn(filepath.Join(l.root, p.File)); err != nil {
				l.idx.markDegraded(ReasonIO)
			}
		}
		if p.TailTorn > 0 {
			if err := healTorn(filepath.Join(l.root, p.File)); err != nil {
				l.idx.markDegraded(ReasonIO)
				continue
			}
		}
		// Record torn-line recovery with auditable seq-range metadata.
		// The torn record fell somewhere between p.FirstSeq and p.LastSeq;
		// the trail needs from_seq / to_seq to be auditable.
		l.bootNotes = append(l.bootNotes, map[string]any{
			"op":         ReasonRecoverTorn,
			"file":       p.File,
			"error_code": string(types.CodeLedger003),
			"lines_lost": 1,
			"from_seq":   p.FirstSeq,
			"to_seq":     p.FirstSeq,
		})
	}
	if torn > 0 {
		l.idx.mu.Lock()
		l.idx.buildStats.TornLines = torn
		l.idx.mu.Unlock()
	}
	if highest != nil {
		st.day = highest.Day
		st.gen = highest.Gen
		st.part = highest.Part
		b, recs, last := l.idx.partStats(highest.Day, highest.Gen, highest.Part)
		st.bytes = b
		st.records = recs
		st.lastTS = highest.LastTS
		if last > st.lastSeq {
			st.lastSeq = last
		}
	} else {
		st.day = l.now().UTC().Format(dayLayout)
		st.gen = 0
		st.part = 1
	}
	// The writer's continuation file must exist even when nothing was indexed.
	if highest != nil {
		l.recovered = st
		return nil
	}
	l.recovered = st
	return nil
}

// healTorn appends the missing "\n" so exactly the torn fragment is dropped
// and the next record starts on a fresh line.
func healTorn(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write([]byte("\n")); err != nil {
		return err
	}
	return f.Sync()
}

// healInteriorTorn neutralizes every interior torn fragment of a file by
// overwriting its bytes with spaces at the same offsets, so the byte layout
// after the fragment never shifts (in-memory samples and sidecar offsets
// stay valid) and no line after it is ever consumed. A blanked interior line
// is the same content the tail heal leaves behind (§6.4): scanner callers skip
// blank lines, and a later boot counts nothing and heals nothing again. A
// healing failure leaves the fragment torn-shaped — re-detected and counted at
// the next boot, degraded at this one: never silent.
func healInteriorTorn(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	blanked := false
	for {
		start, end, found, rerr := findInteriorTorn(f)
		if rerr != nil {
			return rerr
		}
		if !found {
			if blanked {
				return f.Sync()
			}
			return nil
		}
		pad := make([]byte, end-start)
		for i := range pad {
			pad[i] = ' '
		}
		if _, werr := f.WriteAt(pad, start); werr != nil {
			return werr
		}
		blanked = true
	}
}

// findInteriorTorn locates the first interior torn fragment (a torn-shaped
// line with a terminating '\n') in the file, returning its byte range
// [start, end), or found=false when none remains.
func findInteriorTorn(f *os.File) (start, end int64, found bool, err error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, 0, false, err
	}
	rd := newLineReader(f)
	for {
		line, lstart, complete, rerr := rd.next()
		if rerr != nil {
			return 0, 0, false, rerr
		}
		if len(line) == 0 && !complete {
			return 0, 0, false, nil
		}
		if complete && len(bytes.TrimSpace(line)) > 0 && tornShaped(line) {
			return lstart, lstart + int64(len(line)) + 1, true, nil
		}
	}
}

// authoritativeFiles lists the authoritative part set in seq order and the
// stale files an interrupted compaction left behind.
func (l *Ledger) authoritativeFiles() ([]*partInfo, []*partInfo, error) {
	ents, err := os.ReadDir(l.root)
	if err != nil {
		return nil, nil, ledgerErr(types.CodeLedger012, ReasonIO, "cannot read ledger dir", err)
	}
	byDay := map[string][]*partInfo{}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		m := fileRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		gen := 0
		part := 1
		if m[2] != "" {
			part, _ = strconv.Atoi(m[2])
		}
		if m[3] != "" {
			gen, _ = strconv.Atoi(m[3])
		}
		fi, ferr := e.Info()
		if ferr != nil {
			return nil, nil, ledgerErr(types.CodeLedger012, ReasonIO,
				fmt.Sprintf("cannot stat %s", e.Name()), ferr)
		}
		byDay[m[1]] = append(byDay[m[1]], &partInfo{
			Day: m[1], Gen: gen, Part: part, File: e.Name(), Bytes: fi.Size(),
		})
	}
	days := make([]string, 0, len(byDay))
	for d := range byDay {
		days = append(days, d)
	}
	sort.Strings(days)
	var files, stale []*partInfo
	for _, d := range days {
		ps := byDay[d]
		maxGen := 0
		for _, p := range ps {
			if p.Gen > maxGen {
				maxGen = p.Gen
			}
		}
		var keep []*partInfo
		for _, p := range ps {
			if p.Gen == maxGen {
				keep = append(keep, p)
			} else {
				stale = append(stale, p)
			}
		}
		sortParts(keep)
		files = append(files, keep...)
	}
	return files, stale, nil
}

// ---- read-only verification ----

// SeqHole is one non-contiguous seq range.
type SeqHole struct {
	From uint64 `json:"from"`
	To   uint64 `json:"to"`
}

// seqSpan is one inclusive seq range. A verified day's file needs to account for
// every seq it no longer carries a line for; a compaction aggregate accounts for
// the range it folded into itself.
type seqSpan struct{ from, to uint64 }

// aggSeqSpans reads the seq ranges a compaction aggregate declares consumed in
// payload.agg_seqs (`[first..last]` plus explicit exceptions, SPEC-01 §3.7 rule
// 5) and returns them along with the aggregate's declared event count.
//
// Both verifiers call it: without it a compacted generation reads as a ledger
// full of seq holes and undeclared tombstones (TRBL-117 — one healthy
// compaction produced 25 seq_holes and 101 tombstone_mismatches). The declared
// count comes from counters.events, falling back to events_aggregated; a
// payload that carries no usable range returns no spans, so the caller keeps the
// strict contiguous-seq behaviour for a generation that has no aggregate at all.
//
// An exception names a seq that was NOT consumed even though it lies inside
// [first..last]: an integer seq splits the range, a {from,to} object (or a
// [from,to] pair) carves a sub-range, and an exception that falls outside the
// declared range is ignored. A malformed agg_seqs leaves the record with no
// spans rather than guessing.
func aggSeqSpans(rec *types.Record) (spans []seqSpan, declared uint64) {
	if rec == nil || rec.Payload == nil {
		return nil, 0
	}
	raw, ok := rec.Payload["agg_seqs"]
	if !ok {
		return nil, 0
	}
	block, ok := raw.(map[string]any)
	if !ok {
		return nil, 0
	}
	first := uint64(numOf(block["first"]))
	last := uint64(numOf(block["last"]))
	// An aggregate that carries the explicit consumed-seq list states exactly
	// what it folded; the [first..last] range cannot, because the writer
	// interleaves groups and the folded seqs of one group are sparse. Prefer
	// the list when present and fall back to the range for older aggregates.
	if list, ok := block["seqs"].([]any); ok && len(list) > 0 {
		numeric := make([]uint64, 0, len(list))
		for _, v := range list {
			if n := uint64(numOf(v)); n != 0 {
				numeric = append(numeric, n)
			}
		}
		if len(numeric) > 0 {
			sort.Slice(numeric, func(i, j int) bool { return numeric[i] < numeric[j] })
			start := numeric[0]
			prev := numeric[0]
			for _, n := range numeric[1:] {
				if n == prev {
					continue
				}
				if n == prev+1 {
					prev = n
					continue
				}
				spans = append(spans, seqSpan{start, prev})
				start, prev = n, n
			}
			spans = append(spans, seqSpan{start, prev})
			declared = uint64(len(numeric))
			if c, ok := rec.Payload["counters"].(map[string]any); ok {
				if n := uint64(numOf(c["events"])); n != 0 {
					declared = n
				}
			} else if n := uint64(numOf(block["count"])); n != 0 {
				declared = n
			}
			if n := uint64(numOf(rec.Payload["events_aggregated"])); n != 0 && declared == 0 {
				declared = n
			}
			return spans, declared
		}
	}
	if first == 0 || last < first {
		return nil, 0
	}
	// The exceptions carve consumed seqs out of [first..last]; everything else
	// in the range was folded into this aggregate.
	var except []seqSpan
	if list, ok := block["exceptions"].([]any); ok {
		for _, e := range list {
			switch v := e.(type) {
			case map[string]any:
				ef, el := uint64(numOf(v["from"])), uint64(numOf(v["to"]))
				if ef == 0 {
					ef = uint64(numOf(v["first"]))
				}
				if el == 0 {
					el = uint64(numOf(v["last"]))
				}
				if ef != 0 && el >= ef {
					except = append(except, seqSpan{ef, el})
				}
			case []any:
				if len(v) == 2 {
					ef, el := uint64(numOf(v[0])), uint64(numOf(v[1]))
					if ef != 0 && el >= ef {
						except = append(except, seqSpan{ef, el})
					}
				}
			case float64, int, int64, json.Number:
				if n := uint64(numOf(v)); n >= first && n <= last {
					except = append(except, seqSpan{n, n})
				}
			}
		}
	}
	sort.Slice(except, func(i, j int) bool { return except[i].from < except[j].from })
	cur := first
	for _, ex := range except {
		if ex.to < cur {
			continue
		}
		if ex.from > last {
			break
		}
		if ex.from > cur {
			spans = append(spans, seqSpan{cur, ex.from - 1})
		}
		if ex.to+1 > cur {
			cur = ex.to + 1
		}
	}
	if cur <= last {
		spans = append(spans, seqSpan{cur, last})
	}
	counted := uint64(0)
	for _, s := range spans {
		counted += s.to - s.from + 1
	}
	declared = uint64(numOf(rec.Payload["events_aggregated"]))
	if c, ok := rec.Payload["counters"].(map[string]any); ok {
		if n := uint64(numOf(c["events"])); n != 0 {
			declared = n
		}
	}
	if declared == 0 {
		declared = counted
	}
	return spans, declared
}

// verifyContinuity folds the seqs a day's lines actually carry together with
// the ranges its compaction aggregates declare consumed, and reports the seq
// ranges that neither accounts for. A line's own seq is always accounted for;
// an aggregate accounts for the folded events whose lines compaction replaced
// (TRBL-117). An aggregate may not account for a seq a line still carries.
func verifyContinuity(seen map[uint64]struct{}, consumed []seqSpan) []SeqHole {
	if len(seen) == 0 {
		return nil
	}
	max := uint64(0)
	for s := range seen {
		if s > max {
			max = s
		}
	}
	for _, sp := range consumed {
		if sp.to > max {
			max = sp.to
		}
	}
	spans := make([]seqSpan, 0, len(consumed))
	for _, sp := range consumed {
		if sp.to >= sp.from {
			spans = append(spans, sp)
		}
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].from < spans[j].from })

	var holes []SeqHole
	var start, end uint64
	record := func(s, e uint64) {
		if start == 0 {
			start, end = s, e
			return
		}
		if s == end+1 {
			end = e
			return
		}
		holes = append(holes, SeqHole{From: start, To: end})
		start, end = s, e
	}
	idx := 0
	for s := uint64(1); s <= max; s++ {
		if _, ok := seen[s]; ok {
			continue
		}
		for idx < len(spans) && spans[idx].to < s {
			idx++
		}
		if idx < len(spans) && spans[idx].from <= s {
			continue
		}
		record(s, s)
	}
	if start != 0 {
		holes = append(holes, SeqHole{From: start, To: end})
	}
	return holes
}

// eventSeqSet is the tombstone-reconciliation denominator for one group
// (TRBL-117): the seqs its compaction aggregate declares consumed, PLUS any
// surviving event/canary line whose own seq falls inside those ranges. A line
// the aggregate did not fold (it stayed verbatim in the generation file) is not
// part of what the aggregate declares, so it must not inflate the count.
// Without the agg_seqs half, every compacted group looked short by the width of
// its folded range; without the in-range restriction, a surviving line outside
// the range made it look long.
func eventSeqSet(lines map[uint64]struct{}, consumed []seqSpan) uint64 {
	set := make(map[uint64]struct{}, len(lines))
	for _, sp := range consumed {
		if sp.to < sp.from {
			continue
		}
		for s := sp.from; s <= sp.to; s++ {
			set[s] = struct{}{}
		}
	}
	for s := range lines {
		for _, sp := range consumed {
			if s >= sp.from && s <= sp.to {
				set[s] = struct{}{}
				break
			}
		}
	}
	return uint64(len(set))
}

// VerifyReport is the read-only walk result (SPEC-01 §2.2).
type VerifyReport struct {
	Day                   string              `json:"day"`
	Files                 []string            `json:"files"`
	Lines                 int64               `json:"lines"`
	SeqHoles              []SeqHole           `json:"seq_holes"`
	TornLines             int                 `json:"torn_lines"`
	CorruptLines          int                 `json:"corrupt_lines"`
	NewerSchema           int                 `json:"newer_schema"`
	PayloadHashMismatches int                 `json:"payload_hash_mismatches"`
	TombstoneMismatches   int                 `json:"tombstone_mismatches"`
	Counts                types.GroupCounters `json:"counts"`
	OK                    bool                `json:"ok"`
	Errors                []string            `json:"errors"`
}

// Verify walks the day's authoritative files read-only: seq continuity, line
// parse, payload hash, tombstone totals. It opens no lock (SPEC-01 §2.4).
func (l *Ledger) Verify(ctx context.Context, day string) (VerifyReport, error) {
	parts, _, err := l.authoritativeFiles()
	if err != nil {
		return VerifyReport{}, err
	}
	rep := VerifyReport{Day: day, OK: true}
	type aggInfo struct {
		declared uint64
		seqs     map[uint64]struct{}
		spans    []seqSpan
	}
	aggs := map[string]*aggInfo{}
	// Seq continuity is a PER-DAY property: every day file starts from its own
	// seq base, so a walk spanning several days must not compare one day's last
	// seq against the next day's first (that invented a hole at every day
	// boundary and, worse, let one day's seqs mask another day's real hole).
	seen := map[uint64]struct{}{}
	var consumed []seqSpan
	curDay := ""
	flushDay := func() {
		if curDay == "" {
			return
		}
		rep.SeqHoles = append(rep.SeqHoles, verifyContinuity(seen, consumed)...)
		seen = map[uint64]struct{}{}
		consumed = nil
	}
	for _, p := range parts {
		if day != "" && p.Day != day {
			continue
		}
		if p.Day != curDay {
			flushDay()
			curDay = p.Day
		}
		rep.Files = append(rep.Files, p.File)
		f, ferr := os.Open(filepath.Join(l.root, p.File))
		if ferr != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: %v", p.File, ferr))
			rep.OK = false
			continue
		}
		werr := walkLines(f, func(line []byte, _ int64, isTail bool) error {
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			if len(bytes.TrimSpace(line)) == 0 {
				return nil
			}
			rec, derr := decodeRecord(line, true)
			if derr != nil || rec == nil {
				if isTail {
					rep.TornLines++
					rep.Errors = append(rep.Errors, fmt.Sprintf("%s: torn last line", p.File))
				} else {
					rep.CorruptLines++
					rep.Errors = append(rep.Errors, fmt.Sprintf("%s: unparsable line", p.File))
				}
				return nil
			}
			if rec.SchemaVersion > l.opts.MaxSchema {
				rep.NewerSchema++
				return nil
			}
			rep.Lines++
			// A zero seq is not a continuity break to special-case here: it is
			// absent from every accounted range, so verifyContinuity reports it
			// like any other unaccounted seq.
			seen[rec.Seq] = struct{}{}
			if h, ok := rec.Payload["payload_sha256"].(string); ok && h != "" {
				got, herr := verifyPayloadHashLine(line)
				switch {
				case herr != nil:
					// The line parsed (decodeRecord succeeded) but its payload
					// could not be re-read for the hash: never silently skipped.
					rep.PayloadHashMismatches++
					rep.Errors = append(rep.Errors,
						fmt.Sprintf("seq %d: payload hash unverifiable: %v", rec.Seq, herr))
				case got != h:
					rep.PayloadHashMismatches++
					rep.Errors = append(rep.Errors, fmt.Sprintf("seq %d: payload hash mismatch", rec.Seq))
				}
			}
			switch rec.Kind {
			case types.KEvent, types.KCanary:
				rep.Counts.Events++
				rep.Counts.Redacted += uint64(rec.Redactions)
				digest, _, _ := groupKeyOf(rec)
				if digest != "" {
					ai := aggs[digest]
					if ai == nil {
						ai = &aggInfo{}
						aggs[digest] = ai
					}
					if ai.seqs == nil {
						ai.seqs = map[uint64]struct{}{}
					}
					ai.seqs[rec.Seq] = struct{}{}
				}
			case types.KGroup:
				// Only the compactor's own aggregate may declare folded event
				// counts; a dedup-core group record carries `aggregate: true`
				// for the live group (TRBL-116).
				if isCompactionMarker(rec) {
					digest, _, _ := groupKeyOf(rec)
					ai := aggs[digest]
					if ai == nil {
						ai = &aggInfo{}
						aggs[digest] = ai
					}
					spans, declared := aggSeqSpans(rec)
					consumed = append(consumed, spans...)
					ai.spans = append(ai.spans, spans...)
					ai.declared += declared
					rep.Counts.Events += uint64(numOf(rec.Payload["events_aggregated"]))
				}
			}
			return nil
		})
		f.Close()
		if werr != nil {
			if werr == context.Canceled || werr == context.DeadlineExceeded {
				return rep, werr
			}
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: read error: %v", p.File, werr))
			rep.OK = false
		}
	}
	flushDay()
	for _, ai := range aggs {
		counted := eventSeqSet(ai.seqs, ai.spans)
		if ai.declared != 0 && counted != 0 && ai.declared != counted {
			rep.TombstoneMismatches++
		}
	}
	rep.OK = rep.TornLines == 0 && rep.CorruptLines == 0 && len(rep.SeqHoles) == 0 &&
		rep.PayloadHashMismatches == 0 && rep.TombstoneMismatches == 0 && len(rep.Errors) == 0
	return rep, nil
}

// verifyPayloadHashLine recomputes one stored line's payload hash the way the
// write boundary computed it when it stamped payload_sha256 (SPEC-01 §3.1:
// one JSON object per line, struct field order, `hex(sha256)[:16]` over the
// canonical payload JSON).
//
// It hashes the payload BYTES AS STORED, minus the reserved payload_sha256
// member. That is the only form the two sides can agree on: the writer stamps
// the hash over the payload its emitter built — a `map[string]any` whose nested
// values may be typed Go structs, so the bytes carry struct FIELD order
// (`{"key":…,"value":…,"source":…,"source_ref":…,"redacted":…}` for a config
// snapshot's `values`). A verifier that decoded the line into `map[string]any`
// and re-marshaled it got Go's map ordering instead (alphabetical:
// `{"key":…,"redacted":…,"source":…}`) and so reported a deterministic mismatch
// for every struct-ordered record — the audit spine's config snapshots and
// lifecycle records (TRBL-106: 1000 re-marshals of one such stored payload
// produced zero byte-identical forms). Nested member values are kept verbatim
// here, so struct order survives; only the top-level map order is the encoder's,
// which is the order the stamped hash was computed over.
func verifyPayloadHashLine(line []byte) (string, error) {
	var probe struct {
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(line, &probe); err != nil {
		return "", err
	}
	body, err := payloadBytesWithoutHash(probe.Payload)
	if err != nil {
		return "", err
	}
	return types.DigestShort(types.SigDigest(body)), nil
}

// payloadBytesWithoutHash returns a stored payload object minus the reserved
// payload_sha256 envelope member, in the write boundary's canonical byte form:
// top-level members in the encoder's (sorted) key order, every member value
// byte-for-byte as stored — so a struct-ordered nested value stays
// struct-ordered. A nested member that happens to be named payload_sha256 is
// not the reserved envelope key and is preserved.
//
// It is the read-side counterpart of payloadHash (writer.go): for a line the
// writer produced, payloadBytesWithoutHash(stored payload) is byte-identical to
// canonicalPayloadJSON(payload) as hashed before the sha member was stamped in.
func payloadBytesWithoutHash(payload json.RawMessage) ([]byte, error) {
	if len(payload) == 0 {
		return []byte("{}"), nil
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(payload, &members); err != nil {
		return nil, err
	}
	delete(members, "payload_sha256")
	return marshalJSON(members)
}

func numOf(v any) float64 {
	f, _ := v.(float64)
	return f
}
