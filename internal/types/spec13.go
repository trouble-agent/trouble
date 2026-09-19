package types

// SPEC-13 (server profiles) shared types: the resolved server profile the hub
// consumes (SPEC-TYPES §3.15.11) and the TROUBLE-HUB error catalog.
//
// Only the RESOLVED shape lives here. The runtime half of the profile — the
// Redis stream/consumer group, the dedup gate and the DuckBrain archival tier —
// belongs to internal/hub (SPEC-13 §2.3, §8) and the profile's config keys are
// ordinary lifecycle keys (SPEC-12 §3.7a rule 1), so nothing here imports
// internal/lifecycle and internal/lifecycle imports nothing from a hub package
// that does not exist yet.

// ProfileConfig is the resolved `[server]` profile and the keys it requires
// (SPEC-13 §2.1, SPEC-TYPES §3.15.11). internal/lifecycle builds it from the
// same registry rows every other config key resolves through, so the profile
// carries provenance in `trouble config explain` like any other key instead of
// being a second config system (SPEC-12 §3.7a rule 1).
//
// Valid/InvalidReason carry the SPEC-13 §4.1 boot gate's verdict: Valid=false
// means the daemon refuses to start with TROUBLE-HUB-001 (exit 13) before the
// first bind. `standalone` (the default) is valid with no Redis and no
// DuckBrain, which is what keeps the standalone default intact.
//
// The empty-string values are meaningful and never rewritten in place: HubID ""
// and Consumer "" both resolve to origin.host_id at the point of USE (one
// consumer per state root, SPEC-13 §3.3), and resolving them here would make the
// explain dump disagree with what the operator declared.
type ProfileConfig struct {
	Profile         string   `json:"profile"`                // "standalone" | "light-hub"
	HubID           string   `json:"hub_id"`                 // "" → origin.host_id
	RedisURL        string   `json:"redis_url"`              // required when profile=light-hub
	RedisStream     string   `json:"redis_stream"`           // "trouble:ingest"
	ConsumerGroup   string   `json:"consumer_group"`         // "ledger-writers"
	Consumer        string   `json:"consumer"`               // "" → origin.host_id (one consumer per state root)
	MaxLen          int64    `json:"maxlen"`                 // 1000000, approximate trim; un-acked entries are never trimmed
	DedupTTL        Duration `json:"dedup_ttl"`              // "24h"
	RequireRedis    bool     `json:"require_redis"`          // false → degrade to the standalone path instead of refusing
	DBNamespace     string   `json:"duckbrain_namespace"`    // required when profile=light-hub
	DBEndpoint      string   `json:"duckbrain_endpoint"`     // the duckbrain driver endpoint (SPEC-09 driver)
	ArchiveInterval Duration `json:"archive_interval"`       // "1h"
	KeepLocalGens   int      `json:"keep_local_generations"` // generations retained on the hot host after a verified export
	Valid           bool     `json:"valid"`                  // false → TROUBLE-HUB-001 at boot, exit 13
	InvalidReason   string   `json:"invalid_reason"`         // "" | missing_redis_url | missing_namespace | satellite_profile | unknown_profile
}

// SPEC-13 error codes (TROUBLE-HUB-001..016) per SPEC-13 §5 and the canonical
// catalog in SPEC-TYPES §5. The class column is the SPEC-TYPES §5 value.
const (
	CodeHub001 ErrorCode = "TROUBLE-HUB-001" // permanent: invalid/incomplete server profile
	CodeHub002 ErrorCode = "TROUBLE-HUB-002" // permanent: Redis connection or credentials rejected
	CodeHub003 ErrorCode = "TROUBLE-HUB-003" // transient: Redis unreachable at boot
	CodeHub004 ErrorCode = "TROUBLE-HUB-004" // transient: stream write failed (429/503 to senders)
	CodeHub005 ErrorCode = "TROUBLE-HUB-005" // permanent: consumer-group operation failed
	CodeHub006 ErrorCode = "TROUBLE-HUB-006" // transient: dedup gate unavailable → bounded LRU fallback
	CodeHub007 ErrorCode = "TROUBLE-HUB-007" // permanent: stream entry does not decode as ForwardEnvelope
	CodeHub008 ErrorCode = "TROUBLE-HUB-008" // transient: entries stranded past claim_min_idle × 3
	CodeHub009 ErrorCode = "TROUBLE-HUB-009" // permanent: archival target unusable
	CodeHub010 ErrorCode = "TROUBLE-HUB-010" // transient: DuckBrain unreachable during export
	CodeHub011 ErrorCode = "TROUBLE-HUB-011" // permanent: export refused (generation not closed / bytes changed)
	CodeHub012 ErrorCode = "TROUBLE-HUB-012" // transient: export verification failed
	CodeHub013 ErrorCode = "TROUBLE-HUB-013" // permanent: live profile switch requested via reload
	CodeHub014 ErrorCode = "TROUBLE-HUB-014" // permanent: retention tried to drop an unverified generation
	CodeHub015 ErrorCode = "TROUBLE-HUB-015" // permanent: Redis deployment topology refused at preflight
	CodeHub016 ErrorCode = "TROUBLE-HUB-016" // permanent: appendonly=no with require_persistence=true
)

