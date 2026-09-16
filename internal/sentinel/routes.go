package sentinel

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// Route identity of the four §2.1 ingestion paths.
type routeKind int

const (
	routeNone routeKind = iota
	routeEnvelope
	routeStore
	routeEvent
	routeProbe
)

type route struct {
	kind   routeKind
	method string
	// dsnHosted is true for routes an SDK reaches through a DSN base.
	dsnHosted bool
}

// parseRoute matches `{project_id}` paths exactly. Anything else is a 404 with
// an empty body: probe traffic must never be able to write ledger records
// (§2.1).
func parseRoute(path string) (route, string, bool) {
	if !strings.HasPrefix(path, "/api/") {
		return route{}, "", false
	}
	rest := strings.TrimPrefix(path, "/api/")
	slash := strings.IndexByte(rest, '/')
	if slash <= 0 {
		return route{}, "", false
	}
	id := rest[:slash]
	if _, err := projectIDAtoi(id); err != nil {
		return route{}, "", false
	}
	tail := rest[slash:]
	switch tail {
	case "/envelope/":
		return route{kind: routeEnvelope, method: http.MethodPost, dsnHosted: true}, id, true
	case "/store/":
		return route{kind: routeStore, method: http.MethodPost, dsnHosted: true}, id, true
	case "/event/":
		return route{kind: routeEvent, method: http.MethodPost, dsnHosted: true}, id, true
	case "/":
		return route{kind: routeProbe, method: http.MethodGet}, id, true
	}
	return route{}, "", false
}

// routes builds the ingestion handler.
func (s *Server) routes() http.Handler {
	return http.HandlerFunc(s.serve)
}

// serve is the whole listener surface: routing, then the shared limits, then the
// route handler.
func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.counters.requests.Add(1)
	rt, projectID, ok := parseRoute(r.URL.Path)
	if !ok {
		s.counters.ingest404.Add(1)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if r.Method != rt.method {
		s.writeError(w, errf(types.CodeSentinel021, "method not allowed on an ingestion route", causeMethod))
		if rt.kind != routeProbe {
			w.Header().Set("Allow", rt.method)
		}
		return
	}

	// Concurrency cap: queued for at most 1s, then 429 overloaded (§3.7).
	if !s.sem.acquire(time.Second) {
		s.rateLimited(w, "1", overloadHeader, string(types.CodeSentinel010), "concurrency cap")
		return
	}
	defer s.sem.release()

	// Per-IP rate limit (§3.7).
	ip, zone := s.clientIP(r.RemoteAddr, r.Header.Get("X-Forwarded-For"))
	key := clientIPHash(ip)
	if key == "" {
		key = "unknown"
	}
	if !s.ipLim.allow(key, s.now()) {
		s.rateLimited(w, "1", ipHeader, string(types.CodeSentinel010), "per-IP rate limit")
		return
	}

	switch rt.kind {
	case routeEnvelope:
		s.handleEnvelope(w, r, projectID, zone)
	case routeStore:
		s.handleStore(w, r, projectID, zone)
	case routeEvent:
		s.handleGeneric(w, r, projectID, zone)
	case routeProbe:
		s.handleProbe(w, r, projectID, zone)
	default:
		s.counters.ingest404.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}
}

