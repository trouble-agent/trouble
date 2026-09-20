package dashboard

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// testutil_test.go holds the fakes every dashboard test shares. Nothing here
// opens a socket to the outside world, touches a real ledger or waits on the
// wall clock: the index, the lookup, the write seams and the clock are all
// in-process fixtures.
//
// The fakes exist because SPEC-10's import rule is stdlib + internal/types:
// the dashboard's collaborators arrive through Deps, so the tests drive the
// same interfaces the composition root adapts.

// ---------------------------------------------------------------- clock

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 9, 16, 9, 14, 3, 221_000_000, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// ---------------------------------------------------------------- index

// fakeIndex is an in-memory Index. trigger() is the AC-19 stand-in for a real
// detection: it appends the incident and advances the ledger seq atomically,
// which is exactly what the writer does before the index publish.
type fakeIndex struct {
	mu        sync.RWMutex
	seq       uint64
	lastTS    string
	incidents map[string]types.Incident
	order     []string
	records   map[string][]types.Record
	groups    []types.GroupStat
	ages      map[string]float64
	perMin    float64

	// lastSeqCalls counts LastSeq reads so tests can prove a render is O(rows)
	// and, in the budget test, that no accessor is called with a path.
	lastSeqCalls int
}

func newFakeIndex() *fakeIndex {
	return &fakeIndex{
		seq:       41207,
		lastTS:    "2026-09-16T09:14:03.221Z",
		incidents: map[string]types.Incident{},
		records:   map[string][]types.Record{},
		ages:      map[string]float64{},
	}
}

func (f *fakeIndex) LastSeq() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastSeqCalls++
	return f.seq
}

func (f *fakeIndex) LastRecordTS() string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.lastTS
}

func (f *fakeIndex) Counters(now time.Time) (int, int, float64) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	open := 0
	for _, inc := range f.incidents {
		if isOpenTestState(inc.State) {
			open++
		}
	}
	return open, len(f.groups), f.perMin
}

func (f *fakeIndex) OpenIncidents(limit int) []types.Incident {
	return f.openIncidents(0, limit)
}

func (f *fakeIndex) OpenIncidentsSince(seq uint64, limit int) []types.Incident {
	return f.openIncidents(seq, limit)
}

func (f *fakeIndex) openIncidents(since uint64, limit int) []types.Incident {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]types.Incident, 0, len(f.order))
	for _, id := range f.order {
		inc, ok := f.incidents[id]
		if !ok || !isOpenTestState(inc.State) {
			continue
		}
		if since > 0 && lastSeqOfRecords(f.records[id]) <= since {
			continue
		}
		out = append(out, inc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedTS > out[j].UpdatedTS })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (f *fakeIndex) GroupsSince(seq uint64, limit int) []types.GroupStat {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]types.GroupStat, 0, len(f.groups))
	for _, g := range f.groups {
		if g.Count == 0 {
			continue
		}
		out = append(out, g)
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (f *fakeIndex) RecordsForIncident(inc string, since uint64, limit int) []types.Record {
	f.mu.RLock()
	defer f.mu.RUnlock()
	src := f.records[inc]
	out := make([]types.Record, 0, len(src))
	for _, r := range src {
		if r.Seq <= since {
			continue
		}
		out = append(out, r)
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (f *fakeIndex) LastEventAge(sig string, now time.Time) (float64, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	age, ok := f.ages[sig]
	return age, ok
}

// trigger appends an incident record and advances the seq (the AC-19
// "real trigger" for the live test).
func (f *fakeIndex) trigger(inc types.Incident, rec types.Record) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	rec.Seq = f.seq
	rec.Inc = inc.ID
	f.records[inc.ID] = append(f.records[inc.ID], rec)
	f.lastTS = rec.TS
	if inc.UpdatedTS == "" {
		inc.UpdatedTS = rec.TS
	}
	f.incidents[inc.ID] = inc
	found := false
	for _, id := range f.order {
		if id == inc.ID {
			found = true
		}
	}
	if !found {
		f.order = append(f.order, inc.ID)
	}
}

func (f *fakeIndex) addRecord(inc string, rec types.Record) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	rec.Seq = f.seq
	f.records[inc] = append(f.records[inc], rec)
}

func (f *fakeIndex) addGroup(g types.GroupStat) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.groups = append(f.groups, g)
}

func (f *fakeIndex) setSeq(seq uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq = seq
}

