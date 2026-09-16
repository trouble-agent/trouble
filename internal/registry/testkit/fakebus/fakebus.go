// Package fakebus is the in-process D-Bus server the service.* module tests run
// against (SPEC-06 §2.4's FakeBus helper, §3.9, §3.10).
//
// It speaks org.freedesktop.systemd1 on a unix socket under the test's own temp
// dir, so no test touches the real systemd, needs root, or reaches the network.
// Every method it answers is recorded, so a test can assert exactly how many
// times a verb was called — the "exactly one execution recorded" half of the
// `once` idempotency contract (SPEC-06 §3.4).
//
// Deliberately not implemented: anything that would mutate the host (StartUnit,
// StopUnit, unit-file writes, EnableUnitFiles), the job queue, signals and unix
// FD passing. A verb this fake does not know is answered with
// org.freedesktop.DBus.Error.UnknownMethod, never guessed at.
package fakebus

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// The identity this fake answers to (SPEC-06 §3.9).
const (
	systemdName         = "org.freedesktop.systemd1"
	systemdManagerPath  = dbus.ObjectPath("/org/freedesktop/systemd1")
	systemdManagerIface = "org.freedesktop.systemd1.Manager"
	systemdUnitIface    = "org.freedesktop.systemd1.Unit"
	systemdServiceIface = "org.freedesktop.systemd1.Service"
	propertiesIface     = "org.freedesktop.DBus.Properties"

	// dbusPolicyDenial is what polkit's refusal looks like on the wire when the
	// 49-trouble.rules artifact is absent (§3.10, edge case 5).
	dbusPolicyDenial = "org.freedesktop.DBus.Error.AccessDenied"

	// busGUID is the (fixed) identity the client keeps after the auth exchange.
	busGUID = "6c3f1d0a5b7e4498a1c2d3e4f5a6b7c8"

	// listUnitsSig is ListUnits' documented reply signature, a(ssssssouso). It is
	// declared explicitly because a struct nested in an array cannot be derived
	// from the Go value (SPEC-03 §3.3 uses the same shape against the real bus).
	listUnitsSig = "a(ssssssouso)"
)

// Start returns a `unix:` D-Bus address of an in-process server speaking
// org.freedesktop.systemd1 for the named units (SPEC-06 §2.4 FakeBus). The socket
// lives under t.TempDir() and is closed with the test.
func Start(t *testing.T, units ...string) string {
	t.Helper()
	return StartBus(t, units...).Address()
}

// Bus is the fake manager. StartBus is the entry point when a test needs the
// knobs and the call counter; Start is the SPEC-06 §2.4 shape when it only needs
// the address.
type Bus struct {
	t    *testing.T
	path string
	ln   net.Listener

	wmu sync.Mutex

	mu    sync.Mutex
	calls map[string]int
	log   []Call
	units map[string]*unitState
	deny  map[string]bool
	hold  map[string]bool

	connMu sync.Mutex
	conns  []net.Conn
	closed bool

	uniq int
}

// Call is one recorded method call.
type Call struct {
	Interface string
	Member    string
	Arg       string
}

// unitState is the state this fake reports for one unit.
type unitState struct {
	Name          string
	Description   string
	LoadState     string
	ActiveState   string
	SubState      string
	MainPID       uint32
	NRestarts     uint32
	ExecMainStart uint64
}

// StartBus starts the fake manager for the named units and returns it.
func StartBus(t *testing.T, units ...string) *Bus {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "bus")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("fakebus: listen %s: %v", sock, err)
	}
	b := &Bus{
		t:     t,
		path:  sock,
		ln:    ln,
		calls: map[string]int{},
		units: map[string]*unitState{},
		deny:  map[string]bool{},
		hold:  map[string]bool{},
	}
	if len(units) == 0 {
		units = []string{"payment-worker.service"}
	}
	for i, u := range units {
		b.units[u] = &unitState{
			Name:          u,
			Description:   "fake unit " + u,
			LoadState:     "loaded",
			ActiveState:   "active",
			SubState:      "running",
			MainPID:       uint32(4000 + 10*i),
			NRestarts:     1,
			ExecMainStart: uint64(time.Now().Add(-time.Hour).UnixMicro()),
		}
	}
	go b.accept()
	t.Cleanup(b.Close)
	return b
}

// Address is the `unix:` D-Bus address of this bus.
func (b *Bus) Address() string { return "unix:path=" + b.path }

