package sensors

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// sensors_ac_test.go covers the four acceptance criteria this area owns
// (SPEC-INDEX §7 step 2): AC-1, AC-2, AC-4, AC-19.
//
// One seam is worth stating plainly: an INCIDENT's lifecycle belongs to
// internal/ladder (SPEC-05). What sensors can prove, and what these tests
// assert, is the detection-side contract the ladder rides on — exactly one
// fired event per qualifying stream, per-signature identity that survives a
// resolve/recurrence, evaluations capped with honest suppression counts, and an
// event that is queryable and visible in /health within its budgets.

func firedFor(h *harness, rule string) []types.Record {
	return h.recordsWhere(func(r types.Record) bool {
		if v, _ := r.Payload["fire"].(bool); !v {
			return false
		}
		n, _ := r.Payload["rule"].(string)
		return n == rule
	})
}

// AC-1: a match at for=2s fires exactly one event inside 2s ±250ms of the
// qualifying stream.
func TestAC1ForWindowFiresExactlyOnce(t *testing.T) {
	h := newHarness(t)
	h.writeRules("10.toml", `
[[rule]]
name = "ac1_io"
source = "psi"
for = "2s"
severity = "high"
entry_rung = "play"
[[rule.match]]
field = "scope"
op = "=="
value = "io"
value_type = "string"
[[rule.match]]
field = "some_avg10"
op = ">="
value = "35"
value_type = "number"
`)
	h.mustReload()
	ctx := context.Background()
	start := h.now()
	// A qualifying stream: the same signature keeps holding.
	h.s.handleEvent(ctx, psiEvent("io", 41.7, false))
	if got := len(firedFor(h, "ac1_io")); got != 0 {
		t.Fatalf("fired %d times at t=0, want 0", got)
	}
	h.advance(1990 * time.Millisecond)
	h.s.handleEvent(ctx, psiEvent("io", 41.8, false))
	if got := len(firedFor(h, "ac1_io")); got != 0 {
		t.Fatalf("fired %d times at 1.99s of a 2s window, want 0", got)
	}
	h.advance(30 * time.Millisecond)
	h.s.handleEvent(ctx, psiEvent("io", 41.8, false))
	fired := firedFor(h, "ac1_io")
	if len(fired) != 1 {
		t.Fatalf("fired %d times after 2.02s of a 2s window, want exactly 1", len(fired))
	}
	elapsed := h.now().Sub(start)
	if elapsed < 2*time.Second-250*time.Millisecond || elapsed > 2*time.Second+250*time.Millisecond {
		t.Fatalf("fired at %s, outside the 2s ±250ms window", elapsed)
	}
	// The fired record carries the ladder's inputs.
	p := fired[0].Payload
	if p["rule"] != "ac1_io" || p["severity"] != "high" || p["entry_rung"] != "play" {
		t.Fatalf("fired payload = %v", p)
	}
	if fired[0].Sig != psiEvent("io", 41.7, false).Sig.String() {
		t.Fatalf("fired sig = %s, want the qualifying signature", fired[0].Sig)
	}
	// And the same qualifying stream does not fire a second event inside the
	// cooldown (the ladder must not be told twice).
	h.advance(100 * time.Millisecond)
	h.s.handleEvent(ctx, psiEvent("io", 41.9, false))
	if got := len(firedFor(h, "ac1_io")); got != 1 {
		t.Fatalf("a second fire inside the cooldown: %d", got)
	}
}

