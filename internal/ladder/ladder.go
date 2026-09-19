package ladder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// SourcePath is one of the four delivery paths a detection can arrive by
// (SPEC-05 §2): "sentinel", "collector", "journald", "dbus", "psi", "disk",
// "timers", "inotify".
type SourcePath string

// SourcePaths lists every legal value.
var SourcePaths = []SourcePath{"sentinel", "collector", "journald", "dbus", "psi", "disk", "timers", "inotify"}

// RunSummary is the play runner's report of one run (SPEC-05 §2).
type RunSummary struct {
	PlayRun     int               `json:"play_run"`
	TasksRun    int               `json:"tasks_run"`
	TasksFailed int               `json:"tasks_failed"`
	Changed     bool              `json:"changed"`
	Applied     []types.DiffEntry `json:"applied"`
	LastTool    types.ToolCall    `json:"last_tool"`
	FailClass   string            `json:"fail_class"`
	DiffSummary string            `json:"diff_summary"`
}

// StabilizationState tracks one rule's `for=` interval (SPEC-05 §2).
type StabilizationState struct {
	FirstQualifyingTS string `json:"first_qualifying_ts"`
	Resets            int    `json:"resets"`
	Degraded          bool   `json:"degraded"`
}

// Subject is the research request payload (SPEC-TYPES §3.15.9).
type Subject = types.Subject

// PlayRunner is the registry's consumer view (SPEC-06 §2 owns the 6-stage
// contract); internal/registry.Runner satisfies the last three methods and the
// composition root adds Run.
type PlayRunner interface {
	Run(ctx context.Context, inc types.Incident, p types.Play, mode string) (RunSummary, error)
	Check(ctx context.Context, tool string, args map[string]any) (types.Diff, types.ToolCall, error)
	Apply(ctx context.Context, tool string, args map[string]any) (types.Result, types.ToolCall, error)
	Rollback(ctx context.Context, t types.ToolCall) error
}

// RuleEvaluator is the shared condition language (default: internal/sensors).
type RuleEvaluator interface {
	Match(r types.Rule, ev Observation) bool
	Stabilized(r types.Rule, s StabilizationState, now time.Time) bool
}

// ResearchPort is the research rung (SPEC-07 §2).
type ResearchPort = types.ResearchPort

// Outlet is the flow/issues fan-out (SPEC-08 §2 / SPEC-09 §2).
type Outlet interface {
	EnsureIssue(ctx context.Context, inc types.Incident) (string, error)
	EnsureBoardRow(ctx context.Context, inc types.Incident) (string, error)
	Comment(ctx context.Context, inc types.Incident, body string) error
	RequestHotfix(ctx context.Context, inc types.Incident) (string, error)
	Promote(ctx context.Context, inc types.Incident, ev types.Evidence) (string, error)
	CloseOutlets(ctx context.Context, inc types.Incident, reason string) error
}

// LedgerWriter is SPEC-01's writer (single writer).
type LedgerWriter interface {
	Append(ctx context.Context, kind types.RecordKind, sig string, inc string, payload map[string]any) (types.Record, error)
	Flush(ctx context.Context) error
}

// IndexReader is SPEC-01's bounded in-memory index.
type IndexReader interface {
	OpenIncidentBySig(ctx context.Context, sig string) (types.Incident, bool, error)
	OpenIncidentByInKey(ctx context.Context, inKey string) (types.Incident, bool, error)
	ResolvedIncidentBySig(ctx context.Context, sig string, window types.Duration) (types.Incident, bool, error)
	EventsInWindow(ctx context.Context, sig string, from, to string) (int, error)
	CountersInWindow(ctx context.Context, from, to string) (map[string]int64, error)
	GapsInWindow(ctx context.Context, from, to string) ([]types.GapRecord, error)
	Seq() uint64
}

// Notifier is the escalation path (SPEC-12).
type Notifier interface {
	Emit(ctx context.Context, inc types.Incident, class string, detail map[string]any) error
}

// Clock is the monotonic-capable clock.
type Clock interface {
	Now() time.Time
	Monotonic() time.Duration
}

