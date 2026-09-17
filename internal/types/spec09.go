package types

import "context"

// Types contributed by SPEC-09 (SPEC-TYPES §3.11 + §3.15.7): the four-method
// issue-driver contract, its request/response/health shapes, and the desk's
// config + runtime counter shapes. Field names, order and JSON tags are verbatim
// from SPEC-TYPES and contractual: this file is the single declaration site, so
// internal/issues and every consumer cannot drift.

// IssueRef is the one issue a sig owns at a driver (SPEC-TYPES §3.11).
type IssueRef struct {
	ID         string `json:"id"`     // iss_ + ULID
	Driver     string `json:"driver"` // github | duckbrain
	Sig        string `json:"sig"`
	ExternalID string `json:"external_id"` // gh issue number | duckbrain key
	URL        string `json:"url"`
	State      string `json:"state"` // open | closed
	CreatedTS  string `json:"created_ts"`
	UpdatedTS  string `json:"updated_ts"`
	Comments   int    `json:"comments"`
	TaskID     string `json:"task_id"`
	ResearchID string `json:"research_id"`
}

// Issue states (§3.2: read back from the driver, never inferred).
const (
	IssueOpen   = "open"
	IssueClosed = "closed"
)

// EnsureBySigRequest is the ensure input (SPEC-TYPES §3.11). Body is already
// scrubbed when it reaches a driver (SPEC-02, SPEC-09 §2.1).
type EnsureBySigRequest struct {
	Sig         string   `json:"sig"`
	Title       string   `json:"title"`
	Body        string   `json:"body"` // scrubbed evidence bundle
	Labels      []string `json:"labels"`
	DedupWindow Duration `json:"dedup_window"`
	Severity    Severity `json:"severity"`
}

// EnsureBySigResponse reports what the ensure did (SPEC-TYPES §3.11).
type EnsureBySigResponse struct {
	Ref       IssueRef `json:"ref"`
	Created   bool     `json:"created"`   // false → existing issue matched inside the dedup window
	Commented bool     `json:"commented"` // true → this call folded a recurrence
}

// IssueDriver is the frozen driver contract (SPEC-TYPES §3.11). Nothing else is
// required of a driver, and nothing in the desk knows a driver's internals.
type IssueDriver interface {
	// Name is the driver identifier: [a-z][a-z0-9_-]{0,31}. It equals the
	// `driver` key in config and the `driver` field of every IssueRef returned.
	Name() string

	// Healthcheck is a cheap read-only probe; it never mutates anything.
	Healthcheck(ctx context.Context) (DriverHealth, error)

	// EnsureBySig guarantees at most ONE issue per (driver, sig, project).
	EnsureBySig(ctx context.Context, req EnsureBySigRequest) (EnsureBySigResponse, error)

	// Comment appends exactly one comment per distinct trigger key.
	Comment(ctx context.Context, ref IssueRef, body string) (IssueRef, error)

	// Close is idempotent: closing a closed issue is success, not an error.
	Close(ctx context.Context, ref IssueRef, reason string) (IssueRef, error)
}

// DriverHealth is one driver's probe result (SPEC-TYPES §3.11). Detail is one
// scrubbed human line and never carries a token, key or query string.
type DriverHealth struct {
	Driver             string `json:"driver"`
	OK                 bool   `json:"ok"`
	Detail             string `json:"detail"`
	RateLimitRemaining int    `json:"rate_limit_remaining"`
	RateLimitResetTS   string `json:"rate_limit_reset_ts"`
	CheckedTS          string `json:"checked_ts"`
}

// IssueDeskConfig is the resolved [issues] table (SPEC-TYPES §3.15.7).
type IssueDeskConfig struct {
	Enabled           bool                `json:"enabled"`
	PrimaryDriver     string              `json:"primary_driver"`
	DedupWindow       Duration            `json:"dedup_window"`
	QuietClose        Duration            `json:"quiet_close"`
	QuietCloseSweep   Duration            `json:"quiet_close_sweep"`
	OpDeadline        Duration            `json:"op_deadline"`
	BodyMaxBytes      int                 `json:"body_max_bytes"`
	TitleMaxChars     int                 `json:"title_max_chars"`
	ReplayInterval    Duration            `json:"replay_interval"`
	ReplayBatch       int                 `json:"replay_batch"`
	HealthcheckEvery  Duration            `json:"healthcheck_interval"`
	HealthcheckIdle   Duration            `json:"healthcheck_idle"`
	FailAfterProbes   int                 `json:"fail_after_probes"`
	SpoolBudgetBytes  int64               `json:"spool_budget_bytes"`
	SpoolMaxEntries   int                 `json:"spool_max_entries"`
	SpoolTTL          Duration            `json:"spool_ttl"`
	SpoolMinRetention Duration            `json:"spool_min_retention"`
	MaxAttemptsPerOp  int                 `json:"max_attempts_per_op"`
	AckDefault        Duration            `json:"ack_default"`
	Caps              IssueCaps           `json:"caps"`
	Drivers           []IssueDriverConfig `json:"drivers"`
}