// seqNow reads the current seq without counting the accessor call (tests that
// need the CAS pair the fragment would carry).
func (f *fakeIndex) seqNow() uint64 {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.seq
}

func (f *fakeIndex) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastSeqCalls
}

func lastSeqOfRecords(recs []types.Record) uint64 {
	var max uint64
	for _, r := range recs {
		if r.Seq > max {
			max = r.Seq
		}
	}
	return max
}

func isOpenTestState(s types.LadderState) bool {
	switch s {
	case types.StResolved, types.StQuarantined, types.StSuppressed:
		return false
	}
	return true
}

// ---------------------------------------------------------------- lookup

type fakeLookup struct {
	mu        sync.RWMutex
	incidents map[string]types.Incident
	groups    map[string]types.Group
	evidence  map[string]types.Evidence
}

func newFakeLookup() *fakeLookup {
	return &fakeLookup{
		incidents: map[string]types.Incident{},
		groups:    map[string]types.Group{},
		evidence:  map[string]types.Evidence{},
	}
}

func (l *fakeLookup) Incident(id string) (*types.Incident, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	inc, ok := l.incidents[id]
	if !ok {
		return nil, false
	}
	out := inc
	return &out, true
}

func (l *fakeLookup) Group(id string) (*types.Group, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	g, ok := l.groups[id]
	if !ok {
		return nil, false
	}
	out := g
	return &out, true
}

func (l *fakeLookup) Evidence(inc string) (*types.Evidence, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	ev, ok := l.evidence[inc]
	if !ok {
		return nil, false
	}
	out := ev
	return &out, true
}

func (l *fakeLookup) putIncident(inc types.Incident) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.incidents[inc.ID] = inc
}

func (l *fakeLookup) putGroup(g types.Group) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.groups[g.ID] = g
}

// ---------------------------------------------------------------- write seams

// seamError is the action error shape the §6.1 mapping consumes (the ladder's
// own *ladder.Error is adapted onto this by the composition root).
type seamError struct {
	Code types.ErrorCode
	Msg  string
}

func (e *seamError) Error() string              { return e.Msg }
func (e *seamError) ErrorCode() types.ErrorCode { return e.Code }

type fakeActions struct {
	mu       sync.Mutex
	acks     []ackCall
	closes   []closeCall
	ackErr   error
	closeErr error
	// lookup, when set, receives the ladder's state effect (a close resolves
	// the incident) so the CAS tests exercise a real state move.
	lookup *fakeLookup
}

type ackCall struct {
	Inc    string
	Actor  types.Actor
	Reason string
	Until  types.Duration
}

type closeCall struct {
	Inc        string
	Actor      types.Actor
	Reason     string
	Resolution string
}

func (a *fakeActions) Ack(ctx context.Context, inc string, actor types.Actor, reason string, until types.Duration) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.acks = append(a.acks, ackCall{Inc: inc, Actor: actor, Reason: reason, Until: until})
	if a.ackErr != nil {
		return a.ackErr
	}
	if a.lookup != nil {
		if cur, ok := a.lookup.Incident(inc); ok {
			cur.UpdatedTS = types.FormatUTC(time.Now())
			a.lookup.putIncident(*cur)
		}
	}
	return nil
}

func (a *fakeActions) Close(ctx context.Context, inc string, actor types.Actor, reason, resolution string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closes = append(a.closes, closeCall{Inc: inc, Actor: actor, Reason: reason, Resolution: resolution})
	if a.closeErr != nil {
		return a.closeErr
	}
	if a.lookup != nil {
		if cur, ok := a.lookup.Incident(inc); ok {
			cur.State = types.StResolved
			cur.ResolvedTS = types.FormatUTC(time.Now())
			a.lookup.putIncident(*cur)
		}
	}
	return nil
}

func (a *fakeActions) ackCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.acks)
}

func (a *fakeActions) lastAck() (ackCall, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.acks) == 0 {
		return ackCall{}, false
	}
	return a.acks[len(a.acks)-1], true
}

type fakeAutonomy struct {
	mu     sync.Mutex
	gates  types.AutonomyGates
	calls  []types.AutonomyGates
	actors []types.Actor
	err    error
}

func newFakeAutonomy() *fakeAutonomy {
	return &fakeAutonomy{gates: types.DefaultGates()}
}

func (a *fakeAutonomy) Gates() types.AutonomyGates {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.gates
}

