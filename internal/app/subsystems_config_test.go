package app

// subsystems_config_test.go is TRBL-020's end-to-end proof: the `[issues]` and
// `[skills]` tables a config FILE declares reach the subsystems (SPEC-12
// §3.1b), and an invalid declaration is refused at boot with the code and the
// key it names instead of being ignored.
//
// Three levels, each asserting what the one below it cannot:
//
//	unit   — issueDeskConfig/skillsLoopConfig over real TOML bytes: precedence,
//	         which shapes are refused, and the code/message each refusal carries;
//	boot   — a config file with a VALID opt-in builds the configured subsystems
//	         (the desk's configured driver is the one that answers, the loop's
//	         configured source is the one it holds), and /health.json says so;
//	refuse — a config file with an INVALID opt-in boots the subsystem refused:
//	         /health.json carries the code and the reason, and is not ok.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/dashboard"
	"github.com/trouble-agent/trouble/internal/lifecycle"
	"github.com/trouble-agent/trouble/internal/types"
)

// localDuckbrainStub stands in for the SPEC-09 §3.10 local-first backend: it
// answers the two read-only probes the desk's boot path makes (GET /health and
// the header-only KV prefix probe) and records what it was asked, so a test can
// prove the driver really used the base_url the config file declared.
func localDuckbrainStub(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := make([]string, len(paths))
		copy(out, paths)
		return out
	}
}

