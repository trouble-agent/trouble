package registry

// modules_flow_test.go — per-module units for flow.file_issue, flow.create_task
// and flow.comment (SPEC-06 §7 rows: check-diff shape, apply semantics, the
// convergent/once idempotency contract, verify, every error in the module's own
// row, and the PONR authorize path). The wired subsystem is an in-process fake
// closure throughout: no test here touches a network, a git checkout, a board
// file or the fleet.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/trouble-agent/trouble/internal/registry/validate"
	"github.com/trouble-agent/trouble/internal/types"
)

const flowTestSig = "sentinel:sha256v1:9f2c1d3e4b5a6c7d"

// ---------------------------------------------------------------------------
// the fake subsystem
// ---------------------------------------------------------------------------

// flowFake is an in-process stand-in for the wired subsystem whose behaviour
// follows the SPEC-08/09 contract the modules are written against: one issue per
// sig (a recurrence is a comment + counter), one board row per sig, one comment
// per body-hash trigger, and dry runs that answer without acting. It logs every
// call so a test can prove what the module asked for.
type flowFake struct {
	mu       sync.Mutex
	issues   map[string]flowFakeIssue
	rows     map[string]string
	comments map[string][]string
	ids      int
	// filings/rowAppends/commentAppends count the artefacts the fake actually
	// created: the observation log of "did the module file twice?".
	filings        int
	rowAppends     int
	commentAppends int
	// validateRC is the rc the driver reports for a board row (SPEC-08 §3.1).
	validateRC int
	// fileErr/taskErr/commentErr make a driver fail.
	fileErr    error
	taskErr    error
	commentErr error
	calls      []flowFakeCall
}

type flowFakeIssue struct {
	ID       string
	State    string
	Comments int
}

type flowFakeCall struct {
	at     string
	args   map[string]any
	dryRun bool
}

func newFlowFake() *flowFake {
	return &flowFake{
		issues:   map[string]flowFakeIssue{},
		rows:     map[string]string{},
		comments: map[string][]string{},
	}
}

func (f *flowFake) env(t *testing.T) *moduleEnv {
	t.Helper()
	return &moduleEnv{cfg: defaultConfig(), deps: f.deps()}
}

func (f *flowFake) deps() RegistryDeps {
	return RegistryDeps{FileIssue: f.fileIssue, CreateTask: f.createTask, Comment: f.comment}
}

func (f *flowFake) fileIssue(_ context.Context, in map[string]any) (map[string]any, error) {
	f.record("file_issue", in)
	if f.fileErr != nil {
		return nil, f.fileErr
	}
	sig := flowTestString(in, "sig")
	if issue, ok := f.issues[sig]; ok && issue.State == flowOpen {
		// EnsureBySig folds the recurrence into a comment + counter, never a
		// second issue.
		if !flowTestDryRun(in) {
			issue.Comments++
			f.issues[sig] = issue
		}
		return map[string]any{"created": false, "external_id": issue.ID, "state": issue.State,
			"commented": true, "comments": issue.Comments}, nil
	}
	if flowTestDryRun(in) {
		return map[string]any{"created": true, "state": flowOpen}, nil
	}
	f.ids++
	issue := flowFakeIssue{ID: fmt.Sprintf("iss_%d", f.ids), State: flowOpen}
	f.issues[sig] = issue
	f.filings++
	return map[string]any{"created": true, "external_id": issue.ID, "state": issue.State}, nil
}

func (f *flowFake) createTask(_ context.Context, in map[string]any) (map[string]any, error) {
	f.record("create_task", in)
	if f.taskErr != nil {
		return nil, f.taskErr
	}
	sig := flowTestString(in, "sig")
	if status, ok := f.rows[sig]; ok {
		return map[string]any{"created": false, "dup_of": "tsk_dup", "external_id": "tsk_dup",
			"row_status": status, "validate_rc": f.validateRC}, nil
	}
	if flowTestDryRun(in) {
		return map[string]any{"created": true, "row_status": flowBoardTodo}, nil
	}
	f.ids++
	id := fmt.Sprintf("tsk_%d", f.ids)
	f.rows[sig] = flowBoardTodo
	f.rowAppends++
	return map[string]any{"created": true, "external_id": id, "task_id": id,
		"row_status": flowBoardTodo, "validate_rc": f.validateRC}, nil
}

func (f *flowFake) comment(_ context.Context, in map[string]any) (map[string]any, error) {
	f.record("comment", in)
	if f.commentErr != nil {
		return nil, f.commentErr
	}
	key := flowTestString(in, "sig") + "|" + flowTestString(in, "ref")
	trigger := flowTestString(in, "trigger")
	seen := f.comments[key]
	n := len(seen)
	for _, t := range seen {
		if t == trigger {
			return map[string]any{"appended": false, "comments": n}, nil
		}
	}
	if flowTestDryRun(in) {
		return map[string]any{"appended": true, "comments": n}, nil
	}
	f.comments[key] = append(seen, trigger)
	f.commentAppends++
	return map[string]any{"appended": true, "comments": n + 1}, nil
}

