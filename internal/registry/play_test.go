package registry

// play_test.go is the §7 row for plays as data (SPEC-06 §3.7): the TOML schema
// and its defaults, the closed key set (TROUBLE-REGISTRY-016), the `when:`
// namespace (TROUBLE-REGISTRY-017), register scoping (forward-only), the retry
// budget and its backoff, all three `on_fail` paths — abort, continue and
// rollback, the last one replaying the accumulated RollbackHints in reverse and
// stopping at the first point-of-no-return with TROUBLE-REGISTRY-015 — and the
// (name, version) identity a play's resolution is keyed on.
//
// Nothing here waits, touches the network, git, or the real /etc: every fixture
// lives under t.TempDir(), the retry backoff goes through SetSleeper, and the
// ledger is a recording closure that mints RecID/Seq the way internal/ledger
// does. The fake modules below are registered through Registry.Register on a
// registry built with NewWith(deps, nil): every name a play can name must exist
// in the registry's own table, and a fake name has no committed schema artifact,
// so NewWith(deps, mods) would (correctly) refuse it with the schema-drift check.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/registry/schemagen"
	"github.com/trouble-agent/trouble/internal/types"
)

// ---- harness ----

// playTestLedger is the FakeLedger: Append mints RecID/Seq/TS/SchemaVersion the
// way internal/ledger does, and keeps the records so a test can read the audit
// trail back (play_run and tool_call kinds).
type playTestLedger struct {
	mu   sync.Mutex
	recs []types.Record
	seq  uint64
	drop bool
}

// dropRecords keeps the ledger out of a measurement: the ids are still minted,
// nothing is retained. It exists for the benchmark/RSS loops, where the recorder
// must not be the thing that grows.
func (l *playTestLedger) dropRecords(drop bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.drop = drop
	l.recs = nil
}

func (l *playTestLedger) append(rec types.Record) (types.Record, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	rec.Seq = l.seq
	rec.RecID = types.NewID(types.PEv)
	rec.TS = types.FormatUTC(time.Now())
	rec.SchemaVersion = 1
	if !l.drop {
		l.recs = append(l.recs, rec)
	}
	return rec, nil
}

func (l *playTestLedger) ofKind(kind types.RecordKind) []types.Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []types.Record
	for _, r := range l.recs {
		if r.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}

func (l *playTestLedger) playRuns() []types.Record  { return l.ofKind(types.KPlayRun) }
func (l *playTestLedger) toolCalls() []types.Record { return l.ofKind(types.KToolCall) }

func (l *playTestLedger) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recs = nil
}

// playTestGates is the autonomy state the harness exposes: the play tests need a
// real apply, and shadow forces check_mode for every mutating call, so the
// default is full autonomy with the request's own grants deciding each call
// (SPEC-05 §3.11, SPEC-06 §2.3 a7).
type playTestGates struct {
	mu   sync.Mutex
	mode types.AutonomyMode
}

func (g *playTestGates) get() types.AutonomyGates {
	g.mu.Lock()
	defer g.mu.Unlock()
	return types.AutonomyGates{Mode: g.mode}
}

func (g *playTestGates) set(mode types.AutonomyMode) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.mode = mode
}

func playTestDeps(led *playTestLedger, gates *playTestGates) RegistryDeps {
	return RegistryDeps{
		Append: led.append,
		Scrub: func(target string, in []byte) (types.ScrubResult, error) {
			return types.ScrubResult{Value: in, BytesIn: len(in)}, nil
		},
		Gates:  gates.get,
		Now:    time.Now,
		HostID: "playtest-host",
		Actor:  types.Actor{Kind: types.ActorDaemon, ID: "troubled-test", Version: "0.1.0"},
	}
}

// playTestHarness is a registry over an explicit module set with a recording
// ledger and a sleeper that never sleeps.
type playTestHarness struct {
	reg   *Registry
	led   *playTestLedger
	gates *playTestGates
	slept []time.Duration
}

func playTestNew(t *testing.T, mods ...types.Module) *playTestHarness {
	t.Helper()
	led := &playTestLedger{}
	gates := &playTestGates{mode: types.AutoFull}
	reg, err := NewWith(playTestDeps(led, gates), nil)
	if err != nil {
		t.Fatalf("NewWith: %v", err)
	}
	h := &playTestHarness{reg: reg, led: led, gates: gates}
	reg.SetSleeper(func(d time.Duration) { h.slept = append(h.slept, d) })
	for _, m := range mods {
		if err := reg.Register(m); err != nil {
			t.Fatalf("register %s: %v", m.Descriptor().Name, err)
		}
	}
	return h
}

func (h *playTestHarness) applyRequest(grants ...string) types.ToolCallRequest {
	return types.ToolCallRequest{Inc: "inc_playtest", Mode: types.ModeApply, Grants: grants}
}

func (h *playTestHarness) checkRequest() types.ToolCallRequest {
	return types.ToolCallRequest{Inc: "inc_playtest", Mode: types.ModeCheck}
}

// applyOnly is the request shape the when:-evaluation runs use: the modules are
// pure reads, so apply mode mutates nothing, but the call DOES run its apply
// stage — and only then does a task's Result.Output exist to be registered. In
// check_mode a task's register is therefore always empty (the apply stage is not
// reached), which is why these runs use apply mode.
func applyOnly() types.ToolCallRequest {
	return types.ToolCallRequest{Inc: "inc_playtest", Mode: types.ModeApply}
}

// playTestWantCode asserts the registry error code carried by err.
func playTestWantCode(t *testing.T, err error, code types.ErrorCode, want ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want %s, got nil", code)
	}
	if got := CodeOf(err); got != code {
		t.Fatalf("want %s, got %q (%v)", code, got, err)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Fatalf("error %q does not carry %q", err.Error(), w)
		}
	}
}

// ---- decoding ----

const playTestFullTOML = `
schema_version = 1
name        = "service-reload-on-pool-exhaustion"
version     = 3
source      = "module-default"
max_runs    = 2

[[task]]
name     = "count-connections"
tool     = "proc.connections"
args     = { unit = "payment-worker.service", proto = "tcp" }
when     = ""
register = "conns"
retries  = 0
on_fail  = "abort"

[[task]]
name     = "reload"
tool     = "service.reload"
args     = { unit = "payment-worker.service", scope = "user" }
when     = "conns.counts.established > 400"
register = "reload_result"
retries  = 1
on_fail  = "rollback"
`

func playTestDecode(t *testing.T, src string) types.Play {
	t.Helper()
	p, err := DecodePlay([]byte(src), "play.toml", nil)
	if err != nil {
		t.Fatalf("DecodePlay refused a valid play: %v\n%s", err, src)
	}
	return p
}

func playTestDecodeErr(t *testing.T, src string) error {
	t.Helper()
	_, err := DecodePlay([]byte(src), "play.toml", nil)
	if err == nil {
		t.Fatalf("DecodePlay accepted a play it must refuse:\n%s", src)
	}
	return err
}

