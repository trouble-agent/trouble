package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/trouble-agent/trouble/internal/types"
)

// GenerationPrefix is the artifact schema version, and it is part of the signed
// material: a future generation cannot be misread as generation 1.
const GenerationPrefix = "trouble.skill.v1"

// skillArtifact is the strict on-disk shape of `skills/<name>/SKILL.toml`
// (SPEC-11 §3.1). It exists so decoding can reject every key outside the frozen
// set: an unknown key is TROUBLE-SKILLS-001, never a warning.
type skillArtifact struct {
	Name             string          `toml:"name"`
	Version          *int            `toml:"version"`
	Sigs             []string        `toml:"sigs"`
	PlayRef          string          `toml:"play_ref"`
	Guards           *artifactGuards `toml:"guards"`
	Provenance       *artifactProv   `toml:"provenance"`
	MinDaemonVersion string          `toml:"min_daemon_version"`
	AllowedModules   []string        `toml:"allowed_modules"`
	Signature        string          `toml:"signature"`
	SignerKeyID      string          `toml:"signer_key_id"`
}

type artifactGuards struct {
	VerifyWindow string `toml:"verify_window"`
	MaxRuns      string `toml:"max_runs"`
	EscalateOn   string `toml:"escalate_on"`
}

type artifactProv struct {
	Incidents []string `toml:"incidents"`
	Research  []string `toml:"research"`
	Author    string   `toml:"author"`
	CreatedTS string   `toml:"created_ts"`
}

// canonicalProjection is the deterministic projection the signature covers
// (§3.2). Field order is part of the contract: `json.Marshal` of this struct
// produces exactly the pinned canonical JSON, and any reordering is a different
// signed material.
type canonicalProjection struct {
	Name             string          `json:"name"`
	Version          int             `json:"version"`
	Sigs             []string        `json:"sigs"`
	PlayRef          string          `json:"play_ref"`
	Guards           canonicalGuards `json:"guards"`
	Provenance       canonicalProv   `json:"provenance"`
	MinDaemonVersion string          `json:"min_daemon_version"`
	AllowedModules   []string        `json:"allowed_modules"`
	SignerKeyID      string          `json:"signer_key_id"`
}

type canonicalGuards struct {
	VerifyWindow string `json:"verify_window"`
	MaxRuns      string `json:"max_runs"`
	EscalateOn   string `json:"escalate_on"`
}

type canonicalProv struct {
	Incidents []string `json:"incidents"`
	Research  []string `json:"research"`
	Author    string   `json:"author"`
	CreatedTS string   `json:"created_ts"`
}

