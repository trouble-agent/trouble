package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// TestGoldenCanonicalJSON pins the §3.2 projection byte-for-byte.
func TestGoldenCanonicalJSON(t *testing.T) {
	skill, err := LoadArtifact([]byte(goldenArtifactTOML), playBytes())
	if err != nil {
		t.Fatalf("the §3.1 fixture must load: %v", err)
	}
	got, err := CanonicalJSON(skill)
	if err != nil {
		t.Fatalf("canonical json: %v", err)
	}
	if got != GoldenCanonicalJSON {
		t.Fatalf("canonical projection drift:\n--- got ---\n%s\n--- want ---\n%s", got, GoldenCanonicalJSON)
	}
}

// TestGoldenCanonicalFormPinnedSignature is the golden vector's verifiable half:
// the canonical bytes built with the spec's own play digest are exactly 662 bytes,
// hash to the spec's pinned digest, and the spec's pinned ed25519 signature
// verifies over them with the spec's pinned public key.
//
// SPEC-11 §3.2 references a §7.1 play fixture that does not exist in the spec, so
// the play bytes themselves cannot be reproduced; everything downstream of them
// is asserted here.
func TestGoldenCanonicalFormPinnedSignature(t *testing.T) {
	skill, err := LoadArtifact([]byte(goldenArtifactTOML), playBytes())
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	canonical, err := canonicalBytesWithPlayHash(skill, goldenPlaySHA256)
	if err != nil {
		t.Fatalf("canonical bytes: %v", err)
	}
	if len(canonical) != 662 {
		t.Fatalf("canonical bytes are %d bytes, want 662", len(canonical))
	}
	if got := sha256Hex(canonical); got != goldenCanonicalSHA256 {
		t.Fatalf("canonical sha256 = %s, want %s", got, goldenCanonicalSHA256)
	}
	if !strings.HasPrefix(string(canonical), "trouble.skill.v1\n") {
		t.Fatalf("canonical bytes lack the generation prefix: %q", string(canonical)[:32])
	}
	if !strings.HasSuffix(string(canonical), "play_sha256="+goldenPlaySHA256+"\n") {
		t.Fatalf("canonical bytes do not end with the play digest line")
	}
	// The pinned signature must verify with the pinned key over exactly these
	// bytes: the projection is the signed material, so this is the strongest
	// available proof that the canonical form is the spec's.
	signers := []types.SkillSigner{{
		KeyID: "skills-2026", PublicKey: goldenPublicKey, Trust: types.TrustRelease,
		Enabled: true, AddedTS: "2026-09-01T00:00:00.000Z",
	}}
	if !verifyCanonical(canonical, goldenSignature, signers[0]) {
		t.Fatalf("the spec's pinned signature does not verify over the canonical bytes")
	}
	if verifyCanonical(append([]byte("x"), canonical...), goldenSignature, signers[0]) {
		t.Fatalf("a mutated canonical form verified: the check is not over the bytes")
	}
}

// TestCanonicalFormInvariance pins the property that makes byte-signing wrong:
// TOML has no canonical form, so comments, key order, inline-vs-table layout and
// whitespace must not change the signed material.
func TestCanonicalFormInvariance(t *testing.T) {
	base, err := LoadArtifact([]byte(goldenArtifactTOML), playBytes())
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	baseJSON, _ := CanonicalJSON(base)

	variants := []string{
		// comments
		"# a comment\n" + goldenArtifactTOML,
		strings.Replace(goldenArtifactTOML, "version            = 3", "version = 3 # inline", 1),
		// whitespace
		strings.ReplaceAll(goldenArtifactTOML, " = ", "   =   "),
		// reordered top-level keys
		`version            = 3
name               = "payment-worker-queue-wedge"
play_ref           = "plays/payment-worker-queue-wedge@3.toml"
sigs               = ["sentinel:sha256v1:9f2c1d3e4b5a6c7d", "journald:sha256v1:2ab4c6d8e0f1a3b5"]
signer_key_id      = "skills-2026"
signature          = "EhHNesvuVNHJa+6HNj2xKS0FGfHrs76+pmGNUBB7+hVzHMphJ/GJD7NKwGolDngaoqL/m/Rq/MRjk6wBb17ZBA=="
min_daemon_version = "0.1.0"
allowed_modules    = ["proc.connections", "service.reload", "config.set"]

[provenance]
incidents  = ["inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE"]
research   = ["res_01J9Z6Q0M2X4T8V1K7B3N5R8WL"]
author     = "troubled@hostA"
created_ts = "2026-09-11T04:00:00.000Z"

[guards]
verify_window = "10m"
max_runs      = "3/day"
escalate_on   = "verify_fail"
`,
	}
	for i, v := range variants {
		skill, err := LoadArtifact([]byte(v), playBytes())
		if err != nil {
			t.Fatalf("variant %d does not load: %v", i, err)
		}
		got, err := CanonicalJSON(skill)
		if err != nil {
			t.Fatalf("variant %d canonical: %v", i, err)
		}
		if got != baseJSON {
			t.Fatalf("variant %d changed the canonical form:\n%s", i, got)
		}
	}
}

