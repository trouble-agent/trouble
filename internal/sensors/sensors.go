package sensors

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// Package sensors is the detection plane (SPEC-03). Six sensor kinds feed one
// ladder through one signature space; the package owns observation, rule
// evaluation, storm control and liveness, and nothing else. It never calls the
// registry, never holds a tool handle and never spawns anything except
// `journalctl`.
//
// Binding constraints encoded here:
//   - sampling is the source of truth; PSI triggers are an accelerator only;
//   - a seek failure is never "no logs";
//   - this fleet's units are USER units, so system-only D-Bus watching is an
//     ALL-GREEN lie;
//   - every wake re-reads its counters before any rule evaluation;
//   - an unarmed PSI fd is never placed in an epoll set;
//   - memory pressure on the host is an incident the daemon files about itself;
//   - a hot-reload failure keeps the previous rule set (a broken edit never
//     disarms the daemon).

// EmitFunc is the injected ledger write boundary. SPEC-03 §2 writes
// `emit func(context.Context, types.Record) error`; the SPEC-01 seam that
// actually exists is Ledger.Append(RecordDraft) (Record, error), and the
// return value is what lets Canary() prove a canary *landed* instead of
// assuming it did, so this package takes that shape.
type EmitFunc func(ctx context.Context, d types.RecordDraft) (types.Record, error)

// RedactFunc is the injected scrub boundary (SPEC-02's Scrubber.Scrub).
type RedactFunc func(b []byte, target string) (types.ScrubResult, error)

// Heartbeat expectations of SPEC-03 §3.9 (the "stale after" column).
var staleAfter = map[types.SensorKind]time.Duration{
	types.SenPSI:      10 * time.Second,
	types.SenJournald: 90 * time.Second,
	types.SenDBus:     90 * time.Second,
	types.SenDisk:     180 * time.Second,
	types.SenTimers:   180 * time.Second,
	types.SenInotify:  900 * time.Second,
}

// canaryInterval and canaryLandDeadline are the §3.9 canary pins.
const (
	canaryInterval     = 10 * time.Minute
	canaryLandDeadline = 30 * time.Second
	reloadTimeout      = time.Second
	reloadStrikeLimit  = 3
	ledgerQueueDepthOK = 0.75
)

// Sensors is the detection plane (SPEC-03 §2). Every exported method below is
// the interface SPEC-10 and SPEC-05 consume; no internal shape leaks.
type Sensors struct {
	cfg    sensorConfig
	emit   EmitFunc
	redact RedactFunc
	now    func() time.Time

	hostID string
	actor  types.Actor

	rules   atomic.Pointer[ruleSet]
	probe   atomic.Pointer[probeResult]
	ruleGen atomic.Uint64
	booted  atomic.Bool

	rt map[types.SensorKind]*sensorRuntime

	psiState psiSamplerState
	jlState  journalState
	dbState  dbusState
	dkState  diskState
	tmState  timerState
	inoState inotifyState

	br   *breakerTable
	stab *stabilizer
	cool *cooldownTable

	suppressed  atomic.Uint64
	evaluations atomic.Uint64
	fired       atomic.Uint64
	merges      atomic.Uint64
	arrivals    atomic.Uint64

	errMu     sync.Mutex
	errSeen   map[types.ErrorCode]time.Time
	gapOpen   map[string]*gapEpisode
	startTS   time.Time
	lastDepth atomic.Int64
	depthCap  atomic.Int64
	loadShed  atomic.Bool

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	probing  sync.Mutex
	probed   atomic.Bool
	reloadMu sync.Mutex
	reloadAt atomic.Int64
	strikes  atomic.Int64
	lastErr  atomic.Pointer[string]

	canarySeq     atomic.Uint64
	canaryLanded  atomic.Int64
	canaryMissing atomic.Uint64
	// reloadDeadline overrides the 1s reload abandon budget. It exists so the
	// abandon path can be exercised deterministically; production always uses
	// reloadTimeout.
	reloadDeadline time.Duration

	reloadPending atomic.Bool
	reloadCh      chan struct{}
	sighup        chan os.Signal
	inotifyOff    atomic.Bool
}

type sensorRuntime struct {
	kind      types.SensorKind
	enabled   atomic.Bool
	degraded  atomic.Bool
	reason    atomic.Pointer[string]
	lastOK    atomic.Int64 // unix nanos
	lastEvent atomic.Int64 // unix nanos
	events    atomic.Uint64
	drops     atomic.Uint64
	gaps      atomic.Int64
}

// gapEpisode tracks one open dropout so a dropout emits exactly one gap record
// and its recovery emits exactly one sensor_recovered (SPEC-03 §3.9).
type gapEpisode struct {
	key     string
	sensor  types.SensorKind
	scope   string
	from    time.Time
	cause   string
	detail  string
	estLost int
	lostFn  func() int
}

// lost is the episode's best available count of what was missed: an exact
// number when the episode can compute one, -1 when it genuinely cannot.
func (e *gapEpisode) lost() int {
	if e.lostFn != nil {
		return e.lostFn()
	}
	return e.estLost
}

