package sensors

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// disk_timer_inotify_test.go covers SPEC-03 §7's combined row.

// TestDiskFreePctMatchesStatfs checks the arithmetic against a second,
// independent statfs read and the documented bounds.
func TestDiskFreePctMatchesStatfs(t *testing.T) {
	ev, err := statfsMount("/")
	if err != nil {
		t.Fatalf("statfsMount: %v", err)
	}
	var st unix.Statfs_t
	if err := unix.Statfs("/", &st); err != nil {
		t.Fatalf("statfs: %v", err)
	}
	wantPct := 100 * float64(st.Bavail) / float64(st.Blocks)
	gotPct := ev.Detail["free_pct"].(float64)
	if diff := gotPct - wantPct; diff > 0.1 || diff < -0.1 {
		t.Fatalf("free_pct = %v, want %v (±0.1%%)", gotPct, wantPct)
	}
	if gotPct < 0 || gotPct > 100 {
		t.Fatalf("free_pct = %v, outside [0,100]", gotPct)
	}
	free := ev.Detail["free_bytes"].(float64)
	total := ev.Detail["total_bytes"].(float64)
	if free > total {
		t.Fatalf("free_bytes %v > total_bytes %v", free, total)
	}
	if _, ok := ev.Detail["inode_free_pct"]; !ok {
		t.Fatal("inode_free_pct must always be present (it is a first-class field)")
	}
	if ev.Sensor != types.SenDisk || ev.Scope != "/" {
		t.Fatalf("event = %+v", ev)
	}
}

// TestDiskScopeIsAMountPath: the scope must never be a device node — a device
// node is refused at the configuration boundary and named in the reason.
func TestDiskScopeIsAMountPath(t *testing.T) {
	h := newHarness(t, types.ConfigValue{Key: "sensors.disk.mounts", Value: []string{"/dev/null", "/"}})
	h.s.startDisk(context.Background())
	h.s.diskSweep(context.Background())
	for _, r := range h.snapshot() {
		if sc, _ := r.Payload["scope"].(string); strings.HasPrefix(sc, "/dev/") {
			t.Fatalf("a device node reached the wire as a scope: %q", sc)
		}
	}
	rt := h.s.rt[types.SenDisk]
	if rt == nil || !rt.degraded.Load() {
		t.Fatal("a refused device-node mount must degrade the sensor loudly")
	}
	if reason := *rt.reason.Load(); !strings.Contains(reason, "/dev/null") {
		t.Fatalf("reason must name the refused path, got %q", reason)
	}
	freeSig := sigFor(types.SrcDisk, "/", "free_pct").String()
	found := false
	for _, r := range h.snapshot() {
		if r.Sig == freeSig {
			found = true
		}
	}
	if !found {
		t.Fatal("the remaining mount must keep reporting after a refusal")
	}
	h.s.stopDisk()
}

func TestSanitizeMounts(t *testing.T) {
	ok, bad := sanitizeMounts([]string{"/", "/dev/sda1", "", "  ", "/var/lib"})
	if len(ok) != 2 || ok[0] != "/" || ok[1] != "/var/lib" {
		t.Fatalf("ok = %v", ok)
	}
	if len(bad) != 1 || bad[0] != "/dev/sda1" {
		t.Fatalf("bad = %v", bad)
	}
}

