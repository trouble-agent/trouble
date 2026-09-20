package registry

// modules_flow.go — flow.file_issue, flow.create_task, flow.comment (SPEC-06 §3.8, §3.9).
//
// The three flow modules are thin wrappers (§3.9): the registry decodes and
// validates, the wired subsystem performs. Nothing here opens a socket, a git
// checkout or a board file — every effect goes through the function-typed
// collaborators of RegistryDeps (§2.2: the SPEC-08 flow subsystem and the
// SPEC-09 issue desk, bound in cmd/troubled), which is what lets the module set
// be exercised with in-process fakes and keeps "no module shells out" true.
//
// All three are sig-keyed: an existing open issue — or board row — for a sig
// receives a comment/cross-ref and a counter, never a second artefact (§3.9), so
// Check is a real dry run against live state and the class is `convergent`.
// flow.comment is `once` with a body-hash replay key: re-running the same body
// is a no-op, a corrected body appends.
//
// The wiring contract (a map in, a map out, §2.2):
//
//	in    the module's own schema-valid args (decodeArgs is strict, so no key
//	      beyond the descriptor's schema ever reaches the subsystem) plus:
//	      - dry_run: true on every Check and Verify call — the subsystem answers
//	        without acting (the flag its own CLI spells --dry-run, SPEC-08 §2.2),
//	        because Check is stage 3 of the call contract and never mutates (§2.3)
//	        and a verify that files an issue to prove a filing is not a verify.
//	      - trigger (flow.comment): the body-hash replay key the desk dedups on
//	        (SPEC-09 §2.1: Comment appends exactly one comment per distinct
//	        trigger key).
//	out   the driver's answer: created, external_id/issue_id/task_id,
//	      state/status/row_status, comments, appended, validate_rc. A reply that
//	      reports a failure carries `ok: false` and/or the driver's own code
//	      (error_code/upstream_code/code, read by upstreamCode), optionally with
//	      class/error_class = transient or retryable/transient = true.
//
// A driver failure is classified, never rewritten (§3.9, §5): transient →
// types.ErrTransient (TROUBLE-REGISTRY-004/010), anything else →
// types.ErrPermanent (011), and the driver's own code rides
// Result.Output.upstream_code and the error text — this area mints no foreign
// code, so a TROUBLE-FLOW-* / TROUBLE-ISSUES-* code never becomes a registry
// code. A missing collaborator is a permanent refusal too: never a panic on a
// nil closure, never a success reporting a filing that never happened.
//
// All three are PONR (SPEC-06 §3.5): append-only evidence is corrected by a
// comment or a close, never a delete, so RollbackInvertible() is false, every
// Result carries an unsupported RollbackHint and authorize a7 demands an
// explicit module-name grant before an apply (a scope: wildcard is refused).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/trouble-agent/trouble/internal/registry/schemagen"
	"github.com/trouble-agent/trouble/internal/types"
)

type flowFileIssueArgs struct {
	Sig      string   `json:"sig"`
	Title    string   `json:"title" js:"maxlen=200"`
	Body     string   `json:"body" js:"maxlen=65536"`
	Labels   []string `json:"labels,omitempty"`
	Severity string   `json:"severity,omitempty" js:"enum=critical|high|medium|low|info"`
	Driver   string   `json:"driver,omitempty" js:"enum=github|duckbrain"`
}

type flowCreateTaskArgs struct {
	Sig       string   `json:"sig"`
	Title     string   `json:"title" js:"maxlen=200"`
	Severity  string   `json:"severity,omitempty" js:"enum=critical|high|medium|low|info"`
	Priority  string   `json:"priority,omitempty" js:"enum=P0|P1|P2|P3"`
	Repo      string   `json:"repo,omitempty"`
	IssueRefs []string `json:"issue_refs,omitempty"`
}

type flowCommentArgs struct {
	Sig  string `json:"sig"`
	Body string `json:"body" js:"maxlen=65536"`
	Ref  string `json:"ref,omitempty"`
}

type flowFileIssueModule struct{ env *moduleEnv }

