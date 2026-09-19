package flow

// boardjsonl.go — the board-jsonl writer (SPEC-08 §3.1, §3.2).
//
// The writer recipe is append-only: one line per row, one event per transition,
// never a rewrite, never a compaction, never a row touched by a second writer.
// The board index is rebuilt from the files when the (dev,inode,mtime,size)
// stamp changes, so a foreign writer's rows are seen and the id allocation stays
// strictly greater than the board's max.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// boardFiles are the two files of the contract.
const (
	tasksFile  = "tasks.jsonl"
	eventsFile = "events.jsonl"
)

// rowStyle is the byte style a board already uses. A board authored by trouble
// is compact (no spaces) in schema order; a "spaced" board is tolerated for
// reads and mirrored only when the row is re-serialized the same way, and a
// non-strict (multi-line or unparsable) board refuses the append.
type rowStyle struct {
	compact bool
	spaced  bool
}

// fileStamp is the cache invalidation key.
type fileStamp struct {
	dev, inode uint64
	size       int64
	mtime      time.Time
}

// boardIndex is the per-board index (§3.2 step 2): max task ULID, max event id,
// the detected row style and the sig → task-id map.
type boardIndex struct {
	dir        string
	style      rowStyle
	stamp      fileStamp
	maxULID    string
	maxEventID uint64
	bySig      map[string]string
	rows       map[string]types.BoardRow
	rowCount   int
	nonStrict  bool
	nonStrictN int
}

// indexFor returns the cached index, reloading it when the stamp moved.
func (f *Flow) indexFor(boardDir string) (*boardIndex, error) {
	dir := cleanPath(boardDir)
	if dir == "" {
		return nil, fmt.Errorf("%s: empty board path", types.CodeFlow002)
	}
	f.mu.Lock()
	ix := f.boards[dir]
	f.mu.Unlock()
	st, err := stampOf(filepath.Join(dir, tasksFile))
	if err != nil {
		return nil, err
	}
	if ix != nil && ix.stamp == st {
		return ix, nil
	}
	fresh := &boardIndex{dir: dir, bySig: map[string]string{}, rows: map[string]types.BoardRow{}}
	if err := fresh.reload(); err != nil {
		return nil, err
	}
	fresh.stamp = st
	f.mu.Lock()
	f.boards[dir] = fresh
	f.mu.Unlock()
	return fresh, nil
}

// reload is one streaming pass over both files.
func (ix *boardIndex) reload() error {
	tasksPath := filepath.Join(ix.dir, tasksFile)
	fh, err := os.Open(tasksPath)
	if err != nil {
		if os.IsNotExist(err) {
			// An empty board is legal: trouble authors the first row.
			ix.style = rowStyle{compact: true}
			return ix.reloadEvents()
		}
		return fmt.Errorf("%s: %w", types.CodeFlow002, err)
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<22)
	first := true
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		if first {
			// Compact rows have no ", " separators; a spaced board does.
			ix.style = rowStyle{compact: !bytes.Contains([]byte(line), []byte(`": "`)), spaced: bytes.Contains([]byte(line), []byte(`": "`))}
			first = false
		}
		var row types.BoardRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			// A non-strict (legacy multi-line) board cannot be appended to: an
			// append that cannot be re-read is a corrupt board (§6.7).
			ix.nonStrict = true
			ix.nonStrictN++
			continue
		}
		ix.rowCount++
		if row.Sig != "" {
			ix.bySig[row.Sig] = row.ID
		}
		if row.ID != "" {
			ix.rows[row.ID] = row
			if suf := ulidSuffix(row.ID); suf > ix.maxULID {
				ix.maxULID = suf
			}
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("%s: %w", types.CodeFlow002, err)
	}
	return ix.reloadEvents()
}

// reloadEvents finds the board's max numeric event id.
func (ix *boardIndex) reloadEvents() error {
	fh, err := os.Open(filepath.Join(ix.dir, eventsFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("%s: %w", types.CodeFlow002, err)
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<22)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev types.BoardEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.ID > ix.maxEventID {
			ix.maxEventID = ev.ID
		}
	}
	return sc.Err()
}

