package issues

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

type quietFixture struct {
	inc       types.Incident
	group     types.Group
	openRow   bool
	openBrief bool
}

func (f *quietFixture) noRow(context.Context, string) (bool, error)   { return f.openRow, nil }
func (f *quietFixture) noBrief(context.Context, string) (bool, error) { return f.openBrief, nil }

// quietDesk builds a desk over a controllable incident projection and files one
// issue, which is the state the quiet-close sweep sees in production.
func quietDesk(t *testing.T, fx *quietFixture) (*Desk, *fakeLedger, *fakeClock, *fakeGitHub) {
	t.Helper()
	api := newFakeGitHub()
	srv := ghServer(t, api)
	t.Setenv(testTokenEnv, "ghp_test_token_value")
	cfg := enabledDefaults()
	cfg.Drivers = []types.IssueDriverConfig{githubTestConfig(srv.URL)}
	cfg.PrimaryDriver = "github"
	cfg.OpDeadline = "2s"
	clk := newFakeClock()
	led := &fakeLedger{now: clk.Now}
	d, err := New(cfg, Deps{
		Ledger: led, Scan: led, Scrub: &fakeScrubber{}, Clock: clk,
		HostID: "7f3a91c2d4e5b607", Actor: testActor(),
		StateRoot: t.TempDir(),
		ProjectOf: func(types.Incident) string { return "1" },
		Group: func(context.Context, string) (types.Group, bool, error) {
			return fx.group, true, nil
		},
		Incident: func(context.Context, string) (types.Incident, bool, error) {
			return fx.inc, true, nil
		},
		OpenBoardRow: fx.noRow,
		OpenBrief:    fx.noBrief,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := d.EnsureBySig(context.Background(), fx.inc, testEvidence()); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	return d, led, clk, api
}

// resolvedFixture is the quiet, resolved, evidence-passed state of §3.11.
func resolvedFixture() *quietFixture {
	inc := testIncident()
	inc.State = types.StResolved
	inc.ResolvedTS = "2026-09-16T10:00:00.000Z"
	inc.Evidence = &types.Evidence{
		EventsObserved:  0,
		CanarySeen:      true,
		WindowS:         600,
		SourcesExpected: []string{"sentinel", "journald"},
		SourcesAlive:    []string{"sentinel", "journald"},
		Result:          types.VerifyPassed,
	}
	group := testEvidence().Group
	group.LastSeenTS = "2026-09-16T09:55:00.000Z"
	return &quietFixture{inc: inc, group: group}
}

// TestQuietCloseClosesOnce pins the happy path: 24 h of quiet after resolution
// closes the issue with the reason comment, exactly once.
func TestQuietCloseClosesOnce(t *testing.T) {
	fx := resolvedFixture()
	d, led, clk, api := quietDesk(t, fx)
	clk.advance(25 * time.Hour) // resolved 10:00 + 24h quiet, plus a margin
	n, err := d.SweepQuietClose(context.Background(), clk.Now())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("closed %d issues, want 1", n)
	}
	it := api.lastIssue()
	if it == nil || it.State != types.IssueClosed {
		t.Fatalf("issue state = %+v", it)
	}
	// The reason comment carries the §3.11 template and the idem marker.
	var reason string
	for _, c := range it.Comments {
		if strings.Contains(c, "Closed by trouble: sig quiet") {
			reason = c
		}
	}
	if reason == "" {
		t.Fatalf("no close reason comment: %v", it.Comments)
	}
	for _, want := range []string{"- incident:", "- evidence:", "- resolved:", "quiet period: 24h", "ledger: last seq", "trouble:idem="} {
		if !strings.Contains(reason, want) {
			t.Fatalf("reason comment is missing %q:\n%s", want, reason)
		}
	}
	// Two sweeps produce one close.
	clk.advance(5 * time.Minute)
	n, err = d.SweepQuietClose(context.Background(), clk.Now())
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("the second sweep closed %d issues, want 0", n)
	}
	closes := led.byOp("close")
	if len(closes) != 1 {
		t.Fatalf("%d close records, want 1", len(closes))
	}
	if strPayload(closes[0].Payload, "result") != "closed" {
		t.Fatalf("close record = %v", closes[0].Payload)
	}
	if api.count("PATCH repo/issues/{n}") != 1 {
		t.Fatalf("PATCH count = %d, want 1", api.count("PATCH repo/issues/{n}"))
	}
	// The close record carries the §3.3 key set (the table is asserted op-wide by
	// ledger_payload_test.go; this is the close-specific half).
	for _, k := range payloadSchema["close"] {
		if _, ok := closes[0].Payload[k]; !ok {
			t.Fatalf("close record is missing %q: %v", k, closes[0].Payload)
		}
	}
}

