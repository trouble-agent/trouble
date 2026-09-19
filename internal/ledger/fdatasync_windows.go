//go:build windows

package ledger

import (
	"os"

	"golang.org/x/sys/windows"
)

// fdatasync on Windows. fdatasync(2) does not exist there (it is a POSIX
// interface), so the documented equivalent is FlushFileBuffers: it flushes the
// file's buffered data to disk. This is NOT a durability downgrade — FlushFileBuffers
// is at least as strong as fdatasync(2), because it also flushes metadata that
// fdatasync(2) is allowed to skip. What is NOT available is the fdatasync
// optimisation itself: an append-only rotation therefore pays a full metadata
// flush on Windows (SPEC-01 §2.5, SPEC-12 §3.5a).
func fdatasync(f *os.File, useFdatasync bool) error {
	if useFdatasync {
		return windows.FlushFileBuffers(windows.Handle(f.Fd()))
	}
	return f.Sync()
}