// Close shuts the listener and every served connection down. It is idempotent.
func (b *Bus) Close() {
	b.connMu.Lock()
	if b.closed {
		b.connMu.Unlock()
		return
	}
	b.closed = true
	conns := b.conns
	b.conns = nil
	b.connMu.Unlock()
	_ = b.ln.Close()
	for _, c := range conns {
		_ = c.Close()
	}
}

// UnitPath is the object path systemd mints for a unit: every byte outside
// [A-Za-z0-9] becomes `_` + two lowercase hex digits, `_` included so the
// transform is round-trippable (`-` → `_2d`, SPEC-06 §3.9).
func (b *Bus) UnitPath(unit string) string {
	return "/org/freedesktop/systemd1/unit/" + escapeUnit(unit)
}

// escapeUnit is the object-path escape of SPEC-06 §3.9.
func escapeUnit(name string) string {
	const hexdigits = "0123456789abcdef"
	var sb strings.Builder
	sb.Grow(len(name) * 2)
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			sb.WriteByte(c)
			continue
		}
		sb.WriteByte('_')
		sb.WriteByte(hexdigits[c>>4])
		sb.WriteByte(hexdigits[c&0x0f])
	}
	return sb.String()
}

// Calls returns how many times a member (GetUnit, ReloadUnit, RestartUnit,
// Subscribe, ListUnits, GetAll, …) was called on this bus.
func (b *Bus) Calls(member string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls[member]
}

// Log returns the recorded calls in order.
func (b *Bus) Log() []Call {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Call, len(b.log))
	copy(out, b.log)
	return out
}

// Reset forgets the recorded calls (not the unit state): it lets one test assert
// the call counts of a single phase.
func (b *Bus) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = map[string]int{}
	b.log = nil
}

// SetUnit drives a unit's reported state.
func (b *Bus) SetUnit(unit, activeState, subState string, mainPID uint32) {
	b.mu.Lock()
	defer b.mu.Unlock()
	u, ok := b.units[unit]
	if !ok {
		u = &unitState{Name: unit, LoadState: "loaded"}
		b.units[unit] = u
	}
	u.ActiveState = activeState
	u.SubState = subState
	u.MainPID = mainPID
}

// Unit reports a unit's current state (ok=false when the fake does not know it).
func (b *Bus) Unit(unit string) (activeState, subState string, mainPID uint32, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	u, ok := b.units[unit]
	if !ok {
		return "", "", 0, false
	}
	return u.ActiveState, u.SubState, u.MainPID, true
}

// RemoveUnit forgets a unit, so GetUnit answers NoSuchUnit (the "unit not
// found / not loadable" error case of SPEC-06 §3.9).
func (b *Bus) RemoveUnit(unit string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.units, unit)
}

// Deny makes a verb answer org.freedesktop.DBus.Error.AccessDenied — polkit's
// refusal, the distinct policy_refused class of SPEC-06 §3.10.
func (b *Bus) Deny(verb string, deny bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deny[verb] = deny
}

// HoldState makes a verb leave the unit's state untouched: the restart happened
// as far as the bus is concerned, but the observable state did not move, which is
// what makes a Verify probe have to report not-ok (SPEC-06 §3.9).
func (b *Bus) HoldState(verb string, hold bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.hold[verb] = hold
}

func (b *Bus) record(iface, member, arg string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls[member]++
	b.log = append(b.log, Call{Interface: iface, Member: member, Arg: arg})
}

func (b *Bus) accept() {
	for {
		c, err := b.ln.Accept()
		if err != nil {
			return
		}
		b.connMu.Lock()
		if b.closed {
			b.connMu.Unlock()
			_ = c.Close()
			return
		}
		b.conns = append(b.conns, c)
		b.connMu.Unlock()
		go b.serve(c)
	}
}

func (b *Bus) serve(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	if err := serverAuth(br, c); err != nil {
		return
	}
	for {
		msg, err := dbus.DecodeMessage(br)
		if err != nil {
			return
		}
		b.handle(c, msg)
	}
}

