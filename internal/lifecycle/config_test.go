package lifecycle

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
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

// ---------------------------------------------------------------------------
// TRBL-086: a leading `~` in a string value is the operator's home directory.
// ---------------------------------------------------------------------------

// trbl086Home pins HOME for one test and returns it. os.UserHomeDir reads $HOME
// on this platform, so t.Setenv IS the fixture — and it registers the restore of
// the caller's own environment itself.
func trbl086Home(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

// TestExpandHomeForms pins the exactly-two-forms rule expandHome implements: "~"
// and "~/…" resolve against the operator's own home directory, and every other
// string is returned byte-for-byte — an absolute path, a relative path, a "~"
// that is not at the front, and the "~user" form, which names ANOTHER user's home
// and which this layer cannot tell apart from a plain string that merely starts
// with a tilde.
func TestExpandHomeForms(t *testing.T) {
	home := trbl086Home(t)
	cases := []struct{ in, want string }{
		{"~", filepath.Clean(home)},
		{"~/test-trbl086", filepath.Join(home, "test-trbl086")},
		{"~/.config/trouble/config.toml", filepath.Join(home, ".config", "trouble", "config.toml")},
		{"~/a/../b", filepath.Join(home, "b")},
		{"/var/lib/trouble", "/var/lib/trouble"},
		{"relative/x", "relative/x"},
		{"./~/literal", "./~/literal"},
		{"sub/~/x", "sub/~/x"},
		{"~root/x", "~root/x"},
		{"", ""},
		{"x~y", "x~y"},
	}
	for _, c := range cases {
		got, err := expandHome(c.in)
		if err != nil {
			t.Errorf("expandHome(%q): unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("expandHome(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestExpandHomeUnknownHomeRefused: the one tilde this layer CAN classify but
// cannot resolve must be an error, never the literal string. A literal "~/…" is
// a path that exists only relative to the cwd, so a fall-back hands the operator
// a path-shaped name for a path that was never meant to exist — exactly the
// TROUBLE-LIFECYCLE-004 "cannot stat" misdiagnosis TRBL-086 was filed for.
func TestExpandHomeUnknownHomeRefused(t *testing.T) {
	t.Setenv("HOME", "")
	got, err := expandHome("~/test-trbl086")
	if err == nil {
		t.Fatalf("expandHome(\"~/test-trbl086\") with no home = (%q, nil), want a refusal", got)
	}
}

// TestResolveExpandsTildeInFileValues is TRBL-086's acceptance: a config that
// declares `state_root = "~/test-trbl086"` — the shape the shipped
// examples/config.toml declares — resolves to $HOME/test-trbl086. Every
// path-typed key gets the same treatment, including the elements of a string
// array, and the explain row carries the path the daemon uses rather than the
// one the file spelled.
func TestResolveExpandsTildeInFileValues(t *testing.T) {
	home := trbl086Home(t)
	// `fs.forbidden_state_roots = []` is this fixture's own concession: home is
	// a t.TempDir() under /tmp, which the DEFAULT forbidden list refuses, and the
	// boot preflight below is what this test drives. That list is not what is
	// under test here.
	cfgPath := writeConfig(t, `state_root = "~/test-trbl086"
config_path = "~/test-trbl086/config.toml"
secrets.environment_file = "~/.config/trouble/trouble.env"
checker.state_file = "~/test-trbl086/checker.state.json"
dashboard.token_file = "~/.config/trouble/dashboard-tokens.json"
fs.forbidden_state_roots = []
`)

	res, err := Resolve(nil, nil, cfgPath)
	if err != nil {
		t.Fatalf("Resolve(%s) = %v", cfgPath, err)
	}

	// Acceptance: the resolved state root is $HOME/test-trbl086, not the
	// literal "~/…" the pre-fix parser handed the daemon.
	root := filepath.Join(home, "test-trbl086")
	if got := res.Config.StateRoot; got != root {
		t.Errorf("Config.StateRoot = %q, want %q", got, root)
	}
	for _, c := range []struct{ key, got, want string }{
		{"config_path", res.Config.ConfigPath, filepath.Join(root, "config.toml")},
		{"secrets.environment_file", res.Config.Secrets.EnvironmentFile, filepath.Join(home, ".config", "trouble", "trouble.env")},
		{"checker.state_file", res.Config.Checker.StateFile, filepath.Join(root, "checker.state.json")},
		{"dashboard.token_file", res.Config.Dashboard.TokenFile, filepath.Join(home, ".config", "trouble", "dashboard-tokens.json")},
	} {
		if c.got != c.want {
			t.Errorf("Config value for %s = %q, want %q", c.key, c.got, c.want)
		}
	}
	// The paths DERIVED from the state root follow the expanded value: the
	// pre-fix parser derived them under <cwd>/~/…
	if got, want := res.Config.Lifecycle.HeartbeatPath, filepath.Join(root, "heartbeat.json"); got != want {
		t.Errorf("Lifecycle.HeartbeatPath = %q, want %q", got, want)
	}
	if got, want := res.Config.Sensors.Rules.Dir, filepath.Join(home, "config", "rules.d"); got != want {
		t.Errorf("Sensors.Rules.Dir = %q, want %q (derived from the RESOLVED state root)", got, want)
	}

	// The explain row carries what the daemon uses, with its provenance intact.
	rows := resolvedRows(res)
	row, ok := rows["state_root"]
	if !ok {
		t.Fatal("no resolved row for state_root")
	}
	if row.Source != "file" || row.SourceRef != cfgPath {
		t.Errorf("state_root row provenance = (%q, %q), want (\"file\", %q)", row.Source, row.SourceRef, cfgPath)
	}
	if got := fmt.Sprint(row.Value); got != root {
		t.Errorf("state_root row value = %v, want the resolved %q", row.Value, root)
	}
	// Nothing here is a two-source disagreement: expansion is not a conflict.
	if len(res.Conflicts) != 0 {
		t.Errorf("a tilde-valued config recorded %d conflict rows, want 0: %+v", len(res.Conflicts), res.Conflicts)
	}

	// Boot: the next consumer of the value is the state-root preflight, which is
	// where TRBL-086 surfaced ("cannot stat <cwd>/~/.local/…"). The expansion
	// must be what the daemon stats.
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	sr, err := CheckStateRoot(res.Config)
	if err != nil {
		t.Fatalf("CheckStateRoot(%q) = %v, want the expanded path to be the one the daemon stats", res.Config.StateRoot, err)
	}
	if sr.Path != root {
		t.Errorf("CheckStateRoot resolved %q, want %q", sr.Path, root)
	}
}

// TestResolveExpandsTildePerSource: expansion runs on EVERY source — the file,
// the environment and the flags — before the precedence ladder and the conflict
// scan see anything. It rewrites what a source SAID; it never changes which
// source won, so a flag still beats an env value and an env value still beats
// the file.
func TestResolveExpandsTildePerSource(t *testing.T) {
	home := trbl086Home(t)
	cfgPath := writeConfig(t, `state_root = "~/file-state"`)
	env := []string{"TROUBLE_CONFIG_PATH=~/env-config.toml"}
	args := []string{"--checker-state_file", "~/flag-checker.json"}

	res, err := Resolve(args, env, cfgPath)
	if err != nil {
		t.Fatalf("Resolve = %v", err)
	}
	for _, c := range []struct{ key, source, got, want string }{
		{"state_root", "file", res.Config.StateRoot, filepath.Join(home, "file-state")},
		{"config_path", "env", res.Config.ConfigPath, filepath.Join(home, "env-config.toml")},
		{"checker.state_file", "flag", res.Config.Checker.StateFile, filepath.Join(home, "flag-checker.json")},
	} {
		if c.got != c.want {
			t.Errorf("%s (%s source) = %q, want %q", c.key, c.source, c.got, c.want)
		}
		if row := resolvedRows(res)[c.key]; row.Source != c.source {
			t.Errorf("%s row source = %q, want %q", c.key, row.Source, c.source)
		}
	}
	if len(res.Conflicts) != 0 {
		t.Errorf("three independent sources recorded %d conflict rows, want 0: %+v", len(res.Conflicts), res.Conflicts)
	}
}

// TestResolveTildeEquivalentSpellingsAreNotAConflict: expansion happens BEFORE
// the conflict scan, so the same directory spelled two ways — "~/state" in the
// file, the absolute path in the environment — is ONE value and records no
// TROUBLE-LIFECYCLE-002 row. Against it, a genuinely different value still wins
// the ladder and is still recorded, so the scan is not simply silenced.
func TestResolveTildeEquivalentSpellingsAreNotAConflict(t *testing.T) {
	home := trbl086Home(t)
	cfgPath := writeConfig(t, `state_root = "~/state"`)

	same, err := Resolve(nil, []string{"TROUBLE_STATE_ROOT=" + filepath.Join(home, "state")}, cfgPath)
	if err != nil {
		t.Fatalf("Resolve = %v", err)
	}
	if got, want := same.Config.StateRoot, filepath.Join(home, "state"); got != want {
		t.Errorf("Config.StateRoot = %q, want %q", got, want)
	}
	if len(same.Conflicts) != 0 {
		t.Errorf("two spellings of one directory recorded %d conflict rows, want 0: %+v", len(same.Conflicts), same.Conflicts)
	}

	other, err := Resolve(nil, []string{"TROUBLE_STATE_ROOT=" + filepath.Join(home, "other")}, cfgPath)
	if err != nil {
		t.Fatalf("Resolve = %v", err)
	}
	if got, want := other.Config.StateRoot, filepath.Join(home, "other"); got != want {
		t.Errorf("Control: Config.StateRoot = %q, want the env value %q", got, want)
	}
	if len(other.Conflicts) != 1 {
		t.Errorf("Control: one genuine env-vs-file divergence recorded %d conflict rows, want exactly 1: %+v", len(other.Conflicts), other.Conflicts)
	}
}

// TestResolveRefusesUnexpandableTilde: a "~/…" value with no home to resolve
// against is refused at resolve time with the config code that names the key —
// loud, before the daemon runs — instead of a literal path the state-root
// preflight later reports as "cannot stat".
func TestResolveRefusesUnexpandableTilde(t *testing.T) {
	t.Setenv("HOME", "")
	cfgPath := writeConfig(t, `state_root = "~/test-trbl086"`)

	_, err := Resolve(nil, nil, cfgPath)
	if err == nil {
		t.Fatal("Resolve with an unresolvable tilde value = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), string(types.CodeLifecycle001)) || !strings.Contains(err.Error(), "state_root") {
		t.Errorf("error = %v, want code 001 naming state_root", err)
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

// trbl030Fixture is the config file both TRBL-030 tests resolve: six keys, each
// declared away from its compiled default, and nothing else. The keys the file
// does not mention stay builtin, so the fixture is exactly "the operator
// configured something".
func trbl030Fixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	content := `state_root = "` + filepath.Join(dir, "state") + `"
ingest.bind = "0.0.0.0:7643"
ingest.advertised_host = "trouble.example"
dashboard.bind = "127.0.0.1:7654"
lifecycle.heartbeat_interval = "45s"
spool.budget_bytes = 134217728
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, cfgPath
}

// TRBL-030, arm 1: a compiled default is not a source. Six file keys declared
// away from their defaults plus one key set by the environment only are
// ordinary configuration — the boot records ZERO TROUBLE-LIFECYCLE-002 rows,
// and the deviation stays legible on the ordinary ConfigValue row's `source`
// field instead (SPEC-12 §3.1f).
func TestResolveFileVsDefaultEmitsNoConflict(t *testing.T) {
	dir, cfgPath := trbl030Fixture(t)
	envOnly := "lifecycle.drain_timeout"
	env := []string{"TROUBLE_LIFECYCLE_DRAIN_TIMEOUT=45s"}

	res, err := Resolve(nil, env, cfgPath)
	if err != nil {
		t.Fatalf("Resolve(%s) = %v", cfgPath, err)
	}

	// Premise, asserted first so a green result cannot be vacuous: every key
	// below really is away from its builtin default, and its row names the
	// source that set it.
	base, err := Resolve(nil, nil, filepath.Join(dir, "absent.toml"))
	if err != nil {
		t.Fatalf("Resolve(no file) = %v", err)
	}
	baseRows, rows := resolvedRows(base), resolvedRows(res)
	declared := []string{
		"state_root", "ingest.bind", "ingest.advertised_host", "dashboard.bind",
		"lifecycle.heartbeat_interval", "spool.budget_bytes",
	}
	for _, key := range declared {
		row, ok := rows[key]
		if !ok {
			t.Fatalf("no resolved row for %q", key)
		}
		if row.Source != "file" || row.SourceRef != cfgPath {
			t.Errorf("%s provenance = (%q, %q), want (\"file\", %q)", key, row.Source, row.SourceRef, cfgPath)
		}
		if fmt.Sprint(row.Value) == fmt.Sprint(baseRows[key].Value) {
			t.Errorf("premise: %s = %v is the builtin default, so this case is not a file-vs-default difference", key, row.Value)
		}
	}
	if row, ok := rows[envOnly]; !ok || row.Source != "env" {
		t.Fatalf("%s row = (%+v, present=%v), want source=env: a key only the environment sets is ONE operator source", envOnly, row, ok)
	} else if fmt.Sprint(row.Value) == fmt.Sprint(baseRows[envOnly].Value) {
		t.Errorf("premise: %s = %v is the builtin default", envOnly, row.Value)
	}

	// The defect: 7 keys away from their defaults, none of them disagreeing with
	// another OPERATOR source, must produce no conflict row at all.
	if len(res.Conflicts) != 0 {
		t.Errorf("a normal boot recorded %d TROUBLE-LIFECYCLE-002 rows, want 0 (a compiled default is not a source): %+v",
			len(res.Conflicts), res.Conflicts)
	}

	// The boot config record is the observable: it must carry no 002 row.
	w := &watermarkWriter{}
	if err := WriteConfigRecord(w, res); err != nil {
		t.Fatalf("WriteConfigRecord = %v", err)
	}
	drafts := w.drafts()
	if len(drafts) != 1 {
		t.Fatalf("boot wrote %d config records, want 1", len(drafts))
	}
	payload := drafts[0].Payload
	if got, ok := payload["conflicts"].(int); !ok || got != 0 {
		t.Errorf("boot config record payload.conflicts = %v (%T), want the integer 0", payload["conflicts"], payload["conflicts"])
	}
	if refs, ok := payload["conflict_refs"]; ok {
		t.Errorf("boot config record carries conflict_refs for a normal boot: %v", refs)
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "TROUBLE-LIFECYCLE-002"); n != 0 {
		t.Errorf("the boot config record contains %d TROUBLE-LIFECYCLE-002 rows, want 0:\n%s", n, b)
	}
}

// TRBL-030, arm 2: the code stays reserved for the case §3.1f defines. One
// genuine env-vs-file divergence on an otherwise all-deviating-from-default
// config produces EXACTLY ONE 002 row carrying both source_refs, and the
// resolved value is the higher-precedence one (recorded, never fatal).
func TestResolveEnvVsFileEmitsExactlyOneConflict(t *testing.T) {
	_, cfgPath := trbl030Fixture(t)
	env := []string{"TROUBLE_INGEST_BIND=127.0.0.1:7643"}

	res, err := Resolve(nil, env, cfgPath)
	if err != nil {
		t.Fatalf("Resolve(%s) = %v", cfgPath, err)
	}
	if got := res.Config.Ingest.Bind; got != "127.0.0.1:7643" {
		t.Errorf("Config.Ingest.Bind = %q, want the env value 127.0.0.1:7643: a 002 row is recorded, never fatal", got)
	}
	if len(res.Conflicts) != 1 {
		t.Fatalf("one env-vs-file divergence recorded %d TROUBLE-LIFECYCLE-002 rows, want exactly 1: %+v", len(res.Conflicts), res.Conflicts)
	}
	c := res.Conflicts[0]
	if c.Key != "ingest.bind" {
		t.Errorf("conflict key = %q, want ingest.bind", c.Key)
	}
	if c.Source != "conflict" || c.Value != "TROUBLE-LIFECYCLE-002" || c.Redacted {
		t.Errorf("conflict row = (source=%q value=%v redacted=%v), want (\"conflict\", \"TROUBLE-LIFECYCLE-002\", false)", c.Source, c.Value, c.Redacted)
	}
	if !strings.Contains(c.SourceRef, "TROUBLE_INGEST_BIND") || !strings.Contains(c.SourceRef, cfgPath) {
		t.Errorf("conflict source_ref = %q, want BOTH source_refs (the env name and the losing file %s)", c.SourceRef, cfgPath)
	}

	w := &watermarkWriter{}
	if err := WriteConfigRecord(w, res); err != nil {
		t.Fatalf("WriteConfigRecord = %v", err)
	}
	drafts := w.drafts()
	if len(drafts) != 1 {
		t.Fatalf("boot wrote %d config records, want 1", len(drafts))
	}
	payload := drafts[0].Payload
	if got, ok := payload["conflicts"].(int); !ok || got != 1 {
		t.Errorf("boot config record payload.conflicts = %v (%T), want the integer 1", payload["conflicts"], payload["conflicts"])
	}
	refs, ok := payload["conflict_refs"].([]types.ConfigValue)
	if !ok || len(refs) != 1 || refs[0].Key != "ingest.bind" {
		t.Fatalf("boot config record conflict_refs = %#v, want exactly one row for ingest.bind", payload["conflict_refs"])
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "TROUBLE-LIFECYCLE-002"); n != 1 {
		t.Errorf("the boot config record carries %d 002 rows, want exactly 1:\n%s", n, b)
	}
}
