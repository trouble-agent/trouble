package app

// flow_bounds_wiring_test.go — TRBL-039 at the composition root.
//
// SPEC-08 §3.9a's queue was bounded by constants inside internal/flow, so the
// composition root could only ever hand it `flow.SpoolBounds{}` and no operator
// could tune the queue. This file drives the REAL wiring helpers the daemon
// boots with — `flowSpoolBounds` (the resolved SPEC-12 §3.1d `flow.*` keys
// projected onto the bound set) and `newFlowSpool` — and proves the configured
// bound is the one in force on BOTH halves:
//
//	store — four writes at `flow.spool_max_entries = 3` leave three entries and
//	        evict the oldest, where the compiled default (256) would have kept
//	        all four; the control run on the same four writes keeps four, so the
//	        eviction is the config's doing and not the harness's;
//	flow  — a two-hour-old entry is dropped with `drop_reason:"ttl"` under
//	        `flow.spool_ttl = 1h`, where the same entry under the compiled
//	        default (72h) is never TTL-dropped. That is the flow's OWN bound set
//	        (the one `Flow.Run` ticks and `Replay` enforces), so no path is left
//	        running on constants alone while an operator's key sits unread.
//
// The recorder here is a test stub: this test owns the flow's bound behaviour,
// not the ledger's write path (that is covered elsewhere).

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/flow"
	"github.com/totalwindupflightsystems/trouble/internal/lifecycle"
	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// boundsRecorder is a recording ledger writer (flow.Deps.Recorder's shape).
type boundsRecorder struct {
	mu     sync.Mutex
	drafts []types.RecordDraft
}

func (r *boundsRecorder) Append(_ context.Context, d types.RecordDraft) (types.Record, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drafts = append(r.drafts, d)
	return types.Record{Seq: uint64(len(r.drafts)), Kind: d.Kind, Sig: d.Sig, Inc: d.Inc, Payload: d.Payload}, nil
}

// dropReasons returns every `flow` record's drop_reason, in write order.
func (r *boundsRecorder) dropReasons() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, d := range r.drafts {
		if d.Kind != types.KFlow {
			continue
		}
		if reason, ok := d.Payload["drop_reason"].(string); ok {
			out = append(out, reason)
		}
	}
	return out
}

// daemonWithConfig is the resolved config an operator's file produced, carried on
// the Daemon the composition root reads (d.Cfg is what newFlowSpool and
// flowSpoolBounds consume).
func daemonWithConfig(t *testing.T, configText string) *Daemon {
	t.Helper()
	dir := t.TempDir()
	stateRoot := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatalf("state root: %v", err)
	}
	path := filepath.Join(dir, "config.toml")
	// state_root comes FIRST: everything after a section header belongs to that
	// section, and the daemon's own table must not swallow it.
	body := "state_root = \"" + stateRoot + "\"\n" + configText
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	r, err := lifecycle.Resolve(nil, nil, path)
	if err != nil {
		t.Fatalf("Resolve(%s) = %v\n"+
			"a `[flow]` table must be an ordinary registered config surface (SPEC-12 §3.1d)", body, err)
	}
	return &Daemon{Cfg: r.Config}
}

// queueEntry is one dispatch the flow parked on its durable queue. The task id is
// GENERATED rather than spelled: a hard-coded ULID-shaped literal trips the repo's
// own secret scanner (`generic-api-key` by entropy), which would put new noise in
// every diff that carries it.
func queueEntry(ts time.Time) types.SpoolEntry {
	taskID := types.NewID(types.PTsk)
	return types.SpoolEntry{
		ID: types.NewID(types.PEv), TS: types.FormatUTC(ts), Kind: types.SpoolSpawn,
		Payload:   []byte(`{"task_id":"` + taskID + `"}`),
		IdemKey:   taskID,
		NextTryTS: types.FormatUTC(ts),
	}
}

