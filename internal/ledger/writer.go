package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// pending is one queued record awaiting the group commit. A group carries
// EITHER one single record (`rec`, from Append and every ledger-authored
// path) OR a batch (`batch`, from AppendBatch, §3.5a): the writer treats both
// shapes as one queue entry, so a batch is one write and one fdatasync and is
// acked when the batch is durable.
type pending struct {
	rec   types.Record
	batch []types.Record
	done  chan error // nil for ledger-authored housekeeping records
	house bool       // written to the trail, nobody waits
}

// headFile is the HEAD hint (SPEC-01 §3.4 rule 7): never a source of truth.
type headFile struct {
	LastSeq uint64 `json:"last_seq"`
	Day     string `json:"day"`
	Gen     int    `json:"gen"`
	Part    int    `json:"part"`
	Bytes   int64  `json:"bytes"`
	TS      string `json:"ts"`
	Version string `json:"version"`
	GitSHA  string `json:"git_sha"`
}

// lockFile is the LOCK payload (SPEC-01 §3.4).
type lockFile struct {
	PID     int    `json:"pid"`
	Version string `json:"version"`
	GitSHA  string `json:"git_sha"`
	BootTS  string `json:"boot_ts"`
	Root    string `json:"root"`
}

// holeRange is a seq range consumed by a failed batch (SPEC-01 §3.5).
type holeRange struct{ from, to uint64 }

type rotateReply struct {
	part int
	err  error
}

// curFile is the file the writer is appending to.
type curFile struct {
	file    *os.File
	day     string
	gen     int
	part    int
	bytes   int64
	records int64
}

// writerState is the published snapshot (guarded by writer.mu).
type writerState struct {
	day     string
	gen     int
	part    int
	bytes   int64
	records int64
	lastTS  string
}

// writer is the single allocator and the only goroutine that touches ledger
// files (SPEC-01 §3.5).
type writer struct {
	l         *Ledger
	ch        chan *pending
	done      chan struct{}
	rotateReq chan chan rotateReply
	house     []*pending
	holes     []holeRange

	enqMu   sync.Mutex
	closing bool
	enqWG   sync.WaitGroup

	lastSeq      atomic.Uint64
	pendingSeq   atomic.Uint64
	fsyncCalls   atomic.Int64
	records      atomic.Int64
	backpressure atomic.Uint64
	depth        atomic.Int64

	errMu sync.Mutex
	werr  error

	mu   sync.Mutex
	st   writerState
	cur  *curFile
	last time.Time

	degradedPayload atomic.Bool
	budgetChecked   atomic.Int64

	headMu   sync.Mutex
	lastHead time.Time
}

func newWriter(l *Ledger) *writer {
	w := &writer{
		l:         l,
		ch:        make(chan *pending, l.opts.Rotation.QueueCapRecords),
		done:      make(chan struct{}),
		rotateReq: make(chan chan rotateReply, 4),
		last:      l.now(),
	}
	w.st.day = l.recovered.day
	w.st.gen = l.recovered.gen
	w.st.part = l.recovered.part
	w.st.bytes = l.recovered.bytes
	w.st.records = l.recovered.records
	w.st.lastTS = l.recovered.lastTS
	w.lastSeq.Store(l.recovered.lastSeq)
	w.pendingSeq.Store(l.recovered.lastSeq)
	return w
}

func (w *writer) start() {
	if w.cur == nil {
		if err := w.openCurrent(); err != nil {
			w.setErr(err)
		}
	}
	go w.run()
}

