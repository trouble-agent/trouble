package issues

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// TestDuckbrainKeyLayout pins the §3.10 namespace/key layout.
func TestDuckbrainKeyLayout(t *testing.T) {
	api := newFakeDuckbrain()
	srv := dbServer(t, api)
	drv, err := newDuckbrainDriver(duckbrainTestConfig(srv.URL), nil, srv.Client())
	if err != nil {
		t.Fatalf("driver: %v", err)
	}
	d := drv.(*duckbrainDriver)
	if got, want := d.AnchorKey(testSig, "1"), "trouble.issues.1.9f2c1d3e4b5a6c7d"; got != want {
		t.Fatalf("anchor key = %q, want %q", got, want)
	}
	ck := d.CommentKey(testSig, "1", "issue_comment|iss_X|ev_Y")
	if !strings.HasPrefix(ck, "trouble.issues.1.9f2c1d3e4b5a6c7d.c.") {
		t.Fatalf("comment key = %q", ck)
	}
	// One key per comment: two markers differ, one marker is stable.
	if ck == d.CommentKey(testSig, "1", "issue_comment|iss_X|ev_Z") {
		t.Fatalf("two markers produced one comment key")
	}
	if ck != d.CommentKey(testSig, "1", "issue_comment|iss_X|ev_Y") {
		t.Fatalf("the same marker produced two comment keys")
	}
	if got := d.IndexKey(testSig, "1"); got != "trouble.issues.1.9f2c1d3e4b5a6c7d.i" {
		t.Fatalf("index key = %q", got)
	}
}

// TestDuckbrainReadbackMismatch pins the §3.10 "the write response is never
// trusted" rule: a backend that accepts a write but stores something else is a
// TROUBLE-ISSUES-006 retry, not a success.
func TestDuckbrainReadbackMismatch(t *testing.T) {
	api := newFakeDuckbrain()
	api.readback = true // PUT stores a perturbed document
	srv := dbServer(t, api)
	drv, err := newDuckbrainDriver(duckbrainTestConfig(srv.URL), nil, srv.Client())
	if err != nil {
		t.Fatalf("driver: %v", err)
	}
	_, err = drv.EnsureBySig(context.Background(), ensureReq())
	if err == nil {
		t.Fatalf("a read-back mismatch must fail")
	}
	if code := CodeOf(err); code != types.CodeIssues006 {
		t.Fatalf("code = %s, want TROUBLE-ISSUES-006", code)
	}
	if !RetryableOf(err) {
		t.Fatalf("a read-back mismatch is transient")
	}
}

// TestDuckbrainBackendDownIsTransient pins the outage classification.
func TestDuckbrainBackendDownIsTransient(t *testing.T) {
	api := newFakeDuckbrain()
	api.down = true
	srv := dbServer(t, api)
	drv, err := newDuckbrainDriver(duckbrainTestConfig(srv.URL), nil, srv.Client())
	if err != nil {
		t.Fatalf("driver: %v", err)
	}
	_, err = drv.EnsureBySig(context.Background(), ensureReq())
	if err == nil {
		t.Fatalf("a dead backend must fail")
	}
	if code := CodeOf(err); code != types.CodeIssues001 {
		t.Fatalf("code = %s, want TROUBLE-ISSUES-001", code)
	}
	if !RetryableOf(err) {
		t.Fatalf("a dead backend must be spoolable")
	}
}

// TestDuckbrainHeaderNameOnlyConfig pins §3.4/§3.10: the header NAME is
// configuration, no key value is ever rendered.
func TestDuckbrainHeaderNameOnlyConfig(t *testing.T) {
	cfg := DefaultConfig()
	lines := Explain(cfg)
	var sawHeader, sawKeyFile bool
	for _, l := range lines {
		if strings.HasSuffix(l.Key, ".api_key_header") {
			sawHeader = true
			if l.Value != "Authorization" {
				t.Fatalf("api_key_header rendered as %q", l.Value)
			}
		}
		if strings.HasSuffix(l.Key, ".api_key_file") {
			sawKeyFile = true
			if !l.Redacted {
				t.Fatalf("api_key_file must be redacted")
			}
		}
		if strings.Contains(l.Value, "Bearer ") {
			t.Fatalf("a scheme+value leaked into config explain: %+v", l)
		}
	}
	if !sawHeader || !sawKeyFile {
		t.Fatalf("config explain is missing the duckbrain credential lines")
	}
}

