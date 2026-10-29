//go:build unix

package loadfence

import "syscall"

// rlimitMemCeiling reports the tighter of the process's RLIMIT_AS and
// RLIMIT_DATA ceilings, naming the resource it came from.
//
// QA-TROUBLE-23: the QA harness's chaos-resource leg caps the suite with
// `ulimit -v 3145728` — RLIMIT_AS, a 3 GiB VIRTUAL address ceiling applied to
// the shell, so no cgroup file moves and a cgroup-only MemCap saw no ceiling.
// SkipUnderMemCap and CappedN therefore never fired in that leg and the
// budget family ran its full fixtures under cap pressure, reddening on the
// host instead of the product (2026-10-28 battery: TestBudgetDashboardRSS,
// TestFsyncCountIsStructural and TestAmortizedThroughput red at load_avg
// 1.3-4.0 with every fence silent). An rlimit is process-local and
// unconditional, which makes it the one cap source the harness cannot lose
// between wrapper layers.
//
// A virtual-address ceiling is a conservative bound for the fence's "does the
// fixture fit" question: VA holds the live set PLUS the runtime's
// reservations, so a fixture whose live set exceeds the VA ceiling cannot
// run, and a fitting fixture is still bounded on the side that thrashes
// first. Falsifiable through EnvMemOverride, which wins over this reading.
func rlimitMemCeiling() (int64, string, bool) {
	best := int64(-1)
	name := ""
	for _, r := range []struct {
		resource int
		name     string
	}{
		{syscall.RLIMIT_AS, "RLIMIT_AS(ulimit -v)"},
		{syscall.RLIMIT_DATA, "RLIMIT_DATA(ulimit -d)"},
	} {
		var lim syscall.Rlimit
		if err := syscall.Getrlimit(r.resource, &lim); err != nil {
			continue
		}
		// RLIM_INFINITY and the page-rounded unlimited sentinels read as no
		// ceiling, mirroring the cgroup v1 sentinel handling in MemCap.
		if lim.Cur > 1<<62 {
			continue
		}
		if best < 0 || int64(lim.Cur) < best {
			best, name = int64(lim.Cur), r.name
		}
	}
	if best <= 0 {
		return 0, "", false
	}
	return best, "rlimit:" + name, true
}
