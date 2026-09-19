//go:build linux

package sensors

// PSI triggers are Linux-only: /proc/pressure/*, epoll(7) and a read+write fd on
// a procfs file. The trigger object and the sampler state stay in psi.go so the
// other platforms keep one Sensors field set; only the syscall surface lives
// here. Non-Linux builds get psi_trigger_other.go (SPEC-12 §3.5a).

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// psiPlatformReason is empty on Linux: the sampler and triggers are available.
func psiPlatformReason() string { return "" }

// arm performs the single legal write and marks the trigger armed (P1, P6).
func (t *psiTrigger) arm() error {
	if err := validateTrigger(t.metric, t.stallUS, t.windowUS); err != nil {
		return err
	}
	n, err := unix.Write(t.fd, triggerBytes(t.metric, t.stallUS, t.windowUS))
	if err != nil {
		return err
	}
	if n != len(triggerBytes(t.metric, t.stallUS, t.windowUS)) {
		return fmt.Errorf("short trigger write: %d bytes", n)
	}
	t.armed.Store(true)
	t.armedAt.Store(time.Now().UnixNano())
	return nil
}

// epollAdd registers the trigger fd. It panics if the fd is not armed: that is
// the 347k-hits/200 ms busy-loop regression (P6), and it must be impossible to
// reach rather than merely unlikely.
func (t *psiTrigger) epollAdd() error {
	if !t.armed.Load() {
		panic(fmt.Sprintf("sensors: psi: refusing to add an unarmed fd to epoll (resource=%s) — an unarmed PSI fd is always ready and spins a core at 100%%", t.resource))
	}
	ev := &unix.EpollEvent{Events: unix.EPOLLPRI | unix.EPOLLERR, Fd: int32(t.fd)}
	if err := unix.EpollCtl(t.epfd, unix.EPOLL_CTL_ADD, t.fd, ev); err != nil {
		return fmt.Errorf("epoll_ctl add fd %d: %w", t.fd, err)
	}
	return nil
}

// pollOnce is the guarded poll path. An unarmed fd returns immediately with no
// events, so the measured "always ready" busy loop cannot be expressed here.
func (t *psiTrigger) pollOnce(timeoutMS int) (int, error) {
	if !t.armed.Load() {
		return 0, nil
	}
	events := make([]unix.EpollEvent, 4)
	n, err := unix.EpollWait(t.epfd, events, timeoutMS)
	if err != nil {
		return 0, err
	}
	got := 0
	for i := 0; i < n; i++ {
		if int(events[i].Fd) == t.shutdownFD {
			return -1, nil
		}
		if int(events[i].Fd) == t.fd {
			if events[i].Events&unix.EPOLLERR != 0 {
				return 0, errPSIERR
			}
			got++
		}
	}
	return got, nil
}

// close de-registers by closing (the kernel destroys the trigger and the fd is
// removed from every epoll set) and never leaves an armed fd behind (P6).
func (t *psiTrigger) close() {
	if t.fd >= 0 {
		_ = unix.Close(t.fd)
		t.fd = -1
	}
	if t.shutdownFD >= 0 {
		_ = unix.Close(t.shutdownFD)
		t.shutdownFD = -1
	}
	if t.epfd >= 0 {
		_ = unix.Close(t.epfd)
		t.epfd = -1
	}
	t.armed.Store(false)
}

// openPSIFd opens one resource for read+write. A read-only open is not enough
// for triggers; /proc/pressure/* are mode 0666 on this kernel.
func openPSIFd(resource string) (int, error) {
	return unix.Open(psiPath(resource), unix.O_RDWR|unix.O_NONBLOCK, 0)
}

func kernelRelease() string {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		b, err := os.ReadFile("/proc/sys/kernel/osrelease")
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	b := u.Release[:]
	if i := indexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

func armOnce(resource, metric string, stallUS, windowUS int64) (errno syscall.Errno, ok bool) {
	fd, err := openPSIFd(resource)
	if err != nil {
		return errnoOf(err), false
	}
	defer unix.Close(fd)
	if _, err := unix.Write(fd, triggerBytes(metric, stallUS, windowUS)); err != nil {
		return errnoOf(err), false
	}
	return 0, true
}

func (s *Sensors) armTrigger(t *psiTrigger) error {
	fd, err := openPSIFd(t.resource)
	if err != nil {
		return err
	}
	t.fd = fd
	if err := t.arm(); err != nil {
		_ = unix.Close(fd)
		t.fd = -1
		return err
	}
	epfd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		t.close()
		return err
	}
	t.epfd = epfd
	sfd, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
	if err != nil {
		t.close()
		return err
	}
	t.shutdownFD = sfd
	// The shutdown eventfd is registered first; the trigger fd only afterwards
	// and only through epollAdd, which refuses an unarmed fd.
	ev := &unix.EpollEvent{Events: unix.EPOLLIN, Fd: int32(sfd)}
	if err := unix.EpollCtl(epfd, unix.EPOLL_CTL_ADD, sfd, ev); err != nil {
		t.close()
		return err
	}
	if err := t.epollAdd(); err != nil {
		t.close()
		return err
	}
	s.psiState.armed.Add(1)
	s.psiState.epollSets.Add(1)
	return nil
}

// runSampler ticks at sample_interval and never increases its cadence under
// pressure (SPEC-03 §3.9 self-observation).

func (s *Sensors) signalShutdown(t *psiTrigger) {
	if t.shutdownFD >= 0 {
		var one = [8]byte{0, 0, 0, 0, 0, 0, 0, 1}
		_, _ = unix.Write(t.shutdownFD, one[:])
	}
}

// handleWake re-reads the counters before any rule evaluation (P4). A wake that
// crosses no boundary produces the same sig as the sample it agrees with; a
// wake whose re-read does not satisfy the rule counts as a spurious wake and
// changes no incident state.