// TestFlowQueueEvictsAtTheConfiguredBoundNotAtTheConstant is acceptance
// criterion 3: the operator's `flow.spool_max_entries` is the bound the store
// really applies. Four writes at a configured bound of 3 must leave three
// entries; the same four writes with no `[flow]` table must leave four, which is
// what makes the first half evidence instead of coincidence.
func TestFlowQueueEvictsAtTheConfiguredBoundNotAtTheConstant(t *testing.T) {
	t.Run("configured bound 3", func(t *testing.T) {
		d := daemonWithConfig(t, "[flow]\nspool_max_entries = 3\n")
		clk := NewClock(time.Now())

		bounds := flowSpoolBounds(d.Cfg)
		if bounds.MaxEntries != 3 {
			t.Fatalf("flowSpoolBounds(d.Cfg).MaxEntries = %d, want 3 — the resolved key did not reach the bound set", bounds.MaxEntries)
		}
		// The four the operator did NOT set stay at the §3.9a defaults.
		if bounds.TTL != 72*time.Hour || bounds.MaxAttempts != 5 ||
			bounds.ReplayEvery != 5*time.Second || bounds.ReplayBatch != 100 {
			t.Fatalf("unset bounds = (%v, %d, %v, %d), want the §3.9a defaults (72h, 5, 5s, 100)",
				bounds.TTL, bounds.MaxAttempts, bounds.ReplayEvery, bounds.ReplayBatch)
		}

		spool, err := newFlowSpool(d, clk, bounds)
		if err != nil {
			t.Fatalf("newFlowSpool: %v", err)
		}
		if got := spool.Bounds().MaxEntries; got != 3 {
			t.Fatalf("the built store's bound = %d, want the configured 3", got)
		}

		base := clk.Now()
		ids := make([]string, 0, 4)
		var overflowDrops []flow.DropEvent
		for i := 0; i < 4; i++ {
			e := queueEntry(base.Add(time.Duration(i) * time.Second))
			ids = append(ids, e.ID)
			drops, err := spool.Put(e)
			if err != nil {
				t.Fatalf("Put(%d): %v", i, err)
			}
			overflowDrops = append(overflowDrops, drops...)
		}
		if got := spool.Count(); got != 3 {
			t.Fatalf("queue depth after 4 writes at a configured bound of 3 = %d, want 3 "+
				"(the compiled default 256 would have kept all four)", got)
		}
		if len(overflowDrops) != 1 || overflowDrops[0].ID != ids[0] || overflowDrops[0].Reason != "overflow" {
			t.Fatalf("overflow drops = %+v, want exactly the OLDEST entry %s with reason \"overflow\"", overflowDrops, ids[0])
		}
		entries, err := spool.List()
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(entries) != 3 {
			t.Fatalf("List returned %d entries, want 3", len(entries))
		}
		for _, e := range entries {
			if e.ID == ids[0] {
				t.Fatalf("the oldest entry survived the configured bound: %+v", entries)
			}
		}
		if _, err := os.Stat(filepath.Join(spool.Root(), ids[0]+".json")); !os.IsNotExist(err) {
			t.Fatalf("the evicted entry is still on disk: stat err = %v", err)
		}
	})

	t.Run("compiled default", func(t *testing.T) {
		d := daemonWithConfig(t, "")
		clk := NewClock(time.Now())
		bounds := flowSpoolBounds(d.Cfg)
		if bounds.MaxEntries != 256 {
			t.Fatalf("flowSpoolBounds(d.Cfg).MaxEntries = %d, want the compiled default 256 when no [flow] table is declared", bounds.MaxEntries)
		}
		spool, err := newFlowSpool(d, clk, bounds)
		if err != nil {
			t.Fatalf("newFlowSpool: %v", err)
		}
		base := clk.Now()
		for i := 0; i < 4; i++ {
			if _, err := spool.Put(queueEntry(base.Add(time.Duration(i) * time.Second))); err != nil {
				t.Fatalf("Put(%d): %v", i, err)
			}
		}
		if got := spool.Count(); got != 4 {
			t.Fatalf("queue depth after 4 writes with no configured bound = %d, want 4", got)
		}
	})
}

// TestFlowReplayUsesTheConfiguredTTLNotTheConstant is the second half of
// acceptance criterion 2: the bounds reach the FLOW (not only the store), so the
// loop the daemon ticks enforces the operator's number. A two-hour-old entry is
// TTL-dropped under `flow.spool_ttl = 1h`; under the compiled default (72h) the
// same entry is never TTL-dropped.
func TestFlowReplayUsesTheConfiguredTTLNotTheConstant(t *testing.T) {
	// The seeded payload is undecodable on purpose: it keeps the default-bound
	// control case off the network (the corrupt branch drops it) while leaving
	// the TTL branch — which runs FIRST — the one under test.
	const seed = `not-json`

	cases := []struct {
		name     string
		config   string
		bounds   func(cfg lifecycle.Config) flow.SpoolBounds
		wantDrop string
	}{
		{
			name:     "configured TTL of 1h drops a two-hour-old entry",
			config:   "[flow]\nspool_ttl = \"1h\"\n",
			bounds:   flowSpoolBounds,
			wantDrop: "ttl",
		},
		{
			name:     "compiled default TTL of 72h keeps it (the corrupt branch drops it instead)",
			config:   "[flow]\nspool_ttl = \"1h\"\n",
			bounds:   func(lifecycle.Config) flow.SpoolBounds { return flow.SpoolBounds{}.WithDefaults() },
			wantDrop: "corrupt",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := daemonWithConfig(t, tc.config)
			clk := NewClock(time.Now())
			bounds := tc.bounds(d.Cfg)

			spool, err := newFlowSpool(d, clk, bounds)
			if err != nil {
				t.Fatalf("newFlowSpool: %v", err)
			}
			now := clk.Now()
			e := queueEntry(now.Add(-2 * time.Hour))
			e.Payload = []byte(seed)
			if _, err := spool.Put(e); err != nil {
				t.Fatalf("seed Put: %v", err)
			}

			fl, err := newFlowSubsystem(d, types.FlowConfig{
				Driver: types.FlowDriverBoard, ReviewMode: types.FlowReviewAuto, BoardPath: d.Cfg.StateRoot,
			}, bounds)
			if err != nil {
				t.Fatalf("newFlowSubsystem: %v", err)
			}
			rec := &boundsRecorder{}
			fl.SetDeps(flow.Deps{Recorder: rec, Spool: spool, Clock: clk})
			if !fl.SpoolWired() {
				t.Fatalf("the flow did not adopt the queue, so Replay would report nothing at all")
			}
			if _, err := fl.Replay(context.Background(), 0); err != nil {
				t.Fatalf("Replay: %v", err)
			}

			got := rec.dropReasons()
			if len(got) != 1 || got[0] != tc.wantDrop {
				t.Fatalf("drop reasons = %v, want exactly [%s] (configured TTL %v)", got, tc.wantDrop, bounds.TTL)
			}
			if n := spool.Count(); n != 0 {
				t.Fatalf("queue depth after the drop = %d, want 0", n)
			}
		})
	}
}
