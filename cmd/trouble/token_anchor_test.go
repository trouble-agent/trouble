package main

// TRBL-085 end-to-end: a scratch config that declares state_root but leaves
// dashboard.token_file unset must make `trouble dashboard token create` mint
// into <state_root>/dashboard-tokens.json — never the operator's real
// ~/.config/trouble/dashboard-tokens.json. The CLI resolves the store through
// lifecycle.Resolve (the same fix the daemon takes), so this test exercises the
// whole path: config file → resolve → dashboardStorePath → LoadTokenStore.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTokenCreateAnchorsToDeclaredStateRoot(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	t.Setenv("HOME", home)
	unsetEnv(t, "XDG_CONFIG_HOME")
	unsetEnv(t, "TROUBLE_DASHBOARD_TOKEN_FILE")
	unsetEnv(t, "TROUBLE_STATE_ROOT")

	root := filepath.Join(base, "state")
	cfg := writeConfig(t, base, "[dashboard]\nread_only = true\n")
	t.Setenv("TROUBLE_CONFIG_PATH", cfg)

	errOut := captureStderr(t, func() {
		if code := run([]string{"dashboard", "token", "create", "--label", "trbl085", "--scopes", "read"}); code != 0 {
			t.Errorf("create exit = %d, want 0", code)
		}
	})

	want := filepath.Join(root, "dashboard-tokens.json")
	if !strings.Contains(errOut, "dashboard token store: "+want) {
		t.Errorf("mint output %q does not name the anchored store %q", errOut, want)
	}
	if strings.Contains(errOut, "WARNING") {
		t.Errorf("anchored mint warned about a divergence: %q", errOut)
	}
	b, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("the anchored store was not written: %v", err)
	}
	var tf struct {
		Tokens []struct {
			ID string `json:"id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(b, &tf); err != nil {
		t.Fatalf("parse store: %v", err)
	}
	if len(tf.Tokens) != 1 || tf.Tokens[0].ID != "trbl085" {
		t.Fatalf("store tokens = %+v, want exactly the minted token", tf.Tokens)
	}

	// Negative control: the operator's real home store was never touched.
	homeStore := filepath.Join(home, ".config", "trouble", "dashboard-tokens.json")
	if _, err := os.Stat(homeStore); !os.IsNotExist(err) {
		t.Fatalf("home store exists (err=%v): the mint leaked into the operator's real store", err)
	}
}
