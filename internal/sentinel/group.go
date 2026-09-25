package sentinel

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/trouble-agent/trouble/internal/hub"
	"github.com/trouble-agent/trouble/internal/types"
)

// groupFlushEvents is the per-group flush trigger of §3.3: 100 events or the
// group_flush interval, whichever comes first.
const groupFlushEvents = 100

// dupWindow is the SDK-retry dedup window of §6.6: a repeated event_id inside
// 10 minutes is a retry, not a new event.
const dupWindow = 10 * time.Minute

// maxDupEntries bounds the dedup map so a hostile client cannot grow it without
// limit; the oldest entries are evicted first.
const maxDupEntries = 65536

// groupState is the live projection of one digest.
type groupState struct {
	grp            types.Group
	eventsUpperSeq uint64
	pendingEvents  int
	lastRelease    string
	created        bool
	sampleRate     float64
	sample         string // rec_id of the representative event (§3.9a Sample)
	dirty          bool
}

// groupIndex is the digest-keyed group index of §3.3.
type groupIndex struct {
	mu       sync.Mutex
	byDigest map[string]*groupState
	order    []string
}

func newGroupIndex() *groupIndex {
	return &groupIndex{byDigest: map[string]*groupState{}}
}

// observeResult says what the caller must persist after one event.
type observeResult struct {
	created bool
	release string // non-empty when the release changed
	flush   bool   // the 100-event trigger fired
	group   *groupState
}

// observe folds one admitted event into the index.
func (g *groupIndex) observe(sig types.Sig, digest string, ts, release, title string, redactions int) observeResult {
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.byDigest[digest]
	res := observeResult{}
	if st == nil {
		st = &groupState{
			grp: types.Group{
				ID:           types.NewID(types.PGrp),
				Sig:          sig.String(),
				Digest:       digest,
				Source:       types.SrcSentinel,
				Title:        title,
				FirstSeenTS:  ts,
				LastSeenTS:   ts,
				Count:        1,
				Counters:     types.GroupCounters{Events: 1, Redacted: uint64(redactions)},
				ReleaseRange: []string{release, release},
			},
			lastRelease: release,
			created:     true,
		}
		g.byDigest[digest] = st
		g.order = append(g.order, digest)
		res.created = true
	} else {
		st.grp.Count++
		st.grp.LastSeenTS = ts
		st.grp.Counters.Events++
		st.grp.Counters.Redacted += uint64(redactions)
		if release != st.lastRelease {
			st.lastRelease = release
			res.release = release
			st.grp.ReleaseRange = releaseRange(st.grp.ReleaseRange, release)
		}
	}
	st.pendingEvents++
	if st.pendingEvents >= groupFlushEvents {
		st.pendingEvents = 0
		res.flush = true
	}
	res.group = st
	return res
}

// releaseRange maintains `[first_seen_release, last_seen_release]`.
func releaseRange(current []string, release string) []string {
	first := ""
	if len(current) > 0 {
		first = current[0]
	}
	if first == "" && release != "" {
		first = release
	}
	return []string{first, release}
}

// groupStateCopy is a locked snapshot of one group's mutable state. Callers that
// need to build a record from a group take a copy: handing out the live
// *groupState would let a request goroutine read fields another request
// goroutine is writing (the counters are hot).
type groupStateCopy struct {
	grp            types.Group
	eventsUpperSeq uint64
	sampleRate     float64
	sample         string
	dirty          bool
}

// state returns a locked snapshot of one digest's state.
func (g *groupIndex) state(digest string) (groupStateCopy, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st, ok := g.byDigest[digest]
	if !ok {
		return groupStateCopy{}, false
	}
	return st.copyLocked(), true
}

// copyLocked copies a state while g.mu is held.
func (st *groupState) copyLocked() groupStateCopy {
	cp := st.grp
	cp.ReleaseRange = append([]string(nil), st.grp.ReleaseRange...)
	return groupStateCopy{
		grp:            cp,
		eventsUpperSeq: st.eventsUpperSeq,
		sampleRate:     st.sampleRate,
		sample:         st.sample,
		dirty:          st.dirty,
	}
}

// markFlushed records the sequence watermark a flush reached.
func (g *groupIndex) markFlushed(digest string, seq uint64, sampleRate float64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.byDigest[digest]
	if st == nil {
		return
	}
	st.eventsUpperSeq = seq
	st.sampleRate = sampleRate
	st.dirty = false
}

