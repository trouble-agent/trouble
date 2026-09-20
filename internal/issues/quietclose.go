package issues

import (
	"context"
	"fmt"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// Reopener is the optional driver capability the §3.11 reopen path needs. The
// frozen contract is four methods (SPEC-TYPES §3.11); reopening an issue is the
// one state transition §3.9.5 and §3.11 require that none of the four expresses,
// so it is an optional interface the desk type-asserts rather than a fifth method
// that every future driver would have to implement.
type Reopener interface {
	Reopen(ctx context.Context, ref types.IssueRef, body string) (types.IssueRef, error)
}

// reopen reopens the same issue for a recurrence after a close (§3.11): fold
// comment first, then the state transition. When the driver refuses the reopen
// the desk retries (the retry loop) and then files a superseding issue whose body
// carries `supersedes: <old external id>` plus the same sig marker, so exactly
// one issue is open per sig in every branch.
func (d *Desk) reopen(ctx context.Context, drv types.IssueDriver, dc types.IssueDriverConfig,
	a *anchorEntry, inc types.Incident, ev Evidence, idem string) (types.IssueRef, error) {

	idemKey := commentIdem(a.ref.ID, "reopen|"+inc.ID)
	line := fmt.Sprintf("reopened %s · %s · occurrences %d · reason: recurrence after close",
		blankDash(ev.Group.LastSeenTS), inc.ID, ev.Group.Count)
	body, _, err := d.scrubBody(ctx, line+"\n\n"+IdemMarker(idemKey), a.project)
	if err != nil {
		return a.ref, err
	}
	payload := map[string]any{
		"op": "reopen", "driver": a.driver, "sig": inc.Sig, "inc": inc.ID, "project": a.project,
		"idem_key": idemKey, "external_id": a.ref.ExternalID, "result": "pending", "state": "",
		"error_code": "", "retryable": false, "attempt": 1,
	}

	reopener, canReopen := drv.(Reopener)
	if canReopen {
		var out types.IssueRef
		attempts, callErr := d.withRetry(ctx, a.driver, dc, "reopen", idemKey, func(c context.Context) error {
			r, err := reopener.Reopen(c, a.ref, body)
			if err != nil {
				return err
			}
			out = r
			return nil
		})
		payload["attempt"] = attempts
		if callErr == nil {
			payload["result"] = "reopened"
			payload["state"] = types.IssueOpen
			out.Driver = a.driver
			out.Sig = inc.Sig
			out.State = types.IssueOpen
			if out.ID == "" {
				out.ID = a.ref.ID
			}
			if out.ExternalID == "" {
				out.ExternalID = a.ref.ExternalID
			}
			_, _ = d.record(ctx, types.KIssue, inc.Sig, inc.ID, payload)
			d.mu.Lock()
			a.ref = out
			a.reopenCount++
			a.updatedTS = types.FormatUTC(d.now())
			d.handled[idemKey] = true
			d.mu.Unlock()
			return out, nil
		}
		e := asError(callErr)
		payload["error_code"] = string(e.Code)
		payload["retryable"] = e.Retryable
		payload["result"] = "refused"
		// §3.11: three attempts, then a superseding issue (new external id, same sig).
		if e.Code == types.CodeIssues008 {
			_, _ = d.record(ctx, types.KIssue, inc.Sig, inc.ID, payload)
			return d.supersede(ctx, drv, dc, a, inc, ev, idem)
		}
		_, _ = d.record(ctx, types.KIssue, inc.Sig, inc.ID, payload)
		return a.ref, e
	}
	_, _ = d.record(ctx, types.KIssue, inc.Sig, inc.ID, payload)
	return d.supersede(ctx, drv, dc, a, inc, ev, idem)
}

// supersede files a fresh issue for a sig whose previous issue cannot be
// reopened. It is the last branch of §3.11's "exactly one open issue per sig".
func (d *Desk) supersede(ctx context.Context, drv types.IssueDriver, dc types.IssueDriverConfig,
	a *anchorEntry, inc types.Incident, ev Evidence, idem string) (types.IssueRef, error) {

	title := d.TitleOf(inc, ev)
	body, _, err := d.scrubBody(ctx, d.BodyOf(inc, ev, a.driver)+
		fmt.Sprintf("\nsupersedes: %s\n", a.ref.ExternalID), a.project)
	if err != nil {
		return a.ref, err
	}
	req := types.EnsureBySigRequest{
		Sig: inc.Sig, Title: title, Body: body, Labels: d.labels(dc, inc, ev),
		DedupWindow: d.dedupWindowDur(dc), Severity: inc.Severity,
	}
	var resp types.EnsureBySigResponse
	_, callErr := d.withRetry(ctx, a.driver, dc, "reopen", idem, func(c context.Context) error {
		out, err := drv.EnsureBySig(c, req)
		if err != nil {
			return err
		}
		resp = out
		return nil
	})
	if callErr != nil {
		e := asError(callErr)
		if e.Retryable {
			d.spoolEnsure(ctx, a.driver, drv, inc, req, idem, a.project, "", e, 1)
		}
		return a.ref, e
	}
	ref := resp.Ref
	if ref.ID == "" {
		ref.ID = types.NewID(types.PIss)
	}
	ref.Driver = a.driver
	ref.Sig = inc.Sig
	ref.State = types.IssueOpen
	if ref.CreatedTS == "" {
		ref.CreatedTS = types.FormatUTC(d.now())
	}
	ref.UpdatedTS = types.FormatUTC(d.now())
	_, _ = d.record(ctx, types.KIssue, inc.Sig, inc.ID, map[string]any{
		"op": "reopen", "driver": a.driver, "sig": inc.Sig, "inc": inc.ID, "project": a.project,
		"idem_key": idem, "idem": idem, "result": "superseded", "state": types.IssueOpen,
		"external_id": ref.ExternalID, "url": ref.URL, "supersedes": a.ref.ExternalID,
		"attempt": 1, "http_status": 200, "error_code": "", "retryable": false,
	})
	d.mu.Lock()
	a.ref = ref
	a.reopenCount++
	a.updatedTS = types.FormatUTC(d.now())
	d.handled[idem] = true
	d.mu.Unlock()
	return ref, nil
}

// quietGate is one §3.11 quiet-close pre-condition's outcome.
type quietGate struct {
	Name string
	OK   bool
	Why  string
}

// SweepQuietClose closes every issue whose sig has been quiet for quiet_close
// after resolution (§3.11). All five gates must hold; a gate that cannot be
// evaluated is a deferral, never a drop.
func (d *Desk) SweepQuietClose(ctx context.Context, now time.Time) (int, error) {
	quiet := d.cfg.QuietClose.Std()
	if quiet <= 0 {
		quiet = 24 * time.Hour
	}
	d.mu.Lock()
	entries := make([]*anchorEntry, 0, len(d.anchors))
	for _, a := range d.anchors {
		entries = append(entries, a)
	}
	d.mu.Unlock()

	closed := 0
	var firstErr error
	for _, a := range entries {
		if a.ref.ExternalID == "" || a.ref.State == types.IssueClosed {
			continue // nothing to close, or already closed
		}
		if d.driverFailed(a.driver) {
			continue // gate 5: deferred, re-tried on the next sweep
		}
		gates, inc, deferral := d.quietGates(ctx, a, now, quiet)
		if deferral {
			continue
		}
		ok := true
		for _, g := range gates {
			if !g.OK {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		if _, err := d.closeQuiet(ctx, a, inc, quiet); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		closed++
	}
	return closed, firstErr
}

// quietGates evaluates gates 1–4 (gate 5 is the driver-health check, handled by
// the caller because a failed driver defers rather than refuses).
func (d *Desk) quietGates(ctx context.Context, a *anchorEntry, now time.Time, quiet time.Duration) ([]quietGate, types.Incident, bool) {
	var inc types.Incident
	if d.deps.Incident != nil {
		got, ok, err := d.deps.Incident(ctx, a.sig)
		if err != nil || !ok {
			return nil, inc, true
		}
		inc = got
	} else {
		return nil, inc, true
	}
	if inc.State != types.StResolved {
		return []quietGate{{Name: "resolved", OK: false, Why: string(inc.State)}}, inc, false
	}
	resolved, err := types.ParseUTC(inc.ResolvedTS)
	if err != nil || now.Sub(resolved) < quiet {
		return []quietGate{{Name: "quiet_period", OK: false, Why: inc.ResolvedTS}}, inc, false
	}
	if inc.ResolvedTS == "" {
		return []quietGate{{Name: "quiet_period", OK: false, Why: "no resolved_ts"}}, inc, false
	}

	var group types.Group
	if d.deps.Group != nil {
		got, ok, err := d.deps.Group(ctx, a.sig)
		if err != nil || !ok {
			return nil, inc, true
		}
		group = got
	} else {
		return nil, inc, true
	}

	gates := make([]quietGate, 0, 4)
	// Gate 1: no event with the sig in the quiet period.
	lastSeen, err := types.ParseUTC(group.LastSeenTS)
	switch {
	case group.LastSeenTS == "":
		gates = append(gates, quietGate{Name: "no_recurrence", OK: false, Why: "no group watermark"})
	case err != nil:
		gates = append(gates, quietGate{Name: "no_recurrence", OK: false, Why: "unparsable last_seen"})
	default:
		gates = append(gates, quietGate{Name: "no_recurrence", OK: !lastSeen.After(resolved), Why: group.LastSeenTS})
	}
	// Gate 2: reopen count unchanged since resolution and the newest verification
	// evidence passed.
	reopenOK := inc.ReopenCount == a.reopenCount
	evidenceOK := inc.Evidence != nil && inc.Evidence.Result == types.VerifyPassed
	gates = append(gates, quietGate{Name: "reopen_count", OK: reopenOK,
		Why: fmt.Sprintf("%d vs %d", inc.ReopenCount, a.reopenCount)})
	gates = append(gates, quietGate{Name: "evidence_passed", OK: evidenceOK, Why: evidenceResult(inc)})
	// Gate 3: no open board row and no open research brief for the sig.
	rowOK := true
	if d.deps.OpenBoardRow != nil {
		open, err := d.deps.OpenBoardRow(ctx, a.sig)
		if err != nil {
			return nil, inc, true
		}
		rowOK = !open
	}
	briefOK := true
	if d.deps.OpenBrief != nil {
		open, err := d.deps.OpenBrief(ctx, a.sig)
		if err != nil {
			return nil, inc, true
		}
		briefOK = !open
	}
	gates = append(gates, quietGate{Name: "no_open_board_row", OK: rowOK})
	gates = append(gates, quietGate{Name: "no_open_brief", OK: briefOK})
	// Gate 4: not escalated, and a human-filed (manual) issue is never closed.
	gates = append(gates, quietGate{Name: "not_escalated", OK: inc.State != types.StEscalated})
	gates = append(gates, quietGate{Name: "not_manual", OK: !a.manual})
	return gates, inc, false
}

func evidenceResult(inc types.Incident) string {
	if inc.Evidence == nil {
		return "missing"
	}
	return string(inc.Evidence.Result)
}

// closeQuiet performs the close and its records (§3.11).
func (d *Desk) closeQuiet(ctx context.Context, a *anchorEntry, inc types.Incident, quiet time.Duration) (types.IssueRef, error) {
	drv, ok := d.drivers[a.driver]
	if !ok {
		return a.ref, newErr(types.CodeIssues003, ReasonUnknownDriver, 0, false, "driver %q is not enabled", a.driver)
	}
	dc := d.drvCfg[a.driver]
	idemKey := closeIdem(a.ref.ID, "resolved")
	ev := types.Evidence{}
	if inc.Evidence != nil {
		ev = *inc.Evidence
	}
	reason := CloseReason(inc, ev, d.cfg.QuietClose, d.nowSeq(), idemKey)
	var out types.IssueRef
	attempts, callErr := d.withRetry(ctx, a.driver, dc, "close", idemKey, func(c context.Context) error {
		r, err := drv.Close(c, a.ref, reason)
		if err != nil {
			return err
		}
		out = r
		return nil
	})
	payload := map[string]any{
		"op": "close", "driver": a.driver, "sig": a.sig, "inc": inc.ID, "project": a.project,
		"idem_key": idemKey, "reason": "resolved", "attempt": attempts, "result": "closed",
		"state": types.IssueClosed, "error_code": "", "retryable": false,
	}
	if callErr != nil {
		e := asError(callErr)
		payload["result"] = "failed"
		payload["error_code"] = string(e.Code)
		payload["retryable"] = e.Retryable
		payload["http_status"] = e.HTTPStatus
		if e.Retryable {
			d.spoolClose(ctx, a.driver, a.ref, idemKey, reason, a.project, attempts)
		}
		_, _ = d.record(ctx, types.KIssue, a.sig, inc.ID, payload)
		return a.ref, e
	}
	out.Driver = a.driver
	out.Sig = a.sig
	out.State = types.IssueClosed
	if out.ID == "" {
		out.ID = a.ref.ID
	}
	out.TaskID = a.ref.TaskID
	out.ResearchID = a.ref.ResearchID
	_, _ = d.record(ctx, types.KIssue, a.sig, inc.ID, payload)
	d.mu.Lock()
	a.ref = out
	a.updatedTS = types.FormatUTC(d.now())
	d.handled[idemKey] = true
	d.mu.Unlock()
	return out, nil
}

// MarkManual records that the anchor's issue was filed by a human and adopted
// into the desk (`manual=true`, §3.11 gate 4). It is the only way an anchor
// becomes uncloseable by the sweep.
func (d *Desk) MarkManual(driver, sig, project string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	a, ok := d.anchors[d.anchorKey(driver, sig, project)]
	if !ok {
		a = &anchorEntry{key: d.anchorKey(driver, sig, project), driver: driver, sig: sig, project: project}
		d.anchors[a.key] = a
	}
	a.manual = true
}

// QuietGateReport is the read-only diagnostic used by tests and by
// `trouble issues health --json` consumers.
func (d *Desk) QuietGateReport(ctx context.Context, driver, sig string, now time.Time) ([]quietGate, bool) {
	project := d.anchorProject(types.IssueRef{Sig: sig, Driver: driver})
	d.mu.Lock()
	a, ok := d.anchors[d.anchorKey(driver, sig, project)]
	d.mu.Unlock()
	if !ok {
		return nil, false
	}
	gates, _, deferral := d.quietGates(ctx, a, now, d.cfg.QuietClose.Std())
	return gates, deferral
}
