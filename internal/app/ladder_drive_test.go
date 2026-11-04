package app

// ladder_drive_test.go — the composition root's rung driver (SPEC-05 §4.4/§4.4a,
// TRBL-123).
//
// The ladder package proves the driver itself. These tests prove the WIRING: the
// boot leg calls ResumePending exactly as §4.4's boot order requires, and the
// cadence key resolves as a real config key (a documented key that nothing reads
// is the inert-feature class this board keeps filing).

import (
	"testing"

	"github.com/trouble-agent/trouble/internal/ladder"
)

// TestBootLegCallsResumePending pins §4.4's boot leg: after the index is rebuilt
// and the gates are loaded, the composition root resumes the pending transitions
// once. The seam counter is the only way to see the call on a boot that has no
// pending transition to resume — the boot itself must not fabricate one.
func TestBootLegCallsResumePending(t *testing.T) {
	h := bootDaemon(t)
	if h.d.Ladder == nil {
		t.Fatal("the boot built no ladder")
	}
	if got := h.d.Ladder.DriveStats().ResumeCalls; got < 1 {
		t.Fatalf("the boot called ResumePending %d time(s), want >= 1 (SPEC-05 §4.4 boot order: rebuild index -> gates -> ReAdopt -> resume pending)", got)
	}
}

// TestLadderDriveIntervalResolvesAndIsRead pins the cadence key end to end: it
// resolves through the lifecycle registry (so `--ladder-drive_interval`,
// TROUBLE_LADDER_DRIVE_INTERVAL and the config file all reach it), the default is
// the documented 30s, and the daemon hands the resolved value to the ladder
// rather than leaving it undefaulted.
func TestLadderDriveIntervalResolvesAndIsRead(t *testing.T) {
	h := bootDaemon(t)
	if got, want := h.d.Cfg.Ladder.DriveInterval, "30s"; string(got) != want {
		t.Fatalf("resolved ladder.drive_interval = %q, want %q", string(got), want)
	}
	// The ladder's own default must agree with the resolved one: the loop falls
	// back to DefaultConfig when a caller leaves the key unset.
	if got := ladder.DefaultConfig().DriveInterval; got != "30s" {
		t.Fatalf("ladder.DefaultConfig().DriveInterval = %q, want 30s", got)
	}
	// FromValues folds the resolved row, so the value the daemon builds the
	// ladder from is the value the registry resolved.
	cfg, cerr := ladder.FromValues(h.d.Resolved.Values)
	if cerr != nil {
		t.Fatalf("ladder.FromValues: %v", cerr)
	}
	if cfg.DriveInterval != h.d.Cfg.Ladder.DriveInterval {
		t.Fatalf("ladder.FromValues drive_interval = %q, want the resolved %q", cfg.DriveInterval, h.d.Cfg.Ladder.DriveInterval)
	}
}
