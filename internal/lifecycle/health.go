package lifecycle

import (
	"github.com/totalwindupflightsystems/trouble/internal/types"
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
		Detail:        detail,
	}
}
