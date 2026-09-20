package ladder

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// gates_test.go is the SPEC-05 §7 row for AC-26: the autonomy gate matrix (§3.11),
// the grant mechanism and the kill-switch (§3.4). The matrix is asserted
// cell-by-cell on this package's surface: the mode the ladder hands the play
// runner, the gate state it reports, and the grant answers it gives.

// modePlay wraps the shared fake PlayRunner and records the mode of every Run.
// The harness's fake records the call, not the mode, and §3.11's play-mutate row
// is exactly a question about the mode: shadow must never ask for "apply".
type modePlay struct {
	inner *fakePlay
	t     *testing.T
	// forbidApply fails the test if Apply is entered (the park row: an
	// already-applied mutating call is never re-applied, INV-4).
	forbidApply bool

	mu      sync.Mutex
	modes   []string
	runs    int
	checks  int
	applies int
}

func (m *modePlay) Run(ctx context.Context, inc types.Incident, p types.Play, mode string) (RunSummary, error) {
	m.mu.Lock()
	m.runs++
	m.modes = append(m.modes, mode)
	m.mu.Unlock()
	return m.inner.Run(ctx, inc, p, mode)
}

func (m *modePlay) Check(ctx context.Context, tool string, args map[string]any) (types.Diff, types.ToolCall, error) {
	m.mu.Lock()
	m.checks++
	m.mu.Unlock()
	return m.inner.Check(ctx, tool, args)
}

func (m *modePlay) Apply(ctx context.Context, tool string, args map[string]any) (types.Result, types.ToolCall, error) {
	m.mu.Lock()
	m.applies++
	forbid := m.forbidApply
	m.mu.Unlock()
	if forbid && m.t != nil {
		m.t.Errorf("Apply(%s) was entered: an already-applied call is never re-applied (INV-4, SPEC-05 §3.5)", tool)
	}
	return m.inner.Apply(ctx, tool, args)
}

func (m *modePlay) Rollback(ctx context.Context, t types.ToolCall) error {
	return m.inner.Rollback(ctx, t)
}

// modeList returns the mode of every Run, in order.
func (m *modePlay) modeList() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.modes...)
}

// withRegistry installs a mode-recording runner as the ladder's PlayRunner.
func withRegistry(h *harness, mp *modePlay) { h.l.deps.Registry = mp }

// gateForMode is the operator state a §3.11 mode comes with: shadow grants
// nothing, assisted carries the operator's grants, full carries the §3.11 flags.
func gateForMode(mode types.AutonomyMode, sig string) types.AutonomyGates {
	short := sig
	if len(short) > 16 {
		short = short[:16]
	}
	g := types.AutonomyGates{Mode: mode, AllowDetect: true, AllowResearch: true, AllowAgent: true}
	switch mode {
	case types.AutoAssisted:
		g.Grants = []string{
			"rule-a|service.reload",
			"rule-a|service.reload|" + short,
			"spawn:repo", "merge:repo", "promote:" + short, "skill-accept:name",
		}
		g.AllowSpawn, g.AllowMerge, g.AllowPromote, g.AllowSkillAccept = true, true, true, true
	case types.AutoFull:
		g.AllowPlayMutate = true
	}
	return g
}

