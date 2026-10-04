package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/dashboard"
	"github.com/trouble-agent/trouble/internal/lifecycle"
	"github.com/trouble-agent/trouble/internal/types"
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
	// Hold BOTH listeners open while choosing, so a parallel test package
	// probing :0 cannot be handed either port (the p, p+1 window race).
	ln1, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln1.Close()
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen2: %v", err)
	}
	defer ln2.Close()
	return ln1.Addr().(*net.TCPAddr).Port, ln2.Addr().(*net.TCPAddr).Port
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

// bootPortPicker is the seam the bind-race retry arms drive: nil for every
// ordinary boot, so freePortPair is consulted as usual; a test installs one to
// return a chosen port pair (e.g. with one port already held by a throwaway
// listener, mimicking a sibling test package that won the :0 probe race) or to
// hold the pair fixed across attempts. bootDaemonWith clears it on return.
var bootPortPicker func(t *testing.T) (int, int)

// bootOnce performs ONE boot attempt: fresh state root, fresh token mint,
// fresh port pick, config + env write, RunDaemon to READY. A boot the bind
// preflight refuses (TROUBLE-LIFECYCLE-003) closes its own ledger and is
// finished — nothing here is reusable by the next attempt, which is why
// bootDaemonWith re-runs this whole step per attempt instead of only
// regenerating the config.
func bootOnce(t *testing.T, so SubsystemOptions) (*harness, error) {
	root := stateBase(t)
	dashPort, ingestPort := freePortPair(t)
	if bootPortPicker != nil {
		dashPort, ingestPort = bootPortPicker(t)
	}
	cfgPath := filepath.Join(root, "config.toml")
	envFile := filepath.Join(root, "trouble.env")

	// The token store is minted first so the checker's environment file and the
	// store agree from the first request.
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
			// bootPhaseObserver is nil unless bootphase_test.go installs it;
			// the boot reports its §4.1 phase boundaries through the same hook
			// an operator harness would use (BootOptions.OnBootPhase).
			OnBootPhase: func(name string, at time.Time) {
				if bootPhaseObserver != nil {
					bootPhaseObserver(name, at)
				}
			},
			Subsystems: so,
		})
		h.done <- err
	}()
	if reached, err := awaitBootReady(t, "daemon", bootReadyBase, ready, h.done); !reached {
		// The boot refused (or settled without READY): RunDaemon has already
		// returned, so release the attempt's context before the harness moves
		// on — a retry builds a fresh everything.
		cancel()
		return nil, err
	}
	t.Cleanup(func() {
		cancel()
		awaitDrainBudget(t, "daemon", bootDrainBase, h.done)
	})
	return h, nil
}

// isBindInUseRefusal reports whether err is a bind preflight refusal
// (TROUBLE-LIFECYCLE-003) caused by a port race — the `address already in
// use` bind error — and not one of the config-shape refusals the same code
// carries (a bad bind string, a duplicate listener, a policy refusal). Those
// are permanent: a fresh port cannot fix them, and retrying would mask a real
// defect.
//
// The preflight wraps the code but FORMATS the bind error into the message
// (`%s`), so the errno is not in the unwrap chain; the discriminator is the
// errno chain when present, else the platform's own EADDRINUSE text the
// message embeds — the same text on every OS the constant builds for.
func isBindInUseRefusal(err error) bool {
	if err == nil || !strings.Contains(err.Error(), string(types.CodeLifecycle003)) {
		return false
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno == syscall.EADDRINUSE
	}
	return strings.Contains(err.Error(), syscall.EADDRINUSE.Error())
}

// maxBootAttempts is the bound on boot attempts per harness: one ordinary
// attempt plus retries only for the TROUBLE-LIFECYCLE-003 bind-in-use race,
// where a sibling test package was handed the probed port between
// freePortPair's release and the daemon's bind preflight (INT-CI-3, CI
// 37163971052: issues.test pid 6463 held the port). Losing five fresh :0
// picks in a row is no longer a race; the last refusal fails loudly, naming
// the code.
const maxBootAttempts = 5

