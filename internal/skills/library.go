// library.go implements SPEC-11 §2b: the LOCAL skill library — one
// `<local_dir>/<name>/SKILL.md` per skill in the Claude-skill format (YAML
// frontmatter + a markdown body) — and the typed step execution of §4.7a.
//
// It is an authoring surface, not a distribution channel. A pulled skill crosses a
// host boundary and is therefore signed, canaried and approved (§3.1, §4.2); a
// library skill never leaves the host, so what bounds it is the descriptor
// allowlist, the six registry stages and the ladder's autonomy gate — exactly a
// locally-authored play's authority. The format makes the same guarantee the artifact
// schema does: there is no key in which arbitrary code could be written, and the
// markdown body is never parsed for commands.
package skills

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// Defaults for the `local_*` keys, applied when the config leaves them unset.
const (
	DefaultLocalMaxBytes    = int64(1048576)
	DefaultLocalMaxSkills   = 64
	DefaultLocalMaxSteps    = 16
	DefaultLocalStepTimeout = types.Duration("60s")
)

// LibraryConfig is the resolved `local_*` half of `[skills]` (§2b).
type LibraryConfig struct {
	// Enabled reads the library at all. Off (the shipped default) means nothing on
	// disk is read and no step can run.
	Enabled bool
	// Dir is the library root: one <name>/SKILL.md per skill.
	Dir string
	// MaxBytes caps one SKILL.md; a larger file is refused, never truncated.
	MaxBytes int64
	// MaxSkills caps the number of library skills.
	MaxSkills int
	// MaxSteps caps the steps of one skill.
	MaxSteps int
	// StepTimeout is the wall clock of one step.
	StepTimeout types.Duration
}

// LocalSkill is one parsed `<name>/SKILL.md` (§2b).
type LocalSkill struct {
	Name        string
	Description string
	Version     int
	Dir         string
	File        string
	// Digest is hex64(sha256(SKILL.md bytes)): the file the steps came from.
	Digest string
	// Frontmatter carries every scalar the file declared, for reporting.
	Frontmatter map[string]string
	Steps       []SkillStep
}

// SkillStep is one typed step from the frontmatter (§2b). A step is a registry
// module plus literal args — there is no command, script or template field.
type SkillStep struct {
	Index  int
	Title  string
	Module string
	Args   map[string]any
}

// StepRunner is the typed execution view of SPEC-06's registry (§2b).
// *registry.Runner satisfies it, and so does the ladder's PlayRunner adapter. There
// is no method that takes a command line, a script, a template or an interpreter.
type StepRunner interface {
	Check(ctx context.Context, tool string, args map[string]any) (types.Diff, types.ToolCall, error)
	Apply(ctx context.Context, tool string, args map[string]any) (types.Result, types.ToolCall, error)
}

// Reason tokens this file adds (§2b/§4.7a). A reason is a stable token, never prose.
const (
	ReasonLibraryDisabled    = "library_disabled"
	ReasonDirMissing         = "dir_missing"
	ReasonDirWritable        = "dir_writable"
	ReasonDirUnreadable      = "dir_unreadable"
	ReasonSkillMDMissing     = "skill_md_missing"
	ReasonFrontmatter        = "frontmatter_unterminated"
	ReasonUnparsableLine     = "unparsable_line"
	ReasonTabIndent          = "tab_indent"
	ReasonIndentInconsistent = "indent_inconsistent"
	ReasonBadHeader          = "bad_header"
	ReasonNestedList         = "nested_list"
	ReasonListItemScalar     = "list_item_scalar"
	ReasonDuplicateKey       = "duplicate_key"
	ReasonNameMismatch       = "name_mismatch"
	ReasonMissingDesc        = "missing_description"
	ReasonBadVersion         = "bad_version"
	ReasonBadModule          = "bad_module"
	ReasonArgsJSON           = "args_json_invalid"
	ReasonFileTooLarge       = "file_too_large"
	ReasonTooManySkills      = "too_many_skills"
	ReasonTooManySteps       = "too_many_steps"
	ReasonUnknownSkill       = "unknown_skill"
	ReasonUnknownStep        = "unknown_step"
	ReasonBadMode            = "bad_mode"
	ReasonNoRunner           = "no_runner"
)

