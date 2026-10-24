//go:build !linux && !windows

package ledger

import (
	"os"
	"path/filepath"
	"syscall"
)

// checkLockOwner is the portable fallback for platforms without Linux's
// /proc/locks. It uses a non-blocking probe and never trusts a dead payload PID.
func checkLockOwner(root string) (int, bool) {
	f, err := os.OpenFile(filepath.Join(root, "LOCK"), os.O_RDWR, 0o600)
	if err != nil {
		return -1, false
	}
	defer f.Close()
	if err := flockAcquire(f); err == nil {
		_ = flockRelease(f)
		return -1, false
	}
	pid := readLockPID(root)
	if pid <= 0 || syscall.Kill(pid, 0) == syscall.ESRCH {
		return -1, true
	}
	return pid, true
}
