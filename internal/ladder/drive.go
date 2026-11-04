package ladder

import (
	"context"
	"sort"
	"sync/atomic"

	"github.com/trouble-agent/trouble/internal/types"
)

// drive.go is the ladder's rung DRIVER (SPEC-05 §4.4a): the half of the state
// machine that moves an incident out of `recorded` without a human clicking a
// dashboard button.
//
// The defect it closes (TRBL-123): `Advance` had exactly two callers — the
// dashboard's ack/close bridge and `ResumePending` — and `ResumePending` had no
// production caller at all. An incident that a sensor or the sentinel admitted
// therefore stayed in `recorded` forever: the product detected and audited, and
// never remediated.
//
// The driver synthesises nothing. Every step below is one row of the §3.2 table,
// driven through Advance, so the kill-switch, the scope breakers, the per-day
// budgets, the ceiling guard, the lease and the quarantine accounting all run as
// they do for any other caller. A rung the guards refuse is recorded by
// `refuseLocked` (the ladder's one refusal path) and the driver moves on.

// rungStep is one rung the driver may enter from `recorded`: the table's own
// row and the state that row lands in. The order is the ladder order of
// SPEC-05 §1: play → research → agent.
type rungStep struct {
	rung types.Rung
	// trigger is the row's Trigger column (§3.2), used to FIND the row so the
	// driver never hardcodes an edge id: a table edit that renames a row or
	// renumbers it moves the driver with it.
	trigger string
	to      types.LadderState
}

// rungSteps is the driver's ladder order. Each entry names the §3.2 trigger;
// advanceFor resolves it to the row id Advance expects (Transition.Trigger
// carries the edge ID, see resolveEdge), so no edge id is restated here.
var rungSteps = []rungStep{
	{rung: types.RungPlay, trigger: "rung_advance_play", to: types.StPlayDrafted},
	{rung: types.RungResearch, trigger: "rung_advance_research", to: types.StResRequested},
	{rung: types.RungAgent, trigger: "rung_advance_agent", to: types.StAgentRunning},
}

// edgeByTrigger resolves a table row by its Trigger column.
func edgeByTrigger(trigger string) (edge, bool) {
	for _, e := range legal {
		if e.Trigger == trigger {
			return e, true
		}
	}
	return edge{}, false
}

// DriveReport is the outcome of one driver pass (SPEC-05 §4.4a).
type DriveReport struct {
	// Considered counts the incidents the pass examined: every open incident in
	// `recorded`, whatever its ceiling.
	Considered int
	// Advanced lists the incident ids that entered a rung this pass.
	Advanced []string
	// Failed lists the ids whose advance failed for a reason other than the
	// table's own refusal (for example a ledger append failure).
	Failed []string
}

// DriveStats is the driver's own trace, exposed so the composition root's boot
// leg and cadence loop can be verified without waiting for a tick.
//
// It is a seam counter, not a health surface: it says what THIS process has
// called, never what the ledger holds (`trouble ledger status` answers that).
type DriveStats struct {
	ResumeCalls uint64
	DriveCalls  uint64
	Advanced    uint64
}

type driveCounters struct {
	resumeCalls atomic.Uint64
	driveCalls  atomic.Uint64
	advanced    atomic.Uint64
}

// DriveStats reports this process's driver counters.
func (l *Ladder) DriveStats() DriveStats {
	return DriveStats{
		ResumeCalls: l.drive.resumeCalls.Load(),
		DriveCalls:  l.drive.driveCalls.Load(),
		Advanced:    l.drive.advanced.Load(),
	}
}

