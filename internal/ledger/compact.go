package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// Compact writes a new generation file for day and unlinks the parts and the
// previous generation only after the new one is fsynced (SPEC-01 §3.7).
// Compaction is never in place: D.N.gen.jsonl is authoritative for the day.
func (l *Ledger) Compact(ctx context.Context, day string, rp RetentionPolicy) (types.CompactionResult, error) {
	return l.compact(ctx, day, rp, false)
}

// CompactDryRun computes the CompactionResult and writes nothing (§2.4).
func (l *Ledger) CompactDryRun(ctx context.Context, day string, rp RetentionPolicy) (types.CompactionResult, error) {
	return l.compact(ctx, day, rp, true)
}

type compactLine struct {
	raw  []byte
	rec  *types.Record
	fold bool // event/canary line older than payload_ttl → folded into an aggregate
}

func (l *Ledger) compact(ctx context.Context, day string, rp RetentionPolicy, dryRun bool) (types.CompactionResult, error) {
	start := time.Now()
	res := types.CompactionResult{Day: day, DryRun: dryRun, TombstoneCountsKept: true}
	if rp.PayloadTTL == "" && rp.DiskBudgetBytes == 0 {
		rp = l.opts.Retention
	}
	de := l.idx.day(day)
	if de == nil || len(de.Parts) == 0 {
		res.ErrorCode = string(types.CodeLedger007)
		return res, ledgerErr(types.CodeLedger007, ReasonCompaction,
			fmt.Sprintf("no files for day %s", day), nil)
	}
	if cur := l.w.currentFile(); cur.day == day {
		res.ErrorCode = string(types.CodeLedger007)
		return res, ledgerErr(types.CodeLedger007, ReasonCompaction,
			"refusing to compact the file the writer is appending to", nil)
	}
	for _, p := range de.Parts {
		res.FromFiles = append(res.FromFiles, p.File)
	}

	// read the day in seq order
	var lines []compactLine
	for _, p := range de.Parts {
		b, err := os.ReadFile(filepath.Join(l.root, p.File))
		if err != nil {
			res.ErrorCode = string(types.CodeLedger007)
			return res, ledgerErr(types.CodeLedger007, ReasonCompaction,
				fmt.Sprintf("cannot read %s", p.File), err)
		}
		res.BytesIn += int64(len(b))
		for _, raw := range bytes.Split(b, []byte("\n")) {
			if len(bytes.TrimSpace(raw)) == 0 {
				continue
			}
			rec, derr := decodeRecord(raw, true)
			if derr != nil || rec == nil {
				continue
			}
			cp := make([]byte, len(raw))
			copy(cp, raw)
			lines = append(lines, compactLine{raw: cp, rec: rec})
		}
	}
	res.LinesIn = int64(len(lines))

	ttl := rp.PayloadTTL.Std()
	cutoff := ""
	if ttl > 0 {
		cutoff = types.FormatUTC(l.now().Add(-ttl))
	}
	diskPressure := rp.DiskBudgetBytes > 0 && l.diskBytes() > rp.DiskBudgetBytes

	// Rule 1 vs rule 3, resolved once here: an event/canary line is folded into
	// its group's aggregate — its payload replaced by counters, identity, seq,
	// ts and redactions preserved — when the day itself is past retention_raw
	// (the normal retention compaction), when the line is past payload_ttl, or
	// under disk pressure. A day that is younger than both keeps its event
	// payloads verbatim, and compaction refuses with 009 when that leaves it
	// nothing legal to reclaim (counts are never sacrificed to make space).
	rawKeep := rp.RawKeep.Std()
	dayTS := de.LastTS
	if dayTS == "" && len(lines) > 0 {
		dayTS = lines[0].rec.TS
	}
	foldDay := false
	if rawKeep <= 0 {
		foldDay = true
	} else if t, terr := types.ParseUTC(dayTS); terr == nil {
		foldDay = l.now().Sub(t) > rawKeep
	}
	for i := range lines {
		rec := lines[i].rec
		if rec.Kind != types.KEvent && rec.Kind != types.KCanary {
			continue
		}
		switch {
		case foldDay, diskPressure:
			lines[i].fold = true
		case cutoff != "" && rec.TS < cutoff:
			lines[i].fold = true
		}
	}

	type agg struct {
		digest   string
		mergeKey string
		title    string
		sig      string
		groupID  string
		incID    string
		count    uint64
		redacted uint64
		firstTS  string
		lastTS   string
		firstSeq uint64
		lastSeq  uint64
		emitAt   int
		src      *types.Record
	}
	aggs := map[string]*agg{}
	emit := map[int]*agg{}
	eventsBefore := uint64(0)
	expirable := 0

	for i := range lines {
		rec := lines[i].rec
		if rec.Kind == types.KEvent || rec.Kind == types.KCanary {
			eventsBefore++
			if !lines[i].fold {
				continue
			}
			expirable++
			digest, mk, title := groupKeyOf(rec)
			if digest == "" {
				digest = fmt.Sprintf("seq:%d", rec.Seq)
			}
			a := aggs[digest]
			if a == nil {
				a = &agg{digest: digest}
				aggs[digest] = a
			}
			a.mergeKey, a.title, a.sig = mk, title, rec.Sig
			if gid, ok := rec.Payload["group_id"].(string); ok {
				a.groupID = gid
			}
			if iid, ok := rec.Payload["incident_id"].(string); ok {
				a.incID = iid
			}
			if a.count == 0 {
				a.firstTS, a.firstSeq = rec.TS, rec.Seq
			}
			a.count++
			a.redacted += uint64(rec.Redactions)
			a.lastTS, a.lastSeq = rec.TS, rec.Seq
			a.src = rec // the newest folded line carries the aggregate's identity
			a.emitAt = i
			continue
		}
		if rec.Kind == types.KGroup && isCompactionAggregate(rec) {
			digest, mk, title := groupKeyOf(rec)
			a := aggs[digest]
			if a == nil {
				a = &agg{digest: digest}
				aggs[digest] = a
			}
			a.mergeKey, a.title, a.sig = mk, title, rec.Sig
			c := decodeCounters(mapOf(rec.Payload["counters"]))
			if a.count == 0 {
				a.firstTS, a.firstSeq = rec.TS, rec.Seq
			}
			eventsBefore += c.Events
			a.count += c.Events
			a.redacted += uint64(rec.Redactions)
			a.lastTS, a.lastSeq = rec.TS, rec.Seq
			a.src = rec
			a.emitAt = i
		}
	}
	// Exactly one aggregate line per (group, day), at the position of the last
	// folded line for that group (SPEC-01 §3.7 rule 1).
	for _, a := range aggs {
		emit[a.emitAt] = a
	}

	// 009: compaction cannot free space without violating tombstone counts.
	if len(aggs) == 0 && expirable == 0 {
		res.ErrorCode = string(types.CodeLedger009)
		return res, ledgerErr(types.CodeLedger009, ReasonRetention,
			"the only compaction candidates are spine records whose counts must be kept", nil)
	}

	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	eventsAfter := uint64(0)
	for i := range lines {
		rec := lines[i].rec
		if a, ok := emit[i]; ok {
			record := *a.src
			record.Kind = types.KGroup
			record.Redactions = int(a.redacted)
			record.Payload = map[string]any{
				"op":                    "compaction_aggregate",
				"aggregate":             true,
				"payload_ttl_expired":   true,
				"day":                   day,
				"digest":                a.digest,
				"merge_key":             a.mergeKey,
				"subject":               a.title,
				"title":                 a.title,
				"group_id":              a.groupID,
				"incident_id":           a.incID,
				"events_aggregated":     a.count,
				"first_ts":              a.firstTS,
				"last_ts":               a.lastTS,
				"first_seq":             a.firstSeq,
				"last_seq":              a.lastSeq,
				"agg_seqs":              map[string]any{"first": a.firstSeq, "last": a.lastSeq, "count": a.count, "exceptions": []any{}},
				"tombstone_counts_kept": true,
				"counters": map[string]any{
					"events":          a.count,
					"suppressed":      0,
					"redacted_values": a.redacted,
					"dropped_events":  0,
					"sample_rate":     1,
				},
			}
			eventsAfter += a.count
			if err := enc.Encode(&record); err != nil {
				res.ErrorCode = string(types.CodeLedger007)
				return res, ledgerErr(types.CodeLedger007, ReasonCompaction, "cannot serialize aggregate", err)
			}
			res.Aggregates++
			continue
		}
		switch rec.Kind {
		case types.KEvent, types.KCanary:
			if lines[i].fold {
				continue // superseded by its aggregate
			}
			// younger than payload_ttl: kept verbatim, payload intact
			if _, err := out.Write(lines[i].raw); err != nil {
				res.ErrorCode = string(types.CodeLedger007)
				return res, ledgerErr(types.CodeLedger007, ReasonCompaction, "buffer write failed", err)
			}
			out.WriteByte('\n')
			res.RecordsKept++
			eventsAfter++
			continue
		case types.KGroup:
			if isCompactionAggregate(rec) {
				continue // already folded into the aggregate emitted at its position
			}
		}
		// audit-spine records are copied verbatim and never expire
		if _, err := out.Write(lines[i].raw); err != nil {
			res.ErrorCode = string(types.CodeLedger007)
			return res, ledgerErr(types.CodeLedger007, ReasonCompaction, "buffer write failed", err)
		}
		out.WriteByte('\n')
		res.RecordsKept++
	}

	if eventsBefore != eventsAfter {
		res.ErrorCode = string(types.CodeLedger007)
		return res, ledgerErr(types.CodeLedger007, ReasonCompaction,
			fmt.Sprintf("tombstone counts would change: %d events before, %d after", eventsBefore, eventsAfter), nil)
	}

	res.PayloadsExpired = int64(expirable)
	if diskPressure {
		res.PayloadsDropped = int64(expirable)
	}

	toGen := de.Gen + 1
	to := partName(day, toGen, 1)
	res.ToFile = to
	res.FromGen = de.Gen
	res.ToGen = toGen
	res.LinesOut = res.RecordsKept + res.Aggregates
	res.BytesOut = int64(out.Len())
	res.ElapsedMS = int(time.Since(start).Milliseconds())

	if dryRun {
		return res, nil
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}

	if err := l.w.compactionStart(day, de.Gen, toGen); err != nil {
		res.ErrorCode = string(types.CodeLedger007)
		return res, err
	}
	tmp := filepath.Join(l.root, "."+to+".tmp")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		res.ErrorCode = string(types.CodeLedger007)
		return res, ledgerErr(types.CodeLedger007, ReasonCompaction, "cannot create the generation tmp file", err)
	}
	if _, err := f.Write(out.Bytes()); err != nil {
		f.Close()
		os.Remove(tmp)
		res.ErrorCode = string(types.CodeLedger007)
		return res, ledgerErr(types.CodeLedger007, ReasonCompaction, "cannot write the generation file", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		res.ErrorCode = string(types.CodeLedger007)
		return res, ledgerErr(types.CodeLedger007, ReasonCompaction, "cannot fsync the generation file", err)
	}
	if err := f.Close(); err != nil {
		res.ErrorCode = string(types.CodeLedger007)
		return res, ledgerErr(types.CodeLedger007, ReasonCompaction, "cannot close the generation file", err)
	}
	if err := os.Rename(tmp, filepath.Join(l.root, to)); err != nil {
		os.Remove(tmp)
		res.ErrorCode = string(types.CodeLedger007)
		return res, ledgerErr(types.CodeLedger007, ReasonCompaction, "cannot rename the generation file", err)
	}
	if err := syncDir(l.root); err != nil {
		res.ErrorCode = string(types.CodeLedger007)
		return res, ledgerErr(types.CodeLedger007, ReasonCompaction, "cannot fsync the ledger directory", err)
	}
	// only now may the previous generation and the parts be unlinked
	for _, p := range de.Parts {
		if err := os.Remove(filepath.Join(l.root, p.File)); err != nil && !os.IsNotExist(err) {
			l.idx.markDegraded(ReasonIO)
		}
	}
	_ = syncDir(l.root)
	l.idx.replaceDay(day, []*partInfo{{
		Day: day, Gen: toGen, Part: 1, File: to, Bytes: int64(out.Len()), Records: res.LinesOut,
		FirstSeq: de.FirstSeq, LastSeq: de.LastSeq, FirstTS: de.FirstTS, LastTS: de.LastTS,
	}}, toGen)
	l.compactionRuns.Add(1)
	res.TombstoneCountsKept = true
	res.ElapsedMS = int(time.Since(start).Milliseconds())
	if err := l.w.compactionDone(day, toGen, res); err != nil {
		return res, err
	}
	return res, nil
}

