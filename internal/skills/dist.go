package skills

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// pullState is pull-state.json (§3.5).
type pullState struct {
	LastPullTS  string `json:"last_pull_ts"`
	LastPullSHA string `json:"last_pull_sha"`
	Failures    int    `json:"failures"`
	NextPullTS  string `json:"next_pull_ts"`
	LastError   string `json:"last_error"`
	Revision    int    `json:"revision"`
}

// Puller is the pull-only distribution side of §4.1. It reads the channel and
// never writes to it: the git invocation log of a run contains no push, commit,
// merge, gc, prune or stash.
type Puller struct {
	cfg   types.SkillsConfig
	store *Store
	deps  Deps

	mu      sync.Mutex
	state   pullState
	running bool
	// calls records every git argv vector issued, for the §7 assertion that the
	// channel is only ever read.
	calls [][]string
}

// NewPuller builds the puller over an existing store.
func NewPuller(cfg types.SkillsConfig, store *Store, deps Deps) (*Puller, error) {
	p := &Puller{cfg: cfg, store: store, deps: deps}
	if raw, err := os.ReadFile(filepath.Join(store.Dir(), "pull-state.json")); err == nil {
		_ = jsonUnmarshal(raw, &p.state)
	}
	return p, nil
}

// GitCalls returns the git argv vectors issued so far (tests and the run report).
func (p *Puller) GitCalls() [][]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]string, len(p.calls))
	copy(out, p.calls)
	return out
}

// State returns the current pull state.
func (p *Puller) State() pullState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

// Run pulls on boot (when configured) and then every pull_interval with the
// per-host jitter, single-flight.
func (p *Puller) Run(ctx context.Context) {
	if p.cfg.PullOnBoot {
		_, _ = p.PullOnce(ctx)
	}
	for {
		interval := p.nextInterval()
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
		if ctx.Err() != nil {
			return
		}
		_, _ = p.PullOnce(ctx)
	}
}

// nextInterval is pull_interval ± jitter, with the failure backoff of §4.1
// (min(interval * 2^failures, 30m)).
func (p *Puller) nextInterval() time.Duration {
	base := p.cfg.PullInterval.Std()
	if base <= 0 {
		base = 15 * time.Minute
	}
	p.mu.Lock()
	failures := p.state.Failures
	p.mu.Unlock()
	if failures > 0 {
		backoff := base
		for i := 0; i < failures && backoff < 30*time.Minute; i++ {
			backoff *= 2
		}
		if backoff > 30*time.Minute {
			backoff = 30 * time.Minute
		}
		return backoff
	}
	jitterPct := p.cfg.PullJitterPct
	if jitterPct <= 0 {
		return base
	}
	// The jitter is per-host and derived from sha256(host_id), so N hosts do not
	// stampede one repo.
	h := fnv1a(p.deps.hostID())
	frac := float64(h%uint64(2*jitterPct+1)) / 100.0
	return base + time.Duration(frac*float64(base))
}

func fnv1a(s string) uint64 {
	var h uint64 = 1469598103934665603
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}

// PullOnce performs one pull: resolve → fetch → checkout → stage → gate → install.
// A resolved sha equal to the last one is a no-op that writes no ledger record.
func (p *Puller) PullOnce(ctx context.Context) (pullReport, error) {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return pullReport{}, newErr(types.CodeSkills010, ReasonPullRef, "a pull is already in flight (single-flight)")
	}
	p.running = true
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.running = false
		p.mu.Unlock()
	}()

	rep := pullReport{Ref: p.cfg.SourceRef, RefMode: p.cfg.RefMode}
	src, err := EffectiveSource(p.cfg)
	if err != nil {
		return rep, err
	}
	timeout := p.cfg.PullTimeout.Std()
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cache := filepath.Join(p.store.Dir(), "repo")
	if err := p.ensureCache(runCtx, src, cache); err != nil {
		p.fail(ctx, rep, err)
		return rep, err
	}
	sha, err := p.resolveRef(runCtx, src, cache)
	if err != nil {
		p.fail(ctx, rep, err)
		return rep, err
	}
	rep.ResolvedSHA = sha
	if sha == p.lastSHA() {
		rep.Noop = true
		p.markPull(sha, false)
		return rep, nil
	}
	if err := p.git(runCtx, cache, "checkout", "--detach", sha); err != nil {
		p.fail(ctx, rep, err)
		return rep, err
	}
	stagingID := types.NewID(types.PEv)
	stage := p.store.stageDir(stagingID)
	defer os.RemoveAll(stage)
	seen, err := p.copyTree(cache, stage)
	if err != nil {
		p.fail(ctx, rep, err)
		return rep, err
	}
	rep.Seen = seen

	results := p.installStaged(ctx, stage, &rep)
	_ = results
	p.markPull(sha, false)
	_, _ = p.deps.phaseRecord(ctx, PhasePulled, "", "", map[string]any{
		"ref": p.cfg.SourceRef, "ref_mode": p.cfg.RefMode, "resolved_sha": sha,
		"seen": rep.Seen, "installed": rep.Installed, "refused": rep.Refused,
		"missing_modules": rep.MissingModules,
	})
	if err := p.saveState(); err != nil {
		return rep, err
	}
	return rep, nil
}

