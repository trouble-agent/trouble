package ledger

// ack_state_test.go — TRBL-118: the incident index folds a dashboard ack into
// the state `acknowledged` (SPEC-05 §3.2a), live and on a boot rebuild, and an
// observation that folds into the same live incident does not re-derive the state
// back to `detected`.
//
// The ledger is authoritative and the index is a cache (SPEC-05 INV-2), so the
// two paths must agree: a live append and a rebuild from the same records have to
// land on the same state, and a record with no `state` key (the pre-amendment ack
// a running daemon already wrote) must not fall through to the `detected`
// default.

import (
	"context"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

func TestAckRecordOwnsAcknowledgedState(t *testing.T) {
	clk := newFakeClock(testNow())
	root := testRoot(t)
	l := testLedgerAt(t, root, clk)
	inc := types.NewID(types.PInc)
	sig := "psi:sha256v1:9f2c1d3e4b5a6c7d"
	origin := types.Origin{HostID: "7f3a91c2d4e5b607", Source: "psi"}

	// T01: the admission record carries no `state` key (it emits from/to), which
	// is why the index starts at the `detected` default.
	mustAppend(t, l, types.RecordDraft{
		Kind: types.KIncident, Sig: sig, Inc: inc, Origin: origin, Actor: testActor(),
		Payload: map[string]any{"transition": "T01", "from": "detected", "to": "recorded", "rung": "play"},
	})
	// The legacy ack record — the shape a daemon at the reported HEAD writes:
	// a transition name and the operator's hold, no `state`.
	mustAppend(t, l, types.RecordDraft{
		Kind: types.KIncident, Sig: sig, Inc: inc, Origin: origin, Actor: testActor(),
		Payload: map[string]any{"transition": "acknowledged", "reason": "on it", "until": "15m", "via": "dashboard"},
	})

	want := func(t *testing.T, l *Ledger, when string) {
		t.Helper()
		got, _, err := l.Query().Incident(inc)
		if err != nil {
			t.Fatalf("%s: Incident(%s): %v", when, inc, err)
		}
		if got == nil {
			t.Fatalf("%s: the index does not know incident %s", when, inc)
		}
		if got.State != types.StAcknowledged {
			t.Fatalf("%s: index state is %q, want acknowledged", when, string(got.State))
		}
	}

	want(t, l, "live append")

	// The T49 transition record the amended ladder writes: it stamps the state
	// explicitly, and the index keeps it.
	mustAppend(t, l, types.RecordDraft{
		Kind: types.KIncident, Sig: sig, Inc: inc, Origin: origin, Actor: testActor(),
		Payload: map[string]any{"transition": "T49", "from": "recorded", "to": "acknowledged", "state": "acknowledged"},
	})
	want(t, l, "after T49")

	// An observation folds into the same live incident: `from`/`to` carry the
	// current state but there is no `state` key. This is the record that used to
	// snap the index back to `detected`.
	mustAppend(t, l, types.RecordDraft{
		Kind: types.KIncident, Sig: sig, Inc: inc, Origin: origin, Actor: testActor(),
		Payload: map[string]any{"transition": "fold", "from": "acknowledged", "to": "acknowledged", "folded": true},
	})
	want(t, l, "after a fold")

	live, _, err := l.Query().Incident(inc)
	if err != nil {
		t.Fatalf("Incident: %v", err)
	}

	// A boot rebuild from the same records must land on the same projection.
	if err := l.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	l2 := testLedgerAt(t, root, clk)
	want(t, l2, "boot rebuild")
	rebuilt, _, err := l2.Query().Incident(inc)
	if err != nil {
		t.Fatalf("Incident (rebuilt): %v", err)
	}
	if rebuilt.State != live.State {
		t.Fatalf("boot rebuild state %q != live state %q", string(rebuilt.State), string(live.State))
	}
	if rebuilt.Sig != live.Sig || rebuilt.UpdatedTS != live.UpdatedTS {
		t.Fatalf("boot rebuild projection differs from the live one: %+v != %+v", rebuilt, live)
	}
}
