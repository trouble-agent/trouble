package skills

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// library_test.go is SPEC-11 §7 item 13: the local SKILL.md library (§2b) and its
// typed step execution (§4.7a). Every fixture lives under t.TempDir(); no test
// starts a process, opens a socket or writes outside its own directory.

// fakeRunner is the StepRunner the library tests drive.
type fakeRunner struct {
	checks   []string
	applies  []string
	lastArgs map[string]any
	diff     types.Diff
	result   types.Result
	err      error
}

func (f *fakeRunner) Check(ctx context.Context, tool string, args map[string]any) (types.Diff, types.ToolCall, error) {
	f.checks = append(f.checks, tool)
	f.lastArgs = args
	if f.err != nil {
		return types.Diff{}, types.ToolCall{}, f.err
	}
	return f.diff, types.ToolCall{ID: "tc_check_" + tool, Module: tool, Mode: types.ModeCheck, Args: args}, nil
}

func (f *fakeRunner) Apply(ctx context.Context, tool string, args map[string]any) (types.Result, types.ToolCall, error) {
	f.applies = append(f.applies, tool)
	f.lastArgs = args
	if f.err != nil {
		return types.Result{}, types.ToolCall{}, f.err
	}
	return f.result, types.ToolCall{ID: "tc_apply_" + tool, Module: tool, Mode: types.ModeApply, Args: args}, nil
}

// goodSkill is a valid §2b file: a folded description, a nested args block and an
// args_json step.
const goodSkill = `---
name: %NAME%
description: >-
  The payment worker's queue wedges when the socket backlog fills; count the
  connections, then reload the unit.
version: 2
category: software-development
trouble:
  steps:
    - title: count the workers' sockets
      module: proc.connections
      args:
        pid: 4242
        proto: tcp
    - title: reload the unit
      module: service.reload
      args_json: '{"unit":"payment-worker.service"}'
---

Body prose. This fenced block is data and must never be executed:

` + "```bash" + `
systemctl restart payment-worker.service
` + "```" + `
`

func writeLibrary(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	// The library root must not be writable by another user (§2b rule 5).
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatalf("chmod root: %v", err)
	}
	writeSkillFile(t, root, "zeta-wedge", strings.ReplaceAll(goodSkill, "%NAME%", "zeta-wedge"))
	writeSkillFile(t, root, "alpha-wedge", strings.ReplaceAll(goodSkill, "%NAME%", "alpha-wedge"))
	writeSkillFile(t, root, "broken-wedge", "---\nname: broken-wedge\n")
	return root
}

func writeSkillFile(t *testing.T, root, name, body string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", dir, err)
	}
}

func libraryCfg(dir string) types.SkillsConfig {
	cfg := DefaultConfig()
	cfg.LocalEnabled = true
	cfg.LocalDir = dir
	return cfg
}

func newLibrary(t *testing.T, dir string, runner StepRunner, registered ...string) (*Library, *fakeLedger) {
	t.Helper()
	led := &fakeLedger{}
	clk := newFakeClock()
	led.now = clk.Now
	cfg := DefaultConfig()
	if dir != "" {
		cfg = libraryCfg(dir)
	}
	lib, err := NewLibrary(cfg, testDeps(t, led, clk, t.TempDir(), registered...), runner)
	if err != nil {
		t.Fatalf("NewLibrary: %v", err)
	}
	return lib, led
}

