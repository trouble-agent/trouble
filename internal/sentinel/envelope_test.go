package sentinel

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// TestEnvelopeFixturesDecode pins the §7 requirement: every fixture in
// testdata/envelopes/ (the sentry-go, sentry-python and @sentry/node wire shapes)
// decodes with 0 panics and produces the expected item accounting.
func TestEnvelopeFixturesDecode(t *testing.T) {
	ts := newTestServer(t, nil)
	defer ts.close()
	cases := []struct {
		fixture   string
		wantItems int
		wantEvent bool
	}{
		{"envelopes/sentry-go-event.envelope", 1, true},
		{"envelopes/sentry-python-event.envelope", 3, true},
		{"envelopes/sentry-node-event.envelope", 1, true},
		{"envelopes/empty.envelope", 0, false},
	}
	for _, tc := range cases {
		body := readFixture(t, tc.fixture)
		resp := ts.post(t, "/api/1/envelope/", map[string]string{
			"X-Sentry-Auth":    ts.authHeader("1"),
			"Content-Encoding": "identity",
		}, body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d (%s)", tc.fixture, resp.StatusCode, readBody(t, resp))
		}
		id := respID(t, resp)
		if len(id) != 32 {
			t.Errorf("%s: success id %q is not 32 hex", tc.fixture, id)
		}
		env, perr := ts.s.parseEnvelope(body)
		if perr != nil {
			t.Fatalf("%s: parseEnvelope: %v", tc.fixture, perr)
		}
		if len(env.Items) != tc.wantItems {
			t.Errorf("%s: %d items, want %d", tc.fixture, len(env.Items), tc.wantItems)
		}
	}
	if got := ts.sink.ofKind(types.KEvent); len(got) == 0 {
		t.Fatal("no event records were written for the SDK fixtures")
	}
	// The python fixture also carries a client_report and a session: the report
	// is parsed (never dropped) and the session is dropped with a counter.
	if ts.s.counters.clientReports.Get() != 1 {
		t.Errorf("client_reports_total = %d, want 1", ts.s.counters.clientReports.Get())
	}
	if ts.s.counters.byItemType["session"] != 1 {
		t.Errorf("items_dropped_total{session} = %d, want 1", ts.s.counters.byItemType["session"])
	}
}

// TestEnvelopeFramingCases pins each malformed case's exact code (§7).
func TestEnvelopeFramingCases(t *testing.T) {
	ts := newTestServer(t, nil)
	defer ts.close()
	base := map[string]any{"event_id": "9f2c1d3e4b5a6c7d8e9f0a1b2c3d4e5f", "sentry_client": "t/0.1"}
	ev := eventJSON(t, nil)

	cases := []struct {
		name   string
		body   []byte
		status int
		code   types.ErrorCode
	}{
		{
			name:   "length present",
			body:   envelopeBytes(t, base, envelopeFixtureItem{Type: "event", Body: ev, Length: true}),
			status: 200,
		},
		{
			name:   "length absent (single-line JSON)",
			body:   envelopeBytes(t, base, envelopeFixtureItem{Type: "event", Body: ev}),
			status: 200,
		},
		{
			name:   "header is not an object",
			body:   []byte("[]\n"),
			status: 400,
			code:   types.CodeSentinel001,
		},
		{
			name:   "item header is not an object",
			body:   []byte("{\"event_id\":\"x\"}\nnotjson\n"),
			status: 400,
			code:   types.CodeSentinel001,
		},
		{
			name:   "binary item without length",
			body:   envelopeBytes(t, base, envelopeFixtureItem{Type: "attachment", Body: []byte("raw-bytes")}),
			status: 400,
			code:   types.CodeSentinel001,
		},
		{
			name: "missing LF after a length-prefixed body",
			// A `length` that understates the body: the extra bytes are
			// discarded and the item parses (§6.4: a lying length is data).
			body:   []byte("{\"event_id\":\"9f2c1d3e4b5a6c7d8e9f0a1b2c3d4e5f\"}\n{\"type\":\"event\",\"length\":2}\n{}trailing-bytes-discarded\n"),
			status: 200,
		},
		{
			name:   "declared length never arrives",
			body:   []byte("{\"event_id\":\"x\"}\n{\"type\":\"event\",\"length\":4096}\nshort"),
			status: 400,
			code:   types.CodeSentinel001,
		},
	}
	for _, tc := range cases {
		resp := ts.post(t, "/api/1/envelope/", map[string]string{"X-Sentry-Auth": ts.authHeader("1")}, tc.body)
		if resp.StatusCode != tc.status {
			t.Errorf("%s: status %d, want %d (%s)", tc.name, resp.StatusCode, tc.status, readBody(t, resp))
			continue
		}
		if tc.code != "" {
			if got := resp.Header.Get("X-Sentry-Error"); got != string(tc.code) {
				t.Errorf("%s: X-Sentry-Error %q, want %q", tc.name, got, tc.code)
			}
		}
		_ = readBody(t, resp)
	}
	// The lying-length case must be counted, never silently swallowed.
	if ts.s.counters.overrun.Get() == 0 {
		t.Error("item_overrun_total is 0 after a lying length")
	}
}

