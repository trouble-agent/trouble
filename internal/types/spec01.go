package types

import (
	"fmt"
	"strconv"
	"strings"
)

// Types contributed by SPEC-01 (SPEC-TYPES §3.15.1). Field names, order and
// JSON tags are verbatim and contractual.

// QueryInfo accompanies every Query answer (SPEC-01 §3.10). Reset/ResetReason
// are the §2.3a page outcome: a page token whose generation is gone, foreign or
// unparsable is answered with Reset=true plus the reason and a newest-first
// restart — never an error, because a client whose page fell off the retention
// cliff has done nothing wrong (SPEC-01 §5).
type QueryInfo struct {
	Indexed           bool   `json:"indexed"`
	Partial           bool   `json:"partial"`
	Degraded          bool   `json:"degraded"`
	Reason            string `json:"reason"`
	ScannedBytes      int64  `json:"scanned_bytes"`
	ScannedLines      int64  `json:"scanned_lines"`
	TruncatedBeforeTS string `json:"truncated_before_ts"`
	ElapsedMS         int    `json:"elapsed_ms"`
	Reset             bool   `json:"reset"`
	ResetReason       string `json:"reset_reason"`
}

// Page reset reasons (SPEC-01 §2.3a outcome table).
const (
	ResetGenerationDropped = "generation_dropped"
	ResetTokenInvalid      = "token_invalid"
	ResetTokenForeign      = "token_foreign"
)

// IndexStats is the O(1) index snapshot (SPEC-01 §3.10).
type IndexStats struct {
	Entries             int     `json:"entries"`
	Groups              int     `json:"groups"`
	GroupsCold          int     `json:"groups_cold"`
	Incidents           int     `json:"incidents"`
	Sources             int     `json:"sources"`
	Days                int     `json:"days"`
	FilesScanned        int     `json:"files_scanned"`
	BytesScanned        int64   `json:"bytes_scanned"`
	LinesScanned        int64   `json:"lines_scanned"`
	TornLines           int     `json:"torn_lines"`
	CorruptLines        int     `json:"corrupt_lines"`
	SkippedNewerSchema  int     `json:"skipped_newer_schema"`
	BuildMS             int     `json:"build_ms"`
	BudgetMS            int     `json:"budget_ms"`
	BudgetBytes         int64   `json:"budget_bytes"`
	IndexBytes          int64   `json:"index_bytes"`
	ScanRateMiBs        float64 `json:"scan_rate_mibs"`
	Degraded            bool    `json:"degraded"`
	DegradedReason      string  `json:"degraded_reason"`
	TruncatedBeforeTS   string  `json:"truncated_before_ts"`
	ColdEvictionsPerMin float64 `json:"cold_evictions_per_min"`
}

