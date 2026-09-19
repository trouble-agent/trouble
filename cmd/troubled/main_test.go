// main_test.go pins the daemon's argv entry point (SPEC-12 §2.5, TRBL-018).
//
// The defect this file exists for: cmd/troubled parsed argv with a
// flag.NewFlagSet carrying only --config/-v/--version, so a documented key flag
// (`--state_root <dir>`) died inside the flag package with
// "flag provided but not defined" and exit 2 before app.RunDaemon ever saw argv
// — while lifecycle.Resolve implemented the whole per-key surface and the
// daemon already handed it argv. The surface is now split here (splitDaemonArgs)
// and the flags reach the resolver, which adjudicates them on its own whitelist.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/totalwindupflightsystems/trouble/internal/lifecycle"
	"github.com/totalwindupflightsystems/trouble/internal/scrub"
	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// childEnvName selects a child mode of this test binary (see TestMain). It is how
// a test gets a REAL /proc/self/cmdline to point the argv path at: the child is
// this same compiled binary, re-exec'd with argv exactly as the test wrote it.
const childEnvName = "TROUBLED_TEST_CHILD_MODE"

// The keys that cannot ride the daemon's argv, and why (SPEC-12 §2.5a). Both lists
// are asserted EXACTLY by TestDaemonArgvAddressesEveryRegisteredKey: a new key that
// cannot be addressed from argv must be named here and in the spec, never skipped.
var (
	// Tables: a flag value is a scalar and these four keys are declarations.
	wantTableRefused = []string{"issues", "llm", "projects", "skills"}
	// Refused by the argv secret scan (SPEC-12 §3.2): the flag NAME matches the
	// mandatory cli_flag_secret rule, so the value that follows it — a path, in
	// practice a token store path, or an environment-variable NAME in
	// server.redis.password_env — is never accepted on argv. Set from file/env.
	wantArgvScanRefused = []string{"dashboard.token_file", "hub.token", "server.redis.password_env"}
)

func TestMain(m *testing.M) {
	switch os.Getenv(childEnvName) {
	case "run":
		// The daemon's own entry point, with the child's argv.
		os.Exit(run(os.Args[1:]))
	case "scan":
		// Step 3 of the boot sequence on its own (SPEC-12 §4.1): the argv secret
		// scan over this process's real cmdline.
		if err := lifecycle.ScanProcCmdline(); err != nil {
			fmt.Fprintf(os.Stderr, "argv scan refused: %v\n", err)
			os.Exit(13)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runChild re-execs this test binary in a child mode with argv exactly as given,
// and reports the exit status plus stderr.
func runChild(t *testing.T, mode string, argv ...string) (int, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cmd := exec.Command(exe, argv...)
	env := make([]string, 0, len(os.Environ())+1)
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, childEnvName+"=") {
			env = append(env, e)
		}
	}
	cmd.Env = append(env, childEnvName+"="+mode)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = io.Discard
	err = cmd.Run()
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("child (%s %v): %v", mode, argv, err)
		}
	}
	return cmd.ProcessState.ExitCode(), stderr.String()
}

// argvStateRoot makes a state root the boot gate accepts: under ~/.local/state (a
// t.TempDir() would be refused 004 for being under /tmp) and 0700.
func argvStateRoot(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home: %v", err)
	}
	base := filepath.Join(home, ".local", "state", "trouble")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", base, err)
	}
	dir, err := os.MkdirTemp(base, "argv-")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func absentConfig(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "absent.toml")
}

func rowFor(t *testing.T, rows []types.ConfigValue, key string) types.ConfigValue {
	t.Helper()
	for _, r := range rows {
		if r.Key == key {
			return r
		}
	}
	t.Fatalf("no resolved row for key %q", key)
	return types.ConfigValue{}
}

// probeValue returns a value of the same shape as def, so a type-checking key
// (int, duration, bool, string list, zone-window map) is exercised with a value it
// can actually accept. The value itself is irrelevant: the assertion is about the
// PROVENANCE of the row, not about its content.
func probeValue(def any) string {
	switch def.(type) {
	case bool:
		return "true"
	case int, int64:
		return "1"
	case float64:
		return "20.5"
	case []string:
		return "probe"
	case types.Duration:
		return "1m"
	case map[string]types.Duration:
		return "loopback=10m"
	}
	return "probe"
}

// --- the split (SPEC-12 §2.5) ------------------------------------------------

