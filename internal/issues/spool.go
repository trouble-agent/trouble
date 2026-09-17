package issues

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// spoolOp is the marshalled payload of a spooled operation (§3.7). The same
// shape is replayed and is the only thing the spool ever contains; it is
// scrubbed before it is marshalled, so the spool is not a leak surface.
type spoolOp struct {
	Op       string                    `json:"op"` // ensure | comment | close
	Driver   string                    `json:"driver"`
	Sig      string                    `json:"sig"`
	Project  string                    `json:"project"`
	Inc      string                    `json:"inc"`
	Ensure   *types.EnsureBySigRequest `json:"ensure,omitempty"`
	Ref      *types.IssueRef           `json:"ref,omitempty"`
	Trigger  string                    `json:"trigger,omitempty"`
	Body     string                    `json:"body,omitempty"`
	Reason   string                    `json:"reason,omitempty"`
	TaskID   string                    `json:"task_id,omitempty"`
	Research string                    `json:"research_id,omitempty"`
	// Unverified marks an ensure whose create outcome is unknown (edge case 1):
	// replay must read the anchor back before it creates anything.
	Unverified bool `json:"ensure_unverified,omitempty"`
}

// dropEvent is one eviction the store performed; the desk turns it into the
// `issue{op:drop}` + `gap{cause:queue_overflow}` record pair (§3.7).
type dropEvent struct {
	Driver   string
	Count    int
	Reason   string // budget | entries | ttl | attempts
	OldestTS string
	EstLost  int
}

// spoolStore is the bounded, atomic, per-driver spool under
// <state root>/spool/issues/<driver>/<ev_ULID>.json (SPEC-09 §3.7).
type spoolStore struct {
	root string
	cfg  types.IssueDeskConfig
	now  func() time.Time

	mu      sync.Mutex
	memOnly bool
	memMax  int                // in-memory queue bound (edge case 9: 1 000 entries, <= 8 MB)
	mem     []types.SpoolEntry // in-memory queue when the directory is unwritable
}

func newSpoolStore(root string, cfg types.IssueDeskConfig, now func() time.Time) *spoolStore {
	s := &spoolStore{root: root, cfg: cfg, now: now, memMax: 1000}
	if err := os.MkdirAll(root, 0o700); err != nil {
		s.memOnly = true
	}
	if s.memOnly {
		return s
	}
	// Prove the tree is writable; a read-only spool must not fail a call, it must
	// queue in memory (edge case 9).
	t := filepath.Join(root, ".probe")
	if err := os.WriteFile(t, []byte("ok"), 0o600); err != nil {
		s.memOnly = true
		return s
	}
	_ = os.Remove(t)
	return s
}

func (s *spoolStore) dir(driver string) string { return filepath.Join(s.root, driver) }

// Put appends an entry, applying the §3.7 bounds. Behaviour at a bound: evict
// oldest entries while they are older than spool_min_retention; entries younger
// than the floor are never evicted, so a full spool refuses the new entry with
// TROUBLE-ISSUES-005 and the caller counts it as suppressed.
func (s *spoolStore) Put(driver string, e types.SpoolEntry) ([]dropEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.memOnly {
		limit := s.memMax
		if limit <= 0 {
			limit = 1000
		}
		if len(s.mem) >= limit { // ≤1 000 entries (≤8 MB) absorbs the burst
			return nil, newErr(types.CodeIssues005, ReasonSpoolFull, 0, true,
				"in-memory spool is full (%d entries) and the spool directory is unwritable", limit)
		}
		s.mem = append(s.mem, e)
		return nil, nil
	}
	// Eviction runs with the incoming entry counted, so a spool is never refused
	// while an evictable entry is still there.
	drops, err := s.evictLocked(driver, entryBytes(e))
	if err != nil {
		return nil, err
	}
	entries, err := s.listLocked(driver)
	if err != nil {
		return nil, err
	}
	var total int64
	for _, it := range entries {
		total += entryBytes(it)
	}
	if (s.cfg.SpoolMaxEntries > 0 && len(entries)+1 > s.cfg.SpoolMaxEntries) ||
		(s.cfg.SpoolBudgetBytes > 0 && total+entryBytes(e) > s.cfg.SpoolBudgetBytes) {
		return drops, newErr(types.CodeIssues005, ReasonSpoolFull, 0, true,
			"spool for %s is full (%d entries, %d bytes) and every entry is younger than the retention floor",
			driver, len(entries), total)
	}
	if err := s.writeLocked(driver, e); err != nil {
		return drops, err
	}
	return drops, nil
}

