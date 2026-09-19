package hub

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// archive_test.go pins SPEC-13 §7's archive row: the plan/export/verify/mark/drop
// cycle, a read-back mismatch (012) that leaves the generation undroppable, an
// unclosed generation (011), a DuckBrain outage (010) with a growing queue, the
// content-derived marker id (a re-export is ONE object), and the 014 drop gate.

func writeGeneration(t *testing.T, dir, name string, seqs ...uint64) string {
	t.Helper()
	path := filepath.Join(dir, name)
	var buf bytes.Buffer
	for i, seq := range seqs {
		rec := map[string]any{
			"rec_id": fmt.Sprintf("ev_%024d", seq),
			"seq":    seq,
			"ts":     fmt.Sprintf("2026-09-16T00:00:%02d.000Z", i),
			"kind":   "event",
			"origin": map[string]any{"host_id": "7f3a91c2d4e5b607", "source": "sentinel"},
			"actor":  map[string]any{"kind": "daemon", "id": "troubled"},
			"payload": map[string]any{
				"subject": "x",
			},
		}
		b, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("marshal record: %v", err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func archiveCfg(t *testing.T, ledgerRoot string, target Target) (ArchiveConfig, string) {
	t.Helper()
	root := t.TempDir()
	cfg := ArchiveConfig{
		Enabled:            true,
		Namespace:          "trouble/7f3a91c2d4e5b607",
		Endpoint:           "http://127.0.0.1:7645",
		Interval:           time.Hour,
		BatchFiles:         8,
		KeepLocalGens:      0,
		VerifyAfterWriting: true,
		Gzip:               true,
		StateRoot:          root,
		LedgerRoot:         ledgerRoot,
		LiveFile:           "2026-09-17.jsonl",
	}
	_ = target
	return cfg, root
}

func TestPlanArchiveListsClosedGenerations(t *testing.T) {
	ledgerRoot := t.TempDir()
	writeGeneration(t, ledgerRoot, "2026-09-15.jsonl", 1, 2)
	p := writeGeneration(t, ledgerRoot, "2026-09-16.1.gen.jsonl", 3, 4, 5)
	writeGeneration(t, ledgerRoot, "2026-09-17.jsonl", 6) // the live file
	cfg, root := archiveCfg(t, ledgerRoot, nil)

	plans, err := PlanArchive(root, cfg)
	if err != nil {
		t.Fatalf("PlanArchive: %v", err)
	}
	if len(plans) != 2 {
		t.Fatalf("plans = %d want 2 (the live file is never a candidate)", len(plans))
	}
	if plans[0].File != "2026-09-15.jsonl" || plans[1].File != "2026-09-16.1.gen.jsonl" {
		t.Fatalf("plan order = %s,%s want date order", plans[0].File, plans[1].File)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read generation: %v", err)
	}
	wantSHA := Sha256Hex(raw)
	if plans[1].SHA256 != wantSHA || plans[1].MarkerID != wantSHA[:16] {
		t.Fatalf("marker id %q / sha %q do not derive from the file bytes", plans[1].MarkerID, plans[1].SHA256)
	}
	if plans[1].Records != 3 || plans[1].FirstSeq != 3 || plans[1].LastSeq != 5 {
		t.Fatalf("plan stats = %+v want 3 records seq 3..5", plans[1])
	}
	if plans[1].MinTS == "" || plans[1].MaxTS == "" {
		t.Fatalf("plan ts range is empty: %+v", plans[1])
	}
	wantKey := "trouble/7f3a91c2d4e5b607/ledger/2026-09-16.1.gen.jsonl.gz"
	if plans[1].ObjectKey != wantKey {
		t.Fatalf("object key = %q want %q", plans[1].ObjectKey, wantKey)
	}
	if plans[1].GzipBytes <= 0 || plans[1].GzipBytes >= plans[1].Bytes {
		t.Fatalf("gzip estimate = %d for %d raw bytes", plans[1].GzipBytes, plans[1].Bytes)
	}
	if plans[1].Closure != "not_live" {
		t.Fatalf("closure evidence = %q want not_live (no .idx sidecar exists in this tree)", plans[1].Closure)
	}
}

func TestPlanArchiveRefusesAnUnknownLiveFile(t *testing.T) {
	ledgerRoot := t.TempDir()
	writeGeneration(t, ledgerRoot, "2026-09-15.jsonl", 1)
	cfg, root := archiveCfg(t, ledgerRoot, nil)
	cfg.LiveFile = ""
	_, err := PlanArchive(root, cfg)
	if CodeOf(err) != types.CodeHub011 {
		t.Fatalf("code = %q want TROUBLE-HUB-011 (%v)", CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "live ledger file is unknown") {
		t.Fatalf("refusal does not name the cause: %v", err)
	}
}

func TestPlanArchiveRefusesADisagreeingSidecar(t *testing.T) {
	ledgerRoot := t.TempDir()
	writeGeneration(t, ledgerRoot, "2026-09-15.jsonl", 1, 2)
	if err := os.WriteFile(filepath.Join(ledgerRoot, "2026-09-15.jsonl.idx"), []byte(`{"Bytes":999999,"sha256":"deadbeef"}`), 0o600); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	cfg, root := archiveCfg(t, ledgerRoot, nil)
	_, err := PlanArchive(root, cfg)
	if CodeOf(err) != types.CodeHub011 {
		t.Fatalf("code = %q want TROUBLE-HUB-011 (%v)", CodeOf(err), err)
	}
}

// TestArchiveExportVerifyMarkDropCycle walks §3.5's five steps.
func TestArchiveExportVerifyMarkDropCycle(t *testing.T) {
	ledgerRoot := t.TempDir()
	writeGeneration(t, ledgerRoot, "2026-09-15.jsonl", 1, 2)
	writeGeneration(t, ledgerRoot, "2026-09-16.jsonl", 3)
	cfg, root := archiveCfg(t, ledgerRoot, nil)
	target := NewDirTarget(t.TempDir())
	events := &eventLog{}
	ledger := newFakeLedger(events)

	plans, err := PlanArchive(root, cfg)
	if err != nil {
		t.Fatalf("PlanArchive: %v", err)
	}
	if len(plans) != 2 {
		t.Fatalf("plans = %d want 2", len(plans))
	}
	arch := NewArchiver(cfg, target, ledger)
	mk, err := arch.Archive(context.Background(), plans[0])
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if mk.State != "verified" {
		t.Fatalf("marker state = %q want verified (verify_after_write=true)", mk.State)
	}
	if mk.MarkerID != plans[0].MarkerID || mk.Sha256 != plans[0].SHA256 {
		t.Fatalf("marker identity drifted: %+v", mk)
	}
	if mk.GzipBytes <= 0 || mk.GzipBytes > mk.Bytes {
		t.Fatalf("marker gzip bytes = %d for %d raw", mk.GzipBytes, mk.Bytes)
	}
	obj, err := target.Get(context.Background(), plans[0].ObjectKey)
	if err != nil {
		t.Fatalf("the object is not readable: %v", err)
	}
	if int64(len(obj)) != mk.GzipBytes {
		t.Fatalf("object bytes = %d want %d", len(obj), mk.GzipBytes)
	}
	unzipped, err := gunzipBytes(obj)
	if err != nil {
		t.Fatalf("object is not gzip: %v", err)
	}
	if Sha256Hex(unzipped) != mk.Sha256 {
		t.Fatalf("exported object does not hash back to the marker")
	}

	// Step 4: one marker AND one lifecycle record.
	store := NewMarkerStore(root)
	marks, skipped, err := store.Load()
	if err != nil {
		t.Fatalf("marker Load: %v", err)
	}
	if skipped != 0 || len(marks) != 1 {
		t.Fatalf("markers = %d skipped=%d want 1/0", len(marks), skipped)
	}
	var lifecycle int
	for _, r := range ledger.Records() {
		if r.Kind == types.KLifecycle {
			if op, _ := r.Payload["op"].(string); op == "archive_exported" {
				lifecycle++
			}
		}
	}
	if lifecycle != 1 {
		t.Fatalf("lifecycle archive_exported records = %d want 1", lifecycle)
	}
	// The queue records the transition.
	q, err := ArchiveQueue(root)
	if err != nil {
		t.Fatalf("ArchiveQueue: %v", err)
	}
	if q.Depth() != 0 || len(q.Jobs()) != 1 || q.Jobs()[0].State != "verified" {
		t.Fatalf("queue = %+v want one verified job and no pending work", q.Jobs())
	}

	// Re-planning the same file is a no-op: the marker already exists, so the
	// content-derived identity is doing the bookkeeping (SPEC-13 §3.5).
	plans2, err := PlanArchive(root, cfg)
	if err != nil {
		t.Fatalf("PlanArchive (second): %v", err)
	}
	if len(plans2) != 1 || plans2[0].File != "2026-09-16.jsonl" {
		t.Fatalf("re-plan = %+v want only the unexported generation", plans2)
	}
	// And a direct re-export of the SAME plan writes the same object, not a
	// second one.
	mk2, err := arch.Archive(context.Background(), plans[0])
	if err != nil {
		t.Fatalf("re-export: %v", err)
	}
	if mk2.MarkerID != mk.MarkerID {
		t.Fatalf("re-export changed the marker id: %q → %q", mk.MarkerID, mk2.MarkerID)
	}
	var objects int
	_ = filepath.Walk(target.Root, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			objects++
		}
		return nil
	})
	if objects != 1 {
		t.Fatalf("target holds %d objects want 1 (idempotent by marker id)", objects)
	}

	// Step 5: only after a verified export may the generation be dropped.
	drops, err := DroppableGenerationsIn(ledgerRoot, root, cfg.LiveFile, 0)
	if err != nil {
		t.Fatalf("DroppableGenerationsIn: %v", err)
	}
	if len(drops) != 1 || drops[0].File != "2026-09-15.jsonl" {
		t.Fatalf("droppable = %+v want the verified generation only", drops)
	}
	if err := MarkDropped(root, drops[0], mustTime(t, "2026-09-17T00:00:00.000Z")); err != nil {
		t.Fatalf("MarkDropped: %v", err)
	}
	marks, _, err = store.Load()
	if err != nil {
		t.Fatalf("marker Load: %v", err)
	}
	if marks[len(marks)-1].State != "dropped" {
		t.Fatalf("the drop did not leave a dropped marker: %+v", marks[len(marks)-1])
	}
	if len(marks) != 2 {
		t.Fatalf("a marker was rewritten instead of appended (markers=%d)", len(marks))
	}
	// After the drop marker, the generation is no longer droppable (it is gone
	// from the hot host's perspective) and nothing re-drops it silently.
	drops2, err := DroppableGenerationsIn(ledgerRoot, root, cfg.LiveFile, 0)
	if err != nil {
		t.Fatalf("DroppableGenerationsIn (after drop): %v", err)
	}
	if len(drops2) != 0 {
		t.Fatalf("droppable after the drop = %+v want none", drops2)
	}
}

// corruptTarget wraps a target and mangles what it reads back — the 012 case.
type corruptTarget struct {
	inner Target
	put   error
}

func (c *corruptTarget) Describe() string { return c.inner.Describe() }
func (c *corruptTarget) Put(ctx context.Context, key string, data []byte) error {
	if c.put != nil {
		return c.put
	}
	return c.inner.Put(ctx, key, data)
}
func (c *corruptTarget) Get(ctx context.Context, key string) ([]byte, error) {
	b, err := c.inner.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	return append(b, 'X'), nil
}

func TestArchiveVerificationFailureKeepsTheGenerationUndroppable(t *testing.T) {
	ledgerRoot := t.TempDir()
	writeGeneration(t, ledgerRoot, "2026-09-15.jsonl", 1, 2)
	cfg, root := archiveCfg(t, ledgerRoot, nil)
	arch := NewArchiver(cfg, &corruptTarget{inner: NewDirTarget(t.TempDir())}, nil)
	plans, err := PlanArchive(root, cfg)
	if err != nil {
		t.Fatalf("PlanArchive: %v", err)
	}
	mk, err := arch.Archive(context.Background(), plans[0])
	if CodeOf(err) != types.CodeHub012 {
		t.Fatalf("code = %q want TROUBLE-HUB-012 (%v)", CodeOf(err), err)
	}
	if mk.State == "verified" {
		t.Fatalf("a failed verification must not be marked verified: %+v", mk)
	}
	drops, derr := DroppableGenerationsIn(ledgerRoot, root, cfg.LiveFile, 0)
	if CodeOf(derr) != types.CodeHub014 {
		t.Fatalf("drop gate code = %q want TROUBLE-HUB-014 (%v)", CodeOf(derr), derr)
	}
	if len(drops) != 0 {
		t.Fatalf("an unverified generation was offered for dropping: %+v", drops)
	}
	if !strings.Contains(derr.Error(), "2026-09-15.jsonl") {
		t.Fatalf("014 does not name the blocked file: %v", derr)
	}
	// The generation is still on disk: history outlives the hot host or it does
	// not leave (SPEC-13 §3.5 step 5).
	if _, err := os.Stat(filepath.Join(ledgerRoot, "2026-09-15.jsonl")); err != nil {
		t.Fatalf("the hot generation disappeared: %v", err)
	}
}

func TestArchiveTargetOutageIsTenAndGrowsTheQueue(t *testing.T) {
	ledgerRoot := t.TempDir()
	writeGeneration(t, ledgerRoot, "2026-09-15.jsonl", 1)
	cfg, root := archiveCfg(t, ledgerRoot, nil)
	arch := NewArchiver(cfg, &corruptTarget{inner: NewDirTarget(t.TempDir()), put: errors.New("dial tcp: connection refused")}, nil)
	plans, err := PlanArchive(root, cfg)
	if err != nil {
		t.Fatalf("PlanArchive: %v", err)
	}
	mk, err := arch.Archive(context.Background(), plans[0])
	if CodeOf(err) != types.CodeHub010 {
		t.Fatalf("code = %q want TROUBLE-HUB-010 (%v)", CodeOf(err), err)
	}
	var he *Error
	if !errors.As(err, &he) || !he.Transient() {
		t.Fatalf("010 must be transient (the job is retried)")
	}
	if mk.State != "failed" {
		t.Fatalf("marker state = %q want failed", mk.State)
	}
	q, err := ArchiveQueue(root)
	if err != nil {
		t.Fatalf("ArchiveQueue: %v", err)
	}
	if q.Depth() != 1 {
		t.Fatalf("queue depth = %d want 1 (the export is still owed)", q.Depth())
	}
	// The next pass re-plans the same file with the SAME marker id.
	plans2, err := PlanArchive(root, cfg)
	if err != nil {
		t.Fatalf("PlanArchive (retry): %v", err)
	}
	if len(plans2) != 1 || plans2[0].MarkerID != plans[0].MarkerID {
		t.Fatalf("the retry re-planned under a different id: %+v vs %+v", plans2, plans[0])
	}
}

func TestArchiveRefusesBytesChangedUnderThePlan(t *testing.T) {
	ledgerRoot := t.TempDir()
	path := writeGeneration(t, ledgerRoot, "2026-09-15.jsonl", 1)
	cfg, root := archiveCfg(t, ledgerRoot, nil)
	plans, err := PlanArchive(root, cfg)
	if err != nil {
		t.Fatalf("PlanArchive: %v", err)
	}
	// A re-compaction between plan and export: new bytes, new identity.
	if err := os.WriteFile(path, []byte(`{"seq":9,"kind":"event"}`+"\n"), 0o600); err != nil {
		t.Fatalf("rewrite generation: %v", err)
	}
	arch := NewArchiver(cfg, NewDirTarget(t.TempDir()), nil)
	if _, err := arch.Archive(context.Background(), plans[0]); CodeOf(err) != types.CodeHub011 {
		t.Fatalf("code = %q want TROUBLE-HUB-011 (%v)", CodeOf(err), err)
	}
}

func TestDroppableGenerationsRetainsTheNewestEvenWhenVerified(t *testing.T) {
	ledgerRoot := t.TempDir()
	writeGeneration(t, ledgerRoot, "2026-09-14.jsonl", 1)
	writeGeneration(t, ledgerRoot, "2026-09-15.jsonl", 2)
	writeGeneration(t, ledgerRoot, "2026-09-16.jsonl", 3)
	cfg, root := archiveCfg(t, ledgerRoot, nil)
	arch := NewArchiver(cfg, NewDirTarget(t.TempDir()), nil)
	plans, err := PlanArchive(root, cfg)
	if err != nil {
		t.Fatalf("PlanArchive: %v", err)
	}
	for _, p := range plans {
		if _, err := arch.Archive(context.Background(), p); err != nil {
			t.Fatalf("Archive(%s): %v", p.File, err)
		}
	}
	drops, err := DroppableGenerationsIn(ledgerRoot, root, cfg.LiveFile, 2)
	if err != nil {
		t.Fatalf("DroppableGenerationsIn: %v", err)
	}
	if len(drops) != 1 || drops[0].File != "2026-09-14.jsonl" {
		t.Fatalf("keep=2 droppable = %+v want only the oldest", drops)
	}
}

func TestMarkerStoreToleratesATornLastLine(t *testing.T) {
	root := t.TempDir()
	if err := EnsureStateDirs(root); err != nil {
		t.Fatalf("EnsureStateDirs: %v", err)
	}
	store := NewMarkerStore(root)
	good := types.LedgerArchiveMarker{MarkerID: "abc", File: "2026-09-15.jsonl", State: "verified"}
	if err := store.Append(good); err != nil {
		t.Fatalf("Append: %v", err)
	}
	f, err := os.OpenFile(store.Path(), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open marker file: %v", err)
	}
	if _, err := f.WriteString(`{"marker_id":"torn","file":"2026-`); err != nil {
		t.Fatalf("write torn line: %v", err)
	}
	_ = f.Close()
	marks, _, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(marks) != 1 || marks[0].MarkerID != "abc" {
		t.Fatalf("a torn last line must truncate, not corrupt: %+v", marks)
	}
	if mk, ok := store.Latest("2026-09-15.jsonl"); !ok || mk.MarkerID != "abc" {
		t.Fatalf("Latest did not find the intact marker")
	}
}

// ---- the DuckBrain KV target ----

func TestNewTargetRefusals(t *testing.T) {
	cfg := ArchiveConfig{Endpoint: "http://x", StateRoot: t.TempDir()}
	if _, err := NewTarget(cfg); CodeOf(err) != types.CodeHub009 {
		t.Fatalf("empty namespace: code = %q want 009 (%v)", CodeOf(err), err)
	}
	cfg.Namespace = "trouble/h1"
	cfg.Endpoint = ""
	if _, err := NewTarget(cfg); CodeOf(err) != types.CodeHub009 {
		t.Fatalf("empty endpoint: code = %q want 009 (%v)", CodeOf(err), err)
	}
}

// TestKVTargetSpeaksTheDuckBrainContract drives the target against a real HTTP
// server implementing the KV shape internal/issues' driver uses.
func TestKVTargetSpeaksTheDuckBrainContract(t *testing.T) {
	store := map[string][]byte{}
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("x-db-key")
		gotPath = r.URL.Path
		if gotAuth != "secret-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if strings.HasSuffix(gotPath, "/boom") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if r.Method == http.MethodPut {
			var body kvValue
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			raw, err := base64.StdEncoding.DecodeString(body.Value.Data)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			store[gotPath] = raw
			w.WriteHeader(http.StatusCreated)
			return
		}
		b, ok := store[gotPath]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(kvValue{Value: kvBlob{
			Encoding: "base64",
			Bytes:    int64(len(b)),
			Sha256:   Sha256Hex(b),
			Data:     base64.StdEncoding.EncodeToString(b),
		}})
	}))
	defer srv.Close()

	t.Setenv("TROUBLE_DUCKBRAIN_KEY", "secret-token")
	cfg := ArchiveConfig{
		Namespace: "trouble/h1",
		Endpoint:  srv.URL,
		KeyEnv:    "TROUBLE_DUCKBRAIN_KEY",
		KeyHeader: "x-db-key",
		Gzip:      true,
		StateRoot: t.TempDir(),
	}
	target, err := NewTarget(cfg)
	if err != nil {
		t.Fatalf("NewTarget: %v", err)
	}
	if strings.Contains(target.Describe(), "secret-token") {
		t.Fatalf("Describe leaked the credential: %q", target.Describe())
	}
	key := cfg.ObjectKey("2026-09-15.jsonl")
	if err := target.Put(context.Background(), key, []byte("raw-bytes")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if gotAuth != "secret-token" {
		t.Fatalf("the credential header did not reach the server (%q)", gotAuth)
	}
	if !strings.Contains(gotPath, "/v1/kv/trouble/h1/ledger/2026-09-15.jsonl.gz") {
		t.Fatalf("object path = %q", gotPath)
	}
	back, err := target.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(back) != "raw-bytes" {
		t.Fatalf("round trip = %q", back)
	}
	// A missing object is a verification failure (012), not a silent empty read.
	if _, err := target.Get(context.Background(), cfg.ObjectKey("2026-09-16.jsonl")); CodeOf(err) != types.CodeHub012 {
		t.Fatalf("missing object: code = %q want 012 (%v)", CodeOf(err), err)
	}
	// A server error is transient (010): the export is retried.
	if err := target.Put(context.Background(), "boom", []byte("x")); CodeOf(err) != types.CodeHub010 {
		t.Fatalf("server error: code = %q want 010 (%v)", CodeOf(err), err)
	}
	// A refused credential is a configuration error (009): archival must not
	// start against a tier that will reject every write.
	t.Setenv("TROUBLE_DUCKBRAIN_KEY", "wrong")
	bad, err := NewTarget(cfg)
	if err != nil {
		t.Fatalf("NewTarget: %v", err)
	}
	if err := bad.Put(context.Background(), key, []byte("x")); CodeOf(err) != types.CodeHub009 {
		t.Fatalf("auth refusal: code = %q want 009 (%v)", CodeOf(err), err)
	}
}