// TestZeroItemEnvelopeIsLegal pins §3.1: a zero-item envelope is 200 with the
// header's (or a synthesized) event id — never 400.
func TestZeroItemEnvelopeIsLegal(t *testing.T) {
	ts := newTestServer(t, nil)
	defer ts.close()
	body := []byte("{\"event_id\":\"9f2c1d3e4b5a6c7d8e9f0a1b2c3d4e5f\"}\n")
	resp := ts.post(t, "/api/1/envelope/", map[string]string{"X-Sentry-Auth": ts.authHeader("1")}, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("zero-item envelope: status %d", resp.StatusCode)
	}
	if id := respID(t, resp); id != "9f2c1d3e4b5a6c7d8e9f0a1b2c3d4e5f" {
		t.Fatalf("zero-item envelope id = %q, want the header event_id", id)
	}
	if ts.s.counters.emptyEnvelopes.Get() != 1 {
		t.Fatalf("empty_envelopes = %d, want 1", ts.s.counters.emptyEnvelopes.Get())
	}
	// Without an event_id the id is synthesized (32 hex) rather than refused.
	resp2 := ts.post(t, "/api/1/envelope/", map[string]string{"X-Sentry-Auth": ts.authHeader("1")}, []byte("{}\n"))
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("zero-item envelope without event_id: status %d", resp2.StatusCode)
	}
	if id := respID(t, resp2); len(id) != 32 {
		t.Fatalf("synthesized id = %q, want 32 hex", id)
	}
}

// TestTrailingLFIsLegal pins the optional trailing LF.
func TestTrailingLFIsLegal(t *testing.T) {
	ts := newTestServer(t, nil)
	defer ts.close()
	ev := eventJSON(t, nil)
	body := envelopeBytes(t, map[string]any{"event_id": "9f2c1d3e4b5a6c7d8e9f0a1b2c3d4e5f"},
		envelopeFixtureItem{Type: "event", Body: ev, Length: true})
	for _, variant := range [][]byte{body, append(append([]byte(nil), body...), '\n')} {
		resp := ts.post(t, "/api/1/envelope/", map[string]string{"X-Sentry-Auth": ts.authHeader("1")}, variant)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("trailing-LF variant: status %d (%s)", resp.StatusCode, readBody(t, resp))
		}
		_ = readBody(t, resp)
	}
}

// itemTypeCount counts an item_types payload value regardless of whether it
// arrived as a Go slice (in-memory sink) or a decoded []any (JSON).
func itemTypeCount(v any) int {
	switch t := v.(type) {
	case []any:
		return len(t)
	case []string:
		return len(t)
	}
	return 0
}

// TestHeaderLineCap pins the 8KB header cap (TROUBLE-SENTINEL-001).
func TestHeaderLineCap(t *testing.T) {
	ts := newTestServer(t, nil)
	defer ts.close()
	huge := "{\"event_id\":\"" + strings.Repeat("a", 9000) + "\"}\n"
	resp := ts.post(t, "/api/1/envelope/", map[string]string{"X-Sentry-Auth": ts.authHeader("1")}, []byte(huge))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized header line: status %d, want 400", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Sentry-Error"); got != string(types.CodeSentinel001) {
		t.Fatalf("oversized header line: code %q, want 001", got)
	}
	_ = readBody(t, resp)
}