// Library is the local SKILL.md library (§2b).
type Library struct {
	cfg    LibraryConfig
	deps   Deps
	runner StepRunner
}

// LocalLibraryConfig resolves the §2b configuration from a `[skills]` config,
// applying the documented defaults for every unset cap.
func LocalLibraryConfig(cfg types.SkillsConfig) LibraryConfig {
	lc := LibraryConfig{
		Enabled:     cfg.LocalEnabled,
		Dir:         cfg.LocalDir,
		MaxBytes:    cfg.LocalMaxBytes,
		MaxSkills:   cfg.LocalMaxSkills,
		MaxSteps:    cfg.LocalMaxSteps,
		StepTimeout: cfg.LocalStepTimeout,
	}
	if lc.MaxBytes <= 0 {
		lc.MaxBytes = DefaultLocalMaxBytes
	}
	if lc.MaxSkills <= 0 {
		lc.MaxSkills = DefaultLocalMaxSkills
	}
	if lc.MaxSteps <= 0 {
		lc.MaxSteps = DefaultLocalMaxSteps
	}
	if lc.StepTimeout == "" {
		lc.StepTimeout = DefaultLocalStepTimeout
	}
	return lc
}

// NewLibrary builds the library. It refuses at construction — never at call time —
// when the declaration cannot be honored: an enabled library with no directory, a
// directory that is missing, or one another user could replace (§2b rules 5 and 6).
func NewLibrary(cfg types.SkillsConfig, deps Deps, runner StepRunner) (*Library, error) {
	lc := LocalLibraryConfig(cfg)
	if !lc.Enabled {
		// Off is a complete posture, not an error: no directory is required and
		// nothing on disk is touched.
		return &Library{cfg: lc, deps: deps, runner: runner}, nil
	}
	if strings.TrimSpace(lc.Dir) == "" {
		return nil, newErr(types.CodeSkills001, ReasonConfig,
			"local_enabled = true needs local_dir: the library root is the operator's statement")
	}
	if strings.Contains(lc.Dir, "..") || strings.Contains(lc.Dir, "://") {
		return nil, newErr(types.CodeSkills001, ReasonConfig,
			"local_dir %q must be a plain absolute path (no .., no URL)", lc.Dir)
	}
	info, err := os.Stat(lc.Dir)
	if err != nil {
		return nil, newErr(types.CodeSkills013, ReasonDirMissing,
			"local_dir %q is not readable: %v", lc.Dir, err)
	}
	if !info.IsDir() {
		return nil, newErr(types.CodeSkills013, ReasonDirMissing, "local_dir %q is not a directory", lc.Dir)
	}
	if p, bad := writableDirOnPath(lc.Dir); bad {
		return nil, newErr(types.CodeSkills013, ReasonDirWritable,
			"local_dir %s is writable by another user (%s): a library an attacker can replace is a capability handed to that user",
			lc.Dir, p)
	}
	return &Library{cfg: lc, deps: deps, runner: runner}, nil
}

// Config returns the resolved library configuration.
func (l *Library) Config() LibraryConfig { return l.cfg }

