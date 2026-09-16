package skills

import (
	"context"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// gateFixture builds a package plus a signed artifact the gate can be called on
// directly, so gate ORDER can be asserted without a git round trip.
func gateFixture(t *testing.T, cfg types.SkillsConfig, kp keyPair, toml string) (*Skills, *fakeLedger, types.Skill, []byte) {
	t.Helper()
	s, led, _ := newTestSkills(t, cfg, "proc.connections", "service.reload", "config.set")
	_, signed := kp.sign(t, toml, playBytes())
	skill, err := LoadArtifact([]byte(signed), playBytes())
	if err != nil {
		t.Fatalf("artifact: %v", err)
	}
	return s, led, skill, playBytes()
}

// TestGateOrder pins §4.2's first-failure-wins ordering by pairing defects.
func TestGateOrder(t *testing.T) {
	kp := newKeyPair(t, "skills-2026", types.TrustRelease)

	t.Run("schema before signature", func(t *testing.T) {
		// An unknown key plus a broken signature: the parse refuses first.
		_, err := LoadArtifact([]byte(goldenArtifactTOML+"\ncmd = \"sh\"\n"), playBytes())
		if CodeOf(err) != types.CodeSkills001 {
			t.Fatalf("code = %s (%v)", CodeOf(err), err)
		}
	})

	t.Run("signature before floor", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.SourcePath = "/nonexistent"
		cfg.Signers = []types.SkillSigner{newKeyPair(t, "other-key", types.TrustRelease).Signer}
		// The artifact is signed by skills-2026 (absent from the config) and asks
		// for a floor this daemon cannot meet.
		toml := replaceOnce(goldenArtifactTOML, `min_daemon_version = "0.1.0"`, `min_daemon_version = "9.9.9"`)
		s, _, _, _ := gateFixture(t, cfg, kp, toml)
		_, signed := kp.sign(t, toml, playBytes())
		skill, err := LoadArtifact([]byte(signed), playBytes())
		if err != nil {
			t.Fatalf("artifact: %v", err)
		}
		err = s.Puller.gate(context.Background(), skill, playBytes(), []string{"proc.connections"}, &pullReport{})
		if CodeOf(err) != types.CodeSkills003 {
			t.Fatalf("the signature gate must fire first: code = %s (%v)", CodeOf(err), err)
		}
	})

	t.Run("floor before canary", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.SourcePath = "/nonexistent"
		cfg.Signers = []types.SkillSigner{kp.Signer}
		cfg.CanaryHostID = "c0ffee1234567890"
		toml := replaceOnce(goldenArtifactTOML, `min_daemon_version = "0.1.0"`, `min_daemon_version = "9.9.9"`)
		s, led, _, _ := gateFixture(t, cfg, kp, toml)
		_, signed := kp.sign(t, toml, playBytes())
		skill, _ := LoadArtifact([]byte(signed), playBytes())
		err := s.Puller.gate(context.Background(), skill, playBytes(), []string{"proc.connections", "service.reload", "config.set"}, &pullReport{})
		if CodeOf(err) != types.CodeSkills013 || ReasonOf(err) != ReasonFloor {
			t.Fatalf("floor gate: %s/%s (%v)", CodeOf(err), ReasonOf(err), err)
		}
		if len(led.byPhase(PhaseRefused)) != 1 {
			t.Fatalf("the floor refusal wrote no record: %v", led.phases())
		}
		if got := str(led.byPhase(PhaseRefused)[0].Payload, "cause_error_code"); got != string(types.CodeSkills004) {
			t.Fatalf("the floor refusal must name the cause code, got %q", got)
		}
	})

	t.Run("canary before approve", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.SourcePath = "/nonexistent"
		cfg.Signers = []types.SkillSigner{kp.Signer}
		cfg.CanaryHostID = "c0ffee1234567890"
		cfg.Approve = "never"
		s, led, _, _ := gateFixture(t, cfg, kp, goldenArtifactTOML)
		_, signed := kp.sign(t, goldenArtifactTOML, playBytes())
		skill, _ := LoadArtifact([]byte(signed), playBytes())
		err := s.Puller.gate(context.Background(), skill, playBytes(), []string{"proc.connections", "service.reload", "config.set"}, &pullReport{})
		if CodeOf(err) != types.CodeSkills012 {
			t.Fatalf("the canary gate must fire before approve: %s (%v)", CodeOf(err), err)
		}
		if row, _ := s.Store.row(skill.Name, skill.Version); row.State != types.SkillCanaryBlocked {
			t.Fatalf("state = %q, want canary_blocked", row.State)
		}
		// A held artifact is retried at the next pull, not refused.
		if len(led.byPhase(PhaseHold)) != 1 {
			t.Fatalf("no hold record: %v", led.phases())
		}
	})

	t.Run("approve before conflict", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.SourcePath = "/nonexistent"
		cfg.Signers = []types.SkillSigner{kp.Signer}
		cfg.Approve = "never"
		s, led, _, _ := gateFixture(t, cfg, kp, goldenArtifactTOML)
		_, signed := kp.sign(t, goldenArtifactTOML, playBytes())
		skill, _ := LoadArtifact([]byte(signed), playBytes())
		// A locally promoted row under the same name (a conflict if reached).
		s.Store.putRow(installRow{Name: skill.Name, Version: skill.Version, State: types.SkillInstalled, Origin: "local"})
		err := s.Puller.gate(context.Background(), skill, playBytes(), []string{"proc.connections", "service.reload", "config.set"}, &pullReport{})
		if ReasonOf(err) != ReasonApproveNever {
			t.Fatalf("approve gate must fire before the conflict rule: %s/%s", CodeOf(err), ReasonOf(err))
		}
		if len(led.byPhase(PhaseConflict)) != 0 {
			t.Fatalf("the conflict rule ran before the approve policy")
		}
	})
}

