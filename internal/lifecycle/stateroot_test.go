package lifecycle

import (
	"fmt"
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
	cfg.FS.ForbiddenStateRoots = nil
	_, err := CheckStateRoot(*cfg)
	if err == nil {
		t.Fatal("expected error for mode 0755")
	}
	if !strings.Contains(err.Error(), "TROUBLE-LIFECYCLE-005") {
		t.Errorf("expected 005, got %v", err)
	}
}

// TestCheckStateRootWrongModeNamesTheRequiredMode pins TRBL-087: the refusal
// has to be self-repairing. A state root created by a plain mkdir is refused
// correctly, but under a umask-022 shell it is 0755 and under this box's
// umask-002 it is 0775 — in both cases the operator must not have to read the
// source to learn the required mode or the command that applies it.
func TestCheckStateRootWrongModeNamesTheRequiredMode(t *testing.T) {
	cases := []struct {
		name string
		mode os.FileMode
	}{
		{"mkdir under umask 022 -> 0755", 0o755},
		{"mkdir under umask 002 -> 0775", 0o775},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			root := filepath.Join(dir, "trouble")
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			// Chmod rather than MkdirAll's mode: the process umask masks it,
			// and the assertion is about the message, not the host umask.
			if err := os.Chmod(root, tc.mode); err != nil {
				t.Fatal(err)
			}
			cfg := defaults()
			cfg.StateRoot = root
			cfg.FS.ForbiddenStateRoots = nil
			_, err := CheckStateRoot(*cfg)
			if err == nil {
				t.Fatalf("mode %04o: expected the 005 refusal", tc.mode)
			}
			msg := err.Error()
			if !strings.Contains(msg, "TROUBLE-LIFECYCLE-005") {
				t.Errorf("expected 005, got %v", err)
			}
			if !strings.Contains(msg, fmt.Sprintf("mode is %04o", tc.mode)) {
				t.Errorf("the refusal does not name the observed mode %04o: %v", tc.mode, err)
			}
			// The required mode, named, and the remedy an operator can paste.
			for _, want := range []string{"0700", fmt.Sprintf("chmod 0700 %q", root)} {
				if !strings.Contains(msg, want) {
					t.Errorf("the refusal does not tell the operator %q:\n%v", want, err)
				}
			}
		})
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
