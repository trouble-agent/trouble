//go:build !windows

package sensors

import "syscall"

// processGroupAttr puts the journalctl follower in its own process group so a
// shutdown can signal the whole group (the follower plus anything it spawns)
// instead of orphaning children (SPEC-03 §3.3).
func processGroupAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup signals the process group led by pid.
func killProcessGroup(pid int, sig syscall.Signal) error {
	return syscall.Kill(-pid, sig)
}
