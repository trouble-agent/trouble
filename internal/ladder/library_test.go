package ladder

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// library_test.go is SPEC-05 §7's battery for §2b: the ladder reads the local skill
// library and runs one typed step against the runner under the play gate.

// fakeSkills is a scripted SkillLibrary.
type fakeSkills struct {
	mu       sync.Mutex
	plays    []types.Play
	playsErr error
	stepErr  error
	calls    []stepCall
}

type stepCall struct {
	Name string
	Step int
	Mode string
}

func (f *fakeSkills) Plays(ctx context.Context) ([]types.Play, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.plays, f.playsErr
}

func (f *fakeSkills) RunStep(ctx context.Context, inc types.Incident, name string, step int, mode string) (types.ToolCall, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, stepCall{Name: name, Step: step, Mode: mode})
	if f.stepErr != nil {
		return types.ToolCall{}, f.stepErr
	}
	return types.ToolCall{ID: "tc_" + name, Module: "service.reload", Mode: mode, Args: map[string]any{"unit": "payment-worker.service"}}, nil
}

func (f *fakeSkills) stepCalls() []stepCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]stepCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// libraryPlay is the compile of two steps: one read-only, one mutating.
func libraryPlay() types.Play {
	return types.Play{
		Name:    "payment-worker-queue-wedge",
		Version: 1,
		Source:  "skill-local:payment-worker-queue-wedge@1",
		Tasks: []types.PlayTask{
			{Name: "count the sockets", Tool: "proc.connections", Args: map[string]any{"pid": 4242}},
			{Name: "reload the unit", Tool: "service.reload", Args: map[string]any{"unit": "payment-worker.service"}},
		},
	}
}

