//go:build linux

package sensors

// The Linux disk surface: one statfs(2) per configured mount plus
// /proc/self/mounts for the fstype field. SPEC-03 §3.3.

import (
	"bufio"
	"os"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// diskPlatformReason is empty on Linux: the disk sensor is implemented here.
func diskPlatformReason() string { return "" }

// mountInfo is one row of /proc/self/mounts, used for the fstype field
// (statfs does not report it).
type mountInfo struct {
	point  string
	fstype string
}

func readMounts() map[string]string {
	out := map[string]string{}
	f, err := os.Open("/proc/self/mounts")
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		point := strings.ReplaceAll(fields[1], `\040`, " ")
		point = strings.ReplaceAll(point, `\011`, "\t")
		out[point] = fields[2]
	}
	return out
}

// statfsMount samples one mount. Errors are returned, never turned into zeros.
func statfsMount(mount string) (types.SensorEvent, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(mount, &st); err != nil {
		return types.SensorEvent{}, err
	}
	total := float64(st.Blocks)
	free := float64(st.Bavail)
	freePct := 0.0
	if total > 0 {
		freePct = 100 * free / total
	}
	// Files == 0 means the node has no inode data: -1, never 0% free.
	inodePct := -1.0
	if st.Files > 0 {
		inodePct = 100 * float64(st.Ffree) / float64(st.Files)
	}
	readOnly := st.Flags&unix.ST_RDONLY != 0
	return types.SensorEvent{
		Sensor: types.SenDisk,
		Scope:  mount,
		Value:  freePct,
		Unit:   "pct",
		Detail: map[string]any{
			"mount":          mount,
			"fstype":         "",
			"free_pct":       freePct,
			"free_bytes":     float64(st.Bavail) * float64(st.Bsize),
			"total_bytes":    float64(st.Blocks) * float64(st.Bsize),
			"inode_free_pct": inodePct,
			"read_only":      readOnly,
			"count":          1,
			"value":          freePct,
			"severity":       string(types.SevInfo),
			"window_s":       0,
			"age_s":          0,
			"msg":            "",
			"substr":         "",
			"unit":           "pct",
		},
	}, nil
}
