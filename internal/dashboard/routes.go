package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// route is one row of the §2.1 table — the only source of routes. Scope ""
// means the route carries no auth (§2.1 row 8's loopback exemption is decided
// at runtime by healthExempt).
type route struct {
	Method  string
	Path    string
	Scope   types.Scope
	Handler http.HandlerFunc
}

// routeTable is SPEC-10 §2.1 verbatim: 20 registrable rows. Row 21 ("any
// unmatched (method,path)") is the router wrapper itself in ServeHTTP, which
// answers every non-row pair with the deterministic 404 of §2.1.3.
func (s *server) routeTable() []route {
	return []route{
		{"GET", "/", types.ScopeRead, s.pageIndex},
		{"GET", "/incidents", types.ScopeRead, s.pageIncidents},
		{"GET", "/incidents/{id}", types.ScopeRead, s.pageIncident},
		{"GET", "/groups", types.ScopeRead, s.pageGroups},
		{"GET", "/groups/{id}", types.ScopeRead, s.pageGroup},
		{"GET", "/rules", types.ScopeRead, s.pageRules},
		{"GET", "/breakers", types.ScopeRead, s.pageBreakers},
		{"GET", "/health.json", types.ScopeRead, s.healthJSON},
		{"POST", "/api/incidents/{id}/ack", types.ScopeWrite, s.actionAck},
		{"POST", "/api/incidents/{id}/close", types.ScopeWrite, s.actionClose},
		{"POST", "/api/autonomy", types.ScopeAutonomy, s.actionAutonomy},
		{"GET", "/partials/health", types.ScopeRead, s.partialHealth},
		{"GET", "/partials/incidents", types.ScopeRead, s.partialIncidents},
		{"GET", "/partials/incidents/{id}/timeline", types.ScopeRead, s.partialTimeline},
		{"GET", "/partials/groups", types.ScopeRead, s.partialGroups},
		{"GET", "/partials/rules", types.ScopeRead, s.partialRules},
		{"GET", "/partials/breakers", types.ScopeRead, s.partialBreakers},
		{"GET", "/partials/budget", types.ScopeRead, s.partialBudget},
		{"GET", "/static/{asset}", types.ScopeRead, s.staticAsset},
		{"GET", "/partials/{name}", types.ScopeRead, s.partialUnknown},
	}
}

// registerRoutes is the only registration path: every row is registered into a
// real http.ServeMux with its Go 1.26 (method, path) pattern. A duplicate
// (method,path) is a startup panic by construction — the mux refuses it.
func (s *server) registerRoutes(mux *http.ServeMux) {
	for _, row := range s.routeTable() {
		row := row
		h := s.authWrap(row.Scope, row.Handler)
		if isPartialPath(row.Path) {
			h = s.shedWrap(row.Path, h)
		}
		mux.HandleFunc(row.Method+" "+row.Path, h)
	}
}

// isPartialPath reports whether a row is one of the polling partials the
// mem-pressure shed covers (rows 12–18 and 20).
func isPartialPath(p string) bool {
	return strings.HasPrefix(p, "/partials/")
}

// routeRow finds the exact (method, path) row, expanding {id}/{name}/{asset}
// wildcards one segment deep.
func (s *server) matchRow(method, path string) (route, bool) {
	for _, row := range s.routeTable() {
		if row.Method == method && pathMatches(row.Path, path) {
			return row, true
		}
	}
	return route{}, false
}

// pathMatches compares a registered pattern against a request path, treating a
// "{name}" segment as exactly one path segment.
func pathMatches(pattern, path string) bool {
	if pattern == path {
		return true
	}
	ps := strings.Split(pattern, "/")
	ss := strings.Split(path, "/")
	if len(ps) != len(ss) {
		return false
	}
	for i := range ps {
		if strings.HasPrefix(ps[i], "{") && strings.HasSuffix(ps[i], "}") {
			if ss[i] == "" {
				return false
			}
			continue
		}
		if ps[i] != ss[i] {
			return false
		}
	}
	return true
}

// urlTokenParams is the §2.2 parameter-name list, matched case-insensitively.
var urlTokenParams = map[string]bool{
	"token": true, "access_token": true, "auth": true, "apikey": true,
	"api_key": true, "key": true, "trouble_token": true, "tdt": true,
}

