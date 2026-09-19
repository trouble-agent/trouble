package app

// flow_spool_wiring_test.go — SPEC-08 §3.9a at the composition root.
//
// This is the test the defect needed: it drives `flowDeps` — the REAL wiring the
// daemon boots with — with the issue desk OFF (the shipped posture, SPEC-09
// §3.4a) and asserts the flow still has a durable dispatch queue it can replay.
// Before §3.9a the same call handed the flow `deskSpool`, whose `Enqueue` reaches
// `issues.Desk.EnqueueSpool`, which REFUSES with TROUBLE-ISSUES-003 while the desk
// is off — so a spawn dispatch could be neither delivered nor durably queued, and
// the flow had no drain of its own to notice.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/flow"
	"github.com/totalwindupflightsystems/trouble/internal/lifecycle"
	"github.com/totalwindupflightsystems/trouble/internal/types"
)

func TestFlowDepsWiresAQueueTheFlowCanReplayWithTheDeskOff(t *testing.T) {
	stateRoot := t.TempDir()
	d := &Daemon{Cfg: lifecycle.Config{StateRoot: stateRoot}}
	clk := NewClock(time.Now())

	spool, err := newFlowSpool(d, clk)
	if err != nil {
		t.Fatalf("newFlowSpool: %v", err)
	}
	// The path is the §3.9a one: under the SPEC-12 §3.2 state tree's `spool/`
	// subtree, in the flow's own subdirectory.
	want := filepath.Join(stateRoot, "spool", "flow", "spawn")
	if spool.Root() != want {
		t.Fatalf("queue root = %q, want %q", spool.Root(), want)
	}
	if fi, err := os.Stat(spool.Root()); err != nil {
		t.Fatalf("stat queue root: %v", err)
	} else if fi.Mode().Perm() != 0o700 {
		t.Fatalf("queue root mode = %v, want 0700", fi.Mode().Perm())
	}

	fl, err := flow.NewFlow(types.FlowConfig{
		Driver: types.FlowDriverBoard, ReviewMode: types.FlowReviewAuto, BoardPath: stateRoot,
	}, types.DefaultGates())
	if err != nil {
		t.Fatalf("NewFlow: %v", err)
	}
	// The desk is nil: the SHIPPED posture (`issues.enabled = false`).
	deps := flowDeps(d, &Subsystems{}, "7f3a91c2d4e5b607",
		types.Actor{Kind: types.ActorDaemon, ID: "troubled"}, clk, spool)
	fl.SetDeps(deps)

	if !fl.SpoolWired() {
		t.Fatalf("the composition root handed the flow a sink it cannot replay: with the issue desk off " +
			"a spawn dispatch would be neither delivered nor durably queued")
	}

	// And the sink the flow received really writes where §3.9a says: one 0600
	// file per entry, under the state root.
	entry := types.SpoolEntry{
		ID: types.NewID(types.PEv), TS: types.FormatUTC(time.Now()), Kind: types.SpoolSpawn,
		Payload:   []byte(`{"task_id":"tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ"}`),
		IdemKey:   "tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ",
		NextTryTS: types.FormatUTC(time.Now()),
	}
	if err := deps.Spool.Enqueue(context.Background(), entry); err != nil {
		t.Fatalf("Enqueue through the wired sink: %v", err)
	}
	des, err := os.ReadDir(spool.Root())
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(des) != 1 {
		t.Fatalf("files under the queue root = %d, want 1", len(des))
	}
	fi, err := des[0].Info()
	if err != nil {
		t.Fatalf("stat entry: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("entry mode = %v, want 0600", fi.Mode().Perm())
	}
	// List is the replay side: if this returns the entry, the queue the flow owns
	// is the queue it drains.
	entries, err := spool.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].IdemKey != entry.IdemKey {
		t.Fatalf("the wired store does not list what it was given: %+v", entries)
	}
}

func TestFlowDepsWiresNoQueueWhenTheStoreCannotBeBuilt(t *testing.T) {
	stateRoot := t.TempDir()
	// A file where the spool directory belongs: the store cannot be built, so the
	// flow gets no queue and every dispatch record must say so.
	if err := os.WriteFile(filepath.Join(stateRoot, "spool"), []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("block the spool path: %v", err)
	}
	d := &Daemon{Cfg: lifecycle.Config{StateRoot: stateRoot}}
	spool, err := newFlowSpool(d, NewClock(time.Now()))
	if err == nil {
		t.Fatalf("newFlowSpool accepted a blocked path (root=%v)", spool)
	}
	// The daemon passes a nil store in that case; the flow must then report the
	// absence rather than claiming durability.
	fl, ferr := flow.NewFlow(types.FlowConfig{
		Driver: types.FlowDriverBoard, ReviewMode: types.FlowReviewAuto, BoardPath: stateRoot,
	}, types.DefaultGates())
	if ferr != nil {
		t.Fatalf("NewFlow: %v", ferr)
	}
	fl.SetDeps(flowDeps(d, &Subsystems{}, "7f3a91c2d4e5b607",
		types.Actor{Kind: types.ActorDaemon, ID: "troubled"}, NewClock(time.Now()), nil))
	if fl.SpoolWired() {
		t.Fatalf("SpoolWired() = true with no store")
	}
	if _, rerr := fl.Replay(context.Background(), 0); rerr == nil {
		t.Fatalf("Replay with no queue must refuse, not report 0 replayed as success")
	}
}
