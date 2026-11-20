package loadfence

import (
	"math"
	"sync"
	"time"
)

// —— in-window CPU baseline (QA-TROUBLE-17-R4) ——
//
// The two contention inputs above are both blind to one failure mode: a
// process that runs slower WITHOUT queueing. SuiteWaitFraction reads
// /proc/self/task/<tid>/schedstat field 2, i.e. time runnable-but-not-running
// — it prices DESCHEDULING. load_avg_1m is a 1-minute trailing average, so a
// short storm of parallel test binaries is both diluted and delayed. Neither
// can see SMT-sibling contention, cache pressure or frequency scaling: the
// thread is on a CPU the whole time, the run queue looks short, and every
// budget in the repo passes both probes while the work still measures 30-40%
// over its quiet cost. That is exactly the TRBL-057 shape measured on
// 2026-11-20: TestDeriveNeverBlocks read 41.557µs/call against a 32µs budget
// at load_avg 10.62 inside the tier-1 suite, while solo runs at similar load
// read 10.5-11.1µs and passed (or SKIPped) at higher load.
//
// The missing input is a BASELINE measured in the same window as the subject:
// a deterministic canary loop whose work never changes, timed immediately
// before and immediately after the subject. Its ratio against a reference-host
// cost is a direct reading of how much CPU throughput this process actually
// got during the window — no queue accounting, no 1-minute averaging — so it
// prices SMT siblings, cache pressure and frequency scaling, and (unlike a
// code change) it cannot be inflated by a regression in the subject.
//
// Two properties make it safe to compose with a budget:
//
//   - LOOSER-ONLY: the ratio is clamped by CanaryWidening to [1, CPUCanaryCap],
//     so a canary that measures faster than the reference can never tighten a
//     budget, and a junk reading degrades to 1 (a model that cannot measure
//     must not become a stricter gate).
//   - ONE CAP FOR BOTH TERMS: ComposeContention folds the suite-wait factor and
//     the canary factor into a single widening bounded by ContentionCap
//     (2.5x), rather than letting two independent 2x terms multiply to 4x.
//     That keeps a gate's regression catch alive: the derive gate's own
//     ceiling is 32µs, so 32µs × 2.5 = 80µs, still below the 83µs per-call
//     regex-compilation regression that gate exists to catch.
//
// The canary is deliberately shaped like a small piece of ordinary gated work
// rather than like a register benchmark: a tight integer phase (frequency
// scaling, ALU ports shared with an SMT sibling) plus a cache-line-strided
// sweep of a 1 MiB buffer (cache pressure from a sibling binary). It is timed
// as a WALL-CLOCK unit, and the FASTER of the two probes in a window is the
// one used — the pilot doctrine the rest of this package follows: a single
// descheduling blip inflates one sample but not both, so a sustained slowdown
// (the thing that falsifies a CPU-bound budget) is what survives, while a blip
// cannot mint a doubling. The first probe also absorbs the cold-cache warm-up,
// which is why one of them is always warm.