// ServeHTTP is the router wrapper: the §2.2 URL-borne token refusal runs
// first, before any authentication evaluation, on every route; then the
// §2.1.3 deterministic 404 (never 405) decides whether the (method,path) pair
// is a row at all. Only a matched row reaches the ServeMux.
func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if rec := recover(); rec != nil {
			// No handler may panic on a request path (contract quality bar):
			// this net keeps a bug from taking the listener down.
			s.counters.panics.Add(1)
			if s.logger != nil {
				s.logger.Error("dashboard handler panic", "method", r.Method, "path_len", len(r.URL.Path))
			}
			s.writeError(w, r, &dashError{Code: types.CodeDashboard013, HTTP: 500, Message: "internal error", Detail: "handler_panic"})
		}
	}()

	s.counters.served.Add(1)

	if s.urlTokenRefusal(w, r) {
		return
	}

	var allow []string
	matched := false
	for _, row := range s.routeTable() {
		if !pathMatches(row.Path, r.URL.Path) {
			continue
		}
		if row.Method == r.Method {
			matched = true
			break
		}
		allow = append(allow, row.Method)
	}
	if !matched {
		s.notFound(w, r, allow)
		return
	}
	s.mux.ServeHTTP(w, r)
}

// urlTokenRefusal implements §2.2: a token in the path or the query string is
// refused with 400 + TROUBLE-DASHBOARD-005 before authentication, even when
// the request would otherwise authenticate. The offending value is never
// logged (parameter name + length only).
func (s *server) urlTokenRefusal(w http.ResponseWriter, r *http.Request) bool {
	refuse := func(why string, name string, length int) bool {
		s.counters.urlToken.Add(1)
		if s.logger != nil {
			s.logger.Warn("token in URL refused", "why", why, "param", name, "value_len", length, "peer", ipString(s.clientIP(r)))
		}
		s.writeError(w, r, &dashError{Code: types.CodeDashboard005, HTTP: 400, Message: "token must not be in a URL", Detail: "url_token_" + why})
		return true
	}
	if raw := r.URL.RawQuery; raw != "" {
		if q, err := parseQuery(raw); err == nil {
			for name, vals := range q {
				lower := strings.ToLower(name)
				if urlTokenParams[lower] {
					l := 0
					if len(vals) > 0 {
						l = len(vals[0])
					}
					return refuse("param_name", lower, l)
				}
				for _, v := range vals {
					if tokenGrammar.MatchString(v) {
						return refuse("param_value", lower, len(v))
					}
				}
			}
		}
	}
	for _, seg := range strings.Split(r.URL.Path, "/") {
		if tokenGrammar.MatchString(seg) {
			return refuse("path_segment", "", len(seg))
		}
	}
	return false
}

// parseQuery is url.ParseQuery with a tolerant fallback for malformed
// raw queries (a malformed query must still be inspected for token names).
func parseQuery(raw string) (map[string][]string, error) {
	out := map[string][]string{}
	var firstErr error
	for _, pair := range strings.Split(raw, "&") {
		if pair == "" {
			continue
		}
		k, v, _ := strings.Cut(pair, "=")
		ku, err := urlUnescape(k)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			ku = k
		}
		vu, err := urlUnescape(v)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			vu = v
		}
		out[ku] = append(out[ku], vu)
	}
	return out, firstErr
}

func urlUnescape(s string) (string, error) {
	if !strings.ContainsAny(s, "%+") {
		return s, nil
	}
	return url.QueryUnescape(s)
}

