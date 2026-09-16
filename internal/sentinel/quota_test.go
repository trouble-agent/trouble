package sentinel

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// postEvent sends one event through the generic JSON route with the loopback
// query-key form (the documented curl shape).
func (t *testServer) postEvent(tb testing.TB, projectID, eventID string) *http.Response {
	tb.Helper()
	body := []byte(fmt.Sprintf(`{"event_id":%q,"message":"queue wedge: pool exhausted depth=912","culprit":"worker.claim","level":"error","release":"payment-api@2.4.1"}`, eventID))
	return t.post(tb, "/api/"+projectID+"/event/?sentry_key="+t.keyFor(projectID), map[string]string{"Content-Type": "application/json"}, body)
}

// TestQuotaHeaderGoldenStrings pins §3.9's header format byte-for-byte and the
// 429 shape of drop-with-counter.
func TestQuotaHeaderGoldenStrings(t *testing.T) {
	ts := newTestServer(t, func(c *Config) {
		c.CanaryProject = ""
		c.Projects[0].QuotaEPM = 2
		c.Projects[0].LossPolicy = types.LossDropCounter
	})
	defer ts.close()

	for i := 0; i < 2; i++ {
		resp := ts.postEvent(t, "1", fmt.Sprintf("%032x", i+1))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("event %d: status %d (%s)", i, resp.StatusCode, readBody(t, resp))
		}
		_ = readBody(t, resp)
	}
	resp := ts.postEvent(t, "1", fmt.Sprintf("%032x", 3))
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over-quota status = %d, want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Sentry-Rate-Limits"); got != "60:error:project:quota_epm:" {
		t.Errorf("X-Sentry-Rate-Limits = %q, want the pinned %q", got, "60:error:project:quota_epm:")
	}
	if got := resp.Header.Get("X-Sentry-Error"); got != string(types.CodeSentinel010) {
		t.Errorf("X-Sentry-Error = %q, want 010", got)
	}
	ra := resp.Header.Get("Retry-After")
	n, err := strconv.Atoi(ra)
	if err != nil || n < 1 || n > 60 {
		t.Errorf("Retry-After = %q, want 1..60", ra)
	}
	_ = readBody(t, resp)

	// The refusal is a ledger record with sig + disposition, and the group's
	// counters carry the drop: a quota breach never destroys the accounting.
	recs := ts.sink.ofKind(types.KEvent)
	last := recs[len(recs)-1]
	if last.Payload["disposition"] != dispDropped {
		t.Errorf("refusal disposition = %v, want %q", last.Payload["disposition"], dispDropped)
	}
	if last.Payload["error_code"] != string(types.CodeSentinel014) {
		t.Errorf("refusal error_code = %v, want 014", last.Payload["error_code"])
	}
	if last.Sig == "" {
		t.Error("the refusal record carries no sig")
	}
	rt, _ := ts.s.ProjectRuntime("1")
	if rt.DroppedTotal != 1 {
		t.Errorf("ProjectRuntime.DroppedTotal = %d, want 1", rt.DroppedTotal)
	}
	if rt.EventsWindow != 2 {
		t.Errorf("events_window = %d, want 2 (rejected events do not consume quota)", rt.EventsWindow)
	}
}

// TestProactiveBackoffAt95Percent pins §3.9's 200-plus-header path: at 95% of
// quota the response is 200 with the same header, which is what turns a flood
// into an orderly slowdown instead of a 429 cliff.
func TestProactiveBackoffAt95Percent(t *testing.T) {
	ts := newTestServer(t, func(c *Config) {
		c.CanaryProject = ""
		c.Projects[0].QuotaEPM = 20
	})
	defer ts.close()
	saw := false
	for i := 0; i < 19; i++ {
		resp := ts.postEvent(t, "1", fmt.Sprintf("%032x", 100+i))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("event %d: status %d", i, resp.StatusCode)
		}
		if h := resp.Header.Get("X-Sentry-Rate-Limits"); h != "" {
			if h != "60:error:project:quota_epm:" {
				t.Errorf("proactive header = %q, want the pinned quota header", h)
			}
			saw = true
		}
		_ = readBody(t, resp)
	}
	if !saw {
		t.Fatal("no proactive backoff header appeared by 95% of quota")
	}
}

