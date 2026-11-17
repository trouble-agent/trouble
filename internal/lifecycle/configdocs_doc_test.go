package lifecycle

// configdocs_doc_test.go — the drift gate for docs/reference/config.md (DOC-24).
//
// The page is generated (`trouble config explain --markdown`), so the gate has
// two jobs. The first is the one this repo keeps learning: a registry key must
// not exist without a row and a meaning, and a meaning must not outlive the key
// it describes. The second is that the committed page must be the generator's
// output — byte for byte — so a hand edit, a stale regeneration or a registry
// addition that nobody re-generated for all fail here.
//
// It is a plain Go test, deliberately: `go test ./internal/lifecycle/` is
// already run by the repo's gates, so the check rides an existing runner rather
// than a Makefile target that can rot.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// configDocsPath is the page this gate defends, relative to the repo root.
const configDocsPath = "docs/reference/config.md"

// configDocsGroupRe matches a TOML-table heading: "## `[sensors.journald]`".
var configDocsGroupRe = regexp.MustCompile("^## `\\[([A-Za-z0-9_.]+)\\]`$")

// configDocsRowRe matches a key row: "| `key` | type | default | from | meaning |".
var configDocsRowRe = regexp.MustCompile("^\\| `([^`]+)` \\| ([^|]+) \\| (.*) \\| ([^|]+) \\| (.*) \\|$")

// configDocsRow is one parsed row of the page.
type configDocsRow struct {
	key     string
	typ     string
	def     string
	from    string
	meaning string
}

// configDocsRows parses the page into full keys. The first column is the short
// key inside its table, so the heading is part of the key.
func configDocsRows(t *testing.T, body string) []configDocsRow {
	t.Helper()
	var rows []configDocsRow
	group := ""
	for _, line := range strings.Split(body, "\n") {
		if m := configDocsGroupRe.FindStringSubmatch(line); m != nil {
			group = m[1]
			continue
		}
		if line == "## Top level" {
			group = ""
			continue
		}
		m := configDocsRowRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		key := m[1]
		if group != "" {
			key = group + "." + key
		}
		rows = append(rows, configDocsRow{
			key:     key,
			typ:     strings.TrimSpace(m[2]),
			def:     strings.TrimSpace(m[3]),
			from:    strings.TrimSpace(m[4]),
			meaning: strings.TrimSpace(m[5]),
		})
	}
	return rows
}

// readConfigDocs reads the committed page.
func readConfigDocs(t *testing.T) string {
	t.Helper()
	root := repoRootForDocs(t)
	body, err := os.ReadFile(filepath.Join(root, configDocsPath))
	if err != nil {
		t.Fatalf("read %s: %v", configDocsPath, err)
	}
	return string(body)
}

// TestConfigReferenceMatchesGenerator is the drift gate proper: the committed
// page must be what the generator prints today. A registry key, a default or a
// meaning that changed without a regeneration fails here, and so does a hand
// edit of a generated row.
func TestConfigReferenceMatchesGenerator(t *testing.T) {
	want, err := ConfigReferenceMarkdown()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	got := readConfigDocs(t)
	if got == want {
		return
	}
	// Name the first differing line: a 400-line diff is not a usable failure.
	gotLines := strings.Split(got, "\n")
	wantLines := strings.Split(want, "\n")
	for i := 0; i < len(gotLines) && i < len(wantLines); i++ {
		if gotLines[i] != wantLines[i] {
			t.Fatalf("%s is stale at line %d:\n  committed: %q\n  generated: %q\nregenerate with: go run ./cmd/trouble config explain --markdown > %s",
				configDocsPath, i+1, gotLines[i], wantLines[i], configDocsPath)
		}
	}
	t.Fatalf("%s has %d lines, the generator produces %d: regenerate with: go run ./cmd/trouble config explain --markdown > %s",
		configDocsPath, len(gotLines), len(wantLines), configDocsPath)
}

