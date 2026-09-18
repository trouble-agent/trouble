package dashboard

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// defaultTokenRel is the SPEC-10 §3.4 default token_file path without its "~/"
// prefix — the shape an operator's config carries out of the box.
const defaultTokenRel = ".config/trouble/dashboard-tokens.json"

// TestExpandTokenPath pins the path rules of ExpandTokenPath: a leading "~/"
// and the bare "~" resolve against the current user's home (SPEC-10 §3.4's
// shipped default), and nothing else moves — absolute paths, relative paths and
// a "~" that is not at the front are returned byte-for-byte.
func TestExpandTokenPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cases := []struct{ in, want string }{
		{"~/.config/trouble/dashboard-tokens.json", filepath.Join(home, ".config/trouble/dashboard-tokens.json")},
		{"~/tokens.json", filepath.Join(home, "tokens.json")},
		{"~/", home},
		{"~", home},
		{"~/a/../b.json", filepath.Join(home, "b.json")},
		{"/etc/trouble/tokens.json", "/etc/trouble/tokens.json"},
		{"relative/tokens.json", "relative/tokens.json"},
		{"./~literal/tokens.json", "./~literal/tokens.json"},
		{"sub/~/tokens.json", "sub/~/tokens.json"},
		{"", ""},
	}
	for _, c := range cases {
		got, err := ExpandTokenPath(c.in)
		if err != nil {
			t.Errorf("ExpandTokenPath(%q): unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ExpandTokenPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestExpandTokenPathRefusesUnexpandableForms: a "~user" path is not a form this
// store supports, so it must be an error rather than a silently-useless literal
// directory name (the class of bug that made a missing store look like a valid
// empty one).
func TestExpandTokenPathRefusesUnexpandableForms(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, in := range []string{"~root/tokens.json", "~backup/.config/trouble/dashboard-tokens.json"} {
		if _, err := ExpandTokenPath(in); err == nil {
			t.Errorf("ExpandTokenPath(%q) = nil error, want a refusal", in)
		}
	}

	// No home to resolve against: refuse instead of returning the literal "~…".
	t.Setenv("HOME", "")
	if got, err := ExpandTokenPath("~/tokens.json"); err == nil {
		t.Errorf("ExpandTokenPath with an unknown home = (%q, nil), want an error", got)
	}
}

// TestLoadTokenStoreExpandsHomePath is the TRBL-006 regression: a store loaded
// with the SPEC-10 §3.4 default "~/…" path reads the file under the current home
// directory — and a store sitting under a literal "~" directory relative to the
// CWD stays unread (the pre-fix behavior that 401'd every minted token).
func TestLoadTokenStoreExpandsHomePath(t *testing.T) {
	home := t.TempDir()
	before := os.Getenv("HOME")
	// Cleanups are LIFO, so this runs after t.Setenv has restored HOME.
	t.Cleanup(func() {
		if got := os.Getenv("HOME"); got != before {
			t.Errorf("HOME not restored: %q, want %q", got, before)
		}
	})
	t.Setenv("HOME", home)

	plain, hash := tokenFor(0x11)
	wantPath := filepath.Join(home, ".config/trouble/dashboard-tokens.json")
	writeStoreAt(t, wantPath, 0o600, types.Token{
		ID: "dash-read@home", Hash: hash, Scopes: []types.Scope{types.ScopeRead},
	})

	// Decoy: what the literal path resolved to before the fix.
	work := t.TempDir()
	decoyPlain, decoyHash := tokenFor(0x22)
	writeStoreAt(t, filepath.Join(work, "~", ".config/trouble/dashboard-tokens.json"), 0o600, types.Token{
		ID: "dash-decoy@literal", Hash: decoyHash, Scopes: []types.Scope{types.ScopeAutonomy},
	})
	t.Chdir(work)

	store, err := LoadTokenStore("~/.config/trouble/dashboard-tokens.json", nil)
	if err != nil {
		t.Fatalf("LoadTokenStore: %v", err)
	}
	if store.path != wantPath {
		t.Fatalf("store path = %q, want %q", store.path, wantPath)
	}
	entry, ok := store.lookup(plain)
	if !ok || entry.ID != "dash-read@home" {
		t.Fatalf("$HOME token did not authenticate: ok=%v entry=%+v", ok, entry)
	}
	if _, ok := store.lookup(decoyPlain); ok {
		t.Fatal("a literal ~ directory authenticated: the path was not expanded")
	}
	if err := store.invalid(); err != nil {
		t.Fatalf("store unexpectedly invalid: %v", err)
	}
}

// TestLoadTokenStoreTildePathReloadsFromHome proves the per-request refresh
// (§3.2) stats and re-reads the EXPANDED path, not a literal "~" one: a rotation
// applied at $HOME/… is picked up without a restart.
func TestLoadTokenStoreTildePathReloadsFromHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".config/trouble/dashboard-tokens.json")

	oldPlain, oldHash := tokenFor(0x55)
	writeStoreAt(t, path, 0o600, types.Token{
		ID: "dash-read@old", Hash: oldHash, Scopes: []types.Scope{types.ScopeRead},
	})
	store, err := LoadTokenStore("~/"+".config/trouble/dashboard-tokens.json", nil)
	if err != nil {
		t.Fatalf("LoadTokenStore: %v", err)
	}
	if _, ok := store.lookup(oldPlain); !ok {
		t.Fatal("token in $HOME did not load")
	}

	// Rotate on disk (a longer label guarantees a size change, so the refresh
	// is never a same-mtime no-op).
	newPlain, newHash := tokenFor(0x66)
	writeStoreAt(t, path, 0o600, types.Token{
		ID: "dash-read@rotated-today", Hash: newHash, Scopes: []types.Scope{types.ScopeRead},
	})
	store.refresh(time.Now())

	if _, ok := store.lookup(newPlain); !ok {
		t.Fatal("rotated-in token not picked up: refresh is not reading the expanded path")
	}
	if _, ok := store.lookup(oldPlain); ok {
		t.Fatal("rotated-away token still authenticates")
	}
}

// TestLoadTokenStoreAbsolutePathUnchanged: an absolute path is passed through
// untouched — a foreign HOME must not re-root it.
func TestLoadTokenStoreAbsolutePathUnchanged(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	plain, hash := tokenFor(0x33)
	abs := writeTokenFile(t, t.TempDir(), types.Token{
		ID: "dash-read@abs", Hash: hash, Scopes: []types.Scope{types.ScopeRead},
	})
	if !filepath.IsAbs(abs) {
		t.Fatalf("fixture path %q is not absolute", abs)
	}

	store, err := LoadTokenStore(abs, nil)
	if err != nil {
		t.Fatalf("LoadTokenStore: %v", err)
	}
	if store.path != abs {
		t.Fatalf("store path = %q, want the unchanged absolute %q", store.path, abs)
	}
	if _, ok := store.lookup(plain); !ok {
		t.Fatal("token in the absolute store did not authenticate")
	}
}

// TestLoadTokenStoreRelativePathUnchanged: a non-tilde relative path keeps
// resolving against the process CWD, exactly as before.
func TestLoadTokenStoreRelativePathUnchanged(t *testing.T) {
	work := t.TempDir()
	t.Chdir(work)

	plain, hash := tokenFor(0x44)
	writeStoreAt(t, filepath.Join(work, "tokens.json"), 0o600, types.Token{
		ID: "dash-read@rel", Hash: hash, Scopes: []types.Scope{types.ScopeRead},
	})

	store, err := LoadTokenStore("tokens.json", nil)
	if err != nil {
		t.Fatalf("LoadTokenStore: %v", err)
	}
	if store.path != "tokens.json" {
		t.Fatalf("store path = %q, want the unchanged relative %q", store.path, "tokens.json")
	}
	if _, ok := store.lookup(plain); !ok {
		t.Fatal("token in the relative store did not authenticate")
	}
}

// TestLoadTokenStoreTildeModeViolationIsBootError: expanding the path must not
// bypass the §3.2 mode check (TROUBLE-LIFECYCLE-013).
func TestLoadTokenStoreTildeModeViolationIsBootError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".config/trouble/dashboard-tokens.json")
	writeStoreAt(t, path, 0o644, types.Token{ID: "dash-read@loose", Hash: tokenHashFor(0x77), Scopes: []types.Scope{types.ScopeRead}})

	store, err := LoadTokenStore("~/.config/trouble/dashboard-tokens.json", nil)
	if err == nil {
		t.Fatalf("world-readable /~ token file loaded (store %+v), want a boot error", store)
	}
	if store != nil {
		t.Fatal("store returned alongside the mode error")
	}
	var de *dashError
	if !errors.As(err, &de) {
		t.Fatalf("error %v is not a *dashError", err)
	}
	if de.Code != types.CodeLifecycle013 || de.Detail != "token_file_mode" {
		t.Fatalf("error = %s (detail %q), want %s / token_file_mode", de.Code, de.Detail, types.CodeLifecycle013)
	}
}

