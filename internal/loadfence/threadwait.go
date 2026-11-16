package loadfence

import (
	"os"
	"time"
)

// —— per-rule descheduling fence (QA-TROUBLE-030) ——
//
// The terms above price a TEST GATE: a Profile prices the host, SuiteWaitFraction
// prices the suite's own pressure. The scrub engine needs the same kind of
// evidence for a PRODUCTION budget. scrub.rule_timeout is specified as the CPU
// time one rule evaluation may take, but it used to be enforced against a raw
// wall-clock reading, so an oversubscribed host fails every payload closed with
// TROUBLE-SCRUB-005 for a rule that costs microseconds. That gate lost tier 1 of
// four separate GitReins runs by itself (every other package green), e.g. at
// load_avg_1m 35.89:
//
//	TROUBLE-SCRUB-005 (rule_timeout): pii_email_ip: rule evaluation exceeded 250ms
//	TestPrefilterEquivalence input 959: pii_identity_kv: rule evaluation exceeded 250ms
//
// The fence is SUBTRACTIVE and per rule: charge the rule for elapsed-minus-the
// runqueue-wait-it-accumulated, so the operator-facing budget keeps its CPU-time
// meaning instead of being widened, and a rule that really does burn its budget
// still fails closed. The alternative shape — multiplying the budget by the
// host's measured contention the way Profile.Contention does — is strictly
// coarser here: it cannot tell this rule's descheduling from the payload's other
// seventeen rules', and any cap on the factor (SuiteContention caps at 2x) is a
// cliff a long stall falls off.
//
// ThreadWait is deliberately THREAD-scoped, unlike the process-wide sum
// SuiteWaitSamples returns. The budget belongs to one rule evaluation running on
// one thread: a process-wide sum would attribute every other ingestion
// goroutine's descheduling to this rule, and reading it (readdir over
// /proc/self/task plus one read per thread) costs orders of magnitude more than
// the single per-thread read below. Measured on a 16-core box at load_avg ~30:
// 1.1 µs for a pread on an already-open per-thread handle, 6.4 µs for a fresh
// open/read/close, against an entire §3.9 full-set 1 KiB scrub pass of ~130 µs.
// A handle cannot be cached instead of re-opened: /proc/thread-self resolves at
// OPEN time and keeps reporting that thread, so a cached handle reads a stale
// thread after a migration (measured: the on-CPU field did not move across 20M
// loop iterations once the goroutine had moved). That cost is why the scrub
// engine samples only when a rule has ALREADY blown its wall budget — see the
// engine's fence comment for the arithmetic that rules out per-rule sampling.

const (
	// ThreadSelfPath is the magic link that resolves to the CALLING thread's
	// /proc/<pid>/task/<tid> directory, so each sample describes exactly the
	// thread that took it.
	ThreadSelfPath = "/proc/thread-self"
)

// threadSelfPath is ThreadSelfPath behind a variable so the degrade path (no
// schedstat, a hidepid mount, a platform without /proc) can be proven without
// unmounting anything.
var threadSelfPath = ThreadSelfPath

// ThreadWait is one thread's cumulative runqueue wait, sampled from schedstat.
//
// schedstat field 2 is nanoseconds the thread sat runnable-but-not-running —
// exactly the time an OS scheduler took away from a measurement. Field 1 (time
// on CPU) is deliberately ignored: the budget this feeds is "wall time the OS
// did not take", which must still count the places a rule blocks voluntarily (a
// fixture that proves the fail-closed path sleeps inside the rule, and sleep is
// not runqueue wait).
//
// best-effort by contract, the same discipline as the suite term above: on a
// kernel without schedstat, under a hidepid mount, or on a platform without
// /proc, the sample has OK == false and every consumer degrades to "0 wait
// measured", i.e. to unmodified wall-clock behaviour. A fence that cannot
// measure must never tighten a budget.
//
// The fields are exported so a caller can carry, log or compare a sample without
// going through this package again; a consumer must still only compare samples
// whose Task matches, which is what RunqueueWaitSince enforces.
type ThreadWait struct {
	// Task is the resolved /proc/thread-self target ("/proc/<pid>/task/<tid>").
	// Two samples are only comparable when it matches: the counters are
	// per-thread, and a goroutine the scheduler migrated between the samples
	// would otherwise be charged another thread's lifetime of cumulative wait
	// as if it happened inside its own window.
	Task string
	// WaitNS is the thread's cumulative nanoseconds spent
	// runnable-but-not-running.
	WaitNS uint64
	// OK is false when nothing could be measured.
	OK bool
}