// handleEnvelope is the primary SDK route (§2.1, §3.1, §3.2).
func (s *Server) handleEnvelope(w http.ResponseWriter, r *http.Request, projectID, zone string) {
	body, rerr := s.readEnvelopeBody(w, r)
	if rerr != nil {
		s.writeError(w, rerr)
		return
	}
	// An unknown route project is TROUBLE-SENTINEL-007 before any auth work: the
	// path names the project the request claims (§5).
	if _, ok := s.projects.project(projectID); !ok {
		s.writeError(w, errf(types.CodeSentinel007, "unknown project", causeProjectUnknown))
		return
	}
	// Auth happens before the envelope is trusted, but the envelope_dsn form
	// lives in the header line, so the header is peeked for the DSN only.
	var envHeader map[string]any
	if r.Header.Get("X-Sentry-Auth") == "" && !r.URL.Query().Has("sentry_key") {
		if dsn := peekEnvelopeDSN(body); dsn != "" {
			envHeader = map[string]any{"dsn": dsn}
		}
	}
	auth, aerr := s.authenticate(r, projectID, envHeader, s.now())
	if aerr != nil {
		s.writeError(w, aerr)
		return
	}
	if auth.entry.proj.ID != projectID {
		s.writeError(w, errf(types.CodeSentinel006, "auth resolved a different project than the path", causeConflictingAuth))
		return
	}
	if dsn, ok := envHeader["dsn"].(string); ok && dsn != "" {
		if d, derr := ParseDSN(s.cfg, dsn); derr == nil && d.ProjectID != projectID {
			s.writeError(w, errf(types.CodeSentinel006, "the dsn project id differs from the request path", causeDSNProjectMism))
			return
		}
	}

	env, perr := s.parseEnvelope(body)
	if perr != nil {
		s.countReject(auth.entry, perr)
		s.writeError(w, perr)
		return
	}
	s.counters.envelopes.Add(1)
	eventID := env.headerString("event_id")
	if !validEventID(eventID) {
		eventID = newEventID()
	}
	if len(env.Items) == 0 {
		s.counters.emptyEnvelopes.Add(1)
		s.noteAccept(auth.entry)
		s.writeJSON(w, http.StatusOK, map[string]any{"id": eventID})
		return
	}

	rateHeader := ""
	var firstRefusal *Error
	for _, item := range env.Items {
		ev, accErr := s.acceptEnvelopeItem(r, auth, item, env)
		if accErr == nil && ev != nil {
			// The envelope layer only decodes; admission (scrub → sig → quota →
			// group → ledger) is the single funnel every path shares.
			_, accErr = s.admitEvent(r.Context(), auth.entry, ev, "event", "envelope")
		}
		if accErr != nil {
			// §6.10: a single envelope larger than quota_epm is admitted to
			// quota_epm items and the remainder follows the loss policy — so the
			// loop keeps going (every refused item writes its own drop record)
			// and the first refusal is the response.
			if firstRefusal == nil {
				firstRefusal = accErr
			}
			continue
		}
		if ev != nil && validEventID(ev.ID) {
			eventID = ev.ID
		}
		if h := s.pendingRateHeader(); h != "" {
			rateHeader = h
		}
	}
	if firstRefusal != nil {
		if s.refusal(w, firstRefusal) {
			return
		}
		s.countReject(auth.entry, firstRefusal)
		s.writeError(w, firstRefusal)
		return
	}
	s.noteAccept(auth.entry)
	if rateHeader != "" {
		w.Header().Set("X-Sentry-Rate-Limits", rateHeader)
	}
	if soft := s.takeSoftCode(); soft != "" {
		w.Header().Set("X-Sentry-Error", soft)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"id": eventID})
}

// pendingRateHeader reports the proactive-backoff header for the last decision,
// if any (§3.9: 200 + header at 95% of quota).
// attachRateHeader emits the proactive-backoff header (200 + header at 95% of
// quota, §3.9) on any successful response.
func (s *Server) attachRateHeader(w http.ResponseWriter) {
	if h := s.pendingRateHeader(); h != "" {
		w.Header().Set("X-Sentry-Rate-Limits", h)
	}
}

func (s *Server) pendingRateHeader() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.lastRateHeader
	s.lastRateHeader = ""
	return h
}

// acceptEnvelopeItem applies the per-item policy of §3.2. It returns the
// admitted event (event items) or nil for accounted drops.
func (s *Server) acceptEnvelopeItem(r *http.Request, auth authOutcome, item envelopeItem, env *envelope) (*rawEvent, *Error) {
	if item.Overrun {
		// A lying length is data, not a rejection (§6.4).
		s.counters.overrun.Add(1)
	}
	switch policyFor(item.Type) {
	case policyEvent:
		obj := map[string]any{}
		if err := json.Unmarshal(item.Body, &obj); err != nil || obj == nil {
			return nil, errf(types.CodeSentinel001, "event item body is not a JSON object", causeFraming)
		}
		ev := parseSentryEvent(obj)
		ev.Raw = append(json.RawMessage(nil), item.Body...)
		ev.SourceKind = sourceEnvelope
		ev.AuthForm = auth.form
		ev.ItemTypes = env.itemTypes()
		return ev, nil
	case policyClientReport:
		rep, err := parseClientReport(item.Body, auth.entry.proj.ID, s.now())
		if err != nil {
			// A malformed client_report is counted, never a rejection: the
			// envelope's other items are unaffected (§3.2 "never dropped").
			s.counters.countReason("client_report")
			return nil, nil
		}
		auth.entry.mergeClientReport(rep.Discarded)
		s.counters.clientReports.Add(1)
		ev := &rawEvent{
			ID:           newEventID(),
			TS:           types.FormatUTC(s.now()),
			Level:        "info",
			Message:      "client report",
			SourceKind:   sourceEnvelope,
			AuthForm:     auth.form,
			ItemTypes:    []string{"client_report"},
			ClientReport: rep,
		}
		if _, aerr := s.admitEvent(r.Context(), auth.entry, ev, "client_report", "client_report"); aerr != nil {
			return nil, aerr
		}
		return nil, nil
	case policyDropPlane, policyDropBinary:
		// Accepted and dropped with a counter; the body never touches the
		// ledger, and the raw bytes are never buffered beyond max_item_bytes.
		s.counters.itemsDropped.Add(1)
		s.counters.countItemType(item.Type)
		auth.entry.mu.Lock()
		auth.entry.itemDropped[item.Type]++
		auth.entry.mu.Unlock()
		return nil, nil
	default:
		// Unknown item type: 200-and-drop plus TROUBLE-SENTINEL-013. An
		// unsupported type NEVER fails the envelope (§3.2).
		s.setSoftCode(string(types.CodeSentinel013))
		s.counters.unknownItems.Add(1)
		s.counters.countItemType(item.Type)
		auth.entry.mu.Lock()
		auth.entry.unknownItems++
		auth.entry.itemDropped[item.Type]++
		auth.entry.mu.Unlock()
		return nil, nil
	}
}

