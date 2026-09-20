package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/dashboard"
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
