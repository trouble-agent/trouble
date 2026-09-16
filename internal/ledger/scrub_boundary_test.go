package ledger

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// TestMandatoryRescanRefuses: a payload matching a mandatory rule is refused,
// no record is written, the propagated code is TROUBLE-SCRUB-008, the refusal
// counter moves and the value appears nowhere in the ledger bytes.
func TestMandatoryRescanRefuses(t *testing.T) {
	clk := newFakeClock(testNow())
	l := testLedger(t, clk, func(o *Options) { o.AssertScrubbed = true })
	const secret = "hunter2swordfish"
	_, err := l.Append(context.Background(), types.RecordDraft{
		Kind: types.KEvent, Sig: "journald:sha256v1:secret",
		Origin: types.Origin{HostID: "h", Source: "journald"}, Actor: testActor(),
		Payload: map[string]any{
			"op":      "sample",
			"message": "connecting with PASSWORD=" + secret + " now",
		},
	})
	if err == nil {
		t.Fatalf("a payload containing a mandatory-rule match was accepted")
	}
	if got := CodeOf(err); got != types.CodeScrub008 {
		t.Errorf("code = %s, want %s (propagated unchanged from SPEC-02)", got, types.CodeScrub008)
	}
	if l.ScrubRefusals() != 1 {
		t.Errorf("scrub_refusal_total = %d, want 1", l.ScrubRefusals())
	}
	if filesContain(t, l.Root(), secret) {
		t.Fatalf("the refused value appears in the ledger bytes")
	}
	// the ledger still works
	rec := mustAppend(t, l, eventDraft("psi", "psi:sha256v1:clean", "01", 0))
	if rec.Seq == 0 {
		t.Errorf("the ledger did not accept the next clean record")
	}
}

func filesContain(t *testing.T, root, needle string) bool {
	t.Helper()
	ents, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, e.Name()))
		if err != nil {
			continue
		}
		if strings.Contains(string(b), needle) {
			return true
		}
	}
	return false
}

// TestScanBudget: ≤50 µs per 4 KiB payload (SPEC-01 §4.2).
func TestScanBudget(t *testing.T) {
	payload := make([]byte, 4096)
	for i := range payload {
		payload[i] = byte('a' + i%26)
	}
	// warm up so the measurement is not dominated by first-call costs
	for i := 0; i < 100; i++ {
		_ = scanForTest(t, payload)
	}
	const runs = 5000
	start := time.Now()
	for i := 0; i < runs; i++ {
		if err := scanForTest(t, payload); err != nil {
			t.Fatalf("clean payload refused: %v", err)
		}
	}
	per := time.Since(start) / runs
	if raceEnabled {
		// the ≤50 µs budget is a host measurement; -race costs 5-10x
		if per > 5*time.Millisecond {
			t.Errorf("MandatoryScan = %s per 4 KiB payload under -race", per)
		}
		t.Logf("MandatoryScan under -race: %s per 4 KiB payload (budget 50 µs measured without -race)", per)
		return
	}
	if per > 50*time.Microsecond {
		t.Errorf("MandatoryScan = %s per 4 KiB payload, budget is 50 µs", per)
	}
	t.Logf("MandatoryScan: %s per 4 KiB payload (budget 50 µs)", per)
}

func scanForTest(t *testing.T, b []byte) error {
	t.Helper()
	return mandatoryScan(b)
}

// TestPrefilterDoesNotGateOutMatches: the performance prefilter must never
// disable a mandatory rule. One fixture per rule is checked twice: the rule must
// match it AND the prefilter must let it through to the rules.
func TestPrefilterDoesNotGateOutMatches(t *testing.T) {
	fixtures := map[string]string{
		"env_assign":   `{"env":"PAYMENT_DB_PASSWORD=hunter2swordfish"}`,
		"bearer_token": `{"header":"authorization: Bearer sk-live-abcdefghijklmnop"}`,
		"private_key":  `{"body":"-----BEGIN RSA PRIVATE KEY-----\nMIIEow"}`,
		"dsn_secret":   `{"dsn":"sentry://0123456789abcdef0123456789abcdef:fedcba9876543210abcdef@host/1"}`,
		"conn_string":  `{"url":"postgres://user:s3cretpw@db.internal:5432/app"}`,
	}
	for rule, body := range fixtures {
		if !prefilterAllows([]byte(body)) {
			t.Errorf("the prefilter gates out the %s fixture: the rule can never fire", rule)
		}
		if err := mandatoryScan([]byte(body)); err == nil {
			t.Errorf("rule %s did not match its fixture %q", rule, body)
		}
	}
	// a very common shape used to regress the env rule: the keyword is the whole
	// env name and the assignment is at a word boundary
	for _, body := range []string{`PASSWORD=x1y2z3`, `SECRET=x1y2z3`, `TOKEN:abcdefgh`, `API_KEY = 1234567890`} {
		if err := mandatoryScan([]byte(body)); err == nil {
			t.Errorf("env_assign missed %q", body)
		}
	}
}

// TestRedactionCountOnly: Record.Redactions equals the scrubber's count and no
// ByRule value ever reaches the ledger.
func TestRedactionCountOnly(t *testing.T) {
	clk := newFakeClock(testNow())
	l := testLedger(t, clk, func(o *Options) { o.AssertScrubbed = true })
	rec := mustAppend(t, l, eventDraft("psi", "psi:sha256v1:redacted", "01", 3))
	if rec.Redactions != 3 {
		t.Errorf("Redactions = %d, want 3 (the count, never the value)", rec.Redactions)
	}
	body, err := os.ReadFile(filepath.Join(l.Root(), testNow().Format(dayLayout)+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "by_rule") {
		t.Errorf("a by_rule map leaked into the ledger")
	}
	if !strings.Contains(string(body), `"redactions":3`) {
		t.Errorf("the redaction count is not on the record: %s", string(body))
	}
}

// TestScrubRefusalWritesNothing: the refusal happens before serialization, so a
// refused append leaves no partial line and no seq hole.
func TestScrubRefusalWritesNothing(t *testing.T) {
	clk := newFakeClock(testNow())
	l := testLedger(t, clk, func(o *Options) { o.AssertScrubbed = true })
	before := l.Status()
	_, err := l.Append(context.Background(), types.RecordDraft{
		Kind: types.KEvent, Sig: "journald:sha256v1:pk",
		Origin: types.Origin{HostID: "h", Source: "journald"}, Actor: testActor(),
		Payload: map[string]any{
			"message": "-----BEGIN RSA PRIVATE KEY-----\nMIIEow...",
		},
	})
	if CodeOf(err) != types.CodeScrub008 {
		t.Fatalf("code = %s, want %s", CodeOf(err), types.CodeScrub008)
	}
	after := l.Status()
	if after.Records != before.Records || after.LastSeq != before.LastSeq {
		t.Errorf("a refused append changed the ledger: records %d→%d seq %d→%d",
			before.Records, after.Records, before.LastSeq, after.LastSeq)
	}
}
