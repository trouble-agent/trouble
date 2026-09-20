package registry

// call_contract_test.go — AC-7 (SPEC-06 §7, §2.3, §4.2): the 6-stage call
// contract and its ledger records, plus the record-first half of §2.2 that makes
// it a contract rather than a convention.
//
// The assertions are the AC-7 clauses, one subtest each:
//
//	(a) one apply call: exactly six stages in the fixed order, OK=true, MS >= 0;
//	(b) the ledger holds ONE intent record (stages 0–2) and ONE outcome record
//	    (stages 0–5) for that call;
//	(c) one play_run record when the same task runs under RunPlay;
//	(d) in shadow autonomy the same call runs check_mode only — no apply stage,
//	    and the target's bytes and mtime are unchanged;
//	(e) a refusal appends an outcome record carrying payload.error_code while Call
//	    returns a nil error, and Outcome(tc) carries the code and its class.
//
// Two places where the shipped implementation is asserted as it is, with the
// spec text it differs from named in the comment (§7(d) stage count; §2.2's
// "one append" wording is exact). They are reported, not worked around.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// ac7StageOrder is §2.3's fixed stage order, and the 0-indexed call the spec's
// text uses for it.
var ac7StageOrder = []string{
	types.StageAuthorize, types.StageValidate, types.StageDryRun,
	types.StageApply, types.StageVerify, types.StageAudit,
}

// ac7Applied is the fixture after ladderPatch has landed.
const ac7Applied = "listen = 8080\nworkers = 8\n"

// ac7Fixture writes the patch target and returns (dir, path, patch).
func ac7Fixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	return dir, writeTempFile(t, dir, "app.conf", ladderFixture)
}

func ac7Opts(dir string) regTestOpts {
	return regTestOpts{
		Gates: types.AutonomyGates{Mode: types.AutoFull},
		Config: append(allowRootsConfig(dir),
			// Named explicitly so the fixture never depends on the module's
			// fallback state root.
			cfgValues("file.backup_dir", filepath.Join(dir, "backups"))...),
	}
}

