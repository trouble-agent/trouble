//go:build windows

package app

import "golang.org/x/sys/windows"

// freeDiskBytesPlatform is GetDiskFreeSpaceEx on Windows: statfs(2) does not
// exist there and syscall.Statfs_t has no counterpart. FreeBytesAvailable is
// the number the caller's quota does not restrict, i.e. the closest analogue of
// statfs Bavail, which is the number the flow §3.8 disk gate refuses on. A
// failure returns the error rather than a guessed 0.
func freeDiskBytesPlatform(path string) (int64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var free, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree); err != nil {
		return 0, err
	}
	return int64(free), nil
}
