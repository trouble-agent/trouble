package research

// service.go — the research rung (SPEC-07 §2, §3, §4).
//
// One Service owns the whole rung: the driver, the class table, the corpus
// grep, the poll budgets, the failure fence, the daily budget and the cost
// accounting. It writes exactly two ledger kinds — `research` and `gap`
// (SPEC-INDEX §3.4) — and nothing else.
//
// Non-negotiable: research is best effort. The lab being unreachable, slow,
// rejecting, unavailable or silent never blocks the rung above it. Every
// failure path returns a ResearchOutcome the ladder can proceed past plus a
// ledger record; the five conditions of §3.6 additionally emit a GapRecord.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// Deps is the injected substrate (implemented by the daemon at the composition
// root). SPEC-07 §2.1 spells it as Append(Record) / Scrub(targets, bytes) /
// Clock() / Since(anchor); it is narrowed to the real subsystem signatures so
// the composition root passes *ledger.Ledger and *scrub.Engine directly
// (recorded deviation: SPEC-01 and SPEC-02 own those two signatures).
type Deps interface {
	Append(ctx context.Context, d types.RecordDraft) (types.Record, error)
	Scrub(ctx context.Context, target types.ScrubTarget, b []byte) (types.ScrubResult, error)
	Now() time.Time
	Monotonic() time.Duration
}

// Appender is SPEC-01's writer (*ledger.Ledger satisfies it).
type Appender interface {
	Append(ctx context.Context, d types.RecordDraft) (types.Record, error)
}

// Scrubber is SPEC-02's engine (*scrub.Engine satisfies it).
type Scrubber interface {
	ScrubBytes(ctx context.Context, target types.ScrubTarget, projectID string, b []byte) ([]byte, types.ScrubResult, error)
}

// Clock is the daemon clock: wall for stamps, monotonic for budgets.
type Clock interface {
	Now() time.Time
	Monotonic() time.Duration
}

// NewDeps binds the three collaborators into the Deps the Service consumes.
func NewDeps(app Appender, sc Scrubber, clk Clock) Deps {
	return hostDeps{app: app, sc: sc, clk: clk}
}

type hostDeps struct {
	app Appender
	sc  Scrubber
	clk Clock
}

func (h hostDeps) Append(ctx context.Context, d types.RecordDraft) (types.Record, error) {
	return h.app.Append(ctx, d)
}

func (h hostDeps) Scrub(ctx context.Context, target types.ScrubTarget, b []byte) (types.ScrubResult, error) {
	out, res, err := h.sc.ScrubBytes(ctx, target, "", b)
	if err != nil {
		return res, err
	}
	res.Value = out
	return res, nil
}

func (h hostDeps) Now() time.Time           { return h.clk.Now() }
func (h hostDeps) Monotonic() time.Duration { return h.clk.Monotonic() }

// config is the resolved [research] table (SPEC-07 §4.3). Every key is
// defaulted; no default carries a fleet value. HostID and DaemonVersion are
// added for the provenance the submit context must carry (§3.3); SPEC-12 owns
// their canonical source.
type config struct {
	Enabled                  bool
	Driver                   string
	LabURL                   string
	LabDataDir               string
	CorpusRoots              []string
	CorpusGlob               string
	CorpusMaxFiles           int
	CorpusMaxBytesPerFile    int64
	CorpusTimeout            time.Duration
	ConnectTimeout           time.Duration
	RequestTimeout           time.Duration
	SubmitTimeout            time.Duration
	PollInterval             time.Duration
	PollJitterPct            float64
	PollTimeout              time.Duration
	PollMaxRequests          int
	DuplicateRecheckInterval time.Duration
	QueueDepthSkip           int64
	MaxInflight              int
	CooldownFailures         int
	Cooldown                 time.Duration
	HealthProbeInterval      time.Duration
	CapabilityProbeInterval  time.Duration
	SubmitFuse400s           int
	BriefMaxBytes            int
	PromptBriefMaxBytes      int
	PromptMaxBytes           int
	RequestsPerDay           int64
	Cadence                  string
	CadenceLadder            []string
	FallbackSlug             string
	AllowUnknownClassSubmit  bool
	ApplyBriefAsPlay         bool
	Table                    string
	UnitKind                 map[string]string
	ContainerMarkers         []string
	AgentTokensSavedIn       int64
	AgentTokensSavedOut      int64
	WebhookURL               string
	WebhookPollURL           string
	HostID                   string
	DaemonVersion            string
}

// defaultConfig is SPEC-07 §4.3 verbatim (plus the two provenance keys).
func defaultConfig() config {
	return config{
		Enabled:                  true,
		Driver:                   types.DriverOffByOne,
		LabURL:                   "http://127.0.0.1:8766",
		LabDataDir:               "./data",
		CorpusRoots:              []string{"answers"},
		CorpusGlob:               "*.json",
		CorpusMaxFiles:           5000,
		CorpusMaxBytesPerFile:    1 << 20,
		CorpusTimeout:            3 * time.Second,
		ConnectTimeout:           2 * time.Second,
		RequestTimeout:           5 * time.Second,
		SubmitTimeout:            10 * time.Second,
		PollInterval:             15 * time.Second,
		PollJitterPct:            0.20,
		PollTimeout:              12 * time.Minute,
		PollMaxRequests:          48,
		DuplicateRecheckInterval: 3 * time.Minute,
		QueueDepthSkip:           25,
		MaxInflight:              4,
		CooldownFailures:         3,
		Cooldown:                 5 * time.Minute,
		HealthProbeInterval:      60 * time.Second,
		CapabilityProbeInterval:  time.Hour,
		SubmitFuse400s:           3,
		BriefMaxBytes:            65536,
		PromptBriefMaxBytes:      16384,
		PromptMaxBytes:           32768,
		RequestsPerDay:           200,
		Cadence:                  "end-of-day",
		CadenceLadder:            []string{"end-of-day", "post-debug", "pre-phase"},
		FallbackSlug:             "unknown",
		ContainerMarkers:         []string{"docker", "podman", "containerd"},
		AgentTokensSavedIn:       25000,
		AgentTokensSavedOut:      8000,
	}
}

// capability is the startup capability probe's verdict (§2.4).
type capability string

const (
	capUnknown      capability = ""
	capFull         capability = "full"
	capDiscoverOnly capability = "discover_only"
)

// rung is one rung run. It is registered before any network call, so a degraded
// or skipped run still has a join key (SPEC-07 §3.9).
type rung struct {
	resID string
	idem  string
	inc   types.Incident
	sig   types.Sig
	slug  types.ClassSlug
	desc  string
	extra map[string]any // caller context (env, lang, version, stack, …)
	brief map[string]any // the accepted brief, when the rung returned one

	mu         sync.Mutex
	out        types.ResearchOutcome
	cost       researchCost
	redactions int
	code       types.ErrorCode
	status     int
	msg        string
	polls      int
	unknown    int
	queueDepth int64
	corpusPath string
	corpusErrs int
	corpusTO   bool
	poll       *pollState
	cancel     context.CancelFunc
	done       chan struct{}
	fin        bool
}

// Service is the research rung.
type Service struct {
	cfg    config
	deps   Deps
	driver types.ResearchDriver
	table  *table
	corpus corpus

	mu        sync.Mutex
	outcomes  map[string][]types.ResearchOutcome // sig → newest first
	byID      map[string]*rung                   // res_ id → rung
	inflight  map[string]*rung                   // idem_key → in-flight rung
	submitted map[string]string                  // idem_key → submission_id
	cadence   map[string]string                  // class → accepted cadence
	day       string
	submits   int64
	cooldown  time.Time
	fails     int
	decoder   int
	fence     bool
	killed    bool
	health    types.LabHealth
	healthTS  time.Time
	capState  capability
	cost      researchCost
	closed    bool
	closeCh   chan struct{}
	wg        sync.WaitGroup

	appendErrs    int
	lastAppendErr string
}

