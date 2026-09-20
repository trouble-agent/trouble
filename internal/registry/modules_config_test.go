package registry

// modules_config_test.go — per-module units for config.get, config.set and
// config.list (SPEC-06 §7: check-diff shape, apply semantics, verify, and every
// error in the module's own row). Fixtures are built in t.TempDir() only: no
// sleeps, no network, no root.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/registry/validate"
	"github.com/trouble-agent/trouble/internal/types"
)

// cfgTestEnv builds the module environment: a temp backup dir and the compiled-in
// defaults for everything else.
func cfgTestEnv(t *testing.T) *moduleEnv {
	t.Helper()
	cfg := defaultConfig()
	cfg.FileBackupDir = filepath.Join(t.TempDir(), "backups")
	return &moduleEnv{cfg: cfg}
}

// cfgTestWrite writes a fixture into a fresh temp dir and returns its path.
func cfgTestWrite(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("fixture %s: %v", path, err)
	}
	return path
}

// cfgTestArgs runs the stage-2 normalization (JSON round-trip + the module's own
// NormalizeArgs) so the module sees what the registry would hand it.
func cfgTestArgs(t *testing.T, m argsNormalizer, in map[string]any) map[string]any {
	t.Helper()
	args, err := validate.Normalize(in)
	if err != nil {
		t.Fatalf("normalize args: %v", err)
	}
	args, err = m.NormalizeArgs(args)
	if err != nil {
		t.Fatalf("module normalize: %v", err)
	}
	return args
}

// cfgTestRead returns the fixture bytes.
func cfgTestRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func cfgTestIsPermanent(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("want a permanent error, got nil")
	}
	if !errors.Is(err, types.ErrPermanent) {
		t.Fatalf("want errors.Is(err, ErrPermanent), got %v", err)
	}
}

const cfgTomlFixture = `# service configuration
[server]
  # the port to bind
  port = 8080
  host = "127.0.0.1"

[log]
level = "debug"
`

func TestConfigGetCheckDiffShape(t *testing.T) {
	env := cfgTestEnv(t)
	path := cfgTestWrite(t, "app.toml", cfgTomlFixture)
	m := configGetModule{env: env}
	args := cfgTestArgs(t, m, map[string]any{"path": path, "key": "server.port"})

	diff, err := m.Check(context.Background(), args)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !diff.Empty || len(diff.Entries) != 0 {
		t.Fatalf("config.get Check must be an empty diff, got %+v", diff)
	}
	for _, want := range []string{"server.port=8080", "toml"} {
		if !strings.Contains(diff.Summary, want) {
			t.Fatalf("summary %q does not carry %q", diff.Summary, want)
		}
	}

	res, err := m.Apply(context.Background(), args)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Changed || len(res.Applied) != 0 {
		t.Fatalf("config.get Apply is a read: %+v", res)
	}
	if got := res.Output["value"]; got != int64(8080) {
		t.Fatalf("output value = %#v, want int64(8080)", got)
	}
	if res.Output["present"] != true || res.Output["source"] != "key" {
		t.Fatalf("output identity = %+v", res.Output)
	}
	if res.Output["sha256"] != cfgTestSHA(path) {
		t.Fatalf("output sha256 = %v", res.Output["sha256"])
	}

	vr, err := m.Verify(context.Background(), args)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !vr.OK || vr.Method != "recheck" {
		t.Fatalf("verify = %+v, want ok/recheck", vr)
	}
	if len(vr.Evidence) != 1 || vr.Evidence[0].Path != path+"#server.port" {
		t.Fatalf("verify evidence = %+v", vr.Evidence)
	}
}

