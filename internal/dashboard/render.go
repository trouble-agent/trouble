package dashboard

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/trouble-agent/trouble/internal/types"
)

// Rendering — SPEC-10 §2.7/§2.8.
//
// html/template only (never text/template, never templ): the whole template
// set is parsed once at startup from the go:embed FS; a parse failure refuses
// the boot. CSS/JS are embedded too — no CDN, no third-party request of any
// kind, no inline script or style anywhere (so script-src 'self' suffices).

//go:embed templates/*.html
var templateFS embed.FS

//go:embed templates/404.html
var notFoundHTML []byte

//go:embed static/app.css static/app.js static/htmx.min.js
var staticFS embed.FS

// cspHeader is the §2.8 policy: every response that carries HTML.
const cspHeader = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

// maxPartialBytes is the §2.1.2 fragment contract: ≤8 KB uncompressed per
// partial. An over-cap fragment is fitted (see fitPartial) rather than shipped.
const maxPartialBytes = 8 << 10

// maxPageBytes is the §2.9 per-request buffer ceiling: an HTML writer streams to
// the socket and keeps no full-page buffer beyond 256 KiB.
const maxPageBytes = 256 << 10

// gzipThreshold is the §2.7 compression floor: HTML ≥1 KB is gzipped, and only
// when the client advertised gzip support.
const gzipThreshold = 1024

// pageFiles are the seven page templates; each defines its own "content".
var pageFiles = []string{"index", "incidents", "incident", "groups", "group", "rules", "breakers"}

// partialTemplates maps the §2.1.2 fragment names onto the template that
// defines them and the page set it lives in.
var partialTemplates = map[string]struct {
	file string
	name string
}{
	"health":    {"shell", "partial-health-strip"},
	"budget":    {"shell", "partial-budget-panel"},
	"incidents": {"incidents", "partial-incident-rows"},
	"timeline":  {"incident", "partial-incident-timeline"},
	"groups":    {"groups", "partial-group-rows"},
	"rules":     {"rules", "partial-rule-rows"},
	"breakers":  {"breakers", "partial-breaker-rows"},
}

// templateSetImpl wraps one parsed page set (shell + that page's content + the
// fragments the shell carries).
type templateSetImpl struct {
	tpl *template.Template
}

// templateEntry is one fragment: the set that defines it plus its template name.
type templateEntry struct {
	set  *templateSetImpl
	name string
}

// partialRows is the fragment payload shared by the §2.1.2 partials. Page
// templates embed it, so a fragment template renders identically whether it is
// polled standalone or included in a page.
type partialRows struct {
	Seq        uint64
	StallS     float64
	RenderTS   string
	Count      int
	Banner     bool
	IncidentID string
	// Truncated is the number of rows left out to keep the fragment within the
	// §2.1.2 8 KB cap (0 when everything fit).
	Truncated int

	// Poll triggers are pre-formatted here so templates stay attribute-only.
	StripTrigger   string
	ContentTrigger string

	Strip     healthStrip
	Budget    budgetPanel
	Incidents []incidentRow
	Timeline  []timelineEntry
	Groups    []groupRow
	Rules     []ruleRow
	Breakers  []breakerRow
}

// pageData is the shell + page model (§3.1).
type pageData struct {
	partialRows

	Title    string
	Nav      string
	CSRF     string
	CSRFC    string // fragment payloads only
	Scope    types.Scope
	ReadOnly bool
	Error    *dashError

	Counters countersView

	Incident *types.Incident
	Group    *types.Group
	Evidence *types.Evidence
	Story    Story
	Sensors  []types.SensorHealth
	// Subsystems is the §3.3a built/refused table of the overview page.
	Subsystems    []subsystemRow
	GroupID       string
	GroupLastAgeS float64

	// Write-control state: the controls always render (never hidden), with
	// data-requires and a disabled attribute when the scope is missing
	// (SPEC-10 §2.2). The server re-checks every POST regardless.
	CanWrite     bool
	CanAutonomy  bool
	KillSwitch   bool
	AutonomyMode string
	AllowResume  bool
	AllowFull    bool

	// CAS pair rendered into the ack/close forms (§2.1.1).
	ExpectedState string
	LedgerSeq     uint64

	// Polling schedule (§2.6).
	PollMS      int
	StripPollMS int
	StallAlertS int
}

