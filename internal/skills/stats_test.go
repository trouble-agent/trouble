package skills

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

// TestStatsAreLocalAndFlushed pins §3.6: counters live in stats.json, are flushed
// within the 5 s window, and never appear in an artifact.
func TestStatsAreLocalAndFlushed(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SourcePath = "/nonexistent"
	cfg.MaxHold = "30m"
	s, _, clk := newTestSkills(t, cfg, "proc.connections")
	if got := s.Store.Stats("x-skill", 1); got.Name != "x-skill" || got.Version != 1 || got.Applied != 0 {
		t.Fatalf("empty stats row = %+v", got)
	}
	s.Store.bumpStats("x-skill", 1, true, 0)
	s.Store.bumpStats("x-skill", 1, true, 0)
	s.Store.bumpStats("x-skill", 1, false, 0)
	got := s.Store.Stats("x-skill", 1)
	if got.Applied != 3 || got.Success != 2 || got.LastUsedTS == "" {
		t.Fatalf("stats = %+v", got)
	}
	// The flush is debounced to 5 s and forced on demand.
	clk.advance(6 * 1000000000)
	if err := s.Store.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(s.Store.Dir(), "stats.json"))
	if err != nil {
		t.Fatalf("stats.json: %v", err)
	}
	if len(raw) == 0 {
		t.Fatalf("stats.json is empty")
	}
	// A stats table inside an artifact is the contradiction §3.6 rejects.
	if _, err := LoadArtifact([]byte(goldenArtifactTOML+"\n[stats]\napplied = 3\n"), playBytes()); CodeOf(err) != types.CodeSkills001 {
		t.Fatalf("an artifact with [stats] must be refused: %v", err)
	}
	// The per-day run counter the guards use is local state too.
	if n := s.Store.RunsToday("x-skill", 1, "sentinel:sha256v1:9f2c1d3e4b5a6c7d"); n != 0 {
		t.Fatalf("runs today before any run = %d", n)
	}
	s.Store.noteRun("x-skill", 1, "sentinel:sha256v1:9f2c1d3e4b5a6c7d")
	s.Store.noteRun("x-skill", 1, "sentinel:sha256v1:9f2c1d3e4b5a6c7d")
	if n := s.Store.RunsToday("x-skill", 1, "sentinel:sha256v1:9f2c1d3e4b5a6c7d"); n != 2 {
		t.Fatalf("runs today = %d, want 2", n)
	}
	// A new UTC day resets the counter.
	clk.advance(24 * 60 * 60 * 1000000000)
	if n := s.Store.RunsToday("x-skill", 1, "sentinel:sha256v1:9f2c1d3e4b5a6c7d"); n != 0 {
		t.Fatalf("runs today after a day boundary = %d", n)
	}
}

// TestStatsWriteFailureIs014AndNeverBlocks pins §3.6: a flush failure is
// TROUBLE-SKILLS-014 and the play is unaffected.
func TestStatsWriteFailureIs014AndNeverBlocks(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory mode this test relies on")
	}
	cfg := DefaultConfig()
	cfg.SourcePath = "/nonexistent"
	s, _, _ := newTestSkills(t, cfg, "proc.connections")
	// Make the state dir unwritable: the flush fails, the counters stay in memory.
	if err := os.Chmod(s.Store.Dir(), 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(s.Store.Dir(), 0o700) })
	s.Store.bumpStats("y-skill", 1, true, 0) // must not panic and must not fail a run
	err := s.Store.StatsFlushError()
	if err == nil {
		t.Fatalf("an unwritable state dir must report a flush failure")
	}
	if CodeOf(err) != types.CodeSkills014 {
		t.Fatalf("flush failure code = %s (%v)", CodeOf(err), err)
	}
	if got := s.Store.Stats("y-skill", 1); got.Applied != 1 {
		t.Fatalf("the counters were lost: %+v", got)
	}
}

// TestQuarantineMovesTheTree pins the quarantine mechanics.
func TestQuarantineMovesTheTree(t *testing.T) {
	kp := newKeyPair(t, "skills-2026", types.TrustRelease)
	src, _ := seedChannel(t, kp)
	cfg := pullerCfg(src, kp)
	s, _, _ := newTestSkills(t, cfg, "proc.connections", "service.reload", "config.set")
	if _, err := s.PullOnce(context.Background()); err != nil {
		t.Fatalf("pull: %v", err)
	}
	row, _ := s.Store.row("payment-worker-queue-wedge", 3)
	if err := s.Store.quarantine(row, types.CodeSkills013, ReasonDemoted); err != nil {
		t.Fatalf("quarantine: %v", err)
	}
	row, _ = s.Store.row("payment-worker-queue-wedge", 3)
	if row.State != types.SkillQuarantined {
		t.Fatalf("state = %s", row.State)
	}
	if _, err := os.Stat(filepath.Join(s.Store.Dir(), row.Dir, "SKILL.toml")); err != nil {
		t.Fatalf("the quarantined tree is not where the row says: %v", err)
	}
	if _, err := os.Stat(s.Store.InstallDir("payment-worker-queue-wedge", 3)); err == nil {
		t.Fatalf("the installed tree survived the quarantine")
	}
}
