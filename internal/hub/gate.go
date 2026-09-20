package hub

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/trouble-agent/trouble/internal/lifecycle"
	"github.com/trouble-agent/trouble/internal/types"
)

// The two accepted `server.profile` values (SPEC-13 §1). They are aliases of
// lifecycle's constants so the spelling exists once in the tree.
const (
	ProfileStandalone = lifecycle.ProfileStandalone
	ProfileLightHub   = lifecycle.ProfileLightHub
)

// ProfileGate is the resolved profile plus the runtime configuration it implies.
//
// SPEC-13 §2.3 spells this type `profileGate` and has it returned by
// `hub.Gate(lifecycle.Config)`. It is exported here because the composition root
// in internal/app holds the value across the package boundary and Go cannot name
// an unexported type from another package; the shape and the field meaning are
// the spec's.
type ProfileGate struct {
	// Profile is the resolved `server.profile` value, verbatim.
	Profile string
	// HubID is `server.hub_id` ("" → origin.host_id).
	HubID string
	// Enabled is true only for light-hub: it is the statement "this daemon is a
	// hub with a Redis ingestion buffer and an archival tier". Under standalone
	// nothing in this package runs (SPEC-13 §4.1 step 2).
	Enabled bool
	// Valid mirrors types.ProfileConfig.Valid: false means TROUBLE-HUB-001.
	Valid bool
	// InvalidReason is the lifecycle reason token (missing_redis_url |
	// missing_namespace | satellite_profile | unknown_profile).
	InvalidReason string

	Redis   RedisConfig
	Archive ArchiveConfig

	// pc is the resolved profiles config this gate was built from, kept verbatim
	// so the runtime can carry the profile's provenance into its state file and
	// lifecycle records without re-projecting (and possibly re-deriving) it.
	pc types.ProfileConfig
	// topo holds the SPEC-12 §3.7 rows captured at Gate time.
	topo []types.TopologyDecision
}

// ProfileConfig returns the resolved SPEC-13 §2.1 profile the gate was built
// from, unchanged.
func (p ProfileGate) ProfileConfig() types.ProfileConfig { return p.pc }

// Consumer resolves the consumer name (SPEC-13 §3.3: `consumer = "" →
// origin.host_id`; one consumer per state root).
func (p ProfileGate) Consumer() string {
	if p.Redis.Consumer != "" {
		return p.Redis.Consumer
	}
	return p.Redis.HostID
}

// Gate is the SPEC-13 §4.1 step 1 boot gate.
//
// It delegates the profile validation to lifecycle.Config.ServerProfile (the one
// copy of the rule, SPEC-12 §3.7a rule 2) and adds nothing that the config
// surface can already decide: a second validator is how the config surface and
// the runtime start disagreeing. What it adds is the PROJECTION of the resolved
// keys onto the runtime structures (RedisConfig, ArchiveConfig) and the profile
// state file's location.
//
// TROUBLE-HUB-001 (class permanent, exit 13) is returned for an invalid or
// incomplete profile. The checks that need a live Redis (002/003/015/016) are
// deliberately NOT here: they belong to OpenRedis, which is where a connection
// exists (SPEC-13 §2.3).
func Gate(cfg lifecycle.Config) (ProfileGate, error) {
	pc := profileConfigFrom(cfg)
	g := ProfileGate{
		Profile:       pc.Profile,
		HubID:         pc.HubID,
		Valid:         pc.Valid,
		InvalidReason: pc.InvalidReason,
		pc:            pc,
		Redis:         redisConfigFrom(cfg),
		topo:          lifecycle.TopologyDecisions(cfg),
	}
	if pc.Profile == ProfileLightHub {
		g.Enabled = true
		// ArchiveConfig needs the local paths and the live file, which are not
		// config keys; Gate fills them when the caller supplies them through
		// GateWithPaths. The defaults below keep a direct Gate call coherent.
		g.Archive = archiveConfigFrom(cfg, "", "", "").WithDefaults()
	}
	// The invalid cases reuse lifecycle's error verbatim: the code (001), the
	// reason and the wording are the config surface's own, so an operator sees
	// one message for one defect.
	if err := cfg.CheckServerProfile(); err != nil {
		return g, &Error{Code: types.CodeHub001, Reason: ReasonProfile, Msg: err.Error(), Err: err}
	}
	return g, nil
}

// GateWithPaths is Gate plus the three local facts the archival tier needs and
// that config cannot carry: the state root, the ledger root and the ledger file
// the writer is currently appending to (never an archival candidate).
func GateWithPaths(cfg lifecycle.Config, stateRoot, ledgerRoot, liveFile string) (ProfileGate, error) {
	g, err := Gate(cfg)
	if err != nil {
		return g, err
	}
	if g.Enabled {
		g.Archive = archiveConfigFrom(cfg, stateRoot, ledgerRoot, liveFile)
	}
	return g, nil
}

