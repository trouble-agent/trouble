package ledger

// incident_subject_test.go — TRBL-126: the incident subject (the unit and the
// message that produced the signal) is a record fact, not a live-process one. It
// must survive a boot rebuild, and the read-only reader `trouble topology` uses
// must report it from the day files without taking the writer seat.

import (
	"context"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

const testIncidentSubject = "payment-worker.service: unit_substate=failed via properties_changed"

func TestIncidentSubjectIsAProjectionFact(t *testing.T) {
	clk := newFakeClock(testNow())
	root := testRoot(t)
	l := testLedgerAt(t, root, clk)

	inc := types.NewID(types.PInc)
	grp := types.NewID(types.PGrp)
	sig := "dbus:sha256v1:1a2b3c4d5e6f7a8b"
	origin := types.Origin{HostID: "7f3a91c2d4e5b607", Source: "dbus"}

	// The T01 admission record the ladder writes, with the subject TRBL-126 adds.
	mustAppend(t, l, types.RecordDraft{
		Kind: types.KIncident, Sig: sig, Inc: inc, Origin: origin, Actor: testActor(),
		Payload: map[string]any{
			"transition": "T01", "from": "detected", "to": "recorded",
			"rung": "play", "entry_rung": "play", "grp": grp,
			"subject": testIncidentSubject,
		},
	})

	want := func(t *testing.T, when string, got *types.Incident) {
		t.Helper()
		if got == nil {
			t.Fatalf("%s: the index does not know incident %s", when, inc)
		}
		if got.Subject != testIncidentSubject {
			t.Fatalf("%s: subject = %q, want %q", when, got.Subject, testIncidentSubject)
		}
		if got.Sig != sig {
			t.Fatalf("%s: sig = %q, want the raw signature %q to be kept", when, got.Sig, sig)
		}
	}

	live, _, err := l.Query().Incident(inc)
	if err != nil {
		t.Fatalf("Incident: %v", err)
	}
	want(t, "live append", live)

	// The read-only path behind `trouble topology`'s incident tail reads the
	// same subject from the day files, with no writer seat taken.
	open, err := ReadOpenIncidents(root, 10)
	if err != nil {
		t.Fatalf("ReadOpenIncidents: %v", err)
	}
	var ro *types.Incident
	for i := range open {
		if open[i].ID == inc {
			ro = &open[i]
		}
	}
	want(t, "read-only reader", ro)

	// A boot rebuild from the same records lands on the same subject.
	if err := l.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	l2 := testLedgerAt(t, root, clk)
	rebuilt, _, err := l2.Query().Incident(inc)
	if err != nil {
		t.Fatalf("Incident (rebuilt): %v", err)
	}
	want(t, "boot rebuild", rebuilt)
	if rebuilt.Subject != live.Subject {
		t.Fatalf("boot rebuild subject %q != live subject %q", rebuilt.Subject, live.Subject)
	}
}
