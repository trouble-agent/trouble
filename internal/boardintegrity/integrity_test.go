package boardintegrity

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newTempGitRepo creates a fully isolated git repo in a temp dir and
// makes one commit. It returns the repo dir and the HEAD SHA.
func newTempGitRepo(t *testing.T) (repoDir, headSHA string) {
	t.Helper()
	tmp := t.TempDir()

	// Create a fully isolated git repo
	cmd := exec.Command("git", "init")
	cmd.Dir = tmp
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, string(out))
	}

	// Configure git so commits get an identity
	cmd = exec.Command("git", "config", "user.email", "test@test.com")
	cmd.Dir = tmp
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("git", "config", "user.name", "Test")
	cmd.Dir = tmp
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}

	// Create a commit
	file := filepath.Join(tmp, "README.md")
	if err := os.WriteFile(file, []byte("# test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("git", "add", "README.md")
	cmd.Dir = tmp
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("git", "commit", "-m", "init")
	cmd.Dir = tmp
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}

	// Read SHA directly from the ref file (more reliable than rev-parse in tests)
	refDir := filepath.Join(tmp, ".git", "refs", "heads")
	// Try master first (new repos default to master), then main
	refFile := filepath.Join(refDir, "master")
	if _, err := os.Stat(refFile); os.IsNotExist(err) {
		refFile = filepath.Join(refDir, "main")
	}
	refContent, err := os.ReadFile(refFile)
	if err != nil {
		t.Fatalf("read ref %s: %v", refFile, err)
	}
	return tmp, strings.TrimSpace(string(refContent))
}

// TestPhantomShaFixture proves that a row closed against a non-existent
// SHA is caught by the validation: git cat-file on 9a8b7c6 must fail.
func TestPhantomShaFixture(t *testing.T) {
	tmp, _ := newTempGitRepo(t)

	boardPath := filepath.Join(tmp, ".coding-hermes", "board", "tasks.jsonl")
	if err := os.MkdirAll(filepath.Dir(boardPath), 0o755); err != nil {
		t.Fatal(err)
	}
	board := `{"id":"TRBL-FAKE","status":"complete","commit_hash":"9a8b7c6","description":"phantom"}` + "\n"
	if err := os.WriteFile(boardPath, []byte(board), 0o644); err != nil {
		t.Fatal(err)
	}

	results, err := Validate(tmp, boardPath)
	if err != nil {
		t.Fatal(err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	r := results[0]
	if r.Exists {
		t.Errorf("expected phantom SHA 9a8b7c6 to NOT resolve, but it did")
	}
	if r.RowID != "TRBL-FAKE" {
		t.Errorf("expected row_id TRBL-FAKE, got %s", r.RowID)
	}
	if r.Msg == "" {
		t.Error("expected error message, got empty")
	}
}

// TestRealShaPasses proves that a valid SHA on main passes validation.
func TestRealShaPasses(t *testing.T) {
	tmp, realSHA := newTempGitRepo(t)

	boardPath := filepath.Join(tmp, ".coding-hermes", "board", "tasks.jsonl")
	if err := os.MkdirAll(filepath.Dir(boardPath), 0o755); err != nil {
		t.Fatal(err)
	}
	board := `{"id":"TRBL-REAL","status":"complete","commit_hash":"` + realSHA + `","description":"real"}` + "\n"
	if err := os.WriteFile(boardPath, []byte(board), 0o644); err != nil {
		t.Fatal(err)
	}

	results, err := Validate(tmp, boardPath)
	if err != nil {
		t.Fatal(err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	r := results[0]
	if !r.Exists {
		t.Errorf("expected real SHA %s to resolve, but it did not", realSHA)
	}
	if r.Branch != "" {
		t.Errorf("expected no branch for a main commit, got %q", r.Branch)
	}
}

// TestNonCompleteRowsSkipped proves that incomplete rows are not validated.
func TestNonCompleteRowsSkipped(t *testing.T) {
	tmp, realSHA := newTempGitRepo(t)

	boardPath := filepath.Join(tmp, ".coding-hermes", "board", "tasks.jsonl")
	if err := os.MkdirAll(filepath.Dir(boardPath), 0o755); err != nil {
		t.Fatal(err)
	}
	board := `{"id":"TRBL-OPEN","status":"open","commit_hash":"0000000","description":"open"}` + "\n" +
		`{"id":"TRBL-BLANK","status":"complete","commit_hash":"","description":"no hash"}` + "\n" +
		`{"id":"TRBL-COMPLETE","status":"complete","commit_hash":"` + realSHA + `","description":"complete"}` + "\n"
	if err := os.WriteFile(boardPath, []byte(board), 0o644); err != nil {
		t.Fatal(err)
	}

	results, err := Validate(tmp, boardPath)
	if err != nil {
		t.Fatal(err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result (only complete with non-empty commit_hash), got %d", len(results))
	}
	if results[0].RowID != "TRBL-COMPLETE" {
		t.Errorf("expected only complete rows, got %s", results[0].RowID)
	}
}

// TestSummaryFormat proves that Summary produces the expected output format.
func TestSummaryFormat(t *testing.T) {
	results := []Result{
		{RowID: "TRBL-A", SHA: "abc123", Exists: true, Branch: "", Msg: ""},
		{RowID: "TRBL-B", SHA: "9a8b7c6", Exists: false, Branch: "", Msg: "not found"},
		{RowID: "TRBL-C", SHA: "def456", Exists: true, Branch: "wt/feat", Msg: "unmerged"},
	}

	sum := Summary(results)
	if !strings.Contains(sum, "OK    TRBL-A") {
		t.Errorf("summary missing OK line for TRBL-A: %s", sum)
	}
	if !strings.Contains(sum, "FAIL  TRBL-B") {
		t.Errorf("summary missing FAIL line for TRBL-B: %s", sum)
	}
	if !strings.Contains(sum, "WARN  TRBL-C") {
		t.Errorf("summary missing WARN line for TRBL-C: %s", sum)
	}
	if !strings.Contains(sum, "Total rows examined: 3") {
		t.Errorf("summary missing total count: %s", sum)
	}
	if !strings.Contains(sum, "Unresolved closes: 1") {
		t.Errorf("summary missing unresolved count: %s", sum)
	}
}
