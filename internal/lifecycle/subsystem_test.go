package lifecycle

// subsystem_test.go pins the two optional subsystem tables as CONFIG KEYS
// (SPEC-12 §3.1b): `[issues]` and `[skills]` are registered in the resolved
// config schema, so a file that declares them resolves instead of dying on
// `unknown file key "issues.enabled"` — the state TRBL-020 was filed against —
// and each table's keys are handed to the subsystem that owns them rather than
// being checked against this package's flat allowlist.
//
// The three halves are load-bearing and are asserted separately:
//
//  1. the keys are REGISTERED (a documented opt-in resolves, and the resolved
//     Config carries the table text plus the declared key names);
//  2. the row carries PROVENANCE (source=file with the config path) and never a
//     value, because a value of these tables can be a credential path;
//  3. the ABSORPTION is scoped (a typo at the root is still 001, a scalar from
//     envgit or argv is refused by name, and an absent table stays absent).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// documentedIssuesOptIn is the `[issues]` opt-in exactly as SPEC-09 §3.4a and
// examples/config.toml document it: the desk on, with the github driver's
// owner/repo pair (the token comes from token_env or a 0600 token_file).
const documentedIssuesOptIn = `[issues]
enabled = true
primary_driver = "github"

[issues.drivers.github]
enabled = true
owner = "acme"
repo = "payment-api"
`

// documentedSkillsOptIn is the `[skills]` opt-in of SPEC-11 §2a: the loop on with
// exactly one source.
const documentedSkillsOptIn = `[skills]
enabled = true
source_path = "/srv/skills/release.git"
`

func writeConfigFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestSubsystemTablesAreRegisteredKeys is half 1: the documented opt-in of both
// subsystems resolves through the real resolver, and the resolved config carries
// each table as one value.
func TestSubsystemTablesAreRegisteredKeys(t *testing.T) {
	path := writeConfigFile(t, documentedIssuesOptIn+"\n"+documentedSkillsOptIn)

	res, err := Resolve(nil, nil, path)
	if err != nil {
		t.Fatalf("Resolve(%s) = %v\n"+
			"the documented [issues]/[skills] opt-in must be a registered config key, not an unknown file key (TRBL-020)", path, err)
	}

	iss := res.Config.IssuesTable
	if !iss.Declared() {
		t.Fatalf("res.Config.IssuesTable = %+v, want the declared [issues] table", iss)
	}
	for _, want := range []string{"[issues]", "[issues.drivers.github]", `owner = "acme"`, `repo = "payment-api"`, "enabled = true"} {
		if !strings.Contains(iss.Text, want) {
			t.Errorf("[issues] table text is missing %q: the owner reads the file's own bytes\n--- text ---\n%s", want, iss.Text)
		}
	}
	wantKeys := []string{"drivers.github.enabled", "drivers.github.owner", "drivers.github.repo", "enabled", "primary_driver"}
	if strings.Join(iss.Keys, ",") != strings.Join(wantKeys, ",") {
		t.Errorf("[issues] declared keys = %v, want %v (relative to the table root, sorted)", iss.Keys, wantKeys)
	}

	sk := res.Config.SkillsTable
	if !sk.Declared() {
		t.Fatalf("res.Config.SkillsTable = %+v, want the declared [skills] table", sk)
	}
	if !strings.Contains(sk.Text, `source_path = "/srv/skills/release.git"`) {
		t.Errorf("[skills] table text is missing the source key: %s", sk.Text)
	}
	if strings.Join(sk.Keys, ",") != "enabled,source_path" {
		t.Errorf("[skills] declared keys = %v, want [enabled source_path]", sk.Keys)
	}

	// Both tables resolve to exactly one row each: the keys inside them are the
	// subsystem's, and this package's one-row-per-key rule still holds.
	rows := map[string]types.ConfigValue{}
	for _, cv := range res.Values {
		if strings.HasPrefix(cv.Key, "issues.") || strings.HasPrefix(cv.Key, "skills.") {
			t.Errorf("resolved values carry a per-sub-key row %q: the table is ONE resolved value (SPEC-12 §3.1b)", cv.Key)
		}
		rows[cv.Key] = cv
	}
	for _, k := range []string{"issues", "skills"} {
		if _, ok := rows[k]; !ok {
			t.Fatalf("resolved values carry no %q row", k)
		}
	}
}

