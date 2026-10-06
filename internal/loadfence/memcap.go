package loadfence

import (
	"fmt"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
)

// Memory-ceiling awareness for the QA-harness fences.
//
// The QA harness caps the suite's cgroup at 3 GiB (GOMEMLIMIT 2 GiB on top of
// it). QA-TROUBLE-6 measured ~1.9 GiB of live set on the 1M-record AC-6
// fixture, so a fixture whose live set cannot fit the cap makes the Go runtime
// thrash against its limit: every allocation path takes the assist, the heavy
// tests slow far past go test's own 10-minute package timeout, and the binary
// PANICS ("panic: test timed out after 10m0s") instead of reporting a clean
// FAIL/SKIP — a capped-leg failure that reads as a hang, not as a verdict.
//
// The clean-failure contract, in priority order:
//
//  1. If the gate's fixture live-set does not fit the detected ceiling, the
//     gate reports an explicit SKIP carrying the MEASURED cap value in its
//     text (SkipUnderMemCap), before any work runs.
//  2. If it does fit, the gate BOUNDS its work to the ceiling (fixture size /
//     sub-iteration scaling, CappedN) so wall time stays well under the
//     package timeout — measured, not guessed — and the spec numbers are
//     still asserted, scaled to the smaller fixture.
//  3. No gate may ever turn a memory constraint into a timeout panic: a panic
//     hides the verdict, a SKIP or a bounded run states it.

const (
	// EnvMemOverride is the GOMEMLIMIT-style test override. When set it wins
	// over every cgroup file, so a harness can falsify the fence without
	// manufacturing a cgroup. Accepts Go flag syntax (2GiB, 512MiB, 268435456)
	// and a raw byte count.
	EnvMemOverride = "TROUBLE_TEST_MEMLIMIT"

	// cgroupV2Cap / cgroupV1Cap are the kernel's per-cgroup memory ceilings.
	cgroupV2Cap = "/sys/fs/cgroup/memory.max"
	cgroupV1Cap = "/sys/fs/cgroup/memory/memory.limit_in_bytes"
)