// notFound is §2.1.3: 404 (never 405) with Allow naming the methods the path
// accepts, a static 1-line HTML document for browsers or a JSON error for API
// clients, and the requested path is never echoed.
func (s *server) notFound(w http.ResponseWriter, r *http.Request, allow []string) {
	s.counters.notfound.Add(1)
	if s.logger != nil {
		s.logger.Warn("dashboard route not found", "method_len", len(r.Method), "path_len", len(r.URL.Path), "peer", ipString(s.clientIP(r)))
	}
	if len(allow) > 0 {
		w.Header().Set("Allow", strings.Join(dedupeMethods(allow), ", "))
	}
	if wantsJSON(r) {
		s.writeError(w, r, &dashError{Code: types.CodeDashboard009, HTTP: 404, Message: "route not found"})
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", cspHeader)
	w.WriteHeader(http.StatusNotFound)
	w.Write(notFoundHTML)
}

func dedupeMethods(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, m := range in {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

// wantsJSON reports whether the error should be JSON: an htmx request or an
// Accept that asks for JSON without asking for HTML (§2.1.3).
func wantsJSON(r *http.Request) bool {
	if r.Header.Get("HX-Request") == "true" {
		return true
	}
	accept := r.Header.Get("Accept")
	if strings.Contains(accept, "text/html") {
		return false
	}
	return strings.Contains(accept, "application/json")
}

// authWrap authenticates the request for the row's scope, enforces the scope,
// the rate bucket and the auth-failure throttle, and stamps the principal into
// the request context for write handlers (SPEC-10 §2.2, §4.2).
func (s *server) authWrap(required types.Scope, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, de := s.authenticate(r, required)
		if de != nil {
			s.writeError(w, r, de)
			return
		}
		h(w, r.WithContext(withPrincipal(r.Context(), p)))
	}
}

// shedWrap applies the §2.9 mem-pressure load shed: when RSS has been at or
// above mem_pressure_pct of MemoryHigh for 60s, new partial polls get 503 +
// Retry-After: 5 while pages, /health.json and POSTs keep serving.
func (s *server) shedWrap(path string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.shed.shedding() {
			w.Header().Set("Retry-After", strconv.Itoa(shedRetryAfter))
			s.writeError(w, r, &dashError{Code: types.CodeDashboard013, HTTP: 503, Message: "memory pressure: poll load shed", Detail: "mem_pressure"})
			return
		}
		h(w, r)
	}
}

// memShedder tracks the sustained-over-threshold condition of §2.9. It is
// evaluated per partial request from the injected watermarks (no sampling
// goroutine, no extra state to leak).
type memShedder struct {
	pct   int
	rw    func() types.RuntimeWatermarks
	clock func() time.Time

	mu        sync.Mutex
	highSince time.Time
}

func newMemShedder(pct int, rw func() types.RuntimeWatermarks, clock func() time.Time) *memShedder {
	return &memShedder{pct: pct, rw: rw, clock: clock}
}

// shedding reports whether new partial polls must be refused right now.
func (m *memShedder) shedding() bool {
	if m == nil || m.pct <= 0 || m.rw == nil {
		return false
	}
	rw := m.rw()
	now := m.clock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if rw.MemHighBytes <= 0 || rw.RSSBytes*100 < int64(m.pct)*rw.MemHighBytes {
		m.highSince = time.Time{}
		return false
	}
	if m.highSince.IsZero() {
		m.highSince = now
		return false
	}
	return now.Sub(m.highSince) >= 60*time.Second
}

// ---------------------------------------------------------------------------
// Read surfaces
// ---------------------------------------------------------------------------

// pageIndex is row 1: counters + open incidents + health strip.
func (s *server) pageIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		s.notFound(w, r, nil)
		return
	}
	ctx := r.Context()
	rows, seq := s.incidentRows(ctx, s.cfg.PageLimit)
	groups, _ := s.groupRows(ctx, s.cfg.PageLimit)
	open, groupsOpen, perMin := s.deps.Index.Counters(s.now())
	d := s.newPage(r, "Overview", "index")
	d.Seq = seq
	d.Banner = s.bannerState()
	// One health assembly per render: the strip and the subsystem table read the
	// same response, so a page can never show a status the table contradicts.
	hr := s.deps.Health(ctx)
	d.Strip = s.stripFrom(hr)
	d.Subsystems = subsystemRows(hr.Subsystems)
	d.Incidents = rows
	d.Groups = groups
	d.Counters = countersView{IncidentsOpen: open, GroupsOpen: groupsOpen, EventsPerMin: perMin}
	if err := s.renderPage(w, r, "index", d); err != nil {
		s.renderFailure(w, r, err)
	}
}

// pageIncidents is row 2: the open-incident table plus each row's last-event age.
func (s *server) pageIncidents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, seq := s.incidentRows(ctx, s.cfg.PageLimit)
	d := s.newPage(r, "Incidents", "incidents")
	d.Seq = seq
	d.Banner = s.bannerState()
	d.Strip = s.stripData(ctx)
	d.Incidents = rows
	if err := s.renderPage(w, r, "incidents", d); err != nil {
		s.renderFailure(w, r, err)
	}
}