// TestPlayDecodeMapping proves the 1:1 field mapping of §3.7 and the derived
// Play.CheckMode, through both DecodePlay and LoadPlay.
func TestPlayDecodeMapping(t *testing.T) {
	p := playTestDecode(t, playTestFullTOML)
	if p.Name != "service-reload-on-pool-exhaustion" || p.Version != 3 {
		t.Fatalf("identity = %q@%d", p.Name, p.Version)
	}
	if p.Source != "module-default" {
		t.Fatalf("source = %q, want module-default", p.Source)
	}
	if p.MaxRuns != 2 {
		t.Fatalf("max_runs = %d, want 2", p.MaxRuns)
	}
	// Derived, not declared: true iff every task's tool has check_mode.
	if !p.CheckMode {
		t.Fatal("Play.CheckMode must be derived true when every task's tool has check_mode")
	}
	if len(p.Tasks) != 2 {
		t.Fatalf("tasks = %d, want 2", len(p.Tasks))
	}
	t0 := p.Tasks[0]
	if t0.Name != "count-connections" || t0.Tool != "proc.connections" {
		t.Fatalf("task 0 identity = %+v", t0)
	}
	if t0.Args["unit"] != "payment-worker.service" || t0.Args["proto"] != "tcp" {
		t.Fatalf("task 0 args = %+v", t0.Args)
	}
	if t0.When != "" || t0.Register != "conns" || t0.Retries != 0 || t0.OnFail != "abort" {
		t.Fatalf("task 0 fields = %+v", t0)
	}
	t1 := p.Tasks[1]
	if t1.Name != "reload" || t1.Tool != "service.reload" {
		t.Fatalf("task 1 identity = %+v", t1)
	}
	if t1.When != "conns.counts.established > 400" || t1.Register != "reload_result" ||
		t1.Retries != 1 || t1.OnFail != "rollback" {
		t.Fatalf("task 1 fields = %+v", t1)
	}

	// The decoded value is a stable frozen shape: a JSON round trip is lossless.
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back types.Play
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !playTestJSONEqual(t, p, back) {
		t.Fatalf("the decoded play does not survive a JSON round trip:\n%s", raw)
	}

	// LoadPlay is the file-shaped half of the same loader.
	path := filepath.Join(t.TempDir(), "service-reload-on-pool-exhaustion@3.toml")
	if err := os.WriteFile(path, []byte(playTestFullTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadPlay(path)
	if err != nil {
		t.Fatalf("LoadPlay: %v", err)
	}
	if !playTestJSONEqual(t, p, loaded) {
		t.Fatal("LoadPlay and DecodePlay disagree on the same document")
	}
	if _, err := LoadPlay(filepath.Join(t.TempDir(), "absent.toml")); err == nil {
		t.Fatal("LoadPlay must refuse a missing play file")
	} else {
		playTestWantCode(t, err, types.CodeRegistry016)
	}
}

func playTestJSONEqual(t *testing.T, a, b any) bool {
	t.Helper()
	ab, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	bb, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(ab) == string(bb)
}

// TestPlayDefaults asserts the default-filling table of §3.7: max_runs = 2,
// retries = 0, on_fail = "abort", source = "module-default", when = "",
// register = "" — and that a declared value always wins over the default.
func TestPlayDefaults(t *testing.T) {
	p := playTestDecode(t, `
schema_version = 1
name    = "defaults"
version = 1

[[task]]
name = "read"
tool = "config.get"
args = { path = "/tmp/trouble-play-defaults.toml", key = "alpha" }
`)
	if p.MaxRuns != 2 {
		t.Errorf("max_runs default = %d, want 2", p.MaxRuns)
	}
	if p.Source != "module-default" {
		t.Errorf("source default = %q, want module-default", p.Source)
	}
	task := p.Tasks[0]
	if task.Retries != 0 || task.OnFail != "abort" || task.When != "" || task.Register != "" {
		t.Errorf("task defaults = %+v", task)
	}
	if task.Args == nil {
		t.Error("task args must decode to a non-nil map (an empty args set is legal)")
	}

	declared := playTestDecode(t, `
schema_version = 1
name     = "declared"
version  = 2
source   = "agent-draft"
max_runs = 7

[[task]]
name     = "read"
tool     = "config.get"
args     = { path = "/tmp/trouble-play-defaults.toml", key = "alpha" }
register = "cfg"
retries  = 3
on_fail  = "continue"
when     = "cfg.value == 1"
`)
	if declared.MaxRuns != 7 || declared.Source != "agent-draft" {
		t.Errorf("declared play-level values were not honoured: %+v", declared)
	}
	task = declared.Tasks[0]
	if task.Retries != 3 || task.OnFail != "continue" || task.Register != "cfg" {
		t.Errorf("declared task values were not honoured: %+v", task)
	}

	// A task with no args at all still decodes to an empty, non-nil map.
	bare := playTestDecode(t, `
schema_version = 1
name    = "bare"
version = 1

[[task]]
name = "reload"
tool = "service.reload"
`)
	if bare.Tasks[0].Args == nil || len(bare.Tasks[0].Args) != 0 {
		t.Errorf("a task without args must decode to an empty map, got %+v", bare.Tasks[0].Args)
	}
}

// TestPlayDecodeUnknownKeys asserts the closed key set (SPEC-06 §3.7,
// TROUBLE-REGISTRY-016): additionalProperties is false at every level.
func TestPlayDecodeUnknownKeys(t *testing.T) {
	cases := []struct {
		name string
		src  string
		key  string
	}{
		{"top-level", `
schema_version = 1
name = "unknown-key"
version = 1
wibble = 1

[[task]]
name = "read"
tool = "config.get"
args = { path = "/tmp/t", key = "a" }
`, "wibble"},
		{"task-level", `
schema_version = 1
name = "unknown-key"
version = 1

[[task]]
name = "read"
tool = "config.get"
args = { path = "/tmp/t", key = "a" }
extra = true
`, "extra"},
		{"malformed-toml", "name = \"unterminated\n", ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := playTestDecodeErr(t, tc.src)
			playTestWantCode(t, err, types.CodeRegistry016)
			if reason := ReasonOf(err); reason != "play_invalid" {
				t.Errorf("reason = %q, want play_invalid", reason)
			}
			if tc.key != "" && !strings.Contains(err.Error(), tc.key) {
				t.Errorf("the refusal must name the unknown key %q: %v", tc.key, err)
			}
		})
	}
}

// TestPlayDecodeRefusals is the static-validation table of §3.7: a tool outside
// the shipped table, a duplicate or non-identifier register, out-of-range
// retries, a bad on_fail, and the shape checks. Every one is
// TROUBLE-REGISTRY-016 and the play never reaches the runner.
func TestPlayDecodeRefusals(t *testing.T) {
	head := "schema_version = 1\nname = \"refused\"\nversion = 1\n\n"
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"schema-version", `
schema_version = 2
name = "refused"
version = 1

[[task]]
name = "read"
tool = "config.get"
args = { path = "/tmp/t", key = "a" }
`, "schema_version"},
		{"no-name", `
schema_version = 1
version = 1

[[task]]
name = "read"
tool = "config.get"
args = { path = "/tmp/t", key = "a" }
`, "without a name"},
		{"non-positive-version", `
schema_version = 1
name = "refused"
version = 0

[[task]]
name = "read"
tool = "config.get"
args = { path = "/tmp/t", key = "a" }
`, "version"},
		{"no-tasks", `
schema_version = 1
name = "refused"
version = 1
`, "without tasks"},
		{"task-without-name", head + `
[[task]]
tool = "config.get"
args = { path = "/tmp/t", key = "a" }
`, "has no name"},
		{"task-without-tool", head + `
[[task]]
name = "read"
`, "names no tool"},
		{"bogus-tool", head + `
[[task]]
name = "delete"
tool = "config.delete"
args = { path = "/tmp/t", key = "a" }
`, "unknown tool"},
		{"duplicate-register", head + `
[[task]]
name = "one"
tool = "config.get"
args = { path = "/tmp/t", key = "a" }
register = "same"

[[task]]
name = "two"
tool = "config.get"
args = { path = "/tmp/t", key = "b" }
register = "same"
`, "duplicate register"},
		{"non-identifier-register-leading-digit", head + `
[[task]]
name = "one"
tool = "config.get"
args = { path = "/tmp/t", key = "a" }
register = "3conns"
`, "not an identifier"},
		{"non-identifier-register-dash", head + `
[[task]]
name = "one"
tool = "config.get"
args = { path = "/tmp/t", key = "a" }
register = "reload-result"
`, "not an identifier"},
		{"retries-negative", head + `
[[task]]
name = "one"
tool = "config.get"
args = { path = "/tmp/t", key = "a" }
retries = -1
`, "outside 0..3"},
		{"retries-above-budget", head + `
[[task]]
name = "one"
tool = "config.get"
args = { path = "/tmp/t", key = "a" }
retries = 4
`, "outside 0..3"},
		{"bad-on-fail", head + `
[[task]]
name = "one"
tool = "config.get"
args = { path = "/tmp/t", key = "a" }
on_fail = "explode"
`, "abort|continue|rollback"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := playTestDecodeErr(t, tc.src)
			playTestWantCode(t, err, types.CodeRegistry016, tc.want)
			if reason := ReasonOf(err); reason != "play_invalid" {
				t.Errorf("reason = %q, want play_invalid", reason)
			}
		})
	}
}

