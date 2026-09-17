//go:build !race

package ledger

// raceEnabled is false on the plain build, which is where the measured
// regression floors of SPEC-01 §7 apply (see race_test.go).
const raceEnabled = false
