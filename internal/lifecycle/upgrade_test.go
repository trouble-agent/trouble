package lifecycle

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestUpgradeParkFailureNoRename(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "trouble")
	if err := os.WriteFile(bin, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	cfg.StateRoot = t.TempDir()
	plan := upgradePlan{
		CurrentBinary: bin,
		NewBinary:     bin,
		Park: func(ctx context.Context) (int, error) {
			return 0, context.Canceled
		},
		Restart: func(ctx context.Context) error { return nil },
		Ready:   func(ctx context.Context) bool { return true },
	}
	err := Upgrade(context.Background(), *cfg, plan)
	if err == nil {
		t.Fatal("expected park failure")
	}
	b, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "old" {
		t.Errorf("binary was modified despite park failure")
	}
}

func TestUpgradeRenameOverSucceeds(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "trouble")
	if err := os.WriteFile(bin, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	newBin := filepath.Join(t.TempDir(), "trouble.new")
	if err := os.WriteFile(newBin, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := defaults()
	cfg.StateRoot = t.TempDir()
	called := false
	plan := upgradePlan{
		CurrentBinary: bin,
		NewBinary:     newBin,
		Park: func(ctx context.Context) (int, error) {
			return 0, nil
		},
		Restart: func(ctx context.Context) error {
			called = true
			return nil
		},
		Ready: func(ctx context.Context) bool { return true },
	}
	if err := Upgrade(context.Background(), *cfg, plan); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if !called {
		t.Error("restart not called")
	}
	b, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "new" {
		t.Errorf("binary not replaced: got %q", string(b))
	}
}

// TestETXTBSYRegression is SPEC-12 §6.2: the fleet's measured trap is that
// open(live, O_WRONLY|O_TRUNC) on a *running* binary fails with ETXTBSY while
// os.Rename over the same path succeeds. It builds a real helper binary with
// `go build` into the test's temp dir, execs it from the live path, and then
// asserts both halves: the naive write fails (ETXTBSY specifically, not any
// error) and the rename-over path replaces the file while the old process
// keeps running against the old inode. Hermetic: the helper source, the go
// build cache and both binaries live under t.TempDir().
func TestETXTBSYRegression(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	mainGo := filepath.Join(src, "main.go")
	// Hold an open descriptor on ourselves for two minutes, then exit.
	if err := os.WriteFile(mainGo, []byte("package main\n\nimport (\n	\"os\"\n	\"time\"\n)\n\nfunc main() {\n	f, err := os.Open(os.Args[0])\n	if err != nil {\n		os.Exit(3)\n	}\n	defer f.Close()\n	_, _ = os.Stdout.WriteString(\"held\\n\")\n	time.Sleep(2 * time.Minute)\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	holder := filepath.Join(dir, "holder")
	build := exec.Command("go", "build", "-o", holder, "main.go")
	build.Dir = src
	// Keep the build hermetic: in-temp cache, no network, no toolchain
	// download if the local toolchain is older than go.mod demands.
	build.Env = append(os.Environ(),
		"GOCACHE="+filepath.Join(dir, "gocache"),
		"GOTOOLCHAIN=local",
		"GOFLAGS=-mod=mod",
	)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build helper: %v\n%s", err, out)
	}

	// "Live" binary: a copy of the helper at the path the upgrade recipe
	// would protect, executed so the kernel maps it and holds it busy.
	bin := filepath.Join(dir, "trouble")
	for _, p := range []struct{ src, dst string }{{holder, bin}} {
		b, err := os.ReadFile(p.src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p.dst, b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(bin)
	var held atomic.Bool
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start live binary: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	go func() {
		buf := make([]byte, 16)
		if n, _ := stdout.Read(buf); n >= 4 && string(buf[:4]) == "held" {
			held.Store(true)
		}
	}()
	defer func() {
		_ = cmd.Process.Kill()
		<-exited
	}()
	t.Cleanup(func() { _ = stdout.Close() })
	deadline := time.Now().Add(5 * time.Second)
	for !held.Load() {
		select {
		case err := <-exited:
			t.Fatalf("helper exited before handshake: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("helper never acknowledged holding its own inode")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Half 1: the naive path must fail, and with ETXTBSY specifically —
	// any other failure would not prove the trap exists.
	f, err := os.OpenFile(bin, os.O_WRONLY|os.O_TRUNC, 0)
	if err == nil {
		f.Close()
		t.Fatal("expected open(live, O_WRONLY|O_TRUNC) to fail on a running binary")
	}
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) {
		t.Fatalf("expected *os.PathError, got %T: %v", err, err)
	}
	if !errors.Is(pathErr.Err, syscall.ETXTBSY) {
		t.Fatalf("expected ETXTBSY, got errno %v", pathErr.Err)
	}
	if runtime.GOOS != "linux" {
		t.Logf("non-Linux host: ETXTBSY identity asserted via syscall constant %v", syscall.ETXTBSY)
	}

	// Half 2: the recipe's path — stage to a fresh inode, chmod 0755,
	// rename over the live path — must succeed while the process runs.
	newBin := bin + ".new"
	hb, err := os.ReadFile(holder)
	if err != nil {
		t.Fatal(err)
	}
	suffix := []byte("\n// v2\n")
	if err := os.WriteFile(newBin, append(hb, suffix...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(newBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(newBin, bin); err != nil {
		t.Fatalf("rename-over a running binary failed (the recipe relies on it): %v", err)
	}
	replaced, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if len(replaced) != len(hb)+len(suffix) {
		t.Errorf("live path does not carry the staged bytes")
	}

	// And the running process is still alive against the old inode.
	select {
	case err := <-exited:
		t.Fatalf("running binary died across the rename: %v", err)
	default:
	}
}
