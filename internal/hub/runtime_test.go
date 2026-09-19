package hub

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// runtime_test.go pins SPEC-13 §4.1's boot sequence and §4.3's degradation matrix
// as the runtime implements them: a refused boot when require_redis=true, a
// degraded start that serves on the standalone path when it is false, the
// reconnect that rewires the consumer, the health stanza, and the archival tier
// that fails independently of ingestion.

func lightHubProfile() types.ProfileConfig {
	return types.ProfileConfig{
		Profile:         ProfileLightHub,
		RedisURL:        "redis://127.0.0.1:6379/0",
		RedisStream:     "trouble:ingest",
		ConsumerGroup:   "ledger-writers",
		MaxLen:          1000,
		DedupTTL:        types.Duration("24h"),
		DBNamespace:     "trouble/h1",
		DBEndpoint:      "http://127.0.0.1:7645",
		ArchiveInterval: types.Duration("1h"),
		Valid:           true,
	}
}

func runtimeCfg(t *testing.T, f *fakeStreams, ledger Appender, standalone Appender, mutate func(*RuntimeConfig)) RuntimeConfig {
	t.Helper()
	cfg := RuntimeConfig{
		Profile:    lightHubProfile(),
		Streams:    f,
		StateRoot:  t.TempDir(),
		HostID:     "7f3a91c2d4e5b607",
		Ledger:     ledger,
		Standalone: standalone,
		Redis: RedisConfig{
			URL:          "redis://127.0.0.1:6379/0",
			Stream:       "trouble:ingest",
			Group:        "ledger-writers",
			HostID:       "7f3a91c2d4e5b607",
			DoorWait:     2 * time.Second,
			ClaimIdle:    time.Millisecond,
			RequireRedis: false,
		},
		Archive: ArchiveConfig{
			Enabled:            true,
			Namespace:          "trouble/h1",
			Endpoint:           "",
			KeepLocalGens:      0,
			VerifyAfterWriting: true,
			Gzip:               true,
		},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return cfg
}

// TestRuntimeDegradedBootServesTheStandalonePath is the §4.3 row
// "Redis unreachable at boot | false": the daemon starts, the in-process path
// serves, and the health surface names the reason.
func TestRuntimeDegradedBootServesTheStandalonePath(t *testing.T) {
	f := newFakeStreams()
	f.pingErr = errors.New("dial tcp 127.0.0.1:6379: connect: connection refused")
	events := &eventLog{}
	ledger := newFakeLedger(events)
	standalone := newFakeLedger(events)
	cfg := runtimeCfg(t, f, ledger, standalone, nil)
	rt, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("a degraded boot must not fail: %v", err)
	}
	if rt.RuntimeMode() != ModeDegradedBoot {
		t.Fatalf("mode = %q want degraded_boot", rt.RuntimeMode())
	}
	if rt.BootCode() != types.CodeHub003 {
		t.Fatalf("boot code = %q want TROUBLE-HUB-003", rt.BootCode())
	}
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer rt.Close()

	draft := draftFor(types.KEvent, "sentinel:sha256v1:9f2c1d3e4b5a6c7d", "sentinel", nil)
	rec, err := rt.Ingest(context.Background(), draft)
	if err != nil {
		t.Fatalf("Ingest on a degraded boot: %v", err)
	}
	if rec.Seq == 0 {
		t.Fatalf("the fallback returned a record with no seq: %+v", rec)
	}
	if standalone.Count() != 1 || ledger.Count() != 0 {
		t.Fatalf("fallback wrote %d / queue wrote %d, want 1 / 0", standalone.Count(), ledger.Count())
	}
	if n := rt.IngestCounters().Fallbacks; n != 1 {
		t.Fatalf("fallback counter = %d want 1", n)
	}
	st := rt.Status(context.Background())
	if !st.Enabled || !st.Degraded || st.DegradedReason != DegradedRedisDown {
		t.Fatalf("status = %+v want enabled+degraded with reason %q", st, DegradedRedisDown)
	}
	if st.Redis.Stream != "trouble:ingest" || st.Redis.Group != "ledger-writers" {
		t.Fatalf("the stanza does not name the queue it could not reach: %+v", st.Redis)
	}
	if st.Redis.DedupWindow != "lru" {
		t.Fatalf("dedup_window = %q want lru while the gate is unreachable", st.Redis.DedupWindow)
	}
	if st.Redis.OptionsChecked {
		t.Fatalf("OptionsChecked must be false while Redis was never reached")
	}
	if st.RouteCounters["A"] != 1 {
		t.Fatalf("route counters = %v want A=1", st.RouteCounters)
	}
}

