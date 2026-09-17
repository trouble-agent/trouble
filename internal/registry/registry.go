// Package registry is the daemon's entire action surface (SPEC-06). Every
// state-changing act trouble can perform is one typed tool call through this
// package: authorize → validate → dry-run → apply → verify → audit. There is no
// second path — no module shells out, and no registered module can name an
// arbitrary command.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/registry/validate"
	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// scrubTarget is the SPEC-02 content class used for the ledger copy of a call's
// args. SPEC-02 §3.1 does not carry a tool-call row, and args are message-shaped
// text, so the generic message class is used; the choice is recorded in the
// SPEC-06 run report.
const scrubTarget = string(types.TgEventMsg)

const reasonScrubRefused = "scrub_refused"

// targetLockBudget is the per-target mutex acquire budget (SPEC-06 §6.8).
const targetLockBudget = 5 * time.Second

// RegistryDeps is the wiring surface (SPEC-06 §2.2). Every collaborator is a
// function value, so no import cycle and no cross-package interface is created:
// internal/registry never imports internal/ladder, internal/flow or internal/issues.
type RegistryDeps struct {
	Append         func(rec types.Record) (types.Record, error)
	Scrub          func(target string, in []byte) (types.ScrubResult, error)
	Gates          func() types.AutonomyGates
	FileIssue      func(ctx context.Context, a map[string]any) (map[string]any, error)
	CreateTask     func(ctx context.Context, a map[string]any) (map[string]any, error)
	Comment        func(ctx context.Context, a map[string]any) (map[string]any, error)
	SkillAuthorize func(ctx context.Context, skillID, module string, scopes []string) error
	DbusAddress    string
	Now            func() time.Time

	// Two additions to the §2.2 list, both required by that section's own
	// consumer contract and recorded in the run report:
	//  - Config carries the resolved §4.3 values; without it no registry.*/file.*
	//    /proc.* key listed in §4.3 has an injection path at all.
	//  - HostID/Actor supply the Origin/Actor SPEC-01 requires on every record.
	Config []types.ConfigValue
	HostID string
	Actor  types.Actor
}

// Registry is the module table plus the six-stage call contract.
type Registry struct {
	deps RegistryDeps
	cfg  config
	dnt  types.DoNotTouch

	mu      sync.RWMutex
	modules map[string]types.Module
	names   []string

	// idem is the replay guard for `once` modules (SPEC-06 §3.4): the last
	// cfg.IdemWindow (module, IdemKey) pairs, rebuilt at boot from play_run and
	// tool_call records by the composition root through NoteExecuted.
	idemMu    sync.Mutex
	idem      map[string]bool
	idemOrder []string

	// target locks serialize calls against the same canonical path/unit (§6.8).
	lockMu  sync.Mutex
	targets map[string]chan struct{}

	// capability is the polkit probe result (SPEC-06 §3.10).
	capMu       sync.RWMutex
	capChecked  bool
	capPresent  bool
	capRulesOK  bool
	capReason   string
	capUnitsOK  []string
	capVerbs    []string
	probeRecord string

	// warned carries boot-time refusals that must stay visible without failing
	// the boot (a weakening do-not-touch file, an unreadable rules file).
	warned []bootWarning

	// sleeper is the retry backoff; tests replace it so no test waits.
	sleeper func(time.Duration)
}

// SetSleeper replaces the retry backoff (tests only).
func (r *Registry) SetSleeper(f func(time.Duration)) { r.sleeper = f }

// bootWarning is a recorded, non-fatal boot refusal.
type bootWarning struct {
	Code   types.ErrorCode
	Reason string
	Detail string
}

func (w bootWarning) Error() string { return string(w.Code) + ": " + w.Reason + ": " + w.Detail }

// Warnings returns the boot-time refusals recorded so far (SPEC-06 §3.6 rule 2:
// the daemon continues with maximum enforcement).
func (r *Registry) Warnings() []bootWarning {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]bootWarning, len(r.warned))
	copy(out, r.warned)
	return out
}

// New builds the registry over the shipped module set (SPEC-06 §2.2).
func New(deps RegistryDeps) (*Registry, error) {
	return NewWith(deps, shippedModules())
}

