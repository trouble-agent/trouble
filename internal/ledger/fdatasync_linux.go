//go:build linux

package ledger

import (
	"os"
	"syscall"
)

// fdatasync is fdatasync(2) when requested, fsync(2) otherwise. fdatasync is
// sufficient for an append-only file (SPEC-01 §2.5 fdatasync=true).
//
// Linux: fdatasync(2) is in the standard syscall package. darwin exposes no
// binding for it (fdatasync_darwin.go) and Windows has no fdatasync(2) at all
// (fdatasync_windows.go). See SPEC-12 §3.5a.
func fdatasync(f *os.File, useFdatasync bool) error {
	if useFdatasync {
		if err := syscall.Fdatasync(int(f.Fd())); err != nil {
			return err
		}
		return nil
	}
	return f.Sync()
}
