package skills

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/trouble-agent/trouble/internal/types"
)

// Authorizer is the ONLY path from a skill artifact to a capability (SPEC-11 §2).
//
// SPEC-06's authorize stage consults it when `req.Source` starts with `skill:`;
// the module is refused unless the resolved installed row's allowed_modules lists
// it. It re-resolves the source on every call, which is the defence in depth
// against a locally mutated play file or a stale in-memory task list.
type Authorizer struct {
	cfg   types.SkillsConfig
	store *Store
	deps  Deps
}

// NewAuthorizer builds the registry hook.
func NewAuthorizer(cfg types.SkillsConfig, store *Store, deps Deps) *Authorizer {
	return &Authorizer{cfg: cfg, store: store, deps: deps}
}

// Authorize implements SPEC-06's SkillAuthorize seam:
// `func(ctx, skillID, module string, scopes []string) error`.
//
// It never invents a capability: a skill can cause typed registry tool calls the
// build already ships and its own allowlist already names.
func (a *Authorizer) Authorize(_ context.Context, skillID, module string, scopes []string) error {
	row, ok := a.store.InstalledByID(skillID)
	if !ok {
		return newErr(types.CodeSkills013, ReasonValidation,
			"skill %q is not an installed artifact on this host", skillID)
	}
	dir := filepath.Join(a.store.Dir(), filepath.FromSlash(row.Dir))
	skill, playBytes, _, err := readArtifact(dir)
	if err != nil {
		return newErr(types.CodeSkills002, ReasonTampered, "skill %s@%d: %v", row.Name, row.Version, err)
	}
	// The artifact on disk must still hash to what was installed: a locally
	// mutated SKILL.toml or play file is refused here, at the only door to a
	// capability, not merely at boot.
	if canon, cerr := CanonicalSHA256(skill, playBytes); cerr == nil && row.CanonicalSV != "" && canon != row.CanonicalSV {
		return newErr(types.CodeSkills002, ReasonTampered,
			"skill %s@%d no longer matches its recorded canonical bytes", row.Name, row.Version)
	}
	allowed := map[string]bool{}
	for _, m := range skill.AllowedModules {
		allowed[m] = true
	}
	if !allowed[module] {
		return newErr(types.CodeSkills005, ReasonAllowlist,
			"%s@%d does not list %q in allowed_modules", row.Name, row.Version, module)
	}
	if registered := a.registered(); len(registered) > 0 && !containsString(registered, module) {
		return newErr(types.CodeSkills013, ReasonMissingModule,
			"%s@%d lists %q but this build does not register it", row.Name, row.Version, module)
	}
	// Scopes are the module's own declaration (SPEC-06 owns them); the hook only
	// asserts the skill may reach the module at all.
	_ = scopes
	return nil
}

// AuthorizeSource is the same check by source string (`skill:<id>` or
// `skill:<name>@<version>`), for callers that hold the raw source.
func (a *Authorizer) AuthorizeSource(ctx context.Context, source, module string) error {
	id := source
	for _, prefix := range []string{"skill:", "$skill:"} {
		if len(id) >= len(prefix) && id[:len(prefix)] == prefix {
			id = id[len(prefix):]
			break
		}
	}
	if id == "" {
		return newErr(types.CodeSkills013, ReasonValidation, "empty skill source")
	}
	return a.Authorize(ctx, id, module, nil)
}

// Explain renders the gate chain for one (name, version): every gate result in
// order, which is what `trouble skills explain <name>@<v>` prints (§2).
func (s *Skills) Explain(name string, version int, moduleList []string) []string {
	out := []string{}
	row, ok := s.Store.row(name, version)
	if !ok {
		return append(out, fmt.Sprintf("%s@%d: not in the local index", name, version))
	}
	out = append(out, fmt.Sprintf("schema: %s", row.State))
	dir := filepath.Join(s.Store.Dir(), filepath.FromSlash(row.Dir))
	skill, playBytes, _, err := readArtifact(dir)
	if err != nil {
		return append(out, fmt.Sprintf("artifact: %v", err))
	}
	out = append(out, fmt.Sprintf("signature: %s", Describe(GateSignature(skill, playBytes, s.Cfg.Signers, false, "@"+s.Deps.hostID()))))
	out = append(out, fmt.Sprintf("floor: %s", Describe(GateFloor(skill, s.Deps.daemonVersion()))))
	if play, perr := DecodePlay(playBytes); perr == nil {
		out = append(out, fmt.Sprintf("modules: %s", Describe(GateModules(skill, play, moduleList))))
	}
	out = append(out, fmt.Sprintf("canary: %s", canaryExplain(s, skill, row)))
	out = append(out, fmt.Sprintf("approve: %s (%s)", row.State, s.Cfg.Approve))
	return out
}

func canaryExplain(s *Skills, skill types.Skill, row installRow) string {
	if s.Cfg.CanaryHostID == "" {
		return "disabled"
	}
	if s.Cfg.CanaryHostID == s.Deps.hostID() {
		return "this host is the canary"
	}
	if rec, ok := s.Store.CanaryGreen(skill.Name, skill.Version, s.Cfg.CanaryHostID, durationOrDays(s.Cfg.CanaryValidity)); ok {
		return "green " + rec.TS
	}
	return "no green result inside " + string(s.Cfg.CanaryValidity)
}

func (a *Authorizer) registered() []string {
	if a.deps.Registered == nil {
		return nil
	}
	return a.deps.Registered()
}
