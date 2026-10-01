package sentinel

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// RELEASE-002: the standalone sentinel on-ramp must dedup event-level retries.
//
// The §3.4a generic-JSON fold (TRBL-074) is the standalone-profile counterpart
// of the hub's dedup gate: the fold key is "generic:" + project + ":" +
// sig.DigestHex() — the §3.3 group identity itself — so both surfaces agree on
// what "the same event" means, and the window is the bounded LRU-backed window
// the hub gate uses as its degraded fallback. These tests pin the on-ramp
// behaviour the way a retried reporter sees it: N identical POSTs produce ONE
// event record with IDENTICAL 200 responses (the first occurrence's event id),
// and a different event posted after them is still recorded (no over-dedup).
//
// The tests exercise the full HTTP surface (parseGenericEvent → admitEvent →
// the fold seam at server.go's §3.4a site), not the fold struct directly, so
// a regression that disconnects the fold from the on-ramp stays red here.

// postGenericBody posts body to the generic on-ramp and returns the decoded
// 200 response object. It fails the test on any non-200 status.
func postGenericBody(tb testing.TB, ts *testServer, body string) map[string]any {
	tb.Helper()
	resp := ts.post(tb, "/api/1/event/", map[string]string{"X-Sentry-Auth": ts.authHeader("1")}, []byte(body))
	if resp.StatusCode != http.StatusOK {
		tb.Fatalf("status %d (%s)", resp.StatusCode, readBody(tb, resp))
	}
	var out map[string]any
	if err := json.Unmarshal(readBody(tb, resp), &out); err != nil {
		tb.Fatalf("response body is not a JSON object: %v", err)
	}
	return out
}

// eventRecordCount is how many event records the sink holds.
func eventRecordCount(tb testing.TB, ts *testServer) int {
	tb.Helper()
	return len(ts.sink.ofKind(types.KEvent))
}

// TestOnrampIdenticalRetriesDedupToOneEvent proves (a): three byte-identical
// generic POSTs inside the dedup window produce exactly ONE event record, and
// every response is the same 200 with the FIRST occurrence's event id
// (idempotent retry, never an error). The clock is injected — no sleeps.
func TestOnrampIdenticalRetriesDedupToOneEvent(t *testing.T) {
	cur := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	ts := newTestServer(t, func(c *Config) { c.CanaryProject = "" })
	defer ts.close()
	ts.s.now = func() time.Time { return cur }

	body := `{"message":"onramp dedup probe","level":"error","release":"0.1.0"}`
	first := postGenericBody(t, ts, body)
	firstID, _ := first["id"].(string)
	if firstID == "" {
		t.Fatalf("first response carries no event id: %v", first)
	}
	for i := 0; i < 2; i++ {
		cur = cur.Add(30 * time.Second) // inside the default 2m window
		again := postGenericBody(t, ts, body)
		if got, _ := again["id"].(string); got != firstID {
			t.Errorf("retry %d answered id %q, want the first occurrence's id %q", i+1, got, firstID)
		}
	}

	if got := eventRecordCount(t, ts); got != 1 {
		t.Fatalf("%d event records in the ledger, want 1 (retries must not mint new records)", got)
	}
	groups := ts.s.Groups()
	if len(groups) != 1 {
		t.Fatalf("%d groups, want 1 (retries fold into the one group)", len(groups))
	}
	if groups[0].Count != 1 {
		t.Errorf("group count = %d, want 1 (a folded retry is not a new occurrence)", groups[0].Count)
	}
}

// TestOnrampDifferentEventStillRecorded proves (b): a DIFFERENT event posted
// after the identical retries is still recorded — the fold keys on the §3.3
// signature, so a distinct body is distinct evidence, never over-deduped.
func TestOnrampDifferentEventStillRecorded(t *testing.T) {
	cur := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	ts := newTestServer(t, func(c *Config) { c.CanaryProject = "" })
	defer ts.close()
	ts.s.now = func() time.Time { return cur }

	same := `{"message":"onramp dedup probe","level":"error","release":"0.1.0"}`
	first := postGenericBody(t, ts, same)
	cur = cur.Add(30 * time.Second)
	postGenericBody(t, ts, same) // folded retry
	other := `{"message":"a genuinely different bug","level":"error","release":"0.1.0"}`
	cur = cur.Add(30 * time.Second)
	otherResp := postGenericBody(t, ts, other)
	otherID, _ := otherResp["id"].(string)
	if otherID == "" {
		t.Fatalf("distinct event carries no id: %v", otherResp)
	}
	if firstID, _ := first["id"].(string); otherID == firstID {
		t.Fatalf("distinct event answered the first occurrence's id %q — over-deduped", firstID)
	}

	if got := eventRecordCount(t, ts); got != 2 {
		t.Fatalf("%d event records in the ledger, want 2 (the distinct event must be recorded)", got)
	}
	if groups := ts.s.Groups(); len(groups) != 2 {
		t.Fatalf("%d groups, want 2 (distinct sigs are distinct groups)", len(groups))
	}
}
