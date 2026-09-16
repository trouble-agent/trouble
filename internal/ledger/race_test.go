//go:build race

package ledger

// raceEnabled reports whether this test binary was built with -race. The §7
// numbers (100k rec/s amortized, ≤50 µs per 4 KiB scrub budget, p99 ≤ window+10
// ms) are host measurements taken WITHOUT instrumentation; race instrumentation
// costs 5-10x, so those absolute floors are asserted on the plain build and the
// race build asserts the structural invariants plus the group-commit vs
// per-line ratio instead. `go test -count=1` is the perf gate; `-race` is the
// correctness gate.
const raceEnabled = true
