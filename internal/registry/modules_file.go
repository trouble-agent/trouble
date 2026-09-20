package registry

// modules_file.go — file.read, file.patch (SPEC-06 §3.8, §3.9).
//
// file.read is pure and bounded by DAC plus do-not-touch; the UTF-8 encoding
// refuses a non-UTF-8 byte sequence while encoding=raw returns base64 so a
// diagnosis can still read a binary artifact (SPEC-06 §6.12). file.patch applies
// a unified diff all-or-nothing through a temp file plus a rename, or restores a
// backup when restore_from is given; Check dry-runs the hunks in memory and never
// writes, and every hunk's after-side already matching is the idempotency
// detector (Diff{Empty:true}). Path safety (file.allow_roots, symlink
// containment) is enforced by the registry's authorize step a4, not here.
//
// The descriptor, args struct, env binding and target declarations below are
// frozen; the behaviour lives in the Check/Apply/Verify bodies and the file*/
// patch* helpers.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/trouble-agent/trouble/internal/registry/schemagen"
	"github.com/trouble-agent/trouble/internal/types"
)

type fileReadArgs struct {
	Path     string `json:"path"`
	MaxBytes int    `json:"max_bytes,omitempty" js:"min=1;max=1048576;default=262144"`
	Encoding string `json:"encoding,omitempty" js:"enum=utf-8|raw;default=utf-8"`
	Offset   int    `json:"offset,omitempty" js:"min=0;default=0"`
}

type filePatchArgs struct {
	Path        string `json:"path"`
	Patch       string `json:"patch,omitempty" js:"maxlen=1048576;oneof=patch|restore_from"`
	RestoreFrom string `json:"restore_from,omitempty"`
	Strip       int    `json:"strip,omitempty" js:"min=0;max=8;default=0"`
	Backup      bool   `json:"backup,omitempty" js:"default=true"`
}

type fileReadModule struct{ env *moduleEnv }

var fileReadDescriptor = types.Descriptor{
	Name:        "file.read",
	Version:     1,
	Schema:      schemagen.Generate("file.read", 1, fileReadArgs{}),
	Scopes:      []string{"file:read"},
	Idempotency: types.IdemPure,
	CheckMode:   true,
	TimeoutS:    5,
	Mutating:    false,
}

func (m fileReadModule) bind(e *moduleEnv)            { m.env = e }
func (m fileReadModule) Descriptor() types.Descriptor { return fileReadDescriptor }

func (m fileReadModule) NormalizeArgs(args map[string]any) (map[string]any, error) {
	if err := normalizePathArgs(args, "path"); err != nil {
		return nil, err
	}
	return args, nil
}

func (m fileReadModule) ProtectedTargets(args map[string]any) []protectedTarget {
	return pathTarget(args)
}

// Check is a pure read: Diff{Empty:true} with the one-line observation
// (`"<bytes> bytes <encoding> at <path>"`, or the truncation report).
func (m fileReadModule) Check(ctx context.Context, args map[string]any) (types.Diff, error) {
	obs, err := fileReadObserve(ctx, args)
	if err != nil {
		return types.Diff{}, err
	}
	return types.Diff{Empty: true, Summary: obs.summary()}, nil
}

// Apply is a read: Changed=false with the payload in Result.Output
// (utf-8 → content; raw → bytes_base64, bytes, sha256).
func (m fileReadModule) Apply(ctx context.Context, args map[string]any) (types.Result, error) {
	obs, err := fileReadObserve(ctx, args)
	if err != nil {
		return types.Result{}, err
	}
	return types.Result{Changed: false, Output: obs.output()}, nil
}

// Verify = recheck: the window is re-read and must still hash to the observed
// sha256 (a file that moved under the call is a verify failure, SPEC-06 §3.9).
func (m fileReadModule) Verify(ctx context.Context, args map[string]any) (types.VerifyResult, error) {
	obs, err := fileReadObserve(ctx, args)
	if err != nil {
		return types.VerifyResult{}, err
	}
	return types.VerifyResult{
		OK:       true,
		Method:   "recheck",
		Detail:   obs.output(),
		Evidence: []types.DiffEntry{{Path: obs.Path, Before: nil, After: obs.SHA256}},
	}, nil
}

type filePatchModule struct{ env *moduleEnv }

