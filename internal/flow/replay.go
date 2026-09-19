package flow

// replay.go — the flow-owned durable queue's write seam, drain loop and the
// honest refusal (SPEC-08 §3.9a, the §3.6 × SPEC-09 §3.6/§3.7 coupling).
//
// Before §3.9a the flow had no drain of its own: `Flow.Start` was Reconcile +
// Probe, the only remnant of a failed dispatch was the in-memory queue (≤256,
// drop-oldest), and the composition root handed the flow a sink whose replay
// walks the desk's configured driver names. A pending spawn therefore did not
// survive a restart in any posture. §3.9a gives the flow both halves — the store
// and the loop — and makes the record tell the truth about which one it got.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// spoolQueue is the flow-owned durable queue: the write seam every caller
// already knew (spoolSink) plus the replay side only its owner uses.
type spoolQueue interface {
	spoolSink
	Put(e types.SpoolEntry) ([]DropEvent, error)
	List() ([]types.SpoolEntry, error)
	Delete(id string) error
	Update(e types.SpoolEntry) error
	Count() int
}

// spoolCoupling names the two-contract seam in the record itself, so an operator
// reading `dispatch_state` learns WHY a dispatch is or is not durable instead of
// having to reconstruct the wiring.
const spoolCoupling = "SPEC-08 §3.6/§3.9 × SPEC-09 §3.6/§3.7"

// spoolPath is the §3.9a location, inside the SPEC-12 §3.2 state tree's own
// `spool/` subtree and in a subdirectory no other subsystem lists. It is exported
// because the composition root owns the state root (SPEC-12 §3.1).
func spoolPath(stateRoot string) string { return stateRoot + "/spool/flow/spawn" }

// SpoolPath is spoolPath for the composition root.
func SpoolPath(stateRoot string) string { return spoolPath(stateRoot) }

// spoolPut writes one durable spawn entry and reports whether it landed on a
// queue THIS subsystem replays. It never claims durability it does not have: a
// sink that is not a spoolQueue (the desk adapter) and a store that refused the
// write both answer false with a reason token, and the caller records that.
func (f *Flow) spoolPut(ctx context.Context, stage, reason string, req types.SpawnRequest, inc types.Incident) bool {
	if f.spool == nil {
		f.record(ctx, inc, map[string]any{
			"stage": stage, "decision": "failed", "dispatch_state": "unspooled",
			"reason": firstNonEmpty(reason, "spool_not_wired"), "task_id": req.TaskID,
			"idem_key": req.TaskID, "coupling": spoolCoupling,
			"error_code": string(types.CodeFlow005),
		})
		return false
	}
	body, err := json.Marshal(req)
	if err != nil {
		f.record(ctx, inc, map[string]any{
			"stage": stage, "decision": "failed", "dispatch_state": "unspooled",
			"reason": "payload_marshal", "task_id": req.TaskID, "idem_key": req.TaskID,
			"coupling": spoolCoupling, "error_code": string(types.CodeFlow005),
		})
		return false
	}
	now := f.clock().Now()
	entry := types.SpoolEntry{
		ID: types.NewID(types.PEv), TS: types.FormatUTC(now), Kind: types.SpoolSpawn,
		Payload: body, IdemKey: req.TaskID, NextTryTS: nextAttemptTS(1, now),
	}
	drops, err := f.spool.Put(entry)
	f.recordDrops(ctx, drops)
	if err != nil {
		f.record(ctx, inc, map[string]any{
			"stage": stage, "decision": "failed", "dispatch_state": "spool_failed",
			"reason": "spool_write: " + err.Error(), "task_id": req.TaskID, "idem_key": req.TaskID,
			"coupling": spoolCoupling, "error_code": string(types.CodeFlow005),
		})
		return false
	}
	// The entry is on disk AND a loop this subsystem owns drains it: this is the
	// one case where the record may claim durability.
	f.record(ctx, inc, map[string]any{
		"stage": stage, "decision": "dispatched", "dispatch_state": "spooled",
		"task_id": req.TaskID, "idem_key": req.TaskID, "router_ref": req.TaskID,
		"coupling": spoolCoupling,
	})
	return true
}