// Deps carries every collaborator; nothing in this package opens a socket or a
// subprocess (SPEC-05 §2).
type Deps struct {
	Ledger   LedgerWriter
	Index    IndexReader
	Registry PlayRunner
	Research ResearchPort
	Outlets  Outlet
	Notify   Notifier
	Clock    Clock
	Eval     RuleEvaluator
	Cfg      Config

	// Rules resolves a rule by name. SPEC-05 §3 reads the rule's EntryRung,
	// Cooldown, MaxRuns, VerifyWin and Hotfix on every rung decision, so the
	// ladder needs a rule lookup; §2's Deps list does not name one, which is a
	// spec gap recorded in the run report. A nil lookup means "no rule metadata":
	// the ladder then uses the documented defaults (entry rung play, ceiling
	// outlets, verify_default window).
	Rules func(name string) (types.Rule, bool)

	// PlayFor resolves the play a rule drafts. SPEC-06 §3.7 owns play resolution;
	// the ladder only asks for the play of a rule. A nil lookup means no play is
	// available and the play rung degrades through T09.
	PlayFor func(rule string) (types.Play, bool)

	// PIDAlive resolves process liveness for the lease and the re-adopter
	// (§3.6, §3.5). A nil check treats every pid as dead.
	PIDAlive func(pid int) bool

	// Agent is the agent stage's LLM port (SPEC-05 §2a). Nil means the stage
	// refuses with TROUBLE-LADDER-021 instead of inventing a completion.
	Agent AgentPort

	// Skills is the local SKILL.md library (SPEC-05 §2b). Nil means no library is
	// read and no skill step runs.
	Skills SkillLibrary
}

// Observation is the admission input (scrubbed and sig-keyed before it arrives).
type Observation struct {
	EventID       string
	TS            string
	Sig           types.Sig
	InKey         string
	Rule          string
	Source        SourcePath
	Subject       string
	Taxonomy      string
	AppKind       string
	Severity      types.Severity
	Stabilization StabilizationState
	Detail        map[string]any
}

// AdmitResult reports what admission did (SPEC-05 §2).
type AdmitResult struct {
	Inc      string
	Created  bool
	Folded   bool
	Reopened bool
	Refused  bool
	Reason   string
}

// Transition is one requested state change (SPEC-05 §2). Trigger names an edge
// (T01…T48) or is empty, in which case the ladder picks the edge implied by
// From/To when exactly one is legal.
type Transition struct {
	Seq      uint64
	From     types.LadderState
	To       types.LadderState
	Trigger  string
	Evidence *types.Evidence
	PlayRun  int
}

// Window is a verification window (SPEC-05 §2).
type Window struct {
	Start string
	End   string
	S     types.Duration
}

// ParkReason is why a run was parked.
type ParkReason = types.ParkReason

// ParkReport is the result of a drain (SPEC-05 §2).
type ParkReport struct {
	Records    []types.ParkRecord
	FlushedSeq uint64
	ElapsedMS  int
}

// ReAdoptReport is the boot re-adoption result (SPEC-05 §2).
type ReAdoptReport struct {
	Resumed  []string
	Failed   []string
	Orphaned []string
}

// PendingItem is one refused-but-remembered transition (SPEC-05 §3.4).
type PendingItem struct {
	Inc     string
	From    types.LadderState
	To      types.LadderState
	SinceTS string
	Reason  string
}