var flowFileIssueDescriptor = types.Descriptor{
	Name:        "flow.file_issue",
	Version:     1,
	Schema:      schemagen.Generate("flow.file_issue", 1, flowFileIssueArgs{}),
	Scopes:      []string{"flow:issue"},
	Idempotency: types.IdemConvergent,
	CheckMode:   true,
	TimeoutS:    30,
	Mutating:    true,
}

func (m flowFileIssueModule) bind(e *moduleEnv)            { m.env = e }
func (m flowFileIssueModule) Descriptor() types.Descriptor { return flowFileIssueDescriptor }
func (m flowFileIssueModule) RollbackInvertible() bool     { return false }

func (m flowFileIssueModule) NormalizeArgs(args map[string]any) (map[string]any, error) {
	return args, nil
}

func (m flowFileIssueModule) ProtectedTargets(args map[string]any) []protectedTarget {
	return nil
}

// Check runs the dry run of §3.9: the diff of the *intent* against live state.
// An issue already open for the sig is the described state — EnsureBySig folds
// the recurrence into a comment + counter, never a second issue — so nothing is
// left to apply and a re-run is Diff{Empty:true} (§3.4 convergent).
func (m flowFileIssueModule) Check(ctx context.Context, args map[string]any) (types.Diff, error) {
	var a flowFileIssueArgs
	if err := decodeArgs(args, &a); err != nil {
		return types.Diff{}, err
	}
	rep, err := m.call(ctx, a, true)
	if err != nil {
		return types.Diff{}, err
	}
	if !rep.Created {
		return types.Diff{Empty: true, Summary: fmt.Sprintf(
			"issue:%s already %s (EnsureBySig folds a recurrence into a comment, never a second issue)",
			a.Sig, flowIssueState(rep))}, nil
	}
	entry := flowIssueEntry(a.Sig)
	return types.Diff{
		Empty:   false,
		Entries: []types.DiffEntry{entry},
		Summary: fmt.Sprintf("issue:%s %s -> %s", a.Sig, entry.Before, entry.After),
	}, nil
}

// Apply files the issue through the wired subsystem (§3.9). Reported
// Created=false — the issue appeared between Check and Apply, or the driver
// folded the recurrence — means this call changed nothing: Changed=false with an
// empty Applied, which is the convergent second-apply contract of §3.4.
func (m flowFileIssueModule) Apply(ctx context.Context, args map[string]any) (types.Result, error) {
	var a flowFileIssueArgs
	if err := decodeArgs(args, &a); err != nil {
		return types.Result{}, err
	}
	rb := flowRollbackHint()
	rep, err := m.call(ctx, a, false)
	if err != nil {
		return types.Result{Output: flowFailureOutput(a.Sig, rep, err), Rollback: rb}, err
	}
	out := flowOutput(a.Sig, rep)
	out["created"] = rep.Created
	if !rep.Created {
		out["folded"] = true
		return types.Result{Changed: false, Output: out, Rollback: rb}, nil
	}
	return types.Result{
		Changed:  true,
		Applied:  []types.DiffEntry{flowIssueEntry(a.Sig)},
		Output:   out,
		Rollback: rb,
	}, nil
}

// Verify is the §3.9 probe: re-EnsureBySig returns Created=false with the same
// external_id, read back as a dry run so the proof never files anything.
func (m flowFileIssueModule) Verify(ctx context.Context, args map[string]any) (types.VerifyResult, error) {
	var a flowFileIssueArgs
	if err := decodeArgs(args, &a); err != nil {
		return types.VerifyResult{}, err
	}
	rep, err := m.call(ctx, a, true)
	if err != nil {
		return types.VerifyResult{}, err
	}
	detail := map[string]any{"sig": a.Sig, "created": rep.Created, "external_id": rep.ExternalID, "state": rep.State}
	switch {
	case rep.Created:
		detail["ok_reason"] = "the driver reports no open issue for the sig: the filing did not stick"
		return types.VerifyResult{OK: false, Method: "probe", Detail: detail}, nil
	case rep.ExternalID == "":
		detail["ok_reason"] = "the driver reports an issue for the sig without an external_id"
		return types.VerifyResult{OK: false, Method: "probe", Detail: detail}, nil
	case rep.State != "" && rep.State != flowOpen:
		detail["ok_reason"] = "the issue for the sig is not open"
		return types.VerifyResult{OK: false, Method: "probe", Detail: detail}, nil
	}
	return types.VerifyResult{
		OK:       true,
		Method:   "probe",
		Detail:   detail,
		Evidence: []types.DiffEntry{{Path: flowIssuePath(a.Sig), After: flowIssueState(rep)}},
	}, nil
}