// TestConfigReferenceIsHostIndependent takes the page's own header claim — "a
// host with no config file, no TROUBLE_* variable and no key flag" produces the
// documented defaults — literally: the bytes must not depend on the environment
// the generator ran in.
//
// This is the class the first cut of the generator missed. Its default cells
// came out of the resolver, which reads HOME, XDG_STATE_HOME, XDG_CONFIG_HOME
// and XDG_RUNTIME_DIR; with HOME unset the resolver falls back to
// /var/lib/trouble and /etc/trouble, and those two literals are not templates,
// so eight cells said something different from what the same generator printed
// on a host with a home directory. The test renders the page once under this
// process's inherited environment (the way the CLI and the drift gate run) and
// then under four others, and requires one set of bytes from all of them.
func TestConfigReferenceIsHostIndependent(t *testing.T) {
	// Baseline: whatever this process inherited, untouched. This is the "as
	// today" half of the criterion, and it is also what the drift gate compares
	// the committed page against.
	baseline, err := ConfigReferenceMarkdown()
	if err != nil {
		t.Fatalf("generate under the inherited environment: %v", err)
	}

	cases := []struct {
		name string
		env  map[string]string
	}{
		{"home-set", map[string]string{"HOME": "/home/operator"}},
		{"home-unset", map[string]string{}},
		{"other-home", map[string]string{"HOME": "/tmp/someotherhome"}},
		{"unusual-xdg", map[string]string{
			"HOME":            "/home/operator",
			"XDG_STATE_HOME":  "/opt/unusual/state",
			"XDG_CONFIG_HOME": "/opt/unusual/config",
		}},
	}

	// Host strings that must never survive into a cell: the bare-host fallbacks
	// this generator must not print, and the sentinel the pinning uses, which a
	// value escaping template() would leak verbatim.
	forbidden := []string{"/var/lib/trouble", "/etc/trouble", configDocPinnedHome}
	if home := os.Getenv("HOME"); home != "" && home != "/" {
		forbidden = append(forbidden, home)
	}

	for _, c := range cases {
		// Control every variable the defaults and the templating read, one
		// variable at a time: an inherited value left in place would make the
		// case test this host instead of the code. An empty value is the way to
		// clear one for the duration of a test (t.Setenv restores it after).
		for _, name := range []string{"HOME", "XDG_STATE_HOME", "XDG_CONFIG_HOME", "XDG_RUNTIME_DIR"} {
			t.Setenv(name, "")
		}
		for name, value := range c.env {
			t.Setenv(name, value)
		}

		got, err := ConfigReferenceMarkdown()
		if err != nil {
			t.Fatalf("%s: generate: %v", c.name, err)
		}
		for _, leak := range append(forbidden, caseValues(c.env)...) {
			if strings.Contains(got, leak) {
				t.Fatalf("%s: the page carries the host path %q; a default cell must be a template, not this host's directory", c.name, leak)
			}
		}
		if got != baseline {
			t.Fatalf("%s: the page is not the same bytes as under the inherited environment (%s)", c.name, configDocsFirstDiff(got, baseline))
		}
	}
}

// caseValues returns one case's environment values, for the leak scan.
func caseValues(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for _, v := range env {
		if len(v) > 1 {
			out = append(out, v)
		}
	}
	return out
}

// configDocsFirstDiff names the first line where two renderings disagree: a
// whole-page diff is not a usable failure message.
func configDocsFirstDiff(got, want string) string {
	gotLines := strings.Split(got, "\n")
	wantLines := strings.Split(want, "\n")
	for i := 0; i < len(gotLines) && i < len(wantLines); i++ {
		if gotLines[i] != wantLines[i] {
			return fmt.Sprintf("line %d:\n  this environment: %q\n  baseline:         %q", i+1, gotLines[i], wantLines[i])
		}
	}
	return fmt.Sprintf("%d lines against %d", len(gotLines), len(wantLines))
}

