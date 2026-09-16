package registry

// modules_file.go — file.read, file.patch (SPEC-06 §3.8, §3.9).
//
// IMPLEMENT: the descriptors, args structs, env binding and target declarations
// below are frozen; fill in NormalizeArgs, Check, Apply and Verify per the spec
// row of each module, and delete the ErrPermanent TODO bodies.

import (
	"context"

	"github.com/totalwindupflightsystems/trouble/internal/registry/schemagen"
	"github.com/totalwindupflightsystems/trouble/internal/types"
)

type fileReadArgs struct {
	Path     string `json:"path"`
	MaxBytes int    `json:"max_bytes,omitempty" js:"min=1;max=1048576;default=262144"`
	Encoding string `json:"encoding,omitempty" js:"enum=utf-8|raw;default=utf-8"`
	Offset   int    `json:"offset,omitempty" js:"min=0;default=0"`
}

type filePatchArgs struct {
	Path        string `json:"path"`
	Patch       string `json:"patch,omitempty" js:"maxlen=1048576;oneof=patch|restore_from"`
	RestoreFrom string `json:"restore_from,omitempty"`
	Strip       int    `json:"strip,omitempty" js:"min=0;max=8;default=0"`
	Backup      bool   `json:"backup,omitempty" js:"default=true"`
}

type fileReadModule struct{ env *moduleEnv }

var fileReadDescriptor = types.Descriptor{
	Name:        "file.read",
	Version:     1,
	Schema:      schemagen.Generate("file.read", 1, fileReadArgs{}),
	Scopes:      []string{"file:read"},
	Idempotency: types.IdemPure,
	CheckMode:   true,
	TimeoutS:    5,
	Mutating:    false,
}

func (m fileReadModule) bind(e *moduleEnv)            { m.env = e }
func (m fileReadModule) Descriptor() types.Descriptor { return fileReadDescriptor }

func (m fileReadModule) NormalizeArgs(args map[string]any) (map[string]any, error) {
	if err := normalizePathArgs(args, "path"); err != nil {
		return nil, err
	}
	return args, nil
}

func (m fileReadModule) ProtectedTargets(args map[string]any) []protectedTarget {
	return pathTarget(args)
}

// IMPLEMENT: the spec §3.9 row of file.read.
func (m fileReadModule) Check(ctx context.Context, args map[string]any) (types.Diff, error) {
	return types.Diff{}, types.ErrPermanent
}

func (m fileReadModule) Apply(ctx context.Context, args map[string]any) (types.Result, error) {
	return types.Result{}, types.ErrPermanent
}

func (m fileReadModule) Verify(ctx context.Context, args map[string]any) (types.VerifyResult, error) {
	return types.VerifyResult{}, types.ErrPermanent
}

type filePatchModule struct{ env *moduleEnv }

var filePatchDescriptor = types.Descriptor{
	Name:        "file.patch",
	Version:     1,
	Schema:      schemagen.Generate("file.patch", 1, filePatchArgs{}),
	Scopes:      []string{"file:write"},
	Idempotency: types.IdemConvergent,
	CheckMode:   true,
	TimeoutS:    10,
	Mutating:    true,
}

func (m filePatchModule) bind(e *moduleEnv)            { m.env = e }
func (m filePatchModule) Descriptor() types.Descriptor { return filePatchDescriptor }
func (m filePatchModule) RollbackInvertible() bool     { return true }

func (m filePatchModule) NormalizeArgs(args map[string]any) (map[string]any, error) {
	if err := normalizePathArgs(args, "path"); err != nil {
		return nil, err
	}
	return args, nil
}

func (m filePatchModule) ProtectedTargets(args map[string]any) []protectedTarget {
	return pathTarget(args)
}

// IMPLEMENT: the spec §3.9 row of file.patch.
func (m filePatchModule) Check(ctx context.Context, args map[string]any) (types.Diff, error) {
	return types.Diff{}, types.ErrPermanent
}

func (m filePatchModule) Apply(ctx context.Context, args map[string]any) (types.Result, error) {
	return types.Result{}, types.ErrPermanent
}

func (m filePatchModule) Verify(ctx context.Context, args map[string]any) (types.VerifyResult, error) {
	return types.VerifyResult{}, types.ErrPermanent
}