var filePatchDescriptor = types.Descriptor{
	Name:        "file.patch",
	Version:     1,
	Schema:      schemagen.Generate("file.patch", 1, filePatchArgs{}),
	Scopes:      []string{"file:write"},
	Idempotency: types.IdemConvergent,
	CheckMode:   true,
	TimeoutS:    10,
	Mutating:    true,
}

func (m filePatchModule) bind(e *moduleEnv)            { m.env = e }
func (m filePatchModule) Descriptor() types.Descriptor { return filePatchDescriptor }
func (m filePatchModule) RollbackInvertible() bool     { return true }

func (m filePatchModule) NormalizeArgs(args map[string]any) (map[string]any, error) {
	if err := normalizePathArgs(args, "path"); err != nil {
		return nil, err
	}
	return args, nil
}

func (m filePatchModule) ProtectedTargets(args map[string]any) []protectedTarget {
	return pathTarget(args)
}

// cfg returns the resolved configuration the module was bound with, falling back
// to the compiled-in defaults so a module that was never bound still works (the
// pure-read paths must not depend on the daemon's wiring).
func (m filePatchModule) cfg() config {
	if m.env != nil {
		return m.env.cfg
	}
	return defaultConfig()
}

// Check dry-runs the hunks in memory: one entry per hunk
// (`{path:"<file>:<line_no>", before:"<removed lines>", after:"<added lines>"}`),
// or Diff{Empty:true} when every hunk's after-side already matches the file —
// the idempotency detector. Check never writes.
func (m filePatchModule) Check(ctx context.Context, args map[string]any) (types.Diff, error) {
	plan, err := filePatchPlanOf(ctx, m.cfg(), args)
	if err != nil {
		return types.Diff{}, err
	}
	if plan.Settled {
		return types.Diff{Empty: true, Summary: plan.Summary}, nil
	}
	return types.Diff{Empty: false, Entries: plan.Entries, Summary: plan.Summary}, nil
}

// Apply writes the backup copy first and then applies all-or-nothing through a
// temp file plus a rename (SPEC-06 §3.9). A second Apply with identical args is
// a no-op: Changed=false with an empty Applied.
func (m filePatchModule) Apply(ctx context.Context, args map[string]any) (types.Result, error) {
	cfg := m.cfg()
	plan, err := filePatchPlanOf(ctx, cfg, args)
	if err != nil {
		return types.Result{}, err
	}
	out := map[string]any{
		"path":    plan.Path,
		"mode":    plan.Mode,
		"hunks":   len(plan.Hunks),
		"sha256":  plan.NewSHA,
		"bytes":   len(plan.Out),
		"changed": !plan.Settled,
	}
	if plan.Settled {
		return types.Result{Changed: false, Output: out}, nil
	}
	if plan.Backup {
		id, err := filePatchWriteBackup(cfg, plan)
		if err != nil {
			return types.Result{}, fmt.Errorf("%w: %v", types.ErrTransient, err)
		}
		out["backup_id"] = id
		out["rollback"] = map[string]any{"path": plan.Path, "restore_from": id}
	}
	if err := writeFileAtomic(plan.Path, plan.Out, plan.Perm); err != nil {
		return types.Result{}, fmt.Errorf("%w: %v", types.ErrTransient, err)
	}
	return types.Result{
		Changed:  true,
		Applied:  plan.Entries,
		Output:   out,
		Rollback: filePatchRollbackHint(plan, out),
	}, nil
}

// filePatchRollbackHint names the module and args that invert the write
// (SPEC-06 §3.5): `file.patch` with the backup id of the pre-change bytes.
func filePatchRollbackHint(plan filePatchPlan, out map[string]any) *types.RollbackHint {
	id, _ := out["backup_id"].(string)
	if id == "" {
		return &types.RollbackHint{Supported: false}
	}
	return &types.RollbackHint{
		Supported: true,
		Module:    "file.patch",
		Args:      map[string]any{"path": plan.Path, "restore_from": id},
	}
}

