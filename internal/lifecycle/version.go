// Package lifecycle owns daemon boot, config, heartbeat, health, upgrades and
// the satellite forward/spool path (SPEC-12).
package lifecycle

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/trouble-agent/trouble/internal/types"
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

// ReleaseVersion maps a `git describe --tags --always --dirty` stamp onto the
// release version it identifies (SPEC-12 §3.4, §3.6).
//
// The Makefile stamps Version with that very command, so the raw value is one of
// the six shapes the command can produce, all measured on this host
// (2026-09-19):
//
//	v0.1.0             the tagged commit, clean
//	v0.1.0-dirty       the tagged commit, uncommitted changes
//	v0.0.9-3-gabc1234  three commits past v0.0.9, clean        ("-N-g<sha>")
//	v0.0.9-3-gabc1234-dirty
//	<sha>              no tag anywhere: --always falls back to the short sha
//	<sha>-dirty
//
// A ledger record or a health row must never lie about which version acted, so
// the mapping is total and its rules apply in this order:
//
//  1. a stamp whose first dot-separated field is not a number is not a version
//     at all — a bare sha, `unknown`, or the `-dirty` form of either — and is
//     returned VERBATIM. So is the documented stamped fallback `0.0.0-dev`
//     (§3.4): it is the sentinel for "no release", and truncating it to `0.0.0`
//     would report a release that does not exist.
//  2. a leading `v` is dropped once, and a trailing `-dirty` with it.
//  3. a git-describe suffix is dropped whole: `-<count>-g<sha>` is the DEV
//     distance to the tag, so a build three commits past v0.0.9 reports
//     `v0.0.9`, the release it DESCRIBES. (The count is not rounded into the
//     version, and the sha is already carried by the §3.4 triple.)
//  4. what is left of a hyphen suffix attached to a digit goes too, which is
//     what makes `v0.1.0-rc1` report `v0.1.0`.
//  5. the `v` is put back only when it was there, and the value is returned as a
//     PREFIX of the stamp — never a re-render of the parsed fields. A stamp that
//     still does not normalize to MAJOR.MINOR.PATCH after those cuts is returned
//     whole: this function never invents or truncates a version.
func ReleaseVersion(stamp string) string {
	trimmed := strings.TrimSpace(stamp)
	if trimmed == "" {
		return trimmed
	}
	s := trimmed
	prefix := ""
	if strings.HasPrefix(s, "v") {
		prefix, s = "v", s[1:]
	}
	if !isNumeric(strings.Split(s, ".")[0]) || s == defaultVersion {
		// Not a version triple at all, or the "no release" sentinel: hand it
		// back exactly as stamped.
		return trimmed
	}
	s = strings.TrimSuffix(s, "-dirty")
	if m := describeSuffix.FindStringSubmatch(s); m != nil {
		s = strings.TrimSuffix(s, m[0])
	}
	if i := strings.LastIndex(s, "-"); i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		s = s[:i]
	}
	fields := strings.Split(s, ".")
	for _, f := range fields {
		if !isNumeric(f) {
			return trimmed
		}
	}
	return prefix + strings.Join(fields, ".")
}

// describeSuffix matches the `-<count>-g<sha>` tail `git describe` appends when
// HEAD is ahead of the tag.
var describeSuffix = regexp.MustCompile(`-\d+-g[0-9a-f]+$`)

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	_, err := strconv.Atoi(s)
	return err == nil
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

// The §3.6 upgrade steps, as they appear in payload.step. Pinned in one place
// so a record's step is asserted by name rather than by literal.
const (
	UpgradeStepPark     = "park"
	UpgradeStepResume   = "resume"
	UpgradeStepRollback = "rollback"
)

// UpgradeRecord builds one `lifecycle` record of the §3.6 upgrade recipe. Every
// step of the recipe lands in the same append-only ledger, which is what lets
// post-incident forensics say which binary acted before and after the swap:
//
//	{"stage":"upgrade","step":"park","parked":N,"from_version":"v0.0.9","to_version":"v0.1.0"}
//	{"stage":"upgrade","step":"resume","from_version":"v0.0.9","to_version":"v0.1.0"}
//	{"stage":"upgrade","step":"rollback","from_version":"v0.1.0","to_version":"v0.0.9","reason":"ready_timeout"}
//
// from_version and to_version are RELEASE versions — ReleaseVersion over the
// stamped triples of the two builds — never the raw `git describe` stamp: a
// record carrying `v0.0.9-3-gabc1234` could not be compared with the `v0.1.0` a
// health row or a later record reports. The Actor is the running binary's own
// triple, so the record names the build that made the decision as well as the
// two builds it was between.
//
// parked belongs to step "park" alone, and only when the caller could COUNT the
// plays the park acked: a park performed by stopping the unit (§3.6 step 2) is
// not observable from outside the process, and an absent count is a different
// fact from a zero one. Pass parked < 0 to omit the key.
func UpgradeRecord(step, fromVersion, toVersion string, parked int) types.RecordDraft {
	payload := map[string]any{
		"stage":        "upgrade",
		"step":         step,
		"from_version": ReleaseVersion(fromVersion),
		"to_version":   ReleaseVersion(toVersion),
	}
	if step == UpgradeStepPark && parked >= 0 {
		payload["parked"] = parked
	}
	return types.RecordDraft{
		Kind:    types.KLifecycle,
		Actor:   Actor(types.ActorDaemon, "troubled"),
		Payload: payload,
	}
}
