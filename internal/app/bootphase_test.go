package app

// bootphase_test.go — the per-phase attribution of a real boot (TRBL-025,
// SPEC-12 §7b).
//
// §7a's READY budget only ever BOUNDS "still booting"; it never said where the
// boot's time goes. TRBL-025 measured that, and the answer is not descheduling:
// the boot is a sequence of ~25 phases, all but one of which settle in well
// under a millisecond, and one of which (sensors_start — the SPEC-03 §4 startup
// reconcile of the D-Bus failure set) writes one ledger record per failed
// systemd unit. Every one of those records costs its own group-commit window
// (SPEC-01 §3.5), so the boot's READY latency is linear in the number of ledger
// records it writes, at one `ledger.fsync_window_ms` (200 ms default) apiece.
//
// This test is the tripwire for that shape. It asserts the phase table is
// COMPLETE and ORDERED (an unattributed boot is exactly what TRBL-025 was filed
// against) and, on a host whose boot really does write that many records, that
// the cost localizes in the one phase that can produce it. The causal assertion
// is gated on the measured record count because a host with a clean unit set
// has no such phase and would red for a reason that is not a regression.

import (
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// bootPhaseMark is one observed §4.1 phase boundary.
type bootPhaseMark struct {
	name string
	at   time.Time
}

// bootPhaseObserver, when non-nil, receives the phase marks of every boot the
// test harness starts (bootDaemonWith). Nil for every other test in the package,
// so the ordinary boot costs one branch per phase.
var bootPhaseObserver func(name string, at time.Time)

// phaseNames renders a phase sequence for a failure message.
func phaseNames(marks []bootPhaseMark) string {
	parts := make([]string, 0, len(marks))
	for _, m := range marks {
		parts = append(parts, m.name)
	}
	return strings.Join(parts, " → ")
}

// TestBootPhaseTableAttributesTheReadyLatency boots the daemon through the
// shipped compiled defaults with BootOptions.OnBootPhase attached and asserts
// the §4.1 startup sequence is fully attributable: every phase named, in order,
// the last one being `ready`.
func TestBootPhaseTableAttributesTheReadyLatency(t *testing.T) {
	var mu sync.Mutex
	var marks []bootPhaseMark
	bootPhaseObserver = func(name string, at time.Time) {
		mu.Lock()
		marks = append(marks, bootPhaseMark{name, at})
		mu.Unlock()
	}
	t.Cleanup(func() { bootPhaseObserver = nil })

	h := bootDaemon(t)
	mu.Lock()
	seen := append([]bootPhaseMark(nil), marks...)
	mu.Unlock()

	// (1) The observed sequence IS the published one: same names, same order,
	// every name exactly once. A boot step added or reordered without updating
	// bootPhaseNames fails here, which is the point — an unlisted phase is a
	// phase no report can attribute.
	want := BootPhaseNames()
	if len(seen) != len(want) {
		t.Fatalf("boot reported %d phases, want %d: %s", len(seen), len(want), phaseNames(seen))
	}
	for i := range want {
		if seen[i].name != want[i] {
			t.Fatalf("phase %d = %q, want %q (got %s)", i, seen[i].name, want[i], phaseNames(seen))
		}
	}
	if seen[len(seen)-1].name != "ready" {
		t.Fatalf("last phase = %q, want ready: the table must run to READY", seen[len(seen)-1].name)
	}

	// (2) Marks are monotone and span a positive interval.
	var total time.Duration
	for i := 1; i < len(seen); i++ {
		if d := seen[i].at.Sub(seen[i-1].at); d < 0 {
			t.Fatalf("phase %q begins before %q: the boot sequence is not monotone", seen[i].name, seen[i-1].name)
		}
		total += seen[i].at.Sub(seen[i-1].at)
	}
	if total <= 0 {
		t.Fatalf("the phase table spans %s; the marks carry no duration", total)
	}

	// (3) The table is the deliverable: log every phase with its measured cost.
	// A mark records when a phase BEGINS, so the phase that ran between two
	// consecutive marks is the EARLIER one — the table is labeled that way, and
	// the last mark (`ready`) closes the sequence rather than opening a phase.
	st := h.d.Ledger.Status()
	t.Logf("boot-to-READY %s attributed over %d phases (load_avg %.2f / %d cores)",
		total.Round(time.Millisecond), len(seen)-1, hostLoadAvg1(), runtime.NumCPU())
	var widest, second time.Duration
	var widestName, secondName string
	for i := 1; i < len(seen); i++ {
		d := seen[i].at.Sub(seen[i-1].at)
		name := seen[i-1].name
		if d >= widest {
			second, secondName = widest, widestName
			widest, widestName = d, name
		} else if d > second {
			second, secondName = d, name
		}
		t.Logf("    %-18s %10s", name, d.Round(time.Millisecond))
	}
	t.Logf("ledger: %d records on the READY path in %d durable commits (%.2f commits/record, loss window %dms)",
		st.Records, st.FsyncCalls, st.FsyncPerRecord, st.LossWindowMS)

	// (4) The measured cause: on a host whose boot writes enough ledger records
	// for the durability term to dominate, that cost must sit in the one phase
	// that emits them — SPEC-03 §4's startup reconcile — and nowhere else. On a
	// host with almost no failed units the boot writes a handful of records and
	// this assertion would be measuring nothing, so it is skipped and said so.
	const recordsForTheCausalClaim = 20
	if st.Records < recordsForTheCausalClaim {
		t.Logf("only %d ledger records on this boot: the durability term is too small to localize, so the "+
			"causal assertion is skipped (SPEC-12 §7b explains why the boot's cost is linear in this count)",
			st.Records)
		return
	}
	if widestName != "sensors_start" {
		t.Fatalf("the widest phase is %q (%s), want sensors_start: the boot's cost is the SPEC-03 §4 startup "+
			"reconcile's ledger appends, so a phase other than sensors_start dominating means a new wait entered "+
			"the READY path (SPEC-12 §7b)", widestName, widest.Round(time.Millisecond))
	}
	ratio := 0.0
	if second > 0 {
		ratio = float64(widest) / float64(second)
	}
	t.Logf("localized: sensors_start %s is the widest phase, %.1fx the next (%s %s) — %d records at one "+
		"group-commit window each is this boot's cost model",
		widest.Round(time.Millisecond), ratio, secondName, second.Round(time.Millisecond), st.Records)
}
