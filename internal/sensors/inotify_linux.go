//go:build linux

package sensors

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// SPEC-03 §3.3 (inotify). The rules directory is ALWAYS watched: it is the
// hot-reload trigger (§3.7). Watch capacity is checked at boot and every 15m
// against /proc/sys/fs/inotify/max_user_watches, and an ENOSPC watch is named
// in the record — never a silent partial watch set.

const inotifyWatchLimitRatio = 0.8

var inotifyMaskBits = map[string]uint32{
	"create":      unix.IN_CREATE,
	"modify":      unix.IN_MODIFY,
	"close_write": unix.IN_CLOSE_WRITE,
	"moved_to":    unix.IN_MOVED_TO,
	"moved_from":  unix.IN_MOVED_FROM,
	"delete":      unix.IN_DELETE,
	"attrib":      unix.IN_ATTRIB,
	"overflow":    unix.IN_Q_OVERFLOW,
}

func (s *Sensors) startInotify(ctx context.Context) error {
	if !s.cfg.inotify.enabled {
		s.setSensor(types.SenInotify, false, false, "disabled by configuration: sensors.inotify.enabled=false")
		return nil
	}
	fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	if err != nil {
		s.recError(types.SenInotify, types.CodeSensors025, "inotify unavailable: "+err.Error())
		s.setSensor(types.SenInotify, false, true, string(types.CodeSensors025)+": inotify unavailable")
		return nil
	}
	s.inoState.fd = fd
	s.inoState.watches = map[int32]*inotifyWatch{}
	s.inoState.byPath = map[string]int32{}
	s.setSensor(types.SenInotify, true, false, "")

	// The rules directory is watched unconditionally.
	rulesMask := uint32(unix.IN_CLOSE_WRITE | unix.IN_MOVED_TO | unix.IN_DELETE)
	s.addWatch(s.cfg.rulesDir, rulesMask, false, 0, "", true)
	for _, p := range s.cfg.inotify.paths {
		mask, ok := parseInotifyMask(p.mask)
		if !ok {
			s.recError(types.SenInotify, types.CodeSensors025,
				fmt.Sprintf("unknown inotify mask %q for %s", p.mask, p.path))
			continue
		}
		depth := p.maxDepth
		if depth <= 0 {
			depth = s.cfg.inotify.maxDepth
		}
		s.addWatch(p.path, mask, p.recursive, depth, p.rule, false)
	}
	s.checkWatchLimit(ctx)

	s.wg.Add(1)
	go s.runInotify(ctx)
	s.wg.Add(1)
	go s.runInotifyRecheck(ctx)
	// Establishing the watch set IS the first successful verification, so
	// liveness has a real starting point instead of "unknown until the first
	// 15-minute recheck".
	s.sensorOK(types.SenInotify)
	return nil
}

func parseInotifyMask(spec string) (uint32, bool) {
	if strings.TrimSpace(spec) == "" {
		spec = defaultInotifyMask
	}
	var mask uint32
	for _, part := range strings.Split(spec, "|") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		bit, ok := inotifyMaskBits[part]
		if !ok {
			return 0, false
		}
		mask |= bit
	}
	return mask, mask != 0
}

// addWatch adds one watch, bounded by max_depth for recursive paths, and names
// an ENOSPC watch in the record (SPEC-03 §3.3).
func (s *Sensors) addWatch(path string, mask uint32, recursive bool, maxDepth int, rule string, isRules bool) {
	info, err := os.Stat(path)
	if err != nil {
		s.recError(types.SenInotify, types.CodeSensors025, fmt.Sprintf("watch path %s: %v", path, err))
		return
	}
	if !info.IsDir() && !recursive {
		s.addOneWatch(path, mask, recursive, maxDepth, rule, isRules)
		return
	}
	if !recursive || maxDepth <= 0 {
		s.addOneWatch(path, mask, recursive, maxDepth, rule, isRules)
		return
	}
	// Recursive: walk bounded by max_depth.
	root := path
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		depth := 0
		if rel != "." {
			depth = len(strings.Split(rel, string(os.PathSeparator)))
		}
		if depth > maxDepth {
			return filepath.SkipDir
		}
		s.addOneWatch(p, mask, recursive, maxDepth, rule, isRules)
		return nil
	})
}