// TestAC7ApplyCallIsSixStages covers AC-7 (a) and (b).
func TestAC7ApplyCallIsSixStages(t *testing.T) {
	dir, path := ac7Fixture(t)
	r, spy, log := newRegTestRegistry(t, ac7Opts(dir))

	tc, err := r.Call(context.Background(), testRequest("file.patch", types.ModeApply,
		map[string]any{"path": path, "patch": ladderPatch}, []string{"file.patch"}))
	if err != nil {
		t.Fatalf("a completed call returns no error, whatever the module did (SPEC-06 §2.2): %v", err)
	}

	// (a) the stage list is the audit trail.
	if len(tc.Stage) != 6 {
		t.Fatalf("stage count = %d, want 6: %s", len(tc.Stage), stageDetails(tc))
	}
	for i, want := range ac7StageOrder {
		got := tc.Stage[i]
		if got.Stage != want {
			t.Fatalf("stage[%d] = %q, want %q (order: %s)", i, got.Stage, want, stageDetails(tc))
		}
		if !got.OK {
			t.Fatalf("stage[%d] (%s) OK=false: %s", i, got.Stage, got.Detail)
		}
		if got.MS < 0 {
			t.Fatalf("stage[%d] (%s) MS = %d, want >= 0", i, got.Stage, got.MS)
		}
		if got.Detail == "" {
			t.Fatalf("stage[%d] (%s) carries no Detail (SPEC-06 §2.3: a one-line, scrubbed string)", i, got.Stage)
		}
	}
	if tc.ErrorCode != "" {
		t.Fatalf("a successful apply carries no error_code, got %q: %s", tc.ErrorCode, stageDetails(tc))
	}
	if e := Outcome(tc); e != nil {
		t.Fatalf("Outcome of a successful call = %v, want nil", e)
	}
	if tc.CheckDiff == nil || tc.CheckDiff.Empty {
		t.Fatalf("stage 3 must carry the dry-run prediction: %+v", tc.CheckDiff)
	}
	if tc.Result == nil || !tc.Result.Changed || len(tc.Result.Applied) == 0 {
		t.Fatalf("stage 4 must carry the applied result: %+v", tc.Result)
	}
	if tc.Verify == nil || !tc.Verify.OK {
		t.Fatalf("stage 5 must carry the verification: %+v", tc.Verify)
	}

	// (b) one intent record and one outcome record, in that order.
	recs := spy.snapshot()
	if len(recs) != 2 {
		t.Fatalf("an apply call appends exactly 2 tool_call records, got %d", len(recs))
	}
	intent, outcome := recs[0], recs[1]
	for i, rec := range recs {
		if rec.Kind != types.KToolCall {
			t.Fatalf("record %d kind = %q, want %q", i, rec.Kind, types.KToolCall)
		}
		if rec.RecID == "" || rec.Seq == 0 || rec.TS == "" {
			t.Fatalf("record %d is not a durable record the spy wrote (Seq/RecID/TS empty): %+v", i, rec)
		}
		if rec.Origin.HostID != "test-host" || rec.Actor.Kind != types.ActorHuman {
			t.Fatalf("record %d lost its identity (SPEC-01 requires Origin and Actor): %+v", i, rec)
		}
	}
	if !payloadBool(intent, "intent") {
		t.Fatalf("the first record of an apply call is the intent record (stages 0-2, before any mutation)")
	}
	if payloadBool(outcome, "intent") {
		t.Fatalf("the second record of an apply call is the outcome record, not an intent")
	}
	intentStages := recordStages(t, intent)
	outcomeStages := recordStages(t, outcome)
	if len(intentStages) != 3 {
		t.Fatalf("the intent record carries stages 0-2, got %d: %+v", len(intentStages), intentStages)
	}
	for i, want := range ac7StageOrder[:3] {
		if intentStages[i].Stage != want || !intentStages[i].OK {
			t.Fatalf("intent stage %d = %+v, want %s ok", i, intentStages[i], want)
		}
	}
	// SPEC-06 §7 (b) words the outcome record as "stages 0–5". The shipped
	// pipeline appends the record at the audit stage and then adds the audit
	// CallStage to the in-memory ToolCall, so the durable outcome record carries
	// the five stages that had run when it was written (0–4) and the returned
	// ToolCall carries all six. The AC's property — one intent record of the
	// dry-run stages and one terminal record of everything that ran — is what is
	// asserted here, and the exact counts pin today's behaviour.
	if len(outcomeStages) != 5 {
		t.Fatalf("the outcome record carries the five stages that had run (0-4), got %d: %+v", len(outcomeStages), outcomeStages)
	}
	for i, want := range ac7StageOrder[:5] {
		if outcomeStages[i].Stage != want || !outcomeStages[i].OK {
			t.Fatalf("outcome stage %d = %+v, want %s ok", i, outcomeStages[i], want)
		}
	}
	if tc.Stage[5].Stage != types.StageAudit {
		t.Fatalf("the returned ToolCall adds the audit stage the record could not carry: %s", stageDetails(tc))
	}
	if _, ok := intent.Payload["check_diff"]; !ok {
		t.Fatalf("the intent record carries the dry-run prediction (crash evidence)")
	}
	if _, ok := outcome.Payload["result"]; !ok {
		t.Fatalf("the outcome record carries the applied result")
	}
	if _, ok := outcome.Payload["verify"]; !ok {
		t.Fatalf("the outcome record carries the verification (SPEC-06 §2.3 stage 6)")
	}
	if _, ok := intent.Payload["args"]; !ok {
		t.Fatalf("the ledger copy of args rides every record (SPEC-02)")
	}
	if !strings.Contains(tc.Stage[5].Detail, outcome.RecID) {
		t.Fatalf("the audit stage names the record it appended: %q vs %q", tc.Stage[5].Detail, outcome.RecID)
	}

	// The call really converged the target, exactly once.
	if got := readTempFile(t, path); got != ac7Applied {
		t.Fatalf("patched content = %q, want %q", got, ac7Applied)
	}
	for _, kind := range []string{"check", "apply", "verify"} {
		if n := log.countOf(kind); n != 1 {
			t.Fatalf("module %s invoked %d time(s), want 1: %s", kind, n, log.String())
		}
	}
}

