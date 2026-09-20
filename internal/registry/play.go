package registry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/trouble-agent/trouble/internal/sensors"
	"github.com/trouble-agent/trouble/internal/types"
)

// playFile is the on-disk TOML shape of a play (SPEC-06 §3.7). Field mapping is
// 1:1 onto types.Play/types.PlayTask: no invented keys, additionalProperties
// false, enforced by the strict decode below.
type playFile struct {
	SchemaVersion int            `toml:"schema_version"`
	Name          string         `toml:"name"`
	Version       int            `toml:"version"`
	Source        string         `toml:"source"`
	MaxRuns       int            `toml:"max_runs"`
	Tasks         []playFileTask `toml:"task"`
	Extra         map[string]any `toml:"-"`
}

type playFileTask struct {
	Name     string         `toml:"name"`
	Tool     string         `toml:"tool"`
	Args     map[string]any `toml:"args"`
	When     string         `toml:"when"`
	Register string         `toml:"register"`
	Retries  *int           `toml:"retries"`
	OnFail   string         `toml:"on_fail"`
}

// dntFile is the on-disk TOML shape of the do-not-touch file (SPEC-06 §3.6).
type dntFile struct {
	SchemaVersion int      `toml:"schema_version"`
	Paths         []string `toml:"paths"`
	Units         []string `toml:"units"`
	Scopes        []string `toml:"scopes"`
	// Weakening keys: their presence is a refusal, never a narrowing.
	Mandatory *bool    `toml:"mandatory"`
	Remove    []string `toml:"remove"`
}

// LoadPlay reads one play TOML file (SPEC-06 §2.2, §3.7). Static validation
// happens here: unknown keys, unknown tools, duplicate/non-identifier registers,
// out-of-range retries, a bad `on_fail` and a literal protected target all refuse
// the play at load, so a protected play never reaches the runner.
func LoadPlay(path string) (types.Play, error) {
	return LoadPlayWith(path, nil)
}

// LoadPlayWith is LoadPlay with an explicit do-not-touch set (the daemon passes
// its merged set; the bare loader falls back to the compiled-in floor).
func LoadPlayWith(path string, dnt *types.DoNotTouch) (types.Play, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return types.Play{}, wrapErr(types.CodeRegistry016, "load", reasonPlayInvalid, err)
	}
	play, err := DecodePlay(b, filepath.Base(path), dnt)
	if err != nil {
		return types.Play{}, err
	}
	return play, nil
}