// Scan reads the library (§2b). It is read-only and deterministic: skills come back
// in name order, and a malformed file is refused with a `library_refused` record
// while the rest of the directory still loads (per-file isolation, rule 4).
func (l *Library) Scan(ctx context.Context) ([]LocalSkill, error) {
	if !l.cfg.Enabled {
		return nil, nil
	}
	entries, err := os.ReadDir(l.cfg.Dir)
	if err != nil {
		return nil, newErr(types.CodeSkills013, ReasonDirUnreadable, "read %s: %v", l.cfg.Dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) > l.cfg.MaxSkills {
		return nil, newErr(types.CodeSkills001, ReasonTooManySkills,
			"the library holds %d skills, above local_max_skills %d: the scan refuses rather than read a subset",
			len(names), l.cfg.MaxSkills)
	}
	out := make([]LocalSkill, 0, len(names))
	refused := 0
	for _, name := range names {
		dir := filepath.Join(l.cfg.Dir, name)
		file := filepath.Join(dir, "SKILL.md")
		raw, err := os.ReadFile(file)
		if err != nil {
			refused++
			l.refusedRecord(ctx, dir, file, ReasonSkillMDMissing, 0, types.CodeSkills001)
			continue
		}
		if int64(len(raw)) > l.cfg.MaxBytes {
			refused++
			l.refusedRecord(ctx, dir, file, ReasonFileTooLarge, 0, types.CodeSkills001)
			continue
		}
		skill, perr := parseLocalSkill(name, dir, file, raw, l.cfg.MaxSteps)
		if perr != nil {
			refused++
			l.refusedRecord(ctx, dir, file, ReasonOf(perr), lineOf(perr), CodeOf(perr))
			continue
		}
		out = append(out, skill)
	}
	l.loadedRecord(ctx, out, refused)
	return out, nil
}

// Get returns one library skill by name.
func (l *Library) Get(name string) (LocalSkill, bool) {
	all, err := l.Scan(context.Background())
	if err != nil {
		return LocalSkill{}, false
	}
	for _, s := range all {
		if s.Name == name {
			return s, true
		}
	}
	return LocalSkill{}, false
}

// Plays lists the library as the play shape the runner already consumes (§2b): one
// task per step, `source = skill-local:<name>@<version>`. With the library off it
// returns an empty list.
func (l *Library) Plays(ctx context.Context) ([]types.Play, error) {
	all, err := l.Scan(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]types.Play, 0, len(all))
	for _, s := range all {
		tasks := make([]types.PlayTask, 0, len(s.Steps))
		for _, st := range s.Steps {
			tasks = append(tasks, types.PlayTask{Name: st.Title, Tool: st.Module, Args: st.Args})
		}
		out = append(out, types.Play{
			Name:      s.Name,
			Version:   s.Version,
			Tasks:     tasks,
			CheckMode: true,
			Source:    "skill-local:" + s.Name + "@" + strconv.Itoa(s.Version),
		})
	}
	return out, nil
}

// RunStep executes one step against the runner and records it as a `skill`-kind
// record (§4.7a). It refuses rather than guesses: an unknown skill, an
// out-of-range step, a mode that is not check_mode/apply, an unregistered module or
// a missing runner all refuse with the runner never called.
func (l *Library) RunStep(ctx context.Context, inc types.Incident, name string, step int, mode string) (types.ToolCall, error) {
	if !l.cfg.Enabled {
		return types.ToolCall{}, newErr(types.CodeSkills013, ReasonLibraryDisabled,
			"the local skill library is off (local_enabled = false): nothing is read and no step runs")
	}
	switch mode {
	case types.ModeCheck, types.ModeApply:
	default:
		return types.ToolCall{}, newErr(types.CodeSkills013, ReasonBadMode,
			"mode %q is not %s|%s", mode, types.ModeCheck, types.ModeApply)
	}
	skill, ok := l.Get(name)
	if !ok {
		return types.ToolCall{}, newErr(types.CodeSkills013, ReasonUnknownSkill,
			"the library has no skill %q", name)
	}
	if step < 0 || step >= len(skill.Steps) {
		return types.ToolCall{}, newErr(types.CodeSkills013, ReasonUnknownStep,
			"skill %q has %d steps: %d is out of range", name, len(skill.Steps), step)
	}
	st := skill.Steps[step]
	if registered := l.registered(); len(registered) > 0 && !containsString(registered, st.Module) {
		return types.ToolCall{}, newErr(types.CodeSkills013, ReasonMissingModule,
			"step %d of %q calls %q, which this build does not register", step, name, st.Module)
	}
	if l.runner == nil {
		return types.ToolCall{}, newErr(types.CodeSkills013, ReasonNoRunner,
			"no runner is wired: a step is a typed tool call and nothing else")
	}

	stepCtx := ctx
	if d := l.cfg.StepTimeout.Std(); d > 0 {
		var cancel context.CancelFunc
		stepCtx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}

	var (
		tc      types.ToolCall
		changed bool
		err     error
	)
	if mode == types.ModeCheck {
		var diff types.Diff
		diff, tc, err = l.runner.Check(stepCtx, st.Module, st.Args)
		changed = !diff.Empty
	} else {
		var res types.Result
		res, tc, err = l.runner.Apply(stepCtx, st.Module, st.Args)
		changed = res.Changed
	}
	payload := map[string]any{
		"name":       skill.Name,
		"version":    skill.Version,
		"digest":     skill.Digest,
		"step":       step,
		"title":      st.Title,
		"module":     st.Module,
		"mode":       mode,
		"changed":    changed,
		"tool_call":  tc.ID,
		"error_code": "",
	}
	if err != nil {
		payload["error_code"] = string(CodeOf(err))
	}
	if _, rerr := l.deps.phaseRecord(ctx, PhaseStepExecuted, inc.Sig, inc.ID, payload); rerr != nil && err == nil {
		return tc, rerr
	}
	return tc, err
}