// openCurrent opens the recovered authoritative file for appending.
func (w *writer) openCurrent() error {
	st := w.st
	name := partName(st.day, st.gen, st.part)
	f, err := os.OpenFile(filepath.Join(w.l.root, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return ledgerErr(types.CodeLedger012, ReasonIO, fmt.Sprintf("cannot open %s", name), err)
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return ledgerErr(types.CodeLedger012, ReasonIO, fmt.Sprintf("cannot set mode 0600 on %s", name), err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return ledgerErr(types.CodeLedger012, ReasonIO, fmt.Sprintf("cannot stat %s", name), err)
	}
	w.cur = &curFile{file: f, day: st.day, gen: st.gen, part: st.part, bytes: fi.Size(), records: st.records}
	w.l.idx.addPart(partInfo{Day: st.day, Gen: st.gen, Part: st.part, File: name, Bytes: fi.Size(), Records: st.records})
	return nil
}

func (w *writer) run() {
	defer close(w.done)
	maxBatch := w.l.opts.Rotation.MaxBatchRecords
	window := time.Duration(w.l.opts.Rotation.FsyncWindowMS) * time.Millisecond
	timer := time.NewTimer(window)
	defer timer.Stop()
	batch := make([]*pending, 0, maxBatch)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		w.flushBatch(batch)
		batch = batch[:0]
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(window)
	}
	for {
		select {
		case p, ok := <-w.ch:
			if !ok {
				flush()
				w.closeCurrent()
				return
			}
			if len(batch) == 0 {
				// Rotation and the housekeeping notes it produces are decided
				// here, before the batch's first record, so a batch never spans
				// two files (SPEC-01 §6.1) and a post-midnight record cannot
				// land in the previous day's file.
				w.beforeBatch()
				if len(w.house) > 0 {
					batch = append(batch, w.house...)
					w.house = nil
				}
			}
			batch = append(batch, p)
			if len(batch) >= maxBatch {
				flush()
			}
		case <-timer.C:
			flush()
			timer.Reset(window)
		case replyCh := <-w.rotateReq:
			flush()
			w.beforeBatch()
			err := w.curErr()
			if len(w.house) > 0 {
				batch = append(batch, w.house...)
				w.house = nil
				flush()
			}
			part := 0
			if w.cur != nil {
				part = w.cur.part
			}
			replyCh <- rotateReply{part: part, err: err}
		}
	}
}

// beforeBatch runs between batches only (SPEC-01 §3.5 rule 4) and injects the
// housekeeping records that belong at the head of the next batch.
func (w *writer) beforeBatch() {
	now := w.l.now()
	w.checkDiskBudget(now)
	if w.cur == nil {
		if err := w.openCurrent(); err != nil {
			w.setErr(err)
		}
	}
	if w.cur != nil {
		day := now.UTC().Format(dayLayout)
		curDay := w.currentFile().day
		switch {
		case day > curDay:
			w.rotateTo(day, 0, 1, ReasonRotation)
		case now.Sub(w.last) >= 24*time.Hour:
			// Monotonic guard: the UTC date did not advance (clock jump) but a
			// day elapsed, so stay inside the same day and roll the part
			// instead of naming a file for an earlier day (SPEC-01 §6.2).
			w.rollPart(ReasonRotation)
		case w.l.opts.Rotation.MaxBytes > 0 && w.cur.bytes >= w.l.opts.Rotation.MaxBytes:
			w.rollPart(ReasonRotation)
		}
	}
	for _, h := range w.holes {
		w.house = append(w.house, w.housePending(ReasonHole, map[string]any{
			"op": "hole", "from": h.from, "to": h.to,
		}))
	}
	w.holes = nil
}

// checkDiskBudget drives the §3.7 escalation: once the ledger directory is over
// disk_budget_bytes, event/canary payloads are dropped (records are still
// written, so AC-6 holds) and one lifecycle{budget_exceeded} record is emitted.
func (w *writer) checkDiskBudget(now time.Time) {
	last := w.budgetChecked.Load()
	if last != 0 && now.Unix()-last < 5 {
		return
	}
	w.budgetChecked.Store(now.Unix())
	budget := w.l.opts.Retention.DiskBudgetBytes
	if budget <= 0 {
		return
	}
	used := w.l.diskBytes()
	if used <= budget {
		w.degradedPayload.Store(false)
		return
	}
	if !w.degradedPayload.Swap(true) {
		w.house = append(w.house, w.housePending(ReasonBudgetExceed, map[string]any{
			"op":                "budget_exceeded",
			"disk_bytes":        used,
			"disk_budget_bytes": budget,
			"error_code":        string(types.CodeLedger010),
			"escalation":        "payload_dropped",
		}))
	}
}

// payloadDegraded reports whether new event/canary payloads are being dropped.
func (w *writer) payloadDegraded() bool { return w.degradedPayload.Load() }

// fileID identifies the file the writer is appending to.
type fileID struct {
	day  string
	gen  int
	part int
}

// currentFile is read by compaction to refuse touching the live file.
func (w *writer) currentFile() fileID {
	w.mu.Lock()
	defer w.mu.Unlock()
	return fileID{day: w.st.day, gen: w.st.gen, part: w.st.part}
}

// compactionStart / compactionDone are the ledger's own housekeeping records
// for a generation rewrite (§4.1).
func (w *writer) compactionStart(day string, fromGen, toGen int) error {
	return w.selfAppend(types.KLifecycle, map[string]any{
		"op": "compaction_start", "day": day, "from_gen": fromGen, "to_gen": toGen,
	})
}

func (w *writer) compactionDone(day string, toGen int, res types.CompactionResult) error {
	return w.selfAppend(types.KLifecycle, map[string]any{
		"op": "compaction_done", "day": day, "to_gen": toGen,
		"lines_in": res.LinesIn, "lines_out": res.LinesOut,
		"aggregates": res.Aggregates, "records_kept": res.RecordsKept,
		"payloads_expired": res.PayloadsExpired, "payloads_dropped": res.PayloadsDropped,
		"bytes_in": res.BytesIn, "bytes_out": res.BytesOut,
		"tombstone_counts_kept": res.TombstoneCountsKept,
	})
}

// rotateTo switches to a new day file (generation 0, part 1).
func (w *writer) rotateTo(day string, gen, part int, op string) {
	from := ""
	if w.cur != nil {
		from = partName(w.cur.day, w.cur.gen, w.cur.part)
		w.closeCurrent()
	}
	w.mu.Lock()
	w.st.day, w.st.gen, w.st.part = day, gen, part
	w.st.bytes, w.st.records = 0, 0
	w.mu.Unlock()
	if err := w.openCurrent(); err != nil {
		w.rotateFailed(from, partName(day, gen, part), err)
		return
	}
	w.last = w.l.now()
	w.forceHeadWrite()
	w.house = append(w.house, w.housePending(op, map[string]any{
		"op": "rotate", "from": from, "to": partName(day, gen, part), "day": day, "gen": gen, "part": part,
	}))
}

// rotateFailed restores the previous file and records a transient rotation
// failure (SPEC-01 §5 code 006). No record is lost: the writer keeps appending
// to the file it already had open.
func (w *writer) rotateFailed(from, to string, err error) {
	w.setErr(err)
	if from != "" {
		if day, gen, part, ok := parsePartName(from); ok {
			w.mu.Lock()
			w.st.day, w.st.gen, w.st.part = day, gen, part
			w.mu.Unlock()
			if rerr := w.openCurrent(); rerr != nil {
				w.setErr(rerr)
			}
		}
	}
	w.house = append(w.house, w.housePending(ReasonRotation, map[string]any{
		"op": "rotate_failed", "from": from, "to": to,
		"error_code": string(types.CodeLedger006), "detail": err.Error(),
	}))
}

// parsePartName reverses partName.
func parsePartName(name string) (day string, gen, part int, ok bool) {
	m := fileRe.FindStringSubmatch(name)
	if m == nil {
		return "", 0, 0, false
	}
	gen, part = 0, 1
	if m[2] != "" {
		part, _ = strconv.Atoi(m[2])
	}
	if m[3] != "" {
		gen, _ = strconv.Atoi(m[3])
	}
	return m[1], gen, part, true
}

// rollPart switches to the next part of the same day.
func (w *writer) rollPart(op string) {
	if w.cur == nil {
		return
	}
	day, gen := w.cur.day, w.cur.gen
	next := w.cur.part + 1
	from := partName(day, gen, w.cur.part)
	w.closeCurrent()
	w.mu.Lock()
	w.st.day, w.st.gen, w.st.part = day, gen, next
	w.st.bytes, w.st.records = 0, 0
	w.mu.Unlock()
	if err := w.openCurrent(); err != nil {
		// 006: keep appending to the current file; the failed rotation is
		// recorded and retried at the next batch boundary.
		w.rotateFailed(from, partName(day, gen, next), err)
		return
	}
	w.last = w.l.now()
	w.forceHeadWrite()
	w.house = append(w.house, w.housePending(op, map[string]any{
		"op": "rotate", "from": from, "to": partName(day, gen, next), "day": day, "gen": gen, "part": next,
	}))
}

func (w *writer) closeCurrent() {
	if w.cur == nil {
		return
	}
	_ = fdatasync(w.cur.file, w.l.opts.Rotation.Fdatasync)
	_ = w.cur.file.Close()
	w.mu.Lock()
	w.st.bytes = w.cur.bytes
	w.st.records = w.cur.records
	w.mu.Unlock()
	_ = syncDir(w.l.root)
	w.cur = nil
}

// housePending builds a ledger-authored housekeeping record (§4.1).
func (w *writer) housePending(op string, payload map[string]any) *pending {
	if payload == nil {
		payload = map[string]any{}
	}
	if _, ok := payload["op"]; !ok {
		payload["op"] = op
	}
	rec := types.Record{
		RecID:         types.NewID(types.PEv),
		TS:            types.FormatUTC(w.l.now()),
		Kind:          types.KLifecycle,
		SchemaVersion: SchemaVersionV1,
		Origin:        w.l.origin("ledger"),
		Actor:         w.l.opts.Writer,
		Payload:       payload,
	}
	return &pending{rec: rec, house: true}
}

// selfAppend writes a ledger-authored record from a non-writer goroutine.
func (w *writer) selfAppend(kind types.RecordKind, payload map[string]any) error {
	_, err := w.enqueue(context.Background(), types.Record{
		RecID:         types.NewID(types.PEv),
		TS:            types.FormatUTC(w.l.now()),
		Kind:          kind,
		SchemaVersion: SchemaVersionV1,
		Origin:        w.l.origin("ledger"),
		Actor:         w.l.opts.Writer,
		Payload:       payload,
	})
	return err
}

// enqueue submits a prepared record and waits for its durable ack.
func (w *writer) enqueue(ctx context.Context, rec types.Record) (types.Record, error) {
	w.enqMu.Lock()
	if w.closing {
		w.enqMu.Unlock()
		return types.Record{}, ledgerErr(types.CodeLedger001, ReasonWrite, "ledger is closing", nil)
	}
	w.enqWG.Add(1)
	w.enqMu.Unlock()
	defer w.enqWG.Done()

	p := &pending{rec: rec, done: make(chan error, 1)}
	w.depth.Add(1)
	select {
	case w.ch <- p:
		w.depth.Add(-1)
	default:
		w.depth.Add(-1)
		wait := w.l.opts.Rotation.MaxEnqueueWait
		d, err := time.ParseDuration(wait)
		if err != nil {
			d = 5 * time.Second
		}
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case w.ch <- p:
		case <-t.C:
			w.backpressure.Add(1)
			return types.Record{}, ledgerErr(types.CodeLedger001, ReasonBackpressure,
				fmt.Sprintf("queue cap %d reached and max_enqueue_wait %s expired",
					w.l.opts.Rotation.QueueCapRecords, wait), nil)
		case <-ctx.Done():
			return types.Record{}, ctx.Err()
		}
	}
	select {
	case err := <-p.done:
		if err != nil {
			return types.Record{}, err
		}
		return p.rec, nil
	case <-ctx.Done():
		// The record may still land; report the caller's cancellation.
		return p.rec, ctx.Err()
	}
}