// runBootAttempts is bootDaemonWith's bounded attempt loop, with the failure
// sink injected: production passes t.Fatalf (a refusal must fail the test
// loud), and the exhaust arm of the regression passes a recorder so the
// failure TEXT can be pinned without aborting the test mid-flight. Every
// non-bind failure is permanent by class and fails on the first attempt;
// only the TROUBLE-LIFECYCLE-003 bind-in-use race is retried, each attempt
// re-running the whole boot step with a FRESH freePortPair (a fresh :0 pick
// is the remedy — no sleep-poll), up to maxBootAttempts.
func runBootAttempts(t *testing.T, so SubsystemOptions, failf func(string, ...any)) (*harness, error) {
	// The phase marks of a boot the preflight refuses stop at bind_preflight;
	// clearing the observer between attempts means a retry's marks start from
	// scratch instead of the phase table (bootphase_test) seeing the sequence
	// twice. Attempt 1 keeps whatever the calling test installed.
	var lastErr error
	for attempt := 1; attempt <= maxBootAttempts; attempt++ {
		if attempt > 1 {
			bootPhaseObserver = nil
		}
		h, err := bootOnce(t, so)
		if err == nil {
			return h, nil
		}
		lastErr = err
		if !isBindInUseRefusal(err) {
			failf("daemon did not reach READY: %v", err)
			return nil, err
		}
		t.Logf("boot attempt %d/%d refused by the bind preflight (bind race): %v", attempt, maxBootAttempts, err)
	}
	failf("daemon did not reach READY after %d attempts; last bind refusal: %v", maxBootAttempts, lastErr)
	return nil, lastErr
}

// bootDaemonWith boots the daemon with per-subsystem options (the e2e for the
// wired subsystems passes real project/driver tables through the same
// BootOptions surface an embedding binary would).
//
// The harness retries only the TROUBLE-LIFECYCLE-003 bind-in-use race (see
// runBootAttempts) and reports the last refusal loud after the final attempt.
func bootDaemonWith(t *testing.T, so SubsystemOptions) *harness {
	t.Helper()
	defer func() {
		bootPortPicker = nil
		bootPhaseObserver = nil
	}()
	h, err := runBootAttempts(t, so, t.Fatalf)
	if err != nil {
		// Unreachable: the injected t.Fatalf does not return (FailNow).
		return nil
	}
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

// ---------------------------------------------------------------------------
// The TROUBLE-LIFECYCLE-003 bind-race retry (INT-CI-3). CI runs
// `go test ./...`, so test packages run in parallel and every one of them
// probes :0 for its own scratch listeners; a package can be handed the port
// freePortPair just released between that release and the daemon's bind
// preflight, and the preflight refuses with TROUBLE-LIFECYCLE-003 naming the
// sibling's pid (CI 37163971052: issues.test pid 6463). freePortPair already
// narrows the window by holding both listeners while choosing; the retry is
// the remedy for the window that remains — re-run the whole boot step with a
// FRESH pick, bounded, loud after the last attempt.
// ---------------------------------------------------------------------------

// TestBootDaemonWithRetriesABindInUseRace is the INT-CI-3 regression: a port
// the picker chose is already held (the sibling won the race), and
// bootDaemonWith must come back with a boot that SERVES on a different port,
// not a refusal naming a foreign holder.
func TestBootDaemonWithRetriesABindInUseRace(t *testing.T) {
	// The sibling's holder: a plain throwaway listener kept open across the
	// first boot attempt(s), released once the retry re-picks — the shape of a
	// sibling package that is holding the port when the daemon binds.
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen (the sibling holder): %v", err)
	}
	heldPort := held.Addr().(*net.TCPAddr).Port

	// Attempt 1's pick lands on the held dashboard port (the race already
	// lost); later attempts pick fresh via the ordinary freePortPair path.
	attempt := 0
	bootPortPicker = func(t *testing.T) (int, int) {
		t.Helper()
		attempt++
		if attempt == 1 {
			return heldPort, freePort(t)
		}
		held.Close() // the sibling let go: later picks are ordinary
		return freePortPair(t)
	}

	h := bootDaemonWith(t, SubsystemOptions{})

	// The boot SERVES: the harness's base URL answers an unauthenticated
	// /health.json (SPEC-10 §2.1 footnote 1), so the retried boot demonstrably
	// reached READY on a port the preflight could hold.
	code, body := h.anon("/health.json", "application/json")
	if code != http.StatusOK {
		t.Fatalf("after the bind-race retry GET /health.json = %d %s, want 200", code, body)
	}

	// ... and the serving address is NOT the held port.
	if strings.Contains(h.d.HealthURL, fmt.Sprintf(":%d", heldPort)) {
		t.Fatalf("the boot is serving on the held port %d: the retry must pick a fresh port (%s)", heldPort, h.d.HealthURL)
	}
	if attempt < 2 {
		t.Fatalf("the picker was consulted %d time(s); the race arm must have forced a retry", attempt)
	}
}