// markDropped adds a loss-policy drop to a group's counters.
func (g *groupIndex) markDropped(digest string, dropped uint64, sampleRate float64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.byDigest[digest]
	if st == nil {
		return
	}
	st.grp.Counters.Dropped += dropped
	if sampleRate > 0 {
		st.grp.Counters.SampleRate = sampleRate
	}
	st.dirty = true
}

// markSuppressed folds one suppressed retry into a group's counters (§3.4a):
// `count` is the occurrences the ledger holds and `counters.suppressed` is the
// retries the fold absorbed — a suppressed retry is never a dropped event, so
// it moves its own counter and the group record says both numbers.
func (g *groupIndex) markSuppressed(digest string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.byDigest[digest]
	if st == nil {
		return
	}
	st.grp.Counters.Suppressed++
	st.dirty = true
}

// snapshot returns a copy of every group in creation order.
func (g *groupIndex) snapshot() []types.Group {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]types.Group, 0, len(g.order))
	for _, d := range g.order {
		st := g.byDigest[d]
		if st == nil {
			continue
		}
		cp := st.grp
		cp.ReleaseRange = append([]string(nil), st.grp.ReleaseRange...)
		out = append(out, cp)
	}
	return out
}

// get returns a copy of one group by digest.
func (g *groupIndex) get(digest string) (types.Group, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st, ok := g.byDigest[digest]
	if !ok {
		return types.Group{}, false
	}
	cp := st.grp
	cp.ReleaseRange = append([]string(nil), st.grp.ReleaseRange...)
	return cp, true
}

// bySig returns a copy of one group by sig string.
func (g *groupIndex) bySig(sig string) (types.Group, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, d := range g.order {
		st := g.byDigest[d]
		if st != nil && st.grp.Sig == sig {
			cp := st.grp
			cp.ReleaseRange = append([]string(nil), st.grp.ReleaseRange...)
			return cp, true
		}
	}
	return types.Group{}, false
}

// len counts the groups held.
func (g *groupIndex) len() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.byDigest)
}

// restore seeds the index from the ledger at boot (§3.3 rebuild): the folded
// `group` record is the base, `event` records after its `events_upper_seq` add
// the delta.
func (g *groupIndex) restore(grp types.Group, eventsUpperSeq uint64, lastRelease string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.byDigest[grp.Digest]; ok {
		return
	}
	st := &groupState{grp: grp, eventsUpperSeq: eventsUpperSeq, lastRelease: lastRelease, created: true}
	if len(grp.ReleaseRange) == 0 {
		st.grp.ReleaseRange = []string{"", ""}
	}
	g.byDigest[grp.Digest] = st
	g.order = append(g.order, grp.Digest)
}

// replayEvent folds one `event` record into a restored group.
func (g *groupIndex) replayEvent(digest string, seq uint64, ts, release string, redactions int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.byDigest[digest]
	if st == nil || seq <= st.eventsUpperSeq {
		return
	}
	st.grp.Count++
	st.grp.Counters.Events++
	st.grp.Counters.Redacted += uint64(redactions)
	if ts > st.grp.LastSeenTS {
		st.grp.LastSeenTS = ts
	}
	if release != "" && release != st.lastRelease {
		st.lastRelease = release
		st.grp.ReleaseRange = releaseRange(st.grp.ReleaseRange, release)
	}
}

// digestSet returns the sorted digest list (test/report helper).
func (g *groupIndex) digestSet() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := append([]string(nil), g.order...)
	sort.Strings(out)
	return out
}

// --- record emission -------------------------------------------------------

// appendRecord writes one sentinel record through the ledger sink.
//
// The sink owns seq/rec_id/ts allocation, so sentinel builds a draft. The
// package records its own counters and never blocks longer than LedgerWait
// (§6.13): a sink that reports backpressure turns into a 429 for the request.
//
// The AC-28 route decision is stamped HERE, on the draft's Origin, before the
// record exists: `origin.route` then survives every downstream transport — the
// in-process ledger, the door → stream → consumer → ledger pipeline of the
// light-hub profile (hub.localRecord copies draft.Origin; hub.consume's
// draftFromRecord carries rec.Origin back) and the satellite spool's
// round-trip — so a reader answers "local or relayed?" without a join
// (§3.10a). The sig is the class the resolution ran against; records with no
// sig carry the default's answer.
func (s *Server) appendRecord(ctx context.Context, kind types.RecordKind, sig, source string, payload map[string]any, redactions int) (types.Record, *Error) {
	return s.appendRecordDraft(ctx, types.RecordDraft{
		Kind:       kind,
		Sig:        sig,
		Origin:     types.Origin{HostID: s.cfg.HostID, Source: source, Route: string(s.routeTable.resolve(sig))},
		Actor:      s.cfg.Actor,
		Redactions: redactions,
		Payload:    payload,
	})
}