// parseTemplates parses the embedded set once: shell + each page's content,
// with the fragments the shell carries. A parse error is a startup failure
// (§2.7): the caller refuses to boot.
func parseTemplates() (map[string]*templateSetImpl, map[string]templateEntry, error) {
	base, err := template.New("root").ParseFS(templateFS, "templates/shell.html")
	if err != nil {
		return nil, nil, err
	}
	sets := make(map[string]*templateSetImpl, len(pageFiles)+1)
	// The shell set carries the 7 fragments every page needs.
	sets["shell"] = &templateSetImpl{tpl: base}
	for _, page := range pageFiles {
		clone, err := base.Clone()
		if err != nil {
			return nil, nil, err
		}
		parsed, err := clone.ParseFS(templateFS, "templates/"+page+".html")
		if err != nil {
			return nil, nil, err
		}
		sets[page] = &templateSetImpl{tpl: parsed}
	}
	partials := make(map[string]templateEntry, len(partialTemplates))
	for name, def := range partialTemplates {
		set, ok := sets[def.file]
		if !ok {
			return nil, nil, fmt.Errorf("partial %q: unknown carrier page %q", name, def.file)
		}
		if set.tpl.Lookup(def.name) == nil {
			return nil, nil, fmt.Errorf("partial %q: template %q not defined", name, def.name)
		}
		partials[name] = templateEntry{set: set, name: def.name}
	}
	return sets, partials, nil
}

// loadStatic builds the row-19 asset table with ETags derived from the bytes.
func loadStatic() (map[string]staticAsset, error) {
	out := map[string]staticAsset{}
	ctypes := map[string]string{
		"static/app.css":     "text/css; charset=utf-8",
		"static/app.js":      "text/javascript; charset=utf-8",
		"static/htmx.min.js": "text/javascript; charset=utf-8",
	}
	for path, ctype := range ctypes {
		b, err := staticFS.ReadFile(path)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(b)
		out[strings.TrimPrefix(path, "static/")] = staticAsset{
			bytes: b,
			etag:  `"sha256-` + hex.EncodeToString(sum[:16]) + `"`,
			ctype: ctype,
		}
	}
	return out, nil
}

// newFragments seeds the shared fragment payload (poll schedule + render stamp).
func (s *server) newFragments() partialRows {
	return partialRows{
		RenderTS:       s.renderTS(),
		StripTrigger:   fmt.Sprintf("every %s", trimSeconds(s.cfg.StripPollMS)),
		ContentTrigger: fmt.Sprintf("every %s, troubleSeq from:body", trimSeconds(s.cfg.PollMS)),
	}
}

// trimSeconds renders a millisecond interval as a compact htmx trigger
// duration (2000ms → "2s", 500ms → "500ms").
func trimSeconds(ms int) string {
	if ms%1000 == 0 {
		return strconv.Itoa(ms/1000) + "s"
	}
	return strconv.Itoa(ms) + "ms"
}

// newPage builds the shell model for a page render: CSRF value for this token,
// scope-driven control state and the polling schedule.
func (s *server) newPage(r *http.Request, title, nav string) *pageData {
	p, _ := principalFrom(r.Context())
	now := s.now()
	d := &pageData{
		partialRows: s.newFragments(),
		Title:       clean(title),
		Nav:         nav,
		Scope:       p.Scopes.best(),
		ReadOnly:    s.cfg.ReadOnly,
		PollMS:      s.cfg.PollMS,
		StripPollMS: s.cfg.StripPollMS,
		StallAlertS: s.cfg.StallAlertS,
		CanWrite:    p.Scopes.has(types.ScopeWrite) && !s.cfg.ReadOnly,
		CanAutonomy: p.Scopes.has(types.ScopeAutonomy) && !s.cfg.ReadOnly,
		AllowResume: s.cfg.AllowResume,
		AllowFull:   s.cfg.AllowFull,
	}
	// The budget panel is seeded from the same source row 18 polls
	// (/partials/budget → budgetData): the page a browser paints first must not
	// show zeroes the next poll contradicts (TRBL-010 defect 2), and the footer
	// version stamp reads the same accessor the health surface reports
	// (TRBL-010 defect 1).
	d.Budget = s.budgetData()
	if s.deps.Autonomy != nil {
		g := s.deps.Autonomy.Gates()
		d.KillSwitch = g.KillSwitch
		d.AutonomyMode = string(g.Mode)
	}
	if s.canWrite() {
		d.CSRF = s.csrf.value(p.ID, now).Value
	}
	return d
}

