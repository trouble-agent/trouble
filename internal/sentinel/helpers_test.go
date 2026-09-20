package sentinel

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/scrub"
	"github.com/trouble-agent/trouble/internal/types"
)

// Test keys (32 lowercase hex, the only form §2.3 accepts).
const (
	testPubA    = "a1b2c3d4e5f60718293a4b5c6d7e8f90"
	testPubB    = "0f1e2d3c4b5a69788796a5b4c3d2e1f0"
	testSecretA = "00112233445566778899aabbccddeeff"
)

// memSink is an in-memory ledgerSink + ledgerReader. The tests that need the
// real durability guarantees run against ledger.Ledger in e2e_test.go; the rest
// use this so a test can inspect the exact record stream without a filesystem.
type memSink struct {
	mu    sync.Mutex
	recs  []types.Record
	seq   uint64
	fail  error
	delay time.Duration
}

func (m *memSink) Append(ctx context.Context, d types.RecordDraft) (types.Record, error) {
	// delay and fail are read under the lock so the tests that flip them mid-run
	// (backpressure, sink failure) are race-free.
	m.mu.Lock()
	delay, fail := m.delay, m.fail
	m.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return types.Record{}, ctx.Err()
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if fail != nil {
		return types.Record{}, fail
	}
	m.seq++
	rec := types.Record{
		Seq: m.seq, RecID: fmt.Sprintf("rec_%d", m.seq),
		TS: types.FormatUTC(nowFunc()), Kind: d.Kind, SchemaVersion: 1,
		Sig: d.Sig, Inc: d.Inc, Origin: d.Origin, Actor: d.Actor,
		Redactions: d.Redactions, Payload: d.Payload,
	}
	m.recs = append(m.recs, rec)
	return rec, nil
}

func (m *memSink) LastSeq() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.seq
}

func (m *memSink) ScanFrom(seq uint64, yield func(types.Record) bool) error {
	m.mu.Lock()
	recs := append([]types.Record(nil), m.recs...)
	m.mu.Unlock()
	for _, r := range recs {
		if r.Seq < seq {
			continue
		}
		if !yield(r) {
			return nil
		}
	}
	return nil
}

// records of one kind.
// setDelay sets the sink's simulated write latency (races-free with Append).
func (m *memSink) setDelay(d time.Duration) {
	m.mu.Lock()
	m.delay = d
	m.mu.Unlock()
}

func (m *memSink) ofKind(kind types.RecordKind) []types.Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []types.Record
	for _, r := range m.recs {
		if r.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}

func (m *memSink) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.recs)
}

// testProjects is the two-project fixture the AC-18 lab shape uses.
func testProjects() []types.Project {
	return []types.Project{
		{
			ID: "1", Slug: "alpha", PublicKey: testPubA, SecretKey: testSecretA,
			QuotaEPM: 600, Enabled: true, LossPolicy: types.LossDropCounter,
			DiskBudget: 1 << 31,
		},
		{
			ID: "2", Slug: "beta", PublicKey: testPubB,
			QuotaEPM: 600, Enabled: true, LossPolicy: types.LossDropCounter,
		},
	}
}

// testConfig is the minimal valid config: a name for advertised_host (never an
// IP), a state root for the spool, and test actor identity.
func testConfig(t *testing.T, mut func(*Config)) Config {
	t.Helper()
	cfg := Config{
		Bind:           "127.0.0.1:7643",
		AdvertisedHost: "trouble.example.net",
		Scheme:         "http",
		ProxyTrust:     proxyTrustLoopback,
		CanaryProject:  "1",
		Projects:       testProjects(),
		SpoolDir:       t.TempDir(),
		HostID:         "7f3a91c2d4e5b607",
		Actor:          types.Actor{Kind: types.ActorDaemon, ID: "troubled", Version: "0.1.0"},
	}
	if mut != nil {
		mut(&cfg)
	}
	return cfg
}

// newTestScrubber builds the real SPEC-02 engine with the default table.
func newTestScrubber(t *testing.T, projects []types.Project) *scrub.Engine {
	t.Helper()
	eng, err := scrub.New(nil, projects)
	if err != nil {
		t.Fatalf("scrub.New: %v", err)
	}
	return eng
}

// testServer bundles a server, its sink and the HTTP listener that mounts
// Handler() the way SPEC-12 does.
type testServer struct {
	s    *Server
	sink *memSink
	ts   *httptest.Server
}

// newTestServer builds a server and a listener in front of Handler().
func newTestServer(t *testing.T, mut func(*Config)) *testServer {
	t.Helper()
	t.Setenv("TZ", "UTC")
	cfg := testConfig(t, mut)
	sink := &memSink{}
	s, err := NewServer(cfg, sink, newTestScrubber(t, cfg.Projects))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	// The canary posts to the configured bind address (that is the daemon's own
	// listener in production); point it at the test listener.
	s.cfg.Bind = strings.TrimPrefix(ts.URL, "http://")
	return &testServer{s: s, sink: sink, ts: ts}
}

func (t *testServer) close() {
	if t.s.spool != nil {
		_ = t.s.spool.Close()
	}
}

// newLoopbackServer mounts a handler on a loopback listener (the load test drives
// it with its own pooled client).
func newLoopbackServer(tb testing.TB, h http.Handler) *httptest.Server {
	tb.Helper()
	return httptest.NewServer(h)
}

// dsnFor renders a project's DSN against the test listener.
func (t *testServer) dsnFor(projectID string) string {
	for _, p := range t.s.cfg.Projects {
		if p.ID == projectID {
			d := DSN{Scheme: "http", PublicKey: p.PublicKey, Host: "127.0.0.1", ProjectID: p.ID}
			if host, port := parseHostPort(t.s.cfg.Bind); port != "" {
				d.Host = host
				d.Port = port
			}
			return d.String()
		}
	}
	return ""
}

