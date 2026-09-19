package lifecycle

import (
	"encoding/json"
	"fmt"
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