// enqueueBatch submits a batch of prepared records as ONE queue entry and
// waits for the batch's durable ack (§3.5a: durable-on-return per BATCH).
// Either every record is durable (nil error) or the batch failed (the error,
// plus a hole for the seq range the batch would have occupied); a partial
// landing is impossible because the records share one write and one fdatasync.
func (w *writer) enqueueBatch(ctx context.Context, recs []types.Record) ([]types.Record, error) {
	w.enqMu.Lock()
	if w.closing {
		w.enqMu.Unlock()
		return recs, ledgerErr(types.CodeLedger001, ReasonWrite, "ledger is closing", nil)
	}
	w.enqMu.Unlock()
	// §3.5a clamp: the writer's own batch cap is a flush boundary, so a batch
	// larger than the remaining space in ledger.max_batch_records would span
	// two commits — exactly what the contract forbids. The reconcile clamps
	// first (AppendBatchLedger), and this is the backstop for a direct caller:
	// the remainder is re-enqueued as its own group (one extra window, never a
	// second boundary inside one).
	if cap := w.l.opts.Rotation.MaxBatchRecords; cap > 0 && len(recs) > cap {
		first, err := w.enqueueBatch(ctx, recs[:cap])
		if err != nil {
			return first, err
		}
		rest, err := w.enqueueBatch(ctx, recs[cap:])
		return append(first, rest...), err
	}
	p := &pending{batch: recs, done: make(chan error, 1)}
	w.depth.Add(1)
	select {
	case w.ch <- p:
		w.depth.Add(-1)
	default:
		w.depth.Add(-1)
		wait := w.l.opts.Rotation.MaxEnqueueWait
		d, err := time.ParseDuration(wait)
		if err != nil {
			d = 5 * time.Second
		}
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case w.ch <- p:
		case <-t.C:
			w.backpressure.Add(1)
			return recs, ledgerErr(types.CodeLedger001, ReasonBackpressure,
				fmt.Sprintf("queue cap %d reached and max_enqueue_wait %s expired",
					w.l.opts.Rotation.QueueCapRecords, wait), nil)
		case <-ctx.Done():
			return recs, ctx.Err()
		}
	}
	select {
	case err := <-p.done:
		if err != nil {
			return recs, err
		}
		return recs, nil
	case <-ctx.Done():
		// The batch may still land; report the caller's cancellation.
		return recs, ctx.Err()
	}
}