// call performs one wired FileIssue call: a dry run for Check/Verify, the real
// call for Apply.
func (m flowFileIssueModule) call(ctx context.Context, a flowFileIssueArgs, dryRun bool) (flowReply, error) {
	env, err := flowEnv(m.env, flowFileIssueDescriptor.Name)
	if err != nil {
		return flowReply{}, err
	}
	in := map[string]any{"sig": a.Sig, "title": a.Title, "body": a.Body}
	if len(a.Labels) > 0 {
		in["labels"] = a.Labels
	}
	if a.Severity != "" {
		in["severity"] = a.Severity
	}
	if a.Driver != "" {
		in["driver"] = a.Driver
	}
	if dryRun {
		in[flowDryRunKey] = true
	}
	return flowCall(ctx, flowFileIssueDescriptor.Name, env.deps.FileIssue, in)
}

type flowCreateTaskModule struct{ env *moduleEnv }

var flowCreateTaskDescriptor = types.Descriptor{
	Name:        "flow.create_task",
	Version:     1,
	Schema:      schemagen.Generate("flow.create_task", 1, flowCreateTaskArgs{}),
	Scopes:      []string{"flow:task"},
	Idempotency: types.IdemConvergent,
	CheckMode:   true,
	TimeoutS:    30,
	Mutating:    true,
}

func (m flowCreateTaskModule) bind(e *moduleEnv)            { m.env = e }
func (m flowCreateTaskModule) Descriptor() types.Descriptor { return flowCreateTaskDescriptor }
func (m flowCreateTaskModule) RollbackInvertible() bool     { return false }

func (m flowCreateTaskModule) NormalizeArgs(args map[string]any) (map[string]any, error) {
	return args, nil
}

func (m flowCreateTaskModule) ProtectedTargets(args map[string]any) []protectedTarget {
	return nil
}

// Check is the §3.9 dry run of the row intent: an existing row for the sig (the
// subsystem comments or cross-refs instead of appending a duplicate) is
// Diff{Empty:true}, an absent one is `board:<sig> absent -> todo`.
func (m flowCreateTaskModule) Check(ctx context.Context, args map[string]any) (types.Diff, error) {
	var a flowCreateTaskArgs
	if err := decodeArgs(args, &a); err != nil {
		return types.Diff{}, err
	}
	rep, err := m.call(ctx, a, true)
	if err != nil {
		return types.Diff{}, err
	}
	if !rep.Created {
		return types.Diff{Empty: true, Summary: fmt.Sprintf(
			"board:%s already %s (the subsystem comments or cross-refs instead of appending a duplicate row)",
			a.Sig, flowStateOrUnknown(rep.State))}, nil
	}
	entry := flowBoardEntry(a.Sig)
	return types.Diff{
		Empty:   false,
		Entries: []types.DiffEntry{entry},
		Summary: fmt.Sprintf("board:%s %s -> %s", a.Sig, entry.Before, entry.After),
	}, nil
}

