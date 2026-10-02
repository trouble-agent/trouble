package sensors

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// substrate_test.go is the SPEC-03 §3.2a battery (TRBL-078): the detection
// plane self-detects the substrate it boots on, and a container profile whose
// sensor facilities are absent files ONE posture record instead of the
// per-subsystem failure records every fresh container install used to ship.

// pinContainer marks the process as running on a container substrate for the
// duration of the test (the runtime markers are package vars: a container
// cannot be provisioned from a unit test, so the evidence is pointed at a
// temp file — the same seam container_posture_test uses in internal/app).
func pinContainer(t *testing.T) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), ".dockerenv")
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	old := containerMarkerPaths
	containerMarkerPaths = []string{marker}
	t.Cleanup(func() { containerMarkerPaths = old })
}

// TestSubstrateDetectionRequiresEvidence: no marker, no verdict — a host that
// merely lacks a socket is NEVER labelled a container (the TRBL-077 rule,
// applied to the detection plane).
func TestSubstrateDetectionRequiresEvidence(t *testing.T) {
	old := containerMarkerPaths
	containerMarkerPaths = nil
	t.Cleanup(func() { containerMarkerPaths = old })

	f := detectSubstrate()
	if f.Container {
		t.Fatal("no marker present: the substrate verdict must be false, never guessed")
	}
	if len(f.Absent) != 0 {
		t.Fatalf("a host substrate claims no absent facilities, got %v", f.Absent)
	}
}

// TestSubstrateDetectionContainerEvidence: the marker decides, the overlay
// root corroborates, and the absent list names the probed facilities.
func TestSubstrateDetectionContainerEvidence(t *testing.T) {
	pinContainer(t)
	f := detectSubstrate()
	if !f.Container {
		t.Fatal("the docker marker is present: the verdict must be container")
	}
	if len(f.Evidence) == 0 {
		t.Fatal("a container verdict carries its evidence")
	}
	markerSeen := false
	for _, e := range f.Evidence {
		if strings.HasPrefix(e, "marker:") {
			markerSeen = true
		}
	}
	if !markerSeen {
		t.Fatalf("evidence must name the marker, got %v", f.Evidence)
	}
}

// pinAbsentFacilities points the two facility probes at empty path lists (a
// distroless container has neither a journalctl binary nor any socket): the
// probes are package vars, the same seam the runtime markers use.
func pinAbsentFacilities(t *testing.T) {
	t.Helper()
	oldJ := journalSocketPaths
	oldD := dbusSystemSocketPaths
	journalSocketPaths = nil
	dbusSystemSocketPaths = nil
	t.Setenv("PATH", "")
	t.Setenv("DBUS_SYSTEM_BUS_ADDRESS", "")
	t.Cleanup(func() {
		journalSocketPaths = oldJ
		dbusSystemSocketPaths = oldD
	})
}

