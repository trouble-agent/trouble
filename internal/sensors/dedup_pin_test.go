package sensors

import (
	"context"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// TRBL-009 AC1 PIN — the refuted half, pinned as SATISFIED so a future change
// cannot silently reintroduce it.
//
// The dogfood read "74 incident records from 21 identities" as 3-4 incidents per
// identity. Re-measured first-hand at HEAD 31d6b76 (tick
// trouble-2026-09-19-08-24-56): 21 incident records, 21 DISTINCT incident ids,
// every one of them a T01 new-incident transition, 0 signatures with more than
// one incident id, 0 fold/reopen records. The gap that remains is not in the
// incident layer at all: firing into the ladder is cooldown-gated per rule+sig
// while PSI SAMPLING is not gated, so RECORDS outnumber INCIDENTS.
//
// This test pins that split at the sensor path it is decided on:
//
//   - every observation is RECORDED (the cooldown never suppresses the record —
//     §3.8 "events are never dropped", handleEvent's own contract), and
//   - at most ONE observation inside the cooldown FIRES, carrying one rule+sig
//     identity, which is what opens exactly one incident.
//
// The incident half of that sentence belongs to internal/ladder and is pinned
// there: internal/app/daemon_test.go's
// TestDaemonRendersAFiredIncidentWithinTwoSeconds asserts, over the real
// ledger+ladder, that a recurrence of the same signature leaves exactly ONE open
// incident (AC-22).
func TestAC1CooldownGatesFiringNotRecording(t *testing.T) {
	// The posture the foreman measured: the fold is OFF, so the raw
	// one-record-per-sample behaviour is what is asserted here.
	h := newHarness(t, types.ConfigValue{Key: "sensors.sample_fold_window", Value: "0"})
	h.writeRules("10.toml", `
[[rule]]
name = "trbl009_io"
source = "psi"
for = "0s"
cooldown = "5m"
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

	// The sampler's own shape, every 2s, well inside the 5m cooldown.
	ev := sampledEvent("io", 41.7)
	sig := ev.Sig.String()
	const observations = 12
	for i := 0; i < observations; i++ {
		if i > 0 {
			h.advance(2 * time.Second)
		}
		h.s.handleEvent(ctx, ev)
	}

	// 1. RECORDING is not gated: the cooldown is a firing gate, never a
	// recording gate.
	recs := recordsForSig(h, sig)
	if len(recs) != observations {
		t.Fatalf("the cooldown gated RECORDING: %d records for %d observations, want one per observation", len(recs), observations)
	}
	for _, r := range recs {
		if r.Sig != sig {
			t.Fatalf("record sig %s, want %s: one identity, or the ladder would open a second incident", r.Sig, sig)
		}
		if r.Kind != types.KEvent {
			t.Fatalf("record kind %s, want event", r.Kind)
		}
	}

	// 2. FIRING is gated: one fired observation inside the cooldown, carrying one
	// (rule, sig) identity — exactly one incident's worth of admission.
	fired := firedFor(h, "trbl009_io")
	if len(fired) != 1 {
		t.Fatalf("%d fired events inside one 5m cooldown, want exactly 1 (one incident, not %d)", len(fired), observations)
	}
	identities := map[string]bool{}
	for _, r := range fired {
		rule, _ := r.Payload["rule"].(string)
		identities[rule+"|"+r.Sig] = true
	}
	if len(identities) != 1 {
		t.Fatalf("fired identities = %v, want one rule+sig pair", identities)
	}
	if got := h.s.suppressed.Load(); got != observations-1 {
		t.Fatalf("Suppressed = %d, want %d: the cooldown must count what it refused, not drop it", got, observations-1)
	}

	// 3. The cooldown is the gate — not a permanent block, and not the breaker:
	// crossing it fires once more, under the same identity (which is what lets
	// the ladder fold the recurrence into the same incident, AC-22).
	h.advance(5 * time.Minute)
	h.s.handleEvent(ctx, ev)
	fired = firedFor(h, "trbl009_io")
	if len(fired) != 2 {
		t.Fatalf("%d fired events after the cooldown expired, want 2 (one per window)", len(fired))
	}
	if fired[1].Sig != fired[0].Sig {
		t.Fatalf("the recurrence fired under a different signature (%s vs %s): the ladder would open a second incident", fired[1].Sig, fired[0].Sig)
	}
	for _, b := range h.s.Breakers() {
		if b.State == types.BreakerOpen {
			t.Fatalf("the breaker opened on a 12-observation cooldown-gated stream: %+v", b)
		}
	}
}
