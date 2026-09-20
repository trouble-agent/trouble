package hub

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// Mode is the runtime's state machine. It is deliberately small: SPEC-13 §4.3
// has one row per state, and each row names what senders see, what the daemon
// does and what the health surface says.
type Mode string

const (
	// ModeStandalone: this daemon is not a light-hub. Nothing in this package
	// runs (SPEC-13 §4.1 step 2).
	ModeStandalone Mode = "standalone"
	// ModeUp: Redis is dialled, the group exists and the consumer is draining.
	ModeUp Mode = "up"
	// ModeDegradedBoot: Redis was unreachable at boot and require_redis=false.
	// The daemon serves on the standalone in-process path and keeps trying to
	// wire the queue (§5, TROUBLE-HUB-003: "degraded start with the standalone
	// path active").
	ModeDegradedBoot Mode = "degraded_boot"
	// ModeUnavailable: Redis was lost at runtime and require_redis=false.
	// Senders get 429 + Retry-After and their spools hold (§4.3).
	ModeUnavailable Mode = "unavailable"
	// ModeRefusing: Redis was lost (or was never reachable) and
	// require_redis=true. Nothing is 200-answered that cannot be queued (§4.2).
	ModeRefusing Mode = "refusing"
)

// RuntimeConfig is everything the runtime needs, all of it resolved elsewhere:
// the profile (internal/lifecycle), the ledger writer (internal/ledger), the
// in-process fallback path, and the injection seams for tests.
type RuntimeConfig struct {
	// Profile is the resolved SPEC-13 §2.1 profile.
	Profile types.ProfileConfig
	Redis   RedisConfig
	Archive ArchiveConfig

	// StateRoot is the daemon's state root; the hub state tree lives under it.
	StateRoot string
	// HostID is origin.host_id: the default consumer name and the envelope's
	// host identity.
	HostID string
	// HubID is server.hub_id ("" → host id).
	HubID string

	// Ledger is the durable writer the consumer appends through. It is required
	// when the profile is light-hub: a hub with no ledger has nothing to be a
	// queue in front of.
	Ledger Appender
	// Standalone is the in-process path used while the profile is degraded at
	// boot. Nil means a degraded boot cannot serve at all, and the runtime says
	// so instead of accepting traffic it cannot record.
	Standalone Appender

	// Target overrides the archival target (tests and embedders). Nil resolves
	// it from Archive through NewTarget.
	Target Target
	// LedgerIndex is the READ half of the ledger the dedup gate re-warms from at
	// every (re)wire (SPEC-13 §2.1.1 rule 5a). The composition root passes the
	// same ledger it passes as Ledger; nil means "no index", and the gate then
	// reports restored_keys=0 rather than pretending the window is warm.
	LedgerIndex LedgerIndex
	// AckCursor stamps the envelope's ack cursor (the ledger's last seq).
	AckCursor func() uint64
	// Streams overrides the Redis command surface (tests). Nil dials the URL.
	Streams Streams

	Log func(format string, args ...any)
	Now func() time.Time
}

// Runtime is the mounted light-hub profile.
type Runtime struct {
	enabled bool
	profile string
	since   string
	cfg     RuntimeConfig
	redis   RedisConfig

	client   *Client
	dedup    *DedupGate
	door     *Door
	consumer *Consumer
	archiver *Archiver
	markers  *MarkerStore
	target   Target

	mode     atomic.Value // string(Mode)
	reason   atomic.Value // string
	bootCode atomic.Value // string

	mu             sync.RWMutex
	consumerCancel context.CancelFunc
	// consumerDone closes when the goroutine registered with consumerCancel has
	// returned. A rewire waits on it before a second consumer starts, so two
	// consumers never share one state root (SPEC-13 §1 rule 3).
	consumerDone  chan struct{}
	lifeCtx       context.Context
	archivePaused bool

	// rewireMu serializes recoveries: two rewires racing would build two
	// consumers for one state root.
	rewireMu sync.Mutex
	// probeFailures counts consecutive failed queue liveness probes taken while
	// the state machine still says "up". The supervisor is the only reader and
	// the only writer.
	probeFailures int
	// wake is the loss signal: markLost (the serve path that DETECTS the loss)
	// pokes it so the supervisor retries the rewire at once instead of sleeping
	// out the rest of its interval. Buffered and written non-blockingly, so the
	// serve path never blocks on the recovery loop.
	wake chan struct{}

	cancel context.CancelFunc
	wg     sync.WaitGroup

	routeA, routeB atomic.Int64

	// droppableSeen is the last pass's droppable-generation count, reported by
	// Status so the health surface never has to rescan the ledger directory on
	// every heartbeat.
	droppableSeen atomic.Int64

	// countIngested/countRefused/countFallback are the ingestion-path counters.
	countIngested atomic.Int64
	countRefused  atomic.Int64
	countFallback atomic.Int64

	// queueDepth/queueLastTS are the status surface's read seams; when nil the
	// status code reads the files. droppable, when set by an embedder, overrides
	// the last pass's own count.
	queueDepth  func() int64
	queueLastTS func() string
	droppable   func() int

	now func() time.Time
}

