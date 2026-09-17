//go:build !race

package skills

// raceEnabled is false on the plain build, which is where the §7 budgets apply.
const raceEnabled = false