// incState is the ladder's in-memory view of one incident. The ledger is
// authoritative; this is a cache (INV-2).
type incState struct {
	Inc                types.Incident
	Sigs               []string
	InKey              string
	Rule               string
	Ceiling            types.Rung
	IllegalTransitions int
	Strikes            []time.Time
	ArrivalPaths       map[string]int
	FoldCount          int64
	SuppressedCount    int64
	SuppressUntil      string
	BreakerScope       string
	Window             *Window
	Evidence           *types.Evidence
	InvalidCount       int
	VerifyFailures     int
	PlayRuns           int
	AgentRuns          int
	ResearchPlays      int
	ReopenWindowMiss   bool
	Stabilization      StabilizationState
	LastSeq            uint64
	LastEdge           string
	LastTransitionTS   string
	Play               *types.Play
	RunSummary         *RunSummary
	ResearchID         string
	WaitOnly           bool
	EffectiveMaxRuns   int
	SuppressCycles     int64
	VerifyRetryUsed    int
	RelatedGroups      []string
	IssueID            string
	TaskID             string
	SpawnID            string
	Outcome            *types.ResearchOutcome

	// Stage facts the table's guards and payloads read. They are set by the
	// stage effects and by the daemon's own callbacks (an agent run completing,
	// a rollback failing, a snapshot failing validation).
	NoRunnable            bool
	Skipped               bool
	RollbackFailures      int
	QuarantineReason      string
	SnapshotInvalid       bool
	WindowUnclampable     bool
	SeqStalled            bool
	SnapshotDisagreement  bool
	AgentResult           string
	AgentMutations        int
	ModuleVerifyOK        bool
	OutOfScopeTool        bool
	StrikesWhileSuspended int
	SuspendUntil          string
	FailureClass          string
	Waiting               bool
	BreakerClosed         bool
	Worktree              string
	PID                   int
	AcquiredLease         types.AgentLease
}

// LeaseID is the lease the incident currently holds ("" when free).
func (st *incState) LeaseID() string { return st.Inc.LeaseID }

// Ladder is the state machine (SPEC-05).
type Ladder struct {
	deps   Deps
	cfg    Config
	hostID string

	mu          sync.Mutex
	incs        map[string]*incState
	openBySig   map[string]string
	openByInKey map[string]string
	seenEvents  map[string]bool
	seenOrder   []string

	canaries map[string][]canaryObs
	breakers map[string]*breakerState
	suppress map[string]suppression
	budget   map[string]int64
	gate     types.AutonomyGates
	pending  map[string]PendingItem
	parkRecs map[string]types.ParkRecord
	resumed  map[string]bool
	lease    *leaseTable
	booted   bool
	draining bool
}

type suppression struct {
	Until   string
	Reason  string
	Count   int64
	Cycles  int64
	Quiet   bool
	Opened  string
	Pending bool
}

// New builds the ladder (SPEC-05 §2). cmd/troubled and this package's tests are
// the only constructors of Deps.
func New(deps Deps) (*Ladder, error) {
	if deps.Ledger == nil {
		return nil, newErr(types.CodeLadder005, "", "Deps.Ledger is required: the ladder is not the ledger's writer")
	}
	if deps.Clock == nil {
		deps.Clock = wallClock{}
	}
	cfg := deps.Cfg.WithDefaults()
	l := &Ladder{
		deps:        deps,
		cfg:         cfg,
		hostID:      cfg.HostID,
		incs:        map[string]*incState{},
		openBySig:   map[string]string{},
		openByInKey: map[string]string{},
		seenEvents:  map[string]bool{},
		breakers:    map[string]*breakerState{},
		suppress:    map[string]suppression{},
		budget:      map[string]int64{},
		pending:     map[string]PendingItem{},
		parkRecs:    map[string]types.ParkRecord{},
		resumed:     map[string]bool{},
		lease:       newLeaseTable(cfg.HostID),
	}
	l.gate = types.AutonomyGates{
		Mode:          cfg.AutonomyMode,
		KillSwitch:    cfg.KillSwitch,
		AllowDetect:   true,
		AllowResearch: true,
		AllowAgent:    true,
	}
	return l, nil
}

type wallClock struct{}

func (wallClock) Now() time.Time           { return time.Now().UTC() }
func (wallClock) Monotonic() time.Duration { return 0 }

// now is the ladder's clock read.
func (l *Ladder) now() time.Time { return l.deps.Clock.Now().UTC() }

// ---------------------------------------------------------------------------
// Admission (SPEC-05 §3.9): the only creator of incidents.
// ---------------------------------------------------------------------------