// appendRecordDraft is appendRecord for a pre-built draft: the draft's
// in-memory Codeplane ride-along (§3.9a) survives to the returned record, so
// the sink that bridges into the ladder can hand the bundle over with the
// group record.
func (s *Server) appendRecordDraft(ctx context.Context, draft types.RecordDraft) (types.Record, *Error) {
	source := draft.Origin.Source
	if source == "" {
		source = "sentinel"
	}
	draft.Origin.Source = source
	if draft.Payload == nil {
		draft.Payload = map[string]any{}
	}
	if draft.Origin.HostID == "" {
		draft.Origin.HostID = s.cfg.HostID
	}
	// AC-28 (TRBL-061): the route stamp belongs to the record, not to the
	// wrapper that happened to build it. appendRecord sets it explicitly, but
	// a pre-built draft (§3.9a) arrives with a zero Origin, and the group
	// records written on auto-resolution/regression paths are built that way —
	// so the stamp is filled here, at the single choke point every record
	// passes through. Without it those records carry origin.route="" and a
	// reader can no longer answer "local or relayed?" without a join (§3.10a).
	if draft.Origin.Route == "" {
		draft.Origin.Route = string(s.routeTable.resolve(draft.Sig))
	}
	if draft.Actor.Kind == "" {
		draft.Actor = s.cfg.Actor
	}
	type res struct {
		rec types.Record
		err error
	}
	ch := make(chan res, 1)
	go func() {
		rec, err := s.sink.Append(ctx, draft)
		ch <- res{rec, err}
	}()
	t := time.NewTimer(s.cfg.LedgerWait.Std())
	defer t.Stop()
	select {
	case r := <-ch:
		if r.err != nil {
			// A hub queue failure is answered by §4.3's matrix, not as a
			// generic ledger failure: require_redis=true's runtime refusal is
			// 503 + Retry-After (hard unavailability), everything else stays
			// the overload 429 the SDK contract already pins.
			if hubCode := hub.CodeOf(r.err); hubCode != "" {
				return types.Record{}, &Error{
					Code:        types.CodeSentinel010,
					Status:      statusForHub(r.err),
					Causes:      []string{causeOverloaded},
					Msg:         "ingestion unavailable: " + string(hubCode),
					RetryAfterS: 1,
				}
			}
			return types.Record{}, errf(types.CodeSentinel010, "ledger append failed", causeOverloaded)
		}
		return r.rec, nil
	case <-t.C:
		return types.Record{}, errf(types.CodeSentinel010, "ledger backpressure", causeOverloaded)
	case <-ctx.Done():
		return types.Record{}, errf(types.CodeSentinel010, "request cancelled during append", causeOverloaded)
	}
}

