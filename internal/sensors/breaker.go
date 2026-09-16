package sensors

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// SPEC-03 §3.8: storm breakers and caps. Sensors own breaker *state and
// evaluation* and publish it through Breakers(); internal/ladder emits the
// `breaker` ledger record at each transition and suppresses ladder work while a
// scope is open (TROUBLE-LADDER-014). Events are never dropped by a breaker:
// they are recorded and suppressed at the ladder gate, and the counts stay
// honest in SensorHealth.Dropped / Group.Counters.Suppressed.

// Breaker window constants (SPEC-03 §3.8).
const (
	sustainWindow   = 2 * time.Minute // "sustained 2 min"
	incidentWindow  = 300 * time.Second
	reopenWindow    = time.Hour
	baseOpenBackoff = 60 * time.Second
	maxOpenBackoff  = 30 * time.Minute
	rateBuckets     = 180 // one per second: long enough for the 120s sustain test
)

type decision struct {
	allowed bool
	probe   bool // half-open probe: exactly one of these per scope per open
	scope   string
	reason  string
}

type breakerState struct {
	scope     string
	state     types.BreakerState
	openedAt  time.Time
	openUntil time.Time
	backoff   time.Duration
	trips     int
	reason    string
	probed    bool

	buckets     [rateBuckets]uint64
	lastBucket  int64
	overSince   time.Time
	incidents   []time.Time
	lastOpenFor time.Time
}

type breakerTable struct {
	mu     sync.Mutex
	limit  limitsConf
	scopes map[string]*breakerState
}

func newBreakerTable(l limitsConf) *breakerTable {
	return &breakerTable{limit: l, scopes: map[string]*breakerState{}}
}

func (b *breakerTable) get(scope string) *breakerState {
	st, ok := b.scopes[scope]
	if !ok {
		st = &breakerState{scope: scope, state: types.BreakerClosed, backoff: baseOpenBackoff}
		b.scopes[scope] = st
	}
	return st
}

func (b *breakerTable) capFor(scope string) int {
	switch {
	case scope == "global":
		return b.limit.globalPerMin
	case len(scope) > 5 && scope[:5] == "rule:":
		return b.limit.rulePerMin
	case len(scope) > 7 && scope[:7] == "source:":
		return b.limit.sourcePerMin
	}
	// sig:<sig> has no per-minute cap: it trips on reopens (below).
	return 0
}

// Decide is the gate: it reports whether an event for this scope may be
// evaluated, and whether it is the single half-open probe (SPEC-03 §3.8).
func (b *breakerTable) Decide(scope string, now time.Time) decision {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.get(scope)
	st.roll(now)

	if st.state == types.BreakerOpen {
		if now.Before(st.openUntil) {
			return decision{allowed: false, scope: scope, reason: st.reason}
		}
		// The open window elapsed: exactly one probe is evaluated.
		st.state = types.BreakerHalfOpen
		st.probed = false
	}
	if st.state == types.BreakerHalfOpen {
		if st.probed {
			return decision{allowed: false, probe: true, scope: scope, reason: st.reason}
		}
		st.probed = true
		return decision{allowed: true, probe: true, scope: scope}
	}

	cap := b.capFor(scope)
	if cap <= 0 {
		return decision{allowed: true, scope: scope}
	}
	if st.rate1m() >= uint64(cap) {
		if st.overSince.IsZero() {
			st.overSince = now
		}
		if now.Sub(st.overSince) >= sustainWindow {
			b.tripLocked(st, now, fmt.Sprintf("%s over cap %d/min for %s", scope, cap, sustainWindow))
			return decision{allowed: false, scope: scope, reason: st.reason}
		}
		return decision{allowed: false, scope: scope, reason: "over cap"}
	}
	if !st.overSince.IsZero() {
		st.overSince = time.Time{}
	}
	return decision{allowed: true, scope: scope}
}

// Observe accounts one evaluated event against every scope that covers it.
func (b *breakerTable) Observe(scopes []string, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, scope := range scopes {
		st := b.get(scope)
		st.roll(now)
		st.bump(now)
	}
}

// OpenIncident accounts "an incident was opened for this sig" (SPEC-03 §3.8
// rule scope's second arm, and the sig scope's flap detector).
func (b *breakerTable) OpenIncident(sig string, scopes []string, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, scope := range scopes {
		st := b.get(scope)
		st.roll(now)
		st.incidents = prune(st.incidents, now.Add(-incidentWindow))
		st.incidents = append(st.incidents, now)
	}
	if sig != "" {
		st := b.get("sig:" + sig)
		st.roll(now)
		st.incidents = prune(st.incidents, now.Add(-reopenWindow))
		st.incidents = append(st.incidents, now)
		if len(st.incidents) > 5 && st.state == types.BreakerClosed {
			b.tripLocked(st, now, fmt.Sprintf("flap: %d arrivals for %s inside %s", len(st.incidents), sig, reopenWindow))
		}
	}
	for _, scope := range scopes {
		st := b.get(scope)
		if scope == "global" && len(st.incidents) > b.limit.incidentsPer5m && st.state == types.BreakerClosed {
			b.tripLocked(st, now, fmt.Sprintf("global: %d incidents inside %s", len(st.incidents), incidentWindow))
		}
		if len(scope) > 5 && scope[:5] == "rule:" && len(st.incidents) > b.limit.ruleIncidents5m && st.state == types.BreakerClosed {
			b.tripLocked(st, now, fmt.Sprintf("%s: %d incidents inside %s", scope, len(st.incidents), incidentWindow))
		}
	}
}

