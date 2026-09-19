package hub

import (
	"context"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// The degradation reasons of SPEC-13 §4.3 (`detail.reason` on the health
// surface). They are stable tokens: a dashboard, an alert rule and an operator
// all name the same state the same way.
const (
	DegradedNone      = ""
	DegradedRedisDown = "redis_unavailable"
	DegradedRefusing  = "redis_refusing"
	DegradedRedisAuth = "redis_auth"
	DegradedArchive   = "archive_paused"
)

// StandaloneStatus is the stanza a standalone profile reports: `Enabled=false`
// and the zero queue (SPEC-13 §4.1 step 2, SPEC-12 §3.7a: "no hub stanza
// (`HubStatus.Enabled=false`)").
func StandaloneStatus(profile, since string) types.HubStatus {
	if profile == "" {
		profile = ProfileStandalone
	}
	return types.HubStatus{
		Enabled: false,
		Profile: profile,
		Since:   since,
		Redis: types.RedisStreamOffsets{
			Degraded: false,
			Since:    since,
		},
		RouteCounters: map[string]int64{"A": 0, "B": 0},
	}
}

// Status assembles the profile's stanza from live state (SPEC-13 §2.3's
// `HubStatus`, §3.3's offsets). It is O(1) per field except the two filesystem
// reads of the archive queue and the marker ledger, which is why the archive
// numbers are read from memory when the runtime is running and from the files
// when it is not (a stopped daemon still answers `trouble hub status`).
func (r *Runtime) Status(ctx context.Context) types.HubStatus {
	if r == nil {
		return StandaloneStatus(ProfileStandalone, types.NowUTC())
	}
	st := types.HubStatus{
		Enabled:        r.enabled,
		Profile:        r.profile,
		Since:          r.since,
		ArchiveQueue:   r.archiveQueueDepth(),
		ArchiveLastTS:  r.archiveLastTS(),
		ArchivedFiles:  r.archivedFiles(),
		DroppableGens:  r.droppableCount(),
		MarkerPending:  r.markerPending(),
		RouteCounters:  map[string]int64{"A": r.routeA.Load(), "B": r.routeB.Load()},
		Degraded:       r.degraded(),
		DegradedReason: r.degradedReason(),
	}
	st.Redis = r.redisOffsets(ctx)
	return st
}

// redisOffsets projects the live queue state onto the SPEC-TYPES shape.
func (r *Runtime) redisOffsets(ctx context.Context) types.RedisStreamOffsets {
	out := types.RedisStreamOffsets{
		Stream:   r.redis.Stream,
		Group:    r.redis.Group,
		Consumer: r.cfg.Redis.HostID,
		Since:    r.since,
		// With no gate at all (a degraded boot, where the queue was never
		// reached) the window in force is the bounded LRU, and the surface says
		// so instead of implying the full 24h Redis window (SPEC-13 §3.4).
		DedupWindow: "lru",
	}
	if r.cfg.Redis.Consumer != "" {
		out.Consumer = r.cfg.Redis.Consumer
	} else if out.Consumer == "" {
		out.Consumer = r.cfg.Redis.HostID
	}
	if gate := r.dedupRef(); gate != nil {
		hits, misses, conflicts, _ := gate.Counters()
		out.DedupHits = hits
		out.DedupMisses = misses
		out.DedupConflicts = conflicts
		out.DedupWindow = gate.Window()
		// The ledger index the gate was re-warmed with (SPEC-13 §2.1.1 rule 5a,
		// §3.1 dedup.state's restored_keys): 0 means the window is NOT warm —
		// no read seam, an unreadable ledger, or a ledger that holds nothing —
		// and a failover that emptied Redis then costs the duplicate §3.4 prices.
		out.DedupRestored, _ = gate.Restored()
	} else if out.DedupHits+out.DedupMisses > 0 {
		out.DedupWindow = "lru"
	}
	out.Degraded = r.degraded()
	out.DegradedReason = r.degradedReason()
	client := r.clientRef()
	if client == nil || !r.connected() {
		// Offline: the offsets are the zero value plus the counters we still own
		// and the LRU window, which is what a degraded stanza must say rather
		// than reporting a stale "ok".
		out.OptionsChecked = false
		return out
	}
	out.OptionsChecked = client.ServerInfo().OptionsChecked
	out.AOF = client.ServerInfo().AOFEnabled
	out.Policy = client.ServerInfo().Policy
	out.EvictedKeys = client.ServerInfo().EvictedKeys
	out.Reclaims = client.Counters.Reclaims.Load()
	out.Backpressure = client.Counters.Backpressure.Load()
	if ctx == nil {
		return out
	}
	if n, err := client.StreamLen(ctx); err == nil {
		out.StreamLen = n
	}
	if gi, err := client.GroupInfo(ctx); err == nil {
		out.Pending = gi.Pending
		out.Lag = gi.Lag
		out.LastDeliveredID = gi.LastDeliveredID
	}
	if pending, err := client.Pending(ctx); err == nil && out.Pending == 0 {
		out.Pending = pending.Count
	}
	acked, _ := client.Watermarks()
	out.LastAckedID = acked
	return out
}

// DegradedReasonFor selects the SPEC-13 §4.3 reason for a failure code.
func DegradedReasonFor(code types.ErrorCode, requireRedis bool) string {
	switch code {
	case types.CodeHub002:
		return DegradedRedisAuth
	case types.CodeHub003:
		return DegradedRedisDown
	case types.CodeHub004:
		if requireRedis {
			return DegradedRefusing
		}
		return DegradedRedisDown
	case types.CodeHub009, types.CodeHub010, types.CodeHub012:
		return DegradedArchive
	default:
		return DegradedNone
	}
}

// archiveQueueDepth, archiveLastTS, archivedFiles and markerPending read the
// in-memory counters when the runtime is live and the files otherwise, so a
// boot that never reached Redis still reports the archival backlog truthfully.
func (r *Runtime) archiveQueueDepth() int64 {
	if r.queueDepth != nil {
		return r.queueDepth()
	}
	q, err := ArchiveQueue(r.cfg.Archive.StateRoot)
	if err != nil {
		return 0
	}
	return q.Depth()
}

func (r *Runtime) archiveLastTS() string {
	if r.queueLastTS != nil {
		return r.queueLastTS()
	}
	q, err := ArchiveQueue(r.cfg.Archive.StateRoot)
	if err != nil {
		return ""
	}
	return q.LastTS()
}

func (r *Runtime) archivedFiles() int64 {
	if r.markers == nil {
		return 0
	}
	_, _, exported, verified, _, _ := r.markers.Summarize()
	return exported + verified
}

func (r *Runtime) markerPending() int64 {
	if r.markers == nil {
		return 0
	}
	_, pending, _, _, _, _ := r.markers.Summarize()
	return pending
}

// droppableCount is the last archival pass's verdict (SPEC-13 §2.2: the status
// verb prints "verified exports the sweep may now delete"). An embedder may
// override it through RuntimeConfig's seam.
func (r *Runtime) droppableCount() int {
	if r.droppable != nil {
		return r.droppable()
	}
	return int(r.droppableSeen.Load())
}
