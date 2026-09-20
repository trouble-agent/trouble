package lifecycle

// sensors_registry_test.go — TRBL-036: the SPEC-03 §4 detection-plane surface
// is a registered key surface (SPEC-12 §3.1e).
//
// The defect this file exists for: `registry()` carried exactly ONE sensors key
// (`sensors.sample_fold_window`), while internal/sensors' own config switch read
// 33. Every other sensor key was therefore an UNKNOWN FILE KEY —
//
//	config: TROUBLE-LIFECYCLE-001: unknown file key "sensors.psi.enabled"   (exit 13)
//
// — so the whole plane (PSI intervals, journald units/queue, D-Bus managers,
// disk mounts, timers, inotify paths/depth, rules dir/debounce, rate limits,
// merge window) could not be tuned from a config file, was unreachable from the
// flag surface, and had no `config explain` row at all.
//
// The halves asserted here, in order:
//
//	(a) COVERAGE, derived from the SENSORS SOURCE — the key list is scanned out
//	    of internal/sensors/config.go's own `case "sensors.` labels and compared
//	    with the registry in BOTH directions, so neither a key the plane reads
//	    nor a key the registry invents can drift unnoticed. A hardcoded
//	    duplicate list here would be the very drift this row exists to kill.
//	(b) the DEFAULTS are the sensors package's own numbers, key by key.
//	(c) PRECEDENCE + PROVENANCE: flag > env > file > default for sensor keys of
//	    every type, with the winning `source`/`source_ref` on each row, plus the
//	    002 conflict record when a lower-precedence source disagrees.
//	(d) the EXPLAIN dump carries every sensor key exactly once, the `--key`
//	    filter form returns exactly the asked row, and the JSON dump marshals.
//	(e) the TYPED HANDOFF: every resolved row's Go type is one the sensors
//	    decoder has a reader for, which is what makes `res.Values` safe to hand
//	    to `sensors.New` — the end-to-end half of that handoff is asserted in
//	    internal/app (the composition root that actually does it).
//	(f) the WHOLE surface set from a config file resolves with no refusal, and
//	    the one declaration-valued key (`sensors.inotify.paths`) is refused by
//	    name from a scalar source instead of being flattened or skipped.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// sensorsConfigSourcePath resolves internal/sensors/config.go from THIS test
// file's own location, so the coverage scan does not depend on the process CWD
// (internal/portability/portability_test.go is the in-repo precedent for
// runtime.Caller).
func sensorsConfigSourcePath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate internal/sensors/config.go")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	p := filepath.Join(root, "internal", "sensors", "config.go")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("the sensors config source is missing at %s (%v): the coverage scan must read the real switch, never a stale copy", p, err)
	}
	return p
}

// sensorKeysReadByThePlane derives the key list from the sensors package's OWN
// `apply` switch: every `case "sensors.<...>"` label names a key the detection
// plane reads. Deriving beats a hardcoded list — the two sides then cannot
// drift, whichever one moves.
func sensorKeysReadByThePlane(t *testing.T) []string {
	t.Helper()
	body, err := os.ReadFile(sensorsConfigSourcePath(t))
	if err != nil {
		t.Fatalf("read the sensors config source: %v", err)
	}
	quoted := regexp.MustCompile(`"([^"]+)"`)
	seen := map[string]bool{}
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "case ") {
			continue
		}
		for _, m := range quoted.FindAllStringSubmatch(line, -1) {
			if strings.HasPrefix(m[1], "sensors.") {
				seen[m[1]] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	// A vacuity floor: a scan that silently matched nothing would otherwise
	// "pass" a comparison against an equally empty registry.
	if len(out) < 30 {
		t.Fatalf("the sensors source scan found %d keys (%v): the scan is broken, not the registry", len(out), out)
	}
	return out
}

// resolvedRows indexes a resolve by key.
func resolvedRows(res Resolved) map[string]types.ConfigValue {
	out := make(map[string]types.ConfigValue, len(res.Values))
	for _, cv := range res.Values {
		out[cv.Key] = cv
	}
	return out
}

// sensorsRegistryKeys returns the keys this package registers under `sensors.`.
func sensorsRegistryKeys(res Resolved) []string {
	var out []string
	for _, cv := range res.Values {
		if strings.HasPrefix(cv.Key, "sensors.") {
			out = append(out, cv.Key)
		}
	}
	sort.Strings(out)
	return out
}

// renderSensorsValue flattens a resolved value to the string an operator reads
// in the explain row.
func renderSensorsValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case types.Duration:
		return string(t)
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case []string:
		return strings.Join(t, ",")
	case []map[string]any:
		return renderInotifyRows(t)
	case []any:
		// The array form a file produces (the reader's array type).
		parts := make([]string, 0, len(t))
		for _, e := range t {
			parts = append(parts, renderSensorsValue(e))
		}
		return strings.Join(parts, ",")
	}
	return ""
}