// IssueCaps is the anti-spray cap set of SPEC-09 §3.8.
type IssueCaps struct {
	PerSigCreates       int      `json:"per_sig_creates"`
	PerSigWindow        Duration `json:"per_sig_window"`
	PerSigComments      int      `json:"per_sig_comments"`
	CommentMinInterval  Duration `json:"comment_min_interval"`
	PerProjectCreatesH  int      `json:"per_project_creates_h"`
	PerProjectCreatesD  int      `json:"per_project_creates_d"`
	PerProjectCommentsH int      `json:"per_project_comments_h"`
	GlobalCreatesH      int      `json:"global_creates_h"`
	GlobalCreatesD      int      `json:"global_creates_d"`
	GlobalCommentsH     int      `json:"global_comments_h"`
}

// IssueDriverConfig is one driver's configuration block (SPEC-TYPES §3.15.7).
// APIKeyHeader is a header NAME; no key value is ever represented here.
type IssueDriverConfig struct {
	Name              string            `json:"name"` // github | duckbrain
	Enabled           bool              `json:"enabled"`
	Primary           bool              `json:"primary"`
	Mirror            bool              `json:"mirror"`
	DedupWindow       Duration          `json:"dedup_window"` // "" → IssueDeskConfig.DedupWindow
	MaxAttempts       int               `json:"max_attempts"`
	BaseBackoff       Duration          `json:"base_backoff"`
	MaxBackoff        Duration          `json:"max_backoff"`
	Jitter            float64           `json:"jitter"`
	Timeout           Duration          `json:"timeout"`
	MinRemaining      int               `json:"min_remaining"`
	SearchMinInterval Duration          `json:"search_min_interval"`
	Owner             string            `json:"owner,omitempty"`
	Repo              string            `json:"repo,omitempty"`
	APIBase           string            `json:"api_base,omitempty"`
	TokenFile         string            `json:"token_file,omitempty"`
	TokenEnv          string            `json:"token_env,omitempty"`
	Labels            []string          `json:"labels,omitempty"`
	LabelsExtra       []string          `json:"labels_extra,omitempty"`
	SeverityLabels    map[string]string `json:"severity_labels,omitempty"`
	SourceLabels      bool              `json:"source_labels,omitempty"`
	BaseURL           string            `json:"base_url,omitempty"`
	APIKeyFile        string            `json:"api_key_file,omitempty"`
	APIKeyEnv         string            `json:"api_key_env,omitempty"`
	APIKeyHeader      string            `json:"api_key_header,omitempty"`
	APIKeyScheme      string            `json:"api_key_scheme,omitempty"`
	NSPrefix          string            `json:"ns_prefix,omitempty"`
}

// IssueCapState is the runtime counter set (SPEC-TYPES §3.15.7). It is rebuilt
// from the ledger at boot, so a restart never lifts a cap.
type IssueCapState struct {
	Driver           string            `json:"driver"`
	WindowTS         string            `json:"window_ts"`
	SigCreates       map[string]int    `json:"sig_creates"`
	SigComments      map[string]int    `json:"sig_comments"`
	LastCommentTS    map[string]string `json:"last_comment_ts"`
	ProjectCreatesH  map[string]int    `json:"project_creates_h"`
	ProjectCreatesD  map[string]int    `json:"project_creates_d"`
	ProjectCommentsH map[string]int    `json:"project_comments_h"`
	GlobalCreatesH   int               `json:"global_creates_h"`
	GlobalCreatesD   int               `json:"global_creates_d"`
	GlobalCommentsH  int               `json:"global_comments_h"`
	Suppressed       int               `json:"suppressed"`
}

// IssueAttempt is the per-attempt ledger record shape (SPEC-TYPES §3.15.7). It
// is written when attempt > 1 or the operation failed.
type IssueAttempt struct {
	Op          string `json:"op"` // ensure | comment | close | reopen | link | healthcheck
	Driver      string `json:"driver"`
	IdemKey     string `json:"idem_key"`
	Attempt     int    `json:"attempt"`
	MaxAttempts int    `json:"max_attempts"`
	Outcome     string `json:"outcome"` // ok | retry | give_up | spooled
	HTTPStatus  int    `json:"http_status"`
	ErrorCode   string `json:"error_code"`
	Retryable   bool   `json:"retryable"`
	BackoffMS   int    `json:"backoff_ms"`
	NextTryTS   string `json:"next_try_ts"`
	Remaining   int    `json:"rate_limit_remaining"`
	ResetTS     string `json:"rate_limit_reset_ts"`
}
