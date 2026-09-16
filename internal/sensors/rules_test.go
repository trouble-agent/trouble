package sensors

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// rules_test.go covers SPEC-03 §7's rules row: TOML decode parity with
// types.Rule for all 12 fields, both match forms, duplicate names, the limit,
// the shipped defaults, and the auto_grants existence check.

func TestRuleTOMLDecodeParity(t *testing.T) {
	body := `
[[rule]]
name = "all_twelve_fields"
enabled = false
source = "psi"
for = "30s"
entry_rung = "research"
severity = "critical"
cooldown = "10m"
max_runs = 3
verify_window = "15m"
auto_grants = ["proc.top"]
hotfix = true

[[rule.match]]
field = "scope"
op = "in"
value = "[\"io\"]"
value_type = "string"

[[rule.match]]
field = "some_avg10"
op = ">="
value = "35"
value_type = "number"
`
	rules, err := decodeRulesTOML([]byte(body), "inline.toml")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("decoded %d rules, want 1", len(rules))
	}
	r := rules[0]
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"name", r.Name, "all_twelve_fields"},
		{"enabled", r.Enabled, false},
		{"source", string(r.Source), "psi"},
		{"for", string(r.For), "30s"},
		{"entry_rung", string(r.EntryRung), "research"},
		{"severity", string(r.Severity), "critical"},
		{"cooldown", string(r.Cooldown), "10m"},
		{"max_runs", r.MaxRuns, 3},
		{"verify_window", string(r.VerifyWin), "15m"},
		{"auto_grants", strings.Join(r.AutoGrants, ","), "proc.top"},
		{"hotfix", r.Hotfix, true},
		{"match", len(r.Match), 2},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("field %s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if r.Match[0].Field != "scope" || r.Match[0].Op != "in" || r.Match[0].ValueType != "string" {
		t.Errorf("match[0] decoded as %+v", r.Match[0])
	}
	if r.Match[1].Value != "35" || r.Match[1].ValueType != "number" {
		t.Errorf("match[1] decoded as %+v", r.Match[1])
	}
}

func TestRuleTOMLInlineTables(t *testing.T) {
	body := `
[[rule]]
name = "inline_match"
source = "disk"
match = [{ field = "free_pct", op = "<", value = "10", value_type = "number" }]
`
	rules, err := decodeRulesTOML([]byte(body), "inline.toml")
	if err != nil {
		t.Fatalf("inline tables must be accepted: %v", err)
	}
	if len(rules) != 1 || len(rules[0].Match) != 1 {
		t.Fatalf("inline match decoded as %+v", rules)
	}
	if rules[0].Match[0].Field != "free_pct" {
		t.Fatalf("inline match field = %q", rules[0].Match[0].Field)
	}
	// Defaults must still be applied for the keys the inline form omits.
	if rules[0].Enabled != true || rules[0].Severity != types.SevMedium || rules[0].MaxRuns != 2 {
		t.Fatalf("defaults were not applied: %+v", rules[0])
	}
}

func TestRuleTOMLExplicitFalseAndEmpty(t *testing.T) {
	body := `
[[rule]]
name = "explicit_false"
source = "psi"
enabled = false
severity = "info"
max_runs = 7
verify_window = "1m"
`
	rules, err := decodeRulesTOML([]byte(body), "x.toml")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	r := rules[0]
	// A missing `enabled` defaults true; an explicit false must survive.
	if r.Enabled {
		t.Error("explicit enabled=false must not be overwritten by the default")
	}
	if r.Severity != types.SevInfo {
		t.Errorf("severity = %s", r.Severity)
	}
	if r.MaxRuns != 7 || r.VerifyWin != "1m" {
		t.Errorf("explicit values overwritten: %+v", r)
	}
	if len(r.Match) != 0 {
		t.Errorf("missing match must decode as an empty list, got %v", r.Match)
	}
	if r.AutoGrants == nil {
		t.Error("auto_grants must decode as an empty slice, not nil")
	}
}