// TestLoadTokenStoreUnexpandablePathFailsLoud: with no resolvable home the "~/"
// path is a boot error (detail token_file_path), never a valid-but-empty store
// that 401s every bearer token.
func TestLoadTokenStoreUnexpandablePathFailsLoud(t *testing.T) {
	t.Setenv("HOME", "")

	store, err := LoadTokenStore("~/.config/trouble/dashboard-tokens.json", nil)
	if err == nil {
		t.Fatalf("unexpandable ~ path returned a store: %+v", store)
	}
	if store != nil {
		t.Fatal("store returned alongside the error")
	}
	var de *dashError
	if !errors.As(err, &de) {
		t.Fatalf("error %v is not a *dashError", err)
	}
	if de.Code != types.CodeLifecycle013 || de.Detail != "token_file_path" {
		t.Fatalf("error = %s (detail %q), want %s / token_file_path", de.Code, de.Detail, types.CodeLifecycle013)
	}

	// "~user/…" is refused on the same path, not read as a literal directory.
	if _, err := LoadTokenStore("~root/tokens.json", nil); err == nil {
		t.Fatal("~user path returned a store, want a boot error")
	}
}

// TestLoadTokenStoreEmptyPathKeepsExistingBehavior: an empty path is left alone
// (Config.normalize supplies the default before this call), so it keeps meaning
// the same thing it did before the change.
func TestLoadTokenStoreEmptyPathKeepsExistingBehavior(t *testing.T) {
	t.Chdir(t.TempDir())

	store, err := LoadTokenStore("", nil)
	if err != nil {
		t.Fatalf("LoadTokenStore(\"\"): %v", err)
	}
	if store.path != "" {
		t.Fatalf("store path = %q, want the unchanged empty path", store.path)
	}
	if err := store.invalid(); err != nil {
		t.Fatalf("empty path must remain a valid empty store, got %v", err)
	}
	if _, ok := store.lookup(tokenFor1()); ok {
		t.Fatal("empty store authenticated a token")
	}
}