// TestAutonomyGateMatrixCells asserts every one of the 30 stage × mode cells the
// ladder's surface can decide (§3.11). "grant" means the cell is allowed only
// through a grant entry; the four outlet stages (spawn, merge, promote,
// skill-accept) carry no code path in this package — SPEC-08 spawns, SPEC-10
// merges by policy and SPEC-11 accepts skills, each reading the flag asserted
// here — so their cells are read from the gate state the ladder reports.
func TestAutonomyGateMatrixCells(t *testing.T) {
	ctx := context.Background()
	cells := []struct{ stage, shadow, assisted, full string }{
		{"detect", "allow", "allow", "allow"},
		{"record", "allow", "allow", "allow"},
		{"research", "allow", "allow", "allow"},
		{"play-check", "allow", "allow", "allow"},
		{"play-mutate", "deny", "grant", "allow"},
		{"agent", "allow", "allow", "allow"},
		{"spawn", "deny", "grant", "allow"},
		{"merge", "deny", "grant", "allow"},
		{"promote", "deny", "grant", "allow"},
		{"skill-accept", "deny", "grant", "allow"},
	}
	if len(cells) != 10 {
		t.Fatalf("§3.11 declares ten stages, the table holds %d", len(cells))
	}
	sig := sigFor("matrix-grant").String()
	for _, mode := range []types.AutonomyMode{types.AutoShadow, types.AutoAssisted, types.AutoFull} {
		mode := mode
		t.Run(string(mode), func(t *testing.T) {
			h := newHarness(t, harnessOpts{
				research: true,
				rules: map[string]types.Rule{
					"rule-a":        {Name: "rule-a", EntryRung: types.RungPlay},
					"rule-agent":    {Name: "rule-agent", EntryRung: types.RungAgent},
					"rule-research": {Name: "rule-research", EntryRung: types.RungResearch},
				},
				playFor: map[string]types.Play{"rule-a": {
					Name: "reload", Version: 1, CheckMode: true,
					Tasks: []types.PlayTask{{Name: "restart", Tool: "service.reload"}},
				}},
				cfg: Config{PlayRunsPerDay: 10, AgentRunsPerDay: 10, ResearchPerDay: 10},
			})
			mp := &modePlay{inner: h.play, t: t}
			withRegistry(h, mp)
			if _, err := h.l.SetGates(ctx, gateForMode(mode, sig), types.Actor{Kind: types.ActorHuman, ID: "opuser"}); err != nil {
				t.Fatalf("SetGates(%s): %v", mode, err)
			}
			if got := h.l.Gates().Mode; got != mode {
				t.Fatalf("the ladder reports mode %q, want %q", got, mode)
			}
			for _, c := range cells {
				want := c.shadow
				switch mode {
				case types.AutoAssisted:
					want = c.assisted
				case types.AutoFull:
					want = c.full
				}
				if got := observeCell(t, h, mp, mode, c.stage); got != want {
					t.Errorf("cell %s × %s = %q, want %q (SPEC-05 §3.11)", c.stage, mode, got, want)
				}
			}
			// The ladder never mints a play-mutate authority in shadow: the row
			// is the mode it asks the runner for.
			if mode == types.AutoShadow {
				for _, got := range mp.modeList() {
					if got == types.ModeApply {
						t.Errorf("shadow asked the play runner for %q: shadow is check_mode only", types.ModeApply)
					}
				}
				if n := h.play.applyCalls; n != 0 {
					t.Errorf("%d apply call(s) in shadow, want 0", n)
				}
			}
		})
	}
}

// observeCell reports the ladder's decision for one §3.11 cell using only this
// package's surface.
func observeCell(t *testing.T, h *harness, mp *modePlay, mode types.AutonomyMode, stage string) string {
	t.Helper()
	ctx := context.Background()
	g := h.l.Gates()
	switch stage {
	case "detect":
		if !g.AllowDetect {
			return "deny"
		}
		inc := admitFresh(t, h, "matrix-detect-"+string(mode), "rule-a")
		got, err := h.l.Incident(ctx, inc)
		if err != nil || got.State != types.StRecorded {
			return "deny"
		}
		return "allow"
	case "record":
		// record is never gated: evidence is not work.
		before := countTransition(h, "T01")
		admitFresh(t, h, "matrix-record-"+string(mode), "rule-a")
		if countTransition(h, "T01") != before+1 {
			return "deny"
		}
		return "allow"
	case "research":
		if !g.AllowResearch {
			return "deny"
		}
		inc := admitFresh(t, h, "matrix-research-"+string(mode), "rule-research")
		if _, err := h.l.Advance(ctx, inc, Transition{Trigger: "T04"}); err != nil {
			return "deny"
		}
		got, err := h.l.Incident(ctx, inc)
		if err != nil || got.State != types.StResRequested {
			return "deny"
		}
		return "allow"
	case "play-check":
		inc := admitFresh(t, h, "matrix-check-"+string(mode), "rule-a")
		if _, err := h.l.Advance(ctx, inc, Transition{Trigger: "T03"}); err != nil {
			return "deny"
		}
		if _, err := h.l.Advance(ctx, inc, Transition{Trigger: "T08"}); err != nil {
			return "deny"
		}
		got, err := h.l.Incident(ctx, inc)
		if err != nil || got.State != types.StPlayCheck {
			return "deny"
		}
		return "allow"
	case "play-mutate":
		name := "matrix-mutate-" + string(mode)
		inc := admitFresh(t, h, name, "rule-a")
		if _, err := h.l.Advance(ctx, inc, Transition{Trigger: "T03"}); err != nil {
			return "deny"
		}
		if _, err := h.l.Advance(ctx, inc, Transition{Trigger: "T08"}); err != nil {
			return "deny"
		}
		if modes := mp.modeList(); len(modes) > 0 && modes[len(modes)-1] == types.ModeApply {
			return "allow"
		}
		if h.l.Granted(ctx, "rule-a", "service.reload", sigFor(name).String()) {
			return "grant"
		}
		return "deny"
	case "agent":
		if !g.AllowAgent {
			return "deny"
		}
		inc := admitFresh(t, h, "matrix-agent-"+string(mode), "rule-agent")
		if _, err := h.l.Advance(ctx, inc, Transition{Trigger: "T05"}); err != nil {
			return "deny"
		}
		got, err := h.l.Incident(ctx, inc)
		if err != nil || got.State != types.StAgentRunning {
			return "deny"
		}
		return "allow"
	}
	return observeOutletStage(g, stage)
}

