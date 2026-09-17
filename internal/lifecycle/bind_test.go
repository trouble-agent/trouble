package lifecycle

import (
	"net"
	"testing"
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

func TestZoneOf(t *testing.T) {
	if got := ZoneOf("127.0.0.1:7644", "psi", AuthToken); got != "loopback" {
		t.Errorf("loopback zone: got %q", got)
	}
	if got := ZoneOf("10.0.0.1:7644", "psi", AuthToken); got != "lan" {
		t.Errorf("lan zone: got %q", got)
	}
}
