package ladder

import (
	"context"
	"sort"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// breakerState is one storm breaker's live state (SPEC-05 §3.8). A suppression
// window IS an open breaker with scope sig:<sig> and reason suppression_window.
type breakerState struct {
	Breaker    types.Breaker
	Trips      int
	Opened     time.Time
	Duration   time.Duration
	HalfOpen   bool
	Folds      int64
	Codes      []string
	Reopens24h []time.Time
	ProbeInc   string
	PreviousS  int
}

const (
	scopeGlobal = "global"
	scopeRule   = "rule:"
	scopeSig    = "sig:"
	scopeSource = "source:"
)

// budgetKey is the per-host, per-UTC-day, per-class counter key.
func budgetKey(hostID, day, class string) string { return hostID + "|" + day + "|" + class }

func dayKey(t time.Time) string { return t.UTC().Format("2006-01-02") }

// Budget reports the per-day counters and which classes are exhausted (§3.7).
func (l *Ladder) Budget(ctx context.Context) (types.BudgetState, error) {
	now := l.now()
	day := dayKey(now)
	l.mu.Lock()
	defer l.mu.Unlock()
	out := types.BudgetState{
		HostID:   l.hostID,
		Day:      day,
		ResetsTS: types.FormatUTC(time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Add(24 * time.Hour)),
		Limits: map[string]int64{
			types.BudgetAgentRuns:    l.cfg.AgentRunsPerDay,
			types.BudgetPlayRuns:     l.cfg.PlayRunsPerDay,
			types.BudgetResearchReqs: l.cfg.ResearchPerDay,
			types.BudgetSpawns:       l.cfg.SpawnsPerDay,
		},
		Used: map[string]int64{
			types.BudgetAgentRuns:    l.budget[budgetKey(l.hostID, day, types.BudgetAgentRuns)],
			types.BudgetPlayRuns:     l.budget[budgetKey(l.hostID, day, types.BudgetPlayRuns)],
			types.BudgetResearchReqs: l.budget[budgetKey(l.hostID, day, types.BudgetResearchReqs)],
			types.BudgetSpawns:       l.budget[budgetKey(l.hostID, day, types.BudgetSpawns)],
		},
		Exhausted: []string{},
	}
	for class, limit := range out.Limits {
		if limit <= 0 {
			continue
		}
		if out.Used[class] >= limit {
			out.Exhausted = append(out.Exhausted, class)
		}
	}
	sort.Strings(out.Exhausted)
	return out, nil
}

// budgetLeftLocked reports whether a class still has budget on the current day.
func (l *Ladder) budgetLeftLocked(class string) bool {
	limit := l.limitFor(class)
	if limit <= 0 {
		return true
	}
	day := dayKey(l.now())
	return l.budget[budgetKey(l.hostID, day, class)] < limit
}

func (l *Ladder) limitFor(class string) int64 {
	switch class {
	case types.BudgetAgentRuns:
		return l.cfg.AgentRunsPerDay
	case types.BudgetPlayRuns:
		return l.cfg.PlayRunsPerDay
	case types.BudgetResearchReqs:
		return l.cfg.ResearchPerDay
	case types.BudgetSpawns:
		return l.cfg.SpawnsPerDay
	}
	return 0
}

// chargeLocked consumes one unit of a class. A rollover mid-run never aborts the
// run: the run is charged to the day it started (§3.7).
func (l *Ladder) chargeLocked(class string, at time.Time) {
	l.budget[budgetKey(l.hostID, dayKey(at), class)]++
}

// ---- breakers ----

// Breaker returns one scope's breaker (§3.8).
func (l *Ladder) Breaker(ctx context.Context, scope string) (types.Breaker, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.breakers[scope]
	if !ok {
		return types.Breaker{}, false
	}
	return b.Breaker, true
}

// TripBreaker opens a breaker by hand (used by the operator path and by tests).
func (l *Ladder) TripBreaker(ctx context.Context, scope, reason string) (types.Breaker, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	d := l.openFor(scope)
	b := l.breakerLocked(scope)
	l.openBreakerLockedWithReason(b, reason, d)
	if err := l.appendBreaker(ctx, b, ""); err != nil {
		return b.Breaker, wrapErr(types.CodeLadder015, "", err)
	}
	return b.Breaker, nil
}

// Suppress opens a suppression window for a sig (§3.8); a suppression window is
// an open breaker with scope sig:<sig> and reason suppression_window.
func (l *Ladder) Suppress(ctx context.Context, sig string, d types.Duration, reason string) (types.Breaker, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if reason == "" {
		reason = reasonSuppressionWindow
	}
	scope := scopeSig + sig
	b := l.breakerLocked(scope)
	dur := d.Std()
	if dur <= 0 {
		dur = l.cfg.BreakerSigOpen.Std()
	}
	l.openBreakerLockedWithReason(b, reason, dur)
	l.suppress[sig] = suppression{Until: b.Breaker.OpenUntil, Reason: reason, Opened: types.FormatUTC(l.now())}
	if err := l.appendBreaker(ctx, b, ""); err != nil {
		return b.Breaker, wrapErr(types.CodeLadder015, "", err)
	}
	return b.Breaker, nil
}

func (l *Ladder) breakerLocked(scope string) *breakerState {
	b, ok := l.breakers[scope]
	if !ok {
		b = &breakerState{Breaker: types.Breaker{Scope: scope, State: types.BreakerClosed}}
		l.breakers[scope] = b
	}
	return b
}

func (l *Ladder) openFor(scope string) time.Duration {
	switch {
	case scope == scopeGlobal:
		return l.cfg.BreakerGlobalOpen.Std()
	case len(scope) > len(scopeRule) && scope[:len(scopeRule)] == scopeRule:
		return l.cfg.BreakerRuleOpen.Std()
	case len(scope) > len(scopeSource) && scope[:len(scopeSource)] == scopeSource:
		return l.cfg.BreakerSourceOpen.Std()
	default:
		return l.cfg.BreakerSigOpen.Std()
	}
}

func (l *Ladder) openBreakerLockedWithReason(b *breakerState, reason string, d time.Duration) {
	now := l.now()
	// A re-open doubles the previous duration, capped at 4h (§3.8).
	if b.Breaker.State == types.BreakerOpen || b.Breaker.State == types.BreakerHalfOpen {
		if b.Duration > 0 {
			d = 2 * b.Duration
		}
	}
	if max := l.cfg.BreakerMaxOpen.Std(); max > 0 && d > max {
		d = max
	}
	b.Trips++
	b.Duration = d
	b.Opened = now
	b.Breaker.State = types.BreakerOpen
	b.Breaker.OpenedTS = types.FormatUTC(now)
	b.Breaker.OpenUntil = types.FormatUTC(now.Add(d))
	b.Breaker.Reason = reason
	b.Breaker.Trips = b.Trips
	b.PreviousS = int(d.Seconds())
	b.HalfOpen = false
	b.Reopens24h = append(b.Reopens24h, now)
}

// breakersFor derives the scopes an arrival is subject to (§3.8).
func (l *Ladder) breakersFor(sig string, obs Observation) []string {
	return []string{scopeSig + sig, scopeRule + obs.Rule, scopeSource + string(obs.Source), scopeGlobal}
}

// openBreakerLocked reports whether any scope holds new work, newest-open first.
func (l *Ladder) openBreakerLocked(sig string, obs Observation) (string, bool) {
	now := l.now()
	for _, scope := range l.breakersFor(sig, obs) {
		b, ok := l.breakers[scope]
		if !ok {
			continue
		}
		switch b.Breaker.State {
		case types.BreakerOpen:
			if until, err := types.ParseUTC(b.Breaker.OpenUntil); err == nil && now.Before(until) {
				return scope, true
			}
			// The window elapsed: exactly one incident is admitted as a probe.
			b.Breaker.State = types.BreakerHalfOpen
			b.HalfOpen = true
			return "", false
		case types.BreakerHalfOpen:
			// A probe is already in flight: hold the rest.
			return scope, true
		}
	}
	return "", false
}

// observeBreakerClosuresLocked closes a half-open breaker whose probe window
// passed, and re-opens it when the probe failed (§3.8).
func (l *Ladder) observeBreakerClosuresLocked(ctx context.Context, st *incState) {
	for _, scope := range []string{scopeSig + st.Inc.Sig, scopeRule + st.Rule, scopeSource + string(st.Inc.Severity)} {
		b, ok := l.breakers[scope]
		if !ok || b.Breaker.State != types.BreakerHalfOpen {
			continue
		}
		b.Trips = 0
		b.Breaker.State = types.BreakerClosed
		b.Breaker.OpenUntil = ""
		b.Breaker.Reason = ""
		b.HalfOpen = false
		_ = l.appendBreaker(ctx, b, "")
	}
}

// breakerEscalationLocked raises the source severity and files an issue when a
// breaker re-opens 4 times in 24h (§3.8: "the human path is never fully
// suppressed").
func (l *Ladder) breakerEscalationLocked(ctx context.Context, b *breakerState, inc types.Incident) {
	cutoff := l.now().Add(-24 * time.Hour)
	recent := 0
	for _, t := range b.Reopens24h {
		if t.After(cutoff) {
			recent++
		}
	}
	b.Reopens24h = b.Reopens24h[len(b.Reopens24h):]
	if recent < 4 {
		return
	}
	if l.deps.Outlets != nil {
		_, _ = l.deps.Outlets.EnsureIssue(ctx, inc)
		_ = l.deps.Outlets.Comment(ctx, inc, "breaker "+b.Breaker.Scope+" re-opened 4 times in 24h")
	}
	if l.deps.Notify != nil {
		_ = l.deps.Notify.Emit(ctx, inc, "breaker_flapping", map[string]any{"scope": b.Breaker.Scope, "trips": b.Tripper()})
	}
}

// Tripper is the trip count for logging.
func (b *breakerState) Tripper() int { return b.Trips }

// ---- suppression windows ----

// suppressedLocked reports whether a sig is inside a suppression window.
func (l *Ladder) suppressedLocked(sig string) bool {
	_, _, ok := l.suppressionWindowLocked(sig)
	return ok
}

// suppressionWindowLocked returns the open window for a sig, closing an elapsed
// one and applying quiet-close when it was silent (§3.8).
func (l *Ladder) suppressionWindowLocked(sig string) (string, string, bool) {
	sup, ok := l.suppress[sig]
	if !ok {
		return "", "", false
	}
	until, err := types.ParseUTC(sup.Until)
	if err == nil && l.now().Before(until) {
		return sup.Until, sup.Reason, true
	}
	delete(l.suppress, sig)
	return "", "", false
}

// quietCloseLocked decides whether an elapsed suppression window resolves
// silently: quiet is only accepted when it was OBSERVED (§3.8).
func (l *Ladder) quietCloseLocked(ctx context.Context, st *incState) {
	sup, ok := l.suppress[st.Inc.Sig]
	if !ok {
		return
	}
	until, err := types.ParseUTC(sup.Until)
	if err != nil {
		return
	}
	if l.now().Before(until) {
		return
	}
	if !l.cfg.QuietClose {
		delete(l.suppress, st.Inc.Sig)
		return
	}
	if st.SuppressedCount > 0 {
		// Not quiet: the window collects evidence and moves back to recorded.
		delete(l.suppress, st.Inc.Sig)
		st.Inc.State = types.StRecorded
		st.Inc.UpdatedTS = types.FormatUTC(l.now())
		_ = l.appendIncident(ctx, st, map[string]any{
			"transition":       "T44",
			"from":             string(types.StSuppressed),
			"to":               string(types.StRecorded),
			"suppress_until":   sup.Until,
			"suppressed_count": st.SuppressedCount,
			"rung":             string(st.Inc.Rung),
		})
		return
	}
	// Quiet-close: zero events for the whole window AND a landed canary.
	ev, _ := l.verifyLocked(ctx, st, l.windowFor(st), canaryOpts{quietClose: true})
	if ev.Result == types.VerifyPassed {
		st.Evidence = &ev
		st.Inc.Evidence = &ev
		st.Inc.State = types.StResolved
		st.Inc.ResolvedTS = types.FormatUTC(l.now())
		st.Inc.UpdatedTS = st.Inc.ResolvedTS
		_ = l.appendVerify(ctx, st, ev, sup.Until)
		_ = l.appendIncident(ctx, st, map[string]any{
			"transition":       "T45",
			"from":             string(types.StSuppressed),
			"to":               string(types.StResolved),
			"state":            string(types.StResolved),
			"quiet_close":      true,
			"resolved_ts":      st.Inc.ResolvedTS,
			"window":           l.windowPayload(st.Window),
			"evidence_ref":     st.Inc.ID,
			"suppressed_count": 0,
		})
	}
	delete(l.suppress, st.Inc.Sig)
}

// suppressionCycleOpenedLocked counts cycles and opens the sig breaker after the
// third cycle in 24h (§3.8).
func (l *Ladder) suppressionCycleOpenedLocked(ctx context.Context, sig string) {
	sup := l.suppress[sig]
	sup.Cycles++
	l.suppress[sig] = sup
	if sup.Cycles < l.cfg.BreakerSigPer24h {
		return
	}
	scope := scopeSig + sig
	b := l.breakerLocked(scope)
	l.openBreakerLockedWithReason(b, reasonSuppressionWindow, l.cfg.BreakerSigOpen.Std())
	_ = l.appendBreaker(ctx, b, "")
}

// ---- record writers ----

// appendIncident writes one `incident`-kind record (SPEC-05 §3.12).
func (l *Ladder) appendIncident(ctx context.Context, st *incState, payload map[string]any) error {
	if payload == nil {
		payload = map[string]any{}
	}
	if _, ok := payload["sigs"]; !ok {
		payload["sigs"] = st.Sigs
	}
	if _, ok := payload["severity"]; !ok {
		payload["severity"] = string(st.Inc.Severity)
	}
	if _, ok := payload["illegal_transitions"]; !ok {
		payload["illegal_transitions"] = st.IllegalTransitions
	}
	if _, ok := payload["play_runs"]; !ok {
		payload["play_runs"] = st.PlayRuns
	}
	if _, ok := payload["agent_runs"]; !ok {
		payload["agent_runs"] = st.AgentRuns
	}
	if _, ok := payload["strikes"]; !ok {
		payload["strikes"] = len(st.Strikes)
	}
	if _, ok := payload["reopen_count"]; !ok {
		payload["reopen_count"] = st.Inc.ReopenCount
	}
	if _, ok := payload["entry_rung"]; !ok {
		payload["entry_rung"] = string(st.Inc.EntryRung)
	}
	if _, ok := payload["rung"]; !ok {
		payload["rung"] = string(st.Inc.Rung)
	}
	if _, ok := payload["rule"]; !ok && st.Rule != "" {
		payload["rule"] = st.Rule
	}
	if _, ok := payload["inKey"]; !ok && st.InKey != "" {
		payload["inKey"] = st.InKey
	}
	_, err := l.deps.Ledger.Append(ctx, types.KIncident, st.Inc.Sig, st.Inc.ID, payload)
	return err
}

// appendBreaker writes one `breaker`-kind record (§3.12).
func (l *Ladder) appendBreaker(ctx context.Context, b *breakerState, probeInc string) error {
	_, err := l.deps.Ledger.Append(ctx, types.KBreaker, "", "", map[string]any{
		"scope":               b.Breaker.Scope,
		"state":               string(b.Breaker.State),
		"trips":               b.Trips,
		"opened_ts":           b.Breaker.OpenedTS,
		"open_until":          b.Breaker.OpenUntil,
		"reason":              b.Breaker.Reason,
		"probe_inc":           probeInc,
		"previous_duration_s": b.PreviousS,
	})
	return err
}

// appendVerify writes one `verify`-kind record (§3.12).
func (l *Ladder) appendVerify(ctx context.Context, st *incState, ev types.Evidence, gapRefs ...string) error {
	payload := map[string]any{
		"evidence":      ev,
		"gap_refs":      gapRefs,
		"invalid_count": st.InvalidCount,
		"window":        l.windowPayload(st.Window),
		"canary": map[string]any{
			"id":          ev.CanaryID,
			"path":        "ladder",
			"observed_ts": ev.TSWindowEnd,
		},
	}
	if ev.Result == types.VerifyInvalid && len(ev.SourcesMissing) > 0 && len(gapRefs) == 0 {
		payload["gap_missing"] = true
	}
	_, err := l.deps.Ledger.Append(ctx, types.KVerify, st.Inc.Sig, st.Inc.ID, payload)
	return err
}

// comment posts a cross-reference comment through the outlets (never a new
// issue or board row — §3.9).
func (l *Ladder) comment(ctx context.Context, st *incState, body string) {
	if l.deps.Outlets == nil {
		return
	}
	inc := st.Inc
	if st.IssueID != "" {
		inc.IssueID = st.IssueID
	}
	if st.TaskID != "" {
		inc.TaskID = st.TaskID
	}
	_ = l.deps.Outlets.Comment(ctx, inc, body)
}

func (l *Ladder) windowPayload(w *Window) map[string]any {
	if w == nil {
		return map[string]any{}
	}
	return map[string]any{"start": w.Start, "end": w.End, "verify_window": string(w.S)}
}