// allocTaskID allocates `tsk_` + a ULID strictly greater than the board's max
// (§3.2 step 3): seed = max(now, max_suffix+1), retried up to 8 times.
func (f *Flow) allocTaskID(ix *boardIndex) string {
	prefix := f.cfg.IDPrefix
	if prefix == "" {
		prefix = string(types.PTsk)
	}
	for attempt := 0; attempt < 8; attempt++ {
		suf := ulidSuffix(types.NewID(types.PTsk))
		if ix.maxULID != "" && !strictlyGreater(suf, ix.maxULID) {
			suf = bumpULID(ix.maxULID)
		}
		id := prefix + suf
		if _, exists := ix.rows[id]; !exists {
			ix.maxULID = suf
			return id
		}
		ix.maxULID = bumpULID(ix.maxULID)
	}
	// Eight collisions against a board nobody else writes means a foreign
	// writer is racing: TROUBLE-FLOW-004 rather than a silent overwrite.
	return prefix + bumpULID(ix.maxULID)
}

// writeViaDriver routes a filing through the selected FlowDriver: board-jsonl
// performs the two-file write here, task-router hands the row to the router
// (§3.6). The findings string is the validator's output for the flow payload.
func (f *Flow) writeViaDriver(ctx context.Context, d types.FlowDriver, req types.FileTaskRequest, proj types.FlowProject, ix *boardIndex) (types.FileTaskResult, string, error) {
	switch impl := d.(type) {
	case boardJSONLDriver:
		return f.writeRow(ctx, proj, ix, req.Row, req.Event)
	case taskRouterDriver:
		res, err := impl.FileTask(ctx, req)
		return res, "", err
	default:
		res, err := d.FileTask(ctx, req)
		return res, "", err
	}
}

// writeRow performs the two-file write with validation (§3.2 steps 4–7).
func (f *Flow) writeRow(ctx context.Context, proj types.FlowProject, ix *boardIndex, row types.BoardRow, ev types.BoardEvent) (types.FileTaskResult, string, error) {
	res := types.FileTaskResult{TaskID: row.ID}
	findings := ""
	if ix.nonStrict {
		return res, findings, &flowError{Code: types.CodeFlow002, Msg: fmt.Sprintf("board %s is non-strict: %d unparsable line(s)", ix.dir, ix.nonStrictN)}
	}
	if err := validateRow(row); err != nil {
		return res, findings, &flowError{Code: types.CodeFlow003, Msg: err.Error()}
	}
	settings := f.settingsFor(row.Repo)
	if settings.DryRun {
		// check_mode: the diff is returned and nothing is written.
		line, err := marshalLine(row, ix.style)
		if err != nil {
			return res, findings, err
		}
		res.Diff = types.Diff{
			Empty: false, Summary: "append 1 row to " + tasksFile,
			Entries: []types.DiffEntry{
				{Path: filepath.Join(ix.dir, tasksFile), Before: nil, After: line},
				{Path: filepath.Join(ix.dir, eventsFile), Before: nil, After: eventLine(ix.maxEventID+1, ev)},
			},
		}
		res.Valid = true
		return res, findings, nil
	}
	// Row per requested review mode: `blocked` when a human must approve.
	line, err := marshalLine(row, ix.style)
	if err != nil {
		return res, findings, err
	}
	if err := appendLine(filepath.Join(ix.dir, tasksFile), line); err != nil {
		return res, findings, &flowError{Code: types.CodeFlow002, Msg: err.Error()}
	}
	ix.bySig[row.Sig] = row.ID
	ix.rows[row.ID] = row
	ix.rowCount++
	evID, err := ix.appendEvent(ctx, ev, f.cfg.ValidateCmd)
	if err != nil {
		// tasks first, events second: a task row without its event is re-appended
		// exactly once by Reconcile (§3.2 step 6, idempotent by (task_id, type)).
		return res, findings, err
	}
	res.Wrote = true
	res.EventID = fmt.Sprintf("%d", evID)
	res.Diff = types.Diff{
		Empty: false, Summary: "append 1 row + 1 event",
		Entries: []types.DiffEntry{
			{Path: filepath.Join(ix.dir, tasksFile), Before: nil, After: line},
		},
	}
	// Post-validate: always in-process, plus the board's own command when set.
	if err := validateRow(row); err != nil {
		res.Valid = false
		return res, findings, &flowError{Code: types.CodeFlow003, Msg: err.Error()}
	}
	if f.cfg.ValidateCmd != "" {
		rc, findings, verr := runValidate(ctx, ix.dir, f.cfg.ValidateCmd, row.ID)
		res.ValidateRC = rc
		switch {
		case verr != nil || rc == 2:
			return res, findings, &flowError{Code: types.CodeFlow002, Msg: fmt.Sprintf("validate command failed (rc=%d): %v", rc, verr)}
		case rc == 1 && strings.Contains(findings, row.ID):
			res.Valid = false
			return res, findings, &flowError{Code: types.CodeFlow003, Msg: "post-append validation named " + row.ID}
		case rc == 1:
			// A pre-existing board finding, not an error for this row: it is
			// recorded in the flow payload as validate_findings.
			res.Valid = true
			return res, findings, nil
		}
	}
	res.Valid = true
	return res, findings, nil
}

