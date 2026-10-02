package lifecycle

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TRBL-085: dashboard.token_file default was HOME-anchored, not
// state_root-anchored. A scratch config that declared state_root (and/or
// config_path) but left dashboard.token_file unset resolved BOTH
// `trouble dashboard token create` and the daemon's token store to the
// operator's real ~/.config/trouble/dashboard-tokens.json. A config with its
// own state root must never read or mint tokens in the operator's home store.
// ---------------------------------------------------------------------------
// These tests sit at the composition root (Resolve) because that is where both
// consumers resolve: the daemon reads res.Config.Dashboard.TokenFile, and the
// operator CLI resolves the same way (lifecycle.Resolve + the same default
// expression). One fix here covers both sides by construction.

// trbl085Home pins HOME for one test and returns it (same fixture rule as
// trbl086Home — os.UserHomeDir reads $HOME on this platform).
func trbl085Home(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "")
	return home
}

// TestResolveTokenFileAnchoredToDeclaredStateRoot is the TRBL-085 acceptance:
// a config that declares state_root but leaves dashboard.token_file unset
// resolves the token store under the DECLARED state root, never under $HOME.
func TestResolveTokenFileAnchoredToDeclaredStateRoot(t *testing.T) {
	home := trbl085Home(t)
	root := t.TempDir()

	res, err := Resolve(nil, nil, writeConfig(t, `state_root = "`+root+`"
`))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	want := filepath.Join(root, "dashboard-tokens.json")
	if got := res.Config.Dashboard.TokenFile; got != want {
		t.Fatalf("Config.Dashboard.TokenFile = %q, want %q (anchored to the declared state_root, never %s/.config/trouble)", got, want, home)
	}

	// The explain row reports the path the daemon will actually use.
	row, ok := resolvedRows(res)["dashboard.token_file"]
	if !ok {
		t.Fatal("no resolved row for dashboard.token_file")
	}
	if got := fmt.Sprint(row.Value); got != want {
		t.Errorf("dashboard.token_file row value = %v, want %q", got, want)
	}
}

// TestResolveTokenFileExplicitWinsOverStateRootAnchoring pins AC (a): an
// explicitly declared dashboard.token_file always wins over any state-root
// anchoring — an operator who deliberately points the store elsewhere keeps
// that path.
func TestResolveTokenFileExplicitWinsOverStateRootAnchoring(t *testing.T) {
	trbl085Home(t)
	root := t.TempDir()
	explicit := filepath.Join(t.TempDir(), "operator-store.json")

	res, err := Resolve(nil, nil, writeConfig(t, `state_root = "`+root+`"
dashboard.token_file = "`+explicit+`"
`))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := res.Config.Dashboard.TokenFile; got != explicit {
		t.Fatalf("Config.Dashboard.TokenFile = %q, want the explicit %q", got, explicit)
	}
}

// TestResolveTokenFileEnvAndFlagStateRootAnchorToo: state_root can arrive from
// a source other than the file (TROUBLE_STATE_ROOT, --state-root on argv) and
// the anchoring follows whichever source declared it — the scratch daemon that
// surfaced TRBL-085 passed --config with state roots set by env and flag.
func TestResolveTokenFileEnvAndFlagStateRootAnchorToo(t *testing.T) {
	trbl085Home(t)
	root := t.TempDir()
	want := filepath.Join(root, "dashboard-tokens.json")

	envRes, err := Resolve(nil, []string{"TROUBLE_STATE_ROOT=" + root}, "")
	if err != nil {
		t.Fatalf("Resolve(env): %v", err)
	}
	if got := envRes.Config.Dashboard.TokenFile; got != want {
		t.Errorf("env state_root: TokenFile = %q, want %q", got, want)
	}

	flagRes, err := Resolve([]string{"--state_root", root}, nil, "")
	if err != nil {
		t.Fatalf("Resolve(flag): %v", err)
	}
	if got := flagRes.Config.Dashboard.TokenFile; got != want {
		t.Errorf("flag state_root: TokenFile = %q, want %q", got, want)
	}
}

// TestResolveTokenFileNoDeclaredStateRootKeepsHomeDefault pins AC (c): with no
// declared state_root (the default install — no config file, no env, no flag)
// nothing is anchored, so the value stays the empty marker and the §3.4 HOME
// default is applied downstream by the consumers (the daemon's dashboard
// config projection and the CLI's store resolution), exactly as before.
func TestResolveTokenFileNoDeclaredStateRootKeepsHomeDefault(t *testing.T) {
	home := trbl085Home(t)

	res, err := Resolve(nil, nil, "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := filepath.Join(home, ".config", "trouble", "dashboard-tokens.json")
	if got := res.Config.Dashboard.TokenFile; got != "" {
		t.Fatalf("Config.Dashboard.TokenFile = %q, want the empty marker (consumers apply the §3.4 default %q)", got, want)
	}
	row, ok := resolvedRows(res)["dashboard.token_file"]
	if !ok {
		t.Fatal("no resolved row for dashboard.token_file")
	}
	if got := fmt.Sprint(row.Value); got != "" {
		t.Errorf("dashboard.token_file row value = %v, want empty (no anchoring)", got)
	}
}

// TestResolveTokenFileDefaultStateRootValueDoesNotAnchor: only a DECLARED
// state_root anchors. A config file that is present but sets nothing still
// leaves the store empty (consumers apply the §3.4 HOME default downstream) —
// the anchoring keys off the winning source of state_root, never off the
// compiled default's value.
func TestResolveTokenFileDefaultStateRootValueDoesNotAnchor(t *testing.T) {
	trbl085Home(t)
	cfg := writeConfig(t, `dashboard.read_only = true
`)

	res, err := Resolve(nil, nil, cfg)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	home := res.Config.StateRoot // compiled default (XDG/home-derived), not declared
	if got := res.Config.Dashboard.TokenFile; got != "" {
		t.Fatalf("TokenFile = %q, want empty — the UNDECLARED default state root (%s) must not anchor", got, home)
	}
}
