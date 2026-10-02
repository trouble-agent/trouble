package sensors

// dbus_boot.go — TRBL-094, the fresh-boot D-Bus incident storm.
//
// On a headless agent host the boot reconcile's first ListUnits returns a
// burst of pre-existing failed USER units — session units that cannot start
// where no real session exists. Expected headless noise: the host is healthy.
// Before this gate every first-sight unit opened its own incident in the same
// moment (fresh per-sig cooldown, `for = 0s`, closed breakers), so 21 units
// opened 21 incidents inside one 300s window and the rule:dbus_unit_failed
// breaker tripped (SPEC-03 §3.8: >20 incidents in 300s) within the first
// minute of daemon life — the breaker ate the burst and suppressed the later
// REAL failure path with it.
//
// The mechanism: boot inventory is STABILIZED, not treated as live edges.
// The gate owns each boot unit's FIRE admission, in three dispositions:
//
//   - bootHold: a unit first sighted failed inside the stabilization window
//     is held. Its observation is still RECORDED (fire=false, Suppressed
//     counted, boot_stabilized named: events are never dropped, §3.8); its
//     merge story is not opened yet.
//   - admission: each later sweep releases the next paced slice of holds
//     (bootAdmitPerSweep per bootDrainInterval — derived from §3.8's own
//     300s incident window, so the rate stays strictly below the >20 trip
//     arm whatever the configured reconcile interval). A released unit that
//     is still failed opens its REAL merge story through the FULL rule
//     pipeline — a real incident, a real fire, exactly once.
//   - bootSuppress: after its admission fire, a boot unit's later reconcile
//     re-sights are suppressed at the ladder gate (recorded, counted) until
//     it RECOVERS. Without this, every persistent failed unit re-fires each
//     cooldown (§3.5's designed re-notification), and 21 persistent units
//     re-firing in one sweep re-form the storm — the breaker would trip
//     again from t+20m onward, forever. A boot unit whose story is open
//     needs no re-notification: its incident IS open. Recovery is observed
//     the only way a sensor can observe it: the unit leaving the sweep's
//     failed set. A recovered unit's gate entries are cleared, so a genuine
//     later failure is a fresh story that fires normally.
//
// A unit first sighted after the window, and every live
// PropertiesChanged/JobRemoved transition, skip the gate entirely and fire
// exactly as before. Breaker thresholds are untouched; the §3.8 trip table
// stays exactly what the spec published.

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

const (
	// bootStabilizeWindow is how long after daemon start a first-sight failed
	// unit counts as boot inventory rather than a live edge. The ListUnits
	// sweep is the boot inventory's only source and the first sweep runs at
	// daemon start; the window covers the boot posture plus a fast resync
	// (a NameOwnerChanged re-subscribe reconcile inside the first minutes).
	bootStabilizeWindow = 90 * time.Second

	// bootAdmitPerSweep is the per-drain admission cap for held boot units.
	// This is the explicitly named, tested cap the acceptance criteria ask
	// the boot burst to be bounded by.
	bootAdmitPerSweep = 5

	// bootDrainInterval paces the admissions. It is DERIVED from §3.8's
	// rule-scope incident window (300s), not from the reconcile timer: one
	// third of it puts the worst-case rate at 15 incidents per 300s —
	// strictly below the >20 trip arm — independent of
	// sensors.dbus.reconcile_interval and of how many managers reconcile
	// back to back (a drain after the first in an interval releases nothing).
	bootDrainInterval = incidentWindow / 3

	// bootHoldMax bounds the hold table (§3.8a's bound shape): at the bound
	// the OLDEST holds are dropped to FIFO rotation rather than the table
	// growing without limit. A dropped hold re-holds on its next sight; the
	// bound costs admission delay, never an observation — a held unit's boot
	// record is already durable.
	bootHoldMax = 4096
)

// bootDisposition is what the gate decides for one sighted unit.
type bootDisposition int

const (
	bootPass     bootDisposition = iota // stock pipeline, gate not involved
	bootHold                            // boot inventory: record, do not open the story yet
	bootSuppress                        // admitted, story open: record the accounting, no re-fire
)

// bootStabilizer is the hold table for the boot inventory. The reconcile's
// decision phase classifies every sighted unit through it, drains the next
// paced slice of holds per interval, and clears units that recovered.
type bootStabilizer struct {
	mu         sync.Mutex
	start      time.Time
	window     time.Duration
	drainEvery time.Duration
	lastDrain  time.Time
	held       map[string]bool // manager/unit held (pending admission)
	admitted   map[string]bool // manager/unit whose admission fire landed
	order      []string        // FIFO hold order
	released   []string        // the slice the current batch carries (rehold on write failure)
	// pendingFires holds the (rule,sig) cooldown spends of the pending
	// admission slice, for the failed-write handback in reholdPending.
	pendingFires []firedEntry
}

