package flow

// spool.go — the flow-owned durable dispatch queue (SPEC-08 §3.9a).
//
// The flow's pending spawns are durable in a store the FLOW owns and REPLAYS.
// SPEC-09's desk spool stays the desk's own queue: its replay walks the
// configured driver names (github/duckbrain), so no loop ever lists the tree a
// foreign `Put("issue", …)` wrote to, its replay decode expects the desk's own
// `spoolOp` shape, and its shipped posture (`issues.enabled = false`,
// SPEC-09 §3.4a) refuses a foreign enqueue outright. A dispatch queued there is
// therefore durable in name only; §3.9a gives the flow its own store so its
// durability stops depending on another subsystem's opt-out posture.

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// SpoolBounds are the flow queue's bounds (§3.9a). The defaults mirror the
// numbers the two adjacent contracts already pin: the in-memory queue's 256
// (§3.9), SPEC-09 §3.7's 72 h spool_ttl and 20-attempt cap, and §3.9's
// five-attempt bounded retry schedule.
type SpoolBounds struct {
	MaxEntries  int           // default 256 — §3.9's in-memory bound
	TTL         time.Duration // default 72h — SPEC-09 §3.7's spool_ttl
	MaxAttempts int           // default 5 — §3.9's bounded retry budget
	ReplayEvery time.Duration // default 5s — the drain cadence
	ReplayBatch int           // default 100 — entries per drain
}

const (
	defaultSpoolMaxEntries  = 256
	defaultSpoolTTL         = 72 * time.Hour
	defaultSpoolMaxAttempts = 5
	defaultFlowReplayEvery  = 5 * time.Second
	defaultFlowReplayBatch  = 100
)

// WithDefaults fills every unset bound, so a half-specified config still bounds
// the queue rather than leaving it unbounded.
func (b SpoolBounds) WithDefaults() SpoolBounds {
	if b.MaxEntries <= 0 {
		b.MaxEntries = defaultSpoolMaxEntries
	}
	if b.TTL <= 0 {
		b.TTL = defaultSpoolTTL
	}
	if b.MaxAttempts <= 0 {
		b.MaxAttempts = defaultSpoolMaxAttempts
	}
	if b.ReplayEvery <= 0 {
		b.ReplayEvery = defaultFlowReplayEvery
	}
	if b.ReplayBatch <= 0 {
		b.ReplayBatch = defaultFlowReplayBatch
	}
	return b
}

// DropEvent is one eviction the store performed. The flow turns it into a
// `flow` record (there is no `gap` writer on this subsystem: SPEC-INDEX §3.4
// gives `gap` to SPEC-03/04/07/09/12/13), so a drop is visible in the ledger
// rather than inferred.
type DropEvent struct {
	ID       string
	TS       string
	Reason   string // overflow | ttl | attempts | corrupt
	TaskID   string
	Sig      string
	Inc      string
	Attempts int
	EstLost  int
}

// Spool is the flow-owned durable dispatch queue: one JSON object per file under
// `<root>/<ev_ULID>.json`, mode 0600, written atomically (temp → fsync → rename →
// fsync dir) so a torn entry can never be replayed. A store whose directory
// cannot be created is NOT silently in-memory: Put returns the error and the
// caller records an honest refusal (§3.9a rule 4).
type Spool struct {
	root string
	b    SpoolBounds
	now  func() time.Time

	mu sync.Mutex
}

// NewSpool builds the store and proves the tree is writable at construction, so
// a misconfigured root is a boot refusal rather than a first-dispatch surprise.
func NewSpool(root string, b SpoolBounds, now func() time.Time) (*Spool, error) {
	if now == nil {
		now = time.Now
	}
	s := &Spool{root: root, b: b.WithDefaults(), now: now}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	probe := filepath.Join(root, ".probe")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		return nil, err
	}
	return s, os.Remove(probe)
}

// Root is the resolved directory (diagnostics and tests).
func (s *Spool) Root() string { return s.root }

// Bounds returns the resolved bounds.
func (s *Spool) Bounds() SpoolBounds { return s.b }

// Enqueue satisfies flow's spoolSink. It is the plain write: the drops it
// performed are discarded here, so an owner that wants the ledger note uses Put
// (the flow does).
func (s *Spool) Enqueue(_ context.Context, e types.SpoolEntry) error {
	_, err := s.Put(e)
	return err
}