// NewWith builds the registry over an explicit module set. It exists so the
// conformance harness can register the deliberate negative modules and so
// cmd/troubled can add a module without editing the package.
func NewWith(deps RegistryDeps, mods []types.Module) (*Registry, error) {
	if deps.Append == nil {
		return nil, newErr(types.CodeRegistry014, "boot", reasonDescriptorInvalid, "RegistryDeps.Append is required (the ledger is the only writer)")
	}
	if deps.Scrub == nil {
		return nil, newErr(types.CodeRegistry014, "boot", reasonDescriptorInvalid, "RegistryDeps.Scrub is required (args are never persisted unscrubbed)")
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	cfg, err := decodeConfig(deps.Config)
	if err != nil {
		return nil, err
	}
	if deps.DbusAddress != "" {
		cfg.DbusAddress = deps.DbusAddress
	}
	r := &Registry{
		deps:    deps,
		cfg:     cfg,
		modules: map[string]types.Module{},
		idem:    map[string]bool{},
		targets: map[string]chan struct{}{},
	}
	dnt, warn := loadDoNotTouchFile(cfg.DoNotTouchFile, cfg.StateRoot)
	r.dnt = mergeDoNotTouch(FloorDoNotTouch(cfg.StateRoot), dnt, cfg.DNTPathsExtra, cfg.DNTUnitsExtra, cfg.DNTScopesExtra)
	if warn != nil {
		// A weakening or malformed file never degrades enforcement: the floor
		// stands and the refusal is recorded (SPEC-06 §3.6 rule 2, §6.6).
		r.warned = append(r.warned, *warn)
	}
	for _, m := range mods {
		if err := r.Register(m); err != nil {
			return nil, err
		}
	}
	// The committed-artifact check covers the shipped set: those are the
	// descriptors `make schema` writes and CI diffs. A module registered from
	// outside the package (a test double, a future extension) has no committed
	// artifact to compare against, so it is validated by its descriptor alone.
	if err := VerifyCommittedSchemas(shippedNames(r.names), func(name string) (types.Descriptor, bool) {
		d, err := r.Descriptor(name)
		return d, err == nil
	}); err != nil {
		return nil, err
	}
	return r, nil
}

// warned carries boot-time refusals that must be visible without failing the boot.
// shippedNames filters a module-name list down to the shipped set.
func shippedNames(names []string) []string {
	shipped := map[string]bool{}
	for _, d := range ShippedDescriptors() {
		shipped[d.Name] = true
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if shipped[n] {
			out = append(out, n)
		}
	}
	return out
}

// Register validates a descriptor and adds the module to the table.
// A mutating module with CheckMode=false is refused with TROUBLE-REGISTRY-014 at
// registration, not at call time (SPEC-06 §3.2, AC-23 condition 2).
func (r *Registry) Register(m types.Module) error {
	if m == nil {
		return newErr(types.CodeRegistry014, "boot", reasonDescriptorInvalid, "nil module")
	}
	d := m.Descriptor()
	if err := validateDescriptor(d, m); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.modules[d.Name]; dup {
		return newErr(types.CodeRegistry014, "boot", reasonDescriptorInvalid, "module %s registered twice", d.Name)
	}
	r.modules[d.Name] = m
	r.names = append(r.names, d.Name)
	sort.Strings(r.names)
	return nil
}

// validateDescriptor enforces the §3.2 descriptor contract.
func validateDescriptor(d types.Descriptor, m types.Module) error {
	if d.Name == "" {
		return newErr(types.CodeRegistry014, "boot", reasonDescriptorInvalid, "descriptor without a name")
	}
	if d.Version <= 0 {
		return newErr(types.CodeRegistry014, "boot", reasonDescriptorInvalid, "%s: version must be a monotonic int > 0", d.Name)
	}
	if len(d.Schema) == 0 {
		return newErr(types.CodeRegistry014, "boot", reasonDescriptorInvalid, "%s: descriptor without a schema", d.Name)
	}
	if got, _ := d.Schema["$schema"].(string); got != schemagenDialect {
		return newErr(types.CodeRegistry014, "boot", reasonDescriptorInvalid, "%s: schema dialect is %q, want %q", d.Name, got, schemagenDialect)
	}
	if err := validate.DialectClosed(d.Schema); err != nil {
		return newErr(types.CodeRegistry014, "boot", reasonDescriptorInvalid, "%s: %v", d.Name, err)
	}
	if len(d.Scopes) == 0 {
		return newErr(types.CodeRegistry014, "boot", reasonDescriptorInvalid, "%s: descriptor without scopes (deny by default)", d.Name)
	}
	if !d.Idempotency.Valid() {
		return newErr(types.CodeRegistry014, "boot", reasonDescriptorInvalid, "%s: unknown idempotency class %q", d.Name, d.Idempotency)
	}
	if d.Mutating && !d.CheckMode {
		return newErr(types.CodeRegistry014, "boot", reasonDescriptorInvalid, "%s: mutating module declares check_mode=false (AC-23 condition 2)", d.Name)
	}
	if d.TimeoutS <= 0 {
		return newErr(types.CodeRegistry014, "boot", reasonDescriptorInvalid, "%s: timeout_s must be > 0", d.Name)
	}
	if err := noArbitraryCommandKeys(d.Schema); err != nil {
		return err
	}
	return nil
}

// noArbitraryCommandKeys refuses any registered module that accepts a command,
// cmd, argv, shell or script args key (SPEC-06 §3.11, TestNoArbitraryCommandModule).
func noArbitraryCommandKeys(schema map[string]any) error {
	props, _ := schema["properties"].(map[string]any)
	for _, banned := range []string{"command", "cmd", "argv", "shell", "script"} {
		if _, ok := props[banned]; ok {
			return newErr(types.CodeRegistry014, "boot", reasonDescriptorInvalid, "module declares a %q args key: no module may accept an arbitrary command", banned)
		}
	}
	return nil
}

// List returns the stable, name-sorted inventory (SPEC-06 §2.2).
func (r *Registry) List() []types.Descriptor {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]types.Descriptor, 0, len(r.names))
	for _, n := range r.names {
		out = append(out, r.modules[n].Descriptor())
	}
	return out
}

