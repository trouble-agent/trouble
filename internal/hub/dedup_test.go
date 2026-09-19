package hub

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// dedup_test.go pins SPEC-13 §3.4 (the gate: fresh → present, TTL expiry
// re-opens, Redis-idle fallback to the bounded LRU) and §3.3's consequence (a
// re-delivery under a surviving claim appends nothing).

func newGate(t *testing.T, f *fakeStreams) (*DedupGate, *Client) {
	t.Helper()
	c := openFake(t, f, nil)
	return NewDedupGate(c.Config(), c), c
}

func TestDedupClaimFreshThenPresent(t *testing.T) {
	f := newFakeStreams()
	f.info = ServerInfo{AOFEnabled: true, Policy: "noeviction", OptionsChecked: true}
	gate, c := newGate(t, f)
	ctx := context.Background()
	key := DedupKey(c.Config().DedupPfx, "sentinel:sha256v1:9f2c1d3e4b5a6c7d", types.NormVersionV1, "7f3a91c2d4e5b607")

	fresh, err := gate.Claim(ctx, key)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !fresh {
		t.Fatalf("first claim must be fresh")
	}
	fresh, err = gate.Claim(ctx, key)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if fresh {
		t.Fatalf("second claim must be a hit (the key is present)")
	}
	state, err := gate.State(ctx, key)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if state != DedupEnqueued {
		t.Fatalf("state = %v want enqueued (the door claimed it, not the consumer)", state)
	}
	gate.MarkAppended(ctx, key)
	state, err = gate.State(ctx, key)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if state != DedupAppended {
		t.Fatalf("state = %v want appended", state)
	}
	if got := gate.Window(); got != "redis" {
		t.Fatalf("window = %q want redis", got)
	}
	if gate.LRUSize() == 0 {
		t.Fatalf("the LRU mirror did not record the claim")
	}
}

// TestDedupTTLExpiryReopensTheWindow is the §3.4 sentence "a replay older than
// 24h re-enters", reduced to its mechanism (the TTL).
func TestDedupTTLExpiryReopensTheWindow(t *testing.T) {
	f := newFakeStreams()
	f.info = ServerInfo{AOFEnabled: true, Policy: "noeviction", OptionsChecked: true}
	gate, c := newGate(t, f)
	ctx := context.Background()
	key := DedupKey(c.Config().DedupPfx, "sig", 1, "h1")
	if fresh, _ := gate.Claim(ctx, key); !fresh {
		t.Fatalf("first claim must be fresh")
	}
	f.expireKey(key)
	if fresh, _ := gate.Claim(ctx, key); !fresh {
		t.Fatalf("after the TTL expired the key must re-enter (fresh again)")
	}
}

func TestDedupGateFallsBackToTheBoundedLRU(t *testing.T) {
	f := newFakeStreams()
	f.info = ServerInfo{AOFEnabled: true, Policy: "noeviction", OptionsChecked: true}
	gate, c := newGate(t, f)
	ctx := context.Background()
	key := DedupKey(c.Config().DedupPfx, "sig", 1, "h1")

	// The gate the door uses still works while Redis refuses SET NX.
	f.failSetNX = errors.New("READONLY You can't write against a read only replica")
	if fresh, err := gate.Claim(ctx, key); err != nil || !fresh {
		t.Fatalf("degraded claim: fresh=%v err=%v (the LRU must answer)", fresh, err)
	}
	if gate.Window() != "lru" {
		t.Fatalf("window = %q want lru while the gate is degraded", gate.Window())
	}
	if !gate.Degraded() {
		t.Fatalf("gate does not report itself degraded")
	}
	if fresh, _ := gate.Claim(ctx, key); fresh {
		t.Fatalf("the LRU must answer the second claim as a hit")
	}
	// One counter increment per degradation WINDOW, not per failing call.
	if _, _, _, windows := gate.Counters(); windows != 1 {
		t.Fatalf("degraded windows = %d want 1 (TROUBLE-HUB-006 is once per window)", windows)
	}
	if state, _ := gate.State(ctx, key); state != DedupEnqueued {
		t.Fatalf("LRU state = %v want enqueued", state)
	}
	gate.MarkAppended(ctx, key)
	if state, _ := gate.State(ctx, key); state != DedupAppended {
		t.Fatalf("LRU state after append = %v want appended", state)
	}

	// Redis returns: the window flips back and the counter stops growing.
	f.failSetNX = nil
	key2 := DedupKey(c.Config().DedupPfx, "sig2", 1, "h1")
	if fresh, err := gate.Claim(ctx, key2); err != nil || !fresh {
		t.Fatalf("claim after recovery: fresh=%v err=%v", fresh, err)
	}
	if gate.Window() != "redis" {
		t.Fatalf("window = %q want redis after a successful op", gate.Window())
	}
	if _, _, _, windows := gate.Counters(); windows != 1 {
		t.Fatalf("degraded windows = %d want 1 (no new window after recovery)", windows)
	}
}

