package issues

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// ── ledger double ────────────────────────────────────────────────────────────

// fakeLedger is the SPEC-01 single-writer double: it mints rec_id/seq like the
// real ledger and keeps every record for assertions.
type fakeLedger struct {
	mu   sync.Mutex
	recs []types.Record
	seq  uint64
	// now is the clock the double stamps records with; the desk's windows are
	// monotonic-clock driven, so a double that used the wall clock would hide
	// every window assertion.
	now func() time.Time
}

func (f *fakeLedger) stamp() time.Time {
	if f.now != nil {
		return f.now()
	}
	return time.Now()
}

func (f *fakeLedger) Append(_ context.Context, d types.RecordDraft) (types.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	rec := types.Record{
		Seq:           f.seq,
		RecID:         types.NewID(types.PEv),
		TS:            types.FormatUTC(f.stamp()),
		Kind:          d.Kind,
		SchemaVersion: 1,
		Sig:           d.Sig,
		Inc:           d.Inc,
		Origin:        d.Origin,
		Actor:         d.Actor,
		Redactions:    d.Redactions,
		Payload:       d.Payload,
	}
	if rec.Origin.Source == "" {
		rec.Origin = types.Origin{HostID: "testhost", Source: "issues"}
	}
	f.recs = append(f.recs, rec)
	return rec, nil
}

func (f *fakeLedger) records() []types.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]types.Record(nil), f.recs...)
}

// ScanFrom satisfies the Scanner port so the desk can rebuild at boot.
func (f *fakeLedger) ScanFrom(_ uint64, yield func(types.Record) bool) error {
	for _, r := range f.records() {
		if !yield(r) {
			return nil
		}
	}
	return nil
}

// ops lists the payload op of every issue record, in order.
func (f *fakeLedger) ops() []string {
	var out []string
	for _, r := range f.records() {
		if r.Kind == types.KIssue {
			out = append(out, strPayload(r.Payload, "op"))
		}
	}
	return out
}

func (f *fakeLedger) byOp(op string) []types.Record {
	var out []types.Record
	for _, r := range f.records() {
		if strPayload(r.Payload, "op") == op {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeLedger) countOps(op string) int { return len(f.byOp(op)) }

func (f *fakeLedger) gaps() []types.Record {
	var out []types.Record
	for _, r := range f.records() {
		if r.Kind == types.KGap {
			out = append(out, r)
		}
	}
	return out
}

// payloadBlob renders a payload for leak assertions.
func payloadBlob(p map[string]any) string {
	raw, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	return string(raw)
}

// ── scrubber double ─────────────────────────────────────────────────────────

type fakeScrubber struct {
	mu    sync.Mutex
	calls int
	// refuse makes every call fail (SPEC-02 refusal path).
	refuse  bool
	lastRaw string
}

func (f *fakeScrubber) ScrubBytes(_ context.Context, target types.ScrubTarget, _ string, b []byte) ([]byte, types.ScrubResult, error) {
	f.mu.Lock()
	f.calls++
	f.lastRaw = string(b)
	refuse := f.refuse
	f.mu.Unlock()
	if refuse {
		return nil, types.ScrubResult{}, fmt.Errorf("scrub refused")
	}
	out := []byte(strings.ReplaceAll(string(b), "supersecret", "[REDACTED:test]"))
	red := 0
	if strings.Contains(string(b), "supersecret") {
		red = 1
	}
	return out, types.ScrubResult{Value: out, Redactions: red, BytesIn: len(b)}, nil
}

// ── clock double ────────────────────────────────────────────────────────────

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
	t0  time.Time
}

func newFakeClock() *fakeClock {
	t := time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)
	return &fakeClock{now: t, t0: t}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Monotonic() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now.Sub(c.t0)
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// ── fake GitHub API ─────────────────────────────────────────────────────────

type ghIssueState struct {
	Number   int
	Title    string
	Body     string
	Labels   []string
	State    string
	Comments []string
	Locked   bool
}

type fakeGitHub struct {
	mu       sync.Mutex
	issues   []*ghIssueState
	next     int
	coreRema int
	search   int
	calls    map[string]int

	// knobs
	createStatus   int    // when > 0, POST /issues returns it
	createBody     string // the message returned with createStatus
	repoStatus     int    // when > 0, the repo probe returns it
	searchDisabled bool   // when true, GET /search/issues returns 422
	rateLimited    bool   // when true, every call returns 429 + reset
	down           bool   // when true, every call returns 503
	locked         bool
	hideCreate     bool // create succeeds but the read-back 404s
	commentsFail   int  // status for POST comments when > 0
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{next: 41, coreRema: 4821, search: 27, calls: map[string]int{}}
}

// bootProbedReset is only used by the fake: nothing caches on the server side.
func (g *fakeGitHub) bootProbedReset() {}

func (g *fakeGitHub) count(name string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls[name]
}

func (g *fakeGitHub) addIssue(body, state string) *ghIssueState {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.next++
	it := &ghIssueState{Number: g.next, Title: "seeded", Body: body, State: state}
	g.issues = append(g.issues, it)
	return it
}

func (g *fakeGitHub) issue(n int) *ghIssueState {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, it := range g.issues {
		if it.Number == n {
			return it
		}
	}
	return nil
}

// lastIssue is the most recently created issue ("" when none exists).
func (g *fakeGitHub) lastIssue() *ghIssueState {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.issues) == 0 {
		return nil
	}
	return g.issues[len(g.issues)-1]
}