// Descriptor returns one module's descriptor.
func (r *Registry) Descriptor(name string) (types.Descriptor, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.modules[name]
	if !ok {
		return types.Descriptor{}, newErr(types.CodeRegistry001, types.StageAuthorize, reasonModuleUnavailable, "unknown module %q", name)
	}
	return m.Descriptor(), nil
}

// module returns a registered module ("" ok=false when unknown).
func (r *Registry) module(name string) (types.Module, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.modules[name]
	return m, ok
}

// Modules exposes the registered module set to the conformance harness. The
// testkit lives outside this package, so a read-only accessor is required; it is
// not part of the daemon's action surface.
func (r *Registry) Modules() map[string]types.Module {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]types.Module, len(r.modules))
	for k, v := range r.modules {
		out[k] = v
	}
	return out
}

// NoteExecuted seeds the `once` replay guard from the ledger at boot
// (SPEC-06 §3.4: rebuilt from play_run/tool_call records).
func (r *Registry) NoteExecuted(module, idemKey string) {
	if idemKey == "" {
		return
	}
	r.idemMu.Lock()
	defer r.idemMu.Unlock()
	r.noteLocked(module, idemKey)
}

func (r *Registry) noteLocked(module, idemKey string) {
	key := module + "\x00" + idemKey
	if r.idem[key] {
		return
	}
	r.idem[key] = true
	r.idemOrder = append(r.idemOrder, key)
	if max := r.cfg.IdemWindow; max > 0 && len(r.idemOrder) > max {
		drop := r.idemOrder[0]
		r.idemOrder = r.idemOrder[1:]
		delete(r.idem, drop)
	}
}

func (r *Registry) executed(module, idemKey string) bool {
	if idemKey == "" {
		return false
	}
	r.idemMu.Lock()
	defer r.idemMu.Unlock()
	return r.idem[module+"\x00"+idemKey]
}

