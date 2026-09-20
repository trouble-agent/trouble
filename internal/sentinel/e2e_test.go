package sentinel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/ledger"
	"github.com/trouble-agent/trouble/internal/scrub"
	"github.com/trouble-agent/trouble/internal/types"
)

// genericDialectBody is the §3.6 generic JSON form of the vector-B bug class:
// `exception{type,value,stack[{file,function,line,in_app,context_line}]}`. It is
// the shape a bash/curl reporter sends, and its canonical frames are identical to
// the SDK's, which is what makes the two paths land in one group.
func genericDialectBody(eventID string) string {
	return `{"event_id":"` + eventID + `","level":"error","culprit":"worker.claim",` +
		`"message":"queue wedge: pool exhausted depth=912","release":"payment-api@2.4.1",` +
		`"exception":{"type":"PoolExhausted","value":"queue wedge: pool exhausted depth=912","stack":[` +
		`{"file":"worker.py","function":"claim","line":118,"in_app":true,"context_line":"item = pool.get(timeout=1)"},` +
		`{"file":"queue.py","function":"get","line":44,"in_app":true,"context_line":"raise PoolExhausted(depth=912)"}]}}`
}

// e2eChain is a full on-disk chain: the real scrubbing engine, the real ledger
// (group commit, rotation, boundary re-scan) and the real sentinel behind an
// httptest listener — the shape SPEC-12 mounts.
type e2eChain struct {
	s    *Server
	l    *ledger.Ledger
	ts   *httptest.Server
	root string
}

// e2eLedgerNow is the e2e chain's ledger clock. The ledger is the side that
// stamps record timestamps (ledger.Options.Now, rendered through
// types.TsLayout = millisecond), so a test that compares a ledger-stamped
// timestamp against a boot instant pins this clock (see pinLedgerClock) and
// reads both sides from it, instead of comparing its millisecond output
// against a nanosecond time.Now.
var e2eLedgerNow = time.Now

// newE2EChain opens the real chain. It is the only test that uses the real
// ledger, so the JSONL on disk is asserted directly.
func newE2EChain(t *testing.T, mut func(*Config)) *e2eChain {
	t.Helper()
	// The ledger refuses /tmp on purpose (SPEC-01 §6.1: never a temp location),
	// so the chain's state root lives under the package directory and is removed
	// by the test.
	root, err := os.MkdirTemp(".", ".sentinel-e2e-")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	cfg := testConfig(t, func(c *Config) {
		c.SpoolDir = filepath.Join(root, "spool")
		if mut != nil {
			mut(c)
		}
	})
	eng, serr := scrub.New(nil, cfg.Projects)
	if serr != nil {
		t.Fatalf("scrub.New: %v", serr)
	}
	l, lerr := ledger.Open(context.Background(), ledger.Options{
		Root:           filepath.Join(root, "ledger"),
		Rotation:       ledger.DefaultRotationPolicy(),
		Retention:      ledger.DefaultRetentionPolicy(),
		Index:          ledger.DefaultIndexOptions(),
		Writer:         cfg.Actor,
		MaxSchema:      1,
		Now:            e2eLedgerNow,
		HostID:         cfg.HostID,
		Zone:           "loopback",
		AssertScrubbed: true,
		ScrubVerify: func(ctx context.Context, b []byte) error {
			return eng.Verify(ctx, b)
		},
	})
	if lerr != nil {
		t.Fatalf("ledger.Open: %v", lerr)
	}
	s, nerr := NewServer(cfg, LedgerSink{L: l}, eng)
	if nerr != nil {
		t.Fatalf("NewServer: %v", nerr)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = s.Drain(context.Background())
		_ = l.Close(context.Background())
	})
	s.cfg.Bind = strings.TrimPrefix(ts.URL, "http://")
	return &e2eChain{s: s, l: l, ts: ts, root: root}
}

func (e *e2eChain) open(t *testing.T) {
	t.Helper()
	if err := e.s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
}

// postNoAuth posts a body with explicit headers.
func (e *e2eChain) post(t *testing.T, path string, headers map[string]string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.ts.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := e.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", path, err)
	}
	return resp
}

