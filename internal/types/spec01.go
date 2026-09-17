package types

// Types contributed by SPEC-01 (SPEC-TYPES §3.15.1). Field names, order and
// JSON tags are verbatim and contractual.

// QueryInfo accompanies every Query answer (SPEC-01 §3.10).
type QueryInfo struct {
	Indexed           bool   `json:"indexed"`
	Partial           bool   `json:"partial"`
	Degraded          bool   `json:"degraded"`
	Reason            string `json:"reason"`
	ScannedBytes      int64  `json:"scanned_bytes"`
	ScannedLines      int64  `json:"scanned_lines"`
	TruncatedBeforeTS string `json:"truncated_before_ts"`
	ElapsedMS         int    `json:"elapsed_ms"`
}

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
type LedgerStatus struct {
	LastSeq           uint64     `json:"last_seq"`
	LastSeqPending    uint64     `json:"last_seq_pending"`
	LastTS            string     `json:"last_ts"`
	StallS            float64    `json:"stall_s"`
	LossWindowMS      int        `json:"loss_window_ms"`
	Day               string     `json:"day"`
	Gen               int        `json:"gen"`
	Part              int        `json:"part"`
	File              string     `json:"file"`
	Bytes             int64      `json:"bytes"`
	Records           int64      `json:"records"`
	FsyncCalls        int64      `json:"fsync_calls"`
	FsyncPerRecord    float64    `json:"fsync_per_record"`
	QueueDepth        int        `json:"queue_depth"`
	QueueCap          int        `json:"queue_cap"`
	BackpressureTotal uint64     `json:"backpressure_total"`
	WriterPID         int        `json:"writer_pid"`
	WriterVersion     string     `json:"writer_version"`
	DiskBytes         int64      `json:"disk_bytes"`
	DiskBudgetBytes   int64      `json:"disk_budget_bytes"`
	Index             IndexStats `json:"index"`
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
