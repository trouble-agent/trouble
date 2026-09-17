package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/scrub"
	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// fakeClock is an injectable clock (SPEC-01 §7: every test that touches time
// injects Options.Now).
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{t: t.UTC()} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t.UTC()
	c.mu.Unlock()
}

// testBase is the parent of every test ledger root. Tests must not use /tmp —
// the state root is never /tmp (SPEC-01 §3.9) and validateRoot enforces it.
func testBase(t *testing.T) string {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("no home dir: %v", err)
	}
	base := filepath.Join(home, ".local", "state", "trouble-test")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatalf("cannot create test base %s: %v", base, err)
	}
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatalf("cannot chmod test base: %v", err)
	}
	return base
}

func testRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(testBase(t), "ledger-")
	if err != nil {
		t.Fatalf("cannot create test root: %v", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("cannot chmod test root: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// testNow is the fixed reference instant every test clock starts at.
func testNow() time.Time { return time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC) }

func testActor() types.Actor {
	return types.Actor{
		Kind: types.ActorDaemon, ID: "troubled", Version: "0.1.0",
		GitSHA: "testsha", BuildTime: "2026-09-16T09:00:00.000Z",
	}
}

func baseTestOptions(t *testing.T, clk *fakeClock) Options {
	return Options{
		Root: testRoot(t),
		Rotation: RotationPolicy{
			Cadence: "daily", AtUTC: "00:00:00Z",
			MaxBytes:        DefaultRotateMaxBytes,
			PartSuffix:      DefaultPartSuffix,
			FsyncWindowMS:   5,
			MaxBatchRecords: 64,
			QueueCapRecords: DefaultQueueCapRecords,
			MaxEnqueueWait:  "1s",
			Fdatasync:       true,
			MaxRecordBytes:  DefaultMaxRecordBytes,
		},
		Retention:      DefaultRetentionPolicy(),
		Index:          DefaultIndexOptions(),
		Writer:         testActor(),
		MaxSchema:      SchemaVersionV1,
		Now:            clk.Now,
		HostID:         "7f3a91c2d4e5b607",
		Zone:           "loopback",
		AssertScrubbed: true,
	}
}

// testLedger opens a ledger over a fresh root with small batch/window values so
// every other test is not paying the 200 ms production window.
func testLedger(t *testing.T, clk *fakeClock, muts ...func(*Options)) *Ledger {
	t.Helper()
	o := baseTestOptions(t, clk)
	for _, m := range muts {
		m(&o)
	}
	l, err := Open(context.Background(), o)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = l.Close(context.Background()) })
	return l
}

func mutOpts(f func(*Options)) func(*Options) { return f }

func eventDraft(source, sig, digest string, redactions int) types.RecordDraft {
	p := map[string]any{"op": "sample", "merge_key": MergeKey("resource_exhaustion", "io")}
	if digest != "" {
		p["digest"] = digest
	}
	return types.RecordDraft{
		Kind:       types.KEvent,
		Sig:        sig,
		Origin:     types.Origin{HostID: "7f3a91c2d4e5b607", Source: source},
		Actor:      testActor(),
		Redactions: redactions,
		Payload:    p,
	}
}

func mustAppend(t *testing.T, l *Ledger, d types.RecordDraft) types.Record {
	t.Helper()
	rec, err := l.Append(context.Background(), d)
	if err != nil {
		t.Fatalf("Append(%s): %v", d.Kind, err)
	}
	return rec
}

// readFileLines returns the raw lines of a ledger file.
func readFileLines(t *testing.T, root, name string) [][]byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var out [][]byte
	for _, ln := range splitLines(b) {
		if len(ln) == 0 {
			continue
		}
		cp := make([]byte, len(ln))
		copy(cp, ln)
		out = append(out, cp)
	}
	return out
}

func splitLines(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i := 0; i < len(b); i++ {
		if b[i] == '\n' {
			out = append(out, b[start:i])
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, b[start:])
	}
	return out
}

// writeRawFixture writes a synthetic day file directly, bypassing Append, so the
// boot-rebuild tests measure the rebuild and not the writer.
func writeRawFixture(t *testing.T, root, name string, n int, payloadBytes int, day time.Time) int64 {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(root, name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("create fixture: %v", err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	pad := make([]byte, payloadBytes)
	for i := range pad {
		pad[i] = 'x'
	}
	ts := types.FormatUTC(day)
	digest := fmt.Sprintf("%064x", 1)
	var wrote int64
	for i := 1; i <= n; i++ {
		rec := types.Record{
			Seq: uint64(i), RecID: types.NewID(types.PEv), TS: ts, Kind: types.KEvent,
			SchemaVersion: SchemaVersionV1, Sig: "psi:sha256v1:00000000000000" + fmt.Sprintf("%02x", i%256),
			Origin: types.Origin{HostID: "7f3a91c2d4e5b607", Source: "psi"},
			Actor:  testActor(),
			Payload: map[string]any{
				"digest": digest, "op": "sample", "pad": string(pad),
			},
		}
		if err := enc.Encode(&rec); err != nil {
			t.Fatalf("encode fixture: %v", err)
		}
		wrote++
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync fixture: %v", err)
	}
	fi, err := f.Stat()
	if err != nil {
		t.Fatalf("stat fixture: %v", err)
	}
	return fi.Size()
}

// loadAvg1 reads the host's 1-minute load average so throughput assertions can
// say which machine they measured on (see TestAmortizedThroughput).
func loadAvg1() float64 {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0
	}
	return v
}

// newJSONEncoder is the pinned serialization shape: no HTML escaping.
func newJSONEncoder(f *os.File) *json.Encoder {
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	return enc
}

// mandatoryScan is the write-boundary re-scan entry point under test.
func mandatoryScan(b []byte) error { return scrub.MandatoryScan(context.Background(), b) }

// prefilterAllows exposes scrub's performance prefilter to the conformance test.
func prefilterAllows(b []byte) bool { return scrub.PrefilterAllows(b) }