// registered lists the descriptor names this build ships (Deps.Registered).
func (l *Library) registered() []string {
	if l.deps.Registered == nil {
		return nil
	}
	return l.deps.Registered()
}

func (l *Library) refusedRecord(ctx context.Context, dir, file, reason string, line int, code types.ErrorCode) {
	_, _ = l.deps.phaseRecord(ctx, PhaseLibraryRefused, "", "", map[string]any{
		"dir":        dir,
		"file":       file,
		"reason":     reason,
		"line":       line,
		"error_code": string(code),
	})
}

func (l *Library) loadedRecord(ctx context.Context, skills []LocalSkill, refused int) {
	steps := 0
	digests := make([]string, 0, len(skills))
	for _, s := range skills {
		steps += len(s.Steps)
		digests = append(digests, s.Name+":"+s.Digest)
	}
	sum := sha256.Sum256([]byte(strings.Join(digests, "\n")))
	_, _ = l.deps.phaseRecord(ctx, PhaseLibraryLoaded, "", "", map[string]any{
		"dir":        l.cfg.Dir,
		"digest":     hex.EncodeToString(sum[:])[:16],
		"skills":     len(skills),
		"refused":    refused,
		"steps":      steps,
		"max_skills": l.cfg.MaxSkills,
	})
}

// writableDirOnPath reports the first path component (the directory itself or an
// ancestor) that another user could write to, and therefore replace the library
// through. A sticky directory (like /tmp) is exempt: the sticky bit is what stops
// another user from renaming or deleting a file they do not own.
func writableDirOnPath(dir string) (string, bool) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", false
	}
	for p := abs; ; {
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			if info.Mode().Perm()&0o022 != 0 && info.Mode()&os.ModeSticky == 0 {
				return p, true
			}
		}
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	return "", false
}

// ---------------------------------------------------------------------------
// The frontmatter subset parser (§2b rule 3).
// ---------------------------------------------------------------------------

// parseLocalSkill parses one SKILL.md into a LocalSkill, refusing unsupported
// syntax by name and line instead of silently dropping what it cannot read.
func parseLocalSkill(name, dir, file string, raw []byte, maxSteps int) (LocalSkill, *Error) {
	fm, err := parseFrontmatter(raw)
	if err != nil {
		return LocalSkill{}, err
	}
	skill := LocalSkill{
		Name:        name,
		Dir:         dir,
		File:        file,
		Version:     1,
		Digest:      sha256Hex(raw),
		Frontmatter: map[string]string{},
	}
	declared, _ := fm["name"].(string)
	if declared == "" {
		return LocalSkill{}, newErr(types.CodeSkills001, ReasonNameMismatch,
			"%s declares no name; the name is required and must equal the directory", file)
	}
	if declared != name {
		return LocalSkill{}, newErr(types.CodeSkills001, ReasonNameMismatch,
			"%s declares name %q but lives in %q", file, declared, name)
	}
	desc, _ := fm["description"].(string)
	if strings.TrimSpace(desc) == "" {
		return LocalSkill{}, newErr(types.CodeSkills001, ReasonMissingDesc,
			"%s declares no description", file)
	}
	skill.Description = strings.TrimSpace(desc)
	for key, v := range fm {
		switch key {
		case "name", "description":
			continue
		case "version":
			n, ok := asIntValue(v)
			if !ok || n < 1 {
				return LocalSkill{}, newErr(types.CodeSkills001, ReasonBadVersion,
					"%s: version must be an integer >= 1, got %v", file, v)
			}
			skill.Version = n
		case "trouble":
			steps, serr := parseSteps(file, v, maxSteps)
			if serr != nil {
				return LocalSkill{}, serr
			}
			skill.Steps = steps
		default:
			// Claude-skill frontmatter carries arbitrary metadata (category,
			// allowed-tools, ...): scalars are kept for reporting, and a nested
			// value under an unknown key is refused rather than ignored, because a
			// nested block is exactly where a step list would hide.
			s, ok := v.(string)
			if !ok {
				return LocalSkill{}, newErr(types.CodeSkills001, ReasonFieldRule,
					"%s: key %q carries a nested block; only `trouble:` may nest", file, key)
			}
			skill.Frontmatter[key] = s
		}
	}
	return skill, nil
}