func TestSplitDaemonArgsOwnFlags(t *testing.T) {
	tests := []struct {
		name     string
		argv     []string
		want     daemonArgs
		wantKeys []string
	}{
		{name: "no arguments", argv: nil, want: daemonArgs{}},
		{name: "-v", argv: []string{"-v"}, want: daemonArgs{verbose: true}},
		{name: "--v", argv: []string{"--v"}, want: daemonArgs{verbose: true}},
		{name: "--v=false", argv: []string{"--v=false"}, want: daemonArgs{}},
		{name: "-v=1", argv: []string{"-v=1"}, want: daemonArgs{verbose: true}},
		{name: "--version", argv: []string{"--version"}, want: daemonArgs{version: true}},
		{name: "-version", argv: []string{"-version"}, want: daemonArgs{version: true}},
		{name: "-h", argv: []string{"-h"}, want: daemonArgs{help: true}},
		{name: "--help", argv: []string{"--help"}, want: daemonArgs{help: true}},
		{name: "-config <path>", argv: []string{"-config", "/x/config.toml"}, want: daemonArgs{config: "/x/config.toml"}},
		{name: "--config <path>", argv: []string{"--config", "/x/config.toml"}, want: daemonArgs{config: "/x/config.toml"}},
		{name: "--config=<path>", argv: []string{"--config=/x/config.toml"}, want: daemonArgs{config: "/x/config.toml"}},
		{
			// The mechanical spelling of the config_path KEY is not the daemon's
			// own flag: it must be forwarded so the resolver records it.
			name: "key flags are not the daemon's own surface",
			argv: []string{"--config_path", "/x/config.toml"},
			want: daemonArgs{},
			// wantKeys nil → asserted below from argv itself
		},
		{
			name: "mixed",
			argv: []string{"--v", "--state_root", "/x", "--config", "/y.toml", "--dashboard-bind", "127.0.0.1:7645"},
			want: daemonArgs{verbose: true, config: "/y.toml"},
		},
		{
			name: "key flags and positionals are forwarded verbatim",
			argv: []string{"--state_root=/x", "--ingest-bind", "127.0.0.1:7643", "positional"},
			want: daemonArgs{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			own, keys, err := splitDaemonArgs(tc.argv)
			if err != nil {
				t.Fatalf("splitDaemonArgs(%q): %v", tc.argv, err)
			}
			if own != tc.want {
				t.Errorf("own = %+v, want %+v", own, tc.want)
			}
			want := tc.wantKeys
			if want == nil {
				// Everything that is not one of the daemon's own flags is
				// forwarded: compute the expectation by removing own tokens.
				want = expectedKeys(tc.argv)
			}
			if !reflect.DeepEqual(keys, want) {
				t.Errorf("passthrough = %q, want %q", keys, want)
			}
		})
	}
}

// expectedKeys is the passthrough expectation for the table above: it drops the
// tokens the daemon owns (and the value token a `--config`/`-config` consumed).
func expectedKeys(argv []string) []string {
	out := []string{}
	for i := 0; i < len(argv); i++ {
		name, _, hasValue, mine := ownFlag(argv[i])
		if !mine {
			out = append(out, argv[i])
			continue
		}
		if name == ownConfig && !hasValue {
			i++
		}
	}
	return out
}

