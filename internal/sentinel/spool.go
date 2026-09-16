package sentinel

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Spool errors.
var (
	ErrSpoolFull  = errors.New("sentinel: spool budget exhausted")
	ErrSpoolEmpty = errors.New("sentinel: spool is empty")
)

// spoolEntryHeader is the pinned on-disk frame: seq(8) + length(4) + crc32c(4),
// all little-endian, followed by `length` bytes of JSON. The CRC32C + length
// prefix is what makes a crash mid-write detectable (§6.12).
const spoolEntryHeader = 16

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// SpoolEntry is one spooled event awaiting replay.
type SpoolEntry struct {
	Seq        uint64          `json:"seq"`
	Project    string          `json:"project"`
	SourceKind string          `json:"source_kind"`
	AuthForm   string          `json:"auth_form"`
	TS         string          `json:"ts"`
	ItemType   string          `json:"item_type"`
	Reason     string          `json:"reason"`
	Raw        json.RawMessage `json:"raw"`
}

// Spool is the loss-policy spool of §3.9: an append-only, CRC-framed file
// inside the pinned state root.
type Spool struct {
	mu           sync.Mutex
	path         string
	f            *os.File
	budget       int64
	bytes        int64
	refs         []spoolRef
	seq          uint64
	torn         uint64
	dropped      uint64
	droppedBytes int64
}

type spoolRef struct {
	off  int64
	size int64
}

// OpenSpool opens (creating if needed) the spool in dir and indexes it,
// counting torn entries rather than trusting the file's length.
func OpenSpool(dir string, budget int64) (*Spool, error) {
	if dir == "" {
		return nil, fmt.Errorf("sentinel: spool directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	sp := &Spool{path: filepath.Join(dir, "sentinel.spool"), budget: budget}
	if err := sp.scan(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(sp.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	sp.f = f
	return sp, nil
}

// Path is the spool file path (tests and docs use it).
func (s *Spool) Path() string { return s.path }

// scan reads the file, validating every frame and counting torn tails.
func (s *Spool) scan() error {
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	var off int64
	for {
		var hdr [spoolEntryHeader]byte
		n, err := io.ReadFull(f, hdr[:])
		if err == io.EOF {
			break
		}
		if err == io.ErrUnexpectedEOF {
			if n > 0 {
				s.torn++
			}
			break
		}
		if err != nil {
			return err
		}
		l := binary.LittleEndian.Uint32(hdr[8:12])
		crc := binary.LittleEndian.Uint32(hdr[12:16])
		body := make([]byte, l)
		if _, err := io.ReadFull(f, body); err != nil {
			s.torn++
			break
		}
		frame := spoolEntryHeader + int64(l)
		if crc32.Checksum(body, crcTable) != crc {
			// A CRC mismatch is a torn write, not data: the entry is dropped
			// with a gap note instead of being replayed (§6.12).
			s.torn++
			off += frame
			s.bytes += frame
			continue
		}
		s.refs = append(s.refs, spoolRef{off: off, size: frame})
		if seq := binary.LittleEndian.Uint64(hdr[0:8]); seq > s.seq {
			s.seq = seq
		}
		off += frame
		s.bytes += frame
	}
	return nil
}

// Append writes one entry, dropping the oldest entries first when the budget
// would be exceeded (drop-oldest-with-ledger-note, §3.9). It returns how many
// entries were dropped so the caller can write the ledger note.
func (s *Spool) Append(e SpoolEntry) (dropped int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return 0, errors.New("sentinel: spool is closed")
	}
	s.seq++
	e.Seq = s.seq
	body, err := json.Marshal(e)
	if err != nil {
		return 0, err
	}
	frame := make([]byte, spoolEntryHeader+len(body))
	binary.LittleEndian.PutUint64(frame[0:8], e.Seq)
	binary.LittleEndian.PutUint32(frame[8:12], uint32(len(body)))
	binary.LittleEndian.PutUint32(frame[12:16], crc32.Checksum(body, crcTable))
	copy(frame[spoolEntryHeader:], body)

	if s.budget > 0 && s.bytes+int64(len(frame)) > s.budget {
		dropped = s.dropOldestLocked(s.bytes + int64(len(frame)) - s.budget)
	}
	if _, err := s.f.Write(frame); err != nil {
		return dropped, err
	}
	if err := s.f.Sync(); err != nil {
		return dropped, err
	}
	s.refs = append(s.refs, spoolRef{off: s.bytes, size: int64(len(frame))})
	s.bytes += int64(len(frame))
	return dropped, nil
}

// dropOldestLocked rewrites the spool without its oldest entries until at least
// `need` bytes are free; the ledger note is the caller's job.
func (s *Spool) dropOldestLocked(need int64) int {
	if need <= 0 || len(s.refs) == 0 {
		return 0
	}
	freed := int64(0)
	drop := 0
	for drop < len(s.refs) && freed < need {
		freed += s.refs[drop].size
		drop++
	}
	s.rewriteLocked(drop)
	s.dropped += uint64(drop)
	s.droppedBytes += freed
	return drop
}

// rewriteLocked compacts the file, keeping every entry after the first `drop`.
func (s *Spool) rewriteLocked(drop int) {
	keep := s.refs[drop:]
	tmpPath := s.path + ".tmp"
	in, err := os.Open(s.path)
	if err != nil {
		return
	}
	out, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		in.Close()
		return
	}
	newIndex := make([]spoolRef, 0, len(keep))
	var off int64
	for _, ref := range keep {
		buf := make([]byte, ref.size)
		if _, err := in.ReadAt(buf, ref.off); err != nil {
			continue
		}
		if _, err := out.Write(buf); err != nil {
			out.Close()
			in.Close()
			os.Remove(tmpPath)
			return
		}
		newIndex = append(newIndex, spoolRef{off: off, size: ref.size})
		off += ref.size
	}
	in.Close()
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmpPath)
		return
	}
	out.Close()
	if s.f != nil {
		s.f.Close()
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		s.f, _ = os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		return
	}
	s.f, _ = os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	s.refs = newIndex
	s.bytes = off
}

