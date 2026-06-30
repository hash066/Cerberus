// Package api is the daemon's local status API for the desktop tray/dashboard.
// It serves JSON over HTTP on localhost; every data endpoint requires a Bearer
// capability token granting "read" (the tray reads the operator token from
// disk). /healthz is unauthenticated for liveness probes. This is consumed by
// the Tauri webview — a native app, not a browser.
package api

import (
	"encoding/json"
	"net/http"

	"github.com/hash066/cerberus/daemon/auth"
)

// PowerView is the dashboard-facing power state.
type PowerView struct {
	Source     string  `json:"source"`
	BatteryPct float64 `json:"battery_pct"`
	Lid        string  `json:"lid"`
	Hint       string  `json:"hint"`
}

// Snapshot is the full state the dashboard renders.
type Snapshot struct {
	Version         string    `json:"version"`
	Profile         string    `json:"profile"`
	Kernel          string    `json:"kernel"`
	UptimeSec       int64     `json:"uptime_sec"`
	MeshUp          bool      `json:"mesh_up"`
	Peers           []string  `json:"peers"`
	OperatorBalance uint64    `json:"operator_balance"`
	Power           PowerView `json:"power"`
}

// Server exposes the status API.
type Server struct {
	authz    auth.Authorizer
	snapshot func() Snapshot
}

// New builds the API server. snapshot is called per request to produce live data.
func New(authz auth.Authorizer, snapshot func() Snapshot) *Server {
	return &Server{authz: authz, snapshot: snapshot}
}

// Handler returns the mux (also used directly in tests).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/api/v1/status", s.requireRead(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, s.snapshot())
	}))
	return mux
}

// Start serves the API on addr (blocking).
func (s *Server) Start(addr string) error {
	return http.ListenAndServe(addr, s.Handler())
}

// requireRead wraps a handler with capability auth (Bearer token granting "read").
func (s *Server) requireRead(next http.HandlerFunc) http.HandlerFunc {
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

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
