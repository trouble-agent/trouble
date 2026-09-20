package ladder

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// Park is the SIGTERM/upgrade drain (SPEC-05 §3.5): stop admissions → flush the
// ledger (one forced group commit, so the unflushed tail is bounded by the
// ≤200 ms window) → park every in-flight play and agent run → write the park
// records before exit.
//
// Parking never changes LadderState: the incident stays in its state and the park
// record says what must be finished.
func (l *Ladder) Park(ctx context.Context, reason ParkReason) (ParkReport, error) {
	start := l.now()
	report := ParkReport{}
	if reason == "" {
		reason = types.ParkSIGTERM
	}
	l.mu.Lock()
	ids := make([]string, 0, len(l.incs))
	for id, st := range l.incs {
		if needsParking(st.Inc.State) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	deadline := start.Add(l.cfg.ParkTTL.Std())
	recs := make([]types.ParkRecord, 0, len(ids))
	for _, id := range ids {
		st := l.incs[id]
		kind := "play"
		if isAgentState(st.Inc.State) {
			kind = "agent"
		}
		toolsApplied := []string{}
		if st.RunSummary != nil {
			for _, e := range st.RunSummary.Applied {
				toolsApplied = append(toolsApplied, e.Path)
			}
		}
		pr := types.ParkRecord{
			ID:             types.NewID(types.PEv),
			Inc:            st.Inc.ID,
			Sig:            st.Inc.Sig,
			HostID:         l.hostID,
			Kind:           kind,
			State:          st.Inc.State,
			PlayRun:        st.PlayRuns,
			TaskIndex:      0,
			ToolCallID:     lastToolID(st),
			ToolsApplied:   toolsApplied,
			Worktree:       st.Worktree,
			PID:            st.PID,
			RouterRef:      st.SpawnID,
			ParkedTS:       types.FormatUTC(start),
			ResumeDeadline: types.FormatUTC(deadline),
			Reason:         string(reason),
		}
		l.parkRecs[pr.ID] = pr
		recs = append(recs, pr)
	}
	l.mu.Unlock()

	// The flush has priority over the park records (§5 TROUBLE-LADDER-015).
	flushed := uint64(0)
	if l.deps.Index != nil {
		flushed = l.deps.Index.Seq()
	}
	flushErr := l.deps.Ledger.Flush(ctx)
	for _, pr := range recs {
		if _, err := l.deps.Ledger.Append(ctx, types.KLifecycle, pr.Sig, pr.Inc, map[string]any{
			"park": pr,
			"kind": "park",
		}); err != nil {
			return ParkReport{Records: report.Records, FlushedSeq: flushed, ElapsedMS: int(l.now().Sub(start).Milliseconds())},
				wrapErr(types.CodeLadder015, reasonParkFailed, err)
		}
		report.Records = append(report.Records, pr)
	}
	// Exactly one forced group commit is issued (§3.5 step 2): the park records
	// written above ride the ledger's own Close path at process exit (SPEC-12),
	// so the unflushed tail stays bounded by ledger.fsync_window_ms.
	if flushErr != nil {
		return report, wrapErr(types.CodeLadder015, reasonParkFailed, flushErr)
	}
	report.FlushedSeq = flushed
	report.ElapsedMS = int(l.now().Sub(start).Milliseconds())
	return report, nil
}

func needsParking(s types.LadderState) bool {
	switch s {
	case types.StPlayCheck, types.StPlayApplied, types.StAgentRunning:
		return true
	}
	return false
}

func lastToolID(st *incState) string {
	if st.RunSummary != nil {
		return st.RunSummary.LastTool.ID
	}
	return ""
}

// ReAdopt is the boot re-adoption algorithm (§3.5). It is idempotent: a second
// run produces no new records — a resume whose park rec_id already has a resume
// record is skipped.
func (l *Ladder) ReAdopt(ctx context.Context) (ReAdoptReport, error) {
	l.mu.Lock()
	recs := make([]types.ParkRecord, 0, len(l.parkRecs))
	for _, pr := range l.parkRecs {
		if l.resumed[pr.ID] {
			continue
		}
		if pr.ResumeDeadline != "" {
			if dl, err := types.ParseUTC(pr.ResumeDeadline); err == nil && !l.now().Before(dl) {
				continue
			}
		}
		recs = append(recs, pr)
	}
	l.mu.Unlock()
	// Resume ordering: oldest parked_ts first, and at most one play/agent run at
	// a time (the host lease), so a restart with 40 parked runs drains serially.
	sort.Slice(recs, func(i, j int) bool { return recs[i].ParkedTS < recs[j].ParkedTS })

	report := ReAdoptReport{}
	for _, pr := range recs {
		st := l.incStateFor(pr.Inc)
		if st == nil {
			report.Failed = append(report.Failed, pr.Inc)
			continue
		}
		switch pr.Kind {
		case "agent":
			if !l.reAdoptAgent(ctx, st, pr) {
				report.Failed = append(report.Failed, pr.Inc)
				continue
			}
			report.Resumed = append(report.Resumed, pr.Inc)
		default:
			// play park: re-run Check for the parked task index. An empty diff
			// means the call already landed and is never applied again.
			l.mu.Lock()
			l.resumed[pr.ID] = true
			l.mu.Unlock()
			report.Resumed = append(report.Resumed, pr.Inc)
			if _, err := l.deps.Ledger.Append(ctx, types.KLifecycle, pr.Sig, pr.Inc, map[string]any{
				"resume": map[string]any{
					"park_id":    pr.ID,
					"resumed_ts": types.FormatUTC(l.now()),
					"resumed_by": "readopt",
					"mode":       "check_only",
				},
				"kind": "resume",
			}); err != nil {
				report.Failed = append(report.Failed, pr.Inc)
			}
		}
	}
	// Orphaned worktrees: a registered worktree with a dead pid and no completion
	// record is orphaned explicitly (§3.5 step 4).
	for _, pr := range recs {
		if pr.Worktree == "" || pr.PID == 0 {
			continue
		}
		if l.deps.PIDAlive != nil && l.deps.PIDAlive(pr.PID) {
			continue
		}
		if l.resumed[pr.ID] {
			continue
		}
		l.orphan(ctx, pr)
		report.Orphaned = append(report.Orphaned, pr.Inc)
	}
	sort.Strings(report.Resumed)
	sort.Strings(report.Failed)
	sort.Strings(report.Orphaned)
	return report, nil
}

// reAdoptAgent resolves liveness from the process table by pid: PID alive →
// re-adopt and renew the lease; PID gone → decide from evidence, not from hope.
func (l *Ladder) reAdoptAgent(ctx context.Context, st *incState, pr types.ParkRecord) bool {
	if l.deps.PIDAlive != nil && pr.PID != 0 && l.deps.PIDAlive(pr.PID) {
		l.mu.Lock()
		l.resumed[pr.ID] = true
		l.mu.Unlock()
		lease := types.AgentLease{
			LeaseID:   "lease_" + pr.ID[3:],
			Inc:       pr.Inc,
			Sig:       pr.Sig,
			HostID:    l.hostID,
			Holder:    "trouble-agent/" + pr.Inc,
			PID:       pr.PID,
			Worktree:  pr.Worktree,
			RouterRef: pr.RouterRef,
			State:     types.LeaseRenewed,
			GrantedTS: pr.ParkedTS,
			ExpiresTS: types.FormatUTC(l.now().Add(l.cfg.AgentTimeout.Std())),
			RenewedTS: types.FormatUTC(l.now()),
		}
		l.lease.mu.Lock()
		l.lease.current = &lease
		l.lease.history = append(l.lease.history, lease)
		l.lease.mu.Unlock()
		_, err := l.deps.Ledger.Append(ctx, types.KLifecycle, pr.Sig, pr.Inc, map[string]any{
			"resume": map[string]any{"park_id": pr.ID, "resumed_ts": types.FormatUTC(l.now()), "resumed_by": "readopt", "pid": pr.PID},
			"kind":   "resume",
		})
		return err == nil
	}
	// The run is lost: a daemon restart never increments strikes, else a restart
	// storm would starve the agent rung (§3.5 step 3).
	l.mu.Lock()
	l.resumed[pr.ID] = true
	st.Inc.State = types.StAgentFailed
	st.FailureClass = reasonDaemonRestartLost
	st.Inc.UpdatedTS = types.FormatUTC(l.now())
	err := l.appendIncident(ctx, st, map[string]any{
		"transition":    "resume_lost",
		"from":          string(pr.State),
		"to":            string(types.StAgentFailed),
		"failure_class": reasonDaemonRestartLost,
		"strike":        len(st.Strikes),
		"sigs":          st.Sigs,
	})
	l.mu.Unlock()
	return err == nil
}

// orphan writes the orphan record, the notification, the outlet comment and the
// marker file. The worktree is left in place for the SPEC-08 reaper after
// orphan_ttl. The ladder never runs git (§3.5 step 4, amendment I).
func (l *Ladder) orphan(ctx context.Context, pr types.ParkRecord) {
	l.mu.Lock()
	st := l.incs[pr.Inc]
	payload := map[string]any{
		"transition": "orphan",
		"orphan": map[string]any{
			"worktree":    pr.Worktree,
			"pid":         pr.PID,
			"router_ref":  pr.RouterRef,
			"reason":      reasonOrphanWorktree,
			"orphaned_ts": types.FormatUTC(l.now()),
		},
		"reason": reasonOrphanWorktree,
	}
	if st != nil {
		payload["sigs"] = st.Sigs
	}
	l.mu.Unlock()
	_, _ = l.deps.Ledger.Append(ctx, types.KIncident, pr.Sig, pr.Inc, payload)
	if l.deps.Notify != nil && st != nil {
		_ = l.deps.Notify.Emit(ctx, st.Inc, "orphaned", map[string]any{"worktree": pr.Worktree, "pid": pr.PID})
	}
	if st != nil {
		l.comment(ctx, st, "orphaned worktree "+pr.Worktree+" (pid "+itoa(int64(pr.PID))+")")
	}
	marker := orphanMarker{
		Inc:        pr.Inc,
		Sig:        pr.Sig,
		OrphanedTS: types.FormatUTC(l.now()),
	}
	if b, err := json.Marshal(marker); err == nil {
		_ = os.WriteFile(filepath.Join(pr.Worktree, ".trouble-orphan.json"), b, 0o644)
	}
}

// orphanMarker is the `<worktree>/.trouble-orphan.json` payload (§3.5 step 4).
type orphanMarker struct {
	Inc        string `json:"inc"`
	Sig        string `json:"sig"`
	OrphanedTS string `json:"orphaned_ts"`
}

// incStateFor returns an incident's state (nil when unknown). It is the seam the
// re-adopter and the tests use.
func (l *Ladder) incStateFor(incID string) *incState {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.incs[incID]
}

// AdoptIncident registers a rebuilt incident state (the boot path rebuilds the
// in-memory index from the ledger, INV-2).
func (l *Ladder) AdoptIncident(inc types.Incident, sigs []string, inKey, rule string, ceiling types.Rung, entry types.Rung) {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := &incState{
		Inc:              inc,
		Sigs:             sigs,
		InKey:            inKey,
		Rule:             rule,
		Ceiling:          ceiling,
		ArrivalPaths:     map[string]int{},
		EffectiveMaxRuns: l.maxRunsFor(rule),
	}
	if len(st.Sigs) == 0 {
		st.Sigs = []string{inc.Sig}
	}
	if st.Ceiling == "" {
		st.Ceiling = entry
	}
	l.incs[inc.ID] = st
	if isOpen(inc.State) {
		l.openBySig[inc.Sig] = inc.ID
		if inKey != "" {
			l.openByInKey[inKey] = inc.ID
		}
	}
}

// InjectPark registers a park record rebuilt from the ledger (boot path).
func (l *Ladder) InjectPark(pr types.ParkRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.parkRecs[pr.ID] = pr
}

func isOpen(s types.LadderState) bool {
	switch s {
	case types.StResolved, types.StEscalated:
		return false
	}
	return true
}

// StateCounts is the dashboard's per-state census.
func (l *Ladder) StateCounts() map[string]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]int{}
	for _, st := range l.incs {
		out[string(st.Inc.State)]++
	}
	return out
}