// Apply appends the board row (board-jsonl) or dispatches it (task-router)
// through the wired subsystem. A row already open for the sig — the driver
// answers Created=false and reports the row it folded into — changes nothing,
// which is the convergent second apply of §3.4.
func (m flowCreateTaskModule) Apply(ctx context.Context, args map[string]any) (types.Result, error) {
	var a flowCreateTaskArgs
	if err := decodeArgs(args, &a); err != nil {
		return types.Result{}, err
	}
	rb := flowRollbackHint()
	rep, err := m.call(ctx, a, false)
	if err != nil {
		return types.Result{Output: flowFailureOutput(a.Sig, rep, err), Rollback: rb}, err
	}
	out := flowOutput(a.Sig, rep)
	out["created"] = rep.Created
	if !rep.Created {
		out["dup_of"] = flowStringOf(rep.Raw, "dup_of", "external_id")
		return types.Result{Changed: false, Output: out, Rollback: rb}, nil
	}
	return types.Result{
		Changed:  true,
		Applied:  []types.DiffEntry{flowBoardEntry(a.Sig)},
		Output:   out,
		Rollback: rb,
	}, nil
}

// Verify re-reads the board row and validates it post-append (§3.9, SPEC-08
// §3.1: a row is validated with the configured validate_cmd, whose rc the driver
// reports as validate_rc). The read is a dry run: the probe never appends.
func (m flowCreateTaskModule) Verify(ctx context.Context, args map[string]any) (types.VerifyResult, error) {
	var a flowCreateTaskArgs
	if err := decodeArgs(args, &a); err != nil {
		return types.VerifyResult{}, err
	}
	rep, err := m.call(ctx, a, true)
	if err != nil {
		return types.VerifyResult{}, err
	}
	detail := map[string]any{"sig": a.Sig, "created": rep.Created, "external_id": rep.ExternalID, "row_status": rep.State}
	if rep.ValidateRC >= 0 {
		detail["validate_rc"] = rep.ValidateRC
	}
	switch {
	case rep.Created:
		detail["ok_reason"] = "the board carries no row for the sig: the append did not stick"
		return types.VerifyResult{OK: false, Method: "recheck", Detail: detail}, nil
	case rep.State == "":
		detail["ok_reason"] = "the row for the sig was re-read without a status"
		return types.VerifyResult{OK: false, Method: "recheck", Detail: detail}, nil
	case rep.ValidateRC > 0:
		detail["ok_reason"] = "the post-append board validation failed"
		return types.VerifyResult{OK: false, Method: "recheck", Detail: detail}, nil
	}
	return types.VerifyResult{
		OK:       true,
		Method:   "recheck",
		Detail:   detail,
		Evidence: []types.DiffEntry{{Path: flowBoardPath(a.Sig), After: rep.State}},
	}, nil
}

func (m flowCreateTaskModule) call(ctx context.Context, a flowCreateTaskArgs, dryRun bool) (flowReply, error) {
	env, err := flowEnv(m.env, flowCreateTaskDescriptor.Name)
	if err != nil {
		return flowReply{}, err
	}
	in := map[string]any{"sig": a.Sig, "title": a.Title}
	if a.Severity != "" {
		in["severity"] = a.Severity
	}
	if a.Priority != "" {
		in["priority"] = a.Priority
	}
	if a.Repo != "" {
		in["repo"] = a.Repo
	}
	if len(a.IssueRefs) > 0 {
		in["issue_refs"] = a.IssueRefs
	}
	if dryRun {
		in[flowDryRunKey] = true
	}
	return flowCall(ctx, flowCreateTaskDescriptor.Name, env.deps.CreateTask, in)
}

type flowCommentModule struct{ env *moduleEnv }

var flowCommentDescriptor = types.Descriptor{
	Name:        "flow.comment",
	Version:     1,
	Schema:      schemagen.Generate("flow.comment", 1, flowCommentArgs{}),
	Scopes:      []string{"flow:comment"},
	Idempotency: types.IdemOnce,
	CheckMode:   true,
	TimeoutS:    30,
	Mutating:    true,
}

func (m flowCommentModule) bind(e *moduleEnv)            { m.env = e }
func (m flowCommentModule) Descriptor() types.Descriptor { return flowCommentDescriptor }
func (m flowCommentModule) RollbackInvertible() bool     { return false }

func (m flowCommentModule) NormalizeArgs(args map[string]any) (map[string]any, error) {
	return args, nil
}

func (m flowCommentModule) ProtectedTargets(args map[string]any) []protectedTarget {
	return nil
}

