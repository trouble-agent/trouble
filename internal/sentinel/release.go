package sentinel

import (
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// Regression verdicts carried by `payload.regression` (§3.4).
const (
	regressionConfirmed   = "confirmed"
	regressionUnconfirmed = "unconfirmed"
)

// releaseOrder orders release strings (§3.4): dot/dash-separated numeric
// segments compare numerically, then lexically; a suffix that no rule can order
// is unorderable, and the caller marks it `regressed_unconfirmed` instead of
// claiming the release is newer.
type releaseOrder struct {
	mu   sync.Mutex
	seq  map[string]int
	next int
}

func newReleaseOrder() *releaseOrder {
	return &releaseOrder{seq: map[string]int{}}
}

// seen returns the first-observation sequence of a release, assigning one on
// first sight (the final tiebreaker of §3.4's ordering).
func (r *releaseOrder) seen(release string) int {
	if release == "" {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if n, ok := r.seq[release]; ok {
		return n
	}
	r.next++
	r.seq[release] = r.next
	return r.next
}

// compareReleases compares two releases. ok=false means "no rule can order
// these" — never a silent guess.
func compareReleases(a, b string) (int, bool) {
	if a == b {
		return 0, true
	}
	if a == "" || b == "" {
		return 0, false
	}
	as := splitRelease(a)
	bs := splitRelease(b)
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		var av, bv string
		if i < len(as) {
			av = as[i]
		}
		if i < len(bs) {
			bv = bs[i]
		}
		if av == bv {
			continue
		}
		an, aok := numericSegment(av)
		bn, bok := numericSegment(bv)
		switch {
		case aok && bok:
			if an != bn {
				if an < bn {
					return -1, true
				}
				return 1, true
			}
		case aok != bok:
			// A missing or non-numeric segment against a numeric one is
			// exactly the "no rule can order these" case (§3.4).
			return 0, false
		default:
			if av < bv {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, false
}

// splitRelease splits a release into dot/dash-separated segments.
func splitRelease(r string) []string {
	return strings.FieldsFunc(r, func(c rune) bool { return c == '.' || c == '-' })
}

// numericSegment parses a segment that is purely digits (with an optional
// leading v/V, e.g. `v2`).
func numericSegment(s string) (int64, bool) {
	t := s
	if len(t) > 0 && (t[0] == 'v' || t[0] == 'V') {
		t = t[1:]
	}
	if t == "" {
		return 0, false
	}
	for _, c := range t {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(t, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// releaseInfo is per (project, release) observation state — the AC-19 inputs.
type releaseInfo struct {
	lastEventTS string
	events      uint64
}

// releaseState is the release bookkeeping of §3.4.
type releaseState struct {
	mu        sync.Mutex
	perRel    map[string]map[string]*releaseInfo // project → release → info
	resolved  map[string]string                  // sig → last pre-resolve release
	lastCanary map[string]string                 // project → canary observation ts
	order     *releaseOrder
}

func newReleaseState() *releaseState {
	return &releaseState{
		perRel:     map[string]map[string]*releaseInfo{},
		resolved:   map[string]string{},
		lastCanary: map[string]string{},
		order:      newReleaseOrder(),
	}
}

// note records an event sighting for a release.
func (r *releaseState) note(project, release, ts string) {
	if release == "" {
		return
	}
	r.order.seen(release)
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.perRel[project]
	if m == nil {
		m = map[string]*releaseInfo{}
		r.perRel[project] = m
	}
	ri := m[release]
	if ri == nil {
		ri = &releaseInfo{}
		m[release] = ri
	}
	if ts > ri.lastEventTS {
		ri.lastEventTS = ts
	}
	ri.events++
}

// noteCanary records a canary observation for a project.
func (r *releaseState) noteCanary(project, ts string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ts > r.lastCanary[project] {
		r.lastCanary[project] = ts
	}
}

// noteResolved records the release a sig was last seen at when its incident was
// resolved/closed (§3.4's regression baseline).
func (r *releaseState) noteResolved(sig, release string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resolved[sig] = release
}

// releaseSeenreturns whether a release was ever observed for a project.
func (r *releaseState) releaseSeen(project, release string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.perRel[project]
	if m == nil {
		return false
	}
	_, ok := m[release]
	return ok
}

// lastEventFor returns the last event ts observed for a (project, release).
func (r *releaseState) lastEventFor(project, release string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m := r.perRel[project]; m != nil {
		if ri := m[release]; ri != nil {
			return ri.lastEventTS
		}
	}
	return ""
}

// releasesFor returns the releases observed for a project, newest first by the
// §3.4 ordering (unorderable strings fall back to first-observation order).
func (r *releaseState) releasesFor(project string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.perRel[project]
	out := make([]string, 0, len(m))
	for rel := range m {
		out = append(out, rel)
	}
	sort.Slice(out, func(i, j int) bool {
		if c, ok := compareReleases(out[i], out[j]); ok && c != 0 {
			return c > 0
		}
		return r.order.seen(out[i]) > r.order.seen(out[j])
	})
	return out
}

// regressionVerdict decides §3.4's regression for a group that just reappeared
// (or changed release).
//
//   - the group's incident was resolved at release R and the new release orders
//     after R → `confirmed`;
//   - the incident was resolved but the two releases cannot be ordered →
//     `unconfirmed` (never a claimed regression);
//   - no resolved incident, or the new release does not order after R → "".
func (r *releaseState) regressionVerdict(sig, newRelease string) string {
	if newRelease == "" {
		return ""
	}
	r.mu.Lock()
	resolvedRelease, ok := r.resolved[sig]
	r.mu.Unlock()
	if !ok {
		return ""
	}
	if resolvedRelease == "" {
		// Resolved without a release: any reappearance is a recurrence, not a
		// release regression.
		return ""
	}
	cmp, orderable := compareReleases(newRelease, resolvedRelease)
	if !orderable {
		return regressionUnconfirmed
	}
	if cmp > 0 {
		return regressionConfirmed
	}
	return ""
}

// ReleaseCoverage reports canary coverage for a release (§3.4 `fixed_in`).
//
// Coverage means: a canary landed after the last event seen for that release, so
// the quiet window is observed rather than assumed. Coverage without a canary is
// UNKNOWN, not fixed.
func (s *Server) ReleaseCoverage(projectID, release string) (canaryOK bool, lastEventTS string) {
	lastEventTS = s.releases.lastEventFor(projectID, release)
	s.releases.mu.Lock()
	canaryTS := s.releases.lastCanary[projectID]
	s.releases.mu.Unlock()
	if canaryTS == "" {
		return false, lastEventTS
	}
	if lastEventTS == "" {
		return true, ""
	}
	return canaryTS >= lastEventTS, lastEventTS
}

// NoteIncidentResolved records the release a sig was at when its incident
// closed, so the next release that orders after it is a confirmed regression
// (§3.4). SPEC-05 calls it on resolve; ObserveRecord replays it at boot.
func (s *Server) NoteIncidentResolved(sig, release string) {
	if sig == "" {
		return
	}
	if release == "" {
		if g, ok := s.groups.bySig(sig); ok && len(g.ReleaseRange) > 1 {
			release = g.ReleaseRange[1]
		}
	}
	s.releases.noteResolved(sig, release)
}

// ReleaseDiffResult carries the AC-19 release-diff view inputs (§3.4).
type ReleaseDiffResult struct {
	NewIn     []types.Group
	FixedIn   []types.Group
	StillOpen []types.Group
	Regressed []types.Group
	Coverage  bool // canary coverage of the `to` release for the project
	To        string
}

// ReleaseDiff composes the release-diff view inputs for a project and a target
// release. SPEC-10 renders them; sentinel owns the coverage rule, so the
// comparison happens here rather than in the dashboard.
func (s *Server) ReleaseDiff(projectID, to string) ReleaseDiffResult {
	out := ReleaseDiffResult{To: to}
	coverage, _ := s.ReleaseCoverage(projectID, to)
	out.Coverage = coverage
	origin := to
	for _, g := range s.groups.snapshot() {
		first := releaseAt(g.ReleaseRange, 0)
		last := releaseAt(g.ReleaseRange, 1)
		switch {
		case first == to:
			out.NewIn = append(out.NewIn, g)
			continue
		case last == origin && s.regressionOf(g.Sig) == regressionConfirmed:
			out.Regressed = append(out.Regressed, g)
			continue
		}
		if last == origin && first != to {
			// Seen at `from`, never at `to`: fixed_in only with proven coverage.
			if coverage {
				out.FixedIn = append(out.FixedIn, g)
			} else {
				out.StillOpen = append(out.StillOpen, g)
			}
			continue
		}
		out.StillOpen = append(out.StillOpen, g)
	}
	return out
}

func releaseAt(rr []string, i int) string {
	if i < len(rr) {
		return rr[i]
	}
	return ""
}

// regressionOf returns the last regression verdict recorded for a sig.
func (s *Server) regressionOf(sig string) string {
	s.gmu.Lock()
	defer s.gmu.Unlock()
	return s.regressions[sig]
}

// noteRegression records a sig's regression verdict for the release view.
func (s *Server) noteRegression(sig, verdict string) {
	if sig == "" || verdict == "" {
		return
	}
	s.gmu.Lock()
	defer s.gmu.Unlock()
	s.regressions[sig] = verdict
}
