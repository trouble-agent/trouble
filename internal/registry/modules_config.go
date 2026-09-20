package registry

// modules_config.go — config.get, config.set, config.list (SPEC-06 §3.8, §3.9).
//
// config.* reads and span-writes one config file (toml|yaml|json|env|ini,
// inferred from the extension when `format` is absent). A write replaces ONLY
// the bytes of the targeted value literal and renames a temp file into place, so
// comments, key order and formatting survive the edit (SPEC-06 §3.9). The
// descriptor, args struct, env binding and target declarations below are frozen;
// the behaviour lives in the Check/Apply/Verify bodies and the cfg* helpers.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
	"github.com/trouble-agent/trouble/internal/registry/schemagen"
	"github.com/trouble-agent/trouble/internal/types"
)

type configGetArgs struct {
	Path    string `json:"path"`
	Key     string `json:"key"`
	Default any    `json:"default,omitempty"`
}

type configSetArgs struct {
	Path   string `json:"path"`
	Key    string `json:"key" js:"pattern=^[A-Za-z0-9_.\\-]{1,128}$"`
	Value  any    `json:"value"`
	Format string `json:"format,omitempty" js:"enum=toml|yaml|json|env|ini"`
	Create bool   `json:"create,omitempty" js:"default=false"`
}

type configListArgs struct {
	Path   string `json:"path"`
	Prefix string `json:"prefix,omitempty" js:"default="`
}

type configGetModule struct{ env *moduleEnv }

var configGetDescriptor = types.Descriptor{
	Name:        "config.get",
	Version:     1,
	Schema:      schemagen.Generate("config.get", 1, configGetArgs{}),
	Scopes:      []string{"config:read"},
	Idempotency: types.IdemPure,
	CheckMode:   true,
	TimeoutS:    5,
	Mutating:    false,
}

func (m configGetModule) bind(e *moduleEnv)            { m.env = e }
func (m configGetModule) Descriptor() types.Descriptor { return configGetDescriptor }

func (m configGetModule) NormalizeArgs(args map[string]any) (map[string]any, error) {
	if err := normalizePathArgs(args, "path"); err != nil {
		return nil, err
	}
	return args, nil
}

func (m configGetModule) ProtectedTargets(args map[string]any) []protectedTarget {
	return pathTarget(args)
}

// Check is a pure read (SPEC-06 §3.4): Diff{Empty:true} with the observation in
// Summary. It never mutates and never writes.
func (m configGetModule) Check(ctx context.Context, args map[string]any) (types.Diff, error) {
	obs, err := cfgGetObserve(ctx, args)
	if err != nil {
		return types.Diff{}, err
	}
	return types.Diff{Empty: true, Summary: obs.summary()}, nil
}

// Apply is a read: Changed=false, Applied empty, the payload in Result.Output.
func (m configGetModule) Apply(ctx context.Context, args map[string]any) (types.Result, error) {
	obs, err := cfgGetObserve(ctx, args)
	if err != nil {
		return types.Result{}, err
	}
	return types.Result{Changed: false, Output: obs.output()}, nil
}

// Verify = recheck: the key is re-read and must still resolve (SPEC-06 §3.9).
func (m configGetModule) Verify(ctx context.Context, args map[string]any) (types.VerifyResult, error) {
	obs, err := cfgGetObserve(ctx, args)
	if err != nil {
		return types.VerifyResult{}, err
	}
	return types.VerifyResult{
		OK:       true,
		Method:   "recheck",
		Detail:   obs.output(),
		Evidence: []types.DiffEntry{{Path: obs.Path + "#" + obs.Key, Before: nil, After: obs.Value}},
	}, nil
}

type configSetModule struct{ env *moduleEnv }

var configSetDescriptor = types.Descriptor{
	Name:        "config.set",
	Version:     1,
	Schema:      schemagen.Generate("config.set", 1, configSetArgs{}),
	Scopes:      []string{"config:write"},
	Idempotency: types.IdemConvergent,
	CheckMode:   true,
	TimeoutS:    10,
	Mutating:    true,
}

func (m configSetModule) bind(e *moduleEnv)            { m.env = e }
func (m configSetModule) Descriptor() types.Descriptor { return configSetDescriptor }
func (m configSetModule) RollbackInvertible() bool     { return true }

func (m configSetModule) NormalizeArgs(args map[string]any) (map[string]any, error) {
	if err := normalizePathArgs(args, "path"); err != nil {
		return nil, err
	}
	return args, nil
}

func (m configSetModule) ProtectedTargets(args map[string]any) []protectedTarget {
	return pathTarget(args)
}

// Check parses the file, locates the key's value span and returns the diff it
// WOULD apply (SPEC-06 §3.9): one entry `{path:"<file>#<key>", before, after}`,
// or Diff{Empty:true} when the key already carries the target value. A missing
// key without create=true, a duplicate key, a non-UTF-8 file and a format that
// cannot be span-edited safely are permanent (TROUBLE-REGISTRY-011).
func (m configSetModule) Check(ctx context.Context, args map[string]any) (types.Diff, error) {
	plan, err := cfgPlanSet(ctx, args)
	if err != nil {
		return types.Diff{}, err
	}
	if plan.settled() {
		return types.Diff{Empty: true, Summary: fmt.Sprintf("%s#%s already %s", plan.Path, plan.Key, plan.Literal)}, nil
	}
	return types.Diff{
		Empty:   false,
		Entries: []types.DiffEntry{plan.entry()},
		Summary: fmt.Sprintf("%s#%s %s -> %s", plan.Path, plan.Key, cfgDisplay(plan.Before), plan.Literal),
	}, nil
}

// Apply replaces only the value bytes and atomically renames a temp file in the
// same directory (fsync → rename). A second Apply with identical args is a
// no-op: Changed=false with an empty Applied (SPEC-06 §3.4 convergent).
func (m configSetModule) Apply(ctx context.Context, args map[string]any) (types.Result, error) {
	plan, err := cfgPlanSet(ctx, args)
	if err != nil {
		return types.Result{}, err
	}
	out := map[string]any{
		"path":    plan.Path,
		"format":  string(plan.Format),
		"key":     plan.Key,
		"value":   plan.After,
		"present": plan.Present,
	}
	if plan.settled() {
		out["changed"] = false
		out["sha256"] = plan.sha256(plan.File.Raw)
		return types.Result{Changed: false, Output: out, Rollback: plan.rollbackHint()}, nil
	}
	if err := writeFileAtomic(plan.Path, plan.Out, 0); err != nil {
		return types.Result{}, fmt.Errorf("%w: %v", types.ErrTransient, err)
	}
	out["changed"] = true
	out["bytes"] = len(plan.Out)
	out["sha256"] = plan.sha256(plan.Out)
	return types.Result{
		Changed:  true,
		Applied:  []types.DiffEntry{plan.entry()},
		Output:   out,
		Rollback: plan.rollbackHint(),
	}, nil
}

// Verify = recheck: re-read, parse and compare the key against the value this
// call set (SPEC-06 §3.9: mismatch is TROUBLE-REGISTRY-005).
func (m configSetModule) Verify(ctx context.Context, args map[string]any) (types.VerifyResult, error) {
	plan, err := cfgPlanSet(ctx, args)
	if err != nil {
		return types.VerifyResult{}, err
	}
	detail := map[string]any{
		"path":     plan.Path,
		"format":   string(plan.Format),
		"key":      plan.Key,
		"expected": plan.After,
		"observed": plan.Before,
		"sha256":   plan.sha256(plan.File.Raw),
	}
	if !plan.settled() {
		detail["ok_reason"] = "the key does not carry the applied value"
		return types.VerifyResult{OK: false, Method: "recheck", Detail: detail}, nil
	}
	return types.VerifyResult{
		OK:       true,
		Method:   "recheck",
		Detail:   detail,
		Evidence: []types.DiffEntry{plan.entry()},
	}, nil
}

type configListModule struct{ env *moduleEnv }

var configListDescriptor = types.Descriptor{
	Name:        "config.list",
	Version:     1,
	Schema:      schemagen.Generate("config.list", 1, configListArgs{}),
	Scopes:      []string{"config:read"},
	Idempotency: types.IdemPure,
	CheckMode:   true,
	TimeoutS:    5,
	Mutating:    false,
}

