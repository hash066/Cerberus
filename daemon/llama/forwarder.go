package llama

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/mesh"
)

// forwarderHost is the ONLY address a forwarder listener binds.
//
// See the "loopback laundering" section of doc.go: this listener is an
// UNAUTHENTICATED TCP port that pipes into an AUTHENTICATED mesh session. Binding
// it anywhere but loopback would hand any host on the LAN the use of a capability
// we already presented — i.e. it would make the mesh gate pointless in the exact
// direction the gate exists to protect. forwarder_test.go asserts this.
const forwarderHost = "127.0.0.1"

// SessionOpener opens a capability-gated llama offload session to a peer and
// splices local to it, blocking until the session ends. *mesh.Fabric implements
// it; it is an interface here so the forwarder is testable without a live mesh.
type SessionOpener interface {
	OpenLlamaRPCSession(peer contract.PeerID, capEnvelope []byte, issuer contract.PeerID, build int, local io.ReadWriter) error
}

// WorkerPeer is one enrolled remote worker and the capability authorizing its use.
type WorkerPeer struct {
	// ID is the worker's mesh PeerID.
	ID contract.PeerID
	// Cap is the signed RightExec envelope for the worker's llama-rpc resource.
	Cap []byte
	// Issuer minted Cap. Under the self-issuer trust model this is the REQUESTER's
	// own PeerID: the worker binds the claimed issuer to the peer its QUIC/TLS
	// handshake authenticated, so a cap for a session we open must be self-issued.
	Issuer contract.PeerID
}

// Forwarder presents each enrolled remote worker as a LOCAL loopback TCP endpoint,
// because that is the only thing llama-server can talk to: it takes
// `--rpc host:port,host:port` and speaks raw ggml-rpc at them. Each accepted
// connection is spliced into a capability-gated mesh session to the real worker.
//
// Upstream's client keeps ONE persistent TCP connection per endpoint (get_socket()
// caches sockets in a static map keyed by endpoint string, with a negotiate_hello
// handshake per connection), so one accept per peer is the normal case and the
// single-slot admission control on the worker side matches the protocol's own
// behaviour. The accept loop still runs continuously to handle a reconnect after
// the client drops a socket.
type Forwarder struct {
	opener SessionOpener
	build  int

	mu        sync.Mutex
	listeners []net.Listener
	addrs     []string
	closed    bool
	wg        sync.WaitGroup
}

// NewForwarder builds a forwarder over a session opener. build is the local
// llama.cpp build number, sent in each session's open frame for the skew check.
func NewForwarder(opener SessionOpener, build int) *Forwarder {
	return &Forwarder{opener: opener, build: build}
}

// Start opens one loopback listener per peer and begins accepting. It returns the
// loopback addresses in the SAME ORDER as peers, suitable for --rpc.
//
// On any error every listener already opened is closed: a half-started forwarder
// would give llama-server endpoints that go nowhere.
func (f *Forwarder) Start(ctx context.Context, peers []WorkerPeer) ([]string, error) {
	if len(peers) == 0 {
		return nil, fmt.Errorf("llama: no worker peers enrolled")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, fmt.Errorf("llama: forwarder is closed")
	}

	for _, p := range peers {
		l, err := net.Listen("tcp", net.JoinHostPort(forwarderHost, "0"))
		if err != nil {
			f.closeLocked()
			return nil, fmt.Errorf("llama: listen on loopback for peer %x: %w", p.ID[:8], err)
		}
		f.listeners = append(f.listeners, l)
		f.addrs = append(f.addrs, l.Addr().String())

		f.wg.Add(1)
		go func(l net.Listener, p WorkerPeer) {
			defer f.wg.Done()
			f.acceptLoop(ctx, l, p)
		}(l, p)
	}
	return append([]string(nil), f.addrs...), nil
}

func (f *Forwarder) acceptLoop(ctx context.Context, l net.Listener, p WorkerPeer) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return // listener closed
		}
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			defer conn.Close()
			// Blocks for the life of the session. An error here means the peer
			// refused (no capability, build skew, busy) or the link dropped;
			// closing conn makes llama-server see its RPC endpoint go away, which
			// is the honest signal — better than a silent stall.
			_ = f.opener.OpenLlamaRPCSession(p.ID, p.Cap, p.Issuer, f.build, conn)
		}()
	}
}

// Addrs returns the loopback endpoints in peer order.
func (f *Forwarder) Addrs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.addrs...)
}

// RPCFlag renders the value for llama-server's --rpc: a comma-separated
// host:port list (verified against common/arg.cpp: `--rpc SERVERS`,
// "comma-separated list of RPC servers (host:port)").
func (f *Forwarder) RPCFlag() string {
	return strings.Join(f.Addrs(), ",")
}

// Close tears down every listener.
//
// This is called when the llama-server child exits, and that timing is one of the
// three mitigations for the unauthenticated loopback listener (see doc.go): the
// port must not outlive the run that needed it.
func (f *Forwarder) Close() {
	f.mu.Lock()
	f.closeLocked()
	f.mu.Unlock()
	f.wg.Wait()
}

func (f *Forwarder) closeLocked() {
	f.closed = true
	for _, l := range f.listeners {
		_ = l.Close()
	}
	f.listeners = nil
}

// Compile-time proof that the real mesh Fabric satisfies SessionOpener. If the
// mesh signature drifts, this fails at build time rather than at runtime on an
// operator's machine.
var _ SessionOpener = (*mesh.Fabric)(nil)
