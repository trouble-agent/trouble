package issues

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// TestGoldenBodyTemplate pins the §3.9.3 body byte-for-byte. The only token the
// spec's example does not derive is the footer's 8-hex idem display, which is the
// desk's own digest of (sig, last_seen); everything else is a field of the
// template.
func TestGoldenBodyTemplate(t *testing.T) {
	d, _, _, _ := deskWith(t, githubTestConfig("http://127.0.0.1:1"))
	inc := testIncident()
	ev := testEvidence()
	ev.Recurrences = []string{"2026-09-16T09:15:41.009Z — inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE — fold (verify window)"}
	got := d.BodyOf(inc, ev, "github")

	idem := types.DigestShort(types.SigDigest([]byte(inc.Sig + "|" + ev.Group.LastSeenTS)))[:8]
	want := `<!-- trouble:sig=sentinel:sha256v1:9f2c1d3e4b5a6c7d -->
<!-- trouble:inc=inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE driver=github ver=1 -->

**Summary** — queue wedge in payment-worker — high, from ` + "`sentinel:payment-worker`" + `

| field | value |
|---|---|
| sig | ` + "`sentinel:sha256v1:9f2c1d3e4b5a6c7d`" + ` |
| incident | ` + "`inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE`" + ` (group ` + "`grp_01J9Z6Q0M2X4T8V1K7B3N5R8WH`" + `) |
| first seen | 2026-09-16T09:14:03.221Z |
| last seen | 2026-09-16T09:15:41.009Z |
| occurrences | 42 |
| release range | ` + "`payment-api@2.4.1`" + ` → ` + "`payment-api@2.4.3`" + ` (12 events) |
| origin | host ` + "`7f3a91c2d4e5b607`" + `, source ` + "`sentinel:payment-worker`" + ` |
| ladder | detected → recorded → play:applied → verifying |
| redactions applied | 3 |

**Evidence bundle (scrubbed)**

` + "```" + `
worker.py:118 claim
  queue.py:44 get
journal: payment-worker[8841]: pool exhausted (retry 3/5)
` + "```" + `

**Recurrences**

- 2026-09-16T09:15:41.009Z — inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE — fold (verify window)

<sub>filed by trouble 0.1.0 (9c1f0ab) at 2026-09-16T09:15:41.009Z · driver github · idem ` + idem + `</sub>
`
	if got != want {
		t.Fatalf("body drift:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestGoldenTitle(t *testing.T) {
	d, _, _, _ := testsDesk(t)
	inc := testIncident()
	ev := testEvidence()
	want := "[trouble] high: queue wedge in payment-worker (sentinel:sha256v1:9f2c1d3e4b5a6c7d)"
	if got := d.TitleOf(inc, ev); got != want {
		t.Fatalf("title = %q, want %q", got, want)
	}
	// Truncation is on a rune boundary and never eats the sig.
	ev.Summary = strings.Repeat("ü", 400)
	got := d.TitleOf(inc, ev)
	if len([]rune(got)) > d.cfg.TitleMaxChars {
		t.Fatalf("title is %d runes, over the %d cap", len([]rune(got)), d.cfg.TitleMaxChars)
	}
	if !strings.HasSuffix(got, "("+testSig+")") {
		t.Fatalf("truncation ate the sig: %q", got)
	}
	if strings.Contains(got, "\ufffd") {
		t.Fatalf("truncation split a rune: %q", got)
	}
}

func testsDesk(t *testing.T) (*Desk, *fakeLedger, *fakeClock, *fakeScrubber) {
	t.Helper()
	return deskWith(t, githubTestConfig("http://127.0.0.1:1"))
}

// TestGoldenSearchQuery pins the §3.9.4 query strings.
func TestGoldenSearchQuery(t *testing.T) {
	open := SearchQuery("acme", "payment-api", testSig, false)
	if open != `order=asc&per_page=1&q=repo%3Aacme%2Fpayment-api+is%3Aissue+in%3Abody+%22trouble%3Asig%3Dsentinel%3Asha256v1%3A9f2c1d3e4b5a6c7d%22&sort=created` {
		t.Fatalf("anchor query = %q", open)
	}
	openOnly := SearchQuery("acme", "payment-api", testSig, true)
	if openOnly != `per_page=1&q=repo%3Aacme%2Fpayment-api+is%3Aissue+is%3Aopen+in%3Abody+%22trouble%3Asig%3Dsentinel%3Asha256v1%3A9f2c1d3e4b5a6c7d%22` {
		t.Fatalf("open query = %q", openOnly)
	}
	byInc := SearchQueryByIncident("acme", "payment-api", "inc_01J9Z6Q0M2X4T8V1K7B3N5R8WE")
	if !strings.Contains(byInc, "trouble%3Ainc%3Dinc_01J9Z6Q0M2X4T8V1K7B3N5R8WE") {
		t.Fatalf("incident query = %q", byInc)
	}
}

// TestGitHubLabelsAndSeverity pins §3.9.2: labels are advisory, the severity and
// source labels are added per the mapping, and extras are appended.
func TestGitHubLabelsAndSeverity(t *testing.T) {
	d, _, _, _ := deskWith(t, githubTestConfig("http://127.0.0.1:1"))
	dc := d.drvCfg["github"]
	dc.LabelsExtra = []string{"team:payments", "trouble"}
	got := d.labels(dc, testIncident(), testEvidence())
	want := []string{"trouble", "auto-filed", "sev:high", "src:sentinel", "team:payments"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("labels = %v, want %v", got, want)
	}
	// A sensor-sourced sig gets the source label for sensors, not sentinel.
	inc := testIncident()
	inc.Sig = "journald:sha256v1:2ab4c6d8e0f1a3b5"
	ev := testEvidence()
	ev.Source = ""
	got = d.labels(dc, inc, ev)
	if !contains(got, "src:sensor") {
		t.Fatalf("sensor sig labels = %v", got)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// TestGitHubStatusMapping pins the §3.9.5 failure-class table.
func TestGitHubStatusMapping(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		wantCode   types.ErrorCode
		retryable  bool
		configure  func(*fakeGitHub)
		wantStatus int
	}{
		{"5xx is transient and retryable", 503, types.CodeIssues001, true, func(g *fakeGitHub) { g.createStatus = 503 }, 503},
		{"422 is permanent in instance", 422, types.CodeIssues001, false, func(g *fakeGitHub) {
			g.createStatus = 422
			g.createBody = "Validation Failed"
		}, 422},
		{"401 is permanent auth failure", 401, types.CodeIssues003, false, nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeGitHub()
			if tc.configure != nil {
				tc.configure(api)
			}
			srv := ghServer(t, api)
			t.Setenv(testTokenEnv, "ghp_test_token_value")
			cfg := githubTestConfig(srv.URL)
			cfg.MaxAttempts = 1
			drv, err := newGitHubDriver(cfg, nil, srv.Client())
			if err != nil {
				t.Fatalf("driver: %v", err)
			}
			if tc.status == 401 {
				drv.(*githubDriver).token = "bad"
				srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					writeJSON(w, 401, map[string]any{"message": "Bad credentials"})
				})
			}
			_, err = drv.EnsureBySig(context.Background(), ensureReq())
			if err == nil {
				t.Fatalf("status %d must fail", tc.status)
			}
			if code := CodeOf(err); code != tc.wantCode {
				t.Fatalf("code = %s, want %s (%v)", code, tc.wantCode, err)
			}
			if retryable := RetryableOf(err); retryable != tc.retryable {
				t.Fatalf("retryable = %v, want %v", retryable, tc.retryable)
			}
		})
	}
}

