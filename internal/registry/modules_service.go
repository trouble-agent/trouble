package registry

// modules_service.go — service.status, service.reload, service.restart (SPEC-06 §3.8, §3.9).
//
// All three talk to org.freedesktop.systemd1 over D-Bus on the configured manager
// (§3.9): `scope=system` → the system bus, `scope=user` → the configured user
// manager, because this fleet's units are user units and a system-only watcher
// reports an all-green lie. service.status is `pure`; reload and restart are
// `once` PONR modules whose only dedup is the registry's IdemKey replay guard
// (§3.4, §3.5).
//
// Why these verbs are an install-time contract (§3.10): measured on this class of
// host, org.freedesktop.systemd1.manage-units resolves to auth_admin from the
// distro policy, so a non-root daemon cannot reload or restart anything without
// the shipped 49-trouble.rules artifact. A refusal therefore surfaces as the
// distinct policy_refused class (AccessDenied → types.ErrPolicyRefused), never as
// `transient` (a retry cannot install a policy) and never as `permanent` (nothing
// about the module is broken): the ladder escalates immediately and consumes no
// rung retry.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/totalwindupflightsystems/trouble/internal/registry/schemagen"
	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// scope values (SPEC-06 §3.9 args schemas).
const (
	scopeSystem = "system"
	scopeUser   = "user"
)

// environment returns a module's bound environment, falling back to the §4.3
// defaults when it has none. Shared by modules_service.go and modules_proc.go.
//
// The fallback exists because the env binding is not wired yet: register.go's
// bindEnv is never called, and every bind method has a value receiver, so a bound
// *moduleEnv cannot reach a module stored in the registry by value. Rather than
// panicking on a nil environment, a module runs with the documented defaults
// (registry.dbus_address="" → systemd discovery, proc.root=/proc, …). The wiring
// gap is reported in the run report; register.go is not this task's file.
func environment(e *moduleEnv) *moduleEnv {
	if e != nil {
		return e
	}
	return &moduleEnv{cfg: defaultConfig()}
}

// The systemd identity and the two D-Bus properties interfaces these modules use.
const (
	systemdBusName      = "org.freedesktop.systemd1"
	systemdManagerIface = "org.freedesktop.systemd1.Manager"
	systemdUnitIface    = "org.freedesktop.systemd1.Unit"
	systemdServiceIface = "org.freedesktop.systemd1.Service"
	dbusPropertiesIface = "org.freedesktop.DBus.Properties"

	systemdManagerPath = dbus.ObjectPath("/org/freedesktop/systemd1")

	// systemdJobMode is the `mode` argument ReloadUnit takes (§3.9: "Apply calls
	// ReloadUnit(unit, replace)"); service.restart takes its own from args.
	systemdJobMode = "replace"

	// serviceReloadProbeBudget is the §3.9 "read back within 2s" window.
	serviceReloadProbeBudget = 2 * time.Second

	// serviceRestartProbeDefaultS is registry.restart_probe_s's default (§4.3).
	serviceRestartProbeDefaultS = 10
)

// restartProbeInterval is the MainPID-change probe's poll interval. It is a seam
// so a test never sits through a whole probe window (§3.9).
var restartProbeInterval = 50 * time.Millisecond

// restartBefore remembers the MainPID observed at apply time. Verify's frozen
// signature — Verify(ctx, args), SPEC-06 §2.1 — carries no Result, so a restart
// probe cannot otherwise prove the PID moved; the registry serializes calls per
// target unit (§6.8), so one entry per unit is enough.
var restartBefore = struct {
	sync.Mutex
	pid map[string]uint32
}{pid: map[string]uint32{}}

func restartBeforeSet(unit string, pid uint32) {
	restartBefore.Lock()
	defer restartBefore.Unlock()
	restartBefore.pid[unit] = pid
}

func restartBeforeOf(unit string) (uint32, bool) {
	restartBefore.Lock()
	defer restartBefore.Unlock()
	pid, ok := restartBefore.pid[unit]
	return pid, ok
}

// unitState is one unit's observed state (§3.9 probe). ExecMainStartTS is the
// ExecMainStartTimestamp property verbatim: microseconds since the epoch, 0 when
// the unit has never run.
type unitState struct {
	Unit            string
	Path            dbus.ObjectPath
	LoadState       string
	ActiveState     string
	SubState        string
	MainPID         uint32
	ExecMainStartTS uint64
	NRestarts       uint32
}

