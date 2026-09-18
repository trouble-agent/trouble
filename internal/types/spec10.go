package types

// SPEC-10 (dashboard) shared types: the health surface every consumer parses and
// the token/scope model the dashboard authenticates against (SPEC-TYPES §3.12).
//
// HealthResponse is assembled once per request by dashboard.BuildHealth from the
// lifecycle triple, SPEC-03 sensors, SPEC-05 liveness/gates/breakers and the
// SPEC-01 index — and it is the same struct the external stall checker
// (SPEC-12 §3.3) parses. There is no second health shape.

// HealthResponse is GET /health.json (and the same fields the dashboard strip
// renders). Field names are frozen by SPEC-12 §3.3.
type HealthResponse struct {
	Status        string            `json:"status"` // ok | degraded | stalled
	Version       string            `json:"version"`
	GitSHA        string            `json:"git_sha"`
	BuildTime     string            `json:"build_time"`
	UptimeS       float64           `json:"uptime_s"`
	LedgerLastSeq uint64            `json:"ledger_last_seq"`
	LedgerLastTS  string            `json:"ledger_last_ts"`
	LedgerStallS  float64           `json:"ledger_stall_s"`
	Sensors       []SensorHealth    `json:"sensors"`
	Sources       []SourceLiveness  `json:"sources"`
	Autonomy      AutonomyGates     `json:"autonomy"`
	Breakers      []Breaker         `json:"breakers"`
	RW            RuntimeWatermarks `json:"runtime_watermarks"`
	// Subsystems is the per-subsystem built/refused block (SPEC-12 §3.3a). The
	// key is always present: a health surface that cannot say whether its
	// subsystems were built is how an instance whose ingest plane is absent
	// reports a green light.
	Subsystems []SubsystemHealth `json:"subsystems"`
	Detail     map[string]any    `json:"detail,omitempty"` // e.g. {"reason":"unstamped_build"}
}

// SubsystemHealth is one row of the per-subsystem built/refused block on the
// health surface (SPEC-TYPES §3.12, rule in SPEC-12 §3.3a). One row exists for
// every late-landing subsystem, in build order, and it is the only shape in
// which a boot reports that a subsystem did not build: `built=false` is never a
// claim that the subsystem is healthy, and `refused=true` carries the code and
// the reason the boot recorded.
type SubsystemHealth struct {
	Name    string `json:"name"`             // sentinel | research | flow | issues | skills
	Built   bool   `json:"built"`            // true only while the composition root holds a live subsystem
	Refused bool   `json:"refused"`          // true when the boot recorded a refusal for this subsystem
	Code    string `json:"code,omitempty"`   // the refusal's TROUBLE-*-NNN code, when it carries one
	Reason  string `json:"reason,omitempty"` // the refusal's own detail (a config key or an unwired driver, never a secret)
}

// RuntimeWatermarks are the resource counters on the health surface
// (SPEC-TYPES §3.12).
type RuntimeWatermarks struct {
	BinaryBytes      int64   `json:"binary_bytes"`
	RSSBytes         int64   `json:"rss_bytes"`
	RSSPeakBytes     int64   `json:"rss_peak_bytes"`
	MemHighBytes     int64   `json:"mem_high_bytes"`
	MemMaxBytes      int64   `json:"mem_max_bytes"`
	LedgerBytes      int64   `json:"ledger_bytes"`
	SpoolBytes       int64   `json:"spool_bytes"`
	SpoolBudgetBytes int64   `json:"spool_budget_bytes"`
	EventsPerMin     float64 `json:"events_per_min"`
	GroupsOpen       int     `json:"groups_open"`
	IncidentsOpen    int     `json:"incidents_open"`
	Worktrees        int     `json:"worktrees"`
}

// Scope is the dashboard's linear scope hierarchy: autonomy ⇒ write ⇒ read
// (SPEC-10 §2.2). Scope values are exactly these three strings.
type Scope string

const (
	ScopeRead     Scope = "read"
	ScopeWrite    Scope = "write"
	ScopeAutonomy Scope = "autonomy"
)

// Token is one entry of the dashboard token store (SPEC-TYPES §3.12). The
// plaintext is never stored: Hash is sha256(token)[:32] hex.
type Token struct {
	ID         string  `json:"id"`   // token label, e.g. "dash-read@phone"
	Hash       string  `json:"hash"` // sha256(token)[:32] hex
	Scopes     []Scope `json:"scopes"`
	CreatedTS  string  `json:"created_ts"`
	Revoked    bool    `json:"revoked"`
	LastUsedTS string  `json:"last_used_ts"`
}

// SPEC-10 error codes (TROUBLE-DASHBOARD-001..013) per SPEC-10 §5.
const (
	CodeDashboard001 ErrorCode = "TROUBLE-DASHBOARD-001" // permanent: no auth material
	CodeDashboard002 ErrorCode = "TROUBLE-DASHBOARD-002" // permanent: auth material invalid
	CodeDashboard003 ErrorCode = "TROUBLE-DASHBOARD-003" // permanent: under-scoped
	CodeDashboard004 ErrorCode = "TROUBLE-DASHBOARD-004" // permanent: CSRF failed
	CodeDashboard005 ErrorCode = "TROUBLE-DASHBOARD-005" // permanent: token in a URL
	CodeDashboard006 ErrorCode = "TROUBLE-DASHBOARD-006" // permanent: non-loopback bind without a mandate
	CodeDashboard007 ErrorCode = "TROUBLE-DASHBOARD-007" // transient: template render failed
	CodeDashboard008 ErrorCode = "TROUBLE-DASHBOARD-008" // permanent: partial/view not found
	CodeDashboard009 ErrorCode = "TROUBLE-DASHBOARD-009" // permanent: route not found
	CodeDashboard010 ErrorCode = "TROUBLE-DASHBOARD-010" // permanent: write action refused / stale view
	CodeDashboard011 ErrorCode = "TROUBLE-DASHBOARD-011" // permanent: body too large
	CodeDashboard012 ErrorCode = "TROUBLE-DASHBOARD-012" // transient: rate limited
	CodeDashboard013 ErrorCode = "TROUBLE-DASHBOARD-013" // permanent: identity provider unavailable
)

// DashboardCodeClass is the SPEC-10 §5 class column.
var DashboardCodeClass = map[ErrorCode]ErrorClass{
	CodeDashboard001: ErrClassPermanent,
	CodeDashboard002: ErrClassPermanent,
	CodeDashboard003: ErrClassPermanent,
	CodeDashboard004: ErrClassPermanent,
	CodeDashboard005: ErrClassPermanent,
	CodeDashboard006: ErrClassPermanent,
	CodeDashboard007: ErrClassTransient,
	CodeDashboard008: ErrClassPermanent,
	CodeDashboard009: ErrClassPermanent,
	CodeDashboard010: ErrClassPermanent,
	CodeDashboard011: ErrClassPermanent,
	CodeDashboard012: ErrClassTransient,
	CodeDashboard013: ErrClassPermanent,
}

func init() {
	for code, class := range DashboardCodeClass {
		CodeClass[code] = class
	}
}
