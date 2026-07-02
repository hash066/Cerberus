// This file adds the v3 dashboard action routes: belief-conflict list/resolve,
// namespace device listing, workload history, capability revoke, and device
// grant. Each route is backed by the SAME underlying subsystem calls the
// control-plane RPC (cmd/cerberusd/rpc.go's DaemonRPC.ConflictsList/
// ConflictsResolve/Devices/CapsRevoke/CapsMint) already uses — this package
// stays decoupled from state/mesh/ninep/auth's issuer internals by taking
// those calls as injected Getters/Actions closures, exactly like Snapshot
// does for /api/v1/status.
//
// Wire shapes here are pinned to what tray/src/main.js already parses (it was
// written first, against the intended-but-until-now-unserved routes) — see
// that file's refreshDevices/refreshWorkloads/refreshConflicts for the exact
// field names this package must not drift from:
//
//	GET  /api/v1/devices           -> [{path, kind, rights}]
//	GET  /api/v1/workloads         -> [{id, model, node, state}]
//	GET  /api/v1/conflicts         -> [{subject, candidates}]   (values is also
//	                                   accepted by the tray but candidates is
//	                                   what this daemon actually emits)
//	POST /api/v1/conflicts/resolve  {subject, winning} -> {resolved, subject}
//	POST /api/v1/cap/revoke         {id}               -> {id, revoked}
//	POST /api/v1/devices/grant      {path, rights}      -> {token, id, subject}
package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/hash066/cerberus/daemon/auth"
)

// NamespaceDevice is one entry in the /api/v1/devices listing: a generic view
// over whatever is registered in the 9P namespace (daemon/ninep), so a new
// device kind (e.g. the audio endpoints daemon/system.Compose now registers)
// shows up automatically without a matching change here. Field names/JSON
// tags match exactly what tray/src/main.js's refreshDevices already expects.
type NamespaceDevice struct {
	Path   string   `json:"path"`
	Kind   string   `json:"kind"`
	Rights []string `json:"rights"`
}

// WorkloadEntry is one row in the /api/v1/workloads history. Field names/JSON
// tags match tray/src/main.js's refreshWorkloads.
type WorkloadEntry struct {
	ID    string `json:"id"`
	Model string `json:"model"`
	Node  string `json:"node"`
	State string `json:"state"`
}

// WalletTxView is one row of the /api/v1/wallet transaction list — a recorded
// compute run and its notional price. Beta records usage only (no credits move).
type WalletTxView struct {
	ID       uint64 `json:"id"`
	TaskID   string `json:"task_id"`
	Model    string `json:"model,omitempty"`
	Consumer string `json:"consumer"`
	Provider string `json:"provider"`
	Amount   uint64 `json:"amount"`
	UnixTime int64  `json:"unix_time"`
	State    string `json:"state"`
}

// WalletView is the /api/v1/wallet payload: an owner's compute-credit balance
// plus recent compute transactions (the ledger's append-only usage log). The
// list does not affect Balance — value transfer is off for beta.
type WalletView struct {
	Owner        string         `json:"owner"`
	Balance      uint64         `json:"balance"`
	TotalSupply  uint64         `json:"total_supply"`
	Transactions []WalletTxView `json:"transactions"`
}

// ConflictCandidateView is one candidate value for a belief-conflict subject.
type ConflictCandidateView struct {
	Actor string `json:"actor,omitempty"`
	Value string `json:"value"`
}

// ConflictView is one open belief-conflict. tray/src/main.js reads either
// `values` or `candidates` (it checks both key names); this daemon emits
// `candidates`.
type ConflictView struct {
	Subject    string                  `json:"subject"`
	Candidates []ConflictCandidateView `json:"candidates"`
}

// ResolveConflictRequest is the POST /api/v1/conflicts/resolve body, matching
// the tray's resolve_conflict command (`{subject, winning}`).
type ResolveConflictRequest struct {
	Subject string `json:"subject"`
	Winning string `json:"winning"`
}

// ResolveConflictResponse is returned by /api/v1/conflicts/resolve.
type ResolveConflictResponse struct {
	Resolved bool   `json:"resolved"`
	Subject  string `json:"subject"`
}

// RevokeRequest is the POST /api/v1/cap/revoke body, matching the tray's
// revoke_capability command (`{id}`).
type RevokeRequest struct {
	ID string `json:"id"`
}

// RevokeResponse is returned by /api/v1/cap/revoke.
type RevokeResponse struct {
	ID      string `json:"id"`
	Revoked bool   `json:"revoked"`
}