// Check is the §3.9 dry run of the comment intent: `comment:<sig> 3 -> 4`. The
// probe carries the body-hash trigger, so a body already appended for this ref is
// Diff{Empty:true} — the module half of the `once` contract of §3.4.
func (m flowCommentModule) Check(ctx context.Context, args map[string]any) (types.Diff, error) {
	var a flowCommentArgs
	if err := decodeArgs(args, &a); err != nil {
		return types.Diff{}, err
	}
	trigger := flowCommentTrigger(a.Sig, a.Ref, a.Body)
	rep, err := m.call(ctx, a, trigger, true)
	if err != nil {
		return types.Diff{}, err
	}
	if !rep.Appended {
		return types.Diff{Empty: true, Summary: fmt.Sprintf(
			"comment:%s already carries this body (trigger %s, %d comment(s))", a.Sig, trigger, rep.Comments)}, nil
	}
	entry := flowCommentEntry(a.Sig, rep, false)
	return types.Diff{
		Empty:   false,
		Entries: []types.DiffEntry{entry},
		Summary: fmt.Sprintf("comment:%s %v -> %v", a.Sig, entry.Before, entry.After),
	}, nil
}

// Apply appends the comment (SPEC-09 Comment, deduped on the body-hash trigger).
// A replay of the same body for the same ref is answered Appended=false by the
// desk, so it changes nothing: Changed=true on a new body hash, false on a
// replay — the `once` contract of §3.9 in module terms.
func (m flowCommentModule) Apply(ctx context.Context, args map[string]any) (types.Result, error) {
	var a flowCommentArgs
	if err := decodeArgs(args, &a); err != nil {
		return types.Result{}, err
	}
	trigger := flowCommentTrigger(a.Sig, a.Ref, a.Body)
	rb := flowRollbackHint()
	rep, err := m.call(ctx, a, trigger, false)
	if err != nil {
		return types.Result{Output: flowFailureOutput(a.Sig, rep, err), Rollback: rb}, err
	}
	out := flowOutput(a.Sig, rep)
	out["trigger"] = trigger
	out["appended"] = rep.Appended
	out["comments"] = rep.Comments
	if !rep.Appended {
		return types.Result{Changed: false, Output: out, Rollback: rb}, nil
	}
	return types.Result{
		Changed:  true,
		Applied:  []types.DiffEntry{flowCommentEntry(a.Sig, rep, true)},
		Output:   out,
		Rollback: rb,
	}, nil
}

// Verify re-reads the comment count and re-probes the trigger. Verify's frozen
// signature carries no Result (SPEC-06 §2.1), so the count cannot be compared
// against the applied one; the durable proof is that the trigger is already on
// the ref — the desk answers Appended=false for it — and the re-read count is
// the evidence.
func (m flowCommentModule) Verify(ctx context.Context, args map[string]any) (types.VerifyResult, error) {
	var a flowCommentArgs
	if err := decodeArgs(args, &a); err != nil {
		return types.VerifyResult{}, err
	}
	trigger := flowCommentTrigger(a.Sig, a.Ref, a.Body)
	rep, err := m.call(ctx, a, trigger, true)
	if err != nil {
		return types.VerifyResult{}, err
	}
	detail := map[string]any{"sig": a.Sig, "ref": a.Ref, "trigger": trigger, "comments": rep.Comments, "appended": rep.Appended}
	switch {
	case rep.Appended:
		detail["ok_reason"] = "the driver reports the comment for this body absent: the append did not stick"
		return types.VerifyResult{OK: false, Method: "recheck", Detail: detail}, nil
	case rep.Comments < 1:
		detail["ok_reason"] = "the driver reports no comment on the ref"
		return types.VerifyResult{OK: false, Method: "recheck", Detail: detail}, nil
	}
	return types.VerifyResult{
		OK:       true,
		Method:   "recheck",
		Detail:   detail,
		Evidence: []types.DiffEntry{{Path: flowCommentPath(a.Sig), After: strconv.Itoa(rep.Comments)}},
	}, nil
}