// writeLocked writes one entry atomically: temp file → fsync → rename → fsync
// dir, so a torn entry can never be replayed.
func (s *spoolStore) writeLocked(driver string, e types.SpoolEntry) error {
	dir := s.dir(driver)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return newErr(types.CodeIssues005, ReasonSpoolWrite, 0, true, "spool mkdir %s: %v", dir, err)
	}
	body, err := json.Marshal(e)
	if err != nil {
		return newErr(types.CodeIssues005, ReasonSpoolWrite, 0, false, "spool marshal: %v", err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return newErr(types.CodeIssues005, ReasonSpoolWrite, 0, true, "spool temp: %v", err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return newErr(types.CodeIssues005, ReasonSpoolWrite, 0, true, "spool chmod: %v", err)
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return newErr(types.CodeIssues005, ReasonSpoolWrite, 0, true, "spool write: %v", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return newErr(types.CodeIssues005, ReasonSpoolWrite, 0, true, "spool fsync: %v", err)
	}
	if err := tmp.Close(); err != nil {
		return newErr(types.CodeIssues005, ReasonSpoolWrite, 0, true, "spool close: %v", err)
	}
	final := filepath.Join(dir, e.ID+".json")
	if err := os.Rename(name, final); err != nil {
		return newErr(types.CodeIssues005, ReasonSpoolWrite, 0, true, "spool rename: %v", err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// evictLocked drops TTL-expired entries and oldest entries over the byte/entry
// budget, never touching entries younger than spool_min_retention. It also drops
// entries that exhausted max_attempts_per_op.
func (s *spoolStore) evictLocked(driver string, incoming int64) ([]dropEvent, error) {
	entries, err := s.listLocked(driver)
	if err != nil {
		return nil, err
	}
	now := s.now()
	floor := s.cfg.SpoolMinRetention.Std()
	ttl := s.cfg.SpoolTTL.Std()
	maxAttempts := s.cfg.MaxAttemptsPerOp

	var events []dropEvent
	drop := func(e types.SpoolEntry, reason string) {
		if err := os.Remove(filepath.Join(s.dir(driver), e.ID+".json")); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return
		}
		ev := dropEvent{Driver: driver, Count: 1, Reason: reason, OldestTS: e.TS, EstLost: -1}
		for i := range events {
			if events[i].Reason == reason {
				events[i].Count++
				if e.TS < events[i].OldestTS {
					events[i].OldestTS = e.TS
				}
				return
			}
		}
		events = append(events, ev)
	}

	kept := entries[:0]
	for _, e := range entries {
		ts, _ := types.ParseUTC(e.TS)
		age := now.Sub(ts)
		switch {
		case maxAttempts > 0 && e.Attempts >= maxAttempts:
			drop(e, "attempts")
		case ttl > 0 && age > ttl:
			drop(e, "ttl")
		default:
			kept = append(kept, e)
		}
	}
	entries = kept

	// Drop-oldest while over either budget, but only above the retention floor.
	over := func(list []types.SpoolEntry) bool {
		var total int64
		for _, e := range list {
			total += entryBytes(e)
		}
		if s.cfg.SpoolMaxEntries > 0 && len(list)+1 > s.cfg.SpoolMaxEntries {
			return true
		}
		return s.cfg.SpoolBudgetBytes > 0 && total+incoming > s.cfg.SpoolBudgetBytes
	}
	sort.SliceStable(entries, func(i, j int) bool { return entryOlder(entries[i], entries[j]) })
	for len(entries) > 0 && over(entries) {
		e := entries[0]
		ts, _ := types.ParseUTC(e.TS)
		if floor > 0 && now.Sub(ts) < floor {
			break // nothing evictable: the caller refuses the new entry
		}
		entries = entries[1:]
		drop(e, "budget")
	}
	return events, nil
}

// List returns a driver's entries in replay order: (next_try_ts asc, ts asc, id asc).
func (s *spoolStore) List(driver string) ([]types.SpoolEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.memOnly {
		out := append([]types.SpoolEntry(nil), s.mem...)
		s.sortEntries(out)
		return out, nil
	}
	return s.listLocked(driver)
}

func (s *spoolStore) sortEntries(list []types.SpoolEntry) {
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		an, bn := a.NextTryTS, b.NextTryTS
		if an == "" {
			an = a.TS
		}
		if bn == "" {
			bn = b.TS
		}
		if an != bn {
			return an < bn
		}
		if a.TS != b.TS {
			return a.TS < b.TS
		}
		return a.ID < b.ID
	})
}

func (s *spoolStore) listLocked(driver string) ([]types.SpoolEntry, error) {
	dir := s.dir(driver)
	ents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, newErr(types.CodeIssues005, ReasonSpoolWrite, 0, true, "spool read %s: %v", dir, err)
	}
	out := make([]types.SpoolEntry, 0, len(ents))
	for _, de := range ents {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, de.Name()))
		if err != nil {
			continue
		}
		var e types.SpoolEntry
		if err := json.Unmarshal(b, &e); err != nil {
			// A torn entry is unreplayable and unrecoverable: drop it and let the
			// caller's next tick notice the count, rather than loop forever.
			_ = os.Remove(filepath.Join(dir, de.Name()))
			continue
		}
		if e.ID == "" {
			e.ID = strings.TrimSuffix(de.Name(), ".json")
		}
		if e.Kind == "" {
			e.Kind = types.SpoolIssue
		}
		out = append(out, e)
	}
	s.sortEntries(out)
	return out, nil
}

