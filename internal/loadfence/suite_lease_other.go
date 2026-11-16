//go:build !linux

package loadfence

import (
	"errors"
	"fmt"
	"os"
)

// AcquireSuiteLease provides the package API on non-Linux platforms without a
// platform locking primitive. The file is still opened so callers exercise the
// same path and lifetime checks; inter-process serialization is Linux-only.
func AcquireSuiteLease(path string) (*SuiteLease, error) {
	if path == "" {
		return nil, errors.New("suite lease path is empty")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open suite lease %q: %w", path, err)
	}
	return &SuiteLease{file: f}, nil
}

// Release closes the non-Linux lease file. There is no inter-process lock to
// release on platforms without Linux's flock primitive. It is safe to call more
// than once, matching the Linux implementation's lifetime contract.
func (l *SuiteLease) Release() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		if l.file != nil {
			l.err = l.file.Close()
		}
	})
	return l.err
}
