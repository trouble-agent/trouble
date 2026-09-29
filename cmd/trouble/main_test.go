package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/dashboard"
	"github.com/trouble-agent/trouble/internal/types"
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

// ---------- TRBL-063: the mint names its store, and warns on divergence ----------

// captureStderr runs fn with os.Stderr redirected to a pipe and returns what it
// wrote. The dashboard token verbs print their store notice on stderr, next to
// the success line they already print there; capturing the process's own stream
// is what proves the operator actually sees the line (rather than proving a
// helper returns a string nobody prints).
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	// cfgPathOverride is process-global state written by captureConfig; a test
	// that passes --config must not leak that path into the next one.
	t.Cleanup(func() { cfgPathOverride = "" })
	// os.Pipe is unbuffered, so fn's output must not exceed the pipe buffer
	// before we drain it: read in a goroutine, then wait.
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	func() {
		defer func() {
			os.Stderr = old
			_ = w.Close()
		}()
		fn()
	}()
	out := <-done
	_ = r.Close()
	return out
}

// unsetEnv removes an environment variable for the duration of the test. A
// plain t.Setenv(name, "") is NOT the same thing: an empty TROUBLE_* value is a
// present env entry that wins over the file layer, so it would mask the config
// declaration these tests are about.
func unsetEnv(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "unset") // registers the restore
	if err := os.Unsetenv(name); err != nil {
		t.Fatalf("unsetenv %s: %v", name, err)
	}
}

// writeConfig writes a 0600 config file declaring state_root under base (the
// state root must live under a real home; /tmp is refused) and returns its path.
func writeConfig(t *testing.T, base, dashboardBody string) string {
	t.Helper()
	cfg := filepath.Join(base, "config.toml")
	body := "state_root = \"" + filepath.Join(base, "state") + "\"\n" + dashboardBody
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	return cfg
}

// TestTokenCreateNamesStorePathWithDivergingConfig is TRBL-063's core case (a):
// a config declaring /data/state/dashboard.token — the two shipped container
// configs' value — must make the mint name BOTH paths and hand the operator the
// one-line remedy, because the CLI's resolved store is not the daemon's.
func TestTokenCreateNamesStorePathWithDivergingConfig(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	t.Setenv("HOME", home)
	unsetEnv(t, "XDG_CONFIG_HOME")
	cfg := writeConfig(t, base, "[dashboard]\ntoken_file = \"/data/state/dashboard.token\"\n")
	t.Setenv("TROUBLE_CONFIG_PATH", cfg)
	t.Setenv("TROUBLE_STATE_ROOT", filepath.Join(base, "state"))
	t.Setenv("TROUBLE_DASHBOARD_TOKEN_FILE", filepath.Join(base, "cli-store.json"))

	// The config's declaration must be the path the DAEMON would resolve, taken
	// from the real resolver — not a string this test made up.
	wantDeclared := configDeclaredStore(cfg)
	if wantDeclared != "/data/state/dashboard.token" {
		t.Fatalf("configDeclaredStore = %q, want the config's declaration", wantDeclared)
	}
	wantStore := filepath.Join(base, "cli-store.json")

	errOut := captureStderr(t, func() {
		if code := run([]string{"dashboard", "token", "create", "--label", "dogfood", "--scopes", "read"}); code != 0 {
			t.Errorf("create exit = %d, want 0", code)
		}
	})

	for _, want := range []string{
		"dashboard token store: " + wantStore,
		"WARNING",
		"/data/state/dashboard.token",
		wantStore,
		"TROUBLE_DASHBOARD_TOKEN_FILE=/data/state/dashboard.token",
		"--dashboard-token_file=/data/state/dashboard.token",
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("mint output %q does not carry %q", errOut, want)
		}
	}
	// The store the CLI actually wrote is the one it named.
	if _, err := os.Stat(wantStore); err != nil {
		t.Fatalf("the named store %s was not written: %v", wantStore, err)
	}
}

