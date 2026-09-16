package registry

// authorize_test.go — the a1–a7 authorize ladder of SPEC-06 §2.3 and the §7 row
// for this file: "the a1–a7 ladder, one test per step, including deny by default
// with an empty grant set; shadow forces check_mode; a scope: wildcard cannot
// authorize a PONR module".
//
// Threshold asserted here (SPEC-06 §7): 100% of the seven steps have a dedicated
// refusal case and every refusal reaches ZERO module invocations. The evidence for
// the zero-invocation property is countingModule (below) — a wrapper that counts
// every Check/Apply/Verify the pipeline reaches — never a diff of the target.
//
// This file also carries the shared harness the other two files of the §7 core
// set use (registryLedgerSpy, newRegTestRegistry, assertRefused): a ledger double
// whose Append fills RecID/Seq exactly as SPEC-01's writer does, a hermetic config
// (state root, do-not-touch file, XDG dirs all under t.TempDir()) and the refusal
// contract one call at a time.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// ---------------------------------------------------------------------------
// the shared harness
// ---------------------------------------------------------------------------

// invocationLog records every module method the pipeline reaches. A refusal that
// invokes a module is a contract violation, and this log is how the tests prove
// the absence rather than assuming it.
type invocationLog struct {
	mu     sync.Mutex
	counts map[string]int
	calls  []string
}

func newInvocationLog() *invocationLog { return &invocationLog{counts: map[string]int{}} }

func (l *invocationLog) add(kind, name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.counts[kind]++
	l.calls = append(l.calls, kind+" "+name)
}

func (l *invocationLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, c := range l.counts {
		n += c
	}
	return n
}

func (l *invocationLog) countOf(kind string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.counts[kind]
}

func (l *invocationLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.calls) == 0 {
		return "(no module invocation)"
	}
	return strings.Join(l.calls, ", ")
}

func (l *invocationLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.counts = map[string]int{}
	l.calls = nil
}

// countingModule wraps one registered module and counts invocations. The wrapper
// keeps the module's own descriptor (and its committed schema), so it registers
// exactly where the module itself would.
type countingModule struct {
	types.Module
	log *invocationLog
}

func (c countingModule) Check(ctx context.Context, args map[string]any) (types.Diff, error) {
	c.log.add("check", c.Module.Descriptor().Name)
	return c.Module.Check(ctx, args)
}

func (c countingModule) Apply(ctx context.Context, args map[string]any) (types.Result, error) {
	c.log.add("apply", c.Module.Descriptor().Name)
	return c.Module.Apply(ctx, args)
}

func (c countingModule) Verify(ctx context.Context, args map[string]any) (types.VerifyResult, error) {
	c.log.add("verify", c.Module.Descriptor().Name)
	return c.Module.Verify(ctx, args)
}

// registryLedgerSpy is the SPEC-01 writer double: it fills Seq, RecID, TS and
// SchemaVersion the way the real writer does (the pipeline reads rec.RecID for the
// audit stage detail, so a spy that returned the draft unchanged would test a
// contract the daemon does not have) and keeps the records for inspection.
type registryLedgerSpy struct {
	mu      sync.Mutex
	records []types.Record
	seq     uint64
	fail    func(types.Record) error
}

func (l *registryLedgerSpy) append(rec types.Record) (types.Record, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fail != nil {
		if err := l.fail(rec); err != nil {
			return rec, err
		}
	}
	l.seq++
	rec.Seq = l.seq
	rec.RecID = types.NewID(types.PEv)
	// SPEC-01's RecordDraft allocates TS and SchemaVersion too: the registry
	// hands over a draft, the writer mints the wire fields.
	rec.TS = types.NowUTC()
	rec.SchemaVersion = 1
	l.records = append(l.records, rec)
	return rec, nil
}

func (l *registryLedgerSpy) snapshot() []types.Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]types.Record, len(l.records))
	copy(out, l.records)
	return out
}

func (l *registryLedgerSpy) ofKind(kind types.RecordKind) []types.Record {
	var out []types.Record
	for _, rec := range l.snapshot() {
		if rec.Kind == kind {
			out = append(out, rec)
		}
	}
	return out
}

