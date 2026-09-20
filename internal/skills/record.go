package skills

import (
	"context"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// Writer is SPEC-01's single-writer append port (§2). A *ledger.Ledger satisfies
// it directly.
type Writer interface {
	Append(ctx context.Context, d types.RecordDraft) (types.Record, error)
}

// Clock is SPEC-12's dual clock (§2 via SPEC-INDEX §6.5): wall for persisted
// timestamps, monotonic for every window that must survive a clock jump.
type Clock interface {
	Now() time.Time
	Monotonic() time.Duration
}

// Deps carries every collaborator the skills package needs. Nothing here shells
// out except the puller's argv-only git, and nothing here opens a socket.
//
// Additions to the §2 surface, recorded in the run report: `StateRoot` (the
// skills-local tree's parent, which SPEC-12 owns), `Registered` (the descriptor
// names GateModules checks against, which SPEC-06 owns) and `HostID`/`Actor`/
// `DaemonVersion` (the Origin/Actor every record needs and the §4.3 floor input).
type Deps struct {
	Ledger    Writer
	Clock     Clock
	HostID    string
	Actor     types.Actor
	StateRoot string
	// DaemonVersion is the ldflags-stamped version (§4.3). An empty value is
	// treated as 0.0.0-dev, which fails every floor.
	DaemonVersion string
	// Registered lists the descriptor names this build ships. An empty list
	// disables the "module is registered" half of GateModules (used by unit tests
	// and by an early boot order).
	Registered func() []string
}

// systemClock is the Clock used when none is injected.
type systemClock struct{ t0 time.Time }

func (c *systemClock) Now() time.Time {
	if c.t0.IsZero() {
		c.t0 = time.Now()
	}
	return time.Now()
}

func (c *systemClock) Monotonic() time.Duration {
	if c.t0.IsZero() {
		c.t0 = time.Now()
	}
	return time.Since(c.t0)
}

func (d *Deps) now() time.Time {
	if d.Clock == nil {
		d.Clock = &systemClock{}
	}
	return d.Clock.Now()
}

func (d *Deps) hostID() string {
	if d.HostID == "" {
		return "unknown"
	}
	return d.HostID
}

func (d *Deps) daemonVersion() string {
	if d.DaemonVersion == "" {
		return "0.0.0-dev"
	}
	return d.DaemonVersion
}

// Version is the fallback stamped version (SPEC-12 sets Deps.DaemonVersion; this
// exists for a caller that only has the package-level value).
var Version = "0.0.0-dev"

// origin is the provenance on every skill record (SPEC-11 §4.7).
func (d *Deps) origin() types.Origin {
	return types.Origin{HostID: d.hostID(), Source: "skills"}
}

// record writes one `kind=skill` record. It is the only writer path in this
// package: a state change without a record never happens, and the callers that
// must be atomic with an install call it before the rename (§6 edge case 9).
func (d *Deps) record(ctx context.Context, sig, inc string, payload map[string]any) (types.Record, error) {
	if d.Ledger == nil {
		return types.Record{}, nil
	}
	return d.Ledger.Append(ctx, types.RecordDraft{
		Kind:    types.KSkill,
		Sig:     sig,
		Inc:     inc,
		Origin:  d.origin(),
		Actor:   d.Actor,
		Payload: payload,
	})
}

// phase names one ledger phase of §4.7.
const (
	PhaseCandidateDrafted  = "candidate_drafted"
	PhaseCandidateReviewed = "candidate_reviewed"
	PhasePromoted          = "promoted"
	PhaseDemoted           = "demoted"
	PhasePulled            = "pulled"
	PhaseInstalled         = "installed"
	PhasePendingReview     = "pending_review"
	PhaseRefused           = "refused"
	PhaseConflict          = "conflict"
	PhaseHold              = "hold"
	PhaseHoldExpired       = "hold_expired"
	PhaseCanaryOK          = "canary_ok"
	PhaseCanaryFailed      = "canary_failed"
	PhaseStats             = "stats"
	PhasePullFailed        = "pull_failed"
	// The local SKILL.md library (SPEC-11 §4.7a, v0.1.1b): a scan and one record
	// per refused file and per executed step.
	PhaseLibraryLoaded  = "library_loaded"
	PhaseLibraryRefused = "library_refused"
	PhaseStepExecuted   = "step_executed"
)

// phaseRecord writes one phase record with its required payload fields (§4.7).
func (d *Deps) phaseRecord(ctx context.Context, phase, sig, inc string, payload map[string]any) (types.Record, error) {
	if payload == nil {
		payload = map[string]any{}
	}
	payload["phase"] = phase
	if _, ok := payload["error_code"]; !ok {
		payload["error_code"] = ""
	}
	return d.record(ctx, sig, inc, payload)
}
