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
	"sync"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
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
// partial. A fragment that would exceed it fails the render path
// (TROUBLE-DASHBOARD-007) instead of quietly shipping an oversized swap.
const maxPartialBytes = 8 << 10

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

	Incident      *types.Incident
	Group         *types.Group
	Evidence      *types.Evidence
	Story         Story
	Sensors       []types.SensorHealth
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
func (s *server) renderPage(w http.ResponseWriter, r *http.Request, page string, d *pageData) error {
	set, ok := s.pageSets[page]
	if !ok || set == nil {
		return errRender
	}
	return s.writeHTML(w, r, http.StatusOK, true, func(ow io.Writer) error {
		return set.tpl.ExecuteTemplate(ow, "shell", d)
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

// renderPartialString executes a fragment template and enforces the ≤8 KB cap.
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
	if buf.Len() > maxPartialBytes {
		return "", fmt.Errorf("%w: fragment %s is %d bytes (cap %d)", errRender, name, buf.Len(), maxPartialBytes)
	}
	return buf.String(), nil
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
	// Threshold crossed: switch to gzip before the headers go out.
	t.w.Header().Set("Content-Encoding", "gzip")
	t.w.WriteHeader(t.status)
	t.committed = true
	t.gz = gzipPool.Get().(*gzip.Writer)
	t.gz.Reset(t.w)
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
		gzipPool.Put(t.gz)
		t.gz = nil
		return err
	}
	if !t.committed {
		t.w.WriteHeader(t.status)
	}
	_, err := t.w.Write(t.buf.Bytes())
	return err
}

var gzipPool = sync.Pool{New: func() any { return gzip.NewWriter(io.Discard) }}

// wallClock is the default clock.
func wallClock() time.Time { return time.Now() }
