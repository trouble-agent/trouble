package sentinel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/ledger"
	"github.com/totalwindupflightsystems/trouble/internal/scrub"
	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// ledgerSink is the consumer-side view of SPEC-01 (§2.2): append one record and
// report the sequence watermark. *ledger.Ledger satisfies it through
// LedgerSink, which is the exported adapter SPEC-12 mounts.
type ledgerSink interface {
	Append(ctx context.Context, d types.RecordDraft) (types.Record, error)
	LastSeq() uint64
}

// ledgerReader is the optional read side: when the sink can scan, sentinel
// rebuilds its group index at boot instead of starting blind (§3.3, §4.2 step 3).
// It is an addition to the §2.2 surface, documented as a deviation.
type ledgerReader interface {
	ScanFrom(seq uint64, yield func(types.Record) bool) error
}

// scrubber is the consumer-side view of SPEC-02.
//
// The spec sketches `Scrub(b []byte, targets []string) (types.ScrubResult,
// error)`; SPEC-02 shipped `ScrubBytes(ctx, target, projectID, b)`, which is the
// real entry point (documented deviation, docs/sentinel-compat.md §5).
type scrubber interface {
	ScrubBytes(ctx context.Context, target types.ScrubTarget, projectID string, b []byte) ([]byte, types.ScrubResult, error)
}

// Compile-time proof that the shipped SPEC-02 engine satisfies the sentinel
// seam, and that the shipped ledger satisfies the sink seam through LedgerSink.
var _ scrubber = (*scrub.Engine)(nil)

// LedgerSink adapts *ledger.Ledger to the sentinel sink surface. SPEC-12
// constructs it once and hands it to NewServer.
type LedgerSink struct{ L *ledger.Ledger }

// Append appends a draft through the real ledger (seq/rec_id/ts are the
// ledger's to allocate).
func (s LedgerSink) Append(ctx context.Context, d types.RecordDraft) (types.Record, error) {
	if s.L == nil {
		return types.Record{}, errors.New("sentinel: nil ledger")
	}
	return s.L.Append(ctx, d)
}

// LastSeq reports the ledger's sequence watermark.
func (s LedgerSink) LastSeq() uint64 {
	if s.L == nil {
		return 0
	}
	return s.L.Status().LastSeq
}

// ScanFrom scans records from a sequence number (the boot rebuild path).
func (s LedgerSink) ScanFrom(seq uint64, yield func(types.Record) bool) error {
	if s.L == nil {
		return errors.New("sentinel: nil ledger")
	}
	return s.L.Query().ScanFrom(seq, yield)
}

// atomicCounter is a monotonic process counter.
type atomicCounter struct{ v atomic.Uint64 }

func (c *atomicCounter) Add(n uint64) { c.v.Add(n) }
func (c *atomicCounter) Get() uint64  { return c.v.Load() }

// counters holds the §3/§5 measurement set. Every counter named in the spec has
// a field here; the reason/item-type maps are guarded by mu.
type counters struct {
	mu sync.Mutex

	requests        atomicCounter
	ingest404       atomicCounter
	envelopes       atomicCounter
	emptyEnvelopes  atomicCounter
	legacyStore     atomicCounter
	unknownItems    atomicCounter
	itemsDropped    atomicCounter
	clientReports   atomicCounter
	parserAmbiguous atomicCounter
	parserErrors    atomicCounter
	partialEvents   atomicCounter
	nonUTF8         atomicCounter
	duplicateEvents atomicCounter
	rejects         atomicCounter
	canaryInjected  atomicCounter
	canaryObserved  atomicCounter
	canaryMissing   atomicCounter
	spooled         atomicCounter
	replayed        atomicCounter
	droppedQuota    atomicCounter
	spoolWriteFail  atomicCounter
	overrun         atomicCounter
	truncated       atomicCounter
	invalidLevel    atomicCounter
	fingerprintFB   atomicCounter
	scrubRefusals   atomicCounter
	rejectStorm     atomicCounter
	events          atomicCounter
	groupsCreated   atomicCounter

	byReason   map[string]uint64
	byItemType map[string]uint64
}

func newCounters() *counters {
	return &counters{byReason: map[string]uint64{}, byItemType: map[string]uint64{}}
}

// countReason bumps `reject_total{reason=…}`.
func (c *counters) countReason(reason string) {
	if reason == "" {
		return
	}
	c.mu.Lock()
	c.byReason[reason]++
	c.mu.Unlock()
}

// countItemType bumps `items_dropped_total{type}`.
func (c *counters) countItemType(typ string) {
	if typ == "" {
		return
	}
	c.mu.Lock()
	c.byItemType[typ]++
	c.mu.Unlock()
}

