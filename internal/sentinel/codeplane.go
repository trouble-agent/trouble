package sentinel

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// codeplane.go implements SPEC-04 §3.9a: the cross-plane bundle the sentinel
// assembles on admission (AC-31), the sentinel-owned convergence map a
// sensor-born admission is filled through, and the release-mismatch discard
// rule. The ladder (SPEC-05 §3.13a) owns what happens to a bundle after it
// leaves this package; the sensor plane never writes the sentinel half.
//
// The bundle is context, never evidence: assembly changes no rung, no gate and
// no verification input, and a discarded bundle leaves the admission exactly as
// it would have run with no bundle at all (0 HTTP responses of its own).

const (
	// codeplaneSideSentinel is the Side a bundle assembled here carries.
	codeplaneSideSentinel = "sentinel"

	// convergenceWindow bounds how long a group stays linked after its last
	// event, and how long a stale-release (sig, release) pair is memoized: the
	// map is a correlation aid, not a store, and is rebuilt at boot from the
	// last 15 minutes of `group` records.
	convergenceWindow = 15 * 60 * time.Second

	// convergenceMaxEntries caps the map; eviction is least-recently-seen.
	convergenceMaxEntries = 4096

	// recentMax is the top-N signature counters a bundle carries.
	recentMax = 5

	// causeCodeplaneReleaseMismatch is the gap cause SPEC-TYPES §3.7 lists for
	// a refused stale-release bundle.
	causeCodeplaneReleaseMismatch = "codeplane_release_mismatch"
)

// convergenceEntry is one sig→group link: the open groups whose subject a
// sensor rule can also observe.
type convergenceEntry struct {
	digest    string
	groupID   string
	projectID string
	lastSeen  int64 // unix seconds of the group's last event
}

// convergenceMap is the sentinel-owned, read-only sig→group index. It is
// derived from the group store, never from config.
type convergenceMap struct {
	mu    sync.Mutex
	bySig map[string]*convergenceEntry
	order []string // sigs in insertion order; the eviction scan walks it
}

func newConvergenceMap() *convergenceMap {
	return &convergenceMap{bySig: map[string]*convergenceEntry{}}
}

// link records a group in the map (create, release change and flush on a live
// group all re-link; the boot rebuild folds rebuilt groups back in).
func (c *convergenceMap) link(digest, sig, groupID, projectID string, lastSeenUnix int64) {
	if sig == "" || digest == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.bySig[sig]
	if !ok {
		if len(c.bySig) >= convergenceMaxEntries {
			c.evictLocked()
		}
		e = &convergenceEntry{digest: digest}
		c.bySig[sig] = e
		c.order = append(c.order, sig)
	}
	e.digest = digest
	e.groupID = groupID
	e.projectID = projectID
	if lastSeenUnix > e.lastSeen {
		e.lastSeen = lastSeenUnix
	}
}

// unlink removes a sig's link (a refused bundle leaves the map).
func (c *convergenceMap) unlink(sig string) {
	if sig == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.bySig, sig)
}

// prune drops entries whose group went quiet before the cutoff.
func (c *convergenceMap) prune(cutoffUnix int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for sig, e := range c.bySig {
		if e.lastSeen < cutoffUnix {
			delete(c.bySig, sig)
		}
	}
}

// evictLocked drops the least-recently-seen entry; c.mu must be held.
func (c *convergenceMap) evictLocked() {
	oldestSig := ""
	var oldest int64 = -1
	for _, sig := range c.order {
		e, ok := c.bySig[sig]
		if !ok {
			continue
		}
		if oldest < 0 || e.lastSeen < oldest {
			oldest = e.lastSeen
			oldestSig = sig
		}
	}
	if oldestSig != "" {
		delete(c.bySig, oldestSig)
	}
}

// lookup returns a copy of a sig's link.
func (c *convergenceMap) lookup(sig string) (convergenceEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.bySig[sig]
	if !ok {
		return convergenceEntry{}, false
	}
	return *e, true
}

// NoteGroupSample records the representative event's rec_id on a group
// projection (§3.9a `Sample`): the composition root's sink calls it when the
// event record that opened or advanced the group became durable.
func (s *Server) NoteGroupSample(digest, recID string) {
	if digest == "" || recID == "" {
		return
	}
	s.groups.mu.Lock()
	if st, ok := s.groups.byDigest[digest]; ok && st.sample == "" {
		st.sample = recID
	}
	s.groups.mu.Unlock()
}

// CodeplaneFor is the one accessor the ladder calls on a sensor-born admission
// (SPEC-04 §3.9a): the open-group facts of an open group whose subject the
// sensor plane also observes, or ok=false when the sig has no live link. A
// stale entry (group closed or aged out) returns ok=false and the sensor-born
// admission proceeds with its own rule context only.
func (s *Server) CodeplaneFor(sig types.Sig) (types.CodeplaneContext, bool) {
	sigStr := sig.String()
	if sigStr == "" {
		return types.CodeplaneContext{}, false
	}
	entry, ok := s.conv.lookup(sigStr)
	if !ok || entry.digest == "" {
		return types.CodeplaneContext{}, false
	}
	st, ok := s.groups.state(entry.digest)
	if !ok || st.grp.ID == "" {
		return types.CodeplaneContext{}, false
	}
	// The group store is the truth; the map only found it. The release-mismatch
	// rule applies here too: a stale entry must not surface a bundle the
	// assembly rule would refuse.
	b, ok := s.assembleCodeplane(entry.digest, st, entry.projectID)
	if !ok {
		return types.CodeplaneContext{}, false
	}
	return *b, true
}