// stop drains the queue, flushes and closes the current file.
func (w *writer) stop() error {
	w.enqMu.Lock()
	if w.closing {
		w.enqMu.Unlock()
		return w.err()
	}
	w.closing = true
	w.enqMu.Unlock()
	w.enqWG.Wait()
	close(w.ch)
	<-w.done
	return w.err()
}

func (w *writer) err() error {
	w.errMu.Lock()
	defer w.errMu.Unlock()
	return w.werr
}

func (w *writer) curErr() error { return w.err() }

func (w *writer) setErr(err error) {
	if err == nil {
		return
	}
	w.errMu.Lock()
	if w.werr == nil {
		w.werr = err
	}
	w.errMu.Unlock()
}

func (w *writer) snapshot() types.LedgerStatus {
	w.mu.Lock()
	st := w.st
	w.mu.Unlock()
	lastSeq := w.lastSeq.Load()
	return types.LedgerStatus{
		LastSeq:        lastSeq,
		LastSeqPending: w.pendingSeq.Load(),
		LastTS:         st.lastTS,
		Day:            st.day,
		Gen:            st.gen,
		Part:           st.part,
		File:           partName(st.day, st.gen, st.part),
		Bytes:          st.bytes,
		Records:        st.records,
		FsyncCalls:     w.fsyncCalls.Load(),
		WriterPID:      os.Getpid(),
	}
}

