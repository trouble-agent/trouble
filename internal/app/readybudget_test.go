package app

// readybudget_test.go — host-derived budgets for this package's boot tests
// (TRBL-024, TRBL-055; SPEC-12 §7a "7a Host-derived test-time budgets", adopting
// the host model of SPEC-01 §7a).
//
// The boot path measures ~22-25s on a quiet reference-class box and the boot
// tests used to fence it with a bare 30s deadline, so the identical tree went
// red under fleet load: the boot was descheduled, not broken. Every
// host-measured wait in this package therefore derives its budget from the host
// itself, and the budget only ever bounds "still booting":
//
//   - the host term is the host's own MEASURED speed × the load it is running
//     under: `I/O tier × (1 + load per core)`, clamped so a budget can only ever
//     be LOOSENED relative to the reference numbers (a budget the reference host
//     meets must stay meetable here). load_avg alone is not enough: it says how
//     BUSY a box is, never how FAST it is, so a load-only multiplier grades a
//     QUIET SLOW box more strictly than a BUSY FAST one — on 2026-09-18 the same
//     commit rebuilt the same 512 MiB ledger fixture in 9,747 ms against a
//     9,000 ms reference budget on a clean JIT box at load_avg 3.99 (FAIL) while
//     a busier box at load_avg 10.78 was graded against 30,000 ms for the
//     identical work (PASS). On a quiet reference-class host the measured tier
//     is 1 and these budgets are exactly the spec numbers this suite shipped
//     with;
//   - the I/O tier — not the CPU tier — is the right speed term for these waits:
//     SPEC-12 §7b measured ~92% of a quiet boot inside ledger group-commit
//     windows (records × (window + device sync latency)), so what stretches a
//     boot is the block device's sync latency, which is what the I/O pilot
//     prices. A CPU-bound gate in this package would use CPUScale() instead;
//   - the host is measured ONCE per test process (loadfence.Measure: one
//     write+read pilot and one CPU pilot), so the figure a failure prints is the
//     figure every wait in that run used;
//   - a boot that settles (RunDaemon returns, error or not) is reported within
//     one poll tick, never after the budget, so a real refusal always fails fast;
//   - a budget that IS exhausted carries the observed host term and the elapsed
//     seconds, so a reader can tell host speed/load from a regression;
//   - on a host carrying four runnable threads per core the same expiry is
//     SKIPped instead of failed: such a box cannot separate a slow boot from a
//     descheduled one, and a red test there would be a lie.

import (
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/loadfence"
)

// Boot-budget policy. The two BASE values are the quiet-host deadlines this
// suite shipped with; every other number here is either a documented bound on
// the derivation or a falsification hook. The derivation itself is unit-tested
// (TestBootBudgetDerivation, TestBootBudgetVerdictText,
// TestBootHostTermLoosensNeverTightens).
const (
	// bootReadyBase is the quiet-host READY fence: the boot measures ~25s
	// (TestShippedExampleConfigBootsToServe), so 30s is the idle-host ceiling.
	bootReadyBase = 30 * time.Second
	// bootDrainBase is the quiet-host SIGTERM drain fence.
	bootDrainBase = 20 * time.Second
	// bootScaleMax is the ceiling on the §7a host term: past 4x the host is no
	// longer an explanation for a slow boot, so the term stops growing and the
	// SKIP fence decides what the expiry means. SPEC-12 §7a states the same
	// bound: the multiplier never drops below 1 and never exceeds 4.
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
	// host term and the elapsed time and may skip, never by the context, whose
	// cancellation looks exactly like a refusal.
	bootCtxGrace = 15 * time.Second
)

// errBootSettledWithoutReady is the terminal error awaitBootReady invents when
// RunDaemon returned nil after having signalled no READY: reporting a bare nil
// would read as "reached READY" and silently pass a boot that never served.
var errBootSettledWithoutReady = errors.New("RunDaemon returned without signalling READY")

// ---------------------------------------------------------------------------
// the host term: measured speed × load contention (SPEC-01 §7a)
// ---------------------------------------------------------------------------

// appHostPilotRuns counts how many times the host pilots actually ran. §7a
// requires the calibration to be measured ONCE per test process — not per
// assertion in a loop — and the I/O pilot writes and re-reads 8 MiB and pays 16
// fsyncs, so this counter is asserted rather than assumed.
var appHostPilotRuns int64

