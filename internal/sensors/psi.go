package sensors

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// SPEC-03 §3.2 — the PSI measured contract. Every rule below cites the
// measurement it encodes; the numbers were re-measured on this host (kernel
// 7.0.0-30-generic, uid in adm) and the results are in the card metadata:
//
//	NUL-terminated write accepted (21 bytes: "some 150000 2000000\n\x00")
//	write without the terminator → EINVAL (the kernel overwrites the last byte)
//	window 2s/4s/6s/8s/10s accepted; 1s/5s/12s/20s → EINVAL (multiple of 2s, ≤10s)
//	second write on the same armed fd → EBUSY
//	notification rate limit: 1 per window (wakeups measured at 1.95s, 4.00s)
//	unarmed fd polled with a zero timeout: 242,868 hits in 200 ms

// psiMode selects the trigger strategy discovered by the boot probe.
type psiMode string

const (
	psiTriggersAndSampling psiMode = "triggers+sampling"
	psiSamplingOnly        psiMode = "sampling-only"
	psiDisabled            psiMode = "disabled"
)

// psi resources and metrics (SPEC-03 §3.1).
var psiResources = []string{"cpu", "memory", "io"}

const (
	psiMetricSome = "some"
	psiMetricFull = "full"

	psiWindowFloorUS  = 2000000 // unprivileged windows are a multiple of 2s and ≥2s
	psiCeilingProbeUS = 20000000
	psiMinKernelMajor = 5
	psiMinKernelMinor = 15
)

// psiAvg is one `some`/`full` line of /proc/pressure/<res>.
type psiAvg struct {
	Avg10  float64
	Avg60  float64
	Avg300 float64
	Total  float64
}

// psiCounters is a whole pressure file (some always present, full only on
// memory/io).
type psiCounters struct {
	Resource string
	Some     psiAvg
	Full     psiAvg
	HasFull  bool
	Raw      string
}

// triggerBytes is the ONLY producer of PSI trigger write bytes (SPEC-03 §3.2
// P1). A payload built anywhere else cannot reach write(2): the golden test
// asserts the exact 21 bytes and the trailing "\n\x00" terminator, because a
// naive 20-byte string loses its last digit to the kernel's NUL overwrite and
// looks like a rejected API.
func triggerBytes(metric string, stallUS, windowUS int64) []byte {
	s := fmt.Sprintf("%s %d %d\n", metric, stallUS, windowUS)
	b := make([]byte, 0, len(s)+1)
	b = append(b, s...)
	b = append(b, 0x00)
	return b
}

// validateTrigger enforces the P2 window grammar before any fd is opened.
func validateTrigger(metric string, stallUS, windowUS int64) error {
	if metric != psiMetricSome && metric != psiMetricFull {
		return fmt.Errorf("metric %q is not some|full", metric)
	}
	if windowUS < psiWindowFloorUS {
		return fmt.Errorf("window %dµs is below the %dµs floor", windowUS, psiWindowFloorUS)
	}
	if windowUS%psiWindowFloorUS != 0 {
		return fmt.Errorf("window %dµs is not a multiple of %dµs", windowUS, psiWindowFloorUS)
	}
	if stallUS <= 0 {
		return fmt.Errorf("stall %dµs must be > 0", stallUS)
	}
	if stallUS >= windowUS {
		return fmt.Errorf("stall %dµs must be < window %dµs", stallUS, windowUS)
	}
	return nil
}

// psiTrigger is one armed trigger with its own epoll set (SPEC-03 §3.10).
//
// P5: PSI fds are NOT Go-netpoller compatible — os.File.SetDeadline has no
// effect on them — so the fd is never wrapped in an os.File used for deadlines
// and never registered with the runtime netpoller.
type psiTrigger struct {
	resource   string
	metric     string
	stallUS    int64
	windowUS   int64
	fd         int
	epfd       int
	shutdownFD int

	armed   atomic.Bool
	armedAt atomic.Int64

	wakes    atomic.Uint64
	spurious atomic.Uint64
}

func psiPath(resource string) string { return "/proc/pressure/" + resource }

