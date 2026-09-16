package flow

// helpers_test.go — the shared fixture: a recording ledger, a scriptable
// scheduler (the admission path), an issue desk, a skill sink, a spool and a
// temp board + repo. No test reaches the network except through an httptest
// server, which is what SPEC-08 §7 requires.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// fakeClock is an adjustable clock: the monotonic side moves with the wall side.
type fakeClock struct {
	mu   sync.Mutex
	base time.Time
	off  time.Duration
}

func newFakeClock() *fakeClock { return &fakeClock{base: time.Now()} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.base.Add(c.off)
}

func (c *fakeClock) Monotonic() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.off
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.off += d
	c.mu.Unlock()
}

// fakeRecorder is a recording ledger.
type fakeRecorder struct {
	mu     sync.Mutex
	drafts []types.RecordDraft
}

func (r *fakeRecorder) Append(_ context.Context, d types.RecordDraft) (types.Record, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drafts = append(r.drafts, d)
	return types.Record{Seq: uint64(len(r.drafts)), Kind: d.Kind, Sig: d.Sig, Inc: d.Inc, Payload: d.Payload}, nil
}

func (r *fakeRecorder) ofKind(k types.RecordKind) []types.RecordDraft {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []types.RecordDraft
	for _, d := range r.drafts {
		if d.Kind == k {
			out = append(out, d)
		}
	}
	return out
}

func (r *fakeRecorder) last(k types.RecordKind) map[string]any {
	all := r.ofKind(k)
	if len(all) == 0 {
		return nil
	}
	return all[len(all)-1].Payload
}

// fakeSpawn is the scheduler's admission path.
type fakeSpawn struct {
	mu        sync.Mutex
	calls     int
	ref       string
	worktree  string
	err       error
	present   bool
	lastReq   types.SpawnRequest
	lastBrief string
}

func (s *fakeSpawn) RequestSpawn(_ context.Context, req types.SpawnRequest) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.lastReq = req
	s.lastBrief = req.Brief
	if s.err != nil {
		return "", "", s.err
	}
	ref := s.ref
	if ref == "" {
		ref = "spawn-8f21c0"
	}
	wt := s.worktree
	if wt == "" {
		wt = filepath.Join(req.Repo, ".worktrees", req.TaskID)
	}
	return ref, wt, nil
}

func (s *fakeSpawn) WorktreePresent(_ context.Context, repo, taskID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.present, nil
}

// fakeSkills records candidate/refusal hand-offs.
type fakeSkills struct {
	mu         sync.Mutex
	candidates int
	refusals   int
}

func (s *fakeSkills) Candidate(context.Context, types.Incident, types.Evidence, string, string, string) error {
	s.mu.Lock()
	s.candidates++
	s.mu.Unlock()
	return nil
}

func (s *fakeSkills) Refusal(context.Context, types.Incident, types.Evidence, string) error {
	s.mu.Lock()
	s.refusals++
	s.mu.Unlock()
	return nil
}

// fakeSpool records durable-queue writes.
type fakeSpool struct {
	mu      sync.Mutex
	entries []types.SpoolEntry
}

func (s *fakeSpool) Enqueue(_ context.Context, e types.SpoolEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, e)
	return nil
}

func (s *fakeSpool) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// fixture bundles everything a flow test needs.
type fixture struct {
	t        *testing.T
	flow     *Flow
	rec      *fakeRecorder
	spawn    *fakeSpawn
	skills   *fakeSkills
	spool    *fakeSpool
	clock    *fakeClock
	repo     string
	board    string
	sched    *httptest.Server
	evidence types.Evidence
	severity types.Severity
	hotfix   bool

	lastTaskLines []string
}