// bootWithExtraTables boots the real daemon on a complete config file plus the
// subsystem tables under test. The host-specific surfaces a test must never take
// from a shipped file — the state root, both binds, the token store and the
// environment file — are supplied here, exactly as bootDaemonWith supplies them.
func bootWithExtraTables(t *testing.T, tables string) *harness {
	t.Helper()
	root := stateBase(t)
	dashPort, ingestPort := freePortPair(t)
	cfgPath := filepath.Join(root, "config.toml")
	envFile := filepath.Join(root, "trouble.env")

	tokenPath := filepath.Join(root, "dashboard-tokens.json")
	store, err := dashboard.LoadTokenStore(tokenPath, nil, nil)
	if err != nil {
		t.Fatalf("LoadTokenStore: %v", err)
	}
	_, plaintext, err := store.Mint("dash-read@e2e", []types.Scope{types.ScopeRead}, time.Now())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	cfgBody := fmt.Sprintf(`state_root = %q
health_url = "http://127.0.0.1:%d/health.json"

[secrets]
environment_file = %q

[lifecycle]
heartbeat_path = %q
heartbeat_interval = "1s"
heartbeat_stale_after = "5s"
idle_heartbeat_interval = "1s"
drain_timeout = "5s"

[stall]
max_seq_age = "300s"

[checker]
interval = "1s"
confirm_runs = 2
alarm_file = %q
state_file = %q

[ingest]
bind = "127.0.0.1:%d"

[dashboard]
bind = "127.0.0.1:%d"
token_file = %q

%s`, root, dashPort, envFile, filepath.Join(root, "heartbeat.json"),
		filepath.Join(root, "checker.alarm"), filepath.Join(root, "checker.state.json"),
		ingestPort, dashPort, tokenPath, tables)

	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.WriteFile(envFile, []byte("TROUBLE_DASHBOARD_TOKEN="+plaintext+"\n"), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{
		cancel: cancel,
		done:   make(chan error, 1),
		base:   fmt.Sprintf("http://127.0.0.1:%d", dashPort),
		token:  plaintext,
	}
	ready := make(chan struct{})
	go func() {
		_, err := RunDaemon(ctx, BootOptions{
			Args: []string{"--config", cfgPath},
			Env:  []string{},
			Log:  nil,
			OnReady: func(d *Daemon) {
				h.d = d
				close(ready)
			},
		})
		h.done <- err
	}()
	if reached, err := awaitBootReady(t, "daemon", bootReadyBase, ready, h.done); !reached {
		t.Fatalf("daemon did not reach READY: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		awaitDrainBudget(t, "daemon", bootDrainBase, h.done)
	})
	return h
}

// ---------------------------------------------------------------------------
// unit: the two resolvers
// ---------------------------------------------------------------------------

func TestIssueDeskConfigFromTheFileTable(t *testing.T) {
	cases := []struct {
		name    string
		table   string
		opt     *types.IssueDeskConfig
		want    func(types.IssueDeskConfig) error
		wantErr string // substring of the refusal
	}{
		{
			name: "the documented local-first opt-in builds an enabled desk",
			table: `[issues]
enabled = true
primary_driver = "duckbrain"

[issues.drivers.github]
enabled = false

[issues.drivers.duckbrain]
enabled = true
base_url = "http://127.0.0.1:7645"
`,
			want: func(c types.IssueDeskConfig) error {
				if !c.Enabled {
					return fmt.Errorf("desk is not enabled")
				}
				if c.PrimaryDriver != "duckbrain" {
					return fmt.Errorf("primary_driver = %q", c.PrimaryDriver)
				}
				d, ok := driverConfigByName(c, "duckbrain")
				if !ok || !d.Enabled || d.BaseURL != "http://127.0.0.1:7645" {
					return fmt.Errorf("duckbrain block = %+v (found=%v)", d, ok)
				}
				if g, _ := driverConfigByName(c, "github"); g.Enabled {
					return fmt.Errorf("the github block stayed enabled although the file disabled it")
				}
				return nil
			},
		},
		{
			name: "the documented github opt-in keeps every defaulted key",
			table: `[issues]
enabled = true
primary_driver = "github"

[issues.drivers.github]
owner = "acme"
repo = "payment-api"
`,
			want: func(c types.IssueDeskConfig) error {
				g, _ := driverConfigByName(c, "github")
				if !g.Enabled || g.Owner != "acme" || g.Repo != "payment-api" {
					return fmt.Errorf("github block = %+v", g)
				}
				if c.DedupWindow != "30m" || c.BodyMaxBytes != 60000 {
					return fmt.Errorf("a key the file did not declare lost its SPEC-09 §3.4 default: window=%q body=%d", c.DedupWindow, c.BodyMaxBytes)
				}
				return nil
			},
		},
		{
			name:  "an absent table is the compiled default (OFF), not an error",
			table: "",
			want: func(c types.IssueDeskConfig) error {
				if c.Enabled {
					return fmt.Errorf("an absent [issues] table produced an ENABLED desk")
				}
				return nil
			},
		},
		{
			name: "desk on with no enabled driver is refused by name",
			table: `[issues]
enabled = true

[issues.drivers.github]
enabled = false

[issues.drivers.duckbrain]
enabled = false
`,
			wantErr: "no enabled driver",
		},
		{
			name: "desk on over the shipped blocks is refused by the key it misses",
			table: `[issues]
enabled = true
`,
			wantErr: "driver github needs owner and repo",
		},
		{
			name: "an unknown key inside the table is refused by name, not ignored",
			table: `[issues]
enabld = true
`,
			wantErr: "enabld",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &Daemon{}
			if tc.table != "" {
				d.Cfg.IssuesTable = lifecycle.SubsystemTable{Text: tc.table, Keys: []string{"declared"}}
			}
			got, err := issueDeskConfig(d, SubsystemOptions{IssuesCfg: tc.opt})
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("issueDeskConfig = nil error, want a refusal naming %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("refusal = %v, want it to name %q", err, tc.wantErr)
				}
				if !strings.Contains(err.Error(), string(types.CodeIssues003)) {
					t.Errorf("refusal = %v, want the desk's own code %s", err, types.CodeIssues003)
				}
				return
			}
			if err != nil {
				t.Fatalf("issueDeskConfig = %v, want a built config", err)
			}
			if err := tc.want(got); err != nil {
				t.Errorf("resolved config: %v", err)
			}
		})
	}
}