func (l *registryLedgerSpy) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = nil
	l.seq = 0
}

// regTestOpts is the wiring a registry test varies.
type regTestOpts struct {
	// Gates is the SPEC-05 snapshot every call reads (zero value → shadow, the
	// boot default of types.DefaultGates).
	Gates types.AutonomyGates
	// Config is appended to the hermetic base config (state root, do-not-touch
	// file): a later key wins, so a caller may override either.
	Config         []types.ConfigValue
	SkillAuthorize func(ctx context.Context, skillID, module string, scopes []string) error
	// Modules replaces the shipped module set (still wrapped for counting).
	Modules []types.Module
	// Extra modules are registered after NewWith — test-only modules, which carry
	// no committed schema and therefore cannot go through NewWith's boot check.
	Extra []types.Module
	// AppendFail makes the ledger double refuse a record.
	AppendFail func(types.Record) error
}

// newRegTestRegistry builds a registry over the shipped module set (wrapped for
// counting) with a hermetic configuration. The XDG environment is redirected into
// temp dirs because a shipped module that reads its own configuration falls back
// to the §4.3 defaults, and those defaults must never reach the developer's real
// state root from a test.
func newRegTestRegistry(t *testing.T, opts regTestOpts) (*Registry, *registryLedgerSpy, *invocationLog) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	stateRoot := t.TempDir()

	log := newInvocationLog()
	spy := &registryLedgerSpy{fail: opts.AppendFail}
	cfg := []types.ConfigValue{
		{Key: "registry.state_root", Value: stateRoot},
		// A temp do-not-touch path keeps the test independent of any file the
		// developer happens to have under ~/.config/trouble.
		{Key: "registry.do_not_touch_file", Value: filepath.Join(stateRoot, "do-not-touch.toml")},
	}
	cfg = append(cfg, opts.Config...)

	deps := RegistryDeps{
		Append: spy.append,
		Scrub: func(target string, in []byte) (types.ScrubResult, error) {
			// Identity scrub: the ledger copy of args is still the pipeline's,
			// and SPEC-02 owns the rules themselves.
			return types.ScrubResult{Value: in, BytesIn: len(in)}, nil
		},
		Gates:          func() types.AutonomyGates { return opts.Gates },
		SkillAuthorize: opts.SkillAuthorize,
		Now:            time.Now,
		Config:         cfg,
		HostID:         "test-host",
		Actor:          types.Actor{Kind: types.ActorDaemon, ID: "troubled-test", Version: "0.1.0"},
	}

	mods := opts.Modules
	if mods == nil {
		shipped := shippedModules()
		mods = make([]types.Module, 0, len(shipped))
		for _, m := range shipped {
			mods = append(mods, countingModule{Module: m, log: log})
		}
	}
	r, err := NewWith(deps, mods)
	if err != nil {
		t.Fatalf("the shipped module set must register (%s on failure): %v", types.CodeRegistry014, err)
	}
	for _, m := range opts.Extra {
		if err := r.Register(m); err != nil {
			t.Fatalf("register test module %s: %v", m.Descriptor().Name, err)
		}
	}
	return r, spy, log
}

// grantHostCapability marks the polkit probe as authorized, so a service.* case
// can reach the step it is written for instead of stopping at a4.
func grantHostCapability(t *testing.T, r *Registry) {
	t.Helper()
	r.capMu.Lock()
	defer r.capMu.Unlock()
	r.capChecked = true
	r.capPresent = true
	r.capRulesOK = true
	r.capReason = ""
}

func allowRootsConfig(root string) []types.ConfigValue {
	return []types.ConfigValue{{Key: "file.allow_roots", Value: []string{root}}}
}

func cfgValues(kv ...any) []types.ConfigValue {
	out := make([]types.ConfigValue, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		out = append(out, types.ConfigValue{Key: kv[i].(string), Value: kv[i+1]})
	}
	return out
}

func testRequest(module, mode string, args map[string]any, grants []string) types.ToolCallRequest {
	return types.ToolCallRequest{
		Module: module, Args: args, Mode: mode, Grants: grants,
		Source: "rule:test-rule", Inc: "inc_test", Rule: "test-rule",
		Actor: types.Actor{Kind: types.ActorHuman, ID: "cli"},
	}
}

