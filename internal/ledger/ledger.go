// Package ledger is the single durable store of trouble: one append-only JSONL
// ledger per host, written by one process, indexed in memory, queried through
// one read API (SPEC-01).
package ledger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/scrub"
	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// SchemaVersionV1 is the record schema version written by this binary.
const SchemaVersionV1 = 1

// Defaults pinned by SPEC-01 §2.5.
const (
	DefaultFsyncWindowMS      = 200
	DefaultMaxBatchRecords    = 4096
	DefaultQueueCapRecords    = 65536
	DefaultMaxEnqueueWait     = "5s"
	DefaultMaxRecordBytes     = 262144
	DefaultRotateMaxBytes     = 536870912
	DefaultIndexHotDays       = 7
	DefaultIndexBuildBudgetMS = 9000
	DefaultIndexBuildBytes    = 536870912
	DefaultIndexMaxGroups     = 50000
	DefaultIndexMaxIncidents  = 50000
	DefaultIndexMaxSources    = 5000
	DefaultIndexIncPerInc     = 512
	DefaultIndexIncidentRing  = 4096
	DefaultIndexMaxBytes      = 67108864
	DefaultColdReadMaxBytes   = 67108864
	DefaultColdReadMaxLines   = 200000
	DefaultDiskBudgetBytes    = 2147483648
	DefaultDiskWarnPct        = 80
	DefaultPartSuffix         = ".p%02d"
)

// Reason values carried in payload.reason for TROUBLE-LEDGER-001 (SPEC-01 §5).
const (
	ReasonValidation    = "validation"
	ReasonBackpressure  = "backpressure"
	ReasonWrite         = "write"
	ReasonFsync         = "fsync"
	ReasonHead          = "head"
	ReasonCompaction    = "compaction"
	ReasonRetention     = "retention"
	ReasonDiskBudget    = "disk_budget"
	ReasonIO            = "io_error"
	ReasonBudgetExceed  = "budget_exceeded"
	ReasonVersionGate   = "version_gate"
	ReasonScrubRefusal  = "scrub_refusal"
	ReasonRotation      = "rotate"
	ReasonRecoverTorn   = "recover_torn"
	ReasonHole          = "hole"
	ReasonCompactionRes = "compaction_resume"
	ReasonIndexDegraded = "index_degraded"
)

// LossWindowStatement is the §2.1 crash-loss window statement. It is normative,
// published verbatim in README.md, docs/operations.md, the `trouble --help`
// ledger section (HelpLedgerSection) and GET /health.json (HealthLedgerFields);
// docs_test.go fails CI if any copy drifts.
const LossWindowStatement = "A record is durable the moment `Append` returns. Records already written to the ledger file but not yet fsynced are lost on **power loss or kernel panic**; the maximum loss window is `ledger.fsync_window_ms`, default **200 ms**, measured from the last successful fsync completion. A process crash (SIGKILL), a daemon restart or a warm OS reboot loses nothing: those records are already in the running kernel's page cache."

// HelpLedgerSection is the `trouble --help` ledger block. SPEC-12 renders it;
// the loss-window sentence must appear here verbatim.
func HelpLedgerSection() string {
	return "Ledger:\n" +
		"  append-only JSONL audit ledger, one writer per host, group-committed.\n" +
		"  " + LossWindowStatement + "\n"
}

// HealthLedgerFields is the ledger contribution to GET /health.json
// (SPEC-10, SPEC-01 §4.4). ledger_loss_window_ms and the durable watermark are
// read from memory at O(1) and only after fsync.
func (l *Ledger) HealthLedgerFields() map[string]any {
	st := l.Status()
	return map[string]any{
		"ledger_last_seq":       st.LastSeq,
		"ledger_last_ts":        st.LastTS,
		"ledger_stall_s":        st.StallS,
		"ledger_bytes":          st.Bytes,
		"ledger_loss_window_ms": st.LossWindowMS,
		"ledger_degraded":       st.Index.Degraded,
	}
}

// LossWindowStatement returns the normative crash-loss sentence (health.json
// and `ledger status --json` carry it as ledger_loss_window_ms + the docs copy).
func (l *Ledger) LossWindowStatement() string { return LossWindowStatement }

