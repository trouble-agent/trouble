package research

// helpers_test.go — the shared test fixture: a recording ledger, an identity
// scrubber, a scriptable Off-by-One lab and a stub corpus. Every §7 test file
// builds on these, and none of them reach the network: the lab is an httptest
// server, which is exactly what §7's gates require.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// fakeDeps is a recording Deps: every appended draft is kept in order, and Scrub
// is an identity function that counts the bytes it saw.
type fakeDeps struct {
	mu      sync.Mutex
	drafts  []types.RecordDraft
	recs    []types.Record
	scrubs  int
	scrubFn func(types.ScrubTarget, []byte) []byte
	nowFn   func() time.Time
	start   time.Time
}

func newFakeDeps() *fakeDeps {
	return &fakeDeps{start: time.Now(), nowFn: time.Now}
}

func (f *fakeDeps) Append(_ context.Context, d types.RecordDraft) (types.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drafts = append(f.drafts, d)
	rec := types.Record{
		Seq: uint64(len(f.recs) + 1), RecID: types.NewID(types.PEv), TS: types.NowUTC(),
		Kind: d.Kind, SchemaVersion: 1, Sig: d.Sig, Inc: d.Inc,
		Origin: d.Origin, Actor: d.Actor, Redactions: d.Redactions, Payload: d.Payload,
	}
	f.recs = append(f.recs, rec)
	return rec, nil
}

func (f *fakeDeps) Scrub(_ context.Context, target types.ScrubTarget, b []byte) (types.ScrubResult, error) {
	f.mu.Lock()
	f.scrubs++
	fn := f.scrubFn
	f.mu.Unlock()
	out := b
	if fn != nil {
		out = fn(target, b)
	}
	return types.ScrubResult{Value: out, BytesIn: len(b), Redactions: 0}, nil
}

func (f *fakeDeps) Now() time.Time { return f.nowFn() }

func (f *fakeDeps) Monotonic() time.Duration { return time.Since(f.start) }

// ofKind returns the appended drafts of one kind.
func (f *fakeDeps) ofKind(kind types.RecordKind) []types.RecordDraft {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []types.RecordDraft
	for _, d := range f.drafts {
		if d.Kind == kind {
			out = append(out, d)
		}
	}
	return out
}

// payloads returns every payload of one kind, newest last.
func (f *fakeDeps) payloads(kind types.RecordKind) []map[string]any {
	var out []map[string]any
	for _, d := range f.ofKind(kind) {
		out = append(out, d.Payload)
	}
	return out
}

// lastPayload returns the newest payload of a kind (nil when none).
func (f *fakeDeps) lastPayload(kind types.RecordKind) map[string]any {
	ps := f.payloads(kind)
	if len(ps) == 0 {
		return nil
	}
	return ps[len(ps)-1]
}

// labStub is a scriptable Off-by-One lab. Every field is a mode switch, so a
// test can reproduce the measured behaviours (strict decoder, SPA catch-all,
// solver unavailable, unknown queue state) without a live lab.
type labStub struct {
	t *testing.T

	srv *httptest.Server

	mu       sync.Mutex
	requests []string // method + path, in order
	bodies   []string // request bodies, in order

	// responses
	discoverFound   bool
	discover404     bool
	discover400     bool
	discoverHTML    bool
	discoverAnswer  map[string]any
	submitID        string
	submit409       bool
	submit503       bool
	submit400       string
	queueStates     []string // consumed in order; last repeats
	queue404        bool
	healthStatus    int
	healthBody      string
	statsBody       string
	openAPIPaths    map[string]any
	failOpenAPI     bool
	queueAnswer     map[string]any
	writeDeadlineMS int
}

func newLabStub(t *testing.T) *labStub {
	l := &labStub{
		t: t, submitID: "sub_87ee13", queueStates: []string{"solving", "solved"},
		healthBody: `{"status":"ok","uptime":"3h47m6s"}`,
		statsBody:  `{"total_problems":1716,"total_answers":1900,"verified_answers":1872,"queue_depth":0,"hit_rate":0.9,"coverage":0.8,"solver_available":true}`,
		openAPIPaths: map[string]any{
			"/api/v1/problems/discover":     map[string]any{},
			"/api/v1/problems/submit":       map[string]any{},
			"/api/v1/queue/{submission_id}": map[string]any{},
		},
		discoverAnswer: map[string]any{
			"id": 1210, "problem_class": "payment-worker-crash-loop", "env": "prod",
			"status": "verified", "solution": "raise the restart limit in the unit",
			"signatures": map[string]any{"result": "passed"},
		},
		queueAnswer: map[string]any{
			"id": 1211, "status": "verified", "solution": "set MemoryMax=512M",
			"signatures": map[string]any{"result": "passed"},
		},
	}
	l.srv = httptest.NewServer(http.HandlerFunc(l.handle))
	t.Cleanup(l.srv.Close)
	return l
}

func (l *labStub) url() string { return l.srv.URL }

