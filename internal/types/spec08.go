package types

// Types contributed by SPEC-08 (SPEC-TYPES §3.10 + §3.15.6 + §3.15.10): the
// fleet board-jsonl row, the two flow drivers' request/result shapes, the
// hot-fix spawn/lease/promotion records and the flow config tables.
//
// Field names, order and JSON tags are verbatim from SPEC-TYPES and are
// contractual: BoardRow is the row trouble appends byte-for-byte (SPEC-08 §3.1)
// and ForemanBrief is what a foreman is told.

import "context"

// BoardRow is the fleet board-jsonl row shape (SPEC-TYPES §3.10). The last four
// fields are the trouble extensions; the first fifteen are the fleet schema.
type BoardRow struct {
	ID               string   `json:"id"` // tsk_ + ULID allocated from the board max
	Title            string   `json:"title"`
	Status           string   `json:"status"` // todo | in_progress | done | blocked
	Priority         string   `json:"priority"`
	Complexity       string   `json:"complexity"`
	DependsOn        []string `json:"depends_on"`
	Blocks           []string `json:"blocks"`
	PrimaryModel     string   `json:"primary_model"`
	PrimaryProvider  string   `json:"primary_provider"`
	FallbackModel    string   `json:"fallback_model"`
	FallbackProvider string   `json:"fallback_provider"`
	Reasoning        string   `json:"reasoning"`
	CapabilityTags   []string `json:"capability_tags"`
	WorkerStatus     string   `json:"worker_status"`
	DispatchedAt     string   `json:"dispatched_at"`
	CompletedAt      string   `json:"completed_at"`
	Sig              string   `json:"sig"`
	Inc              string   `json:"inc"`
	IssueRefs        []string `json:"issue_refs"`
	Repo             string   `json:"repo"`
}

// BoardEvent is the companion event row (SPEC-TYPES §3.15.6). Its ID is the
// board's own MAX(id)+1 across the event file, which is why it is typed.
type BoardEvent struct {
	ID     uint64         `json:"id"`
	Type   string         `json:"type"` // task_created | task_comment | review_approve | task_closed
	TaskID string         `json:"task_id"`
	TS     string         `json:"ts"`
	Actor  string         `json:"actor"` // "troubled@0.1.0" | "human:<label>"
	Detail map[string]any `json:"detail"`
}

// Board event types (SPEC-08 §3.1).
const (
	EvTaskCreated  = "task_created"
	EvTaskComment  = "task_comment"
	EvReviewApprov = "review_approve"
	EvTaskClosed   = "task_closed"
)

// FileTaskRequest is the one input of a filing (SPEC-TYPES §3.15.6).
type FileTaskRequest struct {
	Project    string     `json:"project"`
	BoardPath  string     `json:"board_path"`
	Row        BoardRow   `json:"row"`
	Event      BoardEvent `json:"event"`
	ReviewMode string     `json:"review_mode"`
	IdemKey    string     `json:"idem_key"`
	DryRun     bool       `json:"dry_run"`
}

// FileTaskResult is a filing's answer (SPEC-TYPES §3.15.6).
type FileTaskResult struct {
	Wrote      bool   `json:"wrote"`
	TaskID     string `json:"task_id"`
	EventID    string `json:"event_id"`
	DupOf      string `json:"dup_of"`
	Diff       Diff   `json:"diff"`
	Valid      bool   `json:"valid"`
	ValidateRC int    `json:"validate_rc"`
	LatencyMS  int    `json:"latency_ms"`
}

// FlowDriver is the two-implementation filing contract (SPEC-TYPES §3.15.6).
type FlowDriver interface {
	Name() string
	Healthcheck(ctx context.Context) (DriverHealth, error)
	FileTask(ctx context.Context, req FileTaskRequest) (FileTaskResult, error)
	CommentTask(ctx context.Context, row BoardRow, body string) (BoardRow, error)
}