// pageIncident is row 3: the per-incident story panel (AC-16: issue ref, board
// row, research, spawn, promotion, candidate).
func (s *server) pageIncident(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id, types.PInc) {
		s.notFound(w, r, nil)
		return
	}
	inc, ok := s.deps.Lookup.Incident(id)
	if !ok || inc == nil {
		s.notFound(w, r, nil)
		return
	}
	ctx := r.Context()
	d := s.newPage(r, "Incident "+id, "incidents")
	d.Seq = s.deps.Index.LastSeq()
	d.Banner = s.bannerState()
	d.Strip = s.stripData(ctx)
	cinc := cleanIncident(*inc)
	d.Incident = &cinc
	d.IncidentID = id
	if inc.GroupID != "" {
		if g, ok := s.deps.Lookup.Group(inc.GroupID); ok && g != nil {
			cg := cleanGroup(*g)
			d.Group = &cg
		}
	}
	if ev, ok := s.deps.Lookup.Evidence(id); ok && ev != nil {
		cev := cleanEvidence(*ev)
		d.Evidence = &cev
	}
	if s.deps.Story != nil {
		d.Story = cleanStory(s.deps.Story(id))
	}
	d.Timeline, _ = s.timelineRows(id, 0, s.cfg.PageLimit)
	if err := s.renderPage(w, r, "incident", d); err != nil {
		s.renderFailure(w, r, err)
	}
}

// pageGroups is row 4: the ranked group table.
func (s *server) pageGroups(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, seq := s.groupRows(ctx, s.cfg.PageLimit)
	d := s.newPage(r, "Groups", "groups")
	d.Seq = seq
	d.Banner = s.bannerState()
	d.Strip = s.stripData(ctx)
	d.Groups = rows
	if err := s.renderPage(w, r, "groups", d); err != nil {
		s.renderFailure(w, r, err)
	}
}

// pageGroup is row 5: one group's projection plus its counter block. The
// per-sig event rate comes from the group projection's own counters; the
// wave-contract Index surface exposes no EventsBySig accessor (reported as a
// deviation in docs/operations.md).
func (s *server) pageGroup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id, types.PGrp) {
		s.notFound(w, r, nil)
		return
	}
	g, ok := s.deps.Lookup.Group(id)
	if !ok || g == nil {
		s.notFound(w, r, nil)
		return
	}
	ctx := r.Context()
	d := s.newPage(r, "Group "+id, "groups")
	d.Seq = s.deps.Index.LastSeq()
	d.Banner = s.bannerState()
	d.Strip = s.stripData(ctx)
	cg := cleanGroup(*g)
	d.Group = &cg
	d.GroupID = id
	if age, ok := s.deps.Index.LastEventAge(g.Sig, s.now()); ok {
		d.GroupLastAgeS = age
	}
	if err := s.renderPage(w, r, "group", d); err != nil {
		s.renderFailure(w, r, err)
	}
}

// pageRules is row 6: the SPEC-03 rule snapshot + RuleStats + sensor health.
func (s *server) pageRules(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d := s.newPage(r, "Rules", "rules")
	d.Seq = s.deps.Index.LastSeq()
	d.Banner = s.bannerState()
	d.Strip = s.stripData(ctx)
	d.Rules = s.ruleRows()
	d.Sensors = s.sensorList()
	if err := s.renderPage(w, r, "rules", d); err != nil {
		s.renderFailure(w, r, err)
	}
}

// pageBreakers is row 7: the SPEC-05 breaker registry.
func (s *server) pageBreakers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d := s.newPage(r, "Breakers", "breakers")
	d.Seq = s.deps.Index.LastSeq()
	d.Banner = s.bannerState()
	d.Strip = s.stripData(ctx)
	d.Breakers = s.breakerRows()
	if err := s.renderPage(w, r, "breakers", d); err != nil {
		s.renderFailure(w, r, err)
	}
}

// healthJSON is row 8: the §2.9 aggregator as JSON, also consumed by the
// external stall checker (SPEC-12 §3). The body never carries payload, message,
// stack, token material or DSN material.
func (s *server) healthJSON(w http.ResponseWriter, r *http.Request) {
	hr := s.deps.Health(r.Context())
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	b, err := json.Marshal(hr)
	if err != nil {
		s.renderFailure(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write(b)
}

// ---------------------------------------------------------------------------
// Polling partials (rows 12–18) and the §2.1.2 fragment contract
// ---------------------------------------------------------------------------

// partialHealth is row 12: the accelerator strip (every 1s).
func (s *server) partialHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := s.newFragments()
	data.Strip = s.stripData(ctx)
	if err := s.renderPartial(w, r, "health", data); err != nil {
		s.renderFailure(w, r, err)
	}
}

// partialIncidents is row 13: open-incident rows newer than since=<seq>, with
// edge case 11 handled — a since past the newest seq returns the current top
// page_limit rows (a full resync, not a gap).
func (s *server) partialIncidents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit := s.cfg.PageLimit
	since := parseSince(r)
	top := s.deps.Index.LastSeq()
	var rows []incidentRow
	var seq uint64
	if since > top {
		rows, seq = s.incidentRows(ctx, limit)
	} else {
		rows, seq = s.incidentRowsSince(ctx, since, limit)
	}
	data := s.newFragments()
	data.Seq, data.StallS, data.Count, data.Incidents = seq, s.healthStall(ctx), len(rows), rows
	if err := s.renderPartial(w, r, "incidents", data); err != nil {
		s.renderFailure(w, r, err)
	}
}

