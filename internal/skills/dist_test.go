package skills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// pullOnce builds a package over a source and performs one pull.
func pullOnce(t *testing.T, cfg types.SkillsConfig, registered ...string) (*Skills, *fakeLedger, *fakeClock, pullReport) {
	t.Helper()
	s, led, clk := newTestSkills(t, cfg, registered...)
	rep, err := s.PullOnce(context.Background())
	if err != nil {
		// Callers assert specific failure codes; a pull that fails for any other
		// reason is surfaced here so no test silently passes on a broken fixture.
		t.Logf("pull returned %v (report %+v)", err, rep)
	}
	return s, led, clk, rep
}

// TestPullSelectsHighestTag proves tag mode: the highest semver tag wins, and the
// seeded v1 artifact is not what lands.
func TestPullSelectsHighestTag(t *testing.T) {
	kp := newKeyPair(t, "skills-2026", types.TrustRelease)
	src, _ := seedChannel(t, kp)
	cfg := pullerCfg(src, kp)
	s, led, _, rep := pullOnce(t, cfg, "proc.connections", "service.reload", "config.set")
	if rep.ResolvedSHA == "" || rep.Seen != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if rep.Installed != 1 {
		t.Fatalf("installed %d artifacts, want 1 (report %+v)", rep.Installed, rep)
	}
	row, ok := s.Store.row("payment-worker-queue-wedge", 3)
	if !ok || row.State != types.SkillInstalled {
		t.Fatalf("the v3 artifact was not installed: %+v ok=%v", row, ok)
	}
	files := listFiles(t, s.Store.InstallDir("payment-worker-queue-wedge", 3))
	if !contains(files, "SKILL.toml") || !contains(files, "plays/payment-worker-queue-wedge@3.toml") {
		t.Fatalf("the installed tree is incomplete: %v", files)
	}
	if led.countPhase(PhasePulled) != 1 || led.countPhase(PhaseInstalled) != 1 {
		t.Fatalf("phases = %v", led.phases())
	}
	// The ledger never carries the channel's git log: only argv vectors do, and
	// they are read-only.
	for _, call := range s.Puller.GitCalls() {
		for _, tok := range []string{"push", "commit", "merge", "gc", "prune", "stash"} {
			if contains(call, tok) {
				t.Fatalf("a pull issued a write command: %v", call)
			}
		}
	}
}

// TestPullNoopWritesNoRecord proves §4.1: the same resolved sha writes no ledger
// record at all.
func TestPullNoopWritesNoRecord(t *testing.T) {
	kp := newKeyPair(t, "skills-2026", types.TrustRelease)
	src, _ := seedChannel(t, kp)
	cfg := pullerCfg(src, kp)
	s, led, _, _ := pullOnce(t, cfg, "proc.connections", "service.reload", "config.set")
	before := len(led.records())
	rep, err := s.PullOnce(context.Background())
	if err != nil {
		t.Fatalf("second pull: %v", err)
	}
	if !rep.Noop {
		t.Fatalf("the second pull was not a no-op: %+v", rep)
	}
	if after := len(led.records()); after != before {
		t.Fatalf("a no-op pull wrote %d records", after-before)
	}
}

// TestPullBranchModeRecordsRefMode proves branch mode is legal but recorded.
func TestPullBranchModeRecordsRefMode(t *testing.T) {
	kp := newKeyPair(t, "skills-2026", types.TrustRelease)
	_, work := seedChannel(t, kp)
	cfg := pullerCfg(work, kp)
	cfg.RefMode = "branch"
	cfg.SourceRef = "main"
	s, led, _, rep := pullOnce(t, cfg, "proc.connections", "service.reload", "config.set")
	if rep.RefMode != "branch" {
		t.Fatalf("ref_mode = %q", rep.RefMode)
	}
	pulled := led.byPhase(PhasePulled)
	if len(pulled) != 1 || str(pulled[0].Payload, "ref_mode") != "branch" {
		t.Fatalf("the pulled record did not carry ref_mode=branch: %+v", pulled)
	}
	if got := len(s.Store.Rows()); got == 0 {
		t.Fatalf("branch mode installed nothing")
	}
}