// ensureCache makes sure the cache clone exists. The clone is a read of the
// channel; the source is never written to.
func (p *Puller) ensureCache(ctx context.Context, src SourcePath, cache string) error {
	// The cache is a NON-bare clone: the artifacts are read from a working tree
	// created by `checkout --detach` in the cache, never in the source.
	if _, err := os.Stat(filepath.Join(cache, ".git")); err == nil {
		return nil
	}
	if err := os.MkdirAll(cache, 0o700); err != nil {
		return newErr(types.CodeSkills010, ReasonPullRef, "cache dir: %v", err)
	}
	if err := p.git(ctx, "", "clone", "--quiet", "--no-checkout", src.Path, cache); err != nil {
		if isGitMissing(err) {
			return newErr(types.CodeSkills011, ReasonPullOffline, "git binary is missing: %v", err)
		}
		return newErr(types.CodeSkills011, ReasonPullOffline, "clone %s: %v", src.Path, err)
	}
	return nil
}

// isGitMissing distinguishes "no git on this host" (TROUBLE-SKILLS-011) from a
// failure of the source itself.
func isGitMissingErr(err error) bool { return isGitMissing(err) }

// resolveRef selects the ref and returns the resolved sha. Tag mode picks the
// highest semver tag; branch mode the branch tip (recorded as mutable).
func (p *Puller) resolveRef(ctx context.Context, src SourcePath, cache string) (string, error) {
	if p.cfg.RefMode == "branch" {
		branch := p.cfg.SourceRef
		if branch == "" || branch == "v*" {
			branch = "main"
		}
		out, err := p.gitOutput(ctx, "", "ls-remote", src.Path, "refs/heads/"+branch)
		if err != nil {
			return "", newErr(types.CodeSkills011, ReasonPullOffline, "ls-remote %s: %v", src.Path, err)
		}
		sha := firstSHA(out)
		if sha == "" {
			return "", newErr(types.CodeSkills010, ReasonPullRef, "branch %q is not present at the source", branch)
		}
		if err := p.git(ctx, cache, "fetch", "--quiet", src.Path, branch); err != nil {
			return "", newErr(types.CodeSkills011, ReasonPullOffline, "fetch %s: %v", branch, err)
		}
		return p.revParse(ctx, cache, "FETCH_HEAD")
	}
	pattern := p.cfg.SourceRef
	if pattern == "" {
		pattern = "v*"
	}
	out, err := p.gitOutput(ctx, "", "ls-remote", "--tags", src.Path)
	if err != nil {
		return "", newErr(types.CodeSkills011, ReasonPullOffline, "ls-remote --tags %s: %v", src.Path, err)
	}
	tag, sha := highestTag(out, pattern)
	if tag == "" {
		return "", newErr(types.CodeSkills010, ReasonPullRef, "no tag matches %q at the source", pattern)
	}
	if err := p.git(ctx, cache, "fetch", "--quiet", "--tags", src.Path, tag); err != nil {
		return "", newErr(types.CodeSkills011, ReasonPullOffline, "fetch %s: %v", tag, err)
	}
	resolved, err := p.revParse(ctx, cache, tag+"^{commit}")
	if err != nil {
		return "", err
	}
	if resolved == "" {
		resolved = sha
	}
	return resolved, nil
}

