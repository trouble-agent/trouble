package registry

// modules_proc.go — proc.top, proc.connections (SPEC-06 §3.8, §3.9).
//
// Both are read-only: proc.root (default /proc), proc.top_n (default 15) and
// proc.fd_scan_max (default 20000) bound them, and both are `pure` — Apply is a
// read that carries the payload, Check returns Diff{Empty:true} with the one-line
// observation and Verify is a recheck (§3.4, §3.9). Neither needs D-Bus or polkit
// ("capability default: always", §3.8): unit attribution comes from each process's
// cgroup, so a process whose unit cannot be resolved is reported with an empty
// unit rather than failing the call.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/registry/schemagen"
	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// procHertz is USER_HZ, the unit of /proc/<pid>/stat's utime/stime and of
// /proc/stat's btime/starttime fields. Every Linux the daemon runs on uses 100.
const procHertz = 100.0

// procPageSize is the page size /proc/<pid>/statm's resident count is in.
var procPageSize = os.Getpagesize()

// procRootDefault / procTopNDefault / procFDScanMaxDefault are the §4.3 defaults,
// applied when the configuration is unset (or absent, see environment()).
const (
	procRootDefault      = "/proc"
	procTopNDefault      = 15
	procFDScanMaxDefault = 20000
	procLimitDefault     = 200
	procLimitMax         = 2000
)

// procRoot resolves the configured proc root.
func procRoot(cfg config) string {
	if strings.TrimSpace(cfg.ProcRoot) != "" {
		return cfg.ProcRoot
	}
	return procRootDefault
}

// procTopN resolves the configured top-N.
func procTopN(cfg config, argsN int) int {
	if argsN > 0 {
		return argsN
	}
	if cfg.ProcTopN > 0 {
		return cfg.ProcTopN
	}
	return procTopNDefault
}

// procFDScanMax resolves the configured fd-scan bound.
func procFDScanMax(cfg config) int {
	if cfg.ProcFDScanMax > 0 {
		return cfg.ProcFDScanMax
	}
	return procFDScanMaxDefault
}

// procRow is one process sample of proc.top (§3.9).
type procRow struct {
	PID      int
	Comm     string
	CPUPct   float64
	RSSBytes uint64
	IOBytes  uint64
	Unit     string
}

// procTimeBase is the cpu_pct denominator: a single sample has no interval to
// divide by, so cpu_pct is the process's lifetime average — (utime+stime)/HZ over
// the wall time since it started. /proc/stat's btime gives the boot instant and
// /proc/uptime is the fallback when btime is absent.
type procTimeBase struct {
	Boot   time.Time
	Uptime float64
}

// procBootTime reads btime/uptime from the proc tree. A missing or unreadable file
// leaves the zero value, and cpu_pct then reports 0 rather than a fabricated rate.
func procBootTime(root string) procTimeBase {
	var base procTimeBase
	if data, err := os.ReadFile(filepath.Join(root, "stat")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if !strings.HasPrefix(line, "btime ") {
				continue
			}
			if secs, err := strconv.ParseInt(strings.TrimSpace(line[6:]), 10, 64); err == nil && secs > 0 {
				base.Boot = time.Unix(secs, 0)
			}
			break
		}
	}
	if data, err := os.ReadFile(filepath.Join(root, "uptime")); err == nil {
		fields := strings.Fields(string(data))
		if len(fields) > 0 {
			if up, err := strconv.ParseFloat(fields[0], 64); err == nil {
				base.Uptime = up
			}
		}
	}
	return base
}

// elapsed returns the process's elapsed wall-clock seconds, or 0 when it cannot
// be derived.
func (b procTimeBase) elapsed(startTicks uint64) float64 {
	if !b.Boot.IsZero() {
		start := b.Boot.Add(time.Duration(float64(startTicks) / procHertz * float64(time.Second)))
		if secs := time.Since(start).Seconds(); secs > 0 {
			return secs
		}
		return 0
	}
	if b.Uptime > 0 {
		secs := b.Uptime - float64(startTicks)/procHertz
		if secs > 0 {
			return secs
		}
	}
	return 0
}

