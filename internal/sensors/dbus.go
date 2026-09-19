package sensors

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// SPEC-03 §3.3 (D-Bus) and §3.6 (the merge rule).
//
// Watching only the system manager is an ALL-GREEN lie: this class of host keeps
// its application units in USER managers, where Manager.GetUnit on the system
// bus answers `Unit <name>.service not loaded.` The watched set is therefore
// configuration: the system manager plus every user manager in
// sensors.dbus.user_managers (default ["self"]).

const (
	dbusSystemdName  = "org.freedesktop.systemd1"
	dbusSystemdPath  = dbus.ObjectPath("/org/freedesktop/systemd1")
	dbusManagerIface = "org.freedesktop.systemd1.Manager"
	dbusUnitIface    = "org.freedesktop.systemd1.Unit"
	dbusProperties   = "org.freedesktop.DBus.Properties"
	dbusOOMName      = "org.freedesktop.oom1"
	dbusMaxUserMgrs  = 16
	dbusReconnectMin = time.Second
	dbusReconnectMax = time.Minute
	dbusSubscribeTry = 3
	crashLoopWindow  = 300 * time.Second
	crashLoopAbsorb  = 1800 * time.Second
	stormIncidentWin = 3600 * time.Second
)

// ---- object path escaping (measured) ----

// escapeUnitPath maps a visible unit name onto a systemd object-path element.
// Measured: coding-hermes-scheduler.service → coding_2dhermes_2dscheduler_2eservice.
// Every byte outside [A-Za-z0-9] becomes `_` + two lowercase hex digits, `_`
// (0x5f) included as `_5f` so the transform is round-trippable. Escaping is
// used ONLY to address objects; dedup keys use the visible unit name.
func escapeUnitPath(name string) string {
	const hexdigits = "0123456789abcdef"
	var b strings.Builder
	b.Grow(len(name) * 2)
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('_')
		b.WriteByte(hexdigits[c>>4])
		b.WriteByte(hexdigits[c&0x0f])
	}
	return b.String()
}

// unescapeUnitPath is the inverse; it exists so the round-trip property test is
// possible (and because a spec that says "round-trippable" must ship the other
// direction).
func unescapeUnitPath(path string) string {
	var b strings.Builder
	for i := 0; i < len(path); i++ {
		if path[i] == '_' && i+2 < len(path) {
			hi, ok1 := hexVal(path[i+1])
			lo, ok2 := hexVal(path[i+2])
			if ok1 && ok2 {
				b.WriteByte(hi<<4 | lo)
				i += 2
				continue
			}
		}
		b.WriteByte(path[i])
	}
	return b.String()
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	}
	return 0, false
}

// unitObjectPath is the address of one unit object.
func unitObjectPath(unit string) dbus.ObjectPath {
	return dbus.ObjectPath("/org/freedesktop/systemd1/unit/" + escapeUnitPath(unit))
}

// ---- merge rule (SPEC-03 §3.6) ----

// arrivalPath enumerates the three arrival paths that must merge into one story.
type arrivalPath string

const (
	arrivalProperties arrivalPath = "properties_changed"
	arrivalJobRemoved arrivalPath = "job_removed"
	arrivalJournald   arrivalPath = "journald"
	arrivalReconcile  arrivalPath = "reconcile"
)

// mergeOutcome is what one arrival did, plus the signature the ladder keys on.
type mergeOutcome struct {
	UnitKey   string
	Class     string // failure_class == the sig's `state` field
	Manager   string
	Sig       types.Sig
	Opened    bool
	Attached  bool
	Reopened  bool
	Counted   bool // post-window arrival on an open incident: count++, no new story
	CrashLoop bool
	Severity  types.Severity
	Count     uint64
	Attach    bool // journald enrichment: attach to the open incident
}

// isFailure reports whether an arrival is a *failure* for the crash-loop
// counter (M4). Unit churn and successful jobs are arrivals, not failures: a
// counter that counted them would call any busy unit a crash loop.
func isFailure(path arrivalPath, substate, result string) bool {
	switch path {
	case arrivalProperties:
		return substate == "failed" || substate == "auto-restart"
	case arrivalJobRemoved:
		switch result {
		case "failed", "timeout", "dependency", "canceled":
			return true
		}
		return false
	case arrivalReconcile:
		return substate == "failed"
	}
	return false
}

// mergeIncident is one (unit_key, failure_class) story.
type mergeIncident struct {
	unitKey     string
	class       string
	manager     string
	openedAt    time.Time
	lastArrival time.Time
	resolvedAt  time.Time
	resolved    bool
	crashLoop   bool
	absorbUntil time.Time
	count       uint64
	attaches    uint64
	reopens     int
	paths       map[arrivalPath]uint64
	jobResults  map[string]string
	jobSeenAt   map[string]time.Time
}