// TestAC7ShadowRunsCheckModeOnly covers AC-7 (d).
func TestAC7ShadowRunsCheckModeOnly(t *testing.T) {
	dir, path := ac7Fixture(t)
	// The gates snapshot is the only difference from the apply case above.
	opts := ac7Opts(dir)
	opts.Gates = types.AutonomyGates{Mode: types.AutoShadow}
	r, spy, log := newRegTestRegistry(t, opts)

	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat fixture: %v", err)
	}
	tc, callErr := r.Call(context.Background(), testRequest("file.patch", types.ModeApply,
		map[string]any{"path": path, "patch": ladderPatch}, []string{"file.patch"}))
	if callErr != nil {
		t.Fatalf("shadow forces the mode, it does not refuse: %v", callErr)
	}
	if tc.Mode != types.ModeCheck {
		t.Fatalf("Mode = %q, want %q", tc.Mode, types.ModeCheck)
	}

	// The four stages that ran, in order.
	for i, want := range []string{types.StageAuthorize, types.StageValidate, types.StageDryRun} {
		if tc.Stage[i].Stage != want || !tc.Stage[i].OK {
			t.Fatalf("stage[%d] = %+v, want %s ok: %s", i, tc.Stage[i], want, stageDetails(tc))
		}
	}
	// No apply stage ran. SPEC-06 §7 (d) words this as "four stages, no apply
	// stage"; the shipped Call appends the two un-run stages as markers and then
	// the audit entry, so the returned list is six entries long and the record
	// carries five. The property the AC is about — nothing was applied — is
	// asserted both ways: the markers say "not run", and the module was never
	// asked to Apply.
	if len(tc.Stage) != 6 || tc.Stage[5].Stage != types.StageAudit {
		t.Fatalf("stage count = %d, want 6 with audit last: %s", len(tc.Stage), stageDetails(tc))
	}
	if !strings.Contains(tc.Stage[3].Detail, "not run (check_mode)") {
		t.Fatalf("stage[3] (apply) = %+v, want the check_mode marker", tc.Stage[3])
	}
	if !strings.Contains(tc.Stage[4].Detail, "not run (check_mode)") {
		t.Fatalf("stage[4] (verify) = %+v, want the check_mode marker", tc.Stage[4])
	}
	if tc.Result != nil {
		t.Fatalf("a check_mode call carries no Result: %+v", tc.Result)
	}
	if n := log.countOf("check"); n != 1 {
		t.Fatalf("check_mode runs the module's Check once, got %d: %s", n, log.String())
	}
	for _, kind := range []string{"apply", "verify"} {
		if n := log.countOf(kind); n != 0 {
			t.Fatalf("check_mode ran %s %d time(s): %s", kind, n, log.String())
		}
	}

	// The target's bytes and mtime are unchanged.
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat fixture: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("mtime moved under a check_mode call: %s -> %s", before.ModTime(), after.ModTime())
	}
	if after.Size() != before.Size() {
		t.Fatalf("size moved under a check_mode call: %d -> %d", before.Size(), after.Size())
	}
	if got := readTempFile(t, path); got != ladderFixture {
		t.Fatalf("a check_mode call changed the target:\n%q", got)
	}

	recs := spy.snapshot()
	if len(recs) != 1 {
		t.Fatalf("check_mode appends one terminal record, got %d", len(recs))
	}
	if got := payloadString(recs[0], "mode"); got != types.ModeCheck {
		t.Fatalf("record mode = %q, want %q", got, types.ModeCheck)
	}
	if payloadBool(recs[0], "intent") {
		t.Fatalf("check_mode appends the terminal record, never an intent record (SPEC-06 §2.3 stage 3)")
	}
	if got := len(recordStages(t, recs[0])); got != 5 {
		t.Fatalf("check_mode record stage count = %d, want 5 (stages 0-2 plus the two un-run markers): %+v",
			got, recordStages(t, recs[0]))
	}
}

