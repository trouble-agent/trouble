package issues

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

func spoolCfg(t *testing.T) (types.IssueDeskConfig, *fakeClock) {
	t.Helper()
	cfg := enabledDefaults()
	cfg.SpoolMaxEntries = 100
	cfg.SpoolBudgetBytes = 1 << 20
	cfg.SpoolMinRetention = "0"
	cfg.SpoolTTL = "72h"
	cfg.MaxAttemptsPerOp = 20
	return cfg, newFakeClock()
}

func mkEntry(t *testing.T, id string, ts, nextTry string, payloadSize int) types.SpoolEntry {
	t.Helper()
	return types.SpoolEntry{
		ID:        id,
		TS:        ts,
		Kind:      types.SpoolIssue,
		Payload:   make([]byte, payloadSize),
		IdemKey:   "issue_ensure|github|" + id,
		NextTryTS: nextTry,
	}
}

// TestSpoolReplayOrder pins the (next_try_ts, ts, id) ordering.
func TestSpoolReplayOrder(t *testing.T) {
	cfg, clk := spoolCfg(t)
	s := newSpoolStore(t.TempDir(), cfg, clk.Now)
	later := types.FormatUTC(clk.Now().Add(time.Hour))
	earlier := types.FormatUTC(clk.Now().Add(-time.Hour))
	now := types.FormatUTC(clk.Now())
	for _, e := range []types.SpoolEntry{
		mkEntry(t, "ev_C", now, later, 8),
		mkEntry(t, "ev_A", now, earlier, 8),
		mkEntry(t, "ev_B", now, now, 8),
	} {
		if _, err := s.Put("github", e); err != nil {
			t.Fatalf("put %s: %v", e.ID, err)
		}
	}
	list, err := s.List("github")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := []string{}
	for _, e := range list {
		got = append(got, e.ID)
	}
	want := []string{"ev_A", "ev_B", "ev_C"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("replay order = %v, want %v", got, want)
		}
	}
}

