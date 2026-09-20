package dashboard

import (
	"context"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// Health aggregation — SPEC-10 §2.9/§3.3. BuildHealth assembles the single
// HealthResponse shape that /health.json serves and the external stall checker
// (SPEC-12 §3) parses. It is the dashboard-side fallback aggregator: the
// composition root wires Deps.Health to lifecycle.Health, which owns the
// authoritative status, and BuildHealth exists so the same shape can be
// assembled from the same named producers without importing lifecycle
// (wave contract boundary 2).
//
// Two rules are enforced here:
//
//   - §6.13: a sensor with no sample yet reports enabled:true, degraded:true,
//     reason:"no sample yet" and is NEVER omitted — an absent entry reads as
//     "healthy" on a dashboard.
//   - ledger_stall_s comes from the lifecycle-provided value and is never
//     recomputed from wall-clock deltas.

// HealthDeps is the producer set for one health assembly. Every field is a
// function so the aggregator never caches a stale subsystem view.
type HealthDeps struct {
	Clock     func() time.Time // nil = time.Now
	StartTime time.Time        // process start, for uptime_s

	Version    func() (version, gitSHA, buildTime string, unstamped bool)
	Watermarks func() types.RuntimeWatermarks
	Sensors    func() []types.SensorHealth
	Sources    func() []types.SourceLiveness
	Breakers   func() []types.Breaker
	Autonomy   func() types.AutonomyGates
	// Subsystems is the built/refused block (SPEC-12 §3.3a). nil means the
	// caller has no subsystem view at all: no rows are invented, and the status
	// is left to the other producers rather than guessed.
	Subsystems func() []types.SubsystemHealth

	LedgerSeq    func() uint64
	LedgerTS     func() string
	LedgerStallS func() float64 // the lifecycle-provided stall, in seconds

	// StallMaxS is the ledger stall beyond which status is "stalled"
	// (lifecycle.max_seq_age; 30s when zero).
	StallMaxS float64
	// Stalled forces status "stalled" (the writer's own lag beyond its cap).
	Stalled bool
	// DegradedReasons lists machine-readable causes (e.g. "unstamped_build");
	// any entry makes the response "degraded".
	DegradedReasons []string
	// RSSWarnBytes degrades the response when RSS exceeds it (0 = off).
	RSSWarnBytes int64
}

// Health statuses (SPEC-10 §2.9, SPEC-12 §3.3).
const (
	StatusOK       = "ok"
	StatusDegraded = "degraded"
	StatusStalled  = "stalled"
)

// BuildHealth assembles the §2.9 HealthResponse. A cancelled context returns
// the context's error and no partial response.
func BuildHealth(ctx context.Context, deps HealthDeps) (types.HealthResponse, error) {
	if err := ctx.Err(); err != nil {
		return types.HealthResponse{}, err
	}
	now := time.Now
	if deps.Clock != nil {
		now = deps.Clock
	}

	var out types.HealthResponse
	if deps.Version != nil {
		out.Version, out.GitSHA, out.BuildTime, _ = deps.Version()
	}
	if !deps.StartTime.IsZero() {
		out.UptimeS = now().Sub(deps.StartTime).Seconds()
		if out.UptimeS < 0 {
			out.UptimeS = 0
		}
	}
	if deps.LedgerSeq != nil {
		out.LedgerLastSeq = deps.LedgerSeq()
	}
	if deps.LedgerTS != nil {
		out.LedgerLastTS = deps.LedgerTS()
	}
	if deps.LedgerStallS != nil {
		out.LedgerStallS = deps.LedgerStallS()
	}
	out.Sensors = normalizeSensors(deps.Sensors)
	out.Sources = normalizeSources(deps.Sources)
	if deps.Autonomy != nil {
		out.Autonomy = deps.Autonomy()
	}
	if deps.Breakers != nil {
		out.Breakers = deps.Breakers()
	}
	if deps.Watermarks != nil {
		out.RW = deps.Watermarks()
	}
	if deps.Subsystems != nil {
		out.Subsystems = deps.Subsystems()
	}

	reasons := append([]string(nil), deps.DegradedReasons...)
	if deps.RSSWarnBytes > 0 && out.RW.RSSBytes > deps.RSSWarnBytes {
		reasons = append(reasons, "rss_above_warn")
	}
	// An omitted version reads as a healthy build on a strip; say so instead.
	if out.Version == "" && out.GitSHA == "" {
		reasons = append(reasons, "unstamped_build")
	}
	// A subsystem that is not built is never healthy (§3.3a): the same rule the
	// lifecycle assembly applies, so the two assemblers cannot disagree.
	refused := ""
	for _, s := range out.Subsystems {
		if s.Built {
			continue
		}
		if s.Refused && refused == "" {
			refused = s.Name
		}
	}

	// Status precedence: stalled beats degraded (a stalled writer is the more
	// severe, more actionable state).
	stallMax := deps.StallMaxS
	if stallMax <= 0 {
		stallMax = 30
	}
	stalled := deps.Stalled || out.LedgerStallS >= stallMax
	degraded := len(reasons) > 0 || anySensorDegraded(out.Sensors) || anySourceDead(out.Sources) || anySubsystemUnbuilt(out.Subsystems)
	switch {
	case stalled:
		out.Status = StatusStalled
	case degraded:
		out.Status = StatusDegraded
	default:
		out.Status = StatusOK
	}
	if len(reasons) > 0 || refused != "" {
		detail := map[string]any{}
		if len(reasons) > 0 {
			detail["reasons"] = reasons
		}
		if refused != "" {
			detail["subsystem"] = refused
		}
		out.Detail = detail
	}
	return out, nil
}

// anySubsystemUnbuilt reports whether any row of the §3.3a block fails to claim
// a live subsystem: refused rows and rows whose build never happened both count.
func anySubsystemUnbuilt(subs []types.SubsystemHealth) bool {
	for _, s := range subs {
		if !s.Built {
			return true
		}
	}
	return false
}

// normalizeSensors applies the §6.13 "no sample yet" rule and never omits an
// entry.
func normalizeSensors(src func() []types.SensorHealth) []types.SensorHealth {
	if src == nil {
		return nil
	}
	in := src()
	out := make([]types.SensorHealth, 0, len(in))
	for _, sh := range in {
		if sh.LastSuccessTS == "" && sh.LastEventTS == "" {
			sh.Enabled = true
			sh.Degraded = true
			if sh.Reason == "" {
				sh.Reason = "no sample yet"
			}
		}
		out = append(out, sh)
	}
	return out
}

// normalizeSources passes the per-source liveness expectations through
// unchanged: SourceLiveness carries no enabled/degraded/reason fields, so the
// §6.13 wording is applied where it is expressible (SensorHealth). A source
// that never reported stays visible with alive=false rather than being
// dropped.
func normalizeSources(src func() []types.SourceLiveness) []types.SourceLiveness {
	if src == nil {
		return nil
	}
	return src()
}

func anySensorDegraded(sensors []types.SensorHealth) bool {
	for _, s := range sensors {
		if s.Degraded {
			return true
		}
	}
	return false
}

func anySourceDead(sources []types.SourceLiveness) bool {
	for _, s := range sources {
		if s.Expected && !s.Alive {
			return true
		}
	}
	return false
}
