package ladder

import (
	"context"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// park_test.go is the SPEC-05 §7 row for §3.5 (park and resume): the SIGTERM
// drain, the re-adopter's check-only resume, the orphan path, re-adoption
// idempotency and the serial drain of a restart with many parked runs. Nothing
// here opens a socket, starts a subprocess or reads a real clock. Its incidents
// are registered through the boot seam described at the top of lease_test.go.

// resumeRecords counts the resume records the re-adopter wrote.
func resumeRecords(h *harness) int { return len(resumeOrder(h)) }

// resumeOrder returns the park ids of the resume records in the order they
// landed: the ledger's append order is the re-adopter's processing order.
func resumeOrder(h *harness) []string {
	h.ledger.mu.Lock()
	defer h.ledger.mu.Unlock()
	out := []string{}
	for _, r := range h.ledger.records {
		if r.Kind != types.KLifecycle {
			continue
		}
		if fmt.Sprint(r.Payload["kind"]) != "resume" {
			continue
		}
		id := ""
		if res, ok := r.Payload["resume"].(map[string]any); ok {
			id = fmt.Sprint(res["park_id"])
		}
		out = append(out, id)
	}
	return out
}

// parkResumeRecords counts the park and resume records in the ledger: the
// idempotency row asserts this total does not move on a second ReAdopt.
func parkResumeRecords(h *harness) int {
	h.ledger.mu.Lock()
	defer h.ledger.mu.Unlock()
	n := 0
	for _, r := range h.ledger.records {
		if r.Kind != types.KLifecycle {
			continue
		}
		switch fmt.Sprint(r.Payload["kind"]) {
		case "park", "resume":
			n++
		}
	}
	return n
}

// currentLeaseInc reads the lease table's current holder without taking its
// mutex: acquire calls the liveness probe from inside the table's critical
// section, so a probe that locked would deadlock, and the drain a test drives is
// single-goroutine.
func currentLeaseInc(l *Ladder) string {
	if l.lease.current == nil {
		return ""
	}
	return l.lease.current.Inc
}

// orphanMarkerJSON is the marker file's wire shape (§3.5 step 4), declared here
// with its literal JSON keys so a tag drift in park.go fails the test instead of
// matching a shared struct.
type orphanMarkerJSON struct {
	Inc        string `json:"inc"`
	Sig        string `json:"sig"`
	OrphanedTS string `json:"orphaned_ts"`
}

// TestParkSIGTERMWritesOneRecordPerInflightRun is the §7 park row: a SIGTERM
// drain parks every in-flight play/agent run, flushes the ledger exactly once
// (one forced group commit, so the unflushed tail is bounded by the ≤200 ms
// window), never moves a LadderState, and sets park_ttl as the resume deadline.
func TestParkSIGTERMWritesOneRecordPerInflightRun(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, harnessOpts{
		rules: map[string]types.Rule{"rule-a": {Name: "rule-a", EntryRung: types.RungPlay}},
		playFor: map[string]types.Play{"rule-a": {
			Name: "reload", Version: 1, CheckMode: true,
			Tasks: []types.PlayTask{{Name: "restart", Tool: "service.reload"}},
		}},
	})
	incA := adoptIncident(t, h, "park-a", "rule-a", types.StRecorded, types.RungPlay)
	incB := adoptIncident(t, h, "park-b", "rule-a", types.StRecorded, types.RungPlay)
	stA := driveToPlayCheck(t, h, incA)
	stB := driveToPlayCheck(t, h, incB)
	idle := adoptIncident(t, h, "park-idle", "rule-a", types.StRecorded, types.RungPlay)

	h.l.BeginDrain()
	if !h.l.Draining() {
		t.Fatal("BeginDrain must put the ladder in the drain window")
	}
	flushesBefore := h.ledger.flushes
	rep, err := h.l.Park(ctx, types.ParkSIGTERM)
	if err != nil {
		t.Fatalf("Park: %v", err)
	}
	if len(rep.Records) != 2 {
		t.Fatalf("Park wrote %d park record(s), want 2 (one per in-flight run)", len(rep.Records))
	}
	if got := h.ledger.flushes - flushesBefore; got != 1 {
		t.Errorf("Park flushed the ledger %d time(s), want exactly 1 (one forced group commit, §3.5)", got)
	}
	if rep.FlushedSeq != h.index.Seq() {
		t.Errorf("FlushedSeq = %d, want the index seq %d", rep.FlushedSeq, h.index.Seq())
	}
	if rep.ElapsedMS < 0 {
		t.Errorf("ElapsedMS = %d, want >= 0", rep.ElapsedMS)
	}
	ttl, perr := time.ParseDuration(string(h.l.cfg.ParkTTL))
	if perr != nil {
		t.Fatalf("park_ttl %q: %v", h.l.cfg.ParkTTL, perr)
	}
	if n := h.ledger.countPayload("kind", "park"); n != 2 {
		t.Errorf("%d park record(s) in the ledger, want 2", n)
	}
	seen := map[string]bool{}
	for _, pr := range rep.Records {
		seen[pr.Inc] = true
		if pr.Kind != "play" {
			t.Errorf("park record %s has kind %q, want play (the incident is in play:check_only)", pr.ID, pr.Kind)
		}
		if pr.Reason != string(types.ParkSIGTERM) {
			t.Errorf("park record %s has reason %q, want %q", pr.ID, pr.Reason, types.ParkSIGTERM)
		}
		if pr.State != types.StPlayCheck {
			t.Errorf("park record %s has state %q, want play:check_only", pr.ID, string(pr.State))
		}
		if pr.ID == "" || pr.Sig == "" {
			t.Errorf("park record carries id %q sig %q, want both set (§3.5 field table)", pr.ID, pr.Sig)
		}
		parked, err := types.ParseUTC(pr.ParkedTS)
		if err != nil {
			t.Fatalf("park record %s: parked_ts %q: %v", pr.ID, pr.ParkedTS, err)
		}
		deadline, err := types.ParseUTC(pr.ResumeDeadline)
		if err != nil {
			t.Fatalf("park record %s: resume_deadline %q: %v", pr.ID, pr.ResumeDeadline, err)
		}
		if got := deadline.Sub(parked); got != ttl {
			t.Errorf("park record %s: resume_deadline is parked_ts+%s, want parked_ts+%s (park_ttl)", pr.ID, got, ttl)
		}
	}
	if !seen[incA] || !seen[incB] {
		t.Errorf("parked %v, want both in-flight incidents %s and %s", seen, incA, incB)
	}
	if seen[idle] {
		t.Errorf("the incident %s was parked: only in-flight runs are parked", idle)
	}
	// Parking is a record, not a transition: the states are untouched.
	for _, inc := range []string{incA, incB} {
		got, err := h.l.Incident(ctx, inc)
		if err != nil {
			t.Fatalf("Incident(%s): %v", inc, err)
		}
		if got.State != types.StPlayCheck {
			t.Errorf("incident %s is %q after the drain, want play:check_only", inc, string(got.State))
		}
	}
	if stA.Inc.State != types.StPlayCheck || stB.Inc.State != types.StPlayCheck {
		t.Errorf("the in-memory states moved: %q / %q", string(stA.Inc.State), string(stB.Inc.State))
	}
	idleInc, err := h.l.Incident(ctx, idle)
	if err != nil {
		t.Fatalf("Incident(idle): %v", err)
	}
	if idleInc.State != types.StRecorded {
		t.Errorf("the idle incident is %q, want recorded", string(idleInc.State))
	}
}

