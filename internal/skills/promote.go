package skills

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// Promoter is the local candidate path of §3.4: draft → review → promote. In v0.1
// the promoted artifact stays on this host; the hand-off to the release channel is
// a (v1.0 hand-off) and `SkillCandidate.BranchOrPR` stays empty.
type Promoter struct {
	cfg   types.SkillsConfig
	store *Store
	deps  Deps
}

// NewPromoter builds the promoter over a store.
func NewPromoter(cfg types.SkillsConfig, store *Store, deps Deps) *Promoter {
	return &Promoter{cfg: cfg, store: store, deps: deps}
}

// Draft turns a verified incident fix into a candidate (§3.4). It refuses
// (TROUBLE-SKILLS-008) when the provenance would be empty, when the research rung
// returned without a brief id, or when the play cannot be rehearsed.
func (p *Promoter) Draft(inc types.Incident, res []types.ResearchOutcome, play types.Play) (types.SkillCandidate, error) {
	var cand types.SkillCandidate
	if inc.ID == "" || inc.Sig == "" {
		return cand, newErr(types.CodeSkills008, ReasonProvenance, "a candidate needs the incident that taught it")
	}
	if play.Name == "" || !nameRE.MatchString(play.Name) {
		return cand, newErr(types.CodeSkills008, ReasonFieldRule,
			"the play name %q is not a valid skill name", play.Name)
	}
	if !play.CheckMode {
		return cand, newErr(types.CodeSkills008, ReasonFieldRule,
			"the play is not check_mode-clean: a candidate must be rehearsable without applying")
	}
	research := make([]string, 0, len(res))
	for _, r := range res {
		if r.State == types.ResReturned {
			if r.ID == "" {
				return cand, newErr(types.CodeSkills008, ReasonProvenance,
					"the research rung returned without a brief id")
			}
			research = append(research, r.ID)
		}
	}
	if p.store.NameOwnedByRelease(play.Name) {
		return cand, newErr(types.CodeSkills013, ReasonNameOwnedRelease,
			"%s is owned by the release channel; a local promotion may not reuse the name", play.Name)
	}
	version := p.store.MaxVersion(play.Name) + 1
	id := types.NewID(types.PSk)
	cand = types.SkillCandidate{
		ID:         id,
		Name:       play.Name,
		Version:    version,
		Sig:        inc.Sig,
		Inc:        inc.ID,
		Play:       play,
		ResearchID: strings.Join(research, ","),
		State:      types.CandDrafted,
		CreatedTS:  types.FormatUTC(p.deps.now()),
	}
	dir := filepath.Join(p.store.Dir(), "candidates", id)
	if err := os.MkdirAll(filepath.Join(dir, "plays"), 0o700); err != nil {
		return cand, newErr(types.CodeSkills014, ReasonStatsWrite, "candidate dir: %v", err)
	}
	if err := p.writeDraft(cand, dir, research); err != nil {
		return cand, err
	}
	_, err := p.deps.phaseRecord(context.Background(), PhaseCandidateDrafted, inc.Sig, inc.ID, map[string]any{
		"name": cand.Name, "version": cand.Version, "inc": inc.ID, "sig": inc.Sig,
		"research_id": cand.ResearchID, "play_sha256": PlaySHA256([]byte(p.renderPlay(cand))),
		"state": cand.State, "candidate_id": id, "dir": dir,
	})
	return cand, err
}

// writeDraft renders the unsigned artifact draft and its play on disk. A draft is
// not an artifact: it carries no signature until Promote.
func (p *Promoter) writeDraft(c types.SkillCandidate, dir string, research []string) error {
	artifact := fmt.Sprintf(`name               = %q
version            = %d
sigs               = [%s]
play_ref           = %q
min_daemon_version = %q
allowed_modules    = [%s]
signer_key_id      = %q
signature          = ""

[guards]
verify_window = %q
max_runs      = "3/day"
escalate_on   = "verify_fail"

[provenance]
incidents  = [%q]
research   = [%s]
author     = %q
created_ts = %q
`,
		c.Name, c.Version, quoteJoin([]string{c.Sig}),
		fmt.Sprintf("plays/%s@%d.toml", c.Name, c.Version),
		p.deps.daemonVersion(), quoteJoin(playTools(c.Play)), "local-"+short(p.deps.hostID()),
		p.cfg.MaxHold, c.Inc, quoteJoin(research),
		p.deps.Actor.ID+"@"+p.deps.hostID(), types.FormatUTC(p.deps.now()))
	if err := os.WriteFile(filepath.Join(dir, "SKILL.toml"), []byte(artifact), 0o600); err != nil {
		return newErr(types.CodeSkills014, ReasonStatsWrite, "draft write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plays", fmt.Sprintf("%s@%d.toml", c.Name, c.Version)),
		[]byte(p.renderPlay(c)), 0o600); err != nil {
		return newErr(types.CodeSkills014, ReasonStatsWrite, "draft play write: %v", err)
	}
	return nil
}

