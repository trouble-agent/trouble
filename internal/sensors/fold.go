package sensors

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// foldMaxOpen bounds the folds held in memory (SPEC-03 §3.8a). A fold key is a
// signature and a fold is held for one window, so the table cannot exceed the
// number of distinct signatures observed inside a window; the cap is the
// backstop that keeps that statement true under a pathological burst, and it is
// reached by CLOSING the oldest fold (which writes its record) rather than by
// discarding one — a dropped fold would drop its observations.
const foldMaxOpen = 4096

// eventFold is the count-preserving fold for repeated identical sampled
// observations (SPEC-03 §3.8a).
//
// P3 makes sampling the source of truth: one `SensorEvent` per
// `sensors.psi.sample_interval` for a condition that has not changed. Persisting
// one `event` record per cycle is the amplifier TRBL-009 measured — a single
// signature wrote 122 records in 259s on an idle host, under the global breaker
// cap, so no breaker ever engaged.
//
// The fold keeps the observations and writes ONE record per (signature, fold
// window). That record carries count/first_ts/last_ts, so nothing is lost: the
// count IS the drop-proof and `fold: true` names the disposition. Everything
// that decides anything happens BEFORE the fold and per observation — rules
// (§3.4/§3.5), stabilization, breakers (§3.8) — so suppression counters, breaker
// state and the "events are never dropped by a breaker" rule are untouched; only
// the record's own cardinality changes, and the count says how many observations
// the record stands for.
type eventFold struct {
	window time.Duration
	// drop accounts an emit failure on the sensor that produced the folded
	// observation: a fold that cannot be written must not be silent.
	drop func(types.SensorKind)

	mu   sync.Mutex
	open map[string]*foldEntry

	observations atomic.Uint64 // observations absorbed by a fold
	records      atomic.Uint64 // records a fold close wrote
	overflow     atomic.Uint64 // folds closed early because the table was full

	emitFailures atomic.Uint64 // fold records the emit path refused
}

// foldEntry is one open fold: the FIRST observation's draft (which is what gets
// written) plus the accounting the record must carry.
type foldEntry struct {
	key     string
	sensor  types.SensorKind
	draft   types.RecordDraft
	firstTS time.Time
	lastTS  time.Time
	count   uint64
}

func newEventFold(window time.Duration, drop func(types.SensorKind)) *eventFold {
	return &eventFold{window: window, drop: drop, open: map[string]*foldEntry{}}
}

// enabled reports whether the fold is configured at all: `0` is OFF and
// reproduces the one-record-per-cycle posture (SPEC-03 §3.8a).
func (f *eventFold) enabled() bool { return f != nil && f.window > 0 }

// observe absorbs one foldable observation. An observation that starts a NEW
// window closes the fold it replaces, so a record is never held past the window
// that owns it.
func (f *eventFold) observe(ctx context.Context, emit EmitFunc, d types.RecordDraft, sensor types.SensorKind, now time.Time) {
	if !f.enabled() {
		return
	}
	var closed []*foldEntry
	f.mu.Lock()
	e := f.open[d.Sig]
	if e != nil && !now.Before(e.firstTS.Add(f.window)) {
		// The window this fold belongs to is over: close it and open the next.
		closed = append(closed, e)
		delete(f.open, d.Sig)
		e = nil
	}
	if e == nil {
		if len(f.open) >= foldMaxOpen {
			if oldest := f.oldestLocked(); oldest != nil {
				closed = append(closed, oldest)
				delete(f.open, oldest.key)
				f.overflow.Add(1)
			}
		}
		e = &foldEntry{key: d.Sig, sensor: sensor, draft: d, firstTS: now, lastTS: now, count: 1}
		f.open[d.Sig] = e
	} else {
		e.count++
		e.lastTS = now
	}
	f.mu.Unlock()

	for _, c := range closed {
		f.close(ctx, emit, c)
	}
	f.observations.Add(1)
}