// TestReAdoptNeverReappliesAnAppliedTool is the §7 park row for INV-4 / §3.5
// step 2: an already-applied mutating tool is never re-applied on resume even
// though the module is idempotent — the re-adopter re-enters check-only, so the
// counting PlayRunner's Apply is not entered by ReAdopt at all.
func TestReAdoptNeverReappliesAnAppliedTool(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, harnessOpts{
		rules:   map[string]types.Rule{"rule-a": {Name: "rule-a", EntryRung: types.RungPlay}},
		playFor: map[string]types.Play{"rule-a": {Name: "reload", Version: 1, CheckMode: true}},
	})
	// The runner's report of the parked task: the applied call is named in the
	// summary the park record carries.
	h.play.summary = RunSummary{
		Changed:     true,
		TasksRun:    1,
		DiffSummary: "service.reload: restart the unit",
		Applied:     []types.DiffEntry{{Path: "service.reload"}},
		LastTool:    types.ToolCall{ID: "tc_applied", Module: "service.reload"},
	}
	inc := adoptIncident(t, h, "reapply", "rule-a", types.StRecorded, types.RungPlay)
	driveToPlayCheck(t, h, inc)

	rep, err := h.l.Park(ctx, types.ParkSIGTERM)
	if err != nil {
		t.Fatalf("Park: %v", err)
	}
	if len(rep.Records) != 1 {
		t.Fatalf("Park wrote %d park record(s), want 1", len(rep.Records))
	}
	pr := rep.Records[0]
	if !contains(pr.ToolsApplied, "service.reload") {
		t.Errorf("the park record's tools_applied is %v, want the applied tool", pr.ToolsApplied)
	}
	if pr.ToolCallID != "tc_applied" {
		t.Errorf("tool_call_id = %q, want the last completed call tc_applied", pr.ToolCallID)
	}

	runsBefore, _, appliesBefore, _ := h.play.counts()
	if appliesBefore != 0 {
		t.Fatalf("%d apply call(s) happened before the park: nothing applies in shadow", appliesBefore)
	}
	// A counting runner that fails the test if Apply is entered at all.
	mp := &modePlay{inner: h.play, t: t, forbidApply: true}
	withRegistry(h, mp)

	report, err := h.l.ReAdopt(ctx)
	if err != nil {
		t.Fatalf("ReAdopt: %v", err)
	}
	if !contains(report.Resumed, inc) {
		t.Errorf("ReAdopt resumed %v, want %s", report.Resumed, inc)
	}
	runsAfter, _, appliesAfter, _ := h.play.counts()
	if appliesAfter != 0 || mp.applies != 0 {
		t.Errorf("ReAdopt called Apply %d time(s) (wrapper %d): an applied tool is never re-applied (INV-4, §3.5 step 2)", appliesAfter, mp.applies)
	}
	if runsAfter != runsBefore {
		t.Errorf("ReAdopt started %d new play run(s): the resume re-enters check-only, it never re-runs the play", runsAfter-runsBefore)
	}
	if n := resumeRecords(h); n != 1 {
		t.Fatalf("%d resume record(s), want 1", n)
	}
	// The resume record points at the park record and records the check-only
	// re-entry: the authority of the resume is the gate, never a replayed call.
	found := false
	for _, r := range h.ledger.records {
		if r.Kind != types.KLifecycle || fmt.Sprint(r.Payload["kind"]) != "resume" {
			continue
		}
		res, ok := r.Payload["resume"].(map[string]any)
		if !ok {
			t.Fatalf("the resume payload is %T, want a map", r.Payload["resume"])
		}
		if fmt.Sprint(res["park_id"]) != pr.ID {
			t.Errorf("the resume record references %v, want the park record %s", res["park_id"], pr.ID)
		}
		if fmt.Sprint(res["mode"]) != types.OutcomeCheckOnly {
			t.Errorf("the resume mode is %v, want %q (the check-only re-entry of §3.5 step 2)", res["mode"], types.OutcomeCheckOnly)
		}
		found = true
	}
	if !found {
		t.Error("no resume record was written")
	}
}

