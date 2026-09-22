package ladder

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// testutil_test.go holds the fakes the ladder's tests share. Every collaborator
// is in-process: no test opens a socket, waits on a real clock or touches git.

type ledgerRec struct {
	Kind    types.RecordKind
	Sig     string
	Inc     string
	Payload map[string]any
}

type fakeLedger struct {
	mu       sync.Mutex
	records  []ledgerRec
	seq      uint64
	flushes  int
	failNext error
}

func (f *fakeLedger) Append(ctx context.Context, kind types.RecordKind, sig, inc string, payload map[string]any) (types.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil
		return types.Record{}, err
	}
	f.seq++
	cp := map[string]any{}
	for k, v := range payload {
		cp[k] = v
	}
	f.records = append(f.records, ledgerRec{Kind: kind, Sig: sig, Inc: inc, Payload: cp})
	return types.Record{Seq: f.seq, RecID: types.NewID(types.PEv), Kind: kind, Sig: sig, Inc: inc, Payload: cp}, nil
}

func (f *fakeLedger) Flush(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flushes++
	return nil
}

// last returns the newest record (nil when none).
func (f *fakeLedger) last() *ledgerRec {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.records) == 0 {
		return nil
	}
	r := f.records[len(f.records)-1]
	return &r
}

// byKind returns every record of a kind in order.
func (f *fakeLedger) byKind(kind types.RecordKind) []ledgerRec {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ledgerRec
	for _, r := range f.records {
		if r.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}

// countOf counts records whose payload carries a given key/value pair.
func (f *fakeLedger) countPayload(key string, want any) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.records {
		if got, ok := r.Payload[key]; ok && fmt.Sprint(got) == fmt.Sprint(want) {
			n++
		}
	}
	return n
}

type fakeIndex struct {
	mu            sync.Mutex
	openBySig     map[string]types.Incident
	openByInKey   map[string]types.Incident
	resolvedBySig map[string]types.Incident
	events        map[string]int
	counters      map[string]int64
	gaps          []types.GapRecord
	seq           uint64
}

func newFakeIndex() *fakeIndex {
	return &fakeIndex{
		openBySig:     map[string]types.Incident{},
		openByInKey:   map[string]types.Incident{},
		resolvedBySig: map[string]types.Incident{},
		events:        map[string]int{},
		counters:      map[string]int64{},
	}
}

func (f *fakeIndex) OpenIncidentBySig(ctx context.Context, sig string) (types.Incident, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	inc, ok := f.openBySig[sig]
	return inc, ok, nil
}

func (f *fakeIndex) OpenIncidentByInKey(ctx context.Context, inKey string) (types.Incident, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	inc, ok := f.openByInKey[inKey]
	return inc, ok, nil
}

func (f *fakeIndex) ResolvedIncidentBySig(ctx context.Context, sig string, window types.Duration) (types.Incident, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	inc, ok := f.resolvedBySig[sig]
	return inc, ok, nil
}

func (f *fakeIndex) EventsInWindow(ctx context.Context, sig string, from, to string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.events[sig], nil
}

func (f *fakeIndex) CountersInWindow(ctx context.Context, from, to string) (map[string]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]int64{}
	for k, v := range f.counters {
		out[k] = v
	}
	return out, nil
}

func (f *fakeIndex) GapsInWindow(ctx context.Context, from, to string) ([]types.GapRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]types.GapRecord(nil), f.gaps...)
	return out, nil
}

func (f *fakeIndex) Seq() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seq
}

type fakePlay struct {
	mu         sync.Mutex
	runs       int
	checkCalls int
	applyCalls int
	rollbacks  int
	summary    RunSummary
	failWith   error
}

func (f *fakePlay) Run(ctx context.Context, inc types.Incident, p types.Play, mode string) (RunSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs++
	if f.failWith != nil {
		return RunSummary{}, f.failWith
	}
	sum := f.summary
	sum.PlayRun = f.runs
	return sum, nil
}

func (f *fakePlay) Check(ctx context.Context, tool string, args map[string]any) (types.Diff, types.ToolCall, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkCalls++
	return types.Diff{}, types.ToolCall{Module: tool}, nil
}

func (f *fakePlay) Apply(ctx context.Context, tool string, args map[string]any) (types.Result, types.ToolCall, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applyCalls++
	return types.Result{Changed: true}, types.ToolCall{Module: tool}, nil
}

func (f *fakePlay) Rollback(ctx context.Context, t types.ToolCall) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rollbacks++
	return nil
}

func (f *fakePlay) counts() (runs, checks, applies, rollbacks int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs, f.checkCalls, f.applyCalls, f.rollbacks
}

type fakeResearch struct {
	mu       sync.Mutex
	requests int
	outcome  types.ResearchOutcome
	err      error
	last     *Subject
}

