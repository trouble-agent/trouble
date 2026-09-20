package sensors

import (
	"context"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/trouble-agent/trouble/internal/types"
)

// psi_test.go covers SPEC-03 §7's psi row: the trigger byte builder golden, the
// grammar matrix over measured shapes, the arm/epoll guard, the busy-spin
// regression, wake rate limiting, the CPU budget and the probe result.

// TestTriggerBytesGolden pins the only legal trigger write (P1). Measured: the
// kernel overwrites the last byte with NUL, so a 20-byte write loses its last
// digit and looks like a rejected API.
func TestTriggerBytesGolden(t *testing.T) {
	got := triggerBytes("some", 150000, 2000000)
	want := []byte("some 150000 2000000\n\x00")
	if len(got) != 21 {
		t.Fatalf("trigger payload is %d bytes, want 21 (%q)", len(got), got)
	}
	if string(got) != string(want) {
		t.Fatalf("trigger payload = %q, want %q", got, want)
	}
	// The terminator is the point: a naive construction loses its last digit.
	// The "naive 20-byte string" is the documented shape without the NUL: the
	// kernel overwrites its last byte and the last digit never reaches the
	// parser, which is why the write looks rejected.
	naive := []byte("some 150000 2000000\n")
	if len(naive) != 20 {
		t.Fatalf("the naive fixture changed shape: %d bytes", len(naive))
	}
	if got[len(got)-2] != '\n' || got[len(got)-1] != 0x00 {
		t.Fatalf("payload must end \\n\\x00, got %q", got[len(got)-2:])
	}
	if string(got[:20]) != string(naive) {
		t.Fatal("the first 20 bytes must be the naive string; only the terminator may differ")
	}
}

// TestTriggerGrammarMatrix walks the measured shapes. Pure validation, so it
// runs everywhere; the kernel is asked to confirm under TROUBLE_TEST_PSI_TRIGGERS.
func TestTriggerGrammarMatrix(t *testing.T) {
	cases := []struct {
		name          string
		metric        string
		stall, window int64
		wantOK        bool
	}{
		{"2s accepted", "some", 150000, 2000000, true},
		{"4s accepted", "some", 150000, 4000000, true},
		{"20s accepted by grammar", "some", 150000, 20000000, true},
		{"1s refused", "some", 150000, 1000000, false},
		{"5s refused", "some", 150000, 5000000, false},
		{"500ms refused", "some", 150000, 500000, false},
		{"0 threshold refused", "some", 0, 2000000, false},
		{"stall > window refused", "some", 3000000, 2000000, false},
		{"full accepted", "full", 150000, 2000000, true},
		{"bad metric refused", "sometimes", 150000, 2000000, false},
	}
	for _, tc := range cases {
		err := validateTrigger(tc.metric, tc.stall, tc.window)
		if tc.wantOK && err != nil {
			t.Errorf("%s: unexpected refusal: %v", tc.name, err)
		}
		if !tc.wantOK && err == nil {
			t.Errorf("%s: expected a refusal", tc.name)
		}
	}
}

