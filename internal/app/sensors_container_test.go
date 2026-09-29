package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

func degradedSensor() types.SensorHealth {
	return types.SensorHealth{Sensor: "timers", Enabled: true, Degraded: true, Reason: "dependency dbus degraded"}
}

// TestSensorsContainerReason pins the four gates: the reason needs a container,
// a degraded sensor, and the reason slot to itself.
func TestSensorsContainerReason(t *testing.T) {
	cases := []struct {
		name      string
		in        types.HealthInputs
		container bool
		want      string
	}{
		{
			name:      "container with a degraded sensor and nothing else wrong",
			in:        types.HealthInputs{Sensors: []types.SensorHealth{degradedSensor()}},
			container: true,
			want:      reasonSensorsContainer,
		},
		{
			name:      "not a container: no marker, no claim",
			in:        types.HealthInputs{Sensors: []types.SensorHealth{degradedSensor()}},
			container: false,
			want:      "",
		},
		{
			name:      "container with every sensor healthy is not degraded at all",
			in:        types.HealthInputs{Sensors: []types.SensorHealth{{Sensor: "disk", Enabled: true}}},
			container: true,
			want:      "",
		},
		{
			name: "another degraded reason keeps the slot",
			in: types.HealthInputs{
				Sensors:         []types.SensorHealth{degradedSensor()},
				DegradedReasons: []string{"unstamped_build"},
			},
			container: true,
			want:      "",
		},
		{
			name: "a degraded hub runtime keeps the slot",
			in: types.HealthInputs{
				Sensors: []types.SensorHealth{degradedSensor()},
				Hub:     &types.HubStatus{Enabled: true, Degraded: true, DegradedReason: "redis_unavailable"},
			},
			container: true,
			want:      "",
		},
		{
			name:      "a stalled writer keeps the slot",
			in:        types.HealthInputs{Sensors: []types.SensorHealth{degradedSensor()}, Stalled: true},
			container: true,
			want:      "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sensorsContainerReason(tc.in, tc.container); got != tc.want {
				t.Errorf("sensorsContainerReason = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestInContainerReadsItsMarkers proves the detection reads the marker files and
// that a missing marker is never read as "container": the temp path in the
// second arm is real for neither runtime.
func TestInContainerReadsItsMarkers(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "dockerenv")
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	absent := filepath.Join(dir, "no-such-marker")
	orig := containerMarkers
	t.Cleanup(func() { containerMarkers = orig })

	containerMarkers = []string{absent}
	if inContainer() {
		t.Error("inContainer = true for a marker that does not exist")
	}

	containerMarkers = []string{absent, marker}
	if !inContainer() {
		t.Error("inContainer = false with the docker marker present")
	}

	// The marker is EVIDENCE, not a guess: with no marker path at all the
	// posture is not claimed, whatever the sensor batch looks like.
	containerMarkers = nil
	if inContainer() {
		t.Error("inContainer = true with no markers configured")
	}
}

// TestDeployReadmeDocumentsTheContainerDegradedPosture is the documentation gate
// for TRBL-077: the container section of deploy/README.md states the expected
// `status=degraded` / `detail.sensor_degraded=<sensor>` posture for the
// distroless image, says it is expected rather than a fault, and names the
// `sensors_container` reason the assembly adds (sensors_container.go). The
// operator-facing half of this row is the sentence; this test is what keeps it
// from being edited away.
func TestDeployReadmeDocumentsTheContainerDegradedPosture(t *testing.T) {
	root := repositoryRootFromThisTest(t)
	deploy := readRepositoryDocument(t, root, "deploy/README.md")

	const heading = "## Container quickstart (compose)"
	start := strings.Index(deploy, heading)
	if start < 0 {
		t.Fatalf("deploy/README.md has no %q section: the container posture this row documents has no home", heading)
	}
	section := deploy[start:]
	if end := strings.Index(section, "\n## "); end >= 0 {
		section = section[:end]
	}

	for _, want := range []string{
		"status=degraded",
		"detail.sensor_degraded=<sensor>",
		"not a fault",
		reasonSensorsContainer,
	} {
		if !strings.Contains(section, want) {
			t.Errorf("deploy/README.md's container section must state %q (TRBL-077)", want)
		}
	}
}