func TestRuleTOMLUnknownKeyRefused(t *testing.T) {
	body := `
[[rule]]
name = "typo_field"
source = "psi"
severty = "high"
`
	_, err := decodeRulesTOML([]byte(body), "typo.toml")
	if err == nil {
		t.Fatal("an unknown key must be refused (a typo must never be silent)")
	}
	if !strings.Contains(err.Error(), "TROUBLE-SENSORS-017") {
		t.Fatalf("wrong code: %v", err)
	}
}

func TestCompileRulesRefusals(t *testing.T) {
	base := func(name string) types.Rule {
		return types.Rule{
			Name: name, Enabled: true, Source: types.SrcPSI,
			Match:     []types.Condition{{Field: "some_avg10", Op: ">=", Value: "5", ValueType: "number"}},
			For:       "0s",
			EntryRung: types.RungRecord,
			Severity:  types.SevMedium,
			Cooldown:  "5m",
			MaxRuns:   2,
			VerifyWin: "10m",
		}
	}
	cases := []struct {
		name    string
		rules   []types.Rule
		modules []string
		wantSub string
		wantOK  bool
	}{
		{"valid", []types.Rule{base("ok_rule")}, []string{"proc.top"}, "", true},
		{"bad name", []types.Rule{{Name: "Bad Name", Source: types.SrcPSI, VerifyWin: "10m", MaxRuns: 1}}, nil, "name must match", false},
		{"duplicate", []types.Rule{base("dup"), base("dup")}, nil, "duplicate rule name", false},
		{"unknown source", []types.Rule{func() types.Rule { r := base("src"); r.Source = "sentinel"; return r }()}, nil, "not one of", false},
		{"bad rung", []types.Rule{func() types.Rule { r := base("rung"); r.EntryRung = types.RungOutlets; return r }()}, nil, "outlets is not a legal entry rung", false},
		{"bad severity", []types.Rule{func() types.Rule { r := base("sev"); r.Severity = "catastrophic"; return r }()}, nil, "severity", false},
		{"verify too long", []types.Rule{func() types.Rule { r := base("verify_long"); r.VerifyWin = "2h"; return r }()}, nil, "verify_window", false},
		{"zero max_runs", []types.Rule{func() types.Rule { r := base("maxruns_zero"); r.MaxRuns = 0; return r }()}, nil, "max_runs", false},
		{"unknown field", []types.Rule{func() types.Rule {
			r := base("fld")
			r.Match = []types.Condition{{Field: "nope", Op: "==", Value: "x", ValueType: "string"}}
			return r
		}()}, nil, "TROUBLE-SENSORS-018", false},
		{"field wrong for source", []types.Rule{func() types.Rule {
			r := base("fld2")
			r.Match = []types.Condition{{Field: "unit_substate", Op: "==", Value: "failed", ValueType: "string"}}
			return r
		}()}, nil, "TROUBLE-SENSORS-018", false},
		{"full on cpu", []types.Rule{func() types.Rule {
			r := base("fullcpu")
			r.Match = []types.Condition{
				{Field: "scope", Op: "in", Value: `["cpu"]`, ValueType: "string"},
				{Field: "full_avg60", Op: ">=", Value: "5", ValueType: "number"},
			}
			return r
		}()}, nil, "TROUBLE-SENSORS-018", false},
		{"too many match entries", []types.Rule{func() types.Rule {
			r := base("many")
			for i := 0; i < maxMatchEntries+1; i++ {
				r.Match = append(r.Match, types.Condition{Field: "some_avg10", Op: ">=", Value: "5", ValueType: "number"})
			}
			return r
		}()}, nil, "match entries", false},
		{"auto_grant unknown", []types.Rule{func() types.Rule {
			r := base("grant")
			r.AutoGrants = []string{"proc.nope"}
			return r
		}()}, []string{"proc.top"}, "not in the registry module list", false},
	}
	for _, c := range cases {
		known := len(c.modules) > 0
		set, problems := compileRules(c.rules, c.modules, known)
		if c.wantOK {
			if problems != nil {
				t.Errorf("%s: unexpected refusal: %v", c.name, problems)
				continue
			}
			if set == nil || len(set.rules) != len(c.rules) {
				t.Errorf("%s: set = %+v", c.name, set)
			}
			continue
		}
		if problems == nil {
			t.Errorf("%s: expected a refusal", c.name)
			continue
		}
		if !strings.Contains(problems.Error(), c.wantSub) {
			t.Errorf("%s: error %q does not contain %q", c.name, problems.Error(), c.wantSub)
		}
	}
}

