//go:build !race

package scrub_test

// raceEnabled is false on the plain build, which is where the measured
// throughput assertions apply. See race_ext_test.go.
const raceEnabled = false