// TestTokenCreateNoConfigNamesDefaultAndDoesNotWarn is case (b): with no config
// file, the mint names the default store and prints NO warning — and the path it
// names is the path the daemon's own default resolves to, both taken from the
// real resolution functions.
func TestTokenCreateNoConfigNamesDefaultAndDoesNotWarn(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	t.Setenv("HOME", home)
	unsetEnv(t, "XDG_CONFIG_HOME")
	// A config path with no file behind it: the no-config case.
	cfg := filepath.Join(base, "absent", "config.toml")
	t.Setenv("TROUBLE_CONFIG_PATH", cfg)
	t.Setenv("TROUBLE_STATE_ROOT", filepath.Join(base, "state"))
	unsetEnv(t, "TROUBLE_DASHBOARD_TOKEN_FILE")

	if got := configDeclaredStore(cfg); got != "" {
		t.Fatalf("configDeclaredStore(absent) = %q, want \"\"", got)
	}

	// The CLI's own default resolver and the dashboard package's must agree:
	// this is the equality the whole defect is about.
	cliDefault, err := defaultTokenPath()
	if err != nil {
		t.Fatalf("defaultTokenPath: %v", err)
	}
	pkgDefault, err := dashboard.DefaultTokenPath()
	if err != nil {
		t.Fatalf("dashboard.DefaultTokenPath: %v", err)
	}
	wantDefault := filepath.Join(home, ".config/trouble/dashboard-tokens.json")
	if cliDefault != wantDefault {
		t.Fatalf("CLI default = %q, want %q", cliDefault, wantDefault)
	}
	if pkgDefault != wantDefault {
		t.Fatalf("dashboard default = %q, want %q", pkgDefault, wantDefault)
	}
	if cliDefault != pkgDefault {
		t.Fatalf("CLI default %q != daemon default %q", cliDefault, pkgDefault)
	}

	errOut := captureStderr(t, func() {
		if code := run([]string{"dashboard", "token", "create", "--label", "no-cfg", "--scopes", "read"}); code != 0 {
			t.Errorf("create exit = %d, want 0", code)
		}
	})
	if !strings.Contains(errOut, "dashboard token store: "+wantDefault) {
		t.Errorf("mint output %q does not name the default store %q", errOut, wantDefault)
	}
	if strings.Contains(errOut, "WARNING") {
		t.Errorf("no-config mint warned about a divergence: %q", errOut)
	}
	if _, err := os.Stat(wantDefault); err != nil {
		t.Fatalf("the named default store was not written: %v", err)
	}
}

// TestTokenStorePathMatchesDaemonResolution is the invariant the two cases above
// instantiate, asserted directly on the resolution functions: for a config that
// declares a token_file the CLI's store path IS that path (the sides can only
// diverge when a switch overrides one of them), and for no config both sides
// land on the same default.
func TestTokenStorePathMatchesDaemonResolution(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	t.Setenv("HOME", home)
	unsetEnv(t, "XDG_CONFIG_HOME")

	cases := []struct {
		name         string
		dashboard    string
		wantDeclared string
	}{
		{"declaring container store", "[dashboard]\ntoken_file = \"/data/state/dashboard.token\"\n", "/data/state/dashboard.token"},
		{"declaring a home-relative store", "[dashboard]\ntoken_file = \"~/.config/trouble/dashboard-tokens.json\"\n", filepath.Join(home, ".config/trouble/dashboard-tokens.json")},
		{"no token_file key", "[dashboard]\nread_only = true\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("HOME", home)
			cfg := writeConfig(t, dir, c.dashboard)
			t.Setenv("TROUBLE_CONFIG_PATH", cfg)
			t.Setenv("TROUBLE_STATE_ROOT", filepath.Join(dir, "state"))
			unsetEnv(t, "TROUBLE_DASHBOARD_TOKEN_FILE")

			got := configDeclaredStore(cfg)
			if got != c.wantDeclared {
				t.Fatalf("configDeclaredStore = %q, want %q", got, c.wantDeclared)
			}
			// The daemon resolves the same file: with no override the resolved
			// config's store, expanded, is what the daemon's store would read.
			res, code := resolve(nil)
			if code != 0 {
				t.Fatalf("resolve exit = %d", code)
			}
			daemonStore := res.Config.Dashboard.TokenFile
			if daemonStore == "" {
				var err error
				daemonStore, err = dashboard.DefaultTokenPath()
				if err != nil {
					t.Fatalf("dashboard.DefaultTokenPath: %v", err)
				}
			}
			daemonStore, err := dashboard.ExpandTokenPath(daemonStore)
			if err != nil {
				t.Fatalf("ExpandTokenPath: %v", err)
			}
			if c.wantDeclared != "" && daemonStore != c.wantDeclared {
				t.Fatalf("daemon store %q != the config's declared %q", daemonStore, c.wantDeclared)
			}
			if c.wantDeclared == "" {
				cliDefault, err := defaultTokenPath()
				if err != nil {
					t.Fatalf("defaultTokenPath: %v", err)
				}
				if daemonStore != cliDefault {
					t.Fatalf("no-config: daemon store %q != CLI default %q", daemonStore, cliDefault)
				}
			}
		})
	}
}