// TestConfigReferenceCoversRegistry fails when the page and the registry
// disagree in either direction, and when the count the page states is not the
// count the binary actually exposes.
func TestConfigReferenceCoversRegistry(t *testing.T) {
	body := readConfigDocs(t)
	rows := configDocsRows(t, body)

	seen := make(map[string]int, len(rows))
	for _, r := range rows {
		seen[r.key]++
	}

	registered := make(map[string]bool)
	metas := registry(defaults())
	keys := make([]string, 0, len(metas))
	for _, m := range metas {
		registered[m.key] = true
		keys = append(keys, m.key)
	}
	sort.Strings(keys)

	var missing, extra, dup []string
	for _, k := range keys {
		if seen[k] == 0 {
			missing = append(missing, k)
		}
	}
	for k, n := range seen {
		if n > 1 {
			dup = append(dup, fmt.Sprintf("%s (%d rows)", k, n))
		}
		if !registered[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	sort.Strings(dup)

	if len(missing) > 0 || len(extra) > 0 || len(dup) > 0 || len(rows) != len(keys) {
		t.Fatalf("%s and the registry disagree: registry %d keys, page %d rows\n  missing from the page: %v\n  on the page but not in the registry: %v\n  duplicated: %v",
			configDocsPath, len(keys), len(rows), missing, extra, dup)
	}

	// The page states its own measured count; that number must be this run's.
	wantLine := fmt.Sprintf("**Keys at HEAD (measured on this tree):** %d", len(keys))
	if !strings.Contains(body, wantLine) {
		t.Fatalf("%s does not state the measured key count %q — the header count is the claim this gate exists to keep true",
			configDocsPath, wantLine)
	}
}

// TestConfigReferenceRowShape checks that every row carries the five cells the
// page promises: a known type, a default, the layer the value came from, and a
// meaning that says something.
func TestConfigReferenceRowShape(t *testing.T) {
	rows := configDocsRows(t, readConfigDocs(t))
	knownTypes := []string{
		"string", "boolean", "integer", "float", "duration",
		"string array", "array of string arrays", "array of tables",
		"string map", "duration map", "table",
	}
	var bad []string
	for _, r := range rows {
		typ := strings.TrimSuffix(r.typ, " (file only)")
		okType := false
		for _, k := range knownTypes {
			if typ == k {
				okType = true
				break
			}
		}
		if !okType {
			bad = append(bad, fmt.Sprintf("%s: type cell %q is not a type the generator emits", r.key, r.typ))
		}
		if r.def == "" {
			bad = append(bad, r.key+": empty default cell")
		}
		if r.from != "default" {
			bad = append(bad, fmt.Sprintf("%s: from cell %q — the page is generated against an absent config file, so every row came from the built-in default", r.key, r.from))
		}
		// A meaning that restates the key is not a meaning, and a sentence
		// shorter than this is a placeholder.
		if len([]rune(r.meaning)) < 25 {
			bad = append(bad, fmt.Sprintf("%s: meaning is %d characters, which cannot explain the key", r.key, len([]rune(r.meaning))))
		}
		echo := strings.ReplaceAll(strings.ReplaceAll(r.key, ".", " "), "_", " ")
		if strings.EqualFold(strings.TrimSuffix(r.meaning, "."), echo) {
			bad = append(bad, r.key+": meaning restates the key name")
		}
	}
	if len(bad) > 0 {
		t.Fatalf("%s has %d malformed row(s):\n  %s", configDocsPath, len(bad), strings.Join(bad, "\n  "))
	}
}

// TestConfigReferenceNeverPrintsSecretValue fails when the page carries a
// credential: a redacted key must show the empty-string cell (the one exception
// is the environment-file path, which is an indirection, not a secret), the
// [REDACTED:config] placeholder must not leak in, and a token present in this
// process's environment must not appear on the page.
func TestConfigReferenceNeverPrintsSecretValue(t *testing.T) {
	body := readConfigDocs(t)
	rows := configDocsRows(t, body)
	byKey := make(map[string]configDocsRow, len(rows))
	for _, r := range rows {
		byKey[r.key] = r
	}

	if strings.Contains(body, "[REDACTED") {
		t.Fatalf("%s carries the config redaction placeholder; the page must name the indirection, not print a marker", configDocsPath)
	}
	for _, m := range registry(defaults()) {
		if !redacted(m.key) {
			continue
		}
		r, ok := byKey[m.key]
		if !ok {
			t.Fatalf("%s has no row for the redacted key %q", configDocsPath, m.key)
		}
		if r.key == configDocPathSecret {
			if !strings.Contains(r.def, "trouble.env") && !strings.Contains(r.def, "environment") {
				t.Fatalf("%s: %s row shows %q; the env-file default is a path the operator needs", configDocsPath, r.key, r.def)
			}
			continue
		}
		if r.def != configDocSecretCell {
			t.Fatalf("%s: redacted key %s shows %q as its default; it must be the empty-string cell %s", configDocsPath, r.key, r.def, configDocSecretCell)
		}
		if strings.Contains(strings.ToLower(r.meaning), "trouble_") {
			// The meaning may name the variable that supplies the value; that
			// is the indirection working. Nothing to assert beyond its presence.
			continue
		}
		t.Fatalf("%s: redacted key %s does not state which environment variable or file supplies it", configDocsPath, r.key)
	}

	// A live credential in this process's environment must not reach the page.
	for _, name := range []string{"TROUBLE_HUB_TOKEN", "TROUBLE_SERVER_REDIS_URL", "TROUBLE_STATE_ROOT"} {
		val := os.Getenv(name)
		if len(val) < 8 {
			continue
		}
		if strings.Contains(body, val) {
			t.Fatalf("%s carries the value of %s; the page documents defaults and must never print a host value", configDocsPath, name)
		}
	}
}

// TestConfigReferenceKeepsCanonicalQuote stops the page being gutted while the
// gates above stay green: the precedence sentence is the one place the operator
// docs state it, and the spelling rule is the reason the flags differ from the
// keys.
func TestConfigReferenceKeepsCanonicalQuote(t *testing.T) {
	body := readConfigDocs(t)
	for _, want := range []struct{ what, text string }{
		{"precedence quote", "> Precedence is flag > env > file > default."},
		{"flag-spelling quote", "> any registered key: `a.b_c` → `--a-b_c`"},
		{"generator command", "go run ./cmd/trouble config explain --markdown > docs/reference/config.md"},
		{"resolution of the page", "Re-running the generator on this tree produces no diff."},
	} {
		if !strings.Contains(body, want.text) {
			t.Fatalf("%s lost its %s (expected %q)", configDocsPath, want.what, want.text)
		}
	}
}