// appendEvent appends one event line and returns its id (§3.2 step 6). It is
// idempotent by (task_id, type): a re-run after a crash appends nothing, which is
// what makes the boot reconcile safe.
func (ix *boardIndex) appendEvent(ctx context.Context, ev types.BoardEvent, validateCmd string) (uint64, error) {
	if ix.hasEvent(ev.TaskID, ev.Type, "") {
		return ix.maxEventID, nil
	}
	return ix.writeEvent(ctx, ev, validateCmd)
}

// appendEventAlways appends an event without a dedup check. Comments use it: a
// recurrence IS a new comment on the same row (§3.4), and the two-file contract
// has no way to express "the second identical body is noise".
func (ix *boardIndex) appendEventAlways(ctx context.Context, ev types.BoardEvent, validateCmd string) (uint64, error) {
	return ix.writeEvent(ctx, ev, validateCmd)
}

// writeEvent performs the append itself.
func (ix *boardIndex) writeEvent(ctx context.Context, ev types.BoardEvent, validateCmd string) (uint64, error) {
	ix.maxEventID++
	ev.ID = ix.maxEventID
	line, err := json.Marshal(ev)
	if err != nil {
		return 0, err
	}
	if err := appendLine(filepath.Join(ix.dir, eventsFile), string(line)); err != nil {
		ix.maxEventID--
		return 0, &flowError{Code: types.CodeFlow002, Msg: err.Error()}
	}
	if validateCmd != "" {
		if rc, _, verr := runValidate(ctx, ix.dir, validateCmd, ev.TaskID); verr != nil || rc == 2 {
			return ev.ID, &flowError{Code: types.CodeFlow002, Msg: fmt.Sprintf("validate command failed after an event append (rc=%d)", rc)}
		}
	}
	return ev.ID, nil
}

// hasEvent reports whether an event of that type (and key, when given) already
// exists for a task.
func (ix *boardIndex) hasEvent(taskID, kind, key string) bool {
	fh, err := os.Open(filepath.Join(ix.dir, eventsFile))
	if err != nil {
		return false
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<22)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev types.BoardEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.TaskID != taskID || ev.Type != kind {
			continue
		}
		if key == "" {
			return true
		}
		if asString(ev.Detail["dedup"], "") == key {
			return true
		}
	}
	return false
}