// recordDrops writes one `flow` record plus one `spawn` record per eviction, so
// a bounded queue costs evidence, never silence. `gap` is not this subsystem's
// kind to write (SPEC-INDEX §3.4).
func (f *Flow) recordDrops(ctx context.Context, drops []DropEvent) {
	for _, d := range drops {
		inc := types.Incident{ID: d.Inc, Sig: d.Sig}
		f.record(ctx, inc, map[string]any{
			"stage": "dispatch", "decision": "failed", "dispatch_state": "dropped",
			"reason": "spool_drop_" + d.Reason, "task_id": d.TaskID, "idem_key": d.TaskID,
			"drop_reason": d.Reason, "oldest_ts": d.TS, "drop_count": 1,
			"coupling": spoolCoupling, "error_code": string(types.CodeLifecycle015),
		})
		f.recordSpawn(ctx, types.SpawnRequest{TaskID: d.TaskID, Sig: d.Sig, Inc: d.Inc},
			"reap", types.SpawnFailed, map[string]any{
				"reason": "spool_drop_" + d.Reason, "attempts": d.Attempts,
				"error_code": string(types.CodeFlow011),
			})
	}
}

// ReplayDue reports whether the flow's queue holds an entry that is due now.
func (f *Flow) ReplayDue() bool {
	if f.spool == nil {
		return false
	}
	entries, err := f.spool.List()
	if err != nil || len(entries) == 0 {
		return false
	}
	now := f.clock().Now()
	due := types.FormatUTC(now)
	for _, e := range entries {
		if e.NextTryTS == "" || e.NextTryTS <= due {
			return true
		}
	}
	return false
}

// Replay drains the flow-owned queue (§3.9a). Order is (next_try_ts, ts, id); one
// in-flight dispatch per IdemKey, so a replay can never race the original
// dispatch or a second replay of the same entry; re-dispatch goes through the
// EXISTING DispatchSpawn with the SAME idem key, so the router's own idem_key
// dedup makes a replay idempotent. TTL and attempts exhaustion drop the entry
// with a ledger record; nothing leaves this loop silently.
func (f *Flow) Replay(ctx context.Context, budget int) (int, error) {
	if f.spool == nil {
		return 0, fmt.Errorf("%s: the flow's durable dispatch queue is not wired — a pending spawn cannot be replayed (%s)",
			types.CodeFlow005, spoolCoupling)
	}
	if budget <= 0 {
		budget = f.spoolBounds().ReplayBatch
	}
	entries, err := f.spool.List()
	if err != nil {
		return 0, err
	}
	now := f.clock().Now()
	replayed := 0
	for i := range entries {
		if replayed >= budget {
			break
		}
		e := entries[i]
		if ts, perr := types.ParseUTC(e.TS); perr == nil && now.Sub(ts) > f.spoolBounds().TTL {
			f.dropSpoolEntry(ctx, e, "ttl")
			continue
		}
		if e.NextTryTS != "" {
			if ts, perr := types.ParseUTC(e.NextTryTS); perr == nil && now.Before(ts) {
				continue
			}
		}
		var req types.SpawnRequest
		if err := json.Unmarshal(e.Payload, &req); err != nil {
			// A payload this build cannot decode is not a retryable failure: it
			// is a corrupt entry, and saying so beats looping on it forever.
			f.dropSpoolEntry(ctx, e, "corrupt")
			continue
		}
		if !f.claimIdem(e.IdemKey) {
			continue
		}
		_, _, derr := f.dispatchSpawn(ctx, req, false)
		f.releaseIdem(e.IdemKey)
		inc := types.Incident{ID: req.Inc, Sig: req.Sig}
		if derr != nil {
			e.Attempts++
			if e.Attempts >= f.spoolBounds().MaxAttempts {
				f.dropSpoolEntry(ctx, e, "attempts")
				continue
			}
			e.NextTryTS = nextAttemptTS(e.Attempts, f.clock().Now())
			_ = f.spool.Update(e)
			f.record(ctx, inc, map[string]any{
				"stage": "dispatch", "decision": "failed", "dispatch_state": "replay_failed",
				"reason": derr.Error(), "task_id": req.TaskID, "idem_key": e.IdemKey,
				"attempts": e.Attempts, "next_try_ts": e.NextTryTS,
				"coupling": spoolCoupling, "error_code": codeOfErr(derr),
			})
			continue
		}
		_ = f.spool.Delete(e.ID)
		f.record(ctx, inc, map[string]any{
			"stage": "dispatch", "decision": "dispatched", "dispatch_state": "replayed",
			"task_id": req.TaskID, "idem_key": e.IdemKey, "router_ref": req.TaskID,
			"attempts": e.Attempts + 1, "coupling": spoolCoupling,
		})
		f.recordSpawn(ctx, req, "spawn", types.SpawnLeased, map[string]any{
			"reason": "spool_replay", "attempts": e.Attempts + 1,
		})
		replayed++
	}
	return replayed, nil
}

