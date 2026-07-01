// Package api is the daemon's local status API for the desktop tray/dashboard.
// It serves JSON over HTTP on localhost; every data endpoint requires a Bearer
// capability token granting "read" (the tray reads the operator token from
// disk). /healthz is unauthenticated for liveness probes. This is consumed by
// the Tauri webview — a native app, not a browser.
//
// v3 Snapshot (additive, backward-compatible): alongside the original fields it
// now carries a device list, wallet balance, open belief-conflict count, a
// metrics summary (tasks placed, transfers, bytes), and per-subsystem health.
// The composition populates these from injected getters (closures) — the api
// package stays decoupled from state/metrics/economy so it can build and test
// standalone.
package api

import (
	"encoding/json"
	"net"
	"net/http"
	"time"

	"github.com/hash066/cerberus/daemon/auth"
)

// PowerView is the dashboard-facing power state.
type PowerView struct {
	Source     string  `json:"source"`
	BatteryPct float64 `json:"battery_pct"`
	Lid        string  `json:"lid"`
	Hint       string  `json:"hint"`
}

// Device is one machine bound into the mesh, as the dashboard lists it. This is
// richer than the bare Peers []string (which stays for backward compatibility):
// a device carries an id/label plus its coarse liveness and role.
type Device struct {
	ID       string `json:"id"`                  // peer id / stable identifier
	Addr     string `json:"addr,omitempty"`      // last-known address
	Kind     string `json:"kind,omitempty"`      // e.g. "self" | "peer"
	Online   bool   `json:"online"`              // reachable at snapshot time
	LastSeen int64  `json:"last_seen,omitempty"` // unix seconds; 0 = unknown
}

// Wallet is the dashboard-facing economy view for the operator principal.
// Balance duplicates the legacy OperatorBalance field; the richer struct lets
// the tray show pending/locked credits without another breaking change.
type Wallet struct {
	Principal string `json:"principal"`
	Balance   uint64 `json:"balance"`
	Locked    uint64 `json:"locked,omitempty"`  // credits escrowed in open settlements
	Pending   uint64 `json:"pending,omitempty"` // settlements awaiting finalization
}

// MetricsSummary is a compact rollup of the daemon's counters for the dashboard.
// It is a snapshot of monotonic totals, not a rate; the tray derives rates by
// diffing successive snapshots.
type MetricsSummary struct {
	TasksPlaced   uint64 `json:"tasks_placed"`   // scheduler placements
	Transfers     uint64 `json:"transfers"`      // data-plane transfers completed
	BytesMoved    uint64 `json:"bytes_moved"`    // bytes across the data plane
	TasksExecuted uint64 `json:"tasks_executed"` // components run to completion
	Errors        uint64 `json:"errors"`         // failed placements/transfers/execs
}

// Health is one subsystem's health as the dashboard renders it. Status is a
// coarse string ("ok" | "degraded" | "down") chosen by the composition.
type Health struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Snapshot is the full state the dashboard renders.
//
// The first block of fields is the original v1 shape (unchanged for backward
// compatibility). Everything below Devices is additive: an older tray ignoring
// the new fields still deserializes the same, and a v3 tray gets the richer
// view. All new fields are omitempty where a zero value is meaningfully "not
// provided".
type Snapshot struct {
	Version         string    `json:"version"`
	Profile         string    `json:"profile"`
	Kernel          string    `json:"kernel"`
	UptimeSec       int64     `json:"uptime_sec"`
	MeshUp          bool      `json:"mesh_up"`
	Peers           []string  `json:"peers"`
	OperatorBalance uint64    `json:"operator_balance"`
	Power           PowerView `json:"power"`

	// --- v3 additive fields ---

	// Devices is the richer machine list (Peers stays for old clients).
	Devices []Device `json:"devices,omitempty"`
	// Wallet is the operator's economy view (Balance mirrors OperatorBalance).
	Wallet Wallet `json:"wallet"`
	// BeliefConflicts is the count of open, unresolved CRDT belief conflicts
	// awaiting human resolution (ARCHITECTURE §1 principle 4).
	BeliefConflicts int `json:"belief_conflicts"`
	// Metrics is the counter rollup (tasks placed, transfers, bytes).
	Metrics MetricsSummary `json:"metrics"`
	// Subsystems is per-subsystem health for the dashboard's status column.
	Subsystems []Health `json:"subsystems,omitempty"`
}