// New resolves and validates the config and returns a Service. It never dials
// the lab: the capability and health probes run in Start (SPEC-07 §2.1).
func New(cfg map[string]any, deps Deps) (*Service, error) {
	c, err := resolveConfig(cfg)
	if err != nil {
		return nil, err
	}
	t, err := LoadTable(c.Table)
	if err != nil {
		return nil, err
	}
	t.SetFallbackSlug(c.FallbackSlug)
	s := &Service{
		cfg: c, deps: deps, table: t,
		outcomes: map[string][]types.ResearchOutcome{}, byID: map[string]*rung{},
		inflight: map[string]*rung{}, submitted: map[string]string{},
		cadence: map[string]string{}, closeCh: make(chan struct{}),
	}
	s.corpus = newFSCorpus(c, deps.Now, func(anchor time.Time) time.Duration { return deps.Now().Sub(anchor) })
	switch c.Driver {
	case types.DriverOffByOne:
		s.driver = newOffByOne(c)
	case types.DriverNone:
		s.driver = driverNone{}
	case types.DriverWebhook:
		if c.WebhookURL == "" {
			return nil, fmt.Errorf("research: driver webhook requires research.webhook_url")
		}
		s.driver = newWebhook(c)
	default:
		return nil, fmt.Errorf("research: unknown driver %q", c.Driver)
	}
	return s, nil
}

// Driver exposes the wired driver (wiring/tests).
func (s *Service) Driver() types.ResearchDriver { return s.driver }

// Start runs the boot probes (SPEC-07 §2.4). §2.1 pins New to "never dials", so
// the probe needs a home and this is it.
func (s *Service) Start(ctx context.Context) error {
	if s.cfg.Driver == types.DriverNone {
		s.setCapability(capDiscoverOnly)
		return nil
	}
	s.probe(ctx)
	return nil
}

// probe runs the capability + health probe and records the verdict.
func (s *Service) probe(ctx context.Context) {
	if p, ok := s.driver.(interface {
		OpenAPIPaths(context.Context) ([]string, error)
	}); ok {
		paths, err := p.OpenAPIPaths(ctx)
		s.charge(nil, "probe", 1024, 128)
		if err != nil {
			s.noteFailure()
			s.setCapability(capUnknown)
		} else {
			need := []string{"/api/v1/problems/discover", "/api/v1/problems/submit", "/api/v1/queue/{submission_id}"}
			full := true
			for _, n := range need {
				if !containsString(paths, n) {
					full = false
					break
				}
			}
			if full {
				s.setCapability(capFull)
			} else {
				// A detected capability, not a failure: the rung runs
				// discover-only and never submits (§2.4).
				s.setCapability(capDiscoverOnly)
			}
		}
	}
	h, err := s.driver.Health(ctx)
	s.charge(nil, "probe", 1024, 128)
	if err != nil {
		s.noteFailure()
		return
	}
	stats, serr := s.driver.Stats(ctx)
	s.mu.Lock()
	if serr == nil {
		s.health = stats
	}
	s.health.Status = h.Status
	s.health.Uptime = h.Uptime
	s.healthTS = s.deps.Now()
	s.fails = 0
	s.mu.Unlock()
}

// setCapability records the probe verdict.
func (s *Service) setCapability(c capability) {
	s.mu.Lock()
	s.capState = c
	s.mu.Unlock()
}

// Capability reports the last probe verdict.
func (s *Service) Capability() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.capState)
}

// SetKillSwitch arms or clears the kill-switch. SPEC-05 owns the checkpoint
// semantics; research reads the flag so no new request starts after it is set.
func (s *Service) SetKillSwitch(on bool) {
	s.mu.Lock()
	s.killed = on
	s.mu.Unlock()
}

// Close cancels every poller and waits ≤2s. Nothing is written after Close.
func (s *Service) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.closeCh)
	for _, r := range s.byID {
		if r.cancel != nil {
			r.cancel()
		}
	}
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	return nil
}

// Snapshot is the dashboard's view: {driver, lab_health, cost, inflight,
// cooldown_until, fence}.
func (s *Service) Snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	fence := ""
	if s.fence {
		fence = "submit_disabled"
	}
	cooldown := ""
	if !s.cooldown.IsZero() && s.deps.Now().Before(s.cooldown) {
		cooldown = types.FormatUTC(s.cooldown)
	}
	return map[string]any{
		"driver":         s.cfg.Driver,
		"lab_health":     s.health,
		"cost":           s.cost.asMap(),
		"inflight":       len(s.inflight),
		"cooldown_until": cooldown,
		"fence":          fence,
		"capability":     string(s.capState),
		"submits_today":  s.submits,
		"health_ts":      s.healthTS.Format(types.TsLayout),
	}
}

// Outcomes returns the rung outcomes for a sig, newest first (SPEC-07 §2.1).
func (s *Service) Outcomes(sig string) []types.ResearchOutcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.outcomes[sig]
	out := make([]types.ResearchOutcome, len(src))
	copy(out, src)
	return out
}

