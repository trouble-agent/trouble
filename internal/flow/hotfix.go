package flow

// hotfix.go — the hot-fix lane's gate chain and its resources (SPEC-08 §3.7,
// §3.8, §3.10).
//
// The gate chain is evaluated in order and the FIRST failure wins and is
// recorded: a lane that refuses for two reasons at once still produces one
// auditable reason. The one-fix-per-sig lease is host-wide and sig-keyed: two
// foremen on one sig are two patches for one bug and two chances to corrupt a
// repo.

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// hotfixGate runs the §3.7 gate chain. It returns the first failing gate's code
// and reason; an empty code means every gate passed.
func (f *Flow) hotfixGate(ctx context.Context, inc types.Incident, req types.SpawnRequest) (types.ErrorCode, string) {
	// 1. the host master gate
	if !f.cfg.Hotfix.Enabled {
		return types.CodeFlow006, "hotfix_disabled"
	}
	// 2. the autonomy gate is checked by the caller (it has its own record)
	// 3. severity ≥ min_severity
	if !severityAtLeast(inc.Severity, f.cfg.Hotfix.MinSeverity) {
		return types.CodeFlow008, fmt.Sprintf("severity %s below %s", inc.Severity, f.cfg.Hotfix.MinSeverity)
	}
	// 4. the rule is a hot-fix rule
	if !f.ruleHotfix(req) {
		return types.CodeFlow008, "rule_not_hotfix"
	}
	// 5. the repo is allowed AND is the project's repo
	if req.Repo == "" {
		return types.CodeFlow007, "repo_is_empty"
	}
	if !f.repoAllowed(req.Repo) {
		return types.CodeFlow007, "repo_not_in_allowed_repos"
	}
	if p, ok := f.projectFor(types.BoardRow{Repo: req.Repo, Sig: req.Sig, Inc: req.Inc}); ok && p.Repo != "" {
		if cleanPath(p.Repo) != cleanPath(req.Repo) {
			return types.CodeFlow007, "repo_differs_from_the_project"
		}
	}
	// 6. the registration proof holds
	if p, ok := f.projectFor(types.BoardRow{Repo: req.Repo, Sig: req.Sig, Inc: req.Inc}); ok {
		if !f.registrationHolds(p) {
			return types.CodeFlow018, "proof_stale_or_absent:" + p.Reason
		}
	}
	// 7. the one-fix-per-sig lease is free
	if held, holder := f.leaseHeld(req.Sig, req.Inc); held {
		return types.CodeFlow009, "lease_held_by_" + holder
	}
	// 8. a concurrency slot is free
	if f.concurrent() >= f.cfg.Hotfix.MaxConcurrent {
		return types.CodeFlow013, "max_concurrent_reached"
	}
	// 9. the disk gate
	if code, reason := f.diskGate(req.Repo); code != "" {
		return code, reason
	}
	// 10. the exempt list is a mode, not a refusal: it is applied after the gate.
	return "", ""
}

// ruleHotfix reports whether the incident's rule is hot-fix eligible. The rule
// comes from the wiring seam (SPEC-05 owns rules); an unwired seam means the
// rule is not hot-fix eligible, which is the conservative answer.
func (f *Flow) ruleHotfix(req types.SpawnRequest) bool {
	if f.deps.RuleHotfix == nil {
		return f.cfg.Hotfix.Enabled
	}
	return f.deps.RuleHotfix(req.Inc)
}

// repoAllowed checks the allowed_repos list with Clean + symlink equality.
func (f *Flow) repoAllowed(repo string) bool {
	if len(f.cfg.Hotfix.AllowedRepos) == 0 {
		return false
	}
	for _, a := range f.cfg.Hotfix.AllowedRepos {
		if cleanPath(a) == cleanPath(repo) {
			return true
		}
	}
	return false
}

// isExempt reports whether a repo is in worktree_exempt (huge checkouts).
func (f *Flow) isExempt(repo string) bool {
	for _, e := range f.cfg.Hotfix.WorktreeExempt {
		if cleanPath(e) == cleanPath(repo) {
			return true
		}
	}
	return false
}

// diskGate refuses a spawn when free space cannot hold three checkouts (§3.7
// check 9). An unwired disk probe passes the gate and records 0: a missing
// measurement must not be read as a full disk.
func (f *Flow) diskGate(repo string) (types.ErrorCode, string) {
	if f.deps.DiskFree == nil {
		return "", ""
	}
	free, err := f.deps.DiskFree(repo)
	if err != nil {
		return "", ""
	}
	need := f.cfg.Hotfix.MinFreeDiskGB
	if need < 3 {
		need = 3
	}
	if free < need {
		return types.CodeFlow012, fmt.Sprintf("free_disk_gb=%d < %d (and 3× the checkout)", free, need)
	}
	return "", ""
}