// TestQuotaWindowRolls pins the fixed 60s tumbling window: after it rolls, the
// project's allowance is whole again and the previous window is still readable.
func TestQuotaWindowRolls(t *testing.T) {
	ts := newTestServer(t, func(c *Config) {
		c.CanaryProject = ""
		c.Projects[0].QuotaEPM = 2
	})
	defer ts.close()
	cur := nowFunc()
	ts.s.now = func() time.Time { return cur }

	for i := 0; i < 2; i++ {
		resp := ts.postEvent(t, "1", fmt.Sprintf("%032x", 200+i))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("event %d: status %d", i, resp.StatusCode)
		}
		_ = readBody(t, resp)
	}
	if resp := ts.postEvent(t, "1", fmt.Sprintf("%032x", 220)); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over-quota status = %d, want 429", resp.StatusCode)
	} else {
		_ = readBody(t, resp)
	}
	if got := ts.s.quota.usedNow("1", cur); got != 2 {
		t.Fatalf("used in the first window = %d, want 2", got)
	}
	// Roll the clock past the window boundary.
	cur = cur.Add(61 * time.Second)
	if got := ts.s.quota.usedNow("1", cur); got != 0 {
		t.Fatalf("used after the window rolled = %d, want 0", got)
	}
	used, prev, _ := ts.s.quota.windowView("1", cur)
	if used != 0 || prev != 2 {
		t.Fatalf("windowView = (used %d, prev %d), want (0, 2): the previous window must be retained", used, prev)
	}
	resp := ts.postEvent(t, "1", fmt.Sprintf("%032x", 300))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("event after the roll: status %d, want 200", resp.StatusCode)
	}
	_ = readBody(t, resp)
}

// TestSampleLossPolicy pins `sample`: the SDK keeps sending, the drop is
// recorded, and Counters.SampleRate is set.
func TestSampleLossPolicy(t *testing.T) {
	ts := newTestServer(t, func(c *Config) {
		c.CanaryProject = ""
		c.Projects[0].QuotaEPM = 1
		c.Projects[0].LossPolicy = types.LossSample
	})
	defer ts.close()
	resp := ts.postEvent(t, "1", fmt.Sprintf("%032x", 1))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first event: status %d", resp.StatusCode)
	}
	_ = readBody(t, resp)

	sampled, dropped := 0, 0
	for i := 0; i < 20; i++ {
		resp := ts.postEvent(t, "1", fmt.Sprintf("%032x", 400+i))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("sample policy must answer 200 (the SDK keeps sending): status %d", resp.StatusCode)
		}
		_ = readBody(t, resp)
	}
	for _, rec := range ts.sink.ofKind(types.KEvent) {
		switch rec.Payload["disposition"] {
		case dispSampled:
			sampled++
		case dispAdmitted:
		default:
			dropped++
		}
	}
	if sampled == 0 {
		t.Fatal("no event record carried disposition=sampled")
	}
	if dropped != 0 {
		t.Fatalf("%d event records carried an unexpected disposition", dropped)
	}
	groups := ts.s.Groups()
	set := false
	for _, g := range groups {
		if g.Counters.SampleRate > 0 {
			set = true
		}
	}
	if !set {
		t.Error("no group reported Counters.SampleRate after sampling")
	}
	if ts.s.counters.sampled.Get() == 0 {
		t.Error("the project's sampled counter did not move")
	}
}

// TestSpoolIfLightPolicy pins `spool-if-light`: the event reaches the spool, is
// counted, and replays when the quota allows.
func TestSpoolIfLightPolicy(t *testing.T) {
	ts := newTestServer(t, func(c *Config) {
		c.CanaryProject = ""
		c.Projects[0].QuotaEPM = 1
		c.Projects[0].LossPolicy = types.LossSpoolIfLight
	})
	defer ts.close()
	resp := ts.postEvent(t, "1", fmt.Sprintf("%032x", 1))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first event: status %d", resp.StatusCode)
	}
	_ = readBody(t, resp)

	for i := 0; i < 5; i++ {
		resp := ts.postEvent(t, "1", fmt.Sprintf("%032x", 500+i))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("spool-if-light must answer 200: status %d", resp.StatusCode)
		}
		_ = readBody(t, resp)
	}
	spooled := 0
	for _, rec := range ts.sink.ofKind(types.KEvent) {
		if rec.Payload["disposition"] == dispSpooled {
			spooled++
		}
	}
	if spooled != 5 {
		t.Fatalf("%d spooled records, want 5", spooled)
	}
	if ts.s.spool.Len() != 5 {
		t.Fatalf("spool holds %d entries, want 5", ts.s.spool.Len())
	}
	if ts.s.SpoolStats().SpooledTotal != 5 {
		t.Fatalf("spooled_total = %d, want 5", ts.s.SpoolStats().SpooledTotal)
	}
	// Roll the window so the quota is free again and replay.
	cur := nowFunc().Add(61 * time.Second)
	ts.s.now = func() time.Time { return cur }
	if n := ts.s.replaySpool(cur); n != 5 {
		t.Fatalf("replayed %d entries, want 5", n)
	}
	if ts.s.spool.Len() != 0 {
		t.Fatalf("spool still holds %d entries after replay", ts.s.spool.Len())
	}
}

