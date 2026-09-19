package lifecycle

// profile.go owns the SPEC-13 §2.1 `[server]` surface at the resolution
// boundary: it projects the resolved keys onto the shared types.ProfileConfig
// and answers the SPEC-12 §3.7a rule 2 boot gate (SPEC-13 §4.1 step 1).
//
// Boundary, stated once so the next task does not have to re-derive it: the
// RUNTIME half of the light-hub profile — dialling Redis, creating the consumer
// group, the XREADGROUP → append → XACK seam, the dedup gate and the DuckBrain
// archival tier — belongs to internal/hub (SPEC-13 §2.3, §3.2–§3.5), which does
// not exist in this tree. Nothing here pretends to queue, dedup or archive:
// what this file owns is everything that must be decided BEFORE a connection is
// opened — the keys resolve like any other key, the profile and its required
// keys are validated, and an unusable profile is refused with SPEC-13's own code
// (TROUBLE-HUB-001) instead of being discovered at runtime. When internal/hub
// lands, hub.Gate consumes Config.ServerProfile() and adds only the checks that
// need a live Redis (TROUBLE-HUB-002/003/015/016); it must not re-implement
// this validation, because a second copy of the rule is how the config surface
// and the runtime start disagreeing.
//
// The one SPEC-13 §4.1 rule this file deliberately does NOT implement is the
// live profile switch refusal (TROUBLE-HUB-013): the daemon has no SIGHUP
// reload path at all (nothing re-runs Resolve while serving), so there is no
// reload to refuse. Refusing a switch that cannot be requested would be an
// assertion about code that does not exist.

import (
	"fmt"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// The two accepted values of `server.profile` (SPEC-13 §1). They are the only
// two: a profile selects plumbing inside the one daemon, so an unknown value is
// a configuration error, never a fallback to standalone.
const (
	ProfileStandalone = "standalone"
	ProfileLightHub   = "light-hub"
)

// InvalidReason values, spelled exactly as SPEC-TYPES §3.15.11 enumerates them.
// The gate below reports the first one that applies in that enumeration order,
// so a config with several defects still yields ONE deterministic reason.
const (
	reasonMissingRedisURL = "missing_redis_url"
	reasonMissingNS       = "missing_namespace"
	reasonSatellite       = "satellite_profile"
	reasonUnknownProfile  = "unknown_profile"
)

// ServerProfile projects the resolved `[server]` keys onto types.ProfileConfig
// and applies the SPEC-12 §3.7a rule 2 validation:
//
//   - `standalone` (the default) is valid with no Redis and no DuckBrain — the
//     standalone path must keep working with an empty [server] table present,
//     absent, or partially declared;
//   - `light-hub` requires server.redis.url and server.duckbrain.namespace, and
//     is refused when hub.mode=satellite (a satellite's durable queue is its
//     spool; a second queue would be a second truth);
//   - any other value is unknown_profile.
//
// satellite_profile keys off hub.mode == "satellite", the one satellite spelling
// SPEC-12 §3.1 pins for that key.
//
// It is a pure projection: nothing is rewritten in place. HubID "" and Consumer
// "" keep their documented meaning ("→ origin.host_id") for the consumer to
// resolve at the point of use, so the explain dump never disagrees with what the
// operator declared.
func (c Config) ServerProfile() types.ProfileConfig {
	p := types.ProfileConfig{
		Profile:         c.Server.Profile,
		HubID:           c.Server.HubID,
		RedisURL:        c.Server.Redis.URL,
		RedisStream:     c.Server.Redis.Stream,
		ConsumerGroup:   c.Server.Redis.Group,
		Consumer:        c.Server.Redis.Consumer,
		MaxLen:          c.Server.Redis.MaxLen,
		DedupTTL:        c.Server.Redis.DedupTTL,
		RequireRedis:    c.Server.Redis.RequireRedis,
		DBNamespace:     c.Server.DuckBrain.Namespace,
		DBEndpoint:      c.Server.DuckBrain.Endpoint,
		ArchiveInterval: c.Server.DuckBrain.ArchiveInterval,
		KeepLocalGens:   c.Server.DuckBrain.KeepLocalGens,
	}

	switch p.Profile {
	case ProfileStandalone:
		p.Valid = true
	case ProfileLightHub:
		switch {
		case p.RedisURL == "":
			p.InvalidReason = reasonMissingRedisURL
		case p.DBNamespace == "":
			p.InvalidReason = reasonMissingNS
		case c.Hub.Mode == "satellite":
			p.InvalidReason = reasonSatellite
		}
		p.Valid = p.InvalidReason == ""
	default:
		p.InvalidReason = reasonUnknownProfile
	}
	return p
}

// CheckServerProfile is the boot gate: nil when the resolved profile is usable,
// otherwise an error carrying TROUBLE-HUB-001 (class permanent, exit 13). It is
// called by the composition root after resolution and BEFORE the bind preflight
// holds a listener, so a refusal is worth exactly zero HTTP responses
// (SPEC-12 §7 profile_test row, SPEC-13 §4.1 step 1).
func (c Config) CheckServerProfile() error {
	p := c.ServerProfile()
	if p.Valid {
		return nil
	}
	switch p.InvalidReason {
	case reasonMissingRedisURL:
		return fmt.Errorf("%w: server.profile=%q requires server.redis.url (SPEC-13 §2.1)", types.CodeHub001, p.Profile)
	case reasonMissingNS:
		return fmt.Errorf("%w: server.profile=%q requires server.duckbrain.namespace (SPEC-13 §2.1)", types.CodeHub001, p.Profile)
	case reasonSatellite:
		return fmt.Errorf("%w: server.profile=%q is refused with hub.mode=satellite: a satellite's durable queue is its spool, not a second queue (SPEC-13 §1)", types.CodeHub001, p.Profile)
	default:
		return fmt.Errorf("%w: server.profile %q is not one of %s|%s (SPEC-13 §2.1)", types.CodeHub001, p.Profile, ProfileStandalone, ProfileLightHub)
	}
}
