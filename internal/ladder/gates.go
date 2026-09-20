package ladder

import (
	"context"
	"sort"
	"strings"

	"github.com/trouble-agent/trouble/internal/types"
)

// Gates returns the current autonomy gates (§3.11). The ladder owns the mode
// matrix, so every consumer reads it here.
func (l *Ladder) Gates() types.AutonomyGates {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.gate
}

// SetGates replaces the gates and records who did it. A mode change takes effect
// at the next checkpoint, never mid-call, and a downgrade never aborts an
// in-flight mutating call (§3.11).
func (l *Ladder) SetGates(ctx context.Context, g types.AutonomyGates, actor types.Actor) (types.AutonomyGates, error) {
	l.mu.Lock()
	prev := l.gate
	if g.Mode == "" {
		g.Mode = types.AutoShadow
	}
	if !g.Mode.Valid() {
		l.mu.Unlock()
		return prev, newErr(types.CodeLadder011, reasonGateDenied, "unknown autonomy mode %q", string(g.Mode))
	}
	g.ChangedBy = actor.ID
	g.ChangedTS = types.FormatUTC(l.now())
	l.gate = g
	st := &incState{Inc: types.Incident{ID: "", Sig: ""}, Sigs: []string{}}
	l.mu.Unlock()

	// The change is persisted through the ledger (SPEC-12 owns the `config`
	// record; the ladder records the gates it will apply).
	_, err := l.deps.Ledger.Append(ctx, types.KConfig, "", "", map[string]any{
		"gates":      g,
		"changed_by": actor.ID,
		"previous":   prev,
	})
	_ = st
	if err != nil {
		return prev, wrapErr(types.CodeLadder015, "", err)
	}
	return g, nil
}

// Granted reports whether a (rule, module, sig) triple is granted (§3.11). The
// whole entry must match exactly: no prefix or glob semantics.
func (l *Ladder) Granted(ctx context.Context, rule, module, sig string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.gate.Mode != types.AutoAssisted {
		return l.gate.Mode == types.AutoFull || !isMutatingModule(module)
	}
	short := sig
	if len(short) > 16 {
		short = short[:16]
	}
	for _, g := range l.gate.Grants {
		if g == rule+"|"+module || g == rule+"|"+module+"|"+short {
			return true
		}
	}
	return false
}

// GrantFor returns the grant token that authorizes a module for a rule, or ""
// when none does (the ladder's escalation body carries it).
func (l *Ladder) GrantFor(ctx context.Context, rule, module, sig string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	short := sig
	if len(short) > 16 {
		short = short[:16]
	}
	for _, g := range l.gate.Grants {
		if g == rule+"|"+module+"|"+short {
			return g
		}
	}
	for _, g := range l.gate.Grants {
		if g == rule+"|"+module {
			return g
		}
	}
	return ""
}

func isMutatingModule(module string) bool {
	prefix, _, _ := strings.Cut(module, ".")
	switch prefix {
	case "config":
		return module != "config.get" && module != "config.list"
	case "service":
		return module != "service.status"
	case "file", "flow":
		return module != "file.read"
	case "proc":
		return false
	}
	return false
}

// Quarantine is the only manual state exit: quarantined is terminal and this is
// how an operator parks an incident for a human (§3.3 R11).
func (l *Ladder) Quarantine(ctx context.Context, incID, reason string) error {
	l.mu.Lock()
	st, ok := l.incs[incID]
	if !ok {
		l.mu.Unlock()
		return newErr(types.CodeLadder012, "", "incident %s is not in the index", incID)
	}
	from := st.Inc.State
	if from == types.StQuarantined {
		l.mu.Unlock()
		return newErr(types.CodeLadder001, reasonQuarantined, "incident %s is already quarantined", incID)
	}
	st.Inc.State = types.StQuarantined
	st.QuarantineReason = reason
	delete(l.openBySig, st.Inc.Sig)
	payload := map[string]any{
		"transition":        "quarantine",
		"from":              string(from),
		"to":                string(types.StQuarantined),
		"quarantine_reason": reason,
		"refused":           false,
		"sigs":              st.Sigs,
	}
	err := l.appendIncident(ctx, st, payload)
	l.mu.Unlock()
	if err != nil {
		return wrapErr(types.CodeLadder015, "", err)
	}
	return nil
}

// Unquarantine writes the human-actor record that re-enters the incident at
// `recorded` (§3.3 R11).
func (l *Ladder) Unquarantine(ctx context.Context, incID, reason string, actor types.Actor) error {
	if actor.Kind != types.ActorHuman {
		return newErr(types.CodeLadder017, reasonQuarantined, "only a human actor may unquarantine (got %q)", string(actor.Kind))
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.incs[incID]
	if !ok {
		return newErr(types.CodeLadder012, "", "incident %s is not in the index", incID)
	}
	if st.Inc.State != types.StQuarantined {
		return newErr(types.CodeLadder001, reasonQuarantined, "incident %s is not quarantined", incID)
	}
	st.Inc.State = types.StRecorded
	st.Inc.UpdatedTS = types.FormatUTC(l.now())
	l.openBySig[st.Inc.Sig] = incID
	if st.InKey != "" {
		l.openByInKey[st.InKey] = incID
	}
	return l.appendIncident(ctx, st, map[string]any{
		"transition":       "unquarantine",
		"from":             string(types.StQuarantined),
		"to":               string(types.StRecorded),
		"state":            string(types.StRecorded),
		"unquarantined_by": actor.ID,
		"reason":           reason,
		"sigs":             st.Sigs,
	})
}

// Pending returns the refused-but-remembered transitions (§3.4). The set is
// O(open incidents), bounded by max_open_incidents.
func (l *Ladder) Pending(ctx context.Context) ([]PendingItem, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Rebuild the set by folding `refused:true,pending:true` rows newer than
	// each incident's last accepted transition: the pending set is derived, not
	// a queue.
	out := make([]PendingItem, 0, len(l.pending))
	for _, p := range l.pending {
		st := l.incs[p.Inc]
		if st == nil {
			continue
		}
		if st.Inc.State != p.From && !st.Waiting {
			// The incident moved on: the row is stale.
			continue
		}
		out = append(out, PendingItem{Inc: p.Inc, From: st.Inc.State, To: p.To, SinceTS: p.SinceTS, Reason: p.Reason})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Inc < out[j].Inc })
	return out, nil
}

// ResumePending re-evaluates the pending transitions under the current gates and
// runs the ones the gates now allow. There is no queue of commands, so resume is
// deterministic and idempotent (§3.4.6).
func (l *Ladder) ResumePending(ctx context.Context) ([]string, error) {
	items, err := l.Pending(ctx)
	if err != nil {
		return nil, err
	}
	resumed := make([]string, 0, len(items))
	for _, p := range items {
		if l.gate.KillSwitch {
			break
		}
		if _, aerr := l.Advance(ctx, p.Inc, Transition{From: p.From, To: p.To}); aerr != nil {
			continue
		}
		l.mu.Lock()
		delete(l.pending, p.Inc)
		l.mu.Unlock()
		resumed = append(resumed, p.Inc)
	}
	return resumed, nil
}

// SetKillSwitch is the operator's kill-switch entry point (§3.4). Setting it is
// persisted and re-derived from the last config record at boot: a restart never
// silently clears it.
func (l *Ladder) SetKillSwitch(ctx context.Context, on bool, actor types.Actor) (types.AutonomyGates, error) {
	g := l.Gates()
	g.KillSwitch = on
	return l.SetGates(ctx, g, actor)
}
