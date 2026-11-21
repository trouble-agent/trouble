package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBootExplicitMissingConfigFailsBeforeStateAndListeners(t *testing.T) {
	for _, port := range []string{"127.0.0.1:7644", "127.0.0.1:7643"} {
		ln, err := net.Listen("tcp", port)
		if err != nil {
			t.Skipf("cannot establish listener baseline on %s: %v", port, err)
		}
		_ = ln.Close()
	}
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.toml")
	state := filepath.Join(dir, "state")
	for _, flag := range []string{"--config", "--config_path"} {
		t.Run(flag, func(t *testing.T) {
			code, output := runChild(t, "run", flag, missing, "--state_root", state)
			if code == 0 || !strings.Contains(output, "TROUBLE-LIFECYCLE-018") || !strings.Contains(output, missing) {
				t.Fatalf("exit=%d output=%s", code, output)
			}
			if _, err := os.Stat(state); !os.IsNotExist(err) {
				t.Fatalf("state root touched: %v", err)
			}
			for _, port := range []string{"127.0.0.1:7644", "127.0.0.1:7643"} {
				ln, err := net.Listen("tcp", port)
				if err != nil {
					t.Errorf("port %s occupied after failed boot: %v", port, err)
				} else {
					_ = ln.Close()
				}
			}
		})
	}
}