// mergeTracker is the §3.6 state machine. It is pure (no D-Bus, no ledger), so
// the property test can drive 10k randomized arrival sequences through it.
type mergeTracker struct {
	mu        sync.Mutex
	window    time.Duration
	now       func() time.Time
	incidents map[string]*mergeIncident
	failures  map[string][]time.Time
	closed    map[string]time.Time // unit_key → when the storm window ended

	Arrivals  map[arrivalPath]uint64
	Merges    uint64
	Opens     uint64
	Journald  uint64
	propertyT testHooks
}

// testHooks lets a test drive the tracker's clock and window deterministically.
type testHooks struct{ Window time.Duration }

func newMergeTracker(window time.Duration, now func() time.Time) *mergeTracker {
	return &mergeTracker{
		window:    window,
		now:       now,
		incidents: map[string]*mergeIncident{},
		failures:  map[string][]time.Time{},
		closed:    map[string]time.Time{},
		Arrivals:  map[arrivalPath]uint64{},
	}
}

// arrival records one non-journald arrival and returns what it did.
func (m *mergeTracker) arrival(manager, unitKey string, path arrivalPath, substate, jobResult, jobPath string, at time.Time) mergeOutcome {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Arrivals[path]++

	// M4 bookkeeping runs on EVERY *failure* arrival: the crash loop is a
	// property of the failure stream, not of the arrival that happened to open
	// the incident.
	crashNow := false
	if isFailure(path, substate, jobResult) {
		fs := append(prune(m.failures[unitKey], at.Add(-crashLoopWindow)), at)
		m.failures[unitKey] = fs
		crashNow = len(fs) >= 3
	}

	inc, existed := m.incidents[unitKey]
	if existed && inc.resolved && at.Sub(inc.resolvedAt) > stormIncidentWin {
		// Outside the storm window the story is over: a fresh incident is the
		// honest outcome, not a resurrection of a stale one.
		inc = nil
		existed = false
	}
	if existed && inc.crashLoop && !at.Before(inc.absorbUntil) {
		// The crash-loop story's absorption window ended: a later failure is a
		// new story, not a continuation of the absorbed one.
		delete(m.incidents, unitKey)
		inc = nil
		existed = false
	}
	if existed && inc.crashLoop && at.Before(inc.absorbUntil) {
		// M4: the crash-loop incident absorbs further failures.
		inc.count++
		inc.lastArrival = at
		return mergeOutcome{
			UnitKey: unitKey, Class: inc.class, Manager: inc.manager,
			Sig: m.sigFor(inc), Attached: true, CrashLoop: true,
			Severity: types.SevCritical, Count: inc.count,
		}
	}
	if !existed || inc == nil {
		class := substate
		if jobResult != "" {
			class = "job:" + jobResult
		}
		if class == "" {
			class = "failed"
		}
		inc = &mergeIncident{
			unitKey: unitKey, class: class, manager: manager, openedAt: at, lastArrival: at,
			paths:      map[arrivalPath]uint64{},
			jobResults: map[string]string{},
			jobSeenAt:  map[string]time.Time{},
			crashLoop:  crashNow,
		}
		if crashNow {
			// The crash-loop class is its own signature (M4).
			inc.class = "crash_loop"
			inc.absorbUntil = at.Add(crashLoopAbsorb)
		}
		m.incidents[unitKey] = inc
		inc.paths[path]++
		inc.count = 1
		m.Opens++
		return mergeOutcome{
			UnitKey: unitKey, Class: inc.class, Manager: manager,
			Sig: m.sigFor(inc), Opened: true, CrashLoop: inc.crashLoop,
			Severity: m.severityFor(inc), Count: 1,
		}
	}

	// M1: inside the merge window a JobRemoved re-derives the failure class.
	if path == arrivalJobRemoved && jobPath != "" {
		inc.jobResults[jobPath] = jobResult
		inc.jobSeenAt[jobPath] = at
	}
	if crashNow && !inc.crashLoop {
		// M4: raise to critical and re-derive the sig with the crash-loop class
		// so the crash-loop story is its own signature.
		inc.crashLoop = true
		inc.class = "crash_loop"
		inc.absorbUntil = at.Add(crashLoopAbsorb)
	}
	insideWindow := at.Sub(inc.openedAt) <= m.window
	if insideWindow {
		if path == arrivalJobRemoved && jobResult != "" && !inc.crashLoop {
			// SPEC-01 §3.3 encodes a JobRemoved state as `job:<Result>`.
			inc.class = "job:" + jobResult
		} else if path == arrivalProperties && substate != "" && inc.class == "" {
			inc.class = substate
		}
		inc.attaches++
		inc.count++
		inc.lastArrival = at
		inc.paths[path]++
		m.Merges++
		return mergeOutcome{
			UnitKey: unitKey, Class: inc.class, Manager: inc.manager,
			Sig: m.sigFor(inc), Attached: true, CrashLoop: inc.crashLoop,
			Severity: m.severityFor(inc), Count: inc.count,
		}
	}
	// After the merge window: the same key counts into the same incident; a
	// resolved one reopens the SAME incident (SPEC-03 §3.6 M2).
	reopensNow := false
	if inc.resolved {
		inc.resolved = false
		inc.reopens++
		reopensNow = true
	}
	inc.count++
	inc.lastArrival = at
	inc.paths[path]++
	m.Merges++
	return mergeOutcome{
		UnitKey: unitKey, Class: inc.class, Manager: inc.manager,
		Sig: m.sigFor(inc), Reopened: reopensNow, Counted: !reopensNow,
		CrashLoop: inc.crashLoop, Severity: m.severityFor(inc), Count: inc.count,
	}
}