// AC-2: 10 crossings in 10s with for=30s ⇒ 0 fires; one continuous 30s ⇒ 1;
// resolve+recurrence ⇒ the same signature (the ladder reopens its incident).
func TestAC2StabilizationAndRecurrence(t *testing.T) {
	h := newHarness(t)
	h.writeRules("10.toml", `
[[rule]]
name = "ac2_io"
source = "psi"
for = "30s"
cooldown = "5m"
[[rule.match]]
field = "scope"
op = "=="
value = "io"
value_type = "string"
[[rule.match]]
field = "some_avg10"
op = ">="
value = "35"
value_type = "number"
`)
	h.mustReload()
	ctx := context.Background()
	// 10 crossings in 10s, each interrupted: 0 fires.
	for i := 0; i < 10; i++ {
		h.s.handleEvent(ctx, psiEvent("io", 41.7, false))
		h.advance(time.Second)
		h.s.handleEvent(ctx, psiEvent("io", 1.0, false)) // same or different sig: either way it breaks
		h.advance(time.Second)
	}
	if got := len(firedFor(h, "ac2_io")); got != 0 {
		t.Fatalf("10 crossings inside a 30s window fired %d times, want 0", got)
	}
	// One continuous 30s: exactly 1.
	h.s.stab.reset(ruleSigKey{rule: "ac2_io", sig: psiEvent("io", 41.7, false).Sig.String()})
	h.s.handleEvent(ctx, psiEvent("io", 41.7, false))
	h.advance(29 * time.Second)
	h.s.handleEvent(ctx, psiEvent("io", 41.7, false))
	if got := len(firedFor(h, "ac2_io")); got != 0 {
		t.Fatalf("fired %d times before the window elapsed", got)
	}
	h.advance(2 * time.Second)
	h.s.handleEvent(ctx, psiEvent("io", 41.7, false))
	fired := firedFor(h, "ac2_io")
	if len(fired) != 1 {
		t.Fatalf("a continuous 30s stream fired %d times, want exactly 1", len(fired))
	}
	firstSig := fired[0].Sig
	// A recurrence after the cooldown is the SAME signature: the ladder reopens
	// one incident rather than opening a second for the same story.
	h.advance(6 * time.Minute)
	h.s.handleEvent(ctx, psiEvent("io", 41.7, false))
	h.advance(31 * time.Second)
	h.s.handleEvent(ctx, psiEvent("io", 41.7, false))
	fired = firedFor(h, "ac2_io")
	if len(fired) != 2 {
		t.Fatalf("expected a second fire after the recurrence, got %d total", len(fired))
	}
	if fired[1].Sig != firstSig {
		t.Fatalf("recurrence produced a different signature (%s vs %s): the ladder would open a second incident", fired[1].Sig, firstSig)
	}
}

// AC-4: 10 000 rule-matching events in 60s ⇒ evaluations ≤120/min, the breaker
// opens within 120s, and Suppressed == events - cap with the ladder gate invoked
// at most cap times.
func TestAC4BreakersAndCaps(t *testing.T) {
	h := newHarness(t)
	h.writeRules("10.toml", `
[[rule]]
name = "ac4_burst"
source = "psi"
for = "0s"
cooldown = "0s"
severity = "low"
[[rule.match]]
field = "scope"
op = "=="
value = "io"
value_type = "string"
[[rule.match]]
field = "some_avg10"
op = ">="
value = "1"
value_type = "number"
`)
	h.mustReload()
	h.s.limitsForTest(120)
	ctx := context.Background()
	const events = 10000
	// 10 000 distinct signatures inside 60 simulated seconds: every event
	// matches the rule, so every one is an evaluation candidate.
	for i := 0; i < events; i++ {
		ev := psiEvent("io", 41.7, false)
		ev.Detail["some_avg10"] = 41.7
		ev.Detail["msg"] = fmt.Sprintf("burst %d", i)
		ev.Sig = sigFor(types.SrcPSI, "io", "some", fmt.Sprintf("burst-%d", i))
		h.s.handleEvent(ctx, ev)
		if i%167 == 0 { // ~60 simulated seconds across 10k events
			h.advance(time.Second)
		}
	}
	fired := len(firedFor(h, "ac4_burst"))
	if fired > 120 {
		t.Fatalf("the ladder gate was invoked %d times in one minute, cap is 120", fired)
	}
	if got := h.s.suppressed.Load(); got != uint64(events-fired) {
		t.Fatalf("Suppressed = %d, want events-fired = %d", got, events-fired)
	}
	if got := h.s.evaluations.Load(); got != uint64(fired) {
		t.Fatalf("evaluations = %d, want %d (one per gate invocation)", got, fired)
	}
	// The scope trips once the rate has been over the cap for the sustain window.
	h.advance(3 * time.Minute)
	for i := 0; i < 500; i++ {
		ev := psiEvent("io", 41.7, false)
		ev.Sig = sigFor(types.SrcPSI, "io", "some", fmt.Sprintf("post-%d", i))
		h.s.handleEvent(ctx, ev)
	}
	open := false
	for _, b := range h.s.Breakers() {
		if b.Scope == "rule:ac4_burst" && b.State == types.BreakerOpen {
			open = true
			if b.Trips < 1 {
				t.Fatalf("an open breaker must carry its trip count: %+v", b)
			}
		}
	}
	if !open {
		t.Fatalf("the rule breaker did not open under a sustained over-cap load; breakers = %+v", h.s.Breakers())
	}
	// Events are recorded even while the scope is open: suppression is at the
	// gate, not at the sensor.
	before := len(h.snapshot())
	h.s.handleEvent(ctx, psiEvent("io", 41.7, false))
	if after := len(h.snapshot()); after != before+1 {
		t.Fatalf("an event was dropped by the breaker: %d -> %d records", before, after)
	}
}