func newBootStabilizer(start time.Time, window, drainEvery time.Duration) *bootStabilizer {
	return &bootStabilizer{
		start:      start,
		window:     window,
		drainEvery: drainEvery,
		held:       map[string]bool{},
		admitted:   map[string]bool{},
	}
}

// inWindow reports whether `now` is inside the boot-stabilization window.
func (b *bootStabilizer) inWindow(now time.Time) bool {
	return now.Sub(b.start) < b.window
}

// classify decides one sighted unit. A first sight inside the window is
// registered as a hold; a held unit stays held; an ADMITTED unit whose story
// is open is suppressed at the ladder gate (recorded, counted) — without
// this arm the steady-state re-sights of 21 persistent failed units would
// re-fire in lockstep each reconcile sweep (the sweep interval equals the
// 5m default cooldown in production) and re-form the storm forever. A unit
// released but never durably fired (a failed drain batch that was reheld)
// must reach bootSuppress only after its admission fire lands; the rehold
// path clears its admitted mark so the retry re-runs the real pipeline.
func (b *bootStabilizer) classify(key string, now time.Time) bootDisposition {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.held[key] {
		return bootHold
	}
	if b.admitted[key] {
		return bootSuppress
	}
	if now.Sub(b.start) >= b.window {
		return bootPass
	}
	// Bound: drop the oldest holds to FIFO rotation instead of growing past
	// it. A dropped hold re-holds on its next sight (the bound costs pacing).
	for len(b.held) >= bootHoldMax && len(b.order) > 0 {
		oldest := b.order[0]
		b.order = b.order[1:]
		delete(b.held, oldest)
	}
	b.held[key] = true
	b.order = append(b.order, key)
	return bootHold
}

// drainReady releases the next slice of the requesting manager's holds (FIFO
// within the shared order) when the pacing interval has elapsed, and reports
// their keys. The pacing clock is SHARED across managers, so the total
// admission rate stays bootAdmitPerSweep per bootDrainInterval whatever the
// manager count; the slice is manager-scoped so every released key is
// admitted by the very sweep that asked. A released unit absent from the
// failed set has recovered — nothing opens, and the recovery sweep clears it.
func (b *bootStabilizer) drainReady(manager string, now time.Time) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if now.Sub(b.lastDrain) < b.drainEvery || len(b.order) == 0 {
		return nil
	}
	b.lastDrain = now
	prefix := manager + "/"
	out := make([]string, 0, bootAdmitPerSweep)
	kept := b.order[:0]
	for _, k := range b.order {
		take := len(out) < bootAdmitPerSweep && strings.HasPrefix(k, prefix)
		if take {
			delete(b.held, k)
			b.admitted[k] = true
			out = append(out, k)
			continue
		}
		kept = append(kept, k)
	}
	b.order = kept
	b.released = out
	return out
}

// firedEntry is one admission fire the decide phase spent cooldown state on:
// the (rule, sig) identity, captured from the draft the pipeline produced.
type firedEntry struct {
	rule string
	sig  string
}

// noteFired records the (rule,sig) cooldown spends of the pending admission
// slice, so a failed batch write can undo exactly what the failed attempt
// consumed.
func (b *bootStabilizer) noteFired(entries []firedEntry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pendingFires = append(b.pendingFires, entries...)
}

// reholdPending returns the last released slice to the hold table after a
// failed batch write, so the next sweep retries the admission instead of the
// units being silently masked: their admitted marks are undone, they rejoin
// the FRONT of the FIFO in release order, and the pacing clock is released —
// the retry must not wait out another full drain interval. The (rule,sig)
// cooldown spends of the failed attempt are handed back: a fire the ledger
// refused never reached the ladder, so the cooldown's purpose (do not
// re-notify faster than the ladder can act) has NOT been served and the
// retry must be able to fire again. The pending slice is cleared either way
// once the batch outcome is known, so a later ordinary batch failure cannot
// rehold an already durable admission.
func (b *bootStabilizer) reholdPending() []firedEntry {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := len(b.released) - 1; i >= 0; i-- {
		k := b.released[i]
		if b.held[k] {
			continue
		}
		delete(b.admitted, k)
		b.held[k] = true
		b.order = append([]string{k}, b.order...)
	}
	b.released = nil
	entries := b.pendingFires
	b.pendingFires = nil
	if len(b.order) > 0 {
		b.lastDrain = time.Time{} // the retry drains on the very next sweep
	}
	return entries
}

