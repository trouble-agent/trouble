package sensors

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// harness wires a Sensors against an in-memory ledger, a controllable clock and
// a temp state root. Every test in this package goes through it so the injected
// seams (emit, redact, now) are the ones production uses.
type harness struct {
	t   *testing.T
	s   *Sensors
	dir string

	// discard makes emit a sink that keeps nothing: it exists for the memory
	// measurement, where a retaining harness would be what grows, not the
	// subsystem under test.
	discard bool

	mu      sync.Mutex
	records []types.Record
	drafts  []types.RecordDraft
	emitErr error
	block   chan struct{}

	clockMu sync.Mutex
	clock   time.Time
}

func newHarness(t *testing.T, extra ...types.ConfigValue) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{t: t, dir: dir, clock: time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)}
	vals := []types.ConfigValue{
		{Key: "origin.host_id", Value: "testhost", Source: "default"},
		{Key: "state_root", Value: filepath.Join(dir, "state"), Source: "default"},
		{Key: "sensors.rules.dir", Value: filepath.Join(dir, "rules.d"), Source: "default"},
		{Key: "registry.modules", Value: []string{"proc.top", "proc.connections", "service.reload"}, Source: "default"},
	}
	vals = append(vals, extra...)
	s, err := New(vals, h.emit, h.redact, h.now)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.s = s
	return h
}

func (h *harness) now() time.Time {
	h.clockMu.Lock()
	defer h.clockMu.Unlock()
	return h.clock
}

func (h *harness) advance(d time.Duration) {
	h.clockMu.Lock()
	defer h.clockMu.Unlock()
	h.clock = h.clock.Add(d)
}

func (h *harness) setClock(t time.Time) {
	h.clockMu.Lock()
	defer h.clockMu.Unlock()
	h.clock = t
}

// setBlock installs a gate that holds every emit until the channel is closed.
// The gate is guarded because a test releases it from another goroutine.
func (h *harness) setBlock(ch chan struct{}) {
	h.mu.Lock()
	h.block = ch
	h.mu.Unlock()
}

func (h *harness) clearBlock() {
	h.mu.Lock()
	h.block = nil
	h.mu.Unlock()
}

func (h *harness) emit(ctx context.Context, d types.RecordDraft) (types.Record, error) {
	h.mu.Lock()
	gate := h.block
	h.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return types.Record{}, ctx.Err()
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.emitErr != nil {
		return types.Record{}, h.emitErr
	}
	rec := types.Record{
		Seq:           uint64(len(h.records) + 1),
		RecID:         types.NewID(types.PEv),
		TS:            types.FormatUTC(h.now()),
		Kind:          d.Kind,
		SchemaVersion: 1,
		Sig:           d.Sig,
		Inc:           d.Inc,
		Origin:        d.Origin,
		Actor:         d.Actor,
		Redactions:    d.Redactions,
		Payload:       d.Payload,
	}
	if h.discard {
		return rec, nil
	}
	h.records = append(h.records, rec)
	h.drafts = append(h.drafts, d)
	return rec, nil
}

// redact mimics SPEC-02: it blanks obvious secrets and counts them, so the
// sensors-side accounting (Redactions, redactions/raw-never-persisted) is
// exercised by the real call path.
func (h *harness) redact(b []byte, _ string) (types.ScrubResult, error) {
	out := make([]byte, 0, len(b))
	n := 0
	i := 0
	for i < len(b) {
		if i+8 <= len(b) && string(b[i:i+7]) == "SECRET=" {
			out = append(out, []byte("[REDACTED:env_assign]")...)
			n++
			i += 7
			for i < len(b) && b[i] != ' ' && b[i] != '\n' {
				i++
			}
			continue
		}
		out = append(out, b[i])
		i++
	}
	return types.ScrubResult{Value: out, Redactions: n, BytesIn: len(b)}, nil
}

func (h *harness) snapshot() []types.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]types.Record, len(h.records))
	copy(out, h.records)
	return out
}

// kinds counts emitted records by kind.
func (h *harness) kinds() map[types.RecordKind]int {
	out := map[types.RecordKind]int{}
	for _, r := range h.snapshot() {
		out[r.Kind]++
	}
	return out
}

// eventsWith returns the emitted event records whose payload matches a predicate.
func (h *harness) recordsWhere(pred func(types.Record) bool) []types.Record {
	var out []types.Record
	for _, r := range h.snapshot() {
		if pred(r) {
			out = append(out, r)
		}
	}
	return out
}

