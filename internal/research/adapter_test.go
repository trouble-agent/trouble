package research

// adapter_test.go — SPEC-07 §7 `adapter_test.go`: the ResearchPort adapter, the
// caller-supplied slug, the boot replay and the play draft.

import (
	"context"
	"strings"
	"testing"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

func TestRequestSuppliedSlugIsVerbatim(t *testing.T) {
	lab := newLabStub(t)
	lab.submit409 = true
	s, deps := newTestService(t, lab, nil)
	out, err := s.Request(context.Background(), testIncident(), types.Subject{
		Slug: "rule-name-as-slug", Description: "operator supplied",
		Context: map[string]any{"sig": testSig().String()},
	})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if out.Slug.Slug != "rule-name-as-slug" {
		t.Fatalf("slug = %q, want the supplied value verbatim", out.Slug.Slug)
	}
	if out.Slug.Fallback {
		t.Fatalf("a supplied slug is not a fallback")
	}
	if p := deps.lastPayload(types.KResearch); p["error_code"] == string(types.CodeResearch005) {
		t.Fatalf("derivation must be skipped when a slug is supplied (no 005)")
	}
	// The probe carried the supplied class.
	var seen bool
	for _, b := range lab.seenBodies() {
		if strings.Contains(b, `"problem_class":"rule-name-as-slug"`) {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("the supplied slug never reached the lab: %v", lab.seenBodies())
	}
}

func TestRequestDerivesWhenSlugAbsent(t *testing.T) {
	lab := newLabStub(t)
	lab.submit409 = true
	s, deps := newTestService(t, lab, nil)
	out, err := s.Request(context.Background(), testIncident(), types.Subject{
		Context: map[string]any{"sig": testSig().String(), "unit": "payment-worker.service", "message": "start request repeated too quickly"},
	})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if out.Slug.Slug != "payment-worker-crash-loop" {
		t.Fatalf("slug = %q, want the derived class", out.Slug.Slug)
	}
	if p := deps.lastPayload(types.KResearch); p["taxonomy"] != TaxCrashLoop {
		t.Fatalf("payload.taxonomy = %v", p["taxonomy"])
	}
}

func TestFallbackSlugSkipsSubmitAndRecords005(t *testing.T) {
	lab := newLabStub(t)
	s, deps := newTestService(t, lab, nil)
	// Nothing identifies the subject and no rule matches: the fallback path.
	out, err := s.Run(context.Background(), testIncident(), testSig(), map[string]any{
		"message": "something entirely unremarkable happened", "source": "generic",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.Slug.Fallback || out.Slug.Slug != "unknown" {
		t.Fatalf("outcome = %+v, want the fallback slug", out)
	}
	if out.State != types.ResSkipped || out.DegradedReason != types.ResSkipClassUnknown {
		t.Fatalf("state = %q reason = %q, want skipped/class_unknown", out.State, out.DegradedReason)
	}
	if lab.count("/api/v1/problems/submit") != 0 {
		t.Fatalf("submitting under a fallback slug would pollute a shared cache (§6.3)")
	}
	p := deps.lastPayload(types.KResearch)
	if p["error_code"] != string(types.CodeResearch005) || p["slug_fallback"] != true {
		t.Fatalf("payload = %v", p)
	}
	// allow_unknown_class_submit makes the submit legal again.
	lab2 := newLabStub(t)
	lab2.submit409 = true
	s2, _ := newTestService(t, lab2, map[string]any{"allow_unknown_class_submit": true})
	if _, err := s2.Run(context.Background(), testIncident(), testSig(), map[string]any{
		"message": "something entirely unremarkable happened", "source": "generic",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if lab2.count("/api/v1/problems/submit") != 1 {
		t.Fatalf("allow_unknown_class_submit did not reach submit")
	}
}

func TestOutcomesNewestFirstAfterReplay(t *testing.T) {
	lab := newLabStub(t)
	lab.submit409 = true
	s, deps := newTestService(t, lab, nil)
	// A few rungs for one sig, then a boot replay of the same records.
	for i := 0; i < 3; i++ {
		if _, err := s.Run(context.Background(), testIncident(), testSig(), bundleFor(nil)); err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
	}
	deps2 := newFakeDeps()
	s2, err := New(map[string]any{"lab_url": lab.url(), "corpus_roots": []any{}, "lab_data_dir": t.TempDir()}, deps2)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s2.Close()
	var recs []types.Record
	for i, d := range deps.ofKind(types.KResearch) {
		recs = append(recs, types.Record{Seq: uint64(i + 1), Kind: types.KResearch, Sig: d.Sig, Inc: d.Inc, TS: types.NowUTC(), Payload: d.Payload})
	}
	s2.Replay(recs)
	got := s2.Outcomes(testSig().String())
	if len(got) != 3 {
		t.Fatalf("outcomes = %d, want 3", len(got))
	}
	// Newest first: the replay prepends, so the last-appended record leads.
	if got[0].ID != recs[len(recs)-1].Payload["res_id"] {
		t.Fatalf("outcomes are not newest-first: %s vs %v", got[0].ID, recs[len(recs)-1].Payload["res_id"])
	}
	for _, o := range got {
		if o.ID == "" || o.State == "" {
			t.Fatalf("replayed outcome is incomplete: %+v", o)
		}
	}
}

func TestResumeReAdoptsAnInFlightSubmission(t *testing.T) {
	lab := newLabStub(t)
	s, _ := newTestService(t, lab, nil)
	out, err := s.Run(context.Background(), testIncident(), testSig(), bundleFor(nil))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.State != types.ResRequested || out.SubmissionID == "" {
		t.Fatalf("outcome = %+v, want a queued submission", out)
	}
	// A restart: a fresh Service re-adopts the recorded outcome and polls it.
	lab2 := newLabStub(t)
	s2, _ := newTestService(t, lab2, nil)
	resumed, err := s2.Resume(context.Background(), out)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.ID != out.ID {
		t.Fatalf("Resume changed the res id: %s → %s", out.ID, resumed.ID)
	}
	final, err := s2.Poll(context.Background(), out.ID)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if final.State != types.ResReturned {
		t.Fatalf("re-adopted rung = %+v, want returned", final)
	}
	_ = s
}

func TestParkWritesKillSwitchAndStops(t *testing.T) {
	lab := newLabStub(t)
	lab.queueStates = []string{"solving"}
	s, deps := newTestService(t, lab, map[string]any{"poll_interval": "5ms", "poll_timeout": "2s"})
	out, err := s.Run(context.Background(), testIncident(), testSig(), bundleFor(nil))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	before := lab.total()
	parked, err := s.Park(context.Background(), out)
	if err != nil {
		t.Fatalf("Park: %v", err)
	}
	if parked.State != types.ResSkipped || parked.DegradedReason != types.ResSkipKillSwitch {
		t.Fatalf("parked outcome = %+v, want skipped/kill_switch", parked)
	}
	if parked.ID != out.ID {
		t.Fatalf("the res_ id must survive the park")
	}
	p := deps.lastPayload(types.KResearch)
	if p["skip_reason"] != types.ResSkipKillSwitch {
		t.Fatalf("payload.skip_reason = %v", p["skip_reason"])
	}
	// The poller stopped: no further requests.
	set := lab.total()
	if set > before+1 {
		t.Fatalf("the poller kept running after Park: %d → %d", before, set)
	}
}

func TestPlayDraftOnlyWhenAskedFor(t *testing.T) {
	answer := map[string]any{
		"status": "verified", "solution": "reload the unit",
		"tasks": []any{
			map[string]any{"name": "reload", "tool": "service.reload", "args": map[string]any{"unit": "payment-worker.service"}},
			map[string]any{"name": "noop"}, // no tool → dropped
		},
	}
	lab := newLabStub(t)
	lab.discoverFound = true
	lab.discoverAnswer = answer

	// Default: apply_brief_as_play=false ⇒ no draft, the brief is text.
	s, _ := newTestService(t, lab, nil)
	out, err := s.Run(context.Background(), testIncident(), testSig(), bundleFor(nil))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, ok := out.Brief["play_draft"]; ok {
		t.Fatalf("a draft must not appear unless research.apply_brief_as_play=true")
	}
	if !boolField(out.Brief, "resolve_outright") {
		t.Fatalf("a verified answer resolves outright: %v", out.Brief)
	}

	lab2 := newLabStub(t)
	lab2.discoverFound = true
	lab2.discoverAnswer = answer
	s2, _ := newTestService(t, lab2, map[string]any{"apply_brief_as_play": true})
	out2, err := s2.Run(context.Background(), testIncident(), testSig(), bundleFor(nil))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	draft, ok := out2.Brief["play_draft"].([]map[string]any)
	if !ok || len(draft) != 1 {
		t.Fatalf("play_draft = %v, want exactly the well-formed task", out2.Brief["play_draft"])
	}
	if draft[0]["tool"] != "service.reload" || draft[0]["name"] != "reload" {
		t.Fatalf("draft task = %v", draft[0])
	}
	for _, k := range []string{"name", "tool", "args", "when", "register", "retries", "on_fail"} {
		if _, ok := draft[0][k]; !ok {
			t.Fatalf("draft task is missing the PlayTask field %q", k)
		}
	}
}

func TestSnapshotShape(t *testing.T) {
	lab := newLabStub(t)
	s, _ := newTestService(t, lab, nil)
	snap := s.Snapshot()
	for _, k := range []string{"driver", "lab_health", "cost", "inflight", "cooldown_until", "fence"} {
		if _, ok := snap[k]; !ok {
			t.Fatalf("Snapshot is missing %q: %v", k, snap)
		}
	}
	if snap["driver"] != types.DriverOffByOne {
		t.Fatalf("snapshot driver = %v", snap["driver"])
	}
}