// canWrite reports whether CSRF values are meaningful for this request (a
// read-only deployment still renders the value so a later scope upgrade needs
// no re-render — the server re-checks on every POST).
func (s *server) canWrite() bool { return true }

// best returns the highest scope in the set for display.
func (s scopeSet) best() types.Scope {
	switch {
	case s.has(types.ScopeAutonomy):
		return types.ScopeAutonomy
	case s.has(types.ScopeWrite):
		return types.ScopeWrite
	case s.has(types.ScopeRead):
		return types.ScopeRead
	}
	return ""
}

// renderPage renders a page through the shell with the §2.8 headers, the CSRF
// cookie and gzip for HTML ≥1 KB.
//
// The page is executed into a bounded buffer first (§2.9 allows a per-request
// buffer up to 256 KiB): a template that fails mid-execute must be able to take
// the §5 007 path, and a status cannot be chosen once the body has started
// streaming. Fragments are bounded by the 8 KB cap for the same reason.
func (s *server) renderPage(w http.ResponseWriter, r *http.Request, page string, d *pageData) error {
	set, ok := s.pageSets[page]
	if !ok || set == nil {
		return errRender
	}
	buf := getBuf()
	defer putBuf(buf)
	if err := set.tpl.ExecuteTemplate(buf, "shell", d); err != nil {
		return err
	}
	if buf.Len() > maxPageBytes {
		return fmt.Errorf("%w: page %s is %d bytes (cap %d)", errRender, page, buf.Len(), maxPageBytes)
	}
	body := buf.String()
	return s.writeHTML(w, r, http.StatusOK, true, func(ow io.Writer) error {
		_, werr := io.WriteString(ow, body)
		return werr
	})
}

// renderPartial renders one §2.1.2 fragment into a bounded buffer (the 8 KB
// contract is enforced, not assumed) and writes it with HTML headers.
func (s *server) renderPartial(w http.ResponseWriter, r *http.Request, name string, data partialRows) error {
	body, err := s.renderPartialString(name, data)
	if err != nil {
		return err
	}
	return s.writeHTML(w, r, http.StatusOK, false, func(ow io.Writer) error {
		_, werr := io.WriteString(ow, body)
		return werr
	})
}

// renderPartialString executes a fragment template and enforces the ≤8 KB cap
// of §2.1.2.
//
// The cap is a hard invariant, so an over-cap render is not shipped and not
// failed either: the fragment is re-rendered with as many complete rows as fit,
// and the remainder is reported as data-truncated plus a marker row (the
// template renders it). §2.1.2's "one <tr> per open incident" and its ≤8 KB cap
// cannot both hold for the row-heavy tables — a required incident row is ~129 B
// (the 26-char ULID in the href alone is 30 B), so 200 rows is ~25 KB. Fitting
// keeps the cap absolute and makes the overflow visible instead of silent; the
// full set stays reachable a page at a time (page_limit, max 500).
func (s *server) renderPartialString(name string, data partialRows) (string, error) {
	entry, ok := s.partials[name]
	if !ok || entry.set == nil {
		return "", errRender
	}
	buf := getBuf()
	defer putBuf(buf)
	if err := entry.set.tpl.ExecuteTemplate(buf, entry.name, data); err != nil {
		return "", err
	}
	if buf.Len() <= maxPartialBytes {
		return buf.String(), nil
	}
	return s.fitPartial(entry, data)
}

