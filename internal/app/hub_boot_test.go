package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/hub"
	"github.com/trouble-agent/trouble/internal/types"
)

// hub_boot_test.go is the composition root's half of SPEC-13: a validated
// light-hub profile opens the queue, mounts it in front of the sentinel's ledger
// sink, reports the `hub` stanza on the one health surface, and STILL serves the
// standalone path unchanged when the profile is the default.
//
// The Redis is an in-memory implementation of hub.Streams (an app-side fake, so
// the hub package ships no test scaffolding): the point of these tests is the
// WIRING — config → gate → runtime → sink → ledger — and that wiring is exactly
// what a live Redis would not exercise differently. Live Redis/DuckBrain
// behaviour is proven in internal/hub's own tests through the same seams.

// ---- in-memory hub.Streams ----

type memStreams struct {
	mu      sync.Mutex
	entries []*memEntry
	groups  map[string]*memGroup
	kv      map[string]string
	seq     int
	pingErr error
	info    hub.ServerInfo
}

type memEntry struct {
	id     string
	fields map[string]string
}

type memGroup struct {
	last    string
	read    int64
	pel     map[string]*memEntry
	order   []string
	entries int64
}

func newMemStreams() *memStreams {
	return &memStreams{
		groups: map[string]*memGroup{},
		kv:     map[string]string{},
		info:   hub.ServerInfo{AOFEnabled: true, Policy: "noeviction", OptionsChecked: true},
	}
}

func (m *memStreams) Ping(ctx context.Context) error { return m.pingErr }

func (m *memStreams) ServerInfo(ctx context.Context) (hub.ServerInfo, error) {
	return m.info, nil
}

func (m *memStreams) XAdd(ctx context.Context, stream string, maxLen int64, values map[string]any) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	ent := &memEntry{id: fmt.Sprintf("1758012841221-%d", m.seq), fields: map[string]string{}}
	for k, v := range values {
		ent.fields[k] = fmt.Sprint(v)
	}
	m.entries = append(m.entries, ent)
	return ent.id, nil
}

func (m *memStreams) XLen(ctx context.Context, stream string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return int64(len(m.entries)), nil
}

func (m *memStreams) XGroupCreate(ctx context.Context, stream, group, start string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.groups[group]; ok {
		return errors.New("BUSYGROUP Consumer Group name already exists")
	}
	m.groups[group] = &memGroup{pel: map[string]*memEntry{}}
	return nil
}

func (m *memStreams) XReadGroup(ctx context.Context, req hub.ReadRequest) ([]hub.StreamEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, ok := m.groups[req.Group]
	if !ok {
		return nil, errors.New("NOGROUP")
	}
	var out []hub.StreamEntry
	for _, ent := range m.entries {
		if len(out) >= req.Count {
			break
		}
		if g.last != "" && ent.id <= g.last {
			continue
		}
		g.pel[ent.id] = ent
		g.order = append(g.order, ent.id)
		g.last = ent.id
		g.read++
		out = append(out, hub.StreamEntry{ID: ent.id, Fields: ent.fields, DeliveryCount: 1})
	}
	return out, nil
}

func (m *memStreams) XAck(ctx context.Context, stream, group string, ids ...string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g := m.groups[group]
	if g == nil {
		return 0, errors.New("NOGROUP")
	}
	var n int64
	for _, id := range ids {
		if _, ok := g.pel[id]; ok {
			delete(g.pel, id)
			n++
		}
	}
	return n, nil
}

func (m *memStreams) XAutoClaim(ctx context.Context, req hub.ClaimRequest) (hub.ClaimResult, error) {
	return hub.ClaimResult{Next: "0-0"}, nil
}

func (m *memStreams) XPending(ctx context.Context, stream, group string) (hub.Pending, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g := m.groups[group]
	if g == nil {
		return hub.Pending{}, errors.New("NOGROUP")
	}
	return hub.Pending{Count: int64(len(g.pel))}, nil
}

func (m *memStreams) XInfoGroups(ctx context.Context, stream string) (hub.GroupInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, g := range m.groups {
		return hub.GroupInfo{Name: name, Consumers: 1, Pending: int64(len(g.pel)), LastDeliveredID: g.last, EntriesRead: g.read, Lag: int64(len(m.entries)) - g.read}, nil
	}
	return hub.GroupInfo{}, errors.New("NOGROUP")
}

func (m *memStreams) SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.kv[key]; ok {
		return false, nil
	}
	m.kv[key] = value
	return true, nil
}

func (m *memStreams) SetXX(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.kv[key]; !ok {
		return false, nil
	}
	m.kv[key] = value
	return true, nil
}

func (m *memStreams) Get(ctx context.Context, key string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.kv[key]
	return v, ok, nil
}