// procPIDs lists the numeric entries of the proc root, sorted ascending.
func procPIDs(root string) []int {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	out := make([]int, 0, len(entries))
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		out = append(out, pid)
	}
	sort.Ints(out)
	return out
}

// procStatFields is the subset of /proc/<pid>/stat the sampler needs.
type procStatFields struct {
	Comm      string
	UTime     uint64
	STime     uint64
	StartTime uint64
}

// procParseStat parses /proc/<pid>/stat. Field 2 (comm) is parenthesised and may
// itself contain spaces and parentheses, so it is cut at the LAST ')'.
func procParseStat(data string) (procStatFields, error) {
	open := strings.IndexByte(data, '(')
	close := strings.LastIndexByte(data, ')')
	if open < 0 || close < open {
		return procStatFields{}, fmt.Errorf("proc: malformed stat line")
	}
	out := procStatFields{Comm: data[open+1 : close]}
	rest := strings.Fields(data[close+1:])
	// rest starts at field 3 (state); utime is field 14 and stime 15, starttime 22.
	if len(rest) < 20 {
		return procStatFields{}, fmt.Errorf("proc: stat line has %d trailing fields, want >= 20", len(rest))
	}
	utime, err := strconv.ParseUint(rest[11], 10, 64)
	if err != nil {
		return procStatFields{}, fmt.Errorf("proc: utime: %v", err)
	}
	stime, err := strconv.ParseUint(rest[12], 10, 64)
	if err != nil {
		return procStatFields{}, fmt.Errorf("proc: stime: %v", err)
	}
	start, err := strconv.ParseUint(rest[19], 10, 64)
	if err != nil {
		return procStatFields{}, fmt.Errorf("proc: starttime: %v", err)
	}
	out.UTime, out.STime, out.StartTime = utime, stime, start
	return out, nil
}

// procRSS reads /proc/<pid>/statm's resident page count.
func procRSS(root string, pid int) uint64 {
	data, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "statm"))
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0
	}
	pages, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return pages * uint64(procPageSize)
}

// procIO reads the process's read+write byte counters. They are frequently
// unreadable without root (and empty in a synthetic tree): 0 is the honest value,
// never an error, and it sorts last under `io`.
func procIO(root string, pid int) uint64 {
	data, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "io"))
	if err != nil {
		return 0
	}
	var total uint64
	for _, line := range strings.Split(string(data), "\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(name) {
		case "read_bytes", "write_bytes":
			if n, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64); err == nil {
				total += n
			}
		}
	}
	return total
}

// procUnit reads /proc/<pid>/cgroup and extracts the systemd unit: the last path
// element that names a unit. A process outside any unit (a bare cgroup, a
// container namespace) has unit "" and is never an error (§3.9).
func procUnit(root string, pid int) string {
	data, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return ""
	}
	return cgroupUnit(string(data))
}

// cgroupUnit extracts a unit name from a /proc/<pid>/cgroup body, whose lines are
// `hierarchy-id:controllers:/path`.
func cgroupUnit(data string) string {
	for _, line := range strings.Split(data, "\n") {
		_, path, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if _, p, ok := strings.Cut(path, ":"); ok {
			path = p
		}
		elems := strings.Split(strings.Trim(path, "/"), "/")
		for i := len(elems) - 1; i >= 0; i-- {
			if strings.HasSuffix(elems[i], ".service") {
				return elems[i]
			}
		}
	}
	return ""
}

// procScan samples every process under the root. A pid that exits mid-scan is
// skipped: the proc tree is a moving target and no read of it is atomic.
func procScan(root string, base procTimeBase) ([]procRow, error) {
	if _, err := os.Stat(root); err != nil {
		return nil, fmt.Errorf("%w: proc root %s is not readable: %v", types.ErrPermanent, root, err)
	}
	pids := procPIDs(root)
	out := make([]procRow, 0, len(pids))
	for _, pid := range pids {
		data, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "stat"))
		if err != nil {
			continue
		}
		fields, err := procParseStat(string(data))
		if err != nil {
			continue
		}
		row := procRow{
			PID:      pid,
			Comm:     fields.Comm,
			RSSBytes: procRSS(root, pid),
			IOBytes:  procIO(root, pid),
			Unit:     procUnit(root, pid),
		}
		if secs := base.elapsed(fields.StartTime); secs > 0 {
			row.CPUPct = float64(fields.UTime+fields.STime) / procHertz * 100 / secs
		}
		out = append(out, row)
	}
	return out, nil
}

