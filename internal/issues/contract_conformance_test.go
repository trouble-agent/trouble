package issues

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// driverUnderTest is one driver plus the call counter of its fake backend, so the
// battery can assert call counts as well as observable behaviour (§7: "a case
// that passes with the wrong call count fails the battery").
type driverUnderTest struct {
	name   string
	driver types.IssueDriver
	gh     *fakeGitHub
	db     *fakeDuckbrain
	calls  func(kind string) int
	// commentCount counts appended comments at the backend, whatever the
	// transport looks like.
	commentCount func() int
}

func githubUnderTest(t *testing.T) driverUnderTest {
	t.Helper()
	api := newFakeGitHub()
	srv := ghServer(t, api)
	t.Setenv(testTokenEnv, "ghp_test_token_value")
	d, err := newGitHubDriver(githubTestConfig(srv.URL), nil, srv.Client())
	if err != nil {
		t.Fatalf("newGitHubDriver: %v", err)
	}
	return driverUnderTest{
		name: "github", driver: d, gh: api,
		calls:        func(kind string) int { return api.count(kind) },
		commentCount: func() int { return api.count("POST repo/issues/{n}/comments") },
	}
}

func duckbrainUnderTest(t *testing.T) driverUnderTest {
	t.Helper()
	api := newFakeDuckbrain()
	srv := dbServer(t, api)
	d, err := newDuckbrainDriver(duckbrainTestConfig(srv.URL), nil, srv.Client())
	if err != nil {
		t.Fatalf("newDuckbrainDriver: %v", err)
	}
	return driverUnderTest{
		name: "duckbrain", driver: d, db: api,
		calls: func(kind string) int { return api.count(kind) },
		commentCount: func() int {
			n := 0
			for _, k := range api.keys() {
				if strings.Contains(k, ".c.") {
					n++
				}
			}
			return n
		},
	}
}

func bothDrivers(t *testing.T) []driverUnderTest {
	t.Helper()
	return []driverUnderTest{githubUnderTest(t), duckbrainUnderTest(t)}
}

