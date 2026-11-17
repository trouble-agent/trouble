package loadfence

import (
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestProfileScaleIsHostTruthful pins the calibration arithmetic the host-measured
// budgets compose. The property that matters is that the model NEVER tightens a
// reference-host budget: whatever the box measures, the scale is >= 1, so a box
// that would meet the spec numbers still meets them, and a box that does not gets
// a bar derived from what it actually measured.
func TestProfileScaleIsHostTruthful(t *testing.T) {
	// Every case names the core count of the host it models, so the arithmetic
	// the case pins is the arithmetic of that box and not of the machine
	// running the test (QA-TROUBLE-17: a fixture that leaves Cores zero now
	// means "this host", which would make the expectations below move with the
	// CI runner's vCPU count).
	cases := []struct {
		name        string
		io, cpu     float64
		load        float64
		cores       int
		wantScale   float64
		wantCPUScal float64
	}{
		{name: "reference host: no slack anywhere", io: 1, cpu: 1, load: 0, cores: 16, wantScale: 1, wantCPUScal: 1},
		{name: "slower disk, quiet box", io: 1.5, cpu: 1.5, load: 0, cores: 16, wantScale: 1.5, wantCPUScal: 1.5},
		{name: "load alone (8/16 cores of contention)", io: 1, cpu: 1, load: 8, cores: 16, wantScale: 1.5, wantCPUScal: 1.5},
		{name: "slower disk AND load multiply (the 09-18 inversion case)", io: 1.3, cpu: 1.3, load: 4, cores: 16, wantScale: 1.625, wantCPUScal: 1.625},
		{name: "a slower-than-reference reading can never tighten", io: 0.5, cpu: 0.5, load: 0, cores: 16, wantScale: 1, wantCPUScal: 1},
		// QA-TROUBLE-17: the 8-vCPU bunker's own line, `io x1.22 cpu x0.71
		// load 1.01/16 cores`. Divided by 16 that host was loosened 1.22x;
		// divided by its own 8 cores it is 1.43x, which is the loosening its
		// measured ~3x slowdown needed.
		{name: "8-vCPU host at load 3.45: contention divided by 8", io: 1, cpu: 1, load: 3.45, cores: 8, wantScale: 1.43125, wantCPUScal: 1.43125},
		{name: "4-vCPU host at load 3.45: a smaller box loosens further", io: 1, cpu: 1, load: 3.45, cores: 4, wantScale: 1.8625, wantCPUScal: 1.8625},
		{name: "8-vCPU host, quiet: no loosening", io: 1, cpu: 1, load: 0, cores: 8, wantScale: 1, wantCPUScal: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := Profile{IOMultiple: c.io, CPUMultiple: c.cpu, Load: c.load, Cores: c.cores}
			if got := p.Scale(); math.Abs(got-c.wantScale) > 1e-9 {
				t.Fatalf("Scale() = %v, want %v", got, c.wantScale)
			}
			if got := p.CPUScale(); math.Abs(got-c.wantCPUScal) > 1e-9 {
				t.Fatalf("CPUScale() = %v, want %v", got, c.wantCPUScal)
			}
			if p.Scale() < 1 || p.CPUScale() < 1 {
				t.Fatalf("profile %+v scales a budget below the reference numbers", p)
			}
		})
	}
	// The scale is monotone in every input: a slower box or a busier box is never
	// graded more strictly than a faster or quieter one, which is exactly the
	// inversion the 2026-09-18 pair measured.
	for _, io := range []float64{0.5, 1, 1.2, 2, 5} {
		for _, load := range []float64{0, 3.99, 10.78, 32} {
			lo := Profile{IOMultiple: io, Load: load, Cores: 8}.Scale()
			hi := Profile{IOMultiple: io * 1.5, Load: load, Cores: 8}.Scale()
			if hi < lo {
				t.Fatalf("scale fell from %v to %v as the measured I/O multiple rose", lo, hi)
			}
			busier := Profile{IOMultiple: io, Load: load + 4, Cores: 8}.Scale()
			if busier < lo {
				t.Fatalf("scale fell from %v to %v as load rose", lo, busier)
			}
			// And a smaller box is never graded more strictly than a bigger one
			// at the same load: per-core load is what deschedules the work.
			smaller := Profile{IOMultiple: io, Load: load, Cores: 4}.Scale()
			if smaller < lo {
				t.Fatalf("scale fell from %v to %v as the host got smaller (4 vs 8 cores, same load)", lo, smaller)
			}
		}
	}
}

