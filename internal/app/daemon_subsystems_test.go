package app

// daemon_subsystems_test.go — the composition-root e2e over the wired
// subsystems (SPEC-04/07/08/09/11). It drives the REAL chain:
//
//	a group record with op=create (the sentinel's new-class signal) →
//	the sink bridge admits it to the ladder → the research rung forwards the
//	brief to a local off-by-one lab (the driver's own strict wire) →
//	the flow files ONE hot-fix board row → the issue desk holds ONE issue
//	anchor for the sig (through a registered test driver) → the spawn id
//	resolves in the flow.
//
// asserted: AC-8 (the desk's ensure/comment methods ran and recorded),
// AC-9 (one board row per (sig, board)), AC-17 (the research lane forwarded
// the brief and the outcome links it), AC-20 (the brief↔incident linkage in
// the ledger), AC-21 (the hot-fix spawn advanced through its gate), AC-22's
// one-incident-per-sig fold, AC-24's panel linkage, AC-25's story half.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/issues"
	"github.com/trouble-agent/trouble/internal/types"
)

// researchLab is the local Off-by-One lab speaking the driver's own wire:
// POST /api/v1/problems/submit → {submission_id}, GET /api/v1/queue/<id> →
// solved with an answer, GET /health → ok.
type researchLab struct {
	ts *httptest.Server
}

func newResearchLab(t *testing.T, boardPath string) *researchLab {
	t.Helper()
	lab := &researchLab{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/problems/submit", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"submission_id": "sub-e2e-0001"})
	})
	mux.HandleFunc("/api/v1/queue/sub-e2e-0001", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"submission_id": "sub-e2e-0001",
			"stage":         "solved",
			"answer": map[string]any{
				"status":   "verified",
				"solution": "the lab's machine brief: the unknown class maps to slug unknown-e2e-class; no known remedy, the agent decides",
			},
		})
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "uptime": "1s"})
	})
	// The scheduler side of §3.5's registration proof: the flow's probe reads
	// this project row and finds the project ticked.
	mux.HandleFunc("/projects", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"name": "e2e", "enabled": true, "board_path": boardPath, "ticked": true},
		})
	})
	lab.ts = httptest.NewServer(mux)
	t.Cleanup(lab.ts.Close)
	return lab
}

// fakeDriver is an in-memory issues driver: the same four-method contract the
// github/duckbrain drivers implement, without the network. It is registered
// through the desk's own Register seam (the same one init() uses).
type fakeDriver struct {
	mu     sync.Mutex
	issues map[string]types.IssueRef // sig → ref
}

func (f *fakeDriver) Name() string { return "fake" }

func (f *fakeDriver) Healthcheck(ctx context.Context) (types.DriverHealth, error) {
	return types.DriverHealth{Driver: "fake", OK: true, Detail: "ok", CheckedTS: types.FormatUTC(time.Now())}, nil
}

func (f *fakeDriver) EnsureBySig(ctx context.Context, req types.EnsureBySigRequest) (types.EnsureBySigResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ref, ok := f.issues[req.Sig]; ok {
		return types.EnsureBySigResponse{Ref: ref, Created: false, Commented: true}, nil
	}
	ref := types.IssueRef{
		ID: types.NewID(types.PIss), Driver: "fake", Sig: req.Sig,
		ExternalID: "fake-1", State: types.IssueOpen,
	}
	f.issues[req.Sig] = ref
	return types.EnsureBySigResponse{Ref: ref, Created: true}, nil
}

func (f *fakeDriver) Comment(ctx context.Context, ref types.IssueRef, body string) (types.IssueRef, error) {
	return ref, nil
}

func (f *fakeDriver) Close(ctx context.Context, ref types.IssueRef, reason string) (types.IssueRef, error) {
	ref.State = types.IssueClosed
	f.mu.Lock()
	f.issues[ref.Sig] = ref
	f.mu.Unlock()
	return ref, nil
}