// Call runs the six-stage contract for one tool call (SPEC-06 §2.3, AC-7).
//
// The returned ToolCall is the in-memory copy of the audit record that was
// appended; it is never nil on a completed call, including refusals. The
// returned error is non-nil only when nothing was appended (the audit-stage
// failure); a refusal or a module failure is data on the record — callers that
// want it as an error use Outcome.
func (r *Registry) Call(ctx context.Context, req types.ToolCallRequest) (types.ToolCall, error) {
	start := r.deps.Now()
	tc := types.ToolCall{
		ID:     types.NewID(types.PEv),
		Module: req.Module,
		Mode:   req.Mode,
		Inc:    req.Inc,
		Args:   req.Args,
		Actor:  req.Actor,
	}
	if tc.Mode == "" {
		tc.Mode = types.ModeCheck
	}
	if tc.Actor.Kind == "" {
		tc.Actor = r.deps.Actor
	}
	stages := make([]types.CallStage, 0, 6)

	// Stage 1 — authorize (a1 module exists … a7 autonomy gate).
	mod, dec, aerr := r.authorize(ctx, req)
	st := types.CallStage{Stage: types.StageAuthorize, OK: aerr == nil}
	if aerr != nil {
		st.Detail = stageDetail(aerr)
		stages = append(stages, st)
		tc.Stage = stages
		tc.ErrorCode = string(aerr.Code)
		tc.LatencyMS = int(r.deps.Now().Sub(start).Milliseconds())
		return r.finish(ctx, tc, false, aerr)
	}
	st.Detail = fmt.Sprintf("mode=%s grants=%d", dec.mode, len(dec.grants))
	stages = append(stages, st)
	tc.Mode = dec.mode

	// Stage 2 — validate (schema + typed normalization; no target IO).
	args, verr := r.validateStage(mod, req.Args)
	st = types.CallStage{Stage: types.StageValidate}
	if verr != nil {
		st.Detail = stageDetail(verr)
		tc.ErrorCode = string(verr.Code)
	} else {
		st.OK = true
		st.Detail = "typed args accepted"
		tc.Args = args
	}
	stages = append(stages, st)
	if verr != nil {
		tc.Stage = stages
		tc.LatencyMS = int(r.deps.Now().Sub(start).Milliseconds())
		return r.finish(ctx, tc, false, verr)
	}

	// Per-target serialization (§6.8): one call at a time per canonical target.
	unlock, lerr := r.lockTargets(mod, args)
	if lerr != nil {
		stages = append(stages, types.CallStage{Stage: types.StageDryRun, OK: false, Detail: stageDetail(lerr)})
		tc.Stage = stages
		tc.ErrorCode = string(lerr.Code)
		tc.LatencyMS = int(r.deps.Now().Sub(start).Milliseconds())
		return r.finish(ctx, tc, false, lerr)
	}
	defer unlock()

	// Stage 3 — dry-run.
	cctx, cancel := context.WithTimeout(ctx, r.effectiveTimeout(mod, req.DeadlineS))
	defer cancel()
	diff, derr := mod.Check(cctx, args)
	st = types.CallStage{Stage: types.StageDryRun}
	var derr2 *Error
	if derr != nil {
		derr2 = moduleError(types.StageDryRun, derr)
		st.Detail = stageDetail(derr2)
		tc.ErrorCode = string(derr2.Code)
	} else {
		st.OK = true
		st.Detail = diffSummary(diff)
		tc.CheckDiff = &diff
	}
	stages = append(stages, st)
	if derr2 != nil {
		tc.Stage = stages
		tc.LatencyMS = int(r.deps.Now().Sub(start).Milliseconds())
		return r.finish(ctx, tc, false, derr2)
	}

	// check_mode stops here: stages 1, 2, 3 and 6 (4 entries, no apply/verify).
	if tc.Mode == types.ModeCheck {
		stages = append(stages, types.CallStage{Stage: types.StageApply, OK: true, Detail: "not run (check_mode)"},
			types.CallStage{Stage: types.StageVerify, OK: true, Detail: "not run (check_mode)"})
		tc.Stage = stages
		tc.LatencyMS = int(r.deps.Now().Sub(start).Milliseconds())
		return r.finish(ctx, tc, false, nil)
	}

	// apply mode: the intent record lands before any mutation (SPEC-06 §4.2).
	if _, ierr := r.appendCall(ctx, tc, stages, true, nil); ierr != nil {
		stages = append(stages, types.CallStage{Stage: types.StageApply, OK: false, Detail: "intent append failed"},
			types.CallStage{Stage: types.StageVerify, OK: false, Detail: "not run"})
		tc.Stage = stages
		e := wrapErr(types.CodeRegistry006, types.StageAudit, reasonAuditAppendFailed, ierr)
		tc.ErrorCode = string(e.Code)
		return r.finish(ctx, tc, false, e)
	}

	d := mod.Descriptor()
	var res types.Result
	var aerr2 *Error
	st = types.CallStage{Stage: types.StageApply}

	switch {
	case r.executed(req.Module, req.IdemKey) && d.Idempotency == types.IdemOnce:
		// A replay of the same IdemKey is a no-op: the module is never invoked.
		st.OK = true
		st.Detail = "replayed"
		res = types.Result{Changed: false}
	case d.Mutating && diff.Empty:
		// Already applied (convergent) or nothing to do: never mutate blind.
		st.OK = true
		st.Detail = "skipped: diff empty"
		res = types.Result{Changed: false, Output: map[string]any{}}
	default:
		actx, acancel := context.WithTimeout(ctx, r.effectiveTimeout(mod, req.DeadlineS))
		out, err := mod.Apply(actx, args)
		acancel()
		if err != nil {
			aerr2 = moduleError(types.StageApply, err)
			st.Detail = stageDetail(aerr2)
		} else {
			st.OK = true
			res = out
			st.Detail = appliedSummary(out)
		}
	}
	stages = append(stages, st)
	if aerr2 != nil {
		tc.Stage = stages
		tc.Result = &res
		tc.ErrorCode = string(aerr2.Code)
		tc.LatencyMS = int(r.deps.Now().Sub(start).Milliseconds())
		if d.Idempotency == types.IdemOnce {
			r.NoteExecuted(req.Module, req.IdemKey)
		}
		return r.finish(ctx, tc, false, aerr2)
	}
	tc.Result = &res
	if res.Changed || !d.Mutating {
		if d.Idempotency == types.IdemOnce {
			r.NoteExecuted(req.Module, req.IdemKey)
		}
	}

	// Stage 5 — verify.
	st = types.CallStage{Stage: types.StageVerify}
	vctx, vcancel := context.WithTimeout(ctx, r.effectiveTimeout(mod, req.DeadlineS))
	vr, verr2 := mod.Verify(vctx, args)
	vcancel()
	if verr2 != nil {
		e := moduleError(types.StageVerify, verr2)
		st.Detail = stageDetail(e)
		tc.ErrorCode = string(e.Code)
		if rollbackAvailable(res) {
			_ = r.Rollback(ctx, tc)
			tc.Rolled = true
		}
	} else {
		st.OK = vr.OK
		st.Detail = verifySummary(vr)
		tc.Verify = &vr
		if !vr.OK {
			e := newErr(types.CodeRegistry005, types.StageVerify, "", "verify not ok for %s", req.Module)
			st.Detail = stageDetail(e)
			tc.ErrorCode = string(e.Code)
			if rollbackAvailable(res) {
				_ = r.Rollback(ctx, tc)
				tc.Rolled = true
			}
		}
	}
	stages = append(stages, st)
	tc.Stage = stages
	tc.LatencyMS = int(r.deps.Now().Sub(start).Milliseconds())

	// Stage 6 — audit.
	return r.finish(ctx, tc, true, outcomeOf(tc))
}

