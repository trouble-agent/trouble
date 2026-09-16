package lifecycle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

func TestResolvePrecedence(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	content := `
state_root = "/from/file"
ingest.bind = "127.0.0.1:7000"
hub.token = "sk_live_fixture_0001"
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{
		"TROUBLE_STATE_ROOT=/from/env",
		"TROUBLE_HUB_TOKEN=env_token",
	}
	args := []string{"--ingest-bind", "127.0.0.1:8000"}

	r, err := Resolve(args, env, cfgPath)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	want := map[string]struct{ value, source, ref string }{
		"state_root":     {"/from/env", "env", "TROUBLE_STATE_ROOT"},
		"ingest.bind":    {"127.0.0.1:8000", "flag", "--ingest-bind"},
		"hub.token":      {"[REDACTED:config]", "env", "TROUBLE_HUB_TOKEN"},
		"dashboard.bind": {"127.0.0.1:7644", "default", "builtin"},
	}
	got := make(map[string]types.ConfigValue)
	for _, cv := range r.Values {
		got[cv.Key] = cv
	}
	for k, w := range want {
		cv, ok := got[k]
		if !ok {
			t.Fatalf("missing key %s", k)
		}
		if cv.Source != w.source {
			t.Errorf("%s source: got %q, want %q", k, cv.Source, w.source)
		}
		if s, _ := cv.Value.(string); s != w.value {
			t.Errorf("%s value: got %q, want %q", k, s, w.value)
		}
	}

	// Dump must not contain the secret fixture string.
	b, err := explainJSON(r.Values)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "sk_live_fixture_0001") || strings.Contains(string(b), "env_token") {
		t.Errorf("marshalled dump contains secret token")
	}
}

func TestResolveUnknownFileKey(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("not_a_key = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Resolve(nil, nil, cfgPath)
	if err == nil {
		t.Fatal("expected error for unknown file key")
	}
	if !strings.Contains(err.Error(), "TROUBLE-LIFECYCLE-001") {
		t.Errorf("expected 001 code, got %v", err)
	}
}

func TestResolveUnknownEnvHint(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("state_root = \"/x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{"TROUBLE_STATE_ROOOT=/typo"} // edit distance 2 from state_root
	r, err := Resolve(nil, env, cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.UnknownEnv) != 1 {
		t.Fatalf("expected 1 unknown env, got %d", len(r.UnknownEnv))
	}
	m, ok := r.UnknownEnv[0].Value.(map[string]any)
	if !ok {
		t.Fatalf("unknown env value type %T", r.UnknownEnv[0].Value)
	}
	if m["hint"] != "state_root" {
		t.Errorf("hint: got %q, want state_root", m["hint"])
	}
}

func TestResolveConflict(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("state_root = \"/file\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{"TROUBLE_STATE_ROOT=/env"}
	r, err := Resolve(nil, env, cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Conflicts) == 0 {
		t.Fatal("expected conflict record")
	}
}