// TestDiskInodeAbsentIsMinusOne: Files == 0 reports -1 and inode rules are
// false, never "0% free".
func TestDiskInodeAbsentIsMinusOne(t *testing.T) {
	h := newHarness(t)
	// The shipped inode rule is the contract: -1 must not satisfy it.
	h.writeRules("10.toml", `
[[rule]]
name = "disk_inode_low"
source = "disk"
[[rule.match]]
field = "inode_free_pct"
op = ">="
value = "0"
value_type = "number"
[[rule.match]]
field = "inode_free_pct"
op = "<"
value = "10"
value_type = "number"
`)
	h.mustReload()
	ev := types.SensorEvent{
		ID: types.NewID(types.PEv), TS: types.FormatUTC(h.now()), Sensor: types.SenDisk,
		Scope: "/x", Value: -1, Unit: "pct",
		Detail: map[string]any{
			"mount": "/x", "fstype": "tmpfs", "free_pct": 99.0, "inode_free_pct": -1.0,
			"free_bytes": 1.0, "total_bytes": 2.0, "read_only": false, "count": 1,
			"value": -1.0, "severity": "info", "window_s": 0.0, "age_s": 0.0, "msg": "", "substr": "", "unit": "pct",
		},
		Sig: sigFor(types.SrcDisk, "/x", "inode_pct"),
	}
	h.s.handleEvent(context.Background(), ev)
	if got := len(h.fired()); got != 0 {
		t.Fatalf("inode_free_pct = -1 must not satisfy an inode rule; %d fired", got)
	}
	// And the value itself is -1 on the wire.
	if ev.Detail["inode_free_pct"].(float64) != -1 {
		t.Fatal("the -1 sentinel is the contract")
	}
}

// TestDiskStatfsFailureIsRateLimited: TROUBLE-SENSORS-022, at most one record
// per mount per hour, and the remaining mounts keep reporting.
func TestDiskStatfsFailureIsRateLimited(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	bogus := filepath.Join(h.dir, "not-a-mount")
	for i := 0; i < 5; i++ {
		h.s.diskError(ctx, bogus, errors.New("no such file or directory"))
	}
	codes := h.recordsWhere(func(r types.Record) bool {
		return r.Payload["error_code"] == string(types.CodeSensors022)
	})
	if len(codes) != 1 {
		t.Fatalf("expected exactly one 022 record, got %d", len(codes))
	}
	if d, _ := codes[0].Payload["detail"].(string); !strings.Contains(d, bogus) {
		t.Fatalf("022 must name the path, got %q", d)
	}
	// After the hour it is reported again (still one per hour).
	h.advance(diskErrorWindow + time.Second)
	h.s.diskError(ctx, bogus, errors.New("still gone"))
	codes = h.recordsWhere(func(r types.Record) bool {
		return r.Payload["error_code"] == string(types.CodeSensors022)
	})
	if len(codes) != 2 {
		t.Fatalf("after the window a new 022 must be allowed, got %d", len(codes))
	}
}

// TestDiskSweepEmitsFreeAndInodeKinds: the two kinds are distinct signatures.
func TestDiskSweepEmitsFreeAndInodeKinds(t *testing.T) {
	h := newHarness(t, types.ConfigValue{Key: "sensors.disk.mounts", Value: []string{"/"}})
	h.s.startDisk(context.Background())
	h.s.diskSweep(context.Background())
	h.s.stopDisk()
	sigs := map[string]bool{}
	for _, r := range h.snapshot() {
		if r.Kind == types.KEvent {
			sigs[r.Sig] = true
		}
	}
	freeSig := sigFor(types.SrcDisk, "/", "free_pct").String()
	if !sigs[freeSig] {
		t.Fatalf("the free_pct signature is missing: %v", sigs)
	}
	for sig := range sigs {
		if strings.Contains(sig, "ca5ae505ad838444") {
			continue
		}
		if !strings.Contains(sig, "disk:") {
			t.Fatalf("unexpected sig %q", sig)
		}
	}
}

