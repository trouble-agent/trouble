package hub

import (
	"container/list"
	"context"
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// The two phases the gate's claim can be in.
//
// SPEC-13 §3.4 pins the KEY (`trouble:dedup:{sig}|{norm_version}|{host_id}`) and
// the claim operation (`SET NX EX`), and §3.3 pins the property that makes the
// gate load-bearing: a crash between `Append` and `XACK` re-delivers the batch
// and "the re-delivery lands zero new records because the dedup key is already
// SET NX-claimed". For that sentence to hold, the value must say WHICH claim it
// is, because two very different claims share the key space:
//
//   - `enqueued`: the door accepted the event and it is in the stream (or about
//     to be). A sender's retry of the same idempotency key is the duplicate this
//     phase exists to absorb, and the answer is the idempotent 200 of §3.4.
//   - `appended`: the consumer has the record in the ledger. A RE-DELIVERY of
//     the same entry must append nothing, which is what §3.3 asserts.
//
// Collapsing the two would be a data-loss bug in one direction (a door claim
// mistaken for an append makes a re-delivery a no-op that never appends) and a
// duplicate-record bug in the other (an append claim mistaken for a door claim
// re-enqueues an event already in the ledger). The phase values are therefore
// part of the gate's contract, and both are exercised by dedup_test.go.
const (
	PhaseEnqueued = "enqueued"
	PhaseAppended = "appended"
)

// DedupState is what the gate knows about one key.
type DedupState int

const (
	// DedupAbsent: never claimed (in this window).
	DedupAbsent DedupState = iota
	// DedupEnqueued: accepted at the door, not yet in the ledger.
	DedupEnqueued
	// DedupAppended: the consumer has appended its records to the ledger.
	DedupAppended
)

func (s DedupState) String() string {
	switch s {
	case DedupEnqueued:
		return PhaseEnqueued
	case DedupAppended:
		return PhaseAppended
	default:
		return ""
	}
}

// DedupGate is SPEC-13 §3.4's gate: Redis `SET NX EX` with the bounded in-memory
// LRU (`hub.dedup_lru`, 65536 by default) as the degraded fallback.
//
// The gate is deliberately NOT the exactly-once mechanism: §3.4 says so outright
// ("The dedup gate is therefore not the exactly-once guarantee; the ack-after-fsync
// rule of §3.3 is"). A failover that loses a claim costs one duplicate entry;
// the gate counts it (`dedup_conflicts`) and the ledger is untouched, which is
// visible rather than silent.
type DedupGate struct {
	cfg RedisConfig
	c   *Client

	mu             sync.Mutex
	fallback       *lruPhases
	degraded       bool
	degradedCount  int64
	restoredKeys   int64
	hits           int64
	misses         int64
	conflicts      int64
	lastDegradeErr string

	// restored is the ledger's idempotency index as the gate holds it:
	// `Restore` REPLACES it at every (re)wire with the keys the ledger's tail
	// holds (SPEC-13 §2.1.1 rule 5a). It is what makes the re-warm a behaviour
	// rather than a count: a key in this set was already appended, so a claim on
	// it is a replay.
	restored map[string]struct{}
	// trustUntil is the end of the `server.redis.failover_grace` window opened by
	// the last Restore. Until it passes, the keys in `restored` are TRUSTED (the
	// gate answers from the index without a Redis round trip); afterwards the
	// gate claims through Redis again and the ledger's own idempotency check is
	// the arbiter of §3.4's priced duplicate.
	trustUntil time.Time
	// grace is the resolved window length (RedisConfig.FailoverGrace).
	grace time.Duration

	now func() time.Time
}

// NewDedupGate builds the gate over a client. A nil client is legal and means
// "LRU only": that is the state the standalone profile and the tests' degraded
// cases run in, and it is reported as `dedup_window="lru"` rather than being
// hidden.
func NewDedupGate(cfg RedisConfig, c *Client) *DedupGate {
	cfg = cfg.WithDefaults()
	return &DedupGate{
		cfg:      cfg,
		c:        c,
		fallback: newLRUPhases(cfg.DedupLRU),
		restored: map[string]struct{}{},
		grace:    cfg.FailoverGrace,
		now:      time.Now,
	}
}

// trusted reports whether key is inside the failover_grace window opened by the
// last Restore (SPEC-13 §2.1.1 rule 5a): the ledger's idempotency index says the
// key was already appended, and the window is still open, so the gate answers
// from the index instead of asking a Redis that a failover may have emptied.
func (g *DedupGate) trusted(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.trustUntil.IsZero() || !g.now().Before(g.trustUntil) {
		return false
	}
	_, ok := g.restored[key]
	return ok
}

// Restored reports the size of the ledger index the gate holds and whether the
// failover_grace window that trusts it is still open. It is what /health.json
// prints as `redis.dedup_restored` (and what dedup.state calls `restored_keys`),
// so a gate that was re-warmed is distinguishable from one that was not.
func (g *DedupGate) Restored() (keys int64, windowOpen bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.trustUntil.IsZero() && g.now().Before(g.trustUntil) {
		windowOpen = true
	}
	return g.restoredKeys, windowOpen
}

// Dedup is SPEC-13 §2.3's function: the `SET NX EX` claim of §3.4 on the key
// derived from `sig + "|" + norm_version + "|" + host_id`. Fresh means the event
// is new; present means it is a replay.
func Dedup(ctx context.Context, c *Client, sig types.Sig, normVersion int, hostID string) (fresh bool, err error) {
	if c == nil {
		return false, errf(types.CodeHub006, ReasonDedupGate, "no dedup gate available")
	}
	if normVersion == 0 {
		normVersion = types.NormVersionV1
	}
	key := DedupKey(c.cfg.DedupPfx, sig.String(), normVersion, hostID)
	return c.s.SetNX(ctx, key, PhaseEnqueued, c.cfg.DedupTTL)
}

// Claim takes the door's claim on a key (phase `enqueued`).
//
// A Redis failure is TROUBLE-HUB-006 (class transient): the gate falls back to
// the bounded LRU for the window, the degradation is recorded once per window,
// and `dedup_window` on the health surface says "lru" so a smaller window is
// never mistaken for the full 24h one (SPEC-13 §3.4).
//
// A key the ledger's idempotency index restored — inside the
// `server.redis.failover_grace` window a (re)wire opens (SPEC-13 §2.1.1 rule
// 5a) — is answered `present` WITHOUT claiming in Redis: the key was already
// appended, so the claim is a replay, and the index is the one piece of
// evidence that survived the failover that emptied Redis. After the window the
// trust is gone and this is the plain `SET NX` of §3.4 again, which is what
// makes a lost claim cost the one duplicate §3.4 prices.
func (g *DedupGate) Claim(ctx context.Context, key string) (bool, error) {
	g.mu.Lock()
	g.misses++
	g.mu.Unlock()
	if g.trusted(key) {
		return false, nil
	}
	if g.c != nil {
		fresh, err := g.c.s.SetNX(ctx, key, PhaseEnqueued, g.cfg.DedupTTL)
		if err == nil {
			g.markHealthy()
			g.fallback.put(key, PhaseEnqueued)
			return fresh, nil
		}
		g.markDegraded(err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.fallback.put(key, PhaseEnqueued), nil
}

// MarkAppended upgrades a claim to phase `appended`. It is called AFTER the
// ledger's Append returned (i.e. after the group-commit fsync) and BEFORE the
// entry is XACKed, so the re-delivery window is covered by the claim and the
// claim is never ahead of durability.
func (g *DedupGate) MarkAppended(ctx context.Context, key string) {
	g.fallback.put(key, PhaseAppended)
	if g.c == nil || g.degraded {
		return
	}
	_ = g.markAppendedRemote(ctx, key)
}

// markAppendedRemote writes the `appended` phase into Redis.
//
// The happy path is `SET … XX`: upgrade the door's claim, and never resurrect a
// claim the TTL already released. When there is no claim to upgrade (the
// failover case of §3.4, where the replication loss took it), the entry is
// re-claimed in the `appended` phase with `SET … NX`, because the alternative
// leaves the key absent forever: every subsequent re-delivery would append
// again, and the spec prices that case as exactly ONE duplicate. Re-claiming is
// also what "a replay older than 24h re-enters" means once the replay has been
// appended: it re-entered, and the gate now says so.
func (g *DedupGate) markAppendedRemote(ctx context.Context, key string) error {
	ok, err := g.c.s.SetXX(ctx, key, PhaseAppended, g.cfg.DedupTTL)
	if err != nil {
		g.markDegraded(err)
		return err
	}
	if ok {
		return nil
	}
	fresh, err := g.c.s.SetNX(ctx, key, PhaseAppended, g.cfg.DedupTTL)
	if err != nil {
		g.markDegraded(err)
		return err
	}
	if !fresh {
		// Something claimed the key between the two writes (the door of another
		// request, or a racing consumer sharing the state root's identity): force
		// the phase so a re-delivery is a skip rather than a duplicate.
		_, err = g.c.s.SetXX(ctx, key, PhaseAppended, g.cfg.DedupTTL)
		if err != nil {
			g.markDegraded(err)
		}
	}
	return err
}

// State reports what the gate knows about a key (phase-aware, per the constants
// above). A Redis failure degrades to the LRU and says so.
//
// A key inside the failover_grace window opened by the last Restore (SPEC-13
// §2.1.1 rule 5a) answers `appended`: the ledger's idempotency index is the
// evidence that it is already in the record, and a Redis that lost its dataset
// would answer "absent" for it — which is exactly the answer that turns a
// crash-between-Append-and-XACK re-delivery into a second ledger record.
func (g *DedupGate) State(ctx context.Context, key string) (DedupState, error) {
	if g.trusted(key) {
		return DedupAppended, nil
	}
	// While the window is degraded the LRU is authoritative: asking Redis would
	// answer "absent" for every key the fallback has been serving, and a missing
	// answer must never be mistaken for "never seen" (that is the duplicate the
	// gate exists to prevent).
	if g.Degraded() {
		if phase, ok := g.fallback.get(key); ok {
			if phase == PhaseAppended {
				return DedupAppended, nil
			}
			return DedupEnqueued, nil
		}
		return DedupAbsent, nil
	}
	if g.c != nil {
		v, ok, err := g.c.s.Get(ctx, key)
		if err == nil {
			g.markHealthy()
			if !ok {
				return DedupAbsent, nil
			}
			if v == PhaseAppended {
				return DedupAppended, nil
			}
			return DedupEnqueued, nil
		}
		g.markDegraded(err)
	}
	if phase, ok := g.fallback.get(key); ok {
		if phase == PhaseAppended {
			return DedupAppended, nil
		}
		return DedupEnqueued, nil
	}
	return DedupAbsent, nil
}

// Release drops a door claim whose stream write failed, so the sender's retry is
// a fresh acceptance instead of an idempotent 200 for an event that never
// reached the queue. SPEC-13 §6.1 states the requirement ("a lost XADD costs
// nothing") and this is the mechanism that makes it true.
func (g *DedupGate) Release(ctx context.Context, key string) {
	g.fallback.remove(key)
	if g.c == nil || g.degraded {
		return
	}
	if _, err := g.c.s.Del(ctx, key); err != nil {
		g.markDegraded(err)
	}
}

// Conflict records a duplicate that the ledger accepted although the gate's
// claim was lost (SPEC-13 §3.4's failover case). It is counted, never hidden:
// the ledger is the record and a duplicate there is a fact about the tier.
func (g *DedupGate) Conflict() {
	g.mu.Lock()
	g.conflicts++
	g.mu.Unlock()
}

// Counters returns the gate's counters (hits, misses, conflicts, degraded windows).
func (g *DedupGate) Counters() (hits, misses, conflicts, degradedWindows int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.hits, g.misses, g.conflicts, g.degradedCount
}

// Hit records an answered-from-the-gate duplicate (the door's and the consumer's
// dedup hits).
func (g *DedupGate) Hit() {
	g.mu.Lock()
	g.hits++
	g.mu.Unlock()
}

// Window reports which key space is currently authoritative: "redis" (the 24h
// TTL) or "lru" (degraded, the smaller window). It is what /health.json prints
// as `dedup_window` (SPEC-13 §3.4).
func (g *DedupGate) Window() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.degraded || g.c == nil {
		return "lru"
	}
	return "redis"
}

// Degraded reports whether the current window is running on the LRU.
func (g *DedupGate) Degraded() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.degraded || g.c == nil
}

// DegradeError is the last error that pushed the gate onto the LRU (one string,
// never a secret; the Redis error text is a classification, not a credential).
func (g *DedupGate) DegradeError() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lastDegradeErr
}

