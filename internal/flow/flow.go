package flow

// flow.go — the flow subsystem (SPEC-08 §2, §3).
//
// internal/flow turns a finding into work in a work system, and — for a
// confirmed code bug on an enabled host — into a running foreman inside an
// isolated git worktree. It owns the `flow` and `spawn` record kinds
// (SPEC-INDEX §3.4) and the error range TROUBLE-FLOW-001..019.
//
// Design constraint (non-negotiable #3): every flow action is a registry tool
// call (`flow.*`, SPEC-06 §6) — schema'd, dry-run-able, audited. This package
// never runs git, never pushes, never merges, never writes a row it did not
// create, and opens a process only for the configured board validator.

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// Deps is the wiring layer's injection of every collaborator (§4). NewFlow takes
// only the config and the gates — the spec's §2 signature — and the composition
// root binds the rest here, so nothing in this package invents a socket, a
// subprocess or a scheduler of its own.
//
// Three of these are additions the §4 wiring table implies but does not name
// (recorded in the run report): Evidence and Severity (SPEC-05 owns the tuple
// and the incident's severity; the flow stage consumes them), PRURL (the repo's
// own PR machinery produces it) and Scrub (SPEC-02 scrubs every brief and
// comment body before it is written).
type Deps struct {
	Recorder recordSink     // SPEC-01 ledger (flow + spawn records)
	Issues   issueDesk      // SPEC-09 issue desk
	Spawn    spawnRequester // the scheduler's admission path (router_spawn)
	Skills   skillSink      // SPEC-11 candidate / refusal
	Spool    spoolSink      // SPEC-12 durable queue
	Clock    flowClock      // monotonic-capable clock
	Scrub    func(types.ScrubTarget, []byte) []byte
	DiskFree func(path string) (int64, error)

	// Evidence, Severity, PRURL and RuleHotfix are the SPEC-05 / repo-owner
	// seams: SPEC-05 owns the evidence tuple, the incident's severity and the
	// rule's hot-fix flag; the repo owner produces the PR url.
	Evidence   func(spawnID string) types.Evidence
	Severity   func(inc string) types.Severity
	PRURL      func(sp types.SpawnRequest) string
	RuleHotfix func(inc string) bool

	Actors ActorInfo
}

// ActorInfo names the daemon on every record it writes.
type ActorInfo struct {
	HostID  string
	Version string
	GitSHA  string
	BuildTS string
	ID      string
}

// flowClock is the clock seam: wall for stamps, monotonic for the 60s budget.
type flowClock interface {
	Now() time.Time
	Monotonic() time.Duration
}

// Flow is the flow subsystem.
type Flow struct {
	cfg    types.FlowConfig
	deps   Deps
	gates  types.AutonomyGates
	bounds SpoolBounds

	mu       sync.Mutex
	boards   map[string]*boardIndex
	leases   map[string]types.HotfixLease
	spawnSeq map[string]types.SpawnRequest
	bySig    map[string]types.SpawnRequest
	queue    []types.SpawnRequest
	inflight map[string]bool

	// spool is the flow-owned durable dispatch queue (§3.9a): non-nil only when
	// the composition root wired a store this subsystem can REPLAY. A sink that
	// only implements Enqueue is not durability, and spoolPut says so.
	spool         spoolQueue
	spoolInflight map[string]bool

	probeTS time.Time
	repoMu  map[string]chan struct{}
	closed  bool
}

// NewFlow validates the config and returns the subsystem. It performs no I/O:
// Start owns the boot reconcile and the registration probes (§2).
func NewFlow(cfg types.FlowConfig, autonomy types.AutonomyGates) (*Flow, error) {
	c := applyFlowDefaults(cfg)
	if err := validateFlowConfig(c); err != nil {
		return nil, err
	}
	return NewFlowWithBounds(c, autonomy, SpoolBounds{})
}

