// Package node contains the v0.1 two-process E2E demo node used by
// cmd/cerberusd's hidden -e2e-node mode and the test/e2e harness.
//
// Transport split (Phase E2 — real cross-node compute over the mesh):
//   - HTTP is used ONLY for harness control and peer bootstrap: /peers and
//     /dispatch let the harness observe discovery and trigger a run, and
//     /discover is the bootstrap handshake that exchanges each node's libp2p
//     AddrInfo + mesh PeerID (mDNS multicast is unreliable in CI/loopback).
//   - The actual WASM exec dispatch runs over the real libp2p/QUIC mesh: the
//     requester content-addresses the component to a CID, dials the worker by
//     its Ed25519 PeerID, and sends a contract.ComputeTask carrying the CID over
//     a capability-gated QUIC stream (daemon/mesh.RequestCompute). The worker
//     verifies the presented capability against its own kernel, resolves the CID
//     against its content store (integrity-checked), runs the wasm via the real
//     wazero executor (daemon/wasm), and returns the result bytes over the mesh.
//
// There is no /run HTTP endpoint anymore: nothing about the exec crosses HTTP.
package node

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
	"github.com/hash066/cerberus/daemon/mesh"
	"github.com/hash066/cerberus/daemon/wasm"
	"github.com/ipfs/go-cid"
	ic "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

const (
	ReadyPrefix       = "CERBERUSD_E2E_READY "
	HelloShardExport  = "hello_shard"
	HelloShardValue   = 1337
	discoveryInterval = 100 * time.Millisecond
	meshSite          = "e2e"
)

var helloShardWASM = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
	0x01, 0x05, 0x01, 0x60, 0x00, 0x01, 0x7f,
	0x03, 0x02, 0x01, 0x00,
	0x07, 0x0f, 0x01, 0x0b, 0x68, 0x65, 0x6c, 0x6c, 0x6f, 0x5f, 0x73, 0x68, 0x61, 0x72, 0x64, 0x00, 0x00,
	0x0a, 0x07, 0x01, 0x05, 0x00, 0x41, 0xb9, 0x0a, 0x0b,
}

type Config struct {
	ID         string
	ListenAddr string
	PeerAddrs  []string
	Ready      io.Writer
	Log        io.Writer
}

// Peer is a discovered node. Addr is its HTTP control URL; MeshPeerID is the
// base64 Ed25519 mesh identity and MeshAddrs are its dialable libp2p p2p
// multiaddrs, used to connect and dispatch over the mesh. ExecCap is the exec
// capability this peer (as the resource owner) granted us so we can dispatch
// compute to it.
type Peer struct {
	ID         string             `json:"id"`
	Addr       string             `json:"addr"`
	MeshPeerID string             `json:"mesh_peer_id,omitempty"`
	MeshAddrs  []string           `json:"mesh_addrs,omitempty"`
	ExecCap    contract.CapHandle `json:"exec_cap,omitempty"`
}

type ReadyMessage struct {
	ID   string `json:"id"`
	Addr string `json:"addr"`
}

type PeersResponse struct {
	Self  Peer   `json:"self"`
	Peers []Peer `json:"peers"`
}

// DiscoverRequest is the bootstrap handshake body: the caller's HTTP+mesh
// coordinates plus an exec cap the caller grants the responder for dispatching
// compute back to the caller. The responder replies (PeersResponse.Self.ExecCap)
// with the symmetric grant. Bidirectional grants make dispatch work regardless
// of which node initiated discovery.
type DiscoverRequest struct {
	ID         string             `json:"id"`
	Addr       string             `json:"addr"`
	MeshPeerID string             `json:"mesh_peer_id"`
	MeshAddrs  []string           `json:"mesh_addrs"`
	ExecCap    contract.CapHandle `json:"exec_cap"`
}

type DispatchRequest struct {
	TargetID string `json:"target_id"`
	WasmPath string `json:"wasm_path"`
}

// RunResponse is the harness-facing result of a dispatch. The exec itself ran on
// the worker over the mesh; this is just how the requester reports it back over
// HTTP to the test harness.
type RunResponse struct {
	TaskID string `json:"task_id"`
	OK     bool   `json:"ok"`
	Value  int    `json:"value,omitempty"`
	Error  string `json:"error,omitempty"`
	PeerID string `json:"peer_id"`
}

type server struct {
	self        Peer
	initialPeer []string
	kernel      *stub.CapKernel
	fabric      *mesh.Fabric
	store       *wasm.ContentStore
	log         *log.Logger
	client      *http.Client

	mu    sync.Mutex
	peers map[string]Peer
}

func HelloShardWASM() []byte {
	return append([]byte(nil), helloShardWASM...)
}

