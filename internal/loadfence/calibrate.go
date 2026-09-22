package loadfence

import (
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Calibration for the host-measured budgets of SPEC-01/03/04/10/11.
//
// Those budgets are reference-host measurements, and every test that asserts
// one has to scale it to the host it is running on. Until this file existed the
// only host signal in the repo was load_avg_1m (LoadAvg1 below), which gets the
// comparison exactly backwards on a quiet-but-slow box: the same commit,
// measured on 2026-09-18, rebuilt the same 512 MiB ledger fixture in 9,747 ms
// at 51.6 MiB/s on a clean JIT box at load_avg 3.99 — graded against the tight
// reference numbers 9,000 ms / 60 MiB/s — while a busier dev box at load_avg
// 10.78 was graded against 30,000 ms / 20 MiB/s for the identical work. The
// busier box got a 3.3x looser bar and passed; the quieter one was held to the
// spec numbers and failed.
//
// A Profile adds the two measurements that make a budget host-truthful:
//
//	IOMultiple  — a bandwidth pilot (write 8 MiB, read it back) plus a
//	              durability pilot (fsync 16 x 4 KiB blocks), each expressed as
//	              a multiple of the reference host's own numbers from SPEC-01
//	              §1 (blockSize 1421 MiB/s, fsync p50 2.0 ms, p95 2.5 ms).
//	CPUMultiple — a fixed integer pilot, expressed as a multiple of the
//	              reference class's cost. It does not know which box is quiet or
//	              busy; it knows what one unit of work costs here.
//
// Load is still carried, because a descheduled box is a genuine cause of a slow
// measurement, so a budget is scaled by the host's own throughput AND by the
// contention it is running under:
//
//	scale = max(1, IOMultiple) x (1 + load_avg_1m / CalibReferenceCores)
//
// Everything is clamped so a host can only ever LOOSEN a budget relative to the
// reference numbers, never tighten it: a budget the reference host meets must
// remain meetable here. Callers compose the scale they need — a filesystem-bound
// gate uses Scale(), a CPU-bound gate uses CPUScale().

const (
	// EnvCalibOverride (TROUBLE_HOST_CALIB) pins both multiples in place of the
	// pilots, so a test can force the reference-host arithmetic without
	// manufacturing load or a slow disk. load_avg still comes from LoadAvg1, so
	// TROUBLE_HOST_LOAD_OVERRIDE composes with it.
	EnvCalibOverride = "TROUBLE_HOST_CALIB"

	// CalibReferenceCores is the core count the reference host's measurements
	// were taken on (SPEC-01 §1: kernel 7.0.0-30-generic, 16 cores).
	CalibReferenceCores = 16

	// The I/O pilot's shape and the reference host's cost for it. SPEC-01 §1
	// measured 1421 MiB/s on 256 KiB blocks and a 2.0 ms fsync p50 / 2.5 ms p95,
	// so the reference pilot costs 8MiB/1421MiB/s + 16 x 2.0 ms.
	calibWriteBytes   = 8 << 20
	calibFsyncBlocks  = 16
	calibFsyncBlockSz = 4096
	refWriteMiBs      = 1421.0
	refFsyncMillis    = 2.0

	// ComputeOps sizes the CPU pilot. Sized for ~30 ms on the reference class:
	// long enough that the timing is not dominated by clock resolution and short
	// enough to pay once per test binary, and run best-of-3 so a descheduling
	// blip cannot inflate the multiple.
	ComputeOps = 8_000_000
	pilotRuns  = 3
)

// RefWriteSeconds is the reference host's cost for the write+read pilot.
const RefWriteSeconds = float64(calibWriteBytes) / (refWriteMiBs * (1 << 20))

// RefFsyncSeconds is the reference host's cost for the fsync pilot.
const RefFsyncSeconds = float64(calibFsyncBlocks) * refFsyncMillis / 1000.0

// RefIOPilotSeconds is the reference host's cost for the whole I/O pilot.
const RefIOPilotSeconds = RefWriteSeconds + RefFsyncSeconds

// ComputeReferenceSeconds is the reference class's cost for the CPU pilot,
// measured in isolation on the reference-class box (kernel 7.0.0-30-generic,
// Go 1.26.5, load_avg < 1): 21.3 ms. Only the RATIO matters, so the constant
// has to be the same class of machine the spec's numbers came from, not the
// same machine; a box at 2x this number gets a 2x looser CPU-bound budget.
const ComputeReferenceSeconds = 0.0213

// Profile is one run's measured machine profile.
type Profile struct {
	IOMultiple  float64 `json:"io_multiple"`
	CPUMultiple float64 `json:"cpu_multiple"`
	Load        float64 `json:"load_avg_1m"`
	Forced      bool    `json:"forced"` // EnvCalibOverride replaced both pilots
}

// Contention is the load-derived factor: one runnable thread per reference core
// deschedules a single-threaded measurement by ~2x (the first-order model the
// boot budget in internal/app uses).
func (p Profile) Contention() float64 {
	c := 1 + PerCore(p.Load, CalibReferenceCores)
	if c < 1 {
		return 1
	}
	return c
}

// Scale is the filesystem-bound factor: I/O tier x contention.
func (p Profile) Scale() float64 { return clamp1(p.IOMultiple) * p.Contention() }

// CPUScale is the CPU-bound factor: single-thread cost x contention.
func (p Profile) CPUScale() float64 { return clamp1(p.CPUMultiple) * p.Contention() }

func clamp1(v float64) float64 {
	if v < 1 || v != v { // NaN or below the reference: never tighten a budget
		return 1
	}
	return v
}

func (p Profile) String() string {
	v := "measured"
	if p.Forced {
		v = "forced by " + EnvCalibOverride
	}
	return "io x" + f2(p.IOMultiple) + " cpu x" + f2(p.CPUMultiple) +
		" load " + f2(p.Load) + "/" + strconv.Itoa(CalibReferenceCores) + " cores" +
		" (contention x" + f2(p.Contention()) + ", " + v + ")"
}

func f2(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }

// Measure resolves a Profile. dir must be a writable directory on the same
// filesystem class as what the caller measures (a test's t.TempDir() is right);
// an empty dir skips the I/O pilot and leaves IOMultiple at 1, so a caller with
// no scratch space still gets the CPU and load halves of the model.
//
// Every pilot failure degrades to the reference number (a multiple of 1) rather
// than failing the caller: a calibration that cannot measure must not become a
// stricter budget.
func Measure(dir string) Profile {
	load := LoadAvg1()
	if v := strings.TrimSpace(os.Getenv(EnvCalibOverride)); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			return Profile{IOMultiple: f, CPUMultiple: f, Load: load, Forced: true}
		}
	}
	p := Profile{Load: load, IOMultiple: 1, CPUMultiple: 1}
	if dir != "" {
		if sec, err := MeasureIOPilot(dir); err == nil && sec > 0 {
			p.IOMultiple = sec / RefIOPilotSeconds
		}
	}
	if sec := MeasureComputePilot(); sec > 0 {
		p.CPUMultiple = sec / ComputeReferenceSeconds
	}
	return p
}