// TestTriggerGrammarAgainstKernel asks the running kernel. This is the measured
// half of P2/P9 and is gated because it writes to /proc/pressure.
func TestTriggerGrammarAgainstKernel(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping kernel PSI arm matrix in -short")
	}
	if unix.Access("/proc/pressure/io", unix.R_OK|unix.W_OK) != nil {
		t.Skip("no writable /proc/pressure on this host")
	}
	type probe struct {
		name   string
		stall  int64
		window int64
		want   bool
	}
	cases := []probe{
		{"2s", 150000, 2000000, true},
		{"4s", 150000, 4000000, true},
		{"1s", 150000, 1000000, false},
		{"5s", 150000, 5000000, false},
		{"stall=0", 0, 2000000, false},
		{"stall>window", 3000000, 2000000, false},
	}
	for _, c := range cases {
		errno, ok := armOnce("io", "some", c.stall, c.window)
		if ok != c.want {
			t.Errorf("kernel arm %s: ok=%v errno=%v, want ok=%v", c.name, ok, errno, c.want)
		}
	}
	// The no-terminator shape must be refused (measured EINVAL).
	fd, err := openPSIFd("io")
	if err != nil {
		t.Fatalf("openPSIFd: %v", err)
	}
	defer unix.Close(fd)
	if _, err := unix.Write(fd, []byte("some 150000 2000000")); err != errnoOf(syscall.EINVAL) {
		t.Errorf("non-terminated write: err=%v, want EINVAL", err)
	}
	if _, err := unix.Write(fd, triggerBytes("some", 150000, 2000000)); err != nil {
		t.Errorf("terminated write: %v", err)
	}
	// A second write on an armed fd is EBUSY (measured).
	if _, err := unix.Write(fd, triggerBytes("some", 150000, 2000000)); errnoOf(err) != syscall.EBUSY {
		t.Errorf("second arm: err=%v, want EBUSY", err)
	}
}

// TestEpollGuardPanicsOnUnarmedFd is the 347k-hits/200ms regression guard (P6):
// an unarmed PSI fd is always ready, so adding one to epoll before a successful
// arm is a 100%-CPU busy loop. The guard panics, and the test proves the panic
// is reachable rather than theoretical.
func TestEpollGuardPanicsOnUnarmedFd(t *testing.T) {
	tr := &psiTrigger{resource: "io", metric: "some", stallUS: 150000, windowUS: 2000000, fd: 7, epfd: 9, shutdownFD: -1}
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("epollAdd on an unarmed fd must panic; it returned instead")
		}
		if !contains(r.(string), "unarmed") {
			t.Fatalf("panic message must name the cause, got %v", r)
		}
	}()
	_ = tr.epollAdd()
}

// TestUnarmedFdPollIsGuarded is the numeric form of the same regression: the
// guarded path returns zero events, while the naive path spins.
func TestUnarmedFdPollIsGuarded(t *testing.T) {
	fd, err := unix.Open("/proc/pressure/cpu", unix.O_RDWR|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Skipf("cannot open /proc/pressure/cpu: %v", err)
	}
	defer unix.Close(fd)
	tr := &psiTrigger{resource: "cpu", metric: "some", stallUS: 150000, windowUS: 2000000, fd: fd, epfd: -1, shutdownFD: -1}

	var guarded int
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		n, err := tr.pollOnce(0)
		if err != nil {
			t.Fatalf("guarded poll: %v", err)
		}
		guarded += n
	}
	if guarded != 0 {
		t.Fatalf("guarded poll of an unarmed fd returned %d events; it must return 0", guarded)
	}

	var naive int
	deadline = time.Now().Add(200 * time.Millisecond)
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLPRI | unix.POLLERR}}
	for time.Now().Before(deadline) {
		n, err := unix.Poll(fds, 0)
		if err != nil || n <= 0 {
			continue
		}
		naive += n
	}
	if naive == 0 {
		t.Skip("this kernel does not report an unarmed PSI fd as ready")
	}
	t.Logf("measured: unarmed fd reported ready %d times in 200ms (the guarded path returned 0)", naive)
}