func (s *Sensors) addOneWatch(path string, mask uint32, recursive bool, maxDepth int, rule string, isRules bool) {
	if mask&unix.IN_Q_OVERFLOW == 0 {
		mask |= unix.IN_Q_OVERFLOW
	}
	wd, err := unix.InotifyAddWatch(s.inoState.fd, path, mask)
	if err != nil {
		if err == unix.ENOSPC {
			s.inoState.nameErrs.Add(1)
			s.recError(types.SenInotify, types.CodeSensors020,
				fmt.Sprintf("ENOSPC adding watch %s (would-watch list: %s)", path, s.watchedPaths()))
			return
		}
		s.recError(types.SenInotify, types.CodeSensors025, fmt.Sprintf("add_watch %s: %v", path, err))
		return
	}
	s.inoState.mu.Lock()
	s.inoState.watches[int32(wd)] = &inotifyWatch{
		wd: int32(wd), path: path, mask: mask, recursive: recursive,
		maxDepth: maxDepth, rule: rule, isRules: isRules,
	}
	s.inoState.byPath[path] = int32(wd)
	s.inoState.mu.Unlock()
}

func (s *Sensors) watchedPaths() string {
	s.inoState.mu.Lock()
	defer s.inoState.mu.Unlock()
	out := make([]string, 0, len(s.inoState.watches))
	for _, w := range s.inoState.watches {
		out = append(out, w.path)
	}
	sortStrings(out)
	return strings.Join(out, ",")
}

// checkWatchLimit implements the 0.8 ratio check with both numbers recorded.
func (s *Sensors) checkWatchLimit(ctx context.Context) {
	limit := readProcIntFn("/proc/sys/fs/inotify/max_user_watches")
	if limit <= 0 {
		return
	}
	s.inoState.mu.Lock()
	used := len(s.inoState.watches)
	s.inoState.mu.Unlock()
	if float64(used)/float64(limit) > inotifyWatchLimitRatio {
		s.recError(types.SenInotify, types.CodeSensors020,
			fmt.Sprintf("inotify watches used=%d limit=%d (ratio %.2f > %.2f)", used, limit,
				float64(used)/float64(limit), inotifyWatchLimitRatio))
	}
}

func (s *Sensors) runInotifyRecheck(ctx context.Context) {
	defer s.wg.Done()
	iv := s.cfg.inotify.recheckInterval
	if iv <= 0 {
		iv = 900 * time.Second
	}
	tk := time.NewTicker(iv)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-tk.C:
			s.checkWatchLimit(ctx)
			s.sensorOK(types.SenInotify)
			s.clearReason(types.SenInotify)
		}
	}
}

// runInotify reads the watch fd. IN_Q_OVERFLOW re-adds every watch and records
// one gap with est_lost = -1 (SPEC-03 §3.3, code 021).
func (s *Sensors) runInotify(ctx context.Context) {
	defer s.wg.Done()
	buf := make([]byte, 64<<10)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		default:
		}
		// The fd is read under the lock: stopInotify closes it from another
		// goroutine on the shutdown path.
		s.inoState.mu.Lock()
		fd := s.inoState.fd
		s.inoState.mu.Unlock()
		if fd < 0 {
			return
		}
		n, err := unix.Read(fd, buf)
		if err != nil {
			if err == unix.EAGAIN || err == unix.EINTR {
				select {
				case <-ctx.Done():
					return
				case <-s.stopCh:
					return
				case <-time.After(100 * time.Millisecond):
				}
				continue
			}
			if err == unix.EBADF {
				return // the watch set was closed under us: this is shutdown
			}
			s.recError(types.SenInotify, types.CodeSensors021, fmt.Sprintf("inotify read: %v", err))
			return
		}
		if n <= 0 {
			continue
		}
		s.consumeInotifyEvents(ctx, buf[:n])
	}
}

