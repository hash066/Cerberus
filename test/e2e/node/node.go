// Package node contains the v0.1 two-process E2E demo node used by
// cmd/cerberusd's hidden -e2e-node mode and the test/e2e harness.
package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
)

const (
	ReadyPrefix       = "CERBERUSD_E2E_READY "
	HelloShardExport  = "hello_shard"
	HelloShardValue   = 1337
	discoveryInterval = 100 * time.Millisecond
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

type Peer struct {
	ID   string `json:"id"`
	Addr string `json:"addr"`
}

type ReadyMessage struct {
	ID   string `json:"id"`
	Addr string `json:"addr"`
}

type PeersResponse struct {
	Self  Peer   `json:"self"`
	Peers []Peer `json:"peers"`
}

type DispatchRequest struct {
	TargetID string `json:"target_id"`
	WasmPath string `json:"wasm_path"`
}

type RunRequest struct {
	TaskID    string             `json:"task_id"`
	Component string             `json:"component"`
	Wasm      []byte             `json:"wasm"`
	Cap       contract.CapHandle `json:"cap"`
}

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

	addr := "http://" + listener.Addr().String()
	node := &server{
		self:        Peer{ID: cfg.ID, Addr: addr},
		initialPeer: normalizePeerAddrs(cfg.PeerAddrs),
		kernel:      stub.NewCapKernel(),
		log:         log.New(cfg.Log, "", log.LstdFlags|log.Lmicroseconds),
		client:      &http.Client{Timeout: 2 * time.Second},
		peers:       map[string]Peer{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", node.handleHealth)
	mux.HandleFunc("GET /peers", node.handlePeers)
	mux.HandleFunc("POST /discover", node.handleDiscover)
	mux.HandleFunc("POST /dispatch", node.handleDispatch)
	mux.HandleFunc("POST /run", node.handleRun)

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
	node.log.Printf("%s listening at %s", node.self.ID, node.self.Addr)

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

func (s *server) handleDiscover(w http.ResponseWriter, r *http.Request) {
	var peer Peer
	if err := json.NewDecoder(r.Body).Decode(&peer); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.addPeer(peer); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.log.Printf("%s discovered %s at %s", s.self.ID, peer.ID, peer.Addr)
	writeJSON(w, http.StatusOK, s.peersResponse())
}

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

	wasm, err := os.ReadFile(req.WasmPath)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	capHandle, err := s.kernel.Mint(
		contract.ResourceRef{Kind: contract.KindGPU, Path: "/cer/demo/wasm/" + worker.ID},
		[]contract.Right{contract.RightExec},
		nil,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.kernel.Verify(capHandle, contract.Request{Op: "exec"}, time.Now().Unix()); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}

	taskID := fmt.Sprintf("%s-to-%s-%d", s.self.ID, worker.ID, time.Now().UnixNano())
	runReq := RunRequest{
		TaskID:    taskID,
		Component: filepath.Base(req.WasmPath),
		Wasm:      wasm,
		Cap:       capHandle,
	}
	var runResp RunResponse
	if err := s.postJSON(r.Context(), worker.Addr+"/run", runReq, &runResp); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	status := http.StatusOK
	if !runResp.OK {
		status = http.StatusBadGateway
	}
	writeJSON(w, status, runResp)
}

func (s *server) handleRun(w http.ResponseWriter, r *http.Request) {
	var req RunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Component != "hello-shard.wasm" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("unsupported component %q", req.Component))
		return
	}
	if req.Cap == 0 {
		writeError(w, http.StatusForbidden, errors.New("missing exec capability"))
		return
	}

	value, err := ExecuteHelloShard(req.Wasm)
	resp := RunResponse{TaskID: req.TaskID, OK: err == nil, PeerID: s.self.ID, Value: value}
	if err != nil {
		resp.Error = err.Error()
	}
	s.log.Printf("%s ran %s task=%s ok=%t value=%d", s.self.ID, req.Component, req.TaskID, resp.OK, resp.Value)
	writeJSON(w, http.StatusOK, resp)
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
	var resp PeersResponse
	if err := s.postJSON(ctx, addr+"/discover", s.self, &resp); err != nil {
		return err
	}
	_ = s.addPeer(resp.Self)
	for _, peer := range resp.Peers {
		_ = s.addPeer(peer)
	}
	return nil
}

func (s *server) addPeer(peer Peer) error {
	if peer.ID == "" || peer.Addr == "" {
		return errors.New("peer requires id and addr")
	}
	if peer.ID == s.self.ID {
		return nil
	}
	s.mu.Lock()
	s.peers[peer.ID] = peer
	s.mu.Unlock()
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
