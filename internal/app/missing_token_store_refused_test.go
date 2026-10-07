package app

// missing_token_store_refused_test.go is QA-TROUBLE-19's composition-root
// regression: a boot whose dashboard token store is MISSING must not serve an
// unauthenticated dashboard. Before the refusal the missing store booted the
// full §2.1 data plane behind a single WARN — every route answered
// 401 + TROUBLE-DASHBOARD-002 (and loopback /health.json 200) — which is the
// state the QA-TROUBLE-19 chaos-errorpath missing-config arm timed out
// against: 127.0.0.1:7644 silently serving with an empty credential store.
//
// The chosen behavior is REFUSE (the TROUBLE-ISSUES-003 / TROUBLE-SKILLS-001
// precedent): the boot records WHY through the same §3.3a machinery the five
// late-landing subsystems use — the health row AND its subsystem_not_built
// record come from one call site — and the dashboard itself falls back to a
// health-only serve so the external stall checker (SPEC-12 §3) sees a
// degraded-but-alive daemon that names the refusal instead of a dead one
// (SPEC-12 §6 edge 8). REGENERATE was rejected: minting credentials at boot
// would put a second mint path beside the CLI's ("the only code path in the
// repository that can mint a dashboard token", SPEC-10 §3.2) and print or
// store plaintext the operator never asked for.
//
// The dashboard-package half of this proof (refusal signal, health-only
// serve, no regeneration ever) lives in
// internal/dashboard/tokens_missing_refusal_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// TestMissingTokenStoreRefusesTheDataPlane boots the REAL daemon with
// dashboard.token_file pointed at a path that does not exist and nothing
// minted anywhere, then asserts the refusal end to end: /health.json answers
// degraded with the dashboard row refused (code + reason), the data plane
// answers 503 + TROUBLE-DASHBOARD-013 (detail dashboard_refused) to BOTH an
// anonymous request and a presented credential, the ledger carries exactly
// one subsystem_not_built record naming the dashboard, and no store file was
// regenerated.
func TestMissingTokenStoreRefusesTheDataPlane(t *testing.T) {
	stampBuildForTest(t)

	root := stateBase(t)
	dashPort, ingestPort := freePortPair(t)
	cfgPath := filepath.Join(root, "config.toml")
	envFile := filepath.Join(root, "trouble.env")

	// THE PREMISE: the token store never existed. No mint, no Save — the
	// exact state a fresh host is in before `trouble dashboard token
	// create` (and the exact state the defeated arm served).
	tokenPath := filepath.Join(root, "dashboard-tokens.json")
	if _, err := os.Stat(tokenPath); !os.IsNotExist(err) {
		t.Fatalf("premise: %s exists, so this boot is not the missing-store case", tokenPath)
	}
	if err := os.WriteFile(envFile, nil, 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
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

[ingest]
bind = "127.0.0.1:%d"

[dashboard]
bind = "127.0.0.1:%d"
token_file = %q
`, root, dashPort, envFile, filepath.Join(root, "heartbeat.json"),
		ingestPort, dashPort, tokenPath)
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan *Daemon, 1)
	done := make(chan error, 1)
	var d *Daemon
	go func() {
		_, err := RunDaemon(ctx, BootOptions{
			Args: []string{"--config", cfgPath},
			Env:  []string{},
			Log:  nil,
			OnReady: func(booted *Daemon) {
				d = booted
				ready <- booted
			},
		})
		done <- err
	}()

	// The boot must REACH READY: the refusal is designed degradation (the
	// health-only serve keeps the watchdog fed), not a dead daemon.
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("the refused-store boot never reached READY: %v", err)
	case <-time.After(bootReadyBase):
		t.Fatalf("the refused-store boot neither reached READY nor settled within %s", bootReadyBase)
	}
	t.Cleanup(func() {
		cancel()
		awaitDrainBudget(t, "the missing-token-store boot", bootDrainBase, done)
	})

	base := fmt.Sprintf("http://127.0.0.1:%d", dashPort)

	// 1. /health.json answers (the checker's surface) and carries the refusal:
	// degraded, one dashboard row, built=false refused=true, code + reason.
	var health types.HealthResponse
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get(base + "/health.json")
		if err != nil {
			t.Fatalf("GET /health.json: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /health.json = %d %s, want 200 (the health surface must survive the refusal)", resp.StatusCode, body)
		}
		if err := json.Unmarshal(body, &health); err != nil {
			t.Fatalf("health is not a HealthResponse: %v (%s)", err, body)
		}
		if len(health.Subsystems) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if health.Status != "degraded" {
		t.Errorf("status = %q, want degraded on a boot that refused the dashboard (QA-TROUBLE-19)", health.Status)
	}
	if got := health.Detail["subsystem_refused"]; got != string(types.CodeLifecycle001) {
		t.Errorf("detail.subsystem_refused = %v, want %s", got, types.CodeLifecycle001)
	}
	if got := health.Detail["subsystem"]; got != "dashboard" {
		t.Errorf("detail.subsystem = %v, want dashboard", got)
	}
	var dashRow *types.SubsystemHealth
	for i := range health.Subsystems {
		if health.Subsystems[i].Name == "dashboard" {
			dashRow = &health.Subsystems[i]
		}
	}
	if dashRow == nil {
		t.Fatalf("health carries no dashboard row (block: %+v)", health.Subsystems)
	}
	if !dashRow.Refused || dashRow.Built {
		t.Errorf("dashboard row = %+v, want built=false refused=true", *dashRow)
	}
	if dashRow.Code != string(types.CodeLifecycle001) {
		t.Errorf("dashboard row code = %q, want %s", dashRow.Code, types.CodeLifecycle001)
	}
	if !strings.Contains(dashRow.Reason, "dashboard token store missing") {
		t.Errorf("dashboard row reason = %q, want it to name the missing store", dashRow.Reason)
	}

	// 2. The data plane is closed: anonymous requests get the boot refusal,
	// not 401 (and certainly not a page).
	resp, err := http.Get(base + "/incidents")
	if err != nil {
		t.Fatalf("GET /incidents: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET /incidents (anonymous) = %d %s, want 503: the unauthenticated dashboard must not serve", resp.StatusCode, body)
	}
	eb := decodeErrorBody(t, body)
	if eb.Error.Code != string(types.CodeDashboard013) || eb.Detail != "dashboard_refused" {
		t.Errorf("anonymous refusal = %s/detail:%s, want %s/dashboard_refused", eb.Error.Code, eb.Detail, types.CodeDashboard013)
	}

	// 3. Even a PRESENTED credential gets the boot refusal: the refusal is
	// about the boot, not the caller.
	req, err := http.NewRequest(http.MethodGet, base+"/incidents", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 32))
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /incidents with a credential: %v", err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET /incidents (with a credential) = %d %s, want 503", resp2.StatusCode, body2)
	}

	// 4. The audit half: exactly one subsystem_not_built record names the
	// dashboard (one truth per refusal — §3.3a).
	if d == nil {
		t.Fatalf("OnReady never handed over the daemon")
	}
	names := map[string]int{}
	if err := d.Ledger.Query().ScanFrom(1, func(r types.Record) bool {
		if r.Kind != types.KLifecycle {
			return true
		}
		if stage, _ := r.Payload["stage"].(string); stage == "subsystem_not_built" {
			name, _ := r.Payload["name"].(string)
			names[name]++
		}
		return true
	}); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("ledger scan: %v", err)
	}
	if names["dashboard"] != 1 {
		t.Errorf("subsystem_not_built records naming the dashboard = %d, want exactly 1 (all: %v)", names["dashboard"], names)
	}

	// 5. REGENERATE was rejected: the store file must still not exist.
	if _, err := os.Stat(tokenPath); !os.IsNotExist(err) {
		t.Errorf("the boot created %s — regeneration is not the chosen behavior", tokenPath)
	}
}

// errBody is the §2.1.3 refusal wire shape (the fields these assertions read).
type errBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Detail string `json:"detail,omitempty"`
}

// decodeErrorBody parses a §2.1.3 refusal body.
func decodeErrorBody(t *testing.T, body []byte) errBody {
	t.Helper()
	var eb errBody
	if err := json.Unmarshal(body, &eb); err != nil {
		t.Fatalf("refusal body is not the error shape: %v (%s)", err, body)
	}
	return eb
}
