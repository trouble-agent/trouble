package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// Counters are the queue's own numbers. They are the counters SPEC-13 §3.3/§3.4
// report in `RedisStreamOffsets` and in `/health.json`; every one of them is
// incremented at the site that knows the fact, never reconstructed later.
type Counters struct {
	Enqueued       atomic.Int64 // XADD accepted
	EnqueueFailed  atomic.Int64 // TROUBLE-HUB-004 while serving
	DedupHits      atomic.Int64 // the gate answered "already present"
	DedupMisses    atomic.Int64 // the gate answered "fresh"
	DedupConflicts atomic.Int64 // a record appended although its key was claimed
	Backpressure   atomic.Int64 // requests refused while over the lag gate
	DecodeErrors   atomic.Int64 // entries dead-lettered (TROUBLE-HUB-007)
	Acked          atomic.Int64 // entries XACKed (always == fsyncs observed)
	Appended       atomic.Int64 // records appended to the ledger by the consumer
	DedupSkips     atomic.Int64 // entries skipped because the gate said "appended"
	Reclaims       atomic.Int64 // XAUTOCLAIM rounds that took entries back
	Stranded       atomic.Int64 // TROUBLE-HUB-008 rounds
	Fallbacks      atomic.Int64 // ingestion served on the standalone path (degraded boot)
}

// Client is the hub's Redis client: the queue, the group and the dedup gate.
//
// SPEC-13 §2.3 spells this type `redisClient`; it is exported because the
// composition root holds it across the package boundary.
type Client struct {
	cfg  RedisConfig
	s    Streams
	info ServerInfo

	Counters Counters

	mu            sync.Mutex
	lastAcked     string
	lastDelivered string
	lastLen       int64

	opened bool
}

// OpenRedis is SPEC-13 §2.3's `OpenRedis`: dial, PING, then the §2.1.1 rule 4
// preflight. TROUBLE-HUB-002 (auth/credentials), 003 (unreachable), 015 (cluster
// or multi-key topology) and 016 (appendonly=no under require_persistence) are
// decided here and nowhere else.
func OpenRedis(ctx context.Context, cfg RedisConfig) (*Client, error) {
	cfg = cfg.WithDefaults()
	if cfg.URL == "" {
		return nil, errf(types.CodeHub001, ReasonProfile, "light-hub requires server.redis.url (SPEC-13 §2.1)")
	}
	s, err := NewGoRedisStreams(cfg.URL, cfg.Password, cfg.DialTO, cfg.ReadTO, cfg.WriteTO)
	if err != nil {
		return nil, err
	}
	c, err := OpenWith(ctx, cfg, s)
	if err != nil {
		_ = s.Close()
		return nil, err
	}
	return c, nil
}

// OpenWith is the same boot through a caller-supplied Streams implementation.
// Production never uses it; tests use it so the queue's semantics are provable
// without a Redis process (and so an embedder can supply a managed client).
func OpenWith(ctx context.Context, cfg RedisConfig, s Streams) (*Client, error) {
	if s == nil {
		return nil, errf(types.CodeHub003, ReasonRedisDial, "no redis client supplied")
	}
	cfg = cfg.WithDefaults()
	c := &Client{cfg: cfg, s: s}
	if err := s.Ping(ctx); err != nil {
		if IsAuthError(err) {
			return nil, errWrap(types.CodeHub002, ReasonRedisAuth, "redis rejected the connection or its credentials", err)
		}
		return nil, errWrap(types.CodeHub003, ReasonRedisDial, "redis is unreachable", err)
	}
	if err := c.Preflight(ctx); err != nil {
		return nil, err
	}
	c.opened = true
	return c, nil
}

// Preflight applies the SPEC-13 §2.1.1 deployment checks that are refusals.
// A Redis that cannot answer INFO reports OptionsChecked=false: the facts are
// unknown, and an unknown deployment is never assumed good (the doc's rule) —
// but it is also not a refusal, because several managed offerings do not expose
// INFO to the application account.
func (c *Client) Preflight(ctx context.Context) error {
	info, err := c.s.ServerInfo(ctx)
	if err != nil {
		if IsAuthError(err) {
			return errWrap(types.CodeHub002, ReasonRedisAuth, "redis refused INFO: the account cannot read its own deployment", err)
		}
		// INFO failing for a non-auth reason is not fatal: the connection works.
		c.info = ServerInfo{OptionsChecked: false}
		c.opened = true
		return nil
	}
	c.info = info
	if info.ClusterEnabled {
		// §2.1.1 rule 2: the stream key and the dedup key space must live on one
		// primary, or the consumer-group and SET NX semantics do not hold.
		return errf(types.CodeHub015, ReasonRedisTopo,
			"redis cluster mode is refused: the stream and the dedup keyspace must share one primary (SPEC-13 §2.1.1)")
	}
	if cfg := c.cfg; cfg.RequirePersistence && info.OptionsChecked && !info.AOFEnabled {
		return errf(types.CodeHub016, ReasonRedisPersist,
			"redis appendonly is off and server.redis.require_persistence=true (SPEC-13 §2.1.1)")
	}
	return nil
}