// stepKeys are the only keys a `trouble.steps[]` item may carry. The set is closed
// on purpose: `shell`, `exec`, `cmd`, `command`, `script`, `eval`, `run` and
// `interpreter` are refused by name, which is the "never arbitrary code" half of
// §2b rule 1.
var stepKeys = map[string]bool{
	"title": true, "module": true, "args": true, "args_json": true,
}

func parseSteps(file string, v any, maxSteps int) ([]SkillStep, *Error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, newErr(types.CodeSkills001, ReasonFieldRule,
			"%s: `trouble:` must be a block of keys (steps:)", file)
	}
	for key := range m {
		if key != "steps" {
			return nil, newErr(types.CodeSkills001, ReasonUnknownKey,
				"%s: `trouble.%s` is not a key of the local skill format (only `steps`)", file, key)
		}
	}
	raw, ok := m["steps"]
	if !ok {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, newErr(types.CodeSkills001, ReasonFieldRule,
			"%s: `trouble.steps` must be a list of `- key: value` items", file)
	}
	if len(list) > maxSteps {
		return nil, newErr(types.CodeSkills001, ReasonTooManySteps,
			"%s: %d steps, above local_max_steps %d", file, len(list), maxSteps)
	}
	out := make([]SkillStep, 0, len(list))
	for i, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			return nil, newErr(types.CodeSkills001, ReasonListItemScalar,
				"%s: step %d is not a `- key: value` block", file, i)
		}
		for key := range entry {
			if !stepKeys[key] {
				return nil, newErr(types.CodeSkills001, ReasonUnknownKey,
					"%s: step %d carries %q, which is not a step key (title|module|args|args_json)", file, i, key)
			}
		}
		step := SkillStep{Index: i}
		module, _ := entry["module"].(string)
		if !isModuleName(module) {
			return nil, newErr(types.CodeSkills001, ReasonBadModule,
				"%s: step %d names module %q, want a dotted descriptor name (proc.connections)", file, i, module)
		}
		step.Module = module
		step.Title, _ = entry["title"].(string)
		switch {
		case entry["args"] != nil && entry["args_json"] != nil:
			return nil, newErr(types.CodeSkills001, ReasonFieldRule,
				"%s: step %d carries both args and args_json; a step has one argument form", file, i)
		case entry["args"] != nil:
			args, ok := entry["args"].(map[string]any)
			if !ok {
				return nil, newErr(types.CodeSkills001, ReasonFieldRule,
					"%s: step %d `args` must be a block of scalar keys", file, i)
			}
			scalars := map[string]any{}
			for k, v := range args {
				switch t := v.(type) {
				case string:
					scalars[k] = coerceScalar(t)
				case int, float64, bool:
					// The lexer already coerced an unquoted scalar; a module schema
					// that wants a string gets one from the JSON layer, not from a
					// guess here.
					scalars[k] = t
				default:
					return nil, newErr(types.CodeSkills001, ReasonFieldRule,
						"%s: step %d arg %q is nested; use args_json for structured args", file, i, k)
				}
			}
			step.Args = scalars
		case entry["args_json"] != nil:
			rawJSON, ok := entry["args_json"].(string)
			if !ok {
				return nil, newErr(types.CodeSkills001, ReasonArgsJSON,
					"%s: step %d `args_json` must be one JSON object in a string", file, i)
			}
			args := map[string]any{}
			if err := json.Unmarshal([]byte(rawJSON), &args); err != nil {
				return nil, newErr(types.CodeSkills001, ReasonArgsJSON,
					"%s: step %d args_json is not a JSON object: %v", file, i, err)
			}
			for k, v := range args {
				switch v.(type) {
				case string, float64, bool:
				default:
					return nil, newErr(types.CodeSkills001, ReasonArgsJSON,
						"%s: step %d arg %q is neither a scalar nor a string", file, i, k)
				}
			}
			step.Args = args
		}
		out = append(out, step)
	}
	return out, nil
}