// HubCodeClass is the SPEC-13 §5 class column.
var HubCodeClass = map[ErrorCode]ErrorClass{
	CodeHub001: ErrClassPermanent,
	CodeHub002: ErrClassPermanent,
	CodeHub003: ErrClassTransient,
	CodeHub004: ErrClassTransient,
	CodeHub005: ErrClassPermanent,
	CodeHub006: ErrClassTransient,
	CodeHub007: ErrClassPermanent,
	CodeHub008: ErrClassTransient,
	CodeHub009: ErrClassPermanent,
	CodeHub010: ErrClassTransient,
	CodeHub011: ErrClassPermanent,
	CodeHub012: ErrClassTransient,
	CodeHub013: ErrClassPermanent,
	CodeHub014: ErrClassPermanent,
	CodeHub015: ErrClassPermanent,
	CodeHub016: ErrClassPermanent,
}

func init() {
	for code, class := range HubCodeClass {
		CodeClass[code] = class
	}
}

// RedisStreamOffsets is the live queue state of SPEC-TYPES §3.15.11: the zero
// value is exactly what a standalone profile reports (SPEC-13 §3.3 reports the
// same fields in /health.json every heartbeat).
type RedisStreamOffsets struct {
	Stream          string `json:"stream"`
	Group           string `json:"group"`
	Consumer        string `json:"consumer"`
	StreamLen       int64  `json:"stream_len"`
	Pending         int64  `json:"pending"`
	Lag             int64  `json:"lag"`
	LastDeliveredID string `json:"last_delivered_id"`
	LastAckedID     string `json:"last_acked_id"`
	Reclaims        int64  `json:"reclaims"`
	DedupHits       int64  `json:"dedup_hits"`
	DedupMisses     int64  `json:"dedup_misses"`
	DedupConflicts  int64  `json:"dedup_conflicts"`
	// DedupRestored is the size of the ledger idempotency index the gate was
	// re-warmed with at the last (re)wire (SPEC-13 §2.1.1 rule 5a; the same
	// number dedup.state persists as `restored_keys`). 0 means the
	// `server.redis.failover_grace` window is not warm.
	DedupRestored  int64  `json:"dedup_restored"`
	AOF            bool   `json:"aof"`
	Policy         string `json:"policy"`
	EvictedKeys    int64  `json:"evicted_keys"`
	OptionsChecked bool   `json:"options_checked"`
	DedupWindow    string `json:"dedup_window"`
	Backpressure   int64  `json:"backpressure_total"`
	Degraded       bool   `json:"degraded"`
	DegradedReason string `json:"degraded_reason"`
	Since          string `json:"since"`
}

// HubStatus is the server profile's stanza in the one health surface
// (SPEC-TYPES §3.15.11, SPEC-13 §4.1 step 4). It is present only when a profile
// with a runtime is in force; `Enabled=false` is the standalone answer.
type HubStatus struct {
	Enabled        bool               `json:"enabled"`
	Profile        string             `json:"profile"`
	Since          string             `json:"since"`
	Redis          RedisStreamOffsets `json:"redis"`
	ArchiveQueue   int64              `json:"archive_queue"`
	ArchiveLastTS  string             `json:"archive_last_ts"`
	ArchivedFiles  int64              `json:"archived_files"`
	DroppableGens  int                `json:"droppable_generations"`
	MarkerPending  int64              `json:"marker_pending"`
	RouteCounters  map[string]int64   `json:"route_counters"`
	Degraded       bool               `json:"degraded"`
	DegradedReason string             `json:"degraded_reason"`
}

// LedgerArchiveMarker is one append-only object per generation transition
// (SPEC-TYPES §3.15.11, SPEC-13 §3.5). State ∈ pending|exported|verified|dropped|failed.
type LedgerArchiveMarker struct {
	MarkerID   string `json:"marker_id"`
	File       string `json:"file"`
	Namespace  string `json:"namespace"`
	ObjectKey  string `json:"object_key"`
	Bytes      int64  `json:"bytes"`
	GzipBytes  int64  `json:"gzip_bytes"`
	Sha256     string `json:"sha256"`
	Records    int64  `json:"records"`
	FirstSeq   uint64 `json:"first_seq"`
	LastSeq    uint64 `json:"last_seq"`
	MinTS      string `json:"min_ts"`
	MaxTS      string `json:"max_ts"`
	State      string `json:"state"`
	VerifiedTS string `json:"verified_ts"`
	ErrorCode  string `json:"error_code"`
	TS         string `json:"ts"`
}