// TestContentionDividesByHostCores is the QA-TROUBLE-17 regression: the
// contention term is load_avg_1m over the HOST's own cores, not over the
// reference host's 16. It fails against the old arithmetic by construction —
// an 8-core box at load 3.45 is 1.43x contended, and dividing by 16 gives
// 1.22x, which is the number the failing QA batteries printed while hosted CI
// stayed green at the same SHA.
func TestContentionDividesByHostCores(t *testing.T) {
	const load = 3.45
	p := Profile{Load: load, Cores: 8}
	want := 1 + load/8 // 1.43125
	if got := p.Contention(); math.Abs(got-want) > 1e-9 {
		t.Fatalf("Contention() on an 8-core host at load %v = %v, want %v (load/host cores)", load, got, want)
	}
	// Discrimination, stated explicitly: the reference-core denominator must
	// NOT be what this profile produced.
	byReferenceCores := 1 + load/float64(CalibReferenceCores) // 1.215625
	if math.Abs(p.Contention()-byReferenceCores) < 1e-9 {
		t.Fatalf("Contention() = %v, which is load/%d — the reference host's core count, not this host's 8 (QA-TROUBLE-17)", p.Contention(), CalibReferenceCores)
	}
	if got := p.Contention(); got < 1.43 || got > 1.44 {
		t.Fatalf("Contention() = %v, want ≈1.43x loosening for load %v on 8 cores", got, load)
	}
	// Both scales carry the corrected term.
	for name, got := range map[string]float64{
		"Scale()":    p.Scale(),
		"CPUScale()": p.CPUScale(),
	} {
		if math.Abs(got-want) > 1e-9 {
			t.Fatalf("%s on an 8-core host at load %v = %v, want %v", name, load, got, want)
		}
	}
	// The divisor is a per-host quantity, not a constant: the same load on
	// four cores is more per-core pressure, on thirty-two less.
	for _, c := range []struct {
		cores int
		want  float64
	}{{2, 1 + load/2}, {4, 1 + load/4}, {8, 1 + load/8}, {16, 1 + load/16}, {32, 1 + load/32}, {64, 1 + load/64}} {
		got := Profile{Load: load, Cores: c.cores}.Contention()
		if math.Abs(got-c.want) > 1e-9 {
			t.Fatalf("Contention() with %d cores = %v, want %v", c.cores, got, c.want)
		}
	}
}

// TestProfileCoreCountDefaultsToHostCores pins the injectable default: a
// profile that did not record a core count means THIS host, so a caller that
// only knows load_avg still divides by the box it is running on, and a profile
// that did record one keeps it.
func TestProfileCoreCountDefaultsToHostCores(t *testing.T) {
	host := runtime.NumCPU()
	if host < 1 {
		t.Fatalf("runtime.NumCPU() = %d, want >= 1", host)
	}
	p := Profile{Load: 3.45}
	if got := p.CoreCount(); got != host {
		t.Fatalf("CoreCount() with no recorded cores = %d, want the host's %d", got, host)
	}
	if got, want := p.Contention(), 1+3.45/float64(host); math.Abs(got-want) > 1e-9 {
		t.Fatalf("Contention() with no recorded cores = %v, want %v (load/host cores)", got, want)
	}
	q := Profile{Load: 3.45, Cores: 8}
	if got := q.CoreCount(); got != 8 {
		t.Fatalf("CoreCount() with 8 recorded cores = %d, want 8 even on a %d-core host", got, host)
	}
	// Junk (a negative count) degrades to the host's own, never to a division
	// by zero or by a reference constant.
	if got := (Profile{Load: 1, Cores: -4}).CoreCount(); got != host {
		t.Fatalf("CoreCount() with Cores=-4 = %d, want the host's %d", got, host)
	}
}

// TestSummaryLineReportsTheDivisorUsed: the calibration line an operator reads
// must print the core count the arithmetic actually divided by, so
// `load 3.45/8 cores` on an 8-vCPU box can be checked against `nproc` — the
// old line printed the reference host's 16 on every host (QA-TROUBLE-17).
func TestSummaryLineReportsTheDivisorUsed(t *testing.T) {
	s := Profile{IOMultiple: 1, CPUMultiple: 1, Load: 3.45, Cores: 8}.String()
	for _, want := range []string{"load 3.45/8 cores", "contention x1.43"} {
		if !strings.Contains(s, want) {
			t.Fatalf("summary line %q does not contain %q", s, want)
		}
	}
	// The zero value reports the host's own count, not the reference's.
	auto := Profile{IOMultiple: 1, CPUMultiple: 1, Load: 1}
	if want := "/" + strconv.Itoa(runtime.NumCPU()) + " cores"; !strings.Contains(auto.String(), want) {
		t.Fatalf("summary line %q does not report the host's divisor %q", auto.String(), want)
	}
}