// OutcomeByID returns a rung outcome by its res_ id.
func (s *Service) OutcomeByID(resID string) (types.ResearchOutcome, bool) {
	s.mu.Lock()
	r, ok := s.byID[resID]
	s.mu.Unlock()
	if !ok {
		return types.ResearchOutcome{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.out, true
}

// Run is the rung's entry point. It performs the synchronous part (derive →
// D1 → D2 → corpus → submit) and returns an outcome whose State is requested |
// returned | degraded | skipped. An accepted submission does not hold this call:
// the poller is a goroutine and Poll reads its result, which is what keeps
// research off the ladder's critical path (§4.2). bundle["await"]=true waits for
// the terminal state instead (the one-shot CLI path).
func (s *Service) Run(ctx context.Context, inc types.Incident, sig types.Sig, bundle map[string]any) (types.ResearchOutcome, error) {
	if bundle == nil {
		bundle = map[string]any{}
	}
	r := s.newRung(inc, sig, bundle)
	await := asBool(bundle["await"], false)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return types.ResearchOutcome{}, fmt.Errorf("research: service is closed")
	}
	if len(s.inflight) >= s.cfg.MaxInflight {
		// Over the concurrency cap: the rung degrades immediately with the lab's
		// own unreachable code — the ladder never waits on research.
		s.mu.Unlock()
		out := r.finish(types.ResDegraded, types.ResReasonLabUnreachable, types.CodeResearch001, 0, "max_inflight reached")
		s.record(r)
		return out, nil
	}
	now := s.deps.Now()
	if !s.cfg.Enabled || s.killed {
		out := r.finish(types.ResSkipped, types.ResSkipKillSwitch, "", 0, "")
		s.registerLocked(r)
		s.mu.Unlock()
		s.record(r)
		return out, nil
	}
	if s.cooldown.After(now) {
		out := r.finish(types.ResDegraded, types.ResReasonLabUnreachable, types.CodeResearch001, 0, "cooldown active")
		s.registerLocked(r)
		s.mu.Unlock()
		s.record(r)
		return out, nil
	}
	if s.rollDayLocked(now) {
		// A new UTC day starts the request budget over; the counter is also
		// rebuilt from the ledger at boot (Replay), so a restart cannot reset it.
	}
	if b := budgetOf(s.cfg); b > 0 && s.submits >= b {
		out := r.finish(types.ResSkipped, types.ResSkipBudgetExhausted, "", 0, "")
		s.registerLocked(r)
		s.mu.Unlock()
		s.record(r)
		return out, nil
	}
	s.registerLocked(r)
	capState := s.capState
	s.mu.Unlock()

	if s.cfg.Driver == types.DriverNone {
		out := r.finish(types.ResSkipped, types.ResSkipDriverNone, types.CodeResearch008, 0, "")
		s.record(r)
		return out, nil
	}

	// A fallback slug means the derivation missed. The corpus grep still runs;
	// submit does not, unless the operator allows it (§5 / TROUBLE-RESEARCH-005).
	if r.slug.Fallback && !s.cfg.AllowUnknownClassSubmit {
		if cr, ok := s.corpusStage(ctx, r); ok {
			s.record(r)
			return s.finishReturned(r, cr), nil
		}
		out := r.finish(types.ResSkipped, types.ResSkipClassUnknown, types.CodeResearch005, 0, "")
		s.record(r)
		return out, nil
	}

	// D1 — the broadest key. Sensor-sourced sigs never narrow (§3.2).
	d1, err := s.driver.Discover(ctx, types.DiscoverRequest{ProblemClass: r.slug.Slug})
	s.charge(r, "discover", 4096, 200)
	if err == nil && d1.Found {
		if brief, ok := s.briefFromAnswer(d1.Solution, false); ok {
			return s.returnBrief(r, brief, corpusResult{}), nil
		}
	}
	if err != nil && !isNotFound(err) {
		out, derr := s.failRun(r, err)
		return out, derr
	}

	// D2 — the narrowing probe, at most once, and only when the incident really
	// carries the facts (§3.2).
	env, lang, version := asString(r.extra["env"], ""), asString(r.extra["lang"], ""), asString(r.extra["version"], "")
	if env != "" || lang != "" || version != "" {
		d2, e2 := s.driver.Discover(ctx, types.DiscoverRequest{ProblemClass: r.slug.Slug, Env: env, Lang: lang, Version: version})
		s.charge(r, "discover", 8192, 400)
		if e2 == nil && d2.Found {
			if brief, ok := s.briefFromAnswer(d2.Solution, false); ok {
				return s.returnBrief(r, brief, corpusResult{}), nil
			}
		}
		if e2 != nil && !isNotFound(e2) {
			out, derr := s.failRun(r, e2)
			return out, derr
		}
	}

	// D3 — the mandatory corpus grep.
	if cr, ok := s.corpusStage(ctx, r); ok {
		return s.finishReturned(r, cr), nil
	}

	// Nothing cached: queue the class.
	if capState == capDiscoverOnly {
		out := r.finish(types.ResSkipped, types.ResSkipCapabilityMissing, "", 0, "")
		s.record(r)
		return out, nil
	}
	if s.fuseOpen() {
		out := r.finish(types.ResDegraded, types.ResReasonStrictDecoder, types.CodeResearch002, 0, "submit fuse open")
		s.record(r)
		return out, nil
	}
	out, err := s.submitStage(ctx, r)
	if err != nil {
		out, derr := s.failRun(r, err)
		return out, derr
	}
	if await && out.State == types.ResRequested {
		return s.Poll(ctx, r.resID)
	}
	return out, nil
}

// Request is the SPEC-TYPES ResearchPort implementation the ladder consumes
// (SPEC-07 §2.1 adapter surface). A caller-supplied slug is used verbatim and
// derivation is skipped; an absent or empty slug derives.
func (s *Service) Request(ctx context.Context, inc types.Incident, sub types.Subject) (types.ResearchOutcome, error) {
	bundle := map[string]any{}
	for k, v := range sub.Context {
		if k == "sig" {
			continue
		}
		bundle[k] = v
	}
	if sub.Slug != "" {
		bundle["slug"] = sub.Slug
	}
	if sub.Description != "" {
		bundle["description"] = sub.Description
	}
	if sub.Cadence != "" {
		bundle["cadence"] = sub.Cadence
	}
	sig := sigFromIncident(inc, sub)
	return s.Run(ctx, inc, sig, bundle)
}

// Poll waits for a rung's terminal state and returns it. The wait is bounded by
// the rung's poll budget; a budget that runs out while the submission is still
// queued ends as degraded / poll_timeout (TROUBLE-RESEARCH-006) with a gap —
// the submission itself is never cancelled (no cancel endpoint is measured).
func (s *Service) Poll(ctx context.Context, resID string) (types.ResearchOutcome, error) {
	s.mu.Lock()
	r, ok := s.byID[resID]
	s.mu.Unlock()
	if !ok {
		return types.ResearchOutcome{}, fmt.Errorf("research: unknown res id %q", resID)
	}
	r.mu.Lock()
	done := r.done
	fin := r.fin
	r.mu.Unlock()
	if fin {
		return s.currentOutcome(r), nil
	}
	select {
	case <-done:
	case <-ctx.Done():
		return s.currentOutcome(r), nil
	case <-s.closeCh:
		return s.currentOutcome(r), nil
	}
	return s.currentOutcome(r), nil
}

// currentOutcome reads the rung's current outcome.
func (s *Service) currentOutcome(r *rung) types.ResearchOutcome {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.out
}

// Resume re-adopts a parked or mid-flight rung after a restart (§4.2). A record
// with state `requested` and a submission id resumes polling with a fresh
// budget; one without a submission id is re-submitted under §3.10.2.
func (s *Service) Resume(ctx context.Context, out types.ResearchOutcome) (types.ResearchOutcome, error) {
	if out.State != types.ResRequested {
		return out, nil
	}
	s.mu.Lock()
	r, ok := s.byID[out.ID]
	s.mu.Unlock()
	if !ok {
		s.mu.Lock()
		r = &rung{
			resID: out.ID, idem: out.SubmissionID, inc: types.Incident{ID: out.Inc},
			slug: out.Slug, extra: map[string]any{}, done: make(chan struct{}), out: out,
		}
		s.byID[out.ID] = r
		s.mu.Unlock()
	}
	if out.SubmissionID != "" {
		s.startPoller(r)
		return out, nil
	}
	// No submission id: the answer may be arriving in the cache, so the class is
	// re-probed at the duplicate recheck interval inside the same budget.
	s.startDuplicateRecheck(r)
	return out, nil
}

// Park stops a rung at a checkpoint (SIGTERM / kill-switch): the current state
// is written, the poller is cancelled and nothing further is written. At the
// kill-switch the state is written as skipped / kill_switch so the res_ link
// chain stays unbroken (§4.2).
func (s *Service) Park(ctx context.Context, out types.ResearchOutcome) (types.ResearchOutcome, error) {
	s.mu.Lock()
	r, ok := s.byID[out.ID]
	s.mu.Unlock()
	if !ok {
		return out, nil
	}
	r.mu.Lock()
	cancel := r.cancel
	fin := r.fin
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if fin {
		return s.currentOutcome(r), nil
	}
	final := r.finish(types.ResSkipped, types.ResSkipKillSwitch, "", 0, "")
	s.record(r)
	return final, nil
}

// Replay rebuilds the in-memory rung registry from the ledger: today's
// `research` records restore the outcomes, the in-flight submissions, the daily
// submit counter and the learned cadence per class (§4.2). It is how a restart
// cannot reset the budget.
func (s *Service) Replay(records []types.Record) {
	day := s.deps.Now().UTC().Format("2006-01-02")
	s.mu.Lock()
	s.day = day
	s.mu.Unlock()
	for _, rec := range records {
		if rec.Kind != types.KResearch {
			continue
		}
		p := rec.Payload
		ts, _ := types.ParseUTC(rec.TS)
		if ts.UTC().Format("2006-01-02") != day {
			continue
		}
		rg := &rung{
			resID: asString(p["res_id"], ""), idem: asString(p["idem_key"], ""),
			sig: sigFromString(rec.Sig), inc: types.Incident{ID: rec.Inc},
			slug: types.ClassSlug{
				Slug: asString(p["slug"], ""), Source: asString(p["source"], ""),
				AppKind: asString(p["app_kind"], ""), Taxonomy: asString(p["taxonomy"], ""),
				Fallback: asBool(p["slug_fallback"], false),
			},
			extra: map[string]any{}, done: make(chan struct{}), fin: true,
		}
		rg.out = types.ResearchOutcome{
			ID: rg.resID, Inc: rec.Inc, Slug: rg.slug,
			State: asString(p["state"], ""), Driver: asString(p["driver"], ""),
			SubmissionID:   asString(p["submission_id"], ""),
			DegradedReason: asString(p["degraded_reason"], ""),
			CorpusGrepHit:  asBool(p["corpus_grep_hit"], false),
			RequestedTS:    rec.TS, ReturnedTS: asString(p["returned_ts"], ""),
		}
		if b, ok := p["brief"].(map[string]any); ok {
			rg.out.Brief = b
		}
		if submits, ok := p["cost"].(map[string]any); ok {
			rg.cost.Submits = int(asInt64(submits["submits"], 0))
		}
		s.mu.Lock()
		if rg.resID != "" {
			s.byID[rg.resID] = rg
		}
		if rec.Sig != "" {
			s.outcomes[rec.Sig] = append([]types.ResearchOutcome{rg.out}, s.outcomes[rec.Sig]...)
		}
		if sid := rg.out.SubmissionID; sid != "" && rg.idem != "" {
			s.submitted[rg.idem] = sid
		}
		s.submits += int64(rg.cost.Submits)
		if cad := asString(p["cadence_accepted"], ""); cad != "" {
			s.cadence[rg.slug.Slug] = cad
		}
		s.mu.Unlock()
	}
}

// newRung builds the rung and derives its class slug before any I/O.
func (s *Service) newRung(inc types.Incident, sig types.Sig, bundle map[string]any) *rung {
	facts := s.factsOf(sig, bundle)
	slug := DeriveClassSlug(sig, facts, s.table)
	if forced := strings.TrimSpace(asString(bundle["slug"], "")); forced != "" {
		slug.Slug = kebab(forced)
		slug.Fallback = false
	}
	r := &rung{
		resID: types.NewID(types.PRes), inc: inc, sig: sig, slug: slug,
		desc: asString(bundle["description"], ""), extra: s.contextOf(bundle),
		done: make(chan struct{}),
	}
	version := asString(bundle["version"], "")
	r.idem = Digest([]byte(sig.String() + "|" + slug.Slug + "|" + version))
	r.out = types.ResearchOutcome{
		ID: r.resID, Inc: inc.ID, Slug: slug, Driver: s.cfg.Driver,
		State: types.ResRequested, RequestedTS: types.FormatUTC(s.deps.Now()),
	}
	return r
}

// factsOf assembles the derivation input from the bundle.
func (s *Service) factsOf(sig types.Sig, bundle map[string]any) subjectFacts {
	f := subjectFacts{
		Sig: sig.String(), Source: sig.Source,
		Subject:    asString(bundle["subject"], ""),
		Message:    asString(bundle["message"], ""),
		Stack:      asString(bundle["stack"], ""),
		Unit:       asString(bundle["unit"], ""),
		ObjectPath: asString(bundle["object_path"], ""),
		Project:    asString(bundle["project"], ""),
		App:        asString(bundle["app"], ""),
		Mount:      asString(bundle["mount"], ""),
		Path:       asString(bundle["path"], ""),
		Scope:      asString(bundle["scope"], ""),
		Origin:     asString(bundle["source"], ""),
		AppKind:    asString(bundle["app_kind"], ""),
		UnitKind:   s.cfg.UnitKind,
		Markers:    s.cfg.ContainerMarkers,
	}
	if f.Origin == "" {
		f.Origin = string(sig.Source)
		if f.Unit != "" {
			f.Origin = string(sig.Source) + ":" + f.Unit
		}
	}
	return f
}

// contextOf normalizes the caller's context map (the slug and the await flag are
// control keys, never evidence).
func (s *Service) contextOf(bundle map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range bundle {
		switch k {
		case "slug", "await", "description", "subject", "app_kind":
			continue
		}
		out[k] = v
	}
	return out
}

// submitStage queues the class and starts the poller or the duplicate recheck.
func (s *Service) submitStage(ctx context.Context, r *rung) (types.ResearchOutcome, error) {
	// One in-flight submission per idem_key: a follower attaches to the existing
	// submission instead of queueing a second one (§3.10.1).
	s.mu.Lock()
	prev, hasPrev := s.submitted[r.idem]
	s.mu.Unlock()
	if hasPrev && prev != "" {
		r.mu.Lock()
		r.out.SubmissionID = prev
		r.mu.Unlock()
		out := r.finish(types.ResRequested, "", "", 0, "")
		out.SubmissionID = prev
		s.record(r)
		s.startPoller(r)
		return out, nil
	}

	req := types.SubmitRequest{
		ProblemClass: r.slug.Slug,
		Description:  s.description(r),
		Cadence:      s.cadenceFor(r.slug.Slug),
		Context:      s.submitContext(r),
	}
	if c := asString(r.extra["cadence"], ""); c != "" {
		req.Cadence = c
	}
	attempted := []string{req.Cadence}
	var last error
	for try := 0; try <= 2; try++ {
		resp, err := s.driver.Submit(ctx, req)
		s.charge(r, "submit", 1024, 600)
		if err == nil {
			if resp.SubmissionID != "" {
				s.mu.Lock()
				s.submitted[r.idem] = resp.SubmissionID
				s.cadence[r.slug.Slug] = req.Cadence
				s.submits++
				s.mu.Unlock()
				r.mu.Lock()
				r.out.SubmissionID = resp.SubmissionID
				r.cost.Submits++
				r.mu.Unlock()
				s.mu.Lock()
				s.cost.Submits++
				s.mu.Unlock()
				out := r.finish(types.ResRequested, "", "", 201, "")
				out.SubmissionID = resp.SubmissionID
				s.mu.Lock()
				s.inflight[r.idem] = r
				s.mu.Unlock()
				s.record(r)
				s.startPoller(r)
				return out, nil
			}
			if resp.Duplicate {
				// A 409 without an id: the answer is arriving in the cache, so
				// the rung re-probes the class at the duplicate recheck interval
				// inside the same poll budget (§3.10.3). It is not a failure.
				s.mu.Lock()
				// A 409 is a submit request that reached the lab: it debits the
				// daily budget exactly like an accepted one (§3.8).
				s.cadence[r.slug.Slug] = req.Cadence
				s.submits++
				s.mu.Unlock()
				r.mu.Lock()
				r.cost.Submits++
				r.mu.Unlock()
				s.mu.Lock()
				s.cost.Submits++
				s.mu.Unlock()
				out := r.finish(types.ResRequested, "", types.CodeResearch003, 409, "")
				s.record(r)
				s.startDuplicateRecheck(r)
				return out, nil
			}
			// A 2xx with no submission id: nothing to poll (010).
			last = &driverError{
				Code: types.CodeResearch010, Reason: types.ResReasonSolverUnavailable,
				Status: 201, Class: types.ErrClassPermanent, Msg: "submit carried no submission_id",
			}
			break
		}
		last = err
		var de *driverError
		if !errors.As(err, &de) || de.Status != 400 || !strings.Contains(strings.ToLower(de.Msg), "cadence") {
			break
		}
		next, ok := nextCadence(s.cfg.CadenceLadder, attempted)
		if !ok {
			break
		}
		attempted = append(attempted, next)
		req.Cadence = next
	}
	if last == nil {
		last = &driverError{Code: types.CodeResearch001, Reason: types.ResReasonLabUnreachable, Class: types.ErrClassTransient}
	}
	return types.ResearchOutcome{}, last
}

// corpusStage runs D3. It returns (result, true) when an accepted answer was
// found — the caller then finishes the rung as returned.
func (s *Service) corpusStage(ctx context.Context, r *rung) (corpusResult, bool) {
	terms := corpusTerms(r.slug.Slug, r.slug.Slug, r.slug.Taxonomy, shortOf(r.sig),
		asString(r.extra["message"], ""), asString(r.extra["stack"], ""))
	res, err := s.corpus.Grep(ctx, r.slug.Slug, terms)
	s.charge(r, "corpus", 0, 0)
	r.mu.Lock()
	r.cost.CorpusGreps++
	r.corpusErrs, r.corpusTO = res.ParseErrors, res.Timeout
	r.mu.Unlock()
	if err != nil {
		res.ParseErrors++
	}
	for _, root := range res.Unreadable {
		s.emitGap(ctx, "research_corpus_unreadable", r, map[string]any{"root": root})
	}
	for _, h := range res.Hits {
		if brief, ok := s.briefFromAnswer(h.Answer, true); ok {
			brief["corpus_path"] = h.Path
			r.mu.Lock()
			r.out.CorpusGrepHit = true
			r.corpusPath = h.Path
			r.mu.Unlock()
			r.brief = brief
			return res, true
		}
	}
	return res, false
}

// finishReturned completes a rung with a brief and writes its records.
func (s *Service) finishReturned(r *rung, cr corpusResult) types.ResearchOutcome {
	r.mu.Lock()
	brief := r.brief
	r.mu.Unlock()
	out := r.finish(types.ResReturned, "", "", 200, "")
	out.Brief = brief
	out.CorpusGrepHit = cr.Hits != nil && len(cr.Hits) > 0
	// A brief that resolves the incident without an agent run records the agent
	// budget that went unspent (§3.8) — explicitly an estimate.
	if boolField(brief, "resolve_outright") {
		r.mu.Lock()
		r.cost.savedByBrief(s.cfg.AgentTokensSavedIn, s.cfg.AgentTokensSavedOut)
		r.mu.Unlock()
	}
	s.record(r)
	return out
}

// returnBrief completes a rung whose answer came from discover.
func (s *Service) returnBrief(r *rung, brief map[string]any, cr corpusResult) types.ResearchOutcome {
	r.mu.Lock()
	r.brief = brief
	r.mu.Unlock()
	return s.finishReturned(r, cr)
}

// failRun maps a driver failure onto the §5 table, records it and emits the gap.
func (s *Service) failRun(r *rung, err error) (types.ResearchOutcome, error) {
	var de *driverError
	if !errors.As(err, &de) {
		de = &driverError{
			Code: types.CodeResearch001, Reason: types.ResReasonLabUnreachable,
			Class: types.ErrClassTransient, Msg: err.Error(),
		}
	}
	if de.Code == types.CodeResearch001 || de.Code == types.CodeResearch004 || de.Code == types.CodeResearch010 {
		s.noteFailure()
	}
	if de.Code == types.CodeResearch002 {
		s.noteDecoderReject()
	}
	out := r.finish(types.ResDegraded, de.Reason, de.Code, de.Status, de.Msg)
	s.record(r)
	switch de.Code {
	case types.CodeResearch001:
		s.emitGap(context.Background(), "research_lab_unreachable", r, map[string]any{"note": de.Note})
	case types.CodeResearch002:
		s.emitGap(context.Background(), "research_strict_decoder_reject", r, map[string]any{"message": de.Msg})
	case types.CodeResearch004, types.CodeResearch010:
		s.emitGap(context.Background(), "research_solver_unavailable", r, map[string]any{"note": de.Note})
	}
	return out, nil
}

// startPoller launches the poll loop for an accepted submission.
func (s *Service) startPoller(r *rung) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.mu.Lock()
	r.cancel = cancel
	r.poll = newPollState(r.out.SubmissionID, pollParams{
		Interval: s.cfg.PollInterval, JitterPct: s.cfg.PollJitterPct,
		Timeout: s.cfg.PollTimeout, MaxRequests: s.cfg.PollMaxRequests,
	}, s.deps.Now())
	r.mu.Unlock()
	s.wg.Add(1)
	s.mu.Unlock()
	go s.pollLoop(ctx, r)
}

