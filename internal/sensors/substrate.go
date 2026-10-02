package sensors

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// Substrate self-detection (TRBL-078, SPEC-03 §3.2a).
//
// A container profile boots on a substrate where several sensor facilities
// CANNOT exist: there is no host systemd journal to follow, no system D-Bus
// socket to watch, and (as a consequence) no timer units. Before this change
// every fresh container boot still filed one failure record per missing
// subsystem (TROUBLE-SENSORS-006/013 event records plus the timers dependency
// degradation), so the first page of every fresh install's ledger was noise —
// eight records carrying fire=false that a operator had to learn to ignore
// before finding a real incident.
//
// The rule here is the same one TRBL-077 put on the health reason, applied to
// the detection plane: the container posture is EVIDENCE, never a guess. The
// runtime markers the container engines leave in the root filesystem
// (/.dockerenv, /run/.containerenv) decide that the substrate IS a container;
// nothing infers "container" from a missing socket, because a plain host can
// lack a socket too and mislabelling that would hide a real fault. The absent
// facilities are then probed individually, and each gate in Start keys on the
// FACILITY (not on the container verdict): a container that does ship a
// journal or a system bus keeps its journald and dbus sensors, exactly as a
// container with a real mount keeps its working disk sensor.

// containerMarkerPaths are the files the container runtimes leave behind. A
// package var so the detection is testable without a container.
var containerMarkerPaths = []string{"/.dockerenv", "/run/.containerenv"}

// journalSocketPaths are where a systemd journal presents its socket. Any hit
// means a journal facility exists even if no journalctl binary does.
var journalSocketPaths = []string{
	"/run/systemd/journal/socket",
	"/run/systemd/journald.sock",
}

// dbusSystemSocketPaths are where a system D-Bus presents its socket.
var dbusSystemSocketPaths = []string{
	"/run/dbus/system_bus_socket",
	"/var/run/dbus/system_bus_socket",
}

// substrateFacts is the probe's read of the substrate: the container verdict
// with the evidence that produced it, and the sensor facilities that are
// absent on this substrate (each one individually probed, never inferred).
type substrateFacts struct {
	Container bool
	Evidence  []string
	Absent    []string
}

// anyPathExists reports whether any of paths exists.
func anyPathExists(paths []string) bool {
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// journaldFacilityPresent reports whether a journal to follow exists on this
// substrate: the journalctl binary, or one of the journal sockets.
func journaldFacilityPresent() bool {
	if _, err := exec.LookPath("journalctl"); err == nil {
		return true
	}
	return anyPathExists(journalSocketPaths)
}

// dbusSystemFacilityPresent reports whether a system D-Bus exists on this
// substrate: a well-known socket path, or an explicit bus address override.
func dbusSystemFacilityPresent() bool {
	if os.Getenv("DBUS_SYSTEM_BUS_ADDRESS") != "" {
		return true
	}
	return anyPathExists(dbusSystemSocketPaths)
}

// detectSubstrate reads the substrate facts (SPEC-03 §3.2a). The container
// verdict comes only from the runtime markers; the absent list comes from the
// facility probes. On a non-container substrate nothing is claimed and the
// absent list stays empty — facility gaps there are exactly what the
// per-sensor start paths have always reported, and stay that way.
func detectSubstrate() substrateFacts {
	var f substrateFacts
	for _, p := range containerMarkerPaths {
		if _, err := os.Stat(p); err == nil {
			f.Container = true
			f.Evidence = append(f.Evidence, "marker:"+p)
		}
	}
	if !f.Container {
		return f
	}
	// Corroboration, recorded but never load-bearing: an overlay root is what
	// a container's / looks like, and naming it in the record makes the
	// substrate claim auditable from the ledger alone.
	if ft, ok := readMounts()["/"]; ok && strings.HasPrefix(ft, "overlay") {
		f.Evidence = append(f.Evidence, "root fstype="+ft)
	}
	if !journaldFacilityPresent() {
		f.Absent = append(f.Absent, "journald")
	}
	if !dbusSystemFacilityPresent() {
		f.Absent = append(f.Absent, "dbus")
	}
	return f
}

// substrate returns the boot probe's substrate facts. Before a probe result
// exists (a direct Start in a test, or an out-of-band construction) the facts
// are read live — the same evidence, one call.
func (s *Sensors) substrate() substrateFacts {
	if pr := s.probe.Load(); pr != nil {
		return pr.Substrate
	}
	return detectSubstrate()
}

// containerSubstrate reports whether the boot probe observed a container
// substrate (SPEC-03 §3.2a). Only the markers decide.
func (s *Sensors) containerSubstrate() bool {
	return s.substrate().Container
}

// substrateSensorReason builds the /health reason for a sensor whose facility
// is absent on the container substrate: the code, the named facility, never a
// bare "unavailable".
func substrateSensorReason(code types.ErrorCode, facility string) string {
	return string(code) + ": container substrate: " + facility + " is absent (TRBL-078)"
}

// substratePayload is the probe record's substrate object (SPEC-03 §3.2a).
func (s *Sensors) substratePayload(f substrateFacts) map[string]any {
	out := map[string]any{
		"container": f.Container,
		"evidence":  f.Evidence,
		"absent":    f.Absent,
	}
	if f.Container {
		out["kind"] = "container"
	} else {
		out["kind"] = "host"
	}
	return out
}

// emitSubstrateRecord writes the ONE container-substrate posture record beside
// the capability_probe record (SPEC-03 §3.2a): kind=container_substrate, a
// gap-shaped payload that names every absent facility and every piece of
// evidence, fire=false, sig_keyed=false — never an incident candidate, never
// per-subsystem noise. It is written only when the probe observed the posture;
// a host boot writes nothing and sees no new records at all.
func (s *Sensors) emitSubstrateRecord(ctx context.Context, f substrateFacts, now time.Time) {
	if !f.Container || len(f.Absent) == 0 {
		return
	}
	payload := map[string]any{
		"kind":         "container_substrate",
		"ts":           types.FormatUTC(now),
		"sensor":       string(types.SenPSI),
		"scope":        "substrate",
		"from_ts":      types.FormatUTC(now),
		"to_ts":        types.FormatUTC(now),
		"est_lost":     0,
		"cause":        "container_substrate",
		"cause_detail": "container substrate: absent sensor facilities named in absent; evidence in evidence (TRBL-078)",
		"subsystem":    "sensors:substrate",
		"absent":       f.Absent,
		"evidence":     f.Evidence,
		"sig_keyed":    false,
		"fire":         false,
		"error_code":   string(types.CodeSensors026),
	}
	d := types.RecordDraft{
		Kind:    types.KGap,
		Sig:     sigFor(types.SrcPSI, "substrate", "container").String(),
		Origin:  types.Origin{HostID: s.hostID, HubID: s.cfg.hubID, Source: string(types.SrcPSI)},
		Actor:   s.actor,
		Payload: payload,
	}
	if _, err := s.emit(ctx, d); err != nil {
		return
	}
}
