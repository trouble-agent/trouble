//go:build !unix

package loadfence

// rlimitMemCeiling has no getrlimit surface off unix (SPEC-12 §3.5a ships
// windows/amd64): it reports no ceiling, leaving the cgroup and override
// sources to answer. A reader that cannot measure must never invent a cap.
func rlimitMemCeiling() (int64, string, bool) { return 0, "", false }
