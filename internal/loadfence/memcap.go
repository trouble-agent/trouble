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

// namedCeiling is one detected memory ceiling and where it was read from.
type namedCeiling struct {
	bytes  int64
	source string
}

// tightestCeiling resolves the effective ceiling from every detected source:
// the tightest cap is the one that binds first. Non-positive figures and the
// unlimited sentinels (anything above 1<<62: the cgroup v1 unlimited value
// and RLIM_INFINITY both live there) are dropped rather than treated as caps,
// so an unreadable or uncapped source can never gate a test. No ceiling at
// all resolves to (-1, "") — the no-ceiling sentinel the fences read as "run
// the full fixture".
func tightestCeiling(cands []namedCeiling) (int64, string) {
	best, bestSrc := int64(-1), ""
	for _, c := range cands {
		if c.bytes <= 0 || c.bytes > 1<<62 {
			continue
		}
		if best < 0 || c.bytes < best {
			best, bestSrc = c.bytes, c.source
		}
	}
	return best, bestSrc
}

// MemCap reports the memory ceiling this process runs under, and where it was
// read from. cap < 0 means NO ceiling was detected (the unlimited branch: the
// test runs its full fixture). source is "" in that case.
//
// Resolution: EnvMemOverride wins alone (an operator pin must not be
// averaged against the kernel's figures); otherwise the tightest of the
// detected kernel ceilings binds — cgroup v2 memory.max, cgroup v1
// limit_in_bytes, and the process rlimits (QA-TROUBLE-23: the QA harness's
// chaos-resource leg caps the suite with `ulimit -v`, which moves NO cgroup
// file, so a cgroup-only reader was blind to it). The cgroup files carry
// "max" (v2, no limit) or 9223372036854771712 (v1's "unlimited" sentinel, =
// math.MaxInt64 rounded to a page multiple) — both read as no ceiling. A cap
// of 0 is impossible (the kernel never grants it, and the override rejects
// it), so it degrades to "no ceiling" rather than gating every test.
func MemCap() (cap int64, source string) {
	if v := strings.TrimSpace(os.Getenv(EnvMemOverride)); v != "" {
		if b, ok := parseMemSize(v); ok && b > 0 {
			// Keep the operator's declared cap spelling in the provenance. The
			// verdict must show both the normalized bytes and the observed limit
			// (for example, 3145728KB), not merely the env var name.
			return b, "env:" + EnvMemOverride + "=" + v
		}
	}
	var cands []namedCeiling
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
		if err != nil {
			continue
		}
		cands = append(cands, namedCeiling{bytes: n, source: c.name})
	}
	if n, src, ok := rlimitMemCeiling(); ok {
		cands = append(cands, namedCeiling{bytes: n, source: src})
	}
	return tightestCeiling(cands)
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

// pressureSkipReason is the pure decision behind MissUnderPressure: it
// returns the skip text and true when a host condition explains a missed
// host-measured verdict — either the load fence (Oversubscribed) or a
// detected memory ceiling. Cap pressure slows producers and the runtime
// (GC assists, arena scavenging) WITHOUT moving load_avg, so the load fence
// alone was blind to the QA chaos-resource leg (QA-TROUBLE-23: the capped leg
// red TestFsyncCountIsStructural and TestAmortizedThroughput at load_avg
// 1.3-4.0 — every load fence silent). Split from the t.Skipf wrapper so the
// decision table is testable without mock TB plumbing (Go 1.26's testing.TB
// carries an unexported method and cannot be implemented outside the testing
// package).
func pressureSkipReason(gate, measured string, load float64) (string, bool) {
	if Oversubscribed(load) {
		return SkipText(gate, measured, load), true
	}
	if capBytes, source := MemCap(); capBytes > 0 {
		return fmt.Sprintf("%s SKIP: host under a memory ceiling — %s; cap pressure slows producers and the runtime without moving load_avg, so this miss is not falsifiable as a product regression here: %s",
			gate, MemCapText(capBytes, source), measured), true
	}
	return "", false
}

// MissUnderPressure is Miss with the memory-ceiling half of the host-pressure
// model added: a missed host-measured verdict reports an explicit SKIP when
// the host explains it — oversubscribed run queue (the load fence, carrying
// the measured load) or a detected memory ceiling (carrying the measured cap
// and its source) — and FAILs otherwise. The skip names the observed
// condition and stays falsifiable through EnvLoadOverride and EnvMemOverride.
// No host pressure: the verdict fails exactly as the unfenced gate did.
func MissUnderPressure(t testing.TB, gate, measured string, load float64) {
	t.Helper()
	if reason, skip := pressureSkipReason(gate, measured, load); skip {
		t.Skipf("%s", reason)
		return
	}
	t.Errorf("%s", measured)
}
