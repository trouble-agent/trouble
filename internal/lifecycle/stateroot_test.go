package lifecycle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckStateRootOK(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "trouble")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	cfg.StateRoot = root
	cfg.FS.ForbiddenStateRoots = nil
	sr, err := CheckStateRoot(*cfg)
	if err != nil {
		t.Fatalf("CheckStateRoot: %v", err)
	}
	if sr.Path != root {
		t.Errorf("path: got %q, want %q", sr.Path, root)
	}
}

func TestCheckStateRootWrongMode(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "trouble")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	cfg.StateRoot = root
	_, err := CheckStateRoot(*cfg)
	if err == nil {
		t.Fatal("expected error for mode 0755")
	}
	if !strings.Contains(err.Error(), "TROUBLE-LIFECYCLE-005") {
		t.Errorf("expected 005, got %v", err)
	}
}

func TestCheckStateRootForbiddenTmp(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "tmp")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	cfg.StateRoot = root
	cfg.FS.ForbiddenStateRoots = []string{tmp}
	_, err := CheckStateRoot(*cfg)
	if err == nil {
		t.Fatal("expected forbidden root refusal")
	}
	if !strings.Contains(err.Error(), "TROUBLE-LIFECYCLE-004") {
		t.Errorf("expected 004, got %v", err)
	}
}

func TestCheckStateRootMissing(t *testing.T) {
	cfg := defaults()
	cfg.StateRoot = "/does/not/exist"
	_, err := CheckStateRoot(*cfg)
	if err == nil {
		t.Fatal("expected error for missing root")
	}
}