// TestArchiveThroughTheKVCycle is the end-to-end archival proof without a live
// DuckBrain: the same HTTP contract, the same verify-before-drop rule.
func TestArchiveThroughTheKVCycle(t *testing.T) {
	store := map[string][]byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/v1/kv/")
		if r.Method == http.MethodPut {
			var body kvValue
			_ = json.NewDecoder(r.Body).Decode(&body)
			raw, _ := base64.StdEncoding.DecodeString(body.Value.Data)
			store[key] = raw
			w.WriteHeader(http.StatusCreated)
			return
		}
		b, ok := store[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(kvValue{Value: kvBlob{
			Bytes: int64(len(b)), Sha256: Sha256Hex(b), Data: base64.StdEncoding.EncodeToString(b),
		}})
	}))
	defer srv.Close()

	ledgerRoot := t.TempDir()
	writeGeneration(t, ledgerRoot, "2026-09-15.jsonl", 1, 2, 3)
	cfg, root := archiveCfg(t, ledgerRoot, nil)
	cfg.Endpoint = srv.URL
	cfg.Gzip = false
	target, err := NewTarget(cfg)
	if err != nil {
		t.Fatalf("NewTarget: %v", err)
	}
	plans, err := PlanArchive(root, cfg)
	if err != nil {
		t.Fatalf("PlanArchive: %v", err)
	}
	arch := NewArchiver(cfg, target, nil)
	mk, err := arch.Archive(context.Background(), plans[0])
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if mk.State != "verified" || !strings.HasSuffix(mk.ObjectKey, "2026-09-15.jsonl") {
		t.Fatalf("marker = %+v want verified with an ungzipped object key", mk)
	}
	drops, err := DroppableGenerationsIn(ledgerRoot, root, cfg.LiveFile, 0)
	if err != nil {
		t.Fatalf("DroppableGenerationsIn: %v", err)
	}
	if len(drops) != 1 {
		t.Fatalf("droppable = %+v want the exported generation", drops)
	}
}