// coerceScalar turns an unquoted frontmatter scalar into the value the module
// schema expects: a bool, an integer, a float or the string as written.
func coerceScalar(s string) any {
	switch s {
	case "true":
		return true
	case "false":
		return false
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	if strings.ContainsAny(s, ".eE") {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f
		}
	}
	return s
}

// isModuleName is the descriptor-name shape (SPEC-06 §3.2): dotted lower-case
// identifiers, at least one dot, no globs.
func isModuleName(s string) bool {
	if s == "" || strings.ContainsAny(s, "*?[] ") {
		return false
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		for i := 0; i < len(p); i++ {
			c := p[i]
			switch {
			case c >= 'a' && c <= 'z', c == '_':
			case c >= '0' && c <= '9' && i > 0:
			default:
				return false
			}
		}
	}
	return true
}

// lineOf reports the line an error names (0 when it names none).
func lineOf(err error) int {
	var le *lineError
	if errors.As(err, &le) {
		return le.line
	}
	return 0
}

// lineError is a *Error that also names the line it refused, so the refusal record
// can point an author at the defect.
type lineError struct {
	err  *Error
	line int
}

func (e *lineError) Error() string { return e.err.Error() }

func (e *lineError) Unwrap() error { return e.err }

// newLineErr builds an error that names its line, keeping the *Error contract the
// rest of this package uses.
func newLineErr(line int, code types.ErrorCode, reason, format string, args ...any) *Error {
	e := newErr(code, reason, format, args...)
	e.Err = &lineError{err: e, line: line}
	return e
}

// asIntValue reads a parsed value as an int.
func asIntValue(v any) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, true
	case string:
		n, err := strconv.Atoi(t)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// The indentation-aware subset lexer.
// ---------------------------------------------------------------------------

// fmLine is one frontmatter line: its 1-based number, its indent width, and either a
// key/value pair or a list item. A block scalar is folded into its key's value at
// lex time.
type fmLine struct {
	no     int
	indent int
	item   bool
	key    string
	val    string
}

// parseFrontmatter returns the frontmatter as nested maps/lists/scalars.
//
// Supported (and nothing else): `key: value`, `key:` + an indented block,
// `- key: value` list items, block scalars (`>`, `>-`, `|`, `|-`, `>+`, `|+`) and
// `#` comments. Anything else is refused with its line number.
func parseFrontmatter(raw []byte) (map[string]any, *Error) {
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return nil, newErr(types.CodeSkills001, ReasonFrontmatter,
			"the file does not start with a `---` frontmatter block")
	}
	rest := text[4:]
	end := -1
	offset := 0
	for _, line := range strings.Split(rest, "\n") {
		if strings.TrimRight(line, " \t") == "---" {
			end = offset
			break
		}
		offset += len(line) + 1
	}
	if end < 0 {
		return nil, newErr(types.CodeSkills001, ReasonFrontmatter,
			"the frontmatter block is never closed by `---`")
	}
	lines, err := lexFrontmatter(rest[:end])
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return map[string]any{}, nil
	}
	if lines[0].item {
		return nil, newErr(types.CodeSkills001, ReasonListItemScalar,
			"line %d: the frontmatter must start with a key", lines[0].no)
	}
	if lines[0].indent != 0 {
		return nil, newErr(types.CodeSkills001, ReasonIndentInconsistent,
			"line %d: the first frontmatter key is indented", lines[0].no)
	}
	out, i, err := parseMapBlock(lines, 0, 0)
	if err != nil {
		return nil, err
	}
	if i != len(lines) {
		return nil, newErr(types.CodeSkills001, ReasonIndentInconsistent,
			"line %d: unexpected indentation (the block opened at column 0)", lines[i].no)
	}
	return out, nil
}

