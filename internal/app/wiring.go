// Package app is the composition root's shared wiring: the adapters that let the
// subsystem packages meet without importing each other.
//
// Nothing in this package makes a policy decision of its own — every adapter is a
// translation between two contracts that already exist:
//
//	internal/ledger  ──►  internal/ladder  (LedgerWriter, IndexReader)
//	internal/registry ──► internal/ladder  (PlayRunner: Run/Check/Apply/Rollback)
//	internal/sensors ──► internal/ladder   (RuleEvaluator: the shared condition language)
//
// SPEC-12 §4.5 names this layer "the app wiring" and SPEC-10 §4.1 keeps the
// dashboard out of it: the dashboard receives function fields, this package
// supplies them. Keeping the translation here is what lets cmd/troubled and
// cmd/trouble stay thin and what keeps internal/ledger free of an import on the
// ladder (the Actor triple is injected, never imported).
package app

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/ladder"
	"github.com/totalwindupflightsystems/trouble/internal/ledger"
	"github.com/totalwindupflightsystems/trouble/internal/registry"
	"github.com/totalwindupflightsystems/trouble/internal/sensors"
	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// Store adapts one *ledger.Ledger to the two views the ladder needs: the writer
// (SPEC-01 §3.5, single writer) and the bounded index (SPEC-01 §3.6).
type Store struct {
	L      *ledger.Ledger
	Actor  types.Actor
	HostID string
	Source string // origin.source stamped on records the wiring writes

	mu    sync.Mutex
	inKey map[string]string // inKey → incident id, for the ladder's cross-path dedup
}

// NewStore returns the adapter.
func NewStore(l *ledger.Ledger, actor types.Actor, hostID string) *Store {
	return &Store{L: l, Actor: actor, HostID: hostID, Source: "troubled", inKey: map[string]string{}}
}

// DraftWriter adapts Store to the draft-shaped writer interfaces the other
// packages declare (lifecycle.RecordWriter and sensors.EmitFunc both take a
// types.RecordDraft and hand back the written record). Two shapes exist because
// the ladder's writer is kind/sig/inc/payload while the ledger's own API is a
// draft; one adapter is cheaper than either package learning the other's shape.
type DraftWriter struct{ S *Store }

// Append writes one draft.
func (w DraftWriter) Append(ctx context.Context, d types.RecordDraft) (types.Record, error) {
	if w.S == nil {
		return types.Record{}, errors.New("app: DraftWriter has no store")
	}
	if d.Actor.ID == "" {
		d.Actor = actorFrom(ctx, w.S.Actor)
	}
	if d.Origin.HostID == "" {
		d.Origin = types.Origin{HostID: w.S.HostID, Source: w.S.Source}
	}
	return w.S.L.Append(ctx, d)
}

// Append writes one record of the requested kind (ladder.LedgerWriter).
func (s *Store) Append(ctx context.Context, kind types.RecordKind, sig, inc string, payload map[string]any) (types.Record, error) {
	if payload == nil {
		payload = map[string]any{}
	}
	rec, err := s.L.Append(ctx, types.RecordDraft{
		Kind:    kind,
		Sig:     sig,
		Inc:     inc,
		Origin:  types.Origin{HostID: s.HostID, Source: s.Source},
		Actor:   s.Actor,
		Payload: payload,
	})
	if err != nil {
		return types.Record{}, err
	}
	if kind == types.KIncident && inc != "" {
		if k, ok := payload["inKey"].(string); ok && k != "" {
			s.mu.Lock()
			s.inKey[k] = inc
			s.mu.Unlock()
		}
	}
	return rec, nil
}