// DecodePlay parses and statically validates one play document.
func DecodePlay(b []byte, name string, dnt *types.DoNotTouch) (types.Play, error) {
	var pf playFile
	md, err := toml.Decode(string(b), &pf)
	if err != nil {
		return types.Play{}, newErr(types.CodeRegistry016, "load", reasonPlayInvalid, "%s: %v", name, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			keys = append(keys, k.String())
		}
		return types.Play{}, newErr(types.CodeRegistry016, "load", reasonPlayInvalid,
			"%s: unknown keys %s (the schema is closed)", name, strings.Join(keys, ", "))
	}
	if pf.SchemaVersion != 1 {
		return types.Play{}, newErr(types.CodeRegistry016, "load", reasonPlayInvalid,
			"%s: schema_version must be 1, got %d", name, pf.SchemaVersion)
	}
	if pf.Name == "" {
		return types.Play{}, newErr(types.CodeRegistry016, "load", reasonPlayInvalid, "%s: play without a name", name)
	}
	if pf.Version <= 0 {
		return types.Play{}, newErr(types.CodeRegistry016, "load", reasonPlayInvalid,
			"%s: version must be a positive int", name)
	}
	if len(pf.Tasks) == 0 {
		return types.Play{}, newErr(types.CodeRegistry016, "load", reasonPlayInvalid, "%s: play without tasks", name)
	}
	src := pf.Source
	if src == "" {
		src = "module-default"
	}
	out := types.Play{Name: pf.Name, Version: pf.Version, Source: src, MaxRuns: pf.MaxRuns}
	if out.MaxRuns == 0 {
		out.MaxRuns = 2
	}
	shipped := shippedByName()
	registers := map[string]bool{}
	checkMode := true
	for i, t := range pf.Tasks {
		if t.Name == "" {
			return types.Play{}, newErr(types.CodeRegistry016, "load", reasonPlayInvalid,
				"%s: task %d has no name", name, i)
		}
		if t.Tool == "" {
			return types.Play{}, newErr(types.CodeRegistry016, "load", reasonPlayInvalid,
				"%s: task %d (%s) names no tool", name, i, t.Name)
		}
		desc, known := shipped[t.Tool]
		if !known {
			return types.Play{}, newErr(types.CodeRegistry016, "load", reasonPlayInvalid,
				"%s: task %d names unknown tool %q (a tool outside the shipped table cannot be named at all)", name, i, t.Tool)
		}
		if !desc.CheckMode {
			checkMode = false
		}
		retries := 0
		if t.Retries != nil {
			retries = *t.Retries
		}
		if retries < 0 || retries > 3 {
			return types.Play{}, newErr(types.CodeRegistry016, "load", reasonPlayInvalid,
				"%s: task %d (%s) retries=%d is outside 0..3", name, i, t.Name, retries)
		}
		onFail := t.OnFail
		if onFail == "" {
			onFail = "abort"
		}
		switch onFail {
		case "abort", "continue", "rollback":
		default:
			return types.Play{}, newErr(types.CodeRegistry016, "load", reasonPlayInvalid,
				"%s: task %d (%s) on_fail=%q is not abort|continue|rollback", name, i, t.Name, onFail)
		}
		if t.Register != "" {
			if !isIdentifier(t.Register) {
				return types.Play{}, newErr(types.CodeRegistry016, "load", reasonPlayInvalid,
					"%s: task %d register %q is not an identifier", name, i, t.Register)
			}
			if registers[t.Register] {
				return types.Play{}, newErr(types.CodeRegistry016, "load", reasonPlayInvalid,
					"%s: duplicate register %q", name, t.Register)
			}
			registers[t.Register] = true
		}
		if t.When != "" {
			if _, err := sensors.CompilePlayWhen(t.When, registerNames(registers)); err != nil {
				return types.Play{}, newErr(types.CodeRegistry017, "load", reasonWhenInvalid,
					"%s: task %d (%s) when: %v", name, i, t.Name, err)
			}
		}
		// Static do-not-touch: a task whose literal args name a protected
		// path/unit is refused at load (SPEC-06 §3.6).
		if e := staticProtected(t.Tool, t.Args, dnt); e != nil {
			return types.Play{}, e
		}
		args := t.Args
		if args == nil {
			args = map[string]any{}
		}
		out.Tasks = append(out.Tasks, types.PlayTask{
			Name: t.Name, Tool: t.Tool, Args: args, When: t.When,
			Register: t.Register, Retries: retries, OnFail: onFail,
		})
	}
	out.CheckMode = checkMode
	return out, nil
}

func registerNames(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}

func isIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// staticProtected refuses a play task whose literal args name a protected target.
func staticProtected(tool string, args map[string]any, dnt *types.DoNotTouch) *Error {
	if args == nil {
		return nil
	}
	set := types.DoNotTouch{Paths: floorPaths, Units: floorUnits, Scopes: floorScopes}
	if dnt != nil {
		set = *dnt
	}
	for key, raw := range args {
		s, ok := raw.(string)
		if !ok {
			continue
		}
		switch key {
		case "path", "paths", "file", "files", "source", "target", "dest":
			cand := canonicalPath(s)
			for _, p := range set.Paths {
				if matchPath(strings.ReplaceAll(p, stateRootPlaceholder, defaultStateRoot()), cand) {
					return newErr(types.CodeRegistry007, "load", reasonDoNotTouchPath,
						"task tool %s statically targets protected path %s", tool, cand)
				}
			}
		case "unit", "units":
			u := normalizeUnit(s)
			for _, want := range set.Units {
				if normalizeUnit(want) == u {
					return newErr(types.CodeRegistry007, "load", reasonDoNotTouchUnit,
						"task tool %s statically targets protected unit %s", tool, u)
				}
			}
		}
	}
	return nil
}

// LoadDoNotTouch reads and strictly validates the do-not-touch file, returning
// the file's (additive) sets (SPEC-06 §2.2, §3.6).
func LoadDoNotTouch(path string) (types.DoNotTouch, error) {
	dnt, warn := loadDoNotTouchFile(path, defaultStateRoot())
	if warn != nil {
		return dnt, warn
	}
	return dnt, nil
}