func (a *fakeAutonomy) SetAutonomy(ctx context.Context, gates types.AutonomyGates, actor types.Actor) (types.AutonomyGates, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return a.gates, a.err
	}
	a.gates = gates
	a.calls = append(a.calls, gates)
	a.actors = append(a.actors, actor)
	return gates, nil
}

func (a *fakeAutonomy) writes() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

// ---------------------------------------------------------------- tokens

// tokenFor returns a deterministic plaintext/hash pair: base64url of 32
// repeated bytes is exactly the 43 characters §3.2 requires.
func tokenFor(b byte) (plain, hash string) {
	raw := bytes.Repeat([]byte{b}, 32)
	plain = tokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	return plain, hashOf(plain)
}

const (
	labelRead  = "dash-read@phone"
	labelWrite = "dash-write@laptop"
	labelAuto  = "dash-auto@host"
)

func writeTokenFile(t *testing.T, dir string, toks ...types.Token) string {
	t.Helper()
	path := filepath.Join(dir, "dashboard-tokens.json")
	b, err := json.Marshal(tokenFile{Version: 1, Tokens: toks})
	if err != nil {
		t.Fatalf("marshal token file: %v", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	return path
}

func tokenEntryFor(label string, b byte, scopes ...types.Scope) types.Token {
	_, hash := tokenFor(b)
	return types.Token{
		ID:        label,
		Hash:      hash,
		Scopes:    scopes,
		CreatedTS: "2026-09-16T09:00:00.000Z",
	}
}

// ---------------------------------------------------------------- logging

// testLogger returns a logger over an in-memory sink plus that sink, so a test
// can assert a boot-time WARN without a real process log (TRBL-006: an empty
// token store must say so at boot).
func testLogger() (*slog.Logger, *bytes.Buffer) {
	buf := new(bytes.Buffer)
	return slog.New(slog.NewTextHandler(buf, nil)), buf
}

// logLines returns the sink's non-empty lines (each line one log record).
func logLines(buf *bytes.Buffer) []string {
	raw := strings.TrimRight(buf.String(), "\n")
	if raw == "" {
		return nil
	}
	return strings.Split(raw, "\n")
}

// ---------------------------------------------------------------- environment

// testEnv is one running dashboard: the real handler over httptest, plus every
// fake the assertions reach into.
type testEnv struct {
	t   *testing.T
	srv *httptest.Server
	s   *server

	idx      *fakeIndex
	lookup   *fakeLookup
	actions  *fakeActions
	autonomy *fakeAutonomy
	clock    *fakeClock

	readPlain, writePlain, autoPlain string
	tokenFile                        string
	cfg                              Config
}

// envOptions tunes the environment for one test.
type envOptions struct {
	cfg       func(*Config)
	deps      func(*Deps)
	tokens    []types.Token
	noRefresh bool // reuse the same index fixtures without the default ones
}

func newEnv(t *testing.T, opt envOptions) *testEnv {
	t.Helper()
	dir := t.TempDir()
	clock := newFakeClock()

	readPlain, _ := tokenFor(1)
	writePlain, _ := tokenFor(2)
	autoPlain, _ := tokenFor(3)

	toks := opt.tokens
	if toks == nil {
		toks = []types.Token{
			tokenEntryFor(labelRead, 1, types.ScopeRead),
			tokenEntryFor(labelWrite, 2, types.ScopeWrite),
			tokenEntryFor(labelAuto, 3, types.ScopeAutonomy),
		}
	}
	tokenFile := writeTokenFile(t, dir, toks...)

	cfg := DefaultConfig()
	cfg.TokenFile = tokenFile
	cfg.Bind = "127.0.0.1"

	idx := newFakeIndex()
	idx.perMin = 18.4
	lookup := newFakeLookup()
	actions := &fakeActions{}
	autonomy := newFakeAutonomy()

	if !opt.noRefresh {
		seedFixtures(idx, lookup)
	}

	deps := Deps{
		Index:    idx,
		Lookup:   lookup,
		Story:    storyFixture(lookup),
		Health:   healthFixture(autonomy),
		Actions:  actions,
		Autonomy: autonomy,
		Rules:    ruleFixtures(),
		RuleStats: func(name string) (string, uint64, uint64) {
			switch name {
			case "io-pressure":
				return clock.Now().Add(-3 * time.Minute).UTC().Format(types.TsLayout), 12, 4
			}
			return "", 0, 0
		},
		Sensors: func() []types.SensorHealth {
			return []types.SensorHealth{
				{Sensor: types.SenPSI, Enabled: true, LastSuccessTS: "2026-09-16T09:14:03.000Z", LastEventTS: "2026-09-16T09:14:03.221Z", EventsTotal: 912},
				{Sensor: types.SenJournald, Enabled: true, EventsTotal: 0},
			}
		},
		Breakers: func() []types.Breaker {
			return []types.Breaker{
				{Scope: "rule:io-pressure", State: types.BreakerOpen, OpenUntil: "2026-09-16T10:00:00.000Z", Trips: 3, Reason: "flapping"},
			}
		},
		Sources: func() []types.SourceLiveness {
			return []types.SourceLiveness{
				{HostID: "7f3a91c2d4e5b607", Source: "sentinel:payment-worker", Zone: "loopback", Expected: true, Alive: true, LastEventTS: "2026-09-16T09:14:03.221Z", LastEventAgeS: 0.2, MaxAgeS: 300},
			}
		},
		Watermarks: func() types.RuntimeWatermarks {
			// The open counters come from the same index the page's header
			// counters read, exactly as the composition root's watermarks()
			// does (internal/app/dashdeps.go): a fixture that disagreed with
			// the index would hide the first-paint defect TRBL-010 fixed
			// rather than reproduce it.
			inc, groups, perMin := idx.Counters(clock.Now())
			return types.RuntimeWatermarks{
				BinaryBytes: 10 << 20, RSSBytes: 41 << 20, MemHighBytes: 192 << 20,
				LedgerBytes: 30 << 20, SpoolBytes: 0, EventsPerMin: perMin,
				GroupsOpen: groups, IncidentsOpen: inc, Worktrees: 1,
			}
		},
		Version: func() (string, string, string, bool) {
			return "0.1.0", "9c1f0ab", "2026-09-16T09:00:00.000Z", false
		},
		Clock:     clock.Now,
		TokenFile: tokenFile,
	}
	if opt.cfg != nil {
		opt.cfg(&cfg)
	}
	if opt.deps != nil {
		opt.deps(&deps)
	}
	deps.Config = cfg
	deps.Clock = clock.Now
	deps.TokenFile = cfg.TokenFile

	actions.lookup = lookup
	s, err := newServer(cfg, deps)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)

	return &testEnv{
		t: t, srv: ts, s: s, idx: idx, lookup: lookup, actions: actions,
		autonomy: autonomy, clock: clock,
		readPlain: readPlain, writePlain: writePlain, autoPlain: autoPlain,
		tokenFile: tokenFile, cfg: cfg,
	}
}

// unlimitedRates removes the §2.8 buckets for tests that intentionally make
// more than a burst of requests (timing and budget rows). The limit logic
// itself is covered by auth_test's throttle cases.
func unlimitedRates(cfg *Config) {
	cfg.ReadRPS = 1e9
	cfg.ReadBurst = 1_000_000
	cfg.WriteRPS = 1e9
	cfg.WriteBurst = 1_000_000
	cfg.AuthFailLimit = 1_000_000
}

// seedFixtures fills the shared index/lookup with the AC-16 entity classes:
// incidents, groups, an issue ref + board row + promotion for the story panel,
// breakers, rules.
func seedFixtures(idx *fakeIndex, lookup *fakeLookup) {
	// Start three records below the §3.4 example seq so the fixtures' CAS pair
	// (ledger_seq 41207, the same literal the spec's example uses) holds.
	idx.setSeq(41204)
	inc1 := types.Incident{
		ID: "inc_01J9F0000000000000000000AB", Sig: "sentinel:sha256v1:9f2c1d3e4b5a6c7d",
		GroupID: "grp_01J9F0000000000000000000AB", State: types.StVerifying,
		EntryRung: types.RungPlay, Rung: types.RungAgent, Severity: types.SevHigh,
		OpenedTS: "2026-09-16T09:04:03.221Z", UpdatedTS: "2026-09-16T09:14:03.221Z",
		VerifyWin: "10m", PlayRuns: 2, AgentRuns: 1, IssueID: "iss_01J9F0000000000000000000AB",
		TaskID: "tsk_01J9F00000000000000000000A",
	}
	inc2 := types.Incident{
		ID: "inc_01J9F0000000000000000000AC", Sig: "journald:sha256v1:2ab4c6d8e0f1a3b5",
		GroupID: "grp_01J9F0000000000000000000AC", State: types.StPlayApplied,
		EntryRung: types.RungPlay, Rung: types.RungPlay, Severity: types.SevMedium,
		OpenedTS: "2026-09-16T08:44:03.221Z", UpdatedTS: "2026-09-16T09:10:00.000Z",
	}
	idx.trigger(inc1, types.Record{
		Seq: 0, RecID: "rec_01", TS: "2026-09-16T09:14:03.221Z", Kind: types.KIncident,
		Inc: inc1.ID, Actor: types.Actor{Kind: types.ActorDaemon, ID: "troubled"},
		Payload: map[string]any{"transition": "verifying", "reason": "play applied", "via": "ladder"},
	})
	idx.trigger(inc2, types.Record{
		Seq: 0, RecID: "rec_02", TS: "2026-09-16T09:10:00.000Z", Kind: types.KToolCall,
		Inc: inc2.ID, Actor: types.Actor{Kind: types.ActorAgent, ID: "agent@hostA"},
		Payload: map[string]any{"module": "config.set", "check_mode": true},
	})
	idx.addRecord(inc1.ID, types.Record{
		Seq: 0, RecID: "rec_03", TS: "2026-09-16T09:12:00.000Z", Kind: types.KToolCall,
		Inc: inc1.ID, Actor: types.Actor{Kind: types.ActorHuman, ID: labelWrite},
		Payload: map[string]any{"transition": "acknowledged", "reason": "on it", "via": "dashboard"},
	})
	idx.addGroup(types.GroupStat{
		GroupID: "grp_01J9F0000000000000000000AB", Sig: inc1.Sig, Digest: "9f2c1d3e4b5a6c7d",
		Source: "sentinel", Title: "payment worker queue wedge", Count: 42, Rate1m: 3.5,
		FirstSeenTS: "2026-09-16T08:00:00.000Z", LastSeenTS: "2026-09-16T09:14:03.221Z",
		IncidentID: inc1.ID,
	})
	idx.addGroup(types.GroupStat{
		GroupID: "grp_01J9F0000000000000000000AC", Sig: inc2.Sig, Digest: "2ab4c6d8e0f1a3b5",
		Source: "journald", Title: "service reload loop", Count: 7, Rate1m: 0.4,
		FirstSeenTS: "2026-09-16T08:30:00.000Z", LastSeenTS: "2026-09-16T09:10:00.000Z",
	})
	idx.ages[inc1.Sig] = 0.2
	idx.ages[inc2.Sig] = 240

	lookup.putIncident(inc1)
	lookup.putIncident(inc2)
	lookup.putGroup(types.Group{
		ID: "grp_01J9F0000000000000000000AB", Sig: inc1.Sig, Digest: "9f2c1d3e4b5a6c7d",
		Source: "sentinel", Title: "payment worker queue wedge",
		FirstSeenTS: "2026-09-16T08:00:00.000Z", LastSeenTS: "2026-09-16T09:14:03.221Z",
		Count: 42, ReleaseRange: []string{"1.4.0", "1.5.0"}, IncidentID: inc1.ID,
		Counters: types.GroupCounters{Events: 42, Suppressed: 5, Redacted: 2, Dropped: 0, SampleRate: 1},
	})
	lookup.mu.Lock()
	lookup.evidence[inc1.ID] = types.Evidence{
		TSWindowStart: "2026-09-16T09:14:03.000Z", TSWindowEnd: "2026-09-16T09:24:03.000Z",
		WindowS: 600, EventsObserved: 12, CanarySeen: true, CanaryID: "can_01",
		Zone: "loopback", Result: types.VerifyPassed,
	}
	lookup.mu.Unlock()
}

func storyFixture(lookup *fakeLookup) func(string) Story {
	return func(inc string) Story {
		return Story{
			IssueRef:   "iss_01J9F0000000000000000000AB https://github.com/example/trouble/issues/7",
			BoardRow:   "tsk_01J9F00000000000000000000A",
			ResearchID: "res_01J9F0000000000000000000AB",
			Research:   "queue-wedge class brief returned",
			SpawnID:    "sp_01J9F0000000000000000000AB",
			Promotion:  "promoted PR https://github.com/example/trouble/pull/12",
			Candidate:  "sk_01J9F0000000000000000000AB reviewed",
		}
	}
}

func healthFixture(autonomy *fakeAutonomy) func(context.Context) types.HealthResponse {
	return func(ctx context.Context) types.HealthResponse {
		return types.HealthResponse{
			Status: "ok", Version: "0.1.0", GitSHA: "9c1f0ab", BuildTime: "2026-09-16T09:00:00.000Z",
			UptimeS: 3881.4, LedgerLastSeq: 41207, LedgerLastTS: "2026-09-16T09:14:03.221Z",
			LedgerStallS: 1.2, Autonomy: autonomy.Gates(),
			RW: types.RuntimeWatermarks{BinaryBytes: 10 << 20, RSSBytes: 41 << 20, EventsPerMin: 18.4, GroupsOpen: 7, IncidentsOpen: 2},
		}
	}
}

func ruleFixtures() []types.Rule {
	return []types.Rule{
		{Name: "io-pressure", Enabled: true, Source: types.SrcPSI, EntryRung: types.RungPlay, Severity: types.SevHigh, VerifyWin: "10m"},
		{Name: "journal-loop", Enabled: true, Source: types.SrcJournald, EntryRung: types.RungRecord, Severity: types.SevLow, VerifyWin: "5m"},
	}
}

// ---------------------------------------------------------------- requests

// req builds a request against the running test server with optional header
// and cookie mutation.
func (e *testEnv) req(method, path string, body []byte, mutate func(*http.Request)) *http.Request {
	e.t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	} else {
		rdr = bytes.NewReader(nil)
	}
	r, err := http.NewRequest(method, e.srv.URL+path, rdr)
	if err != nil {
		e.t.Fatalf("new request: %v", err)
	}
	if mutate != nil {
		mutate(r)
	}
	return r
}