// TestRefusalRecordsAreDeduplicated pins §4.7: a permanently refused artifact on
// every pull interval produces one record per 24 h with the accumulated count.
func TestRefusalRecordsAreDeduplicated(t *testing.T) {
	kp := newKeyPair(t, "skills-2026", types.TrustRelease)
	cfg := DefaultConfig()
	cfg.SourcePath = "/nonexistent"
	cfg.Signers = []types.SkillSigner{kp.Signer}
	toml := replaceOnce(goldenArtifactTOML, `min_daemon_version = "0.1.0"`, `min_daemon_version = "9.9.9"`)
	s, led, clk := newTestSkills(t, cfg, "proc.connections", "service.reload", "config.set")
	_, signed := kp.sign(t, toml, playBytes())
	skill, err := LoadArtifact([]byte(signed), playBytes())
	if err != nil {
		t.Fatalf("artifact: %v", err)
	}
	for i := 0; i < 10; i++ {
		_ = s.Puller.gate(context.Background(), skill, playBytes(), []string{"proc.connections", "service.reload", "config.set"}, &pullReport{})
	}
	refusals := led.byPhase(PhaseRefused)
	if len(refusals) != 1 {
		t.Fatalf("%d refusal records over 10 pulls, want 1 inside the 24h window", len(refusals))
	}
	if s.Store.RefusalCount("payment-worker-queue-wedge|3|refused|TROUBLE-SKILLS-013|floor") != 10 {
		t.Fatalf("the store did not accumulate the refusal count: %d", s.Store.RefusalCount("payment-worker-queue-wedge|3|refused|TROUBLE-SKILLS-013|floor"))
	}
	// After 24 h the record is re-emitted with everything accumulated since.
	clk.advance(25 * time.Hour)
	_ = s.Puller.gate(context.Background(), skill, playBytes(), []string{"proc.connections", "service.reload", "config.set"}, &pullReport{})
	refusals = led.byPhase(PhaseRefused)
	if len(refusals) != 2 {
		t.Fatalf("%d refusal records after the window, want 2", len(refusals))
	}
	if count, _ := refusals[1].Payload["count"].(int); count != 11 {
		t.Fatalf("re-emitted count = %v, want 11", refusals[1].Payload["count"])
	}
	// Nothing was ever dropped: 11 occurrences, 2 records, 11 accumulated.
	if got := s.Store.RefusalCount("payment-worker-queue-wedge|3|refused|TROUBLE-SKILLS-013|floor"); got != 11 {
		t.Fatalf("accumulated count = %d, want 11", got)
	}
}

