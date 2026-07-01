package main

// rpc.go is the control-plane RPC surface the `cerberus` CLI (and the tray) talk
// to. Every method is capability/token-gated exactly like the original Status
// method: the caller presents the operator token (an Ed25519-signed bearer
// capability minted by daemon/auth), and each handler calls Authorize with the
// right it needs before touching any subsystem. Nothing here acts on ambient
// authority.
//
// The handlers are thin: they marshal already-composed daemon objects (the mesh
// fabric, the scheduler, the wazero executor, the durable eUTXO ledger, the
// durable CRDT engine, the auth issuer) into flat, gob-friendly response
// structs. The heavy lifting lives in the daemon packages; this file is the
// seam that exposes them to the CLI.

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/hash066/cerberus/daemon/ledger"
	"github.com/hash066/cerberus/daemon/lifecycle"
	"github.com/hash066/cerberus/daemon/mesh"
	"github.com/hash066/cerberus/daemon/metrics"
	"github.com/hash066/cerberus/daemon/scheduler"
	"github.com/hash066/cerberus/daemon/state"
	"github.com/hash066/cerberus/daemon/wasm"
)

// deviceInfo is a namespace device the daemon exposes over 9P. The ninep.Server
// keeps its device table private, so the composition layer records what it
// registered and hands the list to the RPC for `cerberus devices`.
type deviceInfo struct {
	Path       string
	Kind       string
	QuotaBytes uint64
}

// mintedToken is a record of a token this daemon issued via `caps mint` /
// `caps attenuate`, so `caps list` can show the operator what is outstanding and
// `caps revoke` can name one by id. Revocation state is authoritative in the
// issuer (durable); this registry is the human-facing catalogue.
type mintedToken struct {
	ID       string
	Subject  string
	Rights   []string
	Resource string
	Expiry   int64 // unix; 0 = no expiry
	Parent   string
}

// capRegistry tracks tokens minted through the RPC. It is in-memory (the issuer
// key is durable, so previously-minted tokens still verify/revoke after a
// restart, but this catalogue is rebuilt from empty). Concurrency-safe: the RPC
// server handles requests on many goroutines.
type capRegistry struct {
	mu     sync.Mutex
	tokens map[string]mintedToken
}

func newCapRegistry() *capRegistry { return &capRegistry{tokens: map[string]mintedToken{}} }

func (r *capRegistry) add(t mintedToken) {
	r.mu.Lock()
	r.tokens[t.ID] = t
	r.mu.Unlock()
}

