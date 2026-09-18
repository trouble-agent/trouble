package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/dashboard"
	"github.com/totalwindupflightsystems/trouble/internal/lifecycle"
	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// shipped_example_test.go is the regression gate for the ONE artifact a fresh
// operator is told to copy: examples/config.toml (docs/cmd.md and
// deploy/README.md both send a new user down the copy-then-edit path). It reads
// the SHIPPED file itself — not a testdata copy, not a hand-written fixture —
// so the example cannot rot away from the parser that has to read it.
//
// TRBL-005 is the bug it pins: the shipped example used TOML inline-table syntax
// for verify.zone_windows while internal/lifecycle's reader hands the value to
// the string branch of applyToMapStringDuration, so the FIRST key a new user's
// boot touched refused with TROUBLE-LIFECYCLE-001 and no listener ever opened.
// SPEC-12 §167 pins the accepted form:
//
//	verify.zone_windows  loopback=10m lan=15m tailnet=20m public=30m
//
// What TestShippedExampleConfigBootsToServe starts: the real composition root
// (RunDaemon) end to end — config → state root → ledger → config record → bind
// preflight → sensors → ladder → dashboard on the listener the preflight holds
// → READY — and it asserts /health.json answers over a real socket.
//
// What it does NOT start/do: the example's two host-specific, mutable surfaces
// (state_root and the ingest/dashboard binds) are overridden with CLI flags, the
// same precedence an operator gets — the example's own header says "edit
// host-specific values" — so the test never writes into
// ~/.local/state/trouble and never binds the fixed 7643/7644. Every other line
// of the example is exactly the file that ships. Optional subsystems
// (SubsystemOptions{}) are left at their defaults, exactly as bootDaemon does.

// shippedZoneWindows is SPEC-12 §167's pinned default, verbatim.
const shippedZoneWindows = "loopback=10m lan=15m tailnet=20m public=30m"

// shippedExampleConfigPath resolves examples/config.toml from THIS test file's
// own location (runtime.Caller), so it does not depend on the process CWD.
// A missing example is a hard failure, never a skip: a moved or renamed example
// must break the build (TRBL-005 AC), because the whole point is that the file
// operators copy is the file this test evaluates.
func shippedExampleConfigPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed; cannot locate the shipped example")
	}
	p := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "examples", "config.toml"))
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("the shipped example config is missing at %s (%v): the example this test gates has moved or been deleted — fix the test's path or restore the example, never skip", p, err)
	}
	return p
}

// TestShippedExampleConfigResolves is (a) of TRBL-005: the shipped example must
// load through the real resolver with no error (no TROUBLE-LIFECYCLE-001), and
// the form it ships must be the form SPEC-12 §167 documents.
func TestShippedExampleConfigResolves(t *testing.T) {
	path := shippedExampleConfigPath(t)

	res, err := lifecycle.Resolve(nil, nil, path)
	if err != nil {
		t.Fatalf("lifecycle.Resolve(nil, nil, %s) = %v\n"+
			"the shipped example must resolve: a fresh operator copies this file and boots it (TRBL-005)", path, err)
	}

	// The shipped file must carry the SPEC-12 §167 string form itself — not a
	// form the parser happens to tolerate. Source=file pins that the value came
	// from the example and not from a default.
	var found bool
	for _, cv := range res.Values {
		if cv.Key != "verify.zone_windows" {
			continue
		}
		found = true
		if cv.Source != "file" {
			t.Errorf("verify.zone_windows resolved from %q, want source \"file\" (the value must come from %s, not a builtin default)", cv.Source, path)
		}
		if cv.Value != shippedZoneWindows {
			t.Errorf("verify.zone_windows in %s = %#v, want the SPEC-12 §167 string form %q", path, cv.Value, shippedZoneWindows)
		}
	}
	if !found {
		t.Fatalf("resolved values carry no verify.zone_windows row")
	}

	want := map[string]types.Duration{
		"loopback": "10m",
		"lan":      "15m",
		"tailnet":  "20m",
		"public":   "30m",
	}
	gotZones := res.Config.Verify.ZoneWindows
	if len(gotZones) != len(want) {
		t.Fatalf("verify.zone_windows parsed to %d zones (%v), want %d (%v)", len(gotZones), gotZones, len(want), want)
	}
	for zone, w := range want {
		if gotZones[zone] != w {
			t.Errorf("verify.zone_windows[%s] = %q, want %q", zone, gotZones[zone], w)
		}
	}
}

// TestShippedExampleConfigBootsToServe is (b) of TRBL-005: with the shipped
// example as the config file, boot reaches serve. See the file comment for
// exactly what starts and what is deliberately overridden.
func TestShippedExampleConfigBootsToServe(t *testing.T) {
	path := shippedExampleConfigPath(t)

	root := stateBase(t)
	dashPort, ingestPort := freePortPair(t)

	// A token store minted into the temp state root, exactly as bootDaemonWith
	// does, so the dashboard serves the authenticated surfaces too; /health.json
	// below stays the anonymous assertion.
	tokenPath := filepath.Join(root, "dashboard-tokens.json")
	store, err := dashboard.LoadTokenStore(tokenPath, nil, nil)
	if err != nil {
		t.Fatalf("LoadTokenStore: %v", err)
	}
	_, plaintext, err := store.Mint("shipped-read@e2e", []types.Scope{types.ScopeRead}, time.Now())
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

	// The shipped example is the config FILE; only the mutable/host-specific
	// values a test must never touch are supplied from the higher-precedence
	// flag source (flag > env > file > default).
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
	ready := make(chan *Daemon, 1)
	go func() {
		_, err := RunDaemon(ctx, BootOptions{
			Args:    args,
			Env:     []string{},
			Log:     nil,
			OnReady: func(d *Daemon) { ready <- d },
		})
		done <- err
	}()

	var d *Daemon
	select {
	case d = <-ready:
	case err := <-done:
		cancel()
		t.Fatalf("boot against the shipped example %s did not reach serve: %v\n"+
			"`cp examples/config.toml <workdir>/config.toml && bin/troubled --config config.toml` must boot (TRBL-005)", path, err)
	case <-time.After(30 * time.Second):
		cancel()
		t.Fatalf("boot against the shipped example did not reach READY within 30s")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Errorf("daemon did not drain within 20s")
		}
	})

	// Serve is real: the anonymous health route answers the JSON the external
	// checker parses (loopback bind, the one route exempt from a token).
	base := fmt.Sprintf("http://127.0.0.1:%d", dashPort)
	resp, err := http.Get(base + "/health.json")
	if err != nil {
		t.Fatalf("GET /health.json on the shipped-example boot: %v", err)
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
	if health.LedgerLastSeq == 0 {
		t.Errorf("health.ledger_last_seq = 0; the boot config record from the shipped example should have advanced it")
	}

	// The shipped example's zone windows reached the RUNNING daemon, not just
	// the resolver.
	zones := d.Cfg.Verify.ZoneWindows
	for zone, want := range map[string]types.Duration{"loopback": "10m", "lan": "15m", "tailnet": "20m", "public": "30m"} {
		if zones[zone] != want {
			t.Errorf("running daemon verify.zone_windows[%s] = %q, want %q", zone, zones[zone], want)
		}
	}
}
