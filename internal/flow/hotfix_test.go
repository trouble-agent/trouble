package flow

// hotfix_test.go — SPEC-08 §7 `hotfix_gate_test.go`, `lease_test.go`,
// `spawn_test.go` and `promote_test.go`: the 10-check chain, the one-fix-per-sig
// lease, the spawn state machine with its spawn_pending fallback, and the
// promotion paths. AC-19's timeline and AC-21's 60s budget are asserted here too.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// spawnReq builds a spawn request for the fixture.
func (f *fixture) spawnReq(sig string) types.SpawnRequest {
	if sig == "" {
		sig = f.incident().Sig
	}
	return types.SpawnRequest{
		Inc: f.incident().ID, Sig: sig, Repo: f.repo, TaskID: "tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ",
		PriorityClass: "hotfix",
	}
}

func TestConfigRefusals(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*types.FlowConfig)
	}{
		{"max_concurrent above the hard cap", func(c *types.FlowConfig) { c.Hotfix.MaxConcurrent = 5 }},
		{"absolute worktree base", func(c *types.FlowConfig) { c.Hotfix.WorktreeBase = "/tmp/x" }},
		{"parent-relative worktree base", func(c *types.FlowConfig) { c.Hotfix.WorktreeBase = "../w" }},
		{"unknown driver", func(c *types.FlowConfig) { c.Driver = "wat" }},
		{"unknown review mode", func(c *types.FlowConfig) { c.ReviewMode = "wat" }},
		{"never with hotfix on", func(c *types.FlowConfig) {
			c.ReviewMode = types.FlowReviewNever
			c.Hotfix.Enabled = true
		}},
		{"unknown promote mode", func(c *types.FlowConfig) { c.Hotfix.Promote = "wat" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := types.FlowConfig{}
			tc.mut(&cfg)
			if _, err := NewFlow(cfg, types.DefaultGates()); err == nil {
				t.Fatalf("NewFlow accepted a config it must refuse")
			}
		})
	}
}

func TestHotfixGateChain(t *testing.T) {
	ctx := context.Background()
	newFlow := func(mut func(*types.FlowConfig)) (*fixture, func(types.SpawnRequest) (types.SpawnRequest, error)) {
		fx := newFixture(t, mut)
		return fx, func(req types.SpawnRequest) (types.SpawnRequest, error) {
			return fx.flow.Spawn(ctx, req)
		}
	}

	// 1. the lane is disabled → the row stands, the spawn is skipped, no error.
	fx, spawn := newFlow(func(c *types.FlowConfig) { c.Hotfix.Enabled = false })
	req := fx.spawnReq("")
	got, err := spawn(req)
	if err != nil {
		t.Fatalf("a disabled lane must not error: %v", err)
	}
	mustEqual(t, got.State, "", "disabled lane state")
	if p := fx.rec.last(types.KFlow); p["error_code"] != string(types.CodeFlow006) {
		t.Fatalf("flow record = %v", p)
	}
	if fx.spawn.calls != 0 {
		t.Fatalf("a disabled lane must not call the admission path")
	}

	// 3. severity below the threshold.
	fx2, spawn2 := newFlow(nil)
	fx2.severity = types.SevMedium
	if _, err := spawn2(fx2.spawnReq("")); err == nil {
		t.Fatalf("a below-threshold severity must be refused")
	}

	// 4. rule not hot-fix.
	fx3, spawn3 := newFlow(nil)
	fx3.hotfix = false
	if _, err := spawn3(fx3.spawnReq("")); err == nil {
		t.Fatalf("a non-hotfix rule must be refused")
	}

	// 5. repo not allowed.
	fx4, spawn4 := newFlow(func(c *types.FlowConfig) { c.Hotfix.AllowedRepos = []string{"/somewhere/else"} })
	if _, err := spawn4(fx4.spawnReq("")); err == nil {
		t.Fatalf("a repo outside allowed_repos must be refused")
	}

	// 6. registration proof missing.
	fx5, spawn5 := newFlow(func(c *types.FlowConfig) {
		c.Projects["payment-api"] = setRegistered(c.Projects["payment-api"], false, "scheduler_project_absent")
	})
	if _, err := spawn5(fx5.spawnReq("")); err == nil {
		t.Fatalf("an unproven project must be refused")
	}

	// 7. the lease is held by another incident.
	fx6, spawn6 := newFlow(nil)
	fx6.flow.leases["sig:held"] = types.HotfixLease{
		Sig: "sig:held", Inc: "inc_other", Holder: "inc_other",
		GrantedTS: types.NowUTC(),
		ExpiresTS: types.FormatUTC(fx6.clock.Now().Add(30 * time.Minute)),
	}
	if _, err := spawn6(fx6.spawnReq("sig:held")); err == nil {
		t.Fatalf("a held lease must be refused (TROUBLE-FLOW-009)")
	}

	// 9. the disk gate.
	fx7, spawn7 := newFlow(nil)
	fx7.flow.deps.DiskFree = func(string) (int64, error) { return 1, nil }
	if _, err := spawn7(fx7.spawnReq("")); err == nil {
		t.Fatalf("the disk gate must refuse")
	}

	// The happy path reaches the admission call once and lands leased.
	fx8, spawn8 := newFlow(nil)
	ok, err := spawn8(fx8.spawnReq(""))
	if err != nil {
		t.Fatalf("a clean spawn must succeed: %v", err)
	}
	mustEqual(t, ok.State, types.SpawnLeased, "leased state")
	mustEqual(t, ok.PriorityClass, "hotfix", "priority class")
	mustEqual(t, fx8.spawn.calls, 1, "admission calls")
	if ok.Worktree == "" {
		t.Fatalf("the router must report the worktree")
	}
	p := fx8.rec.last(types.KSpawn)
	if p["trig_to_spawn_ms"] == nil || asMS(p["budget_ms"]) != int64(budgetMS) {
		t.Fatalf("the spawn record must carry the 60s budget measurement: %v", p)
	}
	mustEqual(t, p["budget_exceeded"], false, "budget_exceeded")
	if p["brief_sha256"] == nil || p["brief_sha256"] == "" {
		t.Fatalf("the brief must be pinned by hash: %v", p)
	}
}