// FlowTimelineStep is the dashboard read model (SPEC-TYPES §3.15.6, AC-19).
type FlowTimelineStep struct {
	Stage     string   `json:"stage"`  // filed | foreman | patch | verify | promote
	TS        string   `json:"ts"`     //
	State     string   `json:"state"`  // done | running | pending | failed | denied
	Detail    string   `json:"detail"` //
	RecIDs    []string `json:"rec_ids"`
	TaskID    string   `json:"task_id"`
	SpawnID   string   `json:"spawn_id"`
	Worktree  string   `json:"worktree"`
	PRURL     string   `json:"pr_url"`
	ErrorCode string   `json:"error_code"`
}

// SpawnRequest is one hot-fix spawn (SPEC-TYPES §3.10).
type SpawnRequest struct {
	ID              string `json:"id"` // sp_ + ULID
	Inc             string `json:"inc"`
	Sig             string `json:"sig"`
	Repo            string `json:"repo"`
	Worktree        string `json:"worktree"`
	TaskID          string `json:"task_id"`
	PriorityClass   string `json:"priority_class"`
	Brief           string `json:"brief"`
	ResearchBriefID string `json:"research_brief_id"`
	RouterRef       string `json:"router_ref"`
	State           string `json:"state"`
	Attempts        int    `json:"attempts"`
	LastError       string `json:"last_error"`
	RequestedTS     string `json:"requested_ts"`
}

// Spawn states (SPEC-08 §3.9).
const (
	SpawnRequested    = "requested"
	SpawnPending      = "spawn_pending"
	SpawnAccepted     = "accepted"
	SpawnFailed       = "failed"
	SpawnLeased       = "leased"
	SpawnReleased     = "released"
	SpawnPromoted     = "promoted"
	SpawnDiscarded    = "discarded"
	SpawnWorktreeMode = "owner_created"
	SpawnModeSerial   = "owner_serialized"
)

// HotfixLease is the one-fix-per-sig lease (SPEC-TYPES §3.10).
type HotfixLease struct {
	Sig       string `json:"sig"`
	Inc       string `json:"inc"`
	Holder    string `json:"holder"`
	GrantedTS string `json:"granted_ts"`
	ExpiresTS string `json:"expires_ts"`
}

// Promotion is the post-verify decision (SPEC-TYPES §3.10).
type Promotion struct {
	Inc            string   `json:"inc"`
	TaskID         string   `json:"task_id"`
	VerifyEvidence Evidence `json:"verify_evidence"`
	Decision       string   `json:"decision"` // promoted | discarded | pending_human
	DecidedBy      string   `json:"decided_by"`
	PRURL          string   `json:"pr_url"`
	Worktree       string   `json:"worktree"`
	DecidedTS      string   `json:"decided_ts"`
}

// Promotion decisions (SPEC-08 §3.12).
const (
	PromotePending = "pending_human"
	PromoteDone    = "promoted"
	PromoteDiscard = "discarded"
)

// FlowConfig is the [flow] table (SPEC-TYPES §3.10).
type FlowConfig struct {
	Driver                 string                 `json:"driver"` // board-jsonl | task-router | none
	ReviewMode             string                 `json:"review_mode"`
	BoardPath              string                 `json:"board_path"`
	IDPrefix               string                 `json:"id_prefix"`
	ValidateCmd            string                 `json:"validate_cmd"`
	DependsOn              []string               `json:"depends_on"`
	ExtraTags              []string               `json:"extra_tags"`
	RegistrationProbeEvery Duration               `json:"registration_probe_interval"`
	RegistrationStaleMax   Duration               `json:"registration_stale_max"`
	SchedulerEndpoint      string                 `json:"scheduler_endpoint"`
	SchedulerTokenFile     string                 `json:"scheduler_token_file"`
	Router                 RouterConfig           `json:"router"`
	Projects               map[string]FlowProject `json:"projects"`
	Hotfix                 HotfixConfig           `json:"hotfix"`
}