func (g *fakeGitHub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.calls[r.Method+" "+pathKind(r.URL.Path)]++
		rateLimited := g.rateLimited
		down := g.down
		core := g.coreRema
		search := g.search
		g.mu.Unlock()
		if down {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"message": "backend down"})
			return
		}
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(core))
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
		if rateLimited {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
			return
		}
		switch {
		case r.URL.Path == "/rate_limit":
			writeJSON(w, 200, map[string]any{"resources": map[string]any{
				"core":   map[string]any{"limit": 5000, "remaining": core, "reset": time.Now().Add(time.Hour).Unix()},
				"search": map[string]any{"limit": 30, "remaining": search, "reset": time.Now().Add(time.Hour).Unix()},
			}})
		case strings.HasPrefix(r.URL.Path, "/search/issues"):
			g.handleSearch(w, r)
		case strings.HasPrefix(r.URL.Path, "/repos/"):
			g.handleRepo(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	return mux
}

func pathKind(p string) string {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if len(parts) >= 5 && parts[0] == "repos" && parts[3] == "issues" {
		if len(parts) >= 6 {
			return "repo/issues/{n}/comments"
		}
		return "repo/issues/{n}"
	}
	if strings.HasPrefix(p, "/search") {
		return "search"
	}
	if len(parts) >= 4 && parts[0] == "repos" && parts[3] == "issues" {
		return "repo/issues"
	}
	return p
}

func (g *fakeGitHub) handleSearch(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	disabled := g.searchDisabled
	g.mu.Unlock()
	if disabled {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "Validation Failed"})
		return
	}
	q := r.URL.Query().Get("q")
	sig := ""
	if i := strings.Index(q, "trouble:sig="); i >= 0 {
		rest := q[i+len("trouble:sig="):]
		if j := strings.IndexAny(rest, `" `); j >= 0 {
			rest = rest[:j]
		}
		sig = rest
	}
	openOnly := strings.Contains(q, "is:open")
	g.mu.Lock()
	defer g.mu.Unlock()
	items := []map[string]any{}
	for _, it := range g.issues {
		if sig != "" && !strings.Contains(it.Body, "trouble:sig="+sig) {
			continue
		}
		if openOnly && it.State != types.IssueOpen {
			continue
		}
		items = append(items, ghIssueJSON(it))
	}
	writeJSON(w, 200, map[string]any{"total_count": len(items), "items": items})
}

