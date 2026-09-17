package lifecycle

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// spoolSegmentFooter is the last line of a sealed segment.
type spoolSegmentFooter struct {
	Kind    string `json:"kind"`
	Records int    `json:"records"`
	FromSeq uint64 `json:"from_seq"`
	ToSeq   uint64 `json:"to_seq"`
	CRC32   string `json:"crc32"`
}

// SpoolState is persisted to forward.state.
type SpoolState struct {
	AckHubSeq    uint64            `json:"ack_hub_seq"`
	AckLocalSeq  uint64            `json:"ack_local_seq"`
	LocalMap     map[string]string `json:"local_map"`
	DroppedTotal uint64            `json:"dropped_total"`
	Bytes        int64             `json:"bytes"`
}

// Spool is the bounded satellite forward queue (SPEC-12 §3.7).
type Spool struct {
	mu        sync.Mutex
	cfg       Config
	root      string
	state     SpoolState
	open      *os.File
	openSeq   uint64
	records   int
	window    time.Duration
	lastFsync time.Time
}

// OpenSpool opens or creates the spool.
func OpenSpool(cfg Config) (*Spool, error) {
	root := filepath.Join(cfg.StateRoot, "spool", "forward")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("%w: cannot create spool dir: %v", types.CodeLifecycle004, err)
	}
	s := &Spool{
		cfg:    cfg,
		root:   root,
		window: time.Duration(cfg.Spool.FsyncWindowMS) * time.Millisecond,
		state: SpoolState{
			LocalMap: make(map[string]string),
		},
	}
	statePath := filepath.Join(root, "forward.state")
	b, err := os.ReadFile(statePath)
	if err == nil {
		_ = json.Unmarshal(b, &s.state)
	}
	if s.state.LocalMap == nil {
		s.state.LocalMap = make(map[string]string)
	}
	return s, nil
}

// Append writes one SpoolEntry to the open segment.
func (s *Spool) Append(e types.SpoolEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open == nil {
		if err := s.newSegment(); err != nil {
			return err
		}
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if _, err := s.open.Write(line); err != nil {
		return err
	}
	s.records++
	s.state.Bytes += int64(len(line))
	return s.maybeFsync()
}

func (s *Spool) newSegment() error {
	name := filepath.Join(s.root, fmt.Sprintf("%010d.fwd", s.openSeq+1))
	f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	s.open = f
	s.openSeq++
	s.records = 0
	return nil
}

func (s *Spool) maybeFsync() error {
	if s.cfg.Spool.Fsync != "group" {
		return nil
	}
	now := time.Now()
	if now.Sub(s.lastFsync) < s.window {
		return nil
	}
	s.lastFsync = now
	return s.open.Sync()
}

// Seal closes the current segment with a footer+CRC.
func (s *Spool) Seal(fromSeq, toSeq uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open == nil {
		return nil
	}
	// Compute CRC over the file content so far.
	if _, err := s.open.Seek(0, 0); err != nil {
		return err
	}
	h := crc32.NewIEEE()
	if _, err := bufio.NewReader(s.open).WriteTo(h); err != nil {
		return err
	}
	footer := spoolSegmentFooter{
		Kind:    "segment_end",
		Records: s.records,
		FromSeq: fromSeq,
		ToSeq:   toSeq,
		CRC32:   fmt.Sprintf("%08x", h.Sum32()),
	}
	b, err := json.Marshal(footer)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if _, err := s.open.Write(b); err != nil {
		return err
	}
	if err := s.open.Sync(); err != nil {
		return err
	}
	if err := s.open.Close(); err != nil {
		return err
	}
	s.open = nil
	s.records = 0
	return nil
}

// ReadSealedSegments reads every sealed segment up to ackLocalSeq, returning batches of records.
func (s *Spool) ReadSealedSegments(batchRecords, batchBytes int) ([][]types.Record, []spoolSegmentFooter, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, nil, err
	}
	var segs []segmentFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".fwd") {
			continue
		}
		seqStr := strings.TrimSuffix(e.Name(), ".fwd")
		seq, err := strconv.ParseUint(seqStr, 10, 64)
		if err != nil {
			continue
		}
		segs = append(segs, segmentFile{name: e.Name(), seq: seq})
	}
	sort.Slice(segs, func(i, j int) bool { return segs[i].seq < segs[j].seq })

	var batches [][]types.Record
	var footers []spoolSegmentFooter
	var cur []types.Record
	var curBytes int
	for _, seg := range segs {
		path := filepath.Join(s.root, seg.name)
		recs, footer, ok, err := readSegment(path)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			// Drop bad-CRC segment, replaced by gap caller-side.
			footers = append(footers, footer)
			continue
		}
		if footer.ToSeq <= s.state.AckLocalSeq {
			continue
		}
		for _, r := range recs {
			b, _ := json.Marshal(r)
			if len(cur) >= batchRecords || curBytes+len(b) > batchBytes {
				batches = append(batches, cur)
				cur = nil
				curBytes = 0
			}
			cur = append(cur, r)
			curBytes += len(b)
		}
		footers = append(footers, footer)
	}
	if len(cur) > 0 {
		batches = append(batches, cur)
	}
	return batches, footers, nil
}

type segmentFile struct {
	name string
	seq  uint64
}

