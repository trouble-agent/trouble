package flow

// spawn.go — the spawn state machine, the promotion decision and the dashboard
// read model (SPEC-08 §3.9, §3.11, §3.12) plus the budget measurement (§3.9).
//
// The state machine:
//
//	requested ──ack──► accepted ──worktree──► leased ──► released
//	    │                  ▲
//	    └──no ack──► spawn_pending ──retry──► accepted
//	                       │
//	                       └──budget exhausted──► failed (the durable queue holds it)
//
// A spawn failure is never silent: spawn_pending persists, the queue holds the
// entry, and the 60s trigger→spawn budget is recorded on every spawn.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// budgetMS is the AC-21 trigger→spawn budget: 60 000 ms, measured per spawn.
const budgetMS = 60000

// attemptBackoff is the bounded retry schedule of §3.9 (5s/15s/45s/2m/5m).
var attemptBackoff = []time.Duration{5 * time.Second, 15 * time.Second, 45 * time.Second, 2 * time.Minute, 5 * time.Minute}

// nextAttemptTS renders when a spawn may be retried.
func nextAttemptTS(attempts int, now time.Time) string {
	if attempts <= 0 {
		return types.FormatUTC(now)
	}
	i := attempts - 1
	if i >= len(attemptBackoff) {
		i = len(attemptBackoff) - 1
	}
	return types.FormatUTC(now.Add(attemptBackoff[i]))
}

// trigMS measures trigger → spawn from the request's own timestamp.
func trigMS(sp types.SpawnRequest, now time.Time) int64 {
	start, err := types.ParseUTC(sp.RequestedTS)
	if err != nil {
		return 0
	}
	ms := now.Sub(start).Milliseconds()
	if ms < 0 {
		return 0
	}
	return ms
}

// leaseIDOf renders a lease's identity for a record.
func leaseIDOf(l types.HotfixLease) string {
	return fmt.Sprintf("lease_%s", l.GrantedTS)
}

// briefSHA256 pins the brief's bytes so a reviewer can prove what the foreman
// was told (§3.13).
func briefSHA256(brief string) string {
	sum := sha256.Sum256([]byte(brief))
	return hex.EncodeToString(sum[:])
}