// TTL answers 0 when the key is absent, -1 when it exists without an expiry
// (the mem fake never expires keys): the §2.2 probe's two non-positive facts.
func (m *memStreams) TTL(ctx context.Context, key string) (time.Duration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.kv[key]; !ok {
		return 0, nil
	}
	return -1, nil
}

func (m *memStreams) Del(ctx context.Context, keys ...string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, k := range keys {
		if _, ok := m.kv[k]; ok {
			delete(m.kv, k)
			n++
		}
	}
	return n, nil
}

func (m *memStreams) Close() error { return nil }

func (m *memStreams) streamLen() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries)
}

// ---- boot helpers ----

// bootLightHub boots the daemon with a light-hub profile body and the injected
// in-memory Redis. It returns the harness and the streams.
func bootLightHub(t *testing.T, serverExtra string, mutate func(*memStreams)) (*harness, *memStreams) {
	t.Helper()
	stampBuildForTest(t)
	root := stateBase(t)
	dashPort, ingestPort := freePortPair(t)
	cfgPath := filepath.Join(root, "config.toml")
	envFile := filepath.Join(root, "trouble.env")
	if err := os.WriteFile(envFile, []byte(""), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}
	body := fmt.Sprintf(`state_root = %q

[secrets]
environment_file = %q

[ingest]
bind = "127.0.0.1:%d"
advertised_host = "trouble.example.net"

[dashboard]
bind = "127.0.0.1:%d"

[[projects]]
id = "1"
slug = "trouble-dev"
public_key = "0123456789abcdef0123456789abcdef"
quota_epm = 600
enabled = true

[server]
profile = "light-hub"
hub_id = "c0ffee1234567890"

[server.redis]
url = "redis://127.0.0.1:6379/0"
stream = "trouble:ingest"
group = "ledger-writers"
require_redis = %s

[server.duckbrain]
namespace = "trouble/7f3a91c2d4e5b607"
`, root, envFile, ingestPort, dashPort, serverExtra)

	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	streams := newMemStreams()
	if mutate != nil {
		mutate(streams)
	}

	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{
		cancel:  cancel,
		done:    make(chan error, 1),
		base:    fmt.Sprintf("http://127.0.0.1:%d", dashPort),
		readURL: fmt.Sprintf("http://127.0.0.1:%d", ingestPort),
	}
	ready := make(chan struct{})
	go func() {
		_, err := RunDaemon(ctx, BootOptions{
			Args:       []string{"--config", cfgPath},
			Env:        []string{},
			OnReady:    func(d *Daemon) { h.d = d; close(ready) },
			HubStreams: streams,
		})
		h.done <- err
	}()
	if reached, err := awaitBootReady(t, "light-hub", bootReadyBase, ready, h.done); !reached {
		t.Fatalf("the light-hub daemon did not reach READY: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		awaitDrainBudget(t, "light-hub", bootDrainBase, h.done)
	})
	return h, streams
}

// ---- the tests ----

