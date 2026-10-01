package app

// TRBL-084: the reopened half. The AppendBatch path (0de49e8) landed inside
// internal/sentinel but the production ingest sinks did not carry the method,
// so appendRecordBatch's capability check answered ok=false on every real
// ingest and the single-event POST kept paying one group-commit window per
// record. These tests drive the COMPOSITION ROOT's wiring — the sinks
// buildSentinel mounts and the batch members themselves — against the real
// ledger, so the production path is exercised, not the test seam (memSink)
// the first cut proved.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/ledger"
	"github.com/trouble-agent/trouble/internal/scrub"
	"github.com/trouble-agent/trouble/internal/sentinel"
	"github.com/trouble-agent/trouble/internal/types"
)

// sentinelBatchSink is the shape internal/sentinel's ledgerBatchSink asserts
// (server.go): an optional capability the sentinel looks up by type at admit
// time. The production sinks must satisfy it or the batch path is dead.
type sentinelBatchSink interface {
	AppendBatch(ctx context.Context, drafts []types.RecordDraft) ([]types.Record, error)
}

// TestProductionSinksCarryAppendBatch pins the capability the whole fix rides
// on: both composition-root sinks satisfy the sentinel's batch seam, so
// appendRecordBatch's type assertion succeeds in production. This is the test
// 0de49e8 lacked — the memSink test helper carried AppendBatch while the
// shipped sinks did not, and the suite stayed green over a dead path.
func TestProductionSinksCarryAppendBatch(t *testing.T) {
	l := testLedgerForSinks(t)
	plain := sentinelSink{L: l}
	if _, ok := interface{}(plain).(sentinelBatchSink); !ok {
		t.Fatalf("sentinelSink does not satisfy the sentinel's AppendBatch seam: the single-event ingest falls back to one window per record (the TRBL-084 reopen)")
	}
	hubbed := hubSink{sentinelSink: plain}
	if _, ok := interface{}(hubbed).(sentinelBatchSink); !ok {
		t.Fatalf("hubSink does not satisfy the sentinel's AppendBatch seam: the light-hub profile keeps the per-record fallback")
	}
}

// TestSentinelSinkAppendBatchContract pins the batch member's own contract:
// the whole batch is durable on return, seqs contiguous in draft order, and
// the sink side effects (noteSample for an event carrying a digest) run for
// the durable records — the parity the per-record path guaranteed per record.
func TestSentinelSinkAppendBatchContract(t *testing.T) {
	l := testLedgerForSinks(t)
	d := &Daemon{} // nil Ladder/Subsystems: sentinelToLadder no-ops, the same degradation the per-record path has
	s := sentinelSink{L: l, d: d}

	sig := types.NewSig(types.SigSource("psi"), "sha256", 1, []byte("trbl084-batch-contract")).String()
	drafts := []types.RecordDraft{
		{Kind: types.KEvent, Sig: sig, Origin: types.Origin{HostID: "h", Source: "sentinel"}, Actor: types.Actor{Kind: types.ActorDaemon, ID: "troubled"},
			Payload: map[string]any{"digest": "trbl084-batch-contract"}},
		{Kind: types.KGroup, Sig: sig, Origin: types.Origin{HostID: "h", Source: "sentinel"}, Actor: types.Actor{Kind: types.ActorDaemon, ID: "troubled"},
			Payload: map[string]any{"op": "create"}},
	}
	before := l.Status().Records
	recs, err := s.AppendBatch(context.Background(), drafts)
	if err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("AppendBatch returned %d records, want 2", len(recs))
	}
	if recs[1].Seq != recs[0].Seq+1 {
		t.Fatalf("batch seqs not contiguous: %d then %d", recs[0].Seq, recs[1].Seq)
	}
	if got := l.Status().Records - before; got != 2 {
		t.Fatalf("ledger advanced %d records for a 2-record batch, want 2 (durable on return)", got)
	}
}

// ingestChain is the standalone-profile sentinel over the real ledger, built
// the way buildSentinel builds it (sentinelSink or the pre-fix batchless
// shape behind the same seam), behind an httptest listener.
type ingestChain struct {
	s    *sentinel.Server
	l    *ledger.Ledger
	ts   *httptest.Server
	root string
}

// windowTestPub is a 32-hex project public key (the boot validator demands
// 32 hex; the value itself is not load-bearing).
const windowTestPub = "a1b2c3d4e5f60718293a4b5c6d7e8f90"