func (m flowCommentModule) call(ctx context.Context, a flowCommentArgs, trigger string, dryRun bool) (flowReply, error) {
	env, err := flowEnv(m.env, flowCommentDescriptor.Name)
	if err != nil {
		return flowReply{}, err
	}
	in := map[string]any{"sig": a.Sig, "body": a.Body, "trigger": trigger}
	if a.Ref != "" {
		in["ref"] = a.Ref
	}
	if dryRun {
		in[flowDryRunKey] = true
	}
	return flowCall(ctx, flowCommentDescriptor.Name, env.deps.Comment, in)
}

// ---------------------------------------------------------------------------
// flow.* — the shared plumbing: state tokens, target paths and the reply
// ---------------------------------------------------------------------------

const (
	// flowDryRunKey asks the wired subsystem to answer without acting: Check is
	// stage 3 of the call contract and Verify must not mutate (§2.3). The
	// subsystem's own CLI spells the same flag --dry-run (SPEC-08 §2.2).
	flowDryRunKey = "dry_run"

	// flowAbsent is the `before` value of a target that does not exist yet
	// (§3.9: "absent").
	flowAbsent = "absent"

	// flowOpen is the issue state the file_issue intent is expressed in (§3.9).
	flowOpen = "open"

	// flowBoardTodo is the status a newly appended board row carries (§3.9's
	// `board:<sig> absent -> todo`; SPEC-08 §3.1's example row).
	flowBoardTodo = "todo"
)

func flowIssuePath(sig string) string   { return "issue:" + sig }
func flowBoardPath(sig string) string   { return "board:" + sig }
func flowCommentPath(sig string) string { return "comment:" + sig }

// flowIssueEntry is the one change flow.file_issue describes and reports (§3.9).
// The concrete issue number is not part of it — it does not exist before the
// filing call — and rides Result.Output.external_id instead; an issue that is
// already open is the fold case, where Check is empty and no entry is emitted.
func flowIssueEntry(sig string) types.DiffEntry {
	return types.DiffEntry{Path: flowIssuePath(sig), Before: flowAbsent, After: flowOpen}
}

// flowBoardEntry is the one change flow.create_task describes and reports
// (§3.9: `{path:"board:<sig>", before:"absent", after:"todo"}`).
func flowBoardEntry(sig string) types.DiffEntry {
	return types.DiffEntry{Path: flowBoardPath(sig), Before: flowAbsent, After: flowBoardTodo}
}

// flowCommentEntry is the one change flow.comment describes and reports (§3.9:
// `{path:"comment:<sig>", before:"3", after:"4"}`).
//
// A dry-run reply reports the count observed before the append it describes, an
// apply reply the count after the append it performed, so `applied` selects the
// side: Check predicts n -> n+1 and Apply reports n -> n+1 for the same append,
// which is what makes the two obligations of §2.4 agree.
func flowCommentEntry(sig string, rep flowReply, applied bool) types.DiffEntry {
	after := rep.Comments
	if applied {
		before := after - 1
		if before < 0 {
			before = 0
		}
		return types.DiffEntry{Path: flowCommentPath(sig), Before: strconv.Itoa(before), After: strconv.Itoa(after)}
	}
	return types.DiffEntry{Path: flowCommentPath(sig), Before: strconv.Itoa(after), After: strconv.Itoa(after + 1)}
}

// flowIssueState renders the issue's observed state the way §3.9 spells it —
// "open#42" — the bare state when the driver reports no id, and "absent" when
// there is nothing to render.
func flowIssueState(rep flowReply) string {
	state := rep.State
	if state == "" {
		if rep.ExternalID == "" {
			return flowAbsent
		}
		state = flowOpen
	}
	if rep.ExternalID == "" {
		return state
	}
	return state + "#" + rep.ExternalID
}

func flowStateOrUnknown(state string) string {
	if state == "" {
		return "unknown"
	}
	return state
}

