// Package testkit is the conformance harness every shipped module must pass
// (SPEC-06 §2.4, AC-23).
//
// It drives a module through the three mandatory obligations of §2.4, in order,
// and fails with TROUBLE-REGISTRY-013 when one of them is violated — a harness
// that quietly passes a broken module is itself a CI failure:
//
//  0. the descriptor contract (schema, dialect, keyword closure, scopes,
//     idempotency class, check_mode, timeout bounds);
//  1. idempotency: double-apply is ok/ok with exactly one observed mutation;
//  2. check-vs-apply equivalence: normalize(Check(args).Entries) ==
//     normalize(Apply(args).Applied) and Check after Apply is empty;
//  3. schema-violation rejection: every malformed args set is refused with
//     TROUBLE-REGISTRY-002 before Check or Apply is reached, with the target's
//     content state unchanged.
//
// The harness calls m.Check / m.Apply / m.Verify directly. It never goes through
// Registry.Call: the six-stage contract, the ledger records and the replay guard
// are the registry's own contract (SPEC-06 §2.3, §3.4) and are covered by its own
// tests.
//
// Fixtures are data (a Fixture is a JSON object under a fixtures directory) and
// their Args carry absolute paths under the fixture temp root, spelled with the
// "{tmp}" placeholder. Two harness conventions keep the Fixture struct exactly as
// §2.4 freezes it, with no setup hook and no fifth field:
//
//   - "{tmp}" is replaced by the fixture's temp root before every call;
//   - a path-shaped argument that does not exist under that root is created empty
//     first, so a fixture's baseline is well defined. RunAll additionally copies
//     <dir>/seed/** into the temp root, so a fixture may reference a seeded file.
//
// The observation log of obligation 1 is the content state (size + sha256) of
// every path-shaped argument. A byte-identical rewrite is not a state change, and
// a unit-scoped module (no path-shaped argument) has an empty observation log,
// which the harness reports as a logged limitation rather than a silent pass.
package testkit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/registry"
	"github.com/totalwindupflightsystems/trouble/internal/registry/schemagen"
	"github.com/totalwindupflightsystems/trouble/internal/registry/validate"
	"github.com/totalwindupflightsystems/trouble/internal/types"
)

const (
	// reasonConformance is the harness failure's reason token.
	reasonConformance = "conformance_violation"

	// The three SecondApply values of §2.4.
	secondOKNoop  = "ok-noop"  // convergent: the second Apply converges to a no-op
	secondIdemKey = "idem-key" // once: the second Apply carries the same IdemKey
	secondPure    = "pure"     // pure: Apply is a read

	// placeholder is the temp-root placeholder of fixture args.
	placeholder = "{tmp}"

	// seedDirName is the subdirectory of the fixtures directory copied into the
	// temp root before a fixture runs (RunAll only).
	seedDirName = "seed"

	// timeoutSlack is added to a descriptor's own budget so a hung module fails
	// the suite instead of hanging it.
	timeoutSlack = 5 * time.Second

	// maxDescriptorTimeoutS is the hard cap over every descriptor timeout
	// (registry.max_timeout_s, SPEC-06 §4.3).
	maxDescriptorTimeoutS = 60
)

// Fixture is one conformance case (SPEC-06 §2.4). Name is "<module>/<case>".
type Fixture struct {
	Name        string         `json:"name"`         // "file.patch/already-applied"
	Args        map[string]any `json:"args"`         // absolute paths always under the fixture temp root
	Mutates     bool           `json:"mutates"`      // true when Apply is expected to change state
	SecondApply string         `json:"second_apply"` // "ok-noop" (convergent) | "idem-key" (once) | "pure"
}

// reporter is the testing surface the harness needs. *testing.T satisfies it, and
// the negative tests supply a recording implementation: a test that asserts the
// harness fails cannot use a real subtest, because a failing subtest fails its
// parent test.
type reporter interface {
	Helper()
	Errorf(format string, args ...any)
	TempDir() string
}

// modulePanic is a module that panicked. It is a harness failure (013) when a
// module is run directly through Run, and a skip in RunAll, where the module
// bodies may still be landing.
type modulePanic struct{ value any }

func (p *modulePanic) Error() string { return fmt.Sprintf("module panicked: %v", p.value) }

// failure builds a conformance harness failure: TROUBLE-REGISTRY-013 (permanent),
// recognisable through registry.CodeOf (SPEC-06 §5).
func failure(format string, args ...any) error {
	return &registry.Error{
		Code:   types.CodeRegistry013,
		Class:  types.ErrClassPermanent,
		Stage:  "conformance",
		Reason: reasonConformance,
		Detail: fmt.Sprintf(format, args...),
	}
}

// TestDescriptor is obligation 0: schema presence, dialect, the closed keyword
// subset, the scope list, the idempotency class, CheckMode and timeout bounds.
func TestDescriptor(t *testing.T, m types.Module) {
	t.Helper()
	if err := describe(m); err != nil {
		t.Error(err)
	}
}

