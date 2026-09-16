package scrub

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// SPEC-02 §7 limits_test.go: the 256 KiB truncation boundary (cut at the last
// newline, exact marker, partial line dropped), refuse-mode targets returning
// SCRUB-003 with no partial output, non-UTF-8 returning SCRUB-007, and timeout
// injection returning SCRUB-005 with no value. Availability loses to leakage,
// deliberately (§3.8).

// testRuleTable appends one test-only rule to the compiled-in table. The kind is
// unreachable from configuration (it is not one of the five rule kinds §3.2
// accepts).
type testRuleOpts struct {
	sleep    time.Duration
	overflow bool
	fail     error
	target   types.ScrubTarget
}

func testRuleTable(o testRuleOpts) []builtinRule {
	table := make([]builtinRule, len(builtinTable), len(builtinTable)+1)
	copy(table, builtinTable)
	targets := []types.ScrubTarget{types.TgEventMsg}
	if o.target != "" {
		targets = []types.ScrubTarget{o.target}
	}
	table = append(table, builtinRule{
		name: "zz_test_only", kind: kindTestScrub, pattern: "builtin",
		replace: "[REDACTED:zz_test_only]", targets: targets,
		testSleep: o.sleep, testOverflow: o.overflow, testFail: o.fail,
	})
	return table
}

func newTestEngineWithRule(t *testing.T, cfg string, o testRuleOpts) *Engine {
	t.Helper()
	e, err := newEngine([]byte(cfg), testProjects(), testRuleTable(o), nil)
	if err != nil {
		t.Fatalf("newEngine: %v", err)
	}
	return e
}

func TestTruncateEventMsgAt256KiB(t *testing.T) {
	// a generous rule budget keeps this test about truncation semantics rather
	// than about wall-clock timing: rule_timeout is a wall-clock budget (§3.8), and
	// a 256 KiB pass over the pii rules can exceed 250 ms under -race
	e := newTestEngine(t, "[scrub]\nrule_timeout = \"10s\"\n")
	line := "2026-09-16T09:14:03.221Z host systemd[1]: Started Session 41207 of user opuser\n"
	var b strings.Builder
	for b.Len() < types256KiB+4096 {
		b.WriteString(line)
	}
	in := b.String()
	out, res, err := e.ScrubString(context.Background(), types.TgEventMsg, "1", in)
	if err != nil {
		t.Fatalf("truncating scrub failed: %v", err)
	}
	if !res.Truncated {
		t.Fatalf("Truncated = false for a %d-byte payload (budget 262144)", len(in))
	}
	if len(out) > types256KiB+64 {
		t.Errorf("output is %d bytes, budget is 262144 plus the marker", len(out))
	}
	i := strings.LastIndex(out, "[SCRUB-TRUNCATED:")
	if i < 0 {
		t.Fatalf("no truncation marker in the output: %q", out[max(0, len(out)-80):])
	}
	marker := out[i:]
	if !strings.HasSuffix(marker, "]") {
		t.Errorf("marker %q is not terminated", marker)
	}
	// the cut is at a line boundary: the retained head ends with a newline and
	// the dropped span is a whole number of lines
	head := out[:i]
	if !strings.HasSuffix(head, "\n") {
		t.Errorf("the retained head does not end at a line boundary: %q", head[max(0, len(head)-40):])
	}
	if !strings.HasPrefix(in, head) {
		t.Errorf("the retained head is not a prefix of the input")
	}
	wantDropped := len(in) - len(head)
	if marker != "[SCRUB-TRUNCATED:"+itoa(wantDropped)+"]" {
		t.Errorf("marker = %q, want [SCRUB-TRUNCATED:%d]", marker, wantDropped)
	}
}

// TestTruncationExactArithmetic pins the cut: last newline inside the budget,
// partial final line dropped entirely, marker exact.
func TestTruncationExactArithmetic(t *testing.T) {
	e := newTestEngine(t, "[scrub]\n[scrub.targets.event_msg]\nmax_bytes = 20\non_over = \"truncate\"\n")
	in := "aaaa\nbbbb\ncccccccccccccccccccccccc\ndddd\n"
	out, res := scrubString(t, e, types.TgEventMsg, "1", in)
	if !res.Truncated {
		t.Fatal("Truncated = false")
	}
	// budget 20: the last '\n' at or before offset 20 is at index 9, so the head
	// is "aaaa\nbbbb\n" (10 bytes) and the remaining 30 bytes are dropped
	want := "aaaa\nbbbb\n[SCRUB-TRUNCATED:30]"
	if out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
	// no newline inside the budget at all: the whole line is a partial line and
	// is dropped, leaving the marker alone
	out2, _ := scrubString(t, e, types.TgEventMsg, "1", strings.Repeat("x", 100))
	if out2 != "[SCRUB-TRUNCATED:100]" {
		t.Errorf("output = %q, want only the marker", out2)
	}
}

