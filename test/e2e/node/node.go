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
	"crypto/ed25519"
	"crypto/rand"
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
	"github.com/hash066/cerberus/daemon/auth"
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
	// SkipHelloShardSeed, if true, does NOT pre-populate this node's content
	// store with the hello-shard bytes. It exists so a test can prove the
	// mesh peer-component-fetch path is real: a node built with this set must
	// fetch the component from a peer over the mesh rather than already
	// having it cached locally. The production e2e demo (test/e2e) leaves
	// this false, matching its historical both-nodes-embed-the-bytes setup.
	SkipHelloShardSeed bool
}

// Peer is a discovered node. Addr is its HTTP control URL; MeshPeerID is the
// base64 Ed25519 mesh identity and MeshAddrs are its dialable libp2p p2p
// multiaddrs, used to connect and dispatch over the mesh.
//
// IssuerPub is the base64 Ed25519 ISSUER public key this peer signs its
// capabilities under — the out-of-band trust anchor. It is exchanged at discovery
// (NOT read from an untrusted envelope) and is the key the other node uses to
// auth.Verify a signed cap this peer minted.
//
// ExecCapEnvelope is the Ed25519-signed exec capability this peer (as the resource
// owner) granted us: a self-verifying token, not an opaque handle into a shared
// kernel. We present it back when we dispatch compute here, and the peer verifies
// it against IssuerPub. ExecCap is the peer's in-process handle for the same
// grant, kept only for the belt-and-suspenders kernel check.
type Peer struct {
	ID              string             `json:"id"`
	Addr            string             `json:"addr"`
	MeshPeerID      string             `json:"mesh_peer_id,omitempty"`
	MeshAddrs       []string           `json:"mesh_addrs,omitempty"`
	IssuerPub       string             `json:"issuer_pub,omitempty"`
	ExecCap         contract.CapHandle `json:"exec_cap,omitempty"`
	ExecCapEnvelope []byte             `json:"exec_cap_envelope,omitempty"`
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
// coordinates, its ISSUER public key (the out-of-band trust anchor), and a SIGNED
// exec cap the caller grants the responder for dispatching compute back to the
// caller. The responder replies (PeersResponse.Self.ExecCapEnvelope + IssuerPub)
// with the symmetric grant. Bidirectional grants make dispatch work regardless of
// which node initiated discovery.
type DiscoverRequest struct {
	ID              string             `json:"id"`
	Addr            string             `json:"addr"`
	MeshPeerID      string             `json:"mesh_peer_id"`
	MeshAddrs       []string           `json:"mesh_addrs"`
	IssuerPub       string             `json:"issuer_pub"`
	ExecCap         contract.CapHandle `json:"exec_cap"`
	ExecCapEnvelope []byte             `json:"exec_cap_envelope"`
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

	// signer mints Ed25519-signed capability envelopes on THIS node's issuer key;
	// issuerPub is the matching public key and issuerID is its PeerID form, shipped
	// to peers at discovery as the out-of-band trust anchor.
	signer    *auth.SignedCap
	issuerPub ed25519.PublicKey
	issuerID  contract.PeerID

	mu          sync.Mutex
	peers       map[string]Peer
	trustedKeys map[contract.PeerID]ed25519.PublicKey // issuer PeerID -> exchanged pubkey
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

	// Per-node ISSUER key custody: an ephemeral in-memory Ed25519 key this node
	// signs its capability envelopes under. Each node has its OWN issuer key, so a
	// cap minted here is verifiable by a peer ONLY via the public key we hand it at
	// discovery — the cross-kernel, zero-trust property (no shared kernel).
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return fmt.Errorf("issuer seed: %w", err)
	}
	keys, err := auth.NewMemoryKeyStore(seed)
	if err != nil {
		return fmt.Errorf("issuer keystore: %w", err)
	}
	signer := auth.NewSignedCap(keys)
	issuerPub, err := keys.PublicKey()
	if err != nil {
		return fmt.Errorf("issuer pubkey: %w", err)
	}
	issuerID, err := signer.IssuerPeerID()
	if err != nil {
		return fmt.Errorf("issuer id: %w", err)
	}

	// Content-addressed component store: the worker resolves a task's CID
	// against the store (integrity-checked) rather than trusting wire bytes. By
	// default both demo nodes embed the hello-shard bytes and register them by
	// CID up front (the historical v0.1 setup). SkipHelloShardSeed lets a test
	// build a node that starts WITHOUT the bytes, to prove the peer-fetch path
	// below is what actually supplies them.
	cstore := wasm.NewContentStore()
	if !cfg.SkipHelloShardSeed {
		if _, err := cstore.Put(HelloShardWASM()); err != nil {
			return fmt.Errorf("seed content store: %w", err)
		}
	}

	addr := "http://" + listener.Addr().String()
	node := &server{
		self: Peer{
			ID:         cfg.ID,
			Addr:       addr,
			MeshPeerID: encodePeerID(fab.PeerID()),
			MeshAddrs:  fab.DialableAddrs(),
			IssuerPub:  base64.StdEncoding.EncodeToString(issuerPub),
		},
		initialPeer: normalizePeerAddrs(cfg.PeerAddrs),
		kernel:      kernel,
		fabric:      fab,
		store:       cstore,
		log:         logger,
		client:      &http.Client{Timeout: 2 * time.Second},
		signer:      signer,
		issuerPub:   issuerPub,
		issuerID:    issuerID,
		peers:       map[string]Peer{},
		trustedKeys: map[contract.PeerID]ed25519.PublicKey{},
	}
	// Trust our own issuer key (we are the resource owner that mints the exec cap
	// for this node, so we also Verify it under our own key on the worker side).
	node.trustedKeys[issuerID] = issuerPub

	// Worker side: serve compute over the mesh, gated by a CROSS-KERNEL SIGNED
	// capability. The worker Verifies the Ed25519 envelope in the task against the
	// issuer key it exchanged at discovery (resolveIssuer) BEFORE any wasm runs —
	// the cap is cryptographically trusted, not an opaque shared-kernel handle. The
	// grant must convey RightExec.
	fab.ServeComputeSigned(
		node.handleComputeSigned,
		node.resolveIssuerKey,
		func() int64 { return time.Now().Unix() },
		auth.RevocationPredicateFromIssuer(nil), // no revocation store wired in the demo
		contract.RightExec,
	)
	// Answer peer component-fetch requests from our own local content store, so
	// another node whose local cidstore misses can fetch the real bytes from us
	// (see fetchComponentFromPeers, called from handleComputeSigned on a miss).
	fab.ServeComponentFetch(cstore)

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

// handleDiscover is the bootstrap handshake. It records the caller (its issuer
// pubkey — the trust anchor — and the signed exec cap it granted us for
// dispatching back to it), connects to the caller over the mesh, and replies with
// this node's coordinates, this node's issuer pubkey, and a symmetric SIGNED exec
// capability that authorizes the caller to dispatch compute here. Granting from
// the resource owner is the ocap model: each node (owner of its exec resource)
// hands the other a self-verifying token plus the key to verify it.
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
	// Record the caller's issuer pubkey as a trusted anchor keyed by its PeerID.
	if err := s.trustIssuer(req.IssuerPub); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("bad issuer pubkey: %w", err))
		return
	}
	if err := s.addPeer(Peer{
		ID:              req.ID,
		Addr:            req.Addr,
		MeshPeerID:      req.MeshPeerID,
		MeshAddrs:       req.MeshAddrs,
		IssuerPub:       req.IssuerPub,
		ExecCap:         req.ExecCap,         // handle the caller granted us
		ExecCapEnvelope: req.ExecCapEnvelope, // signed cap the caller granted us
	}); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.connectMesh(r.Context(), req.MeshAddrs)

	// Grant the caller a SIGNED exec capability scoped to our wasm-exec resource.
	// The caller presents the envelope on dispatch; we Verify it against our issuer
	// pubkey then.
	handle, env, err := s.grantExecCap()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.log.Printf("%s discovered %s; granted signed exec cap (%d bytes, handle %d)", s.self.ID, req.ID, len(env), handle)

	resp := s.peersResponse()
	resp.Self.ExecCap = handle
	resp.Self.ExecCapEnvelope = env
	writeJSON(w, http.StatusOK, resp)
}

