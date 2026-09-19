package hub

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// Streams is the Redis command surface the hub uses.
//
// It exists so the queue's semantics are testable without a live Redis and so
// the production client (go-redis v9, SPEC-13 §8) is one implementation of a
// seam rather than an ambient dependency of every test. The surface is
// deliberately narrow: streams + consumer groups + the dedup gate's three
// commands, which is the whole of what this package may do to Redis.
type Streams interface {
	Ping(ctx context.Context) error
	// ServerInfo reads the deployment facts of SPEC-13 §2.1.1 rule 4
	// (appendonly, maxmemory-policy, evicted_keys, cluster_enabled).
	ServerInfo(ctx context.Context) (ServerInfo, error)
	XAdd(ctx context.Context, stream string, maxLen int64, values map[string]any) (string, error)
	XLen(ctx context.Context, stream string) (int64, error)
	XGroupCreate(ctx context.Context, stream, group, start string) error
	XReadGroup(ctx context.Context, req ReadRequest) ([]StreamEntry, error)
	XAck(ctx context.Context, stream, group string, ids ...string) (int64, error)
	XAutoClaim(ctx context.Context, req ClaimRequest) (ClaimResult, error)
	XPending(ctx context.Context, stream, group string) (Pending, error)
	XInfoGroups(ctx context.Context, stream string) (GroupInfo, error)
	// SetNX is the dedup gate's claim: `SET key value NX EX ttl` (§3.4).
	SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error)
	// SetXX is the gate's phase upgrade: `SET key value XX EX ttl`, which writes
	// ONLY if the claim already exists — the primitive that keeps a phase
	// transition from resurrecting an expired window.
	SetXX(ctx context.Context, key, value string, ttl time.Duration) (bool, error)
	// Get returns the value and whether the key exists.
	Get(ctx context.Context, key string) (string, bool, error)
	Del(ctx context.Context, keys ...string) (int64, error)
	Close() error
}

// ServerInfo is the preflight fact set of SPEC-13 §2.1.1 rule 4. OptionsChecked
// is false when the connection lacks INFO rights (managed Redis): the facts are
// then absent, not assumed good.
type ServerInfo struct {
	AOFEnabled     bool
	Policy         string
	EvictedKeys    int64
	ClusterEnabled bool
	OptionsChecked bool
	MaxMemory      int64
}

// StreamEntry is one delivered stream entry.
type StreamEntry struct {
	ID     string
	Fields map[string]string
	// DeliveryCount is > 1 for an entry taken back by XAUTOCLAIM (it was
	// delivered to a consumer that died before acking it).
	DeliveryCount int64
}

// ReadRequest parameterizes XREADGROUP (§3.3: one batch per call, `>` for new
// entries, a bounded BLOCK so a shutdown is not delayed by a long block).
type ReadRequest struct {
	Stream   string
	Group    string
	Consumer string
	Count    int
	Block    time.Duration
	// Start is the id to read from; ">" means new entries only.
	Start string
}

// ClaimRequest parameterizes XAUTOCLAIM (§3.3 reclaim after restart).
type ClaimRequest struct {
	Stream   string
	Group    string
	Consumer string
	MinIdle  time.Duration
	Start    string
	Count    int
}

// ClaimResult is the XAUTOCLAIM answer: the entries taken back plus the cursor
// to continue from.
type ClaimResult struct {
	Entries []StreamEntry
	Next    string
}

// Pending is the XPENDING summary of one group.
type Pending struct {
	Count     int64
	MinID     string
	MaxID     string
	Consumers int64
}

// GroupInfo is the XINFO GROUPS row of one group.
type GroupInfo struct {
	Name            string
	Consumers       int64
	Pending         int64
	LastDeliveredID string
	EntriesRead     int64
	Lag             int64
}