// appHostProfileOnce measures this host's profile on first use and reuses it for
// every later call in the process.
var appHostProfileOnce = sync.OnceValue(measureAppHostProfile)

// appHostProfile is the process's measured machine profile: the I/O tier the
// filesystem-bound budgets below scale by (and the CPU tier, carried for the log
// line and for any CPU-bound gate that later needs it). Both come from
// internal/loadfence — the same pilots every other host-calibrated gate in the
// fleet runs, so two packages on one host never disagree about how fast it is.
func appHostProfile() loadfence.Profile { return appHostProfileOnce() }

// measureAppHostProfile runs the pilots once. A TROUBLE_HOST_CALIB pin reaches
// this call unchanged: Measure returns the forced multiples without running the
// pilots, so an operator override still wins over automatic calibration.
func measureAppHostProfile() loadfence.Profile {
	atomic.AddInt64(&appHostPilotRuns, 1)
	// The pilot has to price the filesystem class the boot writes to. The boot
	// tests put every state root in t.TempDir(), i.e. under os.TempDir(), so a
	// process-wide scratch directory is that same class; the pilot removes its
	// own files. With no scratch space at all, Measure("") still resolves the
	// CPU and load halves and leaves the I/O tier at the reference — a
	// calibration that cannot measure must never become a stricter budget.
	dir, err := os.MkdirTemp("", "trouble-app-calib-")
	if err != nil {
		return loadfence.Measure("")
	}
	defer os.RemoveAll(dir)
	return loadfence.Measure(dir)
}

// hostLoadAvg1 is the host signal the budgets are derived from: field 0 of
// /proc/loadavg, and 0 on any read or parse error (SPEC-12 §7a) — 0 keeps the
// base budget and never selects the extreme-load verdict. It is the shared
// loadfence.LoadAvg1, so this package reads the same signal, with the same
// 0-on-error branch and the same fleet-wide TROUBLE_HOST_LOAD_OVERRIDE, as every
// other host-calibrated gate.
func hostLoadAvg1() float64 { return loadfence.LoadAvg1() }

// bootLoadAvg is the load figure a budget is derived from. The override exists
// so the fence itself can be exercised on demand (see the falsification recipe
// in the Testing section of SPEC-12); it is inert unless set, and it takes
// precedence over the fleet-wide load override.
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

// bootIOTier is the speed half of the host term: the measured I/O multiple,
// clamped in the one direction the model is allowed to move. A host that
// measures below the reference class — a failed pilot, a NaN, a fraction — is
// graded at exactly the reference tier 1, never below it, so calibration can
// only ever LOOSEN a budget and the reference numbers stay meetable everywhere.
func bootIOTier(ioMultiple float64) float64 {
	if ioMultiple < 1 || ioMultiple != ioMultiple {
		return 1
	}
	return ioMultiple
}

// bootLoadFactor is the load half of the host term: L other runnable threads
// per core deschedule a single-threaded boot by ~(1+L). The slope is 1 per
// runnable thread per core, the first-order model for a CPU-bound boot sharing
// the box, and it is measured rather than assumed: SPEC-12 §7b records an
// in-suite boot at 1.23 runnable threads per core needing 1.95x.
func bootLoadFactor(loadAvg float64, cores int) float64 {
	return 1 + loadfence.PerCore(loadAvg, cores)
}

// bootHostScale is the SPEC-01 §7a host term for the waits in this package:
//
//	measured I/O tier × (1 + load per core), clamped into [1, bootScaleMax]
//
// On a quiet reference-class host the tier is 1 and this is exactly the
// load-only multiplier the package shipped with, so every spec number is
// enforced unchanged; on any other host it is ≥ that multiplier, which is what
// makes a budget host-truthful instead of load-truthful. The clamp is one-way on
// purpose: a budget the reference host meets must remain meetable here.
func bootHostScale(loadAvg float64, cores int, ioMultiple float64) float64 {
	s := bootIOTier(ioMultiple) * bootLoadFactor(loadAvg, cores)
	if s < 1 {
		return 1
	}
	if s > bootScaleMax {
		return bootScaleMax
	}
	return s
}

