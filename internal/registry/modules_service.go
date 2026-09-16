package registry

// modules_service.go — service.status, service.reload, service.restart (SPEC-06 §3.8, §3.9).
//
// IMPLEMENT: the descriptors, args structs, env binding and target declarations
// below are frozen; fill in NormalizeArgs, Check, Apply and Verify per the spec
// row of each module, and delete the ErrPermanent TODO bodies.

import (
	"context"

	"github.com/totalwindupflightsystems/trouble/internal/registry/schemagen"
	"github.com/totalwindupflightsystems/trouble/internal/types"
)

type serviceStatusArgs struct {
	Unit  string `json:"unit" js:"pattern=^[A-Za-z0-9@._:\\-]{1,255}$"`
	Scope string `json:"scope,omitempty" js:"enum=system|user;default=system"`
}

type serviceReloadArgs struct {
	Unit  string `json:"unit"`
	Scope string `json:"scope,omitempty" js:"enum=system|user;default=system"`
}

type serviceRestartArgs struct {
	Unit  string `json:"unit"`
	Scope string `json:"scope,omitempty" js:"enum=system|user;default=system"`
	Mode  string `json:"mode,omitempty" js:"enum=replace|fail;default=replace"`
}

type serviceStatusModule struct{ env *moduleEnv }

var serviceStatusDescriptor = types.Descriptor{
	Name:        "service.status",
	Version:     1,
	Schema:      schemagen.Generate("service.status", 1, serviceStatusArgs{}),
	Scopes:      []string{"service:read"},
	Idempotency: types.IdemPure,
	CheckMode:   true,
	TimeoutS:    5,
	Mutating:    false,
}

func (m serviceStatusModule) bind(e *moduleEnv)            { m.env = e }
func (m serviceStatusModule) Descriptor() types.Descriptor { return serviceStatusDescriptor }

func (m serviceStatusModule) NormalizeArgs(args map[string]any) (map[string]any, error) {
	normalizeUnitArgs(args)
	return args, nil
}

func (m serviceStatusModule) ProtectedTargets(args map[string]any) []protectedTarget {
	return unitTarget(args)
}

// IMPLEMENT: the spec §3.9 row of service.status.
func (m serviceStatusModule) Check(ctx context.Context, args map[string]any) (types.Diff, error) {
	return types.Diff{}, types.ErrPermanent
}

func (m serviceStatusModule) Apply(ctx context.Context, args map[string]any) (types.Result, error) {
	return types.Result{}, types.ErrPermanent
}

func (m serviceStatusModule) Verify(ctx context.Context, args map[string]any) (types.VerifyResult, error) {
	return types.VerifyResult{}, types.ErrPermanent
}

type serviceReloadModule struct{ env *moduleEnv }

var serviceReloadDescriptor = types.Descriptor{
	Name:        "service.reload",
	Version:     1,
	Schema:      schemagen.Generate("service.reload", 1, serviceReloadArgs{}),
	Scopes:      []string{"service:write"},
	Idempotency: types.IdemOnce,
	CheckMode:   true,
	TimeoutS:    20,
	Mutating:    true,
}

func (m serviceReloadModule) bind(e *moduleEnv)            { m.env = e }
func (m serviceReloadModule) Descriptor() types.Descriptor { return serviceReloadDescriptor }
func (m serviceReloadModule) RollbackInvertible() bool     { return false }

func (m serviceReloadModule) NormalizeArgs(args map[string]any) (map[string]any, error) {
	normalizeUnitArgs(args)
	return args, nil
}

func (m serviceReloadModule) ProtectedTargets(args map[string]any) []protectedTarget {
	return unitTarget(args)
}

// IMPLEMENT: the spec §3.9 row of service.reload.
func (m serviceReloadModule) Check(ctx context.Context, args map[string]any) (types.Diff, error) {
	return types.Diff{}, types.ErrPermanent
}

func (m serviceReloadModule) Apply(ctx context.Context, args map[string]any) (types.Result, error) {
	return types.Result{}, types.ErrPermanent
}

func (m serviceReloadModule) Verify(ctx context.Context, args map[string]any) (types.VerifyResult, error) {
	return types.VerifyResult{}, types.ErrPermanent
}

type serviceRestartModule struct{ env *moduleEnv }

var serviceRestartDescriptor = types.Descriptor{
	Name:        "service.restart",
	Version:     1,
	Schema:      schemagen.Generate("service.restart", 1, serviceRestartArgs{}),
	Scopes:      []string{"service:write"},
	Idempotency: types.IdemOnce,
	CheckMode:   true,
	TimeoutS:    25,
	Mutating:    true,
}

func (m serviceRestartModule) bind(e *moduleEnv)            { m.env = e }
func (m serviceRestartModule) Descriptor() types.Descriptor { return serviceRestartDescriptor }
func (m serviceRestartModule) RollbackInvertible() bool     { return false }

func (m serviceRestartModule) NormalizeArgs(args map[string]any) (map[string]any, error) {
	normalizeUnitArgs(args)
	return args, nil
}

func (m serviceRestartModule) ProtectedTargets(args map[string]any) []protectedTarget {
	return unitTarget(args)
}

// IMPLEMENT: the spec §3.9 row of service.restart.
func (m serviceRestartModule) Check(ctx context.Context, args map[string]any) (types.Diff, error) {
	return types.Diff{}, types.ErrPermanent
}

func (m serviceRestartModule) Apply(ctx context.Context, args map[string]any) (types.Result, error) {
	return types.Result{}, types.ErrPermanent
}

func (m serviceRestartModule) Verify(ctx context.Context, args map[string]any) (types.VerifyResult, error) {
	return types.VerifyResult{}, types.ErrPermanent
}
