package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The mint must be able to place TROUBLE_DASHBOARD_TOKEN into the daemon's
// 0600 env file itself (TRBL-028): the distroless container image has no
// shell, and every path that authors the file from OUTSIDE the container
// fails a shipped rule (host-uid bind mount, host-uid docker cp, 0444 compose
// secret). The mint is the only code path allowed to handle token plaintext,
// so it writes the line.

func TestWriteTokenEnvCreateMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trouble.env")
	if err := writeTokenEnv(path, "tdt_abc123"); err != nil {
		t.Fatalf("writeTokenEnv: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %04o, want 0600", got)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.HasPrefix(string(b), "TROUBLE_DASHBOARD_TOKEN=tdt_abc123\n") {
		t.Fatalf("body = %q, want a single TROUBLE_DASHBOARD_TOKEN line", b)
	}
}

func TestWriteTokenEnvReplacesOnlyTokenLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trouble.env")
	if err := os.WriteFile(path, []byte("# keep\nTROUBLE_HUB_TOKEN=sk_live_old\nTROUBLE_DASHBOARD_TOKEN=tdt_old\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := writeTokenEnv(path, "tdt_new"); err != nil {
		t.Fatalf("writeTokenEnv: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(b)
	for _, want := range []string{"# keep\n", "TROUBLE_HUB_TOKEN=sk_live_old\n", "TROUBLE_DASHBOARD_TOKEN=tdt_new\n"} {
		if !strings.Contains(got, want) {
			t.Fatalf("body %q lost %q", got, want)
		}
	}
	if strings.Contains(got, "tdt_old") || strings.Count(got, "TROUBLE_DASHBOARD_TOKEN=") != 1 {
		t.Fatalf("body %q must carry exactly one fresh token line", got)
	}
	if got := fileMode(t, path); got != 0o600 {
		t.Fatalf("mode = %04o, want 0600 preserved", got)
	}
}

func TestWriteTokenEnvRefusesWideFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trouble.env")
	if err := os.WriteFile(path, []byte("x=1\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	err := writeTokenEnv(path, "tdt_abc")
	if err == nil || !strings.Contains(err.Error(), "want 0600") {
		t.Fatalf("err = %v, want the 0600 refusal", err)
	}
}

func TestWriteTokenEnvRefusesDirectory(t *testing.T) {
	dir := t.TempDir()
	err := writeTokenEnv(dir, "tdt_abc") // the path itself is a directory
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("err = %v, want the not-a-regular-file refusal", err)
	}
}

func TestWriteTokenEnvRefusesEmptyToken(t *testing.T) {
	err := writeTokenEnv(filepath.Join(t.TempDir(), "trouble.env"), "")
	if err == nil || !strings.Contains(err.Error(), "empty token") {
		t.Fatalf("err = %v, want the empty-token refusal", err)
	}
}

// End-to-end through the CLI: run(["dashboard","token","create",...]) with the
// env file chosen via the registered env key, asserting the plaintext reaches
// the env file AND the token store file, in one process.
func TestRunTokenCreateOutputEnv(t *testing.T) {
	base := t.TempDir()
	cfg := filepath.Join(base, "config.toml")
	envf := filepath.Join(base, "trouble.env")
	tokenFile := filepath.Join(base, "dashboard.token")
	body := "state_root = \"" + filepath.Join(base, "state") + "\"\n" +
		"[secrets]\nenvironment_file = \"" + envf + "\"\n" +
		"[dashboard]\ntoken_file = \"" + tokenFile + "\"\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	t.Setenv("TROUBLE_CONFIG_PATH", cfg)
	t.Setenv("TROUBLE_STATE_ROOT", filepath.Join(base, "state"))

	code := run([]string{"dashboard", "token", "create", "--label", "e2e", "--scopes", "read", "--output-env", envf})
	if code != 0 {
		t.Fatalf("run exit = %d, want 0", code)
	}
	b, err := os.ReadFile(envf)
	if err != nil {
		t.Fatalf("env file not written: %v", err)
	}
	line := string(b)
	if !strings.HasPrefix(line, "TROUBLE_DASHBOARD_TOKEN=tdt_") {
		t.Fatalf("env body = %q, want the minted token", line)
	}
	plaintext := strings.TrimPrefix(strings.TrimRight(line, "\n"), "TROUBLE_DASHBOARD_TOKEN=")
	if len(plaintext) != 47 {
		t.Fatalf("plaintext len = %d, want 47", len(plaintext))
	}
	tok, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatalf("token store not written: %v", err)
	}
	if strings.Contains(string(tok), plaintext) {
		t.Fatal("token store must carry the hash, never the plaintext")
	}
	if got := fileMode(t, envf); got != 0o600 {
		t.Fatalf("env mode = %04o, want 0600", got)
	}
}

// Rotate through the same flag: the fresh plaintext lands in the env file and
// the previous one is gone from it (the daemon reads the file at check time).
func TestRunTokenRotateOutputEnv(t *testing.T) {
	base := t.TempDir()
	cfg := filepath.Join(base, "config.toml")
	envf := filepath.Join(base, "trouble.env")
	tokenFile := filepath.Join(base, "dashboard.token")
	body := "state_root = \"" + filepath.Join(base, "state") + "\"\n" +
		"[secrets]\nenvironment_file = \"" + envf + "\"\n" +
		"[dashboard]\ntoken_file = \"" + tokenFile + "\"\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	t.Setenv("TROUBLE_CONFIG_PATH", cfg)
	t.Setenv("TROUBLE_STATE_ROOT", filepath.Join(base, "state"))
	if code := run([]string{"dashboard", "token", "create", "--label", "e2e", "--scopes", "read", "--output-env", envf}); code != 0 {
		t.Fatalf("create exit = %d", code)
	}
	first := envToken(t, envf)
	if code := run([]string{"dashboard", "token", "rotate", "--label", "e2e", "--output-env", envf}); code != 0 {
		t.Fatalf("rotate exit = %d", code)
	}
	second := envToken(t, envf)
	if first == second {
		t.Fatal("rotate must replace the env token line")
	}
	if strings.Contains(second, first) && first != "" {
		t.Fatal("old plaintext must not survive in the env file")
	}
}

func envToken(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "TROUBLE_DASHBOARD_TOKEN=") {
			return strings.TrimPrefix(l, "TROUBLE_DASHBOARD_TOKEN=")
		}
	}
	t.Fatalf("no TROUBLE_DASHBOARD_TOKEN line in %s", path)
	return ""
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Mode().Perm()
}