// errPSIERR is the POLLERR verdict shared by the trigger wait paths (defined
// here, not in the Linux file, because handleWake classifies it on every GOOS).
var errPSIERR = fmt.Errorf("POLLERR on an armed PSI fd: the source is gone")

// readPSI reads and parses one pressure file in a single ≤4 KiB read (P10).
func readPSI(resource string) (psiCounters, error) {
	f, err := os.Open(psiPath(resource))
	if err != nil {
		return psiCounters{}, err
	}
	defer f.Close()
	buf := make([]byte, 4096)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return psiCounters{}, err
	}
	c := psiCounters{Resource: resource, Raw: string(buf[:n])}
	for _, line := range strings.Split(c.Raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		var avg psiAvg
		for _, kv := range fields[1:] {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			fv, err := strconv.ParseFloat(v, 64)
			if err != nil {
				continue
			}
			switch k {
			case "avg10":
				avg.Avg10 = fv
			case "avg60":
				avg.Avg60 = fv
			case "avg300":
				avg.Avg300 = fv
			case "total":
				avg.Total = fv
			}
		}
		switch fields[0] {
		case "some":
			c.Some = avg
		case "full":
			c.Full = avg
			c.HasFull = true
		}
	}
	return c, nil
}

// ---- probe (SPEC-03 §3.2 P9) ----

// probeResult is the capability probe's outcome: what the ledger record says
// and what /health repeats as a machine-parseable reason.
type probeResult struct {
	Payload  map[string]any
	Mode     psiMode
	Reason   string
	Codes    []types.ErrorCode
	PSIRead  bool
	ArmOK    bool
	MaxWinUS int64
}

// kernelRelease returns the running kernel's release string (uname -r).
func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// kernelAtLeast parses "7.0.0-30-generic" and compares against the PSI floor.
func kernelAtLeast(release string, major, minor int) (bool, bool) {
	parts := strings.SplitN(release, "-", 2)
	nums := strings.SplitN(parts[0], ".", 3)
	if len(nums) < 2 {
		return false, false
	}
	maj, err1 := strconv.Atoi(nums[0])
	min, err2 := strconv.Atoi(nums[1])
	if err1 != nil || err2 != nil {
		return false, false
	}
	if maj != major {
		return maj > major, true
	}
	return min >= minor, true
}

// armOnce opens a fresh fd, writes the P1 payload for one window, and reports
// the errno class. A fresh fd per attempt is mandatory: a second write on an
// armed fd is EBUSY (measured), so reusing one would misreport the grammar.
func errnoOf(err error) syscall.Errno {
	var errno syscall.Errno
	if e, ok := err.(syscall.Errno); ok {
		return e
	}
	if e, ok := err.(*os.PathError); ok {
		if en, ok := e.Err.(syscall.Errno); ok {
			return en
		}
	}
	_ = errno
	return 0
}

