package sentinel

// routes_mode_test.go — AC-28 (SPEC-04 §3.10a): the sensor transport route
// matrix, TRBL-061.
//
// What this file proves, level by level:
//
//	matrix    — every §3.10a resolution row resolves to its pinned route:
//	            auto ⇒ B with a hub endpoint and A without; a per_class
//	            prefix wins over the default; unmatched classes take the
//	            default; the longest prefix wins between two overlapping
//	            prefixes; an empty [sentinel.routes] runs the documented
//	            auto posture;
//	override  — end to end through the HTTP surface: one class routes B by
//	            default on a satellite and A once per_class overrides it;
//	stamp     — every landed record carries origin.route = the route that
//	            carried it, events and groups alike, so the ledger answers
//	            "local or relayed?" without a join;
//	refusal   — a proxy selector with no hub endpoint is refused at boot
//	            with TROUBLE-SENTINEL-023 naming the offending key, and so
//	            is an unknown route name and an empty prefix key (§6);
//	inert     — a per_class prefix no event ever matches is legal and NOT
//	            refused (:859): routing policy, not a validation target.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// routeUpstream is the satellite topology of §3.10a: hub.url set with
// hub.mode="satellite".
func routeUpstream(c *Config) {
	c.HubURL = "http://hub.example.net:7643"
	c.HubMode = "satellite"
}

// TestRouteMatrixSection310a is the resolution table of §3.10a, row by row.
func TestRouteMatrixSection310a(t *testing.T) {
	const psiSig = "psi:io_pressurev1:abcd1234abcd1234"
	const sentA = "sentinel:sha256v1:aaaa1111aaaa1111"
	const sentB = "sentinel:sha256v1:bbbb2222bbbb2222"

	rows := []struct {
		name     string
		def      RouteMode
		perClass map[string]RouteMode
		upstream bool
		sig      string
		want     string
	}{
		// | auto | ""  | hub       | —         | A | no upstream exists |
		{"auto no hub endpoint resolves A", RouteAuto, nil, false, psiSig, "A"},
		// | auto | set | satellite | —         | B | relay by default |
		{"auto with hub endpoint resolves B", RouteAuto, nil, true, psiSig, "B"},
		// | direct | set | satellite | —      | A | whole-daemon direct |
		{"direct beats a configured hub", RouteDirect, nil, true, psiSig, "A"},
		// | auto | set | satellite | "direct"  | A | the per-class override wins |
		{"per_class override wins over auto", RouteAuto, map[string]RouteMode{"psi:io_pressure": RouteDirect}, true, psiSig, "A"},
		// the same table, an unmatched class: takes the default
		{"unmatched class takes the default", RouteAuto, map[string]RouteMode{"psi:io_pressure": RouteDirect}, true, sentB, "B"},
		// longest sig-prefix wins between two overlapping prefixes
		{"longest prefix wins", RouteAuto, map[string]RouteMode{
			"sentinel:sha256v1:aaaa": RouteDirect,
			"sentinel:":              RouteProxy,
		}, true, sentA, "A"},
		{"shorter prefix catches the rest", RouteAuto, map[string]RouteMode{
			"sentinel:sha256v1:aaaa": RouteDirect,
			"sentinel:":              RouteProxy,
		}, true, sentB, "B"},
		// the zero Routes (no [sentinel.routes] table at all) is the auto posture
		{"unset default runs the auto posture without a hub", "", nil, false, psiSig, "A"},
		{"unset default runs the auto posture with a hub", "", nil, true, psiSig, "B"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			tb := newRouteTable(RouteConfig{Default: row.def, PerClass: row.perClass}, row.upstream)
			if got := string(tb.resolve(row.sig)); got != row.want {
				t.Errorf("resolve(%q) = %q, want %q", row.sig, got, row.want)
			}
		})
	}
}

// postRouteEvent posts one generic-dialect event through the listener and
// returns the response.
func postRouteEvent(t *testing.T, ts *testServer) *http.Response {
	t.Helper()
	resp := ts.post(t, "/api/1/event/", map[string]string{"X-Sentry-Auth": ts.authHeader("1")},
		[]byte(genericDialectBody("aaaabbbbccccddddeeeeffff00001111")))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/1/event/ = %d, want 200", resp.StatusCode)
	}
	return resp
}

