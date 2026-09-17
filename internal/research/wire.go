package research

// wire.go — the strict wire codec (SPEC-07 §3.4).
//
// The lab's field names are not trouble's: discover sends environment/language/
// version, submit takes exactly four top-level fields, and the answer arrives
// under `answer` with `status`/`stage` folded into one state. The unexported
// structs here are the only place those names appear, so a lab rename is one
// file and the shared types never drift.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// Digest is sha256 hex truncated to 16 chars: the brief/prompt digest and the
// idempotency key form (SPEC-07 §3.7, §3.10).
func Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

// wireDiscoverRequest is the discover body. include_related is always sent as
// false: the body is smaller and trouble nests what it needs under context.
type wireDiscoverRequest struct {
	ProblemClass   string `json:"problem_class"`
	Environment    string `json:"environment,omitempty"`
	Language       string `json:"language,omitempty"`
	Version        string `json:"version,omitempty"`
	IncludeRelated bool   `json:"include_related"`
}

// wireSubmitRequest is the submit body: exactly four top-level fields.
type wireSubmitRequest struct {
	ProblemClass string         `json:"problem_class"`
	Description  string         `json:"description"`
	Cadence      string         `json:"cadence"`
	Context      map[string]any `json:"context"`
}

// wireDiscoverResponse is the discover answer. The answer object is decoded as a
// raw map: the brief carries the lab's answer verbatim (§3.2), so a key trouble
// does not model (a solution's task list, an evidence blob) still reaches the
// brief and the ledger.
type wireDiscoverResponse struct {
	Found       bool           `json:"found"`
	Answer      map[string]any `json:"answer,omitempty"`
	Error       string         `json:"error,omitempty"`
	Message     string         `json:"message,omitempty"`
	IncludeRel  bool           `json:"include_related,omitempty"`
	ExistingSol int            `json:"existing_solutions,omitempty"`
}

// wireSubmitResponse is the submit answer (200/201) and the 503 body.
type wireSubmitResponse struct {
	SubmissionID      string `json:"submission_id,omitempty"`
	ProblemClass      string `json:"problem_class,omitempty"`
	Status            string `json:"status,omitempty"`
	Position          int    `json:"position,omitempty"`
	EstimatedTime     string `json:"estimated_time,omitempty"`
	ExistingSolutions int    `json:"existing_solutions,omitempty"`
	Error             string `json:"error,omitempty"`
	Message           string `json:"message,omitempty"`
}

// wireQueueStatus is one poll's answer.
type wireQueueStatus struct {
	SubmissionID  string         `json:"submission_id,omitempty"`
	Status        string         `json:"status,omitempty"`
	Stage         string         `json:"stage,omitempty"`
	Position      int            `json:"position,omitempty"`
	EstimatedTime string         `json:"estimated_time,omitempty"`
	StartedAt     string         `json:"started_at,omitempty"`
	CompletedAt   string         `json:"completed_at,omitempty"`
	Answer        map[string]any `json:"answer,omitempty"`
	Error         string         `json:"error,omitempty"`
	Message       string         `json:"message,omitempty"`
}

// wireLabError decodes the lab's error bodies (400/404/409/503).
type wireLabError struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// wireHealth decodes GET /health (root path; /api/v1/health is 404).
type wireHealth struct {
	Status string `json:"status"`
	Uptime string `json:"uptime"`
}

// wireStats decodes GET /api/v1/stats. The keys are snake_case and are decoded
// into a typed struct: a jq filter on `problems`/`answers` returns null.
type wireStats struct {
	TotalProblems   int64   `json:"total_problems"`
	TotalAnswers    int64   `json:"total_answers"`
	VerifiedAnswers int64   `json:"verified_answers"`
	QueueDepth      int64   `json:"queue_depth"`
	HitRate         float64 `json:"hit_rate"`
	Coverage        float64 `json:"coverage"`
	SolverAvailable bool    `json:"solver_available"`
}

// wireOpenAPI decodes the slice of /openapi.json the capability probe reads.
type wireOpenAPI struct {
	Paths map[string]json.RawMessage `json:"paths"`
}

// encodeDiscover renders the discover body. narrow adds the D2 fields; a
// sensor-sourced sig never sends them (§3.2).
func encodeDiscover(problemClass, env, lang, version string, narrow bool) ([]byte, error) {
	body := wireDiscoverRequest{ProblemClass: problemClass, IncludeRelated: false}
	if narrow {
		body.Environment, body.Language, body.Version = env, lang, version
	}
	return json.Marshal(body)
}

// encodeSubmit renders the submit body.
func encodeSubmit(req types.SubmitRequest) ([]byte, error) {
	return json.Marshal(wireSubmitRequest{
		ProblemClass: req.ProblemClass,
		Description:  req.Description,
		Cadence:      req.Cadence,
		Context:      req.Context,
	})
}

// isJSON reports whether a response may be decoded as JSON. The lab's SPA
// catch-all answers 200 text/html for an unknown path (measured), which is a
// protocol mismatch, never a decode panic.
func isJSON(h http.Header) bool {
	ct := h.Get("Content-Type")
	if ct == "" {
		return false
	}
	ct = strings.ToLower(ct)
	return strings.Contains(ct, "json")
}

// answerMap normalizes the lab's answer object: it is returned as decoded, so
// every key the lab sent survives into the brief.
func answerMap(a map[string]any) map[string]any {
	if a == nil {
		return nil
	}
	return a
}

// answerStatus reads the answer's status for the brief-acceptance rule (§3.2.4):
// only `verified` and `ci_passed` are accepted, and only when the recorded
// signature result is not "failed".
func answerStatus(a map[string]any) string {
	if a == nil {
		return ""
	}
	if s, ok := a["status"].(string); ok {
		return strings.ToLower(s)
	}
	return ""
}

// signatureFailed reports whether the answer's signatures say the result failed.
func signatureFailed(a map[string]any) bool {
	if a == nil {
		return false
	}
	sig, ok := a["signatures"].(map[string]any)
	if !ok {
		return false
	}
	r, ok := sig["result"].(string)
	return ok && strings.EqualFold(r, "failed")
}

// answerAccepted is the acceptance predicate shared by the discover and corpus
// paths (§3.2 step 4): status verified|ci_passed and signatures.result != failed.
func answerAccepted(a map[string]any) bool {
	st := answerStatus(a)
	if st != "verified" && st != "ci_passed" {
		return false
	}
	return !signatureFailed(a)
}

// canonicalJSON renders a value deterministically for digesting: encoding/json
// sorts map keys, so the bytes are stable across runs and Go versions.
func canonicalJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}