// bootHostTerm is one wait's host term: the figures the budget was derived from
// and the budget itself. One resolution serves the budget, the log line and the
// verdict text, so the scale a failure prints is always the arithmetic of the
// speed and load printed beside it; re-reading the host at expiry would print a
// figure that no longer explains the budget.
type bootHostTerm struct {
	Load   float64 // the load figure the budget was derived from
	Cores  int     // the cores it was divided by
	IOTier float64 // the measured speed multiple, after the one-way clamp
	Scale  float64 // the §7a host term: tier × (1 + load per core), clamped
	Base   time.Duration
	Budget time.Duration
}

// newBootHostTerm composes a host term from explicit figures. The production
// path (@hostTermFor) resolves them from the measured profile; the unit tests
// pass synthetic ones so the arithmetic is checkable by hand. An unusable core
// count is read as one core (the same coercion loadfence.PerCore applies), so
// the term can never print a division by zero or a budget of NaN.
func newBootHostTerm(base time.Duration, loadAvg float64, cores int, ioMultiple float64) bootHostTerm {
	if cores < 1 {
		cores = 1
	}
	scale := bootHostScale(loadAvg, cores, ioMultiple)
	return bootHostTerm{
		Load:   loadAvg,
		Cores:  cores,
		IOTier: bootIOTier(ioMultiple),
		Scale:  scale,
		Base:   base,
		Budget: time.Duration(float64(base) * scale),
	}
}

// hostTermFor resolves the host term for one wait: the base (possibly forced by
// TROUBLE_BOOT_BUDGET_MS), the load figure (possibly forced by
// TROUBLE_BOOT_LOAD_OVERRIDE) and this process's measured I/O tier.
func hostTermFor(base time.Duration) bootHostTerm {
	return newBootHostTerm(bootBaseOverride(base), bootLoadAvg(), runtime.NumCPU(), appHostProfile().IOMultiple)
}

// figures renders the terms the host term was built from — the measured speed,
// the load per core, and the cap.
func (h bootHostTerm) figures() string {
	return fmt.Sprintf("io x%.2f × %.2f contention at load_avg %.2f / %d cores = %.2f per core, cap x%.0f",
		h.IOTier, bootLoadFactor(h.Load, h.Cores), h.Load, h.Cores,
		loadfence.PerCore(h.Load, h.Cores), bootScaleMax)
}

// hostLine renders this process's host term for the logs that are not one
// wait's — the §7b phase table prints it beside the attribution so a reader can
// see which host the table was measured on. The base is irrelevant to the
// description; any base resolves the same host term.
func hostLine() string { return hostTermFor(bootReadyBase).figures() }

// scaledBootBudget is the budget one wait may take on this host, for callers
// that need the figure before the wait exists: config_projects_test.go bounds
// the boot context with it, which keeps that bound strictly looser than the wait
// it bounds (budget + bootCtxGrace) even as the host term grows.
func scaledBootBudget(base time.Duration, loadAvg float64, cores int) time.Duration {
	return time.Duration(float64(base) * bootHostScale(loadAvg, cores, appHostProfile().IOMultiple))
}