// ScrubFunc re-scans serialized bytes at the persistence boundary (§4.2).
type ScrubFunc func(ctx context.Context, b []byte) error

// IndexOptions is the in-memory index budget surface (SPEC-01 §2.5, §3.6).
type IndexOptions struct {
	HotDays          int
	BuildBudgetMS    int
	BuildBudgetBytes int64
	MinScanRateMiBs  float64
	MaxGroups        int
	MaxIncidents     int
	MaxSources       int
	IncPerIncident   int
	IncidentRing     int
	MaxBytes         int64
	ColdReadMaxBytes int64
	ColdReadMaxLines int64
}

// Options configures Open. Root, Rotation, Retention, Index, Writer, MaxSchema
// and Now are the §2.2 fields; AssertScrubbed, ScrubVerify and PerLineFsync are
// the seams §4.2 and §7 require (assert_scrubbed is a §2.5 config key; per-line
// mode exists only so TestPerLineRegression can prove group commit's advantage).
type Options struct {
	Root      string          // ledger dir; resolved from the state root. 0700, never /tmp
	Rotation  RotationPolicy  // see types.RotationPolicy
	Retention RetentionPolicy // see types.RetentionPolicy
	Index     IndexOptions
	Writer    types.Actor      // stamped into every record this process writes
	MaxSchema int              // highest schema_version this binary understands (1)
	Now       func() time.Time // injectable clock; production = time.Now

	// HostID is the SPEC-12 host identity stamped into the origin of the
	// housekeeping records the ledger authors itself (§4.1). SPEC-12 owns the
	// value; an empty value degrades to "unknown-host" rather than blocking.
	HostID string
	// Zone labels the host's network zone (loopback|lan|tailnet|public) on
	// per-source liveness rows (§4.4). Empty → "loopback".
	Zone string

	// AssertScrubbed enables the write-boundary re-scan (§4.2, assert_scrubbed).
	AssertScrubbed bool
	// ScrubVerify overrides the mandatory re-scan; nil means scrub.MandatoryScan.
	ScrubVerify ScrubFunc
	// PerLineFsync is test-only: fsync per record instead of group commit.
	PerLineFsync bool
}

// RotationPolicy and RetentionPolicy alias the SPEC-TYPES definitions so the
// ledger's public surface reads as the spec writes it.
type RotationPolicy = types.RotationPolicy

// RetentionPolicy is the SPEC-TYPES retention surface.
type RetentionPolicy = types.RetentionPolicy

// DefaultRotationPolicy returns the §2.5 [ledger] defaults.
func DefaultRotationPolicy() RotationPolicy {
	return RotationPolicy{
		Cadence:         "daily",
		AtUTC:           "00:00:00Z",
		MaxBytes:        DefaultRotateMaxBytes,
		PartSuffix:      DefaultPartSuffix,
		FsyncWindowMS:   DefaultFsyncWindowMS,
		MaxBatchRecords: DefaultMaxBatchRecords,
		QueueCapRecords: DefaultQueueCapRecords,
		MaxEnqueueWait:  DefaultMaxEnqueueWait,
		Fdatasync:       true,
		MaxRecordBytes:  DefaultMaxRecordBytes,
	}
}

// DefaultRetentionPolicy returns the §2.5 retention defaults.
func DefaultRetentionPolicy() RetentionPolicy {
	return RetentionPolicy{
		RawKeep:            "72h",
		CompactedKeep:      "8760h",
		PayloadTTL:         "720h",
		TombstonesKeep:     true,
		CompactionInterval: "24h",
		CompactionMinAge:   "24h",
		CompactionMinBytes: 8388608,
		DiskBudgetBytes:    DefaultDiskBudgetBytes,
		DiskWarnPct:        DefaultDiskWarnPct,
		SpineKinds:         spineKindNames(),
	}
}

