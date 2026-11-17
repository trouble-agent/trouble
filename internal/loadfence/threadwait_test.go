package loadfence

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The per-rule descheduling fence (QA-TROUBLE-030). Two things must be true for
// a scrub rule_timeout budget to be charged CPU time instead of wall time:
// the sample must describe THIS thread and move when the OS deschedules it, and
// every unreadable or incomparable sample must degrade to "no evidence" rather
// than to a longer or a shorter budget.

var waitTestSink uint64

func TestParseSchedstatWaitShapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want uint64
		ok   bool
	}{
		{"three fields (the kernel shape)", "1234567 89012 345\n", 89012, true},
		{"zeroes", "0 0 0\n", 0, true},
		{"two fields, newline", "12345 678\n", 678, true},
		{"two fields, no trailing separator", "12345 678", 678, true},
		{"tab separated", "12345\t678\t9\n", 678, true},
		{"only one field", "12345\n", 0, false},
		{"empty", "", 0, false},
		{"not numbers", "abc def ghi\n", 0, false},
		{"empty wait field", "12345  \n", 0, false},
		{"absurd counter", "12345 9999999999999999 9\n", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseSchedstatWait([]byte(tc.in))
			if ok != tc.ok {
				t.Fatalf("parseSchedstatWait(%q) ok = %v, want %v", tc.in, ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Fatalf("parseSchedstatWait(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestRunqueueWaitSinceRejectsIncomparableSamples(t *testing.T) {
	const t1, t2 = "/proc/1/task/1", "/proc/1/task/2"
	const window = 40 * time.Millisecond
	ok := func(task string, wait time.Duration) ThreadWait {
		return ThreadWait{Task: task, WaitNS: uint64(wait), OK: true}
	}
	for _, tc := range []struct {
		name    string
		before  ThreadWait
		after   ThreadWait
		elapsed time.Duration
		want    time.Duration
	}{
		{"same thread, 5ms of wait", ok(t1, 0), ok(t1, 5*time.Millisecond), window, 5 * time.Millisecond},
		{"same thread, no wait", ok(t1, 7), ok(t1, 7), window, 0},
		{"same thread, wait exactly the window", ok(t1, 0), ok(t1, window), window, window},
		{"different thread (goroutine migrated)", ok(t1, 0), ok(t2, 5*time.Millisecond), window, 0},
		{"non-monotonic counter", ok(t1, 9*time.Millisecond), ok(t1, time.Millisecond), window, 0},
		{"wait longer than the window is junk", ok(t1, 0), ok(t1, time.Second), window, 0},
		{"before unmeasurable", ThreadWait{}, ok(t1, 5*time.Millisecond), window, 0},
		{"after unmeasurable", ok(t1, 0), ThreadWait{}, window, 0},
		{"zero window", ok(t1, 0), ok(t1, 5*time.Millisecond), 0, 0},
		{"negative window", ok(t1, 0), ok(t1, 5*time.Millisecond), -time.Millisecond, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.after.RunqueueWaitSince(tc.before, tc.elapsed); got != tc.want {
				t.Fatalf("RunqueueWaitSince = %s, want %s", got, tc.want)
			}
		})
	}
}

// waitPressure is one rung of the positive control's escalation ladder.
//
// The control has to make the KERNEL deschedule this thread, and only one knob
// does that: how many runnable THREADS this process keeps. Competitor count on
// its own cannot: goroutines are multiplexed onto GOMAXPROCS OS threads, so
// three spinning goroutines on one P are one runnable thread, and a quiet host
// with a vCPU to spare never enqueues it. The Go scheduler still preempts it
// (async preemption, every ~10 ms) and moves it between the process's own
// runqueues, which stretches a 40 ms window to ~80 ms while schedstat field 2 —
// time the KERNEL spent with this thread runnable-but-not-running — stays flat.
// That is exactly the hosted-CI failure this ladder exists for (run 44049265682:
// three ~80 ms windows with 3 competitors on 1 P, 0 ns measured, "the sample is
// inert"), while the same commit measured wait 3/3 on a dev box carrying
// load_avg ~60. A crowded box deschedules a lone thread for free; a dedicated
// runner only deschedules what it cannot fit on its cores.
//
// A longer window adds chances to be descheduled, but it cannot create the
// descheduling itself — three ~80 ms windows on the runner were still flat — so
// the ladder escalates the thread count first and the window alongside it.
type waitPressure struct {
	name        string
	gomaxprocs  int
	competitors int
	window      time.Duration
}

// waitPressureLadder is the escalation, cheapest-first and bounded. The rungs
// are derived from the host's own core count so the last one oversubscribes the
// host by construction rather than by an absolute number that is generous on a
// small runner and inert on a big one.
func waitPressureLadder() []waitPressure {
	ncpu := runtime.NumCPU()
	if ncpu < 2 {
		ncpu = 2
	}
	return []waitPressure{
		// The original shape, kept as the first rung: minimal pressure, so a
		// busy host still measures wait and the control costs 40 ms where it
		// can. It is also the rung that proves the instrument on the descheduled
		// hosts these gates were written against.
		{name: "1 P, 3 competitors", gomaxprocs: 1, competitors: 3, window: 40 * time.Millisecond},
		// One thread per core with a competitor queue as long as the thread
		// pool: saturates the host's cores without oversubscribing them.
		{name: fmt.Sprintf("%d P (NumCPU), %d competitors", ncpu, ncpu), gomaxprocs: ncpu, competitors: ncpu, window: 100 * time.Millisecond},
		// Two runnable threads MORE than the host has cores — Go's own default
		// thread count on this host, plus two — and a competitor queue longer
		// than the thread pool, which is the regime where the kernel must
		// time-slice a quiet box. Cost is what an ordinary Go program on this
		// host already pays, for a quarter of a second.
		{name: fmt.Sprintf("%d P (NumCPU+2), %d competitors", ncpu+2, ncpu+8), gomaxprocs: ncpu + 2, competitors: ncpu + 8, window: 250 * time.Millisecond},
	}
}

// waitProbe is one rung's outcome: either a reading, or the reason the pair was
// not comparable (the fence discards those by design).
type waitProbe struct {
	pressure waitPressure
	window   time.Duration
	measured time.Duration
	task     string
	rejected string
	// invalid records that a snapshot stopped being readable mid-run, which is
	// a broken instrument rather than a pressure problem.
	invalid bool
}

func (p waitProbe) detail() string {
	if p.rejected != "" {
		return p.rejected
	}
	if p.window <= 0 {
		return "window unmeasurable"
	}
	return fmt.Sprintf("runqueue wait %s (%.0f%% descheduled) on %s",
		p.measured, 100*float64(p.measured)/float64(p.window), p.task)
}

// probeThreadWait holds the given pressure across ONE window and reports the
// wait the fence attributed to this thread. Every invariant of the single-shot
// control is kept: both samples must be valid, and only a same-task pair is
// compared (the caller is pinned to its OS thread, so a cross-task pair here is
// a genuine surprise worth naming rather than a silent skip).
func probeThreadWait(p waitPressure) waitProbe {
	out := waitProbe{pressure: p}

	prevProcs := runtime.GOMAXPROCS(p.gomaxprocs)
	defer runtime.GOMAXPROCS(prevProcs)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < p.competitors; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var x uint64
			for {
				select {
				case <-stop:
					atomic.AddUint64(&waitTestSink, x)
					return
				default:
					x = x*6364136223846793005 + 1442695040888963407
				}
			}
		}()
	}

	before := ThreadWaitSnapshot()
	start := time.Now()
	var x uint64
	for time.Since(start) < p.window {
		x = x*6364136223846793005 + 1442695040888963407
	}
	out.window = time.Since(start)
	after := ThreadWaitSnapshot()
	atomic.AddUint64(&waitTestSink, x)

	close(stop)
	wg.Wait()

	if !before.OK || !after.OK {
		out.invalid = true
		out.rejected = fmt.Sprintf("a sample became invalid on a host where the first was valid (%+v -> %+v)", before, after)
		return out
	}
	out.task = before.Task
	if after.Task != before.Task {
		out.rejected = fmt.Sprintf("pair straddled a thread migration (%s -> %s), not comparable", before.Task, after.Task)
		return out
	}
	out.measured = after.RunqueueWaitSince(before, out.window)
	return out
}

// TestThreadWaitSnapshotMeasuresRealDescheduling is the positive control: under
// enough pressure this thread's window MUST accumulate runqueue wait, and the
// sample must attribute it to this thread. Without it the fence could be
// silently inert (always 0) and every test above would still pass.
//
// The control is adaptive rather than weaker. Each rung is a real attempt to
// deschedule the thread and every rung keeps the original invariants, so a
// sample that is inert under ALL of them still fails here, and its message names
// the configuration that failed. Nothing is skipped on the strength of a zero
// reading: the escalation is pressure, not permission.
func TestThreadWaitSnapshotMeasuresRealDescheduling(t *testing.T) {
	// Pin the measuring goroutine to its OS thread for the whole control. The
	// fence only compares samples whose Task matches, and this goroutine runs
	// hot at every rung, so a preemption that moved it to another thread would
	// throw a rung's evidence away as a migration.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	first := ThreadWaitSnapshot()
	if !first.OK {
		t.Skipf("schedstat is unavailable on this host (%s); the fence degrades to 0 by contract", first.Task)
	}

	ladder := waitPressureLadder()
	probes := make([]waitProbe, 0, len(ladder))
	var bestWait time.Duration
	for _, rung := range ladder {
		p := probeThreadWait(rung)
		probes = append(probes, p)
		if p.invalid {
			// Validity is not a matter of pressure: the whole control is
			// meaningless if the instrument stopped working mid-run.
			t.Fatalf("%s: %s", p.pressure.name, p.rejected)
		}
		if p.measured > p.window {
			t.Fatalf("%s: measured wait %s exceeds the %s window: the delta is not bounded by the window",
				p.pressure.name, p.measured, p.window)
		}
		if p.measured > bestWait {
			bestWait = p.measured
		}
		t.Logf("%s: window %s, %s", p.pressure.name, p.window, p.detail())
	}

	if bestWait <= 0 {
		var b strings.Builder
		b.WriteString("no runqueue wait measured on any rung of the pressure ladder: the sample is inert\n")
		for _, p := range probes {
			fmt.Fprintf(&b, "  %s (window %s): %s\n", p.pressure.name, p.window, p.detail())
		}
		t.Fatal(b.String())
	}
}

// TestThreadWaitSnapshotDegradesToNothing proves the best-effort contract: with
// schedstat unreadable, the sample is invalid and every consumer sees 0 wait.
func TestThreadWaitSnapshotDegradesToNothing(t *testing.T) {
	prev := threadSelfPath
	threadSelfPath = "/nonexistent-qa-trouble-030"
	defer func() { threadSelfPath = prev }()

	w := ThreadWaitSnapshot()
	if w.OK {
		t.Fatalf("a snapshot from an unreadable path reported OK: %+v", w)
	}
	if got := w.RunqueueWaitSince(ThreadWait{Task: "t", WaitNS: 1, OK: true}, time.Second); got != 0 {
		t.Fatalf("an unmeasurable sample produced a %s discount, want 0", got)
	}
}
