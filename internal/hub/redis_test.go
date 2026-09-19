package hub

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// redis_test.go pins SPEC-13 §7's redis_test row: EnsureGroup twice (BUSYGROUP is
// success, not an error), a non-BUSYGROUP group refusal → 005, an auth failure →
// 002, a dial failure → 003, and the deployment preflight (015 cluster, 016
// persistence-less Redis, policy as a warning that surfaces instead of refusing).

func testRedisCfg() RedisConfig {
	return RedisConfig{
		URL:                "redis://127.0.0.1:6379/0",
		Stream:             "trouble:ingest",
		Group:              "ledger-writers",
		HostID:             "7f3a91c2d4e5b607",
		RequirePersistence: true,
		CheckPolicy:        true,
	}.WithDefaults()
}

func openFake(t *testing.T, f *fakeStreams, mutate func(*RedisConfig)) *Client {
	t.Helper()
	cfg := testRedisCfg()
	if mutate != nil {
		mutate(&cfg)
	}
	c, err := OpenWith(context.Background(), cfg, f)
	if err != nil {
		t.Fatalf("OpenWith: %v", err)
	}
	return c
}

func TestEnsureGroupBusyGroupIsSuccess(t *testing.T) {
	f := newFakeStreams()
	f.info = ServerInfo{AOFEnabled: true, Policy: "noeviction", OptionsChecked: true}
	c := openFake(t, f, nil)
	if err := c.EnsureGroup(context.Background()); err != nil {
		t.Fatalf("first EnsureGroup: %v", err)
	}
	// The second call answers BUSYGROUP, which SPEC-13 §3.3 pins as success.
	if err := c.EnsureGroup(context.Background()); err != nil {
		t.Fatalf("second EnsureGroup must treat BUSYGROUP as success: %v", err)
	}
	if f.groupCreateCalled != 2 {
		t.Fatalf("XGROUP CREATE calls = %d want 2", f.groupCreateCalled)
	}
	if _, ok := f.groups["trouble:ingest"]["ledger-writers"]; !ok {
		t.Fatalf("the group does not exist after both calls")
	}
}

func TestEnsureGroupRefusalIsFive(t *testing.T) {
	f := newFakeStreams()
	f.info = ServerInfo{AOFEnabled: true, Policy: "noeviction", OptionsChecked: true}
	f.failXGroupCreate = errors.New("WRONGTYPE Operation against a key holding the wrong kind of value")
	c := openFake(t, f, nil)
	err := c.EnsureGroup(context.Background())
	if CodeOf(err) != types.CodeHub005 {
		t.Fatalf("code = %q want TROUBLE-HUB-005 (%v)", CodeOf(err), err)
	}
	var he *Error
	if !errors.As(err, &he) || he.Class() != types.ErrClassPermanent {
		t.Fatalf("005 must be permanent")
	}
}

func TestOpenClassifiesAuthAndDialFailures(t *testing.T) {
	t.Run("auth", func(t *testing.T) {
		f := newFakeStreams()
		f.pingErr = errors.New("WRONGPASS invalid username-password pair")
		_, err := OpenWith(context.Background(), testRedisCfg(), f)
		if CodeOf(err) != types.CodeHub002 {
			t.Fatalf("code = %q want TROUBLE-HUB-002 (%v)", CodeOf(err), err)
		}
		if ReasonOf(err) != ReasonRedisAuth {
			t.Fatalf("reason = %q want %q", ReasonOf(err), ReasonRedisAuth)
		}
	})
	t.Run("dial", func(t *testing.T) {
		f := newFakeStreams()
		f.pingErr = errors.New("dial tcp 127.0.0.1:6379: connect: connection refused")
		_, err := OpenWith(context.Background(), testRedisCfg(), f)
		if CodeOf(err) != types.CodeHub003 {
			t.Fatalf("code = %q want TROUBLE-HUB-003 (%v)", CodeOf(err), err)
		}
		var he *Error
		if !errors.As(err, &he) || !he.Transient() {
			t.Fatalf("003 must be transient")
		}
	})
}

func TestPreflightRefusesClusterAndPersistencelessRedis(t *testing.T) {
	t.Run("cluster", func(t *testing.T) {
		f := newFakeStreams()
		f.info = ServerInfo{AOFEnabled: true, Policy: "noeviction", ClusterEnabled: true, OptionsChecked: true}
		_, err := OpenWith(context.Background(), testRedisCfg(), f)
		if CodeOf(err) != types.CodeHub015 {
			t.Fatalf("code = %q want TROUBLE-HUB-015 (%v)", CodeOf(err), err)
		}
	})
	t.Run("appendonly=no with require_persistence", func(t *testing.T) {
		f := newFakeStreams()
		f.info = ServerInfo{AOFEnabled: false, Policy: "noeviction", OptionsChecked: true}
		_, err := OpenWith(context.Background(), testRedisCfg(), f)
		if CodeOf(err) != types.CodeHub016 {
			t.Fatalf("code = %q want TROUBLE-HUB-016 (%v)", CodeOf(err), err)
		}
	})
	t.Run("appendonly=no without require_persistence is a warning", func(t *testing.T) {
		f := newFakeStreams()
		f.info = ServerInfo{AOFEnabled: false, Policy: "allkeys-lru", EvictedKeys: 12, OptionsChecked: true}
		cfg := testRedisCfg()
		cfg.RequirePersistence = false
		c, err := OpenWith(context.Background(), cfg, f)
		if err != nil {
			t.Fatalf("OpenWith must not refuse a deliberate non-persistent Redis: %v", err)
		}
		warns := c.PolicyWarnings()
		if len(warns) != 3 {
			t.Fatalf("warnings = %v want three (policy, evicted_keys, appendonly)", warns)
		}
		joined := strings.Join(warns, " | ")
		for _, want := range []string{"maxmemory-policy=allkeys-lru", "evicted_keys=12", "appendonly=no"} {
			if !strings.Contains(joined, want) {
				t.Fatalf("warnings %q missing %q", joined, want)
			}
		}
	})
	t.Run("INFO unavailable leaves the facts unknown", func(t *testing.T) {
		f := newFakeStreams()
		f.info = ServerInfo{OptionsChecked: false}
		c, err := OpenWith(context.Background(), testRedisCfg(), f)
		if err != nil {
			t.Fatalf("a managed Redis without INFO rights must not refuse the boot: %v", err)
		}
		if c.ServerInfo().OptionsChecked {
			t.Fatalf("OptionsChecked must stay false when INFO did not answer")
		}
		if len(c.PolicyWarnings()) != 0 {
			t.Fatalf("unknown facts must not produce warnings: %v", c.PolicyWarnings())
		}
	})
}