// TestLightHubBootQueuesIngestionThroughRedis is the acceptance test for
// TRBL-029's first AC: the event is accepted through the Redis stream and the
// ledger gains the record, with the ack after the group-commit fsync.
func TestLightHubBootQueuesIngestionThroughRedis(t *testing.T) {
	h, streams := bootLightHub(t, "false", nil)
	if h.d.Hub == nil || !h.d.Hub.Enabled() {
		t.Fatalf("a validated light-hub boot has no hub runtime")
	}
	if got := h.d.Hub.RuntimeMode(); got != hub.ModeUp {
		t.Fatalf("runtime mode = %q want up", got)
	}

	// The documented on-ramp: POST /api/1/event/?sentry_key=<public_key>.
	projectKey := h.d.Cfg.Projects[0].PublicKey
	evBody := `{"message":"light-hub on-ramp","level":"error","release":"0.1.0"}`
	resp, err := http.Post(h.readURL+"/api/1/event/?sentry_key="+projectKey, "application/json", strings.NewReader(evBody))
	if err != nil {
		t.Fatalf("POST /api/1/event/: %v", err)
	}
	post, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/1/event/ = %d %s, want 200 (the event was queued and appended)", resp.StatusCode, post)
	}
	var posted struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(post, &posted); err != nil || posted.ID == "" {
		t.Fatalf("POST body carried no event id: %v (%s)", err, post)
	}

	// The ledger is the record: the event the sender was answered about is IN it,
	// which is only true because the consumer appended before the door answered.
	deadline := time.Now().Add(3 * time.Second)
	found := false
	for time.Now().Before(deadline) && !found {
		if recs := ledgerEventIDs(t, h.d); recs[posted.ID] {
			found = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !found {
		t.Fatalf("the ledger does not hold the posted event %s: the 200 was not backed by a durable record", posted.ID)
	}
	if streams.streamLen() == 0 {
		t.Fatalf("the event never reached the Redis stream: the profile did not queue it")
	}
	counters := h.d.Hub.IngestCounters()
	// One ingestion request writes more than one record (the sentinel folds the
	// event into its `event` record AND a `group` record, SPEC-04 §3.3), so the
	// count is "at least the POST's own records", and the two things that must be
	// exactly zero are the FALLBACK and any route other than A.
	if counters.Ingested < 2 {
		t.Fatalf("ingested = %d, want at least the event+group records of one POST", counters.Ingested)
	}
	if counters.Fallbacks != 0 {
		t.Fatalf("the standalone fallback served a request while the queue was up")
	}
	if counters.RouteA != counters.Ingested || counters.RouteB != 0 {
		t.Fatalf("route counters = %+v want every ingest on route A (hub-side sensors take A, SPEC-04 §3.10a)", counters)
	}
	// The queue drained: nothing pending, and the acks are counted.
	if st := h.d.Hub.Status(context.Background()); st.Redis.Pending != 0 || st.Redis.LastAckedID == "" {
		t.Fatalf("the stanza does not show a drained, acked queue: %+v", st.Redis)
	}
}

// TestLightHubHealthStanza: /health.json carries the profile's stanza, which is
// the surface a "validated light-hub config still serves standalone" claim was
// missing before.
func TestLightHubHealthStanza(t *testing.T) {
	h, _ := bootLightHub(t, "false", nil)
	code, body := h.anon("/health.json", "application/json")
	if code != http.StatusOK {
		t.Fatalf("GET /health.json = %d %s", code, body)
	}
	var health types.HealthResponse
	if err := json.Unmarshal([]byte(body), &health); err != nil {
		t.Fatalf("health is not a HealthResponse: %v", err)
	}
	if health.Hub == nil {
		t.Fatalf("light-hub health carries no hub stanza: %s", body)
	}
	if !health.Hub.Enabled || health.Hub.Profile != "light-hub" {
		t.Fatalf("hub stanza = %+v want enabled light-hub", health.Hub)
	}
	if health.Hub.Redis.Stream != "trouble:ingest" || health.Hub.Redis.Group != "ledger-writers" {
		t.Fatalf("hub stanza does not name the queue: %+v", health.Hub.Redis)
	}
	if health.Hub.Redis.Consumer == "" {
		t.Fatalf("hub stanza has no consumer (SPEC-13 §3.3: one consumer per state root)")
	}
	if health.Hub.Redis.DedupWindow != "redis" {
		t.Fatalf("dedup_window = %q want redis", health.Hub.Redis.DedupWindow)
	}
	if health.Hub.Degraded {
		t.Fatalf("a healthy queue reported degraded: %+v", health.Hub)
	}
	// The archival tier is unconfigured here (no [server.duckbrain] endpoint),
	// so the queue depth is reported instead of pretending archival works.
	if health.Hub.ArchiveQueue != 0 {
		t.Fatalf("archive_queue = %d want 0 (no generations yet)", health.Hub.ArchiveQueue)
	}
}

// TestLightHubDegradedBootServesTheStandalonePath: Redis unreachable with
// require_redis=false → the daemon starts, answers 200, and SAYS it is degraded
// (§4.3 row one).
func TestLightHubDegradedBootServesTheStandalonePath(t *testing.T) {
	h, _ := bootLightHub(t, "false", func(m *memStreams) {
		m.pingErr = errors.New("dial tcp 127.0.0.1:6379: connect: connection refused")
	})
	if h.d.Hub == nil || !h.d.Hub.Enabled() {
		t.Fatalf("the degraded boot has no hub runtime at all")
	}
	if got := h.d.Hub.RuntimeMode(); got != hub.ModeDegradedBoot {
		t.Fatalf("mode = %q want degraded_boot", got)
	}
	code, body := h.anon("/health.json", "application/json")
	if code != http.StatusOK {
		t.Fatalf("GET /health.json = %d %s", code, body)
	}
	var health types.HealthResponse
	if err := json.Unmarshal([]byte(body), &health); err != nil {
		t.Fatalf("health: %v", err)
	}
	if health.Hub == nil || !health.Hub.Degraded {
		t.Fatalf("the degraded queue is not reported: %+v", health.Hub)
	}
	if health.Hub.DegradedReason != hub.DegradedRedisDown {
		t.Fatalf("degraded reason = %q want %q", health.Hub.DegradedReason, hub.DegradedRedisDown)
	}
	if health.Status != "degraded" {
		t.Fatalf("health status = %q want degraded (SPEC-13 §2.4)", health.Status)
	}

	// Ingestion still works: the standalone in-process path serves, and the
	// ledger (not Redis) holds the record.
	projectKey := h.d.Cfg.Projects[0].PublicKey
	resp, err := http.Post(h.readURL+"/api/1/event/?sentry_key="+projectKey, "application/json",
		strings.NewReader(`{"message":"degraded on-ramp","level":"error"}`))
	if err != nil {
		t.Fatalf("POST during a degraded boot: %v", err)
	}
	post, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST during a degraded boot = %d %s, want 200 (the in-process path serves)", resp.StatusCode, post)
	}
	// The degraded path is COUNTED, not silent (the POST's own records: the
	// sentinel writes an `event` record and a `group` record).
	if got := h.d.Hub.IngestCounters().Fallbacks; got < 1 {
		t.Fatalf("fallbacks = %d want at least one (the degraded path is counted)", got)
	}
	if got := h.d.Hub.IngestCounters().Ingested; got != 0 {
		t.Fatalf("ingested = %d want 0: nothing may be reported as queued while Redis is unreachable", got)
	}
}