func loadDoNotTouchFile(path, stateRoot string) (types.DoNotTouch, *bootWarning) {
	if path == "" {
		return types.DoNotTouch{}, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return types.DoNotTouch{}, nil
		}
		return types.DoNotTouch{}, &bootWarning{Code: types.CodeRegistry016, Reason: reasonDoNotTouchUnavailable, Detail: err.Error()}
	}
	var df dntFile
	md, err := toml.Decode(string(b), &df)
	if err != nil {
		return types.DoNotTouch{}, &bootWarning{Code: types.CodeRegistry016, Reason: reasonPlayInvalid, Detail: err.Error()}
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			keys = append(keys, k.String())
		}
		return types.DoNotTouch{}, &bootWarning{Code: types.CodeRegistry016, Reason: reasonPlayInvalid,
			Detail: "unknown keys: " + strings.Join(keys, ", ")}
	}
	// Configuration may widen the deny set, never narrow it: a weakening file is
	// refused, the floor stands, and the refusal is recorded.
	if df.Mandatory != nil && !*df.Mandatory {
		return types.DoNotTouch{}, &bootWarning{Code: types.CodeRegistry006, Reason: reasonDoNotTouchWeaken,
			Detail: "mandatory = false is refused"}
	}
	if len(df.Remove) > 0 {
		return types.DoNotTouch{}, &bootWarning{Code: types.CodeRegistry006, Reason: reasonDoNotTouchWeaken,
			Detail: fmt.Sprintf("remove = %v is refused", df.Remove)}
	}
	return types.DoNotTouch{
		Paths:  resolveRoots(df.Paths, stateRoot),
		Units:  df.Units,
		Scopes: df.Scopes,
	}, nil
}

func resolveRoots(in []string, stateRoot string) []string {
	out := make([]string, 0, len(in))
	for _, p := range in {
		out = append(out, strings.ReplaceAll(p, stateRootPlaceholder, stateRoot))
	}
	return out
}

// ---- the play engine ----