func (w *writer) queueDepth() int { return int(w.depth.Load()) }

func (w *writer) requestRotate(now time.Time) (int, error) {
	replyCh := make(chan rotateReply, 1)
	select {
	case w.rotateReq <- replyCh:
	case <-w.done:
		return 0, ledgerErr(types.CodeLedger001, ReasonWrite, "ledger writer stopped", nil)
	}
	r := <-replyCh
	return r.part, r.err
}

// flushBatch is the group commit: allocate seqs, serialize, write once,
// fdatasync once, then publish LastSeq and ack every record (SPEC-01 §3.5).
// The queue holds groups; an AppendBatch group contributes all of its records
// to one write and one fdatasync, which is what makes the batch path's
// durable-on-return-per-batch contract a property of the same code the single
// path has always used (§3.5a).
func (w *writer) flushBatch(batch []*pending) {
	if len(batch) == 0 {
		return
	}
	if w.l.opts.PerLineFsync {
		for _, p := range batch {
			if len(p.batch) == 0 {
				w.writeOne(p)
				continue
			}
			// Per-line mode is the test-only §7 diagnostic (one fdatasync per
			// record), so an AppendBatch group degrades to exactly that: every
			// record pays its own sync, and the group is acked ONCE — on the
			// first new failure, or after the last record lands. (done is
			// buffered; a per-record ack would deadlock the writer on the
			// second send.)
			errBefore := w.err()
			for i := range p.batch {
				w.writeOne(&pending{rec: p.batch[i]})
			}
			if err := w.err(); err != nil && err != errBefore {
				p.done <- err
			} else {
				p.done <- nil
			}
		}
		return
	}
	if w.cur == nil {
		for _, p := range batch {
			if p.done != nil {
				p.done <- ledgerErr(types.CodeLedger012, ReasonIO, "ledger file unavailable", nil)
			}
		}
		return
	}
	from := w.pendingSeq.Load() + 1
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	recs := make([]types.Record, 0, len(batch))
	for _, p := range batch {
		for i := range p.batch {
			rec := &p.batch[i]
			rec.Seq = w.pendingSeq.Add(1)
			if err := enc.Encode(rec); err != nil {
				to := rec.Seq
				w.failBatch(batch, ledgerErr(types.CodeLedger001, ReasonWrite,
					fmt.Sprintf("cannot serialize record seq %d", rec.Seq), err), from, to)
				return
			}
			recs = append(recs, *rec)
		}
		if len(p.batch) == 0 {
			p.rec.Seq = w.pendingSeq.Add(1)
			if err := enc.Encode(&p.rec); err != nil {
				to := p.rec.Seq
				w.failBatch(batch, ledgerErr(types.CodeLedger001, ReasonWrite,
					fmt.Sprintf("cannot serialize record seq %d", p.rec.Seq), err), from, to)
				return
			}
			recs = append(recs, p.rec)
		}
	}
	n, err := w.cur.file.Write(buf.Bytes())
	if err == nil && n < buf.Len() {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.failBatch(batch, ledgerErr(types.CodeLedger001, ReasonWrite, "write failed", err), from, w.pendingSeq.Load())
		return
	}
	if err := fdatasync(w.cur.file, w.l.opts.Rotation.Fdatasync); err != nil {
		w.failBatch(batch, ledgerErr(types.CodeLedger001, ReasonFsync, "fdatasync failed", err), from, w.pendingSeq.Load())
		return
	}
	w.fsyncCalls.Add(1)
	w.cur.bytes += int64(buf.Len())
	w.cur.records += int64(len(recs))
	last := recs[len(recs)-1]
	w.lastSeq.Store(last.Seq)
	w.mu.Lock()
	w.st.bytes = w.cur.bytes
	w.st.records = w.cur.records
	w.st.lastTS = last.TS
	w.mu.Unlock()
	w.l.idx.applyBatch(recs)
	w.l.idx.fileAppended(w.cur.day, w.cur.gen, w.cur.part, int64(buf.Len()), int64(len(recs)), last.Seq)
	w.maybeWriteHead(w.l.now())
	for _, p := range batch {
		if p.done != nil {
			p.done <- nil
		}
	}
}