// TestDuckbrainAuthHeaderUsesConfiguredName proves the header name is honoured.
func TestDuckbrainAuthHeaderUsesConfiguredName(t *testing.T) {
	api := newFakeDuckbrain()
	api.header = "X-Api-Key"
	api.value = "kb_test_key"
	srv := dbServer(t, api)
	cfg := duckbrainTestConfig(srv.URL)
	cfg.APIKeyHeader = "X-Api-Key"
	cfg.APIKeyEnv = ""
	cfg.APIKeyFile = ""
	t.Setenv(testTokenEnv, "unused")
	drv, err := newDuckbrainDriver(cfg, nil, srv.Client())
	if err != nil {
		t.Fatalf("driver: %v", err)
	}
	drv.(*duckbrainDriver).key = "kb_test_key"
	if _, err := drv.Healthcheck(context.Background()); err != nil {
		t.Fatalf("healthcheck with the configured header: %v", err)
	}
	// A wrong header name is an auth failure, not a silent success.
	drv.(*duckbrainDriver).cfg.APIKeyHeader = "Authorization"
	if _, err := drv.Healthcheck(context.Background()); err == nil {
		t.Fatalf("a wrong header name must fail")
	}
}

// TestDuckbrainCloseThenReopenSameKey pins §3.11 on the KV backend: the anchor
// key never changes, so the reconnect is the same issue.
func TestDuckbrainCloseThenReopenSameKey(t *testing.T) {
	api := newFakeDuckbrain()
	srv := dbServer(t, api)
	drv, err := newDuckbrainDriver(duckbrainTestConfig(srv.URL), nil, srv.Client())
	if err != nil {
		t.Fatalf("driver: %v", err)
	}
	resp, err := drv.EnsureBySig(context.Background(), ensureReq())
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	closed, err := drv.Close(context.Background(), resp.Ref, "resolved")
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if closed.State != types.IssueClosed {
		t.Fatalf("state = %q", closed.State)
	}
	again, err := drv.Close(context.Background(), closed, "resolved")
	if err != nil || again.State != types.IssueClosed {
		t.Fatalf("second close must be a no-op success, got %v / %q", err, again.State)
	}
	out, err := drv.(Reopener).Reopen(context.Background(), closed, "recurrence")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if out.ExternalID != resp.Ref.ExternalID {
		t.Fatalf("reopen changed the anchor key: %s vs %s", out.ExternalID, resp.Ref.ExternalID)
	}
	if out.State != types.IssueOpen {
		t.Fatalf("state after reopen = %q", out.State)
	}
}

// TestDuckbrainAnchorAdoption proves the KV driver folds into the existing
// document instead of creating a second one.
func TestDuckbrainAnchorAdoption(t *testing.T) {
	api := newFakeDuckbrain()
	srv := dbServer(t, api)
	drv, err := newDuckbrainDriver(duckbrainTestConfig(srv.URL), nil, srv.Client())
	if err != nil {
		t.Fatalf("driver: %v", err)
	}
	first, err := drv.EnsureBySig(context.Background(), ensureReq())
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	before := len(api.keys())
	second, err := drv.EnsureBySig(context.Background(), ensureReq())
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if second.Created {
		t.Fatalf("the second ensure created a second document")
	}
	if second.Ref.ExternalID != first.Ref.ExternalID {
		t.Fatalf("adoption moved to a different key: %s vs %s", second.Ref.ExternalID, first.Ref.ExternalID)
	}
	after := api.keys()
	if len(after) <= before {
		t.Fatalf("the fold appended no comment key: %v", after)
	}
	c := 0
	for _, k := range after {
		if strings.Contains(k, ".c.") {
			c++
		}
	}
	if c != 1 {
		t.Fatalf("fold produced %d comment keys, want 1", c)
	}
}

// TestDriverRefsCarryTheLocalIDOnReadBack pins that a read-back never drops the
// desk-owned iss_ id or the linkage fields.
func TestDriverRefsCarryTheLocalIDOnReadBack(t *testing.T) {
	api := newFakeGitHub()
	srv := ghServer(t, api)
	t.Setenv(testTokenEnv, "ghp_test_token_value")
	drv, err := newGitHubDriver(githubTestConfig(srv.URL), nil, srv.Client())
	if err != nil {
		t.Fatalf("driver: %v", err)
	}
	resp, err := drv.EnsureBySig(context.Background(), ensureReq())
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	ref := resp.Ref
	ref.ID = types.NewID(types.PIss)
	ref.TaskID = "tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ"
	out, err := drv.Comment(context.Background(), ref, commentBody("issue_comment|"+ref.ID+"|ev_1"))
	if err != nil {
		t.Fatalf("comment: %v", err)
	}
	if out.ID != ref.ID {
		t.Fatalf("read-back replaced the local id: %s vs %s", out.ID, ref.ID)
	}
	if out.TaskID != ref.TaskID {
		t.Fatalf("read-back dropped the task link")
	}
}

