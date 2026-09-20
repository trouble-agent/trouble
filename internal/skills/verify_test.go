package skills

import (
	"strings"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/loadfence"
	"github.com/trouble-agent/trouble/internal/types"
)

// TestVerifyHappyPathAndFailures pins §3.2's verification order and the -002/-003
// split for a locally signed fixture.
func TestVerifyHappyPathAndFailures(t *testing.T) {
	kp := newKeyPair(t, "skills-2026", types.TrustRelease)
	skill, signed := kp.sign(t, goldenArtifactTOML, playBytes())
	_ = skill
	signedSkill, err := LoadArtifact([]byte(signed), playBytes())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := Verify(signedSkill, playBytes(), []types.SkillSigner{kp.Signer}); err != nil {
		t.Fatalf("a correctly signed artifact must verify: %v", err)
	}

	// Wrong key: the signature is real but for another public key.
	other := newKeyPair(t, "skills-2026", types.TrustRelease)
	if err := Verify(signedSkill, playBytes(), []types.SkillSigner{other.Signer}); CodeOf(err) != types.CodeSkills002 {
		t.Fatalf("wrong key: code = %s (%v)", CodeOf(err), err)
	}
	// Unknown signer id.
	unknown := []types.SkillSigner{{KeyID: "skills-2027", PublicKey: kp.Signer.PublicKey,
		Trust: types.TrustRelease, Enabled: true}}
	if err := Verify(signedSkill, playBytes(), unknown); CodeOf(err) != types.CodeSkills003 {
		t.Fatalf("unknown signer: code = %s (%v)", CodeOf(err), err)
	}
	// Disabled signer.
	disabled := []types.SkillSigner{{KeyID: "skills-2026", PublicKey: kp.Signer.PublicKey,
		Trust: types.TrustRelease, Enabled: false}}
	if err := Verify(signedSkill, playBytes(), disabled); ReasonOf(err) != ReasonSignerDisabled {
		t.Fatalf("disabled signer: reason = %s (%v)", ReasonOf(err), err)
	}
	// A local key may not sign a pulled artifact for another host's author.
	local := []types.SkillSigner{{KeyID: "skills-2026", PublicKey: kp.Signer.PublicKey,
		Trust: types.TrustLocal, Enabled: true}}
	if err := GateSignature(signedSkill, playBytes(), local, true, "@7f3a91c2d4e5b607"); ReasonOf(err) != ReasonSignerTrust {
		t.Fatalf("local trust on a pulled artifact: reason = %s (%v)", ReasonOf(err), err)
	}
	// Truncated base64.
	truncated := signedSkill
	truncated.Signature = signedSkill.Signature[:40]
	if err := GateSignature(truncated, playBytes(), []types.SkillSigner{kp.Signer}, true, ""); CodeOf(err) != types.CodeSkills002 {
		t.Fatalf("truncated signature: code = %s", CodeOf(err))
	}
	// Space-padded base64.
	padded := signedSkill
	padded.Signature = strings.ReplaceAll(signedSkill.Signature, "=", " ")
	if err := GateSignature(padded, playBytes(), []types.SkillSigner{kp.Signer}, true, ""); CodeOf(err) != types.CodeSkills002 {
		t.Fatalf("space-padded signature: code = %s", CodeOf(err))
	}
	// A play edited after signing fails even though SKILL.toml is untouched.
	edited := append([]byte{}, playBytes()...)
	edited[len(edited)-2] = 'x'
	if err := Verify(signedSkill, edited, []types.SkillSigner{kp.Signer}); CodeOf(err) != types.CodeSkills002 {
		t.Fatalf("edited play: code = %s (%v)", CodeOf(err), err)
	}
	// require_signature=false is legal only for the local-source + review path, and
	// then an unsigned artifact is accepted by GateSignature.
	unsigned := signedSkill
	unsigned.Signature = ""
	if err := GateSignature(unsigned, playBytes(), nil, true, ""); CodeOf(err) != types.CodeSkills002 {
		t.Fatalf("unsigned artifact with require_signature: code = %s", CodeOf(err))
	}
	if err := GateSignature(unsigned, playBytes(), nil, false, ""); err != nil {
		t.Fatalf("unsigned artifact with require_signature=false: %v", err)
	}
}