// Outcome converts a completed call's in-memory audit record into the error the
// caller should classify: "" code means success. This is the ladder's view of the
// contract (SPEC-05 §2 PlayRunner); it exists so Call can stay record-first
// (SPEC-06 §2.2) while consumers still key retries on the class (SPEC-06 §5).
func Outcome(tc types.ToolCall) error {
	if tc.ErrorCode == "" {
		return nil
	}
	code := types.ErrorCode(tc.ErrorCode)
	class, ok := types.CodeClass[code]
	if !ok {
		class = types.ErrClassPermanent
	}
	stage := types.StageApply
	if len(tc.Stage) > 0 {
		stage = tc.Stage[len(tc.Stage)-1].Stage
		if tc.Stage[0].OK && len(tc.Stage) > 2 && !tc.Stage[2].OK {
			stage = types.StageDryRun
		}
	}
	detail := ""
	for _, s := range tc.Stage {
		if !s.OK && s.Detail != "" {
			detail = s.Detail
			break
		}
	}
	return &Error{Code: code, Class: class, Stage: stage, Detail: detail}
}

func outcomeOf(tc types.ToolCall) *Error {
	if e, ok := Outcome(tc).(*Error); ok {
		return e
	}
	return nil
}

// finish appends the terminal (or intent) record and returns the pair the
// contract promises.
func (r *Registry) finish(ctx context.Context, tc types.ToolCall, appliedIntent bool, e *Error) (types.ToolCall, error) {
	rec, err := r.appendCall(ctx, tc, tc.Stage, false, e)
	if err != nil {
		ups := ledgerCodeOf(err)
		tc.Stage = append(tc.Stage, types.CallStage{Stage: types.StageAudit, OK: false, Detail: "audit append failed: " + ups})
		if tc.ErrorCode == "" {
			tc.ErrorCode = string(types.CodeRegistry006)
		}
		failed := wrapErr(types.CodeRegistry006, types.StageAudit, reasonAuditAppendFailed, err)
		failed.Detail = fmt.Sprintf("audit_failed=true upstream_code=%s", ups)
		return tc, failed
	}
	tc.Stage = append(tc.Stage, types.CallStage{Stage: types.StageAudit, OK: true, Detail: "kind=tool_call rec=" + rec.RecID})
	return tc, nil
}

