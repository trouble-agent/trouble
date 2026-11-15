package ledger

// incident_state_fold_test.go — TRBL-127: the incident projection must report the
// state the ladder actually reached for an incident record that carries no
// `state` key, taking it from the transition's `to` destination. The T01
// admission record is the common case: `from:detected to:recorded` with no
// `state`, which used to leave the index (and therefore the dashboard's incident
// page) at the `detected` default while the ladder and `trouble ledger status`
// said `recorded`.
//
// The second half is the guard the fix must not break: a REFUSED transition
// records the ATTEMPTED target in `to` and leaves the machine byte-identical
// (SPEC-05 §3.3), so its `to` must never be folded into the projection.

import (
	"context"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

func TestIncidentProjectionFoldsTransitionDestination(t *testing.T) {
	clk := newFakeClock(testNow())
	root := testRoot(t)
	l := testLedgerAt(t, root, clk)
	inc := types.NewID(types.PInc)
	sig := "psi:sha256v1:5b1e7c9a2d4f6081"
	origin := types.Origin{HostID: "7f3a91c2d4e5b607", Source: "psi"}

	// T01 admission: the shape the ladder writes today — no `state` key.
	mustAppend(t, l, types.RecordDraft{
		Kind: types.KIncident, Sig: sig, Inc: inc, Origin: origin, Actor: testActor(),
		Payload: map[string]any{
			"transition": "T01", "from": string(types.StDetected), "to": string(types.StRecorded),
			"entry_rung": "play", "rung": "play",
		},
	})

	indexState := func(t *testing.T, when string) types.LadderState {
		t.Helper()
		got, _, err := l.Query().Incident(inc)
		if err != nil {
			t.Fatalf("%s: Incident(%s): %v", when, inc, err)
		}
		if got == nil {
			t.Fatalf("%s: the index does not know incident %s", when, inc)
		}
		return got.State
	}

	if got := indexState(t, "after T01"); got != types.StRecorded {
		t.Fatalf("after T01: index state is %q, want recorded (the transition's `to`, not the detected default)", string(got))
	}

	// SPEC-05 §3.3: a refusal names the target it refused to move to. Folding it
	// would fabricate a state the machine never entered, so this row must leave
	// the projection at `recorded`.
	mustAppend(t, l, types.RecordDraft{
		Kind: types.KIncident, Sig: sig, Inc: inc, Origin: origin, Actor: testActor(),
		Payload: map[string]any{
			"transition": "T50", "from": string(types.StRecorded), "to": string(types.StResolved),
			"refused": true, "illegal": true, "error_code": "TROUBLE-LADDER-001",
		},
	})
	if got := indexState(t, "after a refusal"); got != types.StRecorded {
		t.Fatalf("after a refusal: index state is %q, want recorded — a refused `to` is not a state the incident reached", string(got))
	}

	// A later state-bearing record still wins outright.
	mustAppend(t, l, types.RecordDraft{
		Kind: types.KIncident, Sig: sig, Inc: inc, Origin: origin, Actor: testActor(),
		Payload: map[string]any{
			"transition": "T49", "from": string(types.StRecorded), "to": string(types.StAcknowledged),
			"state": string(types.StAcknowledged),
		},
	})
	if got := indexState(t, "after T49"); got != types.StAcknowledged {
		t.Fatalf("after T49: index state is %q, want acknowledged", string(got))
	}

	// The lock-free read `trouble ledger status` prints its incident tail from
	// must agree with the live index for the same id.
	open, err := ReadOpenIncidents(root, 50)
	if err != nil {
		t.Fatalf("ReadOpenIncidents: %v", err)
	}
	var status types.LadderState
	for _, row := range open {
		if row.ID == inc {
			status = row.State
			break
		}
	}
	if status == "" {
		t.Fatalf("ReadOpenIncidents did not list open incident %s", inc)
	}
	if live := indexState(t, "live"); status != live {
		t.Fatalf("ledger status state %q != live index state %q for %s", string(status), string(live), inc)
	}

	// A boot rebuild folds the same records through the same function and must
	// land on the same state.
	if err := l.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	rebuilt := testLedgerAt(t, root, clk)
	got, _, err := rebuilt.Query().Incident(inc)
	if err != nil {
		t.Fatalf("rebuilt Incident(%s): %v", inc, err)
	}
	if got == nil || got.State != types.StAcknowledged {
		t.Fatalf("rebuild state = %v, want acknowledged", got)
	}
}

// TRBL-127 companion pin at the mapping level (the dashboard render path has its
// own end-to-end test in internal/app, which carries the row's ch:trace marker).