// handleStore is the legacy compatibility route (§2.1, §3.1).
func (s *Server) handleStore(w http.ResponseWriter, r *http.Request, projectID, zone string) {
	if _, ok := s.projects.project(projectID); !ok {
		s.writeError(w, errf(types.CodeSentinel007, "unknown project", causeProjectUnknown))
		return
	}
	body, rerr := s.readEnvelopeBody(w, r)
	if rerr != nil {
		s.writeError(w, rerr)
		return
	}
	auth, aerr := s.authenticate(r, projectID, nil, s.now())
	if aerr != nil {
		s.writeError(w, aerr)
		return
	}
	if auth.entry.proj.ID != projectID {
		s.writeError(w, errf(types.CodeSentinel006, "auth resolved a different project than the path", causeConflictingAuth))
		return
	}
	jsonBody, berr := storeBody(r.Header.Get("Content-Type"), body)
	if berr != nil {
		s.countReject(auth.entry, berr)
		s.writeError(w, berr)
		return
	}
	obj := map[string]any{}
	if err := json.Unmarshal(jsonBody, &obj); err != nil || obj == nil {
		e := errf(types.CodeSentinel001, "store body is not a JSON object", causeFraming)
		s.countReject(auth.entry, e)
		s.writeError(w, e)
		return
	}
	ev := parseSentryEvent(obj)
	ev.Raw = append(json.RawMessage(nil), jsonBody...)
	ev.SourceKind = sourceStore
	ev.AuthForm = auth.form
	s.counters.legacyStore.Add(1)
	auth.entry.mu.Lock()
	auth.entry.legacyStore++
	auth.entry.mu.Unlock()
	rec, aerr := s.admitEvent(r.Context(), auth.entry, ev, "event", "store")
	if aerr != nil {
		if handled := s.refusal(w, aerr); handled {
			return
		}
		s.writeError(w, aerr)
		return
	}
	s.noteAccept(auth.entry)
	s.attachRateHeader(w)
	w.Header().Set("X-Sentry-Deprecated", "store")
	s.writeJSON(w, http.StatusOK, map[string]any{"id": rec.Payload["native_id"]})
}

// handleGeneric is the §3.6 generic JSON on-ramp.
func (s *Server) handleGeneric(w http.ResponseWriter, r *http.Request, projectID, zone string) {
	if _, ok := s.projects.project(projectID); !ok {
		s.writeError(w, errf(types.CodeSentinel007, "unknown project", causeProjectUnknown))
		return
	}
	body, rerr := s.readEnvelopeBody(w, r)
	if rerr != nil {
		s.writeError(w, rerr)
		return
	}
	auth, aerr := s.authenticate(r, projectID, nil, s.now())
	if aerr != nil {
		s.writeError(w, aerr)
		return
	}
	if auth.entry.proj.ID != projectID {
		s.writeError(w, errf(types.CodeSentinel006, "auth resolved a different project than the path", causeConflictingAuth))
		return
	}
	obj := map[string]any{}
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		e := errf(types.CodeSentinel019, "body is not a JSON object", causeGenericJSON)
		s.countReject(auth.entry, e)
		s.writeError(w, e)
		return
	}
	ev, perr := parseGenericEvent(obj)
	if perr != nil {
		s.countReject(auth.entry, perr)
		s.writeError(w, perr)
		return
	}
	ev.Raw = append(json.RawMessage(nil), body...)
	ev.SourceKind = sourceGeneric
	ev.AuthForm = auth.form
	rec, aerr := s.admitEvent(r.Context(), auth.entry, ev, "event", "generic_json")
	if aerr != nil {
		if handled := s.refusal(w, aerr); handled {
			return
		}
		s.writeError(w, aerr)
		return
	}
	s.noteAccept(auth.entry)
	s.attachRateHeader(w)
	s.writeJSON(w, http.StatusOK, map[string]any{"id": rec.Payload["native_id"]})
}