// Verify = recheck: the file is re-read, every hunk's after-side must be present
// (restore mode: the content must equal the backup) and the observed sha256 is
// reported so the ledger's Result.Output.sha256 can be compared; a mismatch is
// ok=false → TROUBLE-REGISTRY-005 (SPEC-06 §3.9).
func (m filePatchModule) Verify(ctx context.Context, args map[string]any) (types.VerifyResult, error) {
	cfg := m.cfg()
	var a filePatchArgs
	if err := decodeArgs(args, &a); err != nil {
		return types.VerifyResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return types.VerifyResult{}, fmt.Errorf("%w: %v", types.ErrTransient, err)
	}
	raw, err := filePatchRead(a.Path)
	if err != nil {
		return types.VerifyResult{}, err
	}
	detail := map[string]any{"path": a.Path, "sha256": fileSHA256(raw), "bytes": len(raw)}
	if a.RestoreFrom != "" {
		backup, err := filePatchReadBackup(cfg, a.Path, a.RestoreFrom)
		if err != nil {
			return types.VerifyResult{}, err
		}
		detail["mode"] = "restore"
		detail["restore_from"] = a.RestoreFrom
		detail["backup_sha256"] = fileSHA256(backup)
		if !bytes.Equal(raw, backup) {
			detail["ok_reason"] = "the file does not match the restored backup"
			return types.VerifyResult{OK: false, Method: "recheck", Detail: detail}, nil
		}
		return types.VerifyResult{
			OK:     true,
			Method: "recheck",
			Detail: detail,
			Evidence: []types.DiffEntry{{
				Path: a.Path, Before: fileSHA256(backup), After: fileSHA256(raw),
			}},
		}, nil
	}
	hunks, err := filePatchParse(a.Patch)
	if err != nil {
		return types.VerifyResult{}, err
	}
	fileLines := filePatchSplitLines(raw)
	detail["mode"] = "patch"
	detail["hunks"] = len(hunks)
	entries := make([]types.DiffEntry, 0, len(hunks))
	present := 0
	for _, h := range hunks {
		entry := filePatchHunkEntry(a.Path, h)
		entries = append(entries, entry)
		if _, ok := filePatchLocate(fileLines, filePatchNewSide(h), h.NewStart-1); ok {
			present++
		}
	}
	detail["hunks_applied"] = present
	if present != len(hunks) {
		detail["ok_reason"] = fmt.Sprintf("%d of %d hunks are present", present, len(hunks))
		return types.VerifyResult{OK: false, Method: "recheck", Detail: detail}, nil
	}
	return types.VerifyResult{OK: true, Method: "recheck", Detail: detail, Evidence: entries}, nil
}

// ---------------------------------------------------------------------------
// file.read
// ---------------------------------------------------------------------------

// fileReadObservation is one file.read: the returned window plus the file
// identity. sha256 is the hash of the returned window (the bytes the caller
// sees), bytes is the window length, total_bytes the file size.
type fileReadObservation struct {
	Path      string
	Encoding  string
	Offset    int
	MaxBytes  int
	Window    []byte
	Total     int64
	Truncated bool
	SHA256    string
}

func fileReadObserve(ctx context.Context, args map[string]any) (fileReadObservation, error) {
	if err := ctx.Err(); err != nil {
		return fileReadObservation{}, fmt.Errorf("%w: %v", types.ErrTransient, err)
	}
	var a fileReadArgs
	if err := decodeArgs(args, &a); err != nil {
		return fileReadObservation{}, err
	}
	if a.MaxBytes == 0 {
		a.MaxBytes = 262144
	}
	if a.Encoding == "" {
		a.Encoding = "utf-8"
	}
	if a.Encoding != "utf-8" && a.Encoding != "raw" {
		return fileReadObservation{}, fmt.Errorf("%w: unknown encoding %q (utf-8|raw)", types.ErrPermanent, a.Encoding)
	}
	if a.Offset < 0 {
		return fileReadObservation{}, fmt.Errorf("%w: offset must not be negative", types.ErrPermanent)
	}
	fi, err := os.Stat(a.Path)
	if err != nil {
		return fileReadObservation{}, fmt.Errorf("%w: cannot stat %s: %v", types.ErrPermanent, a.Path, err)
	}
	if fi.IsDir() {
		return fileReadObservation{}, fmt.Errorf("%w: %s is a directory", types.ErrPermanent, a.Path)
	}
	obs := fileReadObservation{Path: a.Path, Encoding: a.Encoding, Offset: a.Offset, MaxBytes: a.MaxBytes, Total: fi.Size()}

	f, err := os.Open(a.Path)
	if err != nil {
		return fileReadObservation{}, fmt.Errorf("%w: cannot open %s: %v", types.ErrPermanent, a.Path, err)
	}
	defer f.Close()
	if a.Offset > 0 {
		if _, err := f.Seek(int64(a.Offset), 0); err != nil {
			return fileReadObservation{}, fmt.Errorf("%w: cannot seek %s: %v", types.ErrPermanent, a.Path, err)
		}
	}
	buf := make([]byte, a.MaxBytes+1)
	n, err := io.ReadFull(f, buf)
	switch {
	case err == io.EOF, err == io.ErrUnexpectedEOF:
		// A short read is the normal end of the file, not a failure.
	case err != nil:
		return fileReadObservation{}, fmt.Errorf("%w: cannot read %s: %v", types.ErrPermanent, a.Path, err)
	}
	obs.Truncated = n > a.MaxBytes
	if obs.Truncated {
		n = a.MaxBytes
	}
	obs.Window = buf[:n]
	obs.SHA256 = fileSHA256(obs.Window)
	if obs.Encoding == "utf-8" && !utf8.Valid(obs.Window) {
		// A truncated window may end mid-rune: only that boundary is forgiven.
		if !obs.Truncated || !utf8.Valid(obs.Window[:fileReadRuneEnd(obs.Window)]) {
			return fileReadObservation{}, fmt.Errorf("%w: %s is not valid UTF-8 (use encoding=raw to read a binary artifact, SPEC-06 §6.12)", types.ErrPermanent, a.Path)
		}
	}
	return obs, nil
}

