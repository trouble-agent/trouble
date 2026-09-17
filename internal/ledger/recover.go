package ledger

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"syscall"

	"github.com/totalwindupflightsystems/trouble/internal/types"
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
// the torn tail of a crash (SPEC-01 §5 code 003) rather than interior
// corruption (011) — that distinction is why the lookahead exists.
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
		if have {
			if err := fn(pending, pendingStart, false); err != nil {
				return err
			}
		}
		pending, pendingStart, have = line, start, true
	}
}

// acquireLock takes flock(LOCK_EX|LOCK_NB) on ledger/LOCK (SPEC-01 §3.4 rule 6).
func acquireLock(root string, actor types.Actor) (*os.File, error) {
	path := filepath.Join(root, "LOCK")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, ledgerErr(types.CodeLedger012, ReasonIO,
			fmt.Sprintf("cannot open %s", path), err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
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
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
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
	}
	if len(parts) > 0 {
		highest = parts[len(parts)-1]
	}
	// torn-line recovery: exactly one line is dropped and the file gets a \n
	// before the next record (SPEC-01 §6.4).
	torn := 0
	for _, p := range parts {
		if p.Torn == 0 {
			continue
		}
		torn += int(p.Torn)
		if err := healTorn(filepath.Join(l.root, p.File)); err != nil {
			l.idx.markDegraded(ReasonIO)
			continue
		}
		l.bootNotes = append(l.bootNotes, map[string]any{
			"op":         ReasonRecoverTorn,
			"file":       p.File,
			"error_code": string(types.CodeLedger003),
			"lines_lost": 1,
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
	type aggInfo struct{ declared, counted uint64 }
	aggs := map[string]*aggInfo{}
	var prev uint64
	for _, p := range parts {
		if day != "" && p.Day != day {
			continue
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
			if rec.Seq == 0 || (prev != 0 && rec.Seq != prev+1) {
				from := prev + 1
				if rec.Seq == 0 {
					from = prev
				}
				rep.SeqHoles = append(rep.SeqHoles, SeqHole{From: from, To: rec.Seq - 1})
			}
			prev = rec.Seq
			if h, ok := rec.Payload["payload_sha256"].(string); ok && h != "" {
				cp := map[string]any{}
				for k, v := range rec.Payload {
					if k == "payload_sha256" {
						continue
					}
					cp[k] = v
				}
				if payloadHash(cp) != h {
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
					ai.counted++
				}
			case types.KGroup:
				if isCompactionAggregate(rec) {
					digest, _, _ := groupKeyOf(rec)
					ai := aggs[digest]
					if ai == nil {
						ai = &aggInfo{}
						aggs[digest] = ai
					}
					if c, ok := rec.Payload["counters"].(map[string]any); ok {
						ai.declared += uint64(numOf(c["events"]))
					}
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
	for _, ai := range aggs {
		if ai.declared != 0 && ai.counted != 0 && ai.declared != ai.counted {
			rep.TombstoneMismatches++
		}
	}
	rep.OK = rep.TornLines == 0 && rep.CorruptLines == 0 && len(rep.SeqHoles) == 0 &&
		rep.PayloadHashMismatches == 0 && rep.TombstoneMismatches == 0 && len(rep.Errors) == 0
	return rep, nil
}

func numOf(v any) float64 {
	f, _ := v.(float64)
	return f
}