// procSort applies the §3.9 `sort` key (cpu|rss|io), descending, with the pid as a
// stable tiebreak so two samples never swap places run to run.
func procSort(rows []procRow, key string) {
	less := func(i, j int) bool { return rows[i].PID < rows[j].PID }
	switch key {
	case "rss":
		less = func(i, j int) bool {
			if rows[i].RSSBytes != rows[j].RSSBytes {
				return rows[i].RSSBytes > rows[j].RSSBytes
			}
			return rows[i].PID < rows[j].PID
		}
	case "io":
		less = func(i, j int) bool {
			if rows[i].IOBytes != rows[j].IOBytes {
				return rows[i].IOBytes > rows[j].IOBytes
			}
			return rows[i].PID < rows[j].PID
		}
	default:
		less = func(i, j int) bool {
			if rows[i].CPUPct != rows[j].CPUPct {
				return rows[i].CPUPct > rows[j].CPUPct
			}
			return rows[i].PID < rows[j].PID
		}
	}
	sort.SliceStable(rows, less)
}

// procTopRows reads and ranks the process table for the args.
func procTopRows(env *moduleEnv, sortKey, unit string, n int) ([]procRow, int, error) {
	root := procRoot(env.cfg)
	base := procBootTime(root)
	rows, err := procScan(root, base)
	if err != nil {
		return nil, 0, err
	}
	want := normalizeUnit(unit)
	matched := rows[:0]
	for _, r := range rows {
		if want != "" && normalizeUnit(r.Unit) != want {
			continue
		}
		matched = append(matched, r)
	}
	procSort(matched, sortKey)
	total := len(matched)
	if n > 0 && len(matched) > n {
		matched = matched[:n]
	}
	return matched, total, nil
}

// procTopOutput is the §3.9 Output.
func procTopOutput(rows []procRow, matched int, sortKey, unit, root string, n int) map[string]any {
	procs := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		procs = append(procs, map[string]any{
			"pid":       r.PID,
			"comm":      r.Comm,
			"cpu_pct":   r.CPUPct,
			"rss_bytes": r.RSSBytes,
			"io_bytes":  r.IOBytes,
			"unit":      r.Unit,
		})
	}
	return map[string]any{
		"procs":   procs,
		"count":   len(procs),
		"matched": matched,
		"sort":    sortKey,
		"n":       n,
		"unit":    normalizeUnit(unit),
		"root":    root,
	}
}

// procTopSummary is the one-line observation (Check's Summary, §3.9).
func procTopSummary(rows []procRow, matched int, sortKey, unit string) string {
	summary := fmt.Sprintf("procs=%d sort=%s", matched, sortKey)
	if len(rows) > 0 {
		summary += fmt.Sprintf(" top=%d:%s", rows[0].PID, rows[0].Comm)
	}
	if u := normalizeUnit(unit); u != "" {
		summary += " unit=" + u
	}
	return summary
}

type procTopArgs struct {
	Sort string `json:"sort,omitempty" js:"enum=cpu|rss|io;default=cpu"`
	N    int    `json:"n,omitempty" js:"min=1;max=100;default=15"`
	Unit string `json:"unit,omitempty"`
}

type procConnectionsArgs struct {
	Unit   string   `json:"unit,omitempty"`
	PID    int      `json:"pid,omitempty" js:"min=1"`
	Proto  string   `json:"proto,omitempty" js:"enum=tcp|udp|all;default=all"`
	States []string `json:"states,omitempty"`
	Limit  int      `json:"limit,omitempty" js:"min=1;max=2000;default=200"`
}