// countTransition counts the ledger records carrying a transition id.
func countTransition(h *harness, id string) int {
	h.ledger.mu.Lock()
	defer h.ledger.mu.Unlock()
	n := 0
	for _, r := range h.ledger.records {
		if fmt.Sprint(r.Payload["transition"]) == id {
			n++
		}
	}
	return n
}

// observeOutletStage reports the spawn / merge / promote / skill-accept cells
// from the gate state the ladder carries. §3.11's "grant only" column is
// expressed by the flag plus an operator grant token; in full mode this package
// denies nothing by mode and the SPEC-08/SPEC-10/SPEC-11 gates (allowed_repos,
// merge_allow_globs, signer, canary) still bind.
func observeOutletStage(g types.AutonomyGates, stage string) string {
	flag := false
	switch stage {
	case "spawn":
		flag = g.AllowSpawn
	case "merge":
		flag = g.AllowMerge
	case "promote":
		flag = g.AllowPromote
	case "skill-accept":
		flag = g.AllowSkillAccept
	}
	switch g.Mode {
	case types.AutoShadow:
		if flag {
			return "allow"
		}
		return "deny"
	case types.AutoAssisted:
		if flag && hasGrantToken(g.Grants, stage) {
			return "grant"
		}
		return "deny"
	}
	return "allow"
}

// hasGrantToken reports whether the operator carried the stage's grant token.
func hasGrantToken(grants []string, stage string) bool {
	for _, g := range grants {
		if strings.HasPrefix(g, stage+":") {
			return true
		}
	}
	return false
}

// TestDefaultGatesAreShadowAndDenyTheOutletStages pins the boot state (§3.11:
// shadow is the default in every install): the ladder never self-grants the four
// outlet stages, and play mutation and the kill-switch are off.
func TestDefaultGatesAreShadowAndDenyTheOutletStages(t *testing.T) {
	h := newHarness(t, harnessOpts{rules: map[string]types.Rule{"rule-a": {Name: "rule-a", EntryRung: types.RungPlay}}})
	g := h.l.Gates()
	if g.Mode != types.AutoShadow {
		t.Errorf("the default mode is %q, want shadow (non-negotiable #11)", g.Mode)
	}
	if g.KillSwitch {
		t.Error("the default kill-switch is set, want clear")
	}
	if !g.AllowDetect || !g.AllowResearch || !g.AllowAgent {
		t.Errorf("detect/research/agent must be allowed by default: %+v", g)
	}
	if g.AllowPlayMutate || g.AllowSpawn || g.AllowMerge || g.AllowPromote || g.AllowSkillAccept {
		t.Errorf("a default gate grants a mutation stage: %+v", g)
	}
	if len(g.Grants) != 0 {
		t.Errorf("the default gate carries grants %v, want none", g.Grants)
	}
	if mg := types.DefaultGates(); mg.Mode != types.AutoShadow || len(mg.Grants) != 0 {
		t.Errorf("types.DefaultGates is %+v, want shadow with no grants", mg)
	}
}

