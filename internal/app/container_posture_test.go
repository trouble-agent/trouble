package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/dashboard"
	"github.com/trouble-agent/trouble/internal/lifecycle"
	"github.com/trouble-agent/trouble/internal/types"
)

// container_posture_test.go is the AC2 regression of TRBL-028: the shipped
// container config — deploy/container/config.toml, the file the compose stack
// bind-mounts at /etc/trouble/config.toml — must keep booting to serve through
// the REAL composition root. Every key of that file was added because a live
// boot refused its absence (the board row quotes each refusal verbatim), so a
// future edit that drops a key silently re-opens a measured refusal instead of
// failing a test. It reads the SHIPPED file itself — not a testdata copy — for
// the same reason shipped_example_test.go reads examples/config.toml.
//
// What the boot test deliberately does NOT adopt from the file: its fixed
// ports (7643/7644 are never bound by tests), its state root (/data/state does
// not exist on a test host), and its token/env file paths. Those are the
// host-specific surfaces, overridden from the higher-precedence flag source —
// the same precedence an operator's edit gets — exactly as
// TestShippedExampleConfigBootsToServe does for examples/config.toml.
//
// The refusal matrix at the end walks the row's four measured refusal classes
// against the shipped posture: the posture minus the one load-bearing line
// must fail the matching gate, carrying the code the row names. That is the
// negative half of "the shipped posture is load-bearing"; the positive half is
// the boot above, which proves the full file passes every gate at once.

// shippedContainerConfigPath resolves deploy/container/config.toml from this
// test file's location, never from the process CWD.
func shippedContainerConfigPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate the shipped container config")
	}
	p := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "deploy", "container", "config.toml"))
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("the shipped container config is missing at %s (%v): the file this test gates has moved or been deleted — fix the test's path or restore the config, never skip", p, err)
	}
	return p
}

// TestShippedContainerConfigBootsToServe boots the real composition root
// (RunDaemon) with deploy/container/config.toml as the config FILE and only
// the host-specific surfaces overridden. AC2's container shape, asserted on a
// live serve: a published-port posture (proxy-mode ingest, mandated dashboard
// with a public origin, token files inside the validated state root) reaches
// READY and answers authenticated health.
func TestShippedContainerConfigBootsToServe(t *testing.T) {
	stampBuildForTest(t)
	path := shippedContainerConfigPath(t)

	root := stateBase(t)
	dashPort, ingestPort := freePortPair(t)

	// The container posture's two secret files, minted exactly as the
	// documented quickstart's seed step produces them: the token store at
	// [dashboard].token_file and the env file at [secrets].environment_file.
	// Under the container paths both live inside the state volume; here both
	// live in the scratch state root, which is the same "inside the validated
	// state root" relationship the volume gives the container.
	tokenPath := filepath.Join(root, "dashboard.token")
	store, err := dashboard.LoadTokenStore(tokenPath, nil, nil)
	if err != nil {
		t.Fatalf("LoadTokenStore: %v", err)
	}
	_, plaintext, err := store.Mint("container-read@e2e", []types.Scope{types.ScopeRead}, time.Now())
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

	// The shipped file stays the config FILE; the overrides ride argv.
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
	ready := make(chan struct{}, 1)
	go func() {
		_, err := RunDaemon(ctx, BootOptions{
			Args:    args,
			Env:     []string{},
			Log:     nil,
			OnReady: func(booted *Daemon) { d = booted; ready <- struct{}{} },
		})
		done <- err
	}()

	if reached, err := awaitBootReady(t, "the shipped container config boot", bootReadyBase, ready, done); !reached {
		cancel()
		t.Fatalf("boot against the shipped container config %s did not reach serve: %v\n"+
			"the compose quickstart's posture (0.0.0.0 binds + proxy mode + mandate + public_origin + token files in the state root) must boot (TRBL-028)", path, err)
	}
	if d == nil {
		t.Fatalf("the shipped container boot signalled READY without handing over a daemon")
	}
	t.Cleanup(func() {
		cancel()
		awaitDrainBudget(t, "the shipped container config boot", bootDrainBase, done)
	})

	// The dashboard serves: an authenticated /health.json answers over the
	// socket the preflight held. (On the container's 0.0.0.0 bind the route is
	// not loopback-exempt, so the authenticated shape is the honest one.)
	base := fmt.Sprintf("http://127.0.0.1:%d", dashPort)
	req, err := http.NewRequest(http.MethodGet, base+"/health.json", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+plaintext)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /health.json on the shipped container boot: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated GET /health.json = %d %s, want 200", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"ledger_last_seq"`) {
		t.Fatalf("health body carries no ledger_last_seq: %s", body)
	}

	// The posture keys must come from the shipped FILE, not from a flag or a
	// default: removing either dashboard key from the file fails here before
	// any container run can.
	res, err := lifecycle.Resolve(nil, nil, path)
	if err != nil {
		t.Fatalf("re-resolve of %s: %v", path, err)
	}
	var mandate, origin string
	for _, cv := range res.Values {
		switch cv.Key {
		case "dashboard.mandate":
			if cv.Source != "file" {
				t.Errorf("dashboard.mandate resolved from %q, want source \"file\"", cv.Source)
			}
			mandate, _ = cv.Value.(string)
		case "dashboard.public_origin":
			if cv.Source != "file" {
				t.Errorf("dashboard.public_origin resolved from %q, want source \"file\"", cv.Source)
			}
			origin, _ = cv.Value.(string)
		}
	}
	if mandate != "proxy" || origin == "" {
		t.Fatalf("container posture drifted: mandate=%q public_origin=%q, want mandate=proxy and a non-empty public_origin from the shipped file", mandate, origin)
	}
}

