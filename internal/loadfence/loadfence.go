// Package loadfence provides explicit verdicts for host-measured test gates.
package loadfence

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	FenceLoadAvg    = 45.0
	EnvLoadOverride = "TROUBLE_HOST_LOAD_OVERRIDE"
)

// LoadAvg1 reads the one-minute host load average. A valid non-negative override
// exists so CI can falsify the fence without manufacturing host load.
func LoadAvg1() float64 {
	if v := os.Getenv(EnvLoadOverride); v != "" {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f >= 0 {
			return f
		}
	}
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0
	}
	f, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || f < 0 {
		return 0
	}
	return f
}

func OverrideActive() bool { return os.Getenv(EnvLoadOverride) != "" }

func PerCore(load float64, cores int) float64 {
	if load < 0 {
		load = 0
	}
	if cores < 1 {
		cores = 1
	}
	return load / float64(cores)
}

func Oversubscribed(load float64) bool { return load >= FenceLoadAvg }

func SkipText(gate, measured string, load float64) string {
	note := ""
	if OverrideActive() {
		note = " [load_avg forced by " + EnvLoadOverride + "]"
	}
	return fmt.Sprintf("%s SKIP: host oversubscribed — load_avg %.2f ≥ fence %.2f (%d cores = %.2f per core)%s: %s", gate, load, FenceLoadAvg, runtime.NumCPU(), PerCore(load, runtime.NumCPU()), note, measured)
}

func Miss(t testing.TB, gate, measured string, load float64) {
	t.Helper()
	if Oversubscribed(load) {
		t.Skipf("%s", SkipText(gate, measured, load))
		return
	}
	t.Errorf("%s", measured)
}

func MissFatal(t testing.TB, gate, measured string, load float64) {
	t.Helper()
	if Oversubscribed(load) {
		t.Skipf("%s", SkipText(gate, measured, load))
		return
	}
	t.Fatalf("%s", measured)
}

// HostGateLoadCeiling is the load_avg_1m above which a host-measured
// throughput/latency budget stops being a product verdict at all.
//
// It is a SECOND fence, deliberately lower than FenceLoadAvg (45). Tier 1 runs
// on a 16-core fleet host while sibling work can keep load_avg_1m at 12 or more;
// at that point these short wall-clock measurements grade scheduler contention,
// not the implementation. The two affected gates therefore SKIP at 12.0 and
// above, naming the observed load, required load and measured value. Below 12.0
// their calibrated assertions still run, so a real regression remains red.
//
// To measure above the ceiling on purpose, force the load with
// TROUBLE_HOST_LOAD_OVERRIDE (the fence is falsifiable, not a dead gate).
const HostGateLoadCeiling = 12.0

// MissHostGateFatal preserves a hard failure below the load ceiling while making
// the loaded-host outcome an explicit SKIP. Callers must pass their measurement
// in measured; the skip text is intentionally not a generic load-only message.
func MissHostGateFatal(t testing.TB, gate, measured string, load float64) {
	t.Helper()
	if AboveHostGate(load) {
		t.Skipf("%s", HostGateSkipText(gate, measured, load))
		return
	}
	t.Fatalf("%s", measured)
}

// AboveHostGate reports whether load is at or above HostGateLoadCeiling.
func AboveHostGate(load float64) bool { return load >= HostGateLoadCeiling }

// HostGateSkipText is the skip reason for a host-measured gate that cannot be
// graded on this host: it names the measurement, the observed load, the ceiling
// constant, and the override when one is active.
func HostGateSkipText(gate, measured string, load float64) string {
	note := ""
	if OverrideActive() {
		note = " [load_avg forced by " + EnvLoadOverride + "]"
	}
	return fmt.Sprintf("%s SKIP: host above loadfence.HostGateLoadCeiling — load_avg_1m %.2f ≥ required load %.2f: %s%s",
		gate, load, HostGateLoadCeiling, measured, note)
}

// HostGateBurstText is the skip reason for a measurement window a load burst
// contaminated: the opening sample was quiet and the peak sampled inside the
// window crossed HostGateLoadCeiling, so the graded numbers belong to the
// burst rather than to the product. It names the same shared ceiling
// constant, both loads (peak first — it is the decisive reading), the
// opening sample, and the override when one is active.
func HostGateBurstText(gate, measured string, peak, start float64) string {
	note := ""
	if OverrideActive() {
		note = " [load_avg forced by " + EnvLoadOverride + "]"
	}
	return fmt.Sprintf("%s SKIP: a host load burst crossed the measurement window — peak load_avg_1m %.2f ≥ required load %.2f (loadfence.HostGateLoadCeiling) on a window that opened quiet at load_avg_1m %.2f: %s%s",
		gate, peak, HostGateLoadCeiling, start, measured, note)
}

const (
	EnvCI            = "CI"
	EnvGitHubActions = "GITHUB_ACTIONS"
)

// LoadWindowPeriod is how often a LoadWindow samples load_avg_1m across a
// measured run. load_avg_1m is itself a one-minute average that moves slowly,
// so once a second resolves a burst's shape for the wall-clock and rate
// budgets the host gates grade, at a cost of one /proc/loadavg read.
const LoadWindowPeriod = time.Second

