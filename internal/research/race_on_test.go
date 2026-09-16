//go:build race

package research

// raceEnabled marks a race-instrumented run: the derivation's per-call timing
// bound is an uninstrumented measurement.
const raceEnabled = true