// fileReadRuneEnd returns the offset of the last complete rune boundary.
func fileReadRuneEnd(b []byte) int {
	for i := len(b); i > 0; i-- {
		if utf8.RuneStart(b[i-1]) {
			return i
		}
	}
	return 0
}

func (o fileReadObservation) summary() string {
	if o.Truncated {
		return fmt.Sprintf("truncated: %d of %d bytes (%s) at %s", len(o.Window), o.Total, o.Encoding, o.Path)
	}
	return fmt.Sprintf("%d bytes (%s) at %s", len(o.Window), o.Encoding, o.Path)
}

func (o fileReadObservation) output() map[string]any {
	out := map[string]any{
		"path":        o.Path,
		"encoding":    o.Encoding,
		"offset":      o.Offset,
		"max_bytes":   o.MaxBytes,
		"bytes":       len(o.Window),
		"total_bytes": o.Total,
		"truncated":   o.Truncated,
		"sha256":      o.SHA256,
	}
	if o.Encoding == "raw" {
		out["bytes_base64"] = base64.StdEncoding.EncodeToString(o.Window)
		return out
	}
	out["content"] = string(o.Window)
	return out
}

// ---------------------------------------------------------------------------
// file.patch
// ---------------------------------------------------------------------------

// filePatchLine is one line of a unified-diff hunk.
type filePatchLine struct {
	Kind      byte // ' ' context, '-' deletion, '+' addition
	Text      string
	NoNewline bool // the "\ No newline at end of file" marker follows it
}

// filePatchHunk is one `@@ -old,count +new,count @@` block.
type filePatchHunk struct {
	OldStart, OldCount int
	NewStart, NewCount int
	Lines              []filePatchLine
}

// filePatchPlan is what file.patch Check predicts and Apply performs.
type filePatchPlan struct {
	Path    string
	Mode    string // patch | restore
	Hunks   []filePatchHunk
	Raw     []byte
	Out     []byte
	Entries []types.DiffEntry
	Settled bool
	Backup  bool
	Perm    os.FileMode
	NewSHA  string
	Summary string
}