// Restore seeds the LRU at boot (SPEC-13 §3.1 `dedup.state`, "boot-time restore
// of the fallback LRU"). The source of the keys is the ledger's own identity
// fields; the composition root supplies them, and a boot with no source reports
// restored_keys=0 truthfully rather than pretending the window is warm.
//
// It is also the re-warm of SPEC-13 §2.1.1 rule 5a, and that is what gives
// `server.redis.failover_grace` its reader: the call REPLACES the gate's index
// with the keys this (re)wire read out of the ledger and OPENS the trust window,
// so from here until the window closes a claim or a re-delivery check on one of
// those keys is answered from the index instead of from a Redis that a failover
// may have emptied. Replace-not-accumulate is deliberate: the ledger's tail as
// of THIS (re)wire is the evidence in force, which is also what bounds the
// structure — the walk that feeds it is capped at `hub.dedup_lru` keys.
func (g *DedupGate) Restore(keys []string) {
	g.mu.Lock()
	if g.restored == nil {
		g.restored = map[string]struct{}{}
	}
	next := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		if k == "" {
			continue
		}
		next[k] = struct{}{}
	}
	g.restored = next
	g.restoredKeys = int64(len(next))
	if g.grace > 0 {
		g.trustUntil = g.now().Add(g.grace)
	}
	g.mu.Unlock()
	// The LRU mirror is written outside the gate's own lock (its `put` takes the
	// LRU's): the two structures are independent, and the mirror is what serves
	// the gate while a Redis window is DEGRADED (that window has no TTL of its
	// own, so it keeps the union of what has been claimed and restored).
	for _, k := range keys {
		if k == "" {
			continue
		}
		g.fallback.put(k, PhaseAppended)
	}
}