func (m configListModule) bind(e *moduleEnv)            { m.env = e }
func (m configListModule) Descriptor() types.Descriptor { return configListDescriptor }

func (m configListModule) NormalizeArgs(args map[string]any) (map[string]any, error) {
	if err := normalizePathArgs(args, "path"); err != nil {
		return nil, err
	}
	return args, nil
}

func (m configListModule) ProtectedTargets(args map[string]any) []protectedTarget {
	return pathTarget(args)
}

// Check is a pure read: Diff{Empty:true} with the observation in Summary.
func (m configListModule) Check(ctx context.Context, args map[string]any) (types.Diff, error) {
	list, err := cfgListObserve(ctx, args)
	if err != nil {
		return types.Diff{}, err
	}
	return types.Diff{Empty: true, Summary: list.summary()}, nil
}

// Apply is a read: Changed=false with the key table in Result.Output.
func (m configListModule) Apply(ctx context.Context, args map[string]any) (types.Result, error) {
	list, err := cfgListObserve(ctx, args)
	if err != nil {
		return types.Result{}, err
	}
	return types.Result{Changed: false, Output: list.output()}, nil
}

// Verify = recheck: the file is re-read and every listed key must still resolve.
func (m configListModule) Verify(ctx context.Context, args map[string]any) (types.VerifyResult, error) {
	list, err := cfgListObserve(ctx, args)
	if err != nil {
		return types.VerifyResult{}, err
	}
	return types.VerifyResult{
		OK:       true,
		Method:   "recheck",
		Detail:   list.output(),
		Evidence: list.evidence(),
	}, nil
}

// ---------------------------------------------------------------------------
// config.* reads
// ---------------------------------------------------------------------------

// cfgObservation is one config.get read: the resolved key, its value and the
// file identity (SPEC-06 §3.9). config.get is pure, so the observation is both
// the Check summary and the Apply payload.
type cfgObservation struct {
	Path    string
	Format  configFormat
	Key     string
	Value   any
	Present bool
	UsedDef bool
	SHA256  string
	Size    int
}

func cfgGetObserve(ctx context.Context, args map[string]any) (cfgObservation, error) {
	if err := ctx.Err(); err != nil {
		return cfgObservation{}, fmt.Errorf("%w: %v", types.ErrTransient, err)
	}
	var a configGetArgs
	if err := decodeArgs(args, &a); err != nil {
		return cfgObservation{}, err
	}
	_, hasDefault := args["default"]
	f, err := cfgLoad(a.Path, "")
	if err != nil {
		return cfgObservation{}, err
	}
	obs := cfgObservation{Path: a.Path, Format: f.Format, Key: a.Key, SHA256: f.sha256(), Size: len(f.Raw)}
	val, present := f.lookup(a.Key)
	if !present {
		if !hasDefault {
			return cfgObservation{}, fmt.Errorf("%w: %s carries no key %q (pass default= to read an absent key)", types.ErrPermanent, a.Path, a.Key)
		}
		obs.UsedDef = true
		obs.Value = a.Default
		return obs, nil
	}
	obs.Present = true
	obs.Value = val
	return obs, nil
}

func (o cfgObservation) summary() string {
	where := "key"
	if !o.Present {
		where = "default"
	}
	return fmt.Sprintf("%s=%s (%s, %s, %s)", o.Key, cfgDisplay(o.Value), where, o.Format, o.Path)
}

func (o cfgObservation) output() map[string]any {
	out := map[string]any{
		"path":    o.Path,
		"format":  string(o.Format),
		"key":     o.Key,
		"value":   o.Value,
		"present": o.Present,
		"sha256":  o.SHA256,
		"bytes":   o.Size,
	}
	if !o.Present {
		out["source"] = "default"
	} else {
		out["source"] = "key"
	}
	return out
}

// cfgListing is one config.list read: every leaf key (optionally under `prefix`)
// with its value, in stable sorted order.
type cfgListing struct {
	Path   string
	Format configFormat
	Prefix string
	Keys   []string
	Values map[string]any
	Total  int
	SHA256 string
	Size   int
}

func cfgListObserve(ctx context.Context, args map[string]any) (cfgListing, error) {
	if err := ctx.Err(); err != nil {
		return cfgListing{}, fmt.Errorf("%w: %v", types.ErrTransient, err)
	}
	var a configListArgs
	if err := decodeArgs(args, &a); err != nil {
		return cfgListing{}, err
	}
	f, err := cfgLoad(a.Path, "")
	if err != nil {
		return cfgListing{}, err
	}
	list := cfgListing{
		Path: a.Path, Format: f.Format, Prefix: a.Prefix,
		Values: map[string]any{}, SHA256: f.sha256(), Size: len(f.Raw),
	}
	for _, k := range f.leafKeys() {
		list.Total++
		if !strings.HasPrefix(k, a.Prefix) {
			continue
		}
		list.Keys = append(list.Keys, k)
		list.Values[k] = f.Values[k]
	}
	return list, nil
}

func (l cfgListing) pairs() []any {
	out := make([]any, 0, len(l.Keys))
	for _, k := range l.Keys {
		out = append(out, map[string]any{"key": k, "value": l.Values[k]})
	}
	return out
}

func (l cfgListing) summary() string {
	where := ""
	if l.Prefix != "" {
		where = fmt.Sprintf(" under prefix %q", l.Prefix)
	}
	return fmt.Sprintf("%d of %d keys%s (%s, %s)", len(l.Keys), l.Total, where, l.Format, l.Path)
}

func (l cfgListing) output() map[string]any {
	return map[string]any{
		"path":   l.Path,
		"format": string(l.Format),
		"prefix": l.Prefix,
		"count":  len(l.Keys),
		"total":  l.Total,
		"keys":   l.pairs(),
		"sha256": l.SHA256,
		"bytes":  l.Size,
	}
}

// evidence caps the per-key evidence list so a large config cannot balloon the
// audit record (the count always rides in Detail).
const cfgEvidenceMax = 64

func (l cfgListing) evidence() []types.DiffEntry {
	out := make([]types.DiffEntry, 0, len(l.Keys))
	for _, k := range l.Keys {
		if len(out) == cfgEvidenceMax {
			break
		}
		out = append(out, types.DiffEntry{Path: l.Path + "#" + k, Before: nil, After: l.Values[k]})
	}
	return out
}

// ---------------------------------------------------------------------------
// config.set planning
// ---------------------------------------------------------------------------

// cfgSetPlan is what config.set Check predicts and Apply performs: the exact
// bytes the file would carry, plus the before/after pair of the one value.
type cfgSetPlan struct {
	File    *cfgFile
	Path    string
	Key     string
	Format  configFormat
	Before  any
	After   any
	Present bool
	Create  bool
	Literal string
	// PresentLiteral is the raw value text the file carries; convergence is
	// judged on the bytes (the literal that would be written), so a numeric arg
	// against a text dialect (env/ini) does not re-write the same value twice.
	PresentLiteral string
	Out            []byte
}

func cfgPlanSet(ctx context.Context, args map[string]any) (cfgSetPlan, error) {
	if err := ctx.Err(); err != nil {
		return cfgSetPlan{}, fmt.Errorf("%w: %v", types.ErrTransient, err)
	}
	var a configSetArgs
	if err := decodeArgs(args, &a); err != nil {
		return cfgSetPlan{}, err
	}
	if a.Key == "" {
		return cfgSetPlan{}, fmt.Errorf("%w: config.set without a key", types.ErrPermanent)
	}
	f, err := cfgLoadForWrite(a.Path, a.Format, a.Create)
	if err != nil {
		return cfgSetPlan{}, err
	}
	plan := cfgSetPlan{File: f, Path: a.Path, Key: a.Key, Format: f.Format, After: a.Value, Create: a.Create}
	before, present := f.lookup(a.Key)
	plan.Before, plan.Present = before, present

	literal, err := cfgEncodeLiteral(f.Format, a.Value)
	if err != nil {
		return plan, err
	}
	plan.Literal = literal

	if present {
		span, ok := f.Spans[a.Key]
		if !ok || !span.Editable {
			return plan, fmt.Errorf("%w: %s#%s is a block/table value that cannot be span-edited safely; set one of its leaves", types.ErrPermanent, a.Path, a.Key)
		}
		plan.PresentLiteral = string(f.Raw[span.Start:span.End])
		out := make([]byte, 0, len(f.Raw)+len(literal))
		out = append(out, f.Raw[:span.Start]...)
		out = append(out, literal...)
		out = append(out, f.Raw[span.End:]...)
		plan.Out = out
		return plan, nil
	}
	if !a.Create {
		return plan, fmt.Errorf("%w: %s carries no key %q and create is false", types.ErrPermanent, a.Path, a.Key)
	}
	at, text, err := f.insertFor(a.Key, literal)
	if err != nil {
		return plan, err
	}
	out := make([]byte, 0, len(f.Raw)+len(text))
	out = append(out, f.Raw[:at]...)
	out = append(out, text...)
	out = append(out, f.Raw[at:]...)
	plan.Out = out
	return plan, nil
}