// filePatchPlanOf reads the target and builds the plan: patch mode dry-runs the
// hunks, restore mode compares the file against the named backup. Nothing is
// written here.
func filePatchPlanOf(ctx context.Context, cfg config, args map[string]any) (filePatchPlan, error) {
	if err := ctx.Err(); err != nil {
		return filePatchPlan{}, fmt.Errorf("%w: %v", types.ErrTransient, err)
	}
	var a filePatchArgs
	if err := decodeArgs(args, &a); err != nil {
		return filePatchPlan{}, err
	}
	plan := filePatchPlan{Path: a.Path, Backup: true}
	if _, ok := args["backup"]; ok {
		plan.Backup = a.Backup
	}
	switch {
	case a.Patch == "" && a.RestoreFrom == "":
		return plan, fmt.Errorf("%w: file.patch needs either patch or restore_from", types.ErrPermanent)
	case a.Patch != "" && a.RestoreFrom != "":
		return plan, fmt.Errorf("%w: file.patch takes patch or restore_from, never both", types.ErrPermanent)
	}
	raw, err := filePatchRead(a.Path)
	if err != nil {
		return plan, err
	}
	plan.Raw = raw
	if fi, err := os.Stat(a.Path); err == nil {
		plan.Perm = fi.Mode().Perm()
	}

	if a.RestoreFrom != "" {
		backup, err := filePatchReadBackup(cfg, a.Path, a.RestoreFrom)
		if err != nil {
			return plan, err
		}
		plan.Mode = "restore"
		plan.Out = backup
		plan.NewSHA = fileSHA256(backup)
		plan.Settled = bytes.Equal(raw, backup)
		plan.Entries = []types.DiffEntry{{
			Path:   a.Path,
			Before: fileSHA256(raw),
			After:  fileSHA256(backup),
		}}
		plan.Summary = fmt.Sprintf("restore %s from %s (%d bytes)", a.Path, a.RestoreFrom, len(backup))
		if plan.Settled {
			plan.Summary = fmt.Sprintf("%s already matches %s", a.Path, a.RestoreFrom)
			plan.Entries = nil
		}
		return plan, nil
	}

	hunks, err := filePatchParse(a.Patch)
	if err != nil {
		return plan, err
	}
	plan.Mode = "patch"
	plan.Hunks = hunks
	fileLines := filePatchSplitLines(raw)
	before, applied := 0, 0
	for _, h := range hunks {
		plan.Entries = append(plan.Entries, filePatchHunkEntry(a.Path, h))
		_, onBefore := filePatchLocate(fileLines, filePatchOldSide(h), h.OldStart-1)
		_, onAfter := filePatchLocate(fileLines, filePatchNewSide(h), h.NewStart-1)
		switch {
		case onBefore:
			before++
		case onAfter:
			applied++
		default:
			return plan, fmt.Errorf("%w: %s hunk @@ -%d,%d +%d,%d @@ matches neither the before nor the after side of the file; refusing to guess",
				types.ErrPermanent, a.Path, h.OldStart, h.OldCount, h.NewStart, h.NewCount)
		}
	}
	if before == 0 && applied == len(hunks) {
		// Every hunk is already applied: the convergent no-op.
		plan.Settled = true
		plan.Out = raw
		plan.NewSHA = fileSHA256(raw)
		plan.Entries = nil
		plan.Summary = fmt.Sprintf("%s already carries all %d hunk(s)", a.Path, len(hunks))
		return plan, nil
	}
	if applied > 0 {
		return plan, fmt.Errorf("%w: %s is partially patched (%d hunk(s) already applied, %d not); a partial state is permanent, never a guess",
			types.ErrPermanent, a.Path, applied, before)
	}
	out, err := filePatchBuild(plan, fileLines)
	if err != nil {
		return plan, err
	}
	// Guard: the produced content must carry every hunk's after-side, otherwise
	// nothing is written (never a guess).
	outLines := filePatchSplitLines(out)
	for _, h := range plan.Hunks {
		if _, ok := filePatchLocate(outLines, filePatchNewSide(h), h.NewStart-1); !ok {
			return plan, fmt.Errorf("%w: the patched content of %s would not carry hunk @@ -%d,%d +%d,%d @@; refusing to write",
				types.ErrPermanent, a.Path, h.OldStart, h.OldCount, h.NewStart, h.NewCount)
		}
	}
	plan.Out = out
	plan.NewSHA = fileSHA256(out)
	plan.Summary = fmt.Sprintf("%d hunk(s), %s -> %s", len(hunks), fileShortHash(fileSHA256(raw)), fileShortHash(plan.NewSHA))
	return plan, nil
}

// filePatchRead reads a patch target; file.patch never creates a file.
func filePatchRead(path string) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot stat %s: %v", types.ErrPermanent, path, err)
	}
	if fi.IsDir() {
		return nil, fmt.Errorf("%w: %s is a directory", types.ErrPermanent, path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot read %s: %v", types.ErrPermanent, path, err)
	}
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("%w: %s is not valid UTF-8; a binary target is never patched (SPEC-06 §6.12)", types.ErrPermanent, path)
	}
	return raw, nil
}