// TestAssistedGrantAuthorizesOnlyTheGrantedModule covers §3.11's grant mechanism:
// a grant for `rule-a|service.reload` makes Granted() true for that module and
// only that module, the whole entry must match exactly (no prefix or glob
// semantics) and the most specific entry is the one GrantFor reports.
func TestAssistedGrantAuthorizesOnlyTheGrantedModule(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, harnessOpts{
		rules:   map[string]types.Rule{"rule-a": {Name: "rule-a", EntryRung: types.RungPlay}},
		playFor: map[string]types.Play{"rule-a": {Name: "reload", Version: 1, CheckMode: true}},
	})
	sig := sigFor("granted").String()
	// A sig-scoped entry is matched against the incident's sig truncated to 16
	// characters (§3.11's `|<sig-short>`); every sig this package mints shares its
	// first 16 characters, so that row is asserted with sigs short enough not to
	// be truncated.
	shortA, shortB := "sigscoped-a", "sigscoped-b"
	actor := types.Actor{Kind: types.ActorHuman, ID: "opuser"}
	if _, err := h.l.SetGates(ctx, types.AutonomyGates{
		Mode:          types.AutoAssisted,
		AllowDetect:   true,
		AllowResearch: true,
		AllowAgent:    true,
		Grants: []string{
			"rule-a|service.reload",
			"rule-b|service.reload",
			"rule-c|service.reload|" + shortA,
		},
	}, actor); err != nil {
		t.Fatalf("SetGates: %v", err)
	}

	if !h.l.Granted(ctx, "rule-a", "service.reload", sig) {
		t.Error("the granted (rule, module) pair must be authorized")
	}
	if h.l.Granted(ctx, "rule-a", "service.delete", sig) {
		t.Error("an ungranted module must not be authorized")
	}
	if h.l.Granted(ctx, "rule-d", "service.reload", sig) {
		t.Error("an ungranted rule must not be authorized")
	}
	// No prefix or glob semantics: the whole entry matches exactly.
	if h.l.Granted(ctx, "rule-a", "service.rel", sig) {
		t.Error("a prefix of a granted module must not be authorized")
	}
	if h.l.Granted(ctx, "rule", "service.reload", sig) {
		t.Error("a prefix of a granted rule must not be authorized")
	}
	if h.l.Granted(ctx, "rule-a", "service.reloadx", sig) {
		t.Error("a granted module plus a suffix must not be authorized")
	}
	// A sig-scoped entry authorizes its own sig and nothing else.
	if !h.l.Granted(ctx, "rule-c", "service.reload", shortA) {
		t.Error("the sig-scoped entry must authorize its own sig")
	}
	if h.l.Granted(ctx, "rule-c", "service.reload", shortB) {
		t.Error("a sig-scoped entry must not authorize another sig")
	}
	if h.l.Granted(ctx, "rule-c", "service.reload", sig) {
		t.Error("a sig-scoped entry must not authorize a truncated sig that differs from its own")
	}
	// The most specific entry wins.
	if got := h.l.GrantFor(ctx, "rule-c", "service.reload", shortA); got != "rule-c|service.reload|"+shortA {
		t.Errorf("GrantFor = %q, want the sig-scoped entry", got)
	}
	if got := h.l.GrantFor(ctx, "rule-a", "service.reload", sig); got != "rule-a|service.reload" {
		t.Errorf("GrantFor = %q, want the only entry that matches", got)
	}
	if got := h.l.GrantFor(ctx, "rule-d", "service.reload", sig); got != "" {
		t.Errorf("GrantFor = %q, want no grant for an unauthorized module", got)
	}
	// An assisted gate is an exact-match table: a mutating call is authorized
	// only through a grant entry.
	for _, module := range []string{"service.reload", "config.set", "file.write", "flow.restart"} {
		if h.l.Granted(ctx, "rule-d", module, sig) {
			t.Errorf("the mutating module %s must need a grant", module)
		}
	}
	// The grant is the authority the registry receives; the mode the ladder hands
	// the runner is its own flag (§3.11: AllowPlayMutate is true when the grant
	// exists — the ladder does not derive it from Grants).
	mp := &modePlay{inner: h.play, t: t}
	withRegistry(h, mp)
	inc := admitFresh(t, h, "granted-run", "rule-a")
	if _, err := h.l.Advance(ctx, inc, Transition{Trigger: "T03"}); err != nil {
		t.Fatalf("T03: %v", err)
	}
	if _, err := h.l.Advance(ctx, inc, Transition{Trigger: "T08"}); err != nil {
		t.Fatalf("T08: %v", err)
	}
	if modes := mp.modeList(); len(modes) != 1 || modes[0] != types.ModeCheck {
		t.Errorf("the runner modes are %v, want one check_mode", modes)
	}

	// Outside assisted the same question is answered by the module's mutability:
	// shadow denies play-mutate and authorizes the read-only modules, full
	// authorizes everything.
	if _, err := h.l.SetGates(ctx, types.AutonomyGates{Mode: types.AutoShadow, AllowDetect: true, AllowResearch: true, AllowAgent: true}, actor); err != nil {
		t.Fatalf("SetGates(shadow): %v", err)
	}
	for _, module := range []string{"config.get", "config.list", "service.status", "file.read", "proc.status"} {
		if !h.l.Granted(ctx, "rule-d", module, sig) {
			t.Errorf("shadow must authorize the read-only module %s", module)
		}
	}
	for _, module := range []string{"service.reload", "config.set", "file.write", "flow.restart"} {
		if h.l.Granted(ctx, "rule-d", module, sig) {
			t.Errorf("shadow must deny the mutating module %s", module)
		}
	}
	if _, err := h.l.SetGates(ctx, types.AutonomyGates{Mode: types.AutoFull}, actor); err != nil {
		t.Fatalf("SetGates(full): %v", err)
	}
	for _, module := range []string{"service.reload", "config.set", "file.write", "proc.status"} {
		if !h.l.Granted(ctx, "rule-d", module, sig) {
			t.Errorf("full mode must authorize %s without a grant entry", module)
		}
	}
}