// Put writes one entry, applying the bounds first. Overflow evicts the OLDEST
// entries — never the incoming one — and reports each eviction, so the queue
// shrinks under pressure instead of refusing the newest work.
func (s *Spool) Put(e types.SpoolEntry) ([]DropEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return nil, err
	}
	existing, err := s.listLocked()
	if err != nil {
		return nil, err
	}
	var drops []DropEvent
	// The incoming entry counts, so a queue at the bound still accepts the new
	// entry and evicts the oldest one to make room.
	over := len(existing) + 1 - s.b.MaxEntries
	for i := 0; i < over && i < len(existing); i++ {
		old := existing[i]
		if err := os.Remove(s.pathOf(old.ID)); err != nil && !os.IsNotExist(err) {
			return drops, err
		}
		drops = append(drops, dropOf(old, "overflow"))
	}
	if e.ID == "" {
		e.ID = types.NewID(types.PEv)
	}
	if e.TS == "" {
		e.TS = types.FormatUTC(s.now())
	}
	b, err := json.Marshal(e)
	if err != nil {
		return drops, err
	}
	return drops, s.writeAtomic(s.pathOf(e.ID), b)
}

// List returns every entry in replay order: (next_try_ts, ts, id), so a due
// retry precedes a fresh entry and two entries of one instant keep a stable
// order.
func (s *Spool) List() ([]types.SpoolEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.listLocked()
	if err != nil {
		return nil, err
	}
	sort.SliceStable(entries, func(i, j int) bool {
		return spoolBefore(entries[i], entries[j])
	})
	return entries, nil
}

// Delete removes one entry (replay success, or a drop the caller has already
// recorded).
func (s *Spool) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.Remove(s.pathOf(id))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Update rewrites one entry in place (attempts and next_try_ts after a failed
// replay).
func (s *Spool) Update(e types.SpoolEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return s.writeAtomic(s.pathOf(e.ID), b)
}

// Count is the queue depth (diagnostics and tests).
func (s *Spool) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.listLocked()
	if err != nil {
		return 0
	}
	return len(entries)
}

// Due reports whether an entry is due now — what keeps the drain cheap while the
// queue is empty or every entry is waiting on its backoff.
func (s *Spool) Due(now time.Time) bool {
	entries, err := s.List()
	if err != nil {
		return false
	}
	due := types.FormatUTC(now)
	for _, e := range entries {
		if e.NextTryTS == "" || e.NextTryTS <= due {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// internals
// ---------------------------------------------------------------------------

func (s *Spool) pathOf(id string) string { return filepath.Join(s.root, id+".json") }

// listLocked reads the directory in (next_try_ts, ts, id) order. A file whose
// name does not look like an entry id (a temp file mid-write, an operator's
// scratch file) is skipped rather than decoded.
func (s *Spool) listLocked() ([]types.SpoolEntry, error) {
	out := []types.SpoolEntry{}
	des, err := os.ReadDir(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	for _, de := range des {
		if de.IsDir() || filepath.Ext(de.Name()) != ".json" {
			continue
		}
		id := de.Name()[:len(de.Name())-len(".json")]
		if !strings.HasPrefix(id, string(types.PEv)) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.root, de.Name()))
		if err != nil {
			continue
		}
		var e types.SpoolEntry
		if err := json.Unmarshal(b, &e); err != nil {
			continue
		}
		if e.ID == "" {
			e.ID = id
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool { return spoolBefore(out[i], out[j]) })
	return out, nil
}

// writeAtomic is the §3.9a durability rule: temp file in the SAME directory →
// fsync the file → rename → fsync the directory, so the rename is on disk
// before the caller is told the entry is queued.
func (s *Spool) writeAtomic(path string, b []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return nil // the rename landed; a directory fsync we cannot open is not a loss
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !isNotSupported(err) {
		return err
	}
	return nil
}

// isNotSupported keeps a filesystem that refuses a directory fsync from turning
// a durable write into a failure.
func isNotSupported(err error) bool {
	return errors.Is(err, fs.ErrInvalid)
}

// spoolBefore is the §3.9a ordering key: (next_try_ts asc, ts asc, id asc).
func spoolBefore(a, b types.SpoolEntry) bool {
	if a.NextTryTS != b.NextTryTS {
		return a.NextTryTS < b.NextTryTS
	}
	if a.TS != b.TS {
		return a.TS < b.TS
	}
	return a.ID < b.ID
}

func dropOf(e types.SpoolEntry, reason string) DropEvent {
	d := DropEvent{ID: e.ID, TS: e.TS, Reason: reason, TaskID: e.IdemKey, Attempts: e.Attempts, EstLost: -1}
	var req types.SpawnRequest
	if err := json.Unmarshal(e.Payload, &req); err == nil {
		d.TaskID = req.TaskID
		d.Sig = req.Sig
		d.Inc = req.Inc
	}
	return d
}
