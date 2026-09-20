package testkit

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/registry"
	"github.com/trouble-agent/trouble/internal/registry/schemagen"
	"github.com/trouble-agent/trouble/internal/registry/testkit/badmodule"
	"github.com/trouble-agent/trouble/internal/types"
)

// This file is the negative half of AC-23 (SPEC-06 §2.4, §7): the harness must
// REJECT a broken module, and the registry must refuse a mutating module without
// a dry run. A pass in either test is the CI failure, not a success.
//
// The tests drive the harness through the same core Run uses (Run is exactly
// obligations(t, ...) plus t.Error) with a recording reporter: a test that asserts
// a failure cannot use a real subtest, because a failing subtest fails its parent
// test as well.
//
// TestHarnessPassesCorrectModule is the positive control. Without it a harness
// that failed every module would also pass the two negative tests.

// recorder is a reporter that records what the harness reports instead of failing
// a *testing.T.
type recorder struct {
	t    *testing.T
	msgs []string
}

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.msgs = append(r.msgs, fmt.Sprintf(format, args...))
}

func (r *recorder) TempDir() string { return r.t.TempDir() }

// TestHarnessRejectsNonIdempotent asserts that testkit.Run fails
// badmodule/nonidempotent with TROUBLE-REGISTRY-013 (SPEC-06 §2.4).
func TestHarnessRejectsNonIdempotent(t *testing.T) {
	rec := &recorder{t: t}
	err := obligations(rec, badmodule.NonIdempotent{}, []Fixture{{
		Name:        "badmodule.nonidempotent/counter",
		Args:        map[string]any{"path": "{tmp}/counter"},
		Mutates:     true,
		SecondApply: secondOKNoop,
	}}, "")

	if len(rec.msgs) == 0 {
		t.Fatal("the harness reported no failure for a deliberately non-idempotent module: " +
			"a harness that quietly passes a broken module is itself a CI failure")
	}
	if code := registry.CodeOf(err); code != types.CodeRegistry013 {
		t.Fatalf("the harness failure must be %s, got %q (%v); reported: %s",
			types.CodeRegistry013, code, err, strings.Join(rec.msgs, " | "))
	}
	if !strings.Contains(err.Error(), "converge") && !strings.Contains(err.Error(), "one mutation") {
		t.Errorf("the failure should name the violated obligation (obligation 1: the second Apply must converge with exactly one mutation), got: %v", err)
	}
}

// TestHarnessRejectsNoCheckMode asserts that NewWith/Register refuses
// badmodule/nocheckmode with TROUBLE-REGISTRY-014 at registration (SPEC-06 §3.2,
// AC-23 condition 2), before any call can reach a target.
func TestHarnessRejectsNoCheckMode(t *testing.T) {
	deps := registry.RegistryDeps{
		Append: func(rec types.Record) (types.Record, error) { return rec, nil },
		Scrub: func(target string, in []byte) (types.ScrubResult, error) {
			return types.ScrubResult{Value: in, BytesIn: len(in)}, nil
		},
	}
	if _, err := registry.NewWith(deps, []types.Module{badmodule.NoCheckMode{}}); err == nil {
		t.Fatal("registry.NewWith registered a mutating module with check_mode=false: " +
			"a mutating module without a dry run is exactly the defect AC-23 condition 2 forbids")
	} else if code := registry.CodeOf(err); code != types.CodeRegistry014 {
		t.Fatalf("NewWith must refuse the module with %s, got %q (%v)", types.CodeRegistry014, code, err)
	}

	// The harness's own descriptor obligation must refuse it as well, so a module
	// that skipped the registry still cannot pass conformance.
	rec := &recorder{t: t}
	derr := obligations(rec, badmodule.NoCheckMode{}, nil, "")
	if code := registry.CodeOf(derr); code != types.CodeRegistry013 {
		t.Fatalf("the harness's descriptor obligation must refuse a mutating check_mode=false module with %s, got %q (%v)",
			types.CodeRegistry013, code, derr)
	}
	if !strings.Contains(derr.Error(), "check_mode") {
		t.Errorf("the descriptor failure should name check_mode, got: %v", derr)
	}
}

// TestHarnessPassesCorrectModule is the positive control: a correct convergent
// module and a correct pure module both pass the harness.
func TestHarnessPassesCorrectModule(t *testing.T) {
	t.Run("convergent", func(t *testing.T) {
		rec := &recorder{t: t}
		err := obligations(rec, convergeTestModule{}, []Fixture{{
			Name:        "testkit.converge/probe",
			Args:        map[string]any{"path": "{tmp}/probe.txt"},
			Mutates:     true,
			SecondApply: secondOKNoop,
		}}, "")
		if err != nil || len(rec.msgs) != 0 {
			t.Fatalf("the harness rejected a correct convergent module: %v %v", err, rec.msgs)
		}
	})
	t.Run("pure", func(t *testing.T) {
		rec := &recorder{t: t}
		err := obligations(rec, pureTestModule{}, []Fixture{{
			Name:        "testkit.read/probe",
			Args:        map[string]any{"path": "{tmp}/probe.txt"},
			Mutates:     false,
			SecondApply: secondPure,
		}}, "")
		if err != nil || len(rec.msgs) != 0 {
			t.Fatalf("the harness rejected a correct pure module: %v %v", err, rec.msgs)
		}
	})
}

