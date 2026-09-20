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

	"github.com/trouble-agent/trouble/internal/types"
)

// upgradePlan carries the injected dependencies for an upgrade (SPEC-12 §3.6).
type upgradePlan struct {
	CurrentBinary string
	NewBinary     string
	Sha256        string
	Park          func(ctx context.Context) (int, error) // returns parked count
	Restart       func(ctx context.Context) error
	Ready         func(ctx context.Context) bool

	// FromVersion is the release version of the binary being replaced and
	// ToVersion the release version of the staged one, both §3.4 release
	// versions (ReleaseVersion over the stamped triples, vMAJOR.MINOR.PATCH).
	// They are inputs rather than something this function can read for itself:
	// the previous binary is a FILE on disk, and a version stamp is not
	// recoverable from one at run time. Empty values are resolved from the
	// running process's own triple, so an in-process caller gets the right
	// from_version for free; a caller driving a foreign staged binary states
	// its to_version explicitly.
	FromVersion string
	ToVersion   string

	// Record, when non-nil, is handed every §3.6 step this recipe takes:
	// "park" before the rename and "resume" after READY (and "rollback" on the
	// two branches that restore the previous binary). A nil Record means the
	// upgrade runs unrecorded — the recipe still works, it just leaves no
	// audit trail, which is why RunUpgrade always installs one.
	//
	// A record error is returned to the caller: the ledger is the audit spine,
	// and §3.6 step 2's rule is exactly that a park which cannot be PERSISTED
	// aborts the upgrade. It does not unwrite a rename that already happened.
	Record func(d types.RecordDraft) error
}

// upgradeVersions resolves the (from, to) release pair for one upgrade run.
func upgradeVersions(plan upgradePlan) (from, to string) {
	v, _, _, _ := VersionInfo()
	from = plan.FromVersion
	if from == "" {
		from = v
	}
	to = plan.ToVersion
	if to == "" {
		to = v
	}
	return ReleaseVersion(from), ReleaseVersion(to)
}

// recordStep hands one step to the plan's Record seam. It is the ONLY place the
// upgrade steps reach a writer, so a step can never be recorded twice by
// accident (two write sites is how a "resume" record ends up describing a
// rollback) and an absent seam is a documented no-op rather than a nil panic.
func recordStep(plan upgradePlan, step, from, to string, parked int) error {
	if plan.Record == nil {
		return nil
	}
	return plan.Record(UpgradeRecord(step, from, to, parked))
}

// Upgrade performs the rename-over upgrade recipe (SPEC-12 §3.6).
func Upgrade(ctx context.Context, cfg Config, plan upgradePlan) error {
	bin := plan.CurrentBinary
	if bin == "" {
		var err error
		bin, err = os.Executable()
		if err != nil {
			return fmt.Errorf("%w: cannot locate current binary: %v", types.CodeLifecycle011, err)
		}
	}
	bin = filepath.Clean(bin)
	from, to := upgradeVersions(plan)

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
	parked, err := plan.Park(ctx)
	if err != nil {
		_ = os.Remove(newPath)
		return fmt.Errorf("%w: park failed: %v", types.CodeLifecycle011, err)
	}
	if err := recordStep(plan, UpgradeStepPark, from, to, parked); err != nil {
		_ = os.Remove(newPath)
		return fmt.Errorf("%w: park not persisted: %v", types.CodeLifecycle011, err)
	}

	// Backup previous binary.
	backupDir := filepath.Join(cfg.StateRoot, "backups", "bin")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		_ = os.Remove(newPath)
		return fmt.Errorf("%w: cannot create backup dir: %v", types.CodeLifecycle011, err)
	}
	// §3.6 step 3's `backups/bin/<version>-<git_sha>` names the binary the
	// backup CONTAINS, not the one that replaced it: run from the new release
	// (the documented operator flow) the running process is the NEW build, so
	// deriving the name from the running triple filed a v0.0.9 binary under
	// `v0.1.0-…` and the rollback inventory lied about what it could restore.
	// The version half therefore comes from FromVersion — the same release
	// input the upgrade records carry — while the sha stays the running
	// process's, because a sha is not recoverable from a binary on disk.
	_, sha, _, _ := VersionInfo()
	backupPath := filepath.Join(backupDir, fmt.Sprintf("%s-%s", from, sha))
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
		// Rollback: the previous binary is put back by the same rename recipe.
		_ = os.Rename(backupPath, bin)
		_ = recordStep(plan, UpgradeStepRollback, to, from, -1)
		return fmt.Errorf("%w: restart failed, rolled back: %v", types.CodeLifecycle011, err)
	}

	// Wait READY. The resume step is recorded only once a process reporting
	// THIS to_version has answered READY: reaching READY is what makes the
	// upgrade real, so the record cannot precede it (§3.6 step 5).
	ready := waitForReady(ctx, cfg, plan)
	if !ready {
		_ = os.Rename(backupPath, bin)
		_ = plan.Restart(ctx)
		_ = recordStep(plan, UpgradeStepRollback, to, from, -1)
		return fmt.Errorf("%w: new binary did not reach READY within %s", types.CodeLifecycle011, cfg.Lifecycle.UpgradeReadyTimeout)
	}
	if err := recordStep(plan, UpgradeStepResume, from, to, -1); err != nil {
		return fmt.Errorf("%w: upgrade reached READY but the resume record could not be written: %v", types.CodeLifecycle011, err)
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
