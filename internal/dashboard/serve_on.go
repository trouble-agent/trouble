package dashboard

import (
	"context"
	"errors"
	"net"
	"net/http"

	"github.com/totalwindupflightsystems/trouble/internal/types"
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
	if s.logger != nil {
		s.logger.Info("dashboard listening", "addr", ln.Addr().String(), "identity", cfg.Identity, "loopback", s.loopback)
	}
	return serveOn(ctx, s, ln)
}

// serveOn is the shared serve/drain body: serve until ctx is cancelled, then
// drain in-flight requests for ≤drainTimeout and close. The dashboard is a
// reader, so there is no state to flush.
func serveOn(ctx context.Context, s *server, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s,
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