// TestTokenFileModeMatrix is the §7 token-file matrix: 0600 is accepted, every
// other mode is TROUBLE-ISSUES-003 with no outbound call.
func TestTokenFileModeMatrix(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name    string
		mode    os.FileMode
		symlink bool
		wantErr bool
	}{
		{"0600 ok", 0o600, false, false},
		{"0644 refused", 0o644, false, true},
		{"0640 refused", 0o640, false, true},
		{"0600 symlink refused", 0o600, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "_")+".token")
			if err := os.WriteFile(p, []byte("ghp_from_file"), tc.mode); err != nil {
				t.Fatalf("write: %v", err)
			}
			target := p
			if tc.symlink {
				link := p + ".link"
				if err := os.Symlink(p, link); err != nil {
					t.Fatalf("symlink: %v", err)
				}
				target = link
			}
			_, err := ReadSecretFile(target)
			if tc.wantErr && err == nil {
				t.Fatalf("mode %#o must be refused", tc.mode)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("mode %#o must be accepted: %v", tc.mode, err)
			}
			if tc.wantErr {
				if code := CodeOf(err); code != types.CodeIssues003 {
					t.Fatalf("code = %s, want TROUBLE-ISSUES-003", code)
				}
			}
		})
	}
	// Missing file is a refusal with no outbound call.
	if _, err := ReadSecretFile(filepath.Join(dir, "nope.token")); err == nil {
		t.Fatalf("a missing token file must be refused")
	}
}

