package ladder

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// lease_test.go is the SPEC-05 §7 row for AC-5 (the host agent lease, §3.6) and
// holds the package's shared test scaffolding: mintID, adoptIncident, admitFresh,
// withPIDAlive and the two stage drivers the park and gate rows reuse.
//
// Every collaborator is in-process and the clock is fake: no test opens a socket,
// spawns a subprocess or waits on a real clock.
//
// Why the scaffolding exists. Two collaborators outside this file cannot carry a
// row that needs more than one incident at a time:
//
//   - types.NewID mints the same id for every call inside one millisecond (1000
//     calls in a tight loop return one value; only the millisecond component
//     varies), so two incidents admitted back to back share an id, the second
//     overwrites the first in Ladder.incs, and Park's rows collapse into a single
//     parkRecs entry (that map is keyed by the park record's own id);
//   - the shared obsFor pins one subject, so two observations derive one inKey
//     and the second merges into the first incident (§3.9), and its event id
//     turns the second arrival into a replay of the first (Admit then answers the
//     id of a sig that has no open incident, i.e. the empty id).
//
// mintID names the ids these rows need, and the rows register their incidents
// through AdoptIncident and their park rows through InjectPark — the same seams a
// boot from the ledger uses (INV-2: the ledger is authoritative, the index is
// rebuilt from it). The admission path itself is covered by the transition-table
// suite and dedup_test.go. Both defects are reported with this task; when
// types.NewID is fixed this scaffolding keeps working, it simply stops being load
// bearing.

var (
	testIDMu  sync.Mutex
	testIDSeq int64
)

// mintID returns a ULID-shaped id with the given prefix, unique within a test
// binary run.
func mintID(prefix string) string {
	testIDMu.Lock()
	testIDSeq++
	n := testIDSeq
	testIDMu.Unlock()
	return prefix + fmt.Sprintf("%026d", n)
}

// adoptIncident registers one incident in the state a row needs, under an id this
// test owns, and returns it.
func adoptIncident(t *testing.T, h *harness, name, rule string, state types.LadderState, rung types.Rung) string {
	t.Helper()
	sig := sigFor(name).String()
	id := mintID("inc_")
	ts := types.FormatUTC(h.clock.Now())
	h.l.AdoptIncident(types.Incident{
		ID:        id,
		Sig:       sig,
		GroupID:   mintID("grp_"),
		State:     state,
		EntryRung: rung,
		Rung:      rung,
		Severity:  types.SevHigh,
		OpenedTS:  ts,
		UpdatedTS: ts,
		VerifyWin: h.l.cfg.VerifyDefault,
	}, []string{sig}, mintID("ink_"), rule, rung, rung)
	return id
}

// admitFresh admits one observation on its own arrival path and returns the
// incident id.
func admitFresh(t *testing.T, h *harness, name, rule string) string {
	t.Helper()
	obs := obsFor(sigFor(name), rule, "journald")
	obs.EventID = ""
	obs.Subject = "probe-" + name + ".service"
	obs.InKey = mintID("ink_")
	res := h.admit(t, obs)
	if res.Inc == "" {
		t.Fatalf("%s: admission returned no incident", name)
	}
	return res.Inc
}

// withPIDAlive installs the host process-liveness seam on a harness. The shared
// harness leaves Deps.PIDAlive nil, which the lease table and the re-adopter read
// as "every pid is dead"; both consult it through l.deps at call time, so a test
// installs its own answer here instead of rebuilding Deps.
func withPIDAlive(h *harness, alive func(pid int) bool) {
	h.l.deps.PIDAlive = alive
}

// driveToPlayCheck advances an incident to play:check_only and returns its state.
func driveToPlayCheck(t *testing.T, h *harness, inc string) *incState {
	t.Helper()
	ctx := context.Background()
	st := h.l.incStateFor(inc)
	if st == nil {
		t.Fatalf("incident %s is not in the index", inc)
	}
	for _, trig := range []string{"T03", "T08"} {
		if _, err := h.l.Advance(ctx, inc, Transition{Trigger: trig}); err != nil {
			t.Fatalf("incident %s: %s: %v", inc, trig, err)
		}
	}
	if st.Inc.State != types.StPlayCheck {
		t.Fatalf("incident %s is %q, want play:check_only", inc, string(st.Inc.State))
	}
	return st
}