// flowCommentTrigger is the body-hash replay key of flow.comment (§3.9: "once
// with a body-hash replay key"): the same sig, ref and body hash to the same
// trigger, and SPEC-09 §2.1's Comment appends once per distinct trigger.
func flowCommentTrigger(sig, ref, body string) string {
	sum := sha256.Sum256([]byte(body))
	return "flow.comment|" + sig + "|" + ref + "|" + hex.EncodeToString(sum[:])[:16]
}

// flowRollbackHint is the unsupported hint every flow apply carries (§3.5):
// append-only evidence is corrected by a comment or a close, never a delete, so
// the call is PONR and authorize a7 requires an explicit module-name grant.
func flowRollbackHint() *types.RollbackHint {
	return &types.RollbackHint{Supported: false, Args: map[string]any{}}
}

// flowReply is one wired-subsystem answer. Every field is read tolerantly
// (the subsystem lands separately and the map is the only interface) but the
// primary key of each is the one SPEC-08 §3.1 names.
type flowReply struct {
	Created    bool           // a new artefact was (dry run: would be) filed
	Appended   bool           // a comment was (dry run: would be) appended
	ExternalID string         // external_id | issue_id | task_id
	State      string         // state | status | row_status
	Comments   int            // the comment count at reply time
	ValidateRC int            // validate_rc; -1 when the driver reports none
	Upstream   string         // the driver's own code (upstreamCode)
	Raw        map[string]any // the raw reply, for the driver's remaining keys
}

func flowReplyOf(raw map[string]any) flowReply {
	rep := flowReply{Raw: raw, ValidateRC: -1}
	if raw == nil {
		return rep
	}
	rep.Upstream = upstreamCode(raw)
	rep.Created = flowBoolOf(raw, "created")
	rep.Appended = flowBoolOf(raw, "appended", "commented")
	rep.ExternalID = flowStringOf(raw, "external_id", "issue_id", "task_id")
	rep.State = flowStringOf(raw, "state", "status", "row_status")
	if n, ok := flowIntOf(raw, "comments", "comment_count"); ok {
		rep.Comments = n
	}
	if n, ok := flowIntOf(raw, "validate_rc"); ok {
		rep.ValidateRC = n
	}
	return rep
}

// failure maps a driver-reported failure onto the sentinel this area returns
// (§3.9): a reply reports a failure when it carries `ok: false` or its own code.
// The code is copied, never rewritten; only the class decides the sentinel.
func (r flowReply) failure(module string) error {
	failed := false
	if ok, has := r.Raw["ok"].(bool); has && !ok {
		failed = true
	}
	if r.Upstream != "" {
		failed = true
	}
	if !failed {
		return nil
	}
	if r.transient() {
		return fmt.Errorf("%w: %s: the driver reported a transient failure (upstream_code=%s)",
			types.ErrTransient, module, r.Upstream)
	}
	return fmt.Errorf("%w: %s: the driver reported a permanent failure (upstream_code=%s)",
		types.ErrPermanent, module, r.Upstream)
}

// transient reads the driver's own class off the reply.
func (r flowReply) transient() bool {
	for _, k := range []string{"retryable", "transient"} {
		if b, ok := r.Raw[k].(bool); ok && b {
			return true
		}
	}
	for _, k := range []string{"class", "error_class"} {
		if s, ok := r.Raw[k].(string); ok && s == string(types.ErrClassTransient) {
			return true
		}
	}
	return false
}

// flowEnv resolves a module's bound environment and refuses when the flow
// collaborators are not wired: RegistryDeps is the whole wiring surface (§2.2),
// so a module whose collaborator is absent is permanent — never a panic on a nil
// closure and never a silent success. environment() supplies the §4.3 defaults
// when the module has no bound env (the binding is a value receiver, so a
// registered module keeps the env it was built with).
func flowEnv(e *moduleEnv, module string) (*moduleEnv, error) {
	env := environment(e)
	if !env.depsAvailable() {
		return env, flowUnwired(module)
	}
	return env, nil
}

