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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
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

// sealedDispatch is the §3.9b entry payload: the §3.6 wire body RENDERED ONCE, at
// the moment the entry was written, plus the identity a replay record needs.
//
// The `sealed` marker is also the format discriminator: an entry written by a
// build before §3.9b carries the bare marshalled `SpawnRequest` and has no such
// field, which is exactly why a replay of it must be RECORDED as a re-derivation
// instead of passing for a delivered-as-written payload.
type sealedDispatch struct {
	Sealed  bool            `json:"sealed"`
	Source  string          `json:"source"` // dispatch | spawn
	TaskID  string          `json:"task_id"`
	Inc     string          `json:"inc"`
	Sig     string          `json:"sig"`
	Payload dispatchPayload `json:"payload"`
}

// Entry origins. A record must say which one a replayed dispatch had.
const (
	sourceDispatch = "dispatch"
	sourceSpawn    = "spawn"

	payloadSealed    = "sealed"
	payloadRederived = "rederived"
	payloadCorrupt   = "corrupt"
)

// rederivedFields names what a pre-§3.9b entry's payload is RE-DERIVED from
// live state at replay time. It is recorded on every such replay so a reader of
// the ledger can tell a delivered-as-written payload from a re-derived one.
var rederivedFields = []string{
	"board_path", "title", "priority", "complexity", "capability_tags",
	"severity", "host_id", "submitted_ts",
}

// comparedFields is the secondary-field set a sealed delivery is diffed against a
// live re-derivation in the record: identity (`task_id`/`idem_key`) and the
// incident/repo keys are excluded because they are stable by construction, and
// `submitted_ts` is excluded because the live render always stamps NOW — it is
// carried in `delivered` instead, where it is information rather than noise.
var comparedFields = []string{
	"board_path", "title", "priority", "complexity", "capability_tags", "severity", "host_id",
}

// sealDispatch pins one rendered payload into an entry payload.
func sealDispatch(p dispatchPayload, source string) sealedDispatch {
	return sealedDispatch{
		Sealed: true, Source: source, TaskID: p.TaskID, Inc: p.Inc, Sig: p.Sig, Payload: p,
	}
}

// sealSpawn renders the payload a spawn request WOULD be dispatched with and seals
// it. The spawn path parks a request before any attempt has run, so its payload is
// rendered here — once — and a replay delivers this render, not a new one.
func (f *Flow) sealSpawn(req types.SpawnRequest) sealedDispatch {
	return sealDispatch(f.wirePayload(f.spawnRow(req), types.SevHigh), sourceSpawn)
}

// decodeSpoolPayload reads one entry payload into its three outcomes: a sealed
// entry, a pre-§3.9b entry (decodable, therefore re-derived with the divergence
// recorded rather than dropped as corrupt), and a payload this build cannot read.
//
// The `sealed` marker decides which shape an entry IS; a sealed envelope that has
// lost its identity is corrupt, and is refused rather than quietly re-derived as if
// it were a request — a silent fallback there is the mutation this sub-§ removes.
func decodeSpoolPayload(payload []byte) (sealedDispatch, types.SpawnRequest, string) {
	var sd sealedDispatch
	if err := json.Unmarshal(payload, &sd); err == nil && sd.Sealed {
		if sd.Payload.TaskID == "" || sd.Payload.IdemKey == "" {
			return sealedDispatch{}, types.SpawnRequest{}, payloadCorrupt
		}
		return sd, types.SpawnRequest{}, payloadSealed
	}
	var req types.SpawnRequest
	if err := json.Unmarshal(payload, &req); err == nil {
		return sealedDispatch{}, req, payloadRederived
	}
	return sealedDispatch{}, types.SpawnRequest{}, payloadCorrupt
}

// spawnFromPayload is the `spawn` record half of a replayed dispatch: the record
// kind takes a SpawnRequest, and the sealed payload carries every field it needs.
func spawnFromPayload(p dispatchPayload) types.SpawnRequest {
	return types.SpawnRequest{
		TaskID: p.TaskID, Sig: p.Sig, Inc: p.Inc, Repo: p.Repo,
		PriorityClass: p.Priority, RequestedTS: p.SubmittedTS,
	}
}