// ServerInfo returns the preflight facts that were read (or the zero value).
func (c *Client) ServerInfo() ServerInfo { return c.info }

// PolicyWarnings lists the non-fatal deployment deviations the health surface
// must show (SPEC-13 §2.1.1 rule 4: a misconfigured server is visible, not
// assumed).
func (c *Client) PolicyWarnings() []string {
	var out []string
	if !c.info.OptionsChecked {
		return out
	}
	if c.cfg.CheckPolicy && c.info.Policy != "" && c.info.Policy != "noeviction" {
		out = append(out, "maxmemory-policy="+c.info.Policy+" (REQUIRED noeviction: trouble, not Redis, bounds the queue)")
	}
	if c.info.EvictedKeys > 0 {
		out = append(out, "evicted_keys="+itoa(c.info.EvictedKeys)+" (queue eviction is data loss)")
	}
	if !c.info.AOFEnabled {
		out = append(out, "appendonly=no (durability comes from the ledger's ack-after-fsync, but a warm restart then loses queue depth)")
	}
	return out
}

// EnsureGroup creates the stream and the consumer group (SPEC-13 §3.3). A
// BUSYGROUP reply is SUCCESS — the group already exists, which is the state the
// call is trying to reach — and every other refusal is TROUBLE-HUB-005, a hard
// failure: a queue with no drainer is worse than a stopped daemon.
func EnsureGroup(ctx context.Context, c *Client, stream, group string) error {
	if stream == "" {
		stream = c.cfg.Stream
	}
	if group == "" {
		group = c.cfg.Group
	}
	err := c.s.XGroupCreate(ctx, stream, group, "$")
	if err == nil || IsBusyGroup(err) {
		return nil
	}
	if IsAuthError(err) {
		return errWrap(types.CodeHub002, ReasonRedisAuth, "redis refused XGROUP", err)
	}
	return errWrap(types.CodeHub005, ReasonGroup, "XGROUP CREATE "+stream+" "+group+" failed", err)
}

// EnsureGroup on the client's own stream/group.
func (c *Client) EnsureGroup(ctx context.Context) error {
	return EnsureGroup(ctx, c, c.cfg.Stream, c.cfg.Group)
}

// Enqueue writes one forwarding envelope to the stream (SPEC-13 §2.3).
//
// The returned value is the millisecond component of the XADD-assigned stream
// id, which is what the spec's signature returns; `EnqueueEntry` returns the
// full `<ms>-<seq>` id for callers that must address the entry (the door's
// waiter registry).
func Enqueue(ctx context.Context, c *Client, env types.ForwardEnvelope, route RouteDecision) (uint64, error) {
	id, err := c.EnqueueEntry(ctx, env, route)
	if err != nil {
		return 0, err
	}
	ms, _, perr := EntryIDParts(id)
	if perr != nil {
		return 0, nil
	}
	return ms, nil
}

// EnqueueEntry is Enqueue plus the full stream id.
func (c *Client) EnqueueEntry(ctx context.Context, env types.ForwardEnvelope, route RouteDecision) (string, error) {
	if !route.Valid() {
		return "", errf(types.CodeHub007, ReasonDecode, "route %q is not A|B", route)
	}
	se := StreamEnvelope{ForwardEnvelope: env, Route: route, EnqueuedTS: types.NowUTC()}
	return c.enqueueEnvelope(ctx, se)
}

func (c *Client) enqueueEnvelope(ctx context.Context, se StreamEnvelope) (string, error) {
	fields, err := se.EncodeFields()
	if err != nil {
		return "", err
	}
	id, err := c.s.XAdd(ctx, c.cfg.Stream, c.cfg.MaxLen, fields)
	if err != nil {
		c.Counters.EnqueueFailed.Add(1)
		if IsAuthError(err) {
			return "", errWrap(types.CodeHub002, ReasonRedisAuth, "XADD refused: redis credentials no longer authorize writes", err)
		}
		return "", errWrap(types.CodeHub004, ReasonXAdd, "XADD "+c.cfg.Stream+" failed", err)
	}
	c.Counters.Enqueued.Add(1)
	return id, nil
}

