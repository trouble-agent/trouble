package app

// missing_rules_dir_test.go is TRBL-022's boot proof of SPEC-03 §3.7b: a state
// root whose sibling `config/rules.d` does not exist — the state a fresh operator
// is in before `trouble install` (or their first `mkdir -p config/rules.d`)
// creates it — boots to `/health.json` **status ok** with the shipped defaults
// active, instead of lifting the whole boot to `degraded` on a normal first-run
// condition.
//
// Before the fix the inotify sensor watched that path unconditionally and
// reported TROUBLE-SENSORS-025 for its absence, so a boot with all five
// subsystem rows built served `status: degraded` with
// `detail.sensor_degraded = inotify`. That is why TestShippedExampleConfigBootsToServe
// had to create the rules directory beside its throwaway state root.
//
// The other half lives in the sensors package: a path that EXISTS and cannot be
// watched still degrades with TROUBLE-SENSORS-025
// (TestUnwatchableRulesPathStillDegrades), so this test cannot be satisfied by
// disabling the check.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/dashboard"
	"github.com/trouble-agent/trouble/internal/types"
)

// shippedDefaultsRuleNames reads the rule names out of the shipped default rule
// file (the same bytes internal/sensors compiles as `defaultRulesTOML`), so
// "the shipped defaults are active" is asserted against the shipped artifact
// rather than against a count this test invented.
func shippedDefaultsRuleNames(t *testing.T) []string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate examples/rules")
	}
	p := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "examples", "rules", "10-defaults.toml"))
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read the shipped defaults %s: %v", p, err)
	}
	re := regexp.MustCompile(`(?m)^\s*name\s*=\s*"([^"]+)"`)
	var out []string
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		out = append(out, m[1])
	}
	if len(out) == 0 {
		t.Fatalf("%s declares no rules; the shipped default set is empty", p)
	}
	sort.Strings(out)
	return out
}