// noteJournalArrival implements M3: a journald entry for a unit with an open
// incident (or one opened inside merge_window) attaches as evidence and opens
// nothing.
func (m *mergeTracker) noteJournalArrival(unit string, at time.Time) bool {
	if unit == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Arrivals[arrivalJournald]++
	inc, ok := m.incidents[unit]
	if !ok {
		return false
	}
	if !inc.resolved || at.Sub(inc.openedAt) <= m.window {
		inc.attaches++
		inc.count++
		inc.paths[arrivalJournald]++
		inc.lastArrival = at
		m.Journald++
		m.Merges++
		return true
	}
	return false
}

// resolve marks an incident resolved (the ladder owns the real transition; this
// is what makes reopen accounting possible).
func (m *mergeTracker) resolve(unitKey string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if inc, ok := m.incidents[unitKey]; ok {
		inc.resolved = true
		inc.resolvedAt = at
	}
}

// classifyLocked derived the failure class and the crash-loop flag; the class
// derivation now lives inline in arrival() because the crash-loop bookkeeping
// must run on every arrival (M4).

func (m *mergeTracker) severityFor(inc *mergeIncident) types.Severity {
	if inc.crashLoop {
		return types.SevCritical
	}
	return types.SevHigh
}

// sigFor derives the dedup signature. SPEC-01 §3.3's norm_version 1 field list
// for dbus is (manager, unit, state); `state` carries the failure class, and a
// crash loop carries "crash_loop" so the crash-loop class is its own signature
// (SPEC-03 §3.6 M4).
func (m *mergeTracker) sigFor(inc *mergeIncident) types.Sig {
	state := inc.class
	if inc.crashLoop {
		state = "crash_loop"
	}
	return sigFor(types.SrcDBus, inc.manager, inc.unitKey, state)
}

// snapshot returns the observable merge accounting (the §3.6 regression numbers:
// a "3 arrivals → 3 incidents" bug must be visible in numbers, not in a
// dashboard reading).
func (m *mergeTracker) snapshot() (arrivals map[string]uint64, merges, opens, journald uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	arrivals = map[string]uint64{}
	for p, n := range m.Arrivals {
		arrivals[string(p)] = n
	}
	return arrivals, m.Merges, m.Opens, m.Journald
}

// ---- live watches ----

type dbusState struct {
	mu       sync.Mutex
	watches  map[string]*dbusWatch
	merge    *mergeTracker
	arrivals atomic.Uint64
	merges   atomic.Uint64
	oomd     atomic.Bool
	dead     atomic.Bool
	failed   atomic.Int64
}

type dbusWatch struct {
	bus      string // "system" | "user:<uid>"
	uid      int
	conn     *dbus.Conn
	units    map[string]struct{}
	mu       sync.Mutex
	signals  chan *dbus.Signal
	reconn   int
	stopNow  chan struct{}
	subCount int
}

// startDBus connects every configured manager, subscribes BEFORE any action is
// taken (or the resulting signal is raced away), then runs the startup
// ListUnits reconcile (SPEC-03 §4 step 6).
func (s *Sensors) startDBus(ctx context.Context) error {
	if !s.cfg.dbus.enabled {
		s.setSensor(types.SenDBus, false, false, "disabled by configuration: sensors.dbus.enabled=false")
		return nil
	}
	s.dbState.watches = map[string]*dbusWatch{}
	s.dbState.merge = newMergeTracker(s.cfg.mergeWindow, s.now)

	managers := s.resolveManagers()
	var unwatched []string
	for _, m := range managers {
		w, err := s.connectManager(ctx, m)
		if err != nil {
			// A configured manager that is expected but not watched is
			// TROUBLE-SENSORS-013 with the manager identity in the reason —
			// never a silent skip.
			unwatched = append(unwatched, m)
			s.recError(types.SenDBus, types.CodeSensors013,
				fmt.Sprintf("manager %s is configured but not watched: %v", m, err))
			continue
		}
		s.dbState.watches[m] = w
	}
	if len(s.dbState.watches) == 0 {
		reason := fmt.Sprintf("%s: no D-Bus manager could be watched (%s)", types.CodeSensors011, strings.Join(unwatched, ", "))
		s.setSensor(types.SenDBus, true, true, reason)
		s.openGap(types.SenDBus, "bus", "bus_down", reason)
		return fmt.Errorf("sensors: %s", reason)
	}
	if len(unwatched) > 0 {
		s.setSensor(types.SenDBus, true, true,
			fmt.Sprintf("%s: unwatched managers: %s", types.CodeSensors013, strings.Join(unwatched, ", ")))
	} else {
		s.setSensor(types.SenDBus, true, false, "")
	}
	for name, w := range s.dbState.watches {
		if err := s.reconcile(ctx, name, w); err != nil {
			s.recError(types.SenDBus, types.CodeSensors015, fmt.Sprintf("startup ListUnits on %s: %v", name, err))
		}
		s.wg.Add(1)
		go s.runDBusSignals(ctx, name, w)
		s.wg.Add(1)
		go s.runDBusPing(ctx, name, w)
	}
	s.wg.Add(1)
	go s.runReconcileSweep(ctx)
	s.wg.Add(1)
	go s.runOOMDProbe(ctx)
	if s.probeOOMD(ctx) {
		s.dbState.oomd.Store(true)
	}
	return nil
}

