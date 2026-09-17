package types

// Types contributed by SPEC-06 (SPEC-TYPES §3.8, §3.15.5): the frozen module SDK,
// the diff/result/audit shapes, plays as data, do-not-touch and the call request.
// Field names, order and JSON tags are verbatim from SPEC-TYPES and contractual —
// the module surface is frozen at v1 and must not evolve in v0.1.

import "context"

// IdempotencyClass is the module idempotency contract (SPEC-06 §3.4).
type IdempotencyClass string

const (
	IdemPure       IdempotencyClass = "pure"
	IdemConvergent IdempotencyClass = "convergent"
	IdemOnce       IdempotencyClass = "once"
)

// Valid reports whether c is one of the three frozen classes.
func (c IdempotencyClass) Valid() bool {
	switch c {
	case IdemPure, IdemConvergent, IdemOnce:
		return true
	}
	return false
}

// Call modes (SPEC-06 §2.3).
const (
	ModeCheck = "check_mode"
	ModeApply = "apply"
)

// Call stages, in fixed order (SPEC-06 §2.3). The CallStage slice is 0-indexed.
const (
	StageAuthorize = "authorize"
	StageValidate  = "validate"
	StageDryRun    = "dry_run"
	StageApply     = "apply"
	StageVerify    = "verify"
	StageAudit     = "audit"
)

// StageIndex maps a stage name onto its fixed position.
var StageIndex = map[string]int{
	StageAuthorize: 0, StageValidate: 1, StageDryRun: 2,
	StageApply: 3, StageVerify: 4, StageAudit: 5,
}

// Descriptor is a module's full declaration (SPEC-TYPES §3.8).
type Descriptor struct {
	Name        string           `json:"name"`
	Version     int              `json:"version"`
	Schema      map[string]any   `json:"schema"`
	Scopes      []string         `json:"scopes"`
	Idempotency IdempotencyClass `json:"idempotency"`
	CheckMode   bool             `json:"check_mode"`
	TimeoutS    int              `json:"timeout_s"`
	Mutating    bool             `json:"mutating"`
}

// DiffEntry is one predicted (or applied) change (SPEC-TYPES §3.8).
type DiffEntry struct {
	Path   string `json:"path"`
	Before any    `json:"before"`
	After  any    `json:"after"`
}

// Diff is the dry-run prediction (SPEC-TYPES §3.8).
type Diff struct {
	Empty   bool        `json:"empty"`
	Entries []DiffEntry `json:"entries"`
	Summary string      `json:"summary"`
}

// Result is the outcome of an apply (SPEC-TYPES §3.8).
type Result struct {
	Changed    bool           `json:"changed"`
	Applied    []DiffEntry    `json:"applied"`
	Output     map[string]any `json:"output"`
	Rollback   *RollbackHint  `json:"rollback,omitempty"`
	DurationMS int            `json:"duration_ms"`
}

// RollbackHint is how a call is inverted (SPEC-TYPES §3.8).
type RollbackHint struct {
	Supported bool           `json:"supported"`
	Module    string         `json:"module"`
	Args      map[string]any `json:"args"`
}

// VerifyResult is a module's immediate, local proof (SPEC-TYPES §3.8).
type VerifyResult struct {
	OK       bool           `json:"ok"`
	Method   string         `json:"method"`
	Detail   map[string]any `json:"detail"`
	Evidence []DiffEntry    `json:"evidence"`
}

// Module is the whole module SDK, frozen at v1 (SPEC-TYPES §3.8, SPEC-06 §2.1).
type Module interface {
	Descriptor() Descriptor
	Check(ctx context.Context, args map[string]any) (Diff, error)
	Apply(ctx context.Context, args map[string]any) (Result, error)
	Verify(ctx context.Context, args map[string]any) (VerifyResult, error)
}

