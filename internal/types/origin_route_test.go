package types

// origin_route_test.go — AC-28 (TRBL-061): Origin.Route rides the record JSON
// only when a route was stamped.
//
// The property this guards is SPEC-TYPES §7.6's no-schema-change guarantee:
// every producer that never heard of the sensor transport marshals
// byte-identically to its pre-AC-28 form (route absent from the JSON), while
// a sensor-written record carries `"route":"A"|"B"` in the §3.1 example's
// shape.
import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOriginRouteAbsentFromNonSensorJSON(t *testing.T) {
	b, err := json.Marshal(Origin{HostID: "7f3a91c2d4e5b607", Source: "hub"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), `"route"`) {
		t.Errorf("non-sensor origin marshals %s, want no route key at all", b)
	}
	var back Origin
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Route != "" {
		t.Errorf("route round-tripped to %q, want empty", back.Route)
	}
}

func TestOriginRouteOnSensorRecordJSON(t *testing.T) {
	for _, route := range []string{"A", "B"} {
		b, err := json.Marshal(Origin{HostID: "7f3a91c2d4e5b607", HubID: "", Source: "sentinel:payment-worker", Route: route})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		want := `{"host_id":"7f3a91c2d4e5b607","hub_id":"","source":"sentinel:payment-worker","route":"` + route + `"}`
		if string(b) != want {
			t.Errorf("origin JSON = %s, want %s (§3.1's example shape)", b, want)
		}
	}
}