func TestSplitDaemonArgsRefusals(t *testing.T) {
	tests := []struct {
		name string
		argv []string
		want string
	}{
		{name: "one-dash token is refused by name", argv: []string{"-state_root", "/x"}, want: `unknown flag "-state_root"`},
		{name: "one-dash short token is refused by name", argv: []string{"-x"}, want: `unknown flag "-x"`},
		{name: "config without a value", argv: []string{"--config"}, want: "flag needs an argument: --config"},
		{name: "bad boolean", argv: []string{"-v=maybe"}, want: `invalid boolean value "maybe"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := splitDaemonArgs(tc.argv)
			if err == nil {
				t.Fatalf("splitDaemonArgs(%q) accepted the argument; want an error naming it", tc.argv)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name %q", err, tc.want)
			}
		})
	}
}

// --- AC1: the documented key flags reach the resolver ------------------------

// TestDaemonArgvAddressesEveryRegisteredKey is the surface-inventory pin: it
// enumerates the keys from the CODE (a default resolve, one row per registered
// key), derives each key's flag spelling, and drives the daemon's own split
// function with `--<spelling> <value>` — so the test fails if the surface shrinks
// or a spelling stops working. Exactly two named classes may not be addressable
// from argv, and both are asserted by name and by reason.
func TestDaemonArgvAddressesEveryRegisteredKey(t *testing.T) {
	res, err := lifecycle.Resolve(nil, nil, absentConfig(t))
	if err != nil {
		t.Fatalf("default resolve: %v", err)
	}
	if len(res.Values) == 0 {
		t.Fatal("the default resolve produced no rows: the registry enumeration is broken")
	}

	var (
		addressable    []string
		resolveRefused []string // key: reason, as the resolver reported it
		scanRefused    []string
	)
	for _, v := range res.Values {
		spelling := "--" + strings.ReplaceAll(v.Key, ".", "-")
		value := probeValue(v.Value)

		own, keys, err := splitDaemonArgs([]string{spelling, value})
		if err != nil {
			t.Fatalf("%s: splitDaemonArgs refused a key flag: %v", spelling, err)
		}
		if own != (daemonArgs{}) {
			t.Errorf("%s: the daemon's own surface claimed a key flag: %+v", spelling, own)
		}
		if !reflect.DeepEqual(keys, []string{spelling, value}) {
			t.Fatalf("%s: the key flag was not forwarded verbatim: %q", spelling, keys)
		}

		// The argv secret scan is a separate gate (boot step 3): a key whose flag
		// name trips a mandatory rule can never be set from argv, whatever the
		// resolver would do with the value.
		if err := scrub.MandatoryScan(context.Background(), []byte(strings.Join(keys, "\n"))); err != nil {
			scanRefused = append(scanRefused, v.Key)
			continue
		}

		got, err := lifecycle.Resolve(keys, nil, absentConfig(t))
		if err != nil {
			resolveRefused = append(resolveRefused, fmt.Sprintf("%s: %v", v.Key, err))
			continue
		}
		row := rowFor(t, got.Values, v.Key)
		if row.Source != "flag" {
			t.Errorf("%s: source = %q, want \"flag\" (the flag did not win)", v.Key, row.Source)
		}
		if row.SourceRef != spelling {
			t.Errorf("%s: source_ref = %q, want %q", v.Key, row.SourceRef, spelling)
		}
		addressable = append(addressable, v.Key)
	}

	sort.Strings(addressable)
	if got, want := namedKeys(resolveRefused), wantTableRefused; !reflect.DeepEqual(got, want) {
		t.Errorf("keys refused because a flag value is a scalar (tables) = %v, want %v\nreasons:\n  %s",
			got, want, strings.Join(resolveRefused, "\n  "))
	}
	if got, want := scanRefused, wantArgvScanRefused; !reflect.DeepEqual(sorted(got), want) {
		t.Errorf("keys refused by the argv secret scan = %v, want %v", got, want)
	}
	if want := len(res.Values) - len(wantTableRefused) - len(wantArgvScanRefused); len(addressable) != want {
		t.Errorf("addressable keys = %d, want %d (of %d registered)", len(addressable), want, len(res.Values))
	}
	t.Logf("%d registered keys: %d addressable from argv, %d refused as tables, %d refused by the argv scan",
		len(res.Values), len(addressable), len(wantTableRefused), len(wantArgvScanRefused))
}

func namedKeys(failures []string) []string {
	out := make([]string, 0, len(failures))
	for _, f := range failures {
		key, _, _ := strings.Cut(f, ":")
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// TestConfigFlagBeatsConfigFile is AC1's precedence half: the SAME key set by the
// file and by a flag resolves to the flag's value, names the flag as the source,
// and records the losing file value as a conflict (TROUBLE-LIFECYCLE-002).
func TestConfigFlagBeatsConfigFile(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfgPath, []byte("ingest.bind = \"127.0.0.1:7001\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	own, keys, err := splitDaemonArgs([]string{"--config", cfgPath, "--ingest-bind", "127.0.0.1:7002"})
	if err != nil {
		t.Fatalf("splitDaemonArgs: %v", err)
	}
	if own.config != cfgPath {
		t.Fatalf("own.config = %q, want %q", own.config, cfgPath)
	}
	res, err := lifecycle.Resolve(keys, nil, cfgPath)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	row := rowFor(t, res.Values, "ingest.bind")
	if row.Value != "127.0.0.1:7002" {
		t.Errorf("ingest.bind = %v, want the flag value 127.0.0.1:7002", row.Value)
	}
	if row.Source != "flag" {
		t.Errorf("ingest.bind source = %q, want \"flag\"", row.Source)
	}
	if row.SourceRef != "--ingest-bind" {
		t.Errorf("ingest.bind source_ref = %q, want \"--ingest-bind\"", row.SourceRef)
	}
	if res.Config.Ingest.Bind != "127.0.0.1:7002" {
		t.Errorf("resolved Config.Ingest.Bind = %q, want the flag value", res.Config.Ingest.Bind)
	}

	// The losing lower-precedence sources are recorded: one conflict row per
	// source that disagrees with the winner (TROUBLE-LIFECYCLE-002), including the
	// FILE that set this key to 127.0.0.1:7001.
	var refs []string
	for _, c := range res.Conflicts {
		if c.Key != "ingest.bind" {
			continue
		}
		refs = append(refs, c.SourceRef)
		if c.Source != "conflict" {
			t.Errorf("conflict source = %q, want \"conflict\"", c.Source)
		}
		if c.Value != "TROUBLE-LIFECYCLE-002" {
			t.Errorf("conflict value = %v, want the code TROUBLE-LIFECYCLE-002", c.Value)
		}
		if !strings.Contains(c.SourceRef, "--ingest-bind") {
			t.Errorf("conflict source_ref = %q, want the winning source named", c.SourceRef)
		}
	}
	if len(refs) == 0 {
		t.Fatalf("no conflict recorded for ingest.bind (file 127.0.0.1:7001 vs flag 127.0.0.1:7002): %+v", res.Conflicts)
	}
	var fileRow bool
	for _, r := range refs {
		if strings.Contains(r, cfgPath) {
			fileRow = true
		}
	}
	if !fileRow {
		t.Errorf("no conflict row names the losing config file %s: %q", cfgPath, refs)
	}
}

// --- B3: an unknown key is refused by name, never ignored --------------------

func TestUnknownKeyFlagRefusedByName(t *testing.T) {
	own, keys, err := splitDaemonArgs([]string{"--no-such-key", "1"})
	if err != nil {
		t.Fatalf("splitDaemonArgs refused an unknown key itself: %v", err)
	}
	if own != (daemonArgs{}) {
		t.Fatalf("an unknown key was treated as one of the daemon's own flags: %+v", own)
	}
	if !reflect.DeepEqual(keys, []string{"--no-such-key", "1"}) {
		t.Fatalf("passthrough = %q, want the argument forwarded to the resolver", keys)
	}

	_, err = lifecycle.Resolve(keys, nil, absentConfig(t))
	if err == nil {
		t.Fatal("Resolve accepted --no-such-key")
	}
	if !errors.Is(err, types.CodeLifecycle001) {
		t.Errorf("error does not carry %s: %v", types.CodeLifecycle001, err)
	}
	if !strings.Contains(err.Error(), `unknown flag "--no-such-key"`) {
		t.Errorf("error = %q, want it to name the flag", err)
	}

	// The daemon maps that refusal to exit 13 (its "refusable condition" code).
	if code := run([]string{"--no-such-key", "1"}); code != 13 {
		t.Errorf("run(--no-such-key) = %d, want 13", code)
	}
}

// TestDocumentedKeyNoLongerDiesInTheFlagSet is B5: the pre-fix entry point is kept
// here as the CONTROL, so the acceptance cannot be satisfied by a test that passes
// on both trees, and the shipped path is then driven end to end — argv in, state
// root out, boot refusal code 13 instead of the flag package's 2.
func TestDocumentedKeyNoLongerDiesInTheFlagSet(t *testing.T) {
	// The pre-fix surface, verbatim: the three flags cmd/troubled registered.
	legacy := flag.NewFlagSet("troubled", flag.ContinueOnError)
	legacy.SetOutput(io.Discard)
	legacy.String("config", "", "config file")
	legacy.Bool("v", false, "debug logging")
	legacy.Bool("version", false, "print the version triple and exit")
	legacyErr := legacy.Parse([]string{"--state_root", t.TempDir()})
	if legacyErr == nil {
		t.Fatal("the pre-fix flag set ACCEPTED --state_root: this control no longer reproduces TRBL-018")
	}
	if !strings.Contains(legacyErr.Error(), "flag provided but not defined") {
		t.Fatalf("pre-fix error = %q, want the flag package's refusal", legacyErr)
	}

	// The shipped path: the same argument is forwarded and resolves.
	root := argvStateRoot(t)
	own, keys, err := splitDaemonArgs([]string{"--state_root", root})
	if err != nil {
		t.Fatalf("splitDaemonArgs refused the documented key: %v", err)
	}
	if own != (daemonArgs{}) || !reflect.DeepEqual(keys, []string{"--state_root", root}) {
		t.Fatalf("own=%+v keys=%q, want the key flag forwarded verbatim", own, keys)
	}
	res, err := lifecycle.Resolve(keys, nil, absentConfig(t))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	row := rowFor(t, res.Values, "state_root")
	if row.Value != root || row.Source != "flag" || row.SourceRef != "--state_root" {
		t.Errorf("state_root row = %+v, want value %q from --state_root", row, root)
	}

	// End to end: argv is no longer the death point. The state root below is under
	// /tmp, which the boot gate refuses (TROUBLE-LIFECYCLE-004) — exit 13, not the
	// flag package's 2, and the refusal names the gate rather than argv.
	code := run([]string{"--state_root", t.TempDir()})
	if code != 13 {
		t.Errorf("run(--state_root <under /tmp>) = %d, want 13 (a boot refusal, not a usage error)", code)
	}
}

// --- B4: the argv secret control still holds ---------------------------------

// TestSecretShapedFlagValueStillRefusedOnArgv is the security invariant of this
// row (SPEC-12 §3.2): making argv a real input path must not give secrets a way
// in. The value rides a REGISTERED key, so it is the scan — not the registry —
// that refuses it, and the refusal is proved on a real /proc/self/cmdline.
func TestSecretShapedFlagValueStillRefusedOnArgv(t *testing.T) {
	const secret = "AKIAIOSFODNN7EXAMPLE" // cloud_key_shape (SPEC-02 mandatory set)

	// (a) the scan the daemon runs at boot step 3, on this process's cmdline.
	code, out := runChild(t, "scan", "--dashboard-mandate", secret)
	if code != 13 || !strings.Contains(out, string(types.CodeLifecycle013)) {
		t.Errorf("scan child with a secret-shaped value: exit %d, stderr %q; want 13 naming %s",
			code, out, types.CodeLifecycle013)
	}
	// (b) control: the same argv with an ordinary value passes the scan, so (a) is
	// about the value and not about the argument itself.
	if code, out := runChild(t, "scan", "--dashboard-mandate", "plain-value"); code != 0 {
		t.Errorf("scan child control: exit %d, stderr %q; want 0", code, out)
	}

	// (c) the daemon's own boot refuses it: the value is accepted by the resolver
	// (step 1) and the scan (step 3) is what ends the boot.
	root := argvStateRoot(t)
	code, out = runChild(t, "run", "--state_root", root, "--dashboard-mandate", secret)
	if code != 13 || !strings.Contains(out, string(types.CodeLifecycle013)) {
		t.Errorf("daemon with a secret-shaped flag value: exit %d, stderr %q; want 13 naming %s",
			code, out, types.CodeLifecycle013)
	}
	// (d) control: the same argv shape failing a DIFFERENT gate reports that gate
	// (the state root under /tmp, 004), so (c) is not "the child always exits 13".
	code, out = runChild(t, "run", "--state_root", t.TempDir(), "--dashboard-mandate", secret)
	if code != 13 || !strings.Contains(out, string(types.CodeLifecycle004)) {
		t.Errorf("daemon control with an unusable state root: exit %d, stderr %q; want 13 naming %s",
			code, out, types.CodeLifecycle004)
	}
}

// --- the daemon's own surface stays honest -----------------------------------

// TestHelpPrintsTheRealSurface: -h/--help must document the surface the binary
// actually has (its own flags AND the per-key form), not the three-flag
// automessage the flag package would print.
func TestHelpPrintsTheRealSurface(t *testing.T) {
	for _, arg := range []string{"-h", "--help"} {
		got, code := captureStdout(t, func() int { return run([]string{arg}) })
		if code != 0 {
			t.Errorf("run(%s) = %d, want 0", arg, code)
		}
		for _, want := range []string{"--config <path>", "--version", "--<key> <value>", "TROUBLE-LIFECYCLE-001", "config explain"} {
			if !strings.Contains(got, want) {
				t.Errorf("run(%s) usage does not mention %q:\n%s", arg, want, got)
			}
		}
		if strings.Contains(got, "-usage:") || strings.Contains(got, "Usage of") {
			t.Errorf("run(%s) printed the flag package automessage:\n%s", arg, got)
		}
	}
}

func TestVersionPrintsTheTriple(t *testing.T) {
	got, code := captureStdout(t, func() int { return run([]string{"--version"}) })
	if code != 0 {
		t.Fatalf("run(--version) = %d, want 0", code)
	}
	got = strings.TrimSpace(got)
	// `version git_sha build_time [stamped|UNSTAMPED]` (SPEC-12 §3.4).
	if !strings.HasSuffix(got, "[stamped]") && !strings.HasSuffix(got, "[UNSTAMPED]") {
		t.Fatalf("run(--version) = %q, want the version triple with its stamp state", got)
	}
	if fields := strings.Fields(got); len(fields) != 4 {
		t.Errorf("run(--version) = %q, want `version git_sha build_time [state]` (4 fields)", got)
	}
}

// captureStdout runs fn with os.Stdout redirected and returns what it printed.
func captureStdout(t *testing.T, fn func() int) (string, int) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	saved := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	code := fn()
	os.Stdout = saved
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out, code
}