// partialTimeline is row 14: the ledger timeline for one incident. A partial
// for an incident that disappeared between render and poll (compaction or
// close) returns 200 with an empty-state fragment — a poll racing a legitimate
// state change is not an error (§6.1).
func (s *server) partialTimeline(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id, types.PInc) {
		s.notFound(w, r, nil)
		return
	}
	ctx := r.Context()
	limit := s.cfg.PageLimit
	since := parseSince(r)
	rows, seq := s.timelineRows(id, since, limit)
	data := s.newFragments()
	data.Seq, data.StallS, data.IncidentID, data.Timeline = seq, s.healthStall(ctx), id, rows
	if err := s.renderPartial(w, r, "timeline", data); err != nil {
		s.renderFailure(w, r, err)
	}
}

// partialGroups is row 15.
func (s *server) partialGroups(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, seq := s.groupRowsSince(ctx, parseSince(r), s.cfg.PageLimit)
	data := s.newFragments()
	data.Seq, data.StallS, data.Groups = seq, s.healthStall(ctx), rows
	if err := s.renderPartial(w, r, "groups", data); err != nil {
		s.renderFailure(w, r, err)
	}
}

// partialRules is row 16.
func (s *server) partialRules(w http.ResponseWriter, r *http.Request) {
	data := s.newFragments()
	data.Seq, data.StallS, data.Rules = s.deps.Index.LastSeq(), s.healthStall(r.Context()), s.ruleRows()
	if err := s.renderPartial(w, r, "rules", data); err != nil {
		s.renderFailure(w, r, err)
	}
}

// partialBreakers is row 17.
func (s *server) partialBreakers(w http.ResponseWriter, r *http.Request) {
	data := s.newFragments()
	data.Seq, data.StallS, data.Breakers = s.deps.Index.LastSeq(), s.healthStall(r.Context()), s.breakerRows()
	if err := s.renderPartial(w, r, "breakers", data); err != nil {
		s.renderFailure(w, r, err)
	}
}

// partialBudget is row 18: the runtime watermark panel.
func (s *server) partialBudget(w http.ResponseWriter, r *http.Request) {
	data := s.newFragments()
	data.Seq, data.StallS, data.Budget = s.deps.Index.LastSeq(), s.healthStall(r.Context()), s.budgetData()
	if err := s.renderPartial(w, r, "budget", data); err != nil {
		s.renderFailure(w, r, err)
	}
}

// partialUnknown is row 20: a partial name outside the §2.1.2 whitelist →
// 404 + TROUBLE-DASHBOARD-008 (JSON for htmx/API, a static fragment for a
// browser).
func (s *server) partialUnknown(w http.ResponseWriter, r *http.Request) {
	s.counters.notfound.Add(1)
	de := &dashError{Code: types.CodeDashboard008, HTTP: 404, Message: "unknown partial"}
	if wantsJSON(r) {
		s.writeError(w, r, de)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusNotFound)
	w.Write([]byte(`<div id="partial-error" data-code="TROUBLE-DASHBOARD-008">unknown partial</div>`))
}

// staticAsset is row 19: the embedded assets with ETag + a private cache.
func (s *server) staticAsset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("asset")
	a, ok := s.assets[name]
	if !ok {
		s.notFound(w, r, nil)
		return
	}
	h := w.Header()
	h.Set("Content-Type", a.ctype)
	h.Set("Cache-Control", "private, max-age=3600")
	h.Set("ETag", a.etag)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, a.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write(a.bytes)
}

// ---------------------------------------------------------------------------
// Write actions (rows 9–11)
// ---------------------------------------------------------------------------

// writeBody is the decoded §2.1.1 body: form values and JSON values flattened
// to strings (numeric ledger_seq included).
type writeBody map[string]string

func (b writeBody) get(k string) string { return b[k] }

