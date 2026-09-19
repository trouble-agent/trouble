package app

// sensors_config_test.go — TRBL-036 at the composition root.
//
// internal/lifecycle now resolves the whole SPEC-03 §4 detection-plane surface
// (SPEC-12 §3.1e), and the composition root hands the resolved set straight to
// the sensor plane:
//
//	sn, err := sensors.New(res.Values, d.emit, d.redact, time.Now)   // daemon.go
//
// Two failure modes hide in that handoff and neither is visible from the
// resolver's own tests:
//
//   - a row whose Go TYPE the decoder has no reader for (`types.Duration` for a
//     duration key was the TRBL-009 case; the array-of-tables form of
//     `sensors.inotify.paths` was TRBL-036's) fails the boot, long after
//     `config explain` printed the value happily;
//   - a row the decoder accepts but never USES leaves an operator tuning a key
//     that changes nothing.
//
// This file drives the real handoff with a real resolve and asserts the plane
// CHANGED because of the file: the configured rules directory is what the
// sensors subsystem watches, where the compiled default is what it watches with
// no `sensors.rules.dir` at all. A wrong-typed row is driven too, so
// "the decoder accepted it" cannot pass vacuously.

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/lifecycle"
	"github.com/totalwindupflightsystems/trouble/internal/sensors"
	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// sensorsEmitter is an emit sink: this test owns the config handoff, not the
// ledger's write path.
type sensorsEmitter struct{ drafts []types.RecordDraft }

func (e *sensorsEmitter) emit(_ context.Context, d types.RecordDraft) (types.Record, error) {
	e.drafts = append(e.drafts, d)
	return types.Record{Seq: uint64(len(e.drafts)), Kind: d.Kind, Sig: d.Sig}, nil
}

// configuredRuleName is the name of the ONLY rule in the rules directory this
// test configures. It is deliberately absent from the shipped default set
// (examples/rules/10-defaults.toml), so finding it in `Rules()` proves the
// configured directory — not a default — is what the plane loaded.
const configuredRuleName = "trbl036_configured_rule"

const configuredRuleTOML = `[[rule]]
name = "` + configuredRuleName + `"
enabled = true
source = "psi"
for = "30s"
entry_rung = "research"
severity = "low"
cooldown = "10m"
max_runs = 1
verify_window = "10m"
auto_grants = []
hotfix = false

[[rule.match]]
field = "scope"
op = "in"
value = "[\"cpu\"]"
value_type = "string"
`

// writeRulesDir writes the one configured rule file and returns its directory.
func writeRulesDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "rules.d")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir rules.d: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "10-configured.toml"), []byte(configuredRuleTOML), 0o600); err != nil {
		t.Fatalf("write the configured rule: %v", err)
	}
	return dir
}

// sensorConfigFile writes a config the resolver reads for real: a state root
// under the test's temp dir, the required origin identity, and the sensor keys
// under test.
func sensorConfigFile(t *testing.T, extra string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	stateRoot := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatalf("mkdir state root: %v", err)
	}
	path := filepath.Join(dir, "config.toml")
	body := "state_root = \"" + stateRoot + "\"\norigin.host_id = \"trbl036-host\"\n" + extra
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path, stateRoot
}

// buildSensorsFromFile resolves the file and hands the resolved set to the real
// sensor constructor — the same call daemon.go makes.
func buildSensorsFromFile(t *testing.T, extra string) (*sensors.Sensors, lifecycle.Resolved, string) {
	t.Helper()
	path, stateRoot := sensorConfigFile(t, extra)
	res, err := lifecycle.Resolve(nil, nil, path)
	if err != nil {
		t.Fatalf("Resolve(%s) = %v\n"+
			"a `[sensors]` file key must resolve (SPEC-12 §3.1e), not refuse as an unknown file key", path, err)
	}
	emit := &sensorsEmitter{}
	s, err := sensors.New(res.Values, emit.emit, nil, func() time.Time { return time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC) })
	if err != nil {
		t.Fatalf("sensors.New over the resolved set = %v\n"+
			"the resolved rows are handed to this constructor verbatim (daemon.go), so a row it cannot read is a boot failure", err)
	}
	return s, res, stateRoot
}

// ruleNames renders the rule set the plane actually loaded.
func ruleNames(s *sensors.Sensors) []string {
	out := make([]string, 0, len(s.Rules()))
	for _, r := range s.Rules() {
		out = append(out, r.Name)
	}
	return out
}

