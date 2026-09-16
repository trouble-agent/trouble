package lifecycle

import (
	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// TopologyDecisions returns the T1..T5 decision rows verbatim (SPEC-12 §3.7).
func TopologyDecisions(cfg Config) []types.TopologyDecision {
	return []types.TopologyDecision{
		{
			From:     types.T1,
			To:       types.T2,
			Decision: "bind-address-scoped auth: loopback = DSN pubkey only; non-loopback = per-project token or mandated reverse proxy; public = proxy mandated",
			ConfigKeys: []string{
				"ingest.auth.loopback_dsn",
				"ingest.auth.nonloopback_mode",
				"ingest.auth.public_require_proxy",
				"ingest.advertised_host",
				"dashboard.auth.transport",
				"dashboard.auth.identity_provider",
			},
		},
		{
			From:     types.T2,
			To:       types.T3,
			Decision: "one forwarding protocol: the sentinel envelope wire format carries forwarded records; there is no second hub/satellite dialect",
			ConfigKeys: []string{
				"hub.mode",
				"hub.url",
				"hub.forward_project_id",
				"hub.protocol_version",
				"spool.budget_bytes",
			},
		},
		{
			From:     types.T3,
			To:       types.T4,
			Decision: "bounded queues + token session model: hard byte budget with defined eviction, browser access is cookie/Bearer session, never a URL token",
			ConfigKeys: []string{
				"spool.budget_bytes",
				"spool.gap_reserve_bytes",
				"spool.fsync",
				"spool.fsync_window_ms",
				"dashboard.auth.transport",
				"dashboard.auth.session_ttl",
				"verify.zone_windows",
			},
		},
		{
			From:     types.T4,
			To:       types.T5,
			Decision: "origin fields in sig/ledger path + proxy key class: origin on every record, reserved credential class for forwarding proxy, per-zone verification",
			ConfigKeys: []string{
				"origin.host_id",
				"origin.hub_id",
				"ingest.auth.proxy_key_class",
				"hub.proxy_trust_header",
				"verify.zone_windows",
				"lifecycle.clock_skew_tolerance",
			},
		},
	}
}

// ZoneWindow returns the configured window for a zone, with a fallback.
func ZoneWindow(cfg Config, zone string) types.Duration {
	if d, ok := cfg.Verify.ZoneWindows[zone]; ok {
		return d
	}
	return "15m"
}
