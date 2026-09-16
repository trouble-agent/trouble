package lifecycle

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

func TestWriteHeartbeatAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "heartbeat.json")
	hb := types.Heartbeat{TS: types.NowUTC(), PID: os.Getpid(), Version: "0.1.0"}
	if err := writeHeartbeat(path, hb); err != nil {
		t.Fatal(err)
	}
	r, err := ReadHeartbeat(path)
	if err != nil {
		t.Fatal(err)
	}
	if r.Version != hb.Version {
		t.Errorf("version: got %q, want %q", r.Version, hb.Version)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("heartbeat mode=%04o, want 0600", info.Mode().Perm())
	}
}

func TestHeartbeatManyWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "heartbeat.json")
	for i := 0; i < 240; i++ {
		hb := types.Heartbeat{TS: types.NowUTC(), PID: os.Getpid(), Version: "0.1.0", LedgerLastSeq: uint64(i)}
		if err := writeHeartbeat(path, hb); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	r, err := ReadHeartbeat(path)
	if err != nil {
		t.Fatal(err)
	}
	if r.LedgerLastSeq != 239 {
		t.Errorf("last seq: got %d, want 239", r.LedgerLastSeq)
	}
}

func TestHeartbeatConcurrentReads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "heartbeat.json")
	if err := writeHeartbeat(path, types.Heartbeat{TS: types.NowUTC(), PID: 1, Version: "0.1.0"}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := ReadHeartbeat(path)
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("read error: %v", err)
	}
}
