package ledger

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// groupSort selects the TopGroups ranking (SPEC-01 §2.3: count|rate|trend).
type groupSort int

const (
	sortCount groupSort = iota
	sortRate
	sortTrend
)

// SortByCount, SortByRate and SortByTrend are the exported handles for the
// package-local groupSort type.
var (
	SortByCount = sortCount
	SortByRate  = sortRate
	SortByTrend = sortTrend
)

// Query is the read surface; every answer is O(1)/O(log n) or a bounded cold
// read (SPEC-01 §2.3).
type Query interface {
	TopGroups(n int, window types.Duration, sort groupSort) ([]types.GroupStat, types.QueryInfo, error)
	Incident(id string) (*types.Incident, types.QueryInfo, error)
	IncidentBySig(sig string) (*types.Incident, types.QueryInfo, error)
	GroupBySig(sig string) (*types.Group, types.QueryInfo, error)
	GroupByDigest(hex32 string) (*types.Group, types.QueryInfo, error)
	Evidence(inc string, maxRecords int) (types.EvidenceBundle, error)
	Sources() ([]types.SourceAge, types.QueryInfo, error)
	CounterDeltas(since, until string) (map[string]int64, types.QueryInfo, error)
	Gaps(since string, limit int) ([]types.GapRecord, types.QueryInfo, error)
	RecordsInSeqRange(from, to uint64, limit int) ([]types.Record, error)
	ScanFrom(seq uint64, yield func(types.Record) bool) error
}

type query struct{ l *Ledger }

// TopGroups ranks groups by count, rate or trend. The sieve never materializes
// every group: it walks the index under the read lock and keeps n rows
// (SPEC-01 §2.3: O(G·log n), G = live groups).
func (q *query) TopGroups(n int, window types.Duration, sortBy groupSort) ([]types.GroupStat, types.QueryInfo, error) {
	start := time.Now()
	if n <= 0 {
		n = 10
	}
	threshold := ""
	if window != "" {
		if secs := window.Seconds(); secs > 0 {
			threshold = types.FormatUTC(q.l.now().Add(-time.Duration(secs * float64(time.Second))))
		}
	}
	top := q.l.idx.topGroups(n, threshold, sortBy)
	info := q.info(start)
	info.Indexed = true
	if len(top) == 0 {
		cold, cInfo := q.l.coldTopGroups(n, threshold, sortBy)
		if len(cold) > 0 {
			return cold, cInfo, nil
		}
		if threshold != "" {
			info.Partial = true
			info.Reason = "outside_index_window"
		}
	}
	return top, info, nil
}

func (q *query) Incident(id string) (*types.Incident, types.QueryInfo, error) {
	start := time.Now()
	if in, ok := q.l.idx.incidentByID(id); ok {
		return in, q.info(start), nil
	}
	in, found, info := q.l.coldIncident(id, start)
	if !found {
		return nil, info, ledgerErr(types.CodeLedger002, ReasonValidation,
			fmt.Sprintf("incident %s is not in the index", id), nil)
	}
	return in, info, nil
}

func (q *query) IncidentBySig(sig string) (*types.Incident, types.QueryInfo, error) {
	start := time.Now()
	if in, ok := q.l.idx.incidentForSig(sig); ok {
		return in, q.info(start), nil
	}
	in, found, info := q.l.coldIncidentBySig(sig, start)
	if !found {
		return nil, info, nil
	}
	return in, info, nil
}

func (q *query) GroupBySig(sig string) (*types.Group, types.QueryInfo, error) {
	start := time.Now()
	if d, ok := q.l.idx.digestForSig(sig); ok {
		if g, ok := q.l.idx.groupByDigest(d); ok {
			return groupStatToGroup(g), q.info(start), nil
		}
	}
	g, found, info := q.l.coldGroupBySig(sig, start)
	if !found {
		return nil, info, nil
	}
	return g, info, nil
}