// TestSpoolBudgetFullIs015 pins §3.9's spool-full path: a budget that cannot hold
// an entry drops the event with TROUBLE-SENTINEL-015 rather than losing it
// silently.
func TestSpoolBudgetFullIs015(t *testing.T) {
	ts := newTestServer(t, func(c *Config) {
		c.CanaryProject = ""
		c.Projects[0].QuotaEPM = 0
		c.Projects[0].LossPolicy = types.LossSpoolIfLight
		c.SpoolBudgetBytes = 64
		c.Projects[0].QuotaEPM = 1
	})
	defer ts.close()
	resp := ts.postEvent(t, "1", fmt.Sprintf("%032x", 1))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first event: status %d", resp.StatusCode)
	}
	_ = readBody(t, resp)
	resp = ts.postEvent(t, "1", fmt.Sprintf("%032x", 2))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("spool-full event: status %d, want 200", resp.StatusCode)
	}
	_ = readBody(t, resp)
	found := false
	for _, rec := range ts.sink.ofKind(types.KEvent) {
		if rec.Payload["disposition"] == dispSpoolFull {
			found = true
			if rec.Payload["error_code"] != string(types.CodeSentinel015) {
				t.Errorf("spool-full error_code = %v, want 015", rec.Payload["error_code"])
			}
		}
	}
	if !found {
		t.Fatal("no event record carried disposition=dropped_spool_full")
	}
}

// TestQuotaEqualsAllowancePlusOne pins §6.10: `used == quota` leaves zero
// allowance and the next item breaches; a single envelope larger than quota_epm
// is admitted to quota_epm items and the remainder follows the loss policy.
func TestQuotaEqualsAllowancePlusOne(t *testing.T) {
	ts := newTestServer(t, func(c *Config) {
		c.CanaryProject = ""
		c.Projects[0].QuotaEPM = 3
		c.Projects[0].LossPolicy = types.LossDropCounter
	})
	defer ts.close()
	items := make([]envelopeFixtureItem, 0, 6)
	for i := 0; i < 6; i++ {
		body := eventJSON(t, func(m map[string]any) {
			m["event_id"] = fmt.Sprintf("%032x", 900+i)
		})
		items = append(items, envelopeFixtureItem{Type: "event", Body: body, Length: true})
	}
	env := envelopeBytes(t, map[string]any{"event_id": fmt.Sprintf("%032x", 999)}, items...)
	resp := ts.post(t, "/api/1/envelope/", map[string]string{"X-Sentry-Auth": ts.authHeader("1")}, env)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("oversized envelope status = %d, want 429 once the quota is spent", resp.StatusCode)
	}
	_ = readBody(t, resp)
	admitted, refused := 0, 0
	for _, rec := range ts.sink.ofKind(types.KEvent) {
		switch rec.Payload["disposition"] {
		case dispAdmitted:
			admitted++
		case dispDropped:
			refused++
		}
	}
	if admitted != 3 {
		t.Errorf("admitted %d events, want exactly quota_epm = 3", admitted)
	}
	if refused != 3 {
		t.Errorf("refused %d events, want the remaining 3", refused)
	}
}

// TestConcurrencyCapHeader pins the concurrency-cap header shape.
func TestConcurrencyCapHeader(t *testing.T) {
	if overloadHeader != "1:error:global:overloaded:" {
		t.Fatalf("overload header = %q, want the pinned shape", overloadHeader)
	}
	if ipHeader != "1:error:ip:overloaded:" {
		t.Fatalf("per-IP header = %q, want the pinned shape", ipHeader)
	}
	if diskBudgetHeader != "300:error:global:disk_budget:" {
		t.Fatalf("disk budget header = %q, want the pinned shape", diskBudgetHeader)
	}
	if breakerHeader != "120:error:project:breaker:" || killSwitchHeader != "3600:error:global:kill_switch:" {
		t.Fatal("breaker/kill-switch header shapes drifted")
	}
}

// TestDiskBudgetAppliesLossPolicy pins §3.9's global disk budget: once sampled
// over budget, the project's loss policy applies with the 300s header.
func TestDiskBudgetAppliesLossPolicy(t *testing.T) {
	ts := newTestServer(t, func(c *Config) {
		c.CanaryProject = ""
		c.DiskBudgetBytes = 1 << 20
		c.Projects[0].LossPolicy = types.LossDropCounter
	})
	defer ts.close()
	ts.s.disk.set(2<<20, 1<<20, nowFunc())
	resp := ts.postEvent(t, "1", fmt.Sprintf("%032x", 1))
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over-budget status = %d, want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Sentry-Rate-Limits"); got != diskBudgetHeader {
		t.Errorf("over-budget header = %q, want %q", got, diskBudgetHeader)
	}
	if got := resp.Header.Get("Retry-After"); got != "300" {
		t.Errorf("over-budget Retry-After = %q, want 300", got)
	}
	if got := resp.Header.Get("X-Sentry-Error"); got != string(types.CodeSentinel011) {
		t.Errorf("over-budget code = %q, want 011", got)
	}
	_ = readBody(t, resp)
}