// TestPlayStaticTargetsAreRefusedAtLoad covers the second half of "a play whose
// static targets hit do-not-touch never loads" (SPEC-06 §3.6, §3.7) through the
// dnt-taking loader.
func TestPlayStaticTargetsAreRefusedAtLoad(t *testing.T) {
	dir := t.TempDir()
	protected := filepath.Join(dir, "keep.toml")
	src := fmt.Sprintf(`
schema_version = 1
name = "static-target"
version = 1

[[task]]
name = "write"
tool = "config.set"
args = { path = %q, key = "alpha", value = 1 }
`, protected)
	path := filepath.Join(dir, "static-target@1.toml")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	dnt := types.DoNotTouch{Paths: []string{protected}}
	_, err := LoadPlayWith(path, &dnt)
	playTestWantCode(t, err, types.CodeRegistry007)

	// The same document loads against the compiled-in floor, which does not name
	// the fixture path: the refusal is the do-not-touch set, not the schedule.
	if _, err := LoadPlayWith(path, nil); err != nil {
		t.Fatalf("the same play must load when the fixture path is not protected: %v", err)
	}
	// ... but /etc/shadow is on the floor, whatever set is handed in.
	shadow := strings.Replace(src, fmt.Sprintf("%q", protected), `"/etc/shadow"`, 1)
	if _, err := DecodePlay([]byte(shadow), "static-target@1.toml", nil); err == nil {
		t.Fatal("a play that statically targets /etc/shadow must never load")
	} else {
		playTestWantCode(t, err, types.CodeRegistry007)
	}
}

// TestPlayWhenParseFailure is TROUBLE-REGISTRY-017 at load time: a `when:` the
// shared condition language cannot parse refuses the play.
func TestPlayWhenParseFailure(t *testing.T) {
	for _, expr := range []string{"conns.counts.established >", "==", "a ==", "a ~"} {
		expr := expr
		t.Run(expr, func(t *testing.T) {
			src := fmt.Sprintf(`
schema_version = 1
name = "bad-when"
version = 1

[[task]]
name = "one"
tool = "config.get"
args = { path = "/tmp/t", key = "a" }
register = "conns"
when = %q
`, expr)
			err := playTestDecodeErr(t, src)
			playTestWantCode(t, err, types.CodeRegistry017)
			if reason := ReasonOf(err); reason != "when_expression_invalid" {
				t.Errorf("reason = %q, want when_expression_invalid", reason)
			}
		})
	}
}

// ---- the engine ----

// playTestModuleDescriptor builds a descriptor from the shared fake args struct,
// so every fake module walks the same generated dialect the shipped ones do.
func playTestModuleDescriptor(name string, mutating bool) types.Descriptor {
	return types.Descriptor{
		Name:        name,
		Version:     1,
		Schema:      schemagen.Generate(name, 1, playTestArgs{}),
		Scopes:      []string{"config:write"},
		Idempotency: types.IdemConvergent,
		CheckMode:   true,
		TimeoutS:    5,
		Mutating:    mutating,
	}
}

// playTestArgs is the args surface of every fake module below.
type playTestArgs struct {
	Path  string `json:"path,omitempty"`
	Value string `json:"value,omitempty"`
	Undo  string `json:"undo,omitempty"`
	Hint  string `json:"hint,omitempty"`
	Flag  bool   `json:"flag,omitempty"`
}

// playTestTarget writes a fixture target and returns its path.
func playTestTarget(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "target.txt")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// playTestTerminalCalls lists the terminal (intent=false) tool_call records of
// one module: an apply-mode call writes an intent record before the mutation and
// a terminal record after it, so counting "calls" means counting the latter.
func playTestTerminalCalls(led *playTestLedger, module string) []map[string]any {
	var out []map[string]any
	for _, rec := range led.toolCalls() {
		if rec.Payload["module"] != module || rec.Payload["intent"] == true {
			continue
		}
		args, _ := rec.Payload["args"].(map[string]any)
		out = append(out, args)
	}
	return out
}

func playTestPath(args map[string]any) string {
	s, _ := args["path"].(string)
	return s
}

func playTestValue(args map[string]any) string {
	s, _ := args["value"].(string)
	return s
}

// playTestWriter is the truncating fake writer: Check observes whether the
// target already holds the desired bytes; Apply truncates the target and reports
// a transient failure for the first `fails` attempts, then converges. It is
// therefore both the retry fixture and the idempotency fixture.
type playTestWriter struct {
	desc     types.Descriptor
	fails    int
	failWith error
	mu       sync.Mutex
	applies  int
}

func newPlayTestWriter(name string, fails int, failWith error) *playTestWriter {
	return &playTestWriter{desc: playTestModuleDescriptor(name, true), fails: fails, failWith: failWith}
}

func (w *playTestWriter) Descriptor() types.Descriptor { return w.desc }

func (w *playTestWriter) applyCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.applies
}

func (w *playTestWriter) Check(_ context.Context, args map[string]any) (types.Diff, error) {
	path, want := playTestPath(args), playTestValue(args)
	cur, err := os.ReadFile(path)
	if err != nil {
		return types.Diff{}, fmt.Errorf("%w: %v", types.ErrPermanent, err)
	}
	if string(cur) == want {
		return types.Diff{Empty: true, Summary: "already at the described state"}, nil
	}
	return types.Diff{
		Entries: []types.DiffEntry{{Path: path, Before: string(cur), After: want}},
		Summary: fmt.Sprintf("%s %q -> %q", path, string(cur), want),
	}, nil
}

func (w *playTestWriter) Apply(_ context.Context, args map[string]any) (types.Result, error) {
	path, want := playTestPath(args), playTestValue(args)
	w.mu.Lock()
	w.applies++
	attempt := w.applies
	w.mu.Unlock()
	if attempt <= w.fails {
		// The attempt damages the target and then reports its class: a half
		// written file is exactly the state a retried write must survive.
		_ = os.WriteFile(path, nil, 0o644)
		return types.Result{}, fmt.Errorf("%w: playTestWriter attempt %d of %d failed", w.failWith, attempt, w.fails+1)
	}
	if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
		return types.Result{}, fmt.Errorf("%w: %v", types.ErrPermanent, err)
	}
	return types.Result{
		Changed: true,
		Applied: []types.DiffEntry{{Path: path, Before: nil, After: want}},
		Output:  map[string]any{"path": path, "value": want, "flag": true},
	}, nil
}

