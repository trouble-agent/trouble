package hub

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/trouble-agent/trouble/internal/types"
)

// MarkersPath is <state_root>/hub/archive/markers.jsonl (0600, append-only).
func MarkersPath(stateRoot string) string {
	return filepath.Join(ArchiveDir(stateRoot), "markers.jsonl")
}

// QueuePath is <state_root>/hub/archive/queue.jsonl (0600, append-only).
func QueuePath(stateRoot string) string {
	return filepath.Join(ArchiveDir(stateRoot), "queue.jsonl")
}

// Sha256Hex is the digest the marker's id derives from and the digest the
// read-back verification compares (SPEC-13 §3.5 step 3).
func Sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// MarkerID is `hex(sha256(file bytes))[:16]` (SPEC-13 §3.5): content-derived, so
// a re-export of the same file is the SAME marker and the SAME object key, and a
// re-export of a re-compacted generation is a new marker because the bytes
// differ. Idempotency by construction, not by bookkeeping.
func MarkerID(fileBytes []byte) string {
	h := Sha256Hex(fileBytes)
	if len(h) < 16 {
		return h
	}
	return h[:16]
}

// MarkerStore is the append-only marker ledger (`markers.jsonl`).
//
// It is the only durable statement about what has been exported (SPEC-13 §3.1):
// the DuckBrain namespace holds the bytes, this file holds the claim, the sha256
// and the verification result. A marker is never rewritten and never deleted —
// dropping a generation writes a `dropped` marker, not a deletion.
type MarkerStore struct {
	path string
	mu   sync.Mutex
}

// NewMarkerStore points a store at a state root.
func NewMarkerStore(stateRoot string) *MarkerStore {
	if stateRoot == "" {
		// See EnsureStateDirs: an empty root must never resolve to a relative
		// path, so the store is built unaddressable and refuses writes.
		return &MarkerStore{}
	}
	return &MarkerStore{path: MarkersPath(stateRoot)}
}

// Path is the resolved marker file.
func (m *MarkerStore) Path() string { return m.path }

// Append adds one marker line. It fsyncs, because the marker is the claim that
// licenses a drop: an unsynced claim is not a claim.
func (m *MarkerStore) Append(mk types.LedgerArchiveMarker) error {
	if m.path == "" {
		return errf(types.CodeHub009, ReasonArchiveCfg, "the marker store has no state root")
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err != nil {
		return errWrap(types.CodeHub009, ReasonArchiveCfg, "cannot create the archive dir", err)
	}
	b, err := json.Marshal(mk)
	if err != nil {
		return errWrap(types.CodeHub009, ReasonArchiveCfg, "cannot encode marker", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	f, err := os.OpenFile(m.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return errWrap(types.CodeHub009, ReasonArchiveCfg, "cannot open the marker file", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return errWrap(types.CodeHub009, ReasonArchiveCfg, "cannot append the marker", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return errWrap(types.CodeHub009, ReasonArchiveCfg, "cannot fsync the marker file", err)
	}
	return f.Close()
}

// Load reads every marker, tolerating a torn last line (SPEC-01's tolerant-reader
// discipline, applied here): a crash mid-append truncates the fragment instead of
// failing the read. A malformed line in the MIDDLE is skipped and counted, never
// guessed at.
func (m *MarkerStore) Load() ([]types.LedgerArchiveMarker, int, error) {
	if m.path == "" {
		return nil, 0, errf(types.CodeHub009, ReasonArchiveCfg, "the marker store has no state root")
	}
	f, err := os.Open(m.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, errWrap(types.CodeHub009, ReasonArchiveCfg, "cannot read the marker file", err)
	}
	defer f.Close()
	var out []types.LedgerArchiveMarker
	skipped := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	lastComplete := 0
	lines := make([]string, 0, 64)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	for i, line := range lines {
		if len(line) == 0 {
			lastComplete = i + 1
			continue
		}
		var mk types.LedgerArchiveMarker
		if err := json.Unmarshal([]byte(line), &mk); err != nil {
			// The LAST line may be torn by a crash; anything else is corruption
			// worth counting.
			if i == len(lines)-1 {
				break
			}
			skipped++
			continue
		}
		out = append(out, mk)
		lastComplete = i + 1
	}
	_ = lastComplete
	return out, skipped, nil
}

// AppendTransition appends a marker unless the SAME (marker_id, state)
// transition is already recorded, in which case it reports false.
//
// The marker file records STATE TRANSITIONS, not attempts: SPEC-13 §3.1 makes it
// append-only and never-rewritten, and a re-export of the same file computes the
// same content-derived id, so the same pair says nothing new on a second line.
// Repeated ATTEMPTS are counted by the archiver's own counters and by the queue.
func (m *MarkerStore) AppendTransition(mk types.LedgerArchiveMarker) (bool, error) {
	marks, _, err := m.Load()
	if err != nil {
		return false, err
	}
	for _, prev := range marks {
		if prev.MarkerID == mk.MarkerID && prev.State == mk.State {
			return false, nil
		}
	}
	return true, m.Append(mk)
}

// Latest returns the most recent marker for a generation file.
func (m *MarkerStore) Latest(file string) (types.LedgerArchiveMarker, bool) {
	marks, _, err := m.Load()
	if err != nil {
		return types.LedgerArchiveMarker{}, false
	}
	var out types.LedgerArchiveMarker
	found := false
	for _, mk := range marks {
		if mk.File == file {
			out = mk
			found = true
		}
	}
	return out, found
}

// ByMarkerID returns the marker with an id, if one exists.
func (m *MarkerStore) ByMarkerID(id string) (types.LedgerArchiveMarker, bool) {
	marks, _, err := m.Load()
	if err != nil {
		return types.LedgerArchiveMarker{}, false
	}
	for _, mk := range marks {
		if mk.MarkerID == id {
			return mk, true
		}
	}
	return types.LedgerArchiveMarker{}, false
}

// Summarize counts the markers by state for the status surface. It also counts
// how many markers are pending (state `pending`, i.e. claimed but not exported).
func (m *MarkerStore) Summarize() (total int64, pending int64, exported int64, verified int64, dropped int64, skipped int) {
	marks, sk, err := m.Load()
	if err != nil {
		return 0, 0, 0, 0, 0, 0
	}
	for _, mk := range marks {
		total++
		switch mk.State {
		case "pending":
			pending++
		case "exported":
			exported++
		case "verified":
			verified++
		case "dropped":
			dropped++
		}
	}
	return total, pending, exported, verified, dropped, sk
}