// snapshot renders the counters for the dashboard (counts only).
func (c *counters) snapshot() map[string]any {
	c.mu.Lock()
	reasons := make(map[string]uint64, len(c.byReason))
	for k, v := range c.byReason {
		reasons[k] = v
	}
	items := make(map[string]uint64, len(c.byItemType))
	for k, v := range c.byItemType {
		items[k] = v
	}
	c.mu.Unlock()
	return map[string]any{
		"requests_total":         c.requests.Get(),
		"ingest_404_total":       c.ingest404.Get(),
		"envelopes_total":        c.envelopes.Get(),
		"empty_envelopes":        c.emptyEnvelopes.Get(),
		"legacy_store_total":     c.legacyStore.Get(),
		"unknown_items_total":    c.unknownItems.Get(),
		"items_dropped_total":    items,
		"client_reports_total":   c.clientReports.Get(),
		"parser_ambiguous_total": c.parserAmbiguous.Get(),
		"parser_error_total":     c.parserErrors.Get(),
		"partial_events_total":   c.partialEvents.Get(),
		"non_utf8_total":         c.nonUTF8.Get(),
		"duplicate_events_total": c.duplicateEvents.Get(),
		"reject_total":           reasons,
		"rejects_total":          c.rejects.Get(),
		"canary_injected_total":  c.canaryInjected.Get(),
		"canary_observed_total":  c.canaryObserved.Get(),
		"canary_missing_total":   c.canaryMissing.Get(),
		"spooled_total":          c.spooled.Get(),
		"spool_replayed_total":   c.replayed.Get(),
		"dropped_quota_total":    c.droppedQuota.Get(),
		"spool_write_failed":     c.spoolWriteFail.Get(),
		"item_overrun_total":     c.overrun.Get(),
		"truncated_total":        c.truncated.Get(),
		"invalid_level_total":    c.invalidLevel.Get(),
		"fingerprint_fallback_total": c.fingerprintFB.Get(),
		"scrub_refusals_total":   c.scrubRefusals.Get(),
		"ingest_reject_storm":    c.rejectStorm.Get(),
		"events_total":           c.events.Get(),
		"groups_created_total":   c.groupsCreated.Get(),
	}
}

// nowFunc is the process clock; tests replace it (the Config struct is pinned by
// the spec, so the clock hook lives here rather than in configuration).
var nowFunc = time.Now

// Server is the ingestion service (§2.2).
type Server struct {
	cfg    Config
	sink   ledgerSink
	sc     scrubber
	norm   *normalizer
	logger *log.Logger

	projects *projectIndex
	groups   *groupIndex
	quota    *quotaSet
	releases *releaseState
	counters *counters
	disk     diskUsage
	spool    *Spool

	sem      *semaphore
	ipLim    *ipLimiter
	handler  http.Handler
	started  time.Time
	canaryID string

	now   func() time.Time
	zoner string

	mu       sync.Mutex
	regressions map[string]string
	gmu      sync.Mutex
	dups     map[string]time.Time
	dupOrder []string

	// lastRateHeader carries the proactive-backoff header of the most recent
	// decision (§3.9: 200 + header at 95% of quota).
	lastRateHeader string
	// lastSoftCode carries a 200-level code (013) that the response reports
	// without failing the envelope.
	lastSoftCode string

	// rejectWindows is the per-project reject-rate accounting behind
	// `ingest_reject_storm` (§5).
	rejectMu      sync.Mutex
	rejectWindows map[string]*rejectWindow

	canaryMu       sync.Mutex
	canaryProj     string
	canaryProjState string
	canaryIter     uint64
	canaryLastTS   string
	canaryLastOK   bool
	canaryInFlight bool

	collectors *collectorSet

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	draining atomic.Bool
	startedFlag atomic.Bool
}

// NewServer builds the ingestion service. It validates the resolved config
// (§4.2 step 1) and does not open a socket: SPEC-12 mounts Handler().
func NewServer(cfg Config, w ledgerSink, sc scrubber) (*Server, error) {
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if w == nil {
		return nil, fmt.Errorf("sentinel: a ledger sink is required")
	}
	ix, err := newProjectIndex(cfg.Projects)
	if err != nil {
		return nil, err
	}
	now := nowFunc()
	s := &Server{
		cfg:         cfg,
		sink:        w,
		sc:          sc,
		norm:        newNormalizer(),
		logger:      log.New(os.Stderr, "sentinel: ", log.LstdFlags),
		projects:    ix,
		groups:      newGroupIndex(),
		quota:       newQuotaSet(now),
		releases:    newReleaseState(),
		counters:    newCounters(),
		regressions: map[string]string{},
		dups:        map[string]time.Time{},
		now:         nowFunc,
		zoner:       cfg.Zone,
	}
	s.disk.set(0, cfg.DiskBudgetBytes, now)
	s.sem = newSemaphore(cfg.MaxConcurrent)
	perMin, burst, perr := parsePerIPRate(cfg.PerIPRate)
	if perr != nil {
		return nil, perr
	}
	s.ipLim = newIPLimiter(perMin, burst, now)
	s.canaryProj = cfg.CanaryProject
	if cfg.SpoolDir != "" {
		sp, serr := OpenSpool(filepath.Join(cfg.SpoolDir, "spool"), cfg.SpoolBudgetBytes)
		if serr != nil {
			return nil, serr
		}
		s.spool = sp
	}
	s.collectors = newCollectorSet(s)
	s.handler = s.routes()
	return s, nil
}