func (g *fakeGitHub) handleRepo(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// /repos/{owner}/{repo}
	if len(parts) == 3 {
		g.mu.Lock()
		st := g.repoStatus
		g.mu.Unlock()
		if st > 0 {
			w.WriteHeader(st)
			_, _ = w.Write([]byte(`{"message":"nope"}`))
			return
		}
		writeJSON(w, 200, map[string]any{"full_name": parts[1] + "/" + parts[2]})
		return
	}
	// /repos/{owner}/{repo}/issues/...
	rest := parts[3:]
	if rest[0] != "issues" {
		http.NotFound(w, r)
		return
	}
	if len(rest) == 1 {
		switch r.Method {
		case http.MethodPost:
			g.createIssue(w, r)
		default:
			http.NotFound(w, r)
		}
		return
	}
	n, _ := strconv.Atoi(rest[1])
	if len(rest) == 3 && rest[2] == "comments" {
		switch r.Method {
		case http.MethodGet:
			g.mu.Lock()
			it := g.findLocked(n)
			g.mu.Unlock()
			if it == nil {
				writeJSON(w, 404, map[string]any{"message": "Not Found"})
				return
			}
			out := make([]map[string]any, 0, len(it.Comments))
			for i, c := range it.Comments {
				out = append(out, map[string]any{"id": int64(i + 1), "body": c})
			}
			writeJSON(w, 200, out)
		case http.MethodPost:
			g.addComment(w, r, n)
		default:
			http.NotFound(w, r)
		}
		return
	}
	switch r.Method {
	case http.MethodGet:
		g.mu.Lock()
		it := g.findLocked(n)
		g.mu.Unlock()
		if it == nil {
			writeJSON(w, 404, map[string]any{"message": "Not Found"})
			return
		}
		writeJSON(w, 200, ghIssueJSON(it))
	case http.MethodPatch:
		g.patchIssue(w, r, n)
	default:
		http.NotFound(w, r)
	}
}

func (g *fakeGitHub) findLocked(n int) *ghIssueState {
	for _, it := range g.issues {
		if it.Number == n {
			return it
		}
	}
	return nil
}

func (g *fakeGitHub) createIssue(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		Labels []string `json:"labels"`
	}
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, &body)
	g.mu.Lock()
	st := g.createStatus
	msg := g.createBody
	hide := g.hideCreate
	g.mu.Unlock()
	if st > 0 {
		writeJSON(w, st, map[string]any{"message": msg})
		return
	}
	g.mu.Lock()
	g.next++
	it := &ghIssueState{Number: g.next, Title: body.Title, Body: body.Body, Labels: body.Labels, State: types.IssueOpen}
	if !hide {
		g.issues = append(g.issues, it)
	}
	g.mu.Unlock()
	writeJSON(w, 201, ghIssueJSON(it))
}

func (g *fakeGitHub) addComment(w http.ResponseWriter, r *http.Request, n int) {
	var body struct {
		Body string `json:"body"`
	}
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, &body)
	g.mu.Lock()
	fail := g.commentsFail
	locked := g.locked
	it := g.findLocked(n)
	if fail > 0 {
		g.mu.Unlock()
		writeJSON(w, fail, map[string]any{"message": "nope"})
		return
	}
	if it == nil {
		g.mu.Unlock()
		writeJSON(w, 404, map[string]any{"message": "Not Found"})
		return
	}
	if locked {
		g.mu.Unlock()
		writeJSON(w, 423, map[string]any{"message": "Locked"})
		return
	}
	it.Comments = append(it.Comments, body.Body)
	g.mu.Unlock()
	writeJSON(w, 201, map[string]any{"id": len(body.Body), "body": body.Body})
}

func (g *fakeGitHub) patchIssue(w http.ResponseWriter, r *http.Request, n int) {
	var body struct {
		State       string `json:"state"`
		StateReason string `json:"state_reason"`
	}
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, &body)
	g.mu.Lock()
	it := g.findLocked(n)
	locked := g.locked
	if it == nil {
		g.mu.Unlock()
		writeJSON(w, 404, map[string]any{"message": "Not Found"})
		return
	}
	if locked {
		g.mu.Unlock()
		writeJSON(w, 423, map[string]any{"message": "Locked"})
		return
	}
	it.State = body.State
	g.mu.Unlock()
	writeJSON(w, 200, ghIssueJSON(it))
}