// pollLoop is the only result channel (§3.5): it polls until solved, failed or
// out of budget, then writes the terminal record.
func (s *Service) pollLoop(ctx context.Context, r *rung) {
	defer func() {
		s.wg.Done()
		// The rung must never be left open: an open rung is a caller blocked in
		// Poll, which is exactly the "research holds a transition" failure the
		// spec forbids.
		if !r.isDone() {
			s.settle(r, types.ResDegraded, types.ResReasonLabUnreachable, types.CodeResearch001, 0, "poll loop ended without a terminal state")
		}
	}()
	for {
		r.mu.Lock()
		ps := r.poll
		sid := r.out.SubmissionID
		r.mu.Unlock()
		if ps == nil || sid == "" {
			return
		}
		now := s.deps.Now()
		if ps.exhausted(now) {
			s.settle(r, types.ResDegraded, types.ResReasonPollTimeout, types.CodeResearch006, 0, "")
			s.emitGap(ctx, "research_poll_timeout", r, map[string]any{"poll_count": ps.Requests})
			s.release(r)
			return
		}
		wait := ps.nextInterval(now)
		select {
		case <-ctx.Done():
			return
		case <-s.closeCh:
			return
		case <-time.After(wait):
		}
		st, err := s.driver.Poll(ctx, sid)
		ps.Requests++
		r.mu.Lock()
		r.polls++
		if st.State == "" {
			r.unknown++
			ps.UnknownStat++
		}
		r.mu.Unlock()
		s.charge(r, "poll", 1024, 20)
		if err != nil {
			var de *driverError
			if errors.As(err, &de) && de.Code == types.CodeResearch004 {
				// 404 or a failed queue state: code 004, then one D1 re-probe.
				if de.Note == "queue_not_found" {
					s.recordDriverFailure(r, de)
					s.reprobe(r) // one D1: it may resolve the rung outright
					if !r.isDone() {
						s.settle(r, types.ResDegraded, types.ResReasonSolverUnavailable, types.CodeResearch004, de.Status, de.Msg)
					}
				} else {
					s.settle(r, types.ResDegraded, types.ResReasonSolverUnavailable, types.CodeResearch004, de.Status, de.Msg)
					s.emitGap(ctx, "research_solver_unavailable", r, nil)
				}
				s.release(r)
				return
			}
			s.settle(r, types.ResDegraded, reasonOf(de, types.ResReasonLabUnreachable), codeOf(de, types.CodeResearch001), 0, "")
			s.emitGap(ctx, "research_lab_unreachable", r, nil)
			s.release(r)
			return
		}
		switch st.State {
		case types.QueueSolved:
			if brief, ok := s.briefFromAnswer(st.Solution, false); ok {
				r.mu.Lock()
				r.brief = brief
				r.mu.Unlock()
				if boolField(brief, "resolve_outright") {
					r.mu.Lock()
					r.cost.savedByBrief(s.cfg.AgentTokensSavedIn, s.cfg.AgentTokensSavedOut)
					r.mu.Unlock()
				}
				out := s.settle(r, types.ResReturned, "", "", 200, "")
				_ = out
				s.release(r)
				return
			}
			// A solved-but-unacceptable answer is not a brief (§6.8): the rung
			// skips with brief_invalid (TROUBLE-RESEARCH-009) and the ladder
			// proceeds — the answer arrived, it just is not usable.
			s.settle(r, types.ResSkipped, types.ResSkipBriefInvalid, types.CodeResearch009, 200, "answer not verified")
			s.release(r)
			return
		case types.QueueFailed:
			s.settle(r, types.ResDegraded, types.ResReasonSolverUnavailable, types.CodeResearch004, 200, "queue reported failed")
			s.emitGap(ctx, "research_solver_unavailable", r, nil)
			s.release(r)
			return
		}
	}
}