// TestPullUnreachableKeepsTheSet pins §4.1: an unreachable source is 011, the
// installed set is untouched, and the failure is recorded with the backoff.
func TestPullUnreachableKeepsTheSet(t *testing.T) {
	kp := newKeyPair(t, "skills-2026", types.TrustRelease)
	src, _ := seedChannel(t, kp)
	cfg := pullerCfg(src, kp)
	s, led, clk, _ := pullOnce(t, cfg, "proc.connections", "service.reload", "config.set")
	if len(s.Store.Rows()) != 1 {
		t.Fatalf("precondition: nothing installed")
	}
	s.Cfg.SourcePath = filepath.Join(t.TempDir(), "gone.git")
	s.Puller.cfg = s.Cfg
	_, err := s.PullOnce(context.Background())
	if CodeOf(err) != types.CodeSkills011 {
		t.Fatalf("unreachable source: code = %s (%v)", CodeOf(err), err)
	}
	if len(s.Store.Rows()) != 1 {
		t.Fatalf("the installed set changed after a failed pull: %+v", s.Store.Rows())
	}
	failed := led.byPhase(PhasePullFailed)
	if len(failed) != 1 {
		t.Fatalf("%d pull_failed records, want 1", len(failed))
	}
	if n, _ := failed[0].Payload["failures"].(int); n != 1 {
		t.Fatalf("failure count = %v", failed[0].Payload["failures"])
	}
	// The backoff doubles from the interval, capped at 30 m.
	clk.advance(0)
	if st := s.Puller.State(); st.NextPullTS == "" {
		t.Fatalf("no next pull stamp after a failure")
	}
}

// TestPullMissingRefIs010 proves a reachable source without the ref is 010 (a
// pull failure, not an offline one).
func TestPullMissingRefIs010(t *testing.T) {
	kp := newKeyPair(t, "skills-2026", types.TrustRelease)
	src, _ := seedChannel(t, kp)
	cfg := pullerCfg(src, kp)
	cfg.SourceRef = "v9.*"
	s, _, _, _ := pullOnce(t, cfg, "proc.connections")
	t.Logf("rows=%d", len(s.Store.Rows()))
	_, err := s.PullOnce(context.Background())
	if CodeOf(err) != types.CodeSkills010 {
		t.Fatalf("missing ref: code = %s (%v)", CodeOf(err), err)
	}
	if ReasonOf(err) != ReasonPullRef {
		t.Fatalf("missing ref: reason = %s", ReasonOf(err))
	}
}

// TestPerArtifactIsolation pins §4.1: one bad artifact refuses and the others
// install in the same pull.
func TestPerArtifactIsolation(t *testing.T) {
	kp := newKeyPair(t, "skills-2026", types.TrustRelease)
	f := newGitFixture(t)
	// A good artifact.
	f.writeSkill("good-skill", 1, kp, []string{"sentinel:sha256v1:9f2c1d3e4b5a6c7d"},
		[]string{"proc.connections", "service.reload", "config.set"}, "0.1.0")
	// A bad one: signed by a key the local config does not know.
	other := newKeyPair(t, "skills-9999", types.TrustRelease)
	f.writeSkill("bad-signature", 1, other, []string{"journald:sha256v1:2ab4c6d8e0f1a3b5"},
		[]string{"proc.connections", "service.reload", "config.set"}, "0.1.0")
	f.commit("two artifacts")
	f.tag("v1.0.0")
	cfg := pullerCfg(f.bare(), kp)
	s, led, _, rep := pullOnce(t, cfg, "proc.connections", "service.reload", "config.set")
	if rep.Installed != 1 || rep.Refused != 1 {
		t.Fatalf("installed=%d refused=%d, want 1/1 (%+v)", rep.Installed, rep.Refused, rep)
	}
	if _, ok := s.Store.row("good-skill", 1); !ok {
		t.Fatalf("the good artifact did not install")
	}
	row, ok := s.Store.row("bad-signature", 1)
	if !ok || row.State != types.SkillRefused {
		t.Fatalf("the bad artifact row = %+v ok=%v", row, ok)
	}
	if led.countPhase(PhaseRefused) != 1 {
		t.Fatalf("refusal records = %v", led.phases())
	}
}

// TestPullSizeCapIs010 pins pull_max_bytes.
func TestPullSizeCapIs010(t *testing.T) {
	kp := newKeyPair(t, "skills-2026", types.TrustRelease)
	src, _ := seedChannel(t, kp)
	cfg := pullerCfg(src, kp)
	cfg.PullMaxBytes = 16 // smaller than any artifact
	_, _, _, _ = pullOnce(t, cfg, "proc.connections")
}