// clearPendingRehold marks the last released slice durable: its batch landed,
// so both the slice and the cooldown spends it recorded are consumed — the
// pending accounting is cleared so a later, unrelated batch failure cannot
// hand back spends that were genuinely served.
func (b *bootStabilizer) clearPendingRehold() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.released = nil
	b.pendingFires = nil
}

// sweepRecovery clears the gate entries of one manager's units that are NOT in
// the sweep's failed set: the unit recovered (or vanished), so its story is
// over and a genuine later failure must fire fresh.
func (b *bootStabilizer) sweepRecovery(manager string, failedSet map[string]bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	prefix := manager + "/"
	for k := range b.held {
		if strings.HasPrefix(k, prefix) && !failedSet[strings.TrimPrefix(k, prefix)] {
			delete(b.held, k)
		}
	}
	for k := range b.admitted {
		if strings.HasPrefix(k, prefix) && !failedSet[strings.TrimPrefix(k, prefix)] {
			delete(b.admitted, k)
		}
	}
	// The FIFO order may name cleared keys; prune it so drains stay honest.
	pruned := b.order[:0]
	for _, k := range b.order {
		unit := strings.TrimPrefix(k, prefix)
		if !strings.HasPrefix(k, prefix) || (b.held[k] && failedSet[unit]) {
			pruned = append(pruned, k)
		}
	}
	b.order = pruned
}

// heldCount is the diagnostic surface (tests + operator notes).
func (b *bootStabilizer) heldCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.held)
}

// initBootGate arms the boot gate. startDBus calls it BEFORE the first
// reconcile so the daemon's very first ListUnits sweep — the boot inventory —
// is inside the stabilization window.
func (s *Sensors) initBootGate() {
	s.dbState.boot = newBootStabilizer(s.now(), bootStabilizeWindow, bootDrainInterval)
}

// markBootDeferred overlays the stabilization disposition on a boot inventory
// record: the observation is recorded (never dropped), the fire is suppressed
// at the ladder gate, and both facts are named on the payload so the
// deferral is visible, not silent (§3.8's counters stay honest).
func (s *Sensors) markBootDeferred(payload map[string]any) {
	payload["fire"] = false
	payload["suppressed"] = true
	payload["boot_stabilized"] = true
	s.suppressed.Add(1)
}

// bootDeferredDraft renders one held boot observation: the reconcile wire
// shape — kind=event, the dbus sig, the per-unit detail — with fire=false,
// suppressed=true and boot_stabilized=true naming the ladder-gate
// disposition. It deliberately does NOT run the rule pipeline: nothing about
// a held observation may open an incident.
func (s *Sensors) bootDeferredDraft(u unitStatus, manager string, now time.Time) types.RecordDraft {
	detail := map[string]any{
		"unit_key":          u.Name,
		"unit_active_state": u.ActiveState,
		"unit_substate":     u.SubState,
		"failure_class":     "failed",
		"crash_loop":        false,
		"arrival_path":      string(arrivalReconcile),
		"manager":           manager,
		"exit_code":         0,
		"count":             float64(1),
		"boot_stabilized":   true,
	}
	ev := types.SensorEvent{
		ID:     types.NewID(types.PEv),
		TS:     types.FormatUTC(now),
		Sensor: types.SenDBus,
		Scope:  u.Name,
		Value:  float64(1),
		Unit:   "",
		Detail: detail,
		Sig:    sigFor(types.SrcDBus, manager, u.Name, "failed"),
	}
	payload := s.eventPayload(ev, "")
	s.markBootDeferred(payload)
	return s.draftFor(ev, payload)
}