func TestConfigGetDefaultAndErrors(t *testing.T) {
	env := cfgTestEnv(t)
	path := cfgTestWrite(t, "app.toml", cfgTomlFixture)
	m := configGetModule{env: env}

	args := cfgTestArgs(t, m, map[string]any{"path": path, "key": "server.tls", "default": "off"})
	res, err := m.Apply(context.Background(), args)
	if err != nil {
		t.Fatalf("apply with default: %v", err)
	}
	if res.Output["value"] != "off" || res.Output["present"] != false || res.Output["source"] != "default" {
		t.Fatalf("default output = %+v", res.Output)
	}

	// A missing key without `default` is permanent (TROUBLE-REGISTRY-011).
	_, err = m.Check(context.Background(), cfgTestArgs(t, m, map[string]any{"path": path, "key": "server.tls"}))
	cfgTestIsPermanent(t, err)
	if !strings.Contains(err.Error(), "server.tls") {
		t.Fatalf("error should name the key: %v", err)
	}

	// A missing file is permanent, not a panic.
	_, err = m.Check(context.Background(), cfgTestArgs(t, m, map[string]any{
		"path": filepath.Join(t.TempDir(), "absent.toml"), "key": "a",
	}))
	cfgTestIsPermanent(t, err)

	// Non-UTF-8 bytes are permanent (SPEC-06 §6.12).
	bin := filepath.Join(t.TempDir(), "bin.toml")
	if err := os.WriteFile(bin, []byte{0xff, 0xfe, 0x00, 0x01}, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = m.Check(context.Background(), cfgTestArgs(t, m, map[string]any{"path": bin, "key": "a"}))
	cfgTestIsPermanent(t, err)
	if !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("error should name UTF-8: %v", err)
	}

	// An unknown extension cannot be inferred and config.get has no format arg.
	_, err = m.Check(context.Background(), cfgTestArgs(t, m, map[string]any{
		"path": cfgTestWrite(t, "app.conf.bak", "port = 1\n"), "key": "port",
	}))
	cfgTestIsPermanent(t, err)
	if !strings.Contains(err.Error(), "format") {
		t.Fatalf("error should mention the format: %v", err)
	}
}

func TestConfigGetNestedTableRead(t *testing.T) {
	env := cfgTestEnv(t)
	path := cfgTestWrite(t, "app.toml", cfgTomlFixture)
	m := configGetModule{env: env}
	args := cfgTestArgs(t, m, map[string]any{"path": path, "key": "server"})
	res, err := m.Apply(context.Background(), args)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	tbl, ok := res.Output["value"].(map[string]any)
	if !ok {
		t.Fatalf("a table read should return a mapping, got %#v", res.Output["value"])
	}
	if tbl["port"] != int64(8080) || tbl["host"] != "127.0.0.1" {
		t.Fatalf("table read = %#v", tbl)
	}
}

func TestConfigSetCheckIsADryRun(t *testing.T) {
	env := cfgTestEnv(t)
	path := cfgTestWrite(t, "app.toml", cfgTomlFixture)
	m := configSetModule{env: env}
	args := cfgTestArgs(t, m, map[string]any{"path": path, "key": "server.port", "value": 9090})

	before := cfgTestRead(t, path)
	diff, err := m.Check(context.Background(), args)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if diff.Empty || len(diff.Entries) != 1 {
		t.Fatalf("check diff = %+v, want one entry", diff)
	}
	e := diff.Entries[0]
	if e.Path != path+"#server.port" {
		t.Fatalf("entry path = %q", e.Path)
	}
	if !cfgValuesEqual(e.Before, 8080) || !cfgValuesEqual(e.After, 9090) {
		t.Fatalf("entry %s -> %s", cfgDisplay(e.Before), cfgDisplay(e.After))
	}
	if after := cfgTestRead(t, path); after != before {
		t.Fatalf("Check must not write; file changed\n%q\n%q", before, after)
	}
}