// TestDedupFailoverLosingTheClaimCostsOneDuplicate is §3.4's failover case: the
// claim is gone, so the entry is re-appended once and the gate COUNTS the
// duplicate instead of hiding it.
func TestDedupFailoverLosingTheClaimCostsOneDuplicate(t *testing.T) {
	f := newFakeStreams()
	f.info = ServerInfo{AOFEnabled: true, Policy: "noeviction", OptionsChecked: true}
	events := &eventLog{}
	ledger := newFakeLedger(events)
	gate, c := newGate(t, f)
	cs := NewConsumer(c, ledger, gate, c.Config())
	ctx := context.Background()

	draft := draftFor(types.KEvent, "sentinel:sha256v1:9f2c1d3e4b5a6c7d", "sentinel", nil)
	id := entryFor(t, f, draft, RouteA, "h1")
	ent := f.entries("trouble:ingest")[0]
	key := idemKeyOf(t, draft, "h1")

	// Failover: the claim is gone (asynchronous replication loss).
	if f.has(key) {
		t.Fatalf("precondition: the claim must not exist yet")
	}
	if _, err := cs.handleEntry(ctx, StreamEntry{ID: ent.id, Fields: ent.fields}); err != nil {
		t.Fatalf("handleEntry: %v", err)
	}
	if _, _, conflicts, _ := gate.Counters(); conflicts != 1 {
		t.Fatalf("dedup_conflicts = %d want 1 (the duplicate is counted, SPEC-13 §3.4)", conflicts)
	}
	if ledger.Count() != 1 {
		t.Fatalf("ledger records = %d want 1", ledger.Count())
	}
	if !f.has(key) {
		t.Fatalf("the appended claim was not written")
	}
	_ = id
}

func TestDedupKeyDerivation(t *testing.T) {
	key := DedupKey("trouble:dedup:", "sentinel:sha256v1:9f2c1d3e4b5a6c7d", 1, "7f3a91c2d4e5b607")
	want := "trouble:dedup:sentinel:sha256v1:9f2c1d3e4b5a6c7d|1|7f3a91c2d4e5b607"
	if key != want {
		t.Fatalf("key = %q want %q", key, want)
	}
	got, ok := IdemKeyForSig("sentinel:sha256v1:9f2c1d3e4b5a6c7d", "h1")
	if !ok || got != "sentinel:sha256v1:9f2c1d3e4b5a6c7d|"+"1"+"|h1" {
		t.Fatalf("IdemKeyForSig = %q ok=%v", got, ok)
	}
	if _, ok := IdemKeyForSig("not-a-sig", "h1"); ok {
		t.Fatalf("an unparsable sig must not mint a norm_version")
	}
	a := ContentIdemKey(types.KGap, []byte(`{"a":1}`))
	b := ContentIdemKey(types.KGap, []byte(`{"a":1}`))
	c2 := ContentIdemKey(types.KGap, []byte(`{"a":2}`))
	if a != b || a == c2 {
		t.Fatalf("content idem key is not content-derived: %q %q %q", a, b, c2)
	}
}

func TestDedupStateFileRoundTrip(t *testing.T) {
	root := t.TempDir()
	gate := NewDedupGate(testRedisCfg(), nil) // LRU-only: the standalone/degraded shape
	gate.Restore([]string{"k1", "k2"})
	if gate.LRUSize() != 2 {
		t.Fatalf("LRU size = %d want 2", gate.LRUSize())
	}
	if err := gate.SaveState(root); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	other := NewDedupGate(testRedisCfg(), nil)
	st, err := other.LoadState(root)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if st.LRUSize != 2 || st.RestoredKeys != 2 {
		t.Fatalf("state = %+v want lru_size 2 restored_keys 2", st)
	}
	if filepath.Base(DedupStatePath(root)) != "dedup.state" {
		t.Fatalf("dedup state path = %q", DedupStatePath(root))
	}
}

func idemKeyOf(t *testing.T, draft types.RecordDraft, host string) string {
	t.Helper()
	key, err := IdemKeyForDraft(draft, host)
	if err != nil {
		t.Fatalf("idemKey: %v", err)
	}
	return key
}

var _ = time.Second