// startDuplicateRecheck re-probes the class when a 409 carried no submission id.
func (s *Service) startDuplicateRecheck(r *rung) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.mu.Lock()
	r.cancel = cancel
	r.poll = newPollState("dup:"+r.idem, pollParams{
		Interval: s.cfg.DuplicateRecheckInterval, JitterPct: 0,
		Timeout: s.cfg.PollTimeout, MaxRequests: s.cfg.PollMaxRequests,
	}, s.deps.Now())
	r.mu.Unlock()
	s.wg.Add(1)
	s.mu.Unlock()
	go s.duplicateLoop(ctx, r)
}

// duplicateLoop is the duplicate-recheck variant of the poll loop.
func (s *Service) duplicateLoop(ctx context.Context, r *rung) {
	defer func() {
		s.wg.Done()
		if !r.isDone() {
			s.settle(r, types.ResDegraded, types.ResReasonPollTimeout, types.CodeResearch006, 0, "duplicate recheck ended without a terminal state")
		}
	}()
	for {
		r.mu.Lock()
		ps := r.poll
		r.mu.Unlock()
		now := s.deps.Now()
		if ps == nil || ps.exhausted(now) {
			s.settle(r, types.ResDegraded, types.ResReasonPollTimeout, types.CodeResearch006, 0, "")
			s.emitGap(ctx, "research_poll_timeout", r, nil)
			s.release(r)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-s.closeCh:
			return
		case <-time.After(s.cfg.DuplicateRecheckInterval):
		}
		ps.Requests++
		d, err := s.driver.Discover(ctx, types.DiscoverRequest{ProblemClass: r.slug.Slug})
		s.charge(r, "discover", 4096, 200)
		r.mu.Lock()
		r.polls++
		r.mu.Unlock()
		if err == nil && d.Found {
			if brief, ok := s.briefFromAnswer(d.Solution, false); ok {
				r.mu.Lock()
				r.brief = brief
				r.mu.Unlock()
				s.settle(r, types.ResReturned, "", "", 200, "")
				s.release(r)
				return
			}
		}
	}
}