const (
	// CPUCanaryOps sizes the canary's tight integer phase. Together with the
	// sweep below it costs ~2.3 ms on the reference class, so a gate pays
	// ~4.6 ms for the two probes it takes — a few percent of a wall-clock
	// measurement window that runs hundreds of milliseconds.
	CPUCanaryOps = 2_000_000

	// The sweep phase: every CPUCanaryStride-th byte of a 1 MiB buffer
	// (larger than L2 on the small boxes the batteries run on), so the phase
	// pays memory traffic rather than cache hits.
	CPUCanarySweepBytes  = 1 << 20
	CPUCanarySweepPasses = 8
	CPUCanaryStride      = 64

	// CPUCanaryReference is the reference class's cost for one canary run
	// (16 cores, kernel 7.0.0-43-generic, Go 1.26.11). Set at the cheapest of
	// 25 fresh-window observations (2.309 ms best, 2.475 ms mean) taken on
	// 2026-11-20, the same doctrine as ComputeReferenceSeconds: the constant
	// is the cheapest observation rather than the quiet-box capability floor,
	// so a quiet box below it clamps to 1 and only a genuine slowdown widens.
	// Only the RATIO matters, so this has to be the same CLASS of machine the
	// gated budgets were measured on, not the same machine.
	CPUCanaryReference = 2300 * time.Microsecond

	// CPUCanaryCap bounds the canary term alone at 2x: a canary measuring
	// twice its quiet reference means the process got half the CPU it
	// expects, which is already the limit of "this host explains the miss";
	// past it the shape of the failure is the code or the host's class (which
	// Profile prices), not in-window pressure.
	CPUCanaryCap = 2.0

	// ContentionCap bounds the PRODUCT of the suite-wait term and the canary
	// term at 2.5x — the single widening any budget may take from in-run
	// pressure. It is the number that keeps every gate's regression catch
	// alive, because it is chosen against the tightest of them rather than in
	// the abstract: TestDeriveNeverBlocks catches a per-call regexp
	// compilation at 83µs, and its own load curve caps at 32µs, so
	// 32µs × 2.5 = 80µs < 83µs is the envelope the mechanism may never
	// cross. A gate with a tighter headroom composes the same terms itself
	// and must state its own arithmetic.
	ContentionCap = 2.5
)

var (
	canaryBufOnce sync.Once
	canaryBuf     []byte

	// canaryRun is canarySample behind a variable so a test can simulate an
	// in-window CPU slowdown — the only way to prove the arithmetic without
	// manufacturing host load. Nothing outside this package may rely on it.
	canaryRun = canarySample
)

// CanarySink keeps the canary's result observable, so no phase of it can be
// optimised away.
var CanarySink uint64

// CPUBaseline is one window's in-window CPU reading: the canary sample used,
// the reference cost it is compared against, and their raw ratio. Ratio is
// unclamped (it can read below 1 on a box faster than the reference class);
// CanaryWidening is what turns it into the looser-only widening a budget takes.
type CPUBaseline struct {
	Sample    time.Duration `json:"sample"`
	Reference time.Duration `json:"reference"`
	Ratio     float64       `json:"ratio"`
}

// InWindowCPUBaseline runs f between two canary samples and reports the FASTER
// of them as the window's CPU baseline (see the file comment: a blip inflates
// one sample, a sustained slowdown inflates both). The subject is untouched —
// it is called exactly once, on the calling goroutine, and its own timing is
// whatever the caller measured inside it.
//
// best-effort by contract: if the canary cannot be timed (a zero sample) the
// ratio reads 1 and every consumer of CanaryWidening keeps its budget
// unmodified.
func InWindowCPUBaseline(f func()) CPUBaseline {
	first := canaryRun()
	f()
	second := canaryRun()

	sample := first
	if second < sample {
		sample = second
	}
	b := CPUBaseline{Sample: sample, Reference: CPUCanaryReference, Ratio: 1}
	if sample > 0 && CPUCanaryReference > 0 {
		b.Ratio = float64(sample) / float64(CPUCanaryReference)
		if math.IsNaN(b.Ratio) || math.IsInf(b.Ratio, 0) {
			b.Ratio = 1
		}
	}
	return b
}

// CanarySample times one canary loop on the calling goroutine. Exported so a
// caller can price a one-sided window (a loop that must not be wrapped) or
// report the reference cost beside its own; InWindowCPUBaseline is the shape
// to prefer.
func CanarySample() time.Duration { return canaryRun() }

// CanaryWidening converts a canary RATIO (sample over reference) into the
// multiplicative allowance a budget takes: 1 for a box at or below the
// reference cost, the ratio itself above it, clamped at CPUCanaryCap. Looser
// only by construction — the output domain is [1, CPUCanaryCap] for every
// input in (-inf, inf) — and a NaN, zero or negative reading is treated as no
// evidence rather than as a widening.
func CanaryWidening(ratio float64) float64 {
	if math.IsNaN(ratio) || ratio <= 1 {
		return 1
	}
	if ratio > CPUCanaryCap {
		return CPUCanaryCap
	}
	return ratio
}