func TestSkillPlays_ReadSurface(t *testing.T) {
	port := &fakeSkills{plays: []types.Play{libraryPlay()}}
	h := newHarness(t, harnessOpts{skills: port, rules: agentRule("rule-agent")})
	got, err := h.l.SkillPlays(context.Background())
	if err != nil {
		t.Fatalf("SkillPlays: %v", err)
	}
	if len(got) != 1 || got[0].Name != "payment-worker-queue-wedge" || len(got[0].Tasks) != 2 {
		t.Fatalf("plays = %+v, want the library's compiled play", got)
	}
	if got[0].Source != "skill-local:payment-worker-queue-wedge@1" {
		t.Errorf("source = %q, want the skill-local marker", got[0].Source)
	}

	// No library wired is not a failure: the stage simply has nothing to run.
	bare := newHarness(t, harnessOpts{})
	empty, err := bare.l.SkillPlays(context.Background())
	if err != nil {
		t.Fatalf("SkillPlays with no library: %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Errorf("plays = %#v, want an empty non-nil list", empty)
	}
}

func TestRunSkillStep_ShadowForcesCheckMode(t *testing.T) {
	port := &fakeSkills{plays: []types.Play{libraryPlay()}}
	h := newHarness(t, harnessOpts{skills: port, rules: agentRule("rule-agent")})
	inc := agentRunning(t, h, "lib-shadow")

	tc, err := h.l.RunSkillStep(context.Background(), inc, "payment-worker-queue-wedge", 1)
	if err != nil {
		t.Fatalf("RunSkillStep: %v", err)
	}
	if tc.Mode != types.ModeCheck {
		t.Errorf("tool call mode = %q, want check_mode under the shadow default", tc.Mode)
	}
	calls := port.stepCalls()
	if len(calls) != 1 || calls[0].Mode != types.ModeCheck {
		t.Fatalf("runner calls = %+v, want exactly one check-mode step", calls)
	}
	if calls[0].Name != "payment-worker-queue-wedge" || calls[0].Step != 1 {
		t.Errorf("runner call = %+v, want the named step", calls[0])
	}
}

func TestRunSkillStep_AssistedNeedsTheModuleGrant(t *testing.T) {
	port := &fakeSkills{plays: []types.Play{libraryPlay()}}
	h := newHarness(t, harnessOpts{skills: port, rules: agentRule("rule-agent")})
	inc := agentRunning(t, h, "lib-assisted")
	gates := types.AutonomyGates{Mode: types.AutoAssisted, AllowPlayMutate: true, AllowAgent: true}
	if _, err := h.l.SetGates(context.Background(), gates, types.Actor{Kind: types.ActorHuman, ID: "op"}); err != nil {
		t.Fatalf("SetGates: %v", err)
	}

	// A mutating step with no grant is refused and the runner is never called.
	_, err := h.l.RunSkillStep(context.Background(), inc, "payment-worker-queue-wedge", 1)
	if got := CodeOf(err); got != types.CodeLadder011 {
		t.Fatalf("code = %q, want TROUBLE-LADDER-011 (err=%v)", got, err)
	}
	if len(port.stepCalls()) != 0 {
		t.Errorf("runner calls = %+v, want none for an ungranted mutating step", port.stepCalls())
	}
	if p := h.incidentPayload(); p["reason"] != reasonGateDenied || p["module"] != "service.reload" {
		t.Errorf("refusal payload = %#v, want a refused row naming the module", p)
	}

	// A read-only step needs no grant in assisted mode.
	if _, err := h.l.RunSkillStep(context.Background(), inc, "payment-worker-queue-wedge", 0); err != nil {
		t.Fatalf("read-only step under assisted: %v", err)
	}
	// With the module grant the mutating step applies.
	gates.Grants = []string{"rule-agent|service.reload"}
	if _, err := h.l.SetGates(context.Background(), gates, types.Actor{Kind: types.ActorHuman, ID: "op"}); err != nil {
		t.Fatalf("SetGates: %v", err)
	}
	if _, err := h.l.RunSkillStep(context.Background(), inc, "payment-worker-queue-wedge", 1); err != nil {
		t.Fatalf("granted mutating step: %v", err)
	}
	calls := port.stepCalls()
	if len(calls) != 2 {
		t.Fatalf("runner calls = %+v, want the read-only step then the granted one", calls)
	}
	if calls[0].Mode != types.ModeApply || calls[1].Mode != types.ModeApply {
		t.Errorf("assisted modes = %q/%q, want apply for both (the registry re-checks scope)", calls[0].Mode, calls[1].Mode)
	}
}

func TestRunSkillStep_KillSwitchRefusesAndRecordsPending(t *testing.T) {
	port := &fakeSkills{plays: []types.Play{libraryPlay()}}
	h := newHarness(t, harnessOpts{skills: port, rules: agentRule("rule-agent")})
	inc := agentRunning(t, h, "lib-kill")
	if _, err := h.l.SetKillSwitch(context.Background(), true, types.Actor{Kind: types.ActorHuman, ID: "op"}); err != nil {
		t.Fatalf("SetKillSwitch: %v", err)
	}
	_, err := h.l.RunSkillStep(context.Background(), inc, "payment-worker-queue-wedge", 0)
	if got := CodeOf(err); got != types.CodeLadder010 {
		t.Fatalf("code = %q, want TROUBLE-LADDER-010 (err=%v)", got, err)
	}
	if len(port.stepCalls()) != 0 {
		t.Errorf("runner calls = %+v, want none under the kill-switch", port.stepCalls())
	}
	p := h.incidentPayload()
	if p["refused"] != true || p["pending"] != true || p["stage"] != "skills" {
		t.Errorf("refusal payload = %#v, want a pending skills-stage row", p)
	}
}

func TestRunSkillStep_UnknownStepAndMissingLibraryRefuse(t *testing.T) {
	port := &fakeSkills{plays: []types.Play{libraryPlay()}}
	h := newHarness(t, harnessOpts{skills: port, rules: agentRule("rule-agent")})
	inc := agentRunning(t, h, "lib-unknown")

	_, err := h.l.RunSkillStep(context.Background(), inc, "no-such-skill", 0)
	if got := CodeOf(err); got != types.CodeLadder011 || !strings.Contains(err.Error(), reasonUnknownStep) {
		t.Fatalf("unknown skill: code=%q err=%v, want TROUBLE-LADDER-011 with %s", got, err, reasonUnknownStep)
	}
	_, err = h.l.RunSkillStep(context.Background(), inc, "payment-worker-queue-wedge", 9)
	if got := CodeOf(err); got != types.CodeLadder011 {
		t.Fatalf("out-of-range step: code = %q, want TROUBLE-LADDER-011", got)
	}
	if len(port.stepCalls()) != 0 {
		t.Errorf("runner calls = %+v, want none", port.stepCalls())
	}

	bare := newHarness(t, harnessOpts{rules: agentRule("rule-agent")})
	bareInc := agentRunning(t, bare, "lib-none")
	if _, err := bare.l.RunSkillStep(context.Background(), bareInc, "x", 0); CodeOf(err) != types.CodeLadder011 {
		t.Errorf("no library: code = %q, want TROUBLE-LADDER-011", CodeOf(err))
	}
	if _, err := h.l.RunSkillStep(context.Background(), "inc_missing", "x", 0); CodeOf(err) != types.CodeLadder012 {
		t.Errorf("unknown incident: code = %q, want TROUBLE-LADDER-012", CodeOf(err))
	}
}