// TestRefuseTargetReturnsNoPartialOutput: a refuse-mode target returns SCRUB-003
// and nothing else — no output, no partial output, and the bytes are counted.
func TestRefuseTargetReturnsNoPartialOutput(t *testing.T) {
	e := newTestEngine(t, "")
	in := strings.Repeat("x", 70000) // issue budget is 65536, on_over = refuse
	before := e.Stats().RefusedBytes
	out, res, err := e.ScrubString(context.Background(), types.TgIssue, "1", in)
	if err == nil {
		t.Fatal("an over-budget issue payload was accepted")
	}
	if CodeOf(err) != types.CodeScrub003 {
		t.Errorf("code = %s, want %s", CodeOf(err), types.CodeScrub003)
	}
	if out != "" || res.Value != nil {
		t.Errorf("a refused payload returned a value: %q", out)
	}
	if res.Redactions != 0 {
		t.Errorf("a refused payload reported %d redactions", res.Redactions)
	}
	if got := e.Stats().RefusedBytes - before; got != uint64(len(in)) {
		t.Errorf("refused_bytes = %d, want %d", got, len(in))
	}
	if !types.CodeScrub003.HasClass(types.ErrClassTransient) {
		t.Error("TROUBLE-SCRUB-003 is not classified transient")
	}
}

func TestNonUTF8FailsClosed(t *testing.T) {
	e := newTestEngine(t, "")
	bad := []byte("prefix \xff\xfe binary suffix")
	before := e.Stats().InvalidUTF8
	out, res, err := e.ScrubBytes(context.Background(), types.TgEventMsg, "1", bad)
	if err == nil {
		t.Fatal("invalid UTF-8 was accepted on a text target")
	}
	if CodeOf(err) != types.CodeScrub007 {
		t.Errorf("code = %s, want %s", CodeOf(err), types.CodeScrub007)
	}
	if out != nil || res.Value != nil {
		t.Error("a fail-closed call returned a value")
	}
	if e.Stats().InvalidUTF8 != before+1 {
		t.Error("invalid_utf8 did not increment")
	}
	// the same on the spool target: the engine is always handed the serialised
	// JSON form (§3.8 spool row), so refusing is the fail-closed reading
	if _, _, err := e.ScrubBytes(context.Background(), types.TgSpool, "1", bad); CodeOf(err) != types.CodeScrub007 {
		t.Errorf("spool code = %s, want %s", CodeOf(err), types.CodeScrub007)
	}
}

func TestRuleTimeoutFailsClosed(t *testing.T) {
	e := newTestEngineWithRule(t, "[scrub]\nrule_timeout = \"5ms\"\n", testRuleOpts{sleep: 40 * time.Millisecond})
	before := e.Stats()
	out, res, err := e.ScrubString(context.Background(), types.TgEventMsg, "1", "nothing sensitive here at all")
	if err == nil {
		t.Fatal("a rule that exceeded rule_timeout did not fail the call")
	}
	if CodeOf(err) != types.CodeScrub005 {
		t.Errorf("code = %s, want %s", CodeOf(err), types.CodeScrub005)
	}
	if out != "" || res.Value != nil {
		t.Error("a timed-out call returned a value")
	}
	if res.Redactions != 0 {
		t.Errorf("a timed-out call reported %d redactions", res.Redactions)
	}
	st := e.Stats()
	if st.Timeouts != before.Timeouts+1 {
		t.Errorf("timeouts = %d, want %d", st.Timeouts, before.Timeouts+1)
	}
	if st.FailClosed != before.FailClosed+1 {
		t.Errorf("fail_closed = %d, want %d", st.FailClosed, before.FailClosed+1)
	}
	if !types.CodeScrub005.HasClass(types.ErrClassTransient) {
		t.Error("TROUBLE-SCRUB-005 is not classified transient")
	}
}

func TestRuleErrorFailsClosed(t *testing.T) {
	e := newTestEngineWithRule(t, "", testRuleOpts{fail: errors.New("bad state")})
	_, _, err := e.ScrubString(context.Background(), types.TgEventMsg, "1", "payload")
	if CodeOf(err) != types.CodeScrub004 {
		t.Errorf("code = %s, want %s", CodeOf(err), types.CodeScrub004)
	}
	if !types.CodeScrub004.HasClass(types.ErrClassPermanent) {
		t.Error("TROUBLE-SCRUB-004 is not classified permanent")
	}
}

func TestReplacementOverflowFailsClosed(t *testing.T) {
	e := newTestEngineWithRule(t, "", testRuleOpts{overflow: true})
	_, _, err := e.ScrubString(context.Background(), types.TgEventMsg, "1", "payload")
	if CodeOf(err) != types.CodeScrub004 {
		t.Errorf("code = %s, want %s", CodeOf(err), types.CodeScrub004)
	}
}