// dropSpoolEntry removes an entry whose budget is spent and writes both halves
// of the loss: a `flow` record with the cause and a `spawn` record naming the
// state, each carrying incident + sig + task_id.
func (f *Flow) dropSpoolEntry(ctx context.Context, e types.SpoolEntry, reason string) {
	var req types.SpawnRequest
	_ = json.Unmarshal(e.Payload, &req)
	_ = f.spool.Delete(e.ID)
	d := DropEvent{ID: e.ID, TS: e.TS, Reason: reason, TaskID: firstNonEmpty(req.TaskID, e.IdemKey),
		Sig: req.Sig, Inc: req.Inc, Attempts: e.Attempts, EstLost: -1}
	f.recordDrops(ctx, []DropEvent{d})
}

// claimIdem takes the single-flight slot for one idem key. One in-flight
// dispatch per key is what keeps a replay from producing a second spawn for work
// the first attempt already placed.
func (f *Flow) claimIdem(key string) bool {
	if key == "" {
		return true
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.spoolInflight[key] {
		return false
	}
	f.spoolInflight[key] = true
	return true
}

func (f *Flow) releaseIdem(key string) {
	if key == "" {
		return
	}
	f.mu.Lock()
	delete(f.spoolInflight, key)
	f.mu.Unlock()
}

// spoolBounds is the resolved bound set; a Flow built before SetDeps still
// answers with the defaults.
func (f *Flow) spoolBounds() SpoolBounds { return f.bounds.WithDefaults() }

// Run is the flow's own background loop (§3.9a): the durable drain and the
// registration probe cadence. Before §3.9a the daemon's comment claimed this loop
// existed while `Flow.Start` ran neither half — Run is what makes the claim true.
// It returns when ctx is done.
func (f *Flow) Run(ctx context.Context) {
	b := f.spoolBounds()
	drain := time.NewTicker(b.ReplayEvery)
	defer drain.Stop()
	probe := time.NewTicker(f.probeInterval())
	defer probe.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-drain.C:
			if f.spool == nil {
				continue
			}
			if _, err := f.Replay(ctx, 0); err != nil {
				f.record(ctx, types.Incident{}, map[string]any{
					"stage": "dispatch", "decision": "failed", "reason": "replay_loop: " + err.Error(),
					"coupling": spoolCoupling, "error_code": string(types.CodeFlow005),
				})
			}
		case <-probe.C:
			if f.ProbeDue() {
				_ = f.Probe(ctx)
			}
		}
	}
}

// probeInterval is the registration probe cadence, defaulted so a half-wired
// instance still probes.
func (f *Flow) probeInterval() time.Duration {
	if d := f.cfg.RegistrationProbeEvery.Std(); d > 0 {
		return d
	}
	return 5 * time.Minute
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