// convergeTestModule is a correct convergent module: it drives its target file to
// the content "x" and its Check observes when that state is already reached. It is
// the positive control of the two negative tests.
type convergeTestModule struct{}

type convergeArgs struct {
	Path string `json:"path"`
}

// The control's schema is deliberately built by the same generator the shipped
// modules use, so the harness walks the real dialect and the real keyword subset.
var convergeDescriptor = types.Descriptor{
	Name:        "testkit.converge",
	Version:     1,
	Schema:      schemagen.Generate("testkit.converge", 1, convergeArgs{}),
	Scopes:      []string{"file:write"},
	Idempotency: types.IdemConvergent,
	CheckMode:   true,
	TimeoutS:    5,
	Mutating:    true,
}

const convergeTarget = "x"

func (convergeTestModule) Descriptor() types.Descriptor { return convergeDescriptor }

func (convergeTestModule) Check(_ context.Context, args map[string]any) (types.Diff, error) {
	path, err := probePath(args)
	if err != nil {
		return types.Diff{}, err
	}
	current, err := os.ReadFile(path)
	if err != nil {
		return types.Diff{}, types.ErrPermanent
	}
	if string(current) == convergeTarget {
		return types.Diff{Empty: true, Summary: "already at the described state"}, nil
	}
	return types.Diff{Entries: []types.DiffEntry{{Path: path, Before: string(current), After: convergeTarget}}}, nil
}

func (convergeTestModule) Apply(_ context.Context, args map[string]any) (types.Result, error) {
	path, err := probePath(args)
	if err != nil {
		return types.Result{}, err
	}
	current, err := os.ReadFile(path)
	if err != nil {
		return types.Result{}, types.ErrPermanent
	}
	if string(current) == convergeTarget {
		return types.Result{Changed: false, Output: map[string]any{"path": path}}, nil
	}
	if err := os.WriteFile(path, []byte(convergeTarget), 0o644); err != nil {
		return types.Result{}, types.ErrPermanent
	}
	return types.Result{
		Changed: true,
		Applied: []types.DiffEntry{{Path: path, Before: string(current), After: convergeTarget}},
		Output:  map[string]any{"path": path},
	}, nil
}

func (convergeTestModule) Verify(_ context.Context, args map[string]any) (types.VerifyResult, error) {
	path, err := probePath(args)
	if err != nil {
		return types.VerifyResult{}, err
	}
	current, err := os.ReadFile(path)
	if err != nil {
		return types.VerifyResult{}, types.ErrPermanent
	}
	return types.VerifyResult{OK: string(current) == convergeTarget, Method: "recheck"}, nil
}

// pureTestModule is a correct pure module: Apply is a read that carries its
// payload in Output.
type pureTestModule struct{}

var pureDescriptor = types.Descriptor{
	Name:        "testkit.read",
	Version:     1,
	Schema:      schemagen.Generate("testkit.read", 1, convergeArgs{}),
	Scopes:      []string{"file:read"},
	Idempotency: types.IdemPure,
	CheckMode:   true,
	TimeoutS:    5,
	Mutating:    false,
}

func (pureTestModule) Descriptor() types.Descriptor { return pureDescriptor }

func (pureTestModule) Check(_ context.Context, args map[string]any) (types.Diff, error) {
	path, err := probePath(args)
	if err != nil {
		return types.Diff{}, err
	}
	current, err := os.ReadFile(path)
	if err != nil {
		return types.Diff{}, types.ErrPermanent
	}
	return types.Diff{Empty: true, Summary: fmt.Sprintf("bytes=%d", len(current))}, nil
}

func (pureTestModule) Apply(_ context.Context, args map[string]any) (types.Result, error) {
	path, err := probePath(args)
	if err != nil {
		return types.Result{}, err
	}
	current, err := os.ReadFile(path)
	if err != nil {
		return types.Result{}, types.ErrPermanent
	}
	return types.Result{Changed: false, Output: map[string]any{"path": path, "bytes": len(current)}}, nil
}

func (pureTestModule) Verify(_ context.Context, args map[string]any) (types.VerifyResult, error) {
	return types.VerifyResult{OK: true, Method: "recheck"}, nil
}

func probePath(args map[string]any) (string, error) {
	path, _ := args["path"].(string)
	if path == "" {
		return "", types.ErrPermanent
	}
	return path, nil
}
