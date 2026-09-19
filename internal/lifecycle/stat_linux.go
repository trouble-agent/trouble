//go:build linux

// Linux filesystem-type detection: statfs(2) reports the fs type as a magic
// number (st.Type), decoded with the linux/magic.h table below. darwin needs its
// own file (stat_darwin.go) because syscall.Statfs_t.Type is uint32 there and its
// values are NOT the Linux magic numbers (SPEC-12 §3.5a).

package lifecycle

import (
	"fmt"
	"os"
	"syscall"
)

// StatT is the unix stat result surfaced by CheckStateRoot.
type StatT struct {
	Uid uint32
	Gid uint32
}

func isRemoteFS(path string, remoteTypes []string) bool {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return false
	}
	// Type is a runtime-generated int on Linux; compare magic numbers for common remotes.
	name := fsTypeName(st.Type)
	for _, rt := range remoteTypes {
		if name == rt {
			return true
		}
	}
	return false
}

func fsTypeName(t int64) string {
	// Linux magic numbers from linux/magic.h.
	switch t {
	case 0x6969:
		return "nfs" // nfs and nfs4 share this magic on many kernels
	case 0xFF534D42:
		return "cifs"
	case 0x517B:
		return "smb"
	case 0x73717368: // squashfs, not remote
		return ""
	}
	return fmt.Sprintf("0x%x", t)
}

func fileOwner(path string) (int, int, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("not a unix stat")
	}
	return int(st.Uid), int(st.Gid), nil
}