// Promote records the post-verify decision (§3.12). Merging is not trouble's
// job: the worktree is merged through its own PR by the repo's machinery under
// the allow_merge gate, and trouble records the url.
func (f *Flow) Promote(ctx context.Context, spawnID string, decision string) (types.Promotion, error) {
	f.mu.Lock()
	sp, ok := f.spawnSeq[spawnID]
	f.mu.Unlock()
	if !ok {
		return types.Promotion{}, &flowError{Code: types.CodeFlow010, Msg: "unknown spawn id " + spawnID}
	}
	ev := f.evidenceFor(sp)
	p := types.Promotion{
		Inc: sp.Inc, TaskID: sp.TaskID, VerifyEvidence: ev,
		Decision: types.PromotePending, PRURL: f.prURL(sp),
		Worktree: sp.Worktree, DecidedTS: types.FormatUTC(f.clock().Now()),
	}
	if ev.Result != types.VerifyPassed && decision != types.PromoteDiscard && decision != "discard" {
		// No promotion without a passed tuple (§7): a failed window is
		// TROUBLE-FLOW-015, and only the discard path may consume it.
		return p, &flowError{Code: types.CodeFlow015,
			Msg: fmt.Sprintf("verify window has not passed for %s (result=%q)", sp.TaskID, ev.Result)}
	}
	if ev.Result != types.VerifyPassed {
		// The discard half of a failed window: recorded with 015, never promoted.
		p.Decision, p.DecidedBy = types.PromoteDiscard, f.actorName()
		f.record(ctx, types.Incident{ID: sp.Inc, Sig: sp.Sig}, map[string]any{
			"stage": "promote", "decision": types.PromoteDiscard, "task_id": sp.TaskID,
			"repo": sp.Repo, "worktree": sp.Worktree, "verify_result": string(ev.Result),
			"error_code": string(types.CodeFlow015),
		})
		sp.State = types.SpawnDiscarded
		f.rememberSpawn(sp)
		f.recordSpawn(ctx, sp, "promote", types.SpawnDiscarded, map[string]any{
			"decision": types.PromoteDiscard, "verify_result": string(ev.Result),
			"error_code": string(types.CodeFlow015), "task_id": sp.TaskID,
		})
		f.emitRefusal(ctx, sp, ev, "verify_failed")
		f.releaseLease(sp.Sig, "verify_failed")
		return p, nil
	}
	switch decision {
	case "promote", types.PromoteDone:
		if f.cfg.Hotfix.Promote == types.FlowPromoteAuto && !f.gates.AllowPromote {
			// Denied by policy outside `full`: the tuple is stored, the decision
			// stays pending_human (§3.12).
			f.record(ctx, types.Incident{ID: sp.Inc, Sig: sp.Sig}, map[string]any{
				"stage": "promote", "decision": types.PromotePending, "reason": "policy_refused",
				"error_code": string(types.CodeFlow016), "task_id": sp.TaskID,
				"repo": sp.Repo, "worktree": sp.Worktree,
			})
			return p, &flowError{Code: types.CodeFlow016, Msg: "promotion denied by policy outside autonomy=full"}
		}
		p.Decision, p.DecidedBy = types.PromoteDone, f.actorName()
	case "discard", types.PromoteDiscard:
		p.Decision, p.DecidedBy = types.PromoteDiscard, f.actorName()
	default:
		p.Decision = types.PromotePending
	}
	if p.Decision == types.PromotePending {
		p.DecidedBy = ""
	}
	sp.State = map[string]string{
		types.PromoteDone: types.SpawnPromoted, types.PromoteDiscard: types.SpawnDiscarded,
	}[p.Decision]
	if sp.State != "" {
		f.rememberSpawn(sp)
	}
	f.recordSpawn(ctx, sp, "promote", nonEmpty(sp.State, sp.State), map[string]any{
		"decision": p.Decision, "decided_by": p.DecidedBy, "pr_url": p.PRURL,
		"verify_result": string(ev.Result), "task_id": sp.TaskID,
	})
	f.record(ctx, types.Incident{ID: sp.Inc, Sig: sp.Sig}, map[string]any{
		"stage": "promote", "decision": p.Decision, "task_id": sp.TaskID, "repo": sp.Repo,
		"worktree": sp.Worktree, "pr_url": p.PRURL, "decided_by": p.DecidedBy,
	})
	// The learning step is never skipped (AC-26): promote captures the remedy,
	// discard captures the refusal.
	switch p.Decision {
	case types.PromoteDone:
		f.emitSkill(ctx, sp, ev, p)
	case types.PromoteDiscard:
		f.emitRefusal(ctx, sp, ev, "verify_failed")
	}
	if p.Decision != types.PromotePending {
		f.releaseLease(sp.Sig, p.Decision)
	}
	return p, nil
}

// Rollback discards a spawn's patch through its owner (§3.11): trouble's
// contribution is the record, and a refusal is recorded, never claimed clean.
func (f *Flow) Rollback(ctx context.Context, spawnID string, reason string) (types.SpawnRequest, error) {
	f.mu.Lock()
	sp, ok := f.spawnSeq[spawnID]
	f.mu.Unlock()
	if !ok {
		return types.SpawnRequest{}, &flowError{Code: types.CodeFlow010, Msg: "unknown spawn id " + spawnID}
	}
	if sp.Worktree == "" && sp.State != types.SpawnLeased {
		// Nothing was ever leased: the honest answer is that there is nothing to
		// return, which is TROUBLE-FLOW-017 and not a clean rollback.
		f.recordSpawn(ctx, sp, "rollback", sp.State, map[string]any{
			"reason": "no_worktree_to_revert", "error_code": string(types.CodeFlow017),
		})
		return sp, &flowError{Code: types.CodeFlow017, Msg: "spawn " + sp.ID + " has no worktree to roll back"}
	}
	sp.State = types.SpawnDiscarded
	f.rememberSpawn(sp)
	f.recordSpawn(ctx, sp, "rollback", types.SpawnDiscarded, map[string]any{
		"reason": reason, "task_id": sp.TaskID, "repo": sp.Repo, "worktree": sp.Worktree,
	})
	f.releaseLease(sp.Sig, reason)
	f.mu.Lock()
	f.queue = removeSpawn(f.queue, sp.ID)
	f.mu.Unlock()
	f.emitRefusal(ctx, sp, f.evidenceFor(sp), reason)
	return sp, nil
}