// TestBootDaemonWithExhaustedBindRetriesFailsLoud is the exhaust arm: with
// every pick colliding for the whole bounded window, the attempt loop must
// exhaust, fail LOUD through the production failure sink (naming
// TROUBLE-LIFECYCLE-003, the attempt bound and the foreign holder), and hand
// the error back — never a half-built harness or a success.
func TestBootDaemonWithExhaustedBindRetriesFailsLoud(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen (the sibling holder): %v", err)
	}
	heldPort := held.Addr().(*net.TCPAddr).Port
	t.Cleanup(func() { held.Close() })

	// Every pick for the whole window returns the held dashboard port: the
	// race is lost on every attempt.
	bootPortPicker = func(t *testing.T) (int, int) {
		t.Helper()
		return heldPort, freePort(t)
	}
	t.Cleanup(func() { bootPortPicker = nil })

	// Drive the loop with a recorder sink: the production path's t.Fatalf is
	// injected unchanged in bootDaemonWith; here the same failf call site is
	// captured so the failure TEXT can be pinned without aborting the test
	// before its assertions.
	var fatal string
	var fatals int
	failf := func(format string, args ...any) {
		fatals++
		fatal = fmt.Sprintf(format, args...)
	}
	h, err := runBootAttempts(t, SubsystemOptions{}, failf)
	if h != nil || err == nil {
		t.Fatalf("a fully-colliding window returned h=%v, err=%v; the loop must exhaust and fail", h, err)
	}
	if fatals != 1 {
		t.Errorf("the failure sink fired %d times, want exactly 1 (loud, not per-attempt)", fatals)
	}
	for _, want := range []string{
		string(types.CodeLifecycle003),
		fmt.Sprintf("%d attempts", maxBootAttempts),
		fmt.Sprintf("%d", heldPort),
	} {
		if !strings.Contains(fatal, want) {
			t.Errorf("exhaustion failure does not name %q: %s", want, fatal)
		}
	}
	// The refusal must name the FOREIGN HOLDER (TRBL-042's diagnostic), so the
	// failure reads as a hygiene condition, not a defect in the code under test.
	if !strings.Contains(fatal, "TROUBLE-SCRATCH-DAEMON") {
		t.Errorf("exhaustion failure does not carry the scratch-daemon holder hint: %s", fatal)
	}
}

// TestIsBindInUseRefusalDiscriminates pins the retry trigger against the
// refusal's REAL wire shape — the code wrapped, the bind error `%s`-formatted
// into the message (the errno is NOT in the unwrap chain; that shape is
// reproduced from internal/lifecycle/bind.go verbatim). The bind-in-use
// refusal is the ONLY TROUBLE-LIFECYCLE-003 shape the harness retries: a
// config-shape 003 refusal (a malformed bind, no errno) stays permanent, a
// non-003 error is no trigger, and a 003 naming a different errno (EACCES)
// is not one either.
func TestIsBindInUseRefusalDiscriminates(t *testing.T) {
	bindInUse := fmt.Errorf("%w: cannot bind dashboard %q: %v (hint: ss -tlnp)",
		types.CodeLifecycle003, "127.0.0.1:40103",
		&net.OpError{Op: "listen", Net: "tcp", Err: os.NewSyscallError("bind", syscall.EADDRINUSE)})
	if !isBindInUseRefusal(bindInUse) {
		t.Fatalf("the bind-in-use refusal is not recognized: %v", bindInUse)
	}

	// The severed chain the live refusal actually carries: same code, same
	// platform text, errno reachable by neither errors.As nor the hint.
	textOnly := fmt.Errorf("%w: cannot bind ingest %q: %s (hint: ss -tlnp)",
		types.CodeLifecycle003, "127.0.0.1:40104",
		(&net.OpError{Op: "listen", Net: "tcp", Err: os.NewSyscallError("bind", syscall.EADDRINUSE)}).Error())
	if !isBindInUseRefusal(textOnly) {
		t.Fatalf("the severed-chain bind-in-use refusal is not recognized: %v", textOnly)
	}

	permanent := fmt.Errorf("%w: dashboard bind %q has invalid port", types.CodeLifecycle003, "127.0.0.1:nope")
	if isBindInUseRefusal(permanent) {
		t.Fatalf("a config-shape 003 refusal must NOT trigger the retry: %v", permanent)
	}
	otherErrno := fmt.Errorf("%w: cannot bind dashboard %q: %v (hint: ss -tlnp)",
		types.CodeLifecycle003, "127.0.0.1:40105",
		&net.OpError{Op: "listen", Net: "tcp", Err: os.NewSyscallError("bind", syscall.EACCES)})
	if isBindInUseRefusal(otherErrno) {
		t.Fatalf("a 003 refusal for a different errno must NOT trigger the retry: %v", otherErrno)
	}
	if isBindInUseRefusal(errors.New("config file invalid")) {
		t.Fatalf("a non-003 error must not trigger the retry")
	}
	if isBindInUseRefusal(nil) {
		t.Fatalf("nil must not trigger the retry")
	}
}
