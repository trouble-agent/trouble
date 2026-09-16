package skills

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// VerifyWindow is one open, unfinished verify window (§4.5 rule 3). The spec's
// local name is `verifyWindow`; it is exported here because the caller — SPEC-05
// or the composition root — is the only party that can build one, and a
// package-private type would make Hold uncallable from outside.
type VerifyWindow struct {
	Name    string
	Version int
	Sig     string
	Sigs    []string
	End     string
	Passed  bool
}

// Resolver answers the recurrence question (§2, §3.3, §4.5): which skill, if any,
// applies to a sig.
type Resolver struct {
	cfg   types.SkillsConfig
	store *Store
	deps  Deps
}

// NewResolver builds the resolver over a store.
func NewResolver(cfg types.SkillsConfig, store *Store, deps Deps) *Resolver {
	return &Resolver{cfg: cfg, store: store, deps: deps}
}

// Match finds the installed skill whose sig set contains sig, exactly.
//
// Two skills matching one sig is a conflict: the highest version wins and the
// loser is named in a conflict record (§4.5 rule 1). Two at the same version with
// different canonical bytes is ambiguity: no winner is invented, both are refused,
// and an earlier installed version for that name is the fallback (§4.5 rule 2).
func (r *Resolver) Match(sig string) (matchResult, error) {
	var out matchResult
	if _, err := types.ParseSig(sig); err != nil {
		return out, newErr(types.CodeSkills001, ReasonSigGrammar, "%v", err)
	}
	type cand struct {
		row   installRow
		skill types.Skill
		play  types.Play
		playB []byte
		canon string
	}
	var cands []cand
	for _, row := range r.store.Rows() {
		if row.State != types.SkillInstalled {
			continue
		}
		dir := filepath.Join(r.store.Dir(), filepath.FromSlash(row.Dir))
		skill, playBytes, _, err := readArtifact(dir)
		if err != nil {
			continue
		}
		if !containsString(skill.Sigs, sig) {
			continue
		}
		play, perr := DecodePlay(playBytes)
		if perr != nil {
			continue
		}
		canon, _ := CanonicalSHA256(skill, playBytes)
		cands = append(cands, cand{row: row, skill: skill, play: play, playB: playBytes, canon: canon})
	}
	switch len(cands) {
	case 0:
		out.Reason = "no_match"
		return out, nil
	case 1:
		out.Name = cands[0].skill.Name
		out.Version = cands[0].skill.Version
		out.Skill = cands[0].skill
		out.Play = cands[0].play
		out.PlayBytes = cands[0].playB
		out.Origin = cands[0].row.Origin
		out.Candidates = 1
		return out, nil
	}
	// Deterministic: highest version wins.
	winner := 0
	for i := 1; i < len(cands); i++ {
		if cands[i].skill.Version > cands[winner].skill.Version {
			winner = i
		}
	}
	sameVersion := 0
	for _, c := range cands {
		if c.skill.Version == cands[winner].skill.Version {
			sameVersion++
		}
	}
	if sameVersion > 1 {
		// Ambiguity: fail closed.
		var names []string
		for _, c := range cands {
			names = append(names, fmt.Sprintf("%s@%d", c.skill.Name, c.skill.Version))
			_, _ = r.deps.phaseRecord(context.Background(), PhaseConflict, sig, "", map[string]any{
				"sig":    sig,
				"winner": map[string]any{"name": c.skill.Name, "version": c.skill.Version},
				"loser":  map[string]any{"name": c.skill.Name, "version": c.skill.Version},
				"count":  1, "error_code": string(types.CodeSkills006), "reason": ReasonVersionAmbiguity,
			})
			_, _ = r.deps.phaseRecord(context.Background(), PhaseRefused, sig, "", map[string]any{
				"name": c.skill.Name, "version": c.skill.Version, "error_code": string(types.CodeSkills013),
				"reason": ReasonVersionAmbiguity, "count": 1,
			})
		}
		out.Conflict = true
		out.ConflictWhy = ReasonVersionAmbiguity
		out.Reason = fmt.Sprintf("version ambiguity between %s", strings.Join(names, ", "))
		return out, newErr(types.CodeSkills006, ReasonVersionAmbiguity, "%s", out.Reason)
	}
	w := cands[winner]
	out.Name = w.skill.Name
	out.Version = w.skill.Version
	out.Skill = w.skill
	out.Play = w.play
	out.PlayBytes = w.playB
	out.Origin = w.row.Origin
	out.Candidates = len(cands)
	out.Conflict = true
	out.ConflictWhy = ReasonConflict
	out.Winner = fmt.Sprintf("%s@%d", w.skill.Name, w.skill.Version)
	for i, c := range cands {
		if i == winner {
			continue
		}
		out.Loser = fmt.Sprintf("%s@%d", c.skill.Name, c.skill.Version)
		_, _ = r.deps.phaseRecord(context.Background(), PhaseConflict, sig, "", map[string]any{
			"sig":    sig,
			"winner": map[string]any{"name": w.skill.Name, "version": w.skill.Version},
			"loser":  map[string]any{"name": c.skill.Name, "version": c.skill.Version},
			"count":  1, "error_code": string(types.CodeSkills006),
		})
	}
	return out, nil
}