// reprobe runs one D1 after a lost queue id (§6.6).
func (s *Service) reprobe(r *rung) {
	d, err := s.driver.Discover(context.Background(), types.DiscoverRequest{ProblemClass: r.slug.Slug})
	s.charge(r, "discover", 4096, 200)
	if err == nil && d.Found {
		if brief, ok := s.briefFromAnswer(d.Solution, false); ok {
			r.mu.Lock()
			r.brief = brief
			r.mu.Unlock()
			s.settle(r, types.ResReturned, "", "", 200, "")
			return
		}
	}
	if err != nil {
		s.settle(r, types.ResDegraded, reasonOfErr(err, types.ResReasonLabUnreachable), codeOfErr(err, types.CodeResearch001), 0, "")
	}
}

// release clears the in-flight registration.
func (s *Service) release(r *rung) {
	s.mu.Lock()
	if s.inflight[r.idem] == r {
		delete(s.inflight, r.idem)
	}
	s.mu.Unlock()
}

// registerLocked registers the rung in the maps (caller holds s.mu).
func (s *Service) registerLocked(r *rung) {
	s.byID[r.resID] = r
	if r.out.State == types.ResRequested {
		s.inflight[r.idem] = r
	}
}

// rollDayLocked resets the daily request counter on a UTC day boundary, and
// returns true when it rolled (caller holds s.mu).
func (s *Service) rollDayLocked(now time.Time) bool {
	day := now.UTC().Format("2006-01-02")
	if s.day == day {
		return false
	}
	prev := s.day
	s.day = day
	if prev != "" {
		// Only a real day change resets the counter. The first Run after boot
		// must keep whatever Replay restored, or a restart would reset the
		// budget — the exact hole §3.8 forbids.
		s.submits = 0
	}
	return true
}

// fuseOpen reports whether the submit fuse is open.
func (s *Service) fuseOpen() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fence
}

// noteFailure advances the cooldown counter: N consecutive transport failures
// close the lab conversation for `cooldown` and every Run degrades immediately
// (§5.2).
func (s *Service) noteFailure() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fails++
	if s.cfg.CooldownFailures > 0 && s.fails >= s.cfg.CooldownFailures {
		s.cooldown = s.deps.Now().Add(s.cfg.Cooldown)
		s.fails = 0
	}
}

// noteDecoderReject opens the submit fuse after N consecutive strict-decoder
// rejects: discover and corpus stay usable, submit stops (§5.2).
func (s *Service) noteDecoderReject() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.decoder++
	if s.cfg.SubmitFuse400s > 0 && s.decoder >= s.cfg.SubmitFuse400s {
		s.fence = true
	}
}

// recordDriverFailure writes a research record for a degraded state without
// finishing the rung (the queue id was lost, the rung continues).
func (s *Service) recordDriverFailure(r *rung, de *driverError) {
	r.mu.Lock()
	r.code, r.status, r.msg = de.Code, de.Status, de.Msg
	r.mu.Unlock()
	s.record(r)
	s.emitGap(context.Background(), "research_solver_unavailable", r, map[string]any{"note": de.Note})
}

// cadenceFor returns the pinned cadence for a class, or the configured start.
func (s *Service) cadenceFor(class string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.cadence[class]; ok && c != "" {
		return c
	}
	return s.cfg.Cadence
}

// description builds the submitted free text: the caller's description, else the
// slug plus the event message (§3.3). Capped at 8KB and scrubbed on egress.
func (s *Service) description(r *rung) string {
	d := strings.TrimSpace(r.desc)
	if d == "" {
		d = fmt.Sprintf("%s: %s", r.slug.Slug, asString(r.extra["message"], ""))
	}
	if len(d) > 8192 {
		d = d[:8192]
	}
	if s.deps != nil {
		if res, err := s.deps.Scrub(context.Background(), types.TgEventMsg, []byte(d)); err == nil && res.Value != nil {
			d = string(res.Value)
			r.mu.Lock()
			r.redactions += res.Redactions
			r.mu.Unlock()
		}
	}
	return d
}