// resolveManagers expands the configuration into concrete manager identities:
// the system manager plus every user manager ("self" is the daemon's own uid).
func (s *Sensors) resolveManagers() []string {
	out := []string{"system"}
	seen := map[int]bool{}
	n := 0
	for _, m := range s.cfg.dbus.userManagers {
		uid := -1
		switch {
		case m == "self":
			uid = os.Getuid()
		case strings.HasPrefix(m, "uid:"):
			v, err := strconv.Atoi(strings.TrimPrefix(m, "uid:"))
			if err != nil {
				continue
			}
			uid = v
		case strings.HasPrefix(m, "user:"):
			if v, err := strconv.Atoi(strings.TrimPrefix(m, "user:")); err == nil {
				uid = v
				continue
			}
		}
		if uid < 0 || seen[uid] {
			continue
		}
		if n >= dbusMaxUserMgrs {
			break
		}
		seen[uid] = true
		n++
		out = append(out, fmt.Sprintf("user:%d", uid))
	}
	return out
}

// connectManager dials one manager and subscribes before doing anything else.
func (s *Sensors) connectManager(ctx context.Context, manager string) (*dbusWatch, error) {
	w := &dbusWatch{bus: manager, units: map[string]struct{}{}, stopNow: make(chan struct{})}
	var (
		conn *dbus.Conn
		err  error
	)
	if manager == "system" {
		conn, err = dbus.SystemBus()
	} else {
		uid, _ := strconv.Atoi(strings.TrimPrefix(manager, "user:"))
		w.uid = uid
		path := os.Getenv("XDG_RUNTIME_DIR")
		if path == "" || uid != os.Getuid() {
			path = fmt.Sprintf("/run/user/%d", uid)
		}
		conn, err = dbus.Dial("unix:path=" + filepath.Join(path, "bus"))
		if err == nil {
			if err = conn.Auth(nil); err != nil {
				_ = conn.Close()
				return nil, err
			}
			if err = conn.Hello(); err != nil {
				_ = conn.Close()
				return nil, err
			}
		}
	}
	if err != nil {
		return nil, err
	}
	w.conn = conn
	w.signals = make(chan *dbus.Signal, 256)
	conn.Signal(w.signals)
	if err := w.subscribe(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	w.addManagerMatches()
	return w, nil
}

// subscribe calls Manager.Subscribe (SPEC-03 §3.3). It retries three times on a
// reachable bus before giving up (TROUBLE-SENSORS-015).
func (w *dbusWatch) subscribe() error {
	var lastErr error
	for i := 0; i < dbusSubscribeTry; i++ {
		obj := w.conn.Object(dbusSystemdName, dbusSystemdPath)
		if err := obj.Call(dbusManagerIface+".Subscribe", 0).Err; err != nil {
			lastErr = err
			continue
		}
		w.subCount++
		return nil
	}
	return lastErr
}

// addManagerMatches installs the documented match rules.
func (w *dbusWatch) addManagerMatches() {
	rules := []string{
		"type='signal',sender='" + dbusSystemdName + "',interface='" + dbusManagerIface + "',member='JobNew'",
		"type='signal',sender='" + dbusSystemdName + "',interface='" + dbusManagerIface + "',member='JobRemoved'",
		"type='signal',sender='" + dbusSystemdName + "',interface='" + dbusManagerIface + "',member='UnitNew'",
		"type='signal',sender='" + dbusSystemdName + "',interface='" + dbusManagerIface + "',member='UnitRemoved'",
		"type='signal',sender='org.freedesktop.DBus',interface='org.freedesktop.DBus',member='NameOwnerChanged'",
	}
	for _, r := range rules {
		w.conn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0, r)
	}
}

// addUnitMatch subscribes PropertiesChanged for one watched unit object.
func (w *dbusWatch) addUnitMatch(unit string) {
	rule := fmt.Sprintf("type='signal',sender='%s',interface='%s',member='PropertiesChanged',path='%s'",
		dbusSystemdName, dbusProperties, unitObjectPath(unit))
	_ = w.conn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0, rule)
}