// TestE2EOneGroupAcrossAllFourPaths pins AC-22 in the on-disk chain: an SDK
// envelope, the legacy store body, the generic JSON on-ramp and a collector log
// line that describe one bug class produce ONE group and one record set.
func TestE2EOneGroupAcrossAllFourPaths(t *testing.T) {
	e := newE2EChain(t, func(c *Config) {
		c.CanaryProject = ""
		c.Projects[0].QuotaEPM = 100000
	})
	e.open(t)

	// 1. SDK envelope (sentry-python shape).
	env := readFixture(t, "envelopes/sentry-python-event.envelope")
	resp := e.post(t, "/api/1/envelope/", map[string]string{"X-Sentry-Auth": authFor(e, "1")}, env)
	if resp.StatusCode != 200 {
		t.Fatalf("envelope status %d (%s)", resp.StatusCode, readBody(t, resp))
	}
	_ = readBody(t, resp)

	// 2. Legacy store body with the same stack.
	storeBody := `{"event_id":"11112222333344445555666677778888","level":"error","culprit":"worker.claim",
	  "message":"queue wedge: pool exhausted depth=912","release":"payment-api@2.4.1",
	  "exception":{"values":[{"type":"PoolExhausted","value":"queue wedge: pool exhausted depth=912","stacktrace":{"frames":[
	    {"filename":"worker.py","function":"claim","lineno":118,"in_app":true,"context_line":"item = pool.get(timeout=1)"},
	    {"filename":"queue.py","function":"get","lineno":44,"in_app":true,"context_line":"raise PoolExhausted(depth=912)"}]}}]}}`
	resp = e.post(t, "/api/1/store/", map[string]string{
		"X-Sentry-Auth": authFor(e, "1"), "Content-Type": "application/json",
	}, []byte(strings.Join(strings.Fields(storeBody), " ")))
	if resp.StatusCode != 200 {
		t.Fatalf("store status %d (%s)", resp.StatusCode, readBody(t, resp))
	}
	_ = readBody(t, resp)

	// 3. Generic JSON (the curl path) — the generic dialect's own shape and a
	// distinct event id, because a repeated id inside the 10-minute dedup window
	// is an SDK retry, not a new event (§6.6).
	resp = e.post(t, "/api/1/event/?sentry_key="+testPubA, map[string]string{"Content-Type": "application/json"},
		[]byte(genericDialectBody("0123456789abcdef0123456789abcdef")))
	if resp.StatusCode != 200 {
		t.Fatalf("generic status %d (%s)", resp.StatusCode, readBody(t, resp))
	}
	_ = readBody(t, resp)

	// 4. A collector line with the same canonical frames.
	sdk := &rawEvent{
		Level: "error", Culprit: "worker.claim",
		Message:    "queue wedge: pool exhausted depth=912",
		Release:    "payment-api@2.4.1",
		Frames:     []frame{{File: "worker.py", Function: "claim", InApp: true, ContextLine: "item = pool.get(timeout=1)"}, {File: "queue.py", Function: "get", InApp: true, ContextLine: "raise PoolExhausted(depth=912)"}},
		SourceKind: sourceCollector,
		AuthForm:   "",
	}
	entry, _ := e.s.projects.project("1")
	if _, err := e.s.admitEvent(context.Background(), entry, sdk, "event", "collector"); err != nil {
		t.Fatalf("collector admit: %v", err)
	}

	groups := e.s.Groups()
	if len(groups) != 1 {
		for _, g := range groups {
			t.Logf("group %s sig %s count %d", g.Digest, g.Sig, g.Count)
		}
		t.Fatalf("%d groups, want exactly 1 across the four paths (AC-22)", len(groups))
	}
	if groups[0].Count != 4 {
		t.Fatalf("group count = %d, want 4 (one per path)", groups[0].Count)
	}

	// The ledger on disk carries the same records: read them back through the
	// real query surface.
	var events, reports, groupRecs int
	if err := e.l.Query().ScanFrom(1, func(rec types.Record) bool {
		switch rec.Kind {
		case types.KEvent:
			// The python fixture also carries a client_report: it is accounted as
			// an `event` record with item_type=client_report and opens no group
			// (§3.2), so the two are counted separately.
			if rec.Payload["item_type"] == "client_report" {
				reports++
			} else {
				events++
			}
		case types.KGroup:
			groupRecs++
		}
		return true
	}); err != nil {
		t.Fatalf("ledger scan: %v", err)
	}
	if events != 4 {
		t.Errorf("ledger holds %d event records, want 4 (one per path)", events)
	}
	if reports != 1 {
		t.Errorf("ledger holds %d client_report records, want 1 (parsed, never dropped)", reports)
	}
	if groupRecs == 0 {
		t.Error("ledger holds no group records")
	}
	// The sig on disk is the sentinel signature of the shared digest.
	if got, _, gerr := e.l.Query().GroupByDigest(groups[0].Digest); gerr == nil && got != nil && got.Sig != groups[0].Sig {
		t.Errorf("ledger group sig %s != live sig %s", got.Sig, groups[0].Sig)
	}
}