// TestUnknownItemTypeIs200AndDrop pins §3.2: an unsupported item type never
// fails the envelope; it is counted.
func TestUnknownItemTypeIs200AndDrop(t *testing.T) {
	ts := newTestServer(t, nil)
	defer ts.close()
	ev := eventJSON(t, nil)
	body := envelopeBytes(t, map[string]any{"event_id": "9f2c1d3e4b5a6c7d8e9f0a1b2c3d4e5f"},
		envelopeFixtureItem{Type: "check_in", Body: []byte(`{"check_in_id":"x"}`), Length: true},
		envelopeFixtureItem{Type: "event", Body: ev, Length: true},
	)
	resp := ts.post(t, "/api/1/envelope/", map[string]string{"X-Sentry-Auth": ts.authHeader("1")}, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unknown item type: status %d (%s)", resp.StatusCode, readBody(t, resp))
	}
	if errCode := resp.Header.Get("X-Sentry-Error"); errCode != string(types.CodeSentinel013) {
		t.Errorf("unknown item type: X-Sentry-Error %q, want 013", errCode)
	}
	if ts.s.counters.unknownItems.Get() != 1 {
		t.Errorf("unknown_items_total = %d, want 1", ts.s.counters.unknownItems.Get())
	}
	records := ts.sink.ofKind(types.KEvent)
	if len(records) != 1 {
		t.Fatalf("%d event records, want exactly 1 (the unknown item must not write one)", len(records))
	}
	if got := itemTypeCount(records[0].Payload["item_types"]); got != 2 {
		t.Errorf("item_types = %v, want both the dropped and the kept type", records[0].Payload["item_types"])
	}
	_ = readBody(t, resp)
}

// TestStoreRouteForms pins the legacy /store/ route: JSON body, gzip'd body and
// the form-encoded `sentry_data` field, all with the deprecation header.
func TestStoreRouteForms(t *testing.T) {
	ts := newTestServer(t, nil)
	defer ts.close()
	ev := eventJSON(t, nil)
	form := "sentry_data=" + url.QueryEscape(string(ev))

	cases := []struct {
		name string
		body []byte
		hdr  map[string]string
		want int
	}{
		{"json", ev, map[string]string{"Content-Type": "application/json"}, 200},
		{"json gzip", gzipBytes(t, ev), map[string]string{"Content-Type": "application/json", "Content-Encoding": "gzip"}, 200},
		{"form sentry_data", []byte(form), map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, 200},
		{"unsupported type", ev, map[string]string{"Content-Type": "text/csv"}, 415},
	}
	for _, tc := range cases {
		hdr := map[string]string{"X-Sentry-Auth": ts.authHeader("1")}
		for k, v := range tc.hdr {
			hdr[k] = v
		}
		resp := ts.post(t, "/api/1/store/", hdr, tc.body)
		if resp.StatusCode != tc.want {
			t.Errorf("%s: status %d, want %d (%s)", tc.name, resp.StatusCode, tc.want, readBody(t, resp))
			continue
		}
		if tc.want == 200 && resp.Header.Get("X-Sentry-Deprecated") != "store" {
			t.Errorf("%s: missing X-Sentry-Deprecated header", tc.name)
		}
		_ = readBody(t, resp)
	}
	if ts.s.counters.legacyStore.Get() != 3 {
		t.Errorf("legacy_store_total = %d, want 3", ts.s.counters.legacyStore.Get())
	}
}

