package ledger

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// RemoveLockIfOwned removes a LOCK file left by this process after it has
// released its flock. Call only after closing the Ledger; a different PID is
// never removed (in particular, a daemon's writer lock is left untouched).
func RemoveLockIfOwned(root string, pid int) error {
	path := filepath.Join(root, "LOCK")
	if readLockPID(root) != pid {
		return nil
	}
	return os.Remove(path)
}

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

// ReadHeadActiveDay reads the active day from the HEAD hint.
// Returns the day string and true if HEAD exists, or "" and false otherwise.
func ReadHeadActiveDay(root string) (string, bool) {
	h, ok := readHeadFile(root)
	if !ok {
		return "", false
	}
	return h.Day, true
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

// CheckWriterHeld reports the kernel's LOCK holder rather than trusting the
// advisory JSON payload. On Linux the holder PID normally comes from /proc/locks;
// a fallback lock probe keeps stale payloads distinct from an unlocked ledger.
func CheckWriterHeld(root string) (int, bool) {
	return checkLockOwner(root)
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
//
// Because it is not the writer, the writer-seat counters are NOT measurable
// here: fsync_calls, fsync_per_record, queue_depth, last_seq_pending and
// backpressure_total are left absent and named in Unmeasured with
// "in_process_counter" (SPEC-01 §3.10b, TRBL-130). stall_s, the refusal folds
// and every other member are derived from the files, so this path reports them
// exactly as the writer seat does.
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

	// Read LOCK (open, NO blocking lock). The kernel flock is authoritative;
	// WriterPID is the live payload identity when valid, and writer_lock_held
	// distinguishes an active lock whose payload is stale or unreadable.
	if pid, held := CheckWriterHeld(root); held {
		st.WriterPID = pid
		st.WriterLockHeld = true
	}

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
	var playRuns, agentRuns int64
	// SPEC-01 §3.5b: the recorded backpressure surface, derived from the day's
	// partitions exactly like the §3.10a rung counters — a read-only status must
	// see a fold the writer persisted, and must agree with the live writer.
	var gapRecords, estLost int64

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
			// SPEC-01 §3.10a: the ladder's rung counters are derived in this
			// same pass. A ladder record that entered a rung names it in
			// payload.rung_entry (SPEC-05 §3.2b), so the counters count rung
			// entries the ledger actually holds — not records that merely
			// mention a counter field.
			switch rungEntry(rec.Payload) {
			case "play":
				playRuns++
			case "agent":
				agentRuns++
			}
			if n, ok := backpressureFold(rec); ok {
				gapRecords++
				estLost += n
			}
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

	// SPEC-01 §3.10a: the rung counters are a property of the day files, not of
	// the HEAD hint, so they are published even on a ledger whose HEAD is behind.
	st.PlayRuns = playRuns
	st.AgentRuns = agentRuns

	// SPEC-01 §3.5b: likewise the recorded backpressure surface. Both status
	// paths answer the same numbers for the same ledger.
	st.GapRecords = gapRecords
	st.EstLostTotal = estLost

	st.DiskBytes = diskBytes(root)

	// SPEC-01 §3.10b (TRBL-130): the writer-seat counters — the fdatasync
	// counter and its rate, the enqueue queue, the allocated-not-yet-durable
	// watermark, this process's refusal tally — belong to a process this walk is
	// not. They are left ABSENT and the reason is published, so no reader sees a
	// 0 that means "this path could not look"; the ledger-side answers
	// (gap_records / est_lost_total) are derived below like any other fold.
	for _, f := range []string{
		"fsync_calls", "fsync_per_record", "queue_depth", "last_seq_pending", "backpressure_total",
	} {
		st.MarkUnmeasured(f, types.UnmeasuredInProcess)
	}

	if seqHoles > 0 {
		st.Index.Degraded = true
		st.Index.DegradedReason = "seq_gaps_detected"
	}

	// The stall IS measurable here — it is last_ts against the current clock, the
	// same arithmetic the writer seat does — so it is measured rather than
	// dropped, and absent only when the ledger holds no record at all.
	if lastTS != "" {
		if t, terr := types.ParseUTC(lastTS); terr == nil {
			stall := time.Since(t).Seconds()
			if stall < 0 {
				stall = 0
			}
			st.StallS = &stall
		}
	}
	if st.StallS == nil {
		st.MarkUnmeasured("stall_s", types.UnmeasuredNoRecords)
	}

	// Fill in defaults that the daemon would set on open (SPEC-01 §2.5). These
	// are policy values, not measurements: they are the same numbers the daemon
	// runs with, which is why both status paths can report them.
	st.LossWindowMS = DefaultFsyncWindowMS
	st.QueueCap = DefaultQueueCapRecords
	st.DiskBudgetBytes = DefaultDiskBudgetBytes

	return st, nil
}

// rungEntry reads the ladder's rung attribution from a record payload
// (SPEC-05 §3.2b): "" when the record is not a rung entry. The value is written
// by the ladder as a plain string, and a decoded ledger payload keeps it as one.
func rungEntry(payload map[string]any) string {
	if payload == nil {
		return ""
	}
	s, _ := payload["rung_entry"].(string)
	return s
}

// backpressureFold reports whether rec is a boundary-refusal fold (SPEC-01
// §3.5b) and, if so, how many refusals it carries. A fold is identified by the
// gap it declares — cause=backpressure over scope=ledger:queue — so a sensor
// dropout that happens to mention backpressure is not counted, and a fold whose
// est_lost is missing or negative contributes 0 rather than a lie.
func backpressureFold(rec *types.Record) (int64, bool) {
	if rec == nil || rec.Kind != types.KGap || rec.Payload == nil {
		return 0, false
	}
	cause, _ := rec.Payload["cause"].(string)
	scope, _ := rec.Payload["scope"].(string)
	if cause != ReasonBackpressure || scope != "ledger:queue" {
		return 0, false
	}
	n := int64(asNum(rec.Payload["est_lost"]))
	if n < 0 {
		n = 0
	}
	return n, true
}

// ReadOpenIncidents returns the ledger's open incidents (newest-updated first)
// for the given root without taking the writer seat: it walks the authoritative
// day files and projects the incident records through the SAME apply path the
// daemon's boot rebuild uses, so a verb that reads a live ledger reports what the
// daemon would report. It is the read side of `trouble topology`'s incident tail
// (SPEC-12 §3.7, TRBL-126) and it writes nothing, locks nothing.
func ReadOpenIncidents(root string, limit int) ([]types.Incident, error) {
	files, _, err := authoritativeFilesRoot(root)
	if err != nil {
		return nil, err
	}
	ix := newIndex(DefaultIndexOptions(), root, "loopback", SchemaVersionV1)
	for _, p := range files {
		f, oerr := os.Open(filepath.Join(root, p.File))
		if oerr != nil {
			continue
		}
		werr := walkLines(f, func(line []byte, _ int64, _ bool) error {
			if len(bytes.TrimSpace(line)) == 0 {
				return nil
			}
			rec, derr := decodeRecord(line, true)
			if derr != nil || rec == nil || rec.Kind != types.KIncident {
				return nil
			}
			ix.applyLocked(rec)
			return nil
		})
		_ = f.Close()
		if werr != nil {
			return nil, werr
		}
	}
	d := &dashReader{l: &Ledger{idx: ix}}
	return d.OpenIncidents(limit), nil
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
	// Seq continuity is a PER-DAY property (see Ledger.Verify): each day file
	// starts from its own seq base, so a multi-day walk neither invents holes at
	// day boundaries nor lets one day's seqs mask another day's real hole.
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

	for _, p := range files {
		if day != "" && p.Day != day {
			continue
		}
		if p.Day != curDay {
			flushDay()
			curDay = p.Day
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

	flushDay()

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

	rep.normalize()
	return rep, nil
}