func ghIssueJSON(it *ghIssueState) map[string]any {
	return map[string]any{
		"number": it.Number, "state": it.State, "html_url": fmt.Sprintf("https://github.com/acme/payment-api/issues/%d", it.Number),
		"title": it.Title, "body": it.Body, "comments": len(it.Comments),
		"created_at": "2026-09-16T09:14:03.221Z", "updated_at": "2026-09-16T09:15:41.009Z",
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func ghServer(t testingT, api *fakeGitHub) *httptest.Server {
	srv := httptest.NewServer(api.handler())
	t.Cleanup(srv.Close)
	return srv
}

// testingT is the slice of testing.TB the helpers use.
type testingT interface {
	Cleanup(func())
	Fatalf(format string, args ...any)
	Errorf(format string, args ...any)
	Logf(format string, args ...any)
	TempDir() string
	Setenv(key, value string)
}

// ── fake duckbrain API ──────────────────────────────────────────────────────

type fakeDuckbrain struct {
	mu     sync.Mutex
	kv     map[string]json.RawMessage
	calls  map[string]int
	header string // required header name
	value  string // required header value ("" = no auth required)

	// knobs
	down       bool // every call returns 503
	dropWrites bool // PUT returns 200 but stores nothing
	readback   bool // PUT stores a perturbed document (read-back mismatch)
}

func newFakeDuckbrain() *fakeDuckbrain {
	return &fakeDuckbrain{kv: map[string]json.RawMessage{}, calls: map[string]int{}}
}

func (f *fakeDuckbrain) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

func (f *fakeDuckbrain) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.kv))
	for k := range f.kv {
		out = append(out, k)
	}
	return out
}

func (f *fakeDuckbrain) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls[r.Method+" "+kvKind(r.URL.Path)]++
		down := f.down
		drop := f.dropWrites
		perturb := f.readback
		needH, needV := f.header, f.value
		f.mu.Unlock()
		if needH != "" && !strings.HasSuffix(r.Header.Get(needH), needV) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		if down {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "down"})
			return
		}
		switch {
		case r.URL.Path == "/health":
			writeJSON(w, 200, map[string]any{"status": "ok"})
		case r.URL.Path == "/v1/kv" && r.Method == http.MethodGet:
			prefix := r.URL.Query().Get("prefix")
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			f.mu.Lock()
			n := 0
			for k := range f.kv {
				if strings.HasPrefix(k, prefix) {
					n++
				}
			}
			f.mu.Unlock()
			if limit > 0 && n > limit {
				n = limit
			}
			writeJSON(w, 200, map[string]any{"count": n})
		case strings.HasPrefix(r.URL.Path, "/v1/kv/"):
			key := strings.TrimPrefix(r.URL.Path, "/v1/kv/")
			key = unescapePath(key)
			switch r.Method {
			case http.MethodGet:
				f.mu.Lock()
				v, ok := f.kv[key]
				f.mu.Unlock()
				if !ok {
					writeJSON(w, 404, map[string]any{"error": "not found"})
					return
				}
				writeJSON(w, 200, map[string]any{"key": key, "value": json.RawMessage(v)})
			case http.MethodPut:
				var doc dbKVDoc
				raw, _ := io.ReadAll(r.Body)
				if err := json.Unmarshal(raw, &doc); err != nil {
					writeJSON(w, 400, map[string]any{"error": "bad body"})
					return
				}
				if drop {
					writeJSON(w, 200, map[string]any{"key": key})
					return
				}
				if perturb {
					doc.Value = json.RawMessage(`{"iss":"other","idem":"different"}`)
				}
				f.mu.Lock()
				f.kv[key] = doc.Value
				f.mu.Unlock()
				writeJSON(w, 200, map[string]any{"key": key})
			default:
				http.NotFound(w, r)
			}
		default:
			http.NotFound(w, r)
		}
	})
}

func kvKind(p string) string {
	if p == "/health" {
		return "health"
	}
	if p == "/v1/kv" {
		return "list"
	}
	return "kv"
}

func unescapePath(s string) string {
	out := strings.Builder{}
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				out.WriteByte(byte(v))
				i += 2
				continue
			}
		}
		out.WriteByte(s[i])
	}
	return out.String()
}

func dbServer(t testingT, api *fakeDuckbrain) *httptest.Server {
	srv := httptest.NewServer(api.handler())
	t.Cleanup(srv.Close)
	return srv
}

// ── shared fixtures ─────────────────────────────────────────────────────────

const testSig = "sentinel:sha256v1:9f2c1d3e4b5a6c7d"

func testIncident() types.Incident {
	return types.Incident{
		ID:        "inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE",
		Sig:       testSig,
		GroupID:   "grp_01J9Z6Q0M2X4T8V1K7B3N5R8WH",
		State:     types.StVerifying,
		EntryRung: types.RungPlay,
		Rung:      types.RungOutlets,
		Severity:  types.SevHigh,
		OpenedTS:  "2026-09-16T09:14:03.221Z",
		UpdatedTS: "2026-09-16T09:15:41.009Z",
	}
}