func Run(ctx context.Context, cfg Config) error {
	if cfg.ID == "" {
		return errors.New("node ID is required")
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = "127.0.0.1:0"
	}
	if cfg.Ready == nil {
		cfg.Ready = io.Discard
	}
	if cfg.Log == nil {
		cfg.Log = io.Discard
	}

	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}

	logger := log.New(cfg.Log, "", log.LstdFlags|log.Lmicroseconds)

	// Real libp2p/QUIC mesh fabric. Every node runs its own capability kernel —
	// the worker's kernel is the authority that gates execution on it.
	kernel := stub.NewCapKernel()
	fab, err := mesh.New(ctx, mesh.Config{Site: meshSite, Kernel: kernel, EnableMDNS: false})
	if err != nil {
		return fmt.Errorf("mesh init: %w", err)
	}
	defer fab.Close()

	// Content-addressed component store: both demo nodes embed the hello-shard
	// bytes and register them by CID, so the worker resolves the task's CID
	// against the store (integrity-checked) rather than trusting wire bytes.
	cstore := wasm.NewContentStore()
	if _, err := cstore.Put(HelloShardWASM()); err != nil {
		return fmt.Errorf("seed content store: %w", err)
	}

	addr := "http://" + listener.Addr().String()
	node := &server{
		self: Peer{
			ID:         cfg.ID,
			Addr:       addr,
			MeshPeerID: encodePeerID(fab.PeerID()),
			MeshAddrs:  fab.DialableAddrs(),
		},
		initialPeer: normalizePeerAddrs(cfg.PeerAddrs),
		kernel:      kernel,
		fabric:      fab,
		store:       cstore,
		log:         logger,
		client:      &http.Client{Timeout: 2 * time.Second},
		peers:       map[string]Peer{},
	}

	// Worker side: serve capability-gated compute over the mesh.
	fab.ServeCompute(node.handleCompute)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", node.handleHealth)
	mux.HandleFunc("GET /peers", node.handlePeers)
	mux.HandleFunc("POST /discover", node.handleDiscover)
	mux.HandleFunc("POST /dispatch", node.handleDispatch)

	httpServer := &http.Server{Handler: mux}
	serveErr := make(chan error, 1)
	go func() {
		if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	ready, err := json.Marshal(ReadyMessage{ID: node.self.ID, Addr: node.self.Addr})
	if err != nil {
		return err
	}
	fmt.Fprintf(cfg.Ready, "%s%s\n", ReadyPrefix, ready)
	node.log.Printf("%s listening at %s (mesh peer %s)", node.self.ID, node.self.Addr, node.self.MeshPeerID)

	discoveryCtx, stopDiscovery := context.WithCancel(ctx)
	defer stopDiscovery()
	go node.discoveryLoop(discoveryCtx)

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return err
		}
		return nil
	case err := <-serveErr:
		return err
	}
}

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) handlePeers(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.peersResponse())
}

// handleDiscover is the bootstrap handshake. It records the caller (including the
// exec cap the caller granted us for dispatching back to it), connects to the
// caller over the mesh, and replies with this node's coordinates plus a symmetric
// exec capability minted on THIS node's kernel that authorizes the caller to
// dispatch compute here. Granting from the resource owner is the ocap model: each
// node (owner of its exec resource) hands the other the authority to use it.
func (s *server) handleDiscover(w http.ResponseWriter, r *http.Request) {
	var req DiscoverRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.ID == "" || req.Addr == "" {
		writeError(w, http.StatusBadRequest, errors.New("peer requires id and addr"))
		return
	}
	if err := s.addPeer(Peer{
		ID:         req.ID,
		Addr:       req.Addr,
		MeshPeerID: req.MeshPeerID,
		MeshAddrs:  req.MeshAddrs,
		ExecCap:    req.ExecCap, // cap the caller granted us
	}); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.connectMesh(r.Context(), req.MeshAddrs)

	// Grant the caller an exec capability on our kernel, scoped to our wasm-exec
	// resource. The caller presents it on dispatch; we verify it then.
	grant, err := s.grantExecCap()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.log.Printf("%s discovered %s; granted exec cap %d", s.self.ID, req.ID, grant)

	resp := s.peersResponse()
	resp.Self.ExecCap = grant
	writeJSON(w, http.StatusOK, resp)
}

// grantExecCap mints an exec capability on this node's kernel scoped to its
// wasm-exec resource. A peer presents the returned handle when dispatching
// compute here, and handleCompute verifies it against this same kernel.
func (s *server) grantExecCap() (contract.CapHandle, error) {
	return s.kernel.Mint(
		contract.ResourceRef{Kind: contract.KindGPU, Path: "/cer/e2e/wasm/" + s.self.ID},
		[]contract.Right{contract.RightExec},
		nil,
	)
}

