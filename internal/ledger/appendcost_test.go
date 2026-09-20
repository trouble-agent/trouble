package ledger

// appendcost_test.go — the cost model behind SPEC-12 §7b (TRBL-025).
//
// The daemon's boot writes one ledger record per refused subsystem, per
// capability, and per failed systemd unit the SPEC-03 §4 startup reconcile
// finds. Those writers are SEQUENTIAL, and the ledger's group commit coalesces
// the appends that share a window — so a serial producer gets no batching at
// all: each `Append` waits for the next window to close, and the stall is one
// `ledger.fsync_window_ms` per record. That is the arithmetic that makes a
// boot's READY latency linear in the number of records it writes, which is the
// whole of §7b's measured table.
//
// The test pins the model, not the clock: N sequential appends must produce N
// durable commits (a batched implementation would produce fewer, and the note
// under the assertion says what to do when that becomes true) and must take at
// least a fraction of N windows. The measured cost is logged with the production
// window substituted, because that is the number a budget is derived from.

import (
	"fmt"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// TestSequentialAppendsCostOneGroupCommitWindowEach is the serial-producer half
// of SPEC-01 §3.5's group commit: a producer that waits for each Append to
// return before issuing the next one can never share a window, so its cost is
// records × window.
func TestSequentialAppendsCostOneGroupCommitWindowEach(t *testing.T) {
	const (
		appends = 5
		window  = 40 * time.Millisecond
	)
	clk := newFakeClock(time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC))
	l := testLedger(t, clk, mutOpts(func(o *Options) { o.Rotation.FsyncWindowMS = int(window / time.Millisecond) }))

	before := l.Status()
	start := time.Now()
	for i := 0; i < appends; i++ {
		// A distinct sig per record: the core dedups on it, so a repeated sig
		// would not be five records.
		sig := types.NewSig(types.SigSource("psi"), "sha256", 1, []byte(fmt.Sprintf("cost-model-digest-%02d", i))).String()
		mustAppend(t, l, eventDraft("psi", sig, "", 0))
	}
	elapsed := time.Since(start)
	after := l.Status()

	// Each record is its own batch: no batching happened, because nothing else
	// was ever in flight. If a future change makes the writer flush a serial
	// producer immediately (or lets Appends share a window), this count drops —
	// that is a real improvement, but it invalidates the budget arithmetic in
	// SPEC-12 §7b, so re-derive the READY budget in the same change.
	if got := after.FsyncCalls - before.FsyncCalls; got != appends {
		t.Fatalf("%d sequential appends produced %d durable commits, want %d: the boot's cost model "+
			"(SPEC-12 §7b) assumes one group-commit window per record on the READY path; if this changed "+
			"deliberately, re-derive the READY budget with it", appends, got, appends)
	}
	if got := after.Records - before.Records; got != appends {
		t.Fatalf("%d appends advanced the record count by %d, want %d", appends, got, appends)
	}

	// The window is a floor, not a ceiling: a flush cannot happen before the
	// timer fires, so a serial producer's floor is (N-1) windows (the first
	// append may catch a window already armed by the writer's start).
	if floor := time.Duration(appends-1) * window; elapsed < floor {
		t.Fatalf("%d sequential appends took %s, below the %s floor of %d windows: the writer is batching a "+
			"serial producer, which contradicts SPEC-01 §3.5's group commit", appends, elapsed, floor, appends-1)
	}

	perAppend := elapsed / appends
	t.Logf("measured: %d sequential appends in %s (%s each) at a %s window",
		appends, elapsed.Round(time.Millisecond), perAppend.Round(time.Millisecond), window)
	production := time.Duration(DefaultFsyncWindowMS) * time.Millisecond
	t.Logf("production (%dms window): a boot that writes R records sequentially pays about R × %s — "+
		"the §7b table's ~16 s at ~80 records and ~150 ms of device sync latency per window",
		DefaultFsyncWindowMS, production)
}