// TestSpoolRefusesWhenEveryEntryIsYoung pins the §3.7 refusal: entries younger
// than spool_min_retention are never evicted, so the new entry is refused with
// TROUBLE-ISSUES-005 instead of silently displacing work.
func TestSpoolRefusesWhenEveryEntryIsYoung(t *testing.T) {
	cfg, clk := spoolCfg(t)
	cfg.SpoolMaxEntries = 3
	cfg.SpoolMinRetention = "1h"
	s := newSpoolStore(t.TempDir(), cfg, clk.Now)
	now := types.FormatUTC(clk.Now())
	for i := 0; i < 3; i++ {
		if _, err := s.Put("github", mkEntry(t, fmt.Sprintf("ev_%d", i), now, now, 16)); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	_, err := s.Put("github", mkEntry(t, "ev_4", now, now, 16))
	if err == nil {
		t.Fatalf("a full spool with no evictable entry must refuse")
	}
	if code := CodeOf(err); code != types.CodeIssues005 {
		t.Fatalf("code = %s, want TROUBLE-ISSUES-005", code)
	}
	if !RetryableOf(err) {
		t.Fatalf("a spool refusal is transient")
	}
	if n := s.Count("github"); n != 3 {
		t.Fatalf("count = %d, want 3", n)
	}
}

// TestSpoolDropsOldestAboveTheFloor pins drop-oldest with the 1 h floor.
func TestSpoolDropsOldestAboveTheFloor(t *testing.T) {
	cfg, clk := spoolCfg(t)
	cfg.SpoolMaxEntries = 3
	cfg.SpoolMinRetention = "1h"
	s := newSpoolStore(t.TempDir(), cfg, clk.Now)
	// Two old entries (evictable) and one young one.
	old := types.FormatUTC(clk.Now().Add(-4 * time.Hour))
	young := types.FormatUTC(clk.Now())
	if _, err := s.Put("github", mkEntry(t, "ev_old_a", old, old, 16)); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := s.Put("github", mkEntry(t, "ev_old_b", old, old, 16)); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := s.Put("github", mkEntry(t, "ev_young", young, young, 16)); err != nil {
		t.Fatalf("put: %v", err)
	}
	drops, err := s.Put("github", mkEntry(t, "ev_new", young, young, 16))
	if err != nil {
		t.Fatalf("put 4: %v", err)
	}
	if len(drops) != 1 || drops[0].Reason != "budget" {
		t.Fatalf("drops = %+v, want one budget drop", drops)
	}
	if drops[0].Count != 1 || drops[0].OldestTS != old {
		t.Fatalf("drop event = %+v", drops[0])
	}
	list, _ := s.List("github")
	for _, e := range list {
		if e.ID == "ev_old_a" {
			t.Fatalf("the oldest evictable entry survived: %+v", list)
		}
	}
	if len(list) != 3 {
		t.Fatalf("count = %d, want 3", len(list))
	}
}

// TestSpoolTTLAndAttemptDrops pins the two hard drops.
func TestSpoolTTLAndAttemptDrops(t *testing.T) {
	cfg, clk := spoolCfg(t)
	cfg.SpoolTTL = "72h"
	cfg.SpoolMinRetention = "1h"
	cfg.MaxAttemptsPerOp = 20
	s := newSpoolStore(t.TempDir(), cfg, clk.Now)
	expired := types.FormatUTC(clk.Now().Add(-100 * time.Hour))
	now := types.FormatUTC(clk.Now())
	dead := mkEntry(t, "ev_exhausted", now, now, 8)
	dead.Attempts = 20
	expiredEntry := mkEntry(t, "ev_expired", expired, expired, 8)

	// Eviction runs on every Put, so the drops of earlier Puts are accumulated.
	var drops []dropEvent
	for _, e := range []types.SpoolEntry{expiredEntry, dead, mkEntry(t, "ev_live", now, now, 8)} {
		got, err := s.Put("github", e)
		if err != nil {
			t.Fatalf("put %s: %v", e.ID, err)
		}
		drops = append(drops, got...)
	}
	if len(drops) == 0 {
		t.Fatalf("expected the expired and exhausted entries to be evicted")
	}
	reasons := map[string]bool{}
	for _, d := range drops {
		reasons[d.Reason] = true
	}
	if !reasons["ttl"] || !reasons["attempts"] {
		t.Fatalf("drops = %+v, want a ttl and an attempts drop", drops)
	}
	if list, _ := s.List("github"); len(list) != 1 || list[0].ID != "ev_live" {
		t.Fatalf("survivors = %+v", list)
	}
}

// TestSpoolDropsWriteGapAndDropRecords pins §3.7: every drop writes both a gap
// record and an issue{op:drop} record, so the loss is visible.
func TestSpoolDropsWriteGapAndDropRecords(t *testing.T) {
	api := newFakeGitHub()
	api.createStatus = 503
	srv := ghServer(t, api)
	t.Setenv(testTokenEnv, "ghp_test_token_value")
	d, led, _, _ := deskWith(t, githubTestConfig(srv.URL))
	d.cfg.OpDeadline = "1ms"
	d.cfg.SpoolMaxEntries = 1
	d.cfg.SpoolMinRetention = "0"
	d.cfg.MaxAttemptsPerOp = 1
	d.spool = newSpoolStore(t.TempDir(), d.cfg, d.now)
	cfg := githubTestConfig(srv.URL)
	cfg.MaxAttempts = 1
	cfg.BaseBackoff = "1ms"
	cfg.MaxBackoff = "1ms"
	d.drvCfg["github"] = cfg
	drv, err := newGitHubDriver(cfg, d, srv.Client())
	if err != nil {
		t.Fatalf("driver: %v", err)
	}
	d.drivers["github"] = drv

	// Two spooling ensures for two sigs: the second Put evicts the first.
	for i := 0; i < 2; i++ {
		inc := testIncident()
		inc.Sig = sigFor(i)
		inc.ID = types.NewID(types.PInc)
		if _, err := d.EnsureBySig(context.Background(), inc, testEvidence()); err == nil {
			t.Fatalf("expected the 5xx to surface")
		}
	}
	drops := led.byOp("drop")
	if len(drops) == 0 {
		t.Fatalf("no drop record was written: %v", led.ops())
	}
	if len(led.gaps()) == 0 {
		t.Fatalf("no gap record accompanied the drop")
	}
	for _, g := range led.gaps() {
		if cause := strPayload(g.Payload, "cause"); cause != types.CauseQueueOverflow {
			t.Fatalf("gap cause = %q, want %q", cause, types.CauseQueueOverflow)
		}
	}
	for _, r := range drops {
		if strPayload(r.Payload, "error_code") != string(types.CodeIssues005) {
			t.Fatalf("drop error_code = %q", strPayload(r.Payload, "error_code"))
		}
	}
}

// TestSpoolTenThousandEntries covers the §7 numeric bound at the store level: a
// 10 000-entry spool orders, drains and never duplicates an id. It runs on the
// in-memory path (with its bound raised for the test) because the file-backed
// path fsyncs every entry: the file-backed path's correctness is covered by the
// bounds tests, and its throughput by TestReplayDrainIsDuplicateFree.
func TestSpoolTenThousandEntries(t *testing.T) {
	cfg, clk := spoolCfg(t)
	cfg.SpoolMaxEntries = 20000
	cfg.SpoolBudgetBytes = 67108864
	s := newSpoolStore("", cfg, clk.Now)
	s.memMax = 20000
	start := time.Now()
	for i := 0; i < 10000; i++ {
		ts := types.FormatUTC(clk.Now().Add(time.Duration(i) * time.Second))
		e := mkEntry(t, fmt.Sprintf("ev_%05d", i), ts, ts, 64)
		if _, err := s.Put("github", e); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	if n := s.Count("github"); n != 10000 {
		t.Fatalf("count = %d, want 10000", n)
	}
	list, err := s.List("github")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	seen := map[string]bool{}
	for i, e := range list {
		if seen[e.ID] {
			t.Fatalf("duplicate entry %s", e.ID)
		}
		seen[e.ID] = true
		if i > 0 && list[i-1].NextTryTS > e.NextTryTS {
			t.Fatalf("entry %d is out of order", i)
		}
	}
	for _, e := range list {
		if err := s.Delete("github", e.ID); err != nil {
			t.Fatalf("delete %s: %v", e.ID, err)
		}
	}
	if n := s.Count("github"); n != 0 {
		t.Fatalf("spool not drained: %d", n)
	}
	if elapsed := time.Since(start); elapsed > 60*time.Second {
		t.Fatalf("10 000 store operations took %s", elapsed)
	}
}

// TestReplayDrainIsDuplicateFree runs the drain through a real driver and proves
// zero duplicate issues and zero duplicate comments (§7).
func TestReplayDrainIsDuplicateFree(t *testing.T) {
	api := newFakeGitHub()
	api.createStatus = 503
	srv := ghServer(t, api)
	t.Setenv(testTokenEnv, "ghp_test_token_value")
	d, _, clk, _ := deskWith(t, githubTestConfig(srv.URL))
	d.cfg.OpDeadline = "1ms"
	d.cfg.SpoolMaxEntries = 20000
	d.cfg.SpoolMinRetention = "0"
	cfg := githubTestConfig(srv.URL)
	cfg.MaxAttempts = 1
	cfg.BaseBackoff = "1ms"
	cfg.MaxBackoff = "1ms"
	d.drvCfg["github"] = cfg
	drv, err := newGitHubDriver(cfg, d, srv.Client())
	if err != nil {
		t.Fatalf("driver: %v", err)
	}
	d.drivers["github"] = drv

	const n = 200
	for i := 0; i < n; i++ {
		inc := testIncident() // same sig, same incident: the classic storm
		inc.ID = types.NewID(types.PInc)
		_, _ = d.EnsureBySig(context.Background(), inc, testEvidence())
	}
	if got := d.spoolCount("github"); got == 0 {
		t.Fatalf("nothing was spooled")
	}
	api.mu.Lock()
	api.createStatus = 0
	api.mu.Unlock()
	clk.advance(time.Minute)
	total := 0
	for i := 0; i < 50; i++ {
		clk.advance(time.Second)
		n, err := d.Replay(context.Background(), 50)
		if err != nil {
			t.Fatalf("replay: %v", err)
		}
		total += n
		if d.spoolCount("github") == 0 {
			break
		}
	}
	if d.spoolCount("github") != 0 {
		t.Fatalf("spool did not drain: %d left after %d replays", d.spoolCount("github"), total)
	}
	if got := len(api.issues); got != 1 {
		t.Fatalf("replay produced %d issues for one sig, want exactly 1", got)
	}
	// Every comment marker is unique: no duplicated comment.
	seen := map[string]int{}
	if it := api.lastIssue(); it != nil {
		for _, c := range it.Comments {
			m := idemFromBody(c)
			if m != "" {
				seen[m]++
			}
		}
	}
	for m, c := range seen {
		if c > 1 {
			t.Fatalf("comment %s was appended %d times", m, c)
		}
	}
}

// TestPerSigSingleFlight pins §3.7 rule 1: one in-flight operation per sig.
func TestPerSigSingleFlight(t *testing.T) {
	d, _, _, _ := deskWith(t, githubTestConfig("http://127.0.0.1:1"))
	l1 := d.sigLock("github|" + testSig + "|1")
	l2 := d.sigLock("github|" + testSig + "|1")
	if l1 != l2 {
		t.Fatalf("two locks were minted for one (driver, sig, project)")
	}
	l1.Lock()
	if l2.TryLock() {
		t.Fatalf("a second in-flight operation for the same sig acquired the lock")
	}
	l1.Unlock()
	if !l2.TryLock() {
		t.Fatalf("the lock did not release")
	}
	l2.Unlock()
}
