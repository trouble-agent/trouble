package loadfence

import (
	"runtime"
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

// TestThreadWaitSnapshotMeasuresRealDescheduling is the positive control: with a
// single P and competitors ready to run, this goroutine's window MUST accumulate
// runqueue wait, and the sample must attribute it to this thread. Without it the
// fence could be silently inert (always 0) and every test above would still pass.
func TestThreadWaitSnapshotMeasuresRealDescheduling(t *testing.T) {
	prev := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(prev)

	first := ThreadWaitSnapshot()
	if !first.OK {
		t.Skipf("schedstat is unavailable on this host (%s); the fence degrades to 0 by contract", first.Task)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
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

	// The kernel accounts a goroutine's runqueue wait only while the thread
	// that carries it is actually handed off to a competitor, so a single
	// window on a quiet box can legitimately measure 0. Three windows make the
	// control about the instrument, not about one unlucky schedule.
	var measured, window time.Duration
	for attempt := 0; attempt < 3 && measured == 0; attempt++ {
		before := ThreadWaitSnapshot()
		start := time.Now()
		var x uint64
		for time.Since(start) < 40*time.Millisecond {
			x = x*6364136223846793005 + 1442695040888963407
		}
		window = time.Since(start)
		after := ThreadWaitSnapshot()
		atomic.AddUint64(&waitTestSink, x)
		if !before.OK || !after.OK {
			t.Fatalf("a sample became invalid on a host where the first was valid (%+v -> %+v)", before, after)
		}
		if after.Task != before.Task {
			continue // migrated: the fence rejects that pair by design
		}
		measured = after.RunqueueWaitSince(before, window)
	}
	close(stop)
	wg.Wait()

	if measured <= 0 {
		t.Fatalf("no runqueue wait measured across three %s windows with 3 competitors on 1 P: the sample is inert", window)
	}
	if measured > window {
		t.Fatalf("measured wait %s exceeds the %s window: the delta is not bounded by the window", measured, window)
	}
	t.Logf("window %s, runqueue wait %s (%.0f%% descheduled) on %s", window, measured, 100*float64(measured)/float64(window), first.Task)
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