func TestIssueDeskConfigExplicitOptionsWin(t *testing.T) {
	// An embedder's own statement is not overwritten by the file's table — the
	// same precedence the sentinel's project set follows.
	d := &Daemon{}
	d.Cfg.IssuesTable = lifecycle.SubsystemTable{Text: "[issues]\nenabled = true\n", Keys: []string{"enabled"}}
	explicit := types.IssueDeskConfig{Enabled: false}
	got, err := issueDeskConfig(d, SubsystemOptions{IssuesCfg: &explicit})
	if err != nil {
		t.Fatalf("issueDeskConfig = %v", err)
	}
	if got.Enabled {
		t.Errorf("SubsystemOptions.IssuesCfg was ignored: got %+v", got)
	}
}

func TestSkillsLoopConfigFromTheFileTable(t *testing.T) {
	cases := []struct {
		name    string
		table   string
		want    func(types.SkillsConfig) error
		wantErr string
	}{
		{
			name: "the documented local-channel opt-in builds an enabled loop",
			table: `[skills]
enabled = true
source_path = "/srv/skills/release.git"
`,
			want: func(c types.SkillsConfig) error {
				if !c.Enabled || c.SourcePath != "/srv/skills/release.git" {
					return fmt.Errorf("loop config = enabled=%v source_path=%q", c.Enabled, c.SourcePath)
				}
				if c.SourceURL != "" || c.RefMode != "tag" || !c.RequireSignature {
					return fmt.Errorf("a key the file did not declare lost its SPEC-11 §2 default: %+v", c)
				}
				return nil
			},
		},
		{
			name: "the documented remote-channel opt-in builds an enabled loop",
			table: `[skills]
enabled = true
source_url = "https://host/org/skills.git"
`,
			want: func(c types.SkillsConfig) error {
				if !c.Enabled || c.SourceURL != "https://host/org/skills.git" {
					return fmt.Errorf("loop config = enabled=%v source_url=%q", c.Enabled, c.SourceURL)
				}
				return nil
			},
		},
		{
			name:  "an absent table is the compiled default (OFF), not an error",
			table: "",
			want: func(c types.SkillsConfig) error {
				if c.Enabled {
					return fmt.Errorf("an absent [skills] table produced an ENABLED loop")
				}
				return nil
			},
		},
		{
			name:    "loop on with no source is refused by name",
			table:   "[skills]\nenabled = true\n",
			wantErr: "exactly one of source_path or source_url",
		},
		{
			name: "loop on with both sources is refused by name",
			table: `[skills]
enabled = true
source_path = "/srv/skills/release.git"
source_url = "https://host/org/skills.git"
`,
			wantErr: "mutually exclusive",
		},
		{
			name:    "an unknown key inside the table is refused by name, not ignored",
			table:   "[skills]\nenable = true\n",
			wantErr: "enable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &Daemon{}
			if tc.table != "" {
				d.Cfg.SkillsTable = lifecycle.SubsystemTable{Text: tc.table, Keys: []string{"declared"}}
			}
			got, err := skillsLoopConfig(d, SubsystemOptions{})
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("skillsLoopConfig = nil error, want a refusal naming %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("refusal = %v, want it to name %q", err, tc.wantErr)
				}
				if !strings.Contains(err.Error(), string(types.CodeSkills001)) {
					t.Errorf("refusal = %v, want the loop's own code %s", err, types.CodeSkills001)
				}
				return
			}
			if err != nil {
				t.Fatalf("skillsLoopConfig = %v, want a built config", err)
			}
			if err := tc.want(got); err != nil {
				t.Errorf("resolved config: %v", err)
			}
		})
	}
}

func driverConfigByName(c types.IssueDeskConfig, name string) (types.IssueDriverConfig, bool) {
	for _, d := range c.Drivers {
		if d.Name == name {
			return d, true
		}
	}
	return types.IssueDriverConfig{}, false
}