// GrantDeviceRequest is the POST /api/v1/devices/grant body, matching the
// tray's grant_device command (`{path, rights}`). Rights is a single
// comma/space-separated string (mirroring how the tray's UI collects it as
// one free-text field) or a JSON array of strings; ParseRights below accepts
// either encoding so the daemon behaves the same regardless of how the tray
// serializes the field.
type GrantDeviceRequest struct {
	Path   string          `json:"path"`
	Rights json.RawMessage `json:"rights"`
}

// GrantDeviceResponse is returned by /api/v1/devices/grant.
type GrantDeviceResponse struct {
	Token   string `json:"token"`
	ID      string `json:"id"`
	Subject string `json:"subject"`
}

// ParseRights decodes GrantDeviceRequest.Rights, which may arrive either as a
// JSON array (["read","alloc"]) or a bare JSON string ("read, alloc") — the
// tray's grant_device command forwards whatever the user typed in a single
// text field, so the wire encoding of that field is not guaranteed to be an
// array. Falls back to ["read"] if nothing usable is present.
func ParseRights(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return []string{"read"}
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil {
		out := make([]string, 0, len(arr))
		for _, r := range arr {
			r = strings.TrimSpace(r)
			if r != "" {
				out = append(out, r)
			}
		}
		if len(out) > 0 {
			return out
		}
		return []string{"read"}
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		var out []string
		for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
			part = strings.TrimSpace(part)
			if part != "" {
				out = append(out, part)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return []string{"read"}
}

// Actions is the set of write-side closures the composition injects for the
// action routes (conflict resolve, cap revoke, device grant). Mirrors
// Getters' role for the read-side Snapshot/list routes: the api package stays
// decoupled from daemon/state, daemon/auth's issuer, and daemon/ninep by
// taking these as plain function values. A nil entry makes the corresponding
// route respond 501 Not Implemented instead of panicking, so a partially
// wired daemon (or a standalone test exercising only some routes) is still
// safe to serve.
type Actions struct {
	// ResolveConflict resolves a belief-conflict subject to winning, mirroring
	// DaemonRPC.ConflictsResolve's write-gated call into daemon/state.
	ResolveConflict func(subject, winning string) (bool, error)
	// RevokeCap revokes a capability by id, mirroring DaemonRPC.CapsRevoke's
	// admin-gated call into the auth issuer.
	RevokeCap func(id string) (bool, error)
	// GrantDevice mints a capability scoped to a device path with the given
	// rights, mirroring DaemonRPC.CapsMint's admin-gated call into the auth
	// issuer. Returns the minted token, its id, and the subject it was minted
	// for.
	GrantDevice func(path string, rights []string) (token, id, subject string, err error)
}

// ListGetters is the read-side closures for the new listing routes
// (conflicts, devices, workloads). Kept separate from the existing Getters
// struct (which assembles the single /api/v1/status Snapshot) since these
// back their own top-level list routes rather than fields of Snapshot.
type ListGetters struct {
	Conflicts func() []ConflictView
	Devices   func() []NamespaceDevice
	Workloads func() []WorkloadEntry
	Wallet    func() WalletView
}

// WithActions attaches the write-side Actions to a Server built by New or
// NewWithGetters. Call once at composition time before Serve.
func (s *Server) WithActions(a Actions) *Server {
	s.actions = a
	return s
}

// WithListGetters attaches the read-side ListGetters to a Server built by New
// or NewWithGetters. Call once at composition time before Serve.
func (s *Server) WithListGetters(g ListGetters) *Server {
	s.lists = g
	return s
}

// registerActionRoutes wires the five new routes onto mux. Split out of
// Handler for readability; behaviourally identical to registering them
// inline.
func (s *Server) registerActionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/conflicts", s.requireRead(s.handleConflictsList))
	// ConflictsResolve requires "write" on the RPC (DaemonRPC.ConflictsResolve);
	// mirror that exactly here.
	mux.HandleFunc("/api/v1/conflicts/resolve", s.requireWrite(s.handleConflictsResolve))
	mux.HandleFunc("/api/v1/devices", s.requireRead(s.handleDevicesList))
	mux.HandleFunc("/api/v1/workloads", s.requireRead(s.handleWorkloadsList))
	mux.HandleFunc("/api/v1/wallet", s.requireRead(s.handleWallet))
	// CapsRevoke and CapsMint both require "admin" on the RPC; mirror that
	// exactly here rather than the weaker "write" (the operator token carries
	// "admin" so this is not a behavior change for the tray's only credential,
	// but it keeps this route no more permissive than its RPC twin).
	mux.HandleFunc("/api/v1/cap/revoke", s.requireAdmin(s.handleCapRevoke))
	mux.HandleFunc("/api/v1/devices/grant", s.requireAdmin(s.handleDeviceGrant))
}

func (s *Server) handleConflictsList(w http.ResponseWriter, _ *http.Request) {
	var out []ConflictView
	if s.lists.Conflicts != nil {
		out = s.lists.Conflicts()
	}
	if out == nil {
		out = []ConflictView{}
	}
	writeJSON(w, out)
}

func (s *Server) handleWallet(w http.ResponseWriter, _ *http.Request) {
	var out WalletView
	if s.lists.Wallet != nil {
		out = s.lists.Wallet()
	}
	if out.Transactions == nil {
		out.Transactions = []WalletTxView{}
	}
	writeJSON(w, out)
}

func (s *Server) handleDevicesList(w http.ResponseWriter, _ *http.Request) {
	var out []NamespaceDevice
	if s.lists.Devices != nil {
		out = s.lists.Devices()
	}
	if out == nil {
		out = []NamespaceDevice{}
	}
	writeJSON(w, out)
}

func (s *Server) handleWorkloadsList(w http.ResponseWriter, _ *http.Request) {
	var out []WorkloadEntry
	if s.lists.Workloads != nil {
		out = s.lists.Workloads()
	}
	if out == nil {
		out = []WorkloadEntry{}
	}
	writeJSON(w, out)
}

func (s *Server) handleConflictsResolve(w http.ResponseWriter, r *http.Request) {
	if s.actions.ResolveConflict == nil {
		http.Error(w, "conflict resolution not available", http.StatusNotImplemented)
		return
	}
	var req ResolveConflictRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "malformed request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Subject) == "" {
		http.Error(w, "subject is required", http.StatusBadRequest)
		return
	}
	resolved, err := s.actions.ResolveConflict(req.Subject, req.Winning)
	if err != nil {
		http.Error(w, "resolve: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, ResolveConflictResponse{Resolved: resolved, Subject: req.Subject})
}

func (s *Server) handleCapRevoke(w http.ResponseWriter, r *http.Request) {
	if s.actions.RevokeCap == nil {
		http.Error(w, "capability revocation not available", http.StatusNotImplemented)
		return
	}
	var req RevokeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "malformed request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.ID) == "" {
		http.Error(w, "capability id is required", http.StatusBadRequest)
		return
	}
	revoked, err := s.actions.RevokeCap(req.ID)
	if err != nil {
		http.Error(w, "revoke: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, RevokeResponse{ID: req.ID, Revoked: revoked})
}

func (s *Server) handleDeviceGrant(w http.ResponseWriter, r *http.Request) {
	if s.actions.GrantDevice == nil {
		http.Error(w, "device grant not available", http.StatusNotImplemented)
		return
	}
	var req GrantDeviceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "malformed request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Path) == "" {
		http.Error(w, "device path is required", http.StatusBadRequest)
		return
	}
	rights := ParseRights(req.Rights)
	token, id, subject, err := s.actions.GrantDevice(req.Path, rights)
	if err != nil {
		http.Error(w, "grant: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, GrantDeviceResponse{Token: token, ID: id, Subject: subject})
}

// requireWrite wraps a handler with capability auth (Bearer token granting
// "write"). Mirrors requireRead exactly except for the right it demands — the
// POST action routes mutate state (resolve a conflict, revoke/mint a
// capability) so a bare "read" token must not satisfy them.
func (s *Server) requireWrite(next http.HandlerFunc) http.HandlerFunc {
	return s.requireRight("write", next)
}

// requireAdmin wraps a handler with capability auth (Bearer token granting
// "admin"), matching the RPC gate DaemonRPC.CapsRevoke/CapsMint already use
// for the equivalent control-plane calls.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return s.requireRight("admin", next)
}

// requireRight is the shared implementation behind requireRead/requireWrite/
// requireAdmin: it demands a Bearer token that Authorize grants `right`
// against (auth.Claims.allows treats "admin" as satisfying any right, so an
// operator token — minted with only "admin" — passes every one of these).
func (s *Server) requireRight(right string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := auth.BearerToken(r.Header.Get("Authorization"))
		if tok == "" {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		if _, err := s.authz.Authorize(tok, right, ""); err != nil {
			http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}