// LedgerStatus is printed by `trouble ledger status --json` (SPEC-01 §3.10).
//
// Its members are two kinds of measurement, and the JSON shape keeps them
// distinguishable (SPEC-01 §3.10b, TRBL-130): the numbers a reader DERIVES from
// the ledger files (the lock-free ReadStatus walk `trouble ledger status`
// takes) and the numbers only the process holding the writer seat can take
// (the fdatasync counter, the enqueue queue, refusals, the allocated-not-yet-
// durable watermark). A member a status path could not measure is OMITTED from
// that path's JSON and named in Unmeasured with its reason — never reported as
// 0, because a 0 an operator cannot tell from "clean" is a lie. Go callers read
// such members through the accessors below, which return ok=false instead of a
// fabricated zero.
type LedgerStatus struct {
	LastSeq uint64 `json:"last_seq"`
	// LastSeqPending is the writer's allocated-not-yet-durable watermark: a
	// writer-seat measurement, absent from the read-only path.
	LastSeqPending *uint64 `json:"last_seq_pending,omitempty"`
	LastTS         string  `json:"last_ts"`
	// StallS is seconds since the durable watermark. Both paths measure it (the
	// read-only one from last_ts and the current clock); it is absent only when
	// the ledger holds no record to be stalled since.
	StallS       *float64 `json:"stall_s,omitempty"`
	LossWindowMS int      `json:"loss_window_ms"`
	Day          string   `json:"day"`
	Gen          int      `json:"gen"`
	Part         int      `json:"part"`
	File         string   `json:"file"`
	Bytes        int64    `json:"bytes"`
	Records      int64    `json:"records"`
	// FsyncCalls is the writer's in-process fdatasync counter. Only the process
	// that owns the writer seat can read it: Ledger.Status() sets it,
	// ReadStatus() leaves it nil (see Unmeasured).
	FsyncCalls *int64 `json:"fsync_calls,omitempty"`
	// FsyncPerRecord is FsyncCalls/Records and shares its presence, plus one
	// extra absence: with no records the rate has no denominator and is
	// "no_records", not 0.
	FsyncPerRecord *float64 `json:"fsync_per_record,omitempty"`
	// QueueDepth is the live writer queue's occupancy: writer seat only.
	QueueDepth *int `json:"queue_depth,omitempty"`
	// QueueCap is the configured queue cap (a policy default, not a
	// measurement), so both paths report it.
	QueueCap int `json:"queue_cap"`
	// BackpressureTotal counts this process's refusals (SPEC-01 §3.5b): writer
	// seat only. GapRecords/EstLostTotal are the ledger-side answer and both
	// paths derive them.
	BackpressureTotal *uint64 `json:"backpressure_total,omitempty"`
	// GapRecords and EstLostTotal are the RECORDED side of the same event
	// (SPEC-01 §3.5b): BackpressureTotal counts refusals in this process, and
	// these say how many of them the ledger actually carries. Every refusal is
	// folded into one gap record per drain window, so the obligation an
	// operator can check at a glance is
	// est_lost_total == backpressure_total for the refusals the writer managed
	// to record — a crash inside a drain window is the only bounded edge, and
	// a mismatch means loss the ledger cannot account for.
	GapRecords   int64 `json:"gap_records"`
	EstLostTotal int64 `json:"est_lost_total"`
	// Unmeasured names every member this status path did not measure, and why
	// (field -> reason: "in_process_counter" | "no_records"). Absent when the
	// path measured everything; a nil map and an empty "{}" both mean "nothing
	// was missing".
	Unmeasured      map[string]string `json:"unmeasured,omitempty"`
	WriterPID       int               `json:"writer_pid"`       // -1 means no verified holder PID; WriterLockHeld distinguishes unknown identity from no writer.
	WriterLockHeld  bool              `json:"writer_lock_held"` // true means the kernel lock is held, even if writer_pid is unavailable.
	WriterVersion   string            `json:"writer_version"`
	DiskBytes       int64             `json:"disk_bytes"`
	DiskBudgetBytes int64             `json:"disk_budget_bytes"`
	// PlayRuns and AgentRuns are the ledger-derived rung counters (SPEC-01
	// §3.10a): the number of records in the day's partitions that carry
	// payload.rung_entry == "play" / "agent", i.e. the rung entries the ladder
	// actually recorded. They are read-only facts about the ledger, not a live
	// in-memory gauge, so `ledger status` answers them without the writer seat.
	PlayRuns  int64      `json:"play_runs"`
	AgentRuns int64      `json:"agent_runs"`
	Index     IndexStats `json:"index"`
}

// Reasons a status member can be absent (SPEC-01 §3.10b). They are wire values:
// a caller reads them from `unmeasured`.
const (
	// UnmeasuredInProcess is the reason for a member that lives in the writer
	// process: another process cannot read its counters, and no reader may
	// invent one.
	UnmeasuredInProcess = "in_process_counter"
	// UnmeasuredNoRecords is the reason for a rate whose denominator is empty:
	// the quantity is undefined on this ledger, not zero.
	UnmeasuredNoRecords = "no_records"
)

// MarkUnmeasured records that this status could not measure field, and why.
func (s *LedgerStatus) MarkUnmeasured(field, reason string) {
	if s.Unmeasured == nil {
		s.Unmeasured = map[string]string{}
	}
	s.Unmeasured[field] = reason
}

// The accessors below are the only safe way for Go callers to read a measured
// member: ok=false means "this status path did not measure it", which a zero
// cannot say.

// FsyncCount returns the measured fdatasync count and whether it was measured.
func (s LedgerStatus) FsyncCount() (int64, bool) {
	if s.FsyncCalls == nil {
		return 0, false
	}
	return *s.FsyncCalls, true
}

// FsyncRate returns fsyncs per record and whether the rate was measured.
func (s LedgerStatus) FsyncRate() (float64, bool) {
	if s.FsyncPerRecord == nil {
		return 0, false
	}
	return *s.FsyncPerRecord, true
}

// PendingSeq returns the allocated-not-yet-durable watermark and whether it was
// measured.
func (s LedgerStatus) PendingSeq() (uint64, bool) {
	if s.LastSeqPending == nil {
		return 0, false
	}
	return *s.LastSeqPending, true
}

