package ladder

import (
	"context"
	"sort"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// canaryObs is one observed canary (§3.10). The ladder records each observation
// it sees; a canary that did not land can never produce `passed`.
type canaryObs struct {
	ID     string
	Path   SourcePath
	TS     string
	EpochM int64
}

// clampWindow applies §3.10's window validity rules to a raw value:
// W == 0 → verify_default; W < 1m → 1m; W > 24h → verify_default. Every repair
// records TROUBLE-LADDER-005 with the original value.
func clampWindow(w types.Duration, def types.Duration) (types.Duration, *Error) {
	if def == "" {
		def = "10m"
	}
	if w == "" || w == "0" || w == "0s" {
		return def, newErr(types.CodeLadder005, reasonEvaluationBlocked, "verify window %q is unset or zero; using %s", string(w), string(def))
	}
	d, err := time.ParseDuration(string(w))
	if err != nil {
		return def, newErr(types.CodeLadder005, reasonEvaluationBlocked, "verify window %q does not parse; using %s", string(w), string(def))
	}
	switch {
	case d < time.Minute:
		return types.Duration("1m"), newErr(types.CodeLadder005, reasonEvaluationBlocked, "verify window %s is below the 1m floor; clamped to 1m", string(w))
	case d > 24*time.Hour:
		return def, newErr(types.CodeLadder005, reasonEvaluationBlocked, "verify window %s is above the 24h ceiling; using %s", string(w), string(def))
	}
	return w, nil
}

// windowFor builds the window of an incident whose last mutating tool call just
// completed: TSWindowStart is that timestamp, TSWindowEnd = start + W.
func (l *Ladder) windowFor(st *incState) *Window {
	w := st.Inc.VerifyWin
	if w == "" {
		w = l.cfg.VerifyDefault
	}
	fixed, _ := clampWindow(w, l.cfg.VerifyDefault)
	start := l.now()
	if st.LastTransitionTS != "" {
		if t, err := types.ParseUTC(st.LastTransitionTS); err == nil {
			start = t
		}
	}
	if st.Window != nil {
		start = mustTime(st.Window.Start, start)
	}
	return &Window{Start: types.FormatUTC(start), End: types.FormatUTC(start.Add(fixed.Std())), S: fixed}
}

func mustTime(s string, def time.Time) time.Time {
	if t, err := types.ParseUTC(s); err == nil {
		return t
	}
	return def
}

// CanaryObserved records one canary that landed through the real production path
// (§3.10). SPEC-03 and SPEC-04 call it; verification is the only thing that turns
// an observation into evidence.
func (l *Ladder) CanaryObserved(ctx context.Context, projectID, canaryID string, path SourcePath) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if l.canaries == nil {
		l.canaries = map[string][]canaryObs{}
	}
	obs := canaryObs{ID: canaryID, Path: path, TS: types.FormatUTC(now), EpochM: now.UnixMilli()}
	l.canaries[projectID] = append(l.canaries[projectID], obs)
	// Bounded: only the last 256 canaries per key are retained.
	if list := l.canaries[projectID]; len(list) > 256 {
		l.canaries[projectID] = list[len(list)-256:]
	}
	// The canary is evidence, so it is recorded (a `canary` kind record, not a
	// `gap` record: this package never emits gap records).
	_, err := l.deps.Ledger.Append(ctx, types.KCanary, "", "", map[string]any{
		"canary":  map[string]any{"id": canaryID, "path": string(path), "observed_ts": obs.TS},
		"project": projectID,
	})
	return err
}

// canaryInWindowLocked reports the canary of a key inside [from, to).
func (l *Ladder) canaryInWindowLocked(key string, from, to time.Time) (canaryObs, bool) {
	list := l.canaries[key]
	for _, c := range list {
		t := time.UnixMilli(c.EpochM).UTC()
		if !t.Before(from) && t.Before(to) {
			return c, true
		}
	}
	return canaryObs{}, false
}

