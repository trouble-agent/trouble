package registry

import (
	"context"
	"fmt"

	"github.com/trouble-agent/trouble/internal/types"
)

// Runner is the adapter that satisfies SPEC-05 §2's PlayRunner view of this
// package (SPEC-06 §2.2). It is a thin adapter, not a second contract: Check and
// Apply fill Module/Args on the bound base request and call Call, returning the
// module-level value AND the audit record the ladder stores as its tool_call_id.
type Runner struct {
	reg  *Registry
	base types.ToolCallRequest
}

// Runner binds a base request (inc, rule, sig, actor, grants, DeadlineS) once,
// so the ladder passes identity once, not per task.
func (r *Registry) Runner(base types.ToolCallRequest) *Runner {
	if base.Actor.Kind == "" {
		base.Actor = r.deps.Actor
	}
	return &Runner{reg: r, base: base}
}

// Check runs stages 1, 2, 3 and 6 (mode check_mode): nothing is touched.
func (x *Runner) Check(ctx context.Context, tool string, args map[string]any) (types.Diff, types.ToolCall, error) {
	req := x.req(tool, args, types.ModeCheck)
	tc, err := x.reg.Call(ctx, req)
	if err != nil {
		return types.Diff{}, tc, err
	}
	if tc.CheckDiff == nil {
		return types.Diff{}, tc, Outcome(tc)
	}
	return *tc.CheckDiff, tc, Outcome(tc)
}

// Apply runs all six stages (mode apply).
func (x *Runner) Apply(ctx context.Context, tool string, args map[string]any) (types.Result, types.ToolCall, error) {
	req := x.req(tool, args, types.ModeApply)
	tc, err := x.reg.Call(ctx, req)
	if err != nil {
		return types.Result{}, tc, err
	}
	if tc.Result == nil {
		return types.Result{}, tc, Outcome(tc)
	}
	return *tc.Result, tc, Outcome(tc)
}

// Rollback re-authorizes a tool call's RollbackHint through the full six stages,
// so a rollback is itself an audited tool call. A PONR hint (unsupported
// rollback) returns TROUBLE-REGISTRY-015 without touching a target.
func (x *Runner) Rollback(ctx context.Context, t types.ToolCall) error {
	if t.Result == nil || t.Result.Rollback == nil || !t.Result.Rollback.Supported || t.Result.Rollback.Module == "" {
		return newErr(types.CodeRegistry015, "rollback", reasonRollbackUnsupported,
			"call %s carries no usable rollback hint", t.ID)
	}
	return x.reg.rollbackHint(ctx, *t.Result.Rollback)
}

// Rollback is the Registry-level entry point of the same operation.
func (r *Registry) Rollback(ctx context.Context, t types.ToolCall) error {
	return r.Runner(types.ToolCallRequest{}).Rollback(ctx, t)
}

func (x *Runner) req(tool string, args map[string]any, mode string) types.ToolCallRequest {
	req := x.base
	req.Module = tool
	req.Args = args
	req.Mode = mode
	if req.Source == "" {
		req.Source = "runner"
	}
	req.IdemKey = fmt.Sprintf("%s|%s|%s", req.Inc, tool, shortHash(args))
	return req
}