// QueueLen returns the writer queue's measured occupancy.
func (s LedgerStatus) QueueLen() (int, bool) {
	if s.QueueDepth == nil {
		return 0, false
	}
	return *s.QueueDepth, true
}

// Refusals returns the measured in-process refusal count.
func (s LedgerStatus) Refusals() (uint64, bool) {
	if s.BackpressureTotal == nil {
		return 0, false
	}
	return *s.BackpressureTotal, true
}

// StallSeconds returns the measured stall and whether it was measured.
func (s LedgerStatus) StallSeconds() (float64, bool) {
	if s.StallS == nil {
		return 0, false
	}
	return *s.StallS, true
}

// GroupStat is one row of TopGroups (SPEC-01 §3.10).
type GroupStat struct {
	GroupID     string        `json:"group_id"`
	Sig         string        `json:"sig"`
	Digest      string        `json:"digest"`
	MergeKey    string        `json:"merge_key"`
	Source      string        `json:"source"`
	Title       string        `json:"title"`
	Count       uint64        `json:"count"`
	Rate1m      float64       `json:"rate_1m"`
	Rate5m      float64       `json:"rate_5m"`
	Rate60m     float64       `json:"rate_60m"`
	Trend       float64       `json:"trend"`
	FirstSeenTS string        `json:"first_seen_ts"`
	LastSeenTS  string        `json:"last_seen_ts"`
	IncidentID  string        `json:"incident_id"`
	Counters    GroupCounters `json:"counters"`
	Cold        bool          `json:"cold"`
}

// SourceAge is per-source liveness (SPEC-01 §3.10).
type SourceAge struct {
	HostID        string  `json:"host_id"`
	Source        string  `json:"source"`
	Zone          string  `json:"zone"`
	LastEventTS   string  `json:"last_event_ts"`
	LastEventAgeS float64 `json:"last_event_age_s"`
	LastSeq       uint64  `json:"last_seq"`
	EventsTotal   uint64  `json:"events_total"`
	Events24h     uint64  `json:"events_24h"`
	Gaps24h       int     `json:"gaps_24h"`
	Dropped24h    uint64  `json:"dropped_24h"`
	Redactions24h uint64  `json:"redactions_24h"`
	CanarySeen    bool    `json:"canary_seen"`
	CanaryLastTS  string  `json:"canary_last_ts"`
}

// EvidenceBundle is one incident's audit chain plus its gaps (SPEC-01 §3.10).
type EvidenceBundle struct {
	Inc        string         `json:"inc"`
	Sig        string         `json:"sig"`
	Digest     string         `json:"digest"`
	MergeKey   string         `json:"merge_key"`
	GroupID    string         `json:"group_id"`
	FirstSeq   uint64         `json:"first_seq"`
	LastSeq    uint64         `json:"last_seq"`
	Records    []Record       `json:"records"`
	KindCounts map[string]int `json:"kind_counts"`
	RefCount   int            `json:"ref_count"`
	Overflow   int            `json:"overflow"`
	Gaps       []GapRecord    `json:"gaps"`
	Partial    bool           `json:"partial"`
	Info       QueryInfo      `json:"info"`
}

// RotationPolicy is the [ledger] rotation surface (SPEC-01 §2.5).
type RotationPolicy struct {
	Cadence         string `json:"cadence"`
	AtUTC           string `json:"at_utc"`
	MaxBytes        int64  `json:"max_bytes"`
	PartSuffix      string `json:"part_suffix"`
	FsyncWindowMS   int    `json:"fsync_window_ms"`
	MaxBatchRecords int    `json:"max_batch_records"`
	QueueCapRecords int    `json:"queue_cap_records"`
	MaxEnqueueWait  string `json:"max_enqueue_wait"`
	Fdatasync       bool   `json:"fdatasync"`
	MaxRecordBytes  int64  `json:"max_record_bytes"`
}

// RetentionPolicy is the [ledger] retention surface (SPEC-01 §2.5).
type RetentionPolicy struct {
	RawKeep            Duration `json:"raw_keep"`
	CompactedKeep      Duration `json:"compacted_keep"`
	PayloadTTL         Duration `json:"payload_ttl"`
	TombstonesKeep     bool     `json:"tombstones_keep"`
	CompactionInterval Duration `json:"compaction_interval"`
	CompactionMinAge   Duration `json:"compaction_min_age"`
	CompactionMinBytes int64    `json:"compaction_min_bytes"`
	DiskBudgetBytes    int64    `json:"disk_budget_bytes"`
	DiskWarnPct        int      `json:"disk_warn_pct"`
	SpineKinds         []string `json:"spine_kinds"`
}

