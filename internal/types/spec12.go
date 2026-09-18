package types

// SPEC-12 (lifecycle) shared types: heartbeat file, spool entries, topology
// decisions, the health assembly input the dashboard and the stall checker share
// (SPEC-TYPES §3.14).

// Heartbeat is the content of <state_root>/heartbeat.json (0600), written by
// lifecycle.HeartbeatLoop every lifecycle.heartbeat_interval.
type Heartbeat struct {
	TS            string            `json:"ts"`
	PID           int               `json:"pid"`
	Version       string            `json:"version"`
	GitSHA        string            `json:"git_sha"`
	LedgerLastSeq uint64            `json:"ledger_last_seq"`
	LedgerLastTS  string            `json:"ledger_last_ts"`
	Stage         string            `json:"stage,omitempty"` // "" | shutdown
	Sensors       map[string]string `json:"sensors"`         // sensor → last-success RFC3339
}

// SpoolEntry is one line of a satellite spool segment (SPEC-TYPES §3.14).
type SpoolEntry struct {
	ID        string `json:"id"` // ev_ + ULID
	TS        string `json:"ts"`
	Kind      string `json:"kind"` // forward | issue | spawn | skill
	Payload   []byte `json:"payload"`
	Attempts  int    `json:"attempts"`
	IdemKey   string `json:"idem_key"`
	NextTryTS string `json:"next_try_ts"`
}

// Topology is the deployment topology rung (SPEC-12 §3.7).
type Topology string

const (
	T1 Topology = "T1"
	T2 Topology = "T2"
	T3 Topology = "T3"
	T4 Topology = "T4"
	T5 Topology = "T5"
)

// TopologyDecision is one row of `trouble topology` (SPEC-TYPES §3.14).
type TopologyDecision struct {
	From       Topology `json:"from"`
	To         Topology `json:"to"`
	Decision   string   `json:"decision"`
	ConfigKeys []string `json:"config_keys"`
}

// HealthInputs is the assembly input for lifecycle.Health: every field comes from
// a named producer (SPEC-12 §3.3 field table) and the result is the single
// HealthResponse shape the dashboard serves and the stall checker parses.
type HealthInputs struct {
	Version       string
	GitSHA        string
	BuildTime     string
	UptimeS       float64
	LedgerLastSeq uint64
	LedgerLastTS  string
	LedgerStallS  float64
	Sensors       []SensorHealth
	Sources       []SourceLiveness
	Autonomy      AutonomyGates
	Breakers      []Breaker
	RW            RuntimeWatermarks
	// Subsystems is the built/refused block of §3.3a, read from the composition
	// root's live subsystem set (never re-derived here: one truth per question).
	Subsystems []SubsystemHealth

	// DegradedReasons is a list of machine-readable causes (e.g. "unstamped_build",
	// "forward_refused", "cgroup_v1"); any entry makes the response "degraded".
	DegradedReasons []string
	// Stalled forces status="stalled" (the writer's own lag exceeds max_seq_age).
	Stalled bool
	// RSSWarnBytes is lifecycle.self_rss_warn: RW.RSSBytes above it degrades.
	RSSWarnBytes int64
}

// SPEC-12 error codes (TROUBLE-LIFECYCLE-001..017) per SPEC-12 §5.
const (
	CodeLifecycle001 ErrorCode = "TROUBLE-LIFECYCLE-001" // permanent: config file invalid
	CodeLifecycle002 ErrorCode = "TROUBLE-LIFECYCLE-002" // permanent: precedence conflict (non-fatal)
	CodeLifecycle003 ErrorCode = "TROUBLE-LIFECYCLE-003" // permanent: bind preflight refused
	CodeLifecycle004 ErrorCode = "TROUBLE-LIFECYCLE-004" // permanent: state root unusable
	CodeLifecycle005 ErrorCode = "TROUBLE-LIFECYCLE-005" // permanent: state dir mode wrong (not 0700)
	CodeLifecycle006 ErrorCode = "TROUBLE-LIFECYCLE-006" // transient: unit install/repair failed
	CodeLifecycle007 ErrorCode = "TROUBLE-LIFECYCLE-007" // transient: watchdog ping missed
	CodeLifecycle008 ErrorCode = "TROUBLE-LIFECYCLE-008" // transient: heartbeat stale / liveness surface unreadable
	CodeLifecycle009 ErrorCode = "TROUBLE-LIFECYCLE-009" // permanent: ledger sequence stall
	CodeLifecycle010 ErrorCode = "TROUBLE-LIFECYCLE-010" // transient: escalation hook failed on every channel
	CodeLifecycle011 ErrorCode = "TROUBLE-LIFECYCLE-011" // transient: upgrade park failed
	CodeLifecycle012 ErrorCode = "TROUBLE-LIFECYCLE-012" // permanent: schema downgrade refused
	CodeLifecycle013 ErrorCode = "TROUBLE-LIFECYCLE-013" // permanent: secret-bearing file mode wrong / argv secret
	CodeLifecycle014 ErrorCode = "TROUBLE-LIFECYCLE-014" // permanent: forward protocol version unsupported
	CodeLifecycle015 ErrorCode = "TROUBLE-LIFECYCLE-015" // transient: spool budget exceeded → drop-oldest
	CodeLifecycle016 ErrorCode = "TROUBLE-LIFECYCLE-016" // permanent: unit missing escalation wiring
	CodeLifecycle017 ErrorCode = "TROUBLE-LIFECYCLE-017" // transient: clock skew beyond tolerance
)

// LifecycleCodeClass is the SPEC-12 §5 class column.
var LifecycleCodeClass = map[ErrorCode]ErrorClass{
	CodeLifecycle001: ErrClassPermanent,
	CodeLifecycle002: ErrClassPermanent,
	CodeLifecycle003: ErrClassPermanent,
	CodeLifecycle004: ErrClassPermanent,
	CodeLifecycle005: ErrClassPermanent,
	CodeLifecycle006: ErrClassTransient,
	CodeLifecycle007: ErrClassTransient,
	CodeLifecycle008: ErrClassTransient,
	CodeLifecycle009: ErrClassPermanent,
	CodeLifecycle010: ErrClassTransient,
	CodeLifecycle011: ErrClassTransient,
	CodeLifecycle012: ErrClassPermanent,
	CodeLifecycle013: ErrClassPermanent,
	CodeLifecycle014: ErrClassPermanent,
	CodeLifecycle015: ErrClassTransient,
	CodeLifecycle016: ErrClassPermanent,
	CodeLifecycle017: ErrClassTransient,
}

func init() {
	for code, class := range LifecycleCodeClass {
		CodeClass[code] = class
	}
}