func (p *Puller) revParse(ctx context.Context, cache, ref string) (string, error) {
	out, err := p.gitOutput(ctx, cache, "rev-parse", ref)
	if err != nil {
		return "", newErr(types.CodeSkills010, ReasonPullRef, "rev-parse %s: %v", ref, err)
	}
	return strings.TrimSpace(out), nil
}

// highestTag picks the highest semver tag matching pattern (v prefix optional).
func highestTag(lsRemote string, pattern string) (string, string) {
	best := ""
	bestSHA := ""
	var bestV semver
	// A pattern with no wildcard is an exact tag name; a pattern with one is a
	// prefix match. Anything else would silently widen an operator's `source_ref`.
	exact := ""
	prefix := ""
	if i := strings.Index(pattern, "*"); i >= 0 {
		prefix = pattern[:i]
	} else if pattern != "" {
		exact = pattern
	}
	for _, line := range strings.Split(lsRemote, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !strings.HasPrefix(fields[1], "refs/tags/") {
			continue
		}
		if strings.HasSuffix(fields[1], "^{}") {
			continue
		}
		name := strings.TrimPrefix(fields[1], "refs/tags/")
		if exact != "" && name != exact {
			continue
		}
		if prefix != "" && !strings.HasPrefix(name, prefix) {
			continue
		}
		v, ok := parseSemver(name)
		if !ok {
			continue
		}
		if best == "" || bestV.less(v) {
			best, bestSHA, bestV = name, fields[0], v
		}
	}
	return best, bestSHA
}

func firstSHA(lsRemote string) string {
	for _, line := range strings.Split(lsRemote, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 1 && len(fields[0]) == 40 {
			return fields[0]
		}
	}
	return ""
}

// copyTree stages the checked-out tree under the size cap and reports how many
// artifacts it holds.
func (p *Puller) copyTree(cache, stage string) (int, error) {
	if err := os.MkdirAll(stage, 0o700); err != nil {
		return 0, newErr(types.CodeSkills010, ReasonPullSize, "staging dir: %v", err)
	}
	var total int64
	seen := 0
	err := filepath.WalkDir(cache, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, rerr := filepath.Rel(cache, path)
		if rerr != nil {
			return nil
		}
		if rel == "." {
			return nil // keep walking: skipping the root would skip everything
		}
		if rel == ".git" || strings.HasPrefix(rel, ".git"+string(filepath.Separator)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(stage, rel), 0o700)
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		total += info.Size()
		if p.cfg.PullMaxBytes > 0 && total > p.cfg.PullMaxBytes {
			return newErr(types.CodeSkills010, ReasonPullSize,
				"the staged tree exceeds pull_max_bytes (%d)", p.cfg.PullMaxBytes)
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return newErr(types.CodeSkills010, ReasonPullRef, "read %s: %v", rel, rerr)
		}
		dst := filepath.Join(stage, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(dst, raw, 0o600); err != nil {
			return newErr(types.CodeSkills010, ReasonPullRef, "stage %s: %v", rel, err)
		}
		if strings.HasSuffix(rel, "SKILL.toml") {
			seen++
		}
		return nil
	})
	if err != nil {
		if e, ok := err.(*Error); ok {
			return seen, e
		}
		return seen, newErr(types.CodeSkills010, ReasonPullRef, "stage: %v", err)
	}
	return seen, nil
}

// installStaged runs the §4.2 gate chain per artifact, in order, first failure
// wins. One artifact's refusal never blocks another's install.
func (p *Puller) installStaged(ctx context.Context, stage string, rep *pullReport) []string {
	var out []string
	root := filepath.Join(stage, "skills")
	entries, err := os.ReadDir(root)
	if err != nil {
		return out
	}
	registered := p.registered()
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		dir := filepath.Join(root, name)
		skill, playBytes, _, err := readArtifact(dir)
		if err != nil {
			p.refuse(ctx, name, 0, err, rep)
			continue
		}
		if err := p.gate(ctx, skill, playBytes, registered, rep); err != nil {
			continue
		}
		if err := p.install(ctx, skill, playBytes, dir, "pull"); err != nil {
			p.refuse(ctx, skill.Name, skill.Version, err, rep)
			continue
		}
		if rep != nil {
			if p.cfg.Approve == "review" {
				rep.Pending++
			} else {
				rep.Installed++
			}
		}
		out = append(out, fmt.Sprintf("%s@%d", skill.Name, skill.Version))
	}
	return out
}

