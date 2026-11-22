package loadfence

// host_gate_test.go — QA-TROUBLE-17-R2: the host-measured ceiling that the
// sentinel throughput gate and the skills parse budget BOTH read.
//
// The two gates must not carry two copies of a threshold: the recurrence this
// row was filed for was two suites drifting apart in how they graded a loaded
// host (one FAILing on an unattainable floor, the other scaling past it). The
// predicate and the skip text are therefore pinned here, once.

import (
	"strings"
	"testing"
)

func TestHostGateCeiling(t *testing.T) {
	cases := []struct {
		load float64
		want bool
	}{
		{0, false},
		{3.9, false},
		{11.99, false},
		{12, true}, // the Tier 1 arm must never FAIL at the requested load boundary
		{15.99, true},
		{HostGateLoadCeiling - 0.01, false},
		{HostGateLoadCeiling, true}, // boundary: at the ceiling it is already unmeasurable
		{20.99, true},               // the measured 2026-10-23 failure point
		{FenceLoadAvg, true},
		{100, true},
	}
	for _, c := range cases {
		if got := AboveHostGate(c.load); got != c.want {
			t.Errorf("AboveHostGate(%.2f) = %t, want %t", c.load, got, c.want)
		}
	}
	// The host gate must fire BEFORE the generic fence: it is the stricter of
	// the two, and a gate that only skipped at 45 would still FAIL at 20.99.
	if !(HostGateLoadCeiling < FenceLoadAvg) {
		t.Fatalf("HostGateLoadCeiling %.1f must be below FenceLoadAvg %.1f", HostGateLoadCeiling, FenceLoadAvg)
	}
}

func TestHostGateSkipTextNamesEverything(t *testing.T) {
	t.Setenv(EnvLoadOverride, "")
	for _, gate := range []string{
		"SPEC-04 §7 ingest throughput (group-commit floor)",
		"SPEC-06a artifact parse budget",
		"TestBudgetDashboardRSS",
		"TestFsyncCountIsStructural",
		"TestAmortizedThroughput",
	} {
		txt := HostGateSkipText(gate, "measured rate/latency/structural result", 20.99)
		for _, want := range []string{
			gate,
			"SKIP",
			"loadfence.HostGateLoadCeiling", // the shared constant, named in the message
			"required load",
			"20.99",
			"measured rate/latency/structural result", // the measurement itself
		} {
			if !strings.Contains(txt, want) {
				t.Errorf("skip text for %q does not name %q: %s", gate, want, txt)
			}
		}
	}
	txt := HostGateSkipText("SPEC-04 §7 ingest throughput", "120 req/s vs a 537 req/s floor", 20.99)
	for _, want := range []string{
		"SPEC-04 §7 ingest throughput",
		"SKIP",
		"loadfence.HostGateLoadCeiling", // the shared constant, named in the message
		"20.99",
		"120 req/s vs a 537 req/s floor", // the measurement itself
	} {
		if !strings.Contains(txt, want) {
			t.Errorf("skip text does not name %q: %s", want, txt)
		}
	}
	if strings.Contains(txt, EnvLoadOverride) {
		t.Errorf("skip text names the override when none is active: %s", txt)
	}

	// An active override is disclosed, so a CI run that forces the load cannot
	// be mistaken for a real host reading.
	t.Setenv(EnvLoadOverride, "20.99")
	txt = HostGateSkipText("gate", "measured", 20.99)
	if !strings.Contains(txt, EnvLoadOverride) {
		t.Errorf("skip text does not disclose the active override: %s", txt)
	}
}