// saw returns whether a path was requested and how many times.
func (l *labStub) count(path string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, r := range l.requests {
		if strings.Contains(r, path) {
			n++
		}
	}
	return n
}

// total counts every request the stub saw.
func (l *labStub) total() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.requests)
}

// seenBodies returns the recorded request bodies.
func (l *labStub) seenBodies() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.bodies))
	copy(out, l.bodies)
	return out
}

func (l *labStub) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	l.mu.Lock()
	l.requests = append(l.requests, r.Method+" "+r.URL.Path)
	l.bodies = append(l.bodies, string(body))
	l.mu.Unlock()

	jsonErr := func(status int, code, msg string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "message": msg})
	}

	switch {
	case r.URL.Path == "/health":
		if l.healthStatus != 0 {
			w.WriteHeader(l.healthStatus)
			_, _ = w.Write([]byte(`{"error":"not_found","message":"no health here"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(l.healthBody))
	case r.URL.Path == "/api/v1/stats":
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(l.statsBody))
	case r.URL.Path == "/openapi.json":
		if l.failOpenAPI {
			jsonErr(http.StatusNotFound, "not_found", "no openapi")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"paths": l.openAPIPaths})
	case r.URL.Path == "/api/v1/problems/discover":
		if l.discoverHTML {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<!doctype html><html>SPA</html>"))
			return
		}
		if l.discover400 {
			jsonErr(http.StatusBadRequest, "invalid_request", `invalid JSON: json: unknown field "fingerprint"`)
			return
		}
		if l.discover404 {
			jsonErr(http.StatusNotFound, "not_found", "problem class not found")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"found": l.discoverFound, "answer": l.discoverAnswer})
	case r.URL.Path == "/api/v1/problems/submit":
		if l.submit503 {
			jsonErr(http.StatusServiceUnavailable, "solver_unavailable", "no solver")
			return
		}
		if l.submit400 != "" {
			jsonErr(http.StatusBadRequest, "invalid_request", l.submit400)
			return
		}
		if l.submit409 {
			jsonErr(http.StatusConflict, "duplicate", "already queued")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"submission_id":"` + l.submitID + `","status":"queued","position":7,"estimated_time":"3m30s","existing_solutions":20}`))
	case strings.HasPrefix(r.URL.Path, "/api/v1/queue/"):
		if l.queue404 {
			jsonErr(http.StatusNotFound, "not_found", "no such submission")
			return
		}
		l.mu.Lock()
		state := l.queueStates[0]
		if len(l.queueStates) > 1 {
			l.queueStates = l.queueStates[1:]
		}
		answer := l.queueAnswer
		l.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		payload := map[string]any{"submission_id": l.submitID, "status": state, "stage": state, "position": 3}
		if state == "solved" {
			payload["answer"] = answer
		}
		_ = json.NewEncoder(w).Encode(payload)
	default:
		jsonErr(http.StatusNotFound, "not_found", "unknown path "+r.URL.Path)
	}
}

// stubCorpus is the corpus seam's test double.
type stubCorpus struct {
	hits    []corpusHit
	res     corpusResult
	err     error
	queries []string
}

func (c *stubCorpus) Grep(_ context.Context, class string, terms []string) (corpusResult, error) {
	c.queries = append(c.queries, class)
	if c.err != nil {
		return c.res, c.err
	}
	r := c.res
	r.Hits = c.hits
	return r, nil
}

// testSig is a canonical journald sig.
func testSig() types.Sig {
	return types.Sig{
		Source: types.SrcJournald, Algo: types.SigAlgoSHA256, NormVersion: types.NormVersionV1,
		Short: "2ab4c6d8e0f1a3b5",
	}
}

// testIncident is the worked-example incident.
func testIncident() types.Incident {
	return types.Incident{
		ID: "inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE", Sig: testSig().String(),
		State: types.StResSkipped, EntryRung: types.RungResearch, Rung: types.RungResearch,
		Severity: types.SevHigh, OpenedTS: "2026-09-16T09:14:05.221Z",
	}
}

// newTestService wires a Service against a lab stub with a fast poll cadence.
func newTestService(t *testing.T, lab *labStub, extra map[string]any) (*Service, *fakeDeps) {
	t.Helper()
	deps := newFakeDeps()
	cfg := map[string]any{
		"lab_url":         lab.url(),
		"poll_interval":   "1ms",
		"poll_timeout":    "2s",
		"corpus_roots":    []any{},     // no filesystem walk unless a test asks
		"lab_data_dir":    t.TempDir(), //
		"connect_timeout": "1s",
		"request_timeout": "2s",
		"submit_timeout":  "2s",
	}
	for k, v := range extra {
		cfg[k] = v
	}
	s, err := New(cfg, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, deps
}

// bundleFor is the incident bundle a sensor-sourced rung passes.
func bundleFor(extra map[string]any) map[string]any {
	b := map[string]any{
		"unit":    "payment-worker.service",
		"message": "start request repeated too quickly",
		"source":  "journald:payment-worker",
	}
	for k, v := range extra {
		b[k] = v
	}
	return b
}