// New builds the detection plane (SPEC-03 §2). Configuration arrives as
// resolved ConfigValues (SPEC-12 owns precedence) and is decoded into the
// package-private sensorConfig; an unknown `sensors.*` key fails loudly.
func New(vals []types.ConfigValue, emit EmitFunc, redact RedactFunc, now func() time.Time) (*Sensors, error) {
	cfg, err := decodeConfig(vals)
	if err != nil {
		return nil, err
	}
	if emit == nil {
		return nil, errors.New("sensors: emit is required (the ledger's Append is the only writer)")
	}
	if now == nil {
		now = time.Now
	}
	if redact == nil {
		redact = identityRedact
	}
	s := &Sensors{
		cfg:      cfg,
		emit:     emit,
		redact:   redact,
		now:      now,
		hostID:   cfg.hostID,
		actor:    cfg.actor,
		rt:       map[types.SensorKind]*sensorRuntime{},
		br:       newBreakerTable(cfg.limits),
		stab:     newStabilizer(),
		cool:     newCooldownTable(),
		errSeen:  map[types.ErrorCode]time.Time{},
		gapOpen:  map[string]*gapEpisode{},
		stopCh:   make(chan struct{}),
		reloadCh: make(chan struct{}, 1),
	}
	for _, k := range types.SensorKinds {
		s.rt[k] = &sensorRuntime{kind: k}
		s.rt[k].enabled.Store(false)
		s.setReason(k, "not started")
	}
	s.startTS = s.now()
	s.cfg.hostID = cfg.hostID
	s.depthCap.Store(int64(cfg.journald.queue))

	// Startup step 3: load + validate rules.d, keeping the shipped defaults on
	// any failure (SPEC-03 §4). An absent directory (first boot, before install)
	// is not a failure; an existing rules.d that yields zero rules IS — an empty
	// directory must never silently disarm the daemon.
	set, problems := s.initialRules()
	if problems != nil {
		s.noteRuleFailure(problems)
	}
	if set == nil {
		set = &ruleSet{rules: defaultRules()}
		if def, dErr := compileRules(set.rules, cfg.knownModules, cfg.modulesKnownSet); dErr == nil {
			set = def
		}
		s.setReasonAll(defaultsNote(problems))
	}
	s.installRules(set)
	return s, nil
}

// initialRules loads the configured rules directory, distinguishing a missing
// directory (defaults, quietly) from an existing one with no usable rules
// (defaults + a recorded refusal).
func (s *Sensors) initialRules() (*ruleSet, *ruleLoadErrors) {
	if _, err := os.Stat(s.cfg.rulesDir); err != nil && os.IsNotExist(err) {
		return nil, nil
	}
	set, problems := s.loadAndCompileFromDir()
	if problems != nil {
		return nil, problems
	}
	if set == nil || len(set.rules) == 0 {
		return nil, &ruleLoadErrors{Problems: []*ruleProblem{{
			Code: types.CodeSensors017, File: s.cfg.rulesDir,
			Msg: "no rule files found; the shipped defaults are active (an empty rules.d is not a valid rule set)",
		}}}
	}
	return set, nil
}

func defaultsNote(problems *ruleLoadErrors) string {
	if problems != nil {
		return fmt.Sprintf("shipped defaults active: %s", problems.First().Error())
	}
	return "shipped defaults active: no rules.d installed yet"
}

// setReasonAll sets the same health reason on every sensor runtime.
func (s *Sensors) setReasonAll(reason string) {
	for _, k := range types.SensorKinds {
		s.setReason(k, reason)
	}
}

func identityRedact(b []byte, _ string) (types.ScrubResult, error) {
	return types.ScrubResult{Value: b, BytesIn: len(b)}, nil
}

// ---- lifecycle ----

// Probe runs the capability probe and writes exactly one capability_probe event
// (SPEC-03 §2, §3.2 P9).
//
// DEVIATION (recorded in the card metadata): SPEC-03 §3.2 shows the probe
// record with `sig: ""` because it is "not sig-keyed". SPEC-01 §3.1/§3.2's
// ledger refuses an empty sig for kind=event (SigRequired), so this package
// emits a stable, non-incident sig and marks the record
// `payload.sig_keyed = false`. The intent — the probe is never an incident
// candidate — is preserved by carrying fire=false; dropping the sig would mean
// bypassing the only writer.
func (s *Sensors) Probe(ctx context.Context) error {
	s.probing.Lock()
	defer s.probing.Unlock()
	res, err := s.probeCapabilities(ctx)
	if err != nil {
		return err
	}
	s.probe.Store(res)
	s.booted.Store(true)
	payload := res.Payload
	payload["sig_keyed"] = false
	payload["fire"] = false
	payload["sensor"] = string(types.SenPSI)
	payload["error_code"] = ""
	if len(res.Codes) > 0 {
		payload["error_code"] = string(res.Codes[0])
	}
	d := types.RecordDraft{
		Kind:    types.KEvent,
		Sig:     sigFor(types.SrcPSI, "capability_probe", "v1").String(),
		Origin:  types.Origin{HostID: s.hostID, HubID: s.cfg.hubID, Source: string(types.SrcPSI)},
		Actor:   s.actor,
		Payload: payload,
	}
	if _, err := s.emit(ctx, d); err != nil {
		return fmt.Errorf("sensors: emitting the capability probe: %w", err)
	}
	if res.Mode == psiDisabled {
		s.setSensor(types.SenPSI, false, true, res.Reason)
	}
	return nil
}

// Start starts every enabled sensor (SPEC-03 §4 startup order). Probe runs
// first when it has not been run yet.
func (s *Sensors) Start(ctx context.Context) error {
	if !s.probed.CompareAndSwap(false, true) {
		return errors.New("sensors: Start called twice")
	}
	if s.probe.Load() == nil {
		if err := s.Probe(ctx); err != nil {
			return err
		}
	}
	if s.strikeProblem() != nil {
		s.emitReloadFailure(ctx, s.strikeErrors(), "boot", s.ruleLoadSkippedReason())
	}
	s.installSignals()
	// §4 step 4: PSI sampler (+ triggers).
	if err := s.startPSI(ctx); err != nil {
		return err
	}
	// §4 steps 5-9: the other five sensors, each degrading only itself.
	if err := s.startJournald(ctx); err != nil {
		s.setSensor(types.SenJournald, false, true, err.Error())
	}
	if err := s.startDBus(ctx); err != nil {
		s.setSensor(types.SenDBus, false, true, err.Error())
	}
	if err := s.startDisk(ctx); err != nil {
		s.setSensor(types.SenDisk, false, true, err.Error())
	}
	if err := s.startTimers(ctx); err != nil {
		s.setSensor(types.SenTimers, false, true, err.Error())
	}
	if err := s.startInotify(ctx); err != nil {
		s.setSensor(types.SenInotify, false, true, err.Error())
	}
	// §4 step 10: heartbeat ticker + canary timer.
	s.wg.Add(1)
	go s.runLiveness(ctx)
	s.wg.Add(1)
	go s.runCanary(ctx)
	s.wg.Add(1)
	go s.runReloadSweep(ctx)
	return nil
}

