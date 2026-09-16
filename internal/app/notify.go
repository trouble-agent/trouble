package app

import (
	"net"
	"os"
	"strings"
	"time"
)

// notifier is the systemd notification socket (SPEC-12 §3.5): sd_notify is the
// only liveness channel that leaves the process without a second daemon, so the
// daemon pings it from a goroutine that never touches the ledger writer — a
// wedged writer must not be able to silence the watchdog.
//
// The protocol is a datagram per message, `KEY=VALUE` lines, to $NOTIFY_SOCKET
// (abstract sockets start with '@'). If the socket is absent — a foreground run,
// a container without systemd, a test — every call is a silent no-op: the daemon
// must not depend on being supervised.
type notifier struct {
	addr string
	conn *net.UnixConn
}

func newNotifier(env func(string) string) *notifier {
	path := env("NOTIFY_SOCKET")
	if path == "" {
		return &notifier{}
	}
	network := "unixgram"
	if strings.HasPrefix(path, "@") {
		path = "\x00" + path[1:]
		network = "unixgram"
	}
	conn, err := net.DialUnix(network, nil, &net.UnixAddr{Name: path, Net: network})
	if err != nil {
		return &notifier{}
	}
	return &notifier{addr: path, conn: conn}
}

func (n *notifier) send(msgs ...string) {
	if n == nil || n.conn == nil {
		return
	}
	_, _ = n.conn.Write([]byte(strings.Join(msgs, "\n")))
}

// ready reports READY=1 after the bind preflight and the first ledger write.
func (n *notifier) ready(status string) { n.send("READY=1", "STATUS="+status) }

// watchdog reports WATCHDOG=1; systemd aborts the unit when it stops arriving.
func (n *notifier) watchdog(status string) { n.send("WATCHDOG=1", "STATUS="+status) }

// stopping reports STOPPING=1 on drain entry.
func (n *notifier) stopping(status string) { n.send("STOPPING=1", "STATUS="+status) }

// status updates STATUS= only.
func (n *notifier) status(status string) { n.send("STATUS=" + status) }

// watchdogLoop pings at watchdog_sec/2 (SPEC-12 §3.5). It runs whether or not a
// socket exists: the loop is also what would record TROUBLE-LIFECYCLE-007 if a
// ping could not be delivered inside the window, and that check must not depend
// on being under systemd.
func (n *notifier) watchdogLoop(stop <-chan struct{}, every time.Duration, status func() string) {
	if every <= 0 {
		every = 30 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			n.watchdog(status())
		}
	}
}

// hostnameOr reads a stable-ish host identity for origin stamping.
func hostnameOr(def string) string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return def
	}
	return h
}
