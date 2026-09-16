//go:build !linux && !darwin

package lifecycle

import "fmt"

// StatT is a no-op owner placeholder on non-unix builds.
type StatT struct {
	Uid uint32
	Gid uint32
}

func isRemoteFS(path string, remoteTypes []string) bool {
	return false
}

func fsTypeName(t int64) string { return fmt.Sprintf("0x%x", t) }

func fileOwner(path string) (int, int, error) {
	return 0, 0, fmt.Errorf("unsupported platform")
}