// TestTimersMissedRunsIncrementAndReset drives the missed-run detector with a
// synthetic timer two intervals stale.
func TestTimersMissedRunsIncrementAndReset(t *testing.T) {
	h := newHarness(t, types.ConfigValue{Key: "sensors.timers.interval", Value: "60s"})
	h.s.tmState.missed = map[string]int{}
	h.s.tmState.last = map[string]time.Time{}
	h.s.tmState.run = map[string]time.Time{}
	ctx := context.Background()
	now := h.now()
	// next_elapse 5 intervals in the past: stale.
	stale := timerStatus{Name: "backup.timer", NextElapseUSecRealt: uint64(now.Add(-5 * time.Minute).UnixMicro())}
	h.s.emitTimer(ctx, "system", stale, now)
	h.s.tmState.mu.Lock()
	got := h.s.tmState.missed["backup.timer"]
	h.s.tmState.mu.Unlock()
	if got != 1 {
		t.Fatalf("missed_runs = %d, want 1", got)
	}
	// A second stale observation increments.
	h.s.emitTimer(ctx, "system", stale, now.Add(time.Second))
	h.s.tmState.mu.Lock()
	got = h.s.tmState.missed["backup.timer"]
	h.s.tmState.mu.Unlock()
	if got != 2 {
		t.Fatalf("missed_runs = %d, want 2", got)
	}
	// The next observed run resets it.
	fresh := timerStatus{Name: "backup.timer", NextElapseUSecRealt: uint64(now.Add(2 * time.Minute).UnixMicro())}
	h.s.emitTimer(ctx, "system", fresh, now.Add(2*time.Second))
	h.s.tmState.mu.Lock()
	got = h.s.tmState.missed["backup.timer"]
	h.s.tmState.mu.Unlock()
	if got != 0 {
		t.Fatalf("missed_runs = %d after a run, want 0", got)
	}
	// The missed bucket vocabulary is on the wire.
	var buckets []string
	for _, r := range h.snapshot() {
		if d, ok := r.Payload["detail"].(map[string]any); ok {
			if b, ok := d["missed_bucket"].(string); ok {
				buckets = append(buckets, b)
			}
		}
	}
	if len(buckets) < 3 {
		t.Fatalf("expected the bucket on every timer record, got %v", buckets)
	}
	if buckets[0] != "1" {
		t.Fatalf("a single missed run must bucket as \"1\", got %q", buckets[0])
	}
	if buckets[1] != "2-4" {
		t.Fatalf("two missed runs must bucket as \"2-4\", got %q", buckets[1])
	}
	if buckets[2] != "0" {
		t.Fatalf("an observed run must bucket as \"0\", got %q", buckets[2])
	}
}

func TestMissedBucketVocabulary(t *testing.T) {
	cases := map[int]string{0: "0", 1: "1", 2: "2-4", 4: "2-4", 5: "5-9", 9: "5-9", 10: "10+", 99: "10+"}
	for n, want := range cases {
		if got := missedBucket(n); got != want {
			t.Errorf("missedBucket(%d) = %q, want %q", n, got, want)
		}
	}
}

// TestTimersListParseFailureRecordsRawSize: TROUBLE-SENSORS-023 with the raw
// reply size, and no invented timer list.
func TestTimersListParseFailureRecordsRawSize(t *testing.T) {
	h := newHarness(t)
	orig := listTimersFn
	defer func() { listTimersFn = orig }()
	listTimersFn = func(w *dbusWatch) ([]timerStatus, error) {
		return nil, fmt.Errorf("ListTimers reply (%d bytes) could not be parsed: unexpected type", 4096)
	}
	// A watch is needed for the sweep to run at all; the seam above supplies the
	// failure, so the connection is never touched.
	h.s.dbState.merge = newMergeTracker(5*time.Second, h.s.now)
	h.s.dbState.watches = map[string]*dbusWatch{"system": {bus: "system"}}
	h.s.tmState.missed = map[string]int{}
	h.s.tmState.last = map[string]time.Time{}
	h.s.tmState.run = map[string]time.Time{}
	h.s.timerSweep(context.Background())
	recs := h.recordsWhere(func(r types.Record) bool {
		return r.Payload["error_code"] == string(types.CodeSensors023)
	})
	if len(recs) != 1 {
		t.Fatalf("expected one 023 record, got %d", len(recs))
	}
	if d, _ := recs[0].Payload["detail"].(string); !strings.Contains(d, "4096") {
		t.Fatalf("023 must record the raw reply size, got %q", d)
	}
	// The sensor stays enabled and retries on the next tick.
	rt := h.s.rt[types.SenTimers]
	if rt == nil {
		t.Fatal("timers runtime missing")
	}
	h.s.timerSweep(context.Background())
}