// TestArchiveRefusesAnEmptyStateRoot pins the guard that keeps a mis-wired
// caller from writing the marker file and the queue into the process's working
// directory (an empty state root derives a relative path).
func TestArchiveRefusesAnEmptyStateRoot(t *testing.T) {
	if err := EnsureStateDirs(""); CodeOf(err) != types.CodeHub009 {
		t.Fatalf("EnsureStateDirs on an empty root: code = %q want 009 (%v)", CodeOf(err), err)
	}
	store := NewMarkerStore("")
	if err := store.Append(types.LedgerArchiveMarker{MarkerID: "x"}); CodeOf(err) != types.CodeHub009 {
		t.Fatalf("MarkerStore.Append with no root: code = %q want 009 (%v)", CodeOf(err), err)
	}
	if err := appendJob("", archiveJob{MarkerID: "x"}); CodeOf(err) != types.CodeHub009 {
		t.Fatalf("appendJob with no root: code = %q want 009 (%v)", CodeOf(err), err)
	}
	ledgerRoot := t.TempDir()
	writeGeneration(t, ledgerRoot, "2026-09-15.jsonl", 1)
	cfg := ArchiveConfig{Enabled: true, Namespace: "trouble/h1", Gzip: true, LedgerRoot: ledgerRoot, LiveFile: "2026-09-16.jsonl"}
	_, err := NewArchiver(cfg, NewDirTarget(t.TempDir()), nil).Archive(context.Background(), ArchivePlan{
		File: "2026-09-15.jsonl",
		Path: filepath.Join(ledgerRoot, "2026-09-15.jsonl"),
	})
	if CodeOf(err) != types.CodeHub009 {
		t.Fatalf("Archive with no state root: code = %q want 009 (%v)", CodeOf(err), err)
	}
}

func TestScanGenerationsIgnoresForeignFiles(t *testing.T) {
	ledgerRoot := t.TempDir()
	writeGeneration(t, ledgerRoot, "2026-09-15.jsonl", 1)
	for _, name := range []string{"HEAD", "not-a-day.jsonl", "2026-09-15.jsonl.idx", "2026-9-1.jsonl"} {
		if err := os.WriteFile(filepath.Join(ledgerRoot, name), []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	gens, err := ScanGenerations(ledgerRoot, "2026-09-16.jsonl")
	if err != nil {
		t.Fatalf("ScanGenerations: %v", err)
	}
	if len(gens) != 1 || gens[0].File != "2026-09-15.jsonl" {
		t.Fatalf("candidates = %+v want only the ledger-shaped file", gens)
	}
}
