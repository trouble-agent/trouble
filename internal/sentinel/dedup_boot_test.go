package sentinel

import (
	"strings"
	"testing"
)

// TestDedupWindowNegativeRefusedAtBoot pins §3.4a's vocabulary: 0 is the
// documented OFF and a positive duration is the window, so a negative window
// is a boot refusal (TROUBLE-SENTINEL-009), never a silent default — the same
// posture the sensors plane holds for a negative fold window.
func TestDedupWindowNegativeRefusedAtBoot(t *testing.T) {
	cfg := testConfig(t, nil)
	cfg.CanaryProject = ""
	cfg.GenericDedupWindow = "-1m"
	if _, err := NewServer(cfg, &memSink{}, newTestScrubber(t, cfg.Projects)); err == nil {
		t.Fatal("NewServer accepted a negative dedup window")
	} else if !strings.Contains(err.Error(), "dedup_window is negative") {
		t.Fatalf("refusal = %v, want the dedup_window message", err)
	}
}
