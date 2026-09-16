package lifecycle

import (
	"context"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"

	"github.com/totalwindupflightsystems/trouble/internal/scrub"
	"github.com/totalwindupflightsystems/trouble/internal/types"
)

//go:embed units/*.tmpl
var unitFS embed.FS

// unitTemplate is a rendered systemd unit.
type unitTemplate struct {
	Name    string
	Path    string
	Content string
}

// Scope is the install scope for units.
type Scope string

const (
	ScopeUser   Scope = "user"
	ScopeSystem Scope = "system"
)

// RenderUnits renders the daemon, escalation and checker units (SPEC-12 §2.3, §3.5).
func RenderUnits(cfg Config) ([]unitTemplate, error) {
	execPath, err := os.Executable()
	if err != nil {
		execPath = "/usr/local/bin/trouble"
	}
	v, sha, _, unstamped := VersionInfo()
	desc := fmt.Sprintf("trouble daemon %s (%s)", v, sha)
	if unstamped {
		desc = "trouble daemon (UNSTAMPED)"
	}

	// argv-secret control: refuse any ExecStart argument matching a mandatory rule.
	execArgs := []string{execPath, "--config", cfg.ConfigPath}
	for _, a := range execArgs {
		if err := scrub.MandatoryScan(context.Background(), []byte(a)); err != nil {
			return nil, fmt.Errorf("%w: ExecStart argument refused by mandatory scan: %v", types.CodeLifecycle006, err)
		}
	}

	memHigh := "192M"
	memMax := "256M"
	watchdog := strconv.Itoa(int(cfg.Lifecycle.WatchdogSec.Seconds()))
	if watchdog == "0" {
		watchdog = "60"
	}
	protectHome := "read-only"
	if cfg.Lifecycle.SystemdScope == "system" {
		protectHome = "yes"
	}

	vars := map[string]string{
		"__EXEC__":         execPath,
		"__CONFIG__":       cfg.ConfigPath,
		"__ENVFILE__":      cfg.Secrets.EnvironmentFile,
		"__MEM_HIGH__":     memHigh,
		"__MEM_MAX__":      memMax,
		"__WATCHDOG__":     watchdog + "s",
		"__STATE_ROOT__":   cfg.StateRoot,
		"__DESC__":         desc,
		"__INTERVAL__":     string(cfg.Checker.Interval),
		"__PROTECT_HOME__": protectHome,
	}

	var out []unitTemplate
	for _, name := range []string{"trouble.service", "trouble-escalate@.service", "trouble-stall.service", "trouble-stall.timer"} {
		tmplName := name + ".tmpl"
		b, err := unitFS.ReadFile("units/" + tmplName)
		if err != nil {
			return nil, fmt.Errorf("%w: cannot read unit template %s: %v", types.CodeLifecycle006, name, err)
		}
		tmpl, err := template.New(name).Parse(string(b))
		if err != nil {
			return nil, fmt.Errorf("%w: cannot parse unit template %s: %v", types.CodeLifecycle006, name, err)
		}
		var sb strings.Builder
		if err := tmpl.Execute(&sb, vars); err != nil {
			return nil, fmt.Errorf("%w: cannot render unit template %s: %v", types.CodeLifecycle006, name, err)
		}
		out = append(out, unitTemplate{Name: name, Content: sb.String()})
	}
	return out, nil
}

// AuditUnits checks rendered units for missing escalation wiring (SPEC-12 §3.5).
func AuditUnits(cfg Config, scope Scope) ([]types.ConfigValue, error) {
	units, err := RenderUnits(cfg)
	if err != nil {
		return nil, err
	}
	var out []types.ConfigValue
	hasOnFailure := false
	hasEscalateUnit := false
	for _, u := range units {
		if u.Name == cfg.Lifecycle.UnitName && strings.Contains(u.Content, "OnFailure=") {
			hasOnFailure = true
		}
		if u.Name == cfg.Lifecycle.EscalateUnit {
			hasEscalateUnit = true
		}
	}
	if !hasOnFailure {
		out = append(out, types.ConfigValue{Key: "lifecycle.unit_name", Value: cfg.Lifecycle.UnitName, Source: "audit", SourceRef: "OnFailure missing"})
	}
	if !hasEscalateUnit {
		out = append(out, types.ConfigValue{Key: "lifecycle.escalate_unit", Value: cfg.Lifecycle.EscalateUnit, Source: "audit", SourceRef: "escalate unit not rendered"})
	}
	if len(cfg.Escalate.Channels) == 0 {
		out = append(out, types.ConfigValue{Key: "escalate.channels", Value: "[]", Source: "audit", SourceRef: "empty"})
	}
	return out, nil
}

// InstallUnits writes rendered units into the configured unit dir.
func InstallUnits(cfg Config, scope Scope, root string, dryRun bool) error {
	units, err := RenderUnits(cfg)
	if err != nil {
		return err
	}
	dir := unitDir(scope, root)
	if !dryRun {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("%w: cannot create unit dir %q: %v", types.CodeLifecycle006, dir, err)
		}
	}
	for _, u := range units {
		path := filepath.Join(dir, u.Name)
		if dryRun {
			continue
		}
		if err := os.WriteFile(path, []byte(u.Content), 0o644); err != nil {
			return fmt.Errorf("%w: cannot write unit %q: %v", types.CodeLifecycle006, path, err)
		}
	}
	return nil
}

func unitDir(scope Scope, root string) string {
	if root != "" {
		return root
	}
	if scope == ScopeSystem {
		return "/etc/systemd/system"
	}
	return filepath.Join(os.Getenv("HOME"), ".config", "systemd", "user")
}