// Close records the half-open probe's verdict. A probe that would trip again
// returns the scope to open with a doubled backoff; otherwise it closes
// (SPEC-03 §3.8).
func (b *breakerTable) Close(scope string, now time.Time) (closed bool, trips int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.get(scope)
	st.roll(now)
	if st.state != types.BreakerHalfOpen {
		return false, st.trips
	}
	cap := b.capFor(scope)
	stillOver := cap > 0 && st.rate1m() >= uint64(cap)
	if !stillOver && !st.overSince.IsZero() && now.Sub(st.overSince) >= sustainWindow {
		stillOver = true
	}
	if stillOver {
		b.tripLocked(st, now, "half-open probe still over cap")
		return false, st.trips
	}
	st.state = types.BreakerClosed
	st.openUntil = time.Time{}
	st.backoff = baseOpenBackoff
	st.probed = false
	st.overSince = time.Time{}
	return true, st.trips
}

// tripLocked opens a scope, doubling the backoff per consecutive trip.
func (b *breakerTable) tripLocked(st *breakerState, now time.Time, reason string) {
	st.trips++
	st.state = types.BreakerOpen
	st.openedAt = now
	if st.trips > 1 {
		st.backoff *= 2
		if st.backoff > maxOpenBackoff {
			st.backoff = maxOpenBackoff
		}
	}
	st.openUntil = now.Add(st.backoff)
	st.reason = reason
	st.probed = false
}

// Snapshot renders the observable breaker state, sorted by scope.
func (b *breakerTable) Snapshot() []types.Breaker {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]types.Breaker, 0, len(b.scopes))
	for _, st := range b.scopes {
		if st.state == types.BreakerClosed && st.trips == 0 {
			continue // a closed scope that never tripped is not news
		}
		out = append(out, types.Breaker{
			Scope:     st.scope,
			State:     st.state,
			OpenedTS:  types.FormatUTC(st.openedAt),
			OpenUntil: types.FormatUTC(st.openUntil),
			Trips:     st.trips,
			Reason:    st.reason,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Scope < out[j].Scope })
	return out
}

// bucketed returns the per-second bucket index for t.
func bucketOf(t time.Time) int64 { return t.Unix() }

func (s *breakerState) roll(now time.Time) {
	cur := bucketOf(now)
	if s.lastBucket == 0 {
		s.lastBucket = cur
		return
	}
	if cur <= s.lastBucket {
		return
	}
	gap := cur - s.lastBucket
	if gap >= rateBuckets {
		for i := range s.buckets {
			s.buckets[i] = 0
		}
	} else {
		for i := int64(1); i <= gap; i++ {
			s.buckets[int((s.lastBucket+i)%rateBuckets)] = 0
		}
	}
	s.lastBucket = cur
}

func (s *breakerState) bump(now time.Time) {
	s.buckets[int(bucketOf(now)%rateBuckets)]++
}

func (s *breakerState) rate1m() uint64 {
	var n uint64
	for i := 0; i < 60; i++ {
		n += s.buckets[i]
	}
	return n
}

func prune(ts []time.Time, cutoff time.Time) []time.Time {
	out := ts[:0]
	for _, t := range ts {
		if t.After(cutoff) {
			out = append(out, t)
		}
	}
	return out
}

// ---- stabilization (`for=`) and cooldown, both keyed by (rule, sig) ----

type ruleSigKey struct{ rule, sig string }

// stabilizer holds the per-(rule,sig) continuous-match timers (SPEC-03 §3.5,
// §3.7). Elapsed time is measured with the monotonic clock in production
// (SPEC-INDEX §6.5); persisted timestamps stay RFC3339 UTC wall clock.
//
// The timer table is BOUNDED. A rule over a high-cardinality signature space
// (a per-request path, a unit name, an ever-changing mount) can otherwise grow
// one timer per signature forever, which is a memory leak wearing a
// stabilization window as a disguise. At capacity the least recently seen
// timers are evicted in one batch, so an eviction costs a scan rather than a
// scan per insert; evictions are counted and observable.
type stabilizer struct {
	mu       sync.Mutex
	timers   map[ruleSigKey]*stabTimer
	evicted  uint64
	capacity int
}

const (
	defaultStabCapacity     = 8192
	defaultCooldownCapacity = 65536
	evictionBatchDivisor    = 8
)

type stabTimer struct {
	key      ruleSigKey
	since    time.Time
	accrued  time.Duration
	running  bool
	finger   string
	lastSeen time.Time
}