// active reports the §3.9 verify predicate: ActiveState == "active" and SubState
// not in {failed, auto-restart}.
func (s unitState) active() bool {
	if s.ActiveState != "active" {
		return false
	}
	switch s.SubState {
	case "failed", "auto-restart":
		return false
	}
	return true
}

// serviceUnitEscape maps a unit name onto a systemd object-path element: every
// byte outside [A-Za-z0-9] becomes `_` + two lowercase hex digits, `_` included so
// the transform is round-trippable (`-` → `_2d`, §3.9).
func serviceUnitEscape(name string) string {
	const hexdigits = "0123456789abcdef"
	var b strings.Builder
	b.Grow(len(name) * 2)
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('_')
		b.WriteByte(hexdigits[c>>4])
		b.WriteByte(hexdigits[c&0x0f])
	}
	return b.String()
}

// serviceUnitPath is the fallback object path for a unit. It is only a fallback:
// systemd is authoritative about its own escaping and GetUnit's reply carries the
// path, because the escaping is version-sensitive (templates, escaped instances).
func serviceUnitPath(unit string) dbus.ObjectPath {
	return dbus.ObjectPath("/org/freedesktop/systemd1/unit/" + serviceUnitEscape(unit))
}

// userManagerSocket resolves the user manager's socket (§4.3, §3.9):
// $XDG_RUNTIME_DIR/bus for the daemon's own uid, otherwise /run/user/<uid>/bus for
// the uid named in registry.systemd_scope_user.
func userManagerSocket(scopeUser string) string {
	uid := os.Getuid()
	if scopeUser != "" {
		if v, err := strconv.Atoi(scopeUser); err == nil {
			uid = v
		}
	}
	if rd := os.Getenv("XDG_RUNTIME_DIR"); rd != "" && uid == os.Getuid() {
		return filepath.Join(rd, "bus")
	}
	return filepath.Join("/run/user", strconv.Itoa(uid), "bus")
}

// serviceDial opens a short-lived connection to the manager the scope names. An
// explicit registry.dbus_address wins over discovery (its contract: "" means
// systemd discovery), which is also the seam the conformance harness uses to
// point the modules at the in-process fake bus (§2.4). The connection is
// authenticated and Hello'd here: a private address needs both before any method
// call, exactly as a user-manager socket does.
func serviceDial(cfg config, scope string) (*dbus.Conn, error) {
	if addr := strings.TrimSpace(cfg.DbusAddress); addr != "" {
		return serviceDialAddress(addr)
	}
	if scope == scopeUser {
		return serviceDialAddress("unix:path=" + userManagerSocket(cfg.SystemdScopeUser))
	}
	return dbus.SystemBus()
}