// bootBudgetVerdict composes the terminal text for an exhausted budget and
// decides its verdict. Under the extreme-load fence the verdict is SKIP: a box
// carrying bootSkipPerCore runnable threads per core cannot separate a slow
// boot from a descheduled one, and a red test there would misreport the host as
// a regression. Both texts quote the SAME figures — the observed load_avg, the
// measured speed tier, the elapsed seconds, the base and the derived budget — so
// a skipped run and a failed run are equally readable evidence.
func bootBudgetVerdict(kind, subject string, host bootHostTerm, elapsed time.Duration, progress string) (bool, string) {
	per := loadfence.PerCore(host.Load, host.Cores)
	figures := fmt.Sprintf(
		"elapsed %s vs allowed %s (base %s × %.2f; %s)",
		roundBudgetTime(elapsed), roundBudgetTime(host.Budget), roundBudgetTime(host.Base), host.Scale, host.figures())
	tail := ""
	if progress != "" {
		tail = "; " + progress
	}
	if per >= bootSkipPerCore {
		return true, fmt.Sprintf(
			"host oversubscribed — load_avg %.2f / %d cores = %.2f per core ≥ %.2f, so this run cannot separate a slow boot from a descheduled one: %s did not reach %s within the host-derived budget: %s%s",
			host.Load, host.Cores, per, bootSkipPerCore, subject, kind, figures, tail)
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
	host := hostTermFor(base)
	t.Logf("%s: waiting up to %s for READY (base %s × %.2f; %s)",
		subject, roundBudgetTime(host.Budget), roundBudgetTime(host.Base), host.Scale, host.figures())
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
		if waited >= host.Budget {
			skip, msg := bootBudgetVerdict("READY", subject, host, waited,
				fmt.Sprintf("the boot was still in flight after %d polls and no READY callback had fired", polls))
			if skip {
				t.Skipf("%s", msg)
			}
			t.Fatalf("%s", msg)
		}
		if waited >= nextProgress {
			t.Logf("%s: still booting after %s (budget %s, io x%.2f, load_avg %.2f)",
				subject, roundBudgetTime(waited), roundBudgetTime(host.Budget), host.IOTier, host.Load)
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
	host := hostTermFor(base)
	t.Logf("%s: waiting up to %s for the drain (base %s × %.2f; %s)",
		subject, roundBudgetTime(host.Budget), roundBudgetTime(host.Base), host.Scale, host.figures())
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
		if waited >= host.Budget {
			skip, msg := bootBudgetVerdict("the SIGTERM drain", subject, host, waited,
				fmt.Sprintf("RunDaemon had not returned after %d polls", polls))
			if skip {
				t.Skipf("%s", msg)
			}
			t.Fatalf("%s", msg)
		}
		if waited >= nextProgress {
			t.Logf("%s: still draining after %s (budget %s, io x%.2f, load_avg %.2f)",
				subject, roundBudgetTime(waited), roundBudgetTime(host.Budget), host.IOTier, host.Load)
			nextProgress += bootProgressEvery
		}
		polls++
		time.Sleep(bootPollTick)
	}
}

// ---------------------------------------------------------------------------
// unit: the host signal, the calibration, the derivation and the verdict text
// ---------------------------------------------------------------------------

// TestHostLoadAvg1ReadsTheKernelSignal pins the shape the rest of the fleet uses
// (internal/ledger, internal/dashboard): field 0 of /proc/loadavg, and 0 on any
// error. The 0-on-error branch is what keeps a missing signal on the base budget
// instead of forcing the extreme-load fence. The comparison is a tolerance
// rather than an equality because the kernel's 1-minute average advances between
// the two reads — a test that reds out on the host's own clock is the class this
// file exists to remove.
func TestHostLoadAvg1ReadsTheKernelSignal(t *testing.T) {
	t.Setenv(loadfence.EnvLoadOverride, "") // the raw kernel reading
	got := hostLoadAvg1()
	if got < 0 {
		t.Fatalf("hostLoadAvg1() = %v, want a non-negative load average", got)
	}
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		t.Logf("no kernel load signal on this host (%v): only the 0-on-error branch is exercised", err)
	} else if f := strings.Fields(string(b)); len(f) == 0 {
		t.Logf("no kernel load signal on this host (/proc/loadavg carries no fields): only the 0-on-error branch is exercised")
	} else if want, perr := strconv.ParseFloat(f[0], 64); perr != nil {
		t.Logf("no kernel load signal on this host (/proc/loadavg field 0 is not a float: %v): only the 0-on-error branch is exercised", perr)
	} else if math.Abs(got-want) > 0.05 {
		t.Fatalf("hostLoadAvg1() = %v, want %v (field 0 of /proc/loadavg)", got, want)
	}

	// The fleet-wide override reaches this package too, so a calibration run can
	// force the load figure for the whole tree instead of per package.
	t.Setenv(loadfence.EnvLoadOverride, "47.64")
	if got := hostLoadAvg1(); got != 47.64 {
		t.Fatalf("hostLoadAvg1() = %v with %s=47.64, want 47.64", got, loadfence.EnvLoadOverride)
	}
}