func TestSpawnPendingFallbackAndQueue(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t, nil)
	fx.spawn.err = errors.New("router_spawn refused the connection")
	got, err := fx.flow.Spawn(ctx, fx.spawnReq(""))
	if err != nil {
		t.Fatalf("a failed admission must still return an outcome: %v", err)
	}
	mustEqual(t, got.State, types.SpawnPending, "state after a failed admission")
	if got.LastError == "" {
		t.Fatalf("the failure must be named on the request")
	}
	if fx.flow.QueueDepth() != 1 {
		t.Fatalf("queue depth = %d, want 1", fx.flow.QueueDepth())
	}
	if fx.spool.len() != 1 {
		t.Fatalf("spool entries = %d, want 1 (the durable queue holds it)", fx.spool.len())
	}
	p := fx.rec.last(types.KSpawn)
	mustEqual(t, p["error_code"], string(types.CodeFlow010), "error code")
	mustEqual(t, p["state"], types.SpawnPending, "recorded state")

	// A second attempt with a working admission path adopts the same task.
	fx.spawn.err = nil
	again, err := fx.flow.Spawn(ctx, got)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	mustEqual(t, again.State, types.SpawnLeased, "state after the retry")
	// The worktree timeout path: acknowledged, but no worktree.
	fx2 := newFixture(t, nil)
	fx2.spawn.worktree = " " // non-empty string with no path
	fx2.spawn.worktree = ""
	fx2.flow.deps.Spawn = noWorktree{fakeSpawn: fx2.spawn}
	out2, err := fx2.flow.Spawn(ctx, fx2.spawnReq(""))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	mustEqual(t, out2.State, types.SpawnPending, "worktree timeout state")
	if p := fx2.rec.last(types.KSpawn); p["error_code"] != string(types.CodeFlow011) {
		t.Fatalf("worktree timeout record = %v", p)
	}
}

// noWorktree acknowledges a spawn without producing a worktree path.
type noWorktree struct{ *fakeSpawn }

func (n noWorktree) RequestSpawn(ctx context.Context, req types.SpawnRequest) (string, string, error) {
	_, _, _ = n.fakeSpawn.RequestSpawn(ctx, req)
	return "spawn-ack", "", nil
}

