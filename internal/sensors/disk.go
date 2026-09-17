package sensors

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// SPEC-03 §3.3 (disk): statfs(2) per configured mount, every 60s. Scope is the
// visible mount path, never a device node, and a node with no Files data
// reports inode_free_pct = -1 so an inode rule can never read it as 0% free.

// diskErrorWindow rate-limits TROUBLE-SENSORS-022 to one record per mount per
// hour: a still-listed-but-unmounted path is a configuration state, not a storm.
const diskErrorWindow = time.Hour

type diskState struct {
	mu      sync.Mutex
	lastErr map[string]time.Time
}

// mountInfo is one row of /proc/self/mounts, used for the fstype field
// (statfs does not report it).
type mountInfo struct {
	point  string
	fstype string
}

func readMounts() map[string]string {
	out := map[string]string{}
	f, err := os.Open("/proc/self/mounts")
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		point := strings.ReplaceAll(fields[1], `\040`, " ")
		point = strings.ReplaceAll(point, `\011`, "\t")
		out[point] = fields[2]
	}
	return out
}

// statfsMount samples one mount. Errors are returned, never turned into zeros.
func statfsMount(mount string) (types.SensorEvent, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(mount, &st); err != nil {
		return types.SensorEvent{}, err
	}
	total := float64(st.Blocks)
	free := float64(st.Bavail)
	freePct := 0.0
	if total > 0 {
		freePct = 100 * free / total
	}
	// Files == 0 means the node has no inode data: -1, never 0% free.
	inodePct := -1.0
	if st.Files > 0 {
		inodePct = 100 * float64(st.Ffree) / float64(st.Files)
	}
	readOnly := st.Flags&unix.ST_RDONLY != 0
	return types.SensorEvent{
		Sensor: types.SenDisk,
		Scope:  mount,
		Value:  freePct,
		Unit:   "pct",
		Detail: map[string]any{
			"mount":          mount,
			"fstype":         "",
			"free_pct":       freePct,
			"free_bytes":     float64(st.Bavail) * float64(st.Bsize),
			"total_bytes":    float64(st.Blocks) * float64(st.Bsize),
			"inode_free_pct": inodePct,
			"read_only":      readOnly,
			"count":          1,
			"value":          freePct,
			"severity":       string(types.SevInfo),
			"window_s":       0,
			"age_s":          0,
			"msg":            "",
			"substr":         "",
			"unit":           "pct",
		},
	}, nil
}

func (s *Sensors) startDisk(ctx context.Context) error {
	if !s.cfg.disk.enabled {
		s.setSensor(types.SenDisk, false, false, "disabled by configuration: sensors.disk.enabled=false")
		return nil
	}
	mounts, bad := sanitizeMounts(s.cfg.disk.mounts)
	if len(bad) > 0 {
		s.recError(types.SenDisk, types.CodeSensors025,
			fmt.Sprintf("device nodes are not mount scopes, refused: %s", strings.Join(bad, ", ")))
		s.cfg.disk.mounts = mounts
		if len(mounts) == 0 {
			s.setSensor(types.SenDisk, false, true,
				fmt.Sprintf("%s: every configured mount is a device node (%s)", types.CodeSensors025, strings.Join(bad, ", ")))
			return nil
		}
	}
	if len(s.cfg.disk.mounts) == 0 {
		s.setSensor(types.SenDisk, false, true, string(types.CodeSensors025)+": no mounts configured")
		return nil
	}
	s.dkState.lastErr = map[string]time.Time{}
	if len(bad) > 0 {
		s.setSensor(types.SenDisk, true, true,
			fmt.Sprintf("%s: refused device-node mounts: %s", types.CodeSensors025, strings.Join(bad, ", ")))
	} else {
		s.setSensor(types.SenDisk, true, false, "")
	}
	s.wg.Add(1)
	go s.runDisk(ctx)
	return nil
}