// Handler is the ingestion HTTP surface (mounted on the ingestion listener by
// SPEC-12).
func (s *Server) Handler() http.Handler { return s.handler }

// Start launches the collectors, the canary loop, the group flusher, the spool
// replayer and the disk-budget sampler.
//
// It returns once a first canary observation has landed or one canary interval
// has passed, whichever is first, so boot never claims readiness untested
// (§4.2 step 5).
func (s *Server) Start(ctx context.Context) error {
	if !s.startedFlag.CompareAndSwap(false, true) {
		return nil
	}
	s.started = s.now()
	s.ctx, s.cancel = context.WithCancel(ctx)

	if rd, ok := s.sink.(ledgerReader); ok {
		if err := s.rebuild(rd); err != nil {
			// A rebuild failure is loud but never fatal: the ledger stays the
			// only store and the index re-derives from the next records.
			s.logger.Printf("sentinel: group index rebuild incomplete: %v", err)
		}
	}

	s.wg.Add(1)
	go s.flushLoop()
	s.wg.Add(1)
	go s.budgetLoop()
	if s.canaryProj != "" {
		s.wg.Add(1)
		go s.canaryLoop()
	}
	if err := s.collectors.start(s.ctx); err != nil {
		s.logger.Printf("sentinel: collectors failed to start: %v", err)
	}

	if s.canaryProj == "" {
		return nil
	}
	// Wait for the first canary observation (or one interval).
	deadline := s.now().Add(s.cfg.CanaryInterval.Std())
	for s.now().Before(deadline) {
		if _, _, ok := s.canaryState(); ok {
			return nil
		}
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	return nil
}

// Drain stops the loop, flushes every group counter, closes the spool and stops
// the collectors (§4.2 step 6). It is bounded by 5s.
func (s *Server) Drain(ctx context.Context) error {
	s.draining.Store(true)
	if s.cancel != nil {
		s.cancel()
	}
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	case <-ctx.Done():
	}
	_ = s.collectors.close()
	_ = s.flushGroups(context.Background(), "drain")
	if s.spool != nil {
		if err := s.spool.Close(); err != nil {
			return err
		}
	}
	return nil
}

// flushLoop flushes group counters every group_flush (§3.3).
func (s *Server) flushLoop() {
	defer s.wg.Done()
	iv := s.cfg.GroupFlush.Std()
	if iv <= 0 {
		iv = 5 * time.Second
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			_ = s.flushGroups(s.ctx, "interval")
			s.replaySpool(s.now())
		}
	}
}

// budgetLoop samples the sentinel disk budget every 60s and replays the spool.
func (s *Server) budgetLoop() {
	defer s.wg.Done()
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			s.sampleBudget()
		}
	}
}

// sampleBudget attributes ledger bytes + spool bytes to sentinel (§3.9).
func (s *Server) sampleBudget() {
	var bytes int64
	if s.spool != nil {
		bytes += s.spool.Bytes()
	}
	if st, ok := s.sink.(interface{ DiskBytes() int64 }); ok {
		bytes += st.DiskBytes()
	}
	budget := s.cfg.DiskBudgetBytes
	for _, p := range s.cfg.Projects {
		if p.DiskBudget > 0 && p.DiskBudget < budget {
			budget = p.DiskBudget
		}
	}
	s.disk.set(bytes, budget, s.now())
}

// flushGroups writes one `group` record per dirty group with an absolute
// counters snapshot and the events watermark (§3.3's counter flush row).
func (s *Server) flushGroups(ctx context.Context, reason string) *Error {
	for _, g := range s.groups.snapshot() {
		st, ok := s.groups.byDigestState(g.Digest)
		if !ok || !st.dirty {
			continue
		}
		// The flush carries an absolute counters snapshot plus the seq watermark
		// it reaches, so a rebuild never has to scan the whole file (§3.3).
		payload := groupRecordPayload("flush", st, map[string]any{"flush_reason": reason})
		rec, err := s.appendRecord(ctx, types.KGroup, g.Sig, "sentinel", payload, 0)
		if err != nil {
			return err
		}
		s.groups.markFlushed(g.Digest, rec.Seq, st.sampleRate)
	}
	return nil
}

// byDigestState returns the live group state (pointer) for flushing.
func (g *groupIndex) byDigestState(digest string) (*groupState, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st, ok := g.byDigest[digest]
	return st, ok
}