func (f *flowFake) record(at string, in map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, flowFakeCall{at: at, args: in, dryRun: flowTestDryRun(in)})
}

// callCount counts every call the module made to one collaborator.
func (f *flowFake) callCount(at string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.at == at {
			n++
		}
	}
	return n
}

// applyCount counts the calls that were allowed to act (no dry_run).
func (f *flowFake) applyCount(at string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.at == at && !c.dryRun {
			n++
		}
	}
	return n
}

// lastCall returns the last call to one collaborator.
func (f *flowFake) lastCall(t *testing.T, at string) flowFakeCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].at == at {
			return f.calls[i]
		}
	}
	t.Fatalf("the module never called %s", at)
	return flowFakeCall{}
}

func (f *flowFake) filedIssues() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.issues)
}

// filedCount/rowCount/commentCountWritten are the artefact counters: what the
// fake actually created, which is the honest form of "the second Apply is a
// no-op" when a module is driven directly (the registry is what skips a
// module whose Check diff is empty).
func (f *flowFake) filedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.filings
}

func (f *flowFake) rowCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rowAppends
}

func (f *flowFake) commentCountWritten() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.commentAppends
}

// ---------------------------------------------------------------------------
// test helpers
// ---------------------------------------------------------------------------

func flowTestDryRun(in map[string]any) bool { b, _ := in[flowDryRunKey].(bool); return b }

func flowTestString(in map[string]any, key string) string {
	s, _ := in[key].(string)
	return s
}

// flowTestArgs runs stage-2 normalization (JSON round trip + the module's own
// NormalizeArgs) so the module sees what the registry would hand it.
func flowTestArgs(t *testing.T, m argsNormalizer, in map[string]any) map[string]any {
	t.Helper()
	args, err := validate.Normalize(in)
	if err != nil {
		t.Fatalf("normalize args: %v", err)
	}
	args, err = m.NormalizeArgs(args)
	if err != nil {
		t.Fatalf("module normalize: %v", err)
	}
	return args
}

func flowTestIsPermanent(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: want a permanent error, got nil", what)
	}
	if !errors.Is(err, types.ErrPermanent) {
		t.Fatalf("%s: want errors.Is(err, ErrPermanent), got %v", what, err)
	}
}

// flowTestRollback asserts the PONR hint of §3.5: unsupported, no module and an
// empty (never nil) args map, on every flow apply including the no-op ones.
func flowTestRollback(t *testing.T, what string, r types.Result) {
	t.Helper()
	if r.Rollback == nil {
		t.Fatalf("%s: every apply must fill Result.Rollback (§3.5)", what)
	}
	if r.Rollback.Supported {
		t.Fatalf("%s: a flow module must never claim a rollback (§3.5 PONR)", what)
	}
	if r.Rollback.Module != "" {
		t.Fatalf("%s: an unsupported hint names no module, got %q", what, r.Rollback.Module)
	}
	if r.Rollback.Args == nil || len(r.Rollback.Args) != 0 {
		t.Fatalf("%s: an unsupported hint carries an empty args map, got %v", what, r.Rollback.Args)
	}
}

// flowScripted wires the three collaborators onto one scripted reply/error pair.
func flowScripted(reply map[string]any, err error) RegistryDeps {
	fn := func(context.Context, map[string]any) (map[string]any, error) { return reply, err }
	return RegistryDeps{FileIssue: fn, CreateTask: fn, Comment: fn}
}

// flowTestCodedError is a driver error carrying the driver's own code.
type flowTestCodedError struct{ code string }

func (e flowTestCodedError) Error() string         { return "driver refused: " + e.code }
func (e flowTestCodedError) Code() types.ErrorCode { return types.ErrorCode(e.code) }
func (e flowTestCodedError) Unwrap() error         { return nil }

// flowTestRegistry builds a Registry over the three flow modules bound to the
// fake subsystem, so the authorize/PONR path of Call can be driven without the
// daemon's own wiring.
func flowTestRegistry(t *testing.T, env *moduleEnv, gates types.AutonomyGates) *Registry {
	t.Helper()
	deps := RegistryDeps{
		Append: func(rec types.Record) (types.Record, error) {
			rec.RecID = "ev_test"
			return rec, nil
		},
		Scrub: func(_ string, in []byte) (types.ScrubResult, error) {
			return types.ScrubResult{Value: in, BytesIn: len(in)}, nil
		},
		Gates: func() types.AutonomyGates { return gates },
	}
	r, err := NewWith(deps, []types.Module{
		flowFileIssueModule{env: env},
		flowCreateTaskModule{env: env},
		flowCommentModule{env: env},
	})
	if err != nil {
		t.Fatalf("the flow module set must register: %v", err)
	}
	return r
}