// gate is the §4.2 chain: parse (done), signature, modules, floor, canary,
// approve, version rules.
func (p *Puller) gate(ctx context.Context, skill types.Skill, playBytes []byte, registered []string, rep *pullReport) error {
	// 2. signature + play payload binding.
	if err := GateSignature(skill, playBytes, p.cfg.Signers, p.cfg.RequireSignature, "@"+p.deps.hostID()); err != nil {
		p.refuse(ctx, skill.Name, skill.Version, err, rep)
		return err
	}
	play, err := DecodePlay(playBytes)
	if err != nil {
		p.refuse(ctx, skill.Name, skill.Version, err, rep)
		return err
	}
	// 3. modules.
	if err := GateModules(skill, play, registered); err != nil {
		p.refuse(ctx, skill.Name, skill.Version, err, rep)
		return err
	}
	if missing := MissingModules(skill, registered); len(missing) > 0 {
		rep.MissingModules = append(rep.MissingModules, missing...)
	}
	// 4. floor: recorded as a refusal whose cause is the floor code (§4.2).
	if err := GateFloor(skill, p.deps.daemonVersion()); err != nil {
		row, _ := p.store.row(skill.Name, skill.Version)
		row.Name, row.Version = skill.Name, skill.Version
		row.State = types.SkillFloorBlocked
		row.SignerKeyID = skill.SignerKeyID
		row.Sigs = len(skill.Sigs)
		row.RefusalCode = string(types.CodeSkills004)
		row.Reason = ReasonFloor
		row.Refusals++
		p.store.putRow(row)
		_ = p.store.saveIndex()
		if rep != nil {
			rep.Refused++
		}
		// §4.2/§4.3: the refusal is 013 with the floor failure as its cause, so a
		// satellite below the floor is recorded as floor_blocked rather than
		// skipped silently.
		p.emitRefusal(ctx, skill.Name, skill.Version, types.CodeSkills013, ReasonFloor, types.CodeSkills004, err.Error())
		return newErr(types.CodeSkills013, ReasonFloor, "%s", err.Error())
	}
	// 5. canary.
	if p.cfg.CanaryHostID != "" {
		isCanaryHost := p.cfg.CanaryHostID == p.deps.hostID()
		if !isCanaryHost {
			rec, ok := p.store.CanaryGreen(skill.Name, skill.Version, p.cfg.CanaryHostID, durationOrDays(p.cfg.CanaryValidity))
			if !ok {
				if prior, found := p.store.row(skill.Name, skill.Version); found && prior.Canary.Result == "failed" {
					p.refuseReason(ctx, skill.Name, skill.Version, types.CodeSkills013, ReasonCanaryFailed, rep)
					return newErr(types.CodeSkills013, ReasonCanaryFailed, "the canary run for this version failed")
				}
				p.hold(ctx, skill.Name, skill.Version, "", "")
				row, _ := p.store.row(skill.Name, skill.Version)
				row.Name, row.Version = skill.Name, skill.Version
				row.State = types.SkillCanaryBlocked
				row.SignerKeyID = skill.SignerKeyID
				row.Sigs = len(skill.Sigs)
				row.RefusalCode = string(types.CodeSkills012)
				row.Reason = ReasonCanaryNeedsApply
				p.store.putRow(row)
				_ = p.store.saveIndex()
				if rep != nil {
					rep.Held++
				}
				return newErr(types.CodeSkills012, ReasonCanaryNeedsApply,
					"no green canary from %s inside %s", p.cfg.CanaryHostID, p.cfg.CanaryValidity)
			}
			_ = rec
		}
	}
	// 6. approve policy.
	switch p.cfg.Approve {
	case "never":
		p.refuseReason(ctx, skill.Name, skill.Version, types.CodeSkills013, ReasonApproveNever, rep)
		return newErr(types.CodeSkills013, ReasonApproveNever, "approve=never refuses every pulled artifact")
	}
	// 7. version rules.
	if err := p.versionRules(ctx, skill, playBytes, rep); err != nil {
		p.refuse(ctx, skill.Name, skill.Version, err, rep)
		return err
	}
	return nil
}

