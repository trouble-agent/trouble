package app

// bootphase_test.go — the per-phase attribution of a real boot (TRBL-025,
// SPEC-12 §7b).
//
// §7a's READY budget only ever BOUNDS "still booting"; it never said where the
// boot's time goes. TRBL-025 measured that, and the answer was a sequence of
// ~25 phases, all but one settling under a millisecond, plus one phase
// (sensors_start — the SPEC-03 §4 startup reconcile) whose ledger records each
// paid their own group-commit window: READY latency was linear in the records
// the boot wrote, at one ledger.fsync_window_ms apiece.
//
// TRBL-044 changed that cost model on purpose (§3.3a batch reconcile + §3.5a
// batch append): the reconcile's records now land in ONE durable batch, so the
// boot's durability term is windows × latency, not records × latency. This
// test still asserts the phase table is COMPLETE and ORDERED (an
// unattributed boot is exactly what TRBL-025 was filed against) and now
// asserts the batching is engaged on a boot that writes enough records.

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

	// (4) The measured cause: TRBL-044 (§3.3a batch reconcile + §3.5a batch
	// append) collapsed the reconcile's N sequential durable commits into
	// ceil(N/K) batch commits, so the boot's cost model changed: the durability
	// term is no longer records × (window + device sync latency) but roughly
	// windows × (window + latency), with windows ≈ durable commits. The table
	// still attributes every phase, and on a boot that writes enough records
	// the ledger accounting is asserted: commits must be well below the record
	// count on a boot whose reconcile found failed units (the batching IS the
	// fix), with the measured numbers logged for the §7b table. On a host with
	// almost no failed units the causal half is skipped and said so.
	const recordsForTheCausalClaim = 20
	if st.Records < recordsForTheCausalClaim {
		t.Logf("only %d ledger records on this boot: the durability term is too small to localize, so the "+
			"causal assertion is skipped (SPEC-12 §7b explains the boot's cost model)",
			st.Records)
		return
	}
	if st.FsyncCalls >= st.Records {
		t.Fatalf("%d records cost %d durable commits — the batch reconcile (§3.3a) is not engaged: "+
			"a boot whose reconcile writes failed-unit records must batch them (TRBL-044), or it pays one "+
			"group-commit window per record again", st.Records, st.FsyncCalls)
	}
	ratio := 0.0
	if second > 0 {
		ratio = float64(widest) / float64(second)
	}
	t.Logf("batched: %d records in %d durable commits (%.2f commits/record) — the §3.3a batch reconcile "+
		"collapsed the READY-path cost model; widest phase %s %s, %.1fx the next (%s %s)",
		st.Records, st.FsyncCalls, st.FsyncPerRecord,
		widestName, widest.Round(time.Millisecond), ratio, secondName, second.Round(time.Millisecond))
}