// TestOrphanWritesRecordNotificationCommentAndMarker is the §7 park row for
// §3.5 step 4: a dead pid plus a worktree path is orphaned explicitly — one
// orphan record, one notification, one outlet comment and the
// `<worktree>/.trouble-orphan.json` marker — and the worktree is left in place
// for the SPEC-08 reaper.
func TestOrphanWritesRecordNotificationCommentAndMarker(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, harnessOpts{
		rules: map[string]types.Rule{"rule-a": {Name: "rule-a", EntryRung: types.RungPlay}},
	})
	dir := t.TempDir()
	inc := adoptIncident(t, h, "orphan", "rule-a", types.StAgentRunning, types.RungAgent)
	st := h.l.incStateFor(inc)
	st.Worktree = dir
	st.PID = 4242
	withPIDAlive(h, func(pid int) bool { return pid != 4242 }) // the run's process is gone
	if h.l.deps.PIDAlive(st.PID) {
		t.Fatalf("the test's liveness seam reports pid %d alive, want dead", st.PID)
	}
	pr := types.ParkRecord{
		ID:             mintID("ev_"),
		Inc:            inc,
		Sig:            st.Inc.Sig,
		HostID:         h.l.cfg.HostID,
		Kind:           "agent",
		State:          types.StAgentRunning,
		Worktree:       dir,
		PID:            st.PID,
		ParkedTS:       types.FormatUTC(h.clock.Now()),
		ResumeDeadline: types.FormatUTC(h.clock.Now().Add(time.Hour)),
		Reason:         string(types.ParkSIGTERM),
	}
	before := len(h.ledger.records)
	h.l.orphan(ctx, pr)

	if got := len(h.ledger.records) - before; got != 1 {
		t.Fatalf("the orphan path appended %d record(s), want 1", got)
	}
	last := h.ledger.last()
	if last == nil || last.Kind != types.KIncident {
		t.Fatalf("the orphan record kind is %v, want the incident kind (the ladder owns it)", last)
	}
	if fmt.Sprint(last.Payload["transition"]) != "orphan" {
		t.Errorf("the orphan record's transition is %v, want orphan", last.Payload["transition"])
	}
	if fmt.Sprint(last.Payload["reason"]) != reasonOrphanWorktree {
		t.Errorf("the orphan record's reason is %v, want %q", last.Payload["reason"], reasonOrphanWorktree)
	}
	orphan, ok := last.Payload["orphan"].(map[string]any)
	if !ok {
		t.Fatalf("the orphan payload is %T, want a map (§3.5 step 4)", last.Payload["orphan"])
	}
	if fmt.Sprint(orphan["worktree"]) != dir {
		t.Errorf("orphan.worktree = %v, want %q", orphan["worktree"], dir)
	}
	if fmt.Sprint(orphan["pid"]) != "4242" {
		t.Errorf("orphan.pid = %v, want 4242", orphan["pid"])
	}
	if fmt.Sprint(orphan["reason"]) != reasonOrphanWorktree || fmt.Sprint(orphan["orphaned_ts"]) == "" {
		t.Errorf("orphan payload %v, want reason %q and an orphaned_ts", orphan, reasonOrphanWorktree)
	}

	if got := h.notify.count(); got != 1 {
		t.Errorf("%d notification(s), want 1 (one Notifier.Emit per orphaned worktree)", got)
	}
	if len(h.notify.class) == 1 && h.notify.class[0] != "orphaned" {
		t.Errorf("the notification class is %q, want orphaned", h.notify.class[0])
	}
	if len(h.outlets.comments) != 1 {
		t.Errorf("%d outlet comment(s), want 1", len(h.outlets.comments))
	} else if !strings.Contains(h.outlets.comments[0], dir) {
		t.Errorf("the outlet comment %q does not name the orphaned worktree %q", h.outlets.comments[0], dir)
	}

	marker := filepath.Join(dir, ".trouble-orphan.json")
	raw, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("the marker file %s is missing: %v", marker, err)
	}
	var got orphanMarkerJSON
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("the marker file does not decode: %v (%s)", err, raw)
	}
	if got.Inc != inc || got.Sig != st.Inc.Sig || got.OrphanedTS == "" {
		t.Errorf("the marker file holds %+v, want inc %s sig %s and an orphaned_ts", got, inc, st.Inc.Sig)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the orphaned worktree was removed (%v): it is left in place for the SPEC-08 reaper", err)
	}
}

