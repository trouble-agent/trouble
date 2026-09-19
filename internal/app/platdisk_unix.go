//go:build !windows

package app

import "syscall"

// freeDiskBytesPlatform is statfs(2) on the Unix platforms (the flow §3.8 disk
// gate). Bavail is the unprivileged free space, which is the number the gate
// must refuse on.
func freeDiskBytesPlatform(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