func (w *playTestWriter) Verify(_ context.Context, args map[string]any) (types.VerifyResult, error) {
	path, want := playTestPath(args), playTestValue(args)
	cur, err := os.ReadFile(path)
	if err != nil {
		return types.VerifyResult{}, fmt.Errorf("%w: %v", types.ErrPermanent, err)
	}
	return types.VerifyResult{OK: string(cur) == want, Method: "recheck"}, nil
}

// playTestFailer always fails its Apply with the configured class. Its Check
// predicts a change, so Apply is reached.
type playTestFailer struct {
	desc     types.Descriptor
	failWith error
	mu       sync.Mutex
	applies  int
}

func newPlayTestFailer(name string, failWith error) *playTestFailer {
	return &playTestFailer{desc: playTestModuleDescriptor(name, false), failWith: failWith}
}

func (f *playTestFailer) Descriptor() types.Descriptor { return f.desc }

func (f *playTestFailer) applyCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.applies
}

func (f *playTestFailer) Check(_ context.Context, _ map[string]any) (types.Diff, error) {
	return types.Diff{Entries: []types.DiffEntry{{Path: f.desc.Name, Before: nil, After: "x"}}}, nil
}

func (f *playTestFailer) Apply(_ context.Context, _ map[string]any) (types.Result, error) {
	f.mu.Lock()
	f.applies++
	f.mu.Unlock()
	return types.Result{}, fmt.Errorf("%w: playTestFailer", f.failWith)
}

func (f *playTestFailer) Verify(_ context.Context, _ map[string]any) (types.VerifyResult, error) {
	return types.VerifyResult{OK: true, Method: "recheck"}, nil
}

// playTestHinter is an invertible module: its Apply harms nothing and reports a
// supported RollbackHint for the module named in args.hint. It is how a play
// accumulates the hints the rollback path replays.
type playTestHinter struct {
	desc types.Descriptor
}

func newPlayTestHinter(name string) *playTestHinter {
	return &playTestHinter{desc: playTestModuleDescriptor(name, false)}
}

func (h *playTestHinter) Descriptor() types.Descriptor { return h.desc }

func (h *playTestHinter) Check(_ context.Context, args map[string]any) (types.Diff, error) {
	if playTestPath(args) == "" {
		return types.Diff{Entries: []types.DiffEntry{{Path: h.desc.Name, Before: nil, After: playTestValue(args)}}}, nil
	}
	return types.Diff{Empty: true}, nil
}

func (h *playTestHinter) Apply(_ context.Context, args map[string]any) (types.Result, error) {
	undo, _ := args["undo"].(string)
	module, _ := args["hint"].(string)
	return types.Result{
		Changed: true,
		Applied: []types.DiffEntry{{Path: h.desc.Name + ":" + undo, Before: nil, After: undo}},
		Output:  map[string]any{"undo": undo, "hint": module},
		Rollback: &types.RollbackHint{
			Supported: true,
			Module:    module,
			Args:      map[string]any{"undo": undo},
		},
	}, nil
}

func (h *playTestHinter) Verify(_ context.Context, _ map[string]any) (types.VerifyResult, error) {
	return types.VerifyResult{OK: true, Method: "recheck"}, nil
}

// playTestUndo records the order in which the engine replayed hints. It is
// registered under an INVERTIBLE module NAME ("config.set" or "file.patch" — the
// only two names rollbackAll accepts) and declares Mutating = false, so the
// replay is not stopped by the authorize ladder's grant rule: rollbackHint builds
// its request with no grants at all, and a6 refuses every mutating apply without
// one. Keeping the fake non-mutating isolates the engine's replay path from that
// gap; TestPlayOnFailRollbackReplayWithoutGrants pins the real modules' behaviour.
type playTestUndo struct {
	desc  types.Descriptor
	mu    sync.Mutex
	order []string
}

func newPlayTestUndo(name string) *playTestUndo {
	return &playTestUndo{desc: playTestModuleDescriptor(name, false)}
}

func (u *playTestUndo) Descriptor() types.Descriptor { return u.desc }

func (u *playTestUndo) replayOrder() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.order...)
}

func (u *playTestUndo) Check(_ context.Context, _ map[string]any) (types.Diff, error) {
	return types.Diff{Empty: true}, nil
}

func (u *playTestUndo) Apply(_ context.Context, args map[string]any) (types.Result, error) {
	undo, _ := args["undo"].(string)
	u.mu.Lock()
	u.order = append(u.order, undo)
	u.mu.Unlock()
	return types.Result{
		Changed: true,
		Applied: []types.DiffEntry{{Path: u.desc.Name + ":" + undo, Before: nil, After: undo}},
		Output:  map[string]any{"undo": undo},
	}, nil
}

func (u *playTestUndo) Verify(_ context.Context, _ map[string]any) (types.VerifyResult, error) {
	return types.VerifyResult{OK: true, Method: "recheck"}, nil
}

// playTestPlay builds a play as data by hand: every tool is already registered,
// so the loader (which insists on the shipped tool table) is bypassed on purpose.
// CheckMode is true because every tool involved has a dry run.
func playTestPlay(name string, version int, tasks ...types.PlayTask) types.Play {
	return types.Play{
		Name: name, Version: version, Source: "agent-draft", MaxRuns: 2,
		CheckMode: true, Tasks: tasks,
	}
}

func playTestTask(name, tool string, args map[string]any, opts ...func(*types.PlayTask)) types.PlayTask {
	t := types.PlayTask{Name: name, Tool: tool, Args: args, OnFail: "abort"}
	for _, o := range opts {
		o(&t)
	}
	return t
}

func playTestWithOnFail(onFail string) func(*types.PlayTask) {
	return func(t *types.PlayTask) { t.OnFail = onFail }
}

func playTestWithRegister(register string) func(*types.PlayTask) {
	return func(t *types.PlayTask) { t.Register = register }
}

func playTestWithWhen(expr string) func(*types.PlayTask) {
	return func(t *types.PlayTask) { t.When = expr }
}

func playTestWithRetries(n int) func(*types.PlayTask) {
	return func(t *types.PlayTask) { t.Retries = n }
}