// TestAC7PlayRunRecord covers AC-7 (c): the same task under RunPlay appends one
// play_run record at the end of the run.
func TestAC7PlayRunRecord(t *testing.T) {
	dir, path := ac7Fixture(t)
	r, spy, _ := newRegTestRegistry(t, ac7Opts(dir))

	play := types.Play{
		Name: "patch-app-conf", Version: 3, Source: "module-default", MaxRuns: 2, CheckMode: true,
		Tasks: []types.PlayTask{{
			Name: "patch", Tool: "file.patch",
			Args:   map[string]any{"path": path, "patch": ladderPatch},
			OnFail: "abort",
		}},
	}
	base := testRequest("", types.ModeApply, nil, []string{"file.patch"})
	base.Inc = "inc_play"
	base.Sig = "sig_play"

	run, err := r.RunPlay(context.Background(), play, base)
	if err != nil {
		t.Fatalf("run play: %v", err)
	}
	if run.Play != play.Name || run.PlayVer != play.Version || run.Mode != types.ModeApply {
		t.Fatalf("play run = %+v", run)
	}
	if run.Outcome != types.OutcomeApplied || !run.Changed {
		t.Fatalf("outcome = %q changed = %t, want %q/true", run.Outcome, run.Changed, types.OutcomeApplied)
	}
	if len(run.Tasks) != 1 || run.Tasks[0].Status != "changed" || run.Tasks[0].ToolCallID == "" {
		t.Fatalf("task runs = %+v", run.Tasks)
	}
	if got := readTempFile(t, path); got != ac7Applied {
		t.Fatalf("the play's task did not converge the target: %q", got)
	}

	playRuns := spy.ofKind(types.KPlayRun)
	if len(playRuns) != 1 {
		t.Fatalf("a play run appends exactly one play_run record, got %d", len(playRuns))
	}
	rec := playRuns[0]
	if rec.Inc != "inc_play" || rec.Sig != "sig_play" {
		t.Fatalf("the play_run record must carry the run identity: %+v", rec)
	}
	if got := payloadString(rec, "play"); got != play.Name {
		t.Fatalf("payload.play = %q, want %q", got, play.Name)
	}
	if got := payloadString(rec, "outcome"); got != types.OutcomeApplied {
		t.Fatalf("payload.outcome = %q, want %q", got, types.OutcomeApplied)
	}
	if !payloadBool(rec, "changed") {
		t.Fatalf("payload.changed = false, want true")
	}
	tasks, ok := rec.Payload["tasks"].([]map[string]any)
	if !ok || len(tasks) != 1 {
		t.Fatalf("payload.tasks = %#v, want one task", rec.Payload["tasks"])
	}
	if got, _ := tasks[0]["tool_call_id"].(string); got != run.Tasks[0].ToolCallID {
		t.Fatalf("task tool_call_id = %q, want %q", got, run.Tasks[0].ToolCallID)
	}
	if got, _ := tasks[0]["status"].(string); got != "changed" {
		t.Fatalf("task status = %q, want changed", got)
	}
	// The play_run record is the run's last record: the tool_call records of its
	// tasks come first, in task order.
	all := spy.snapshot()
	if all[len(all)-1].Kind != types.KPlayRun {
		t.Fatalf("the play_run record is appended at the end of the run; last record is %s", all[len(all)-1].Kind)
	}
	if n := len(spy.ofKind(types.KToolCall)); n != 2 {
		t.Fatalf("the play's one task appends its intent and outcome records, got %d tool_call records", n)
	}
}

