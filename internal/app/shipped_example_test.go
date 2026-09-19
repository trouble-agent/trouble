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
	"strings"
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

// stampBuildForTest supplies the link-time version triple (SPEC-12 §3.4) that a
// `go test` binary does not carry. /health.json degrades every build whose
// GitSHA is empty or "unknown" (`detail.reason=unstamped_build`,
// internal/lifecycle/health.go), so without this the status assertion in
// TestShippedExampleConfigBootsToServe could only ever prove "degraded for a
// reason that is not a subsystem". The triple `make bin` passes through ldflags
// is supplied here for the duration of the test instead of asserting a weaker
// status.
func stampBuildForTest(t *testing.T) {
	t.Helper()
	v, sha, bt := lifecycle.Version, lifecycle.GitSHA, lifecycle.BuildTime
	t.Cleanup(func() { lifecycle.Version, lifecycle.GitSHA, lifecycle.BuildTime = v, sha, bt })
	lifecycle.Version, lifecycle.GitSHA, lifecycle.BuildTime =
		"0.1.0", "767e537", "2026-09-18T00:00:00.000Z"
}

// assertShippedSubsystemTable requires the shipped example's own bytes to carry
// the LIVE declaration of one optional subsystem: the named block, the table
// `[<name>]` as a real key (not a commented-out sketch), `enabled = false` as
// its first setting, and — in that same block — every key an operator must set
// to turn the subsystem on.
//
// The table being LIVE is the point (TRBL-020): SPEC-12 §3.1b registers these
// two tables in the resolved config schema, so the posture the file states is the
// posture the daemon reads. Deleting the table from examples/config.toml fails
// this test; the posture is never implied by an absent table, and never left as
// a comment that no boot can see.
func assertShippedSubsystemTable(t *testing.T, name string, wantKeys ...string) {
	t.Helper()
	path := shippedExampleConfigPath(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	text := string(raw)

	anchor := "# --- [" + name + "] "
	marker := "\n[" + name + "]\n"
	ai, mi := strings.Index(text, anchor), strings.Index(text, marker)
	if ai < 0 {
		t.Fatalf("%s carries no `%s` block: the opt-out of SPEC-09 §3.4a / SPEC-11 §2a must be visible in the file operators copy", path, anchor)
	}
	if mi < 0 || mi < ai {
		t.Fatalf("%s carries no live `[%s]` table: the shipped default must be a key the daemon reads (SPEC-12 §3.1b), not a commented sketch or an absent table", path, name)
	}
	after := text[mi+len(marker):]
	if !strings.HasPrefix(after, "enabled = false") {
		t.Fatalf("%s: `[%s]` is not followed by `enabled = false` (found %q): the shipped posture is OFF (SPEC-09 §3.4a / SPEC-11 §2a)", path, name, firstLineOf(after))
	}
	block := text[ai:mi]
	for _, k := range wantKeys {
		if !strings.Contains(block, k) {
			t.Errorf("%s: the [%s] block does not name %q as part of the opt-in; a fresh operator must be told what turns it on (SPEC-09 §3.4a / SPEC-11 §2a)", path, name, k)
		}
	}
}

func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// repositoryRootFromThisTest resolves repository-owned documentation from this
// source file rather than the test process CWD. The quickstart is operator
// documentation, but its commands are part of the shipped interface and must
// not drift from the config and listener contract that this package boots.
func repositoryRootFromThisTest(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate repository documentation")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func readRepositoryDocument(t *testing.T, root, rel string) string {
	t.Helper()
	path := filepath.Join(root, rel)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

// TestCanonicalDeployQuickstartContract is the documentation regression gate
// for TRBL-008. It checks the single copy-paste foreground path against the
// shipped config and the actual listener/auth contract without running shell
// commands or depending on a network.
func TestCanonicalDeployQuickstartContract(t *testing.T) {
	root := repositoryRootFromThisTest(t)
	deploy := readRepositoryDocument(t, root, filepath.Join("deploy", "README.md"))
	readme := readRepositoryDocument(t, root, "README.md")

	if strings.Count(deploy, "## First foreground run (canonical quickstart)") != 1 {
		t.Errorf("deploy/README.md must contain exactly one canonical quickstart heading")
	}
	if !strings.Contains(readme, "deploy/README.md#first-foreground-run-canonical-quickstart") {
		t.Error("README.md must point fresh operators to deploy/README.md's canonical quickstart")
	}

	for _, want := range []string{
		"Go 1.26 or newer",
		"examples/config.toml",
		"examples/trouble.env",
		"chmod 0600 \"$CONFIG_DIR/config.toml\" \"$CONFIG_DIR/trouble.env\"",
		"chmod 0700 \"$STATE_ROOT\"",
		"state_root = \"/home/you/.local/state/trouble\"",
		"config_path = \"/home/you/.config/trouble/config.toml\"",
		"environment_file = \"/home/you/.config/trouble/trouble.env\"",
		"advertised_host = \"trouble.your-domain.example\"",
		"public_key = \"replace-with-a-new-32-lowercase-hex-character-key\"",
		"ingest.bind = \"127.0.0.1:7643\"",
		"dashboard.bind = \"127.0.0.1:7644\"",
		"make bin",
		"bin/troubled --config \"$CONFIG_DIR/config.toml\"",
		"http://127.0.0.1:7644/health.json",
		"http://127.0.0.1:7643/api/1/event/?sentry_key=$PUBLIC_KEY",
		"Content-Type: application/json",
		"\"$STATE_ROOT\"/ledger/*.jsonl",
		"Ctrl-C",
		"SIGINT",
		"SIGTERM",
	} {
		if !strings.Contains(deploy, want) {
			t.Errorf("canonical quickstart is missing %q", want)
		}
	}

	for _, stale := range []string{
		"bin/trouble install",
		"cp examples/config.toml ~/.config/trouble/config.toml",
		"bin/troubled --config ~/.config/trouble/config.toml",
	} {
		if strings.Contains(deploy, stale) || strings.Contains(readme, stale) {
			t.Errorf("README/deploy quickstart retains stale first-run command %q", stale)
		}
	}
}

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
	stampBuildForTest(t)
	path := shippedExampleConfigPath(t)

	root := stateBase(t)
	dashPort, ingestPort := freePortPair(t)

	// The rules dir the sensors watch unconditionally lives beside the state root
	// (SPEC-03 §3.7: `<state_root>/../config/rules.d`). It is created here — empty,
	// which the §3.5 validator reports as "no rule files found; the shipped
	// defaults are active" — so this boot exercises the INSTALLED state beside its
	// throwaway state root, deterministically and independently of whatever a
	// previous run left in the shared parent directory. An ABSENT rules dir is
	// equally non-failing (SPEC-03 §3.7b, pinned by
	// TestMissingRulesDirBootsToHealthOK): before TRBL-022 the inotify sensor
	// degraded this whole boot with TROUBLE-SENSORS-025 for the absence, which is
	// why the directory used to be created for a reason that had nothing to do
	// with the subsystems under test.
	rulesDir := filepath.Join(filepath.Dir(root), "config", "rules.d")
	if err := os.MkdirAll(rulesDir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", rulesDir, err)
	}

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

	if reached, err := awaitBootReady(t, "the shipped-example boot", bootReadyBase, ready, done); !reached {
		cancel()
		t.Fatalf("boot against the shipped example %s did not reach serve: %v\n"+
			"`cp examples/config.toml <workdir>/config.toml && bin/troubled --config config.toml` must boot (TRBL-005)", path, err)
	}
	if d == nil {
		t.Fatalf("the shipped-example boot signalled READY without handing over a daemon")
	}
	t.Cleanup(func() {
		cancel()
		awaitDrainBudget(t, "the shipped-example boot", bootDrainBase, done)
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

	// --- TRBL-007: the shipped example declares a project (SPEC-12 §3.1a), so
	// the ingest plane is BUILT and the README's on-ramp is reachable. Before
	// this the example's boot had no listener on ingest.bind at all: the
	// headline `POST /api/{id}/event/` was connection-refused.
	if d.Subsystems == nil || d.Subsystems.Sentinel == nil {
		t.Fatalf("the shipped example did not build the sentinel: subsystems=%+v", d.Subsystems)
	}
	if len(d.Cfg.Projects) != 1 {
		t.Fatalf("the shipped example declares %d projects, want 1", len(d.Cfg.Projects))
	}
	projectKey := d.Cfg.Projects[0].PublicKey

	ingestBase := fmt.Sprintf("http://127.0.0.1:%d", ingestPort)
	// The DSN reachability probe: the listener is bound and resolves the project.
	probe, err := http.Get(ingestBase + "/api/1/")
	if err != nil {
		t.Fatalf("GET the ingest probe on the shipped-example boot: %v\n"+
			"CP: the shipped example must bind ingest.bind (TRBL-007 AC2)", err)
	}
	probeBody, _ := io.ReadAll(probe.Body)
	probe.Body.Close()
	if probe.StatusCode != http.StatusOK || !strings.Contains(string(probeBody), `"id":"1"`) {
		t.Fatalf("GET /api/1/ = %d %s, want 200 and the declared project", probe.StatusCode, probeBody)
	}

	// The documented on-ramp for curl: SPEC-04 §2.4's generic_json_query form. A
	// loopback request authenticates with the DSN public key alone while
	// ingest.auth.loopback_dsn = true (SPEC-12 §3.1), so no secret is needed.
	evBody := `{"message":"shipped-example on-ramp","level":"error","release":"0.1.0"}`
	post, err := http.Post(ingestBase+"/api/1/event/?sentry_key="+projectKey,
		"application/json", strings.NewReader(evBody))
	if err != nil {
		t.Fatalf("POST the documented on-ramp form: %v", err)
	}
	postBody, _ := io.ReadAll(post.Body)
	post.Body.Close()
	if post.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/1/event/ = %d %s, want 200 (auth form: ?sentry_key=<public_key>)",
			post.StatusCode, postBody)
	}
	if !strings.Contains(string(postBody), `"id"`) {
		t.Errorf("POST /api/1/event/ answered without an event id: %s", postBody)
	}

	// AC3's ledger half: the POST's event record AND its fingerprint group
	// record. The sensors write `event` records of their own on a boot, so the
	// assertion keys on the event id the on-ramp returned and ties the group to
	// that event's digest instead of counting the kind.
	var postedID string
	if err := json.Unmarshal(postBody, &struct{ ID *string }{&postedID}); err != nil || postedID == "" {
		t.Fatalf("POST response carried no event id: %v (%s)", err, postBody)
	}
	var onRampEvents, groups int
	var onRampDigest string
	var groupOps []string
	var groupDigests []string
	if err := d.Ledger.Query().ScanFrom(1, func(rec types.Record) bool {
		switch rec.Kind {
		case types.KEvent:
			if id, _ := rec.Payload["native_id"].(string); id == postedID {
				onRampEvents++
				onRampDigest, _ = rec.Payload["digest"].(string)
			}
		case types.KGroup:
			groups++
			if op, _ := rec.Payload["op"].(string); op != "" {
				groupOps = append(groupOps, op)
			}
			if dg, _ := rec.Payload["digest"].(string); dg != "" {
				groupDigests = append(groupDigests, dg)
			}
		}
		return true
	}); err != nil {
		t.Fatalf("ledger scan: %v", err)
	}
	if onRampEvents != 1 {
		t.Errorf("%d event records for the posted event id %s, want exactly 1 (AC3)", onRampEvents, postedID)
	}
	if groups != 1 || len(groupOps) != 1 || groupOps[0] != "create" {
		t.Errorf("group records = %d ops=%v, want exactly 1 with op=create (AC3)", groups, groupOps)
	}
	if onRampDigest == "" || len(groupDigests) != 1 || groupDigests[0] != onRampDigest {
		t.Errorf("the group record does not belong to the posted event: event digest %q, group digests %v",
			onRampDigest, groupDigests)
	}

	// --- TRBL-016 AC2/AC3: the shipped example ships the two optional
	// subsystems OFF, deliberately and visibly (SPEC-09 §3.4a, SPEC-11 §2a), so
	// this boot is COMPLETE rather than permanently degraded: every subsystem row
	// is built, none is refused, and /health.json reads ok. Before the change the
	// two shipped defaults failed their own validators, so the shipped example —
	// the file docs/cmd.md and deploy/README.md tell a new operator to copy —
	// booted permanently missing the issue desk and the skill loop.
	resp2, err := http.Get(base + "/health.json")
	if err != nil {
		t.Fatalf("GET /health.json after the on-ramp: %v", err)
	}
	healthBody, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	var after types.HealthResponse
	if err := json.Unmarshal(healthBody, &after); err != nil {
		t.Fatalf("health is not a HealthResponse: %v (%s)", err, healthBody)
	}
	if len(after.Subsystems) != len(subsystemNames) {
		t.Errorf("the health block carries %d subsystem rows, want %d: %+v", len(after.Subsystems), len(subsystemNames), after.Subsystems)
	}
	for _, name := range subsystemNames {
		row := rowOf(t, after, name)
		if row.Refused {
			t.Errorf("%s row = %+v, want not refused: the shipped example declares the deliberate opt-out instead of asking for a subsystem it cannot build", name, row)
		}
		if !row.Built {
			t.Errorf("%s row = %+v, want built=true on the shipped-example boot", name, row)
		}
	}
	// AC3's status half, with nothing left to explain the degraded branch away:
	// no row refused/unbuilt, no subsystem key in detail, and no sensor degraded
	// (a degraded sensor would make the status assertion fail for a reason this
	// test is not about, so the sensor is named instead of the status being
	// loosened).
	for _, k := range []string{"subsystem_refused", "subsystem_unbuilt"} {
		if v, ok := after.Detail[k]; ok {
			t.Errorf("detail.%s = %v on a boot whose every subsystem row is built", k, v)
		}
	}
	for _, s := range after.Sensors {
		if s.Degraded {
			t.Errorf("sensor %s is degraded on the shipped-example boot (%s): the AC3 ok assertion below would fail for a sensor reason, not a subsystem one", s.Sensor, s.Reason)
		}
	}
	if after.Status != "ok" {
		t.Errorf("status = %q on the shipped-example boot, want ok (every subsystem built, stamped build, healthy sensors): %s", after.Status, healthBody)
	}

	// AC2's file half: the opt-out is DECLARED in the shipped bytes as LIVE keys
	// — the file an operator copies states the posture in a table the daemon
	// reads (SPEC-12 §3.1b) and names what turns each subsystem on — so removing
	// it, or commenting it out again, breaks this test.
	assertShippedSubsystemTable(t, "issues", "owner", "repo", "duckbrain")
	assertShippedSubsystemTable(t, "skills", "source_path", "source_url")
}
