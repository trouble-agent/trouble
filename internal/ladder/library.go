package ladder

import (
	"context"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// SkillLibrary is the ladder's read-and-execute view of the local skill library
// (SPEC-05 §2b). Both methods are typed: a step is a `PlayTask` — a registry module
// name plus literal args — and execution returns the audit record of one typed tool
// call. There is no shell, interpreter, template or script in this path, and no
// method here accepts one.
//
// It is implemented by internal/skills' Library and by this package's tests.
type SkillLibrary interface {
	// Plays lists the library's skills, each compiled to the play shape the runner
	// already consumes. Read-only.
	Plays(ctx context.Context) ([]types.Play, error)
	// RunStep executes step `step` of library skill `name` against the runner:
	// check_mode when the gate denies mutation, apply only when it allows it. It
	// writes the `skill`-kind step record (SPEC-11 §4.7a) and returns the tool
	// call's audit record.
	RunStep(ctx context.Context, inc types.Incident, name string, step int, mode string) (types.ToolCall, error)
}

// reason tokens this file adds (the rest live in errors.go).
const (
	reasonNoSkillLibrary = "no_skill_library"
	reasonUnknownStep    = "unknown_skill_step"
	reasonStepLookup     = "skill_lookup_failed"
)

// SkillPlays is the read surface for the local library (SPEC-05 §2b): the compiled
// plays, or an empty list when no library is wired. Reading a library that is not
// there is not a failure — the skills stage simply has nothing to run.
func (l *Ladder) SkillPlays(ctx context.Context) ([]types.Play, error) {
	if l.deps.Skills == nil {
		return []types.Play{}, nil
	}
	plays, err := l.deps.Skills.Plays(ctx)
	if plays == nil {
		plays = []types.Play{}
	}
	return plays, err
}

// RunSkillStep is the skills stage entry (SPEC-05 §2b): the §3.11 gate and the
// §3.4 kill-switch check, then exactly one typed step against the runner.
//
// The mode is the play gate's, because a library step is play material: `shadow`
// runs it in check_mode and mutates nothing; `assisted` applies only with the
// module grant the play path already uses (`<rule>|<module>[|<sig-short>]`);
// `full` applies, with the registry's own six stages still binding. A denied
// entry is recorded (pending under the kill-switch, refused otherwise) and the
// runner is never called.
func (l *Ladder) RunSkillStep(ctx context.Context, incID, name string, step int) (types.ToolCall, error) {
	l.mu.Lock()
	st := l.incs[incID]
	if st == nil {
		l.mu.Unlock()
		return types.ToolCall{}, newErr(types.CodeLadder012, "", "incident %s is not in the index", incID)
	}
	if l.deps.Skills == nil {
		l.mu.Unlock()
		return types.ToolCall{}, newErr(types.CodeLadder011, reasonNoSkillLibrary,
			"no skill library is wired: the skills stage has nothing to run")
	}
	if l.gate.KillSwitch {
		_ = l.appendIncident(ctx, st, map[string]any{
			"stage":       "skills",
			"refused":     true,
			"pending":     true,
			"resume_from": string(st.Inc.State),
			"skill_step":  skillStepRef(name, step),
			"reason":      reasonKillSwitch,
			"error_code":  string(types.CodeLadder010),
		})
		l.mu.Unlock()
		return types.ToolCall{}, newErr(types.CodeLadder010, reasonKillSwitch,
			"the kill-switch is set: the skills-stage entry is refused and recorded as pending")
	}
	inc := st.Inc
	rule := st.Rule
	// The mode is the play gate's: shadow (or a cleared AllowPlayMutate) forces
	// check_mode for every step, so the default posture mutates nothing.
	mode := types.ModeApply
	if !l.gate.AllowPlayMutate || l.gate.Mode == types.AutoShadow || l.gate.Mode == "" {
		mode = types.ModeCheck
	}
	l.mu.Unlock()

	// The step's module decides whether it may mutate, so it is read from the
	// library's own typed compilation rather than guessed.
	plays, err := l.deps.Skills.Plays(ctx)
	if err != nil {
		return types.ToolCall{}, wrapErr(types.CodeLadder021, reasonStepLookup, err)
	}
	module, ok := stepModule(plays, name, step)
	if !ok {
		l.mu.Lock()
		_ = l.appendIncident(ctx, st, map[string]any{
			"stage":      "skills",
			"refused":    true,
			"pending":    false,
			"skill_step": skillStepRef(name, step),
			"reason":     reasonUnknownStep,
			"error_code": string(types.CodeLadder011),
		})
		l.mu.Unlock()
		return types.ToolCall{}, newErr(types.CodeLadder011, reasonUnknownStep,
			"the library has no skill %q step %d", name, step)
	}

	// Assisted mode: a MUTATING step needs the same module grant a play needs, and
	// the ladder asks its own grant table (Granted) rather than re-implementing the
	// matching. A non-mutating module still runs under assisted: the registry's own
	// authorize stage is what decides a module's mutating-ness per call, and this
	// check only refuses the steps that could change the host.
	if mode == types.ModeApply && isMutatingModule(module) && !l.Granted(ctx, rule, module, inc.Sig) {
		l.mu.Lock()
		_ = l.appendIncident(ctx, st, map[string]any{
			"stage":      "skills",
			"refused":    true,
			"pending":    false,
			"skill_step": skillStepRef(name, step),
			"module":     module,
			"mode":       mode,
			"reason":     reasonGateDenied,
			"error_code": string(types.CodeLadder011),
		})
		l.mu.Unlock()
		return types.ToolCall{}, newErr(types.CodeLadder011, reasonGateDenied,
			"the gate denies %s for rule %q: a library step mutates only under a grant", module, rule)
	}

	return l.deps.Skills.RunStep(ctx, inc, name, step, mode)
}

// skillStepRef is the payload reference of one library step: an index and a name,
// never a command line.
func skillStepRef(name string, step int) map[string]any {
	return map[string]any{"skill": name, "step": step}
}

// stepModule resolves the module of one library step from the compiled plays.
func stepModule(plays []types.Play, name string, step int) (string, bool) {
	for _, p := range plays {
		if p.Name != name {
			continue
		}
		if step < 0 || step >= len(p.Tasks) {
			return "", false
		}
		return p.Tasks[step].Tool, true
	}
	return "", false
}