// CompactionResult is the outcome of one generation rewrite (SPEC-01 §3.10).
type CompactionResult struct {
	Day                 string   `json:"day"`
	FromFiles           []string `json:"from_files"`
	ToFile              string   `json:"to_file"`
	FromGen             int      `json:"from_gen"`
	ToGen               int      `json:"to_gen"`
	LinesIn             int64    `json:"lines_in"`
	LinesOut            int64    `json:"lines_out"`
	BytesIn             int64    `json:"bytes_in"`
	BytesOut            int64    `json:"bytes_out"`
	RecordsKept         int64    `json:"records_kept"`
	Aggregates          int64    `json:"aggregates"`
	PayloadsExpired     int64    `json:"payloads_expired"`
	PayloadsDropped     int64    `json:"payloads_dropped"`
	TombstoneCountsKept bool     `json:"tombstone_counts_kept"`
	ElapsedMS           int      `json:"elapsed_ms"`
	DryRun              bool     `json:"dry_run"`
	ErrorCode           string   `json:"error_code"`
}

// ---- page and sidecar types (SPEC-TYPES §3.15.1, SPEC-01 §2.3a / §3.7a) ----

// pageTokenSep is the token grammar separator: "{generation}::{byte_offset}::{seq}".
const pageTokenSep = "::"

// PageToken is the ledger page cursor. It is opaque to clients (they never
// construct one), stable within a generation's lifetime, and invalidated cleanly
// when a generation is dropped (SPEC-01 §2.3a). ByteOffset is a real offset into
// a real file and Seq is the resume point if the file changed under the walk.
type PageToken struct {
	Generation string `json:"generation"`
	ByteOffset int64  `json:"byte_offset"`
	Seq        uint64 `json:"seq"`
}

// String renders the token in the §2.3a grammar. The empty token (the
// newest-first start) renders as "".
func (t PageToken) String() string {
	if t.Generation == "" {
		return ""
	}
	return t.Generation + pageTokenSep +
		strconv.FormatInt(t.ByteOffset, 10) + pageTokenSep +
		strconv.FormatUint(t.Seq, 10)
}

// IsZero reports whether the token starts a newest-first walk.
func (t PageToken) IsZero() bool { return t.Generation == "" }

// ParsePageToken parses the §2.3a grammar. "" is the start token, not an error;
// any other malformed form is an error (the caller answers with a
// ResetReason="token_invalid" reset, never a 5xx).
func ParsePageToken(s string) (PageToken, error) {
	if s == "" {
		return PageToken{}, nil
	}
	parts := strings.Split(s, pageTokenSep)
	if len(parts) != 3 {
		return PageToken{}, fmt.Errorf("types: page token %q is not {generation}::{byte_offset}::{seq}", s)
	}
	gen := parts[0]
	if gen == "" {
		return PageToken{}, fmt.Errorf("types: page token %q has an empty generation", s)
	}
	if strings.ContainsAny(gen, "/\\") {
		return PageToken{}, fmt.Errorf("types: page token %q has a path in its generation", s)
	}
	off, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || off < 0 {
		return PageToken{}, fmt.Errorf("types: page token %q has an invalid byte_offset", s)
	}
	seq, err := strconv.ParseUint(parts[2], 10, 64)
	if err != nil {
		return PageToken{}, fmt.Errorf("types: page token %q has an invalid seq", s)
	}
	return PageToken{Generation: gen, ByteOffset: off, Seq: seq}, nil
}

// GenerationIndex is the {file}.idx sidecar (SPEC-01 §3.7a): written at rotation
// (and for the new generation at compaction), fsynced before the file is
// announced as authoritative. Offsets + FirstSeq/LastSeq are what make a
// {file}::{byte_offset}::{seq} token resolvable in O(log n). The sidecar is an
// accelerator: losing it costs a scan, never an answer.
type GenerationIndex struct {
	File         string  `json:"file"`
	Records      int64   `json:"records"`
	Bytes        int64   `json:"bytes"`
	FirstSeq     uint64  `json:"first_seq"`
	LastSeq      uint64  `json:"last_seq"`
	MinTS        string  `json:"min_ts"`
	MaxTS        string  `json:"max_ts"`
	Offsets      []int64 `json:"offsets"`
	OffsetStride int     `json:"offset_stride"`
	TornLines    int     `json:"torn_lines"`
	Sha256       string  `json:"sha256"`
}
