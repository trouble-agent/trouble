package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// upgradePlan carries the injected dependencies for an upgrade (SPEC-12 §3.6).
type upgradePlan struct {
	NewBinary string
	Sha256    string
	Park      func(ctx context.Context) (int, error) // returns parked count
	Restart   func(ctx context.Context) error
	Ready     func(ctx context.Context) bool
}

// Upgrade performs the rename-over upgrade recipe (SPEC-12 §3.6).
func Upgrade(ctx context.Context, cfg Config, plan upgradePlan) error {
	bin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("%w: cannot locate current binary: %v", types.CodeLifecycle011, err)
	}
	bin = filepath.Clean(bin)

	// Stage.
	newPath := bin + ".new"
	if err := copyFile(plan.NewBinary, newPath); err != nil {
		return fmt.Errorf("%w: cannot stage new binary: %v", types.CodeLifecycle011, err)
	}
	if err := os.Chmod(newPath, 0o755); err != nil {
		_ = os.Remove(newPath)
		return fmt.Errorf("%w: cannot chmod new binary: %v", types.CodeLifecycle011, err)
	}
	if plan.Sha256 != "" {
		if err := verifySHA256(newPath, plan.Sha256); err != nil {
			_ = os.Remove(newPath)
			return fmt.Errorf("%w: staged binary sha256 mismatch: %v", types.CodeLifecycle011, err)
		}
	}
	// Self-check: run new binary --self-check.
	if err := selfCheck(ctx, newPath, cfg); err != nil {
		_ = os.Remove(newPath)
		return fmt.Errorf("%w: staged binary self-check failed: %v", types.CodeLifecycle011, err)
	}

	// Park.
	if _, err := plan.Park(ctx); err != nil {
		_ = os.Remove(newPath)
		return fmt.Errorf("%w: park failed: %v", types.CodeLifecycle011, err)
	}

	// Backup previous binary.
	backupDir := filepath.Join(cfg.StateRoot, "backups", "bin")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		_ = os.Remove(newPath)
		return fmt.Errorf("%w: cannot create backup dir: %v", types.CodeLifecycle011, err)
	}
	v, sha, _, _ := VersionInfo()
	backupPath := filepath.Join(backupDir, fmt.Sprintf("%s-%s", v, sha))
	if err := linkOrCopy(bin, backupPath); err != nil {
		_ = os.Remove(newPath)
		return fmt.Errorf("%w: cannot backup current binary: %v", types.CodeLifecycle011, err)
	}
	pruneBackups(backupDir, cfg.Lifecycle.RollbackDepth)

	// Rename-over (never open live path for write).
	if err := os.Rename(newPath, bin); err != nil {
		_ = os.Remove(newPath)
		return fmt.Errorf("%w: rename-over failed: %v", types.CodeLifecycle011, err)
	}

	// Restart.
	if err := plan.Restart(ctx); err != nil {
		// Rollback.
		_ = os.Rename(backupPath, bin)
		return fmt.Errorf("%w: restart failed, rolled back: %v", types.CodeLifecycle011, err)
	}

	// Wait READY.
	ready := waitForReady(ctx, cfg, plan)
	if !ready {
		_ = os.Rename(backupPath, bin)
		_ = plan.Restart(ctx)
		return fmt.Errorf("%w: new binary did not reach READY within %s", types.CodeLifecycle011, cfg.Lifecycle.UpgradeReadyTimeout)
	}

	return nil
}

func selfCheck(ctx context.Context, bin string, cfg Config) error {
	// Minimal self-check: config parse and unit render.
	if _, err := Resolve(nil, nil, cfg.ConfigPath); err != nil {
		return err
	}
	if _, err := RenderUnits(cfg); err != nil {
		return err
	}
	return nil
}

func waitForReady(ctx context.Context, cfg Config, plan upgradePlan) bool {
	timeout := cfg.Lifecycle.UpgradeReadyTimeout.Std()
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			if plan.Ready(ctx) {
				return true
			}
		}
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	buf := make([]byte, 64*1024)
	for {
		n, err := in.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				out.Close()
				return werr
			}
		}
		if err != nil {
			out.Close()
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func verifySHA256(path, want string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	got := sha256.Sum256(b)
	if hex.EncodeToString(got[:]) != want {
		return fmt.Errorf("sha256 mismatch")
	}
	return nil
}

func linkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	return copyFile(src, dst)
}

func pruneBackups(dir string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	if len(entries) <= keep {
		return
	}
	// Remove oldest by modtime.
	type item struct {
		path string
		info os.FileInfo
	}
	var items []item
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, item{path: filepath.Join(dir, e.Name()), info: info})
	}
	for i := 0; i < len(items)-1; i++ {
		for j := i + 1; j < len(items); j++ {
			if items[j].info.ModTime().Before(items[i].info.ModTime()) {
				items[i], items[j] = items[j], items[i]
			}
		}
	}
	for i := 0; i < len(items)-keep; i++ {
		_ = os.Remove(items[i].path)
	}
}
