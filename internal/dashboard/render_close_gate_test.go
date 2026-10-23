package dashboard

// render_close_gate_test.go — TRBL-118 (SPEC-10 §2.2): the close control is
// STATE-gated. It renders for exactly the states whose edge into `resolved`
// exists in SPEC-05's table (verifying T38, suppressed T45, acknowledged T50) and
// is not offered for a state that can only answer TROUBLE-LADDER-012
// (no_close_edge). This is a different axis from the SCOPE gate the ack form
// keeps: an under-scoped session still SEES every control it may not click,
// disabled and with a reason — a hidden control reads as "this feature does not
// exist". A control whose only possible outcome is a refusal reads as a broken
// product, which is what the dogfood tick reported.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

func TestCloseControlIsStateGated(t *testing.T) {
	env := newEnv(t, envOptions{cfg: unlimitedRates})
	lookup := env.lookup

	const (
		detectedID     = "inc_01J9F0000000000000000000D1"
		recordedID     = "inc_01J9F0000000000000000000D2"
		acknowledgedID = "inc_01J9F0000000000000000000D3"
		suppressedID   = "inc_01J9F0000000000000000000D4"
	)
	verifyingID := validIncidentID()
	for _, inc := range []types.Incident{
		{ID: detectedID, Sig: "psi:sha256v1:00000000000000d1", State: types.StDetected, EntryRung: types.RungPlay, Rung: types.RungPlay, Severity: types.SevHigh, OpenedTS: "2026-09-16T09:00:00.000Z", UpdatedTS: "2026-09-16T09:00:00.000Z"},
		{ID: recordedID, Sig: "psi:sha256v1:00000000000000d2", State: types.StRecorded, EntryRung: types.RungPlay, Rung: types.RungPlay, Severity: types.SevHigh, OpenedTS: "2026-09-16T09:00:00.000Z", UpdatedTS: "2026-09-16T09:00:00.000Z"},
		{ID: acknowledgedID, Sig: "psi:sha256v1:00000000000000d3", State: types.StAcknowledged, EntryRung: types.RungPlay, Rung: types.RungPlay, Severity: types.SevHigh, OpenedTS: "2026-09-16T09:00:00.000Z", UpdatedTS: "2026-09-16T09:05:00.000Z"},
		{ID: suppressedID, Sig: "psi:sha256v1:00000000000000d4", State: types.StSuppressed, EntryRung: types.RungPlay, Rung: types.RungPlay, Severity: types.SevHigh, OpenedTS: "2026-09-16T09:00:00.000Z", UpdatedTS: "2026-09-16T09:05:00.000Z"},
	} {
		lookup.putIncident(inc)
	}

	isCloseForm := func(id, page string) bool {
		return strings.Contains(page, `action="/api/incidents/`+id+`/close"`)
	}

	// States with no edge into `resolved`: no close control, and the page says
	// why instead of leaving the operator to guess.
	for _, id := range []string{detectedID, recordedID} {
		resp, page := env.get("/incidents/"+id, env.writePlain)
		wantStatus(t, resp, page, http.StatusOK)
		if isCloseForm(id, page) {
			t.Errorf("%s: the close control is rendered for a state with no edge into resolved", id)
		}
		if !strings.Contains(page, "close is not offered in state") {
			t.Errorf("%s: the page does not explain why close is unavailable: %.600s", id, page)
		}
		// The ack control stays: a pre-work ack is the action that makes the
		// close possible (T49), and a post-work ack is still an audit record.
		if !strings.Contains(page, `action="/api/incidents/`+id+`/ack"`) {
			t.Errorf("%s: the ack control was dropped with the close control", id)
		}
	}

	// States with an edge into `resolved`: the control renders, and still
	// carries the disabled + reason pair for an under-scoped session (§2.2).
	for _, id := range []string{acknowledgedID, suppressedID, verifyingID} {
		resp, page := env.get("/incidents/"+id, env.readPlain)
		wantStatus(t, resp, page, http.StatusOK)
		if !isCloseForm(id, page) {
			t.Errorf("%s: the close control is missing for a closable state: %.600s", id, page)
		}
		if !strings.Contains(page, "requires a write-scope token") {
			t.Errorf("%s: the read-scope page lost the inline write-scope reason", id)
		}
	}
}
