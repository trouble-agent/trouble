package loadfence

import (
	"path/filepath"
	"testing"
	"time"
)

func TestSuiteLeaseSerializesConcurrentAcquisition(t *testing.T) {
	path := filepath.Join(t.TempDir(), "suite.lock")
	holder, err := AcquireSuiteLease(path)
	if err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	acquired := make(chan *SuiteLease, 1)
	go func() {
		close(started)
		lease, acquireErr := AcquireSuiteLease(path)
		if acquireErr != nil {
			return
		}
		acquired <- lease
	}()
	<-started

	select {
	case <-acquired:
		t.Fatal("second suite acquired the lease before the first released it")
	case <-time.After(100 * time.Millisecond):
	}

	if err := holder.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case waiter := <-acquired:
		if err := waiter.Release(); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("second suite did not acquire the lease after release")
	}
}

func TestSuiteLeasePathIsOutsideCheckoutAndStable(t *testing.T) {
	first, err := SuiteLeasePath()
	if err != nil {
		t.Fatal(err)
	}
	second, err := SuiteLeasePath()
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("suite lease path changed between calls: %q != %q", first, second)
	}
	if filepath.Dir(first) == "" || filepath.Dir(first) == "." {
		t.Fatalf("suite lease path has no absolute directory: %q", first)
	}
}