// submitContext builds the provenance-carrying context object (§3.3): always a
// JSON object, always scrubbed before egress (§4.4).
func (s *Service) submitContext(r *rung) map[string]any {
	m := map[string]any{"fingerprint": r.sig.String()}
	for _, k := range []string{"stack", "release", "unit", "repro", "evidence", "env", "lang", "version", "message"} {
		if v, ok := r.extra[k]; ok && !isZero(v) {
			m[k] = v
		}
	}
	// §3.10a (AC-31): the persisted bundle is embedded at context.codeplane
	// verbatim — never re-derived, never merged with the evidence bundle. When
	// inc.Codeplane is non-nil it replaces any caller-supplied copy (single
	// source); when it is nil the key is absent, so the lab can tell a
	// system-only incident from a code-plane one with no code-plane facts.
	if inc := r.inc; inc.Codeplane != nil {
		m["codeplane"] = inc.Codeplane
	} else if caller, ok := r.extra["codeplane"]; ok && caller != nil {
		m["codeplane"] = caller
	}
	m["origin"] = map[string]any{"host_id": s.cfg.HostID, "hub_id": "", "source": r.sig.String()}
	m["incident_id"] = r.inc.ID
	m["daemon_version"] = s.cfg.DaemonVersion
	if s.deps != nil {
		if b, err := json.Marshal(m); err == nil {
			if res, err := s.deps.Scrub(context.Background(), types.TgStack, b); err == nil && res.Value != nil {
				var back map[string]any
				if json.Unmarshal(res.Value, &back) == nil {
					m = back
				}
				r.mu.Lock()
				r.redactions += res.Redactions
				r.mu.Unlock()
			}
		}
	}
	return m
}

// briefFromAnswer accepts an answer and builds the brief (§3.2 step 4, §3.7).
// Only `verified` / `ci_passed` with signatures.result != failed are briefs.
func (s *Service) briefFromAnswer(ans map[string]any, fromCorpus bool) (map[string]any, bool) {
	if !answerAccepted(ans) {
		return nil, false
	}
	brief := map[string]any{
		"answer":           ans,
		"solution":         asString(ans["solution"], ""),
		"status":           answerStatus(ans),
		"corpus_grep_hit":  fromCorpus,
		"resolve_outright": true,
	}
	// Local validation (TROUBLE-RESEARCH-009): a brief without solution text is
	// unusable, and an unusable brief is never handed to an agent.
	if strings.TrimSpace(asString(brief["solution"], "")) == "" {
		return nil, false
	}
	if s.cfg.ApplyBriefAsPlay {
		// research.apply_brief_as_play drafts a play from the answer as DATA:
		// research never executes it (SPEC-05 T22 owns applying it), and a task
		// whose tool is not a string is dropped rather than half-drafted.
		if draft := playDraftOf(ans); len(draft) > 0 {
			brief["play_draft"] = draft
		}
	}
	b, err := json.Marshal(brief)
	if err != nil {
		return nil, false
	}
	if s.cfg.BriefMaxBytes > 0 && len(b) > s.cfg.BriefMaxBytes {
		brief["truncated"] = true
		for len(b) > s.cfg.BriefMaxBytes {
			sol := asString(brief["solution"], "")
			if len(sol) <= 128 {
				break
			}
			brief["solution"] = sol[:len(sol)/2]
			nb, err := json.Marshal(brief)
			if err != nil {
				break
			}
			b = nb
		}
	}
	return brief, true
}

// settle is the async half of finish: it stamps the terminal state, writes the
// record and only then closes the door. A caller waiting in Poll must never
// observe a terminal state whose record has not landed yet.
func (s *Service) settle(r *rung, state, reason string, code types.ErrorCode, status int, msg string) types.ResearchOutcome {
	out := r.mark(state, reason, code, status, msg)
	s.record(r)
	r.seal()
	return out
}

// isDone reports whether the rung reached a terminal state.
func (r *rung) isDone() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fin
}

// seal closes the rung's door exactly once.
func (r *rung) seal() {
	select {
	case <-r.done:
	default:
		close(r.done)
	}
}

// mark stamps a state on the rung WITHOUT closing its door. The async paths
// (settle) use it so the ledger record lands before a waiter can wake up.
func (r *rung) mark(state, reason string, code types.ErrorCode, status int, msg string) types.ResearchOutcome {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.out.State = state
	if state == types.ResReturned {
		r.out.ReturnedTS = types.NowUTC()
		if r.brief != nil {
			// The brief must be on the stored outcome, not only on the copy the
			// caller receives: Poll() reads the rung's own outcome.
			r.out.Brief = r.brief
		}
	}
	if state == types.ResDegraded || state == types.ResSkipped {
		r.out.DegradedReason = reason
	}
	r.code, r.status, r.msg = code, status, msg
	if state != types.ResRequested {
		r.fin = true
	}
	return r.out
}

// finish stamps a terminal state and closes the door (the synchronous paths: no
// caller can be waiting on a rung that never had a poller).
func (r *rung) finish(state, reason string, code types.ErrorCode, status int, msg string) types.ResearchOutcome {
	out := r.mark(state, reason, code, status, msg)
	if state != types.ResRequested {
		r.seal()
	}
	return out
}

// charge records one request's cost for the rung and the process. Request counts
// are exact; the byte figures come from the measured §3.8 table, which is why
// they are recorded as an estimate-bearing cost object rather than a measurement.
func (s *Service) charge(r *rung, kind string, in, out int64) {
	if r != nil {
		r.mu.Lock()
		r.cost.charge(kind, in, out)
		r.mu.Unlock()
	}
	s.mu.Lock()
	s.cost.charge(kind, in, out)
	s.mu.Unlock()
}

