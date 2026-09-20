package issues

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// TestAC8FileCommentClose is AC-8's acceptance test: the desk files an issue for
// an incident, folds a recurrence into it, and closes it after the quiet period —
// with the ladder-visible IssueRef correct at every step.
func TestAC8FileCommentClose(t *testing.T) {
	fx := resolvedFixture()
	d, led, clk, api := quietDesk(t, fx) // files the issue for fx.inc

	// Step 1: filed. The desk must have returned a ref with an external id inside
	// one op_deadline.
	ref, ok := d.Anchor(testSig)
	if !ok {
		t.Fatalf("no anchor after the filing")
	}
	if ref.ExternalID == "" || ref.URL == "" {
		t.Fatalf("the returned ref is not populated: %+v", ref)
	}
	if ref.State != types.IssueOpen {
		t.Fatalf("filed ref state = %q", ref.State)
	}
	if ref.ID == "" || !strings.HasPrefix(ref.ID, "iss_") {
		t.Fatalf("the ref has no local iss_ id: %q", ref.ID)
	}
	if got := len(api.issues); got != 1 {
		t.Fatalf("backend holds %d issues, want 1", got)
	}

	// Step 2: a recurrence folds into the same issue and never files a second.
	clk.advance(31 * time.Minute)
	fresh := testIncident()
	fresh.State = types.StVerifying
	fresh.ID = types.NewID(types.PInc)
	out, err := d.EnsureBySig(context.Background(), fresh, testEvidence())
	if err != nil {
		t.Fatalf("recurrence: %v", err)
	}
	if out.ExternalID != ref.ExternalID {
		t.Fatalf("recurrence moved to another issue: %s vs %s", out.ExternalID, ref.ExternalID)
	}
	if len(api.issues) != 1 {
		t.Fatalf("recurrence filed a duplicate: %d issues", len(api.issues))
	}

	// Step 3: close after exactly the quiet period — not one minute earlier. The
	// fixture resolved at 10:00 and the clock started at 09:00, so the remaining
	// quiet period is 24h29m from here.
	clk.advance(24*time.Hour + 28*time.Minute)
	if n, err := d.SweepQuietClose(context.Background(), clk.Now()); err != nil || n != 0 {
		t.Fatalf("closed %d issues one minute early (err=%v)", n, err)
	}
	clk.advance(2 * time.Minute)
	if n, err := d.SweepQuietClose(context.Background(), clk.Now()); err != nil || n != 1 {
		t.Fatalf("closed %d issues after the quiet period (err=%v)", n, err)
	}
	closed, ok := d.Anchor(testSig)
	if !ok || closed.State != types.IssueClosed {
		t.Fatalf("anchor after close = %+v ok=%v", closed, ok)
	}
	if api.lastIssue().State != types.IssueClosed {
		t.Fatalf("driver state after close = %q", api.lastIssue().State)
	}
	// The ladder-visible sequence is in the ledger: one create, at least one fold,
	// exactly one close.
	ops := map[string]int{}
	for _, r := range led.byOp("ensure") {
		if boolPayload(r.Payload, "created") {
			ops["created"]++
		}
	}
	for _, r := range led.byOp("comment") {
		if boolPayload(r.Payload, "commented") {
			ops["commented"]++
		}
	}
	ops["closed"] = len(led.byOp("close"))
	if ops["created"] != 1 || ops["commented"] < 1 || ops["closed"] != 1 {
		t.Fatalf("ledger sequence = %v, want 1 create, >=1 fold, 1 close", ops)
	}
}

// TestAC8FilingLatencyIsInsideTheDeadline asserts the §7 threshold: a filing
// completes inside one op_deadline against a local driver.
func TestAC8FilingLatencyIsInsideTheDeadline(t *testing.T) {
	api := newFakeGitHub()
	srv := ghServer(t, api)
	t.Setenv(testTokenEnv, "ghp_test_token_value")
	d, _, _, _ := deskWith(t, githubTestConfig(srv.URL))
	deadline := d.cfg.OpDeadline.Std()
	start := time.Now()
	if _, err := d.EnsureBySig(context.Background(), testIncident(), testEvidence()); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if elapsed := time.Since(start); elapsed > deadline {
		t.Fatalf("filing took %s, over the %s op_deadline", elapsed, deadline)
	}
}