// leaseHeld reports whether another incident holds the sig's lease.
func (f *Flow) leaseHeld(sig, inc string) (bool, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.leases[sig]
	if !ok {
		return false, ""
	}
	if l.Holder == inc {
		return false, ""
	}
	// An expired lease is free: expiry releases it (§3.10).
	if ts, err := types.ParseUTC(l.ExpiresTS); err == nil && f.clock().Now().After(ts) {
		delete(f.leases, sig)
		return false, ""
	}
	return true, l.Holder
}

// grantLease grants the sig lease to this incident.
func (f *Flow) grantLease(inc types.Incident, req types.SpawnRequest) (types.HotfixLease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if l, ok := f.leases[req.Sig]; ok {
		if ts, err := types.ParseUTC(l.ExpiresTS); err == nil && f.clock().Now().Before(ts) && l.Holder != inc.ID {
			return types.HotfixLease{}, &flowError{Code: types.CodeFlow009, Msg: "one-fix-per-sig lease held by " + l.Holder}
		}
	}
	now := f.clock().Now()
	ttl := f.cfg.Hotfix.LeaseTTL.Std()
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	l := types.HotfixLease{
		Sig: req.Sig, Inc: inc.ID, Holder: inc.ID,
		GrantedTS: types.FormatUTC(now), ExpiresTS: types.FormatUTC(now.Add(ttl)),
	}
	f.leases[req.Sig] = l
	return l, nil
}

// releaseLease releases a sig's lease.
func (f *Flow) releaseLease(sig, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.leases[sig]
	if !ok {
		return
	}
	delete(f.leases, sig)
	_ = l
	_ = reason
}

// Lease returns a sig's lease (dashboard/explain).
func (f *Flow) Lease(sig string) (types.HotfixLease, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.leases[sig]
	return l, ok
}

// concurrent counts the live (leased) spawns.
func (f *Flow) concurrent() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, sp := range f.spawnSeq {
		if sp.State == types.SpawnLeased || sp.State == types.SpawnAccepted {
			n++
		}
	}
	return n
}

// lockRepo serializes worktree creation per repo (§3.8). The lock is in-process
// (one daemon owns one state root, SPEC-INDEX §6.4); it is held only around the
// spawn request, never around the foreman's own work.
func (f *Flow) lockRepo(ctx context.Context, repo string) (func(), error) {
	key := cleanPath(repo)
	f.mu.Lock()
	ch, ok := f.repoMu[key]
	if !ok {
		ch = make(chan struct{}, 1)
		f.repoMu[key] = ch
	}
	f.mu.Unlock()
	wait := f.cfg.Hotfix.MutexWait.Std()
	if wait <= 0 {
		wait = 5 * time.Second
	}
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-time.After(wait):
		return nil, &flowError{Code: types.CodeFlow013, Msg: fmt.Sprintf("worktree mutex for %s timed out after %s", repo, wait)}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// worktreePresent asks the repo's owner whether the worktree still exists.
// trouble never deletes a directory it does not own (§3.8).
func (f *Flow) worktreePresent(ctx context.Context, sp types.SpawnRequest) (bool, error) {
	if f.deps.Spawn == nil {
		return true, nil
	}
	return f.deps.Spawn.WorktreePresent(ctx, sp.Repo, sp.TaskID)
}

// worktreePath documents the adopted convention (§3.8): <repo>/<base>/<task-id>.
// trouble does NOT create it — the repo owner does — so this is only a helper for
// reporting and for the exempt path.
func (f *Flow) worktreePath(repo, taskID string) string {
	base := f.cfg.Hotfix.WorktreeBase
	if base == "" || strings.HasPrefix(base, "/") {
		return ""
	}
	return filepath.Join(repo, base, taskID)
}

// requestSpawn is the scheduler's admission call: the ONE place trouble asks for
// a worktree, and the request carries the priority class, the task id and the
// brief so the receiving side can reproduce the decision.
func (f *Flow) requestSpawn(ctx context.Context, req types.SpawnRequest) (string, string, error) {
	if f.deps.Spawn == nil {
		return "", "", &flowError{Code: types.CodeFlow010, Msg: "the spawn request seam is unwired"}
	}
	ref, worktree, err := f.deps.Spawn.RequestSpawn(ctx, req)
	if err != nil {
		return "", "", &flowError{Code: types.CodeFlow010, Msg: err.Error()}
	}
	return ref, worktree, nil
}

// rememberSpawn records a spawn in the two indexes (by id and by sig).
func (f *Flow) rememberSpawn(sp types.SpawnRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spawnSeq[sp.ID] = sp
	if sp.Sig != "" {
		f.bySig[sp.Sig] = sp
	}
}

// SpawnForSig returns the spawn a sig produced (the one-fix-per-sig invariant).
func (f *Flow) SpawnForSig(sig string) (types.SpawnRequest, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sp, ok := f.bySig[sig]
	return sp, ok
}