func (q *query) GroupByDigest(hex32 string) (*types.Group, types.QueryInfo, error) {
	start := time.Now()
	if g, ok := q.l.idx.groupByDigest(hex32); ok {
		return groupStatToGroup(g), q.info(start), nil
	}
	g, found, info := q.l.coldGroupByDigest(hex32, start)
	if !found {
		return nil, info, nil
	}
	return g, info, nil
}

// Evidence assembles one incident's ordered audit chain plus its gaps.
func (q *query) Evidence(inc string, maxRecords int) (types.EvidenceBundle, error) {
	start := time.Now()
	if maxRecords <= 0 {
		maxRecords = q.l.opts.Index.IncPerIncident
	}
	bundle := types.EvidenceBundle{Inc: inc}
	refs, overflow := q.l.idx.refsFor(inc)
	bundle.RefCount = len(refs) + overflow
	bundle.Overflow = overflow
	kindCounts := map[string]int{}
	if in, ok := q.l.idx.incidentByID(inc); ok {
		bundle.Sig = in.Sig
		bundle.GroupID = in.GroupID
	}
	if len(refs) == 0 {
		// cold path: no indexed refs (degraded index or compacted day)
		recs, info, err := q.l.coldEvidence(inc, maxRecords, start)
		if err != nil {
			return bundle, err
		}
		bundle.Records = recs
		bundle.Partial = true
		bundle.Info = info
		bundle.Gaps = q.l.idx.gapsBetween("", "", 0)
		return bundle, nil
	}
	out := make([]types.Record, 0, len(refs))
	for _, seq := range refs {
		if len(out) >= maxRecords {
			bundle.Partial = true
			break
		}
		rec, ok, err := q.l.recordAt(seq)
		if err != nil {
			return bundle, err
		}
		if !ok {
			continue
		}
		if rec.Inc == inc || inc == "" {
			out = append(out, rec)
			kindCounts[string(rec.Kind)]++
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	bundle.Records = out
	bundle.KindCounts = kindCounts
	if len(out) > 0 {
		bundle.FirstSeq = out[0].Seq
		bundle.LastSeq = out[len(out)-1].Seq
	}
	bundle.Gaps = q.l.idx.gapsBetween("", "", 0)
	bundle.Info = q.info(start)
	return bundle, nil
}

func (q *query) Sources() ([]types.SourceAge, types.QueryInfo, error) {
	start := time.Now()
	out := q.l.idx.sourceAges(q.l.now())
	return out, q.info(start), nil
}

// CounterDeltas returns per-source counter deltas for a window, read from the
// day index (bounded) rather than a full scan (SPEC-01 §4.4).
func (q *query) CounterDeltas(since, until string) (map[string]int64, types.QueryInfo, error) {
	start := time.Now()
	out := map[string]int64{}
	scanned, lines, partial, err := q.l.scanWindow(since, until, func(rec types.Record) bool {
		key := rec.Origin.HostID + ":" + rec.Origin.Source
		out[key]++
		return true
	})
	if err != nil {
		return nil, q.info(start), err
	}
	info := q.info(start)
	info.ScannedBytes = scanned
	info.ScannedLines = lines
	info.Partial = partial

	return out, info, nil
}

func (q *query) Gaps(since string, limit int) ([]types.GapRecord, types.QueryInfo, error) {
	start := time.Now()
	g := q.l.idx.gapsBetween(since, "", limit)
	return g, q.info(start), nil
}

func (q *query) RecordsInSeqRange(from, to uint64, limit int) ([]types.Record, error) {
	return q.l.RecordsInSeqRange(from, to, limit)
}

func (q *query) ScanFrom(seq uint64, yield func(types.Record) bool) error {
	return q.l.ScanFrom(seq, yield)
}

func (q *query) info(start time.Time) types.QueryInfo {
	st := q.l.idx.stats()
	return types.QueryInfo{
		Indexed:           true,
		Degraded:          st.Degraded,
		TruncatedBeforeTS: st.TruncatedBeforeTS,
		ElapsedMS:         int(time.Since(start).Milliseconds()),
	}
}

func groupStatToGroup(g types.GroupStat) *types.Group {
	return &types.Group{
		ID:          g.GroupID,
		Sig:         g.Sig,
		Digest:      g.Digest,
		Source:      types.SigSource(g.Source),
		Title:       g.Title,
		FirstSeenTS: g.FirstSeenTS,
		LastSeenTS:  g.LastSeenTS,
		Count:       g.Count,
		Counters:    g.Counters,
		IncidentID:  g.IncidentID,
	}
}

// ---- the cold-read path (SPEC-01 §3.6) ----

// coldTopGroups re-derives top groups from the files when the index window
// excludes them, inside the cold_read caps.
func (l *Ledger) coldTopGroups(n int, threshold string, sortBy groupSort) ([]types.GroupStat, types.QueryInfo) {
	start := time.Now()
	acc := map[string]*types.GroupStat{}
	scanned, lines, _, _ := l.scanAll(func(rec types.Record) bool {
		if rec.Kind != types.KEvent && rec.Kind != types.KCanary {
			return true
		}
		if threshold != "" && rec.TS < threshold {
			return true
		}
		digest, mk, title := groupKeyOf(&rec)
		if digest == "" {
			return true
		}
		g := acc[digest]
		if g == nil {
			g = &types.GroupStat{Digest: digest, MergeKey: mk, Title: title, FirstSeenTS: rec.TS}
			acc[digest] = g
		}
		g.Sig = rec.Sig
		g.Count++
		g.Counters.Events++
		g.Counters.Redacted += uint64(rec.Redactions)
		g.LastSeenTS = rec.TS
		return true
	})
	rows := make([]types.GroupStat, 0, len(acc))
	for _, g := range acc {
		rows = append(rows, *g)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Count > rows[j].Count })
	if len(rows) > n {
		rows = rows[:n]
	}
	info := types.QueryInfo{
		Indexed: false, Partial: true, Reason: "outside_index_window",
		Degraded:     l.idx.stats().Degraded,
		ScannedBytes: scanned, ScannedLines: lines,
		ElapsedMS: int(time.Since(start).Milliseconds()),
	}
	return rows, info
}

func (l *Ledger) coldGroupBySig(sig string, start time.Time) (*types.Group, bool, types.QueryInfo) {
	g, found, info := l.coldGroupBy(func(rec types.Record) bool { return rec.Sig == sig }, start)
	return g, found, info
}

func (l *Ledger) coldGroupByDigest(digest string, start time.Time) (*types.Group, bool, types.QueryInfo) {
	g, found, info := l.coldGroupBy(func(rec types.Record) bool {
		d, _, _ := groupKeyOf(&rec)
		return d == digest
	}, start)
	return g, found, info
}

func (l *Ledger) coldGroupBy(match func(types.Record) bool, start time.Time) (*types.Group, bool, types.QueryInfo) {
	var out *types.Group
	scanned, lines, _, _ := l.scanAll(func(rec types.Record) bool {
		if rec.Kind != types.KEvent && rec.Kind != types.KCanary && rec.Kind != types.KGroup {
			return true
		}
		if !match(rec) {
			return true
		}
		digest, _, title := groupKeyOf(&rec)
		if out == nil {
			out = &types.Group{ID: types.NewID(types.PGrp), Digest: digest, Title: title, FirstSeenTS: rec.TS}
		}
		out.Sig = rec.Sig
		out.Count++
		out.Counters.Events++
		out.LastSeenTS = rec.TS
		return true
	})
	info := types.QueryInfo{
		Indexed: false, Partial: true, Reason: "outside_index_window",
		Degraded:     l.idx.stats().Degraded,
		ScannedBytes: scanned, ScannedLines: lines,
		ElapsedMS: int(time.Since(start).Milliseconds()),
	}
	if out == nil {
		return nil, false, info
	}
	return out, true, info
}

func (l *Ledger) coldIncident(id string, start time.Time) (*types.Incident, bool, types.QueryInfo) {
	return l.coldIncidentBy(func(rec types.Record) bool { return rec.Inc == id }, start)
}

func (l *Ledger) coldIncidentBySig(sig string, start time.Time) (*types.Incident, bool, types.QueryInfo) {
	return l.coldIncidentBy(func(rec types.Record) bool { return rec.Sig == sig }, start)
}

func (l *Ledger) coldIncidentBy(match func(types.Record) bool, start time.Time) (*types.Incident, bool, types.QueryInfo) {
	var out *types.Incident
	scanned, lines, _, _ := l.scanAll(func(rec types.Record) bool {
		if rec.Kind != types.KIncident || !match(rec) {
			return true
		}
		tmp := &index{maxSchema: l.opts.MaxSchema,
			incidents: map[string]*types.Incident{}, openBySig: map[string]string{},
			groups: map[string]*groupEntry{}, cold: map[string]*groupEntry{},
			sigToDigest: map[string]string{}, mergeToDigest: map[string]string{},
			refsByInc: map[string][]uint64{}, refOverflow: map[string]int{},
			sources: map[string]*sourceEntry{}, days: map[string]*dayEntry{},
		}
		tmp.opts = l.opts.Index
		tmp.applyLocked(&rec)
		for _, in := range tmp.incidents {
			if out == nil || in.UpdatedTS >= out.UpdatedTS {
				cp := *in
				out = &cp
			}
		}
		return true
	})
	info := types.QueryInfo{
		Indexed: false, Partial: true, Reason: "outside_index_window",
		Degraded:     l.idx.stats().Degraded,
		ScannedBytes: scanned, ScannedLines: lines,
		ElapsedMS: int(time.Since(start).Milliseconds()),
	}
	if out == nil {
		return nil, false, info
	}
	return out, true, info
}

func (l *Ledger) coldEvidence(inc string, maxRecords int, start time.Time) ([]types.Record, types.QueryInfo, error) {
	var out []types.Record
	scanned, lines, _, err := l.scanAll(func(rec types.Record) bool {
		if rec.Inc != inc && inc != "" {
			return true
		}
		out = append(out, rec)
		return !(maxRecords > 0 && len(out) >= maxRecords)
	})
	info := types.QueryInfo{
		Indexed: false, Partial: true, Reason: "outside_index_window",
		Degraded:     l.idx.stats().Degraded,
		ScannedBytes: scanned, ScannedLines: lines,
		ElapsedMS: int(time.Since(start).Milliseconds()),
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, info, err
}

// recordAt resolves one seq through the index and reads it back.
func (l *Ledger) recordAt(seq uint64) (types.Record, bool, error) {
	name, off, err := l.Resolve(seq)
	if err != nil {
		return types.Record{}, false, nil
	}
	f, oerr := os.Open(filepath.Join(l.root, name))
	if oerr != nil {
		return types.Record{}, false, ledgerErr(types.CodeLedger012, ReasonIO,
			fmt.Sprintf("cannot open %s", name), oerr)
	}
	defer f.Close()
	if _, err := f.Seek(off, 0); err != nil {
		return types.Record{}, false, ledgerErr(types.CodeLedger012, ReasonIO, "seek failed", err)
	}
	rd := newLineReader(f)
	for {
		line, _, complete, rerr := rd.next()
		if rerr != nil {
			return types.Record{}, false, ledgerErr(types.CodeLedger012, ReasonIO, "read failed", rerr)
		}
		if !complete && len(line) == 0 {
			return types.Record{}, false, nil
		}
		if !complete {
			return types.Record{}, false, nil
		}
		rec, derr := decodeRecord(line, true)
		if derr == nil && rec != nil && rec.Seq == seq {
			return *rec, true, nil
		}
		if derr == nil && rec != nil && rec.Seq > seq {
			return types.Record{}, false, nil
		}
	}
}

// scanAll walks the whole authoritative file set inside the cold_read caps.
func (l *Ledger) scanAll(yield func(types.Record) bool) (scannedBytes, scannedLines int64, partial bool, err error) {
	parts, _, derr := l.authoritativeFiles()
	if derr != nil {
		return 0, 0, false, derr
	}
	for _, p := range parts {
		f, oerr := os.Open(filepath.Join(l.root, p.File))
		if oerr != nil {
			return scannedBytes, scannedLines, true,
				ledgerErr(types.CodeLedger012, ReasonIO, fmt.Sprintf("cannot open %s", p.File), oerr)
		}
		rd := newLineReader(f)
		for {
			line, _, complete, rerr := rd.next()
			if rerr != nil {
				f.Close()
				return scannedBytes, scannedLines, true,
					ledgerErr(types.CodeLedger012, ReasonIO, "read failed", rerr)
			}
			if !complete && len(line) == 0 {
				break
			}
			scannedBytes += int64(len(line)) + 1
			if scannedBytes > l.opts.Index.ColdReadMaxBytes || scannedLines > l.opts.Index.ColdReadMaxLines {
				f.Close()
				return scannedBytes, scannedLines, true, nil
			}
			if !complete {
				break
			}
			if len(strings.TrimSpace(string(line))) == 0 {
				continue
			}
			rec, cerr := decodeRecord(line, true)
			if cerr != nil || rec == nil {
				continue
			}
			scannedLines++
			if !yield(*rec) {
				f.Close()
				return scannedBytes, scannedLines, false, nil
			}
		}
		f.Close()
	}
	return scannedBytes, scannedLines, false, nil
}

// scanWindow walks only the days overlapping [since, until].
func (l *Ledger) scanWindow(since, until string, yield func(types.Record) bool) (scannedBytes, scannedLines int64, partial bool, err error) {
	sinceDay, untilDay := "", ""
	if len(since) >= 10 {
		sinceDay = since[:10]
	}
	if len(until) >= 10 {
		untilDay = until[:10]
	}
	for _, de := range l.idx.dayEntries() {
		if sinceDay != "" && de.Day < sinceDay {
			continue
		}
		if untilDay != "" && de.Day > untilDay {
			continue
		}
		for _, p := range de.Parts {
			f, oerr := os.Open(filepath.Join(l.root, p.File))
			if oerr != nil {
				return scannedBytes, scannedLines, true,
					ledgerErr(types.CodeLedger012, ReasonIO, fmt.Sprintf("cannot open %s", p.File), oerr)
			}
			rd := newLineReader(f)
			for {
				line, _, complete, rerr := rd.next()
				if rerr != nil {
					f.Close()
					return scannedBytes, scannedLines, true,
						ledgerErr(types.CodeLedger012, ReasonIO, "read failed", rerr)
				}
				if !complete && len(line) == 0 {
					break
				}
				scannedBytes += int64(len(line)) + 1
				if scannedBytes > l.opts.Index.ColdReadMaxBytes || scannedLines > l.opts.Index.ColdReadMaxLines {
					f.Close()
					return scannedBytes, scannedLines, true, nil
				}
				if !complete {
					break
				}
				rec, cerr := decodeRecord(line, true)
				if cerr != nil || rec == nil {
					continue
				}
				if since != "" && rec.TS < since {
					continue
				}
				if until != "" && rec.TS > until {
					continue
				}
				scannedLines++
				if !yield(*rec) {
					f.Close()
					return scannedBytes, scannedLines, false, nil
				}
			}
			f.Close()
		}
	}
	return scannedBytes, scannedLines, false, nil
}