func TestLeaseOnePerSig(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t, nil)
	// 100 incidents: one grant, 99 refusals.
	granted := 0
	refused := 0
	for i := 0; i < 100; i++ {
		req := fx.spawnReq("sig:one")
		req.Inc = fmt.Sprintf("inc_%03d", i)
		_, err := fx.flow.Spawn(ctx, req)
		if err != nil {
			if !containsCode(err, types.CodeFlow009) {
				t.Fatalf("unexpected refusal: %v", err)
			}
			refused++
			continue
		}
		granted++
	}
	if granted != 1 {
		t.Fatalf("grants = %d, want exactly 1", granted)
	}
	if refused != 99 {
		t.Fatalf("refusals = %d, want 99", refused)
	}
	l, ok := fx.flow.Lease("sig:one")
	if !ok {
		t.Fatalf("the lease must exist")
	}
	if ts, err := types.ParseUTC(l.ExpiresTS); err != nil || ts.Before(fx.clock.Now()) {
		t.Fatalf("lease = %+v, want a live TTL", l)
	}
	// Expiry releases the lease, and a live heartbeat is never stolen.
	fx.clock.Advance(31 * time.Minute)
	if _, err := fx.flow.Spawn(ctx, fx.spawnReq("sig:one")); err != nil {
		t.Fatalf("an expired lease must not block a new spawn: %v", err)
	}
}

// TestExemptRepoIsOwnerSerialized: worktree_exempt is a mode, not a refusal.
func TestExemptRepoIsOwnerSerialized(t *testing.T) {
	fx := newFixture(t, func(c *types.FlowConfig) {
		c.Hotfix.WorktreeExempt = []string{c.Projects["payment-api"].Repo}
	})
	got, err := fx.flow.Spawn(context.Background(), fx.spawnReq(""))
	if err != nil {
		t.Fatalf("an exempt repo must still file: %v", err)
	}
	if got.Worktree == "" {
		t.Fatalf("the exempt path still reports the owner's tree")
	}
	p := fx.rec.last(types.KSpawn)
	mustEqual(t, p["worktree_mode"], types.SpawnModeSerial, "worktree mode")
}

func TestPromotePaths(t *testing.T) {
	ctx := context.Background()
	// (a) human default: a passed tuple yields pending_human.
	fx := newFixture(t, nil)
	sp, err := fx.flow.Spawn(ctx, fx.spawnReq(""))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	p, err := fx.flow.Promote(ctx, sp.ID, "")
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	mustEqual(t, p.Decision, types.PromotePending, "human default decision")
	mustEqual(t, p.VerifyEvidence.Result, types.VerifyPassed, "stored tuple result")

	// (b) a human promotion records the actor and emits one skill candidate.
	p2, err := fx.flow.Promote(ctx, sp.ID, "promote")
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	mustEqual(t, p2.Decision, types.PromoteDone, "promoted decision")
	if p2.DecidedBy == "" {
		t.Fatalf("a human decision must carry the actor")
	}
	mustEqual(t, fx.skills.candidates, 1, "skill candidates")

	// (c) a failed verify is refused with TROUBLE-FLOW-015 and no promotion.
	fx2 := newFixture(t, nil)
	fx2.evidence = types.Evidence{Result: types.VerifyFailed, WindowS: 600}
	sp2, err := fx2.flow.Spawn(ctx, fx2.spawnReq(""))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if _, err := fx2.flow.Promote(ctx, sp2.ID, "promote"); err == nil || !containsCode(err, types.CodeFlow015) {
		t.Fatalf("err = %v, want TROUBLE-FLOW-015", err)
	}
	// Discard on a failed window is the recorded half of TROUBLE-FLOW-015: the
	// decision lands, the refusal is emitted and nothing is promoted.
	dp, err := fx2.flow.Promote(ctx, sp2.ID, "discard")
	if err != nil {
		t.Fatalf("a discard on a failed window must record, not error: %v", err)
	}
	mustEqual(t, dp.Decision, types.PromoteDiscard, "discard decision")
	if fx2.skills.candidates != 0 {
		t.Fatalf("a failed verify must not produce a skill candidate")
	}

	// (d) auto-after-verify outside `full` is denied by policy (016).
	fx3 := newFixture(t, func(c *types.FlowConfig) { c.Hotfix.Promote = types.FlowPromoteAuto })
	fx3.flow.SetGates(types.AutonomyGates{Mode: types.AutoAssisted, AllowSpawn: true})
	sp3, err := fx3.flow.Spawn(ctx, fx3.spawnReq(""))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if _, err := fx3.flow.Promote(ctx, sp3.ID, "promote"); err == nil || !containsCode(err, types.CodeFlow016) {
		t.Fatalf("err = %v, want TROUBLE-FLOW-016", err)
	}
	// In `full` the same config auto-promotes (AC-26).
	fx4 := newFixture(t, func(c *types.FlowConfig) { c.Hotfix.Promote = types.FlowPromoteAuto })
	sp4, err := fx4.flow.Spawn(ctx, fx4.spawnReq(""))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	p4, err := fx4.flow.Promote(ctx, sp4.ID, "promote")
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	mustEqual(t, p4.Decision, types.PromoteDone, "auto promotion in full")
	mustEqual(t, fx4.skills.candidates, 1, "auto skill candidate")
}

