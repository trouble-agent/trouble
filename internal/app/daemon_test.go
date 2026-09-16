package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/dashboard"
	"github.com/totalwindupflightsystems/trouble/internal/lifecycle"
	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// daemon_test.go is the composition root's own end-to-end proof, and the only
// test in the tree that starts the real daemon: config → state root → ledger →
// sensors → ladder → dashboard → health → write actions → drain.
//
// It is the AC-16/AC-19/AC-26 evidence that cannot come from a package test:
// the incident it asserts on is created by the same bridge the daemon uses when a
// rule fires, and the fragment it polls is served by the real server over a real
// socket.

type harness struct {
	d       *Daemon
	cancel  context.CancelFunc
	done    chan error
	base    string
	token   string
	readURL string
}

// freePort returns a port nothing holds (bind, read, close).
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// stateBase is a state root the ledger accepts: 0700 under $HOME, never /tmp
// (SPEC-01 §4.3 / SPEC-12 §3.2).
func stateBase(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home: %v", err)
	}
	base := filepath.Join(home, ".local", "state", "trouble-test")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatalf("mkdir base: %v", err)
	}
	dir, err := os.MkdirTemp(base, "e2e-")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func bootDaemon(t *testing.T) *harness {
	t.Helper()
	root := stateBase(t)
	dashPort := freePort(t)
	ingestPort := freePort(t)
	cfgPath := filepath.Join(root, "config.toml")
	envFile := filepath.Join(root, "trouble.env")

	// The token store is minted first so the checker's environment file and the
	// store agree from the first request.
	tokenPath := filepath.Join(root, "dashboard-tokens.json")
	store, err := dashboard.LoadTokenStore(tokenPath, nil)
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
`, root, dashPort, envFile, filepath.Join(root, "heartbeat.json"),
		filepath.Join(root, "checker.alarm"), filepath.Join(root, "checker.state.json"),
		ingestPort, dashPort, tokenPath)

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
	select {
	case <-ready:
	case err := <-h.done:
		t.Fatalf("daemon did not reach READY: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatalf("daemon did not reach READY within 30s")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(20 * time.Second):
			t.Errorf("daemon did not drain within 20s")
		}
	})
	return h
}

func (h *harness) get(t *testing.T, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.base+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Accept", "text/html")
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (h *harness) anon(path string, accept string) (int, string) {
	req, _ := http.NewRequest(http.MethodGet, h.base+path, nil)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestDaemonBootsServesHealthAndDrains(t *testing.T) {
	h := bootDaemon(t)

	// /health.json on a loopback bind is the one route that answers without a
	// token (SPEC-10 §2.1 footnote 1) — that is what lets the external checker
	// distinguish "daemon dead" from "token store broken".
	code, body := h.anon("/health.json", "application/json")
	if code != http.StatusOK {
		t.Fatalf("GET /health.json = %d %s", code, body)
	}
	var health types.HealthResponse
	if err := json.Unmarshal([]byte(body), &health); err != nil {
		t.Fatalf("health is not a HealthResponse: %v (%s)", err, body)
	}
	if health.LedgerLastSeq == 0 {
		t.Errorf("health.ledger_last_seq = 0; the boot config record should have advanced it")
	}
	if health.Version == "" || health.GitSHA == "" {
		t.Errorf("health version triple is empty: %+v", health)
	}

	// An unauthenticated read of a page is 401 + TROUBLE-DASHBOARD-001.
	if code, body := h.anon("/", "text/html"); code != http.StatusUnauthorized {
		t.Errorf("anonymous GET / = %d %s, want 401", code, body)
	} else if !strings.Contains(body, "TROUBLE-DASHBOARD-001") {
		t.Errorf("401 body does not carry the dashboard auth code: %s", body)
	}

	// The same page with a read token renders (AC-19's read-only clause).
	code, body = h.get(t, "/")
	if code != http.StatusOK {
		t.Fatalf("GET / with a read token = %d %s", code, body)
	}
	if !strings.Contains(body, "<html") {
		t.Errorf("GET / did not render an HTML shell")
	}

	// The stall checker, pointed at the live daemon, exits 0: the sequence is
	// advancing, which is the primary signal (SPEC-12 §3.3).
	res, err := lifecycle.Resolve([]string{"--config", filepath.Join(h.d.Cfg.StateRoot, "config.toml")}, []string{}, filepath.Join(h.d.Cfg.StateRoot, "config.toml"))
	if err != nil {
		t.Fatalf("resolve for the checker: %v", err)
	}
	verdict, err := lifecycle.StallCheck(context.Background(), res.Config)
	if err != nil {
		t.Fatalf("StallCheck: %v", err)
	}
	if verdict.Exit != 0 {
		t.Errorf("StallCheck on a live daemon = exit %d (%s): an advancing sequence must not alarm", verdict.Exit, verdict.Reason)
	}
}

func TestDaemonRendersAFiredIncidentWithinTwoSeconds(t *testing.T) {
	h := bootDaemon(t)

	// Drive the daemon's own sensors→ladder bridge: this is the path a fired rule
	// takes (SPEC-03 emits, the ladder admits, the ledger records, the index
	// projects, the dashboard renders).
	sig := "journald\x1fpayment-worker\x1fqueue wedge: pool exhausted"
	start := time.Now()
	rec, err := h.d.emit(context.Background(), types.RecordDraft{
		Kind:   types.KEvent,
		Sig:    sig,
		Origin: types.Origin{HostID: "e2e-host", Source: "journald"},
		Actor:  lifecycle.Actor(types.ActorDaemon, "troubled"),
		Payload: map[string]any{
			"fire":       true,
			"rule":       "journald_queue_wedge",
			"subject":    "payment-worker",
			"entry_rung": string(types.RungPlay),
			"severity":   string(types.SevHigh),
			"message":    "queue wedge",
		},
	})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}

	// Poll the partial exactly as the browser's htmx poll does.
	deadline := start.Add(2 * time.Second)
	var found string
	for time.Now().Before(deadline) {
		code, body := h.get(t, fmt.Sprintf("/partials/incidents?since=%d", rec.Seq-1))
		if code == http.StatusOK && strings.Contains(body, "inc_") {
			found = body
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if found == "" {
		t.Fatalf("no incident fragment containing an incident id within 2s of the trigger (AC-19)")
	}
	if !strings.Contains(found, "incident-rows") {
		t.Errorf("the incidents partial did not return its documented root element: %s", found)
	}

	open := h.d.Ledger.DashReader().OpenIncidents(10)
	if len(open) != 1 {
		t.Fatalf("open incidents = %d, want exactly 1 (AC-22: one incident per underlying bug)", len(open))
	}
	inc := open[0].ID
	if tl := h.d.Ledger.DashReader().RecordsForIncident(inc, 0, 50); len(tl) == 0 {
		t.Errorf("the incident has no timeline rows")
	}

	// The story page renders the incident (AC-16's incident surface).
	code, body := h.get(t, "/incidents/"+inc)
	if code != http.StatusOK {
		t.Errorf("GET /incidents/%s = %d", inc, code)
	}
	if !strings.Contains(body, inc) {
		t.Errorf("the incident page does not name the incident")
	}
}

func TestDaemonRefusesWriteActionsWithAReadToken(t *testing.T) {
	h := bootDaemon(t)
	if _, err := h.d.emit(context.Background(), types.RecordDraft{
		Kind:   types.KEvent,
		Sig:    "psi\x1fcpu\x1fsome avg10 high",
		Origin: types.Origin{HostID: "e2e-host", Source: "psi"},
		Actor:  lifecycle.Actor(types.ActorDaemon, "troubled"),
		Payload: map[string]any{
			"fire": true, "rule": "psi_cpu_some_avg10_high", "severity": string(types.SevHigh),
			"entry_rung": string(types.RungPlay), "subject": "cpu",
		},
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}
	open := h.d.Ledger.DashReader().OpenIncidents(10)
	if len(open) == 0 {
		t.Fatalf("no incident to act on")
	}
	inc := open[0].ID

	// AC-19: every read-only action works without write grants, and every write
	// action is refused by the SERVER (not merely hidden by the UI).
	for _, path := range []string{"/api/incidents/" + inc + "/ack", "/api/incidents/" + inc + "/close", "/api/autonomy"} {
		req, _ := http.NewRequest(http.MethodPost, h.base+path, strings.NewReader(`{"reason":"e2e"}`))
		req.Header.Set("Authorization", "Bearer "+h.token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("POST %s with a read token = %d, want 403 (%s)", path, resp.StatusCode, body)
		}
		if resp.Header.Get("X-Trouble-Required-Scope") == "" {
			t.Errorf("POST %s refusal does not name the required scope", path)
		}
	}
}