// Admit takes one scrubbed observation that a rule qualified. It is an
// idempotent upsert keyed by (sig, open incident) with a second key for
// cross-plane correlation (AC-22).
func (l *Ladder) Admit(ctx context.Context, obs Observation) (AdmitResult, error) {
	if obs.Sig.String() == "" {
		return AdmitResult{}, newErr(types.CodeLadder001, "", "admission without a sig")
	}
	sig := obs.Sig.String()
	l.mu.Lock()
	defer l.mu.Unlock()

	if obs.EventID != "" && l.seenEvents[obs.EventID] {
		// Replaying the same arrival yields one record and one counter
		// increment: admission is idempotent on the event id.
		incID := l.openBySig[sig]
		return AdmitResult{Inc: incID, Folded: incID != ""}, nil
	}

	inKey := obs.InKey
	if inKey == "" {
		inKey = l.inKeyFor(obs)
	}
	if obs.EventID != "" {
		l.noteEventLocked(obs.EventID)
	}

	// A suppression window already open at commit time puts a fresh admission
	// straight into `suppressed` (T02).
	if until, reason, ok := l.suppressionWindowLocked(sig); ok {
		id := l.newIncidentLocked(ctx, obs, sig, inKey)
		st := l.incs[id]
		st.Inc.State = types.StSuppressed
		st.SuppressUntil = until
		st.SuppressedCount = 0
		_ = l.appendIncident(ctx, st, map[string]any{
			"transition":       "T02",
			"error_code":       string(types.CodeLadder018),
			"from":             string(types.StDetected),
			"to":               string(types.StSuppressed),
			"suppress_until":   until,
			"suppressed_count": 0,
			"breaker_scope":    reason,
			"entry_rung":       string(st.Inc.EntryRung),
			"rung":             string(st.Inc.Rung),
			"sigs":             st.Sigs,
			"severity":         string(st.Inc.Severity),
		})
		return AdmitResult{Inc: id, Created: true, Reason: string(types.CodeLadder018)}, nil
	}

	// A scope breaker holds new work: the arrival folds (T48).
	if scope, open := l.openBreakerLocked(sig, obs); open {
		incID := l.foldLocked(ctx, sig, inKey, obs, scope)
		return AdmitResult{Inc: incID, Folded: incID != "", Reason: scope}, nil
	}

	// An open incident collects the arrival (never a duplicate — INV-1).
	if id, ok := l.openBySig[sig]; ok {
		st := l.incs[id]
		if st != nil {
			st.ArrivalPaths[string(obs.Source)]++
			st.FoldCount++
			if st.SuppressedCount > 0 || l.suppressedLocked(sig) {
				st.SuppressedCount++
			}
			if err := l.appendIncident(ctx, st, map[string]any{
				"transition":       "fold",
				"folded":           true,
				"from":             string(st.Inc.State),
				"to":               string(st.Inc.State),
				"sigs":             st.Sigs,
				"arrival_paths":    st.ArrivalPaths,
				"suppressed_count": st.SuppressedCount,
			}); err != nil {
				return AdmitResult{}, wrapErr(types.CodeLadder015, "", err)
			}
			// Cross-ref, never duplicate: the first recurrence and every 10th
			// fold comment on the existing issue/board row (§3.9).
			if st.FoldCount == 1 || st.FoldCount%10 == 0 {
				l.comment(ctx, st, "recurrence fold #"+itoa(st.FoldCount))
			}
			return AdmitResult{Inc: id, Folded: true}, nil
		}
	}

	// The correlation key: two sigs with equal inKey share one incident (§3.9).
	if id, ok := l.openByInKey[inKey]; ok {
		st := l.incs[id]
		if st != nil && !contains(st.Sigs, sig) {
			st.Sigs = append(st.Sigs, sig)
			st.Inc.Sig = st.Sigs[0]
			st.ArrivalPaths[string(obs.Source)]++
			l.openBySig[sig] = id
			if err := l.appendIncident(ctx, st, map[string]any{
				"transition":    "merge",
				"folded":        true,
				"from":          string(st.Inc.State),
				"to":            string(st.Inc.State),
				"sigs":          st.Sigs,
				"inKey":         inKey,
				"arrival_paths": st.ArrivalPaths,
			}); err != nil {
				return AdmitResult{}, wrapErr(types.CodeLadder015, "", err)
			}
			return AdmitResult{Inc: id, Folded: true, Reason: "merged"}, nil
		}
	}

	// A resolved incident inside the reopen window reopens (T42), keeping the
	// same incident id.
	if id, st, ok := l.resolvedLocked(sig, inKey); ok {
		if st.Inc.ReopenCount < l.cfg.ReopenMax {
			st.Inc.ReopenCount++
			fr := st.Inc.State
			st.Inc.State = types.StRecorded
			st.Inc.UpdatedTS = types.FormatUTC(l.now())
			st.Inc.ResolvedTS = ""
			l.openBySig[sig] = id
			if inKey != "" {
				l.openByInKey[inKey] = id
			}
			if err := l.appendIncident(ctx, st, map[string]any{
				"transition":   "reopen",
				"from":         string(fr),
				"to":           string(types.StRecorded),
				"reopen":       true,
				"reopen_count": st.Inc.ReopenCount,
				"rung":         string(st.Inc.Rung),
				"sigs":         st.Sigs,
			}); err != nil {
				return AdmitResult{}, wrapErr(types.CodeLadder015, "", err)
			}
			l.comment(ctx, st, "recurrence after resolve (reopen #"+itoa(int64(st.Inc.ReopenCount))+")")
			return AdmitResult{Inc: id, Reopened: true}, nil
		}
		// The reopen cap is spent: a new incident opens (§3.9).
	}

	// A mismatched open incident for the sig is an index-integrity failure: the
	// mismatched incident is quarantined and both ids are recorded (§3.9).
	if _, st, ok := l.mismatchedLocked(sig, obs); ok {
		st.Inc.State = types.StQuarantined
		st.IllegalTransitions += 3
		newID := l.newIncidentLocked(ctx, obs, sig, inKey)
		_ = l.appendIncident(ctx, st, map[string]any{
			"transition":          "quarantine",
			"quarantine_reason":   reasonIndexMismatch,
			"from":                string(types.StRecorded),
			"to":                  string(types.StQuarantined),
			"sigs":                st.Sigs,
			"illegal_transitions": st.IllegalTransitions,
			"mismatched_with":     newID,
		})
		return AdmitResult{Inc: newID, Created: true, Reason: string(types.CodeLadder020)}, nil
	}

	id := l.newIncidentLocked(ctx, obs, sig, inKey)
	return AdmitResult{Inc: id, Created: true}, nil
}

