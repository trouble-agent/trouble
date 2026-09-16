package lifecycle

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckSecretFiles(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "trouble")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(root, "trouble.env")
	if err := os.WriteFile(envFile, []byte("TOKEN=x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	cfg.StateRoot = root
	cfg.Secrets.EnvironmentFile = envFile
	offenders, err := CheckSecretFiles(*cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) == 0 {
		t.Fatal("expected secret-file offender")
	}
	// fix mode and re-check
	if err := os.Chmod(envFile, 0o600); err != nil {
		t.Fatal(err)
	}
	offenders, err = CheckSecretFiles(*cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) != 0 {
		t.Fatalf("expected no offenders after chmod, got %v", offenders)
	}
}
