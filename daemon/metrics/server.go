package metrics

// server.go is the HTTP surface for metrics + health. It mirrors daemon/api's
// posture: bind localhost, gate the data endpoint (/metrics) behind a Bearer
// capability granting "read", and leave the probe endpoints (/healthz, /readyz)
// unauthenticated so an orchestrator/loopback probe can poll liveness and
// readiness without a token (matching api's unauthenticated /healthz).
//
// /metrics  — Prometheus text exposition (token-gated "read").
// /healthz  — liveness: 200 as long as the process serves HTTP.
// /readyz   — readiness: 200 only once the caller-supplied Ready func returns
//             true (e.g. mesh up, stores opened); 503 + reason otherwise.

import (
	"net/http"
	"strings"
	"time"

	"github.com/hash066/cerberus/daemon/auth"
)

// ContentType is the Prometheus text exposition format v0.0.4 content type.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// ReadyFunc reports whether the daemon is ready to serve. It returns a bool and,
// when not ready, a short human-readable reason for the probe body.
type ReadyFunc func() (ready bool, reason string)

// Server exposes /metrics, /healthz and /readyz.
type Server struct {
	reg   *Registry
	authz auth.Authorizer // nil => /metrics is open (localhost-only binds)
	ready ReadyFunc       // nil => always ready
}

// New builds the metrics server.
//   - reg:   the registry to expose (use Metrics.Registry).
//   - authz: capability verifier for /metrics; pass nil to leave /metrics open
//     (only do this on a localhost-only bind).
//   - ready: readiness predicate for /readyz; pass nil to report always-ready.
func New(reg *Registry, authz auth.Authorizer, ready ReadyFunc) *Server {
	return &Server{reg: reg, authz: authz, ready: ready}
}

// Handler returns the mux (also used directly in tests).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)
	mux.HandleFunc("/metrics", s.gate(s.handleMetrics))
	return mux
}

// Start serves on addr (blocking). Callers bind 127.0.0.1 to keep it local.
// Timeouts mirror daemon/gateway's Start: an unbounded server lets a slow or
// stalled client (even against the unauthenticated /healthz) pin a
// connection's goroutine/fd forever — enough of those exhaust the daemon and
// make liveness/readiness probes fail even though the daemon is otherwise fine.
func (s *Server) Start(addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	return srv.ListenAndServe()
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	ready, reason := true, ""
	if s.ready != nil {
		ready, reason = s.ready()
	}
	if !ready {
		w.WriteHeader(http.StatusServiceUnavailable)
		if reason == "" {
			reason = "not ready"
		}
		_, _ = w.Write([]byte("not ready: " + reason))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready"))
}

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	var b strings.Builder
	s.reg.Write(&b)
	w.Header().Set("Content-Type", ContentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}

// gate wraps a handler with capability auth (Bearer token granting "read") when
// an Authorizer is configured; when authz is nil the handler is open (the bind
// is localhost-only). Mirrors daemon/api.Server.requireRead.
func (s *Server) gate(next http.HandlerFunc) http.HandlerFunc {
	if s.authz == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		tok := auth.BearerToken(r.Header.Get("Authorization"))
		if tok == "" {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		if _, err := s.authz.Authorize(tok, "read", ""); err != nil {
			http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}
