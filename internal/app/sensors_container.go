package app

import (
	"os"

	"github.com/trouble-agent/trouble/internal/types"
)

// TRBL-077: the container posture's machine-readable reason.
//
// A container boot degrades its sensor batch by design. The image has no host
// systemd manager and no system D-Bus, so the `dbus` sensor degrades and the
// `timers` sensor that depends on it follows ("dependency dbus degraded") on
// every boot — /health.json has always reported that honestly
// (`status=degraded`, `detail.sensor_degraded=<sensor>`), but a first-time
// container operator reading it could not tell the designed posture from a real
// fault, because the container path carried no `detail.reason` at all. This
// reason names the posture, so the two cases are distinguishable without reading
// the docs (deploy/README.md "Container quickstart (compose)" states it too).
//
// Two rules keep the claim honest:
//
//   - The marker path is EVIDENCE, never a guess. The reason is claimed only when
//     the process is observed to run inside a container, from the files the
//     runtimes leave in the root filesystem (docker writes /.dockerenv, podman
//     writes /run/.containerenv). Nothing infers "container" from a systemd-less
//     environment: a plain host can be systemd-less too, and mislabelling that as
//     the designed posture would hide a real fault.
//   - The reason is claimed only when it is the ONLY cause of the alarm. A
//     degraded hub runtime, an unstamped build or a stalled writer keeps the
//     slot, because `detail.reason` carries one cause and those are the more
//     actionable ones.
const reasonSensorsContainer = "sensors_container"

// containerMarkers are the files the container runtimes leave behind. It is a
// package variable so the detection is testable without a container: the test
// points it at a temp file (missing markers must never claim the posture).
var containerMarkers = []string{"/.dockerenv", "/run/.containerenv"}

// inContainer reports whether this process runs inside a container, from the
// markers above. Absence of every marker means "not known to be a container".
func inContainer() bool {
	for _, p := range containerMarkers {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// sensorsContainerReason returns reasonSensorsContainer when the container
// posture is the only cause of the alarm: the process is in a container, at
// least one sensor is degraded, and nothing else already owns the reason slot
// (no degraded reason recorded, no degraded hub runtime, not stalled).
func sensorsContainerReason(in types.HealthInputs, container bool) string {
	if !container || in.Stalled || len(in.DegradedReasons) > 0 {
		return ""
	}
	if in.Hub != nil && in.Hub.Degraded {
		return ""
	}
	for _, s := range in.Sensors {
		if s.Degraded {
			return reasonSensorsContainer
		}
	}
	return ""
}
