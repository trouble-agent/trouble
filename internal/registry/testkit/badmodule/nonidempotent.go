// Package badmodule ships the two deliberate negative modules of SPEC-06 §2.4.
//
// They exist so `make conformance` can prove the harness actually rejects a
// broken module: the negative test asserts that testkit.Run fails the
// non-idempotent module with TROUBLE-REGISTRY-013, and that registry.NewWith /
// Register refuse the check_mode-less module with TROUBLE-REGISTRY-014. A pass in
// either place is the CI failure.
//
// The package is test-only: it is not registered by internal/registry/register.go
// (the only registration site) and is never linked into the daemon binary.
// Nothing here shells out and nothing here accepts an arbitrary command
// (SPEC-06 §3.11).
package badmodule

import (
	"context"
	"os"

	"github.com/trouble-agent/trouble/internal/registry/schemagen"
	"github.com/trouble-agent/trouble/internal/types"
)

// CounterArgs is the args schema of the non-idempotent negative module.
type CounterArgs struct {
	Path string `json:"path"`
}

// counterSuffix is the byte the non-idempotent module appends on *every* Apply:
// the counter never reaches a fixed point, which is exactly the defect
// obligation 1 must catch.
const counterSuffix = "x"

// NonIdempotent is the deliberate defect of SPEC-06 §2.4: it declares
// Idempotency=convergent and CheckMode=true, but its Apply appends a byte to the
// counter file on every call instead of converging.
type NonIdempotent struct{}

var nonIdempotentDescriptor = types.Descriptor{
	Name:        "badmodule.nonidempotent",
	Version:     1,
	Schema:      schemagen.Generate("badmodule.nonidempotent", 1, CounterArgs{}),
	Scopes:      []string{"file:write"},
	Idempotency: types.IdemConvergent,
	CheckMode:   true,
	TimeoutS:    5,
	Mutating:    true,
}

// Descriptor implements types.Module. The descriptor is deliberately valid: the
// failure must come from obligation 1, not from the descriptor check.
func (m NonIdempotent) Descriptor() types.Descriptor { return nonIdempotentDescriptor }

// Check is honest — a non-converging target always has work left to do — so the
// defect is visible only in Apply, which is what obligation 1 compares.
func (m NonIdempotent) Check(_ context.Context, args map[string]any) (types.Diff, error) {
	path, err := counterPath(args)
	if err != nil {
		return types.Diff{}, err
	}
	current, err := os.ReadFile(path)
	if err != nil {
		return types.Diff{}, types.ErrPermanent
	}
	return types.Diff{Entries: []types.DiffEntry{{
		Path:   path,
		Before: string(current),
		After:  string(current) + counterSuffix,
	}}}, nil
}

// Apply appends counterSuffix to the counter file on EVERY call, even when the
// args (and therefore the IdemKey) are identical. This is the violation.
func (m NonIdempotent) Apply(_ context.Context, args map[string]any) (types.Result, error) {
	path, err := counterPath(args)
	if err != nil {
		return types.Result{}, err
	}
	current, err := os.ReadFile(path)
	if err != nil {
		return types.Result{}, types.ErrPermanent
	}
	next := string(current) + counterSuffix
	if err := os.WriteFile(path, []byte(next), 0o644); err != nil {
		return types.Result{}, types.ErrPermanent
	}
	return types.Result{
		Changed: true,
		Applied: []types.DiffEntry{{Path: path, Before: string(current), After: next}},
		Output:  map[string]any{"path": path, "bytes": len(next)},
	}, nil
}

// Verify always reports OK: the defect the harness must catch is the second
// Apply, not a failing verification.
func (m NonIdempotent) Verify(_ context.Context, args map[string]any) (types.VerifyResult, error) {
	return types.VerifyResult{OK: true, Method: "recheck", Detail: map[string]any{"module": nonIdempotentDescriptor.Name}}, nil
}

func counterPath(args map[string]any) (string, error) {
	path, _ := args["path"].(string)
	if path == "" {
		return "", types.ErrPermanent
	}
	return path, nil
}