// driveToVerifying advances an incident to verifying with a changed run summary.
func driveToVerifying(t *testing.T, h *harness, inc string) *incState {
	t.Helper()
	ctx := context.Background()
	st := driveToPlayCheck(t, h, inc)
	st.RunSummary = &RunSummary{Changed: true, TasksRun: 1, DiffSummary: "service.reload: restart the unit"}
	for _, trig := range []string{"T10", "T12"} {
		if _, err := h.l.Advance(ctx, inc, Transition{Trigger: trig}); err != nil {
			t.Fatalf("incident %s: %s: %v", inc, trig, err)
		}
	}
	if st.Inc.State != types.StVerifying {
		t.Fatalf("incident %s is %q, want verifying", inc, string(st.Inc.State))
	}
	return st
}

// TestLeaseContentionGrantsOneAndEnqueuesFirst is the §7 AC-5 row: eight
// goroutines acquire concurrently for eight different incidents → exactly one
// grant and seven TROUBLE-LADDER-003 (transient: a busy host is not a fault,
// contention never escalates, §3.6 step 4), and in every case the `requested`
// lease change precedes the `granted` one in LeaseHistory() — the enqueue-first
// handshake of §3.6 step 1.
func TestLeaseContentionGrantsOneAndEnqueuesFirst(t *testing.T) {
	const n = 8
	ctx := context.Background()
	h := newHarness(t, harnessOpts{
		rules: map[string]types.Rule{"rule-a": {Name: "rule-a", EntryRung: types.RungPlay}},
	})
	incs := make([]string, 0, n)
	for i := 0; i < n; i++ {
		incs = append(incs, adoptIncident(t, h, fmt.Sprintf("lease-contend-%d", i), "rule-a", types.StRecorded, types.RungPlay))
	}

	var (
		mu      sync.Mutex
		granted []types.AgentLease
		refused []error
	)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // all eight enter the handshake together
			lease, err := h.l.AcquireLease(ctx, incs[i], fmt.Sprintf("trouble-agent/%d", i), 4000+i, "")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				refused = append(refused, err)
				return
			}
			granted = append(granted, lease)
		}(i)
	}
	close(start)
	wg.Wait()

	if len(granted) != 1 {
		t.Fatalf("%d of %d concurrent acquisitions were granted, want exactly 1", len(granted), n)
	}
	if len(refused) != n-1 {
		t.Fatalf("%d acquisition(s) were refused, want %d", len(refused), n-1)
	}
	for _, err := range refused {
		if code := CodeOf(err); code != types.CodeLadder003 {
			t.Errorf("refusal code %q, want TROUBLE-LADDER-003 (%v)", code, err)
		}
		if cls := ClassOf(err); cls != types.ErrClassTransient {
			t.Errorf("TROUBLE-LADDER-003 class %q, want transient (the holder is requeued with lease_retry)", cls)
		}
	}
	if cur, ok := h.l.Lease(); !ok || cur.Holder != granted[0].Holder {
		t.Fatalf("the lease table holds %+v (%t), want the granted lease %s", cur, ok, granted[0].Holder)
	}
	if got := h.ledger.countPayload("lease_state", types.LeaseGranted); got != 1 {
		t.Errorf("%d granted lease transition record(s), want 1", got)
	}

	// Enqueue-first: the `requested` record is appended before any check, so it
	// precedes the `granted` record for that lease in every case — including the
	// seven requests that are never granted.
	hist := h.l.LeaseHistory()
	if len(hist) == 0 || hist[0].State != types.LeaseRequested {
		t.Fatalf("the first lease change is %+v, want a requested record", hist)
	}
	requested, grantedRecords := 0, 0
	seenRequest := 0
	for i, lz := range hist {
		switch lz.State {
		case types.LeaseRequested:
			requested++
			seenRequest++
		case types.LeaseGranted:
			grantedRecords++
			if seenRequest == 0 {
				t.Errorf("granted record at index %d has no requested record before it: the handshake is not enqueue-first", i)
			}
		default:
			t.Errorf("unexpected lease change %q at index %d", lz.State, i)
		}
	}
	if requested != n {
		t.Errorf("%d requested record(s) in the history, want %d (one per attempt)", requested, n)
	}
	if grantedRecords != 1 {
		t.Errorf("%d granted record(s) in the history, want 1", grantedRecords)
	}
}