// TestTokenListMirrorsDivergenceWarning: the read side owes the same feedback —
// `token list` against a diverging config names its store and warns, so an
// operator staring at a list that is not the daemon's store can tell why the
// token they see 401s.
func TestTokenListMirrorsDivergenceWarning(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	t.Setenv("HOME", home)
	unsetEnv(t, "XDG_CONFIG_HOME")
	cfg := writeConfig(t, base, "[dashboard]\ntoken_file = \"/data/state/dashboard.token\"\n")
	t.Setenv("TROUBLE_CONFIG_PATH", cfg)
	t.Setenv("TROUBLE_STATE_ROOT", filepath.Join(base, "state"))
	cliStore := filepath.Join(base, "cli-store.json")
	t.Setenv("TROUBLE_DASHBOARD_TOKEN_FILE", cliStore)

	// Seed the store the CLI will read, via a mint.
	if code := run([]string{"dashboard", "token", "create", "--label", "listed", "--scopes", "read"}); code != 0 {
		t.Fatalf("seed create exit = %d", code)
	}

	errOut := captureStderr(t, func() {
		if code := run([]string{"dashboard", "token", "list"}); code != 0 {
			t.Errorf("list exit = %d, want 0", code)
		}
	})
	for _, want := range []string{
		"dashboard token store: " + cliStore,
		"WARNING",
		"/data/state/dashboard.token",
		"TROUBLE_DASHBOARD_TOKEN_FILE=/data/state/dashboard.token",
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("list output %q does not carry %q", errOut, want)
		}
	}
}

// TestTokenCreateDefaultIgnoresXDGConfigHome pins the second route to the same
// defect, measured live before the fix: the daemon's default store is the `~/`
// form under $HOME (Config.normalize's documented default), while the CLI used
// os.UserConfigDir(), which honours XDG_CONFIG_HOME. With XDG_CONFIG_HOME set,
// the CLI minted into $XDG_CONFIG_HOME/trouble/… and the daemon read
// $HOME/.config/trouble/… — a minted token that 401s with no feedback, which is
// exactly TRBL-063's symptom. Both sides must resolve the default to $HOME.
func TestTokenCreateDefaultIgnoresXDGConfigHome(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	xdg := filepath.Join(home, "xdg")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("TROUBLE_CONFIG_PATH", filepath.Join(base, "absent", "config.toml"))
	t.Setenv("TROUBLE_STATE_ROOT", filepath.Join(base, "state"))
	unsetEnv(t, "TROUBLE_DASHBOARD_TOKEN_FILE")

	want := filepath.Join(home, ".config/trouble/dashboard-tokens.json")
	got, err := defaultTokenPath()
	if err != nil {
		t.Fatalf("defaultTokenPath: %v", err)
	}
	if got != want {
		t.Fatalf("default with XDG_CONFIG_HOME set = %q, want %q (the daemon's)", got, want)
	}
	daemon, err := dashboard.DefaultTokenPath()
	if err != nil {
		t.Fatalf("dashboard.DefaultTokenPath: %v", err)
	}
	if daemon != want {
		t.Fatalf("dashboard default = %q, want %q", daemon, want)
	}
	if stale := filepath.Join(xdg, "trouble", "dashboard-tokens.json"); got == stale {
		t.Fatalf("CLI default still follows XDG_CONFIG_HOME (%q)", stale)
	}

	errOut := captureStderr(t, func() {
		if code := run([]string{"dashboard", "token", "create", "--label", "xdg", "--scopes", "read"}); code != 0 {
			t.Errorf("create exit = %d, want 0", code)
		}
	})
	if !strings.Contains(errOut, "dashboard token store: "+want) {
		t.Errorf("mint output %q does not name %q", errOut, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("mint did not write the daemon's store %s: %v", want, err)
	}
}

// ---------- TRBL-081: the token verbs can address a non-default store ----------
//
// The row: docs/cmd.md advertises `troubled --dashboard-token_file <path>` as a
// scratch-daemon store, and the token CLI had no counterpart — create/list/
// rotate/revoke always wrote the compiled default, so a mint against an
// argv-booted daemon printed a plaintext that 401'd everywhere and nothing said
// the two sides were on different files. These tests pin the counterpart: the
// flag (in both spellings) on all four verbs, the precedence, and the one rule
// the value must satisfy.

// runCaptured runs the CLI with stdout and stderr captured, so a test can assert on
// the plaintext a mint prints AND on the store notice printed beside it.
func runCaptured(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	// cfgPathOverride is process-global state written by captureConfig; a test
	// that passes --config must not leak that path into the next one.
	t.Cleanup(func() { cfgPathOverride = "" })
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	savedOut, savedErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = wOut, wErr
	outCh := make(chan string, 1)
	errCh := make(chan string, 1)
	go func() { b, _ := io.ReadAll(rOut); outCh <- string(b) }()
	go func() { b, _ := io.ReadAll(rErr); errCh <- string(b) }()
	func() {
		defer func() {
			os.Stdout, os.Stderr = savedOut, savedErr
			_ = wOut.Close()
			_ = wErr.Close()
		}()
		code = run(args)
	}()
	stdout, stderr = <-outCh, <-errCh
	_ = rOut.Close()
	_ = rErr.Close()
	return stdout, stderr, code
}

// atRestHash is sha256(s)[:32] hex — the store's own token form (SPEC-10 §3.2),
// recomputed here so a test can prove the plaintext a mint printed is the entry
// that landed in the store it named.
func atRestHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:32])
}