// filePatchLineInfo is one line of the target file with its byte span.
type filePatchLineInfo struct {
	Text       string
	Term       string
	Start, End int
}

// filePatchSplitLines splits the target into lines, keeping each terminator so
// untouched regions can be copied byte-for-byte.
func filePatchSplitLines(raw []byte) []filePatchLineInfo {
	var out []filePatchLineInfo
	for i := 0; i < len(raw); {
		nl := bytes.IndexByte(raw[i:], '\n')
		if nl < 0 {
			out = append(out, filePatchLineInfo{Text: string(raw[i:]), Start: i, End: len(raw)})
			break
		}
		end := i + nl + 1
		text := raw[i : end-1]
		term := "\n"
		if len(text) > 0 && text[len(text)-1] == '\r' {
			text = text[:len(text)-1]
			term = "\r\n"
		}
		out = append(out, filePatchLineInfo{Text: string(text), Term: term, Start: i, End: end})
		i = end
	}
	return out
}

// filePatchParse parses a unified diff into hunks. A malformed patch or a hunk
// whose declared line counts disagree with its body is permanent.
func filePatchParse(text string) ([]filePatchHunk, error) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var hunks []filePatchHunk
	i := 0
	for i < len(lines) {
		line := lines[i]
		if !strings.HasPrefix(line, "@@") {
			i++
			continue
		}
		h, err := filePatchHeader(line)
		if err != nil {
			return nil, err
		}
		i++
		for i < len(lines) {
			body := lines[i]
			if strings.HasPrefix(body, "@@") || strings.HasPrefix(body, "diff ") ||
				strings.HasPrefix(body, "--- ") || strings.HasPrefix(body, "+++ ") ||
				strings.HasPrefix(body, "Index: ") {
				break
			}
			if body == "" {
				if i == len(lines)-1 {
					i++
					break
				}
				return nil, fmt.Errorf("%w: the patch has an empty line inside a hunk", types.ErrPermanent)
			}
			switch body[0] {
			case ' ', '-', '+':
				h.Lines = append(h.Lines, filePatchLine{Kind: body[0], Text: body[1:]})
			case '\\':
				if len(h.Lines) == 0 {
					return nil, fmt.Errorf("%w: the patch marks \"\\ No newline\" without a preceding line", types.ErrPermanent)
				}
				h.Lines[len(h.Lines)-1].NoNewline = true
			default:
				return nil, fmt.Errorf("%w: the patch line %q is not a unified-diff line", types.ErrPermanent, body)
			}
			i++
		}
		old, new := 0, 0
		for _, l := range h.Lines {
			switch l.Kind {
			case ' ':
				old++
				new++
			case '-':
				old++
			case '+':
				new++
			}
		}
		if old != h.OldCount || new != h.NewCount {
			return nil, fmt.Errorf("%w: hunk @@ -%d,%d +%d,%d @@ carries %d old/%d new line(s)",
				types.ErrPermanent, h.OldStart, h.OldCount, h.NewStart, h.NewCount, old, new)
		}
		hunks = append(hunks, h)
	}
	if len(hunks) == 0 {
		return nil, fmt.Errorf("%w: the patch carries no @@ hunk", types.ErrPermanent)
	}
	return hunks, nil
}

var filePatchHeaderRe = regexp.MustCompile(`^@@+ -([0-9]+)(?:,([0-9]+))? \+([0-9]+)(?:,([0-9]+))? @@`)

func filePatchHeader(line string) (filePatchHunk, error) {
	m := filePatchHeaderRe.FindStringSubmatch(line)
	if m == nil {
		return filePatchHunk{}, fmt.Errorf("%w: cannot parse the hunk header %q", types.ErrPermanent, line)
	}
	num := func(s string, def int) int {
		if s == "" {
			return def
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return def
		}
		return n
	}
	return filePatchHunk{
		OldStart: num(m[1], 0),
		OldCount: num(m[2], 1),
		NewStart: num(m[3], 0),
		NewCount: num(m[4], 1),
	}, nil
}

// filePatchOldSide is the sequence of file lines a hunk replaces.
func filePatchOldSide(h filePatchHunk) []string {
	var out []string
	for _, l := range h.Lines {
		if l.Kind == ' ' || l.Kind == '-' {
			out = append(out, l.Text)
		}
	}
	return out
}

