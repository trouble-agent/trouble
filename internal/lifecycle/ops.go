package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// ops.go is the operator-facing surface of SPEC-12 §2.1 that the CLI calls
// directly. The three verbs here are the ones that cannot be expressed as a
// single call on an existing structure:
//
//   - Escalate is the last link of the watchdog chain (§4.3): it runs OUTSIDE
//     the daemon, reads systemd and the journal for the failing unit, and
//     delivers through escalate.channels. It touches no ledger, no state-root
//     write path except escalate.log, and no socket.
//   - RunUpgrade is the upgrade verb (§3.6), with the park step performed by the
//     unit's own drain: the daemon parks in-flight plays as part of STOPPING
//     (§4.2), so the CLI stops the unit, waits for it to become inactive, then
//     renames. The count of parked runs is not observable from outside the
//     process, which is why the result records how the park happened rather than
//     a number it cannot know.
//   - Install is the install verb (§2.1): render, audit, cross-check the
//     escalation wiring, then write (or refuse).
//
// Everything else the CLI needs is a plain call on an existing function.

// UpgradeOptions is one operator upgrade request.
type UpgradeOptions struct {
	To           string         // path to the new binary
	Rollback     bool           // restore backups/bin/<previous> instead
	Wait         types.Duration // READY deadline after the restart
	CurrentBin   string         // defaults to os.Executable()
	RestartUnit  bool           // default true: restart the unit after the rename
	RestartScope string         // "user" | "system" (default: cfg.Lifecycle.SystemdScope)
}

// InstallOptions is one operator install request.
type InstallOptions struct {
	Scope  string // "" | user | system
	Root   string // write the units into this root instead of the real one
	Check  bool   // verify only, write nothing
	DryRun bool   // render and report, do not enable
	Force  bool   // install despite an unstamped build
}

// InstallReport is what `trouble install` prints.
type InstallReport struct {
	UnitDir  string
	Units    []string
	Rows     []types.ConfigValue
	Refused  bool
	Reason   string
	Degraded string // e.g. "cgroup_v1"
}

// Install renders the unit pair, audits the escalation wiring and (unless this is
// a check or a dry run) writes the units (SPEC-12 §2.1, §3.5).
//
// The refusals are the spec's: an unstamped build without --force is
// TROUBLE-LIFECYCLE-006, a missing escalation channel is 016, and a sandbox /
// registry path cross-validation mismatch is 006.
func Install(cfg Config, o InstallOptions) (InstallReport, error) {
	scope := Scope(o.Scope)
	if scope == "" {
		scope = Scope(cfg.Lifecycle.SystemdScope)
	}
	units, err := RenderUnits(cfg)
	if err != nil {
		return InstallReport{Refused: true, Reason: err.Error()}, err
	}
	rep := InstallReport{UnitDir: unitDir(scope, o.Root)}
	for _, u := range units {
		rep.Units = append(rep.Units, u.Name)
	}

	rows, err := AuditUnits(cfg, scope)
	if err != nil {
		return rep, err
	}
	rep.Rows = rows
	for _, row := range rows {
		if row.Key == "escalate.channels" {
			rep.Refused = true
			rep.Reason = "escalate.channels is empty: a watchdog with no alarm channel is an install failure (TROUBLE-LIFECYCLE-016)"
			return rep, fmt.Errorf("%w: %s", types.CodeLifecycle016, rep.Reason)
		}
	}

	v, sha, _, unstamped := VersionInfo()
	if unstamped && !o.Force {
		rep.Refused = true
		rep.Reason = fmt.Sprintf("build is unstamped (%s/%s): pass --force to install anyway (TROUBLE-LIFECYCLE-006)", v, sha)
		return rep, fmt.Errorf("%w: %s", types.CodeLifecycle006, rep.Reason)
	}

	if o.Check {
		return rep, nil
	}
	if err := InstallUnits(cfg, scope, o.Root, o.DryRun); err != nil {
		rep.Refused = true
		rep.Reason = err.Error()
		return rep, err
	}
	if o.DryRun {
		return rep, nil
	}
	if o.Root == "" {
		if err := systemctl(o.ScopeOrDefault(cfg), "daemon-reload"); err != nil {
			rep.Degraded = "daemon-reload: " + err.Error()
		}
		if err := systemctl(o.ScopeOrDefault(cfg), "enable", "--now", cfg.Lifecycle.UnitName); err != nil {
			rep.Refused = true
			rep.Reason = "enable --now: " + err.Error()
			return rep, fmt.Errorf("%w: %s", types.CodeLifecycle006, rep.Reason)
		}
	}
	return rep, nil
}

