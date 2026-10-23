package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/trouble-agent/trouble/internal/dashboard"
	"github.com/trouble-agent/trouble/internal/ladder"
	"github.com/trouble-agent/trouble/internal/ledger"
	"github.com/trouble-agent/trouble/internal/lifecycle"
	"github.com/trouble-agent/trouble/internal/types"
)

// dashbridge.go adapts the read side of the ledger and the write side of the
// ladder onto the dashboard's declared interfaces (SPEC-10 §4.1/§4.2). The
// dashboard imports none of these packages, so this file is the only place the
// two contracts touch.
//
// SPEC-10 §4.2 pins where each click lands:
//
//	ack   → ladder.Ack(...)   → incident record {transition:"acknowledged", via:"dashboard"}
//	close → ladder.Close(...) → incident record {transition:"closed", resolution, via:"dashboard"}
//	autonomy → lifecycle.SetAutonomy → config record
//
// Both human decisions own a state in SPEC-05's table (amendment TRBL-118,
// §3.2a): the ack walks T49 `human_ack` (detected|recorded → acknowledged) and
// the close walks T50 `human_close` (acknowledged → resolved), beside the
// ladder's own proof edges T38 (window_passed from verifying) and T45
// (quiet_close from suppressed). Before the amendment an ack wrote only an audit
// record, so a freshly-acked incident stayed in a state with no edge into
// `resolved` and every close was refused — the adapters below therefore move the
// machine for the states that have an edge and refuse, never invent, for the
// states that do not. Both are recorded with the human actor.

// actorKey carries a per-request actor override into Store.Append, so a record
// written on behalf of a human says so (SPEC-TYPES §3.1: Actor.Kind=human, ID =
// the token label).
type actorKey struct{}

// WithActor stamps an actor onto a context for one write.
func WithActor(ctx context.Context, a types.Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}

func actorFrom(ctx context.Context, def types.Actor) types.Actor {
	if a, ok := ctx.Value(actorKey{}).(types.Actor); ok && a.ID != "" {
		return a
	}
	return def
}

// Lookup answers the dashboard's single-incident/single-group/evidence reads.
type Lookup struct {
	L *ledger.Ledger
}

// NewLookup returns the lookup adapter.
func NewLookup(l *ledger.Ledger) Lookup { return Lookup{L: l} }

// Incident resolves one incident by id.
func (l Lookup) Incident(id string) (*types.Incident, bool) {
	inc, _, err := l.L.Query().Incident(id)
	if err != nil || inc == nil {
		return nil, false
	}
	return inc, true
}

// Group resolves one group by its group id (the id the dashboard links to);
// SPEC-01's index is keyed by digest and sig, so the shared-id lookup walks the
// bounded group list the index already keeps in memory.
func (l Lookup) Group(id string) (*types.Group, bool) {
	if g, _, err := l.L.Query().GroupByDigest(id); err == nil && g != nil {
		return g, true
	}
	for _, st := range l.L.DashReader().GroupsSince(0, 5000) {
		if st.GroupID != id && st.Digest != id {
			continue
		}
		g, _, err := l.L.Query().GroupByDigest(st.Digest)
		if err != nil || g == nil {
			return nil, false
		}
		return g, true
	}
	return nil, false
}

// Evidence returns the verification tuple for an incident: the newest `verify`
// record's evidence payload, which is the only evidence the ladder ever writes
// (SPEC-05 §3).
func (l Lookup) Evidence(inc string) (*types.Evidence, bool) {
	for _, rec := range l.L.DashReader().RecordsForIncident(inc, 0, 200) {
		if rec.Kind != types.KVerify {
			continue
		}
		raw, ok := rec.Payload["evidence"]
		if !ok {
			continue
		}
		b, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		var ev types.Evidence
		if err := json.Unmarshal(b, &ev); err != nil {
			continue
		}
		return &ev, true
	}
	return nil, false
}

