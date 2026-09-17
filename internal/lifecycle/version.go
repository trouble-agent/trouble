// Package lifecycle owns daemon boot, config, heartbeat, health, upgrades and
// the satellite forward/spool path (SPEC-12).
package lifecycle

import (
	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// Link-time vars (SPEC-12 §3.4).
var (
	Version   string
	GitSHA    string
	BuildTime string
)

const (
	defaultVersion   = "0.0.0-dev"
	defaultGitSHA    = "unknown"
	defaultBuildTime = "1970-01-01T00:00:00.000Z"
)

// VersionInfo returns the resolved version triple and whether the build is
// unstamped (SPEC-12 §3.4).
func VersionInfo() (v, sha, bt string, unstamped bool) {
	v = Version
	if v == "" {
		v = defaultVersion
	}
	sha = GitSHA
	if sha == "" {
		sha = defaultGitSHA
	}
	bt = BuildTime
	if bt == "" {
		bt = defaultBuildTime
	}
	unstamped = sha == defaultGitSHA
	return
}

// Actor builds a daemon Actor from the stamped triple (SPEC-12 §3.4).
func Actor(kind types.ActorKind, id string) types.Actor {
	v, sha, bt, _ := VersionInfo()
	return types.Actor{
		Kind:      kind,
		ID:        id,
		Version:   v,
		GitSHA:    sha,
		BuildTime: bt,
	}
}