func TestLibrary_ScanParsesTheFrontmatterSubset(t *testing.T) {
	root := writeLibrary(t)
	lib, led := newLibrary(t, root, &fakeRunner{})

	all, err := lib.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("skills = %d, want 2 (the malformed file is refused, not fatal)", len(all))
	}
	// Name order, deterministic.
	if all[0].Name != "alpha-wedge" || all[1].Name != "zeta-wedge" {
		t.Errorf("order = %s,%s, want alpha-wedge,zeta-wedge", all[0].Name, all[1].Name)
	}
	skill := all[1]
	if !strings.Contains(skill.Description, "socket backlog fills") || !strings.Contains(skill.Description, "reload the unit") {
		t.Errorf("description = %q, want the folded block scalar", skill.Description)
	}
	if skill.Version != 2 {
		t.Errorf("version = %d, want 2", skill.Version)
	}
	if skill.Frontmatter["category"] != "software-development" {
		t.Errorf("frontmatter = %+v, want the extra scalar kept for reporting", skill.Frontmatter)
	}
	if len(skill.Steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(skill.Steps))
	}
	if skill.Steps[0].Module != "proc.connections" || skill.Steps[0].Title != "count the workers' sockets" {
		t.Errorf("step 0 = %+v, want the typed step", skill.Steps[0])
	}
	if got := skill.Steps[0].Args["pid"]; got != 4242 {
		t.Errorf("args[pid] = %#v, want the integer 4242", got)
	}
	if got := skill.Steps[0].Args["proto"]; got != "tcp" {
		t.Errorf("args[proto] = %#v, want the string tcp", got)
	}
	if got := skill.Steps[1].Args["unit"]; got != "payment-worker.service" {
		t.Errorf("args_json unit = %#v, want the parsed JSON object", got)
	}
	if skill.Digest == "" || len(skill.Digest) != 64 {
		t.Errorf("digest = %q, want hex64(sha256(file))", skill.Digest)
	}

	// The malformed file is refused with its reason, and the scan is still recorded.
	refusals := led.byPhase(PhaseLibraryRefused)
	if len(refusals) != 1 {
		t.Fatalf("library_refused records = %d, want 1", len(refusals))
	}
	if got := str(refusals[0].Payload, "reason"); got != ReasonFrontmatter {
		t.Errorf("refusal reason = %q, want %q", got, ReasonFrontmatter)
	}
	loaded := led.byPhase(PhaseLibraryLoaded)
	if len(loaded) != 1 {
		t.Fatalf("library_loaded records = %d, want 1", len(loaded))
	}
	if str(loaded[0].Payload, "digest") == "" {
		t.Error("library_loaded carries no digest")
	}
}

func TestLibrary_PlaysCompilesTypedSteps(t *testing.T) {
	root := writeLibrary(t)
	lib, _ := newLibrary(t, root, &fakeRunner{})

	plays, err := lib.Plays(context.Background())
	if err != nil {
		t.Fatalf("Plays: %v", err)
	}
	if len(plays) != 2 {
		t.Fatalf("plays = %d, want 2", len(plays))
	}
	p := plays[0]
	if p.Name != "alpha-wedge" || p.Version != 2 {
		t.Errorf("play = %+v, want the library skill", p)
	}
	if p.Source != "skill-local:alpha-wedge@2" {
		t.Errorf("source = %q, want skill-local:<name>@<version>", p.Source)
	}
	if len(p.Tasks) != 2 || p.Tasks[0].Tool != "proc.connections" || p.Tasks[1].Tool != "service.reload" {
		t.Fatalf("tasks = %+v, want one typed task per step", p.Tasks)
	}
	if p.Tasks[0].Args["pid"] != 4242 {
		t.Errorf("task args = %+v, want the literal args", p.Tasks[0].Args)
	}
}

