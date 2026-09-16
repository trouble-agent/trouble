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

// freePortRange allocates two adjacent ports in ONE probe bind, so the
// preflight's two listeners cannot collide with a sibling test process that
// grabbed the port between the two freePort calls.
func freePortPair(t *testing.T) (int, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	p := ln.Addr().(*net.TCPAddr).Port
	return p, p + 1
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
	return bootDaemonWith(t, SubsystemOptions{})
}

// bootDaemonWith boots the daemon with per-subsystem options (the e2e for the
// wired subsystems passes real project/driver tables through the same
// BootOptions surface an embedding binary would).
func bootDaemonWith(t *testing.T, so SubsystemOptions) *harness {
	t.Helper()
	root := stateBase(t)
	dashPort, ingestPort := freePortPair(t)
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
			Subsystems: so,
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
	res, err := lifecycle.Resolve(nil, nil, filepath.Join(h.d.Cfg.StateRoot, "config.toml"))
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

	// Drive the daemon's own sensors→ladder bridge with a rule the shipped set
	// actually carries (a fired rule's own name), so the ladder's rung metadata
	// lookup behaves exactly as it does in production.
	rule := types.Rule{}
	for _, r := range h.d.Sensors.Rules() {
		if r.Name == "psi_cpu_some_avg10_high" {
			rule = r
		}
	}
	if rule.Name == "" {
		t.Fatalf("the shipped rule set does not contain psi_cpu_some_avg10_high; the e2e needs a real rule")
	}
	sig := types.NewSig(types.SigSource("psi"), "sha256", 1, []byte("e2e-dedup-digest-0001")).String()
	start := time.Now()
	rec, err := h.d.emit(context.Background(), types.RecordDraft{
		Kind:   types.KEvent,
		Sig:    sig,
		Origin: types.Origin{HostID: h.d.Cfg.Origin.HostID, Source: "psi"},
		Actor:  lifecycle.Actor(types.ActorDaemon, "troubled"),
		Payload: map[string]any{
			"fire":       true,
			"rule":       rule.Name,
			"subject":    "cpu",
			"entry_rung": string(rule.EntryRung),
			"severity":   string(rule.Severity),
			"some_avg10": 91.0,
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

	// The incident for THIS trigger is open — the host's own sensors may have
	// opened others concurrently (this box really does have failed units), so the
	// assertion is scoped to the signature the trigger used.
	open := h.d.Ledger.DashReader().OpenIncidents(100)
	inc := ""
	for _, o := range open {
		if o.Sig == sig {
			inc = o.ID
		}
	}
	if inc == "" {
		t.Fatalf("the fired rule's incident is not open (%d open incidents, none for %s)", len(open), sig)
	}
	if tl := h.d.Ledger.DashReader().RecordsForIncident(inc, 0, 50); len(tl) == 0 {
		t.Errorf("the incident has no timeline rows")
	}

	// AC-22, scoped: the same underlying bug arriving again folds into the same
	// incident instead of opening a second one.
	again, err := h.d.emit(context.Background(), types.RecordDraft{
		Kind:   types.KEvent,
		Sig:    sig,
		Origin: types.Origin{HostID: h.d.Cfg.Origin.HostID, Source: "psi"},
		Actor:  lifecycle.Actor(types.ActorDaemon, "troubled"),
		Payload: map[string]any{
			"fire": true, "rule": rule.Name, "subject": "cpu",
			"entry_rung": string(rule.EntryRung), "severity": string(rule.Severity), "some_avg10": 93.0,
		},
	})
	if err != nil {
		t.Fatalf("second emit: %v", err)
	}
	same := 0
	for _, o := range h.d.Ledger.DashReader().OpenIncidents(100) {
		if o.Sig == sig {
			same++
		}
	}
	if same != 1 {
		t.Errorf("recurrence of the same sig produced %d open incidents, want 1 (AC-22)", same)
	}
	if again.Seq <= rec.Seq {
		t.Errorf("the recurrence record did not advance the sequence")
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
		Sig:    types.NewSig(types.SigSource("psi"), "sha256", 1, []byte("e2e-write-digest-0002")).String(),
		Origin: types.Origin{HostID: h.d.Cfg.Origin.HostID, Source: "psi"},
		Actor:  lifecycle.Actor(types.ActorDaemon, "troubled"),
		Payload: map[string]any{
			"fire": true, "rule": "psi_cpu_some_avg10_high", "severity": string(types.SevHigh),
			"entry_rung": string(types.RungPlay), "subject": "cpu", "some_avg10": 88.0,
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