// TestQuietCloseGates pins every gate of §3.11: each one alone stops the close.
func TestQuietCloseGates(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(*quietFixture)
		desk  func(*Desk)
		delay time.Duration
	}{
		{"recurrence inside the quiet period", func(f *quietFixture) {
			f.group.LastSeenTS = "2026-09-17T09:30:00.000Z" // after resolution
		}, nil, 25 * time.Hour},
		{"quiet period not elapsed", nil, nil, 23*time.Hour + 59*time.Minute},
		{"evidence not passed", func(f *quietFixture) { f.inc.Evidence.Result = types.VerifyFailed }, nil, 25 * time.Hour},
		{"no evidence at all", func(f *quietFixture) { f.inc.Evidence = nil }, nil, 25 * time.Hour},
		{"open board row", func(f *quietFixture) { f.openRow = true }, nil, 25 * time.Hour},
		{"open research brief", func(f *quietFixture) { f.openBrief = true }, nil, 25 * time.Hour},
		{"escalated incident", func(f *quietFixture) { f.inc.State = types.StEscalated }, nil, 25 * time.Hour},
		{"reopen count moved", func(f *quietFixture) { f.inc.ReopenCount = 2 }, nil, 25 * time.Hour},
		{"manual anchor", nil, func(d *Desk) { d.MarkManual("github", testSig, "1") }, 25 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := resolvedFixture()
			if tc.mut != nil {
				tc.mut(fx)
			}
			d, _, clk, api := quietDesk(t, fx)
			if tc.desk != nil {
				tc.desk(d)
			}
			clk.advance(tc.delay)
			n, err := d.SweepQuietClose(context.Background(), clk.Now())
			if err != nil {
				t.Fatalf("sweep: %v", err)
			}
			if n != 0 {
				t.Fatalf("the gate did not stop the close (%d closed)", n)
			}
			if it := api.lastIssue(); it != nil && it.State == types.IssueClosed {
				t.Fatalf("the issue was closed through a failing gate")
			}
			if api.count("PATCH repo/issues/{n}") != 0 {
				t.Fatalf("a close transition reached the driver")
			}
		})
	}
}

// TestQuietCloseDefersWhileTheDriverIsDown pins gate 5: the close is deferred,
// never dropped, and happens on a later sweep after recovery.
func TestQuietCloseDefersWhileTheDriverIsDown(t *testing.T) {
	fx := resolvedFixture()
	d, led, clk, api := quietDesk(t, fx)
	clk.advance(25 * time.Hour)
	// The driver goes down: two consecutive failed probes mark it failed.
	api.mu.Lock()
	api.down = true
	api.mu.Unlock()
	d.probe(context.Background(), "github")
	d.probe(context.Background(), "github")
	if !d.driverFailed("github") {
		t.Fatalf("the driver was not marked failed")
	}
	n, err := d.SweepQuietClose(context.Background(), clk.Now())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("closed %d issues while the driver was down, want 0", n)
	}
	if len(led.byOp("close")) != 0 {
		t.Fatalf("a close was recorded while the driver was down")
	}
	if len(led.gaps()) == 0 {
		t.Fatalf("the outage produced no gap record")
	}
	// Recovery, then the next sweep closes.
	api.mu.Lock()
	api.down = false
	api.mu.Unlock()
	d.probe(context.Background(), "github")
	clk.advance(5 * time.Minute)
	n, err = d.SweepQuietClose(context.Background(), clk.Now())
	if err != nil {
		t.Fatalf("sweep after recovery: %v", err)
	}
	if n != 1 {
		t.Fatalf("closed %d after recovery, want 1", n)
	}
}

