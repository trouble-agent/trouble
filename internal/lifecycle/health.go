package lifecycle

import (
	"github.com/trouble-agent/trouble/internal/types"
)

// Health assembles the single HealthResponse (SPEC-12 §3.3).
func Health(cfg Config, in types.HealthInputs) types.HealthResponse {
	status := "ok"
	detail := make(map[string]any)

	if in.Stalled {
		status = "stalled"
	}

	for _, s := range in.Sensors {
		if s.Degraded {
			if status != "stalled" {
				status = "degraded"
			}
			detail["sensor_degraded"] = s.Sensor
		}
	}

	// Subsystems (SPEC-12 §3.3a): a subsystem the boot did not build is never
	// reported as healthy. The `subsystems` block is the per-row truth; `detail`
	// names one cause for the alarm line, exactly as sensor_degraded does.
	for _, s := range in.Subsystems {
		if s.Built {
			continue
		}
		if status != "stalled" {
			status = "degraded"
		}
		if s.Refused {
			detail["subsystem_refused"] = subsystemRefusalCode(s)
			detail["subsystem"] = s.Name
			continue
		}
		// Not built with no refusal recorded: the row still refuses to claim
		// the subsystem is up (a health surface served before the subsystems
		// land, or a build that left no reason).
		detail["subsystem_unbuilt"] = s.Name
	}

	if in.RW.RSSBytes > in.RSSWarnBytes && in.RSSWarnBytes > 0 {
		if status != "stalled" {
			status = "degraded"
		}
		detail["rss_over_budget"] = true
	}

	if in.GitSHA == "" || in.GitSHA == "unknown" {
		if status != "stalled" {
			status = "degraded"
		}
		detail["reason"] = "unstamped_build"
	}

	// The server profile (SPEC-13 §4.3): a degraded queue or archive tier
	// degrades the ONE health status and names its reason, so a hub that stopped
	// accepting is never reported as `ok` (rule 4 of SPEC-13 §1: degradation is a
	// designed state, never a silent success).
	if in.Hub != nil && in.Hub.Degraded {
		if status != "stalled" {
			status = "degraded"
		}
		if in.Hub.DegradedReason != "" {
			detail["reason"] = in.Hub.DegradedReason
		}
	}

	for _, r := range in.DegradedReasons {
		if status != "stalled" {
			status = "degraded"
		}
		detail["reason"] = r
	}

	return types.HealthResponse{
		Status:        status,
		Version:       in.Version,
		GitSHA:        in.GitSHA,
		BuildTime:     in.BuildTime,
		UptimeS:       in.UptimeS,
		LedgerLastSeq: in.LedgerLastSeq,
		LedgerLastTS:  in.LedgerLastTS,
		LedgerStallS:  in.LedgerStallS,
		Sensors:       in.Sensors,
		Sources:       in.Sources,
		Autonomy:      in.Autonomy,
		Breakers:      in.Breakers,
		RW:            in.RW,
		Hub:           in.Hub,
		Subsystems:    in.Subsystems,
		Detail:        detail,
	}
}

// subsystemRefusalCode is the code the health surface reports for a refused
// subsystem: its own error code when it carries one, else the reason text (a
// refusal recorded before the code convention still names its cause).
func subsystemRefusalCode(s types.SubsystemHealth) string {
	if s.Code != "" {
		return s.Code
	}
	return s.Reason
}