// TestRouteOverrideBeatsDefaultEndToEnd is AC-28 (a)+(d) through the wire:
// with a hub endpoint the class routes B by default and A once per_class
// overrides it, and Origin.Route is stamped with the route that carried each
// record. The override prefix is the event's own canonical sig, discovered
// from the first server, so the test proves PREFIX MATCHING, not a stub.
func TestRouteOverrideBeatsDefaultEndToEnd(t *testing.T) {
	// Baseline: satellite, auto → the event and its group land as B.
	base := newTestServer(t, func(c *Config) {
		c.CanaryProject = ""
		routeUpstream(c)
	})
	defer base.close()
	postRouteEvent(t, base)
	var sig string
	for _, rec := range base.sink.ofKind(types.KEvent) {
		sig = rec.Sig
		if rec.Origin.Route != "B" {
			t.Errorf("auto on a satellite: origin.route = %q, want B", rec.Origin.Route)
		}
	}
	if sig == "" {
		t.Fatal("no event record landed; cannot derive the class sig")
	}
	for _, rec := range base.sink.ofKind(types.KGroup) {
		if rec.Origin.Route != "B" {
			t.Errorf("group record origin.route = %q, want B (the stamp is on every record)", rec.Origin.Route)
		}
	}

	// Override: the same class with per_class[sig]="direct" → A.
	over := newTestServer(t, func(c *Config) {
		c.CanaryProject = ""
		routeUpstream(c)
		c.Routes = RouteConfig{Default: RouteAuto, PerClass: map[string]RouteMode{sig: RouteDirect}}
	})
	defer over.close()
	postRouteEvent(t, over)
	n := 0
	for _, rec := range over.sink.ofKind(types.KEvent) {
		n++
		if rec.Origin.Route != "A" {
			t.Errorf("per_class override: origin.route = %q, want A", rec.Origin.Route)
		}
	}
	if n == 0 {
		t.Fatal("no event record landed on the overridden server")
	}
}

// TestNoHubEndpointEveryClassDirect is AC-28 (b): with no hub endpoint every
// class takes A, end to end, and the A case is stamped explicitly — no relay
// is as much a fact as a relay (§3.10a).
func TestNoHubEndpointEveryClassDirect(t *testing.T) {
	ts := newTestServer(t, func(c *Config) { c.CanaryProject = "" })
	defer ts.close()
	postRouteEvent(t, ts)
	n := 0
	for _, rec := range ts.sink.ofKind(types.KEvent) {
		n++
		if rec.Origin.Route != "A" {
			t.Errorf("no hub endpoint: origin.route = %q, want A", rec.Origin.Route)
		}
	}
	for _, rec := range ts.sink.ofKind(types.KGroup) {
		if rec.Origin.Route != "A" {
			t.Errorf("no hub endpoint group: origin.route = %q, want A", rec.Origin.Route)
		}
	}
	if n == 0 {
		t.Fatal("no event record landed")
	}
}

// TestRouteProxyWithoutHubRefusedAtBoot is AC-28 (c): a proxy selector with
// no hub endpoint is refused with TROUBLE-SENTINEL-023 naming the key, at
// boot, before any listener exists (§3.10a / §6 TROUBLE-SENTINEL-023).
func TestRouteProxyWithoutHubRefusedAtBoot(t *testing.T) {
	rows := []struct {
		name   string
		routes RouteConfig
		wantIn string // the key the refusal must name
	}{
		{
			name:   "default proxy with no hub endpoint",
			routes: RouteConfig{Default: RouteProxy},
			wantIn: "sentinel.routes.default",
		},
		{
			name:   "per_class proxy entry with no hub endpoint",
			routes: RouteConfig{Default: RouteAuto, PerClass: map[string]RouteMode{"psi:io_pressure": RouteProxy}},
			wantIn: "sentinel.routes.per_class",
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			cfg := testConfig(t, func(c *Config) {
				c.CanaryProject = ""
				c.Routes = row.routes
			})
			_, err := NewServer(cfg, &memSink{}, newTestScrubber(t, cfg.Projects))
			if err == nil {
				t.Fatal("NewServer accepted a proxy route with no hub endpoint")
			}
			e, ok := err.(*Error)
			if !ok {
				t.Fatalf("refusal is %T, want *sentinel.Error: %v", err, err)
			}
			if e.Code != types.CodeSentinel023 {
				t.Errorf("code = %s, want %s", e.Code, types.CodeSentinel023)
			}
			if !strings.Contains(e.Msg, row.wantIn) {
				t.Errorf("refusal %q does not name the offending key %q", e.Msg, row.wantIn)
			}
			if !strings.Contains(e.Msg, "hub.url") {
				t.Errorf("refusal %q does not name the fix (hub.url + hub.mode=satellite)", e.Msg)
			}
		})
	}
	// Proxy refusal is the boot gate, not a request path: validate() carries
	// the same refusal NewServer returns, and no server comes back.
	cfg := testConfig(t, func(c *Config) {
		c.CanaryProject = ""
		c.Routes = RouteConfig{Default: RouteProxy}
	})
	if err := cfg.validate(); err == nil {
		t.Error("validate() accepted default=proxy with no hub endpoint")
	}
}