// TestLedgerBackpressureIsOverloaded pins §6.13: a sink that blocks past
// ledger_wait turns into 429 overloaded rather than a silent loss.
func TestLedgerBackpressureIsOverloaded(t *testing.T) {
	ts := newTestServer(t, func(c *Config) {
		c.CanaryProject = ""
		c.LedgerWait = types.Duration("20ms")
	})
	defer ts.close()
	ts.sink.setDelay(200 * time.Millisecond)
	defer ts.sink.setDelay(0)
	resp := ts.postEvent(t, "1", fmt.Sprintf("%032x", 1))
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("backpressure status = %d, want 429", resp.StatusCode)
	}
	_ = readBody(t, resp)
	// Once the sink recovers, the path works again.
	ts.sink.setDelay(0)
	resp = ts.postEvent(t, "1", fmt.Sprintf("%032x", 2))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post-backpressure status = %d, want 200", resp.StatusCode)
	}
	_ = readBody(t, resp)
}

// TestProjectRuntimeView pins the §3.10 view fields the dashboard reads.
func TestProjectRuntimeView(t *testing.T) {
	ts := newTestServer(t, func(c *Config) {
		c.CanaryProject = ""
		c.DiskBudgetBytes = 4096
	})
	defer ts.close()
	if _, ok := ts.s.ProjectRuntime("99"); ok {
		t.Fatal("ProjectRuntime must not invent a project")
	}
	rt, ok := ts.s.ProjectRuntime("1")
	if !ok {
		t.Fatal("ProjectRuntime(1) missing")
	}
	if rt.Project != "1" || rt.QuotaEPM != 600 || rt.WindowS != 60 {
		t.Fatalf("ProjectRuntime = %+v, want project 1 with quota 600 and a 60s window", rt)
	}
	if rt.Remaining != 600 {
		t.Fatalf("remaining = %d, want the whole quota", rt.Remaining)
	}
	if rt.DiskBudgetBytes != 4096 {
		t.Fatalf("disk budget = %d, want 4096", rt.DiskBudgetBytes)
	}
	// client reports fold into the runtime view.
	ts.s.projects.mu.RLock()
	entry := ts.s.projects.byID["1"]
	ts.s.projects.mu.RUnlock()
	entry.mergeClientReport([]types.DiscardCount{
		{Reason: "queue_overflow", Category: "error", Quantity: 2},
		{Reason: "network_error", Category: "error", Quantity: 5},
	})
	rt, _ = ts.s.ProjectRuntime("1")
	if rt.ClientReportDiscards["queue_overflow"] != 2 || rt.ClientReportDiscards["network_error"] != 5 {
		t.Fatalf("client_report_discards = %v, want the merged reasons", rt.ClientReportDiscards)
	}
}

// TestRejectStormGap pins §5: a project above 50% rejects over the window emits
// `gap` cause ingest_reject_storm.
func TestRejectStormGap(t *testing.T) {
	ts := newTestServer(t, func(c *Config) {
		c.CanaryProject = ""
		c.Projects[0].Enabled = false
	})
	defer ts.close()
	entry, _ := ts.s.projects.project("1")
	bad := errf(types.CodeSentinel008, "project is disabled", causeProjectDisabled)
	for i := 0; i < 12; i++ {
		ts.s.countReject(entry, bad)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, rec := range ts.sink.ofKind(types.KGap) {
			if rec.Payload["cause"] == "ingest_reject_storm" {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no gap record with cause ingest_reject_storm")
}

// TestQuotaDoesNotConsumeRefusedEvents pins the self-perpetuation rule of §3.9.
func TestQuotaDoesNotConsumeRefusedEvents(t *testing.T) {
	ts := newTestServer(t, func(c *Config) {
		c.CanaryProject = ""
		c.Projects[0].QuotaEPM = 1
		c.Projects[0].LossPolicy = types.LossDropCounter
	})
	defer ts.close()
	_ = readBody(t, ts.postEvent(t, "1", fmt.Sprintf("%032x", 1)))
	for i := 0; i < 5; i++ {
		_ = readBody(t, ts.postEvent(t, "1", fmt.Sprintf("%032x", 10+i)))
	}
	rt, _ := ts.s.ProjectRuntime("1")
	if rt.EventsWindow != 1 {
		t.Fatalf("events_window = %d, want 1: refused events must not consume quota", rt.EventsWindow)
	}
	_ = context.Background()
}
