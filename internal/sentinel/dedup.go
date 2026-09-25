package sentinel

import (
	"context"
	"sync"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// The §3.4a generic-JSON fold (TRBL-074).
//
// The generic JSON on-ramp (§3.6) accepts a body with no event_id: a client
// that retries on a timeout sends byte-identical bodies, so the §6.6 id-dedup
// (which keys on the event id an SDK mints once per occurrence) never sees the
// retry. Before the fold, every retrying POST minted a fresh event id, wrote
// its own `event` record and folded into the group as a new occurrence — the
// retry storm duplicated evidence and inflated group counts.
//
// The fold is the standalone-profile counterpart of the hub's phase-aware
// dedup gate (SPEC-13 §2.3): duplicates keyed on the fold key inside a bounded
// window are accepted-and-ignored at the admission seam. The first POST of a
// window is the occurrence: it scrubs, admits, opens or folds into its group
// and writes its `event` record exactly as §3.3 pins. Every identical POST
// inside the window is answered 200 with the FIRST occurrence's event id (the
// idempotent-retry answer) and folded into the group as
// counters.suppressed += 1 on a group record with payload op=update — so the
// group's count stays 1 while the suppression counter says how many retries
// the record stands for. The key is bounded by the same discipline as the
// §6.6 map (an LRU cap), and window <= 0 (the `0` spelling) disables the fold
// entirely, reproducing the pre-fix posture.

// genericFoldMax bounds the fold key table: the table cannot exceed the
// distinct (project, sig) pairs observed inside a window, and the cap is the
// backstop that keeps that statement true under a pathological burst. The
// oldest keys are evicted first.
const genericFoldMax = 65536

// genericFoldEntry is one open window: when it opened and the event id of the
// occurrence that opened it (the idempotent-retry answer of §3.4a — every
// folded retry is answered 200 with the FIRST occurrence's id, so a reporter
// can correlate its own retries).
type genericFoldEntry struct {
	opened  time.Time
	firstID string
}

// genericFold is the bounded dedup window over generic-JSON admissions.
type genericFold struct {
	mu     sync.Mutex
	window time.Duration
	keys   map[string]genericFoldEntry
	order  []string
}

func newGenericFold(window time.Duration) *genericFold {
	return &genericFold{window: window, keys: map[string]genericFoldEntry{}}
}

// enabled reports whether the fold is configured: window <= 0 is OFF (§3.4a).
func (f *genericFold) enabled() bool { return f != nil && f.window > 0 }

// claim records the key's window opening at now with firstID as the
// occurrence's id, and reports whether the key was ALREADY inside its window
// (wasSeen true = a fold: the caller absorbs the event and answers first).
func (f *genericFold) claim(key, firstID string, now time.Time) (first string, wasSeen bool) {
	if !f.enabled() {
		return "", false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if e, ok := f.keys[key]; ok && now.Sub(e.opened) < f.window {
		return e.firstID, true
	}
	f.keys[key] = genericFoldEntry{opened: now, firstID: firstID}
	f.order = append(f.order, key)
	for len(f.order) > genericFoldMax {
		old := f.order[0]
		f.order = f.order[1:]
		delete(f.keys, old)
	}
	return "", false
}

// newGenericFoldFromConfig builds the fold the server admits through. It is a
// server field, so the fold shares the §6.6 map's lifetime: a restart opens a
// cold window, which is the documented bounded-window posture (a crash that
// loses a fold costs one duplicate event record, never a lost one).
func newGenericFoldFromConfig(cfg Config) *genericFold {
	return newGenericFold(cfg.GenericDedupWindow.Std())
}

// genericFoldKey derives the §3.4a fold key: one key per (project, sig). The
// sig digest is the §3.3 identity itself, so the fold cannot disagree with the
// group: identical bodies land on one key and distinct bodies never do. The
// event id is deliberately NOT part of the key — an id-less retry must match
// the occurrence it retries.
func genericFoldKey(project string, sig types.Sig) string {
	return "generic:" + project + ":" + sig.DigestHex()
}

// genericFoldApplies reports whether one admission is foldable at all (§3.4a).
// Four facts take an event out of the fold's scope before the window is asked:
//
//   - the source is not the generic on-ramp: envelope, store and collector
//     events carry real event ids (the SDK mints one per occurrence), so §6.6
//     is their dedup;
//   - the event is accounted-but-ungrouped (a client_report has no group to
//     fold into);
//   - the event carried an EXPLICIT event_id: §3.6 makes that field the
//     reporter's occurrence identity and §6.6 dedups on it — a client that
//     mints ids has already told us which retries are retries;
//   - the admission is a spool replay (reason "spool_replay"): a replayed
//     event was accepted once and must land exactly once, never be absorbed.
func genericFoldApplies(ev *rawEvent, reason string, explicitID bool) bool {
	return ev.SourceKind == sourceGeneric && !ev.NoGroup && !explicitID && reason != "spool_replay"
}

// foldGenericRetry applies the §3.4a fold to one generic-JSON admission. It
// returns true when the event was folded (accepted-and-ignored): the caller
// answers 200 with firstID and touches nothing else. The first occurrence of a
// window passes through untouched — its `event` record, group folding and
// counters are the ordinary §3.3 path.
func (s *Server) foldGenericRetry(entry *projectEntry, ev *rawEvent, sig types.Sig, now time.Time) (folded bool, firstID string) {
	first, seen := s.genFold.claim(genericFoldKey(entry.proj.ID, sig), ev.ID, now)
	if !seen {
		return false, ""
	}
	return true, first
}

// foldGenericGroup folds one suppressed retry into the group the occurrence
// opened: counters.suppressed += 1 (§3.3's suppressed-event row), and one
// `group` record with payload op=update is appended so the ledger carries the
// fold without re-scanning the file. The record's counters snapshot and its
// `events_upper_seq` follow the shipped flush semantics (snapshot, append,
// then advance the index watermark): the fold writes no `event` record, so the
// rebuild replays nothing for the folded retries themselves.
func (s *Server) foldGenericGroup(ctx context.Context, entry *projectEntry, digest string) {
	s.groups.markSuppressed(digest)
	st, ok := s.groups.state(digest)
	if !ok {
		// The window says fold but the group index no longer holds the group
		// (a test rebuilt it, or the group was evicted): the retry is still
		// absorbed — the window is the fold's authority — but there is no
		// group record to update.
		return
	}
	draft := types.RecordDraft{
		Kind:    types.KGroup,
		Sig:     st.grp.Sig,
		Origin:  types.Origin{HostID: s.cfg.HostID, Source: "sentinel", Route: string(s.routeTable.resolve(st.grp.Sig))},
		Actor:   s.cfg.Actor,
		Payload: groupRecordPayload("update", st, map[string]any{"suppressed_retry": true}),
	}
	rec, gerr := s.appendRecordDraft(ctx, draft)
	if gerr != nil {
		s.logger.Printf("sentinel: group update record failed: %v", gerr)
		return
	}
	// The update record is the new watermark: the counters snapshot it carries
	// already includes every event up to the last real event, so the group
	// stops being dirty and the periodic flush has nothing to repeat.
	s.groups.markFlushed(digest, rec.Seq, st.sampleRate)
}