// ComposeContention folds the suite-wait factor (SuiteContention) and the
// in-window canary factor (CanaryWidening) into the single widening a budget
// consumes. Each input is clamped to its own term cap first — so a caller that
// hands over a hand-built number cannot smuggle in a term the individual
// functions would have refused — and the PRODUCT is then clamped to
// ContentionCap. The second result reports whether the product cap bound the
// value, so a gate can log that its budget was at the envelope rather than at
// its measured contention.
//
// The cap is what makes the composition safe: two independent 2x terms would
// otherwise multiply to 4x, and a gate whose regression line is 3-4x its
// budget would stop catching that regression. See ContentionCap for the
// arithmetic of the tightest gate in the repo.
func ComposeContention(suiteFactor, canaryFactor float64) (factor float64, capped bool) {
	total := clampTerm(suiteFactor, suiteFactorCap) * clampTerm(canaryFactor, CPUCanaryCap)
	if math.IsNaN(total) || total < 1 {
		return 1, false
	}
	if total > ContentionCap {
		return ContentionCap, true
	}
	return total, false
}

// suiteFactorCap is the suite-wait term's own ceiling (SuiteContention's
// output domain is [1, 1/(1-SuiteWaitCap)]), named here so ComposeContention
// clamps each input exactly as the function that produced it would have.
const suiteFactorCap = 1 / (1 - SuiteWaitCap)

// clampTerm clamps one factor into [1, cap], treating a NaN or a junk reading
// as no evidence.
func clampTerm(v, cap float64) float64 {
	if math.IsNaN(v) || v < 1 {
		return 1
	}
	if v > cap {
		return cap
	}
	return v
}

// canarySample times one canary run: the tight integer phase then the
// cache-strided sweep, in one wall-clock window.
func canarySample() time.Duration {
	start := time.Now()
	x := canaryIntegerPhase(CPUCanaryOps)
	x ^= canarySweepPhase(canaryBuffer(), CPUCanarySweepPasses)
	CanarySink = x
	return time.Since(start)
}

// canaryBuffer is the sweep phase's buffer, allocated once per process: the
// canary runs on every gated window, and a 1 MiB allocation per run would
// price the allocator and the scavenger instead of the CPU.
func canaryBuffer() []byte {
	canaryBufOnce.Do(func() {
		canaryBuf = make([]byte, CPUCanarySweepBytes)
	})
	return canaryBuf
}

// canaryIntegerPhase is the canary's tight phase — deliberately the same
// dependency-chain shape the pilot uses (a multiply per iteration, no
// vectorisation, no memory traffic), so it prices the CPU the process actually
// got rather than the width of the SIMD unit.
func canaryIntegerPhase(ops uint64) uint64 {
	x := uint64(14695981039346656037)
	for j := uint64(1); j <= ops; j++ {
		x = (x ^ j) * 1099511628211
	}
	return x
}

// canarySweepPhase is the canary's memory phase: every CPUCanaryStride-th
// byte of the buffer, read-modify-written, so a sibling process thrashing the
// cache inflates the sample.
func canarySweepPhase(buf []byte, passes int) uint64 {
	var sum uint64
	for p := 0; p < passes; p++ {
		for i := 0; i < len(buf); i += CPUCanaryStride {
			buf[i]++
			sum += uint64(buf[i])
		}
	}
	return sum
}

// String is the log line a gate carries: the sample, the reference it is
// compared against and the raw ratio.
func (b CPUBaseline) String() string {
	return "canary " + b.Sample.String() + " vs reference " + b.Reference.String() +
		" (ratio x" + f2(b.Ratio) + ")"
}
