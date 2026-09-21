package app

// routes_wiring_test.go — AC-28 (TRBL-061): the composition root's half. The
// resolved [sentinel.routes] keys reach the daemon's config (and through it
// the sentinel's boot validation); a route policy the sentinel refuses
// becomes the §3.3a sentinel refusal row with its code and the key it names;
// a policy it accepts leaves a live ingest listener whose landed records
// carry the route that carried them.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// routesWiringPubKey is a valid project key for the boot tests (SPEC-04
// §3.1: exactly 32 lowercase hex).
var routesWiringPubKey = strings.Repeat("ab", 16)

// routesWiringProject is the one enabled project these boots ingest against.
func routesWiringProject() types.Project {
	return types.Project{
		ID: "1", Slug: "alpha", PublicKey: routesWiringPubKey,
		QuotaEPM: 600, Enabled: true, LossPolicy: types.LossDropCounter, DiskBudget: 1 << 31,
	}
}

// TestRouteTableResolvesThroughARealBoot: the file's [sentinel.routes] table
// resolves into the daemon's config through the ordinary boot path — the leaf
// keys are known to the registry (a unknown-key refusal here would fail the
// boot itself) and the typed values carry the operator's declaration.
func TestRouteTableResolvesThroughARealBoot(t *testing.T) {
	h := bootWithExtraTables(t, `
[sentinel.routes]
default = "auto"
per_class = { "psi:io_pressure" = "direct" }
`)
	if h.d == nil {
		t.Fatal("daemon did not boot")
	}
	if got := h.d.Cfg.Routes.Default; got != "auto" {
		t.Errorf("resolved routes.default = %q, want auto", got)
	}
	if got := h.d.Cfg.Routes.PerClass["psi:io_pressure"]; got != "direct" {
		t.Errorf("resolved per_class[psi:io_pressure] = %q, want direct", got)
	}
	if len(h.d.Cfg.Routes.PerClass) != 1 {
		t.Errorf("resolved per_class has %d entries, want 1", len(h.d.Cfg.Routes.PerClass))
	}
}

// TestRouteStampOnALiveIngestListener: on a hub-less boot (the stock
// topology: hub.mode=hub, no hub.url) the built sentinel's ingest listener
// accepts an event and the landed record carries origin.route="A" — the
// resolution the composition root wired is live, not a config field (§3.10a:
// with no hub endpoint every class takes A, and the A case is stamped).
func TestRouteStampOnALiveIngestListener(t *testing.T) {
	h := bootDaemonWith(t, SubsystemOptions{
		SentinelProjects: []types.Project{routesWiringProject()},
	})
	d := h.d
	if d.Subsystems.Sentinel == nil {
		t.Fatal("sentinel not built on a stock boot")
	}
	// The stock topology has no hub endpoint: the auto posture is A.
	if d.Cfg.Hub.URL != "" || d.Cfg.Hub.Mode != "hub" {
		t.Fatalf("stock boot resolved hub = (%q, %q), want (\"\", hub)", d.Cfg.Hub.URL, d.Cfg.Hub.Mode)
	}
	bind := d.Cfg.Ingest.Bind
	body := `{"event_id":"aaaabbbbccccddddeeeeffff00001111","level":"error","message":"route wiring probe","exception":{"type":"RouteWiringProbe","value":"route wiring probe"}}`
	resp, err := http.Post("http://"+bind+"/api/1/event/?sentry_key="+routesWiringPubKey, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post event: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/1/event/ = %d, want 200", resp.StatusCode)
	}
	stamped := 0
	for _, rec := range allRecords(t, h) {
		if rec.Kind != types.KEvent {
			continue
		}
		// Scope to the probe's own records: other writers also land event-kind
		// records (lifecycle, sensors' own paths), and only the sentinel
		// transport's records carry the route stamp. The probe's message lives
		// inside the embedded Sentry event payload.
		if rec.Payload["event"] == nil {
			continue
		}
		if se, ok := rec.Payload["event"].(types.SentryEvent); ok {
			if se.Message != "route wiring probe" {
				continue
			}
		} else if b, err := json.Marshal(rec.Payload["event"]); err != nil ||
			!strings.Contains(string(b), "route wiring probe") {
			continue
		}
		if rec.Origin.Route == "" {
			t.Errorf("event %s carried no origin.route", rec.RecID)
			continue
		}
		if rec.Origin.Route != "A" {
			t.Errorf("event %s origin.route = %q, want A (no hub endpoint on this boot)", rec.RecID, rec.Origin.Route)
		}
		stamped++
	}
	if stamped == 0 {
		t.Fatal("no event record landed; the route stamp was never exercised")
	}
}

// TestRoutePolicyRefusalIsTheSentinelRefusalRow: a route policy the sentinel
// refuses (proxy with no hub endpoint) does NOT fail the daemon — it is the
// §3.3a sentinel refusal row: refused=true, the code is
// TROUBLE-SENTINEL-023 and the reason names the offending key.
func TestRoutePolicyRefusalIsTheSentinelRefusalRow(t *testing.T) {
	h := bootDaemonWith(t, SubsystemOptions{
		SentinelProjects: []types.Project{routesWiringProject()},
	})
	d := h.d
	// The embedder declares the unachievable policy on the resolved config —
	// the same statement a `routes.default = "proxy"` file line resolves to —
	// and asks for a build: the refusal, never a server.
	d.Cfg.Routes.Default = "proxy"
	projects := []types.Project{routesWiringProject()}
	srv, reason := buildSentinel(d, d.Cfg.Origin.HostID, projects)
	if srv != nil {
		t.Fatal("buildSentinel accepted default=proxy with no hub endpoint")
	}
	for _, want := range []string{string(types.CodeSentinel023), "sentinel.routes.default"} {
		if !strings.Contains(reason, want) {
			t.Errorf("refusal %q does not carry %q", reason, want)
		}
	}
}