func (h *harness) fired() []types.Record {
	return h.recordsWhere(func(r types.Record) bool {
		v, ok := r.Payload["fire"].(bool)
		return ok && v
	})
}

func (h *harness) gaps() []types.Record {
	return h.recordsWhere(func(r types.Record) bool { return r.Kind == types.KGap })
}

// psiEvent is a convenient psi SensorEvent for rule tests.
func psiEvent(scope string, avg10 float64, wake bool) types.SensorEvent {
	hasFull := scope == "memory" || scope == "io"
	d := map[string]any{
		"metric":      "some",
		"band":        psiBucket(avg10),
		"scope":       scope,
		"some_avg10":  avg10,
		"some_avg60":  avg10,
		"some_avg300": avg10,
		"some_total":  float64(1),
		"stall_us":    float64(0),
		"count":       1,
		"severity":    "info",
		"window_s":    2.0,
		"age_s":       0.0,
		"msg":         "",
		"substr":      "",
		"unit":        "pct",
		"sensor":      "psi",
		"source":      "psi",
	}
	if hasFull {
		d["full_avg10"] = avg10
		d["full_avg60"] = avg10
		d["full_avg300"] = avg10
		d["full_total"] = float64(1)
	}
	return types.SensorEvent{
		ID:     types.NewID(types.PEv),
		TS:     types.FormatUTC(time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)),
		Sensor: types.SenPSI,
		Scope:  scope,
		Value:  avg10,
		Unit:   "pct",
		Detail: d,
		Wake:   wake,
		Sig:    sigFor(types.SrcPSI, scope, "some", psiBucket(avg10)),
	}
}

// writeRules writes one rules file into the harness's rules directory.
func (h *harness) writeRules(name, body string) string {
	dir := filepath.Join(h.dir, "rules.d")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.t.Fatalf("mkdir rules.d: %v", err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		h.t.Fatalf("write %s: %v", name, err)
	}
	return p
}

// mustReload reloads and fails the test on error.
func (h *harness) mustReload() {
	h.t.Helper()
	if err := h.s.Reload(context.Background()); err != nil {
		h.t.Fatalf("Reload: %v", err)
	}
}

func testConfig() []types.ConfigValue {
	return []types.ConfigValue{
		{Key: "sensors.psi.sample_interval", Value: "1s"},
		{Key: "sensors.disk.interval", Value: "50ms"},
		{Key: "sensors.timers.interval", Value: "50ms"},
		{Key: "sensors.journald.probe_interval", Value: "50ms"},
		{Key: "sensors.inotify.recheck_interval", Value: "50ms"},
	}
}

func TestNewRequiresHostIDAndEmit(t *testing.T) {
	if _, err := New([]types.ConfigValue{}, func(context.Context, types.RecordDraft) (types.Record, error) {
		return types.Record{}, nil
	}, nil, nil); err == nil {
		t.Fatal("expected a missing origin.host_id to fail loudly")
	}
	dir := t.TempDir()
	_, err := New([]types.ConfigValue{
		{Key: "origin.host_id", Value: "h"},
		{Key: "state_root", Value: filepath.Join(dir, "s")},
		{Key: "sensors.rules.dir", Value: filepath.Join(dir, "r")},
	}, nil, nil, nil)
	if err == nil {
		t.Fatal("expected a nil emit to fail loudly")
	}
}

func TestUnknownSensorKeyFailsLoudly(t *testing.T) {
	dir := t.TempDir()
	_, err := New([]types.ConfigValue{
		{Key: "origin.host_id", Value: "h"},
		{Key: "state_root", Value: filepath.Join(dir, "s")},
		{Key: "sensors.rules.dir", Value: filepath.Join(dir, "r")},
		{Key: "sensors.psi.sample_intervall", Value: "2s"}, // typo
	}, func(context.Context, types.RecordDraft) (types.Record, error) { return types.Record{}, nil }, nil, nil)
	if err == nil {
		t.Fatal("a misspelled sensors.* key must be a loud failure, not a silent ignore")
	}
	if got := fmt.Sprint(err); !contains(got, "unknown configuration key") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNonSensorKeyIsIgnored(t *testing.T) {
	h := newHarness(t, types.ConfigValue{Key: "sentinel.bind", Value: "127.0.0.1:7643"})
	if h.s == nil {
		t.Fatal("a non-sensors key must not fail construction")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
