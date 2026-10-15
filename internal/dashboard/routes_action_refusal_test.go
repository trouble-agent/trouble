package dashboard

// routes_action_refusal_test.go — TRBL-REVIEW-CLOSE-ERR: a write seam's NAMED
// refusal must reach the wire as the owning subsystem's code + its
// machine-readable detail, never the generic 500 action_failed (SPEC-10 §5:
// `error.code` is the owning subsystem's code when a write seam exposes one;
// §7.6: `detail` is present when the refusal has a machine-readable variant).
//
// The composition-root type that carries the close refusal lives in
// internal/app (the two packages cannot import each other), so the wire
// contract is pinned here through an equivalent carrier of the same seam
// shape: ErrorCode + ErrorDetail + ErrorReason.

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// closeEdgeRefusal mirrors the app bridge's no_close_edge carrier: the
// ladder's TROUBLE-LADDER-012, the no_close_edge detail and the human reason.
type closeEdgeRefusal struct{ state types.LadderState }

func (e *closeEdgeRefusal) Error() string {
	return "TROUBLE-LADDER-012: incident state " + string(e.state) +
		" has no legal edge to resolved; a human close is defined from verifying (T38) or suppressed (T45)"
}

func (e *closeEdgeRefusal) ErrorCode() types.ErrorCode { return types.CodeLadder012 }
func (e *closeEdgeRefusal) ErrorDetail() string        { return "no_close_edge" }
func (e *closeEdgeRefusal) ErrorReason() string {
	return "incident state " + string(e.state) + " has no legal edge to resolved"
}
func (e *closeEdgeRefusal) RefusedTransition() bool { return true }

// TestCloseRefusalSurfacesLadderCode: closing a non-closable incident is a
// refused state transition — 409 Conflict with the ladder's code, the
// no_close_edge detail and the reason in the message — never a generic 500.
func TestCloseRefusalSurfacesLadderCode(t *testing.T) {
	env := newEnv(t, envOptions{cfg: unlimitedRates})
	inc := validIncidentID()
	env.actions.closeErr = &closeEdgeRefusal{state: types.StRecorded}

	payload := `{"reason":"human close","resolution":"fixed"}`
	resp, body := env.post("/api/incidents/"+inc+"/close", env.writePlain, payload)
	wantStatus(t, resp, body, http.StatusConflict)
	eb := decodeError(t, body)
	if eb.Error.Code != string(types.CodeLadder012) {
		t.Fatalf("code = %s, want the ladder's %s", eb.Error.Code, types.CodeLadder012)
	}
	if eb.Detail != "no_close_edge" {
		t.Fatalf("detail = %q, want no_close_edge", eb.Detail)
	}
	if !strings.Contains(eb.Error.Message, "no legal edge to resolved") {
		t.Fatalf("message = %q, want the refusal reason in the message", eb.Error.Message)
	}
}

// TestCloseUnexpectedErrorIsStillGeneric500 keeps the untyped close path
// honest: an error the adapter did not wrap is still the generic 500
// action_failed — the named refusal path must not swallow real faults.
func TestCloseUnexpectedErrorIsStillGeneric500(t *testing.T) {
	env := newEnv(t, envOptions{cfg: unlimitedRates})
	inc := validIncidentID()
	env.actions.closeErr = errors.New("boom")

	payload := `{"reason":"human close","resolution":"fixed"}`
	resp, body := env.post("/api/incidents/"+inc+"/close", env.writePlain, payload)
	wantStatus(t, resp, body, http.StatusInternalServerError)
	eb := decodeError(t, body)
	if eb.Error.Code != string(types.CodeDashboard013) {
		t.Fatalf("code = %s, want %s", eb.Error.Code, types.CodeDashboard013)
	}
	if eb.Detail != "action_failed" {
		t.Fatalf("detail = %q, want action_failed", eb.Detail)
	}
}
