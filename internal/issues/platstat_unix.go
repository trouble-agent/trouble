//go:build !windows

package issues

import (
	"os"
	"syscall"
)

// platOwnerUID reports the POSIX owner uid of a file, and whether a POSIX uid
// exists at all. Linux and darwin both have one, so the daemon-uid credential
// check (§3.9.1) is enforced there.
func platOwnerUID(st os.FileInfo) (int, bool) {
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		return int(sys.Uid), true
	}
	return 0, false
}