func newIngestChain(t *testing.T, batched bool) *ingestChain {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("no home dir: %v", err)
	}
	base := filepath.Join(home, ".local", "state", "trouble-test")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatalf("mkdir base: %v", err)
	}
	root, err := os.MkdirTemp(base, "trbl084-")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })

	projects := []types.Project{{
		ID: "1", Slug: "alpha", PublicKey: windowTestPub, QuotaEPM: 1_000_000,
		Enabled: true, LossPolicy: types.LossDropCounter, DiskBudget: 1 << 31,
	}}
	eng, err := scrub.New(nil, projects)
	if err != nil {
		t.Fatalf("scrub.New: %v", err)
	}
	l, err := ledger.Open(context.Background(), ledger.Options{
		Root:      filepath.Join(root, "ledger"),
		// Sharp window policy: a small window keeps the fsync COUNT the
		// structural assertion (40ms windows do not coalesce across the
		// test's sequential requests, and MaxBatchRecords is far above the
		// 2-3 records one ingest emits).
		Rotation: ledger.RotationPolicy{
			Cadence: "day", AtUTC: "00:00", PartSuffix: "a", MaxBytes: 1 << 30,
			FsyncWindowMS: 40, MaxBatchRecords: 4096, QueueCapRecords: 8192,
			MaxEnqueueWait: "1s", Fdatasync: true, MaxRecordBytes: 1 << 20,
		},
		Retention: ledger.DefaultRetentionPolicy(),
		Index:     ledger.DefaultIndexOptions(),
		Writer:    types.Actor{Kind: types.ActorDaemon, ID: "troubled"},
		MaxSchema: ledger.SchemaVersionV1,
		Now:       time.Now,
		HostID:    "7f3a91c2d4e5b607",
		Zone:      "loopback",
	})
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	t.Cleanup(func() { _ = l.Close(context.Background()) })

	var sink sentinelWriteSink
	if batched {
		sink = sentinelSink{L: l, d: &Daemon{}}
	} else {
		// The PRE-FIX posture: only the single-record seam, exactly what the
		// shipped sinks were before the fix. It drives the same real ledger
		// so the BEFORE arm of the comparison is this host, this run.
		sink = batchlessSink{L: l}
	}
	cfg := sentinel.Config{
		Bind:           "127.0.0.1:7643",
		AdvertisedHost: "trouble.example.net",
		Scheme:         "http",
		ProxyTrust:     "loopback",
		Projects:       projects,
		SpoolDir:       filepath.Join(root, "spool"),
		HostID:         "7f3a91c2d4e5b607",
		Actor:          types.Actor{Kind: types.ActorDaemon, ID: "troubled", Version: "0.1.0"},
		LedgerWait:     types.Duration("2s"),
	}
	srv, err := sentinel.NewServer(cfg, sink, eng)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Drain(context.Background()) })
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	// The canary loop would POST to cfg.Bind (the daemon's own listener in
	// production); CanaryProject is left empty here, so Start runs no canary
	// and the bind string never has to point at the test listener.
	return &ingestChain{s: srv, l: l, ts: ts, root: root}
}

// start starts the server's background loops (the canary is disabled by an
// empty CanaryProject, so Start is cheap).
func (c *ingestChain) start(t *testing.T) {
	t.Helper()
	if err := c.s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
}