// do runs a request and returns the response with the body already read.
func (e *testEnv) do(r *http.Request) (*http.Response, string) {
	e.t.Helper()
	resp, err := e.srv.Client().Do(r)
	if err != nil {
		e.t.Fatalf("do %s %s: %v", r.Method, r.URL.String(), err)
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		e.t.Fatalf("read body: %v", err)
	}
	return resp, buf.String()
}

// bearer sets the Authorization header.
func bearer(token string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
}

// withCookies attaches cookies to a request.
func withCookies(cs ...*http.Cookie) func(*http.Request) {
	return func(r *http.Request) {
		for _, c := range cs {
			r.AddCookie(c)
		}
	}
}

// jsonBody sets a JSON content type and marks the body.
func jsonBody(r *http.Request) { r.Header.Set("Content-Type", "application/json") }

// get is the common authenticated GET.
func (e *testEnv) get(path, token string, mutate ...func(*http.Request)) (*http.Response, string) {
	e.t.Helper()
	return e.do(e.req(http.MethodGet, path, nil, func(r *http.Request) {
		if token != "" {
			bearer(token)(r)
		}
		for _, m := range mutate {
			m(r)
		}
	}))
}

// post is the common authenticated POST (JSON content type by default).
func (e *testEnv) post(path, token string, body string, mutate ...func(*http.Request)) (*http.Response, string) {
	e.t.Helper()
	return e.do(e.req(http.MethodPost, path, []byte(body), func(r *http.Request) {
		r.Header.Set("Content-Type", "application/json")
		if token != "" {
			bearer(token)(r)
		}
		for _, m := range mutate {
			m(r)
		}
	}))
}