// Drive advances every open incident that is waiting for its first rung.
//
// It is bounded by the open-incident set (max_open_incidents), takes no lock
// across a transition and never blocks on a port: the rung-entry edges it drives
// are table rows, and the one edge that calls out (T04's research request) is the
// ladder's own, with its own budget and degradation. A kill-switch returns an
// empty report: the driver does not queue work behind a freeze (ResumePending
// owns the pending set).
func (l *Ladder) Drive(ctx context.Context) (DriveReport, error) {
	rep := DriveReport{}
	l.drive.driveCalls.Add(1)
	if l.Gates().KillSwitch {
		return rep, nil
	}
	l.mu.Lock()
	ids := make([]string, 0, len(l.incs))
	for id, st := range l.incs {
		if st.Inc.State == types.StRecorded {
			ids = append(ids, id)
		}
	}
	l.mu.Unlock()
	// Deterministic order: two passes over the same ledger drive the same
	// incidents in the same sequence.
	sort.Strings(ids)
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		rep.Considered++
		advanced, err := l.DriveOne(ctx, id)
		if err != nil {
			rep.Failed = append(rep.Failed, id)
			continue
		}
		if advanced {
			rep.Advanced = append(rep.Advanced, id)
			l.drive.advanced.Add(1)
		}
	}
	return rep, nil
}

// DriveOne advances one incident from `recorded` to the FIRST rung its ceiling
// admits, in ladder order play → research → agent. It reports whether a rung was
// entered; a refusal is recorded in the ledger by the ladder itself and is not an
// error here (the incident simply stays where it is until the next pass).
//
// An incident in any other state, or one whose ceiling is `record` (the
// audit-only rung: §3.2's T06 outlets-only path is not a rung advance), is left
// untouched.
func (l *Ladder) DriveOne(ctx context.Context, incID string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	l.mu.Lock()
	st, ok := l.incs[incID]
	if !ok {
		l.mu.Unlock()
		return false, newErr(types.CodeLadder012, "", "incident %s is not in the index", incID)
	}
	state := st.Inc.State
	ceiling := st.Ceiling
	killSwitch := l.gate.KillSwitch
	// The transition sequence is the ledger's own watermark, raised above the
	// incident's last accepted seq so a stale reader can never make the next
	// Advance look like a replay (INV-5 dedups a real repeat, not a new drive).
	seq := st.LastSeq + 1
	l.mu.Unlock()
	if killSwitch || state != types.StRecorded {
		return false, nil
	}
	if l.deps.Index != nil {
		if idx := l.deps.Index.Seq(); idx > seq {
			seq = idx
		}
	}
	for _, step := range rungSteps {
		// The ceiling check runs again inside the edge's own guard
		// (guardRungAtLeast); this filter only keeps the driver from writing a
		// refusal record per pass for a rung the ceiling puts out of reach.
		if rungRank(ceiling) < rungRank(step.rung) {
			continue
		}
		row, ok := edgeByTrigger(step.trigger)
		if !ok || row.To != step.to {
			// The table no longer carries this rung row: the driver has nothing
			// to drive, and inventing a transition is exactly what it must not do.
			continue
		}
		next, err := l.Advance(ctx, incID, Transition{Seq: seq, From: state, To: step.to, Trigger: row.ID})
		if err == nil {
			if next.State == state {
				// The seq deduped the call: nothing moved, so nothing advanced.
				return false, nil
			}
			return true, nil
		}
		var le *Error
		if asLadderError(err, &le) {
			switch le.Code {
			case types.CodeLadder010, types.CodeLadder014:
				// The kill-switch (TROUBLE-LADDER-010) and an open scope breaker
				// (TROUBLE-LADDER-014) hold the whole incident: a lower rung is
				// not a way around either. The refusal is already on the ledger.
				return false, nil
			}
		}
		// The rung is unavailable (ceiling, no port, budget): the next rung in
		// ladder order is the table's own answer. Any transition that is not one
		// of the two holds above cannot mutate the incident.
	}
	return false, nil
}

// asLadderError is errors.As localised to *Error (kept here so drive.go names no
// error-wrapping helper the rest of the package does not use).
func asLadderError(err error, target **Error) bool {
	for err != nil {
		if e, ok := err.(*Error); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