// assembleCodeplane builds the sentinel half of a bundle from a locked group
// snapshot (§3.9a's field table): the facts this package is the authority for
// and nothing else. ok=false means the release-mismatch rule refused the
// bundle.
func (s *Server) assembleCodeplane(digest string, st groupStateCopy, projectID string) (*types.CodeplaneContext, bool) {
	grp := st.grp
	release := ""
	if len(grp.ReleaseRange) > 1 {
		release = grp.ReleaseRange[1]
	}
	running := s.runningRelease(projectID, release)
	// The release-mismatch rule (normative): both values non-empty and unequal
	// means the group describes code that is not the code running, so the
	// bundle is discarded at assembly. An empty release is an absent fact,
	// never a mismatch (§3.9a edge cases).
	if release != "" && running != "" && release != running {
		return nil, false
	}
	b := &types.CodeplaneContext{
		Side:      codeplaneSideSentinel,
		Sig:       grp.Sig,
		GroupID:   grp.ID,
		Project:   projectID,
		Release:   release,
		Regressed: s.regressionOf(grp.Sig) != "",
		Recent:    s.recentSigCounts(grp.Sig),
		Sample:    st.sample,
		TS:        types.FormatUTC(s.now()),
	}
	return b, true
}

// runningRelease returns the newest release seen for the project under §3.4's
// releaseOrder — the release the sentinel believes is deployed. A project with
// no observed release falls back to the group's own last-seen release.
func (s *Server) runningRelease(projectID, fallback string) string {
	rels := s.releases.releasesFor(projectID)
	if len(rels) > 0 && rels[0] != "" {
		return rels[0]
	}
	return fallback
}

// recentSigCounts folds a sig's group counters into the top-5 recent
// signatures, count-descending (§3.9a: always on a sentinel admission).
func (s *Server) recentSigCounts(sig string) []types.SigCount {
	grp, ok := s.groups.bySig(sig)
	if !ok || grp.Count == 0 {
		return nil
	}
	out := []types.SigCount{{
		Sig:   grp.Sig,
		Count: int64(grp.Count),
		First: grp.FirstSeenTS,
		Last:  grp.LastSeenTS,
	}}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	if len(out) > recentMax {
		out = out[:recentMax]
	}
	return out
}

// attachCodeplane assembles the bundle for a group whose event just crossed a
// threshold and attaches it to the group draft the admission seam emits
// (§3.9a: every admission from this package fills the bundle). A refused
// bundle writes its gap and leaves the draft untouched — the admission
// proceeds unchanged and the discard path serves 0 HTTP responses of its own.
func (s *Server) attachCodeplane(draft *types.RecordDraft, digest, projectID string, st groupStateCopy) {
	if draft == nil {
		return
	}
	sig := st.grp.Sig
	b, ok := s.assembleCodeplane(digest, st, projectID)
	if !ok {
		// The mismatched bundle never reaches the ladder: the draft's bundle
		// stays nil and exactly one gap record is written per (sig, release)
		// pair inside the convergence window.
		bundleRelease := ""
		if len(st.grp.ReleaseRange) > 1 {
			bundleRelease = st.grp.ReleaseRange[1]
		}
		s.noteCodeplaneMismatch(sig, st.grp.ID, projectID, bundleRelease)
		s.conv.unlink(sig)
		return
	}
	draft.Codeplane = b
	var lastUnix int64
	if t, err := types.ParseUTC(st.grp.LastSeenTS); err == nil {
		lastUnix = t.Unix()
	}
	s.conv.link(digest, b.Sig, b.GroupID, projectID, lastUnix)
}

// noteCodeplaneMismatch writes the one gap record a stale-release pair
// memoizes for the convergence window (§3.9a step 2): cause
// codeplane_release_mismatch, sensor sentinel, scope <sig>, est_lost 0, and
// the {sig, grp, bundle_release, running_release} facts.
func (s *Server) noteCodeplaneMismatch(sig, grpID, projectID, bundleRelease string) {
	now := s.now()
	key := sig + "|" + bundleRelease
	s.gmu.Lock()
	if s.mismatchMemo == nil {
		s.mismatchMemo = map[string]time.Time{}
	}
	if at, ok := s.mismatchMemo[key]; ok && now.Sub(at) < convergenceWindow {
		s.gmu.Unlock()
		return
	}
	s.mismatchMemo[key] = now
	s.gmu.Unlock()
	payload := gapRecordPayload(causeCodeplaneReleaseMismatch, sig, 0, "", types.FormatUTC(now))
	payload["grp"] = grpID
	payload["bundle_release"] = bundleRelease
	payload["running_release"] = s.runningRelease(projectID, bundleRelease)
	_, _ = s.appendRecord(context.Background(), types.KGap, sig, "sentinel", payload, 0)
}

// rebuildConvergence folds a boot-rebuilt group base into the map from the
// last 15 minutes of `group` records (§3.9a: derived from the store, never
// from config).
func (s *Server) rebuildConvergence(grp types.Group, now time.Time) {
	if s.conv == nil || grp.Digest == "" {
		return
	}
	var lastUnix int64
	if t, err := types.ParseUTC(grp.LastSeenTS); err == nil {
		lastUnix = t.Unix()
	}
	if now.Unix()-lastUnix > int64(convergenceWindow/time.Second) {
		return
	}
	s.conv.link(grp.Digest, grp.Sig, grp.ID, "", lastUnix)
}

// pruneConvergence ages the map out (the flush loop calls it once per tick).
func (s *Server) pruneConvergence(now time.Time) {
	s.conv.prune(now.Unix() - int64(convergenceWindow/time.Second))
}