// TestKillSwitchRefusesStageEntriesAndResumesPending is the §7 AC-26 row for
// §3.4: while the switch is set every stage entry is refused with
// TROUBLE-LADDER-010 and recorded as pending, no tool call runs and no state
// moves; observation (verifying → resolved) still works; clearing the switch lets
// ResumePending continue from resume_from with no replayed call.
func TestKillSwitchRefusesStageEntriesAndResumesPending(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, harnessOpts{
		rules: map[string]types.Rule{
			"rule-a":     {Name: "rule-a", EntryRung: types.RungPlay},
			"rule-agent": {Name: "rule-agent", EntryRung: types.RungAgent},
		},
		playFor: map[string]types.Play{"rule-a": {
			Name: "reload", Version: 1, CheckMode: true,
			Tasks: []types.PlayTask{{Name: "restart", Tool: "service.reload"}},
		}},
	})
	mp := &modePlay{inner: h.play, t: t}
	withRegistry(h, mp)

	// Three incidents, each parked in front of a stage entry. They are registered
	// through the boot seam so they carry distinct ids (see lease_test.go).
	check := adoptIncident(t, h, "kill-check", "rule-a", types.StRecorded, types.RungPlay)
	if _, err := h.l.Advance(ctx, check, Transition{Trigger: "T03"}); err != nil {
		t.Fatalf("T03: %v", err)
	}
	retry := adoptIncident(t, h, "kill-retry", "rule-a", types.StPlayFailed, types.RungPlay)
	retrySt := h.l.incStateFor(retry)
	retrySt.RunSummary = &RunSummary{FailClass: "transient"} // T13 is the stage entry in front of it
	verify := adoptIncident(t, h, "kill-verify", "rule-a", types.StRecorded, types.RungPlay)
	verifySt := driveToVerifying(t, h, verify)
	// The observation incident ran its play before the switch: the refusals must
	// add no further call.
	runsBefore, _, appliesBefore, _ := h.play.counts()
	wrapperRunsBefore := len(mp.modeList())

	actor := types.Actor{Kind: types.ActorHuman, ID: "opuser"}
	if _, err := h.l.SetKillSwitch(ctx, true, actor); err != nil {
		t.Fatalf("SetKillSwitch(true): %v", err)
	}
	if !h.l.Gates().KillSwitch {
		t.Fatal("the kill-switch is not set")
	}

	attempts := []struct {
		inc    string
		trig   string
		from   types.LadderState
		to     types.LadderState
		repeat int
	}{
		{inc: check, trig: "T08", from: types.StPlayDrafted, to: types.StPlayCheck, repeat: 2},
		{inc: retry, trig: "T13", from: types.StPlayFailed, to: types.StPlayDrafted, repeat: 1},
	}
	expected := 0
	for _, a := range attempts {
		for i := 0; i < a.repeat; i++ {
			expected++
			got, err := h.l.Advance(ctx, a.inc, Transition{Trigger: a.trig})
			if err == nil {
				t.Fatalf("%s attempt %d returned no error while the kill-switch is set", a.trig, i+1)
			}
			if code := CodeOf(err); code != types.CodeLadder010 {
				t.Errorf("%s attempt %d: code %q, want TROUBLE-LADDER-010", a.trig, i+1, code)
			}
			if cls := ClassOf(err); cls != types.ErrClassPermanent {
				t.Errorf("%s attempt %d: class %q, want permanent", a.trig, i+1, cls)
			}
			if got.State != a.from {
				t.Errorf("%s attempt %d moved the state to %q, want %q (a refusal never mutates state)", a.trig, i+1, string(got.State), string(a.from))
			}
		}
	}
	if n := killSwitchRefusals(h); n != expected {
		t.Errorf("%d kill-switch refusal record(s), want %d (one per attempted stage entry)", n, expected)
	}
	if after, _, _, _ := h.play.counts(); after != runsBefore {
		t.Errorf("%d run(s) reached the play runner while the kill-switch is set, want 0", after-runsBefore)
	}
	if len(mp.modeList()) != wrapperRunsBefore {
		t.Errorf("the runner modes grew to %v while the kill-switch is set, want no new call", mp.modeList())
	}
	if appliesBefore != 0 || mp.applies != 0 || h.play.applyCalls != 0 {
		t.Errorf("%d apply call(s) so far, want 0 while the kill-switch is set", mp.applies)
	}

	// Observation is not work: verifying → resolved stays legal (§3.4.4).
	ev := passingEvidence(verifySt)
	h.canaryIn(t, verifySt.Inc.Sig)
	h.index.events[verifySt.Inc.Sig] = 0
	verifySt.Evidence = &ev
	if _, err := h.l.Advance(ctx, verify, Transition{Trigger: "T38", Evidence: &ev}); err != nil {
		t.Errorf("verifying → resolved while the kill-switch is set: %v (the window is observation, not work)", err)
	}
	if st := h.l.incStateFor(verify); st.Inc.State != types.StResolved {
		t.Errorf("the observation incident is %q, want resolved", string(st.Inc.State))
	}

	// The pending set lists the refusals with their resume point.
	items, err := h.l.Pending(ctx)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("Pending returned %d item(s) %v, want 2", len(items), items)
	}
	byInc := map[string]PendingItem{}
	for _, it := range items {
		byInc[it.Inc] = it
	}
	it, ok := byInc[check]
	if !ok {
		t.Fatalf("Pending does not list the refused stage entry for %s", check)
	}
	if it.From != types.StPlayDrafted || it.To != types.StPlayCheck || it.Reason != reasonKillSwitch || it.SinceTS == "" {
		t.Errorf("pending item %+v, want from play:drafted to play:check_only reason %q", it, reasonKillSwitch)
	}
	if it, ok := byInc[retry]; !ok || it.From != types.StPlayFailed || it.To != types.StPlayDrafted {
		t.Errorf("pending item for the retry is %+v (%t), want play:failed → play:drafted", it, ok)
	}

	// Clearing the switch lets the pending work continue with no replayed call.
	if _, err := h.l.SetKillSwitch(ctx, false, actor); err != nil {
		t.Fatalf("SetKillSwitch(false): %v", err)
	}
	if h.l.Gates().KillSwitch {
		t.Fatal("the kill-switch is still set")
	}
	if n := len(h.ledger.byKind(types.KConfig)); n != 2 {
		t.Errorf("%d config record(s), want 2 (setting and clearing the switch are both persisted)", n)
	}
	resumed, err := h.l.ResumePending(ctx)
	if err != nil {
		t.Fatalf("ResumePending: %v", err)
	}
	if len(resumed) != 2 {
		t.Errorf("ResumePending resumed %v, want both pending incidents", resumed)
	}
	if st := h.l.incStateFor(check); st.Inc.State != types.StPlayCheck {
		t.Errorf("the resumed stage entry left %s in %q, want play:check_only", check, string(st.Inc.State))
	}
	if st := h.l.incStateFor(retry); st.Inc.State != types.StPlayDrafted {
		t.Errorf("the resumed retry left %s in %q, want play:drafted", retry, string(st.Inc.State))
	}
	if len(mp.modeList()) != wrapperRunsBefore+1 {
		t.Errorf("the runner was entered %d time(s) after the clear, want 1 (one fresh stage entry, no replay)", len(mp.modeList())-wrapperRunsBefore)
	} else if modes := mp.modeList(); modes[len(modes)-1] != types.ModeCheck {
		t.Errorf("the runner mode after the clear is %q, want check_mode", modes[len(modes)-1])
	}
	if mp.applies != 0 || h.play.applyCalls != 0 {
		t.Errorf("%d apply call(s) after the clear, want 0 (no replayed call)", mp.applies)
	}
	if n := killSwitchRefusals(h); n != expected {
		t.Errorf("%d kill-switch refusal record(s) after the clear, want %d", n, expected)
	}
	if left, err := h.l.Pending(ctx); err != nil || len(left) != 0 {
		t.Errorf("Pending after the resume returned %v (%v), want empty", left, err)
	}
	// The resume continues from resume_from: the emitted transition starts at the
	// refused state, not at a replayed command.
	found := false
	for _, r := range h.ledger.records {
		if r.Kind != types.KIncident || fmt.Sprint(r.Payload["transition"]) != "T08" {
			continue
		}
		if fmt.Sprint(r.Payload["from"]) == string(types.StPlayDrafted) && fmt.Sprint(r.Payload["to"]) == string(types.StPlayCheck) {
			found = true
		}
	}
	if !found {
		t.Error("no T08 record resumes from play:drafted: the resume must continue from resume_from")
	}
}

