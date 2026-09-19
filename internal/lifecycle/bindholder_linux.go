//go:build linux

// Linux diagnostics for the bind preflight (TROUBLE-LIFECYCLE-003, TRBL-042):
// when a documented port is already held, the refusal must name the holder —
// pid, name, start time — instead of surfacing as a bare `bind: address
// already in use` that reads like a defect in the code under test. The class
// this answers: a scratch daemon booted by a finished tick still owns the
// documented port, and the next tick's suite reds on the preflight.
//
// Everything here reads /proc only. No netlink, no external commands, no
// elevated privileges: the same tables `ss -tlnp` reads, walked in-process.

package lifecycle

import (
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// findBindHolder looks for the process whose listener table owns host:port.
// It walks /proc/net/tcp and /proc/net/tcp6 for a TCP_LISTEN entry on the
// address, maps the socket inode to an owning pid via /proc/<pid>/fd, and
// returns ok=false when nothing holds it (or nothing can be proven: a holder
// the error message cannot name is a holder the message must not invent).
func findBindHolder(host, port string) (bindHolder, bool) {
	ino := ""
	for _, table := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(table)
		if err != nil {
			continue
		}
		for i, line := range strings.Split(string(data), "\n") {
			if i == 0 { // header
				continue
			}
			fields := strings.Fields(line)
			// sl local_address rem_address st ... inode is field 10 (idx 9).
			if len(fields) < 10 || fields[3] != "0A" { // 0A = TCP_LISTEN
				continue
			}
			lAddr, lPort, ok := parseProcAddr(fields[1])
			if !ok || lPort != port || !sameListenerHost(host, lAddr) {
				continue
			}
			ino = fields[9]
			break
		}
		if ino != "" {
			break
		}
	}
	if ino == "" {
		return bindHolder{}, false
	}
	return holderBySocketInode(ino)
}

// parseProcAddr decodes a /proc/net address field "HHHHHHHH:PPPP" (IPv4,
// little-endian hex) or "32-hex-chars:PPPP" (IPv6, big-endian hex) into a
// normalised IP string and decimal port.
func parseProcAddr(field string) (ip, port string, ok bool) {
	hexAddr, hexPort, found := strings.Cut(field, ":")
	if !found {
		return "", "", false
	}
	p, err := strconv.ParseUint(hexPort, 16, 16)
	if err != nil {
		return "", "", false
	}
	raw, err := hex.DecodeString(hexAddr)
	if err != nil {
		return "", "", false
	}
	switch len(raw) {
	case 4: // IPv4 is little-endian in /proc/net/tcp
		for i, j := 0, 3; i < j; i, j = i+1, j-1 {
			raw[i], raw[j] = raw[j], raw[i]
		}
	case 16: // IPv6 is network order already
	default:
		return "", "", false
	}
	return net.IP(raw).String(), strconv.FormatUint(p, 10), true
}

// sameListenerHost reports whether the address the table reports belongs to
// the configured bind host. IP literals are compared directly (an IPv4
// socket reported on the tcp6 table as ::ffff:x.y.z.w still matches its v4
// literal); a hostname binds is resolved once — the common loopback names
// resolve from /etc/hosts without a network round trip.
func sameListenerHost(wantHost, gotIP string) bool {
	got := net.ParseIP(gotIP)
	if got == nil {
		return false
	}
	if want := net.ParseIP(wantHost); want != nil {
		return ipsEqual(want, got)
	}
	resolved, err := net.LookupHost(wantHost)
	if err != nil {
		return false
	}
	for _, r := range resolved {
		if want := net.ParseIP(r); want != nil && ipsEqual(want, got) {
			return true
		}
	}
	return false
}

func ipsEqual(a, b net.IP) bool {
	a4, b4 := a.To4(), b.To4()
	if a4 != nil && b4 != nil {
		return a4.Equal(b4)
	}
	return a.Equal(b)
}

// holderBySocketInode scans /proc for the pid holding socket:[<ino>].
func holderBySocketInode(ino string) (bindHolder, bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return bindHolder{}, false
	}
	want := "socket:[" + ino + "]"
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // not a pid directory
		}
		fds, err := os.ReadDir(filepath.Join("/proc", e.Name(), "fd"))
		if err != nil {
			continue // exited mid-scan, or not ours to read
		}
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join("/proc", e.Name(), "fd", fd.Name()))
			if err != nil || link != want {
				continue
			}
			return procIdentity(pid), true
		}
	}
	return bindHolder{}, false
}

// procIdentity reads name, cmdline and start time for one pid.
func procIdentity(pid int) bindHolder {
	h := bindHolder{PID: pid}
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid)); err == nil {
		h.Comm = strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
		parts := strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
		if len(parts) == 1 && parts[0] == "" {
			h.Cmdline = "[" + h.Comm + "]" // kernel thread: no cmdline
		} else {
			h.Cmdline = strings.Join(parts, " ")
		}
	}
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		h.Started = procStartTime(string(b))
	}
	return h
}

// procStartTime parses field 22 (starttime, in clock ticks since boot) from a
// /proc/<pid>/stat record and renders it as a wall-clock timestamp. The comm
// field may contain spaces and parentheses, so parsing starts after the last
// ')'. /proc timestamps count in USER_HZ jiffies, which proc(5) fixes at 100.
func procStartTime(stat string) string {
	const userHZ = 100
	s := stat
	if i := strings.LastIndex(s, ")"); i >= 0 {
		s = s[i+1:]
	}
	fields := strings.Fields(s)
	// fields[0] is state (field 3); starttime is field 22 → index 19.
	if len(fields) <= 19 {
		return ""
	}
	ticks, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return ""
	}
	btime := bootTimeEpoch()
	if btime == 0 {
		return fmt.Sprintf("%d ticks after boot", ticks)
	}
	return time.Unix(btime+ticks/userHZ, (ticks%userHZ)*int64(time.Second)/userHZ).
		Format("2006-01-02 15:04:05 MST")
}

// bootTimeEpoch reads the btime line of /proc/stat (epoch seconds of boot).
func bootTimeEpoch() int64 {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "btime "); ok {
			if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
				return n
			}
		}
	}
	return 0
}