// renderPlay re-renders the candidate's play in the SPEC-06 on-disk form.
func (p *Promoter) renderPlay(c types.SkillCandidate) string {
	var b strings.Builder
	fmt.Fprintf(&b, "schema_version = 1\nname        = %q\nversion     = %d\nsource      = %q\nmax_runs    = %d\n",
		c.Play.Name, c.Play.Version, "skill:"+c.ID, c.Play.MaxRuns)
	for _, t := range c.Play.Tasks {
		b.WriteString("\n[[task]]\n")
		fmt.Fprintf(&b, "name     = %q\n", t.Name)
		fmt.Fprintf(&b, "tool     = %q\n", t.Tool)
		fmt.Fprintf(&b, "args     = {%s}\n", renderArgs(t.Args))
		fmt.Fprintf(&b, "when     = %q\n", t.When)
		fmt.Fprintf(&b, "register = %q\n", t.Register)
		fmt.Fprintf(&b, "retries  = %d\n", t.Retries)
		fmt.Fprintf(&b, "on_fail  = %q\n", t.OnFail)
	}
	return b.String()
}

func renderArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sortStrings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		switch v := args[k].(type) {
		case string:
			parts = append(parts, fmt.Sprintf("%s = %q", k, v))
		case bool:
			parts = append(parts, fmt.Sprintf("%s = %t", k, v))
		case int:
			parts = append(parts, fmt.Sprintf("%s = %d", k, v))
		case int64:
			parts = append(parts, fmt.Sprintf("%s = %d", k, v))
		case float64:
			parts = append(parts, fmt.Sprintf("%s = %v", k, v))
		default:
			parts = append(parts, fmt.Sprintf("%s = %q", k, fmt.Sprint(v)))
		}
	}
	return strings.Join(parts, ", ")
}