// serverAuth runs the server half of the D-Bus auth handshake the godbus client
// performs: NUL, AUTH → REJECTED <mechs>, AUTH EXTERNAL <hex uid> → OK <guid>,
// an optional NEGOTIATE_UNIX_FD (refused: this fake passes no fds) and BEGIN.
func serverAuth(br *bufio.Reader, w io.Writer) error {
	nul, err := br.ReadByte()
	if err != nil {
		return err
	}
	if nul != 0 {
		return fmt.Errorf("fakebus: auth did not open with a nul byte (%#x)", nul)
	}
	if _, err := readLine(br); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "REJECTED EXTERNAL\r\n"); err != nil {
		return err
	}
	line, err := readLine(br)
	if err != nil {
		return err
	}
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != "AUTH" || fields[1] != "EXTERNAL" {
		return fmt.Errorf("fakebus: unexpected auth command %q", line)
	}
	if len(fields) == 2 {
		// The client asked for the uid to be challenged: DATA <hex uid>.
		if _, err := io.WriteString(w, "DATA\r\n"); err != nil {
			return err
		}
		data, err := readLine(br)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(data, "DATA") {
			return fmt.Errorf("fakebus: expected DATA, got %q", data)
		}
	}
	if _, err := io.WriteString(w, "OK "+busGUID+"\r\n"); err != nil {
		return err
	}
	line, err = readLine(br)
	if err != nil {
		return err
	}
	if strings.HasPrefix(line, "NEGOTIATE_UNIX_FD") {
		if _, err := io.WriteString(w, "ERROR unix fd passing is not supported\r\n"); err != nil {
			return err
		}
		line, err = readLine(br)
		if err != nil {
			return err
		}
	}
	if !strings.HasPrefix(line, "BEGIN") {
		return fmt.Errorf("fakebus: expected BEGIN, got %q", line)
	}
	return nil
}