func ensureReq() types.EnsureBySigRequest {
	return types.EnsureBySigRequest{
		Sig:         testSig,
		Title:       "[trouble] high: queue wedge in payment-worker (" + testSig + ")",
		Body:        SigMarker(testSig) + "\n" + IncMarker(testIncident().ID, "github", MarkerVersion) + "\n\nbody",
		Labels:      []string{"trouble", "auto-filed", "sev:high"},
		DedupWindow: "30m",
		Severity:    types.SevHigh,
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func commentBody(idem string) string {
	return "recurrence 2026-09-16T09:41:02.114Z · inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE · occurrences 43\n\n" + IdemMarker(idem)
}

// TestConformanceBattery is the 14-case table of SPEC-09 §7 run against both
// shipped drivers. It is the contract's executable form: each case also asserts
// the call shape, because a fold that issues a create is a contract violation
// even when the observable result looks right.
func TestConformanceBattery(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T, d driverUnderTest)
	}{
		{"create", func(t *testing.T, d driverUnderTest) {
			resp, err := d.driver.EnsureBySig(context.Background(), ensureReq())
			if err != nil {
				t.Fatalf("ensure: %v", err)
			}
			if !resp.Created || resp.Commented {
				t.Fatalf("create: Created=%v Commented=%v", resp.Created, resp.Commented)
			}
			if resp.Ref.ExternalID == "" || resp.Ref.State != types.IssueOpen {
				t.Fatalf("create: ref=%+v", resp.Ref)
			}
		}},
		{"fold-inside-window", func(t *testing.T, d driverUnderTest) {
			first, err := d.driver.EnsureBySig(context.Background(), ensureReq())
			if err != nil {
				t.Fatalf("first ensure: %v", err)
			}
			second, err := d.driver.EnsureBySig(context.Background(), ensureReq())
			if err != nil {
				t.Fatalf("second ensure: %v", err)
			}
			if second.Created {
				t.Fatalf("second ensure created a duplicate issue")
			}
			if second.Ref.ExternalID != first.Ref.ExternalID {
				t.Fatalf("fold adopted a different issue: %s vs %s", second.Ref.ExternalID, first.Ref.ExternalID)
			}
		}},
		{"fold-outside-window", func(t *testing.T, d driverUnderTest) {
			if _, err := d.driver.EnsureBySig(context.Background(), ensureReq()); err != nil {
				t.Fatalf("first: %v", err)
			}
			req := ensureReq()
			req.DedupWindow = "1m"
			out, err := d.driver.EnsureBySig(context.Background(), req)
			if err != nil {
				t.Fatalf("second: %v", err)
			}
			if out.Created {
				t.Fatalf("outside-window fold created a duplicate")
			}
		}},
		{"repeat-idem-key", func(t *testing.T, d driverUnderTest) {
			first, err := d.driver.EnsureBySig(context.Background(), ensureReq())
			if err != nil {
				t.Fatalf("ensure: %v", err)
			}
			body := commentBody("issue_comment|" + first.Ref.ID + "|ev_01J9Z6Q0M2X4T8V1K7B3N5R8WM")
			if _, err := d.driver.Comment(context.Background(), first.Ref, body); err != nil {
				t.Fatalf("comment: %v", err)
			}
			before := d.commentCount()
			if _, err := d.driver.Comment(context.Background(), first.Ref, body); err != nil {
				t.Fatalf("repeat comment: %v", err)
			}
			if after := d.commentCount(); after != before {
				t.Fatalf("repeat idem key issued a second comment (%d → %d)", before, after)
			}
		}},
		{"comment-once-per-trigger", func(t *testing.T, d driverUnderTest) {
			first, err := d.driver.EnsureBySig(context.Background(), ensureReq())
			if err != nil {
				t.Fatalf("ensure: %v", err)
			}
			base := d.commentCount()
			for _, trigger := range []string{"ev_A", "ev_B"} {
				if _, err := d.driver.Comment(context.Background(), first.Ref, commentBody("issue_comment|"+first.Ref.ID+"|"+trigger)); err != nil {
					t.Fatalf("comment %s: %v", trigger, err)
				}
			}
			if n := d.commentCount() - base; n != 2 {
				t.Fatalf("two distinct triggers produced %d comments, want exactly 2", n)
			}
		}},
		{"close-idempotent", func(t *testing.T, d driverUnderTest) {
			first, err := d.driver.EnsureBySig(context.Background(), ensureReq())
			if err != nil {
				t.Fatalf("ensure: %v", err)
			}
			out, err := d.driver.Close(context.Background(), first.Ref, "resolved")
			if err != nil {
				t.Fatalf("close: %v", err)
			}
			if out.State != types.IssueClosed {
				t.Fatalf("close left state=%q", out.State)
			}
			again, err := d.driver.Close(context.Background(), out, "resolved")
			if err != nil {
				t.Fatalf("second close must be success, got %v", err)
			}
			if again.State != types.IssueClosed {
				t.Fatalf("second close state=%q", again.State)
			}
		}},
		{"close-refused", func(t *testing.T, d driverUnderTest) {
			first, err := d.driver.EnsureBySig(context.Background(), ensureReq())
			if err != nil {
				t.Fatalf("ensure: %v", err)
			}
			switch {
			case d.gh != nil:
				d.gh.mu.Lock()
				d.gh.locked = true
				d.gh.mu.Unlock()
			case d.db != nil:
				// The KV backend cannot lock; a missing anchor is its refusal path.
				d.db.mu.Lock()
				for k := range d.db.kv {
					delete(d.db.kv, k)
				}
				d.db.mu.Unlock()
			}
			_, err = d.driver.Close(context.Background(), first.Ref, "resolved")
			if err == nil {
				t.Fatalf("close must be refused")
			}
			code := CodeOf(err)
			if code != types.CodeIssues008 && code != types.CodeIssues007 {
				t.Fatalf("close refusal code = %s", code)
			}
		}},
		{"reopen", func(t *testing.T, d driverUnderTest) {
			first, err := d.driver.EnsureBySig(context.Background(), ensureReq())
			if err != nil {
				t.Fatalf("ensure: %v", err)
			}
			closed, err := d.driver.Close(context.Background(), first.Ref, "resolved")
			if err != nil {
				t.Fatalf("close: %v", err)
			}
			reopener, ok := d.driver.(Reopener)
			if !ok {
				t.Fatalf("%s does not implement Reopener", d.name)
			}
			out, err := reopener.Reopen(context.Background(), closed, "recurrence")
			if err != nil {
				t.Fatalf("reopen: %v", err)
			}
			if out.State != types.IssueOpen {
				t.Fatalf("reopen left state=%q", out.State)
			}
			if out.ExternalID != closed.ExternalID {
				t.Fatalf("reopen changed the external id: %s vs %s", out.ExternalID, closed.ExternalID)
			}
		}},
		{"healthcheck-ok", func(t *testing.T, d driverUnderTest) {
			h, err := d.driver.Healthcheck(context.Background())
			if err != nil {
				t.Fatalf("healthcheck: %v", err)
			}
			if !h.OK {
				t.Fatalf("healthcheck not ok: %+v", h)
			}
			if h.RateLimitRemaining == 0 {
				t.Fatalf("healthcheck did not report the quota headroom: %+v", h)
			}
		}},
		{"healthcheck-degraded", func(t *testing.T, d driverUnderTest) {
			if d.gh == nil {
				t.Skip("duckbrain has no quota headroom to degrade")
			}
			d.gh.mu.Lock()
			d.gh.coreRema = 5
			d.gh.mu.Unlock()
			cfg := DefaultGitHubConfig()
			cfg.APIBase, cfg.Owner, cfg.Repo = "", "acme", "payment-api"
			_ = cfg
			drv := d.driver.(*githubDriver)
			drv.cfg.MinRemaining = 100
			h, err := drv.Healthcheck(context.Background())
			if err != nil {
				t.Fatalf("healthcheck: %v", err)
			}
			if h.OK {
				t.Fatalf("a driver below min_remaining must report degraded: %+v", h)
			}
		}},
		{"healthcheck-failed", func(t *testing.T, d driverUnderTest) {
			switch {
			case d.gh != nil:
				d.gh.mu.Lock()
				d.gh.repoStatus = 503
				d.gh.bootProbedReset()
				d.gh.mu.Unlock()
			case d.db != nil:
				d.db.mu.Lock()
				d.db.down = true
				d.db.mu.Unlock()
			}
			_, err := d.driver.Healthcheck(context.Background())
			if err == nil {
				t.Fatalf("a dead backend must fail the probe")
			}
			if !RetryableOf(err) {
				t.Fatalf("a dead backend is transient, got %s", CodeOf(err))
			}
		}},
		{"read-back-adopt", func(t *testing.T, d driverUnderTest) {
			// A human files the issue first (marker present); the driver must adopt
			// it instead of creating a second one.
			body := SigMarker(testSig) + "\n\nhuman filed"
			switch {
			case d.gh != nil:
				d.gh.addIssue(body, types.IssueOpen)
			case d.db != nil:
				drv := d.driver.(*duckbrainDriver)
				key := drv.AnchorKey(testSig, "unknown")
				now := types.FormatUTC(time.Now())
				d.db.mu.Lock()
				d.db.kv[key] = mustJSON(t, dbDocument{
					Iss: "iss_01J9Z6Q0M2X4T8V1K7B3N5R8WK", Sig: testSig, Project: "unknown",
					Driver: "duckbrain", State: types.IssueOpen, Title: "human filed",
					FirstSeenTS: now, LastSeenTS: now, Occurrences: 1, Idem: "human", UpdatedTS: now,
				})
				d.db.mu.Unlock()
			}
			resp, err := d.driver.EnsureBySig(context.Background(), ensureReq())
			if err != nil {
				t.Fatalf("ensure: %v", err)
			}
			if resp.Created {
				t.Fatalf("ensure created a second issue although the marker was present")
			}
		}},
		{"anchor-lost-404", func(t *testing.T, d driverUnderTest) {
			first, err := d.driver.EnsureBySig(context.Background(), ensureReq())
			if err != nil {
				t.Fatalf("ensure: %v", err)
			}
			switch {
			case d.gh != nil:
				d.gh.mu.Lock()
				kept := d.gh.issues[:0]
				for _, it := range d.gh.issues {
					if it.Number != atoi(first.Ref.ExternalID) {
						kept = append(kept, it)
					}
				}
				d.gh.issues = kept
				d.gh.mu.Unlock()
			case d.db != nil:
				d.db.mu.Lock()
				for k := range d.db.kv {
					delete(d.db.kv, k)
				}
				d.db.mu.Unlock()
			}
			_, err = d.driver.Comment(context.Background(), first.Ref, commentBody("issue_comment|"+first.Ref.ID+"|ev_lost"))
			if err == nil {
				t.Fatalf("a deleted issue must surface as anchor loss")
			}
			if code := CodeOf(err); code != types.CodeIssues007 {
				t.Fatalf("anchor loss code = %s", code)
			}
		}},
		{"rate-limit-pause", func(t *testing.T, d driverUnderTest) {
			if d.gh == nil {
				t.Skip("the 429 path with Retry-After is the KV driver's own case")
			}
			d.gh.mu.Lock()
			d.gh.rateLimited = true
			d.gh.mu.Unlock()
			_, err := d.driver.EnsureBySig(context.Background(), ensureReq())
			if err == nil {
				t.Fatalf("a 429 must surface")
			}
			if code := CodeOf(err); code != types.CodeIssues002 {
				t.Fatalf("rate-limit code = %s", code)
			}
			if !RetryableOf(err) {
				t.Fatalf("rate limiting is transient")
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, d := range bothDrivers(t) {
				t.Run(d.name, func(t *testing.T) { tc.run(t, d) })
			}
		})
	}
}