func (r *capRegistry) list() []mintedToken {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]mintedToken, 0, len(r.tokens))
	for _, t := range r.tokens {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// DaemonRPC is the RPC service exposed to the CLI and tray. Every method
// requires a capability token, so the control socket is not an open backdoor.
// It borrows the already-composed daemon objects (no ambient state of its own
// beyond the minted-token catalogue).
type DaemonRPC struct {
	authz     *auth.Issuer
	lifecycle *lifecycle.Monitor
	fabric    *mesh.Fabric // may be nil if the mesh failed to compose
	sched     *scheduler.Scheduler
	exec      *wasm.Executor
	ledger    *ledger.Ledger
	crdt      *state.Engine
	metrics   *metrics.Metrics
	daemonDoc []byte // the CRDT doc id the daemon checkpoints belief state under

	devices []deviceInfo
	caps    *capRegistry

	profile string
	kernel  string
	started time.Time
}

// ---- status ---------------------------------------------------------------

type StatusRequest struct{ Token string }
type StatusResponse struct {
	Version   string
	State     string
	Subject   string
	Profile   string
	Kernel    string
	UptimeSec int64
	MeshUp    bool
	PeerCount int
	SelfPeer  string
	Balance   uint64
}

// Status reports daemon health, identity, and a few live rollups. Requires read.
func (d *DaemonRPC) Status(req *StatusRequest, resp *StatusResponse) error {
	claims, err := d.authz.Authorize(req.Token, "read", "")
	if err != nil {
		return fmt.Errorf("unauthorized: %w", err)
	}
	resp.Version = contract.ContractVersion
	resp.Subject = claims.Subject
	resp.Profile = d.profile
	resp.Kernel = d.kernel
	resp.UptimeSec = int64(time.Since(d.started).Seconds())
	st := d.lifecycle.State()
	resp.State = fmt.Sprintf("Running (Power: %v, Battery: %.1f%%)", st.Src, st.BatteryPct)
	resp.MeshUp = d.fabric != nil
	if d.fabric != nil {
		resp.PeerCount = len(d.fabric.Peers())
		resp.SelfPeer = hex.EncodeToString(peerBytes(d.fabric.PeerID()))
	}
	if d.ledger != nil {
		resp.Balance, _ = d.ledger.Balance(claims.Subject)
		if resp.Balance == 0 {
			resp.Balance, _ = d.ledger.Balance("operator")
		}
	}
	return nil
}

// ---- run (real compute) ---------------------------------------------------

type RunRequest struct {
	Token string
	// Component is the raw WASM bytes to execute. The CLI reads them from the
	// file argument and ships them here; the daemon executes with real wazero.
	Component []byte
	// On, if set, is the hex Ed25519 PeerID of a mesh worker to dispatch to.
	// Empty means run locally on this node.
	On string
}

type RunResponse struct {
	OK     bool
	Output string
	Error  string
	TaskID string
	Where  string // "local" or the peer id it ran on
	CID    string // content address of the executed component
	Remote bool
}

// Run dispatches a WASM workload and returns the real result. Requires exec.
//
// Local path (default): the daemon's wazero-backed executor runs the component
// now and resolves the promise — real execution, not a canned value.
// Remote path (--on <peer>): if the composed mesh can reach that peer, the task
// is dispatched over the capability-gated QUIC compute stream. The composed
// daemon does not itself register a worker-side compute handler, so a remote
// dispatch to a peer that is not serving compute returns a clear error rather
// than a fake success (maturity honesty).
func (d *DaemonRPC) Run(req *RunRequest, resp *RunResponse) error {
	claims, err := d.authz.Authorize(req.Token, "exec", "")
	if err != nil {
		return fmt.Errorf("unauthorized: %w", err)
	}
	if len(req.Component) == 0 {
		return fmt.Errorf("run: empty component (need WASM bytes)")
	}
	if !isWasmMagic(req.Component) {
		return fmt.Errorf("run: not a WASM module (bad magic header)")
	}

	// Content-address the component so the result is auditable and matches the
	// integrity model the workers use.
	if c, cerr := wasm.ComponentCID(req.Component); cerr == nil {
		resp.CID = c.String()
	}

	// TaskID: a time+subject id for traceability.
	taskID := taskIDFor(claims.Subject)
	resp.TaskID = hex.EncodeToString(taskID)

	task := contract.ComputeTask{TaskID: taskID, Component: req.Component}

	// Remote dispatch requested.
	if strings.TrimSpace(req.On) != "" {
		peer, perr := parsePeerID(req.On)
		if perr != nil {
			return fmt.Errorf("run --on: %w", perr)
		}
		if d.fabric == nil {
			return fmt.Errorf("run --on: mesh not composed on this daemon")
		}
		resp.Remote = true
		resp.Where = req.On
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		// In the shared-kernel demo model the cap handle travels in-band; the
		// worker's kernel is the authority. We present 0 (worker authorizes).
		result, rerr := d.fabric.RequestCompute(ctx, peer, task, contract.CapHandle(0))
		if rerr != nil {
			return fmt.Errorf("run --on %s: %w", short(req.On), rerr)
		}
		fillRunResult(resp, result)
		d.countExec()
		return nil
	}

	// Local dispatch: real wazero execution via the composed executor.
	if d.exec == nil {
		return fmt.Errorf("run: no executor composed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	h, derr := d.exec.Dispatch(ctx, task)
	if derr != nil {
		return fmt.Errorf("run: dispatch: %w", derr)
	}
	result, rerr := d.exec.Resolve(ctx, h)
	if rerr != nil {
		return fmt.Errorf("run: resolve: %w", rerr)
	}
	resp.Where = "local"
	fillRunResult(resp, result)
	d.countExec()
	return nil
}

func fillRunResult(resp *RunResponse, r contract.ComputeResult) {
	resp.OK = r.OK
	resp.Output = string(r.Output)
	resp.Error = r.Error
	if r.TaskID != nil {
		resp.TaskID = hex.EncodeToString(r.TaskID)
	}
}

func (d *DaemonRPC) countExec() {
	if d.metrics != nil {
		d.metrics.WasmExecsTotal.Inc()
	}
}

// ---- nodes ----------------------------------------------------------------

type NodesRequest struct{ Token string }
type NodeEntry struct {
	PeerID string
	Addr   string
	Self   bool
}
type NodesResponse struct {
	SelfPeer string
	Nodes    []NodeEntry
	MeshUp   bool
}

// Nodes lists the mesh peers this node is connected to (plus itself). Requires read.
func (d *DaemonRPC) Nodes(req *NodesRequest, resp *NodesResponse) error {
	if _, err := d.authz.Authorize(req.Token, "read", ""); err != nil {
		return fmt.Errorf("unauthorized: %w", err)
	}
	if d.fabric == nil {
		resp.MeshUp = false
		return nil
	}
	resp.MeshUp = true
	self := hex.EncodeToString(peerBytes(d.fabric.PeerID()))
	resp.SelfPeer = self
	resp.Nodes = append(resp.Nodes, NodeEntry{PeerID: self, Addr: "(this node)", Self: true})
	for _, p := range d.fabric.Peers() {
		resp.Nodes = append(resp.Nodes, NodeEntry{
			PeerID: hex.EncodeToString(peerBytes(p.ID)),
			Addr:   p.Addr,
		})
	}
	return nil
}

// ---- devices --------------------------------------------------------------

type DevicesRequest struct{ Token string }
type DeviceEntry struct {
	Path       string
	Kind       string
	QuotaBytes uint64
}
type DevicesResponse struct {
	Devices []DeviceEntry
}

// Devices lists the 9P namespace devices the daemon exposes. Requires read.
func (d *DaemonRPC) Devices(req *DevicesRequest, resp *DevicesResponse) error {
	if _, err := d.authz.Authorize(req.Token, "read", ""); err != nil {
		return fmt.Errorf("unauthorized: %w", err)
	}
	for _, dev := range d.devices {
		resp.Devices = append(resp.Devices, DeviceEntry{
			Path: dev.Path, Kind: dev.Kind, QuotaBytes: dev.QuotaBytes,
		})
	}
	sort.Slice(resp.Devices, func(i, j int) bool { return resp.Devices[i].Path < resp.Devices[j].Path })
	return nil
}

// ---- wallet ---------------------------------------------------------------

type WalletRequest struct {
	Token string
	Owner string // empty => the caller's subject, then "operator"
}
type WalletResponse struct {
	Owner       string
	Balance     uint64
	Enabled     bool
	TotalSupply uint64
}

// Wallet reports an owner's compute-credit balance from the durable eUTXO
// ledger, plus the ledger's total supply (the conservation invariant). Requires
// read.
func (d *DaemonRPC) Wallet(req *WalletRequest, resp *WalletResponse) error {
	claims, err := d.authz.Authorize(req.Token, "read", "")
	if err != nil {
		return fmt.Errorf("unauthorized: %w", err)
	}
	if d.ledger == nil {
		return fmt.Errorf("wallet: ledger not available")
	}
	owner := strings.TrimSpace(req.Owner)
	if owner == "" {
		owner = claims.Subject
	}
	bal, berr := d.ledger.Balance(owner)
	if berr != nil {
		return fmt.Errorf("wallet: balance: %w", berr)
	}
	// If the caller's own subject holds nothing, fall back to the operator wallet
	// so the default `cerberus wallet` shows the genesis credits.
	if bal == 0 && owner == claims.Subject && owner != "operator" {
		if ob, _ := d.ledger.Balance("operator"); ob > 0 {
			owner, bal = "operator", ob
		}
	}
	resp.Owner = owner
	resp.Balance = bal
	resp.Enabled = d.profile == "open_mesh"
	resp.TotalSupply, _ = d.ledger.TotalSupply()
	return nil
}

// ---- caps -----------------------------------------------------------------

type CapsMintRequest struct {
	Token    string
	Subject  string
	Rights   []string
	Resource string
	TTLSecs  int64
}
type CapsAttenuateRequest struct {
	Token    string
	Parent   string // the parent token to attenuate
	Rights   []string
	Resource string
	TTLSecs  int64
}
type CapsRevokeRequest struct {
	Token string
	ID    string
}
type CapsListRequest struct{ Token string }

type CapsMintResponse struct {
	Token   string
	ID      string
	Subject string
}
type CapsRevokeResponse struct {
	ID      string
	Revoked bool
}
type CapEntry struct {
	ID       string
	Subject  string
	Rights   []string
	Resource string
	Expiry   int64
	Parent   string
	Revoked  bool
}
type CapsListResponse struct {
	Caps []CapEntry
}

// CapsMint issues a new bearer token for a subject. Requires admin (minting
// authority is an admin act; the operator token carries admin).
func (d *DaemonRPC) CapsMint(req *CapsMintRequest, resp *CapsMintResponse) error {
	if _, err := d.authz.Authorize(req.Token, "admin", ""); err != nil {
		return fmt.Errorf("unauthorized (mint requires admin): %w", err)
	}
	if strings.TrimSpace(req.Subject) == "" {
		return fmt.Errorf("caps mint: subject required")
	}
	rights := req.Rights
	if len(rights) == 0 {
		rights = []string{"read"}
	}
	ttl := time.Duration(req.TTLSecs) * time.Second
	tok, err := d.authz.Mint(req.Subject, rights, req.Resource, ttl)
	if err != nil {
		return fmt.Errorf("caps mint: %w", err)
	}
	id := tokenID(tok)
	resp.Token = tok
	resp.ID = id
	resp.Subject = req.Subject
	d.caps.add(mintedToken{
		ID: id, Subject: req.Subject, Rights: rights, Resource: req.Resource,
		Expiry: expiryOf(ttl),
	})
	return nil
}

// CapsAttenuate derives a strictly narrower token from a parent. Requires admin.
func (d *DaemonRPC) CapsAttenuate(req *CapsAttenuateRequest, resp *CapsMintResponse) error {
	if _, err := d.authz.Authorize(req.Token, "admin", ""); err != nil {
		return fmt.Errorf("unauthorized (attenuate requires admin): %w", err)
	}
	if strings.TrimSpace(req.Parent) == "" {
		return fmt.Errorf("caps attenuate: parent token required")
	}
	rights := req.Rights
	if len(rights) == 0 {
		return fmt.Errorf("caps attenuate: at least one right to keep is required")
	}
	ttl := time.Duration(req.TTLSecs) * time.Second
	tok, err := d.authz.Attenuate(req.Parent, rights, req.Resource, ttl)
	if err != nil {
		return fmt.Errorf("caps attenuate: %w", err)
	}
	id := tokenID(tok)
	resp.Token = tok
	resp.ID = id
	d.caps.add(mintedToken{
		ID: id, Subject: "(attenuated)", Rights: rights, Resource: req.Resource,
		Expiry: expiryOf(ttl), Parent: tokenID(req.Parent),
	})
	return nil
}

// CapsRevoke revokes a token by id (durably, and gossiped mesh-wide via the
// issuer's OnRevoke hook). Requires admin.
func (d *DaemonRPC) CapsRevoke(req *CapsRevokeRequest, resp *CapsRevokeResponse) error {
	if _, err := d.authz.Authorize(req.Token, "admin", ""); err != nil {
		return fmt.Errorf("unauthorized (revoke requires admin): %w", err)
	}
	if strings.TrimSpace(req.ID) == "" {
		return fmt.Errorf("caps revoke: token id required")
	}
	if err := d.authz.Revoke(req.ID); err != nil {
		return fmt.Errorf("caps revoke: %w", err)
	}
	if d.metrics != nil {
		d.metrics.RevocationsTotal.Inc()
	}
	resp.ID = req.ID
	resp.Revoked = true
	return nil
}

// CapsList lists tokens this daemon minted, with their current revocation
// status. Requires read.
func (d *DaemonRPC) CapsList(req *CapsListRequest, resp *CapsListResponse) error {
	if _, err := d.authz.Authorize(req.Token, "read", ""); err != nil {
		return fmt.Errorf("unauthorized: %w", err)
	}
	for _, t := range d.caps.list() {
		resp.Caps = append(resp.Caps, CapEntry{
			ID: t.ID, Subject: t.Subject, Rights: t.Rights, Resource: t.Resource,
			Expiry: t.Expiry, Parent: t.Parent, Revoked: d.authz.IsRevoked(t.ID),
		})
	}
	return nil
}

// ---- conflicts ------------------------------------------------------------

type ConflictsListRequest struct {
	Token string
	Doc   string // hex doc id; empty => the daemon's belief doc
}
type ConflictCandidate struct {
	Actor string
	Value string
}
type ConflictEntry struct {
	Subject    string
	Candidates []ConflictCandidate
}
type ConflictsListResponse struct {
	Doc       string
	Conflicts []ConflictEntry
}

type ConflictsResolveRequest struct {
	Token   string
	Doc     string // hex doc id; empty => the daemon's belief doc
	Subject string
	Value   string
}
type ConflictsResolveResponse struct {
	Resolved bool
	Subject  string
}

// ConflictsList returns the open belief conflicts for a CRDT doc. Requires read.
func (d *DaemonRPC) ConflictsList(req *ConflictsListRequest, resp *ConflictsListResponse) error {
	if _, err := d.authz.Authorize(req.Token, "read", ""); err != nil {
		return fmt.Errorf("unauthorized: %w", err)
	}
	if d.crdt == nil {
		return fmt.Errorf("conflicts: CRDT engine not available")
	}
	doc, err := d.resolveDoc(req.Doc)
	if err != nil {
		return err
	}
	resp.Doc = hex.EncodeToString(doc)
	for _, c := range d.crdt.Conflicts(doc) {
		entry := ConflictEntry{Subject: c.Subject}
		for _, cand := range c.Candidates {
			entry.Candidates = append(entry.Candidates, ConflictCandidate{
				Actor: hex.EncodeToString(peerBytes(cand.Actor)),
				Value: string(cand.Value),
			})
		}
		resp.Conflicts = append(resp.Conflicts, entry)
	}
	return nil
}

// ConflictsResolve records a human decision that supersedes a belief conflict
// frontier (a causally-dominating write). Requires write.
func (d *DaemonRPC) ConflictsResolve(req *ConflictsResolveRequest, resp *ConflictsResolveResponse) error {
	claims, err := d.authz.Authorize(req.Token, "write", "")
	if err != nil {
		return fmt.Errorf("unauthorized (resolve requires write): %w", err)
	}
	if d.crdt == nil {
		return fmt.Errorf("conflicts: CRDT engine not available")
	}
	if strings.TrimSpace(req.Subject) == "" {
		return fmt.Errorf("conflicts resolve: subject required")
	}
	doc, derr := d.resolveDoc(req.Doc)
	if derr != nil {
		return derr
	}
	var resolver contract.PeerID
	copy(resolver[:], []byte(claims.Subject))
	ok, rerr := d.crdt.Resolve(doc, resolver, req.Subject, req.Value)
	if rerr != nil {
		return fmt.Errorf("conflicts resolve: %w", rerr)
	}
	resp.Resolved = ok
	resp.Subject = req.Subject
	return nil
}

func (d *DaemonRPC) resolveDoc(hexDoc string) ([]byte, error) {
	hexDoc = strings.TrimSpace(hexDoc)
	if hexDoc == "" {
		if len(d.daemonDoc) == 0 {
			return nil, fmt.Errorf("conflicts: no doc id given and no default belief doc")
		}
		return d.daemonDoc, nil
	}
	b, err := hex.DecodeString(hexDoc)
	if err != nil {
		// Allow a plain (non-hex) doc name too: use its raw bytes.
		return []byte(hexDoc), nil
	}
	return b, nil
}

// ---- helpers --------------------------------------------------------------

func peerBytes(p contract.PeerID) []byte { return append([]byte(nil), p[:]...) }

func parsePeerID(s string) (contract.PeerID, error) {
	var p contract.PeerID
	b, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return p, fmt.Errorf("peer id must be hex: %w", err)
	}
	if len(b) != len(p) {
		return p, fmt.Errorf("peer id must be %d bytes (%d hex chars), got %d", len(p), 2*len(p), len(b))
	}
	copy(p[:], b)
	return p, nil
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12] + "…"
	}
	return s
}

func isWasmMagic(b []byte) bool {
	return len(b) >= 4 && b[0] == 0x00 && b[1] == 0x61 && b[2] == 0x73 && b[3] == 0x6d
}

// tokenID extracts the claim id from a bearer token without needing the issuer
// key: the token is <base64url(payload)>.<base64url(sig)> and the payload is
// JSON with an "id" field. Used so the catalogue/revoke can name a token by id.
func tokenID(token string) string {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return ""
	}
	const key = "\"id\":\""
	s := string(payload)
	i := strings.Index(s, key)
	if i < 0 {
		return ""
	}
	rest := s[i+len(key):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return rest[:j]
}

func expiryOf(ttl time.Duration) int64 {
	if ttl <= 0 {
		return 0
	}
	return time.Now().Add(ttl).Unix()
}

func taskIDFor(subject string) []byte {
	now := time.Now().UnixNano()
	b := make([]byte, 0, 16)
	for i := 0; i < 8; i++ {
		b = append(b, byte(now>>(8*i)))
	}
	sum := byte(0)
	for i := 0; i < len(subject); i++ {
		sum += subject[i]
	}
	for i := 0; i < 8; i++ {
		b = append(b, sum+byte(i))
	}
	return b
}