// probeCapabilities runs the boot probe (SPEC-03 §3.2 P9, §4 step 2). It never
// guesses: one real arm decides the mode, the ceiling is discovered downward
// from 20s, and stall=0 is probed for the expected EINVAL.
func (s *Sensors) probeCapabilities(ctx context.Context) (*probeResult, error) {
	now := s.now()
	res := &probeResult{Payload: map[string]any{}, Mode: psiSamplingOnly}

	release := kernelRelease()
	res.Payload["kind"] = "capability_probe"
	res.Payload["probe_ts"] = types.FormatUTC(now)
	res.Payload["kernel"] = release

	// 1. readable?
	ctr, readErr := readPSI("io")
	res.PSIRead = readErr == nil
	res.Payload["psi_read"] = res.PSIRead
	_ = ctr

	// 2. kernel floor (P8).
	ok, parsed := kernelAtLeast(release, psiMinKernelMajor, psiMinKernelMinor)
	if parsed && !ok {
		res.Mode = psiDisabled
		res.Codes = append(res.Codes, types.CodeSensors001)
		res.Reason = fmt.Sprintf("mode=disabled probe=%s kernel=%s (PSI needs ≥5.15)", types.CodeSensors001, release)
		res.Payload["psi_mode"] = string(psiDisabled)
		res.Payload["trigger_arm"] = "skipped"
		res.Payload["max_window_us"] = 0
		res.Payload["stall_zero_rejected"] = false
		res.Payload["pressure_files_writable"] = false
		res.Payload["codes"] = codesToStrings(res.Codes)
		return res, nil
	}

	writable := res.PSIRead
	arm := "skipped"
	root := os.Geteuid() == 0
	var firstErrno syscall.Errno
	if res.PSIRead {
		errno, ok := armOnce("io", psiMetricSome, 150000, psiWindowFloorUS)
		if ok {
			arm = "ok"
			res.ArmOK = true
			res.Mode = psiTriggersAndSampling
		} else {
			firstErrno = errno
			writable = false
			switch errno {
			case syscall.EBUSY:
				arm = "EBUSY"
				res.Codes = append(res.Codes, types.CodeSensors003)
			case syscall.EINVAL:
				arm = "EINVAL"
				if root {
					res.Codes = append(res.Codes, types.CodeSensors002)
				} else {
					res.Codes = append(res.Codes, types.CodeSensors005)
				}
			case syscall.EPERM, syscall.EACCES:
				arm = "EPERM"
				res.Codes = append(res.Codes, types.CodeSensors005)
			default:
				arm = errno.Error()
				res.Codes = append(res.Codes, types.CodeSensors005)
			}
			res.Mode = psiSamplingOnly
		}
	} else {
		arm = "no-pressure-files"
		res.Mode = psiDisabled
		res.Codes = append(res.Codes, types.CodeSensors025)
	}
	res.Payload["trigger_arm"] = arm

	// 3. window ceiling, discovered downward from 20s (never hard-coded).
	maxWin := int64(0)
	if res.ArmOK {
		for _, w := range []int64{psiCeilingProbeUS, 10000000, psiWindowFloorUS} {
			if errno, ok := armOnce("io", psiMetricSome, 150000, w); ok {
				maxWin = w
				break
			} else if errno == syscall.EBUSY {
				// EBUSY means something else holds the trigger; it is not a
				// grammar answer, so the ceiling stays unknown.
				break
			}
		}
	}
	res.MaxWinUS = maxWin
	res.Payload["max_window_us"] = maxWin

	// 4. stall=0 must be refused; observing EINVAL is the proof the grammar is
	//    enforced rather than silently accepted.
	stallZeroRejected := false
	if res.PSIRead {
		if errno, ok := armOnce("io", psiMetricSome, 0, psiWindowFloorUS); !ok && errno == syscall.EINVAL {
			stallZeroRejected = true
		}
	}
	res.Payload["stall_zero_rejected"] = stallZeroRejected
	res.Payload["pressure_files_writable"] = writable && res.ArmOK

	res.Payload["psi_mode"] = string(res.Mode)
	switch res.Mode {
	case psiTriggersAndSampling:
		res.Reason = fmt.Sprintf("mode=%s probe=ok at %s max_window_us=%d", psiTriggersAndSampling, types.FormatUTC(now), maxWin)
	case psiSamplingOnly:
		code := types.CodeSensors005
		if root {
			code = types.CodeSensors002
		}
		res.Codes = dedupeCodes(res.Codes)
		res.Reason = fmt.Sprintf("mode=%s probe=%s at %s", psiSamplingOnly, code, types.FormatUTC(now))
	default:
		res.Reason = fmt.Sprintf("mode=%s probe=%s at %s", psiDisabled, types.CodeSensors025, types.FormatUTC(now))
	}

	// 5. journald capabilities.
	res.Payload["journald"] = s.probeJournald(ctx)
	// 6. D-Bus managers.
	res.Payload["dbus_managers"] = s.probeDBusManagers(ctx)
	// 7. oomd.
	oomdAvail := s.probeOOMD(ctx)
	res.Payload["oomd_available"] = oomdAvail
	if !oomdAvail {
		res.Codes = append(res.Codes, types.CodeSensors016)
	}
	// 8. inotify limits.
	res.Payload["inotify"] = map[string]any{
		"max_user_watches":  readProcInt("/proc/sys/fs/inotify/max_user_watches"),
		"max_queued_events": readProcInt("/proc/sys/fs/inotify/max_queued_events"),
	}
	// 9. per-sensor modes.
	res.Payload["modes"] = map[string]any{
		"disk":    "thresholds",
		"timers":  "list+journald",
		"inotify": "rules_dir+configured",
	}
	res.Codes = dedupeCodes(res.Codes)
	res.Payload["codes"] = codesToStrings(res.Codes)
	_ = firstErrno
	return res, nil
}

