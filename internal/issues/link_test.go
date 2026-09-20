package issues

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// TestOneBugThreePathsIsOneIssue is the AC-22 desk half: the same underlying bug
// arriving through the sensor, sentinel and collector paths produces one issue,
// folded — never a duplicate.
func TestOneBugThreePathsIsOneIssue(t *testing.T) {
	api := newFakeGitHub()
	srv := ghServer(t, api)
	t.Setenv(testTokenEnv, "ghp_test_token_value")
	d, led, clk, _ := deskWith(t, githubTestConfig(srv.URL))
	paths := []string{"sensor:rule-queue-wedge", "sentinel:payment-worker", "collector:go-panic"}
	for i, p := range paths {
		inc := testIncident()
		inc.ID = types.NewID(types.PInc)
		ev := testEvidence()
		ev.Source = p
		if i > 0 {
			clk.advance(11 * time.Minute) // outside comment_min_interval
		}
		if _, err := d.EnsureBySig(context.Background(), inc, ev); err != nil {
			t.Fatalf("path %s: %v", p, err)
		}
	}
	if got := len(api.issues); got != 1 {
		t.Fatalf("%d issues for one underlying bug, want 1", got)
	}
	creates, folds := 0, 0
	for _, r := range led.byOp("ensure") {
		if boolPayload(r.Payload, "created") {
			creates++
		}
	}
	for _, r := range led.byOp("comment") {
		if boolPayload(r.Payload, "commented") {
			folds++
		}
	}
	if creates != 1 {
		t.Fatalf("%d create records, want 1", creates)
	}
	if folds < 2 {
		t.Fatalf("%d fold comments, want >= 2 (the two recurrences)", folds)
	}
}

// TestLinkWiresTaskAndBrief pins §3.12: the desk appends the cross-reference
// comment, sets the linkage fields, and records op=link.
func TestLinkWiresTaskAndBrief(t *testing.T) {
	api := newFakeGitHub()
	srv := ghServer(t, api)
	t.Setenv(testTokenEnv, "ghp_test_token_value")
	d, led, _, _ := deskWith(t, githubTestConfig(srv.URL))
	ref, err := d.EnsureBySig(context.Background(), testIncident(), testEvidence())
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	taskID := "tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ"
	brief := "res_01J9Z6Q0M2X4T8V1K7B3N5R8WL"
	out, err := d.Link(context.Background(), ref, taskID, brief)
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	if out.TaskID != taskID || out.ResearchID != brief {
		t.Fatalf("linkage = %q / %q", out.TaskID, out.ResearchID)
	}
	it := api.lastIssue()
	var sawTask, sawBrief bool
	for _, c := range it.Comments {
		if strings.Contains(c, "linked: "+taskID+" (board-jsonl)") {
			sawTask = true
		}
		if strings.Contains(c, "research: "+brief) {
			sawBrief = true
		}
	}
	if !sawTask || !sawBrief {
		t.Fatalf("cross-reference comments missing: %v", it.Comments)
	}
	links := led.byOp("link")
	if len(links) != 1 {
		t.Fatalf("%d link records, want 1", len(links))
	}
	if strPayload(links[0].Payload, "task_id") != taskID || strPayload(links[0].Payload, "research_id") != brief {
		t.Fatalf("link record = %v", links[0].Payload)
	}
	// The row writer (SPEC-08) can look the anchor up by sig, both directions.
	got, ok := d.AnchorForRow(testSig)
	if !ok || got.TaskID != taskID {
		t.Fatalf("AnchorForRow = %+v ok=%v", got, ok)
	}
}

// TestFiveHundredRecurrencesStayOneIssue is the AC-22 volume assertion: 500
// synthetic recurrences produce one issue and no duplicate comments.
func TestFiveHundredRecurrencesStayOneIssue(t *testing.T) {
	api := newFakeGitHub()
	srv := ghServer(t, api)
	t.Setenv(testTokenEnv, "ghp_test_token_value")
	d, _, clk, _ := deskWith(t, githubTestConfig(srv.URL))
	inc := testIncident()
	ev := testEvidence()
	if _, err := d.EnsureBySig(context.Background(), inc, ev); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	for i := 0; i < 500; i++ {
		clk.advance(11 * time.Minute)
		inc.ID = types.NewID(types.PInc)
		_, _ = d.EnsureBySig(context.Background(), inc, ev)
	}
	if got := len(api.issues); got != 1 {
		t.Fatalf("%d issues after 500 recurrences, want 1", got)
	}
	it := api.lastIssue()
	seen := map[string]int{}
	for _, c := range it.Comments {
		if m := idemFromBody(c); m != "" {
			seen[m]++
		}
	}
	for m, n := range seen {
		if n > 1 {
			t.Fatalf("duplicate comment %s (%d times)", m, n)
		}
	}
}

// TestAnchorNeverInventsAnID pins the "a value exists only after a driver
// returned a ref" rule: a lookup for an unknown sig returns nothing.
func TestAnchorNeverInventsAnID(t *testing.T) {
	d, _, _, _ := deskWith(t, githubTestConfig("http://127.0.0.1:1"))
	if _, ok := d.Anchor("sentinel:sha256v1:0000000000000000"); ok {
		t.Fatalf("a lookup for an unfiled sig returned a ref")
	}
	if _, ok := d.AnchorForRow("journald:sha256v1:2ab4c6d8e0f1a3b5"); ok {
		t.Fatalf("a lookup for an unfiled sig returned a ref")
	}
}