// IsAuthError reports whether err is a Redis authentication/authorization
// refusal. SPEC-13 §5 splits these from reachability: 002 (permanent, fix the
// URL/credentials) versus 003 (transient, start Redis), and the health surface
// distinguishes `redis_auth` from `redis_unavailable`.
func IsAuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToUpper(err.Error())
	return strings.Contains(msg, "NOAUTH") || strings.Contains(msg, "WRONGPASS") ||
		strings.Contains(msg, "AUTHENTICATION") || strings.Contains(msg, "NOPERM") ||
		strings.Contains(msg, "NO PERMISSIONS")
}

// IsBusyGroup reports whether err is Redis's BUSYGROUP reply, which SPEC-13 §3.3
// pins as SUCCESS: the group already exists, which is exactly the state
// EnsureGroup is trying to reach.
func IsBusyGroup(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "BUSYGROUP")
}

// ---- go-redis implementation ----

type goRedisStreams struct{ c *redis.Client }

// NewGoRedisStreams dials the SPEC-13 §2.1 redis URL and returns the production
// Streams implementation. The password arrives already resolved from the 0600
// EnvironmentFile (SPEC-12 §3.2) and is never logged, echoed or recorded.
func NewGoRedisStreams(rawURL, password string, dialTO, readTO, writeTO time.Duration) (Streams, error) {
	opt, err := redis.ParseURL(rawURL)
	if err != nil {
		return nil, errWrap(types.CodeHub002, ReasonRedisAuth, "redis url is not usable", err)
	}
	if password != "" {
		opt.Password = password
	}
	if dialTO > 0 {
		opt.DialTimeout = dialTO
	}
	if readTO > 0 {
		opt.ReadTimeout = readTO
	}
	if writeTO > 0 {
		opt.WriteTimeout = writeTO
	}
	opt.MaxRetries = 1
	return &goRedisStreams{c: redis.NewClient(opt)}, nil
}

func (g *goRedisStreams) Ping(ctx context.Context) error { return g.c.Ping(ctx).Err() }

func (g *goRedisStreams) ServerInfo(ctx context.Context) (ServerInfo, error) {
	txt, err := g.c.Info(ctx).Result()
	if err != nil {
		// A managed Redis may refuse INFO: the facts are then unknown, and the
		// honest answer is "not checked", never a default (SPEC-13 §2.1.1 rule 4).
		if IsAuthError(err) {
			return ServerInfo{}, err
		}
		return ServerInfo{OptionsChecked: false}, nil
	}
	info := parseRedisInfo(txt)
	return info, nil
}

// parseRedisInfo reads the four facts SPEC-13 §2.1.1 rule 4 names out of an INFO
// reply. A section that is absent stays at its zero value and OptionsChecked
// stays false, so an unreadable reply can never read as "noeviction, AOF on".
func parseRedisInfo(txt string) ServerInfo {
	info := ServerInfo{OptionsChecked: false}
	seen := map[string]bool{}
	for _, line := range strings.Split(txt, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch k {
		case "aof_enabled":
			info.AOFEnabled = v == "1"
			seen["aof"] = true
		case "maxmemory_policy":
			info.Policy = v
			seen["policy"] = true
		case "evicted_keys":
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				info.EvictedKeys = n
				seen["evicted"] = true
			}
		case "cluster_enabled":
			info.ClusterEnabled = v == "1"
			seen["cluster"] = true
		case "maxmemory":
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				info.MaxMemory = n
			}
		}
	}
	info.OptionsChecked = seen["aof"] && seen["policy"] && seen["cluster"]
	return info
}

func (g *goRedisStreams) XAdd(ctx context.Context, stream string, maxLen int64, values map[string]any) (string, error) {
	args := &redis.XAddArgs{Stream: stream, Values: values}
	if maxLen > 0 {
		// Approximate trim is a memory ceiling, never an ack-aware deletion
		// (SPEC-13 §3.2 rule 3): the lag gate below is what stops the world.
		args.MaxLen = maxLen
		args.Approx = true
	}
	return g.c.XAdd(ctx, args).Result()
}

func (g *goRedisStreams) XLen(ctx context.Context, stream string) (int64, error) {
	n, err := g.c.XLen(ctx, stream).Result()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	return n, err
}

