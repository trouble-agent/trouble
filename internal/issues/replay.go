package issues

import (
	"context"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// Replay drains the spool (§3.7): replay order is (next_try_ts, ts, id), one
// in-flight operation per sig, every replayed operation carrying its original
// IdemKey so replay cannot create a duplicate issue or comment. A rate-limit
// signal stops the batch immediately and shifts the rest to the next tick.
func (d *Desk) Replay(ctx context.Context, budget int) (int, error) {
	if budget <= 0 {
		budget = d.cfg.ReplayBatch
		if budget <= 0 {
			budget = 100
		}
	}
	total := 0
	for _, driver := range d.order {
		if d.driverFailed(driver) {
			continue
		}
		entries, err := d.spool.List(driver)
		if err != nil {
			continue
		}
		replayed, stop := d.replayBatch(ctx, driver, entries, budget-total)
		total += replayed
		if stop || total >= budget {
			break
		}
	}
	return total, nil
}

func (d *Desk) replayBatch(ctx context.Context, driver string, entries []types.SpoolEntry, budget int) (int, bool) {
	drv, ok := d.drivers[driver]
	if !ok {
		return 0, true
	}
	replayed := 0
	for i := range entries {
		if replayed >= budget {
			return replayed, true
		}
		e := entries[i]
		if e.NextTryTS != "" {
			if ts, err := types.ParseUTC(e.NextTryTS); err == nil && d.now().Before(ts) {
				continue
			}
		}
		op, err := DecodePayload(e.Payload)
		if err != nil {
			d.dropEntry(ctx, driver, e, "corrupt")
			continue
		}
		// Per-sig single flight: a sig's operations can never reorder.
		key := d.anchorKey(driver, op.Sig, op.Project)
		lock := d.sigLock(key)
		if !lock.TryLock() {
			continue
		}
		err = d.dispatch(ctx, drv, op, e.IdemKey)
		lock.Unlock()
		if err != nil {
			ee := asError(err)
			e.Attempts++
			if d.cfg.MaxAttemptsPerOp > 0 && e.Attempts >= d.cfg.MaxAttemptsPerOp {
				d.dropEntry(ctx, driver, e, "attempts")
			} else {
				wait := d.backoffFor(d.drvCfg[driver], e.Attempts+1)
				e.NextTryTS = types.FormatUTC(d.now().Add(wait))
				_ = d.spool.Update(driver, e)
			}
			_, _ = d.record(ctx, types.KIssue, op.Sig, op.Inc, map[string]any{
				"op": "replay", "driver": driver, "sig": op.Sig, "inc": op.Inc, "idem_key": e.IdemKey,
				"result": "failed", "attempts": e.Attempts, "error_code": string(ee.Code),
				"retryable": ee.Retryable, "next_try_ts": e.NextTryTS,
			})
			if ee.Code == types.CodeIssues002 {
				// Rate-limited: shift the rest of this driver's batch to the next tick.
				return replayed, true
			}
			continue
		}
		_ = d.spool.Delete(driver, e.ID)
		d.mu.Lock()
		if g, ok := d.gaps[driver]; ok {
			g.replayed++
		}
		d.mu.Unlock()
		_, _ = d.record(ctx, types.KIssue, op.Sig, op.Inc, map[string]any{
			"op": "replay", "driver": driver, "sig": op.Sig, "inc": op.Inc, "idem_key": e.IdemKey,
			"result": "replayed", "attempts": e.Attempts + 1, "error_code": "", "retryable": false,
		})
		replayed++
	}
	return replayed, false
}

// dispatch re-runs one spooled operation under its original idempotency key.
func (d *Desk) dispatch(ctx context.Context, drv types.IssueDriver, op spoolOp, idemKey string) error {
	dc := d.drvCfg[op.Driver]
	switch op.Op {
	case "ensure":
		if op.Ensure == nil {
			return newErr(types.CodeIssues006, ReasonReplayFailed, 0, false, "ensure entry has no request")
		}
		// An ensure_unverified entry re-runs the read-back (the driver's ensure is
		// search-first), so an adopted create is never duplicated (edge case 1).
		var resp types.EnsureBySigResponse
		_, err := d.withRetry(ctx, op.Driver, dc, "ensure", idemKey, func(c context.Context) error {
			out, err := drv.EnsureBySig(c, *op.Ensure)
			if err != nil {
				return err
			}
			resp = out
			return nil
		})
		if err != nil {
			return err
		}
		ref := resp.Ref
		if ref.ID == "" {
			ref.ID = types.NewID(types.PIss)
		}
		ref.Driver = op.Driver
		ref.Sig = op.Sig
		ref.State = types.IssueOpen
		if ref.CreatedTS == "" {
			ref.CreatedTS = types.FormatUTC(d.now())
		}
		ref.UpdatedTS = types.FormatUTC(d.now())
		result := "replayed"
		if resp.Created {
			result = "replayed_created"
			d.caps.noteCreate(op.Sig, op.Project, d.now())
		}
		_, _ = d.record(ctx, types.KIssue, op.Sig, op.Inc, map[string]any{
			"op": "ensure", "driver": op.Driver, "sig": op.Sig, "inc": op.Inc, "project": op.Project,
			"idem_key": idemKey, "result": result, "created": resp.Created, "commented": resp.Commented,
			"external_id": ref.ExternalID, "url": ref.URL, "state": ref.State, "issue_id": ref.ID,
			"attempt": 1, "http_status": 200, "error_code": "", "retryable": false,
		})
		d.mu.Lock()
		a, ok := d.anchors[d.anchorKey(op.Driver, op.Sig, op.Project)]
		if !ok {
			a = &anchorEntry{key: d.anchorKey(op.Driver, op.Sig, op.Project), driver: op.Driver, sig: op.Sig, project: op.Project}
			d.anchors[a.key] = a
		}
		a.ref = ref
		a.ref.State = types.IssueOpen
		a.idem = idemKey
		a.updatedTS = types.FormatUTC(d.now())
		if a.createdTS == "" {
			a.createdTS = types.FormatUTC(d.now())
		}
		d.handled[idemKey] = true
		d.mu.Unlock()
		return nil
	case "comment":
		if op.Ref == nil {
			return newErr(types.CodeIssues006, ReasonReplayFailed, 0, false, "comment entry has no ref")
		}
		var out types.IssueRef
		_, err := d.withRetry(ctx, op.Driver, dc, "comment", idemKey, func(c context.Context) error {
			r, err := drv.Comment(c, *op.Ref, op.Body)
			if err != nil {
				return err
			}
			out = r
			return nil
		})
		if err != nil {
			return err
		}
		out.Driver = op.Driver
		out.Sig = op.Sig
		if out.ID == "" {
			out.ID = op.Ref.ID
		}
		_, _ = d.record(ctx, types.KIssue, op.Sig, op.Inc, map[string]any{
			"op": "comment", "driver": op.Driver, "sig": op.Sig, "inc": op.Inc, "project": op.Project,
			"idem_key": idemKey, "result": "replayed", "created": false, "commented": true,
			"external_id": out.ExternalID, "attempt": 1, "http_status": 200, "error_code": "", "retryable": false,
		})
		d.mu.Lock()
		if a, ok := d.anchors[d.anchorKey(op.Driver, op.Sig, op.Project)]; ok {
			a.ref = out
			a.updatedTS = types.FormatUTC(d.now())
		}
		d.handled[idemKey] = true
		d.mu.Unlock()
		return nil
	case "close":
		if op.Ref == nil {
			return newErr(types.CodeIssues006, ReasonReplayFailed, 0, false, "close entry has no ref")
		}
		var out types.IssueRef
		_, err := d.withRetry(ctx, op.Driver, dc, "close", idemKey, func(c context.Context) error {
			r, err := drv.Close(c, *op.Ref, op.Reason)
			if err != nil {
				return err
			}
			out = r
			return nil
		})
		if err != nil {
			return err
		}
		out.Driver = op.Driver
		out.Sig = op.Sig
		out.State = types.IssueClosed
		_, _ = d.record(ctx, types.KIssue, op.Sig, op.Inc, map[string]any{
			"op": "close", "driver": op.Driver, "sig": op.Sig, "inc": op.Inc, "project": op.Project,
			"idem_key": idemKey, "result": "replayed", "state": types.IssueClosed, "reason": op.Reason,
			"attempt": 1, "http_status": 200, "error_code": "", "retryable": false,
		})
		d.mu.Lock()
		if a, ok := d.anchors[d.anchorKey(op.Driver, op.Sig, op.Project)]; ok {
			a.ref = out
			a.ref.State = types.IssueClosed
			a.updatedTS = types.FormatUTC(d.now())
		}
		d.handled[idemKey] = true
		d.mu.Unlock()
		return nil
	}
	return newErr(types.CodeIssues006, ReasonReplayFailed, 0, false, "unknown spooled op %q", op.Op)
}

// dropEntry removes a spool entry and writes the drop pair (§3.7).
func (d *Desk) dropEntry(ctx context.Context, driver string, e types.SpoolEntry, reason string) {
	op, _ := DecodePayload(e.Payload)
	_ = d.spool.Delete(driver, e.ID)
	d.mu.Lock()
	if g, ok := d.gaps[driver]; ok {
		g.dropped++
	}
	d.mu.Unlock()
	_, _ = d.record(ctx, types.KGap, op.Sig, op.Inc, map[string]any{
		"id": types.NewID(types.PEv), "sensor": "issues", "scope": driver,
		"from_ts": e.TS, "to_ts": types.FormatUTC(d.now()), "est_lost": -1,
		"cause": types.CauseQueueOverflow,
	})
	_, _ = d.record(ctx, types.KIssue, op.Sig, op.Inc, map[string]any{
		"op": "drop", "driver": driver, "result": "dropped", "count": 1, "reason": reason,
		"oldest_ts": e.TS, "error_code": string(types.CodeIssues005), "retryable": false,
	})
}

// ReplayDue reports whether a driver's spool has an entry that is due now; the
// desk loop uses it to keep the replay timer cheap when the spool is empty.
func (d *Desk) ReplayDue(driver string) bool {
	entries, err := d.spool.List(driver)
	if err != nil || len(entries) == 0 {
		return false
	}
	now := types.FormatUTC(d.now())
	for _, e := range entries {
		if e.NextTryTS == "" || e.NextTryTS <= now {
			return true
		}
	}
	return false
}

// spoolCount is exposed for tests and diagnostics.
func (d *Desk) spoolCount(driver string) int { return d.spool.Count(driver) }

// spoolEntries exposes the raw entries for tests (ordered).
func (d *Desk) spoolEntries(driver string) []types.SpoolEntry {
	list, _ := d.spool.List(driver)
	return list
}

// WaitForSpool is a test helper: it polls until the spool holds n entries or the
// deadline passes.
func (d *Desk) WaitForSpool(driver string, n int, deadline time.Duration) bool {
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if d.spoolCount(driver) >= n {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return d.spoolCount(driver) >= n
}
