package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/dashboard"
	"github.com/trouble-agent/trouble/internal/lifecycle"
	"github.com/trouble-agent/trouble/internal/sentinel"
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

	// TRBL-062: the published port puts every reporter in a NON-loopback zone
	// — including one running on the host itself. The container's socket peer
	// is docker's bridge gateway (a private 172.x address => zone lan), which
	// is exactly what the operator's failing curl looked like from inside the
	// stack (measured: 401 `query-string key refused on a non-loopback
	// request`). The shipped project must therefore carry a secret_key, so the
	// X-Sentry-Auth form the quickstart documents authenticates through the
	// §3.7 bind matrix.
	//
	// The zone is what makes this probe non-vacuous, and it is NOT the daemon's
	// own socket shape. Over 127.0.0.1 the TCP peer is loopback, clientIP()
	// returns zone loopback (trustedProxies is empty here, so the XFF loop falls
	// back to the peer), the bind matrix is skipped, and a request with no
	// secret at all answers 200 — so a loopback-shaped probe would pass with or
	// without this row's fix. The real container posture is driven through the
	// live sentinel handler with the peer set to docker's bridge gateway and NO
	// X-Forwarded-For, matching a request that traverses the published port.
	//
	// The secret is read from the RESOLVED project set, not from the raw
	// resolved ConfigValue: `secret_key` is an alias the parser folds into
	// ProjectConfig.Secret (lifecycle config.go, the "secret", "secret_key"
	// case), and ProjectConfig.SecretKey — the struct field — is never
	// populated by any parse path. Asserting on it would fail on a correct
	// config, so the assertion below reads the same types.Project the
	// sentinel's bind matrix resolves its material from.
	projSet, err := res.Config.ProjectsSet()
	if err != nil {
		t.Fatalf("resolve the shipped container config's project set: %v", err)
	}
	if len(projSet) != 1 {
		t.Fatalf("the shipped container config declares %d projects, want 1", len(projSet))
	}
	proj := projSet[0]
	if proj.SecretKey == "" {
		t.Fatalf("the shipped container project carries no secret_key: on a published port EVERY reporter (host included) arrives off loopback and the bind matrix admits no auth form (TRBL-062)")
	}
	if d.Subsystems == nil || d.Subsystems.Sentinel == nil {
		t.Fatalf("the shipped container boot built no sentinel: the container's whole ingest surface is missing (TRBL-062)")
	}
	ingest := d.Subsystems.Sentinel.Handler()

	// The published-port first event: the shipped X-Sentry-Auth form, on the
	// docker-gateway peer the published port actually presents.
	first := httptest.NewRequest(http.MethodPost, "/api/1/event/", strings.NewReader(`{"message":"container quickstart first event (TRBL-062)","level":"error","release":"0.1.0"}`))
	first.RemoteAddr = dockerBridgePeer
	first.Header.Set("Content-Type", "application/json")
	first.Header.Set("X-Sentry-Auth", fmt.Sprintf("Sentry sentry_version=7, sentry_key=%s, sentry_secret=%s", proj.PublicKey, proj.SecretKey))
	rec := httptest.NewRecorder()
	ingest.ServeHTTP(rec, first)
	firstResp := rec.Result()
	firstBody, _ := io.ReadAll(firstResp.Body)
	firstResp.Body.Close()
	if firstResp.StatusCode != http.StatusOK {
		t.Fatalf("published-port first event (X-Sentry-Auth) = %d %s, want 200 — a reporter on this host must authenticate with the shipped form (TRBL-062)", firstResp.StatusCode, firstBody)
	}
	if !strings.Contains(string(firstBody), `"id"`) {
		t.Fatalf("published-port first event answered without an event id: %s", firstBody)
	}

	// And the bare query form stays refused on the SAME zone shape: same
	// project, same peer, no secret — exactly the operator's 401 before this
	// row. This is the assertion that makes the 200 above load-bearing: strip
	// the shipped secret and this failure is what a reporter gets instead.
	bare := httptest.NewRequest(http.MethodPost, "/api/1/event/?sentry_key="+proj.PublicKey, strings.NewReader(`{"message":"trbl-062 bare form","level":"error"}`))
	bare.RemoteAddr = dockerBridgePeer
	bare.Header.Set("Content-Type", "application/json")
	bareRec := httptest.NewRecorder()
	ingest.ServeHTTP(bareRec, bare)
	bareResp := bareRec.Result()
	bareBody, _ := io.ReadAll(bareResp.Body)
	bareResp.Body.Close()
	if bareResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bare ?sentry_key= on a published-port zone = %d %s, want 401: the loopback exception must NOT apply to the container posture (TRBL-062)", bareResp.StatusCode, bareBody)
	}
	if !strings.Contains(string(bareBody), "query_key_remote") {
		t.Fatalf("bare ?sentry_key= on a published-port zone answered 401 without the measured cause: %s (X-Sentry-Error: %s)", bareBody, bareResp.Header.Get("X-Sentry-Error"))
	}

	// Non-vacuity: the 200 above must be the SECRET doing the work, not the
	// zone. Same wire, same peer, header form, one bit flipped in the secret —
	// the bind matrix must refuse it. Without this the first-event assertion
	// could pass on a posture that admits any header at all.
	wrongSecret := "0" + proj.SecretKey[1:]
	if wrongSecret == proj.SecretKey {
		wrongSecret = "1" + proj.SecretKey[1:]
	}
	wrong := httptest.NewRequest(http.MethodPost, "/api/1/event/", strings.NewReader(`{"message":"trbl-062 wrong secret","level":"error"}`))
	wrong.RemoteAddr = dockerBridgePeer
	wrong.Header.Set("Content-Type", "application/json")
	wrong.Header.Set("X-Sentry-Auth", fmt.Sprintf("Sentry sentry_version=7, sentry_key=%s, sentry_secret=%s", proj.PublicKey, wrongSecret))
	wrongRec := httptest.NewRecorder()
	ingest.ServeHTTP(wrongRec, wrong)
	wrongResp := wrongRec.Result()
	wrongBody, _ := io.ReadAll(wrongResp.Body)
	wrongResp.Body.Close()
	if wrongResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("X-Sentry-Auth with a WRONG secret on a published-port zone = %d %s, want 401: the 200 first-event proof would be vacuous (TRBL-062)", wrongResp.StatusCode, wrongBody)
	}
}

