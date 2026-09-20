package scrub

import (
	"fmt"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// SPEC-02 §7 config_test.go: one case per §3.2 validation row, asserting the
// exact TROUBLE-SCRUB-002 and the offending dotted key.

func TestConfigValidationRows(t *testing.T) {
	cases := []struct {
		row  string
		cfg  string
		code types.ErrorCode
		key  string
	}{
		{
			row: "unknown key under [scrub]", code: types.CodeScrub002, key: "max_byte_default",
			cfg: "[scrub]\nmax_byte_default = 4096\n",
		},
		{
			row: "unknown key in the flat form", code: types.CodeScrub002, key: "boundary_verif",
			cfg: "boundary_verif = true\n",
		},
		{
			row: "boundary_verify is not configurable", code: types.CodeScrub002, key: "scrub.boundary_verify",
			cfg: "[scrub]\nboundary_verify = false\n",
		},
		{
			row: "[scrub.rules.<mandatory>] present", code: types.CodeScrub002, key: "scrub.rules.env_assign",
			cfg: "[scrub]\n[scrub.rules.env_assign]\nenabled = true\n",
		},
		{
			row: "unknown built-in rule name", code: types.CodeScrub002, key: "scrub.rules.token_bucket",
			cfg: "[scrub]\n[scrub.rules.token_bucket]\nenabled = true\n",
		},
		{
			row: "a key other than enabled on a built-in rule", code: types.CodeScrub002, key: "scrub.rules.entropy_token.pattern",
			cfg: "[scrub]\n[scrub.rules.entropy_token]\npattern = \"x\"\n",
		},
		{
			row: "rule name outside the marker grammar", code: types.CodeScrub002, key: "scrub.projects.1.rules.name",
			cfg: projectRules(`{name = "Bad-Name", kind = "regex", pattern = "x", replace = "[REDACTED:bad]", targets = ["event_msg"]}`),
		},
		{
			row: "rule name that is a marker fragment", code: types.CodeScrub002, key: "scrub.projects.1.rules.name",
			cfg: projectRules(`{name = "x]", kind = "regex", pattern = "x", replace = "[REDACTED:x]", targets = ["event_msg"]}`),
		},
		{
			row: "duplicate rule name against a built-in", code: types.CodeScrub002, key: "scrub.projects.1.rules.name",
			cfg: projectRules(`{name = "entropy_token", kind = "regex", pattern = "x", replace = "[REDACTED:x]", targets = ["event_msg"]}`),
		},
		{
			row: "duplicate rule name inside one project", code: types.CodeScrub002, key: "scrub.projects.1.rules.name",
			cfg: projectRules(
				`{name = "acct_id", kind = "regex", pattern = "x", replace = "[REDACTED:acct_id]", targets = ["event_msg"]}`,
				`{name = "acct_id", kind = "regex", pattern = "y", replace = "[REDACTED:acct_id]", targets = ["issue"]}`),
		},
		{
			row: "config-authored kind = dsn_part", code: types.CodeScrub002, key: "scrub.projects.1.rules.kind",
			cfg: projectRules(`{name = "my_dsn", kind = "dsn_part", replace = "[REDACTED:my_dsn]", targets = ["event_msg"]}`),
		},
		{
			row: "config-authored kind = path_allowlist", code: types.CodeScrub002, key: "scrub.projects.1.rules.kind",
			cfg: projectRules(`{name = "my_path", kind = "path_allowlist", replace = "[REDACTED:my_path]", targets = ["event_msg"]}`),
		},
		{
			row: "unknown kind", code: types.CodeScrub002, key: "scrub.projects.1.rules.kind",
			cfg: projectRules(`{name = "my_rule", kind = "glob", pattern = "x", replace = "[REDACTED:my_rule]", targets = ["event_msg"]}`),
		},
		{
			row: "missing kind", code: types.CodeScrub002, key: "scrub.projects.1.rules.kind",
			cfg: projectRules(`{name = "my_rule", pattern = "x", replace = "[REDACTED:my_rule]", targets = ["event_msg"]}`),
		},
		{
			row: "uncompilable pattern", code: types.CodeScrub002, key: "scrub.projects.1.rules.pattern",
			cfg: projectRules(`{name = "my_rule", kind = "regex", pattern = "([", replace = "[REDACTED:my_rule]", targets = ["event_msg"]}`),
		},
		{
			row: "missing pattern for kind = regex", code: types.CodeScrub002, key: "scrub.projects.1.rules.pattern",
			cfg: projectRules(`{name = "my_rule", kind = "regex", replace = "[REDACTED:my_rule]", targets = ["event_msg"]}`),
		},
		{
			row: "empty replace", code: types.CodeScrub002, key: "scrub.projects.1.rules.replace",
			cfg: projectRules(`{name = "my_rule", kind = "regex", pattern = "x", targets = ["event_msg"]}`),
		},
		{
			row: "no targets", code: types.CodeScrub002, key: "scrub.projects.1.rules.targets",
			cfg: projectRules(`{name = "my_rule", kind = "regex", pattern = "x", replace = "[REDACTED:my_rule]", targets = []}`),
		},
		{
			row: "unknown target", code: types.CodeScrub002, key: "scrub.projects.1.rules.targets",
			cfg: projectRules(`{name = "my_rule", kind = "regex", pattern = "x", replace = "[REDACTED:my_rule]", targets = ["logfile"]}`),
		},
		{
			row: "a project rule claiming the mandatory bit", code: types.CodeScrub002, key: "scrub.projects.1.rules.mandatory",
			cfg: projectRules(`{name = "my_rule", kind = "regex", pattern = "x", replace = "[REDACTED:my_rule]", mandatory = true, targets = ["event_msg"]}`),
		},
		{
			row: "project keyed by an unknown Project.ID", code: types.CodeScrub002, key: "scrub.projects.99",
			cfg: "[scrub]\n[scrub.projects.\"99\"]\npii_mode = \"keep\"\n",
		},
		{
			row: "a mandatory rule disabled per project", code: types.CodeScrub002, key: "scrub.projects.1.disable",
			cfg: "[scrub]\n[scrub.projects.\"1\"]\ndisable = [\"env_assign\"]\n",
		},
		{
			row: "an unknown rule disabled per project", code: types.CodeScrub002, key: "scrub.projects.1.disable",
			cfg: "[scrub]\n[scrub.projects.\"1\"]\ndisable = [\"my_rule\"]\n",
		},
		{
			row: "invalid pii_mode", code: types.CodeScrub002, key: "scrub.pii_mode",
			cfg: "[scrub]\npii_mode = \"maybe\"\n",
		},
		{
			row: "invalid per-project pii_mode", code: types.CodeScrub002, key: "scrub.projects.1.pii_mode",
			cfg: "[scrub]\n[scrub.projects.\"1\"]\npii_mode = \"maybe\"\n",
		},
		{
			row: "invalid path_mode", code: types.CodeScrub002, key: "scrub.path_mode",
			cfg: "[scrub]\npath_mode = \"hide\"\n",
		},
		{
			row: "invalid per-project path_mode", code: types.CodeScrub002, key: "scrub.projects.1.path_mode",
			cfg: "[scrub]\n[scrub.projects.\"1\"]\npath_mode = \"hide\"\n",
		},
		{
			row: "invalid on_over", code: types.CodeScrub002, key: "scrub.targets.event_msg.on_over",
			cfg: "[scrub]\n[scrub.targets.event_msg]\non_over = \"drop\"\n",
		},
		{
			row: "unknown scrub target", code: types.CodeScrub002, key: "scrub.targets.logfile",
			cfg: "[scrub]\n[scrub.targets.logfile]\nmax_bytes = 10\n",
		},
		{
			row: "non-positive max_bytes", code: types.CodeScrub002, key: "scrub.targets.event_msg.max_bytes",
			cfg: "[scrub]\n[scrub.targets.event_msg]\nmax_bytes = 0\n",
		},
		{
			row: "non-positive max_bytes_default", code: types.CodeScrub002, key: "scrub.max_bytes_default",
			cfg: "[scrub]\nmax_bytes_default = -1\n",
		},
		{
			row: "invalid rule_timeout", code: types.CodeScrub002, key: "scrub.rule_timeout",
			cfg: "[scrub]\nrule_timeout = \"soon\"\n",
		},
		{
			row: "zero rule_timeout", code: types.CodeScrub002, key: "scrub.rule_timeout",
			cfg: "[scrub]\nrule_timeout = \"0s\"\n",
		},
		{
			row: "rules_version below 1", code: types.CodeScrub002, key: "scrub.rules_version",
			cfg: "[scrub]\nrules_version = 0\n",
		},
		{
			row: "malformed TOML", code: types.CodeScrub002,
			cfg: "[scrub]\nmax_bytes_default = \n",
		},
		{
			row: "type mismatch", code: types.CodeScrub002,
			cfg: "[scrub]\nmax_bytes_default = \"big\"\n",
		},
		{
			row: "too many project rescan rules", code: types.CodeScrub002, key: "scrub.projects.1.rules",
			cfg: projectRules(rescanRules(9)...),
		},
	}
	for _, c := range cases {
		_, err := New([]byte(c.cfg), testProjects())
		if err == nil {
			t.Errorf("%s: New accepted an invalid config", c.row)
			continue
		}
		if got := CodeOf(err); got != c.code {
			t.Errorf("%s: code = %s, want %s (%v)", c.row, got, c.code, err)
			continue
		}
		if c.key != "" && !strings.Contains(err.Error(), c.key) {
			t.Errorf("%s: error %q does not name the offending key %q", c.row, err.Error(), c.key)
		}
	}
}

// TestConfigRuleTableLimit: more than 64 rules in an effective set is
// TROUBLE-SCRUB-002 (13 mandatory + 5 optional + ≤46 project).
func TestConfigRuleTableLimit(t *testing.T) {
	ok := 46
	if _, err := New([]byte(projectRules(numberedRules(ok)...)), testProjects()); err != nil {
		t.Errorf("46 project rules (the 64-rule limit) were refused: %v", err)
	}
	if _, err := New([]byte(projectRules(numberedRules(ok+1)...)), testProjects()); err == nil {
		t.Errorf("47 project rules (65 in the effective set) were accepted")
	} else if CodeOf(err) != types.CodeScrub002 {
		t.Errorf("code = %s, want %s", CodeOf(err), types.CodeScrub002)
	}
}

// TestConfigAcceptsTheSpecExample: the §3.2 example must load, and its project
// rule must run after rule 18 in declared order.
func TestConfigAcceptsTheSpecExample(t *testing.T) {
	cfg := `
[scrub]
rules_version     = 1
max_bytes_default = 262144
rule_timeout      = "250ms"
prefilter         = true
boundary_verify   = true
entropy           = true
pii_mode          = "redact"
path_mode         = "keep"
path_allowlist    = ["/srv/src", "/opt/apps", "/usr/lib", "/var/log"]
home_roots        = ["/home/*", "/Users/*", "/root", "/var/home/*"]

[scrub.targets.journal_tail]
max_bytes = 65536
on_over   = "truncate"

[scrub.rules.entropy_token]
enabled = true

[scrub.projects."1"]
disable        = ["entropy_token"]
pii_mode       = "keep"
path_allowlist = ["/srv/customer-x"]

[[scrub.projects."1".rules]]
name      = "customer_account_id"
kind      = "regex"
pattern   = "(?i)(?:^|[^A-Za-z0-9])(?:acct|customer)[_-]?id\\s*[:=]\\s*([0-9]{6,12})"
replace   = "[REDACTED:customer_account_id]"
mandatory = false
targets   = ["event_msg", "stack", "issue"]
rescan    = false
`
	e, err := New([]byte(cfg), testProjects())
	if err != nil {
		t.Fatalf("the SPEC-02 §3.2 example was refused: %v", err)
	}
	rules := e.Rules("1")
	last := rules[len(rules)-1]
	if last.Name != "customer_account_id" {
		t.Fatalf("project rules do not append after rule 18: last rule is %s", last.Name)
	}
	out, res, err := e.ScrubString(newCtx(), types.TgEventMsg, "1", "customer_id=918273645 failed")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[REDACTED:customer_account_id]") {
		t.Errorf("project rule did not fire: %q", out)
	}
	if res.ByRule["customer_account_id"] != 1 {
		t.Errorf("by_rule = %s, want customer_account_id=1", byRuleString(res.ByRule))
	}
	// the project disabled entropy_token and kept pii: the effective set differs
	// from the global one only by those two rules
	glob := e.Rules("")
	// project 1: -entropy_token, pii_mode = keep (-2 pii rules), +1 project rule
	if len(rules) != len(glob)-3+1 {
		t.Errorf("project table has %d rules, want %d (global %d - entropy - 2 pii + 1 project)",
			len(rules), len(glob)-3+1, len(glob))
	}
	// pii_mode = keep for the project: an email in the project's events survives
	out2, _, _ := e.ScrubString(newCtx(), types.TgEventMsg, "1", "contact alice@example.com")
	if !strings.Contains(out2, "alice@example.com") {
		t.Errorf("project pii_mode = keep did not disable the pii rules: %q", out2)
	}
	out3, _, _ := e.ScrubString(newCtx(), types.TgEventMsg, "2", "contact alice@example.com")
	if strings.Contains(out3, "alice@example.com") {
		t.Errorf("the global pii_mode = redact did not apply to project 2: %q", out3)
	}
}