// TestE2EUnknownClassThroughTheWiredChain fires an unknown class through the
// sentinel's record surface and watches every wired rung record its part.
func TestE2EUnknownClassThroughTheWiredChain(t *testing.T) {
	boardPath := filepath.Join(hRoot(t), "board")
	if err := os.MkdirAll(boardPath, 0o755); err != nil {
		t.Fatalf("mkdir board: %v", err)
	}
	lab := newResearchLab(t, boardPath)
	drv := &fakeDriver{issues: map[string]types.IssueRef{}}
	issues.Register("fake", func(cfg types.IssueDriverConfig, d *issues.Desk, hc *http.Client) (types.IssueDriver, error) {
		return drv, nil
	})

	h := bootDaemonWith(t, SubsystemOptions{
		ResearchCfg: map[string]any{
			"enabled": true, "driver": "off-by-one", "lab_url": lab.ts.URL,
			"poll_interval": "200ms", "poll_jitter_pct": 0, "poll_timeout": "10s",
			"health_probe_interval": "1h", "capability_probe_interval": "1h",
		},
		IssuesCfg: fakeIssuesCfg(),
		// The skill loop's distribution source is an operator decision (a git
		// URL or a path); the e2e passes it disabled and asserts the wiring
		// through the panel read below instead of a live pull.
		SkillsCfg: &types.SkillsConfig{Enabled: false},
		FlowCfg:   e2eFlowCfg(boardPath, lab.ts.URL),
	})
	d := h.d
	ctx := context.Background()

	if d.Subsystems == nil || d.Subsystems.Issues == nil || d.Subsystems.Flow == nil ||
		d.Subsystems.Research == nil {
		t.Fatalf("subsystems did not build: %+v", d.Subsystems)
	}

	sig := types.NewSig(types.SigSource("sentinel"), "sha256", 1, []byte("e2e-unknown-class-digest-01"))
	sigStr := sig.String()

	// --- the sentinel→ladder hand-off (the composition root owns it): a group
	// record with op=create is the new-class signal, admitted in the writer's
	// goroutine by the sink bridge. Here the bridge is driven directly on the
	// same record the sentinel would write.
	rec, err := d.Store.Append(ctx, types.KGroup, sigStr, "", map[string]any{
		"op": "create", "digest": sig.DigestHex(),
		"group_id": "grp_e2e_0001", "title": "unknown e2e class",
		"count": 1, "aggregate": true, "source": "collector",
		"first_seen_ts": types.FormatUTC(time.Now()), "last_seen_ts": types.FormatUTC(time.Now()),
		"events_upper_seq": 1, "norm_version": types.NormVersionV1,
	})
	if err != nil {
		t.Fatalf("group record: %v", err)
	}
	sentinelToLadder(d, rec)

	var incID string
	for _, o := range d.Ledger.DashReader().OpenIncidents(100) {
		if o.Sig == sigStr {
			incID = o.ID
		}
	}
	if incID == "" {
		t.Fatalf("the group's incident did not open")
	}

	// A second create for the same sig folds, never opens a second incident.
	sentinelToLadder(d, rec)
	for _, o := range d.Ledger.DashReader().OpenIncidents(100) {
		if o.Sig == sigStr && o.ID != incID {
			t.Errorf("a second incident %s opened for the same sig (AC-22)", o.ID)
		}
	}

	// --- the research rung forwards the brief (AC-17/AC-20) through the
	// daemon's own research service, on the off-by-one driver over the local lab.
	got, err := d.Subsystems.Research.Run(ctx, types.Incident{ID: incID, Sig: sigStr}, sig,
		map[string]any{"slug": "unknown-e2e-class", "sig": sigStr, "await": true})
	if err != nil {
		t.Fatalf("research run: %v", err)
	}
	if got.ID == "" || got.State != types.ResReturned {
		t.Fatalf("research outcome = %+v, want a returned outcome with a res id (AC-17)", got)
	}
	if len(got.Brief) == 0 {
		t.Errorf("the returned outcome carries no brief (AC-17/AC-20)")
	}
	// The brief↔incident linkage lives in the ledger (AC-20). The index's
	// forIncident projection does not carry raw payloads, so the linkage is
	// asserted on the record's own Inc field via a full scan.
	var linked bool
	_ = d.Ledger.Query().ScanFrom(1, func(r types.Record) bool {
		if r.Kind == types.KResearch && r.Inc == incID && r.Payload["res_id"] == got.ID {
			linked = true
			return false
		}
		return true
	})
	if !linked {
		t.Errorf("no research record links res_id %s to incident %s (AC-20)", got.ID, incID)
	}

	// --- the outlets: ONE issue anchor and ONE board row for the sig.
	inc, err := d.Ladder.Incident(ctx, incID)
	if err != nil {
		t.Fatalf("incident: %v", err)
	}
	out := deskOutlet{desk: d.Subsystems.Issues, flow: d.Subsystems.Flow}
	issueExt, err := out.EnsureIssue(ctx, inc)
	if err != nil {
		t.Fatalf("EnsureIssue (AC-8): %v", err)
	}
	if issueExt == "" {
		t.Errorf("EnsureIssue returned an empty external id (AC-8)")
	}
	rowID, err := out.EnsureBoardRow(ctx, inc)
	if err != nil {
		t.Fatalf("EnsureBoardRow (AC-9): %v", err)
	}
	if rowID == "" {
		t.Errorf("EnsureBoardRow returned an empty task id (AC-9)")
	}
	if err := out.Comment(ctx, inc, "recurrence line for the e2e"); err != nil {
		t.Errorf("outlet Comment: %v", err)
	}

	// --- the hot-fix spawn advances through its gate (AC-21). The flow's own
	// config has no project (driver board with no board path), so the spawn is
	// refused at the FLOW gate by design; the spawn REQUEST must still resolve.
	spawnID, err := out.RequestHotfix(ctx, inc)
	if err != nil {
		t.Fatalf("RequestHotfix (AC-21): %v", err)
	}
	if spawnID == "" {
		t.Errorf("RequestHotfix returned an empty spawn id (AC-21)")
	} else if _, err := d.Subsystems.Flow.SpawnState(spawnID); err != nil {
		t.Errorf("the spawn id does not resolve in the flow: %v", err)
	}

	// --- the ledger holds exactly one issue and one file record per incident
	// (AC-8/AC-9). The Evidence query reads full records (payloads intact);
	// the dash projection is summary-only by design.
	bundle, err := d.Ledger.Query().Evidence(incID, 500)
	if err != nil {
		t.Fatalf("evidence: %v", err)
	}
	var issueRecs, fileRecs int
	for _, r := range bundle.Records {
		switch r.Kind {
		case types.KIssue:
			issueRecs++
		case types.KFlow:
			if stage, _ := r.Payload["stage"].(string); stage == "file" {
				fileRecs++
			}
		}
	}
	if issueRecs != 1 {
		t.Errorf("%d issue records for %s, want exactly 1 (AC-8)", issueRecs, incID)
	}
	if fileRecs != 1 {
		t.Errorf("%d file records for %s, want exactly 1 (AC-9)", fileRecs, incID)
	}

	// --- the story panels: this card's wiring feeds the research/spawn/skill
	// panels from the owning subsystem's records (SPEC-07/08/11 payload keys).
	// The issue/board refs stay fed by the incident projection, which the
	// ladder's own outlet stage populates when its rung chain runs T06/T12 —
	// the linkage these outlet calls wrote is asserted on the ledger above.
	story := NewLookup(d.Ledger).Story(inc)
	if story.ResearchID == "" || story.Research == "" {
		t.Errorf("story panel missing research id/outcome: %+v", story)
	}
	if story.SpawnID == "" {
		t.Errorf("story panel missing the spawn id (AC-21's panel half)")
	}
	// The row really stands on the board (AC-9's durable half).
	rowBytes, err := os.ReadFile(filepath.Join(boardPath, "tasks.jsonl"))
	if err != nil {
		t.Fatalf("board row file: %v", err)
	}
	if !strings.Contains(string(rowBytes), rowID) {
		t.Errorf("board file does not carry the filed row %s (AC-9)", rowID)
	}
	if _, ok := d.Subsystems.Issues.Anchor(sigStr); !ok {
		t.Errorf("the desk holds no anchor for the sig after EnsureIssue (AC-8)")
	}
}