func TestEnqueueWritesOneFieldEntryAndCounts(t *testing.T) {
	f := newFakeStreams()
	f.info = ServerInfo{AOFEnabled: true, Policy: "noeviction", OptionsChecked: true}
	c := openFake(t, f, nil)
	if err := c.EnsureGroup(context.Background()); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	draft := draftFor(types.KEvent, "sentinel:sha256v1:9f2c1d3e4b5a6c7d", "sentinel:payment-worker", map[string]any{"subject": "q"})
	rec := localRecord(draft, "7f3a91c2d4e5b607", "")
	env := types.ForwardEnvelope{
		ProtocolVersion: 1,
		IdempotencyKey:  "sentinel:sha256v1:9f2c1d3e4b5a6c7d|1|7f3a91c2d4e5b607",
		HostID:          "7f3a91c2d4e5b607",
		Origin:          rec.Origin,
		Records:         []types.Record{rec},
	}
	ms, err := Enqueue(context.Background(), c, env, RouteA)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if ms == 0 {
		t.Fatalf("Enqueue returned a zero millisecond component")
	}
	if c.Counters.Enqueued.Load() != 1 {
		t.Fatalf("Enqueued = %d want 1", c.Counters.Enqueued.Load())
	}
	ents := f.entries("trouble:ingest")
	if len(ents) != 1 {
		t.Fatalf("stream holds %d entries want 1", len(ents))
	}
	if _, ok := ents[0].fields[StreamField]; !ok {
		t.Fatalf("entry does not carry the single %q field: %v", StreamField, ents[0].fields)
	}
	if len(ents[0].fields) != 1 {
		t.Fatalf("entry carries %d fields, want exactly 1 (SPEC-13 §3.2)", len(ents[0].fields))
	}
	// The encoded envelope round-trips through the decoder.
	dec, err := DecodeEntry(ents[0].fields, testRedisCfg())
	if err != nil {
		t.Fatalf("DecodeEntry: %v", err)
	}
	if dec.IdempotencyKey != env.IdempotencyKey || dec.Route != RouteA || dec.EnqueuedTS == "" {
		t.Fatalf("round trip lost the envelope's own facts: %+v", dec)
	}
}

func TestEnqueueFailureIsFourAndLeavesNoEntry(t *testing.T) {
	f := newFakeStreams()
	f.info = ServerInfo{AOFEnabled: true, Policy: "noeviction", OptionsChecked: true}
	c := openFake(t, f, nil)
	draft := draftFor(types.KEvent, "sentinel:sha256v1:9f2c1d3e4b5a6c7d", "sentinel", nil)
	rec := localRecord(draft, "h1", "")
	f.failXAdd = errors.New("READONLY You can't write against a read only replica")
	_, err := c.EnqueueEntry(context.Background(), types.ForwardEnvelope{
		ProtocolVersion: 1,
		IdempotencyKey:  "k",
		HostID:          "h1",
		Origin:          rec.Origin,
		Records:         []types.Record{rec},
	}, RouteA)
	if CodeOf(err) != types.CodeHub004 {
		t.Fatalf("code = %q want TROUBLE-HUB-004 (%v)", CodeOf(err), err)
	}
	if got := len(f.entries("trouble:ingest")); got != 0 {
		t.Fatalf("a failed XADD left %d entries in the stream", got)
	}
	if c.Counters.EnqueueFailed.Load() != 1 {
		t.Fatalf("EnqueueFailed = %d want 1", c.Counters.EnqueueFailed.Load())
	}
}

// TestOverloadedLagGate pins the §4.2 backpressure trigger: the lag gate, not
// Redis-side eviction, is what bounds the queue.
func TestOverloadedLagGate(t *testing.T) {
	f := newFakeStreams()
	f.info = ServerInfo{AOFEnabled: true, Policy: "noeviction", OptionsChecked: true}
	c := openFake(t, f, func(cfg *RedisConfig) { cfg.MaxLen = 10 })
	if err := c.EnsureGroup(context.Background()); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	for i := 0; i < 6; i++ {
		ent := draftFor(types.KEvent, "sentinel:sha256v1:9f2c1d3e4b5a6c7d", "s", map[string]any{"i": i})
		rec := localRecord(ent, "h1", "")
		if _, err := c.EnqueueEntry(context.Background(), types.ForwardEnvelope{
			ProtocolVersion: 1, IdempotencyKey: "k" + itoa(int64(i)), HostID: "h1",
			Origin: rec.Origin, Records: []types.Record{rec},
		}, RouteA); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	over, reason := c.Overloaded(ctx)
	if !over {
		t.Fatalf("lag gate did not engage at 6 entries with maxlen 10 (lag/2 = 5)")
	}
	if reason != "lag_over_half_maxlen" {
		t.Fatalf("reason = %q want lag_over_half_maxlen", reason)
	}
}