// TestIdempotency is obligation 1 for one fixture.
func TestIdempotency(t *testing.T, m types.Module, f Fixture) {
	t.Helper()
	if err := idempotency(m, f, t.TempDir()); err != nil {
		t.Error(err)
	}
}

// TestCheckApplyEquivalence is obligation 2 for one fixture.
func TestCheckApplyEquivalence(t *testing.T, m types.Module, f Fixture) {
	t.Helper()
	if err := equivalence(m, f, t.TempDir()); err != nil {
		t.Error(err)
	}
}

// TestSchemaRejection is obligation 3: every malformed args set in bad must be
// refused with TROUBLE-REGISTRY-002 before Check or Apply is reached, with the
// target inert. A nil bad slice means "derive the cases from the descriptor"
// (missing required key, wrong type, unknown key, out-of-range value, bad regex
// target, bad enum member, oneOf violation), which is what Run and RunAll use.
func TestSchemaRejection(t *testing.T, m types.Module, bad []map[string]any) {
	t.Helper()
	root := t.TempDir()
	var base map[string]any
	if bad == nil {
		base, bad = syntheticBadCases(m.Descriptor(), root)
	}
	if err := rejectBadArgs(m, base, bad); err != nil {
		t.Error(err)
	}
}

// Run drives the three obligations, in order, over the given fixtures. Every
// violation is reported with a TROUBLE-REGISTRY-013 error (SPEC-06 §2.4).
func Run(t *testing.T, m types.Module, fixtures ...Fixture) {
	t.Helper()
	if err := obligations(t, m, fixtures, ""); err != nil {
		// obligations already reported the failure through t; this branch exists
		// so the intent (a broken module fails the test) is explicit.
		t.Logf("conformance failed with %s: %v", registry.CodeOf(err), err)
	}
}

// RunAll drives every registered module with every fixture found in dir
// (SPEC-06 §2.4). Fixture names are "<module>/<case>".
//
// A module whose Check still returns an error — an unimplemented body, a missing
// target, an absent D-Bus, a panic — is skipped for that fixture with the reason
// logged: the three obligations presuppose a runnable module, and the module's own
// unit tests own that path. A fixture naming a module that is not in ms is a hard
// error: a fixture for a module that does not exist is a broken fixture.
func RunAll(t *testing.T, ms map[string]types.Module, dir string) {
	t.Helper()
	fixtures, err := LoadFixtures(dir)
	if err != nil {
		t.Fatalf("testkit: %v", err)
	}
	byModule := map[string][]Fixture{}
	for _, f := range fixtures {
		mod := moduleOf(f.Name)
		byModule[mod] = append(byModule[mod], f)
	}
	names := make([]string, 0, len(ms))
	for name := range ms {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		name, m := name, ms[name]
		t.Run(name, func(t *testing.T) {
			t.Helper()
			TestDescriptor(t, m)
			TestSchemaRejection(t, m, nil)
			for _, f := range byModule[name] {
				f := f
				t.Run(fixtureOf(f.Name), func(t *testing.T) {
					t.Helper()
					if reason := notRunnable(m, f, seededRoot(t, dir)); reason != nil {
						t.Skipf("module %s is not runnable yet (%v)", name, reason)
					}
					for _, obligation := range []func() error{
						func() error { return idempotency(m, f, seededRoot(t, dir)) },
						func() error { return equivalence(m, f, seededRoot(t, dir)) },
					} {
						if err := guard(obligation); err != nil {
							if isNotRunnable(err) {
								t.Skipf("module %s is not runnable yet (%v)", name, err)
							}
							t.Error(err)
						}
					}
				})
			}
		})
	}

	for _, f := range fixtures {
		if _, ok := ms[moduleOf(f.Name)]; !ok {
			t.Errorf("fixture %q names module %q, which is not registered", f.Name, moduleOf(f.Name))
		}
	}
}