// Update rewrites an entry's attempts/next-try fields in place.
func (s *spoolStore) Update(driver string, e types.SpoolEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.memOnly {
		for i := range s.mem {
			if s.mem[i].ID == e.ID {
				s.mem[i] = e
				return nil
			}
		}
		return nil
	}
	return s.writeLocked(driver, e)
}

// Delete removes one entry (a successful replay, or an explicit drop).
func (s *spoolStore) Delete(driver, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.memOnly {
		out := s.mem[:0]
		for _, e := range s.mem {
			if e.ID != id {
				out = append(out, e)
			}
		}
		s.mem = out
		return nil
	}
	if err := os.Remove(filepath.Join(s.dir(driver), id+".json")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return newErr(types.CodeIssues005, ReasonSpoolWrite, 0, true, "spool delete %s: %v", id, err)
	}
	return nil
}

// Count reports the entries held for a driver.
func (s *spoolStore) Count(driver string) int {
	list, _ := s.List(driver)
	return len(list)
}

// Empty reports whether every driver's spool is empty.
func (s *spoolStore) Empty(drivers []string) bool {
	for _, d := range drivers {
		if s.Count(d) > 0 {
			return false
		}
	}
	return true
}

// Bytes reports the on-disk size of a driver's spool tree.
func (s *spoolStore) Bytes(driver string) int64 {
	if s.memOnly {
		var n int64
		for _, e := range s.mem {
			n += entryBytes(e)
		}
		return n
	}
	var total int64
	_ = filepath.WalkDir(s.dir(driver), func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

func entryBytes(e types.SpoolEntry) int64 {
	return int64(len(e.Payload)) + int64(len(e.IdemKey)) + 128
}

func entryOlder(a, b types.SpoolEntry) bool {
	if a.TS != b.TS {
		return a.TS < b.TS
	}
	return a.ID < b.ID
}

// EncodePayload marshals a spoolOp; the desk scrubs the body before this call.
func EncodePayload(op spoolOp) ([]byte, error) {
	b, err := json.Marshal(op)
	if err != nil {
		return nil, newErr(types.CodeIssues005, ReasonSpoolWrite, 0, false, "spool payload: %v", err)
	}
	return b, nil
}

// DecodePayload is the replay-side decode.
func DecodePayload(b []byte) (spoolOp, error) {
	var op spoolOp
	if err := json.Unmarshal(b, &op); err != nil {
		return op, newErr(types.CodeIssues006, ReasonReplayFailed, 0, true, "spool payload decode: %v", err)
	}
	return op, nil
}

// NewSpoolEntry builds an entry with a fresh ev_ id and the driver's next-try
// timestamp.
func NewSpoolEntry(driver string, idemKey string, payload []byte, nextTryTS string, now time.Time) types.SpoolEntry {
	return types.SpoolEntry{
		ID:        types.NewID(types.PEv),
		TS:        types.FormatUTC(now),
		Kind:      types.SpoolIssue,
		Payload:   payload,
		Attempts:  0,
		IdemKey:   idemKey,
		NextTryTS: nextTryTS,
	}
}

// spoolQueue is a tiny FIFO used by the in-memory path and by tests.
type spoolQueue struct {
	items []types.SpoolEntry
}

func (q *spoolQueue) push(e types.SpoolEntry) { q.items = append(q.items, e) }

func (q *spoolQueue) pop() (types.SpoolEntry, bool) {
	if len(q.items) == 0 {
		return types.SpoolEntry{}, false
	}
	e := q.items[0]
	q.items = q.items[1:]
	return e, true
}

func (q *spoolQueue) len() int { return len(q.items) }

func (q *spoolQueue) String() string {
	return fmt.Sprintf("spoolQueue(%d)", len(q.items))
}