// versionRules is §3.3/§4.5: same version with different canonical bytes is a
// conflict, a downgrade is refused, and a locally promoted name may not be taken
// over by the channel.
func (p *Puller) versionRules(ctx context.Context, skill types.Skill, playBytes []byte, rep *pullReport) error {
	// A locally promoted name may not be taken over by the channel: the channel's
	// int sequence is authoritative and a local int would eventually collide
	// (§3.3).
	if best, ok := p.store.HighestInstalled(skill.Name); ok && best.Origin == "local" && !p.store.NameOwnedByRelease(skill.Name) {
		p.conflict(ctx, skill, best, rep)
		return newErr(types.CodeSkills013, ReasonNameOwnedRelease,
			"%s was promoted locally, so the channel may not take it over", skill.Name)
	}
	// Same version, different canonical bytes: no winner is derivable, so both are
	// refused and the caller falls back (§4.5 rule 2).
	if prior, ok := p.store.row(skill.Name, skill.Version); ok {
		dir := filepath.Join(p.store.Dir(), filepath.FromSlash(prior.Dir))
		if pskill, pplay, _, err := readArtifact(dir); err == nil {
			priorCanon, e1 := CanonicalSHA256(pskill, pplay)
			incoming, e2 := CanonicalSHA256(skill, playBytes)
			if e1 == nil && e2 == nil && priorCanon != incoming {
				p.conflict(ctx, skill, prior, rep)
				return newErr(types.CodeSkills006, ReasonVersionAmbiguity,
					"%s@%d already exists with different canonical bytes (%s vs %s)",
					skill.Name, skill.Version, short(priorCanon), short(incoming))
			}
		}
	}
	if best, ok := p.store.HighestInstalled(skill.Name); ok && skill.Version < best.Version {
		return newErr(types.CodeSkills007, ReasonDowngrade,
			"%s@%d is below the highest installed version %d", skill.Name, skill.Version, best.Version)
	}
	return nil
}