type procTopModule struct{ env *moduleEnv }

var procTopDescriptor = types.Descriptor{
	Name:        "proc.top",
	Version:     1,
	Schema:      schemagen.Generate("proc.top", 1, procTopArgs{}),
	Scopes:      []string{"proc:read"},
	Idempotency: types.IdemPure,
	CheckMode:   true,
	TimeoutS:    5,
	Mutating:    false,
}

func (m procTopModule) bind(e *moduleEnv)            { m.env = e }
func (m procTopModule) Descriptor() types.Descriptor { return procTopDescriptor }

func (m procTopModule) NormalizeArgs(args map[string]any) (map[string]any, error) {
	normalizeUnitArgs(args)
	return args, nil
}

func (m procTopModule) ProtectedTargets(args map[string]any) []protectedTarget {
	if _, ok := args["unit"]; ok {
		return unitTarget(args)
	}
	return nil
}

// Check returns the one-line observation with an empty diff: nothing a read-only
// module predicts can be applied (§3.9).
func (m procTopModule) Check(ctx context.Context, args map[string]any) (types.Diff, error) {
	env := environment(m.env)
	var a procTopArgs
	if err := decodeArgs(args, &a); err != nil {
		return types.Diff{}, err
	}
	sortKey := procSortKey(a.Sort)
	rows, matched, err := procTopRows(env, sortKey, a.Unit, procTopN(env.cfg, a.N))
	if err != nil {
		return types.Diff{}, err
	}
	return types.Diff{Empty: true, Summary: procTopSummary(rows, matched, sortKey, a.Unit)}, nil
}

// Apply is a read (§3.4 `pure`): Changed=false, the sample in Output.
func (m procTopModule) Apply(ctx context.Context, args map[string]any) (types.Result, error) {
	start := time.Now()
	env := environment(m.env)
	var a procTopArgs
	if err := decodeArgs(args, &a); err != nil {
		return types.Result{}, err
	}
	sortKey := procSortKey(a.Sort)
	n := procTopN(env.cfg, a.N)
	rows, matched, err := procTopRows(env, sortKey, a.Unit, n)
	if err != nil {
		return types.Result{}, err
	}
	return types.Result{
		Changed:    false,
		Output:     procTopOutput(rows, matched, sortKey, a.Unit, procRoot(env.cfg), n),
		Rollback:   &types.RollbackHint{Supported: false, Args: map[string]any{}},
		DurationMS: serviceDuration(start),
	}, nil
}

// Verify is a recheck: the same ranking, read again.
func (m procTopModule) Verify(ctx context.Context, args map[string]any) (types.VerifyResult, error) {
	env := environment(m.env)
	var a procTopArgs
	if err := decodeArgs(args, &a); err != nil {
		return types.VerifyResult{}, err
	}
	sortKey := procSortKey(a.Sort)
	rows, matched, err := procTopRows(env, sortKey, a.Unit, procTopN(env.cfg, a.N))
	if err != nil {
		return types.VerifyResult{}, err
	}
	detail := procTopOutput(rows, matched, sortKey, a.Unit, procRoot(env.cfg), procTopN(env.cfg, a.N))
	detail["recheck"] = true
	return types.VerifyResult{OK: true, Method: "recheck", Detail: detail}, nil
}

// procSortKey applies the `sort` default.
func procSortKey(key string) string {
	switch key {
	case "rss", "io":
		return key
	}
	return "cpu"
}

// procSocketStates maps the kernel's hex `st` column of /proc/net/{tcp,tcp6,udp,
// udp6} onto state names. UDP uses the same enum: 07 is an unconnected socket and
// 01 an established one.
var procSocketStates = map[string]string{
	"01": "established",
	"02": "syn_sent",
	"03": "syn_recv",
	"04": "fin_wait1",
	"05": "fin_wait2",
	"06": "time_wait",
	"07": "close",
	"08": "close_wait",
	"09": "last_ack",
	"0a": "listen",
	"0b": "closing",
	"0c": "new_syn_recv",
}