// bootSuppressedDraft renders a post-admission re-sight of a boot unit whose
// story is open: the FULL merge accounting (count, attach, reopen) rides on
// the record like any reconcile record, but the rule pipeline did not run —
// no re-fire, no breaker contact, no cooldown consumed. The suppression is
// declared on the payload and counted in Suppressed (§3.8).
func (s *Sensors) bootSuppressedDraft(res mergeOutcome, manager string, u unitStatus, now time.Time) types.RecordDraft {
	arr, merges, opens, journald := s.dbState.merge.snapshot()
	s.merges.Store(merges)
	_ = opens
	_ = journald
	detail := map[string]any{
		"unit_key":          res.UnitKey,
		"unit_active_state": u.ActiveState,
		"unit_substate":     u.SubState,
		"failure_class":     res.Class,
		"crash_loop":        res.CrashLoop,
		"arrival_path":      string(arrivalReconcile),
		"manager":           manager,
		"exit_code":         0,
		"count":             float64(res.Count),
		"merge_arrivals":    arr,
		"merged":            res.Attached,
		"reopen_count":      res.Reopened,
		"severity":          string(res.Severity),
		"sensors_arrivals":  s.dbState.arrivals.Load(),
		"sensors_merges":    merges,
		"value":             float64(res.Count),
		"unit_field":        "",
		"msg":               "",
		"substr":            "",
		"window_s":          0,
		"age_s":             0,
		"boot_stabilized":   true,
	}
	ev := types.SensorEvent{
		ID:     types.NewID(types.PEv),
		TS:     types.FormatUTC(now),
		Sensor: types.SenDBus,
		Scope:  res.UnitKey,
		Value:  float64(res.Count),
		Unit:   "",
		Detail: detail,
		Sig:    res.Sig,
	}
	payload := s.eventPayload(ev, "")
	s.markBootDeferred(payload)
	return s.draftFor(ev, payload)
}

// runBootInventoryGate is the reconcile decision phase's TRBL-094 half: clear
// recovered units, release the next paced admission slice, then decide each
// sighted unit through the gate — every unit arrives at the merge tracker
// EXACTLY ONCE per sweep (the released slice through the admission arm, the
// rest through classify). evaluateEveryUnit delegates here when the boot gate
// is armed; the returned drafts ride the caller's ONE batch.
func (s *Sensors) runBootInventoryGate(name string, failed []unitStatus, now time.Time) []types.RecordDraft {
	failedSet := make(map[string]unitStatus, len(failed))
	failedNames := make(map[string]bool, len(failed))
	for _, u := range failed {
		failedSet[u.Name] = u
		failedNames[u.Name] = true
	}
	s.dbState.boot.sweepRecovery(name, failedNames)
	releasedSet := map[string]bool{}
	for _, k := range s.dbState.boot.drainReady(name, now) {
		releasedSet[bootUnitOf(k, name)] = true
	}
	drafts := make([]types.RecordDraft, 0, len(failed))
	var firedEntries []firedEntry
	for _, u := range failed {
		if releasedSet[u.Name] {
			// Admission: the one arrival that opens (or folds into) the
			// unit's real story, through the FULL rule pipeline — a real
			// fire. An accepted outcome of ANY kind drafts: after a failed
			// batch write the story may already be open, and the admission
			// fire must fold into it (the ladder's AC-22 shape), not vanish.
			res := s.dbState.merge.arrival(name, u.Name, arrivalReconcile, u.SubState, "", "", now)
			if res.Opened || res.Attached || res.Reopened || res.Counted {
				d := s.dbusOutcomeDraft(res, name, u.ActiveState, u.SubState, arrivalReconcile)
				if fire, _ := d.Payload["fire"].(bool); fire {
					firedEntries = append(firedEntries, firedEntry{rule: fmt.Sprint(d.Payload["rule"]), sig: d.Sig})
				}
				drafts = append(drafts, d)
			}
			continue
		}
		switch s.dbState.boot.classify(name+"/"+u.Name, now) {
		case bootHold:
			drafts = append(drafts, s.bootDeferredDraft(u, name, now))
		case bootSuppress:
			// The merge accounting still runs (the story's count and the
			// tracker's own M4 crash-loop re-derivation ride on the arrival);
			// only the rule pipeline is skipped — no re-fire, no breaker
			// contact, no cooldown consumed.
			res := s.dbState.merge.arrival(name, u.Name, arrivalReconcile, u.SubState, "", "", now)
			if res.Opened || res.Attached || res.Reopened {
				drafts = append(drafts, s.bootSuppressedDraft(res, name, u, now))
			}
		default: // bootPass: stock pipeline, unchanged
			res := s.dbState.merge.arrival(name, u.Name, arrivalReconcile, u.SubState, "", "", now)
			if res.Opened || res.Attached || res.Reopened {
				drafts = append(drafts, s.dbusOutcomeDraft(res, name, u.ActiveState, u.SubState, arrivalReconcile))
			}
		}
	}
	if len(firedEntries) > 0 {
		s.dbState.boot.noteFired(firedEntries)
	}
	return drafts
}

// bootUnitOf maps a hold key back to its unit name for one manager's sweep.
func bootUnitOf(key, manager string) string {
	return strings.TrimPrefix(key, manager+"/")
}