// settled reports whether the value span already holds exactly the literal that
// would be written: the convergent no-op the second Apply must return
// (SPEC-06 §3.4). Comparing the bytes rather than the parsed values also makes a
// numeric arg against a text dialect (env/ini) converge on the first write.
func (p cfgSetPlan) settled() bool {
	return p.Present && p.PresentLiteral == p.Literal
}

func (p cfgSetPlan) entry() types.DiffEntry {
	return types.DiffEntry{Path: p.Path + "#" + p.Key, Before: p.Before, After: p.After}
}

func (p cfgSetPlan) sha256(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// rollbackHint inverts the write with the `before` value (SPEC-06 §3.5).
func (p cfgSetPlan) rollbackHint() *types.RollbackHint {
	return &types.RollbackHint{
		Supported: true,
		Module:    "config.set",
		Args:      map[string]any{"path": p.Path, "key": p.Key, "value": p.Before},
	}
}

// ---------------------------------------------------------------------------
// config formats: load, parse, encode, locate
// ---------------------------------------------------------------------------

// configFormat is one of the five config dialects config.* span-edits
// (SPEC-06 §3.8: toml|yaml|json|env|ini).
type configFormat string

const (
	cfgFormatTOML configFormat = "toml"
	cfgFormatYAML configFormat = "yaml"
	cfgFormatJSON configFormat = "json"
	cfgFormatENV  configFormat = "env"
	cfgFormatINI  configFormat = "ini"
)

// cfgExtensions maps a file extension onto a format when `format` is absent.
var cfgExtensions = map[string]configFormat{
	".toml": cfgFormatTOML,
	".yaml": cfgFormatYAML,
	".yml":  cfgFormatYAML,
	".json": cfgFormatJSON,
	".env":  cfgFormatENV,
	".ini":  cfgFormatINI,
	".conf": cfgFormatINI,
}

// cfgFormatFor resolves the format: an explicit `format` wins, otherwise the
// extension decides. An unresolvable format is permanent — never a guess.
func cfgFormatFor(path, explicit string) (configFormat, error) {
	if explicit != "" {
		f := configFormat(explicit)
		switch f {
		case cfgFormatTOML, cfgFormatYAML, cfgFormatJSON, cfgFormatENV, cfgFormatINI:
			return f, nil
		}
		return "", fmt.Errorf("%w: unknown config format %q (toml|yaml|json|env|ini)", types.ErrPermanent, explicit)
	}
	if f, ok := cfgExtensions[strings.ToLower(filepath.Ext(path))]; ok {
		return f, nil
	}
	return "", fmt.Errorf("%w: cannot infer a config format from %s (extension %q); pass format=toml|yaml|json|env|ini",
		types.ErrPermanent, path, filepath.Ext(path))
}

// cfgSpan is where one key's value literal lives in the raw bytes.
type cfgSpan struct {
	Start    int
	End      int
	Editable bool
}

// cfgFile is a parsed config file: the raw bytes, the flattened key → value map
// (the `.` path is the nesting separator for toml/yaml/json and the section
// prefix for ini) and the key → value span map config.set edits.
type cfgFile struct {
	Format configFormat
	Path   string
	Raw    []byte
	Values map[string]any
	Spans  map[string]cfgSpan
	// sections names the table/section headers the file declares, so a create
	// can append inside an existing block instead of redefining it.
	sections map[string]bool
}

func (f *cfgFile) sha256() string {
	sum := sha256.Sum256(f.Raw)
	return hex.EncodeToString(sum[:])
}

// lookup returns a key's value; present is false for an unknown key.
func (f *cfgFile) lookup(key string) (any, bool) {
	v, ok := f.Values[key]
	return v, ok
}

// leafKeys lists the scalar/array keys — the keys config.list reports — sorted.
func (f *cfgFile) leafKeys() []string {
	out := make([]string, 0, len(f.Values))
	for k, v := range f.Values {
		if _, isMap := v.(map[string]any); isMap {
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// cfgLoad reads and parses an existing config file. A missing file, invalid
// UTF-8 or a parse failure is permanent (SPEC-06 §6.12, §3.9).
func cfgLoad(path string, explicit string) (*cfgFile, error) {
	format, err := cfgFormatFor(path, explicit)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot read %s: %v", types.ErrPermanent, path, err)
	}
	return cfgParse(path, format, raw)
}

// cfgLoadForWrite loads the target of a write; create=true turns a missing file
// into its empty document form so a first key can be added.
func cfgLoadForWrite(path, explicit string, create bool) (*cfgFile, error) {
	format, err := cfgFormatFor(path, explicit)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		return cfgParse(path, format, raw)
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("%w: cannot read %s: %v", types.ErrPermanent, path, err)
	case !create:
		return nil, fmt.Errorf("%w: %s does not exist and create is false", types.ErrPermanent, path)
	}
	return cfgParse(path, format, cfgEmptyDocument(format))
}

// cfgEmptyDocument is the empty form of each format (create=true on a new file).
func cfgEmptyDocument(format configFormat) []byte {
	if format == cfgFormatJSON {
		return []byte("{}")
	}
	return []byte{}
}

// cfgParse parses raw bytes in the given format.
func cfgParse(path string, format configFormat, raw []byte) (*cfgFile, error) {
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("%w: %s is not valid UTF-8; a non-UTF-8 config cannot be span-edited (SPEC-06 §6.12)", types.ErrPermanent, path)
	}
	f := &cfgFile{
		Format: format, Path: path, Raw: raw,
		Values: map[string]any{}, Spans: map[string]cfgSpan{}, sections: map[string]bool{},
	}
	var err error
	switch format {
	case cfgFormatTOML:
		err = f.parseTOML()
	case cfgFormatYAML:
		err = f.parseYAML()
	case cfgFormatJSON:
		err = f.parseJSON()
	case cfgFormatENV:
		err = f.parseENV()
	case cfgFormatINI:
		err = f.parseINI()
	default:
		err = fmt.Errorf("%w: unsupported config format %q", types.ErrPermanent, format)
	}
	if err != nil {
		return nil, err
	}
	for k, v := range f.Values {
		f.Values[k] = cfgNormalizeValue(v)
	}
	return f, nil
}

// insertFor computes the create=true insertion: the byte offset and the exact
// text appended, so a missing key is added without rewriting what is there.
func (f *cfgFile) insertFor(key, literal string) (int, string, error) {
	switch f.Format {
	case cfgFormatTOML:
		return f.insertTOML(key, literal)
	case cfgFormatYAML:
		return f.insertYAML(key, literal)
	case cfgFormatJSON:
		return f.insertJSON(key, literal)
	case cfgFormatENV:
		return f.insertENV(key, literal)
	case cfgFormatINI:
		return f.insertINI(key, literal)
	}
	return 0, "", fmt.Errorf("%w: unsupported config format %q", types.ErrPermanent, f.Format)
}

// ---- shared helpers -------------------------------------------------------

// cfgLine is one line of a config file with its absolute byte offsets.
type cfgLine struct {
	Start, End int
	Body       string
}

// cfgLines splits raw into lines, keeping the terminator in End.
func cfgLines(raw []byte) []cfgLine {
	var out []cfgLine
	for i := 0; i < len(raw); {
		nl := bytes.IndexByte(raw[i:], '\n')
		end := len(raw)
		if nl >= 0 {
			end = i + nl + 1
		}
		body := strings.TrimRight(string(raw[i:end]), "\r\n")
		out = append(out, cfgLine{Start: i, End: end, Body: body})
		i = end
	}
	return out
}

// cfgIndentBefore returns the leading whitespace of the line holding pos.
func cfgIndentBefore(raw []byte, pos int) string {
	start := bytes.LastIndexByte(raw[:pos], '\n') + 1
	end := start
	for end < len(raw) && (raw[end] == ' ' || raw[end] == '\t') {
		end++
	}
	return string(raw[start:end])
}

// cfgNewline supplies a leading newline when an insertion at off would land on
// an unterminated last line.
func cfgNewline(raw []byte, off int) string {
	if off > 0 && off <= len(raw) && raw[off-1] != '\n' {
		return "\n"
	}
	return ""
}

// cfgIndexOutsideString returns the index of ch in s, ignoring quoted spans.
func cfgIndexOutsideString(s string, ch byte) int {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == ch:
			return i
		}
	}
	return -1
}