// TestMeasureRecordsHostCores: the production path (Measure) stamps the host's
// core count into the profile, so every budget scaled by it divides by this box.
func TestMeasureRecordsHostCores(t *testing.T) {
	t.Setenv(EnvCalibOverride, "1")
	t.Setenv(EnvLoadOverride, "3.45")
	p := Measure("")
	if p.Cores != runtime.NumCPU() {
		t.Fatalf("Measure().Cores = %d, want runtime.NumCPU() = %d", p.Cores, runtime.NumCPU())
	}
	if got, want := p.Contention(), 1+3.45/float64(runtime.NumCPU()); math.Abs(got-want) > 1e-9 {
		t.Fatalf("Measure().Contention() = %v, want %v", got, want)
	}
}

// TestMeasureOverrideForcesBothMultiples pins the falsification knob the QA lane
// uses: TROUBLE_HOST_CALIB sets the scale directly so a budget's derivation can be
// driven without a slow disk, and load_avg still comes from its own override.
func TestMeasureOverrideForcesBothMultiples(t *testing.T) {
	t.Setenv(EnvCalibOverride, "2.5")
	t.Setenv(EnvLoadOverride, "8")
	p := Measure("")
	if !p.Forced {
		t.Fatalf("Measure() with %s set reported Forced=false: %+v", EnvCalibOverride, p)
	}
	if math.Abs(p.IOMultiple-2.5) > 1e-9 || math.Abs(p.CPUMultiple-2.5) > 1e-9 {
		t.Fatalf("forced multiples = %v/%v, want 2.5/2.5", p.IOMultiple, p.CPUMultiple)
	}
	if math.Abs(p.Load-8) > 1e-9 {
		t.Fatalf("forced profile load = %v, want the load override 8", p.Load)
	}
	// 2.5 x (1 + 8/host cores): the divisor is this host's own core count.
	wantScale := 2.5 * (1 + 8/float64(runtime.NumCPU()))
	if got := p.Scale(); math.Abs(got-wantScale) > 1e-9 {
		t.Fatalf("forced Scale() = %v, want %v (2.5 x (1 + 8/%d))", got, wantScale, runtime.NumCPU())
	}
	// A junk override is ignored rather than poisoning the model.
	t.Setenv(EnvCalibOverride, "not-a-number")
	if p := Measure(""); p.Forced || p.IOMultiple < 1 {
		t.Fatalf("junk override produced %+v", p)
	}
}

// TestMeasureDegradesToReferenceOnPilotFailure: a host with no writable scratch
// space (or a read-only filesystem) must not lose its budget model. The I/O
// multiple falls back to the reference number and the CPU pilot still runs.
// The CPU pilot always runs (it needs no filesystem), so the raw CPUMultiple is
// whatever this host measured — legitimately BELOW 1 on a box faster than the
// reference class (this host measured 0.41, i.e. ~2.4x reference speed). The
// invariant the package guarantees is never a raw multiple: it is that the
// scale a budget consumes (Scale()/CPUScale()) is >= 1, via clamp1 inside
// Scale()/CPUScale(). A raw multiple is never clamped in Measure(): clamping
// there would discard the real measurement and make the profile lie about the
// host.
func TestMeasureDegradesToReferenceOnPilotFailure(t *testing.T) {
	t.Setenv(EnvCalibOverride, "")
	missing := filepath.Join(t.TempDir(), "does-not-exist", "nested")
	p := Measure(missing)
	if p.IOMultiple != 1 {
		t.Errorf("IOMultiple = %v on an unusable pilot dir, want the reference 1", p.IOMultiple)
	}
	if p.CPUMultiple <= 0 {
		t.Errorf("CPUMultiple = %v, want > 0 (the CPU pilot needs no scratch space and must still run)", p.CPUMultiple)
	}
	if got := p.Scale(); got < 1 {
		t.Errorf("Scale() = %v, want >= 1 (clamp1 must never tighten a budget)", got)
	}
	if got := p.CPUScale(); got < 1 {
		t.Errorf("CPUScale() = %v, want >= 1 (clamp1 must never tighten a budget, even when the raw CPUMultiple is %v)", got, p.CPUMultiple)
	}
}