// TestRuntimeRequireRedisRefusesTheBoot is the other §4.3 row: true means the
// daemon exits instead of serving without a queue.
func TestRuntimeRequireRedisRefusesTheBoot(t *testing.T) {
	f := newFakeStreams()
	f.pingErr = errors.New("dial tcp: connection refused")
	cfg := runtimeCfg(t, f, newFakeLedger(nil), newFakeLedger(nil), func(c *RuntimeConfig) {
		c.Redis.RequireRedis = true
	})
	_, err := Open(context.Background(), cfg)
	if CodeOf(err) != types.CodeHub003 {
		t.Fatalf("code = %q want TROUBLE-HUB-003 (%v)", CodeOf(err), err)
	}
}

func TestRuntimeAuthRefusalIsTwoAndRefusesWhenRequired(t *testing.T) {
	f := newFakeStreams()
	f.pingErr = errors.New("NOAUTH Authentication required")
	cfg := runtimeCfg(t, f, newFakeLedger(nil), newFakeLedger(nil), func(c *RuntimeConfig) {
		c.Redis.RequireRedis = true
	})
	_, err := Open(context.Background(), cfg)
	if CodeOf(err) != types.CodeHub002 {
		t.Fatalf("code = %q want TROUBLE-HUB-002 (%v)", CodeOf(err), err)
	}
	cfg.Redis.RequireRedis = false
	rt, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open (degraded): %v", err)
	}
	defer rt.Close()
	if got := rt.Status(context.Background()).DegradedReason; got != DegradedRedisAuth {
		t.Fatalf("degraded reason = %q want %q", got, DegradedRedisAuth)
	}
}

// TestRuntimeUnusableGroupStopsTheBoot: TROUBLE-HUB-005 — "a queue with no
// drainer is worse than a stopped daemon".
func TestRuntimeUnusableGroupStopsTheBoot(t *testing.T) {
	f := newFakeStreams()
	f.info = ServerInfo{AOFEnabled: true, Policy: "noeviction", OptionsChecked: true}
	f.failXGroupCreate = errors.New("WRONGTYPE Operation against a key holding the wrong kind of value")
	cfg := runtimeCfg(t, f, newFakeLedger(nil), newFakeLedger(nil), nil)
	if _, err := Open(context.Background(), cfg); CodeOf(err) != types.CodeHub005 {
		t.Fatalf("code = %q want TROUBLE-HUB-005 (%v)", CodeOf(err), err)
	}
}