// cfgStripComment removes a trailing comment ('#' for toml/env/yaml, ';' or '#'
// for ini) that starts outside quotes and is preceded by whitespace.
func cfgStripComment(s string, marks string) string {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case strings.IndexByte(marks, c) >= 0:
			if i > 0 && (s[i-1] == ' ' || s[i-1] == '\t') {
				return s[:i]
			}
		}
	}
	return s
}

// cfgValuesEqual compares a parsed value against an args value; JSON numbers and
// Go ints/floats are folded onto one numeric form so `8080`, `8080.0` and
// json.Number("8080") are the same value.
func cfgValuesEqual(a, b any) bool {
	ab, err1 := json.Marshal(cfgNormalizeValue(a))
	bb, err2 := json.Marshal(cfgNormalizeValue(b))
	if err1 != nil || err2 != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}

// cfgNormalizeValue folds interface{} inputs onto their canonical JSON form.
func cfgNormalizeValue(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case json.Number:
		return cfgNumber(t.String())
	case int:
		return cfgNumber(strconv.Itoa(t))
	case int32:
		return cfgNumber(strconv.FormatInt(int64(t), 10))
	case int64:
		return cfgNumber(strconv.FormatInt(t, 10))
	case float64:
		return cfgFloat(t)
	case float32:
		return cfgFloat(float64(t))
	case []any:
		out := make([]any, 0, len(t))
		for _, item := range t {
			out = append(out, cfgNormalizeValue(item))
		}
		return out
	case []string:
		out := make([]any, 0, len(t))
		for _, item := range t {
			out = append(out, item)
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for k, item := range t {
			out[k] = cfgNormalizeValue(item)
		}
		return out
	}
	return v
}

// cfgNumber canonicalizes a numeric literal: an integral form becomes an int.
func cfgNumber(lit string) any {
	if n, err := strconv.ParseInt(lit, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(lit, 64); err == nil {
		return f
	}
	return lit
}

func cfgFloat(f float64) any {
	if f == float64(int64(f)) {
		return int64(f)
	}
	return f
}

// cfgDisplay renders a value for the one-line summaries.
func cfgDisplay(v any) string {
	if v == nil {
		return "null"
	}
	b, err := json.Marshal(cfgNormalizeValue(v))
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	if len(b) > 80 {
		return string(b[:77]) + "..."
	}
	return string(b)
}

// cfgEncodeLiteral renders an args value as the format's value literal.
func cfgEncodeLiteral(format configFormat, v any) (string, error) {
	switch format {
	case cfgFormatTOML:
		return cfgEncodeTOML(v)
	case cfgFormatYAML:
		return cfgEncodeYAML(v)
	case cfgFormatJSON:
		return cfgEncodeJSON(v)
	case cfgFormatENV:
		return cfgEncodeENV(v)
	case cfgFormatINI:
		return cfgEncodeINI(v)
	}
	return "", fmt.Errorf("%w: unsupported config format %q", types.ErrPermanent, format)
}

func cfgScalar(v any) (kind string, lit string, ok bool) {
	switch t := v.(type) {
	case nil:
		return "null", "", true
	case bool:
		if t {
			return "bool", "true", true
		}
		return "bool", "false", true
	case string:
		return "string", t, true
	case json.Number:
		return "number", t.String(), true
	case int:
		return "number", strconv.Itoa(t), true
	case int64:
		return "number", strconv.FormatInt(t, 10), true
	case float64:
		return "number", strconv.FormatFloat(t, 'g', -1, 64), true
	}
	return "", "", false
}

func cfgEncodeJSON(v any) (string, error) {
	b, err := json.Marshal(cfgNormalizeValue(v))
	if err != nil {
		return "", fmt.Errorf("%w: value cannot be written as JSON: %v", types.ErrPermanent, err)
	}
	return string(b), nil
}

// cfgQuoteTOML renders a TOML basic string (valid TOML escapes only).
func cfgQuoteTOML(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString("\\\"")
		case '\\':
			b.WriteString("\\\\")
		case '\n':
			b.WriteString("\\n")
		case '\r':
			b.WriteString("\\r")
		case '\t':
			b.WriteString("\\t")
		case '\b':
			b.WriteString("\\b")
		case '\f':
			b.WriteString("\\f")
		default:
			if r < 0x20 || r == 0x7f {
				b.WriteString(fmt.Sprintf("\\u%04X", r))
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func cfgEncodeTOML(v any) (string, error) {
	switch t := v.(type) {
	case []any:
		parts := make([]string, 0, len(t))
		for _, item := range t {
			lit, err := cfgEncodeTOML(item)
			if err != nil {
				return "", err
			}
			parts = append(parts, lit)
		}
		return "[" + strings.Join(parts, ", ") + "]", nil
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			lit, err := cfgEncodeTOML(t[k])
			if err != nil {
				return "", err
			}
			parts = append(parts, cfgTomlKey(k)+" = "+lit)
		}
		return "{" + strings.Join(parts, ", ") + "}", nil
	}
	kind, lit, ok := cfgScalar(v)
	if !ok {
		return "", fmt.Errorf("%w: %T cannot be written as a config value", types.ErrPermanent, v)
	}
	if kind == "string" {
		return cfgQuoteTOML(lit), nil
	}
	if kind == "null" {
		return "", fmt.Errorf("%w: a null value cannot be written as TOML/INI/ENV (unset the key instead)", types.ErrPermanent)
	}
	return lit, nil
}

// cfgTomlKey renders one TOML key segment (quoting when it is not a bare key).
func cfgTomlKey(seg string) string {
	if seg == "" {
		return `""`
	}
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return cfgQuoteTOML(seg)
		}
	}
	return seg
}

// cfgYAMLPlain reports whether a string can be written as a YAML plain scalar.
func cfgYAMLPlain(s string) bool {
	if s == "" {
		return false
	}
	switch strings.ToLower(s) {
	case "true", "false", "yes", "no", "on", "off", "null", "~":
		return false
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return false
	}
	if strings.ContainsAny(s, "\n\t#:{}[],&*!|>'\"%@`\\") {
		return false
	}
	if s[0] == ' ' || s[len(s)-1] == ' ' || s[0] == '-' {
		return false
	}
	return true
}

func cfgEncodeYAML(v any) (string, error) {
	switch t := v.(type) {
	case []any:
		parts := make([]string, 0, len(t))
		for _, item := range t {
			lit, err := cfgEncodeYAML(item)
			if err != nil {
				return "", err
			}
			parts = append(parts, lit)
		}
		return "[" + strings.Join(parts, ", ") + "]", nil
	case map[string]any:
		return "", fmt.Errorf("%w: a mapping value cannot be written as one YAML scalar (set one of its leaves)", types.ErrPermanent)
	}
	kind, lit, ok := cfgScalar(v)
	if !ok {
		return "", fmt.Errorf("%w: %T cannot be written as a config value", types.ErrPermanent, v)
	}
	switch kind {
	case "string":
		if cfgYAMLPlain(lit) {
			return lit, nil
		}
		b, err := json.Marshal(lit)
		if err != nil {
			return "", fmt.Errorf("%w: %v", types.ErrPermanent, err)
		}
		return string(b), nil
	case "null":
		return "null", nil
	}
	return lit, nil
}