// DefaultIndexOptions returns the §2.5 index defaults.
func DefaultIndexOptions() IndexOptions {
	return IndexOptions{
		HotDays:          DefaultIndexHotDays,
		BuildBudgetMS:    DefaultIndexBuildBudgetMS,
		BuildBudgetBytes: DefaultIndexBuildBytes,
		MinScanRateMiBs:  60,
		MaxGroups:        DefaultIndexMaxGroups,
		MaxIncidents:     DefaultIndexMaxIncidents,
		MaxSources:       DefaultIndexMaxSources,
		IncPerIncident:   DefaultIndexIncPerInc,
		IncidentRing:     DefaultIndexIncidentRing,
		MaxBytes:         DefaultIndexMaxBytes,
		ColdReadMaxBytes: DefaultColdReadMaxBytes,
		ColdReadMaxLines: DefaultColdReadMaxLines,
	}
}

func spineKindNames() []string {
	out := []string{
		string(types.KIncident), string(types.KGroup), string(types.KVerify),
		string(types.KPlayRun), string(types.KAgentRun), string(types.KToolCall),
		string(types.KResearch), string(types.KIssue), string(types.KFlow),
		string(types.KSpawn), string(types.KSkill), string(types.KBreaker),
		string(types.KConfig), string(types.KLifecycle), string(types.KGap),
		string(types.KCanary),
	}
	return out
}

// Error is a ledger error carrying a TROUBLE-LEDGER code and the payload.reason
// the ladder reads (SPEC-01 §5).
type Error struct {
	Code   types.ErrorCode
	Reason string
	Msg    string
	Err    error
}

func (e *Error) Error() string {
	if e.Msg == "" {
		return fmt.Sprintf("%s (%s): %v", e.Code, e.Reason, e.Err)
	}
	return fmt.Sprintf("%s (%s): %s", e.Code, e.Reason, e.Msg)
}

func (e *Error) Unwrap() error { return e.Err }

func ledgerErr(code types.ErrorCode, reason, msg string, err error) *Error {
	return &Error{Code: code, Reason: reason, Msg: msg, Err: err}
}

// CodeOf returns the TROUBLE code carried by err, or "".
func CodeOf(err error) types.ErrorCode {
	var le *Error
	if errors.As(err, &le) {
		return le.Code
	}
	var se *scrub.ScanError
	if errors.As(err, &se) {
		return se.Code
	}
	return ""
}

// ReasonOf returns the payload.reason carried by err, or "".
func ReasonOf(err error) string {
	var le *Error
	if errors.As(err, &le) {
		return le.Reason
	}
	return ""
}

// Ledger is the append-only ledger (SPEC-01 §2.2).
type Ledger struct {
	opts      Options
	root      string
	lock      *os.File
	now       func() time.Time
	scrub     ScrubFunc
	idx       *index
	w         *writer
	started   time.Time
	recovered recoveredState
	bootNotes []map[string]any

	closeOnce sync.Once
	closed    atomic.Bool

	// counters surfaced by Status
	headWrites     atomic.Int64
	headFailures   atomic.Int64
	scrubRefusals  atomic.Uint64
	compactionRuns atomic.Int64
	lastCompaction atomic.Int64
}

// origin builds the origin of a ledger-authored housekeeping record (§4.1).
func (l *Ledger) origin(source string) types.Origin {
	host := l.opts.HostID
	if host == "" {
		host = "unknown-host"
	}
	return types.Origin{HostID: host, Source: source}
}

