//go:build !race

package sentinel

// raceEnabled is false on the plain build, which is where the measured §7 load
// numbers apply (see race_test.go).
const raceEnabled = false
