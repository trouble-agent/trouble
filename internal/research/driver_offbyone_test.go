package research

// driver_offbyone_test.go — SPEC-07 §7 `driver_offbyone_test.go`.
//
// The first case is the live regression the spec pins: the lab's strict decoder
// rejects a top-level `fingerprint`, which is the exact payload PRD §05 ②½
// imagined. The test asserts trouble's codec can never produce that body, that
// a recorded 400 maps onto TROUBLE-RESEARCH-002 + degraded +
// strict_decoder_reject, and that exactly one `research` and one `gap` record
// land in the ledger.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

func TestStrictDecoderFingerprintRejected(t *testing.T) {
	lab := newLabStub(t)
	lab.discover400 = true
	s, deps := newTestService(t, lab, nil)

	out, err := s.Run(context.Background(), testIncident(), testSig(), bundleFor(nil))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.State != types.ResDegraded {
		t.Fatalf("state = %q, want degraded", out.State)
	}
	if out.DegradedReason != types.ResReasonStrictDecoder {
		t.Fatalf("reason = %q, want %s", out.DegradedReason, types.ResReasonStrictDecoder)
	}
	recs := deps.payloads(types.KResearch)
	if len(recs) != 1 {
		t.Fatalf("research records = %d, want exactly 1", len(recs))
	}
	if got := recs[0]["error_code"]; got != string(types.CodeResearch002) {
		t.Fatalf("payload.error_code = %v, want %s", got, types.CodeResearch002)
	}
	if got := recs[0]["http_status"]; got != 400 {
		t.Fatalf("payload.http_status = %v, want 400", got)
	}
	if gaps := deps.payloads(types.KGap); len(gaps) != 1 || gaps[0]["cause"] != "research_strict_decoder_reject" {
		t.Fatalf("gap records = %v, want exactly one research_strict_decoder_reject", gaps)
	}
	// The codec can never produce a body carrying a top-level fingerprint: the
	// only place the sig appears is inside context (§3.3).
	for _, b := range lab.seenBodies() {
		if strings.Contains(b, `"fingerprint"`) && !strings.Contains(b, `"context"`) {
			t.Fatalf("codec emitted a top-level fingerprint: %s", b)
		}
	}
}