// TopologyImpact returns the SPEC-12 §3.7 decision rows plus the profile's own
// row (SPEC-13 §2.3).
//
// The profile row carries the ZERO Topology on both ends on purpose: SPEC-13 §1
// states that the profile is orthogonal to the rung table (a T1 laptop and a T5
// regional hub both default to standalone), so claiming a rung transition would
// invent a mapping the spec denies. The row's decision string is SPEC-12 §3.7a's
// aspect table, and its config keys are the `server.*` keys the profile owns.
func TopologyImpact(p ProfileGate) []types.TopologyDecision {
	out := make([]types.TopologyDecision, 0, len(p.topo)+1)
	out = append(out, p.topo...)
	decision := "server profile standalone: in-process ingestion, no archival tier, zero external dependencies"
	if p.Enabled {
		decision = "server profile light-hub: Redis stream + consumer group before the same ledger Append, closed generations exported to DuckBrain then droppable (archival target: " + archiveTargetState(p.Archive) + ")"
	}
	keys := []string{"server.profile"}
	if p.Enabled {
		keys = append(keys, "server.redis.url", "server.redis.stream", "server.redis.group", "server.redis.maxlen", "server.redis.dedup_ttl", "server.redis.require_redis", "server.duckbrain.namespace", "server.duckbrain.endpoint", "server.duckbrain.archive_interval", "server.duckbrain.keep_local_generations")
	}
	out = append(out, types.TopologyDecision{
		Decision:   decision,
		ConfigKeys: keys,
	})
	return out
}

func archiveTargetState(a ArchiveConfig) string {
	switch {
	case a.Namespace == "":
		return "unconfigured"
	case a.Endpoint == "":
		return "namespace " + a.Namespace + ", no endpoint"
	default:
		return "namespace " + a.Namespace + " at " + a.Endpoint
	}
}

// ---- profile state (SPEC-13 §3.1: one file, one truth) ----

// ProfileState is the content of <state_root>/hub/profile.json (0600).
type ProfileState struct {
	Profile    string `json:"profile"`
	Since      string `json:"since"`
	HubID      string `json:"hub_id"`
	ConfigHash string `json:"config_hash"`
}

// StateDir is <state_root>/hub (0700), created on demand. Every other path in
// this package is derived from it, so a test state root never touches a live one.
func StateDir(stateRoot string) string { return filepath.Join(stateRoot, "hub") }

// ArchiveDir is <state_root>/hub/archive (0700).
func ArchiveDir(stateRoot string) string { return filepath.Join(StateDir(stateRoot), "archive") }

// ProfilePath is <state_root>/hub/profile.json.
func ProfilePath(stateRoot string) string { return filepath.Join(StateDir(stateRoot), "profile.json") }

// DedupStatePath is <state_root>/hub/dedup.state.
func DedupStatePath(stateRoot string) string {
	return filepath.Join(StateDir(stateRoot), "dedup.state")
}

// EnsureStateDirs creates the hub state tree with the SPEC-13 §3.1 modes
// (hub 0700, archive 0700, files 0600).
func EnsureStateDirs(stateRoot string) error {
	if stateRoot == "" {
		// An empty state root derives a RELATIVE path ("hub/archive"), which
		// would drop state into the process's working directory. That is never
		// intended, so it is refused by name instead of guessed.
		return errf(types.CodeHub009, ReasonArchiveCfg, "the hub state root is empty: refusing to write hub state into the working directory")
	}
	for _, dir := range []string{StateDir(stateRoot), ArchiveDir(stateRoot)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return errWrap(types.CodeHub009, ReasonArchiveCfg, fmt.Sprintf("cannot create %s", dir), err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return errWrap(types.CodeHub009, ReasonArchiveCfg, fmt.Sprintf("cannot set mode 0700 on %s", dir), err)
		}
	}
	return nil
}

// ConfigHash is a stable 16-hex digest of the resolved profile configuration.
// It is what makes "the profile did not change across this restart" checkable
// without diffing the whole config dump.
func ConfigHash(p types.ProfileConfig) string {
	b, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

// WriteProfileState writes profile.json. `since` is preserved while the profile
// AND its configuration hash are unchanged (the profile was adopted earlier and
// is still in force), and moves forward when either changed — which is exactly
// the fact TROUBLE-HUB-013's "refused live switch, effective after restart" needs:
// a restart that adopts a different profile or a different queue shows a new
// `since`, so "the profile in force" is never a guess.
func WriteProfileState(stateRoot string, pc types.ProfileConfig, now time.Time) (ProfileState, error) {
	if err := EnsureStateDirs(stateRoot); err != nil {
		return ProfileState{}, err
	}
	hash := ConfigHash(pc)
	st := ProfileState{
		Profile:    pc.Profile,
		Since:      types.FormatUTC(now),
		HubID:      pc.HubID,
		ConfigHash: hash,
	}
	if prev, err := ReadProfileState(stateRoot); err == nil && prev.Profile == st.Profile && prev.ConfigHash == hash {
		st.Since = prev.Since
	}
	b, err := json.Marshal(st)
	if err != nil {
		return ProfileState{}, errWrap(types.CodeHub009, ReasonArchiveCfg, "cannot encode profile state", err)
	}
	path := ProfilePath(stateRoot)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return ProfileState{}, errWrap(types.CodeHub009, ReasonArchiveCfg, "cannot write profile state", err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return ProfileState{}, errWrap(types.CodeHub009, ReasonArchiveCfg, "cannot set mode 0600 on profile state", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return ProfileState{}, errWrap(types.CodeHub009, ReasonArchiveCfg, "cannot publish profile state", err)
	}
	return st, nil
}

// ReadProfileState reads profile.json. A missing or torn file is a zero state
// and no error for the PRESENT-tense callers (a first boot has no state); a
// genuine I/O failure is reported.
func ReadProfileState(stateRoot string) (ProfileState, error) {
	b, err := os.ReadFile(ProfilePath(stateRoot))
	if err != nil {
		if os.IsNotExist(err) {
			return ProfileState{}, nil
		}
		return ProfileState{}, errWrap(types.CodeHub009, ReasonArchiveCfg, "cannot read profile state", err)
	}
	var st ProfileState
	if err := json.Unmarshal(b, &st); err != nil {
		return ProfileState{}, nil // a torn profile.json is "unknown", never a crash
	}
	return st, nil
}
