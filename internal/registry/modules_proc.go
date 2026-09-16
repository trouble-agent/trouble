package registry

// modules_proc.go — proc.top, proc.connections (SPEC-06 §3.8, §3.9).
//
// IMPLEMENT: the descriptors, args structs, env binding and target declarations
// below are frozen; fill in NormalizeArgs, Check, Apply and Verify per the spec
// row of each module, and delete the ErrPermanent TODO bodies.

import (
	"context"

	"github.com/totalwindupflightsystems/trouble/internal/registry/schemagen"
	"github.com/totalwindupflightsystems/trouble/internal/types"
)

type procTopArgs struct {
	Sort string `json:"sort,omitempty" js:"enum=cpu|rss|io;default=cpu"`
	N    int    `json:"n,omitempty" js:"min=1;max=100;default=15"`
	Unit string `json:"unit,omitempty"`
}

type procConnectionsArgs struct {
	Unit   string   `json:"unit,omitempty"`
	PID    int      `json:"pid,omitempty" js:"min=1"`
	Proto  string   `json:"proto,omitempty" js:"enum=tcp|udp|all;default=all"`
	States []string `json:"states,omitempty"`
	Limit  int      `json:"limit,omitempty" js:"min=1;max=2000;default=200"`
}

type procTopModule struct{ env *moduleEnv }

var procTopDescriptor = types.Descriptor{
	Name:        "proc.top",
	Version:     1,
	Schema:      schemagen.Generate("proc.top", 1, procTopArgs{}),
	Scopes:      []string{"proc:read"},
	Idempotency: types.IdemPure,
	CheckMode:   true,
	TimeoutS:    5,
	Mutating:    false,
}

func (m procTopModule) bind(e *moduleEnv)            { m.env = e }
func (m procTopModule) Descriptor() types.Descriptor { return procTopDescriptor }

func (m procTopModule) NormalizeArgs(args map[string]any) (map[string]any, error) {
	normalizeUnitArgs(args)
	return args, nil
}

func (m procTopModule) ProtectedTargets(args map[string]any) []protectedTarget {
	if _, ok := args["unit"]; ok {
		return unitTarget(args)
	}
	return nil
}

// IMPLEMENT: the spec §3.9 row of proc.top.
func (m procTopModule) Check(ctx context.Context, args map[string]any) (types.Diff, error) {
	return types.Diff{}, types.ErrPermanent
}

func (m procTopModule) Apply(ctx context.Context, args map[string]any) (types.Result, error) {
	return types.Result{}, types.ErrPermanent
}

func (m procTopModule) Verify(ctx context.Context, args map[string]any) (types.VerifyResult, error) {
	return types.VerifyResult{}, types.ErrPermanent
}

type procConnectionsModule struct{ env *moduleEnv }

var procConnectionsDescriptor = types.Descriptor{
	Name:        "proc.connections",
	Version:     1,
	Schema:      schemagen.Generate("proc.connections", 1, procConnectionsArgs{}),
	Scopes:      []string{"proc:read"},
	Idempotency: types.IdemPure,
	CheckMode:   true,
	TimeoutS:    10,
	Mutating:    false,
}

func (m procConnectionsModule) bind(e *moduleEnv)            { m.env = e }
func (m procConnectionsModule) Descriptor() types.Descriptor { return procConnectionsDescriptor }

func (m procConnectionsModule) NormalizeArgs(args map[string]any) (map[string]any, error) {
	normalizeUnitArgs(args)
	return args, nil
}

func (m procConnectionsModule) ProtectedTargets(args map[string]any) []protectedTarget {
	if _, ok := args["unit"]; ok {
		return unitTarget(args)
	}
	return nil
}

// IMPLEMENT: the spec §3.9 row of proc.connections.
func (m procConnectionsModule) Check(ctx context.Context, args map[string]any) (types.Diff, error) {
	return types.Diff{}, types.ErrPermanent
}

func (m procConnectionsModule) Apply(ctx context.Context, args map[string]any) (types.Result, error) {
	return types.Result{}, types.ErrPermanent
}

func (m procConnectionsModule) Verify(ctx context.Context, args map[string]any) (types.VerifyResult, error) {
	return types.VerifyResult{}, types.ErrPermanent
}