// NewFlowWithBounds is NewFlow plus the composition root's §3.9a bound set, so
// the queue's limits are wiring rather than a compile-time constant.
func NewFlowWithBounds(cfg types.FlowConfig, autonomy types.AutonomyGates, b SpoolBounds) (*Flow, error) {
	c := applyFlowDefaults(cfg)
	if err := validateFlowConfig(c); err != nil {
		return nil, err
	}
	return &Flow{
		cfg: c, gates: autonomy, bounds: b.WithDefaults(),
		boards: map[string]*boardIndex{}, leases: map[string]types.HotfixLease{},
		spawnSeq: map[string]types.SpawnRequest{}, bySig: map[string]types.SpawnRequest{},
		inflight: map[string]bool{}, spoolInflight: map[string]bool{},
		repoMu: map[string]chan struct{}{},
	}, nil
}

// SetDeps binds the collaborators (the composition root). A sink that can be
// replayed is adopted as the flow's own queue; anything else leaves the flow with
// no durable queue, which every dispatch record then states truthfully.
func (f *Flow) SetDeps(d Deps) {
	f.deps = d
	f.mu.Lock()
	f.spool = nil
	if q, ok := d.Spool.(spoolQueue); ok {
		f.spool = q
	}
	f.mu.Unlock()
}

// SpoolWired reports whether the flow owns a replayable durable queue (§3.9a).
func (f *Flow) SpoolWired() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.spool != nil
}

// Config returns the resolved config.
func (f *Flow) Config() types.FlowConfig { return f.cfg }

// Gates returns the autonomy gates this instance reads.
func (f *Flow) Gates() types.AutonomyGates {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gates
}

// SetGates updates the autonomy gates (the dashboard's autonomy route).
func (f *Flow) SetGates(g types.AutonomyGates) {
	f.mu.Lock()
	f.gates = g
	f.mu.Unlock()
}

// clock returns the injected clock, defaulting to the system clock so a
// half-wired instance still stamps correctly.
func (f *Flow) clock() flowClock {
	if f.deps.Clock != nil {
		return f.deps.Clock
	}
	return systemClock{}
}

type systemClock struct{}

func (systemClock) Now() time.Time           { return time.Now() }
func (systemClock) Monotonic() time.Duration { return time.Duration(time.Now().UnixNano()) }

// Start runs the boot reconcile and the registration probes (§3.5, §3.8).
func (f *Flow) Start(ctx context.Context) error {
	if err := f.Reconcile(ctx); err != nil {
		return err
	}
	return f.Probe(ctx)
}

// Stop parks every in-flight spawn at its state boundary and releases leases:
// nothing is left ambiguous.
func (f *Flow) Stop(ctx context.Context) error {
	f.mu.Lock()
	f.closed = true
	queue := append([]types.SpawnRequest(nil), f.queue...)
	leases := make([]types.HotfixLease, 0, len(f.leases))
	for _, l := range f.leases {
		leases = append(leases, l)
	}
	f.mu.Unlock()
	for _, sp := range queue {
		f.recordSpawn(ctx, sp, "reap", sp.State, map[string]any{"reason": "shutdown"})
	}
	for _, l := range leases {
		f.releaseLease(l.Sig, "shutdown")
	}
	return nil
}