// maybeWriteHead refreshes the HEAD hint. HEAD is a hint, never a source of
// truth (§3.4 rule 7), so it must not sit on the latency path of every batch:
// while there is a backlog the refresh is throttled to once per second, and when
// the writer has drained its queue (no backlog) the hint is refreshed on every
// flush, which is what keeps HEAD useful for a quiet ledger where each record is
// durable on its own.
func (w *writer) maybeWriteHead(now time.Time) {
	backlog := len(w.ch) > 0
	w.headMu.Lock()
	stale := w.lastHead.IsZero() || time.Since(w.lastHead) >= time.Second
	if !stale && backlog {
		w.headMu.Unlock()
		return
	}
	w.lastHead = time.Now()
	w.headMu.Unlock()
	if herr := w.l.writeHead(); herr != nil {
		w.l.headFailures.Add(1)
	}
}

// forceHeadWrite marks the hint stale so the next flush refreshes it.
func (w *writer) forceHeadWrite() {
	w.headMu.Lock()
	w.lastHead = time.Time{}
	w.headMu.Unlock()
}

// writeOne is the test-only per-line mode (SPEC-01 §7 TestPerLineRegression).
func (w *writer) writeOne(p *pending) {
	if w.cur == nil {
		if p.done != nil {
			p.done <- ledgerErr(types.CodeLedger012, ReasonIO, "ledger file unavailable", nil)
		}
		return
	}
	p.rec.Seq = w.pendingSeq.Add(1)
	line, err := marshalRecordLine(&p.rec)
	if err != nil {
		w.failBatch([]*pending{p}, ledgerErr(types.CodeLedger001, ReasonWrite, "cannot serialize record", err), p.rec.Seq, p.rec.Seq)
		return
	}
	n, werr := w.cur.file.Write(line)
	if werr == nil && n < len(line) {
		werr = io.ErrShortWrite
	}
	if werr == nil {
		werr = fdatasync(w.cur.file, w.l.opts.Rotation.Fdatasync)
	}
	if werr != nil {
		w.failBatch([]*pending{p}, ledgerErr(types.CodeLedger001, ReasonWrite, "write failed", werr), p.rec.Seq, p.rec.Seq)
		return
	}
	w.fsyncCalls.Add(1)
	w.cur.bytes += int64(len(line))
	w.cur.records++
	w.lastSeq.Store(p.rec.Seq)
	w.mu.Lock()
	w.st.bytes = w.cur.bytes
	w.st.records = w.cur.records
	w.st.lastTS = p.rec.TS
	w.mu.Unlock()
	w.l.idx.applyBatch([]types.Record{p.rec})
	w.l.idx.fileAppended(w.cur.day, w.cur.gen, w.cur.part, int64(len(line)), 1, p.rec.Seq)
	w.maybeWriteHead(w.l.now())
	if p.done != nil {
		p.done <- nil
	}
}