// RunPlay executes a play as data (SPEC-06 §2.2, §3.7). One play run appends one
// `play_run` record; each task is one six-stage call through Call.
func (r *Registry) RunPlay(ctx context.Context, p types.Play, base types.ToolCallRequest) (types.PlayRun, error) {
	if len(p.Tasks) == 0 {
		return types.PlayRun{}, newErr(types.CodeRegistry016, "play", reasonPlayInvalid, "play %s has no tasks", p.Name)
	}
	if base.Mode == "" {
		base.Mode = types.ModeApply
	}
	run := types.PlayRun{
		ID:        types.NewID(types.PEv),
		Play:      p.Name,
		PlayVer:   p.Version,
		Inc:       base.Inc,
		Sig:       base.Sig,
		Source:    p.Source,
		Mode:      base.Mode,
		StartedTS: types.FormatUTC(r.deps.Now()),
	}
	registers := map[string]any{}
	applied := []types.RollbackHint{}
	for i, task := range p.Tasks {
		tr := types.TaskRun{Index: i, Name: task.Name, Tool: task.Tool}
		start := r.deps.Now()
		if task.When != "" {
			pred, err := sensors.CompilePlayWhen(task.When, registerNamesOf(p))
			if err != nil {
				tr.Status = "failed"
				tr.ErrorCode = string(types.CodeRegistry017)
				run.Tasks = append(run.Tasks, tr)
				break
			}
			if !pred(r.playVars(base, registers)) {
				tr.Status = "skipped"
				tr.SkippedBy = task.When
				tr.MS = int(r.deps.Now().Sub(start).Milliseconds())
				run.Tasks = append(run.Tasks, tr)
				continue
			}
		}
		// A mutating task whose module has no check_mode is not runnable under
		// any gate (SPEC-05 T09).
		if d, ok := shippedByName()[task.Tool]; ok && d.Mutating && !d.CheckMode {
			tr.Status = "refused"
			tr.ErrorCode = string(types.CodeRegistry014)
			run.Tasks = append(run.Tasks, tr)
			break
		}
		req := base
		req.Module = task.Tool
		req.Args = task.Args
		req.Source = fmt.Sprintf("play:%s@%d", p.Name, p.Version)
		req.IdemKey = r.idemKey(base, p, i, task)
		mode := base.Mode
		if !p.CheckMode && mode == types.ModeApply {
			// The play itself declares no check_mode: the mutating task stays
			// refused rather than applied blind.
			tr.Status = "refused"
			tr.ErrorCode = string(types.CodeRegistry014)
			run.Tasks = append(run.Tasks, tr)
			break
		}
		attempts := task.Retries + 1
		var tc types.ToolCall
		var callErr error
		for a := 0; a < attempts; a++ {
			tc, callErr = r.Call(ctx, req)
			if callErr == nil {
				if e := Outcome(tc); e != nil {
					callErr = e
				}
			}
			if callErr == nil {
				break
			}
			if ClassOf(callErr) != types.ErrClassTransient {
				break
			}
			if a < attempts-1 {
				r.sleep(backoffFor(a))
			}
		}
		tr.ToolCallID = tc.ID
		tr.MS = int(r.deps.Now().Sub(start).Milliseconds())
		if callErr != nil {
			tr.ErrorCode = string(CodeOf(callErr))
			if ClassOf(callErr) == types.ErrClassPolicyRefused {
				tr.Status = "refused"
			} else {
				tr.Status = "failed"
			}
			// on_fail: abort stops, continue records and proceeds, rollback
			// replays the accumulated hints in reverse and stops at the first
			// PONR (SPEC-06 §3.7).
			switch task.OnFail {
			case "continue":
				registers[task.Register] = map[string]any{"ok": false}
				run.Tasks = append(run.Tasks, tr)
				continue
			case "rollback":
				rbErr := r.rollbackAll(ctx, applied)
				run.Tasks = append(run.Tasks, tr)
				if rbErr != nil {
					run.Outcome = types.OutcomeRolledBack
					run.EndedTS = types.FormatUTC(r.deps.Now())
					r.appendPlayRun(ctx, run)
					return run, rbErr
				}
				run.Outcome = types.OutcomeRolledBack
				run.EndedTS = types.FormatUTC(r.deps.Now())
				r.appendPlayRun(ctx, run)
				return run, callErr
			default: // abort
				run.Tasks = append(run.Tasks, tr)
				run.Outcome = types.OutcomeFailed
				run.EndedTS = types.FormatUTC(r.deps.Now())
				r.appendPlayRun(ctx, run)
				return run, callErr
			}
		}
		if task.Register != "" {
			out := map[string]any{}
			if tc.Result != nil && tc.Result.Output != nil {
				out = tc.Result.Output
			}
			registers[task.Register] = out
			tr.Registers = map[string]any{task.Register: out}
		}
		if tc.Result != nil && tc.Result.Rollback != nil && tc.Result.Rollback.Supported {
			applied = append(applied, *tc.Result.Rollback)
		}
		switch {
		case tc.Result != nil && tc.Result.Changed:
			tr.Status = "changed"
			run.Changed = true
		default:
			tr.Status = "ok"
		}
		run.Tasks = append(run.Tasks, tr)
	}
	if run.Outcome == "" {
		switch {
		case run.Changed:
			run.Outcome = types.OutcomeApplied
		case base.Mode == types.ModeCheck:
			run.Outcome = types.OutcomeCheckOnly
		case allSkipped(run.Tasks):
			run.Outcome = types.OutcomeDrafted
		default:
			run.Outcome = types.OutcomeCheckOnly
		}
	}
	run.EndedTS = types.FormatUTC(r.deps.Now())
	if _, err := r.appendPlayRun(ctx, run); err != nil {
		return run, wrapErr(types.CodeRegistry006, types.StageAudit, reasonAuditAppendFailed, err)
	}
	return run, nil
}

// rollbackAll replays hints in reverse, stopping at the first PONR.
func (r *Registry) rollbackAll(ctx context.Context, hints []types.RollbackHint) error {
	for i := len(hints) - 1; i >= 0; i-- {
		h := hints[i]
		if IsPONR(h.Module) || !invertibleModules[h.Module] {
			return newErr(types.CodeRegistry015, "rollback", reasonRollbackUnsupported,
				"rollback stopped at %s: %s", h.Module, reasonRollbackUnsupported)
		}
		if err := r.rollbackHint(ctx, h); err != nil {
			return err
		}
	}
	return nil
}