// fitPartial re-renders a fragment with a reduced row set until it fits the cap.
// Each pass scales the kept row count by the measured overshoot, so a normal
// table converges in one or two passes; the last resort is a one-row cut.
func (s *server) fitPartial(entry templateEntry, data partialRows) (string, error) {
	total := partialRowCount(data)
	if total == 0 {
		return "", fmt.Errorf("%w: fragment over the cap with no rows to drop", errRender)
	}
	keep := total
	for pass := 0; pass < 6; pass++ {
		buf := getBuf()
		err := entry.set.tpl.ExecuteTemplate(buf, entry.name, data)
		n := buf.Len()
		body := buf.String()
		putBuf(buf)
		if err != nil {
			return "", err
		}
		if n <= maxPartialBytes {
			return body, nil
		}
		next := keep * maxPartialBytes / n
		if next >= keep {
			next = keep - 1
		}
		if next <= 0 {
			return "", fmt.Errorf("%w: fragment cannot fit the %d-byte cap", errRender, maxPartialBytes)
		}
		keep = next
		data = trimPartial(data, keep, total)
	}
	return "", fmt.Errorf("%w: fragment did not converge under the %d-byte cap", errRender, maxPartialBytes)
}

// partialRowCount is the number of rows the fragment currently carries.
func partialRowCount(d partialRows) int {
	switch {
	case d.Incidents != nil:
		return len(d.Incidents)
	case d.Timeline != nil:
		return len(d.Timeline)
	case d.Groups != nil:
		return len(d.Groups)
	case d.Rules != nil:
		return len(d.Rules)
	case d.Breakers != nil:
		return len(d.Breakers)
	}
	return 0
}

// trimPartial keeps the first n rows and records how many were left out.
func trimPartial(d partialRows, n, total int) partialRows {
	d.Truncated = total - n
	switch {
	case d.Incidents != nil:
		d.Incidents = d.Incidents[:n]
	case d.Timeline != nil:
		d.Timeline = d.Timeline[:n]
	case d.Groups != nil:
		d.Groups = d.Groups[:n]
	case d.Rules != nil:
		d.Rules = d.Rules[:n]
	case d.Breakers != nil:
		d.Breakers = d.Breakers[:n]
	}
	return d
}

// renderFailure is the §5 TROUBLE-DASHBOARD-007 path: a template execute
// failure returns a static error fragment (never a template) and increments
// dash_render_errors_total; the rest of the dashboard keeps serving.
func (s *server) renderFailure(w http.ResponseWriter, r *http.Request, err error) {
	s.counters.renderErr.Add(1)
	if s.logger != nil {
		s.logger.Error("dashboard render failure", "path_len", len(r.URL.Path), "err", err.Error())
	}
	s.writeError(w, r, &dashError{Code: types.CodeDashboard007, HTTP: 500, Message: "render failed"})
}

// writeHTML is the one HTML write path: §2.8 security headers, the §2.8 CSP,
// no-store caching, the CSRF cookie on pages, and gzip for HTML ≥1 KB when the
// client advertised gzip.
func (s *server) writeHTML(w http.ResponseWriter, r *http.Request, status int, setCSRFCookie bool, write func(io.Writer) error) error {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", cspHeader)
	if setCSRFCookie {
		p, _ := principalFrom(r.Context())
		now := s.now()
		v := s.csrf.value(p.ID, now)
		http.SetCookie(w, s.csrfCookie(v.Value, s.cookieSecure(r), now))
	}
	gz := newThresholdGzip(w, status, gzipThreshold, clientAcceptsGzip(r))
	err := write(gz)
	if ferr := gz.finish(); err == nil {
		err = ferr
	}
	return err
}

// cookieSecure reports whether cookies must carry Secure: an https effective
// scheme or a non-loopback bind (§2.2, §2.3.4).
func (s *server) cookieSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return !s.loopback
}