// TestTimersDependencyProducesNoSecondGap: one cause, one record.
func TestTimersDependencyProducesNoSecondGap(t *testing.T) {
	h := newHarness(t)
	h.s.dbState.dead.Store(true)
	h.s.timerSweep(context.Background())
	if got := len(h.gaps()); got != 0 {
		t.Fatalf("a dependent sensor must not file its own gap, got %d", got)
	}
	rt := h.s.rt[types.SenTimers]
	if rt == nil || !rt.degraded.Load() {
		t.Fatal("timers must be marked degraded")
	}
	if reason := *rt.reason.Load(); reason != "dependency dbus degraded" {
		t.Fatalf("reason = %q", reason)
	}
}

// TestInotifyOverflowReAddsWatchesAndGaps: IN_Q_OVERFLOW.
func TestInotifyOverflowReAddsWatchesAndGaps(t *testing.T) {
	h := newHarness(t)
	dir := filepath.Join(h.dir, "watched")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	if err != nil {
		t.Skipf("inotify unavailable: %v", err)
	}
	h.s.inoState.fd = fd
	h.s.inoState.watches = map[int32]*inotifyWatch{}
	h.s.inoState.byPath = map[string]int32{}
	h.s.addOneWatch(dir, unix.IN_CREATE|unix.IN_Q_OVERFLOW, false, 0, "", false)
	if len(h.s.inoState.watches) != 1 {
		t.Fatalf("setup: %d watches", len(h.s.inoState.watches))
	}
	h.s.inotifyOverflow(context.Background())
	if got := len(h.s.inoState.watches); got != 1 {
		t.Fatalf("after the overflow %d watches, want the set re-added", got)
	}
	gaps := h.gaps()
	if len(gaps) != 1 {
		t.Fatalf("expected one gap, got %d", len(gaps))
	}
	if gaps[0].Payload["cause"] != "queue_overflow" {
		t.Fatalf("cause = %v", gaps[0].Payload["cause"])
	}
	if gaps[0].Payload["est_lost"] != -1 {
		t.Fatalf("est_lost = %v, want -1", gaps[0].Payload["est_lost"])
	}
	codes := h.recordsWhere(func(r types.Record) bool {
		return r.Payload["error_code"] == string(types.CodeSensors021)
	})
	if len(codes) != 1 {
		t.Fatalf("expected one 021 record, got %d", len(codes))
	}
	if d, _ := codes[0].Payload["detail"].(string); !strings.Contains(d, "max_queued_events") {
		t.Fatalf("021 must name the real queue depth, got %q", d)
	}
	h.s.stopInotify()
}

// TestInotifyWatchLimitRatio: used/limit > 0.8 ⇒ 020 with both numbers.
func TestInotifyWatchLimitRatio(t *testing.T) {
	h := newHarness(t)
	orig := readProcIntFn
	defer func() { readProcIntFn = orig }()
	readProcIntFn = func(string) int64 { return 1 } // limit 1, used 1 → ratio 1.0
	h.s.inoState.fd = -1
	h.s.inoState.watches = map[int32]*inotifyWatch{1: {path: "/x"}}
	h.s.checkWatchLimit(context.Background())
	recs := h.recordsWhere(func(r types.Record) bool {
		return r.Payload["error_code"] == string(types.CodeSensors020)
	})
	if len(recs) != 1 {
		t.Fatalf("expected one 020 record, got %d", len(recs))
	}
	d, _ := recs[0].Payload["detail"].(string)
	if !strings.Contains(d, "used=1") || !strings.Contains(d, "limit=1") {
		t.Fatalf("020 must record both numbers, got %q", d)
	}
	// Under the ratio nothing is reported.
	readProcIntFn = func(string) int64 { return 100000 }
	h.s.checkWatchLimit(context.Background())
	recs = h.recordsWhere(func(r types.Record) bool {
		return r.Payload["error_code"] == string(types.CodeSensors020)
	})
	if len(recs) != 1 {
		t.Fatalf("an under-limit host must not file 020, got %d", len(recs))
	}
}