// install writes one verified artifact: record first, then the atomic rename
// (§6 edge case 9: a failed ledger write rolls the install back).
func (p *Puller) install(ctx context.Context, skill types.Skill, playBytes []byte, stageDir, origin string) error {
	pending := p.cfg.Approve == "review"
	state := types.SkillInstalled
	bucket := "installed"
	if pending {
		state = types.SkillPendingReview
		bucket = "pending"
	}
	dst := filepath.Join(p.store.Dir(), bucket, skill.Name, fmt.Sprint(skill.Version))
	row, err := p.store.installRowFromSkill(skill, playBytes, filepath.Join(bucket, skill.Name, fmt.Sprint(skill.Version)), state, origin, canaryRecord{
		Result:        canaryResultFor(p, skill),
		HostID:        p.deps.hostID(),
		TS:            types.FormatUTC(p.deps.now()),
		ApplyMode:     false,
		DaemonVersion: p.deps.daemonVersion(),
	})
	if err != nil {
		return err
	}
	payload := map[string]any{
		"name": skill.Name, "version": skill.Version, "origin": origin,
		"signer_key_id": skill.SignerKeyID, "canonical_sha256": row.CanonicalSV,
		"play_sha256": row.PlaySHA, "sigs_sha256": row.SigsSHA,
		"canary": row.Canary, "state": state, "sigs": skill.Sigs,
		"provenance": skill.Provenance,
	}
	phase := PhaseInstalled
	if pending {
		phase = PhasePendingReview
	}
	if _, err := p.deps.phaseRecord(ctx, phase, "", "", payload); err != nil {
		return newErr(types.CodeSkills010, ReasonStatsWrite, "ledger write failed, install rolled back: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return newErr(types.CodeSkills010, ReasonPullRef, "install dir: %v", err)
	}
	_ = os.RemoveAll(dst)
	if err := os.Rename(stageDir, dst); err != nil {
		if err := copyDir(stageDir, dst); err != nil {
			return newErr(types.CodeSkills010, ReasonPullRef, "install %s@%d: %v", skill.Name, skill.Version, err)
		}
	}
	p.store.putRow(row)
	if err := p.store.saveIndex(); err != nil {
		return err
	}
	p.store.retentionPrune(skill.Name)
	_ = p.store.saveIndex()
	return nil
}

// canaryResultFor records whether this host is the canary (§4.4).
func canaryResultFor(p *Puller, skill types.Skill) string {
	if p.cfg.CanaryHostID == "" {
		return ""
	}
	if p.cfg.CanaryHostID == p.deps.hostID() {
		return "inconclusive"
	}
	return "green"
}

// Approve promotes a pending artifact to installed (the per-host approve policy).
func (p *Puller) Approve(name string, version int, actor types.Actor, allowDowngrade bool, overrideCanary bool, reason string) error {
	row, ok := p.store.row(name, version)
	if !ok {
		return newErr(types.CodeSkills007, ReasonPullRef, "%s@%d is not in the local index", name, version)
	}
	if p.cfg.Approve == "never" {
		return newErr(types.CodeSkills013, ReasonApproveNever, "approve=never forbids installing a pulled artifact")
	}
	if !allowDowngrade {
		if best, ok := p.store.HighestInstalled(name); ok && version < best.Version && best.State == types.SkillInstalled {
			return newErr(types.CodeSkills007, ReasonDowngrade,
				"%s@%d is below the installed %d (use --allow-downgrade)", name, version, best.Version)
		}
	}
	if row.State == types.SkillCanaryBlocked {
		if !overrideCanary {
			return newErr(types.CodeSkills012, ReasonCanaryNeedsApply,
				"%s@%d has no green canary; the override needs canary_override=true and a reason", name, version)
		}
		if !p.cfg.CanaryOverride {
			return newErr(types.CodeSkills013, ReasonCanaryNeedsApply,
				"canary_override is false on this host")
		}
		if strings.TrimSpace(reason) == "" {
			return newErr(types.CodeSkills013, ReasonCanaryNeedsApply, "the canary override needs a reason")
		}
		row.CanaryOverride = true
	}
	pending := filepath.Join(p.store.Dir(), filepath.FromSlash(row.Dir))
	installed := filepath.Join(p.store.Dir(), "installed", name, fmt.Sprint(version))
	if err := os.MkdirAll(filepath.Dir(installed), 0o700); err != nil {
		return newErr(types.CodeSkills014, ReasonStatsWrite, "install dir: %v", err)
	}
	_ = os.RemoveAll(installed)
	if err := os.Rename(pending, installed); err != nil {
		if cerr := copyDir(pending, installed); cerr != nil {
			return newErr(types.CodeSkills014, ReasonStatsWrite, "approve %s@%d: %v", name, version, err)
		}
	}
	row.State = types.SkillInstalled
	row.Dir = filepath.ToSlash(filepath.Join("installed", name, fmt.Sprint(version)))
	row.Refusals = 0
	row.RefusalCode = ""
	row.Reason = reason
	p.store.putRow(row)
	if err := p.store.saveIndex(); err != nil {
		return err
	}
	_, err := p.deps.phaseRecord(context.Background(), PhaseInstalled, "", "", map[string]any{
		"name": name, "version": version, "origin": row.Origin, "actor": actor.ID,
		"actor_kind": string(actor.Kind), "signer_key_id": row.SignerKeyID,
		"canonical_sha256": row.CanonicalSV, "canary": row.Canary,
		"canary_override": row.CanaryOverride, "reason": reason,
	})
	return err
}

// OverrideCanary is the per-host canary override (§4.4): it installs on this host
// only and never propagates.
func (p *Puller) OverrideCanary(name string, version int, actor types.Actor, reason string) error {
	return p.Approve(name, version, actor, false, true, reason)
}

// Reject records an operator rejection (§2 CLI).
func (p *Puller) Reject(name string, version int, actor types.Actor, reason string) error {
	p.store.recordRefusal(name, version, types.CodeSkills009, ReasonRejected)
	if err := p.store.saveIndex(); err != nil {
		return err
	}
	_, err := p.deps.phaseRecord(context.Background(), PhaseCandidateReviewed, "", "", map[string]any{
		"name": name, "version": version, "decision": "reject", "actor": actor.ID,
		"reason": reason, "error_code": string(types.CodeSkills009),
	})
	return err
}

// Reverify is the boot re-verification pass (§3.5).
func (p *Puller) Reverify(ctx context.Context) (int, error) {
	return p.store.reverify(ctx, func(row installRow, skill types.Skill, err error) error {
		// The row is authoritative for identity: a tree that fails to parse has no
		// Skill to read a name from, and an unnamed refusal wastes the audit trail.
		p.store.recordRefusal(row.Name, row.Version, types.CodeSkills002, ReasonTampered)
		_, rerr := p.deps.phaseRecord(ctx, PhaseRefused, "", "", map[string]any{
			"name": row.Name, "version": row.Version,
			"error_code": string(types.CodeSkills002), "reason": ReasonTampered,
			"count": 1, "detail": err.Error(),
		})
		return rerr
	})
}

// refuse records a refusal row and (rate-limited) a refusal ledger record.
func (p *Puller) refuse(ctx context.Context, name string, version int, err error, rep *pullReport) {
	code := CodeOf(err)
	reason := ReasonOf(err)
	if code == "" {
		code = types.CodeSkills001
	}
	p.store.recordRefusal(name, version, code, reason)
	_ = p.store.saveIndex()
	if rep != nil {
		rep.Refused++
	}
	p.emitRefusal(ctx, name, version, code, reason, "", "")
}

func (p *Puller) refuseReason(ctx context.Context, name string, version int, code types.ErrorCode, reason string, rep *pullReport) {
	p.store.recordRefusal(name, version, code, reason)
	_ = p.store.saveIndex()
	if rep != nil {
		rep.Refused++
	}
	p.emitRefusal(ctx, name, version, code, reason, "", "")
}

// emitRefusal writes the deduplicated refusal record (§4.7: one record per 24 h
// per (name, version, phase, code, reason) with the accumulated count).
func (p *Puller) emitRefusal(ctx context.Context, name string, version int, code types.ErrorCode, reason string, cause types.ErrorCode, detail string) {
	key := fmt.Sprintf("%s|%d|refused|%s|%s", name, version, code, reason)
	n, emit := p.store.noteRefusal(key)
	if !emit {
		return
	}
	payload := map[string]any{
		"name": name, "version": version, "error_code": string(code),
		"reason": reason, "count": n,
	}
	if cause != "" {
		payload["cause_error_code"] = string(cause)
	}
	if detail != "" {
		payload["detail"] = detail
	}
	_, _ = p.deps.phaseRecord(ctx, PhaseRefused, "", "", payload)
}

// conflict records §4.5's conflict shape.
func (p *Puller) conflict(ctx context.Context, skill types.Skill, winner installRow, rep *pullReport) {
	if rep != nil {
		rep.Refused++
	}
	_, _ = p.deps.phaseRecord(ctx, PhaseConflict, "", "", map[string]any{
		"sig":        strings.Join(skill.Sigs, ","),
		"winner":     map[string]any{"name": winner.Name, "version": winner.Version},
		"loser":      map[string]any{"name": skill.Name, "version": skill.Version},
		"count":      1,
		"error_code": string(types.CodeSkills006),
	})
}

// hold records §4.5 rule 3's hold.
func (p *Puller) hold(ctx context.Context, name string, version int, sig, windowEnd string) {
	h := p.store.OpenHold(sig, name, version, windowEnd)
	_, _ = p.deps.phaseRecord(ctx, PhaseHold, sig, "", map[string]any{
		"sig": sig, "held": map[string]any{"name": name, "version": version},
		"window_end": windowEnd, "count": 1, "held_ts": h.HeldTS,
	})
}

// ExpireHolds converts holds older than max_hold into refusals (§4.5 rule 3).
func (p *Puller) ExpireHolds(ctx context.Context) int {
	maxHold := p.cfg.MaxHold.Std()
	if maxHold <= 0 {
		maxHold = 30 * time.Minute
	}
	expired := p.store.ExpireHolds(maxHold)
	for _, h := range expired {
		p.store.recordRefusal(h.Name, h.Version, types.CodeSkills013, ReasonHoldExpired)
		_, _ = p.deps.phaseRecord(ctx, PhaseHoldExpired, h.Sig, "", map[string]any{
			"sig": h.Sig, "held": map[string]any{"name": h.Name, "version": h.Version},
			"window_end": h.WindowEnd, "count": 1,
			"error_code": string(types.CodeSkills013), "reason": ReasonHoldExpired,
		})
	}
	if len(expired) > 0 {
		_ = p.store.saveIndex()
	}
	return len(expired)
}

// fail records a pull failure and advances the backoff.
func (p *Puller) fail(ctx context.Context, rep pullReport, err error) {
	code := CodeOf(err)
	if code == "" {
		code = types.CodeSkills011
	}
	p.mu.Lock()
	p.state.Failures++
	failures := p.state.Failures
	p.state.LastError = err.Error()
	backoff := p.nextIntervalLocked()
	p.state.NextPullTS = types.FormatUTC(p.deps.now().Add(backoff))
	p.mu.Unlock()
	_ = p.saveState()
	_, _ = p.deps.phaseRecord(ctx, PhasePullFailed, "", "", map[string]any{
		"ref": p.cfg.SourceRef, "ref_mode": p.cfg.RefMode, "error_code": string(code),
		"failures": failures, "next_pull_ts": p.state.NextPullTS,
	})
}

func (p *Puller) nextIntervalLocked() time.Duration {
	base := p.cfg.PullInterval.Std()
	if base <= 0 {
		base = 15 * time.Minute
	}
	backoff := base
	for i := 0; i < p.state.Failures && backoff < 30*time.Minute; i++ {
		backoff *= 2
	}
	if backoff > 30*time.Minute {
		backoff = 30 * time.Minute
	}
	return backoff
}

func (p *Puller) markPull(sha string, failed bool) {
	p.mu.Lock()
	p.state.LastPullTS = types.FormatUTC(p.deps.now())
	p.state.LastPullSHA = sha
	if !failed {
		p.state.Failures = 0
		p.state.LastError = ""
	}
	next := p.cfg.PullInterval.Std()
	if next <= 0 {
		next = 15 * time.Minute
	}
	p.state.NextPullTS = types.FormatUTC(p.deps.now().Add(next))
	p.mu.Unlock()
	_ = p.saveState()
}

func (p *Puller) lastSHA() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state.LastPullSHA
}