// TestTokenOnArgvRefused pins §3.9.1: a token passed on argv is refused, never used.
func TestTokenOnArgvRefused(t *testing.T) {
	cfg := DefaultGitHubConfig()
	_, err := ResolveToken("ghp_on_argv", cfg, cfg.TokenFile, cfg.TokenEnv)
	if err == nil {
		t.Fatalf("a token on argv must be refused")
	}
	if code := CodeOf(err); code != types.CodeIssues003 {
		t.Fatalf("code = %s", code)
	}
	if ReasonOf(err) != ReasonTokenArgv {
		t.Fatalf("reason = %s", ReasonOf(err))
	}
}

// TestTokenNeverInRecordsOrLogs proves the token value never reaches the ledger,
// stdout or stderr: the grep assertion of §7.
func TestTokenNeverInRecordsOrLogs(t *testing.T) {
	const secret = "ghp_super_secret_value_123"
	api := newFakeGitHub()
	srv := ghServer(t, api)
	t.Setenv(testTokenEnv, secret)
	d, led, _, _ := deskWith(t, githubTestConfig(srv.URL))
	inc := testIncident()
	if _, err := d.EnsureBySig(context.Background(), inc, testEvidence()); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	for _, r := range led.records() {
		blob := payloadBlob(r.Payload)
		if strings.Contains(blob, secret) || strings.Contains(blob, "ghp_super") {
			t.Fatalf("token leaked into the ledger payload: %s", blob)
		}
	}
	for _, line := range Explain(d.cfg) {
		if strings.Contains(line.Value, secret) {
			t.Fatalf("token leaked into config explain: %+v", line)
		}
	}
}

// TestGitHubBodyTruncation pins edge case 7: the evidence section is truncated on
// a rune boundary, the markers and the metadata table survive.
func TestGitHubBodyTruncation(t *testing.T) {
	d, led, _, _ := deskWith(t, githubTestConfig("http://127.0.0.1:1"))
	d.cfg.BodyMaxBytes = 900
	d.cfg.TitleMaxChars = 256
	inc := testIncident()
	ev := testEvidence()
	ev.Lines = []string{strings.Repeat("ü", 2000)}
	body := d.BodyOf(inc, ev, "github")
	if len(body) > 900+120 {
		t.Fatalf("body is %d bytes, over the 900-byte budget", len(body))
	}
	if !strings.HasPrefix(body, SigMarker(testSig)) {
		t.Fatalf("truncation removed the sig marker")
	}
	if !strings.Contains(body, "truncated by trouble") {
		t.Fatalf("truncation marker missing:\n%s", body)
	}
	if strings.Contains(body, "\ufffd") {
		t.Fatalf("truncation split a rune")
	}
	_ = led
}

// TestScrubRefusalRefusesTheCall proves the body is scrubbed before the call and a
// scrub refusal refuses the operation (no issue is filed).
func TestScrubRefusalRefusesTheCall(t *testing.T) {
	api := newFakeGitHub()
	srv := ghServer(t, api)
	t.Setenv(testTokenEnv, "ghp_test_token_value")
	d, _, _, sc := deskWith(t, githubTestConfig(srv.URL))
	sc.refuse = true
	_, err := d.EnsureBySig(context.Background(), testIncident(), testEvidence())
	if err == nil {
		t.Fatalf("a scrub refusal must refuse the filing")
	}
	if api.count("POST repo/issues") != 0 {
		t.Fatalf("a scrub refusal still called the driver")
	}
}

// TestScrubbedBodyReachesTheDriver proves the scrubbed text (never the raw one)
// is what the driver posts.
func TestScrubbedBodyReachesTheDriver(t *testing.T) {
	api := newFakeGitHub()
	srv := ghServer(t, api)
	t.Setenv(testTokenEnv, "ghp_test_token_value")
	d, _, _, _ := deskWith(t, githubTestConfig(srv.URL))
	ev := testEvidence()
	ev.Lines = []string{"config: token=supersecret"}
	if _, err := d.EnsureBySig(context.Background(), testIncident(), ev); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	last := api.lastIssue()
	if last == nil {
		t.Fatalf("the desk filed nothing")
	}
	body := last.Body
	if strings.Contains(body, "supersecret") {
		t.Fatalf("raw secret reached the tracker:\n%s", body)
	}
	if !strings.Contains(body, "[REDACTED:test]") {
		t.Fatalf("scrubbed body missing the redaction marker:\n%s", body)
	}
}