// ThreadWaitSnapshot samples the calling thread's runqueue wait. It costs one
// readlink and one small read of a procfs file; both are best-effort, and any
// failure yields an invalid sample rather than an error.
func ThreadWaitSnapshot() ThreadWait {
	// The link is resolved first so the identity in the sample is the thread
	// that was current when the window opened or closed; a migration in the
	// microseconds between the two calls makes the pair disagree, and
	// RunqueueWaitSince then reports no evidence — the safe direction.
	task, err := os.Readlink(threadSelfPath)
	if err != nil || task == "" {
		return ThreadWait{}
	}
	b, err := os.ReadFile(threadSelfPath + "/schedstat")
	if err != nil {
		return ThreadWait{}
	}
	wait, ok := parseSchedstatWait(b)
	if !ok {
		return ThreadWait{}
	}
	return ThreadWait{Task: task, WaitNS: wait, OK: true}
}

// RunqueueWaitSince reports how much of elapsed this thread spent descheduled
// between before and w, or 0 when the two samples cannot be compared. Every
// rejection is deliberate, and every one degrades to unmodified wall-clock
// behaviour rather than to a longer or a shorter budget:
//
//   - either sample invalid: nothing was measured (no schedstat);
//   - different task: the window straddles a goroutine migration, so the
//     counters describe two different threads;
//   - non-monotonic wait: the same thread but out of order (a tick artefact);
//   - wait longer than elapsed: one thread cannot be runnable-but-not-running
//     for longer than the window that contains that time, so the reading is
//     junk (it is also what a stale or cross-thread sample looks like), and
//     treating it as evidence would subtract more than the window has to give.
//
// The result is therefore always in [0, elapsed], which is what keeps the
// subtraction from ever charging a rule less than zero time.
func (w ThreadWait) RunqueueWaitSince(before ThreadWait, elapsed time.Duration) time.Duration {
	if !before.OK || !w.OK || elapsed <= 0 {
		return 0
	}
	if before.Task != w.Task {
		return 0
	}
	if w.WaitNS <= before.WaitNS {
		return 0
	}
	d := time.Duration(w.WaitNS - before.WaitNS)
	if d > elapsed {
		return 0
	}
	return d
}

// parseSchedstatWait extracts field 2 (runqueue wait, nanoseconds) from a
// schedstat line: "<time on cpu> <runqueue wait> <timeslices run>". It parses by
// hand rather than through strings.Fields because the values are only ever read
// on a rule path.
func parseSchedstatWait(b []byte) (uint64, bool) {
	separators := 0
	var v uint64
	seen := false
	for _, c := range b {
		switch {
		case c >= '0' && c <= '9':
			if v > 1<<40 {
				return 0, false // absurd for a nanosecond counter: unreadable
			}
			v = v*10 + uint64(c-'0')
			if separators == 1 {
				seen = true
			}
		case c == ' ' || c == '\t':
			if separators == 1 {
				if !seen {
					return 0, false
				}
				return v, true // start of field 3
			}
			separators++
			v = 0
		default: // '\n', '\r' or anything else
			if (c == '\n' || c == '\r') && separators == 1 && seen {
				return v, true // end of a two-field line
			}
			return 0, false
		}
	}
	if separators == 1 && seen {
		return v, true // no trailing separator
	}
	return 0, false
}