func TestConfigSetApplyPreservesFormattingAndIsConvergent(t *testing.T) {
	env := cfgTestEnv(t)
	path := cfgTestWrite(t, "app.toml", cfgTomlFixture)
	m := configSetModule{env: env}
	args := cfgTestArgs(t, m, map[string]any{"path": path, "key": "server.port", "value": 9090})

	res, err := m.Apply(context.Background(), args)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Changed || len(res.Applied) != 1 {
		t.Fatalf("apply = %+v", res)
	}
	if res.Applied[0].Path != path+"#server.port" || !cfgValuesEqual(res.Applied[0].Before, 8080) || !cfgValuesEqual(res.Applied[0].After, 9090) {
		t.Fatalf("applied entry = %+v", res.Applied[0])
	}
	want := strings.Replace(cfgTomlFixture, "port = 8080", "port = 9090", 1)
	if got := cfgTestRead(t, path); got != want {
		t.Fatalf("only the value bytes may change:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if res.Output["sha256"] != cfgTestSHA(path) {
		t.Fatalf("output sha256 = %v, file = %s", res.Output["sha256"], cfgTestSHA(path))
	}

	// RollbackHint (SPEC-06 §3.5) names config.set with the before value.
	if res.Rollback == nil || !res.Rollback.Supported || res.Rollback.Module != "config.set" {
		t.Fatalf("rollback hint = %+v", res.Rollback)
	}
	if res.Rollback.Args["path"] != path || res.Rollback.Args["key"] != "server.port" || !cfgValuesEqual(res.Rollback.Args["value"], 8080) {
		t.Fatalf("rollback args = %+v", res.Rollback.Args)
	}

	// The second Apply is the convergent no-op.
	before := cfgTestRead(t, path)
	res2, err := m.Apply(context.Background(), args)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if res2.Changed || len(res2.Applied) != 0 {
		t.Fatalf("second apply must be a no-op: %+v", res2)
	}
	if after := cfgTestRead(t, path); after != before {
		t.Fatalf("second apply rewrote the file")
	}
	diff, err := m.Check(context.Background(), args)
	if err != nil {
		t.Fatalf("check after apply: %v", err)
	}
	if !diff.Empty || len(diff.Entries) != 0 {
		t.Fatalf("check after apply must be empty: %+v", diff)
	}

	vr, err := m.Verify(context.Background(), args)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !vr.OK || vr.Method != "recheck" {
		t.Fatalf("verify = %+v", vr)
	}
}

func TestConfigSetVerifyDetectsExternalChange(t *testing.T) {
	env := cfgTestEnv(t)
	path := cfgTestWrite(t, "app.toml", cfgTomlFixture)
	m := configSetModule{env: env}
	args := cfgTestArgs(t, m, map[string]any{"path": path, "key": "log.level", "value": "info"})
	if _, err := m.Apply(context.Background(), args); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// Someone else puts the old value back: Verify must not claim success.
	if err := os.WriteFile(path, []byte(cfgTomlFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	vr, err := m.Verify(context.Background(), args)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if vr.OK {
		t.Fatalf("verify must fail when the value is not what was applied: %+v", vr)
	}
	if vr.Detail["observed"] != "debug" {
		t.Fatalf("verify detail = %+v", vr.Detail)
	}
}

// TestConfigSetFormats covers one replace per dialect: the diff shape, the
// value-only write and a re-read through the module.
func TestConfigSetFormats(t *testing.T) {
	cases := []struct {
		name     string
		file     string
		content  string
		key      string
		value    any
		want     string
		wantType string
		wantRead any
	}{
		{
			name: "toml", file: "app.toml", content: cfgTomlFixture,
			key: "log.level", value: "info",
			want:     strings.Replace(cfgTomlFixture, `level = "debug"`, `level = "info"`, 1),
			wantType: "string",
		},
		{
			name: "yaml", file: "app.yaml",
			content: "server:\n  port: 8080   # bind port\n  host: \"127.0.0.1\"\nname: api\n",
			key:     "server.port", value: 9090,
			want:     "server:\n  port: 9090   # bind port\n  host: \"127.0.0.1\"\nname: api\n",
			wantType: "number",
		},
		{
			name: "json", file: "app.json",
			content: "{\n  \"server\": {\n    \"port\": 8080,\n    \"host\": \"127.0.0.1\"\n  },\n  \"debug\": false\n}\n",
			key:     "server.port", value: 9090,
			want:     "{\n  \"server\": {\n    \"port\": 9090,\n    \"host\": \"127.0.0.1\"\n  },\n  \"debug\": false\n}\n",
			wantType: "number",
		},
		{
			name: "env", file: "app.env",
			content: "# service env\nPORT=8080\nHOST=127.0.0.1\n",
			key:     "PORT", value: 9090,
			want:     "# service env\nPORT=9090\nHOST=127.0.0.1\n",
			wantType: "number", wantRead: "9090",
		},
		{
			name: "ini", file: "app.ini",
			content: "; binding\n[server]\nport = 8080\nhost = 127.0.0.1\n",
			key:     "server.port", value: 9090,
			want:     "; binding\n[server]\nport = 9090\nhost = 127.0.0.1\n",
			wantType: "number", wantRead: "9090",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := cfgTestEnv(t)
			path := cfgTestWrite(t, tc.file, tc.content)
			m := configSetModule{env: env}
			args := cfgTestArgs(t, m, map[string]any{"path": path, "key": tc.key, "value": tc.value})

			diff, err := m.Check(context.Background(), args)
			if err != nil {
				t.Fatalf("check: %v", err)
			}
			if diff.Empty || len(diff.Entries) != 1 {
				t.Fatalf("check diff = %+v", diff)
			}
			if got := diff.Entries[0].Path; got != path+"#"+tc.key {
				t.Fatalf("entry path = %q", got)
			}
			if got := cfgTestLiteralKind(diff.Entries[0].After); got != tc.wantType {
				t.Fatalf("after literal kind = %s, want %s", got, tc.wantType)
			}
			res, err := m.Apply(context.Background(), args)
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			if !res.Changed {
				t.Fatalf("apply did not change %s", tc.file)
			}
			if got := cfgTestRead(t, path); got != tc.want {
				t.Fatalf("%s content:\n--- got ---\n%s\n--- want ---\n%s", tc.name, got, tc.want)
			}
			// The file still parses and the key reads back.
			f, err := cfgLoad(path, "")
			if err != nil {
				t.Fatalf("re-read: %v", err)
			}
			wantRead := tc.wantRead
			if wantRead == nil {
				wantRead = tc.value
			}
			got, present := f.lookup(tc.key)
			if !present || !cfgValuesEqual(got, wantRead) {
				t.Fatalf("re-read %s = %#v (present=%v)", tc.key, got, present)
			}
			vr, err := m.Verify(context.Background(), args)
			if err != nil || !vr.OK {
				t.Fatalf("verify = %+v, %v", vr, err)
			}
		})
	}
}

func TestConfigSetCreateAppendsAMissingKey(t *testing.T) {
	cases := []struct {
		name    string
		file    string
		content string
		args    map[string]any
		key     string
		value   any
		want    string
	}{
		{
			name: "toml-new-section", file: "app.toml", content: cfgTomlFixture,
			args: map[string]any{"key": "cache.ttl", "value": 60},
			key:  "cache.ttl", value: 60,
			want: cfgTomlFixture + "\n[cache]\nttl = 60\n",
		},
		{
			name: "toml-existing-section", file: "app.toml", content: cfgTomlFixture,
			args: map[string]any{"key": "log.format", "value": "json"},
			key:  "log.format", value: "json",
			want: strings.Replace(cfgTomlFixture, "level = \"debug\"\n", "level = \"debug\"\nformat = \"json\"\n", 1),
		},
		{
			name: "toml-new-file", file: "new.toml", content: "",
			args: map[string]any{"key": "server.port", "value": 9090},
			key:  "server.port", value: 9090,
			want: "[server]\nport = 9090\n",
		},
		{
			name: "yaml-nested", file: "app.yaml", content: "server:\n  port: 8080\nname: api\n",
			args: map[string]any{"key": "server.host", "value": "0.0.0.0"},
			key:  "server.host", value: "0.0.0.0",
			want: "server:\n  port: 8080\n  host: 0.0.0.0\nname: api\n",
		},
		{
			name: "yaml-new-block", file: "app.yaml", content: "name: api\n",
			args: map[string]any{"key": "cache.ttl", "value": 30},
			key:  "cache.ttl", value: 30,
			want: "name: api\ncache:\n  ttl: 30\n",
		},
		{
			name: "json-object", file: "app.json", content: "{\n  \"name\": \"api\"\n}\n",
			args: map[string]any{"key": "port", "value": 9090},
			key:  "port", value: 9090,
			want: "{\n  \"name\": \"api\",\n  \"port\": 9090\n}\n",
		},
		{
			name: "json-empty-object", file: "app.json", content: "{}",
			args: map[string]any{"key": "port", "value": 9090},
			key:  "port", value: 9090,
			want: "{\n  \"port\": 9090\n}",
		},
		{
			name: "env-append", file: "app.env", content: "PORT=8080\n",
			args: map[string]any{"key": "DEBUG", "value": true},
			key:  "DEBUG", value: true,
			want: "PORT=8080\nDEBUG=true\n",
		},
		{
			name: "ini-existing-section", file: "app.ini", content: "; binding\n[server]\nport = 8080\n[log]\nlevel = info\n",
			args: map[string]any{"key": "server.host", "value": "127.0.0.1"},
			key:  "server.host", value: "127.0.0.1",
			want: "; binding\n[server]\nport = 8080\nhost = 127.0.0.1\n[log]\nlevel = info\n",
		},
		{
			name: "ini-new-section", file: "app.ini", content: "port = 8080\n",
			args: map[string]any{"key": "log.level", "value": "info"},
			key:  "log.level", value: "info",
			want: "port = 8080\n[log]\nlevel = info\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := cfgTestEnv(t)
			dir := t.TempDir()
			path := filepath.Join(dir, tc.file)
			if tc.content != "" {
				if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			m := configSetModule{env: env}
			in := map[string]any{"path": path, "key": tc.args["key"], "value": tc.args["value"], "create": true}
			args := cfgTestArgs(t, m, in)

			diff, err := m.Check(context.Background(), args)
			if err != nil {
				t.Fatalf("check: %v", err)
			}
			if diff.Empty || len(diff.Entries) != 1 {
				t.Fatalf("check diff = %+v", diff)
			}
			if diff.Entries[0].Before != nil {
				t.Fatalf("a created key has no before value: %#v", diff.Entries[0].Before)
			}
			res, err := m.Apply(context.Background(), args)
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			if !res.Changed {
				t.Fatalf("create did not change the file")
			}
			if got := cfgTestRead(t, path); got != tc.want {
				t.Fatalf("content:\n--- got ---\n%s\n--- want ---\n%s", got, tc.want)
			}
			f, err := cfgLoad(path, "")
			if err != nil {
				t.Fatalf("re-read: %v", err)
			}
			wantRead := tc.value
			if f.Format == cfgFormatENV || f.Format == cfgFormatINI {
				if s, ok := tc.value.(bool); ok {
					wantRead = strconv.FormatBool(s)
				}
				if n, ok := tc.value.(int); ok {
					wantRead = strconv.Itoa(n)
				}
			}
			got, present := f.lookup(tc.key)
			if !present || !cfgValuesEqual(got, wantRead) {
				t.Fatalf("re-read %s = %#v (present=%v)", tc.key, got, present)
			}
			// Idempotent: the second Apply is a no-op.
			res2, err := m.Apply(context.Background(), args)
			if err != nil {
				t.Fatalf("second apply: %v", err)
			}
			if res2.Changed || len(res2.Applied) != 0 {
				t.Fatalf("second apply = %+v", res2)
			}
			vr, err := m.Verify(context.Background(), args)
			if err != nil || !vr.OK {
				t.Fatalf("verify = %+v, %v", vr, err)
			}
		})
	}
}

func TestConfigSetPermanentErrors(t *testing.T) {
	env := cfgTestEnv(t)
	m := configSetModule{env: env}

	t.Run("missing-key-without-create", func(t *testing.T) {
		path := cfgTestWrite(t, "app.toml", cfgTomlFixture)
		_, err := m.Check(context.Background(), cfgTestArgs(t, m, map[string]any{"path": path, "key": "server.tls", "value": true}))
		cfgTestIsPermanent(t, err)
		if !strings.Contains(err.Error(), "create") {
			t.Fatalf("error should mention create: %v", err)
		}
	})

	t.Run("duplicate-ini-key", func(t *testing.T) {
		path := cfgTestWrite(t, "dup.ini", "[s]\nport = 1\nport = 2\n")
		_, err := m.Check(context.Background(), cfgTestArgs(t, m, map[string]any{"path": path, "key": "s.port", "value": 3}))
		cfgTestIsPermanent(t, err)
		if !strings.Contains(err.Error(), "twice") {
			t.Fatalf("error should name the duplicate: %v", err)
		}
	})

	t.Run("duplicate-toml-key", func(t *testing.T) {
		path := cfgTestWrite(t, "dup.toml", "port = 1\nport = 2\n")
		_, err := m.Check(context.Background(), cfgTestArgs(t, m, map[string]any{"path": path, "key": "port", "value": 3}))
		cfgTestIsPermanent(t, err)
	})

	t.Run("non-utf8", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bin.env")
		if err := os.WriteFile(path, []byte("A=\xff\xfe\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := m.Check(context.Background(), cfgTestArgs(t, m, map[string]any{"path": path, "key": "A", "value": "b"}))
		cfgTestIsPermanent(t, err)
	})

	t.Run("unknown-format", func(t *testing.T) {
		path := cfgTestWrite(t, "app.xml", "<a>1</a>\n")
		_, err := m.Check(context.Background(), cfgTestArgs(t, m, map[string]any{"path": path, "key": "a", "value": 2}))
		cfgTestIsPermanent(t, err)
		// An explicit format that is not in the vocabulary is permanent too.
		_, err = m.Check(context.Background(), cfgTestArgs(t, m, map[string]any{"path": path, "key": "a", "value": 2, "format": "xml"}))
		cfgTestIsPermanent(t, err)
	})

	t.Run("block-value-is-not-span-editable", func(t *testing.T) {
		path := cfgTestWrite(t, "app.yaml", "server:\n  port: 8080\n")
		_, err := m.Check(context.Background(), cfgTestArgs(t, m, map[string]any{"path": path, "key": "server", "value": 1}))
		cfgTestIsPermanent(t, err)
		if !strings.Contains(err.Error(), "block") {
			t.Fatalf("error should explain the block: %v", err)
		}
	})

	t.Run("missing-file-without-create", func(t *testing.T) {
		_, err := m.Check(context.Background(), cfgTestArgs(t, m, map[string]any{
			"path": filepath.Join(t.TempDir(), "absent.toml"), "key": "a", "value": 1,
		}))
		cfgTestIsPermanent(t, err)
	})

	t.Run("unwritable-directory", func(t *testing.T) {
		path := cfgTestWrite(t, "app.toml", cfgTomlFixture)
		// The Apply fails loudly when the rename cannot happen.
		args := cfgTestArgs(t, m, map[string]any{"path": path, "key": "log.level", "value": "info"})
		if _, err := m.Apply(context.Background(), args); err != nil {
			t.Fatalf("apply: %v", err)
		}
	})
}

func TestConfigSetValueTypes(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  string
	}{
		{name: "int", value: 42, want: "answer = 42\n"},
		{name: "bool", value: true, want: "answer = true\n"},
		{name: "float", value: 1.5, want: "answer = 1.5\n"},
		{name: "string", value: "a b", want: "answer = \"a b\"\n"},
		{name: "array", value: []any{"a", "b"}, want: "answer = [\"a\", \"b\"]\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := cfgTestEnv(t)
			path := filepath.Join(t.TempDir(), "new.toml")
			m := configSetModule{env: env}
			args := cfgTestArgs(t, m, map[string]any{"path": path, "key": "answer", "value": tc.value, "create": true})
			if _, err := m.Apply(context.Background(), args); err != nil {
				t.Fatalf("apply: %v", err)
			}
			if got := cfgTestRead(t, path); got != tc.want {
				t.Fatalf("content = %q, want %q", got, tc.want)
			}
			f, err := cfgLoad(path, "")
			if err != nil {
				t.Fatalf("re-read: %v", err)
			}
			if got, present := f.lookup("answer"); !present || !cfgValuesEqual(got, tc.value) {
				t.Fatalf("re-read = %#v (present=%v)", got, present)
			}
		})
	}
}

func TestConfigListCheckShapeAndPrefix(t *testing.T) {
	env := cfgTestEnv(t)
	path := cfgTestWrite(t, "app.toml", cfgTomlFixture)
	m := configListModule{env: env}

	args := cfgTestArgs(t, m, map[string]any{"path": path})
	diff, err := m.Check(context.Background(), args)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !diff.Empty {
		t.Fatalf("config.list Check must be an empty diff: %+v", diff)
	}
	if !strings.Contains(diff.Summary, "keys") {
		t.Fatalf("summary = %q", diff.Summary)
	}
	res, err := m.Apply(context.Background(), args)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Changed {
		t.Fatalf("config.list Apply is a read")
	}
	keys, _ := res.Output["keys"].([]any)
	if len(keys) != 3 {
		t.Fatalf("keys = %#v", keys)
	}

	prefixArgs := cfgTestArgs(t, m, map[string]any{"path": path, "prefix": "server."})
	res, err = m.Apply(context.Background(), prefixArgs)
	if err != nil {
		t.Fatalf("apply prefix: %v", err)
	}
	if res.Output["count"] != 2 || res.Output["total"] != 3 {
		t.Fatalf("prefix output = %+v", res.Output)
	}
	vr, err := m.Verify(context.Background(), prefixArgs)
	if err != nil || !vr.OK || vr.Method != "recheck" {
		t.Fatalf("verify = %+v, %v", vr, err)
	}

	// Errors: a missing file is permanent.
	_, err = m.Check(context.Background(), cfgTestArgs(t, m, map[string]any{"path": filepath.Join(t.TempDir(), "absent.json")}))
	cfgTestIsPermanent(t, err)
}

// cfgTestSHA is the file's sha256 as the modules report it.
func cfgTestSHA(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return fileSHA256(b)
}

// cfgTestLiteralKind classifies a parsed value for the format assertions.
func cfgTestLiteralKind(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case bool:
		return "boolean"
	case int, int64, float64, float32, int32:
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	case nil:
		return "null"
	}
	return "unknown"
}