// lexFrontmatter turns the block's text into lines, refusing tabs, headers,
// nested lists and block-scalar syntax this subset does not implement.
func lexFrontmatter(body string) ([]fmLine, *Error) {
	rawLines := strings.Split(body, "\n")
	out := []fmLine{}
	for i := 0; i < len(rawLines); i++ {
		no := i + 1
		line, cerr := stripComment(rawLines[i])
		if cerr != nil {
			return nil, newLineErr(no, types.CodeSkills001, ReasonFrontmatter, "line %d: %v", no, cerr)
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := 0
		for indent < len(line) && line[indent] == ' ' {
			indent++
		}
		if indent < len(line) && line[indent] == '\t' {
			return nil, newErr(types.CodeSkills001, ReasonTabIndent,
				"line %d: a tab is not an indent character", no)
		}
		content := line[indent:]
		switch {
		case strings.HasPrefix(content, "["):
			return nil, newErr(types.CodeSkills001, ReasonBadHeader,
				"line %d: `[[…]]`/`[table]` headers are not part of the frontmatter subset", no)
		case strings.HasPrefix(content, "- "), content == "-":
			itemText := strings.TrimSpace(strings.TrimPrefix(content, "-"))
			if strings.HasPrefix(itemText, "- ") || itemText == "-" {
				return nil, newErr(types.CodeSkills001, ReasonNestedList,
					"line %d: a nested list is not part of the frontmatter subset", no)
			}
			key, val, marker, ok := splitKeyValue(itemText)
			if !ok {
				return nil, newErr(types.CodeSkills001, ReasonListItemScalar,
					"line %d: a list item must be `- key: value`", no)
			}
			if marker != "" {
				folded, next, ferr := blockBody(rawLines, i, indent, marker, no)
				if ferr != nil {
					return nil, ferr
				}
				out = append(out, fmLine{no: no, indent: indent, item: true, key: key, val: folded})
				i = next
				continue
			}
			out = append(out, fmLine{no: no, indent: indent, item: true, key: key, val: val})
		default:
			key, val, marker, ok := splitKeyValue(content)
			if !ok {
				return nil, newErr(types.CodeSkills001, ReasonUnparsableLine,
					"line %d: expected `key: value`, a `key:` block or a `- key: value` item", no)
			}
			if marker != "" {
				folded, next, ferr := blockBody(rawLines, i, indent, marker, no)
				if ferr != nil {
					return nil, ferr
				}
				out = append(out, fmLine{no: no, indent: indent, key: key, val: folded})
				i = next
				continue
			}
			out = append(out, fmLine{no: no, indent: indent, key: key, val: val})
		}
	}
	return out, nil
}

// blockBody collects the indented lines of a block scalar, returning the joined text
// and the index of the last consumed line. `>` folds newlines into spaces, `|` keeps
// them; all chomping indicators are accepted and trailing whitespace is trimmed.
func blockBody(rawLines []string, keyIndex, keyIndent int, marker string, no int) (string, int, *Error) {
	body := []string{}
	blockIndent := -1
	last := keyIndex
	for j := keyIndex + 1; j < len(rawLines); j++ {
		line := rawLines[j]
		if strings.TrimSpace(line) == "" {
			body = append(body, "")
			last = j
			continue
		}
		indent := 0
		for indent < len(line) && line[indent] == ' ' {
			indent++
		}
		if indent < len(line) && line[indent] == '\t' {
			return "", last, newErr(types.CodeSkills001, ReasonTabIndent,
				"line %d: a tab is not an indent character", j+1)
		}
		if indent <= keyIndent {
			break
		}
		if blockIndent < 0 {
			blockIndent = indent
		}
		cut := blockIndent
		if len(line) < cut {
			cut = len(line)
		}
		body = append(body, line[cut:])
		last = j
	}
	if len(body) == 0 {
		return "", keyIndex, newErr(types.CodeSkills001, ReasonUnparsableLine,
			"line %d: a block scalar must carry indented text", no)
	}
	for len(body) > 0 && strings.TrimSpace(body[len(body)-1]) == "" {
		body = body[:len(body)-1]
	}
	sep := "\n"
	if strings.HasPrefix(marker, ">") {
		sep = " "
	}
	joined := strings.Join(body, sep)
	return strings.TrimSpace(joined), last, nil
}

// stripComment removes a trailing comment: a `#` that is at the start of the line or
// preceded by whitespace. A `#` inside a value (an args_json payload, a path) is left
// alone, and an apostrophe in prose is an ordinary character — this subset does not
// track quotes, which is why it never "repairs" a value it read.
func stripComment(line string) (string, error) {
	for i := 0; i < len(line); i++ {
		if line[i] != '#' {
			continue
		}
		if i == 0 || line[i-1] == ' ' || line[i-1] == '\t' {
			return line[:i], nil
		}
	}
	return line, nil
}

// splitKeyValue splits `key: value`, `key:` and the `key: >`/`key: |` block forms.
func splitKeyValue(s string) (key, val, marker string, ok bool) {
	idx := strings.Index(s, ":")
	if idx <= 0 {
		return "", "", "", false
	}
	key = strings.TrimSpace(s[:idx])
	if key == "" {
		return "", "", "", false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return "", "", "", false
		}
	}
	val = strings.TrimSpace(s[idx+1:])
	switch val {
	case ">", ">-", ">+", "|", "|-", "|+":
		return key, "", val, true
	}
	if val == "" {
		return key, "", "", true
	}
	if strings.ContainsAny(val, "|>") {
		return "", "", "", false
	}
	return key, unquote(val), "", true
}

