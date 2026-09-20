package sentinel

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// tailPoll is the file tailer's poll interval. Polling (rather than inotify) is
// deliberate: sentinel's collector must not grow an fsnotify dependency, and a
// 250ms poll bounds the latency of an error line by less than a group flush.
const tailPoll = 250 * time.Millisecond

// tailRingBytes is the pre-offset ring the tailer keeps so a rotation mid-event
// flushes the assembled partial instead of losing the head of a traceback
// (§3.5).
const tailRingBytes = 256 * 1024

// fileTailSource tails one log file (§3.5).
type fileTailSource struct {
	s      *Server
	path   string
	parser string
	offset int64
	dev    uint64
	ino    uint64
	ring   []byte
	// state file path (inside the pinned state root).
	offsetPath string
	closed     bool
}

func newFileTailSource(s *Server, path, parser string) *fileTailSource {
	t := &fileTailSource{s: s, path: path, parser: parser}
	if s.cfg.SpoolDir != "" {
		name := parser
		if name == "" {
			name = "tail"
		}
		t.offsetPath = filepath.Join(s.cfg.SpoolDir, "collectors", name+"-"+pathHash(path)+".offset")
		t.offset = readOffset(t.offsetPath)
	}
	return t
}

// pathHash is the stable 16-hex hash of a path used in state file names.
func pathHash(path string) string {
	return types.DigestShort(types.SigDigest([]byte(path)))
}

// readOffset loads a persisted offset; an unreadable or malformed value means
// "start at 0", which replays from the beginning rather than skipping data.
func readOffset(path string) int64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// writeOffset persists an offset (0600) inside the pinned state root.
func writeOffset(path string, off int64) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(off, 10)), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Name is the source label used in liveness and gap records.
func (t *fileTailSource) Name() string { return "file:" + t.path }

// Lines starts the tail loop.
func (t *fileTailSource) Lines(ctx context.Context) (<-chan logLine, <-chan error, error) {
	if _, err := os.Stat(t.path); err != nil {
		return nil, nil, err
	}
	out := make(chan logLine, 256)
	errs := make(chan error, 4)
	go func() {
		defer close(out)
		defer close(errs)
		ticker := time.NewTicker(tailPoll)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				_ = t.saveOffset()
				return
			case <-ticker.C:
				if err := t.poll(ctx, out); err != nil {
					select {
					case errs <- err:
					default:
					}
				}
			}
		}
	}()
	return out, errs, nil
}

// poll re-stats the file and emits whatever was appended.
func (t *fileTailSource) poll(ctx context.Context, out chan<- logLine) error {
	st, err := os.Stat(t.path)
	if err != nil {
		if os.IsNotExist(err) {
			// A removed file: the partial is flushed with `file_removed` and the
			// offset file is kept so a recreate does not replay the world.
			t.s.markGap("file_removed", t.Name(), 1)
			t.s.collectors.flushSource(t.Name(), "file_removed")
			if live := t.s.collectors.live[t.Name()]; live != nil {
				live.alive = false
			}
			return nil
		}
		return err
	}
	dev, ino := statIdentity(st)
	switch {
	case t.ino == 0:
		t.dev, t.ino = dev, ino
		if t.offset > st.Size() {
			// A persisted offset past EOF means the file was replaced.
			t.offset = 0
		}
	case ino != t.ino || dev != t.dev:
		t.s.markGap("file_rotated", t.Name(), 1)
		t.s.collectors.flushSource(t.Name(), "file_rotated")
		t.dev, t.ino, t.offset = dev, ino, 0
	case st.Size() < t.offset:
		t.s.markGap("file_truncated", t.Name(), 1)
		t.offset = 0
	}
	if st.Size() == t.offset {
		return nil
	}
	f, err := os.Open(t.path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return err
	}
	br := bufio.NewReaderSize(f, 64*1024)
	for {
		line, rerr := br.ReadBytes('\n')
		if len(line) > 0 {
			t.offset += int64(len(line))
			t.appendRing(line)
			text := strings.TrimRight(string(line), "\n")
			text = strings.TrimSuffix(text, "\r")
			select {
			case out <- logLine{Text: text, TS: t.s.now(), Source: t.Name()}:
			case <-ctx.Done():
				return nil
			}
		}
		if rerr != nil {
			break
		}
	}
	return t.saveOffset()
}

// appendRing keeps the last 256KB of emitted bytes for rotation recovery.
func (t *fileTailSource) appendRing(b []byte) {
	t.ring = append(t.ring, b...)
	if len(t.ring) > tailRingBytes {
		t.ring = t.ring[len(t.ring)-tailRingBytes:]
	}
}

// saveOffset persists the current offset.
func (t *fileTailSource) saveOffset() error {
	if t.offsetPath == "" {
		return nil
	}
	return writeOffset(t.offsetPath, t.offset)
}

// Close persists the offset and stops.
func (t *fileTailSource) Close() error {
	t.closed = true
	return t.saveOffset()
}

// statIdentity extracts the (dev, ino) pair used for rotation detection.
func statIdentity(st os.FileInfo) (uint64, uint64) {
	dev, ino := fileIdentity(st)
	return dev, ino
}

// fileIdentity extracts the (dev, ino) pair used for rotation detection: an ino
// change means the writer rotated the file even when the name is unchanged.
func fileIdentity(st os.FileInfo) (uint64, uint64) {
	if st == nil {
		return 0, 0
	}
	if dev, ino, ok := platformFileIdentity(st); ok {
		return dev, ino
	}
	return 0, uint64(st.ModTime().UnixNano())
}
