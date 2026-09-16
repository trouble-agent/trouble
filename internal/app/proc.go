package app

import (
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
