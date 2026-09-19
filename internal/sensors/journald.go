package sensors

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// SPEC-03 §3.3 — the journald collector.
//
// A seek failure is never "no logs": measured on this host,
// `journalctl -n 0 -o json --after-cursor=notacursor` exits rc=1 with zero
// stdout and `Failed to seek to cursor: Invalid argument`, which must become
// TROUBLE-SENSORS-007 + a `--since` fallback + one gap, never an empty stream
// treated as silence.

// journalBackoff is the restart ladder of SPEC-03 §3.3.
var journalBackoff = []time.Duration{
	250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second,
	4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second,
}

const (
	journalCursorRing     = 4096
	journalSaveEveryN     = 25
	journalSaveEvery      = 5 * time.Second
	journalOutputFields   = "__CURSOR,MESSAGE,PRIORITY,_SYSTEMD_UNIT,SYSLOG_IDENTIFIER,__REALTIME_TIMESTAMP,_PID,_HOSTNAME,_COMM"
	journalChildJoinBound = 5 * time.Second
)

// journalResult is the outcome of one bounded journalctl invocation.
type journalResult struct {
	stdout string
	stderr string
	err    error
}

// runJournalCmd runs one journalctl invocation with the guarantees the
// collector depends on:
//   - the child gets its own process group, so a shell wrapper's descendants
//     cannot hold the pipe open past the context deadline (the measured failure
//     mode: `-n 0` answers instantly, but a wedged child would otherwise eat the
//     unit's TimeoutStopSec);
//   - cancellation kills the whole group, not just the direct child;
//   - WaitDelay bounds the reap after cancellation.
func runJournalCmd(ctx context.Context, path string, args ...string) journalResult {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.SysProcAttr = processGroupAttr()
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return killProcessGroup(cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	return journalResult{stdout: out.String(), stderr: errb.String(), err: err}
}

// probeContext bounds a probe and cancels it as soon as the daemon is asked to
// stop, so Stop never waits on a journalctl that is not answering.
func (s *Sensors) probeContext(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(parent, d)
	go func() {
		select {
		case <-s.stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// journalEntry is one parsed, already-normalized journald entry.
type journalEntry struct {
	cursor   string
	ts       time.Time
	message  []byte // scrubbed bytes; never the raw journal bytes
	rawMsg   []byte // pre-scrub, in-memory only (never persisted)
	unit     string // _SYSTEMD_UNIT (visible unit name)
	comm     string // _COMM / SYSLOG_IDENTIFIER
	priority int
	pid      int
	nonUTF8  bool
	trunc    bool
	dropped  int // bytes dropped by the per-entry cap
	redact   int
}

type journalState struct {
	mu         sync.Mutex
	followers  map[string]*journalFollower
	path       string
	childDead  atomic.Bool
	disabled   atomic.Bool
	readDenied atomic.Bool
	dropped    atomic.Uint64
	entries    atomic.Uint64
	depth      atomic.Int64
	capacity   atomic.Int64
	// handlerPaused lets a test prove the drop accounting exactly: with the
	// handler stopped, every push past the cap drops exactly one entry.
	handlerPaused atomic.Bool
	pausedWG      sync.WaitGroup
}

// journalFollower is one `journalctl -f` child at one scope (SPEC-03 §3.10).
type journalFollower struct {
	scope      string
	cursorFile string
	cmd        *exec.Cmd
	cursor     string
	lastTS     time.Time
	ring       []string
	ringAt     int

	q        *journalQueue
	unsaved  int
	lastSave time.Time
	// overflowing is the current overflow episode: it opens on the first drop
	// and closes when the handler drains the queue back under its cap, so one
	// episode produces exactly one gap record.
	overflowing bool

	backoffIx int
	entries   int64
}

// queueBytesUsed tracks the byte budget of the bounded queue (32 MB default).
type journalQueue struct {
	entries []*journalEntry
	bytes   int
	cap     int
	maxByte int
	// dropped is atomic: the reader increments it while the handler closes the
	// episode that reports it.
	dropped atomic.Uint64
}

func newJournalQueue(cap, maxByte int) *journalQueue {
	return &journalQueue{cap: cap, maxByte: maxByte}
}

// push appends, dropping the oldest entry when either bound is exceeded. A drop
// is never silent and never blocks the child (SPEC-03 §3.3).
func (q *journalQueue) push(e *journalEntry) (droppedOne bool) {
	sz := len(e.message) + len(e.rawMsg) + 64
	if len(q.entries) >= q.cap || (q.maxByte > 0 && q.bytes+sz > q.maxByte) {
		q.pop()
		q.dropped.Add(1)
		droppedOne = true
	}
	q.entries = append(q.entries, e)
	q.bytes += sz
	return droppedOne
}

func (q *journalQueue) pop() *journalEntry {
	if len(q.entries) == 0 {
		return nil
	}
	e := q.entries[0]
	q.entries = q.entries[1:]
	q.bytes -= len(e.message) + len(e.rawMsg) + 64
	if q.bytes < 0 {
		q.bytes = 0
	}
	return e
}

func (q *journalQueue) len() int { return len(q.entries) }

// startJournald starts the follower set (SPEC-03 §4 step 5).
func (s *Sensors) startJournald(ctx context.Context) error {
	if !s.cfg.journald.enabled {
		s.setSensor(types.SenJournald, false, false, "disabled by configuration: sensors.journald.enabled=false")
		return nil
	}
	s.jlState.followers = map[string]*journalFollower{}
	s.jlState.capacity.Store(int64(s.cfg.journald.queue))

	path := s.jlState.path
	if path == "" {
		p, err := exec.LookPath("journalctl")
		if err != nil {
			s.recError(types.SenJournald, types.CodeSensors006, "journalctl not found in PATH")
			s.setSensor(types.SenJournald, false, true, string(types.CodeSensors006)+": journalctl not found in PATH")
			s.jlState.disabled.Store(true)
			return nil
		}
		path = p
	}
	// Membership is verified by *executing* the follow, never by reading the
	// process group list: the grant is effective only after a restart.
	if !s.journalReadable(ctx, path) {
		s.jlState.readDenied.Store(true)
		reason := fmt.Sprintf("no journal read access: uid=%d not in adm/systemd-journal (restart required after grant)",
			os.Getuid())
		s.recError(types.SenJournald, types.CodeSensors009, reason)
		s.setSensor(types.SenJournald, true, true, reason)
		s.openGap(types.SenJournald, "all", "access_denied", reason)
		s.emitGapNow(ctx, string(types.SenJournald), "all", "access_denied", "-1")
		return nil
	}
	s.setSensor(types.SenJournald, true, false, "")

	if s.cfg.journald.followAll {
		if n, err := s.journalEntryCount(ctx, path); err == nil && n > int64(s.cfg.journald.followAllMaxEntries) {
			reason := fmt.Sprintf("follow_all refused: journal holds %d entries (> sensors.journald.follow_all_max_entries=%d)",
				n, s.cfg.journald.followAllMaxEntries)
			s.recError(types.SenJournald, types.CodeSensors009, reason)
			s.setSensor(types.SenJournald, true, true, reason)
			return nil
		}
	}

	scopes := s.journalScopes()
	for _, scope := range scopes {
		f := &journalFollower{
			scope:      scope,
			cursorFile: filepath.Join(s.cfg.stateRoot, "spool", "journald", scope+".cursor"),
			ring:       make([]string, journalCursorRing),
			q:          newJournalQueue(s.cfg.journald.queue, s.cfg.journald.queueBytes),
		}
		f.cursor = s.loadCursor(f.cursorFile)
		if f.cursor != "" {
			f.lastTS = s.cursorMTime(f.cursorFile)
		}
		s.jlState.mu.Lock()
		s.jlState.followers[scope] = f
		s.jlState.mu.Unlock()
		s.wg.Add(3)
		go s.runJournalReader(ctx, f, path)
		go s.runJournalSeekProbe(ctx, f, path)
		go s.runJournalHandler(ctx, f)
	}
	return nil
}

// emitterGapCloser closes an overflow episode once the queue drains, so the
// recovery record is emitted with the episode it ends (SPEC-03 §3.9).
func (s *Sensors) journalEpisodeCheck(ctx context.Context, f *journalFollower) {
	if !f.overflowing {
		return
	}
	if f.q.len() < f.q.cap {
		f.overflowing = false
		s.closeGap(ctx, types.SenJournald, f.scope, "queue_overflow")
	}
}

// runJournalHandler drains one follower's bounded queue. The reader never
// blocks on the ledger: a backed-up handler costs drops (counted and gapped),
// not a stalled journald attribution.
func (s *Sensors) runJournalHandler(ctx context.Context, f *journalFollower) {
	defer s.wg.Done()
	for {
		if !s.jlState.handlerPaused.Load() {
			for {
				e := f.q.pop()
				if e == nil {
					break
				}
				s.jlState.depth.Add(-1)
				s.drainJournal(ctx, f, e)
			}
			s.journalEpisodeCheck(ctx, f)
		}
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// journalScopes returns the follow scopes: the configured units, or the
// daemon's own unit when the list is empty (never the whole journal by
// accident).
func (s *Sensors) journalScopes() []string {
	if len(s.cfg.journald.units) > 0 {
		return append([]string{}, s.cfg.journald.units...)
	}
	if s.cfg.journald.followAll {
		return []string{"all"}
	}
	self := os.Getenv("TROUBLE_SELF_UNIT")
	if self == "" {
		self = "troubled.service"
	}
	return []string{self}
}

// journalReadable executes the `-n 1` verification (SPEC-03 §3.3 membership).
func (s *Sensors) journalReadable(ctx context.Context, path string) bool {
	cctx, cancel := s.probeContext(ctx, 5*time.Second)
	defer cancel()
	res := runJournalCmd(cctx, path, "-n", "1", "-o", "json", "--output-fields", "__CURSOR")
	if res.err != nil {
		return false
	}
	return strings.Contains(res.stdout, "__CURSOR") || len(strings.TrimSpace(res.stdout)) > 0
}

func (s *Sensors) journalEntryCount(ctx context.Context, path string) (int64, error) {
	cctx, cancel := s.probeContext(ctx, 20*time.Second)
	defer cancel()
	res := runJournalCmd(cctx, path, "--disk-usage")
	if res.err != nil {
		return 0, res.err
	}
	// "Archived and active journals take up 1023.3M in the file system."
	f := strings.Fields(res.stdout)
	for i, tok := range f {
		if strings.HasPrefix(tok, "up") && i+1 < len(f) {
			return parseJournalSize(f[i+1])
		}
	}
	return 0, nil
}

func parseJournalSize(tok string) (int64, error) {
	tok = strings.TrimRight(tok, ".,")
	if tok == "" {
		return 0, errors.New("empty size")
	}
	mult := int64(1)
	switch tok[len(tok)-1] {
	case 'K':
		mult = 1 << 10
		tok = tok[:len(tok)-1]
	case 'M':
		mult = 1 << 20
		tok = tok[:len(tok)-1]
	case 'G':
		mult = 1 << 30
		tok = tok[:len(tok)-1]
	}
	v, err := strconv.ParseFloat(tok, 64)
	if err != nil {
		return 0, err
	}
	// Roughly 1 KiB per entry: the cap is an entry count, and this keeps the
	// refusal conservative rather than optimistic.
	return int64(v * float64(mult) / 1024), nil
}

func (s *Sensors) loadCursor(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (s *Sensors) cursorMTime(path string) time.Time {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime().UTC()
}

// saveCursor writes the cursor file (SPEC-03 §3.3: after every 25 entries or
// 5s, whichever first).
func (s *Sensors) saveCursor(f *journalFollower, force bool) {
	if f.cursor == "" || f.cursorFile == "" {
		// A follower with no cursor path is a programming error, not a reason
		// to write into the process's working directory.
		return
	}
	if !force {
		if f.unsaved < journalSaveEveryN && s.now().Sub(f.lastSave) < journalSaveEvery {
			return
		}
	}
	if err := os.MkdirAll(filepath.Dir(f.cursorFile), 0o700); err != nil {
		return
	}
	tmp := f.cursorFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(f.cursor+"\n"), 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, f.cursorFile); err != nil {
		return
	}
	f.unsaved = 0
	f.lastSave = s.now()
}

// journalArgs builds the documented command line. `-n 0` after the cursor is
// what makes the follow start *after* the cursor rather than replaying.
func journalArgs(scope, cursor string) []string {
	args := []string{"-f", "-o", "json"}
	if cursor != "" {
		args = append(args, "--after-cursor", cursor)
	}
	if scope != "all" {
		args = append(args, "-u", scope)
	}
	args = append(args, "--output-fields", journalOutputFields, "-n", "0")
	return args
}

// runJournalReader supervises one child with the §3.3 backoff ladder, the
// cursor-invalid fallback and the gap-on-death accounting.
func (s *Sensors) runJournalReader(ctx context.Context, f *journalFollower, path string) {
	defer s.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		default:
		}
		if s.jlState.disabled.Load() {
			return
		}
		started := s.now()
		err := s.followOnce(ctx, f, path)
		if ctx.Err() != nil {
			return
		}
		select {
		case <-s.stopCh:
			return
		default:
		}
		s.jlState.childDead.Store(true)
		downtime := s.now().Sub(started)
		s.recError(types.SenJournald, types.CodeSensors008,
			fmt.Sprintf("journal child for %s exited: %v", f.scope, err))
		s.emitGapNow(ctx, string(types.SenJournald), f.scope, "child_died", fmt.Sprintf("exit status %v", err))
		// Resume from the persisted cursor before the child is restarted, so
		// no entry is re-delivered.
		s.saveCursor(f, true)
		d := s.backoff(f)
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-time.After(d):
		}
		_ = downtime
	}
}

func (s *Sensors) backoff(f *journalFollower) time.Duration {
	ix := f.backoffIx
	if ix >= len(journalBackoff) {
		ix = len(journalBackoff) - 1
	}
	f.backoffIx++
	base := journalBackoff[ix]
	// ≤10% jitter, derived from the cursor length so it is deterministic in
	// tests and still spread in production.
	jitter := (int64(len(f.cursor)) + f.entries) % 11 * int64(base/100)
	return base + time.Duration(jitter)
}

// followOnce runs one child to completion.
func (s *Sensors) followOnce(ctx context.Context, f *journalFollower, path string) error {
	// A malformed cursor is a hard failure: probe it before following, so the
	// fallback is chosen deliberately instead of discovered as silence.
	if f.cursor != "" && !s.cursorValid(ctx, path, f.cursor) {
		s.recError(types.SenJournald, types.CodeSensors007,
			fmt.Sprintf("cursor invalid for %s: Failed to seek to cursor", f.scope))
		since := f.lastTS.Add(-time.Second)
		if since.IsZero() {
			since = s.now().Add(-time.Minute)
		}
		s.emitGapNow(ctx, string(types.SenJournald), f.scope, "cursor_invalid", "-1")
		f.cursor = ""
		return s.followSince(ctx, f, path, since)
	}
	args := journalArgs(f.scope, f.cursor)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(cctx, path, args...)
	cmd.SysProcAttr = processGroupAttr()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr := &strings.Builder{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	f.cmd = cmd
	// The child is terminated as a PROCESS GROUP on the stop path: a shell
	// wrapper's descendants (`sh -c "journalctl …"`) inherit the pipe and would
	// otherwise hold the reader open for as long as they sleep, which is how a
	// 5s join turns into a 30s drain. The main path reaps; the supervisor never
	// calls Wait (a second Wait on the same Cmd is an error).
	childDone := make(chan struct{})
	s.superviseChild(cmd, cctx, childDone)
	err = s.consumeJournal(ctx, f, stdout)
	if werr := waitJournal(cmd); werr != nil && err == nil {
		err = werr
	}
	close(childDone)
	if strings.Contains(stderr.String(), "Failed to seek") {
		s.recError(types.SenJournald, types.CodeSensors007,
			"Failed to seek to cursor: "+strings.TrimSpace(stderr.String()))
		since := f.lastTS.Add(-time.Second)
		s.emitGapNow(ctx, string(types.SenJournald), f.scope, "cursor_invalid", "-1")
		f.cursor = ""
		return s.followSince(ctx, f, path, since)
	}
	return err
}

func waitJournal(cmd *exec.Cmd) error {
	if cmd.ProcessState != nil {
		return nil
	}
	return cmd.Wait()
}

// superviseChild terminates one child (and its whole process group) on the
// shutdown path: SIGTERM, join with the documented 5s bound, then SIGKILL.
// done is closed by the caller after it has reaped the child; a nil done means
// the caller reaps on its own and only the SIGTERM is sent.
func (s *Sensors) superviseChild(cmd *exec.Cmd, cctx context.Context, done <-chan struct{}) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	go func() {
		select {
		case <-s.stopCh:
		case <-cctx.Done():
			return
		}
		// The cursor is persisted before the child is signalled on the stop
		// path, so no entry is re-delivered (SPEC-03 §6 edge case 24).
		_ = killProcessGroup(pid, syscall.SIGTERM)
		if done == nil {
			return
		}
		select {
		case <-done:
		case <-time.After(journalChildJoinBound):
			_ = killProcessGroup(pid, syscall.SIGKILL)
		}
	}()
}

// followSince implements the §3.3 fallback: --since=<last_entry_ts - 1s> + a
// rescan. If the fallback timestamp is outside the journal's retention the
// entries are gone, and that is recorded as est_lost = -1.
func (s *Sensors) followSince(ctx context.Context, f *journalFollower, path string, since time.Time) error {
	args := []string{"-f", "-o", "json"}
	if f.scope != "all" {
		args = append(args, "-u", f.scope)
	}
	args = append(args, "--since", since.Format("2006-01-02 15:04:05"),
		"--output-fields", journalOutputFields, "-n", "0")
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(cctx, path, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr := &strings.Builder{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	f.cmd = cmd
	childDone := make(chan struct{})
	s.superviseChild(cmd, cctx, childDone)
	err = s.consumeJournal(ctx, f, stdout)
	waitErr := cmd.Wait()
	close(childDone)
	if err == nil {
		err = waitErr
	}
	if strings.Contains(stderr.String(), "too old") || strings.Contains(stderr.String(), "Failed to seek") {
		s.emitGapNow(ctx, string(types.SenJournald), f.scope, "cursor_invalid",
			"retention_window_exceeded")
		s.recError(types.SenJournald, types.CodeSensors007, "retention_window_exceeded")
	}
	return err
}

// consumeJournal reads the child's JSON stream, dedupes by exact cursor
// equality (cursors are not orderable) and feeds the bounded queue.
func (s *Sensors) consumeJournal(ctx context.Context, f *journalFollower, r io.Reader) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
	for sc.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.stopCh:
			return nil
		default:
		}
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		e, err := s.parseJournalEntry(f, line)
		if err != nil {
			continue // a malformed line is not an entry; it is not silence either
		}
		if e == nil {
			continue // duplicate cursor: dropped by dedupe, not by a queue
		}
		f.entries++
		s.jlState.entries.Add(1)
		s.queueJournal(ctx, f, e)
	}
	return sc.Err()
}

// queueJournal pushes one entry into the bounded queue and turns a drop into
// the documented accounting: Dropped++, TROUBLE-SENSORS-010, and exactly one
// gap record per overflow episode (SPEC-03 §3.3, §5 row 010).
func (s *Sensors) queueJournal(ctx context.Context, f *journalFollower, e *journalEntry) {
	dropped := f.q.push(e)
	if !dropped {
		// The pipeline gauge is occupancy, not throughput: a dropping push
		// leaves the depth unchanged because it evicted what it added.
		s.jlState.depth.Add(1)
		return
	}
	s.jlState.dropped.Add(1)
	if rt := s.rt[types.SenJournald]; rt != nil {
		rt.drops.Add(1)
	}
	// One episode, one gap: the episode opens on the first drop and closes when
	// the handler has drained the queue back under its cap.
	if !f.overflowing {
		f.overflowing = true
		s.recError(types.SenJournald, types.CodeSensors010,
			fmt.Sprintf("journal queue overflow at %d entries: drop-oldest", f.q.cap))
		// The number lost is only final when the episode closes, so the episode
		// reads it from the queue rather than freezing it at the first drop.
		s.openGapFunc(types.SenJournald, f.scope, "queue_overflow",
			fmt.Sprintf("cap=%d", f.q.cap), func() int { return int(f.q.dropped.Load()) })
	}
}

// parseJournalEntry decodes one JSON line; a repeated cursor returns (nil,nil).
func (s *Sensors) parseJournalEntry(f *journalFollower, line []byte) (*journalEntry, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, err
	}
	get := func(k string) string {
		if v, ok := raw[k]; ok {
			var sv string
			if err := json.Unmarshal(v, &sv); err == nil {
				return sv
			}
			var nv json.Number
			if err := json.Unmarshal(v, &nv); err == nil {
				return nv.String()
			}
		}
		return ""
	}
	cur := get("__CURSOR")
	if cur == "" {
		return nil, errors.New("entry without __CURSOR")
	}
	// Dedupe by exact cursor equality only (SPEC-03 §3.3).
	for i := 0; i < len(f.ring); i++ {
		if f.ring[i] == cur {
			return nil, nil
		}
	}
	f.ring[f.ringAt] = cur
	f.ringAt = (f.ringAt + 1) % len(f.ring)
	f.cursor = cur
	f.unsaved++

	msgRaw := []byte(get("MESSAGE"))
	e := &journalEntry{cursor: cur, rawMsg: msgRaw}
	e.unit = get("_SYSTEMD_UNIT")
	e.comm = get("SYSLOG_IDENTIFIER")
	if e.comm == "" {
		e.comm = get("_COMM")
	}
	if p := get("PRIORITY"); p != "" {
		e.priority, _ = strconv.Atoi(p)
	}
	if p := get("_PID"); p != "" {
		e.pid, _ = strconv.Atoi(p)
	}
	if micro := get("__REALTIME_TIMESTAMP"); micro != "" {
		if n, err := strconv.ParseInt(micro, 10, 64); err == nil {
			e.ts = time.UnixMicro(n).UTC()
			f.lastTS = e.ts
		}
	}
	if e.ts.IsZero() {
		e.ts = s.now()
	}
	// Non-UTF-8 MESSAGE: replace, flag, keep the entry, never persist the raw
	// bytes (SPEC-03 §3.3, edge case 8). journalctl emits the message bytes
	// inside a JSON string, so the invalid sequence is detected on the raw line;
	// by the time encoding/json has decoded the field it is already U+FFFD, and
	// this flag is what keeps that substitution from being silent.
	if !utf8.Valid(line) || !utf8.Valid(msgRaw) {
		e.nonUTF8 = true
		msgRaw = []byte(strings.ToValidUTF8(string(msgRaw), "\uFFFD"))
	}
	// Per-entry cap (SPEC-03 §3.3): truncate at the cap and record the delta.
	if cap := s.cfg.journald.maxEntry; cap > 0 && len(msgRaw) > cap {
		e.trunc = true
		e.dropped = len(msgRaw) - cap
		msgRaw = msgRaw[:cap]
	}
	// The scrubbed bytes are what the ledger may ever see.
	res, err := s.redact(msgRaw, "journal_tail")
	if err != nil {
		return nil, err
	}
	e.message = res.Value
	e.redact = res.Redactions
	return e, nil
}

// drainJournal emits one entry's event record (the handler half of the queue).
func (s *Sensors) drainJournal(ctx context.Context, f *journalFollower, e *journalEntry) {
	if s.jlState.handlerPaused.Load() {
		return
	}
	s.saveCursor(f, false)
	ident := e.unit
	if ident == "" {
		ident = e.comm
	}
	norm := messageNorm(string(e.message))
	masked := maskMessage(string(e.message))
	scope := f.scope
	if e.unit != "" {
		scope = e.unit
	}
	detail := map[string]any{
		"unit":           e.unit,
		"comm":           e.comm,
		"priority":       float64(e.priority),
		"priority_name":  priorityName(e.priority),
		"non_utf8":       e.nonUTF8,
		"truncated":      e.trunc,
		"dropped_bytes":  float64(e.dropped),
		"masked_message": masked,
		"message_norm":   norm,
		"cursor":         e.cursor,
		"pid":            float64(e.pid),
		"msg":            masked,
		"substr":         masked,
		"count":          1,
		"value":          1,
		"unit_field":     e.unit,
		"severity":       string(severityForPriority(e.priority)),
		"window_s":       0,
		"age_s":          0,
		"redactions":     e.redact,
		"ts_entry":       types.FormatUTC(e.ts),
	}
	ev := types.SensorEvent{
		ID:     types.NewID(types.PEv),
		TS:     types.FormatUTC(e.ts),
		Sensor: types.SenJournald,
		Scope:  scope,
		Value:  1,
		Unit:   "count",
		Detail: detail,
	}
	ev.Sig = sigFor(types.SrcJournald, ident, norm)
	// M3: a journald entry for a unit with a fresh dbus arrival is evidence for
	// that unit's incident, not a new one.
	if e.unit != "" && s.dbState.merge != nil {
		if attach := s.dbState.merge.noteJournalArrival(e.unit, e.ts); attach {
			detail["attach"] = true
			detail["arrival_path"] = "journald"
			s.dbState.arrivals.Add(1)
		}
	}
	if rt := s.rt[types.SenJournald]; rt != nil {
		rt.events.Add(1)
		rt.lastEvent.Store(s.now().UnixNano())
	}
	s.sensorOK(types.SenJournald)
	s.handleEvent(ctx, ev)
	s.jlState.depth.Store(int64(s.journalQueueDepth()))
}

func (s *Sensors) journalQueueDepth() int {
	s.jlState.mu.Lock()
	defer s.jlState.mu.Unlock()
	return 0
}

// runJournalSeekProbe is the journald liveness proof: the seek probe, never
// entry arrival (SPEC-03 §3.3, §3.9). A quiet journal is healthy.
func (s *Sensors) runJournalSeekProbe(ctx context.Context, f *journalFollower, path string) {
	defer s.wg.Done()
	iv := s.cfg.journald.probeInterval
	if iv <= 0 {
		iv = 30 * time.Second
	}
	tk := time.NewTicker(iv)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-tk.C:
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		ok := s.cursorValidOrQuiet(cctx, path, f)
		cancel()
		if ok {
			s.sensorOK(types.SenJournald)
			s.jlState.childDead.Store(false)
			if rt := s.rt[types.SenJournald]; rt != nil && rt.degraded.Load() {
				s.closeGap(ctx, types.SenJournald, f.scope, "child_died")
				s.closeGap(ctx, types.SenJournald, f.scope, "heartbeat_stale")
			}
			continue
		}
	}
}