func mapOf(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// ApplyRetention compacts every day older than compaction_min_age that is at
// least compaction_min_bytes (SPEC-01 §3.7 rule 6).
func (l *Ledger) ApplyRetention(ctx context.Context) ([]types.CompactionResult, error) {
	rp := l.opts.Retention
	age := rp.CompactionMinAge.Std()
	if age <= 0 {
		age = 24 * time.Hour
	}
	cut := l.now().Add(-age).UTC().Format(dayLayout)
	var out []types.CompactionResult
	for _, de := range l.idx.dayEntries() {
		if de.Day >= cut {
			continue
		}
		if rp.CompactionMinBytes > 0 && de.Bytes < rp.CompactionMinBytes {
			continue
		}
		res, err := l.Compact(ctx, de.Day, rp)
		if err != nil {
			if CodeOf(err) == types.CodeLedger009 {
				continue
			}
			return out, err
		}
		out = append(out, res)
	}
	return out, nil
}

// ExpireDays unlinks generation files older than retention_compacted.
func (l *Ledger) ExpireDays() ([]string, error) {
	rp := l.opts.Retention
	keep := rp.CompactedKeep.Std()
	if keep <= 0 {
		keep = 8760 * time.Hour
	}
	compactCut := l.now().Add(-keep).UTC().Format(dayLayout)
	var removed []string
	for _, de := range l.idx.dayEntries() {
		if de.Day >= compactCut {
			continue
		}
		if de.Gen == 0 {
			// a raw day past retention_raw must be compacted first, never
			// unlinked: an incident's Evidence never loses its spine (§3.7.6)
			continue
		}
		for _, p := range de.Parts {
			if err := os.Remove(filepath.Join(l.root, p.File)); err == nil || os.IsNotExist(err) {
				removed = append(removed, p.File)
			}
		}
		l.idx.removeDay(de.Day)
	}
	return removed, nil
}