func TestRollbackRefusalIsNotACleanRollback(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t, nil)
	// A spawn that was never leased has nothing to revert: TROUBLE-FLOW-017.
	sp := types.SpawnRequest{ID: "sp_01J9Z6Q0M2X4T8V1K7B3N5R8WP", Sig: "sig:x", Inc: "inc_x", TaskID: "tsk_x", Repo: fx.repo}
	fx.flow.rememberSpawn(sp)
	if _, err := fx.flow.Rollback(ctx, sp.ID, "verify_failed"); err == nil || !containsCode(err, types.CodeFlow017) {
		t.Fatalf("err = %v, want TROUBLE-FLOW-017", err)
	}
	// A leased spawn rolls back with a record and a refusal.
	leased, err := fx.flow.Spawn(ctx, fx.spawnReq(""))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	out, err := fx.flow.Rollback(ctx, leased.ID, "verify_failed")
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	mustEqual(t, out.State, types.SpawnDiscarded, "rollback state")
	mustEqual(t, fx.skills.refusals, 1, "skill refusals")
	if _, ok := fx.flow.Lease(leased.Sig); ok {
		t.Fatalf("a rollback must release the sig lease")
	}
}

func TestTimelineIsOrderedAndComplete(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t, func(c *types.FlowConfig) { c.Hotfix.Promote = types.FlowPromoteAuto })
	sp, err := fx.flow.Spawn(ctx, fx.spawnReq(""))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if _, err := fx.flow.Promote(ctx, sp.ID, "promote"); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	steps, err := fx.flow.Timeline(ctx, sp.Inc)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	want := []string{"filed", "foreman", "patch", "verify", "promote"}
	if len(steps) != len(want) {
		t.Fatalf("steps = %d, want %d (%+v)", len(steps), len(want), steps)
	}
	for i, s := range steps {
		if s.Stage != want[i] {
			t.Fatalf("step %d = %q, want %q", i, s.Stage, want[i])
		}
		if s.TaskID == "" && s.SpawnID == "" {
			t.Fatalf("step %d names neither a task nor a spawn", i)
		}
	}
	// The verify step carries the tuple and the promote step carries the PR url.
	if steps[3].State != "done" {
		t.Fatalf("verify step = %+v", steps[3])
	}
	if steps[4].PRURL == "" {
		t.Fatalf("the promote step must carry the PR link: %+v", steps[4])
	}
}

