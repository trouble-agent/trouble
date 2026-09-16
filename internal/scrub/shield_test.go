package scrub

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// SPEC-02 §7 shield_test.go: every configured public key survives verbatim in
// the output and no returned value contains \x00.

func TestShieldPublicKeysSurvive(t *testing.T) {
	e := newTestEngine(t, "")
	cases := []struct {
		name   string
		target types.ScrubTarget
		in     string
	}{
		{"alone", types.TgEventMsg, "registered project " + testPubKeyA + " for hooks.example"},
		{"in a dsn", types.TgEventMsg, "http://" + testPubKeyA + ":0123456789abcdef0123456789abcdef@hooks.example:7643/7"},
		{"in a dsn without secret", types.TgEventMsg, "http://" + testPubKeyA + "@hooks.example:7643/7"},
		{"in a message", types.TgEventMsg, "project " + testPubKeyB + " rejected the envelope"},
		{"in a stack", types.TgStack, "at main (http://" + testPubKeyA + "@hooks.example:7643/1 send)"},
		{"in a config snapshot", types.TgConfigSnapshot, `dsn = "http://` + testPubKeyB + `@hooks.example:7643/2"`},
		{"inside a private key block", types.TgJournalTail,
			"-----BEGIN RSA PRIVATE KEY-----\ncomment " + testPubKeyA + "\nMIIEow\n-----END RSA PRIVATE KEY-----\n"},
	}
	for _, c := range cases {
		out, _ := scrubString(t, e, c.target, "1", c.in)
		if bytes.Contains([]byte(out), []byte{0x00}) {
			t.Errorf("%s: output contains a NUL placeholder: %q", c.name, out)
		}
		for _, k := range []string{testPubKeyA, testPubKeyB} {
			if strings.Contains(c.in, k) {
				if !strings.Contains(out, k) {
					t.Errorf("%s: public key %s did not survive: in=%q out=%q", c.name, k, c.in, out)
				}
			}
		}
	}
	// the private key block around a shielded public key is still redacted
	out, res := scrubString(t, e, types.TgJournalTail, "1",
		"-----BEGIN RSA PRIVATE KEY-----\ncomment "+testPubKeyA+"\nMIIEow\n-----END RSA PRIVATE KEY-----\n")
	if !strings.Contains(out, "[REDACTED:private_key_block]") {
		t.Errorf("the private key block was not redacted: %q", out)
	}
	if res.ByRule["private_key_block"] != 1 {
		t.Errorf("by_rule = %s, want private_key_block=1", byRuleString(res.ByRule))
	}
}

// TestShieldNoPlaceholderEverReturned: no returned value, on any path, may carry
// a placeholder — a placeholder that reached disk would be a silent corruption
// of the designed-public credential (§3.5).
func TestShieldNoPlaceholderEverReturned(t *testing.T) {
	e := newTestEngine(t, "")
	payloads := []string{
		testPubKeyA, "x" + testPubKeyA + "y", "http://" + testPubKeyA + ":" + testPubKeyB + "@h/1",
		"password=" + testPubKeyA, "-----BEGIN PRIVATE KEY-----\n" + testPubKeyB + "\n",
		strings.Repeat(testPubKeyA+" ", 40),
	}
	for _, in := range payloads {
		for _, tg := range []types.ScrubTarget{types.TgEventMsg, types.TgHeader, types.TgDSN, types.TgSpool} {
			out, _, err := e.ScrubString(context.Background(), tg, "1", in)
			if err != nil {
				continue // fail-closed paths return no value at all
			}
			if bytes.Contains([]byte(out), []byte{0x00}) {
				t.Errorf("target %s: placeholder leaked into the output: %q", tg, out)
			}
		}
	}
}

func TestAllowedPublicKeys(t *testing.T) {
	projects := testProjects()
	projects = append(projects, types.Project{ID: "3", PublicKey: "", Enabled: true})
	projects = append(projects, types.Project{ID: "4", PublicKey: testPubKeyA, Enabled: false})
	got := AllowedPublicKeys(projects)
	if len(got) != 2 || got[0] != testPubKeyA || got[1] != testPubKeyB {
		t.Fatalf("AllowedPublicKeys = %v, want exactly the two configured keys (deduplicated, disabled still shielded)", got)
	}
	// exactly those keys are exempt: a third, unconfigured 32-hex value is not a
	// public key and is left alone only because no rule matches it
	e := newTestEngine(t, "")
	const stranger = "00112233445566778899aabbccddeeff"
	out, res := scrubString(t, e, types.TgEventMsg, "1", "project "+stranger+" reported")
	if out != "project "+stranger+" reported" || res.Redactions != 0 {
		t.Errorf("output = %q redactions = %d, want the input unchanged", out, res.Redactions)
	}
}