// LoadFixtures reads every *.json file under dir. A file holds one Fixture or an
// array of them. The result is sorted by name (SPEC-06 §2.4).
func LoadFixtures(dir string) ([]Fixture, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("fixtures directory %s: %w", dir, err)
	}
	var out []Fixture
	seen := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("fixture %s: %w", path, err)
		}
		var list []Fixture
		if jerr := json.Unmarshal(raw, &list); jerr != nil {
			var one Fixture
			if jerr2 := json.Unmarshal(raw, &one); jerr2 != nil {
				return nil, fmt.Errorf("fixture %s: %v", path, jerr)
			}
			list = []Fixture{one}
		}
		for _, f := range list {
			if err := checkFixture(f); err != nil {
				return nil, fmt.Errorf("fixture %s: %w", path, err)
			}
			if prev, dup := seen[f.Name]; dup {
				return nil, fmt.Errorf("fixture %q is defined twice (%s and %s)", f.Name, prev, e.Name())
			}
			seen[f.Name] = e.Name()
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// checkFixture refuses a malformed fixture, so a typo fails the suite instead of
// silently reducing coverage.
func checkFixture(f Fixture) error {
	if strings.TrimSpace(f.Name) == "" {
		return errors.New("fixture without a name")
	}
	if moduleOf(f.Name) == "" || fixtureOf(f.Name) == "" {
		return fmt.Errorf("fixture %q must be named <module>/<case>", f.Name)
	}
	if f.Args == nil {
		return fmt.Errorf("fixture %q has no args (use {} for a module that takes none)", f.Name)
	}
	switch f.SecondApply {
	case "", secondOKNoop, secondIdemKey, secondPure:
	default:
		return fmt.Errorf("fixture %q declares second_apply=%q, want ok-noop|idem-key|pure", f.Name, f.SecondApply)
	}
	return nil
}

// obligations runs the three obligations in order through rep, reporting every
// violation and returning the first one (nil when the module conforms).
func obligations(rep reporter, m types.Module, fixtures []Fixture, seedDir string) error {
	rep.Helper()
	var first error
	report := func(err error) {
		if err == nil {
			return
		}
		if first == nil {
			first = err
		}
		rep.Errorf("%v", err)
	}
	report(describe(m))
	for _, f := range fixtures {
		report(idempotency(m, f, fixtureRoot(rep, seedDir)))
		report(equivalence(m, f, fixtureRoot(rep, seedDir)))
	}
	root := fixtureRoot(rep, seedDir)
	report(rejectBadArgs(m, nil, derivedBadCases(m, root)))
	return first
}

// guard converts a module panic into an error, so a broken module is a reported
// failure instead of a crashing test binary.
func guard(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &modulePanic{value: r}
		}
	}()
	return fn()
}

// isNotRunnable reports whether err means "the module is not implemented or not
// runnable in this environment" rather than "the module violates the contract".
func isNotRunnable(err error) bool {
	var p *modulePanic
	if errors.As(err, &p) {
		return true
	}
	return errors.Is(err, types.ErrPermanent)
}

// notRunnable probes the fixture with Check: any error means the three
// obligations cannot be evaluated (no target, no D-Bus, unimplemented body). The
// probe runs in its own temp root so it cannot disturb an obligation's baseline.
func notRunnable(m types.Module, f Fixture, root string) error {
	return guard(func() error {
		args, err := prepareArgs(m, f, root)
		if err != nil {
			return err
		}
		ctx, cancel := callContext(m.Descriptor())
		defer cancel()
		_, err = m.Check(ctx, args)
		return err
	})
}

// fixtureRoot returns a fresh temp root, seeded when the harness was given a
// fixtures directory (RunAll) — obligations that mutate state must not share a
// baseline.
func fixtureRoot(rep reporter, seedDir string) string {
	root := rep.TempDir()
	if seedDir != "" {
		_ = seedRoot(root, seedDir)
	}
	return root
}

// seededRoot returns a fresh, seeded temp root for a *testing.T.
func seededRoot(t *testing.T, dir string) string {
	t.Helper()
	root := t.TempDir()
	if dir != "" {
		if err := seedRoot(root, filepath.Join(dir, seedDirName)); err != nil {
			t.Logf("testkit: seed %s: %v", dir, err)
		}
	}
	return root
}

// seedRoot copies a seed tree into root (missing seed tree = no-op).
func seedRoot(root, seed string) error {
	if seed == "" {
		return nil
	}
	if _, err := os.Stat(seed); err != nil {
		return nil
	}
	return filepath.WalkDir(seed, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(seed, path)
		if err != nil {
			return err
		}
		dst := filepath.Join(root, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
}

// describe checks the descriptor contract of obligation 0.
func describe(m types.Module) error {
	d := m.Descriptor()
	var bad []string
	if d.Name == "" {
		bad = append(bad, "descriptor without a name")
	}
	if d.Version <= 0 {
		bad = append(bad, fmt.Sprintf("%s: version must be a monotonic int > 0, got %d", d.Name, d.Version))
	}
	if len(d.Schema) == 0 {
		bad = append(bad, fmt.Sprintf("%s: descriptor without a schema", d.Name))
	} else {
		if got, _ := d.Schema["$schema"].(string); got != schemagen.Dialect {
			bad = append(bad, fmt.Sprintf("%s: schema dialect is %q, want %q", d.Name, got, schemagen.Dialect))
		}
		want := fmt.Sprintf("%s%s@%d", schemagen.IDBase, d.Name, d.Version)
		if got, _ := d.Schema["$id"].(string); got != want {
			bad = append(bad, fmt.Sprintf("%s: $id is %q, want %q", d.Name, got, want))
		}
		if ap, ok := d.Schema["additionalProperties"].(bool); !ok || ap {
			bad = append(bad, fmt.Sprintf("%s: schema must declare additionalProperties:false", d.Name))
		}
		if err := validate.DialectClosed(d.Schema); err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", d.Name, err))
		}
		if err := bannedCommandKeys(d.Schema); err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", d.Name, err))
		}
	}
	if len(d.Scopes) == 0 {
		bad = append(bad, fmt.Sprintf("%s: descriptor without scopes (deny by default)", d.Name))
	}
	for _, s := range d.Scopes {
		if !scopeShape(s) {
			bad = append(bad, fmt.Sprintf("%s: scope %q is not <namespace>:<verb>", d.Name, s))
		}
	}
	if !d.Idempotency.Valid() {
		bad = append(bad, fmt.Sprintf("%s: unknown idempotency class %q", d.Name, d.Idempotency))
	}
	if d.Mutating && !d.CheckMode {
		bad = append(bad, fmt.Sprintf("%s: mutating module declares check_mode=false (AC-23 condition 2)", d.Name))
	}
	if d.TimeoutS < 1 || d.TimeoutS > maxDescriptorTimeoutS {
		bad = append(bad, fmt.Sprintf("%s: timeout_s %d is outside 1..%d", d.Name, d.TimeoutS, maxDescriptorTimeoutS))
	}
	if len(bad) == 0 {
		return nil
	}
	return failure("descriptor: %s", strings.Join(bad, "; "))
}