// Open boots the runtime: profile gate → archive target → Redis dial → group →
// door. It returns an error only when the boot must be REFUSED (SPEC-13 §4.1
// step 1/3, §5 codes 001/002/003/005/015/016 with require_redis=true); a
// degraded boot returns a runtime in ModeDegradedBoot.
func Open(ctx context.Context, cfg RuntimeConfig) (*Runtime, error) {
	cfg.Redis = cfg.Redis.WithDefaults()
	cfg.Archive = cfg.Archive.WithDefaults()
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	// The hub state tree lives under the DAEMON's state root: the archival tier's
	// state root is not a separate key (SPEC-13 §3.1 has one state root), so the
	// runtime pins it here rather than letting a zero value write to the cwd.
	cfg.Archive.StateRoot = cfg.StateRoot
	if cfg.Archive.LedgerRoot == "" {
		cfg.Archive.LedgerRoot = LedgerRootFromState(cfg.StateRoot)
	}
	r := &Runtime{cfg: cfg, redis: cfg.Redis, now: cfg.Now, since: types.FormatUTC(cfg.Now())}
	r.profile = cfg.Profile.Profile
	if r.profile == "" {
		r.profile = ProfileStandalone
	}
	r.mode.Store(string(ModeStandalone))
	r.reason.Store(DegradedNone)
	r.bootCode.Store("")

	if cfg.Profile.Profile != ProfileLightHub {
		// Standalone: nothing runs, nothing is created, and the state root is
		// untouched (§4.1 step 2).
		return r, nil
	}
	r.enabled = true
	r.wake = make(chan struct{}, 1)
	r.eachRoute()

	if err := EnsureStateDirs(cfg.StateRoot); err != nil {
		return nil, err
	}
	if _, err := WriteProfileState(cfg.StateRoot, cfg.Profile, cfg.Now()); err != nil {
		return nil, err
	}
	r.markers = NewMarkerStore(cfg.StateRoot)

	// The archival tier fails INDEPENDENTLY of the queue (§4.3's two rules): an
	// unusable target pauses archival and leaves ingestion alone.
	r.target = cfg.Target
	if r.target == nil {
		t, err := NewTarget(cfg.Archive)
		if err != nil {
			cfg.Log("hub: archival paused: %v", err)
			r.setBootCode(err)
		} else {
			r.target = t
		}
	}
	if r.target != nil {
		r.archiver = NewArchiver(cfg.Archive, r.target, cfg.Ledger)
	}

	c, err := r.dial(ctx)
	if err != nil {
		if cfg.Redis.RequireRedis {
			return nil, err
		}
		r.setBootCode(err)
		r.setMode(ModeDegradedBoot, DegradedReasonFor(CodeOf(err), false))
		cfg.Log("hub: %v (require_redis=false): serving on the standalone in-process path until redis returns", err)
		return r, nil
	}
	if err := r.attach(ctx, c); err != nil {
		_ = c.Close()
		return nil, err
	}
	return r, nil
}

// dial opens Redis through the configured seam (or the injected Streams).
func (r *Runtime) dial(ctx context.Context) (*Client, error) {
	if r.cfg.Streams != nil {
		return OpenWith(ctx, r.redis, r.cfg.Streams)
	}
	return OpenRedis(ctx, r.redis)
}

// rewarmGate seeds a freshly attached gate from the ledger's idempotency index
// (SPEC-13 §2.1.1 rule 5a) and reports how many keys it restored.
//
// It is called by attach, i.e. by BOTH ways the queue is wired: the boot of §4.1
// step 3 and the reconnect of §4.3's "Redis returns" row. A ledger with no read
// seam, or one whose walk fails, leaves the gate with restored_keys=0 — the
// window is then simply not warm, which is reported (`redis.dedup_restored`,
// dedup.state's `restored_keys`) instead of being hidden behind a healthy
// looking gate. Nothing here is fatal: the ledger is the record and the gate is
// an optimisation over it (§3.4), so a re-warm that cannot run must never keep
// the queue from being wired.
func (r *Runtime) rewarmGate(gate *DedupGate) int {
	if gate == nil || r.cfg.LedgerIndex == nil {
		return 0
	}
	keys, err := ledgerIdemKeys(r.cfg.LedgerIndex, r.hostID(), gate.cfg.DedupLRU)
	if err != nil {
		r.cfg.Log("hub: the ledger's idempotency index could not be read (%v): the gate starts with restored_keys=0", err)
	}
	if len(keys) == 0 {
		return 0
	}
	gate.Restore(keys)
	r.cfg.Log("hub: dedup gate re-warmed from the ledger's idempotency index (restored_keys=%d, failover_grace=%s)",
		len(keys), r.redis.FailoverGrace)
	return len(keys)
}

