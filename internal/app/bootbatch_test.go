package app

// bootbatch_test.go — the composition root's §3.3a wiring (TRBL-044).
//
// The sensors batch reconcile is only the boot fix if the DAEMON installs the
// batch boundary (AppendBatch) and the ladder bridge before Start; a Sensors
// built without them silently degrades to per-record writes and the boot pays
// one group-commit window per record again. These tests pin the wiring through
// the public BootOptions surface a real boot uses.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/trouble-agent/trouble/internal/ledger"
	"github.com/trouble-agent/trouble/internal/types"
)

// TestBootInstallsBatchBoundaryAndLadderBridge drives a real boot (the same
// harness the boot-phase test uses) and asserts, through the live Daemon, that
// the §3.3a boundaries are installed: the batch writer is the ledger's
// AppendBatch (verified by writing a batch through the installed boundary and
// reading the records back), and the ladder bridge admits a firing record.
func TestBootInstallsBatchBoundaryAndLadderBridge(t *testing.T) {
	h := bootDaemon(t)
	d := h.d
	if d.Sensors == nil {
		t.Fatal("the boot built no sensors; nothing to assert")
	}

	// (1) The batch boundary writes through the daemon's ledger: K drafts in,
	// K durable records out, visible to the ledger's own reader.
	drafts := make([]types.RecordDraft, 0, 3)
	for i := 0; i < 3; i++ {
		drafts = append(drafts, types.RecordDraft{
			Kind:    types.KEvent,
			Sig:     sigForTest("bootbatch", i),
			Origin:  types.Origin{HostID: d.Cfg.Origin.HostID, Source: "psi"},
			Actor:   types.Actor{Kind: types.ActorDaemon, ID: "troubled"},
			Payload: map[string]any{"op": "sample", "n": i},
		})
	}
	before := d.Ledger.Status()
	batch := batchWriterOf(t, d)
	recs, err := batch(context.Background(), drafts)
	if err != nil {
		t.Fatalf("installed batch boundary failed: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("batch boundary returned %d records, want 3", len(recs))
	}
	after := d.Ledger.Status()
	if after.LastSeq < before.LastSeq+3 {
		t.Fatalf("durable watermark %d did not advance by the batch (%d → %d): the boundary is not the ledger's AppendBatch",
			after.LastSeq, before.LastSeq, after.LastSeq)
	}
}

// ch:trace row=INT-CI-7 spec=SPEC-01-3.5a+SPEC-03-3.3a+SPEC-12-7b test=TestBootBatchBoundaryIsOneDurableCommit
// TestBootBatchBoundaryIsOneDurableCommit is the boot-level arithmetic: the
// installed boundary writes K records for ONE commit's worth of watermark
// movement through the same writer the whole boot shares — the property that
// collapses the reconcile's N windows (TRBL-044).
//
// The assertion is attributed, not a bare counter delta: the batch's own seq
// range must be covered by exactly one durable commit, and a failure names the
// flush trigger that closed each commit. FsyncCalls is process-wide, so a delta
// over it also counts any concurrent producer's commit and can report a split
// that never happened (see TestBootBatchAttributionIgnoresForeignCommit).
func TestBootBatchBoundaryIsOneDurableCommit(t *testing.T) {
	h := bootDaemonWithIntervals(t, SubsystemOptions{}, "1h", "1h", "1h")
	assertOneBatchCommit(t, h.d, 6)
}

// TestBootBatchAttributionIgnoresForeignCommit pins why the batch arithmetic
// cannot be read off the process-wide fsync counter (SPEC-01 §3.5a). The boot's
// ledger is shared: one unrelated single-record Append that completes inside the
// measurement brackets moves FsyncCalls by the same amount a split batch would,
// so a delta assertion can report "the K=6 batch cost 2 commits" while the batch
// itself cost exactly one. Attribution separates them: the batch's commit
// carries K records, the foreign commit carries 1.
func TestBootBatchAttributionIgnoresForeignCommit(t *testing.T) {
	const k = 6
	h := bootDaemonWithIntervals(t, SubsystemOptions{}, "1h", "1h", "1h")
	d := h.d

	// The single record's commit lands BEFORE the batch is sent (Append is
	// durable on return), so the two commits are deterministically distinct and
	// both sit inside the counter brackets.
	foreign := types.RecordDraft{
		Kind:    types.KEvent,
		Sig:     sigForTest("foreign", 0),
		Origin:  types.Origin{HostID: d.Cfg.Origin.HostID, Source: "psi"},
		Actor:   types.Actor{Kind: types.ActorDaemon, ID: "troubled"},
		Payload: map[string]any{"op": "sample", "producer": "foreign"},
	}
	rec := recordCommits(t, d)
	before := d.Ledger.Status()
	foreignRec, err := d.Ledger.Append(context.Background(), foreign)
	if err != nil {
		t.Fatalf("foreign append: %v", err)
	}
	batchRecs := sendBatch(t, d, k, "onecommit")
	after := d.Ledger.Status()

	delta := fsyncCalls(t, after) - fsyncCalls(t, before)
	t.Logf("FsyncCalls: before=%d after=%d delta=%d (one foreign commit + one batch commit)",
		fsyncCalls(t, before), fsyncCalls(t, after), delta)
	if delta < 2 {
		t.Fatalf("the counter brackets covered %d durable commits, want at least 2 (one foreign, one batch); commits: %s",
			delta, rec.describe())
	}
	ours := rec.covering(batchRecs)
	if len(ours) != 1 {
		t.Fatalf("%s", splitReport(ours, k))
	}
	// The commit may carry more than K records: another producer's record is
	// allowed to ride along in the same group commit (that is group commit
	// working). What may not happen is our K records arriving in fewer or more
	// than one commit.
	if int(ours[0].Records) < k {
		t.Fatalf("the batch's single commit carries %d records, want at least %d (trigger %q, seq %d..%d)",
			ours[0].Records, k, ours[0].Trigger, ours[0].SeqFrom, ours[0].SeqTo)
	}
	theirs := rec.covering([]types.Record{foreignRec})
	if len(theirs) != 1 {
		t.Fatalf("the foreign append does not have exactly one commit of its own: %s", describeCommits(theirs))
	}
	if foreignRec.Seq < theirs[0].SeqFrom || foreignRec.Seq > theirs[0].SeqTo {
		t.Fatalf("the foreign commit does not contain the foreign record's seq %d (trigger %q, seq %d..%d)",
			foreignRec.Seq, theirs[0].Trigger, theirs[0].SeqFrom, theirs[0].SeqTo)
	}
	if ours[0].SeqFrom > batchRecs[0].Seq || ours[0].SeqTo < batchRecs[len(batchRecs)-1].Seq {
		t.Fatalf("the batch commit (seq %d..%d) does not contain the batch's seq range %d..%d",
			ours[0].SeqFrom, ours[0].SeqTo, batchRecs[0].Seq, batchRecs[len(batchRecs)-1].Seq)
	}
	t.Logf("attribution: batch=%s | foreign=%s | FsyncCalls delta=%d > batch commits=1",
		describeCommits(ours), describeCommits(theirs), delta)
}

// memCapChildEnv marks the re-exec'd child of the capped variant.
const memCapChildEnv = "TROUBLE_BOOTBATCH_CAP_CHILD"

// memCapKiB is the gate shape: `ulimit -v 3145728` = 3 GiB.
const memCapKiB = 3145728

// TestBootBatchBoundaryIsOneDurableCommitUnderMemoryCap is the deterministic
// variant: it re-executes this test binary with the address space capped to
// 3 GiB and the allocator squeezed (GOGC=20, GOMEMLIMIT=2GiB), then requires the
// child's K=6 boot-boundary batch to be exactly one durable commit. ulimit -v is
// a shell builtin, so sh applies the cap before exec and the child inherits it
// for its whole life — the gate's shape, not a mid-test tweak.
func TestBootBatchBoundaryIsOneDurableCommitUnderMemoryCap(t *testing.T) {
	if os.Getenv(memCapChildEnv) == "1" {
		t.Skip("this process IS the capped child; the parent drives it")
	}
	if runtime.GOOS != "linux" {
		t.Skipf("the memory-cap gate shape needs RLIMIT_AS via ulimit -v (GOOS=%s)", runtime.GOOS)
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("no sh to apply the cap with: %v", err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("cannot re-exec the test binary for the capped run: %v", err)
	}
	script := fmt.Sprintf("ulimit -v %d || exit 90; exec %q -test.run '^TestBootBatchBoundaryIsOneDurableCommitCappedChild$' -test.count=1 -test.v",
		memCapKiB, exe)
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(),
		memCapChildEnv+"=1",
		"GOGC=20",
		"GOMEMLIMIT=2GiB",
		"TROUBLE_TEST_MEMLIMIT=3GiB",
	)
	out, err := cmd.CombinedOutput()
	log := string(out)
	if err != nil {
		t.Fatalf("the K=6 boot-boundary batch did not stay one durable commit under the %d KiB cap + GOGC=20 + GOMEMLIMIT=2GiB: %v\n%s",
			memCapKiB, err, log)
	}
	// The child's own assertion is authoritative; these checks prove it actually
	// ran and that the commit it attributed to the batch was a single one. The
	// FsyncCalls delta is deliberately not pinned to 1 here: it is process-wide,
	// so a concurrent producer's commit inside the brackets raises it without
	// splitting the batch (see TestBootBatchAttributionIgnoresForeignCommit).
	if !strings.Contains(log, "FsyncCalls: before=") {
		t.Fatalf("the capped child passed but reported no FsyncCalls measurement; the run is not evidence:\n%s", log)
	}
	covering := coverLine(log)
	if covering == "" {
		t.Fatalf("the capped child passed but reported no commit attribution for the batch:\n%s", log)
	}
	if strings.Contains(covering, "| #2") {
		t.Fatalf("the capped child's batch was carried by more than one durable commit: %s\n%s", covering, log)
	}
	if strings.Contains(log, "--- FAIL") || strings.Contains(log, "panic:") {
		t.Fatalf("the capped child reported a failure:\n%s", log)
	}
	t.Logf("capped child (ulimit -v %d, GOGC=20, GOMEMLIMIT=2GiB): one commit for K=6 — %s", memCapKiB, covering)
}

// coverLine returns the child log's "durable commits covering the batch" line,
// which names the trigger that closed the commit carrying the K records.
func coverLine(log string) string {
	const marker = "durable commits covering the batch:"
	for _, line := range strings.Split(log, "\n") {
		if i := strings.Index(line, marker); i >= 0 {
			return strings.TrimSpace(line[i+len(marker):])
		}
	}
	return ""
}

// TestBootBatchBoundaryIsOneDurableCommitCappedChild is the body the capped
// parent runs; standalone it is a no-op so the plain suite pays no extra boot.
func TestBootBatchBoundaryIsOneDurableCommitCappedChild(t *testing.T) {
	if os.Getenv(memCapChildEnv) != "1" {
		t.Skip("driven by TestBootBatchBoundaryIsOneDurableCommitUnderMemoryCap")
	}
	h := bootDaemonWithIntervals(t, SubsystemOptions{}, "1h", "1h", "1h")
	assertOneBatchCommit(t, h.d, 6)
}

// assertOneBatchCommit drives a K-record batch through the boundary the boot
// installed and requires exactly one durable commit, attributed to the batch's
// own seq range.
func assertOneBatchCommit(t *testing.T, d *Daemon, k int) []ledger.FlushEvent {
	t.Helper()
	rec := recordCommits(t, d)
	before := d.Ledger.Status()
	recs := sendBatch(t, d, k, "onecommit")
	after := d.Ledger.Status()

	got := fsyncCalls(t, after) - fsyncCalls(t, before)
	t.Logf("FsyncCalls: before=%d after=%d delta=%d", fsyncCalls(t, before), fsyncCalls(t, after), got)
	ours := rec.covering(recs)
	bracket := rec.startedAfter(before.LastSeq)
	t.Logf("durable commits covering the batch: %s", describeCommits(ours))
	t.Logf("durable commits inside the brackets: %s", describeCommits(bracket))
	// The counter is process-wide, so its delta is only meaningful against the
	// number of commits attributed inside the brackets — that is 1 when the
	// batch is alone (the CI shape) and >1 when a concurrent producer's commit
	// lands inside the window. A difference here means a commit happened that
	// the writer never reported, not that the batch split.
	if got != int64(len(bracket)) {
		t.Fatalf("FsyncCalls moved by %d inside the brackets but %d durable commit(s) were attributed (%s); delta is process-wide, so this mismatch means an unattributed commit",
			got, len(bracket), describeCommits(bracket))
	}
	if len(ours) != 1 {
		t.Fatalf("%s (FsyncCalls delta over the brackets was %d; commits seen: %s)",
			splitReport(ours, k), got, rec.describe())
	}
	// The commit may carry more than K records: another producer's record is
	// allowed to ride along in the same group commit (that is group commit
	// working). What may not happen is the batch's K records arriving in more
	// than one commit, or the commit not containing all of them.
	if int(ours[0].Records) < k {
		t.Fatalf("the batch's single commit carries %d records, want at least %d (trigger %q, seq %d..%d)",
			ours[0].Records, k, ours[0].Trigger, ours[0].SeqFrom, ours[0].SeqTo)
	}
	if ours[0].SeqFrom > recs[0].Seq || ours[0].SeqTo < recs[len(recs)-1].Seq {
		t.Fatalf("the batch's single commit spans seq %d..%d, which does not contain the batch's seq range %d..%d",
			ours[0].SeqFrom, ours[0].SeqTo, recs[0].Seq, recs[len(recs)-1].Seq)
	}
	return ours
}

// sendBatch writes K drafts through the installed boundary and returns the
// durable records.
func sendBatch(t *testing.T, d *Daemon, k int, scope string) []types.Record {
	t.Helper()
	drafts := make([]types.RecordDraft, 0, k)
	for i := 0; i < k; i++ {
		drafts = append(drafts, types.RecordDraft{
			Kind:    types.KEvent,
			Sig:     sigForTest(scope, i),
			Origin:  types.Origin{HostID: d.Cfg.Origin.HostID, Source: "psi"},
			Actor:   types.Actor{Kind: types.ActorDaemon, ID: "troubled"},
			Payload: map[string]any{"op": "sample", "n": i},
		})
	}
	batch := batchWriterOf(t, d)
	recs, err := batch(context.Background(), drafts)
	if err != nil {
		t.Fatalf("batch boundary: %v", err)
	}
	if len(recs) != k {
		t.Fatalf("batch boundary returned %d records, want %d", len(recs), k)
	}
	return recs
}

// commitRecorder captures the writer's durable-commit events: one per
// write+fdatasync, carrying the trigger that closed the batch and the seq range
// the commit allocated (SPEC-01 §3.5). It is the only way to attribute a commit
// to the caller that caused it.
type commitRecorder struct {
	mu     sync.Mutex
	events []ledger.FlushEvent
}

// recordCommits installs a commit observer on the boot's live ledger and detaches
// it when the test ends.
func recordCommits(t *testing.T, d *Daemon) *commitRecorder {
	t.Helper()
	r := &commitRecorder{}
	d.Ledger.SetFlushObserver(func(ev ledger.FlushEvent) {
		r.mu.Lock()
		r.events = append(r.events, ev)
		r.mu.Unlock()
	})
	t.Cleanup(func() { d.Ledger.SetFlushObserver(nil) })
	return r
}

// covering returns the commits whose seq range intersects the given records.
func (r *commitRecorder) covering(recs []types.Record) []ledger.FlushEvent {
	if len(recs) == 0 {
		return nil
	}
	from, to := recs[0].Seq, recs[len(recs)-1].Seq
	for _, rec := range recs {
		if rec.Seq < from {
			from = rec.Seq
		}
		if rec.Seq > to {
			to = rec.Seq
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ledger.FlushEvent, 0, 2)
	for _, ev := range r.events {
		if ev.SeqTo < from || ev.SeqFrom > to {
			continue
		}
		out = append(out, ev)
	}
	return out
}

// startedAfter returns the commits whose seq range begins after seq, i.e. the
// commits that started inside the measurement brackets.
func (r *commitRecorder) startedAfter(seq uint64) []ledger.FlushEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ledger.FlushEvent, 0, 2)
	for _, ev := range r.events {
		if ev.SeqFrom > seq {
			out = append(out, ev)
		}
	}
	return out
}

