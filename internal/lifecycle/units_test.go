package lifecycle

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

// TestSystemdAnalyzeVerify is SPEC-12 §7's units row: the rendered units are
// loaded by real systemd, not just pattern-checked. The units are always
// rendered into a temp unit root; when `systemd-analyze` exists on the host,
// `verify --root` must pass cleanly (a mismatch is a hard failure, never a
// skip) and `security --offline` reports the exposure score into the test
// log. Only a *missing* systemd-analyze binary downgrades the assertion —
// recorded loudly, not silently.
//
// Root layout notes (probed on systemd 259): units must sit at
// <root>/etc/systemd/system so `--root` search paths find them;
// --recursive-errors=no keeps verification to the named units (no chroot
// dependency chase) while still failing on unknown directives and missing
// ExecStart executables, which the test materializes as stand-ins inside the
// root. Run `go test -v` to see the verdict either way.
func TestSystemdAnalyzeVerify(t *testing.T) {
	cfg := defaults()
	units, err := RenderUnits(*cfg)
	if err != nil {
		t.Fatalf("RenderUnits: %v", err)
	}
	root := t.TempDir()
	unitDir := filepath.Join(root, "etc", "systemd", "system")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(units))
	for _, u := range units {
		if err := os.WriteFile(filepath.Join(unitDir, u.Name), []byte(u.Content), 0o644); err != nil {
			t.Fatal(err)
		}
		names = append(names, u.Name)
	}

	analyzer, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Log("systemd-analyze not found on host: unit files were rendered to the temp root, but the verify assertion is not exercised (SPEC-12 §7)")
		return
	}

	// Executables referenced by ExecStart/ExecReload must exist inside the
	// root; stand-ins keep the assertion focused on unit grammar and
	// wiring, not on a chrooted userland.
	rendered := units[0].Content
	for _, exe := range execStartPaths(rendered) {
		fake := filepath.Join(root, exe)
		if err := os.MkdirAll(filepath.Dir(fake), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fake, []byte("#!/bin/true\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	args := []string{"verify", "--root=" + root, "--recursive-errors=no"}
	args = append(args, names...)
	out, err := exec.Command(analyzer, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("systemd-analyze verify rejected the rendered units: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Logf("systemd-analyze verify output: %s", out)
	}

	// §7 also asks for a reported security score. Report-only: the spec
	// pins no threshold.
	sec, err := exec.Command(analyzer, "security", "--offline=true", "--root="+root, cfg.Lifecycle.UnitName).CombinedOutput()
	sc := extractExposureScore(string(sec))
	if err != nil {
		t.Logf("systemd-analyze security did not produce a score: %v\n%s", err, sec)
		return
	}
	if sc == "" {
		t.Logf("systemd-analyze security output carried no exposure score:\n%s", sec)
		return
	}
	if f, perr := strconv.ParseFloat(sc, 64); perr == nil {
		t.Logf("systemd-analyze security --offline score for %s: %s (exposure %.1f)", cfg.Lifecycle.UnitName, sc, f)
	} else {
		t.Logf("systemd-analyze security score for %s: %s", cfg.Lifecycle.UnitName, sc)
	}
}

// execStartPaths collects the executable paths referenced by ExecStart= and
// ExecReload= lines of a rendered unit, so the test can materialize
// stand-ins for them inside the verify root. ExecStart supports multiple
// commands (";"-separated); argv[0] of each is the leading absolute path.
func execStartPaths(content string) []string {
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(content, "\n") {
		for _, prefix := range []string{"ExecStart=", "ExecReload="} {
			if !strings.HasPrefix(line, prefix) {
				continue
			}
			for _, command := range strings.Split(strings.TrimPrefix(line, prefix), ";") {
				fields := strings.Fields(command)
				if len(fields) > 0 && strings.HasPrefix(fields[0], "/") {
					if !seen[fields[0]] {
						seen[fields[0]] = true
						out = append(out, fields[0])
					}
				}
			}
		}
	}
	return out
}

// extractExposureScore pulls the trailing "8.2" token off systemd-analyze
// security's verdict line, e.g. "→ Overall exposure level for x: 8.2 EXPOSED :-(".
func extractExposureScore(out string) string {
	const marker = "Overall exposure level"
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, marker) {
			continue
		}
		fields := strings.Fields(strings.TrimSuffix(strings.TrimSpace(line), ":-("))
		if len(fields) >= 3 {
			// "... : 8.2 EXPOSED" → the numeric token precedes EXPOSED/OK/... .
			if f, err := strconv.ParseFloat(fields[len(fields)-2], 64); err == nil {
				return strconv.FormatFloat(f, 'f', -1, 64)
			}
		}
	}
	return ""
}

// TestVerifyDetectsBrokenDirective is the teeth of the §7 row: the same
// verify invocation must reject a unit with an unknown directive, proving
// the pass above is a real grammar check and not a vacuous exit 0.
func TestVerifyDetectsBrokenDirective(t *testing.T) {
	analyzer, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Log("systemd-analyze not found on host: rendering still happened, negative control not exercised")
		return
	}
	cfg := defaults()
	units, err := RenderUnits(*cfg)
	if err != nil {
		t.Fatalf("RenderUnits: %v", err)
	}
	root := t.TempDir()
	unitDir := filepath.Join(root, "etc", "systemd", "system")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var broken string
	for _, u := range units {
		if u.Name != cfg.Lifecycle.UnitName {
			continue
		}
		content := strings.Replace(u.Content, "ExecStart=", "ExecStar=", 1)
		if content == u.Content {
			t.Fatal("could not corrupt the unit: no ExecStart line")
		}
		broken = u.Name
		if err := os.WriteFile(filepath.Join(unitDir, broken), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if broken == "" {
		t.Fatalf("unit %s not rendered", cfg.Lifecycle.UnitName)
	}
	out, err := exec.Command(analyzer, "verify", "--root="+root, "--recursive-errors=no", broken).CombinedOutput()
	if err == nil {
		t.Fatalf("systemd-analyze verify accepted a unit with an unknown directive:\n%s", out)
	}
	t.Logf("negative control rejected as expected: %v\n%s", err, out)
}