// attach wires a live client: gate → door → consumer. The group is created here
// and a TROUBLE-HUB-005 refusal is returned, never swallowed: a queue nobody
// drains must not accept traffic (SPEC-13 §5).
//
// Every pointer it installs is written under the runtime's lock, and the client
// it REPLACES is released once installed: the serve path and the health surface
// read the wiring while a rewire is in flight, and the previous client's
// consumer has already been stopped by reconnect before this is called (so
// closing it releases a connection nothing is using).
func (r *Runtime) attach(ctx context.Context, c *Client) error {
	if err := c.EnsureGroup(ctx); err != nil {
		return err
	}
	gate := NewDedupGate(r.redis, c)
	// The gate measures the `server.redis.failover_grace` window on the
	// runtime's clock, so an embedder (or a test) that drives time drives the
	// window too, and the window and the runtime's own `since` cannot disagree.
	if r.now != nil {
		gate.now = r.now
	}
	// The re-warm of SPEC-13 §2.1.1 rule 5a, BEFORE the gate can be used: the
	// gate this (re)wire installs is the ledger's index, not a fresh one, so a
	// duplicate that arrives right after a recovery is suppressed by the tier
	// rather than re-appended.
	r.rewarmGate(gate)
	door := NewDoor(c, gate, DoorConfig{
		Route:  RouteA,
		HostID: r.hostID(),
		HubID:  r.hubID(),
		Ack:    r.cfg.AckCursor,
		Wait:   r.redis.DoorWait,
	})
	cs := NewConsumer(c, r.cfg.Ledger, gate, r.redis).
		WithCompletion(door.Complete).
		WithStrandedHook(r.onStranded).
		WithLogger(r.cfg.Log)
	r.mu.Lock()
	prev := r.client
	r.client = c
	r.dedup = gate
	r.door = door
	r.consumer = cs
	r.mu.Unlock()
	if prev != nil && prev != c {
		_ = prev.Close()
	}
	r.setMode(ModeUp, DegradedNone)
	return nil
}

// Start launches the consumer, the recovery supervisor and the archive timer.
// It is idempotent: a second call is a no-op.
//
// The supervisor runs for EVERY enabled runtime, not only for one that booted
// degraded. A runtime that booted UP and then LOST its queue has no other path
// back: SPEC-13 §4.3's "Redis returns" row is a daemon behaviour ("rewires the
// consumer"), and §2.1.1 rule 5 makes a cold (empty) server a recovery too, not
// a reason to restart the process.
func (r *Runtime) Start(ctx context.Context) error {
	if r == nil || !r.enabled {
		return nil
	}
	r.mu.Lock()
	if r.cancel != nil {
		r.mu.Unlock()
		return nil
	}
	cctx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	// The runtime's own lifetime context: every goroutine it spawns descends
	// from it, so Close stops them all regardless of which caller triggered the
	// last rewire.
	r.lifeCtx = cctx
	r.mu.Unlock()

	if r.connected() {
		r.startConsumer(cctx)
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.supervise(cctx)
	}()
	if r.archiver != nil {
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.archiveLoop(cctx)
		}()
	}
	return nil
}

func (r *Runtime) startConsumer(ctx context.Context) {
	r.mu.Lock()
	if r.consumer == nil || r.consumerCancel != nil {
		r.mu.Unlock()
		return
	}
	if r.lifeCtx != nil {
		ctx = r.lifeCtx
	}
	cs := r.consumer
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	r.consumerCancel = cancel
	r.consumerDone = done
	r.mu.Unlock()
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		// The registration is released BEFORE `done` closes (deferred calls run
		// in reverse order), so a caller that waited on `done` knows the slot is
		// free — and only the goroutine that owns the registration clears it: a
		// rewire that already installed a new one must not have it wiped by the
		// consumer it replaced.
		defer func() {
			r.mu.Lock()
			if r.consumerDone == done {
				r.consumerCancel = nil
				r.consumerDone = nil
			}
			r.mu.Unlock()
		}()
		defer close(done)
		if err := cs.Run(runCtx); err != nil {
			r.cfg.Log("hub: consumer stopped: %v", err)
		}
	}()
}