// TestBootHostProfileIsMeasuredOncePerProcess pins the cost model of §7a: the
// pilots run once for the whole test process, every later caller gets the same
// profile, and the tier is never below the reference.
func TestBootHostProfileIsMeasuredOncePerProcess(t *testing.T) {
	first, second := appHostProfile(), appHostProfile()
	if first != second {
		t.Fatalf("appHostProfile() returned two different profiles in one process: %+v then %+v", first, second)
	}
	if n := atomic.LoadInt64(&appHostPilotRuns); n != 1 {
		t.Fatalf("the host pilots ran %d times; SPEC-01 §7a measures the host once per test process (not per assertion)", n)
	}
	if math.IsNaN(first.IOMultiple) || math.IsNaN(first.CPUMultiple) {
		t.Fatalf("a pilot produced NaN: %+v", first)
	}
	if first.IOMultiple < 0 || first.CPUMultiple < 0 {
		t.Fatalf("a pilot produced a negative multiple: %+v", first)
	}
	if tier := bootIOTier(first.IOMultiple); tier < 1 {
		t.Fatalf("bootIOTier(%v) = %v: a below-reference host must be graded at 1, never below it", first.IOMultiple, tier)
	}
	if first.IOMultiple < 1 {
		t.Logf("the I/O pilot measured this host below the reference class (io x%.2f): the tier clamps to 1 and the budgets stay at the spec numbers", first.IOMultiple)
	}
	t.Logf("host profile: %s (host term for a READY wait on this host: x%.2f)",
		first, bootHostScale(bootLoadAvg(), runtime.NumCPU(), first.IOMultiple))
}

// TestBootHostCalibrationOverridePinsTheTier pins the operator override the rest
// of the fleet uses: TROUBLE_HOST_CALIB replaces the pilots, so a calibration
// run is reproducible without manufacturing load or a slow disk. The forced
// profile is measured directly (the process profile is resolved once, above).
func TestBootHostCalibrationOverridePinsTheTier(t *testing.T) {
	t.Setenv(loadfence.EnvCalibOverride, "3.00")
	p := loadfence.Measure("") // forced: no pilots run
	if !p.Forced {
		t.Fatalf("%s=3.00 did not pin the profile: %+v", loadfence.EnvCalibOverride, p)
	}
	if tier := bootIOTier(p.IOMultiple); tier != 3.00 {
		t.Fatalf("pinned io tier = %v, want 3.00", tier)
	}
	if host := newBootHostTerm(bootReadyBase, 0, 16, p.IOMultiple); host.Budget != 90*time.Second {
		t.Fatalf("a 3.00 tier on a quiet host gave %s, want 90s (30s × 3.00)", host.Budget)
	}
}

