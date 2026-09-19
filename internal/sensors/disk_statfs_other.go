//go:build !linux

package sensors

import (
	"fmt"
	"runtime"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// The disk sensor samples statfs(2) per mount and reads /proc/self/mounts for
// fstype; both are Linux-only, and no non-Linux implementation is shipped.
// startDisk reports the sensor UNavailable with this reason rather than
// sampling through a stub (SPEC-03 §3.3, SPEC-12 §3.5a).
const diskPlatformReasonStr = "platform gap: the disk sensor samples statfs(2) per mount and reads /proc/self/mounts for fstype, both Linux-only (SPEC-12 §3.5a)"

func diskPlatformReason() string { return diskPlatformReasonStr }

// readMounts has no source outside Linux. An empty map is the honest answer and
// is only reachable through the non-Linux stub paths below.
func readMounts() map[string]string { return map[string]string{} }

// statfsMount fails loudly: a caller must never mistake "no implementation" for
// a mount that is 0% free.
func statfsMount(mount string) (types.SensorEvent, error) {
	return types.SensorEvent{}, fmt.Errorf(
		"platform gap: statfs(2) is unavailable on %s; the disk sensor has no non-Linux implementation (SPEC-12 §3.5a)", runtime.GOOS)
}