// ---------------------------------------------------------------------------
// boot: a valid file opt-in builds the configured subsystem
// ---------------------------------------------------------------------------

// TestConfigFileOptInBuildsTheConfiguredSubsystems is AC1's boot half: the file
// declares both opt-ins, and the daemon that comes up holds a desk whose
// configured driver is the one that answers and a loop whose configured source
// is the one it carries. Both subsystems are BUILT, so /health.json says so.
func TestConfigFileOptInBuildsTheConfiguredSubsystems(t *testing.T) {
	stub, calls := localDuckbrainStub(t)
	srcDir := t.TempDir()

	tables := fmt.Sprintf(`[issues]
enabled = true
primary_driver = "duckbrain"

[issues.drivers.github]
enabled = false

[issues.drivers.duckbrain]
enabled = true
base_url = %q

[skills]
enabled = true
source_path = %q
`, stub.URL, srcDir)

	h := bootWithExtraTables(t, tables)

	// The desk: built, enabled, and holding exactly the driver the file named —
	// which is also the driver that received the boot probe.
	if h.d.Subsystems == nil || h.d.Subsystems.Issues == nil {
		t.Fatalf("the [issues] opt-in did not build the desk: %+v", h.d.Subsystems)
	}
	rows := h.d.Subsystems.Issues.Health(context.Background())
	if len(rows) != 1 || rows[0].Driver != "duckbrain" {
		t.Fatalf("the built desk reports drivers %+v, want exactly the configured duckbrain driver", rows)
	}
	seen := calls()
	if len(seen) == 0 {
		t.Fatalf("the configured duckbrain driver never reached the base_url the file declared")
	}
	if seen[0] != "/health" {
		t.Errorf("first request to the configured backend = %q, want /health (SPEC-09 §3.10 probe)", seen[0])
	}
	if h.d.Subsystems.Issues.Degraded() {
		t.Errorf("the desk booted degraded although its configured driver answered 200")
	}

	// The loop: built, enabled, holding the configured source.
	if h.d.Subsystems.Skills == nil {
		t.Fatalf("the [skills] opt-in did not build the loop: %+v", h.d.Subsystems)
	}
	if got := h.d.Subsystems.Skills.Cfg; !got.Enabled || got.SourcePath != srcDir {
		t.Errorf("the built loop holds %+v, want enabled=true source_path=%q", got, srcDir)
	}

	// Both rows are built and neither is refused (SPEC-12 §3.3a).
	code, body := h.anon("/health.json", "application/json")
	if code != http.StatusOK {
		t.Fatalf("GET /health.json = %d %s", code, body)
	}
	var health types.HealthResponse
	if err := json.Unmarshal([]byte(body), &health); err != nil {
		t.Fatalf("health is not a HealthResponse: %v (%s)", err, body)
	}
	for _, name := range []string{"issues", "skills"} {
		row := rowOf(t, health, name)
		if !row.Built || row.Refused {
			t.Errorf("%s row = %+v, want built=true refused=false with the file's valid opt-in", name, row)
		}
	}
}