func (r *Registry) rollbackHint(ctx context.Context, h types.RollbackHint) error {
	req := types.ToolCallRequest{
		Module: h.Module,
		Args:   h.Args,
		Mode:   types.ModeApply,
		Source: "rollback:" + h.Module,
		Actor:  r.deps.Actor,
	}
	req.IdemKey = "rollback|" + h.Module + "|" + shortHash(h.Args)
	tc, err := r.Call(ctx, req)
	if err != nil {
		return err
	}
	if e := Outcome(tc); e != nil {
		return e
	}
	return nil
}

func (r *Registry) appendPlayRun(ctx context.Context, run types.PlayRun) (types.Record, error) {
	tasks := make([]map[string]any, 0, len(run.Tasks))
	for _, t := range run.Tasks {
		tasks = append(tasks, map[string]any{
			"index": t.Index, "name": t.Name, "tool": t.Tool, "status": t.Status,
			"skipped_by": t.SkippedBy, "tool_call_id": t.ToolCallID, "error_code": t.ErrorCode, "ms": t.MS,
		})
	}
	rec := types.Record{
		Kind:   types.KPlayRun,
		Sig:    run.Sig,
		Inc:    run.Inc,
		Origin: r.origin(),
		Actor:  r.deps.Actor,
		Payload: map[string]any{
			"play": run.Play, "play_version": run.PlayVer, "source": run.Source,
			"mode": run.Mode, "outcome": run.Outcome, "changed": run.Changed,
			"started_ts": run.StartedTS, "ended_ts": run.EndedTS, "tasks": tasks,
		},
	}
	out, err := r.deps.Append(rec)
	if err != nil {
		return out, err
	}
	run.ID = out.RecID
	return out, nil
}

// playVars builds the evaluation scope of a play `when:` (SPEC-06 §3.7): the
// incident context plus every register's Output map flattened into dotted names.
func (r *Registry) playVars(base types.ToolCallRequest, registers map[string]any) map[string]any {
	vars := map[string]any{
		"inc": base.Inc, "sig": base.Sig, "rule": base.Rule,
	}
	if base.Inc != "" {
		vars["inc"] = base.Inc
	}
	for name, out := range registers {
		flatten(vars, name, out)
	}
	return vars
}

func flatten(vars map[string]any, prefix string, v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, sub := range t {
			flatten(vars, prefix+"."+k, sub)
		}
	case []any:
		for i, item := range t {
			flatten(vars, fmt.Sprintf("%s.%d", prefix, i), item)
		}
	case string:
		vars[prefix] = t
	case bool:
		vars[prefix] = t
	case int:
		vars[prefix] = t
	case int64:
		vars[prefix] = t
	case float64:
		vars[prefix] = t
	}
}

func (r *Registry) idemKey(base types.ToolCallRequest, p types.Play, index int, task types.PlayTask) string {
	return fmt.Sprintf("%s|%s@%d|%d|%s|%s", base.Inc, p.Name, p.Version, index, task.Tool, shortHash(task.Args))
}

func shortHash(v any) string {
	b, err := json.Marshal(canonicalJSON(v))
	if err != nil {
		return "unhashable"
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

// canonicalJSON normalises numbers to json.Number so two spellings of the same
// args hash identically.
func canonicalJSON(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return v
	}
	return out
}

func registerNamesOf(p types.Play) []string {
	out := []string{}
	for _, t := range p.Tasks {
		if t.Register != "" {
			out = append(out, t.Register)
		}
	}
	return out
}

func allSkipped(tasks []types.TaskRun) bool {
	for _, t := range tasks {
		if t.Status != "skipped" {
			return false
		}
	}
	return len(tasks) > 0
}

func backoffFor(attempt int) time.Duration {
	d := time.Second << attempt
	if d > 8*time.Second {
		d = 8 * time.Second
	}
	return d
}

// sleep is the retry backoff; it is a field so tests never wait.
func (r *Registry) sleep(d time.Duration) {
	if r.sleeper != nil {
		r.sleeper(d)
		return
	}
	time.Sleep(d)
}

func asRegistryError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Code: types.CodeRegistry011, Class: types.ErrClassPermanent, Detail: err.Error()}
}