// decodeBody enforces the body cap (413 + 011 before any decode) and accepts
// exactly application/json and application/x-www-form-urlencoded (SPEC-10
// §2.1.1/§2.8).
func (s *server) decodeBody(w http.ResponseWriter, r *http.Request) (writeBody, *dashError) {
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)
	ctype := r.Header.Get("Content-Type")
	if i := strings.IndexByte(ctype, ';'); i >= 0 {
		ctype = strings.TrimSpace(ctype[:i])
	}
	switch ctype {
	case "application/json", "":
		var m map[string]any
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&m); err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				s.counters.bodyTooBig.Add(1)
				return nil, &dashError{Code: types.CodeDashboard011, HTTP: 413, Message: "request body too large"}
			}
			return nil, invalidBody("json")
		}
		out := writeBody{}
		for k, v := range m {
			switch t := v.(type) {
			case string:
				out[k] = t
			case bool:
				out[k] = strconv.FormatBool(t)
			case float64:
				out[k] = strconv.FormatInt(int64(t), 10)
			case []any:
				parts := make([]string, 0, len(t))
				for _, e := range t {
					if s, ok := e.(string); ok {
						parts = append(parts, s)
					}
				}
				out[k] = strings.Join(parts, ",")
			case nil:
				// absent
			default:
				return nil, invalidBody("json_type")
			}
		}
		return out, nil
	case "application/x-www-form-urlencoded":
		body, err := io.ReadAll(r.Body)
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				s.counters.bodyTooBig.Add(1)
				return nil, &dashError{Code: types.CodeDashboard011, HTTP: 413, Message: "request body too large"}
			}
			return nil, invalidBody("form")
		}
		vals, err := parseQuery(string(body))
		if err != nil {
			return nil, invalidBody("form")
		}
		out := writeBody{}
		for k, vs := range vals {
			if len(vs) > 0 {
				out[k] = vs[0]
			}
		}
		return out, nil
	default:
		return nil, invalidBody("content_type")
	}
}

// invalidBody is the §2.1.1 body-validation refusal. The catalog has no
// dedicated body code, so the write-refusal code 010 carries the
// machine-readable detail (reported in docs/operations.md).
func invalidBody(detail string) *dashError {
	return &dashError{Code: types.CodeDashboard010, HTTP: 400, Message: "invalid request body", Detail: "invalid_body_" + detail}
}

// actionError is the minimal error shape a write seam may return so the
// dashboard surfaces the owning subsystem's code (e.g. TROUBLE-LADDER-012)
// instead of a generic 500 (SPEC-INDEX §5 rule 2, §6.1).
type actionError interface {
	ErrorCode() types.ErrorCode
}

// mapActionError converts a seam failure into a dashError, preserving the
// subsystem's code when it is exposed.
func (s *server) mapActionError(err error) *dashError {
	if err == nil {
		return nil
	}
	var ae actionError
	if errors.As(err, &ae) {
		code := ae.ErrorCode()
		status := http.StatusBadRequest
		if types.CodeClass[code] == types.ErrClassTransient {
			status = http.StatusServiceUnavailable
		}
		return &dashError{Code: code, HTTP: status, Message: "action refused", Detail: "subsystem_refused"}
	}
	return &dashError{Code: types.CodeDashboard013, HTTP: 500, Message: "action failed", Detail: "action_failed"}
}

// actionAck is row 9: ladder.Ack with the §2.1.1 compare-and-set pair. A
// mismatch is 409 + 010 (stale_view) with the refreshed fragment; the ladder
// owns the authoritative CAS (SPEC-05 §2).
func (s *server) actionAck(w http.ResponseWriter, r *http.Request) {
	id, ok := s.actionTarget(w, r)
	if !ok {
		return
	}
	body, de := s.decodeBody(w, r)
	if de != nil {
		s.writeError(w, r, de)
		return
	}
	if strings.TrimSpace(body.get("reason")) == "" {
		s.writeError(w, r, invalidBody("reason_required"))
		return
	}
	if de := s.casCheck(w, r, id, body); de != nil {
		s.writeError(w, r, de)
		return
	}
	if s.cfg.ReadOnly {
		s.writeError(w, r, s.readOnlyRefusal())
		return
	}
	p := principalOrEmpty(r)
	actor := humanActor(p)
	err := s.deps.Actions.Ack(r.Context(), id, actor, body.get("reason"), types.Duration(body.get("until")))
	if de := s.mapActionError(err); de != nil {
		s.writeError(w, r, de)
		return
	}
	s.counters.writeOk.Add(1)
	s.writeRefreshedIncidentRows(w, r)
}