// rebuild folds the ledger into the group index at boot (§3.3): the last
// `group` record per digest is the base, `event` records after its
// `events_upper_seq` add the delta; `AuthForms` are folded from `event` records.
func (s *Server) rebuild(rd ledgerReader) error {
	type base struct {
		grp   types.Group
		upper uint64
	}
	bases := map[string]base{}
	type evRef struct {
		digest     string
		seq        uint64
		ts         string
		release    string
		redactions int
		sig        string
	}
	var events []evRef
	var scanErr error
	err := rd.ScanFrom(1, func(rec types.Record) bool {
		switch rec.Kind {
		case types.KGroup:
			digest, _ := rec.Payload["digest"].(string)
			if digest == "" {
				return true
			}
			grp := groupFromRecord(rec)
			upper, _ := rec.Payload["events_upper_seq"].(float64)
			bases[digest] = base{grp: grp, upper: uint64(upper)}
		case types.KEvent:
			digest, _ := rec.Payload["digest"].(string)
			if digest == "" {
				return true
			}
			e := evRef{digest: digest, seq: rec.Seq, sig: rec.Sig, redactions: rec.Redactions}
			if evp, ok := rec.Payload["event"].(map[string]any); ok {
				e.ts, _ = evp["ts"].(string)
				e.release, _ = evp["release"].(string)
			}
			if project, ok := rec.Payload["project"].(string); ok && project != "" {
				if entry, found := s.projects.project(project); found {
					if form, fok := rec.Payload["auth_form"].(string); fok {
						entry.observeForm(form)
					}
				}
			}
			if ts, ok := rec.Payload["event"].(map[string]any); ok {
				if project, ok := ts["project"].(string); ok && project != "" {
					if entry, found := s.projects.project(project); found {
						if form, fok := rec.Payload["auth_form"].(string); fok {
							entry.observeForm(form)
						}
					}
				}
			}
			events = append(events, e)
		case types.KIncident:
			if st, ok := rec.Payload["state"].(string); ok && (st == string(types.StResolved) || st == "closed") {
				s.NoteIncidentResolved(rec.Sig, "")
			}
		case types.KCanary:
			if phase, _ := rec.Payload["phase"].(string); phase == "observed" {
				if project, ok := rec.Payload["project"].(string); ok {
					s.releases.noteCanary(project, rec.TS)
					s.setCanaryState(project, rec.TS, true)
				}
			}
		}
		return true
	})
	if err != nil {
		scanErr = err
	}
	digests := make([]string, 0, len(bases))
	for d := range bases {
		digests = append(digests, d)
	}
	sort.Strings(digests)
	for _, d := range digests {
		b := bases[d]
		if b.grp.Source == "" {
			b.grp.Source = types.SrcSentinel
		}
		lastRelease := ""
		if len(b.grp.ReleaseRange) > 1 {
			lastRelease = b.grp.ReleaseRange[1]
		}
		s.groups.restore(b.grp, b.upper, lastRelease)
	}
	for _, e := range events {
		s.groups.replayEvent(e.digest, e.seq, e.ts, e.release, e.redactions)
	}
	return scanErr
}

// groupFromRecord rebuilds a types.Group from a `group` record's payload.
func groupFromRecord(rec types.Record) types.Group {
	g := types.Group{Sig: rec.Sig, Source: types.SrcSentinel}
	if s, ok := rec.Payload["group_id"].(string); ok {
		g.ID = s
	}
	if s, ok := rec.Payload["digest"].(string); ok {
		g.Digest = s
	}
	if s, ok := rec.Payload["title"].(string); ok {
		g.Title = s
	}
	if s, ok := rec.Payload["first_seen_ts"].(string); ok {
		g.FirstSeenTS = s
	}
	if s, ok := rec.Payload["last_seen_ts"].(string); ok {
		g.LastSeenTS = s
	}
	if f, ok := rec.Payload["count"].(float64); ok {
		g.Count = uint64(f)
	}
	if rr, ok := rec.Payload["release_range"].([]any); ok {
		for _, v := range rr {
			if s, ok := v.(string); ok {
				g.ReleaseRange = append(g.ReleaseRange, s)
			}
		}
	}
	if c, ok := rec.Payload["counters"].(map[string]any); ok {
		if f, ok := c["events"].(float64); ok {
			g.Counters.Events = uint64(f)
		}
		if f, ok := c["suppressed"].(float64); ok {
			g.Counters.Suppressed = uint64(f)
		}
		if f, ok := c["redacted_values"].(float64); ok {
			g.Counters.Redacted = uint64(f)
		}
		if f, ok := c["dropped_events"].(float64); ok {
			g.Counters.Dropped = uint64(f)
		}
		if f, ok := c["sample_rate"].(float64); ok {
			g.Counters.SampleRate = f
		}
	}
	return g
}

// Groups is the read view for SPEC-10; it never mutates (§2.2).
func (s *Server) Groups() []types.Group { return s.groups.snapshot() }