// TestAC21BudgetIsRecordedPerSpawn: the 60s budget is measured, not asserted.
func TestAC21BudgetIsRecordedPerSpawn(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t, nil)
	// A spawn whose trigger→spawn path takes longer than the budget.
	req := fx.spawnReq("")
	req.RequestedTS = types.FormatUTC(fx.clock.Now().Add(-90 * time.Second))
	got, err := fx.flow.Spawn(ctx, req)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	mustEqual(t, got.State, types.SpawnLeased, "state")
	p := fx.rec.last(types.KSpawn)
	if p["budget_exceeded"] != true {
		t.Fatalf("budget_exceeded = %v, want true for a 90s path", p["budget_exceeded"])
	}
	if ms := asMS(p["trig_to_spawn_ms"]); ms <= int64(budgetMS) {
		t.Fatalf("trig_to_spawn_ms = %v, want > %d", p["trig_to_spawn_ms"], budgetMS)
	}
	// The over-budget path is loud: a flow record with stage=budget.
	found := false
	for _, d := range fx.rec.ofKind(types.KFlow) {
		if d.Payload["stage"] == "budget" {
			found = true
		}
	}
	if !found {
		t.Fatalf("an over-budget spawn must write a flow record with stage=budget")
	}
}

// TestReconcileAdoptsAndPrunesWithoutDeleting.
func TestReconcileAdoptsAndPrunesWithoutDeleting(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t, nil)
	// A task row with no event must get exactly one re-appended event.
	if _, err := fx.flow.File(ctx, fx.incident(), fx.row("")); err != nil {
		t.Fatalf("File: %v", err)
	}
	if err := os.Remove(filepath.Join(fx.board, eventsFile)); err != nil {
		t.Fatal(err)
	}
	if err := fx.flow.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if err := fx.flow.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile (2): %v", err)
	}
	_, events, _ := fx.readBoard()
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1 (the re-append is idempotent)", len(events))
	}
	// A leased spawn whose directory is gone is prunable and never deleted by
	// trouble: the record says the owner must reap it.
	sp, err := fx.flow.Spawn(ctx, fx.spawnReq(""))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := os.RemoveAll(sp.Worktree); err != nil {
		t.Fatal(err)
	}
	fx.spawn.present = false
	if err := fx.flow.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	p := fx.rec.last(types.KSpawn)
	mustEqual(t, p["stage"], "reap", "reconcile stage")
	mustEqual(t, p["reason"], "worktree_missing", "reconcile reason")
	// The queue is adopted on boot: a pending spawn keeps its place and its
	// next_try_ts.
	fx2 := newFixture(t, nil)
	fx2.spawn.err = errors.New("down")
	if _, err := fx2.flow.Spawn(ctx, fx2.spawnReq("")); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := fx2.flow.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	seen := false
	for _, d := range fx2.rec.ofKind(types.KSpawn) {
		if d.Payload["reason"] == "queue_adopted" {
			seen = true
			if d.Payload["next_try_ts"] == nil {
				t.Fatalf("an adopted queue entry must carry next_try_ts: %v", d.Payload)
			}
		}
	}
	if !seen {
		t.Fatalf("Reconcile must adopt the durable queue")
	}
}

// containsCode reports whether an error names a TROUBLE code. A typed
// *flowError is the contract; the string form is accepted because a wiring-layer
// adapter may wrap one.
func containsCode(err error, code types.ErrorCode) bool {
	if err == nil {
		return false
	}
	var fe *flowError
	if errors.As(err, &fe) {
		return fe.Code == code
	}
	return strings.Contains(err.Error(), string(code))
}

// asMS coerces a payload's numeric field to int64 whatever width it landed in.
func asMS(v any) int64 {
	switch t := v.(type) {
	case int:
		return int64(t)
	case int64:
		return t
	case float64:
		return int64(t)
	}
	return -1
}

// TestSpawnRecordKeysArePinned guards the §3.13 payload contract.
func TestSpawnRecordKeysArePinned(t *testing.T) {
	fx := newFixture(t, nil)
	if _, err := fx.flow.Spawn(context.Background(), fx.spawnReq("")); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	p := fx.rec.last(types.KSpawn)
	for _, k := range []string{
		"stage", "state", "spawn_id", "task_id", "repo", "worktree", "priority_class",
		"router_ref", "attempts", "trig_to_spawn_ms", "budget_ms", "budget_exceeded",
	} {
		if _, ok := p[k]; !ok {
			t.Fatalf("spawn payload is missing the pinned key %q: %v", k, p)
		}
	}
	raw, _ := json.Marshal(p)
	if len(raw) == 0 {
		t.Fatalf("spawn payload is not serializable")
	}
}