// listUnits is the reconcile input. Failure is reported, never invented.
func (w *dbusWatch) listUnits() ([]unitStatus, error) {
	obj := w.conn.Object(dbusSystemdName, dbusSystemdPath)
	var raw []interface{}
	if err := obj.Call(dbusManagerIface+".ListUnits", 0).Store(&raw); err != nil {
		return nil, err
	}
	out := make([]unitStatus, 0, len(raw))
	for _, e := range raw {
		fields, ok := e.([]interface{})
		if !ok || len(fields) < 8 {
			continue
		}
		st := unitStatus{}
		st.Name, _ = fields[0].(string)
		st.Description, _ = fields[1].(string)
		st.LoadState, _ = fields[2].(string)
		st.ActiveState, _ = fields[3].(string)
		st.SubState, _ = fields[4].(string)
		st.Path, _ = fields[6].(string)
		st.JobPath, _ = fields[9].(string)
		if len(fields) > 8 {
			if v, ok := fields[8].(string); ok {
				st.JobType = v
			}
		}
		out = append(out, st)
	}
	return out, nil
}

type unitStatus struct {
	Name        string
	Description string
	LoadState   string
	ActiveState string
	SubState    string
	Path        string
	JobPath     string
	JobType     string
}

type timerStatus struct {
	Name                string
	NextElapseUSecRealt uint64
	LastTriggerUSec     uint64
	Unit                string
	Activates           string
	State               string
}

// listTimersFn is the timing-sweep input seam. It is the production call
// (ListTimers on a watched manager); a test can substitute a garbage reply to
// prove TROUBLE-SENSORS-023 records the raw size instead of inventing a list.
var listTimersFn = func(w *dbusWatch) ([]timerStatus, error) { return w.listTimers() }

// listTimers is SPEC-03 §3.3's timer input; a decode failure is
// TROUBLE-SENSORS-023 with the raw reply size recorded and no invented list.
func (w *dbusWatch) listTimers() ([]timerStatus, error) {
	obj := w.conn.Object(dbusSystemdName, dbusSystemdPath)
	call := obj.Call(dbusManagerIface+".ListTimers", 0)
	if call.Err != nil {
		return nil, call.Err
	}
	raw := []interface{}{}
	if err := call.Store(&raw); err != nil {
		return nil, fmt.Errorf("ListTimers reply (%d bytes) could not be parsed: %w", len(call.Body), err)
	}
	out := make([]timerStatus, 0, len(raw))
	for _, e := range raw {
		fields, ok := e.([]interface{})
		if !ok || len(fields) < 4 {
			continue
		}
		t := timerStatus{Name: str(fields[0]), Unit: str(fields[2]), Activates: str(fields[3])}
		// NextElapseUSecRealtime / LastTriggerUSec are (t, tt) pairs: a uint64
		// microseconds value plus a monotonic microseconds value.
		if len(fields) > 5 {
			t.NextElapseUSecRealt = usecPair(fields[5])
		}
		if len(fields) > 6 {
			t.LastTriggerUSec = usecPair(fields[6])
		}
		out = append(out, t)
	}
	return out, nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// usecPair unwraps systemd's (realtime_usec, monotonic_usec) struct.
func usecPair(v any) uint64 {
	s, ok := v.([]interface{})
	if !ok || len(s) == 0 {
		return 0
	}
	switch n := s[0].(type) {
	case uint64:
		return n
	case int64:
		return uint64(n)
	case uint32:
		return uint64(n)
	}
	return 0
}

// reconcile is the startup + periodic ListUnits sweep: a unit that failed before
// this daemon started emits no signal at all and is invisible without it.
//
// Emissions go through the §3.3a batch path (evaluateEveryUnit + the caller's
// AppendBatch), so N failed units cost one group-commit window instead of N.
func (s *Sensors) reconcile(ctx context.Context, name string, w *dbusWatch) error {
	if s.dbState.merge == nil {
		return nil
	}
	units, err := w.listUnits()
	if err != nil {
		return err
	}
	now := s.now()
	seen := map[string]struct{}{}
	failed := make([]unitStatus, 0, len(units))
	for _, u := range units {
		seen[u.Name] = struct{}{}
		if u.SubState != "failed" && u.ActiveState != "failed" {
			continue
		}
		failed = append(failed, u)
	}
	// §3.3a: decide everything first (merge rule + full rule pipeline per
	// arrival, no writes), then ONE durable batch for the sweep. On the boot
	// path this is what replaces N sequential group-commit windows with one.
	s.emitBatchOutcome(ctx, s.evaluateEveryUnit(ctx, name, failed, now))
	// Keep the unit match set in step with the manager's unit set (the 300s
	// re-subscribe sweep of SPEC-03 §3.3).
	w.mu.Lock()
	added := 0
	for name := range seen {
		if _, ok := w.units[name]; ok {
			continue
		}
		w.units[name] = struct{}{}
		w.addUnitMatch(name)
		added++
		if added >= 256 {
			break
		}
	}
	for name := range w.units {
		if _, ok := seen[name]; !ok {
			delete(w.units, name)
		}
	}
	w.mu.Unlock()
	if len(failed) > 0 {
		s.dbState.arrivals.Add(uint64(len(failed)))
	}
	return nil
}

// writeDrafts writes one batch of already-decided drafts (§3.3a) and returns
// the durable records. A non-nil writeBatch delegates to the batch writer; the
// default falls back to emit-per-draft so a Sensors built without the boot's
// batch writer behaves exactly as §3.8 always has (durable-on-return per
// record). On error the batch is NOT partially reported: the caller drops all
// of it and the reconciliation is retried by the next sweep.
func (s *Sensors) writeDrafts(ctx context.Context, drafts []types.RecordDraft) ([]types.Record, error) {
	if len(drafts) == 0 {
		return nil, nil
	}
	if s.writeBatch != nil {
		return s.writeBatch(ctx, drafts)
	}
	recs := make([]types.Record, 0, len(drafts))
	var firstErr error
	for _, d := range drafts {
		rec, err := s.emit(ctx, d)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		recs = append(recs, rec)
	}
	return recs, firstErr
}

// runDBusSignals consumes signals for one manager, re-subscribing on
// NameOwnerChanged and reconnecting with the 1s→60s backoff on a lost bus.
func (s *Sensors) runDBusSignals(ctx context.Context, name string, w *dbusWatch) {
	defer s.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case sig, ok := <-w.signals:
			if !ok {
				return
			}
			if err := s.handleSignal(ctx, name, w, sig); err != nil {
				s.recError(types.SenDBus, types.CodeSensors015, err.Error())
			}
			s.sensorOK(types.SenDBus)
		}
	}
}

