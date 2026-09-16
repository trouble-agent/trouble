package flow

// driver.go — the two-implementation driver contract and this package's view of
// its collaborators (SPEC-08 §2, §3.6).
//
// `FlowDriver` (the filing contract) and its request/result types live in
// SPEC-TYPES, once; boardjsonl.go and router.go are its two implementations.
// The collaborator seams below are package-private by design: they are this
// package's view of SPEC-01/09/11/12, so no shared type is minted for a seam and
// the one-way dependency direction (subsystems → internal/types) holds.

import (
	"context"
	"errors"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// recordSink is SPEC-01's ledger (the only writer).
type recordSink interface {
	Append(ctx context.Context, d types.RecordDraft) (types.Record, error)
}

// issueDesk is SPEC-09.
type issueDesk interface {
	EnsureBySig(ctx context.Context, req types.EnsureBySigRequest) (types.EnsureBySigResponse, error)
	Comment(ctx context.Context, ref types.IssueRef, body string) (types.IssueRef, error)
}

// spawnRequester is the scheduler's admission path: `router_spawn` (§3.9).
// Worktree is REPORTED by the router, never computed here: the component that
// owns the repo performs `git worktree add`.
type spawnRequester interface {
	RequestSpawn(ctx context.Context, req types.SpawnRequest) (routerRef string, worktree string, err error)
	WorktreePresent(ctx context.Context, repo, taskID string) (bool, error)
}

// skillSink is SPEC-11's candidate/refusal hand-off (§3.12).
type skillSink interface {
	Candidate(ctx context.Context, inc types.Incident, ev types.Evidence, taskID, researchID, prURL string) error
	Refusal(ctx context.Context, inc types.Incident, ev types.Evidence, reason string) error
}

// spoolSink is SPEC-12's durable queue.
type spoolSink interface {
	Enqueue(ctx context.Context, e types.SpoolEntry) error
}

// record writes one `flow` record (§3.13). Every field is best-effort: a nil
// sink means the subsystem is running unwired, which tests do deliberately.
func (f *Flow) record(ctx context.Context, inc types.Incident, payload map[string]any) {
	if f.deps.Recorder == nil {
		return
	}
	sig := payload["sig"]
	draft := types.RecordDraft{
		Kind: types.KFlow, Sig: asString(sig, inc.Sig), Inc: inc.ID,
		Origin: f.originOf(inc.Sig), Actor: f.actor(), Payload: payload,
	}
	_, _ = f.deps.Recorder.Append(ctx, draft)
}

// recordSpawn writes one `spawn` record per state change (§3.13).
func (f *Flow) recordSpawn(ctx context.Context, sp types.SpawnRequest, stage, state string, extra map[string]any) {
	if f.deps.Recorder == nil {
		return
	}
	payload := map[string]any{
		"stage": stage, "state": state, "spawn_id": sp.ID, "task_id": sp.TaskID,
		"repo": sp.Repo, "worktree": sp.Worktree, "priority_class": sp.PriorityClass,
		"router_ref": sp.RouterRef, "attempts": sp.Attempts,
	}
	for k, v := range extra {
		payload[k] = v
	}
	draft := types.RecordDraft{
		Kind: types.KSpawn, Sig: sp.Sig, Inc: sp.Inc,
		Origin: f.originOf(sp.Sig), Actor: f.actor(), Payload: payload,
	}
	_, _ = f.deps.Recorder.Append(ctx, draft)
}

// originOf stamps the provenance every record carries (a T5 enabler).
func (f *Flow) originOf(sig string) types.Origin {
	return types.Origin{HostID: f.deps.Actors.HostID, HubID: "", Source: "flow:" + sig}
}

// actor is the daemon identity on every record this package writes.
func (f *Flow) actor() types.Actor {
	a := f.deps.Actors
	id := a.ID
	if id == "" {
		id = "troubled"
	}
	return types.Actor{
		Kind: types.ActorDaemon, ID: id, Version: a.Version,
		GitSHA: a.GitSHA, BuildTime: a.BuildTS,
	}
}

// actorName renders the board-event actor string ("troubled@0.1.0").
func (f *Flow) actorName() string {
	v := f.deps.Actors.Version
	if v == "" {
		v = "0.1.0"
	}
	return "troubled@" + v
}

// scrub runs a body through SPEC-02 when it is wired, and is identity otherwise.
func (f *Flow) scrub(target types.ScrubTarget, s string) string {
	if f.deps.Scrub == nil || s == "" {
		return s
	}
	out := f.deps.Scrub(target, []byte(s))
	if out == nil {
		return s
	}
	return string(out)
}

// severityFor reads the incident's severity from the wiring seam, defaulting to
// the hot-fix threshold's own severity (a missing seam must not silently floor
// every gate).
func (f *Flow) severityFor(inc string) types.Severity {
	if f.deps.Severity != nil {
		if s := f.deps.Severity(inc); s != "" {
			return s
		}
	}
	return f.cfg.Hotfix.MinSeverity
}

// evidenceFor reads SPEC-05's evidence tuple for a spawn (§3.11).
func (f *Flow) evidenceFor(sp types.SpawnRequest) types.Evidence {
	if f.deps.Evidence != nil {
		return f.deps.Evidence(sp.ID)
	}
	return types.Evidence{Result: ""}
}

// error helpers ---------------------------------------------------------------

// codeOfErr renders an error's TROUBLE code for a record payload.
func codeOfErr(err error) string {
	if err == nil {
		return ""
	}
	var fe *flowError
	if errors.As(err, &fe) {
		return string(fe.Code)
	}
	return string(types.CodeFlow002)
}

// flowError carries a SPEC-08 §5 code with its message.
type flowError struct {
	Code      types.ErrorCode
	Msg       string
	Retryable bool
}

func (e *flowError) Error() string { return string(e.Code) + ": " + e.Msg }

// decisionOf maps a filing result onto the `flow` payload's decision token
// (§3.13: created | commented | dispatched | skipped | drafted | failed).
func decisionOf(err error, res types.FileTaskResult) string {
	switch {
	case err != nil:
		return "failed"
	case res.DupOf != "":
		return "commented"
	case res.Wrote:
		return "created"
	case res.TaskID != "" && res.Valid:
		// A task-router filing: the router owns the append, so trouble
		// dispatched the row rather than writing it.
		return "dispatched"
	default:
		return "skipped"
	}
}

// sinceMS renders a latency in milliseconds.
func sinceMS(a, b time.Time) int {
	d := b.Sub(a).Milliseconds()
	if d < 0 {
		return 0
	}
	return int(d)
}

// asString coerces a payload value to a string.
func asString(v any, def string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return def
}

// defaultSlice normalizes a nil slice to an empty one: a board row must carry
// [] and never null (the fleet's row contract).
func defaultSlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
