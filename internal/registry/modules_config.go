package registry

// modules_config.go — config.get, config.set, config.list (SPEC-06 §3.8, §3.9).
//
// IMPLEMENT: the descriptors, args structs, env binding and target declarations
// below are frozen; fill in NormalizeArgs, Check, Apply and Verify per the spec
// row of each module, and delete the ErrPermanent TODO bodies.

import (
	"context"

	"github.com/totalwindupflightsystems/trouble/internal/registry/schemagen"
	"github.com/totalwindupflightsystems/trouble/internal/types"
)

type configGetArgs struct {
	Path    string `json:"path"`
	Key     string `json:"key"`
	Default any    `json:"default,omitempty"`
}

type configSetArgs struct {
	Path   string `json:"path"`
	Key    string `json:"key" js:"pattern=^[A-Za-z0-9_.\\-]{1,128}$"`
	Value  any    `json:"value"`
	Format string `json:"format,omitempty" js:"enum=toml|yaml|json|env|ini"`
	Create bool   `json:"create,omitempty" js:"default=false"`
}

type configListArgs struct {
	Path   string `json:"path"`
	Prefix string `json:"prefix,omitempty" js:"default="`
}

type configGetModule struct{ env *moduleEnv }

var configGetDescriptor = types.Descriptor{
	Name:        "config.get",
	Version:     1,
	Schema:      schemagen.Generate("config.get", 1, configGetArgs{}),
	Scopes:      []string{"config:read"},
	Idempotency: types.IdemPure,
	CheckMode:   true,
	TimeoutS:    5,
	Mutating:    false,
}

func (m configGetModule) bind(e *moduleEnv)            { m.env = e }
func (m configGetModule) Descriptor() types.Descriptor { return configGetDescriptor }

func (m configGetModule) NormalizeArgs(args map[string]any) (map[string]any, error) {
	if err := normalizePathArgs(args, "path"); err != nil {
		return nil, err
	}
	return args, nil
}

func (m configGetModule) ProtectedTargets(args map[string]any) []protectedTarget {
	return pathTarget(args)
}

// IMPLEMENT: the SPEC-06 §3.9 row of config.get: span-read one key, Diff{Empty:true} with the observed value in Summary, Apply is a read.
func (m configGetModule) Check(ctx context.Context, args map[string]any) (types.Diff, error) {
	return types.Diff{}, types.ErrPermanent
}

func (m configGetModule) Apply(ctx context.Context, args map[string]any) (types.Result, error) {
	return types.Result{}, types.ErrPermanent
}

func (m configGetModule) Verify(ctx context.Context, args map[string]any) (types.VerifyResult, error) {
	return types.VerifyResult{}, types.ErrPermanent
}

type configSetModule struct{ env *moduleEnv }

var configSetDescriptor = types.Descriptor{
	Name:        "config.set",
	Version:     1,
	Schema:      schemagen.Generate("config.set", 1, configSetArgs{}),
	Scopes:      []string{"config:write"},
	Idempotency: types.IdemConvergent,
	CheckMode:   true,
	TimeoutS:    10,
	Mutating:    true,
}

func (m configSetModule) bind(e *moduleEnv)            { m.env = e }
func (m configSetModule) Descriptor() types.Descriptor { return configSetDescriptor }
func (m configSetModule) RollbackInvertible() bool     { return true }

func (m configSetModule) NormalizeArgs(args map[string]any) (map[string]any, error) {
	if err := normalizePathArgs(args, "path"); err != nil {
		return nil, err
	}
	return args, nil
}

func (m configSetModule) ProtectedTargets(args map[string]any) []protectedTarget {
	return pathTarget(args)
}

// IMPLEMENT: the SPEC-06 §3.9 row of config.set: configSpan locates the value span, Apply replaces only the value bytes and atomically renames, preserving comments/key order/format; missing key without create=true is TROUBLE-REGISTRY-011; non-UTF-8 or unsafely span-editable formats are 011 too. RollbackHint = config.set with the `before` value.
func (m configSetModule) Check(ctx context.Context, args map[string]any) (types.Diff, error) {
	return types.Diff{}, types.ErrPermanent
}

func (m configSetModule) Apply(ctx context.Context, args map[string]any) (types.Result, error) {
	return types.Result{}, types.ErrPermanent
}

func (m configSetModule) Verify(ctx context.Context, args map[string]any) (types.VerifyResult, error) {
	return types.VerifyResult{}, types.ErrPermanent
}

type configListModule struct{ env *moduleEnv }

var configListDescriptor = types.Descriptor{
	Name:        "config.list",
	Version:     1,
	Schema:      schemagen.Generate("config.list", 1, configListArgs{}),
	Scopes:      []string{"config:read"},
	Idempotency: types.IdemPure,
	CheckMode:   true,
	TimeoutS:    5,
	Mutating:    false,
}

func (m configListModule) bind(e *moduleEnv)            { m.env = e }
func (m configListModule) Descriptor() types.Descriptor { return configListDescriptor }

func (m configListModule) NormalizeArgs(args map[string]any) (map[string]any, error) {
	if err := normalizePathArgs(args, "path"); err != nil {
		return nil, err
	}
	return args, nil
}

func (m configListModule) ProtectedTargets(args map[string]any) []protectedTarget {
	return pathTarget(args)
}

// IMPLEMENT: the SPEC-06 §3.9 row of config.list: list keys (optionally under `prefix`) with their values.
func (m configListModule) Check(ctx context.Context, args map[string]any) (types.Diff, error) {
	return types.Diff{}, types.ErrPermanent
}

func (m configListModule) Apply(ctx context.Context, args map[string]any) (types.Result, error) {
	return types.Result{}, types.ErrPermanent
}

func (m configListModule) Verify(ctx context.Context, args map[string]any) (types.VerifyResult, error) {
	return types.VerifyResult{}, types.ErrPermanent
}