// Open takes the LOCK, recovers the ledger (seq + torn lines), builds the index
// and starts the writer goroutine — all three or it fails (SPEC-01 §2.2, §4.3).
func Open(ctx context.Context, o Options) (*Ledger, error) {
	applyDefaults(&o)
	if err := validateRoot(o.Root); err != nil {
		return nil, err
	}
	qdir := filepath.Join(o.Root, "quarantine")
	if err := os.MkdirAll(qdir, 0o700); err != nil {
		return nil, ledgerErr(types.CodeLedger012, ReasonIO,
			fmt.Sprintf("cannot create %s", qdir), err)
	}
	if err := os.Chmod(qdir, 0o700); err != nil {
		return nil, ledgerErr(types.CodeLedger012, ReasonIO,
			fmt.Sprintf("cannot set mode 0700 on %s", qdir), err)
	}
	lock, err := acquireLock(o.Root, o.Writer)
	if err != nil {
		return nil, err
	}
	l := &Ledger{
		opts:    o,
		root:    o.Root,
		lock:    lock,
		now:     o.Now,
		scrub:   scrub.MandatoryScan,
		idx:     newIndex(o.Index, o.Root, o.Zone, o.MaxSchema),
		started: o.Now(),
	}
	if o.ScrubVerify != nil {
		l.scrub = o.ScrubVerify
	}
	if err := l.recover(ctx); err != nil {
		_ = releaseLock(lock)
		return nil, err
	}
	l.w = newWriter(l)
	l.w.start()
	// Recovery findings reach the audit trail before any producer writes
	// (§3.4 rule 3, §5 code 003, §3.6 rung 5).
	for _, note := range l.bootNotes {
		if err := l.w.selfAppend(types.KLifecycle, note); err != nil {
			_ = l.w.stop()
			_ = releaseLock(lock)
			return nil, err
		}
	}
	if err := l.w.selfAppend(types.KLifecycle, map[string]any{
		"op":       "boot",
		"last_seq": l.recovered.lastSeq,
		"day":      l.recovered.day,
		"file":     partName(l.recovered.day, l.recovered.gen, l.recovered.part),
	}); err != nil {
		_ = l.w.stop()
		_ = releaseLock(lock)
		return nil, err
	}
	return l, nil
}

func applyDefaults(o *Options) {
	d := DefaultRotationPolicy()
	r := DefaultRetentionPolicy()
	ix := DefaultIndexOptions()
	if o.Rotation.Cadence == "" {
		o.Rotation.Cadence = d.Cadence
	}
	if o.Rotation.AtUTC == "" {
		o.Rotation.AtUTC = d.AtUTC
	}
	if o.Rotation.PartSuffix == "" {
		o.Rotation.PartSuffix = d.PartSuffix
	}
	if o.Rotation.MaxBytes == 0 {
		o.Rotation.MaxBytes = d.MaxBytes
	}
	if o.Rotation.FsyncWindowMS == 0 {
		o.Rotation.FsyncWindowMS = d.FsyncWindowMS
	}
	if o.Rotation.MaxBatchRecords == 0 {
		o.Rotation.MaxBatchRecords = d.MaxBatchRecords
	}
	if o.Rotation.QueueCapRecords == 0 {
		o.Rotation.QueueCapRecords = d.QueueCapRecords
	}
	if o.Rotation.MaxEnqueueWait == "" {
		o.Rotation.MaxEnqueueWait = d.MaxEnqueueWait
	}
	if o.Rotation.MaxRecordBytes == 0 {
		o.Rotation.MaxRecordBytes = d.MaxRecordBytes
	}
	if o.Retention.RawKeep == "" {
		o.Retention.RawKeep = r.RawKeep
	}
	if o.Retention.CompactedKeep == "" {
		o.Retention.CompactedKeep = r.CompactedKeep
	}
	if o.Retention.PayloadTTL == "" {
		o.Retention.PayloadTTL = r.PayloadTTL
	}
	if o.Retention.CompactionInterval == "" {
		o.Retention.CompactionInterval = r.CompactionInterval
	}
	if o.Retention.CompactionMinAge == "" {
		o.Retention.CompactionMinAge = r.CompactionMinAge
	}
	if o.Retention.CompactionMinBytes == 0 {
		o.Retention.CompactionMinBytes = r.CompactionMinBytes
	}
	if o.Retention.DiskBudgetBytes == 0 {
		o.Retention.DiskBudgetBytes = r.DiskBudgetBytes
	}
	if o.Retention.DiskWarnPct == 0 {
		o.Retention.DiskWarnPct = r.DiskWarnPct
	}
	if len(o.Retention.SpineKinds) == 0 {
		o.Retention.SpineKinds = r.SpineKinds
	}
	o.Retention.TombstonesKeep = true
	if o.Index.HotDays == 0 {
		o.Index.HotDays = ix.HotDays
	}
	if o.Index.BuildBudgetMS == 0 {
		o.Index.BuildBudgetMS = ix.BuildBudgetMS
	}
	if o.Index.BuildBudgetBytes == 0 {
		o.Index.BuildBudgetBytes = ix.BuildBudgetBytes
	}
	if o.Index.MinScanRateMiBs == 0 {
		o.Index.MinScanRateMiBs = ix.MinScanRateMiBs
	}
	if o.Index.MaxGroups == 0 {
		o.Index.MaxGroups = ix.MaxGroups
	}
	if o.Index.MaxIncidents == 0 {
		o.Index.MaxIncidents = ix.MaxIncidents
	}
	if o.Index.MaxSources == 0 {
		o.Index.MaxSources = ix.MaxSources
	}
	if o.Index.IncPerIncident == 0 {
		o.Index.IncPerIncident = ix.IncPerIncident
	}
	if o.Index.IncidentRing == 0 {
		o.Index.IncidentRing = ix.IncidentRing
	}
	if o.Index.MaxBytes == 0 {
		o.Index.MaxBytes = ix.MaxBytes
	}
	if o.Index.ColdReadMaxBytes == 0 {
		o.Index.ColdReadMaxBytes = ix.ColdReadMaxBytes
	}
	if o.Index.ColdReadMaxLines == 0 {
		o.Index.ColdReadMaxLines = ix.ColdReadMaxLines
	}
	if o.MaxSchema == 0 {
		o.MaxSchema = SchemaVersionV1
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

func validateRoot(root string) error {
	if root == "" {
		return ledgerErr(types.CodeLedger012, ReasonValidation, "ledger root is empty", nil)
	}
	if filepath.Clean(root) == "/tmp" || strings.HasPrefix(filepath.Clean(root), "/tmp/") {
		return ledgerErr(types.CodeLedger012, ReasonValidation,
			fmt.Sprintf("state root %s is under /tmp (never a ledger location)", root), nil)
	}
	fi, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			if mkErr := os.MkdirAll(root, 0o700); mkErr != nil {
				return ledgerErr(types.CodeLedger012, ReasonIO,
					fmt.Sprintf("ledger dir %s does not exist and cannot be created", root), mkErr)
			}
			fi, err = os.Stat(root)
		}
		if err != nil {
			return ledgerErr(types.CodeLedger012, ReasonIO,
				fmt.Sprintf("cannot stat ledger dir %s", root), err)
		}
	}
	if !fi.IsDir() {
		return ledgerErr(types.CodeLedger012, ReasonValidation,
			fmt.Sprintf("ledger dir %s is not a directory", root), nil)
	}
	if mode := fi.Mode().Perm(); mode != 0o700 {
		return ledgerErr(types.CodeLedger012, ReasonValidation,
			fmt.Sprintf("ledger dir %s has mode %04o, expected %04o", root, mode, 0o700), nil)
	}
	return nil
}