func TestEmptyAndNilInput(t *testing.T) {
	e := newTestEngine(t, "")
	out, res, err := e.ScrubBytes(context.Background(), types.TgEventMsg, "1", nil)
	if err != nil {
		t.Fatalf("ScrubBytes(nil): %v", err)
	}
	if out == nil || len(out) != 0 {
		t.Errorf("ScrubBytes(nil) = %v, want an empty non-nil slice", out)
	}
	if res.Redactions != 0 || res.BytesIn != 0 {
		t.Errorf("bytes_in = %d redactions = %d, want 0/0", res.BytesIn, res.Redactions)
	}
	for _, in := range []string{"", "   ", "\n\n", "[SCRUB-TRUNCATED:4096]", `[REDACTED:bearer_token]`} {
		got, res := scrubString(t, e, types.TgEventMsg, "1", in)
		if got != in {
			t.Errorf("input %q changed to %q", in, got)
		}
		if res.Redactions != 0 {
			t.Errorf("input %q reported %d redactions", in, res.Redactions)
		}
	}
}

func TestCancelledContextFailsClosed(t *testing.T) {
	e := newTestEngine(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, _, err := e.ScrubString(ctx, types.TgEventMsg, "1", "PASSWORD=hunter2")
	if err == nil {
		t.Fatal("a cancelled context was ignored")
	}
	if out != "" {
		t.Errorf("a cancelled call returned a value: %q", out)
	}
}

func TestUnknownTargetRefused(t *testing.T) {
	e := newTestEngine(t, "")
	_, _, err := e.ScrubString(context.Background(), types.ScrubTarget("logfile"), "1", "x")
	if CodeOf(err) != types.CodeScrub002 {
		t.Errorf("code = %s, want %s", CodeOf(err), types.CodeScrub002)
	}
}

// TestTruncationDropsHalfValues: a secret that straddles the budget may not be
// persisted in halves — the cut moves back to the last newline, so the kept head
// is fully scrubbed and the tail is gone.
func TestTruncationDropsHalfValues(t *testing.T) {
	e := newTestEngine(t, "[scrub]\n[scrub.targets.event_msg]\nmax_bytes = 40\non_over = \"truncate\"\n")
	secret := strings.Repeat("A1b2C3d4", 8)
	in := "line one\nline two with PASSWORD=" + secret + " trailing\nline three\n"
	out, res := scrubString(t, e, types.TgEventMsg, "1", in)
	if !res.Truncated {
		t.Fatal("Truncated = false")
	}
	if strings.Contains(out, secret) || strings.Contains(out, secret[:8]) {
		t.Errorf("a partial secret was persisted: %q", out)
	}
	head := out[:strings.Index(out, "[SCRUB-TRUNCATED:")]
	if !strings.HasSuffix(head, "\n") {
		t.Errorf("the retained head does not end at a line boundary: %q", head)
	}
	if !strings.HasPrefix(in, head) {
		t.Errorf("the retained head is not a prefix of the input: %q", head)
	}
	if !strings.Contains(out, "[SCRUB-TRUNCATED:") {
		t.Errorf("no truncation marker: %q", out)
	}
}

// TestStatsAccounting: the process counters add up (calls, bytes, redactions,
// by_rule) and RulesVersion is reported.
func TestStatsAccounting(t *testing.T) {
	e := newTestEngine(t, "")
	before := e.Stats()
	_, r1 := scrubString(t, e, types.TgEventMsg, "1", "PASSWORD=hunter2")
	_, r2 := scrubString(t, e, types.TgEventMsg, "1", `{"token": "abc123xyz"}`)
	st := e.Stats()
	if st.Calls != before.Calls+2 {
		t.Errorf("calls = %d, want %d", st.Calls, before.Calls+2)
	}
	if st.Redactions != before.Redactions+2 {
		t.Errorf("redactions = %d, want %d", st.Redactions, before.Redactions+2)
	}
	if st.BytesIn != before.BytesIn+uint64(r1.BytesIn+r2.BytesIn) {
		t.Errorf("bytes_in = %d, want %d", st.BytesIn, before.BytesIn+uint64(r1.BytesIn+r2.BytesIn))
	}
	if st.ByRule["env_assign"] == 0 || st.ByRule["kv_secret_assign"] == 0 {
		t.Errorf("by_rule = %v, want both rule names counted", st.ByRule)
	}
	if st.RulesVersion != DefaultRulesVersion {
		t.Errorf("rules_version = %d, want %d", st.RulesVersion, DefaultRulesVersion)
	}
}

const types256KiB = 262144

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