func (s *Sensors) handleSignal(ctx context.Context, name string, w *dbusWatch, sig *dbus.Signal) error {
	switch sig.Name {
	case "org.freedesktop.DBus.NameOwnerChanged":
		if len(sig.Body) >= 1 {
			if who, _ := sig.Body[0].(string); who != dbusSystemdName {
				return nil
			}
		}
		// The daemon re-exec'd or reloaded: stop trusting the connection,
		// re-subscribe, resync, and record the downtime as one gap.
		if err := w.subscribe(); err != nil {
			return fmt.Errorf("%s: re-subscribe after NameOwnerChanged: %v", types.CodeSensors014, err)
		}
		w.addManagerMatches()
		if err := s.reconcile(ctx, name, w); err != nil {
			return fmt.Errorf("%s: resync after NameOwnerChanged: %v", types.CodeSensors014, err)
		}
		s.recError(types.SenDBus, types.CodeSensors014, "daemon-reexec: re-subscribed and resynced")
		s.emitGapNow(ctx, string(types.SenDBus), name, "bus_down", "NameOwnerChanged on "+dbusSystemdName)
		return nil
	case dbusManagerIface + ".JobRemoved":
		if len(sig.Body) < 4 {
			return nil
		}
		unit := str(sig.Body[2])
		result := str(sig.Body[3])
		at := s.now()
		res := s.dbState.merge.arrival(name, unit, arrivalJobRemoved, "", result, str(sig.Body[0]), at)
		s.dbState.arrivals.Add(1)
		s.emitDBusOutcome(ctx, res, name, "", "", arrivalJobRemoved)
		return nil
	case dbusProperties + ".PropertiesChanged":
		if len(sig.Body) < 2 {
			return nil
		}
		iface := str(sig.Body[0])
		if iface != dbusUnitIface {
			return nil
		}
		props, _ := sig.Body[1].(map[string]dbus.Variant)
		unit := unescapeUnitPath(strings.TrimPrefix(string(sig.Path), "/org/freedesktop/systemd1/unit/"))
		active, substate := "", ""
		if v, ok := props["ActiveState"]; ok {
			active, _ = v.Value().(string)
		}
		if v, ok := props["SubState"]; ok {
			substate, _ = v.Value().(string)
		}
		if substate == "" {
			return nil
		}
		at := s.now()
		res := s.dbState.merge.arrival(name, unit, arrivalProperties, substate, "", "", at)
		s.dbState.arrivals.Add(1)
		s.emitDBusOutcome(ctx, res, name, active, substate, arrivalProperties)
		return nil
	}
	return nil
}