// ToolCall is the audit record of one call (SPEC-TYPES §3.8).
type ToolCall struct {
	ID        string         `json:"id"`
	Module    string         `json:"module"`
	Args      map[string]any `json:"args"`
	Mode      string         `json:"mode"`
	Stage     []CallStage    `json:"stage"`
	CheckDiff *Diff          `json:"check_diff,omitempty"`
	Result    *Result        `json:"result,omitempty"`
	Verify    *VerifyResult  `json:"verify,omitempty"`
	Rolled    bool           `json:"rolled_back"`
	ErrorCode string         `json:"error_code"`
	LatencyMS int            `json:"latency_ms"`
	Inc       string         `json:"inc"`
	Actor     Actor          `json:"actor"`
}

// CallStage is one stage's audit entry (SPEC-TYPES §3.8).
type CallStage struct {
	Stage  string `json:"stage"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	MS     int    `json:"ms"`
}

// PlayTask is one task of a play (SPEC-TYPES §3.8).
type PlayTask struct {
	Name     string         `json:"name"`
	Tool     string         `json:"tool"`
	Args     map[string]any `json:"args"`
	When     string         `json:"when"`
	Register string         `json:"register"`
	Retries  int            `json:"retries"`
	OnFail   string         `json:"on_fail"`
}

// Play is a play as data (SPEC-TYPES §3.8, SPEC-06 §3.7).
type Play struct {
	Name      string     `json:"name"`
	Version   int        `json:"version"`
	Tasks     []PlayTask `json:"tasks"`
	CheckMode bool       `json:"check_mode"`
	MaxRuns   int        `json:"max_runs"`
	Source    string     `json:"source"`
}

// DoNotTouch is the merged deny set (SPEC-TYPES §3.8, SPEC-06 §3.6).
type DoNotTouch struct {
	Paths  []string `json:"paths"`
	Units  []string `json:"units"`
	Scopes []string `json:"scopes"`
}

// ToolCallRequest is the one input of Registry.Call (SPEC-TYPES §3.15.5).
type ToolCallRequest struct {
	Module  string         `json:"module"`
	Args    map[string]any `json:"args"`
	Mode    string         `json:"mode"`
	IdemKey string         `json:"idem_key"`
	Grants  []string       `json:"grants"`
	Source  string         `json:"source"`
	Inc     string         `json:"inc"`
	Rule    string         `json:"rule"`
	// Sig is part of the runner's bound base (SPEC-06 §2.2 names it) because a
	// play_run record is a sig-required spine kind (SPEC-01 §3.1).
	Sig       string `json:"sig"`
	Actor     Actor  `json:"actor"`
	DeadlineS int    `json:"deadline_s"`
}

// PlayRun is the run-level audit record (SPEC-TYPES §3.15.5).
type PlayRun struct {
	ID        string    `json:"id"`
	Play      string    `json:"play"`
	PlayVer   int       `json:"play_version"`
	Inc       string    `json:"inc"`
	Sig       string    `json:"sig"`
	Source    string    `json:"source"`
	Mode      string    `json:"mode"`
	StartedTS string    `json:"started_ts"`
	EndedTS   string    `json:"ended_ts"`
	Outcome   string    `json:"outcome"`
	Changed   bool      `json:"changed"`
	Tasks     []TaskRun `json:"tasks"`
}

// Play-run outcomes (SPEC-06 §3.1).
const (
	OutcomeDrafted    = "drafted"
	OutcomeCheckOnly  = "check_only"
	OutcomeApplied    = "applied"
	OutcomeFailed     = "failed"
	OutcomeRolledBack = "rolled_back"
	OutcomeParked     = "parked"
)

// TaskRun is one task's line in a PlayRun (SPEC-TYPES §3.15.5).
type TaskRun struct {
	Index      int            `json:"index"`
	Name       string         `json:"name"`
	Tool       string         `json:"tool"`
	Status     string         `json:"status"` // ok | changed | skipped | failed | refused
	SkippedBy  string         `json:"skipped_by"`
	Registers  map[string]any `json:"registers"`
	ToolCallID string         `json:"tool_call_id"`
	ErrorCode  string         `json:"error_code"`
	MS         int            `json:"ms"`
}
