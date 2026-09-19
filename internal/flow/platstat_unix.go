//go:build !windows

package flow

import (
	"os"
	"syscall"
)

// platFileID returns the (device, inode) identity of a file, the pair the board
// writer uses to notice that the file it is appending to was replaced. Linux and
// darwin both expose it through syscall.Stat_t; Windows has no such pair
// (platstat_windows.go returns the documented not-applicable result).
func platFileID(st os.FileInfo) (uint64, uint64) {
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		return uint64(sys.Dev), uint64(sys.Ino)
	}
	return 0, 0
}