// eventRecordPayload builds the §4 `event` record payload for an admitted,
// scrubbed event.
func eventRecordPayload(ev *rawEvent, sig types.Sig, itemType string, redactions int, extra map[string]any) map[string]any {
	se := types.SentryEvent{
		ID:          ev.ID,
		Project:     ev.Project,
		TS:          ev.TS,
		Level:       ev.Level,
		Message:     ev.Message,
		Culprit:     ev.Culprit,
		Stack:       renderStack(ev.Frames),
		Fingerprint: ev.Fingerprint,
		Release:     ev.Release,
		Env:         ev.Env,
		Sig:         sig,
		Redactions:  redactions,
		ItemTypes:   ev.ItemTypes,
	}
	p := map[string]any{
		"item_type":    itemType,
		"native_id":    ev.ID,
		"event":        se,
		"auth_form":    ev.AuthForm,
		"source_kind":  ev.SourceKind,
		"zone":         ev.Zone,
		"digest":       sig.DigestHex(),
		"norm_version": sig.NormVersion,
	}
	if ev.ItemTypes != nil {
		p["item_types"] = ev.ItemTypes
	}
	if ev.LevelDegraded {
		p["invalid_level"] = true
	}
	if ev.Partial {
		p["partial"] = true
	}
	if ev.FlushReason != "" {
		p["flush_reason"] = ev.FlushReason
	}
	if ev.Truncated {
		p["truncated"] = true
	}
	if ev.ClockSkewS != 0 {
		p["clock_skew_s"] = ev.ClockSkewS
	}
	if ev.CollectorParser != "" {
		p["parser"] = ev.CollectorParser
	}
	if ev.CollectorSource != "" {
		p["collector_source"] = ev.CollectorSource
	}
	if ev.ClientReport != nil {
		p["client_report"] = ev.ClientReport
	}
	if ev.SourceKind == sourceCanary {
		p["canary"] = true
	}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

// renderStack renders frames in the `in_app|module|function|file|LINE|context`
// form stored in SentryEvent.Stack (already scrubbed and normalized).
func renderStack(frames []frame) string {
	if len(frames) == 0 {
		return ""
	}
	var b []byte
	for i, f := range frames {
		if i > 0 {
			b = append(b, '\n')
		}
		b = append(b, frameNorm(f)...)
	}
	return string(b)
}

// groupRecordPayload builds a `group` record payload (§3.3's op vocabulary) from
// a locked snapshot.
func groupRecordPayload(op string, st groupStateCopy, extra map[string]any) map[string]any {
	p := map[string]any{
		"op":               op,
		"digest":           st.grp.Digest,
		"sig":              st.grp.Sig,
		"group_id":         st.grp.ID,
		"title":            st.grp.Title,
		"count":            st.grp.Count,
		"counters":         st.grp.Counters,
		"first_seen_ts":    st.grp.FirstSeenTS,
		"last_seen_ts":     st.grp.LastSeenTS,
		"release_range":    st.grp.ReleaseRange,
		"norm_version":     types.NormVersionV1,
		"events_upper_seq": st.eventsUpperSeq,
		"aggregate":        true,
	}
	if st.sampleRate > 0 {
		p["sample_rate"] = st.sampleRate
	}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

// gapRecordPayload builds a `gap` record payload (one cause, one record).
func gapRecordPayload(cause, scope string, estLost int, fromTS, toTS string) map[string]any {
	return map[string]any{
		"cause":    cause,
		"sensor":   "sentinel",
		"scope":    scope,
		"est_lost": estLost,
		"from_ts":  fromTS,
		"to_ts":    toTS,
	}
}

// --- payload readers --------------------------------------------------------
//
// A payload read from the ledger has been through canonical JSON, so every
// number is a float64, every array an []any and every object a map[string]any.
// A payload read from an in-process sink still holds its Go types. These helpers
// accept both, so the rebuild path is identical in tests and in production.

func asString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func asUint64(v any) (uint64, bool) {
	switch n := v.(type) {
	case float64:
		if n < 0 {
			return 0, false
		}
		return uint64(n), true
	case int:
		if n < 0 {
			return 0, false
		}
		return uint64(n), true
	case int64:
		if n < 0 {
			return 0, false
		}
		return uint64(n), true
	case uint64:
		return n, true
	case uint:
		return uint64(n), true
	case json.Number:
		i, err := n.Int64()
		if err != nil || i < 0 {
			return 0, false
		}
		return uint64(i), true
	}
	return 0, false
}

func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func asStringSlice(v any) []string {
	switch t := v.(type) {
	case []string:
		return append([]string(nil), t...)
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// asCounters reads a GroupCounters value in either shape.
func asCounters(v any) (types.GroupCounters, bool) {
	switch c := v.(type) {
	case types.GroupCounters:
		return c, true
	case map[string]any:
		var out types.GroupCounters
		if n, ok := asUint64(c["events"]); ok {
			out.Events = n
		}
		if n, ok := asUint64(c["suppressed"]); ok {
			out.Suppressed = n
		}
		if n, ok := asUint64(c["redacted_values"]); ok {
			out.Redacted = n
		}
		if n, ok := asUint64(c["dropped_events"]); ok {
			out.Dropped = n
		}
		if f, ok := asFloat(c["sample_rate"]); ok {
			out.SampleRate = f
		}
		return out, true
	}
	return types.GroupCounters{}, false
}

// eventProject reads the project id of an `event` record's nested event.
func eventProject(payload map[string]any) string {
	switch e := payload["event"].(type) {
	case types.SentryEvent:
		return e.Project
	case map[string]any:
		return asString(e, "project")
	}
	return ""
}

// eventPayloadFields reads the (ts, release) of an `event` record regardless of
// whether the nested event arrived as a struct or as decoded JSON.
func eventPayloadFields(payload map[string]any) (ts, release string) {
	switch e := payload["event"].(type) {
	case types.SentryEvent:
		return e.TS, e.Release
	case map[string]any:
		return asString(e, "ts"), asString(e, "release")
	}
	return "", ""
}