// TestEnvelopeAuthForms pins the three accepted forms and the envelope_dsn path
// (§2.4), including the DSN/path project mismatch.
func TestEnvelopeAuthForms(t *testing.T) {
	ts := newTestServer(t, nil)
	defer ts.close()
	ev := eventJSON(t, nil)
	dsn := "http://" + testPubA + "@trouble.example.net:7643/1"
	header := map[string]any{"event_id": "9f2c1d3e4b5a6c7d8e9f0a1b2c3d4e5f", "dsn": dsn}
	body := envelopeBytes(t, header, envelopeFixtureItem{Type: "event", Body: ev, Length: true})

	resp := ts.post(t, "/api/1/envelope/", nil, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("envelope_dsn form: status %d (%s)", resp.StatusCode, readBody(t, resp))
	}
	_ = readBody(t, resp)

	// The key in the query string is the documented form for curl (loopback).
	// A different event id keeps this a distinct event rather than an SDK retry
	// inside the 10-minute dedup window (§6.6).
	queryEvent := eventJSON(t, func(m map[string]any) {
		m["event_id"] = "aaaabbbbccccddddeeeeffff00001111"
	})
	queryBody := envelopeBytes(t, map[string]any{"event_id": "aaaabbbbccccddddeeeeffff00001111"},
		envelopeFixtureItem{Type: "event", Body: queryEvent, Length: true})
	resp = ts.post(t, "/api/1/envelope/?sentry_key="+testPubA, nil, queryBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("query form: status %d (%s)", resp.StatusCode, readBody(t, resp))
	}
	_ = readBody(t, resp)

	// A DSN whose project id differs from the path is TROUBLE-SENTINEL-006.
	badHeader := map[string]any{"dsn": "http://" + testPubA + "@trouble.example.net:7643/2"}
	badBody := envelopeBytes(t, badHeader, envelopeFixtureItem{Type: "event", Body: ev, Length: true})
	resp = ts.post(t, "/api/1/envelope/", nil, badBody)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("dsn project mismatch: status %d, want 401", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Sentry-Error"); got != string(types.CodeSentinel006) {
		t.Fatalf("dsn project mismatch: code %q, want 006", got)
	}
	if !strings.Contains(string(readBody(t, resp)), causeDSNProjectMism) {
		t.Fatal("dsn project mismatch: causes must name dsn_project_mismatch")
	}

	// No auth material at all is 005 (the plain body above carries no dsn).
	plain := envelopeBytes(t, map[string]any{"event_id": "9f2c1d3e4b5a6c7d8e9f0a1b2c3d4e5f"},
		envelopeFixtureItem{Type: "event", Body: ev, Length: true})
	resp = ts.post(t, "/api/1/envelope/", nil, plain)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no auth: status %d, want 401", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Sentry-Error"); got != string(types.CodeSentinel005) {
		t.Fatalf("no auth: code %q, want 005", got)
	}
	_ = readBody(t, resp)

	// The auth forms actually used are recorded on the event records (§2.4).
	forms := map[string]bool{}
	for _, rec := range ts.sink.ofKind(types.KEvent) {
		if f, ok := rec.Payload["auth_form"].(string); ok {
			forms[f] = true
		}
	}
	if !forms[fmtEnvelopeDSN] || !forms[fmtQueryKey] {
		t.Fatalf("observed auth forms = %v, want envelope_dsn and query_sentry_key", forms)
	}
}