func scopeShape(s string) bool {
	parts := strings.Split(s, ":")
	return len(parts) == 2 && parts[0] != "" && parts[1] != ""
}

// bannedCommandKeys refuses a schema that accepts an arbitrary command, so the
// harness catches a module the registry would refuse at boot (SPEC-06 §3.11).
func bannedCommandKeys(schema map[string]any) error {
	props, _ := schema["properties"].(map[string]any)
	for _, banned := range []string{"command", "cmd", "argv", "shell", "script"} {
		if _, ok := props[banned]; ok {
			return fmt.Errorf("schema declares a %q args key: no module may accept an arbitrary command", banned)
		}
	}
	return nil
}

// idempotency is obligation 1 (SPEC-06 §2.4): double-apply is ok/ok with exactly
// one observed mutation.
func idempotency(m types.Module, f Fixture, root string) error {
	d := m.Descriptor()
	args, err := prepareArgs(m, f, root)
	if err != nil {
		return failure("fixture %s: %v", f.Name, err)
	}
	kind := fixtureKind(d, f)
	if kind == secondPure && f.Mutates {
		return failure("fixture %s declares mutates=true for the pure module %s: the fixture is wrong", f.Name, d.Name)
	}
	paths := pathsOf(args)
	obs := newObserver(paths)

	before := obs.snapshot()
	r1, err1 := apply(m, args)
	if err1 != nil {
		return failure("fixture %s: first Apply failed: %v", f.Name, err1)
	}
	afterFirst := obs.snapshot()
	r2, err2 := apply(m, args)
	if err2 != nil {
		return failure("fixture %s: second Apply (same args, same IdemKey) failed: %v", f.Name, err2)
	}
	afterSecond := obs.snapshot()

	mutations := 0
	if afterFirst != before {
		mutations++
	}
	if afterSecond != afterFirst {
		mutations++
	}
	observed := len(paths) > 0
	detail := fmt.Sprintf(" [changed1=%t applied1=%d changed2=%t applied2=%d mutations=%d observed_paths=%d]",
		r1.Changed, len(r1.Applied), r2.Changed, len(r2.Applied), mutations, len(paths))
	fail := func(format string, a ...any) error {
		return failure("fixture %s: %s%s", f.Name, fmt.Sprintf(format, a...), detail)
	}

	switch kind {
	case secondPure:
		if r1.Changed || len(r1.Applied) > 0 {
			return fail("a pure module's Apply is a read: it must return Changed=false with an empty Applied")
		}
		if r2.Changed || len(r2.Applied) > 0 {
			return fail("the second Apply of a pure module must not change anything")
		}
		if observed && mutations != 0 {
			return fail("a pure module must not mutate its target")
		}
	case secondIdemKey:
		if !r1.Changed {
			return fail("the first Apply of a once module is an event and must report Changed=true")
		}
		// The module never sees the IdemKey (the surface is frozen), so the
		// module-level half of "never re-execute an applied call" is that after a
		// successful event the module's own dry run observes nothing left to do;
		// the registry's replay guard (§3.4) is the other half and is covered by
		// the registry's own tests.
		d1, err := check(m, args)
		if err != nil {
			return failure("fixture %s: Check after Apply failed: %v", f.Name, err)
		}
		if !d1.Empty {
			return fail("Check after a successful once-module Apply must be Diff{Empty:true} so a replay is a no-op")
		}
		if observed {
			want := 1
			if r2.Changed {
				want = 2 // the event really ran again; the registry short-circuits it
			}
			if mutations != want {
				return fail("expected %d observed mutation(s)", want)
			}
		}
	default: // ok-noop (convergent)
		if !r1.Changed || len(r1.Applied) == 0 {
			return fail("the first Apply of a convergent module must return Changed=true with a non-empty Applied")
		}
		if r2.Changed || len(r2.Applied) > 0 {
			return fail("a second Apply with identical args must converge: Changed=false and an empty Applied")
		}
		if observed && mutations != 1 {
			return fail("double-apply must record exactly one mutation")
		}
	}
	// Honest bound: a unit-scoped module has no path-shaped argument, so the
	// observation log cannot see a re-executed mutation.
	return nil
}