// MemCap reports the memory ceiling this process runs under, and where it was
// read from. cap < 0 means NO ceiling was detected (the unlimited branch: the
// test runs its full fixture). source is "" in that case.
//
// Resolution order: EnvMemOverride, cgroup v2 memory.max, cgroup v1
// limit_in_bytes. The cgroup files carry "max" (v2, no limit) or
// 9223372036854771712 (v1's "unlimited" sentinel, = math.MaxInt64 rounded to a
// page multiple) — both read as no ceiling. A cap of 0 is impossible (the
// kernel never grants it, and the override rejects it), so it degrades to
// "no ceiling" rather than gating every test.
func MemCap() (cap int64, source string) {
	if v := strings.TrimSpace(os.Getenv(EnvMemOverride)); v != "" {
		if b, ok := parseMemSize(v); ok && b > 0 {
			return b, "env:" + EnvMemOverride
		}
	}
	for _, c := range []struct{ path, name string }{
		{cgroupV2Cap, "cgroup:v2:memory.max"},
		{cgroupV1Cap, "cgroup:v1:limit_in_bytes"},
	} {
		b, err := os.ReadFile(c.path)
		if err != nil {
			continue
		}
		s := strings.TrimSpace(string(b))
		if s == "max" || s == "" {
			continue
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n <= 0 {
			continue
		}
		// v1 encodes "no limit" as a near-MaxInt64 page-aligned value.
		if n > 1<<62 {
			continue
		}
		return n, c.name
	}
	return -1, ""
}

// parseMemSize parses a Go flag-style byte size: "2GiB", "512MiB", "1GiB",
// "2500KiB", or a raw byte count. SI units (GB, MB) are honoured too. Returns
// ok=false for anything it cannot fully parse — an override that silently
// degraded to a default would invert the fence's meaning.
func parseMemSize(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	// Longest units first so "GiB" is not eaten by "B".
	for _, u := range []struct {
		suf  string
		mult int64
	}{
		{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
		{"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10},
		{"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10},
		{"B", 1},
	} {
		if strings.HasSuffix(s, u.suf) {
			num := strings.TrimSpace(strings.TrimSuffix(s, u.suf))
			f, err := strconv.ParseFloat(num, 64)
			if err != nil || f <= 0 {
				return 0, false
			}
			v := f * float64(u.mult)
			if v >= 1<<62 {
				return 0, false
			}
			return int64(v), true
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// MemCapText renders the measured ceiling for a verdict line: the byte figure
// AND the source it was read from, so a SKIP never asks the reader to trust
// that a cap existed.
func MemCapText(capBytes int64, source string) string {
	if capBytes < 0 {
		return "no memory cap detected"
	}
	return fmt.Sprintf("mem cap %s (%d bytes, from %s)", HumanBytes(capBytes), capBytes, source)
}

// HumanBytes renders a byte count in the largest binary unit that keeps one
// decimal place.
func HumanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2fGiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// GOMEMLIMITBytes reads the Go runtime's own soft limit as set for THIS
// process. Best-effort: a runtime whose SetMemoryLimit was never called
// reports math.MaxInt64 (no limit), which the caller treats as "unknown, rely
// on the cgroup figures".
func GOMEMLIMITBytes() int64 {
	return debug.SetMemoryLimit(-1)
}

// MemCapSkipReason is the pure decision behind SkipUnderMemCap: it returns
// the full skip text and true when the gate must fence, or ("", false) when
// the gate runs (no ceiling, no live-set figure, or a live-set that fits).
// Split from the t.Skipf wrapper so the decision table is testable without
// mock TB plumbing (Go 1.26's testing.TB carries an unexported method and
// cannot be implemented outside the testing package).
func MemCapSkipReason(gate string, liveSet int64, liveDesc string) (string, bool) {
	capBytes, source := MemCap()
	if capBytes < 0 || liveSet <= 0 || liveSet <= capBytes {
		return "", false
	}
	return fmt.Sprintf("%s SKIP: fixture live-set %s (%s) does not fit the detected ceiling — %s. "+
		"The run would thrash against the Go soft limit and exceed go test's package "+
		"timeout (QA-TROUBLE-22 capped-leg panic). The spec numbers stay asserted on the "+
		"quiet host with the full fixture.",
		gate, HumanBytes(liveSet), liveDesc, MemCapText(capBytes, source)), true
}

// SkipUnderMemCap is the fence a heavy gate calls BEFORE building its
// fixture: when a ceiling is detected and the gate's measured live-set does
// not fit under it, the gate reports an explicit SKIP whose text carries the
// MEASURED cap value (bytes + source), the live-set estimate that did not
// fit, and the quiet-host condition the full run remains bound to.
//
// liveSet is the fixture's measured or estimated live-set in bytes; liveDesc
// is the evidence line (e.g. "1M-record AC-6 fixture, QA-TROUBLE-6 measured
// ~1.9GiB live"). ref is appended for the gate's quiet-host reference.
//
// No ceiling or a fitting live-set: no-op, the gate runs exactly as before.
func SkipUnderMemCap(t testing.TB, gate string, liveSet int64, liveDesc, ref string) {
	if reason, skip := MemCapSkipReason(gate, liveSet, liveDesc); skip {
		t.Skipf("%s Quiet-host reference: %s", reason, ref)
	}
}

// CappedN bounds a repeat count to the ceiling. unlimited is the count the
// gate runs on an uncapped host; livePerUnit is the live-set one unit (record,
// iteration, fixture chunk) costs. When the ceiling times livePerUnit cannot
// hold unlimited units, the count is scaled down to what fits — with floor as
// the smallest count that still exercises the gate's assertion — and the
// scale is reported so the caller's log line carries the evidence. On an
// uncapped host (or when the fixture already fits) the count is unchanged.
//
// The intent is the QA-TROUBLE-22 gate contract: a capped suite must FINISH
// under go test's package timeout with every verdict stated, not panic.
func CappedN(t testing.TB, gate string, unlimited, livePerUnit, floor int64) int64 {
	capBytes, source := MemCap()
	if capBytes < 0 || livePerUnit <= 0 || unlimited <= floor {
		return unlimited
	}
	fits := capBytes / livePerUnit
	if fits >= unlimited {
		return unlimited
	}
	if fits < floor {
		fits = floor
	}
	t.Logf("%s: mem cap %s (%s) bounds the run at %d of %d units (floor %d, ~%s live/unit); "+
		"the %d-unit run stays the quiet-host contract",
		gate, HumanBytes(capBytes), source, fits, unlimited, floor, HumanBytes(livePerUnit), unlimited)
	return fits
}