// procSocketStateName names one socket state; an unknown hex code is reported
// verbatim rather than guessed at.
func procSocketStateName(proto, hexState string) string {
	h := strings.ToLower(strings.TrimSpace(hexState))
	name, ok := procSocketStates[h]
	if !ok {
		return "state_" + h
	}
	if proto == "udp" && h == "07" {
		return "unconnected"
	}
	return name
}

// procSocketEntry is one row of a socket table.
type procSocketEntry struct {
	Proto  string
	State  string
	Hex    string
	Local  string
	Remote string
	UID    uint32
	Inode  uint64
}

// procParseNetTable parses one /proc/net/{tcp,tcp6,udp,udp6} body. The header line
// is skipped; a line with fewer than the ten columns the format defines is
// skipped rather than half-read.
func procParseNetTable(proto, data string) []procSocketEntry {
	var out []procSocketEntry
	for i, line := range strings.Split(data, "\n") {
		if i == 0 || strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 10 {
			continue
		}
		inode, err := strconv.ParseUint(fields[9], 10, 64)
		if err != nil {
			continue
		}
		uid, _ := strconv.ParseUint(fields[7], 10, 32)
		out = append(out, procSocketEntry{
			Proto:  proto,
			State:  procSocketStateName(proto, fields[3]),
			Hex:    strings.ToLower(fields[3]),
			Local:  fields[1],
			Remote: fields[2],
			UID:    uint32(uid),
			Inode:  inode,
		})
	}
	return out
}

// procSocketInode extracts the inode from an fd link target ("socket:[12345]").
func procSocketInode(target string) (uint64, bool) {
	const prefix = "socket:["
	if !strings.HasPrefix(target, prefix) || !strings.HasSuffix(target, "]") {
		return 0, false
	}
	inode, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(target, prefix), "]"), 10, 64)
	if err != nil {
		return 0, false
	}
	return inode, true
}

