package registry

import (
	"context"
	"strings"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// policyDecision is the output of the authorize stage (SPEC-06 §2.3).
type policyDecision struct {
	allow  bool
	mode   string
	grants []string
	reason string
}

// ponrModules are the point-of-no-return modules: mutating modules with no
// rollback hint (SPEC-06 §3.5). A PONR call is never applied without an explicit
// grant naming the module itself, in every autonomy mode.
var ponrModules = map[string]bool{
	"service.reload":     true,
	"service.restart":    true,
	"flow.file_issue":    true,
	"flow.create_task":   true,
	"flow.comment":       true,
	"flow.spawn_foreman": true,
	"flow.promote":       true,
	"flow.rollback":      true,
}

// invertibleModules are the modules whose Apply carries a usable RollbackHint
// (SPEC-06 §3.5 table): config.set and file.patch.
var invertibleModules = map[string]bool{
	"config.set": true,
	"file.patch": true,
}

// IsPONR reports whether a module name is in the point-of-no-return set.
func IsPONR(module string) bool { return ponrModules[module] }

// authorize is the fixed seven-step ladder of SPEC-06 §2.3: deny by default at
// every step, the first refusal wins.
func (r *Registry) authorize(ctx context.Context, req types.ToolCallRequest) (types.Module, policyDecision, *Error) {
	dec := policyDecision{mode: req.Mode, grants: req.Grants}
	if dec.mode == "" {
		dec.mode = types.ModeCheck
	}
	// a1 — the module name exists in the registry.
	mod, ok := r.module(req.Module)
	if !ok {
		return nil, dec, newErr(types.CodeRegistry001, types.StageAuthorize, reasonModuleUnavailable, "unknown module %q", req.Module)
	}
	d := mod.Descriptor()
	// The reserved capability probe is an audit subject, never dispatchable.
	if req.Module == CapabilityProbeModule {
		return nil, dec, newErr(types.CodeRegistry001, types.StageAuthorize, reasonReservedCapability,
			"%s is reserved and non-dispatchable", CapabilityProbeModule)
	}
	gates := r.gates()

	// a2 — kill-switch off.
	if gates.KillSwitch {
		return nil, dec, newErr(types.CodeRegistry006, types.StageAuthorize, reasonKillSwitch,
			"the kill-switch is set; no stage entry runs")
	}
	// a3 — do-not-touch, BEFORE validation and before any target is touched.
	if e := r.checkProtected(mod, req.Args); e != nil {
		return nil, dec, e
	}
	// a4 — capability.
	if e := r.checkCapability(mod, req.Args, gates); e != nil {
		return nil, dec, e
	}
	// a5 — the skill allowlist (the Authorizer seam); a nil hook denies.
	if src, ok := strings.CutPrefix(req.Source, "skill:"); ok && d.Mutating {
		if r.deps.SkillAuthorize == nil {
			return nil, dec, newErr(types.CodeRegistry006, types.StageAuthorize, reasonSkillAllowlist,
				"skill-sourced mutating call refused: no Authorizer is wired (deny by default)")
		}
		if err := r.deps.SkillAuthorize(ctx, src, req.Module, d.Scopes); err != nil {
			return nil, dec, newErr(types.CodeRegistry006, types.StageAuthorize, reasonSkillAllowlist,
				"skill %s may not call %s: %v", src, req.Module, err)
		}
	}
	// a7 — autonomy gate. Shadow forces check_mode for every mutating call; a
	// PONR call needs an explicit module-name grant in every mode.
	applyWanted := dec.mode == types.ModeApply
	if d.Mutating && applyWanted {
		switch gates.Mode {
		case types.AutoShadow, "":
			dec.mode = types.ModeCheck
			dec.reason = "shadow forces check_mode"
		case types.AutoAssisted:
			if !grantedModule(req.Grants, req.Module) && !grantedModule(req.Grants, "scope:"+d.Scopes[0]) {
				return nil, dec, newErr(types.CodeRegistry006, types.StageAuthorize, reasonScopeNotGranted,
					"assisted mode: %s is not granted by this rule", req.Module)
			}
		case types.AutoFull:
			// Everything is allowed, the PONR rule below excepted.
		default:
			return nil, dec, newErr(types.CodeRegistry008, types.StageAuthorize, reasonScopeNotGranted,
				"unknown autonomy mode %q", gates.Mode)
		}
	}
	if d.Mutating && dec.mode == types.ModeApply && ponrModules[req.Module] && !grantedModule(req.Grants, req.Module) {
		return nil, dec, newErr(types.CodeRegistry015, types.StageAuthorize, reasonPONRWithoutGrant,
			"%s is a point-of-no-return module: it needs an explicit module-name grant (a scope: wildcard never authorizes it)", req.Module)
	}
	// a6 — every required scope present in the effective grants. A check_mode
	// call mutates nothing and a pure module reads only, so neither consumes a
	// grant (§3.3's class table); a mutating apply must be covered.
	if d.Mutating && dec.mode == types.ModeApply {
		for _, scope := range d.Scopes {
			if !grantedModule(req.Grants, req.Module) && !grantedModule(req.Grants, "scope:"+scope) {
				return nil, dec, newErr(types.CodeRegistry008, types.StageAuthorize, reasonScopeNotGranted,
					"%s requires scope %s which the effective grants do not carry", req.Module, scope)
			}
		}
	}
	dec.allow = true
	return mod, dec, nil
}

// GrantTokenMatches reports whether one grant token covers a module (SPEC-05
// §3.11: the whole entry must match exactly — no prefix or glob semantics).
func GrantTokenMatches(token, module string) bool {
	if token == module {
		return true
	}
	if scope, ok := strings.CutPrefix(token, "scope:"); ok {
		mod, _, _ := strings.Cut(module, ".")
		return scope == mod+":write" || scope == mod+":read"
	}
	return false
}

func grantedModule(grants []string, want string) bool {
	for _, g := range grants {
		if g == want {
			return true
		}
	}
	return false
}

// checkCapability is step a4: the mutating verbs that need an install-time
// capability, and the allow-root containment rule of file.patch.
func (r *Registry) checkCapability(mod types.Module, args map[string]any, gates types.AutonomyGates) *Error {
	d := mod.Descriptor()
	if !d.Mutating {
		return nil
	}
	switch {
	case strings.HasPrefix(d.Name, "service."):
		unit := ""
		if s, ok := args["unit"].(string); ok {
			unit = s
		}
		norm := normalizeUnit(unit)
		allowed := false
		for _, u := range r.cfg.ServiceUnits {
			if normalizeUnit(u) == norm {
				allowed = true
				break
			}
		}
		if !allowed {
			return newErr(types.CodeRegistry006, types.StageAuthorize, reasonServeUnitNotAllowed,
				"unit %s is not in registry.service_units (empty means no unit may be reloaded or restarted)", norm)
		}
		if r.capabilityState() != capOK {
			return newErr(types.CodeRegistry006, types.StageAuthorize, reasonPolicyMissing,
				"service:write capability is %s for this host; install contrib/polkit/49-trouble.rules and reload polkit", r.capabilityReason())
		}
	case d.Name == "file.patch":
		ok, reason := r.withinAllowRoots(args)
		if !ok {
			return newErr(types.CodeRegistry006, types.StageAuthorize, reasonAllowRootEscape, "%s", reason)
		}
	}
	return nil
}

// withinAllowRoots enforces file.allow_roots containment on the symlink-resolved
// path: the resolved path must be inside a configured root (SPEC-06 §6.7).
func (r *Registry) withinAllowRoots(args map[string]any) (bool, string) {
	var p string
	if s, ok := args["path"].(string); ok {
		p = s
	}
	if p == "" {
		return false, "file.patch without a path"
	}
	cand := canonicalPath(p)
	if len(r.cfg.FileAllowRoots) == 0 {
		return false, "file.allow_roots is empty: nothing is patchable until it is configured"
	}
	for _, root := range r.cfg.FileAllowRoots {
		cr := canonicalPath(root)
		if cr == "" {
			continue
		}
		if cand == cr || strings.HasPrefix(cand, strings.TrimSuffix(cr, "/")+"/") {
			return true, ""
		}
	}
	return false, "resolved path " + cand + " escapes every file.allow_roots entry (symlinks are never written through)"
}

func (r *Registry) gates() types.AutonomyGates {
	if r.deps.Gates == nil {
		return types.DefaultGates()
	}
	g := r.deps.Gates()
	if g.Mode == "" {
		g.Mode = types.AutoShadow
	}
	return g
}

// AutonomyGates is the read-through the CLI and the dashboard use.
func (r *Registry) AutonomyGates() types.AutonomyGates { return r.gates() }