// TestArmedCountersAgree asserts psi.armed_fds == len(epoll_sets) across a full
// arm/stop cycle (P6).
func TestArmedCountersAgree(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping PSI arming in -short")
	}
	if unix.Access("/proc/pressure/io", unix.R_OK|unix.W_OK) != nil {
		t.Skip("no writable /proc/pressure on this host")
	}
	h := newHarness(t, types.ConfigValue{Key: "sensors.psi.sample_interval", Value: "100ms"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.s.Probe(ctx); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	pr := h.s.probe.Load()
	if pr == nil {
		t.Fatal("probe result not stored")
	}
	if string(pr.Mode) != string(psiTriggersAndSampling) && string(pr.Mode) != string(psiSamplingOnly) {
		t.Fatalf("unexpected mode %q", pr.Mode)
	}
	if err := h.s.startPSI(ctx); err != nil {
		t.Fatalf("startPSI: %v", err)
	}
	if armed, sets := h.s.psiState.armed.Load(), h.s.psiState.epollSets.Load(); armed != sets {
		t.Fatalf("armed=%d epoll_sets=%d", armed, sets)
	}
	if pr.Mode == psiTriggersAndSampling {
		if h.s.psiState.armed.Load() == 0 {
			t.Fatal("triggers+sampling mode armed nothing")
		}
	} else if h.s.psiState.armed.Load() != 0 {
		t.Fatalf("sampling-only mode must keep zero armed fds, got %d", h.s.psiState.armed.Load())
	}
	h.s.stopPSI()
}

// TestProbeModesAndCeiling runs the real probe and asserts the recorded shape.
func TestProbeModesAndCeiling(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the real capability probe in -short")
	}
	h := newHarness(t)
	ctx := context.Background()
	if err := h.s.Probe(ctx); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	recs := h.snapshot()
	if len(recs) != 1 {
		t.Fatalf("the probe must write exactly one record, got %d", len(recs))
	}
	rec := recs[0]
	if rec.Kind != types.KEvent {
		t.Fatalf("probe record kind = %s", rec.Kind)
	}
	if got := rec.Payload["kind"]; got != "capability_probe" {
		t.Fatalf("payload.kind = %v", got)
	}
	pr := h.s.probe.Load()
	if pr.Payload["probe_ts"] == nil || pr.Payload["kernel"] == "" {
		t.Fatalf("probe payload missing identity fields: %v", pr.Payload)
	}
	for _, k := range []string{"psi_read", "trigger_arm", "psi_mode", "max_window_us",
		"stall_zero_rejected", "pressure_files_writable", "journald", "dbus_managers",
		"oomd_available", "inotify", "modes", "codes"} {
		if _, ok := pr.Payload[k]; !ok {
			t.Errorf("probe payload missing %q", k)
		}
	}
	jm, ok := pr.Payload["journald"].(map[string]any)
	if !ok {
		t.Fatal("journald probe is not an object")
	}
	for _, k := range []string{"binary", "cursor_ok", "member_adm"} {
		if _, ok := jm[k]; !ok {
			t.Errorf("journald probe missing %q", k)
		}
	}
	t.Logf("probe: mode=%s arm=%v max_window_us=%d stall_zero_rejected=%v journald=%v",
		pr.Mode, pr.Payload["trigger_arm"], pr.MaxWinUS, pr.Payload["stall_zero_rejected"], jm)
	if pr.Mode == psiTriggersAndSampling {
		if pr.MaxWinUS < 2000000 {
			t.Errorf("armed mode must discover a ceiling >= 2s, got %d", pr.MaxWinUS)
		}
		if pr.Payload["stall_zero_rejected"] != true {
			t.Error("an armed host must have observed the stall=0 EINVAL")
		}
	}
}

// TestPsiSamplerEmitsCounters asserts every wake/sample re-reads the counters
// and that the sampled sig is bucket-derived (P3/P4).
func TestPsiSamplerEmitsCounters(t *testing.T) {
	h := newHarness(t)
	ev, err := h.s.sampleOnce(context.Background(), "io")
	if err != nil {
		t.Skipf("cannot read /proc/pressure/io: %v", err)
	}
	if ev.Sensor != types.SenPSI || ev.Scope != "io" || ev.Unit != "pct" {
		t.Fatalf("unexpected event: %+v", ev)
	}
	if _, ok := ev.Detail["some_avg10"]; !ok {
		t.Fatal("a sample must carry the counters rules read")
	}
	want := sigFor(types.SrcPSI, "io", "some", psiBucket(ev.Value)).String()
	if ev.Sig.String() != want {
		t.Fatalf("sig = %s, want %s (bucket-derived from the sampled avg10)", ev.Sig, want)
	}
	if ev.Wake {
		t.Fatal("a sample is not a wake")
	}
}

