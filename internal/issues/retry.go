package issues

import (
	"context"
	"math"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// attemptBudget is one operation's sleep budget: the sum of in-operation sleeps
// is capped by op_deadline (45 s), so a driver can never sleep past the ladder's
// bound (§3.5). When the cap or max_attempts is reached the operation is spooled,
// never retried inline.
type attemptBudget struct {
	remaining time.Duration
}

func newBudget(total time.Duration) *attemptBudget {
	if total <= 0 {
		total = 45 * time.Second
	}
	return &attemptBudget{remaining: total}
}

func (b *attemptBudget) canSleep(d time.Duration) bool { return d < b.remaining }

func (b *attemptBudget) spend(d time.Duration) { b.remaining -= d }

// backoffFor is the §3.5 delay for attempt n (1-based):
// min(max_backoff, base * 2^(n-1)) * (1 + U(-jitter, +jitter)).
func (d *Desk) backoffFor(dc types.IssueDriverConfig, attempt int) time.Duration {
	base := dc.BaseBackoff.Std()
	if base <= 0 {
		base = time.Second
	}
	maxB := dc.MaxBackoff.Std()
	if maxB <= 0 {
		maxB = 60 * time.Second
	}
	delay := float64(base) * math.Pow(2, float64(attempt-1))
	if delay > float64(maxB) {
		delay = float64(maxB)
	}
	j := dc.Jitter
	if j <= 0 {
		j = 0.25
	}
	if j > 1 {
		j = 1
	}
	d.mu.Lock()
	u := d.rand.Float64()
	d.mu.Unlock()
	delay *= 1 + j*(2*u-1)
	if delay < 0 {
		delay = 0
	}
	return time.Duration(delay)
}

// maxAttemptsOf is the effective attempt count for a driver.
func maxAttemptsOf(dc types.IssueDriverConfig) int {
	if dc.MaxAttempts > 0 {
		return dc.MaxAttempts
	}
	return 5
}

// withRetry runs one driver operation under the §3.5 policy: per-attempt timeout
// = the driver's `timeout`, exponential backoff with ±jitter, max_attempts, and
// one ledger record per attempt that mattered. A permanent failure is never
// retried and never spooled; a budget exhaustion returns the error so the caller
// spools it.
func (d *Desk) withRetry(ctx context.Context, driver string, dc types.IssueDriverConfig, op, idem string,
	fn func(context.Context) error) (int, error) {

	maxAttempts := maxAttemptsOf(dc)
	perAttempt := dc.Timeout.Std()
	if perAttempt <= 0 {
		perAttempt = 15 * time.Second
	}
	budget := newBudget(d.cfg.OpDeadline.Std())
	var last *Error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		actx, cancel := context.WithTimeout(ctx, perAttempt)
		err := fn(actx)
		cancel()
		if err == nil {
			if attempt > 1 {
				d.recordAttempt(driver, op, idem, attempt, maxAttempts, "ok", 0, nil, 0)
			}
			return attempt, nil
		}
		e := asError(err)
		last = e
		if !e.Retryable {
			d.recordAttempt(driver, op, idem, attempt, maxAttempts, "give_up", 0, e, 0)
			return attempt, e
		}
		if attempt == maxAttempts {
			d.recordAttempt(driver, op, idem, attempt, maxAttempts, "give_up", 0, e, 0)
			return attempt, e
		}
		// A rate limit is waited out only while the op deadline allows it; beyond
		// 15 m the operation is spooled and the driver pauses until reset.
		delay := d.backoffFor(dc, attempt)
		if e.Code == types.CodeIssues002 {
			if wait := d.rateLimitWait(e); wait > 0 {
				if wait > 15*time.Minute || wait > budget.remaining {
					d.recordAttempt(driver, op, idem, attempt, maxAttempts, "spooled", int(delay.Milliseconds()), e, 0)
					return attempt, e
				}
				delay = wait
			}
		}
		if !budget.canSleep(delay) {
			d.recordAttempt(driver, op, idem, attempt, maxAttempts, "spooled", int(delay.Milliseconds()), e, 0)
			return attempt, e
		}
		d.recordAttempt(driver, op, idem, attempt, maxAttempts, "retry", int(delay.Milliseconds()), e, 0)
		if delay > 0 {
			budget.spend(delay)
			select {
			case <-ctx.Done():
				return attempt, wrapErr(types.CodeIssues001, ReasonTimeout, 0, true, ctx.Err())
			case <-time.After(delay):
			}
		}
	}
	if last == nil {
		last = newErr(types.CodeIssues001, ReasonTransient, 0, true, "no attempt ran for %s", op)
	}
	return maxAttempts, last
}

