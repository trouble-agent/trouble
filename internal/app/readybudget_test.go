package app

// readybudget_test.go — host-derived budgets for this package's boot tests
// (TRBL-024; SPEC-12 "7a Host-derived test-time budgets").
//
// The boot path measures ~22-25s on a quiet 16-core box and the boot tests used
// to fence it with a bare 30s deadline, so the identical tree went red under
// fleet load: the boot was descheduled, not broken. Every host-measured wait in
// this package now derives its budget from the host's 1-minute load average, and
// the budget only ever bounds "still booting":
//
//   - a boot that settles (RunDaemon returns, error or not) is reported within
//     one poll tick, never after the budget, so a real refusal always fails fast;
//   - a budget that IS exhausted carries the observed load_avg and the elapsed
//     seconds, so a reader can tell host load from a regression;
//   - on a host carrying four runnable threads per core the same expiry is
//     SKIPped instead of failed: such a box cannot separate a slow boot from a
//     descheduled one, and a red test there would be a lie.

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Boot-budget policy. The two BASE values are the quiet-host deadlines this
// suite shipped with; every other number here is either a documented bound on
// the derivation or a falsification hook. The derivation itself is unit-tested
// (TestBootBudgetDerivation, TestBootBudgetVerdictText).
const (
	// bootReadyBase is the quiet-host READY fence: the boot measures ~25s
	// (TestShippedExampleConfigBootsToServe), so 30s is the idle-host ceiling.
	bootReadyBase = 30 * time.Second
	// bootDrainBase is the quiet-host SIGTERM drain fence.
	bootDrainBase = 20 * time.Second
	// bootScaleMax is the ceiling on the load multiplier: past 4x the host is no
	// longer an explanation for a slow boot, so the scale stops growing and the
	// SKIP fence decides what the expiry means.
	bootScaleMax = 4.0
	// bootSkipPerCore is the extreme-load fence: at four runnable threads per
	// core, an exhausted budget says nothing about the code under test.
	bootSkipPerCore = 4.0
	// bootPollTick is the probe interval. It bounds how long a refusal or a
	// READY can go unnoticed (one tick) and it paces the progress lines.
	bootPollTick = 250 * time.Millisecond
	// bootProgressEvery is how often the wait logs a progress line while it is
	// still waiting.
	bootProgressEvery = 5 * time.Second
	// bootCtxGrace keeps a context bound STRICTLY looser than the wait it
	// bounds: an exhausted budget must be reported by the wait, which knows the
	// load and the elapsed time and may skip, never by the context, whose
	// cancellation looks exactly like a refusal.
	bootCtxGrace = 15 * time.Second
)

// errBootSettledWithoutReady is the terminal error awaitBootReady invents when
// RunDaemon returned nil after having signalled no READY: reporting a bare nil
// would read as "reached READY" and silently pass a boot that never served.
var errBootSettledWithoutReady = errors.New("RunDaemon returned without signalling READY")

// hostLoadAvg1 reads the host's 1-minute load average. 0 means the host signal
// is unavailable (any read or parse error); it is not a claim that the host is
// quiet, it just leaves the base budget in place (the same shape
// internal/ledger/testutil_test.go and internal/dashboard/budget_test.go use).
func hostLoadAvg1() float64 {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil || v < 0 {
		return 0
	}
	return v
}

// bootLoadAvg is the load figure a budget is derived from. The override exists
// so the fence itself can be exercised on demand (see the falsification recipe
// in the Testing section of SPEC-12); it is inert unless set.
func bootLoadAvg() float64 {
	if v := os.Getenv("TROUBLE_BOOT_LOAD_OVERRIDE"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			return f
		}
	}
	return hostLoadAvg1()
}

// bootBaseOverride forces the quiet-host base, in milliseconds, so a real boot
// test can be driven through the budget-exhausted path without waiting for a
// real one. Inert unless set.
func bootBaseOverride(quiet time.Duration) time.Duration {
	if v := os.Getenv("TROUBLE_BOOT_BUDGET_MS"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms > 0 {
			return time.Duration(ms) * time.Millisecond
		}
	}
	return quiet
}

