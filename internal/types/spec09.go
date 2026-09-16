package types

// Types contributed by SPEC-09 (SPEC-TYPES §3.11) that SPEC-08 and SPEC-10 consume
// across a package boundary: the issue reference and the driver health surface.
// The rest of SPEC-09's contract (EnsureBySig*, IssueDriver) lands with that
// area; declaring only these two keeps one declaration site.

import "context"

// IssueRef is the issue desk's reference projection (SPEC-TYPES §3.11).
type IssueRef struct {
	ID         string `json:"id"` // iss_ + ULID
	Driver     string `json:"driver"`
	Sig        string `json:"sig"`
	ExternalID string `json:"external_id"`
	URL        string `json:"url"`
	State      string `json:"state"` // open | closed
	CreatedTS  string `json:"created_ts"`
	UpdatedTS  string `json:"updated_ts"`
	Comments   int    `json:"comments"`
	TaskID     string `json:"task_id"`
	ResearchID string `json:"research_id"`
}

// DriverHealth is one driver's healthcheck answer (SPEC-TYPES §3.11). Both the
// flow drivers and the issue drivers report it.
type DriverHealth struct {
	Driver             string `json:"driver"`
	OK                 bool   `json:"ok"`
	Detail             string `json:"detail"`
	RateLimitRemaining int    `json:"rate_limit_remaining"`
	RateLimitResetTS   string `json:"rate_limit_reset_ts"`
	CheckedTS          string `json:"checked_ts"`
}

// EnsureBySigRequest and EnsureBySigResponse are the issue desk's dedup entry
// point (SPEC-TYPES §3.11); declared here because SPEC-08's issueDesk seam and
// SPEC-09's driver both name them.
type EnsureBySigRequest struct {
	Sig         string   `json:"sig"`
	Title       string   `json:"title"`
	Body        string   `json:"body"`
	Labels      []string `json:"labels"`
	DedupWindow Duration `json:"dedup_window"`
	Severity    Severity `json:"severity"`
}

// EnsureBySigResponse is the issue desk's answer (SPEC-TYPES §3.11).
type EnsureBySigResponse struct {
	Ref       IssueRef `json:"ref"`
	Created   bool     `json:"created"`
	Commented bool     `json:"commented"`
}

// IssueDriver is the issue desk's driver contract (SPEC-TYPES §3.11).
type IssueDriver interface {
	Name() string
	Healthcheck(ctx context.Context) (DriverHealth, error)
	EnsureBySig(ctx context.Context, req EnsureBySigRequest) (EnsureBySigResponse, error)
	Comment(ctx context.Context, ref IssueRef, body string) (IssueRef, error)
	Close(ctx context.Context, ref IssueRef, reason string) (IssueRef, error)
}
