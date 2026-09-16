package skills

import (
	"context"
	"testing"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// TestTwoSkillsOneSigConflict pins §4.5 rule 1: the highest version wins and the
// loser is named — deterministically, with a conflict record.
func TestTwoSkillsOneSigConflict(t *testing.T) {
	kp := newKeyPair(t, "skills-2026", types.TrustRelease)
	f := newGitFixture(t)
	sig := "sentinel:sha256v1:9f2c1d3e4b5a6c7d"
	f.writeSkill("alpha-skill", 2, kp, []string{sig},
		[]string{"proc.connections", "service.reload", "config.set"}, "0.1.0")
	f.commit("alpha")
	f.tag("v1.0.0")
	f.writeSkill("beta-skill", 5, kp, []string{sig},
		[]string{"proc.connections", "service.reload", "config.set"}, "0.1.0")
	f.commit("beta")
	f.tag("v1.1.0")

	cfg := pullerCfg(f.bare(), kp)
	s, led, _ := newTestSkills(t, cfg, "proc.connections", "service.reload", "config.set")
	if _, err := s.PullOnce(context.Background()); err != nil {
		t.Fatalf("pull: %v", err)
	}
	res, err := s.Resolver.Match(sig)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if res.Name != "beta-skill" || res.Version != 5 {
		t.Fatalf("winner = %s@%d, want beta-skill@5", res.Name, res.Version)
	}
	if res.Loser != "alpha-skill@2" {
		t.Fatalf("loser = %q", res.Loser)
	}
	if !res.Conflict || res.Candidates != 2 {
		t.Fatalf("result = %+v", res)
	}
	// Selection is deterministic: the same call returns the same winner.
	again, _ := s.Resolver.Match(sig)
	if again.Name != res.Name || again.Version != res.Version {
		t.Fatalf("selection is not deterministic: %+v vs %+v", again, res)
	}
	// The loser is never applied while the winner is installed.
	installed := false
	for _, r := range s.Store.Rows() {
		if r.Name == "alpha-skill" && r.State == types.SkillInstalled {
			installed = true
		}
	}
	if !installed {
		t.Fatalf("the losing artifact was not installed as an artifact (only its application is blocked)")
	}
	if len(res.Skill.Sigs) == 0 || res.Play.Name == "" {
		t.Fatalf("the match carries no usable artifact: %+v", res)
	}
	if led.countPhase(PhaseConflict) == 0 {
		t.Fatalf("no conflict record: %v", led.phases())
	}
}

// TestHoldOnOverlappingWindows pins §4.5 rule 3: a skill is held while another
// skill's window is open for the sig, and the hold expires into a refusal.
func TestHoldOnOverlappingWindows(t *testing.T) {
	kp := newKeyPair(t, "skills-2026", types.TrustRelease)
	src, _ := seedChannel(t, kp)
	cfg := pullerCfg(src, kp)
	s, led, clk := newTestSkills(t, cfg, "proc.connections", "service.reload", "config.set")
	if _, err := s.PullOnce(context.Background()); err != nil {
		t.Fatalf("pull: %v", err)
	}
	sig := "sentinel:sha256v1:9f2c1d3e4b5a6c7d"
	open := []VerifyWindow{{
		Name: "other-skill", Version: 1, Sig: sig, Sigs: []string{sig, "journald:sha256v1:2ab4c6d8e0f1a3b5"},
		End: types.FormatUTC(clk.Now().Add(10 * 60 * 1000000000)),
	}}
	h, held := s.Resolver.Hold(sig, open)
	if !held {
		t.Fatalf("an open window covering the sig must hold the candidate")
	}
	if h.Name != "other-skill" || h.WindowEnd == "" {
		t.Fatalf("hold = %+v", h)
	}
	if len(led.byPhase(PhaseHold)) != 1 {
		t.Fatalf("no hold record: %v", led.phases())
	}
	// An unrelated sig is not held.
	if _, held := s.Resolver.Hold("journald:sha256v1:2ab4c6d8e0f1a3b5", nil); held {
		t.Fatalf("a sig with no open window must not be held")
	}
	// The hold expires into an explicit refusal, never a silent drop.
	clk.advance(31 * 60 * 1000000000)
	if n := s.Puller.ExpireHolds(context.Background()); n != 1 {
		t.Fatalf("expired %d holds, want 1", n)
	}
	var sawExpiry bool
	for _, r := range led.byPhase(PhaseHoldExpired) {
		if str(r.Payload, "reason") == ReasonHoldExpired {
			sawExpiry = true
		}
	}
	if !sawExpiry {
		t.Fatalf("no hold_expired record: %v", led.phases())
	}
	// A passed window releases the hold instead of expiring it.
	h2, _ := s.Resolver.Hold(sig, open)
	if h2.Name == "" {
		t.Fatalf("the window did not re-hold")
	}
	if _, held := s.Resolver.Hold(sig, []VerifyWindow{{Name: "other-skill", Version: 1, Sigs: []string{sig}, Passed: true, End: "x"}}); held {
		t.Fatalf("a passed window must not hold")
	}
}

// TestWindowFailureDemotesAtTheThreshold pins §4.5 rule 3: two consecutive verify
// failures demote the version and the sig escalates.
func TestWindowFailureDemotesAtTheThreshold(t *testing.T) {
	kp := newKeyPair(t, "skills-2026", types.TrustRelease)
	src, _ := seedChannel(t, kp)
	cfg := pullerCfg(src, kp)
	cfg.DemoteAfterFailures = 2
	s, led, _ := newTestSkills(t, cfg, "proc.connections", "service.reload", "config.set")
	if _, err := s.PullOnce(context.Background()); err != nil {
		t.Fatalf("pull: %v", err)
	}
	sig := "sentinel:sha256v1:9f2c1d3e4b5a6c7d"
	ev := types.Evidence{Result: types.VerifyFailed}
	if err := s.Resolver.RecordRun("payment-worker-queue-wedge", 3, false, ev, sig); err != nil {
		t.Fatalf("first failure: %v", err)
	}
	row, _ := s.Store.row("payment-worker-queue-wedge", 3)
	if row.State != types.SkillInstalled {
		t.Fatalf("one failure demoted the version: %s", row.State)
	}
	if err := s.Resolver.RecordRun("payment-worker-queue-wedge", 3, false, ev, sig); err != nil {
		t.Fatalf("second failure: %v", err)
	}
	row, _ = s.Store.row("payment-worker-queue-wedge", 3)
	if row.State != types.SkillQuarantined {
		t.Fatalf("state after two failures = %s, want quarantined", row.State)
	}
	if len(led.byPhase(PhaseDemoted)) != 1 {
		t.Fatalf("no demoted record: %v", led.phases())
	}
	var sawRefusal bool
	for _, r := range led.byPhase(PhaseRefused) {
		if str(r.Payload, "reason") == ReasonDemoted {
			sawRefusal = true
		}
	}
	if !sawRefusal {
		t.Fatalf("the demotion was not recorded as a refusal")
	}
	// A quarantined version is not resolvable: the sig escalates to the agent rung.
	res, err := s.Resolver.Match(sig)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if res.Candidates != 0 {
		t.Fatalf("a quarantined version still resolved: %+v", res)
	}
}

// TestMaxRunsBudget pins §4.5 rule 4.
func TestMaxRunsBudget(t *testing.T) {
	kp := newKeyPair(t, "skills-2026", types.TrustRelease)
	src, _ := seedChannel(t, kp)
	cfg := pullerCfg(src, kp)
	s, led, _ := newTestSkills(t, cfg, "proc.connections", "service.reload", "config.set")
	if _, err := s.PullOnce(context.Background()); err != nil {
		t.Fatalf("pull: %v", err)
	}
	sig := "sentinel:sha256v1:9f2c1d3e4b5a6c7d"
	// The fixture artifact allows 3/day.
	for i := 0; i < 3; i++ {
		ok, why := s.Resolver.AllowRun("payment-worker-queue-wedge", 3, sig)
		if !ok {
			t.Fatalf("run %d refused: %s", i, why)
		}
		if err := s.Resolver.RecordRun("payment-worker-queue-wedge", 3, true, types.Evidence{Result: types.VerifyPassed}, sig); err != nil {
			t.Fatalf("record run %d: %v", i, err)
		}
	}
	ok, why := s.Resolver.AllowRun("payment-worker-queue-wedge", 3, sig)
	if ok || why != ReasonMaxRuns {
		t.Fatalf("the fourth run must be refused with max_runs, got ok=%v why=%s", ok, why)
	}
	var sawMax bool
	for _, r := range led.byPhase(PhaseRefused) {
		if str(r.Payload, "reason") == ReasonMaxRuns {
			sawMax = true
		}
	}
	if !sawMax {
		t.Fatalf("no max_runs refusal record: %v", led.phases())
	}
	// Stats counted three applications and three successes.
	st := s.Store.Stats("payment-worker-queue-wedge", 3)
	if st.Applied != 3 || st.Success != 3 {
		t.Fatalf("stats = %+v", st)
	}
}
