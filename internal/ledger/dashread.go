package ledger

import (
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// dashread.go is the SPEC-10 §3.3 read surface: the exact accessors the dashboard
// requires, served from the SPEC-01 §3.6 in-memory index. No accessor here opens,
// mmaps or scans a ledger file (SPEC-10 §2.9), which is why the index keeps a
// bounded ring of the most recent records: a timeline render is a ring lookup,
// never a re-read.
//
// The ring is bounded (dashRingCap) and holds a *projection* per record — seq,
// ts, kind, sig, inc, actor id, error code and a ≤200-rune summary — never the
// payload. Its steady cost is ~2 MB at the cap, inside the daemon's ≤80 MB RSS
// budget and separate from the dashboard's own ≤6 MB slice.

// dashRingCap is the number of most recent records the dashboard can render.
const dashRingCap = 8192

// dashRec is one projected record.
type dashRec struct {
	Seq      uint64
	TS       string
	Kind     types.RecordKind
	Sig      string
	Inc      string
	ActorID  string
	ErrCode  string
	Summary  string
	Rendered time.Time
}

// dashRing is the bounded recent-record ring plus the counters the dashboard and
// the health aggregator read. It is a fixed-size circular buffer: appending is
// O(1) at the cap (no slice shifting), because a boot rebuild projects every
// record in the ledger and must not turn that into an O(records × cap) walk.
type dashRing struct {
	mu   sync.RWMutex
	buf  []dashRec
	n    int // records currently held (≤ len(buf))
	head int // index of the oldest record once the ring is full
}

// newDashRing returns an empty ring.
func newDashRing() *dashRing { return &dashRing{buf: make([]dashRec, dashRingCap)} }

// add projects one record into the ring.
func (r *dashRing) add(rec *types.Record, ts time.Time) {
	d := dashRec{
		Seq:      rec.Seq,
		TS:       rec.TS,
		Kind:     rec.Kind,
		Sig:      rec.Sig,
		Inc:      rec.Inc,
		ActorID:  rec.Actor.ID,
		ErrCode:  payloadString(rec.Payload, "error_code"),
		Summary:  summarize(rec),
		Rendered: ts,
	}
	r.mu.Lock()
	if r.n < len(r.buf) {
		r.buf[(r.head+r.n)%len(r.buf)] = d
		r.n++
	} else {
		r.buf[r.head] = d
		r.head = (r.head + 1) % len(r.buf)
	}
	r.mu.Unlock()
}

// at returns the i-th record counting from the oldest (caller holds the lock).
func (r *dashRing) at(i int) *dashRec { return &r.buf[(r.head+i)%len(r.buf)] }

// since returns up to limit records with Seq > since, oldest first.
func (r *dashRing) since(since uint64, limit int) []dashRec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if limit <= 0 {
		limit = 100
	}
	out := make([]dashRec, 0, 16)
	for i := r.n - 1; i >= 0 && len(out) < limit; i-- {
		if rec := r.at(i); rec.Seq > since {
			out = append(out, *rec)
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// forIncident returns up to limit records carrying inc, oldest first, bounded by
// the ring (a record older than the ring is not rendered; the page is a window,
// not a full history).
func (r *dashRing) forIncident(inc string, since uint64, limit int) []dashRec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if limit <= 0 {
		limit = 100
	}
	out := make([]dashRec, 0, 16)
	for i := r.n - 1; i >= 0 && len(out) < limit; i-- {
		if rec := r.at(i); rec.Inc == inc && rec.Seq > since {
			out = append(out, *rec)
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// lastEventAge returns how long ago the newest event/canary record for sig was
// projected, and whether one exists in the ring.
func (r *dashRing) lastEventAge(sig string, now time.Time) (float64, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for i := r.n - 1; i >= 0; i-- {
		rec := r.at(i)
		if rec.Sig != sig {
			continue
		}
		if rec.Kind != types.KEvent && rec.Kind != types.KCanary {
			continue
		}
		return now.Sub(rec.Rendered).Seconds(), true
	}
	return 0, false
}

// eventsPerMin counts event/canary records seen in the last 60 seconds.
func (r *dashRing) eventsPerMin(now time.Time) float64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cut := now.Add(-time.Minute)
	var n float64
	for i := r.n - 1; i >= 0; i-- {
		rec := r.at(i)
		if rec.Rendered.Before(cut) {
			break
		}
		if rec.Kind == types.KEvent || rec.Kind == types.KCanary {
			n++
		}
	}
	return n
}

// countBetween counts event/canary records for sig with TS inside [fromTS, toTS]
// (an empty bound is unbounded). TS comparisons are string comparisons on the
// canonical RFC3339 UTC form, which is monotonic in this layout.
func (r *dashRing) countBetween(sig, fromTS, toTS string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for i := 0; i < r.n; i++ {
		rec := r.at(i)
		if rec.Sig != sig {
			continue
		}
		if rec.Kind != types.KEvent && rec.Kind != types.KCanary {
			continue
		}
		if fromTS != "" && rec.TS < fromTS {
			continue
		}
		if toTS != "" && rec.TS > toTS {
			continue
		}
		n++
	}
	return n
}

// lastTS returns the TS of the newest projected record, "" when the ring is empty.
func (r *dashRing) lastTS() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.n == 0 {
		return ""
	}
	return r.at(r.n - 1).TS
}

// payloadString reads a string payload key.
func payloadString(p map[string]any, key string) string {
	if p == nil {
		return ""
	}
	if v, ok := p[key].(string); ok {
		return v
	}
	return ""
}

// summarize renders the one-line summary a timeline <li> shows
// (SPEC-10 §2.1.2): kind-specific, ≤200 runes, never the payload.
func summarize(rec *types.Record) string {
	p := rec.Payload
	parts := make([]string, 0, 3)
	if tr := payloadString(p, "transition"); tr != "" {
		parts = append(parts, tr)
	}
	if st := payloadString(p, "stage"); st != "" {
		parts = append(parts, st)
	}
	for _, k := range []string{"rule", "reason", "resolution", "detail"} {
		if v := payloadString(p, k); v != "" {
			parts = append(parts, v)
			break
		}
	}
	if v := payloadString(p, "tool"); v != "" {
		parts = append(parts, v)
	}
	if v := payloadString(p, "module"); v != "" {
		parts = append(parts, v)
	}
	if len(parts) == 0 {
		if t := payloadString(p, "title"); t != "" {
			parts = append(parts, t)
		}
	}
	s := strings.Join(parts, " · ")
	return truncateSummary(s, 200)
}

func truncateSummary(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

// DashReader is the SPEC-10 §3.3 accessor list, implemented by the ledger's
// in-memory index. Every method is O(rows returned) and touches no file.
type DashReader interface {
	// LastSeq is the newest sequence number in the index (the stall test's
	// primary input, SPEC-12 §3.3).
	LastSeq() uint64
	// LastRecordTS is the wall-clock TS of the newest record, "" when unknown.
	LastRecordTS() string
	// Counters returns the open-incident count, the live group count and the
	// current events-per-minute rate.
	Counters(now time.Time) (incidentsOpen, groupsOpen int, eventsPerMin float64)
	// OpenIncidents lists open incidents, newest-updated first.
	OpenIncidents(limit int) []types.Incident
	// OpenIncidentsSince lists open incidents with a record newer than seq.
	OpenIncidentsSince(seq uint64, limit int) []types.Incident
	// GroupsSince lists groups whose newest record is newer than seq.
	GroupsSince(seq uint64, limit int) []types.GroupStat
	// RecordsForIncident renders an incident's timeline window.
	RecordsForIncident(inc string, since uint64, limit int) []types.Record
	// LastEventAge is the age in seconds of the newest event for sig.
	LastEventAge(sig string, now time.Time) (float64, bool)
	// EventsBetween counts the event/canary records projected for sig whose TS
	// falls inside [fromTS, toTS] (empty bound = unbounded). It is the ladder's
	// verification input, served from the same bounded ring.
	EventsBetween(sig, fromTS, toTS string) int
}

// DashReader returns the dashboard read surface (SPEC-10 §3.3).
func (l *Ledger) DashReader() DashReader { return &dashReader{l: l} }

type dashReader struct{ l *Ledger }

func (d *dashReader) LastSeq() uint64 { return d.l.idx.lastSeq.Load() }

func (d *dashReader) LastRecordTS() string {
	if ts := d.l.idx.dash.lastTS(); ts != "" {
		return ts
	}
	return types.FormatUTC(d.l.now())
}

func (d *dashReader) Counters(now time.Time) (int, int, float64) {
	ix := d.l.idx
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	open := 0
	for _, id := range ix.incRing {
		if inc, ok := ix.incidents[id]; ok && isOpenState(inc.State) {
			open++
		}
	}
	return open, len(ix.groups), ix.dash.eventsPerMin(now)
}

func (d *dashReader) OpenIncidents(limit int) []types.Incident {
	return d.openIncidents(0, limit)
}

func (d *dashReader) OpenIncidentsSince(seq uint64, limit int) []types.Incident {
	return d.openIncidents(seq, limit)
}

func (d *dashReader) openIncidents(since uint64, limit int) []types.Incident {
	if limit <= 0 {
		limit = 100
	}
	ix := d.l.idx
	ix.mu.RLock()
	out := make([]types.Incident, 0, 16)
	for _, id := range ix.incRing {
		inc, ok := ix.incidents[id]
		if !ok || !isOpenState(inc.State) {
			continue
		}
		if since > 0 && lastRefSeq(ix.refsByInc[id]) <= since {
			continue
		}
		out = append(out, *inc)
	}
	ix.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedTS != out[j].UpdatedTS {
			return out[i].UpdatedTS > out[j].UpdatedTS
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (d *dashReader) GroupsSince(seq uint64, limit int) []types.GroupStat {
	if limit <= 0 {
		limit = 100
	}
	ix := d.l.idx
	ix.mu.RLock()
	out := make([]types.GroupStat, 0, 16)
	for _, ge := range ix.order {
		if ge.retired || ge.lastSeq <= seq {
			continue
		}
		st := ge.stat
		st.Cold = false
		out = append(out, st)
	}
	ix.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastSeenTS != out[j].LastSeenTS {
			return out[i].LastSeenTS > out[j].LastSeenTS
		}
		return out[i].GroupID < out[j].GroupID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (d *dashReader) RecordsForIncident(inc string, since uint64, limit int) []types.Record {
	rows := d.l.idx.dash.forIncident(inc, since, limit)
	out := make([]types.Record, 0, len(rows))
	for _, r := range rows {
		out = append(out, types.Record{
			Seq:  r.Seq,
			TS:   r.TS,
			Kind: r.Kind,
			Sig:  r.Sig,
			Inc:  r.Inc,
			Actor: types.Actor{
				Kind: types.ActorHuman,
				ID:   r.ActorID,
			},
			Payload: map[string]any{
				"summary":    r.Summary,
				"error_code": r.ErrCode,
			},
		})
	}
	return out
}

func (d *dashReader) LastEventAge(sig string, now time.Time) (float64, bool) {
	return d.l.idx.dash.lastEventAge(sig, now)
}

func (d *dashReader) EventsBetween(sig, fromTS, toTS string) int {
	return d.l.idx.dash.countBetween(sig, fromTS, toTS)
}

// lastRefSeq returns the newest seq recorded for an incident (0 when none).
func lastRefSeq(refs []uint64) uint64 {
	if len(refs) == 0 {
		return 0
	}
	return refs[len(refs)-1]
}