// TestQuietCloseRefusedLeavesItOpen pins the close-refusal path: the issue stays
// open with a ledger note, and a later sweep (after the lock clears) closes it.
func TestQuietCloseRefusedLeavesItOpen(t *testing.T) {
	fx := resolvedFixture()
	d, led, clk, api := quietDesk(t, fx)
	api.mu.Lock()
	api.locked = true
	api.mu.Unlock()
	clk.advance(25 * time.Hour)
	n, err := d.SweepQuietClose(context.Background(), clk.Now())
	if err != nil && CodeOf(err) != types.CodeIssues008 {
		t.Fatalf("sweep error = %v", err)
	}
	if n != 0 {
		t.Fatalf("closed %d issues through a locked issue, want 0", n)
	}
	var noted bool
	for _, r := range led.byOp("close") {
		if strPayload(r.Payload, "error_code") == string(types.CodeIssues008) {
			noted = true
		}
	}
	if !noted {
		t.Fatalf("the refused close was not recorded: %v", led.ops())
	}
	if it := api.lastIssue(); it.State == types.IssueClosed {
		t.Fatalf("a locked issue was closed anyway")
	}
	api.mu.Lock()
	api.locked = false
	api.mu.Unlock()
	clk.advance(5 * time.Minute)
	if n, err := d.SweepQuietClose(context.Background(), clk.Now()); err != nil || n != 1 {
		t.Fatalf("after unlock: closed=%d err=%v", n, err)
	}
}

// TestReopenAfterCloseKeepsTheSameIssue pins §3.11 on the desk: a recurrence
// after a close reopens the same external id and creates nothing new.
func TestReopenAfterCloseKeepsTheSameIssue(t *testing.T) {
	fx := resolvedFixture()
	d, led, clk, api := quietDesk(t, fx)
	external := api.lastIssue().Number
	clk.advance(25 * time.Hour)
	if _, err := d.SweepQuietClose(context.Background(), clk.Now()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if api.lastIssue().State != types.IssueClosed {
		t.Fatalf("precondition: the issue is not closed")
	}
	// Recurrence: the incident is live again.
	fx.inc.State = types.StVerifying
	fx.inc.ResolvedTS = ""
	fx.group.LastSeenTS = types.FormatUTC(clk.Now())
	ref, err := d.EnsureBySig(context.Background(), fx.inc, testEvidence())
	if err != nil {
		t.Fatalf("ensure after close: %v", err)
	}
	if api.count("POST repo/issues") != 1 {
		t.Fatalf("the reopen filed a second issue (%d creates)", api.count("POST repo/issues"))
	}
	if ref.ExternalID != itoa(external) {
		t.Fatalf("reopen moved to a different issue: %s vs %d", ref.ExternalID, external)
	}
	if got := api.lastIssue().State; got != types.IssueOpen {
		t.Fatalf("state after the reopen = %q", got)
	}
	var reopens []types.Record
	for _, r := range led.records() {
		if strPayload(r.Payload, "op") == "reopen" {
			reopens = append(reopens, r)
		}
	}
	if len(reopens) != 1 {
		t.Fatalf("%d reopen records, want 1", len(reopens))
	}
	for _, k := range payloadSchema["reopen"] {
		if _, ok := reopens[0].Payload[k]; !ok {
			t.Fatalf("reopen record is missing %q: %v", k, reopens[0].Payload)
		}
	}
	if strPayload(reopens[0].Payload, "result") != "reopened" {
		t.Fatalf("reopen record = %v", reopens[0].Payload)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// TestQuietCloseGateReport is the read surface's diagnostic: it names each gate
// and why it failed.
func TestQuietCloseGateReport(t *testing.T) {
	fx := resolvedFixture()
	fx.openRow = true
	d, _, clk, _ := quietDesk(t, fx)
	clk.advance(25 * time.Hour)
	gates, deferred := d.QuietGateReport(context.Background(), "github", testSig, clk.Now())
	if deferred {
		t.Fatalf("the report deferred although every gate was resolvable")
	}
	var sawRow bool
	for _, g := range gates {
		if g.Name == "no_open_board_row" && !g.OK {
			sawRow = true
		}
	}
	if !sawRow {
		t.Fatalf("the open board row gate did not report: %+v", gates)
	}
}