// loadPerCore is the runnable-thread load the box carries per usable core.
// A load below 0 and a core count below 1 are both coerced, so the derivation
// is total and can never produce a negative or infinite budget.
func loadPerCore(loadAvg float64, cores int) float64 {
	if loadAvg < 0 {
		loadAvg = 0
	}
	if cores < 1 {
		cores = 1
	}
	return loadAvg / float64(cores)
}

// bootBudgetScale scales a quiet-host budget by the observed load:
// 1 + loadPerCore/2, clamped into [1, bootScaleMax]. It is monotonically
// non-decreasing in load, never below 1 (a budget is never tightened by a quiet
// host) and never above bootScaleMax.
func bootBudgetScale(loadAvg float64, cores int) float64 {
	s := 1 + loadPerCore(loadAvg, cores)/2
	if s < 1 {
		return 1
	}
	if s > bootScaleMax {
		return bootScaleMax
	}
	return s
}

// scaledBootBudget is the budget one wait may take on this host.
func scaledBootBudget(base time.Duration, loadAvg float64, cores int) time.Duration {
	return time.Duration(float64(base) * bootBudgetScale(loadAvg, cores))
}

// bootBudget resolves the host signal and the budget for one wait. One load
// reading serves both the budget and the verdict text, so the scale printed in
// a failure is always the arithmetic of the load printed beside it; re-reading
// the load at expiry would print a figure that no longer explains the budget.
func bootBudget(base time.Duration) (load float64, cores int, budget time.Duration) {
	load, cores = bootLoadAvg(), runtime.NumCPU()
	base = bootBaseOverride(base)
	return load, cores, scaledBootBudget(base, load, cores)
}

// bootBudgetVerdict composes the terminal text for an exhausted budget and
// decides its verdict. Under the extreme-load fence the verdict is SKIP: a box
// carrying bootSkipPerCore runnable threads per core cannot separate a slow
// boot from a descheduled one, and a red test there would misreport the host as
// a regression. Both texts quote the SAME figures — the observed load_avg, the
// elapsed seconds, the base and the derived budget — so a skipped run and a
// failed run are equally readable evidence.
func bootBudgetVerdict(kind, subject string, base, scaled, elapsed time.Duration, loadAvg float64, cores int, progress string) (bool, string) {
	per := loadPerCore(loadAvg, cores)
	scale := 1.0
	if base > 0 {
		scale = float64(scaled) / float64(base)
	}
	figures := fmt.Sprintf(
		"elapsed %s vs allowed %s (base %s × %.2f at load_avg %.2f / %d cores = %.2f per core)",
		roundBudgetTime(elapsed), roundBudgetTime(scaled), roundBudgetTime(base), scale, loadAvg, cores, per)
	tail := ""
	if progress != "" {
		tail = "; " + progress
	}
	if per >= bootSkipPerCore {
		return true, fmt.Sprintf(
			"host oversubscribed — load_avg %.2f / %d cores = %.2f per core ≥ %.2f, so this run cannot separate a slow boot from a descheduled one: %s did not reach %s within the host-derived budget: %s%s",
			loadAvg, cores, per, bootSkipPerCore, subject, kind, figures, tail)
	}
	return false, fmt.Sprintf(
		"%s did not reach %s within the host-derived budget: %s%s",
		subject, kind, figures, tail)
}

// roundBudgetTime keeps the verdict figures readable without hiding a difference
// between two budgets: 10ms granularity above a second, 10µs below it, so a
// forced-low-budget run still prints the budget it actually allowed.
func roundBudgetTime(d time.Duration) time.Duration {
	if d >= time.Second {
		return d.Round(10 * time.Millisecond)
	}
	return d.Round(10 * time.Microsecond)
}

