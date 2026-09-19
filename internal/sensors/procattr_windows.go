//go:build windows

package sensors

import "syscall"

// processGroupAttr returns the plain SysProcAttr on Windows: setpgid(2) is a
// Unix call and syscall.SysProcAttr has no Setpgid field there. The child is
// still given its own process object by Go, but it is NOT put in a signalable
// process group — this is a KNOWN GAP on Windows, stated rather than hidden
// (SPEC-12 §3.5a).
func processGroupAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}

// killProcessGroup kills the single follower process on Windows. A Windows
// job object would be needed to kill the whole tree; that is not implemented
// here, so a grandchild of the follower can outlive this call (SPEC-12 §3.5a).
// The journald sensor itself only ever runs where journalctl exists, so this
// path is reachable only if a Windows build is pointed at one.
func killProcessGroup(pid int, sig syscall.Signal) error {
	p, err := findProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
