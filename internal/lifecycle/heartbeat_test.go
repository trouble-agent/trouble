package lifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
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

// ---------------------------------------------------------------- the loop

// watermarkWriter is a RecordWriter that also carries the ledger watermark seam
// the daemon's DraftWriter exposes (SPEC-12 §3.3): the heartbeat reads
// ledger_last_seq/ledger_last_ts through an optional type assertion.
type watermarkWriter struct {
	mu      sync.Mutex
	seq     uint64
	ts      string
	appends []types.RecordDraft
}

func (w *watermarkWriter) Append(_ context.Context, d types.RecordDraft) (types.Record, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.appends = append(w.appends, d)
	return types.Record{}, nil
}

func (w *watermarkWriter) Seq() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.seq
}

func (w *watermarkWriter) LastRecordTS() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ts
}

func (w *watermarkWriter) set(seq uint64, ts string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.seq, w.ts = seq, ts
}

func (w *watermarkWriter) drafts() []types.RecordDraft {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]types.RecordDraft(nil), w.appends...)
}

// plainWriter is a RecordWriter with NO watermark seam: the honest-degradation
// case (the heartbeat reports zero rather than inventing a sequence).
type plainWriter struct{}

func (plainWriter) Append(context.Context, types.RecordDraft) (types.Record, error) {
	return types.Record{}, nil
}

// waitHeartbeat polls the heartbeat file until want holds (the loop writes on a
// ticker, so the test cannot assume a write lands synchronously with the read).
func waitHeartbeat(t *testing.T, path string, want func(types.Heartbeat) bool) types.Heartbeat {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last types.Heartbeat
	for time.Now().Before(deadline) {
		if hb, err := ReadHeartbeat(path); err == nil {
			last = hb
			if want(hb) {
				return hb
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("heartbeat never satisfied the predicate; last read: %+v", last)
	return last
}

// TestHeartbeatCarriesTheLedgerWatermark is TRBL-010: heartbeat.json must carry
// the real ledger_last_seq/ledger_last_ts. They used to be a pair of locals
// initialised to zero and never updated, so a live instance with records wrote
// ledger_last_seq=0 / ledger_last_ts="" on every beat — including the shutdown
// beat, whose own writer seam can never succeed while the adapter hides it.
func TestHeartbeatCarriesTheLedgerWatermark(t *testing.T) {
	cfg := opsCfg(t)
	cfg.Lifecycle.HeartbeatInterval = types.Duration("20ms")
	cfg.Lifecycle.IdleHeartbeatInterval = types.Duration("1h") // no idle tick here

	w := &watermarkWriter{}
	w.set(41207, "2026-09-16T09:14:03.221Z")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- HeartbeatLoop(ctx, cfg, w, func() map[string]string { return map[string]string{} }) }()

	hb := waitHeartbeat(t, cfg.Lifecycle.HeartbeatPath, func(hb types.Heartbeat) bool {
		return hb.LedgerLastSeq == 41207
	})
	if hb.LedgerLastTS != "2026-09-16T09:14:03.221Z" {
		t.Errorf("ledger_last_ts = %q, want the writer's current stamp", hb.LedgerLastTS)
	}

	// The watermark is read per write, not cached at boot: a record that lands
	// between two beats shows up on the next one.
	w.set(41208, "2026-09-16T09:14:05.001Z")
	waitHeartbeat(t, cfg.Lifecycle.HeartbeatPath, func(hb types.Heartbeat) bool {
		return hb.LedgerLastSeq == 41208
	})

	cancel()
	if err := <-done; err == nil || !errors.Is(err, context.Canceled) {
		t.Errorf("HeartbeatLoop returned %v, want context.Canceled", err)
	}
	// The shutdown beat carries the same real watermark.
	shutdown := waitHeartbeat(t, cfg.Lifecycle.HeartbeatPath, func(hb types.Heartbeat) bool {
		return hb.Stage == "shutdown"
	})
	if shutdown.LedgerLastSeq != 41208 || shutdown.LedgerLastTS == "" {
		t.Errorf("shutdown heartbeat = seq %d ts %q, want the live watermark 41208", shutdown.LedgerLastSeq, shutdown.LedgerLastTS)
	}
}

// TestHeartbeatIdleTickAdvancesTheLedger is SPEC-12 §3.3: on a quiet host the
// loop appends one lifecycle record with payload stage=idle_tick once
// now - last_seq_ts >= idle_heartbeat_interval. The rule was dead code while the
// timestamp it tests was permanently empty.
func TestHeartbeatIdleTickAdvancesTheLedger(t *testing.T) {
	cfg := opsCfg(t)
	cfg.Lifecycle.HeartbeatInterval = types.Duration("10ms")
	cfg.Lifecycle.IdleHeartbeatInterval = types.Duration("20ms")

	w := &watermarkWriter{}
	w.set(7, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- HeartbeatLoop(ctx, cfg, w, func() map[string]string { return map[string]string{} }) }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, d := range w.drafts() {
			if d.Kind == types.KLifecycle && d.Payload["stage"] == "idle_tick" {
				cancel()
				<-done
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	t.Fatal("a quiet ledger never produced an idle_tick record")
}

// TestHeartbeatWithoutWatermarkSeamReportsZero: a writer that does not expose
// the seam reports zero — never an invented number — and no idle_tick can be
// decided without a timestamp.
func TestHeartbeatWithoutWatermarkSeamReportsZero(t *testing.T) {
	cfg := opsCfg(t)
	cfg.Lifecycle.HeartbeatInterval = types.Duration("20ms")
	cfg.Lifecycle.IdleHeartbeatInterval = types.Duration("1ms")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- HeartbeatLoop(ctx, cfg, plainWriter{}, func() map[string]string { return map[string]string{} })
	}()

	hb := waitHeartbeat(t, cfg.Lifecycle.HeartbeatPath, func(hb types.Heartbeat) bool { return hb.PID != 0 })
	if hb.LedgerLastSeq != 0 || hb.LedgerLastTS != "" {
		t.Errorf("watermark = %d/%q, want 0/\"\" for a writer with no seam", hb.LedgerLastSeq, hb.LedgerLastTS)
	}
	cancel()
	<-done
}