// procSocketOwners attributes socket inodes to pids by scanning <root>/<pid>/fd,
// bounded by proc.fd_scan_max. A pid whose fd table cannot be read (a different
// uid, a vanished pid) is counted and never fails the call (§3.9).
func procSocketOwners(root string, max int) (map[uint64][]int, int, int) {
	owners := map[uint64][]int{}
	unreadable, scanned := 0, 0
	for _, pid := range procPIDs(root) {
		dir := filepath.Join(root, strconv.Itoa(pid), "fd")
		entries, err := os.ReadDir(dir)
		if err != nil {
			unreadable++
			continue
		}
		for _, e := range entries {
			if max > 0 && scanned >= max {
				return owners, unreadable, scanned
			}
			scanned++
			target, err := os.Readlink(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			inode, ok := procSocketInode(target)
			if !ok {
				continue
			}
			owners[inode] = append(owners[inode], pid)
		}
	}
	return owners, unreadable, scanned
}

// procConnection is one attributed connection row.
type procConnection struct {
	Proto  string
	State  string
	Local  string
	Remote string
	Inode  uint64
	PID    int
	Comm   string
	Unit   string
}

// procConnectionArgs are the resolved filters of one proc.connections read.
type procConnectionsFilter struct {
	Unit   string
	PID    int
	Proto  string
	States map[string]bool
	Limit  int
}

// procConnections reads the socket tables, attributes inodes to pids and applies
// the filters. The returned counts are per state name over the filtered set, plus
// fd_unreadable; sockets that belong to no readable pid are still counted when no
// unit/pid filter is set (they exist on the host and hiding them would be a lie).
func procConnections(env *moduleEnv, a procConnectionsFilter) ([]procConnection, map[string]int, int, int, int, error) {
	root := procRoot(env.cfg)
	if _, err := os.Stat(root); err != nil {
		return nil, nil, 0, 0, 0, fmt.Errorf("%w: proc root %s is not readable: %v", types.ErrPermanent, root, err)
	}
	protos := []string{"tcp", "tcp6", "udp", "udp6"}
	switch a.Proto {
	case "tcp":
		protos = []string{"tcp", "tcp6"}
	case "udp":
		protos = []string{"udp", "udp6"}
	}
	var entries []procSocketEntry
	for _, p := range protos {
		data, err := os.ReadFile(filepath.Join(root, "net", p))
		if err != nil {
			// A missing table (tcp6 or udp6 without IPv6) is not a failure.
			continue
		}
		entries = append(entries, procParseNetTable(p, string(data))...)
	}
	owners, unreadable, scanned := procSocketOwners(root, procFDScanMax(env.cfg))

	want := normalizeUnit(a.Unit)
	comms := map[int]string{}
	rows := make([]procConnection, 0, len(entries))
	counts := map[string]int{"fd_unreadable": unreadable}
	for _, e := range entries {
		if len(a.States) > 0 && !a.States[e.State] && !a.States[e.Hex] {
			continue
		}
		pids := owners[e.Inode]
		if a.PID > 0 {
			filtered := pids[:0]
			for _, p := range pids {
				if p == a.PID {
					filtered = append(filtered, p)
				}
			}
			pids = filtered
		}
		if want != "" {
			filtered := pids[:0]
			for _, p := range pids {
				if normalizeUnit(procUnitCached(root, p, comms)) == want {
					filtered = append(filtered, p)
				}
			}
			pids = filtered
			if len(pids) == 0 {
				continue
			}
		}
		pid := 0
		unit := ""
		if len(pids) > 0 {
			sort.Ints(pids)
			pid = pids[0]
			unit = normalizeUnit(procUnitCached(root, pid, comms))
		}
		counts[e.State]++
		rows = append(rows, procConnection{
			Proto: e.Proto, State: e.State, Local: e.Local, Remote: e.Remote,
			Inode: e.Inode, PID: pid, Comm: comms[pid], Unit: unit,
		})
	}
	limit := a.Limit
	if limit <= 0 {
		limit = procLimitDefault
	}
	if limit > len(rows) {
		limit = len(rows)
	}
	return rows[:limit], counts, len(rows), unreadable, scanned, nil
}

// procUnitCached reads a pid's unit once per call.
func procUnitCached(root string, pid int, comms map[int]string) string {
	if _, ok := comms[pid]; !ok {
		comms[pid] = procUnit(root, pid)
	}
	return comms[pid]
}

// procConnectionsOutput is the §3.9 Output: the counts, the unit when one was
// named, and the attributed rows.
func procConnectionsOutput(rows []procConnection, counts map[string]int, total, unreadable, scanned int, unit string) map[string]any {
	conns := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		conns = append(conns, map[string]any{
			"proto":  r.Proto,
			"state":  r.State,
			"local":  r.Local,
			"remote": r.Remote,
			"inode":  r.Inode,
			"pid":    r.PID,
			"comm":   r.Comm,
			"unit":   r.Unit,
		})
	}
	return map[string]any{
		"counts":        counts,
		"total":         total,
		"connections":   conns,
		"returned":      len(conns),
		"unit":          normalizeUnit(unit),
		"fd_unreadable": unreadable,
		"fds_scanned":   scanned,
	}
}

// procConnectionsSummary is the §3.9 one-line observation
// ("established=412 unit=payment-worker.service").
func procConnectionsSummary(counts map[string]int, unit string) string {
	states := make([]string, 0, len(counts))
	for name, n := range counts {
		if name == "fd_unreadable" || n == 0 {
			continue
		}
		states = append(states, name)
	}
	sort.Strings(states)
	parts := make([]string, 0, len(states)+1)
	for _, name := range states {
		parts = append(parts, fmt.Sprintf("%s=%d", name, counts[name]))
	}
	if u := normalizeUnit(unit); u != "" {
		parts = append(parts, "unit="+u)
	}
	if n := counts["fd_unreadable"]; n > 0 {
		parts = append(parts, fmt.Sprintf("fd_unreadable=%d", n))
	}
	if len(parts) == 0 {
		return "no sockets matched"
	}
	return strings.Join(parts, " ")
}

