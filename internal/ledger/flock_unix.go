//go:build !windows

package ledger

import (
	"os"
	"syscall"
)

// flockAcquire takes flock(LOCK_EX|LOCK_NB) on f, the single-writer guard for
// ledger/LOCK (SPEC-01 §3.4 rule 6). Linux and darwin share this
// implementation; Windows uses LockFileEx (flock_windows.go, SPEC-12 §3.5a).
func flockAcquire(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func flockRelease(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