// TestResolvedSensorsKeysReachTheSensorPlane: the file's `sensors.rules.dir`
// decides which rules directory the sensor plane watches — and with no such key
// the compiled default stands, so the first assertion is evidence rather than
// coincidence.
func TestResolvedSensorsKeysReachTheSensorPlane(t *testing.T) {
	t.Run("a configured rules dir is the one the plane loads", func(t *testing.T) {
		rulesDir := writeRulesDir(t)
		s, res, _ := buildSensorsFromFile(t, "[sensors.rules]\ndir = \""+rulesDir+"\"\n")

		// The resolver carried the key with file provenance...
		row := types.ConfigValue{}
		for _, cv := range res.Values {
			if cv.Key == "sensors.rules.dir" {
				row = cv
			}
		}
		if row.Source != "file" {
			t.Fatalf("sensors.rules.dir provenance = %q, want file (declared in the file under test)", row.Source)
		}
		// ...and the plane's OWN rule set is the configured directory.
		got := ruleNames(s)
		if len(got) != 1 || got[0] != configuredRuleName {
			t.Fatalf("the sensor plane loaded rules %v, want exactly [%s]\n"+
				"the resolved sensors.rules.dir did not reach internal/sensors", got, configuredRuleName)
		}
	})

	t.Run("no sensors.rules.dir keeps the compiled default", func(t *testing.T) {
		s, _, _ := buildSensorsFromFile(t, "[sensors.psi]\nsample_interval = \"3s\"\n")
		got := ruleNames(s)
		if len(got) == 0 {
			t.Fatal("the shipped default rule set is empty: the control arm proves nothing")
		}
		for _, name := range got {
			if name == configuredRuleName {
				t.Fatalf("the control arm loaded the CONFIGURED rule %q with no sensors.rules.dir in the file: %v", configuredRuleName, got)
			}
		}
	})
}

// TestResolvedSensorsRowTypesAreAcceptedAndValidated: the handoff is only
// meaningful if the decoder really reads these rows. The first arm is the whole
// resolved surface (every registered sensor key, default or file); the second
// proves the acceptance is not vacuous by handing the same constructor a row of
// a type no reader accepts.
func TestResolvedSensorsRowTypesAreAcceptedAndValidated(t *testing.T) {
	// Every registerable form: a typed default row, a file value and a flag
	// value for keys of each type, plus the array-of-tables key.
	path, _ := sensorConfigFile(t, `
[sensors.psi]
enabled = true
sample_interval = "3s"

[sensors.disk]
mounts = ["/", "/srv"]

[[sensors.inotify.paths]]
path = "/etc"
recursive = true
`)
	res, err := lifecycle.Resolve([]string{"--sensors-journald-queue", "4096"}, nil, path)
	if err != nil {
		t.Fatalf("Resolve = %v", err)
	}
	emit := &sensorsEmitter{}
	if _, err := sensors.New(res.Values, emit.emit, nil, nil); err != nil {
		t.Fatalf("sensors.New over the resolved set = %v", err)
	}

	// Control: the same constructor refuses a row of a type the decoder has no
	// reader for, so the green arm above is a real acceptance.
	bad := []types.ConfigValue{
		{Key: "origin.host_id", Value: "trbl036-host", Source: "file"},
		{Key: "sensors.psi.enabled", Value: []string{"yes"}, Source: "file"},
	}
	if _, err := sensors.New(bad, emit.emit, nil, nil); err == nil {
		t.Fatal("sensors.New accepted a []string for a bool key: the accepted-arm assertion above would be vacuous")
	} else if !strings.Contains(err.Error(), "sensors.psi.enabled") {
		t.Fatalf("the refusal does not name the key: %v", err)
	}
}

// TestShippedExampleSensorsKeysResolveAndBoot is acceptance criterion 4 at the
// composition root: the file an operator copies declares the detection-plane
// surface as LIVELY READ keys, the resolver reads it without a refusal, and the
// daemon that boots from those bytes carries the file's values (not the
// compiled defaults) in its resolved config — the boot itself is asserted by
// TestShippedExampleConfigBootsToServe, which loads the same file.
func TestShippedExampleSensorsKeysResolveAndBoot(t *testing.T) {
	path := shippedExampleConfigPath(t)
	res, err := lifecycle.Resolve(nil, nil, path)
	if err != nil {
		t.Fatalf("the shipped example must resolve: %v\n"+
			"a stock config that declares `[sensors.*]` keys must not refuse as an unknown file key", err)
	}

	rows := map[string]types.ConfigValue{}
	for _, cv := range res.Values {
		rows[cv.Key] = cv
	}
	for key, want := range map[string]string{
		"sensors.sample_fold_window":    "5m",
		"sensors.merge_window":          "5s",
		"sensors.psi.sample_interval":   "2s",
		"sensors.psi.window":            "2s",
		"sensors.limits.rule_per_min":   "120",
		"sensors.limits.global_per_min": "1200",
	} {
		row, ok := rows[key]
		if !ok {
			t.Errorf("%s has no resolved row", key)
			continue
		}
		if row.Source != "file" {
			t.Errorf("%s resolved from %q, want file: the shipped example must DECLARE it, so what an operator copies is what the daemon reads", key, row.Source)
		}
		if got := renderAppValue(row.Value); got != want {
			t.Errorf("%s in the shipped example = %q, want %q", key, got, want)
		}
	}

	// The values reached the typed config the daemon boots with.
	if got := res.Config.Sensors.PSI.SampleInterval.Std(); got != 2*time.Second {
		t.Errorf("resolved Sensors.PSI.SampleInterval = %s, want 2s", got)
	}
	if got := res.Config.Sensors.Limits.IncidentsPer5m; got != 25 {
		t.Errorf("resolved Sensors.Limits.IncidentsPer5m = %d, want the shipped 25", got)
	}
}

// renderAppValue renders a resolved default-row value the way the explain dump
// reads it.
func renderAppValue(v any) string {
	switch t := v.(type) {
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
	}
	return ""
}