// TestLightHubRequireRedisRefusesTheBoot: require_redis=true + an unreachable
// Redis is a boot refusal with TROUBLE-HUB-003 and ZERO HTTP responses.
func TestLightHubRequireRedisRefusesTheBoot(t *testing.T) {
	stampBuildForTest(t)
	root := stateBase(t)
	dashPort, ingestPort := freePortPair(t)
	cfgPath := filepath.Join(root, "config.toml")
	envFile := filepath.Join(root, "trouble.env")
	if err := os.WriteFile(envFile, []byte(""), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}
	body := fmt.Sprintf(`state_root = %q

[secrets]
environment_file = %q

[ingest]
bind = "127.0.0.1:%d"
advertised_host = "trouble.example.net"

[dashboard]
bind = "127.0.0.1:%d"

[server]
profile = "light-hub"

[server.redis]
url = "redis://127.0.0.1:6379/0"
require_redis = true

[server.duckbrain]
namespace = "trouble/7f3a91c2d4e5b607"
`, root, envFile, ingestPort, dashPort)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	streams := newMemStreams()
	streams.pingErr = errors.New("dial tcp 127.0.0.1:6379: connect: connection refused")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	var booted bool
	done := make(chan error, 1)
	go func() {
		_, err := RunDaemon(ctx, BootOptions{
			Args:       []string{"--config", cfgPath},
			Env:        []string{},
			OnReady:    func(d *Daemon) { booted = true; close(ready) },
			HubStreams: streams,
		})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("require_redis=true with an unreachable Redis booted successfully")
		}
		if !strings.Contains(err.Error(), string(types.CodeHub003)) {
			t.Fatalf("boot refusal = %v, want %s", err, types.CodeHub003)
		}
	case <-time.After(bootReadyBase):
		t.Fatalf("the refused boot neither returned nor reached READY")
	}
	if booted {
		t.Fatalf("the daemon signalled READY although Redis was required and unreachable")
	}
	// Zero HTTP responses: neither port was ever served.
	for _, port := range []int{ingestPort, dashPort} {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/health.json", port))
		if err == nil {
			resp.Body.Close()
			t.Fatalf("port %d answered after a refused boot", port)
		}
	}
	// And the refusal is auditable with the hub's own code.
	if !scanLedgerForBootRefusal(t, root, types.CodeHub003) {
		t.Fatalf("no boot_refused record carrying %s in %s", types.CodeHub003, root)
	}
}

// TestStandaloneBootHasNoHubRuntimeOrStanza is the standalone invariant: the
// default profile builds nothing, reports no stanza, and creates no hub state.
func TestStandaloneBootHasNoHubRuntimeOrStanza(t *testing.T) {
	h := bootDaemon(t)
	if h.d.Hub != nil {
		t.Fatalf("the standalone boot built a hub runtime")
	}
	code, body := h.anon("/health.json", "application/json")
	if code != http.StatusOK {
		t.Fatalf("GET /health.json = %d %s", code, body)
	}
	if strings.Contains(body, `"hub"`) {
		t.Fatalf("the standalone health surface carries a hub stanza: %s", body)
	}
	if _, err := os.Stat(filepath.Join(h.d.Cfg.StateRoot, "hub")); !os.IsNotExist(err) {
		t.Fatalf("the standalone boot created the hub state tree: %v", err)
	}
}

// ledgerEventIDs reads the ledger for the event ids it holds (the on-ramp's own
// id field), which is the only place a "the 200 was backed by a record" claim can
// be checked.
func ledgerEventIDs(t *testing.T, d *Daemon) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	if err := d.Ledger.Query().ScanFrom(0, func(rec types.Record) bool {
		if rec.Kind != types.KEvent {
			return true
		}
		ev, ok := rec.Payload["event"].(map[string]any)
		if !ok {
			return true
		}
		if id, _ := ev["id"].(string); id != "" {
			out[id] = true
		}
		return true
	}); err != nil {
		t.Fatalf("ledger scan: %v", err)
	}
	return out
}