func flowTestCall(t *testing.T, r *Registry, req types.ToolCallRequest) types.ToolCall {
	t.Helper()
	tc, err := r.Call(context.Background(), req)
	if err != nil {
		t.Fatalf("Call(%s, %s) appended nothing: %v", req.Module, req.Mode, err)
	}
	return tc
}

func flowTestNoError(t *testing.T, what string, tc types.ToolCall) types.ToolCall {
	t.Helper()
	if tc.ErrorCode != "" {
		t.Fatalf("%s: want no error code, got %s (%v)", what, tc.ErrorCode, tc.Stage)
	}
	return tc
}

// ---------------------------------------------------------------------------
// flow.file_issue
// ---------------------------------------------------------------------------

func TestFlowFileIssueCheckAndApply(t *testing.T) {
	ctx := context.Background()
	f := newFlowFake()
	m := flowFileIssueModule{env: f.env(t)}
	args := flowTestArgs(t, m, map[string]any{
		"sig": flowTestSig, "title": "pool exhaustion in payment-worker",
		"body": "412 established connections", "labels": []any{"hotfix"}, "severity": "high",
	})

	d0, err := m.Check(ctx, args)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d0.Empty || len(d0.Entries) != 1 {
		t.Fatalf("want one predicted entry against an absent issue, got %+v", d0)
	}
	want := types.DiffEntry{Path: "issue:" + flowTestSig, Before: flowAbsent, After: flowOpen}
	if d0.Entries[0].Path != want.Path || d0.Entries[0].Before != want.Before || d0.Entries[0].After != want.After {
		t.Fatalf("Check entry = %+v, want %+v", d0.Entries[0], want)
	}
	if c := f.lastCall(t, "file_issue"); !c.dryRun {
		t.Fatalf("Check must probe with %s=true, got %v", flowDryRunKey, c.args)
	}
	if c := f.lastCall(t, "file_issue"); c.args["driver"] != nil && c.args["driver"] != "" {
		t.Fatalf("Check passed an unexpected driver arg: %v", c.args)
	}

	r, err := m.Apply(ctx, args)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !r.Changed || len(r.Applied) != 1 {
		t.Fatalf("the first Apply must change state exactly once, got %+v", r)
	}
	if r.Applied[0] != d0.Entries[0] {
		t.Fatalf("Check predicted %+v, Apply reported %+v", d0.Entries[0], r.Applied[0])
	}
	if got := r.Output["external_id"]; got != "iss_1" {
		t.Fatalf("Apply Output external_id = %v, want the driver's iss_1", got)
	}
	flowTestRollback(t, "flow.file_issue Apply", r)
	if n := f.filedIssues(); n != 1 {
		t.Fatalf("want exactly one issue filed, got %d", n)
	}

	// The issue now exists: Check is empty (the described state holds) and a
	// second Apply with identical args is a convergent no-op (§3.4).
	d1, err := m.Check(ctx, args)
	if err != nil {
		t.Fatalf("Check after Apply: %v", err)
	}
	if !d1.Empty || len(d1.Entries) != 0 {
		t.Fatalf("Check after Apply must be Diff{Empty:true}, got %+v", d1)
	}
	r2, err := m.Apply(ctx, args)
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if r2.Changed || len(r2.Applied) != 0 {
		t.Fatalf("the second Apply must converge to a no-op, got %+v", r2)
	}
	flowTestRollback(t, "flow.file_issue second Apply", r2)
	if n := f.filedIssues(); n != 1 {
		t.Fatalf("a second Apply filed a second issue: %d", n)
	}
	if n := f.filedCount(); n != 1 {
		t.Fatalf("want exactly one filing across both applies, got %d", n)
	}
}