var (
	nameRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}[a-z0-9]$`)
	keyIDRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,63}$`)
	sigRE     = regexp.MustCompile(`^(sentinel|journald|psi|dbus|disk|timers|inotify|collector|generic|unknown):sha256v([1-9][0-9]*):([0-9a-f]{16})$`)
	maxRunsRE = regexp.MustCompile(`^([0-9]{1,2})/day$`)
	tsRE      = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`)
	moduleRE  = regexp.MustCompile(`^[a-z][a-z0-9_]*\.[a-z][a-z0-9_]*$`)
)

// allowedArtifactKeys is the frozen key set of §3.1. Everything else is a
// refusal, including `stats`, `shell`, `exec`, `cmd` and `script`, which is what
// makes the artifact incapable of expressing arbitrary code.
var allowedArtifactKeys = map[string]bool{
	"name": true, "version": true, "sigs": true, "play_ref": true, "guards": true,
	"provenance": true, "min_daemon_version": true, "allowed_modules": true,
	"signature": true, "signer_key_id": true,
	"guards.verify_window": true, "guards.max_runs": true, "guards.escalate_on": true,
	"provenance.incidents": true, "provenance.research": true, "provenance.author": true,
	"provenance.created_ts": true,
}

// checkKeySet refuses an unknown key or table before any field rule runs, and it
// is also where a `[stats]` table is rejected by name.
func checkKeySet(raw map[string]any) error {
	for k, v := range raw {
		if !allowedArtifactKeys[k] {
			if k == "stats" {
				return newErr(types.CodeSkills001, ReasonStatsInArtifact,
					"the artifact declares [stats]: local stats live in the state root, never in the signed artifact")
			}
			if _, isTable := v.(map[string]any); isTable {
				return newErr(types.CodeSkills001, ReasonUnknownTable, "unknown table %q", k)
			}
			return newErr(types.CodeSkills001, ReasonUnknownKey, "unknown key %q", k)
		}
		if sub, isTable := v.(map[string]any); isTable {
			for sk := range sub {
				full := k + "." + sk
				if !allowedArtifactKeys[full] {
					return newErr(types.CodeSkills001, ReasonUnknownKey, "unknown key %q", full)
				}
			}
		}
	}
	return nil
}

// LoadArtifact parses and validates one artifact plus its play bytes (§3.1: the
// strict decoder; §3.2 steps 1–3). It never returns a partially validated Skill.
func LoadArtifact(tomlBytes, playBytes []byte) (types.Skill, error) {
	var skill types.Skill
	if len(tomlBytes) == 0 {
		return skill, newErr(types.CodeSkills001, ReasonValidation, "empty artifact")
	}
	var raw map[string]any
	if err := decodeTOML(tomlBytes, &raw); err != nil {
		return skill, newErr(types.CodeSkills001, ReasonValidation, "artifact is not valid TOML: %v", err)
	}
	if err := checkKeySet(raw); err != nil {
		return skill, err
	}
	var a skillArtifact
	if err := decodeTOML(tomlBytes, &a); err != nil {
		return skill, newErr(types.CodeSkills001, ReasonValidation, "artifact is not valid TOML: %v", err)
	}
	if a.Version == nil {
		return skill, newErr(types.CodeSkills001, ReasonFieldRule, "version is required")
	}
	skill = types.Skill{
		Name:             a.Name,
		Version:          *a.Version,
		Sigs:             a.Sigs,
		PlayRef:          a.PlayRef,
		MinDaemonVersion: a.MinDaemonVersion,
		AllowedModules:   a.AllowedModules,
		Signature:        a.Signature,
		SignerKeyID:      a.SignerKeyID,
	}
	if a.Guards != nil {
		skill.Guards = types.SkillGuards{
			VerifyWindow: types.Duration(a.Guards.VerifyWindow),
			MaxRuns:      a.Guards.MaxRuns,
			EscalateOn:   a.Guards.EscalateOn,
		}
	}
	if a.Provenance != nil {
		skill.Provenance = types.Provenance{
			Incidents: a.Provenance.Incidents,
			Research:  a.Provenance.Research,
			Author:    a.Provenance.Author,
			CreatedTS: a.Provenance.CreatedTS,
		}
	}
	if err := validateFields(&skill); err != nil {
		return types.Skill{}, err
	}
	if playBytes == nil {
		return types.Skill{}, newErr(types.CodeSkills001, ReasonPlayMissing,
			"play_ref %q does not resolve in the artifact's tree", skill.PlayRef)
	}
	return skill, nil
}

// validateFields is the §3.1 rule table, one branch per row.
func validateFields(s *types.Skill) error {
	if !nameRE.MatchString(s.Name) {
		return newErr(types.CodeSkills001, ReasonFieldRule, "name %q is not kebab-case (^[a-z0-9][a-z0-9-]{1,62}[a-z0-9]$)", s.Name)
	}
	if s.Version < 1 {
		return newErr(types.CodeSkills001, ReasonFieldRule, "version %d must be >= 1", s.Version)
	}
	if len(s.Sigs) == 0 || len(s.Sigs) > 64 {
		return newErr(types.CodeSkills001, ReasonFieldRule, "sigs must hold 1..64 entries, got %d", len(s.Sigs))
	}
	seen := map[string]bool{}
	for _, sig := range s.Sigs {
		if seen[sig] {
			return newErr(types.CodeSkills001, ReasonFieldRule, "sigs repeats %q", sig)
		}
		seen[sig] = true
		if !sigRE.MatchString(sig) {
			return newErr(types.CodeSkills001, ReasonSigGrammar,
				"sig %q is not <source>:<algo>v<norm>:<hex16>", sig)
		}
	}
	if err := validatePlayRef(s.PlayRef, s.Name, s.Version); err != nil {
		return err
	}
	if !semverRE.MatchString(s.MinDaemonVersion) {
		return newErr(types.CodeSkills001, ReasonFieldRule,
			"min_daemon_version %q is not semver", s.MinDaemonVersion)
	}
	if len(s.AllowedModules) == 0 || len(s.AllowedModules) > 32 {
		return newErr(types.CodeSkills001, ReasonFieldRule,
			"allowed_modules must hold 1..32 descriptor names, got %d", len(s.AllowedModules))
	}
	for _, m := range s.AllowedModules {
		if strings.ContainsAny(m, "*?") {
			return newErr(types.CodeSkills001, ReasonFieldRule,
				"allowed_modules entry %q is a glob: exact descriptor names only", m)
		}
		if !moduleRE.MatchString(m) {
			return newErr(types.CodeSkills001, ReasonFieldRule,
				"allowed_modules entry %q is not a descriptor name", m)
		}
	}
	if !keyIDRE.MatchString(s.SignerKeyID) {
		return newErr(types.CodeSkills001, ReasonFieldRule, "signer_key_id %q is malformed", s.SignerKeyID)
	}
	// signature is required unless the loader's caller relaxes it (a local source
	// with approve=review); the field itself only has to be well-formed here.
	if s.Signature != "" && len(s.Signature) != 88 {
		return newErr(types.CodeSkills001, ReasonSignature,
			"signature is %d chars, want 88 (base64 std of 64 bytes, with padding)", len(s.Signature))
	}
	if d := s.Guards.VerifyWindow.Std(); d <= 0 || d > 3600*1e9 {
		return newErr(types.CodeSkills001, ReasonFieldRule,
			"guards.verify_window %q must be > 0 and <= 1h", s.Guards.VerifyWindow)
	}
	m := maxRunsRE.FindStringSubmatch(s.Guards.MaxRuns)
	if m == nil {
		return newErr(types.CodeSkills001, ReasonFieldRule, "guards.max_runs %q is not \"<n>/day\"", s.Guards.MaxRuns)
	}
	if n, _ := strconv.Atoi(m[1]); n < 1 || n > 20 {
		return newErr(types.CodeSkills001, ReasonFieldRule, "guards.max_runs %s is outside 1..20/day", m[1])
	}
	switch s.Guards.EscalateOn {
	case "verify_fail", "tool_error", "never":
	default:
		return newErr(types.CodeSkills001, ReasonFieldRule,
			"guards.escalate_on %q is not verify_fail|tool_error|never", s.Guards.EscalateOn)
	}
	if len(s.Provenance.Incidents) == 0 && len(s.Provenance.Research) == 0 {
		return newErr(types.CodeSkills001, ReasonProvenance,
			"provenance needs at least one incident or research id")
	}
	if strings.TrimSpace(s.Provenance.Author) == "" {
		return newErr(types.CodeSkills001, ReasonFieldRule, "provenance.author is empty")
	}
	if !tsRE.MatchString(s.Provenance.CreatedTS) {
		return newErr(types.CodeSkills001, ReasonFieldRule,
			"provenance.created_ts %q is not RFC3339 UTC with millisecond precision", s.Provenance.CreatedTS)
	}
	return nil
}

// validatePlayRef enforces §3.1's path rules: relative, forward slashes, no
// `..`, no URL, no absolute path, no glob, and the version token must match.
func validatePlayRef(ref, name string, version int) error {
	if ref == "" {
		return newErr(types.CodeSkills001, ReasonFieldRule, "play_ref is required")
	}
	if strings.Contains(ref, "..") {
		return newErr(types.CodeSkills001, ReasonFieldRule, "play_ref %q contains '..'", ref)
	}
	if strings.HasPrefix(ref, "/") {
		return newErr(types.CodeSkills001, ReasonFieldRule, "play_ref %q is absolute", ref)
	}
	if strings.Contains(ref, "://") {
		return newErr(types.CodeSkills001, ReasonFieldRule, "play_ref %q is a URL", ref)
	}
	if strings.ContainsAny(ref, "*?") {
		return newErr(types.CodeSkills001, ReasonFieldRule, "play_ref %q is a glob", ref)
	}
	if strings.Contains(ref, "\\") {
		return newErr(types.CodeSkills001, ReasonFieldRule, "play_ref %q does not use forward slashes", ref)
	}
	want := fmt.Sprintf("plays/%s@%d.toml", name, version)
	if ref != want {
		return newErr(types.CodeSkills001, ReasonFieldRule,
			"play_ref %q is not plays/<name>@<version>.toml (%s)", ref, want)
	}
	return nil
}

// PlaySHA256 is hex64(sha256(play_bytes)) — the payload binding of §3.2.
func PlaySHA256(playBytes []byte) string {
	sum := sha256.Sum256(playBytes)
	return hex.EncodeToString(sum[:])
}

// CanonicalBytes is the exact signed material (§3.2): the generation prefix, the
// canonical JSON projection and the play payload digest.
func CanonicalBytes(s types.Skill, playBytes []byte) ([]byte, error) {
	if playBytes == nil {
		return nil, newErr(types.CodeSkills002, ReasonPlayMissing, "no play bytes to bind")
	}
	return canonicalBytesWithPlayHash(s, PlaySHA256(playBytes))
}

// canonicalBytesWithPlayHash is the same construction with the payload digest
// supplied directly. It exists because SPEC-11 pins a golden vector whose §7.1
// play fixture is absent from the spec: the suite can therefore verify the
// canonical form and the pinned signature against the spec's own bytes.
func canonicalBytesWithPlayHash(s types.Skill, playSHA string) ([]byte, error) {
	cj, err := CanonicalJSON(s)
	if err != nil {
		return nil, err
	}
	return []byte(GenerationPrefix + "\n" + cj + "\n" + "play_sha256=" + playSHA + "\n"), nil
}

// CanonicalJSON is the compact canonical projection (no HTML escaping, UTF-8,
// field order as pinned).
func CanonicalJSON(s types.Skill) (string, error) {
	p := canonicalProjection{
		Name:    s.Name,
		Version: s.Version,
		Sigs:    append([]string{}, s.Sigs...),
		PlayRef: s.PlayRef,
		Guards: canonicalGuards{
			VerifyWindow: string(s.Guards.VerifyWindow),
			MaxRuns:      s.Guards.MaxRuns,
			EscalateOn:   s.Guards.EscalateOn,
		},
		Provenance: canonicalProv{
			Incidents: append([]string{}, s.Provenance.Incidents...),
			Research:  append([]string{}, s.Provenance.Research...),
			Author:    s.Provenance.Author,
			CreatedTS: s.Provenance.CreatedTS,
		},
		MinDaemonVersion: s.MinDaemonVersion,
		AllowedModules:   append([]string{}, s.AllowedModules...),
		SignerKeyID:      s.SignerKeyID,
	}
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(p); err != nil {
		return "", newErr(types.CodeSkills001, ReasonValidation, "canonical projection: %v", err)
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}

// CanonicalSHA256 is the digest recorded in the install row.
func CanonicalSHA256(s types.Skill, playBytes []byte) (string, error) {
	b, err := CanonicalBytes(s, playBytes)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// SigsSHA256 is the short digest of the sig set, recorded in the install row.
func SigsSHA256(s types.Skill) string {
	sum := sha256.Sum256([]byte(strings.Join(s.Sigs, "\n")))
	return hex.EncodeToString(sum[:])[:16]
}
