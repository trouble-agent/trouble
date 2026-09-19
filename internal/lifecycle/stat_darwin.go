//go:build darwin

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

// isRemoteFS on darwin compares the name statfs(2) reports (f_fstypename is a
// string there, not a magic number). The Linux table does not apply: darwin's
// f_type values are a different numbering, so decoding them through the Linux
// magic table would print "0x1a" for an NFS mount and look like a successful
// decode. Comparing the reported name instead keeps the check honest on darwin
// and leaves Linux detection untouched (SPEC-12 §3.5a).
func isRemoteFS(path string, remoteTypes []string) bool {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return false
	}
	name := fstypeNameDarwin(st.Fstypename[:])
	for _, rt := range remoteTypes {
		if name == rt {
			return true
		}
	}
	return false
}

// fstypeNameDarwin decodes the NUL-terminated f_fstypename field. The field is
// [MFSNAMELEN]int8 on darwin, so it is decoded byte by byte.
func fstypeNameDarwin(b []int8) string {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c == 0 {
			break
		}
		out = append(out, byte(c))
	}
	return string(out)
}

func fsTypeName(t uint32) string { return fmt.Sprintf("0x%x", t) }

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