// newIncidentLocked opens an incident in `recorded` (T01) and emits its record.
func (l *Ladder) newIncidentLocked(ctx context.Context, obs Observation, sig, inKey string) string {
	now := types.FormatUTC(l.now())
	rung, ceiling := l.rungsFor(obs.Rule)
	id := types.NewID(types.PInc)
	st := &incState{
		Inc: types.Incident{
			ID:        id,
			Sig:       sig,
			GroupID:   types.NewID(types.PGrp),
			State:     types.StRecorded,
			EntryRung: rung,
			Rung:      rung,
			Severity:  obs.Severity,
			OpenedTS:  now,
			UpdatedTS: now,
			VerifyWin: l.verifyWindowFor(obs.Rule),
		},
		Sigs:             []string{sig},
		InKey:            inKey,
		Rule:             obs.Rule,
		Ceiling:          ceiling,
		ArrivalPaths:     map[string]int{string(obs.Source): 1},
		Stabilization:    obs.Stabilization,
		EffectiveMaxRuns: l.maxRunsFor(obs.Rule),
	}
	l.incs[id] = st
	l.openBySig[sig] = id
	if inKey != "" {
		l.openByInKey[inKey] = id
	}
	payload := map[string]any{
		"transition":          "T01",
		"from":                string(types.StDetected),
		"to":                  string(types.StRecorded),
		"entry_rung":          string(rung),
		"rung":                string(rung),
		"rule":                obs.Rule,
		"inKey":               inKey,
		"sigs":                st.Sigs,
		"arrival_paths":       st.ArrivalPaths,
		"severity":            string(obs.Severity),
		"stabilize_degraded":  obs.Stabilization.Degraded,
		"illegal_transitions": 0,
	}
	if err := l.appendIncident(ctx, st, payload); err != nil {
		_ = err
	}
	return id
}

