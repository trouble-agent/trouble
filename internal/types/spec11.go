package types

// Types contributed by SPEC-11 (SPEC-TYPES §3.13 + §3.15.8): the immutable skill
// artifact, its guards and provenance, the local-only stats row, the candidate
// and the config/status surfaces of the skills package. Field names, order and
// tags are verbatim from SPEC-TYPES and contractual.

// Skill is the SKILL.toml artifact (SPEC-TYPES §3.13). It is immutable: written
// by an author or the promote loop, merged by review in the release channel, and
// thereafter only read. `Stats` is deliberately absent — local stats live in the
// state root's skills-local/stats.json, never in the artifact (SPEC-11 §3.6).
type Skill struct {
	Name             string      `json:"name" toml:"name"`
	Version          int         `json:"version" toml:"version"`
	Sigs             []string    `json:"sigs" toml:"sigs"`
	PlayRef          string      `json:"play_ref" toml:"play_ref"`
	Guards           SkillGuards `json:"guards" toml:"guards"`
	Provenance       Provenance  `json:"provenance" toml:"provenance"`
	MinDaemonVersion string      `json:"min_daemon_version" toml:"min_daemon_version"`
	AllowedModules   []string    `json:"allowed_modules" toml:"allowed_modules"`
	Signature        string      `json:"signature" toml:"signature"`
	SignerKeyID      string      `json:"signer_key_id" toml:"signer_key_id"`
}

// SkillGuards bounds how often and under what conditions a skill may run.
type SkillGuards struct {
	VerifyWindow Duration `json:"verify_window" toml:"verify_window"`
	MaxRuns      string   `json:"max_runs" toml:"max_runs"`       // "<n>/day"
	EscalateOn   string   `json:"escalate_on" toml:"escalate_on"` // verify_fail | tool_error | never
}

// Provenance ties an artifact to the incidents and briefs that taught it.
type Provenance struct {
	Incidents []string `json:"incidents" toml:"incidents"`
	Research  []string `json:"research" toml:"research"`
	Author    string   `json:"author" toml:"author"`
	CreatedTS string   `json:"created_ts" toml:"created_ts"`
}

// SkillStats is LOCAL state (SPEC-TYPES §3.13, SPEC-11 §3.6): it never appears
// in an artifact and is never written back into the channel.
type SkillStats struct {
	Name       string `json:"name"`
	Version    int    `json:"version"`
	Applied    int    `json:"applied"`
	Success    int    `json:"success"`
	LastUsedTS string `json:"last_used_ts"`
	Refusals   int    `json:"refusals"`
}

// SkillCandidate is a local draft of an artifact (SPEC-TYPES §3.13).
type SkillCandidate struct {
	ID          string `json:"id"` // sk_ + ULID
	Name        string `json:"name"`
	Version     int    `json:"version"`
	Sig         string `json:"sig"`
	Inc         string `json:"inc"`
	Play        Play   `json:"play"`
	ResearchID  string `json:"research_id"`
	State       string `json:"state"` // drafted | reviewed | promoted | rejected | pulled | refused
	ReviewActor string `json:"review_actor"`
	BranchOrPR  string `json:"branch_or_pr"`
	CreatedTS   string `json:"created_ts"`
}

// Candidate + install states (SPEC-11 §3.4, §3.7).
const (
	CandDrafted  = "drafted"
	CandReviewed = "reviewed"
	CandPromoted = "promoted"
	CandRejected = "rejected"
	CandRefused  = "refused"
	CandPulled   = "pulled"

	SkillInstalled     = "installed"
	SkillPendingReview = "pending_review"
	SkillHeld          = "held"
	SkillRefused       = "refused"
	SkillConflict      = "conflict"
	SkillCanaryBlocked = "canary_blocked"
	SkillFloorBlocked  = "floor_blocked"
	SkillQuarantined   = "quarantined"
)

