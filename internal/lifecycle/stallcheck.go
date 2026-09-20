package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// Verdict is the stall check outcome (SPEC-12 §3.3).
type Verdict struct {
	Class       string  `json:"class"`
	Code        string  `json:"code"`
	Exit        int     `json:"exit"`
	SeqAgeS     float64 `json:"seq_age_s"`
	BreachCount int     `json:"breach_count"`
	Reason      string  `json:"reason"`
}

// IsStall reports whether the checker should exit non-zero.
func (v Verdict) IsStall() bool { return v.Exit != 0 }

// StallCheck runs the external stall checker (SPEC-12 §3.3).
func StallCheck(ctx context.Context, cfg Config) (Verdict, error) {
	return stallCheck(ctx, cfg, http.DefaultClient, time.Now())
}

type checkerState struct {
	Seq         uint64 `json:"seq"`
	TS          string `json:"ts"`
	BreachCount int    `json:"breach_count"`
	LastCheckTS string `json:"last_check_ts,omitempty"`
}

func loadCheckerState(path string) (checkerState, error) {
	var st checkerState
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, err
	}
	return st, nil
}

func saveCheckerState(path string, st checkerState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp := filepath.Join(dir, fmt.Sprintf(".checker-%d.tmp", os.Getpid()))
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readToken(envFile string) string {
	b, err := os.ReadFile(envFile)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "TROUBLE_DASHBOARD_TOKEN=") {
			return strings.TrimPrefix(line, "TROUBLE_DASHBOARD_TOKEN=")
		}
	}
	return ""
}

func stallCheck(ctx context.Context, cfg Config, client *http.Client, now time.Time) (Verdict, error) {
	st, err := loadCheckerState(cfg.Checker.StateFile)
	if err != nil {
		return Verdict{Class: "liveness_surface_unreadable", Code: string(types.CodeLifecycle008), Exit: 8, Reason: err.Error()}, nil
	}

	health, ok, err := fetchHealth(ctx, cfg, client)
	class := ""
	if err != nil || !ok {
		// fallback to heartbeat.json
		hb, err2 := ReadHeartbeat(cfg.Lifecycle.HeartbeatPath)
		if err2 != nil {
			return Verdict{Class: "heartbeat_stale", Code: string(types.CodeLifecycle008), Exit: 8, Reason: "health and heartbeat unreadable"}, nil
		}
		health = types.HealthResponse{
			LedgerLastSeq: hb.LedgerLastSeq,
			LedgerLastTS:  hb.LedgerLastTS,
			Version:       hb.Version,
			GitSHA:        hb.GitSHA,
		}
		if hb.TS != "" {
			ts, _ := time.Parse(time.RFC3339Nano, hb.TS)
			if now.Sub(ts) <= cfg.Lifecycle.HeartbeatStaleAfter.Std() {
				class = "liveness_surface_unreadable"
			} else {
				class = "heartbeat_stale"
			}
		} else {
			class = "heartbeat_stale"
		}
	}

	seqAge := 0.0
	if health.LedgerLastTS != "" {
		ts, err := time.Parse(time.RFC3339Nano, health.LedgerLastTS)
		if err == nil {
			seqAge = now.Sub(ts).Seconds()
		}
	}

	maxSeqAge := cfg.Stall.MaxSeqAge.Seconds()
	if maxSeqAge <= 0 {
		maxSeqAge = 300
	}

	if seqAge > maxSeqAge && health.LedgerLastSeq == st.Seq {
		st.BreachCount++
		v := Verdict{
			Class:       "ledger_stall",
			Code:        string(types.CodeLifecycle009),
			Exit:        9,
			SeqAgeS:     seqAge,
			BreachCount: st.BreachCount,
			Reason:      fmt.Sprintf("seq %d unchanged for %.0fs", health.LedgerLastSeq, seqAge),
		}
		st.Seq = health.LedgerLastSeq
		st.TS = health.LedgerLastTS
		st.LastCheckTS = types.NowUTC()
		if err := saveCheckerState(cfg.Checker.StateFile, st); err != nil {
			return v, err
		}
		if err := appendAlarm(cfg.Checker.AlarmFile, v, health); err != nil {
			return v, err
		}
		if st.BreachCount >= cfg.Checker.ConfirmRuns {
			if err := runEscalation(ctx, cfg); err != nil {
				v.Code = string(types.CodeLifecycle010)
				v.Reason = v.Reason + "; escalation failed: " + err.Error()
			}
		}
		return v, nil
	}

	// healthy
	st.Seq = health.LedgerLastSeq
	st.TS = health.LedgerLastTS
	st.BreachCount = 0
	st.LastCheckTS = types.NowUTC()
	var v Verdict
	if class != "" {
		v = Verdict{Class: class, Code: string(types.CodeLifecycle008), Exit: 8, SeqAgeS: seqAge}
	}
	return v, saveCheckerState(cfg.Checker.StateFile, st)
}

func fetchHealth(ctx context.Context, cfg Config, client *http.Client) (types.HealthResponse, bool, error) {
	url := "http://" + cfg.Dashboard.Bind + "/health.json"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return types.HealthResponse{}, false, err
	}
	tok := readToken(cfg.Secrets.EnvironmentFile)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := client.Do(req)
	if err != nil {
		return types.HealthResponse{}, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return types.HealthResponse{}, false, fmt.Errorf("health returned %d", resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return types.HealthResponse{}, false, err
	}
	var h types.HealthResponse
	if err := json.Unmarshal(b, &h); err != nil {
		return types.HealthResponse{}, false, err
	}
	return h, true, nil
}

func appendAlarm(path string, v Verdict, h types.HealthResponse) error {
	line := map[string]any{
		"ts":           types.NowUTC(),
		"class":        v.Class,
		"code":         v.Code,
		"seq":          h.LedgerLastSeq,
		"seq_age_s":    v.SeqAgeS,
		"breach_count": v.BreachCount,
		"version":      h.Version,
		"git_sha":      h.GitSHA,
	}
	b, err := json.Marshal(line)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err1 := f.Close(); err == nil {
		err = err1
	}
	return err
}

func runEscalation(ctx context.Context, cfg Config) error {
	var lastErr error
	failed := 0
	timeout := cfg.Escalate.Timeout.Std()
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	for _, ch := range cfg.Escalate.Channels {
		if len(ch) == 0 {
			continue
		}
		cmd := exec.CommandContext(ctx, ch[0], ch[1:]...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := runWithTimeout(cmd, timeout); err != nil {
			lastErr = err
			failed++
		}
	}
	if len(cfg.Checker.AlarmCommand) > 0 {
		cmd := exec.CommandContext(ctx, cfg.Checker.AlarmCommand[0], cfg.Checker.AlarmCommand[1:]...)
		if err := runWithTimeout(cmd, timeout); err != nil {
			lastErr = err
			failed++
		}
	}
	if failed > 0 && (len(cfg.Escalate.Channels) > 0 || len(cfg.Checker.AlarmCommand) > 0) {
		return fmt.Errorf("%d escalation channels failed: %w", failed, lastErr)
	}
	return nil
}

func runWithTimeout(cmd *exec.Cmd, d time.Duration) error {
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		_ = cmd.Process.Kill()
		return fmt.Errorf("escalation channel timed out after %s", d)
	}
}

// DetectionBound returns the worst-case detection bound in seconds.
func DetectionBound(cfg Config) float64 {
	return cfg.Stall.MaxSeqAge.Seconds() + float64(cfg.Checker.ConfirmRuns)*cfg.Checker.Interval.Seconds()
}
