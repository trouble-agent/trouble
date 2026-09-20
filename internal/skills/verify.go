package skills

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/trouble-agent/trouble/internal/types"
)

// decodeTOML is the single decode entry point (BurntSushi, strict about types,
// explicit about unknown-key detection by way of the raw key walk).
func decodeTOML(b []byte, out any) error {
	return toml.Unmarshal(b, out)
}

// semverRE is the §4.3 grammar.
var semverRE = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(-([0-9A-Za-z.-]+))?(\+([0-9A-Za-z.-]+))?$`)

// semver is a parsed version with the numeric compare and the prerelease rank.
type semver struct {
	major, minor, patch int
	pre                 string
	raw                 string
}

func parseSemver(s string) (semver, bool) {
	m := semverRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return semver{}, false
	}
	v := semver{raw: s, pre: m[5]}
	v.major, _ = strconv.Atoi(m[1])
	v.minor, _ = strconv.Atoi(m[2])
	v.patch, _ = strconv.Atoi(m[3])
	return v, true
}

// less reports whether a < b (prerelease ranks lower than the release; build
// metadata is ignored).
func (a semver) less(b semver) bool {
	if a.major != b.major {
		return a.major < b.major
	}
	if a.minor != b.minor {
		return a.minor < b.minor
	}
	if a.patch != b.patch {
		return a.patch < b.patch
	}
	switch {
	case a.pre == "" && b.pre == "":
		return false
	case a.pre == "":
		return false // a is the release, b is a prerelease
	case b.pre == "":
		return true // a is a prerelease, b is the release
	default:
		return a.pre < b.pre
	}
}

// GateSignature is §4.2 step 2 and SPEC-11 §2's `GateSignature`: the artifact must
// name a configured, enabled signer whose trust class matches the origin, and the
// ed25519 signature must verify over the canonical bytes.
func GateSignature(s types.Skill, playBytes []byte, signers []types.SkillSigner, requireSignature bool, localAuthorSuffix string) error {
	if s.Signature == "" {
		if !requireSignature {
			return nil
		}
		return newErr(types.CodeSkills002, ReasonSignature, "the artifact carries no signature")
	}
	sig, err := base64.StdEncoding.DecodeString(s.Signature)
	if err != nil {
		return newErr(types.CodeSkills002, ReasonSignature, "signature is not base64 std: %v", err)
	}
	if len(sig) != ed25519.SignatureSize {
		return newErr(types.CodeSkills002, ReasonSignature,
			"signature is %d bytes, want %d", len(sig), ed25519.SignatureSize)
	}
	signer, err := lookupSigner(s.SignerKeyID, signers)
	if err != nil {
		return err
	}
	// A local key can only ever sign this host's own promotions (§3.4).
	if signer.Trust == types.TrustLocal {
		if localAuthorSuffix == "" || !strings.HasSuffix(s.Provenance.Author, localAuthorSuffix) {
			return newErr(types.CodeSkills003, ReasonSignerTrust,
				"the %s key signs local promotions only (author %q)", types.TrustLocal, s.Provenance.Author)
		}
	}
	canonical, err := canonicalBytesFor(s, playBytes)
	if err != nil {
		return err
	}
	if !verifyCanonical(canonical, s.Signature, signer) {
		return newErr(types.CodeSkills002, ReasonSignature,
			"ed25519 verification failed: the artifact or its play was edited after signing")
	}
	return nil
}

// verifyCanonical is the ed25519 check itself: the signature, decoded, over the
// canonical bytes with the signer's public key. It is separate so the suite can
// assert the spec's own pinned vector, whose private half is not in this repo.
func verifyCanonical(canonical []byte, signatureB64 string, signer types.SkillSigner) bool {
	sig, err := base64.StdEncoding.DecodeString(signatureB64)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false
	}
	pub, err := base64.StdEncoding.DecodeString(signer.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(pub), canonical, sig)
}

// sha256Hex is hex64(sha256(b)).
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// canonicalBytesFor binds the play payload and re-checks the digest of the bytes
// actually on disk, so a play edited after review fails verification even though
// SKILL.toml is untouched (§3.2 step 7).
func canonicalBytesFor(s types.Skill, playBytes []byte) ([]byte, error) {
	if playBytes == nil {
		return nil, newErr(types.CodeSkills002, ReasonPlayMissing, "no play bytes to verify")
	}
	return canonicalBytesWithPlayHash(s, PlaySHA256(playBytes))
}

// Verify is §2's `Verify`: signature + payload binding in one call.
func Verify(s types.Skill, playBytes []byte, signers []types.SkillSigner) error {
	return GateSignature(s, playBytes, signers, true, localAuthorSuffix(signers, hostIDSentinel))
}

// hostIDSentinel is replaced by the caller's host id through VerifyForHost; the
// plain Verify keeps the release-only posture.
const hostIDSentinel = ""

// VerifyForHost is Verify with the host id the local trust class is scoped to.
func VerifyForHost(s types.Skill, playBytes []byte, signers []types.SkillSigner, hostID string) error {
	return GateSignature(s, playBytes, signers, true, localAuthorSuffix(signers, hostID))
}

func localAuthorSuffix(_ []types.SkillSigner, hostID string) string {
	if hostID == "" {
		return ""
	}
	return "@" + hostID
}

func lookupSigner(keyID string, signers []types.SkillSigner) (types.SkillSigner, error) {
	for _, s := range signers {
		if s.KeyID != keyID {
			continue
		}
		if !s.Enabled {
			return s, newErr(types.CodeSkills003, ReasonSignerDisabled, "signer %q is disabled", keyID)
		}
		switch s.Trust {
		case types.TrustRelease, types.TrustLocal:
		default:
			return s, newErr(types.CodeSkills003, ReasonSignerUnknown,
				"signer %q has unknown trust class %q", keyID, s.Trust)
		}
		return s, nil
	}
	return types.SkillSigner{}, newErr(types.CodeSkills003, ReasonSignerUnknown,
		"signer_key_id %q is not in the configured signer set", keyID)
}

// GateFloor is §4.3: the daemon version must satisfy min_daemon_version, and an
// unstamped binary (0.0.0-dev) satisfies nothing.
func GateFloor(s types.Skill, daemonVersion string) error {
	want, ok := parseSemver(s.MinDaemonVersion)
	if !ok {
		return newErr(types.CodeSkills001, ReasonFieldRule,
			"min_daemon_version %q is not semver", s.MinDaemonVersion)
	}
	got, ok := parseSemver(daemonVersion)
	if !ok {
		return newErr(types.CodeSkills004, ReasonUnstampedBinary,
			"daemon version %q is not semver (an unstamped build reports 0.0.0-dev)", daemonVersion)
	}
	if got.less(want) {
		return newErr(types.CodeSkills004, ReasonFloor,
			"this daemon is %s, the artifact needs >= %s", got.raw, want.raw)
	}
	return nil
}

// GateModules is §4.6: the play's tool set must be a subset of
// allowed_modules, and every allowlisted tool must exist as a local descriptor
// (a missing descriptor for a tool the play never calls is reported elsewhere and
// does not block).
func GateModules(s types.Skill, play types.Play, registered []string) error {
	allowed := map[string]bool{}
	for _, m := range s.AllowedModules {
		allowed[m] = true
	}
	have := map[string]bool{}
	for _, r := range registered {
		have[r] = true
	}
	for _, task := range play.Tasks {
		if task.Tool == "" {
			return newErr(types.CodeSkills001, ReasonFieldRule, "a play task names no tool")
		}
		if !allowed[task.Tool] {
			return newErr(types.CodeSkills005, ReasonAllowlist,
				"the play calls %q which allowed_modules does not list", task.Tool)
		}
		if len(registered) > 0 && !have[task.Tool] {
			return newErr(types.CodeSkills013, ReasonMissingModule,
				"the play calls %q which this build does not register", task.Tool)
		}
	}
	return nil
}

// MissingModules reports the allowed_modules entries that have no local
// descriptor: recorded in the install payload, never blocking (§4.6).
func MissingModules(s types.Skill, registered []string) []string {
	have := map[string]bool{}
	for _, r := range registered {
		have[r] = true
	}
	var out []string
	for _, m := range s.AllowedModules {
		if !have[m] {
			out = append(out, m)
		}
	}
	return out
}

// GateOrder names the §4.2 gate sequence so a caller can assert first-failure-wins.
var GateOrder = []string{"schema", "signature", "modules", "floor", "canary", "approve", "conflict", "install"}

// Describe renders a gate failure for a human line (never a credential).
func Describe(err error) string {
	if err == nil {
		return "ok"
	}
	return fmt.Sprintf("%s (%s)", ReasonOf(err), CodeOf(err))
}
