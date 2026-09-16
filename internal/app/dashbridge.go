package app

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/dashboard"
	"github.com/totalwindupflightsystems/trouble/internal/ladder"
	"github.com/totalwindupflightsystems/trouble/internal/ledger"
	"github.com/totalwindupflightsystems/trouble/internal/lifecycle"
	"github.com/totalwindupflightsystems/trouble/internal/types"
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
// SPEC-05's frozen 19-state machine has no `acknowledged` and no human-close
// edge (the legal edges into `resolved` are T38 window_passed from `verifying`
// and T45 quiet_close from `suppressed`). The adapters below therefore do the
// two things the spec's own semantics define and record the difference:
// an ack is a sig-scoped hold until the given time (SPEC-05 §3's suppression)
// plus its audit record, and a close walks the ladder's own edge into `resolved`
// when the incident is in a state that has one, refusing otherwise instead of
// inventing a transition. Both are recorded with the human actor.

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

// Ack records the human acknowledgement and applies the hold the `until` means.
func (a Actions) Ack(ctx context.Context, inc string, actor types.Actor, reason string, until types.Duration) error {
	cur, err := a.Ladder.Incident(ctx, inc)
	if err != nil {
		return err
	}
	ctx = WithActor(ctx, actor)
	payload := map[string]any{
		"transition": "acknowledged",
		"reason":     reason,
		"until":      string(until),
		"via":        "dashboard",
	}
	if _, err := a.Store.Append(ctx, types.KIncident, cur.Sig, inc, payload); err != nil {
		return err
	}
	if until != "" && until.Std() > 0 {
		if _, err := a.Ladder.Suppress(ctx, cur.Sig, until, reason); err != nil {
			return err
		}
	}
	return nil
}

// Close resolves the incident through the ladder's own edge and records the
// resolution. An incident with no legal edge into `resolved` is refused: the
// state machine is data and the composition root does not widen it.
func (a Actions) Close(ctx context.Context, inc string, actor types.Actor, reason, resolution string) error {
	cur, err := a.Ladder.Incident(ctx, inc)
	if err != nil {
		return err
	}
	if _, ok := ladder.RefuseFor(cur.State, types.StResolved); ok {
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
		return err
	}
	_, err = a.Store.Append(ctx, types.KIncident, cur.Sig, inc, map[string]any{
		"transition": "closed",
		"reason":     reason,
		"resolution": resolution,
		"via":        "dashboard",
	})
	return err
}

func (a Actions) seq() uint64 {
	if a.Store == nil || a.Store.L == nil {
		return 0
	}
	return a.Store.L.DashReader().LastSeq()
}

// closeTrigger names the ladder's own edge for the state the incident is in.
func closeTrigger(st types.LadderState) string {
	switch st {
	case types.StVerifying:
		return "window_passed" // T38
	case types.StSuppressed:
		return "quiet_close" // T45
	default:
		return ""
	}
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

// errNoCloseEdge is the refusal a human close gets when the incident is not in a
// state the frozen state machine can leave for `resolved`. It names both states
// so the UI can say why instead of "failed".
func errNoCloseEdge(st types.LadderState) error {
	return &noCloseEdge{state: st}
}

type noCloseEdge struct{ state types.LadderState }

func (e *noCloseEdge) Error() string {
	return "TROUBLE-LADDER-012: incident state " + string(e.state) +
		" has no legal edge to resolved; a human close is defined from verifying (T38) or suppressed (T45)"
}
