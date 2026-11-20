package loadfence

import (
	"math"
	"testing"
	"time"
)

// TestCanaryWideningIsRatioBased is the core arithmetic of the QA-TROUBLE-17-R4
// term: the allowance is the canary's measured ratio against its quiet
// reference, looser-only, clamped at CPUCanaryCap. Table-driven, no wall clock:
// every row is a ratio, so the test says exactly what the mechanism promises.
func TestCanaryWideningIsRatioBased(t *testing.T) {
	cases := []struct {
		ratio float64
		want  float64
	}{
		{0, 1},    // no sample: no evidence, never a tightening
		{-3, 1},   // junk
		{0.5, 1},  // faster than the reference class: clamped, never tightened
		{0.99, 1}, // just under the reference
		{1, 1},    // exactly the reference: the quiet-host budget, unchanged
		{1.05, 1.05},
		{1.3, 1.3}, // the measured TRBL-057 shape: 41.557µs / 32µs ≈ 1.30
		{1.5, 1.5},
		{2, 2},   // the term's own cap
		{2.5, 2}, // above the cap: clamped
		{100, 2}, // far above: still the cap
		{math.NaN(), 1},
	}
	for _, c := range cases {
		got := CanaryWidening(c.ratio)
		if math.Abs(got-c.want) > 1e-9 {
			t.Fatalf("CanaryWidening(%v) = %v, want %v", c.ratio, got, c.want)
		}
		if got < 1 {
			t.Fatalf("CanaryWidening(%v) = %v: the term tightened a budget", c.ratio, got)
		}
		if got > CPUCanaryCap {
			t.Fatalf("CanaryWidening(%v) = %v: above the %vx cap", c.ratio, got, CPUCanaryCap)
		}
	}
	// Monotone non-decreasing in the ratio, so a slower window is never graded
	// more strictly than a faster one.
	prev := 0.0
	for r := 0.0; r <= 3.0; r += 0.05 {
		f := CanaryWidening(r)
		if f < prev {
			t.Fatalf("CanaryWidening fell from %v to %v as the ratio rose to %v", prev, f, r)
		}
		prev = f
	}
}

// TestInWindowCPUBaselineUsesTheFasterProbe pins the window shape: the canary
// is sampled on BOTH sides of the subject, the subject runs exactly once, and
// the faster sample is the one that is used — a single descheduling blip (the
// 4.6 ms outlier this mechanism would otherwise turn into a doubling) is
// discarded, while a slowdown that spans the window raises the sample. The
// seam is stubbed, so the test needs no load at all.
func TestInWindowCPUBaselineUsesTheFasterProbe(t *testing.T) {
	defer func(orig func() time.Duration) { canaryRun = orig }(canaryRun)

	cases := []struct {
		name         string
		first        time.Duration
		second       time.Duration
		wantSample   time.Duration
		wantRatio    float64
		wantWidening float64
	}{
		{
			name:  "quiet window: both probes at the reference",
			first: CPUCanaryReference, second: CPUCanaryReference,
			wantSample: CPUCanaryReference, wantRatio: 1, wantWidening: 1,
		},
		{
			name:  "one blip is discarded, not priced",
			first: 2 * CPUCanaryReference, second: CPUCanaryReference,
			wantSample: CPUCanaryReference, wantRatio: 1, wantWidening: 1,
		},
		{
			name:  "sustained slowdown across the window is priced",
			first: time.Duration(float64(CPUCanaryReference) * 1.3), second: time.Duration(float64(CPUCanaryReference) * 1.35),
			wantSample: time.Duration(float64(CPUCanaryReference) * 1.3), wantRatio: 1.3, wantWidening: 1.3,
		},
		{
			name:  "a slowdown past the cap is clamped",
			first: 4 * CPUCanaryReference, second: 5 * CPUCanaryReference,
			wantSample: 4 * CPUCanaryReference, wantRatio: 4, wantWidening: CPUCanaryCap,
		},
		{
			name:  "faster than the reference: still no tightening",
			first: CPUCanaryReference / 2, second: CPUCanaryReference / 2,
			wantSample: CPUCanaryReference / 2, wantRatio: 0.5, wantWidening: 1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			samples := []time.Duration{c.first, c.second}
			i := 0
			canaryRun = func() time.Duration {
				d := samples[i]
				i++
				return d
			}
			ran := 0
			b := InWindowCPUBaseline(func() { ran++ })
			if ran != 1 {
				t.Fatalf("subject ran %d times, want exactly once", ran)
			}
			if i != 2 {
				t.Fatalf("canary sampled %d times, want 2 (one on each side of the subject)", i)
			}
			if b.Sample != c.wantSample {
				t.Fatalf("sample = %s, want the faster probe %s", b.Sample, c.wantSample)
			}
			if math.Abs(b.Ratio-c.wantRatio) > 1e-9 {
				t.Fatalf("ratio = %v, want %v", b.Ratio, c.wantRatio)
			}
			if b.Reference != CPUCanaryReference {
				t.Fatalf("reference = %s, want %s", b.Reference, CPUCanaryReference)
			}
			if got := CanaryWidening(b.Ratio); math.Abs(got-c.wantWidening) > 1e-9 {
				t.Fatalf("widening = %v, want %v", got, c.wantWidening)
			}
			t.Logf("%s: %s → ratio %v → widening x%.2f", c.name, b, b.Ratio, CanaryWidening(b.Ratio))
		})
	}
	// A zero sample (a canary that could not be timed) is no evidence.
	canaryRun = func() time.Duration { return 0 }
	if b := InWindowCPUBaseline(func() {}); b.Ratio != 1 || CanaryWidening(b.Ratio) != 1 {
		t.Fatalf("unmeasurable canary produced ratio %v (want 1, no widening)", b.Ratio)
	}
}

