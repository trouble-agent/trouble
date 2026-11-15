package app

// dashbridge_state_render_test.go — TRBL-127: the dashboard's incident page and
// the surface `trouble ledger status` prints its incident tail from must report
// the same state string for the same incident id. Both are projections of the
// same incident records through the same fold (internal/ledger applyIncident), so
// this test drives the production path — real ladder, real ledger, real
// dashboard server — and pins the two surfaces against each other and against
// the ladder's own state.
//
// The negative control is the `detected` assertion at the end: before the fix the
// T01 admission record (which carries `from:detected to:recorded` and no `state`
// key) left the projection at the `detected` default, so the page rendered
// `detected` while the ladder said `recorded`.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/ledger"
	"github.com/trouble-agent/trouble/internal/types"
)

func TestDashboardStateMatchesLedgerStatus(t *testing.T) {
	c := bootCoreLoop(t)

	// Premise: the admission record is T01 detected -> recorded and carries no
	// `state` key. If the ladder ever starts stamping `state`, the `to` fold this
	// test pins stops being the live path and this failure says so directly
	// instead of letting the assertions below pass vacuously.
	var t01 map[string]any
	if err := c.store.L.Query().ScanFrom(0, func(rec types.Record) bool {
		if rec.Kind == types.KIncident && rec.Inc == c.inc && rec.Payload["transition"] == "T01" {
			t01 = rec.Payload
			return false
		}
		return true
	}); err != nil {
		t.Fatalf("ScanFrom: %v", err)
	}
	if t01 == nil {
		t.Fatalf("no T01 admission record for incident %s", c.inc)
	}
	if stamped, ok := t01["state"]; ok {
		t.Fatalf("the T01 record now carries state=%v; this test pins the `to` fold, which is no longer the live path", stamped)
	}
	if to, _ := t01["to"].(string); to != string(types.StRecorded) {
		t.Fatalf("T01 `to` = %q, want recorded", to)
	}

	// The ladder is authoritative (SPEC-05 INV-2).
	want := ladderState(t, c.l, c.inc)
	if want != types.StRecorded {
		t.Fatalf("ladder state for %s = %q, want recorded", c.inc, string(want))
	}

	// Surface 1: the rendered dashboard page (/incidents/{id}).
	code, page := c.get(t, "/incidents/"+c.inc)
	if code != http.StatusOK {
		t.Fatalf("GET /incidents/%s = %d, want 200 (%.400s)", c.inc, code, page)
	}
	rendered := "<dt>state</dt><dd>" + string(want) + "</dd>"
	if !strings.Contains(page, rendered) {
		t.Fatalf("dashboard did not render the ledger state: missing %q in %.800s", rendered, page)
	}

	// Negative control: the old mapping rendered exactly this.
	if strings.Contains(page, "<dt>state</dt><dd>"+string(types.StDetected)+"</dd>") {
		t.Fatalf("dashboard rendered the detected default for %s while the ledger says %q", c.inc, string(want))
	}

	// Surface 2: the lock-free read that `trouble ledger status` prints.
	open, err := ledger.ReadOpenIncidents(c.store.L.Root(), 50)
	if err != nil {
		t.Fatalf("ReadOpenIncidents: %v", err)
	}
	var statusState types.LadderState
	for _, row := range open {
		if row.ID == c.inc {
			statusState = row.State
			break
		}
	}
	if statusState == "" {
		t.Fatalf("ledger status projection did not list open incident %s", c.inc)
	}
	if statusState != want {
		t.Fatalf("the two surfaces disagree for %s: dashboard %q, ledger status %q", c.inc, string(want), string(statusState))
	}
}

// ch:trace row=TRBL-127 spec=specs/SPEC-10-dashboard.md#7-testing wave=none:serial test=internal/app/dashbridge_state_render_test.go::TestDashboardStateMatchesLedgerStatus doc=none evidence=internal/app/dashbridge_state_render_test.go witness=none:ephemeral-worker-verification verdict=none:foreman-judge commit=none:foreman-integration memory=none:foreman-write
// STATUS: complete | TIER: T1
// EVIDENCE: internal/app/dashbridge_state_render_test.go