// TestGitHubSpoolingOnTransientFailure pins the desk-level spool decision: a
// retryable failure spools, a permanent one does not.
func TestGitHubSpoolingOnTransientFailure(t *testing.T) {
	api := newFakeGitHub()
	api.createStatus = 503
	srv := ghServer(t, api)
	t.Setenv(testTokenEnv, "ghp_test_token_value")
	d, led, _, _ := deskWith(t, githubTestConfig(srv.URL))
	d.cfg.OpDeadline = "20ms"
	d.drvCfg["github"] = func() types.IssueDriverConfig {
		c := githubTestConfig(srv.URL)
		c.MaxAttempts = 2
		c.BaseBackoff = "1ms"
		c.MaxBackoff = "2ms"
		return c
	}()
	// Rebuild the driver with the same knobs so the desk's copy agrees.
	drv, err := newGitHubDriver(d.drvCfg["github"], nil, srv.Client())
	if err != nil {
		t.Fatalf("driver: %v", err)
	}
	d.drivers["github"] = drv

	_, err = d.EnsureBySig(context.Background(), testIncident(), testEvidence())
	if err == nil {
		t.Fatalf("a 5xx must surface")
	}
	if !RetryableOf(err) {
		t.Fatalf("5xx must be retryable")
	}
	if n := d.spoolCount("github"); n != 1 {
		t.Fatalf("spool holds %d entries, want 1", n)
	}
	spooled := led.byOp("ensure")
	var sawSpooled bool
	for _, r := range spooled {
		if strPayload(r.Payload, "result") == "spooled" {
			sawSpooled = true
		}
	}
	if !sawSpooled {
		t.Fatalf("no ensure record reported result=spooled: %v", led.ops())
	}

	// A permanent refusal (422) is never spooled.
	d.spool = &spoolStore{root: "", cfg: d.cfg, now: d.now, memOnly: true}
	api.mu.Lock()
	api.createStatus = 422
	api.createBody = "Validation Failed"
	api.mu.Unlock()
	_, err = d.EnsureBySig(context.Background(), testIncident(), testEvidence())
	if err == nil {
		t.Fatalf("422 must surface")
	}
	if RetryableOf(err) {
		t.Fatalf("422 must not be retryable")
	}
	if n := d.spoolCount("github"); n != 0 {
		t.Fatalf("a permanent refusal entered the spool (%d entries)", n)
	}
}

// TestGitHubSpoolReplayIsDuplicateFree pins §3.7 rule 3 on the real path: a
// spooled ensure replays into the same issue, never a second one.
func TestGitHubSpoolReplayIsDuplicateFree(t *testing.T) {
	api := newFakeGitHub()
	api.createStatus = 503
	srv := ghServer(t, api)
	t.Setenv(testTokenEnv, "ghp_test_token_value")
	d, _, clk, _ := deskWith(t, githubTestConfig(srv.URL))
	cfg := githubTestConfig(srv.URL)
	cfg.MaxAttempts = 1
	d.drvCfg["github"] = cfg
	drv, err := newGitHubDriver(cfg, d, srv.Client())
	if err != nil {
		t.Fatalf("driver: %v", err)
	}
	d.drivers["github"] = drv

	if _, err := d.EnsureBySig(context.Background(), testIncident(), testEvidence()); err == nil {
		t.Fatalf("expected the 5xx to surface")
	}
	if n := d.spoolCount("github"); n != 1 {
		t.Fatalf("spool = %d, want 1", n)
	}
	// The backend recovers; the replay must file exactly one issue.
	api.mu.Lock()
	api.createStatus = 0
	api.mu.Unlock()
	clk.advance(time.Minute)
	n, err := d.Replay(context.Background(), 10)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if n != 1 {
		t.Fatalf("replayed %d entries, want 1", n)
	}
	if left := d.spoolCount("github"); left != 0 {
		t.Fatalf("spool not drained: %d left", left)
	}
	if got := len(api.issues); got != 1 {
		t.Fatalf("replay produced %d issues, want 1", got)
	}
	// A second trigger must fold into that same issue.
	clk.advance(31 * time.Minute)
	if _, err := d.EnsureBySig(context.Background(), testIncident(), testEvidence()); err != nil {
		t.Fatalf("post-replay ensure: %v", err)
	}
	if got := len(api.issues); got != 1 {
		t.Fatalf("recurrence created a duplicate issue: %d issues", got)
	}
}