// File writes one board row for an incident (AC-9). It is the module behind
// `flow.create_task`: the six-stage contract wraps it, and the gate chain, dedup
// and validation are this method's job.
func (f *Flow) File(ctx context.Context, inc types.Incident, row types.BoardRow) (types.FileTaskResult, error) {
	start := f.clock().Now()
	if f.cfg.Driver == types.FlowDriverNone {
		f.record(ctx, inc, map[string]any{
			"stage": "gate", "decision": "failed", "driver": f.cfg.Driver,
			"reason": "driver_none", "error_code": string(types.CodeFlow001),
			"task_id": row.ID, "latency_ms": sinceMS(start, f.clock().Now()),
		})
		return types.FileTaskResult{TaskID: row.ID}, fmt.Errorf("%s: flow driver is disabled by config", types.CodeFlow001)
	}

	// One row per (sig, board): a lookup hit is the comment path, never a second
	// row (AC-22, §3.4).
	if existing, ok := f.sigRow(row.Sig); ok {
		if _, err := f.Comment(ctx, existing, f.recurrenceBody(inc, row)); err != nil {
			f.record(ctx, inc, map[string]any{
				"stage": "comment", "decision": "failed", "task_id": existing.ID,
				"reason": "cross_ref_conflict", "error_code": string(types.CodeFlow019),
				"latency_ms": sinceMS(start, f.clock().Now()),
			})
			return types.FileTaskResult{TaskID: existing.ID, DupOf: existing.ID}, err
		}
		f.record(ctx, inc, map[string]any{
			"stage": "comment", "decision": "commented", "task_id": existing.ID,
			"dup_of": existing.ID, "issue_refs": row.IssueRefs,
			"latency_ms": sinceMS(start, f.clock().Now()),
		})
		return types.FileTaskResult{Wrote: false, TaskID: existing.ID, EventID: "", DupOf: existing.ID, Valid: true}, nil
	}

	// Never file into an unregistered project: before this point no bytes are
	// written (§3.2 step 8).
	proj, ok := f.projectFor(row)
	if !ok {
		f.record(ctx, inc, map[string]any{
			"stage": "gate", "decision": "failed", "task_id": row.ID,
			"reason": "project_unknown", "error_code": string(types.CodeFlow018),
		})
		return types.FileTaskResult{TaskID: row.ID}, fmt.Errorf("%s: no configured project for repo %q", types.CodeFlow018, row.Repo)
	}
	if !f.registrationHolds(proj) {
		f.record(ctx, inc, map[string]any{
			"stage": "gate", "decision": "failed", "project": proj.Name,
			"board_path": proj.BoardPath, "reason": proj.Reason,
			"error_code": string(types.CodeFlow018), "task_id": row.ID,
		})
		return types.FileTaskResult{TaskID: row.ID}, fmt.Errorf("%s: project %s is not registered (%s)", types.CodeFlow018, proj.Name, proj.Reason)
	}

	mode := f.reviewMode(proj, inc)
	if mode == types.FlowReviewNever {
		f.record(ctx, inc, map[string]any{
			"stage": "file", "decision": "skipped", "reason": "review_mode_never",
			"project": proj.Name, "board_path": proj.BoardPath, "task_id": row.ID,
			"review_mode": mode, "latency_ms": sinceMS(start, f.clock().Now()),
		})
		return types.FileTaskResult{Wrote: false, TaskID: row.ID, Valid: true}, nil
	}

	ix, err := f.indexFor(proj.BoardPath)
	if err != nil {
		f.record(ctx, inc, map[string]any{
			"stage": "file", "decision": "failed", "project": proj.Name,
			"board_path": proj.BoardPath, "reason": "board_index_failed",
			"error_code": string(types.CodeFlow002), "message": err.Error(),
		})
		return types.FileTaskResult{TaskID: row.ID}, fmt.Errorf("%s: %w", types.CodeFlow002, err)
	}
	if row.ID == "" {
		row.ID = f.allocTaskID(ix)
	}
	row.Status = rowStatus(mode)
	row.DependsOn = defaultSlice(row.DependsOn)
	row.Blocks = defaultSlice(row.Blocks)
	row.IssueRefs = defaultSlice(row.IssueRefs)
	ev := types.BoardEvent{
		Type: types.EvTaskCreated, TaskID: row.ID, TS: types.FormatUTC(f.clock().Now()),
		Actor: f.actorName(), Detail: map[string]any{"sig": row.Sig, "inc": row.Inc},
	}
	driver := f.driverFor(mode, inc)
	req := types.FileTaskRequest{
		Project: proj.Name, BoardPath: proj.BoardPath, Row: row, Event: ev,
		ReviewMode: mode, IdemKey: row.ID, DryRun: false,
	}
	res, findings, err := f.writeViaDriver(ctx, driver, req, proj, ix)
	res.LatencyMS = sinceMS(start, f.clock().Now())
	f.record(ctx, inc, map[string]any{
		"stage": "file", "decision": decisionOf(err, res), "driver": driver.Name(),
		"review_mode": mode, "project": proj.Name, "board_path": proj.BoardPath,
		"task_id": res.TaskID, "event_id": res.EventID, "row_status": row.Status,
		"priority": row.Priority, "complexity": row.Complexity, "repo": row.Repo,
		"issue_refs": row.IssueRefs, "dup_of": res.DupOf, "idem_key": row.ID,
		"validate_cmd": f.cfg.ValidateCmd, "validate_rc": res.ValidateRC,
		"validate_findings": findings,
		"latency_ms":        res.LatencyMS, "error_code": codeOfErr(err),
	})
	return res, err
}

