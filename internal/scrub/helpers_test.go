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
// SPEC-01 §4.3) and cleans it up with the test.
func mustMkdirTemp(t *testing.T, prefix string) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(wd, prefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
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