// equivalence is obligation 2 (SPEC-06 §2.4): the diff Check predicted is exactly
// what Apply changed, and Check after Apply is empty.
func equivalence(m types.Module, f Fixture, root string) error {
	d := m.Descriptor()
	args, err := prepareArgs(m, f, root)
	if err != nil {
		return failure("fixture %s: %v", f.Name, err)
	}
	if fixtureKind(d, f) == secondPure {
		d0, err := check(m, args)
		if err != nil {
			return failure("fixture %s: Check failed: %v", f.Name, err)
		}
		if !d0.Empty {
			return failure("fixture %s: a pure module's Check must return Diff{Empty:true}, got %d entries", f.Name, len(d0.Entries))
		}
		r, err := apply(m, args)
		if err != nil {
			return failure("fixture %s: Apply failed: %v", f.Name, err)
		}
		switch {
		case r.Changed:
			return failure("fixture %s: a pure module's Apply must return Changed=false", f.Name)
		case len(r.Applied) > 0:
			return failure("fixture %s: a pure module's Apply must return an empty Applied, got %d entries", f.Name, len(r.Applied))
		case len(r.Output) == 0:
			return failure("fixture %s: a pure module's Apply must carry its payload in Output", f.Name)
		}
		return nil
	}
	d0, err := check(m, args)
	if err != nil {
		return failure("fixture %s: Check (dry run) failed: %v", f.Name, err)
	}
	r, err := apply(m, args)
	if err != nil {
		return failure("fixture %s: Apply failed: %v", f.Name, err)
	}
	d1, err := check(m, args)
	if err != nil {
		return failure("fixture %s: Check after Apply failed: %v", f.Name, err)
	}
	if !d1.Empty {
		return failure("fixture %s: Check after Apply must be empty, got %d entries (%s)", f.Name, len(d1.Entries), d1.Summary)
	}
	want := normalizeEntries(d0.Entries)
	got := normalizeEntries(r.Applied)
	if diff := diffEntrySets(want, got); diff != "" {
		return failure("fixture %s: Check predicted a different change set than Apply reported: %s", f.Name, diff)
	}
	return nil
}

// rejectBadArgs is obligation 3 (SPEC-06 §2.4): every malformed args set must be
// refused by validate with TROUBLE-REGISTRY-002, before Check or Apply is reached
// and with the target's content state unchanged.
func rejectBadArgs(m types.Module, base map[string]any, cases []map[string]any) error {
	target := &spyModule{Module: m}
	if base != nil {
		if err := validateArgs(m, base); err != nil {
			return failure("the harness could not build a schema-valid args set for %s: %v", m.Descriptor().Name, err)
		}
	}
	observed := map[string]bool{}
	for _, p := range pathsOf(base) {
		if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(p, nil, 0o644); err != nil {
				return err
			}
		}
		observed[p] = true
	}
	for _, c := range cases {
		for _, p := range pathsOf(c) {
			if _, err := os.Stat(p); err == nil {
				observed[p] = true
			}
		}
	}
	paths := make([]string, 0, len(observed))
	for p := range observed {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	obs := newObserver(paths)
	before := obs.snapshot()

	for _, c := range cases {
		norm, err := validate.Normalize(c)
		if err != nil {
			return failure("args %s are not JSON-representable: %v", jsonText(c), err)
		}
		if verr := validate.Args(m.Descriptor().Schema, norm); verr == nil {
			return failure("validate accepted the malformed args %s (every case of TestSchemaRejection must be refused with %s)",
				jsonText(c), types.CodeRegistry002)
		} else if code := validateCode(verr); code != types.CodeRegistry002 {
			return failure("malformed args %s were refused with %s, want %s: %v", jsonText(c), code, types.CodeRegistry002, verr)
		}
	}
	if after := obs.snapshot(); after != before {
		return failure("a rejected args set touched the target: validate runs before any IO beyond target extraction")
	}
	if n := target.calls(); n != 0 {
		return failure("the harness reached the module %d time(s) while rejecting malformed args: rejection must precede Check and Apply", n)
	}
	return nil
}

// spyModule counts module invocations, so obligation 3 can prove that a rejected
// args set never reaches the module.
type spyModule struct {
	types.Module
	checks  int
	applies int
	verifys int
}

func (s *spyModule) calls() int { return s.checks + s.applies + s.verifys }

func (s *spyModule) Check(ctx context.Context, args map[string]any) (types.Diff, error) {
	s.checks++
	return s.Module.Check(ctx, args)
}