// grantExecCap mints an exec capability scoped to this node's wasm-exec resource
// in TWO forms: (1) an Ed25519-SIGNED envelope on this node's issuer key — the
// wire authority a peer presents on dispatch and this node Verifies against its
// own (exchanged) issuer pubkey; (2) an in-process kernel handle for the
// belt-and-suspenders kernel check. The two describe the same grant.
func (s *server) grantExecCap() (contract.CapHandle, []byte, error) {
	res := contract.ResourceRef{Kind: contract.KindGPU, Path: "/cer/e2e/wasm/" + s.self.ID}
	handle, err := s.kernel.Mint(res, []contract.Right{contract.RightExec}, nil)
	if err != nil {
		return 0, nil, err
	}
	g, err := auth.NewGrant(res, []contract.Right{contract.RightExec}, nil, time.Hour)
	if err != nil {
		return 0, nil, err
	}
	env, err := s.signer.Issue(g) // stamps our issuer PeerID + signs
	if err != nil {
		return 0, nil, err
	}
	return handle, env, nil
}

// trustIssuer records a peer's base64 Ed25519 issuer pubkey as a trust anchor,
// keyed by its PeerID (which, for Ed25519, is the key bytes). resolveIssuerKey
// consults this map when Verifying a signed cap that peer minted.
func (s *server) trustIssuer(b64 string) error {
	if b64 == "" {
		return errors.New("empty issuer pubkey")
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return err
	}
	if len(raw) != ed25519.PublicKeySize {
		return fmt.Errorf("issuer pubkey has length %d, want %d", len(raw), ed25519.PublicKeySize)
	}
	var id contract.PeerID
	copy(id[:], raw)
	s.mu.Lock()
	s.trustedKeys[id] = ed25519.PublicKey(append([]byte(nil), raw...))
	s.mu.Unlock()
	return nil
}

