//go:build race

package scrub

// raceEnabled reports whether this test binary was built with -race. The §3.9
// budget table is a host measurement taken without instrumentation; race
// instrumentation costs 5-10x, so the absolute numbers are asserted on the plain
// build and the race build asserts correctness and concurrency instead.
// `go test -count=1` is the perf gate; `-race` is the correctness gate.
const raceEnabled = true