// cfgEncodeENV renders a `KEY=value` assignment's right-hand side.
func cfgEncodeENV(v any) (string, error) {
	kind, lit, ok := cfgScalar(v)
	if !ok {
		return "", fmt.Errorf("%w: %T cannot be written as an env value", types.ErrPermanent, v)
	}
	if kind == "null" {
		return "", fmt.Errorf("%w: a null value cannot be written as an env value", types.ErrPermanent)
	}
	if kind != "string" {
		return lit, nil
	}
	if lit == "" || strings.ContainsAny(lit, " \t#'\"$\\") {
		b, err := json.Marshal(lit)
		if err != nil {
			return "", fmt.Errorf("%w: %v", types.ErrPermanent, err)
		}
		return string(b), nil
	}
	return lit, nil
}

// cfgEncodeINI renders an INI value: INI has no portable quoting, so a value
// that cannot be written verbatim on one line is permanent.
func cfgEncodeINI(v any) (string, error) {
	kind, lit, ok := cfgScalar(v)
	if !ok {
		return "", fmt.Errorf("%w: %T cannot be written as an INI value", types.ErrPermanent, v)
	}
	if kind == "null" {
		return "", fmt.Errorf("%w: a null value cannot be written as an INI value", types.ErrPermanent)
	}
	if strings.ContainsAny(lit, "\n\r\x00") {
		return "", fmt.Errorf("%w: an INI value with a line break cannot be written on one line", types.ErrPermanent)
	}
	if strings.TrimSpace(lit) != lit {
		return "", fmt.Errorf("%w: an INI value with leading/trailing whitespace cannot be represented portably", types.ErrPermanent)
	}
	return lit, nil
}

// cfgSplitTopLevel splits s on sep outside quotes and brackets.
func cfgSplitTopLevel(s string, sep byte) []string {
	var out []string
	depth := 0
	var quote byte
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '[' || c == '{' || c == '(':
			depth++
		case c == ']' || c == '}' || c == ')':
			depth--
		case c == sep && depth == 0:
			out = append(out, strings.TrimSpace(s[start:i]))
			start = i + 1
		}
	}
	out = append(out, strings.TrimSpace(s[start:]))
	return out
}

// cfgKeyPath splits a dotted config key into its segments.
func cfgKeyPath(key string) []string {
	parts := strings.Split(key, ".")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// ---- TOML -----------------------------------------------------------------

// parseTOML parses with the repository's TOML decoder (the authoritative value
// source, which also refuses duplicate keys) and locates each value span with a
// line scanner so config.set can replace only the value bytes.
func (f *cfgFile) parseTOML() error {
	var tree map[string]any
	if _, err := toml.Decode(string(f.Raw), &tree); err != nil {
		return fmt.Errorf("%w: %s is not valid TOML: %v", types.ErrPermanent, f.Path, err)
	}
	cfgFlattenValues("", tree, f.Values)
	return f.scanTOMLSpans()
}

// cfgFlattenValues flattens a decoded document into dotted keys; a table is kept
// as its own (non-span-editable) key so config.get can read a whole section.
func cfgFlattenValues(prefix string, in map[string]any, out map[string]any) {
	for k, v := range in {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		if m, ok := v.(map[string]any); ok {
			out[key] = m
			cfgFlattenValues(key, m, out)
			continue
		}
		out[key] = v
	}
}

func (f *cfgFile) scanTOMLSpans() error {
	section := ""
	arraySection := false
	for _, ln := range cfgLines(f.Raw) {
		text := ln.Body
		trimmed := strings.TrimSpace(text)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			isArray := strings.HasPrefix(trimmed, "[[")
			body := strings.TrimSuffix(trimmed, "]")
			if isArray {
				body = strings.TrimSuffix(body, "]")
			}
			name := strings.TrimSpace(strings.TrimPrefix(body, "["))
			section = strings.Join(cfgParseTomlKeyPath(name), ".")
			arraySection = isArray
			f.sections[section] = true
			f.Spans[section] = cfgSpan{Editable: false}
			continue
		}
		eq := cfgIndexOutsideString(text, '=')
		if eq < 0 {
			return fmt.Errorf("%w: %s: cannot parse the TOML line %q", types.ErrPermanent, f.Path, trimmed)
		}
		key := strings.Join(cfgParseTomlKeyPath(strings.TrimSpace(text[:eq])), ".")
		if section != "" {
			key = section + "." + key
		}
		if _, dup := f.Spans[key]; dup {
			return fmt.Errorf("%w: %s declares %q twice; a duplicate key cannot be span-edited safely", types.ErrPermanent, f.Path, key)
		}
		start, end, err := cfgScanTOMLValue(f.Raw, ln.Start+eq+1)
		if err != nil {
			return fmt.Errorf("%w: %s: %v", types.ErrPermanent, f.Path, err)
		}
		f.Spans[key] = cfgSpan{Start: start, End: end, Editable: !arraySection}
	}
	return nil
}

// cfgParseTomlKeyPath parses a (possibly dotted, possibly quoted) TOML key.
func cfgParseTomlKeyPath(s string) []string {
	parts := cfgSplitTopLevel(s, '.')
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if len(p) >= 2 && (p[0] == '"' || p[0] == '\'') && p[len(p)-1] == p[0] {
			var decoded string
			if p[0] == '"' && json.Unmarshal([]byte(p), &decoded) == nil {
				out = append(out, decoded)
				continue
			}
			if p[0] == '\'' {
				out = append(out, p[1:len(p)-1])
				continue
			}
		}
		out = append(out, p)
	}
	return out
}

// cfgScanTOMLValue returns the span [start, end) of the TOML value that begins
// at or after i: strings, arrays and inline tables are walked so a multi-line
// array is covered, and a trailing comment is left outside the replaced span.
func cfgScanTOMLValue(raw []byte, i int) (int, int, error) {
	j := i
	for j < len(raw) && (raw[j] == ' ' || raw[j] == '\t') {
		j++
	}
	start := j
	depth := 0
	for j < len(raw) {
		switch c := raw[j]; c {
		case '"', '\'':
			next, err := cfgSkipQuoted(raw, j, c)
			if err != nil {
				return 0, 0, err
			}
			j = next
		case '[', '{':
			depth++
			j++
		case ']', '}':
			if depth > 0 {
				depth--
			}
			j++
		case '#':
			if depth == 0 {
				return start, cfgTrimEnd(raw, start, j), nil
			}
			j++
		case '\n':
			if depth == 0 {
				return start, cfgTrimEnd(raw, start, j), nil
			}
			j++
		default:
			j++
		}
	}
	return start, cfgTrimEnd(raw, start, j), nil
}

// cfgSkipQuoted returns the index just past a quoted run starting at i.
func cfgSkipQuoted(raw []byte, i int, q byte) (int, error) {
	triple := i+2 < len(raw) && raw[i+1] == q && raw[i+2] == q
	if triple {
		j := i + 3
		for j < len(raw) {
			if raw[j] == '\\' && q == '"' {
				j += 2
				continue
			}
			if raw[j] == q && j+2 < len(raw) && raw[j+1] == q && raw[j+2] == q {
				return j + 3, nil
			}
			j++
		}
		return 0, fmt.Errorf("unterminated triple-quoted string")
	}
	j := i + 1
	for j < len(raw) {
		if raw[j] == '\\' && q == '"' {
			j += 2
			continue
		}
		if raw[j] == '\n' {
			return 0, fmt.Errorf("unterminated string")
		}
		if raw[j] == q {
			return j + 1, nil
		}
		j++
	}
	return 0, fmt.Errorf("unterminated string")
}

// cfgTrimEnd trims trailing blanks off raw[start:end].
func cfgTrimEnd(raw []byte, start, end int) int {
	for end > start && (raw[end-1] == ' ' || raw[end-1] == '\t' || raw[end-1] == '\r') {
		end--
	}
	return end
}

