package llama

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"
)

// loopbackHost is the ONLY address ggml-rpc-server is ever bound to.
//
// This is a deliberate constant and not a configuration field. ggml-rpc's
// deserializer trusts its peer, and CVE-2026-34159 was an unauthenticated pre-auth
// RCE reachable by anyone who could open a TCP connection to this port. Binding it
// to 0.0.0.0 would re-expose exactly that, and would make the Cerberus capability
// gate ceremonial: an attacker would simply skip the mesh and talk to the port.
//
// The port is reachable ONLY through a mesh stream that already passed the
// capability gate (see daemon/mesh/llamarpc.go). There is no supported way to
// change this host, and rpcserver_test.go asserts it.
const loopbackHost = "127.0.0.1"

// ErrBusy is returned when a second concurrent offload session is attempted.
var ErrBusy = errors.New("busy: llama offload session already active")

// ErrPackMissing signals that no llama.cpp pack is installed.
var ErrPackMissing = errors.New("llama pack not installed")

// RPCServerConfig configures a ggml-rpc-server child process.
//
// There is deliberately NO Host field — see loopbackHost.
type RPCServerConfig struct {
	// Port to bind on loopback. Zero means "pick a free one" (see reservePort);
	// upstream REJECTS -p 0, so Cerberus resolves the port itself.
	Port int
	// Threads maps to -t. Zero omits the flag (upstream defaults to
	// max(1, hardware_concurrency/2)).
	Threads int
	// Device maps to -d (comma-separated). Empty omits the flag (auto-detect).
	Device string
	// Cache enables -c, ggml-rpc-server's local tensor file cache. Worth enabling:
	// the main node pushes tensors over the wire at load time, and the cache lets a
	// repeat run skip that transfer.
	Cache bool
}

// rpcServerArgs builds the ggml-rpc-server argv.
//
// Verified against upstream tools/rpc/rpc-server.cpp at b10021:
//
//	-H, --host HOST      host to bind to      (default 127.0.0.1)
//	-p, --port PORT      port to bind to      (default 50052)
//	-t, --threads N      number of threads for the CPU device
//	-d, --device <list>  comma-separated list of devices
//	-c, --cache          enable local file cache
//
// There is NO -m/--mem flag; it was removed upstream. Do not add one back.
//
// -H 127.0.0.1 is passed EXPLICITLY even though it is also upstream's default.
// Relying on a default for a security property is how security properties get
// lost: a future upstream change to that default, or a distro patch, would
// silently expose an RCE-prone port to the LAN. Stating it costs one argument.
func rpcServerArgs(cfg RPCServerConfig) []string {
	args := []string{
		"-H", loopbackHost,
		"-p", strconv.Itoa(cfg.Port),
	}
	if cfg.Threads > 0 {
		args = append(args, "-t", strconv.Itoa(cfg.Threads))
	}
	if cfg.Device != "" {
		args = append(args, "-d", cfg.Device)
	}
	if cfg.Cache {
		args = append(args, "-c")
	}
	return args
}

// reservePort asks the OS for a free loopback port, then releases it.
//
// Upstream rejects -p 0 outright — rpc-server.cpp validates
// `if (params.port <= 0 || params.port > 65535)` — so "let the server pick" is not
// available and Cerberus must name a concrete port.
//
// This carries an unavoidable TOCTOU race: between Close() here and the child's
// bind(), another process could take the port. The child then fails to bind and
// exits, Start reports it, and the session is refused — a loud, recoverable
// failure, not a silent misbind. Binding loopback-only keeps the blast radius to
// this machine.
func reservePort() (int, error) {
	l, err := net.Listen("tcp", net.JoinHostPort(loopbackHost, "0"))
	if err != nil {
		return 0, fmt.Errorf("llama: reserve loopback port: %w", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		return 0, fmt.Errorf("llama: release reserved port %d: %w", port, err)
	}
	return port, nil
}

// RPCServer supervises ONE ggml-rpc-server child.
//
// ADMISSION CONTROL — why this is a hard single slot and not a queue or a mux:
//
//   - ggml-rpc-server serves ONE client at a time. Upstream is explicit that
//     serving clients with separate privilege levels is out of scope, so
//     multiplexing two Cerberus peers onto one process is not something the
//     protocol supports and would corrupt both sessions.
//   - Queueing is worse than refusing. A queued peer sits on an open socket
//     producing no bytes while llama.cpp's client side runs its own timeouts; the
//     peer sees an opaque stall instead of a clear "busy". Refusing immediately is
//     information the caller can act on.
//
// So: one process per session, refuse the second, and kill the child on close
// unconditionally.
type RPCServer struct {
	bins Binaries
	cfg  RPCServerConfig

	mu      sync.Mutex
	active  *session
	logSink io.Writer
}

type session struct {
	cmd  *exec.Cmd
	port int
	done chan struct{}
}

// NewRPCServer builds a supervisor over located, version-gated binaries.
func NewRPCServer(bins Binaries, cfg RPCServerConfig) *RPCServer {
	return &RPCServer{bins: bins, cfg: cfg, logSink: os.Stderr}
}

// OpenLocal admits ONE offload session, lazily spawning ggml-rpc-server, and
// returns the loopback address the caller should dial.
//
// It returns ErrBusy if a session is already active. Callers must call
// CloseLocal exactly once per successful OpenLocal.
func (s *RPCServer) OpenLocal(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.active != nil {
		return "", ErrBusy
	}

	port := s.cfg.Port
	if port == 0 {
		p, err := reservePort()
		if err != nil {
			return "", err
		}
		port = p
	}

	cfg := s.cfg
	cfg.Port = port
	args := rpcServerArgs(cfg)

	// Not exec.CommandContext: the child's lifetime is the SESSION's, not the
	// caller's request context. CloseLocal owns termination.
	cmd := exec.Command(s.bins.RPCServer, args...)
	cmd.Stdout = s.logSink
	cmd.Stderr = s.logSink
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("llama: start %s: %w", s.bins.RPCServer, err)
	}

	sess := &session{cmd: cmd, port: port, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(sess.done)
	}()

	addr := net.JoinHostPort(loopbackHost, strconv.Itoa(port))
	if err := waitReady(ctx, addr, sess.done, 20*time.Second); err != nil {
		_ = kill(sess)
		return "", err
	}
	s.active = sess
	return addr, nil
}

// CloseLocal terminates the active session's child unconditionally.
//
// Unconditionally is the point: ggml-rpc-server holds the model's tensors in
// memory and has no session-reset concept. Reusing a process across peers would
// leak one peer's residency into the next. Kill it.
func (s *RPCServer) CloseLocal() {
	s.mu.Lock()
	sess := s.active
	s.active = nil
	s.mu.Unlock()
	if sess != nil {
		_ = kill(sess)
	}
}

// Active reports whether a session currently holds the slot.
func (s *RPCServer) Active() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active != nil
}

func kill(sess *session) error {
	if sess.cmd.Process == nil {
		return nil
	}
	_ = sess.cmd.Process.Kill()
	select {
	case <-sess.done:
	case <-time.After(5 * time.Second):
	}
	return nil
}

// waitReady polls the loopback port until the child accepts a connection, the
// child dies, or ctx/timeout expires. ggml-rpc-server prints a banner but has no
// machine-readable ready signal, so a successful dial is the signal.
func waitReady(ctx context.Context, addr string, done <-chan struct{}, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		select {
		case <-done:
			return fmt.Errorf("llama: ggml-rpc-server exited before it accepted a connection on %s "+
				"(a port collision on the reserved port is the usual cause)", addr)
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("llama: ggml-rpc-server did not listen on %s within %s", addr, timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