// procConnectionArgsOf resolves args into the filter set, validating the proto and
// state names the schema could not express.
func procConnectionsFilterOf(args map[string]any) (procConnectionsFilter, error) {
	var a procConnectionsArgs
	if err := decodeArgs(args, &a); err != nil {
		return procConnectionsFilter{}, err
	}
	proto := a.Proto
	switch proto {
	case "", "all":
		proto = "all"
	case "tcp", "udp":
	default:
		return procConnectionsFilter{}, fmt.Errorf("%w: proto %q is not tcp, udp or all", types.ErrPermanent, a.Proto)
	}
	states := map[string]bool{}
	for _, s := range a.States {
		name := strings.ToLower(strings.TrimSpace(s))
		if name == "" {
			continue
		}
		states[name] = true
	}
	limit := a.Limit
	if limit <= 0 {
		limit = procLimitDefault
	}
	if limit > procLimitMax {
		limit = procLimitMax
	}
	return procConnectionsFilter{
		Unit:   normalizeUnit(a.Unit),
		PID:    a.PID,
		Proto:  proto,
		States: states,
		Limit:  limit,
	}, nil
}

type procConnectionsModule struct{ env *moduleEnv }

var procConnectionsDescriptor = types.Descriptor{
	Name:        "proc.connections",
	Version:     1,
	Schema:      schemagen.Generate("proc.connections", 1, procConnectionsArgs{}),
	Scopes:      []string{"proc:read"},
	Idempotency: types.IdemPure,
	CheckMode:   true,
	TimeoutS:    10,
	Mutating:    false,
}

func (m procConnectionsModule) bind(e *moduleEnv)            { m.env = e }
func (m procConnectionsModule) Descriptor() types.Descriptor { return procConnectionsDescriptor }

func (m procConnectionsModule) NormalizeArgs(args map[string]any) (map[string]any, error) {
	normalizeUnitArgs(args)
	return args, nil
}

func (m procConnectionsModule) ProtectedTargets(args map[string]any) []protectedTarget {
	if _, ok := args["unit"]; ok {
		return unitTarget(args)
	}
	return nil
}

// Check returns Diff{Empty:true} with the §3.9 one-line observation.
func (m procConnectionsModule) Check(ctx context.Context, args map[string]any) (types.Diff, error) {
	env := environment(m.env)
	a, err := procConnectionsFilterOf(args)
	if err != nil {
		return types.Diff{}, err
	}
	_, counts, _, _, _, err := procConnections(env, a)
	if err != nil {
		return types.Diff{}, err
	}
	return types.Diff{Empty: true, Summary: procConnectionsSummary(counts, a.Unit)}, nil
}

// Apply is a read (§3.4 `pure`).
func (m procConnectionsModule) Apply(ctx context.Context, args map[string]any) (types.Result, error) {
	start := time.Now()
	env := environment(m.env)
	a, err := procConnectionsFilterOf(args)
	if err != nil {
		return types.Result{}, err
	}
	rows, counts, total, unreadable, scanned, err := procConnections(env, a)
	if err != nil {
		return types.Result{}, err
	}
	return types.Result{
		Changed:    false,
		Output:     procConnectionsOutput(rows, counts, total, unreadable, scanned, a.Unit),
		Rollback:   &types.RollbackHint{Supported: false, Args: map[string]any{}},
		DurationMS: serviceDuration(start),
	}, nil
}

// Verify is a recheck: the same read, again. A unit that cannot be resolved is
// reported with an empty attribution, not a failure (§3.9).
func (m procConnectionsModule) Verify(ctx context.Context, args map[string]any) (types.VerifyResult, error) {
	env := environment(m.env)
	a, err := procConnectionsFilterOf(args)
	if err != nil {
		return types.VerifyResult{}, err
	}
	rows, counts, total, unreadable, scanned, err := procConnections(env, a)
	if err != nil {
		return types.VerifyResult{}, err
	}
	detail := procConnectionsOutput(rows, counts, total, unreadable, scanned, a.Unit)
	detail["recheck"] = true
	return types.VerifyResult{OK: true, Method: "recheck", Detail: detail}, nil
}