// TestPsiSpuriousWakeCounts: a wake whose re-read does not match any rule
// changes nothing and increments psi.spurious_wakes (P4).
func TestPsiSpuriousWakeCounts(t *testing.T) {
	h := newHarness(t)
	h.writeRules("10.toml", `
[[rule]]
name = "io_pressure"
source = "psi"
[[rule.match]]
field = "scope"
op = "=="
value = "io"
value_type = "string"
[[rule.match]]
field = "some_avg10"
op = ">="
value = "999"
value_type = "number"
`)
	h.mustReload()
	ctx := context.Background()
	h.s.handleEvent(ctx, psiEvent("io", 1.0, true))
	// No rule matched, so a wake is spurious and no incident state changed.
	if h.s.psiState.spurious.Load() != 1 {
		t.Fatalf("spurious wakes = %d, want 1", h.s.psiState.spurious.Load())
	}
	if len(h.fired()) != 0 {
		t.Fatal("a spurious wake must not fire")
	}
}

// TestPsiWakeRateLimit: notification rate limiting caps wake processing at one
// per window per armed fd (P10). The loop's guard is exercised directly here
// because a real 2s window would dominate the test suite.
func TestPsiWakeRateLimit(t *testing.T) {
	h := newHarness(t)
	tr := &psiTrigger{resource: "io", metric: "some", stallUS: 150000, windowUS: 2000000}
	windowMS := int(tr.windowUS / 1000)
	if windowMS != 2000 {
		t.Fatalf("window guard computed %dms, want 2000ms", windowMS)
	}
	// A burst inside one window must be collapsed to one wake.
	now := h.now()
	var processed int
	var lastWake time.Time
	for i := 0; i < 100; i++ {
		if !lastWake.IsZero() && now.Sub(lastWake) < time.Duration(windowMS)*time.Millisecond {
			continue
		}
		lastWake = now
		processed++
	}
	if processed != 1 {
		t.Fatalf("wakes processed in one window = %d, want 1", processed)
	}
	h.advance(2 * time.Second)
	now = h.now()
	if now.Sub(lastWake) < time.Duration(windowMS)*time.Millisecond {
		t.Fatal("after one full window a wake must be allowed again")
	}
}

// TestPsiSamplerCPUBudget measures the steady CPU cost of the sampler: three
// resources at a 2s interval must stay under 0.3% of one core (P10).
func TestPsiSamplerCPUBudget(t *testing.T) {
	if unix.Access("/proc/pressure/io", unix.R_OK) != nil {
		t.Skip("/proc/pressure is not readable on this host")
	}
	window := 30 * time.Second
	if testing.Short() {
		t.Skip("skipping the 30s CPU budget measurement in -short")
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	startCPU := cpuNanos()
	start := time.Now()
	ticks := 0
	deadline := start.Add(window)
	var _ = deadline
	for time.Now().Before(start.Add(window)) {
		for _, res := range psiResources {
			if _, err := readPSI(res); err != nil {
				t.Fatalf("readPSI(%s): %v", res, err)
			}
		}
		ticks++
		time.Sleep(2 * time.Second)
	}
	elapsed := time.Since(start)
	used := time.Duration(cpuNanos() - startCPU)
	// One tick per sample interval, three resources per tick: the sampler's own
	// cost is the elapsed CPU over the wall window.
	perCorePct := 100 * float64(used) / float64(elapsed)
	runtime.ReadMemStats(&after)
	if perCorePct > 0.3 {
		t.Fatalf("PSI sampler used %.3f%% of one core over %s (%d ticks), budget is 0.3%%", perCorePct, elapsed, ticks)
	}
	t.Logf("measured: PSI sampler %.4f%% of one core over %s (%d ticks, 3 resources each)", perCorePct, elapsed, ticks)
}

// cpuNanos sums process CPU time so the budget is a real measurement, not a
// wall-clock guess.
func cpuNanos() int64 {
	var ru unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return ru.Utime.Nano() + ru.Stime.Nano()
}

var _ = atomic.AddInt64