// killSwitchRefusals counts the refused+pending records carrying
// TROUBLE-LADDER-010: the kill-switch row's one-per-attempt rule.
func killSwitchRefusals(h *harness) int {
	h.ledger.mu.Lock()
	defer h.ledger.mu.Unlock()
	n := 0
	for _, r := range h.ledger.records {
		if r.Kind != types.KIncident {
			continue
		}
		if fmt.Sprint(r.Payload["error_code"]) != string(types.CodeLadder010) {
			continue
		}
		if r.Payload["refused"] == true && r.Payload["pending"] == true {
			n++
		}
	}
	return n
}

// TestModeDowngradeMidIncidentObeysTheNextStageEntry covers §3.11's change rule:
// a mode change is persisted as a config record through SetGates, takes effect at
// the next checkpoint (never mid-call) and a downgrade never aborts an in-flight
// mutating call.
func TestModeDowngradeMidIncidentObeysTheNextStageEntry(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, harnessOpts{
		rules:   map[string]types.Rule{"rule-a": {Name: "rule-a", EntryRung: types.RungPlay}},
		playFor: map[string]types.Play{"rule-a": {Name: "reload", Version: 1, CheckMode: true}},
	})
	mp := &modePlay{inner: h.play, t: t}
	withRegistry(h, mp)
	actor := types.Actor{Kind: types.ActorHuman, ID: "opuser"}

	up, err := h.l.SetGates(ctx, types.AutonomyGates{
		Mode: types.AutoFull, AllowDetect: true, AllowResearch: true, AllowAgent: true, AllowPlayMutate: true,
	}, actor)
	if err != nil {
		t.Fatalf("SetGates(full): %v", err)
	}
	if up.ChangedBy != actor.ID || up.ChangedTS == "" {
		t.Errorf("the persisted gates carry changed_by %q changed_ts %q, want the actor and a timestamp", up.ChangedBy, up.ChangedTS)
	}
	res := admitFresh(t, h, "downgrade", "rule-a")
	if _, err := h.l.Advance(ctx, res, Transition{Trigger: "T03"}); err != nil {
		t.Fatalf("T03: %v", err)
	}
	if _, err := h.l.Advance(ctx, res, Transition{Trigger: "T08"}); err != nil {
		t.Fatalf("T08: %v", err)
	}
	if modes := mp.modeList(); len(modes) != 1 || modes[0] != types.ModeApply {
		t.Fatalf("in full mode the runner modes are %v, want one apply", modes)
	}

	// The downgrade lands mid-incident, with the run sitting in play:check_only.
	configsBefore := len(h.ledger.byKind(types.KConfig))
	recordsBefore := len(h.ledger.records)
	if _, err := h.l.SetGates(ctx, gateForMode(types.AutoShadow, sigFor("downgrade").String()), actor); err != nil {
		t.Fatalf("SetGates(shadow): %v", err)
	}
	if got := h.l.Gates().Mode; got != types.AutoShadow {
		t.Fatalf("the ladder reports mode %q, want shadow", got)
	}
	configs := h.ledger.byKind(types.KConfig)
	if len(configs) != configsBefore+1 {
		t.Fatalf("the downgrade appended %d config record(s), want 1", len(configs)-configsBefore)
	}
	gates, ok := configs[len(configs)-1].Payload["gates"].(types.AutonomyGates)
	if !ok {
		t.Fatalf("the config record's gates are %T, want types.AutonomyGates", configs[len(configs)-1].Payload["gates"])
	}
	if gates.Mode != types.AutoShadow || gates.ChangedBy != actor.ID {
		t.Errorf("the persisted gates are %+v, want shadow changed by %s", gates, actor.ID)
	}
	if prev, ok := configs[len(configs)-1].Payload["previous"].(types.AutonomyGates); !ok || prev.Mode != types.AutoFull {
		t.Errorf("the config record's previous gates are %v, want mode full", configs[len(configs)-1].Payload["previous"])
	}
	if len(h.ledger.records) != recordsBefore+1 {
		t.Errorf("the downgrade appended %d record(s), want 1 (config only)", len(h.ledger.records)-recordsBefore)
	}
	if st := h.l.incStateFor(res); st.Inc.State != types.StPlayCheck {
		t.Errorf("the downgrade moved the in-flight incident to %q, want play:check_only", string(st.Inc.State))
	}
	if len(mp.modeList()) != 1 {
		t.Errorf("the downgrade re-entered the runner: modes %v", mp.modeList())
	}
	// The in-flight call finishes under the new mode: the run is not aborted.
	if _, err := h.l.Advance(ctx, res, Transition{Trigger: "T10"}); err != nil {
		t.Fatalf("T10 after the downgrade: %v", err)
	}
	if _, err := h.l.Advance(ctx, res, Transition{Trigger: "T12"}); err != nil {
		t.Fatalf("T12 after the downgrade: %v", err)
	}
	if st := h.l.incStateFor(res); st.Inc.State != types.StVerifying {
		t.Errorf("the in-flight incident is %q, want verifying", string(st.Inc.State))
	}

	// The next stage entry obeys the new mode: check_mode, nothing applied.
	next := admitFresh(t, h, "downgrade-next", "rule-a")
	if _, err := h.l.Advance(ctx, next, Transition{Trigger: "T03"}); err != nil {
		t.Fatalf("T03 (after the downgrade): %v", err)
	}
	if _, err := h.l.Advance(ctx, next, Transition{Trigger: "T08"}); err != nil {
		t.Fatalf("T08 (after the downgrade): %v", err)
	}
	if modes := mp.modeList(); len(modes) != 2 || modes[1] != types.ModeCheck {
		t.Errorf("the runner modes are %v, want [apply check_mode]: the next stage entry obeys the new mode", modes)
	}
	if h.play.applyCalls != 0 {
		t.Errorf("%d apply call(s) after the downgrade, want 0", h.play.applyCalls)
	}
}