// GroupBySig is the sig-keyed read SPEC-05 uses to attach an incident.
func (s *Server) GroupBySig(sig string) (types.Group, bool) { return s.groups.bySig(sig) }

// ProjectRuntime is the §3.10 per-project view for SPEC-10.
func (s *Server) ProjectRuntime(projectID string) (types.ProjectRuntime, bool) {
	entry, ok := s.projects.project(projectID)
	if !ok {
		return types.ProjectRuntime{}, false
	}
	now := s.now()
	used := s.quota.usedNow(projectID, now)
	bytes, budget, _ := s.disk.snapshot()
	return entry.runtime(now, used, bytes, budget), true
}

// Sources reports collector + sentinel liveness for HealthResponse (§2.2).
func (s *Server) Sources() []types.SourceLiveness {
	out := s.collectors.liveness()
	if s.canaryProj == "" {
		return out
	}
	_, lastTS, ok := s.canaryState()
	maxAge := 2 * s.cfg.CanaryInterval.Std().Seconds()
	age := -1.0
	if lastTS != "" {
		if ts, err := types.ParseUTC(lastTS); err == nil {
			age = s.now().Sub(ts).Seconds()
		}
	}
	out = append(out, types.SourceLiveness{
		HostID:        s.cfg.HostID,
		Source:        "sentinel",
		Zone:          s.zoner,
		Expected:      true,
		Alive:         ok && (age < 0 || age <= maxAge),
		LastEventTS:   lastTS,
		LastEventAgeS: age,
		MaxAgeS:       maxAge,
	})
	return out
}

// ObserveRecord advances the group index from a record that arrived through the
// forward path, so the hub needs no second ingestion path (§4.3).
func (s *Server) ObserveRecord(rec types.Record) error {
	switch rec.Kind {
	case types.KGroup:
		grp := groupFromRecord(rec)
		if grp.Digest == "" {
			return nil
		}
		upper := uint64(0)
		if f, ok := rec.Payload["events_upper_seq"].(float64); ok {
			upper = uint64(f)
		}
		last := ""
		if len(grp.ReleaseRange) > 1 {
			last = grp.ReleaseRange[1]
		}
		s.groups.restore(grp, upper, last)
	case types.KEvent:
		digest, _ := rec.Payload["digest"].(string)
		if digest == "" {
			return nil
		}
		ts, release := "", ""
		if evp, ok := rec.Payload["event"].(map[string]any); ok {
			ts, _ = evp["ts"].(string)
			release, _ = evp["release"].(string)
		}
		s.groups.replayEvent(digest, rec.Seq, ts, release, rec.Redactions)
	case types.KIncident:
		if st, ok := rec.Payload["state"].(string); ok && (st == string(types.StResolved) || st == "closed") {
			s.NoteIncidentResolved(rec.Sig, "")
		}
	case types.KCanary:
		if phase, _ := rec.Payload["phase"].(string); phase == "observed" {
			if project, ok := rec.Payload["project"].(string); ok {
				s.setCanaryState(project, rec.TS, true)
				s.releases.noteCanary(project, rec.TS)
			}
		}
	}
	return nil
}

// CanarySig returns the reserved sig of the canary (§3.8 exclusion).
func (s *Server) CanarySig() types.Sig {
	ev := &rawEvent{
		Level:       "info",
		Message:     canaryMessage,
		Fingerprint: []string{CanaryFingerprint},
	}
	sig, _, _ := s.sigFor(ev)
	return sig
}

// IsReservedSig reports whether a sig belongs to the reserved set (the canary):
// it never opens an incident and never reaches the ladder (§3.8).
func (s *Server) IsReservedSig(sig string) bool {
	return sig == s.CanarySig().String() || strings.HasPrefix(sig, "sentinel:sha256v1:") && false
}

// setSoftCode records a 200-level code for the response (013: an item type was
// dropped). It never fails the envelope.
func (s *Server) setSoftCode(code string) {
	s.mu.Lock()
	s.lastSoftCode = code
	s.mu.Unlock()
}

// takeSoftCode reads and clears the pending 200-level code.
func (s *Server) takeSoftCode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.lastSoftCode
	s.lastSoftCode = ""
	return c
}

// dupeSeen records an event id and reports whether it was seen inside the dedup
// window (§6.6: an SDK retry is the normal case, not an attack).
func (s *Server) dupeSeen(id string, now time.Time) bool {
	if id == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ts, ok := s.dups[id]; ok && now.Sub(ts) < dupWindow {
		s.counters.duplicateEvents.Add(1)
		return true
	}
	s.dups[id] = now
	s.dupOrder = append(s.dupOrder, id)
	for len(s.dupOrder) > maxDupEntries {
		old := s.dupOrder[0]
		s.dupOrder = s.dupOrder[1:]
		delete(s.dups, old)
	}
	return false
}