// TestAC8FoldLatency is the §7 recurrence threshold: a fold against a local fake
// stays under the 2 s p95 the spec names (measured over 20 folds).
func TestAC8FoldLatency(t *testing.T) {
	api := newFakeGitHub()
	srv := ghServer(t, api)
	t.Setenv(testTokenEnv, "ghp_test_token_value")
	d, _, clk, _ := deskWith(t, githubTestConfig(srv.URL))
	inc := testIncident()
	ev := testEvidence()
	if _, err := d.EnsureBySig(context.Background(), inc, ev); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	worst := time.Duration(0)
	for i := 0; i < 20; i++ {
		clk.advance(11 * time.Minute)
		inc.ID = types.NewID(types.PInc)
		start := time.Now()
		if _, err := d.EnsureBySig(context.Background(), inc, ev); err != nil {
			t.Fatalf("fold %d: %v", i, err)
		}
		if d := time.Since(start); d > worst {
			worst = d
		}
	}
	if worst > 2*time.Second {
		t.Fatalf("worst fold took %s, over the 2s budget", worst)
	}
}

// clkAdvanceForReplay moves the fake clock past a spooled entry's next-try stamp,
// which the replay order honours.
func clkAdvanceForReplay(d *Desk) {
	if c, ok := d.clk.(*fakeClock); ok {
		c.advance(time.Second)
	}
}

// TestDriverDegradationKeepsTheIncidentMoving pins §3.6 step 3: a failing outlet
// degrades the issues rung; it neither fails the incident nor loses the work.
func TestDriverDegradationKeepsTheIncidentMoving(t *testing.T) {
	api := newFakeGitHub()
	srv := ghServer(t, api)
	t.Setenv(testTokenEnv, "ghp_test_token_value")
	d, led, _, _ := deskWith(t, githubTestConfig(srv.URL))
	api.mu.Lock()
	api.down = true
	api.mu.Unlock()
	d.probe(context.Background(), "github")
	d.probe(context.Background(), "github")
	if !d.driverFailed("github") {
		t.Fatalf("the driver was not marked failed")
	}
	inc := testIncident()
	_, err := d.EnsureBySig(context.Background(), inc, testEvidence())
	if err == nil {
		t.Fatalf("a dead driver must surface an error to the caller")
	}
	if !RetryableOf(err) {
		t.Fatalf("a driver outage is transient, so the ladder may degrade and continue")
	}
	if n := d.spoolCount("github"); n != 1 {
		t.Fatalf("the operation was not spooled: %d entries", n)
	}
	var sawHealth, sawGap bool
	for _, r := range led.records() {
		switch r.Kind {
		case types.KIssue:
			if strPayload(r.Payload, "op") == "healthcheck" && !boolPayload(r.Payload, "ok") {
				sawHealth = true
			}
		case types.KGap:
			if strPayload(r.Payload, "cause") == types.CauseDriverDown {
				sawGap = true
			}
		}
	}
	if !sawHealth || !sawGap {
		t.Fatalf("the outage produced healthcheck=%v gap=%v", sawHealth, sawGap)
	}
	// Recovery drains the spool before new work, and the drayed operation sets the
	// anchor without re-entering the ladder.
	api.mu.Lock()
	api.down = false
	api.mu.Unlock()
	clkAdvanceForReplay(d)
	d.probe(context.Background(), "github")
	if d.driverFailed("github") {
		t.Fatalf("the driver did not recover")
	}
	n, err := d.Replay(context.Background(), 10)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if n != 1 {
		t.Fatalf("replayed %d entries, want 1", n)
	}
	if _, ok := d.Anchor(testSig); !ok {
		t.Fatalf("the replayed operation did not populate the anchor")
	}
	var sawReplayed bool
	for _, r := range led.byOp("ensure") {
		if strPayload(r.Payload, "result") == "replayed" || strPayload(r.Payload, "result") == "replayed_created" {
			sawReplayed = true
		}
	}
	if !sawReplayed {
		t.Fatalf("the drained operation wrote no result=replayed record")
	}
}