// TestLeaseStaleHolderIsReclaimableAfterGrace is the §7 AC-5 row: a holder whose
// PID is not alive on this host is reclaimable only after `lease_grace` (90s
// simulated), and the reclaim is visible as a `reclaimed` lease change whose
// release_reason is `stale`.
func TestLeaseStaleHolderIsReclaimableAfterGrace(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, harnessOpts{
		rules: map[string]types.Rule{"rule-a": {Name: "rule-a", EntryRung: types.RungPlay}},
	})
	holder := adoptIncident(t, h, "lease-stale-holder", "rule-a", types.StRecorded, types.RungPlay)
	waiter := adoptIncident(t, h, "lease-stale-waiter", "rule-a", types.StRecorded, types.RungPlay)

	if _, err := h.l.AcquireLease(ctx, holder, "trouble-agent/holder", 4242, "/worktrees/holder"); err != nil {
		t.Fatalf("AcquireLease(holder): %v", err)
	}
	// The holder's process is gone (a killed run): the §3.6 "stale" condition.
	withPIDAlive(h, func(pid int) bool { return pid != 4242 })

	if _, err := h.l.AcquireLease(ctx, waiter, "trouble-agent/waiter", 0, ""); CodeOf(err) != types.CodeLadder003 {
		t.Fatalf("inside lease_grace the lease is still held: got %v, want TROUBLE-LADDER-003", err)
	}
	h.clock.advance(89 * time.Second)
	if _, err := h.l.AcquireLease(ctx, waiter, "trouble-agent/waiter", 0, ""); CodeOf(err) != types.CodeLadder003 {
		t.Fatalf("at 89s the grace window has not elapsed: got %v, want TROUBLE-LADDER-003", err)
	}
	h.clock.advance(2 * time.Second) // 91s > lease_grace (90s)

	reclaimed, err := h.l.AcquireLease(ctx, waiter, "trouble-agent/waiter", 0, "")
	if err != nil {
		t.Fatalf("AcquireLease after lease_grace: %v", err)
	}
	if reclaimed.Inc != waiter {
		t.Errorf("the reclaimed lease is held by %s, want %s", reclaimed.Inc, waiter)
	}
	if cur, ok := h.l.Lease(); !ok || cur.Inc != waiter {
		t.Errorf("the lease table holds %+v (%t), want the waiter %s", cur, ok, waiter)
	}
	reclaims := 0
	for _, lz := range h.l.LeaseHistory() {
		if lz.State != types.LeaseReclaimed {
			continue
		}
		reclaims++
		if lz.ReleaseReason != "stale" {
			t.Errorf("the reclaimed record's release_reason is %q, want stale", lz.ReleaseReason)
		}
		if lz.Inc != holder {
			t.Errorf("the reclaimed record names %s, want the stale holder %s", lz.Inc, holder)
		}
		if lz.PID != 4242 {
			t.Errorf("the reclaimed record's pid is %d, want 4242", lz.PID)
		}
	}
	if reclaims != 1 {
		t.Errorf("%d reclaimed lease change(s), want 1: history %v", reclaims, leaseStates(h.l.LeaseHistory()))
	}
}