func (s *Sensors) runDisk(ctx context.Context) {
	defer s.wg.Done()
	iv := s.diskInterval()
	tk := time.NewTicker(iv)
	defer tk.Stop()
	s.diskSweep(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-tk.C:
			if next := s.diskInterval(); next != iv {
				iv = next
				tk.Reset(iv)
			}
			s.diskSweep(ctx)
		}
	}
}

// diskInterval implements the §3.9 load-shed rule: under pipeline pressure the
// non-PSI cadences drop to their configured maximum (here: 4× the interval)
// while PSI sampling keeps its interval.
func (s *Sensors) diskInterval() time.Duration {
	iv := s.cfg.disk.interval
	if iv <= 0 {
		iv = 60 * time.Second
	}
	if s.pipelineBackedUp() {
		iv = 4 * iv
	}
	return iv
}

func (s *Sensors) stopDisk() {}

func (s *Sensors) diskSweep(ctx context.Context) {
	mounts := readMounts()
	for _, m := range s.cfg.disk.mounts {
		ev, err := statfsMount(m)
		if err != nil {
			s.diskError(ctx, m, err)
			continue
		}
		if ft, ok := mounts[m]; ok {
			ev.Detail["fstype"] = ft
		}
		ev.ID = types.NewID(types.PEv)
		ev.TS = types.FormatUTC(s.now())
		s.emitDiskKind(ctx, ev, "free_pct")
		if pct, ok := ev.Detail["inode_free_pct"].(float64); ok && pct >= 0 {
			s.emitDiskKind(ctx, ev, "inode_pct")
		}
		if ro, _ := ev.Detail["read_only"].(bool); ro {
			s.emitDiskKind(ctx, ev, "readonly")
		}
		if s.dkState.lastErr != nil {
			if _, bad := s.dkState.lastErr[m]; bad {
				s.closeGap(ctx, types.SenDisk, m, "statfs_failed")
				s.dkState.mu.Lock()
				delete(s.dkState.lastErr, m)
				s.dkState.mu.Unlock()
			}
		}
	}
	if rt := s.rt[types.SenDisk]; rt != nil {
		rt.events.Add(uint64(len(s.cfg.disk.mounts)))
		rt.lastEvent.Store(s.now().UnixNano())
	}
	s.sensorOK(types.SenDisk)
	// Corroboration for the inode-before-space class: both numbers are on the
	// wire, so the shipped inode rule needs no second sweep.
	_ = mounts
}

// emitDiskKind emits one event per (mount, kind) so the sig space keeps
// free_pct and inode_pct as distinct signatures (SPEC-01 §3.3 disk fields).
func (s *Sensors) emitDiskKind(ctx context.Context, base types.SensorEvent, kind string) {
	ev := base
	ev.ID = types.NewID(types.PEv)
	detail := make(map[string]any, len(base.Detail)+2)
	for k, v := range base.Detail {
		detail[k] = v
	}
	detail["kind"] = kind
	if kind == "inode_pct" {
		if pct, ok := detail["inode_free_pct"].(float64); ok {
			ev.Value = pct
			detail["value"] = pct
		}
	}
	ev.Detail = detail
	ev.Sig = sigFor(types.SrcDisk, base.Scope, kind)
	s.handleEvent(ctx, ev)
}

// diskError reports one statfs failure per mount per hour (SPEC-03 §3.3),
// keeping the remaining mounts reporting.
func (s *Sensors) diskError(ctx context.Context, mount string, err error) {
	s.dkState.mu.Lock()
	if s.dkState.lastErr == nil {
		s.dkState.lastErr = map[string]time.Time{}
	}
	last, seen := s.dkState.lastErr[mount]
	if seen && s.now().Sub(last) < diskErrorWindow {
		s.dkState.mu.Unlock()
		return
	}
	s.dkState.lastErr[mount] = s.now()
	s.dkState.mu.Unlock()
	s.recError(types.SenDisk, types.CodeSensors022, fmt.Sprintf("statfs %s: %v", mount, err))
	s.openGap(types.SenDisk, mount, "statfs_failed", err.Error())
	if rt := s.rt[types.SenDisk]; rt != nil {
		rt.degraded.Store(true)
	}
}