// stopConsumer cancels the running consumer and waits for its goroutine to
// return, reporting whether it did within the timeout.
//
// It is what makes "the stale consumer must be stopped before the new one
// drains" a structural property rather than a hope: SPEC-13 §1 rule 3 allows
// exactly one consumer per state root, and a rewire that started a second one
// while the first was between two XREADGROUPs would be two writers on one
// ledger. A consumer that does not stop in time is a REFUSAL to rewire (the
// caller keeps the old wiring and retries), never a second consumer.
func (r *Runtime) stopConsumer(timeout time.Duration) bool {
	r.mu.Lock()
	cancel, done := r.consumerCancel, r.consumerDone
	r.mu.Unlock()
	if cancel == nil {
		return true
	}
	cancel()
	if done == nil {
		return true
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

// consumerRunning reports whether a consumer goroutine is registered. It is the
// second half of "the queue is live": a group nobody drains is not a live queue
// (SPEC-13 §5, TROUBLE-HUB-005).
func (r *Runtime) consumerRunning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.consumerCancel != nil
}

// supervise is the recovery loop. It keeps asking whether the queue is LIVE
// (attached, group present, consumer draining) and rewires it when it is not —
// with no external trigger, which is what SPEC-13 §4.3's "Redis returns" row
// and §2.1.1 rule 5 require of the daemon.
//
// The cadence is deliberately two-speed. While the queue is live the loop is a
// cheap liveness probe on the steady-state interval; while it is NOT, it retries
// on the consumer's own bounded backoff (≈1–2s) so a Redis that answers is wired
// in about a second. The operation is identical either way — only how soon a
// FAILED attempt is retried changes.
func (r *Runtime) supervise(ctx context.Context) {
	wait := DefaultSuperviseInterval
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-r.wake:
			// A loss was detected by the serve path: stop waiting out the
			// interval and act now.
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		if r.superviseOnce(ctx) {
			wait = DefaultSuperviseInterval
		} else {
			wait = nextBackoff(wait)
		}
		timer.Reset(wait)
	}
}

// superviseOnce is one pass of the recovery loop. It reports whether the queue
// is live afterwards.
func (r *Runtime) superviseOnce(ctx context.Context) bool {
	if !r.connected() {
		// The state machine already knows the queue is down (a refusal moved it
		// there): retry the rewire.
		r.probeFailures = 0
		return r.reconnect(ctx)
	}
	live, err := r.queueLive(ctx)
	if !live {
		r.probeFailures++
		// A NOGROUP answer is a FACT, not a hypothesis: the server replied, and
		// it does not know the group — the cold-Redis case of §2.1.1 rule 5 —
		// so it is acted on at once. A transport failure is re-probed once: one
		// slow round trip must not tear down a working consumer.
		if !IsNoGroup(err) && r.probeFailures < 2 {
			r.cfg.Log("hub: queue probe failed once (%v): re-probing before the rewire", err)
			return false
		}
		r.cfg.Log("hub: the queue is not live while the runtime says up (%v): rewiring", err)
		// The state machine said "up"; it must not (SPEC-13 §1 rule 4:
		// degradation is a designed state, never a silent success).
		r.markLost(ctx, err)
		r.probeFailures = 0
		return r.reconnect(ctx)
	}
	r.probeFailures = 0
	if !r.consumerRunning() {
		// The group is fine and nobody is draining it: the consumer returned
		// (TROUBLE-HUB-005 is the §5 case) and the runtime would otherwise
		// report `up` with no writer behind it.
		r.cfg.Log("hub: the queue is live but no consumer is running: restarting the consumer")
		r.startConsumer(ctx)
		return r.consumerRunning()
	}
	return true
}