// Comment appends a comment event to an existing row (§3.4). It never rewrites
// the row: the two-file contract's second file carries the recurrence.
func (f *Flow) Comment(ctx context.Context, row types.BoardRow, body string) (types.BoardRow, error) {
	proj, ok := f.projectFor(row)
	if !ok {
		return row, fmt.Errorf("%s: no configured project for repo %q", types.CodeFlow019, row.Repo)
	}
	ix, err := f.indexFor(proj.BoardPath)
	if err != nil {
		return row, fmt.Errorf("%s: %w", types.CodeFlow019, err)
	}
	if row.ID == "" {
		if id, ok := ix.bySig[row.Sig]; ok {
			row.ID = id
		}
	}
	body = f.scrub(types.TgBoard, body)
	ev := types.BoardEvent{
		Type: types.EvTaskComment, TaskID: row.ID, TS: types.FormatUTC(f.clock().Now()),
		Actor: f.actorName(),
		Detail: map[string]any{
			"inc": row.Inc, "sig": row.Sig, "body": body,
			"issue_refs": row.IssueRefs, "recurrence": true,
			"dedup": briefSHA256(body + "|" + row.Inc),
		},
	}
	if _, err := ix.appendEventAlways(ctx, ev, f.cfg.ValidateCmd); err != nil {
		return row, err
	}
	return row, nil
}