// readTokenFile parses a store the way `token list` does.
func readTokenFile(t *testing.T, path string) tokenFileView {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var tf tokenFileView
	if err := json.Unmarshal(b, &tf); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return tf
}

// tokenStoreFixture is the scratch-daemon shape the row is about: a config that
// declares NO dashboard.token_file (so the compiled default would win), an
// isolated HOME so that default is a temp path, and no TROUBLE_* store override.
// It returns the base dir and the default store the flag has to divert from.
func tokenStoreFixture(t *testing.T) (base, defStore string) {
	t.Helper()
	base = t.TempDir()
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	t.Setenv("HOME", home)
	unsetEnv(t, "XDG_CONFIG_HOME")
	unsetEnv(t, "TROUBLE_DASHBOARD_TOKEN_FILE")
	cfg := writeConfig(t, base, "")
	t.Setenv("TROUBLE_CONFIG_PATH", cfg)
	t.Setenv("TROUBLE_STATE_ROOT", filepath.Join(base, "state"))
	return base, filepath.Join(home, ".config/trouble/dashboard-tokens.json")
}

// TestTokenStoreFlagSpellings: `--token-file <path>` and the daemon's own
// `--dashboard-token_file <path>` are one flag, and the mint lands in the file
// the flag named — hashed, 0600 — while the compiled default stays untouched.
// Touching BOTH is the silent divergence: the operator would find a token in a
// file the daemon never reads.
func TestTokenStoreFlagSpellings(t *testing.T) {
	for _, spelling := range []string{flagTokenFile, flagDaemonTokenFile} {
		t.Run(spelling, func(t *testing.T) {
			base, defStore := tokenStoreFixture(t)
			alt := filepath.Join(base, "daemon-store.json")

			stdout, stderr, code := runCaptured(t, "dashboard", "token", "create",
				"--"+spelling, alt, "--label", "scratch", "--scopes", "read")
			if code != 0 {
				t.Fatalf("create --%s exit = %d, stderr %q", spelling, code, stderr)
			}
			plaintext := strings.TrimSpace(stdout)
			if !strings.HasPrefix(plaintext, "tdt_") || len(plaintext) != 47 {
				t.Fatalf("plaintext = %q, want a 47-char tdt_ token", plaintext)
			}
			for _, want := range []string{
				"dashboard token store: " + alt,
				"(from --" + flagTokenFile + ")",
			} {
				if !strings.Contains(stderr, want) {
					t.Errorf("mint output %q does not carry %q", stderr, want)
				}
			}
			if got := fileMode(t, alt); got != 0o600 {
				t.Errorf("the flag-addressed store mode = %04o, want 0600", got)
			}
			b, err := os.ReadFile(alt)
			if err != nil {
				t.Fatalf("the flag-addressed store %s was not written: %v", alt, err)
			}
			if strings.Contains(string(b), plaintext) {
				t.Error("the flag-addressed store leaked the plaintext")
			}
			if !strings.Contains(string(b), atRestHash(plaintext)) {
				t.Error("the flag-addressed store does not hold the hash of the plaintext it printed")
			}
			if _, err := os.Stat(defStore); !os.IsNotExist(err) {
				t.Fatalf("the compiled default %s exists (stat err %v); the mint did not stay on the flag store", defStore, err)
			}
		})
	}
}