// TestConfigFlatFormIsAccepted: internal/lifecycle may hand over either the
// [scrub] subtree or a document that still carries the header.
func TestConfigFlatFormIsAccepted(t *testing.T) {
	flat := "max_bytes_default = 4096\nrules_version = 1\n"
	e, err := New([]byte(flat), testProjects())
	if err != nil {
		t.Fatalf("flat [scrub] subtree rejected: %v", err)
	}
	if got := e.budget(types.TgEventMsg).maxBytes; got != 4096 {
		t.Errorf("max_bytes_default = %d, want 4096", got)
	}
}

// projectRules renders [[scrub.projects."1".rules]] blocks from inline-table
// rows: TOML does not allow an inline table as the body of an array-of-tables
// header, so each row is expanded into key lines.
func projectRules(rows ...string) string {
	var b strings.Builder
	b.WriteString("[scrub]\n")
	for _, row := range rows {
		b.WriteString("[[scrub.projects.\"1\".rules]]\n")
		for _, kv := range splitInlineTable(row) {
			b.WriteString(kv)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// splitInlineTable splits "{a = 1, b = \"x\"}" into key lines, ignoring commas
// inside brackets and quotes.
func splitInlineTable(row string) []string {
	row = strings.TrimSpace(row)
	row = strings.TrimPrefix(row, "{")
	row = strings.TrimSuffix(row, "}")
	var out []string
	depth, inQuote := 0, byte(0)
	start := 0
	for i := 0; i < len(row); i++ {
		switch c := row[i]; {
		case inQuote != 0:
			if c == inQuote {
				inQuote = 0
			}
		case c == '"' || c == '\'':
			inQuote = c
		case c == '[' || c == '{':
			depth++
		case c == ']' || c == '}':
			depth--
		case c == ',' && depth == 0:
			out = append(out, strings.TrimSpace(row[start:i]))
			start = i + 1
		}
	}
	if s := strings.TrimSpace(row[start:]); s != "" {
		out = append(out, s)
	}
	return out
}

func numberedRules(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf(`{name = "r%02d", kind = "regex", pattern = "x%02d", replace = "[REDACTED:r%02d]", targets = ["event_msg"]}`, i, i, i))
	}
	return out
}

func rescanRules(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf(`{name = "s%02d", kind = "regex", pattern = "x%02d", replace = "[REDACTED:s%02d]", targets = ["event_msg"], rescan = true}`, i, i, i))
	}
	return out
}