func TestLibrary_SubsetRefusalsAreLoudAndComplete(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		reason string
	}{
		{"tab indent", "---\nname: %NAME%\ndescription: d\n\ttrouble:\n---\n", ReasonTabIndent},
		{"duplicate key", "---\nname: %NAME%\nname: %NAME%\ndescription: d\n---\n", ReasonDuplicateKey},
		// A nested arg value has no scalar form: refused by key (the module schema
		// is the authority on shape, and args_json is the way to express structure).
		{"nested arg value", "---\nname: %NAME%\ndescription: d\ntrouble:\n  steps:\n    - module: proc.connections\n      args:\n        a:\n          b: c\n---\n", ReasonFieldRule},
		{"mis-indented key", "---\nname: %NAME%\ndescription: d\ntrouble:\n  steps:\n    - module: proc.connections\n   title: oops\n---\n", ReasonIndentInconsistent},
		{"table header", "---\nname: %NAME%\ndescription: d\n[[trouble]]\n---\n", ReasonBadHeader},
		{"list of lists", "---\nname: %NAME%\ndescription: d\ntrouble:\n  steps:\n    - - module: proc.connections\n---\n", ReasonNestedList},
		{"unterminated frontmatter", "---\nname: %NAME%\ndescription: d\n", ReasonFrontmatter},
		{"scalar list item", "---\nname: %NAME%\ndescription: d\ntrouble:\n  steps:\n    - just-a-string\n---\n", ReasonListItemScalar},
		{"unknown trouble key", "---\nname: %NAME%\ndescription: d\ntrouble:\n  cmds: []\n---\n", ReasonUnknownKey},
		{"shell key", "---\nname: %NAME%\ndescription: d\ntrouble:\n  steps:\n    - module: proc.connections\n      shell: systemctl restart x\n---\n", ReasonUnknownKey},
		{"exec key", "---\nname: %NAME%\ndescription: d\ntrouble:\n  steps:\n    - module: proc.connections\n      exec: /bin/sh\n---\n", ReasonUnknownKey},
		{"name mismatch", "---\nname: something-else\ndescription: d\n---\n", ReasonNameMismatch},
		{"missing description", "---\nname: %NAME%\n---\n", ReasonMissingDesc},
		{"bad module", "---\nname: %NAME%\ndescription: d\ntrouble:\n  steps:\n    - module: shell\n---\n", ReasonBadModule},
		{"glob module", "---\nname: %NAME%\ndescription: d\ntrouble:\n  steps:\n    - module: 'proc.*'\n---\n", ReasonBadModule},
		{"bad version", "---\nname: %NAME%\ndescription: d\nversion: zero\n---\n", ReasonBadVersion},
		{"args_json invalid", "---\nname: %NAME%\ndescription: d\ntrouble:\n  steps:\n    - module: service.reload\n      args_json: 'not json'\n---\n", ReasonArgsJSON},
		{"nested non-trouble key", "---\nname: %NAME%\ndescription: d\nmetadata:\n  a: b\n---\n", ReasonFieldRule},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Chmod(root, 0o700); err != nil {
				t.Fatalf("chmod: %v", err)
			}
			writeSkillFile(t, root, "wedge", strings.ReplaceAll(tc.body, "%NAME%", "wedge"))
			lib, led := newLibrary(t, root, &fakeRunner{})

			all, err := lib.Scan(context.Background())
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}
			if len(all) != 0 {
				t.Fatalf("skills = %d, want 0: the file must be refused, never silently shortened (%+v)", len(all), all)
			}
			refusals := led.byPhase(PhaseLibraryRefused)
			if len(refusals) != 1 {
				t.Fatalf("library_refused records = %d, want 1", len(refusals))
			}
			if got := str(refusals[0].Payload, "reason"); got != tc.reason {
				t.Errorf("reason = %q, want %q", got, tc.reason)
			}
			if got := str(refusals[0].Payload, "file"); !strings.HasSuffix(got, filepath.Join("wedge", "SKILL.md")) {
				t.Errorf("refusal file = %q, want the offending SKILL.md", got)
			}
		})
	}
}

func TestLibrary_BodyIsNeverExecuted(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	writeSkillFile(t, root, "wedge", strings.ReplaceAll(goodSkill, "%NAME%", "wedge"))
	runner := &fakeRunner{}
	lib, led := newLibrary(t, root, runner)

	if _, err := lib.Plays(context.Background()); err != nil {
		t.Fatalf("Plays: %v", err)
	}
	if len(runner.checks) != 0 || len(runner.applies) != 0 {
		t.Fatalf("a scan executed something: checks=%v applies=%v", runner.checks, runner.applies)
	}
	if n := led.countPhase(PhaseStepExecuted); n != 0 {
		t.Errorf("step_executed records = %d, want 0 for a read-only scan", n)
	}
	// The only executable material a step has is its module and its args.
	skill, ok := lib.Get("wedge")
	if !ok {
		t.Fatal("Get(wedge) failed")
	}
	for _, st := range skill.Steps {
		if st.Module == "" {
			t.Errorf("step %d has no module", st.Index)
		}
	}
}