// TestLeaseRenewalExtendsTheDeadline is the §7 AC-5 row: renewal extends
// expires_ts and increments renew_count (§3.6: every tool call and every
// lease_heartbeat), and only the current lease may be renewed or released.
func TestLeaseRenewalExtendsTheDeadline(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, harnessOpts{
		rules: map[string]types.Rule{"rule-a": {Name: "rule-a", EntryRung: types.RungPlay}},
	})
	inc := adoptIncident(t, h, "lease-renew", "rule-a", types.StRecorded, types.RungPlay)
	lease, err := h.l.AcquireLease(ctx, inc, "trouble-agent/renew", 0, "")
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	timeout, perr := time.ParseDuration(string(h.l.cfg.AgentTimeout))
	if perr != nil {
		t.Fatalf("agent_timeout %q: %v", h.l.cfg.AgentTimeout, perr)
	}
	first, perr := types.ParseUTC(lease.ExpiresTS)
	if perr != nil {
		t.Fatalf("expires_ts %q: %v", lease.ExpiresTS, perr)
	}
	if got := first.Sub(h.clock.Now()); got != timeout {
		t.Errorf("expires_ts is granted_ts+%s, want granted_ts+%s (agent_timeout)", got, timeout)
	}
	if lease.RenewCount != 0 {
		t.Errorf("a fresh lease has renew_count %d, want 0", lease.RenewCount)
	}

	h.clock.advance(5 * time.Minute)
	r1, err := h.l.RenewLease(ctx, lease.LeaseID)
	if err != nil {
		t.Fatalf("RenewLease: %v", err)
	}
	if r1.RenewCount != 1 {
		t.Errorf("renew_count = %d, want 1", r1.RenewCount)
	}
	if r1.State != types.LeaseRenewed {
		t.Errorf("lease state %q, want renewed", r1.State)
	}
	if r1.RenewedTS != types.FormatUTC(h.clock.Now()) {
		t.Errorf("renewed_ts = %q, want the clock's now %q", r1.RenewedTS, types.FormatUTC(h.clock.Now()))
	}
	second, perr := types.ParseUTC(r1.ExpiresTS)
	if perr != nil {
		t.Fatalf("expires_ts %q: %v", r1.ExpiresTS, perr)
	}
	if !second.After(first) {
		t.Errorf("renewal did not extend expires_ts: %s → %s", lease.ExpiresTS, r1.ExpiresTS)
	}
	if got := second.Sub(h.clock.Now()); got != timeout {
		t.Errorf("the renewed expires_ts is now+%s, want now+%s", got, timeout)
	}
	h.clock.advance(3 * time.Minute)
	r2, err := h.l.RenewLease(ctx, lease.LeaseID)
	if err != nil {
		t.Fatalf("second RenewLease: %v", err)
	}
	if r2.RenewCount != 2 {
		t.Errorf("renew_count after the second renewal = %d, want 2", r2.RenewCount)
	}
	third, perr := types.ParseUTC(r2.ExpiresTS)
	if perr != nil {
		t.Fatalf("expires_ts %q: %v", r2.ExpiresTS, perr)
	}
	if !third.After(second) {
		t.Errorf("the second renewal did not extend expires_ts: %s → %s", r1.ExpiresTS, r2.ExpiresTS)
	}
	if _, err := h.l.RenewLease(ctx, "lease_not_current"); CodeOf(err) != types.CodeLadder003 {
		t.Errorf("renewing a foreign lease returned %v, want TROUBLE-LADDER-003", err)
	}

	if err := h.l.ReleaseLease(ctx, lease.LeaseID, "completed"); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}
	if _, ok := h.l.Lease(); ok {
		t.Error("the lease table is still held after ReleaseLease")
	}
	released := 0
	for _, lz := range h.l.LeaseHistory() {
		if lz.State != types.LeaseReleased {
			continue
		}
		released++
		if lz.ReleaseReason != "completed" {
			t.Errorf("release_reason = %q, want completed", lz.ReleaseReason)
		}
	}
	if released != 1 {
		t.Errorf("%d released lease change(s), want 1: %v", released, leaseStates(h.l.LeaseHistory()))
	}
	next := adoptIncident(t, h, "lease-after-release", "rule-a", types.StRecorded, types.RungPlay)
	if _, err := h.l.AcquireLease(ctx, next, "trouble-agent/next", 0, ""); err != nil {
		t.Errorf("a released table must grant the next request: %v", err)
	}
}