// queueLive asks the attached server whether it still knows the consumer group.
//
// This is the probe §2.1.1 rule 5 needs and the serve path cannot provide: a
// cold (empty) Redis that answers on the same address leaves the daemon attached
// to a server with no `trouble:ingest` — and XADD happily creates the key again,
// so an offer that follows is answered 429 by a door timeout, not by anything
// that says "the group is gone".
func (r *Runtime) queueLive(ctx context.Context) (bool, error) {
	c := r.clientRef()
	if c == nil {
		return false, errf(types.CodeHub004, ReasonXAdd, "the queue is not attached: nothing is draining the stream")
	}
	if _, err := c.Pending(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// reconnect is one attempt to bring the queue back (SPEC-13 §4.3 row "Redis
// returns": "rewires the consumer", lifecycle `redis_restored`). It reports
// whether the queue is live afterwards, and it is callable outside the ticker so
// a caller can force a rewire (the `trouble hub` verbs) instead of waiting.
//
// The stale consumer is stopped FIRST and the rewire is refused if it will not
// stop: the new consumer must never drain alongside the old one (§1 rule 3).
func (r *Runtime) reconnect(ctx context.Context) bool {
	r.rewireMu.Lock()
	defer r.rewireMu.Unlock()

	if !r.stopConsumer(consumerStopTimeout) {
		r.cfg.Log("hub: the previous consumer did not stop within %s: refusing to run a second one", consumerStopTimeout)
		return false
	}
	c, err := r.dial(ctx)
	if err != nil {
		r.setMode(r.failureMode(), DegradedReasonFor(CodeOf(err), r.redis.RequireRedis))
		return false
	}
	if err := r.attach(ctx, c); err != nil {
		_ = c.Close()
		r.cfg.Log("hub: consumer group unusable: %v", err)
		// The group is unusable: the daemon must not accept what it cannot drain,
		// so the runtime stays refusing rather than pretending to serve (§5, 005).
		r.setMode(ModeRefusing, DegradedRedisDown)
		return false
	}
	r.cfg.Log("hub: redis restored (stream %s group %s consumer %s)", r.redis.Stream, r.redis.Group, c.ConsumerName())
	r.recordLifecycle(ctx, "redis_restored", map[string]any{
		"stream":   r.redis.Stream,
		"group":    r.redis.Group,
		"consumer": c.ConsumerName(),
	})
	r.startConsumer(ctx)
	return true
}

// WaitDrained polls the queue's own drain state until it holds or `timeout`
// elapses. The drain condition — lag == 0 AND pending == 0 — is exactly
// "every entry XADDed so far has been delivered to the consumer AND acked":
// the XACK that empties the pending list is the same batch call that advances
// the client's acked watermark (SetLastAcked), so once the condition holds,
// reading `redis.last_acked_id` (the health stanza's cursor) cannot observe
// the pre-ack state, no matter how the caller got here. That makes it the
// bounded wait a caller uses to order a watermark read on the ack instead of
// racing it — the level-based form, which needs no registration and is immune
// to completion events that fired before the caller began waiting.
func (r *Runtime) WaitDrained(ctx context.Context, timeout time.Duration) bool {
	if r == nil {
		return false
	}
	deadline := time.Now().Add(timeout)
	for {
		c := r.clientRef()
		if c == nil {
			return false
		}
		gi, gerr := c.GroupInfo(ctx)
		var pending Pending
		var perr error
		if gerr == nil {
			pending, perr = c.Pending(ctx)
		}
		switch {
		case gerr == nil && perr == nil:
			if gi.Lag == 0 && pending.Count == 0 {
				return true
			}
		case CodeOf(gerr) == types.CodeHub004 || CodeOf(perr) == types.CodeHub004:
			// A transient 004 (the probe itself failed): keep polling until
			// the deadline rather than failing the wait.
		default:
			return false
		}
		remain := time.Until(deadline)
		if remain <= 0 || ctx.Err() != nil {
			return false
		}
		sleepCtx(ctx, min(remain, 5*time.Millisecond))
	}
}

// Ingest is the ingestion-path seam the composition root mounts in front of the
// sentinel's ledger sink when the profile is light-hub (SPEC-13 §4.2).
//
// The four answers are the spec's four:
//
//   - ModeUp → enqueue and wait for the consumer's append+fsync, returning the
//     record the LEDGER wrote.
//   - ModeDegradedBoot → the standalone in-process path (§5, code 003's
//     behaviour), counted so the fallback is visible.
//   - ModeUnavailable / ModeRefusing → a transient refusal (TROUBLE-HUB-004);
//     the sender gets 429 (+Retry-After) or, under require_redis, a hard
//     refusal. Never a 200.
func (r *Runtime) Ingest(ctx context.Context, draft types.RecordDraft) (types.Record, error) {
	if r == nil {
		return types.Record{}, errf(types.CodeHub003, ReasonRedisDial, "no hub runtime")
	}
	switch Mode(r.mode.Load().(string)) {
	case ModeUp:
		// The door is always present while the mode says "up": attach installs
		// it before the state machine moves, and a rewire replaces it with
		// another one, never with nil.
		door := r.doorRef()
		rec, err := door.Offer(ctx, draft)
		if err != nil {
			// A failed offer is NOT automatically a Redis loss: the door answers
			// three different things here (see markLost).
			r.markLost(ctx, err)
			return types.Record{}, err
		}
		r.countIngested.Add(1)
		r.countRoute(door.Route())
		return rec, nil
	case ModeDegradedBoot:
		if r.cfg.Standalone == nil {
			r.countRefused.Add(1)
			return types.Record{}, errf(types.CodeHub004, ReasonXAdd,
				"redis is unavailable and no standalone path is wired for the light-hub profile")
		}
		rec, err := r.cfg.Standalone.Append(ctx, draft)
		if err != nil {
			return types.Record{}, err
		}
		r.countFallback.Add(1)
		r.countRoute(RouteA)
		return rec, nil
	case ModeStandalone:
		if r.cfg.Standalone != nil {
			rec, err := r.cfg.Standalone.Append(ctx, draft)
			if err != nil {
				return types.Record{}, err
			}
			r.countRoute(RouteA)
			return rec, nil
		}
		return types.Record{}, errf(types.CodeHub001, ReasonProfile, "hub runtime is not enabled")
	default:
		r.countRefused.Add(1)
		return types.Record{}, errf(types.CodeHub004, ReasonXAdd,
			"the redis ingestion queue is unavailable (%s): the event was refused, not accepted",
			r.degradedReason())
	}
}

// markLost records a runtime Redis loss and moves the state machine (§4.3).
//
// It is the ONE place that decides the daemon lost its queue, so it is also the
// place that refuses to record a loss that is not one. §4.3's rows are statements
// about REDIS — "Redis lost at runtime" refuses senders, names code 004 and buys
// a rewire — and the door answers THREE different things, only one of which is
// about Redis:
//
//	the sender's request ended   context.Canceled / DeadlineExceeded
//	the draft was refused        TROUBLE-HUB-007 (the payload is not canonical JSON)
//	the queue failed             002 / 003 / 004 from the client, NOGROUP, transport
//
// Treating all three as a loss cost a live tick: a POST served with an
// already-dead context made the door return `context.Canceled`, the daemon wrote
// a `redis_lost` record with an EMPTY code and reason ("hub: redis lost at
// runtime (): context canceled" on stdout and
// {"op":"redis_lost","error_code":"","reason":""} in the ledger), tore down a
// healthy consumer mid-append — stranding the entry the sender had already
// enqueued until claim_min_idle — and rewired the queue. A recovery for a Redis
// failure that never happened.
//
// An error that cannot be attributed to the queue is therefore not acted on and
// not recorded: the supervisor's own liveness probe (§2.1.1 rule 5) decides that
// case on its next pass, with its own attributable error. The record describes
// the TRANSITION, not every observation: while the state machine is already
// degraded the supervisor keeps retrying the rewire without writing a second
// `redis_lost` for every attempt. The loss signal is poked afterwards so the
// recovery loop retries the rewire at once instead of sleeping out the rest of
// its interval.
//
// One offer failure still moves the state machine although Redis itself answered:
// the door's own TIMEOUT (code 004, "enqueued as <entry> but the consumer did not
// append within <wait>"). That stays a loss deliberately — the entry is IN the
// stream and nothing is draining it, which is the degradation §4.3 describes for
// senders (429 + Retry-After), and the rewire it buys is the remedy for a wedged
// consumer — and the record it writes carries that code and the matching reason,
// so it is never an empty one. This is also the shape a real mid-ack failure
// (XACK refused while a sender waits) reaches the ledger through: the consumer
// logs the un-acked batch and leaves its entries pending (consume.go), the
// sender's door wait expires, and the loss is recorded once, with a reason.
func (r *Runtime) markLost(ctx context.Context, err error) {
	if r.degraded() {
		return
	}
	if IsRequestEnd(ctx, err) {
		r.cfg.Log("hub: the sender's request ended (%v): the queue is left as it is", err)
		return
	}
	if !IsQueueFailure(err) {
		r.cfg.Log("hub: the offer failed for a reason that is not the queue (%v): the queue is left as it is", err)
		return
	}
	code := CodeOf(err)
	reason := DegradedReasonFor(code, r.redis.RequireRedis)
	if code == "" || reason == DegradedNone {
		// A `redis_lost` record with an empty code and an empty reason names no
		// failure at all, so it is not written. Unreachable for the errors the
		// door and the probe actually raise (every one of them carries a code
		// from the 002/003/004 family); kept as the invariant this function owes
		// its callers.
		r.cfg.Log("hub: an unattributable failure (%v): no state change and no record", err)
		return
	}
	mode := ModeUnavailable
	if r.redis.RequireRedis {
		mode = ModeRefusing
	}
	r.setMode(mode, reason)
	r.cfg.Log("hub: redis lost at runtime (%s): %v", r.degradedReason(), err)
	r.recordLifecycle(context.Background(), "redis_lost", map[string]any{
		"error_code": string(code),
		"reason":     reason,
	})
	r.signalLoss()
}

// signalLoss wakes the supervisor (non-blocking: the serve path must never wait
// on the recovery loop).
func (r *Runtime) signalLoss() {
	if r == nil || r.wake == nil {
		return
	}
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// onStranded is the consumer's TROUBLE-HUB-008 hook: one ledger record per
// reclaim round that took back entries with no progress.
func (r *Runtime) onStranded(rounds, entries int) {
	r.recordLifecycle(context.Background(), "stranded_entries", map[string]any{
		"error_code": string(types.CodeHub008),
		"rounds":     rounds,
		"entries":    entries,
		"min_idle":   r.redis.ClaimIdle.String(),
	})
}

// ArchiveOnce runs one archival pass (SPEC-13 §4.1 step 5: "The archive timer
// starts on archive_interval and runs one pass at boot"). A DuckBrain outage is a
// paused queue, never a failed boot.
func (r *Runtime) ArchiveOnce(ctx context.Context) (ArchiveReport, error) {
	report := ArchiveReport{}
	if r == nil || !r.enabled {
		return report, nil
	}
	if r.archiver == nil {
		r.setArchivePaused(true)
		return report, errf(types.CodeHub009, ReasonArchiveCfg,
			"archival does not start: no usable DuckBrain namespace/endpoint (SPEC-13 §5)")
	}
	plans, err := PlanArchive(r.cfg.Archive.StateRoot, r.cfg.Archive)
	if err != nil {
		return report, err
	}
	report.Planned = len(plans)
	batch := r.cfg.Archive.BatchFiles
	for i, plan := range plans {
		if batch > 0 && i >= batch {
			break
		}
		if err := ctx.Err(); err != nil {
			return report, err
		}
		mk, err := r.archiver.Archive(ctx, plan)
		if err != nil {
			report.Failed++
			report.Errors = append(report.Errors, err.Error())
			// 010/012 are transient: the job stays pending and the next pass
			// retries the SAME marker id. 009/011 are permanent refusals that a
			// later pass re-plans.
			r.setArchivePaused(CodeOf(err) == types.CodeHub010 || CodeOf(err) == types.CodeHub012)
			r.recordArchiveFailure(ctx, plan, err)
			continue
		}
		if mk.State == "verified" {
			report.Verified++
		} else {
			report.Exported++
		}
	}
	if report.Failed == 0 {
		r.setArchivePaused(false)
		report.Droppable = r.sweepDroppable(ctx)
		r.droppableSeen.Store(int64(report.Droppable))
	}
	report.TotalFiles = r.archiver.Exported.Load() + r.archiver.Verified.Load()
	return report, nil
}

// ArchiveReport is one pass's outcome.
type ArchiveReport struct {
	Planned    int
	Exported   int
	Verified   int
	Failed     int
	Droppable  int
	TotalFiles int64
	Errors     []string
}

// sweepDroppable lists the generations a verified export licenses the sweep to
// delete, and records the TROUBLE-HUB-014 refusal when one is blocked.
func (r *Runtime) sweepDroppable(ctx context.Context) int {
	drops, err := DroppableGenerationsIn(r.cfg.Archive.LedgerRoot, r.cfg.Archive.StateRoot, r.cfg.Archive.LiveFile, r.cfg.Archive.KeepLocalGens)
	if err != nil && CodeOf(err) == types.CodeHub014 {
		r.recordLifecycle(ctx, "retention_deferred", map[string]any{
			"error_code": string(types.CodeHub014),
			"detail":     err.Error(),
		})
	}
	if err != nil {
		r.cfg.Log("hub: retention deferred: %v", err)
	}
	return len(drops)
}

func (r *Runtime) recordArchiveFailure(ctx context.Context, plan ArchivePlan, err error) {
	r.recordLifecycle(ctx, "archive_failed", map[string]any{
		"error_code": string(CodeOf(err)),
		"file":       plan.File,
		"marker_id":  plan.MarkerID,
		"reason":     ReasonOf(err),
		"detail":     err.Error(),
	})
}

// archiveLoop is the interval timer of §4.1 step 5.
func (r *Runtime) archiveLoop(ctx context.Context) {
	interval := r.cfg.Archive.Interval
	if interval <= 0 {
		interval = DefaultArchiveInterval
	}
	// One pass at start: a boot pass with a DuckBrain outage pauses the queue
	// instead of failing the boot.
	if _, err := r.ArchiveOnce(ctx); err != nil {
		r.cfg.Log("hub: archive pass: %v", err)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := r.ArchiveOnce(ctx); err != nil {
				r.cfg.Log("hub: archive pass: %v", err)
			}
		}
	}
}

// ---- wiring accessors ----
//
// The wiring (client, gate, door, consumer) is REPLACED by a rewire while
// requests are being served and while the health surface is reading, so every
// read goes through the runtime's lock. A pointer handed to a caller is a
// snapshot: it stays valid for the call, and the rewire replaces r.<field>, not
// the object.

func (r *Runtime) clientRef() *Client {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.client
}

func (r *Runtime) dedupRef() *DedupGate {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.dedup
}

func (r *Runtime) doorRef() *Door {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.door
}

// Close stops the consumer, the supervisor and the archive timer, and releases
// Redis. The ledger is not touched: it belongs to the composition root.
func (r *Runtime) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	cancel := r.cancel
	r.cancel = nil
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	r.wg.Wait()
	r.mu.Lock()
	dedup, client := r.dedup, r.client
	r.client = nil
	r.mu.Unlock()
	if dedup != nil {
		_ = dedup.SaveState(r.cfg.StateRoot)
	}
	if client != nil {
		return client.Close()
	}
	return nil
}

// Enabled reports whether a light-hub runtime is in force.
func (r *Runtime) Enabled() bool { return r != nil && r.enabled }

// Profile is the resolved profile name.
func (r *Runtime) Profile() string {
	if r == nil {
		return ProfileStandalone
	}
	return r.profile
}

// Since is the profile's adoption timestamp.
func (r *Runtime) Since() string {
	if r == nil {
		return ""
	}
	return r.since
}

// RuntimeMode is the state machine's current state.
func (r *Runtime) RuntimeMode() Mode {
	if r == nil {
		return ModeStandalone
	}
	return Mode(r.mode.Load().(string))
}

// BootCode is the code the boot recorded for a degraded start ("" when the boot
// was clean). It is what a caller reports as the reason the queue is not live.
func (r *Runtime) BootCode() types.ErrorCode {
	if r == nil {
		return ""
	}
	return types.ErrorCode(r.bootCode.Load().(string))
}

// Door is the ingestion seam, nil while the queue is not attached.
func (r *Runtime) Door() *Door {
	if r == nil {
		return nil
	}
	return r.doorRef()
}

// Dedup is the dedup gate (nil while the queue is not attached).
func (r *Runtime) Dedup() *DedupGate {
	if r == nil {
		return nil
	}
	return r.dedupRef()
}

// Consumer is the ledger writer (nil while the queue is not attached). A rewire
// installs a NEW one, so a caller must read it again after a recovery rather
// than hold the value it saw at boot.
func (r *Runtime) Consumer() *Consumer {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.consumer
}

// Counters is the ingestion-path counter snapshot.
type IngestCounters struct {
	Ingested  int64
	Refused   int64
	Fallbacks int64
	RouteA    int64
	RouteB    int64
}

// IngestCounters returns the counters the status surface reports alongside the
// Redis ones.
func (r *Runtime) IngestCounters() IngestCounters {
	if r == nil {
		return IngestCounters{}
	}
	return IngestCounters{
		Ingested:  r.countIngested.Load(),
		Refused:   r.countRefused.Load(),
		Fallbacks: r.countFallback.Load(),
		RouteA:    r.routeA.Load(),
		RouteB:    r.routeB.Load(),
	}
}

// ---- internals ----

func (r *Runtime) eachRoute() {
	r.routeA.Store(0)
	r.routeB.Store(0)
}

func (r *Runtime) countRoute(route RouteDecision) {
	if route == RouteB {
		r.routeB.Add(1)
		return
	}
	r.routeA.Add(1)
}

func (r *Runtime) hostID() string {
	if r.cfg.HostID != "" {
		return r.cfg.HostID
	}
	return r.redis.HostID
}

func (r *Runtime) hubID() string {
	if r.cfg.HubID != "" {
		return r.cfg.HubID
	}
	return r.hostID()
}

func (r *Runtime) connected() bool { return Mode(r.mode.Load().(string)) == ModeUp }

func (r *Runtime) degraded() bool {
	switch Mode(r.mode.Load().(string)) {
	case ModeUp, ModeStandalone:
		return false
	default:
		return true
	}
}

func (r *Runtime) degradedReason() string {
	v, _ := r.reason.Load().(string)
	return v
}

func (r *Runtime) setMode(m Mode, reason string) {
	r.mode.Store(string(m))
	r.reason.Store(reason)
}

func (r *Runtime) failureMode() Mode {
	if r.redis.RequireRedis {
		return ModeRefusing
	}
	return ModeUnavailable
}

func (r *Runtime) setBootCode(err error) {
	if err == nil {
		return
	}
	r.bootCode.Store(string(CodeOf(err)))
}

func (r *Runtime) setArchivePaused(paused bool) {
	r.mu.Lock()
	r.archivePaused = paused
	r.mu.Unlock()
}

// ArchivePaused reports whether the last pass left the queue paused.
func (r *Runtime) ArchivePaused() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.archivePaused
}