// Stop is the SIGTERM drain (SPEC-03 §4): canary/heartbeat → inotify watches →
// disk/timers sweeps → D-Bus → journald child + cursor flush → PSI triggers →
// a final gap for any scope still degraded. It returns only after every
// goroutine has joined.
func (s *Sensors) Stop(ctx context.Context) error {
	select {
	case <-s.stopCh:
		return nil
	default:
	}
	s.stopOnce.Do(func() { close(s.stopCh) })
	s.stopPSI()
	s.stopInotify()
	s.stopDBus()
	s.stopTimers()
	s.stopDisk()
	s.stopJournald()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		// The child reaped with a bound; the goroutine leaks only if the
		// ledger's emit blocks forever, which is the ledger's own fault line.
	case <-ctx.Done():
	}
	// Invariant P6: every armed fd is closed, so the counts must agree.
	if armed, sets := s.psiState.armed.Load(), s.psiState.epollSets.Load(); armed != sets {
		s.recError(types.SenPSI, types.CodeSensors004,
			fmt.Sprintf("armed_fds=%d epoll_sets=%d at shutdown", armed, sets))
	}
	// A degraded scope that never recovered gets its final documented gap.
	s.errMu.Lock()
	open := make([]*gapEpisode, 0, len(s.gapOpen))
	for _, e := range s.gapOpen {
		open = append(open, e)
	}
	s.gapOpen = map[string]*gapEpisode{}
	s.errMu.Unlock()
	for _, e := range open {
		s.emitGap(ctx, e, s.now())
	}
	return nil
}

// Reload performs the atomic rule-set swap (SPEC-03 §3.7). Any failure keeps
// the previous set: a broken edit never disarms the daemon.
func (s *Sensors) Reload(ctx context.Context) error {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	started := s.now()
	type result struct {
		set      *ruleSet
		problems *ruleLoadErrors
	}
	ch := make(chan result, 1)
	go func() {
		set, problems := s.loadAndCompileFromDir()
		if problems == nil && set != nil && len(set.rules) == 0 {
			// An empty directory must not silently disarm the daemon: the
			// shipped defaults stay authoritative.
			set = nil
			problems = &ruleLoadErrors{Problems: []*ruleProblem{{
				Code: types.CodeSensors017, File: s.cfg.rulesDir,
				Msg: "no rule files found; keeping the previous set (an empty rules.d is not a valid rule set)",
			}}}
		}
		ch <- result{set, problems}
	}()
	var r result
	deadline := reloadTimeout
	if s.reloadDeadline > 0 {
		deadline = s.reloadDeadline
	}
	select {
	case r = <-ch:
	case <-time.After(deadline):
		s.strikes.Add(1)
		s.noteRuleFailure(&ruleLoadErrors{Problems: []*ruleProblem{{
			Code: types.CodeSensors019, File: s.cfg.rulesDir, Msg: "reload_timeout",
		}}})
		s.emitReloadFailure(ctx, s.strikeErrors(), "reload_timeout", "")
		return fmt.Errorf("sensors: rule reload exceeded %s; previous set kept", deadline)
	case <-ctx.Done():
		return ctx.Err()
	}
	if r.problems != nil {
		s.strikes.Add(1)
		s.noteRuleFailure(r.problems)
		s.emitReloadFailure(ctx, r.problems, "", "")
		s.afterReloadFailure(ctx)
		return r.problems
	}
	prev := s.rules.Load()
	// Carry stabilization timers over for rules whose predicate is unchanged,
	// and tell the operator which ones reset (SPEC-03 §3.7).
	resetRules := s.stab.carryOverFingerprints(fingerprintMap(r.set))
	if len(resetRules) > 0 {
		s.emitResetNotice(ctx, resetRules)
	}
	s.installRules(r.set)
	s.strikes.Store(0)
	s.reloadAt.Store(s.now().UnixNano())
	s.clearReason(types.SenPSI)
	s.clearReason(types.SenJournald)
	s.clearReason(types.SenDBus)
	s.clearReason(types.SenDisk)
	s.clearReason(types.SenTimers)
	s.clearReason(types.SenInotify)
	_ = prev
	if d := s.now().Sub(started); d > 250*time.Millisecond {
		s.emitReloadSlow(ctx, d)
	}
	return nil
}

func fingerprintMap(set *ruleSet) map[string]string {
	out := map[string]string{}
	for _, rules := range set.bySource {
		for _, cr := range rules {
			out[cr.rule.Name] = cr.matchKey
		}
	}
	return out
}