// insertTOML appends a missing key: into the section when it exists, before the
// first section header for a top-level key, otherwise as a new section at EOF.
func (f *cfgFile) insertTOML(key, literal string) (int, string, error) {
	segments := cfgKeyPath(key)
	if len(segments) == 0 {
		return 0, "", fmt.Errorf("%w: empty config key", types.ErrPermanent)
	}
	leaf := segments[len(segments)-1]
	parent := strings.Join(segments[:len(segments)-1], ".")
	assign := cfgTomlKey(leaf) + " = " + literal + "\n"

	if parent != "" && f.hasTOMLSection(parent) {
		at := f.tomlBlockEnd(parent)
		return at, cfgNewline(f.Raw, at) + assign, nil
	}
	if parent == "" {
		at, err := f.beforeFirstTOMLSection()
		if err != nil {
			return 0, "", err
		}
		return at, cfgNewline(f.Raw, at) + assign, nil
	}
	if _, isTable := f.Values[parent]; isTable {
		// The parent is an inline table: appending a `[parent]` header would
		// redefine it, so the create is refused rather than guessed.
		return 0, "", fmt.Errorf("%w: %s declares %q as an inline table; a key cannot be appended to it", types.ErrPermanent, f.Path, parent)
	}
	// The parent table does not exist yet: define it at EOF (a table may be
	// defined after a deeper one already created it implicitly).
	at := len(f.Raw)
	header := "[" + cfgTomlSectionPath(segments[:len(segments)-1]) + "]\n"
	sep := cfgNewline(f.Raw, at)
	if len(f.Raw) > 0 && sep == "" && !bytes.HasSuffix(f.Raw, []byte("\n\n")) {
		sep = "\n" // keep a blank line before a new section
	}
	return at, sep + header + assign, nil
}

func cfgTomlSectionPath(segments []string) string {
	parts := make([]string, 0, len(segments))
	for _, s := range segments {
		parts = append(parts, cfgTomlKey(s))
	}
	return strings.Join(parts, ".")
}

func (f *cfgFile) hasTOMLSection(name string) bool { return f.sections[name] }

// tomlBlockEnd returns the offset where another assignment may be added to a
// section: the start of the next section header, or EOF.
func (f *cfgFile) tomlBlockEnd(name string) int {
	lines := cfgLines(f.Raw)
	in := false
	for _, ln := range lines {
		trimmed := strings.TrimSpace(ln.Body)
		if strings.HasPrefix(trimmed, "[") {
			header := strings.Join(cfgParseTomlKeyPath(strings.Trim(strings.TrimSpace(trimmed), "[]")), ".")
			if in && header != name {
				return ln.Start
			}
			if header == name {
				in = true
				continue
			}
		}
	}
	if in {
		return len(f.Raw)
	}
	return len(f.Raw)
}

// beforeFirstTOMLSection returns the offset of the first section header (top
// level keys must precede any table) or EOF.
func (f *cfgFile) beforeFirstTOMLSection() (int, error) {
	for _, ln := range cfgLines(f.Raw) {
		if strings.HasPrefix(strings.TrimSpace(ln.Body), "[") {
			return ln.Start, nil
		}
	}
	return len(f.Raw), nil
}

// ---- JSON -----------------------------------------------------------------

// cfgJSONNode is one member of a JSON document: the span of its value and, for
// object values, what config.set needs to insert a new member into it.
type cfgJSONNode struct {
	IsObject     bool
	ValueStart   int
	ValueEnd     int
	ObjClose     int
	Indent       string
	Members      int
	LastValueEnd int
}

type cfgJSONScanner struct {
	raw    []byte
	i      int
	values map[string]any
	nodes  map[string]cfgJSONNode
}

func (f *cfgFile) parseJSON() error {
	s := &cfgJSONScanner{raw: f.Raw, values: f.Values, nodes: map[string]cfgJSONNode{}}
	if err := s.parseValue(""); err != nil {
		return fmt.Errorf("%w: %s is not valid JSON: %v", types.ErrPermanent, f.Path, err)
	}
	s.skip()
	if s.i != len(s.raw) {
		return fmt.Errorf("%w: %s carries trailing content after the root value", types.ErrPermanent, f.Path)
	}
	for path, node := range s.nodes {
		f.Spans[path] = cfgSpan{Start: node.ValueStart, End: node.ValueEnd, Editable: path != "" && !node.IsObject}
	}
	return nil
}

func (s *cfgJSONScanner) skip() {
	for s.i < len(s.raw) {
		switch s.raw[s.i] {
		case ' ', '\t', '\r', '\n':
			s.i++
		default:
			return
		}
	}
}

func (s *cfgJSONScanner) parseValue(path string) error {
	s.skip()
	if s.i >= len(s.raw) {
		return fmt.Errorf("unexpected end of document")
	}
	switch s.raw[s.i] {
	case '{':
		return s.parseObject(path)
	case '[':
		start := s.i
		end, err := s.skipContainer('[', ']')
		if err != nil {
			return err
		}
		v, err := cfgJSONValue(s.raw[start:end])
		if err != nil {
			return err
		}
		if path != "" {
			s.values[path] = v
			s.nodes[path] = cfgJSONNode{ValueStart: start, ValueEnd: end}
		}
		return nil
	}
	start := s.i
	j := s.i
	for j < len(s.raw) && !strings.ContainsRune(",}] \t\r\n", rune(s.raw[j])) {
		j++
	}
	v, err := cfgJSONValue(s.raw[start:j])
	if err != nil {
		return err
	}
	s.i = j
	if path == "" {
		return fmt.Errorf("the root of a JSON config file must be an object")
	}
	s.values[path] = v
	s.nodes[path] = cfgJSONNode{ValueStart: start, ValueEnd: j}
	return nil
}

func (s *cfgJSONScanner) parseObject(path string) error {
	open := s.i
	s.i++
	node := cfgJSONNode{IsObject: true, ValueStart: open}
	members := 0
	for {
		s.skip()
		if s.i >= len(s.raw) {
			return fmt.Errorf("unterminated object")
		}
		if s.raw[s.i] == '}' {
			break
		}
		if members > 0 {
			if s.raw[s.i] != ',' {
				return fmt.Errorf("expected ',' between members of %s", cfgPathLabel(path))
			}
			s.i++
			s.skip()
			if s.i < len(s.raw) && s.raw[s.i] == '}' {
				return fmt.Errorf("trailing comma in %s", cfgPathLabel(path))
			}
		}
		if s.raw[s.i] != '"' {
			return fmt.Errorf("expected an object key in %s", cfgPathLabel(path))
		}
		keyStart := s.i
		key, err := s.parseString()
		if err != nil {
			return err
		}
		s.skip()
		if s.i >= len(s.raw) || s.raw[s.i] != ':' {
			return fmt.Errorf("expected ':' after the key %q", key)
		}
		s.i++
		childPath := key
		if path != "" {
			childPath = path + "." + key
		}
		if _, dup := s.nodes[childPath]; dup {
			return fmt.Errorf("duplicate key %q; a duplicate key cannot be span-edited safely", childPath)
		}
		if node.Indent == "" {
			node.Indent = cfgIndentBefore(s.raw, keyStart)
		}
		if err := s.parseValue(childPath); err != nil {
			return err
		}
		members++
		node.LastValueEnd = s.i
	}
	node.ObjClose = s.i
	node.Members = members
	node.ValueEnd = s.i + 1
	s.i++
	s.nodes[path] = node
	return nil
}

func cfgPathLabel(path string) string {
	if path == "" {
		return "the root object"
	}
	return path
}

func (s *cfgJSONScanner) parseString() (string, error) {
	start := s.i
	j := s.i + 1
	for j < len(s.raw) {
		switch s.raw[j] {
		case '\\':
			j += 2
			continue
		case '"':
			var out string
			if err := json.Unmarshal(s.raw[start:j+1], &out); err != nil {
				return "", err
			}
			s.i = j + 1
			return out, nil
		}
		j++
	}
	return "", fmt.Errorf("unterminated string")
}

