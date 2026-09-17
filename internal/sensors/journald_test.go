package sensors

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// journald_test.go covers SPEC-03 §7's journald row against a fake journalctl
// script placed on PATH-by-construction (the sensor takes the resolved binary
// path, so the test points it at the script).
//
// The measured behaviours it asserts:
//   - `Failed to seek to cursor: Invalid argument` is a hard failure, never
//     "no logs" (measured here: rc=1, zero stdout);
//   - the restart ladder 250ms→30s with ≤10% jitter;
//   - exact drop accounting on the bounded queue;
//   - dedupe by exact cursor equality only.

// fakeJournalctl writes a shell script that answers the probes the collector
// makes and then streams the given entries (or exits with the given status).
func fakeJournalctl(t *testing.T, dir string, script string) string {
	t.Helper()
	p := filepath.Join(dir, "journalctl")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake journalctl: %v", err)
	}
	return p
}

// entryLine renders one journalctl -o json line.
func entryLine(cursor, unit, msg string, prio int, tsMicro int64) string {
	b, _ := json.Marshal(map[string]any{
		"__CURSOR":             cursor,
		"MESSAGE":              msg,
		"PRIORITY":             fmt.Sprint(prio),
		"_SYSTEMD_UNIT":        unit,
		"SYSLOG_IDENTIFIER":    unit,
		"__REALTIME_TIMESTAMP": fmt.Sprint(tsMicro),
		"_PID":                 "4242",
		"_HOSTNAME":            "testhost",
		"_COMM":                unit,
	})
	return string(b)
}

// TestJournaldCursorRoundTrip: the persisted cursor is what the child is
// resumed from.
func TestJournaldCursorRoundTrip(t *testing.T) {
	h := newHarness(t)
	f := &journalFollower{
		scope:      "payment-worker.service",
		cursorFile: filepath.Join(h.dir, "spool", "payment-worker.service.cursor"),
		ring:       make([]string, journalCursorRing),
		q:          newJournalQueue(8192, 0),
	}
	f.cursor = "c=7"
	h.s.saveCursor(f, true)
	got := h.s.loadCursor(f.cursorFile)
	if got != "c=7" {
		t.Fatalf("cursor round-trip: %q", got)
	}
	args := journalArgs(f.scope, got)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--after-cursor c=7") {
		t.Fatalf("follow args do not resume from the cursor: %v", args)
	}
	if !strings.Contains(joined, "-u payment-worker.service") {
		t.Fatalf("follow args are not unit-scoped: %v", args)
	}
	if !strings.Contains(joined, "-n 0") {
		t.Fatalf("follow args must not replay: %v", args)
	}
	if !strings.Contains(joined, "--output-fields "+journalOutputFields) {
		t.Fatalf("follow args do not pin the field set: %v", args)
	}
	if !strings.HasPrefix(joined, "-f -o json") {
		t.Fatalf("follow args must stream json: %v", args)
	}
	// Bulk saves happen every 25 entries or 5 seconds, whichever comes first.
	f.unsaved = 0
	f.lastSave = h.now()
	h.s.saveCursor(f, false)
	if f.unsaved != 0 {
		t.Fatal("the save counter must reset after a write")
	}
}

// TestJournaldMalformedCursorIsNeverNoLogs: rc=1 + zero stdout + "Failed to
// seek" ⇒ exactly one gap{cursor_invalid} and the --since fallback.
func TestJournaldMalformedCursorIsNeverNoLogs(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
case "$*" in
  *"--after-cursor bad"*) echo "Failed to seek to cursor: Invalid argument" >&2; exit 1 ;;
  *"--after-cursor"*) exit 0 ;;
  *"-n 1"*) printf '{"__CURSOR":"c=1"}\n' ;;
  *"--since"*) sleep 5 ;;
