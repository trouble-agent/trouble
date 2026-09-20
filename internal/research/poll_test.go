package research

// poll_test.go — SPEC-07 §7 `poll_test.go`: the deterministic jitter, the
// backoff, the request/wall budget, the poll-timeout degrade, and the AC-20
// "never blocks" assertion.

import (
	"context"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

func TestPollJitterIsDeterministicAndBounded(t *testing.T) {
	p := pollParams{Interval: 15 * time.Second, JitterPct: 0.20, Timeout: time.Minute, MaxRequests: 48}
	now := time.Now()
	first := newPollState("sub_87ee13", p, now)
	for i := 0; i < 20; i++ {
		again := newPollState("sub_87ee13", p, now)
		if again.Interval != first.Interval {
			t.Fatalf("jitter is not deterministic for one submission id: %v vs %v", again.Interval, first.Interval)
		}
	}
	lo, hi := time.Duration(float64(p.Interval)*0.79), time.Duration(float64(p.Interval)*1.21)
	if first.Interval < lo || first.Interval > hi {
		t.Fatalf("interval %v outside ±20%% of %v", first.Interval, p.Interval)
	}
	// Different submission ids de-synchronise (not all identical).
	distinct := map[time.Duration]bool{}
	for i := 0; i < 40; i++ {
		distinct[newPollState("sub_"+string(rune('a'+i)), p, now).Interval] = true
	}
	if len(distinct) < 3 {
		t.Fatalf("jitter produced %d distinct intervals: pollers are not de-synchronised", len(distinct))
	}
}

func TestPollBackoffAndCap(t *testing.T) {
	p := pollParams{Interval: 15 * time.Second, JitterPct: 0, Timeout: time.Hour, MaxRequests: 100}
	start := time.Now()
	ps := newPollState("sub_x", p, start)
	if got := ps.nextInterval(start); got != 15*time.Second {
		t.Fatalf("initial interval = %v", got)
	}
	if got := ps.nextInterval(start.Add(3 * time.Minute)); got != 22*time.Second+500*time.Millisecond {
		t.Fatalf("backoff after 2m = %v, want 1.5×", got)
	}
	ps2 := newPollState("sub_y", pollParams{Interval: 50 * time.Second, Timeout: time.Hour, MaxRequests: 100}, start)
	if got := ps2.nextInterval(start.Add(3 * time.Minute)); got != 60*time.Second {
		t.Fatalf("backoff cap = %v, want 60s", got)
	}
}

func TestPollBudgetEndsTheLoop(t *testing.T) {
	lab := newLabStub(t)
	// The lab never solves: every poll answers "solving".
	lab.queueStates = []string{"solving"}
	s, deps := newTestService(t, lab, map[string]any{
		"poll_interval": "1ms", "poll_timeout": "60ms", "poll_max_requests": 48,
	})
	start := time.Now()
	out, err := s.Run(context.Background(), testIncident(), testSig(), bundleFor(map[string]any{"await": true}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.State != types.ResDegraded || out.DegradedReason != types.ResReasonPollTimeout {
		t.Fatalf("outcome = %+v, want degraded/poll_timeout", out)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the poll budget took %s, want ≤ poll_timeout + slack", elapsed)
	}
	p := deps.lastPayload(types.KResearch)
	if p["error_code"] != string(types.CodeResearch006) {
		t.Fatalf("error_code = %v, want 006", p["error_code"])
	}
	gaps := deps.payloads(types.KGap)
	if len(gaps) != 1 || gaps[0]["cause"] != "research_poll_timeout" {
		t.Fatalf("gaps = %v, want one research_poll_timeout", gaps)
	}
	if p["poll_count"] == 0 {
		t.Fatalf("payload.poll_count must record the poll pressure")
	}
}

func TestPollMaxRequestsEndsTheLoop(t *testing.T) {
	lab := newLabStub(t)
	lab.queueStates = []string{"solving"}
	s, deps := newTestService(t, lab, map[string]any{
		"poll_interval": "1ms", "poll_timeout": "10s", "poll_max_requests": 2,
	})
	out, err := s.Run(context.Background(), testIncident(), testSig(), bundleFor(map[string]any{"await": true}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.State != types.ResDegraded || out.DegradedReason != types.ResReasonPollTimeout {
		t.Fatalf("outcome = %+v", out)
	}
	if claimed := lab.count("/api/v1/queue/"); claimed > 2 {
		t.Fatalf("polls = %d, want ≤2 (poll_max_requests)", claimed)
	}
	if p := deps.lastPayload(types.KResearch); p["error_code"] != string(types.CodeResearch006) {
		t.Fatalf("error_code = %v", p["error_code"])
	}
}

// TestPollNeverBlocksTheLadder is the AC-20 non-negotiable: with a lab that
// never answers, the rung still returns inside the poll budget, and Run without
// await returns immediately while the poller is still going.
func TestPollNeverBlocksTheLadder(t *testing.T) {
	lab := newLabStub(t)
	lab.queueStates = []string{"solving"}
	s, _ := newTestService(t, lab, map[string]any{
		"poll_interval": "1ms", "poll_timeout": "30ms", "poll_max_requests": 10,
	})
	start := time.Now()
	out, err := s.Run(context.Background(), testIncident(), testSig(), bundleFor(nil))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.State != types.ResRequested {
		t.Fatalf("Run must return at once with requested, got %+v", out)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("Run held the caller for %s", elapsed)
	}
	// Poll waits out the budget and reports the timeout.
	final, err := s.Poll(context.Background(), out.ID)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if final.State != types.ResDegraded {
		t.Fatalf("Poll = %+v, want degraded", final)
	}
	if total := time.Since(start); total > 3*time.Second {
		t.Fatalf("the whole rung took %s, want ≤ poll_timeout + 2s", total)
	}
}

func TestPollSolvedReturnsBrief(t *testing.T) {
	lab := newLabStub(t)
	s, deps := newTestService(t, lab, nil)
	out, err := s.Run(context.Background(), testIncident(), testSig(), bundleFor(map[string]any{"await": true}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.State != types.ResReturned {
		t.Fatalf("outcome = %+v, want returned", out)
	}
	if out.Brief == nil || out.Brief["solution"] != "set MemoryMax=512M" {
		t.Fatalf("brief = %v", out.Brief)
	}
	p := deps.lastPayload(types.KResearch)
	if p["state"] != types.ResReturned || p["brief_digest"] == nil || p["brief_digest"] == "" {
		t.Fatalf("payload = %v", p)
	}
	if p["brief"] == nil {
		t.Fatalf("the brief must be stored in the record")
	}
}

// TestQueueNotFoundThenReprobe covers §6.6: a lab restart loses the queue id.
func TestQueueNotFoundThenReprobe(t *testing.T) {
	lab := newLabStub(t)
	lab.queue404 = true
	s, deps := newTestService(t, lab, map[string]any{"poll_interval": "1ms", "poll_timeout": "200ms"})
	out, err := s.Run(context.Background(), testIncident(), testSig(), bundleFor(map[string]any{"await": true}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.State != types.ResDegraded {
		t.Fatalf("outcome = %+v, want degraded", out)
	}
	// One D1 re-probe after the lost id (§6.6), so discover ran at least twice.
	if n := lab.count("/api/v1/problems/discover"); n < 2 {
		t.Fatalf("discover calls = %d, want the post-404 D1 re-probe", n)
	}
	gaps := deps.payloads(types.KGap)
	if len(gaps) == 0 || gaps[0]["cause"] != "research_solver_unavailable" {
		t.Fatalf("gaps = %v", gaps)
	}
}
