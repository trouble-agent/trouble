package sentinel

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// projectEntry is per-project runtime state: the config record, the keys that
// currently resolve to it (including a rotation overlap key), the auth forms
// observed over the ledger, and the counters of §3.10's ProjectRuntime.
type projectEntry struct {
	proj types.Project

	mu        sync.Mutex
	keys      map[string]time.Time // pubkey → valid_until (zero = forever)
	forms     map[string]bool      // observed auth_forms
	lastEvent string
	canaryTS  string
	canaryOK  bool

	rejected      uint64
	dropped       uint64
	spooled       uint64
	sampled       uint64
	legacyStore   uint64
	unknownItems  uint64
	clientReport  map[string]int
	eventsTotal   uint64
	duplicateEvts uint64
	itemDropped   map[string]uint64
}

func newProjectEntry(p types.Project) *projectEntry {
	e := &projectEntry{
		proj:         p,
		keys:         map[string]time.Time{},
		forms:        map[string]bool{},
		clientReport: map[string]int{},
		itemDropped:  map[string]uint64{},
	}
	for _, f := range p.AuthForms {
		e.forms[f] = true
	}
	return e
}

// projectIndex resolves keys and projects (§2.4).
type projectIndex struct {
	mu    sync.RWMutex
	byID  map[string]*projectEntry
	byKey map[string]*projectEntry
	order []string
}

func newProjectIndex(projects []types.Project) (*projectIndex, *Error) {
	ix := &projectIndex{byID: map[string]*projectEntry{}, byKey: map[string]*projectEntry{}}
	for _, p := range projects {
		if _, err := projectIDAtoi(p.ID); err != nil {
			return nil, errf(types.CodeSentinel007, "project id must be numeric-as-string", causeProjectUnknown)
		}
		if _, dup := ix.byID[p.ID]; dup {
			return nil, errf(types.CodeSentinel007, "duplicate project id", causeProjectUnknown)
		}
		e := newProjectEntry(p)
		e.keys[p.PublicKey] = time.Time{}
		ix.byID[p.ID] = e
		ix.byKey[p.PublicKey] = e
		ix.order = append(ix.order, p.ID)
	}
	sort.Slice(ix.order, func(i, j int) bool { return ix.order[i] < ix.order[j] })
	return ix, nil
}

// project resolves a project id.
func (ix *projectIndex) project(id string) (*projectEntry, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	e, ok := ix.byID[id]
	return e, ok
}

// byPublicKey resolves a 32-hex public key, honoring rotation validity.
func (ix *projectIndex) byPublicKey(key string, now time.Time) (*projectEntry, *Error) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	e, ok := ix.byKey[key]
	if !ok {
		return nil, errf(types.CodeSentinel006, "unknown public key", causeUnknownKey)
	}
	e.mu.Lock()
	until, known := e.keys[key]
	e.mu.Unlock()
	if !known {
		return nil, errf(types.CodeSentinel006, "unknown public key", causeUnknownKey)
	}
	if !until.IsZero() && !now.Before(until) {
		return nil, errf(types.CodeSentinel006, "public key is expired or revoked", causeKeyExpired)
	}
	return e, nil
}

// addRotationKey installs a second public key for a project with a validity
// deadline (§2.3 rotation: reporters roll without a blind window).
func (ix *projectIndex) addRotationKey(pubkey, projectID string, validUntil time.Time) *Error {
	e, ok := ix.project(projectID)
	if !ok {
		return errf(types.CodeSentinel007, "unknown project", causeProjectUnknown)
	}
	if !validPubKey(pubkey) {
		return errf(types.CodeSentinel006, "rotation key must be exactly 32 lowercase hex", causeSecretLength)
	}
	ix.mu.Lock()
	ix.byKey[pubkey] = e
	ix.mu.Unlock()
	e.mu.Lock()
	e.keys[pubkey] = validUntil
	e.mu.Unlock()
	return nil
}

// revokeKey sets valid_until = now so subsequent requests fail 006 (§2.3).
func (ix *projectIndex) revokeKey(pubkey string, now time.Time) *Error {
	ix.mu.RLock()
	e, ok := ix.byKey[pubkey]
	ix.mu.RUnlock()
	if !ok {
		return errf(types.CodeSentinel006, "unknown public key", causeUnknownKey)
	}
	e.mu.Lock()
	e.keys[pubkey] = now
	e.mu.Unlock()
	return nil
}

// observeForm records an accepted auth form for the project (§2.4: the deduped
// union observed over the ledger, rebuilt at boot, persisted as the project
// index).
func (e *projectEntry) observeForm(form string) {
	if form == "" {
		return
	}
	e.mu.Lock()
	e.forms[form] = true
	e.mu.Unlock()
}

// forms returns the observed auth forms in a stable order.
func (e *projectEntry) authFormList() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.forms))
	for f := range e.forms {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// runtime composes the §3.10 ProjectRuntime view.
func (e *projectEntry) runtime(now time.Time, eventsWindow int, diskBytes int64, diskBudget int64) types.ProjectRuntime {
	e.mu.Lock()
	defer e.mu.Unlock()
	rt := types.ProjectRuntime{
		Project:              e.proj.ID,
		QuotaEPM:             e.proj.QuotaEPM,
		WindowS:              60,
		EventsWindow:         eventsWindow,
		Remaining:            e.proj.QuotaEPM - eventsWindow,
		RejectedTotal:        e.rejected,
		DroppedTotal:         e.dropped,
		SpooledTotal:         e.spooled,
		SampledTotal:         e.sampled,
		LegacyStoreTotal:     e.legacyStore,
		UnknownItemsTotal:    e.unknownItems,
		ClientReportDiscards: map[string]int{},
		AuthForms:            e.authFormListLocked(),
		LastEventTS:          e.lastEvent,
		CanaryLastTS:         e.canaryTS,
		CanaryLastOK:         e.canaryOK,
		DiskBytes:            diskBytes,
		DiskBudgetBytes:      diskBudget,
	}
	_ = now
	if rt.Remaining < 0 {
		rt.Remaining = 0
	}
	for k, v := range e.clientReport {
		rt.ClientReportDiscards[k] = v
	}
	return rt
}

func (e *projectEntry) authFormListLocked() []string {
	out := make([]string, 0, len(e.forms))
	for f := range e.forms {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// mergeClientReport folds a parsed client report into the runtime counters
// (§3.2: SDK-side drops are visible data, never silence).
func (e *projectEntry) mergeClientReport(discards []types.DiscardCount) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, d := range discards {
		key := d.Reason
		if key == "" {
			key = d.Category
		}
		if key == "" {
			key = "unknown"
		}
		e.clientReport[key] += d.Quantity
	}
}

// counterSnapshot is the read side of the per-project counters.
func (e *projectEntry) counterSnapshot() map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	items := map[string]any{}
	for k, v := range e.itemDropped {
		items[k] = v
	}
	return map[string]any{
		"events_total":           e.eventsTotal,
		"duplicate_events_total": e.duplicateEvts,
		"rejected_total":         e.rejected,
		"dropped_total":          e.dropped,
		"spooled_total":          e.spooled,
		"sampled_total":          e.sampled,
		"legacy_store_total":     e.legacyStore,
		"unknown_items_total":    e.unknownItems,
		"items_dropped_total":    items,
	}
}

// projectSlug normalizes a configured slug; a project without one displays its
// id so the dashboard is never blank.
func projectSlug(p types.Project) string {
	if strings.TrimSpace(p.Slug) != "" {
		return strings.TrimSpace(p.Slug)
	}
	return p.ID
}