func (s *spyModule) Apply(ctx context.Context, args map[string]any) (types.Result, error) {
	s.applies++
	return s.Module.Apply(ctx, args)
}

func (s *spyModule) Verify(ctx context.Context, args map[string]any) (types.VerifyResult, error) {
	s.verifys++
	return s.Module.Verify(ctx, args)
}

// validateArgs normalizes and validates args against the module's schema, the way
// the registry's validate stage does before it calls the module.
func validateArgs(m types.Module, args map[string]any) error {
	norm, err := validate.Normalize(args)
	if err != nil {
		return err
	}
	return validate.Args(m.Descriptor().Schema, norm)
}

func validateCode(err error) types.ErrorCode {
	var ve *validate.Error
	if errors.As(err, &ve) {
		return ve.Code
	}
	return ""
}

// prepareArgs expands "{tmp}", gives missing path-shaped arguments an empty
// baseline, and normalizes the args the way the registry's validate stage does
// before it calls a module.
func prepareArgs(m types.Module, f Fixture, root string) (map[string]any, error) {
	args := map[string]any{}
	for k, v := range f.Args {
		args[k] = expandValue(v, root)
	}
	for _, p := range pathsOf(args) {
		if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(p, nil, 0o644); err != nil {
				return nil, err
			}
		}
	}
	norm, err := validate.Normalize(args)
	if err != nil {
		return nil, fmt.Errorf("args are not JSON-representable: %w", err)
	}
	if verr := validate.Args(m.Descriptor().Schema, norm); verr != nil {
		return nil, fmt.Errorf("the fixture's own args do not satisfy %s's schema: %v", m.Descriptor().Name, verr)
	}
	return norm, nil
}

func expandValue(v any, root string) any {
	switch t := v.(type) {
	case string:
		return strings.ReplaceAll(t, placeholder, root)
	case []any:
		out := make([]any, 0, len(t))
		for _, item := range t {
			out = append(out, expandValue(item, root))
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, item := range t {
			out[k] = expandValue(item, root)
		}
		return out
	}
	return v
}

// pathsOf lists the absolute, path-shaped argument values of an args map: the
// fixture's observation log derives from these.
func pathsOf(args map[string]any) []string {
	seen := map[string]bool{}
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case string:
			if filepath.IsAbs(t) {
				seen[filepath.Clean(t)] = true
			}
		case []any:
			for _, item := range t {
				walk(item)
			}
		case map[string]any:
			for _, item := range t {
				walk(item)
			}
		}
	}
	for _, v := range args {
		walk(v)
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// observer is a fixture's observation log: the content state (size + sha256) of
// every path-shaped argument.
type observer struct{ paths []string }

func newObserver(paths []string) *observer { return &observer{paths: paths} }

func (o *observer) snapshot() string {
	var b strings.Builder
	for _, p := range o.paths {
		b.WriteString(p)
		b.WriteByte(0)
		st, err := os.Stat(p)
		if err != nil {
			b.WriteString("absent\n")
			continue
		}
		h := sha256.New()
		f, err := os.Open(p)
		if err != nil {
			b.WriteString("unreadable\n")
			continue
		}
		if _, err := io.Copy(h, f); err != nil {
			f.Close()
			b.WriteString("unreadable\n")
			continue
		}
		f.Close()
		fmt.Fprintf(&b, "%d:%s\n", st.Size(), hex.EncodeToString(h.Sum(nil)))
	}
	return b.String()
}

// normalizeEntries maps diff entries onto a path-keyed set of {before, after}
// values, with paths canonical and numbers as JSON literals (SPEC-06 §2.4).
func normalizeEntries(entries []types.DiffEntry) map[string]string {
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		out[canonicalKey(e.Path)] = jsonValue(e.Before) + "\x00" + jsonValue(e.After)
	}
	return out
}

// canonicalKey canonicalizes the path part of a diff entry key while preserving
// its suffix ("<file>#<key>", "<file>:<line>"), so two hunks of one file never
// collapse into one entry.
func canonicalKey(key string) string {
	if !strings.HasPrefix(key, "/") {
		return key
	}
	sep := len(key)
	if i := strings.IndexByte(key, '#'); i > 0 {
		sep = i
	}
	if i := strings.LastIndexByte(key, ':'); i > strings.LastIndexByte(key, '/') {
		if i < sep {
			sep = i
		}
	}
	head, tail := key[:sep], key[sep:]
	head = filepath.Clean(head)
	if resolved, err := filepath.EvalSymlinks(head); err == nil {
		head = resolved
	}
	return head + tail
}

func jsonValue(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var x any
	if err := dec.Decode(&x); err != nil {
		return string(b)
	}
	switch t := x.(type) {
	case json.Number:
		return "number:" + t.String()
	case nil:
		return "null"
	case string:
		return "string:" + t
	case bool:
		return "bool:" + strconv.FormatBool(t)
	}
	return string(b)
}

