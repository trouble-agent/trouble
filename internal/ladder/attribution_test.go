package ladder

// attribution_test.go — the record-side rung attribution (SPEC-05 §3.2b,
// SPEC-01 §3.10a; TRBL-123).
//
// The live product question these answer: a transition record has to NAME the
// play it drafted and the rung it entered, and a rung entry has to be
// distinguishable from every other incident record, because the ledger's
// play_runs/agent_runs counters fold exactly that key.

import (
	"context"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// TestRungAdvanceRecordNamesThePlayAndTheRung drives an incident through the
// production driver and asserts the T03 record carries the play's identity and
// the rung attribution. Without these keys the ledger cannot say what the ladder
// did, only that it did something.
func TestRungAdvanceRecordNamesThePlayAndTheRung(t *testing.T) {
	play := types.Play{Name: "service-reload", Version: 3, Source: "agent-draft", CheckMode: true}
	h := newHarness(t, harnessOpts{
		rules:   map[string]types.Rule{"dbus_unit_failed": {Name: "dbus_unit_failed", EntryRung: types.RungPlay}},
		playFor: map[string]types.Play{"dbus_unit_failed": play},
	})
	res := h.admit(t, driveObservation())
	if _, err := h.l.Drive(context.Background()); err != nil {
		t.Fatalf("Drive: %v", err)
	}

	rec := incidentRecord(t, h, res.Inc, "T03")
	if got, _ := rec.Payload["play"].(string); got != play.Name {
		t.Errorf("T03 payload.play = %q, want %q", got, play.Name)
	}
	if got, _ := rec.Payload["play_version"].(int); got != play.Version {
		t.Errorf("T03 payload.play_version = %v, want %d", rec.Payload["play_version"], play.Version)
	}
	if got, _ := rec.Payload["play_source"].(string); got != play.Source {
		t.Errorf("T03 payload.play_source = %q, want %q", got, play.Source)
	}
	if got, _ := rec.Payload["rung"].(string); got != string(types.RungPlay) {
		t.Errorf("T03 payload.rung = %q, want %q", got, string(types.RungPlay))
	}
	if got, _ := rec.Payload["rung_entry"].(string); got != string(types.RungPlay) {
		t.Errorf("T03 payload.rung_entry = %q, want %q", got, string(types.RungPlay))
	}
}

// TestRungEntryIsOnlyWrittenByARungEntry pins the negative half: admission (T01),
// a fold and a refusal all leave no rung_entry, so the ledger counter cannot
// count them as runs.
func TestRungEntryIsOnlyWrittenByARungEntry(t *testing.T) {
	h := newHarness(t, harnessOpts{
		rules:   map[string]types.Rule{"dbus_unit_failed": {Name: "dbus_unit_failed", EntryRung: types.RungPlay}},
		playFor: map[string]types.Play{"dbus_unit_failed": drivePlay()},
	})
	res := h.admit(t, driveObservation())
	if _, err := h.l.Drive(context.Background()); err != nil {
		t.Fatalf("Drive: %v", err)
	}

	t01 := incidentRecord(t, h, res.Inc, "T01")
	if v, ok := t01.Payload["rung_entry"]; ok {
		t.Errorf("the T01 admission record carries rung_entry=%v, want no rung attribution", v)
	}
	// A refused stage entry (kill-switch) is a refusal record, never a rung entry.
	if _, err := h.l.SetKillSwitch(context.Background(), true, types.Actor{Kind: types.ActorHuman, ID: "op:test"}); err != nil {
		t.Fatalf("SetKillSwitch: %v", err)
	}
	obs := driveObservation()
	obs.Sig = types.NewSigFromFields(types.SrcJournald, "unit", "payload.service", "second")
	second := h.admit(t, obs)
	if _, err := h.l.Advance(context.Background(), second.Inc, Transition{From: types.StRecorded, To: types.StPlayDrafted, Trigger: "T03"}); err == nil {
		t.Fatal("the kill-switch did not refuse the stage entry")
	}
	last := h.ledger.last()
	if last == nil || last.Inc != second.Inc {
		t.Fatalf("no refusal record was appended for %s", second.Inc)
	}
	if v, ok := last.Payload["rung_entry"]; ok {
		t.Errorf("a refusal record carries rung_entry=%v, want no rung attribution", v)
	}
	if refused, _ := last.Payload["refused"].(bool); !refused {
		t.Error("the record appended under the kill-switch is not marked refused")
	}
}

// incidentRecord returns the incident record for (inc, transition).
func incidentRecord(t *testing.T, h *harness, inc, transition string) ledgerRec {
	t.Helper()
	for _, rec := range h.ledger.byKind(types.KIncident) {
		if rec.Inc == inc && rec.Payload["transition"] == transition {
			return rec
		}
	}
	t.Fatalf("no incident record with transition %s for %s", transition, inc)
	return ledgerRec{}
}