// handleProbe is the DSN reachability probe (§2.1): it answers 200 with the
// project state, and 401 only when auth is present but invalid.
func (s *Server) handleProbe(w http.ResponseWriter, r *http.Request, projectID, zone string) {
	entry, ok := s.projects.project(projectID)
	if !ok {
		s.writeError(w, errf(types.CodeSentinel007, "unknown project", causeProjectUnknown))
		return
	}
	hasAuth := r.Header.Get("X-Sentry-Auth") != "" || r.URL.Query().Has("sentry_key") || r.URL.Query().Has("sentry_secret")
	if hasAuth {
		if _, aerr := s.authenticate(r, projectID, nil, s.now()); aerr != nil {
			s.writeError(w, aerr)
			return
		}
	}
	status := "ok"
	if !entry.proj.Enabled {
		status = "disabled"
	}
	entry.mu.Lock()
	canaryTS := entry.canaryTS
	entry.mu.Unlock()
	s.writeJSON(w, http.StatusOK, map[string]any{
		"id":             entry.proj.ID,
		"slug":           projectSlug(entry.proj),
		"enabled":        entry.proj.Enabled,
		"status":         status,
		"canary_last_ts": canaryTS,
	})
}

// refusal turns a loss-policy refusal into the pinned 429/200 shape. It returns
// whether the response was already written.
func (s *Server) refusal(w http.ResponseWriter, err *Error) bool {
	switch err.Status {
	case http.StatusTooManyRequests:
		s.rateLimited(w, itoa(err.retryAfter()), err.Header, string(err.Code), err.Msg)
		return true
	}
	return false
}

// countReject records a rejected request for the storm detector of §5.
func (s *Server) countReject(entry *projectEntry, err *Error) {
	s.counters.countReason(rejectReason(err))
	s.noteReject(entry)
}

// rateLimited writes a 429 with the pinned headers.
func (s *Server) rateLimited(w http.ResponseWriter, retry, header, code, msg string) {
	if header != "" {
		w.Header().Set("X-Sentry-Rate-Limits", header)
	}
	w.Header().Set("Retry-After", retry)
	w.Header().Set("X-Sentry-Error", code)
	s.writeJSON(w, http.StatusTooManyRequests, map[string]any{"detail": msg, "causes": []string{}})
}

// writeError writes the pinned error shape: status + X-Sentry-Error + body.
func (s *Server) writeError(w http.ResponseWriter, err *Error) {
	if err == nil {
		err = errf(types.CodeSentinel001, "unknown failure", causeFraming)
	}
	w.Header().Set("X-Sentry-Error", string(err.Code))
	status := err.Status
	if status == 0 {
		status = http.StatusBadRequest
	}
	if status == http.StatusOK {
		status = http.StatusBadRequest
	}
	s.writeJSON(w, status, errBody(err))
}

// writeJSON writes a JSON response with the pinned content type.
func (s *Server) writeJSON(w http.ResponseWriter, status int, body any) {
	b, err := json.Marshal(body)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// peekEnvelopeDSN extracts the `dsn` key of the envelope header line without
// decoding the rest (the envelope_dsn auth form must be visible before the
// envelope is trusted).
func peekEnvelopeDSN(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	nl := len(body)
	if i := indexByte(body, '\n'); i >= 0 {
		nl = i
	}
	if nl == 0 || nl > maxHeaderLine {
		return ""
	}
	var probe struct {
		DSN string `json:"dsn"`
	}
	if err := json.Unmarshal(body[:nl], &probe); err != nil {
		return ""
	}
	return probe.DSN
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// parseClientReport decodes the SDK's own accounting (§3.2).
func parseClientReport(body []byte, projectID string, now time.Time) (*types.ClientReport, *Error) {
	var raw struct {
		Timestamp string `json:"timestamp"`
		Discarded []struct {
			Reason   string `json:"reason"`
			Category string `json:"category"`
			Quantity int    `json:"quantity"`
		} `json:"discarded_events"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, errf(types.CodeSentinel019, "client_report is not a JSON object", "client_report")
	}
	rep := &types.ClientReport{Project: projectID, TS: types.FormatUTC(now)}
	if ts, err := types.ParseUTC(raw.Timestamp); err == nil {
		rep.TS = types.FormatUTC(ts)
	}
	for _, d := range raw.Discarded {
		reason := d.Reason
		if reason == "" {
			reason = d.Category
		}
		rep.Discarded = append(rep.Discarded, types.DiscardCount{
			Reason:   reason,
			Category: d.Category,
			Quantity: d.Quantity,
		})
	}
	return rep, nil
}

// readBodyLimited reads a request body within a cap (the probe/GET path).
func readBodyLimited(r *http.Request, limit int64) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer r.Body.Close()
	return io.ReadAll(io.LimitReader(r.Body, limit))
}
