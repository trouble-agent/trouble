package scrub

import (
	"context"
	"strings"
	"testing"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// SPEC-02 §7 conformance_test.go: one positive vector per mandatory rule proving
// each is compiled, present and fires; the optional rules likewise; and a
// deliberately missing mandatory rule returns TROUBLE-SCRUB-006.

var mandatoryFixtures = map[string]string{
	"private_key_block":    "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEAx3Zk\n-----END RSA PRIVATE KEY-----\n",
	"private_key_inline":   `secret_key: "aVeryLongBase64LookingValue12345678"`,
	"dsn_secret":           "sentry://a1b2c3d4e5f60718293a4b5c6d7e8f90:0123456789abcdef0123456789abcdef@hooks.example:7643/7",
	"dsn_any":              "sentry://deployuser:deploysecret@hooks.example:7643/7",
	"conn_string_password": "postgres://app:s3cr3t@db.internal:5432/app",
	"url_basic_auth":       "https://deploy:hunter2@registry.internal/v2/",
	"env_assign":           "PASSWORD=hunter2swordfish",
	"kv_secret_assign":     `{"token": "abc123xyz"}`,
	"cli_flag_secret":      "--api-key=abcdef123456",
	"auth_header":          "Authorization: Basic dXNlcjpwYXNzd29yZA==",
	"bearer_token":         "Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.sigpart",
	"jwt":                  "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.sigpart",
	"cloud_key_shape":      "token AKIAIOSFODNN7EXAMPLE rejected",
	"entropy_token":        "Qw9zXk2Lm7Tp4Rv8Bn1Yh6Jd3Fg0Sa5Ce2Ui9Ol4Wq7",
	"pii_email_ip":         "contact alice@example.com from 203.0.113.9",
	"pii_identity_kv":      `{"user": "alice"}`,
	"path_home_root":       "~/projects/trouble/internal/scrub/engine.go",
	"path_disclosure":      "open /srv/data/app/logs/trouble.log failed",
}

func TestConformanceMandatoryRulesFire(t *testing.T) {
	e := newTestEngine(t, "")
	rules := e.Rules("")
	byName := map[string]types.ScrubRule{}
	for _, r := range rules {
		byName[r.Name] = r
	}
	fired := 0
	for _, name := range mandatoryNames {
		r, ok := byName[name]
		if !ok {
			t.Errorf("mandatory rule %s is absent from the effective set", name)
			continue
		}
		if !r.Mandatory {
			t.Errorf("mandatory rule %s does not carry the mandatory bit", name)
		}
		fixture := mandatoryFixtures[name]
		out, res := scrubString(t, e, types.TgEventMsg, "1", fixture)
		if res.ByRule[name] == 0 {
			t.Errorf("rule %s did not fire on its own fixture %q (by_rule=%s)", name, fixture, byRuleString(res.ByRule))
			continue
		}
		if !strings.Contains(out, "[REDACTED:") {
			t.Errorf("rule %s fired but produced no marker: %q → %q", name, fixture, out)
		}
		fired++
	}
	if fired != 13 {
		t.Errorf("fired %d/13 mandatory rules", fired)
	}
	// the marker of each rule is the exact grammar [REDACTED:<name>]
	for _, name := range mandatoryNames {
		e2 := newTestEngine(t, "")
		_, res := scrubString(t, e2, types.TgEventMsg, "1", mandatoryFixtures[name])
		if res.ByRule[name] > 0 {
			want := "[REDACTED:" + name + "]"
			out, _ := scrubString(t, e2, types.TgEventMsg, "1", mandatoryFixtures[name])
			if !strings.Contains(out, want) {
				t.Errorf("rule %s produced %q, want it to contain %q", name, out, want)
			}
		}
	}
}

func TestConformanceOptionalRulesFire(t *testing.T) {
	e := newTestEngine(t, "")
	fired := 0
	for _, name := range optionalNames {
		fixture := mandatoryFixtures[name]
		target := types.TgEventMsg
		if name == "path_disclosure" {
			e = newTestEngine(t, "[scrub]\npath_mode = \"redact\"\n")
		}
		_, res := scrubString(t, e, target, "1", fixture)
		if res.ByRule[name] == 0 {
			t.Errorf("optional rule %s did not fire on its fixture %q (by_rule=%s)",
				name, fixture, byRuleString(res.ByRule))
			continue
		}
		fired++
	}
	if fired != 5 {
		t.Errorf("fired %d/5 optional rules", fired)
	}
	// each optional rule can be disabled per project, and disabling is
	// set-subtraction: survivors keep their order (§3.6 rule 1)
	cfg := "[scrub]\n[scrub.projects.\"1\"]\ndisable = [\"entropy_token\", \"path_home_root\"]\n"
	e3 := newTestEngine(t, cfg)
	global := e3.Rules("")
	proj := e3.Rules("1")
	if len(proj) != len(global)-2 {
		t.Fatalf("project table has %d rules, want %d", len(proj), len(global)-2)
	}
	for _, r := range proj {
		if r.Name == "entropy_token" || r.Name == "path_home_root" {
			t.Errorf("rule %s was not disabled for project 1", r.Name)
		}
	}
	// the order of the survivors is unchanged
	gi := 0
	for _, r := range proj {
		for global[gi].Name != r.Name {
			gi++
		}
	}
	// empty rule delta: a project with no overrides has the global table
	e4 := newTestEngine(t, "[scrub]\n[scrub.projects.\"1\"]\n")
	projRules := e4.Rules("1")
	globRules := e4.Rules("")
	if len(projRules) != len(globRules) {
		t.Fatalf("empty delta changed the table: %d vs %d", len(projRules), len(globRules))
	}
	for i := range projRules {
		if projRules[i].Name != globRules[i].Name || projRules[i].Kind != globRules[i].Kind ||
			projRules[i].Replace != globRules[i].Replace || projRules[i].Pattern != globRules[i].Pattern ||
			projRules[i].Mandatory != globRules[i].Mandatory ||
			strings.Join(projRules[i].Targets, ",") != strings.Join(globRules[i].Targets, ",") {
			t.Errorf("empty delta changed rule %d: %+v vs %+v", i, projRules[i], globRules[i])
		}
	}
}

// TestConformanceRuleTableShape pins the §3.3 table: names, kinds, mandatory
// bits and the 18-rule order.
func TestConformanceRuleTableShape(t *testing.T) {
	e := newTestEngine(t, "")
	rules := e.Rules("")
	// 13 mandatory + 5 optional, minus path_disclosure which is off at
	// path_mode = keep (its default).
	if len(rules) != 17 {
		t.Fatalf("effective global table has %d rules, want 17", len(rules))
	}
	wantKind := map[string]types.RuleKind{
		"private_key_block":    types.KindPrefix,
		"private_key_inline":   types.KindRegex,
		"dsn_secret":           types.KindDSNPart,
		"dsn_any":              types.KindDSNPart,
		"conn_string_password": types.KindRegex,
		"url_basic_auth":       types.KindRegex,
		"env_assign":           types.KindRegex,
		"kv_secret_assign":     types.KindRegex,
		"cli_flag_secret":      types.KindRegex,
		"auth_header":          types.KindRegex,
		"bearer_token":         types.KindRegex,
		"jwt":                  types.KindRegex,
		"cloud_key_shape":      types.KindRegex,
		"entropy_token":        types.KindEntropy,
		"pii_email_ip":         types.KindRegex,
		"pii_identity_kv":      types.KindRegex,
		"path_home_root":       types.KindPathAllowlist,
		"path_disclosure":      types.KindPathAllowlist,
	}
	order := append(append([]string(nil), mandatoryNames...), optionalNames...)
	// path_disclosure is dropped at path_mode = keep (its default)
	order = order[:len(order)-1]
	for i, r := range rules {
		if r.Name != order[i] {
			t.Errorf("rule %d is %s, want %s (order is part of the artifact)", i, r.Name, order[i])
		}
		if k, ok := wantKind[r.Name]; ok && r.Kind != k {
			t.Errorf("rule %s kind = %s, want %s", r.Name, r.Kind, k)
		}
		if r.Replace != "[REDACTED:"+r.Name+"]" && r.Name != "path_home_root" {
			t.Errorf("rule %s replace = %q, want the marker grammar", r.Name, r.Replace)
		}
		for _, tg := range r.Targets {
			if !types.ScrubTarget(tg).Valid() {
				t.Errorf("rule %s targets unknown target %q", r.Name, tg)
			}
		}
		if r.Kind != types.KindRegex && r.Pattern != "builtin" {
			t.Errorf("rule %s (kind %s) carries pattern %q, want \"builtin\"", r.Name, r.Kind, r.Pattern)
		}
	}
}

// TestConformanceMissingMandatory: a rule table without a mandatory rule must
// refuse to build the engine (TROUBLE-SCRUB-006), because a build that loses a
// mandatory rule cannot run (§3.4 point 1).
func TestConformanceMissingMandatory(t *testing.T) {
	for _, drop := range []string{"env_assign", "bearer_token", "private_key_block", "dsn_secret"} {
		table := make([]builtinRule, 0, len(builtinTable))
		for _, r := range builtinTable {
			if r.name == drop {
				continue
			}
			table = append(table, r)
		}
		_, err := newEngine(nil, testProjects(), table, []string{testPubKeyA})
		if err == nil {
			t.Fatalf("engine built without mandatory rule %s", drop)
		}
		if got := CodeOf(err); got != types.CodeScrub006 {
			t.Errorf("code for missing %s = %s, want %s", drop, got, types.CodeScrub006)
		}
	}
}

// TestConformancePrefilterCoverage: the prefilter is a gate, never a filter. For
// every rule and every fixture, a fixture the rule fires on must also pass the
// gate (the equivalence test below proves the converse direction: switching the
// prefilter off never changes an output byte).
func TestConformancePrefilterCoverage(t *testing.T) {
	e := newTestEngine(t, "")
	for _, name := range append(append([]string(nil), mandatoryNames...), optionalNames...) {
		fixture := mandatoryFixtures[name]
		if fixture == "" {
			continue
		}
		var matched [maxTrigMaskWords]uint64
		g := gateState{on: true, matched: matched[:e.trigWords]}
		g.sigs = e.trig.fire([]byte(fixture), g.matched)
		for _, r := range e.table("1").rules {
			if r.name == name && g.ruleGated(r.gate, r.signals) {
				t.Errorf("the prefilter gates out rule %s on its own fixture %q", name, fixture)
			}
		}
	}
}

// TestPrefilterEquivalence: the prefilter never changes an output byte. Every
// vector, every marker context and 2,000 generated payloads are scrubbed with the
// prefilter on and off; the results must be identical.
func TestPrefilterEquivalence(t *testing.T) {
	on := newTestEngine(t, "")
	off := newTestEngine(t, "[scrub]\nprefilter = false\n")
	if !on.prefilterForTest("1") {
		t.Fatal("the default engine has no prefilter")
	}
	if off.prefilterForTest("1") {
		t.Fatal("prefilter = false did not disable the table prefilter")
	}
	inputs := []string{}
	for _, v := range positiveVectors() {
		inputs = append(inputs, v.in)
	}
	for _, v := range negativeVectors() {
		inputs = append(inputs, v.in)
	}
	for _, c := range markerContexts(testPubKeyA) {
		inputs = append(inputs, c.in)
	}
	inputs = append(inputs, generatePayloads(2000)...)
	targets := []types.ScrubTarget{types.TgEventMsg, types.TgStack, types.TgJournalTail, types.TgHeader, types.TgConfigSnapshot, types.TgIssue, types.TgDSN, types.TgSpool}
	for i, in := range inputs {
		tg := targets[i%len(targets)]
		a, ra, err1 := on.ScrubString(context.Background(), tg, "1", in)
		b, rb, err2 := off.ScrubString(context.Background(), tg, "1", in)
		if (err1 == nil) != (err2 == nil) {
			t.Fatalf("input %d: error mismatch: %v vs %v", i, err1, err2)
		}
		if a != b {
			t.Errorf("input %d: prefilter changed the output\n  on  %q\n  off %q", i, a, b)
		}
		if byRuleString(ra.ByRule) != byRuleString(rb.ByRule) {
			t.Errorf("input %d: by_rule differs: %s vs %s", i, byRuleString(ra.ByRule), byRuleString(rb.ByRule))
		}
	}
}