func testEvidence() Evidence {
	return Evidence{
		Summary: "queue wedge in payment-worker",
		Source:  "sentinel:payment-worker",
		Group: types.Group{
			ID:           "grp_01J9Z6Q0M2X4T8V1K7B3N5R8WH",
			Sig:          testSig,
			Digest:       "9f2c1d3e4b5a6c7d9f2c1d3e4b5a6c7d9f2c1d3e4b5a6c7d9f2c1d3e4b5a6c7d",
			Source:       types.SrcSentinel,
			Title:        "queue wedge in payment-worker",
			FirstSeenTS:  "2026-09-16T09:14:03.221Z",
			LastSeenTS:   "2026-09-16T09:15:41.009Z",
			Count:        42,
			Counters:     types.GroupCounters{Events: 12, Suppressed: 1, Dropped: 0},
			ReleaseRange: []string{"payment-api@2.4.1", "payment-api@2.4.3"},
		},
		Ladder:     []string{"detected", "recorded", "play:applied", "verifying"},
		Lines:      []string{"worker.py:118 claim", "  queue.py:44 get", "journal: payment-worker[8841]: pool exhausted (retry 3/5)"},
		Redactions: 3,
	}
}

func testActor() types.Actor {
	return types.Actor{Kind: types.ActorDaemon, ID: "troubled", Version: "0.1.0", GitSHA: "9c1f0ab"}
}

// githubTestConfig is the §3.4 github block pointed at a test server.
// testTokenEnv is the token environment variable the suite uses; the default
// TROUBLE_GITHUB_TOKEN is left untouched so a developer's real shell cannot leak
// into the tests.
const testTokenEnv = "TROUBLE_ISSUES_TEST_TOKEN"

func githubTestConfig(base string) types.IssueDriverConfig {
	c := DefaultGitHubConfig()
	c.APIBase = base
	c.Owner = "acme"
	c.Repo = "payment-api"
	c.TokenEnv = testTokenEnv // set by the helpers with t.Setenv
	c.TokenFile = ""          // no file: the tests inject the token through the env
	c.SearchMinInterval = "1ms"
	c.MaxAttempts = 3
	c.BaseBackoff = "1ms"
	c.MaxBackoff = "5ms"
	c.MinRemaining = 0
	return c
}

func duckbrainTestConfig(base string) types.IssueDriverConfig {
	c := DefaultDuckbrainConfig()
	c.BaseURL = base
	c.APIKeyFile = ""
	c.APIKeyEnv = ""
	c.MaxAttempts = 3
	c.BaseBackoff = "1ms"
	c.MaxBackoff = "5ms"
	return c
}

// enabledDefaults is the config the desk unit tests exercise: the package
// defaults with the desk turned ON. SPEC-09 §3.4a ships the compiled default OFF
// (no deployment value is compiled in), so a test that wants a filing desk has
// to say so — and the shipped-default posture has its own test in
// config_default_test.go, which is where "OFF" is pinned.
func enabledDefaults() types.IssueDeskConfig {
	c := DefaultConfig()
	c.Enabled = true
	return c
}

// deskWith builds a desk over one driver and returns it with its doubles.
func deskWith(t testingT, cfgs ...types.IssueDriverConfig) (*Desk, *fakeLedger, *fakeClock, *fakeScrubber) {
	clk := newFakeClock()
	led := &fakeLedger{now: clk.Now}
	sc := &fakeScrubber{}
	cfg := enabledDefaults()
	cfg.Drivers = cfgs
	if len(cfgs) > 0 {
		cfg.PrimaryDriver = cfgs[0].Name
	}
	for _, c := range cfgs {
		if c.Primary {
			cfg.PrimaryDriver = c.Name
		}
	}
	cfg.HealthcheckEvery = "60s"
	cfg.HealthcheckIdle = "5m"
	cfg.OpDeadline = "2s"
	t.Setenv(testTokenEnv, "ghp_test_token_value")
	d, err := New(cfg, Deps{
		Ledger:    led,
		Scan:      led,
		Scrub:     sc,
		Clock:     clk,
		HostID:    "7f3a91c2d4e5b607",
		Actor:     testActor(),
		StateRoot: t.TempDir(),
		ProjectOf: func(inc types.Incident) string { return "1" },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d, led, clk, sc
}
