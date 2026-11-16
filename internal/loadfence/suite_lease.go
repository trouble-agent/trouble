package loadfence

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// SuiteLease is a lease for host-measured test gates. On Linux it is an
// advisory inter-process lock, so only tests which use this helper participate;
// that is deliberate because the lease protects a measurement, not production
// work. A process exit releases the Linux kernel lock even if Release is not
// reached.
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
