package flow

// replay_seal_test.go — SPEC-08 §3.9b: a REPLAYED dispatch carries the payload
// the entry was WRITTEN with, not one re-derived from live config and board state.
//
// The defect these tests pin: `DispatchSpawn` rebuilds the whole §3.6 wire payload
// at replay time from the current config and board state, so an entry written
// before a config or board change is re-dispatched carrying NEW secondary fields —
// board path, priority, complexity, capability tags, severity, host id — while
// `idem_key`/`task_id` stay stable. The router still dedups (no duplicate spawn),
// so nothing looks wrong: only the evidence trail mutates, and a ledger reader then
// sees a dispatch whose board path/priority/severity are not what the incident that
// produced it actually requested.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// bodyLog records every `/dispatch` body the router received, in order, and lets
// the test flip the status between the original attempt and the replay.
type bodyLog struct {
	mu     sync.Mutex
	bodies []map[string]any
	status int
}

func (l *bodyLog) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var p map[string]any
		_ = json.Unmarshal(b, &p)
		l.mu.Lock()
		l.bodies = append(l.bodies, p)
		st := l.status
		l.mu.Unlock()
		w.WriteHeader(st)
	}
}

func (l *bodyLog) setStatus(st int) {
	l.mu.Lock()
	l.status = st
	l.mu.Unlock()
}

func (l *bodyLog) at(i int) map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.bodies[i]
}

func (l *bodyLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.bodies)
}

// jsonCanon renders a payload the way the ledger compares it: encoding/json sorts
// map keys, so two payloads are byte-equal here iff every field matches.
func jsonCanon(t *testing.T, m map[string]any) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return string(b)
}

// driftLine renders the field-by-field difference between two wire payloads, one
// line per differing field — the shape the RED run prints.
func driftLine(t *testing.T, before, after map[string]any) string {
	t.Helper()
	keys := map[string]bool{}
	for k := range before {
		keys[k] = true
	}
	for k := range after {
		keys[k] = true
	}
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	var out []string
	for _, k := range names {
		a, _ := json.Marshal(before[k])
		b, _ := json.Marshal(after[k])
		if string(a) != string(b) {
			out = append(out, fmt.Sprintf("%s: %s -> %s", k, a, b))
		}
	}
	if len(out) == 0 {
		return "(identical)"
	}
	return strings.Join(out, "; ")
}

// deliveredField reads one field out of a replay record's delivered field set,
// tolerating both the in-process map and a JSON-round-tripped one.
func deliveredField(t *testing.T, rec map[string]any, key string) string {
	t.Helper()
	switch m := rec["delivered"].(type) {
	case map[string]string:
		if v, ok := m[key]; ok {
			return v
		}
	case map[string]any:
		if v, ok := m[key]; ok {
			return fmt.Sprint(v)
		}
	}
	t.Fatalf("the replay record carries no delivered field set (%v): %v", key, rec)
	return ""
}

// namesOf flattens a ledger field that may be a []string or a JSON []any.
func namesOf(t *testing.T, rec map[string]any, key string) []string {
	t.Helper()
	switch v := rec[key].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			out = append(out, fmt.Sprint(e))
		}
		return out
	}
	t.Fatalf("the replay record carries no %s list: %v", key, rec)
	return nil
}