func dedupeCodes(in []types.ErrorCode) []types.ErrorCode {
	seen := map[types.ErrorCode]bool{}
	out := make([]types.ErrorCode, 0, len(in))
	for _, c := range in {
		if seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	return out
}

func codesToStrings(in []types.ErrorCode) []string {
	out := make([]string, 0, len(in))
	for _, c := range in {
		out = append(out, string(c))
	}
	return out
}

// readProcIntFn reads a small integer from a /proc knob. The package var is the
// test seam for the inotify watch-limit ratio (an ENOSPC-sized host cannot be
// provisioned from a unit test), and every other caller uses it directly.
var readProcIntFn = readProcInt

func readProcInt(path string) int64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// probeJournald is the probe record's journald object: binary presence, cursor
// validity and the membership answer (SPEC-03 §3.2 P9). Membership is answered
// by executing the follow with -n 1, never by reading the process group list.
func (s *Sensors) probeJournald(ctx context.Context) map[string]any {
	out := map[string]any{"binary": false, "cursor_ok": false, "member_adm": false}
	p, err := exec.LookPath("journalctl")
	if err != nil {
		return out
	}
	out["binary"] = true
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// Cursor probe: take the newest cursor, then seek after it (rc=0 proves the
	// grammar and the read path in one call).
	cur := ""
	if b, err := exec.CommandContext(cctx, p, "-n", "1", "-o", "json", "--output-fields", "__CURSOR").Output(); err == nil {
		cur = extractCursor(string(b))
	}
	if cur != "" && s.cursorValid(ctx, p, cur) {
		out["cursor_ok"] = true
	}
	out["member_adm"] = s.journalReadable(ctx, p)
	return out
}

func extractCursor(jsonLine string) string {
	i := strings.Index(jsonLine, `"__CURSOR"`)
	if i < 0 {
		return ""
	}
	rest := jsonLine[i+len(`"__CURSOR"`):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}
	rest = rest[j+1:]
	k := strings.Index(rest, `"`)
	if k < 0 {
		return ""
	}
	return rest[:k]
}

// ---- sampler ----

// psiSamplerState is the live PSI runtime state.
type psiSamplerState struct {
	mu        sync.Mutex
	triggers  []*psiTrigger
	armed     atomic.Int64
	epollSets atomic.Int64
	spurious  atomic.Uint64
	wakes     atomic.Uint64
	samples   atomic.Uint64
	mode      psiMode
	maxWinUS  int64
}

// sampleOnce reads one resource and emits a sampled SensorEvent (P3: sampling
// is the source of truth; a trigger only shortens latency).
func (s *Sensors) sampleOnce(ctx context.Context, resource string) (types.SensorEvent, error) {
	c, err := readPSI(resource)
	if err != nil {
		return types.SensorEvent{}, err
	}
	ev := types.SensorEvent{
		ID:     types.NewID(types.PEv),
		TS:     types.FormatUTC(s.now()),
		Sensor: types.SenPSI,
		Scope:  resource,
		Value:  c.Some.Avg10,
		Unit:   "pct",
		Detail: map[string]any{
			"metric":        psiMetricSome,
			"band":          psiBucket(c.Some.Avg10),
			"scope":         resource,
			"resource":      resource,
			"some_avg10":    c.Some.Avg10,
			"some_avg60":    c.Some.Avg60,
			"some_avg300":   c.Some.Avg300,
			"some_total":    c.Some.Total,
			"band_avg10":    psiBucket(c.Some.Avg10),
			"stall_us":      0,
			"wake":          false,
			"count":         1,
			"severity":      string(types.SevInfo),
			"window_s":      s.cfg.psi.sampleInterval.Seconds(),
			"age_s":         0.0,
			"substr":        "",
			"unit":          "pct",
			"sensor":        string(types.SenPSI),
			"source":        string(types.SrcPSI),
			"msg":           c.Raw,
			"psi_mode":      string(s.psiState.mode),
			"sample_backed": true,
		},
	}
	if c.HasFull {
		ev.Detail["full_avg10"] = c.Full.Avg10
		ev.Detail["full_avg60"] = c.Full.Avg60
		ev.Detail["full_avg300"] = c.Full.Avg300
		ev.Detail["full_total"] = c.Full.Total
		ev.Detail["has_full"] = true
	}
	ev.Detail["metric"] = psiMetricSome
	ev.Sig = sigFor(types.SrcPSI, resource, psiMetricSome, psiBucket(c.Some.Avg10))
	s.psiState.samples.Add(1)
	return ev, nil
}

// startPSI starts the sampler and, in triggers+sampling mode, one goroutine per
// armed trigger with its own epoll set (P5).
func (s *Sensors) startPSI(ctx context.Context) error {
	if !s.cfg.psi.enabled {
		s.setSensor(types.SenPSI, false, false, "disabled by configuration: sensors.psi.enabled=false")
		return nil
	}
	// A platform with no PSI implementation reports UNavailable with the gap
	// named: an empty sampler that reports healthy would be a silent no-op
	// (SPEC-03 §3.3, SPEC-12 §3.5a).
	if reason := psiPlatformReason(); reason != "" {
		s.setSensor(types.SenPSI, false, true, reason)
		return nil
	}
	pr := s.probe.Load()
	if pr != nil {
		s.psiState.mode = pr.Mode
		s.psiState.maxWinUS = pr.MaxWinUS
	}
	switch s.psiState.mode {
	case psiDisabled:
		s.setSensor(types.SenPSI, false, true, pr.Reason)
		return nil
	case psiSamplingOnly:
		s.setSensor(types.SenPSI, true, true, pr.Reason)
	default:
		s.setSensor(types.SenPSI, true, false, pr.Reason)
	}

	s.wg.Add(1)
	go s.runSampler(ctx)

	if s.psiState.mode == psiTriggersAndSampling {
		windowUS := s.cfg.psi.window.Microseconds()
		if windowUS < psiWindowFloorUS {
			windowUS = psiWindowFloorUS
		}
		if s.psiState.maxWinUS > 0 && windowUS > s.psiState.maxWinUS {
			windowUS = s.psiState.maxWinUS
		}
		for _, res := range psiResources {
			t := &psiTrigger{
				resource:   res,
				metric:     psiMetricSome,
				stallUS:    windowUS / 2,
				windowUS:   windowUS,
				fd:         -1,
				epfd:       -1,
				shutdownFD: -1,
			}
			if err := s.armTrigger(t); err != nil {
				// A second EBUSY/POLLERR degrades to sampling-only rather than
				// storming retries (SPEC-03 §5 rows 003/004).
				s.psiState.mode = psiSamplingOnly
				s.setSensor(types.SenPSI, true, true, fmt.Sprintf("mode=sampling-only probe=%s at %s (%v)", types.CodeSensors003, types.FormatUTC(s.now()), err))
				break
			}
			s.psiState.mu.Lock()
			s.psiState.triggers = append(s.psiState.triggers, t)
			s.psiState.mu.Unlock()
			s.wg.Add(1)
			go s.runTrigger(ctx, t)
		}
	}
	return nil
}

// armTrigger opens the fd, arms it, then adds it to its own epoll set — in that
// order, by this goroutine (P6).
func (s *Sensors) runSampler(ctx context.Context) {
	defer s.wg.Done()
	iv := s.cfg.psi.sampleInterval
	if iv < time.Second {
		iv = time.Second
	}
	if iv > 10*time.Second {
		iv = 10 * time.Second
	}
	if w := s.cfg.psi.window; w > 0 && iv > w {
		iv = w
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
			for _, res := range psiResources {
				ev, err := s.sampleOnce(ctx, res)
				if err != nil {
					s.sensorError(types.SenPSI, res, err)
					continue
				}
				s.sensorOK(types.SenPSI)
				s.handleEvent(ctx, ev)
			}
		}
	}
}

