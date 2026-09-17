//go:build race

package skills

// raceEnabled reports whether this test binary was built with -race. SPEC-11 §7's
// budgets (1000 artifacts ≤ 500 ms, 100 verifies ≤ 200 ms) are host measurements
// taken WITHOUT instrumentation; race instrumentation costs 5-10x, so the absolute
// budgets are asserted on the plain build and the race build asserts correctness
// only. `go test -count=1` is the perf gate; `-race` is the correctness gate.
const raceEnabled = true
