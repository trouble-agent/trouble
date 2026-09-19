package sensors

import (
	"sync"
	"sync/atomic"
)

// The inotify state and mask vocabulary are shared so the Sensors struct keeps
// one field set on every GOOS. The inotify(7) mechanism itself is Linux-only
// (inotify_linux.go); darwin and windows report the platform gap from
// inotify_other.go (SPEC-12 §3.5a).

// defaultInotifyMask is the documented mask set.
const defaultInotifyMask = "close_write|moved_to|create|delete|attrib|overflow"

type inotifyState struct {
	mu       sync.Mutex
	fd       int
	watches  map[int32]*inotifyWatch
	byPath   map[string]int32
	dropped  atomic.Uint64
	nameErrs atomic.Uint64
}

type inotifyWatch struct {
	wd        int32
	path      string
	mask      uint32
	recursive bool
	maxDepth  int
	rule      string
	isRules   bool
}
