package llama

import (
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
)

type fakeOpener struct {
	mu     sync.Mutex
	opened []contract.PeerID
	block  chan struct{}
}

func (f *fakeOpener) OpenLlamaRPCSession(peer contract.PeerID, _ []byte, _ contract.PeerID, _ int, local io.ReadWriter) error {
	f.mu.Lock()
	f.opened = append(f.opened, peer)
	block := f.block
	f.mu.Unlock()
	// Echo so the test can prove the accepted conn is really wired to the session.
	go func() { _, _ = io.Copy(local, local) }()
	if block != nil {
		<-block
	}
	return nil
}

func (f *fakeOpener) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.opened)
}

// TestForwarderBindsLoopbackOnly is the forwarder's counterpart to
// TestRPCServerArgsAlwaysBindsLoopback, and it matters for the same reason.
//
// The forwarder listener is UNAUTHENTICATED: anything that connects to it borrows
// the capability Cerberus already presented to the remote worker. Loopback-only
// binding is what keeps that borrowing on-box. Bind 0.0.0.0 and any LAN host can
// launder its traffic through our capability into someone else's worker.
//
// If this fails, do not update the expectation.
func TestForwarderBindsLoopbackOnly(t *testing.T) {
	op := &fakeOpener{}
	fwd := NewForwarder(op, PinnedBuild)
	t.Cleanup(fwd.Close)

	peers := []WorkerPeer{
		{ID: contract.PeerID{1}},
		{ID: contract.PeerID{2}},
	}
	addrs, err := fwd.Start(context.Background(), peers)
	if err != nil {
		t.Fatal(err)
	}
	if len(addrs) != len(peers) {
		t.Fatalf("got %d addrs for %d peers", len(addrs), len(peers))
	}
	for _, a := range addrs {
		host, port, err := net.SplitHostPort(a)
		if err != nil {
			t.Fatalf("addr %q: %v", a, err)
		}
		if host != "127.0.0.1" {
			t.Fatalf("forwarder bound %q, want 127.0.0.1 — this listener is unauthenticated and "+
				"must never be reachable off-box", host)
		}
		if port == "0" || port == "" {
			t.Fatalf("addr %q has no concrete port", a)
		}
	}
	if forwarderHost != "127.0.0.1" {
		t.Fatalf("forwarderHost = %q; it must remain 127.0.0.1", forwarderHost)
	}
}

// TestForwarderRPCFlagFormat pins the --rpc value against upstream's documented
// format: common/arg.cpp declares `--rpc SERVERS` as a "comma-separated list of
// RPC servers (host:port)".
func TestForwarderRPCFlagFormat(t *testing.T) {
	op := &fakeOpener{}
	fwd := NewForwarder(op, PinnedBuild)
	t.Cleanup(fwd.Close)

	if _, err := fwd.Start(context.Background(), []WorkerPeer{{ID: contract.PeerID{1}}, {ID: contract.PeerID{2}}}); err != nil {
		t.Fatal(err)
	}
	flag := fwd.RPCFlag()
	parts := strings.Split(flag, ",")
	if len(parts) != 2 {
		t.Fatalf("RPCFlag() = %q, want 2 comma-separated endpoints", flag)
	}
	for _, p := range parts {
		host, _, err := net.SplitHostPort(p)
		if err != nil {
			t.Fatalf("endpoint %q is not host:port: %v", p, err)
		}
		if host != "127.0.0.1" {
			t.Fatalf("endpoint %q is not loopback", p)
		}
	}
	if strings.Contains(flag, " ") {
		t.Fatalf("RPCFlag() = %q contains a space; upstream splits on commas only", flag)
	}
}

// TestForwarderAcceptOpensSession proves an accepted loopback connection is really
// spliced into a mesh session for the right peer.
func TestForwarderAcceptOpensSession(t *testing.T) {
	op := &fakeOpener{}
	fwd := NewForwarder(op, PinnedBuild)
	t.Cleanup(fwd.Close)

	want := contract.PeerID{7}
	addrs, err := fwd.Start(context.Background(), []WorkerPeer{{ID: want}})
	if err != nil {
		t.Fatal(err)
	}

	conn, err := net.DialTimeout("tcp", addrs[0], 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	deadline := time.Now().Add(3 * time.Second)
	for op.count() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("dialing the forwarder did not open a mesh session")
		}
		time.Sleep(10 * time.Millisecond)
	}
	op.mu.Lock()
	got := op.opened[0]
	op.mu.Unlock()
	if got != want {
		t.Fatalf("session opened for peer %x, want %x", got[:4], want[:4])
	}
}

// TestForwarderStartRequiresPeers: zero peers must be an error, not a forwarder
// that silently yields an empty --rpc value (which llama-server would accept while
// quietly running everything locally — a wrong answer that looks like success).
func TestForwarderStartRequiresPeers(t *testing.T) {
	fwd := NewForwarder(&fakeOpener{}, PinnedBuild)
	if _, err := fwd.Start(context.Background(), nil); err == nil {
		t.Fatal("Start with no peers succeeded; it must fail loudly")
	}
}

// TestForwarderCloseReleasesPorts pins the teardown that keeps the unauthenticated
// listener from outliving the llama-server run that needed it.
func TestForwarderCloseReleasesPorts(t *testing.T) {
	op := &fakeOpener{}
	fwd := NewForwarder(op, PinnedBuild)
	addrs, err := fwd.Start(context.Background(), []WorkerPeer{{ID: contract.PeerID{1}}})
	if err != nil {
		t.Fatal(err)
	}
	fwd.Close()

	if _, err := net.DialTimeout("tcp", addrs[0], 500*time.Millisecond); err == nil {
		t.Fatalf("forwarder port %s still accepts connections after Close", addrs[0])
	}
}