// filePatchNewSide is the sequence of file lines a hunk produces.
func filePatchNewSide(h filePatchHunk) []string {
	var out []string
	for _, l := range h.Lines {
		if l.Kind == ' ' || l.Kind == '+' {
			out = append(out, l.Text)
		}
	}
	return out
}

// filePatchHunkEntry is one hunk's diff entry: its anchor line and the removed
// and added lines (the hunk's change, not its context).
func filePatchHunkEntry(path string, h filePatchHunk) types.DiffEntry {
	var removed, added []string
	for _, l := range h.Lines {
		switch l.Kind {
		case '-':
			removed = append(removed, l.Text)
		case '+':
			added = append(added, l.Text)
		}
	}
	return types.DiffEntry{
		Path:   fmt.Sprintf("%s:%d", path, h.OldStart),
		Before: strings.Join(removed, "\n"),
		After:  strings.Join(added, "\n"),
	}
}

// filePatchLocate finds the position of a line block: the hunk's declared line
// number wins when it matches, otherwise a search is allowed only when it is
// unambiguous — an ambiguous match is refused rather than guessed.
func filePatchLocate(fileLines []filePatchLineInfo, want []string, hint int) (int, bool) {
	if len(want) == 0 {
		return 0, false
	}
	if hint >= 0 && filePatchMatchesAt(fileLines, want, hint) {
		return hint, true
	}
	hits := 0
	first := -1
	for i := 0; i+len(want) <= len(fileLines); i++ {
		if !filePatchMatchesAt(fileLines, want, i) {
			continue
		}
		hits++
		if first < 0 {
			first = i
		}
	}
	if hits == 1 {
		return first, true
	}
	return 0, false
}

// filePatchMatchesAt reports whether want matches the file at line index i.
func filePatchMatchesAt(fileLines []filePatchLineInfo, want []string, i int) bool {
	if i < 0 || i+len(want) > len(fileLines) {
		return false
	}
	for j, w := range want {
		if fileLines[i+j].Text != w {
			return false
		}
	}
	return true
}

// filePatchBuild assembles the new bytes: everything outside a hunk is copied
// as-is, so comments, formatting and untouched lines survive exactly.
func filePatchBuild(plan filePatchPlan, fileLines []filePatchLineInfo) ([]byte, error) {
	type span struct {
		start int // file line index
		end   int // file line index (exclusive)
		hunk  filePatchHunk
	}
	spans := make([]span, 0, len(plan.Hunks))
	prevLine := 0
	for _, h := range plan.Hunks {
		idx, ok := filePatchLocate(fileLines, filePatchOldSide(h), h.OldStart-1)
		if !ok {
			return nil, fmt.Errorf("%w: %s hunk @@ -%d,%d +%d,%d @@ no longer matches the file", types.ErrPermanent, plan.Path, h.OldStart, h.OldCount, h.NewStart, h.NewCount)
		}
		n := len(filePatchOldSide(h))
		if idx < prevLine {
			return nil, fmt.Errorf("%w: %s hunks overlap or are out of order", types.ErrPermanent, plan.Path)
		}
		spans = append(spans, span{start: idx, end: idx + n, hunk: h})
		prevLine = idx + n
	}
	term := lineterm(plan.Raw)
	var out bytes.Buffer
	pos := 0
	for _, sp := range spans {
		startByte := fileLines[sp.start].Start
		endByte := startByte
		if sp.end > sp.start {
			endByte = fileLines[sp.end-1].End
		}
		if startByte < pos {
			return nil, fmt.Errorf("%w: %s hunks overlap", types.ErrPermanent, plan.Path)
		}
		out.Write(plan.Raw[pos:startByte])
		fileIdx := sp.start
		for _, l := range sp.hunk.Lines {
			switch l.Kind {
			case ' ':
				// A context line keeps its original bytes, terminators included.
				if fileIdx < len(fileLines) {
					out.Write(plan.Raw[fileLines[fileIdx].Start:fileLines[fileIdx].End])
				} else {
					out.WriteString(l.Text + term)
				}
				fileIdx++
			case '-':
				fileIdx++
			case '+':
				out.WriteString(l.Text)
				if !l.NoNewline {
					out.WriteString(term)
				}
			}
		}
		pos = endByte
	}
	out.Write(plan.Raw[pos:])
	return out.Bytes(), nil
}