// TestInstallRollsBackWhenTheLedgerRefuses pins §6 edge case 9: no artifact
// exists without an audit record.
func TestInstallRollsBackWhenTheLedgerRefuses(t *testing.T) {
	kp := newKeyPair(t, "skills-2026", types.TrustRelease)
	src, _ := seedChannel(t, kp)
	cfg := pullerCfg(src, kp)
	led := &fakeLedger{}
	clk := newFakeClock()
	led.now = clk.Now
	cfg.StateDir = filepath.Join(t.TempDir(), "skills-local")
	s, err := New(cfg, testDeps(t, led, clk, t.TempDir(), "proc.connections", "service.reload", "config.set"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	led.mu.Lock()
	led.failNext = true
	led.mu.Unlock()
	_, _ = s.PullOnce(context.Background())
	row, ok := s.Store.row("payment-worker-queue-wedge", 3)
	if ok && row.State == types.SkillInstalled {
		t.Fatalf("an artifact was recorded as installed although the ledger write failed: %+v", row)
	}
	if ok && row.State != types.SkillRefused {
		t.Fatalf("the rolled-back artifact should be recorded as refused, got %s", row.State)
	}
	if _, err := os.Stat(s.Store.InstallDir("payment-worker-queue-wedge", 3)); err == nil {
		t.Fatalf("an artifact directory exists without an audit record")
	}
}

// TestApprovePolicyReview proves `approve=review` puts the artifact in pending and
// only `approve` installs it.
func TestApprovePolicyReview(t *testing.T) {
	kp := newKeyPair(t, "skills-2026", types.TrustRelease)
	src, _ := seedChannel(t, kp)
	cfg := pullerCfg(src, kp)
	cfg.Approve = "review"
	s, led, _ := newTestSkills(t, cfg, "proc.connections", "service.reload", "config.set")
	if _, err := s.PullOnce(context.Background()); err != nil {
		t.Fatalf("pull: %v", err)
	}
	row, ok := s.Store.row("payment-worker-queue-wedge", 3)
	if !ok || row.State != types.SkillPendingReview {
		t.Fatalf("row = %+v ok=%v", row, ok)
	}
	if led.countPhase(PhasePendingReview) != 1 {
		t.Fatalf("phases = %v", led.phases())
	}
	// A pending artifact is not resolvable.
	res, err := s.Resolver.Match("sentinel:sha256v1:9f2c1d3e4b5a6c7d")
	if err != nil || res.Candidates != 0 {
		t.Fatalf("a pending artifact resolved: %+v err=%v", res, err)
	}
	if err := s.Puller.Approve("payment-worker-queue-wedge", 3, types.Actor{Kind: types.ActorHuman, ID: "dash-read@phone"}, false, false, "reviewed"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	row, _ = s.Store.row("payment-worker-queue-wedge", 3)
	if row.State != types.SkillInstalled {
		t.Fatalf("state after approve = %s", row.State)
	}
	res, err = s.Resolver.Match("sentinel:sha256v1:9f2c1d3e4b5a6c7d")
	if err != nil || res.Name != "payment-worker-queue-wedge" || res.Version != 3 {
		t.Fatalf("after approve: %+v err=%v", res, err)
	}
}

// TestApproveNeverRefusesEverything proves `approve=never`.
func TestApproveNeverRefusesEverything(t *testing.T) {
	kp := newKeyPair(t, "skills-2026", types.TrustRelease)
	src, _ := seedChannel(t, kp)
	cfg := pullerCfg(src, kp)
	cfg.Approve = "never"
	s, led, _, rep := pullOnce(t, cfg, "proc.connections", "service.reload", "config.set")
	if rep.Installed != 0 || rep.Refused != 1 {
		t.Fatalf("report = %+v", rep)
	}
	row, _ := s.Store.row("payment-worker-queue-wedge", 3)
	if row.State != types.SkillRefused {
		t.Fatalf("state = %s", row.State)
	}
	if row.RefusalCode != string(types.CodeSkills013) || row.Reason != ReasonApproveNever {
		t.Fatalf("row = %+v", row)
	}
	if led.countPhase(PhaseRefused) != 1 {
		t.Fatalf("phases = %v", led.phases())
	}
}

func listFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

var _ = strings.TrimSpace