// Timeline is the dashboard read model (AC-19): filed → foreman → patch →
// verify → promote, ordered, each step naming the records that prove it.
func (f *Flow) Timeline(ctx context.Context, inc string) ([]types.FlowTimelineStep, error) {
	f.mu.Lock()
	spawns := make([]types.SpawnRequest, 0, 4)
	for _, sp := range f.spawnSeq {
		if sp.Inc == inc || inc == "" {
			spawns = append(spawns, sp)
		}
	}
	leases := make([]types.HotfixLease, 0, 4)
	for _, l := range f.leases {
		leases = append(leases, l)
	}
	f.mu.Unlock()
	sort.Slice(spawns, func(i, j int) bool { return spawns[i].RequestedTS < spawns[j].RequestedTS })

	var steps []types.FlowTimelineStep
	for _, sp := range spawns {
		ev := f.evidenceFor(sp)
		steps = append(steps,
			types.FlowTimelineStep{
				Stage: "filed", TS: sp.RequestedTS, State: "done",
				Detail: "board row " + sp.TaskID + " (sig " + sp.Sig + ")",
				RecIDs: []string{sp.ID}, TaskID: sp.TaskID, SpawnID: sp.ID,
			},
			types.FlowTimelineStep{
				Stage: "foreman", TS: sp.RequestedTS, State: foremanStepState(sp),
				Detail: "router ref " + nonEmpty(sp.RouterRef, "(none)") + ", priority class " + sp.PriorityClass,
				RecIDs: []string{sp.ID}, TaskID: sp.TaskID, SpawnID: sp.ID, Worktree: sp.Worktree,
			},
			types.FlowTimelineStep{
				Stage: "patch", TS: sp.RequestedTS, State: patchStepState(sp),
				Detail: "patch confined to " + nonEmpty(sp.Worktree, "(no worktree)"),
				RecIDs: []string{sp.ID}, TaskID: sp.TaskID, SpawnID: sp.ID, Worktree: sp.Worktree,
			},
			types.FlowTimelineStep{
				Stage: "verify", TS: ev.TSWindowEnd, State: verifyStepState(ev),
				Detail: fmt.Sprintf("evidence tuple result=%s, window %.0fs, %d events observed",
					nonEmpty(string(ev.Result), "pending"), ev.WindowS, ev.EventsObserved),
				RecIDs: []string{sp.ID}, TaskID: sp.TaskID, SpawnID: sp.ID, Worktree: sp.Worktree,
			},
			types.FlowTimelineStep{
				Stage: "promote", TS: types.FormatUTC(f.clock().Now()),
				State:  promoteStepState(ev, sp, f.cfg.Hotfix.Promote),
				Detail: "promote=" + f.cfg.Hotfix.Promote + ", pr " + nonEmpty(sp.RouterRef, "(pending)"),
				RecIDs: []string{sp.ID}, TaskID: sp.TaskID, SpawnID: sp.ID,
				Worktree: sp.Worktree, PRURL: f.prURL(sp),
			},
		)
	}
	if len(steps) == 0 {
		for _, l := range leases {
			if inc != "" && l.Holder != inc {
				continue
			}
			steps = append(steps, types.FlowTimelineStep{
				Stage: "filed", State: "running",
				Detail: "sig " + l.Sig + " holds the hot-fix lease until " + l.ExpiresTS,
				TaskID: l.Holder, SpawnID: l.Holder,
			})
		}
	}
	return steps, nil
}

// foremanStepState is the foreman step's state token.
func foremanStepState(sp types.SpawnRequest) string {
	switch sp.State {
	case types.SpawnRequested:
		return "pending"
	case types.SpawnPending:
		return "running"
	case types.SpawnAccepted, types.SpawnLeased:
		return "done"
	case types.SpawnFailed:
		return "failed"
	}
	return "done"
}

// patchStepState is the patch step's state token.
func patchStepState(sp types.SpawnRequest) string {
	if sp.Worktree == "" {
		return "pending"
	}
	if sp.State == types.SpawnDiscarded {
		return "failed"
	}
	return "done"
}