func diffEntrySets(want, got map[string]string) string {
	if len(want) == 0 && len(got) == 0 {
		return ""
	}
	var b strings.Builder
	for _, k := range sortedKeysOf(want) {
		w, ok := got[k]
		if !ok {
			fmt.Fprintf(&b, "Check predicted %s=%q but Apply did not report it; ", k, want[k])
			continue
		}
		if w != want[k] {
			fmt.Fprintf(&b, "%s: Check predicted %q, Apply reported %q; ", k, want[k], w)
		}
	}
	for _, k := range sortedKeysOf(got) {
		if _, ok := want[k]; !ok {
			fmt.Fprintf(&b, "Apply reported %s=%q but Check did not predict it; ", k, got[k])
		}
	}
	return strings.TrimSpace(b.String())
}

func sortedKeysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// fixtureKind resolves the SecondApply semantics: the fixture states it, and an
// absent value falls back to the descriptor's idempotency class.
func fixtureKind(d types.Descriptor, f Fixture) string {
	switch f.SecondApply {
	case secondOKNoop, secondIdemKey, secondPure:
		return f.SecondApply
	}
	switch d.Idempotency {
	case types.IdemPure:
		return secondPure
	case types.IdemOnce:
		return secondIdemKey
	}
	return secondOKNoop
}

func check(m types.Module, args map[string]any) (types.Diff, error) {
	ctx, cancel := callContext(m.Descriptor())
	defer cancel()
	return m.Check(ctx, args)
}

func apply(m types.Module, args map[string]any) (types.Result, error) {
	ctx, cancel := callContext(m.Descriptor())
	defer cancel()
	return m.Apply(ctx, args)
}

// callContext bounds one harness call by the descriptor's own budget plus slack,
// so a hung module fails the suite instead of hanging it.
func callContext(d types.Descriptor) (context.Context, context.CancelFunc) {
	budget := time.Duration(d.TimeoutS) * time.Second
	if budget <= 0 {
		budget = 5 * time.Second
	}
	return context.WithTimeout(context.Background(), budget+timeoutSlack)
}

// derivedBadCases builds the synthetic malformed args sets for a module whose
// fixtures are not shipped with explicit bad cases.
func derivedBadCases(m types.Module, root string) []map[string]any {
	_, cases := syntheticBadCases(m.Descriptor(), root)
	return cases
}

// syntheticBadCases builds a schema-valid base args set and the malformed variants
// of §2.4 obligation 3: missing required key, wrong type, unknown key,
// out-of-range value, bad regex target, bad enum member and oneOf violation.
func syntheticBadCases(d types.Descriptor, root string) (map[string]any, []map[string]any) {
	props := propertiesOf(d.Schema)
	base := syntheticValidArgs(d, root)
	var cases []map[string]any

	for _, name := range requiredOf(d.Schema) {
		c := cloneArgs(base)
		delete(c, name)
		cases = append(cases, c)
	}
	if len(base) > 0 || len(props) > 0 {
		c := cloneArgs(base)
		c["trouble_unknown_key"] = true
		cases = append(cases, c)
	}
	if name, _, ok := firstProp(props, "string"); ok {
		c := cloneArgs(base)
		c[name] = 1
		cases = append(cases, c)
	} else if name, _, ok := firstProp(props, "integer"); ok {
		c := cloneArgs(base)
		c[name] = "not-an-integer"
		cases = append(cases, c)
	}
	if name, p, ok := firstPropWith(props, "maximum"); ok {
		c := cloneArgs(base)
		c[name] = intOf(p["maximum"]) + 1
		cases = append(cases, c)
	} else if name, p, ok := firstPropWith(props, "minimum"); ok {
		c := cloneArgs(base)
		c[name] = intOf(p["minimum"]) - 1
		cases = append(cases, c)
	}
	if name, p, ok := firstPropWith(props, "pattern"); ok {
		pat, _ := p["pattern"].(string)
		if v, ok := nonMatching(pat); ok {
			c := cloneArgs(base)
			c[name] = v
			cases = append(cases, c)
		}
	}
	if name, p, ok := firstPropWith(props, "enum"); ok {
		members, _ := p["enum"].([]any)
		c := cloneArgs(base)
		c[name] = "__not_an_enum_member__"
		if !enumHas(members, "__not_an_enum_member__") {
			cases = append(cases, c)
		}
	}
	if alts := oneOfAlternatives(d.Schema); len(alts) > 0 {
		c := cloneArgs(base)
		for _, alt := range alts {
			delete(c, alt)
		}
		cases = append(cases, c)
	}
	return base, dedupeCases(cases)
}

// syntheticValidArgs builds the smallest args set the descriptor's schema accepts.
func syntheticValidArgs(d types.Descriptor, root string) map[string]any {
	props := propertiesOf(d.Schema)
	out := map[string]any{}
	for _, name := range requiredOf(d.Schema) {
		out[name] = sampleValue(name, props[name], root)
	}
	for _, alt := range oneOfAlternatives(d.Schema) {
		// Exactly one alternative must be satisfied: satisfy the first.
		if _, ok := out[alt]; !ok {
			out[alt] = sampleValue(alt, props[alt], root)
		}
		break
	}
	return out
}