func TestRuleLimits(t *testing.T) {
	mk := func(n int) []types.Rule {
		out := make([]types.Rule, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, types.Rule{
				Name: fmt.Sprintf("rule_%04d", i), Enabled: true, Source: types.SrcPSI,
				Match:     []types.Condition{{Field: "some_avg10", Op: ">=", Value: "5", ValueType: "number"}},
				EntryRung: types.RungRecord, Severity: types.SevMedium, Cooldown: "5m", MaxRuns: 1, VerifyWin: "10m",
			})
		}
		return out
	}
	if _, problems := compileRules(mk(maxRulesPerSource), nil, false); problems != nil {
		t.Fatalf("%d rules for one source must be accepted: %v", maxRulesPerSource, problems)
	}
	if _, problems := compileRules(mk(maxRulesPerSource+1), nil, false); problems == nil {
		t.Fatalf("%d rules for one source must be refused", maxRulesPerSource+1)
	}
	// A second source lifts the per-source limit but not the total.
	spread := []types.Rule{}
	for i := 0; i < 5; i++ {
		rs := mk(maxRulesPerSource)
		for j := range rs {
			rs[j].Name = fmt.Sprintf("s%d_rule_%04d", i, j)
			if i%2 == 1 {
				rs[j].Source = types.SrcDisk
				rs[j].Match = []types.Condition{{Field: "free_pct", Op: "<", Value: "10", ValueType: "number"}}
			}
			rs[j].AutoGrants = nil
		}
		spread = append(spread, rs...)
	}
	if _, problems := compileRules(spread, nil, false); problems == nil {
		t.Fatalf("%d rules total must be refused", len(spread))
	}
}

func TestShippedDefaultsLoadAndValidate(t *testing.T) {
	rules := defaultRules()
	if len(rules) != 9 {
		t.Fatalf("the shipped default set has %d rules, spec lists 9", len(rules))
	}
	set, problems := compileRules(rules, []string{"proc.top", "proc.connections", "service.reload"}, true)
	if problems != nil {
		t.Fatalf("shipped defaults must validate: %v", problems)
	}
	names := map[string]types.Rule{}
	for _, r := range set.rules {
		names[r.Name] = r
	}
	want := map[string]struct {
		source types.SigSource
		rung   types.Rung
		sev    types.Severity
		forD   types.Duration
	}{
		"psi_cpu_some_avg10_high": {types.SrcPSI, types.RungPlay, types.SevMedium, "60s"},
		"psi_io_some_avg10_high":  {types.SrcPSI, types.RungPlay, types.SevHigh, "30s"},
		"self_memory_pressure":    {types.SrcPSI, types.RungPlay, types.SevHigh, "30s"},
		"dbus_unit_failed":        {types.SrcDBus, types.RungPlay, types.SevHigh, "0s"},
		"dbus_crash_loop":         {types.SrcDBus, types.RungResearch, types.SevCritical, "0s"},
		"disk_space_low":          {types.SrcDisk, types.RungRecord, types.SevHigh, "5m"},
		"disk_inode_low":          {types.SrcDisk, types.RungRecord, types.SevMedium, "5m"},
		"timers_run_missed":       {types.SrcTimers, types.RungRecord, types.SevMedium, "0s"},
		"inotify_queue_overflow":  {types.SrcInotify, types.RungRecord, types.SevLow, "0s"},
	}
	for name, exp := range want {
		r, ok := names[name]
		if !ok {
			t.Errorf("shipped rule %q is missing", name)
			continue
		}
		if r.Source != exp.source || r.EntryRung != exp.rung || r.Severity != exp.sev || r.For != exp.forD {
			t.Errorf("shipped rule %q = source=%s rung=%s sev=%s for=%s, want %s/%s/%s/%s",
				name, r.Source, r.EntryRung, r.Severity, r.For, exp.source, exp.rung, exp.sev, exp.forD)
		}
	}
	// self_memory_pressure is the daemon's incident about itself and must be
	// play-rung with proc.top granted (AD-H).
	if got := strings.Join(names["self_memory_pressure"].AutoGrants, ","); got != "proc.top" {
		t.Errorf("self_memory_pressure auto_grants = %q", got)
	}
	if !names["self_memory_pressure"].Enabled {
		t.Error("self_memory_pressure must be enabled by default")
	}
}

