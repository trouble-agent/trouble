package scrub

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// SPEC-02 §7 "AC-derived tests". AC-18 and AC-22 exercise the *scrubbing* half
// of the two acceptance criteria: the one-group/one-issue half belongs to SPEC-05
// and the quota half to SPEC-04. The ledger-side assertions (grep -F over the
// hub's ledger, and Verify over the persisted bytes) live in
// e2e_ledger_test.go, which can import internal/ledger.

const (
	// the same config bytes for every engine: the rule table is the authority
	ac18Config = "[scrub]\npii_mode = \"keep\"\npath_mode = \"keep\"\n"
	ac18HostA  = "7f3a91c2d4e5b607"
	ac18HostB  = "c0ffee1234567890"
)

func digestOf(t *testing.T, b []byte) string {
	t.Helper()
	return hex.EncodeToString(types.SigDigest(b))
}

// TestAC18ScrubAcrossTheWire: two hosts, two arrival paths (a Go SDK envelope and
// a curl'd JSON body), one bug.
func TestAC18ScrubAcrossTheWire(t *testing.T) {
	// both hosts run the same rule table (SPEC-02 §3.3: tables_version is the
	// rule-table identity), built from the same config bytes
	hostA := newTestEngine(t, ac18Config)
	hub := newTestEngine(t, ac18Config)

	const secretA = "0123456789abcdef0123456789abcdef"
	const secretB = "fedcba9876543210fedcba9876543210"
	const pwA = "goSdkPassword1"
	const pwB = "curlPassword22"

	// host A: the Go SDK posts an envelope whose exception message carries the
	// DSN with its secret half
	sdkMessage := "queue wedge: pool exhausted dsn=http://" + testPubKeyA + ":" + secretA + "@hooks.example:7643/7"
	sdkStack := "at worker.flush (postgres://app:" + pwA + "@db.internal:5432/app)"
	// host B: curl posts a JSON body whose stack embeds a connection string
	curlMessage := "queue wedge: pool exhausted dsn=http://" + testPubKeyA + ":" + secretB + "@hooks.example:7643/7"
	curlStack := "at worker.flush (postgres://app:" + pwB + "@db.internal:5432/app)"

	scrub25 := func(e *Engine, hostID, message, stack string) ([]byte, types.ScrubResult) {
		t.Helper()
		out, res, err := e.ScrubFields(context.Background(), "1", map[string][]byte{
			"message": []byte(message), "stack": []byte(stack),
		}, map[string]types.ScrubTarget{"message": types.TgEventMsg, "stack": types.TgStack})
		if err != nil {
			t.Fatalf("host %s: %v", hostID, err)
		}
		canon := append(append([]byte{}, out["message"]...), '\n')
		canon = append(canon, out["stack"]...)
		return canon, res
	}

	wireA, resA := scrub25(hostA, ac18HostA, sdkMessage, sdkStack)
	wireB, resB := scrub25(hostA, ac18HostB, curlMessage, curlStack)

	// the scripts differ, the scrubbed bytes do not: this is what makes the two
	// hosts' records the same group on the hub (AC-22's mechanism)
	if string(wireA) != string(wireB) {
		t.Errorf("the two arrival paths did not normalise to the same bytes:\n  A %q\n  B %q", wireA, wireB)
	}
	if digestOf(t, wireA) != digestOf(t, wireB) {
		t.Error("the canonical digests differ across the wire")
	}
	rawA := sdkMessage + "\n" + sdkStack
	rawB := curlMessage + "\n" + curlStack
	if digestOf(t, []byte(rawA)) == digestOf(t, []byte(rawB)) {
		t.Fatal("the raw payloads already digest equally: the test cannot show what the scrubber contributes")
	}

	// both secrets are gone from what crosses the wire
	for _, s := range []string{secretA, secretB, pwA, pwB} {
		if strings.Contains(string(wireA), s) {
			t.Errorf("secret %q survived into the wire bytes: %q", s, wireA)
		}
	}
	if resA.Redactions == 0 || resB.Redactions == 0 {
		t.Errorf("redactions A=%d B=%d, want both > 0", resA.Redactions, resB.Redactions)
	}

	// the hub re-scrubs what it received: idempotent, so Redactions == 0 and the
	// sig the hub computes is the sig the satellite computed
	hubBytes, hubRes, err := hub.ScrubString(context.Background(), types.TgEventMsg, "1", string(wireA))
	if err != nil {
		t.Fatalf("hub re-scrub: %v", err)
	}
	if hubRes.Redactions != 0 {
		t.Errorf("hub re-scrub reported %d redactions (%s), want 0", hubRes.Redactions, byRuleString(hubRes.ByRule))
	}
	if hubBytes != string(wireA) {
		t.Errorf("hub re-scrub changed the bytes:\n  in  %q\n  out %q", wireA, hubBytes)
	}
	if digestOf(t, []byte(hubBytes)) != digestOf(t, wireA) {
		t.Error("the hub's digest differs from the satellite's")
	}
	// and the boundary re-scan accepts it
	if err := hub.Verify(context.Background(), wireB); err != nil {
		t.Errorf("the hub's boundary re-scan refused the satellite's record: %v", err)
	}
}

// TestAC22ScrubDeterminism: the same bug described three ways (a sensor detail, a
// sentinel SDK event and a collector journal line) normalises to one scrubbed
// byte string and one digest — which is what makes "same bug" mean "same group".
func TestAC22ScrubDeterminism(t *testing.T) {
	e := newTestEngine(t, ac18Config)
	const secret = "0123456789abcdef0123456789abcdef"
	const msg = "pool exhausted: connection to http://" + testPubKeyA + ":" + secret +
		"@hooks.example:7643/7 refused after 30s"

	paths := []struct {
		name   string
		target types.ScrubTarget
		in     string
	}{
		// SPEC-03's numeric sensors take no scrub call (their payload is numbers
		// plus configured names); the incident detail the ladder records is the
		// text that reaches the ledger, and that is what is scrubbed here.
		{"sensor event detail", types.TgEventMsg, msg},
		{"sentinel sdk event", types.TgEventMsg, msg},
		{"collector journal line", types.TgJournalTail, "2026-09-16T09:14:03.221Z host app[1]: " + msg + "\n"},
	}
	outs := make([]string, 0, len(paths))
	for _, p := range paths {
		out, res := scrubString(t, e, p.target, "1", p.in)
		if strings.Contains(out, secret) {
			t.Fatalf("%s: the seeded secret survived: %q", p.name, out)
		}
		if res.Redactions != 1 {
			t.Errorf("%s: redactions = %d, want 1", p.name, res.Redactions)
		}
		// the normalisation step of the pipeline (SPEC-01 §3.4's canonical bytes)
		// strips the journal line's prefix and trailing newline
		out = strings.TrimSuffix(out, "\n")
		if i := strings.Index(out, "pool exhausted"); i > 0 {
			out = out[i:]
		}
		outs = append(outs, out)
	}
	if outs[0] != outs[1] || outs[1] != outs[2] {
		t.Errorf("the three paths did not converge:\n  sensor   %q\n  sentinel %q\n  journal  %q", outs[0], outs[1], outs[2])
	}
	d := digestOf(t, []byte(outs[0]))
	for i, o := range outs {
		if digestOf(t, []byte(o)) != d {
			t.Errorf("path %d digest differs: %s vs %s", i, digestOf(t, []byte(o)), d)
		}
	}
}