// MeasureIOPilot runs the write/read + fsync pilot in dir and returns its
// seconds. Best of pilotRuns: a single run can be inflated by a descheduling
// blip, and the pilot is a lower bound on what the box can do, not an average.
func MeasureIOPilot(dir string) (float64, error) {
	best := 0.0
	for i := 0; i < pilotRuns; i++ {
		sec, err := ioPilotOnce(dir, i)
		if err != nil {
			return 0, err
		}
		if best == 0 || sec < best {
			best = sec
		}
	}
	return best, nil
}

func ioPilotOnce(dir string, run int) (float64, error) {
	path := filepath.Join(dir, "loadfence-io-pilot-"+strconv.Itoa(run)+".bin")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(path)
	}()
	block := make([]byte, 1<<20)
	for i := range block {
		block[i] = byte(i)
	}
	start := time.Now()
	for i := 0; i < calibWriteBytes>>20; i++ {
		if _, werr := f.Write(block); werr != nil {
			return 0, werr
		}
	}
	if serr := f.Sync(); serr != nil {
		return 0, serr
	}
	if _, serr := f.Seek(0, io.SeekStart); serr != nil {
		return 0, serr
	}
	rb := make([]byte, len(block))
	for i := 0; i < calibWriteBytes>>20; i++ {
		if _, rerr := io.ReadFull(f, rb); rerr != nil {
			return 0, rerr
		}
	}
	writeRead := time.Since(start).Seconds()

	fsStart := time.Now()
	for i := 0; i < calibFsyncBlocks; i++ {
		x := []byte{byte(i)}
		if _, werr := f.WriteAt(x, int64(i)*calibFsyncBlockSz); werr != nil {
			return 0, werr
		}
		if serr := f.Sync(); serr != nil {
			return 0, serr
		}
	}
	fsync := time.Since(fsStart).Seconds()
	return writeRead + fsync, nil
}