// renderInotifyRows renders the list of tables deterministically: one
// `{k=v k=v}` group per row, keys sorted, rows in declared order.
func renderInotifyRows(rows []map[string]any) string {
	parts := make([]string, 0, len(rows))
	for _, row := range rows {
		keys := make([]string, 0, len(row))
		for k := range row {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		kv := make([]string, 0, len(keys))
		for _, k := range keys {
			kv = append(kv, k+"="+renderSensorsValue(row[k]))
		}
		parts = append(parts, "{"+strings.Join(kv, " ")+"}")
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// TestSensorKeySurfaceIsRegistered is (a): the registry and the sensors
// package's own switch agree, in both directions.
func TestSensorKeySurfaceIsRegistered(t *testing.T) {
	want := sensorKeysReadByThePlane(t)

	res, err := Resolve(nil, nil, filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("default resolve: %v", err)
	}
	got := sensorsRegistryKeys(res)

	if strings.Join(got, ",") != strings.Join(want, ",") {
		missing := difference(want, got)
		extra := difference(got, want)
		t.Errorf("the registry does not match the surface internal/sensors reads (SPEC-03 §4).\n"+
			"read by the plane but NOT registered (%d): %v\n"+
			"registered but NOT read by the plane (%d): %v\n"+
			"a key read by the plane but unregistered is an unknown file key (TROUBLE-LIFECYCLE-001); "+
			"a registered key the plane never reads is inert config",
			len(missing), missing, len(extra), extra)
	}
}

func difference(a, b []string) []string {
	set := make(map[string]bool, len(b))
	for _, k := range b {
		set[k] = true
	}
	var out []string
	for _, k := range a {
		if !set[k] {
			out = append(out, k)
		}
	}
	return out
}

// sensorKeyDefaults is this file's single statement of what each key defaults
// to. These are internal/sensors' compiled defaults, and
// internal/sensors/config_test.go asserts the same numbers from the sensors
// side: a drift fails one of the two tests, never both silently.
var sensorKeyDefaults = map[string]string{
	"sensors.sample_fold_window":              "5m",
	"sensors.merge_window":                    "5s",
	"sensors.psi.enabled":                     "true",
	"sensors.psi.sample_interval":             "2s",
	"sensors.psi.window":                      "2s",
	"sensors.journald.enabled":                "true",
	"sensors.journald.units":                  "",
	"sensors.journald.follow_all":             "false",
	"sensors.journald.follow_all_max_entries": "2000000",
	"sensors.journald.queue":                  "8192",
	"sensors.journald.queue_bytes":            strconv.Itoa(32 << 20),
	"sensors.journald.max_entry":              "65536",
	"sensors.journald.probe_interval":         "30s",
	"sensors.dbus.enabled":                    "true",
	"sensors.dbus.user_managers":              "self",
	"sensors.dbus.ping_interval":              "30s",
	"sensors.dbus.reconcile_interval":         "300s",
	"sensors.dbus.oomd_probe_interval":        "10m",
	"sensors.disk.enabled":                    "true",
	"sensors.disk.mounts":                     "/",
	"sensors.disk.interval":                   "60s",
	"sensors.timers.enabled":                  "true",
	"sensors.timers.interval":                 "60s",
	"sensors.inotify.enabled":                 "true",
	"sensors.inotify.paths":                   "[]",
	"sensors.inotify.recheck_interval":        "900s",
	"sensors.inotify.max_depth":               "8",
	// sensors.rules.dir is derived from the resolved state root; it is asserted
	// by TestSensorRulesDirFollowsTheStateRoot instead of a literal here.
	"sensors.rules.dir":               "",
	"sensors.rules.reload_debounce":   "500ms",
	"sensors.limits.rule_per_min":     "120",
	"sensors.limits.source_per_min":   "600",
	"sensors.limits.global_per_min":   "1200",
	"sensors.limits.incidents_per_5m": "25",
}

// TestSensorKeyDefaultsAreTheCompiledPosture is (b): every registered sensor
// key resolves to the sensors package's own default, and the map above covers
// the derived key list exactly (so a new key cannot slip in without a default
// being stated).
func TestSensorKeyDefaultsAreTheCompiledPosture(t *testing.T) {
	keys := sensorKeysReadByThePlane(t)
	if len(sensorKeyDefaults) != len(keys) {
		t.Errorf("sensorKeyDefaults covers %d keys, the plane reads %d: %v", len(sensorKeyDefaults), len(keys), keys)
	}
	res, err := Resolve(nil, nil, filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("default resolve: %v", err)
	}
	rows := resolvedRows(res)
	for _, key := range keys {
		if key == "sensors.rules.dir" {
			continue // derived from the state root; see the dedicated test
		}
		want, ok := sensorKeyDefaults[key]
		if !ok {
			t.Errorf("%s has no pinned default in sensorKeyDefaults", key)
			continue
		}
		cv, ok := rows[key]
		if !ok {
			t.Errorf("%s has no resolved row", key)
			continue
		}
		if cv.Source != "default" || cv.SourceRef != "builtin" {
			t.Errorf("%s provenance = (%q, %q), want the builtin default", key, cv.Source, cv.SourceRef)
		}
		if got := renderSensorsValue(cv.Value); got != want {
			t.Errorf("%s default = %q, want %q (internal/sensors pins the same number)", key, got, want)
		}
	}
}

// TestSensorRulesDirFollowsTheStateRoot: the rules directory default is derived
// from the RESOLVED state root (`<state_root>/../config/rules.d`, SPEC-03 §3.7),
// and an explicit `sensors.rules.dir` beats that derivation. The row carries the
// derived path — never the empty marker the registry starts from.
func TestSensorRulesDirFollowsTheStateRoot(t *testing.T) {
	dir := t.TempDir()
	stateRoot := filepath.Join(dir, "state")
	path := filepath.Join(dir, "config.toml")
	writeSensorsConfig(t, path, "state_root = \""+stateRoot+"\"\n")
	res, err := Resolve(nil, nil, path)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := filepath.Join(dir, "config", "rules.d")
	if got := res.Config.Sensors.Rules.Dir; got != want {
		t.Errorf("resolved sensors.rules.dir = %q, want %q (derived from the resolved state root)", got, want)
	}
	if got := renderSensorsValue(resolvedRows(res)["sensors.rules.dir"].Value); got != want {
		t.Errorf("sensors.rules.dir row = %q, want the derived path %q", got, want)
	}

	explicit := filepath.Join(dir, "elsewhere")
	writeSensorsConfig(t, path, "state_root = \""+stateRoot+"\"\n[sensors.rules]\ndir = \""+explicit+"\"\n")
	res, err = Resolve(nil, nil, path)
	if err != nil {
		t.Fatalf("Resolve with an explicit rules dir: %v", err)
	}
	if got := res.Config.Sensors.Rules.Dir; got != explicit {
		t.Errorf("explicit sensors.rules.dir = %q, want %q", got, explicit)
	}
	row := resolvedRows(res)["sensors.rules.dir"]
	if row.Source != "file" || row.SourceRef != path {
		t.Errorf("explicit sensors.rules.dir provenance = (%q, %q), want (file, %q)", row.Source, row.SourceRef, path)
	}
}

func writeSensorsConfig(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// sensorsStep is one expected resolve step: the rendered value and the source
// that must have won.
type sensorsStep struct {
	value     string
	source    string
	sourceRef string
}

// TestSensorKeyPrecedenceAndProvenance is (c): flag > env > file > default for
// sensor keys of every scalar type, with the winning source and source_ref on
// each row — and the 002 conflict whenever a lower-precedence source disagrees.
func TestSensorKeyPrecedenceAndProvenance(t *testing.T) {
	const fileRef = "@file"
	cases := []struct {
		key   string
		file  string
		env   []string
		flags []string
		// steps are the default / file / env / flag expectations, in that order.
		steps [4]sensorsStep
		field func(Config) string
	}{
		{
			key:   "sensors.psi.sample_interval",
			file:  "[sensors.psi]\nsample_interval = \"3s\"\n",
			env:   []string{"TROUBLE_SENSORS_PSI_SAMPLE_INTERVAL=4s"},
			flags: []string{"--sensors-psi-sample_interval", "5s"},
			steps: [4]sensorsStep{
				{"2s", "default", "builtin"},
				{"3s", "file", fileRef},
				{"4s", "env", "TROUBLE_SENSORS_PSI_SAMPLE_INTERVAL"},
				{"5s", "flag", "--sensors-psi-sample_interval"},
			},
			field: func(c Config) string { return string(c.Sensors.PSI.SampleInterval) },
		},
		{
			key:   "sensors.journald.queue",
			file:  "[sensors.journald]\nqueue = 4096\n",
			env:   []string{"TROUBLE_SENSORS_JOURNALD_QUEUE=2048"},
			flags: []string{"--sensors-journald-queue", "1024"},
			steps: [4]sensorsStep{
				{"8192", "default", "builtin"},
				{"4096", "file", fileRef},
				{"2048", "env", "TROUBLE_SENSORS_JOURNALD_QUEUE"},
				{"1024", "flag", "--sensors-journald-queue"},
			},
			field: func(c Config) string { return strconv.Itoa(c.Sensors.Journald.Queue) },
		},
		{
			key:   "sensors.disk.enabled",
			file:  "[sensors.disk]\nenabled = false\n",
			env:   []string{"TROUBLE_SENSORS_DISK_ENABLED=true"},
			flags: []string{"--sensors-disk-enabled", "false"},
			steps: [4]sensorsStep{
				{"true", "default", "builtin"},
				{"false", "file", fileRef},
				{"true", "env", "TROUBLE_SENSORS_DISK_ENABLED"},
				{"false", "flag", "--sensors-disk-enabled"},
			},
			field: func(c Config) string { return strconv.FormatBool(c.Sensors.Disk.Enabled) },
		},
		{
			key:   "sensors.disk.mounts",
			file:  "[sensors.disk]\nmounts = [\"/srv\", \"/var\"]\n",
			env:   []string{"TROUBLE_SENSORS_DISK_MOUNTS=/env1,/env2"},
			flags: []string{"--sensors-disk-mounts", "/flag1,/flag2"},
			steps: [4]sensorsStep{
				{"/", "default", "builtin"},
				{"/srv,/var", "file", fileRef},
				{"/env1,/env2", "env", "TROUBLE_SENSORS_DISK_MOUNTS"},
				{"/flag1,/flag2", "flag", "--sensors-disk-mounts"},
			},
			field: func(c Config) string { return strings.Join(c.Sensors.Disk.Mounts, ",") },
		},
		{
			key:   "sensors.rules.reload_debounce",
			file:  "[sensors.rules]\nreload_debounce = \"250ms\"\n",
			env:   []string{"TROUBLE_SENSORS_RULES_RELOAD_DEBOUNCE=100ms"},
			flags: []string{"--sensors-rules-reload_debounce", "50ms"},
			steps: [4]sensorsStep{
				{"500ms", "default", "builtin"},
				{"250ms", "file", fileRef},
				{"100ms", "env", "TROUBLE_SENSORS_RULES_RELOAD_DEBOUNCE"},
				{"50ms", "flag", "--sensors-rules-reload_debounce"},
			},
			field: func(c Config) string { return string(c.Sensors.Rules.ReloadDebounce) },
		},
		{
			key:   "sensors.inotify.max_depth",
			file:  "[sensors.inotify]\nmax_depth = 4\n",
			env:   []string{"TROUBLE_SENSORS_INOTIFY_MAX_DEPTH=3"},
			flags: []string{"--sensors-inotify-max_depth", "2"},
			steps: [4]sensorsStep{
				{"8", "default", "builtin"},
				{"4", "file", fileRef},
				{"3", "env", "TROUBLE_SENSORS_INOTIFY_MAX_DEPTH"},
				{"2", "flag", "--sensors-inotify-max_depth"},
			},
			field: func(c Config) string { return strconv.Itoa(c.Sensors.Inotify.MaxDepth) },
		},
	}

	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.toml")
			writeSensorsConfig(t, path, tc.file)
			absent := filepath.Join(dir, "absent.toml")

			steps := []struct {
				name        string
				args        []string
				env         []string
				path        string
				want        sensorsStep
				conflicting bool
			}{
				{name: "default", path: absent, want: tc.steps[0]},
				{name: "file", path: path, want: tc.steps[1]},
				{name: "env beats file", env: tc.env, path: path, want: tc.steps[2], conflicting: true},
				{name: "flag beats env and file", args: tc.flags, env: tc.env, path: path, want: tc.steps[3], conflicting: true},
			}
			for _, s := range steps {
				want := s.want
				wantRef := want.sourceRef
				if wantRef == fileRef {
					wantRef = s.path
				}
				res, err := Resolve(s.args, s.env, s.path)
				if err != nil {
					t.Fatalf("%s: Resolve = %v", s.name, err)
				}
				row, ok := resolvedRows(res)[tc.key]
				if !ok {
					t.Fatalf("%s: no %s row in the resolved set", s.name, tc.key)
				}
				if got := renderSensorsValue(row.Value); got != want.value {
					t.Errorf("%s: %s = %q, want %q", s.name, tc.key, got, want.value)
				}
				if row.Source != want.source || row.SourceRef != wantRef {
					t.Errorf("%s: %s provenance = (%q, %q), want (%q, %q)", s.name, tc.key, row.Source, row.SourceRef, want.source, wantRef)
				}
				if got := tc.field(res.Config); got != want.value {
					t.Errorf("%s: the typed Config carries %q, want %q (row and config must agree)", s.name, got, want.value)
				}
				// A lower-precedence source that disagrees is recorded once per
				// boot (TROUBLE-LIFECYCLE-002), never silently outranked.
				if s.conflicting {
					found := false
					for _, c := range res.Conflicts {
						if c.Key == tc.key {
							found = true
						}
					}
					if !found {
						t.Errorf("%s: no TROUBLE-LIFECYCLE-002 conflict row for %s (conflicts=%+v)", s.name, tc.key, res.Conflicts)
					}
				}
			}
		})
	}
}

// TestSensorKeysAppearInTheExplainDump is (d): every sensor key has exactly one
// explain row, the `--key` filter form returns exactly the asked row, and the
// JSON dump marshals with all of them.
func TestSensorKeysAppearInTheExplainDump(t *testing.T) {
	keys := sensorKeysReadByThePlane(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	writeSensorsConfig(t, path, "[sensors.psi]\nsample_interval = \"7s\"\n")
	res, err := Resolve(nil, nil, path)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	all, err := Explain(res, nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	counts := map[string]int{}
	for _, cv := range all {
		counts[cv.Key]++
	}
	for _, key := range keys {
		if counts[key] != 1 {
			t.Errorf("%s appears %d times in the explain dump, want exactly 1", key, counts[key])
		}
	}

	one, err := Explain(res, []string{"sensors.psi.sample_interval"})
	if err != nil {
		t.Fatalf("Explain(--key): %v", err)
	}
	if len(one) != 1 || one[0].Key != "sensors.psi.sample_interval" || renderSensorsValue(one[0].Value) != "7s" {
		t.Errorf("Explain([sensors.psi.sample_interval]) = %+v, want exactly the file's 7s row", one)
	}

	b, err := explainJSON(res.Values)
	if err != nil {
		t.Fatalf("explainJSON: %v", err)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("the explain dump is not valid JSON: %v", err)
	}
	present := map[string]bool{}
	for _, row := range decoded {
		if k, _ := row["key"].(string); k != "" {
			present[k] = true
		}
	}
	for _, key := range keys {
		if !present[key] {
			t.Errorf("%s is missing from the marshalled explain dump", key)
		}
	}
}

// typeName renders a value's Go type the way the reader table below names it.
func typeName(v any) string {
	switch v.(type) {
	case types.Duration:
		return "types.Duration"
	case bool:
		return "bool"
	case int:
		return "int"
	case int64:
		return "int64"
	case string:
		return "string"
	case []string:
		return "[]string"
	case []map[string]any:
		return "[]map[string]any"
	case []any:
		return "[]any"
	}
	return "unreadable"
}

// TestSensorRowTypesAreTheDecodersHandoff is (e): the Go type of every sensor
// row is one the sensors decoder has a reader for. This is the producer half of
// the typed handoff — the composition root hands `res.Values` straight to
// `sensors.New` (internal/app, daemon.go), so a row type with no reader is a
// boot failure, and the type is decided HERE, by the Config field the registry
// entry reads.
func TestSensorRowTypesAreTheDecodersHandoff(t *testing.T) {
	// Readers in internal/sensors/config.go that must accept each row type. The
	// typed default-row forms and the raw file/env/flag forms are all listed;
	// every one of them is driven from the sensors side in
	// internal/sensors/config_test.go.
	readers := map[string]string{
		"types.Duration":   "asDur (typed default row; TRBL-009's sharp edge)",
		"bool":             "asBool",
		"int":              "asInt (typed default row)",
		"int64":            "asInt (a file integer)",
		"string":           "asString",
		"[]string":         "asStringSlice",
		"[]map[string]any": "asInotifyPaths (the array-of-tables form)",
		"[]any":            "asStringSlice / asInotifyPaths (an untyped carrier)",
	}

	keys := sensorKeysReadByThePlane(t)
	res, err := Resolve(nil, nil, filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("default resolve: %v", err)
	}
	rows := resolvedRows(res)
	for _, key := range keys {
		row, ok := rows[key]
		if !ok {
			t.Errorf("%s has no resolved row", key)
			continue
		}
		if _, ok := readers[typeName(row.Value)]; !ok {
			t.Errorf("%s carries a %s row and internal/sensors has no reader for it: the composition root hands this row straight to sensors.New", key, typeName(row.Value))
		}
	}
}

// surfLine is one generated file line plus the rendered value the resolver must
// produce from it.
type surfLine struct {
	key   string
	line  string
	value string
}

// wholeSurfaceFile builds a config file that sets EVERY key of the surface, one
// line per key, generated from the key list and each key's own default type — so
// a newly registered key is covered by this test the moment it exists, with no
// second list to maintain.
func wholeSurfaceFile(t *testing.T, keys []string, stateRoot string) (string, []surfLine) {
	t.Helper()
	def := defaults()
	var lines []surfLine
	for _, key := range keys {
		if key == "sensors.inotify.paths" {
			continue // a list of tables: written last, see below
		}
		var v any
		for _, m := range registry(def) {
			if m.key == key {
				v = m.defaultVal
			}
		}
		switch v.(type) {
		case types.Duration:
			lines = append(lines, surfLine{key, key + ` = "3s"`, "3s"})
		case string:
			lines = append(lines, surfLine{key, key + ` = "/srv/rules.d"`, "/srv/rules.d"})
		case bool:
			lines = append(lines, surfLine{key, key + " = false", "false"})
		case int:
			lines = append(lines, surfLine{key, key + " = 7", "7"})
		case []string:
			lines = append(lines, surfLine{key, key + ` = ["a", "b"]`, "a,b"})
		default:
			t.Fatalf("%s: no file literal for a default of type %T", key, v)
		}
	}
	// The declaration-valued key is written as what it is: a list of tables. It
	// goes LAST because the array-of-tables reader attributes every following
	// `key = value` line to the last row of the array.
	lines = append(lines, surfLine{
		key:   "sensors.inotify.paths",
		line:  "[[sensors.inotify.paths]]\npath = \"/etc\"\nrecursive = true\nmax_depth = 2",
		value: "[{max_depth=2 path=/etc recursive=true}]",
	})
	body := "state_root = \"" + stateRoot + "\"\n"
	for _, l := range lines {
		body += l.line + "\n"
	}
	return body, lines
}

// TestWholeSensorSurfaceResolvesFromAFile is (f): a config file that sets EVERY
// key of the surface resolves with no refusal — the defect was exit 13 on the
// first `sensors.*` key.
func TestWholeSensorSurfaceResolvesFromAFile(t *testing.T) {
	keys := sensorKeysReadByThePlane(t)
	dir := t.TempDir()
	stateRoot := filepath.Join(dir, "state")
	path := filepath.Join(dir, "config.toml")
	body, want := wholeSurfaceFile(t, keys, stateRoot)
	writeSensorsConfig(t, path, body)

	res, err := Resolve(nil, nil, path)
	if err != nil {
		t.Fatalf("Resolve of a config that sets the whole sensors surface = %v\n"+
			"the defect this pins: any `sensors.*` key outside the registry is an unknown file key (TROUBLE-LIFECYCLE-001, exit 13)\n--- file ---\n%s", err, body)
	}

	if len(want) != len(keys) {
		t.Fatalf("the generated file covers %d keys, the surface has %d: %v", len(want), len(keys), keys)
	}
	rows := resolvedRows(res)
	for _, l := range want {
		row, ok := rows[l.key]
		if !ok {
			t.Errorf("%s: no row after a file that sets it", l.key)
			continue
		}
		if row.Source != "file" || row.SourceRef != path {
			t.Errorf("%s provenance = (%q, %q), want (file, %q)", l.key, row.Source, row.SourceRef, path)
		}
		if got := renderSensorsValue(row.Value); got != l.value {
			t.Errorf("%s = %q, want %q", l.key, got, l.value)
		}
	}

	// The declaration reached the typed config too, not only the row; the
	// sensors package remains the judge of each row's contents.
	if len(res.Config.Sensors.Inotify.Paths) != 1 || res.Config.Sensors.Inotify.Paths[0]["path"] != "/etc" {
		t.Errorf("resolved Sensors.Inotify.Paths = %+v, want the declared /etc table", res.Config.Sensors.Inotify.Paths)
	}
	if got := res.Config.Sensors.PSI.SampleInterval; got != "3s" {
		t.Errorf("resolved Sensors.PSI.SampleInterval = %q, want 3s from the file", got)
	}
	if got := res.Config.Sensors.Limits.GlobalPerMin; got != 7 {
		t.Errorf("resolved Sensors.Limits.GlobalPerMin = %d, want 7 from the file", got)
	}
}

// TestInotifyPathsScalarSourceIsRefusedByName: the one key of the surface whose
// value is a declaration. A flag or an environment variable is a scalar and
// cannot express a list of tables, so it is refused BY NAME (SPEC-12 §2.5a) —
// the file owns the key. Never silently skipped: the shape is named in the
// refusal, in SPEC-12 §3.1e and in SPEC-03 §4, and the delegated path (the file
// form) is exercised here and by TestWholeSensorSurfaceResolvesFromAFile.
func TestInotifyPathsScalarSourceIsRefusedByName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	writeSensorsConfig(t, path, "state_root = \"/tmp/x\"\n")

	_, err := Resolve([]string{"--sensors-inotify-paths", "/etc,/srv"}, nil, path)
	if err == nil {
		t.Fatal("a scalar flag value for sensors.inotify.paths was accepted")
	}
	if !strings.Contains(err.Error(), string(types.CodeLifecycle001)) || !strings.Contains(err.Error(), "sensors.inotify.paths") {
		t.Errorf("flag refusal = %v, want TROUBLE-LIFECYCLE-001 naming the key", err)
	}
	if !strings.Contains(err.Error(), "list of tables") {
		t.Errorf("flag refusal = %v, want it to name the shape the key needs", err)
	}

	_, err = Resolve(nil, []string{"TROUBLE_SENSORS_INOTIFY_PATHS=/etc"}, path)
	if err == nil {
		t.Fatal("a scalar environment value for sensors.inotify.paths was accepted")
	}
	if !strings.Contains(err.Error(), "list of tables") {
		t.Errorf("env refusal = %v, want it to name the shape the key needs", err)
	}

	// The file form is what sets it — and the rows land in the typed config.
	writeSensorsConfig(t, path, "state_root = \"/tmp/x\"\n[[sensors.inotify.paths]]\npath = \"/etc\"\nrecursive = true\n\n[[sensors.inotify.paths]]\npath = \"/srv\"\nmask = \"IN_MODIFY\"\n")
	res, err := Resolve(nil, nil, path)
	if err != nil {
		t.Fatalf("the file form of sensors.inotify.paths must resolve: %v", err)
	}
	if len(res.Config.Sensors.Inotify.Paths) != 2 {
		t.Fatalf("resolved paths = %+v, want the two declared tables", res.Config.Sensors.Inotify.Paths)
	}
	if got := res.Config.Sensors.Inotify.Paths[0]["recursive"]; got != true {
		t.Errorf("first row recursive = %v, want true", got)
	}
	if got := res.Config.Sensors.Inotify.Paths[1]["mask"]; got != "IN_MODIFY" {
		t.Errorf("second row mask = %v, want IN_MODIFY", got)
	}
}

// TestSensorFoldWindowKeyStillWorks keeps SPEC-03 §3.8a's key exactly as it was
// before the surface around it was registered: `0` is OFF and survives
// resolution verbatim.
func TestSensorFoldWindowKeyStillWorks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	writeSensorsConfig(t, path, "[sensors]\nsample_fold_window = \"0\"\n")
	res, err := Resolve(nil, nil, path)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	row := resolvedRows(res)["sensors.sample_fold_window"]
	if row.Source != "file" || renderSensorsValue(row.Value) != "0" {
		t.Errorf("fold window row = (%q, %q), want the file's \"0\" (OFF)", renderSensorsValue(row.Value), row.Source)
	}
	if got := res.Config.Sensors.SampleFoldWindow.Std(); got != 0 {
		t.Errorf("OFF parses to %s, want 0", got)
	}
	if got := res.Config.Sensors.PSI.SampleInterval.Std(); got != 2*time.Second {
		t.Errorf("the surface around the fold key moved: sample interval = %s, want the 2s default", got)
	}
}