func TestShippedDefaultsFileMatchesEmbedded(t *testing.T) {
	path := filepath.Join("..", "..", "examples", "rules", "10-defaults.toml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(b) != defaultRulesTOML {
		t.Fatal("examples/rules/10-defaults.toml and the embedded default set have drifted apart; they must be one source of truth")
	}
}

func TestLoadRuleDirSortedAndDefaultsOnFailure(t *testing.T) {
	h := newHarness(t)
	h.writeRules("20-b.toml", `
[[rule]]
name = "b_rule"
source = "disk"
[[rule.match]]
field = "free_pct"
op = "<"
value = "10"
value_type = "number"
`)
	h.writeRules("10-a.toml", `
[[rule]]
name = "a_rule"
source = "psi"
[[rule.match]]
field = "some_avg10"
op = ">="
value = "5"
value_type = "number"
`)
	rules, err := loadRuleDir(filepath.Join(h.dir, "rules.d"))
	if err != nil {
		t.Fatalf("loadRuleDir: %v", err)
	}
	if len(rules) != 2 || rules[0].Name != "a_rule" || rules[1].Name != "b_rule" {
		t.Fatalf("rules are not sorted by filename: %+v", rules)
	}
	// A broken edit keeps the previous set: New must fall back to the shipped
	// defaults rather than disarming the daemon.
	h.writeRules("30-broken.toml", "[[rule]]\nname = \"broken\"\nsource = \"nope\"\n")
	h2 := newHarnessWithRules(t, h.dir)
	if got := len(h2.s.Rules()); got != 9 {
		t.Fatalf("after a broken rules.d the shipped defaults must be active, got %d rules", got)
	}
}

// newHarnessWithRules builds a harness whose rules dir is an existing path.
func newHarnessWithRules(t *testing.T, dir string) *harness {
	t.Helper()
	h := &harness{t: t, dir: dir, clock: time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)}
	vals := []types.ConfigValue{
		{Key: "origin.host_id", Value: "testhost"},
		{Key: "state_root", Value: filepath.Join(dir, "state")},
		{Key: "sensors.rules.dir", Value: filepath.Join(dir, "rules.d")},
	}
	s, err := New(vals, h.emit, h.redact, h.now)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.s = s
	return h
}

func TestRulesAccessorIsACopy(t *testing.T) {
	h := newHarness(t)
	got := h.s.Rules()
	if len(got) != 9 {
		t.Fatalf("Rules() returned %d rules", len(got))
	}
	got[0].Name = "mutated"
	if h.s.Rules()[0].Name == "mutated" {
		t.Fatal("Rules() must return a copy: the live set is immutable")
	}
}

func TestAutoGrantsCheckSkippedIsRecorded(t *testing.T) {
	dir := t.TempDir()
	h := &harness{t: t, dir: dir, clock: time.Now()}
	s, err := New([]types.ConfigValue{
		{Key: "origin.host_id", Value: "h"},
		{Key: "state_root", Value: filepath.Join(dir, "state")},
		{Key: "sensors.rules.dir", Value: filepath.Join(dir, "rules.d")},
	}, h.emit, h.redact, h.now)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.s = s
	rs := s.rules.Load()
	if rs == nil || rs.skipped == "" {
		t.Fatal("a skipped auto_grants check must be recorded, not silently passed")
	}
	if !strings.Contains(rs.skipped, "registry.modules") {
		t.Fatalf("skip reason must name the missing input, got %q", rs.skipped)
	}
}