// Story assembles the extra panels of /incidents/{id}. The refs come from the
// incident projection (the ladder sets them when its own outlet stage runs);
// the research/spawn/skill panels come from the owning subsystem's FULL
// records via the evidence query — the SPEC-07/08/11 payload key set
// (research: res_id/state, flow: spawn_id + decision, skills: candidate_id).
// An absent panel means "no such record", never "not wired".
func (l Lookup) Story(inc types.Incident) dashboard.Story {
	st := dashboard.Story{
		IssueRef:   inc.IssueID,
		BoardRow:   inc.TaskID,
		ResearchID: inc.ResearchID,
	}
	bundle, err := l.L.Query().Evidence(inc.ID, 200)
	if err != nil {
		st.Records = l.L.DashReader().RecordsForIncident(inc.ID, 0, 200)
		return st
	}
	st.Records = bundle.Records
	for _, rec := range bundle.Records {
		if rec.Payload == nil {
			continue
		}
		// SPEC-07: the rung's machine brief lands in `research` records.
		if rec.Kind == types.KResearch {
			if v, ok := rec.Payload["res_id"].(string); ok && v != "" && st.ResearchID == "" {
				st.ResearchID = v
			}
			if v, ok := rec.Payload["state"].(string); ok && v != "" {
				st.Research = v
			}
		}
		// SPEC-08: one `spawn` record per state change carries the spawn id
		// and the promotion decision.
		if rec.Kind == types.KSpawn {
			if v, ok := rec.Payload["spawn_id"].(string); ok && v != "" {
				st.SpawnID = v
			}
			if v, ok := rec.Payload["decision"].(string); ok && v != "" {
				st.Promotion = v
			}
			if v, ok := rec.Payload["pr_url"].(string); ok && v != "" {
				st.Promotion = strings.TrimSpace(st.Promotion + " " + v)
			}
		}
		// SPEC-11: the candidate loop's draft/review/promote records.
		if rec.Kind == types.KSkill {
			if v, ok := rec.Payload["candidate_id"].(string); ok && v != "" {
				st.Candidate = v
			}
			if v, ok := rec.Payload["state"].(string); ok && v != "" {
				st.Candidate = strings.TrimSpace(st.Candidate + " " + v)
			}
		}
	}
	return st
}

// Actions is the dashboard's write seam (SPEC-10 §4.2).
type Actions struct {
	Ladder *ladder.Ladder
	Store  *Store
	Now    func() time.Time
}

// NewActions returns the write adapter.
func NewActions(lb *ladder.Ladder, store *Store) Actions {
	return Actions{Ladder: lb, Store: store, Now: time.Now}
}

// Ack records the human acknowledgement, walks the ladder's own pre-work edge
// into `acknowledged` (T49), and applies the hold the `until` means.
//
// TRBL-118: the record alone was not enough. The ack used to write ONLY an audit
// record, so the incident stayed in a state with no edge into `resolved` and the
// dashboard's own close button could never succeed (every close was a
// no_close_edge refusal or a stale_view 409). The ladder owns the state now, and
// the record carries the operator's reason/until/via beside it, plus the
// resulting state so a boot rebuild of the index agrees with the live append.
//
// An incident the ladder has already taken into work, verification, suppression
// or a terminal state keeps the record-only behaviour: there the ack is an audit
// note and the table declares no edge (R14), so the machine is left untouched
// instead of counting the click as an illegal transition (§3.3/INV-6).
func (a Actions) Ack(ctx context.Context, inc string, actor types.Actor, reason string, until types.Duration) error {
	cur, err := a.Ladder.Incident(ctx, inc)
	if err != nil {
		return adaptSeam(err)
	}
	ctx = WithActor(ctx, actor)
	state := cur.State
	if trig, ok := ladder.AckEdge(cur.State); ok {
		tr := ladder.Transition{Seq: a.seq(), From: cur.State, To: types.StAcknowledged, Trigger: trig}
		next, err := a.Ladder.Advance(ctx, inc, tr)
		if err != nil {
			return adaptSeam(err)
		}
		state = next.State
	}
	payload := map[string]any{
		"transition": "acknowledged",
		"state":      string(state),
		"reason":     reason,
		"until":      string(until),
		"via":        "dashboard",
	}
	if _, err := a.Store.Append(ctx, types.KIncident, cur.Sig, inc, payload); err != nil {
		return adaptSeam(err)
	}
	if until != "" && until.Std() > 0 {
		if _, err := a.Ladder.Suppress(ctx, cur.Sig, until, reason); err != nil {
			return adaptSeam(err)
		}
	}
	return nil
}