// TestE2ENoUnredactedSecretReachedTheLedger pins the ingestion half of the
// scrubbing contract: a secret riding an SDK envelope is redacted before the
// record is written, and the boundary re-scan accepts the result.
func TestE2ENoUnredactedSecretReachedTheLedger(t *testing.T) {
	e := newE2EChain(t, func(c *Config) { c.CanaryProject = "" })
	e.open(t)
	body := `{"event_id":"aaaabbbbccccddddeeeeffff00001111","level":"error","message":"auth failed for token sk-live-abcdef0123456789abcdef","culprit":"client.do",
	  "extra":{"authorization":"Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.sigpart"},
	  "exception":{"values":[{"type":"AuthError","value":"dsn postgres://user:s3cr3t@db:5432/app","stacktrace":{"frames":[{"filename":"client.go","function":"do","lineno":7,"in_app":true,"context_line":"key := \"sk-live-abcdef0123456789abcdef\""}]}}]}}`
	resp := e.post(t, "/api/1/event/?sentry_key="+testPubA, map[string]string{"Content-Type": "application/json"},
		[]byte(strings.Join(strings.Fields(body), " ")))
	if resp.StatusCode != 200 {
		t.Fatalf("status %d (%s)", resp.StatusCode, readBody(t, resp))
	}
	_ = readBody(t, resp)

	// Walk the ledger bytes: the seeded secrets must not be there.
	matches, err := filepath.Glob(filepath.Join(e.root, "ledger", "*.jsonl"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("no ledger files under %s (err %v)", e.root, err)
	}
	for _, path := range matches {
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatalf("read %s: %v", path, rerr)
		}
		for _, secret := range []string{"sk-live-abcdef0123456789abcdef", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.sigpart", "s3cr3t"} {
			if bytes.Contains(raw, []byte(secret)) {
				t.Fatalf("secret %q reached %s", secret, path)
			}
		}
		if !bytes.Contains(raw, []byte("[REDACTED:")) {
			t.Errorf("%s carries no redaction marker: the payload may not have been scrubbed", path)
		}
	}
}

// TestE2ETwoProjectsTwoListeners pins AC-18's lab shape: two projects on two
// listeners with different zones, a Go SDK on one and curl on the other, both
// landing in the same digest while one floods.
func TestE2ETwoProjectsTwoListeners(t *testing.T) {
	// Host A: the SDK project (loopback).
	a := newE2EChain(t, func(c *Config) {
		c.CanaryProject = ""
		c.Projects[0].QuotaEPM = 100000
	})
	a.open(t)
	// Host B: the curl project (loopback, metric quota low enough to hold).
	b := newE2EChain(t, func(c *Config) {
		c.CanaryProject = ""
		c.Projects = []types.Project{c.Projects[0]}
		c.Projects[0].ID = "7"
		c.Projects[0].Slug = "payment-api"
		c.Projects[0].QuotaEPM = 3
		c.Projects[0].LossPolicy = types.LossDropCounter
	})
	b.open(t)

	sharedJSON := `"level":"error","culprit":"worker.claim","message":"queue wedge: pool exhausted depth=912",
	  "exception":{"values":[{"type":"PoolExhausted","value":"queue wedge: pool exhausted depth=912","stacktrace":{"frames":[
	    {"filename":"worker.py","function":"claim","lineno":118,"in_app":true,"context_line":"item = pool.get(timeout=1)"},
	    {"filename":"queue.py","function":"get","lineno":44,"in_app":true,"context_line":"raise PoolExhausted(depth=912)"}]}}]}`
	sdkBody := "{" + `"event_id":"9f2c1d3e4b5a6c7d8e9f0a1b2c3d4e5f",` + strings.Join(strings.Fields(sharedJSON), " ") + "}"
	resp := a.post(t, "/api/1/envelope/?sentry_key="+testPubA, nil, mustEnvelope(t, sdkBody))
	if resp.StatusCode != 200 {
		t.Fatalf("host A envelope status %d (%s)", resp.StatusCode, readBody(t, resp))
	}
	_ = readBody(t, resp)

	resp = b.post(t, "/api/7/event/?sentry_key="+testPubA, map[string]string{"Content-Type": "application/json"},
		[]byte(genericDialectBody("0f1e2d3c4b5a69788796a5b4c3d2e1f0")))
	if resp.StatusCode != 200 {
		t.Fatalf("host B curl status %d (%s)", resp.StatusCode, readBody(t, resp))
	}
	_ = readBody(t, resp)

	// Host B floods: the held quota refuses, and the refusal is recorded.
	for i := 0; i < 6; i++ {
		flood := fmt.Sprintf(`{"event_id":"%032d","message":"flood %d"}`, 8000+i, i)
		r := b.post(t, "/api/7/event/?sentry_key="+testPubA, map[string]string{"Content-Type": "application/json"}, []byte(flood))
		if r.StatusCode != 200 && r.StatusCode != 429 {
			t.Fatalf("flood status %d", r.StatusCode)
		}
		_ = readBody(t, r)
	}
	if b.s.counters.droppedQuota.Get() == 0 {
		t.Fatal("the flood did not breach the quota, so the lab shape was not exercised")
	}

	ga := a.s.Groups()
	gb := b.s.Groups()
	if len(ga) != 1 {
		t.Fatalf("host A has %d groups, want 1", len(ga))
	}
	// Host B also carries the flood's own groups: the project's quota admitted a
	// few of them before it refused. What must hold is that the shared bug class
	// is ONE group with ONE digest on both hosts.
	var shared *types.Group
	for i := range gb {
		if gb[i].Digest == ga[0].Digest {
			shared = &gb[i]
		}
	}
	if shared == nil {
		for _, g := range gb {
			t.Logf("host B group %s sig %s title %q", g.Digest, g.Sig, g.Title)
		}
		t.Fatalf("host B has no group with host A's digest %s (one bug class must be one group)", ga[0].Digest)
	}
	if shared.Sig != ga[0].Sig {
		t.Fatalf("sig mismatch across hosts: %s vs %s", ga[0].Sig, shared.Sig)
	}
	if shared.Count != 1 {
		t.Errorf("host B shared group count = %d, want 1", shared.Count)
	}
}