// TestEvaluateRuleFiresAfterStabilization asserts the `for=` gate end to end
// through handleEvent: continuous match fires, an interruption restarts.
func TestEvaluateRuleFiresAfterStabilization(t *testing.T) {
	h := newHarness(t)
	h.writeRules("10.toml", `
[[rule]]
name = "io_pressure"
source = "psi"
for = "2s"
severity = "high"
[[rule.match]]
field = "scope"
op = "=="
value = "io"
value_type = "string"
[[rule.match]]
field = "some_avg10"
op = ">="
value = "35"
value_type = "number"
`)
	h.mustReload()
	ctx := context.Background()
	// A non-matching sample must not fire and must not start the timer.
	h.s.handleEvent(ctx, psiEvent("io", 1.0, false))
	if len(h.fired()) != 0 {
		t.Fatal("a non-matching event fired")
	}
	// Matching samples accumulate until `for` elapses.
	h.s.handleEvent(ctx, psiEvent("io", 41.7, false))
	if len(h.fired()) != 0 {
		t.Fatal("fired before the stabilization window elapsed")
	}
	h.advance(2100 * time.Millisecond)
	h.s.handleEvent(ctx, psiEvent("io", 42.0, false))
	if got := len(h.fired()); got != 1 {
		t.Fatalf("fired %d times after 2s of continuous match, want 1", got)
	}
	// The same rule cannot fire again inside its cooldown for the same sig.
	h.advance(time.Second)
	h.s.handleEvent(ctx, psiEvent("io", 42.0, false))
	if got := len(h.fired()); got != 1 {
		t.Fatalf("cooldown was not honoured: %d fires", got)
	}
}

func TestInterruptedStabilizationRestarts(t *testing.T) {
	h := newHarness(t)
	// The rule's threshold sits inside one psi bucket so the interrupting
	// sample shares the sig: with bucket-derived sigs, a value in a different
	// bucket is a different story, not an interruption of this one.
	h.writeRules("10.toml", `
[[rule]]
name = "io_pressure"
source = "psi"
for = "5s"
[[rule.match]]
field = "scope"
op = "=="
value = "io"
value_type = "string"
[[rule.match]]
field = "some_avg10"
op = ">="
value = "70"
value_type = "number"
`)
	h.mustReload()
	ctx := context.Background()
	h.s.handleEvent(ctx, psiEvent("io", 80.0, false)) // b4, matches
	h.advance(4 * time.Second)
	h.s.handleEvent(ctx, psiEvent("io", 55.0, false)) // b4, same sig, fails the rule
	h.advance(2 * time.Second)
	h.s.handleEvent(ctx, psiEvent("io", 80.0, false))
	if len(h.fired()) != 0 {
		t.Fatal("a restart must not fire: the match has held for only 2s of a 5s window")
	}
	h.advance(6 * time.Second)
	h.s.handleEvent(ctx, psiEvent("io", 80.0, false))
	if len(h.fired()) != 1 {
		t.Fatalf("expected exactly one fire after the restarted window, got %d", len(h.fired()))
	}
}

func TestDisabledRuleIsValidatedButNeverEvaluated(t *testing.T) {
	h := newHarness(t)
	h.writeRules("10.toml", `
[[rule]]
name = "disabled_rule"
source = "psi"
enabled = false
[[rule.match]]
field = "some_avg10"
op = ">="
value = "1"
value_type = "number"
`)
	h.mustReload()
	ctx := context.Background()
	h.s.handleEvent(ctx, psiEvent("io", 99.0, false))
	h.advance(2 * time.Second)
	h.s.handleEvent(ctx, psiEvent("io", 99.0, false))
	if len(h.fired()) != 0 {
		t.Fatal("a disabled rule must never fire")
	}
}
