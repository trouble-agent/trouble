package ledger_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/trouble-agent/trouble/internal/ledger"
)

// TestReadHeadActiveDay_basic verifies the library function used by the
// TRBL-112 fix: it reads the active day from the HEAD hint without locking.
func TestReadHeadActiveDay_basic(t *testing.T) {
	root := t.TempDir()

	// Write a HEAD hint for a known date.
	head := `{"last_seq":10,"day":"2026-01-15","gen":0,"part":1,"bytes":100,"ts":"2026-01-15T00:00:00Z","version":"abc123","git_sha":"abc123"}`
	if err := os.WriteFile(filepath.Join(root, "HEAD"), []byte(head), 0o600); err != nil {
		t.Fatalf("write HEAD: %v", err)
	}

	day, ok := ledger.ReadHeadActiveDay(root)
	if !ok {
		t.Fatal("ReadHeadActiveDay should return true when HEAD exists")
	}
	if day != "2026-01-15" {
		t.Fatalf("expected day 2026-01-15, got %q", day)
	}
}

// TestReadHeadActiveDay_absent verifies behavior when no HEAD exists.
func TestReadHeadActiveDay_absent(t *testing.T) {
	root := t.TempDir()

	day, ok := ledger.ReadHeadActiveDay(root)
	if ok {
		t.Fatal("ReadHeadActiveDay should return false when HEAD is absent")
	}
	if day != "" {
		t.Fatalf("expected empty day, got %q", day)
	}
}

// TestReadHeadActiveDay_corrupt verifies behavior with a corrupt HEAD.
func TestReadHeadActiveDay_corrupt(t *testing.T) {
	root := t.TempDir()

	if err := os.WriteFile(filepath.Join(root, "HEAD"), []byte("not json"), 0o600); err != nil {
		t.Fatalf("write corrupt HEAD: %v", err)
	}

	day, ok := ledger.ReadHeadActiveDay(root)
	if ok {
		t.Fatal("ReadHeadActiveDay should return false for corrupt HEAD")
	}
	if day != "" {
		t.Fatalf("expected empty day, got %q", day)
	}
}
