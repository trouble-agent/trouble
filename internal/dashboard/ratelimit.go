package dashboard

import (
	"math"
	"sync"
	"time"
)

// Rate limiting — SPEC-10 §2.8. Two token buckets: reads at read_rps (20/s)
// with burst read_burst (60), keyed by token AND client IP; writes at
// write_rps (5/s) with burst write_burst (10), keyed by token. A bucket empty
// at request time → 429 + Retry-After (integer seconds ≥1) +
// TROUBLE-DASHBOARD-012. Fairness (edge case 10): an authenticated hammering
// client cannot starve another token, and an unauthenticated flood cannot
// consume an authenticated bucket.

type bucket struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
}

// take consumes one token if available and reports the integer Retry-After
// seconds (≥1) when the bucket is empty.
func (b *bucket) take(now time.Time, rate float64, burst float64) (bool, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if rate <= 0 {
		return true, 0
	}
	if b.last.IsZero() {
		b.last = now
		b.tokens = burst
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens = math.Min(burst, b.tokens+elapsed*rate)
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := (1 - b.tokens) / rate
	ra := int(math.Ceil(wait))
	if ra < 1 {
		ra = 1
	}
	return false, ra
}

// limiter owns the bucket maps. Buckets are created lazily and reaped
// opportunistically so a long-lived daemon does not accumulate per-token
// state forever.
type limiter struct {
	readRPS, readBurst   float64
	writeRPS, writeBurst float64

	mu           sync.Mutex
	readBuckets  map[string]*bucket
	writeBuckets map[string]*bucket
	created      int
}

func newLimiter(cfg Config) *limiter {
	return &limiter{
		readRPS:      cfg.ReadRPS,
		readBurst:    float64(cfg.ReadBurst),
		writeRPS:     cfg.WriteRPS,
		writeBurst:   float64(cfg.WriteBurst),
		readBuckets:  map[string]*bucket{},
		writeBuckets: map[string]*bucket{},
	}
}

// readTake consumes one token from the token bucket and one from the client-IP
// bucket (both must refill; the longer Retry-After wins).
func (l *limiter) readTake(tokenID, ip string, now time.Time) (bool, int) {
	okT, raT := l.take(l.readBuckets, "r:"+tokenID, now, l.readRPS, l.readBurst)
	okI, raI := l.take(l.readBuckets, "i:"+ip, now, l.readRPS, l.readBurst)
	if okT && okI {
		return true, 0
	}
	if raT < raI {
		raT = raI
	}
	return false, raT
}

// writeTake consumes one token from the per-token write bucket.
func (l *limiter) writeTake(tokenID string, now time.Time) (bool, int) {
	return l.take(l.writeBuckets, "w:"+tokenID, now, l.writeRPS, l.writeBurst)
}

func (l *limiter) take(m map[string]*bucket, key string, now time.Time, rate, burst float64) (bool, int) {
	l.mu.Lock()
	b, ok := m[key]
	if !ok {
		b = &bucket{}
		m[key] = b
		l.created++
		if l.created%10000 == 0 {
			l.reap(now)
		}
	}
	l.mu.Unlock()
	return b.take(now, rate, burst)
}

// reap drops buckets idle for more than 10 minutes (keys no longer polling).
func (l *limiter) reap(now time.Time) {
	idle := 10 * time.Minute
	for _, m := range []map[string]*bucket{l.readBuckets, l.writeBuckets} {
		for k, b := range m {
			b.mu.Lock()
			if now.Sub(b.last) > idle {
				delete(m, k)
			}
			b.mu.Unlock()
		}
	}
}

// ipThrottle is the per-IP auth-failure throttle (§2.8): auth_fail_limit
// failures from one client IP inside auth_fail_window → that IP is throttled
// for the window. Once throttled, requests from the IP answer 429 + 012.
type ipThrottle struct {
	limit  int
	window time.Duration

	mu    sync.Mutex
	fails map[string][]time.Time
	block map[string]time.Time // ip → blocked until
}

func newThrottle(cfg Config) *ipThrottle {
	return &ipThrottle{
		limit:  cfg.AuthFailLimit,
		window: cfg.AuthFailWindow.Std(),
		fails:  map[string][]time.Time{},
		block:  map[string]time.Time{},
	}
}

// blocked reports whether the IP is inside its throttle window.
func (t *ipThrottle) blocked(ip string, now time.Time) bool {
	if t == nil || ip == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if until, ok := t.block[ip]; ok && now.Before(until) {
		return true
	}
	delete(t.block, ip)
	return false
}

// fail records one auth failure at now; returns true when the limit is crossed
// (the IP is now throttled).
func (t *ipThrottle) fail(ip string, now time.Time) bool {
	if t == nil || ip == "" || t.limit <= 0 {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	cut := now.Add(-t.window)
	f := t.fails[ip]
	kept := f[:0]
	for _, ts := range f {
		if ts.After(cut) {
			kept = append(kept, ts)
		}
	}
	kept = append(kept, now)
	t.fails[ip] = kept
	if len(kept) > t.limit {
		t.block[ip] = now.Add(t.window)
		delete(t.fails, ip)
		return true
	}
	return false
}

// success clears the failure history on a successful authentication.
func (t *ipThrottle) success(ip string) {
	if t == nil || ip == "" {
		return
	}
	t.mu.Lock()
	delete(t.fails, ip)
	t.mu.Unlock()
}