// MeasureComputePilot returns the best of pilotRuns runs of the fixed integer
// workload, in seconds. The result is stashed in an exported sink so the
// compiler cannot delete a loop whose only cost is the timing.
func MeasureComputePilot() float64 {
	best := 0.0
	for i := 0; i < pilotRuns; i++ {
		start := time.Now()
		x := uint64(14695981039346656037)
		for j := uint64(1); j <= ComputeOps; j++ {
			x = (x ^ j) * 1099511628211
		}
		sec := time.Since(start).Seconds()
		ComputeSink = x
		if best == 0 || sec < best {
			best = sec
		}
	}
	return best
}

// ComputeSink keeps the pilot's result observable.
var ComputeSink uint64

// —— suite-internal contention (TRBL-057) ——
//
// The Profile above prices a host (what one unit of work costs here) and the
// system's run queue (load_avg_1m). It cannot see CONTENTION THAT THE SUITE
// ITSELF CREATES: `go test ./internal/...` runs many test binaries in parallel
// on the same cores, that pressure builds and drains in seconds, and the 1m
// loadavg both lags and dilutes it (three full-suite runs rotated FAILs at
// load 21/34/41 whose margins are absent in isolation — TRBL-057). The
// best-of-3 pilot is blind to it by construction: its best run prices the
// box's capability, not the environment a gate experiences seconds later.
//
// The honest signal is the gate's OWN process: Linux accounts exactly how long
// the process's threads sat runnable-but-not-running in
// /proc/self/task/<tid>/schedstat (field 2, nanoseconds). A measurement window
// that spends a fraction f of its wall time descheduled was slowed by ~1/(1-f)
// from scheduling alone — no code regression mints runqueue wait, so widening
// a budget by that factor (and only that direction) prices the suite's own
// load without ever hiding a regression behind it. It composes with the
// Profile: the pilot still prices a genuinely slower box, load still prices a
// descheduling REGIME, and this term prices the suite's own pressure that
// neither can observe.

const (
	// SuiteSchedStatPath is the per-thread scheduler-statistics file the term
	// reads. /proc/self is process-scoped by the kernel, so parallel test
	// binaries each measure exactly their own threads.
	SuiteSchedStatPath = "/proc/self/task"

	// SuiteWaitCap bounds the term at 2x: a window that was half wait is
	// already a 2x widening, and past that the shape of the failure is not
	// scheduling (the full suite at its worst sampled 0.12-0.3 in these
	// investigations). The cap is what keeps each gate's regression catch
	// alive — e.g. TestDeriveNeverBlocks must still fail a per-call regex
	// compilation (83µs measured) at EVERY contention level against its
	// 20µs quiet budget: 20µs × 32µs-ceiling × 2 = 128µs > 83µs would fail
	// that catch without a cap, so the catch is re-proven per gate below.
	SuiteWaitCap = 0.50
)

// SuiteWaitSamples reads the process's cumulative runqueue wait (nanoseconds,
// summed over every thread) from schedstat. best-effort by contract: a kernel
// without schedstat (or a read race with thread exit) returns 0 and the term
// degrades to 1 — a model that cannot measure must not tighten a budget.
// The sum is over threads: a caller comparing two samples over a window with
// T concurrent threads must divide the delta by T (the mean of the window's
// start and end task counts) before dividing by wall time, or a concurrent
// window mints T thread-seconds of wait per wall second and the fraction
// leaves its [0,1] domain (QA-TROUBLE-9 measured 1.33 this way: a sentinel
// load window ran 88 goroutine-backed threads, and the raw sum exceeded wall
// time by the worker count while the 1m loadavg sat at 2.22). SuiteTaskCount
// is the normalizer; SuiteWaitFraction applies it.
func SuiteWaitSamples() uint64 {
	entries, err := os.ReadDir(SuiteSchedStatPath)
	if err != nil {
		return 0
	}
	var total uint64
	for _, e := range entries {
		b, err := os.ReadFile(SuiteSchedStatPath + "/" + e.Name() + "/schedstat")
		if err != nil {
			continue // thread exited between readdir and read
		}
		fields := strings.Fields(string(b))
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		total += v
	}
	return total
}