// appendCall writes one tool_call record: scrubbed args, the stage list, the
// error code/class and the run identity (SPEC-06 §2.3 stage 6, §4.4).
func (r *Registry) appendCall(ctx context.Context, tc types.ToolCall, stages []types.CallStage, intent bool, e *Error) (types.Record, error) {
	payload := map[string]any{
		"module":     tc.Module,
		"mode":       tc.Mode,
		"stage":      stages,
		"inc":        tc.Inc,
		"intent":     intent,
		"latency_ms": tc.LatencyMS,
	}
	if tc.CheckDiff != nil {
		payload["check_diff"] = tc.CheckDiff
	}
	if tc.Result != nil {
		payload["result"] = tc.Result
	}
	if tc.Verify != nil {
		payload["verify"] = tc.Verify
	}
	if tc.Rolled {
		payload["rolled_back"] = true
	}
	if e != nil {
		payload["error_code"] = string(e.Code)
		payload["error_class"] = string(e.Class)
		if e.Reason != "" {
			payload["reason"] = e.Reason
		}
		payload["stage_failed"] = e.Stage
	}
	args, redactions := r.scrubArgs(tc.Args)
	payload["args"] = args
	rec := types.Record{
		Kind:       types.KToolCall,
		Inc:        tc.Inc,
		Origin:     r.origin(),
		Actor:      tc.Actor,
		Redactions: redactions,
		Payload:    payload,
	}
	// The record id is the audit record's own id: the ledger mints rec_id, so the
	// caller-visible id is filled on the way back (SPEC-06 §2.2).
	out, err := r.deps.Append(rec)
	if err != nil {
		return out, err
	}
	return out, nil
}

// scrubArgs scrubs the ledger copy of args (SPEC-02). A refusal refuses the
// call: args are never persisted raw.
func (r *Registry) scrubArgs(args map[string]any) (map[string]any, int) {
	if args == nil {
		return map[string]any{}, 0
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return map[string]any{"scrub": "args_not_serialisable"}, 0
	}
	res, err := r.deps.Scrub(scrubTarget, raw)
	if err != nil {
		return map[string]any{"scrub": "refused", "bytes_in": len(raw)}, 0
	}
	var out map[string]any
	if err := json.Unmarshal(res.Value, &out); err != nil || out == nil {
		return map[string]any{"scrub": "unrepresentable"}, res.Redactions
	}
	out["scrub"] = res.ByRule
	return out, res.Redactions
}

func (r *Registry) origin() types.Origin {
	return types.Origin{HostID: r.deps.HostID, Source: "registry"}
}

// validateStage is stage 2: JSON normalization, schema validation and the
// module's own typed normalization (§3.6: paths absolute + symlink-resolved,
// units normalized, ints only as JSON integers, unknown keys rejected).
func (r *Registry) validateStage(m types.Module, raw map[string]any) (map[string]any, *Error) {
	args, err := validate.Normalize(raw)
	if err != nil {
		return nil, toRegistryError(err, types.StageValidate)
	}
	if err := validate.Args(m.Descriptor().Schema, args); err != nil {
		return nil, toRegistryError(err, types.StageValidate)
	}
	if n, ok := m.(argsNormalizer); ok {
		norm, nerr := n.NormalizeArgs(args)
		if nerr != nil {
			return nil, toRegistryError(nerr, types.StageValidate)
		}
		if norm != nil {
			args = norm
		}
	}
	return args, nil
}