func TestGoldenWireBodies(t *testing.T) {
	lab := newLabStub(t)
	lab.discoverFound = true
	s, _ := newTestService(t, lab, nil)
	d := s.Driver()

	// Discover: broad (D1) sends no narrowing fields and include_related=false.
	if _, err := d.Discover(context.Background(), types.DiscoverRequest{ProblemClass: "svc-crash-loop"}); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	// Discover: narrowed (D2) sends environment/language/version.
	if _, err := d.Discover(context.Background(), types.DiscoverRequest{
		ProblemClass: "svc-crash-loop", Env: "prod", Lang: "go", Version: "2.4.1",
	}); err != nil {
		t.Fatalf("Discover narrow: %v", err)
	}
	// Submit: exactly four top-level fields.
	if _, err := d.Submit(context.Background(), types.SubmitRequest{
		ProblemClass: "svc-crash-loop", Description: "worker restart loop", Cadence: "end-of-day",
		Context: map[string]any{"fingerprint": "sentinel:sha256v1:9f2c1d3e4b5a6c7d"},
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	// Poll, health and stats.
	if _, err := d.Poll(context.Background(), "sub_87ee13"); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	h, err := d.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if h.Status != "ok" || h.Uptime != "3h47m6s" {
		t.Fatalf("health = %+v", h)
	}
	st, err := d.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.TotalProblems != 1716 || st.TotalAnswers != 1900 || st.VerifiedAnswers != 1872 || st.QueueDepth != 0 || !st.SolverAvailable {
		t.Fatalf("stats = %+v", st)
	}

	bodies := lab.seenBodies()
	if len(bodies) < 3 {
		t.Fatalf("recorded %d bodies, want ≥3", len(bodies))
	}
	// D1 body byte-for-byte.
	if bodies[0] != `{"problem_class":"svc-crash-loop","include_related":false}` {
		t.Fatalf("D1 body = %s", bodies[0])
	}
	// D2 body byte-for-byte.
	if bodies[1] != `{"problem_class":"svc-crash-loop","environment":"prod","language":"go","version":"2.4.1","include_related":false}` {
		t.Fatalf("D2 body = %s", bodies[1])
	}
	// Submit body: the four keys in schema order.
	var m map[string]any
	if err := json.Unmarshal([]byte(bodies[2]), &m); err != nil {
		t.Fatalf("submit body is not JSON: %v", err)
	}
	if len(m) != 4 {
		t.Fatalf("submit body has %d top-level fields, want 4: %s", len(m), bodies[2])
	}
	for _, k := range []string{"problem_class", "description", "cadence", "context"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("submit body missing %q: %s", k, bodies[2])
		}
	}
	if !strings.HasPrefix(bodies[2], `{"problem_class":"svc-crash-loop","description":`) {
		t.Fatalf("submit body key order drifted: %s", bodies[2])
	}
}

func TestNonJSON200IsProtocolError(t *testing.T) {
	lab := newLabStub(t)
	lab.discoverHTML = true
	s, deps := newTestService(t, lab, nil)
	out, err := s.Run(context.Background(), testIncident(), testSig(), bundleFor(nil))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.State != types.ResDegraded || out.DegradedReason != types.ResReasonLabUnreachable {
		t.Fatalf("outcome = %+v, want degraded/lab_unreachable", out)
	}
	p := deps.lastPayload(types.KResearch)
	if p["error_code"] != string(types.CodeResearch001) {
		t.Fatalf("error_code = %v, want 001", p["error_code"])
	}
	gaps := deps.payloads(types.KGap)
	if len(gaps) != 1 || gaps[0]["cause"] != "research_lab_unreachable" {
		t.Fatalf("gaps = %v, want one research_lab_unreachable", gaps)
	}
}

func TestHealthAtRootPathOnly(t *testing.T) {
	lab := newLabStub(t)
	lab.healthStatus = 404
	s, _ := newTestService(t, lab, nil)
	if _, err := s.Driver().Health(context.Background()); err == nil {
		t.Fatalf("a 404 health must be an error (health is at the root path)")
	} else if !strings.Contains(err.Error(), "health_404") {
		t.Fatalf("error = %v, want the health_404 note", err)
	}
}

func TestQueueStateMapping(t *testing.T) {
	lab := newLabStub(t)
	s, _ := newTestService(t, lab, nil)
	d := s.Driver()
	// stage wins over status; an unknown status is not decoded as a terminal
	// state (the poll budget ends the loop, §2.3 C).
	lab.queueStates = []string{"queued", "solving", "storing", "failed", "weird"}
	want := []string{types.QueueQueued, types.QueueSolving, types.QueueSolved, types.QueueFailed, ""}
	for i, w := range want {
		st, err := d.Poll(context.Background(), "sub_x")
		if err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
		if st.State != w {
			t.Fatalf("poll %d state = %q, want %q", i, st.State, w)
		}
	}
}

func TestSubmitStatuses(t *testing.T) {
	// 409 is success and carries duplicate=true (never parsed from a body).
	lab := newLabStub(t)
	lab.submit409 = true
	s, _ := newTestService(t, lab, nil)
	resp, err := s.Driver().Submit(context.Background(), types.SubmitRequest{ProblemClass: "x"})
	if err != nil {
		t.Fatalf("409 must not be an error: %v", err)
	}
	if !resp.Duplicate {
		t.Fatalf("resp = %+v, want duplicate", resp)
	}
	// 503 is code 004 / solver_unavailable.
	lab2 := newLabStub(t)
	lab2.submit503 = true
	s2, _ := newTestService(t, lab2, nil)
	if _, err := s2.Driver().Submit(context.Background(), types.SubmitRequest{ProblemClass: "x"}); err == nil {
		t.Fatalf("503 must be an error")
	} else {
		var de *driverError
		if !asDriverError(err, &de) || de.Code != types.CodeResearch004 {
			t.Fatalf("503 error = %v, want code 004", err)
		}
	}
}

func TestDiscoverMissIsNotFound(t *testing.T) {
	lab := newLabStub(t)
	lab.discover404 = true
	s, _ := newTestService(t, lab, nil)
	_, err := s.Driver().Discover(context.Background(), types.DiscoverRequest{ProblemClass: "nope"})
	if !isNotFound(err) {
		t.Fatalf("a 404 discover must be a miss, got %v", err)
	}
}

func TestCapabilityProbeDiscoversOnlyMode(t *testing.T) {
	lab := newLabStub(t)
	lab.openAPIPaths = map[string]any{"/api/v1/problems/discover": map[string]any{}}
	s, deps := newTestService(t, lab, nil)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if s.Capability() != string(capDiscoverOnly) {
		t.Fatalf("capability = %q, want discover_only", s.Capability())
	}
	// Discover-only: the rung runs D1 + corpus and never submits.
	out, err := s.Run(context.Background(), testIncident(), testSig(), bundleFor(nil))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.State != types.ResSkipped {
		t.Fatalf("state = %q, want skipped (capability missing)", out.State)
	}
	if lab.count("/api/v1/problems/submit") != 0 {
		t.Fatalf("submit was called in discover-only mode")
	}
	p := deps.lastPayload(types.KResearch)
	if p["state"] != types.ResSkipped || p["skip_reason"] != types.ResSkipCapabilityMissing {
		t.Fatalf("payload = %v", p)
	}
	// A detected capability is not a failure: no error code and no gap.
	if p["error_code"] != "" {
		t.Fatalf("error_code = %v, want empty", p["error_code"])
	}
	if len(deps.payloads(types.KGap)) != 0 {
		t.Fatalf("a capability-detected skip must not emit a gap")
	}
}

func TestUnknownDriverRefusedAtLoad(t *testing.T) {
	if _, err := New(map[string]any{"driver": "nope", "lab_url": "http://x"}, newFakeDeps()); err == nil {
		t.Fatalf("an unknown driver must fail startup config validation")
	}
	if _, err := New(map[string]any{"driver": types.DriverWebhook}, newFakeDeps()); err == nil {
		t.Fatalf("the webhook driver requires a url")
	}
}

// asDriverError is errors.As for *driverError, kept local so the test file does
// not import errors twice.
func asDriverError(err error, out **driverError) bool {
	de, ok := err.(*driverError)
	if ok {
		*out = de
	}
	return ok
}

// TestWebhookDriverUsesConfiguredURLs asserts the relayed driver posts the same
// bodies to the operator's URL (SPEC-07 §2.2).
func TestWebhookDriverUsesConfiguredURLs(t *testing.T) {
	lab := newLabStub(t)
	lab.discoverFound = true
	s, _ := newTestService(t, lab, map[string]any{
		"driver": types.DriverWebhook, "webhook_url": lab.url() + "/api/v1/problems/discover",
	})
	resp, err := s.Driver().Discover(context.Background(), types.DiscoverRequest{ProblemClass: "x"})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if !resp.Found {
		t.Fatalf("resp = %+v", resp)
	}
	if lab.count("/api/v1/problems/discover") != 1 {
		t.Fatalf("webhook url was not used: %v", lab.requests)
	}
}

// TestRequestContextHasProvenance pins the §3.3 submit context keys.
func TestRequestContextHasProvenance(t *testing.T) {
	lab := newLabStub(t)
	lab.submit409 = true // keeps the rung short: no poller
	s, deps := newTestService(t, lab, map[string]any{"host_id": "7f3a91c2d4e5b607", "daemon_version": "0.1.0"})
	out, err := s.Run(context.Background(), testIncident(), testSig(), bundleFor(map[string]any{
		"env": "prod", "lang": "go", "version": "2.4.1", "stack": "worker.py:118 claim",
	}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.State != types.ResRequested || out.SubmissionID != "" {
		t.Fatalf("outcome = %+v, want requested without an id", out)
	}
	// The submit body carries the provenance under context.
	var found bool
	for _, b := range lab.seenBodies() {
		if !strings.Contains(b, `"context"`) {
			continue
		}
		found = true
		var body struct {
			Context map[string]any `json:"context"`
		}
		if err := json.Unmarshal([]byte(b), &body); err != nil {
			t.Fatalf("submit body: %v", err)
		}
		if body.Context["fingerprint"] != testSig().String() {
			t.Fatalf("context.fingerprint = %v", body.Context["fingerprint"])
		}
		if body.Context["incident_id"] != testIncident().ID {
			t.Fatalf("context.incident_id = %v", body.Context["incident_id"])
		}
		origin, _ := body.Context["origin"].(map[string]any)
		if origin["host_id"] != "7f3a91c2d4e5b607" {
			t.Fatalf("context.origin = %v", body.Context["origin"])
		}
	}
	if !found {
		t.Fatalf("no submit body carried a context")
	}
	if p := deps.lastPayload(types.KResearch); p["state"] != types.ResRequested {
		t.Fatalf("payload state = %v", p["state"])
	}
}

// TestRequestTimeoutIsTransient covers the transport-failure class.
func TestRequestTimeoutIsTransient(t *testing.T) {
	lab := newLabStub(t)
	s, deps := newTestService(t, lab, map[string]any{
		"request_timeout": "1ns", // every request times out immediately
	})
	out, err := s.Run(context.Background(), testIncident(), testSig(), bundleFor(nil))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.State != types.ResDegraded || out.DegradedReason != types.ResReasonLabUnreachable {
		t.Fatalf("outcome = %+v", out)
	}
	if p := deps.lastPayload(types.KResearch); p["error_code"] != string(types.CodeResearch001) {
		t.Fatalf("error_code = %v", p["error_code"])
	}
	_ = time.Millisecond
}