func stageDetails(tc types.ToolCall) string {
	var b strings.Builder
	for i, s := range tc.Stage {
		b.WriteString(s.Stage)
		b.WriteString("/ok=")
		if s.OK {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
		b.WriteString(" ")
		b.WriteString(s.Detail)
		if i < len(tc.Stage)-1 {
			b.WriteString("; ")
		}
	}
	return b.String()
}

func payloadString(rec types.Record, key string) string {
	s, _ := rec.Payload[key].(string)
	return s
}

func payloadBool(rec types.Record, key string) bool {
	b, _ := rec.Payload[key].(bool)
	return b
}

// recordStages reads the stage list off a ledger record.
func recordStages(t *testing.T, rec types.Record) []types.CallStage {
	t.Helper()
	stages, ok := rec.Payload["stage"].([]types.CallStage)
	if !ok {
		t.Fatalf("record %s carries no readable stage list (%T)", rec.RecID, rec.Payload["stage"])
	}
	return stages
}

// assertRefused pins the refusal contract shared by every authorize step
// (SPEC-06 §2.3 stage 1, §5): the ladder refuses, the module is never called, the
// ladder escalates on the recorded code+class, and the refusal is one durable
// outcome record.
func assertRefused(t *testing.T, tc types.ToolCall, spy *registryLedgerSpy, log *invocationLog, code types.ErrorCode, reason string) {
	t.Helper()
	if got := types.ErrorCode(tc.ErrorCode); got != code {
		t.Fatalf("error_code = %q, want %q (stages: %s)", got, code, stageDetails(tc))
	}
	if len(tc.Stage) != 2 || tc.Stage[0].Stage != types.StageAuthorize || tc.Stage[0].OK {
		t.Fatalf("a refusal is one failed authorize stage plus the audit stage, got %d: %s", len(tc.Stage), stageDetails(tc))
	}
	if tc.Stage[1].Stage != types.StageAudit || !tc.Stage[1].OK {
		t.Fatalf("the refusal's outcome record is appended at audit: %s", stageDetails(tc))
	}
	if n := log.count(); n != 0 {
		t.Fatalf("a refusal invoked the module %d time(s): %s", n, log.String())
	}
	recs := spy.snapshot()
	if len(recs) != 1 {
		t.Fatalf("a refusal appends exactly one outcome record, got %d", len(recs))
	}
	rec := recs[0]
	if rec.Kind != types.KToolCall {
		t.Fatalf("record kind = %q, want %q", rec.Kind, types.KToolCall)
	}
	if rec.RecID == "" || rec.Seq == 0 {
		t.Fatalf("the ledger double must fill RecID/Seq like SPEC-01's writer: %+v", rec)
	}
	if got := payloadString(rec, "error_code"); got != string(code) {
		t.Fatalf("payload.error_code = %q, want %q", got, code)
	}
	wantClass := types.CodeClass[code]
	if got := payloadString(rec, "error_class"); got != string(wantClass) {
		t.Fatalf("payload.error_class = %q, want %q (SPEC-06 §5 keys retries on the class)", got, wantClass)
	}
	if reason != "" {
		if got := payloadString(rec, "reason"); got != reason {
			t.Fatalf("payload.reason = %q, want %q", got, reason)
		}
	}
	if _, ok := rec.Payload["intent"]; !ok {
		t.Fatalf("every tool_call record carries the intent flag (SPEC-06 §4.2)")
	}
	// The caller-visible half of the record-first contract (SPEC-06 §2.2).
	e := Outcome(tc)
	if e == nil {
		t.Fatalf("Outcome(tc) must carry the refusal")
	}
	if CodeOf(e) != code {
		t.Fatalf("Outcome code = %q, want %q", CodeOf(e), code)
	}
	if ClassOf(e) != wantClass {
		t.Fatalf("Outcome class = %q, want %q", ClassOf(e), wantClass)
	}
}

// ---------------------------------------------------------------------------
// a1–a7: one dedicated refusal case per step
// ---------------------------------------------------------------------------

// authCase is one ladder row's request.
type authCase struct {
	module     string
	args       map[string]any
	mode       string
	grants     []string
	source     string
	gates      types.AutonomyGates
	config     []types.ConfigValue
	capability bool
	skill      func(ctx context.Context, skillID, module string, scopes []string) error
}

type authRow struct {
	name       string
	setup      func(t *testing.T, c *authCase)
	wantCode   types.ErrorCode
	wantReason string
}

// TestAuthorizeLadder covers the seven steps of SPEC-06 §2.3's authorize ladder,
// the first refusal winning at every step.
func TestAuthorizeLadder(t *testing.T) {
	denyingSkill := func(ctx context.Context, skillID, module string, scopes []string) error {
		return errors.New("skill allowlist says no")
	}
	unit := "payment-worker.service"

	rows := []authRow{
		{
			name: "a1-unknown-module",
			setup: func(t *testing.T, c *authCase) {
				c.module = "service.stop" // not in the §3.8 table
				c.mode = types.ModeCheck
				c.args = map[string]any{}
				c.grants = []string{"service.stop"}
			},
			wantCode:   types.CodeRegistry001,
			wantReason: reasonModuleUnavailable,
		},
		{
			// SPEC-06 §6.14: the reserved capability-probe name is never
			// dispatchable. The refusal is a1's unknown-module refusal — the
			// name is not in the §3.8 table — which is also why authorize.go's
			// dedicated reserved-name branch is unreachable (reported, not
			// fixed: authorize.go is not this task's file).
			name: "a1-reserved-capability-probe",
			setup: func(t *testing.T, c *authCase) {
				c.module = CapabilityProbeModule
				c.mode = types.ModeCheck
				c.args = map[string]any{}
			},
			wantCode:   types.CodeRegistry001,
			wantReason: reasonModuleUnavailable,
		},
		{
			name: "a2-kill-switch",
			setup: func(t *testing.T, c *authCase) {
				tmp := t.TempDir()
				path := writeTempFile(t, tmp, "app.conf", ladderFixture)
				c.module = "file.patch"
				c.mode = types.ModeApply
				c.args = map[string]any{"path": path, "patch": ladderPatch}
				c.grants = []string{"file.patch"}
				c.gates = types.AutonomyGates{Mode: types.AutoFull, KillSwitch: true}
				c.config = allowRootsConfig(tmp)
			},
			wantCode:   types.CodeRegistry006,
			wantReason: reasonKillSwitch,
		},
		{
			name: "a3-protected-path",
			setup: func(t *testing.T, c *authCase) {
				c.module = "file.read"
				c.mode = types.ModeCheck
				c.args = map[string]any{"path": "/etc/shadow"}
			},
			wantCode:   types.CodeRegistry007,
			wantReason: reasonDoNotTouchPath,
		},
		{
			name: "a3-protected-unit",
			setup: func(t *testing.T, c *authCase) {
				c.module = "service.status"
				c.mode = types.ModeCheck
				c.args = map[string]any{"unit": "sshd.service"}
			},
			wantCode:   types.CodeRegistry007,
			wantReason: reasonDoNotTouchUnit,
		},
		{
			// a3 runs before a4: the unit is also absent from
			// registry.service_units, and the do-not-touch hit still wins
			// (SPEC-06 §3.6 "Enforcement point: step a3").
			name: "a3-precedes-a4",
			setup: func(t *testing.T, c *authCase) {
				c.module = "service.reload"
				c.mode = types.ModeApply
				c.args = map[string]any{"unit": "sshd.service"}
				c.grants = []string{"service.reload"}
				c.gates = types.AutonomyGates{Mode: types.AutoFull}
				c.config = cfgValues("registry.service_units", []string{})
			},
			wantCode:   types.CodeRegistry007,
			wantReason: reasonDoNotTouchUnit,
		},
		{
			name: "a4-service-unit-not-allowed",
			setup: func(t *testing.T, c *authCase) {
				c.module = "service.reload"
				c.mode = types.ModeApply
				c.args = map[string]any{"unit": unit}
				c.grants = []string{"service.reload"}
				c.gates = types.AutonomyGates{Mode: types.AutoFull}
				c.config = cfgValues("registry.service_units", []string{})
			},
			wantCode:   types.CodeRegistry006,
			wantReason: reasonServeUnitNotAllowed,
		},
		{
			name: "a4-service-unit-not-on-the-allowlist",
			setup: func(t *testing.T, c *authCase) {
				c.module = "service.reload"
				c.mode = types.ModeApply
				c.args = map[string]any{"unit": unit}
				c.grants = []string{"service.reload"}
				c.gates = types.AutonomyGates{Mode: types.AutoFull}
				c.config = cfgValues("registry.service_units", []string{"other.service"})
			},
			wantCode:   types.CodeRegistry006,
			wantReason: reasonServeUnitNotAllowed,
		},
		{
			// The unit is allowlisted but the host has no polkit capability: the
			// distinct policy_refused class of SPEC-06 §3.10.
			name: "a4-polkit-missing-policy",
			setup: func(t *testing.T, c *authCase) {
				c.module = "service.reload"
				c.mode = types.ModeApply
				c.args = map[string]any{"unit": unit}
				c.grants = []string{"service.reload"}
				c.gates = types.AutonomyGates{Mode: types.AutoFull}
				c.config = cfgValues("registry.service_units", []string{unit})
			},
			wantCode:   types.CodeRegistry006,
			wantReason: reasonPolicyMissing,
		},
		{
			name: "a4-allow-root-escape",
			setup: func(t *testing.T, c *authCase) {
				tmp := t.TempDir()
				outside := t.TempDir()
				path := writeTempFile(t, outside, "app.conf", ladderFixture)
				c.module = "file.patch"
				c.mode = types.ModeApply
				c.args = map[string]any{"path": path, "patch": ladderPatch}
				c.grants = []string{"file.patch"}
				c.gates = types.AutonomyGates{Mode: types.AutoFull}
				c.config = allowRootsConfig(tmp)
			},
			wantCode:   types.CodeRegistry006,
			wantReason: reasonAllowRootEscape,
		},
		{
			name: "a4-allow-roots-empty-nothing-is-patchable",
			setup: func(t *testing.T, c *authCase) {
				tmp := t.TempDir()
				path := writeTempFile(t, tmp, "app.conf", ladderFixture)
				c.module = "file.patch"
				c.mode = types.ModeApply
				c.args = map[string]any{"path": path, "patch": ladderPatch}
				c.grants = []string{"file.patch"}
				c.gates = types.AutonomyGates{Mode: types.AutoFull}
				c.config = cfgValues("file.allow_roots", []string{})
			},
			wantCode:   types.CodeRegistry006,
			wantReason: reasonAllowRootEscape,
		},
		{
			// Deny-by-default on the one remotely authored input path: a nil
			// Authorizer hook refuses every skill-sourced mutating call
			// (SPEC-06 §3.3).
			name: "a5-skill-source-with-nil-authorizer",
			setup: func(t *testing.T, c *authCase) {
				tmp := t.TempDir()
				path := writeTempFile(t, tmp, "app.conf", ladderFixture)
				c.module = "file.patch"
				c.mode = types.ModeApply
				c.source = "skill:sk_01J9"
				c.args = map[string]any{"path": path, "patch": ladderPatch}
				c.grants = []string{"file.patch"}
				c.gates = types.AutonomyGates{Mode: types.AutoFull}
				c.config = allowRootsConfig(tmp)
			},
			wantCode:   types.CodeRegistry006,
			wantReason: reasonSkillAllowlist,
		},
		{
			name: "a5-skill-source-with-a-denying-authorizer",
			setup: func(t *testing.T, c *authCase) {
				tmp := t.TempDir()
				path := writeTempFile(t, tmp, "app.conf", ladderFixture)
				c.module = "file.patch"
				c.mode = types.ModeApply
				c.source = "skill:sk_01J9"
				c.args = map[string]any{"path": path, "patch": ladderPatch}
				c.grants = []string{"file.patch"}
				c.gates = types.AutonomyGates{Mode: types.AutoFull}
				c.config = allowRootsConfig(tmp)
				c.skill = denyingSkill
			},
			wantCode:   types.CodeRegistry006,
			wantReason: reasonSkillAllowlist,
		},
		{
			name: "a6-scope-not-granted",
			setup: func(t *testing.T, c *authCase) {
				tmp := t.TempDir()
				path := writeTempFile(t, tmp, "app.conf", ladderFixture)
				c.module = "file.patch"
				c.mode = types.ModeApply
				c.args = map[string]any{"path": path, "patch": ladderPatch}
				c.grants = []string{}
				c.gates = types.AutonomyGates{Mode: types.AutoFull}
				c.config = allowRootsConfig(tmp)
			},
			wantCode:   types.CodeRegistry008,
			wantReason: reasonScopeNotGranted,
		},
		{
			name: "a6-wildcard-of-another-scope-does-not-cover-file-write",
			setup: func(t *testing.T, c *authCase) {
				tmp := t.TempDir()
				path := writeTempFile(t, tmp, "app.conf", ladderFixture)
				c.module = "file.patch"
				c.mode = types.ModeApply
				c.args = map[string]any{"path": path, "patch": ladderPatch}
				c.grants = []string{"scope:file:read"}
				c.gates = types.AutonomyGates{Mode: types.AutoFull}
				c.config = allowRootsConfig(tmp)
			},
			wantCode:   types.CodeRegistry008,
			wantReason: reasonScopeNotGranted,
		},
		{
			// A scope: wildcard never authorizes a PONR call (SPEC-06 §3.5).
			name: "a7-ponr-with-a-scope-wildcard",
			setup: func(t *testing.T, c *authCase) {
				c.module = "service.reload"
				c.mode = types.ModeApply
				c.args = map[string]any{"unit": unit}
				c.grants = []string{"scope:service:write"}
				c.gates = types.AutonomyGates{Mode: types.AutoFull}
				c.config = cfgValues("registry.service_units", []string{unit})
				c.capability = true
			},
			wantCode:   types.CodeRegistry015,
			wantReason: reasonPONRWithoutGrant,
		},
		{
			name: "a7-ponr-without-any-grant",
			setup: func(t *testing.T, c *authCase) {
				c.module = "flow.create_task"
				c.mode = types.ModeApply
				c.args = map[string]any{"sig": "sig_test", "title": "a task"}
				c.grants = []string{}
				c.gates = types.AutonomyGates{Mode: types.AutoFull}
			},
			wantCode:   types.CodeRegistry015,
			wantReason: reasonPONRWithoutGrant,
		},
	}

	for _, row := range rows {
		row := row
		t.Run(row.name, func(t *testing.T) {
			c := authCase{mode: types.ModeCheck, gates: types.AutonomyGates{Mode: types.AutoFull}}
			if row.setup != nil {
				row.setup(t, &c)
			}
			r, spy, log := newRegTestRegistry(t, regTestOpts{
				Gates: c.gates, Config: c.config, SkillAuthorize: c.skill,
			})
			if c.capability {
				grantHostCapability(t, r)
			}
			req := testRequest(c.module, c.mode, c.args, c.grants)
			if c.source != "" {
				req.Source = c.source
			}
			tc, err := r.Call(context.Background(), req)
			if err != nil {
				t.Fatalf("a refusal is data on the record, not an error (SPEC-06 §2.2): %v", err)
			}
			assertRefused(t, tc, spy, log, row.wantCode, row.wantReason)
		})
	}
}

// TestAuthorizeDenyByDefaultWithAnEmptyGrantSet is the §7 threshold's "deny by
// default": with no grant at all, every mutating module of the v0.1 set refuses at
// authorize and reaches no module. A PONR module refuses at a7 (015); a module
// with a usable rollback refuses at a6 (008).
func TestAuthorizeDenyByDefaultWithAnEmptyGrantSet(t *testing.T) {
	tmp := t.TempDir()
	path := writeTempFile(t, tmp, "app.conf", ladderFixture)
	unit := "payment-worker.service"

	args := map[string]map[string]any{
		"config.set":       {"path": filepath.Join(tmp, "svc.conf"), "key": "port", "value": 1},
		"file.patch":       {"path": path, "patch": ladderPatch},
		"service.reload":   {"unit": unit},
		"service.restart":  {"unit": unit},
		"flow.file_issue":  {"sig": "sig_test", "title": "t", "body": "b"},
		"flow.create_task": {"sig": "sig_test", "title": "t"},
		"flow.comment":     {"sig": "sig_test", "body": "b"},
	}

	r, spy, log := newRegTestRegistry(t, regTestOpts{
		Gates: types.AutonomyGates{Mode: types.AutoFull},
		Config: append(allowRootsConfig(tmp),
			cfgValues("registry.service_units", []string{unit})...),
	})
	grantHostCapability(t, r)

	var mutating []string
	for _, d := range r.List() {
		if d.Mutating {
			mutating = append(mutating, d.Name)
		}
	}
	if len(mutating) != len(args) {
		t.Fatalf("the v0.1 mutating surface is %d modules %v, the fixture list has %d", len(mutating), mutating, len(args))
	}

	for _, name := range mutating {
		name := name
		t.Run(name, func(t *testing.T) {
			spy.reset()
			log.reset()
			// Half the rows pass a nil grant slice, half an empty one: both
			// spellings are an empty grant set.
			var grants []string
			if name == "file.patch" || name == "flow.comment" {
				grants = []string{}
			}
			tc, err := r.Call(context.Background(), testRequest(name, types.ModeApply, args[name], grants))
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			want, wantReason := types.CodeRegistry008, reasonScopeNotGranted
			if IsPONR(name) {
				// A PONR module refuses one step earlier, at a7.
				want, wantReason = types.CodeRegistry015, reasonPONRWithoutGrant
			}
			assertRefused(t, tc, spy, log, want, wantReason)
		})
	}
}

// TestAuthorizeShadowForcesCheckMode pins step a7's mode forcing (SPEC-06 §3.3):
// under shadow an apply request is allowed, and what runs is check_mode — the
// module is asked to Check, never to Apply, and the target is untouched.
func TestAuthorizeShadowForcesCheckMode(t *testing.T) {
	tmp := t.TempDir()
	path := writeTempFile(t, tmp, "app.conf", ladderFixture)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat fixture: %v", err)
	}

	r, spy, log := newRegTestRegistry(t, regTestOpts{
		Gates:  types.AutonomyGates{Mode: types.AutoShadow},
		Config: allowRootsConfig(tmp),
	})
	tc, callErr := r.Call(context.Background(), testRequest("file.patch", types.ModeApply,
		map[string]any{"path": path, "patch": ladderPatch}, []string{"file.patch"}))
	if callErr != nil {
		t.Fatalf("a mode-forced call is allowed, not an error: %v", callErr)
	}
	if !tc.Stage[0].OK {
		t.Fatalf("shadow forces the mode, it does not refuse: %s", stageDetails(tc))
	}
	if tc.Mode != types.ModeCheck {
		t.Fatalf("Mode = %q, want %q (shadow forces check_mode)", tc.Mode, types.ModeCheck)
	}
	if log.countOf("apply") != 0 {
		t.Fatalf("shadow ran Apply: %s", log.String())
	}
	if log.countOf("check") == 0 {
		t.Fatalf("a check_mode call must still run the module's Check (SPEC-06 §2.3 stage 3)")
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat fixture: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("mtime moved: %s -> %s", before.ModTime(), after.ModTime())
	}
	if got := readTempFile(t, path); got != ladderFixture {
		t.Fatalf("a mode-forced call changed the target:\n%q", got)
	}
	recs := spy.snapshot()
	if len(recs) != 1 {
		t.Fatalf("a check_mode call is one terminal record, got %d", len(recs))
	}
	if got := payloadString(recs[0], "mode"); got != types.ModeCheck {
		t.Fatalf("record mode = %q, want %q", got, types.ModeCheck)
	}
}