// ScopeOrDefault reports the configured systemd scope.
func (o InstallOptions) ScopeOrDefault(cfg Config) string {
	if o.Scope != "" {
		return o.Scope
	}
	if cfg.Lifecycle.SystemdScope != "" {
		return cfg.Lifecycle.SystemdScope
	}
	return "user"
}

// RunUpgrade performs the rename-over upgrade (SPEC-12 §3.6): stage and
// self-check, park (by stopping the unit, whose drain parks in-flight plays),
// back up the previous binary, rename, restart and wait for READY, with rollback
// on a missed deadline.
func RunUpgrade(ctx context.Context, cfg Config, o UpgradeOptions) error {
	bin := o.CurrentBin
	if bin == "" {
		var err error
		bin, err = os.Executable()
		if err != nil {
			return fmt.Errorf("%w: cannot locate the running binary: %v", types.CodeLifecycle011, err)
		}
	}
	scope := o.RestartScope
	if scope == "" {
		scope = cfg.Lifecycle.SystemdScope
	}
	unit := cfg.Lifecycle.UnitName
	wait := o.Wait.Std()
	if wait <= 0 {
		wait = cfg.Lifecycle.UpgradeReadyTimeout.Std()
	}

	if o.Rollback {
		prev, err := latestBackup(cfg)
		if err != nil {
			return fmt.Errorf("%w: %v", types.CodeLifecycle011, err)
		}
		o.To = prev
		bin = bin // rename over the live path, exactly as the forward path does
	}

	if o.To == "" {
		return fmt.Errorf("%w: no --to binary or --rollback given", types.CodeLifecycle011)
	}

	plan := upgradePlan{
		CurrentBinary: bin,
		NewBinary:     o.To,
		Park: func(ctx context.Context) (int, error) {
			// SPEC-12 §4.2: the daemon's drain parks in-flight plays. Stopping
			// the unit is therefore the park step, and a unit that will not stop
			// is a park failure (011) — not a silent rename over a live daemon.
			if !o.RestartUnit {
				return 0, nil
			}
			if err := systemctl(scope, "stop", unit); err != nil {
				return 0, err
			}
			return 0, waitInactive(ctx, scope, unit, 60*time.Second)
		},
		Restart: func(ctx context.Context) error {
			if !o.RestartUnit {
				return nil
			}
			return systemctl(scope, "restart", unit)
		},
		Ready: func(ctx context.Context) bool {
			return waitReady(ctx, cfg, wait)
		},
	}
	if err := Upgrade(ctx, cfg, plan); err != nil {
		return err
	}
	return nil
}