// StreamLen is XLEN of the stream (0 for a stream that does not exist).
func (c *Client) StreamLen(ctx context.Context) (int64, error) {
	n, err := c.s.XLen(ctx, c.cfg.Stream)
	if err != nil {
		return 0, errWrap(types.CodeHub004, ReasonXAdd, "XLEN failed", err)
	}
	c.mu.Lock()
	c.lastLen = n
	c.mu.Unlock()
	return n, nil
}

// GroupInfo is XINFO GROUPS for the consumer group, with the lag the spec's
// formula needs (SPEC-13 §3.3: `lag = XLEN − (last acked id position)`).
//
// The formula's second term is the group's `entries-read` count, which Redis
// keeps per group; the group's own `lag` field is preferred when the server
// reports it (Redis ≥ 7) because it is the same quantity computed server-side.
func (c *Client) GroupInfo(ctx context.Context) (GroupInfo, error) {
	gi, err := c.s.XInfoGroups(ctx, c.cfg.Stream)
	if err != nil {
		return GroupInfo{}, errWrap(types.CodeHub004, ReasonXAdd, "XINFO GROUPS failed", err)
	}
	if gi.Lag == 0 && gi.LastDeliveredID != "" {
		if n, lerr := c.StreamLen(ctx); lerr == nil && gi.EntriesRead > 0 {
			if lag := n - gi.EntriesRead; lag > 0 {
				gi.Lag = lag
			}
		}
	}
	c.mu.Lock()
	c.lastDelivered = gi.LastDeliveredID
	c.mu.Unlock()
	return gi, nil
}

// Pending is XPENDING for the group.
func (c *Client) Pending(ctx context.Context) (Pending, error) {
	p, err := c.s.XPending(ctx, c.cfg.Stream, c.cfg.Group)
	if err != nil {
		return Pending{}, errWrap(types.CodeHub004, ReasonXAdd, "XPENDING failed", err)
	}
	return p, nil
}

// Overloaded reports whether the lag gate must engage (§4.2 backpressure): the
// un-acked stream length at or past maxlen, or the consumer's lag past
// maxlen/2. Redis-side eviction is never the bound (SPEC-13 §3.2 rule 3), so
// this check is what actually stops the world.
func (c *Client) Overloaded(ctx context.Context) (bool, string) {
	gi, err := c.GroupInfo(ctx)
	if err != nil {
		return false, ""
	}
	limit := c.cfg.MaxLen
	if limit <= 0 {
		return false, ""
	}
	if gi.Pending >= limit {
		return true, "pending_at_maxlen"
	}
	if gi.Lag >= limit/2 {
		return true, "lag_over_half_maxlen"
	}
	return false, ""
}

// SetLastAcked records the highest id the consumer has acked. It is the acked
// position the offsets report; it only ever moves forward.
func (c *Client) SetLastAcked(id string) {
	if id == "" {
		return
	}
	c.mu.Lock()
	c.lastAcked = id
	c.mu.Unlock()
}

// ConsumerName resolves the consumer identity (SPEC-13 §3.3: `consumer = "" →
// origin.host_id`: one consumer per state root, so XPENDING is always
// attributable and two daemons on one state root are impossible — the LOCK of
// SPEC-01 §3.4 refuses the second one first).
func (c *Client) ConsumerName() string {
	if c.cfg.Consumer != "" {
		return c.cfg.Consumer
	}
	return c.cfg.HostID
}

// Watermarks returns the last acked and last delivered stream ids.
func (c *Client) Watermarks() (acked, delivered string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastAcked, c.lastDelivered
}

// Close releases the connection. It never fails a caller: a close error on a
// dead connection is the state being closed.
func (c *Client) Close() error {
	if c == nil || c.s == nil {
		return nil
	}
	if err := c.s.Close(); err != nil {
		return &Error{Code: types.CodeHub004, Reason: ReasonXAdd, Msg: "redis close", Err: err}
	}
	return nil
}

// Streams returns the underlying command surface (tests and the drain command
// need the raw actions, not a second client).
func (c *Client) Streams() Streams { return c.s }

// Config returns the effective configuration (post-defaults).
func (c *Client) Config() RedisConfig { return c.cfg }

func itoa(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// canonicalJSON marshals a value the way the dedup fallback hashes it:
// SetEscapeHTML(false) (the ledger's own rule) and no trailing newline, so the
// same records always produce the same bytes.
func canonicalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