func (f *fakeResearch) Request(ctx context.Context, inc types.Incident, sub Subject) (types.ResearchOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	cp := sub
	f.last = &cp
	if f.err != nil {
		return types.ResearchOutcome{}, f.err
	}
	out := f.outcome
	if out.Inc == "" {
		out.Inc = inc.ID
	}
	return out, nil
}

func (f *fakeResearch) Poll(ctx context.Context, resID string) (types.ResearchOutcome, error) {
	return f.outcome, nil
}

// lastSubject snapshots the most recent Request call's Subject (the AC-31
// copy-out join asserts against it). Nil when nothing was captured.
func (f *fakeResearch) lastSubject() *Subject {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.last == nil {
		return nil
	}
	cp := *f.last
	return &cp
}

type fakeOutlets struct {
	mu       sync.Mutex
	issues   []string
	boards   []string
	comments []string
	promoted []string
	hotfixes []string
	closed   []string
}

func (f *fakeOutlets) EnsureIssue(ctx context.Context, inc types.Incident) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// sig-keyed: one issue per incident, never a second.
	for _, i := range f.issues {
		if i == inc.ID {
			return inc.IssueID, nil
		}
	}
	f.issues = append(f.issues, inc.ID)
	return "iss_" + inc.ID, nil
}

func (f *fakeOutlets) EnsureBoardRow(ctx context.Context, inc types.Incident) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, i := range f.boards {
		if i == inc.ID {
			return inc.TaskID, nil
		}
	}
	f.boards = append(f.boards, inc.ID)
	return "tsk_" + inc.ID, nil
}

func (f *fakeOutlets) Comment(ctx context.Context, inc types.Incident, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments = append(f.comments, body)
	return nil
}

func (f *fakeOutlets) RequestHotfix(ctx context.Context, inc types.Incident) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hotfixes = append(f.hotfixes, inc.ID)
	return "sp_" + inc.ID, nil
}

func (f *fakeOutlets) Promote(ctx context.Context, inc types.Incident, ev types.Evidence) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.promoted = append(f.promoted, inc.ID)
	return "pr_" + inc.ID, nil
}

func (f *fakeOutlets) CloseOutlets(ctx context.Context, inc types.Incident, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, reason)
	return nil
}

func (f *fakeOutlets) counts() (issues, boards, comments int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.issues), len(f.boards), len(f.comments)
}

type fakeNotifier struct {
	mu    sync.Mutex
	class []string
}

func (f *fakeNotifier) Emit(ctx context.Context, inc types.Incident, class string, detail map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.class = append(f.class, class)
	return nil
}

func (f *fakeNotifier) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.class)
}

// fakeClock is a controllable clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Monotonic() time.Duration { return 0 }

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeEval is the rule evaluator (the real one is internal/sensors' condition
// language; the ladder only needs the two answers).
type fakeEval struct {
	Matched bool
	Stab    bool
}

func (f fakeEval) Match(r types.Rule, ev Observation) bool { return f.Matched }
func (f fakeEval) Stabilized(r types.Rule, s StabilizationState, now time.Time) bool {
	return f.Stab
}

// harness bundles the ladder and its fakes.
type harness struct {
	l        *Ladder
	ledger   *fakeLedger
	index    *fakeIndex
	play     *fakePlay
	research *fakeResearch
	outlets  *fakeOutlets
	notify   *fakeNotifier
	clock    *fakeClock
	rules    map[string]types.Rule
	plays    map[string]types.Play
}

type harnessOpts struct {
	cfg      Config
	research bool
	playFor  map[string]types.Play
	rules    map[string]types.Rule
	agent    AgentPort    // SPEC-05 §2a; nil = the stage refuses
	skills   SkillLibrary // SPEC-05 §2b; nil = no library is read
}

func newHarness(t *testing.T, opts harnessOpts) *harness {
	t.Helper()
	h := &harness{
		ledger:   &fakeLedger{},
		index:    newFakeIndex(),
		play:     &fakePlay{},
		research: &fakeResearch{},
		outlets:  &fakeOutlets{},
		notify:   &fakeNotifier{},
		clock:    newClock(),
		rules:    opts.rules,
		plays:    opts.playFor,
	}
	deps := Deps{
		Ledger: h.ledger,
		Index:  h.index,
		Clock:  h.clock,
		Eval:   fakeEval{Matched: true, Stab: true},
		Cfg:    opts.cfg,
		Agent:  opts.agent,
		Skills: opts.skills,
	}
	if h.rules != nil {
		deps.Rules = func(name string) (types.Rule, bool) {
			r, ok := h.rules[name]
			return r, ok
		}
	}
	if h.plays != nil {
		deps.PlayFor = func(rule string) (types.Play, bool) {
			p, ok := h.plays[rule]
			return p, ok
		}
	}
	if opts.research {
		deps.Research = h.research
	}
	deps.Registry = h.play
	deps.Outlets = h.outlets
	deps.Notify = h.notify
	l, err := New(deps)
	if err != nil {
		t.Fatalf("ladder.New: %v", err)
	}
	h.l = l
	return h
}

// admit is the shared admission helper.
func (h *harness) admit(t *testing.T, obs Observation) AdmitResult {
	t.Helper()
	if obs.Sig.String() == "" {
		obs.Sig = types.NewSigFromFields(types.SrcJournald, "unit", "payload.service", "msg")
	}
	if obs.TS == "" {
		obs.TS = types.FormatUTC(h.clock.Now())
	}
	res, err := h.l.Admit(context.Background(), obs)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if res.Inc != "" {
		h.index.mu.Lock()
		h.index.openBySig[obs.Sig.String()] = types.Incident{
			ID: res.Inc, Sig: obs.Sig.String(), State: types.StRecorded,
			EntryRung: types.RungPlay, Rung: types.RungPlay, Severity: obs.Severity,
		}
		h.index.mu.Unlock()
	}
	return res
}

// obs builds an observation with sane defaults.
func obsFor(sig types.Sig, rule string, src SourcePath) Observation {
	return Observation{
		EventID:  types.NewID(types.PEv),
		TS:       types.FormatUTC(time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)),
		Sig:      sig,
		Rule:     rule,
		Source:   src,
		Subject:  "payment-worker.service",
		Severity: types.SevHigh,
		Detail:   map[string]any{"unit": "payment-worker.service"},
	}
}