func (p *Puller) saveState() error {
	raw, err := jsonMarshal(p.state)
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(p.store.Dir(), "pull-state.json"), raw)
}

func (p *Puller) registered() []string {
	if p.deps.Registered == nil {
		return nil
	}
	return p.deps.Registered()
}

// git runs one argv-only git command, never through a shell.
func (p *Puller) git(ctx context.Context, dir string, args ...string) error {
	_, err := p.gitOutput(ctx, dir, args...)
	return err
}

func (p *Puller) gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	binary := p.cfg.GitBinary
	if binary == "" {
		binary = "git"
	}
	p.mu.Lock()
	p.calls = append(p.calls, append([]string{binary}, args...))
	p.mu.Unlock()
	cmd := exec.CommandContext(ctx, binary, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return "", newErr(types.CodeSkills011, ReasonPullTimeout, "git %s exceeded pull_timeout", strings.Join(args, " "))
	}
	if err != nil {
		return out.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

func isGitMissing(err error) bool {
	return err != nil && strings.Contains(err.Error(), "executable file not found")
}

// copyDir is the fallback when a cross-device rename is not possible.
func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, path)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		return os.WriteFile(target, raw, 0o600)
	})
}

// ITagList is a test/diagnostic helper: the tags a source exposes, highest first.
func (p *Puller) ITagList(ctx context.Context, srcPath string) ([]string, error) {
	out, err := p.gitOutput(ctx, "", "ls-remote", "--tags", srcPath)
	if err != nil {
		return nil, err
	}
	var tags []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !strings.HasPrefix(fields[1], "refs/tags/") || strings.HasSuffix(fields[1], "^{}") {
			continue
		}
		tags = append(tags, strings.TrimPrefix(fields[1], "refs/tags/"))
	}
	sort.Strings(tags)
	return tags, nil
}

var _ = strconv.Itoa
