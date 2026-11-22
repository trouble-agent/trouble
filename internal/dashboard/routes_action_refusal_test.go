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

// ---------------------------------------------------------------------------
// TRBL-144: NO operator action on a legal-but-refused transition returns 5xx.
// The ladder's *Error now carries the seam shape itself (the composition root
// adapts it), so every typed refusal — refused transition, transient, or a
// plain policy refusal — must reach the wire on all three operator writes
// (ack, close, autonomy) with its owning code, never the generic 500
// action_failed. The sweep drives each route through the real handler with an
// equivalent carrier of the seam shape (the packages cannot import each
// other), the same discipline as the close test above.
// ---------------------------------------------------------------------------

// seamCarrier is the full seam shape a write seam may expose: the owning
// code, the machine-readable detail, the human reason and the
// refused-transition mark. Zero-value fields mean "absent", exactly like a
// seam carrier that implements only part of the interfaces.
type seamCarrier struct {
	code      types.ErrorCode
	detail    string
	reason    string
	refused   bool
	withShape bool // false = the carrier implements ErrorCode only
}

func (e *seamCarrier) Error() string { return "seam carrier: " + string(e.code) }

func (e *seamCarrier) ErrorCode() types.ErrorCode { return e.code }

func (e *seamCarrier) ErrorDetail() string {
	if !e.withShape {
		return ""
	}
	return e.detail
}

func (e *seamCarrier) ErrorReason() string {
	if !e.withShape {
		return ""
	}
	return e.reason
}

func (e *seamCarrier) RefusedTransition() bool {
	if !e.withShape {
		return false
	}
	return e.refused
}

func TestOperatorRefusalsNeverReturn5xx(t *testing.T) {
	rows := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   types.ErrorCode
	}{
		{
			name: "refused transition (close on a state with no/close edge)",
			err: &seamCarrier{code: types.CodeLadder012, detail: "no_close_edge",
				reason: "incident state recorded has no legal edge to resolved", refused: true, withShape: true},
			wantStatus: http.StatusConflict,
			wantCode:   types.CodeLadder012,
		},
		{
			name: "transient ladder refusal",
			err: &seamCarrier{code: types.CodeLadder015, detail: "ladder_015",
				reason: "park_failed", refused: false, withShape: true},
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   types.CodeLadder015,
		},
		{
			name:       "typed policy refusal of another subsystem",
			err:        &seamCarrier{code: types.CodeRegistry006},
			wantStatus: http.StatusBadRequest,
			wantCode:   types.CodeRegistry006,
		},
	}

	for _, row := range rows {
		for _, route := range []struct {
			name     string
			path     string
			payload  string
			tokenIdx func(*testEnv) string
		}{
			{"ack", "/ack", `{"reason":"operator check"}`, func(e *testEnv) string { return e.writePlain }},
			{"close", "/close", `{"reason":"operator check","resolution":"fixed"}`, func(e *testEnv) string { return e.writePlain }},
			{"autonomy", "", `{"mode":"assisted"}`, func(e *testEnv) string { return e.autoPlain }},
		} {
			t.Run(row.name+"/"+route.name, func(t *testing.T) {
				env := newEnv(t, envOptions{cfg: unlimitedRates})
				switch route.name {
				case "ack":
					env.actions.ackErr = row.err
				case "close":
					env.actions.closeErr = row.err
				case "autonomy":
					env.autonomy.err = row.err
				}
				path := "/api/autonomy"
				if route.name != "autonomy" {
					path = "/api/incidents/" + validIncidentID() + route.path
				}
				resp, body := env.post(path, route.tokenIdx(env), route.payload)
				if resp.StatusCode == http.StatusInternalServerError {
					t.Fatalf("%s returned the generic 500 (%.300s): a typed seam refusal must never collapse into action_failed", route.name, body)
				}
				wantStatus(t, resp, body, row.wantStatus)
				eb := decodeError(t, body)
				if eb.Error.Code != string(row.wantCode) {
					t.Fatalf("error.code = %s, want the owning code %s (body %.300s)", eb.Error.Code, row.wantCode, body)
				}
			})
		}
	}
}