// TestSetGatesRejectsAnUnknownMode covers §3.11's frozen mode set: an unknown
// mode is a refusal and the previous gates stay in force.
func TestSetGatesRejectsAnUnknownMode(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, harnessOpts{rules: map[string]types.Rule{"rule-a": {Name: "rule-a", EntryRung: types.RungPlay}}})
	actor := types.Actor{Kind: types.ActorHuman, ID: "opuser"}
	if _, err := h.l.SetGates(ctx, types.AutonomyGates{Mode: types.AutoAssisted, AllowDetect: true}, actor); err != nil {
		t.Fatalf("SetGates(assisted): %v", err)
	}
	prev, err := h.l.SetGates(ctx, types.AutonomyGates{Mode: types.AutonomyMode("yolo"), AllowDetect: true}, actor)
	if CodeOf(err) != types.CodeLadder011 {
		t.Fatalf("an unknown mode returned %v, want TROUBLE-LADDER-011", err)
	}
	if prev.Mode != types.AutoAssisted {
		t.Errorf("the returned gates are %+v, want the previous mode assisted", prev)
	}
	if got := h.l.Gates().Mode; got != types.AutoAssisted {
		t.Errorf("the ladder's mode is %q after the refusal, want assisted (a refused change never applies)", got)
	}
	for _, mode := range []types.AutonomyMode{types.AutoShadow, types.AutoAssisted, types.AutoFull} {
		if !mode.Valid() {
			t.Errorf("the frozen mode %q reports invalid", mode)
		}
	}
}