// Escalate is `trouble escalate --unit NAME` (SPEC-12 §2.1/§3.5). It is invoked
// by trouble-escalate@.service and by the stall checker: it must work when the
// daemon is dead, so it uses systemd and the journal as its only inputs and the
// configured channels as its only outputs.
func Escalate(ctx context.Context, cfg Config, unit string) error {
	if unit == "" {
		return errors.New("escalate: --unit is required")
	}
	if len(cfg.Escalate.Channels) == 0 {
		return fmt.Errorf("%w: escalate.channels is empty", types.CodeLifecycle016)
	}
	timeout := cfg.Escalate.Timeout.Std()
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	facts := map[string]string{
		"unit":  unit,
		"state": runCapture(ctx, timeout, "systemctl", "show", unit, "-p", "ActiveState,Result,ExecMainStatus"),
		"tail":  runCapture(ctx, timeout, "journalctl", "-u", unit, "-n", "50", "--no-pager"),
	}

	var failed []string
	for i, argv := range cfg.Escalate.Channels {
		if len(argv) == 0 {
			failed = append(failed, fmt.Sprintf("channel %d: empty argv", i))
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
		cmd.Env = childEnv(facts)
		err := cmd.Run()
		cancel()
		if err != nil {
			failed = append(failed, fmt.Sprintf("channel %d (%s): %v", i, argv[0], err))
		}
	}
	appendEscalateLog(cfg, unit, failed, len(cfg.Escalate.Channels))

	if len(failed) == len(cfg.Escalate.Channels) {
		return fmt.Errorf("%w: every escalation channel failed: %s", types.CodeLifecycle010, strings.Join(failed, "; "))
	}
	return nil
}

// WriteShutdownHeartbeat writes the final heartbeat with stage="shutdown" so a
// fresh heartbeat can never be read off a stopped daemon (SPEC-12 §4.2).
func WriteShutdownHeartbeat(cfg Config, w RecordWriter) error {
	path := cfg.Lifecycle.HeartbeatPath
	if path == "" {
		path = filepath.Join(cfg.StateRoot, "heartbeat.json")
	}
	seq, lastTS := uint64(0), ""
	if r, ok := w.(interface {
		Seq() uint64
		LastRecordTS() string
	}); ok {
		seq, lastTS = r.Seq(), r.LastRecordTS()
	}
	v, sha, _, _ := VersionInfo()
	return writeHeartbeat(path, types.Heartbeat{
		TS:            types.NowUTC(),
		PID:           os.Getpid(),
		Version:       v,
		GitSHA:        sha,
		LedgerLastSeq: seq,
		LedgerLastTS:  lastTS,
		Stage:         "shutdown",
		Sensors:       map[string]string{},
	})
}

// ---- helpers ----

// childEnv builds an explicit allowlist environment for every child process
// (SPEC-12 §3.2 control 3): the daemon never re-exports its own secret-bearing
// environment. Only the facts the child needs are added.
func childEnv(facts map[string]string) []string {
	env := []string{"PATH=" + os.Getenv("PATH")}
	for _, k := range []string{"HOME", "LANG", "LC_ALL", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS", "TZ"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	for k, v := range facts {
		env = append(env, "TROUBLE_ESCALATE_"+strings.ToUpper(k)+"="+v)
	}
	return env
}

func runCapture(ctx context.Context, timeout time.Duration, name string, args ...string) string {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	cmd.Env = childEnv(nil)
	out, err := cmd.Output()
	if err != nil {
		return strings.TrimSpace(string(out)) + " [error: " + err.Error() + "]"
	}
	return strings.TrimSpace(string(out))
}

func appendEscalateLog(cfg Config, unit string, failed []string, channels int) {
	path := filepath.Join(cfg.StateRoot, "escalate.log")
	if cfg.StateRoot == "" {
		return
	}
	line := fmt.Sprintf("%s unit=%s channels=%d failed=%d detail=%s\n",
		types.NowUTC(), unit, channels, len(failed), strings.Join(failed, " | "))
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line)
}

func systemctl(scope string, args ...string) error {
	full := args
	if scope == "user" {
		full = append([]string{"--user"}, args...)
	}
	cmd := exec.Command("systemctl", full...)
	cmd.Env = childEnv(nil)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %v: %s", strings.Join(full, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func waitInactive(ctx context.Context, scope, unit string, max time.Duration) error {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		out := runCapture(ctx, 5*time.Second, "systemctl", scopeArgs(scope, "show", unit, "-p", "ActiveState")...)
		if strings.Contains(out, "ActiveState=inactive") || strings.Contains(out, "ActiveState=failed") {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("unit %s did not become inactive within %s", unit, max)
}

func scopeArgs(scope string, args ...string) []string {
	if scope == "user" {
		return append([]string{"--user"}, args...)
	}
	return args
}

// waitReady polls the health surface until it answers, which is the daemon's own
// definition of READY (SPEC-12 §3.6 step 5).
func waitReady(ctx context.Context, cfg Config, max time.Duration) bool {
	url := cfg.HealthURL
	if url == "" {
		bind := cfg.Dashboard.Bind
		if bind == "" {
			bind = "127.0.0.1:7644"
		}
		if !strings.Contains(bind, ":") {
			bind = bind + ":7644"
		}
		url = "http://" + bind + "/health.json"
	}
	deadline := time.Now().Add(max)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err == nil {
			if resp, err := client.Do(req); err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return true
				}
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(500 * time.Millisecond):
		}
	}
	return false
}

func latestBackup(cfg Config) (string, error) {
	dir := filepath.Join(cfg.StateRoot, "backups", "bin")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("no backups in %s: %v", dir, err)
	}
	var newest string
	var newestTS time.Time
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(newestTS) {
			newestTS = info.ModTime()
			newest = filepath.Join(dir, e.Name())
		}
	}
	if newest == "" {
		return "", fmt.Errorf("no usable backup binary in %s", dir)
	}
	return newest, nil
}