// TestTokenStoreFlagAllFourVerbs: create, list, rotate and revoke all address the
// store the flag names — the acceptance criterion's "same for list/rotate/revoke".
func TestTokenStoreFlagAllFourVerbs(t *testing.T) {
	base, defStore := tokenStoreFixture(t)
	alt := filepath.Join(base, "daemon-store.json")

	for _, label := range []string{"keep", "gone"} {
		if _, stderr, code := runCaptured(t, "dashboard", "token", "create",
			"--token-file", alt, "--label", label, "--scopes", "read,write"); code != 0 {
			t.Fatalf("create --label %s exit = %d, stderr %q", label, code, stderr)
		}
	}

	// list --token-file reads the addressed store.
	stdout, stderr, code := runCaptured(t, "dashboard", "token", "list", "--token-file", alt, "--json")
	if code != 0 {
		t.Fatalf("list exit = %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stderr, "dashboard token store: "+alt) {
		t.Errorf("list output %q does not name the flag store", stderr)
	}
	for _, want := range []string{"keep", "gone"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("list output %q is missing %q", stdout, want)
		}
	}

	// rotate --token-file rotates IN the addressed store: the fresh plaintext is
	// printed and its hash — and only revoked keepers of the old entry — are in
	// that same file.
	out, stderr, code := runCaptured(t, "dashboard", "token", "rotate", "--token-file", alt, "--label", "keep")
	if code != 0 {
		t.Fatalf("rotate exit = %d, stderr %q", code, stderr)
	}
	fresh := strings.TrimSpace(out)
	if !strings.HasPrefix(fresh, "tdt_") || len(fresh) != 47 {
		t.Fatalf("rotate plaintext = %q, want a 47-char tdt_ token", fresh)
	}
	tf := readTokenFile(t, alt)
	var freshInStore, revokedKeeps int
	for _, tok := range tf.Tokens {
		if tok.Hash == atRestHash(fresh) && !tok.Revoked {
			freshInStore++
		}
		if strings.HasPrefix(tok.ID, "keep") && tok.Revoked {
			revokedKeeps++
		}
	}
	if freshInStore != 1 {
		t.Errorf("the rotated plaintext is in the flag store %d times, want 1", freshInStore)
	}
	if revokedKeeps != 1 {
		t.Errorf("revoked `keep` entries in the flag store = %d, want 1 (no grace window)", revokedKeeps)
	}

	// revoke --token-file revokes in the addressed store.
	if _, stderr, code := runCaptured(t, "dashboard", "token", "revoke", "--token-file", alt, "--label", "gone"); code != 0 {
		t.Fatalf("revoke exit = %d, stderr %q", code, stderr)
	}
	tf = readTokenFile(t, alt)
	revokedGone := 0
	for _, tok := range tf.Tokens {
		if strings.HasPrefix(tok.ID, "gone") && tok.Revoked {
			revokedGone++
		}
	}
	if revokedGone != 1 {
		t.Errorf("revoked `gone` entries in the flag store = %d, want 1", revokedGone)
	}

	// The control: none of the four verbs touched the compiled default, and a
	// verb WITHOUT the flag cannot read the store at all — which is what makes
	// the flag the thing that addresses it.
	if _, err := os.Stat(defStore); !os.IsNotExist(err) {
		t.Fatalf("the compiled default %s exists (stat err %v); a verb left the flag store", defStore, err)
	}
	if _, stderr, code := runCaptured(t, "dashboard", "token", "list"); code != 13 {
		t.Errorf("list without the flag = %d (stderr %q), want 13: the default store was never written", code, stderr)
	}
}

