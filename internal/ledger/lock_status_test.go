package ledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/trouble-agent/trouble/internal/types"
)

func TestCheckWriterHeldDistinguishesStalePayloadFromUnlockedFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ledger")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireLock(root, types.Actor{Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer releaseLock(lock)

	// Keep the kernel flock held while replacing its advisory JSON with a PID
	// that is guaranteed not to exist. A PID probe alone mistakes this for no writer.
	stalePID := 999999
	payload, err := json.Marshal(lockFile{PID: stalePID})
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Truncate(0); err != nil {
		t.Fatal(err)
	}
	if _, err := lock.WriteAt(append(payload, '\n'), 0); err != nil {
		t.Fatal(err)
	}
	pid, held := CheckWriterHeld(root)
	if !held {
		t.Fatalf("CheckWriterHeld(%q)=(%d,%t), want flock held despite stale payload pid %d", root, pid, held, stalePID)
	}
	if pid != -1 && pid != os.Getpid() {
		t.Fatalf("stale payload pid %d yielded invalid holder pid %d; want actual holder %d or unknown -1", stalePID, pid, os.Getpid())
	}

	st, err := ReadStatus(root)
	if err != nil {
		t.Fatal(err)
	}
	if (st.WriterPID != -1 && st.WriterPID != os.Getpid()) || !st.WriterLockHeld {
		t.Fatalf("status with stale payload and live flock = writer_pid:%d writer_lock_held:%t; want holder %d or unknown/-1 with held=true", st.WriterPID, st.WriterLockHeld, os.Getpid())
	}
}

func TestCheckWriterHeldNoWriterWithDeadPID(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ledger")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(lockFile{PID: 999999})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "LOCK"), append(payload, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	pid, held := CheckWriterHeld(root)
	if held || pid != -1 {
		t.Fatalf("unlocked stale LOCK=(%d,%t), want (-1,false)", pid, held)
	}
	st, err := ReadStatus(root)
	if err != nil {
		t.Fatal(err)
	}
	if st.WriterPID != -1 || st.WriterLockHeld {
		t.Fatalf("status for dead PID without flock = writer_pid:%d writer_lock_held:%t", st.WriterPID, st.WriterLockHeld)
	}
}

func TestAcquireLockPayloadNamesHoldingProcess(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ledger")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireLock(root, types.Actor{Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer releaseLock(lock)
	if pid := readLockPID(root); pid != os.Getpid() {
		t.Fatalf("LOCK pid=%d, want holder process pid %d", pid, os.Getpid())
	}
	if pid, held := CheckWriterHeld(root); !held || pid != os.Getpid() {
		t.Fatalf("CheckWriterHeld=(%d,%t), want (%d,true)", pid, held, os.Getpid())
	}
}
