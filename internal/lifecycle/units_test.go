package lifecycle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderUnits(t *testing.T) {
	cfg := defaults()
	units, err := RenderUnits(*cfg)
	if err != nil {
		t.Fatalf("RenderUnits: %v", err)
	}
	if len(units) != 4 {
		t.Fatalf("expected 4 units, got %d", len(units))
	}
	for _, u := range units {
		if u.Name == "trouble-stall.timer" {
			if !strings.Contains(u.Content, "OnUnitActiveSec=") {
				t.Errorf("timer %s missing OnUnitActiveSec", u.Name)
			}
			continue
		}
		if !strings.Contains(u.Content, "ExecStart=") {
			t.Errorf("unit %s missing ExecStart", u.Name)
		}
	}
}

func TestRenderUnitsRefusesSecretArg(t *testing.T) {
	cfg := defaults()
	// Simulate an illegal extra flag containing a secret shape.
	cfg.ConfigPath = "/etc/trouble/config.toml --token=sk_live_abc"
	_, err := RenderUnits(*cfg)
	if err == nil {
		t.Fatal("expected render refusal for secret-shaped arg")
	}
}

func TestInstallUnitsRootOnly(t *testing.T) {
	cfg := defaults()
	root := t.TempDir()
	if err := InstallUnits(*cfg, ScopeUser, root, false); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"trouble.service", "trouble-escalate@.service", "trouble-stall.service", "trouble-stall.timer"} {
		path := filepath.Join(root, name)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("missing unit %s", name)
		}
	}
}

func TestAuditUnitsMissingChannels(t *testing.T) {
	cfg := defaults()
	cfg.Escalate.Channels = nil
	out, err := AuditUnits(*cfg, ScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, cv := range out {
		if cv.Key == "escalate.channels" {
			found = true
		}
	}
	if !found {
		t.Error("expected escalation channel audit finding")
	}
}
