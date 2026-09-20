package skills

// config_default_test.go pins the shipped compiled default posture of TRBL-016
// (SPEC-11 §2a): the loop the build ships with is OFF and validates, the two
// named "off" constructors are the same value, and switching the loop ON without
// naming exactly one source is refused with TROUBLE-SKILLS-001.
//
// The unit-level companion to internal/app's shipped-example test: no boot, no
// socket, no channel — it fails fast when the posture regresses.

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// TestShippedDefaultLoopIsOffAndValidates is AC1's second half.
func TestShippedDefaultLoopIsOffAndValidates(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Enabled {
		t.Fatalf("skills.DefaultConfig().Enabled = true, want false: SPEC-11 §2a ships the loop OFF (no distribution channel is compiled in)")
	}
	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig(skills.DefaultConfig()) = %v, want nil: the shipped default must satisfy its own validator", err)
	}
	if cfg.SourcePath != "" || cfg.SourceURL != "" {
		t.Errorf("the shipped default names source_path=%q source_url=%q, want both empty: the operator names the channel", cfg.SourcePath, cfg.SourceURL)
	}

	// The two "off" constructors cannot drift: DisabledConfig is the shipped
	// default, not a second reading of it.
	if !reflect.DeepEqual(DisabledConfig(), DefaultConfig()) {
		t.Errorf("DisabledConfig() = %+v, want it to equal DefaultConfig() = %+v (SPEC-11 §2a: one off posture, not two)", DisabledConfig(), DefaultConfig())
	}

	// The posture reaches the status surface the CLI prints (the same surface the
	// dashboard's skills row reads).
	if st := DisabledConfig(); st.Enabled {
		t.Errorf("DisabledConfig().Enabled = true, want false")
	}
}

// TestEnablingTheShippedDefaultRefusesByName is the falsified direction for the
// loop: enabled = true with neither source key (and with both) is refused with
// the code and the message that names the rule.
func TestEnablingTheShippedDefaultRefusesByName(t *testing.T) {
	none := DefaultConfig()
	none.Enabled = true
	err := ValidateConfig(none)
	if err == nil {
		t.Fatalf("ValidateConfig(enabled loop, no source) = nil, want the TROUBLE-SKILLS-001 refusal")
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("ValidateConfig = %v (%T), want a classified *skills.Error", err, err)
	}
	if e.Code != types.CodeSkills001 || e.Reason != ReasonConfig {
		t.Errorf("refusal = %s/%s, want %s/config_invalid", e.Code, e.Reason, types.CodeSkills001)
	}
	if !strings.Contains(err.Error(), "exactly one of source_path or source_url must be set") {
		t.Errorf("refusal %q does not name the rule an operator must satisfy", err)
	}
	if _, err := New(none, Deps{StateRoot: t.TempDir()}); err == nil {
		t.Fatalf("New(enabled loop, no source) = nil error, want the refusal: a stock boot must never build a loop it cannot pull with")
	}

	both := DefaultConfig()
	both.Enabled = true
	both.SourcePath = "/srv/skills/release.git"
	both.SourceURL = "https://host/org/skills.git"
	if err := ValidateConfig(both); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("ValidateConfig(enabled loop, both sources) = %v, want the mutual-exclusion refusal", err)
	}
}

// TestDisabledLoopBuildsAndPullsNothing: enabled = false is a BUILT loop that
// holds no channel — /health.json reads built=true, refused=false — and its pull
// surface stays empty.
func TestDisabledLoopBuildsAndPullsNothing(t *testing.T) {
	s, err := New(DefaultConfig(), Deps{StateRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("New(skills.DefaultConfig()) = %v, want a built, disabled loop (the /health.json row reads built=true, refused=false)", err)
	}
	if st := s.Status(); st.Enabled {
		t.Errorf("a disabled loop reports Enabled=true on its status surface: %+v", st)
	}
	if got := s.Store.Status(s.Puller.State()); len(got.Skills) != 0 {
		t.Errorf("a disabled loop lists %d skills, want none: the local store is empty and nothing was pulled", len(got.Skills))
	}
	if _, err := EffectiveSource(DefaultConfig()); err == nil {
		t.Errorf("EffectiveSource(DefaultConfig()) = nil error, want the no-source refusal when something does ask for the channel")
	}
}