// handleDispatch is triggered by the harness on the requester. It dispatches the
// component to the target worker OVER THE MESH (not HTTP): content-address the
// wasm to a CID, then send a ComputeTask carrying the CID and the worker-granted
// exec cap over a capability-gated QUIC stream.
func (s *server) handleDispatch(w http.ResponseWriter, r *http.Request) {
	var req DispatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.TargetID == "" {
		writeError(w, http.StatusBadRequest, errors.New("target_id is required"))
		return
	}
	worker, ok := s.peer(req.TargetID)
	if !ok {
		writeError(w, http.StatusPreconditionFailed, fmt.Errorf("peer %q has not been discovered", req.TargetID))
		return
	}
	if worker.MeshPeerID == "" {
		writeError(w, http.StatusPreconditionFailed, fmt.Errorf("peer %q has no mesh identity yet", req.TargetID))
		return
	}
	if worker.ExecCap == 0 {
		writeError(w, http.StatusPreconditionFailed, fmt.Errorf("peer %q has not granted an exec capability yet", req.TargetID))
		return
	}

	component, err := os.ReadFile(req.WasmPath)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	// Content-address the component. The worker will resolve this CID against its
	// own store and verify integrity before running — the bytes never need to
	// cross the wire (both nodes embed the hello-shard).
	c, err := s.store.Put(component)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	workerPID, err := decodePeerID(worker.MeshPeerID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	taskID := fmt.Sprintf("%s-to-%s-%d", s.self.ID, worker.ID, time.Now().UnixNano())
	task := contract.ComputeTask{
		TaskID:    []byte(taskID),
		Component: c.Bytes(), // the CID, not the wasm bytes
	}

	res, err := s.fabric.RequestCompute(r.Context(), workerPID, task, worker.ExecCap)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("mesh dispatch to %s: %w", worker.ID, err))
		return
	}

	runResp := RunResponse{TaskID: taskID, OK: res.OK, PeerID: worker.ID}
	if res.OK {
		v, convErr := strconv.Atoi(strings.TrimSpace(string(res.Output)))
		if convErr != nil {
			runResp.OK = false
			runResp.Error = fmt.Sprintf("worker returned non-integer output %q: %v", string(res.Output), convErr)
		} else {
			runResp.Value = v
		}
	} else {
		runResp.Error = res.Error
	}
	s.log.Printf("%s dispatched task=%s to %s over mesh ok=%t value=%d", s.self.ID, taskID, worker.ID, runResp.OK, runResp.Value)

	status := http.StatusOK
	if !runResp.OK {
		status = http.StatusBadGateway
	}
	writeJSON(w, status, runResp)
}

// handleCompute is the worker side of the mesh compute round-trip. It verifies
// the presented capability against this node's kernel (real gate — no ambient
// authority), resolves the task's component CID against the content store with
// an integrity check, then runs the wasm via the real wazero executor.
func (s *server) handleCompute(ctx context.Context, task contract.ComputeTask, capH contract.CapHandle) (contract.ComputeResult, error) {
	taskID := string(task.TaskID)

	// 1. Authorize: the capability must be valid on OUR kernel for an exec.
	if capH == 0 {
		return failResult(task.TaskID, "missing exec capability"), nil
	}
	if err := s.kernel.Verify(capH, contract.Request{Op: "exec"}, time.Now().Unix()); err != nil {
		return failResult(task.TaskID, fmt.Sprintf("capability denied: %v", err)), nil
	}

	// 2. Resolve the component CID against the content store (integrity-checked).
	c, err := cid.Cast(task.Component)
	if err != nil {
		return failResult(task.TaskID, fmt.Sprintf("invalid component CID: %v", err)), nil
	}
	component, err := s.store.Get(c)
	if err != nil {
		return failResult(task.TaskID, err.Error()), nil
	}

	// 3. Run the resolved wasm via the real wazero engine.
	value, err := ExecuteHelloShard(component)
	if err != nil {
		s.log.Printf("%s ran task=%s FAILED: %v", s.self.ID, taskID, err)
		return failResult(task.TaskID, err.Error()), nil
	}
	s.log.Printf("%s ran task=%s cid=%s value=%d (over mesh)", s.self.ID, taskID, c, value)
	return contract.ComputeResult{
		TaskID: task.TaskID,
		OK:     true,
		Output: []byte(strconv.Itoa(value)),
	}, nil
}