// verifyStepState renders the verification step from the tuple.
func verifyStepState(ev types.Evidence) string {
	switch ev.Result {
	case types.VerifyPassed:
		return "done"
	case types.VerifyFailed:
		return "failed"
	case types.VerifyInvalid:
		return "denied"
	}
	return "pending"
}

// promoteStepState renders the promotion step's state (§3.12).
func promoteStepState(ev types.Evidence, sp types.SpawnRequest, mode string) string {
	if ev.Result != types.VerifyPassed {
		return "pending"
	}
	switch sp.State {
	case types.SpawnPromoted:
		return "done"
	case types.SpawnDiscarded:
		return "failed"
	}
	if mode == types.FlowPromoteAuto {
		return "pending"
	}
	return "pending"
}

// removeSpawn drops a spawn from the durable queue.
func removeSpawn(list []types.SpawnRequest, id string) []types.SpawnRequest {
	out := list[:0]
	for _, sp := range list {
		if sp.ID != id {
			out = append(out, sp)
		}
	}
	return out
}

// nonEmpty returns s, or the fallback when s is empty.
func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// briefFor assembles the foreman brief for a spawn (§3.14). Every field is
// scrubbed before it is written and every size is capped, so a flood cannot turn
// a brief into a ledger bomb.
func (f *Flow) briefFor(req types.SpawnRequest) string {
	brief := types.ForemanBrief{
		Sig: req.Sig, Inc: req.Inc, TaskID: req.TaskID, Title: f.titleFor(req),
		Severity: f.severityFor(req.Inc), Repo: req.Repo, Worktree: req.Worktree,
		BoardPath: f.cfg.BoardPath, EvidenceBundle: map[string]any{},
		ResearchBrief: map[string]any{}, FailingTests: []string{},
		ToolContract: types.ToolContractRegistryOnly, AllowedModules: []string{},
		DoNotTouch:   types.DoNotTouch{Paths: []string{}, Units: []string{}, Scopes: []string{}},
		VerifyWindow: f.cfg.Hotfix.VerifyWindow, Promote: f.cfg.Hotfix.Promote,
		Models: f.cfg.Hotfix.Models, Budget: map[string]any{
			"max_attempts": f.cfg.Hotfix.MaxAttempts, "wall_clock": "30m",
		},
		Constraints: []string{
			"no shell: registry tool calls only",
			"no git fetch/gc/prune/worktree mutation inside a foreman worktree",
			"PR-only: the worktree is merged through its own PR",
			"worktree-only: never touch the main checkout",
		},
		DaemonVersion: f.deps.Actors.Version, GitSHA: f.deps.Actors.GitSHA,
		CreatedTS: types.FormatUTC(f.clock().Now()),
	}
	if brief.Models == nil {
		brief.Models = map[string]string{}
	}
	body, err := json.Marshal(brief)
	if err != nil {
		return ""
	}
	return string(body)
}

// titleFor is the board title (≤200 chars, scrubbed).
func (f *Flow) titleFor(req types.SpawnRequest) string {
	t := fmt.Sprintf("hotfix: %s (%s)", req.Sig, req.TaskID)
	if len(t) > 200 {
		t = t[:200]
	}
	return f.scrub(types.TgBoard, t)
}

// prURL asks the repo's PR machinery for the url; trouble never invents one.
func (f *Flow) prURL(sp types.SpawnRequest) string {
	if f.deps.PRURL == nil {
		return ""
	}
	return f.deps.PRURL(sp)
}

// emitSkill hands the remedy to SPEC-11 (§3.12).
func (f *Flow) emitSkill(ctx context.Context, sp types.SpawnRequest, ev types.Evidence, p types.Promotion) {
	if f.deps.Skills == nil {
		return
	}
	_ = f.deps.Skills.Candidate(ctx, types.Incident{ID: sp.Inc, Sig: sp.Sig}, ev, sp.TaskID, sp.ResearchBriefID, p.PRURL)
}

// emitRefusal records the learning refusal when a fix is discarded (§3.12).
func (f *Flow) emitRefusal(ctx context.Context, sp types.SpawnRequest, ev types.Evidence, reason string) {
	if f.deps.Skills == nil {
		return
	}
	_ = f.deps.Skills.Refusal(ctx, types.Incident{ID: sp.Inc, Sig: sp.Sig}, ev, reason)
}