// Health fills /health.json sensors[] (SPEC-03 §2, §3.9). Atomics only, so it
// stays well inside the 50 ms budget with a full rule set installed.
func (s *Sensors) Health() []types.SensorHealth {
	out := make([]types.SensorHealth, 0, len(types.SensorKinds))
	for _, k := range types.SensorKinds {
		rt := s.rt[k]
		if rt == nil || !rt.enabled.Load() {
			continue
		}
		h := types.SensorHealth{
			Sensor:      k,
			Enabled:     true,
			Degraded:    rt.degraded.Load(),
			EventsTotal: rt.events.Load(),
			Gaps:        int(rt.gaps.Load()),
			Dropped:     rt.drops.Load(),
		}
		if p := rt.reason.Load(); p != nil {
			h.Reason = *p
		}
		ok := rt.lastOK.Load()
		if ok > 0 {
			ts := time.Unix(0, ok)
			h.LastSuccessTS = types.FormatUTC(ts)
		}
		le := rt.lastEvent.Load()
		if le > 0 {
			ts := time.Unix(0, le)
			h.LastEventTS = types.FormatUTC(ts)
			h.LastEventAgeS = s.now().Sub(ts).Seconds()
		}
		out = append(out, h)
	}
	return out
}

// Liveness fills the per-source entries verification reads (SPEC-03 §3.9).
// Liveness is proven by the probe (LastSuccessTS), never by arrivals: a quiet
// journal is healthy, and entry silence is not staleness.
func (s *Sensors) Liveness() []types.SourceLiveness {
	out := make([]types.SourceLiveness, 0, len(types.SensorKinds))
	for _, k := range types.SensorKinds {
		rt := s.rt[k]
		if rt == nil || !rt.enabled.Load() {
			continue
		}
		maxAge := staleAfter[k].Seconds()
		l := types.SourceLiveness{
			HostID:   s.hostID,
			Source:   string(k.SigSource()),
			Expected: true,
			MaxAgeS:  maxAge,
		}
		if le := rt.lastEvent.Load(); le > 0 {
			ts := time.Unix(0, le)
			l.LastEventTS = types.FormatUTC(ts)
			l.LastEventAgeS = s.now().Sub(ts).Seconds()
		}
		ok := rt.lastOK.Load()
		l.Alive = ok > 0 && s.now().Sub(time.Unix(0, ok)).Seconds() <= maxAge
		if rt.degraded.Load() {
			l.Alive = false
		}
		out = append(out, l)
	}
	return out
}

// Breakers publishes the storm-breaker state for /breakers and the ladder gate.
func (s *Sensors) Breakers() []types.Breaker { return s.br.Snapshot() }

// Canary injects one canary event through the real pipeline
// (normalization → dedup → ledger) and reports its id (SPEC-03 §2, §3.9).
func (s *Sensors) Canary(ctx context.Context, scope string) (string, error) {
	n := s.canarySeq.Add(1)
	id := fmt.Sprintf("can_%s%03d", s.startTS.UTC().Format("060102150405"), n)
	sensor := types.SenPSI
	detail := map[string]any{
		"kind":        "canary",
		"canary_id":   id,
		"injected_ts": types.FormatUTC(s.now()),
		"scope":       scope,
		"count":       1,
		"value":       0,
		"unit":        "",
		"msg":         "",
	}
	ev := types.SensorEvent{
		ID:     types.NewID(types.PEv),
		TS:     types.FormatUTC(s.now()),
		Sensor: sensor,
		Scope:  scope,
		Value:  0,
		Unit:   "",
		Detail: detail,
		Sig:    sigFor(types.SrcPSI, "canary", scope, id),
	}
	payload := s.eventPayload(ev, "")
	payload["kind"] = "canary"
	payload["canary_id"] = id
	payload["injected_ts"] = types.FormatUTC(s.now())
	payload["observed_ts"] = types.FormatUTC(s.now())
	payload["latency_ms"] = 0
	d := types.RecordDraft{
		Kind:    types.KCanary,
		Sig:     ev.Sig.String(),
		Origin:  types.Origin{HostID: s.hostID, HubID: s.cfg.hubID, Source: string(types.SrcPSI)},
		Actor:   s.actor,
		Payload: payload,
	}
	started := s.now()
	rec, err := s.emit(ctx, d)
	if err != nil {
		payload["landed"] = false
		// The gap gets its own bounded context: the caller's context may be the
		// very thing that expired, and a missing canary must still be recorded.
		gctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		s.emitGapNow(gctx, "psi", scope, "canary_missing", fmt.Sprintf("canary %s was not accepted: %v", id, err))
		cancel()
		s.canaryMissing.Add(1)
		return id, fmt.Errorf("sensors: canary %s did not land: %w", id, err)
	}
	payload["landed"] = true
	payload["observed_ts"] = types.FormatUTC(s.now())
	payload["latency_ms"] = s.now().Sub(started).Milliseconds()
	rec.Sig = ev.Sig.String()
	s.canaryLanded.Store(s.now().UnixNano())
	return id, nil
}

// ---- runtime bookkeeping ----

// setSensor publishes one sensor's state. A start with no reason clears any
// previous one: a sensor that is running cleanly must not keep reporting why it
// was degraded an hour ago (a stale reason is how /health starts lying).
func (s *Sensors) setSensor(k types.SensorKind, enabled, degraded bool, reason string) {
	rt := s.rt[k]
	if rt == nil {
		return
	}
	rt.enabled.Store(enabled)
	rt.degraded.Store(degraded)
	if reason == "" {
		if enabled && !degraded {
			empty := ""
			rt.reason.Store(&empty)
		}
		return
	}
	s.setReason(k, reason)
}

func (s *Sensors) setReason(k types.SensorKind, reason string) {
	if rt := s.rt[k]; rt != nil {
		r := reason
		rt.reason.Store(&r)
	}
}

// clearReason drops a stale degradation reason only on an explicit recovery
// (SPEC-03 §3.9: a degraded sensor keeps a non-empty Reason until recovery).
func (s *Sensors) clearReason(k types.SensorKind) {
	if rt := s.rt[k]; rt != nil {
		rt.degraded.Store(false)
		empty := ""
		rt.reason.Store(&empty)
	}
}

