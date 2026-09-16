package scrub

import (
	"context"
	"strings"
	"testing"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// SPEC-02 §7 dsn_test.go: DSN with secret, without secret, percent-encoded,
// embedded in a traceback, and Project.SecretKey echoed into a record — the
// secret bytes must be absent from all five.

const testSecretKey = "00112233445566778899aabbccddeeff"

func TestDSNWithSecret(t *testing.T) {
	e := newTestEngine(t, "")
	in := "envelope rejected: http://" + testPubKeyA + ":" + testSecretKey + "@hooks.example:7643/7"
	out, res := scrubString(t, e, types.TgEventMsg, "1", in)
	if strings.Contains(out, testSecretKey) {
		t.Errorf("the DSN secret survived: %q", out)
	}
	if !strings.Contains(out, "[REDACTED:dsn_secret]") {
		t.Errorf("output = %q, want the dsn_secret marker", out)
	}
	if !strings.Contains(out, testPubKeyA) {
		t.Errorf("the public key did not survive: %q", out)
	}
	if !strings.Contains(out, "hooks.example:7643/7") {
		t.Errorf("the diagnostic remainder was lost: %q", out)
	}
	if res.ByRule["dsn_secret"] != 1 {
		t.Errorf("by_rule = %s, want dsn_secret=1", byRuleString(res.ByRule))
	}
}

func TestDSNWithoutSecretIsByteIdentical(t *testing.T) {
	e := newTestEngine(t, "")
	in := "http://" + testPubKeyA + "@hooks.example:7643/7"
	out, res := scrubString(t, e, types.TgEventMsg, "1", in)
	if out != in {
		t.Errorf("output = %q, want the input unchanged (canonical public-key-only DSN)", out)
	}
	if res.Redactions != 0 {
		t.Errorf("redactions = %d, want 0", res.Redactions)
	}
}

func TestDSNPercentEncodedSecret(t *testing.T) {
	e := newTestEngine(t, "")
	// the secret half is percent-encoded, so dsn_secret (32-hex grammar) does not
	// match it: dsn_any drops everything between "://" and "@" that is not a
	// 32-hex public key (§6)
	in := "http://" + testPubKeyA + ":abc%2Fdef%2Bghi@hooks.example:7643/7"
	out, res := scrubString(t, e, types.TgEventMsg, "1", in)
	if strings.Contains(out, "abc%2Fdef") {
		t.Errorf("the percent-encoded secret survived: %q", out)
	}
	if out != "http://"+testPubKeyA+"@hooks.example:7643/7" {
		t.Errorf("output = %q, want the canonical public-key-only form", out)
	}
	if res.ByRule["dsn_any"] != 1 {
		t.Errorf("by_rule = %s, want dsn_any=1", byRuleString(res.ByRule))
	}
}

func TestDSNInTraceback(t *testing.T) {
	e := newTestEngine(t, "")
	in := strings.Join([]string{
		"Traceback (most recent call last):",
		`  File "/srv/app/worker.py", line 41, in send`,
		"    client = Client(dsn=\"https://" + testPubKeyB + ":" + testSecretKey + "@hooks.example:7643/2\")",
		"sentry_sdk.errors.LoggingTransportError: failed to send",
	}, "\n")
	out, _ := scrubString(t, e, types.TgStack, "2", in)
	if strings.Contains(out, testSecretKey) {
		t.Errorf("the DSN secret survived the traceback: %q", out)
	}
	if !strings.Contains(out, testPubKeyB) {
		t.Errorf("the public key did not survive: %q", out)
	}
	if !strings.Contains(out, "worker.py") {
		t.Errorf("the diagnostic frames were lost: %q", out)
	}
}

func TestDSNProjectSecretKeyEcho(t *testing.T) {
	e := newTestEngine(t, "")
	in := `{"project":{"id":"1","public_key":"` + testPubKeyA + `","secret_key":"` + testSecretKey + `"}}`
	out, _ := scrubString(t, e, types.TgDSN, "1", in)
	if strings.Contains(out, testSecretKey) {
		t.Errorf("Project.SecretKey survived: %q", out)
	}
	if !strings.Contains(out, testPubKeyA) {
		t.Errorf("the public key did not survive: %q", out)
	}
	// the raw payload, before scrubbing, is refused at the persistence boundary
	// (fail closed: a caller that forgot to scrub does not get to persist it)
	if err := e.Verify(context.Background(), []byte(in)); err == nil {
		t.Error("Verify accepted an unredacted Project.SecretKey echo")
	}
	// and after the scrubber has run, the same bytes pass the boundary
	if err := e.Verify(context.Background(), []byte(out)); err != nil {
		t.Errorf("Verify refused the scrubbed payload: %v", err)
	}
}

// TestDSNAnyMarkerIsFixedPoint: a DSN whose secret half is already a marker is
// left alone (vector P11's output has to be a fixed point for hub re-scrubbing
// to be byte-identical).
func TestDSNAnyMarkerIsFixedPoint(t *testing.T) {
	e := newTestEngine(t, "")
	in := "http://" + testPubKeyA + ":[REDACTED:dsn_secret]@hooks.example:7643/7"
	out, res := scrubString(t, e, types.TgEventMsg, "1", in)
	if out != in {
		t.Errorf("output = %q, want the input unchanged", out)
	}
	if res.Redactions != 0 {
		t.Errorf("redactions = %d, want 0", res.Redactions)
	}
}

// TestDSNSecretKeyEchoWithoutTarget: a project secret echoed into a payload with
// no declared target is refused at the boundary (SCRUB-008).
func TestDSNSecretKeyEchoWithoutTarget(t *testing.T) {
	e := newTestEngine(t, "")
	in := []byte(`{"dsn":"http://` + testPubKeyA + ":" + testSecretKey + `@hooks.example:7643/7"}`)
	if err := e.Verify(context.Background(), in); err == nil {
		t.Fatal("Verify accepted an unredacted DSN with a secret half")
	} else if CodeOf(err) != types.CodeScrub008 {
		t.Errorf("code = %s, want %s", CodeOf(err), types.CodeScrub008)
	}
	if e.Stats().BoundaryRefusals == 0 {
		t.Error("boundary_refusals did not increment")
	}
}
