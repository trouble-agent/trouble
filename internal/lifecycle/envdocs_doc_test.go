package lifecycle

// envdocs_doc_test.go — the drift gate for docs/reference/env-vars.md (DOC-26).
//
// The page documents two halves of the TROUBLE_* surface: the derived twin of
// every registry key, and the literal names the owning subsystems read directly.
// Both can drift silently — a new registry key adds an environment variable
// nobody documented, and a new literal adds an override nobody can find. This
// gate walks the same sources the page cites and fails when they disagree.
//
// It is deliberately a plain Go test (no Makefile hook): `go test
// ./internal/lifecycle/` is already run by the repo's gates, so the check rides
// an existing runner.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// envDocsPath is the page this gate defends, relative to the repo root.
const envDocsPath = "docs/reference/env-vars.md"

// literalVarRe matches a TROUBLE_* literal. A trailing underscore is excluded so
// the TROUBLE_ESCALATE_ prefix (a childEnv fact prefix, not a variable) is not
// mistaken for a name.
var literalVarRe = regexp.MustCompile(`\bTROUBLE_[A-Z0-9_]+\b`)

// repoRootForDocs walks up from the test's working directory until it finds
// go.mod. Deriving the root keeps the gate working if the file moves.
func repoRootForDocs(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

// docTableKeys returns the first-column key of every markdown table row in the
// page, with backticks and separators stripped.
func docTableKeys(t *testing.T, body string) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) < 3 {
			continue
		}
		cell := strings.Trim(strings.TrimSpace(cells[1]), "`")
		switch cell {
		case "", "---", "config key", "variable", "key that names a variable":
			continue
		}
		out[cell]++
	}
	return out
}

// TestEnvDocsEveryRegistryKeyHasARow fails when a registry key has no row in the
// derived table, i.e. its environment variable is undocumented.
func TestEnvDocsEveryRegistryKeyHasARow(t *testing.T) {
	root := repoRootForDocs(t)
	body, err := os.ReadFile(filepath.Join(root, envDocsPath))
	if err != nil {
		t.Fatalf("read %s: %v", envDocsPath, err)
	}
	rows := docTableKeys(t, string(body))

	metas := registry(defaults())
	keys := make([]string, 0, len(metas))
	for _, m := range metas {
		keys = append(keys, m.key)
	}
	sort.Strings(keys)

	var missing []string
	for _, k := range keys {
		if rows[k] == 0 {
			missing = append(missing, fmt.Sprintf("%s (env %s)", k, envName(k)))
		}
	}
	if len(missing) > 0 {
		t.Fatalf("%s is missing %d of %d registry keys; add a row for each:\n  %s",
			envDocsPath, len(missing), len(keys), strings.Join(missing, "\n  "))
	}
}

// TestEnvDocsEveryLiteralIsDocumented fails when a production TROUBLE_* literal
// is neither the derived twin of a registry key nor a documented row. That is
// the drift this page exists to prevent: an override the owning subsystem reads,
// that no operator can find.
func TestEnvDocsEveryLiteralIsDocumented(t *testing.T) {
	root := repoRootForDocs(t)
	body, err := os.ReadFile(filepath.Join(root, envDocsPath))
	if err != nil {
		t.Fatalf("read %s: %v", envDocsPath, err)
	}
	rows := docTableKeys(t, string(body))

	derived := map[string]bool{}
	for _, m := range registry(defaults()) {
		derived[envName(m.key)] = true
	}

	lits := map[string]bool{}
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			base := filepath.Base(path)
			if base == ".git" || base == "node_modules" || base == "testdata" || base == "docs" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// The TROUBLE_ESCALATE_ prefix is a name PREFIX the daemon writes into
		// its children's environment, not a variable anyone sets; strip it so
		// the prefix itself is not demanded as a row.
		text := strings.ReplaceAll(string(b), "TROUBLE_ESCALATE_", "")
		for _, name := range literalVarRe.FindAllString(text, -1) {
			if strings.HasSuffix(name, "_") {
				continue
			}
			lits[name] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	var undocumented []string
	for name := range lits {
		if derived[name] || rows[name] > 0 {
			continue
		}
		undocumented = append(undocumented, name)
	}
	sort.Strings(undocumented)
	if len(undocumented) > 0 {
		t.Fatalf("%s: %d production TROUBLE_* literal(s) are neither derived from a registry key nor documented:\n  %s",
			envDocsPath, len(undocumented), strings.Join(undocumented, "\n  "))
	}
}

// TestEnvDocsKeepsTheCanonicalStatements stops the page being gutted while the
// two gates above stay green: the derivation rule, the precedence quote and the
// no-near-miss statement are the reason the page exists.
func TestEnvDocsKeepsTheCanonicalStatements(t *testing.T) {
	root := repoRootForDocs(t)
	body, err := os.ReadFile(filepath.Join(root, envDocsPath))
	if err != nil {
		t.Fatalf("read %s: %v", envDocsPath, err)
	}
	text := string(body)

	wants := []struct {
		what string
		text string
	}{
		{"derivation rule (source line)", "internal/lifecycle/config.go:2269"},
		{"flag spelling (source line)", "internal/lifecycle/config.go:2272"},
		{"precedence quote", "flag > env > file > default"},
		{"no near-miss hint on stray env keys", "* **There is no near-miss hint.**"},
		{"secret indirection row", "server.redis.password_env"},
	}
	var missing []string
	for _, w := range wants {
		if !strings.Contains(text, w.text) {
			missing = append(missing, w.what+" — expected to contain "+`"`+w.text+`"`)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("%s lost canonical statements:\n  %s", envDocsPath, strings.Join(missing, "\n  "))
	}
}