// flowUnwired is the refusal a flow module raises when RegistryDeps.FileIssue/
// CreateTask/Comment are not wired (§2.2): permanent, so the ladder escalates
// instead of reporting a filing that never happened.
func flowUnwired(module string) error {
	return fmt.Errorf("%w: %s: the flow collaborators are not wired (RegistryDeps.FileIssue/CreateTask/Comment, SPEC-06 §2.2); the call is refused rather than reported as performed",
		types.ErrPermanent, module)
}

// flowCall performs one wired-subsystem call: the context's own error first (the
// registry's effective timeout, §6.16), then the driver, then its reply.
func flowCall(ctx context.Context, module string, fn func(context.Context, map[string]any) (map[string]any, error), in map[string]any) (flowReply, error) {
	if err := ctx.Err(); err != nil {
		return flowReply{}, fmt.Errorf("%w: %s: %v", types.ErrTransient, module, err)
	}
	raw, err := fn(ctx, in)
	if err != nil {
		// The driver's own code is copied onto the reply — never into the error
		// chain, whose only job is to carry the class this area chose — so it
		// still rides Result.Output.upstream_code (§3.9, §5).
		return flowReply{Upstream: flowErrorCode(err)}, flowDriverError(module, err)
	}
	rep := flowReplyOf(raw)
	if ferr := rep.failure(module); ferr != nil {
		return rep, ferr
	}
	return rep, nil
}

// flowDriverError classifies a driver error (§3.9 flow row): transient →
// ErrTransient (TROUBLE-REGISTRY-004/010), anything else — including a driver's
// own policy refusal — → ErrPermanent (011). The driver's own code, when its
// error exposes one, is quoted in the message so it reaches the ledger's stage
// detail; this area never mints a registry code for it (§5).
func flowDriverError(module string, err error) error {
	where := module
	if code := flowErrorCode(err); code != "" {
		where = module + ": upstream " + code
	}
	if errors.Is(err, types.ErrTransient) {
		return fmt.Errorf("%w: %s: %v", types.ErrTransient, where, err)
	}
	return fmt.Errorf("%w: %s: %v", types.ErrPermanent, where, err)
}

// flowErrorCode copies a driver's own code off an error: the same
// `Code() types.ErrorCode` shape the ledger's append-failure code is read with.
func flowErrorCode(err error) string {
	type coder interface{ Code() types.ErrorCode }
	var c coder
	if errors.As(err, &c) {
		return string(c.Code())
	}
	return ""
}

// flowOutput is the Result.Output of a flow apply: the driver's own identifiers
// and its code when it reported one (§3.9), never a registry code.
func flowOutput(sig string, rep flowReply) map[string]any {
	out := map[string]any{"sig": sig}
	if rep.ExternalID != "" {
		out["external_id"] = rep.ExternalID
	}
	if rep.State != "" {
		out["state"] = rep.State
	}
	if rep.Upstream != "" {
		out["upstream_code"] = rep.Upstream
	}
	return out
}

// flowFailureOutput is the Result.Output of a failed flow call. Registry.Call is
// record-first and discards a module's Result when Apply returns an error, so the
// code also rides the error text (which the stage detail copies); it is filled
// here for every caller that reads the Result directly.
func flowFailureOutput(sig string, rep flowReply, err error) map[string]any {
	out := map[string]any{"ok": false, "sig": sig}
	code := rep.Upstream
	if code == "" {
		code = flowErrorCode(err)
	}
	if code != "" {
		out["upstream_code"] = code
	}
	if rep.ExternalID != "" {
		out["external_id"] = rep.ExternalID
	}
	return out
}

func flowStringOf(raw map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := raw[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func flowBoolOf(raw map[string]any, keys ...string) bool {
	for _, k := range keys {
		if b, ok := raw[k].(bool); ok {
			return b
		}
	}
	return false
}

func flowIntOf(raw map[string]any, keys ...string) (int, bool) {
	for _, k := range keys {
		switch v := raw[k].(type) {
		case int:
			return v, true
		case int64:
			return int(v), true
		case float64:
			return int(v), true
		case json.Number:
			if n, err := v.Int64(); err == nil {
				return int(n), true
			}
		}
	}
	return 0, false
}