func (s *Sensors) sensorOK(k types.SensorKind) {
	if rt := s.rt[k]; rt != nil {
		rt.lastOK.Store(s.now().UnixNano())
	}
}

func (s *Sensors) sensorError(k types.SensorKind, scope string, err error) {
	_ = scope
	_ = err
	if rt := s.rt[k]; rt != nil {
		rt.drops.Add(1)
	}
}

// noteRuleFailure remembers the reload strike state.
func (s *Sensors) noteRuleFailure(problems *ruleLoadErrors) {
	if problems == nil || len(problems.Problems) == 0 {
		return
	}
	p := problems.Problems[0]
	msg := p.Error()
	s.lastErr.Store(&msg)
	if p.Code == types.CodeSensors018 || p.Code == types.CodeSensors017 || p.Code == types.CodeSensors019 {
		for _, k := range types.SensorKinds {
			s.setReason(k, fmt.Sprintf("rule reload failed at %s: %s", types.FormatUTC(s.now()), p.Error()))
		}
	}
}

func (s *Sensors) strikeProblem() *ruleProblem {
	if p := s.lastErr.Load(); p != nil {
		return &ruleProblem{Code: types.CodeSensors019, Msg: *p}
	}
	return nil
}

// strikeErrors wraps the remembered problem as the one-element refusal set the
// reload-failure record needs.
func (s *Sensors) strikeErrors() *ruleLoadErrors {
	p := s.strikeProblem()
	if p == nil {
		return nil
	}
	return &ruleLoadErrors{Problems: []*ruleProblem{p}}
}

func (s *Sensors) ruleLoadSkippedReason() string {
	if rs := s.rules.Load(); rs != nil {
		return rs.skipped
	}
	return ""
}

// afterReloadFailure implements the §3.7 loop guard: after 3 consecutive
// failed reloads the inotify trigger is disabled and only SIGHUP retries
// immediately.
func (s *Sensors) afterReloadFailure(ctx context.Context) {
	if s.strikes.Load() < reloadStrikeLimit {
		return
	}
	if !s.inotifyOff.Swap(true) {
		s.emitRuleReloadEvent(ctx, map[string]any{
			"kind":       "rule_reload",
			"reason":     "3 consecutive reload failures: inotify trigger disabled, SIGHUP and the 60s mtime sweep continue",
			"strikes":    s.strikes.Load(),
			"error_code": string(types.CodeSensors019),
		})
	}
}

func (s *Sensors) emitReloadFailure(ctx context.Context, problems *ruleLoadErrors, reason, skipped string) {
	p := problems.First()
	payload := map[string]any{
		"kind":       "rule_reload",
		"error_code": string(p.Code),
		"file":       p.File,
		"line":       p.Line,
		"rule":       p.Rule,
		"detail":     p.Msg,
	}
	if reason != "" {
		payload["reason"] = reason
	}
	if skipped != "" {
		payload["auto_grants_check"] = skipped
	}
	s.emitRuleReloadEvent(ctx, payload)
}

func (s *Sensors) emitReloadSlow(ctx context.Context, d time.Duration) {
	s.emitRuleReloadEvent(ctx, map[string]any{
		"kind":       "rule_reload",
		"reason":     fmt.Sprintf("reload took %s (>250ms)", d),
		"error_code": string(types.CodeSensors019),
		"rules":      s.ruleCount(),
	})
}

func (s *Sensors) emitResetNotice(ctx context.Context, rules []string) {
	s.emitRuleReloadEvent(ctx, map[string]any{
		"kind":             "rule_reload",
		"reason":           "rule predicate changed: stabilization timers reset",
		"rules":            rules,
		"rule_state_reset": true,
		"error_code":       "",
	})
}

// emitRuleReloadEvent writes one payload.kind="rule_reload" event (SPEC-03 §3.7).
func (s *Sensors) emitRuleReloadEvent(ctx context.Context, payload map[string]any) {
	payload["sig_keyed"] = false
	payload["fire"] = false
	if _, ok := payload["error_code"]; !ok {
		payload["error_code"] = ""
	}
	d := types.RecordDraft{
		Kind:    types.KEvent,
		Sig:     sigFor(types.SrcPSI, "rule_reload", "v1").String(),
		Origin:  types.Origin{HostID: s.hostID, HubID: s.cfg.hubID, Source: string(types.SrcGeneric)},
		Actor:   s.actor,
		Payload: payload,
	}
	_, _ = s.emit(ctx, d)
}

// capabilityCodes are the "once per boot per sensor" codes (SPEC-03 §5 last
// paragraph). Everything else is governed by its own rate: an episode code must
// be able to recur (a second statfs failure an hour later, a second cursor
// problem after a rotation), and collapsing those into one-per-boot would make
// the ledger claim a host is healthy because it failed only once.
var capabilityCodes = map[types.ErrorCode]bool{
	types.CodeSensors001: true, types.CodeSensors002: true,
	types.CodeSensors003: true, types.CodeSensors004: true,
	types.CodeSensors005: true, types.CodeSensors006: true,
	types.CodeSensors009: true, types.CodeSensors013: true,
	types.CodeSensors016: true, types.CodeSensors025: true,
}