// TestReplayDeliversTheSealedPayloadNotTheLiveRederivation is the reproduction:
// one §3.6 dispatch fails and is parked; the project's board moves and the daemon
// reports another host id while the entry sits in the queue; the replay then
// delivers the payload the entry was WRITTEN with, byte for byte, and the ledger
// shows the divergence from what a live re-derivation would have produced.
func TestReplayDeliversTheSealedPayloadNotTheLiveRederivation(t *testing.T) {
	ctx := context.Background()
	log := &bodyLog{status: 500}
	srv := schedulerStub(t, log.handler())
	defer srv.Close()

	fx := newFixture(t, func(c *types.FlowConfig) {
		c.Router = types.RouterConfig{Mode: "http", Endpoint: srv.URL, DispatchPath: "/dispatch", Retries: 0}
	})
	// applyFlowDefaults turns a declared `Retries = 0` into the 3-retry default;
	// this test wants exactly one immediate attempt (no 1 s/3 s/9 s backoff), so the
	// resolved config is adjusted after construction.
	fx.flow.cfg.Router.Retries = 0
	sp := fx.withQueue(t, filepath.Join(t.TempDir(), "q"), SpoolBounds{})

	// The original attempt: a row whose secondary fields come from the filing path
	// (priority, complexity, capability tags), dispatched at the incident's own
	// severity — the shape `taskRouterDriver.FileTask` and the §3.6 healthcheck use.
	row := types.BoardRow{
		ID: "tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ", Sig: "sig:sealed", Inc: "inc_sealed",
		Repo: fx.repo, Title: "hotfix: sealed payload", Priority: "P3", Complexity: "S",
		CapabilityTags: []string{"docs", "infra"},
	}
	if _, err := fx.flow.dispatch(ctx, row, types.SevLow); err == nil {
		t.Fatalf("a 500 must not read as delivered")
	}
	if sp.Count() != 1 {
		t.Fatalf("queue depth after the failed attempt = %d, want 1 (the §3.6 retries exhausted spool ONE entry)", sp.Count())
	}
	if log.count() != 1 {
		t.Fatalf("router saw %d attempts, want 1 (Retries=0)", log.count())
	}

	// The live inputs the replay derives from change while the entry is parked:
	// the project's board moves, and the daemon reports a different host.
	moved := filepath.Join(t.TempDir(), ".board-moved")
	fx.flow.cfg.Projects["payment-api"] = setBoard(fx.flow.cfg.Projects["payment-api"], moved)
	d := fx.flow.deps
	d.Actors.HostID = "0011223344556677"
	fx.flow.SetDeps(d)

	log.setStatus(200)
	fx.clock.Advance(6 * time.Second)
	n, err := fx.flow.Replay(ctx, 0)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if n != 1 {
		t.Fatalf("replayed = %d, want 1", n)
	}
	if log.count() != 2 {
		t.Fatalf("router saw %d dispatches, want 2 (the failed attempt + the replay)", log.count())
	}

	sealed, delivered := log.at(0), log.at(1)
	if jsonCanon(t, sealed) != jsonCanon(t, delivered) {
		t.Fatalf("the replayed dispatch does not carry the payload it was written with\n"+
			"  requested (original attempt): %s\n"+
			"  delivered (replay):           %s\n"+
			"  drift: %s", jsonCanon(t, sealed), jsonCanon(t, delivered), driftLine(t, sealed, delivered))
	}
	// Idempotency is untouched: the router's dedup key is stable across the replay.
	mustEqual(t, delivered["idem_key"], sealed["idem_key"], "replay idem_key")
	mustEqual(t, delivered["task_id"], delivered["idem_key"], "replay task_id vs idem_key")
	mustEqual(t, delivered["idem_key"], "tsk_01J9Z6Q0M2X4T8V1K7B3N5R8WJ", "replay idem_key value")

	// Every secondary field is the SEALED one, not the live one.
	mustEqual(t, delivered["board_path"], fx.board, "delivered board_path (the board the entry was written for)")
	mustEqual(t, delivered["priority"], "P3", "delivered priority")
	mustEqual(t, delivered["complexity"], "S", "delivered complexity")
	mustEqual(t, delivered["severity"], "low", "delivered severity")
	mustEqual(t, delivered["host_id"], "7f3a91c2d4e5b607", "delivered host_id")
	mustEqual(t, fmt.Sprint(delivered["capability_tags"]), "[docs infra]", "delivered capability_tags")

	// And the ledger says what was delivered AND where the live inputs have moved:
	// the mutation is visible instead of silent.
	last := fx.rec.last(types.KFlow)
	mustEqual(t, last["dispatch_state"], "replayed", "replay dispatch_state")
	mustEqual(t, last["payload_origin"], "sealed", "payload_origin")
	mustEqual(t, deliveredField(t, last, "board_path"), fx.board, "recorded delivered board_path")
	mustEqual(t, deliveredField(t, last, "priority"), "P3", "recorded delivered priority")
	mustEqual(t, deliveredField(t, last, "severity"), "low", "recorded delivered severity")
	if last["payload_sha256"] == nil || last["payload_sha256"] == "" {
		t.Fatalf("the replay record must pin the delivered bytes: %v", last)
	}
	diverged := namesOf(t, last, "diverged_fields")
	joined := strings.Join(diverged, ",")
	for _, want := range []string{"board_path", "priority", "complexity", "capability_tags", "severity", "host_id"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("diverged_fields = %v, want it to name %q (the live inputs DID move; a replay must say so)", diverged, want)
		}
	}

	// Dedup intact: a second drain dispatches nothing — the first replay already
	// placed the work, which is the property `idem_key` exists to keep.
	n2, err := fx.flow.Replay(ctx, 0)
	if err != nil {
		t.Fatalf("second Replay: %v", err)
	}
	if n2 != 0 || log.count() != 2 {
		t.Fatalf("second drain replayed %d entries and the router saw %d dispatches, want 0 and 2", n2, log.count())
	}
}

