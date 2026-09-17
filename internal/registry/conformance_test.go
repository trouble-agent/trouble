package registry_test

// conformance_test.go is the AC-23 aggregate (SPEC-06 §7): the AC passes only
// when `make conformance` is green AND the deliberate non-idempotent module is
// rejected. The three obligations themselves are enforced by testkit.Run (see
// internal/registry/testkit_run_test.go and the harness's own negative tests);
// this file pins the two things only the registry's external view can pin — that
// the harness is wired into the shipped set, that a broken module cannot register,
// and that the Makefile target really runs the §2.4 commands.

import (
	"os"
	"strings"
	"testing"

	"github.com/totalwindupflightsystems/trouble/internal/registry"
	"github.com/totalwindupflightsystems/trouble/internal/registry/testkit/badmodule"
	"github.com/totalwindupflightsystems/trouble/internal/types"
)

func conformanceDeps() registry.RegistryDeps {
	return registry.RegistryDeps{
		Append: func(rec types.Record) (types.Record, error) { return rec, nil },
		Scrub: func(target string, in []byte) (types.ScrubResult, error) {
			return types.ScrubResult{Value: in, BytesIn: len(in)}, nil
		},
	}
}

// TestConformanceShippedModulesRegister asserts the 13-module set registers,
// which is the precondition every obligation runs under: a descriptor that does
// not validate (a mutating module without check_mode, a stale committed schema)
// fails here before a single call is made.
func TestConformanceShippedModulesRegister(t *testing.T) {
	r, err := registry.NewWith(conformanceDeps(), registry.ShippedModules())
	if err != nil {
		t.Fatalf("the shipped set must register (a failure here is %s): %v", types.CodeRegistry014, err)
	}
	mods := r.Modules()
	if len(mods) != 13 {
		t.Fatalf("the v0.1 module set is 13 modules, got %d", len(mods))
	}
	for _, d := range r.List() {
		if d.Mutating && !d.CheckMode {
			t.Errorf("%s is mutating with check_mode=false: AC-23 condition 2", d.Name)
		}
		if len(d.Schema) == 0 {
			t.Errorf("%s has no schema", d.Name)
		}
	}
}

// TestConformanceRejectsNoCheckModeModule is AC-23 condition 2 from the
// registry's side: the deliberate negative module never becomes callable.
func TestConformanceRejectsNoCheckModeModule(t *testing.T) {
	_, err := registry.NewWith(conformanceDeps(), []types.Module{badmodule.NoCheckMode{}})
	if code := registry.CodeOf(err); code != types.CodeRegistry014 {
		t.Fatalf("a mutating module with check_mode=false must be refused with %s, got %q (%v)",
			types.CodeRegistry014, code, err)
	}
}

// TestConformanceMakeTargetRunsTheSpecCommands pins the §2.4 CI commands to the
// `conformance` Makefile target: if someone edits the target, the commands the AC
// names stop being the ones CI runs, and this test says so.
func TestConformanceMakeTargetRunsTheSpecCommands(t *testing.T) {
	b, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	target := makeTarget(string(b), "conformance")
	if target == "" {
		t.Fatal("the repository Makefile has no `conformance` target (SPEC-06 §2.4 names `make conformance` as the one gate)")
	}
	// $(GO) is the Makefile's variable for the toolchain; compare the command
	// shapes the spec names, not the variable spelling.
	normalized := strings.ReplaceAll(target, "$(GO)", "go")
	for _, want := range []string{
		"go test ./internal/registry/... -count=1 -race",
		"TestHarnessRejectsNonIdempotent|TestHarnessRejectsNoCheckMode|TestSchemaDialectClosed",
	} {
		want := want
		if !strings.Contains(normalized, want) {
			t.Errorf("the `conformance` target does not run %q (SPEC-06 §2.4)", want)
		}
	}
}

// makeTarget returns the recipe body of one Makefile target.
func makeTarget(src, name string) string {
	lines := strings.Split(src, "\n")
	var out []string
	collecting := false
	for _, l := range lines {
		if strings.HasPrefix(l, name+":") {
			collecting = true
			continue
		}
		if collecting {
			if l == "" || strings.HasPrefix(l, "\t") {
				out = append(out, l)
				continue
			}
			break
		}
	}
	return strings.Join(out, "\n")
}