// Flush is a no-op that means what SPEC-05 expects it to mean: Append does not
// return until the record's batch has been written and fsynced (SPEC-01 §3.5
// group commit), so by the time the ladder calls Flush there is nothing buffered
// to force out. It is declared rather than omitted because the ladder's contract
// has a flush point, and a silent nil would be indistinguishable from a dropped
// flush.
func (s *Store) Flush(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

// OpenIncidentBySig returns the open incident for a signature.
func (s *Store) OpenIncidentBySig(ctx context.Context, sig string) (types.Incident, bool, error) {
	if err := ctx.Err(); err != nil {
		return types.Incident{}, false, err
	}
	inc, _, err := s.L.Query().IncidentBySig(sig)
	if err != nil || inc == nil {
		return types.Incident{}, false, err
	}
	if !isOpen(inc.State) {
		return types.Incident{}, false, nil
	}
	return *inc, true, nil
}

// OpenIncidentByInKey resolves the inKey→incident fold the ladder performs across
// arrival paths (SPEC-05 admission).
//
// SPEC-01's index keys incidents by sig, not by inKey, so the fold is served
// from (a) the live map this adapter maintains and (b) a bounded fallback that
// reads the head of each open incident's timeline from the index ring and takes
// the `inKey` of its admission record. Both are index/ring reads — no ledger file
// walk — which is what keeps a boot-time admission as cheap as a live one.
func (s *Store) OpenIncidentByInKey(ctx context.Context, inKey string) (types.Incident, bool, error) {
	if err := ctx.Err(); err != nil {
		return types.Incident{}, false, err
	}
	s.mu.Lock()
	id, ok := s.inKey[inKey]
	s.mu.Unlock()
	if ok {
		inc, _, err := s.L.Query().Incident(id)
		if err != nil {
			return types.Incident{}, false, err
		}
		if inc != nil && isOpen(inc.State) {
			return *inc, true, nil
		}
	}
	dr := s.L.DashReader()
	for _, open := range dr.OpenIncidents(200) {
		head := dr.RecordsForIncident(open.ID, 0, 8)
		for _, rec := range head {
			if rec.Payload == nil {
				continue
			}
			if k, _ := rec.Payload["inKey"].(string); k == inKey {
				s.mu.Lock()
				s.inKey[inKey] = open.ID
				s.mu.Unlock()
				return open, true, nil
			}
		}
	}
	return types.Incident{}, false, nil
}

// ResolvedIncidentBySig returns a resolved incident for a signature inside the
// given window (the ladder's recurrence test).
func (s *Store) ResolvedIncidentBySig(ctx context.Context, sig string, window types.Duration) (types.Incident, bool, error) {
	if err := ctx.Err(); err != nil {
		return types.Incident{}, false, err
	}
	inc, _, err := s.L.Query().IncidentBySig(sig)
	if err != nil || inc == nil {
		return types.Incident{}, false, err
	}
	if isOpen(inc.State) || inc.State != types.StResolved {
		return types.Incident{}, false, nil
	}
	w := window.Std()
	if w > 0 && inc.ResolvedTS != "" {
		ts, perr := types.ParseUTC(inc.ResolvedTS)
		if perr == nil && time.Since(ts) > w {
			return types.Incident{}, false, nil
		}
	}
	return *inc, true, nil
}

// EventsInWindow counts the events projected for a signature inside [from, to].
// It is served from the index's bounded recent-record ring: a verification window
// is minutes wide, and the ring holds the newest 8192 records, so the count is a
// bounded in-memory walk rather than a ledger read (SPEC-10 §2.9's no-scan rule
// applied to the ladder's own verification input).
func (s *Store) EventsInWindow(ctx context.Context, sig string, from, to string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return s.L.DashReader().EventsBetween(sig, from, to), nil
}

// CountersInWindow returns the per-group counter deltas inside a window.
func (s *Store) CountersInWindow(ctx context.Context, from, to string) (map[string]int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	got, _, err := s.L.Query().CounterDeltas(from, to)
	return got, err
}

// GapsInWindow returns the gap records overlapping a window.
func (s *Store) GapsInWindow(ctx context.Context, from, to string) ([]types.GapRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	gaps, _, err := s.L.Query().Gaps(from, 500)
	if err != nil {
		return nil, err
	}
	out := make([]types.GapRecord, 0, len(gaps))
	for _, g := range gaps {
		if to != "" && g.FromTS > to {
			continue
		}
		out = append(out, g)
	}
	return out, nil
}

// Seq is the newest sequence number (SPEC-05's monotonic anchor).
func (s *Store) Seq() uint64 { return s.L.DashReader().LastSeq() }

func isOpen(st types.LadderState) bool {
	switch st {
	case types.StResolved, "":
		return false
	}
	return true
}

// PlayEngine adapts *registry.Registry to the ladder's PlayRunner: the four
// methods the ladder calls (SPEC-05 §2) mapped onto the six-stage call contract
// and the play runner SPEC-06 owns.
type PlayEngine struct {
	Reg *registry.Registry

	mu sync.Mutex
}

// NewPlayEngine returns the adapter.
func NewPlayEngine(reg *registry.Registry) *PlayEngine { return &PlayEngine{Reg: reg} }

// Run executes a play and maps the registry's PlayRun onto the ladder's summary.
func (p *PlayEngine) Run(ctx context.Context, inc types.Incident, play types.Play, mode string) (ladder.RunSummary, error) {
	base := types.ToolCallRequest{
		Inc:   inc.ID,
		Rule:  play.Name,
		Sig:   inc.Sig,
		Mode:  mode,
		Actor: types.Actor{Kind: types.ActorDaemon, ID: "troubled"},
	}
	run, err := p.Reg.RunPlay(ctx, play, base)
	sum := ladder.RunSummary{
		TasksRun:    len(run.Tasks),
		Changed:     run.Changed,
		DiffSummary: run.Outcome,
	}
	for _, t := range run.Tasks {
		if t.Status == "failed" || t.Status == "refused" {
			sum.TasksFailed++
			if sum.FailClass == "" {
				sum.FailClass = t.ErrorCode
			}
		}
	}
	if err != nil {
		if sum.FailClass == "" {
			sum.FailClass = string(registry.CodeOf(err))
		}
		return sum, err
	}
	return sum, nil
}

// Check runs the registry's check-only path.
func (p *PlayEngine) Check(ctx context.Context, tool string, args map[string]any) (types.Diff, types.ToolCall, error) {
	return p.Reg.Runner(types.ToolCallRequest{}).Check(ctx, tool, args)
}

// Apply runs all six stages.
func (p *PlayEngine) Apply(ctx context.Context, tool string, args map[string]any) (types.Result, types.ToolCall, error) {
	return p.Reg.Runner(types.ToolCallRequest{}).Apply(ctx, tool, args)
}

// Rollback re-authorizes a rollback hint through the same six stages.
func (p *PlayEngine) Rollback(ctx context.Context, t types.ToolCall) error {
	return p.Reg.Runner(types.ToolCallRequest{}).Rollback(ctx, t)
}

// Evaluator is the ladder's RuleEvaluator over the shared condition language
// (SPEC-03 §3.4). Compiled predicates are cached per rule name: a rule's match
// table is data and recompiling it on every admission would put a parser on the
// hot path.
type Evaluator struct {
	mu      sync.Mutex
	rules   map[string]sensors.RulePredicate
	failed  map[string]struct{}
	sources map[string]types.SigSource
}

// NewEvaluator returns an empty evaluator.
func NewEvaluator() *Evaluator {
	return &Evaluator{rules: map[string]sensors.RulePredicate{}, failed: map[string]struct{}{}, sources: map[string]types.SigSource{}}
}

// Match evaluates a rule's match list against an observation's variables.
func (e *Evaluator) Match(r types.Rule, ev ladder.Observation) bool {
	pred, ok := e.predicate(r)
	if !ok {
		// A rule whose match table cannot compile admits nothing: the refusal is
		// recorded by the caller (the ladder's own T01 guard), and "false" is the
		// fail-closed answer.
		return false
	}
	return pred(e.vars(ev))
}

func (e *Evaluator) predicate(r types.Rule) (sensors.RulePredicate, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if p, ok := e.rules[r.Name]; ok {
		return p, true
	}
	if _, bad := e.failed[r.Name]; bad {
		return nil, false
	}
	p, err := sensors.CompileRuleMatch(r)
	if err != nil {
		e.failed[r.Name] = struct{}{}
		return nil, false
	}
	e.rules[r.Name] = p
	e.sources[r.Name] = r.Source
	return p, true
}

// vars projects an observation into the condition language's variable map.
func (e *Evaluator) vars(ev ladder.Observation) map[string]any {
	vars := map[string]any{
		"sig":      ev.Sig.String(),
		"rule":     ev.Rule,
		"source":   string(ev.Source),
		"subject":  ev.Subject,
		"taxonomy": ev.Taxonomy,
		"app_kind": ev.AppKind,
		"severity": string(ev.Severity),
		"event_id": ev.EventID,
	}
	for k, v := range ev.Detail {
		if _, taken := vars[k]; !taken {
			vars[k] = v
		}
	}
	return vars
}

// Stabilized reports whether a rule's `for=` window has elapsed since the first
// qualifying observation (the ladder owns the state; this only compares the
// clock against the state it was handed).
func (e *Evaluator) Stabilized(r types.Rule, s ladder.StabilizationState, now time.Time) bool {
	w := r.For.Std()
	if w <= 0 {
		return true
	}
	if s.FirstQualifyingTS == "" {
		return false
	}
	ts, err := types.ParseUTC(s.FirstQualifyingTS)
	if err != nil {
		return false
	}
	return !now.Before(ts.Add(w))
}

// Notifier is the ladder's escalation hook. SPEC-12 owns the channels (the unit
// pair and `escalate.channels`); this adapter is the in-daemon side of it: it
// records one `incident` notification on the ledger (so the dashboard's incident
// story shows it) and logs at WARN so the journal carries it even when the ledger
// writer is the thing that is wedged.
type Notifier struct {
	Store *Store
	Log   *slog.Logger
}

// Emit records and logs one escalation.
func (n Notifier) Emit(ctx context.Context, inc types.Incident, class string, detail map[string]any) error {
	payload := map[string]any{
		"transition": "notify",
		"class":      class,
		"via":        "app.notifier",
		"incident":   inc.ID,
	}
	for k, v := range detail {
		payload[k] = v
	}
	if n.Log != nil {
		n.Log.Warn("ladder escalation", "class", class, "incident", inc.ID, "detail", detail)
	}
	if n.Store == nil {
		return nil
	}
	_, err := n.Store.Append(ctx, types.KIncident, inc.Sig, inc.ID, payload)
	return err
}

// Clock is the ladder's clock: wall now plus a monotonic reading.
type Clock struct{ Start time.Time }

// NewClock returns a clock whose monotonic base is process start.
func NewClock(start time.Time) Clock { return Clock{Start: start} }

// Now is the wall clock.
func (c Clock) Now() time.Time { return time.Now() }

// Monotonic is the time since process start.
func (c Clock) Monotonic() time.Duration { return time.Since(c.Start) }

// Compile-time proof that the adapters satisfy the contracts they exist for.
var (
	_ ladder.LedgerWriter  = (*Store)(nil)
	_ ladder.IndexReader   = (*Store)(nil)
	_ ladder.PlayRunner    = (*PlayEngine)(nil)
	_ ladder.RuleEvaluator = (*Evaluator)(nil)
	_ ladder.Notifier      = Notifier{}
	_ ladder.Clock         = Clock{}
)
