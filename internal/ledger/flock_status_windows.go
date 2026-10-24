//go:build windows

package ledger

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

const windowsStillActive = 259

// checkLockOwner is the Windows counterpart to the non-Linux lock probe. The
// LockFileEx probe remains authoritative for whether LOCK is held; the process
// query only prevents a stale payload PID from being reported as its owner.
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
	if pid <= 0 || windowsProcessDead(pid) {
		return -1, true
	}
	return pid, true
}

// windowsProcessDead mirrors the Unix probe's narrow dead-process test:
// access-denied and other query failures are treated as live/unknown rather
// than allowing a held lock to be reported as unheld.
func windowsProcessDead(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return errors.Is(err, windows.ERROR_INVALID_PARAMETER)
	}
	defer windows.CloseHandle(h)

	var exitCode uint32
	if err := windows.GetExitCodeProcess(h, &exitCode); err != nil {
		return false
	}
	return exitCode != windowsStillActive
}