// Append fills Seq, RecID, TS and SchemaVersion and returns only once the record
// is durable (SPEC-01 §2.2, §2.1). A draft that supplies a ledger-owned field is
// refused (TROUBLE-LEDGER-001, payload.reason=validation).
func (l *Ledger) Append(ctx context.Context, d types.RecordDraft) (types.Record, error) {
	if l.closed.Load() {
		return types.Record{}, ledgerErr(types.CodeLedger001, ReasonWrite, "ledger is closed", nil)
	}
	if err := ctx.Err(); err != nil {
		return types.Record{}, err
	}
	rec, err := l.prepare(ctx, d)
	if err != nil {
		return types.Record{}, err
	}
	return l.w.enqueue(ctx, rec)
}

// AppendBatch writes K records in ONE group-commit window and returns only
// once the batch is durable (§3.5a). The batch is the durability unit: on a
// nil error every returned record is durable (seqs contiguous, in order); on
// an error the batch may be absent entirely — never partially present — and a
// hole record covers the seq range it would have occupied. Each draft is
// prepared and boundary-checked exactly as Append would (a bad draft fails
// nothing before it was written); an empty draft list returns nil, nil and
// writes nothing. The single-record invariant of §2.2 is untouched: a batch
// MUST NOT span two files, so K is clamped to the space left in
// ledger.max_batch_records and the remainder is a second batch (its own
// window), not a second batch boundary inside one.
func (l *Ledger) AppendBatch(ctx context.Context, drafts []types.RecordDraft) ([]types.Record, error) {
	if l.closed.Load() {
		return nil, ledgerErr(types.CodeLedger001, ReasonWrite, "ledger is closed", nil)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(drafts) == 0 {
		return nil, nil
	}
	recs := make([]types.Record, 0, len(drafts))
	for _, d := range drafts {
		rec, err := l.prepare(ctx, d)
		if err != nil {
			return nil, err
		}
		recs = append(recs, rec)
	}
	return l.w.enqueueBatch(ctx, recs)
}

// prepare validates a draft and stamps everything Append owns except Seq.
func (l *Ledger) prepare(ctx context.Context, d types.RecordDraft) (types.Record, error) {
	if !d.Kind.Valid() {
		return types.Record{}, ledgerErr(types.CodeLedger001, ReasonValidation,
			fmt.Sprintf("unknown record kind %q", d.Kind), nil)
	}
	if d.Origin.HostID == "" || d.Origin.Source == "" {
		return types.Record{}, ledgerErr(types.CodeLedger001, ReasonValidation,
			"origin.host_id and origin.source are required", nil)
	}
	if d.Actor.Kind == "" || d.Actor.ID == "" {
		return types.Record{}, ledgerErr(types.CodeLedger001, ReasonValidation,
			"actor.kind and actor.id are required", nil)
	}
	if d.Sig == "" && d.Kind.SigRequired() {
		return types.Record{}, ledgerErr(types.CodeLedger001, ReasonValidation,
			fmt.Sprintf("kind %s requires a sig", d.Kind), nil)
	}
	body, jerr := canonicalPayloadJSON(d.Payload)
	if jerr != nil {
		return types.Record{}, ledgerErr(types.CodeLedger001, ReasonValidation,
			"payload is not JSON-serializable", jerr)
	}
	if n := int64(len(body)); n > l.opts.Rotation.MaxRecordBytes {
		switch d.Kind {
		case types.KEvent, types.KCanary, types.KGap:
			// high-volume kinds are truncated, never refused (§3.1)
			d.Payload = truncatePayload(d.Payload, n, l.opts.Rotation.MaxRecordBytes)
			body, jerr = canonicalPayloadJSON(d.Payload)
			if jerr != nil {
				return types.Record{}, ledgerErr(types.CodeLedger001, ReasonValidation,
					"payload is not JSON-serializable", jerr)
			}
		default:
			// the audit spine is refused so the chain cannot be mutilated (§3.1)
			return types.Record{}, ledgerErr(types.CodeLedger001, ReasonValidation,
				fmt.Sprintf("payload %d bytes exceeds max_record_bytes %d and kind %s may not be truncated",
					n, l.opts.Rotation.MaxRecordBytes, d.Kind), nil)
		}
	} else if n >= 4096 {
		// payload_sha256 is mandatory once the canonical payload JSON is ≥4096 bytes
		if d.Payload == nil {
			d.Payload = map[string]any{}
		}
		d.Payload["payload_sha256"] = types.DigestShort(types.SigDigest(body))
		body, jerr = canonicalPayloadJSON(d.Payload)
		if jerr != nil {
			return types.Record{}, ledgerErr(types.CodeLedger001, ReasonValidation,
				"payload is not JSON-serializable", jerr)
		}
	}
	if l.opts.AssertScrubbed {
		if err := l.scrub(ctx, body); err != nil {
			l.scrubRefusals.Add(1)
			return types.Record{}, err
		}
	}
	// Degraded payload mode (SPEC-01 §3.7 rule 3): when the disk budget is
	// exceeded, event/canary records keep identity and counters but drop the
	// payload, so AC-6's chain never develops a hole.
	if l.w != nil && l.w.payloadDegraded() {
		switch d.Kind {
		case types.KEvent, types.KCanary:
			d.Payload = map[string]any{"payload_dropped": true, "reason": ReasonDiskBudget}
		}
	}
	return types.Record{
		RecID:         types.NewID(types.PEv),
		TS:            types.FormatUTC(l.now()),
		Kind:          d.Kind,
		SchemaVersion: SchemaVersionV1,
		Sig:           d.Sig,
		Inc:           d.Inc,
		Origin:        d.Origin,
		Actor:         d.Actor,
		Redactions:    d.Redactions,
		Payload:       d.Payload,
	}, nil
}

// Close drains the queue, fdatasyncs, writes HEAD and releases the LOCK.
func (l *Ledger) Close(ctx context.Context) error {
	var err error
	l.closeOnce.Do(func() {
		l.closed.Store(true)
		if l.w != nil {
			err = l.w.stop()
		}
		if l.idx != nil {
			if herr := l.writeHead(); herr != nil && err == nil {
				err = herr
			}
		}
		if l.lock != nil {
			if lerr := releaseLock(l.lock); lerr != nil && err == nil {
				err = lerr
			}
		}
	})
	return err
}

// Status is O(1): no I/O, no lock (SPEC-01 §2.3).
func (l *Ledger) Status() types.LedgerStatus {
	st := l.w.snapshot()
	st.LossWindowMS = l.opts.Rotation.FsyncWindowMS
	st.QueueCap = l.opts.Rotation.QueueCapRecords
	st.QueueDepth = l.w.queueDepth()
	st.BackpressureTotal = l.w.backpressure.Load()
	st.WriterVersion = l.opts.Writer.Version
	st.DiskBudgetBytes = l.opts.Retention.DiskBudgetBytes
	st.DiskBytes = l.diskBytes()
	st.Index = l.idx.stats()
	if st.Records > 0 {
		st.FsyncPerRecord = float64(st.FsyncCalls) / float64(st.Records)
	}
	if st.LastTS != "" {
		if t, err := types.ParseUTC(st.LastTS); err == nil {
			st.StallS = l.now().Sub(t).Seconds()
			if st.StallS < 0 {
				st.StallS = 0
			}
		}
	}
	return st
}

// IndexStats is an O(1) snapshot.
func (l *Ledger) IndexStats() types.IndexStats { return l.idx.stats() }

// Query returns the read surface (SPEC-01 §2.3).
func (l *Ledger) Query() Query { return &query{l: l} }

// Rotate forces a rotation at the next batch boundary and returns the new part
// number (SPEC-01 §3.5 rule 4: rotation is legal only between batches).
func (l *Ledger) Rotate(now time.Time) (int, error) { return l.w.requestRotate(now) }

// DiskBytes returns the ledger directory's total size in bytes.
func (l *Ledger) diskBytes() int64 {
	entries, err := os.ReadDir(l.root)
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

// writeHead writes HEAD via tmp+rename+fsync(dir) (SPEC-01 §3.4).
func (l *Ledger) writeHead() error {
	st := l.w.snapshot()
	h := headFile{
		LastSeq: st.LastSeq,
		Day:     st.Day,
		Gen:     st.Gen,
		Part:    st.Part,
		Bytes:   st.Bytes,
		TS:      types.FormatUTC(l.now()),
		Version: l.opts.Writer.Version,
		GitSHA:  l.opts.Writer.GitSHA,
	}
	b, err := marshalJSON(h)
	if err != nil {
		return ledgerErr(types.CodeLedger001, ReasonHead, "cannot marshal HEAD", err)
	}
	tmp := filepath.Join(l.root, ".HEAD.tmp")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return ledgerErr(types.CodeLedger001, ReasonHead, "cannot create HEAD tmp", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return ledgerErr(types.CodeLedger001, ReasonHead, "cannot write HEAD tmp", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return ledgerErr(types.CodeLedger001, ReasonHead, "cannot fsync HEAD tmp", err)
	}
	if err := f.Close(); err != nil {
		return ledgerErr(types.CodeLedger001, ReasonHead, "cannot close HEAD tmp", err)
	}
	if err := os.Rename(tmp, filepath.Join(l.root, "HEAD")); err != nil {
		return ledgerErr(types.CodeLedger001, ReasonHead, "cannot rename HEAD", err)
	}
	l.headWrites.Add(1)
	return syncDir(l.root)
}

// ScrubRefusals reports how many appends were refused by the write boundary.
func (l *Ledger) ScrubRefusals() uint64 { return l.scrubRefusals.Load() }

// Root returns the ledger directory.
func (l *Ledger) Root() string { return l.root }

// syncDir fsyncs a directory so a rename/create is durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
