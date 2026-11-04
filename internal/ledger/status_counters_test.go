package ledger

// status_counters_test.go — the ladder's ledger-derived rung counters
// (SPEC-01 §3.10a, TRBL-123).
//
// The operator question these answer: did the ladder actually REMEDIATE, or did
// it only detect and audit? `trouble ledger status` reports play_runs/agent_runs
// from the records on disk, so the answer survives a restart and does not depend
// on the daemon's in-memory state.

import (
	"context"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// TestReadStatusDerivesPlayAndAgentRunCounters pins the derivation: only records
// that carry a rung attribution (payload.rung_entry, SPEC-05 §3.2b) count, and
// they count per record — a record that merely mentions play_runs, or an
// unrelated record, is not a rung entry.
func TestReadStatusDerivesPlayAndAgentRunCounters(t *testing.T) {
	clk := newFakeClock(testNow())
	o := baseTestOptions(t, clk)
	l, err := Open(context.Background(), o)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	root := o.Root
	before, err := ReadStatus(root)
	if err != nil {
		t.Fatalf("baseline ReadStatus: %v", err)
	}

	appendIncident := func(sig string, payload map[string]any) {
		t.Helper()
		if _, aerr := l.Append(context.Background(), types.RecordDraft{
			Kind:    types.KIncident,
			Sig:     sig,
			Inc:     "inc_" + sig,
			Origin:  types.Origin{HostID: "7f3a91c2d4e5b607", Source: "troubled"},
			Actor:   testActor(),
			Payload: payload,
		}); aerr != nil {
			t.Fatalf("Append(%s): %v", sig, aerr)
		}
	}

	// Two play-rung entries, one agent-rung entry.
	appendIncident("psi:sha256v1:rung0000000000000001", map[string]any{
		"transition": "T03", "rung": "play", "rung_entry": "play", "play_runs": 0,
	})
	appendIncident("psi:sha256v1:rung0000000000000002", map[string]any{
		"transition": "T13", "rung": "play", "rung_entry": "play", "play_runs": 1,
	})
	appendIncident("psi:sha256v1:rung0000000000000003", map[string]any{
		"transition": "T05", "rung": "agent", "rung_entry": "agent", "agent_runs": 0,
	})
	// A research entry is a rung entry, but not one of the two counters.
	appendIncident("psi:sha256v1:rung0000000000000004", map[string]any{
		"transition": "T04", "rung": "research", "rung_entry": "research",
	})
	// A record that mentions the counter fields but entered no rung (the shape
	// every incident record has, because appendIncident defaults them).
	appendIncident("psi:sha256v1:rung0000000000000005", map[string]any{
		"transition": "T01", "rung": "play", "play_runs": 0, "agent_runs": 0,
	})

	st, err := ReadStatus(root)
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	if st.PlayRuns != 2 {
		t.Errorf("status.play_runs = %d, want 2", st.PlayRuns)
	}
	if st.AgentRuns != 1 {
		t.Errorf("status.agent_runs = %d, want 1", st.AgentRuns)
	}
	if got, want := st.Records-before.Records, int64(5); got != want {
		t.Errorf("the derivation changed the record count: %d new record(s), want %d", got, want)
	}

	// A second read is stable: the counters are derived, never accumulated.
	again, err := ReadStatus(root)
	if err != nil {
		t.Fatalf("second ReadStatus: %v", err)
	}
	if again.PlayRuns != st.PlayRuns || again.AgentRuns != st.AgentRuns {
		t.Errorf("second read = (%d,%d), want the same (%d,%d)", again.PlayRuns, again.AgentRuns, st.PlayRuns, st.AgentRuns)
	}
}

// TestReadStatusCountersAreZeroOnAnEmptyLedger pins the absent case: a ledger
// with no rung records reports zero, never a stale or invented count.
func TestReadStatusCountersAreZeroOnAnEmptyLedger(t *testing.T) {
	clk := newFakeClock(testNow())
	o := baseTestOptions(t, clk)
	l, err := Open(context.Background(), o)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := l.Append(context.Background(), eventDraft("psi", "psi:sha256v1:plain00000000001", "deadbeef", 0)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	st, err := ReadStatus(o.Root)
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	if st.PlayRuns != 0 || st.AgentRuns != 0 {
		t.Fatalf("status counters = (%d,%d) on a ledger with no rung entry, want (0,0)", st.PlayRuns, st.AgentRuns)
	}
}