// Close resolves the incident through the ladder's own edge and records the
// resolution. An incident with no legal edge into `resolved` is refused up
// front: the state machine is data and the composition root does not widen it.
// The pre-check is the close trigger, not RefuseFor alone — a state with
// neither a trigger nor a refusal rule (recorded, detected, …) would otherwise
// fall into Advance, which records the click as an illegal transition
// (quarantining the incident on the third one) instead of refusing it.
func (a Actions) Close(ctx context.Context, inc string, actor types.Actor, reason, resolution string) error {
	cur, err := a.Ladder.Incident(ctx, inc)
	if err != nil {
		return err
	}
	if closeTrigger(cur.State) == "" {
		return errNoCloseEdge(cur.State)
	}
	tr := ladder.Transition{
		Seq:     a.seq(),
		From:    cur.State,
		To:      types.StResolved,
		Trigger: closeTrigger(cur.State),
	}
	ctx = WithActor(ctx, actor)
	if _, err := a.Ladder.Advance(ctx, inc, tr); err != nil {
		return adaptSeam(err)
	}
	_, err = a.Store.Append(ctx, types.KIncident, cur.Sig, inc, map[string]any{
		"transition": "closed",
		"state":      string(types.StResolved),
		"reason":     reason,
		"resolution": resolution,
		"via":        "dashboard",
	})
	return adaptSeam(err)
}

func (a Actions) seq() uint64 {
	if a.Store == nil || a.Store.L == nil {
		return 0
	}
	return a.Store.L.DashReader().LastSeq()
}

// closeTrigger names the ladder's own edge for the state the incident is in.
// Transition.Trigger carries the edge ID — FindEdge resolves by ID, not by the
// trigger's display name — so a verifying close walks T38, a suppressed close
// walks T45 and an acknowledged close (amendment TRBL-118) walks T50. The set is
// DERIVED from the transition table, never restated here: a state that gains an
// edge into `resolved` becomes closable without a second list to keep in sync.
func closeTrigger(st types.LadderState) string {
	if id, ok := ladder.CloseEdge(st); ok {
		return id
	}
	return ""
}

// Autonomy is the dashboard's autonomy seam: reads come from the ladder, and the
// write goes through lifecycle.SetAutonomy so exactly one code path writes the
// `config` record (SPEC-10 §4.2).
type Autonomy struct {
	Ladder *ladder.Ladder
	Store  *Store
	// Record is the audited config-record writer (lifecycle.SetAutonomy takes
	// lifecycle.RecordWriter); nil falls back to the store's draft adapter.
	Record lifecycle.RecordWriter
}

type gatesStore struct{ l *ladder.Ladder }

func (g gatesStore) Gates() types.AutonomyGates {
	if g.l == nil {
		return types.AutonomyGates{}
	}
	return g.l.Gates()
}

// Gates reports the live gates.
func (a Autonomy) Gates() types.AutonomyGates { return gatesStore{a.Ladder}.Gates() }

// SetAutonomy writes the config record and applies the gates to the live ladder.
func (a Autonomy) SetAutonomy(ctx context.Context, gates types.AutonomyGates, actor types.Actor) (types.AutonomyGates, error) {
	ctx = WithActor(ctx, actor)
	applied, err := lifecycle.SetAutonomy(ctx, lifecycle.AutonomyDeps{Gates: gatesStore{a.Ladder}, Writer: a.records()}, gates, actor)
	if err != nil {
		return types.AutonomyGates{}, err
	}
	if a.Ladder != nil {
		if _, err := a.Ladder.SetGates(ctx, applied, actor); err != nil {
			return types.AutonomyGates{}, err
		}
	}
	return applied, nil
}