func (s *Sensors) consumeInotifyEvents(ctx context.Context, buf []byte) {
	off := 0
	for off+unix.SizeofInotifyEvent <= len(buf) {
		wd := int32(int(uint32(buf[off]) | uint32(buf[off+1])<<8 | uint32(buf[off+2])<<16 | uint32(buf[off+3])<<24))
		mask := uint32(buf[off+4]) | uint32(buf[off+5])<<8 | uint32(buf[off+6])<<16 | uint32(buf[off+7])<<24
		nameLen := int(uint32(buf[off+12]) | uint32(buf[off+13])<<8 | uint32(buf[off+14])<<16 | uint32(buf[off+15])<<24)
		nameStart := off + unix.SizeofInotifyEvent
		nameEnd := nameStart + nameLen
		if nameEnd > len(buf) {
			break
		}
		name := strings.TrimRight(string(buf[nameStart:nameEnd]), "\x00")
		off = nameEnd

		s.inoState.mu.Lock()
		w, ok := s.inoState.watches[wd]
		s.inoState.mu.Unlock()
		if !ok {
			continue
		}
		if mask&unix.IN_Q_OVERFLOW != 0 {
			s.inotifyOverflow(ctx)
			continue
		}
		path := w.path
		if name != "" {
			path = filepath.Join(w.path, name)
		}
		class := maskClass(mask)
		if class == "" {
			continue
		}
		if w.isRules && (mask&(unix.IN_CLOSE_WRITE|unix.IN_MOVED_TO|unix.IN_DELETE) != 0) {
			s.markReloadPending()
		}
		s.emitInotify(ctx, path, class, w.rule, mask)
	}
}

func maskClass(mask uint32) string {
	switch {
	case mask&unix.IN_Q_OVERFLOW != 0:
		return "overflow"
	case mask&(unix.IN_CREATE|unix.IN_MOVED_TO) != 0:
		return "create"
	case mask&(unix.IN_MODIFY|unix.IN_CLOSE_WRITE) != 0:
		return "modify"
	case mask&(unix.IN_DELETE|unix.IN_DELETE_SELF|unix.IN_MOVED_FROM) != 0:
		return "delete"
	case mask&unix.IN_ATTRIB != 0:
		return "attrib"
	}
	return ""
}

func (s *Sensors) emitInotify(ctx context.Context, path, class, rule string, mask uint32) {
	detail := map[string]any{
		"path":       path,
		"path_norm":  path,
		"mask_class": class,
		"overflow":   class == "overflow",
		"mask":       int64(mask),
		"rule":       rule,
		"count":      1,
		"value":      1,
		"unit":       "count",
		"severity":   string(types.SevLow),
		"window_s":   0,
		"age_s":      0,
		"msg":        "",
		"substr":     "",
	}
	ev := types.SensorEvent{
		ID:     types.NewID(types.PEv),
		TS:     types.FormatUTC(s.now()),
		Sensor: types.SenInotify,
		Scope:  path,
		Value:  1,
		Unit:   "count",
		Detail: detail,
		Sig:    sigFor(types.SrcInotify, path, class),
	}
	if rt := s.rt[types.SenInotify]; rt != nil {
		rt.events.Add(1)
		rt.lastEvent.Store(s.now().UnixNano())
	}
	s.sensorOK(types.SenInotify)
	s.handleEvent(ctx, ev)
}

// inotifyOverflow re-adds every watch and records one gap (SPEC-03 §3.3).
func (s *Sensors) inotifyOverflow(ctx context.Context) {
	s.inoState.mu.Lock()
	wdList := make([]*inotifyWatch, 0, len(s.inoState.watches))
	for _, w := range s.inoState.watches {
		wdList = append(wdList, w)
	}
	s.inoState.watches = map[int32]*inotifyWatch{}
	s.inoState.byPath = map[string]int32{}
	s.inoState.mu.Unlock()
	for _, w := range wdList {
		s.addOneWatch(w.path, w.mask, w.recursive, w.maxDepth, w.rule, w.isRules)
	}
	qdepth := readProcIntFn("/proc/sys/fs/inotify/max_queued_events")
	s.recError(types.SenInotify, types.CodeSensors021,
		fmt.Sprintf("IN_Q_OVERFLOW: all %d watches re-added (max_queued_events=%d)", len(wdList), qdepth))
	s.emitGapNow(ctx, string(types.SenInotify), "watch-set", "queue_overflow", "-1")
}

func (s *Sensors) stopInotify() {
	s.inoState.mu.Lock()
	fd := s.inoState.fd
	s.inoState.fd = -1
	s.inoState.watches = map[int32]*inotifyWatch{}
	s.inoState.byPath = map[string]int32{}
	s.inoState.mu.Unlock()
	if fd >= 0 {
		_ = unix.Close(fd)
	}
}