// Hold reports whether an open verify window blocks this sig (§4.5 rule 3). The
// incident is never blocked by the hold: it continues with the next rung.
func (r *Resolver) Hold(sig string, open []VerifyWindow) (holdEntry, bool) {
	for _, w := range open {
		if w.Passed || w.End == "" {
			continue
		}
		if !containsString(w.Sigs, sig) && w.Sig != sig {
			continue
		}
		before := len(r.store.Holds())
		h := r.store.OpenHold(sig, w.Name, w.Version, w.End)
		if len(r.store.Holds()) != before {
			// A new hold is a state change: one record, never one per check (§4.7).
			_, _ = r.deps.phaseRecord(context.Background(), PhaseHold, sig, "", map[string]any{
				"sig": sig, "held": map[string]any{"name": w.Name, "version": w.Version},
				"window_end": w.End, "count": 1, "held_ts": h.HeldTS,
			})
		}
		return h, true
	}
	// No matching open window: release any hold this sig had.
	r.store.ReleaseHolds(sig)
	return holdEntry{}, false
}

// AllowRun enforces `[guards].max_runs` ("<n>/day") per sig per host per day
// (§4.5 rule 4). Exceeding it is a refusal with reason=max_runs and no play run.
func (r *Resolver) AllowRun(name string, version int, sig string) (bool, string) {
	row, ok := r.store.row(name, version)
	if !ok || row.State != types.SkillInstalled {
		return false, ReasonMissingModule
	}
	dir := filepath.Join(r.store.Dir(), filepath.FromSlash(row.Dir))
	skill, _, _, err := readArtifact(dir)
	if err != nil {
		return false, ReasonTampered
	}
	max := parseMaxRuns(skill.Guards.MaxRuns)
	if max <= 0 {
		return true, ""
	}
	if r.store.RunsToday(name, version, sig) >= max {
		r.recordRefusal(sig, name, version, ReasonMaxRuns)
		return false, ReasonMaxRuns
	}
	return true, ""
}

func (r *Resolver) recordRefusal(sig, name string, version int, reason string) {
	key := fmt.Sprintf("%s|%d|refused|%s|%s", name, version, types.CodeSkills013, reason)
	n, emit := r.store.noteRefusal(key)
	if !emit {
		return
	}
	_, _ = r.deps.phaseRecord(context.Background(), PhaseRefused, sig, "", map[string]any{
		"name": name, "version": version, "error_code": string(types.CodeSkills013),
		"reason": reason, "count": n,
	})
}