// rateLimitWait is the bounded sleep to a rate-limit reset: a reset in the past
// (or a backwards clock jump) is treated as "probe again in 60 s", so there is
// no infinite sleep and no negative backoff (edge case 10).
func (d *Desk) rateLimitWait(e *Error) time.Duration {
	if e.ResetTS == "" {
		return 60 * time.Second
	}
	reset, err := types.ParseUTC(e.ResetTS)
	if err != nil {
		return 60 * time.Second
	}
	wait := reset.Sub(d.now())
	if wait <= 0 {
		return 60 * time.Second
	}
	return wait
}

// recordAttempt writes the per-attempt ledger record of §3.5, using the example's
// flat key names.
func (d *Desk) recordAttempt(driver, op, idem string, attempt, maxAttempts int, outcome string,
	backoffMS int, e *Error, nextTry time.Duration) {

	if d.deps.Ledger == nil {
		return
	}
	payload := map[string]any{
		"op": op, "driver": driver, "idem_key": idem,
		"attempt": attempt, "max_attempts": maxAttempts, "outcome": outcome,
		"http_status": 0, "error_code": "", "retryable": false, "backoff_ms": backoffMS,
		"next_try_ts": "", "remaining": -1, "reset_ts": "",
	}
	if e != nil {
		payload["http_status"] = e.HTTPStatus
		payload["error_code"] = string(e.Code)
		payload["retryable"] = e.Retryable
		if e.ResetTS != "" {
			payload["reset_ts"] = e.ResetTS
		}
	}
	if nextTry > 0 {
		payload["next_try_ts"] = types.FormatUTC(d.now().Add(nextTry))
	}
	_, _ = d.record(context.Background(), types.KIssue, "", "", payload)
}

// ensurePayload builds the §3.3 `kind=issue` ensure payload. It is assembled
// before the call so the failure paths only have to override the outcome keys.
func (d *Desk) ensurePayload(driver string, inc types.Incident, idem, body string, attempts int,
	dc types.IssueDriverConfig, redactions int, project string, anchorHit bool) map[string]any {

	return map[string]any{
		"op": "ensure", "driver": driver, "sig": inc.Sig, "inc": inc.ID, "project": project,
		"idem_key": idem, "result": "pending", "created": false, "commented": false,
		"external_id": "", "url": "", "attempt": attempts, "max_attempts": maxAttemptsOf(dc),
		"http_status": 0, "error_code": "", "retryable": false,
		"dedup_window": string(dedupWindowText(dc)), "anchor_hit": anchorHit,
		"body_bytes": len(body), "truncated": len(body) >= d.cfg.BodyMaxBytes,
		"redactions": redactions,
	}
}

func dedupWindowText(dc types.IssueDriverConfig) types.Duration {
	if dc.DedupWindow != "" {
		return dc.DedupWindow
	}
	return "30m"
}

// dedupWindowDur is the effective window rendered as the frozen config type.
func (d *Desk) dedupWindowDur(dc types.IssueDriverConfig) types.Duration {
	return types.Duration(d.dedupWindow(dc).String())
}

