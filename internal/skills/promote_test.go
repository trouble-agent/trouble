package skills

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// TestAC24LocalPromoteThenPullDown is AC-24's skills half: host A's agent fix
// becomes a signed artifact with full provenance, the release channel carries it
// (the v1.0 hand-off), and hosts B and C pull it and resolve the sig to it.
//
// The ladder's rung-② run and the "zero tokens" claim live in SPEC-05/SPEC-06's
// suites; what this test proves is that the artifact and its provenance arrive
// intact and that B never needed an agent run of its own to get there.
func TestAC24LocalPromoteThenPullDown(t *testing.T) {
	// ── host A: draft → review → promote ────────────────────────────────────
	localKP := newKeyPair(t, "local", types.TrustLocal)
	cfgA := DefaultConfig()
	cfgA.SourcePath = "/nonexistent"
	cfgA.Signers = []types.SkillSigner{localKP.Signer}
	cfgA.StateDir = filepath.Join(t.TempDir(), "A", "skills-local")
	a, ledA, _ := newTestSkills(t, cfgA, "proc.connections", "service.reload", "config.set")

	play, err := DecodePlay([]byte(replaceOnce(playFixture, "payment-worker-queue-wedge", "queue-wedge-fix")))
	if err != nil {
		t.Fatalf("play: %v", err)
	}
	inc := types.Incident{ID: "inc_8812", Sig: "sentinel:sha256v1:9f2c1d3e4b5a6c7d"}
	brief := types.ResearchOutcome{ID: "res_2214", State: types.ResReturned, Inc: inc.ID}
	cand, err := a.Promoter.Draft(inc, []types.ResearchOutcome{brief}, play)
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	if cand.State != types.CandDrafted || cand.Version != 1 {
		t.Fatalf("candidate = %+v", cand)
	}
	if cand.ResearchID != "res_2214" {
		t.Fatalf("the brief link was dropped: %+v", cand)
	}
	if _, err := a.Promoter.Review(cand, types.Actor{Kind: types.ActorHuman, ID: "dash-write@phone"}, "approve", "reviewed", false); err != nil {
		t.Fatalf("review: %v", err)
	}
	// A daemon actor must not be able to auto-accept in shadow/assisted.
	if ok, why := a.Promoter.AutoAcceptAllowed(cand, types.AutonomyGates{Mode: types.AutoShadow}, 9, false); ok {
		t.Fatalf("auto-accept passed in shadow mode: %s", why)
	}
	cand.State = types.CandReviewed
	art, err := a.Promoter.Promote(cand)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if art.Signature == "" || !strings.HasPrefix(art.SignerKeyID, "local-") {
		t.Fatalf("the promoted artifact is not signed by the local key: %+v", art)
	}
	row, ok := a.Store.row(art.Name, art.Version)
	if !ok || row.State != types.SkillInstalled || row.Origin != "local" {
		t.Fatalf("promoted row = %+v ok=%v", row, ok)
	}
	if !contains(row.Provenance, "inc_8812") || !contains(row.Provenance, "res_2214") {
		t.Fatalf("the install row lost its provenance: %v", row.Provenance)
	}
	if ledA.countPhase(PhaseCandidateDrafted) != 1 || ledA.countPhase(PhaseCandidateReviewed) != 1 || ledA.countPhase(PhasePromoted) != 1 {
		t.Fatalf("phases = %v", ledA.phases())
	}

	// ── the release channel: the merge/review step signs with the release key ─
	releaseKP := newKeyPair(t, "skills-2026", types.TrustRelease)
	channel := newGitFixture(t)
	installedDir := a.Store.InstallDir(art.Name, art.Version)
	skill, playBytesIn, _, err := readArtifact(installedDir)
	if err != nil {
		t.Fatalf("read the promoted artifact: %v", err)
	}
	dest := filepath.Join(channel.dir, "skills", art.Name)
	if err := os.MkdirAll(filepath.Join(dest, "plays"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// The channel's maintainer signs what review approved: the author stays the
	// promoting host, the signing key becomes the release key.
	body := strings.Replace(renderArtifactTOML(skill), skill.SignerKeyID, releaseKP.Signer.KeyID, 1)
	_, signedRelease := releaseKP.sign(t, body, playBytesIn)
	if err := os.WriteFile(filepath.Join(dest, "SKILL.toml"), []byte(signedRelease), 0o644); err != nil {
		t.Fatalf("write channel artifact: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dest, "plays", filepath.Base(skill.PlayRef)), playBytesIn, 0o644); err != nil {
		t.Fatalf("write channel play: %v", err)
	}
	channel.commit("merge the promoted fix")
	channel.tag("v1.0.0")

	// ── hosts B and C pull it ───────────────────────────────────────────────
	for _, host := range []string{"B", "C"} {
		cfg := pullerCfg(channel.bare(), releaseKP)
		cfg.StateDir = filepath.Join(t.TempDir(), host, "skills-local")
		b, ledB, _ := newTestSkills(t, cfg, "proc.connections", "service.reload", "config.set")
		if _, err := b.PullOnce(context.Background()); err != nil {
			t.Fatalf("host %s pull: %v", host, err)
		}
		res, err := b.Resolver.Match("sentinel:sha256v1:9f2c1d3e4b5a6c7d")
		if err != nil {
			t.Fatalf("host %s match: %v", host, err)
		}
		if res.Name != art.Name || res.Version != art.Version {
			t.Fatalf("host %s resolved %s@%d, want %s@%d", host, res.Name, res.Version, art.Name, art.Version)
		}
		if res.Play.Source == "" {
			t.Fatalf("host %s: the play carries no source", host)
		}
		// Provenance arrives intact, incident visible in the install record.
		records := ledB.byPhase(PhaseInstalled)
		if len(records) != 1 {
			t.Fatalf("host %s: %d install records", host, len(records))
		}
		prov, _ := records[0].Payload["provenance"].(types.Provenance)
		if !contains(prov.Incidents, "inc_8812") {
			t.Fatalf("host %s: the incident is not in the installed record's provenance: %+v", host, records[0].Payload["provenance"])
		}
		// Zero agent runs on this host: the desk's own ledger carries no such kind.
		for _, r := range ledB.records() {
			if r.Kind == types.KAgentRun {
				t.Fatalf("host %s wrote an agent_run record", host)
			}
		}
	}
}

// renderArtifactTOML renders an installed Skill back to TOML with an empty
// signature, so the channel copy can be re-signed.
func renderArtifactTOML(s types.Skill) string {
	var b strings.Builder
	fmt.Fprintf(&b, "name               = %q\n", s.Name)
	fmt.Fprintf(&b, "version            = %d\n", s.Version)
	fmt.Fprintf(&b, "sigs               = [%s]\n", quoteJoin(s.Sigs))
	fmt.Fprintf(&b, "play_ref           = %q\n", s.PlayRef)
	fmt.Fprintf(&b, "min_daemon_version = %q\n", s.MinDaemonVersion)
	fmt.Fprintf(&b, "allowed_modules    = [%s]\n", quoteJoin(s.AllowedModules))
	fmt.Fprintf(&b, "signer_key_id      = %q\n", s.SignerKeyID)
	b.WriteString("signature          = \"\"\n\n")
	fmt.Fprintf(&b, "[guards]\nverify_window = %q\nmax_runs      = %q\nescalate_on   = %q\n\n",
		s.Guards.VerifyWindow, s.Guards.MaxRuns, s.Guards.EscalateOn)
	fmt.Fprintf(&b, "[provenance]\nincidents  = [%s]\nresearch   = [%s]\nauthor     = %q\ncreated_ts = %q\n",
		quoteJoin(s.Provenance.Incidents), quoteJoin(s.Provenance.Research), s.Provenance.Author, s.Provenance.CreatedTS)
	return b.String()
}

// TestAutoAcceptGateSet is the §4.6/AC-26 auto-accept matrix.
func TestAutoAcceptGateSet(t *testing.T) {
	kp := newKeyPair(t, "local", types.TrustLocal)
	cfg := DefaultConfig()
	cfg.SourcePath = "/nonexistent"
	cfg.Signers = []types.SkillSigner{kp.Signer}
	cfg.AutoAcceptEnabled = true
	cfg.AutoAcceptThreshold = 1
	cfg.AutoAcceptModules = []string{"proc.connections", "service.reload", "config.set"}
	cfg.StateDir = filepath.Join(t.TempDir(), "skills-local")
	s, _, _ := newTestSkills(t, cfg, "proc.connections", "service.reload", "config.set")
	play, err := DecodePlay([]byte(replaceOnce(playFixture, "payment-worker-queue-wedge", "auto-skill")))
	if err != nil {
		t.Fatalf("play: %v", err)
	}
	cand := types.SkillCandidate{ID: "sk_x", Name: "auto-skill", Version: 1, Sig: "sentinel:sha256v1:9f2c1d3e4b5a6c7d",
		Inc: "inc_8812", Play: play, State: types.CandReviewed}

	full := types.AutonomyGates{Mode: types.AutoFull, AllowSkillAccept: true}
	if ok, why := s.Promoter.AutoAcceptAllowed(cand, full, 1, false); !ok {
		t.Fatalf("the full gate set must allow auto-accept: %s", why)
	}
	if ok, _ := s.Promoter.AutoAcceptAllowed(cand, full, 0, false); ok {
		t.Fatalf("threshold 1 with 0 rehearsals must not auto-accept")
	}
	if ok, _ := s.Promoter.AutoAcceptAllowed(cand, types.AutonomyGates{Mode: types.AutoFull}, 5, false); ok {
		t.Fatalf("allow_skill_accept=false must refuse")
	}
	if ok, _ := s.Promoter.AutoAcceptAllowed(cand, types.AutonomyGates{Mode: types.AutoAssisted, AllowSkillAccept: true}, 5, false); ok {
		t.Fatalf("autonomy=assisted must refuse")
	}
	if ok, _ := s.Promoter.AutoAcceptAllowed(cand, full, 5, true); ok {
		t.Fatalf("an open conflict must refuse")
	}
	// A play outside auto_accept_modules never auto-accepts.
	s.Promoter.cfg.AutoAcceptModules = []string{"proc.top"}
	if ok, why := s.Promoter.AutoAcceptAllowed(cand, full, 5, false); ok {
		t.Fatalf("a play outside auto_accept_modules auto-accepted")
	} else if !strings.Contains(why, "auto_accept_modules") {
		t.Fatalf("reason = %s", why)
	}
	// Threshold 2 needs two clean rehearsals.
	s.Promoter.cfg.AutoAcceptModules = []string{"proc.connections", "service.reload", "config.set"}
	s.Promoter.cfg.AutoAcceptThreshold = 2
	if ok, _ := s.Promoter.AutoAcceptAllowed(cand, full, 1, false); ok {
		t.Fatalf("threshold 2 with one rehearsal must refuse")
	}
	if ok, _ := s.Promoter.AutoAcceptAllowed(cand, full, 2, false); !ok {
		t.Fatalf("threshold 2 with two rehearsals must pass")
	}
	// A rejected play content hash is never auto-accepted.
	hash := PlaySHA256([]byte(s.Promoter.renderPlay(cand)))
	s.Store.recordRejectedHash(cand.Name, cand.Version, hash)
	if ok, why := s.Promoter.AutoAcceptAllowed(cand, full, 5, false); ok {
		t.Fatalf("a rejected play hash auto-accepted")
	} else if !strings.Contains(why, "rejected") {
		t.Fatalf("reason = %s", why)
	}
}

// TestDraftRefusesWithoutProvenance pins TROUBLE-SKILLS-008 and the check_mode
// requirement.
func TestDraftRefusesWithoutProvenance(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SourcePath = "/nonexistent"
	s, _, _ := newTestSkills(t, cfg, "proc.connections", "service.reload", "config.set")
	play, err := DecodePlay([]byte(replaceOnce(playFixture, "payment-worker-queue-wedge", "no-prov")))
	if err != nil {
		t.Fatalf("play: %v", err)
	}
	if _, err := s.Promoter.Draft(types.Incident{}, nil, play); CodeOf(err) != types.CodeSkills008 {
		t.Fatalf("an incident-less draft must be 008, got %s", CodeOf(err))
	}
	// A research rung that returned without an id is provenance loss.
	inc := types.Incident{ID: "inc_8812", Sig: "sentinel:sha256v1:9f2c1d3e4b5a6c7d"}
	bad := types.ResearchOutcome{State: types.ResReturned}
	if _, err := s.Promoter.Draft(inc, []types.ResearchOutcome{bad}, play); CodeOf(err) != types.CodeSkills008 {
		t.Fatalf("a brief without an id must be 008, got %s", CodeOf(err))
	}
	// A play that is not check_mode-clean cannot be rehearsed.
	noCheck := play
	noCheck.CheckMode = false
	if _, err := s.Promoter.Draft(inc, nil, noCheck); CodeOf(err) != types.CodeSkills008 {
		t.Fatalf("a non-check_mode play must be 008, got %s", CodeOf(err))
	}
	// A rejected candidate is recorded with 009 and its hash is marked.
	good, err := s.Promoter.Draft(inc, nil, play)
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	_, err = s.Promoter.Review(good, types.Actor{Kind: types.ActorHuman, ID: "dash-write@phone"}, "reject", "not safe", false)
	if CodeOf(err) != types.CodeSkills009 {
		t.Fatalf("rejection: %s", CodeOf(err))
	}
	if _, err := s.Promoter.Promote(good); CodeOf(err) == "" {
		t.Fatalf("a rejected candidate must not promote")
	}
}