// HotfixConfig is the [flow.hotfix] table (SPEC-TYPES §3.10).
type HotfixConfig struct {
	Enabled              bool              `json:"enabled"`
	AllowedRepos         []string          `json:"allowed_repos"`
	UnitRepoMap          map[string]string `json:"unit_repo_map"`
	ForemanSpawn         string            `json:"foreman_spawn"`
	PriorityClass        string            `json:"priority_class"`
	VerifyWindow         Duration          `json:"verify_window"`
	Promote              string            `json:"promote"`
	MaxConcurrent        int               `json:"max_concurrent"`
	MinFreeDiskGB        int64             `json:"min_free_disk_gb"`
	WorktreeBase         string            `json:"worktree_base"`
	WorktreeExempt       []string          `json:"worktree_exempt"`
	LeaseTTL             Duration          `json:"lease_ttl"`
	SpawnAckTimeout      Duration          `json:"spawn_ack_timeout"`
	SpawnWorktreeTimeout Duration          `json:"spawn_worktree_timeout"`
	MaxAttempts          int               `json:"max_attempts"`
	MinSeverity          Severity          `json:"min_severity"`
	MutexWait            Duration          `json:"mutex_wait"`
	TestHintCmd          string            `json:"test_hint_cmd"`
	Models               map[string]string `json:"models"`
	CapabilityTags       []string          `json:"capability_tags"`
}

// FlowProject is one filing target (SPEC-TYPES §3.15.10).
type FlowProject struct {
	Name        string `json:"name"`
	Repo        string `json:"repo"`
	BoardPath   string `json:"board_path"`
	Enabled     bool   `json:"enabled"`
	Hotfix      bool   `json:"hotfix"`
	Scheduler   string `json:"scheduler"`
	Registered  bool   `json:"registered"`
	Ticked      bool   `json:"ticked"`
	LastProbeTS string `json:"last_probe_ts"`
	Reason      string `json:"reason"`
}

// RouterConfig is the task-router driver's config (SPEC-TYPES §3.15.10).
type RouterConfig struct {
	Mode         string   `json:"mode"` // http | cli
	Endpoint     string   `json:"endpoint"`
	DispatchPath string   `json:"dispatch_path"`
	CLIPath      string   `json:"cli_path"`
	TokenEnv     string   `json:"token_env"`
	TokenFile    string   `json:"token_file"`
	Timeout      Duration `json:"timeout"`
	Retries      int      `json:"retries"`
}

// ForemanBrief is what a spawned foreman is told (SPEC-TYPES §3.15.10).
type ForemanBrief struct {
	Sig            string            `json:"sig"`
	Inc            string            `json:"inc"`
	TaskID         string            `json:"task_id"`
	Title          string            `json:"title"`
	Severity       Severity          `json:"severity"`
	Repo           string            `json:"repo"`
	Worktree       string            `json:"worktree"`
	BoardPath      string            `json:"board_path"`
	EvidenceBundle map[string]any    `json:"evidence_bundle"`
	ResearchBrief  map[string]any    `json:"research_brief"`
	FailingTests   []string          `json:"failing_test_hints"`
	ToolContract   string            `json:"tool_contract"`
	AllowedModules []string          `json:"allowed_modules"`
	DoNotTouch     DoNotTouch        `json:"do_not_touch"`
	VerifyWindow   Duration          `json:"verify_window"`
	Promote        string            `json:"promote"`
	Models         map[string]string `json:"models"`
	Budget         map[string]any    `json:"budget"`
	Constraints    []string          `json:"constraints"`
	DaemonVersion  string            `json:"daemon_version"`
	GitSHA         string            `json:"git_sha"`
	CreatedTS      string            `json:"created_ts"`
}

// Flow driver names, review modes, promote modes and the tool contract
// (SPEC-08 §3.3, §3.6, §3.7, §3.14).
const (
	FlowDriverBoard  = "board-jsonl"
	FlowDriverRouter = "task-router"
	FlowDriverNone   = "none"

	FlowReviewAuto   = "auto"
	FlowReviewReview = "review"
	FlowReviewNever  = "never"

	FlowPromoteHuman = "human"
	FlowPromoteAuto  = "auto-after-verify"
	FlowSpawnRouter  = "router_spawn"

	ToolContractRegistryOnly = "registry-only"
)