// recError records one error code with its once-per-boot dedup for the
// permanent capability class (SPEC-03 §5 last paragraph) and publishes it into
// a record so the code is never log-only.
func (s *Sensors) recError(k types.SensorKind, code types.ErrorCode, detail string) {
	s.errMu.Lock()
	_, seen := s.errSeen[code]
	s.errSeen[code] = s.now()
	s.errMu.Unlock()
	if seen && capabilityCodes[code] {
		return
	}
	rt := s.rt[k]
	if rt != nil && code.Is(types.ErrClassPermanent) {
		rt.degraded.Store(true)
		s.setReason(k, fmt.Sprintf("%s: %s", code, detail))
	}
	ctx := context.Background()
	payload := map[string]any{
		"kind":       "sensor_error",
		"sensor":     string(k),
		"error_code": string(code),
		"detail":     detail,
		"sig_keyed":  false,
		"fire":       false,
	}
	d := types.RecordDraft{
		Kind:    types.KEvent,
		Sig:     sigFor(k.SigSource(), "sensor_error", string(code)).String(),
		Origin:  types.Origin{HostID: s.hostID, HubID: s.cfg.hubID, Source: string(k.SigSource())},
		Actor:   s.actor,
		Payload: payload,
	}
	_, _ = s.emit(ctx, d)
}

// emitGap writes one gap record for a dropout episode (SPEC-03 §3.9). The
// GapRecord fields sit at the payload top level plus cause_detail and
// subsystem, which SPEC-04/07/09/12 reuse verbatim.
func (s *Sensors) emitGap(ctx context.Context, e *gapEpisode, to time.Time) {
	payload := map[string]any{
		"id":           types.NewID(types.PEv),
		"sensor":       string(e.sensor),
		"scope":        e.scope,
		"from_ts":      types.FormatUTC(e.from),
		"to_ts":        types.FormatUTC(to),
		"est_lost":     e.lost(),
		"cause":        e.cause,
		"cause_detail": e.detail,
		"subsystem":    "sensors:" + string(e.sensor),
		"kind":         "gap",
		"sig_keyed":    false,
		"fire":         false,
	}
	d := types.RecordDraft{
		Kind:    types.KGap,
		Origin:  types.Origin{HostID: s.hostID, HubID: s.cfg.hubID, Source: string(e.sensor.SigSource())},
		Actor:   s.actor,
		Payload: payload,
	}
	if _, err := s.emit(ctx, d); err != nil {
		return
	}
	if rt := s.rt[e.sensor]; rt != nil {
		rt.gaps.Add(1)
	}
}

// openGap opens a dropout episode; a second open for the same key is the same
// episode (one cause, one record).
func (s *Sensors) openGap(k types.SensorKind, scope, cause, detail string) {
	s.openGapLost(k, scope, cause, detail, -1)
}

// openGapLost opens a dropout episode with an explicit est_lost: a queue
// overflow knows how many entries it dropped, and reporting "unknown" when the
// number is known would be the dishonest direction of that field.
func (s *Sensors) openGapLost(k types.SensorKind, scope, cause, detail string, estLost int) {
	s.openGapFunc(k, scope, cause, detail, func() int { return estLost })
}

// openGapFunc opens a dropout episode whose est_lost is computed when the
// episode closes.
func (s *Sensors) openGapFunc(k types.SensorKind, scope, cause, detail string, lost func() int) {
	key := fmt.Sprintf("%s|%s|%s", k, scope, cause)
	s.errMu.Lock()
	if _, ok := s.gapOpen[key]; ok {
		s.errMu.Unlock()
		return
	}
	e := &gapEpisode{key: key, sensor: k, scope: scope, from: s.now(), cause: cause, detail: detail, lostFn: lost}
	s.gapOpen[key] = e
	s.errMu.Unlock()
	if rt := s.rt[k]; rt != nil {
		rt.degraded.Store(true)
		s.setReason(k, fmt.Sprintf("%s (%s)", cause, types.CodeSensors024))
	}
}

// closeGap closes an episode with a sensor_recovered event (SPEC-03 §3.9).
func (s *Sensors) closeGap(ctx context.Context, k types.SensorKind, scope, cause string) {
	key := fmt.Sprintf("%s|%s|%s", k, scope, cause)
	s.errMu.Lock()
	e, ok := s.gapOpen[key]
	if ok {
		delete(s.gapOpen, key)
	}
	s.errMu.Unlock()
	if !ok {
		return
	}
	now := s.now()
	s.emitGap(ctx, e, now)
	s.clearReason(k)
	payload := map[string]any{
		"kind":       "sensor_recovered",
		"sensor":     string(k),
		"scope":      scope,
		"cause":      cause,
		"detail":     map[string]any{"downtime_s": now.Sub(e.from).Seconds()},
		"sig_keyed":  false,
		"fire":       false,
		"error_code": "",
	}
	d := types.RecordDraft{
		Kind:    types.KEvent,
		Sig:     sigFor(k.SigSource(), "sensor_recovered", scope).String(),
		Origin:  types.Origin{HostID: s.hostID, HubID: s.cfg.hubID, Source: string(k.SigSource())},
		Actor:   s.actor,
		Payload: payload,
	}
	_, _ = s.emit(ctx, d)
}

// emitGapNow writes a standalone gap with explicit cause/detail and closes any
// open episode with the same key, so a condition that both opens an episode and
// files its record immediately produces exactly one record — never one here and
// another on recovery.
func (s *Sensors) emitGapNow(ctx context.Context, sensor, scope, cause, detail string) {
	k := types.SensorKind(sensor)
	if !k.Valid() {
		k = types.SenPSI
	}
	key := fmt.Sprintf("%s|%s|%s", k, scope, cause)
	s.errMu.Lock()
	e, open := s.gapOpen[key]
	if open {
		delete(s.gapOpen, key)
	}
	s.errMu.Unlock()
	now := s.now()
	if !open {
		e = &gapEpisode{sensor: k, scope: scope, from: now, cause: cause, detail: detail, estLost: -1}
	}
	if detail != "" {
		e.detail = detail
	}
	s.emitGap(ctx, e, now)
}

// ---- the rule pipeline ----