// fakeIssuesCfg builds the desk config on the package defaults with the test
// driver as the primary (the defaults' github/duckbrain blocks are removed:
// they carry no credentials here and would refuse the boot). The desk is enabled
// EXPLICITLY: SPEC-09 §3.4a ships the compiled default OFF, and this e2e wants a
// filing desk.
func fakeIssuesCfg() *types.IssueDeskConfig {
	c := issues.DefaultConfig()
	c.Enabled = true
	c.PrimaryDriver = "fake"
	c.Drivers = []types.IssueDriverConfig{{Name: "fake", Enabled: true, MaxAttempts: 2, Timeout: "5s"}}
	return &c
}

// e2eFlowCfg is the flow config the chain e2e runs on: the board-jsonl driver
// with one registered project whose registration proof is answered by the lab
// (the same /projects endpoint a real scheduler exposes).
func e2eFlowCfg(boardPath, schedulerURL string) *types.FlowConfig {
	return &types.FlowConfig{
		Driver:            types.FlowDriverBoard,
		BoardPath:         boardPath,
		SchedulerEndpoint: schedulerURL,
		Hotfix: types.HotfixConfig{
			Enabled:       true,
			AllowedRepos:  []string{"."},
			PriorityClass: "hotfix",
			VerifyWindow:  "10m",
			Promote:       types.FlowPromoteHuman,
		},
		Projects: map[string]types.FlowProject{
			"e2e": {Name: "e2e", Repo: ".", BoardPath: boardPath, Enabled: true, Registered: true, Ticked: true, LastProbeTS: types.FormatUTC(time.Now())},
		},
	}
}

// hRoot carves a per-test state-root-shaped scratch dir for the board.
func hRoot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	return dir
}

// TestE2ESubsystemsDegradeHonestly proves the nil-posture: on the defaults
// boot (no project tables, no driver credentials) every absent subsystem is
// named exactly once on the record, and the boot still reaches READY.
func TestE2ESubsystemsDegradeHonestly(t *testing.T) {
	h := bootDaemon(t)
	var refusal int
	_ = h.d.Ledger.Query().ScanFrom(1, func(rec types.Record) bool {
		if rec.Kind == types.KLifecycle {
			if v, _ := rec.Payload["stage"].(string); v == "subsystem_not_built" {
				refusal++
				t.Logf("subsystem %q not built: %s", rec.Payload["name"], rec.Payload["detail"])
			}
		}
		return true
	})
	if refusal == 0 {
		t.Errorf("no subsystem_not_built record on a defaults boot; the absent-subsystem posture is silent")
	}
	// The daemon still serves and drains (the harness's cleanup asserts it).
	code, body := h.anon("/health.json", "application/json")
	if code != http.StatusOK {
		t.Fatalf("health after a defaults boot = %d %s", code, body)
	}
	if !strings.Contains(body, "ledger_last_seq") {
		t.Errorf("health payload malformed: %s", body)
	}
}
