package ladder

// drive_test.go — the rung driver (SPEC-05 §4.4a, TRBL-123).
//
// The defect: nothing moved an incident out of `recorded` without a dashboard
// click, so a play on disk was never drafted, let alone run. These tests drive
// the incident through the PRODUCTION path only — Admit (the ladder's sole
// creator) then Drive (the composition root's own loop body) — never through
// dashbridge and never by calling Advance directly.

import (
	"context"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// drivePlay is the play a rule drafts in these tests.
func drivePlay() types.Play {
	return types.Play{Name: "service-reload", Version: 1, Source: "module-default", CheckMode: true}
}

// driveObservation is an admission a rule fired for.
func driveObservation() Observation {
	return Observation{
		Sig:    types.NewSigFromFields(types.SrcJournald, "unit", "payload.service", "msg"),
		Rule:   "dbus_unit_failed",
		Source: "journald",
	}
}

// TestDriveAdvancesRecordedIncidentToThePlayRung is the TRBL-123 regression: an
// unattended incident with a play on disk leaves `recorded` through the driver,
// and the transition record names the rung. It fails if the rung does not move.
func TestDriveAdvancesRecordedIncidentToThePlayRung(t *testing.T) {
	h := newHarness(t, harnessOpts{
		rules:   map[string]types.Rule{"dbus_unit_failed": {Name: "dbus_unit_failed", EntryRung: types.RungPlay}},
		playFor: map[string]types.Play{"dbus_unit_failed": drivePlay()},
	})
	res := h.admit(t, driveObservation())

	rep, err := h.l.Drive(context.Background())
	if err != nil {
		t.Fatalf("Drive: %v", err)
	}
	if len(rep.Advanced) != 1 || rep.Advanced[0] != res.Inc {
		t.Fatalf("Drive advanced %v, want [%s] (considered %d, failed %v)", rep.Advanced, res.Inc, rep.Considered, rep.Failed)
	}
	got, err := h.l.Incident(context.Background(), res.Inc)
	if err != nil {
		t.Fatalf("Incident: %v", err)
	}
	if got.State != types.StPlayDrafted {
		t.Fatalf("state = %q, want %q: the driver did not move the rung", string(got.State), string(types.StPlayDrafted))
	}
	if got.Rung != types.RungPlay {
		t.Fatalf("rung = %q, want %q", string(got.Rung), string(types.RungPlay))
	}

	// The ledger evidence: the table's own T03 row, recorded, with the rung the
	// incident entered.
	var t03 *ledgerRec
	for _, rec := range h.ledger.byKind(types.KIncident) {
		if rec.Inc != res.Inc {
			continue
		}
		if rec.Payload["transition"] == "T03" {
			cp := rec
			t03 = &cp
		}
	}
	if t03 == nil {
		t.Fatal("no T03 (rung_advance_play) record was appended: the rung advance left no ledger evidence")
	}
	if gotRung, _ := t03.Payload["rung"].(string); gotRung != string(types.RungPlay) {
		t.Errorf("T03 payload.rung = %q, want %q", gotRung, string(types.RungPlay))
	}

	// A second pass is a no-op: the incident left `recorded`, so nothing is
	// driven twice and no refusal is fabricated.
	before := len(h.ledger.byKind(types.KIncident))
	rep2, err := h.l.Drive(context.Background())
	if err != nil {
		t.Fatalf("second Drive: %v", err)
	}
	if len(rep2.Advanced) != 0 {
		t.Errorf("the second pass advanced %v, want none", rep2.Advanced)
	}
	if after := len(h.ledger.byKind(types.KIncident)); after != before {
		t.Errorf("the second pass appended %d incident record(s), want 0", after-before)
	}
}

// TestDriveNeverMovesAnAuditOnlyIncident pins the ceiling: a rule that enters at
// `record` is the audit-only rung, and the driver leaves it in `recorded` rather
// than forcing a play it never asked for.
func TestDriveNeverMovesAnAuditOnlyIncident(t *testing.T) {
	h := newHarness(t, harnessOpts{
		rules:   map[string]types.Rule{"audit_only": {Name: "audit_only", EntryRung: types.RungRecord}},
		playFor: map[string]types.Play{"audit_only": drivePlay()},
	})
	obs := driveObservation()
	obs.Rule = "audit_only"
	res := h.admit(t, obs)

	rep, err := h.l.Drive(context.Background())
	if err != nil {
		t.Fatalf("Drive: %v", err)
	}
	if len(rep.Advanced) != 0 {
		t.Fatalf("Drive advanced %v, want none for a `record` ceiling", rep.Advanced)
	}
	got, err := h.l.Incident(context.Background(), res.Inc)
	if err != nil {
		t.Fatalf("Incident: %v", err)
	}
	if got.State != types.StRecorded {
		t.Fatalf("state = %q, want %q", string(got.State), string(types.StRecorded))
	}
}

// TestDriveWithNoPlayAndNoPortsDegradesInsteadOfPanicking is the nil-safety row:
// no PlayFor, no Research and no Agent, and the incident still leaves `recorded`
// through T03 (which resolves the play opportunistically); the drafted rung
// simply carries no play and the next pass drives nothing.
func TestDriveWithNoPlayAndNoPortsDegradesInsteadOfPanicking(t *testing.T) {
	h := newHarness(t, harnessOpts{
		rules: map[string]types.Rule{"dbus_unit_failed": {Name: "dbus_unit_failed", EntryRung: types.RungPlay}},
	})
	res := h.admit(t, driveObservation())

	rep, err := h.l.Drive(context.Background())
	if err != nil {
		t.Fatalf("Drive: %v", err)
	}
	if len(rep.Advanced) != 1 {
		t.Fatalf("Drive advanced %v, want the incident to enter the play rung", rep.Advanced)
	}
	got, err := h.l.Incident(context.Background(), res.Inc)
	if err != nil {
		t.Fatalf("Incident: %v", err)
	}
	if got.State != types.StPlayDrafted {
		t.Fatalf("state = %q, want %q", string(got.State), string(types.StPlayDrafted))
	}
	if rep2, err2 := h.l.Drive(context.Background()); err2 != nil || len(rep2.Advanced) != 0 {
		t.Fatalf("second Drive = %v, %v; want no advance and no error", rep2.Advanced, err2)
	}
}

// TestDriveFollowsLadderOrderAndTheCeiling pins the order: with a research
// ceiling the PLAY rung is still attempted first (ladder order), and the
// research rung is only reached when the play rung cannot apply.
func TestDriveFollowsLadderOrderAndTheCeiling(t *testing.T) {
	h := newHarness(t, harnessOpts{
		research: true,
		rules:    map[string]types.Rule{"crash": {Name: "crash", EntryRung: types.RungResearch}},
	})
	obs := driveObservation()
	obs.Rule = "crash"
	res := h.admit(t, obs)

	if _, err := h.l.Drive(context.Background()); err != nil {
		t.Fatalf("Drive: %v", err)
	}
	got, err := h.l.Incident(context.Background(), res.Inc)
	if err != nil {
		t.Fatalf("Incident: %v", err)
	}
	if got.State != types.StPlayDrafted || got.Rung != types.RungPlay {
		t.Fatalf("state/rung = %q/%q, want the play rung first", string(got.State), string(got.Rung))
	}
}

// TestDriveRefusesWithTheKillSwitchAndRecordsNothing pins the freeze: with the
// kill-switch set the driver does not attempt a stage entry at all, so the
// incident stays in `recorded` and no refusal record is manufactured per pass.
func TestDriveRefusesWithTheKillSwitchAndRecordsNothing(t *testing.T) {
	h := newHarness(t, harnessOpts{
		rules:   map[string]types.Rule{"dbus_unit_failed": {Name: "dbus_unit_failed", EntryRung: types.RungPlay}},
		playFor: map[string]types.Play{"dbus_unit_failed": drivePlay()},
	})
	res := h.admit(t, driveObservation())
	if _, err := h.l.SetKillSwitch(context.Background(), true, types.Actor{Kind: types.ActorHuman, ID: "op:test"}); err != nil {
		t.Fatalf("SetKillSwitch: %v", err)
	}
	before := len(h.ledger.byKind(types.KIncident))

	rep, err := h.l.Drive(context.Background())
	if err != nil {
		t.Fatalf("Drive: %v", err)
	}
	if len(rep.Advanced) != 0 {
		t.Fatalf("Drive advanced %v under the kill-switch", rep.Advanced)
	}
	if after := len(h.ledger.byKind(types.KIncident)); after != before {
		t.Errorf("the driver appended %d incident record(s) under the kill-switch, want 0", after-before)
	}
	got, err := h.l.Incident(context.Background(), res.Inc)
	if err != nil {
		t.Fatalf("Incident: %v", err)
	}
	if got.State != types.StRecorded {
		t.Fatalf("state = %q under the kill-switch, want %q", string(got.State), string(types.StRecorded))
	}
}

// TestDriveDoesNotFallThroughAnOpenBreaker pins the one refusal that must hold
// the whole incident: with the sig's scope breaker open, T03 refuses with
// TROUBLE-LADDER-014 and the driver does not reach a lower rung instead. The
// refusal is recorded exactly once, and the incident stays put.
func TestDriveDoesNotFallThroughAnOpenBreaker(t *testing.T) {
	h := newHarness(t, harnessOpts{
		research: true,
		rules:    map[string]types.Rule{"dbus_unit_failed": {Name: "dbus_unit_failed", EntryRung: types.RungAgent}},
	})
	res := h.admit(t, driveObservation())
	sig, err := h.l.Incident(context.Background(), res.Inc)
	if err != nil {
		t.Fatalf("Incident: %v", err)
	}
	h.l.mu.Lock()
	scope := scopeSig + sig.Sig
	h.l.breakers[scope] = &breakerState{Breaker: types.Breaker{Scope: scope, State: types.BreakerOpen}}
	h.l.mu.Unlock()

	rep, err := h.l.Drive(context.Background())
	if err != nil {
		t.Fatalf("Drive: %v", err)
	}
	if len(rep.Advanced) != 0 {
		t.Fatalf("Drive advanced %v through an open breaker", rep.Advanced)
	}
	got, err := h.l.Incident(context.Background(), res.Inc)
	if err != nil {
		t.Fatalf("Incident: %v", err)
	}
	if got.State != types.StRecorded {
		t.Fatalf("state = %q behind an open breaker, want %q", string(got.State), string(types.StRecorded))
	}
	if n := len(h.ledger.byKind(types.KIncident)); n != 2 {
		t.Errorf("the pass appended %d incident record(s) total (admission + refusals), want 2: one refusal, no fall-through", n)
	}
}

// TestDriveOneReportsTheIncidentItCannotFind keeps the driver's error path
// honest: an unknown id is the ladder's own TROUBLE-LADDER-012, never a silent
// success.
func TestDriveOneReportsTheIncidentItCannotFind(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	if _, err := h.l.DriveOne(context.Background(), "inc_missing"); err == nil {
		t.Fatal("DriveOne(unknown id) returned no error")
	} else if CodeOf(err) != types.CodeLadder012 {
		t.Errorf("DriveOne(unknown id) code = %q, want %q", CodeOf(err), types.CodeLadder012)
	}
}