// sampleValue samples one schema-valid value for a property.
func sampleValue(name string, schema map[string]any, root string) any {
	if len(schema) == 0 {
		return "sample"
	}
	if members, ok := schema["enum"].([]any); ok && len(members) > 0 {
		return members[0]
	}
	kind, _ := schema["type"].(string)
	switch kind {
	case "string":
		if name == "path" || strings.HasSuffix(name, "_path") || name == "file" {
			return filepath.Join(root, "inert.txt")
		}
		pat, _ := schema["pattern"].(string)
		if pat == "" {
			return "sample"
		}
		for _, cand := range []string{"sample", "trouble-sample.service", "payment-worker.service", "unit.service", "a.b-c_1"} {
			if re, err := regexp.Compile(pat); err == nil && re.MatchString(cand) {
				return cand
			}
		}
		return "sample"
	case "integer":
		n := 0
		if min, ok := intOfOK(schema["minimum"]); ok {
			n = min
		}
		if max, ok := intOfOK(schema["maximum"]); ok && n > max {
			n = max
		}
		return n
	case "boolean":
		return false
	case "array":
		count := 0
		if min, ok := intOfOK(schema["minItems"]); ok {
			count = min
		}
		item, _ := schema["items"].(map[string]any)
		out := make([]any, 0, count)
		for i := 0; i < count; i++ {
			out = append(out, sampleValue(name+"_item", item, root))
		}
		return out
	}
	return "sample"
}

func propertiesOf(schema map[string]any) map[string]map[string]any {
	raw, _ := schema["properties"].(map[string]any)
	out := make(map[string]map[string]any, len(raw))
	for k, v := range raw {
		if m, ok := v.(map[string]any); ok {
			out[k] = m
		}
	}
	return out
}

func requiredOf(schema map[string]any) []string {
	switch raw := schema["required"].(type) {
	case []any:
		out := make([]string, 0, len(raw))
		for _, v := range raw {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
		sort.Strings(out)
		return out
	case []string:
		out := append([]string(nil), raw...)
		sort.Strings(out)
		return out
	}
	return nil
}

// oneOfAlternatives flattens the "exactly one of these must be present" list the
// generator emits for file.patch.
func oneOfAlternatives(schema map[string]any) []string {
	raw, _ := schema["oneOf"].([]any)
	var out []string
	for _, sub := range raw {
		sub, ok := sub.(map[string]any)
		if !ok {
			continue
		}
		req, _ := sub["required"].([]any)
		if len(req) == 1 {
			if s, ok := req[0].(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func firstProp(props map[string]map[string]any, kind string) (string, map[string]any, bool) {
	for _, name := range sortedPropNames(props) {
		if t, _ := props[name]["type"].(string); t == kind {
			return name, props[name], true
		}
	}
	return "", nil, false
}

func firstPropWith(props map[string]map[string]any, keyword string) (string, map[string]any, bool) {
	for _, name := range sortedPropNames(props) {
		if _, ok := props[name][keyword]; ok {
			return name, props[name], true
		}
	}
	return "", nil, false
}

func sortedPropNames(props map[string]map[string]any) []string {
	out := make([]string, 0, len(props))
	for k := range props {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func nonMatching(pattern string) (string, bool) {
	for _, cand := range []string{"!!", "%%%", "\t", "____________________"} {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return "", false
		}
		if !re.MatchString(cand) {
			return cand, true
		}
	}
	return "", false
}

func enumHas(members []any, v any) bool {
	for _, m := range members {
		if fmt.Sprintf("%v", m) == fmt.Sprintf("%v", v) {
			return true
		}
	}
	return false
}

func intOf(v any) int {
	if n, ok := intOfOK(v); ok {
		return n
	}
	return 0
}

func intOfOK(v any) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, true
	case int64:
		return int(t), true
	case float64:
		return int(t), true
	case json.Number:
		n, err := t.Int64()
		if err != nil {
			return 0, false
		}
		return int(n), true
	}
	return 0, false
}

func cloneArgs(in map[string]any) map[string]any {
	out := make(map[string]any, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	return out
}

// dedupeCases drops cases that repeat an earlier one, so a report never carries
// the same malformed args set twice.
func dedupeCases(cases []map[string]any) []map[string]any {
	seen := map[string]bool{}
	out := make([]map[string]any, 0, len(cases))
	for _, c := range cases {
		key := jsonText(c)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, c)
	}
	return out
}

func jsonText(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// moduleOf and fixtureOf split a fixture name "<module>/<case>".
func moduleOf(name string) string {
	if i := strings.IndexByte(name, '/'); i >= 0 {
		return name[:i]
	}
	return name
}

func fixtureOf(name string) string {
	if i := strings.IndexByte(name, '/'); i >= 0 {
		return name[i+1:]
	}
	return name
}