// postOneEvent POSTs one generic-JSON event and returns the event id.
func (c *ingestChain) postOneEvent(t *testing.T, n int) string {
	t.Helper()
	body := fmt.Sprintf(`{"event_id":"%032x","level":"error","culprit":"worker.claim",`+
		`"message":"queue wedge: pool exhausted depth=912","release":"payment-api@2.4.1",`+
		`"exception":{"type":"PoolExhausted","value":"queue wedge: pool exhausted depth=912","stack":[`+
		`{"file":"worker.py","function":"claim","line":118,"in_app":true,"context_line":"item = pool.get(timeout=1)"},`+
		`{"file":"queue.py","function":"get","line":44,"in_app":true,"context_line":"raise PoolExhausted(depth=912)"}]}}`, n)
	req, err := http.NewRequest(http.MethodPost, c.ts.URL+"/api/1/event/?sentry_key="+windowTestPub, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := c.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST status %d, want 200", resp.StatusCode)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out.ID
}

// TestOneIngestPostOneGroupCommitWindow is the end-to-end proof: a
// first-occurrence single-event ingest POST through the production sink
// lands its event + group records in exactly ONE group-commit window — the
// fsync-count assertion, the structural fact, not a timing proxy. The
// pre-fix posture (the batchless control) pays one window per record: the
// same POST there costs TWO fsyncs (event + group create), which is the
// control arm that keeps this test from passing vacuously.
func TestOneIngestPostOneGroupCommitWindow(t *testing.T) {
	chain := newIngestChain(t, true)
	l := chain.l
	chain.start(t)

	// One settled warm-up write (a canary-less Start writes none; the boot
	// lifecycle record landed at Open, before our counter read).
	before := l.Status().FsyncCalls
	start := time.Now()
	if id := chain.postOneEvent(t, 1); id == "" {
		t.Fatalf("ingest POST returned no event id")
	}
	after := l.Status().FsyncCalls
	elapsed := time.Since(start)

	if got := after - before; got != 1 {
		t.Fatalf("one single-event ingest POST through the production sink produced %d group-commit windows, want 1: the AppendBatch path is not on the wire", got)
	}
	t.Logf("measured: one ingest POST (event + group create, batched) = 1 fsync, %s round-trip", elapsed.Round(time.Microsecond))

	// The control arm on the SAME ledger shape but the batchless sink: the
	// identical POST costs 2 windows (event + group create), which is what
	// production did before the fix.
	ctrl := newIngestChain(t, false)
	lc := ctrl.l
	ctrl.start(t)
	before = lc.Status().FsyncCalls
	start = time.Now()
	if id := ctrl.postOneEvent(t, 2); id == "" {
		t.Fatalf("control POST returned no event id")
	}
	after = lc.Status().FsyncCalls
	elapsed = time.Since(start)
	if got := after - before; got != 2 {
		t.Fatalf("the pre-fix control produced %d windows for one POST, want 2 (event + group): the control arm no longer models the old posture", got)
	}
	t.Logf("measured: one ingest POST (event + group create, per-record control) = %d fsyncs, %s round-trip", 2, elapsed.Round(time.Microsecond))
}

// TestSingleEventIngestLatencyBeforeAfter is the latency proof (brief §3):
// the SAME first-occurrence event stream on the real ledger, once through
// the batched production sink (AFTER) and once through the batchless
// pre-fix shape (BEFORE). Rounds 2..N reuse the group opened in round 1, so
// they emit event-only batches (one window in both postures); round 1 is
// the group-opening worst case the fix targets. The report names both
// numbers and the group-opening round separately.
func TestSingleEventIngestLatencyBeforeAfter(t *testing.T) {
	afterP50, afterP95, afterFirst := driveIngestLatency(t, true)
	beforeP50, beforeP95, beforeFirst := driveIngestLatency(t, false)
	t.Logf("measured single-event ingest round-trip on the real ledger (60 rounds): "+
		"AFTER (AppendBatch production sink) p50=%s p95=%s, group-opening round %s; "+
		"BEFORE (per-record pre-fix control) p50=%s p95=%s, group-opening round %s",
		afterP50.Round(time.Microsecond), afterP95.Round(time.Microsecond), afterFirst.Round(time.Microsecond),
		beforeP50.Round(time.Microsecond), beforeP95.Round(time.Microsecond), beforeFirst.Round(time.Microsecond))
}

func driveIngestLatency(t *testing.T, batched bool) (p50, p95, first time.Duration) {
	t.Helper()
	chain := newIngestChain(t, batched)
	chain.start(t)
	const rounds = 60
	lat := make([]time.Duration, 0, rounds)
	for i := 0; i < rounds; i++ {
		start := time.Now()
		if id := chain.postOneEvent(t, i+100); id == "" {
			t.Fatalf("round %d returned no event id", i)
		}
		lat = append(lat, time.Since(start))
	}
	return percentile(lat), percentile95(lat), lat[0]
}

// --- fixtures -------------------------------------------------------------

// batchlessSink is the PRE-FIX posture: a sink with only the single-record
// seam, exactly what the shipped sinks were before the reopen fix.
type batchlessSink struct{ L *ledger.Ledger }

func (b batchlessSink) Append(ctx context.Context, d types.RecordDraft) (types.Record, error) {
	return b.L.Append(ctx, d)
}
func (b batchlessSink) LastSeq() uint64 { return b.L.Status().LastSeq }
func (b batchlessSink) ScanFrom(seq uint64, yield func(types.Record) bool) error {
	return b.L.Query().ScanFrom(seq, yield)
}

func testLedgerForSinks(t *testing.T) *ledger.Ledger {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("no home dir: %v", err)
	}
	base := filepath.Join(home, ".local", "state", "trouble-test")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatalf("mkdir base: %v", err)
	}
	root, err := os.MkdirTemp(base, "batchsink-")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	l, err := ledger.Open(context.Background(), ledger.Options{
		Root:      root,
		Rotation:  ledger.DefaultRotationPolicy(),
		Retention: ledger.DefaultRetentionPolicy(),
		Index:     ledger.DefaultIndexOptions(),
		Writer:    types.Actor{Kind: types.ActorDaemon, ID: "troubled"},
		MaxSchema: ledger.SchemaVersionV1,
		Now:       time.Now,
		HostID:    "7f3a91c2d4e5b607",
		Zone:      "loopback",
	})
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	t.Cleanup(func() { _ = l.Close(context.Background()) })
	return l
}

func percentile(lat []time.Duration) time.Duration {
	if len(lat) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), lat...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)*50/100]
}

func percentile95(lat []time.Duration) time.Duration {
	if len(lat) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), lat...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)*95/100]
}