// QA-TROUBLE-17-R4-R2: the window gate. The start gate answers only "was the
// host already loud when the run began"; a burst that begins after that
// sample contaminated the window in the reported recurrence (p99 133.5 ms vs
// a 40 ms 2x-clamped budget, 67 rps vs a 75% floor, at load 31.38). The
// scripted sampler below reproduces exactly that shape — quiet open, burst
// mid-window — without manufacturing host load.
func TestLoadWindowBurstSkipsNamed(t *testing.T) {
	t.Setenv(EnvLoadOverride, "")
	// Quiet open (3.9), burst to 31.38 mid-window, still loud at grading.
	seq := []float64{3.9, 4.1, 31.38, 20.99}
	i := -1
	w := WatchLoadWindow(func() float64 {
		i++
		return seq[i%len(seq)]
	})
	defer w.Stop()
	if w.Start() != 3.9 {
		t.Fatalf("Start = %v, want the opening sample 3.9", w.Start())
	}
	if w.Peak() != 3.9 {
		t.Fatalf("Peak = %v; want the opening sample 3.9 before the window ticks", w.Peak())
	}
	// Start gate: the window opened quiet, so the pre-run gate must NOT skip.
	w.SkipStart(t, "TestLoadWindowBurst", "start gate on a quiet-open window")
	// The sampler folds its samples through the same record path; fold the
	// scripted burst in through it, exactly as the ticker would.
	for _, l := range seq[1:] {
		w.record(l)
	}
	if got := w.Peak(); got != 31.38 {
		t.Fatalf("Peak = %v, want the burst peak 31.38", got)
	}
	// Grading gate: the peak crosses the ceiling while the window opened
	// quiet → a named skip, not a graded red.
	w.SkipBurst(t, "TestLoadWindowBurst",
		"p99 render 133.5 ms vs a 40 ms 2x-clamped budget and 67 rps vs a 75% floor")
}

// The GREEN arm of the same gate: a window whose peak stays below the ceiling
// grades normally — no skip, no error — and a burst exactly AT the ceiling
// also crosses it (the boundary is >=, as for AboveHostGate).
func TestLoadWindowQuietGrades(t *testing.T) {
	t.Setenv(EnvLoadOverride, "")
	fired := 0
	w := WatchLoadWindow(func() float64 {
		fired++
		return 3.9
	})
	defer w.Stop()
	w.record(11.99) // below the ceiling even after a mid-window bump
	w.Start()       // opening sample pinned below the ceiling
	// Start gate stays quiet; grading gate stays quiet.
	w.SkipStart(t, "TestLoadWindowQuiet", "start gate below the ceiling")
	peak := w.Peak()
	if peak != 11.99 {
		t.Fatalf("Peak = %v, want the recorded 11.99", peak)
	}
	w.SkipBurst(t, "TestLoadWindowQuiet", "graded normally at load_avg_1m %.2f")
	if t.Failed() {
		t.Fatalf("a quiet window must not skip or fail")
	}
	if fired < 1 {
		t.Fatalf("sampler never fired")
	}
}

func TestLoadWindowBurstTextNamesEverything(t *testing.T) {
	t.Setenv(EnvLoadOverride, "")
	txt := HostGateBurstText("TestBudgetDashboardRSS",
		"dashboard render/RSS/request-rate budgets are not falsifiable on a burst-contaminated window",
		31.38, 2.4)
	for _, want := range []string{
		"TestBudgetDashboardRSS",
		"SKIP",
		"loadfence.HostGateLoadCeiling", // the shared constant, named in the message
		"31.38",                         // the peak
		"2.4",                           // the quiet opening sample
		"dashboard render/RSS/request-rate budgets are not falsifiable on a burst-contaminated window",
	} {
		if !strings.Contains(txt, want) {
			t.Errorf("burst skip text does not name %q: %s", want, txt)
		}
	}
	if strings.Contains(txt, EnvLoadOverride) {
		t.Errorf("burst skip text names the override when none is active: %s", txt)
	}

	t.Setenv(EnvLoadOverride, "31.38")
	txt = HostGateBurstText("gate", "measured", 31.38, 31.38)
	if !strings.Contains(txt, EnvLoadOverride) {
		t.Errorf("burst skip text does not disclose the active override: %s", txt)
	}
}

func TestLoadWindowStopIsIdempotentAndTerminates(t *testing.T) {
	fired := 0
	w := WatchLoadWindow(func() float64 { fired++; return 1 })
	w.Stop()
	if fired != 1 { // opening sample only; no ticker fired within the run
		t.Fatalf("sampler fired %d times after Stop; want 1 (opening sample)", fired)
	}
	w.Stop() // idempotent: no double-close, no panic
}
