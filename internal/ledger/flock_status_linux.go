//go:build linux

package ledger

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// checkLockOwner reads Linux's kernel lock table without acquiring LOCK. The
// device/inode tuple identifies this LOCK inode even when its JSON payload is
// stale or malformed.
func checkLockOwner(root string) (int, bool) {
	path := filepath.Join(root, "LOCK")
	info, err := os.Stat(path)
	if err != nil {
		return -1, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return -1, false
	}
	lockID := fmt.Sprintf("%02x:%02x:%d", unix.Major(uint64(stat.Dev)), unix.Minor(uint64(stat.Dev)), stat.Ino)
	f, err := os.Open("/proc/locks")
	if err != nil {
		return probeLockOwner(root)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 6 || fields[1] != "FLOCK" || fields[5] != lockID {
			continue
		}
		pid, err := strconv.Atoi(fields[4])
		if err != nil || pid <= 0 {
			return -1, true
		}
		return pid, true
	}
	if scanner.Err() != nil {
		return probeLockOwner(root)
	}
	return probeLockOwner(root)
}

// procfs is the normal read-only path. Probe only when procfs cannot give a
// conclusive answer; this closes the race between LOCK stat and /proc/locks.
func probeLockOwner(root string) (int, bool) {
	f, err := os.OpenFile(filepath.Join(root, "LOCK"), os.O_RDWR, 0o600)
	if err != nil {
		return -1, false
	}
	defer f.Close()
	if err := flockAcquire(f); err == nil {
		_ = flockRelease(f)
		return -1, false
	}
	pid, held := readLockPID(root), true
	if pid <= 0 || syscall.Kill(pid, 0) == syscall.ESRCH {
		return -1, held
	}
	return pid, held
}
