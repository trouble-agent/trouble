//go:build linux

package ledger

import (
	"os"
	"syscall"
)

// Linux: fdatasync(2) is in the standard syscall package.
// fdatasync is fdatasync(2) when requested, fsync(2) otherwise. fdatasync is
// sufficient for an append-only file (SPEC-01 §2.5 fdatasync=true).
//
// Linux and darwin both provide fdatasync(2); only Windows needs the alternate
// path (fdatasync_windows.go, SPEC-12 §3.5a).
// fdatasync is fdatasync(2) when requested, fsync(2) otherwise. fdatasync is
// sufficient for an append-only file (SPEC-01 §2.5 fdatasync=true).
func fdatasync(f *os.File, useFdatasync bool) error {
	if useFdatasync {
		if err := syscall.Fdatasync(int(f.Fd())); err != nil {
			return err
		}
		return nil
	}
	return f.Sync()
}
