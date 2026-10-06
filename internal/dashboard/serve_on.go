package dashboard

import (
	"context"
	"errors"
	"net"
	"net/http"

	"github.com/trouble-agent/trouble/internal/types"
)

// serve_on.go is Serve with a caller-owned listener.
//
// SPEC-12 §3.2 runs a bind preflight that *keeps* the resulting listener fds for
// the servers, precisely so there is no close-then-re-open race: between the
// check and the bind, another process can take the port, and the daemon would
// then serve nothing while reporting a successful preflight. The composition root
// therefore hands its preflight listener to ServeOn instead of letting the
// dashboard bind again.
func ServeOn(ctx context.Context, cfg Config, deps Deps, ln net.Listener) error {
	s, err := newServer(cfg, deps)
	if err != nil {
		return err
	}
	if ln == nil {
		return &dashError{Code: types.CodeLifecycle003, HTTP: 500, Message: "bind preflight returned no listener", Detail: "no_listener"}
	}
	if s.store.Refused() {
		// The boot refused the credential substrate (QA-TROUBLE-19): the
		// §2.1 data plane never serves. The health-only fallback keeps
		// /health.json alive — the external stall checker (SPEC-12 §3)
		// consumes it, and a dead-looking health surface would misreport
		// this designed refusal as a crashed daemon (SPEC-12 §6 edge 8).
		if s.logger != nil {
			s.logger.Info("dashboard listening (refused: token store missing; health-only)",
				"addr", ln.Addr().String(), "identity", cfg.Identity, "loopback", s.loopback)
		}
		return serveOn(ctx, s, &refusedOnlyServer{server: s}, ln)
	}
	if s.logger != nil {
		s.logger.Info("dashboard listening", "addr", ln.Addr().String(), "identity", cfg.Identity, "loopback", s.loopback)
	}
	return serveOn(ctx, s, s, ln)
}

// refusedOnlyServer serves exactly one route — GET /health.json, through the
// ordinary auth chain (the loopback exemption still applies) — and refuses
// every OTHER (method, path) with 503 + TROUBLE-DASHBOARD-013
// (detail "dashboard_refused") BEFORE any credential evaluation. It wraps the
// fully wired server after a QA-TROUBLE-19 boot refusal: the token store was
// missing, so the data plane must stay closed even to a valid token (the
// credential store that would authorize it was refused at boot), while the
// health surface keeps telling the watchdog what the boot refused.
type refusedOnlyServer struct {
	*server
}

func (r *refusedOnlyServer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodGet && req.URL.Path == "/health.json" {
		r.server.ServeHTTP(w, req)
		return
	}
	r.counters.denied.Add(1)
	r.writeError(w, req, &dashError{
		Code:    types.CodeDashboard013,
		HTTP:    http.StatusServiceUnavailable,
		Message: "dashboard refused at boot: token store missing",
		Detail:  "dashboard_refused",
	})
}

// serveOn is the shared serve/drain body: serve until ctx is cancelled, then
// drain in-flight requests for ≤drainTimeout and close. The dashboard is a
// reader, so there is no state to flush. The handler is passed separately from
// the server so the QA-TROUBLE-19 health-only wrapper can stand in for the
// §2.1 route table without the drain logs losing their server context.
func serveOn(ctx context.Context, s *server, h http.Handler, ln net.Listener) error {
	srv := &http.Server{
		Handler:           h,
		MaxHeaderBytes:    maxHeaderBytes,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		ReadHeaderTimeout: readTimeout,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
		defer cancel()
		if err := srv.Shutdown(shCtx); err != nil && s.logger != nil {
			s.logger.Warn("dashboard drain incomplete", "err", err)
		}
		_ = srv.Close()
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
