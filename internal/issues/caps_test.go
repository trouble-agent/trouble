package issues

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// TestSigCreateCapIsOne pins the §3.8 per-sig create cap: 1 000 ensures for one
// sig in one hour produce exactly one create, at most the comment cap in any
// rolling hour, and a recorded suppression for everything else.
func TestSigCreateCapIsOne(t *testing.T) {
	api := newFakeGitHub()
	srv := ghServer(t, api)
	t.Setenv(testTokenEnv, "ghp_test_token_value")
	d, led, clk, _ := deskWith(t, githubTestConfig(srv.URL))
	d.cfg.OpDeadline = "2s"

	inc := testIncident()
	ev := testEvidence()
	if _, err := d.EnsureBySig(context.Background(), inc, ev); err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	suppressed := 0
	for i := 0; i < 1000; i++ {
		clk.advance(time.Second) // 1 000 ensures inside one 24h window
		_, err := d.EnsureBySig(context.Background(), inc, ev)
		if err == nil {
			continue
		}
		if CodeOf(err) != types.CodeIssues010 && CodeOf(err) != types.CodeIssues004 {
			t.Fatalf("unexpected error at iteration %d: %v", i, err)
		}
		suppressed++
	}
	if got := len(api.issues); got != 1 {
		t.Fatalf("%d issues were created for one sig, want exactly 1", got)
	}

	// Counts from the ledger, which is the audit truth.
	creates, comments := 0, 0
	var commentTimes []time.Time
	for _, r := range led.records() {
		if r.Kind != types.KIssue {
			continue
		}
		op := strPayload(r.Payload, "op")
		switch op {
		case "ensure", "replay":
			if boolPayload(r.Payload, "created") {
				creates++
			}
		case "comment":
			switch strPayload(r.Payload, "result") {
			case "suppressed":
				suppressed++
			default:
				if boolPayload(r.Payload, "commented") {
					comments++
					ts, err := types.ParseUTC(r.TS)
					if err == nil {
						commentTimes = append(commentTimes, ts)
					}
				}
			}
		case "cap":
			suppressed++
		}
	}
	if creates != 1 {
		t.Fatalf("ledger records %d creates, want 1", creates)
	}
	// No rolling hour may hold more than the per-sig comment cap.
	for i := range commentTimes {
		n := 0
		for j := range commentTimes {
			if !commentTimes[j].Before(commentTimes[i]) && commentTimes[j].Sub(commentTimes[i]) < time.Hour {
				n++
			}
		}
		if n > d.cfg.Caps.PerSigComments {
			t.Fatalf("a rolling hour holds %d comments, over the cap %d", n, d.cfg.Caps.PerSigComments)
		}
	}
	if suppressed < 990 {
		t.Fatalf("only %d operations were recorded suppressed, want >= 990", suppressed)
	}
	if comments == 0 || comments > d.cfg.Caps.PerSigComments {
		t.Fatalf("%d comments for one sig inside one hour, want 1..%d", comments, d.cfg.Caps.PerSigComments)
	}
	if st := d.CapState(); st.SigCreates[inc.Sig] != 1 {
		t.Fatalf("cap state reports %d creates for the sig", st.SigCreates[inc.Sig])
	}
}