func newStabilizer() *stabilizer {
	return &stabilizer{timers: map[ruleSigKey]*stabTimer{}, capacity: defaultStabCapacity}
}

// mark records that the condition held at time now, and reports whether it has
// held continuously for at least `need`.
func (st *stabilizer) mark(key ruleSigKey, finger string, need time.Duration, now time.Time) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	t, ok := st.timers[key]
	if !ok {
		st.evictIfFull()
		t = &stabTimer{key: key, finger: finger}
		st.timers[key] = t
	} else if t.finger != finger {
		t = &stabTimer{key: key, finger: finger}
		st.timers[key] = t
	}
	t.lastSeen = now
	if !t.running {
		t.running = true
		t.since = now
		t.accrued = 0
	}
	held := t.accrued + now.Sub(t.since)
	return held >= need
}

// evictIfFull drops the oldest timers in one batch when the table is at
// capacity. The batch (capacity/8) keeps the amortized cost of an insert
// constant instead of one full scan per insert.
func (st *stabilizer) evictIfFull() {
	if st.capacity <= 0 || len(st.timers) < st.capacity {
		return
	}
	batch := st.capacity / evictionBatchDivisor
	if batch < 1 {
		batch = 1
	}
	type victim struct {
		key  ruleSigKey
		seen time.Time
	}
	victims := make([]victim, 0, batch)
	for k, t := range st.timers {
		victims = append(victims, victim{key: k, seen: t.lastSeen})
	}
	sort.Slice(victims, func(i, j int) bool { return victims[i].seen.Before(victims[j].seen) })
	for i := 0; i < batch && i < len(victims); i++ {
		delete(st.timers, victims[i].key)
		st.evicted++
	}
}

// evictions reports how many timers the bound has dropped.
func (st *stabilizer) evictions() uint64 {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.evicted
}

// reset clears a timer because the condition stopped holding.
func (st *stabilizer) reset(key ruleSigKey) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.timers, key)
}

// held reports the continuous-hold duration currently accrued.
func (st *stabilizer) held(key ruleSigKey, now time.Time) time.Duration {
	st.mu.Lock()
	defer st.mu.Unlock()
	t, ok := st.timers[key]
	if !ok || !t.running {
		return 0
	}
	return t.accrued + now.Sub(t.since)
}

// carryOverFingerprints applies SPEC-03 §3.7's reload rule: a rule that
// survived with a byte-identical match+for keeps every one of its timers (and
// the elapsed window of each); a rule whose predicate changed resets; a removed
// rule drops its timers. It returns the names of rules whose timers were reset,
// so the caller can record rule_state_reset.
//
// The map is keyed by rule NAME, not by (rule, sig): §3.7 keys the state by
// rule name and carries the timers of that rule across, while each timer keeps
// its own signature.
func (st *stabilizer) carryOverFingerprints(next map[string]string) []string {
	return st.carryOver(next)
}

func (st *stabilizer) carryOver(next map[string]string) []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	var reset []string
	for key, t := range st.timers {
		finger, ok := next[key.rule]
		if !ok {
			delete(st.timers, key)
			continue
		}
		if finger != t.finger {
			delete(st.timers, key)
			reset = append(reset, key.rule)
		}
	}
	sort.Strings(reset)
	return dedupe(reset)
}

func dedupe(in []string) []string {
	if len(in) == 0 {
		return in
	}
	out := in[:1]
	for _, s := range in[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}

// cooldownTable is the second, independent limiter of §3.5/§3.8. It is bounded
// for the same reason the stabilizer is: its key space is (rule, sig) and a
// high-cardinality signature space would otherwise retain one entry per
// signature for the life of the process.
type cooldownTable struct {
	mu       sync.Mutex
	last     map[ruleSigKey]time.Time
	evicted  uint64
	capacity int
}

func newCooldownTable() *cooldownTable {
	return &cooldownTable{last: map[ruleSigKey]time.Time{}, capacity: defaultCooldownCapacity}
}

func (c *cooldownTable) allow(key ruleSigKey, cd time.Duration, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	last, ok := c.last[key]
	if !ok {
		return true
	}
	return now.Sub(last) >= cd
}

func (c *cooldownTable) fired(key ruleSigKey, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.last[key]; !ok {
		c.evictIfFullLocked()
	}
	c.last[key] = now
}

func (c *cooldownTable) evictIfFullLocked() {
	if c.capacity <= 0 || len(c.last) < c.capacity {
		return
	}
	batch := c.capacity / evictionBatchDivisor
	if batch < 1 {
		batch = 1
	}
	type victim struct {
		key  ruleSigKey
		seen time.Time
	}
	victims := make([]victim, 0, batch)
	for k, t := range c.last {
		victims = append(victims, victim{key: k, seen: t})
	}
	sort.Slice(victims, func(i, j int) bool { return victims[i].seen.Before(victims[j].seen) })
	for i := 0; i < batch && i < len(victims); i++ {
		delete(c.last, victims[i].key)
		c.evicted++
	}
}

func (c *cooldownTable) evictions() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.evicted
}
