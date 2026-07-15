package llama

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/hash066/cerberus/daemon/auth"
	"github.com/hash066/cerberus/daemon/mesh"
)

// worker.go is the WORKER half of the integration seam: it registers this node as
// a llama.cpp offload target for peers holding a RightExec capability.
//
// It mirrors gpu.WireWorker (daemon/gpu/worker.go) deliberately — same shape, same
// self-issuer resolver, same revocation predicate — so there is one pattern to
// learn for "expose a local resource to the mesh under a capability".

// WorkerConfig configures the mesh llama worker registered on a Fabric.
type WorkerConfig struct {
	// Site scopes the capability resource (LlamaRPCResource).
	Site string
	// Revoked is consulted on every session open. May be nil.
	Revoked auth.RevocationPredicate
	// Threads maps to ggml-rpc-server's -t. Zero uses upstream's default.
	Threads int
	// Device maps to -d (e.g. "Vulkan0"). Empty auto-detects.
	Device string
	// Cache enables ggml-rpc-server's -c tensor file cache.
	Cache bool
	// Logf receives worker lifecycle lines. Nil uses the standard logger.
	Logf func(format string, args ...any)
}

// WireWorker registers ServeLlamaRPC on fab so remote peers can offload llama.cpp
// tensor work to this node after presenting a signed RightExec capability.
//
// THIS GRANTS CODE EXECUTION. It must only be called when the operator has
// explicitly opted in (cerberusd -llama-worker / the tray toggle), and the UX that
// offers it must say so. ggml-rpc's deserializer trusts its peer, so a peer that
// holds a valid capability and chooses to be malicious can compromise this
// process. The capability gate narrows WHO can try; it does not make trying safe.
// See doc.go.
//
// It returns ErrPackMissing if no version-gated llama.cpp pack is installed, so a
// caller can offer to fetch one rather than treat it as fatal.
func WireWorker(fab *mesh.Fabric, cfg WorkerConfig) error {
	if fab == nil {
		return fmt.Errorf("llama: nil fabric")
	}
	if cfg.Site == "" {
		cfg.Site = "local"
	}
	logf := cfg.Logf
	if logf == nil {
		logf = log.Printf
	}

	// Locate applies the CVE floor. A worker is never registered against binaries
	// we could not prove are patched.
	bins, err := Locate(context.Background())
	if err != nil {
		return err
	}

	be := &worker{
		rpc: NewRPCServer(bins, RPCServerConfig{
			Threads: cfg.Threads,
			Device:  cfg.Device,
			Cache:   cfg.Cache,
		}),
		build: bins.Build,
		logf:  logf,
	}

	fab.ServeLlamaRPC(be, mesh.SelfIssuerResolver, func() int64 { return time.Now().Unix() }, cfg.Revoked)

	logf("llama: worker ENABLED — peers holding a signed RightExec capability for %q may run "+
		"llama.cpp tensor work on this node (llama.cpp b%d). This grants code execution to those "+
		"peers; the capability gate limits who can connect, it does not sandbox them.",
		mesh.LlamaRPCResource(cfg.Site).Path, bins.Build)
	return nil
}

// worker implements mesh.LlamaBackend over a supervised ggml-rpc-server.
type worker struct {
	rpc   *RPCServer
	build int
	logf  func(string, ...any)

	mu     sync.Mutex
	closed bool
}

func (w *worker) Build() int { return w.build }

// OpenLocal admits one session: it applies the grant's bounds, spawns/locates the
// local ggml-rpc-server, and dials it on loopback.
func (w *worker) OpenLocal(ctx context.Context, grant auth.Grant) (io.ReadWriteCloser, error) {
	// Bounds the grant asks for that this path cannot honour are refused, not
	// ignored — see quota.go.
	ttl, err := sessionTTL(grant)
	if err != nil {
		return nil, err
	}

	addr, err := w.rpc.OpenLocal(ctx)
	if err != nil {
		return nil, err
	}

	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		w.rpc.CloseLocal()
		return nil, fmt.Errorf("llama: dial local ggml-rpc-server at %s: %w", addr, err)
	}

	w.logf("llama: offload session OPEN (worker on %s, llama.cpp b%d)", addr, w.build)
	sc := &sessionConn{Conn: conn, w: w}

	if ttl != noSessionLimit {
		// The grant bounded the session's wall-clock life; honour it by cutting the
		// connection, which tears down the worker via sessionConn.Close.
		t := time.AfterFunc(ttl, func() {
			w.logf("llama: offload session hit its %s capability quota — closing", ttl)
			_ = sc.Close()
		})
		sc.timer = t
	}
	return sc, nil
}

// sessionConn ties the mesh session's lifetime to the ggml-rpc-server child: when
// the session ends, the worker process dies. That is unconditional by design — it
// holds the peer's tensors in memory and has no session-reset concept, so reusing
// it across peers would leak one peer's residency into the next.
type sessionConn struct {
	net.Conn
	w     *worker
	timer *time.Timer
	once  sync.Once
}

func (s *sessionConn) Close() error {
	err := s.Conn.Close()
	s.once.Do(func() {
		if s.timer != nil {
			s.timer.Stop()
		}
		s.w.rpc.CloseLocal()
		s.w.logf("llama: offload session CLOSED — ggml-rpc-server terminated")
	})
	return err
}