// dedupStateFile is the JSON of <state_root>/hub/dedup.state (SPEC-13 §3.1).
type dedupStateFile struct {
	LRUSize         int   `json:"lru_size"`
	RestoredKeys    int64 `json:"restored_keys"`
	DegradedWindows int64 `json:"degraded_windows"`
}

// SaveState writes dedup.state (0600).
func (g *DedupGate) SaveState(stateRoot string) error {
	if stateRoot == "" {
		return nil
	}
	if err := EnsureStateDirs(stateRoot); err != nil {
		return err
	}
	g.mu.Lock()
	st := dedupStateFile{
		LRUSize:         g.fallback.len(),
		RestoredKeys:    g.restoredKeys,
		DegradedWindows: g.degradedCount,
	}
	g.mu.Unlock()
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	path := DedupStatePath(stateRoot)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return errWrap(types.CodeHub006, ReasonDedupGate, "cannot write dedup state", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return errWrap(types.CodeHub006, ReasonDedupGate, "cannot publish dedup state", err)
	}
	return nil
}

// LoadState reads dedup.state (a missing or torn file is a zero state).
func (g *DedupGate) LoadState(stateRoot string) (dedupStateFile, error) {
	b, err := os.ReadFile(DedupStatePath(stateRoot))
	if err != nil {
		if os.IsNotExist(err) {
			return dedupStateFile{}, nil
		}
		return dedupStateFile{}, errWrap(types.CodeHub006, ReasonDedupGate, "cannot read dedup state", err)
	}
	var st dedupStateFile
	if err := json.Unmarshal(b, &st); err != nil {
		return dedupStateFile{}, nil
	}
	g.mu.Lock()
	g.restoredKeys = st.RestoredKeys
	g.degradedCount = st.DegradedWindows
	g.mu.Unlock()
	return st, nil
}