// TestCanonicalFormNonInvariance pins what must change the signed material.
func TestCanonicalFormNonInvariance(t *testing.T) {
	base, err := LoadArtifact([]byte(goldenArtifactTOML), playBytes())
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	baseJSON, _ := CanonicalJSON(base)

	// sigs order is signed (matching is set-based, the artifact is not).
	reordered := base
	reordered.Sigs = []string{base.Sigs[1], base.Sigs[0]}
	if got, _ := CanonicalJSON(reordered); got == baseJSON {
		t.Fatalf("reordering sigs did not change the canonical form")
	}
	// signer_key_id is signed, so swapping the key id invalidates the signature.
	swapped := base
	swapped.SignerKeyID = "skills-2027"
	if got, _ := CanonicalJSON(swapped); got == baseJSON {
		t.Fatalf("changing signer_key_id did not change the canonical form")
	}
	// A one-byte play edit changes the payload digest.
	b1, err := CanonicalBytes(base, playBytes())
	if err != nil {
		t.Fatalf("canonical bytes: %v", err)
	}
	edited := append([]byte{}, playBytes()...)
	edited[len(edited)-1] = 'q'
	b2, err := CanonicalBytes(base, edited)
	if err != nil {
		t.Fatalf("canonical bytes: %v", err)
	}
	if string(b1) == string(b2) {
		t.Fatalf("a one-byte play edit did not change the signed material")
	}
}

// TestCanonicalBytesRejectsMissingPlay pins §3.2 step 3/7.
func TestCanonicalBytesRejectsMissingPlay(t *testing.T) {
	skill, err := LoadArtifact([]byte(goldenArtifactTOML), playBytes())
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if _, err := CanonicalBytes(skill, nil); err == nil {
		t.Fatalf("canonical bytes with no play must fail")
	}
}