// limitsForTest pins the per-rule cap so the AC-4 arithmetic does not depend on
// a configuration value.
func (s *Sensors) limitsForTest(rulePerMin int) {
	s.br.mu.Lock()
	s.br.limit.rulePerMin = rulePerMin
	s.br.mu.Unlock()
}

// AC-19: an event is queryable through the index ≤1s (p95) after emission and
// Health() reflects its last_event_age_s within one heartbeat.
func TestAC19EmissionIsImmediatelyVisible(t *testing.T) {
	h := newHarness(t)
	enableAll(t, h)
	h.writeRules("10.toml", `
[[rule]]
name = "ac19_io"
source = "psi"
for = "0s"
[[rule.match]]
field = "scope"
op = "=="
value = "io"
value_type = "string"
`)
	h.mustReload()
	ctx := context.Background()
	const n = 200
	lat := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		ev := psiEvent("io", 41.7, false)
		ev.Sig = sigFor(types.SrcPSI, "io", "some", fmt.Sprintf("lat-%d", i))
		start := time.Now()
		h.s.handleEvent(ctx, ev)
		lat = append(lat, time.Since(start))
	}
	sortDurations(lat)
	p95 := lat[(len(lat)*95)/100]
	if p95 > time.Second {
		t.Fatalf("p95 emission latency = %s, budget is 1s", p95)
	}
	if got := len(h.snapshot()); got < n {
		t.Fatalf("%d events emitted, %d records", n, got)
	}
	// Health reflects the newest event within one heartbeat.
	h.advance(2 * time.Second)
	for _, sh := range h.s.Health() {
		if sh.Sensor == types.SenPSI {
			if sh.LastEventAgeS < 1.9 || sh.LastEventAgeS > 2.1 {
				t.Fatalf("last_event_age_s = %v, want ~2s", sh.LastEventAgeS)
			}
		}
	}
	t.Logf("measured: p95 emission latency %s over %d events", p95, n)
}

func sortDurations(d []time.Duration) {
	for i := 1; i < len(d); i++ {
		for j := i; j > 0 && d[j] < d[j-1]; j-- {
			d[j], d[j-1] = d[j-1], d[j]
		}
	}
}

// TestEventsAreNeverDroppedByABreaker states the §3.8 rule directly.
func TestEventsAreNeverDroppedByABreaker(t *testing.T) {
	h := newHarness(t)
	h.writeRules("10.toml", `
[[rule]]
name = "always"
source = "psi"
for = "0s"
cooldown = "0s"
[[rule.match]]
field = "scope"
op = "=="
value = "io"
value_type = "string"
`)
	h.mustReload()
	ctx := context.Background()
	h.s.br.tripLocked(h.s.br.get("global"), h.now(), "test trip")
	before := len(h.snapshot())
	for i := 0; i < 50; i++ {
		ev := psiEvent("io", 41.7, false)
		ev.Sig = sigFor(types.SrcPSI, "io", "some", fmt.Sprintf("g-%d", i))
		h.s.handleEvent(ctx, ev)
	}
	after := len(h.snapshot())
	if after-before != 50 {
		t.Fatalf("%d of 50 events were dropped by an open breaker", 50-(after-before))
	}
	if h.s.suppressed.Load() != 50 {
		t.Fatalf("Suppressed = %d, want 50", h.s.suppressed.Load())
	}
}
