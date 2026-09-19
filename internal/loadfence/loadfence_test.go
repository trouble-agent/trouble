package loadfence

import (
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