// playTools lists the distinct tools a play calls.
func playTools(play types.Play) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range play.Tasks {
		if t.Tool != "" && !seen[t.Tool] {
			seen[t.Tool] = true
			out = append(out, t.Tool)
		}
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func quoteJoin(items []string) string {
	if len(items) == 0 {
		return ""
	}
	out := make([]string, 0, len(items))
	for _, i := range items {
		out = append(out, fmt.Sprintf("%q", i))
	}
	return strings.Join(out, ", ")
}

// Review records a review decision (§3.4). The default reviewer is human; in
// autonomy=full the caller may pass the daemon actor with autoAccept=true.
func (p *Promoter) Review(c types.SkillCandidate, actor types.Actor, decision, reason string, autoAccept bool) (types.SkillCandidate, error) {
	switch decision {
	case "approve", "accept":
		c.State = types.CandReviewed
		c.ReviewActor = actor.ID
	case "reject":
		c.State = types.CandRejected
		c.ReviewActor = actor.ID
	default:
		return c, newErr(types.CodeSkills008, ReasonValidation, "decision %q is not approve|reject", decision)
	}
	payload := map[string]any{
		"name": c.Name, "version": c.Version, "decision": decision,
		"actor": actor.ID, "reason": reason, "auto_accept": autoAccept,
		"candidate_id": c.ID,
	}
	if c.State == types.CandRejected {
		payload["error_code"] = string(types.CodeSkills009)
		// The play is not deleted (audit), but its hash is marked rejected and no
		// promotion may reuse it.
		p.store.recordRejectedHash(c.Name, c.Version, PlaySHA256([]byte(p.renderPlay(c))))
	}
	_, err := p.deps.phaseRecord(context.Background(), PhaseCandidateReviewed, c.Sig, c.Inc, payload)
	if err != nil {
		return c, err
	}
	if c.State == types.CandRejected {
		return c, newErr(types.CodeSkills009, ReasonRejected, "candidate %s was rejected at review", c.ID)
	}
	return c, nil
}

// Promote signs the candidate with the host's local key and installs it for this
// host only (§3.4).
func (p *Promoter) Promote(c types.SkillCandidate) (types.Skill, error) {
	var skill types.Skill
	if c.State != types.CandReviewed {
		return skill, newErr(types.CodeSkills008, ReasonValidation,
			"candidate %s is %s, not reviewed", c.ID, c.State)
	}
	hash := PlaySHA256([]byte(p.renderPlay(c)))
	if p.store.RejectedHash(c.Name, hash) {
		return skill, newErr(types.CodeSkills009, ReasonRejected,
			"this play content was rejected before and may not be promoted")
	}
	dir := filepath.Join(p.store.Dir(), "candidates", c.ID)
	raw, err := os.ReadFile(filepath.Join(dir, "SKILL.toml"))
	if err != nil {
		return skill, newErr(types.CodeSkills008, ReasonValidation, "candidate draft: %v", err)
	}
	playBytes, err := os.ReadFile(filepath.Join(dir, "plays", fmt.Sprintf("%s@%d.toml", c.Name, c.Version)))
	if err != nil {
		return skill, newErr(types.CodeSkills008, ReasonValidation, "candidate play: %v", err)
	}
	skill, err = LoadArtifact(raw, playBytes)
	if err != nil {
		return types.Skill{}, err
	}
	// Rehearsal proof: the play must be check_mode-clean (already asserted at
	// draft) and the module allowlist must hold against this build.
	if err := GateModules(skill, c.Play, p.registered()); err != nil {
		return types.Skill{}, err
	}
	signer, priv, err := p.localKey()
	if err != nil {
		return types.Skill{}, err
	}
	skill.SignerKeyID = signer.KeyID
	skill.Signature = ""
	canonical, err := CanonicalBytes(skill, playBytes)
	if err != nil {
		return types.Skill{}, err
	}
	sig := ed25519.Sign(priv, canonical)
	signed := base64.StdEncoding.EncodeToString(sig)
	// Rewrite the artifact with the signature in place.
	body := string(raw)
	if !strings.Contains(body, "signature") {
		return types.Skill{}, newErr(types.CodeSkills009, ReasonValidation, "draft has no signature key")
	}
	body = replaceSignatureLine(body, signed)
	if err := os.WriteFile(filepath.Join(dir, "SKILL.toml"), []byte(body), 0o600); err != nil {
		return types.Skill{}, newErr(types.CodeSkills014, ReasonStatsWrite, "sign draft: %v", err)
	}
	skill.Signature = signed
	reloaded, err := LoadArtifact([]byte(body), playBytes)
	if err != nil {
		return types.Skill{}, err
	}
	if err := GateSignature(reloaded, playBytes, p.cfg.Signers, false, "@"+p.deps.hostID()); err != nil {
		return types.Skill{}, err
	}
	installed := filepath.Join(p.store.Dir(), "installed", reloaded.Name, fmt.Sprint(reloaded.Version))
	if err := os.MkdirAll(filepath.Dir(installed), 0o700); err != nil {
		return types.Skill{}, newErr(types.CodeSkills014, ReasonStatsWrite, "install dir: %v", err)
	}
	_ = os.RemoveAll(installed)
	if err := copyDir(dir, installed); err != nil {
		return types.Skill{}, newErr(types.CodeSkills014, ReasonStatsWrite, "promote %s: %v", reloaded.Name, err)
	}
	row, err := p.store.installRowFromSkill(reloaded, playBytes,
		filepath.Join("installed", reloaded.Name, fmt.Sprint(reloaded.Version)), types.SkillInstalled, "local",
		canaryRecord{Result: "green", HostID: p.deps.hostID(), TS: types.FormatUTC(p.deps.now()), ApplyMode: true,
			DaemonVersion: p.deps.daemonVersion()})
	if err != nil {
		return types.Skill{}, err
	}
	row.SkillID = c.ID
	p.store.putRow(row)
	if err := p.store.saveIndex(); err != nil {
		return types.Skill{}, err
	}
	_, err = p.deps.phaseRecord(context.Background(), PhasePromoted, c.Sig, c.Inc, map[string]any{
		"name": reloaded.Name, "version": reloaded.Version, "sig": c.Sig,
		"signer_key_id": reloaded.SignerKeyID, "canonical_sha256": row.CanonicalSV,
		"candidate_id": c.ID, "origin": "local",
	})
	if err != nil {
		return reloaded, err
	}
	return reloaded, nil
}

func replaceSignatureLine(body, sig string) string {
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "signature") {
			lines[i] = fmt.Sprintf("signature          = %q", sig)
			return strings.Join(lines, "\n")
		}
	}
	return body + fmt.Sprintf("\nsignature          = %q\n", sig)
}