// TestShippedContainerRefusalsStayMeasured is AC2's negative half: each of the
// row's four measured refusals is still enforced, proven by dropping (or
// breaking) exactly the shipped line that answers it and naming the refusal
// code that comes back. A future edit that removes a shipped key, loosens a
// shipped rule, or renames a code fails here — not in front of an operator.
func TestShippedContainerRefusalsStayMeasured(t *testing.T) {
	path := shippedContainerConfigPath(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	shipped := string(raw)

	type refusalCase struct {
		name string
		// dropLine must appear in the shipped file: the load-bearing line the
		// case targets.
		dropLine string
		// replacement, when non-empty, REPLACES dropLine's line instead of
		// removing it (used when the refusal needs the key present but wrong,
		// e.g. a state root pointed at a 0755 directory).
		replacement string
		boot        func(t *testing.T, cfgPath string) error
		wantInErr   []string
	}
	cases := []refusalCase{
		{
			// Trap 1: the state root must be 0700 (shipped as the image's
			// /data/state). The case re-points the shipped line at a 0755
			// scratch dir — the fresh-named-volume shape — and demands the
			// same refusal an operator's first boot produced.
			name:        "state root not 0700 (TROUBLE-LIFECYCLE-005)",
			dropLine:    `state_root = "/data/state"`,
			replacement: "__STATE_ROOT__", // substituted with the scratch dir below
			boot: func(t *testing.T, cfgPath string) error {
				res, err := lifecycle.Resolve(nil, nil, cfgPath)
				if err != nil {
					return err
				}
				_, serr := lifecycle.CheckStateRoot(res.Config)
				return serr
			},
			wantInErr: []string{"TROUBLE-LIFECYCLE-005", "mode is 0755", "want 0700"},
		},
		{
			// The SPEC-12 bind matrix: a public ingest bind without proxy
			// mode. The refusal fires inside PreflightBinds BEFORE any listen,
			// so the shipped 0.0.0.0:7643 bind can stay in the file untouched.
			name:     "public ingest bind without proxy mode (TROUBLE-LIFECYCLE-003)",
			dropLine: `nonloopback_mode = "proxy"`,
			boot: func(t *testing.T, cfgPath string) error {
				res, err := lifecycle.Resolve(nil, nil, cfgPath)
				if err != nil {
					return err
				}
				probes, perr := lifecycle.PreflightBinds(res.Config)
				if perr != nil {
					return perr
				}
				for _, p := range probes {
					_ = p.Listener.Close()
				}
				t.Fatalf("PreflightBinds accepted a public ingest bind without proxy mode: the TROUBLE-LIFECYCLE-003 refusal is gone")
				return nil
			},
			wantInErr: []string{"TROUBLE-LIFECYCLE-003", "requires proxy mode"},
		},
		{
			// SPEC-10 §2.4: a non-loopback dashboard bind requires a declared
			// trust mandate. Dropping the line leaves mandate empty on a
			// 0.0.0.0 bind — exactly the operator's first-boot refusal.
			name:     "non-loopback dashboard without a mandate (TROUBLE-DASHBOARD-006)",
			dropLine: `mandate = "proxy"`,
			boot: func(t *testing.T, cfgPath string) error {
				res, err := lifecycle.Resolve(nil, nil, cfgPath)
				if err != nil {
					return err
				}
				return dashboard.ValidateConfig(dashboardConfig(res.Config))
			},
			wantInErr: []string{"TROUBLE-DASHBOARD-006", "mandate_required"},
		},
		{
			// The second half of the same §2.4 gate: off loopback the origin
			// users actually reach must be declared (TROUBLE-LIFECYCLE-001).
			name:     "non-loopback dashboard without a public_origin (TROUBLE-LIFECYCLE-001)",
			dropLine: `public_origin = "https://trouble.example.net"`,
			boot: func(t *testing.T, cfgPath string) error {
				res, err := lifecycle.Resolve(nil, nil, cfgPath)
				if err != nil {
					return err
				}
				return dashboard.ValidateConfig(dashboardConfig(res.Config))
			},
			wantInErr: []string{"TROUBLE-LIFECYCLE-001", "public_origin_required"},
		},
		{
			// Trap 3: the documented no-go alternative to the state-volume
			// token file — a compose `secrets:` entry mounts 0444 root-owned.
			// The 0600 check that refuses it is what forces the placement, so
			// the case drives the shipped subsystem with the exact 0444 shape.
			name:     "compose-secrets token file mode 0444 (TROUBLE-LIFECYCLE-013)",
			dropLine: `token_file = "/data/state/dashboard.token"`,
			boot: func(t *testing.T, cfgPath string) error {
				_ = cfgPath
				secret := filepath.Join(t.TempDir(), "dashboard.token")
				if err := os.WriteFile(secret, []byte("{}\n"), 0o444); err != nil {
					t.Fatalf("write 0444 store: %v", err)
				}
				_, lerr := dashboard.LoadTokenStore(secret, nil, nil)
				return lerr
			},
			wantInErr: []string{"TROUBLE-LIFECYCLE-013", "token file mode not 0600"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(shipped, tc.dropLine) {
				t.Fatalf("the shipped container config no longer carries %q: the refusal this case pins has no shipped line to drop — update the case", tc.dropLine)
			}
			mutated := shipped
			if tc.replacement != "" {
				// The loose dir must live OUTSIDE the forbidden roots (/tmp,
				// /var/tmp), so it takes the same sanctioned base the package's
				// scratch state roots use — only the mode differs.
				home, err := os.UserHomeDir()
				if err != nil {
					t.Fatalf("home: %v", err)
				}
				base := filepath.Join(home, ".local", "state", "trouble-test")
				if err := os.MkdirAll(base, 0o700); err != nil {
					t.Fatalf("mkdir base: %v", err)
				}
				loose, err := os.MkdirTemp(base, "loose0755-")
				if err != nil {
					t.Fatalf("mkdir loose state root: %v", err)
				}
				if err := os.Chmod(loose, 0o755); err != nil {
					t.Fatalf("chmod loose state root: %v", err)
				}
				t.Cleanup(func() { os.RemoveAll(loose) })
				mutated = strings.Replace(mutated, tc.dropLine, `state_root = "`+loose+`"`, 1)
			} else {
				mutated = strings.Replace(shipped, tc.dropLine+"\n", "", 1)
			}
			cfg := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(cfg, []byte(mutated), 0o600); err != nil {
				t.Fatalf("write mutated config: %v", err)
			}
			err := tc.boot(t, cfg)
			if err == nil {
				t.Fatalf("the shipped posture minus %q passed clean: the refusal is no longer measured — a documented quickstart line went dead", tc.dropLine)
			}
			for _, want := range tc.wantInErr {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error = %v\nwant it to contain %q", err, want)
				}
			}
		})
	}

	// The matrix mutates copies, never the shipped bytes: the shipped file
	// itself still resolves.
	if _, err := lifecycle.Resolve(nil, nil, path); err != nil {
		t.Fatalf("the shipped container config must resolve: %v", err)
	}
}