// decodeError unmarshals an error body.
func decodeError(t *testing.T, body string) errorBody {
	t.Helper()
	var eb errorBody
	if err := json.Unmarshal([]byte(body), &eb); err != nil {
		t.Fatalf("error body is not JSON (%v): %.200s", err, body)
	}
	return eb
}

// wantStatus asserts the status and returns the body.
func wantStatus(t *testing.T, resp *http.Response, body string, want int) string {
	t.Helper()
	if resp.StatusCode != want {
		t.Fatalf("status = %d, want %d (body: %.300s)", resp.StatusCode, want, body)
	}
	return body
}

// validIncidentID returns a well-formed fixture incident id.
func validIncidentID() string { return "inc_01J9F0000000000000000000AB" }

func validGroupID() string { return "grp_01J9F0000000000000000000AB" }

// csrfFor derives the double-submit value for a token label at the env clock.
func (e *testEnv) csrfFor(label string) string {
	return e.s.csrf.value(label, e.clock.Now()).Value
}

// htmlRequest marks a request as a browser HTML request.
func htmlRequest(r *http.Request) {
	r.Header.Set("Accept", "text/html,application/xhtml+xml")
}

func fmtBytes(n int) string { return fmt.Sprintf("%d bytes", n) }

var _ = strings.TrimSpace