// unquote strips one layer of matching quotes.
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '\'' && s[len(s)-1] == '\'') || (s[0] == '"' && s[len(s)-1] == '"') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// parseMapBlock parses a map at one indentation level.
func parseMapBlock(lines []fmLine, i, indent int) (map[string]any, int, *Error) {
	out := map[string]any{}
	for i < len(lines) {
		line := lines[i]
		if line.item || line.indent < indent {
			break
		}
		if line.indent > indent {
			return nil, i, newErr(types.CodeSkills001, ReasonIndentInconsistent,
				"line %d: unexpected indentation (expected column %d)", line.no, indent)
		}
		val, next, err := parseEntry(lines, i, indent)
		if err != nil {
			return nil, next, err
		}
		if _, dup := out[line.key]; dup {
			return nil, next, newErr(types.CodeSkills001, ReasonDuplicateKey,
				"line %d: key %q is declared twice in the same block", line.no, line.key)
		}
		out[line.key] = val
		i = next
	}
	return out, i, nil
}

// parseEntry parses one `key: …` line and everything indented under it.
func parseEntry(lines []fmLine, i, indent int) (any, int, *Error) {
	line := lines[i]
	if line.val != "" {
		return coerceScalar(line.val), i + 1, nil
	}
	// A `key:` with no inline value: the next line must open a block.
	if i+1 >= len(lines) {
		return nil, i + 1, newErr(types.CodeSkills001, ReasonUnparsableLine,
			"line %d: key %q has no value", line.no, line.key)
	}
	next := lines[i+1]
	if next.indent <= indent {
		return nil, i + 1, newErr(types.CodeSkills001, ReasonUnparsableLine,
			"line %d: key %q has no value (the next line is not indented)", line.no, line.key)
	}
	if next.item {
		list, end, err := parseListBlock(lines, i+1, next.indent)
		return list, end, err
	}
	child, end, err := parseMapBlock(lines, i+1, next.indent)
	return child, end, err
}

// parseListBlock parses `- key: value` items at one indentation level.
func parseListBlock(lines []fmLine, i, indent int) ([]any, int, *Error) {
	out := []any{}
	for i < len(lines) {
		line := lines[i]
		if !line.item || line.indent != indent {
			break
		}
		entry := map[string]any{}
		val, next, err := parseEntry(lines, i, line.indent)
		if err != nil {
			return nil, next, err
		}
		entry[line.key] = val
		i = next
		// Further keys of the same item sit at the item's key column.
		keyIndent := line.indent + 2
		for i < len(lines) && !lines[i].item && lines[i].indent == keyIndent {
			key := lines[i].key
			sub, n, serr := parseEntry(lines, i, keyIndent)
			if serr != nil {
				return nil, n, serr
			}
			if _, dup := entry[key]; dup {
				return nil, n, newErr(types.CodeSkills001, ReasonDuplicateKey,
					"line %d: key %q is declared twice in the same item", lines[i].no, key)
			}
			entry[key] = sub
			i = n
		}
		out = append(out, any(entry))
	}
	return out, i, nil
}
