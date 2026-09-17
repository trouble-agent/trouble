package skills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// TestBootReverificationQuarantinesTamperedArtifacts pins §3.5: every boot
// re-verifies each installed row against its recorded digests, and a row that no
// longer matches is quarantined with TROUBLE-SKILLS-002 + a refusal record. Local
// tampering is detected, never executed.
func TestBootReverificationQuarantinesTamperedArtifacts(t *testing.T) {
	kp := newKeyPair(t, "skills-2026", types.TrustRelease)
	src, _ := seedChannel(t, kp)
	stateDir := filepath.Join(t.TempDir(), "skills-local")
	cfg := pullerCfg(src, kp)
	cfg.StateDir = stateDir

	led := &fakeLedger{}
	clk := newFakeClock()
	led.now = clk.Now
	s, err := New(cfg, testDeps(t, led, clk, t.TempDir(), "proc.connections", "service.reload", "config.set"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.PullOnce(context.Background()); err != nil {
		t.Fatalf("pull: %v", err)
	}
	if _, ok := s.Store.row("payment-worker-queue-wedge", 3); !ok {
		t.Fatalf("nothing was installed")
	}

	// Two kinds of tampering: a changed play byte (the payload the signature
	// covers) and an appended key in SKILL.toml (a schema the decoder refuses).
	for _, tamper := range []string{"play", "toml"} {
		t.Run(tamper, func(t *testing.T) {
			// A fresh state dir per case, seeded by a fresh pull.
			dir := filepath.Join(t.TempDir(), "skills-local")
			c := cfg
			c.StateDir = dir
			l2 := &fakeLedger{}
			c2 := newFakeClock()
			l2.now = c2.Now
			first, err := New(c, testDeps(t, l2, c2, t.TempDir(), "proc.connections", "service.reload", "config.set"))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, err := first.PullOnce(context.Background()); err != nil {
				t.Fatalf("pull: %v", err)
			}
			installed := first.Store.InstallDir("payment-worker-queue-wedge", 3)
			switch tamper {
			case "play":
				p := filepath.Join(installed, "plays", "payment-worker-queue-wedge@3.toml")
				raw, _ := os.ReadFile(p)
				if err := os.WriteFile(p, append(raw, []byte("\n# tampered\n")...), 0o600); err != nil {
					t.Fatalf("tamper: %v", err)
				}
			case "toml":
				p := filepath.Join(installed, "SKILL.toml")
				raw, _ := os.ReadFile(p)
				if err := os.WriteFile(p, append(raw, []byte("\nshell = \"/bin/sh\"\n")...), 0o600); err != nil {
					t.Fatalf("tamper: %v", err)
				}
			}
			// Boot the package again over the tampered tree: it must never fail
			// the boot, and must never keep the row installed.
			led3 := &fakeLedger{}
			c3 := newFakeClock()
			led3.now = c3.Now
			second, err := New(c, testDeps(t, led3, c3, t.TempDir(), "proc.connections", "service.reload", "config.set"))
			if err != nil {
				t.Fatalf("boot after tampering must not fail: %v", err)
			}
			// §3.5: "moved to quarantine/, marked state=refused, and reported with
			// TROUBLE-SKILLS-002 + a refusal record".
			row, ok := second.Store.row("payment-worker-queue-wedge", 3)
			if !ok || row.State != types.SkillRefused {
				t.Fatalf("row after the boot re-verification = %+v ok=%v", row, ok)
			}
			if !strings.HasPrefix(row.Dir, "quarantine/") {
				t.Fatalf("the tampered tree was not moved to quarantine: %q", row.Dir)
			}
			if _, err := os.Stat(filepath.Join(second.Store.Dir(), row.Dir, "SKILL.toml")); err != nil {
				t.Fatalf("the quarantined tree is not where the row says: %v", err)
			}
			if _, err := os.Stat(second.Store.InstallDir("payment-worker-queue-wedge", 3)); err == nil {
				t.Fatalf("the installed tree survived the quarantine")
			}
			var refusal types.Record
			sawRefusal := false
			for _, r := range led3.byPhase(PhaseRefused) {
				if str(r.Payload, "error_code") == string(types.CodeSkills002) {
					refusal, sawRefusal = r, true
				}
			}
			if !sawRefusal {
				t.Fatalf("no 002 refusal record: %v", led3.phases())
			}
			if str(refusal.Payload, "reason") != ReasonTampered {
				t.Fatalf("refusal reason = %q", str(refusal.Payload, "reason"))
			}
			// A quarantined artifact is not resolvable and not authorizable.
			res, err := second.Resolver.Match("sentinel:sha256v1:9f2c1d3e4b5a6c7d")
			if err != nil || res.Candidates != 0 {
				t.Fatalf("a tampered artifact resolved: %+v err=%v", res, err)
			}
			if err := second.Authorizer.Authorize(context.Background(), "payment-worker-queue-wedge@3", "proc.connections", nil); err == nil {
				t.Fatalf("a tampered artifact authorized a capability")
			}
		})
	}
}

// TestBootReverificationAcceptsCleanTrees proves the pass is silent when nothing
// was touched: no record, no state change.
func TestBootReverificationAcceptsCleanTrees(t *testing.T) {
	kp := newKeyPair(t, "skills-2026", types.TrustRelease)
	src, _ := seedChannel(t, kp)
	stateDir := filepath.Join(t.TempDir(), "skills-local")
	cfg := pullerCfg(src, kp)
	cfg.StateDir = stateDir
	led := &fakeLedger{}
	clk := newFakeClock()
	led.now = clk.Now
	s, err := New(cfg, testDeps(t, led, clk, t.TempDir(), "proc.connections", "service.reload", "config.set"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.PullOnce(context.Background()); err != nil {
		t.Fatalf("pull: %v", err)
	}
	led2 := &fakeLedger{}
	clk2 := newFakeClock()
	led2.now = clk2.Now
	before, _ := os.ReadFile(filepath.Join(stateDir, "index.json"))
	second, err := New(cfg, testDeps(t, led2, clk2, t.TempDir(), "proc.connections", "service.reload", "config.set"))
	if err != nil {
		t.Fatalf("second boot: %v", err)
	}
	out := ""
	for _, l := range second.Explain("payment-worker-queue-wedge", 3, []string{"proc.connections", "service.reload", "config.set"}) {
		out += l
	}
	if !strings.Contains(out, "signature: ok") {
		t.Fatalf("a clean tree must still verify: %s", out)
	}
	if len(led2.records()) != 0 {
		t.Fatalf("a clean boot wrote records: %v", led2.phases())
	}
	after, _ := os.ReadFile(filepath.Join(stateDir, "index.json"))
	if string(before) != string(after) {
		t.Fatalf("a clean boot changed the index:\nbefore=%s\nafter=%s", before, after)
	}
}