// handleEvent evaluates one SensorEvent against the live rule set and reports
// whether any rule's match held. Every event is recorded either way; only a
// match that completed its stabilization and passed the breaker gate tells the
// ladder to act (payload.fire = true).
func (s *Sensors) handleEvent(ctx context.Context, ev types.SensorEvent) bool {
	if rt := s.rt[ev.Sensor]; rt != nil {
		rt.events.Add(1)
		now := s.now()
		rt.lastEvent.Store(now.UnixNano())
		// An observation is proof the sensor just read its source successfully:
		// liveness is still decided by last success, never by arrival, but an
		// arrival IS a success when it happens.
		rt.lastOK.Store(now.UnixNano())
	}
	rs := s.rules.Load()
	now := s.now()
	sc := newEvalScope(evalVars(ev))
	matchedAny := false
	var fired []firingRule

	if rs != nil {
		for _, cr := range rs.bySource[ev.Sensor.SigSource()] {
			if !cr.rule.Enabled {
				continue // disabled rules are validated, never evaluated
			}
			key := ruleSigKey{rule: cr.rule.Name, sig: ev.Sig.String()}
			ok := true
			for _, p := range cr.prog {
				if !p.eval(sc) {
					ok = false
					break
				}
			}
			if !ok {
				s.stab.reset(key) // the condition stopped holding: timer restarts
				continue
			}
			matchedAny = true
			need := cr.rule.For.Std()
			if !s.stab.mark(key, cr.matchKey, need, now) {
				continue
			}
			cd := cr.rule.Cooldown.Std()
			if !s.cool.allow(key, cd, now) {
				s.suppressed.Add(1)
				continue
			}
			scopes := []string{
				"global",
				"source:" + string(ev.Sensor.SigSource()),
				"rule:" + cr.rule.Name,
			}
			allowed := true
			probed := false
			for _, scope := range scopes {
				d := s.br.Decide(scope, now)
				if d.probe {
					probed = true
				}
				if !d.allowed {
					allowed = false
					break
				}
			}
			if !allowed {
				s.suppressed.Add(1)
				continue
			}
			s.br.Observe(scopes, now)
			if probed {
				for _, scope := range scopes {
					if closed, trips := s.br.Close(scope, now); closed {
						s.emitRuleReloadEvent(ctx, map[string]any{
							"kind":       "breaker_closed",
							"scope":      scope,
							"trips":      trips,
							"error_code": "",
						})
					}
				}
			}
			s.cool.fired(key, now)
			s.evaluations.Add(1)
			s.fired.Add(1)
			fired = append(fired, firingRule{name: cr.rule.Name, rule: cr.rule})
		}
	}
	// A wake that crosses no boundary is counted as a spurious wake and
	// changes no incident state (P4). Firing is what decides.
	if ev.Wake && len(fired) == 0 && !matchedAny {
		s.psiState.spurious.Add(1)
	}
	payload := s.eventPayload(ev, "")
	if len(fired) > 0 {
		f := fired[0]
		payload["fire"] = true
		payload["rule"] = f.name
		payload["rules"] = firingNames(fired)
		payload["entry_rung"] = string(f.rule.EntryRung)
		payload["severity"] = string(f.rule.Severity)
		payload["subject"] = ev.Scope
		s.br.OpenIncident(ev.Sig.String(), []string{"global", "source:" + string(ev.Sensor.SigSource()), "rule:" + f.name}, now)
	} else {
		payload["fire"] = false
		payload["suppressed"] = matchedAny
	}
	d := types.RecordDraft{
		Kind:       types.KEvent,
		Sig:        ev.Sig.String(),
		Origin:     types.Origin{HostID: s.hostID, HubID: s.cfg.hubID, Source: string(ev.Sensor.SigSource())},
		Actor:      s.actor,
		Redactions: redactionCount(ev.Detail),
		Payload:    payload,
	}
	if _, err := s.emit(ctx, d); err != nil {
		if rt := s.rt[ev.Sensor]; rt != nil {
			rt.drops.Add(1)
		}
	}
	return matchedAny
}

type firingRule struct {
	name string
	rule types.Rule
}

func firingNames(in []firingRule) []string {
	out := make([]string, 0, len(in))
	for _, f := range in {
		out = append(out, f.name)
	}
	sort.Strings(out)
	return out
}

// eventPayload is the wire payload of a sensor event record: the SensorEvent
// fields plus the detail map rules read (SPEC-03 §3.1).
func (s *Sensors) eventPayload(ev types.SensorEvent, rule string) map[string]any {
	payload := map[string]any{
		"kind":       "sensor",
		"id":         ev.ID,
		"sensor":     string(ev.Sensor),
		"scope":      ev.Scope,
		"value":      ev.Value,
		"unit":       ev.Unit,
		"wake":       ev.Wake,
		"detail":     ev.Detail,
		"subject":    ev.Scope,
		"ts":         ev.TS,
		"sig_keyed":  true,
		"error_code": "",
	}
	if rule != "" {
		payload["rule"] = rule
	}
	return payload
}

func redactionCount(detail map[string]any) int {
	if v, ok := detail["redactions"].(int); ok {
		return v
	}
	return 0
}

// evalVars builds the evaluation scope: every sensor event field is resolvable
// from the detail map, with the event's own top-level fields layered on top.
func evalVars(ev types.SensorEvent) map[string]value {
	vars := make(map[string]value, len(ev.Detail)+12)
	for k, raw := range ev.Detail {
		if v, ok := toValue(raw); ok {
			vars[k] = v
		}
	}
	set := func(k string, v value) { vars[k] = v }
	set("value", numberValue(ev.Value))
	set("unit", stringValue(ev.Unit))
	set("scope", stringValue(ev.Scope))
	set("sensor", stringValue(string(ev.Sensor)))
	set("source", stringValue(string(ev.Sensor.SigSource())))
	set("wake", boolValue(ev.Wake))
	if _, ok := vars["count"]; !ok {
		set("count", numberValue(1))
	}
	if _, ok := vars["severity"]; !ok {
		set("severity", stringValue(string(types.SevInfo)))
	}
	if _, ok := vars["window_s"]; !ok {
		set("window_s", numberValue(0))
	}
	if _, ok := vars["age_s"]; !ok {
		set("age_s", numberValue(0))
	}
	if _, ok := vars["substr"]; !ok {
		set("substr", stringValue(""))
	}
	if _, ok := vars["msg"]; !ok {
		set("msg", stringValue(""))
	}
	return vars
}