// ---------------------------------------------------------------------------
// file.patch backups
// ---------------------------------------------------------------------------

var filePatchBackupIDRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// filePatchWriteBackup copies the pre-change bytes into file.backup_dir as
// "<backup_id>-<basename>" and prunes older backups for the same path.
func filePatchWriteBackup(cfg config, plan filePatchPlan) (string, error) {
	dir := cfg.FileBackupDir
	if dir == "" {
		return "", fmt.Errorf("file.backup_dir is not configured")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("cannot create the backup dir %s: %w", dir, err)
	}
	id := filePatchNewBackupID(plan.Raw)
	target := filepath.Join(dir, id+"-"+filepath.Base(plan.Path))
	if err := writeFileAtomic(target, plan.Raw, 0o600); err != nil {
		return "", fmt.Errorf("cannot write the backup %s: %w", target, err)
	}
	filePatchPruneBackups(cfg, filepath.Base(plan.Path), id)
	return id, nil
}

// filePatchNewBackupID mints a sortable, collision-free backup id.
func filePatchNewBackupID(raw []byte) string {
	var rnd [4]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		rnd = [4]byte{}
	}
	sum := sha256.Sum256(raw)
	stamp := strings.ReplaceAll(types.NowUTC(), ":", "")
	return "bk_" + stamp + "_" + hex.EncodeToString(sum[:4]) + hex.EncodeToString(rnd[:2])
}

// filePatchPruneBackups keeps at most file.keep_backups files per target path; a
// non-positive limit disables pruning rather than deleting the backup the
// rollback hint points at.
func filePatchPruneBackups(cfg config, base, keepID string) {
	if cfg.FileKeepBackups <= 0 {
		return
	}
	entries, err := os.ReadDir(cfg.FileBackupDir)
	if err != nil {
		return
	}
	suffix := "-" + base
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, suffix) {
			continue
		}
		ids = append(ids, name)
	}
	sort.Strings(ids)
	if len(ids) <= cfg.FileKeepBackups {
		return
	}
	drop := len(ids) - cfg.FileKeepBackups
	for _, name := range ids {
		if drop == 0 {
			break
		}
		if name == keepID+suffix {
			continue
		}
		_ = os.Remove(filepath.Join(cfg.FileBackupDir, name))
		drop--
	}
}

// filePatchReadBackup resolves one backup id for a path and reads it. The id is
// validated so a backup id can never name a file outside file.backup_dir.
func filePatchReadBackup(cfg config, path, id string) ([]byte, error) {
	if !filePatchBackupIDRe.MatchString(id) {
		return nil, fmt.Errorf("%w: %q is not a valid backup id", types.ErrPermanent, id)
	}
	if cfg.FileBackupDir == "" {
		return nil, fmt.Errorf("%w: file.backup_dir is not configured", types.ErrPermanent)
	}
	file := filepath.Join(cfg.FileBackupDir, id+"-"+filepath.Base(path))
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("%w: %s has no backup %q: %v", types.ErrPermanent, path, id, err)
	}
	return raw, nil
}

// ---------------------------------------------------------------------------
// shared IO
// ---------------------------------------------------------------------------

// fileSHA256 is the hex sha256 of a byte slice (the hash the spec shares between
// Result.Output and Verify).
func fileSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func fileShortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// writeFileAtomic writes data through a temp file in the same directory, fsyncs
// it and renames it over the target (SPEC-06 §3.9). mode 0 preserves the target's
// existing permissions (0644 for a new file).
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if mode == 0 {
		if fi, err := os.Stat(path); err == nil {
			mode = fi.Mode().Perm()
		} else {
			mode = 0o644
		}
	}
	tmp, err := os.CreateTemp(dir, ".trouble-*.tmp")
	if err != nil {
		return fmt.Errorf("cannot create a temp file in %s: %w", dir, err)
	}
	name := tmp.Name()
	cleanup := func() {
		if name != "" {
			_ = os.Remove(name)
		}
	}
	defer cleanup()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("cannot write %s: %w", name, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("cannot chmod %s: %w", name, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("cannot fsync %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("cannot close %s: %w", name, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("cannot rename %s to %s: %w", name, path, err)
	}
	name = ""
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