func (t *testServer) keyFor(projectID string) string {
	for _, p := range t.s.cfg.Projects {
		if p.ID == projectID {
			return p.PublicKey
		}
	}
	return ""
}

// post sends a request with the given headers and body.
func (t *testServer) post(tb testing.TB, path string, headers map[string]string, body []byte) *http.Response {
	tb.Helper()
	req, err := http.NewRequest(http.MethodPost, t.ts.URL+path, bytes.NewReader(body))
	if err != nil {
		tb.Fatalf("new request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := t.ts.Client().Do(req)
	if err != nil {
		tb.Fatalf("post %s: %v", path, err)
	}
	return resp
}

// authHeader is the X-Sentry-Auth form for project 1.
func (t *testServer) authHeader(projectID string) string {
	for _, p := range t.s.cfg.Projects {
		if p.ID == projectID {
			if p.SecretKey != "" {
				return fmt.Sprintf("Sentry sentry_version=7, sentry_key=%s, sentry_secret=%s, sentry_client=trouble-test/0.1", p.PublicKey, p.SecretKey)
			}
			return fmt.Sprintf("Sentry sentry_version=7, sentry_key=%s, sentry_client=trouble-test/0.1", p.PublicKey)
		}
	}
	return ""
}

// readBody reads and closes a response body.
func readBody(tb testing.TB, resp *http.Response) []byte {
	tb.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		tb.Fatalf("read body: %v", err)
	}
	return b
}

// respID decodes the pinned `{"id":"…"}` success body.
func respID(tb testing.TB, resp *http.Response) string {
	tb.Helper()
	var body struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(readBody(tb, resp), &body); err != nil {
		tb.Fatalf("success body is not {\"id\":…}: %v", err)
	}
	return body.ID
}

// firstCause decodes the first cause of a pinned error body
// (`{"causes":[...],"detail":"…"}`).
func firstCause(tb testing.TB, body []byte) string {
	tb.Helper()
	var out struct {
		Causes []string `json:"causes"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		tb.Fatalf("error body is not JSON: %v (%s)", err, body)
	}
	if len(out.Causes) == 0 {
		return ""
	}
	return out.Causes[0]
}

// gzipBytes compresses b.
func gzipBytes(tb testing.TB, b []byte) []byte {
	tb.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		tb.Fatalf("gzip: %v", err)
	}
	if err := zw.Close(); err != nil {
		tb.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// envelopeBytes frames one header line plus one item with an explicit length.
func envelopeBytes(tb testing.TB, header map[string]any, items ...envelopeFixtureItem) []byte {
	tb.Helper()
	var buf bytes.Buffer
	hb, err := json.Marshal(header)
	if err != nil {
		tb.Fatalf("marshal header: %v", err)
	}
	buf.Write(hb)
	buf.WriteByte('\n')
	for _, it := range items {
		ih := map[string]any{"type": it.Type}
		if it.Length {
			ih["length"] = len(it.Body)
		}
		if it.ContentType != "" {
			ih["content_type"] = it.ContentType
		}
		ib, merr := json.Marshal(ih)
		if merr != nil {
			tb.Fatalf("marshal item header: %v", merr)
		}
		buf.Write(ib)
		buf.WriteByte('\n')
		buf.Write(it.Body)
		if it.Length {
			buf.WriteByte('\n')
		}
	}
	return buf.Bytes()
}

type envelopeFixtureItem struct {
	Type        string
	Body        []byte
	Length      bool
	ContentType string
}

// eventJSON builds a minimal SDK-shaped event object.
func eventJSON(tb testing.TB, mut func(map[string]any)) []byte {
	tb.Helper()
	obj := map[string]any{
		"event_id":    "9f2c1d3e4b5a6c7d8e9f0a1b2c3d4e5f",
		"level":       "error",
		"message":     "queue wedge: pool exhausted",
		"culprit":     "worker.claim",
		"release":     "payment-api@2.4.1",
		"environment": "prod",
		"platform":    "python",
		"exception": map[string]any{
			"values": []any{map[string]any{
				"type":  "PoolExhausted",
				"value": "queue wedge: pool exhausted depth=912",
				"stacktrace": map[string]any{"frames": []any{
					map[string]any{"filename": "worker.py", "function": "claim", "lineno": 118,
						"in_app": true, "context_line": "item = pool.get(timeout=1)"},
					map[string]any{"filename": "queue.py", "function": "get", "lineno": 44,
						"in_app": true, "context_line": "raise PoolExhausted(depth=912)"},
				}},
			}},
		},
	}
	if mut != nil {
		mut(obj)
	}
	b, err := json.Marshal(obj)
	if err != nil {
		tb.Fatalf("marshal event: %v", err)
	}
	return b
}

// readFixture loads a testdata file.
func readFixture(tb testing.TB, name string) []byte {
	tb.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		tb.Fatalf("fixture %s: %v", name, err)
	}
	return b
}

// writeFixture writes a testdata file (fixtures are checked in; this exists for
// the golden-file regeneration path).
func writeFixture(tb testing.TB, name string, b []byte) {
	tb.Helper()
	path := filepath.Join("testdata", name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		tb.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		tb.Fatalf("write fixture: %v", err)
	}
}

// feedLines pushes assembled log lines through the collector dispatcher (the
// synthetic source the collector tests use instead of journalctl).
func (t *testServer) feedLines(source string, lines ...string) *rawEvent {
	for _, l := range lines {
		t.s.collectors.feed(logLine{Text: l, TS: nowFunc(), Source: source})
	}
	t.s.collectors.flushExpired(nowFunc().Add(time.Hour))
	return nil
}