// SuiteTaskCount reports how many task threads the process currently has. It
// exists because SuiteWaitSamples SUMS wait across threads: a gate measuring a
// high-concurrency window (a load test running 64 goroutines) must normalise by
// the thread count or its quotient exceeds wall time and reads as junk. Returns
// 1 when schedstat is unreadable so a caller's division stays well-defined.
func SuiteTaskCount() int {
	entries, err := os.ReadDir(SuiteSchedStatPath)
	if err != nil || len(entries) == 0 {
		return 1
	}
	return len(entries)
}

// SuiteContention converts a measured wait FRACTION (runqueue wait / wall time
// over the gate's own window) into the multiplicative allowance the budget
// takes. The fraction is clamped to [0, SuiteWaitCap], so the term lies in
// [1, 2]: looser-only by construction, and a NaN or out-of-range reading
// (clock skew between samples) is treated as no evidence rather than as a
// widening. The returned second value reports whether the term is active, so
// a gate can log the evidence on every run.
func SuiteContention(waitFraction float64) (factor float64, active bool) {
	if waitFraction <= 0 || math.IsNaN(waitFraction) || waitFraction > 1 {
		return 1, false
	}
	// QA-TROUBLE-9, defence in depth: a fraction above the cap is clamped
	// here (already true below), but a caller that hands the factor to a
	// budget composed with other multiplicative widens would otherwise read
	// a term above the documented 2x ceiling. Clamp the INPUT to the cap
	// before the reciprocal: 1/(1-f) for f in (0, cap] is in (1, 2], and
	// f <= 0 or junk stays at factor 1, so the output domain is [1, 2] for
	// every input in (-inf, inf).
	if waitFraction > SuiteWaitCap {
		waitFraction = SuiteWaitCap
	}
	return 1 / (1 - waitFraction), true
}

// SuiteWaitFraction is the whole measurement: sample, run f, sample again, and
// report the fraction of wall time this process spent descheduled. The wait
// delta is a SUM over the process's task threads, so a window running T
// concurrent threads can accumulate up to T seconds of wait per wall second;
// the raw quotient then leaves the [0,1] domain and — read as a fraction —
// double-counts the contention (QA-TROUBLE-9's measured 1.33 at load_avg 2.22
// came from exactly this: a sentinel-style concurrent window whose worker
// threads each accumulated real runqueue wait). The delta is therefore
// normalised by the mean thread count over the window, which makes the
// reading the per-thread deschedule fraction the term is defined on and pins
// the quotient to [0,1] by construction. An earlier caller-wide
// GOMAXPROCS-less measurement window (runtime.NumCPU threads on a 16-core
// box) is exactly the regime the full suite creates, so a quiet host measures
// ~0 here even under fleet load — the fleet's own threads do not enter this
// process's schedstat.
func SuiteWaitFraction(f func()) float64 {
	before := SuiteWaitSamples()
	tasksBefore := SuiteTaskCount()
	start := time.Now()
	f()
	elapsed := time.Since(start)
	after := SuiteWaitSamples()
	tasksAfter := SuiteTaskCount()
	if elapsed <= 0 || after < before {
		return 0
	}
	threads := float64(tasksBefore+tasksAfter) / 2.0
	if threads < 1 {
		threads = 1 // unreadable schedstat: degrade to 0, never tighten
	}
	return float64(after-before) / (float64(elapsed) * threads)
}

// SuiteWaitMeasure is SuiteWaitFraction for a measurement that must KEEP its
// duration: the gate gets both its number and the contention evidence the
// budget consumes, taken over exactly the measured window.
func SuiteWaitMeasure(f func() time.Duration) (time.Duration, float64) {
	var dur time.Duration
	frac := SuiteWaitFraction(func() { dur = f() })
	return dur, frac
}
