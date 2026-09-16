package lifecycle

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestUpgradeParkFailureNoRename(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "trouble")
	if err := os.WriteFile(bin, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	cfg.StateRoot = t.TempDir()
	plan := upgradePlan{
		NewBinary: bin,
		Park: func(ctx context.Context) (int, error) {
			return 0, context.Canceled
		},
		Restart: func(ctx context.Context) error { return nil },
		Ready:   func(ctx context.Context) bool { return true },
	}
	err := Upgrade(context.Background(), *cfg, plan)
	if err == nil {
		t.Fatal("expected park failure")
	}
	b, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "old" {
		t.Errorf("binary was modified despite park failure")
	}
}

func TestUpgradeRenameOverSucceeds(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "trouble")
	if err := os.WriteFile(bin, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	newBin := filepath.Join(t.TempDir(), "trouble.new")
	if err := os.WriteFile(newBin, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	cfg.StateRoot = t.TempDir()
	called := false
	plan := upgradePlan{
		NewBinary: newBin,
		Park: func(ctx context.Context) (int, error) {
			return 0, nil
		},
		Restart: func(ctx context.Context) error {
			called = true
			return nil
		},
		Ready: func(ctx context.Context) bool { return true },
	}
	if err := Upgrade(context.Background(), *cfg, plan); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if !called {
		t.Error("restart not called")
	}
	b, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "new" {
		t.Errorf("binary not replaced")
	}
}

func TestETXTBSYRegression(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "trouble")
	if err := os.WriteFile(bin, []byte("live"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Run a long-lived copy of this binary to hold the inode busy.
	cmd := exec.Command(bin)
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start helper binary: %v", err)
	}
	defer cmd.Process.Kill()

	f, err := os.OpenFile(bin, os.O_WRONLY|os.O_TRUNC, 0)
	if err == nil {
		f.Close()
		t.Fatal("expected ETXTBSY opening live binary for write")
	}
}
