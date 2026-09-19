package lifecycle

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

func TestPreflightBindsHappy(t *testing.T) {
	cfg := defaults()
	probes, err := PreflightBinds(*cfg)
	if err != nil {
		t.Fatalf("PreflightBinds: %v", err)
	}
	if len(probes) != 2 {
		t.Fatalf("expected 2 probes, got %d", len(probes))
	}
	releaseBinds(probes)
}

func TestPreflightBindsDuplicate(t *testing.T) {
	cfg := defaults()
	cfg.Dashboard.Bind = "127.0.0.1:7643"
	cfg.Ingest.Bind = "127.0.0.1:7643"
	_, err := PreflightBinds(*cfg)
	if err == nil {
		t.Fatal("expected duplicate bind error")
	}
}

func TestPreflightBindsPortInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String()
	cfg := defaults()
	cfg.Ingest.Bind = addr
	_, err = PreflightBinds(*cfg)
	if err == nil {
		t.Fatal("expected bind-in-use error")
	}
}

// TestPreflightBindsNamesForeignHolder is the TRBL-042 regression: when a port
// is already held, the refusal must NAME the holder — pid, name, start time,
// the TROUBLE-SCRATCH-DAEMON condition — instead of a bare TROUBLE-LIFECYCLE-003
// that blames the code under test. The holder here is this test process's own
// listener, which /proc reports exactly as it would report a leftover scratch
// daemon.
func TestPreflightBindsNamesForeignHolder(t *testing.T) {
	// Reserve both ports and close them only at test end (the deferred close
	// inside a helper closure would free the port before the probe ran).
	dLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	iLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer dLn.Close()
	defer iLn.Close()
	cfg := defaults()
	cfg.Dashboard.Bind = dLn.Addr().String()
	cfg.Ingest.Bind = iLn.Addr().String() // the collision is deterministic
	_, err = PreflightBinds(*cfg)
	if err == nil {
		t.Fatal("expected bind-in-use error")
	}
	if !errors.Is(err, types.CodeLifecycle003) {
		t.Fatalf("error must stay wrapped as TROUBLE-LIFECYCLE-003, got: %v", err)
	}
	msg := err.Error()
	for _, want := range []string{
		"TROUBLE-SCRATCH-DAEMON",
		fmt.Sprintf("pid %d", os.Getpid()),
		"started ",
		"kill it by exact PID",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("holder diagnostic missing %q in:\n%s", want, msg)
		}
	}
	t.Logf("holder diagnostic:\n%s", msg)
}

// TestPreflightBindsForeignHolderNamedByCmdline proves the diagnostic works
// for holders OUTSIDE the test process: a child process (a plain python
// listener, the shape of any leftover scratch boot) owns the port, and the
// refusal must identify it — cmdline, pid, start time — so the next tick
// kills a process, not the code.
func TestPreflightBindsForeignHolderNamedByCmdline(t *testing.T) {
	// The dashboard bind must SUCCEED so the refusal reported is the ingest
	// one: reserve a fresh port, release it, and pin it (defaults would
	// collide with any leftover scratch daemon on 7644 — the very condition
	// this package is about).
	dLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dashAddr := dLn.Addr().String()
	dLn.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	// Reserve the port, then hand it to a child that outlives the
	// reservation: close ours, start the child, confirm it holds the port,
	// then run the probe.
	ln.Close()
	cmd := exec.Command("python3", "-c", fmt.Sprintf(
		"import socket,time; s=socket.socket(); s.bind(('127.0.0.1',%d)); s.listen(); time.sleep(120)", port))
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a scratch holder process: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()
	if err := waitForListener(fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second); err != nil {
		t.Skipf("child holder did not take the port: %v", err)
	}

	cfg := defaults()
	cfg.Dashboard.Bind = dashAddr
	cfg.Ingest.Bind = fmt.Sprintf("127.0.0.1:%d", port)
	_, err = PreflightBinds(*cfg)
	if err == nil {
		t.Fatal("expected bind-in-use error")
	}
	msg := err.Error()
	for _, want := range []string{
		"TROUBLE-SCRATCH-DAEMON",
		"python3",
		fmt.Sprintf("pid %d", cmd.Process.Pid),
		"started ",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("holder diagnostic missing %q in:\n%s", want, msg)
		}
	}
	t.Logf("holder diagnostic:\n%s", msg)
}

// TestPreflightBindsNoHolderKeepsBareHint pins the other side of the contract:
// a bind failure with NO foreign listener (here: a non-local address, refused
// before any socket exists) keeps the original single-line diagnostic — the
// holder line is additive, never fabricated.
func TestPreflightBindsNoHolderKeepsBareHint(t *testing.T) {
	cfg := defaults()
	cfg.Dashboard.Bind = "192.0.2.1:7699" // TEST-NET-1: EADDRNOTAVAIL, no listener
	_, err := PreflightBinds(*cfg)
	if err == nil {
		t.Fatal("expected bind failure")
	}
	msg := err.Error()
	if strings.Contains(msg, "TROUBLE-SCRATCH-DAEMON") {
		t.Fatalf("holder line fabricated with no holder:\n%s", msg)
	}
	if !strings.Contains(msg, "ss -tlnp") {
		t.Fatalf("bare hint lost:\n%s", msg)
	}
}

// waitForListener polls until something accepts on addr (the child holder's
// listen() is asynchronous relative to Start()).
func waitForListener(addr string, within time.Duration) error {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("nothing listening on %s within %v", addr, within)
}

func TestZoneOf(t *testing.T) {
	if got := ZoneOf("127.0.0.1:7644", "psi", AuthToken); got != "loopback" {
		t.Errorf("loopback zone: got %q", got)
	}
	if got := ZoneOf("10.0.0.1:7644", "psi", AuthToken); got != "lan" {
		t.Errorf("lan zone: got %q", got)
	}
}