// foldLocked folds an arrival into the newest open incident for the source,
// returning its id ("" when none exists yet).
func (l *Ladder) foldLocked(ctx context.Context, sig, inKey string, obs Observation, scope string) string {
	id := ""
	if v, ok := l.openBySig[sig]; ok {
		id = v
	} else if v, ok := l.openByInKey[inKey]; ok {
		id = v
	}
	if id == "" {
		l.noteOneScopeCodeLocked(ctx, scope, nil)
		return ""
	}
	st := l.incs[id]
	if st != nil {
		st.ArrivalPaths[string(obs.Source)]++
		st.FoldCount++
		st.BreakerScope = scope
		st.SuppressedCount++
		_ = l.appendIncident(ctx, st, map[string]any{
			"transition":       "T48",
			"folded":           true,
			"from":             string(st.Inc.State),
			"to":               string(st.Inc.State),
			"breaker_scope":    scope,
			"suppressed_count": st.SuppressedCount,
			"sigs":             st.Sigs,
			"arrival_paths":    st.ArrivalPaths,
		})
	}
	l.noteOneScopeCodeLocked(ctx, scope, st)
	return id
}

// noteOneScopeCodeLocked records TROUBLE-LADDER-014 once per window and then
// every 100th arrival (§3.8), and gives a flapping breaker its human path: four
// re-opens in 24h file an issue.
func (l *Ladder) noteOneScopeCodeLocked(ctx context.Context, scope string, st *incState) {
	b := l.breakers[scope]
	if b == nil {
		return
	}
	b.Folds++
	if b.Folds == 1 || b.Folds%100 == 0 {
		b.Codes = append(b.Codes, string(types.CodeLadder014))
	}
	if b.Breaker.State == types.BreakerOpen {
		inc := types.Incident{}
		if st != nil {
			inc = st.Inc
		}
		l.breakerEscalationLocked(ctx, b, inc)
	}
}

// ---- idempotency of arrivals ----

func (l *Ladder) seenEvent(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seenEvents[id]
}

func (l *Ladder) noteEventLocked(id string) {
	if l.seenEvents[id] {
		return
	}
	l.seenEvents[id] = true
	l.seenOrder = append(l.seenOrder, id)
	const maxSeen = 4096
	if len(l.seenOrder) > maxSeen {
		drop := l.seenOrder[0]
		l.seenOrder = l.seenOrder[1:]
		delete(l.seenEvents, drop)
	}
}

// ---- helpers ----

// rungFor maps a rule to its entry rung; a rule that does not resolve is `play`
// (the §4.3 default).
func (l *Ladder) rungFor(rule string) types.Rung {
	r, ok := l.rule(rule)
	if !ok || r.EntryRung == "" {
		return types.RungPlay
	}
	return r.EntryRung
}

// rungsFor returns (entry rung, ceiling). The ceiling is the rung the incident
// may reach: the entry rung itself, per §3.2's "ceiling ≥ play" language and
// T18's "ceiling == play" (an incident entering at play escalates after its
// play runs are spent instead of entering research).
func (l *Ladder) rungsFor(rule string) (types.Rung, types.Rung) {
	r := l.rungFor(rule)
	switch r {
	case types.RungRecord:
		return r, types.RungRecord
	case types.RungResearch:
		return r, types.RungResearch
	case types.RungAgent:
		return r, types.RungAgent
	default:
		return types.RungPlay, types.RungPlay
	}
}

func (l *Ladder) rule(name string) (types.Rule, bool) {
	if l.deps.Rules == nil {
		return types.Rule{}, false
	}
	return l.deps.Rules(name)
}

// verifyWindowFor applies §3.10's window validity rules.
func (l *Ladder) verifyWindowFor(rule string) types.Duration {
	w := l.cfg.VerifyDefault
	if r, ok := l.rule(rule); ok && r.VerifyWin != "" {
		w = r.VerifyWin
	}
	fixed, _ := clampWindow(w, l.cfg.VerifyDefault)
	return fixed
}

// maxRunsFor is the effective play cap (Rule.MaxRuns, clamped to the cap).
func (l *Ladder) maxRunsFor(rule string) int {
	n := l.cfg.PlayMaxRunsDefault
	if r, ok := l.rule(rule); ok && r.MaxRuns > 0 {
		n = r.MaxRuns
	}
	if l.cfg.PlayMaxRunsCap > 0 && n > l.cfg.PlayMaxRunsCap {
		n = l.cfg.PlayMaxRunsCap
	}
	return n
}

