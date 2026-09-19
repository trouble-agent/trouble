//go:build !windows

package sentinel

import (
	"os"
	"syscall"
)

// platformFileIdentity reports the (dev, ino) pair used for rotation
// detection. Linux and darwin both expose it through syscall.Stat_t.
func platformFileIdentity(st os.FileInfo) (uint64, uint64, bool) {
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		return uint64(sys.Dev), uint64(sys.Ino), true
	}
	return 0, 0, false
}