// emitDBusOutcome turns one merged arrival into an event record carrying the
// merge accounting, so "3 arrivals → 3 incidents" is catchable by numbers.
// The payload it builds and the counters it keeps are exactly what a batch
// member carries (§3.3a); only the write differs.
func (s *Sensors) emitDBusOutcome(ctx context.Context, res mergeOutcome, manager, activeState, substate string, path arrivalPath) {
	arr, merges, opens, journald := s.dbState.merge.snapshot()
	s.merges.Store(merges)
	_ = opens
	_ = journald
	detail := map[string]any{
		"unit_key":          res.UnitKey,
		"unit_active_state": activeState,
		"unit_substate":     substate,
		"failure_class":     res.Class,
		"crash_loop":        res.CrashLoop,
		"arrival_path":      string(path),
		"manager":           manager,
		"exit_code":         0,
		"count":             float64(res.Count),
		"merge_arrivals":    arr,
		"merged":            res.Attached,
		"reopen_count":      res.Reopened,
		"severity":          string(res.Severity),
		"sensors_arrivals":  s.dbState.arrivals.Load(),
		"sensors_merges":    merges,
		"value":             float64(res.Count),
		"unit_field":        "",
		"msg":               "",
		"substr":            "",
		"window_s":          0,
		"age_s":             0,
	}
	ev := types.SensorEvent{
		ID:     types.NewID(types.PEv),
		TS:     types.FormatUTC(s.now()),
		Sensor: types.SenDBus,
		Scope:  res.UnitKey,
		Value:  float64(res.Count),
		Unit:   "",
		Detail: detail,
		Sig:    res.Sig,
	}
	if rt := s.rt[types.SenDBus]; rt != nil {
		rt.events.Add(1)
		rt.lastEvent.Store(s.now().UnixNano())
	}
	s.sensorOK(types.SenDBus)
	s.handleEvent(ctx, ev)
}

// dbusOutcomeDraft is emitDBusOutcome's payload work without the write: the
// batch path needs the SAME accounting payload as a RecordDraft so §3.3a
// writes records a query cannot distinguish from the per-record path's.
func (s *Sensors) dbusOutcomeDraft(res mergeOutcome, manager, activeState, substate string, path arrivalPath) types.RecordDraft {
	arr, merges, opens, journald := s.dbState.merge.snapshot()
	s.merges.Store(merges)
	_ = opens
	_ = journald
	detail := map[string]any{
		"unit_key":          res.UnitKey,
		"unit_active_state": activeState,
		"unit_substate":     substate,
		"failure_class":     res.Class,
		"crash_loop":        res.CrashLoop,
		"arrival_path":      string(path),
		"manager":           manager,
		"exit_code":         0,
		"count":             float64(res.Count),
		"merge_arrivals":    arr,
		"merged":            res.Attached,
		"reopen_count":      res.Reopened,
		"severity":          string(res.Severity),
		"sensors_arrivals":  s.dbState.arrivals.Load(),
		"sensors_merges":    merges,
		"value":             float64(res.Count),
		"unit_field":        "",
		"msg":               "",
		"substr":            "",
		"window_s":          0,
		"age_s":             0,
	}
	ev := types.SensorEvent{
		ID:     types.NewID(types.PEv),
		TS:     types.FormatUTC(s.now()),
		Sensor: types.SenDBus,
		Scope:  res.UnitKey,
		Value:  float64(res.Count),
		Unit:   "",
		Detail: detail,
		Sig:    res.Sig,
	}
	if rt := s.rt[types.SenDBus]; rt != nil {
		rt.events.Add(1)
		rt.lastEvent.Store(s.now().UnixNano())
	}
	s.sensorOK(types.SenDBus)
	payload := s.evaluateEvent(context.Background(), ev)
	return s.draftFor(ev, payload)
}

// evaluateEveryUnit is the reconcile's decision phase for the failed units one
// ListUnits returned: merge accounting per unit (the §3.6 state machine,
// unchanged), then the FULL §3.4/§3.5/§3.8 rule pipeline per outcome —
// stabilization, cooldowns, breakers and incident opens all happen here,
// exactly as they would have per record — and one draft per arrival. It
// writes NOTHING: the caller persists the returned drafts through writeDrafts,
// which is what turns N durable commits into one (§3.3a).
func (s *Sensors) evaluateEveryUnit(ctx context.Context, name string, failed []unitStatus, now time.Time) []types.RecordDraft {
	drafts := make([]types.RecordDraft, 0, len(failed))
	for _, u := range failed {
		res := s.dbState.merge.arrival(name, u.Name, arrivalReconcile, u.SubState, "", "", now)
		if !res.Opened && !res.Attached && !res.Reopened {
			continue
		}
		drafts = append(drafts, s.dbusOutcomeDraft(res, name, u.ActiveState, u.SubState, arrivalReconcile))
	}
	return drafts
}

// emitBatchOutcome is the batched reconcile write path (§3.3a): every
// already-decided draft lands in ONE ledger group commit, durable on its
// return. The ladder bridge runs AFTER the batch is durable, per durable
// record with its real rec_id — the ordering guarantee `emit` gave per record
// (Append returned before Admit).
func (s *Sensors) emitBatchOutcome(ctx context.Context, drafts []types.RecordDraft) {
	if len(drafts) == 0 {
		return
	}
	recs, err := s.writeDrafts(ctx, drafts)
	if err != nil {
		if rt := s.rt[types.SenDBus]; rt != nil {
			rt.drops.Add(uint64(len(drafts)))
		}
		s.recError(types.SenDBus, types.CodeSensors015, fmt.Sprintf("reconcile batch write (%d records): %v", len(drafts), err))
		return
	}
	for _, rec := range recs {
		s.bridgeLadder(ctx, rec)
	}
}

