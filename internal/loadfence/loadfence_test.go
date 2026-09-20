package loadfence

import (
	"math"
	"strings"
	"testing"
)

func TestFenceThresholdAndText(t *testing.T) {
	for _, tc := range []struct {
		load float64
		want bool
	}{
		{44.99, false},
		{45, true},
		{61.5, true},
	} {
		if got := Oversubscribed(tc.load); got != tc.want {
			t.Errorf("Oversubscribed(%v) = %v, want %v", tc.load, got, tc.want)
		}
	}

	t.Setenv(EnvLoadOverride, "")
	measured := "derivation took 70.1µs/call, want <=32µs"
	text := SkipText("TestDeriveNeverBlocks", measured, 46.91)
	for _, want := range []string{"TestDeriveNeverBlocks SKIP", "load_avg 46.91", "fence 45.00", measured} {
		if !strings.Contains(text, want) {
			t.Errorf("SkipText missing %q: %s", want, text)
		}
	}
	if strings.Contains(text, EnvLoadOverride) {
		t.Errorf("unforced verdict names override: %s", text)
	}

	t.Setenv(EnvLoadOverride, "47.64")
	if got := LoadAvg1(); got != 47.64 || !OverrideActive() {
		t.Fatalf("override load = %v, active=%v", got, OverrideActive())
	}
	if text := SkipText("gate", measured, 47.64); !strings.Contains(text, EnvLoadOverride) {
		t.Errorf("forced verdict does not identify override: %s", text)
	}
}

func TestPerCoreCoercesInvalidInputs(t *testing.T) {
	if got := PerCore(-1, 0); got != 0 {
		t.Errorf("PerCore(-1, 0) = %v, want 0", got)
	}
	if got := PerCore(48, 0); got != 48 {
		t.Errorf("PerCore(48, 0) = %v, want 48", got)
	}
}

// TestSuiteContentionLooserOnlyAndCapped pins the TRBL-057 term's arithmetic:
// the factor lies in [1, 1/(1-SuiteWaitCap)], never tightens, treats no
// evidence (0) and junk readings (NaN, out of range) as factor 1, and the
// wait-fraction measurement itself degrades to 0 on an unreadable schedstat.
func TestSuiteContentionLooserOnlyAndCapped(t *testing.T) {
	for _, c := range []struct {
		frac     float64
		want     float64
		expected bool // whether the term reports evidence
	}{
		{0, 1, false},        // no wait measured: factor exactly 1
		{-1, 1, false},       // negative: junk, no widening
		{math.NaN(), 1, false},
		{1.5, 1, false},      // > 1 is impossible evidence: no widening
		{0.25, 1 / 0.75, true},
		{SuiteWaitCap, 2, true},
	} {
		got, active := SuiteContention(c.frac)
		if math.Abs(got-c.want) > 1e-9 || active != c.expected {
			t.Errorf("SuiteContention(%v) = (%v, %v), want (%v, %v)", c.frac, got, active, c.want, c.expected)
		}
		if got < 1 {
			t.Fatalf("SuiteContention(%v) = %v tightened a budget", c.frac, got)
		}
		if got > 1/(1-SuiteWaitCap)+1e-9 {
			t.Fatalf("SuiteContention(%v) = %v exceeded the cap", c.frac, got)
		}
	}

	// The live probe: the pilot loop inside SuiteWaitFraction runs ~30ms on
	// this class of box, and an idle-ish process shows a small but nonzero
	// wait fraction; the shape (not a budget) is what is asserted.
	frac := SuiteWaitFraction(func() {
		x := uint64(1)
		for j := uint64(1); j <= ComputeOps; j++ {
			x = (x ^ j) * 1099511628211
		}
		ComputeSink = x
	})
	if frac < 0 || frac > 1 {
		t.Fatalf("SuiteWaitFraction = %v, want a fraction in [0,1]", frac)
	}
	if frac == 0 {
		t.Log("suite-wait measured exactly 0 (schedstat absent or a perfectly clean window); term degrades to factor 1 by contract")
	}

	// Missing schedstat must read 0, not fail: the term never tightens a
	// budget because it could not measure.
	if got := SuiteWaitSamples(); got < 0 {
		t.Fatalf("SuiteWaitSamples = %v, want >= 0", got)
	}
}
