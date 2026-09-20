package ledger

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// dayLayout is the UTC day naming used by every ledger file.
const dayLayout = "2006-01-02"

// fileRe is the boot naming check (SPEC-01 §3.4).
var fileRe = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})(?:\.p(\d{2}))?(?:\.(\d+)\.gen)?\.jsonl$`)

// partName renders the file name of a (day, generation, part) triple.
func partName(day string, gen, part int) string {
	switch {
	case gen > 0:
		return fmt.Sprintf("%s.%d.gen.jsonl", day, gen)
	case part <= 1:
		return day + ".jsonl"
	default:
		return fmt.Sprintf("%s.p%02d.jsonl", day, part)
	}
}

// seqOffset is one sparse sample per 512 lines (SPEC-01 §3.6).
type seqOffset struct {
	Seq    uint64 `json:"seq"`
	Offset int64  `json:"offset"`
	Line   int64  `json:"line"`
}

// partInfo describes one physical file of a day.
type partInfo struct {
	Day      string      `json:"day"`
	Gen      int         `json:"gen"`
	Part     int         `json:"part"`
	File     string      `json:"file"`
	Bytes    int64       `json:"bytes"`
	Records  int64       `json:"records"`
	FirstSeq uint64      `json:"first_seq"`
	LastSeq  uint64      `json:"last_seq"`
	FirstTS  string      `json:"first_ts"`
	LastTS   string      `json:"last_ts"`
	Samples  []seqOffset `json:"samples,omitempty"`
	Corrupt  int64       `json:"corrupt,omitempty"`
	Torn     int64       `json:"torn,omitempty"`
}

// dayEntry is one indexed day: its authoritative part set plus totals.
type dayEntry struct {
	Day      string
	Gen      int
	Parts    []*partInfo
	Bytes    int64
	Records  int64
	FirstSeq uint64
	LastSeq  uint64
	FirstTS  string
	LastTS   string
}

// hourRing is a bounded 24-bucket hourly counter ring (§3.6: no unbounded
// structure in the design).
type hourRing struct {
	buckets  [24]uint64
	lastHour int64
	seen     bool
}

func (r *hourRing) add(t time.Time, v uint64) {
	h := t.Unix() / 3600
	switch {
	case !r.seen:
		r.seen = true
		r.lastHour = h
	case h > r.lastHour:
		if h-r.lastHour >= 24 {
			r.buckets = [24]uint64{}
		} else {
			for m := r.lastHour + 1; m <= h; m++ {
				r.buckets[((m%24)+24)%24] = 0
			}
		}
		r.lastHour = h
	}
	r.buckets[((h%24)+24)%24] += v
}

// total sums the live 24-hour window ending at now.
func (r *hourRing) total(now time.Time) uint64 {
	if !r.seen {
		return 0
	}
	h := now.Unix() / 3600
	var sum uint64
	for i := int64(0); i < 24; i++ {
		m := h - i
		if m < r.lastHour-23 {
			break
		}
		sum += r.buckets[((m%24)+24)%24]
	}
	return sum
}

// sourceEntry is the per-source liveness projection.
type sourceEntry struct {
	age     types.SourceAge
	events  hourRing
	redact  hourRing
	dropped hourRing
	gaps    hourRing
}

// groupEntry is one dedup group plus its 60×1-minute ring.
type groupEntry struct {
	stat     types.GroupStat
	ring     [60]uint32
	lastMin  int64
	firstSeq uint64
	lastSeq  uint64
	bytes    int64
	slot     int  // index into the index's parallel rank-key arrays
	retired  bool // evicted cold: kept in the order slice, skipped by the sieve
}

// newGroupLocked creates and registers a group.
func (ix *index) newGroupLocked(digest string) *groupEntry {
	ge := &groupEntry{bytes: 512}
	ge.stat.Digest = digest
	ge.stat.GroupID = types.NewID(types.PGrp)
	ge.stat.Counters.SampleRate = 1
	ix.groups[digest] = ge
	ge.slot = len(ix.order)
	ix.order = append(ix.order, ge)
	ix.counts = append(ix.counts, 0)
	ix.rates = append(ix.rates, 0)
	ix.trends = append(ix.trends, 0)
	ix.retired = append(ix.retired, false)
	ix.indexBytes += 512 + 25
	return ge
}

// rankKeys refreshes a group's parallel rank keys. The top-N sieve streams a
// contiguous float64 array instead of walking a few hundred bytes per group,
// which is what keeps TopGroups inside its budget at the 50,000-group ceiling.
func (ix *index) rankKeys(ge *groupEntry) {
	if ge.slot < 0 || ge.slot >= len(ix.counts) {
		return
	}
	ix.counts[ge.slot] = float64(ge.stat.Count)
	ix.rates[ge.slot] = ge.stat.Rate1m
	ix.trends[ge.slot] = ge.stat.Trend
}

// index is the bounded in-memory index (SPEC-01 §3.6).
type index struct {
	opts      IndexOptions
	rootDir   string
	zone      string
	maxSchema int

	mu            sync.RWMutex
	groups        map[string]*groupEntry
	order         []*groupEntry // insertion order: the top-N sieve walks this
	counts        []float64     // parallel rank keys: the sieve streams these
	rates         []float64
	trends        []float64
	retired       []bool // parallel eviction flags: the sieve never derefs
	cold          map[string]*groupEntry
	sigToDigest   map[string]string
	mergeToDigest map[string]string
	incidents     map[string]*types.Incident
	openBySig     map[string]string
	incRing       []string
	refsByInc     map[string][]uint64
	refOverflow   map[string]int
	sources       map[string]*sourceEntry
	gaps          []types.GapRecord
	days          map[string]*dayEntry
	dayOrder      []string

	entries       int64
	indexBytes    int64
	coldEvictions int64
	coldWinStart  time.Time

	// dash is the bounded recent-record ring SPEC-10 §3.3 renders from
	// (dashread.go). It is projected in applyLocked, so a boot-time rebuild and a
	// live append go through exactly one code path.
	dash *dashRing

	lastSeq atomic.Uint64

	buildStats types.IndexStats
}

func newIndex(opts IndexOptions, rootDir, zone string, maxSchema int) *index {
	if maxSchema <= 0 {
		maxSchema = SchemaVersionV1
	}
	if zone == "" {
		zone = "loopback"
	}
	return &index{
		opts:          opts,
		rootDir:       rootDir,
		zone:          zone,
		maxSchema:     maxSchema,
		groups:        map[string]*groupEntry{},
		cold:          map[string]*groupEntry{},
		sigToDigest:   map[string]string{},
		mergeToDigest: map[string]string{},
		incidents:     map[string]*types.Incident{},
		openBySig:     map[string]string{},
		refsByInc:     map[string][]uint64{},
		refOverflow:   map[string]int{},
		sources:       map[string]*sourceEntry{},
		days:          map[string]*dayEntry{},
		dash:          newDashRing(),
		buildStats: types.IndexStats{
			BudgetMS:    opts.BuildBudgetMS,
			BudgetBytes: opts.BuildBudgetBytes,
		},
	}
}

// ---- day / part bookkeeping ----

func (ix *index) addPart(p partInfo) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.addPartLocked(p)
	ix.buildStats.FilesScanned++
}

func (ix *index) addPartLocked(p partInfo) {
	de := ix.days[p.Day]
	if de == nil {
		de = &dayEntry{Day: p.Day, Gen: p.Gen}
		ix.days[p.Day] = de
		ix.dayOrder = append(ix.dayOrder, p.Day)
		sort.Strings(ix.dayOrder)
	}
	for _, ex := range de.Parts {
		if ex.File == p.File {
			return
		}
	}
	cp := p
	de.Parts = append(de.Parts, &cp)
	sortParts(de.Parts)
	ix.recomputeDayLocked(de)
	ix.indexBytes += int64(len(p.File)) + 128
}

func sortParts(ps []*partInfo) {
	sort.Slice(ps, func(i, j int) bool {
		if ps[i].Gen != ps[j].Gen {
			return ps[i].Gen < ps[j].Gen
		}
		return ps[i].Part < ps[j].Part
	})
}

func (ix *index) recomputeDayLocked(de *dayEntry) {
	de.Bytes, de.Records = 0, 0
	de.FirstSeq, de.LastSeq = 0, 0
	de.FirstTS, de.LastTS = "", ""
	for _, p := range de.Parts {
		de.Bytes += p.Bytes
		de.Records += p.Records
		if p.FirstSeq != 0 && (de.FirstSeq == 0 || p.FirstSeq < de.FirstSeq) {
			de.FirstSeq = p.FirstSeq
		}
		if p.LastSeq > de.LastSeq {
			de.LastSeq = p.LastSeq
		}
		if p.FirstTS != "" && (de.FirstTS == "" || p.FirstTS < de.FirstTS) {
			de.FirstTS = p.FirstTS
		}
		if p.LastTS > de.LastTS {
			de.LastTS = p.LastTS
		}
	}
}

// fileAppended advances the counters of the part the writer just extended.
func (ix *index) fileAppended(day string, gen, part int, nbytes, nrecords int64, lastSeq uint64) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	name := partName(day, gen, part)
	de := ix.days[day]
	if de == nil {
		ix.addPartLocked(partInfo{Day: day, Gen: gen, Part: part, File: name})
		de = ix.days[day]
	}
	found := false
	for _, p := range de.Parts {
		if p.File == name {
			p.Bytes += nbytes
			p.Records += nrecords
			if p.FirstSeq == 0 {
				p.FirstSeq = lastSeq
			}
			if lastSeq > p.LastSeq {
				p.LastSeq = lastSeq
			}
			found = true
		}
	}
	if !found {
		de.Parts = append(de.Parts, &partInfo{
			Day: day, Gen: gen, Part: part, File: name,
			Bytes: nbytes, Records: nrecords, FirstSeq: lastSeq, LastSeq: lastSeq,
		})
		sortParts(de.Parts)
	}
	ix.recomputeDayLocked(de)
	if lastSeq > ix.lastSeq.Load() {
		ix.lastSeq.Store(lastSeq)
	}
}

func (ix *index) partStats(day string, gen, part int) (bytes, records int64, lastSeq uint64) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	de := ix.days[day]
	if de == nil {
		return 0, 0, 0
	}
	for _, p := range de.Parts {
		if p.Gen == gen && p.Part == part {
			return p.Bytes, p.Records, p.LastSeq
		}
	}
	return 0, 0, 0
}

func (ix *index) dayEntries() []*dayEntry {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	out := make([]*dayEntry, 0, len(ix.dayOrder))
	for _, d := range ix.dayOrder {
		if de := ix.days[d]; de != nil {
			cp := *de
			cp.Parts = append([]*partInfo(nil), de.Parts...)
			out = append(out, &cp)
		}
	}
	return out
}

// partIndex returns a snapshot of every indexed part keyed by file name. The
// §2.3a page walk needs it because a part discovered after the boot rebuild (or
// one the rebuild scanned in a projected mode) may not carry its Samples on the
// authoritativeFiles() copy, and the index's copy is the one the rebuild filled.
func (ix *index) partIndex() map[string]partInfo {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	out := make(map[string]partInfo, len(ix.days))
	for _, d := range ix.dayOrder {
		de := ix.days[d]
		if de == nil {
			continue
		}
		for _, p := range de.Parts {
			out[p.File] = *p
		}
	}
	return out
}

func (ix *index) day(day string) *dayEntry {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	de := ix.days[day]
	if de == nil {
		return nil
	}
	cp := *de
	cp.Parts = append([]*partInfo(nil), de.Parts...)
	return &cp
}

func (ix *index) replaceDay(day string, parts []*partInfo, gen int) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	de := ix.days[day]
	if de == nil {
		de = &dayEntry{Day: day, Gen: gen}
		ix.days[day] = de
		ix.dayOrder = append(ix.dayOrder, day)
		sort.Strings(ix.dayOrder)
	}
	old := int64(0)
	for _, p := range de.Parts {
		old += int64(len(p.File)) + 128
	}
	de.Parts = parts
	de.Gen = gen
	ix.recomputeDayLocked(de)
	ix.indexBytes -= old
	for _, p := range parts {
		ix.indexBytes += int64(len(p.File)) + 128
	}
	ix.buildStats.Days = len(ix.days)
}

func (ix *index) removeDay(day string) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	de := ix.days[day]
	if de == nil {
		return
	}
	delete(ix.days, day)
	ix.dayOrder = removeString(ix.dayOrder, day)
	ix.buildStats.Days = len(ix.days)
}

// findPartForSeq locates the authoritative part holding seq.
func (ix *index) findPartForSeq(seq uint64) (*partInfo, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	for _, d := range ix.dayOrder {
		de := ix.days[d]
		if de == nil || de.FirstSeq == 0 || seq < de.FirstSeq || seq > de.LastSeq {
			continue
		}
		for _, p := range de.Parts {
			if p.FirstSeq != 0 && seq >= p.FirstSeq && seq <= p.LastSeq {
				cp := *p
				return &cp, true
			}
		}
	}
	return nil, false
}

// ---- the boot rebuild ----

// build walks the authoritative file set in seq order and fills the index,
// applying the §3.6 degradation ladder. It never fails the boot.
func (ix *index) build(files []*partInfo) error {
	start := time.Now()
	projected := int64(0)
	days := map[string]int64{}
	for _, p := range files {
		projected += p.Bytes
		days[p.Day] += p.Bytes
	}
	order := make([]string, 0, len(days))
	for d := range days {
		order = append(order, d)
	}
	sort.Strings(order)
	hot := map[string]bool{}
	for i := len(order) - 1; i >= 0 && len(order)-1-i < ix.opts.HotDays; i-- {
		hot[order[i]] = true
	}
	hotBytes := int64(0)
	for d := range hot {
		hotBytes += days[d]
	}
	mode := "full"
	switch {
	case projected <= ix.opts.BuildBudgetBytes:
		mode = "full"
	case hotBytes <= ix.opts.BuildBudgetBytes:
		mode = "tiered"
	default:
		mode = "bounded"
	}

	var bytesScanned, linesScanned int64
	var torn, corrupt, skipped int
	var firstErr error
	ix.mu.Lock()
	ix.buildStats.FilesScanned = 0
	ix.mu.Unlock()
	for _, p := range files {
		full := mode == "full" || hot[p.Day]
		skipEvents := false
		if mode == "tiered" && !hot[p.Day] {
			skipEvents = true
		}
		if mode == "bounded" {
			skipEvents = true
		}
		b, ln, tc, cc, sc, err := ix.scanFile(p, full, skipEvents)
		bytesScanned += b
		linesScanned += ln
		torn += tc
		corrupt += cc
		skipped += sc
		if err != nil && firstErr == nil {
			firstErr = err
		}
		// Register the scanned part so Resolve, partStats and the day byte
		// accounting see the recovered file set before the writer starts.
		ix.mu.Lock()
		ix.addPartLocked(*p)
		ix.buildStats.FilesScanned++
		ix.mu.Unlock()
	}
	elapsed := time.Since(start)
	rate := 0.0
	if elapsed > 0 {
		rate = (float64(bytesScanned) / (1024 * 1024)) / elapsed.Seconds()
	}
	ix.mu.Lock()
	ix.buildStats.BytesScanned = bytesScanned
	ix.buildStats.LinesScanned = linesScanned
	ix.buildStats.TornLines = torn
	ix.buildStats.CorruptLines = corrupt
	ix.buildStats.SkippedNewerSchema = skipped
	ix.buildStats.BuildMS = int(elapsed.Milliseconds())
	ix.buildStats.ScanRateMiBs = rate
	ix.buildStats.Entries = int(ix.entries)
	ix.buildStats.Groups = len(ix.groups) + len(ix.cold)
	ix.buildStats.GroupsCold = len(ix.cold)
	ix.buildStats.Incidents = len(ix.incidents)
	ix.buildStats.Sources = len(ix.sources)
	ix.buildStats.Days = len(ix.days)
	ix.buildStats.IndexBytes = ix.indexBytes
	switch {
	case firstErr != nil:
		ix.buildStats.Degraded = true
		ix.buildStats.DegradedReason = ReasonIO
	case mode != "full":
		ix.buildStats.Degraded = true
		ix.buildStats.DegradedReason = ReasonBudgetExceed
	case skipped > 0:
		ix.buildStats.Degraded = true
		ix.buildStats.DegradedReason = ReasonVersionGate
	}
	if mode == "bounded" && len(order) > ix.opts.HotDays {
		ix.buildStats.TruncatedBeforeTS = order[len(order)-ix.opts.HotDays] + "T00:00:00.000Z"
	}
	ix.mu.Unlock()
	return firstErr
}

// scanFile indexes one part. full=false uses the projection decode (header
// fields only, payload bytes skipped); skipEvents drops event/canary lines.
func (ix *index) scanFile(p *partInfo, full, skipEvents bool) (bytesScanned, lines int64, torn, corrupt, skipped int, err error) {
	path := filepath.Join(ix.rootDir, p.File)
	f, ferr := os.Open(path)
	if ferr != nil {
		return 0, 0, 0, 0, 0, ledgerErr(types.CodeLedger008, ReasonIO,
			fmt.Sprintf("cannot read %s", p.File), ferr)
	}
	defer f.Close()
	if fi, serr := f.Stat(); serr == nil {
		bytesScanned = fi.Size()
	}
	var lineNo int64
	werr := walkLines(f, func(line []byte, start int64, isTail bool) error {
		if len(bytes.TrimSpace(line)) == 0 {
			// a healed torn tail leaves one blank line behind (§6.4)
			return nil
		}
		lineNo++
		rec, derr := decodeProjected(line)
		if derr != nil || rec == nil {
			if isTail {
				torn++
			} else {
				corrupt++
			}
			return nil
		}
		if rec.SchemaVersion > ix.maxSchema {
			skipped++
			return nil
		}
		if skipEvents && (rec.Kind == types.KEvent || rec.Kind == types.KCanary) {
			return nil
		}
		if p.FirstSeq == 0 {
			p.FirstSeq = rec.Seq
		}
		if rec.Seq > p.LastSeq {
			p.LastSeq = rec.Seq
		}
		if p.FirstTS == "" {
			p.FirstTS = rec.TS
		}
		p.LastTS = rec.TS
		p.Records++
		if lineNo%512 == 0 {
			p.Samples = append(p.Samples, seqOffset{Seq: rec.Seq, Offset: start, Line: lineNo})
		}
		ix.mu.Lock()
		ix.applyLocked(rec)
		ix.mu.Unlock()
		lines++
		return nil
	})
	if werr != nil {
		return bytesScanned, lines, torn, corrupt, skipped,
			ledgerErr(types.CodeLedger008, ReasonIO, fmt.Sprintf("read %s failed", p.File), werr)
	}
	p.Torn = int64(torn)
	p.Corrupt = int64(corrupt)
	return bytesScanned, lines, torn, corrupt, skipped, nil
}

// ---- decoding ----

type recordHeader struct {
	Seq           uint64           `json:"seq"`
	RecID         string           `json:"rec_id"`
	TS            string           `json:"ts"`
	Kind          types.RecordKind `json:"kind"`
	SchemaVersion int              `json:"schema_version"`
	Sig           string           `json:"sig"`
	Inc           string           `json:"inc"`
	Origin        types.Origin     `json:"origin"`
	Actor         types.Actor      `json:"actor"`
	Redactions    int              `json:"redactions"`
	Payload       json.RawMessage  `json:"payload"`
}

// projectedPayload is the §3.6 projection decode: exactly the payload fields
// the index needs, so a boot rebuild never materializes event payloads.
type projectedPayload struct {
	Digest     string  `json:"digest"`
	MergeKey   string  `json:"merge_key"`
	Subject    string  `json:"subject"`
	Title      string  `json:"title"`
	GroupID    string  `json:"group_id"`
	IncidentID string  `json:"incident_id"`
	Count      float64 `json:"count"`
	Counters   *struct {
		Events     float64 `json:"events"`
		Suppressed float64 `json:"suppressed"`
		Redacted   float64 `json:"redacted_values"`
		Dropped    float64 `json:"dropped_events"`
		SampleRate float64 `json:"sample_rate"`
	} `json:"counters"`
	Op        string `json:"op"`
	Aggregate bool   `json:"aggregate"`
	Dropped   bool   `json:"dropped"`

	ID          string          `json:"id"`
	State       string          `json:"state"`
	Grp         string          `json:"grp"`
	Severity    string          `json:"severity"`
	EntryRung   string          `json:"entry_rung"`
	Rung        string          `json:"rung"`
	OpenedTS    string          `json:"opened_ts"`
	ResolvedTS  string          `json:"resolved_ts"`
	ReopenCount float64         `json:"reopen_count"`
	PlayRuns    float64         `json:"play_runs"`
	AgentRuns   float64         `json:"agent_runs"`
	LeaseID     string          `json:"lease_id"`
	IssueID     string          `json:"issue_id"`
	TaskID      string          `json:"task_id"`
	ResearchID  string          `json:"research_id"`
	VerifyWin   string          `json:"verify_window"`
	Incident    json.RawMessage `json:"incident"`

	Sensor  string  `json:"sensor"`
	Scope   string  `json:"scope"`
	FromTS  string  `json:"from_ts"`
	ToTS    string  `json:"to_ts"`
	Cause   string  `json:"cause"`
	EstLost float64 `json:"est_lost"`

	FirstSeenTS string `json:"first_seen_ts"`
	LastSeenTS  string `json:"last_seen_ts"`
}

func (p projectedPayload) toMap() map[string]any {
	m := make(map[string]any, 8)
	if p.Digest != "" {
		m["digest"] = p.Digest
	}
	if p.MergeKey != "" {
		m["merge_key"] = p.MergeKey
	}
	if p.Subject != "" {
		m["subject"] = p.Subject
	}
	if p.Title != "" {
		m["title"] = p.Title
	}
	if p.GroupID != "" {
		m["group_id"] = p.GroupID
	}
	if p.IncidentID != "" {
		m["incident_id"] = p.IncidentID
	}
	if p.Count != 0 {
		m["count"] = p.Count
	}
	if p.Counters != nil {
		m["counters"] = map[string]any{
			"events":          p.Counters.Events,
			"suppressed":      p.Counters.Suppressed,
			"redacted_values": p.Counters.Redacted,
			"dropped_events":  p.Counters.Dropped,
			"sample_rate":     p.Counters.SampleRate,
		}
	}
	if p.Op != "" {
		m["op"] = p.Op
	}
	if p.Aggregate {
		m["aggregate"] = true
	}
	if p.Dropped {
		m["dropped"] = true
	}
	if p.ID != "" {
		m["id"] = p.ID
	}
	if p.State != "" {
		m["state"] = p.State
	}
	if p.Grp != "" {
		m["grp"] = p.Grp
	}
	if p.Severity != "" {
		m["severity"] = p.Severity
	}
	if p.EntryRung != "" {
		m["entry_rung"] = p.EntryRung
	}
	if p.Rung != "" {
		m["rung"] = p.Rung
	}
	if p.OpenedTS != "" {
		m["opened_ts"] = p.OpenedTS
	}
	if p.ResolvedTS != "" {
		m["resolved_ts"] = p.ResolvedTS
	}
	if p.ReopenCount != 0 {
		m["reopen_count"] = p.ReopenCount
	}
	if p.PlayRuns != 0 {
		m["play_runs"] = p.PlayRuns
	}
	if p.AgentRuns != 0 {
		m["agent_runs"] = p.AgentRuns
	}
	if p.LeaseID != "" {
		m["lease_id"] = p.LeaseID
	}
	if p.IssueID != "" {
		m["issue_id"] = p.IssueID
	}
	if p.TaskID != "" {
		m["task_id"] = p.TaskID
	}
	if p.ResearchID != "" {
		m["research_id"] = p.ResearchID
	}
	if p.VerifyWin != "" {
		m["verify_window"] = p.VerifyWin
	}
	if len(p.Incident) > 0 {
		var raw any
		if err := json.Unmarshal(p.Incident, &raw); err == nil {
			m["incident"] = raw
		}
	}
	if p.Sensor != "" {
		m["sensor"] = p.Sensor
	}
	if p.Scope != "" {
		m["scope"] = p.Scope
	}
	if p.FromTS != "" {
		m["from_ts"] = p.FromTS
	}
	if p.ToTS != "" {
		m["to_ts"] = p.ToTS
	}
	if p.Cause != "" {
		m["cause"] = p.Cause
	}
	if p.EstLost != 0 {
		m["est_lost"] = p.EstLost
	}
	if p.LastSeenTS != "" {
		m["last_seen_ts"] = p.LastSeenTS
	}
	if p.FirstSeenTS != "" {
		m["first_seen_ts"] = p.FirstSeenTS
	}
	return m
}

// decodeProjected decodes a line through the index projection.
func decodeProjected(line []byte) (*types.Record, error) {
	var h struct {
		Seq           uint64           `json:"seq"`
		RecID         string           `json:"rec_id"`
		TS            string           `json:"ts"`
		Kind          types.RecordKind `json:"kind"`
		SchemaVersion int              `json:"schema_version"`
		Sig           string           `json:"sig"`
		Inc           string           `json:"inc"`
		Origin        types.Origin     `json:"origin"`
		Actor         types.Actor      `json:"actor"`
		Redactions    int              `json:"redactions"`
		Payload       projectedPayload `json:"payload"`
	}
	if err := json.Unmarshal(line, &h); err != nil {
		return nil, err
	}
	return &types.Record{
		Seq: h.Seq, RecID: h.RecID, TS: h.TS, Kind: h.Kind,
		SchemaVersion: h.SchemaVersion, Sig: h.Sig, Inc: h.Inc,
		Origin: h.Origin, Actor: h.Actor, Redactions: h.Redactions,
		Payload: h.Payload.toMap(),
	}, nil
}

// decodeRecord decodes a line. full=true materializes the payload; full=false
// keeps the payload bytes unparsed (the §3.6 projection decode).
func decodeRecord(line []byte, full bool) (*types.Record, error) {
	var h recordHeader
	if err := json.Unmarshal(line, &h); err != nil {
		return nil, err
	}
	rec := &types.Record{
		Seq: h.Seq, RecID: h.RecID, TS: h.TS, Kind: h.Kind,
		SchemaVersion: h.SchemaVersion, Sig: h.Sig, Inc: h.Inc,
		Origin: h.Origin, Actor: h.Actor, Redactions: h.Redactions,
	}
	if full && len(h.Payload) > 0 {
		var m map[string]any
		if err := json.Unmarshal(h.Payload, &m); err != nil {
			return nil, err
		}
		rec.Payload = m
	}
	return rec, nil
}

// ---- apply ----

func (ix *index) applyBatch(recs []types.Record) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	for i := range recs {
		ix.applyLocked(&recs[i])
	}
}

func (ix *index) applyLocked(rec *types.Record) {
	ix.entries++
	if rec.Payload == nil {
		rec.Payload = map[string]any{}
	}
	if rec.Seq > ix.lastSeq.Load() {
		ix.lastSeq.Store(rec.Seq)
	}
	ts, terr := types.ParseUTC(rec.TS)
	if terr != nil {
		ts = time.Unix(0, 0)
	}
	ix.touchSource(rec, ts)
	if rec.Inc != "" {
		ix.addRefLocked(rec.Inc, rec.Seq)
	}
	switch rec.Kind {
	case types.KEvent, types.KCanary:
		ix.applyEventLocked(rec, ts)
	case types.KGroup:
		ix.applyGroupLocked(rec, ts)
	case types.KIncident:
		ix.applyIncidentLocked(rec, ts)
	case types.KGap:
		ix.applyGapLocked(rec)
	}
	// The dashboard's bounded render ring (SPEC-10 §3.3) is projected here so a
	// boot rebuild and a live append cannot diverge.
	if ix.dash != nil {
		ix.dash.add(rec, ts)
	}
}

func isCompactionAggregate(rec *types.Record) bool {
	if v, ok := rec.Payload["op"].(string); ok && v == "compaction_aggregate" {
		return true
	}
	if v, ok := rec.Payload["aggregate"].(bool); ok && v {
		return true
	}
	return false
}

// groupKeyOf resolves a record's group key. The precedence is the dedup core's:
// an explicit full 64-hex digest wins, then the cross-source merge key (which is
// how three arrival paths sharing one bug class collapse into one group, AC-22),
// then the source-scoped sig. Keys are namespaced so a merge key can never
// collide with a digest or a sig.
func groupKeyOf(rec *types.Record) (digest, mergeKey, title string) {
	if v, ok := rec.Payload["digest"].(string); ok && v != "" {
		digest = v
	}
	if v, ok := rec.Payload["merge_key"].(string); ok && v != "" {
		mergeKey = v
	}
	if digest == "" && mergeKey != "" {
		digest = "mk:" + mergeKey
	}
	if digest == "" {
		digest = sigDigestKey(rec.Sig)
	}
	if v, ok := rec.Payload["title"].(string); ok {
		title = v
	}
	if v, ok := rec.Payload["subject"].(string); ok && title == "" {
		title = v
	}
	return
}

// sigDigestKey namespaces a sig-keyed group so it can never collide with a
// 64-hex digest.
func sigDigestKey(sig string) string {
	if sig == "" {
		return ""
	}
	return "sig:" + sig
}

func (ix *index) applyEventLocked(rec *types.Record, ts time.Time) {
	digest, mergeKey, title := groupKeyOf(rec)
	if digest == "" {
		return
	}
	ge := ix.groups[digest]
	if ge == nil {
		ix.evictIfNeededLocked()
		ge = ix.newGroupLocked(digest)
		ge.stat.Source = strings.SplitN(rec.Origin.Source, ":", 2)[0]
		ge.stat.FirstSeenTS = rec.TS
		ge.firstSeq = rec.Seq
	}
	ge.stat.Sig = rec.Sig
	if mergeKey != "" {
		ge.stat.MergeKey = mergeKey
		ix.mergeToDigest[mergeKey] = digest
	}
	if title != "" {
		ge.stat.Title = title
	}
	if rec.Inc != "" {
		ge.stat.IncidentID = rec.Inc
	}
	ge.stat.Count++
	ge.stat.Counters.Events++
	ge.stat.Counters.Redacted += uint64(rec.Redactions)
	ge.stat.LastSeenTS = rec.TS
	ge.lastSeq = rec.Seq
	ringAdd(ge, ts)
	ix.rankKeys(ge)
	if rec.Sig != "" {
		ix.sigToDigest[rec.Sig] = digest
	}
}

func (ix *index) applyGroupLocked(rec *types.Record, ts time.Time) {
	digest, mergeKey, title := groupKeyOf(rec)
	if digest == "" {
		return
	}
	ge := ix.groups[digest]
	if ge == nil {
		ge = ix.newGroupLocked(digest)
	}
	ge.stat.Sig = rec.Sig
	if mergeKey != "" {
		ge.stat.MergeKey = mergeKey
		ix.mergeToDigest[mergeKey] = digest
	}
	if title != "" {
		ge.stat.Title = title
	}
	if id, ok := rec.Payload["group_id"].(string); ok && id != "" {
		ge.stat.GroupID = id
	}
	if v, ok := rec.Payload["count"]; ok {
		ge.stat.Count = uint64(asNum(v))
	}
	if v, ok := rec.Payload["first_seen_ts"].(string); ok && v != "" {
		ge.stat.FirstSeenTS = v
	}
	if v, ok := rec.Payload["last_seen_ts"].(string); ok && v != "" {
		ge.stat.LastSeenTS = v
	}
	if v, ok := rec.Payload["incident_id"].(string); ok {
		ge.stat.IncidentID = v
	}
	if m, ok := rec.Payload["counters"].(map[string]any); ok {
		ge.stat.Counters = decodeCounters(m)
	}
	if isCompactionAggregate(rec) {
		ge.stat.Cold = false
	}
	if ge.stat.FirstSeenTS == "" {
		ge.stat.FirstSeenTS = rec.TS
	}
	if ge.stat.LastSeenTS == "" {
		ge.stat.LastSeenTS = rec.TS
	}
	if rec.Sig != "" {
		ix.sigToDigest[rec.Sig] = digest
	}
	_ = ts
}

func decodeCounters(m map[string]any) types.GroupCounters {
	return types.GroupCounters{
		Events:     uint64(asNum(m["events"])),
		Suppressed: uint64(asNum(m["suppressed"])),
		Redacted:   uint64(asNum(m["redacted_values"])),
		Dropped:    uint64(asNum(m["dropped_events"])),
		SampleRate: asNum(m["sample_rate"]),
	}
}

// asNum coerces a payload value to a number. A payload built in Go carries
// int/int64 while the same payload read back from JSON carries float64, so the
// index must accept both (the ledger is not allowed to care which path a record
// arrived through).
func asNum(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case int32:
		return float64(n)
	case uint64:
		return float64(n)
	case uint:
		return float64(n)
	default:
		return 0
	}
}

func (ix *index) applyIncidentLocked(rec *types.Record, ts time.Time) {
	id := rec.Inc
	if id == "" {
		if v, ok := rec.Payload["id"].(string); ok {
			id = v
		}
	}
	if id == "" {
		if v, ok := rec.Payload["incident_id"].(string); ok {
			id = v
		}
	}
	if id == "" {
		return
	}
	inc := ix.incidents[id]
	if inc == nil {
		ix.evictIncidentIfNeededLocked()
		inc = &types.Incident{ID: id, OpenedTS: rec.TS}
		ix.incidents[id] = inc
		ix.indexBytes += 384
	}
	if raw, ok := rec.Payload["incident"]; ok {
		if b, err := json.Marshal(raw); err == nil {
			var decoded types.Incident
			if err := json.Unmarshal(b, &decoded); err == nil && decoded.ID != "" {
				if decoded.OpenedTS == "" {
					decoded.OpenedTS = inc.OpenedTS
				}
				inc = &decoded
				ix.incidents[id] = inc
			}
		}
	}
	if inc.Sig == "" {
		inc.Sig = rec.Sig
	}
	if v, ok := rec.Payload["state"].(string); ok && v != "" {
		inc.State = types.LadderState(v)
	}
	if inc.State == "" {
		inc.State = types.StDetected
	}
	if v, ok := rec.Payload["grp"].(string); ok && v != "" {
		inc.GroupID = v
	}
	if v, ok := rec.Payload["severity"].(string); ok && v != "" {
		inc.Severity = types.Severity(v)
	}
	if v, ok := rec.Payload["entry_rung"].(string); ok && v != "" {
		inc.EntryRung = types.Rung(v)
	}
	if v, ok := rec.Payload["rung"].(string); ok && v != "" {
		inc.Rung = types.Rung(v)
	}
	if v, ok := rec.Payload["opened_ts"].(string); ok && v != "" {
		inc.OpenedTS = v
	}
	if v, ok := rec.Payload["resolved_ts"].(string); ok {
		inc.ResolvedTS = v
	}
	if v, ok := rec.Payload["reopen_count"]; ok {
		inc.ReopenCount = int(asNum(v))
	}
	if v, ok := rec.Payload["play_runs"]; ok {
		inc.PlayRuns = int(asNum(v))
	}
	if v, ok := rec.Payload["agent_runs"]; ok {
		inc.AgentRuns = int(asNum(v))
	}
	if v, ok := rec.Payload["lease_id"].(string); ok {
		inc.LeaseID = v
	}
	if v, ok := rec.Payload["issue_id"].(string); ok {
		inc.IssueID = v
	}
	if v, ok := rec.Payload["task_id"].(string); ok {
		inc.TaskID = v
	}
	if v, ok := rec.Payload["research_id"].(string); ok {
		inc.ResearchID = v
	}
	if v, ok := rec.Payload["verify_window"].(string); ok {
		inc.VerifyWin = types.Duration(v)
	}
	inc.UpdatedTS = rec.TS
	ix.reindexIncidentLocked(inc)
}

func (ix *index) reindexIncidentLocked(inc *types.Incident) {
	if inc.Sig != "" {
		if isOpenState(inc.State) {
			ix.openBySig[inc.Sig] = inc.ID
		} else if ix.openBySig[inc.Sig] == inc.ID {
			delete(ix.openBySig, inc.Sig)
		}
	}
	ix.incRing = append([]string{inc.ID}, removeString(ix.incRing, inc.ID)...)
	if len(ix.incRing) > ix.opts.IncidentRing {
		ix.incRing = ix.incRing[:ix.opts.IncidentRing]
	}
}

func isOpenState(s types.LadderState) bool {
	switch s {
	case types.StResolved, types.StSuppressed, types.StQuarantined, "":
		return false
	}
	return true
}

func removeString(in []string, v string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != v {
			out = append(out, s)
		}
	}
	return out
}

func (ix *index) applyGapLocked(rec *types.Record) {
	g := types.GapRecord{ID: rec.RecID, EstLost: -1}
	if v, ok := rec.Payload["sensor"].(string); ok {
		g.Sensor = v
	}
	if v, ok := rec.Payload["scope"].(string); ok {
		g.Scope = v
	}
	if v, ok := rec.Payload["from_ts"].(string); ok {
		g.FromTS = v
	}
	if v, ok := rec.Payload["to_ts"].(string); ok {
		g.ToTS = v
	}
	if v, ok := rec.Payload["cause"].(string); ok {
		g.Cause = v
	}
	if v, ok := rec.Payload["est_lost"]; ok {
		g.EstLost = int(asNum(v))
	}
	if g.ToTS == "" {
		g.ToTS = rec.TS
	}
	ix.gaps = append(ix.gaps, g)
	if len(ix.gaps) > 4096 {
		ix.gaps = ix.gaps[len(ix.gaps)-4096:]
	}
}

func (ix *index) addRefLocked(inc string, seq uint64) {
	if len(ix.refsByInc[inc]) >= ix.opts.IncPerIncident {
		ix.refOverflow[inc]++
		return
	}
	ix.refsByInc[inc] = append(ix.refsByInc[inc], seq)
	ix.indexBytes += 8
}

func (ix *index) touchSource(rec *types.Record, ts time.Time) {
	key := rec.Origin.HostID + ":" + rec.Origin.Source
	se := ix.sources[key]
	if se == nil {
		if len(ix.sources) >= ix.opts.MaxSources {
			return
		}
		se = &sourceEntry{}
		se.age.HostID = rec.Origin.HostID
		se.age.Source = rec.Origin.Source
		se.age.Zone = ix.zone
		ix.sources[key] = se
		ix.indexBytes += 192
	}
	if rec.Seq > se.age.LastSeq {
		se.age.LastSeq = rec.Seq
	}
	se.age.LastEventTS = rec.TS
	se.age.EventsTotal++
	se.events.add(ts, 1)
	if rec.Redactions > 0 {
		se.redact.add(ts, uint64(rec.Redactions))
	}
	if rec.Kind == types.KCanary {
		se.age.CanarySeen = true
		se.age.CanaryLastTS = rec.TS
	}
	if rec.Kind == types.KGap {
		se.gaps.add(ts, 1)
	}
	if rec.Kind == types.KEvent {
		if v, ok := rec.Payload["dropped"].(bool); ok && v {
			se.dropped.add(ts, 1)
		}
	}
}

// evictIfNeededLocked applies rung 4: the least-recently-seen group with no open
// incident goes cold, its counts preserved in a per-day aggregate row.
func (ix *index) evictIfNeededLocked() {
	if len(ix.groups) < ix.opts.MaxGroups {
		return
	}
	var victim string
	var victimTS string
	for k, ge := range ix.groups {
		if ge.stat.IncidentID != "" {
			continue
		}
		if victim == "" || ge.stat.LastSeenTS < victimTS {
			victim, victimTS = k, ge.stat.LastSeenTS
		}
	}
	if victim == "" {
		for k, ge := range ix.groups {
			if victim == "" || ge.stat.LastSeenTS < victimTS {
				victim, victimTS = k, ge.stat.LastSeenTS
			}
		}
	}
	if victim == "" {
		return
	}
	ge := ix.groups[victim]
	ge.stat.Cold = true
	ge.ring = [60]uint32{}
	ge.retired = true
	if ge.slot >= 0 && ge.slot < len(ix.retired) {
		ix.retired[ge.slot] = true
	}
	delete(ix.groups, victim)
	ix.cold[victim] = ge
	if ix.mergeToDigest[ge.stat.MergeKey] == victim {
		delete(ix.mergeToDigest, ge.stat.MergeKey)
	}
	if ix.sigToDigest[ge.stat.Sig] == victim {
		delete(ix.sigToDigest, ge.stat.Sig)
	}
	ix.coldEvictions++
	if ix.coldWinStart.IsZero() {
		ix.coldWinStart = time.Now()
	}
}

func (ix *index) evictIncidentIfNeededLocked() {
	if len(ix.incidents) < ix.opts.MaxIncidents {
		return
	}
	var victim string
	var victimTS string
	for k, in := range ix.incidents {
		if isOpenState(in.State) {
			continue
		}
		if victim == "" || in.UpdatedTS < victimTS {
			victim, victimTS = k, in.UpdatedTS
		}
	}
	if victim == "" {
		return
	}
	victimSig := ix.incidents[victim].Sig
	delete(ix.incidents, victim)
	delete(ix.refsByInc, victim)
	delete(ix.refOverflow, victim)
	if ix.openBySig[victimSig] == victim {
		delete(ix.openBySig, victimSig)
	}
}

func ringAdd(ge *groupEntry, ts time.Time) {
	m := ts.Unix() / 60
	if ge.lastMin == 0 {
		ge.lastMin = m
		ge.ring[((m%60)+60)%60]++
		computeRates(ge)
		return
	}
	if m > ge.lastMin {
		prev5 := 0.0
		for i := int64(5); i <= 9; i++ {
			prev5 += float64(ge.ring[(((ge.lastMin-i)%60)+60)%60])
		}
		if prev5 > 0 {
			ge.stat.Trend = (ge.stat.Rate5m / (prev5 / 5)) - 1
		}
		if m-ge.lastMin >= 60 {
			ge.ring = [60]uint32{}
		} else {
			for x := ge.lastMin + 1; x <= m; x++ {
				ge.ring[((x%60)+60)%60] = 0
			}
		}
		ge.lastMin = m
	}
	ge.ring[((m%60)+60)%60]++
	computeRates(ge)
}

func computeRates(ge *groupEntry) {
	m := ge.lastMin
	var s1, s5, s60 float64
	for i := int64(0); i < 60; i++ {
		v := float64(ge.ring[(((m-i)%60)+60)%60])
		s60 += v
		if i < 5 {
			s5 += v
		}
		if i == 0 {
			s1 = v
		}
	}
	ge.stat.Rate1m = s1
	ge.stat.Rate5m = s5 / 5
	ge.stat.Rate60m = s60 / 60
}

// ---- snapshots for the query surface ----

// topGroups is the bounded min-heap sieve behind TopGroups (SPEC-01 §2.3:
// O(G·log n), G = live groups). It streams the parallel rank-key array — a
// contiguous float64 scan — touches a group only when it wins a slot, and
// applies the window filter on the win path (rejecting a stale winner is
// equivalent to filtering it before the comparison and costs nothing while the
// common no-window call stays a pure key stream).
func (ix *index) topGroups(n int, threshold string, sortBy groupSort) []types.GroupStat {
	if n <= 0 {
		n = 10
	}
	ix.mu.RLock()
	defer ix.mu.RUnlock()

	var keys []float64
	switch sortBy {
	case sortRate:
		keys = ix.rates
	case sortTrend:
		keys = ix.trends
	default:
		keys = ix.counts
	}
	stale := func(ge *groupEntry) bool {
		return threshold != "" && ge.stat.LastSeenTS != "" && ge.stat.LastSeenTS < threshold
	}

	idx := make([]int, 0, n)
	hk := make([]float64, 0, n)
	siftUp := func(i int) {
		for i > 0 {
			parent := (i - 1) / 2
			if hk[parent] <= hk[i] {
				break
			}
			hk[parent], hk[i] = hk[i], hk[parent]
			idx[parent], idx[i] = idx[i], idx[parent]
			i = parent
		}
	}
	siftDown := func(i int) {
		for {
			l, r := 2*i+1, 2*i+2
			small := i
			if l < len(hk) && hk[l] < hk[small] {
				small = l
			}
			if r < len(hk) && hk[r] < hk[small] {
				small = r
			}
			if small == i {
				return
			}
			hk[small], hk[i] = hk[i], hk[small]
			idx[small], idx[i] = idx[i], idx[small]
			i = small
		}
	}
	push := func(slot int, k float64) {
		if len(idx) < n {
			idx = append(idx, slot)
			hk = append(hk, k)
			siftUp(len(idx) - 1)
			return
		}
		if k <= hk[0] {
			return
		}
		idx[0], hk[0] = slot, k
		siftDown(0)
	}

	for slot := range keys {
		if slot < len(ix.retired) && ix.retired[slot] {
			continue
		}
		k := keys[slot]
		if len(idx) < n {
			if stale(ix.order[slot]) {
				continue
			}
			push(slot, k)
			continue
		}
		if k <= hk[0] {
			continue
		}
		if stale(ix.order[slot]) {
			continue
		}
		push(slot, k)
	}
	// cold (evicted) groups carry no ring but still have counts; they are few
	for _, ge := range ix.cold {
		if stale(ge) {
			continue
		}
		keys2 := func() float64 {
			switch sortBy {
			case sortRate:
				return ge.stat.Rate1m
			case sortTrend:
				return ge.stat.Trend
			default:
				return float64(ge.stat.Count)
			}
		}()
		if len(idx) < n {
			idx = append(idx, ge.slot)
			hk = append(hk, keys2)
			siftUp(len(idx) - 1)
			continue
		}
		if keys2 <= hk[0] {
			continue
		}
		idx[0], hk[0] = ge.slot, keys2
		siftDown(0)
	}

	out := make([]types.GroupStat, 0, len(idx))
	for _, slot := range idx {
		if slot >= 0 && slot < len(ix.order) {
			out = append(out, ix.order[slot].stat)
		}
	}
	// order the winners by the same key, descending
	sort.Slice(out, func(i, j int) bool {
		switch sortBy {
		case sortRate:
			return out[i].Rate1m > out[j].Rate1m
		case sortTrend:
			return out[i].Trend > out[j].Trend
		default:
			return out[i].Count > out[j].Count
		}
	})
	return out
}

func (ix *index) groupByDigest(digest string) (types.GroupStat, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	if ge := ix.groups[digest]; ge != nil {
		return ge.stat, true
	}
	if ge := ix.cold[digest]; ge != nil {
		return ge.stat, true
	}
	return types.GroupStat{}, false
}

func (ix *index) digestForSig(sig string) (string, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	d, ok := ix.sigToDigest[sig]
	return d, ok
}

func (ix *index) digestForMergeKey(mk string) (string, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	d, ok := ix.mergeToDigest[mk]
	return d, ok
}

func (ix *index) incidentByID(id string) (*types.Incident, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	in := ix.incidents[id]
	if in == nil {
		return nil, false
	}
	cp := *in
	return &cp, true
}

// incidentForSig answers the reopen path: the open incident for a sig. A sig
// that never opened an incident still resolves through its group — that is how
// three arrival paths sharing one merge key reach one incident (SPEC-01 §4.4).
func (ix *index) incidentForSig(sig string) (*types.Incident, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	id := ix.openBySig[sig]
	if id == "" {
		if digest, ok := ix.sigToDigest[sig]; ok {
			if ge := ix.groups[digest]; ge != nil {
				id = ge.stat.IncidentID
			}
			if id == "" {
				if ge := ix.cold[digest]; ge != nil {
					id = ge.stat.IncidentID
				}
			}
		}
	}
	if id == "" {
		return nil, false
	}
	in := ix.incidents[id]
	if in == nil {
		return nil, false
	}
	cp := *in
	return &cp, true
}

func (ix *index) refsFor(inc string) ([]uint64, int) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return append([]uint64(nil), ix.refsByInc[inc]...), ix.refOverflow[inc]
}

func (ix *index) sourceAges(now time.Time) []types.SourceAge {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	out := make([]types.SourceAge, 0, len(ix.sources))
	for _, se := range ix.sources {
		age := se.age
		age.Events24h = se.events.total(now)
		age.Redactions24h = se.redact.total(now)
		age.Dropped24h = se.dropped.total(now)
		age.Gaps24h = int(se.gaps.total(now))
		if age.LastEventTS != "" {
			if t, err := types.ParseUTC(age.LastEventTS); err == nil {
				age.LastEventAgeS = now.Sub(t).Seconds()
			}
		}
		out = append(out, age)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].HostID != out[j].HostID {
			return out[i].HostID < out[j].HostID
		}
		return out[i].Source < out[j].Source
	})
	return out
}

func (ix *index) gapsBetween(since, until string, limit int) []types.GapRecord {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	out := make([]types.GapRecord, 0, len(ix.gaps))
	for i := len(ix.gaps) - 1; i >= 0; i-- {
		g := ix.gaps[i]
		if since != "" && g.ToTS < since {
			continue
		}
		if until != "" && g.ToTS > until {
			continue
		}
		out = append(out, g)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// stats is the O(1) index snapshot (§2.3).
func (ix *index) stats() types.IndexStats {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	st := ix.buildStats
	st.Entries = int(ix.entries)
	st.Groups = len(ix.groups) + len(ix.cold)
	st.GroupsCold = len(ix.cold)
	st.Incidents = len(ix.incidents)
	st.Sources = len(ix.sources)
	st.Days = len(ix.days)
	st.IndexBytes = ix.indexBytes
	if !ix.coldWinStart.IsZero() {
		if el := time.Since(ix.coldWinStart).Minutes(); el > 0 {
			st.ColdEvictionsPerMin = float64(ix.coldEvictions) / el
		}
	}
	return st
}

func (ix *index) markDegraded(reason string) {
	ix.mu.Lock()
	ix.buildStats.Degraded = true
	if ix.buildStats.DegradedReason == "" {
		ix.buildStats.DegradedReason = reason
	}
	ix.mu.Unlock()
}

func minU64(a, b uint64) uint64 {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}