// failBatch records the seq range as a hole, reports it and acks every record
// with the failure (SPEC-01 §3.5, §5 code 001/002).
func (w *writer) failBatch(batch []*pending, err error, from, to uint64) {
	w.setErr(err)
	if to >= from && from > 0 {
		w.holes = append(w.holes, holeRange{from: from, to: to})
	}
	for _, p := range batch {
		if p.done != nil {
			p.done <- err
		}
	}
}

// ---- serialization helpers (SPEC-01 §3.1 "Serialization rules") ----

func marshalRecordLine(rec *types.Record) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(rec); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func marshalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	b := buf.Bytes()
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return b, nil
}

// canonicalPayloadJSON marshals a payload as the write boundary reads it.
func canonicalPayloadJSON(payload map[string]any) ([]byte, error) {
	if payload == nil {
		return []byte("{}"), nil
	}
	return marshalJSON(payload)
}

// payloadBytes is the canonical payload size used by the caps and by the
// payload_sha256 rule.
func payloadBytes(payload map[string]any) int64 {
	b, err := canonicalPayloadJSON(payload)
	if err != nil {
		return 0
	}
	return int64(len(b))
}

// payloadHash is hex(sha256(canonical payload JSON))[:16].
func payloadHash(payload map[string]any) string {
	b, err := canonicalPayloadJSON(payload)
	if err != nil {
		return ""
	}
	return types.DigestShort(types.SigDigest(b))
}

// truncatePayload keeps the reserved envelope keys and flags the truncation.
func truncatePayload(payload map[string]any, size, max int64) map[string]any {
	out := map[string]any{
		"truncated":       true,
		"truncated_bytes": size,
	}
	for _, k := range []string{"op", "error_code", "merge_key", "subject", "zone", "local_id", "alias_sig"} {
		if v, ok := payload[k]; ok {
			out[k] = v
		}
	}
	_ = max
	return out
}
