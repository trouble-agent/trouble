package sentinel

import (
	"strings"
	"sync"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// quotaWindowSeconds is the pinned window length: a fixed 60s tumbling window
// aligned to boot, with the previous window retained for the verification
// inputs (§3.9).
const quotaWindowSeconds = 60

// proactiveBackoffPct is the §3.9 proactive threshold: at 95% of quota the
// server still answers 200 but sends the same rate-limit header, which is what
// turns a flood into an orderly slowdown instead of a 429 cliff.
const proactiveBackoffPct = 95

// quotaWindow is one project's window state.
type quotaWindow struct {
	index     int64 // window index since boot
	used      int
	prevIndex int64
	prevUsed  int
}

// quotaSet holds every project's window.
type quotaSet struct {
	mu   sync.Mutex
	boot time.Time
	per  map[string]*quotaWindow
}

func newQuotaSet(boot time.Time) *quotaSet {
	return &quotaSet{boot: boot, per: map[string]*quotaWindow{}}
}

// advance rolls the window if necessary and returns the current one.
func (q *quotaSet) advance(project string, now time.Time) *quotaWindow {
	idx := int64(now.Sub(q.boot) / (quotaWindowSeconds * time.Second))
	w := q.per[project]
	if w == nil {
		w = &quotaWindow{index: idx, prevIndex: idx - 1}
		q.per[project] = w
		return w
	}
	if idx != w.index {
		w.prevIndex, w.prevUsed = w.index, w.used
		w.index = idx
		w.used = 0
	}
	return w
}

// usedNow reports the events admitted in the current window.
func (q *quotaSet) usedNow(project string, now time.Time) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.advance(project, now).used
}

// windowView returns the current and previous window usage (the verification
// inputs of §3.9: a breach that dropped a sig's own events must be visible).
func (q *quotaSet) windowView(project string, now time.Time) (used, prevUsed int, elapsed int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	w := q.advance(project, now)
	elapsed = int(now.Sub(q.boot)/time.Second) % quotaWindowSeconds
	return w.used, w.prevUsed, elapsed
}

// admit decides how many of n event items the project may take.
//
// Rejected, sampled and spooled events do not consume quota: a breach may never
// be self-perpetuating (§3.9).
func (q *quotaSet) admit(project string, quotaEPM, n int, now time.Time) (allow int, decision types.RateLimitDecision) {
	q.mu.Lock()
	defer q.mu.Unlock()
	w := q.advance(project, now)
	elapsed := int(now.Sub(q.boot)/time.Second) % quotaWindowSeconds
	retryAfter := quotaWindowSeconds - elapsed
	if retryAfter < 1 {
		retryAfter = 1
	}
	header := quotaHeader(retryAfter)
	remaining := quotaEPM - w.used
	if remaining < 0 {
		remaining = 0
	}
	if n > remaining {
		// `used == quota` leaves zero allowance; `used + n > quota` means the
		// next item breaches (§6.10).
		allow = remaining
		w.used += allow
		return allow, types.RateLimitDecision{
			Allowed:     false,
			Reason:      causeQuota,
			RetryAfterS: retryAfter,
			Categories:  []string{"error"},
			Header:      header,
		}
	}
	w.used += n
	allow = n
	// Proactive backoff: 200 + the same header once usage is past 95%.
	if quotaEPM > 0 && w.used*100 >= quotaEPM*proactiveBackoffPct {
		return allow, types.RateLimitDecision{
			Allowed:     true,
			RetryAfterS: retryAfter,
			Categories:  []string{"error"},
			Header:      header,
		}
	}
	return allow, types.RateLimitDecision{}
}

// quotaHeader renders the pinned `retry:categories:scope:reason:namespaces` form
// with an empty namespace list (all categories), §3.9.
func quotaHeader(retryAfter int) string {
	return itoa(retryAfter) + ":error:project:quota_epm:"
}

// diskBudgetHeader is the §3.9 global-disk-budget header.
const diskBudgetHeader = "300:error:global:disk_budget:"

// ipHeader is the §3.9 per-IP header.
const ipHeader = "1:error:ip:overloaded:"

// overloadHeader is the §3.9 concurrency-cap header.
const overloadHeader = "1:error:global:overloaded:"

// Breaker and kill-switch header shapes, pinned for reuse by the ladder
// (SPEC-05 emits them; sentinel owns the shape so one string is used everywhere).
const (
	breakerHeader    = "120:error:project:breaker:"
	killSwitchHeader = "3600:error:global:kill_switch:"
)

// parseHeaderRetry extracts the retry-after seconds from a header string.
func parseHeaderRetry(h string) int {
	if i := strings.IndexByte(h, ':'); i > 0 {
		if n := atoiSafe(h[:i]); n > 0 {
			return n
		}
	}
	return 1
}

// diskUsage tracks the disk bytes attributed to sentinel (ledger + spool),
// sampled every 60s (§3.9). It is refreshed by the budget sampler goroutine and
// read on the request path, so the request path never stats the filesystem.
type diskUsage struct {
	mu      sync.Mutex
	bytes   int64
	budget  int64
	over    bool
	sampled time.Time
}

func (d *diskUsage) set(bytes, budget int64, now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.bytes = bytes
	if budget > 0 {
		d.budget = budget
	}
	d.over = d.budget > 0 && bytes > d.budget
	d.sampled = now
}

func (d *diskUsage) snapshot() (bytes, budget int64, over bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.bytes, d.budget, d.over
}