// TestSubsystemTableRowCarriesProvenanceAndNoValue is half 2: the row names the
// declaration and its source, and never a value of the table.
func TestSubsystemTableRowCarriesProvenanceAndNoValue(t *testing.T) {
	// A credential PATH is the shape this row must not echo (SPEC-09 §3.4 prints
	// token_file/api_key_file Redacted=true); the declared keys are all it may
	// carry.
	doc := `[issues]
enabled = true
primary_driver = "duckbrain"

[issues.drivers.duckbrain]
enabled = true
base_url = "http://127.0.0.1:7645"
api_key_file = "/home/op/.config/trouble/duckbrain.key"
`
	path := writeConfigFile(t, doc)
	res, err := Resolve(nil, nil, path)
	if err != nil {
		t.Fatalf("Resolve = %v", err)
	}

	var row types.ConfigValue
	found := false
	for _, cv := range res.Values {
		if cv.Key == "issues" {
			row, found = cv, true
		}
	}
	if !found {
		t.Fatal("no `issues` row in the resolved values")
	}
	if row.Source != "file" || row.SourceRef != path {
		t.Errorf("issues row provenance = (%q, %q), want (\"file\", %q)", row.Source, row.SourceRef, path)
	}
	if row.Redacted {
		t.Errorf("issues row is marked redacted: the row carries key NAMES, so there is nothing to redact")
	}
	value, _ := row.Value.(string)
	if !strings.Contains(value, "enabled") || !strings.Contains(value, "drivers.duckbrain.api_key_file") {
		t.Errorf("issues row value = %q, want the declared key names", value)
	}
	if strings.Contains(value, "/home/op/.config/trouble/duckbrain.key") || strings.Contains(value, "7645") {
		t.Errorf("issues row value = %q echoes a value from the table; the row may only carry key names", value)
	}

	// The same row travels in the boot config record, so the marshalled dump must
	// not carry the credential path either.
	b, err := explainJSON(res.Values)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "duckbrain.key") {
		t.Errorf("marshalled explain dump carries the credential path: %s", b)
	}
}

// TestSubsystemTableNotDeclaredStaysNotDeclared is the other side: a file with no
// such table resolves, the row says so, and the compiled default stands (the
// subsystem is OFF — SPEC-09 §3.4a / SPEC-11 §2a).
func TestSubsystemTableNotDeclaredStaysNotDeclared(t *testing.T) {
	path := writeConfigFile(t, "state_root = \"/x\"\n")
	res, err := Resolve(nil, nil, path)
	if err != nil {
		t.Fatalf("Resolve = %v", err)
	}
	if res.Config.IssuesTable.Declared() || res.Config.SkillsTable.Declared() {
		t.Errorf("a file without [issues]/[skills] produced tables: issues=%+v skills=%+v",
			res.Config.IssuesTable, res.Config.SkillsTable)
	}
	for _, cv := range res.Values {
		if cv.Key != "issues" && cv.Key != "skills" {
			continue
		}
		if cv.Source != "default" || cv.SourceRef != "builtin" {
			t.Errorf("%s row provenance = (%q, %q), want the builtin default", cv.Key, cv.Source, cv.SourceRef)
		}
		if cv.Value != "not declared" {
			t.Errorf("%s row value = %v, want \"not declared\"", cv.Key, cv.Value)
		}
	}
}

// TestSubsystemTableRootLevelDottedKeyDeclaresTheTable: TOML lets the table be
// written without a header (`issues.enabled = true`), and the reader must
// attribute those keys to the table exactly as it does for `[issues]`.
func TestSubsystemTableRootLevelDottedKeyDeclaresTheTable(t *testing.T) {
	path := writeConfigFile(t, "issues.enabled = true\nskills.enabled = false\n")
	res, err := Resolve(nil, nil, path)
	if err != nil {
		t.Fatalf("Resolve = %v", err)
	}
	if !res.Config.IssuesTable.Declared() {
		t.Fatalf("issues table not declared from a root-level dotted key: %+v", res.Config.IssuesTable)
	}
	if got := strings.TrimSpace(res.Config.IssuesTable.Text); got != "issues.enabled = true" {
		t.Errorf("issues table text = %q, want the declared line verbatim", got)
	}
	if got := strings.Join(res.Config.IssuesTable.Keys, ","); got != "enabled" {
		t.Errorf("issues declared keys = %q, want \"enabled\"", got)
	}
	if !res.Config.SkillsTable.Declared() {
		t.Errorf("skills table not declared from a root-level dotted key: %+v", res.Config.SkillsTable)
	}
}

