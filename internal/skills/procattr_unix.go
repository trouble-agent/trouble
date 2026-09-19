//go:build !windows

package skills

import "syscall"

// processGroupAttr puts a spawned helper in its own process group so the whole
// group can be signalled on shutdown instead of orphaning grandchildren.
func processGroupAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

func killProcessGroup(pid int, sig syscall.Signal) error {
	return syscall.Kill(-pid, sig)
}