// clientAcceptsGzip reports whether the request advertised gzip.
func clientAcceptsGzip(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
}

// thresholdGzip buffers the first <min bytes: a small response (most partials,
// the 404 page) is written plain, a large one switches to gzip once the
// threshold is crossed. The response buffer therefore never exceeds the
// threshold value, which is what keeps the §2.9 per-request budget honest.
type thresholdGzip struct {
	w      http.ResponseWriter
	status int
	min    int
	allow  bool

	buf       bytes.Buffer
	gz        *gzip.Writer
	committed bool
}

func newThresholdGzip(w http.ResponseWriter, status, min int, allow bool) *thresholdGzip {
	return &thresholdGzip{w: w, status: status, min: min, allow: allow}
}

func (t *thresholdGzip) Write(p []byte) (int, error) {
	if t.gz != nil {
		return t.gz.Write(p)
	}
	if !t.allow || t.buf.Len()+len(p) < t.min {
		return t.buf.Write(p)
	}
	// Threshold crossed: switch to gzip before the headers go out. A compressed
	// writer is taken from a bounded pool: a fresh flate writer is ~814 KB of
	// hash tables, which at 100 concurrent renders would be 81 MB — far past the
	// §2.9 ceiling of 12 MB. Past the pool's size the response is served
	// uncompressed (identity) rather than queueing behind a writer or growing
	// the heap: the content is identical, only the encoding differs, and the
	// pool is warmed before the boot baseline so its fixed cost is not billed to
	// the steady delta.
	gz := takeGzip(t.w)
	if gz == nil {
		return writeThrough(t.w, t.status, t.buf.Bytes(), p, &t.committed)
	}
	t.w.Header().Set("Content-Encoding", "gzip")
	t.w.WriteHeader(t.status)
	t.committed = true
	t.gz = gz
	if _, err := t.gz.Write(t.buf.Bytes()); err != nil {
		return 0, err
	}
	t.buf.Reset()
	return t.gz.Write(p)
}

// finish flushes the plain buffer or closes the gzip stream.
func (t *thresholdGzip) finish() error {
	if t.gz != nil {
		err := t.gz.Close()
		putGzip(t.gz)
		t.gz = nil
		return err
	}
	if !t.committed {
		t.w.WriteHeader(t.status)
	}
	_, err := t.w.Write(t.buf.Bytes())
	return err
}

// writeThrough serves an uncompressed response once the compression pool is
// busy: headers first, then the buffered prefix and the current chunk.
func writeThrough(w http.ResponseWriter, status int, prefix, chunk []byte, committed *bool) (int, error) {
	if !*committed {
		w.WriteHeader(status)
		*committed = true
	}
	nw, err := w.Write(prefix)
	if err != nil {
		return nw, err
	}
	n, err := w.Write(chunk)
	return nw + n, err
}

// gzipPoolSize bounds the compression pool. Four writers is ~3.3 MB of flate
// state — a fixed cost, sized against the §2.9 steady budget — and it caps the
// memory a burst of concurrent renders can hold.
const gzipPoolSize = 4

var gzipPool = make(chan *gzip.Writer, gzipPoolSize)

// takeGzip hands out a pooled, reset writer, or nil when the pool is empty.
func takeGzip(w io.Writer) *gzip.Writer {
	select {
	case gz := <-gzipPool:
		gz.Reset(w)
		return gz
	default:
		return nil
	}
}

// putGzip returns a writer to the pool (dropping it if the pool is full).
func putGzip(gz *gzip.Writer) {
	if gz == nil {
		return
	}
	select {
	case gzipPool <- gz:
	default:
	}
}

// WarmGzipPool allocates the pool's writers up front so their fixed cost lands
// in the boot baseline rather than in the first requests' working set.
func WarmGzipPool() {
	for i := 0; i < gzipPoolSize; i++ {
		select {
		case gzipPool <- gzip.NewWriter(io.Discard):
		default:
			return
		}
	}
}

// wallClock is the default clock.
func wallClock() time.Time { return time.Now() }