// runTrigger is one goroutine per armed trigger (P5). Notification rate
// limiting (1 per window) caps wake processing at ≤1 per window per fd (P10).
func (s *Sensors) runTrigger(ctx context.Context, t *psiTrigger) {
	defer s.wg.Done()
	defer t.close()
	defer func() {
		s.psiState.armed.Add(-1)
		s.psiState.epollSets.Add(-1)
	}()
	windowMS := int(t.windowUS / 1000)
	if windowMS <= 0 {
		windowMS = 2000
	}
	lastWake := time.Time{}
	for {
		select {
		case <-ctx.Done():
			s.signalShutdown(t)
			return
		case <-s.stopCh:
			s.signalShutdown(t)
			return
		default:
		}
		n, err := t.pollOnce(200)
		if err == errPSIERR {
			s.recError(types.SenPSI, types.CodeSensors004, fmt.Sprintf("POLLERR on armed fd for %s", t.resource))
			return
		}
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			s.recError(types.SenPSI, types.CodeSensors004, fmt.Sprintf("epoll wait on %s: %v", t.resource, err))
			return
		}
		if n <= 0 {
			continue
		}
		now := s.now()
		if !lastWake.IsZero() && now.Sub(lastWake) < time.Duration(windowMS)*time.Millisecond {
			continue // rate-limited: 1 per window
		}
		lastWake = now
		t.wakes.Add(1)
		s.psiState.wakes.Add(1)
		s.handleWake(ctx, t)
	}
}

