package sentinel

import (
	"fmt"
	"sort"
	"strings"

	"github.com/trouble-agent/trouble/internal/hub"
	"github.com/trouble-agent/trouble/internal/types"
)

// RouteMode is the configured transport-route selector of SPEC-TYPES
// §3.15.3: how an event class chooses between the two routes of SPEC-04
// §3.10a.
type RouteMode string

const (
	// RouteAuto resolves per event: B when this daemon has a configured hub
	// endpoint (hub.url set on a satellite, §3.10a's "the auto rule, one
	// rule, two inputs"), else A.
	RouteAuto RouteMode = "auto"
	// RouteDirect forces the local hop (A) for the classes it selects.
	RouteDirect RouteMode = "direct"
	// RouteProxy forces the hub hop (B); refused at boot when no hub endpoint
	// is configured (TROUBLE-SENTINEL-023).
	RouteProxy RouteMode = "proxy"
)

// RouteConfig is the resolved `[sentinel.routes]` surface (SPEC-TYPES
// §3.15.3): the default selector plus the sig-prefix → selector table
// consulted before the default. The zero value is the documented posture:
// auto with no overrides.
type RouteConfig struct {
	Default  RouteMode            `json:"default"`   // auto | direct | proxy
	PerClass map[string]RouteMode `json:"per_class"` // sig-prefix → route; longest prefix wins
}

// routeTable is the boot-validated routing policy of §3.10a: the resolved
// [sentinel.routes] plus the prefix order its resolution consumes and the
// topology input the auto rule needs. It is built ONCE in NewServer, so the
// sorted order and the hub-upstream fact are fixed for the server's lifetime;
// a re-resolved table (:856) means a rebuilt server, and the swap is that
// single pointer — the previous table answers no event mid-swap.
type routeTable struct {
	cfg      RouteConfig
	ordered  []string // per-class prefixes, longest first, ties lexicographic
	upstream bool     // a hub endpoint exists (hub.url + hub.mode=satellite)
}

// newRouteTable precomputes the prefix order. The caller has already run
// validate (NewServer does), so the values are in the pinned vocabulary and
// every prefix is non-empty; empty tables leave ordered nil and resolution
// falls straight through to the default.
func newRouteTable(cfg RouteConfig, hubUpstream bool) *routeTable {
	ordered := make([]string, 0, len(cfg.PerClass))
	for p := range cfg.PerClass {
		ordered = append(ordered, p)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if len(ordered[i]) != len(ordered[j]) {
			return len(ordered[i]) > len(ordered[j])
		}
		return ordered[i] < ordered[j]
	})
	return &routeTable{cfg: cfg, ordered: ordered, upstream: hubUpstream}
}

// resolve answers one event's route (§3.10a): the longest per-class prefix
// that matches the canonical sig string wins over the default (ties cannot
// occur — ordered is strictly longest-first); a class with no matching prefix
// takes the default; auto resolves against the configured topology. The empty
// default is the unset key and means auto, so a host that declares no
// [sentinel.routes] table runs the documented posture.
func (t *routeTable) resolve(sig string) hub.RouteDecision {
	for _, p := range t.ordered {
		if strings.HasPrefix(sig, p) {
			return routeOf(t.cfg.PerClass[p], t.upstream)
		}
	}
	return t.defaultRoute()
}

// defaultRoute answers the auto rule for an event whose class no per-class
// prefix selected.
func (t *routeTable) defaultRoute() hub.RouteDecision {
	mode := t.cfg.Default
	if mode == "" {
		mode = RouteAuto
	}
	return routeOf(mode, t.upstream)
}

// routeOf turns one selector into the decision; validate has already refused
// anything outside the vocabulary, so only the three pinned values arrive.
func routeOf(mode RouteMode, upstream bool) hub.RouteDecision {
	if mode == RouteProxy {
		return hub.RouteB
	}
	if mode == RouteDirect {
		return hub.RouteA
	}
	// RouteAuto defers to the topology: a satellite with a hub endpoint
	// relays (B), a hub itself and a host with no upstream go direct (A).
	if upstream {
		return hub.RouteB
	}
	return hub.RouteA
}

// validateRoutePolicy is §3.10a's boot validation of [sentinel.routes]: the
// default and every per-class value must name a route mode, and a proxy
// selector with no configured hub endpoint is an unachievable claim — refused
// as TROUBLE-SENTINEL-023 with exit 13 BEFORE the listener binds, so a
// routing policy that cannot be honoured is found at boot, not under load.
// A per-class prefix no event ever matches is legal and inert (:859): the
// table is routing policy, not a validation target, and `trouble hub status`
// is what makes dead policy visible.
func (c *Config) validateRoutePolicy() *Error {
	upstream := c.hubEndpointConfigured()
	def := c.Routes.Default
	if def == "" {
		def = RouteAuto
	}
	switch def {
	case RouteAuto, RouteDirect:
	case RouteProxy:
		if !upstream {
			return errf(types.CodeSentinel023,
				`sentinel.routes.default="proxy" is refused: no hub endpoint is configured (hub.url set with hub.mode="satellite"), so the route cannot be honoured (SPEC-04 §3.10a)`,
				causeRouteUnachievable)
		}
	default:
		return errf(types.CodeSentinel023,
			fmt.Sprintf("sentinel.routes.default must be auto|direct|proxy, got %q (SPEC-04 §3.10a)", string(def)),
			causeRouteModeUnknown)
	}
	// Sorted so the refusal names the offending key deterministically.
	prefixes := make([]string, 0, len(c.Routes.PerClass))
	for p := range c.Routes.PerClass {
		prefixes = append(prefixes, p)
	}
	sort.Strings(prefixes)
	for _, p := range prefixes {
		if p == "" {
			return errf(types.CodeSentinel023,
				"sentinel.routes.per_class keys must be non-empty sig-prefixes (SPEC-04 §3.10a)",
				causeRoutePrefixEmpty)
		}
		switch c.Routes.PerClass[p] {
		case RouteAuto, RouteDirect:
		case RouteProxy:
			if !upstream {
				return errf(types.CodeSentinel023,
					"sentinel.routes.per_class[%q]=\"proxy\" is refused: no hub endpoint is configured (hub.url set with hub.mode=\"satellite\"), so the route cannot be honoured (SPEC-04 §3.10a)",
					causeRouteUnachievable, p)
			}
		default:
			return errf(types.CodeSentinel023,
				fmt.Sprintf("sentinel.routes.per_class[%q] must be auto|direct|proxy, got %q (SPEC-04 §3.10a)", p, string(c.Routes.PerClass[p])),
				causeRouteModeUnknown)
		}
	}
	return nil
}

// hubEndpointConfigured answers the auto rule's topology input (§3.10a): a
// hub endpoint exists when hub.url is set AND this daemon is a satellite
// (hub.mode="satellite"). A hub has no upstream, so hub-side sensors always
// take A; both keys arrive [SPEC-12-resolved] and sentinel never re-reads the
// file for them.
func (c *Config) hubEndpointConfigured() bool {
	return c.HubURL != "" && c.HubMode == "satellite"
}