// localKey loads (or mints, 0600) the host's local ed25519 key and returns the
// matching signer entry. A local key is accepted only for artifacts whose author
// ends with @<this host_id> (§3.4).
func (p *Promoter) localKey() (types.SkillSigner, ed25519.PrivateKey, error) {
	path := filepath.Join(p.store.Dir(), "local.ed25519")
	if raw, err := os.ReadFile(path); err == nil && len(raw) == ed25519.PrivateKeySize {
		priv := ed25519.PrivateKey(raw)
		if s, ok := p.signerFor(priv); ok {
			return s, priv, nil
		}
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return types.SkillSigner{}, nil, newErr(types.CodeSkills014, ReasonStatsWrite, "keygen: %v", err)
	}
	if err := os.WriteFile(path, priv, 0o600); err != nil {
		return types.SkillSigner{}, nil, newErr(types.CodeSkills014, ReasonStatsWrite, "local key: %v", err)
	}
	pubB64 := base64.StdEncoding.EncodeToString(pub)
	// The key id must satisfy the artifact field rule
	// (^[a-z0-9][a-z0-9._-]{2,63}$), which excludes "@", even though SPEC-11's
	// status example renders a local id as "local@<host>". The field rule wins:
	// an artifact that cannot validate is not an artifact.
	entry := types.SkillSigner{
		KeyID:     "local-" + short(p.deps.hostID()),
		PublicKey: pubB64,
		Trust:     types.TrustLocal,
		Enabled:   true,
		AddedTS:   types.FormatUTC(p.deps.now()),
	}
	// The local signer joins the in-memory set so verification works without an
	// operator edit; the config file is the operator's business.
	p.cfg.Signers = append(p.cfg.Signers, entry)
	p.store.cfg.Signers = p.cfg.Signers
	return entry, priv, nil
}

func (p *Promoter) signerFor(priv ed25519.PrivateKey) (types.SkillSigner, bool) {
	pub := priv.Public().(ed25519.PublicKey)
	b64 := base64.StdEncoding.EncodeToString(pub)
	for _, s := range p.cfg.Signers {
		if s.PublicKey == b64 {
			return s, true
		}
	}
	entry := types.SkillSigner{KeyID: "local-" + short(p.deps.hostID()), PublicKey: b64,
		Trust: types.TrustLocal, Enabled: true, AddedTS: types.FormatUTC(p.deps.now())}
	p.cfg.Signers = append(p.cfg.Signers, entry)
	p.store.cfg.Signers = p.cfg.Signers
	return entry, true
}

func (p *Promoter) registered() []string {
	if p.deps.Registered == nil {
		return nil
	}
	return p.deps.Registered()
}

// AutoAcceptAllowed is the §4.6 auto-accept gate set. It returns the refusal
// reason when the candidate may not be auto-accepted.
func (p *Promoter) AutoAcceptAllowed(c types.SkillCandidate, gates types.AutonomyGates, rehearsals int, openConflict bool) (bool, string) {
	if gates.Mode != types.AutoFull || !gates.AllowSkillAccept {
		return false, "autonomy is not full with allow_skill_accept"
	}
	if !p.cfg.AutoAcceptEnabled {
		return false, "auto_accept_enabled is false"
	}
	allowed := map[string]bool{}
	for _, m := range p.cfg.AutoAcceptModules {
		allowed[m] = true
	}
	for _, t := range c.Play.Tasks {
		if !allowed[t.Tool] {
			return false, "the play calls " + t.Tool + ", which auto_accept_modules does not list"
		}
	}
	if p.store.RejectedHash(c.Name, PlaySHA256([]byte(p.renderPlay(c)))) {
		return false, "this play content was rejected before"
	}
	if openConflict {
		return false, "a conflict or refusal is open for the sig"
	}
	threshold := p.cfg.AutoAcceptThreshold
	if threshold < 1 {
		threshold = 1
	}
	if rehearsals < threshold {
		return false, fmt.Sprintf("%d clean rehearsals, %d needed", rehearsals, threshold)
	}
	return true, ""
}
