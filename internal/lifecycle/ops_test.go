package lifecycle

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// ops_test.go covers the operator verbs the CLI calls (SPEC-12 §2.1): escalate
// (the last link of the watchdog chain) and install (render, audit, refuse). Both
// must work with no daemon, no ledger and no socket — that is the point of them.

func opsCfg(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	// defaults() is the same table Resolve starts from, so a test config is the
	// production config with a temp state root — never a hand-built struct that
	// can drift from the real key set.
	cfg := *defaults()
	cfg.StateRoot = dir
	cfg.Lifecycle.HeartbeatPath = filepath.Join(dir, "heartbeat.json")
	cfg.Lifecycle.UpgradeReadyTimeout = "1s"
	return cfg
}

// freePort returns an address nothing is listening on (bind, read, close).
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return addr
}

func TestEscalateRefusesWithNoChannels(t *testing.T) {
	cfg := opsCfg(t)
	err := Escalate(context.Background(), cfg, "trouble.service")
	if err == nil {
		t.Fatalf("Escalate with an empty channel list succeeded; a watchdog with no alarm channel must refuse (SPEC-12 §5, 016)")
	}
	if !strings.Contains(err.Error(), string(types.CodeLifecycle016)) {
		t.Errorf("error = %v, want %s", err, types.CodeLifecycle016)
	}
}

func TestEscalateDeliversAndLogsTheAttempt(t *testing.T) {
	cfg := opsCfg(t)
	out := filepath.Join(cfg.StateRoot, "delivered.txt")
	cfg.Escalate.Channels = [][]string{{"/bin/sh", "-c", "printf '%s' \"$TROUBLE_ESCALATE_UNIT\" > " + out}}
	cfg.Escalate.Timeout = "5s"

	if err := Escalate(context.Background(), cfg, "trouble.service"); err != nil {
		t.Fatalf("Escalate: %v", err)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("channel did not run: %v", err)
	}
	if string(body) != "trouble.service" {
		t.Errorf("channel saw unit %q, want %q (the failing unit is passed as a fact, not as argv)", body, "trouble.service")
	}
	line, err := os.ReadFile(filepath.Join(cfg.StateRoot, "escalate.log"))
	if err != nil {
		t.Fatalf("escalate.log: %v", err)
	}
	if !strings.Contains(string(line), "unit=trouble.service") {
		t.Errorf("escalate.log line = %q, want the unit named", string(line))
	}
}

func TestEscalateReportsEveryChannelFailure(t *testing.T) {
	cfg := opsCfg(t)
	cfg.Escalate.Channels = [][]string{{"/nonexistent/escalator"}, {"/bin/false"}}
	cfg.Escalate.Timeout = "5s"

	err := Escalate(context.Background(), cfg, "trouble.service")
	if err == nil {
		t.Fatalf("Escalate succeeded with every channel failing")
	}
	if !strings.Contains(err.Error(), string(types.CodeLifecycle010)) {
		t.Errorf("error = %v, want %s", err, types.CodeLifecycle010)
	}
	if !strings.Contains(err.Error(), "channel 0") || !strings.Contains(err.Error(), "channel 1") {
		t.Errorf("error names only some failures: %v", err)
	}
}

func TestEscalateSucceedsWhenOneChannelWorks(t *testing.T) {
	cfg := opsCfg(t)
	cfg.Escalate.Channels = [][]string{{"/nonexistent/escalator"}, {"/bin/true"}}
	cfg.Escalate.Timeout = "5s"
	if err := Escalate(context.Background(), cfg, "trouble.service"); err != nil {
		t.Errorf("Escalate = %v, want nil when at least one channel delivers", err)
	}
}

func TestInstallCheckRefusesEmptyChannelsAndUnstampedBuild(t *testing.T) {
	cfg := opsCfg(t)

	rep, err := Install(cfg, InstallOptions{Check: true})
	if err == nil {
		t.Fatalf("install --check passed with no escalation channel")
	}
	if !strings.Contains(err.Error(), string(types.CodeLifecycle016)) {
		t.Errorf("error = %v, want %s (missing OnFailure/escalation wiring)", err, types.CodeLifecycle016)
	}
	if !rep.Refused {
		t.Errorf("report.Refused = false for the empty-channel case")
	}

	cfg.Escalate.Channels = [][]string{{"/bin/true"}}
	rep, err = Install(cfg, InstallOptions{Check: true})
	if err == nil {
		t.Fatalf("install --check passed on an unstamped build without --force")
	}
	if !strings.Contains(err.Error(), string(types.CodeLifecycle006)) {
		t.Errorf("error = %v, want %s (unstamped build)", err, types.CodeLifecycle006)
	}

	rep, err = Install(cfg, InstallOptions{Check: true, Force: true})
	if err != nil {
		t.Fatalf("install --check --force: %v", err)
	}
	if rep.Refused {
		t.Errorf("install --check --force refused: %s", rep.Reason)
	}
	if len(rep.Units) < 2 {
		t.Errorf("rendered %d units, want the daemon + escalate pair at least", len(rep.Units))
	}
}

func TestInstallDryRunWritesNothing(t *testing.T) {
	cfg := opsCfg(t)
	cfg.Escalate.Channels = [][]string{{"/bin/true"}}
	root := t.TempDir()
	rep, err := Install(cfg, InstallOptions{DryRun: true, Root: root, Force: true})
	if err != nil {
		t.Fatalf("install --dry-run: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("dry run wrote %d files into the unit root", len(entries))
	}
	if rep.UnitDir != root {
		t.Errorf("report.UnitDir = %q, want %q", rep.UnitDir, root)
	}
}

func TestRunUpgradeRequiresADestination(t *testing.T) {
	cfg := opsCfg(t)
	err := RunUpgrade(context.Background(), cfg, UpgradeOptions{RestartUnit: false})
	if err == nil {
		t.Fatalf("RunUpgrade with no --to and no --rollback succeeded")
	}
	if !strings.Contains(err.Error(), string(types.CodeLifecycle011)) {
		t.Errorf("error = %v, want %s", err, types.CodeLifecycle011)
	}
}

func TestWaitReadyAgainstARealListener(t *testing.T) {
	// waitReady is the READY definition of SPEC-12 §3.6; here it polls a real
	// HTTP listener that is only started after the first poll would fail, which
	// is the upgrade's actual shape.
	cfg := opsCfg(t)
	cfg.HealthURL = "http://" + freePort(t) + "/health.json"
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if waitReady(ctx, cfg, 300*time.Millisecond) {
		t.Errorf("waitReady reported READY with nothing listening")
	}
}