func (g *goRedisStreams) XGroupCreate(ctx context.Context, stream, group, start string) error {
	return g.c.XGroupCreateMkStream(ctx, stream, group, start).Err()
}

func (g *goRedisStreams) XReadGroup(ctx context.Context, req ReadRequest) ([]StreamEntry, error) {
	start := req.Start
	if start == "" {
		start = ">"
	}
	streams, err := g.c.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    req.Group,
		Consumer: req.Consumer,
		Streams:  []string{req.Stream, start},
		Count:    int64(req.Count),
		Block:    req.Block,
	}).Result()
	if errors.Is(err, redis.Nil) {
		// A BLOCK expiry is "no entries", not an error (§4.3 keeps the daemon
		// running through a quiet queue).
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []StreamEntry
	for _, s := range streams {
		for _, m := range s.Messages {
			out = append(out, toStreamEntry(m))
		}
	}
	return out, nil
}

func (g *goRedisStreams) XAck(ctx context.Context, stream, group string, ids ...string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	return g.c.XAck(ctx, stream, group, ids...).Result()
}

func (g *goRedisStreams) XAutoClaim(ctx context.Context, req ClaimRequest) (ClaimResult, error) {
	start := req.Start
	if start == "" {
		start = "0-0"
	}
	msgs, next, err := g.c.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream:   req.Stream,
		Group:    req.Group,
		Consumer: req.Consumer,
		MinIdle:  req.MinIdle,
		Start:    start,
		Count:    int64(req.Count),
	}).Result()
	if errors.Is(err, redis.Nil) {
		return ClaimResult{Next: "0-0"}, nil
	}
	if err != nil {
		return ClaimResult{}, err
	}
	out := ClaimResult{Next: next}
	for _, m := range msgs {
		e := toStreamEntry(m)
		// A reclaimed entry was delivered at least once before (§3.3), which is
		// what makes it a RE-delivery for the consumer's ordering decision.
		e.DeliveryCount = 2
		out.Entries = append(out.Entries, e)
	}
	return out, nil
}

func (g *goRedisStreams) XPending(ctx context.Context, stream, group string) (Pending, error) {
	p, err := g.c.XPending(ctx, stream, group).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return Pending{}, nil
		}
		return Pending{}, err
	}
	return Pending{Count: p.Count, MinID: p.Lower, MaxID: p.Higher, Consumers: int64(len(p.Consumers))}, nil
}

func (g *goRedisStreams) XInfoGroups(ctx context.Context, stream string) (GroupInfo, error) {
	groups, err := g.c.XInfoGroups(ctx, stream).Result()
	if err != nil {
		return GroupInfo{}, err
	}
	for _, grp := range groups {
		return GroupInfo{
			Name:            grp.Name,
			Consumers:       int64(grp.Consumers),
			Pending:         grp.Pending,
			LastDeliveredID: grp.LastDeliveredID,
			EntriesRead:     grp.EntriesRead,
			Lag:             grp.Lag,
		}, nil
	}
	return GroupInfo{}, nil
}

func (g *goRedisStreams) SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	return g.c.SetNX(ctx, key, value, ttl).Result()
}

func (g *goRedisStreams) SetXX(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	return g.c.SetXX(ctx, key, value, ttl).Result()
}

func (g *goRedisStreams) Get(ctx context.Context, key string) (string, bool, error) {
	v, err := g.c.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

func (g *goRedisStreams) Del(ctx context.Context, keys ...string) (int64, error) {
	if len(keys) == 0 {
		return 0, nil
	}
	return g.c.Del(ctx, keys...).Result()
}

func (g *goRedisStreams) Close() error { return g.c.Close() }

func toStreamEntry(m redis.XMessage) StreamEntry {
	fields := make(map[string]string, len(m.Values))
	for k, v := range m.Values {
		switch t := v.(type) {
		case string:
			fields[k] = t
		case []byte:
			fields[k] = string(t)
		default:
			fields[k] = fmt.Sprint(v)
		}
	}
	return StreamEntry{ID: m.ID, Fields: fields}
}