// TestMeasurePilotsProduceUsableNumbers is the live probe: on a real writable
// directory the I/O pilot returns a positive duration in a plausible band, and
// the CPU pilot returns a positive duration. It asserts shape, not a budget — the
// band is wide on purpose, because this repository is routinely measured while
// other builds run.
func TestMeasurePilotsProduceUsableNumbers(t *testing.T) {
	dir := t.TempDir()
	ioSec, err := MeasureIOPilot(dir)
	if err != nil {
		t.Fatalf("I/O pilot: %v", err)
	}
	if ioSec <= 0 || ioSec > 60 {
		t.Fatalf("I/O pilot = %vs, want a positive duration under a minute", ioSec)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("I/O pilot left %d files behind in %s", len(left), dir)
	}
	cpuSec := MeasureComputePilot()
	if cpuSec <= 0 || cpuSec > 10 {
		t.Fatalf("compute pilot = %vs, want a positive duration well under ten seconds", cpuSec)
	}
	t.Logf("pilots: io %.1fms (reference %.2fms, multiple %.2f) · cpu %.1fms (reference %.1fms, multiple %.2f)",
		ioSec*1000, RefIOPilotSeconds*1000, ioSec/RefIOPilotSeconds,
		cpuSec*1000, ComputeReferenceSeconds*1000, cpuSec/ComputeReferenceSeconds)
}

// bestOf returns the cheapest of n runs of f, in seconds. Cheapest, not mean:
// the pilot is a lower bound on what the box can do, not an average of what the
// suite's own pressure let it do.
func bestOf(n int, f func()) float64 {
	best := math.Inf(1)
	for i := 0; i < n; i++ {
		start := time.Now()
		f()
		if sec := time.Since(start).Seconds(); sec < best {
			best = sec
		}
	}
	return best
}

// TestComputePilotMeasuresMixedWork is the QA-TROUBLE-17 CPU-pilot proof: the
// pilot must price the work the gated budgets actually measure — allocations,
// lock handoff and memory traffic — and not just a tight register loop. The
// integer-only pilot read the 8-vCPU bunker FASTER than the reference class
// (0.71, clamped to 1.0 = no loosening) while that box ran the gated workloads
// ~3x slower, because none of their cost is ALU cost.
func TestComputePilotMeasuresMixedWork(t *testing.T) {
	intAlone := bestOf(pilotRuns, func() { ComputeSink = pilotIntegerPhase(pilotIntegerOps) })
	mixed := MeasureComputePilot()
	t.Logf("integer phase alone %.2fms vs mixed pilot %.2fms", intAlone*1000, mixed*1000)
	if mixed <= intAlone*1.5 {
		t.Fatalf("mixed pilot = %.2fms, integer phase alone = %.2fms: the pilot is an integer loop in disguise and cannot price alloc/lock/memory-bound work",
			mixed*1000, intAlone*1000)
	}

	// Each non-integer phase must cost measurable time at the pilot's own
	// sizes: a phase that got optimised away would make the pilot lie again.
	allocSec := bestOf(pilotRuns, func() { ComputeSink ^= pilotAllocPhase(pilotAllocOps) })
	lockSec := bestOf(pilotRuns, func() { ComputeSink ^= pilotLockPhase(pilotLockWorkers, pilotLockOps) })
	memSec := bestOf(pilotRuns, func() { ComputeSink ^= pilotMemoryPhase(make([]byte, pilotSweepBytes), pilotSweepPasses) })
	for _, p := range []struct {
		name string
		sec  float64
	}{{"alloc", allocSec}, {"lock", lockSec}, {"memory", memSec}} {
		if p.sec <= 0 {
			t.Fatalf("%s phase measured %vs: the phase must cost real time for the pilot to price it", p.name, p.sec)
		}
		t.Logf("%s phase: %.2fms", p.name, p.sec*1000)
	}
	// The memory phase must actually be memory traffic: a sweep of a
	// cache-line-strided buffer larger than L2 costs orders of magnitude more
	// than the same code over a few KiB.
	tiny := bestOf(pilotRuns, func() {
		ComputeSink ^= pilotMemoryPhase(make([]byte, 1<<10), pilotSweepPasses)
	})
	if memSec <= tiny*1.2 {
		t.Fatalf("memory phase over %d bytes = %.2fms vs %d bytes = %.2fms: the sweep is not paying memory traffic",
			pilotSweepBytes, memSec*1000, 1<<10, tiny*1000)
	}
	// The allocation phase must be allocation: the same number of iterations
	// writing into one reused object costs a fraction of allocating them.
	reuseSec := bestOf(pilotRuns, func() {
		var one pilotAlloc
		for i := 0; i < pilotAllocOps; i++ {
			one[0] = uint64(i)
			ComputeSink ^= one[0]
		}
	})
	if allocSec <= reuseSec*1.2 {
		t.Fatalf("alloc phase = %.2fms vs a reused object at the same op count = %.2fms: the phase is not paying the allocator",
			allocSec*1000, reuseSec*1000)
	}
}
