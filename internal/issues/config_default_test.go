package issues

// config_default_test.go pins the shipped compiled default posture of TRBL-016
// (SPEC-09 §3.4a): the desk the build ships with is OFF and validates, and the
// moment it is switched ON over the same driver blocks it is refused BY NAME.
// Both halves are load-bearing — a default that validates only because nothing is
// checked is not a default, and a refusal that does not name the missing key is
// not actionable for the operator who just turned the desk on.
//
// The unit-level companion to internal/app's shipped-example test: this one needs
// no boot, no socket and no file, so it fails fast when the posture regresses.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// TestShippedDefaultDeskIsOffAndValidates is AC1's first half: the desk the
// build ships with is disabled, satisfies its own validator, and the driver
// blocks it carries are left exactly as SPEC-09 §3.4 writes them.
func TestShippedDefaultDeskIsOffAndValidates(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Enabled {
		t.Fatalf("issues.DefaultConfig().Enabled = true, want false: SPEC-09 §3.4a ships the desk OFF (no deployment value is compiled in)")
	}
	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig(issues.DefaultConfig()) = %v, want nil: the shipped default must satisfy its own validator", err)
	}

	gh, ok := driverByName(cfg.Drivers, "github")
	if !ok {
		t.Fatalf("the default driver set carries no github block: %+v", cfg.Drivers)
	}
	if !gh.Enabled {
		t.Errorf("the default github block is disabled inside the list: the opt-out is the DESK's, and the driver must stay enabled so enabling the desk still names the missing key (SPEC-09 §3.4a)")
	}
	if gh.Owner != "" || gh.Repo != "" {
		t.Errorf("the shipped github block names owner=%q repo=%q, want both empty: no deployment value is compiled in", gh.Owner, gh.Repo)
	}

	// The posture is visible on the explain surface too (SPEC-12's dump reads it).
	var sawEnabled bool
	for _, l := range Explain(cfg) {
		if l.Key == "issues.enabled" {
			sawEnabled = true
			if l.Value != "false" {
				t.Errorf("explain renders %s = %s, want false", l.Key, l.Value)
			}
		}
	}
	if !sawEnabled {
		t.Errorf("explain carries no issues.enabled row: %+v", Explain(cfg))
	}
}

// TestEnablingTheShippedDefaultRefusesByName is the falsified direction: an
// operator who sets enabled = true over the shipped driver blocks gets the
// refusal, with TROUBLE-ISSUES-003 and the message naming the missing key.
func TestEnablingTheShippedDefaultRefusesByName(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled = true

	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatalf("ValidateConfig(the shipped driver set, enabled) = nil, want the owner/repo refusal")
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("ValidateConfig = %v (%T), want a classified *issues.Error", err, err)
	}
	if e.Code != types.CodeIssues003 || e.Reason != ReasonConfig {
		t.Errorf("refusal = %s/%s, want %s/config_invalid", e.Code, e.Reason, types.CodeIssues003)
	}
	if !strings.Contains(err.Error(), "driver github needs owner and repo") {
		t.Errorf("refusal %q does not name the missing keys (owner and repo): a fresh operator must be told what to set", err)
	}

	// New is the composition root's door: the same refusal, so buildSubsystems
	// records TROUBLE-ISSUES-003 rather than building a desk that cannot file.
	if _, err := New(cfg, Deps{}); err == nil {
		t.Fatalf("New(the shipped driver set, enabled) = nil error, want the refusal")
	}
}

// TestDisabledDeskIsBuiltAndAnswersWithTheOptOut is the other side of the same
// coin: with enabled = false the desk BUILDS (/health.json reads built=true,
// refused=false) and it holds no driver — so every operation names the opt-out
// instead of blaming a driver block that is, in fact, enabled.
func TestDisabledDeskIsBuiltAndAnswersWithTheOptOut(t *testing.T) {
	cfg := DefaultConfig()
	d, err := New(cfg, Deps{})
	if err != nil {
		t.Fatalf("New(issues.DefaultConfig()) = %v, want a built, disabled desk (the /health.json row reads built=true, refused=false)", err)
	}
	if d.Degraded() {
		t.Errorf("a disabled desk reports degraded at boot; OFF is a posture, not a failure")
	}
	if got := d.Health(context.Background()); len(got) != 0 {
		t.Errorf("a disabled desk health-checks %d drivers (%+v), want none: no driver is constructed", len(got), got)
	}

	_, err = d.EnsureBySig(context.Background(), types.Incident{ID: "inc_trbl016", Sig: "sentinel:sha256v1:0123456789abcdef"}, Evidence{})
	if err == nil {
		t.Fatalf("EnsureBySig on a disabled desk = nil error, want the disabled refusal")
	}
	if !strings.Contains(err.Error(), "the issue desk is disabled") {
		t.Errorf("EnsureBySig refusal = %q, want it to name the disabled desk (issues.enabled = false)", err)
	}
	if !strings.Contains(err.Error(), "owner/repo") {
		t.Errorf("EnsureBySig refusal = %q, want it to name the opt-in key an operator must set", err)
	}

	// Replay is loop-driven and must answer the same way, not report "0 replayed
	// so all is well" while the desk is off.
	if n, err := d.Replay(context.Background(), 10); err == nil || n != 0 {
		t.Errorf("Replay on a disabled desk = (%d, %v), want (0, the disabled refusal)", n, err)
	}
}