// cursorValid runs the documented probe: `-n 0 -o json --after-cursor=<c>`;
// rc=0 means the cursor is still valid and the journal is readable.
func (s *Sensors) cursorValid(ctx context.Context, path, cursor string) bool {
	cctx, cancel := s.probeContext(ctx, 10*time.Second)
	defer cancel()
	res := runJournalCmd(cctx, path, "-n", "0", "-o", "json", "--after-cursor", cursor)
	if res.err != nil {
		return false
	}
	return !strings.Contains(res.stderr, "Failed to seek")
}

func (s *Sensors) cursorValidOrQuiet(ctx context.Context, path string, f *journalFollower) bool {
	if f.cursor == "" {
		return s.journalReadable(ctx, path)
	}
	return s.cursorValid(ctx, path, f.cursor)
}

// stopJournald terminates every child and flushes its cursor before the signal
// so no entry is re-delivered (SPEC-03 §4, edge case 24).
func (s *Sensors) stopJournald() {
	s.jlState.mu.Lock()
	followers := make([]*journalFollower, 0, len(s.jlState.followers))
	for _, f := range s.jlState.followers {
		followers = append(followers, f)
	}
	s.jlState.mu.Unlock()
	for _, f := range followers {
		s.saveCursor(f, true)
		if f.cmd != nil && f.cmd.Process != nil {
			_ = f.cmd.Process.Signal(syscall.SIGTERM)
		}
	}
}

func priorityName(p int) string {
	switch {
	case p <= 0:
		return "emerg"
	case p == 1:
		return "alert"
	case p == 2:
		return "crit"
	case p == 3:
		return "err"
	case p == 4:
		return "warning"
	case p == 5:
		return "notice"
	case p == 6:
		return "info"
	}
	return "debug"
}

func severityForPriority(p int) types.Severity {
	switch {
	case p <= 2:
		return types.SevCritical
	case p == 3:
		return types.SevHigh
	case p == 4:
		return types.SevMedium
	case p <= 6:
		return types.SevLow
	}
	return types.SevInfo
}