// Getters is the set of closures the composition injects so the api package can
// assemble a Snapshot without importing state/metrics/economy/mesh. Every field
// is optional: a nil getter contributes a zero value, so a partially-wired
// daemon (or a standalone test) still produces a valid Snapshot. This is the
// explicit seam the LEAD wires in cmd/cerberusd.
type Getters struct {
	Version         func() string
	Profile         func() string
	Kernel          func() string
	UptimeSec       func() int64
	MeshUp          func() bool
	Peers           func() []string
	Devices         func() []Device
	OperatorBalance func() uint64
	Wallet          func() Wallet
	BeliefConflicts func() int
	Metrics         func() MetricsSummary
	Subsystems      func() []Health
	Power           func() PowerView
}

// Snapshot assembles a Snapshot from whichever getters are set. Nil getters are
// skipped (their fields stay zero). OperatorBalance is kept in sync with the
// Wallet balance: if only one of the two getters is provided the other is
// derived, so old and new clients see consistent numbers.
func (g Getters) Snapshot() Snapshot {
	s := Snapshot{}
	if g.Version != nil {
		s.Version = g.Version()
	}
	if g.Profile != nil {
		s.Profile = g.Profile()
	}
	if g.Kernel != nil {
		s.Kernel = g.Kernel()
	}
	if g.UptimeSec != nil {
		s.UptimeSec = g.UptimeSec()
	}
	if g.MeshUp != nil {
		s.MeshUp = g.MeshUp()
	}
	if g.Peers != nil {
		s.Peers = g.Peers()
	}
	if g.Devices != nil {
		s.Devices = g.Devices()
	}
	if g.OperatorBalance != nil {
		s.OperatorBalance = g.OperatorBalance()
	}
	if g.Wallet != nil {
		s.Wallet = g.Wallet()
	}
	if g.BeliefConflicts != nil {
		s.BeliefConflicts = g.BeliefConflicts()
	}
	if g.Metrics != nil {
		s.Metrics = g.Metrics()
	}
	if g.Subsystems != nil {
		s.Subsystems = g.Subsystems()
	}
	if g.Power != nil {
		s.Power = g.Power()
	}
	// Reconcile the legacy balance and the wallet so a client reading either is
	// consistent. Prefer an explicit wallet balance; otherwise mirror the legacy.
	if s.Wallet.Balance == 0 && s.OperatorBalance != 0 {
		s.Wallet.Balance = s.OperatorBalance
	}
	if s.OperatorBalance == 0 && s.Wallet.Balance != 0 {
		s.OperatorBalance = s.Wallet.Balance
	}
	if s.Wallet.Principal == "" {
		s.Wallet.Principal = "operator"
	}
	return s
}

// Server exposes the status API.
type Server struct {
	authz    auth.Authorizer
	snapshot func() Snapshot
}

// New builds the API server. snapshot is called per request to produce live
// data. Unchanged from v1 — the composition may pass any closure, including one
// built from Getters (see NewWithGetters).
func New(authz auth.Authorizer, snapshot func() Snapshot) *Server {
	return &Server{authz: authz, snapshot: snapshot}
}

// NewWithGetters builds the API server from an injected set of getters. It is a
// convenience over New for the v3 composition: the api package owns the
// assembly (keeping the Snapshot shape in one place) while the daemon supplies
// only closures. Equivalent to New(authz, g.Snapshot).
func NewWithGetters(authz auth.Authorizer, g Getters) *Server {
	return &Server{authz: authz, snapshot: g.Snapshot}
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

// Start serves the API on addr (blocking). Timeouts mirror daemon/gateway's
// Start: without them a client that opens a connection and never finishes
// sending (or reading) ties up a goroutine/fd indefinitely, which can starve
// this same process's /healthz — a trivial Slowloris would otherwise make a
// healthy node look dead to anything polling liveness.
func (s *Server) Start(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return s.Serve(ln)
}

// Serve is like Start but serves on an already-bound net.Listener instead of
// calling net.Listen internally, so a caller can bind the listener itself
// first, react to a bind failure (e.g. fall back to an ephemeral port on
// conflict), and log the real bound address. Start is a thin wrapper over
// this for backward compatibility. Timeouts mirror daemon/gateway's Start.
func (s *Server) Serve(ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	return srv.Serve(ln)
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