// argsNormalizer is the package-private extension a shipped module implements to
// turn schema-valid args into typed args (absolute paths, normalized units).
type argsNormalizer interface {
	NormalizeArgs(args map[string]any) (map[string]any, error)
}

func toRegistryError(err error, stage string) *Error {
	var ve *validate.Error
	if errors.As(err, &ve) {
		return newErr(ve.Code, stage, "", "%s", ve.Reason)
	}
	return wrapErr(types.CodeRegistry002, stage, "", err)
}

// moduleError attaches the stage's code to a module error by class (§5).
func moduleError(stage string, err error) *Error {
	class := classOfModuleError(err)
	code := checkCodeForStage(stage, class)
	e := newErr(code, stage, "", "%v", err)
	e.Class = class
	e.Err = err
	return e
}

// effectiveTimeout = min(Descriptor.TimeoutS, DeadlineS, registry.max_timeout_s)
// (SPEC-06 §6.16).
func (r *Registry) effectiveTimeout(m types.Module, deadlineS int) time.Duration {
	d := m.Descriptor()
	secs := d.TimeoutS
	if deadlineS > 0 && deadlineS < secs {
		secs = deadlineS
	}
	if r.cfg.MaxTimeoutS > 0 && secs > r.cfg.MaxTimeoutS {
		secs = r.cfg.MaxTimeoutS
	}
	return time.Duration(secs) * time.Second
}

// lockTargets serializes calls that touch the same canonical target (§6.8).
func (r *Registry) lockTargets(m types.Module, args map[string]any) (func(), *Error) {
	keys := map[string]bool{}
	for _, t := range targetsOf(m, args) {
		switch t.Kind {
		case "path":
			if c := canonicalPath(t.Value); c != "" {
				keys["path:"+c] = true
			}
		case "unit":
			if u := normalizeUnit(t.Value); u != "" {
				keys["unit:"+u] = true
			}
		}
	}
	if len(keys) == 0 {
		return func() {}, nil
	}
	ordered := make([]string, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Strings(ordered)
	held := make([]string, 0, len(ordered))
	deadline := r.deps.Now().Add(targetLockBudget)
	for _, k := range ordered {
		r.lockMu.Lock()
		ch, ok := r.targets[k]
		if !ok {
			ch = make(chan struct{}, 1)
			r.targets[k] = ch
		}
		r.lockMu.Unlock()
		for {
			select {
			case ch <- struct{}{}:
				held = append(held, k)
			default:
			}
			if len(held) > 0 && held[len(held)-1] == k {
				break
			}
			if r.deps.Now().After(deadline) {
				for _, h := range held {
					r.lockMu.Lock()
					if c := r.targets[h]; c != nil {
						<-c
					}
					r.lockMu.Unlock()
				}
				return func() {}, newErr(types.CodeRegistry009, types.StageDryRun, reasonTargetLockContention,
					"target lock %s not acquired within %s", k, targetLockBudget)
			}
			time.Sleep(time.Millisecond)
		}
	}
	return func() {
		for _, k := range held {
			r.lockMu.Lock()
			if c := r.targets[k]; c != nil {
				<-c
			}
			r.lockMu.Unlock()
		}
	}, nil
}

func stageDetail(e *Error) string {
	if e == nil {
		return ""
	}
	if e.Reason != "" {
		return e.Reason + ": " + e.Detail
	}
	return e.Detail
}

func diffSummary(d types.Diff) string {
	if d.Summary != "" {
		return d.Summary
	}
	if d.Empty {
		return "diff empty (already at the described state)"
	}
	return fmt.Sprintf("%d change(s)", len(d.Entries))
}

func appliedSummary(r types.Result) string {
	if !r.Changed {
		return "no change"
	}
	return fmt.Sprintf("changed=%d duration_ms=%d", len(r.Applied), r.DurationMS)
}

func verifySummary(v types.VerifyResult) string {
	return fmt.Sprintf("method=%s ok=%t", v.Method, v.OK)
}

func rollbackAvailable(r types.Result) bool {
	return r.Rollback != nil && r.Rollback.Supported && r.Rollback.Module != ""
}

func ledgerCodeOf(err error) string {
	type coder interface{ Code() types.ErrorCode }
	var c coder
	if errors.As(err, &c) {
		return string(c.Code())
	}
	return ""
}