func readLine(br *bufio.Reader) (string, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (b *Bus) handle(conn net.Conn, msg *dbus.Message) {
	if msg.Type != dbus.TypeMethodCall {
		return
	}
	iface := headerString(msg, dbus.FieldInterface)
	member := headerString(msg, dbus.FieldMember)
	path := headerPath(msg, dbus.FieldPath)

	// The bus interfaces are matched exactly, not by prefix:
	// org.freedesktop.DBus.Properties shares the org.freedesktop.DBus. prefix and
	// is a different object model entirely.
	switch iface {
	case propertiesIface:
		b.handleProperties(conn, msg, member, path)
	case systemdManagerIface:
		b.handleManager(conn, msg, member, path)
	case "org.freedesktop.DBus", "org.freedesktop.DBus.Peer",
		"org.freedesktop.DBus.Introspectable", "org.freedesktop.DBus.Monitoring":
		b.handleBus(conn, msg, member)
	default:
		b.record(iface, member, string(path))
		b.replyError(conn, msg, "org.freedesktop.DBus.Error.UnknownInterface",
			fmt.Sprintf("interface %q is not implemented by fakebus", iface))
	}
}

func (b *Bus) handleBus(conn net.Conn, msg *dbus.Message, member string) {
	b.record("org.freedesktop.DBus", member, "")
	switch member {
	case "Hello":
		b.reply(conn, msg, "", ":1.0")
	case "AddMatch", "RemoveMatch", "Ping", "GetId":
		b.reply(conn, msg, "", "fakebus")
	case "GetNameOwner":
		b.reply(conn, msg, "", ":1.0")
	case "NameHasOwner":
		b.reply(conn, msg, "", true)
	case "RequestName":
		b.reply(conn, msg, "", uint32(1))
	case "Introspect":
		b.reply(conn, msg, "", "<node/>")
	default:
		b.replyError(conn, msg, "org.freedesktop.DBus.Error.UnknownMethod",
			fmt.Sprintf("org.freedesktop.DBus.%s is not implemented by fakebus", member))
	}
}

func (b *Bus) handleManager(conn net.Conn, msg *dbus.Message, member string, path dbus.ObjectPath) {
	if path != systemdManagerPath {
		b.record(systemdManagerIface, member, string(path))
		b.replyError(conn, msg, "org.freedesktop.DBus.Error.UnknownObject",
			fmt.Sprintf("%s is not an object of fakebus", path))
		return
	}
	switch member {
	case "GetUnit":
		unit := argString(msg, 0)
		b.record(systemdManagerIface, member, unit)
		if _, ok := b.unit(unit); !ok {
			b.replyError(conn, msg, "org.freedesktop.systemd1.NoSuchUnit",
				fmt.Sprintf("Unit %s not loaded.", unit))
			return
		}
		b.reply(conn, msg, "", dbus.ObjectPath(b.UnitPath(unit)))
	case "ReloadUnit", "RestartUnit":
		unit := argString(msg, 0)
		mode := argString(msg, 1)
		b.record(systemdManagerIface, member, unit+" "+mode)
		if b.denied(member) {
			b.replyError(conn, msg, dbusPolicyDenial, "Access denied")
			return
		}
		if _, ok := b.unit(unit); !ok {
			b.replyError(conn, msg, "org.freedesktop.systemd1.NoSuchUnit",
				fmt.Sprintf("Unit %s not found.", unit))
			return
		}
		b.applyVerb(member, unit)
		b.uniq++
		b.reply(conn, msg, "", dbus.ObjectPath(fmt.Sprintf("/org/freedesktop/systemd1/job/%d", b.uniq)))
	case "Subscribe":
		b.record(systemdManagerIface, member, "")
		b.reply(conn, msg, "")
	case "ListUnits":
		b.record(systemdManagerIface, member, "")
		rows := b.listUnits()
		b.reply(conn, msg, listUnitsSig, rows)
	case "ListUnitFiles", "GetUnitFileState":
		b.record(systemdManagerIface, member, argString(msg, 0))
		b.replyError(conn, msg, "org.freedesktop.DBus.Error.UnknownMethod",
			fmt.Sprintf("%s is not implemented by fakebus (trouble never writes unit files, SPEC-06 §3.10)", member))
	default:
		b.record(systemdManagerIface, member, "")
		b.replyError(conn, msg, "org.freedesktop.DBus.Error.UnknownMethod",
			fmt.Sprintf("%s.%s is not implemented by fakebus", systemdManagerIface, member))
	}
}

func (b *Bus) handleProperties(conn net.Conn, msg *dbus.Message, member string, path dbus.ObjectPath) {
	unit := b.unitOfPath(path)
	// Record the member and the interface it asked for: a test can then assert the
	// client asked the interface that actually carries the property it wants
	// (MainPID lives on org.freedesktop.systemd1.Service, not on .Unit).
	switch member {
	case "GetAll":
		want := argString(msg, 0)
		b.record(propertiesIface, member, want)
		if unit == "" {
			b.replyError(conn, msg, "org.freedesktop.DBus.Error.UnknownObject",
				fmt.Sprintf("%s is not a known unit object", path))
			return
		}
		props, ok := b.props(unit, want)
		if !ok {
			b.replyError(conn, msg, "org.freedesktop.DBus.Error.UnknownInterface",
				fmt.Sprintf("%s has no interface %q", path, want))
			return
		}
		b.reply(conn, msg, "a{sv}", props)
	case "Get":
		want := argString(msg, 0)
		name := argString(msg, 1)
		b.record(propertiesIface, member, want+"."+name)
		if unit == "" {
			b.replyError(conn, msg, "org.freedesktop.DBus.Error.UnknownObject",
				fmt.Sprintf("%s is not a known unit object", path))
			return
		}
		props, ok := b.props(unit, want)
		if !ok {
			b.replyError(conn, msg, "org.freedesktop.DBus.Error.UnknownInterface",
				fmt.Sprintf("%s has no interface %q", path, want))
			return
		}
		v, ok := props[name]
		if !ok {
			b.replyError(conn, msg, "org.freedesktop.DBus.Error.UnknownProperty",
				fmt.Sprintf("%s has no property %s", want, name))
			return
		}
		b.reply(conn, msg, "v", v)
	default:
		b.record(propertiesIface, member, string(path))
		b.replyError(conn, msg, "org.freedesktop.DBus.Error.UnknownMethod",
			fmt.Sprintf("%s.%s is not implemented by fakebus", propertiesIface, member))
	}
}

// unitOfPath maps an object path back onto a unit name ("" when unknown).
func (b *Bus) unitOfPath(path dbus.ObjectPath) string {
	const prefix = "/org/freedesktop/systemd1/unit/"
	p := strings.TrimPrefix(string(path), prefix)
	if p == string(path) {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for name := range b.units {
		if escapeUnit(name) == p {
			return name
		}
	}
	return ""
}

// props is the interface's property set. A unit that is not a service has no
// org.freedesktop.systemd1.Service interface; fakebus answers those calls the way
// systemd does (UnknownInterface) rather than inventing MainPID = 0.
func (b *Bus) props(unit, iface string) (map[string]dbus.Variant, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	u, ok := b.units[unit]
	if !ok {
		return nil, false
	}
	switch iface {
	case systemdUnitIface:
		return map[string]dbus.Variant{
			"Name":         dbus.MakeVariant(u.Name),
			"Description":  dbus.MakeVariant(u.Description),
			"LoadState":    dbus.MakeVariant(u.LoadState),
			"ActiveState":  dbus.MakeVariant(u.ActiveState),
			"SubState":     dbus.MakeVariant(u.SubState),
			"FragmentPath": dbus.MakeVariant("/etc/systemd/system/" + u.Name),
		}, true
	case systemdServiceIface:
		return map[string]dbus.Variant{
			"MainPID":                dbus.MakeVariant(u.MainPID),
			"NRestarts":              dbus.MakeVariant(u.NRestarts),
			"ExecMainStartTimestamp": dbus.MakeVariant(u.ExecMainStart),
		}, true
	}
	return nil, false
}

// unitRow is one ListUnits row, a(ssssssouso).
type unitRow struct {
	Name        string
	Description string
	LoadState   string
	ActiveState string
	SubState    string
	Followed    string
	Path        dbus.ObjectPath
	JobID       uint32
	JobType     string
	JobPath     dbus.ObjectPath
}

func (b *Bus) listUnits() []unitRow {
	b.mu.Lock()
	defer b.mu.Unlock()
	names := make([]string, 0, len(b.units))
	for n := range b.units {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]unitRow, 0, len(names))
	for _, n := range names {
		u := b.units[n]
		out = append(out, unitRow{
			Name:        u.Name,
			Description: u.Description,
			LoadState:   u.LoadState,
			ActiveState: u.ActiveState,
			SubState:    u.SubState,
			Path:        dbus.ObjectPath(b.UnitPath(u.Name)),
			// A unit with no job carries the root path, not the empty string: the
			// empty string is not a valid object path on the wire.
			JobPath: "/",
		})
	}
	return out
}

func (b *Bus) unit(name string) (*unitState, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	u, ok := b.units[name]
	return u, ok
}

func (b *Bus) denied(member string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.deny[member]
}

// applyVerb moves a unit's state the way the real manager would: a reload leaves
// the process alone, a restart mints a new MainPID.
func (b *Bus) applyVerb(member, unit string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	u, ok := b.units[unit]
	if !ok {
		return
	}
	if b.hold[member] {
		return
	}
	switch member {
	case "ReloadUnit":
		u.ActiveState = "active"
		u.SubState = "running"
	case "RestartUnit":
		u.ActiveState = "active"
		u.SubState = "running"
		u.MainPID++
		u.NRestarts++
		u.ExecMainStart = uint64(time.Now().UnixMicro())
	}
}

func (b *Bus) reply(conn net.Conn, msg *dbus.Message, sig string, values ...any) {
	reply := &dbus.Message{
		Type:    dbus.TypeMethodReply,
		Headers: map[dbus.HeaderField]dbus.Variant{dbus.FieldReplySerial: dbus.MakeVariant(msg.Serial())},
		Body:    values,
	}
	if len(values) > 0 {
		s := dbus.SignatureOf(values...)
		if sig != "" {
			parsed, err := dbus.ParseSignature(sig)
			if err != nil {
				b.t.Errorf("fakebus: bad signature %q: %v", sig, err)
				return
			}
			s = parsed
		}
		reply.Headers[dbus.FieldSignature] = dbus.MakeVariant(s)
	}
	b.send(conn, reply)
}

func (b *Bus) replyError(conn net.Conn, msg *dbus.Message, name, text string) {
	reply := &dbus.Message{
		Type: dbus.TypeError,
		Headers: map[dbus.HeaderField]dbus.Variant{
			dbus.FieldReplySerial: dbus.MakeVariant(msg.Serial()),
			dbus.FieldErrorName:   dbus.MakeVariant(name),
			dbus.FieldSignature:   dbus.MakeVariant(dbus.SignatureOf(text)),
		},
		Body: []any{text},
	}
	b.send(conn, reply)
}

func (b *Bus) send(conn net.Conn, msg *dbus.Message) {
	b.wmu.Lock()
	defer b.wmu.Unlock()
	if err := msg.EncodeTo(conn, binary.LittleEndian); err != nil && !errors.Is(err, os.ErrClosed) {
		// A client that hung up mid-call is the test's business, not a failure of
		// the fake; only a genuine encoding error is worth reporting.
		if _, isNet := err.(*net.OpError); !isNet {
			b.t.Logf("fakebus: send %v: %v", msg.Type, err)
		}
	}
}

func headerString(msg *dbus.Message, f dbus.HeaderField) string {
	if v, ok := msg.Headers[f]; ok {
		if s, ok := v.Value().(string); ok {
			return s
		}
	}
	return ""
}

func headerPath(msg *dbus.Message, f dbus.HeaderField) dbus.ObjectPath {
	if v, ok := msg.Headers[f]; ok {
		switch p := v.Value().(type) {
		case dbus.ObjectPath:
			return p
		case string:
			return dbus.ObjectPath(p)
		}
	}
	return ""
}

func argString(msg *dbus.Message, i int) string {
	if i >= len(msg.Body) {
		return ""
	}
	switch v := msg.Body[i].(type) {
	case string:
		return v
	case dbus.ObjectPath:
		return string(v)
	case uint32:
		return strconv.FormatUint(uint64(v), 10)
	}
	return fmt.Sprint(msg.Body[i])
}