// canaryIn lands a canary for a key at the clock's current time.
func (h *harness) canaryIn(t *testing.T, key string) {
	t.Helper()
	if err := h.l.CanaryObserved(context.Background(), key, "can_test", "sentinel"); err != nil {
		t.Fatalf("CanaryObserved: %v", err)
	}
}

// payloadKeys returns the sorted keys of a payload.
func payloadKeys(p map[string]any) []string {
	out := make([]string, 0, len(p))
	for k := range p {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// incidentPayload returns the payload of the newest incident record.
func (h *harness) incidentPayload() map[string]any {
	for i := len(h.ledger.records) - 1; i >= 0; i-- {
		if h.ledger.records[i].Kind == types.KIncident {
			return h.ledger.records[i].Payload
		}
	}
	return nil
}

// ---- scenario helpers ----

// sigFor is a stable sig per test name so vectors never collide.
func sigFor(name string) types.Sig {
	return types.NewSigFromFields(types.SrcJournald, "unit", "payload-"+name, "msg-"+name)
}

// playChecked drives an incident to play:check_only and returns its state.
func playChecked(t *testing.T, h *harness, name string) (string, *incState) {
	t.Helper()
	res := h.admit(t, obsFor(sigFor(name), "rule-a", "journald"))
	if _, err := h.l.Advance(context.Background(), res.Inc, Transition{Trigger: "T03"}); err != nil {
		t.Fatalf("%s: T03: %v", name, err)
	}
	if _, err := h.l.Advance(context.Background(), res.Inc, Transition{Trigger: "T08"}); err != nil {
		t.Fatalf("%s: T08: %v", name, err)
	}
	return res.Inc, h.l.incStateFor(res.Inc)
}

// playFailed drives an incident to play:failed.
func playFailed(t *testing.T, h *harness, name string) (string, *incState) {
	t.Helper()
	inc, st := playChecked(t, h, name)
	st.RunSummary = &RunSummary{FailClass: "permanent", TasksFailed: 1}
	if _, err := h.l.Advance(context.Background(), inc, Transition{Trigger: "T11"}); err != nil {
		t.Fatalf("%s: T11: %v", name, err)
	}
	return inc, st
}

// researchRequested drives an incident to research:requested.
func researchRequested(t *testing.T, h *harness, name string) (string, *incState) {
	t.Helper()
	res := h.admit(t, obsFor(sigFor(name), "rule-agent", "journald"))
	if _, err := h.l.Advance(context.Background(), res.Inc, Transition{Trigger: "T04"}); err != nil {
		t.Fatalf("%s: T04: %v", name, err)
	}
	return res.Inc, h.l.incStateFor(res.Inc)
}

// researchSkipped drives an incident to research:skipped.
func researchSkipped(t *testing.T, h *harness, name string) (string, *incState) {
	t.Helper()
	inc, st := playFailed(t, h, name)
	st.RunSummary = &RunSummary{FailClass: "transient"}
	st.PlayRuns = st.EffectiveMaxRuns
	if _, err := h.l.Advance(context.Background(), inc, Transition{Trigger: "T15"}); err != nil {
		t.Fatalf("%s: T15: %v", name, err)
	}
	return inc, st
}

// agentRunning drives an incident to agent:running.
func agentRunning(t *testing.T, h *harness, name string) string {
	t.Helper()
	res := h.admit(t, obsFor(sigFor(name), "rule-agent", "journald"))
	if _, err := h.l.Advance(context.Background(), res.Inc, Transition{Trigger: "T05"}); err != nil {
		t.Fatalf("%s: T05: %v", name, err)
	}
	return res.Inc
}

// agentDone drives an incident to agent:done.
func agentDone(t *testing.T, h *harness, name string) string {
	t.Helper()
	inc := agentRunning(t, h, name)
	h.l.incStateFor(inc).AgentResult = "done"
	if _, err := h.l.Advance(context.Background(), inc, Transition{Trigger: "T27"}); err != nil {
		t.Fatalf("%s: T27: %v", name, err)
	}
	return inc
}

// suspended drives an incident to agent:suspended.
func suspended(t *testing.T, h *harness, name string) (string, *incState) {
	t.Helper()
	inc := agentRunning(t, h, name)
	st := h.l.incStateFor(inc)
	st.AgentResult = "failed"
	st.Strikes = []time.Time{h.clock.Now(), h.clock.Now()}
	if _, err := h.l.Advance(context.Background(), inc, Transition{Trigger: "T29"}); err != nil {
		t.Fatalf("%s: T29: %v", name, err)
	}
	return inc, st
}

// verifying drives an incident to verifying.
func verifying(t *testing.T, h *harness, name string) (string, *incState) {
	t.Helper()
	inc, st := playChecked(t, h, name)
	st.RunSummary = &RunSummary{Changed: true, TasksRun: 1}
	if _, err := h.l.Advance(context.Background(), inc, Transition{Trigger: "T10"}); err != nil {
		t.Fatalf("%s: T10: %v", name, err)
	}
	if _, err := h.l.Advance(context.Background(), inc, Transition{Trigger: "T12"}); err != nil {
		t.Fatalf("%s: T12: %v", name, err)
	}
	return inc, st
}

// resolved drives an incident to resolved through a passing window.
func resolved(t *testing.T, h *harness, name string) (string, *incState) {
	t.Helper()
	inc, st := verifying(t, h, name)
	ev := passingEvidence(st)
	h.canaryIn(t, st.Inc.Sig)
	h.index.events[st.Inc.Sig] = 0
	st.Evidence = &ev
	if _, err := h.l.Advance(context.Background(), inc, Transition{Trigger: "T38", Evidence: &ev}); err != nil {
		t.Fatalf("%s: T38: %v", name, err)
	}
	return inc, st
}

// escalated drives an incident to escalated through the play ceiling.
func escalated(t *testing.T, h *harness, name string) (string, *incState) {
	t.Helper()
	inc, st := playFailed(t, h, name)
	st.RunSummary = &RunSummary{FailClass: "permanent"}
	if _, err := h.l.Advance(context.Background(), inc, Transition{Trigger: "T18"}); err != nil {
		t.Fatalf("%s: T18: %v", name, err)
	}
	return inc, st
}

// suppressedIncident drives an incident to suppressed.
func suppressedIncident(t *testing.T, h *harness, name string) (string, *incState) {
	t.Helper()
	sig := sigFor(name)
	res := h.admit(t, obsFor(sig, "rule-a", "journald"))
	if _, err := h.l.Suppress(context.Background(), sig.String(), "30m", reasonSuppressionWindow); err != nil {
		t.Fatalf("Suppress: %v", err)
	}
	if _, err := h.l.Advance(context.Background(), res.Inc, Transition{Trigger: "T46"}); err != nil {
		t.Fatalf("%s: T46: %v", name, err)
	}
	return res.Inc, h.l.incStateFor(res.Inc)
}

// passingEvidence builds a `passed` evidence tuple for an incident's window.
func passingEvidence(st *incState) types.Evidence {
	w := st.Window
	start, end, s := "", "", 600.0
	if w != nil {
		start, end = w.Start, w.End
		if w.S.Seconds() != 0 {
			s = w.S.Seconds()
		}
	}
	return types.Evidence{
		TSWindowStart:   start,
		TSWindowEnd:     end,
		WindowS:         s,
		EventsObserved:  0,
		CanarySeen:      true,
		CanaryID:        "can_test",
		CounterDeltas:   map[string]int64{},
		SourcesExpected: []string{},
		SourcesAlive:    []string{},
		SourcesQuiet:    []string{},
		SourcesMissing:  []string{},
		Zone:            "loopback",
		Result:          types.VerifyPassed,
	}
}