// closeElapsed writes every fold whose window has passed. It is called on each
// arrival, so the record of a fold whose signature stopped repeating is written
// on the first observation that arrives afterwards instead of waiting for that
// signature to come back.
func (f *eventFold) closeElapsed(ctx context.Context, emit EmitFunc, now time.Time) {
	if !f.enabled() {
		return
	}
	f.mu.Lock()
	var closed []*foldEntry
	for k, e := range f.open {
		if !now.Before(e.firstTS.Add(f.window)) {
			closed = append(closed, e)
			delete(f.open, k)
		}
	}
	f.mu.Unlock()
	f.closeAll(ctx, emit, closed)
}

// closeSig writes the fold for one signature, if it is open. It is called before
// a record that must NOT be folded (a fire, a wake, a non-sampled event) is
// emitted, so the ledger keeps the chronology: the folded observations first,
// then the record that ended the fold.
func (f *eventFold) closeSig(ctx context.Context, emit EmitFunc, sig string) {
	if !f.enabled() {
		return
	}
	f.mu.Lock()
	e := f.open[sig]
	delete(f.open, sig)
	f.mu.Unlock()
	if e != nil {
		f.close(ctx, emit, e)
	}
}

// flush writes every open fold (the shutdown path): an observation that already
// happened is never lost to a stop.
func (f *eventFold) flush(ctx context.Context, emit EmitFunc) {
	if !f.enabled() {
		return
	}
	f.mu.Lock()
	closed := make([]*foldEntry, 0, len(f.open))
	for k, e := range f.open {
		closed = append(closed, e)
		delete(f.open, k)
	}
	f.mu.Unlock()
	f.closeAll(ctx, emit, closed)
}

// openCount is the number of folds currently held (test/diagnostic surface).
func (f *eventFold) openCount() int {
	if f == nil {
		return 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.open)
}

// oldestLocked returns the fold to close when the table is full: the one that
// has been open longest, ties broken by key so the choice is deterministic.
func (f *eventFold) oldestLocked() *foldEntry {
	var oldest *foldEntry
	for _, e := range f.open {
		switch {
		case oldest == nil:
			oldest = e
		case e.firstTS.Before(oldest.firstTS):
			oldest = e
		case e.firstTS.Equal(oldest.firstTS) && e.key < oldest.key:
			oldest = e
		}
	}
	return oldest
}

func (f *eventFold) closeAll(ctx context.Context, emit EmitFunc, closed []*foldEntry) {
	sort.Slice(closed, func(i, j int) bool {
		if closed[i].firstTS.Equal(closed[j].firstTS) {
			return closed[i].key < closed[j].key
		}
		return closed[i].firstTS.Before(closed[j].firstTS)
	})
	for _, e := range closed {
		f.close(ctx, emit, e)
	}
}

// close renders one fold as ONE record: the first observation's payload, with
// the fold's own accounting layered on it (§3.8a).
func (f *eventFold) close(ctx context.Context, emit EmitFunc, e *foldEntry) {
	p := e.draft.Payload
	// `count` is the number of identical sampled observations this record stands
	// for — the drop-proof. `first_ts`/`last_ts` are the window the folded
	// observations cover and `fold` names the disposition.
	p["count"] = e.count
	p["first_ts"] = types.FormatUTC(e.firstTS)
	p["last_ts"] = types.FormatUTC(e.lastTS)
	p["fold"] = true
	p["fold_window_s"] = f.window.Seconds()
	if _, err := emit(ctx, e.draft); err != nil {
		f.emitFailures.Add(1)
		if f.drop != nil {
			f.drop(e.sensor)
		}
		return
	}
	f.records.Add(1)
}

// foldableObservation reports whether one event may be folded (SPEC-03 §3.8a):
// a plain sampled observation (a sampler tick, never a trigger wake) for which
// no rule fired. A firing event is the ladder's input and is always written
// immediately, and a wake is an edge observation the sampler did not produce —
// neither is a repeat.
func foldableObservation(ev types.SensorEvent, d types.RecordDraft) bool {
	if ev.Wake {
		return false
	}
	if fire, _ := d.Payload["fire"].(bool); fire {
		return false
	}
	backed, _ := ev.Detail["sample_backed"].(bool)
	return backed
}