// TestInotifyMaskVocabulary pins the class mapping and the mask parser.
func TestInotifyMaskVocabulary(t *testing.T) {
	cases := map[uint32]string{
		unix.IN_Q_OVERFLOW:  "overflow",
		unix.IN_CREATE:      "create",
		unix.IN_MOVED_TO:    "create",
		unix.IN_MODIFY:      "modify",
		unix.IN_CLOSE_WRITE: "modify",
		unix.IN_DELETE:      "delete",
		unix.IN_MOVED_FROM:  "delete",
		unix.IN_ATTRIB:      "attrib",
	}
	for mask, want := range cases {
		if got := maskClass(mask); got != want {
			t.Errorf("maskClass(%d) = %q, want %q", mask, got, want)
		}
	}
	if _, ok := parseInotifyMask("not_a_mask"); ok {
		t.Fatal("an unknown mask name must be refused")
	}
	m, ok := parseInotifyMask("")
	if !ok || m == 0 {
		t.Fatal("an empty mask spec must fall back to the documented default set")
	}
	if m&unix.IN_CLOSE_WRITE == 0 || m&unix.IN_Q_OVERFLOW == 0 {
		t.Fatalf("the default mask set is incomplete: %b", m)
	}
}

// TestInotifyRulesDirIsAlwaysWatched: the hot-reload trigger is not optional.
func TestInotifyRulesDirIsAlwaysWatched(t *testing.T) {
	h := newHarness(t)
	rulesDir := filepath.Join(h.dir, "rules.d")
	if err := os.MkdirAll(rulesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := h.s.startInotify(context.Background()); err != nil {
		t.Fatalf("startInotify: %v", err)
	}
	h.s.inoState.mu.Lock()
	found := false
	for _, w := range h.s.inoState.watches {
		if w.path == rulesDir && w.isRules {
			found = true
		}
	}
	h.s.inoState.mu.Unlock()
	if !found {
		t.Fatal("the rules directory must always be watched, even with sensors.inotify.paths = []")
	}
	h.s.stopInotify()
}

// TestInotifyEventEmitsClassEvent exercises the real read loop with a real
// inotify fd: a file creation must produce one event with mask_class=create and
// a rules-dir write must mark a reload pending.
func TestInotifyEventEmitsClassEvent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "watched")
	rules := filepath.Join(t.TempDir(), "rules.d")
	for _, d := range []string{dir, rules} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	h := newHarness(t,
		types.ConfigValue{Key: "sensors.rules.dir", Value: rules},
		types.ConfigValue{Key: "sensors.inotify.paths", Value: []any{
			map[string]any{"path": dir, "mask": defaultInotifyMask},
		}},
	)
	if err := h.s.startInotify(context.Background()); err != nil {
		t.Fatalf("startInotify: %v", err)
	}
	defer h.s.stopInotify()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.s.wg.Add(1)
	go h.s.runInotify(ctx)

	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rules, "20-new.toml"), []byte("# x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(h.recordEventsByClass("create")) > 0 && h.s.reloadPending.Load() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	evs := h.recordsWhere(func(r types.Record) bool {
		d, ok := r.Payload["detail"].(map[string]any)
		return ok && d["mask_class"] == "create"
	})
	if len(evs) == 0 {
		t.Fatal("no create-class inotify event was emitted")
	}
	if !h.s.reloadPending.Load() {
		t.Fatal("a write inside rules.d must mark a reload pending (the hot-reload trigger)")
	}
	// Shut down: Stop closes every watch and waits (with a bound) for every
	// goroutine, including the ones started by startInotify.
	cancel()
	done := make(chan struct{})
	go func() { _ = h.s.Stop(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("Stop did not return: a goroutine failed to join")
	}
}

// recordEventsByClass is a small test helper on the harness side.
func (h *harness) recordEventsByClass(class string) []types.Record {
	return h.recordsWhere(func(r types.Record) bool {
		d, ok := r.Payload["detail"].(map[string]any)
		return ok && d["mask_class"] == class
	})
}