// sanitizeMounts enforces the "scope is a visible mount path, never a device
// node" rule (SPEC-03 §3.1) at the configuration boundary: a device node is
// refused loudly instead of becoming a scope that cannot be reasoned about.
func sanitizeMounts(mounts []string) (ok []string, bad []string) {
	for _, m := range mounts {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if strings.HasPrefix(m, "/dev/") || m == "/dev" {
			bad = append(bad, m)
			continue
		}
		ok = append(ok, m)
	}
	return ok, bad
}

// ---- timers (SPEC-03 §3.3) ----

type timerState struct {
	mu     sync.Mutex
	missed map[string]int
	last   map[string]time.Time // last observed next_elapse
	run    map[string]time.Time // last unit state / journald corroboration
	errAt  atomic.Int64
}

func (s *Sensors) startTimers(ctx context.Context) error {
	if !s.cfg.timers.enabled {
		s.setSensor(types.SenTimers, false, false, "disabled by configuration: sensors.timers.enabled=false")
		return nil
	}
	s.tmState.missed = map[string]int{}
	s.tmState.last = map[string]time.Time{}
	s.tmState.run = map[string]time.Time{}
	s.setSensor(types.SenTimers, true, false, "")
	s.wg.Add(1)
	go s.runTimers(ctx)
	return nil
}

func (s *Sensors) runTimers(ctx context.Context) {
	defer s.wg.Done()
	iv := s.cfg.timers.interval
	if iv <= 0 {
		iv = 60 * time.Second
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
			s.timerSweep(ctx)
		}
	}
}

func (s *Sensors) stopTimers() {}

// timerSweep lists timers on every watched manager and derives missed runs.
func (s *Sensors) timerSweep(ctx context.Context) {
	if s.dbState.dead.Load() || s.dbState.merge == nil {
		// One cause, one record: the D-Bus gap is already filed, so timers only
		// report the dependency and produce no second gap (SPEC-03 §3.9).
		s.setSensor(types.SenTimers, true, true, "dependency dbus degraded")
		return
	}
	s.dbState.mu.Lock()
	watches := make(map[string]*dbusWatch, len(s.dbState.watches))
	for k, v := range s.dbState.watches {
		watches[k] = v
	}
	s.dbState.mu.Unlock()
	if len(watches) == 0 {
		s.setSensor(types.SenTimers, true, true, "dependency dbus degraded")
		return
	}
	now := s.now()
	total := 0
	for name, w := range watches {
		timers, err := listTimersFn(w)
		if err != nil {
			// TROUBLE-SENSORS-023: per-tick retry, raw reply size recorded, no
			// invented timer list.
			s.recError(types.SenTimers, types.CodeSensors023,
				fmt.Sprintf("ListTimers on %s: %v", name, err))
			s.tmState.errAt.Store(now.UnixNano())
			continue
		}
		for _, t := range timers {
			total++
			s.emitTimer(ctx, name, t, now)
		}
	}
	if rt := s.rt[types.SenTimers]; rt != nil {
		rt.events.Add(uint64(total))
		rt.lastEvent.Store(now.UnixNano())
	}
	s.sensorOK(types.SenTimers)
	s.clearReason(types.SenTimers)
}