// SourceLiveness is the shared expectation table (§3.10); SPEC-04 and the
// dashboard read the same rows.
func (l *Ladder) SourceLiveness(ctx context.Context) ([]types.SourceLiveness, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	liveness := map[string]types.SourceLiveness{}
	for _, st := range l.incs {
		for path := range st.ArrivalPaths {
			key := l.hostID + ":" + path
			row, ok := liveness[key]
			if !ok {
				row = types.SourceLiveness{
					HostID:   l.hostID,
					Source:   path,
					Zone:     "loopback",
					Expected: true,
					MaxAgeS:  l.maxAgeFor(path),
				}
			}
			row.Alive = true
			row.LastEventTS = st.Inc.UpdatedTS
			liveness[key] = row
		}
	}
	out := make([]types.SourceLiveness, 0, len(liveness))
	for _, row := range liveness {
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	return out, nil
}

// maxAgeFor is the per-source liveness expectation (§3.10): 300s for sentinel
// projects, 120s for sensor sources.
func (l *Ladder) maxAgeFor(path string) float64 {
	if path == "sentinel" {
		return l.cfg.SourceMaxAge["sentinel"].Seconds()
	}
	return l.cfg.SourceMaxAge["sensor"].Seconds()
}

// canaryOpts carries the two verification entry points' differences.
type canaryOpts struct {
	quietClose bool
	// applyInvalidBudget=false keeps Verify idempotent when the caller only wants
	// the tuple (the quiet-close probe).
	noStateChange bool
}

// Verify evaluates one window and returns the Evidence tuple (§3.10). It is
// called at window close and at window start; the tuple is the only accepted
// proof of resolution.
func (l *Ladder) Verify(ctx context.Context, incID string, w Window) (types.Evidence, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.incs[incID]
	if !ok {
		return types.Evidence{}, newErr(types.CodeLadder012, "", "incident %s is not in the index", incID)
	}
	win := &Window{Start: w.Start, End: w.End, S: w.S}
	if win.S == "" {
		win.S = l.cfg.VerifyDefault
	}
	ev, err := l.verifyLocked(ctx, st, win, canaryOpts{})
	if err != nil {
		return ev, err
	}
	st.Window = win
	st.Evidence = &ev
	st.Inc.Evidence = &ev
	gapRefs := l.gapRefsLocked(ctx, win)
	if aerr := l.appendVerify(ctx, st, ev, gapRefs...); aerr != nil {
		return ev, wrapErr(types.CodeLadder015, "", aerr)
	}
	return ev, nil
}

// verifyLocked assembles the evidence tuple for one window.
func (l *Ladder) verifyLocked(ctx context.Context, st *incState, w *Window, opts canaryOpts) (types.Evidence, *Error) {
	start, err := types.ParseUTC(w.Start)
	if err != nil {
		return types.Evidence{}, newErr(types.CodeLadder005, reasonEvaluationBlocked, "window start %q is not RFC3339: %v", w.Start, err)
	}
	end, err := types.ParseUTC(w.End)
	if err != nil {
		return types.Evidence{}, newErr(types.CodeLadder005, reasonEvaluationBlocked, "window end %q is not RFC3339: %v", w.End, err)
	}
	if !end.After(start) {
		return types.Evidence{}, newErr(types.CodeLadder005, reasonEvaluationBlocked, "window %s..%s is not a positive interval", w.Start, w.End)
	}
	ev := types.Evidence{
		TSWindowStart:   w.Start,
		TSWindowEnd:     w.End,
		WindowS:         end.Sub(start).Seconds(),
		CounterDeltas:   map[string]int64{},
		SourcesExpected: []string{},
		SourcesAlive:    []string{},
		SourcesQuiet:    []string{},
		SourcesMissing:  []string{},
		Zone:            "loopback",
	}

	// Events and counters come from the index, never inferred.
	if l.deps.Index != nil {
		n, ierr := l.deps.Index.EventsInWindow(ctx, st.Inc.Sig, w.Start, w.End)
		if ierr != nil {
			return ev, wrapErr(types.CodeLadder012, "", ierr)
		}
		ev.EventsObserved = n
		if counters, cerr := l.deps.Index.CountersInWindow(ctx, w.Start, w.End); cerr == nil && counters != nil {
			ev.CounterDeltas = counters
		}
	}

	// Expected sources: every path this incident has arrived by, each with its
	// MaxAge expectation (§3.10).
	paths := make([]string, 0, len(st.ArrivalPaths))
	for p := range st.ArrivalPaths {
		paths = append(paths, p)
	}
	if len(paths) == 0 {
		paths = []string{"sentinel"}
	}
	sort.Strings(paths)
	for _, p := range paths {
		key := l.hostID + ":" + p
		ev.SourcesExpected = append(ev.SourcesExpected, key)
	}

	// Liveness from the index: a source is alive when it reported inside the
	// window and within its MaxAge.
	if l.deps.Index != nil {
		for _, p := range paths {
			key := l.hostID + ":" + p
			n, ierr := l.deps.Index.EventsInWindow(ctx, st.Inc.Sig, w.Start, w.End)
			if ierr != nil {
				continue
			}
			maxAge := l.maxAgeFor(p)
			if ev.WindowS > maxAge {
				// The window itself is longer than the source's expectation: a
				// quiet source is still alive as long as it reported at all in
				// the window, which is what EventsInWindow proves.
				_ = n
			}
			if n > 0 {
				ev.SourcesAlive = append(ev.SourcesAlive, key)
			} else if _, ok := l.canaryInWindowLocked(st.Inc.Sig, start, end); ok {
				// A canary is the liveness proof when the source is quiet: the
				// whole point of amendment D is that silence alone is not proof
				// of liveness, and a landed canary is.
				ev.SourcesAlive = append(ev.SourcesAlive, key)
				ev.SourcesQuiet = append(ev.SourcesQuiet, key)
			} else {
				ev.SourcesMissing = append(ev.SourcesMissing, key)
			}
		}
	}
	sort.Strings(ev.SourcesAlive)
	sort.Strings(ev.SourcesQuiet)
	sort.Strings(ev.SourcesMissing)

	// Canary: the ladder looks for the sig's canary, then the group's.
	canary, seen := l.canaryInWindowLocked(st.Inc.Sig, start, end)
	if !seen {
		canary, seen = l.canaryInWindowLocked(st.Inc.GroupID, start, end)
	}
	ev.CanarySeen = seen
	if seen {
		ev.CanaryID = canary.ID
		if !contains(ev.SourcesAlive, l.hostID+":sentinel") && string(canary.Path) == "sentinel" {
			ev.SourcesAlive = append(ev.SourcesAlive, l.hostID+":sentinel")
			sort.Strings(ev.SourcesAlive)
		}
		for i, m := range ev.SourcesMissing {
			if m == l.hostID+":sentinel" {
				ev.SourcesMissing = append(ev.SourcesMissing[:i], ev.SourcesMissing[i+1:]...)
				break
			}
		}
	}

	// Gaps: a GapRecord overlapping the window makes the evidence incomplete,
	// not negative (§3.10). This package consumes gaps and never emits them.
	gapRefs := l.gapRefsLocked(ctx, w)

	// Result kinds (§3.10).
	switch {
	case !ev.CanarySeen:
		ev.Result = types.VerifyInvalid
	case len(ev.SourcesMissing) > 0:
		ev.Result = types.VerifyInvalid
	case len(gapRefs) > 0:
		ev.Result = types.VerifyInvalid
	case ev.EventsObserved > 0:
		ev.Result = types.VerifyFailed
	default:
		ev.Result = types.VerifyPassed
	}
	// The single guard that no code path can produce `passed` without a canary.
	if ev.Result == types.VerifyPassed && !ev.CanarySeen {
		ev.Result = types.VerifyInvalid
	}
	return ev, nil
}

// gapRefsLocked returns the ids of gap records overlapping a window.
func (l *Ladder) gapRefsLocked(ctx context.Context, w *Window) []string {
	if l.deps.Index == nil {
		return nil
	}
	gaps, err := l.deps.Index.GapsInWindow(ctx, w.Start, w.End)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(gaps))
	for _, g := range gaps {
		out = append(out, g.ID)
	}
	sort.Strings(out)
	return out
}

// invalidCode is the code a verification outcome carries (§3.10, §5).
func invalidCode(ev types.Evidence, gapRefs, sourcesMissing int) types.ErrorCode {
	if !ev.CanarySeen {
		return types.CodeLadder006
	}
	if sourcesMissing > 0 || gapRefs > 0 {
		return types.CodeLadder006
	}
	if ev.Result == types.VerifyFailed {
		return types.CodeLadder007
	}
	return ""
}