// TestMethodAndPathPolicy pins 405 + 021 and the empty-body 404.
func TestMethodAndPathPolicy(t *testing.T) {
	ts := newTestServer(t, nil)
	defer ts.close()
	resp, err := ts.ts.Client().Get(ts.ts.URL + "/api/1/envelope/")
	if err != nil {
		t.Fatalf("GET envelope route: %v", err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET on POST route: status %d, want 405", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Sentry-Error"); got != string(types.CodeSentinel021) {
		t.Errorf("GET on POST route: code %q, want 021", got)
	}
	_ = readBody(t, resp)

	for _, path := range []string{"/", "/api/", "/api/1", "/api/1/nope/", "/health.json", "/api/abc/envelope/", "/api/1/envelope"} {
		r2, err := ts.ts.Client().Get(ts.ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body := readBody(t, r2)
		if path == "/api/1" || path == "/api/1/" {
			continue
		}
		if r2.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404", path, r2.StatusCode)
		}
		if len(body) != 0 {
			t.Errorf("GET %s: 404 body must be empty (probe traffic must not write records), got %q", path, body)
		}
	}
	if ts.s.counters.ingest404.Get() == 0 {
		t.Error("ingest_404_total is 0 after unmatched paths")
	}
}

// TestProbeRoute pins the DSN probe shape and that auth is optional but checked
// when present.
func TestProbeRoute(t *testing.T) {
	ts := newTestServer(t, nil)
	defer ts.close()
	resp, err := ts.ts.Client().Get(ts.ts.URL + "/api/1/?sentry_key=" + testPubA)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("probe status %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.Unmarshal(readBody(t, resp), &out); err != nil {
		t.Fatalf("probe body: %v", err)
	}
	for _, key := range []string{"id", "slug", "enabled", "status", "canary_last_ts"} {
		if _, ok := out[key]; !ok {
			t.Errorf("probe body is missing %q", key)
		}
	}
	if out["slug"] != "alpha" {
		t.Errorf("probe slug = %v, want alpha", out["slug"])
	}

	// A disabled project still answers 200 with enabled:false.
	ts2 := newTestServer(t, func(c *Config) {
		c.Projects[0].Enabled = false
	})
	defer ts2.close()
	resp2, err := ts2.ts.Client().Get(ts2.ts.URL + "/api/1/")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	var out2 map[string]any
	if err := json.Unmarshal(readBody(t, resp2), &out2); err != nil {
		t.Fatalf("probe body: %v", err)
	}
	if resp2.StatusCode != http.StatusOK || out2["enabled"] != false || out2["status"] != "disabled" {
		t.Fatalf("disabled probe = %d %v, want 200 with enabled:false", resp2.StatusCode, out2)
	}
	// An ingestion request to a disabled project is 401 + 008.
	ev := eventJSON(t, nil)
	resp3 := ts2.post(t, "/api/1/envelope/", map[string]string{"X-Sentry-Auth": ts2.authHeader("1")}, envelopeBytes(t, map[string]any{}, envelopeFixtureItem{Type: "event", Body: ev, Length: true}))
	if resp3.StatusCode != http.StatusUnauthorized || resp3.Header.Get("X-Sentry-Error") != string(types.CodeSentinel008) {
		t.Fatalf("disabled project ingest = %d %s, want 401 008", resp3.StatusCode, resp3.Header.Get("X-Sentry-Error"))
	}
	_ = readBody(t, resp3)
}

// TestUnknownProjectIs401 pins TROUBLE-SENTINEL-007.
func TestUnknownProjectIs401(t *testing.T) {
	ts := newTestServer(t, nil)
	defer ts.close()
	ev := eventJSON(t, nil)
	resp := ts.post(t, "/api/9/envelope/", map[string]string{"X-Sentry-Auth": ts.authHeader("1")}, envelopeBytes(t, map[string]any{}, envelopeFixtureItem{Type: "event", Body: ev, Length: true}))
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("X-Sentry-Error") != string(types.CodeSentinel007) {
		t.Fatalf("unknown project = %d %s, want 401 007", resp.StatusCode, resp.Header.Get("X-Sentry-Error"))
	}
	_ = readBody(t, resp)
}

// TestGzipEnvelopeAccepted pins the SDK's default Content-Encoding.
func TestGzipEnvelopeAccepted(t *testing.T) {
	ts := newTestServer(t, nil)
	defer ts.close()
	ev := eventJSON(t, nil)
	body := envelopeBytes(t, map[string]any{"event_id": "9f2c1d3e4b5a6c7d8e9f0a1b2c3d4e5f"},
		envelopeFixtureItem{Type: "event", Body: ev, Length: true})
	resp := ts.post(t, "/api/1/envelope/", map[string]string{
		"X-Sentry-Auth":    ts.authHeader("1"),
		"Content-Encoding": "gzip",
	}, gzipBytes(t, body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gzip envelope: status %d (%s)", resp.StatusCode, readBody(t, resp))
	}
	_ = readBody(t, resp)

	// Deflate/br/zstd are unsupported: 415 + 022.
	for _, enc := range []string{"deflate", "br", "zstd"} {
		resp := ts.post(t, "/api/1/envelope/", map[string]string{
			"X-Sentry-Auth":    ts.authHeader("1"),
			"Content-Encoding": enc,
		}, body)
		if resp.StatusCode != http.StatusUnsupportedMediaType {
			t.Errorf("Content-Encoding %s: status %d, want 415", enc, resp.StatusCode)
		}
		if got := resp.Header.Get("X-Sentry-Error"); got != string(types.CodeSentinel022) {
			t.Errorf("Content-Encoding %s: code %q, want 022", enc, got)
		}
		_ = readBody(t, resp)
	}
}