func toValue(raw any) (value, bool) {
	switch t := raw.(type) {
	case bool:
		return boolValue(t), true
	case string:
		return stringValue(t), true
	case int:
		return numberValue(float64(t)), true
	case int64:
		return numberValue(float64(t)), true
	case uint64:
		return numberValue(float64(t)), true
	case float64:
		return numberValue(t), true
	case float32:
		return numberValue(float64(t)), true
	}
	return value{}, false
}

// ---- background loops ----

// runLiveness is the heartbeat evaluator: it turns "last success is older than
// the expectation" into exactly one gap + degraded state per episode, and
// recovery into one sensor_recovered (SPEC-03 §3.9).
func (s *Sensors) runLiveness(ctx context.Context) {
	defer s.wg.Done()
	tk := time.NewTicker(time.Second)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-tk.C:
			s.livenessCheck(ctx)
		}
	}
}

// livenessCheck is the heartbeat evaluator's body: "last success is older than
// the expectation" becomes exactly one gap + degraded per episode, and recovery
// becomes one sensor_recovered (SPEC-03 §3.9).
func (s *Sensors) livenessCheck(ctx context.Context) {
	now := s.now()
	for _, k := range types.SensorKinds {
		rt := s.rt[k]
		if rt == nil || !rt.enabled.Load() {
			continue
		}
		maxAge := staleAfter[k]
		ok := rt.lastOK.Load()
		if ok == 0 {
			continue // never succeeded yet: the boot probe decides
		}
		age := now.Sub(time.Unix(0, ok))
		if age > maxAge {
			cause := "heartbeat_stale"
			if k == types.SenJournald && s.jlState.childDead.Load() {
				cause = "child_died"
			}
			s.openGap(k, string(k), cause, fmt.Sprintf("last success %s ago, expectation %s", age.Round(time.Millisecond), maxAge))
			continue
		}
		if rt.degraded.Load() {
			s.closeGap(ctx, k, string(k), "heartbeat_stale")
			s.closeGap(ctx, k, string(k), "child_died")
		}
	}
	// A bound that silently drops state would be its own invisible failure: the
	// eviction counts are published on the sensors' health reason.
	if ev := s.stab.evictions() + s.cool.evictions(); ev > 0 {
		for _, k := range types.SensorKinds {
			rt := s.rt[k]
			if rt == nil || !rt.enabled.Load() {
				continue
			}
			if reason := *rt.reason.Load(); reason == "" {
				s.setReason(k, fmt.Sprintf("stabilization/cooldown bounds evicted %d entries (capacity %d/%d)",
					ev, defaultStabCapacity, defaultCooldownCapacity))
			}
		}
	}
}

// runCanary injects one canary through the real pipeline every 10m and asserts
// it lands (SPEC-03 §3.9): absence of evidence is never evidence of health.
func (s *Sensors) runCanary(ctx context.Context) {
	defer s.wg.Done()
	tk := time.NewTicker(canaryInterval)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-tk.C:
			cctx, cancel := context.WithTimeout(ctx, canaryLandDeadline)
			_, _ = s.Canary(cctx, "io")
			cancel()
		}
	}
}

// runReloadSweep is the 60s mtime backstop plus the debounced inotify path
// (SPEC-03 §3.7). SIGHUP retries immediately and unconditionally.
func (s *Sensors) runReloadSweep(ctx context.Context) {
	defer s.wg.Done()
	tk := time.NewTicker(s.cfg.rulesMTMSSweep)
	defer tk.Stop()
	last, _ := rulesMTimes(s.cfg.rulesDir)
	debounce := time.NewTimer(time.Hour)
	debounce.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-s.sighup:
			_ = s.Reload(ctx)
		case <-s.reloadDebounceCh():
			debounce.Reset(s.cfg.reloadDebounce)
		case <-debounce.C:
			if s.inotifyOff.Load() {
				continue // the trigger is disabled after 3 strikes
			}
			_ = s.Reload(ctx)
		case <-tk.C:
			cur, err := rulesMTimes(s.cfg.rulesDir)
			if err == nil && cur != last {
				last = cur
				_ = s.Reload(ctx)
			}
		}
	}
}

func (s *Sensors) reloadDebounceCh() <-chan struct{} { return s.reloadCh }

func (s *Sensors) installSignals() {
	s.sighup = make(chan os.Signal, 1)
	signal.Notify(s.sighup, syscall.SIGHUP)
}

// markReloadPending is called by the inotify collector on a rules.d change and
// feeds the debounce timer; the flag itself is what the loop reads.
func (s *Sensors) markReloadPending() {
	s.reloadPending.Store(true)
	select {
	case s.reloadCh <- struct{}{}:
	default:
	}
}

// NoteLedgerQueue lets the composition root feed the ledger's own writer queue
// depth in, so the §3.9 load-shed rule ("when the event pipeline is backed up
// (>75% queue depth)") reacts to the real pipeline rather than a proxy.
func (s *Sensors) NoteLedgerQueue(depth, cap int) {
	s.lastDepth.Store(int64(depth))
	if cap > 0 {
		s.depthCap.Store(int64(cap))
	}
}
