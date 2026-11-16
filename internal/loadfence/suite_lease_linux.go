//go:build linux

package loadfence

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// AcquireSuiteLease blocks until path's exclusive lease is available.
func AcquireSuiteLease(path string) (*SuiteLease, error) {
	if path == "" {
		return nil, errors.New("suite lease path is empty")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open suite lease %q: %w", path, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("acquire suite lease %q: %w", path, err)
	}
	return &SuiteLease{file: f}, nil
}

// Release releases the lease. It is safe to call more than once.
func (l *SuiteLease) Release() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		if err := unix.Flock(int(l.file.Fd()), unix.LOCK_UN); err != nil {
			l.err = err
		}
		if err := l.file.Close(); l.err == nil {
			l.err = err
		}
	})
	return l.err
}
