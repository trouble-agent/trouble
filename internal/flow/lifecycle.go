package flow

// lifecycle.go — boot reconcile and the registration probe cadence (SPEC-08
// §3.5, §3.8).
//
// Reconcile compares the in-memory spawn registry with the router's worktree
// registry and records one `spawn` action per finding. It NEVER deletes a
// directory trouble does not own: the repo's owner prunes, trouble asks.

import (
	"context"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// Probe refreshes the registration proof of every configured project (§3.5).
// A state change is edge-triggered: one record per change, never one per event.
func (f *Flow) Probe(ctx context.Context) error {
	f.mu.Lock()
	names := make([]string, 0, len(f.cfg.Projects))
	for name := range f.cfg.Projects {
		names = append(names, name)
	}
	f.mu.Unlock()
	for _, name := range names {
		p, ok := f.projectByName(name)
		if !ok {
			continue
		}
		res := f.probeProject(ctx, p)
		f.mu.Lock()
		prev := f.cfg.Projects[name]
		changed := prev.Registered != res.Registered || prev.Reason != res.Reason ||
			prev.Ticked != res.Ticked || prev.LastProbeTS == ""
		f.cfg.Projects[name] = res
		f.probeTS = f.clock().Now()
		f.mu.Unlock()
		if !changed {
			continue
		}
		decision := "failed"
		if res.Registered {
			decision = "created"
		}
		f.record(ctx, types.Incident{}, map[string]any{
			"stage": "gate", "decision": decision, "project": res.Name,
			"board_path": res.BoardPath, "reason": res.Reason,
			"registered": res.Registered, "ticked": res.Ticked,
			"error_code": registrationCode(res),
		})
	}
	return nil
}

// registrationCode maps a failed proof onto TROUBLE-FLOW-018.
func registrationCode(p types.FlowProject) string {
	if p.Registered {
		return ""
	}
	return string(types.CodeFlow018)
}

// ProbeDue reports whether the probe cadence has elapsed.
func (f *Flow) ProbeDue() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	every := f.cfg.RegistrationProbeEvery.Std()
	if every <= 0 {
		every = 5 * time.Minute
	}
	return f.probeTS.IsZero() || f.clock().Now().Sub(f.probeTS) >= every
}

// Reconcile is the boot pass (§3.8): it re-appends a missing task_created event
// exactly once, records a reaped worktree, and adopts the durable queue. Every
// action is one `spawn` (or `flow`) record.
func (f *Flow) Reconcile(ctx context.Context) error {
	f.mu.Lock()
	queue := append([]types.SpawnRequest(nil), f.queue...)
	spawns := make([]types.SpawnRequest, 0, len(f.spawnSeq))
	for _, sp := range f.spawnSeq {
		spawns = append(spawns, sp)
	}
	boards := make(map[string]*boardIndex, len(f.boards))
	for dir, ix := range f.boards {
		boards[dir] = ix
	}
	f.mu.Unlock()

	// (a) A task row without its task_created event is re-appended once.
	for dir, ix := range boards {
		if ix.nonStrict {
			f.record(ctx, types.Incident{}, map[string]any{
				"stage": "file", "decision": "failed", "board_path": dir,
				"reason": "non_strict_board", "error_code": string(types.CodeFlow002),
			})
			continue
		}
		for id, row := range ix.rows {
			if ix.hasEvent(id, types.EvTaskCreated, "") {
				continue
			}
			ev := types.BoardEvent{
				Type: types.EvTaskCreated, TaskID: id, TS: types.FormatUTC(f.clock().Now()),
				Actor: f.actorName(), Detail: map[string]any{"sig": row.Sig, "inc": row.Inc, "reconciled": true},
			}
			if _, err := ix.appendEvent(ctx, ev, f.cfg.ValidateCmd); err != nil {
				f.record(ctx, types.Incident{ID: row.Inc, Sig: row.Sig}, map[string]any{
					"stage": "file", "decision": "failed", "task_id": id,
					"reason": "event_reappend_failed", "error_code": string(types.CodeFlow002),
				})
			}
		}
	}

	// (b) A terminal spawn whose directory is gone is prunable; one that is
	// still leased is adopted. trouble never deletes anything itself.
	for _, sp := range spawns {
		switch sp.State {
		case types.SpawnLeased, types.SpawnAccepted:
			present, err := f.worktreePresent(ctx, sp)
			if err == nil && present {
				f.recordSpawn(ctx, sp, "renew", sp.State, map[string]any{"reason": "boot_adopted"})
				continue
			}
			f.recordSpawn(ctx, sp, "reap", sp.State, map[string]any{
				"reason": "worktree_missing", "worktree": sp.Worktree, "repo": sp.Repo,
			})
		case types.SpawnPromoted, types.SpawnDiscarded:
			f.recordSpawn(ctx, sp, "reap", sp.State, map[string]any{"reason": "terminal"})
		}
	}

	// (c) The durable queue is adopted: a pending spawn keeps its place.
	for _, sp := range queue {
		if sp.State != types.SpawnPending {
			continue
		}
		f.recordSpawn(ctx, sp, "reap", types.SpawnPending, map[string]any{
			"reason": "queue_adopted", "attempts": sp.Attempts,
			"next_try_ts": nextAttemptTS(sp.Attempts, f.clock().Now()),
		})
	}
	return nil
}