// resolveIssuerKey is the mesh IssuerPubResolver: it returns the trusted public
// key for an issuer PeerID we exchanged at discovery, or ok=false so the worker
// rejects a cap from an issuer it never met.
func (s *server) resolveIssuerKey(issuer contract.PeerID) (ed25519.PublicKey, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pub, ok := s.trustedKeys[issuer]
	return pub, ok
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
	if len(worker.ExecCapEnvelope) == 0 {
		writeError(w, http.StatusPreconditionFailed, fmt.Errorf("peer %q has not granted a signed exec capability yet", req.TargetID))
		return
	}
	workerIssuerID, err := decodeIssuerID(worker.IssuerPub)
	if err != nil {
		writeError(w, http.StatusPreconditionFailed, fmt.Errorf("peer %q has no usable issuer key: %w", req.TargetID, err))
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
	// Carry the worker-granted SIGNED exec cap in the task's cap slot; the worker
	// Verifies it (against the issuer key it exchanged) before running anything.
	task := contract.ComputeTask{
		TaskID:    []byte(taskID),
		Component: c.Bytes(), // the CID, not the wasm bytes
		Caps:      [][]byte{worker.ExecCapEnvelope},
	}

	res, err := s.fabric.RequestComputeSigned(r.Context(), workerPID, task, workerIssuerID, worker.ExecCap)
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

// handleComputeSigned is the worker side of the mesh compute round-trip. By the
// time it runs, mesh has ALREADY cryptographically verified the signed capability
// envelope against the issuer key we exchanged at discovery (Ed25519 signature +
// validity window + RightExec), so grant is a trusted authority we did NOT mint
// in a shared kernel — the zero-trust cross-kernel property this whole change
// delivers. This handler resolves the task's component CID against the content
// store (integrity-checked) and runs the wasm via the real wazero executor. The
// signed grant is now the authority; there is no ambient authority and no opaque
// shared-kernel handle in the trust decision.
func (s *server) handleComputeSigned(ctx context.Context, task contract.ComputeTask, grant auth.Grant) (contract.ComputeResult, error) {
	taskID := string(task.TaskID)
	_ = grant // authority already verified by the mesh signed-cap gate

	// Resolve the component CID against the content store (integrity-checked).
	c, err := cid.Cast(task.Component)
	if err != nil {
		return failResult(task.TaskID, fmt.Sprintf("invalid component CID: %v", err)), nil
	}
	component, err := s.store.Get(c)
	if err != nil {
		// Local miss: try fetching the bytes from a known mesh peer before giving
		// up. This is the real p2p fetch path (daemon/mesh.RequestComponent) —
		// the peer's returned bytes are re-hashed and verified against c before
		// they are trusted, then cached locally so a later lookup is a local hit.
		fetched, ferr := s.fetchComponentFromPeers(ctx, c)
		if ferr != nil {
			return failResult(task.TaskID, fmt.Sprintf("%v (peer fetch also failed: %v)", err, ferr)), nil
		}
		component = fetched
	}

	// Run the resolved wasm via the real wazero engine.
	value, err := ExecuteHelloShard(component)
	if err != nil {
		s.log.Printf("%s ran task=%s FAILED: %v", s.self.ID, taskID, err)
		return failResult(task.TaskID, err.Error()), nil
	}
	s.log.Printf("%s ran task=%s cid=%s value=%d (signed cap verified, over mesh)", s.self.ID, taskID, c, value)
	return contract.ComputeResult{
		TaskID: task.TaskID,
		OK:     true,
		Output: []byte(strconv.Itoa(value)),
	}, nil
}

// fetchComponentFromPeers is the "missing-component" fallback: it tries every
// currently-known mesh peer, in turn, asking each for the bytes behind c via
// daemon/mesh.RequestComponent (which itself verifies the returned bytes hash
// to c before returning them — a peer cannot hand back a substituted
// payload). The first peer that has it wins; the bytes are then cached in
// this node's own content store so a subsequent lookup for the same CID is a
// local hit. If no known peer has it, a clear aggregate error is returned —
// never a hang or a panic.
func (s *server) fetchComponentFromPeers(ctx context.Context, c cid.Cid) ([]byte, error) {
	if s.fabric == nil {
		return nil, fmt.Errorf("no mesh fabric composed on this node")
	}
	candidates := s.peerIDs()
	if len(candidates) == 0 {
		return nil, fmt.Errorf("component %s not found locally and no mesh peers are known", c)
	}

	var errs []string
	for _, p := range candidates {
		fctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		b, err := s.fabric.RequestComponent(fctx, p.pid, c)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", p.id, err))
			continue
		}
		// Populate the local store so future lookups for this CID are local hits.
		if _, perr := s.store.Put(b); perr != nil {
			errs = append(errs, fmt.Sprintf("%s: fetched but failed to cache: %v", p.id, perr))
			continue
		}
		s.log.Printf("%s fetched component %s from peer %s over the mesh (%d bytes)", s.self.ID, c, p.id, len(b))
		return b, nil
	}
	return nil, fmt.Errorf("component %s not found on any of %d known peer(s): %s", c, len(candidates), strings.Join(errs, "; "))
}

// peerCandidate pairs a peer's human id with its mesh PeerID, for logging.
type peerCandidate struct {
	id  string
	pid contract.PeerID
}

// peerIDs returns the mesh PeerIDs of all currently-known peers that have
// completed discovery (i.e. have a usable mesh identity).
func (s *server) peerIDs() []peerCandidate {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]peerCandidate, 0, len(s.peers))
	for _, p := range s.peers {
		if p.MeshPeerID == "" {
			continue
		}
		pid, err := decodePeerID(p.MeshPeerID)
		if err != nil {
			continue
		}
		out = append(out, peerCandidate{id: p.ID, pid: pid})
	}
	return out
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
	// Grant the peer a SIGNED exec cap so it can dispatch back to us (symmetric
	// grant), and ship our issuer pubkey as the trust anchor.
	handle, env, err := s.grantExecCap()
	if err != nil {
		return err
	}
	var resp PeersResponse
	body := DiscoverRequest{
		ID:              s.self.ID,
		Addr:            s.self.Addr,
		MeshPeerID:      s.self.MeshPeerID,
		MeshAddrs:       s.self.MeshAddrs,
		IssuerPub:       s.self.IssuerPub,
		ExecCap:         handle,
		ExecCapEnvelope: env,
	}
	if err := s.postJSON(ctx, addr+"/discover", body, &resp); err != nil {
		return err
	}
	// resp.Self carries the peer's issuer pubkey and the signed exec cap it granted
	// us; record the trust anchor and keep the peer.
	if resp.Self.IssuerPub != "" {
		if terr := s.trustIssuer(resp.Self.IssuerPub); terr != nil {
			return fmt.Errorf("trust %s issuer key: %w", resp.Self.ID, terr)
		}
	}
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

// addPeer merges a discovered peer, preserving any grant already recorded (a
// later peers-list echo that lacks the cap/key must not clobber it).
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
		if len(p.ExecCapEnvelope) == 0 {
			p.ExecCapEnvelope = existing.ExecCapEnvelope
		}
		if p.IssuerPub == "" {
			p.IssuerPub = existing.IssuerPub
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

// decodeIssuerID converts a peer's base64 Ed25519 issuer pubkey to its PeerID
// form (the key bytes), which is what the signed grant names as its Issuer and
// what RequestComputeSigned ships for the worker's key lookup.
func decodeIssuerID(b64 string) (contract.PeerID, error) {
	var id contract.PeerID
	if b64 == "" {
		return id, errors.New("peer has no issuer pubkey")
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return id, fmt.Errorf("decode issuer pubkey: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return id, fmt.Errorf("issuer pubkey has length %d, want %d", len(raw), ed25519.PublicKeySize)
	}
	copy(id[:], raw)
	return id, nil
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