// spoolEnsure queues a failed ensure. The entry carries its original idem key, so
// replay cannot create a duplicate issue (§3.7 rule 3).
func (d *Desk) spoolEnsure(ctx context.Context, driver string, drv types.IssueDriver, inc types.Incident,
	req types.EnsureBySigRequest, idem, project, opID string, e *Error, attempts int) {

	op := spoolOp{
		Op: "ensure", Driver: driver, Sig: inc.Sig, Project: project, Inc: inc.ID,
		Ensure: &req, Unverified: true,
	}
	d.spoolOp(ctx, drv, op, idem, attempts, driver)
}

// spoolComment queues a failed fold/comment.
func (d *Desk) spoolComment(ctx context.Context, driver string, drv types.IssueDriver, ref types.IssueRef,
	inc types.Incident, idemKey, body, project string, e *Error, attempts int) {

	r := ref
	op := spoolOp{
		Op: "comment", Driver: driver, Sig: inc.Sig, Project: project, Inc: inc.ID,
		Ref: &r, Trigger: inc.ID, Body: body,
	}
	d.spoolOp(ctx, drv, op, idemKey, attempts, driver)
}

// spoolClose queues a failed close.
func (d *Desk) spoolClose(ctx context.Context, driver string, ref types.IssueRef, idemKey, reason, project string, attempts int) {
	r := ref
	op := spoolOp{Op: "close", Driver: driver, Sig: ref.Sig, Project: project, Ref: &r, Reason: reason}
	d.spoolOp(ctx, nil, op, idemKey, attempts, driver)
}

// spoolOp writes one bounded spool entry and its records: the refusal path
// (TROUBLE-ISSUES-005) counts as suppressed, and every eviction writes both a
// `gap` record and an `issue{op:drop}` record so the loss is visible in the
// ledger rather than inferred.
func (d *Desk) spoolOp(ctx context.Context, drv types.IssueDriver, op spoolOp, idemKey string, attempts int, driver string) {
	if drv == nil {
		drv = d.drivers[driver]
	}
	body, err := EncodePayload(op)
	if err != nil {
		_, _ = d.record(ctx, types.KIssue, op.Sig, op.Inc, map[string]any{
			"op": "drop", "driver": driver, "result": "failed", "count": 1, "reason": "encode",
			"error_code": string(types.CodeIssues005), "retryable": false,
		})
		return
	}
	next := types.FormatUTC(d.now().Add(d.backoffFor(d.drvCfg[driver], attempts+1)))
	entry := NewSpoolEntry(driver, idemKey, body, next, d.now())
	drops, perr := d.spool.Put(driver, entry)
	d.dropRecords(ctx, drops)
	if perr != nil {
		// Spool full: the operations stay counted in the group's suppressed
		// counter so nothing is lost silently (§3.7).
		d.caps.noteSuppressed()
		_, _ = d.record(ctx, types.KIssue, op.Sig, op.Inc, map[string]any{
			"op": "drop", "driver": driver, "result": "refused", "count": 1, "reason": "spool_full",
			"error_code": string(types.CodeIssues005), "retryable": true, "detail": perr.Error(),
		})
		return
	}
}

// dropRecords writes the gap + drop pair for every eviction.
func (d *Desk) dropRecords(ctx context.Context, drops []dropEvent) {
	for _, ev := range drops {
		d.mu.Lock()
		if g, ok := d.gaps[ev.Driver]; ok {
			g.dropped += ev.Count
		}
		d.mu.Unlock()
		_, _ = d.record(ctx, types.KGap, "", "", map[string]any{
			"id": types.NewID(types.PEv), "sensor": "issues", "scope": ev.Driver,
			"from_ts": ev.OldestTS, "to_ts": types.FormatUTC(d.now()),
			"est_lost": ev.EstLost, "cause": types.CauseQueueOverflow,
		})
		_, _ = d.record(ctx, types.KIssue, "", "", map[string]any{
			"op": "drop", "driver": ev.Driver, "result": "dropped", "count": ev.Count,
			"reason": ev.Reason, "oldest_ts": ev.OldestTS,
			"error_code": string(types.CodeIssues005), "retryable": false,
		})
	}
}