// TestArtifactStrictSchema pins §3.1: the accepted key set is exact, and every
// invalid fixture yields TROUBLE-SKILLS-001 with the expected reason.
func TestArtifactStrictSchema(t *testing.T) {
	cases := []struct {
		name   string
		toml   string
		reason string
	}{
		{"unknown key", goldenArtifactTOML + "\nshell = \"/bin/sh -c whoami\"\n", ReasonUnknownKey},
		{"unknown table", goldenArtifactTOML + "\n[exec]\ncmd = \"rm -rf /\"\n", ReasonUnknownTable},
		{"stats table", goldenArtifactTOML + "\n[stats]\napplied = 4\n", ReasonStatsInArtifact},
		{"free-text sig", replaceOnce(goldenArtifactTOML, `"journald:sha256v1:2ab4c6d8e0f1a3b5"`, `"journal:payment-worker:restart-loop"`), ReasonSigGrammar},
		{"uppercase hex sig", replaceOnce(goldenArtifactTOML, `"journald:sha256v1:2ab4c6d8e0f1a3b5"`, `"journald:sha256v1:2AB4C6D8E0F1A3B5"`), ReasonSigGrammar},
		{"0x hex sig", replaceOnce(goldenArtifactTOML, `"journald:sha256v1:2ab4c6d8e0f1a3b5"`, `"journald:sha256v1:0x2ab4c6d8e0f1a3b5"`), ReasonSigGrammar},
		{"17-hex sig", replaceOnce(goldenArtifactTOML, `"journald:sha256v1:2ab4c6d8e0f1a3b5"`, `"journald:sha256v1:2ab4c6d8e0f1a3b5c"`), ReasonSigGrammar},
		{"unknown sig source", replaceOnce(goldenArtifactTOML, `"journald:sha256v1:2ab4c6d8e0f1a3b5"`, `"syslog:sha256v1:2ab4c6d8e0f1a3b5"`), ReasonSigGrammar},
		{"duplicate sigs", replaceOnce(goldenArtifactTOML, `"journald:sha256v1:2ab4c6d8e0f1a3b5"`, `"sentinel:sha256v1:9f2c1d3e4b5a6c7d"`), ReasonFieldRule},
		{"empty sigs", replaceOnce(goldenArtifactTOML, `sigs               = ["sentinel:sha256v1:9f2c1d3e4b5a6c7d", "journald:sha256v1:2ab4c6d8e0f1a3b5"]`, `sigs               = []`), ReasonFieldRule},
		{"version 0", replaceOnce(goldenArtifactTOML, "version            = 3", "version            = 0"), ReasonFieldRule},
		{"name with underscore", replaceOnce(goldenArtifactTOML, `name               = "payment-worker-queue-wedge"`, `name               = "payment_worker"`), ReasonFieldRule},
		{"play_ref with ..", replaceOnce(goldenArtifactTOML, `play_ref           = "plays/payment-worker-queue-wedge@3.toml"`, `play_ref           = "../plays/x@3.toml"`), ReasonFieldRule},
		{"play_ref absolute", replaceOnce(goldenArtifactTOML, `play_ref           = "plays/payment-worker-queue-wedge@3.toml"`, `play_ref           = "/plays/x@3.toml"`), ReasonFieldRule},
		{"play_ref url", replaceOnce(goldenArtifactTOML, `play_ref           = "plays/payment-worker-queue-wedge@3.toml"`, `play_ref           = "https://evil.example/x@3.toml"`), ReasonFieldRule},
		{"allowed_modules glob", replaceOnce(goldenArtifactTOML, `allowed_modules    = ["proc.connections", "service.reload", "config.set"]`, `allowed_modules    = ["proc.*"]`), ReasonFieldRule},
		{"bad created_ts", replaceOnce(goldenArtifactTOML, `created_ts = "2026-09-11T04:00:00.000Z"`, `created_ts = "2026-09-11"`), ReasonFieldRule},
		{"bad min_daemon_version", replaceOnce(goldenArtifactTOML, `min_daemon_version = "0.1.0"`, `min_daemon_version = "one"`), ReasonFieldRule},
		{"bad escalate_on", replaceOnce(goldenArtifactTOML, `escalate_on   = "verify_fail"`, `escalate_on   = "panic"`), ReasonFieldRule},
		{"bad max_runs", replaceOnce(goldenArtifactTOML, `max_runs      = "3/day"`, `max_runs      = "3/hour"`), ReasonFieldRule},
		{"verify_window too long", replaceOnce(goldenArtifactTOML, `verify_window = "10m"`, `verify_window = "2h"`), ReasonFieldRule},
		{"empty provenance", replaceOnce(replaceOnce(goldenArtifactTOML,
			`incidents  = ["inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE"]`, `incidents  = []`),
			`research   = ["res_01J9Z6Q0M2X4T8V1K7B3N5R8WL"]`, `research   = []`), ReasonProvenance},
		{"empty author", replaceOnce(goldenArtifactTOML, `author     = "troubled@hostA"`, `author     = ""`), ReasonFieldRule},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadArtifact([]byte(tc.toml), playBytes())
			if err == nil {
				t.Fatalf("fixture must be rejected")
			}
			if code := CodeOf(err); code != types.CodeSkills001 {
				t.Fatalf("code = %s, want TROUBLE-SKILLS-001 (%v)", code, err)
			}
			if reason := ReasonOf(err); reason != tc.reason {
				t.Fatalf("reason = %s, want %s (%v)", reason, tc.reason, err)
			}
		})
	}
	// The PRD §06c artifact byte-for-byte: `[play]` with list syntax is not valid
	// TOML, and its sigs are free text. It must be refused, not parsed loosely.
	prd := `name = "payment-worker-queue-wedge"
version = 3
sigs = ["journal:payment-worker:restart-loop"]
play_ref = "plays/x@3.toml"
min_daemon_version = "0.1.0"
allowed_modules = ["proc.connections"]
signer_key_id = "skills-2026"

[play]
- tool = "proc.connections"
- tool = "service.reload"
`
	if _, err := LoadArtifact([]byte(prd), playBytes()); err == nil {
		t.Fatalf("the PRD's own artifact must be refused")
	}
}

func TestArtifactParseBudget(t *testing.T) {
	start := time.Now()
	for i := 0; i < 1000; i++ {
		if _, err := LoadArtifact([]byte(goldenArtifactTOML), playBytes()); err != nil {
			t.Fatalf("parse %d: %v", i, err)
		}
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond && !raceEnabled {
		t.Fatalf("1000 artifacts parsed in %s, over the 500ms budget", elapsed)
	}
}

// TestArtifactLoadsFromDisk proves play_ref resolution against a real tree.
func TestArtifactLoadsFromDisk(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "plays"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.toml"), []byte(goldenArtifactTOML), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	skill, play, _, err := readArtifact(dir)
	if err != nil {
		// The play is missing: that is the §3.2 step-3 failure, asserted next.
		if CodeOf(err) != types.CodeSkills001 || ReasonOf(err) != ReasonPlayMissing {
			t.Fatalf("missing play gave %v", err)
		}
	} else {
		t.Fatalf("readArtifact found a play that is not there: %+v", skill)
	}
	if err := os.WriteFile(filepath.Join(dir, "plays", "payment-worker-queue-wedge@3.toml"), playBytes(), 0o600); err != nil {
		t.Fatalf("write play: %v", err)
	}
	skill, play, raw, err := readArtifact(dir)
	if err != nil {
		t.Fatalf("readArtifact: %v", err)
	}
	if skill.Name != "payment-worker-queue-wedge" || len(play) == 0 || len(raw) == 0 {
		t.Fatalf("readArtifact returned %+v", skill)
	}
}

func replaceOnce(s, old, new string) string {
	if !strings.Contains(s, old) {
		panic("fixture text not found: " + old)
	}
	return strings.Replace(s, old, new, 1)
}