func serviceDialAddress(address string) (*dbus.Conn, error) {
	conn, err := dbus.Dial(address)
	if err != nil {
		return nil, err
	}
	if err := conn.Auth(nil); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := conn.Hello(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// policyDenials are the D-Bus error names that mean "the policy refused", not
// "the call failed": polkit's AccessDenied from a missing/stale 49-trouble.rules
// artifact (§3.10, edge case 5).
func policyDenial(name string) bool {
	switch name {
	case "org.freedesktop.DBus.Error.AccessDenied",
		"org.freedesktop.DBus.Error.AuthFailed",
		"org.freedesktop.DBus.Error.InteractiveAuthorizationRequired",
		"org.freedesktop.PolicyKit1.Error.NotAuthorized",
		"org.freedesktop.systemd1.PermissionDenied":
		return true
	}
	return strings.Contains(name, "AccessDenied") || strings.Contains(name, "NotAuthorized")
}

// permanentDenials are the D-Bus error names that mean "the unit cannot be acted
// on, ever, as asked": not loaded, not loadable, masked.
func permanentDenial(name string) bool {
	switch name {
	case "org.freedesktop.systemd1.NoSuchUnit",
		"org.freedesktop.systemd1.LoadFailed",
		"org.freedesktop.systemd1.UnitMasked",
		"org.freedesktop.systemd1.UnitExists",
		"org.freedesktop.DBus.Error.UnknownObject",
		"org.freedesktop.DBus.Error.UnknownInterface",
		"org.freedesktop.DBus.Error.UnknownProperty":
		return true
	}
	return strings.HasPrefix(name, "org.freedesktop.systemd1.Unit")
}

// serviceDBusError maps a D-Bus failure onto the class sentinel this area returns
// (SPEC-06 §5): modules never mint a TROUBLE-REGISTRY code, they return one of the
// three sentinels and the registry attaches the code by stage. Anything unmatched
// is left transient only when it smells like a bus problem; an unclassifiable
// failure stays permanent through the registry's own default.
func serviceDBusError(op, unit string, err error) error {
	if err == nil {
		return nil
	}
	if name := dbusErrorName(err); name != "" {
		switch {
		case policyDenial(name):
			// The distinct class of §3.10: never retried, always escalated with
			// the .rules install command.
			return fmt.Errorf("%w: %s(%s): %s: install contrib/polkit/49-trouble.rules and reload polkit",
				types.ErrPolicyRefused, op, unit, name)
		case permanentDenial(name):
			return fmt.Errorf("%w: %s(%s): %s", types.ErrPermanent, op, unit, name)
		default:
			return fmt.Errorf("%w: %s(%s): %s", types.ErrTransient, op, unit, name)
		}
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		// The caller's budget ran out. Transient is the honest class (a retry may
		// still fit), and the registry's class mapping keys on the class.
		return fmt.Errorf("%w: %s(%s): %v", types.ErrTransient, op, unit, err)
	case errors.Is(err, dbus.ErrClosed), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF),
		errors.Is(err, os.ErrNotExist):
		// The bus went away or was never there: the unit is untouched.
		return fmt.Errorf("%w: %s(%s): %v", types.ErrTransient, op, unit, err)
	}
	return fmt.Errorf("%w: %s(%s): %v", types.ErrTransient, op, unit, err)
}

// dbusErrorName extracts a D-Bus error name from a call error ("" when the error
// is not a D-Bus error at all).
func dbusErrorName(err error) string {
	var value dbus.Error
	if errors.As(err, &value) {
		return value.Name
	}
	var ptr *dbus.Error
	if errors.As(err, &ptr) && ptr != nil {
		return ptr.Name
	}
	return ""
}

// serviceReadUnit reads one unit's state: GetUnit → the object path, then
// Properties.GetAll on the two interfaces that carry the §3.9 fields. MainPID,
// NRestarts and ExecMainStartTimestamp live on org.freedesktop.systemd1.Service,
// not on .Unit; a unit that has no Service interface (a target, a scope) is read
// with those three left at zero rather than failing the status call.
func serviceReadUnit(ctx context.Context, conn *dbus.Conn, unit string) (unitState, error) {
	st := unitState{Unit: unit}
	mgr := conn.Object(systemdBusName, systemdManagerPath)
	call := mgr.CallWithContext(ctx, systemdManagerIface+".GetUnit", 0, unit)
	if call.Err != nil {
		return st, serviceDBusError("GetUnit", unit, call.Err)
	}
	var path dbus.ObjectPath
	if err := call.Store(&path); err != nil {
		return st, fmt.Errorf("%w: GetUnit(%s) reply did not decode: %v", types.ErrTransient, unit, err)
	}
	st.Path = path
	if st.Path == "" {
		st.Path = serviceUnitPath(unit)
	}
	unitProps, err := serviceProperties(ctx, conn, st.Path, systemdUnitIface)
	if err != nil {
		return st, err
	}
	st.LoadState = variantString(unitProps, "LoadState")
	st.ActiveState = variantString(unitProps, "ActiveState")
	st.SubState = variantString(unitProps, "SubState")
	if svcProps, err := serviceProperties(ctx, conn, st.Path, systemdServiceIface); err == nil {
		st.MainPID = variantUint32(svcProps, "MainPID")
		st.NRestarts = variantUint32(svcProps, "NRestarts")
		st.ExecMainStartTS = variantUint64(svcProps, "ExecMainStartTimestamp")
	}
	return st, nil
}

// serviceProperties is one Properties.GetAll call.
func serviceProperties(ctx context.Context, conn *dbus.Conn, path dbus.ObjectPath, iface string) (map[string]dbus.Variant, error) {
	obj := conn.Object(systemdBusName, path)
	call := obj.CallWithContext(ctx, dbusPropertiesIface+".GetAll", 0, iface)
	if call.Err != nil {
		return nil, serviceDBusError("Properties.GetAll", iface, call.Err)
	}
	var props map[string]dbus.Variant
	if err := call.Store(&props); err != nil {
		return nil, fmt.Errorf("%w: Properties.GetAll(%s) reply did not decode: %v", types.ErrTransient, iface, err)
	}
	return props, nil
}

// serviceVerb calls one manager verb and returns the job path systemd minted
// ("" when the reply could not be read: the verb itself already landed, and
// pretending it did not is the one answer that would be wrong).
func serviceVerb(ctx context.Context, conn *dbus.Conn, member, unit, mode string) (string, error) {
	obj := conn.Object(systemdBusName, systemdManagerPath)
	call := obj.CallWithContext(ctx, systemdManagerIface+"."+member, 0, unit, mode)
	if call.Err != nil {
		return "", serviceDBusError(member, unit, call.Err)
	}
	var job dbus.ObjectPath
	if err := call.Store(&job); err != nil {
		return "", nil
	}
	return string(job), nil
}

// serviceProbe polls the §3.9 probe predicate until it holds or the budget runs
// out. The budget is the caller's deadline when that is tighter, and each sleep
// is capped by restartProbeInterval.
func serviceProbe(ctx context.Context, cfg config, scope, unit string, budget time.Duration, ok func(unitState) bool) (unitState, bool, error) {
	conn, err := serviceDial(cfg, scope)
	if err != nil {
		return unitState{}, false, serviceDBusError("dial", unit, err)
	}
	defer func() { _ = conn.Close() }()

	deadline := time.Now().Add(budget)
	if d, has := ctx.Deadline(); has && d.Before(deadline) {
		deadline = d
	}
	var last unitState
	for {
		st, err := serviceReadUnit(ctx, conn, unit)
		if err != nil {
			return st, false, err
		}
		last = st
		if ok(st) {
			return st, true, nil
		}
		if !time.Now().Before(deadline) {
			return last, false, nil
		}
		select {
		case <-ctx.Done():
			return last, false, nil
		case <-time.After(restartProbeInterval):
		}
	}
}

// serviceResult builds the Result of a read (the `pure` answer, §3.4) plus the
// unsupported rollback hint every apply carries (§3.5).
func serviceReadResult(start time.Time, output map[string]any) types.Result {
	return types.Result{
		Changed:    false,
		Output:     output,
		Rollback:   &types.RollbackHint{Supported: false, Args: map[string]any{}},
		DurationMS: int(time.Since(start).Milliseconds()),
	}
}

func serviceDuration(start time.Time) int { return int(time.Since(start).Milliseconds()) }

// variantString / variantUint32 / variantUint64 read one typed property out of a
// GetUnit/GetAll reply, tolerating the signed/unsigned variants other systemd
// builds emit.
func variantString(props map[string]dbus.Variant, key string) string {
	if v, ok := props[key]; ok {
		if s, ok := v.Value().(string); ok {
			return s
		}
	}
	return ""
}

func variantUint32(props map[string]dbus.Variant, key string) uint32 {
	v, ok := props[key]
	if !ok {
		return 0
	}
	switch n := v.Value().(type) {
	case uint32:
		return n
	case int32:
		return uint32(n)
	case uint64:
		return uint32(n)
	case int64:
		return uint32(n)
	}
	return 0
}

func variantUint64(props map[string]dbus.Variant, key string) uint64 {
	v, ok := props[key]
	if !ok {
		return 0
	}
	switch n := v.Value().(type) {
	case uint64:
		return n
	case int64:
		return uint64(n)
	case uint32:
		return uint64(n)
	case int32:
		return uint64(n)
	}
	return 0
}

type serviceStatusArgs struct {
	Unit  string `json:"unit" js:"pattern=^[A-Za-z0-9@._:\\-]{1,255}$"`
	Scope string `json:"scope,omitempty" js:"enum=system|user;default=system"`
}

type serviceReloadArgs struct {
	Unit  string `json:"unit"`
	Scope string `json:"scope,omitempty" js:"enum=system|user;default=system"`
}

type serviceRestartArgs struct {
	Unit  string `json:"unit"`
	Scope string `json:"scope,omitempty" js:"enum=system|user;default=system"`
	Mode  string `json:"mode,omitempty" js:"enum=replace|fail;default=replace"`
}

type serviceStatusModule struct{ env *moduleEnv }

var serviceStatusDescriptor = types.Descriptor{
	Name:        "service.status",
	Version:     1,
	Schema:      schemagen.Generate("service.status", 1, serviceStatusArgs{}),
	Scopes:      []string{"service:read"},
	Idempotency: types.IdemPure,
	CheckMode:   true,
	TimeoutS:    5,
	Mutating:    false,
}

func (m serviceStatusModule) bind(e *moduleEnv)            { m.env = e }
func (m serviceStatusModule) Descriptor() types.Descriptor { return serviceStatusDescriptor }

func (m serviceStatusModule) NormalizeArgs(args map[string]any) (map[string]any, error) {
	normalizeUnitArgs(args)
	return args, nil
}

func (m serviceStatusModule) ProtectedTargets(args map[string]any) []protectedTarget {
	return unitTarget(args)
}

// Check is the pure row's dry-run: nothing can be changed by reading, so the diff
// is empty and the Summary carries the observation (§3.9).
func (m serviceStatusModule) Check(ctx context.Context, args map[string]any) (types.Diff, error) {
	env := environment(m.env)
	var a serviceStatusArgs
	if err := decodeArgs(args, &a); err != nil {
		return types.Diff{}, err
	}
	unit, scope := normalizeUnit(a.Unit), serviceScope(a.Scope)
	conn, err := serviceDial(env.cfg, scope)
	if err != nil {
		return types.Diff{}, serviceDBusError("dial", unit, err)
	}
	defer func() { _ = conn.Close() }()
	st, err := serviceReadUnit(ctx, conn, unit)
	if err != nil {
		return types.Diff{}, err
	}
	return types.Diff{Empty: true, Summary: serviceStatusSummary(st)}, nil
}

// Apply is a read (§3.4 `pure`): Changed=false, the payload in Output.
func (m serviceStatusModule) Apply(ctx context.Context, args map[string]any) (types.Result, error) {
	start := time.Now()
	env := environment(m.env)
	var a serviceStatusArgs
	if err := decodeArgs(args, &a); err != nil {
		return types.Result{}, err
	}
	unit, scope := normalizeUnit(a.Unit), serviceScope(a.Scope)
	conn, err := serviceDial(env.cfg, scope)
	if err != nil {
		return types.Result{}, serviceDBusError("dial", unit, err)
	}
	defer func() { _ = conn.Close() }()
	st, err := serviceReadUnit(ctx, conn, unit)
	if err != nil {
		return types.Result{}, err
	}
	return serviceReadResult(start, serviceStatusOutput(st)), nil
}

// Verify is a probe: the unit is read again and the observation is the evidence
// (§3.9). A unit that cannot be read at all is an error, not a silent ok.
func (m serviceStatusModule) Verify(ctx context.Context, args map[string]any) (types.VerifyResult, error) {
	env := environment(m.env)
	var a serviceStatusArgs
	if err := decodeArgs(args, &a); err != nil {
		return types.VerifyResult{}, err
	}
	unit, scope := normalizeUnit(a.Unit), serviceScope(a.Scope)
	conn, err := serviceDial(env.cfg, scope)
	if err != nil {
		return types.VerifyResult{}, serviceDBusError("dial", unit, err)
	}
	defer func() { _ = conn.Close() }()
	st, err := serviceReadUnit(ctx, conn, unit)
	if err != nil {
		return types.VerifyResult{}, err
	}
	return types.VerifyResult{OK: true, Method: "probe", Detail: serviceStatusOutput(st)}, nil
}

// serviceStatusOutput is the §3.9 Output: the five named fields, plus the unit the
// read was for.
func serviceStatusOutput(st unitState) map[string]any {
	return map[string]any{
		"unit":               st.Unit,
		"active_state":       st.ActiveState,
		"sub_state":          st.SubState,
		"main_pid":           st.MainPID,
		"exec_main_start_ts": st.ExecMainStartTS,
		"n_restarts":         st.NRestarts,
		"load_state":         st.LoadState,
		"unit_object_path":   string(st.Path),
	}
}

// serviceStatusSummary is the one-line observation of a pure read.
func serviceStatusSummary(st unitState) string {
	return fmt.Sprintf("active_state=%s sub_state=%s main_pid=%d n_restarts=%d unit=%s",
		st.ActiveState, st.SubState, st.MainPID, st.NRestarts, st.Unit)
}

// serviceScope applies the args default (§3.9: scope defaults to system).
func serviceScope(scope string) string {
	if scope == scopeUser {
		return scopeUser
	}
	return scopeSystem
}

type serviceReloadModule struct{ env *moduleEnv }

var serviceReloadDescriptor = types.Descriptor{
	Name:        "service.reload",
	Version:     1,
	Schema:      schemagen.Generate("service.reload", 1, serviceReloadArgs{}),
	Scopes:      []string{"service:write"},
	Idempotency: types.IdemOnce,
	CheckMode:   true,
	TimeoutS:    20,
	Mutating:    true,
}

func (m serviceReloadModule) bind(e *moduleEnv)            { m.env = e }
func (m serviceReloadModule) Descriptor() types.Descriptor { return serviceReloadDescriptor }
func (m serviceReloadModule) RollbackInvertible() bool     { return false }

func (m serviceReloadModule) NormalizeArgs(args map[string]any) (map[string]any, error) {
	normalizeUnitArgs(args)
	return args, nil
}

func (m serviceReloadModule) ProtectedTargets(args map[string]any) []protectedTarget {
	return unitTarget(args)
}

// serviceReloadDiff is the §3.9 dry-run predicate: a unit that is not active has
// nothing to reload, so the diff is empty and the runner skips the task as ok.
// The entry is present exactly when the diff is not empty — a skipped task must
// predict zero changes (§2.4 obligation 2), and no other shipped module predicts
// entries it will not apply.
func serviceReloadDiff(unit string, st unitState) types.Diff {
	d := types.Diff{
		Empty:   !st.active(),
		Summary: fmt.Sprintf("reload %s: active_state=%s sub_state=%s", unit, st.ActiveState, st.SubState),
	}
	if !d.Empty {
		d.Entries = []types.DiffEntry{{
			Path:   "unit:" + unit + "#reload",
			Before: st.SubState,
			After:  "reloading",
		}}
	}
	return d
}

// Check never mutates: it reads the unit and returns the predicate above.
func (m serviceReloadModule) Check(ctx context.Context, args map[string]any) (types.Diff, error) {
	env := environment(m.env)
	var a serviceReloadArgs
	if err := decodeArgs(args, &a); err != nil {
		return types.Diff{}, err
	}
	unit, scope := normalizeUnit(a.Unit), serviceScope(a.Scope)
	conn, err := serviceDial(env.cfg, scope)
	if err != nil {
		return types.Diff{}, serviceDBusError("dial", unit, err)
	}
	defer func() { _ = conn.Close() }()
	st, err := serviceReadUnit(ctx, conn, unit)
	if err != nil {
		return types.Diff{}, err
	}
	return serviceReloadDiff(unit, st), nil
}

// Apply calls ReloadUnit(unit, replace). Idempotency is `once`: the registry's
// IdemKey replay guard is the only dedup (§3.4), so Apply is honest about Changed
// and does not invent a second one. A unit that is not active is not reloaded —
// that is the same predicate Check reported, and reporting Changed=true for a
// reload that meant nothing would be a lie.
func (m serviceReloadModule) Apply(ctx context.Context, args map[string]any) (types.Result, error) {
	start := time.Now()
	env := environment(m.env)
	var a serviceReloadArgs
	if err := decodeArgs(args, &a); err != nil {
		return types.Result{}, err
	}
	unit, scope := normalizeUnit(a.Unit), serviceScope(a.Scope)
	conn, err := serviceDial(env.cfg, scope)
	if err != nil {
		return types.Result{}, serviceDBusError("dial", unit, err)
	}
	defer func() { _ = conn.Close() }()

	st, err := serviceReadUnit(ctx, conn, unit)
	if err != nil {
		return types.Result{}, err
	}
	diff := serviceReloadDiff(unit, st)
	if diff.Empty {
		return types.Result{
			Changed: false,
			Output: map[string]any{
				"unit": unit, "reloaded": false, "reason": "unit_not_active",
				"active_state": st.ActiveState, "sub_state": st.SubState, "main_pid": st.MainPID,
			},
			Rollback:   &types.RollbackHint{Supported: false, Args: map[string]any{}},
			DurationMS: serviceDuration(start),
		}, nil
	}
	job, err := serviceVerb(ctx, conn, "ReloadUnit", unit, systemdJobMode)
	if err != nil {
		return types.Result{}, err
	}
	return types.Result{
		Changed: true,
		Applied: diff.Entries,
		Output: map[string]any{
			"unit": unit, "reloaded": true, "job": job,
			"before_sub_state": st.SubState, "active_state": st.ActiveState, "main_pid": st.MainPID,
		},
		Rollback:   &types.RollbackHint{Supported: false, Args: map[string]any{}},
		DurationMS: serviceDuration(start),
	}, nil
}

// Verify is the §3.9 probe: GetUnit → ActiveState=="active" and SubState not in
// {failed, auto-restart}, read back within 2s.
func (m serviceReloadModule) Verify(ctx context.Context, args map[string]any) (types.VerifyResult, error) {
	env := environment(m.env)
	var a serviceReloadArgs
	if err := decodeArgs(args, &a); err != nil {
		return types.VerifyResult{}, err
	}
	unit, scope := normalizeUnit(a.Unit), serviceScope(a.Scope)
	st, ok, err := serviceProbe(ctx, env.cfg, scope, unit, serviceReloadProbeBudget, unitState.active)
	if err != nil {
		return types.VerifyResult{}, err
	}
	detail := map[string]any{
		"unit":            unit,
		"active_state":    st.ActiveState,
		"sub_state":       st.SubState,
		"main_pid":        st.MainPID,
		"active_expected": true,
		"probe_window_s":  serviceReloadProbeBudget.Seconds(),
	}
	if !ok {
		detail["reason"] = "unit is not active after the reload"
	}
	return types.VerifyResult{OK: ok, Method: "probe", Detail: detail}, nil
}

type serviceRestartModule struct{ env *moduleEnv }

var serviceRestartDescriptor = types.Descriptor{
	Name:        "service.restart",
	Version:     1,
	Schema:      schemagen.Generate("service.restart", 1, serviceRestartArgs{}),
	Scopes:      []string{"service:write"},
	Idempotency: types.IdemOnce,
	CheckMode:   true,
	TimeoutS:    25,
	Mutating:    true,
}

func (m serviceRestartModule) bind(e *moduleEnv)            { m.env = e }
func (m serviceRestartModule) Descriptor() types.Descriptor { return serviceRestartDescriptor }
func (m serviceRestartModule) RollbackInvertible() bool     { return false }

func (m serviceRestartModule) NormalizeArgs(args map[string]any) (map[string]any, error) {
	normalizeUnitArgs(args)
	return args, nil
}

func (m serviceRestartModule) ProtectedTargets(args map[string]any) []protectedTarget {
	return unitTarget(args)
}

// serviceRestartDiff is the reload shape with the restart marker: `after` carries
// the MainPID observed when the diff was computed. The predicate is the same as
// reload's (§3.9 "same shape") — a unit that is not active has no running process
// to restart, so the runner skips the task.
func serviceRestartDiff(unit string, st unitState) types.Diff {
	d := types.Diff{
		Empty: !st.active(),
		Summary: fmt.Sprintf("restart %s: active_state=%s sub_state=%s main_pid=%d",
			unit, st.ActiveState, st.SubState, st.MainPID),
	}
	if !d.Empty {
		d.Entries = []types.DiffEntry{{
			Path:   "unit:" + unit + "#restart",
			Before: st.SubState,
			After:  fmt.Sprintf("restarted:%d", st.MainPID),
		}}
	}
	return d
}

// Check reads the unit and reports the predicate; it never mutates.
func (m serviceRestartModule) Check(ctx context.Context, args map[string]any) (types.Diff, error) {
	env := environment(m.env)
	var a serviceRestartArgs
	if err := decodeArgs(args, &a); err != nil {
		return types.Diff{}, err
	}
	unit, scope := normalizeUnit(a.Unit), serviceScope(a.Scope)
	conn, err := serviceDial(env.cfg, scope)
	if err != nil {
		return types.Diff{}, serviceDBusError("dial", unit, err)
	}
	defer func() { _ = conn.Close() }()
	st, err := serviceReadUnit(ctx, conn, unit)
	if err != nil {
		return types.Diff{}, err
	}
	return serviceRestartDiff(unit, st), nil
}

// Apply calls RestartUnit(unit, mode) and remembers the pre-restart MainPID for
// Verify. The Applied entry carries the MainPID the restart actually minted: a
// `once` module's diff is not a convergent promise, and the post-state is the
// honest value to report (§3.4, §3.9 probe).
func (m serviceRestartModule) Apply(ctx context.Context, args map[string]any) (types.Result, error) {
	start := time.Now()
	env := environment(m.env)
	var a serviceRestartArgs
	if err := decodeArgs(args, &a); err != nil {
		return types.Result{}, err
	}
	unit, scope := normalizeUnit(a.Unit), serviceScope(a.Scope)
	mode := a.Mode
	if mode == "" {
		mode = systemdJobMode
	}
	conn, err := serviceDial(env.cfg, scope)
	if err != nil {
		return types.Result{}, serviceDBusError("dial", unit, err)
	}
	defer func() { _ = conn.Close() }()

	st, err := serviceReadUnit(ctx, conn, unit)
	if err != nil {
		return types.Result{}, err
	}
	diff := serviceRestartDiff(unit, st)
	if diff.Empty {
		return types.Result{
			Changed: false,
			Output: map[string]any{
				"unit": unit, "restarted": false, "reason": "unit_not_active",
				"active_state": st.ActiveState, "sub_state": st.SubState, "main_pid": st.MainPID,
			},
			Rollback:   &types.RollbackHint{Supported: false, Args: map[string]any{}},
			DurationMS: serviceDuration(start),
		}, nil
	}
	// Remember the "before" MainPID before the verb, so Verify can require the
	// change itself rather than assume it (§3.9: "the restart actually happened").
	restartBeforeSet(unit, st.MainPID)
	job, err := serviceVerb(ctx, conn, "RestartUnit", unit, mode)
	if err != nil {
		return types.Result{}, err
	}
	applied := append([]types.DiffEntry(nil), diff.Entries...)
	if after, readErr := serviceReadUnit(ctx, conn, unit); readErr == nil && len(applied) == 1 {
		applied[0].After = fmt.Sprintf("restarted:%d", after.MainPID)
		applied[0].Before = st.SubState
	}
	return types.Result{
		Changed: true,
		Applied: applied,
		Output: map[string]any{
			"unit": unit, "restarted": true, "job": job, "mode": mode,
			"before_sub_state": st.SubState, "main_pid_before": st.MainPID,
		},
		Rollback:   &types.RollbackHint{Supported: false, Args: map[string]any{}},
		DurationMS: serviceDuration(start),
	}, nil
}

// Verify is the §3.9 restart probe: active and not failed, plus a MainPID that
// actually changed within registry.restart_probe_s. When no apply was observed
// (a replayed `once` call, a resumed run) there is no "before" value to compare
// against, and the probe reports that it could not observe the change instead of
// inventing one.
func (m serviceRestartModule) Verify(ctx context.Context, args map[string]any) (types.VerifyResult, error) {
	env := environment(m.env)
	var a serviceRestartArgs
	if err := decodeArgs(args, &a); err != nil {
		return types.VerifyResult{}, err
	}
	unit, scope := normalizeUnit(a.Unit), serviceScope(a.Scope)
	before, remembered := restartBeforeOf(unit)
	budget := serviceRestartBudget(env.cfg.RestartProbeS)
	st, ok, err := serviceProbe(ctx, env.cfg, scope, unit, budget, func(s unitState) bool {
		if !s.active() {
			return false
		}
		if !remembered {
			return true
		}
		return s.MainPID != before
	})
	if err != nil {
		return types.VerifyResult{}, err
	}
	changed := remembered && st.MainPID != before
	detail := map[string]any{
		"unit":             unit,
		"active_state":     st.ActiveState,
		"sub_state":        st.SubState,
		"main_pid":         st.MainPID,
		"main_pid_before":  before,
		"main_pid_changed": changed,
		"pid_change_seen":  remembered,
		"probe_window_s":   budget.Seconds(),
	}
	switch {
	case !ok && remembered && st.active() && st.MainPID == before:
		detail["reason"] = "MainPID did not change within the probe window"
	case !ok:
		detail["reason"] = "unit is not active after the restart"
	}
	return types.VerifyResult{OK: ok, Method: "probe", Detail: detail}, nil
}

// serviceRestartBudget is registry.restart_probe_s with its default applied.
func serviceRestartBudget(seconds int) time.Duration {
	if seconds <= 0 {
		seconds = serviceRestartProbeDefaultS
	}
	return time.Duration(seconds) * time.Second
}