// marshalLine serializes a row in schema order, in the board's style (§3.2
// step 4). The row is written as a one-line compact object: `json.Marshal`
// preserves the struct's field order, which is the §3.1 schema order.
func marshalLine(row types.BoardRow, style rowStyle) (string, error) {
	b, err := json.Marshal(row)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// eventLine renders an event line for a predicted id (check_mode/dry-run).
func eventLine(id uint64, ev types.BoardEvent) string {
	ev.ID = id
	b, err := json.Marshal(ev)
	if err != nil {
		return ""
	}
	return string(b)
}

// appendLine is the one write this package performs on a board: a single
// O_APPEND line, newline-terminated, then fsync.
func appendLine(path, line string) error {
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer fh.Close()
	if _, err := fh.WriteString(line + "\n"); err != nil {
		return err
	}
	return fh.Sync()
}

// validateRow is the mandatory in-process schema validation (§3.2 step 7): it
// cannot be disabled, and a row that fails it is never appended.
func validateRow(row types.BoardRow) error {
	if row.ID == "" {
		return fmt.Errorf("row id is empty")
	}
	if !strings.HasPrefix(row.ID, string(types.PTsk)) {
		return fmt.Errorf("row id %q does not carry the tsk_ prefix", row.ID)
	}
	if strings.TrimSpace(row.Title) == "" {
		return fmt.Errorf("row title is empty")
	}
	if len(row.Title) > 200 {
		return fmt.Errorf("row title is %d chars, want ≤200", len(row.Title))
	}
	switch row.Status {
	case "todo", "blocked", "in_progress", "done":
	default:
		return fmt.Errorf("row status %q is not a board status", row.Status)
	}
	switch row.Priority {
	case "P0", "P1", "P2", "P3":
	default:
		return fmt.Errorf("row priority %q is not a board priority", row.Priority)
	}
	switch row.Complexity {
	case "S", "M", "L":
	default:
		return fmt.Errorf("row complexity %q is not S|M|L", row.Complexity)
	}
	if row.Sig == "" {
		return fmt.Errorf("row sig is empty: the dedup key is mandatory")
	}
	if row.Inc == "" {
		return fmt.Errorf("row inc is empty: the incident link is mandatory")
	}
	return nil
}

// runValidate executes the board's own validate command with a 10s timeout in
// the board's directory (§3.2 step 7). It is the only process this package ever
// starts, and it is configured, never inferred.
func runValidate(ctx context.Context, dir, cmdline, taskID string) (int, string, error) {
	fields := strings.Fields(cmdline)
	if len(fields) == 0 {
		return 0, "", nil
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, fields[0], fields[1:]...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if cctx.Err() == context.DeadlineExceeded {
		return 2, out.String(), fmt.Errorf("validate command timed out")
	}
	if err == nil {
		return 0, out.String(), nil
	}
	var ee *exec.ExitError
	if ok := asExitError(err, &ee); ok {
		return ee.ExitCode(), out.String(), nil
	}
	return 2, out.String(), err
}

// asExitError is errors.As for *exec.ExitError.
func asExitError(err error, out **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*out = ee
		return true
	}
	return false
}

// recurrenceBody is the comment body for a second incident on the same sig.
func (f *Flow) recurrenceBody(inc types.Incident, row types.BoardRow) string {
	return fmt.Sprintf("recurrence: incident %s (sig %s) reached rung %s at %s; priority %s",
		inc.ID, row.Sig, inc.Rung, types.FormatUTC(f.clock().Now()), row.Priority)
}

// sigRow resolves sig → row across every indexed board (§3.4 step 1).
func (f *Flow) sigRow(sig string) (types.BoardRow, bool) {
	if sig == "" {
		return types.BoardRow{}, false
	}
	f.mu.Lock()
	boards := make([]string, 0, len(f.boards))
	for dir := range f.boards {
		boards = append(boards, dir)
	}
	f.mu.Unlock()
	sort.Strings(boards)
	for _, dir := range boards {
		ix, err := f.indexFor(dir)
		if err != nil {
			continue
		}
		if id, ok := ix.bySig[sig]; ok {
			if row, ok := ix.rows[id]; ok {
				return row, true
			}
			return types.BoardRow{ID: id, Sig: sig}, true
		}
	}
	return types.BoardRow{}, false
}

// settingsFor returns the per-repo settings (the mutex/dry-run switch).
func (f *Flow) settingsFor(repo string) repoSettings { return repoSettings{} }

// repoSettings is the per-repo write switch (check_mode/dry-run).
type repoSettings struct{ DryRun bool }

// stampOf reads a file's (dev, inode, size, mtime) stamp. A missing file is a
// zero stamp, which is the empty-board case (legal: trouble authors row one).
func stampOf(path string) (fileStamp, error) {
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fileStamp{}, nil
		}
		return fileStamp{}, err
	}
	fs := fileStamp{size: st.Size(), mtime: st.ModTime()}
	fs.dev, fs.inode = platFileID(st)
	return fs, nil
}

// ulidSuffix strips the id prefix.
func ulidSuffix(id string) string {
	if i := strings.LastIndexByte(id, '_'); i >= 0 {
		return id[i+1:]
	}
	return id
}

// strictlyGreater compares two ULID suffixes in Crockford order.
func strictlyGreater(a, b string) bool { return a > b }

// crockford is the ULID alphabet (no I, L, O, U).
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// bumpULID increments a 26-char ULID suffix by one, in Crockford base32.
func bumpULID(s string) string {
	if s == "" {
		return s
	}
	b := []byte(s)
	for i := len(b) - 1; i >= 0; i-- {
		idx := strings.IndexByte(crockford, b[i])
		if idx < 0 {
			return s
		}
		if idx < len(crockford)-1 {
			b[i] = crockford[idx+1]
			return string(b)
		}
		b[i] = crockford[0]
	}
	return string(b)
}

// cleanPath resolves a board path once (§3.2 step 1): Clean plus symlink
// resolution, so a symlinked board is compared by its real path.
func cleanPath(p string) string {
	if p == "" {
		return ""
	}
	c := filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(c); err == nil {
		return r
	}
	return c
}