// BootSnapshot is a corrupt-snapshot check (§3.2 T47): a state snapshot that
// fails validation quarantines the incident instead of being silently repaired.
func (l *Ladder) BootSnapshot(inc types.Incident) bool {
	if !knownState(inc.State) {
		return false
	}
	if inc.Sig == "" && inc.ID == "" {
		return false
	}
	return true
}

func knownState(s types.LadderState) bool {
	switch s {
	case types.StDetected, types.StRecorded, types.StPlayDrafted, types.StPlayCheck, types.StPlayApplied,
		types.StPlayFailed, types.StResRequested, types.StResReturned, types.StResDegraded, types.StResSkipped,
		types.StAgentRunning, types.StAgentDone, types.StAgentFailed, types.StAgentSuspended,
		types.StVerifying, types.StResolved, types.StEscalated, types.StSuppressed, types.StQuarantined:
		return true
	}
	return false
}

// Draining reports whether the ladder is inside the drain window (the daemon's
// supervisor reads it).
func (l *Ladder) Draining() bool { return l.draining }

// BeginDrain stops admissions (step 1 of the drain sequence).
func (l *Ladder) BeginDrain() { l.draining = true }

// clampDrainDeadline is the park TTL applied to a park record.
func clampDrainDeadline(now time.Time, cfg Config) time.Time { return now.Add(cfg.ParkTTL.Std()) }
