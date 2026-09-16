package issues

import (
	"sort"
	"sync"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// capCounters is the §3.8 anti-spray state. Windows are fixed; the counters are
// rebuilt from the ledger at boot inside the window, so a restart never lifts a
// cap. Counters are kept per driver and globally.
type capCounters struct {
	mu  sync.Mutex
	cfg types.IssueCaps
	now func() time.Time

	// one timestamp per counted operation, pruned by window on every read.
	sigCreates       map[string][]time.Time
	sigComments      map[string][]time.Time
	projectCreatesH  map[string][]time.Time
	projectCreatesD  map[string][]time.Time
	projectCommentsH map[string][]time.Time
	globalCreatesH   []time.Time
	globalCreatesD   []time.Time
	globalCommentsH  []time.Time

	lastComment map[string]time.Time // sig → last accepted fold comment
	suppressed  int
}

func newCapCounters(cfg types.IssueCaps, now func() time.Time) *capCounters {
	return &capCounters{
		cfg:              cfg,
		now:              now,
		sigCreates:       map[string][]time.Time{},
		sigComments:      map[string][]time.Time{},
		projectCreatesH:  map[string][]time.Time{},
		projectCreatesD:  map[string][]time.Time{},
		projectCommentsH: map[string][]time.Time{},
		lastComment:      map[string]time.Time{},
	}
}

func within(ts []time.Time, now time.Time, w time.Duration) []time.Time {
	if w <= 0 {
		return ts
	}
	cut := now.Add(-w)
	out := ts[:0:0]
	for _, t := range ts {
		if !t.Before(cut) {
			out = append(out, t)
		}
	}
	return out
}

func (c *capCounters) sigWindow() time.Duration {
	if d := c.cfg.PerSigWindow.Std(); d > 0 {
		return d
	}
	return 24 * time.Hour
}

// allowCreate reports whether a new issue may be filed for (sig, project). The
// returned token names the violated cap, or "" when the create is allowed.
func (c *capCounters) allowCreate(sig, project string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.sigCreates[sig] = within(c.sigCreates[sig], now, c.sigWindow())
	c.projectCreatesH[project] = within(c.projectCreatesH[project], now, time.Hour)
	c.projectCreatesD[project] = within(c.projectCreatesD[project], now, 24*time.Hour)
	c.globalCreatesH = within(c.globalCreatesH, now, time.Hour)
	c.globalCreatesD = within(c.globalCreatesD, now, 24*time.Hour)

	if c.cfg.PerSigCreates > 0 && len(c.sigCreates[sig]) >= c.cfg.PerSigCreates {
		return "per_sig_creates", false
	}
	if c.cfg.PerProjectCreatesH > 0 && len(c.projectCreatesH[project]) >= c.cfg.PerProjectCreatesH {
		return "per_project_creates_h", false
	}
	if c.cfg.PerProjectCreatesD > 0 && len(c.projectCreatesD[project]) >= c.cfg.PerProjectCreatesD {
		return "per_project_creates_d", false
	}
	if c.cfg.GlobalCreatesH > 0 && len(c.globalCreatesH) >= c.cfg.GlobalCreatesH {
		return "global_creates_h", false
	}
	if c.cfg.GlobalCreatesD > 0 && len(c.globalCreatesD) >= c.cfg.GlobalCreatesD {
		return "global_creates_d", false
	}
	return "", true
}

// allowComment reports whether a fold comment may be appended for (sig,
// project). The token is "comment_min_interval", "per_sig_comments",
// "per_project_comments_h" or "global_comments_h".
func (c *capCounters) allowComment(sig, project string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if iv := c.cfg.CommentMinInterval.Std(); iv > 0 {
		if last, ok := c.lastComment[sig]; ok && now.Sub(last) < iv {
			return "comment_min_interval", false
		}
	}
	c.sigComments[sig] = within(c.sigComments[sig], now, time.Hour)
	c.projectCommentsH[project] = within(c.projectCommentsH[project], now, time.Hour)
	c.globalCommentsH = within(c.globalCommentsH, now, time.Hour)
	if c.cfg.PerSigComments > 0 && len(c.sigComments[sig]) >= c.cfg.PerSigComments {
		return "per_sig_comments", false
	}
	if c.cfg.PerProjectCommentsH > 0 && len(c.projectCommentsH[project]) >= c.cfg.PerProjectCommentsH {
		return "per_project_comments_h", false
	}
	if c.cfg.GlobalCommentsH > 0 && len(c.globalCommentsH) >= c.cfg.GlobalCommentsH {
		return "global_comments_h", false
	}
	return "", true
}

func (c *capCounters) noteCreate(sig, project string, ts time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sigCreates[sig] = append(c.sigCreates[sig], ts)
	c.projectCreatesH[project] = append(c.projectCreatesH[project], ts)
	c.projectCreatesD[project] = append(c.projectCreatesD[project], ts)
	c.globalCreatesH = append(c.globalCreatesH, ts)
	c.globalCreatesD = append(c.globalCreatesD, ts)
}

func (c *capCounters) noteComment(sig, project string, ts time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sigComments[sig] = append(c.sigComments[sig], ts)
	c.projectCommentsH[project] = append(c.projectCommentsH[project], ts)
	c.globalCommentsH = append(c.globalCommentsH, ts)
	c.lastComment[sig] = ts
}

func (c *capCounters) noteSuppressed() {
	c.mu.Lock()
	c.suppressed++
	c.mu.Unlock()
}

// projectCounts reports the project of a counted record: payload.project when the
// record carries one, else the sig's fallback project.
func (c *capCounters) rebuild(recs []types.Record) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sigCreates = map[string][]time.Time{}
	c.sigComments = map[string][]time.Time{}
	c.projectCreatesH = map[string][]time.Time{}
	c.projectCreatesD = map[string][]time.Time{}
	c.projectCommentsH = map[string][]time.Time{}
	c.globalCreatesH = nil
	c.globalCreatesD = nil
	c.globalCommentsH = nil
	c.lastComment = map[string]time.Time{}

	for _, r := range recs {
		ts, err := types.ParseUTC(r.TS)
		if err != nil {
			continue
		}
		op := strPayload(r.Payload, "op")
		sig := r.Sig
		project := strPayload(r.Payload, "project")
		switch op {
		case "ensure":
			if boolPayload(r.Payload, "created") {
				c.sigCreates[sig] = append(c.sigCreates[sig], ts)
				if project != "" {
					c.projectCreatesH[project] = append(c.projectCreatesH[project], ts)
					c.projectCreatesD[project] = append(c.projectCreatesD[project], ts)
				}
				c.globalCreatesH = append(c.globalCreatesH, ts)
				c.globalCreatesD = append(c.globalCreatesD, ts)
			}
		case "comment":
			if boolPayload(r.Payload, "commented") {
				c.sigComments[sig] = append(c.sigComments[sig], ts)
				if project != "" {
					c.projectCommentsH[project] = append(c.projectCommentsH[project], ts)
				}
				c.globalCommentsH = append(c.globalCommentsH, ts)
				if last, ok := c.lastComment[sig]; !ok || ts.After(last) {
					c.lastComment[sig] = ts
				}
			}
		}
	}
	c.suppressed = 0
}