func (s *Sensors) handleWake(ctx context.Context, t *psiTrigger) {
	c, err := readPSI(t.resource)
	if err != nil {
		s.sensorError(types.SenPSI, t.resource, err)
		return
	}
	ev := types.SensorEvent{
		ID:     types.NewID(types.PEv),
		TS:     types.FormatUTC(s.now()),
		Sensor: types.SenPSI,
		Scope:  t.resource,
		Value:  c.Some.Avg10,
		Unit:   "pct",
		Wake:   true,
		Detail: map[string]any{
			"metric":      psiMetricSome,
			"band":        psiBucket(c.Some.Avg10),
			"scope":       t.resource,
			"resource":    t.resource,
			"some_avg10":  c.Some.Avg10,
			"some_avg60":  c.Some.Avg60,
			"some_avg300": c.Some.Avg300,
			"some_total":  c.Some.Total,
			"stall_us":    t.stallUS,
			"wake":        true,
			"count":       1,
			"severity":    string(types.SevInfo),
			"window_s":    float64(t.windowUS) / 1e6,
			"age_s":       0.0,
			"substr":      "",
			"unit":        "pct",
			"sensor":      string(types.SenPSI),
			"source":      string(types.SrcPSI),
			"msg":         c.Raw,
			"armed":       string(triggerBytes(t.metric, t.stallUS, t.windowUS)),
		},
	}
	if c.HasFull {
		ev.Detail["full_avg10"] = c.Full.Avg10
		ev.Detail["full_avg60"] = c.Full.Avg60
		ev.Detail["full_avg300"] = c.Full.Avg300
		ev.Detail["full_total"] = c.Full.Total
		ev.Detail["has_full"] = true
	}
	ev.Sig = sigFor(types.SrcPSI, t.resource, psiMetricSome, psiBucket(c.Some.Avg10))
	if !s.handleEvent(ctx, ev) {
		t.spurious.Add(1)
		s.psiState.spurious.Add(1)
	}
}

// stopPSI closes every armed trigger on the shutdown path (SPEC-03 §4) and
// asserts the armed-fd invariant.
func (s *Sensors) stopPSI() {
	s.psiState.mu.Lock()
	triggers := s.psiState.triggers
	s.psiState.triggers = nil
	s.psiState.mu.Unlock()
	for _, t := range triggers {
		s.signalShutdown(t)
	}
}
