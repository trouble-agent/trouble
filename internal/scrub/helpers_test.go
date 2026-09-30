package scrub

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// The two projects every test engine is built with. The public keys are the
// designed-public credentials of SPEC-02 §3.5.
const (
	testPubKeyA = "a1b2c3d4e5f60718293a4b5c6d7e8f90"
	testPubKeyB = "0f1e2d3c4b5a69788796a5b4c3d2e1f0"
)

func testProjects() []types.Project {
	return []types.Project{
		{ID: "1", Slug: "alpha", PublicKey: testPubKeyA, SecretKey: "00112233445566778899aabbccddeeff", Enabled: true},
		{ID: "2", Slug: "beta", PublicKey: testPubKeyB, Enabled: true},
	}
}

// newTestEngine builds an engine from a config document (nil = defaults).
func newTestEngine(t *testing.T, cfg string) *Engine {
	t.Helper()
	e, err := New([]byte(cfg), testProjects())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

// newCtx is the background context every non-cancelled test call uses.
func newCtx() context.Context { return context.Background() }

// scrubString is the assertion-friendly entry point: it fails the test on error.
func scrubString(t *testing.T, e *Engine, target types.ScrubTarget, projectID, in string) (string, types.ScrubResult) {
	t.Helper()
	out, res, err := e.ScrubString(context.Background(), target, projectID, in)
	if err != nil {
		t.Fatalf("ScrubString(%q): %v", in, err)
	}
	return out, res
}

func sameByRule(got map[string]int, want map[string]int) bool {
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

func byRuleString(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+itoa(m[k]))
	}
	return strings.Join(parts, ",")
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		p--
		b[p] = '-'
	}
	return string(b[p:])
}

// readTestdata reads a fixture under testdata/.
func readTestdata(rel string) (string, error) {
	b, err := os.ReadFile(filepath.Join("testdata", rel))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// mustMkdirTemp makes a state root that the ledger accepts (never under /tmp,
// SPEC-01 §4.3) and cleans it up with the test. It must NOT be derived from the
// repo cwd: when the checkout itself lives under /tmp (ephemeral CI clone,
// review clone), TROUBLE-LEDGER-012 would refuse every state root. Resolve a
// writable scratch root outside /tmp instead: $TROUBLE_TEST_SCRATCH_ROOT if
// set, else the user cache dir, else $HOME. If all of those fail we still fall
// back to the old cwd behavior rather than skip the test.
func mustMkdirTemp(t *testing.T, prefix string) string {
	t.Helper()
	root := testScratchRoot(t)
	dir, err := os.MkdirTemp(root, prefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// testScratchRoot picks a writable directory for scratch state roots that is
// guaranteed not under /tmp even when the repo cwd is.
func testScratchRoot(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("TROUBLE_TEST_SCRATCH_ROOT"); v != "" {
		return v
	}
	if cache, err := os.UserCacheDir(); err == nil && cache != "" {
		root := filepath.Join(cache, "trouble-test-scratch")
		if err := os.MkdirAll(root, 0o755); err == nil {
			return root
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		root := filepath.Join(home, ".cache", "trouble-test-scratch")
		if err := os.MkdirAll(root, 0o755); err == nil {
			return root
		}
	}
	// Last resort: the repo cwd (pre-fix behavior). Tests running there still
	// fail the ledger guard, but only when nothing better exists.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

func writeFileT(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestMustMkdirTempNotUnderTmpOrRepoCwd is the TRBL-090 regression test: the
// scratch state root must never be under /tmp (TROUBLE-LEDGER-012 refuses it,
// SPEC-01 §4.3) even when the checkout itself lives under /tmp, and must not
// pollute the repo cwd. Replicates the guard's predicate locally (test-only;
// internal/ledger is not modified).
func TestMustMkdirTempNotUnderTmpOrRepoCwd(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := mustMkdirTemp(t, ".scrub-regression-")
	if clean := filepath.Clean(dir); clean == "/tmp" || strings.HasPrefix(clean, "/tmp/") {
		t.Fatalf("scratch root %s is under /tmp; ledger guard TROUBLE-LEDGER-012 would refuse it", dir)
	}
	if rel, err := filepath.Rel(wd, dir); err == nil && !strings.HasPrefix(rel, "..") {
		t.Fatalf("scratch root %s is inside repo cwd %s", dir, wd)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("scratch root %s does not exist: %v", dir, err)
	}
}