func (a Autonomy) records() lifecycle.RecordWriter {
	if a.Record != nil {
		return a.Record
	}
	return DraftWriter{a.Store}
}

// Compile-time proof the bridges satisfy the dashboard's contracts.
var (
	_ dashboard.IncidentLookup  = Lookup{}
	_ dashboard.IncidentActions = Actions{}
	_ dashboard.AutonomyWriter  = Autonomy{}
)

// errNoCloseEdge is the refusal a human close gets when the incident is not in
// a state the ladder's table can leave for `resolved`. It names both
// states so the UI can say why instead of "failed", and it carries the seam
// shape the dashboard maps to the wire: the ladder's TROUBLE-LADDER-012 as the
// owning code, no_close_edge as the machine-readable detail, the human reason
// as the message, and the refused-transition mark that makes the API answer
// 409 Conflict instead of a blanket 500 (SPEC-10 §5/§7.6).
func errNoCloseEdge(st types.LadderState) error {
	return &noCloseEdge{state: st}
}

type noCloseEdge struct{ state types.LadderState }

func (e *noCloseEdge) Error() string {
	return "TROUBLE-LADDER-012: incident state " + string(e.state) +
		" has no legal edge to resolved; a human close is defined from verified work (T38), a quiet suppression window (T45) or an acknowledged incident (T50)"
}

// ErrorCode is the seam code: TROUBLE-LADDER-012 is the ladder's permanent
// code for an identity/state the index cannot resolve the close through, and
// the dashboard surfaces it verbatim (SPEC-INDEX §5 rule 2).
func (e *noCloseEdge) ErrorCode() types.ErrorCode { return types.CodeLadder012 }

func (e *noCloseEdge) ErrorDetail() string { return "no_close_edge" }

func (e *noCloseEdge) ErrorReason() string {
	return "incident state " + string(e.state) + " has no legal edge to resolved"
}

func (e *noCloseEdge) RefusedTransition() bool { return true }

// ladderSeamError adapts the ladder's own *ladder.Error onto the seam shape
// the dashboard maps (SPEC-INDEX §5 rule 2): the ladder's code, class and
// reason travel to the wire instead of collapsing into the generic 500
// action_failed. A guard refusal that REJECTED a write the human asked for
// (e.g. LADDER-018 quiet-window-not-quiet on a close, LADDER-006 canary) is
// marked as a refused transition so it answers 409 with its reason; a
// transient class keeps its 503.
type ladderSeamError struct{ e *ladder.Error }

func (a ladderSeamError) Error() string { return a.e.Error() }

// Unwrap exposes the wrapped *ladder.Error so errors.As finds the seam shape
// through the adapter (and any further wrapping).
func (a ladderSeamError) Unwrap() error { return a.e }

func (a ladderSeamError) ErrorCode() types.ErrorCode { return a.e.Code }

func (a ladderSeamError) ErrorDetail() string {
	if a.e.Detail != "" {
		return "ladder_" + strings.ToLower(strings.TrimPrefix(string(a.e.Code), "TROUBLE-LADDER-"))
	}
	return ""
}

func (a ladderSeamError) ErrorReason() string {
	if a.e.Reason != "" {
		return a.e.Reason
	}
	// Guard refusals carry the human cause in Detail (newErr's format string).
	return a.e.Detail
}

func (a ladderSeamError) RefusedTransition() bool {
	return types.CodeClass[a.e.Code] == types.ErrClassPermanent
}

// adaptSeam wraps a write-seam error from the owning subsystem onto the shape
// the dashboard's error mapping consumes; an error with no ladder shape (a
// store fault, a context cancel) is returned unchanged so it stays the
// generic 500 action_failed.
func adaptSeam(err error) error {
	if err == nil {
		return nil
	}
	var le *ladder.Error
	if errors.As(err, &le) {
		return ladderSeamError{e: le}
	}
	return err
}