// RecordRun is the post-run hook: counters, the failed-window demotion path
// (§4.5 rule 3) and the stats record of §4.7.
func (r *Resolver) RecordRun(name string, version int, success bool, ev types.Evidence, sig string) error {
	r.store.bumpStats(name, version, success, 0)
	r.store.noteRun(name, version, sig)
	if _, err := r.deps.phaseRecord(context.Background(), PhaseStats, sig, "", map[string]any{
		"name": name, "version": version,
		"applied":         r.store.Stats(name, version).Applied,
		"success":         r.store.Stats(name, version).Success,
		"refusals":        r.store.Stats(name, version).Refusals,
		"evidence_result": string(ev.Result),
	}); err != nil {
		return err
	}
	if success {
		r.store.ReleaseHolds(sig)
		return nil
	}
	row, ok := r.store.row(name, version)
	if !ok {
		return nil
	}
	row.Failures++
	demote := r.cfg.DemoteAfterFailures
	if demote <= 0 {
		demote = 2
	}
	if row.Failures >= demote {
		dir := filepath.Join(r.store.Dir(), filepath.FromSlash(row.Dir))
		if err := r.store.quarantine(row, types.CodeSkills013, ReasonDemoted); err != nil {
			return err
		}
		_ = dir
		if err := r.store.saveIndex(); err != nil {
			return err
		}
		_, err := r.deps.phaseRecord(context.Background(), PhaseDemoted, sig, "", map[string]any{
			"name": name, "version": version, "failures": row.Failures,
			"error_code": string(types.CodeSkills013), "reason": ReasonDemoted,
		})
		// The sig escalates to the agent rung; the refusal is explicit so nothing
		// was dropped silently.
		r.recordRefusal(sig, name, version, ReasonDemoted)
		return err
	}
	r.store.putRow(row)
	return r.store.saveIndex()
}

// RecordCanary stores a canary outcome for a version (§4.4).
func (r *Resolver) RecordCanary(name string, version int, result string, applyMode bool, verifyResult types.VerifyResultKind) error {
	rec := canaryRecord{
		Result:        result,
		HostID:        r.deps.hostID(),
		TS:            types.FormatUTC(r.deps.now()),
		ApplyMode:     applyMode,
		DaemonVersion: r.deps.daemonVersion(),
	}
	r.store.SetCanary(name, version, rec)
	if err := r.store.saveIndex(); err != nil {
		return err
	}
	phase := PhaseCanaryOK
	if result == "failed" {
		phase = PhaseCanaryFailed
	}
	_, err := r.deps.phaseRecord(context.Background(), phase, "", "", map[string]any{
		"name": name, "version": version, "host_id": rec.HostID, "result": result,
		"apply_mode": applyMode, "verify_result": string(verifyResult),
	})
	return err
}

// ExpireStaleCanaries clears a green canary older than canary_validity: a mutating
// remedy is never applied by timeout, only by a fresh green run (§4.4).
func (r *Resolver) ExpireStaleCanaries() int {
	validity := durationOrDays(r.cfg.CanaryValidity)
	if validity <= 0 {
		return 0
	}
	n := 0
	for _, row := range r.store.Rows() {
		if row.Canary.Result != "green" || row.Canary.TS == "" {
			continue
		}
		ts, err := types.ParseUTC(row.Canary.TS)
		if err != nil || r.deps.now().Sub(ts) <= validity {
			continue
		}
		row.Canary.Result = ""
		r.store.putRow(row)
		n++
	}
	if n > 0 {
		_ = r.store.saveIndex()
	}
	return n
}

// parseMaxRuns reads "<n>/day".
func parseMaxRuns(s string) int {
	parts := strings.SplitN(strings.TrimSpace(s), "/", 2)
	if len(parts) != 2 {
		return 0
	}
	n := 0
	if _, err := fmt.Sscanf(parts[0], "%d", &n); err != nil {
		return 0
	}
	return n
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// dayKey is the UTC day a run counts against.
func dayKey(now time.Time) string { return now.UTC().Format("2006-01-02") }
