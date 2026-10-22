package ledger

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"syscall"

	"github.com/trouble-agent/trouble/internal/types"
)

// readHeadFile reads the HEAD hint from a bare root path without locking.
func readHeadFile(root string) (headFile, bool) {
	b, err := os.ReadFile(filepath.Join(root, "HEAD"))
	if err != nil {
		return headFile{}, false
	}
	var h headFile
	if err := json.Unmarshal(bytes.TrimSpace(b), &h); err != nil {
		return headFile{}, false
	}
	return h, true
}

// readLockPID opens the LOCK file (no flock) and returns the PID it records,
// or -1 if the file does not exist or is empty.
func readLockPID(root string) int {
	path := filepath.Join(root, "LOCK")
	b, err := os.ReadFile(path)
	if err != nil || len(bytes.TrimSpace(b)) == 0 {
		return -1
	}
	var lf lockFile
	if err := json.Unmarshal(bytes.TrimSpace(b), &lf); err != nil {
		return -1
	}
	return lf.PID
}

// CheckWriterHeld returns the LOCK-file PID and true if a writer actually
// holds LOCK, or (-1, false) when there is no writer. It opens the file
// with ReadFile (no flock), then verifies the PID is a real process by
// attempting to kill it with signal 0. This prevents false positives from
// stale LOCK files left over from previous runs.
func CheckWriterHeld(root string) (int, bool) {
	path := filepath.Join(root, "LOCK")
	b, err := os.ReadFile(path)
	if err != nil || len(bytes.TrimSpace(b)) == 0 {
		return -1, false
	}
	var lf lockFile
	if err := json.Unmarshal(bytes.TrimSpace(b), &lf); err != nil {
		return -1, false
	}
	// Verify the PID is a living process. kill with signal 0 returns 0 if
	// the process exists (and we can signal it), non-zero otherwise.
	err = syscall.Kill(lf.PID, 0)
	if err != nil {
		// ESRCH = no such process (stale LOCK); EPERM = process exists but
		// we can't signal it (different user) — treat EPERM as "writer exists".
		if err == syscall.ESRCH {
			return lf.PID, false // stale lock
		}
		return lf.PID, true // process alive, writer holds LOCK
	}
	return lf.PID, true // process alive, writer holds LOCK
}

// authoritativeFilesRoot lists part files for a bare root path.
// It duplicates Ledger.authoritativeFiles() logic without touching the Ledger struct.
func authoritativeFilesRoot(root string) ([]*partInfo, []*partInfo, error) {
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil, nil, ledgerErr(types.CodeLedger012, ReasonIO,
			"cannot read ledger dir", err)
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

// diskBytes computes the total size of all files in a directory.
func diskBytes(root string) int64 {
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	var total int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if fi, err := e.Info(); err == nil {
			total += fi.Size()
		}
	}
	return total
}

// ReadStatus returns a LedgerStatus for the ledger at the given root without
// taking any locks or writing anything. It reads HEAD for writer metadata, the
// LOCK file (open, no flock) for the writer PID, and walks day files for record
// counts and last-sequence. This is the read-only path that SPEC-01 §2.4
// requires for `ledger status` — the CLI verb must never touch the writer seat.
func ReadStatus(root string) (types.LedgerStatus, error) {
	st := types.LedgerStatus{
		WriterPID:       -1, // signals "not read via writer"
		DiskBudgetBytes: DefaultRetentionPolicy().DiskBudgetBytes,
	}

	// Read HEAD for writer metadata and day info.
	head, ok := readHeadFile(root)
	if ok {
		st.Day = head.Day
		st.Gen = head.Gen
		st.Part = head.Part
		st.LastSeq = head.LastSeq
		st.Bytes = head.Bytes
		st.WriterVersion = head.Version
	}

	// Read LOCK file (open, NO flock) to get the actual writer PID.
	st.WriterPID = readLockPID(root)

	// Walk authoritative files to count records and find last seq/ts.
	files, _, err := authoritativeFilesRoot(root)
	if err != nil {
		return st, err
	}

	var lastSeq uint64
	var lastTS string
	var records int64
	var seqHoles int
	var prevSeq uint64

	for _, p := range files {
		path := filepath.Join(root, p.File)
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		err = walkLines(f, func(line []byte, _ int64, _ bool) error {
			if len(bytes.TrimSpace(line)) == 0 {
				return nil
			}
			rec, derr := decodeRecord(line, true)
			if derr != nil || rec == nil {
				return nil
			}
			records++
			if rec.Seq > lastSeq {
				lastSeq = rec.Seq
			}
			if rec.TS > lastTS {
				lastTS = rec.TS
			}
			if prevSeq > 0 && rec.Seq != prevSeq+1 {
				seqHoles++
			}
			prevSeq = rec.Seq
			return nil
		})
		_ = f.Close()
		if err != nil {
			return st, err
		}
	}

	if lastSeq > 0 {
		st.LastSeq = lastSeq
		st.LastTS = lastTS
		st.Records = records
	}

	st.DiskBytes = diskBytes(root)

	if seqHoles > 0 {
		st.Index.Degraded = true
		st.Index.DegradedReason = "seq_gaps_detected"
	}

	// Fill in defaults that the daemon would set on open (SPEC-01 §2.5).
	st.LossWindowMS = DefaultFsyncWindowMS
	st.DiskBudgetBytes = DefaultDiskBudgetBytes

	return st, nil
}

// ReadVerify returns a VerifyReport for the ledger at the given root without
// taking any locks or writing anything. It walks day files, checks seq
// monotonicity, validates torn/corrupt lines, and verifies payload hashes.
// This is the read-only path required for `ledger verify` per SPEC-01 §2.4.
func ReadVerify(root string, day string) (VerifyReport, error) {
	files, _, err := authoritativeFilesRoot(root)
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
	seen := map[uint64]struct{}{}
	var consumed []seqSpan

	for _, p := range files {
		if day != "" && p.Day != day {
			continue
		}
		rep.Files = append(rep.Files, p.File)

		f, ferr := os.Open(filepath.Join(root, p.File))
		if ferr != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: %v", p.File, ferr))
			rep.OK = false
			continue
		}

		err := walkLines(f, func(line []byte, _ int64, isTail bool) error {
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
			rep.Lines++
			// A zero seq is not a continuity break to special-case here: it is
			// absent from every accounted range, so verifyContinuity reports it
			// like any other unaccounted seq.
			seen[rec.Seq] = struct{}{}
			if h, ok := rec.Payload["payload_sha256"].(string); ok && h != "" {
				got, herr := verifyPayloadHashLine(line)
				if herr == nil && got != h {
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
		_ = f.Close()
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: read error: %v", p.File, err))
			rep.OK = false
		}
	}

	rep.SeqHoles = verifyContinuity(seen, consumed)

	// Check compaction aggregate counts. The denominator is the union of the
	// seqs the surviving in-range event lines carry and the seqs the aggregates
	// declare consumed in payload.agg_seqs (TRBL-117): counting only surviving
	// lines made every compacted group look short by the width of its folded
	// range.
	for _, ai := range aggs {
		counted := eventSeqSet(ai.seqs, ai.spans)
		if ai.declared != 0 && counted != 0 && ai.declared != counted {
			rep.TombstoneMismatches++
		}
	}

	rep.OK = rep.TornLines == 0 && rep.CorruptLines == 0 &&
		len(rep.SeqHoles) == 0 && rep.PayloadHashMismatches == 0 &&
		rep.TombstoneMismatches == 0 && len(rep.Errors) == 0

	return rep, nil
}