// TestBootBudgetDerivation is the table for the derivation: over synthetic host
// figures the budget is monotone non-decreasing, never below the base, capped at
// base × bootScaleMax, exactly the base on a quiet reference-class host, and
// loosened by the host's own measured speed. The expected values are hand
// arithmetic on `I/O tier × (1 + load per core)`, independent of the
// implementation.
func TestBootBudgetDerivation(t *testing.T) {
	cases := []struct {
		name      string
		load      float64
		cores     int
		io        float64
		base      time.Duration
		want      time.Duration
		wantScale float64
		wantCap   bool // the host term is at bootScaleMax
	}{
		{name: "no host signal keeps the base", load: 0, cores: 16, io: 1, base: bootReadyBase, want: 30 * time.Second, wantScale: 1},
		{name: "negative load is coerced to zero", load: -3, cores: 16, io: 1, base: bootReadyBase, want: 30 * time.Second, wantScale: 1},
		{name: "a tier below the reference never tightens a budget", load: 0, cores: 16, io: 0.49, base: bootReadyBase, want: 30 * time.Second, wantScale: 1},
		{name: "a failed pilot leaves the reference tier", load: 0, cores: 16, io: math.NaN(), base: bootReadyBase, want: 30 * time.Second, wantScale: 1},
		{name: "the quiet box from the incident (9,747 ms against a 9,000 ms budget) is no longer graded at 1.00x", load: 0, cores: 16, io: 9747.0 / 9000.0, base: bootReadyBase, want: 32490 * time.Millisecond, wantScale: 9747.0 / 9000.0},
		{name: "a quiet host whose pilots measure it 1.32x the reference", load: 0, cores: 16, io: 1.32, base: bootReadyBase, want: 39600 * time.Millisecond, wantScale: 1.32},
		{name: "half a thread per core", load: 8, cores: 16, io: 1, base: bootReadyBase, want: 45 * time.Second, wantScale: 1.5},
		{name: "one thread per core", load: 16, cores: 16, io: 1, base: bootReadyBase, want: 60 * time.Second, wantScale: 2},
		{name: "two threads per core", load: 32, cores: 16, io: 1, base: bootReadyBase, want: 90 * time.Second, wantScale: 3},
		{name: "the drain base scales identically", load: 32, cores: 16, io: 1, base: bootDrainBase, want: 60 * time.Second, wantScale: 3},
		{name: "a slow disk and load compose", load: 32, cores: 16, io: 1.25, base: bootReadyBase, want: 112500 * time.Millisecond, wantScale: 3.75},
		{name: "three threads per core reaches the load ceiling", load: 48, cores: 16, io: 1, base: bootReadyBase, want: 120 * time.Second, wantScale: 4, wantCap: true},
		{name: "the skip fence is already at the ceiling", load: 64, cores: 16, io: 1, base: bootReadyBase, want: 120 * time.Second, wantScale: 4, wantCap: true},
		{name: "eight cores carry twice the per-core load", load: 32, cores: 8, io: 1, base: bootReadyBase, want: 120 * time.Second, wantScale: 4, wantCap: true},
		{name: "an unusable core count is read as one core", load: 16, cores: 0, io: 1, base: bootReadyBase, want: 120 * time.Second, wantScale: 4, wantCap: true},
		{name: "a silly load stays at the ceiling", load: 1e6, cores: 16, io: 1, base: bootReadyBase, want: 120 * time.Second, wantScale: 4, wantCap: true},
		{name: "an unbounded tier is still capped", load: 0, cores: 16, io: math.Inf(1), base: bootReadyBase, want: 120 * time.Second, wantScale: 4, wantCap: true},
	}
	var prevScale float64
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			host := newBootHostTerm(c.base, c.load, c.cores, c.io)
			if math.Abs(host.Scale-c.wantScale) > 1e-9 {
				t.Fatalf("host term = %v, want %v (io %v × (1 + %v/%d))", host.Scale, c.wantScale, c.io, c.load, c.cores)
			}
			if host.Scale < 1 {
				t.Fatalf("host term %v is below 1: a host never tightens a budget", host.Scale)
			}
			if host.Scale > bootScaleMax {
				t.Fatalf("host term %v exceeds the documented maximum %v", host.Scale, bootScaleMax)
			}
			if capped := host.Scale == bootScaleMax; capped != c.wantCap {
				t.Fatalf("host term %v at load %v / %d cores with io %v: cap reached = %v, want %v", host.Scale, c.load, c.cores, c.io, capped, c.wantCap)
			}
			if host.Budget != c.want {
				d := host.Budget - c.want
				if d > time.Millisecond || d < -time.Millisecond {
					t.Fatalf("budget = %s, want %s (%s × %.2f)", host.Budget, c.want, c.base, host.Scale)
				}
				t.Logf("budget %s differs from the hand figure %s by %s (float64 truncation of a non-terminating multiple)", host.Budget, c.want, d)
			}
			if host.Budget < c.base {
				t.Fatalf("budget %s is below the base %s", host.Budget, c.base)
			}
			if max := time.Duration(float64(c.base) * bootScaleMax); host.Budget > max {
				t.Fatalf("budget %s exceeds base × %.0f = %s", host.Budget, bootScaleMax, max)
			}
			if host.IOTier < 1 {
				t.Fatalf("reported I/O tier %v is below the reference: the clamp is one-way", host.IOTier)
			}
			if i > 0 {
				if host.Scale < prevScale {
					t.Fatalf("table hygiene: host term %v follows %v (the monotonicity check needs a non-decreasing table)", host.Scale, prevScale)
				}
			}
			prevScale = host.Scale
		})
	}
}