func (s *Sensors) emitTimer(ctx context.Context, manager string, t timerStatus, now time.Time) {
	next := time.Time{}
	if t.NextElapseUSecRealt > 0 {
		next = time.Unix(0, int64(t.NextElapseUSecRealt)*1000).UTC()
	}
	last := time.Time{}
	if t.LastTriggerUSec > 0 {
		last = time.Unix(0, int64(t.LastTriggerUSec)*1000).UTC()
	}
	// The timer's own interval is not in ListTimers; it is estimated from the
	// previous sweep's next_elapse, falling back to the sweep interval. The
	// estimate is recorded so the number is auditable.
	iv := s.timerInterval()
	estimated := false
	s.tmState.mu.Lock()
	if prev, ok := s.tmState.last[t.Name]; ok && !prev.IsZero() && next.After(prev) {
		iv = next.Sub(prev)
		estimated = true
	}
	s.tmState.last[t.Name] = next
	s.tmState.mu.Unlock()

	missedRuns := 0
	if !next.IsZero() && now.After(next) && iv > 0 && now.Sub(next) > iv {
		// Neither a unit state transition nor a journald entry inside the
		// interval means the run was missed.
		corroborated := false
		if lastRun, ok := s.tmState.run[t.Name]; ok && lastRun.After(next.Add(-iv)) {
			corroborated = true
		}
		if !corroborated {
			s.tmState.mu.Lock()
			s.tmState.missed[t.Name]++
			missedRuns = s.tmState.missed[t.Name]
			s.tmState.mu.Unlock()
		}
	} else {
		s.tmState.mu.Lock()
		if s.tmState.missed[t.Name] != 0 {
			// The next observed run resets the counter.
			s.tmState.missed[t.Name] = 0
			s.tmState.run[t.Name] = now
		}
		s.tmState.mu.Unlock()
	}
	nextIn := 0.0
	if !next.IsZero() {
		nextIn = next.Sub(now).Seconds()
	}
	lastAge := -1.0
	if !last.IsZero() {
		lastAge = now.Sub(last).Seconds()
	}
	kind := "inactive"
	switch {
	case missedRuns > 0:
		kind = "missed"
	case next.IsZero():
		kind = "inactive"
	}
	detail := map[string]any{
		"timer_unit":       t.Name,
		"missed_runs":      float64(missedRuns),
		"missed_bucket":    missedBucket(missedRuns),
		"next_elapse_in_s": nextIn,
		"last_run_age_s":   lastAge,
		"interval_source":  map[bool]string{true: "observed", false: "sweep_interval"}[estimated],
		"corroborated":     false, // journald corroboration is optional (§6 edge 16)
		"manager":          manager,
		"kind":             kind,
		"count":            float64(missedRuns),
		"value":            float64(missedRuns),
		"unit":             "count",
		"severity":         string(types.SevMedium),
		"window_s":         iv.Seconds(),
		"age_s":            lastAge,
		"msg":              "",
		"substr":           "",
	}
	ev := types.SensorEvent{
		ID:     types.NewID(types.PEv),
		TS:     types.FormatUTC(now),
		Sensor: types.SenTimers,
		Scope:  t.Name,
		Value:  float64(missedRuns),
		Unit:   "count",
		Detail: detail,
		Sig:    sigFor(types.SrcTimers, t.Name, kind),
	}
	s.handleEvent(ctx, ev)
}

func (s *Sensors) timerInterval() time.Duration {
	iv := s.cfg.timers.interval
	if iv <= 0 {
		iv = 60 * time.Second
	}
	if s.pipelineBackedUp() {
		iv = 4 * iv
	}
	return iv
}

// missedBucket is the SPEC-03 §3.1 bucket vocabulary.
func missedBucket(n int) string {
	switch {
	case n <= 0:
		return "0"
	case n == 1:
		return "1"
	case n <= 4:
		return "2-4"
	case n <= 9:
		return "5-9"
	}
	return "10+"
}

// pipelineBackedUp is the §3.9 self-observation input.
func (s *Sensors) pipelineBackedUp() bool {
	cap := s.depthCap.Load()
	if cap <= 0 {
		return false
	}
	return float64(s.lastDepth.Load())/float64(cap) > ledgerQueueDepthOK
}