func TestFlowFileIssueVerify(t *testing.T) {
	ctx := context.Background()
	f := newFlowFake()
	m := flowFileIssueModule{env: f.env(t)}
	args := flowTestArgs(t, m, map[string]any{"sig": flowTestSig, "title": "t", "body": "b"})

	// Before the filing the probe must not claim success.
	vr, err := m.Verify(ctx, args)
	if err != nil {
		t.Fatalf("Verify before Apply: %v", err)
	}
	if vr.OK {
		t.Fatalf("Verify before any filing must be OK=false, got %+v", vr)
	}

	if _, err := m.Apply(ctx, args); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	vr, err = m.Verify(ctx, args)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !vr.OK || vr.Method != "probe" {
		t.Fatalf("want a successful probe, got %+v", vr)
	}
	if got := vr.Detail["external_id"]; got != "iss_1" {
		t.Fatalf("Verify must read back the same external_id, got %v", got)
	}
	if len(vr.Evidence) != 1 || vr.Evidence[0].After != "open#iss_1" {
		t.Fatalf("want the §3.9 shape open#<external_id> as evidence, got %+v", vr.Evidence)
	}
	if c := f.lastCall(t, "file_issue"); !c.dryRun {
		t.Fatalf("Verify must probe with %s=true so the proof files nothing", flowDryRunKey)
	}

	// A driver that reports a closed issue for the sig is a verify failure.
	closed := &moduleEnv{cfg: defaultConfig(), deps: flowScripted(map[string]any{
		"created": false, "external_id": "iss_9", "state": "closed"}, nil)}
	mc := flowFileIssueModule{env: closed}
	vr, err = mc.Verify(ctx, args)
	if err != nil {
		t.Fatalf("Verify (closed): %v", err)
	}
	if vr.OK {
		t.Fatalf("an issue that is not open must fail Verify, got %+v", vr)
	}

	// A driver that folds onto an issue without an external_id cannot prove
	// anything either.
	anon := &moduleEnv{cfg: defaultConfig(), deps: flowScripted(map[string]any{
		"created": false, "state": flowOpen}, nil)}
	vr, err = flowFileIssueModule{env: anon}.Verify(ctx, args)
	if err != nil {
		t.Fatalf("Verify (no id): %v", err)
	}
	if vr.OK {
		t.Fatalf("a fold without an external_id must fail Verify, got %+v", vr)
	}
}