// TestBootHostTermLoosensNeverTightens asserts the one-way contract as a
// property over a load sweep rather than at the table's points: the host term is
// monotone in load, bounded by the cap, never below 1, and never TIGHTER than
// the load-only term this package shipped with — which is what "a budget the
// reference host meets must stay meetable here" means in code.
func TestBootHostTermLoosensNeverTightens(t *testing.T) {
	const cores = 16
	// The term this file replaced: load_avg alone, same slope, same cap. Hand
	// arithmetic, deliberately not a call into the implementation.
	loadOnly := func(loadAvg float64, cores int) float64 {
		s := 1 + loadfence.PerCore(loadAvg, cores)
		if s < 1 {
			return 1
		}
		if s > bootScaleMax {
			return bootScaleMax
		}
		return s
	}

	for _, io := range []float64{1, 1.32, 2.5, 8} {
		prev := 0.0
		for load := 0.0; load <= 400; load += 0.25 {
			got := bootHostScale(load, cores, io)
			if got < 1 {
				t.Fatalf("host term %v at load %v with io %v: below 1", got, load, io)
			}
			if got > bootScaleMax {
				t.Fatalf("host term %v at load %v with io %v: above the cap %v", got, load, io, bootScaleMax)
			}
			if load > 0 && got < prev {
				t.Fatalf("host term fell from %v to %v as load rose to %v (io %v)", prev, got, load, io)
			}
			prev = got
			if old := loadOnly(load, cores); got < old-1e-9 {
				t.Fatalf("host term %v at load %v with io %v is TIGHTER than the load-only term it replaces (%v): calibration may only loosen", got, load, io, old)
			}
		}
	}

	// The exact case that filed this task: a QUIET host whose own pilots measure
	// it 8.3% slower than the reference class (the ledger's 9,747 ms rebuild
	// against a 9,000 ms reference budget) was graded at 1.00x by the load-only
	// term, because load_avg says nothing about speed. The measured tier is what
	// closes that inversion; load keeps the other half.
	if got := bootHostScale(0, cores, 9747.0/9000.0); got < 9747.0/9000.0 {
		t.Fatalf("a quiet host 8.3%% slower than the reference is graded at x%.4f: the load-only inversion is still open", got)
	}
	// And the busy-but-fast box keeps the loosening load gave it: the same
	// load_avg 18.72 / 16 cores that the incident recorded yields ~2.17x on a
	// reference-speed disk.
	if got := bootHostScale(18.72, cores, 1); math.Abs(got-2.17) > 1e-9 {
		t.Fatalf("host term at load_avg 18.72 with a reference-speed disk = %v, want 2.17", got)
	}
	// A slow disk on a quiet host must not be *capped away* into a tighter bar
	// than its own measurement: below the cap the tier passes through whole.
	if got := bootHostScale(0, cores, 3.32); got < 3.32 {
		t.Fatalf("host term on a quiet host measuring io x3.32 = %v, want the measured 3.32", got)
	}
}