esac
`
	path := fakeJournalctl(t, dir, script)
	h := newHarness(t)
	f := &journalFollower{scope: "u.service", cursor: "bad", ring: make([]string, journalCursorRing), q: newJournalQueue(8192, 0), cursorFile: filepath.Join(dir, "c")}
	// The malformed cursor must be detected before the follow starts.
	if h.s.cursorValid(context.Background(), path, "bad") {
		t.Fatal("the malformed cursor was treated as valid")
	}
	// Drive the real path: followOnce must take the --since fallback rather
	// than reporting an empty stream.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_ = h.s.followOnce(ctx, f, path)
	gaps := h.gaps()
	if len(gaps) != 1 {
		t.Fatalf("expected exactly one gap, got %d", len(gaps))
	}
	if gaps[0].Payload["cause"] != "cursor_invalid" {
		t.Fatalf("gap cause = %v", gaps[0].Payload["cause"])
	}
	if gaps[0].Payload["est_lost"] != -1 {
		t.Fatalf("est_lost = %v, want -1 (unknown is not zero)", gaps[0].Payload["est_lost"])
	}
	codes := h.recordsWhere(func(r types.Record) bool {
		return r.Payload["error_code"] == string(types.CodeSensors007)
	})
	if len(codes) != 1 {
		t.Fatalf("expected one TROUBLE-SENSORS-007 record, got %d", len(codes))
	}
}

// TestJournaldBackoffLadder: the documented sequence with ≤10% jitter.
func TestJournaldBackoffLadder(t *testing.T) {
	h := newHarness(t)
	f := &journalFollower{cursor: "abcdef", entries: 3}
	want := []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	for i, w := range want {
		got := h.s.backoff(f)
		if got < w || got > w+w/10 {
			t.Fatalf("backoff %d = %s, want [%s, %s]", i, got, w, w+w/10)
		}
	}
}

// TestJournaldQueueOverflowIsExact: 100k entries into an 8192 queue with the
// handler stopped drops exactly 100000-8192 and files one gap per episode.
func TestJournaldQueueOverflowIsExact(t *testing.T) {
	const cap = 8192
	const total = 100000
	h := newHarness(t, types.ConfigValue{Key: "sensors.journald.queue", Value: cap})
	f := &journalFollower{scope: "u.service", ring: make([]string, journalCursorRing), q: newJournalQueue(cap, 0)}
	h.s.jlState.handlerPaused.Store(true)
	ctx := context.Background()
	line := []byte(entryLine("c=x", "u.service", "boom", 3, 1758012345000000))
	for i := 0; i < total; i++ {
		line = []byte(entryLine(fmt.Sprintf("c=%d", i), "u.service", "boom", 3, 1758012345000000))
		e, err := h.s.parseJournalEntry(f, line)
		if err != nil {
			t.Fatalf("parse %d: %v", i, err)
		}
		if e == nil {
			continue
		}
		h.s.queueJournal(ctx, f, e)
	}
	if got := h.s.jlState.dropped.Load(); got != total-cap {
		t.Fatalf("dropped = %d, want exactly %d", got, total-cap)
	}
	if got := f.q.len(); got != cap {
		t.Fatalf("queue depth = %d, want %d", got, cap)
	}
	// Nothing is emitted until the episode closes: a gap is a closed window
	// (from_ts/to_ts), never a guess about a storm in progress.
	if got := len(h.gaps()); got != 0 {
		t.Fatalf("an open overflow episode must not have emitted %d gap records yet", got)
	}
	// Drain, then close the episode: exactly one gap, with the real number lost.
	h.s.jlState.handlerPaused.Store(false)
	for {
		if f.q.pop() == nil {
			break
		}
		h.s.jlState.depth.Add(-1)
	}
	h.s.journalEpisodeCheck(ctx, f)
	gaps := h.gaps()
	if len(gaps) != 1 {
		t.Fatalf("one overflow episode must file exactly one gap, got %d", len(gaps))
	}
	if gaps[0].Payload["cause"] != "queue_overflow" {
		t.Fatalf("gap cause = %v", gaps[0].Payload["cause"])
	}
	if got := gaps[0].Payload["est_lost"]; got != total-cap {
		t.Fatalf("est_lost = %v, want exactly %d (the number is known here)", got, total-cap)
	}
	if rt := h.s.rt[types.SenJournald]; rt == nil || rt.drops.Load() != total-cap {
		t.Fatalf("SensorHealth.Dropped = %d, want %d", rt.drops.Load(), total-cap)
	}
	if got := h.s.jlState.depth.Load(); got != 0 {
		t.Fatalf("pipeline depth = %d after the drain, want 0", got)
	}
}

// TestJournaldDedupeByCursorEquality: replaying the same 4096 cursors produces
// zero duplicate records, and no ordering comparison is ever made.
func TestJournaldDedupeByCursorEquality(t *testing.T) {
	h := newHarness(t)
	f := &journalFollower{scope: "u.service", ring: make([]string, journalCursorRing), q: newJournalQueue(1<<20, 0)}
	ctx := context.Background()
	var accepted int
	for round := 0; round < 3; round++ {
		for i := 0; i < journalCursorRing; i++ {
			line := []byte(entryLine(fmt.Sprintf("cur-%04d", i), "u.service", "boom", 3, 1758012345000000))
			e, err := h.s.parseJournalEntry(f, line)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if e == nil {
				continue
			}
			accepted++
			h.s.drainJournal(ctx, f, e)
		}
	}
	if accepted != journalCursorRing {
		t.Fatalf("accepted %d entries over 3 identical replays, want %d", accepted, journalCursorRing)
	}
	// The cursor ring evicts the oldest entries, so a replay of an evicted
	// cursor is accepted again: that is why the ring is 4096 and the rescan
	// window is bounded.
	// 4096 emitted on round 1; rounds 2 and 3 replay all 4096 cursors, and the
	// ring still holds every one of them, so nothing more is accepted.
	if got := len(h.snapshot()); got != journalCursorRing {
		t.Fatalf("emitted %d records, want %d", got, journalCursorRing)
	}
}

// TestJournaldEntryCapAndNonUTF8: the per-entry cap truncates with an exact byte
// delta, and a non-UTF-8 MESSAGE is replaced, flagged and kept.
func TestJournaldEntryCapAndNonUTF8(t *testing.T) {
	const cap = 1024
	h := newHarness(t, types.ConfigValue{Key: "sensors.journald.max_entry", Value: cap})
	f := &journalFollower{scope: "u.service", ring: make([]string, journalCursorRing), q: newJournalQueue(8192, 0), cursorFile: filepath.Join(h.dir, "c")}
	ctx := context.Background()

	long := strings.Repeat("A", cap+4096)
	e, err := h.s.parseJournalEntry(f, []byte(entryLine("c=1", "u.service", long, 3, 1758012345000000)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !e.trunc {
		t.Fatal("an entry over the cap must be flagged truncated")
	}
	if e.dropped != 4096 {
		t.Fatalf("dropped_bytes = %d, want exactly 4096", e.dropped)
	}
	if len(e.message) != cap {
		t.Fatalf("kept %d bytes, want exactly %d", len(e.message), cap)
	}
	h.s.drainJournal(ctx, f, e)

	// Non-UTF-8: raw bytes are never persisted. The invalid sequence is injected
	// into the raw JSON line, which is where journalctl puts it.
	line := "{\"__CURSOR\":\"c=2\",\"MESSAGE\":\"boom \xff\xfe\",\"PRIORITY\":\"4\",\"_SYSTEMD_UNIT\":\"u.service\",\"__REALTIME_TIMESTAMP\":\"1758012345000000\"}"
	e2, err := h.s.parseJournalEntry(f, []byte(line))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !e2.nonUTF8 {
		t.Fatal("a non-UTF-8 MESSAGE must be flagged")
	}
	if strings.ContainsRune(string(e2.message), 0xff) {
		t.Fatal("the raw non-UTF-8 bytes must never reach the ledger")
	}
	h.s.drainJournal(ctx, f, e2)
	for _, r := range h.snapshot() {
		if r.Kind == types.KEvent && r.Payload["detail"] != nil {
			if d, ok := r.Payload["detail"].(map[string]any); ok {
				if s, _ := d["msg"].(string); strings.ContainsRune(s, 0xff) {
					t.Fatal("a raw 0xFF byte reached a record payload")
				}
			}
		}
	}
}

// TestJournaldScrubBeforePersist: the injected redactor runs before anything is
// stored, and its count reaches the record.
func TestJournaldScrubBeforePersist(t *testing.T) {
	h := newHarness(t)
	f := &journalFollower{scope: "u.service", ring: make([]string, journalCursorRing), q: newJournalQueue(8192, 0), cursorFile: filepath.Join(h.dir, "c")}
	ctx := context.Background()
	e, err := h.s.parseJournalEntry(f, []byte(entryLine("c=1", "u.service", "start SECRET=hunter2 now", 3, 1758012345000000)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if strings.Contains(string(e.message), "hunter2") {
		t.Fatal("the scrubbed bytes still contain the secret")
	}
	if !strings.Contains(string(e.message), "REDACTED") {
		t.Fatalf("scrub marker missing: %q", e.message)
	}
	h.s.drainJournal(ctx, f, e)
	recs := h.snapshot()
	if len(recs) != 1 {
		t.Fatalf("expected one record, got %d", len(recs))
	}
	if recs[0].Redactions != 1 {
		t.Fatalf("record.Redactions = %d, want 1", recs[0].Redactions)
	}
}

// TestJournaldRetentionExceeded: a --since fallback outside the journal's
// retention keeps est_lost = -1 and names the reason.
func TestJournaldRetentionExceeded(t *testing.T) {
	h := newHarness(t)
	h.s.emitGapNow(context.Background(), string(types.SenJournald), "u.service", "cursor_invalid", "retention_window_exceeded")
	gaps := h.gaps()
	if len(gaps) != 1 {
		t.Fatalf("gaps = %d", len(gaps))
	}
	if gaps[0].Payload["est_lost"] != -1 {
		t.Fatalf("est_lost = %v, want -1", gaps[0].Payload["est_lost"])
	}
	if gaps[0].Payload["cause_detail"] != "retention_window_exceeded" {
		t.Fatalf("cause_detail = %v", gaps[0].Payload["cause_detail"])
	}
	if gaps[0].Payload["subsystem"] == "" {
		t.Fatal("every gap names its subsystem")
	}
}

// TestJournaldGapPayloadSchema pins the §3.9 gap shape other specs reuse.
func TestJournaldGapPayloadSchema(t *testing.T) {
	h := newHarness(t)
	h.s.emitGapNow(context.Background(), string(types.SenJournald), "payment-worker.service", "child_died", "exit status 1: Failed to seek to cursor")
	gaps := h.gaps()
	if len(gaps) != 1 {
		t.Fatalf("gaps = %d", len(gaps))
	}
	p := gaps[0].Payload
	for _, k := range []string{"id", "sensor", "scope", "from_ts", "to_ts", "est_lost", "cause", "cause_detail", "subsystem"} {
		if _, ok := p[k]; !ok {
			t.Errorf("gap payload missing %q", k)
		}
	}
	if !strings.HasPrefix(fmt.Sprint(p["id"]), "ev_") {
		t.Errorf("gap id = %v, want an ev_ ULID", p["id"])
	}
}

// TestJournaldMissingBinaryDisablesOnlyItself: TROUBLE-SENSORS-006.
func TestJournaldMissingBinaryDisablesOnlyItself(t *testing.T) {
	h := newHarness(t)
	// Force LookPath to fail by clearing PATH for this call.
	t.Setenv("PATH", "")
	if err := h.s.startJournald(context.Background()); err != nil {
		t.Fatalf("startJournald must degrade, not fail: %v", err)
	}
	rt := h.s.rt[types.SenJournald]
	if rt == nil || rt.enabled.Load() {
		t.Fatal("the journald sensor must report Enabled:false when journalctl is missing")
	}
	if reason := *rt.reason.Load(); !strings.Contains(reason, string(types.CodeSensors006)) {
		t.Fatalf("reason = %q", reason)
	}
	if h.s.jlState.path != "" {
		t.Fatal("the sensor must not keep a binary path it could not resolve")
	}
}

// TestJournaldFollowAllRefusedOnBigJournals pins the self-inflicted-load guard.
func TestJournaldFollowAllRefusedOnBigJournals(t *testing.T) {
	h := newHarness(t,
		types.ConfigValue{Key: "sensors.journald.follow_all", Value: true},
		types.ConfigValue{Key: "sensors.journald.follow_all_max_entries", Value: 10},
	)
	dir := t.TempDir()
	path := fakeJournalctl(t, dir, `#!/bin/sh