// newFixture builds a wired Flow over a temp repo and board, with a scheduler
// that reports the project as registered and enabled.
func newFixture(t *testing.T, mut func(*types.FlowConfig)) *fixture {
	t.Helper()
	repo := t.TempDir()
	board := filepath.Join(repo, ".board")
	if err := os.MkdirAll(board, 0o755); err != nil {
		t.Fatal(err)
	}
	f := &fixture{
		t: t, rec: &fakeRecorder{}, spawn: &fakeSpawn{present: true},
		skills: &fakeSkills{}, spool: &fakeSpool{}, clock: newFakeClock(),
		repo: repo, board: board,
		evidence: types.Evidence{Result: types.VerifyPassed, WindowS: 600, TSWindowEnd: types.NowUTC()},
		severity: types.SevHigh, hotfix: true,
	}
	cfg := types.FlowConfig{
		Driver: types.FlowDriverBoard, ReviewMode: types.FlowReviewAuto,
		BoardPath: board, IDPrefix: string(types.PTsk),
		RegistrationProbeEvery: "5m", RegistrationStaleMax: "1h",
		Projects: map[string]types.FlowProject{
			"payment-api": {
				Name: "payment-api", Repo: repo, BoardPath: board,
				Enabled: true, Hotfix: true, Scheduler: "payment-api",
				Registered: true, Ticked: true, LastProbeTS: types.NowUTC(),
			},
		},
		Hotfix: types.HotfixConfig{
			Enabled: true, AllowedRepos: []string{repo}, ForemanSpawn: types.FlowSpawnRouter,
			PriorityClass: "hotfix", VerifyWindow: "10m", Promote: types.FlowPromoteHuman,
			MaxConcurrent: 2, MinFreeDiskGB: 10, WorktreeBase: ".worktrees",
			LeaseTTL: "30m", SpawnAckTimeout: "5s", SpawnWorktreeTimeout: "30s",
			MaxAttempts: 5, MinSeverity: types.SevHigh, MutexWait: "5s",
			CapabilityTags: []string{"hotfix", "trouble"},
		},
	}
	if mut != nil {
		mut(&cfg)
	}
	fl, err := NewFlow(cfg, types.AutonomyGates{
		Mode: types.AutoFull, AllowDetect: true, AllowResearch: true, AllowPlayMutate: true,
		AllowAgent: true, AllowSpawn: true, AllowMerge: true, AllowPromote: true,
		AllowSkillAccept: true,
	})
	if err != nil {
		t.Fatalf("NewFlow: %v", err)
	}
	fl.SetDeps(Deps{
		Recorder: f.rec, Spawn: f.spawn, Skills: f.skills, Spool: f.spool, Clock: f.clock,
		Evidence:   func(string) types.Evidence { return f.evidence },
		Severity:   func(string) types.Severity { return f.severity },
		RuleHotfix: func(string) bool { return f.hotfix },
		DiskFree:   func(string) (int64, error) { return 500, nil },
		PRURL:      func(types.SpawnRequest) string { return "https://example.invalid/pr/42" },
		Actors:     ActorInfo{HostID: "7f3a91c2d4e5b607", Version: "0.1.0", GitSHA: "9c1f0ab"},
	})
	f.flow = fl
	return f
}

// incident is the incident a filing belongs to.
func (f *fixture) incident() types.Incident {
	return types.Incident{
		ID: "inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE", Sig: "sentinel:sha256v1:9f2c1d3e4b5a6c7d",
		Severity: f.severity, EntryRung: types.RungAgent, Rung: types.RungOutlets,
		OpenedTS: types.NowUTC(),
	}
}

// row is a correctly-shaped filing request for this fixture.
func (f *fixture) row(sig string) types.BoardRow {
	if sig == "" {
		sig = f.incident().Sig
	}
	return types.BoardRow{
		Title:    "hotfix: queue wedge in payment-worker (" + sig + ")",
		Priority: "P1", Complexity: "M", Sig: sig, Inc: f.incident().ID,
		Repo: f.repo, CapabilityTags: []string{"hotfix", "trouble"},
		Reasoning: "trouble 0.1.0 flow; sig " + sig,
	}
}

// readBoard returns the appended task rows and events.
func (f *fixture) readBoard() ([]map[string]any, []map[string]any, []string) {
	tasks, err := os.ReadFile(filepath.Join(f.board, tasksFile))
	if err != nil && !os.IsNotExist(err) {
		f.t.Fatalf("read tasks: %v", err)
	}
	evs, _ := os.ReadFile(filepath.Join(f.board, eventsFile))
	parse := func(b []byte) ([]map[string]any, []string) {
		var out []map[string]any
		var lines []string
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			lines = append(lines, line)
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				continue
			}
			out = append(out, m)
		}
		return out, lines
	}
	rows, taskLines := parse(tasks)
	events, _ := parse(evs)
	f.lastTaskLines = taskLines
	return rows, events, taskLines
}

// schedulerStub serves the registration-proof response.
func schedulerStub(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(srv.Close)
	return srv
}

// projectList renders a /projects response for one project.
func projectList(name, board string, enabled bool) string {
	b, _ := json.Marshal([]map[string]any{{
		"name": name, "enabled": enabled, "board_path": board, "ticked": true,
	}})
	return string(b)
}

// mustEqual fails with a formatted message.
func mustEqual(t *testing.T, got, want any, what string) {
	t.Helper()
	if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", want) {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

// Project mutators: the fixture's project rows are values, so a test that wants a
// different proof state rebuilds the row.
func setRegistered(p types.FlowProject, reg bool, reason string) types.FlowProject {
	p.Registered, p.Reason = reg, reason
	if reason != "" {
		p.Ticked = false
	}
	return p
}

func setEnabled(p types.FlowProject, on bool) types.FlowProject { p.Enabled = on; return p }

func setBoard(p types.FlowProject, board string) types.FlowProject { p.BoardPath = board; return p }

func setProbeTS(p types.FlowProject, ts string) types.FlowProject { p.LastProbeTS = ts; return p }

func setSeverityNeutral(p types.FlowProject) types.FlowProject { return p }

// nowMinus turns a duration backward from the wall clock into a stamp.
func nowMinus(d time.Duration) time.Time { return time.Now().Add(-d) }