// TestRuntimeReconnectRewiresTheConsumer: the degraded boot becomes a live queue
// the moment Redis answers, and ingestion switches to the queue.
func TestRuntimeReconnectRewiresTheConsumer(t *testing.T) {
	f := newFakeStreams()
	f.pingErr = errors.New("connection refused")
	events := &eventLog{}
	ledger := newFakeLedger(events)
	standalone := newFakeLedger(events)
	cfg := runtimeCfg(t, f, ledger, standalone, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer rt.Close()
	if rt.IngestCounters().Fallbacks != 0 {
		t.Fatalf("nothing has been ingested yet")
	}

	// Redis returns.
	f.mu.Lock()
	f.pingErr = nil
	f.info = ServerInfo{AOFEnabled: true, Policy: "noeviction", OptionsChecked: true}
	f.mu.Unlock()
	if !rt.reconnect(ctx) {
		t.Fatalf("reconnect did not rewire the queue")
	}
	if rt.RuntimeMode() != ModeUp {
		t.Fatalf("mode = %q want up", rt.RuntimeMode())
	}
	draft := draftFor(types.KEvent, "sentinel:sha256v1:9f2c1d3e4b5a6c7d", "sentinel", nil)
	rec, err := rt.Ingest(ctx, draft)
	if err != nil {
		t.Fatalf("Ingest after the rewiring: %v", err)
	}
	if rec.Seq == 0 {
		t.Fatalf("the queue returned a record with no seq: %+v", rec)
	}
	deadline := time.Now().Add(2 * time.Second)
	for eventRecords(ledger) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// One EVENT record: the runtime also writes its own lifecycle records through
	// the same ledger seam (redis_restored), which is the point of the seam.
	if eventRecords(ledger) != 1 {
		t.Fatalf("the ledger did not receive the queued event (events=%d total=%d)", eventRecords(ledger), ledger.Count())
	}
	if standalone.Count() != 0 {
		t.Fatalf("the fallback path was used after the queue came back")
	}
	st := rt.Status(ctx)
	if st.Degraded || st.DegradedReason != DegradedNone {
		t.Fatalf("status still degraded after the rewiring: %+v", st)
	}
	if st.Redis.StreamLen == 0 {
		t.Fatalf("the stanza does not report the live stream length: %+v", st.Redis)
	}
	if st.Redis.LastAckedID == "" {
		t.Fatalf("the stanza does not report the acked watermark: %+v", st.Redis)
	}
	if st.Redis.DedupWindow != "redis" {
		t.Fatalf("dedup_window = %q want redis while the gate is live", st.Redis.DedupWindow)
	}
	if st.Redis.DedupMisses == 0 {
		t.Fatalf("the gate did not count the fresh claim: %+v", st.Redis)
	}
}

// TestRuntimeRefusesIngestionWhenTheQueueIsLost: §4.3's runtime-loss rows — the
// daemon keeps running and REFUSES (never a silent 200).
func TestRuntimeRefusesIngestionWhenTheQueueIsLost(t *testing.T) {
	f := newFakeStreams()
	f.info = ServerInfo{AOFEnabled: true, Policy: "noeviction", OptionsChecked: true}
	ledger := newFakeLedger(&eventLog{})
	cfg := runtimeCfg(t, f, ledger, newFakeLedger(&eventLog{}), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer rt.Close()
	// Redis dies: XADD fails with a READONLY-style refusal.
	f.mu.Lock()
	f.failXAdd = errors.New("READONLY You can't write against a read only replica")
	f.mu.Unlock()
	draft := draftFor(types.KEvent, "sentinel:sha256v1:9f2c1d3e4b5a6c7d", "sentinel", nil)
	if _, err := rt.Ingest(ctx, draft); CodeOf(err) != types.CodeHub004 {
		t.Fatalf("code = %q want TROUBLE-HUB-004 (%v)", CodeOf(err), err)
	}
	if rt.RuntimeMode() != ModeUnavailable {
		t.Fatalf("mode = %q want unavailable (require_redis=false)", rt.RuntimeMode())
	}
	if got := rt.Status(ctx).DegradedReason; got != DegradedRedisDown {
		t.Fatalf("degraded reason = %q want %q", got, DegradedRedisDown)
	}
	// A second request is refused WITHOUT touching the queue at all.
	before := rt.IngestCounters().Ingested
	if _, err := rt.Ingest(ctx, draft); CodeOf(err) != types.CodeHub004 {
		t.Fatalf("second refusal: code = %q err=%v", CodeOf(err), err)
	}
	if rt.IngestCounters().Ingested != before {
		t.Fatalf("an ingested event was counted while the queue was down")
	}
	if rt.IngestCounters().Refused == 0 {
		t.Fatalf("the refusal was not counted")
	}
}

// TestRuntimeArchiveFailsIndependentlyOfIngestion: an unusable DuckBrain pauses
// archival (009/`archive_paused`) and ingestion keeps serving (§4.3's two rules).
func TestRuntimeArchiveFailsIndependentlyOfIngestion(t *testing.T) {
	f := newFakeStreams()
	f.info = ServerInfo{AOFEnabled: true, Policy: "noeviction", OptionsChecked: true}
	ledger := newFakeLedger(&eventLog{})
	cfg := runtimeCfg(t, f, ledger, newFakeLedger(&eventLog{}), nil) // Archive.Endpoint is empty → unusable target
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rt.Close()
	report, err := rt.ArchiveOnce(ctx)
	if CodeOf(err) != types.CodeHub009 {
		t.Fatalf("ArchiveOnce code = %q want TROUBLE-HUB-009 (%v)", CodeOf(err), err)
	}
	if !rt.ArchivePaused() {
		t.Fatalf("the archival tier did not report itself paused")
	}
	if report.Planned != 0 {
		t.Fatalf("report = %+v want nothing planned", report)
	}
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	rec, err := rt.Ingest(ctx, draftFor(types.KEvent, "sentinel:sha256v1:9f2c1d3e4b5a6c7d", "sentinel", nil))
	if err != nil {
		t.Fatalf("ingestion must not depend on the archive tier: %v", err)
	}
	if rec.Seq == 0 {
		t.Fatalf("no record identity: %+v", rec)
	}
}

// TestRuntimeArchivesAVerifiedGeneration is the local half of §3.5 with a real
// target: plan → export → verify → mark, queue depth back to zero, and the
// generation droppable.
func TestRuntimeArchivesAVerifiedGeneration(t *testing.T) {
	ledgerRoot := t.TempDir()
	writeGeneration(t, ledgerRoot, "2026-09-15.jsonl", 1, 2)
	live := "2026-09-16.jsonl"
	writeGeneration(t, ledgerRoot, live, 3)

	f := newFakeStreams()
	f.info = ServerInfo{AOFEnabled: true, Policy: "noeviction", OptionsChecked: true}
	ledger := newFakeLedger(&eventLog{})
	cfg := runtimeCfg(t, f, ledger, newFakeLedger(&eventLog{}), func(c *RuntimeConfig) {
		c.Archive.LedgerRoot = ledgerRoot
		c.Archive.LiveFile = live
		c.Target = NewDirTarget(t.TempDir())
	})
	rt, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rt.Close()
	report, err := rt.ArchiveOnce(context.Background())
	if err != nil {
		t.Fatalf("ArchiveOnce: %v", err)
	}
	if report.Planned != 1 || report.Verified != 1 || report.Failed != 0 {
		t.Fatalf("report = %+v want 1 planned / 1 verified", report)
	}
	if report.TotalFiles != 1 {
		t.Fatalf("TotalFiles = %d want 1", report.TotalFiles)
	}
	if report.Droppable != 1 {
		t.Fatalf("droppable = %d want 1 (the verified export licenses the drop)", report.Droppable)
	}
	st := rt.Status(context.Background())
	if st.ArchivedFiles != 1 || st.MarkerPending != 0 {
		t.Fatalf("status archive fields = %+v", st)
	}
	if st.DroppableGens != 1 {
		t.Fatalf("status droppable = %d want 1", st.DroppableGens)
	}
	// A second pass has nothing to do (the marker already exists).
	if _, err := rt.ArchiveOnce(context.Background()); err != nil {
		t.Fatalf("second ArchiveOnce: %v", err)
	}
	// Open pins the archival state root to the daemon's own state root, so the
	// marker file lands under RuntimeConfig.StateRoot (SPEC-13 §3.1: one state
	// root, not a second one for the archive tier).
	if _, err := os.Stat(filepath.Join(cfg.StateRoot, "hub", "archive", "markers.jsonl")); err != nil {
		t.Fatalf("the marker file was not written: %v", err)
	}
}

// TestHubStatusHealthStanza: the one health surface carries the profile stanza
// only when a runtime exists (SPEC-13 §4.1 step 4, SPEC-TYPES §3.12).
func TestHubStatusHealthStanza(t *testing.T) {
	f := newFakeStreams()
	f.info = ServerInfo{AOFEnabled: true, Policy: "noeviction", OptionsChecked: true}
	ledger := newFakeLedger(&eventLog{})
	cfg := runtimeCfg(t, f, ledger, newFakeLedger(&eventLog{}), nil)
	rt, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rt.Close()
	st := rt.Status(context.Background())
	raw, err := json.Marshal(types.HealthResponse{Status: "ok", Hub: &st})
	if err != nil {
		t.Fatalf("marshal health: %v", err)
	}
	if !strings.Contains(string(raw), `"hub":{`) {
		t.Fatalf("health JSON has no hub stanza: %s", raw)
	}
	for _, want := range []string{`"profile":"light-hub"`, `"redis":{`, `"dedup_window":"redis"`, `"route_counters":{"A":0,"B":0}`, `"droppable_generations":0`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("health JSON missing %s: %s", want, raw)
		}
	}
	// Standalone: the field is absent, not zeroed (omitempty).
	bad, err := json.Marshal(types.HealthResponse{Status: "ok"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(bad), `"hub"`) {
		t.Fatalf("a standalone health response carries a hub stanza: %s", bad)
	}
}

// TestRuntimeWritesProfileStateAndBootRecord: §3.1's one-file truth and §4.1
// step 4's lifecycle record.
func TestRuntimeWritesProfileStateAndBootRecord(t *testing.T) {
	f := newFakeStreams()
	f.info = ServerInfo{AOFEnabled: true, Policy: "noeviction", OptionsChecked: true}
	ledger := newFakeLedger(&eventLog{})
	cfg := runtimeCfg(t, f, ledger, newFakeLedger(&eventLog{}), nil)
	rt, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rt.Close()
	st, err := ReadProfileState(cfg.StateRoot)
	if err != nil {
		t.Fatalf("ReadProfileState: %v", err)
	}
	if st.Profile != ProfileLightHub || st.Since == "" || st.ConfigHash == "" {
		t.Fatalf("profile state = %+v", st)
	}
	rt.RecordBoot(context.Background())
	var boot bool
	for _, r := range ledger.Records() {
		if op, _ := r.Payload["op"].(string); op == "hub_boot" {
			boot = true
			if r.Payload["mode"] != "up" || r.Payload["profile"] != ProfileLightHub {
				t.Fatalf("boot record = %+v", r.Payload)
			}
			if r.Payload["stream"] != "trouble:ingest" || r.Payload["group"] != "ledger-writers" {
				t.Fatalf("boot record does not name the queue: %+v", r.Payload)
			}
		}
	}
	if !boot {
		t.Fatalf("no hub_boot lifecycle record was written")
	}
}

// eventRecords counts the EVENT records an appender holds, which is what the
// queue's own contribution is: the runtime's lifecycle records share the ledger.
func eventRecords(l *fakeLedger) int {
	n := 0
	for _, r := range l.Records() {
		if r.Kind == types.KEvent {
			n++
		}
	}
	return n
}

// TestProfileInvarianceSameRecordSequence is AC-29's core claim at the package
// level: "the profile changes plumbing only". One scripted draft sequence is
// replayed twice — once on the standalone in-process path (a direct Append) and
// once through the light-hub queue (Ingest → door → XADD → consumer → Append) —
// and the two ledgers must hold the SAME sequence of records. Nothing about the
// rule engine or the ladder is involved here; what is asserted is that the hop
// neither reorders, rewrites nor drops a record.
func TestProfileInvarianceSameRecordSequence(t *testing.T) {
	script := []types.RecordDraft{
		draftFor(types.KEvent, "sentinel:sha256v1:9f2c1d3e4b5a6c7d", "sentinel:payment-worker", map[string]any{"item_type": "event", "n": 1}),
		draftFor(types.KGroup, "sentinel:sha256v1:9f2c1d3e4b5a6c7d", "sentinel:payment-worker", map[string]any{"op": "create", "group_id": "grp_1"}),
		draftFor(types.KEvent, "sentinel:sha256v1:9f2c1d3e4b5a6c7d", "sentinel:payment-worker", map[string]any{"item_type": "event", "n": 2}),
		draftFor(types.KIncident, "sentinel:sha256v1:9f2c1d3e4b5a6c7d", "ladder", map[string]any{"state": "open"}),
		draftFor(types.KEvent, "sensor:psi:1a2b3c4d", "sensor:psi", map[string]any{"rule_id": "io.full", "n": 3}),
	}

	// The standalone path: the daemon's in-process append, nothing else.
	standalone := newFakeLedger(&eventLog{})

	// The light-hub path: the same drafts through the queue.
	f := newFakeStreams()
	f.info = hubInfoOK()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	queued := newFakeLedger(&eventLog{})
	cfg := runtimeCfg(t, f, queued, standalone, nil)
	rt, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rt.Close()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	for _, d := range script {
		if _, err := standalone.Append(ctx, d); err != nil {
			t.Fatalf("standalone append: %v", err)
		}
		if _, err := rt.Ingest(ctx, d); err != nil {
			t.Fatalf("queued ingest (%s): %v", d.Kind, err)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for queued.Count() < len(script) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if counters := rt.IngestCounters(); counters.Ingested != int64(len(script)) || counters.Fallbacks != 0 {
		t.Fatalf("counters = %+v want %d queued ingests and no fallback", counters, len(script))
	}
	assertSameSequence(t, "standalone", standalone, "light-hub", queued)
}

func hubInfoOK() ServerInfo {
	return ServerInfo{AOFEnabled: true, Policy: "noeviction", OptionsChecked: true}
}

// assertSameSequence compares two ledgers' appended records by the identity the
// pipeline controls (kind, sig, incubation, and the local record id the hub
// carries into payload.local_rec_id). The canonical rec_id and the seq are
// minted per writer, so they are compared for PRESENCE, not equality: what must
// not differ is which records arrived, in what order, under what identity.
func assertSameSequence(t *testing.T, nameA string, a *fakeLedger, nameB string, b *fakeLedger) {
	t.Helper()
	ra, rb := a.Records(), b.Records()
	if len(ra) != len(rb) {
		t.Fatalf("%s holds %d records, %s holds %d", nameA, len(ra), nameB, len(rb))
	}
	for i := range ra {
		if ra[i].Kind != rb[i].Kind || ra[i].Sig != rb[i].Sig || ra[i].Inc != rb[i].Inc {
			t.Fatalf("record %d differs: %s(%s/%s/%s) vs %s(%s/%s/%s)", i,
				nameA, ra[i].Kind, ra[i].Sig, ra[i].Inc, nameB, rb[i].Kind, rb[i].Sig, rb[i].Inc)
		}
		if rb[i].RecID == "" || rb[i].Seq == 0 {
			t.Fatalf("record %d on %s has no ledger identity: %+v", i, nameB, rb[i])
		}
	}
	// The one payload-level difference the profile introduces is BY DESIGN and
	// asserted rather than waved away: a record that travelled the queue carries
	// the ingress identity the hub minted for it in `payload.local_rec_id`
	// (SPEC-13 §3.2 rule 5), because it left the process; a record on the
	// in-process path never needed one. AC-29's assertion is about the
	// incident/verify SEQUENCE, which this test has just proved identical.
	for i, r := range rb {
		if _, ok := r.Payload["local_rec_id"]; !ok {
			t.Fatalf("queued record %d lost its local_rec_id: %+v", i, r.Payload)
		}
	}
}

func TestDegradedReasonForCodes(t *testing.T) {
	cases := []struct {
		code    types.ErrorCode
		require bool
		want    string
	}{
		{types.CodeHub002, false, DegradedRedisAuth},
		{types.CodeHub003, false, DegradedRedisDown},
		{types.CodeHub004, false, DegradedRedisDown},
		{types.CodeHub004, true, DegradedRefusing},
		{types.CodeHub009, false, DegradedArchive},
		{types.CodeHub010, false, DegradedArchive},
		{types.CodeHub012, false, DegradedArchive},
		{types.CodeHub005, true, DegradedNone},
	}
	for _, tc := range cases {
		if got := DegradedReasonFor(tc.code, tc.require); got != tc.want {
			t.Fatalf("DegradedReasonFor(%s, require=%v) = %q want %q", tc.code, tc.require, got, tc.want)
		}
	}
}