// TestVerifyBudget pins §7 item 3's numeric budget (SPEC-11: 100
// parse+canonicalize+ed25519-verify cycles ≤ 200 ms). The 200 ms is a
// reference-host measurement (SPEC-01 §1 recorded the reference class; SPEC-01
// §7a is the calibration model), so it is scaled by the host's measured CPU
// speed and load via loadfence.CPUScale(): on a quiet reference-class host the
// scale is 1 and the spec number is enforced unchanged, on a slower box the bar
// follows that box's own measured cost, and TROUBLE_HOST_CALIB pins it for
// falsification. The artifact's ed25519 verify cost is host-invariant; the box
// is not. The -race build keeps its existing unconditional skip (the spec
// number is a plain-build measurement).
func TestVerifyBudget(t *testing.T) {
	if raceEnabled {
		t.Skip("absolute parse+verify budgets are measured without -race")
	}
	budget := time.Duration(float64(200*time.Millisecond) * loadfence.Measure(t.TempDir()).CPUScale())
	kp := newKeyPair(t, "skills-2026", types.TrustRelease)
	_, signed := kp.sign(t, goldenArtifactTOML, playBytes())
	start := time.Now()
	for i := 0; i < 100; i++ {
		skill, err := LoadArtifact([]byte(signed), playBytes())
		if err != nil {
			t.Fatalf("load %d: %v", i, err)
		}
		if err := Verify(skill, playBytes(), []types.SkillSigner{kp.Signer}); err != nil {
			t.Fatalf("verify %d: %v", i, err)
		}
	}
	if elapsed := time.Since(start); elapsed > budget {
		t.Fatalf("100 parse+canonicalize+verify cycles took %s, over the %s budget (200 ms x cpu-scale, SPEC-01 §7a)", elapsed, budget)
	}
}

// TestSemverFloorMatrix is the §4.3 table.
func TestSemverFloorMatrix(t *testing.T) {
	cases := []struct {
		min    string
		daemon string
		ok     bool
		reason string
	}{
		{"0.1.0", "0.1.0", true, ""},
		{"0.1.0", "0.1.1", true, ""},
		{"0.1.0", "0.0.9", false, ReasonFloor},
		{"0.1.0-rc1", "0.1.0", true, ""},
		{"0.1.0", "0.1.0-rc1", false, ReasonFloor},
		{"0.2.0", "0.1.9", false, ReasonFloor},
		{"0.1.0", "0.0.0-dev", false, ReasonFloor},
		{"0.1.0", "dev", false, ReasonUnstampedBinary},
		{"0.1.0", "", false, ReasonUnstampedBinary},
		{"v0.1.0", "0.1.0", true, ""},
		{"0.1.0+build7", "0.1.0", true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.min+"_"+tc.daemon, func(t *testing.T) {
			skill := types.Skill{MinDaemonVersion: tc.min}
			err := GateFloor(skill, tc.daemon)
			if tc.ok && err != nil {
				t.Fatalf("floor %s with %s must pass: %v", tc.min, tc.daemon, err)
			}
			if !tc.ok {
				if err == nil {
					t.Fatalf("floor %s with %s must fail", tc.min, tc.daemon)
				}
				if got := ReasonOf(err); got != tc.reason {
					t.Fatalf("reason = %s, want %s", got, tc.reason)
				}
				if CodeOf(err) != types.CodeSkills004 {
					t.Fatalf("code = %s", CodeOf(err))
				}
			}
		})
	}
}

// TestGateModules pins §4.6: the play's tools must be inside allowed_modules, and
// a tool this build does not register is a refusal when the play calls it.
func TestGateModules(t *testing.T) {
	skill := types.Skill{AllowedModules: []string{"proc.connections", "service.reload"}}
	play, err := DecodePlay(playBytes())
	if err != nil {
		t.Fatalf("play: %v", err)
	}
	// The fixture play calls config.set as well.
	err = GateModules(skill, play, []string{"proc.connections", "service.reload", "config.set"})
	if CodeOf(err) != types.CodeSkills005 {
		t.Fatalf("a play tool outside allowed_modules must be 005, got %s (%v)", CodeOf(err), err)
	}
	skill.AllowedModules = append(skill.AllowedModules, "config.set")
	if err := GateModules(skill, play, []string{"proc.connections", "service.reload", "config.set"}); err != nil {
		t.Fatalf("a fully allowed play must pass: %v", err)
	}
	if err := GateModules(skill, play, []string{"proc.connections"}); CodeOf(err) != types.CodeSkills013 ||
		ReasonOf(err) != ReasonMissingModule {
		t.Fatalf("an unregistered tool must be 013/missing_module, got %s/%s", CodeOf(err), ReasonOf(err))
	}
	// An allowlist entry with no local descriptor does not block.
	if missing := MissingModules(skill, []string{"proc.connections"}); len(missing) != 2 {
		t.Fatalf("missing modules = %v, want 2", missing)
	}
}

// TestDecodePlayRejectsUnknownKeys proves the play schema is closed on this path
// too (the decoder is SPEC-06's).
func TestDecodePlayRejectsUnknownKeys(t *testing.T) {
	bad := strings.Replace(playFixture, "on_fail  = \"abort\"", "on_fail  = \"abort\"\nshell    = \"sh -c whoami\"", 1)
	if _, err := DecodePlay([]byte(bad)); err == nil {
		t.Fatalf("an unknown play key must be refused")
	}
}
