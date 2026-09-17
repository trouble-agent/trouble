package app

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strconv"
	"strings"
)

// proc.go reads the daemon's own resource facts for the health watermarks
// (SPEC-10 §3.12). All three come from the kernel, never from a counter this
// process keeps: a self-reported RSS is a number that cannot be wrong in an
// interesting way, which is exactly how a memory leak goes unnoticed.

// processMemory returns RSS bytes, peak RSS bytes and the binary's size in bytes.
func processMemory() (rss, peak, binary int64) {
	rss = readStatmRSS()
	peak = readStatusKB("VmHWM")
	if exe, err := os.Executable(); err == nil {
		if fi, err := os.Stat(exe); err == nil {
			binary = fi.Size()
		}
	}
	return rss, peak, binary
}

// stableHostID derives the host identity from /etc/machine-id when it exists
// (systemd's stable machine identity), hashed so the raw id is never written into
// a ledger record. Without it, the hostname is the last resort and it is marked
// as such by using its own bytes rather than pretending to be a derived id.
func stableHostID() string {
	if b, err := os.ReadFile("/etc/machine-id"); err == nil {
		s := strings.TrimSpace(string(b))
		if s != "" {
			sum := sha256.Sum256([]byte(s))
			return hex.EncodeToString(sum[:8])
		}
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		sum := sha256.Sum256([]byte("hostname:" + h))
		return hex.EncodeToString(sum[:8])
	}
	return "0000000000000000"
}

// readStatmRSS reads /proc/self/statm (resident pages × page size). A failure
// returns 0, which the health assembly reports as an unknown watermark rather
// than a healthy zero.
func readStatmRSS() int64 {
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(b))
	if len(fields) < 2 {
		return 0
	}
	pages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return pages * int64(os.Getpagesize())
}

// readStatusKB reads a "Vm*: <n> kB" line out of /proc/self/status.
func readStatusKB(key string) int64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, key+":") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, key+":"))
		if len(fields) == 0 {
			return 0
		}
		n, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return 0
		}
		return n * 1024
	}
	return 0
}