func (s *server) discoveryLoop(ctx context.Context) {
	ticker := time.NewTicker(discoveryInterval)
	defer ticker.Stop()
	for {
		for _, addr := range s.initialPeer {
			if err := s.register(ctx, addr); err != nil {
				s.log.Printf("%s discovery with %s failed: %v", s.self.ID, addr, err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *server) register(ctx context.Context, addr string) error {
	// Grant the peer an exec cap so it can dispatch back to us (symmetric grant).
	grant, err := s.grantExecCap()
	if err != nil {
		return err
	}
	var resp PeersResponse
	body := DiscoverRequest{
		ID:         s.self.ID,
		Addr:       s.self.Addr,
		MeshPeerID: s.self.MeshPeerID,
		MeshAddrs:  s.self.MeshAddrs,
		ExecCap:    grant,
	}
	if err := s.postJSON(ctx, addr+"/discover", body, &resp); err != nil {
		return err
	}
	// resp.Self carries the exec cap the peer granted us; keep it.
	_ = s.addPeer(resp.Self)
	s.connectMesh(ctx, resp.Self.MeshAddrs)
	for _, peer := range resp.Peers {
		_ = s.addPeer(peer)
		s.connectMesh(ctx, peer.MeshAddrs)
	}
	return nil
}

// connectMesh dials a peer's libp2p p2p multiaddrs so a later mesh
// Dial/RequestCompute has a route. The addrs all share one PeerID; we merge them
// into a single AddrInfo and connect once. Errors are best-effort (the next
// discovery tick retries).
func (s *server) connectMesh(ctx context.Context, meshAddrs []string) {
	if len(meshAddrs) == 0 {
		return
	}
	var merged *peer.AddrInfo
	for _, a := range meshAddrs {
		ai, err := peer.AddrInfoFromString(a)
		if err != nil {
			s.log.Printf("%s parse mesh addr %q failed: %v", s.self.ID, a, err)
			continue
		}
		if merged == nil {
			merged = ai
		} else if merged.ID == ai.ID {
			merged.Addrs = append(merged.Addrs, ai.Addrs...)
		}
	}
	if merged == nil {
		return
	}
	// Never dial ourselves (a peers-list echo can include our own addr).
	if selfPID, err := decodePeerID(s.self.MeshPeerID); err == nil {
		if libp2pSelf, err := selfLibp2pID(selfPID); err == nil && libp2pSelf == merged.ID {
			return
		}
	}
	cctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := s.fabric.Connect(cctx, *merged); err != nil {
		s.log.Printf("%s mesh connect to %s failed: %v", s.self.ID, merged.ID, err)
	}
}

// addPeer merges a discovered peer, preserving any exec cap already granted to us
// (a later peers-list echo that lacks the cap must not clobber it).
func (s *server) addPeer(p Peer) error {
	if p.ID == "" || p.Addr == "" {
		return errors.New("peer requires id and addr")
	}
	if p.ID == s.self.ID {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.peers[p.ID]; ok {
		if p.ExecCap == 0 {
			p.ExecCap = existing.ExecCap
		}
		if p.MeshPeerID == "" {
			p.MeshPeerID = existing.MeshPeerID
		}
		if len(p.MeshAddrs) == 0 {
			p.MeshAddrs = existing.MeshAddrs
		}
	}
	s.peers[p.ID] = p
	return nil
}

func (s *server) peer(id string) (Peer, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	peer, ok := s.peers[id]
	return peer, ok
}

func (s *server) peersResponse() PeersResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	peers := make([]Peer, 0, len(s.peers))
	for _, peer := range s.peers {
		peers = append(peers, peer)
	}
	return PeersResponse{Self: s.self, Peers: peers}
}

func (s *server) postJSON(ctx context.Context, url string, req any, resp any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpResp, err := s.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		payload, _ := io.ReadAll(io.LimitReader(httpResp.Body, 4096))
		return fmt.Errorf("%s returned %s: %s", url, httpResp.Status, strings.TrimSpace(string(payload)))
	}
	return json.NewDecoder(httpResp.Body).Decode(resp)
}

func failResult(taskID []byte, msg string) contract.ComputeResult {
	return contract.ComputeResult{TaskID: taskID, OK: false, Error: msg}
}

func encodePeerID(p contract.PeerID) string {
	return base64.StdEncoding.EncodeToString(p[:])
}

// selfLibp2pID derives the libp2p peer.ID for a raw Ed25519 contract.PeerID so
// connectMesh can recognize (and skip) a dial to ourselves.
func selfLibp2pID(p contract.PeerID) (peer.ID, error) {
	pub, err := ic.UnmarshalEd25519PublicKey(p[:])
	if err != nil {
		return "", err
	}
	return peer.IDFromPublicKey(pub)
}

func decodePeerID(s string) (contract.PeerID, error) {
	var id contract.PeerID
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return id, fmt.Errorf("decode mesh peer id: %w", err)
	}
	if len(raw) != len(id) {
		return id, fmt.Errorf("mesh peer id has length %d, want %d", len(raw), len(id))
	}
	copy(id[:], raw)
	return id, nil
}

func normalizePeerAddrs(addrs []string) []string {
	normalized := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			continue
		}
		addr = strings.TrimRight(addr, "/")
		if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
			addr = "http://" + addr
		}
		normalized = append(normalized, addr)
	}
	return normalized
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
