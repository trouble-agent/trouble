package sentinel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// postGeneric sends a raw body to the generic JSON route.
func (t *testServer) postGeneric(tb testing.TB, body string) *http.Response {
	tb.Helper()
	return t.post(tb, "/api/1/event/?sentry_key="+t.keyFor("1"), map[string]string{"Content-Type": "application/json"}, []byte(body))
}

// TestGenericJSONMinimalBody pins the smallest accepted body: one `message`, one
// curl, one event (AC-15, AC-18).
func TestGenericJSONMinimalBody(t *testing.T) {
	ts := newTestServer(t, func(c *Config) { c.CanaryProject = "" })
	defer ts.close()
	resp := ts.postGeneric(t, `{"message":"queue wedge: pool exhausted depth=912"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d (%s)", resp.StatusCode, readBody(t, resp))
	}
	id := respID(t, resp)
	if len(id) != 32 {
		t.Fatalf("id = %q, want a generated 32-hex event id", id)
	}
	recs := ts.sink.ofKind(types.KEvent)
	if len(recs) != 1 {
		t.Fatalf("%d event records, want 1", len(recs))
	}
	ev := eventPayload(t, recs[0])
	// The stored message is the *scrubbed* value (secrets redacted); the 15-rule
	// masking is the signature normalization and shows up in the digest, not in
	// the record's message field (§3.3 vs §3.6).
	if ev["message"] != "queue wedge: pool exhausted depth=912" {
		t.Errorf("message = %v, want the scrubbed body", ev["message"])
	}
	if recs[0].Payload["source_kind"] != sourceGeneric {
		t.Errorf("source_kind = %v, want %q", recs[0].Payload["source_kind"], sourceGeneric)
	}
	if recs[0].Payload["auth_form"] != fmtGenericQuery {
		t.Errorf("auth_form = %v, want %q", recs[0].Payload["auth_form"], fmtGenericQuery)
	}
}

// TestGenericJSONStackEqualsVectorB pins the AC-18/AC-22 claim in its strongest
// testable form: a body reported with the vector-B frames produces the vector-B
// digest, i.e. a bash script and an SDK reporting the same bug class land in one
// group.
func TestGenericJSONStackEqualsVectorB(t *testing.T) {
	ts := newTestServer(t, func(c *Config) { c.CanaryProject = "" })
	defer ts.close()
	body := `{"event_id":"9f2c1d3e4b5a6c7d8e9f0a1b2c3d4e5f","level":"error","culprit":"worker.claim",
	  "message":"queue wedge: pool exhausted depth=912","release":"payment-api@2.4.1",
	  "exception":{"type":"PoolExhausted","value":"queue wedge: pool exhausted depth=912","stack":[
	    {"file":"worker.py","function":"claim","line":118,"in_app":true,"context_line":"item = pool.get(timeout=1)"},
	    {"file":"queue.py","function":"get","line":44,"in_app":true,"context_line":"raise PoolExhausted(depth=912)"}]}}`
	// The body is multi-line JSON: the route accepts it as one object.
	resp := ts.postGeneric(t, strings.Join(strings.Fields(body), " "))
	if resp.StatusCode != 200 {
		t.Fatalf("status %d (%s)", resp.StatusCode, readBody(t, resp))
	}
	_ = readBody(t, resp)
	recs := ts.sink.ofKind(types.KEvent)
	if len(recs) != 1 {
		t.Fatalf("%d event records, want 1", len(recs))
	}
	if recs[0].Sig != "sentinel:sha256v1:af7e89fe750191f9" {
		t.Fatalf("generic JSON sig = %s, want vector B (sentinel:sha256v1:af7e89fe750191f9)", recs[0].Sig)
	}
}

// TestGenericJSONFieldLimits pins §3.6's field caps and the 400/019 shape with
// `causes` naming each offending field.
func TestGenericJSONFieldLimits(t *testing.T) {
	ts := newTestServer(t, func(c *Config) { c.CanaryProject = "" })
	defer ts.close()
	cases := []struct {
		name  string
		body  string
		cause string
	}{
		{"neither message nor exception", `{"level":"error"}`, "message_or_exception_required"},
		{"message too long", fmt.Sprintf(`{"message":%q}`, strings.Repeat("x", 17*1024)), "message_too_long"},
		{"message not a string", `{"message":42}`, "message_not_string"},
		{"tags not an object", `{"message":"m","tags":[]}`, "tags_not_object"},
		{"too many tags", `{"message":"m","tags":{` + manyTags(33) + `}}`, "tags_too_many"},
		{"extra not an object", `{"message":"m","extra":"nope"}`, "extra_not_object"},
		{"extra too large", fmt.Sprintf(`{"message":"m","extra":{"blob":%q}}`, strings.Repeat("x", 9*1024)), "extra_too_large"},
		{"fingerprint too long", `{"message":"m","fingerprint":[` + strings.Repeat(`"a",`, 32) + `"a"]}`, "fingerprint_too_long"},
		{"timestamp not a string", `{"message":"m","timestamp":12}`, "timestamp_not_string"},
	}
	for _, tc := range cases {
		resp := ts.postGeneric(t, tc.body)
		if resp.StatusCode != 400 {
			t.Errorf("%s: status %d, want 400", tc.name, resp.StatusCode)
			_ = readBody(t, resp)
			continue
		}
		if got := resp.Header.Get("X-Sentry-Error"); got != string(types.CodeSentinel019) {
			t.Errorf("%s: code %q, want 019", tc.name, got)
		}
		var out struct {
			Detail string   `json:"detail"`
			Causes []string `json:"causes"`
		}
		if err := json.Unmarshal(readBody(t, resp), &out); err != nil {
			t.Fatalf("%s: error body: %v", tc.name, err)
		}
		if !contains(out.Causes, tc.cause) {
			t.Errorf("%s: causes %v do not name %q", tc.name, out.Causes, tc.cause)
		}
	}
	// A body that is not an object at all.
	resp := ts.postGeneric(t, `[1,2,3]`)
	if resp.StatusCode != 400 || resp.Header.Get("X-Sentry-Error") != string(types.CodeSentinel019) {
		t.Fatalf("array body = %d %s, want 400 019", resp.StatusCode, resp.Header.Get("X-Sentry-Error"))
	}
	_ = readBody(t, resp)
}

// TestGenericJSONInvalidLevelDegrades pins §3.6: an unrecognized level becomes
// `error` with a counter — never a refusal.
func TestGenericJSONInvalidLevelDegrades(t *testing.T) {
	ts := newTestServer(t, func(c *Config) { c.CanaryProject = "" })
	defer ts.close()
	resp := ts.postGeneric(t, `{"message":"m","level":"banana"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d, want 200 (an invalid level is a counter, not a rejection)", resp.StatusCode)
	}
	_ = readBody(t, resp)
	if ts.s.counters.invalidLevel.Get() == 0 {
		t.Error("invalid_level_total did not move")
	}
	ev := eventPayload(t, ts.sink.ofKind(types.KEvent)[0])
	if ev["level"] != "error" {
		t.Errorf("level = %v, want error", ev["level"])
	}
}

// TestGenericJSONExtraFingerprintIgnored pins §3.6: `extra.fingerprint` is
// ignored because the top-level field wins.
func TestGenericJSONExtraFingerprintIgnored(t *testing.T) {
	ts := newTestServer(t, func(c *Config) { c.CanaryProject = "" })
	defer ts.close()
	// The same message, once with the top-level fingerprint and once with only
	// the extra one: the digests must differ from the extra-only case (the extra
	// fingerprint was ignored, so the sig is the message-derived one).
	resp := ts.postGeneric(t, `{"event_id":"11111111111111111111111111111111","message":"m 912","extra":{"fingerprint":["sneaky"]}}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	_ = readBody(t, resp)
	resp = ts.postGeneric(t, `{"event_id":"22222222222222222222222222222222","message":"m 912","fingerprint":["sneaky"]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	_ = readBody(t, resp)
	recs := ts.sink.ofKind(types.KEvent)
	if len(recs) != 2 {
		t.Fatalf("%d records, want 2", len(recs))
	}
	if recs[0].Sig == recs[1].Sig {
		t.Fatal("extra.fingerprint was honored: both events produced the same digest")
	}
}

// TestGenericJSONTimestampAndAuthForms pins the timestamp default and the
// documented curl auth form.
func TestGenericJSONTimestampAndAuthForms(t *testing.T) {
	ts := newTestServer(t, func(c *Config) { c.CanaryProject = "" })
	defer ts.close()
	resp := ts.postGeneric(t, `{"message":"m","timestamp":"2026-09-16T09:14:03.221Z"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	_ = readBody(t, resp)
	ev := eventPayload(t, ts.sink.ofKind(types.KEvent)[0])
	if ev["ts"] != "2026-09-16T09:14:03.221Z" {
		t.Errorf("ts = %v, want the SDK timestamp", ev["ts"])
	}
	// Without a timestamp the receive time is used.
	before := types.FormatUTC(time.Now().Add(-2 * time.Second))
	resp = ts.postGeneric(t, `{"message":"second"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	_ = readBody(t, resp)
	ev2 := eventPayload(t, ts.sink.ofKind(types.KEvent)[1])
	if fmt.Sprint(ev2["ts"]) < before {
		t.Errorf("ts = %v, want the receive time", ev2["ts"])
	}
	// The form-encoded query key is the same documented curl form.
	form := ts.post(t, "/api/1/event/", map[string]string{"Content-Type": "application/json"},
		[]byte(`{"message":"third"}`))
	if form.StatusCode != 401 {
		t.Fatalf("no auth: status %d, want 401", form.StatusCode)
	}
	_ = readBody(t, form)
	auth := ts.post(t, "/api/1/event/", map[string]string{
		"Content-Type":  "application/json",
		"X-Sentry-Auth": ts.authHeader("1"),
	}, []byte(`{"message":"fourth"}`))
	if auth.StatusCode != 200 {
		t.Fatalf("X-Sentry-Auth form: status %d (%s)", auth.StatusCode, readBody(t, auth))
	}
	_ = readBody(t, auth)
}

// manyTags builds n tag pairs.
func manyTags(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"k%d":"v"`, i)
	}
	return b.String()
}

// contains reports whether a slice holds s.
func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