// TestDashboardBearerTokenAuthenticatesFromTildeStore is the reported symptom
// end to end: with dashboard.token_file left in its SPEC-10 §3.4 default form
// ("~/.config/trouble/dashboard-tokens.json"), a token stored under the current
// user's home must authenticate over HTTP (200) instead of 401ing with
// TROUBLE-DASHBOARD-002.
func TestDashboardBearerTokenAuthenticatesFromTildeStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	env := newEnv(t, envOptions{noRefresh: true, cfg: func(c *Config) {
		c.TokenFile = "~/" + defaultTokenRel
	}})

	// newEnv wrote its fixture store into a temp dir; the deployed shape is the
	// same bytes under the home directory the token_file path names.
	b, err := os.ReadFile(env.tokenFile)
	if err != nil {
		t.Fatalf("read fixture store: %v", err)
	}
	deployed := filepath.Join(home, defaultTokenRel)
	if err := os.MkdirAll(filepath.Dir(deployed), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(deployed, b, 0o600); err != nil {
		t.Fatalf("write deployed store: %v", err)
	}
	if err := os.Chmod(deployed, 0o600); err != nil {
		t.Fatalf("chmod deployed store: %v", err)
	}
	if env.s.store.path != deployed {
		t.Fatalf("daemon store path = %q, want %q", env.s.store.path, deployed)
	}

	resp, body := env.get("/", env.readPlain)
	wantStatus(t, resp, body, http.StatusOK)

	// Negative control: a token that is not in that file still 401s.
	resp, body = env.get("/", tokenFor1())
	wantStatus(t, resp, body, http.StatusUnauthorized)
}

// writeStoreAt writes a §3.2 token file at an explicit path (writeTokenFile
// always lands in the directory it is handed, which cannot express a nested
// "~/.config/trouble/…" shape), pinning the mode so the 0600 checks are stable
// regardless of umask.
func writeStoreAt(t *testing.T, path string, mode os.FileMode, toks ...types.Token) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	b, err := json.Marshal(tokenFile{Version: 1, Tokens: toks})
	if err != nil {
		t.Fatalf("marshal token file: %v", err)
	}
	if err := os.WriteFile(path, b, mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}

// tokenHashFor is the at-rest half of tokenFor for cases that never present the
// plaintext.
func tokenHashFor(b byte) string {
	_, hash := tokenFor(b)
	return hash
}

// tokenFor1 is a stable 47-char plaintext for the "must not authenticate" probe.
func tokenFor1() string {
	plain, _ := tokenFor(0x99)
	return plain
}