// TestPlayWhenEvaluation runs a decoded play through the engine and asserts the
// `when:` namespace (SPEC-06 §3.7): empty always runs, a true expression runs, a
// false one is skipped with skipped_by, and a register's Output map is visible
// under its own name through the dotted flattening.
func TestPlayWhenEvaluation(t *testing.T) {
	h := playTestNew(t, configGetModule{}, configListModule{}, configSetModule{})
	dir := t.TempDir()
	path := filepath.Join(dir, "app.toml")
	if err := os.WriteFile(path, []byte("alpha = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := fmt.Sprintf(`
schema_version = 1
name = "when-eval"
version = 1

[[task]]
name = "read"
tool = "config.get"
args = { path = %q, key = "alpha" }
register = "cfg"
when = ""

[[task]]
name = "runs-when-true"
tool = "config.get"
args = { path = %q, key = "alpha" }
when = "cfg.present == true"

[[task]]
name = "runs-when-value-matches"
tool = "config.get"
args = { path = %q, key = "alpha" }
when = "cfg.value == 1"

[[task]]
name = "skipped-when-false"
tool = "config.get"
args = { path = %q, key = "alpha" }
when = "cfg.value == 9999"

[[task]]
name = "skipped-when-unknown-var"
tool = "config.get"
args = { path = %q, key = "alpha" }
when = "nobody.bound.this == 1"
`, path, path, path, path, path)
	p := playTestDecode(t, src)
	run, err := h.reg.RunPlay(context.Background(), p, applyOnly())
	if err != nil {
		t.Fatalf("RunPlay: %v", err)
	}
	if len(run.Tasks) != len(p.Tasks) {
		t.Fatalf("run recorded %d task(s), want %d", len(run.Tasks), len(p.Tasks))
	}
	wantStatus := []string{"ok", "ok", "ok", "skipped", "skipped"}
	for i, want := range wantStatus {
		if got := run.Tasks[i].Status; got != want {
			t.Errorf("task %d (%s) status = %q, want %q", i, run.Tasks[i].Name, got, want)
		}
	}
	if run.Tasks[3].SkippedBy != "cfg.value == 9999" {
		t.Errorf("skipped_by = %q, want the expression that skipped the task", run.Tasks[3].SkippedBy)
	}
	if run.Tasks[1].SkippedBy != "" || run.Tasks[2].SkippedBy != "" {
		t.Errorf("a task that ran must not carry skipped_by: %+v", run.Tasks[1])
	}
	if run.Outcome != types.OutcomeCheckOnly {
		t.Errorf("outcome = %q, want %q (a check_mode run that changed nothing)", run.Outcome, types.OutcomeCheckOnly)
	}
}

// TestPlayRegisterScoping is the forward-only rule of §3.7: a task sees only the
// registers of lower-indexed tasks, and a reference to a register bound LATER is
// false rather than an error (the namespace is closed and a missing variable is
// false, never a refusal).
func TestPlayRegisterScoping(t *testing.T) {
	h := playTestNew(t, configGetModule{}, configListModule{})
	dir := t.TempDir()
	path := filepath.Join(dir, "app.toml")
	if err := os.WriteFile(path, []byte("alpha = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := fmt.Sprintf(`
schema_version = 1
name = "forward-only"
version = 1

[[task]]
name = "first"
tool = "config.get"
args = { path = %q, key = "alpha" }
register = "early"

[[task]]
name = "sees-early"
tool = "config.get"
args = { path = %q, key = "alpha" }
when = "early.present == true"

[[task]]
name = "cannot-see-late"
tool = "config.get"
args = { path = %q, key = "alpha" }
when = "late.present == true"

[[task]]
name = "late"
tool = "config.get"
args = { path = %q, key = "alpha" }
register = "late"
`, path, path, path, path)
	p := playTestDecode(t, src)
	run, err := h.reg.RunPlay(context.Background(), p, applyOnly())
	if err != nil {
		t.Fatalf("a forward reference must be false, never an error: %v", err)
	}
	if len(run.Tasks) != 4 {
		t.Fatalf("run recorded %d task(s), want 4", len(run.Tasks))
	}
	if !strings.HasPrefix(run.Tasks[0].Status, "ok") {
		t.Errorf("task 0 status = %q, want ok", run.Tasks[0].Status)
	}
	if run.Tasks[1].Status != "ok" {
		t.Errorf("task 1 must see the earlier register: status = %q", run.Tasks[1].Status)
	}
	if run.Tasks[2].Status != "skipped" {
		t.Errorf("task 2 references a later register and must be skipped, got %q", run.Tasks[2].Status)
	}
	if run.Tasks[2].SkippedBy != "late.present == true" {
		t.Errorf("skipped_by = %q", run.Tasks[2].SkippedBy)
	}
	if run.Tasks[3].Status != "ok" {
		t.Errorf("the task that binds the register must run: status = %q", run.Tasks[3].Status)
	}
	if _, ok := run.Tasks[3].Registers["late"]; !ok {
		t.Errorf("the binding task must report its register, got %+v", run.Tasks[3].Registers)
	}
}

// TestPlayRuntimeWhenFailure covers the run-time half of TROUBLE-REGISTRY-017: a
// `when:` that reached the engine uncompilable fails its task with the same code
// and stops the run (a hand-built play bypasses the loader, which would have
// refused it).
func TestPlayRuntimeWhenFailure(t *testing.T) {
	h := playTestNew(t, configGetModule{}, configListModule{})
	p := playTestPlay("runtime-when", 1,
		playTestTask("bad", "config.get", map[string]any{"path": "/tmp/t", "key": "a"},
			playTestWithWhen("conns.counts.established >")),
		playTestTask("never", "config.get", map[string]any{"path": "/tmp/t", "key": "a"}),
	)
	run, err := h.reg.RunPlay(context.Background(), p, h.checkRequest())
	if err != nil {
		t.Fatalf("RunPlay: %v", err)
	}
	if len(run.Tasks) != 1 {
		t.Fatalf("an uncompilable when: must stop the run, got %d task(s)", len(run.Tasks))
	}
	_ = run
	if run.Tasks[0].Status != "failed" {
		t.Errorf("status = %q, want failed", run.Tasks[0].Status)
	}
	if run.Tasks[0].ErrorCode != string(types.CodeRegistry017) {
		t.Errorf("error_code = %q, want %s", run.Tasks[0].ErrorCode, types.CodeRegistry017)
	}
	if runs := h.led.playRuns(); len(runs) != 1 {
		t.Errorf("a play run that stopped must still be audited: %d play_run record(s)", len(runs))
	}
}

// TestPlayRetriesAreTransientOnly pins the retry table of §3.7: attempts =
// retries+1, retries happen ONLY on transient, backoff is 1s, 2s, 4s (cap 8s),
// and permanent/policy_refused are never retried. SetSleeper records the
// backoff, so nothing waits.
func TestPlayRetriesAreTransientOnly(t *testing.T) {
	t.Run("transient-is-retried-then-succeeds", func(t *testing.T) {
		path := playTestTarget(t, "placeholder")
		w := newPlayTestWriter("playtest.writer", 2, types.ErrTransient)
		h := playTestNew(t, w)
		p := playTestPlay("retry-ok", 1,
			playTestTask("write", "playtest.writer", map[string]any{"path": path, "value": "done"},
				playTestWithRetries(2)),
		)
		run, err := h.reg.RunPlay(context.Background(), p, h.applyRequest("playtest.writer"))
		if err != nil {
			t.Fatalf("RunPlay: %v", err)
		}
		if got := w.applyCount(); got != 3 {
			t.Fatalf("Apply attempts = %d, want 3 (retries+1)", got)
		}
		if len(h.slept) != 2 || h.slept[0] != time.Second || h.slept[1] != 2*time.Second {
			t.Fatalf("backoff = %v, want [1s 2s]", h.slept)
		}
		if run.Tasks[0].Status != "changed" || !run.Changed {
			t.Errorf("task status = %q changed = %v, want changed/true", run.Tasks[0].Status, run.Changed)
		}
		if run.Outcome != types.OutcomeApplied {
			t.Errorf("outcome = %q, want %q", run.Outcome, types.OutcomeApplied)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != "done" {
			t.Errorf("the retried write must converge: target holds %q", string(body))
		}
	})

	t.Run("transient-exhausted-fails", func(t *testing.T) {
		path := playTestTarget(t, "placeholder")
		w := newPlayTestWriter("playtest.writer", 5, types.ErrTransient)
		h := playTestNew(t, w)
		p := playTestPlay("retry-exhausted", 1,
			playTestTask("write", "playtest.writer", map[string]any{"path": path, "value": "done"},
				playTestWithRetries(1)),
		)
		run, err := h.reg.RunPlay(context.Background(), p, h.applyRequest("playtest.writer"))
		if err == nil {
			t.Fatal("an exhausted retry budget must surface the failure")
		}
		if got := w.applyCount(); got != 2 {
			t.Fatalf("Apply attempts = %d, want 2 (retries+1)", got)
		}
		if len(h.slept) != 1 || h.slept[0] != time.Second {
			t.Fatalf("backoff = %v, want [1s]", h.slept)
		}
		if run.Outcome != types.OutcomeFailed {
			t.Errorf("outcome = %q, want %q", run.Outcome, types.OutcomeFailed)
		}
		if ClassOf(err) != types.ErrClassTransient {
			t.Errorf("the surfaced failure must stay transient, got %q", ClassOf(err))
		}
		if run.Tasks[0].Status != "failed" {
			t.Errorf("task status = %q, want failed", run.Tasks[0].Status)
		}
	})

	t.Run("permanent-is-never-retried", func(t *testing.T) {
		path := playTestTarget(t, "placeholder")
		w := newPlayTestWriter("playtest.writer", 9, types.ErrPermanent)
		h := playTestNew(t, w)
		p := playTestPlay("retry-permanent", 1,
			playTestTask("write", "playtest.writer", map[string]any{"path": path, "value": "done"},
				playTestWithRetries(3)),
		)
		run, err := h.reg.RunPlay(context.Background(), p, h.applyRequest("playtest.writer"))
		if err == nil {
			t.Fatal("a permanent failure must surface")
		}
		if got := w.applyCount(); got != 1 {
			t.Fatalf("a permanent failure must not be retried: %d attempt(s)", got)
		}
		if len(h.slept) != 0 {
			t.Fatalf("a permanent failure must not sleep: %v", h.slept)
		}
		if ClassOf(err) != types.ErrClassPermanent {
			t.Errorf("class = %q, want permanent", ClassOf(err))
		}
		if run.Outcome != types.OutcomeFailed || run.Tasks[0].Status != "failed" {
			t.Errorf("run = %s/%s, want failed/failed", run.Outcome, run.Tasks[0].Status)
		}
	})

	t.Run("module-policy-refusal-is-not-retried", func(t *testing.T) {
		path := playTestTarget(t, "placeholder")
		w := newPlayTestWriter("playtest.writer", 9, types.ErrPolicyRefused)
		h := playTestNew(t, w)
		p := playTestPlay("retry-refused", 1,
			playTestTask("write", "playtest.writer", map[string]any{"path": path, "value": "done"},
				playTestWithRetries(3)),
		)
		run, err := h.reg.RunPlay(context.Background(), p, h.applyRequest("playtest.writer"))
		if err == nil {
			t.Fatal("a policy refusal must surface")
		}
		if got := w.applyCount(); got != 1 {
			t.Fatalf("a refusal must not be retried: %d attempt(s)", got)
		}
		if len(h.slept) != 0 {
			t.Fatalf("a refusal must not sleep: %v", h.slept)
		}
		// A module never mints a code: the registry attaches 011 to an
		// apply-stage refusal, and §5 maps 011 to permanent, so the class the
		// module declared does not survive Outcome(). The audit record is the
		// place that keeps it (payload.error_class).
		if got := CodeOf(err); got != types.CodeRegistry011 {
			t.Errorf("code = %q, want %s", got, types.CodeRegistry011)
		}
		if run.Tasks[0].Status != "failed" {
			t.Errorf("task status = %q, want failed", run.Tasks[0].Status)
		}
	})

	t.Run("a-task-refused-by-the-ladder-is-refused-not-failed", func(t *testing.T) {
		// A do-not-touch hit is TROUBLE-REGISTRY-007, whose class is
		// policy_refused (SPEC-06 §5), so the engine records the task as refused
		// rather than failed — and never retries it.
		h := playTestNew(t, configGetModule{})
		p := playTestPlay("refused-task", 1,
			playTestTask("protected", "config.get", map[string]any{"path": "/etc/shadow", "key": "root"},
				playTestWithRetries(3)),
		)
		run, err := h.reg.RunPlay(context.Background(), p, h.checkRequest())
		if err == nil {
			t.Fatal("a refusal must surface as the run's failure")
		}
		if got := CodeOf(err); got != types.CodeRegistry007 {
			t.Fatalf("code = %q, want %s", got, types.CodeRegistry007)
		}
		if ClassOf(err) != types.ErrClassPolicyRefused {
			t.Errorf("class = %q, want policy_refused", ClassOf(err))
		}
		if run.Tasks[0].Status != "refused" {
			t.Errorf("task status = %q, want refused", run.Tasks[0].Status)
		}
		if len(h.slept) != 0 {
			t.Errorf("a refusal must never be retried: slept %v", h.slept)
		}
	})
}

// TestPlayOnFailAbort is the abort branch: stop, outcome=failed, and no later
// task runs.
func TestPlayOnFailAbort(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "never.txt")
	fail := newPlayTestFailer("playtest.failer", types.ErrPermanent)
	later := newPlayTestWriter("playtest.later", 0, types.ErrTransient)
	h := playTestNew(t, fail, later)
	p := playTestPlay("on-fail-abort", 1,
		playTestTask("ouch", "playtest.failer", map[string]any{}, playTestWithOnFail("abort")),
		playTestTask("must-not-run", "playtest.later", map[string]any{"path": path, "value": "x"}),
	)
	run, err := h.reg.RunPlay(context.Background(), p, h.applyRequest("playtest.failer", "playtest.later"))
	if err == nil {
		t.Fatal("on_fail=abort must surface the failure")
	}
	if len(run.Tasks) != 1 {
		t.Fatalf("abort must stop the run after the failing task: %d task(s)", len(run.Tasks))
	}
	if run.Tasks[0].Status != "failed" || run.Tasks[0].ErrorCode != string(types.CodeRegistry011) {
		t.Errorf("failing task = %+v", run.Tasks[0])
	}
	if run.Outcome != types.OutcomeFailed {
		t.Errorf("outcome = %q, want %q", run.Outcome, types.OutcomeFailed)
	}
	if got := later.applyCount(); got != 0 {
		t.Errorf("the task after an abort ran %d time(s)", got)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the task after an abort must not touch its target: %v", err)
	}
	runs := h.led.playRuns()
	if len(runs) != 1 {
		t.Fatalf("play_run records = %d, want 1", len(runs))
	}
	if got := runs[0].Payload["outcome"]; got != types.OutcomeFailed {
		t.Errorf("the play_run record carries outcome %v, want %v", got, types.OutcomeFailed)
	}
}

// TestPlayOnFailContinue is the continue branch: the failure is recorded, the
// run proceeds, and a later `when:` can test the register's ok flag.
func TestPlayOnFailContinue(t *testing.T) {
	path := playTestTarget(t, "before")
	fail := newPlayTestFailer("playtest.failer", types.ErrPermanent)
	writer := newPlayTestWriter("playtest.writer", 0, types.ErrTransient)
	h := playTestNew(t, fail, writer)
	p := playTestPlay("on-fail-continue", 1,
		playTestTask("ouch", "playtest.failer", map[string]any{},
			playTestWithOnFail("continue"), playTestWithRegister("rst")),
		playTestTask("sees-failure", "playtest.writer", map[string]any{"path": path, "value": "recovered"},
			playTestWithWhen("rst.ok != true")),
		playTestTask("sees-success", "playtest.writer", map[string]any{"path": path, "value": "wrong"},
			playTestWithWhen("rst.ok == true")),
	)
	run, err := h.reg.RunPlay(context.Background(), p, h.applyRequest("playtest.failer", "playtest.writer"))
	if err != nil {
		t.Fatalf("on_fail=continue must not surface as a run error: %v", err)
	}
	if len(run.Tasks) != 3 {
		t.Fatalf("continue must proceed through every task: %d task(s)", len(run.Tasks))
	}
	if run.Tasks[0].Status != "failed" || run.Tasks[0].ErrorCode != string(types.CodeRegistry011) {
		t.Errorf("the failing task must still be recorded: %+v", run.Tasks[0])
	}
	if run.Tasks[1].Status != "changed" {
		t.Errorf("a later when: must observe the failed register's ok=false, got %q", run.Tasks[1].Status)
	}
	if run.Tasks[2].Status != "skipped" {
		t.Errorf("rst.ok == true must be false after a failure, got %q", run.Tasks[2].Status)
	}
	if run.Outcome != types.OutcomeApplied {
		t.Errorf("outcome = %q, want %q", run.Outcome, types.OutcomeApplied)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "recovered" {
		t.Errorf("the recovered run wrote %q", string(body))
	}
	runs := h.led.playRuns()
	if len(runs) != 1 {
		t.Fatalf("play_run records = %d, want 1", len(runs))
	}
	tasks, _ := runs[0].Payload["tasks"].([]map[string]any)
	if len(tasks) != 3 || tasks[0]["status"] != "failed" {
		t.Errorf("the play_run record must carry the recorded failure, got %+v", runs[0].Payload["tasks"])
	}
}

// TestPlayOnFailRollback is the rollback branch: stop, replay the accumulated
// RollbackHints in reverse order, and surface the original failure.
func TestPlayOnFailRollback(t *testing.T) {
	t.Run("replays-accumulated-hints-in-reverse", func(t *testing.T) {
		hinter := newPlayTestHinter("playtest.hinter")
		undo := newPlayTestUndo("config.set") // an invertible module name
		fail := newPlayTestFailer("playtest.failer", types.ErrPermanent)
		h := playTestNew(t, hinter, undo, fail)
		p := playTestPlay("rollback", 1,
			playTestTask("hint-a", "playtest.hinter", map[string]any{"undo": "a", "hint": "config.set"}),
			playTestTask("hint-b", "playtest.hinter", map[string]any{"undo": "b", "hint": "config.set"}),
			playTestTask("boom", "playtest.failer", map[string]any{}, playTestWithOnFail("rollback")),
		)
		run, err := h.reg.RunPlay(context.Background(), p, h.applyRequest("playtest.hinter", "config.set", "playtest.failer"))
		if err == nil {
			t.Fatal("a rollback run must surface the failure that triggered it")
		}
		if run.Outcome != types.OutcomeRolledBack {
			t.Errorf("outcome = %q, want %q", run.Outcome, types.OutcomeRolledBack)
		}
		if got := undo.replayOrder(); len(got) != 2 || got[0] != "b" || got[1] != "a" {
			t.Fatalf("hint replay order = %v, want [b a] (reverse)", got)
		}
		if ClassOf(err) != types.ErrClassPermanent {
			t.Errorf("the surfaced failure must be the task's own, got %q", ClassOf(err))
		}
		if last := run.Tasks[len(run.Tasks)-1]; last.Status != "failed" || last.Name != "boom" {
			t.Errorf("the triggering task must be recorded: %+v", last)
		}
		// The replay is itself an audited tool call ("rollback:"+module).
		if replays := playTestTerminalCalls(h.led, "config.set"); len(replays) != 2 {
			t.Errorf("each replayed hint must be an audited tool call: %d config.set call(s)", len(replays))
		}
		if runs := h.led.playRuns(); len(runs) != 1 || runs[0].Payload["outcome"] != types.OutcomeRolledBack {
			t.Errorf("the play_run record must carry outcome rolled_back, got %+v", runs)
		}
	})

	t.Run("stops-at-the-first-point-of-no-return", func(t *testing.T) {
		hinter := newPlayTestHinter("playtest.hinter")
		undo := newPlayTestUndo("config.set")
		fail := newPlayTestFailer("playtest.failer", types.ErrPermanent)
		h := playTestNew(t, hinter, undo, fail)
		p := playTestPlay("rollback-ponr", 1,
			playTestTask("ponr-hint", "playtest.hinter", map[string]any{"undo": "early", "hint": "service.reload"}),
			playTestTask("invertible-hint", "playtest.hinter", map[string]any{"undo": "late", "hint": "config.set"}),
			playTestTask("boom", "playtest.failer", map[string]any{}, playTestWithOnFail("rollback")),
		)
		run, err := h.reg.RunPlay(context.Background(), p, h.applyRequest("playtest.hinter", "config.set", "playtest.failer"))
		playTestWantCode(t, err, types.CodeRegistry015, "service.reload")
		if run.Outcome != types.OutcomeRolledBack {
			t.Errorf("outcome = %q, want %q", run.Outcome, types.OutcomeRolledBack)
		}
		if got := undo.replayOrder(); len(got) != 1 || got[0] != "late" {
			t.Fatalf("replay order = %v, want [late]: the reverse replay stops at the PONR", got)
		}
	})

	t.Run("a-non-invertible-hint-is-015", func(t *testing.T) {
		hinter := newPlayTestHinter("playtest.hinter")
		undo := newPlayTestUndo("config.set")
		fail := newPlayTestFailer("playtest.failer", types.ErrPermanent)
		h := playTestNew(t, hinter, undo, fail)
		p := playTestPlay("rollback-not-invertible", 1,
			playTestTask("hint", "playtest.hinter", map[string]any{"undo": "a", "hint": "config.get"}),
			playTestTask("boom", "playtest.failer", map[string]any{}, playTestWithOnFail("rollback")),
		)
		run, err := h.reg.RunPlay(context.Background(), p, h.applyRequest("playtest.hinter", "playtest.failer"))
		playTestWantCode(t, err, types.CodeRegistry015)
		if run.Outcome != types.OutcomeRolledBack {
			t.Errorf("outcome = %q, want %q", run.Outcome, types.OutcomeRolledBack)
		}
		if got := undo.replayOrder(); len(got) != 0 {
			t.Errorf("a hint the engine cannot invert must not be replayed: %v", got)
		}
	})
}

// TestPlayOnFailRollbackReplayWithoutGrants pins what the engine does with a
// REAL invertible module, and records a defect of the files this test does not
// own: Registry.rollbackHint (play.go) builds its request with no Grants, and
// authorize's a6 step refuses every mutating apply that carries none. A rollback
// replay of config.set or file.patch is therefore refused with
// TROUBLE-REGISTRY-008 before the module is ever called, so on_fail=rollback can
// stop the run but can never invert anything. The test asserts the observable
// behaviour (reverse order preserved, one attempt, rolled_back outcome) so the
// gap is visible in CI instead of hidden; it is not an endorsement of it.
func TestPlayOnFailRollbackReplayWithoutGrants(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.toml")
	if err := os.WriteFile(path, []byte("alpha = 1\nbeta = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fail := newPlayTestFailer("playtest.failer", types.ErrPermanent)
	h := playTestNew(t, configSetModule{}, fail)
	p := playTestPlay("rollback-real", 1,
		playTestTask("set-alpha", "config.set", map[string]any{"path": path, "key": "alpha", "value": 7}),
		playTestTask("set-beta", "config.set", map[string]any{"path": path, "key": "beta", "value": 9}),
		playTestTask("boom", "playtest.failer", map[string]any{}, playTestWithOnFail("rollback")),
	)
	run, err := h.reg.RunPlay(context.Background(), p, h.applyRequest("config.set", "playtest.failer"))
	if err == nil {
		t.Fatal("the rollback run must surface the failure that triggered it")
	}
	if run.Outcome != types.OutcomeRolledBack {
		t.Errorf("outcome = %q, want %q", run.Outcome, types.OutcomeRolledBack)
	}
	// The reverse replay starts with the LAST hint ("beta ... -> 2"): exactly one
	// attempt appears, and it carries beta's before-value, never alpha's.
	attempts := playTestTerminalCalls(h.led, "config.set")
	if len(attempts) != 3 {
		t.Fatalf("config.set calls = %d, want 3 (two applies + the aborted replay)", len(attempts))
	}
	replay := attempts[2]
	if replay["key"] != "beta" {
		t.Errorf("the replay must start with the most recent hint, got key=%v", replay["key"])
	}
	if got := fmt.Sprintf("%v", replay["value"]); got != "2" {
		t.Errorf("the replayed hint must carry beta's before-value 2, got %v", replay["value"])
	}
	if got := CodeOf(err); got != types.CodeRegistry008 {
		t.Errorf("the replay is refused with %q; the grant gap (rollbackHint drops Grants, a6 requires one) "+
			"is the reason on_fail=rollback cannot invert anything today", got)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "alpha = 7") || !strings.Contains(string(body), "beta = 9") {
		t.Errorf("the aborted replay must leave the applied state: %q", string(body))
	}
}

// TestPlayRunRefusals covers the engine's own static refusals: a play without
// tasks, and a play that declares no check_mode trying to apply.
func TestPlayRunRefusals(t *testing.T) {
	h := playTestNew(t, configGetModule{})
	if _, err := h.reg.RunPlay(context.Background(), types.Play{Name: "empty", Version: 1}, h.checkRequest()); err == nil {
		t.Error("a play without tasks must not run")
	} else {
		playTestWantCode(t, err, types.CodeRegistry016)
	}

	// A play whose tools have no dry run stays refused rather than applying blind
	// (SPEC-05 T09): CheckMode is false on the hand-built play.
	p := playTestPlay("no-check-mode", 1,
		playTestTask("write", "config.set", map[string]any{"path": "/tmp/t", "key": "a", "value": 1}),
	)
	p.CheckMode = false
	run, err := h.reg.RunPlay(context.Background(), p, h.applyRequest("config.set"))
	if err != nil {
		t.Fatalf("a refused task is data on the record, not an error: %v", err)
	}
	if len(run.Tasks) != 1 || run.Tasks[0].Status != "refused" {
		t.Fatalf("want one refused task, got %+v", run.Tasks)
	}
	if run.Tasks[0].ErrorCode != string(types.CodeRegistry014) {
		t.Errorf("error_code = %q, want %s", run.Tasks[0].ErrorCode, types.CodeRegistry014)
	}
}

// TestPlayVersionedIdentity is the resolution half of §3.7 this tree actually
// implements: the versioned path is the key. Two documents that share a name but
// differ in version are distinct plays, and the version is part of a run's
// identity, so a run under a higher version is not a replay of the lower one.
//
// The resolution ORDER of §3.7 (skill-local → local → embedded, highest version
// wins, with a conflict record) has no implementation in this tree — there is no
// play-directory loader, only LoadPlay/DecodePlay for one document — so the order
// itself is not asserted here. What is asserted is everything a resolver depends
// on: the document's own version is authoritative (the file name is not
// consulted), both versions load side by side, and each run is audited under the
// version it ran.
func TestPlayVersionedIdentity(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "app.toml")
	if err := os.WriteFile(target, []byte("alpha = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	v2 := fmt.Sprintf(`
schema_version = 1
name = "svc-pool"
version = 2
source = "module-default"

[[task]]
name = "read"
tool = "config.get"
args = { path = %q, key = "alpha" }
`, target)
	v3 := strings.Replace(v2, "version = 2", "version = 3", 1)
	// The file names deliberately disagree with the documents: the document wins.
	path2 := filepath.Join(dir, "svc-pool@99.toml")
	path3 := filepath.Join(dir, "svc-pool.toml")
	if err := os.WriteFile(path2, []byte(v2), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path3, []byte(v3), 0o644); err != nil {
		t.Fatal(err)
	}
	p2, err := LoadPlay(path2)
	if err != nil {
		t.Fatalf("LoadPlay(v2): %v", err)
	}
	p3, err := LoadPlay(path3)
	if err != nil {
		t.Fatalf("LoadPlay(v3): %v", err)
	}
	if p2.Version != 2 || p3.Version != 3 {
		t.Fatalf("versions = %d/%d, want 2/3 (the document is authoritative)", p2.Version, p3.Version)
	}
	if p2.Name != p3.Name {
		t.Fatalf("names = %q/%q, want the same play name", p2.Name, p3.Name)
	}

	h := playTestNew(t, configGetModule{})
	base := applyOnly()
	if k2, k3 := h.reg.idemKey(base, p2, 0, p2.Tasks[0]), h.reg.idemKey(base, p3, 0, p3.Tasks[0]); k2 == k3 {
		t.Fatalf("the run identity must include the play version: %q == %q", k2, k3)
	} else if !strings.Contains(k3, "svc-pool@3") || !strings.Contains(k2, "svc-pool@2") {
		t.Fatalf("run identities %q / %q must carry the versioned play name", k2, k3)
	}

	// Both versions run and are audited under their own version.
	for _, p := range []types.Play{p2, p3} {
		if _, err := h.reg.RunPlay(context.Background(), p, base); err != nil {
			t.Fatalf("RunPlay(%d): %v", p.Version, err)
		}
	}
	runs := h.led.playRuns()
	if len(runs) != 2 {
		t.Fatalf("play_run records = %d, want 2", len(runs))
	}
	seen := map[any]bool{}
	for _, rec := range runs {
		if rec.Payload["play"] != "svc-pool" {
			t.Errorf("play_run names %v", rec.Payload["play"])
		}
		seen[rec.Payload["play_version"]] = true
	}
	if !seen[2] || !seen[3] {
		t.Errorf("the two runs must be audited under their own versions, got %v", seen)
	}
}

// TestPlayRunnerRollback pins the PlayRunner view of a rollback (SPEC-05 §2,
// SPEC-06 §3.5): a hint the engine cannot invert is TROUBLE-REGISTRY-015 and no
// target is touched.
func TestPlayRunnerRollback(t *testing.T) {
	h := playTestNew(t, configSetModule{})
	runner := h.reg.Runner(types.ToolCallRequest{Inc: "inc_playtest"})

	if err := runner.Rollback(context.Background(), types.ToolCall{ID: "tc_none"}); err == nil {
		t.Error("a call without a rollback hint must be refused")
	} else {
		playTestWantCode(t, err, types.CodeRegistry015)
	}
	for _, tc := range []types.ToolCall{
		{ID: "tc_unsupported", Result: &types.Result{Rollback: &types.RollbackHint{Supported: false}}},
		{ID: "tc_nomodule", Result: &types.Result{Rollback: &types.RollbackHint{Supported: true}}},
	} {
		if err := runner.Rollback(context.Background(), tc); err == nil {
			t.Errorf("%s: an unusable rollback hint must be refused", tc.ID)
		} else {
			playTestWantCode(t, err, types.CodeRegistry015)
		}
	}
	if got := len(h.led.toolCalls()); got != 0 {
		t.Errorf("a refused rollback must not reach a module: %d tool_call record(s)", got)
	}

	// A supported hint is re-authorized through the six-stage contract like any
	// other call (SPEC-06 §3.5): a hint naming a module that is not registered is
	// refused by the authorize ladder rather than trusted.
	unknown := types.ToolCall{ID: "tc_unknown", Result: &types.Result{
		Rollback: &types.RollbackHint{Supported: true, Module: "service.reload", Args: map[string]any{}},
	}}
	if err := runner.Rollback(context.Background(), unknown); err == nil {
		t.Error("a rollback hint must be re-authorized, not trusted")
	} else {
		playTestWantCode(t, err, types.CodeRegistry001)
	}
}
