// Package loadfence provides explicit verdicts for host-measured test gates.
package loadfence

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
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

const (
	EnvCI            = "CI"
	EnvGitHubActions = "GITHUB_ACTIONS"
)

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