// awaitBootReady waits for READY, and returns as soon as the boot settles.
// reached is true when the READY callback fired; otherwise err is what the boot
// refused with (a nil RunDaemon return is reported as
// errBootSettledWithoutReady, never as success). It does not return on an
// exhausted budget: that path fails the test, or skips it under the extreme-load
// fence with the observed numbers.
func awaitBootReady(t *testing.T, subject string, base time.Duration, ready <-chan struct{}, done <-chan error) (bool, error) {
	t.Helper()
	load, cores, budget := bootBudget(base)
	t.Logf("%s: waiting up to %s for READY (base %s × %.2f at load_avg %.2f / %d cores)",
		subject, roundBudgetTime(budget), roundBudgetTime(bootBaseOverride(base)), bootBudgetScale(load, cores), load, cores)
	start := time.Now()
	nextProgress := bootProgressEvery
	polls := 0
	for {
		// READY first: a boot that served passes immediately, and a boot that
		// refused never fires this probe, so the refusal is still seen at once.
		select {
		case <-ready:
			return true, nil
		default:
		}
		select {
		case err := <-done:
			if err == nil {
				err = errBootSettledWithoutReady
			}
			return false, err
		default:
		}
		waited := time.Since(start)
		if waited >= budget {
			skip, msg := bootBudgetVerdict("READY", subject, bootBaseOverride(base), budget, waited, load, cores,
				fmt.Sprintf("the boot was still in flight after %d polls and no READY callback had fired", polls))
			if skip {
				t.Skipf("%s", msg)
			}
			t.Fatalf("%s", msg)
		}
		if waited >= nextProgress {
			t.Logf("%s: still booting after %s (budget %s, load_avg %.2f)", subject, roundBudgetTime(waited), roundBudgetTime(budget), load)
			nextProgress += bootProgressEvery
		}
		polls++
		time.Sleep(bootPollTick)
	}
}

// awaitDrainBudget waits for the daemon to settle after cancel(). The budget
// bounds "still draining"; an exhausted budget fails the test, or skips it under
// the extreme-load fence. The drain result itself is not inspected: the tests
// assert drained behaviour through the daemon's own records, exactly as before.
func awaitDrainBudget(t *testing.T, subject string, base time.Duration, done <-chan error) {
	t.Helper()
	load, cores, budget := bootBudget(base)
	t.Logf("%s: waiting up to %s for the drain (base %s × %.2f at load_avg %.2f / %d cores)",
		subject, roundBudgetTime(budget), roundBudgetTime(bootBaseOverride(base)), bootBudgetScale(load, cores), load, cores)
	start := time.Now()
	nextProgress := bootProgressEvery
	polls := 0
	for {
		select {
		case <-done:
			return
		default:
		}
		waited := time.Since(start)
		if waited >= budget {
			skip, msg := bootBudgetVerdict("the SIGTERM drain", subject, bootBaseOverride(base), budget, waited, load, cores,
				fmt.Sprintf("RunDaemon had not returned after %d polls", polls))
			if skip {
				t.Skipf("%s", msg)
			}
			t.Fatalf("%s", msg)
		}
		if waited >= nextProgress {
			t.Logf("%s: still draining after %s (budget %s, load_avg %.2f)", subject, roundBudgetTime(waited), roundBudgetTime(budget), load)
			nextProgress += bootProgressEvery
		}
		polls++
		time.Sleep(bootPollTick)
	}
}

// ---------------------------------------------------------------------------
// unit: the derivation and the verdict text
// ---------------------------------------------------------------------------

// TestHostLoadAvg1ReadsTheKernelSignal pins the shape the rest of the fleet uses
// (internal/ledger, internal/dashboard): field 0 of /proc/loadavg, and 0 on any
// error. The 0-on-error branch is what keeps a missing signal on the base
// budget instead of forcing the extreme-load fence.
func TestHostLoadAvg1ReadsTheKernelSignal(t *testing.T) {
	got := hostLoadAvg1()
	if got < 0 {
		t.Fatalf("hostLoadAvg1() = %v, want a non-negative load average", got)
	}
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		t.Logf("no kernel load signal on this host (%v): only the 0-on-error branch is exercised", err)
		return
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		t.Logf("no kernel load signal on this host (/proc/loadavg carries no fields): only the 0-on-error branch is exercised")
		return
	}
	want, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		t.Logf("no kernel load signal on this host (/proc/loadavg field 0 is not a float: %v): only the 0-on-error branch is exercised", err)
		return
	}
	if got != want {
		t.Fatalf("hostLoadAvg1() = %v, want %v (field 0 of /proc/loadavg)", got, want)
	}
}

