package skills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// TestAuthorizerOnlyPathToACapability pins §4.6: the hook resolves the source to
// an INSTALLED artifact and re-checks the allowlist on every call, which is the
// defence in depth against a mutated play file or a stale task list.
func TestAuthorizerOnlyPathToACapability(t *testing.T) {
	kp := newKeyPair(t, "skills-2026", types.TrustRelease)
	src, _ := seedChannel(t, kp)
	cfg := pullerCfg(src, kp)
	s, _, _ := newTestSkills(t, cfg, "proc.connections", "service.reload", "config.set")
	if _, err := s.PullOnce(context.Background()); err != nil {
		t.Fatalf("pull: %v", err)
	}
	auth := s.Authorizer
	// A module the artifact lists and this build registers: allowed.
	if err := auth.Authorize(context.Background(), "payment-worker-queue-wedge@3", "proc.connections", []string{"read"}); err != nil {
		t.Fatalf("an allowed module must authorize: %v", err)
	}
	// A module the artifact does not list: 005.
	err := auth.Authorize(context.Background(), "payment-worker-queue-wedge@3", "service.restart", nil)
	if CodeOf(err) != types.CodeSkills005 {
		t.Fatalf("unlisted module: %s (%v)", CodeOf(err), err)
	}
	// The source token may also be the local candidate id or the name.
	if err := auth.Authorize(context.Background(), "payment-worker-queue-wedge", "service.reload", nil); err != nil {
		t.Fatalf("name source: %v", err)
	}
	// An unknown source is refused.
	if err := auth.Authorize(context.Background(), "sk_01J9Z6Q0M2X4T8V1K7B3N5R8WK", "proc.connections", nil); CodeOf(err) != types.CodeSkills013 {
		t.Fatalf("unknown id: %s (%v)", CodeOf(err), err)
	}
	// A module the artifact lists but this build does not register: 013.
	s.Authorizer.deps.Registered = func() []string { return []string{"proc.connections"} }
	err = auth.Authorize(context.Background(), "payment-worker-queue-wedge@3", "service.reload", nil)
	if CodeOf(err) != types.CodeSkills013 || ReasonOf(err) != ReasonMissingModule {
		t.Fatalf("unregistered module: %s/%s (%v)", CodeOf(err), ReasonOf(err), err)
	}
	// AuthorizeSource wraps the same check.
	if err := auth.AuthorizeSource(context.Background(), "skill:payment-worker-queue-wedge@3", "proc.connections"); err != nil {
		t.Fatalf("AuthorizeSource: %v", err)
	}
	if err := auth.AuthorizeSource(context.Background(), "rule:pool-exhaustion", "proc.connections"); CodeOf(err) != types.CodeSkills013 {
		t.Fatalf("a rule source is not a skill source: %s", CodeOf(err))
	}
	// A quarantined artifact is not authorizable.
	s.Store.putRow(installRow{Name: "payment-worker-queue-wedge", Version: 3, State: types.SkillQuarantined,
		Origin: "pull", Dir: "quarantine/payment-worker-queue-wedge/3"})
	if err := auth.Authorize(context.Background(), "payment-worker-queue-wedge@3", "proc.connections", nil); CodeOf(err) != types.CodeSkills013 {
		t.Fatalf("a quarantined artifact authorized a capability: %v", err)
	}
}

// TestAuthorizeRejectsATamperedPlay proves the hook re-reads the artifact from
// disk on every call: a locally mutated play turns into 002, not into a run.
func TestAuthorizeRejectsATamperedPlay(t *testing.T) {
	kp := newKeyPair(t, "skills-2026", types.TrustRelease)
	src, _ := seedChannel(t, kp)
	cfg := pullerCfg(src, kp)
	s, _, _ := newTestSkills(t, cfg, "proc.connections", "service.reload", "config.set")
	if _, err := s.PullOnce(context.Background()); err != nil {
		t.Fatalf("pull: %v", err)
	}
	dir := s.Store.InstallDir("payment-worker-queue-wedge", 3)
	play := filepath.Join(dir, "plays", "payment-worker-queue-wedge@3.toml")
	raw, err := os.ReadFile(play)
	if err != nil {
		t.Fatalf("read play: %v", err)
	}
	if err := os.WriteFile(play, append(raw, []byte("\n# tampered\n")...), 0o600); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	err = s.Authorizer.Authorize(context.Background(), "payment-worker-queue-wedge@3", "proc.connections", nil)
	if CodeOf(err) != types.CodeSkills002 {
		t.Fatalf("a tampered play must not authorize a capability: %s (%v)", CodeOf(err), err)
	}
}

// TestExplainWalksTheGateOrder is the `trouble skills explain` surface.
func TestExplainWalksTheGateOrder(t *testing.T) {
	kp := newKeyPair(t, "skills-2026", types.TrustRelease)
	src, _ := seedChannel(t, kp)
	cfg := pullerCfg(src, kp)
	s, _, _ := newTestSkills(t, cfg, "proc.connections", "service.reload", "config.set")
	if _, err := s.PullOnce(context.Background()); err != nil {
		t.Fatalf("pull: %v", err)
	}
	lines := s.Explain("payment-worker-queue-wedge", 3, []string{"proc.connections", "service.reload", "config.set"})
	if len(lines) < 5 {
		t.Fatalf("explain produced %d lines: %v", len(lines), lines)
	}
	joined := ""
	for _, l := range lines {
		joined += l + "\n"
	}
	for _, want := range []string{"schema:", "signature:", "floor:", "modules:", "canary:", "approve:"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("explain is missing %q:\n%s", want, joined)
		}
	}
	if unknown := s.Explain("nope", 1, nil); len(unknown) != 1 {
		t.Fatalf("explain for an unknown version = %v", unknown)
	}
}

// TestStatusSurface pins §3.7's status shape.
func TestStatusSurface(t *testing.T) {
	kp := newKeyPair(t, "skills-2026", types.TrustRelease)
	src, _ := seedChannel(t, kp)
	cfg := pullerCfg(src, kp)
	s, _, _ := newTestSkills(t, cfg, "proc.connections", "service.reload", "config.set")
	before := s.Status()
	if before.HostID != "7f3a91c2d4e5b607" || !before.Enabled {
		t.Fatalf("status = %+v", before)
	}
	if before.LastPullSHA != "" {
		t.Fatalf("a status before any pull carries a sha")
	}
	if _, err := s.PullOnce(context.Background()); err != nil {
		t.Fatalf("pull: %v", err)
	}
	// A canary host is configured after the (canary-free) install so the status
	// field is asserted without holding the artifact.
	s.Cfg.CanaryHostID = "c0ffee1234567890"
	s.Store.cfg.CanaryHostID = "c0ffee1234567890"
	st := s.Status()
	if st.LastPullSHA == "" || st.LastPullTS == "" || st.NextPullTS == "" {
		t.Fatalf("status after a pull = %+v", st)
	}
	if len(st.Skills) != 1 || st.Skills[0].Name != "payment-worker-queue-wedge" {
		t.Fatalf("status rows = %+v", st.Skills)
	}
	if st.Skills[0].State != types.SkillInstalled || st.Skills[0].Origin != "pull" {
		t.Fatalf("row = %+v", st.Skills[0])
	}
	if len(st.Signers) != 1 || st.Signers[0] != "skills-2026:release" {
		t.Fatalf("signers = %v", st.Signers)
	}
	if st.CanaryHostID != "c0ffee1234567890" {
		t.Fatalf("canary host = %q", st.CanaryHostID)
	}
	if st.Degraded {
		t.Fatalf("a healthy package reports degraded")
	}
}