// SkillsConfig is the [skills] table (SPEC-TYPES §3.15.8).
type SkillsConfig struct {
	Enabled             bool     `json:"enabled" toml:"enabled"`
	SourcePath          string   `json:"source_path" toml:"source_path"`
	SourceURL           string   `json:"source_url" toml:"source_url"`
	SourceRef           string   `json:"source_ref" toml:"source_ref"`
	RefMode             string   `json:"ref_mode" toml:"ref_mode"` // tag | branch
	PullInterval        Duration `json:"pull_interval" toml:"pull_interval"`
	PullJitterPct       int      `json:"pull_jitter_pct" toml:"pull_jitter_pct"`
	PullOnBoot          bool     `json:"pull_on_boot" toml:"pull_on_boot"`
	PullTimeout         Duration `json:"pull_timeout" toml:"pull_timeout"`
	PullMaxBytes        int64    `json:"pull_max_bytes" toml:"pull_max_bytes"`
	RequireSignature    bool     `json:"require_signature" toml:"require_signature"`
	Approve             string   `json:"approve" toml:"approve"` // auto | review | never
	CanaryHostID        string   `json:"canary_host_id" toml:"canary_host_id"`
	CanaryValidity      Duration `json:"canary_validity" toml:"canary_validity"`
	CanaryOverride      bool     `json:"canary_override" toml:"canary_override"`
	AutoAcceptEnabled   bool     `json:"auto_accept_enabled" toml:"auto_accept_enabled"`
	AutoAcceptThreshold int      `json:"auto_accept_threshold" toml:"auto_accept_threshold"`
	AutoAcceptWindow    Duration `json:"auto_accept_window" toml:"auto_accept_window"`
	AutoAcceptModules   []string `json:"auto_accept_modules" toml:"auto_accept_modules"`
	MaxHold             Duration `json:"max_hold" toml:"max_hold"`
	DemoteAfterFailures int      `json:"demote_after_failures" toml:"demote_after_failures"`
	RetainVersions      int      `json:"retain_versions" toml:"retain_versions"`
	StateDir            string   `json:"state_dir" toml:"state_dir"`
	GitBinary           string   `json:"git_binary" toml:"git_binary"`
	// The local SKILL.md library (SPEC-11 §2b), off by default: nothing is read
	// and nothing is executed unless local_enabled names a local_dir.
	LocalEnabled     bool          `json:"local_enabled" toml:"local_enabled"`
	LocalDir         string        `json:"local_dir" toml:"local_dir"`
	LocalMaxBytes    int64         `json:"local_max_bytes" toml:"local_max_bytes"`
	LocalMaxSkills   int           `json:"local_max_skills" toml:"local_max_skills"`
	LocalMaxSteps    int           `json:"local_max_steps" toml:"local_max_steps"`
	LocalStepTimeout Duration      `json:"local_step_timeout" toml:"local_step_timeout"`
	Signers          []SkillSigner `json:"signers" toml:"signers"`
}

// SkillSigner is one entry of the authoritative signer set (SPEC-TYPES §3.15.8).
// The set is config, never a file in the channel repo.
type SkillSigner struct {
	KeyID     string `json:"key_id" toml:"key_id"`
	PublicKey string `json:"public_key" toml:"public_key"` // base64 std, 32-byte ed25519
	Trust     string `json:"trust" toml:"trust"`           // release | local
	Enabled   bool   `json:"enabled" toml:"enabled"`
	AddedTS   string `json:"added_ts" toml:"added_ts"` // RFC3339 UTC
}

// Signer trust classes (§3.4: a local key can never sign a pulled artifact).
const (
	TrustRelease = "release"
	TrustLocal   = "local"
)

// SkillsStatus is the machine read surface (`trouble skills status --json`).
type SkillsStatus struct {
	HostID        string     `json:"host_id"`
	DaemonVersion string     `json:"daemon_version"`
	Enabled       bool       `json:"enabled"`
	Source        string     `json:"source"` // resolved path/URL, credentials stripped
	Ref           string     `json:"ref"`
	RefMode       string     `json:"ref_mode"`
	LastPullTS    string     `json:"last_pull_ts"`
	LastPullSHA   string     `json:"last_pull_sha"`
	PullFailures  int        `json:"pull_failures"`
	NextPullTS    string     `json:"next_pull_ts"`
	Degraded      bool       `json:"degraded"`
	Approve       string     `json:"approve"`
	CanaryHostID  string     `json:"canary_host_id"`
	Signers       []string   `json:"signers"` // key_id[:trust], enabled only
	Skills        []SkillRow `json:"skills"`
}

// SkillRow is one (name, version) row of the status surface.
type SkillRow struct {
	Name        string `json:"name"`
	Version     int    `json:"version"`
	State       string `json:"state"`
	Origin      string `json:"origin"` // local | pull
	SignerKeyID string `json:"signer_key_id"`
	Sigs        int    `json:"sigs"`
	Applied     int    `json:"applied"`
	Success     int    `json:"success"`
	Refusals    int    `json:"refusals"`
	ErrorCode   string `json:"error_code"`
	Reason      string `json:"reason"`
	InstalledTS string `json:"installed_ts"`
}