func readSegment(path string) ([]types.Record, spoolSegmentFooter, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, spoolSegmentFooter{}, false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	const maxScan = 4 << 20
	buf := make([]byte, maxScan)
	sc.Buffer(buf, maxScan)
	var lines [][]byte
	for sc.Scan() {
		lines = append(lines, append([]byte(nil), sc.Bytes()...))
	}
	if len(lines) == 0 {
		return nil, spoolSegmentFooter{}, true, nil
	}
	last := lines[len(lines)-1]
	var footer spoolSegmentFooter
	if err := json.Unmarshal(last, &footer); err != nil || footer.Kind != "segment_end" {
		// Open segment: torn last line tolerated.
		lines = lines[:len(lines)-1]
	}
	if footer.Kind == "segment_end" {
		// Verify CRC over preceding lines.
		h := crc32.NewIEEE()
		for i, l := range lines {
			if i > 0 {
				h.Write([]byte{'\n'})
			}
			h.Write(l)
		}
		want := fmt.Sprintf("%08x", h.Sum32())
		if footer.CRC32 != want {
			return nil, footer, false, nil
		}
		lines = lines[:len(lines)-1]
	}
	var recs []types.Record
	for _, l := range lines {
		var rec types.Record
		if err := json.Unmarshal(l, &rec); err != nil {
			continue
		}
		recs = append(recs, rec)
	}
	return recs, footer, true, nil
}

// Trim deletes sealed segments whose to_seq <= ackLocalSeq.
func (s *Spool) Trim(ackLocalSeq uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".fwd") {
			continue
		}
		path := filepath.Join(s.root, e.Name())
		_, footer, ok, err := readSegment(path)
		if err != nil {
			continue
		}
		if !ok {
			continue
		}
		if footer.ToSeq <= ackLocalSeq {
			_ = os.Remove(path)
		}
	}
	return nil
}

// SaveState persists forward.state.
func (s *Spool) SaveState() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.Marshal(s.state)
	if err != nil {
		return err
	}
	path := filepath.Join(s.root, "forward.state")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// State returns a copy of the current spool state.
func (s *Spool) State() SpoolState {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := s.state
	cp.LocalMap = make(map[string]string, len(s.state.LocalMap))
	for k, v := range s.state.LocalMap {
		cp.LocalMap[k] = v
	}
	return cp
}

// EnforceBudget drops oldest sealed segments if the spool exceeds budget.
func (s *Spool) EnforceBudget() ([]types.GapRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var gaps []types.GapRecord
	budget := s.cfg.Spool.BudgetBytes
	reserve := s.cfg.Spool.GapReserveBytes
	for {
		sz, err := dirSize(s.root)
		if err != nil {
			return gaps, err
		}
		if sz <= budget {
			break
		}
		seg, err := oldestSegment(s.root)
		if err != nil {
			break
		}
		path := filepath.Join(s.root, seg.name)
		recs, footer, ok, err := readSegment(path)
		if err != nil {
			break
		}
		_ = recs
		if !ok {
			footer.CRC32 = "bad"
		}
		fromTS, toTS := "", ""
		if len(recs) > 0 {
			fromTS = recs[0].TS
			toTS = recs[len(recs)-1].TS
		}
		gaps = append(gaps, types.GapRecord{
			ID:      "gap_" + seg.name,
			Sensor:  "forward",
			Scope:   "spool_overflow",
			FromTS:  fromTS,
			ToTS:    toTS,
			EstLost: footer.Records,
			Cause:   "spool_overflow",
		})
		_ = os.Remove(path)
		s.state.DroppedTotal += uint64(footer.Records)
		// Stop if only gap reserve remains.
		sz2, _ := dirSize(s.root)
		if sz2 <= reserve {
			break
		}
	}
	return gaps, nil
}

func dirSize(path string) (int64, error) {
	var total int64
	err := filepath.WalkDir(path, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

func oldestSegment(root string) (segmentFile, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return segmentFile{}, err
	}
	var oldest segmentFile
	found := false
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".fwd") {
			continue
		}
		seqStr := strings.TrimSuffix(e.Name(), ".fwd")
		seq, err := strconv.ParseUint(seqStr, 10, 64)
		if err != nil {
			continue
		}
		if !found || seq < oldest.seq {
			oldest = segmentFile{name: e.Name(), seq: seq}
			found = true
		}
	}
	if !found {
		return oldest, fmt.Errorf("no segment")
	}
	return oldest, nil
}

// ForwardEnvelope wraps a batch for the hub.
func ForwardEnvelope(recs []types.Record, cfg Config, idem string) ([]byte, string, error) {
	var buf strings.Builder
	buf.WriteString(fmt.Sprintf("trouble-protocol-version: %d\n", cfg.Hub.ProtocolVersion))
	buf.WriteString(fmt.Sprintf("host_id: %s\n", cfg.Origin.HostID))
	buf.WriteString(fmt.Sprintf("hub_id: %s\n", cfg.Origin.HubID))
	buf.WriteString(fmt.Sprintf("idempotency_key: %s\n", idem))
	buf.WriteString(fmt.Sprintf("ack: %d\n", 0))
	buf.WriteString("Content-Type: application/json\n")
	buf.WriteString("\n")
	payload, err := json.Marshal(recs)
	if err != nil {
		return nil, "", err
	}
	gz, err := gzipEnvelope(payload)
	if err != nil {
		return nil, "", err
	}
	return gz, buf.String(), nil
}

func gzipEnvelope(data []byte) ([]byte, error) {
	var buf strings.Builder
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(data); err != nil {
		gw.Close()
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}
