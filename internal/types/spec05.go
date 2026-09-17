package types

import "context"

// Types contributed by SPEC-05 (SPEC-TYPES §3.7, §3.15.4) plus the research seam
// (§3.15.9) the ladder's ResearchPort consumes. Field names, order and JSON tags
// are verbatim from SPEC-TYPES and are contractual: this file is the single
// declaration site, so internal/ladder and internal/registry cannot drift.

// ParkReason is why an in-flight run was parked (SPEC-05 §3.5).
type ParkReason string

const (
	ParkSIGTERM     ParkReason = "sigterm"
	ParkCrash       ParkReason = "crash"
	ParkUpgrade     ParkReason = "upgrade"
	ParkLeaseExpiry ParkReason = "lease_expired"
	ParkKillSwitch  ParkReason = "kill_switch"
)

// AutonomyMode is the autonomy gate matrix's mode (SPEC-TYPES §3.7).
type AutonomyMode string

const (
	AutoShadow   AutonomyMode = "shadow"
	AutoAssisted AutonomyMode = "assisted"
	AutoFull     AutonomyMode = "full"
)

// Valid reports whether m is one of the three frozen modes.
func (m AutonomyMode) Valid() bool {
	switch m {
	case AutoShadow, AutoAssisted, AutoFull:
		return true
	}
	return false
}

// AutonomyGates is the operator state SPEC-05 §3.11 pins; the ladder is its
// owner and every other package reads it.
type AutonomyGates struct {
	Mode             AutonomyMode `json:"mode"`
	KillSwitch       bool         `json:"kill_switch"`
	AllowDetect      bool         `json:"allow_detect"`
	AllowResearch    bool         `json:"allow_research"`
	AllowPlayMutate  bool         `json:"allow_play_mutate"`
	AllowAgent       bool         `json:"allow_agent"`
	AllowSpawn       bool         `json:"allow_spawn"`
	AllowMerge       bool         `json:"allow_merge"`
	AllowPromote     bool         `json:"allow_promote"`
	AllowSkillAccept bool         `json:"allow_skill_accept"`
	Grants           []string     `json:"grants"`
	ChangedBy        string       `json:"changed_by"`
	ChangedTS        string       `json:"changed_ts"`
}

// DefaultGates is the boot state: shadow, nothing granted, kill-switch clear
// (SPEC-05 §3.11: shadow is the default in every install).
func DefaultGates() AutonomyGates {
	return AutonomyGates{Mode: AutoShadow}
}

// ParkRecord is a persisted park row (SPEC-TYPES §3.15.4). A park record is a
// record, never a state: parking never changes LadderState.
type ParkRecord struct {
	ID             string      `json:"id"`
	Inc            string      `json:"inc"`
	Sig            string      `json:"sig"`
	HostID         string      `json:"host_id"`
	Kind           string      `json:"kind"` // play | agent | spawn
	State          LadderState `json:"state"`
	PlayRun        int         `json:"play_run"`
	TaskIndex      int         `json:"task_index"`
	ToolCallID     string      `json:"tool_call_id"`
	ToolsApplied   []string    `json:"tools_applied"`
	Worktree       string      `json:"worktree"`
	PID            int         `json:"pid"`
	RouterRef      string      `json:"router_ref"`
	ParkedTS       string      `json:"parked_ts"`
	ResumeDeadline string      `json:"resume_deadline"`
	Reason         string      `json:"reason"`
	ResumedTS      string      `json:"resumed_ts"`
	ResumedBy      string      `json:"resumed_by"`
}

// Lease states (SPEC-05 §3.6).
const (
	LeaseRequested = "requested"
	LeaseGranted   = "granted"
	LeaseRenewed   = "renewed"
	LeaseReleased  = "released"
	LeaseExpired   = "expired"
	LeaseReclaimed = "reclaimed"
)

// AgentLease is the host-wide agent lease (SPEC-TYPES §3.15.4).
type AgentLease struct {
	LeaseID       string `json:"lease_id"`
	Inc           string `json:"inc"`
	Sig           string `json:"sig"`
	HostID        string `json:"host_id"`
	Holder        string `json:"holder"`
	PID           int    `json:"pid"`
	Worktree      string `json:"worktree"`
	RouterRef     string `json:"router_ref"`
	State         string `json:"state"`
	GrantedTS     string `json:"granted_ts"`
	ExpiresTS     string `json:"expires_ts"`
	RenewedTS     string `json:"renewed_ts"`
	RenewCount    int    `json:"renew_count"`
	ReleaseReason string `json:"release_reason"`
}

// BudgetState holds the per-host, per-UTC-day, per-class counters (SPEC-TYPES §3.15.4).
type BudgetState struct {
	HostID    string           `json:"host_id"`
	Day       string           `json:"day"`
	ResetsTS  string           `json:"resets_ts"`
	Limits    map[string]int64 `json:"limits"`
	Used      map[string]int64 `json:"used"`
	Exhausted []string         `json:"exhausted"`
}

// Budget classes (SPEC-05 §3.7).
const (
	BudgetAgentRuns    = "agent_runs"
	BudgetPlayRuns     = "play_runs"
	BudgetResearchReqs = "research_requests"
	BudgetSpawns       = "spawns"
	BudgetAgentCost    = "agent_cost"
)

// Subject is the research request payload (SPEC-TYPES §3.15.9).
type Subject struct {
	Slug        string         `json:"slug"`
	Description string         `json:"description"`
	Cadence     string         `json:"cadence"`
	Context     map[string]any `json:"context"`
}

// ResearchOutcome is the research rung's result (SPEC-TYPES §3.9).
type ResearchOutcome struct {
	ID             string         `json:"id"`
	Inc            string         `json:"inc"`
	Slug           ClassSlug      `json:"slug"`
	State          string         `json:"state"` // requested | returned | degraded | skipped
	Driver         string         `json:"driver"`
	SubmissionID   string         `json:"submission_id"`
	Brief          map[string]any `json:"brief"`
	DegradedReason string         `json:"degraded_reason"`
	CorpusGrepHit  bool           `json:"corpus_grep_hit"`
	RequestedTS    string         `json:"requested_ts"`
	ReturnedTS     string         `json:"returned_ts"`
}

// Research states (SPEC-07 §3).
const (
	ResRequested = "requested"
	ResReturned  = "returned"
	ResDegraded  = "degraded"
	ResSkipped   = "skipped"
)

// ClassSlug is the class derivation carried on a research outcome (SPEC-TYPES §3.9).
type ClassSlug struct {
	Slug     string `json:"slug"`
	Source   string `json:"source"`
	AppKind  string `json:"app_kind"`
	Taxonomy string `json:"taxonomy"`
	Fallback bool   `json:"fallback"`
}

// ResearchPort is the ladder's dependency-inverted view of the research rung
// (SPEC-TYPES §3.15.9). internal/research satisfies it with a one-line adapter.
type ResearchPort interface {
	Request(ctx context.Context, inc Incident, sub Subject) (ResearchOutcome, error)
	Poll(ctx context.Context, resID string) (ResearchOutcome, error)
}