func TestLibrary_RunStepCheckAndApply(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	writeSkillFile(t, root, "wedge", strings.ReplaceAll(goodSkill, "%NAME%", "wedge"))
	runner := &fakeRunner{diff: types.Diff{Empty: false, Summary: "one change"}, result: types.Result{Changed: true}}
	lib, led := newLibrary(t, root, runner, "proc.connections", "service.reload")

	inc := types.Incident{ID: "inc_test", Sig: "journald:sha256v1:2ab4c6d8e0f1a3b5"}
	tc, err := lib.RunStep(context.Background(), inc, "wedge", 0, types.ModeCheck)
	if err != nil {
		t.Fatalf("RunStep(check): %v", err)
	}
	if tc.Mode != types.ModeCheck {
		t.Errorf("mode = %q, want check_mode", tc.Mode)
	}
	if len(runner.checks) != 1 || len(runner.applies) != 0 {
		t.Fatalf("check_mode called check=%d apply=%d, want 1/0", len(runner.checks), len(runner.applies))
	}
	recs := led.byPhase(PhaseStepExecuted)
	if len(recs) != 1 {
		t.Fatalf("step_executed records = %d, want 1", len(recs))
	}
	if got := recs[0].Payload["changed"]; got != true {
		t.Errorf("changed = %v, want true (a non-empty diff)", got)
	}
	if recs[0].Inc != "inc_test" {
		t.Errorf("record inc = %q, want the incident", recs[0].Inc)
	}

	tc, err = lib.RunStep(context.Background(), inc, "wedge", 1, types.ModeApply)
	if err != nil {
		t.Fatalf("RunStep(apply): %v", err)
	}
	if tc.Mode != types.ModeApply || len(runner.applies) != 1 {
		t.Fatalf("apply mode called apply=%d, want 1", len(runner.applies))
	}
	if runner.lastArgs["unit"] != "payment-worker.service" {
		t.Errorf("apply args = %+v, want the step's literals", runner.lastArgs)
	}
	if n := led.countPhase(PhaseStepExecuted); n != 2 {
		t.Errorf("step_executed records = %d, want 2", n)
	}
}

func TestLibrary_RunStepRefusalsNeverCallTheRunner(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	writeSkillFile(t, root, "wedge", strings.ReplaceAll(goodSkill, "%NAME%", "wedge"))
	runner := &fakeRunner{}
	// Only proc.connections is registered: the second step's module is not.
	lib, led := newLibrary(t, root, runner, "proc.connections")

	inc := types.Incident{ID: "inc_test", Sig: "journald:sha256v1:2ab4c6d8e0f1a3b5"}
	cases := []struct {
		name   string
		skill  string
		step   int
		mode   string
		reason string
	}{
		{"unknown skill", "nope", 0, types.ModeCheck, ReasonUnknownSkill},
		{"out of range step", "wedge", 9, types.ModeCheck, ReasonUnknownStep},
		{"negative step", "wedge", -1, types.ModeCheck, ReasonUnknownStep},
		{"bad mode", "wedge", 0, "stream", ReasonBadMode},
		{"unregistered module", "wedge", 1, types.ModeApply, ReasonMissingModule},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := lib.RunStep(context.Background(), inc, tc.skill, tc.step, tc.mode)
			if err == nil {
				t.Fatal("RunStep succeeded")
			}
			if got := CodeOf(err); got != types.CodeSkills013 {
				t.Errorf("code = %q, want TROUBLE-SKILLS-013 (err=%v)", got, err)
			}
			if got := ReasonOf(err); got != tc.reason {
				t.Errorf("reason = %q, want %q", got, tc.reason)
			}
			if len(runner.checks) != 0 || len(runner.applies) != 0 {
				t.Errorf("the runner was called for a refused step: %v/%v", runner.checks, runner.applies)
			}
			if n := led.countPhase(PhaseStepExecuted); n != 0 {
				t.Errorf("step_executed records = %d, want 0", n)
			}
		})
	}
}

func TestLibrary_RunnerErrorIsMirroredNotRewritten(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	writeSkillFile(t, root, "wedge", strings.ReplaceAll(goodSkill, "%NAME%", "wedge"))
	denied := newErr(types.CodeRegistry007, "do_not_touch", "payment-worker.service is protected")
	runner := &fakeRunner{err: denied}
	lib, led := newLibrary(t, root, runner, "proc.connections")

	_, err := lib.RunStep(context.Background(), types.Incident{ID: "inc_test"}, "wedge", 0, types.ModeApply)
	if err == nil {
		t.Fatal("RunStep reported success for a refused call")
	}
	if got := CodeOf(err); got != types.CodeRegistry007 {
		t.Errorf("code = %q, want the runner's own TROUBLE-REGISTRY-007 mirrored", got)
	}
	recs := led.byPhase(PhaseStepExecuted)
	if len(recs) != 1 {
		t.Fatalf("step_executed records = %d, want the failed step recorded", len(recs))
	}
	if got := str(recs[0].Payload, "error_code"); got != string(types.CodeRegistry007) {
		t.Errorf("recorded error_code = %q, want the mirrored code", got)
	}
}

