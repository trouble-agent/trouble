package app

// incident_subject_test.go — TRBL-126, driven through the composition root.
//
// `bootDaemon` builds the daemon the fleet runs: real ledger, real index, real
// dashboard server. `h.d.emit` is the production sensors→ladder bridge, so this
// test exercises the same admission path a dbus unit failure takes — only the
// D-Bus connection is replaced by the event payload the dbus sensor produces.

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/ledger"
	"github.com/trouble-agent/trouble/internal/lifecycle"
	"github.com/trouble-agent/trouble/internal/types"
)

// TestUnitFailureIncidentNamesTheUnitEverywhere is the acceptance test: a
// unit-scoped failure carries the unit identifier and the originating message in
// the incident payload, and the operator surfaces that already list incidents
// render it without opening the ledger.
func TestUnitFailureIncidentNamesTheUnitEverywhere(t *testing.T) {
	h := bootDaemon(t)

	const unit = "payment-worker.service"
	const wantSubject = unit + ": unit_substate=failed via properties_changed"
	sig := types.NewSig(types.SigSource("dbus"), "sha256", 1, []byte("trbl-126-unit-failure-0001")).String()

	// The payload the dbus sensor's emitDBusOutcome builds for a failed unit:
	// the unit as the event subject, the state transition under detail.
	rec, err := h.d.emit(context.Background(), types.RecordDraft{
		Kind:   types.KEvent,
		Sig:    sig,
		Origin: types.Origin{HostID: h.d.Cfg.Origin.HostID, Source: "dbus"},
		Actor:  lifecycle.Actor(types.ActorDaemon, "troubled"),
		Payload: map[string]any{
			"kind":       "sensor",
			"sensor":     "dbus",
			"scope":      unit,
			"subject":    unit,
			"fire":       true,
			"rule":       "dbus_unit_failed",
			"entry_rung": "play",
			"severity":   "high",
			"detail": map[string]any{
				"unit_key":      unit,
				"unit_substate": "failed",
				"arrival_path":  "properties_changed",
				"msg":           "",
			},
		},
	})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}

	// The incident that this trigger opened (the host's own sensors may have
	// opened others), scoped by signature.
	var inc types.Incident
	for _, o := range h.d.Ledger.DashReader().OpenIncidents(200) {
		if o.Sig == sig {
			inc = o
		}
	}
	if inc.ID == "" {
		t.Fatalf("the unit-failure event opened no incident for %s", sig)
	}
	if inc.Subject != wantSubject {
		t.Fatalf("incident subject = %q, want %q (the unit identifier and its message)", inc.Subject, wantSubject)
	}
	if !strings.Contains(inc.Subject, unit) {
		t.Fatalf("subject %q does not name the unit", inc.Subject)
	}

	// The ledger file itself: `ledger.ReadOpenIncidents` projects from the day
	// files only, so a subject it reports can only have come from the record a
	// restart would rebuild from (the dashboard ring does not carry it).
	onDisk, err := ledger.ReadOpenIncidents(filepath.Join(h.d.Cfg.StateRoot, "ledger"), 200)
	if err != nil {
		t.Fatalf("ledger.ReadOpenIncidents: %v", err)
	}
	var persisted *types.Incident
	for i := range onDisk {
		if onDisk[i].ID == inc.ID {
			persisted = &onDisk[i]
		}
	}
	if persisted == nil {
		t.Fatalf("incident %s is not readable from the ledger files", inc.ID)
	}
	if persisted.Subject != wantSubject {
		t.Fatalf("on-disk incident subject = %q, want %q (the record must carry the unit and its message)", persisted.Subject, wantSubject)
	}

	// The dashboard incident row: the unit is on screen, not only in the ledger.
	code, body := h.get(t, fmt.Sprintf("/partials/incidents?since=%d", rec.Seq-1))
	if code != 200 {
		t.Fatalf("GET /partials/incidents = %d", code)
	}
	if !strings.Contains(body, unit) {
		t.Fatalf("the incident row does not name the unit %q: %s", unit, body)
	}
	if !strings.Contains(body, "class=\"subject\"") {
		t.Fatalf("the incident row renders no subject element: %s", body)
	}

	// The per-incident page names it too.
	code, body = h.get(t, "/incidents/"+inc.ID)
	if code != 200 {
		t.Fatalf("GET /incidents/%s = %d", inc.ID, code)
	}
	if !strings.Contains(body, unit) {
		t.Fatalf("the incident page does not name the unit %q: %s", unit, body)
	}
}