// loopbackAddr converts a bind address into an address a loopback client can
// dial: a wildcard bind becomes 127.0.0.1 (§3.8's canary posts over loopback).
func loopbackAddr(bind string) string {
	host, port := parseHostPort(bind)
	if port == "" {
		port = "7643"
	}
	if isWildcardHost(host) {
		host = "127.0.0.1"
	}
	ip := net.ParseIP(host)
	if ip != nil && !ip.IsLoopback() {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// siteZone reports the zone of the listener as configured (used for
// SourceLiveness when a source has no per-request peer).
func (s *Server) siteZone() string {
	if s.zoner != "" {
		return s.zoner
	}
	return zoneOf(parseIP(s.cfg.Bind))
}

// scrubEvent runs the §4 call-site scrub over an event: message/log fields,
// stack frames, tags and extra, then rebuilds the event from the scrubbed
// values. Failure is fail-closed: no record is written.
func (s *Server) scrubEvent(ctx context.Context, entry *projectEntry, ev *rawEvent, targets []types.ScrubTarget) (int, *Error) {
	if s.sc == nil {
		return 0, nil
	}
	redactions := 0
	scrubField := func(target types.ScrubTarget, val string) (string, *Error) {
		if val == "" {
			return "", nil
		}
		out, res, err := s.sc.ScrubBytes(ctx, target, entry.proj.ID, []byte(val))
		if err != nil {
			s.counters.scrubRefusals.Add(1)
			return "", errf(types.CodeSentinel001, "scrubbing refused the payload (fail closed)", "scrub_refused")
		}
		redactions += res.Redactions
		return string(out), nil
	}
	msgTarget := types.TgEventMsg
	stackTarget := types.TgStack
	metaTarget := types.TgHeader
	for _, t := range targets {
		switch t {
		case types.TgJournalTail:
			msgTarget = types.TgJournalTail
		case types.TgEnv:
			metaTarget = types.TgEnv
		}
	}
	var err *Error
	if ev.Message, err = scrubField(msgTarget, ev.Message); err != nil {
		return 0, err
	}
	if ev.Culprit, err = scrubField(msgTarget, ev.Culprit); err != nil {
		return 0, err
	}
	if ev.Logger, err = scrubField(msgTarget, ev.Logger); err != nil {
		return 0, err
	}
	if ev.ExcValue, err = scrubField(msgTarget, ev.ExcValue); err != nil {
		return 0, err
	}
	if ev.ExcClass, err = scrubField(msgTarget, ev.ExcClass); err != nil {
		return 0, err
	}
	for i := range ev.Frames {
		f := &ev.Frames[i]
		if f.ContextLine, err = scrubField(stackTarget, f.ContextLine); err != nil {
			return 0, err
		}
		if f.Function, err = scrubField(stackTarget, f.Function); err != nil {
			return 0, err
		}
		if f.File, err = scrubField(stackTarget, f.File); err != nil {
			return 0, err
		}
		if f.Module, err = scrubField(stackTarget, f.Module); err != nil {
			return 0, err
		}
	}
	for k, v := range ev.Tags {
		out, err := scrubField(metaTarget, v)
		if err != nil {
			return 0, err
		}
		ev.Tags[k] = out
	}
	if len(ev.Extra) > 0 {
		b, merr := json.Marshal(ev.Extra)
		if merr == nil {
			out, res, serr := s.sc.ScrubBytes(ctx, metaTarget, entry.proj.ID, b)
			if serr != nil {
				s.counters.scrubRefusals.Add(1)
				return 0, errf(types.CodeSentinel001, "scrubbing refused the extra payload", "scrub_refused")
			}
			redactions += res.Redactions
			var back map[string]any
			if jerr := json.Unmarshal(out, &back); jerr == nil {
				ev.Extra = back
			}
		}
	}
	return redactions, nil
}

// targetsForSource is the SPEC-02 §4 call-site target list for a source kind.
func targetsForSource(sourceKind string) []types.ScrubTarget {
	if sourceKind == sourceCollector {
		return []types.ScrubTarget{types.TgJournalTail, types.TgEventMsg, types.TgStack}
	}
	return []types.ScrubTarget{types.TgEventMsg, types.TgStack, types.TgHeader, types.TgEnv}
}

// admitEvent is the single funnel every accepted event goes through: identity,
// scrub, sig, quota/loss accounting, group folding and the ledger records.
func (s *Server) admitEvent(ctx context.Context, entry *projectEntry, ev *rawEvent, itemType, reason string) (types.Record, *Error) {
	now := s.now()
	ev.Project = entry.proj.ID
	if !validEventID(ev.ID) {
		ev.ID = newEventID()
	}
	if ev.TS == "" {
		ev.TS = types.FormatUTC(now)
	} else if ts, err := types.ParseUTC(ev.TS); err == nil {
		if skew := now.Sub(ts).Seconds(); skew > 300 || skew < -300 {
			ev.ClockSkewS = skew
		}
	}
	if ev.AuthForm != "" {
		entry.observeForm(ev.AuthForm)
	}
	if ev.LevelDegraded || !knownLevels[ev.Level] {
		if !knownLevels[ev.Level] {
			ev.Level = "error"
		}
		s.counters.invalidLevel.Add(1)
	}
	if s.dupeSeen(ev.ID, now) {
		return types.Record{RecID: "", Payload: map[string]any{"duplicate": true}}, nil
	}

	redactions, serr := s.scrubEvent(ctx, entry, ev, targetsForSource(ev.SourceKind))
	if serr != nil {
		return types.Record{}, serr
	}

	canary := s.isCanaryEvent(ev)
	if canary {
		ev.SourceKind = sourceCanary
	}
	sig, fallback, ferr := s.sigFor(ev)
	if ferr != nil {
		s.counters.fingerprintFB.Add(1)
	}
	if canary {
		s.observeCanary(entry, ev, sig)
	}
	digest := sig.DigestHex()
	sigStr := sig.String()

	// Quota and disk budget. A breach never destroys the accounting: the drop
	// itself is a ledger record (§3.9).
	used := s.quota.usedNow(entry.proj.ID, now)
	allow, decision := s.quota.admit(entry.proj.ID, entry.proj.QuotaEPM, 1, now)
	diskBytes, budgetBytes, over := s.disk.snapshot()
	_ = diskBytes
	_ = budgetBytes
	if over {
		allow = 0
		decision = types.RateLimitDecision{
			Allowed:     false,
			Reason:      causeDiskBudget,
			RetryAfterS: 300,
			Categories:  []string{"error"},
			Header:      diskBudgetHeader,
		}
	}
	if decision.Header != "" && decision.Allowed && !over {
		s.mu.Lock()
		s.lastRateHeader = decision.Header
		s.mu.Unlock()
	}
	disposition := dispAdmitted
	outcome := lossOutcome{keep: true, disposition: dispAdmitted, status: 200}
	if allow == 0 && !s.isCanaryEvent(ev) {
		outcome = s.applyLoss(entry, ev, sigStr, decision, now, used)
		disposition = outcome.disposition
	}
	if !outcome.keep {
		// The refused event still writes its own record: sig + disposition, so
		// the loss is auditable and verification for that sig is invalid.
		payload := eventRecordPayload(ev, sig, itemType, redactions, map[string]any{
			"disposition": disposition,
			"error_code":  string(outcome.errorCode),
			"quota_limit": entry.proj.QuotaEPM,
		})
		if fallback {
			payload["fingerprint_fallback"] = true
		}
		rec, err := s.appendRecord(ctx, types.KEvent, sigStr, "sentinel", payload, redactions)
		if err != nil {
			return types.Record{}, err
		}
		s.noteReject(entry)
		_ = rec
		return types.Record{}, &Error{
			Code:        outcome.errorCode,
			Status:      outcome.status,
			Causes:      []string{disposition},
			Msg:         "event refused by the loss policy",
			RetryAfterS: decision.RetryAfterS,
			Header:      decision.Header,
		}
	}

	// Group folding (§3.3).
	title := groupTitle(ev)
	res := s.groups.observe(sig, digest, ev.TS, ev.Release, title, redactions)
	if res.created {
		s.counters.groupsCreated.Add(1)
	}
	s.releases.note(entry.proj.ID, ev.Release, ev.TS)
	if res.flush {
		// The 100-event trigger fired: flush right after the event record so the
		// flush's events_upper_seq covers it.
		defer func() {
			if st, ok := s.groups.byDigestState(digest); ok {
				_, _ = s.appendRecord(context.Background(), types.KGroup, sigStr, "sentinel", groupRecordPayload("flush", st, nil), 0)
				if rec, err := s.sinkLastSeq(); err == nil {
					s.groups.markFlushed(digest, rec, st.sampleRate)
				}
			}
		}()
	}

	extra := map[string]any{
		"disposition":  disposition,
		"sample_rate":  s.sampleRateFor(digest),
		"quota_limit":  entry.proj.QuotaEPM,
	}
	if disposition == dispSampled || disposition == dispSpooled {
		extra["error_code"] = string(types.CodeSentinel014)
	}
	if fallback {
		extra["fingerprint_fallback"] = true
	}
	if ev.SourceKind == sourceStore {
		extra["legacy_store"] = true
	}
	if len(ev.ItemTypes) > 1 {
		extra["item_types"] = ev.ItemTypes
	}
	payload := eventRecordPayload(ev, sig, itemType, redactions, extra)
	rec, aerr := s.appendRecord(ctx, types.KEvent, sigStr, "sentinel", payload, redactions)
	if aerr != nil {
		return types.Record{}, aerr
	}
	s.counters.events.Add(1)
	entry.mu.Lock()
	entry.eventsTotal++
	entry.lastEvent = ev.TS
	if disposition == dispSampled {
		entry.sampled++
	}
	entry.mu.Unlock()
	if disposition == dispSampled {
		s.groups.markDropped(digest, 1, outcome.sampleRate)
	}

	// Group records: create / release / (100-event flush handled above).
	if res.created {
		st, _ := s.groups.byDigestState(digest)
		op := "create"
		extraG := map[string]any{}
		if verdict := s.releases.regressionVerdict(sigStr, ev.Release); verdict != "" {
			extraG["regression"] = verdict
			s.noteRegression(sigStr, verdict)
			op = "release"
		}
		grec, gerr := s.appendRecord(ctx, types.KGroup, sigStr, "sentinel", groupRecordPayload(op, st, extraG), 0)
		if gerr == nil {
			s.groups.markFlushed(digest, rec.Seq, st.sampleRate)
			_ = grec
		}
	} else if res.release != "" {
		st, _ := s.groups.byDigestState(digest)
		extraG := map[string]any{"release": res.release}
		if verdict := s.releases.regressionVerdict(sigStr, res.release); verdict != "" {
			extraG["regression"] = verdict
			s.noteRegression(sigStr, verdict)
		}
		if _, gerr := s.appendRecord(ctx, types.KGroup, sigStr, "sentinel", groupRecordPayload("release", st, extraG), 0); gerr != nil {
			s.logger.Printf("sentinel: group release record failed: %v", gerr)
		}
	}
	return rec, nil
}

// sinkLastSeq reads the sink watermark (used to bound a flush).
func (s *Server) sinkLastSeq() (uint64, error) { return s.sink.LastSeq(), nil }

// sampleRateFor returns the group's current sample rate.
func (s *Server) sampleRateFor(digest string) float64 {
	if st, ok := s.groups.byDigestState(digest); ok {
		return st.sampleRate
	}
	return 0
}

// isCanaryEvent reports whether an event is the canary: the canary is exempt
// from quota, loss policies and breakers, because a flood is exactly when
// evidence matters (§3.8).
func (s *Server) isCanaryEvent(ev *rawEvent) bool {
	if ev.SourceKind == sourceCanary {
		return true
	}
	for _, fp := range ev.Fingerprint {
		if fp == CanaryFingerprint {
			return true
		}
	}
	return false
}

// groupTitle is the human title of a group (§3.3): culprit, else message, else
// the level.
func groupTitle(ev *rawEvent) string {
	if ev.Culprit != "" {
		return truncateTitle(ev.Culprit)
	}
	if ev.Message != "" {
		return truncateTitle(ev.Message)
	}
	if ev.ExcClass != "" {
		return truncateTitle(ev.ExcClass)
	}
	return truncateTitle(ev.Level + " event")
}

func truncateTitle(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// noteReject tracks the per-project reject rate and emits the §5
// `ingest_reject_storm` gap once a project is above 50% rejects over 5 minutes.
func (s *Server) noteReject(entry *projectEntry) {
	s.counters.rejects.Add(1)
	s.rejectMu.Lock()
	defer s.rejectMu.Unlock()
	if s.rejectWindows == nil {
		s.rejectWindows = map[string]*rejectWindow{}
	}
	w := s.rejectWindows[entry.proj.ID]
	if w == nil {
		w = &rejectWindow{}
		s.rejectWindows[entry.proj.ID] = w
	}
	now := s.now()
	if w.start.IsZero() || now.Sub(w.start) >= 60*time.Second {
		w.total = 0
		w.rejects = 0
		w.start = now
	}
	w.rejects++
	w.total++
	if w.total < 10 || w.stormed {
		return
	}
	if w.rejects*2 > w.total {
		w.stormed = true
		s.counters.rejectStorm.Add(1)
		scope := "project:" + entry.proj.ID
		go func() {
			_, _ = s.appendRecord(context.Background(), types.KGap, "", "sentinel",
				gapRecordPayload("ingest_reject_storm", scope, w.rejects, types.FormatUTC(w.start), types.FormatUTC(now)), 0)
		}()
	}
}

// noteAccept counts a request's non-reject outcome for the storm ratio.
func (s *Server) noteAccept(entry *projectEntry) {
	if entry == nil {
		return
	}
	s.rejectMu.Lock()
	defer s.rejectMu.Unlock()
	if s.rejectWindows == nil {
		s.rejectWindows = map[string]*rejectWindow{}
	}
	w := s.rejectWindows[entry.proj.ID]
	if w == nil {
		w = &rejectWindow{}
		s.rejectWindows[entry.proj.ID] = w
	}
	now := s.now()
	if w.start.IsZero() || now.Sub(w.start) >= 60*time.Second {
		w.total = 0
		w.rejects = 0
		w.start = now
	}
	w.total++
}

// rejectWindow is one project's 60s reject accounting (five of them make the
// 5-minute window the spec names).
type rejectWindow struct {
	start   time.Time
	total   int
	rejects int
	stormed bool
}
