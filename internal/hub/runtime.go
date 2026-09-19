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

	mu             sync.Mutex
	consumerCancel context.CancelFunc
	lifeCtx        context.Context
	archivePaused  bool

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

// attach wires a live client: gate → door → consumer. The group is created here
// and a TROUBLE-HUB-005 refusal is returned, never swallowed: a queue nobody
// drains must not accept traffic (SPEC-13 §5).
func (r *Runtime) attach(ctx context.Context, c *Client) error {
	if err := c.EnsureGroup(ctx); err != nil {
		return err
	}
	r.client = c
	r.dedup = NewDedupGate(r.redis, c)
	r.door = NewDoor(c, r.dedup, DoorConfig{
		Route:  RouteA,
		HostID: r.hostID(),
		HubID:  r.hubID(),
		Ack:    r.cfg.AckCursor,
		Wait:   r.redis.DoorWait,
	})
	r.consumer = NewConsumer(c, r.cfg.Ledger, r.dedup, r.redis).
		WithCompletion(r.door.Complete).
		WithStrandedHook(r.onStranded).
		WithLogger(r.cfg.Log)
	r.setMode(ModeUp, DegradedNone)
	return nil
}

// Start launches the consumer, the reconnect supervisor (only when needed) and
// the archive timer. It is idempotent: a second call is a no-op.
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
	needSupervisor := !r.connected()
	r.mu.Unlock()

	if !needSupervisor {
		r.startConsumer(cctx)
	} else {
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.supervise(cctx)
		}()
	}
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
	if r.consumer == nil {
		r.mu.Unlock()
		return
	}
	if r.consumerCancel != nil {
		r.mu.Unlock()
		return
	}
	if r.lifeCtx != nil {
		ctx = r.lifeCtx
	}
	cs := r.consumer
	runCtx, cancel := context.WithCancel(ctx)
	r.consumerCancel = cancel
	r.mu.Unlock()
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer func() {
			r.mu.Lock()
			r.consumerCancel = nil
			r.mu.Unlock()
		}()
		if err := cs.Run(runCtx); err != nil {
			r.cfg.Log("hub: consumer stopped: %v", err)
		}
	}()
}

// supervise retries the queue while the profile is degraded at boot, and rewires
// the consumer the moment Redis returns (SPEC-13 §4.3 row "Redis returns":
// "rewires the consumer", lifecycle `redis_restored`).
func (r *Runtime) supervise(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if r.connected() {
			continue
		}
		r.reconnect(ctx)
	}
}

// reconnect is one attempt to bring the queue back (SPEC-13 §4.3 row "Redis
// returns": "rewires the consumer", lifecycle `redis_restored`). It reports
// whether the queue is live afterwards, and it is callable outside the ticker so
// a caller can force a rewire (the `trouble hub` verbs) instead of waiting.
func (r *Runtime) reconnect(ctx context.Context) bool {
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
		rec, err := r.door.Offer(ctx, draft)
		if err != nil {
			r.markLost(err)
			return types.Record{}, err
		}
		r.countIngested.Add(1)
		r.countRoute(r.door.Route())
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
func (r *Runtime) markLost(err error) {
	code := CodeOf(err)
	reason := DegradedReasonFor(code, r.redis.RequireRedis)
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
	if r.dedup != nil {
		_ = r.dedup.SaveState(r.cfg.StateRoot)
	}
	if r.client != nil {
		err := r.client.Close()
		r.client = nil
		return err
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
	return r.door
}

// Dedup is the dedup gate (nil while the queue is not attached).
func (r *Runtime) Dedup() *DedupGate {
	if r == nil {
		return nil
	}
	return r.dedup
}

// Consumer is the ledger writer (nil while the queue is not attached).
func (r *Runtime) Consumer() *Consumer {
	if r == nil {
		return nil
	}
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
	if r.client != nil {
		return r.client.ConsumerName()
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