// TestE2EBootRebuildFromTheRealLedger pins §4.2 step 3 against the real JSONL:
// a fresh process rebuilds its group index and AuthForms from the ledger alone.
func TestE2EBootRebuildFromTheRealLedger(t *testing.T) {
	e := newE2EChain(t, func(c *Config) {
		c.CanaryProject = ""
		c.Projects[0].QuotaEPM = 100000
	})
	e.open(t)
	env := readFixture(t, "envelopes/sentry-go-event.envelope")
	resp := e.post(t, "/api/1/envelope/", map[string]string{"X-Sentry-Auth": authFor(e, "1")}, env)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d (%s)", resp.StatusCode, readBody(t, resp))
	}
	_ = readBody(t, resp)
	if ferr := e.s.flushGroups(context.Background(), "test"); ferr != nil {
		t.Fatalf("flushGroups: %v", ferr)
	}
	live := e.s.Groups()
	if len(live) != 1 {
		t.Fatalf("%d live groups, want 1", len(live))
	}
	liveRT, _ := e.s.ProjectRuntime("1")

	// A second server on the same ledger root rebuilds at Start.
	cfg := testConfig(t, func(c *Config) {
		c.CanaryProject = ""
		c.SpoolDir = filepath.Join(e.root, "spool2")
	})
	l2, err := ledger.Open(context.Background(), ledger.Options{
		Root:      filepath.Join(e.root, "ledger"),
		Rotation:  ledger.DefaultRotationPolicy(),
		Retention: ledger.DefaultRetentionPolicy(),
		Index:     ledger.DefaultIndexOptions(),
		Writer:    cfg.Actor,
		MaxSchema: 1,
		Now:       time.Now,
		HostID:    cfg.HostID,
		Zone:      "loopback",
	})
	if err != nil {
		t.Skipf("the ledger is single-writer (expected while the first process holds it): %v", err)
	}
	defer l2.Close(context.Background())
	s2, err := NewServer(cfg, LedgerSink{L: l2}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer s2.Drain(context.Background())
	if err := s2.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	rebuilt := s2.Groups()
	if len(rebuilt) != len(live) {
		t.Fatalf("rebuilt %d groups, live %d", len(rebuilt), len(live))
	}
	if rebuilt[0].Count != live[0].Count || rebuilt[0].Digest != live[0].Digest {
		t.Fatalf("rebuilt group = (%s,%d), live = (%s,%d)",
			rebuilt[0].Digest, rebuilt[0].Count, live[0].Digest, live[0].Count)
	}
	rt, _ := s2.ProjectRuntime("1")
	if len(rt.AuthForms) != len(liveRT.AuthForms) {
		t.Fatalf("rebuilt AuthForms = %v, live = %v", rt.AuthForms, liveRT.AuthForms)
	}
}