// TestAuthorizePONRNeedsAModuleNameGrant is the other half of a7 (SPEC-06 §3.5):
// the explicit module-name grant is the only thing that authorizes a
// point-of-no-return call, and it authorizes it in every mode including full.
func TestAuthorizePONRNeedsAModuleNameGrant(t *testing.T) {
	unit := "payment-worker.service"
	ctx := context.Background()

	// (1) The scope wildcard is refused (through Call: the durable refusal).
	r, spy, log := newRegTestRegistry(t, regTestOpts{
		Gates:  types.AutonomyGates{Mode: types.AutoFull},
		Config: cfgValues("registry.service_units", []string{unit}),
	})
	grantHostCapability(t, r)
	tc, err := r.Call(ctx, testRequest("service.reload", types.ModeApply,
		map[string]any{"unit": unit}, []string{"scope:service:write"}))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	assertRefused(t, tc, spy, log, types.CodeRegistry015, reasonPONRWithoutGrant)

	// (2) The same call with the module-name grant clears authorize. authorize is
	// consulted directly so the assertion stops at the gate and never dials
	// D-Bus.
	_, dec, aerr := r.authorize(ctx, testRequest("service.reload", types.ModeApply,
		map[string]any{"unit": unit}, []string{"service.reload"}))
	if aerr != nil {
		t.Fatalf("an explicit module-name grant must authorize a PONR call: %v", aerr)
	}
	if dec.mode != types.ModeApply {
		t.Fatalf("decision mode = %q, want %q", dec.mode, types.ModeApply)
	}

	// (3) Same shape for a capability-free PONR module (the whole flow.* set),
	// through Call: the grant clears authorize and the call proceeds.
	r2, _, log2 := newRegTestRegistry(t, regTestOpts{Gates: types.AutonomyGates{Mode: types.AutoFull}})
	tc2, err := r2.Call(ctx, testRequest("flow.create_task", types.ModeApply,
		map[string]any{"sig": "sig_test", "title": "t"}, []string{"flow.create_task"}))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !tc2.Stage[0].OK || tc2.ErrorCode == string(types.CodeRegistry015) {
		t.Fatalf("the module-name grant must clear a7: %s", stageDetails(tc2))
	}
	if log2.countOf("check") == 0 {
		t.Fatalf("the granted call must reach the dry-run stage")
	}
}

