package loadfence

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

// SuiteLease is an inter-process lease for host-measured test gates. The lock
// is advisory, so only tests which use this helper participate; that is
// deliberate because the lease protects a measurement, not production work.
// A process exit releases the kernel lock even if Release is not reached.
type SuiteLease struct {
	file *os.File
	once sync.Once
	err  error
}

// SuiteLeasePath returns a stable, checkout-specific path outside the working
// tree. Two test processes running against one checkout therefore share a
// lease, while independent checkouts do not serialize one another. The file
// itself is disposable state: flock, rather than its existence, is the lock.
func SuiteLeasePath() (string, error) {
	root, err := moduleRoot()
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256([]byte(root))
	return filepath.Join(os.TempDir(), "trouble-suite-"+hex.EncodeToString(hash[:8])+".lock"), nil
}

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

func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory for suite lease: %w", err)
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("resolve working directory for suite lease: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("find module root from %q: go.mod not found", dir)
		}
		dir = parent
	}
}