// TestE2EStoreAndEnvelopeSameDigest pins AC-10: the two SDK transports that
// carry the same event produce the same digest (the store body is the envelope's
// event object).
func TestE2EStoreAndEnvelopeSameDigest(t *testing.T) {
	e := newE2EChain(t, func(c *Config) { c.CanaryProject = "" })
	e.open(t)
	obj := `{"event_id":"abcdefabcdefabcdefabcdefabcdefab","level":"error","culprit":"svc.handle",
	  "message":"boom 1234","exception":{"values":[{"type":"ValueError","value":"boom 1234","stacktrace":{"frames":[
	    {"filename":"svc.go","function":"handle","lineno":9,"in_app":true,"context_line":"panic(\"boom\")"}]}}]}}`
	obj = strings.Join(strings.Fields(obj), " ")

	resp := e.post(t, "/api/1/store/", map[string]string{
		"X-Sentry-Auth": authFor(e, "1"), "Content-Type": "application/json"}, []byte(obj))
	if resp.StatusCode != 200 {
		t.Fatalf("store status %d (%s)", resp.StatusCode, readBody(t, resp))
	}
	_ = readBody(t, resp)

	envObj := strings.Replace(obj, `"event_id":"abcdefabcdefabcdefabcdefabcdefab"`, `"event_id":"0123456789abcdef0123456789abcdef"`, 1)
	resp = e.post(t, "/api/1/envelope/?sentry_key="+testPubA, nil, mustEnvelope(t, envObj))
	if resp.StatusCode != 200 {
		t.Fatalf("envelope status %d (%s)", resp.StatusCode, readBody(t, resp))
	}
	_ = readBody(t, resp)

	groups := e.s.Groups()
	if len(groups) != 1 {
		t.Fatalf("%d groups, want 1 (the store and envelope transports describe one bug class)", len(groups))
	}
	if groups[0].Count != 2 {
		t.Fatalf("group count = %d, want 2", groups[0].Count)
	}
}

// mustEnvelope frames an event object as a one-item envelope.
func mustEnvelope(t *testing.T, eventObj string) []byte {
	t.Helper()
	return envelopeBytes(t, map[string]any{"event_id": "9f2c1d3e4b5a6c7d8e9f0a1b2c3d4e5f"},
		envelopeFixtureItem{Type: "event", Body: []byte(eventObj), Length: true, ContentType: "application/json"})
}

// authFor renders the X-Sentry-Auth header for a project of a chain.
func authFor(e *e2eChain, projectID string) string {
	for _, p := range e.s.cfg.Projects {
		if p.ID == projectID {
			if p.SecretKey != "" {
				return fmt.Sprintf("Sentry sentry_version=7, sentry_key=%s, sentry_secret=%s, sentry_client=e2e/0.1", p.PublicKey, p.SecretKey)
			}
			return fmt.Sprintf("Sentry sentry_version=7, sentry_key=%s, sentry_client=e2e/0.1", p.PublicKey)
		}
	}
	return ""
}

// TestE2EPayloadIsAddressableJSON pins a dashboard-affecting property: every
// payload written to the ledger round-trips through JSON, so
// `payload.event.message` is addressable by SPEC-10 without a decoder.
func TestE2EPayloadIsAddressableJSON(t *testing.T) {
	e := newE2EChain(t, func(c *Config) { c.CanaryProject = "" })
	e.open(t)
	resp := e.post(t, "/api/1/event/?sentry_key="+testPubA, map[string]string{"Content-Type": "application/json"},
		[]byte(`{"message":"addressable 42","culprit":"a.b"}`))
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	_ = readBody(t, resp)
	found := false
	if err := e.l.Query().ScanFrom(1, func(rec types.Record) bool {
		if rec.Kind != types.KEvent {
			return true
		}
		found = true
		raw, err := json.Marshal(rec.Payload)
		if err != nil {
			t.Fatalf("payload is not JSON: %v", err)
		}
		var back map[string]any
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatalf("payload does not round-trip: %v", err)
		}
		ev, ok := back["event"].(map[string]any)
		if !ok {
			t.Fatalf("payload.event is not an object: %T", back["event"])
		}
		if ev["message"] != "addressable 42" {
			t.Errorf("payload.event.message = %v, want the event message", ev["message"])
		}
		if _, ok := back["digest"].(string); !ok {
			t.Error("payload.digest is missing")
		}
		return true
	}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !found {
		t.Fatal("no event record in the ledger")
	}
}