// TestAuthorizeSkillSeamIsConsulted pins a5's positive side: the seam receives the
// skill id, the module and the descriptor scopes, and its verdict is what decides.
func TestAuthorizeSkillSeamIsConsulted(t *testing.T) {
	tmp := t.TempDir()
	path := writeTempFile(t, tmp, "app.conf", ladderFixture)

	var gotSkill, gotModule string
	var gotScopes []string
	allow := func(ctx context.Context, skillID, module string, scopes []string) error {
		gotSkill, gotModule, gotScopes = skillID, module, scopes
		return nil
	}
	r, _, log := newRegTestRegistry(t, regTestOpts{
		Gates:          types.AutonomyGates{Mode: types.AutoFull},
		Config:         allowRootsConfig(tmp),
		SkillAuthorize: allow,
	})
	// check_mode keeps the call inert (a5 is a source rule, not a mode rule).
	tc, err := r.Call(context.Background(), types.ToolCallRequest{
		Module: "file.patch", Args: map[string]any{"path": path, "patch": ladderPatch},
		Mode: types.ModeCheck, Grants: []string{"file.patch"}, Source: "skill:sk_01J9",
		Inc: "inc_test", Actor: types.Actor{Kind: types.ActorHuman, ID: "cli"},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !tc.Stage[0].OK {
		t.Fatalf("an authorizer that allows must clear a5: %s", stageDetails(tc))
	}
	if gotSkill != "sk_01J9" {
		t.Fatalf("authorizer skill id = %q, want %q (the skill: prefix is stripped)", gotSkill, "sk_01J9")
	}
	if gotModule != "file.patch" {
		t.Fatalf("authorizer module = %q", gotModule)
	}
	if len(gotScopes) != 1 || gotScopes[0] != "file:write" {
		t.Fatalf("authorizer scopes = %v, want [file:write]", gotScopes)
	}
	if log.countOf("check") == 0 {
		t.Fatalf("an allowed skill-sourced call must reach the dry-run stage")
	}
}

// ladders fixtures: one tiny file and the one-hunk patch every ladder row uses.
const ladderFixture = "listen = 8080\nworkers = 4\n"

const ladderPatch = `--- a/app.conf
+++ b/app.conf
@@ -1,2 +1,2 @@
 listen = 8080
-workers = 4
+workers = 8
`

func writeTempFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("fixture %s: %v", path, err)
	}
	return path
}

func readTempFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