// LRUSize is the current number of remembered keys.
func (g *DedupGate) LRUSize() int { return g.fallback.len() }

func (g *DedupGate) markHealthy() {
	g.mu.Lock()
	g.degraded = false
	g.lastDegradeErr = ""
	g.mu.Unlock()
}

// markDegraded enters (or stays in) a degraded window. One counter increment per
// window: SPEC-13 §3.4's "records TROUBLE-HUB-006 once per degradation window".
func (g *DedupGate) markDegraded(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.degraded {
		g.degradedCount++
	}
	g.degraded = true
	if err != nil {
		g.lastDegradeErr = err.Error()
	}
}

// ---- bounded LRU of phases ----

type lruPhases struct {
	cap   int
	mu    sync.Mutex
	items map[string]*list.Element
	order *list.List
}

type lruEntry struct {
	key   string
	phase string
}

func newLRUPhases(capacity int) *lruPhases {
	if capacity <= 0 {
		capacity = DefaultDedupLRU
	}
	return &lruPhases{cap: capacity, items: map[string]*list.Element{}, order: list.New()}
}

// put stores the phase and returns true when the key was not already present
// with a phase that dominates (appended dominates enqueued). The return value is
// the "fresh" answer of the degraded window.
func (l *lruPhases) put(key, phase string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if el, ok := l.items[key]; ok {
		e := el.Value.(*lruEntry)
		if phase == PhaseAppended || e.phase == PhaseAppended {
			e.phase = PhaseAppended
		}
		l.order.MoveToFront(el)
		return false
	}
	l.items[key] = l.order.PushFront(&lruEntry{key: key, phase: phase})
	for l.order.Len() > l.cap {
		back := l.order.Back()
		if back == nil {
			break
		}
		l.order.Remove(back)
		delete(l.items, back.Value.(*lruEntry).key)
	}
	return true
}

func (l *lruPhases) get(key string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	el, ok := l.items[key]
	if !ok {
		return "", false
	}
	l.order.MoveToFront(el)
	return el.Value.(*lruEntry).phase, true
}

func (l *lruPhases) remove(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if el, ok := l.items[key]; ok {
		l.order.Remove(el)
		delete(l.items, key)
	}
}

func (l *lruPhases) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.order.Len()
}
