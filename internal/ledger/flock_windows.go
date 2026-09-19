//go:build windows

package ledger

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// flockAcquire is the Windows single-writer guard for ledger/LOCK: the same
// LOCK_EX|LOCK_NB semantics expressed as LockFileEx with LOCKFILE_EXCLUSIVE_LOCK
// | LOCKFILE_FAIL_IMMEDIATELY, so a second writer fails immediately instead of
// blocking (SPEC-01 §3.4 rule 6, SPEC-12 §3.5a).
//
// Behavioural difference to Linux, stated rather than hidden: flock(2) is an
// advisory lock on the whole file; LockFileEx locks a byte RANGE — this
// implementation locks the first byte, which is the same mutual exclusion for
// the daemon's single LOCK file. flock locks are also released by closing ANY
// fd of the file, while LockFileEx locks are released by closing the locking
// handle; the caller here holds exactly one handle.
func flockAcquire(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, &windows.Overlapped{})
}

func flockRelease(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{})
}

var _ = unsafe.Pointer(nil)