// TestLadderPackageIssuesNoExecCommand is the §7 park row's construction guard
// for amendment I: the ladder never runs git (or anything else). It is checked
// with go/parser over the package's own sources rather than a text grep, so a
// comment or a string literal mentioning git cannot satisfy or break it.
func TestLadderPackageIssuesNoExecCommand(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed: the guard cannot locate the package")
	}
	dir := filepath.Dir(thisFile)
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	parsed := 0
	for _, ent := range ents {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, filepath.Join(dir, ent.Name()), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", ent.Name(), err)
		}
		if f.Name.Name != "ladder" {
			t.Errorf("%s declares package %q, want ladder", ent.Name(), f.Name.Name)
		}
		parsed++
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if p == "os/exec" || strings.HasSuffix(p, "/exec") {
				t.Errorf("%s imports %q: the ladder issues no git command and no subprocess (§3.5 step 4, amendment I)", ent.Name(), p)
			}
		}
	}
	if parsed < 5 {
		t.Fatalf("only %d Go file(s) parsed: the guard is not looking at the package", parsed)
	}
}

// TestReAdoptIsIdempotentOnASecondRun is the §7 park row: a second ReAdopt
// produces no new park or resume records — a resume whose park rec_id already has
// a resume record is skipped.
func TestReAdoptIsIdempotentOnASecondRun(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, harnessOpts{
		rules:   map[string]types.Rule{"rule-a": {Name: "rule-a", EntryRung: types.RungPlay}},
		playFor: map[string]types.Play{"rule-a": {Name: "reload", Version: 1, CheckMode: true}},
	})
	first := adoptIncident(t, h, "idem-a", "rule-a", types.StRecorded, types.RungPlay)
	second := adoptIncident(t, h, "idem-b", "rule-a", types.StRecorded, types.RungPlay)
	driveToPlayCheck(t, h, first)
	driveToPlayCheck(t, h, second)
	rep, err := h.l.Park(ctx, types.ParkSIGTERM)
	if err != nil {
		t.Fatalf("Park: %v", err)
	}
	if len(rep.Records) != 2 {
		t.Fatalf("Park wrote %d park record(s), want 2", len(rep.Records))
	}
	reRegisterParks(h, rep.Records)

	one, err := h.l.ReAdopt(ctx)
	if err != nil {
		t.Fatalf("first ReAdopt: %v", err)
	}
	if len(one.Resumed) != 2 {
		t.Fatalf("the first ReAdopt resumed %v, want both parked runs", one.Resumed)
	}
	recordsAfterFirst := parkResumeRecords(h)
	ledgerAfterFirst := len(h.ledger.records)

	two, err := h.l.ReAdopt(ctx)
	if err != nil {
		t.Fatalf("second ReAdopt: %v", err)
	}
	if len(two.Resumed) != 0 || len(two.Failed) != 0 || len(two.Orphaned) != 0 {
		t.Errorf("the second ReAdopt reported resumed=%v failed=%v orphaned=%v, want all empty", two.Resumed, two.Failed, two.Orphaned)
	}
	if got := parkResumeRecords(h); got != recordsAfterFirst {
		t.Errorf("the second ReAdopt left %d park/resume record(s), want %d (idempotent)", got, recordsAfterFirst)
	}
	if got := len(h.ledger.records); got != ledgerAfterFirst {
		t.Errorf("the second ReAdopt appended %d record(s), want 0", got-ledgerAfterFirst)
	}
}

