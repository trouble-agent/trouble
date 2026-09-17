package ledger

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// Resolve maps a seq to (file, byte offset) in O(log D + log S + 512 bytes):
// the day index, then the sparse samples (one per 512 lines), then a bounded
// forward scan (SPEC-01 §3.5 "Readers by offset").
func (l *Ledger) Resolve(seq uint64) (string, int64, error) {
	p, ok := l.idx.findPartForSeq(seq)
	if !ok {
		return "", 0, ledgerErr(types.CodeLedger002, ReasonValidation,
			fmt.Sprintf("seq %d is outside the indexed window", seq), nil)
	}
	if len(p.Samples) == 0 {
		if p.FirstSeq != 0 && seq < p.FirstSeq {
			return "", 0, ledgerErr(types.CodeLedger002, ReasonValidation,
				fmt.Sprintf("seq %d predates file %s", seq, p.File), nil)
		}
		return p.File, 0, nil
	}
	i := sort.Search(len(p.Samples), func(i int) bool { return p.Samples[i].Seq > seq })
	best := seqOffset{}
	if i == 0 {
		best = seqOffset{Seq: p.FirstSeq, Offset: 0}
	} else {
		best = p.Samples[i-1]
	}
	f, err := l.openPart(p.File)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	if _, err := f.Seek(best.Offset, 0); err != nil {
		return "", 0, ledgerErr(types.CodeLedger012, ReasonIO, "seek failed", err)
	}
	rd := newLineReader(f)
	off := best.Offset
	for {
		line, start, complete, rerr := rd.next()
		if rerr != nil {
			return "", 0, ledgerErr(types.CodeLedger012, ReasonIO, "read failed", rerr)
		}
		if !complete && len(line) == 0 {
			break
		}
		if !complete {
			break
		}
		rec, derr := decodeRecord(line, false)
		if derr == nil && rec != nil {
			if rec.Seq == seq {
				return p.File, start, nil
			}
			if rec.Seq > seq {
				return p.File, off, nil
			}
		}
		off = start
	}
	return p.File, off, nil
}

// ScanFrom is the only supported bulk walk. It stops when yield returns false
// and is bounded by cold_read_max_bytes / cold_read_max_lines (SPEC-01 §3.5).
func (l *Ledger) ScanFrom(seq uint64, yield func(types.Record) bool) error {
	_, _, _, err := l.scanFrom(seq, yield)
	return err
}

func (l *Ledger) scanFrom(seq uint64, yield func(types.Record) bool) (scannedBytes, scannedLines int64, partial bool, err error) {
	parts, _, derr := l.authoritativeFiles()
	if derr != nil {
		return 0, 0, false, derr
	}
	started := seq == 0
	for _, p := range parts {
		if !started && p.LastSeq != 0 && p.LastSeq < seq {
			continue
		}
		f, oerr := l.openPart(p.File)
		if oerr != nil {
			return scannedBytes, scannedLines, true, oerr
		}
		rd := newLineReader(f)
		for {
			line, _, complete, rerr := rd.next()
			if rerr != nil {
				f.Close()
				return scannedBytes, scannedLines, true,
					ledgerErr(types.CodeLedger012, ReasonIO, "read failed", rerr)
			}
			if !complete && len(line) == 0 {
				break
			}
			scannedBytes += int64(len(line)) + 1
			if scannedBytes > l.opts.Index.ColdReadMaxBytes || scannedLines > l.opts.Index.ColdReadMaxLines {
				f.Close()
				return scannedBytes, scannedLines, true, nil
			}
			if !complete {
				break
			}
			rec, cerr := decodeRecord(line, true)
			if cerr != nil || rec == nil {
				continue
			}
			scannedLines++
			if rec.Seq < seq {
				started = true
				continue
			}
			started = true
			if rec.SchemaVersion > l.opts.MaxSchema {
				continue
			}
			if !yield(*rec) {
				f.Close()
				return scannedBytes, scannedLines, false, nil
			}
		}
		f.Close()
	}
	return scannedBytes, scannedLines, false, nil
}

func (l *Ledger) openPart(name string) (*os.File, error) {
	f, err := os.Open(filepath.Join(l.root, name))
	if err != nil {
		return nil, ledgerErr(types.CodeLedger012, ReasonIO, fmt.Sprintf("cannot open %s", name), err)
	}
	return f, nil
}

// RecordsInSeqRange returns up to limit records with from <= seq <= to.
func (l *Ledger) RecordsInSeqRange(from, to uint64, limit int) ([]types.Record, error) {
	var out []types.Record
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = ctx
	err := l.ScanFrom(from, func(r types.Record) bool {
		if r.Seq > to {
			return false
		}
		out = append(out, r)
		return !(limit > 0 && len(out) >= limit)
	})
	return out, err
}