// TestBootBudgetDerivation is the table for the derivation: over synthetic load
// values the scaled budget is monotone non-decreasing, never below the base,
// capped at base × bootScaleMax, and exactly the base on a quiet host. The
// expected values are hand arithmetic on 1 + loadPerCore/2, independent of the
// implementation.
func TestBootBudgetDerivation(t *testing.T) {
	cases := []struct {
		name    string
		load    float64
		cores   int
		base    time.Duration
		want    time.Duration
		wantPer float64
		wantCap bool // the scale is at bootScaleMax
	}{
		{name: "no host signal keeps the base", load: 0, cores: 16, base: bootReadyBase, want: 30 * time.Second, wantPer: 0},
		{name: "negative load is coerced to zero", load: -3, cores: 16, base: bootReadyBase, want: 30 * time.Second, wantPer: 0},
		{name: "half a thread per core", load: 8, cores: 16, base: bootReadyBase, want: 37500 * time.Millisecond, wantPer: 0.5},
		{name: "one thread per core", load: 16, cores: 16, base: bootReadyBase, want: 45 * time.Second, wantPer: 1},
		{name: "two threads per core", load: 32, cores: 16, base: bootReadyBase, want: 60 * time.Second, wantPer: 2},
		{name: "the drain base scales identically", load: 32, cores: 16, base: bootDrainBase, want: 40 * time.Second, wantPer: 2},
		{name: "the skip fence", load: 64, cores: 16, base: bootReadyBase, want: 90 * time.Second, wantPer: 4},
		{name: "eight cores carry twice the per-core load", load: 32, cores: 8, base: bootReadyBase, want: 90 * time.Second, wantPer: 4},
		{name: "six threads per core reaches the cap", load: 96, cores: 16, base: bootReadyBase, want: 120 * time.Second, wantPer: 6, wantCap: true},
		{name: "an unusable core count is read as one core", load: 16, cores: 0, base: bootReadyBase, want: 120 * time.Second, wantPer: 16, wantCap: true},
		{name: "a silly load stays at the cap", load: 1e6, cores: 16, base: bootReadyBase, want: 120 * time.Second, wantPer: 62500, wantCap: true},
	}
	var prevPer, prevScale float64
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			per := loadPerCore(c.load, c.cores)
			if per != c.wantPer {
				t.Fatalf("loadPerCore(%v, %d) = %v, want %v", c.load, c.cores, per, c.wantPer)
			}
			scale := bootBudgetScale(c.load, c.cores)
			if scale < 1 {
				t.Fatalf("scale %v is below 1: a budget is never tightened by a quiet host", scale)
			}
			if scale > bootScaleMax {
				t.Fatalf("scale %v exceeds the documented maximum %v", scale, bootScaleMax)
			}
			if capped := scale == bootScaleMax; capped != c.wantCap {
				t.Fatalf("scale %v at load %v / %d cores: cap reached = %v, want %v", scale, c.load, c.cores, capped, c.wantCap)
			}
			got := scaledBootBudget(c.base, c.load, c.cores)
			if got != c.want {
				t.Fatalf("scaledBootBudget(%s, %v, %d) = %s, want %s", c.base, c.load, c.cores, got, c.want)
			}
			if got < c.base {
				t.Fatalf("budget %s is below the base %s", got, c.base)
			}
			if max := time.Duration(float64(c.base) * bootScaleMax); got > max {
				t.Fatalf("budget %s exceeds base × %.0f = %s", got, bootScaleMax, max)
			}
			if i > 0 {
				if per < prevPer {
					t.Fatalf("table hygiene: per-core load %v follows %v (the monotonicity check needs a non-decreasing table)", per, prevPer)
				}
				if scale < prevScale {
					t.Fatalf("scale %v at %v per core is below the scale %v at the lower %v per core: the derivation is not monotone", scale, per, prevScale, prevPer)
				}
			}
			prevPer, prevScale = per, scale
		})
	}

	// The two documented bounds, asserted as properties over a load sweep rather
	// than at the table's points: monotone between adjacent loads and capped.
	for load := 0.0; load <= 400; load += 0.25 {
		lo := scaledBootBudget(bootReadyBase, load, 16)
		hi := scaledBootBudget(bootReadyBase, load+0.25, 16)
		if hi < lo {
			t.Fatalf("budget fell from %s to %s as load rose %v → %v", lo, hi, load, load+0.25)
		}
		if hi > time.Duration(float64(bootReadyBase)*bootScaleMax) {
			t.Fatalf("budget %s at load %v exceeds the cap", hi, load+0.25)
		}
	}
}