func (s *cfgJSONScanner) skipContainer(open, close byte) (int, error) {
	depth := 0
	for s.i < len(s.raw) {
		switch s.raw[s.i] {
		case '"':
			if _, err := s.skipString(); err != nil {
				return 0, err
			}
			continue
		case open:
			depth++
		case close:
			depth--
			s.i++
			if depth == 0 {
				return s.i, nil
			}
			continue
		}
		s.i++
	}
	return 0, fmt.Errorf("unterminated %c", open)
}

func (s *cfgJSONScanner) skipString() (int, error) {
	_, err := s.parseString()
	return s.i, err
}

// cfgJSONValue decodes one JSON literal with numbers kept as literals.
func cfgJSONValue(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// insertJSON adds a member to an existing object (create=true).
func (f *cfgFile) insertJSON(key, literal string) (int, string, error) {
	parent := ""
	if i := strings.LastIndexByte(key, '.'); i >= 0 {
		parent = key[:i]
	}
	leaf := key
	if i := strings.LastIndexByte(key, '.'); i >= 0 {
		leaf = key[i+1:]
	}
	jsNode, ok := f.jsonNodes()[parent]
	if !ok || !jsNode.IsObject {
		if parent == "" {
			return 0, "", fmt.Errorf("%w: %s: the root of a JSON config file must be an object", types.ErrPermanent, f.Path)
		}
		return 0, "", fmt.Errorf("%w: %s has no object %q to add %q to (create cannot invent a nesting level in JSON)", types.ErrPermanent, f.Path, parent, key)
	}
	kv := cfgEncodeJSONString(leaf) + ": " + literal
	if jsNode.Members == 0 {
		at := jsNode.ValueStart + 1
		indent := jsNode.Indent
		if indent == "" {
			indent = cfgIndentBefore(f.Raw, jsNode.ValueStart) + "  "
		}
		closeIndent := cfgIndentBefore(f.Raw, jsNode.ObjClose)
		return at, "\n" + indent + kv + "\n" + closeIndent, nil
	}
	at := jsNode.LastValueEnd
	indent := jsNode.Indent
	if indent == "" {
		indent = "  "
	}
	return at, ",\n" + indent + kv, nil
}

// jsonNodes re-runs the JSON scanner to expose the container metadata that
// cfgJSONNode needs for insertion (the spans map only carries value offsets).
func (f *cfgFile) jsonNodes() map[string]cfgJSONNode {
	s := &cfgJSONScanner{raw: f.Raw, values: map[string]any{}, nodes: map[string]cfgJSONNode{}}
	if err := s.parseValue(""); err != nil {
		return map[string]cfgJSONNode{}
	}
	return s.nodes
}

func cfgEncodeJSONString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// ---- YAML -----------------------------------------------------------------

// parseYAML walks the indentation blocks of a single-document YAML file.
func (f *cfgFile) parseYAML() error {
	type frame struct {
		indent int
		path   string
	}
	frames := []frame{{indent: -1, path: ""}}
	seq := map[string][]any{}
	lines := cfgLines(f.Raw)
	for _, ln := range lines {
		text := ln.Body
		if strings.TrimSpace(text) == "" || strings.HasPrefix(strings.TrimSpace(text), "#") {
			continue
		}
		if strings.TrimSpace(text) == "---" || strings.TrimSpace(text) == "..." {
			continue
		}
		indent := 0
		for indent < len(text) && text[indent] == ' ' {
			indent++
		}
		if indent < len(text) && text[indent] == '\t' {
			return fmt.Errorf("%w: %s:%d uses a tab for indentation, which YAML forbids", types.ErrPermanent, f.Path, 0)
		}
		body := text[indent:]
		for len(frames) > 1 && frames[len(frames)-1].indent >= indent {
			frames = frames[:len(frames)-1]
		}
		top := frames[len(frames)-1]
		if strings.HasPrefix(body, "- ") || body == "-" {
			item := cfgParseYAMLScalar(cfgStripComment(strings.TrimPrefix(body, "-"), "#"))
			if top.path == "" {
				return fmt.Errorf("%w: %s: a top-level sequence is not a config mapping", types.ErrPermanent, f.Path)
			}
			seq[top.path] = append(seq[top.path], item)
			f.Values[top.path] = seq[top.path]
			f.Spans[top.path] = cfgSpan{Editable: false}
			continue
		}
		colon := cfgIndexOutsideString(body, ':')
		if colon < 0 {
			return fmt.Errorf("%w: %s: cannot parse the YAML line %q", types.ErrPermanent, f.Path, strings.TrimSpace(text))
		}
		rawKey := strings.TrimSpace(body[:colon])
		key := strings.Trim(rawKey, `"'`)
		if key == "" {
			return fmt.Errorf("%w: %s: empty YAML key", types.ErrPermanent, f.Path)
		}
		full := key
		if top.path != "" {
			full = top.path + "." + key
		}
		if _, dup := f.Values[full]; dup {
			return fmt.Errorf("%w: %s declares %q twice; a duplicate key cannot be span-edited safely", types.ErrPermanent, f.Path, full)
		}
		rest := body[colon+1:]
		valueText := cfgStripComment(rest, "#")
		if strings.TrimSpace(valueText) == "" {
			// A block mapping: keep the key readable, but it is not span-editable.
			f.Values[full] = map[string]any{}
			f.Spans[full] = cfgSpan{Editable: false}
			frames = append(frames, frame{indent: indent, path: full})
			continue
		}
		// The scalar span starts after "key:" plus its blanks and ends where the
		// scalar ends, so an inline comment stays outside the replaced bytes.
		lead := len(rest) - len(strings.TrimLeft(rest, " \t"))
		leafStart := ln.Start + indent + colon + 1 + lead
		scalar := strings.TrimSpace(valueText)
		leafEnd := leafStart + len(scalar)
		if leafEnd > ln.Start+len(text) {
			leafEnd = ln.Start + len(text)
		}
		f.Values[full] = cfgParseYAMLScalar(scalar)
		f.Spans[full] = cfgSpan{Start: leafStart, End: leafEnd, Editable: true}
	}
	return nil
}

// cfgParseYAMLScalar parses the scalar forms a config file carries.
func cfgParseYAMLScalar(s string) any {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		var out string
		if err := json.Unmarshal([]byte(s), &out); err == nil {
			return out
		}
	}
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'")
	}
	switch strings.ToLower(s) {
	case "true", "yes", "on":
		return true
	case "false", "no", "off":
		return false
	case "null", "~":
		return nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	if fl, err := strconv.ParseFloat(s, 64); err == nil {
		return fl
	}
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		items := cfgSplitTopLevel(s[1:len(s)-1], ',')
		out := make([]any, 0, len(items))
		for _, item := range items {
			if item == "" {
				continue
			}
			out = append(out, cfgParseYAMLScalar(item))
		}
		return out
	}
	return s
}

// insertYAML adds a missing key under the deepest existing block (or at the top
// level), creating any intermediate block that does not exist yet.
func (f *cfgFile) insertYAML(key, literal string) (int, string, error) {
	segments := cfgKeyPath(key)
	if len(segments) == 0 {
		return 0, "", fmt.Errorf("%w: empty config key", types.ErrPermanent)
	}
	type block struct {
		indent int
		body   strings.Builder
		ok     bool
	}
	// Find the deepest existing prefix container.
	best := -1
	bestIndent := -1
	for i := 0; i < len(segments)-1; i++ {
		path := strings.Join(segments[:i+1], ".")
		if _, ok := f.Values[path]; !ok {
			continue
		}
		best = i
		bestIndent = f.yamlBlockIndent(path)
	}
	at := len(f.Raw)
	if best >= 0 {
		path := strings.Join(segments[:best+1], ".")
		end, err := f.yamlBlockEnd(path)
		if err != nil {
			return 0, "", err
		}
		at = end
	} else {
		at = len(f.Raw)
	}
	var b strings.Builder
	b.WriteString(cfgNewline(f.Raw, at))
	indent := bestIndent + 2
	if best < 0 {
		indent = 0
	}
	for _, seg := range segments[best+1 : len(segments)-1] {
		b.WriteString(strings.Repeat(" ", indent))
		b.WriteString(cfgYAMLKey(seg))
		b.WriteString(":\n")
		indent += 2
	}
	b.WriteString(strings.Repeat(" ", indent))
	b.WriteString(cfgYAMLKey(segments[len(segments)-1]))
	b.WriteString(": ")
	b.WriteString(literal)
	b.WriteString("\n")
	return at, b.String(), nil
}