// inKeyFor computes the cross-plane correlation key (SPEC-05 §3.9):
//
//	ink_ + hex(sha256(source_class + US + subject + US + taxonomy + US + app_kind))[:16]
func (l *Ladder) inKeyFor(obs Observation) string {
	class := sourceClass(obs.Source)
	subject := l.subjectFor(obs)
	sum := sha256.Sum256([]byte(class + "\x1f" + subject + "\x1f" + obs.Taxonomy + "\x1f" + obs.AppKind))
	return "ink_" + hex.EncodeToString(sum[:])[:16]
}

// sourceClass maps a delivery path onto its class (§3.9).
func sourceClass(p SourcePath) string {
	switch p {
	case "sentinel", "collector":
		return "app"
	case "journald", "dbus":
		return "unit"
	default:
		return "resource"
	}
}

// subjectFor derives the merge subject: for `unit` the unescaped unit name, for
// `app` the unit alias when the config maps the project, else project:<id>, for
// `resource` the sensor|scope pair.
func (l *Ladder) subjectFor(obs Observation) string {
	switch sourceClass(obs.Source) {
	case "unit":
		return strings.TrimSpace(obs.Subject)
	case "app":
		if unit, ok := l.cfg.CorrelationUnitAlias[obs.Subject]; ok && unit != "" {
			return "unit:" + unit
		}
		if obs.Subject == "" {
			return ""
		}
		return "project:" + obs.Subject
	default:
		return obs.Subject
	}
}

// ---- incident accessors ----

// Incident returns one incident's projection.
func (l *Ladder) Incident(ctx context.Context, incID string) (types.Incident, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.incs[incID]
	if !ok {
		if l.deps.Index != nil {
			if inc, found, err := l.deps.Index.OpenIncidentBySig(ctx, incID); err == nil && found {
				return inc, nil
			}
		}
		return types.Incident{}, newErr(types.CodeLadder012, "", "incident %s is not in the index", incID)
	}
	return st.Inc, nil
}

// OpenForKey returns the open incident for a sig or inKey (§3.9).
func (l *Ladder) OpenForKey(ctx context.Context, sig, inKey string) (types.Incident, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if id, ok := l.openBySig[sig]; ok {
		if st := l.incs[id]; st != nil {
			return st.Inc, true, nil
		}
	}
	if inKey != "" {
		if id, ok := l.openByInKey[inKey]; ok {
			if st := l.incs[id]; st != nil {
				return st.Inc, true, nil
			}
		}
	}
	return types.Incident{}, false, nil
}

// resolvedLocked finds a resolved incident inside the reopen window.
func (l *Ladder) resolvedLocked(sig, inKey string) (string, *incState, bool) {
	for id, st := range l.incs {
		if st.Inc.State != types.StResolved && st.Inc.State != types.StEscalated {
			continue
		}
		if !contains(st.Sigs, sig) && (inKey == "" || st.InKey != inKey) {
			continue
		}
		return id, st, true
	}
	return "", nil, false
}

// mismatchedLocked detects an open incident whose sig does not match the
// arriving sig but which the index maps to it (TROUBLE-LADDER-020, §3.9).
func (l *Ladder) mismatchedLocked(sig string, obs Observation) (string, *incState, bool) {
	if l.deps.Index == nil {
		return "", nil, false
	}
	inc, found, err := l.deps.Index.OpenIncidentBySig(context.Background(), sig)
	if err != nil || !found {
		return "", nil, false
	}
	if inc.Sig == sig {
		return "", nil, false
	}
	if st, ok := l.incs[inc.ID]; ok {
		return inc.ID, st, true
	}
	return "", nil, false
}

// inKeyOf exposes the derived inKey for tests and the SPEC-04 hand-off.
func (l *Ladder) InKeyFor(obs Observation) string {
	if obs.InKey != "" {
		return obs.InKey
	}
	return l.inKeyFor(obs)
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