// TestRouteVocabularyAndPrefixRefused covers the rest of §6's 023 cause set:
// an unknown route name and an empty sig-prefix key are refused, each naming
// the offending entry.
func TestRouteVocabularyAndPrefixRefused(t *testing.T) {
	rows := []struct {
		name   string
		routes RouteConfig
		wantIn string
	}{
		{
			name:   "unknown default route name",
			routes: RouteConfig{Default: RouteMode("bogus")},
			wantIn: "sentinel.routes.default",
		},
		{
			name:   "unknown per_class route name",
			routes: RouteConfig{Default: RouteAuto, PerClass: map[string]RouteMode{"psi:io_pressure": RouteMode("bogus")}},
			wantIn: "psi:io_pressure",
		},
		{
			name:   "empty sig-prefix key",
			routes: RouteConfig{Default: RouteAuto, PerClass: map[string]RouteMode{"": RouteDirect}},
			wantIn: "non-empty",
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			cfg := testConfig(t, func(c *Config) {
				c.CanaryProject = ""
				c.Routes = row.routes
			})
			_, err := NewServer(cfg, &memSink{}, newTestScrubber(t, cfg.Projects))
			if err == nil {
				t.Fatal("NewServer accepted an invalid route table")
			}
			e, ok := err.(*Error)
			if !ok {
				t.Fatalf("refusal is %T, want *sentinel.Error", err)
			}
			if e.Code != types.CodeSentinel023 {
				t.Errorf("code = %s, want %s", e.Code, types.CodeSentinel023)
			}
			if !strings.Contains(e.Msg, row.wantIn) {
				t.Errorf("refusal %q does not name the offending entry %q", e.Msg, row.wantIn)
			}
		})
	}
}

// TestInertPrefixNotRefused is AC-28 (f): a per_class prefix no event ever
// matches is legal and inert (:859) — routing policy, not a validation
// target. The server builds, and an unrelated event still takes the default.
func TestInertPrefixNotRefused(t *testing.T) {
	ts := newTestServer(t, func(c *Config) {
		c.CanaryProject = ""
		c.Routes = RouteConfig{
			Default:  RouteAuto,
			PerClass: map[string]RouteMode{"collector:go-panic-never-matches-anything": RouteDirect},
		}
	})
	defer ts.close()
	postRouteEvent(t, ts)
	for _, rec := range ts.sink.ofKind(types.KEvent) {
		if rec.Origin.Route != "A" {
			t.Errorf("inert prefix: origin.route = %q, want A (the default, no hub)", rec.Origin.Route)
		}
	}
}

// TestInertProxyPrefixWithHubNotRefused is the same inertness for a proxy
// value, on a satellite where the route IS achievable.
func TestInertProxyPrefixWithHubNotRefused(t *testing.T) {
	cfg := testConfig(t, func(c *Config) {
		c.CanaryProject = ""
		routeUpstream(c)
		c.Routes = RouteConfig{
			Default:  RouteAuto,
			PerClass: map[string]RouteMode{"psi:io_pressure-never-matches": RouteProxy},
		}
	})
	if _, err := NewServer(cfg, &memSink{}, newTestScrubber(t, cfg.Projects)); err != nil {
		t.Fatalf("NewServer refused an inert proxy prefix on a satellite: %v", err)
	}
}