func TestLibrary_OffReadsNothing(t *testing.T) {
	// The shipped default: local_enabled = false, no dir. Nothing is read and no
	// step can run.
	lib, led := newLibrary(t, "", &fakeRunner{})
	all, err := lib.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("skills = %d, want 0", len(all))
	}
	plays, err := lib.Plays(context.Background())
	if err != nil {
		t.Fatalf("Plays: %v", err)
	}
	if len(plays) != 0 {
		t.Errorf("plays = %d, want 0", len(plays))
	}
	if _, err := lib.RunStep(context.Background(), types.Incident{ID: "inc_x"}, "wedge", 0, types.ModeCheck); ReasonOf(err) != ReasonLibraryDisabled {
		t.Errorf("reason = %q, want %q", ReasonOf(err), ReasonLibraryDisabled)
	}
	if n := len(led.records()); n != 0 {
		t.Errorf("ledger records = %d, want 0 with the library off", n)
	}
}

func TestLibrary_ConstructionRefusals(t *testing.T) {
	clk := newFakeClock()
	led := &fakeLedger{}
	led.now = clk.Now
	deps := testDeps(t, led, clk, t.TempDir())

	// enabled with no dir
	cfg := DefaultConfig()
	cfg.LocalEnabled = true
	if _, err := NewLibrary(cfg, deps, &fakeRunner{}); err == nil {
		t.Error("NewLibrary accepted an enabled library with no local_dir")
	} else if ReasonOf(err) != ReasonConfig {
		t.Errorf("reason = %q, want %q", ReasonOf(err), ReasonConfig)
	}

	// missing dir
	cfg.LocalDir = filepath.Join(t.TempDir(), "nope")
	if _, err := NewLibrary(cfg, deps, &fakeRunner{}); err == nil {
		t.Error("NewLibrary accepted a missing local_dir")
	} else if ReasonOf(err) != ReasonDirMissing {
		t.Errorf("reason = %q, want %q", ReasonOf(err), ReasonDirMissing)
	}

	// a world-writable directory is a capability handed to another user
	open := t.TempDir()
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	cfg.LocalDir = open
	if _, err := NewLibrary(cfg, deps, &fakeRunner{}); err == nil {
		t.Error("NewLibrary accepted a world-writable local_dir")
	} else if ReasonOf(err) != ReasonDirWritable {
		t.Errorf("reason = %q, want %q", ReasonOf(err), ReasonDirWritable)
	}

	// a `..` path is refused before any stat
	cfg.LocalDir = "/tmp/../tmp"
	if _, err := NewLibrary(cfg, deps, &fakeRunner{}); err == nil {
		t.Error("NewLibrary accepted a `..` path")
	}
}

func TestLibrary_CapsAreEnforced(t *testing.T) {
	root := writeLibrary(t)
	clk := newFakeClock()
	led := &fakeLedger{}
	led.now = clk.Now

	cfg := libraryCfg(root)
	cfg.LocalMaxSkills = 1
	lib, err := NewLibrary(cfg, testDeps(t, led, clk, t.TempDir()), &fakeRunner{})
	if err != nil {
		t.Fatalf("NewLibrary: %v", err)
	}
	if _, err := lib.Scan(context.Background()); err == nil {
		t.Error("Scan read a library above local_max_skills")
	} else if ReasonOf(err) != ReasonTooManySkills {
		t.Errorf("reason = %q, want %q", ReasonOf(err), ReasonTooManySkills)
	}

	cfg = libraryCfg(root)
	cfg.LocalMaxSteps = 1
	lib, err = NewLibrary(cfg, testDeps(t, led, clk, t.TempDir()), &fakeRunner{})
	if err != nil {
		t.Fatalf("NewLibrary: %v", err)
	}
	all, err := lib.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("skills = %d, want 0: both files carry 2 steps, above the cap of 1", len(all))
	}
	if n := led.countPhase(PhaseLibraryRefused); n == 0 {
		t.Error("no refusal record for a step-cap refusal")
	}
}

func TestLibrary_RunnerErrorsKeepTheirClass(t *testing.T) {
	// A missing runner is a refusal, not a panic and not a silent no-op.
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	writeSkillFile(t, root, "wedge", strings.ReplaceAll(goodSkill, "%NAME%", "wedge"))
	lib, _ := newLibrary(t, root, nil)
	_, err := lib.RunStep(context.Background(), types.Incident{ID: "inc_x"}, "wedge", 0, types.ModeCheck)
	if err == nil {
		t.Fatal("RunStep succeeded with no runner")
	}
	if ReasonOf(err) != ReasonNoRunner {
		t.Errorf("reason = %q, want %q", ReasonOf(err), ReasonNoRunner)
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("err = %T, want a classified *skills.Error", err)
	}
}
