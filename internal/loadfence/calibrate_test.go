package loadfence

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

// TestProfileScaleIsHostTruthful pins the calibration arithmetic the host-measured
// budgets compose. The property that matters is that the model NEVER tightens a
// reference-host budget: whatever the box measures, the scale is >= 1, so a box
// that would meet the spec numbers still meets them, and a box that does not gets
// a bar derived from what it actually measured.
func TestProfileScaleIsHostTruthful(t *testing.T) {
	cases := []struct {
		name        string
		io, cpu     float64
		load        float64
		wantScale   float64
		wantCPUScal float64
	}{
		{name: "reference host: no slack anywhere", io: 1, cpu: 1, load: 0, wantScale: 1, wantCPUScal: 1},
		{name: "slower disk, quiet box", io: 1.5, cpu: 1.5, load: 0, wantScale: 1.5, wantCPUScal: 1.5},
		{name: "load alone (8/16 cores of contention)", io: 1, cpu: 1, load: 8, wantScale: 1.5, wantCPUScal: 1.5},
		{name: "slower disk AND load multiply (the 09-18 inversion case)", io: 1.3, cpu: 1.3, load: 4, wantScale: 1.625, wantCPUScal: 1.625},
		{name: "a slower-than-reference reading can never tighten", io: 0.5, cpu: 0.5, load: 0, wantScale: 1, wantCPUScal: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := Profile{IOMultiple: c.io, CPUMultiple: c.cpu, Load: c.load}
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
			lo := Profile{IOMultiple: io, Load: load}.Scale()
			hi := Profile{IOMultiple: io * 1.5, Load: load}.Scale()
			if hi < lo {
				t.Fatalf("scale fell from %v to %v as the measured I/O multiple rose", lo, hi)
			}
			busier := Profile{IOMultiple: io, Load: load + 4}.Scale()
			if busier < lo {
				t.Fatalf("scale fell from %v to %v as load rose", lo, busier)
			}
		}
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
	// 2.5 x (1 + 8/16) = 3.75
	if got := p.Scale(); math.Abs(got-3.75) > 1e-9 {
		t.Fatalf("forced Scale() = %v, want 3.75", got)
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
func TestMeasureDegradesToReferenceOnPilotFailure(t *testing.T) {
	t.Setenv(EnvCalibOverride, "")
	missing := filepath.Join(t.TempDir(), "does-not-exist", "nested")
	p := Measure(missing)
	if p.IOMultiple != 1 {
		t.Errorf("IOMultiple = %v on an unusable pilot dir, want the reference 1", p.IOMultiple)
	}
	if p.CPUMultiple < 1 {
		t.Errorf("CPUMultiple = %v, want >= 1 even when the I/O pilot failed", p.CPUMultiple)
	}
	if p.Scale() < 1 {
		t.Errorf("Scale() = %v, want >= 1", p.Scale())
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