// liveRederivation renders the §3.6 payload the PRE-§3.9b rule would produce
// NOW: the row rebuilt from the entry's request fields, dispatched at SevHigh —
// exactly the body a legacy entry's replay delivers. It is never delivered; it
// exists so the record can show what a re-derivation would have sent in place of
// the sealed payload, which is what makes the divergence visible instead of silent.
func (f *Flow) liveRederivation(p dispatchPayload) dispatchPayload {
	return f.wirePayload(f.spawnRow(spawnFromPayload(p)), types.SevHigh)
}

// payloadFieldSet renders the §3.6 payload's secondary fields as a flat map a
// ledger reader can diff — everything except the identity the router dedups on.
func payloadFieldSet(p dispatchPayload) map[string]string {
	return map[string]string{
		"board_path": p.BoardPath, "title": p.Title, "priority": p.Priority,
		"complexity": p.Complexity, "capability_tags": strings.Join(p.CapabilityTag, ","),
		"severity": p.Severity, "host_id": p.HostID, "submitted_ts": p.SubmittedTS,
	}
}

// payloadSHA256 pins the delivered bytes so a record can be compared with the
// entry that produced it.
func payloadSHA256(p dispatchPayload) string {
	b, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// recordSealedDelivery writes the delivered field set and, when the live inputs
// have moved since the entry was written, what a re-derivation WOULD have produced.
// The divergence is visible in the ledger instead of silent in the wire body.
func recordSealedDelivery(rec map[string]any, delivered, live dispatchPayload) {
	d, l := payloadFieldSet(delivered), payloadFieldSet(live)
	rec["delivered"] = d
	rec["payload_sha256"] = payloadSHA256(delivered)
	var diverged []string
	moved := map[string]string{}
	for _, k := range comparedFields {
		if l[k] != d[k] {
			diverged = append(diverged, k)
			moved[k] = l[k]
		}
	}
	if len(diverged) == 0 {
		return
	}
	sort.Strings(diverged)
	rec["diverged_fields"] = diverged
	rec["live_fields"] = moved
}

// spoolPath is the §3.9a location, inside the SPEC-12 §3.2 state tree's own
// `spool/` subtree and in a subdirectory no other subsystem lists. It is exported
// because the composition root owns the state root (SPEC-12 §3.1).
func spoolPath(stateRoot string) string { return stateRoot + "/spool/flow/spawn" }

// SpoolPath is spoolPath for the composition root.
func SpoolPath(stateRoot string) string { return spoolPath(stateRoot) }

// spoolPut writes one durable spawn entry for a request that never reached the
// wire, and reports whether it landed on a queue THIS subsystem replays. It seals
// the payload it renders (§3.9b), so the bytes on disk are the bytes a replay
// delivers. It never claims durability it does not have: a sink that is not a
// spoolQueue (the desk adapter) and a store that refused the write both answer
// false with a reason token, and the caller records that.
func (f *Flow) spoolPut(ctx context.Context, stage, reason string, req types.SpawnRequest, inc types.Incident) bool {
	return f.spoolWrite(ctx, stage, reason, f.sealSpawn(req), inc)
}

// spoolWrite is the one durable write seam: it marshals the sealed entry payload,
// asks the store, records the drops and states truthfully whether the entry landed
// on a queue this subsystem replays.
func (f *Flow) spoolWrite(ctx context.Context, stage, reason string, sd sealedDispatch, inc types.Incident) bool {
	taskID := firstNonEmpty(sd.TaskID, sd.Payload.IdemKey)
	if f.spool == nil {
		f.record(ctx, inc, map[string]any{
			"stage": stage, "decision": "failed", "dispatch_state": "unspooled",
			"reason": firstNonEmpty(reason, "spool_not_wired"), "task_id": taskID,
			"idem_key": taskID, "coupling": spoolCoupling,
			"error_code": string(types.CodeFlow005),
		})
		return false
	}
	body, err := json.Marshal(sd)
	if err != nil {
		f.record(ctx, inc, map[string]any{
			"stage": stage, "decision": "failed", "dispatch_state": "unspooled",
			"reason": "payload_marshal", "task_id": taskID, "idem_key": taskID,
			"coupling": spoolCoupling, "error_code": string(types.CodeFlow005),
		})
		return false
	}
	now := f.clock().Now()
	entry := types.SpoolEntry{
		ID: types.NewID(types.PEv), TS: types.FormatUTC(now), Kind: types.SpoolSpawn,
		Payload: body, IdemKey: taskID, NextTryTS: nextAttemptTS(1, now),
	}
	drops, err := f.spool.Put(entry)
	f.recordDrops(ctx, drops)
	if err != nil {
		f.record(ctx, inc, map[string]any{
			"stage": stage, "decision": "failed", "dispatch_state": "spool_failed",
			"reason": "spool_write: " + err.Error(), "task_id": taskID, "idem_key": taskID,
			"coupling": spoolCoupling, "error_code": string(types.CodeFlow005),
		})
		return false
	}
	// The entry is on disk AND a loop this subsystem owns drains it: this is the
	// one case where the record may claim durability.
	f.record(ctx, inc, map[string]any{
		"stage": stage, "decision": "dispatched", "dispatch_state": "spooled",
		"task_id": taskID, "idem_key": taskID, "router_ref": taskID,
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
// dispatch or a second replay of the same entry; re-dispatch delivers the entry's
// SEALED payload with the SAME idem key (§3.9b), so the router's own idem_key
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
		sd, legacy, origin := decodeSpoolPayload(e.Payload)
		if origin == payloadCorrupt {
			// A payload this build cannot decode is not a retryable failure: it
			// is a corrupt entry, and saying so beats looping on it forever.
			f.dropSpoolEntry(ctx, e, payloadCorrupt)
			continue
		}
		req := legacy
		if origin == payloadSealed {
			req = spawnFromPayload(sd.Payload)
		}
		if !f.claimIdem(e.IdemKey) {
			continue
		}
		var derr error
		if origin == payloadSealed {
			// The payload the entry was WRITTEN with, delivered as it was rendered:
			// neither a config change nor a board move retro-mutates a queued
			// dispatch, so `requested` and `delivered` cannot silently part ways.
			_, derr = f.attempt(ctx, sd.Payload)
		} else {
			// A pre-§3.9b entry: the bare marshalled SpawnRequest, so the body
			// must be re-derived from live state. It is delivered (never dropped as
			// corrupt) and the record says so.
			_, _, derr = f.dispatchSpawn(ctx, legacy, false)
		}
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
				"payload_origin": origin,
				"coupling":       spoolCoupling, "error_code": codeOfErr(derr),
			})
			continue
		}
		_ = f.spool.Delete(e.ID)
		rec := map[string]any{
			"stage": "dispatch", "decision": "dispatched", "dispatch_state": "replayed",
			"task_id": req.TaskID, "idem_key": e.IdemKey, "router_ref": req.TaskID,
			"attempts": e.Attempts + 1, "payload_origin": origin, "coupling": spoolCoupling,
		}
		if origin == payloadSealed {
			// The delivered field set plus, when the live inputs have moved since
			// the entry was written, what a re-derivation WOULD have produced: a
			// reader can always tell what was requested from what was delivered.
			recordSealedDelivery(rec, sd.Payload, f.liveRederivation(sd.Payload))
		} else {
			rec["rederived_fields"] = rederivedFields
		}
		f.record(ctx, inc, rec)
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
	sd, legacy, origin := decodeSpoolPayload(e.Payload)
	taskID, sig, inc := firstNonEmpty(legacy.TaskID, e.IdemKey), legacy.Sig, legacy.Inc
	if origin == payloadSealed {
		taskID, sig, inc = firstNonEmpty(sd.TaskID, sd.Payload.IdemKey), sd.Sig, sd.Inc
	}
	_ = f.spool.Delete(e.ID)
	d := DropEvent{ID: e.ID, TS: e.TS, Reason: reason, TaskID: taskID,
		Sig: sig, Inc: inc, Attempts: e.Attempts, EstLost: -1}
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