// yamlBlockIndent returns the indentation of the line that declares path.
func (f *cfgFile) yamlBlockIndent(path string) int {
	leaf := path
	if i := strings.LastIndexByte(path, '.'); i >= 0 {
		leaf = path[i+1:]
	}
	for _, ln := range cfgLines(f.Raw) {
		text := ln.Body
		trimmed := strings.TrimSpace(text)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(text) - len(strings.TrimLeft(text, " "))
		body := strings.TrimSpace(text)
		if body == leaf+":" || strings.HasPrefix(body, leaf+":") {
			return indent
		}
	}
	return 0
}

// yamlBlockEnd returns the offset where the block of path ends: the first
// following line that is not more deeply indented, or EOF.
func (f *cfgFile) yamlBlockEnd(path string) (int, error) {
	leaf := path
	if i := strings.LastIndexByte(path, '.'); i >= 0 {
		leaf = path[i+1:]
	}
	lines := cfgLines(f.Raw)
	seen := false
	base := 0
	for _, ln := range lines {
		trimmed := strings.TrimSpace(ln.Body)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(ln.Body) - len(strings.TrimLeft(ln.Body, " "))
		if !seen {
			if strings.TrimSpace(ln.Body) == leaf+":" || strings.HasPrefix(strings.TrimSpace(ln.Body), leaf+":") {
				seen = true
				base = indent
			}
			continue
		}
		if trimmed == "" {
			continue
		}
		if indent <= base {
			return ln.Start, nil
		}
	}
	if !seen {
		return len(f.Raw), fmt.Errorf("%w: %s: no YAML block %q to append to", types.ErrPermanent, f.Path, path)
	}
	return len(f.Raw), nil
}

func cfgYAMLKey(seg string) string {
	if cfgYAMLPlain(seg) {
		return seg
	}
	b, err := json.Marshal(seg)
	if err != nil {
		return seg
	}
	return string(b)
}

// ---- ENV ------------------------------------------------------------------

// parseENV reads `KEY=value` lines (`export KEY=...` is accepted).
func (f *cfgFile) parseENV() error {
	for _, ln := range cfgLines(f.Raw) {
		trimmed := strings.TrimSpace(ln.Body)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		line, off := ln.Body, 0
		if i := strings.Index(ln.Body, "export "); i >= 0 && strings.TrimSpace(ln.Body[:i]) == "" {
			line, off = ln.Body[i+len("export "):], i+len("export ")
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			return fmt.Errorf("%w: %s: cannot parse the env line %q", types.ErrPermanent, f.Path, trimmed)
		}
		key := strings.TrimSpace(line[:eq])
		if key == "" {
			return fmt.Errorf("%w: %s: empty env key", types.ErrPermanent, f.Path)
		}
		if _, dup := f.Values[key]; dup {
			return fmt.Errorf("%w: %s declares %s twice; a duplicate env key cannot be span-edited safely", types.ErrPermanent, f.Path, key)
		}
		valText := strings.TrimRight(line[eq+1:], " \t")
		f.Values[key] = cfgParseENVValue(valText)
		start := ln.Start + off + eq + 1
		f.Spans[key] = cfgSpan{Start: start, End: start + len(valText), Editable: true}
	}
	return nil
}

func cfgParseENVValue(s string) any {
	if len(s) >= 2 {
		if s[0] == '"' && s[len(s)-1] == '"' {
			var out string
			if err := json.Unmarshal([]byte(s), &out); err == nil {
				return out
			}
		}
		if s[0] == '\'' && s[len(s)-1] == '\'' {
			return s[1 : len(s)-1]
		}
	}
	return s
}

func (f *cfgFile) insertENV(key, literal string) (int, string, error) {
	if !utf8.ValidString(key) || !utf8.ValidString(literal) {
		return 0, "", fmt.Errorf("%w: an env assignment must be valid UTF-8", types.ErrPermanent)
	}
	at := len(f.Raw)
	return at, cfgNewline(f.Raw, at) + key + "=" + literal + "\n", nil
}

// ---- INI ------------------------------------------------------------------

// parseINI reads `key = value` / `key: value` lines, tracking [sections]; the
// flattened key of a sectioned key is "<section>.<key>". A duplicate key in the
// same section is permanent: INI has no way to span-edit it unambiguously.
func (f *cfgFile) parseINI() error {
	section := ""
	for _, ln := range cfgLines(f.Raw) {
		trimmed := strings.TrimSpace(ln.Body)
		if trimmed == "" || strings.HasPrefix(trimmed, ";") || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			end := strings.IndexByte(trimmed, ']')
			if end < 0 {
				return fmt.Errorf("%w: %s: unterminated INI section header %q", types.ErrPermanent, f.Path, trimmed)
			}
			section = strings.TrimSpace(trimmed[1:end])
			f.sections[section] = true
			continue
		}
		lead := len(ln.Body) - len(strings.TrimLeft(ln.Body, " \t"))
		inner := ln.Body[lead:]
		sep := strings.IndexAny(inner, "=:")
		if sep < 0 {
			return fmt.Errorf("%w: %s: cannot parse the INI line %q", types.ErrPermanent, f.Path, trimmed)
		}
		key := strings.TrimSpace(inner[:sep])
		if key == "" {
			return fmt.Errorf("%w: %s: empty INI key", types.ErrPermanent, f.Path)
		}
		full := key
		if section != "" {
			full = section + "." + key
		}
		if _, dup := f.Values[full]; dup {
			return fmt.Errorf("%w: %s declares %q twice in the same section; a duplicate INI key cannot be span-edited safely", types.ErrPermanent, f.Path, full)
		}
		valueRel := sep + 1
		for valueRel < len(inner) && (inner[valueRel] == ' ' || inner[valueRel] == '\t') {
			valueRel++
		}
		valText := strings.TrimRight(inner[valueRel:], " \t")
		f.Values[full] = cfgParseINIValue(valText)
		start := ln.Start + lead + valueRel
		f.Spans[full] = cfgSpan{Start: start, End: start + len(valText), Editable: true}
	}
	return nil
}

// cfgParseINIValue strips a matching pair of quotes off an INI value.
func cfgParseINIValue(s string) any {
	if len(s) >= 2 && ((s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'')) {
		return s[1 : len(s)-1]
	}
	return s
}

func (f *cfgFile) insertINI(key, literal string) (int, string, error) {
	parent := ""
	leaf := key
	if i := strings.LastIndexByte(key, '.'); i >= 0 {
		parent, leaf = key[:i], key[i+1:]
	}
	assign := leaf + " = " + literal + "\n"
	if parent == "" {
		at := f.beforeFirstINISection()
		return at, cfgNewline(f.Raw, at) + assign, nil
	}
	if f.sections[parent] {
		if end, ok := f.iniSectionEnd(parent); ok {
			return end, cfgNewline(f.Raw, end) + assign, nil
		}
	}
	at := len(f.Raw)
	return at, cfgNewline(f.Raw, at) + "[" + parent + "]\n" + assign, nil
}

func (f *cfgFile) beforeFirstINISection() int {
	for _, ln := range cfgLines(f.Raw) {
		if strings.HasPrefix(strings.TrimSpace(ln.Body), "[") {
			return ln.Start
		}
	}
	return len(f.Raw)
}

func (f *cfgFile) iniSectionEnd(name string) (int, bool) {
	in := false
	seen := false
	for _, ln := range cfgLines(f.Raw) {
		text := strings.TrimSpace(ln.Body)
		if strings.HasPrefix(text, "[") {
			if in {
				return ln.Start, true
			}
			end := strings.IndexByte(text, ']')
			if end > 0 && strings.TrimSpace(text[1:end]) == name {
				in, seen = true, true
			}
			continue
		}
	}
	if in {
		return len(f.Raw), seen
	}
	return 0, false
}

// lineterm reports the line terminator style of a file.
func lineterm(raw []byte) string {
	if bytes.Contains(raw, []byte("\r\n")) {
		return "\r\n"
	}
	return "\n"
}
