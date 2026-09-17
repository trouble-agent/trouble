//go:build race

package sentinel

// raceEnabled reports whether this test binary was built with -race. SPEC-04 §7's
// load test is a host measurement taken without instrumentation; race
// instrumentation costs 5-10x and its allocation tracking inflates RSS by orders
// of magnitude, so the absolute numbers are asserted on the plain build and the
// race build asserts correctness and concurrency instead.
const raceEnabled = true