// snapshot renders the caller-visible counter set (`trouble issues health --json`).
func (c *capCounters) snapshot(driver string) types.IssueCapState {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	out := types.IssueCapState{
		Driver:           driver,
		WindowTS:         types.FormatUTC(now),
		SigCreates:       map[string]int{},
		SigComments:      map[string]int{},
		LastCommentTS:    map[string]string{},
		ProjectCreatesH:  map[string]int{},
		ProjectCreatesD:  map[string]int{},
		ProjectCommentsH: map[string]int{},
		Suppressed:       c.suppressed,
	}
	for sig, ts := range c.sigCreates {
		if n := len(within(ts, now, c.sigWindow())); n > 0 {
			out.SigCreates[sig] = n
		}
	}
	for sig, ts := range c.sigComments {
		if n := len(within(ts, now, time.Hour)); n > 0 {
			out.SigComments[sig] = n
		}
	}
	for sig, ts := range c.lastComment {
		out.LastCommentTS[sig] = types.FormatUTC(ts)
	}
	for p, ts := range c.projectCreatesH {
		if n := len(within(ts, now, time.Hour)); n > 0 {
			out.ProjectCreatesH[p] = n
		}
	}
	for p, ts := range c.projectCreatesD {
		if n := len(within(ts, now, 24*time.Hour)); n > 0 {
			out.ProjectCreatesD[p] = n
		}
	}
	for p, ts := range c.projectCommentsH {
		if n := len(within(ts, now, time.Hour)); n > 0 {
			out.ProjectCommentsH[p] = n
		}
	}
	out.GlobalCreatesH = len(within(c.globalCreatesH, now, time.Hour))
	out.GlobalCreatesD = len(within(c.globalCreatesD, now, 24*time.Hour))
	out.GlobalCommentsH = len(within(c.globalCommentsH, now, time.Hour))
	return out
}

// sortedSigKeys is a deterministic iteration helper for tests and diagnostics.
func (c *capCounters) sortedSigKeys() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.sigCreates)+len(c.sigComments))
	seen := map[string]bool{}
	for k := range c.sigCreates {
		if !seen[k] {
			out = append(out, k)
			seen[k] = true
		}
	}
	for k := range c.sigComments {
		if !seen[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func strPayload(p map[string]any, key string) string {
	if p == nil {
		return ""
	}
	if s, ok := p[key].(string); ok {
		return s
	}
	return ""
}

func boolPayload(p map[string]any, key string) bool {
	if p == nil {
		return false
	}
	b, _ := p[key].(bool)
	return b
}

func intPayload(p map[string]any, key string) int {
	if p == nil {
		return 0
	}
	switch v := p[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}