// TestSealedEnvelopeWithoutIdentityIsCorruptNotReDerived pins the format
// discriminator: the `sealed` marker decides which shape an entry IS, so an
// envelope that has lost its identity is a corrupt entry — never a payload quietly
// re-derived as if it were a bare request, which is the same silent mutation in a
// different disguise.
func TestSealedEnvelopeWithoutIdentityIsCorruptNotReDerived(t *testing.T) {
	ctx := context.Background()
	log := &bodyLog{status: 200}
	srv := schedulerStub(t, log.handler())
	defer srv.Close()

	fx := newFixture(t, func(c *types.FlowConfig) {
		c.Router = types.RouterConfig{Mode: "http", Endpoint: srv.URL, DispatchPath: "/dispatch", Retries: 0}
	})
	sp := fx.withQueue(t, filepath.Join(t.TempDir(), "q"), SpoolBounds{})
	if _, err := sp.Put(types.SpoolEntry{
		ID: types.NewID(types.PEv), TS: types.FormatUTC(fx.clock.Now()), Kind: types.SpoolSpawn,
		Payload: []byte(`{"sealed":true,"source":"dispatch"}`), IdemKey: "tsk_identityless",
		NextTryTS: types.FormatUTC(fx.clock.Now()),
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := fx.flow.Replay(ctx, 0); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if log.count() != 0 {
		t.Fatalf("router saw %d dispatches for an identity-less envelope, want 0", log.count())
	}
	if sp.Count() != 0 {
		t.Fatalf("queue depth = %d, want 0 (a corrupt entry is dropped, not looped)", sp.Count())
	}
	last := fx.rec.last(types.KFlow)
	mustEqual(t, last["drop_reason"], "corrupt", "drop_reason")
}

// TestLegacySpoolEntryReDerivationIsRecordedNotSilent is the upgrade path: an
// entry written by a build BEFORE §3.9b carries the marshalled `SpawnRequest`,
// so the only payload a replay can produce for it is re-derived from live state.
// The dispatch must not be lost, and the re-derivation must be named in the record
// rather than passed off as a delivered-as-written payload.
func TestLegacySpoolEntryReDerivationIsRecordedNotSilent(t *testing.T) {
	ctx := context.Background()
	log := &bodyLog{status: 200}
	srv := schedulerStub(t, log.handler())
	defer srv.Close()

	fx := newFixture(t, func(c *types.FlowConfig) {
		c.Router = types.RouterConfig{Mode: "http", Endpoint: srv.URL, DispatchPath: "/dispatch", Retries: 0}
	})
	sp := fx.withQueue(t, filepath.Join(t.TempDir(), "q"), SpoolBounds{})

	req := fx.spawnReq("")
	if _, err := sp.Put(types.SpoolEntry{
		ID: types.NewID(types.PEv), TS: types.FormatUTC(fx.clock.Now()), Kind: types.SpoolSpawn,
		Payload: mustMarshalSpawn(t, req), IdemKey: req.TaskID,
		NextTryTS: types.FormatUTC(fx.clock.Now()),
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	fx.clock.Advance(6 * time.Second)
	n, err := fx.flow.Replay(ctx, 0)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if n != 1 {
		t.Fatalf("replayed = %d, want 1 — an upgrade must not silently lose a queued dispatch", n)
	}
	if sp.Count() != 0 {
		t.Fatalf("queue depth = %d, want 0", sp.Count())
	}
	body := log.at(0)
	mustEqual(t, body["idem_key"], req.TaskID, "legacy replay idem_key")

	last := fx.rec.last(types.KFlow)
	mustEqual(t, last["dispatch_state"], "replayed", "legacy replay dispatch_state")
	mustEqual(t, last["payload_origin"], "rederived", "legacy replay payload_origin")
	fields := namesOf(t, last, "rederived_fields")
	joined := strings.Join(fields, ",")
	for _, want := range []string{"board_path", "priority", "severity", "host_id"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("rederived_fields = %v, want it to name %q", fields, want)
		}
	}
}
