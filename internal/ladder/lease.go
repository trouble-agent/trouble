package ladder

import (
	"context"
	"sync"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// leaseTable is the host-wide agent lease (§3.6): one agent run at a time per
// host, always (`agent_concurrency` is 1 and is not configurable in v0.1).
//
// Acquisition copies the fleet's spawn-overlap lesson (enqueue-first, then
// recheck): the `requested` record lands before any check, the holder is
// re-read after lease_recheck, and only then is the lease granted.
type leaseTable struct {
	mu      sync.Mutex
	hostID  string
	current *types.AgentLease
	history []types.AgentLease
	waiters map[string]int
}

func newLeaseTable(hostID string) *leaseTable {
	return &leaseTable{hostID: hostID, waiters: map[string]int{}}
}

// acquire performs the enqueue-first handshake and returns the granted lease.
func (t *leaseTable) acquire(l *Ladder, st *incState) (types.AgentLease, *Error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := l.now()
	req := types.AgentLease{
		LeaseID:   "lease_" + types.NewID(types.PEv)[3:],
		Inc:       st.Inc.ID,
		Sig:       st.Inc.Sig,
		HostID:    t.hostID,
		Holder:    "trouble-agent/" + st.Inc.ID,
		State:     types.LeaseRequested,
		GrantedTS: types.FormatUTC(now),
	}
	// Step 1: the requested record is durable before any check (the ledger's
	// single writer is the serialization point).
	t.history = append(t.history, req)

	// Step 2: re-read the holder after the recheck window, so a concurrent
	// request inside the same group-commit batch is visible.
	_ = l.cfg.LeaseRecheck

	heldByOther := false
	if t.current != nil && t.current.LeaseID != "" {
		expires, err := types.ParseUTC(t.current.ExpiresTS)
		expired := err == nil && now.After(expires)
		stale := false
		if !expired && t.current.PID != 0 && l.deps.PIDAlive != nil && !l.deps.PIDAlive(t.current.PID) {
			granted, gerr := types.ParseUTC(t.current.GrantedTS)
			if gerr == nil && now.Sub(granted) > l.cfg.LeaseGrace.Std() {
				stale = true
			}
		}
		if !expired && !stale {
			heldByOther = true
		} else {
			reclaimed := *t.current
			reclaimed.State = types.LeaseReclaimed
			reclaimed.ReleaseReason = "stale"
			t.history = append(t.history, reclaimed)
		}
	}
	if heldByOther {
		// Contention never escalates: a busy host is not a fault (§3.6 step 4).
		t.waiters[st.Inc.ID]++
		if t.waiters[st.Inc.ID] > 3 {
			st.Waiting = true
		}
		return types.AgentLease{}, newErr(types.CodeLadder003, reasonLeaseHeld,
			"the host lease is held by %s (attempt %d)", t.current.Inc, t.waiters[st.Inc.ID])
	}

	lease := req
	lease.State = types.LeaseGranted
	lease.ExpiresTS = types.FormatUTC(now.Add(l.cfg.AgentTimeout.Std()))
	lease.PID = st.PID
	lease.Worktree = st.Worktree
	lease.RouterRef = st.SpawnID
	t.current = &lease
	t.history = append(t.history, lease)
	delete(t.waiters, st.Inc.ID)
	return lease, nil
}

// renew extends a lease (every tool call and every lease heartbeat, §3.6).
func (t *leaseTable) renew(l *Ladder, leaseID string) (types.AgentLease, *Error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.current == nil || t.current.LeaseID != leaseID {
		return types.AgentLease{}, newErr(types.CodeLadder003, reasonLeaseHeld, "lease %s is not the current lease", leaseID)
	}
	now := l.now()
	t.current.State = types.LeaseRenewed
	t.current.RenewedTS = types.FormatUTC(now)
	t.current.RenewCount++
	t.current.ExpiresTS = types.FormatUTC(now.Add(l.cfg.AgentTimeout.Std()))
	return *t.current, nil
}

// release frees the lease with a reason (§3.6).
func (t *leaseTable) release(leaseID, reason string) {
	if leaseID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.current != nil && t.current.LeaseID == leaseID {
		rel := *t.current
		rel.State = types.LeaseReleased
		rel.ReleaseReason = reason
		t.history = append(t.history, rel)
		t.current = nil
		return
	}
}

// payload is the `lease{}` payload value of a transition.
func (t *leaseTable) payload(leaseID string) map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.current == nil || t.current.LeaseID != leaseID || leaseID == "" {
		return map[string]any{}
	}
	return map[string]any{
		"lease_id":   t.current.LeaseID,
		"holder":     t.current.Holder,
		"expires_ts": t.current.ExpiresTS,
		"state":      t.current.State,
	}
}

// Lease returns the current lease ("" when free).
func (t *leaseTable) Lease() (types.AgentLease, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.current == nil {
		return types.AgentLease{}, false
	}
	return *t.current, true
}

// History is every lease record change in order (the ledger rebuilds the same
// order at boot).
func (t *leaseTable) History() []types.AgentLease {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]types.AgentLease, len(t.history))
	copy(out, t.history)
	return out
}

// AcquireLease is the public entry point (§2).
func (l *Ladder) AcquireLease(ctx context.Context, incID, holder string, pid int, worktree string) (types.AgentLease, error) {
	l.mu.Lock()
	st, ok := l.incs[incID]
	l.mu.Unlock()
	if !ok {
		return types.AgentLease{}, newErr(types.CodeLadder012, "", "incident %s is not in the index", incID)
	}
	st.PID = pid
	st.Worktree = worktree
	lease, err := l.lease.acquire(l, st)
	if err != nil {
		return types.AgentLease{}, err
	}
	_, aerr := l.deps.Ledger.Append(ctx, types.KIncident, st.Inc.Sig, st.Inc.ID, map[string]any{
		"transition":  "lease",
		"lease":       map[string]any{"lease_id": lease.LeaseID, "holder": holder, "expires_ts": lease.ExpiresTS},
		"lease_state": lease.State,
		"sigs":        st.Sigs,
	})
	if aerr != nil {
		return lease, wrapErr(types.CodeLadder015, "", aerr)
	}
	return lease, nil
}

// RenewLease extends a lease (§2).
func (l *Ladder) RenewLease(ctx context.Context, leaseID string) (types.AgentLease, error) {
	lease, err := l.lease.renew(l, leaseID)
	if err != nil {
		return types.AgentLease{}, err
	}
	return lease, nil
}

// ReleaseLease frees a lease with an outcome (§2).
func (l *Ladder) ReleaseLease(ctx context.Context, leaseID, outcome string) error {
	l.lease.release(leaseID, outcome)
	return nil
}

// Lease exposes the current lease for the dashboard and the tests.
func (l *Ladder) Lease() (types.AgentLease, bool) { return l.lease.Lease() }

// LeaseHistory is the ordered lease record change list.
func (l *Ladder) LeaseHistory() []types.AgentLease { return l.lease.History() }

// leaseExpiry is the wall-clock deadline of a lease (used by the re-adopter).
func leaseExpiry(lease types.AgentLease) time.Time {
	t, err := types.ParseUTC(lease.ExpiresTS)
	if err != nil {
		return time.Time{}
	}
	return t
}