// record writes the `research` record for the rung's current state (§3.6). The
// key set is pinned, so a reader can compare two rungs without guessing.
func (s *Service) record(r *rung) {
	if s.deps == nil {
		return
	}
	r.mu.Lock()
	out := r.out
	cost := r.cost
	code, status, msg := r.code, r.status, r.msg
	redactions := r.redactions
	brief := r.brief
	r.mu.Unlock()

	payload := map[string]any{
		"res_id": out.ID, "state": out.State, "driver": out.Driver,
		"slug": out.Slug.Slug, "slug_fallback": out.Slug.Fallback,
		"source": out.Slug.Source, "app_kind": out.Slug.AppKind, "taxonomy": out.Slug.Taxonomy,
		"idem_key": r.idem, "submission_id": out.SubmissionID, "duplicate": code == types.CodeResearch003,
		"cadence_attempted": s.cadenceFor(out.Slug.Slug), "cadence_accepted": s.cadenceFor(out.Slug.Slug),
		"http_status": status, "error_code": string(code),
		"degraded_reason": out.DegradedReason, "skip_reason": skipReasonOf(out),
		"corpus_grep_hit": out.CorpusGrepHit, "corpus_path": r.corpusPath,
		"corpus_parse_errors": r.corpusErrs, "corpus_timeout": r.corpusTO,
		"poll_count": r.polls, "elapsed_ms": 0,
		"queue_depth_at_submit": r.queueDepth, "unknown_status": r.unknown,
		"cost": cost.asMap(), "error_message": msg, "returned_ts": out.ReturnedTS,
	}
	if brief != nil {
		b := canonicalJSON(brief)
		truncated := boolField(brief, "truncated")
		if s.cfg.BriefMaxBytes > 0 && len(b) > s.cfg.BriefMaxBytes {
			b = b[:s.cfg.BriefMaxBytes]
			truncated = true
		}
		payload["brief_digest"] = Digest(canonicalJSON(brief))
		payload["brief"] = json.RawMessage(b)
		payload["brief_truncated"] = truncated
		// prompt_digest pins the prompt the agent would have seen, which is what
		// makes "the agent consumed the brief" (AC-20) auditable.
		if p, err := buildPromptCapped(r.inc, out, r.extra,
			promptCaps{Brief: s.cfg.PromptBriefMaxBytes, Prompt: s.cfg.PromptMaxBytes}); err == nil {
			payload["prompt_digest"] = Digest([]byte(p))
		}
	}
	if out.State == types.ResSkipped {
		payload["skip_reason"] = out.DegradedReason
		payload["degraded_reason"] = ""
	}
	draft := types.RecordDraft{
		Kind: types.KResearch, Sig: out.Slug.Source, Inc: out.Inc,
		Redactions: redactions, Payload: payload,
	}
	if r.sig.String() != "" {
		draft.Sig = r.sig.String()
	}
	if _, err := s.deps.Append(context.Background(), draft); err != nil {
		// A ledger failure is the one error this package cannot paper over; it
		// stays visible in the snapshot rather than being swallowed.
		s.mu.Lock()
		s.appendErrs++
		s.lastAppendErr = err.Error()
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	s.outcomes[r.sig.String()] = append([]types.ResearchOutcome{out}, s.outcomes[r.sig.String()]...)
	s.mu.Unlock()
}

// emitGap writes a `gap` record for one of the five §3.6 conditions. est_lost is
// -1 everywhere: research degradation loses an enrichment, never evidence.
func (s *Service) emitGap(ctx context.Context, cause string, r *rung, detail map[string]any) {
	if s.deps == nil {
		return
	}
	r.mu.Lock()
	out := r.out
	r.mu.Unlock()
	gap := map[string]any{
		"gap_id": types.NewID(types.PEv), "sensor": "research", "scope": out.Slug.Slug,
		"from_ts": out.RequestedTS, "to_ts": types.FormatUTC(s.deps.Now()),
		"est_lost": -1, "cause": cause, "res_id": out.ID,
	}
	for k, v := range detail {
		gap[k] = v
	}
	draft := types.RecordDraft{
		Kind: types.KGap, Sig: r.sig.String(), Inc: out.Inc, Payload: gap,
	}
	_, _ = s.deps.Append(ctx, draft)
}

// skipReasonOf returns the skip reason for a skipped outcome (the reason rides
// in DegradedReason because ResearchOutcome has one reason field).
func skipReasonOf(out types.ResearchOutcome) string {
	if out.State == types.ResSkipped {
		return out.DegradedReason
	}
	return ""
}

// helpers -------------------------------------------------------------------

// sigFromIncident rebuilds the sig for a ResearchPort call: the incident's own
// sig when it has one, else the context's, else a source-only sig.
func sigFromIncident(inc types.Incident, sub types.Subject) types.Sig {
	if inc.Sig != "" {
		if s := sigFromString(inc.Sig); s.Source != types.SrcUnknown {
			return s
		}
	}
	if v, ok := sub.Context["sig"]; ok {
		if str, ok := v.(string); ok {
			return sigFromString(str)
		}
	}
	return sigFromString(inc.Sig)
}

// sigFromString parses a canonical sig string back into a Sig. An unparsable
// value yields a source-only sig, which still derives a slug from the taxonomy.
func sigFromString(s string) types.Sig {
	sig := types.Sig{Algo: types.SigAlgoSHA256, NormVersion: types.NormVersionV1, Source: types.SrcUnknown}
	if s == "" {
		return sig
	}
	parts := strings.Split(s, ":")
	if len(parts) > 0 {
		if types.SigSource(parts[0]).Valid() {
			sig.Source = types.SigSource(parts[0])
		}
	}
	if len(parts) >= 3 {
		sig.Short = parts[2]
	}
	return sig
}

// isNotFound reports the lab's 404 on a discover: a miss, not a failure.
func isNotFound(err error) bool { return errors.Is(err, errNotFound) }

// shortOf renders a sig's 16-hex short form.
func shortOf(sig types.Sig) string {
	if sig.Short != "" {
		return sig.Short
	}
	return sig.String()
}

// nextCadence walks the cadence ladder, skipping values already attempted.
func nextCadence(ladder []string, attempted []string) (string, bool) {
	for _, c := range ladder {
		done := false
		for _, a := range attempted {
			if a == c {
				done = true
				break
			}
		}
		if !done {
			return c, true
		}
	}
	return "", false
}

// isZero reports whether a bundle value carries no information.
func isZero(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(t) == ""
	case map[string]any:
		return len(t) == 0
	case []any:
		return len(t) == 0
	}
	return false
}

// sortedKeys is the deterministic key order used by the read models and tests.
func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// containsString is a tiny membership test (no dependency on slices).
func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// asBool/asString/asStrings/asInt64/asFloat/asDur/asStringMap are the config
// coercions: a wrong type falls back to the default rather than panicking.
func asBool(v any, def bool) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return def
}

func asString(v any, def string) string {
	if s, ok := v.(string); ok {
		return s
	}
	return def
}

func asStrings(v any, def []string) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return def
}

func asInt64(v any, def int64) int64 {
	switch t := v.(type) {
	case int:
		return int64(t)
	case int64:
		return t
	case float64:
		return int64(t)
	case string:
		var n int64
		if _, err := fmt.Sscanf(t, "%d", &n); err == nil {
			return n
		}
	}
	return def
}

func asFloat(v any, def float64) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	}
	return def
}

func asDur(v any, def time.Duration) time.Duration {
	switch t := v.(type) {
	case time.Duration:
		return t
	case string:
		if d, err := time.ParseDuration(t); err == nil {
			return d
		}
	case int64:
		return time.Duration(t)
	case float64:
		return time.Duration(t)
	}
	return def
}

func asStringMap(v any) map[string]string {
	out := map[string]string{}
	switch t := v.(type) {
	case map[string]string:
		return t
	case map[string]any:
		for k, e := range t {
			if s, ok := e.(string); ok {
				out[k] = s
			}
		}
	}
	return out
}

// reasonOf/codeOf read a driverError's fields with defaults, and the *Err forms
// unwrap a plain error first: the poll loop turns any failure into a §5 outcome
// without guessing the code.
func reasonOf(de *driverError, def string) string {
	if de != nil && de.Reason != "" {
		return de.Reason
	}
	return def
}

func codeOf(de *driverError, def types.ErrorCode) types.ErrorCode {
	if de != nil && de.Code != "" {
		return de.Code
	}
	return def
}

func reasonOfErr(err error, def string) string {
	var de *driverError
	if errors.As(err, &de) {
		return reasonOf(de, def)
	}
	return def
}

func codeOfErr(err error, def types.ErrorCode) types.ErrorCode {
	var de *driverError
	if errors.As(err, &de) {
		return codeOf(de, def)
	}
	return def
}

// playDraftOf turns an answer's task list into PlayTask-shaped maps (SPEC-07
// §3.2). A task without a tool name is dropped: a draft is either well-formed
// data or it is not a draft.
func playDraftOf(ans map[string]any) []map[string]any {
	raw, ok := ans["tasks"]
	if !ok {
		if raw, ok = ans["play"]; !ok {
			return nil
		}
	}
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		tool := asString(m["tool"], "")
		if tool == "" {
			continue
		}
		task := map[string]any{
			"name": asString(m["name"], tool), "tool": tool,
			"args": m["args"], "when": asString(m["when"], ""),
			"register": asString(m["register"], ""),
			"retries":  asInt64(m["retries"], 0),
			"on_fail":  asString(m["on_fail"], ""),
		}
		out = append(out, task)
	}
	return out
}