// TestComposeContentionFoldsBothTermsUnderOneCap is the composition contract:
// the suite-wait and canary terms are multiplied, each is clamped to its own
// term cap first, and the PRODUCT is clamped at ContentionCap so two
// independent 2x terms cannot add up to 4x and swallow a gate's regression
// catch. It also pins the envelope arithmetic against the tightest gate in the
// repo, which is the reason the cap is 2.5 and not higher.
func TestComposeContentionFoldsBothTermsUnderOneCap(t *testing.T) {
	cases := []struct {
		name          string
		suite, canary float64
		want          float64
		wantCapped    bool
	}{
		{name: "quiet host, quiet suite", suite: 1, canary: 1, want: 1},
		{name: "canary alone (the TRBL-057 shape)", suite: 1, canary: 1.3, want: 1.3},
		{name: "suite alone", suite: 1.5, canary: 1, want: 1.5},
		{name: "both moderate", suite: 1.5, canary: 1.4, want: 2.1},
		{name: "both at their own caps: the product cap binds", suite: 2, canary: 2, want: ContentionCap, wantCapped: true},
		{name: "over-cap inputs are clamped before multiplying", suite: 9, canary: 9, want: ContentionCap, wantCapped: true},
		{name: "a hand-built under-1 term cannot tighten", suite: 0.5, canary: 0.5, want: 1},
		{name: "junk is no evidence", suite: math.NaN(), canary: math.NaN(), want: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, capped := ComposeContention(c.suite, c.canary)
			if math.Abs(got-c.want) > 1e-9 {
				t.Fatalf("ComposeContention(%v, %v) = %v, want %v", c.suite, c.canary, got, c.want)
			}
			if capped != c.wantCapped {
				t.Fatalf("ComposeContention(%v, %v) capped = %v, want %v", c.suite, c.canary, capped, c.wantCapped)
			}
			if got < 1 {
				t.Fatalf("ComposeContention(%v, %v) = %v: the composition tightened a budget", c.suite, c.canary, got)
			}
			if got > ContentionCap {
				t.Fatalf("ComposeContention(%v, %v) = %v: above the %vx product cap", c.suite, c.canary, got, ContentionCap)
			}
		})
	}

	// The envelope, stated as arithmetic because that is what it is: the
	// derive gate's load curve caps its own budget at 32µs and the regression
	// it exists to catch costs 83µs, so the widest budget the mechanism may
	// ever produce must stay below 83µs.
	const (
		deriveCeiling   = 32 * time.Microsecond
		regexRegression = 83 * time.Microsecond
	)
	worstCase, capped := ComposeContention(suiteFactorCap, CPUCanaryCap)
	if !capped || math.Abs(worstCase-ContentionCap) > 1e-9 {
		t.Fatalf("worst-case composition = %v (capped %v), want the product cap %v", worstCase, capped, ContentionCap)
	}
	widest := time.Duration(float64(deriveCeiling) * worstCase)
	if widest >= regexRegression {
		t.Fatalf("widest admissible derive budget = %s, which admits the %s regex-compilation regression", widest, regexRegression)
	}
	t.Logf("envelope: %s ceiling × %.2f max contention = %s < %s regression line", deriveCeiling, ContentionCap, widest, regexRegression)
}

// TestCanarySampleIsInTheReferenceClass is the live probe for the canary
// itself: the loop must cost a real, plausible amount of time and read within
// the same order of magnitude as the reference constant it is compared
// against. It asserts shape, not a budget — this repository is routinely
// measured while other builds run, and the whole point of the term is that the
// reading moves with the host.
func TestCanarySampleIsInTheReferenceClass(t *testing.T) {
	if CPUCanaryReference <= 0 {
		t.Fatalf("CPUCanaryReference = %s, want a positive reference cost", CPUCanaryReference)
	}
	best := time.Duration(0)
	for i := 0; i < 5; i++ {
		if d := CanarySample(); best == 0 || d < best {
			best = d
		}
	}
	if best <= 0 {
		t.Fatalf("CanarySample measured %s, want a positive duration", best)
	}
	// A canary that reads 20x its reference is a stale constant from another
	// design, not a busy host: the loop's own work never changes, so the only
	// thing that can move it by orders of magnitude is the constant being
	// wrong.
	ratio := float64(best) / float64(CPUCanaryReference)
	if ratio > 20 || ratio < 1.0/20 {
		t.Fatalf("canary measured %s vs reference %s (ratio %.2f): the constant is not in this loop's class", best, CPUCanaryReference, ratio)
	}
	if CanarySink == 0 {
		t.Fatalf("canary result is zero: the loop may have been optimised away")
	}
	t.Logf("canary: %s best of 5 vs reference %s (ratio x%.2f, widening x%.2f)", best, CPUCanaryReference, ratio, CanaryWidening(ratio))
}