// dockerBridgePeer is the TCP peer a request presents when it arrives over a
// docker published port: the bridge gateway, a private address, i.e. the LAN
// zone of the §3.7 bind matrix (and never a loopback peer, which is the one
// zone the matrix exempts). Fixed on purpose: the assertion it feeds is about
// the shipped posture, not about this host's docker network numbering.
const dockerBridgePeer = "172.18.0.1:41234"

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
		{
			// TRBL-062: the published port admits NO bare-public-key form —
			// a shipped project without a secret leaves the quickstart's
			// reporter with no accepted auth form at all. Dropping the
			// shipped secret_key line must return the measured 401 shape
			// (`query_key_remote`) through the live resolution path.
			name:     "published-port project without a secret (TRBL-062, TROUBLE-SENTINEL-006 query_key_remote)",
			dropLine: `secret_key = "fedcba9876543210fedcba9876543210"`,
			boot: func(t *testing.T, cfgPath string) error {
				res, err := lifecycle.Resolve(nil, nil, cfgPath)
				if err != nil {
					return err
				}
				projects, err := res.Config.ProjectsSet()
				if err != nil {
					return err
				}
				if len(projects) == 0 {
					t.Fatalf("the mutated config declares no projects; the TRBL-062 case needs the shipped project minus its secret")
				}
				// The same field mapping buildSentinel performs (the daemon's
				// composition of the sentinel config); the scrubber is nil on
				// purpose — this probe must refuse at AUTH, before any scrub.
				scfg := sentinel.Config{
					Bind:           "127.0.0.1:0",
					AdvertisedHost: res.Config.Ingest.AdvertisedHost,
					Scheme:         "http",
					SpoolDir:       t.TempDir(),
					HostID:         "container-posture-test",
					Actor:          lifecycle.Actor(types.ActorDaemon, "troubled"),
					Projects:       projects,
					ProxyTrust:     "loopback",
					RequireSecret:  !res.Config.Ingest.Auth.LoopbackDSN,
					LedgerWait:     types.Duration("2s"),
				}
				s, serr := sentinel.NewServer(scfg, stubSink{}, nil)
				if serr != nil {
					return serr
				}
				// The bare query form on the published-port wire: the TCP peer
				// is the docker bridge itself (a non-loopback container IP,
				// which adds no X-Forwarded-For) — the recorded request keeps
				// that peer, so the zone resolves to lan exactly as it does
				// behind docker's published port.
				req2 := httptest.NewRequest(http.MethodPost, "/api/1/event/", strings.NewReader(`{"message":"trbl-062 bare form","level":"error"}`))
				req2.Host = "trouble.example.net"
				req2.RemoteAddr = "172.18.0.7:41234"
				q := req2.URL.Query()
				q.Set("sentry_key", projects[0].PublicKey)
				req2.URL.RawQuery = q.Encode()
				rec := httptest.NewRecorder()
				s.Handler().ServeHTTP(rec, req2)
				res2 := rec.Result()
				b, _ := io.ReadAll(res2.Body)
				res2.Body.Close()
				return fmt.Errorf("wire answer %d %s (X-Sentry-Error: %s)", res2.StatusCode, b, res2.Header.Get("X-Sentry-Error"))
			},
			wantInErr: []string{"TROUBLE-SENTINEL-006", "query-string key refused on a non-loopback request"},
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

// stubSink satisfies sentinel's ledgerSink for auth-path probes that must
// never reach the ledger; an append here is a bug the probe wants loud.
type stubSink struct{}

func (stubSink) Append(ctx context.Context, d types.RecordDraft) (types.Record, error) {
	return types.Record{}, fmt.Errorf("stubSink: the TRBL-062 auth probe must not append")
}

func (stubSink) LastSeq() uint64 { return 0 }