// recordLifecycle writes one lifecycle record through the ledger seam (SPEC-13
// §4.1 step 4: lifecycle writes the boot's own records). A hub with no ledger
// (a test runtime) simply logs.
func (r *Runtime) recordLifecycle(ctx context.Context, op string, payload map[string]any) {
	if r == nil || r.cfg.Ledger == nil {
		return
	}
	if payload == nil {
		payload = map[string]any{}
	}
	if _, ok := payload["op"]; !ok {
		payload["op"] = op
	}
	payload["profile"] = r.profile
	_, _ = r.cfg.Ledger.Append(ctx, types.RecordDraft{
		Kind:    types.KLifecycle,
		Origin:  types.Origin{HostID: r.hostID(), Source: "hub"},
		Actor:   types.Actor{Kind: types.ActorDaemon, ID: "hub"},
		Payload: payload,
	})
}

// RecordBoot writes the one lifecycle record that says the profile booted in the
// state it booted in. It is called by the composition root after Start, so the
// record describes the state that is actually serving.
func (r *Runtime) RecordBoot(ctx context.Context) {
	if r == nil || !r.enabled {
		return
	}
	payload := map[string]any{
		"op":              "hub_boot",
		"profile":         r.profile,
		"mode":            string(r.RuntimeMode()),
		"degraded_reason": r.degradedReason(),
		"stream":          r.redis.Stream,
		"group":           r.redis.Group,
		"consumer":        r.consumerName(),
		"archive":         r.archiveDescribe(),
	}
	if code := r.BootCode(); code != "" {
		payload["error_code"] = string(code)
		if code == types.CodeHub003 || code == types.CodeHub002 {
			payload["behaviour"] = "degraded start: the standalone in-process path is serving until redis returns"
		}
	}
	if r.target == nil {
		payload["archive"] = "paused: no usable duckbrain namespace/endpoint"
	}
	r.recordLifecycle(ctx, "hub_boot", payload)
}

func (r *Runtime) consumerName() string {
	if c := r.clientRef(); c != nil {
		return c.ConsumerName()
	}
	if r.redis.Consumer != "" {
		return r.redis.Consumer
	}
	return r.redis.HostID
}

func (r *Runtime) archiveDescribe() string {
	if r.target == nil {
		return "unconfigured"
	}
	return r.target.Describe()
}