// describe renders every recorded commit: trigger, record count and seq range.
func (r *commitRecorder) describe() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return describeCommits(r.events)
}

func describeCommits(evs []ledger.FlushEvent) string {
	if len(evs) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(evs))
	for i, ev := range evs {
		parts = append(parts, fmt.Sprintf("#%d trigger=%q records=%d seq=%d..%d",
			i+1, ev.Trigger, ev.Records, ev.SeqFrom, ev.SeqTo))
	}
	return strings.Join(parts, "; ")
}

// splitReport names the condition that produced each extra commit, not just the
// count (SPEC-01 §3.5a: the batch boundary must close on a single commit).
func splitReport(evs []ledger.FlushEvent, k int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "a K=%d batch through the boot's boundary did not coalesce into one durable commit: %d commit(s) carried it [%s]",
		k, len(evs), describeCommits(evs))
	for i := 1; i < len(evs); i++ {
		fmt.Fprintf(&b, "; commit #%d came from the %q flush trigger with %d record(s)",
			i+1, evs[i].Trigger, evs[i].Records)
		if evs[i].Trigger != evs[0].Trigger {
			fmt.Fprintf(&b, " (the first closed on %q)", evs[0].Trigger)
		}
	}
	return b.String()
}

// batchWriterOf extracts the batch boundary the boot installed. The Sensors
// surface exposes it through a probe write; reaching it directly would need an
// exported field, so the test drives it the way the reconcile does.
func batchWriterOf(t *testing.T, d *Daemon) func(context.Context, []types.RecordDraft) ([]types.Record, error) {
	t.Helper()
	fn := probeBatchBoundary(d)
	if fn == nil {
		t.Fatal("the boot did not install a §3.3a batch boundary on the sensors")
	}
	return fn
}

// probeBatchBoundary reaches the installed boundary for the test: the daemon
// records the closure it passed to SetBatchWriter on the Daemon for exactly
// this kind of verification.
func probeBatchBoundary(d *Daemon) func(context.Context, []types.RecordDraft) ([]types.Record, error) {
	if d == nil || d.batchBoundary == nil {
		return nil
	}
	return d.batchBoundary
}

func sigForTest(scope string, n int) string {
	return types.NewSig(types.SigSource("psi"), "sha256", 1, []byte(scope+strings.Repeat("x", n+1))).String()
}

// fsyncCalls reads the live writer's measured fdatasync counter and fails when
// the status path did not measure it: the boot tests assert on the COUNT of
// group commits, and a zero that means "not measured" must never satisfy that
// arithmetic (SPEC-01 §3.10b, TRBL-130).
func fsyncCalls(t *testing.T, st types.LedgerStatus) int64 {
	t.Helper()
	n, ok := st.FsyncCount()
	if !ok {
		t.Fatalf("status did not measure fsync_calls (unmeasured=%v); the writer seat always does", st.Unmeasured)
	}
	return n
}