// actionClose is row 10: ladder.Close with the same CAS pair.
func (s *server) actionClose(w http.ResponseWriter, r *http.Request) {
	id, ok := s.actionTarget(w, r)
	if !ok {
		return
	}
	body, de := s.decodeBody(w, r)
	if de != nil {
		s.writeError(w, r, de)
		return
	}
	if strings.TrimSpace(body.get("reason")) == "" {
		s.writeError(w, r, invalidBody("reason_required"))
		return
	}
	switch body.get("resolution") {
	case "fixed", "false_positive", "wontfix":
	default:
		s.writeError(w, r, invalidBody("resolution"))
		return
	}
	if de := s.casCheck(w, r, id, body); de != nil {
		s.writeError(w, r, de)
		return
	}
	if s.cfg.ReadOnly {
		s.writeError(w, r, s.readOnlyRefusal())
		return
	}
	p := principalOrEmpty(r)
	actor := humanActor(p)
	err := s.deps.Actions.Close(r.Context(), id, actor, body.get("reason"), body.get("resolution"))
	if de := s.mapActionError(err); de != nil {
		s.writeError(w, r, de)
		return
	}
	s.counters.writeOk.Add(1)
	s.writeRefreshedIncidentRows(w, r)
}

// actionAutonomy is row 11: the §2.2/§4.2 autonomy write. Enabling the
// kill-switch is always allowed; clearing it needs allow_resume; mode:"full"
// needs allow_full; read_only refuses everything.
func (s *server) actionAutonomy(w http.ResponseWriter, r *http.Request) {
	body, de := s.decodeBody(w, r)
	if de != nil {
		s.writeError(w, r, de)
		return
	}
	if s.cfg.ReadOnly {
		s.writeError(w, r, s.readOnlyRefusal())
		return
	}
	if s.deps.Autonomy == nil {
		s.writeError(w, r, &dashError{Code: types.CodeDashboard013, HTTP: 503, Message: "autonomy writer unavailable", Detail: "action_unavailable"})
		return
	}
	cur := s.deps.Autonomy.Gates()
	next := cur

	if mode := strings.TrimSpace(body.get("mode")); mode != "" {
		m := types.AutonomyMode(mode)
		if !m.Valid() {
			s.writeError(w, r, invalidBody("mode"))
			return
		}
		if m == types.AutoFull && !s.cfg.AllowFull {
			s.writeError(w, r, &dashError{Code: types.CodeDashboard010, HTTP: 403, Message: "mode full is merge-by-policy: configured off", Detail: "full_disabled"})
			return
		}
		next.Mode = m
	}
	if ks := body.get("kill_switch"); ks != "" {
		want, err := strconv.ParseBool(ks)
		if err != nil {
			s.writeError(w, r, invalidBody("kill_switch"))
			return
		}
		// Clearing the kill-switch requires allow_resume; enabling it is
		// always allowed with an autonomy token.
		if !want && cur.KillSwitch && !s.cfg.AllowResume {
			s.writeError(w, r, &dashError{Code: types.CodeDashboard010, HTTP: 403, Message: "kill-switch clearing requires allow_resume", Detail: "resume_disabled"})
			return
		}
		next.KillSwitch = want
	}
	if g, ok := body["grants"]; ok {
		if strings.TrimSpace(g) == "" {
			next.Grants = []string{}
		} else {
			next.Grants = strings.Split(g, ",")
		}
	}
	p := principalOrEmpty(r)
	actor := humanActor(p)
	gates, err := s.deps.Autonomy.SetAutonomy(r.Context(), next, actor)
	if de := s.mapActionError(err); de != nil {
		s.writeError(w, r, de)
		return
	}
	s.counters.writeOk.Add(1)
	s.writeRefreshedStrip(w, r, gates)
}

// actionTarget validates the {id} path segment for the ack/close rows: an id
// that is not `<prefix>_<ULID>` is not a route match, and an unknown incident
// is 404 carrying the ladder's own code (§6.1).
func (s *server) actionTarget(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !validID(id, types.PInc) {
		s.notFound(w, r, nil)
		return "", false
	}
	if _, ok := s.deps.Lookup.Incident(id); !ok {
		s.writeError(w, r, &dashError{Code: types.CodeLadder012, HTTP: 404, Message: "incident not found", Detail: "unknown_incident"})
		return "", false
	}
	return id, true
}

