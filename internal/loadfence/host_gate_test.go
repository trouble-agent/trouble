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
		{15.99, false},
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
