package registry

// modules_flow.go — flow.file_issue, flow.create_task, flow.comment (SPEC-06 §3.8, §3.9).
//
// IMPLEMENT: the descriptors, args structs, env binding and target declarations
// below are frozen; fill in NormalizeArgs, Check, Apply and Verify per the spec
// row of each module, and delete the ErrPermanent TODO bodies.

import (
	"context"

	"github.com/totalwindupflightsystems/trouble/internal/registry/schemagen"
	"github.com/totalwindupflightsystems/trouble/internal/types"
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

// IMPLEMENT: the spec §3.9 row of flow.file_issue.
func (m flowFileIssueModule) Check(ctx context.Context, args map[string]any) (types.Diff, error) {
	return types.Diff{}, types.ErrPermanent
}

func (m flowFileIssueModule) Apply(ctx context.Context, args map[string]any) (types.Result, error) {
	return types.Result{}, types.ErrPermanent
}

func (m flowFileIssueModule) Verify(ctx context.Context, args map[string]any) (types.VerifyResult, error) {
	return types.VerifyResult{}, types.ErrPermanent
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

// IMPLEMENT: the spec §3.9 row of flow.create_task.
func (m flowCreateTaskModule) Check(ctx context.Context, args map[string]any) (types.Diff, error) {
	return types.Diff{}, types.ErrPermanent
}

func (m flowCreateTaskModule) Apply(ctx context.Context, args map[string]any) (types.Result, error) {
	return types.Result{}, types.ErrPermanent
}

func (m flowCreateTaskModule) Verify(ctx context.Context, args map[string]any) (types.VerifyResult, error) {
	return types.VerifyResult{}, types.ErrPermanent
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

// IMPLEMENT: the spec §3.9 row of flow.comment.
func (m flowCommentModule) Check(ctx context.Context, args map[string]any) (types.Diff, error) {
	return types.Diff{}, types.ErrPermanent
}

func (m flowCommentModule) Apply(ctx context.Context, args map[string]any) (types.Result, error) {
	return types.Result{}, types.ErrPermanent
}

func (m flowCommentModule) Verify(ctx context.Context, args map[string]any) (types.VerifyResult, error) {
	return types.VerifyResult{}, types.ErrPermanent
}