// TestConfigFileInvalidOptInIsRefusedAtBoot is AC3: both of the ticket's invalid
// declarations boot as REFUSED subsystems — the row carries the subsystem's own
// code and the reason names the key that is missing — and the instance never
// reports itself ok.
func TestConfigFileInvalidOptInIsRefusedAtBoot(t *testing.T) {
	// The desk is on with no enabled driver; the loop is on with no source.
	h := bootWithExtraTables(t, `[issues]
enabled = true

[issues.drivers.github]
enabled = false

[issues.drivers.duckbrain]
enabled = false

[skills]
enabled = true
`)

	if h.d.Subsystems.Issues != nil {
		t.Errorf("the desk was built from an invalid declaration: %+v", h.d.Subsystems.Issues)
	}
	if h.d.Subsystems.Skills != nil {
		t.Errorf("the loop was built from an invalid declaration: %+v", h.d.Subsystems.Skills)
	}

	code, body := h.anon("/health.json", "application/json")
	if code != http.StatusOK {
		t.Fatalf("GET /health.json = %d %s", code, body)
	}
	var health types.HealthResponse
	if err := json.Unmarshal([]byte(body), &health); err != nil {
		t.Fatalf("health is not a HealthResponse: %v (%s)", err, body)
	}

	iss := rowOf(t, health, "issues")
	if !iss.Refused || iss.Built {
		t.Errorf("issues row = %+v, want refused=true built=false", iss)
	}
	if iss.Code != string(types.CodeIssues003) {
		t.Errorf("issues row code = %q, want %s", iss.Code, types.CodeIssues003)
	}
	if !strings.Contains(iss.Reason, "no enabled driver") || !strings.Contains(iss.Reason, "issues.drivers") {
		t.Errorf("issues refusal reason = %q, want it to name the missing key (issues.drivers.<name> enabled)", iss.Reason)
	}

	sk := rowOf(t, health, "skills")
	if !sk.Refused || sk.Built {
		t.Errorf("skills row = %+v, want refused=true built=false", sk)
	}
	if sk.Code != string(types.CodeSkills001) {
		t.Errorf("skills row code = %q, want %s", sk.Code, types.CodeSkills001)
	}
	if !strings.Contains(sk.Reason, "source_path") || !strings.Contains(sk.Reason, "source_url") {
		t.Errorf("skills refusal reason = %q, want it to name the missing keys", sk.Reason)
	}

	// SPEC-12 §3.3a: a refused subsystem can never present as healthy.
	if health.Status == "ok" {
		t.Errorf("status = %q with two refused subsystems, want degraded/stalled: %s", health.Status, body)
	}

	// The refusals are also on the ledger: one subsystem_not_built record per
	// refused row, carrying the same detail the health surface serves.
	recorded := map[string]string{}
	for _, rec := range allRecords(t, h) {
		if rec.Kind != types.KLifecycle {
			continue
		}
		if stage, _ := rec.Payload["stage"].(string); stage != "subsystem_not_built" {
			continue
		}
		name, _ := rec.Payload["name"].(string)
		detail, _ := rec.Payload["detail"].(string)
		recorded[name] = detail
	}
	for _, name := range []string{"issues", "skills"} {
		detail, ok := recorded[name]
		if !ok {
			t.Errorf("no subsystem_not_built record for %s", name)
			continue
		}
		if !strings.Contains(detail, "TROUBLE-") {
			t.Errorf("the %s refusal record does not carry a code: %q", name, detail)
		}
	}
}