// TestBootBudgetVerdictText pins the verdict text: the failure and the SKIP
// carry the same observed figures (load_avg and the elapsed seconds), the SKIP
// is chosen only at or above the extreme-load fence, and the composed message
// for a forced low budget is printed as the recorded falsification.
func TestBootBudgetVerdictText(t *testing.T) {
	const cores = 16
	load := 32.0                                  // 2.0 per core on 16 cores
	base := bootReadyBase                         // 30s
	scaled := scaledBootBudget(base, load, cores) // 60s
	elapsed := 65 * time.Second

	skip, msg := bootBudgetVerdict("READY", "daemon", base, scaled, elapsed, load, cores, "the boot was still in flight after 260 polls")
	if skip {
		t.Fatalf("a load of %.2f per core must not skip: %s", loadPerCore(load, cores), msg)
	}
	for _, want := range []string{"daemon did not reach READY", "load_avg 32.00", "load_avg 32.00 / 16 cores = 2.00 per core", "elapsed 1m5s", "allowed 1m0s", "base 30s × 2.00", "still in flight after 260 polls"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("failure text is missing %q:\n%s", want, msg)
		}
	}
	t.Logf("FALSIFICATION (composed failure, load %.2f / %d cores): %s", load, cores, msg)

	// The same numbers at the fence: the verdict flips, the figures do not.
	fence := 64.0 // 4.00 per core on 16 cores
	skip, skipMsg := bootBudgetVerdict("READY", "daemon", base, scaledBootBudget(base, fence, cores), elapsed, fence, cores, "the boot was still in flight after 260 polls")
	if !skip {
		t.Fatalf("a load of %.2f per core must skip, got a failure:\n%s", loadPerCore(fence, cores), skipMsg)
	}
	for _, want := range []string{"load_avg 64.00", "load_avg 64.00 / 16 cores = 4.00 per core ≥ 4.00", "elapsed 1m5s", "did not reach READY"} {
		if !strings.Contains(skipMsg, want) {
			t.Fatalf("SKIP text is missing %q:\n%s", want, skipMsg)
		}
	}
	t.Logf("FALSIFICATION (composed SKIP, load %.2f / %d cores): %s", fence, cores, skipMsg)

	// Just below the fence the verdict is still a failure: the fence is a
	// threshold, not a band.
	if skip, below := bootBudgetVerdict("READY", "daemon", base, scaled, elapsed, 63.99, cores, ""); skip {
		t.Fatalf("a load below the fence skipped: %s", below)
	}

	// A forced low budget — the falsification the task asks to record: the
	// budget the harness actually allowed, and the elapsed time that blew it.
	lowBase, lowScaled, lowElapsed := time.Millisecond, scaledBootBudget(time.Millisecond, 8, cores), 2*time.Second
	_, lowMsg := bootBudgetVerdict("READY", "boot against the shipped example", lowBase, lowScaled, lowElapsed, 8, cores, "the boot was still in flight after 8 polls")
	for _, want := range []string{"load_avg 8.00", "elapsed 2s", "allowed 1.25ms", "base 1ms × 1.25"} {
		if !strings.Contains(lowMsg, want) {
			t.Fatalf("forced-low-budget text is missing %q:\n%s", want, lowMsg)
		}
	}
	t.Logf("FALSIFICATION (forced low budget %s at load_avg 8.00): %s", lowBase, lowMsg)

	// The drain phase composes the same figures with its own phase name.
	if _, drainMsg := bootBudgetVerdict("the SIGTERM drain", "daemon", bootDrainBase, scaledBootBudget(bootDrainBase, load, cores), 25*time.Second, load, cores, "RunDaemon had not returned after 100 polls"); !strings.Contains(drainMsg, "did not reach the SIGTERM drain") || !strings.Contains(drainMsg, "elapsed 25s") {
		t.Fatalf("drain text is wrong:\n%s", drainMsg)
	}

	// The composer is total: an empty progress field is legal and base ≤ 0 must
	// not divide by zero.
	if _, m := bootBudgetVerdict("READY", "daemon", 0, 0, time.Second, 0, 0, ""); !strings.Contains(m, "did not reach READY") {
		t.Fatalf("degenerate inputs produced %q", m)
	}
}
