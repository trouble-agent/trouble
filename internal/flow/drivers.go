package flow

// drivers.go — the two FlowDriver implementations (SPEC-08 §2, §3.6).
//
// `board-jsonl` is the hot-fix lane's driver: the row must exist even when the
// router is down (the fleet's own `spawn_pending` path is what carries the work
// forward). `task-router` is the doctrine for normal filing, where router-side
// dedup, queueing and ordering are worth the extra hop. Both produce the
// identical BoardRow; only the write path differs.

import (
	"context"
	"fmt"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// driverFor selects the driver for a filing. At or above the hot-fix threshold
// the board-jsonl lane is chosen even when the configured driver is the router:
// an urgent row must not depend on a service being up (§3.6).
func (f *Flow) driverFor(mode string, inc types.Incident) types.FlowDriver {
	if f.cfg.Driver == types.FlowDriverRouter && severityAtLeast(inc.Severity, types.SevHigh) {
		return boardJSONLDriver{f: f}
	}
	if f.cfg.Driver == types.FlowDriverRouter {
		return taskRouterDriver{f: f}
	}
	return boardJSONLDriver{f: f}
}

// DriverName reports the configured driver.
func (f *Flow) DriverName() string { return f.cfg.Driver }

// boardJSONLDriver appends directly to the board (§3.2's writer recipe).
type boardJSONLDriver struct{ f *Flow }

// Name is the driver's config value.
func (d boardJSONLDriver) Name() string { return types.FlowDriverBoard }

// Healthcheck reports the board directory's reachability.
func (d boardJSONLDriver) Healthcheck(ctx context.Context) (types.DriverHealth, error) {
	proj := d.f.cfg.BoardPath
	ok := true
	detail := "board path " + proj
	if _, err := d.f.indexFor(proj); err != nil {
		ok = false
		detail = err.Error()
	}
	return types.DriverHealth{
		Driver: types.FlowDriverBoard, OK: ok, Detail: detail,
		CheckedTS: types.FormatUTC(d.f.clock().Now()),
	}, nil
}

// FileTask performs the two-file write (§3.2).
func (d boardJSONLDriver) FileTask(ctx context.Context, req types.FileTaskRequest) (types.FileTaskResult, error) {
	f := d.f
	ix, err := f.indexFor(req.BoardPath)
	if err != nil {
		return types.FileTaskResult{TaskID: req.Row.ID}, err
	}
	row := req.Row
	if row.ID == "" {
		row.ID = f.allocTaskID(ix)
	}
	if req.DryRun {
		// check_mode: the diff is returned and nothing is written.
		ix2 := *ix
		res, _, err := f.writeRow(ctx, types.FlowProject{BoardPath: req.BoardPath}, &ix2, row, req.Event)
		return res, err
	}
	res, _, err := f.writeRow(ctx, types.FlowProject{BoardPath: req.BoardPath}, ix, row, req.Event)
	return res, err
}

// CommentTask appends a comment event (§3.4): the row itself is never rewritten.
func (d boardJSONLDriver) CommentTask(ctx context.Context, row types.BoardRow, body string) (types.BoardRow, error) {
	_, err := d.f.Comment(ctx, row, body)
	return row, err
}

// taskRouterDriver dispatches to the router and lets it own the board.
type taskRouterDriver struct{ f *Flow }

// Name is the driver's config value.
func (d taskRouterDriver) Name() string { return types.FlowDriverRouter }

// Healthcheck reports whether the router answered.
func (d taskRouterDriver) Healthcheck(ctx context.Context) (types.DriverHealth, error) {
	ref, err := d.f.dispatch(ctx, types.BoardRow{
		ID: types.NewID(types.PTsk), Sig: "flow:healthcheck", Inc: "",
		Title: "flow healthcheck", Priority: "P3", Complexity: "S",
	}, types.SevLow)
	ok := err == nil
	detail := "router dispatch ok"
	if err != nil {
		detail = err.Error()
	}
	return types.DriverHealth{
		Driver: types.FlowDriverRouter, OK: ok, Detail: detail + " " + ref,
		CheckedTS: types.FormatUTC(d.f.clock().Now()),
	}, nil
}

// FileTask dispatches the row: the router writes it, so `Wrote` is false here.
// The id is still allocated locally so the dispatch is idempotent by task id.
func (d taskRouterDriver) FileTask(ctx context.Context, req types.FileTaskRequest) (types.FileTaskResult, error) {
	f := d.f
	row := req.Row
	if row.ID == "" {
		ix, err := f.indexFor(req.BoardPath)
		if err != nil {
			return types.FileTaskResult{}, err
		}
		row.ID = f.allocTaskID(ix)
	}
	if req.DryRun {
		return types.FileTaskResult{
			Wrote: false, TaskID: row.ID, Valid: true,
			Diff: types.Diff{Empty: false, Summary: "dispatch 1 row via task-router",
				Entries: []types.DiffEntry{{Path: req.BoardPath + "/" + tasksFile, Before: nil, After: row.ID}}},
		}, nil
	}
	if _, err := f.dispatch(ctx, row, types.SevHigh); err != nil {
		return types.FileTaskResult{TaskID: row.ID}, err
	}
	// The router owns the append; trouble records the dispatch and never
	// pretends it wrote the row.
	return types.FileTaskResult{Wrote: false, TaskID: row.ID, Valid: true}, nil
}

// CommentTask dispatches a comment. The router has no comment endpoint in v0.1
// (the wire payload of §3.6 is a filing), so the comment lands as an event
// append — the additive half of the two-file contract — and the dispatch path
// stays the router's. This is recorded as a spec deviation in the run report.
func (d taskRouterDriver) CommentTask(ctx context.Context, row types.BoardRow, body string) (types.BoardRow, error) {
	if row.ID == "" {
		return row, fmt.Errorf("%s: comment needs the row id", types.CodeFlow019)
	}
	ev := types.BoardEvent{
		Type: types.EvTaskComment, TaskID: row.ID, TS: types.FormatUTC(d.f.clock().Now()),
		Actor: d.f.actorName(),
		Detail: map[string]any{"inc": row.Inc, "sig": row.Sig,
			"body": d.f.scrub(types.TgBoard, body)},
	}
	ix, err := d.f.indexFor(d.f.boardOfRow(row))
	if err != nil {
		return row, err
	}
	if _, err := ix.appendEventAlways(ctx, ev, d.f.cfg.ValidateCmd); err != nil {
		return row, err
	}
	return row, nil
}
