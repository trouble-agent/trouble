package ladder

import (
	"context"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// TestTRBL134DraftedIncidentReachesRunStartedCheck is the TRBL-134-R2
// regression and falsifier for the old collection filter. The first pass uses
// T03 to draft the play; the second pass must collect that drafted state and
// use the existing T08 run_started_check checkpoint. With the old
// `state == recorded` filter, the second pass considered zero incidents and
// this assertion failed, leaving the incident at play:drafted forever.
func TestTRBL134DraftedIncidentReachesRunStartedCheck(t *testing.T) {
	h := newHarness(t, harnessOpts{
		rules:   map[string]types.Rule{"dbus_unit_failed": {Name: "dbus_unit_failed", EntryRung: types.RungPlay}},
		playFor: map[string]types.Play{"dbus_unit_failed": drivePlay()},
	})
	res := h.admit(t, driveObservation())
	if _, err := h.l.Drive(context.Background()); err != nil {
		t.Fatalf("draft pass: %v", err)
	}

	rep, err := h.l.Drive(context.Background())
	if err != nil {
		t.Fatalf("run-start pass: %v", err)
	}
	if rep.Considered != 1 || len(rep.Advanced) != 1 || rep.Advanced[0] != res.Inc {
		t.Fatalf("drafted pass = considered %d, advanced %v, want considered 1 and [%s] (failed %v)", rep.Considered, rep.Advanced, res.Inc, rep.Failed)
	}
	got, err := h.l.Incident(context.Background(), res.Inc)
	if err != nil {
		t.Fatalf("Incident: %v", err)
	}
	if got.State != types.StPlayCheck {
		t.Fatalf("state = %q, want %q after the existing run_started_check checkpoint", string(got.State), string(types.StPlayCheck))
	}
	if runs, _, _, _ := h.play.counts(); runs != 1 {
		t.Fatalf("play runs = %d, want 1 real run", runs)
	}
	if runs := len(h.agentRunRecords()); runs != 0 {
		t.Fatalf("agent_run records = %d, want 0: this play checkpoint must not fabricate an agent run", runs)
	}
	if rec := incidentRecord(t, h, res.Inc, "T08"); rec.Payload["transition"] != "T08" {
		t.Fatalf("checkpoint transition = %v, want T08", rec.Payload["transition"])
	}
}

// TestTRBL134DraftedIncidentWithoutRunnablePlayFailsClosed verifies the
// specified alternate exit. A drafted incident without a play or runner uses
// T09 and records no run of any kind rather than parking at play:drafted or
// pretending that an agent stage ran.
func TestTRBL134DraftedIncidentWithoutRunnablePlayFailsClosed(t *testing.T) {
	h := newHarness(t, harnessOpts{
		rules: map[string]types.Rule{"dbus_unit_failed": {Name: "dbus_unit_failed", EntryRung: types.RungPlay}},
	})
	res := h.admit(t, driveObservation())
	if _, err := h.l.Drive(context.Background()); err != nil {
		t.Fatalf("draft pass: %v", err)
	}
	if _, err := h.l.Drive(context.Background()); err != nil {
		t.Fatalf("no-runnable pass: %v", err)
	}
	got, err := h.l.Incident(context.Background(), res.Inc)
	if err != nil {
		t.Fatalf("Incident: %v", err)
	}
	if got.State != types.StPlayFailed {
		t.Fatalf("state = %q, want %q", string(got.State), string(types.StPlayFailed))
	}
	if len(h.agentRunRecords()) != 0 {
		t.Fatal("no-runnable path reported an agent run")
	}
	if rec := incidentRecord(t, h, res.Inc, "T09"); rec.Payload["reason"] != reasonNoRunnableTask {
		t.Fatalf("no-runnable reason = %v, want %q", rec.Payload["reason"], reasonNoRunnableTask)
	}
}

// ch:trace row=TRBL-134-R2 spec=specs/SPEC-05-ladder.md#4-wiring wave=none:serial test=internal/ladder/trbl134_test.go::TestTRBL134DraftedIncidentReachesRunStartedCheck doc=none evidence=internal/ladder/trbl134_test.go witness=none:ephemeral-worker-verification verdict=none:foreman-judge commit=none:foreman-integration memory=none:foreman-write
// STATUS: complete
// TIER: T1
// EVIDENCE: internal/ladder/trbl134_test.go
