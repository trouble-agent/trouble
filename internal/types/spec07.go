package types

// Types contributed by SPEC-07 (SPEC-TYPES §3.9 + §3.15.9): the Off-by-One lab's
// wire shapes plus the driver contract. ClassSlug, ResearchOutcome,
// ResearchPort and Subject are declared once in spec05.go; this file adds only
// what SPEC-07's own section declares outside that fold.
//
// Field names, order and JSON tags are verbatim from SPEC-TYPES and are
// contractual: internal/research maps them onto the lab's wire names with
// unexported structs (SPEC-07 §3.4), so the shared shape never drifts.

import "context"

// DiscoverRequest is the cache-lookup request (SPEC-TYPES §3.9). The lab's wire
// names are environment/language/version and are produced by the codec.
type DiscoverRequest struct {
	ProblemClass string `json:"problem_class"`
	Env          string `json:"env"`
	Lang         string `json:"lang"`
	Version      string `json:"version"`
}

// DiscoverResponse is the cache-lookup answer (SPEC-TYPES §3.9). Solution is
// the lab's `answer` object verbatim; Found=false is not absence (SPEC-07 §3.2
// requires the corpus grep as a second probe).
type DiscoverResponse struct {
	Found    bool           `json:"found"`
	Solution map[string]any `json:"solution"`
	CachedTS string         `json:"cached_ts"`
}

// SubmitRequest is the queue-on-a-miss request (SPEC-TYPES §3.9). Exactly these
// four top-level fields: the lab's decoder rejects every unknown field, so an
// extra top-level field is an avoidable 400 (SPEC-07 §2.3 B).
type SubmitRequest struct {
	ProblemClass string         `json:"problem_class"`
	Description  string         `json:"description"`
	Cadence      string         `json:"cadence"`
	Context      map[string]any `json:"context"`
}

// SubmitResponse is the submit answer (SPEC-TYPES §3.9). Duplicate is set by
// the codec from the HTTP 409 status, never parsed from a body.
type SubmitResponse struct {
	SubmissionID string `json:"submission_id"`
	Duplicate    bool   `json:"duplicate"`
}

// QueueStatus is one poll's answer (SPEC-TYPES §3.9). State is the lab's
// status/stage folded into queued | solving | solved | failed.
type QueueStatus struct {
	SubmissionID string         `json:"submission_id"`
	State        string         `json:"state"`
	Solution     map[string]any `json:"solution"`
	ElapsedS     float64        `json:"elapsed_s"`
}

// Queue-state vocabulary (SPEC-07 §2.3 C).
const (
	QueueQueued  = "queued"
	QueueSolving = "solving"
	QueueSolved  = "solved"
	QueueFailed  = "failed"
)

// LabHealth is the lab's liveness/capability surface (SPEC-07 §2.3 D): `GET
// /health` fills Status/Uptime, `GET /api/v1/stats` fills the counters and
// `GET /openapi.json` fills Paths for the capability probe. SPEC-07 §3.0 names
// this type `labHealth` and keeps it package-local; it is exported here because
// the driver contract below crosses a package boundary.
type LabHealth struct {
	Status          string   `json:"status"`           // health: "ok"
	Uptime          string   `json:"uptime"`           // health: "3h47m6s"
	TotalProblems   int64    `json:"total_problems"`   // stats, snake_case
	TotalAnswers    int64    `json:"total_answers"`    //
	VerifiedAnswers int64    `json:"verified_answers"` //
	QueueDepth      int64    `json:"queue_depth"`      //
	HitRate         float64  `json:"hit_rate"`         //
	Coverage        float64  `json:"coverage"`         //
	SolverAvailable bool     `json:"solver_available"` //
	Paths           []string `json:"paths,omitempty"`  // /openapi.json capability list
}

// ResearchDriver is the lab driver contract (SPEC-TYPES §3.15.9). internal/research
// ships the three implementations (off-by-one, none, webhook) and the ladder
// consumes the narrow ResearchPort.
type ResearchDriver interface {
	Name() string
	Discover(ctx context.Context, req DiscoverRequest) (DiscoverResponse, error)
	Submit(ctx context.Context, req SubmitRequest) (SubmitResponse, error)
	Poll(ctx context.Context, submissionID string) (QueueStatus, error)
	Health(ctx context.Context) (LabHealth, error)
	Stats(ctx context.Context) (LabHealth, error)
}

// Research driver names (SPEC-07 §2.2).
const (
	DriverOffByOne = "off-by-one"
	DriverNone     = "none"
	DriverWebhook  = "webhook"
)

// Pinned research degrade reasons (SPEC-07 §3.6, SPEC-TYPES §3.9).
const (
	ResReasonLabUnreachable    = "lab_unreachable"
	ResReasonSolverUnavailable = "solver_unavailable"
	ResReasonStrictDecoder     = "strict_decoder_reject"
	ResReasonPollTimeout       = "poll_timeout"
	ResSkipDriverNone          = "driver_none"
	ResSkipCapabilityMissing   = "driver_capability_missing"
	ResSkipClassUnknown        = "class_unknown"
	ResSkipBriefInvalid        = "brief_invalid"
	ResSkipBudgetExhausted     = "budget_exhausted"
	ResSkipKillSwitch          = "kill_switch"
)
