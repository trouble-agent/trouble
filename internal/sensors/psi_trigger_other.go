//go:build !linux

package sensors

import (
	"errors"
	"syscall"
)

// PSI needs /proc/pressure (a read+write procfs fd), epoll(7) for the trigger
// wait, and a kernel PSI interface. None of the three exists off Linux, and no
// substitute (kqueue EVFILT_* / IOCP) is implemented here. startPSI reports the
// sensor UNavailable with the gap named rather than sampling an empty set
// (SPEC-03 §3.3, SPEC-12 §3.5a).
const psiPlatformReasonStr = "platform gap: PSI sampling and triggers read /proc/pressure and wait with epoll(7), both Linux-only (SPEC-12 §3.5a)"

var errPSIPlatform = errors.New("platform gap: PSI triggers need epoll(7) and a read+write /proc/pressure fd, both Linux-only")

func psiPlatformReason() string { return psiPlatformReasonStr }

func (t *psiTrigger) arm() error { return errPSIPlatform }

func (t *psiTrigger) epollAdd() error { return errPSIPlatform }

func (t *psiTrigger) pollOnce(timeoutMS int) (int, error) { return 0, errPSIPlatform }

// close never has an fd to close off Linux: no trigger is ever opened.
func (t *psiTrigger) close() {
	t.fd = -1
	t.shutdownFD = -1
	t.epfd = -1
	t.armed.Store(false)
}

func openPSIFd(resource string) (int, error) { return -1, errPSIPlatform }

// kernelRelease has no non-Linux source: uname(2) is a Unix interface and Go
// exposes no portable substitute. The empty string makes kernelAtLeast report
// "unknown", which is the same input the Linux path treats as "cannot probe".
func kernelRelease() string { return "" }

// armOnce never arms anything off Linux.
func armOnce(resource, metric string, stallUS, windowUS int64) (syscall.Errno, bool) {
	return syscall.Errno(0), false
}

// armTrigger cannot open, arm or register a trigger without epoll(7).
func (s *Sensors) armTrigger(t *psiTrigger) error { return errPSIPlatform }

// signalShutdown has no shutdown eventfd to write: no trigger was ever armed.
func (s *Sensors) signalShutdown(t *psiTrigger) {}