func TestMissingRulesDirBootsToHealthOK(t *testing.T) {
	stampBuildForTest(t)

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home: %v", err)
	}
	// SPEC-03 §3.7: the rules dir is `<state_root>/../config/rules.d`, derived from
	// the state root's PARENT. The throwaway state root therefore gets a parent of
	// its own: a sibling test that created
	// `<home>/.local/state/trouble-test/config/rules.d` would otherwise mask the
	// very state this test is about.
	base, err := os.MkdirTemp(filepath.Join(home, ".local", "state"), "trbl022-norules-")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	root := filepath.Join(base, "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatalf("mkdir state root: %v", err)
	}
	rulesDir := filepath.Join(base, "config", "rules.d")
	if _, err := os.Stat(rulesDir); err == nil {
		t.Fatalf("premise: %s exists, so this boot is not the absent-rules-dir case", rulesDir)
	}

	// The shipped example is the config: it declares one [[projects]] table, so
	// every subsystem row is built and the ONLY unusual condition is the absent
	// rules directory.
	path := shippedExampleConfigPath(t)
	dashPort, ingestPort := freePortPair(t)

	tokenPath := filepath.Join(root, "dashboard-tokens.json")
	store, err := dashboard.LoadTokenStore(tokenPath, nil, nil)
	if err != nil {
		t.Fatalf("LoadTokenStore: %v", err)
	}
	_, plaintext, err := store.Mint("norules-read@e2e", []types.Scope{types.ScopeRead}, time.Now())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	envFile := filepath.Join(root, "trouble.env")
	if err := os.WriteFile(envFile, []byte("TROUBLE_DASHBOARD_TOKEN="+plaintext+"\n"), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}

	args := []string{
		"--config", path,
		"--state_root", root,
		"--secrets-environment_file", envFile,
		"--ingest-bind", fmt.Sprintf("127.0.0.1:%d", ingestPort),
		"--dashboard-bind", fmt.Sprintf("127.0.0.1:%d", dashPort),
		"--dashboard-token_file", tokenPath,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var d *Daemon
	ready := make(chan struct{})
	go func() {
		_, err := RunDaemon(ctx, BootOptions{
			Args:    args,
			Env:     []string{},
			Log:     nil,
			OnReady: func(booted *Daemon) { d = booted; close(ready) },
		})
		done <- err
	}()
	if reached, err := awaitBootReady(t, "the absent-rules-dir boot", bootReadyBase, ready, done); !reached {
		cancel()
		t.Fatalf("the boot against a state root with no sibling config/rules.d did not reach serve: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		awaitDrainBudget(t, "the absent-rules-dir boot", bootDrainBase, done)
	})
	if d == nil {
		t.Fatal("the absent-rules-dir boot signalled READY without handing over a daemon")
	}

	// --- AC1: /health.json is OK on this boot, and nothing sensor-shaped is
	// named as the reason it might not be.
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/health.json", dashPort))
	if err != nil {
		t.Fatalf("GET /health.json: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /health.json = %d %s, want 200", resp.StatusCode, body)
	}
	var health types.HealthResponse
	if err := json.Unmarshal(body, &health); err != nil {
		t.Fatalf("health is not a HealthResponse: %v (%s)", err, body)
	}
	if v, ok := health.Detail["sensor_degraded"]; ok {
		t.Errorf("detail.sensor_degraded = %v on a boot whose only unusual condition is an absent rules directory", v)
	}
	for _, s := range health.Sensors {
		if s.Degraded {
			t.Errorf("sensor %s is degraded with an absent rules.d (%s): SPEC-03 §3.7b keeps it up with the shipped defaults active", s.Sensor, s.Reason)
		}
	}
	if health.Status != "ok" {
		t.Errorf("status = %q on a first-run boot with no rules directory, want ok: %s", health.Status, body)
	}

	// The condition is still VISIBLE: the inotify row names the absent directory.
	var ino *types.SensorHealth
	for i := range health.Sensors {
		if health.Sensors[i].Sensor == types.SenInotify {
			ino = &health.Sensors[i]
		}
	}
	if ino == nil {
		t.Fatalf("health carries no inotify row: %+v", health.Sensors)
	}
	if !ino.Enabled {
		t.Errorf("inotify Enabled = false for an absent rules dir: %+v", *ino)
	}
	if !strings.Contains(ino.Reason, rulesDir) {
		t.Errorf("the inotify Reason must NAME the absent rules directory, got %q", ino.Reason)
	}
	if strings.Contains(ino.Reason, "TROUBLE-SENSORS-") {
		t.Errorf("the absence is reported as a TROUBLE-SENSORS-* refusal: %q", ino.Reason)
	}

	// --- The shipped defaults are the LIVE set, compared against the shipped
	// file's own rule names.
	live := make([]string, 0, len(d.Sensors.Rules()))
	for _, r := range d.Sensors.Rules() {
		live = append(live, r.Name)
	}
	sort.Strings(live)
	want := shippedDefaultsRuleNames(t)
	if strings.Join(live, ",") != strings.Join(want, ",") {
		t.Errorf("live rule set = %v, want the shipped defaults %v", live, want)
	}

	// --- The ledger names the absence once, informationally, and carries no
	// TROUBLE-SENSORS-025 anywhere in the boot.
	var noteRecords []types.Record
	var refusals []string
	var bootRecords []types.Record
	if err := d.Ledger.Query().ScanFrom(1, func(r types.Record) bool {
		bootRecords = append(bootRecords, r)
		return true
	}); err != nil {
		t.Fatalf("ledger scan: %v", err)
	}
	for _, r := range bootRecords {
		if code, _ := r.Payload["error_code"].(string); code == string(types.CodeSensors025) {
			refusals = append(refusals, fmt.Sprintf("%v", r.Payload["detail"]))
		}
		if k, _ := r.Payload["kind"].(string); k == "sensor_note" {
			noteRecords = append(noteRecords, r)
		}
	}
	if len(refusals) != 0 {
		t.Errorf("the boot emitted %d TROUBLE-SENSORS-025 record(s) for an absent rules directory: %v", len(refusals), refusals)
	}
	if len(noteRecords) != 1 {
		t.Fatalf("expected exactly one informational sensor_note record naming the absent directory, got %d: %+v", len(noteRecords), noteRecords)
	}
	note := noteRecords[0]
	if code, _ := note.Payload["error_code"].(string); code != "" {
		t.Errorf("the informational record carries error_code %q, want none (it is a note, not a refusal)", code)
	}
	if p, _ := note.Payload["path"].(string); p != rulesDir {
		t.Errorf("the informational record names path %q, want %q", p, rulesDir)
	}
	if deg, _ := note.Payload["degraded"].(bool); deg {
		t.Error("the informational record says degraded = true")
	}
}