// TestReAdoptDrainsFortyParkedRunsSerially is the §7 park row for §3.5 step 5: a
// restart with forty parked runs drains serially instead of stampeding the box.
// The seriality seam is the host lease: every liveness probe reads the lease
// table's current holder, so two runs in flight at once would show up as a probe
// seeing the lease of a run that had not finished yet.
func TestReAdoptDrainsFortyParkedRunsSerially(t *testing.T) {
	const n = 40
	ctx := context.Background()
	h := newHarness(t, harnessOpts{
		cfg:   Config{AgentRunsPerDay: 100},
		rules: map[string]types.Rule{"rule-agent": {Name: "rule-agent", EntryRung: types.RungAgent}},
	})
	for i := 0; i < n; i++ {
		inc := adoptIncident(t, h, fmt.Sprintf("serial-%02d", i), "rule-agent", types.StAgentRunning, types.RungAgent)
		h.l.incStateFor(inc).PID = 5000 + i
	}
	rep, err := h.l.Park(ctx, types.ParkSIGTERM)
	if err != nil {
		t.Fatalf("Park: %v", err)
	}
	if len(rep.Records) != n {
		t.Fatalf("Park wrote %d park record(s), want %d", len(rep.Records), n)
	}
	parkInc := map[string]string{}
	for _, pr := range rep.Records {
		if pr.Kind != "agent" {
			t.Errorf("park record %s has kind %q, want agent", pr.ID, pr.Kind)
		}
		if pr.PID == 0 {
			t.Errorf("park record %s lost its pid", pr.ID)
		}
	}
	reRegisterParks(h, rep.Records)
	for _, pr := range rep.Records {
		parkInc[pr.ID] = pr.Inc
	}

	var observed []string
	withPIDAlive(h, func(pid int) bool {
		observed = append(observed, currentLeaseInc(h.l))
		return true
	})
	histBefore := len(h.l.LeaseHistory())
	got, err := h.l.ReAdopt(ctx)
	if err != nil {
		t.Fatalf("ReAdopt: %v", err)
	}
	if len(got.Resumed) != n {
		t.Errorf("the re-adopter resumed %d run(s), want %d", len(got.Resumed), n)
	}
	if len(observed) != n {
		t.Fatalf("the re-adopter probed liveness %d time(s), want %d (once per parked agent run)", len(observed), n)
	}
	order := resumeOrder(h)
	if len(order) != n {
		t.Fatalf("%d resume record(s) landed, want %d", len(order), n)
	}
	if observed[0] != "" {
		t.Errorf("a lease was already in flight before the first parked run was probed (holder %q): the drain starts with the host free", observed[0])
	}
	for k := 1; k < n; k++ {
		want := parkInc[order[k-1]]
		if observed[k] != want {
			t.Errorf("at the liveness probe of parked run %d the lease was held by %q, want %q: two agent runs were in flight at once", k, observed[k], want)
		}
	}
	// One renewal per resumed run, and the last resumed run is the one left in
	// flight.
	hist := h.l.LeaseHistory()
	if added := len(hist) - histBefore; added != n {
		t.Errorf("the drain appended %d lease change(s), want %d (one renewed lease per run)", added, n)
	}
	for _, lz := range hist[histBefore:] {
		if lz.State != types.LeaseRenewed {
			t.Errorf("lease %s state is %q, want renewed", lz.LeaseID, lz.State)
		}
	}
	last, ok := h.l.Lease()
	if !ok {
		t.Fatal("the drain left no run in flight, want the last resumed run holding the lease")
	}
	if want := parkInc[order[n-1]]; last.Inc != want {
		t.Errorf("the lease is held by %q after the drain, want %q", last.Inc, want)
	}
	// A resume is a record, not a transition: every parked incident is still in
	// agent:running with its pid intact.
	for _, pr := range rep.Records {
		st := h.l.incStateFor(pr.Inc)
		if st == nil {
			t.Fatalf("incident %s is not in the index", pr.Inc)
		}
		if st.Inc.State != types.StAgentRunning {
			t.Errorf("incident %s is %q after the drain, want agent:running", pr.Inc, string(st.Inc.State))
		}
		if st.PID != pr.PID {
			t.Errorf("incident %s lost its pid: %d, want %d", pr.Inc, st.PID, pr.PID)
		}
	}
}