// TestShippedExampleConfiguredValidlyBootsOk is AC2's second half: the shipped
// example's documented opt-in, applied to the shipped file itself, still boots
// to /health.json status ok. The example is not edited — its bytes are read and
// the documented keys appended, exactly as the file tells an operator to do.
func TestShippedExampleConfiguredValidlyBootsOk(t *testing.T) {
	stampBuildForTest(t)
	path := shippedExampleConfigPath(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	stub, calls := localDuckbrainStub(t)
	srcDir := t.TempDir()

	// The opt-in is applied the way the example tells an operator to apply it: by
	// EDITING the two live tables the file ships (`enabled = false` → the
	// documented opt-in), not by declaring them a second time.
	issuesOptIn := fmt.Sprintf(`[issues]
enabled = true
primary_driver = "duckbrain"

[issues.drivers.github]
enabled = false

[issues.drivers.duckbrain]
enabled = true
base_url = %q
`, stub.URL)
	skillsOptIn := fmt.Sprintf(`[skills]
enabled = true
source_path = %q
`, srcDir)

	doc := string(raw)
	for _, repl := range []struct{ from, to string }{
		{"[issues]\nenabled = false\n", issuesOptIn},
		{"[skills]\nenabled = false\n", skillsOptIn},
	} {
		if !strings.Contains(doc, repl.from) {
			t.Fatalf("examples/config.toml no longer ships the live table %q: this test edits the shipped file, so its shape is part of the contract (TRBL-020 AC2)", repl.from)
		}
		doc = strings.Replace(doc, repl.from, repl.to, 1)
	}

	root := stateBase(t)
	dashPort, ingestPort := freePortPair(t)
	// The sensors watch <state_root>/../config/rules.d unconditionally (SPEC-03
	// §3.7); `trouble install` creates it on a real host, and without it the boot
	// degrades for a reason this test is not about.
	rulesDir := filepath.Join(filepath.Dir(root), "config", "rules.d")
	if err := os.MkdirAll(rulesDir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", rulesDir, err)
	}

	tokenPath := filepath.Join(root, "dashboard-tokens.json")
	store, err := dashboard.LoadTokenStore(tokenPath, nil, nil)
	if err != nil {
		t.Fatalf("LoadTokenStore: %v", err)
	}
	_, plaintext, err := store.Mint("shipped-optin@e2e", []types.Scope{types.ScopeRead}, time.Now())
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

	cfgPath := filepath.Join(root, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(doc), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{
		cancel: cancel,
		done:   make(chan error, 1),
		base:   fmt.Sprintf("http://127.0.0.1:%d", dashPort),
	}
	ready := make(chan struct{})
	go func() {
		_, err := RunDaemon(ctx, BootOptions{
			Args: []string{
				"--config", cfgPath,
				"--state_root", root,
				"--secrets-environment_file", envFile,
				"--ingest-bind", fmt.Sprintf("127.0.0.1:%d", ingestPort),
				"--dashboard-bind", fmt.Sprintf("127.0.0.1:%d", dashPort),
				"--dashboard-token_file", tokenPath,
			},
			Env: []string{},
			Log: nil,
			OnReady: func(d *Daemon) {
				h.d = d
				close(ready)
			},
		})
		h.done <- err
	}()
	if reached, err := awaitBootReady(t, "the shipped example with its documented opt-in", bootReadyBase, ready, h.done); !reached {
		cancel()
		t.Fatalf("the shipped example with its documented opt-in did not reach READY: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		awaitDrainBudget(t, "the shipped example with its documented opt-in", bootDrainBase, h.done)
	})

	code, body := h.anon("/health.json", "application/json")
	if code != http.StatusOK {
		t.Fatalf("GET /health.json = %d %s", code, body)
	}
	var health types.HealthResponse
	if err := json.Unmarshal([]byte(body), &health); err != nil {
		t.Fatalf("health is not a HealthResponse: %v (%s)", err, body)
	}
	for _, name := range subsystemNames {
		row := rowOf(t, health, name)
		if row.Refused {
			t.Errorf("%s row = %+v, want not refused: the documented opt-in is a VALID declaration", name, row)
		}
		if !row.Built {
			t.Errorf("%s row = %+v, want built=true", name, row)
		}
	}
	for _, s := range health.Sensors {
		if s.Degraded {
			t.Errorf("sensor %s is degraded (%s): the status assertion below would fail for a sensor reason", s.Sensor, s.Reason)
		}
	}
	if health.Status != "ok" {
		t.Errorf("status = %q with the example's documented opt-in applied, want ok: %s", health.Status, body)
	}
	if len(calls()) == 0 {
		t.Errorf("the configured duckbrain driver never reached the base_url the opt-in declared")
	}
	if h.d.Subsystems == nil || h.d.Subsystems.Issues == nil || h.d.Subsystems.Skills == nil {
		t.Fatalf("the opt-in did not build both subsystems: %+v", h.d.Subsystems)
	}
	if got := h.d.Subsystems.Skills.Cfg; !got.Enabled || got.SourcePath != srcDir {
		t.Errorf("the built loop holds %+v, want enabled=true source_path=%q", got, srcDir)
	}
}
