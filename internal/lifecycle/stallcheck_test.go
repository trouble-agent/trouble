package lifecycle

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

func TestStallCheckAdvancing(t *testing.T) {
	cfg := stallSetup(t)
	seq := uint64(0)
	ts := types.NowUTC()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seq++
		ts = types.NowUTC()
		writeHealth(w, seq, ts)
	}))
	defer srv.Close()
	cfg.Dashboard.Bind = srv.Listener.Addr().String()

	v, err := stallCheck(context.Background(), cfg, srv.Client(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if v.Exit != 0 {
		t.Errorf("exit: got %d, want 0", v.Exit)
	}
}

func TestStallCheckFrozenSeq(t *testing.T) {
	cfg := stallSetup(t)
	oldTS := time.Now().UTC().Add(-400 * time.Second).Format(time.RFC3339Nano)
	writeHealthFile(t, cfg.Checker.StateFile, 42, oldTS)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeHealth(w, 42, oldTS)
	}))
	defer srv.Close()
	cfg.Dashboard.Bind = srv.Listener.Addr().String()

	v, err := stallCheck(context.Background(), cfg, srv.Client(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if v.Exit != 9 {
		t.Errorf("exit: got %d, want 9", v.Exit)
	}
	if v.Code != string(types.CodeLifecycle009) {
		t.Errorf("code: got %q, want 009", v.Code)
	}
}

func TestStallCheckLivenessUnreadable(t *testing.T) {
	cfg := stallSetup(t)
	fresh := time.Now().UTC().Format(time.RFC3339Nano)
	if err := writeHeartbeat(cfg.Lifecycle.HeartbeatPath, types.Heartbeat{TS: fresh, LedgerLastSeq: 1, LedgerLastTS: fresh}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad", http.StatusInternalServerError)
	}))
	defer srv.Close()
	cfg.Dashboard.Bind = srv.Listener.Addr().String()

	v, err := stallCheck(context.Background(), cfg, srv.Client(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if v.Exit != 8 {
		t.Errorf("exit: got %d, want 8", v.Exit)
	}
	if v.Class != "liveness_surface_unreadable" {
		t.Errorf("class: got %q", v.Class)
	}
}

// TRBL-124: `--health-url` was accepted and stored in cfg.HealthURL, but
// fetchHealth built its URL from Dashboard.Bind unconditionally — so pointing
// the checker at another host (or a dead one) silently measured the local
// dashboard instead and reported it healthy. These two tests pin the flag shut.
func TestStallCheckHonoursConfiguredHealthURL(t *testing.T) {
	cfg := stallSetup(t)
	fresh := time.Now().UTC().Format(time.RFC3339Nano)
	if err := writeHeartbeat(cfg.Lifecycle.HeartbeatPath, types.Heartbeat{TS: fresh, LedgerLastSeq: 1, LedgerLastTS: fresh}); err != nil {
		t.Fatal(err)
	}
	// Seed the checker state so the CONFIGURED surface's answer is a stall:
	// same seq, old timestamp (the same shape TestStallCheckFrozenSeq uses).
	oldTS := time.Now().UTC().Add(-400 * time.Second).Format(time.RFC3339Nano)
	writeHealthFile(t, cfg.Checker.StateFile, 7, oldTS)

	// The real health surface: a FROZEN seq. Grading it must produce a stall.
	var hits int
	want := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		writeHealth(w, 7, oldTS)
	}))
	defer want.Close()

	// A decoy on Dashboard.Bind that always looks perfectly healthy. If the
	// checker ignores cfg.HealthURL it hits THIS and reports green instead.
	decoy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeHealth(w, 99, time.Now().UTC().Format(time.RFC3339Nano))
	}))
	defer decoy.Close()

	cfg.HealthURL = want.URL + "/health.json"
	cfg.Dashboard.Bind = decoy.Listener.Addr().String()

	v, err := stallCheck(context.Background(), cfg, want.Client(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if hits == 0 {
		t.Fatalf("the configured health URL was never contacted — the checker "+
			"measured %q instead (verdict class=%q exit=%d)", cfg.Dashboard.Bind, v.Class, v.Exit)
	}
	if v.Exit == 0 {
		t.Errorf("exit 0 for a frozen seq on the CONFIGURED health URL — the decoy's "+
			"healthy answer was graded instead (class=%q)", v.Class)
	}
}

func TestStallCheckDeadHealthURLIsNotGreen(t *testing.T) {
	cfg := stallSetup(t)
	fresh := time.Now().UTC().Format(time.RFC3339Nano)
	if err := writeHeartbeat(cfg.Lifecycle.HeartbeatPath, types.Heartbeat{TS: fresh, LedgerLastSeq: 1, LedgerLastTS: fresh}); err != nil {
		t.Fatal(err)
	}

	// A decoy on Dashboard.Bind that always looks healthy.
	decoy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeHealth(w, 99, time.Now().UTC().Format(time.RFC3339Nano))
	}))
	defer decoy.Close()
	cfg.Dashboard.Bind = decoy.Listener.Addr().String()

	// The configured endpoint is dead: nothing listens there.
	cfg.HealthURL = "http://127.0.0.1:1/health.json"

	v, err := stallCheck(context.Background(), cfg, decoy.Client(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if v.Exit == 0 {
		t.Fatalf("exit 0 against a DEAD configured health URL — the checker fell back to "+
			"the live dashboard at %q and reported it healthy", cfg.Dashboard.Bind)
	}
	if v.Class != "liveness_surface_unreadable" {
		t.Errorf("class: got %q, want liveness_surface_unreadable", v.Class)
	}
}

func TestDetectionBound(t *testing.T) {
	cfg := defaults()
	bound := DetectionBound(*cfg)
	if bound > 420 {
		t.Errorf("detection bound %.0fs > 420s", bound)
	}
}

func stallSetup(t *testing.T) Config {
	dir := t.TempDir()
	cfg := defaults()
	cfg.StateRoot = dir
	resolved := postResolve(*cfg)
	resolved.Lifecycle.HeartbeatPath = filepath.Join(dir, "heartbeat.json")
	resolved.Checker.StateFile = filepath.Join(dir, "checker.state.json")
	resolved.Checker.AlarmFile = filepath.Join(dir, "checker.alarm")
	return resolved
}

func writeHealth(w http.ResponseWriter, seq uint64, ts string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(types.HealthResponse{
		LedgerLastSeq: seq,
		LedgerLastTS:  ts,
	})
}

func writeHealthFile(t *testing.T, path string, seq uint64, ts string) {
	st := checkerState{Seq: seq, TS: ts}
	b, _ := json.Marshal(st)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}