func TestFlowFileIssueRefusals(t *testing.T) {
	ctx := context.Background()
	args := flowTestArgs(t, flowFileIssueModule{}, map[string]any{"sig": flowTestSig, "title": "t", "body": "b"})

	// No wired collaborators at all: permanent, never a panic and never a
	// silent success (§2.2, §3.9).
	var unbound flowFileIssueModule
	if _, err := unbound.Check(ctx, args); !errors.Is(err, types.ErrPermanent) {
		t.Fatalf("unwired Check must be permanent, got %v", err)
	}
	if _, err := unbound.Apply(ctx, args); !errors.Is(err, types.ErrPermanent) {
		t.Fatalf("unwired Apply must be permanent, got %v", err)
	}
	if _, err := unbound.Verify(ctx, args); !errors.Is(err, types.ErrPermanent) {
		t.Fatalf("unwired Verify must be permanent, got %v", err)
	}

	// A partially wired environment is still "not wired" (§2.2's predicate).
	partial := &moduleEnv{cfg: defaultConfig(), deps: RegistryDeps{
		FileIssue: func(context.Context, map[string]any) (map[string]any, error) { return nil, nil },
	}}
	partialMod := flowFileIssueModule{env: partial}
	if _, err := partialMod.Check(ctx, args); !errors.Is(err, types.ErrPermanent) {
		t.Fatalf("a partially wired environment must refuse, got %v", err)
	}

	// The driver's own classes: transient stays transient, anything else is
	// permanent (§3.9 flow row).
	transient := newFlowFake()
	transient.fileErr = fmt.Errorf("%w: github 502", types.ErrTransient)
	transientMod := flowFileIssueModule{env: transient.env(t)}
	if _, err := transientMod.Check(ctx, args); !errors.Is(err, types.ErrTransient) {
		t.Fatalf("a transient driver error must stay transient, got %v", err)
	}
	permanent := newFlowFake()
	permanent.fileErr = errors.New("github 422")
	permanentMod := flowFileIssueModule{env: permanent.env(t)}
	if _, err := permanentMod.Check(ctx, args); !errors.Is(err, types.ErrPermanent) {
		t.Fatalf("an unclassified driver error must be permanent, got %v", err)
	}

	// The driver's own code never becomes a registry code (§5): it rides the
	// error text and Result.Output.upstream_code.
	failed := &moduleEnv{cfg: defaultConfig(), deps: flowScripted(
		map[string]any{"ok": false, "error_code": "TROUBLE-FLOW-001"}, nil)}
	mf := flowFileIssueModule{env: failed}
	_, err := mf.Check(ctx, args)
	flowTestIsPermanent(t, "a driver-reported permanent failure", err)
	if !strings.Contains(err.Error(), "TROUBLE-FLOW-001") {
		t.Fatalf("the driver's own code must ride the error text, got %v", err)
	}
	if code := CodeOf(err); code != "" {
		t.Fatalf("a flow driver failure must not mint a registry code, got %s", code)
	}
	res, err := mf.Apply(ctx, args)
	if err == nil {
		t.Fatalf("a failed driver reply must fail Apply")
	}
	if got := res.Output["upstream_code"]; got != "TROUBLE-FLOW-001" {
		t.Fatalf("Result.Output.upstream_code = %v, want the driver's code", got)
	}
	flowTestRollback(t, "failed flow.file_issue Apply", res)

	retry := &moduleEnv{cfg: defaultConfig(), deps: flowScripted(
		map[string]any{"ok": false, "code": "TROUBLE-ISSUES-009", "retryable": true}, nil)}
	retryMod := flowFileIssueModule{env: retry}
	if _, err := retryMod.Check(ctx, args); !errors.Is(err, types.ErrTransient) {
		t.Fatalf("a reply that declares itself retryable must be transient, got %v", err)
	}

	// An error carrying the driver's code is quoted and copied.
	coded := &moduleEnv{cfg: defaultConfig(), deps: flowScripted(nil, flowTestCodedError{code: "TROUBLE-ISSUES-003"})}
	mc := flowFileIssueModule{env: coded}
	if _, err := mc.Check(ctx, args); !errors.Is(err, types.ErrPermanent) || !strings.Contains(err.Error(), "TROUBLE-ISSUES-003") {
		t.Fatalf("a coded driver error must be permanent and quoted, got %v", err)
	}
	res, _ = mc.Apply(ctx, args)
	if got := res.Output["upstream_code"]; got != "TROUBLE-ISSUES-003" {
		t.Fatalf("Result.Output.upstream_code = %v, want TROUBLE-ISSUES-003", got)
	}

	// A cancelled context is transient, never a silent success.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	cancelledMod := flowFileIssueModule{env: newFlowFake().env(t)}
	if _, err := cancelledMod.Check(cancelled, args); !errors.Is(err, types.ErrTransient) {
		t.Fatalf("a cancelled context must be transient, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// flow.create_task
// ---------------------------------------------------------------------------

func TestFlowCreateTaskCheckAndApply(t *testing.T) {
	ctx := context.Background()
	f := newFlowFake()
	m := flowCreateTaskModule{env: f.env(t)}
	args := flowTestArgs(t, m, map[string]any{
		"sig": flowTestSig, "title": "hotfix: queue wedge", "severity": "high",
		"priority": "P1", "repo": "/srv/src/payment-api", "issue_refs": []any{"iss_01"},
	})

	d0, err := m.Check(ctx, args)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	want := types.DiffEntry{Path: "board:" + flowTestSig, Before: flowAbsent, After: flowBoardTodo}
	if d0.Empty || len(d0.Entries) != 1 || d0.Entries[0] != want {
		t.Fatalf("Check = %+v, want the §3.9 entry %+v", d0, want)
	}
	if c := f.lastCall(t, "create_task"); !c.dryRun {
		t.Fatalf("Check must probe with %s=true", flowDryRunKey)
	}

	r, err := m.Apply(ctx, args)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !r.Changed || len(r.Applied) != 1 || r.Applied[0] != want {
		t.Fatalf("Apply = %+v, want one change %+v", r, want)
	}
	if got := r.Output["external_id"]; got != "tsk_1" {
		t.Fatalf("Apply Output must carry the row id, got %v", got)
	}
	flowTestRollback(t, "flow.create_task Apply", r)

	// Row present: no duplicate append, Check empty, second Apply a no-op.
	d1, err := m.Check(ctx, args)
	if err != nil {
		t.Fatalf("Check after Apply: %v", err)
	}
	if !d1.Empty {
		t.Fatalf("a row for the sig is the described state, got %+v", d1)
	}
	r2, err := m.Apply(ctx, args)
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if r2.Changed || len(r2.Applied) != 0 {
		t.Fatalf("the second Apply must converge, got %+v", r2)
	}
	if got := r2.Output["dup_of"]; got != "tsk_dup" {
		t.Fatalf("the fold must report the row it cross-refs, got %v", got)
	}
	if n := f.rowCount(); n != 1 {
		t.Fatalf("want exactly one row append across both applies, got %d", n)
	}
}

func TestFlowCreateTaskVerify(t *testing.T) {
	ctx := context.Background()
	f := newFlowFake()
	m := flowCreateTaskModule{env: f.env(t)}
	args := flowTestArgs(t, m, map[string]any{"sig": flowTestSig, "title": "t"})

	vr, err := m.Verify(ctx, args)
	if err != nil {
		t.Fatalf("Verify before Apply: %v", err)
	}
	if vr.OK {
		t.Fatalf("Verify without a row must be OK=false, got %+v", vr)
	}

	if _, err := m.Apply(ctx, args); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	vr, err = m.Verify(ctx, args)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !vr.OK || vr.Method != "recheck" {
		t.Fatalf("want a successful recheck, got %+v", vr)
	}
	if got := vr.Detail["row_status"]; got != flowBoardTodo {
		t.Fatalf("Verify must re-read the row, got %v", got)
	}
	if len(vr.Evidence) != 1 || vr.Evidence[0].Path != "board:"+flowTestSig || vr.Evidence[0].After != flowBoardTodo {
		t.Fatalf("want the row as evidence, got %+v", vr.Evidence)
	}

	// A row whose post-append validation failed (validate_rc != 0) is a verify
	// failure (SPEC-08 §3.1).
	bad := newFlowFake()
	bad.validateRC = 1
	mb := flowCreateTaskModule{env: bad.env(t)}
	if _, err := mb.Apply(ctx, args); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	vr, err = mb.Verify(ctx, args)
	if err != nil {
		t.Fatalf("Verify (bad rc): %v", err)
	}
	if vr.OK {
		t.Fatalf("a failed board validation must fail Verify, got %+v", vr)
	}
	if _, ok := vr.Detail["validate_rc"]; !ok {
		t.Fatalf("the recheck must report the validation rc, got %+v", vr.Detail)
	}
}

// ---------------------------------------------------------------------------
// flow.comment
// ---------------------------------------------------------------------------

func TestFlowCommentOnceBodyHash(t *testing.T) {
	ctx := context.Background()
	f := newFlowFake()
	m := flowCommentModule{env: f.env(t)}
	args := flowTestArgs(t, m, map[string]any{"sig": flowTestSig, "body": "escalated: pool exhausted", "ref": "iss_01"})

	d0, err := m.Check(ctx, args)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d0.Empty || len(d0.Entries) != 1 {
		t.Fatalf("want one predicted entry, got %+v", d0)
	}
	e := d0.Entries[0]
	if e.Path != "comment:"+flowTestSig || e.Before != "0" || e.After != "1" {
		t.Fatalf("Check entry = %+v, want comment:<sig> 0 -> 1", e)
	}
	c := f.lastCall(t, "comment")
	if !c.dryRun {
		t.Fatalf("Check must probe with %s=true", flowDryRunKey)
	}
	if c.args["trigger"] != flowCommentTrigger(flowTestSig, "iss_01", "escalated: pool exhausted") {
		t.Fatalf("Check must pass the body-hash trigger, got %v", c.args["trigger"])
	}

	r, err := m.Apply(ctx, args)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !r.Changed || len(r.Applied) != 1 || r.Applied[0] != e {
		t.Fatalf("Apply must report the predicted change %+v, got %+v", e, r)
	}
	flowTestRollback(t, "flow.comment Apply", r)
	if c := f.lastCall(t, "comment"); c.dryRun {
		t.Fatalf("Apply must not be a dry run")
	}

	// Same body again: the desk answers Appended=false for the same trigger, so
	// the diff is empty and a second Apply changes nothing (§3.4 once).
	d1, err := m.Check(ctx, args)
	if err != nil {
		t.Fatalf("Check after Apply: %v", err)
	}
	if !d1.Empty {
		t.Fatalf("a replayed body is the described state, got %+v", d1)
	}
	r2, err := m.Apply(ctx, args)
	if err != nil {
		t.Fatalf("replay Apply: %v", err)
	}
	if r2.Changed || len(r2.Applied) != 0 {
		t.Fatalf("a replay must change nothing, got %+v", r2)
	}
	flowTestRollback(t, "flow.comment replay", r2)
	if n := f.commentCountWritten(); n != 1 {
		t.Fatalf("a replay must not append, got %d appends", n)
	}

	// A corrected body is a new event: Changed=true again.
	other := flowTestArgs(t, m, map[string]any{"sig": flowTestSig, "body": "escalated: pool exhausted (retry)", "ref": "iss_01"})
	d2, err := m.Check(ctx, other)
	if err != nil {
		t.Fatalf("Check (new body): %v", err)
	}
	if d2.Empty || d2.Entries[0].Before != "1" || d2.Entries[0].After != "2" {
		t.Fatalf("a new body must predict the next count, got %+v", d2)
	}
	r3, err := m.Apply(ctx, other)
	if err != nil {
		t.Fatalf("Apply (new body): %v", err)
	}
	if !r3.Changed || len(r3.Applied) != 1 || r3.Applied[0] != d2.Entries[0] {
		t.Fatalf("a new body must append once, got %+v", r3)
	}
	if n := f.commentCountWritten(); n != 2 {
		t.Fatalf("want two appends, got %d", n)
	}
}

func TestFlowCommentVerify(t *testing.T) {
	ctx := context.Background()
	f := newFlowFake()
	m := flowCommentModule{env: f.env(t)}
	args := flowTestArgs(t, m, map[string]any{"sig": flowTestSig, "body": "b", "ref": "iss_01"})

	vr, err := m.Verify(ctx, args)
	if err != nil {
		t.Fatalf("Verify before Apply: %v", err)
	}
	if vr.OK {
		t.Fatalf("Verify before the append must fail, got %+v", vr)
	}

	if _, err := m.Apply(ctx, args); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	vr, err = m.Verify(ctx, args)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !vr.OK || vr.Method != "recheck" {
		t.Fatalf("want a successful recheck, got %+v", vr)
	}
	if got := vr.Detail["comments"]; got != 1 {
		t.Fatalf("Verify must re-read the comment count, got %v", got)
	}
	if len(vr.Evidence) != 1 || vr.Evidence[0].After != "1" {
		t.Fatalf("want the re-read count as evidence, got %+v", vr.Evidence)
	}
	if c := f.lastCall(t, "comment"); !c.dryRun {
		t.Fatalf("Verify must probe with %s=true", flowDryRunKey)
	}
}

// ---------------------------------------------------------------------------
// the PONR authorize path (§3.5) and the registry-level `once` replay (§3.4)
// ---------------------------------------------------------------------------

func TestFlowPONRAuthorize(t *testing.T) {
	f := newFlowFake()
	env := f.env(t)
	r := flowTestRegistry(t, env, types.AutonomyGates{Mode: types.AutoFull})
	args := map[string]any{"sig": flowTestSig, "title": "pool exhaustion", "body": "evidence"}
	module := "flow.file_issue"

	// apply with no grants: refused at step a7 with TROUBLE-REGISTRY-015 and the
	// module is never reached.
	tc := flowTestCall(t, r, types.ToolCallRequest{Module: module, Mode: types.ModeApply, Args: args})
	if tc.ErrorCode != string(types.CodeRegistry015) {
		t.Fatalf("a PONR apply without a grant must be %s, got %q (%v)",
			types.CodeRegistry015, tc.ErrorCode, tc.Stage)
	}
	if len(tc.Stage) == 0 || tc.Stage[0].OK {
		t.Fatalf("the refusal must be recorded at the authorize stage, got %+v", tc.Stage)
	}
	if n := f.callCount("file_issue"); n != 0 {
		t.Fatalf("the module was reached %d time(s) on a refused PONR apply", n)
	}
	if n := f.filedIssues(); n != 0 {
		t.Fatalf("a refused PONR apply must not file anything, got %d", n)
	}

	// A scope: wildcard never authorizes a PONR call — in any mode.
	for _, gates := range []types.AutonomyGates{{Mode: types.AutoFull}, {Mode: types.AutoAssisted}} {
		rg := flowTestRegistry(t, env, gates)
		tc := flowTestCall(t, rg, types.ToolCallRequest{Module: module, Mode: types.ModeApply, Args: args,
			Grants: []string{"scope:flow:issue"}})
		if tc.ErrorCode != string(types.CodeRegistry015) {
			t.Fatalf("mode %s: a scope: wildcard must not authorize a PONR call, got %q",
				gates.Mode, tc.ErrorCode)
		}
	}
	if n := f.filedIssues(); n != 0 {
		t.Fatalf("no refused variant may file anything, got %d", n)
	}

	// The same call in check_mode is allowed without any grant (§3.3): it
	// mutates nothing, so it consumes none.
	tc = flowTestNoError(t, "check_mode without a grant", flowTestCall(t, r, types.ToolCallRequest{
		Module: module, Mode: types.ModeCheck, Args: args}))
	if tc.Mode != types.ModeCheck {
		t.Fatalf("check_mode must stay check_mode, got %q", tc.Mode)
	}
	if len(tc.Stage) < 3 || !tc.Stage[0].OK || !tc.Stage[2].OK {
		t.Fatalf("authorize and dry_run must both pass, got %+v", tc.Stage)
	}
	if tc.CheckDiff == nil || tc.CheckDiff.Empty {
		t.Fatalf("check_mode must return the intent diff, got %+v", tc.CheckDiff)
	}
	if n := f.applyCount("file_issue"); n != 0 {
		t.Fatalf("a check_mode call must only probe, got %d acting call(s)", n)
	}
	if n := f.filedIssues(); n != 0 {
		t.Fatalf("a check_mode call filed an issue: %d", n)
	}

	// An explicit module-name grant authorizes the apply.
	tc = flowTestNoError(t, "apply with the module-name grant", flowTestCall(t, r, types.ToolCallRequest{
		Module: module, Mode: types.ModeApply, Args: args, Grants: []string{module}}))
	if tc.Result == nil || !tc.Result.Changed {
		t.Fatalf("the granted apply must report a change, got %+v", tc.Result)
	}
	flowTestRollback(t, "granted flow.file_issue apply", *tc.Result)
	if n := f.filedIssues(); n != 1 {
		t.Fatalf("want exactly one issue filed, got %d", n)
	}
	if tc.Verify == nil || !tc.Verify.OK {
		t.Fatalf("the granted apply must verify, got %+v", tc.Verify)
	}

	// Shadow forces check_mode even with the grant: the apply becomes a probe.
	shadow := flowTestRegistry(t, newFlowFake().env(t), types.AutonomyGates{Mode: types.AutoShadow})
	tc = flowTestNoError(t, "shadow apply", flowTestCall(t, shadow, types.ToolCallRequest{
		Module: module, Mode: types.ModeApply, Args: args, Grants: []string{module}}))
	if tc.Mode != types.ModeCheck {
		t.Fatalf("shadow must force check_mode, got %q", tc.Mode)
	}
}

func TestFlowCreateTaskPONRAuthorize(t *testing.T) {
	f := newFlowFake()
	r := flowTestRegistry(t, f.env(t), types.AutonomyGates{Mode: types.AutoFull})
	args := map[string]any{"sig": flowTestSig, "title": "hotfix"}

	tc := flowTestCall(t, r, types.ToolCallRequest{Module: "flow.create_task", Mode: types.ModeApply, Args: args,
		Grants: []string{"scope:flow:task"}})
	if tc.ErrorCode != string(types.CodeRegistry015) {
		t.Fatalf("flow.create_task is PONR: want %s, got %q", types.CodeRegistry015, tc.ErrorCode)
	}
	if n := f.applyCount("create_task"); n != 0 {
		t.Fatalf("a refused PONR apply reached the driver %d time(s)", n)
	}

	tc = flowTestNoError(t, "flow.create_task apply", flowTestCall(t, r, types.ToolCallRequest{
		Module: "flow.create_task", Mode: types.ModeApply, Args: args, Grants: []string{"flow.create_task"}}))
	if tc.Result == nil || !tc.Result.Changed {
		t.Fatalf("the granted apply must append the row, got %+v", tc.Result)
	}
}

func TestFlowCommentOnceRegistryReplay(t *testing.T) {
	f := newFlowFake()
	r := flowTestRegistry(t, f.env(t), types.AutonomyGates{Mode: types.AutoFull})
	args := map[string]any{"sig": flowTestSig, "body": "escalated: pool exhausted", "ref": "iss_01"}
	req := types.ToolCallRequest{Module: "flow.comment", Mode: types.ModeApply, Args: args,
		Grants: []string{"flow.comment"}, IdemKey: "inc_1|comment|1"}

	tc := flowTestNoError(t, "flow.comment apply", flowTestCall(t, r, req))
	if tc.Result == nil || !tc.Result.Changed {
		t.Fatalf("the first comment must append once, got %+v", tc.Result)
	}
	if n := f.applyCount("comment"); n != 1 {
		t.Fatalf("want one acting comment call, got %d", n)
	}

	// The same IdemKey is short-circuited by the registry before the module runs
	// (§3.4: Stage[3].Detail == "replayed").
	tc = flowTestNoError(t, "flow.comment replay", flowTestCall(t, r, req))
	if len(tc.Stage) < 4 || tc.Stage[3].Detail != "replayed" {
		t.Fatalf("the same IdemKey must be a replay, got %+v", tc.Stage)
	}
	if n := f.commentCountWritten(); n != 1 {
		t.Fatalf("a replay must not append again, got %d appends", n)
	}

	// A fresh IdemKey with the same body is caught by the module's own body-hash
	// replay key: Check is empty, so the registry skips the apply.
	req2 := req
	req2.IdemKey = "inc_1|comment|2"
	tc = flowTestNoError(t, "flow.comment same body, new key", flowTestCall(t, r, req2))
	if tc.Result == nil || tc.Result.Changed {
		t.Fatalf("the body-hash replay key must converge, got %+v", tc.Result)
	}
	if len(tc.Stage) < 4 || !strings.Contains(tc.Stage[3].Detail, "skipped") {
		t.Fatalf("an empty diff skips the apply, got %+v", tc.Stage)
	}
	if n := f.commentCountWritten(); n != 1 {
		t.Fatalf("a converged body must not append again, got %d appends", n)
	}

	// A corrected body is a new event.
	req3 := req
	req3.IdemKey = "inc_1|comment|3"
	req3.Args = map[string]any{"sig": flowTestSig, "body": "escalated: pool exhausted (retry)", "ref": "iss_01"}
	tc = flowTestNoError(t, "flow.comment new body", flowTestCall(t, r, req3))
	if tc.Result == nil || !tc.Result.Changed {
		t.Fatalf("a corrected body must append once more, got %+v", tc.Result)
	}
	if n := f.commentCountWritten(); n != 2 {
		t.Fatalf("want two appends, got %d", n)
	}
}