// TestTokenStoreFlagRefusesEmptyValue closes the last silent fallback in the
// flag's own resolution: `--token-file "$STORE"` with STORE unset is how an
// operator most plausibly ends up minting into a store they did not name, and
// falling back to the config/default without a word is the very shape of
// TRBL-081. An empty value that was GIVEN is refused; an absent flag still
// resolves the config as before.
func TestTokenStoreFlagRefusesEmptyValue(t *testing.T) {
	_, defStore := tokenStoreFixture(t)

	_, stderr, code := runCaptured(t, "dashboard", "token", "create",
		"--token-file", "", "--label", "empty", "--scopes", "read")
	if code != 13 {
		t.Fatalf("create --token-file <empty> = %d (stderr %q), want 13", code, stderr)
	}
	if !strings.Contains(stderr, "--"+flagTokenFile) || !strings.Contains(stderr, "empty value") {
		t.Errorf("refusal %q does not name the flag and the empty value", stderr)
	}
	if _, err := os.Stat(defStore); !os.IsNotExist(err) {
		t.Errorf("the compiled default %s exists after a refused mint", defStore)
	}
	// The `=` spelling sets the same flag, and is caught by the same check.
	if _, _, code := runCaptured(t, "dashboard", "token", "create",
		"--token-file=", "--label", "empty", "--scopes", "read"); code != 13 {
		t.Errorf("create --token-file= = %d, want 13", code)
	}
}

// TestTokenStoreFlagBeatsEnvAndConfigFile pins the precedence: the explicit flag
// wins the WHOLE chain (env > file > default), so an operator who passes the
// daemon's own store argument cannot be silently outvoted by their config.
func TestTokenStoreFlagBeatsEnvAndConfigFile(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	t.Setenv("HOME", home)
	unsetEnv(t, "XDG_CONFIG_HOME")

	declared := filepath.Join(base, "config-store.json")
	cfg := writeConfig(t, base, "[dashboard]\ntoken_file = \""+declared+"\"\n")
	t.Setenv("TROUBLE_CONFIG_PATH", cfg)
	envStore := filepath.Join(base, "env-store.json")
	t.Setenv("TROUBLE_DASHBOARD_TOKEN_FILE", envStore)
	flagStore := filepath.Join(base, "flag-store.json")

	_, stderr, code := runCaptured(t, "dashboard", "token", "create",
		"--token-file", flagStore, "--label", "prec", "--scopes", "read")
	if code != 0 {
		t.Fatalf("create exit = %d, stderr %q", code, stderr)
	}
	if _, err := os.Stat(flagStore); err != nil {
		t.Fatalf("the flag store was not written: %v", err)
	}
	for _, other := range []struct{ name, path string }{
		{"the config's declared store", declared},
		{"the env store", envStore},
	} {
		if _, err := os.Stat(other.path); !os.IsNotExist(err) {
			t.Errorf("%s (%s) exists (stat err %v); the flag must win the chain", other.name, other.path, err)
		}
	}
	// The divergence warning still fires — the config declares a different store
	// — and it names the config's path, not the flag's.
	if !strings.Contains(stderr, "WARNING") || !strings.Contains(stderr, declared) {
		t.Errorf("mint output %q does not warn about the config's %q", stderr, declared)
	}
	// But with the store named on the command line the notice must NOT claim the
	// token will fail: argv outranks the file for the daemon too (SPEC-12 §3.1),
	// so the two sides agree whenever the daemon was booted with the same value.
	// It states the condition, and tells the operator how to agree.
	if strings.Contains(stderr, "will not authenticate") {
		t.Errorf("the flag-case warning guesses the token will 401: %q", stderr)
	}
	if !strings.Contains(stderr, "--dashboard-token_file="+flagStore) {
		t.Errorf("the flag-case warning does not say how to agree: %q", stderr)
	}
}