// casCheck implements the §2.1.1 compare-and-set: expected_state and
// ledger_seq are rendered into the fragment and must still hold. A mismatch is
// 409 + TROUBLE-DASHBOARD-010 (stale_view) plus the refreshed fragment, so a
// double-tap on a phone cannot double-close an incident.
func (s *server) casCheck(w http.ResponseWriter, r *http.Request, id string, body writeBody) *dashError {
	inc, ok := s.deps.Lookup.Incident(id)
	if !ok || inc == nil {
		return &dashError{Code: types.CodeLadder012, HTTP: 404, Message: "incident not found", Detail: "unknown_incident"}
	}
	stale := false
	if want := strings.TrimSpace(body.get("expected_state")); want != "" && want != string(inc.State) {
		stale = true
	}
	if seqS := strings.TrimSpace(body.get("ledger_seq")); seqS != "" {
		if want, err := strconv.ParseUint(seqS, 10, 64); err != nil || want != s.deps.Index.LastSeq() {
			stale = true
		}
	}
	if !stale {
		return nil
	}
	frag := s.incidentRowsFragment(r.Context())
	return &dashError{Code: types.CodeDashboard010, HTTP: 409, Message: "stale view", Detail: "stale_view", Fragment: frag}
}

// writeRefreshedIncidentRows answers a successful ack/close with the refreshed
// incident-rows fragment (row 9/10 response type).
func (s *server) writeRefreshedIncidentRows(w http.ResponseWriter, r *http.Request) {
	rows, seq := s.incidentRows(r.Context(), s.cfg.PageLimit)
	data := s.newFragments()
	data.Seq, data.StallS, data.Count, data.Incidents = seq, s.healthStall(r.Context()), len(rows), rows
	if err := s.renderPartial(w, r, "incidents", data); err != nil {
		s.renderFailure(w, r, err)
	}
}

// writeRefreshedStrip answers a successful autonomy write with the refreshed
// health-strip fragment (row 11 response type).
func (s *server) writeRefreshedStrip(w http.ResponseWriter, r *http.Request, gates types.AutonomyGates) {
	data := s.newFragments()
	data.Strip = s.stripDataWith(r.Context(), gates)
	if err := s.renderPartial(w, r, "health", data); err != nil {
		s.renderFailure(w, r, err)
	}
}

// incidentRowsFragment renders the incident-rows fragment into a string (used
// as the refreshed fragment on a 409).
func (s *server) incidentRowsFragment(ctx context.Context) string {
	rows, seq := s.incidentRows(ctx, s.cfg.PageLimit)
	data := s.newFragments()
	data.Seq, data.StallS, data.Count, data.Incidents = seq, s.healthStall(ctx), len(rows), rows
	b, err := s.renderPartialString("incidents", data)
	if err != nil {
		return ""
	}
	return b
}

// readOnlyRefusal is the §2.2 read_only policy: all POSTs 403 + 010.
func (s *server) readOnlyRefusal() *dashError {
	return &dashError{Code: types.CodeDashboard010, HTTP: 403, Message: "dashboard is read-only", Detail: "read_only"}
}

// humanActor builds the §4.2 actor for every human-initiated write: kind
// human, id = the token label, and the three build fields empty (SPEC-TYPES
// §3.1: "" for human).
func humanActor(p principal) types.Actor {
	return types.Actor{Kind: types.ActorHuman, ID: p.ID}
}

func principalOrEmpty(r *http.Request) principal {
	p, _ := principalFrom(r.Context())
	return p
}

func parseSince(r *http.Request) uint64 {
	v := r.URL.Query().Get("since")
	if v == "" {
		return 0
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// validID reports whether id is `<prefix><ULID>` for the given prefix.
func validID(id string, p types.Prefix) bool {
	if !strings.HasPrefix(id, string(p)) {
		return false
	}
	_, err := types.ParseID(p, id)
	return err == nil
}

// pageData and partialRows are the §3.1 view models; see render.go and
// view.go for their construction.

// viewRows is the fragment payload shape shared by the partial handlers.
type viewRows = partialRows

// errRender is the §5 TROUBLE-DASHBOARD-007 marker for a template execute
// failure at render time.
var errRender = errors.New("template render failed")

// bytesBuf pools the fragment buffers (each partial renders into a bounded
// buffer so the 8 KB contract can be enforced).
var bufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

func getBuf() *bytes.Buffer {
	b := bufPool.Get().(*bytes.Buffer)
	b.Reset()
	return b
}

func putBuf(b *bytes.Buffer) {
	if b.Cap() > 64<<10 {
		return // let a huge buffer be collected; never pool it
	}
	bufPool.Put(b)
}

// fmtLen renders a byte count for the budget panel.
func fmtLen(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