// TestSubsystemTableAbsorptionIsScoped is half 3: registering the tables must not
// turn the file allowlist into a prefix wildcard.
func TestSubsystemTableAbsorptionIsScoped(t *testing.T) {
	t.Run("a typo at the root is still 001", func(t *testing.T) {
		path := writeConfigFile(t, "issue.enabled = true\n")
		_, err := Resolve(nil, nil, path)
		if err == nil {
			t.Fatal("Resolve accepted `issue.enabled` (a typo of `issues.enabled`), want TROUBLE-LIFECYCLE-001")
		}
		if !strings.Contains(err.Error(), string(types.CodeLifecycle001)) || !strings.Contains(err.Error(), "issue.enabled") {
			t.Errorf("refusal = %v, want 001 naming the unknown key", err)
		}
	})

	t.Run("an unregistered sibling table is still 001", func(t *testing.T) {
		path := writeConfigFile(t, "[sentinel.routes]\nproxy = true\n")
		_, err := Resolve(nil, nil, path)
		if err == nil {
			t.Fatal("Resolve accepted [sentinel.routes], want TROUBLE-LIFECYCLE-001")
		}
		if !strings.Contains(err.Error(), "sentinel.routes.proxy") {
			t.Errorf("refusal = %v, want it to name the unknown key", err)
		}
	})

	t.Run("a scalar from env is refused by name", func(t *testing.T) {
		path := writeConfigFile(t, "state_root = \"/x\"\n")
		_, err := Resolve(nil, []string{"TROUBLE_ISSUES=true"}, path)
		if err == nil {
			t.Fatal("Resolve accepted TROUBLE_ISSUES=true, want the by-name refusal: a table cannot come from a scalar source")
		}
		if !strings.Contains(err.Error(), "issues") || !strings.Contains(err.Error(), "is a table") {
			t.Errorf("refusal = %v, want it to name `issues` as a table", err)
		}
	})

	t.Run("a scalar from argv is refused by name", func(t *testing.T) {
		path := writeConfigFile(t, "state_root = \"/x\"\n")
		_, err := Resolve([]string{"--skills", "on"}, nil, path)
		if err == nil {
			t.Fatal("Resolve accepted --skills on, want the by-name refusal")
		}
		if !strings.Contains(err.Error(), "skills") || !strings.Contains(err.Error(), "is a table") {
			t.Errorf("refusal = %v, want it to name `skills` as a table", err)
		}
	})

	t.Run("the table key is not a prefix wildcard", func(t *testing.T) {
		// A root-level key that shares a prefix with a table is not under it: the
		// table owns `issues.<key>`, never `issues<more>`.
		bad := writeConfigFile(t, "issues_note = \"x\"\n\n[issues]\nenabled = true\n")
		_, err := Resolve(nil, nil, bad)
		if err == nil {
			t.Fatal("Resolve accepted the root-level key `issues_note`, want 001: only keys under the registered root belong to the table")
		}
		if !strings.Contains(err.Error(), "issues_note") {
			t.Errorf("refusal = %v, want it to name the unknown key", err)
		}
	})

	t.Run("a table declared twice is refused by the reader, not by the subsystem", func(t *testing.T) {
		// TOML declares a table once. The subset reader would otherwise fold both
		// declarations into one document and hand the operator a line number from
		// an extracted fragment; the repeated header is named here instead.
		bad := writeConfigFile(t, "[issues]\nenabled = false\n\n[issues.drivers.github]\nowner = \"acme\"\n\n[issues]\nenabled = true\n")
		_, err := Resolve(nil, nil, bad)
		if err == nil {
			t.Fatal("Resolve accepted [issues] declared twice, want 001")
		}
		if !strings.Contains(err.Error(), string(types.CodeLifecycle001)) || !strings.Contains(err.Error(), "declared") {
			t.Errorf("refusal = %v, want 001 naming the repeated table", err)
		}
	})

	t.Run("sub-table order is the legal one", func(t *testing.T) {
		// `[issues]` followed by its sub-table is legal TOML and must stay legal:
		// only the repeat and the reopen are refused.
		ok := writeConfigFile(t, "[issues]\nenabled = true\nprimary_driver = \"duckbrain\"\n\n[issues.drivers.github]\nenabled = false\n\n[issues.drivers.duckbrain]\nenabled = true\n")
		if _, err := Resolve(nil, nil, ok); err != nil {
			t.Errorf("Resolve refused a legal [issues] + [issues.drivers.*] file: %v", err)
		}
	})
}