// TestTokenStoreFlagExpandsTilde: the flag value goes through the same expansion
// as the config's own path, so `~/…` cannot land the CLI on a second file
// (TROUBLE-DASHBOARD-002's route).
func TestTokenStoreFlagExpandsTilde(t *testing.T) {
	base, _ := tokenStoreFixture(t)
	home := filepath.Join(base, "home")

	_, stderr, code := runCaptured(t, "dashboard", "token", "create",
		"--token-file", "~/flag-store.json", "--label", "tilde", "--scopes", "read")
	if code != 0 {
		t.Fatalf("create exit = %d, stderr %q", code, stderr)
	}
	want := filepath.Join(home, "flag-store.json")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("`~/flag-store.json` did not expand to %s: %v", want, err)
	}
	if !strings.Contains(stderr, "dashboard token store: "+want) {
		t.Errorf("mint output %q does not name the expanded path %q", stderr, want)
	}
}

// TestTokenStoreFlagRefusesNonPathValue: the value carries the daemon's own argv
// rule — by calling that rule's scanner, not a copy of it. A value the daemon
// could never be booted with (a bare name, a relative word, a token) would put
// the two sides on different files again, silently, so the CLI refuses it before
// it writes anything, and does not echo the value in the refusal.
func TestTokenStoreFlagRefusesNonPathValue(t *testing.T) {
	base, defStore := tokenStoreFixture(t)

	// The positive control: an explicit path is accepted, so the refusals below
	// are about the value's shape and not about the flag existing.
	good := filepath.Join(base, "good-store.json")
	if _, stderr, code := runCaptured(t, "dashboard", "token", "create",
		"--token-file", good, "--label", "ok", "--scopes", "read"); code != 0 {
		t.Fatalf("create --token-file <absolute path> = %d, stderr %q, want 0", code, stderr)
	}

	for _, bad := range []string{
		"scratch-store.json",           // a bare name: no path shape
		"relative/nested/store.json",   // a relative word: not one of the four prefixes
		"tdt_abcDEF123ghiJKL456mnoPQR", // a token: the shape the rule exists to catch
	} {
		t.Run(bad, func(t *testing.T) {
			_, stderr, code := runCaptured(t, "dashboard", "token", "create",
				"--token-file", bad, "--label", "x", "--scopes", "read")
			if code != 13 {
				t.Fatalf("create --token-file %q = %d, want 13 (the daemon's argv rule refuses it)", bad, code)
			}
			for _, want := range []string{string(types.CodeLifecycle013), "cli_flag_secret"} {
				if !strings.Contains(stderr, want) {
					t.Errorf("refusal %q does not name %q", stderr, want)
				}
			}
			if strings.Contains(stderr, bad) {
				t.Errorf("the refusal echoed the value it refused: %q", stderr)
			}
		})
	}
	// Nothing was minted: the refusal precedes the write.
	if _, err := os.Stat(defStore); !os.IsNotExist(err) {
		t.Errorf("the compiled default %s exists after a refused mint", defStore)
	}
}

// TestHelpNamesTheTokenStoreFlag: the flag has to be discoverable from the CLI
// itself — it is the remedy the row asks for, and a flag nobody can find leaves
// the divergence intact. The per-verb usage errors name it too, so a caller who
// forgot --label still learns the store can be addressed.
func TestHelpNamesTheTokenStoreFlag(t *testing.T) {
	tokenStoreFixture(t) // isolated HOME: no verb here may touch a real store
	stdout, _, code := runCaptured(t, "--help")
	if code != 0 {
		t.Fatalf("--help exit = %d, want 0", code)
	}
	for _, want := range []string{"--token-file PATH", "--dashboard-token_file"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("--help does not name %q:\n%s", want, stdout)
		}
	}
	for _, args := range [][]string{
		{"dashboard"},
		{"dashboard", "token", "create"},
		{"dashboard", "token", "rotate"},
		{"dashboard", "token", "revoke"},
	} {
		_, stderr, code := runCaptured(t, args...)
		if code != 2 {
			t.Errorf("run(%v) = %d, want 2", args, code)
		}
		if !strings.Contains(stderr, "--token-file PATH") {
			t.Errorf("run(%v) usage %q does not name --token-file PATH", args, stderr)
		}
	}
}