// TestContainerSubstrateProbeCarriesOneCode: the boot probe's payload carries
// the substrate object and the ONE 026 code; it must not carry a code per
// absent subsystem.
func TestContainerSubstrateProbeCarriesOneCode(t *testing.T) {
	pinContainer(t)
	pinAbsentFacilities(t)
	h := newHarness(t)
	if err := h.s.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	pr := h.s.probe.Load()
	if pr == nil {
		t.Fatal("no probe result stored")
	}
	if !pr.Substrate.Container {
		t.Fatal("the probe must have observed the pinned container substrate")
	}
	if len(pr.Substrate.Absent) == 0 {
		t.Fatal("a pinned container without journal/dbus fixtures must report absent facilities")
	}
	count := 0
	for _, c := range pr.Codes {
		if c == types.CodeSensors026 {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("TROUBLE-SENSORS-026 must appear exactly once, got %d", count)
	}
	sub, ok := pr.Payload["substrate"].(map[string]any)
	if !ok {
		t.Fatalf("the probe payload must carry the substrate object, got %T", pr.Payload["substrate"])
	}
	if c, _ := sub["container"].(bool); !c {
		t.Fatalf("substrate.container must be true, got %v", sub["container"])
	}
	if ab, _ := sub["absent"].([]string); len(ab) == 0 {
		t.Fatalf("substrate.absent must name the absent facilities, got %v", sub["absent"])
	}
}

// TestContainerSubstrateOneRecordNotEight is the row's acceptance shape: a
// boot on a container substrate with no journal and no D-Bus files ONE
// substrate posture record plus the capability_probe record — not one failure
// record per absent subsystem. The before-behaviour (a 006 journald failure
// record, a 013 dbus failure record) must be GONE.
func TestContainerSubstrateOneRecordNotEight(t *testing.T) {
	pinContainer(t)
	pinAbsentFacilities(t)
	h := newHarness(t)
	ctx := context.Background()
	if err := h.s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	h.s.diskSweep(ctx) // the disk sensor WORKS on a container: it keeps recording
	h.s.timerSweep(ctx)
	h.s.stopDisk()

	recs := h.snapshot()
	kinds := map[string]int{}
	for _, r := range recs {
		k, _ := r.Payload["kind"].(string)
		kinds[k]++
	}
	if kinds["container_substrate"] != 1 {
		t.Fatalf("exactly ONE container_substrate record, got %d (all records: %d, kinds: %v)",
			kinds["container_substrate"], len(recs), kinds)
	}
	if kinds["sensor_error"] != 0 {
		t.Fatalf("a container substrate boot must file NO per-subsystem sensor_error records, got %d", kinds["sensor_error"])
	}
	// The one record names what is absent, carries the code, and never fires.
	var sub *types.Record
	for i := range recs {
		if k, _ := recs[i].Payload["kind"].(string); k == "container_substrate" {
			sub = &recs[i]
		}
	}
	if sub == nil {
		t.Fatal("unreachable: the count above found the record")
	}
	if fire, _ := sub.Payload["fire"].(bool); fire {
		t.Fatal("the substrate posture record must never fire")
	}
	if code, _ := sub.Payload["error_code"].(string); code != string(types.CodeSensors026) {
		t.Fatalf("the substrate record carries TROUBLE-SENSORS-026, got %q", code)
	}
	absent, _ := sub.Payload["absent"].([]string)
	if len(absent) == 0 {
		t.Fatal("the substrate record names the absent facilities")
	}
	for _, a := range absent {
		if a != "journald" && a != "dbus" {
			t.Fatalf("unexpected absent facility %q", a)
		}
	}
}

// TestContainerSubstrateGatedSensorsHealth: the gated sensors carry the 026
// substrate reason (Enabled:false like every documented no-op — the spec keeps
// disabled sensors out of the sensors[] array, so the reason is asserted on
// the runtime, the same surface TestJournaldMissingBinaryDisablesOnlyItself
// reads) — and the disk sensor stays enabled because its facility (statfs on a
// visible mount) exists on every container.
func TestContainerSubstrateGatedSensorsHealth(t *testing.T) {
	pinContainer(t)
	pinAbsentFacilities(t)
	h := newHarness(t)
	ctx := context.Background()
	if err := h.s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.s.Stop(ctx)
	for _, k := range []types.SensorKind{types.SenJournald, types.SenDBus} {
		rt := h.s.rt[k]
		if rt == nil {
			t.Fatalf("%s has no runtime row at all", k)
		}
		if rt.enabled.Load() {
			t.Fatalf("%s must not start on a substrate without its facility", k)
		}
		reason := *rt.reason.Load()
		if !strings.Contains(reason, string(types.CodeSensors026)) {
			t.Fatalf("%s reason must carry TROUBLE-SENSORS-026, got %q", k, reason)
		}
		if !strings.Contains(reason, "container substrate") {
			t.Fatalf("%s reason must name the substrate, got %q", k, reason)
		}
	}
	disk := h.s.rt[types.SenDisk]
	if disk == nil || !disk.enabled.Load() {
		t.Fatal("the disk sensor works on a container and must stay enabled")
	}
	// Health() itself stays at the enabled set (the spec's sensors[] contract),
	// so the gated sensors must not appear as phantom rows.
	for _, sh := range h.s.Health() {
		if sh.Sensor == types.SenJournald || sh.Sensor == types.SenDBus {
			t.Fatalf("%s must be absent from Health() while disabled, got %+v", sh.Sensor, sh)
		}
	}
}

// TestHostSubstrateEmitsNothingNew is the anti-regression half: a HOST boot
// (no markers) writes no substrate record and gains no new records of any
// kind — the change is invisible on the substrate that never had the noise.
func TestHostSubstrateEmitsNothingNew(t *testing.T) {
	old := containerMarkerPaths
	containerMarkerPaths = nil
	t.Cleanup(func() { containerMarkerPaths = old })

	h := newHarness(t)
	ctx := context.Background()
	if err := h.s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	h.s.stopDisk()
	for _, r := range h.snapshot() {
		if k, _ := r.Payload["kind"].(string); k == "container_substrate" {
			t.Fatal("a host boot must never write a container_substrate record")
		}
	}
	if c := h.recordsWhere(func(r types.Record) bool {
		return r.Payload["error_code"] == string(types.CodeSensors026)
	}); len(c) != 0 {
		t.Fatalf("a host boot must never carry TROUBLE-SENSORS-026, got %d records", len(c))
	}
}

// TestContainerSubstrateLedgerCensus is the row's before/after census, on the
// harness fixture: a container boot with absent facilities must be leaded by
// ONE substrate record (plus probe/canary producers), NOT the ~8 per-subsystem
// failure records the row measured on dogfood 09-25 — and the disk sensor
// (which works there) keeps its kinds while the gated ones contribute zero.
// The before-count is pinned by the fixture itself: with the gates MUTE (the
// posture not observed) the same boot shape files a 006 sensor_error record
// (TestRedBeforeNoSubstrateAwareness proved that against the pre-change blobs
// at HEAD), so after the fix the same fixture carries zero 006 and one 026.
func TestContainerSubstrateLedgerCensus(t *testing.T) {
	pinContainer(t)
	pinAbsentFacilities(t)
	h := newHarness(t, types.ConfigValue{Key: "sensors.disk.mounts", Value: []string{"/"}})
	ctx := context.Background()
	if err := h.s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	h.s.diskSweep(ctx)
	h.s.timerSweep(ctx)
	h.s.stopDisk()

	kinds := h.kinds()
	if kinds[types.KGap] != 1 {
		t.Fatalf("exactly ONE gap record (the substrate posture), got %d", kinds[types.KGap])
	}
	if kinds["sensor_error_count"] > 0 {
		t.Fatal("unreachable")
	}
	errs := h.recordsWhere(func(r types.Record) bool {
		return r.Payload["kind"] == "sensor_error"
	})
	if len(errs) != 0 {
		t.Fatalf("0 per-subsystem sensor_error records on the container posture, got %d: %v",
			len(errs), len(errs))
	}
	for _, r := range errs {
		if c, _ := r.Payload["error_code"].(string); c == string(types.CodeSensors006) {
			t.Fatalf("a 006 record survived the gates: %+v", r.Payload)
		}
	}
	if kinds[types.KEvent] > 2 {
		// Events on this fixture: the capability_probe + the disk sweep's own
		// kinds only. Before the change the SAME calls produced the probe
		// record, a 006 failure event, and the disk kinds with the timers
		// dependency note on top. The census asserts the GATED subsystems
		// contribute zero, so the disk kinds are counted by signature source.
		bysrc := map[string]int{}
		for _, r := range h.snapshot() {
			if r.Kind != types.KEvent {
				continue
			}
			src := r.Origin.Source
			bysrc[src]++
		}
		for _, src := range []string{"journald", "dbus", "timers"} {
			if n := bysrc[src]; n > 0 {
				t.Fatalf("gated subsystem %s must file 0 event records on the container posture, got %d (by source: %v)", src, n, bysrc)
			}
		}
		if psi := bysrc["psi"]; psi != 1 {
			t.Fatalf("exactly 1 psi event (the capability probe), got %d", psi)
		}
	}
	notes := h.recordsWhere(func(r types.Record) bool {
		return r.Payload["kind"] == "sensor_note"
	})
	for _, r := range notes {
		if d, _ := r.Payload["detail"].(string); strings.Contains(d, "dependency dbus degraded") {
			t.Fatal("the timers dependency degradation must not be filed on the container posture")
		}
	}
}