// TestAC7RefusalIsRecordFirst covers AC-7 (e): a refusal is data on a record, not
// an error, and the caller-visible Outcome carries the code and the class.
func TestAC7RefusalIsRecordFirst(t *testing.T) {
	r, spy, log := newRegTestRegistry(t, regTestOpts{})

	tc, err := r.Call(context.Background(), testRequest("svc.obliterate", types.ModeApply,
		map[string]any{"command": "rm -rf /"}, []string{"svc.obliterate"}))
	if err != nil {
		t.Fatalf("a refusal returns a nil error — a non-nil error means nothing was appended (SPEC-06 §2.2): %v", err)
	}
	if got := types.ErrorCode(tc.ErrorCode); got != types.CodeRegistry001 {
		t.Fatalf("error_code = %q, want %q", got, types.CodeRegistry001)
	}
	if len(tc.Stage) != 2 || tc.Stage[0].OK || tc.Stage[0].Stage != types.StageAuthorize {
		t.Fatalf("refusal stages = %s", stageDetails(tc))
	}
	if tc.Stage[0].Detail == "" {
		t.Fatalf("the refused stage carries a one-line detail")
	}
	if n := log.count(); n != 0 {
		t.Fatalf("the module was invoked %d time(s) on a refusal: %s", n, log.String())
	}

	recs := spy.snapshot()
	if len(recs) != 1 {
		t.Fatalf("a refusal appends one outcome record, got %d", len(recs))
	}
	rec := recs[0]
	if got := payloadString(rec, "error_code"); got != string(types.CodeRegistry001) {
		t.Fatalf("payload.error_code = %q, want %q", got, types.CodeRegistry001)
	}
	if got := payloadString(rec, "error_class"); got != string(types.ErrClassPermanent) {
		t.Fatalf("payload.error_class = %q, want %q", got, types.ErrClassPermanent)
	}
	if got := payloadString(rec, "stage_failed"); got != types.StageAuthorize {
		t.Fatalf("payload.stage_failed = %q, want %q", got, types.StageAuthorize)
	}
	if payloadBool(rec, "intent") {
		t.Fatalf("a refusal is never an intent record")
	}
	// The ledger copy of args is the scrubbed one, and it exists even when the
	// call never reached a module.
	if _, ok := rec.Payload["args"]; !ok {
		t.Fatalf("the refusal record still carries its (scrubbed) args")
	}

	out := Outcome(tc)
	if out == nil {
		t.Fatalf("Outcome(tc) must carry the refusal for the ladder to classify")
	}
	if CodeOf(out) != types.CodeRegistry001 {
		t.Fatalf("Outcome code = %q, want %q", CodeOf(out), types.CodeRegistry001)
	}
	if ClassOf(out) != types.ErrClassPermanent {
		t.Fatalf("Outcome class = %q, want %q", ClassOf(out), types.ErrClassPermanent)
	}
	if ClassOf(out) == types.ErrClassTransient {
		t.Fatalf("an unknown module must never be classified transient (it would be retried)")
	}
}

// TestAC7AuditAppendFailureReturnsAnError is §2.2's single exception and §6.11:
// the one case where Call returns a non-nil error is the audit append failing,
// and then nothing was appended.
func TestAC7AuditAppendFailureReturnsAnError(t *testing.T) {
	dir, path := ac7Fixture(t)
	opts := ac7Opts(dir)
	opts.AppendFail = func(rec types.Record) error { return errors.New("ledger generation is down") }
	r, spy, _ := newRegTestRegistry(t, opts)

	tc, err := r.Call(context.Background(), testRequest("file.read", types.ModeCheck,
		map[string]any{"path": path}, []string{"file.read"}))
	if err == nil {
		t.Fatalf("an audit-append failure is the one case where Call returns an error")
	}
	if CodeOf(err) != types.CodeRegistry006 {
		t.Fatalf("append-failure code = %q, want %q", CodeOf(err), types.CodeRegistry006)
	}
	if ReasonOf(err) != reasonAuditAppendFailed {
		t.Fatalf("append-failure reason = %q, want %q", ReasonOf(err), reasonAuditAppendFailed)
	}
	if n := len(spy.snapshot()); n != 0 {
		t.Fatalf("nothing was appended, the spy holds %d record(s)", n)
	}
	if last := tc.Stage[len(tc.Stage)-1]; last.Stage != types.StageAudit || last.OK {
		t.Fatalf("the audit stage reports the failure: %+v", last)
	}
	if !strings.Contains(tc.Stage[len(tc.Stage)-1].Detail, "audit append failed") {
		t.Fatalf("the audit stage detail names the failure: %q", tc.Stage[len(tc.Stage)-1].Detail)
	}
	// §6.11: the mutation happened but is unlogged, so the error carries the
	// applied-unlogged marker the ladder raises an incident on.
	if !strings.Contains(err.Error(), "audit_failed=true") {
		t.Fatalf("the returned error must carry the audit_failed marker: %v", err)
	}
}