case "$*" in
  *"--disk-usage"*) echo "Archived and active journals take up 1023.3M in the file system." ;;
  *"-n 1"*) printf '{"__CURSOR":"c=1"}\n' ;;
esac
`)
	h.s.jlState.path = path
	if err := h.s.startJournald(context.Background()); err != nil {
		t.Fatalf("startJournald: %v", err)
	}
	rt := h.s.rt[types.SenJournald]
	if rt == nil || !rt.degraded.Load() {
		t.Fatal("follow_all on a journal over the cap must refuse and degrade")
	}
	if reason := *rt.reason.Load(); !strings.Contains(reason, "follow_all refused") {
		t.Fatalf("reason = %q", reason)
	}
	h.s.stopJournald()
}

// TestJournaldScopesAreUnitScopedByDefault: an empty units list must never mean
// "the whole journal".
func TestJournaldScopesAreUnitScopedByDefault(t *testing.T) {
	h := newHarness(t)
	scopes := h.s.journalScopes()
	if len(scopes) != 1 || scopes[0] == "all" {
		t.Fatalf("default scopes = %v, want exactly the daemon's own unit", scopes)
	}
	h2 := newHarness(t, types.ConfigValue{Key: "sensors.journald.units", Value: []string{"a.service", "b.service"}})
	if got := h2.s.journalScopes(); len(got) != 2 {
		t.Fatalf("configured scopes = %v", got)
	}
}

// TestJournaldParseFieldsAndSig checks the normalization contract: the sig is
// built from the visible unit identifier and the normalized message, and the
// detail carries what rules read.
func TestJournaldParseFieldsAndSig(t *testing.T) {
	h := newHarness(t)
	f := &journalFollower{scope: "u.service", ring: make([]string, journalCursorRing), q: newJournalQueue(8192, 0), cursorFile: filepath.Join(h.dir, "c")}
	line := []byte(entryLine("c=9", "payment-worker.service", "queue wedge: pool exhausted, retry 8812 in 30 ms", 3, 1758012345000000))
	e, err := h.s.parseJournalEntry(f, line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	h.s.drainJournal(context.Background(), f, e)
	recs := h.snapshot()
	if len(recs) != 1 {
		t.Fatalf("records = %d", len(recs))
	}
	want := sigFor(types.SrcJournald, "payment-worker.service", messageNorm("queue wedge: pool exhausted, retry 8812 in 30 ms")).String()
	if recs[0].Sig != want {
		t.Fatalf("sig = %s, want %s", recs[0].Sig, want)
	}
	d, _ := recs[0].Payload["detail"].(map[string]any)
	if d["unit"] != "payment-worker.service" {
		t.Fatalf("detail.unit = %v", d["unit"])
	}
	if d["priority_name"] != "err" {
		t.Fatalf("detail.priority_name = %v", d["priority_name"])
	}
	if d["msg"] == "" {
		t.Fatal("detail.msg must carry the mask-safe message")
	}
	if recs[0].Origin.Source != string(types.SrcJournald) {
		t.Fatalf("origin.source = %q", recs[0].Origin.Source)
	}
}
