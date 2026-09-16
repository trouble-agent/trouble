package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/totalwindupflightsystems/trouble/internal/types"
)

// dashError is every refusal path of SPEC-10 §5: code, HTTP status, a
// human-readable message and a machine-readable Detail naming the failed
// check (read_only | resume_disabled | stale_view | invalid_body |
// project_scope_required | …).
type dashError struct {
	Code    types.ErrorCode // TROUBLE-DASHBOARD-NNN (or the cross-area code it references)
	HTTP    int
	Message string
	Detail  string // machine-readable: read_only | resume_disabled | stale_view | invalid_body | …
	// RetryAfter carries the integer Retry-After seconds for 429 responses
	// (0 = default).
	RetryAfter int
	// Fragment carries a refreshed HTML fragment for 409 stale_view responses
	// (SPEC-10 §2.1.1); it is embedded in the JSON body.
	Fragment string
}

func (e *dashError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("%s (%s): %s", e.Code, e.Detail, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// dashError.As unwraps a *dashError wrapped with fmt.Errorf for errors.As.
var _ = errors.As

// errorBody is the wire shape of every JSON refusal (SPEC-10 §5, §2.1.3).
type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Detail   string `json:"detail,omitempty"`
	TS       string `json:"ts"`
	Fragment string `json:"fragment,omitempty"` // refreshed fragment on a 409 stale_view
}

// errorCode exposes the catalog code of this refusal.
func (e *dashError) errorCode() types.ErrorCode { return e.Code }

// counters is the request/log counter store (SPEC-10 §4.1 step 5, §2.1.3,
// §4.2): every refusal class the #health-strip exposes and the two totals the
// error path increments.
type counters struct {
	denied     atomic.Uint64 // 401/403 refusals (auth + scope)
	csrf       atomic.Uint64 // 403 + TROUBLE-DASHBOARD-004
	rl         atomic.Uint64 // 429 + TROUBLE-DASHBOARD-012
	notfound   atomic.Uint64 // 404/405, §2.1.3
	renderErr  atomic.Uint64 // 500 + TROUBLE-DASHBOARD-007
	bodyTooBig atomic.Uint64 // 413 + TROUBLE-DASHBOARD-011
	urlToken   atomic.Uint64 // 400 + TROUBLE-DASHBOARD-005
	writeOk    atomic.Uint64 // successful POST actions
	panics     atomic.Uint64 // recovered handler panics (defensive net)
	served     atomic.Uint64 // requests served (health-strip surface)
}

// stallState is the server-side stale-render tracker (§3.1, §2.6): three
// consecutive identical health-strip seqs combined with a stall counter at or
// above stall_alert_s turns the advisory banner on. Identical seqs alone are
// normal on a quiet host; the growing stall counter is the signal.
type stallState struct {
	LastSeq uint64
	Repeat  int
	LastOK  string // RFC3339 of the last successful strip render
	Banner  bool
}

// server is the dashboard runtime: config, injected deps, the token store,
// the route mux, the parsed template sets, the in-memory CSRF secret, the
// rate limiter and the request counters. Constructed by newServer; Serve owns
// the listener lifecycle.
type server struct {
	cfg      Config
	deps     Deps
	store    *TokenStore
	mux      *http.ServeMux
	pageSets map[string]*templateSet
	partials map[string]templateEntry
	csrf     *csrfEngine
	limiter  *limiter
	throttle *ipThrottle
	identity identityProvider
	shed     *memShedder

	counters counters

	stallMu sync.Mutex
	stall   stallState

	clock    func() time.Time
	logger   *slog.Logger
	started  time.Time
	loopback bool // the effective bind is loopback (§2.4, health exemption)
	assets   map[string]staticAsset
}

// templateSet and templateEntry are thin aliases decoupling render.go from the
// concrete *template.Template so tests can inject broken sets.
type templateSet = templateSetImpl

// staticAsset is one embedded asset (SPEC-10 §2.1 row 19).
type staticAsset struct {
	bytes []byte
	etag  string
	ctype string
}

// Deps is everything the dashboard needs from the outside world (SPEC-10
// §4.1, wave contract). The composition root adapts its subsystems onto these
// interfaces; the dashboard imports none of them.
type Deps struct {
	Config   Config
	Index    Index                                          // required — SPEC-01 index read surface
	Lookup   IncidentLookup                                 // required — incident/group/evidence lookup
	Story    func(inc string) Story                         // required — the /incidents/{id} story panel
	Health   func(ctx context.Context) types.HealthResponse // required — lifecycle assembly
	Actions  IncidentActions                                // required for ack/close
	Autonomy AutonomyWriter                                 // required for POST /api/autonomy

	Rules      []types.Rule
	RuleStats  func(name string) (lastFireTS string, fires, suppressed uint64)
	Sensors    func() []types.SensorHealth
	Breakers   func() []types.Breaker
	Sources    func() []types.SourceLiveness
	Watermarks func() types.RuntimeWatermarks
	Version    func() (version, gitSHA, buildTime string, unstamped bool)
	Clock      func() time.Time // injectable for tests; nil = time.Now
	Logger     *slog.Logger

	// IngestionKeys are the configured project DSN PublicKey/SecretKey
	// plaintexts. The token store refuses a hash equal to any of them at load
	// (§4.3.3) and Mint refuses a plaintext equal to any of them (§4.3.2).
	IngestionKeys []string

	// TokenFile overrides Config.TokenFile when non-empty. It exists so the
	// composition root can hand the resolved 0600 store path to the server it
	// constructs (and so tests can point at a temp file).
	TokenFile string
}

// Index is the dashboard's declared read surface, structurally identical to
// ledger.DashReader (SPEC-10 §3.3). Every method is O(rows returned) and
// touches no file; the composition root passes ledger.DashReader() straight
// in.
type Index interface {
	LastSeq() uint64
	LastRecordTS() string
	Counters(now time.Time) (incidentsOpen, groupsOpen int, eventsPerMin float64)
	OpenIncidents(limit int) []types.Incident
	OpenIncidentsSince(seq uint64, limit int) []types.Incident
	GroupsSince(seq uint64, limit int) []types.GroupStat
	RecordsForIncident(inc string, since uint64, limit int) []types.Record
	LastEventAge(sig string, now time.Time) (float64, bool)
}

// IncidentLookup resolves a single incident/group/evidence for the detail
// pages and the ack/close CAS.
type IncidentLookup interface {
	Incident(id string) (*types.Incident, bool)
	Group(id string) (*types.Group, bool)
	Evidence(inc string) (*types.Evidence, bool) // nil when absent, never an error
}

// Story is the /incidents/{id} panel beyond the index rows (SPEC-10 §2.1 row
// 3): issue, board row, research outcome, spawn, promotion and skill candidate
// links. Assembled by the composition root; ""/nil fields render as absent.
type Story struct {
	IssueRef   string // issue id + URL, "" when none
	BoardRow   string // the tsk_ row id, "" when none
	ResearchID string
	Research   string // research outcome summary, "" when none
	SpawnID    string // flow spawn id
	Promotion  string // promotion state + PR link
	Candidate  string // skill candidate id + state
	Records    []types.Record
}

// IncidentActions is the write seam onto the ladder (SPEC-05): the dashboard
// never appends to the ledger itself, §4.2.
type IncidentActions interface {
	Ack(ctx context.Context, inc string, actor types.Actor, reason string, until types.Duration) error
	Close(ctx context.Context, inc string, actor types.Actor, reason, resolution string) error
}

// AutonomyWriter is the write seam onto the autonomy gates (SPEC-05 §3.11);
// the composition root adapts it onto lifecycle.SetAutonomy so the config
// record is written through one audited path (SPEC-10 §4.2).
type AutonomyWriter interface {
	Gates() types.AutonomyGates
	SetAutonomy(ctx context.Context, gates types.AutonomyGates, actor types.Actor) (types.AutonomyGates, error)
}

// serveTimeout constants — SPEC-10 §2.8.
const (
	maxHeaderBytes = 16 << 10 // 16 KiB request header cap
	readTimeout    = 10 * time.Second
	writeTimeout   = 10 * time.Second
	idleTimeout    = 60 * time.Second
	drainTimeout   = 5 * time.Second // graceful drain on ctx cancel, §4.1 step 6
	shedRetryAfter = 5               // Retry-After seconds for the mem-pressure shed
)

// Serve starts the dashboard listener per SPEC-10 §4.1: validate config, bind
// (collision → TROUBLE-LIFECYCLE-003, fail loud), load the token store, parse
// the template set, register the §2.1 route table, and serve until ctx is
// cancelled — then drain in-flight requests for ≤5s and close. The dashboard
// is a reader: there is no ledger state to flush.
func Serve(ctx context.Context, cfg Config, deps Deps) error {
	s, err := newServer(cfg, deps)
	if err != nil {
		return err
	}

	addr := net.JoinHostPort(cfg.Bind, fmt.Sprintf("%d", cfg.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return &dashError{Code: types.CodeLifecycle003, HTTP: 500, Message: "bind preflight refused", Detail: "bind_collision"}
	}

	srv := &http.Server{
		Handler:           s,
		MaxHeaderBytes:    maxHeaderBytes,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		ReadHeaderTimeout: readTimeout,
	}
	if s.logger != nil {
		s.logger.Info("dashboard listening", "addr", addr, "identity", cfg.Identity, "loopback", s.loopback)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ln)
	}()

	select {
	case <-ctx.Done():
		// Stop accepting; drain in-flight requests for ≤5s (SPEC-10 §4.1
		// step 6). No ledger state exists to flush.
		shCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
		defer cancel()
		if err := srv.Shutdown(shCtx); err != nil && s.logger != nil {
			s.logger.Warn("dashboard drain incomplete", "err", err)
		}
		srv.Close()
		if s.logger != nil {
			s.logger.Info("dashboard stopped")
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// newServer implements the §4.1 startup sequence up to (but not including)
// the listener: validate config, load the token store, parse the embedded
// template set, generate k_csrf, stand up the limiter/throttle/counters and
// register the §2.1 route table. Everything here fails loud; the caller
// (Serve, or a test) owns the listener.
func newServer(cfg Config, deps Deps) (*server, error) {
	cfg.normalize()
	if err := ValidateConfig(cfg); err != nil {
		return nil, err
	}
	if deps.Index == nil {
		return nil, errors.New("dashboard: Deps.Index is required")
	}
	if deps.Health == nil {
		return nil, errors.New("dashboard: Deps.Health is required")
	}
	if deps.Clock == nil {
		deps.Clock = wallClock
	}
	if deps.TokenFile != "" {
		// A caller may pass the token path through Deps (tests and the
		// composition root both do); Config is the fallback.
		cfg.TokenFile = deps.TokenFile
	}

	store, err := LoadTokenStore(cfg.TokenFile, deps.IngestionKeys)
	if err != nil {
		return nil, err
	}
	WarmGzipPool()
	pageSets, partials, err := parseTemplates()
	if err != nil {
		// A parse/define collision is a startup failure, not a 500 on every
		// page (SPEC-10 §2.7).
		return nil, err
	}
	assets, err := loadStatic()
	if err != nil {
		return nil, err
	}
	csrf, err := newCSRFEngine()
	if err != nil {
		return nil, err
	}

	logger := deps.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	s := &server{
		cfg:      cfg,
		deps:     deps,
		store:    store,
		mux:      http.NewServeMux(),
		pageSets: pageSets,
		partials: partials,
		csrf:     csrf,
		limiter:  newLimiter(cfg),
		throttle: newThrottle(cfg),
		assets:   assets,
		clock:    deps.Clock,
		logger:   logger,
		started:  deps.Clock(),
		loopback: isLoopbackAddr(cfg.Bind),
	}
	s.identity = identityFor(cfg, store)
	s.shed = newMemShedder(cfg.MemPressurePct, deps.Watermarks, deps.Clock)
	s.registerRoutes(s.mux)
	return s, nil
}

// now is the injectable clock (identical to Deps.Clock; tests drive every
// timestamp and throttle window through it).
func (s *server) now() time.Time { return s.clock() }

// writeError writes a dashError as the §2.1.3/§5 JSON body (with ts), plus the
// headers each status requires (Retry-After on 429, Allow on 404 of a known
// path, X-Trouble-Required-Scope on 403).
func (s *server) writeError(w http.ResponseWriter, r *http.Request, e *dashError) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	switch e.HTTP {
	case 429:
		ra := e.RetryAfter
		if ra < 1 {
			ra = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(ra)))
	case 403:
		if req := s.requiredScope(r); req != "" {
			w.Header().Set("X-Trouble-Required-Scope", string(req))
		}
	}
	body := errorBody{TS: s.now().UTC().Format(types.TsLayout)}
	body.Error.Code = string(e.Code)
	body.Error.Message = e.Message
	body.Detail = e.Detail
	body.Fragment = e.Fragment
	b, _ := json.Marshal(body)
	w.WriteHeader(e.HTTP)
	w.Write(b)
}

// requiredScope returns the scope the current route requires (empty when the
// route carries none), for the X-Trouble-Required-Scope header.
func (s *server) requiredScope(r *http.Request) types.Scope {
	if row, ok := s.matchRow(r.Method, r.URL.Path); ok {
		return row.Scope
	}
	return ""
}

// retryAfterSeconds is the integer seconds until the bucket refills, ≥1
// (SPEC-10 §2.8).
func retryAfterSeconds(n int) int {
	if n < 1 {
		return 1
	}
	return n
}
