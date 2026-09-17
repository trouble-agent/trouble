package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// HeartbeatLoop writes heartbeat.json atomically every cfg.Lifecycle.HeartbeatInterval
// and emits an idle_tick lifecycle record when the ledger has been quiet
// (SPEC-12 §3.3). It never blocks on the ledger writer.
func HeartbeatLoop(ctx context.Context, cfg Config, w RecordWriter, sensors func() map[string]string) error {
	path := cfg.Lifecycle.HeartbeatPath
	interval := cfg.Lifecycle.HeartbeatInterval.Std()
	idleInterval := cfg.Lifecycle.IdleHeartbeatInterval.Std()
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if idleInterval <= 0 {
		idleInterval = 60 * time.Second
	}

	v, sha, _, _ := VersionInfo()
	lastSeq := uint64(0)
	lastSeqTS := ""
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	write := func(stage string) error {
		hb := types.Heartbeat{
			TS:            types.NowUTC(),
			PID:           os.Getpid(),
			Version:       v,
			GitSHA:        sha,
			LedgerLastSeq: lastSeq,
			LedgerLastTS:  lastSeqTS,
			Stage:         stage,
			Sensors:       sensors(),
		}
		return writeHeartbeat(path, hb)
	}

	if err := write(""); err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			_ = write("shutdown")
			return ctx.Err()
		case <-ticker.C:
			now := time.Now().UTC()
			if w != nil && lastSeqTS != "" {
				ts, err := time.Parse(time.RFC3339Nano, lastSeqTS)
				if err == nil && now.Sub(ts) >= idleInterval {
					_, _ = w.Append(ctx, types.RecordDraft{
						Kind:    types.KLifecycle,
						Actor:   Actor(types.ActorDaemon, "troubled"),
						Payload: map[string]any{"stage": "idle_tick"},
					})
				}
			}
			if err := write(""); err != nil {
				return err
			}
		}
	}
}

// writeHeartbeat writes tmp → fsync → rename (SPEC-12 §3.3).
func writeHeartbeat(path string, hb types.Heartbeat) error {
	b, err := json.Marshal(hb)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp := filepath.Join(dir, fmt.Sprintf(".heartbeat-%d.tmp", os.Getpid()))
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadHeartbeat reads the current heartbeat file.
func ReadHeartbeat(path string) (types.Heartbeat, error) {
	var hb types.Heartbeat
	b, err := os.ReadFile(path)
	if err != nil {
		return hb, err
	}
	if err := json.Unmarshal(b, &hb); err != nil {
		return hb, err
	}
	return hb, nil
}