// TestCapsSurviveRestart pins §3.8/edge 12: the counters are rebuilt from the
// ledger, so a restart cannot lift a cap or file a second issue.
func TestCapsSurviveRestart(t *testing.T) {
	api := newFakeGitHub()
	srv := ghServer(t, api)
	t.Setenv(testTokenEnv, "ghp_test_token_value")
	led := &fakeLedger{}
	clk := newFakeClock()
	mk := func() *Desk {
		cfg := enabledDefaults()
		cfg.Drivers = []types.IssueDriverConfig{githubTestConfig(srv.URL)}
		cfg.PrimaryDriver = "github"
		cfg.OpDeadline = "2s"
		d, err := New(cfg, Deps{
			Ledger: led, Scan: led, Scrub: &fakeScrubber{}, Clock: clk,
			HostID: "7f3a91c2d4e5b607", Actor: testActor(),
			StateRoot: t.TempDir(),
			ProjectOf: func(types.Incident) string { return "1" },
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return d
	}
	first := mk()
	if _, err := first.EnsureBySig(context.Background(), testIncident(), testEvidence()); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if got := len(api.issues); got != 1 {
		t.Fatalf("issues = %d", got)
	}
	// Restart: a fresh desk over the same ledger.
	second := mk()
	if _, err := second.EnsureBySig(context.Background(), testIncident(), testEvidence()); err == nil {
		// An in-window recurrence folds (that is legal) — what must not happen is a
		// second issue.
		t.Logf("post-restart ensure folded (expected)")
	}
	if got := len(api.issues); got != 1 {
		t.Fatalf("a restart created a duplicate issue: %d", got)
	}
	st := second.CapState()
	if st.SigCreates[testSig] != 1 {
		t.Fatalf("the create counter was not rebuilt from the ledger: %+v", st.SigCreates)
	}
}

// TestPerProjectAndGlobalCaps pins the project/global tiers: each distinct sig is
// capped as well, and the project/global ceilings stop a spray.
func TestPerProjectAndGlobalCaps(t *testing.T) {
	cases := []struct {
		name      string
		project   int
		global    int
		wantMake  int
		wantCap   int
		wantToken string
	}{
		{"project ceiling", 3, 100, 3, 5, "per_project_creates_h"},
		{"global ceiling", 100, 4, 4, 4, "global_creates_h"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeGitHub()
			srv := ghServer(t, api)
			t.Setenv(testTokenEnv, "ghp_test_token_value")
			d, led, _, _ := deskWith(t, githubTestConfig(srv.URL))
			d.cfg.OpDeadline = "2s"
			d.cfg.Caps.PerProjectCreatesH = tc.project
			d.cfg.Caps.GlobalCreatesH = tc.global
			d.caps = newCapCounters(d.cfg.Caps, d.now)

			capped, made := 0, 0
			for i := 0; i < 8; i++ {
				inc := testIncident()
				inc.Sig = sigFor(i)
				inc.ID = types.NewID(types.PInc)
				ev := testEvidence()
				if _, err := d.EnsureBySig(context.Background(), inc, ev); err != nil {
					if CodeOf(err) == types.CodeIssues004 {
						capped++
						continue
					}
					t.Fatalf("unexpected error: %v", err)
				}
				made++
			}
			if made != tc.wantMake || capped != tc.wantCap {
				t.Fatalf("made %d / capped %d, want %d / %d", made, capped, tc.wantMake, tc.wantCap)
			}
			capRecs := led.byOp("cap")
			if len(capRecs) != tc.wantCap {
				t.Fatalf("%d cap records, want %d", len(capRecs), tc.wantCap)
			}
			for _, r := range capRecs {
				if strPayload(r.Payload, "error_code") != string(types.CodeIssues004) {
					t.Fatalf("cap record error_code = %q", strPayload(r.Payload, "error_code"))
				}
				if strPayload(r.Payload, "cap") != tc.wantToken {
					t.Fatalf("cap token = %q, want %q", strPayload(r.Payload, "cap"), tc.wantToken)
				}
				if !neverSpooled(r) {
					t.Fatalf("a capped operation must never be spooled: %v", r.Payload)
				}
			}
			if n := d.spoolCount("github"); n != 0 {
				t.Fatalf("capped operations entered the spool: %d", n)
			}
			if len(api.issues) != tc.wantMake {
				t.Fatalf("backend holds %d issues, want %d", len(api.issues), tc.wantMake)
			}
		})
	}
}

// neverSpooled expresses "a cap is a policy decision, never a retry": the record
// carries retryable=false.
func neverSpooled(r types.Record) bool {
	v, ok := r.Payload["retryable"].(bool)
	return ok && !v
}

func sigFor(i int) string {
	hex := []string{"0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "a", "b", "c", "d", "e", "f"}
	s := ""
	for j := 0; j < 16; j++ {
		s += hex[(i*7+j*3)%16]
	}
	return "sentinel:sha256v1:" + s
}

// TestCommentMinIntervalDefers pins the minimum spacing: two folds inside the
// interval produce one comment and one recorded suppression.
func TestCommentMinIntervalDefers(t *testing.T) {
	api := newFakeGitHub()
	srv := ghServer(t, api)
	t.Setenv(testTokenEnv, "ghp_test_token_value")
	d, led, clk, _ := deskWith(t, githubTestConfig(srv.URL))
	inc := testIncident()
	ev := testEvidence()
	if _, err := d.EnsureBySig(context.Background(), inc, ev); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	clk.advance(31 * time.Minute) // outside the dedup window: the first fold is a block
	if _, err := d.EnsureBySig(context.Background(), inc, ev); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	before := 0
	for _, r := range led.byOp("comment") {
		if boolPayload(r.Payload, "commented") {
			before++
		}
	}
	clk.advance(time.Minute) // inside comment_min_interval (10m)
	if _, err := d.EnsureBySig(context.Background(), inc, ev); err != nil {
		t.Fatalf("third ensure: %v", err)
	}
	after := 0
	suppressed := 0
	for _, r := range led.byOp("comment") {
		if boolPayload(r.Payload, "commented") {
			after++
		} else if strPayload(r.Payload, "result") == "suppressed" {
			suppressed++
		}
	}
	if after != before {
		t.Fatalf("a fold inside comment_min_interval appended a comment (%d → %d)", before, after)
	}
	if suppressed == 0 {
		t.Fatalf("the deferred fold was not recorded as suppressed")
	}
}

// TestAckSuppressesFolds pins edge case 14: an ack suppresses the fold comment
// while the incident and the anchor still update.
func TestAckSuppressesFolds(t *testing.T) {
	api := newFakeGitHub()
	srv := ghServer(t, api)
	t.Setenv(testTokenEnv, "ghp_test_token_value")
	d, led, clk, _ := deskWith(t, githubTestConfig(srv.URL))
	inc := testIncident()
	ev := testEvidence()
	ref, err := d.EnsureBySig(context.Background(), inc, ev)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if _, err := d.Ack(context.Background(), ref, time.Hour); err != nil {
		t.Fatalf("ack: %v", err)
	}
	clk.advance(31 * time.Minute)
	commentsBefore := api.count("POST repo/issues/{n}/comments")
	if _, err := d.EnsureBySig(context.Background(), inc, ev); err != nil {
		t.Fatalf("acked ensure: %v", err)
	}
	if got := api.count("POST repo/issues/{n}/comments"); got != commentsBefore {
		t.Fatalf("an acked fold still commented (%d → %d)", commentsBefore, got)
	}
	if api.count("GET repo/issues/{n}") == 0 && api.count("POST repo/issues") == 0 {
		t.Fatalf("the anchor did not update at all")
	}
	var ackOnly bool
	for _, r := range led.byOp("comment") {
		if strPayload(r.Payload, "reason") == "ack_active" {
			ackOnly = true
		}
	}
	if !ackOnly {
		var dump []string
		for _, r := range led.records() {
			dump = append(dump, fmt.Sprintf("kind=%s op=%s result=%s reason=%s payload=%v", r.Kind,
				strPayload(r.Payload, "op"), strPayload(r.Payload, "result"), strPayload(r.Payload, "reason"), r.Payload))
		}
		t.Fatalf("the suppressed fold did not record ack_active: %v\n%s", led.ops(), strings.Join(dump, "\n"))
	}
	if len(api.issues) != 1 {
		t.Fatalf("the ack path created a duplicate")
	}
}