// TestParkAndReAdoptResolvesLivenessByPID is the §7 AC-5 row and §3.5 step 3: a
// park plus ReAdopt re-adopts a live PID (renewing the lease, never moving the
// state) and resolves a dead PID from evidence — `agent:failed` with
// failure_class daemon_restart_lost and no strike, so a restart storm cannot
// starve the agent rung.
//
// §3.5 step 4's orphan branch is asserted in park_test.go: ReAdopt marks a park
// record resumed on both the live and the lost path before the orphan sweep reads
// l.resumed, so a known incident's dead worktree never reaches Ladder.orphan from
// that entry point. That is a defect in park.go, which this task does not own; the
// orphan effects themselves are pinned directly there.
func TestParkAndReAdoptResolvesLivenessByPID(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, harnessOpts{
		rules: map[string]types.Rule{"rule-agent": {Name: "rule-agent", EntryRung: types.RungAgent}},
	})
	dirLive := t.TempDir()
	dirDead := t.TempDir()
	live := adoptIncident(t, h, "readopt-live", "rule-agent", types.StAgentRunning, types.RungAgent)
	dead := adoptIncident(t, h, "readopt-dead", "rule-agent", types.StAgentRunning, types.RungAgent)
	h.l.incStateFor(live).PID, h.l.incStateFor(live).Worktree = 4242, dirLive
	h.l.incStateFor(dead).PID, h.l.incStateFor(dead).Worktree = 4243, dirDead

	// The drain parks both runs; the rows are then re-registered under the ids a
	// boot from the ledger would carry (see the scaffolding note).
	rep, err := h.l.Park(ctx, types.ParkSIGTERM)
	if err != nil {
		t.Fatalf("Park: %v", err)
	}
	if len(rep.Records) != 2 {
		t.Fatalf("Park wrote %d park record(s), want 2 (one per in-flight run)", len(rep.Records))
	}
	reRegisterParks(h, rep.Records)
	for _, pr := range rep.Records {
		if pr.Kind != "agent" {
			t.Errorf("park record %s has kind %q, want agent (the incident is in agent:running)", pr.ID, pr.Kind)
		}
		if pr.Worktree == "" || pr.PID == 0 {
			t.Errorf("park record %s lost the process facts: pid=%d worktree=%q", pr.ID, pr.PID, pr.Worktree)
		}
	}

	// Only 4242 is alive on this host.
	withPIDAlive(h, func(pid int) bool { return pid == 4242 })
	histBefore := len(h.l.LeaseHistory())
	got, err := h.l.ReAdopt(ctx)
	if err != nil {
		t.Fatalf("ReAdopt: %v", err)
	}
	if len(got.Resumed) != 2 {
		t.Errorf("ReAdopt reported %d resumed run(s) %v, want 2 (the live re-adoption and the lost run)", len(got.Resumed), got.Resumed)
	}

	// Live PID → re-adopt: the state never moves and the lease is renewed.
	stLive := h.l.incStateFor(live)
	if stLive.Inc.State != types.StAgentRunning {
		t.Errorf("re-adoption moved the live run to %q, want agent:running (a park is a record, not a state)", string(stLive.Inc.State))
	}
	lease, ok := h.l.Lease()
	if !ok {
		t.Fatal("the re-adopter holds no lease for the live run")
	}
	if lease.Inc != live || lease.PID != 4242 {
		t.Errorf("the re-adopted lease is %s pid=%d, want %s pid=4242", lease.Inc, lease.PID, live)
	}
	if lease.Worktree != dirLive {
		t.Errorf("the re-adopted lease worktree is %q, want %q", lease.Worktree, dirLive)
	}
	if lease.State != types.LeaseRenewed {
		t.Errorf("the re-adopted lease state is %q, want renewed", lease.State)
	}
	if added := len(h.l.LeaseHistory()) - histBefore; added != 1 {
		t.Errorf("ReAdopt appended %d lease change(s), want 1 (the renewal of the live run)", added)
	}

	// Dead PID → the run is lost, decided from evidence, never struck.
	stDead := h.l.incStateFor(dead)
	if stDead.Inc.State != types.StAgentFailed {
		t.Errorf("the lost run is %q, want agent:failed", string(stDead.Inc.State))
	}
	if stDead.FailureClass != reasonDaemonRestartLost {
		t.Errorf("failure_class = %q, want %q", stDead.FailureClass, reasonDaemonRestartLost)
	}
	if len(stDead.Strikes) != 0 {
		t.Errorf("the lost run carries %d strike(s), want 0: a daemon restart never increments strikes", len(stDead.Strikes))
	}
	if n := h.ledger.countPayload("transition", "resume_lost"); n != 1 {
		t.Errorf("%d resume_lost record(s), want 1", n)
	}
	if n := h.ledger.countPayload("failure_class", reasonDaemonRestartLost); n != 1 {
		t.Errorf("%d record(s) carry failure_class %q, want 1", n, reasonDaemonRestartLost)
	}
	if n := resumeRecords(h); n != 1 {
		t.Errorf("%d resume record(s), want 1 (only the live run resumes)", n)
	}

	// The worktrees are left in place for the SPEC-08 reaper.
	for _, dir := range []string{dirLive, dirDead} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("worktree %s is gone: %v", dir, err)
		}
	}
}

// reRegisterParks replaces the park rows Park registered with the same rows under
// unique ids: Park keys l.parkRecs by the record id, and types.NewID gives every
// record minted inside one millisecond the same id, so the map would hold one row
// (see the scaffolding note at the top of this file).
func reRegisterParks(h *harness, recs []types.ParkRecord) {
	h.l.mu.Lock()
	h.l.parkRecs = map[string]types.ParkRecord{}
	h.l.mu.Unlock()
	for i := range recs {
		recs[i].ID = mintID("ev_")
		h.l.InjectPark(recs[i])
	}
}

// leaseStates renders a lease history for failure messages.
func leaseStates(hist []types.AgentLease) []string {
	out := make([]string, 0, len(hist))
	for _, lz := range hist {
		out = append(out, lz.State+" "+lz.Inc)
	}
	return out
}