// Spawn requests a foreman spawn through the scheduler's admission path (§3.9).
// A spawn failure is never silent: the failing path is exactly the path the
// fleet walks when it is sick.
func (f *Flow) Spawn(ctx context.Context, req types.SpawnRequest) (types.SpawnRequest, error) {
	if f.cfg.Driver == types.FlowDriverNone {
		return req, fmt.Errorf("%s: flow driver is disabled by config", types.CodeFlow001)
	}
	inc := types.Incident{ID: req.Inc, Sig: req.Sig, Severity: f.severityFor(req.Inc), EntryRung: types.RungAgent}
	if !f.cfg.Hotfix.Enabled {
		// The row still stands; only the spawn is skipped (TROUBLE-FLOW-006).
		f.recordSpawn(ctx, req, "gate", types.SpawnFailed, map[string]any{
			"reason": "hotfix_disabled", "error_code": string(types.CodeFlow006),
		})
		f.record(ctx, inc, map[string]any{
			"stage": "gate", "decision": "skipped", "reason": "hotfix_disabled",
			"error_code": string(types.CodeFlow006), "task_id": req.TaskID, "repo": req.Repo,
		})
		return req, nil
	}
	if code, reason := f.hotfixGate(ctx, inc, req); code != "" {
		f.recordSpawn(ctx, req, "gate", types.SpawnFailed, map[string]any{
			"reason": reason, "error_code": string(code),
		})
		return req, fmt.Errorf("%s: %s", code, reason)
	}
	if !f.gates.AllowSpawn {
		// Shadow / assisted without a grant: the row stands, the spawn does not
		// (SPEC-05's gate matrix; TROUBLE-LADDER-011 is the ladder's record).
		f.recordSpawn(ctx, req, "gate", types.SpawnRequested, map[string]any{
			"reason": "autonomy_shadow", "error_code": string(types.CodeLadder011),
		})
		f.record(ctx, inc, map[string]any{
			"stage": "gate", "decision": "drafted", "reason": "autonomy_shadow",
			"task_id": req.TaskID, "error_code": string(types.CodeLadder011),
		})
		return req, nil
	}
	if req.ID == "" {
		req.ID = types.NewID(types.PSpawn)
	}
	if req.RequestedTS == "" {
		req.RequestedTS = types.FormatUTC(f.clock().Now())
	}
	if req.PriorityClass == "" {
		req.PriorityClass = f.cfg.Hotfix.PriorityClass
	}
	req.State = types.SpawnRequested
	f.rememberSpawn(req)
	// `requested` is written BEFORE the router call.
	f.recordSpawn(ctx, req, "request", types.SpawnRequested, map[string]any{
		"attempts": req.Attempts, "priority_class": req.PriorityClass, "repo": req.Repo,
	})

	lease, err := f.grantLease(inc, req)
	if err != nil {
		f.recordSpawn(ctx, req, "lease", types.SpawnFailed, map[string]any{
			"reason": "lease_held", "error_code": string(types.CodeFlow009),
		})
		return req, err
	}
	// Serialize worktree creation per repo: concurrent `git worktree add` calls
	// contend on the shared config lock (§3.8).
	unlock, err := f.lockRepo(ctx, req.Repo)
	if err != nil {
		f.recordSpawn(ctx, req, "lease", types.SpawnPending, map[string]any{
			"reason": "mutex_timeout", "error_code": string(types.CodeFlow013),
		})
		f.releaseLease(req.Sig, "mutex_timeout")
		return req, err
	}
	if req.Brief == "" {
		req.Brief = f.briefFor(req)
	}
	ref, worktree, err := f.requestSpawn(ctx, req)
	unlock()
	req.Attempts++
	if err != nil {
		req.State = types.SpawnPending
		req.LastError = err.Error()
		f.enqueue(ctx, req)
		f.recordSpawn(ctx, req, "spawn", types.SpawnPending, map[string]any{
			"reason": "router_spawn_failed", "error_code": string(types.CodeFlow010),
			"attempts": req.Attempts, "trig_to_spawn_ms": trigMS(req, f.clock().Now()),
			"budget_ms": budgetMS, "mutex_wait_ms": 0,
		})
		return req, nil
	}
	req.RouterRef = ref
	req.Worktree = worktree
	f.mu.Lock()
	lease.Holder = req.Inc
	lease.ExpiresTS = types.FormatUTC(f.clock().Now().Add(f.cfg.Hotfix.LeaseTTL.Std()))
	f.leases[req.Sig] = lease
	f.mu.Unlock()
	if worktree == "" {
		// Acknowledged, but no worktree inside spawn_worktree_timeout (§6.20).
		req.State = types.SpawnPending
		req.LastError = "worktree_timeout"
		f.enqueue(ctx, req)
		f.recordSpawn(ctx, req, "spawn", types.SpawnPending, map[string]any{
			"reason": "worktree_timeout", "error_code": string(types.CodeFlow011),
			"attempts": req.Attempts,
		})
		return req, nil
	}
	mode := types.SpawnWorktreeMode
	if f.isExempt(req.Repo) {
		mode = types.SpawnModeSerial
	}
	req.State = types.SpawnLeased
	f.rememberSpawn(req)
	trig := trigMS(req, f.clock().Now())
	f.recordSpawn(ctx, req, "spawn", types.SpawnLeased, map[string]any{
		"worktree": worktree, "worktree_mode": mode, "router_ref": ref,
		"attempts": req.Attempts, "lease_id": leaseIDOf(lease),
		"lease_expires_ts": lease.ExpiresTS, "trig_to_spawn_ms": trig,
		"budget_ms": budgetMS, "budget_exceeded": trig > budgetMS,
		"brief_sha256": briefSHA256(req.Brief),
	})
	if trig > budgetMS {
		// AC-21 is measured per spawn, not asserted once: a miss is loud.
		f.record(ctx, inc, map[string]any{
			"stage": "budget", "decision": "failed", "reason": "budget_exceeded",
			"task_id": req.TaskID, "trig_to_spawn_ms": trig, "budget_exceeded": true,
		})
	}
	return req, nil
}

// SpawnState returns a spawn by id.
func (f *Flow) SpawnState(spawnID string) (types.SpawnRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sp, ok := f.spawnSeq[spawnID]
	if !ok {
		return types.SpawnRequest{}, fmt.Errorf("%s: unknown spawn id %q", types.CodeFlow010, spawnID)
	}
	return sp, nil
}

// QueueDepth is the durable queue's length (the dashboard budget panel's first
// number, SPEC-10 §2.1 row 18).
func (f *Flow) QueueDepth() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, sp := range f.queue {
		if sp.State == types.SpawnPending {
			n++
		}
	}
	return n
}