// LoadWindow samples load_avg_1m in the background across a test's whole
// measurement window.
//
// QA-TROUBLE-17-R4-R2: the start gate (SkipStart / the copied
// AboveHostGate(loadAtStart) prelude) answers only "was the host already loud
// when the run began?" and nothing else. A sibling's burst that begins AFTER
// that sample pushes load above HostGateLoadCeiling while the numbers are
// still being graded — the 2026-11-22 recurrence graded p99 133.5 ms against
// a 40 ms 2x-clamped budget and 67 rps against a 75% floor, at load 31.38 on
// an 8-core box, on gates whose opening sample was quiet. A LoadWindow
// re-samples across the whole window and records the peak, so the grading
// step can turn a contaminated window into a NAMED SKIP instead of a graded
// red.
type LoadWindow struct {
	startc  chan struct{} // closed exactly once, by Stop
	donec   chan struct{} // closed by the sampler goroutine on exit
	stopped sync.Once

	mu    sync.Mutex
	first float64
	peak  float64
}

// StartLoadWindow opens the window: it takes the opening sample immediately
// (the same reading the start prelude grades) and keeps re-sampling every
// LoadWindowPeriod until Stop. Callers defer Stop.
func StartLoadWindow() *LoadWindow {
	return WatchLoadWindow(LoadAvg1)
}

// WatchLoadWindow is the scripted seam behind StartLoadWindow: it samples
// exactly the source passed in. Tests use it to force a burst shape without
// manufacturing host load.
func WatchLoadWindow(sample func() float64) *LoadWindow {
	w := &LoadWindow{
		startc: make(chan struct{}),
		donec:  make(chan struct{}),
	}
	first := sample()
	w.mu.Lock()
	w.first, w.peak = first, first
	w.mu.Unlock()
	go func() {
		defer close(w.donec)
		tick := time.NewTicker(LoadWindowPeriod)
		defer tick.Stop()
		for {
			select {
			case <-w.startc:
				return
			case <-tick.C:
				w.record(sample())
			}
		}
	}()
	return w
}

// Start is the window's opening sample — the reading the start gate grades
// (the per-test loadAtStart of the one-sample era).
func (w *LoadWindow) Start() float64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.first
}

// Peak is the highest sample over the window so far. The opening sample is
// included, so a burst that was already underway at window open appears here
// even if it never crossed the ceiling again.
func (w *LoadWindow) Peak() float64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.peak
}

// record folds one sample into the window's peak. Tests script bursts by
// calling it directly.
func (w *LoadWindow) record(l float64) {
	if l <= 0 {
		return // an unreadable /proc/loadavg must not inflate the peak
	}
	w.mu.Lock()
	if l > w.peak {
		w.peak = l
	}
	w.mu.Unlock()
}

// Stop ends the sampler and waits for it to exit, so no goroutine outlives
// the window. Idempotent.
func (w *LoadWindow) Stop() {
	w.stopped.Do(func() {
		close(w.startc)
		<-w.donec
	})
}

// SkipStart is the pre-run gate: skip when the window's opening sample is
// already at or above HostGateLoadCeiling.
func (w *LoadWindow) SkipStart(t testing.TB, gate, measured string) {
	t.Helper()
	start := w.Start()
	if !AboveHostGate(start) {
		return
	}
	t.Skipf("%s", HostGateSkipText(gate, measured, start))
}

// SkipBurst is the GRADING-time gate: skip when the window's peak crossed
// HostGateLoadCeiling — a burst inside the measurement window — so the graded
// numbers become a named SKIP instead of a red the host explains.
//
// It MUST be called after the measurement and BEFORE any t.Error*/t.Fatal*:
// a skip recorded after a failure does not un-fail the test, so the burst
// check has to fence every load-sensitive verdict, not trail it.
func (w *LoadWindow) SkipBurst(t testing.TB, gate, measured string) {
	t.Helper()
	start, peak := w.Start(), w.Peak()
	if !AboveHostGate(peak) {
		return
	}
	t.Skipf("%s", HostGateBurstText(gate, measured, peak, start))
}

// SkipUnderCIIfLoadCalibrated reports, for a load-calibrated performance gate,
// the shared-CI-runner SKIP verdict: on GitHub Actions (or any CI flagged via
// CI=) the runner's timing is not comparable to the quiet-host calibration the
// gate asserts, so the gate skips and the quiet host remains the binding
// verification. ref is the gate's quiet-host reference figure, recorded in the
// skip line so CI output still states the number the gate enforces at home.
// Away from CI the call is a no-op and the gate behaves exactly as before.
func SkipUnderCIIfLoadCalibrated(t testing.TB, ref string) {
	if os.Getenv(EnvCI) != "" || os.Getenv(EnvGitHubActions) == "true" {
		t.Skipf("load-calibrated perf gate: shared-runner timing not comparable to quiet-host calibration — quiet-host reference %s", ref)
	}
}
