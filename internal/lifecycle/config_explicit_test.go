package lifecycle

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

func TestExplicitConfigRefusesUnavailableFile(t *testing.T) {
	dir := t.TempDir()
	unreadable := filepath.Join(dir, "unreadable.toml")
	if err := os.WriteFile(unreadable, []byte("state_root = \"x\"\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o600) })
	cases := []struct {
		name, path, reason string
	}{
		{"missing", filepath.Join(dir, "missing.toml"), "no such file"},
		{"unreadable", unreadable, "permission denied"},
		{"directory", dir, "is a directory"},
		{"empty", "", "no such file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "unreadable" {
				f, err := os.Open(tc.path)
				if err == nil {
					_ = f.Close()
					t.Skip("current credentials can read mode 000")
				}
			}
			_, err := ResolveExplicit(nil, nil, tc.path)
			if !errors.Is(err, types.CodeLifecycle018) || !strings.Contains(err.Error(), tc.path) || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("ResolveExplicit(%q): %v", tc.path, err)
			}
		})
	}
	if _, err := Resolve(nil, nil, filepath.Join(dir, "missing.toml")); err != nil {
		t.Fatalf("optional discovery: %v", err)
	}
}