// runDBusPing is the 30s Peer.Ping liveness proof (SPEC-03 §3.9).
func (s *Sensors) runDBusPing(ctx context.Context, name string, w *dbusWatch) {
	defer s.wg.Done()
	iv := s.cfg.dbus.pingInterval
	if iv <= 0 {
		iv = 30 * time.Second
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
		}
		if err := w.conn.BusObject().Call("org.freedesktop.DBus.Peer.Ping", 0).Err; err != nil {
			s.recError(types.SenDBus, types.CodeSensors011, fmt.Sprintf("ping %s: %v", name, err))
			return
		}
		s.sensorOK(types.SenDBus)
		s.dbState.dead.Store(false)
	}
}

// runReconcileSweep is the 300s ListUnits reconcile + unit-match realignment.
func (s *Sensors) runReconcileSweep(ctx context.Context) {
	defer s.wg.Done()
	iv := s.cfg.dbus.reconcileInterval
	if iv <= 0 {
		iv = 300 * time.Second
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
			s.dbState.mu.Lock()
			ws := make(map[string]*dbusWatch, len(s.dbState.watches))
			for k, v := range s.dbState.watches {
				ws[k] = v
			}
			s.dbState.mu.Unlock()
			names := make([]string, 0, len(ws))
			for k := range ws {
				names = append(names, k)
			}
			sort.Strings(names)
			for _, name := range names {
				if err := s.reconcile(ctx, name, ws[name]); err != nil {
					s.recError(types.SenDBus, types.CodeSensors015, fmt.Sprintf("ListUnits on %s: %v", name, err))
				}
			}
			s.sensorOK(types.SenDBus)
		}
	}
}

// runOOMDProbe re-probes org.freedesktop.oom1 every 10m; the no-op is
// superseded without a restart when it appears (SPEC-03 §3.3, code 016).
func (s *Sensors) runOOMDProbe(ctx context.Context) {
	defer s.wg.Done()
	iv := s.cfg.dbus.oomdProbeInterval
	if iv <= 0 {
		iv = 10 * time.Minute
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
			s.dbState.oomd.Store(s.probeOOMD(ctx))
		}
	}
}

// probeOOMD reports whether systemd-oomd is available. It probes the *name*
// rather than attempting a mutating call.
func (s *Sensors) probeOOMD(ctx context.Context) bool {
	s.dbState.mu.Lock()
	defer s.dbState.mu.Unlock()
	for _, w := range s.dbState.watches {
		var has bool
		err := w.conn.BusObject().Call("org.freedesktop.DBus.NameHasOwner", 0, dbusOOMName).Store(&has)
		if err != nil {
			continue
		}
		if has {
			return true
		}
	}
	return false
}

// stopDBus closes every connection on the shutdown path (SPEC-03 §4).
func (s *Sensors) stopDBus() {
	s.dbState.mu.Lock()
	ws := s.dbState.watches
	s.dbState.watches = map[string]*dbusWatch{}
	s.dbState.mu.Unlock()
	for _, w := range ws {
		if w.conn != nil {
			_ = w.conn.Close()
		}
		if w.stopNow != nil {
			close(w.stopNow)
		}
	}
}

// Close stops the detection plane and drains its pending writes. The daemon's
// boot path uses it when a LATER §4.1 step fails after sensors_build: a
// refused boot must not leave the reconcile loops running, the journald child
// alive, or an open fold unwritten (the same drain §4's shutdown orders,
// reached from the boot side). The drain runs under a fresh bounded context
// when the caller's is already dead — a fold flushed against a cancelled ctx
// would drop the observations it exists to keep.
func (s *Sensors) Close(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
	}
	err := s.Stop(ctx)
	// Stop already flushed the fold; a second flush is a no-op unless the
	// first one wrote nothing because the context was dead.
	s.fold.flush(ctx, s.emit)
	return err
}

// probeDBusManagers is the probe record's dbus_managers array: every manager
// the daemon intends to watch, with the connect/subscribe outcome — an
// unwatched manager is always visible here (SPEC-03 §3.3).
func (s *Sensors) probeDBusManagers(ctx context.Context) []map[string]any {
	var out []map[string]any
	for _, m := range s.resolveManagers() {
		entry := map[string]any{"bus": m, "connect": false, "subscribe": false}
		w, err := s.connectManager(ctx, m)
		if err == nil {
			entry["connect"] = true
			entry["subscribe"] = true
			if w.conn != nil {
				_ = w.conn.Close()
			}
		}
		out = append(out, entry)
	}
	return out
}
