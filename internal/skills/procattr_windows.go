//go:build windows

package skills

import (
	"os"
	"syscall"
)

// processGroupAttr returns the plain SysProcAttr: syscall.SysProcAttr has no
// Setpgid field on Windows, so no process group is created — a KNOWN GAP,
// stated rather than hidden (SPEC-12 §3.5a).
func processGroupAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{} }

// killProcessGroup kills the single process. Killing the whole tree needs a
// Windows job object, which is not implemented (SPEC-12 §3.5a).
func killProcessGroup(pid int, sig syscall.Signal) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
