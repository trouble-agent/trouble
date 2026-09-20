package registry_test

// testkit_run_test.go is the §7 row for the conformance harness (SPEC-06 §2.4,
// AC-23): testkit.RunAll over every registered module with the fixtures under
// internal/registry/testkit/testdata.
//
// It is the external test package (registry_test) on purpose: package testkit
// imports internal/registry for the TROUBLE-REGISTRY-013 error type, so an
// internal test file could not import it without an import cycle.

import (
	"testing"

	"github.com/trouble-agent/trouble/internal/registry"
	"github.com/trouble-agent/trouble/internal/registry/testkit"
	"github.com/trouble-agent/trouble/internal/types"
)

// TestConformanceRunAll runs the three obligations of §2.4 over the shipped module
// set. A module whose body is not implemented yet (its Check still returns
// types.ErrPermanent) is skipped by the harness with the module name, so the
// aggregate stays green while the module bodies land; a module that is implemented
// and violates an obligation fails here with TROUBLE-REGISTRY-013.
func TestConformanceRunAll(t *testing.T) {
	deps := registry.RegistryDeps{
		Append: func(rec types.Record) (types.Record, error) { return rec, nil },
		Scrub: func(target string, in []byte) (types.ScrubResult, error) {
			return types.ScrubResult{Value: in, BytesIn: len(in)}, nil
		},
	}
	r, err := registry.NewWith(deps, registry.ShippedModules())
	if err != nil {
		t.Fatalf("the shipped module set must register (a failure here is %s: descriptor or committed-schema drift): %v",
			types.CodeRegistry014, err)
	}
	modules := r.Modules()
	if len(modules) != 13 {
		t.Fatalf("the v0.1 module set is 13 modules, got %d", len(modules))
	}
	testkit.RunAll(t, modules, "testkit/testdata")
}