// readAt reads one frame's body, returning ok=false for a torn frame.
func (s *Spool) readAt(f *os.File, ref spoolRef) (SpoolEntry, bool) {
	buf := make([]byte, ref.size)
	if _, err := f.ReadAt(buf, ref.off); err != nil {
		return SpoolEntry{}, false
	}
	body := buf[spoolEntryHeader:]
	if crc32.Checksum(body, crcTable) != binary.LittleEndian.Uint32(buf[12:16]) {
		return SpoolEntry{}, false
	}
	var e SpoolEntry
	if err := json.Unmarshal(body, &e); err != nil {
		return SpoolEntry{}, false
	}
	return e, true
}

// Drain reads and removes up to n of the oldest entries (the replay path).
func (s *Spool) Drain(n int) ([]SpoolEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.refs) == 0 {
		return nil, ErrSpoolEmpty
	}
	if n <= 0 || n > len(s.refs) {
		n = len(s.refs)
	}
	f, err := os.Open(s.path)
	if err != nil {
		return nil, err
	}
	out := make([]SpoolEntry, 0, n)
	for i := 0; i < n; i++ {
		e, ok := s.readAt(f, s.refs[i])
		if !ok {
			s.torn++
			continue
		}
		out = append(out, e)
	}
	f.Close()
	s.rewriteLocked(n)
	return out, nil
}

// Peek reads up to n of the oldest entries without removing them.
func (s *Spool) Peek(n int) ([]SpoolEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.refs) == 0 {
		return nil, ErrSpoolEmpty
	}
	if n <= 0 || n > len(s.refs) {
		n = len(s.refs)
	}
	f, err := os.Open(s.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := make([]SpoolEntry, 0, n)
	for i := 0; i < n; i++ {
		e, ok := s.readAt(f, s.refs[i])
		if !ok {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// Stats reports the spool accounting (bytes, budget, entries, torn, dropped).
func (s *Spool) Stats() (bytes, budget int64, entries int, torn, dropped uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytes, s.budget, len(s.refs), s.torn, s.dropped
}

// Bytes reports the current spool size.
func (s *Spool) Bytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytes
}

// Len counts the entries held.
func (s *Spool) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.refs)
}

// Close flushes and closes the spool.
func (s *Spool) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Sync()
	cerr := s.f.Close()
	s.f = nil
	if err != nil {
		return err
	}
	return cerr
}

// SpoolStats is the read-only spool view for the dashboard.
type SpoolStats struct {
	Bytes        int64  `json:"bytes"`
	BudgetBytes  int64  `json:"budget_bytes"`
	Entries      int    `json:"entries"`
	Torn         uint64 `json:"torn"`
	Dropped      uint64 `json:"dropped"`
	SpooledTotal uint64 `json:"spooled_total"`
}

// SpoolStats reads the spool accounting.
func (s *Server) SpoolStats() SpoolStats {
	if s.spool == nil {
		return SpoolStats{BudgetBytes: s.cfg.SpoolBudgetBytes}
	}
	bytes, budget, entries, torn, dropped := s.spool.Stats()
	return SpoolStats{
		Bytes: bytes, BudgetBytes: budget, Entries: entries,
		Torn: torn, Dropped: dropped, SpooledTotal: s.counters.spooled.Get(),
	}
}

// replaySpool drains the spool through the real ingestion path, oldest first,
// and stops when the project's quota is exhausted again (§3.9 "replay when
// under quota").
func (s *Server) replaySpool(now time.Time) int {
	if s.spool == nil {
		return 0
	}
	replayed := 0
	for {
		entries, err := s.spool.Peek(1)
		if err != nil || len(entries) == 0 {
			return replayed
		}
		e := entries[0]
		entry, ok := s.projects.project(e.Project)
		if !ok {
			// The project is gone from the config: this entry can never be
			// replayed, so it is dropped rather than blocking the spool.
			_, _ = s.spool.Drain(1)
			continue
		}
		if used := s.quota.usedNow(e.Project, now); used >= entry.proj.QuotaEPM {
			return replayed
		}
		drained, derr := s.spool.Drain(1)
		if derr != nil || len(drained) == 0 {
			return replayed
		}
		var obj map[string]any
		if err := json.Unmarshal(e.Raw, &obj); err != nil {
			continue
		}
		ev := parseSentryEvent(obj)
		ev.Project = e.Project
		ev.SourceKind = e.SourceKind
		ev.AuthForm = e.AuthForm
		ev.TS = e.TS
		if _, aerr := s.admitEvent(context.Background(), entry, ev, e.ItemType, "spool_replay"); aerr != nil {
			// A refused replay is dropped; the spool must never wedge.
			continue
		}
		replayed++
	}
}
