//go:build !race

package scrub

// raceEnabled is false on the plain build, which is where the measured §3.9
// performance budgets apply (see race_test.go).
const raceEnabled = false