// TestBootBudgetVerdictText pins the verdict text: the failure and the SKIP
// carry the same observed figures (speed tier, load_avg, elapsed seconds), the
// SKIP is chosen only at or above the extreme-load fence, and the composed
// message for a forced low budget is printed as the recorded falsification.
func TestBootBudgetVerdictText(t *testing.T) {
	const cores = 16
	load := 32.0                                     // 2.00 per core on 16 cores
	base := bootReadyBase                            // 30s
	host := newBootHostTerm(base, load, cores, 1.25) // x3.75: a 1.25 tier × 3.00 contention
	scaled := scaledBootBudget(base, load, cores)    // the process's own tier, for the log
	elapsed := 90 * time.Second

	if host.Budget != 112500*time.Millisecond {
		t.Fatalf("host budget = %s, want 1m52.5s (30s × 3.75)", host.Budget)
	}
	t.Logf("host term for the composed failure: %s (x%.2f); budget as scaledBootBudget resolves it: %s", host.figures(), host.Scale, scaled)

	skip, msg := bootBudgetVerdict("READY", "daemon", host, elapsed, "the boot was still in flight after 260 polls")
	if skip {
		t.Fatalf("a load of %.2f per core must not skip: %s", loadfence.PerCore(load, cores), msg)
	}
	for _, want := range []string{
		"daemon did not reach READY",
		"elapsed 1m30s",
		"allowed 1m52.5s",
		"base 30s × 3.75",
		"io x1.25 × 3.00 contention at load_avg 32.00",
		"load_avg 32.00 / 16 cores = 2.00 per core",
		"still in flight after 260 polls",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("failure text is missing %q:\n%s", want, msg)
		}
	}
	t.Logf("FALSIFICATION (composed failure, load %.2f / %d cores, io x1.25): %s", load, cores, msg)

	// The same numbers at the fence: the verdict flips, the figures do not.
	fence := 64.0 // 4.00 per core on 16 cores
	fenceHost := newBootHostTerm(base, fence, cores, 1.25)
	skip, skipMsg := bootBudgetVerdict("READY", "daemon", fenceHost, elapsed, "the boot was still in flight after 260 polls")
	if !skip {
		t.Fatalf("a load of %.2f per core must skip, got a failure:\n%s", loadfence.PerCore(fence, cores), skipMsg)
	}
	for _, want := range []string{
		"load_avg 64.00",
		"load_avg 64.00 / 16 cores = 4.00 per core ≥ 4.00",
		"io x1.25 × 5.00 contention",
		"elapsed 1m30s",
		"did not reach READY",
	} {
		if !strings.Contains(skipMsg, want) {
			t.Fatalf("SKIP text is missing %q:\n%s", want, skipMsg)
		}
	}
	t.Logf("FALSIFICATION (composed SKIP, load %.2f / %d cores, io x1.25): %s", fence, cores, skipMsg)

	// Just below the fence the verdict is still a failure: the fence is a
	// threshold, not a band.
	if skip, below := bootBudgetVerdict("READY", "daemon", newBootHostTerm(base, 63.99, cores, 1.25), elapsed, ""); skip {
		t.Fatalf("a load below the fence skipped: %s", below)
	}

	// A forced low budget — the falsification the task asks to record: the
	// budget the harness actually allowed, and the elapsed time that blew it.
	lowHost := newBootHostTerm(time.Millisecond, 8, cores, 1)
	_, lowMsg := bootBudgetVerdict("READY", "boot against the shipped example", lowHost, 2*time.Second,
		"the boot was still in flight after 8 polls")
	for _, want := range []string{"load_avg 8.00", "elapsed 2s", "allowed 1.5ms", "base 1ms × 1.50"} {
		if !strings.Contains(lowMsg, want) {
			t.Fatalf("forced-low-budget text is missing %q:\n%s", want, lowMsg)
		}
	}
	t.Logf("FALSIFICATION (forced low budget %s at load_avg 8.00): %s", lowHost.Base, lowMsg)

	// The drain phase composes the same figures with its own phase name.
	if _, drainMsg := bootBudgetVerdict("the SIGTERM drain", "daemon", newBootHostTerm(bootDrainBase, load, cores, 1.25), 25*time.Second, "RunDaemon had not returned after 100 polls"); !strings.Contains(drainMsg, "did not reach the SIGTERM drain") || !strings.Contains(drainMsg, "elapsed 25s") {
		t.Fatalf("drain text is wrong:\n%s", drainMsg)
	}

	// The composer is total: an empty progress field is legal, a NaN tier and a
	// base ≤ 0 must not divide by zero or print a NaN scale.
	_, degenerate := bootBudgetVerdict("READY", "daemon", newBootHostTerm(0, 0, 0, math.NaN()), time.Second, "")
	if !strings.Contains(degenerate, "did not reach READY") || strings.Contains(degenerate, "NaN") {
		t.Fatalf("degenerate inputs produced %q", degenerate)
	}
}

// TestBootHostTermResolvesTheProcessHost pins the wiring between the walls that
// use a budget and the measured profile: the term a wait computes must carry
// this process's own measured tier and this host's core count, not a default.
func TestBootHostTermResolvesTheProcessHost(t *testing.T) {
	host := hostTermFor(bootReadyBase)
	prof := appHostProfile()
	if host.Cores != runtime.NumCPU() {
		t.Fatalf("host term used %d cores, want this host's %d", host.Cores, runtime.NumCPU())
	}
	if host.IOTier != bootIOTier(prof.IOMultiple) {
		t.Fatalf("host term used io tier %v, want the measured %v", host.IOTier, bootIOTier(prof.IOMultiple))
	}
	if host.Load != bootLoadAvg() {
		t.Fatalf("host term used load %v, want the resolved %v", host.Load, bootLoadAvg())
	}
	if host.Scale != bootHostScale(host.Load, host.Cores, prof.IOMultiple) {
		t.Fatalf("host term is not the derivation of its own figures: %+v", host)
	}
	if host.Budget != scaledBootBudget(bootReadyBase, host.Load, host.Cores) {
		t.Fatalf("the wait budget %s and the pre-resolved scaledBootBudget %s disagree", host.Budget, scaledBootBudget(bootReadyBase, host.Load, host.Cores))
	}
}