// TestCanaryGateRequiresAGreenResult pins §4.4 on a non-canary host.
func TestCanaryGateRequiresAGreenResult(t *testing.T) {
	kp := newKeyPair(t, "skills-2026", types.TrustRelease)
	cfg := DefaultConfig()
	cfg.SourcePath = "/nonexistent"
	cfg.Signers = []types.SkillSigner{kp.Signer}
	cfg.CanaryHostID = "c0ffee1234567890"
	s, _, clk := newTestSkills(t, cfg, "proc.connections", "service.reload", "config.set")
	_, signed := kp.sign(t, goldenArtifactTOML, playBytes())
	skill, _ := LoadArtifact([]byte(signed), playBytes())
	// No green result: held.
	if err := s.Puller.gate(context.Background(), skill, playBytes(), []string{"proc.connections", "service.reload", "config.set"}, &pullReport{}); CodeOf(err) != types.CodeSkills012 {
		t.Fatalf("no canary: %s (%v)", CodeOf(err), err)
	}
	// A green result from the canary host inside validity: the gate passes.
	s.Store.SetCanary(skill.Name, skill.Version, canaryRecord{
		Result: "green", HostID: "c0ffee1234567890", TS: types.FormatUTC(clk.Now()),
		ApplyMode: true, DaemonVersion: "0.1.0",
	})
	if err := s.Puller.gate(context.Background(), skill, playBytes(), []string{"proc.connections", "service.reload", "config.set"}, &pullReport{}); err != nil {
		t.Fatalf("a green canary must pass: %v", err)
	}
	// Expired green: held again, and the stale record is cleared.
	clk.advance(8 * 24 * 60 * 60 * 1000000000)
	if got := s.Resolver.ExpireStaleCanaries(); got != 1 {
		t.Fatalf("expired canaries = %d, want 1", got)
	}
	if err := s.Puller.gate(context.Background(), skill, playBytes(), []string{"proc.connections", "service.reload", "config.set"}, &pullReport{}); CodeOf(err) != types.CodeSkills012 {
		t.Fatalf("an expired canary must hold the artifact: %s (%v)", CodeOf(err), err)
	}
}

// TestFailClosedOnReadbackMismatchishStateError pins that a failed canary is
// terminal for the version (§4.4).
func TestFailedCanaryIsTerminal(t *testing.T) {
	kp := newKeyPair(t, "skills-2026", types.TrustRelease)
	cfg := DefaultConfig()
	cfg.SourcePath = "/nonexistent"
	cfg.Signers = []types.SkillSigner{kp.Signer}
	cfg.CanaryHostID = "c0ffee1234567890"
	s, _, clk := newTestSkills(t, cfg, "proc.connections", "service.reload", "config.set")
	_, signed := kp.sign(t, goldenArtifactTOML, playBytes())
	skill, _ := LoadArtifact([]byte(signed), playBytes())
	s.Store.SetCanary(skill.Name, skill.Version, canaryRecord{
		Result: "failed", HostID: "c0ffee1234567890", TS: types.FormatUTC(clk.Now()),
		ApplyMode: true, DaemonVersion: "0.1.0",
	})
	err := s.Puller.gate(context.Background(), skill, playBytes(), []string{"proc.connections", "service.reload", "config.set"}, &pullReport{})
	if CodeOf(err) != types.CodeSkills013 || ReasonOf(err) != ReasonCanaryFailed {
		t.Fatalf("a failed canary must refuse the version: %s/%s (%v)", CodeOf(err), ReasonOf(err), err)
	}
}
