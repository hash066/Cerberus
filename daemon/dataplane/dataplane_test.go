package dataplane

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
)

// newTestIdentity generates a fresh Ed25519 keypair standing in for a node's
// real mesh identity, for tests that just need "some" valid identity key rather
// than a specific known one (see tls_pinning_test.go for tests exercising
// PINNING against a KNOWN identity).
func newTestIdentity(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen identity: %v", err)
	}
	return priv
}

// These tests exercise the data plane over a real loopback QUIC connection on
// 127.0.0.1, using the contract stub CapKernel as the authority (per the task).
// They assert the three required properties:
//  1. a within-quota transfer round-trips the exact bytes;
//  2. an over-quota transfer is rejected (and the bytes do NOT land in the sink);
//  3. a missing/invalid capability is denied before any bytes flow.

const testNow int64 = 1_700_000_000

// collectSink is a Sink that captures the received blob for assertions.
type collectSink struct {
	mu   sync.Mutex
	got  []byte
	hits int
}

// sink reads the whole blob and commits it ONLY on a clean read. A well-behaved
// sink does not commit a partial/aborted transfer — so an over-quota transfer the
// server tears down mid-stream lands nothing here, which is exactly the property
// the tests assert.
func (c *collectSink) sink(_ uint64, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.got = b
	c.hits++
	c.mu.Unlock()
	return nil
}

func (c *collectSink) bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.got...)
}

// newRunningServer brings up a server on an ephemeral loopback port and serves in
// the background until the returned cancel is called.
func newRunningServer(t *testing.T, kernel contract.CapKernel, sink Sink) (*Server, func()) {
	t.Helper()
	srv := NewServer(kernel, testNow, newTestIdentity(t))
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = srv.Serve(ctx, sink)
		close(done)
	}()
	return srv, func() {
		cancel()
		_ = srv.Close()
		<-done
	}
}

func TestWithinQuotaRoundTrips(t *testing.T) {
	kernel := stub.NewCapKernel()
	cap, err := kernel.Mint(contract.ResourceRef{Kind: contract.KindVRAM}, []contract.Right{contract.RightRead}, nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	sink := &collectSink{}
	srv, stop := newRunningServer(t, kernel, sink.sink)
	defer stop()

	blob := bytes.Repeat([]byte("cerberus-zero-copy-"), 5000) // ~95 KiB, spans many chunks
	ep := srv.RegisterGrant(1, cap, contract.Quota{Bytes: uint64(len(blob)) + 1024})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := NewClient().SendBytes(ctx, ep, blob); err != nil {
		t.Fatalf("send within quota: %v", err)
	}

	if got := sink.bytes(); !bytes.Equal(got, blob) {
		t.Fatalf("round-trip mismatch: got %d bytes, want %d", len(got), len(blob))
	}
}

func TestOverQuotaRejected(t *testing.T) {
	kernel := stub.NewCapKernel()
	cap, err := kernel.Mint(contract.ResourceRef{Kind: contract.KindVRAM}, []contract.Right{contract.RightRead}, nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	sink := &collectSink{}
	srv, stop := newRunningServer(t, kernel, sink.sink)
	defer stop()

	blob := bytes.Repeat([]byte("x"), 4096)
	// Grant only 1024 bytes; the 4096-byte blob must be rejected.
	ep := srv.RegisterGrant(2, cap, contract.Quota{Bytes: 1024})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = NewClient().SendBytes(ctx, ep, blob)
	if err == nil {
		t.Fatal("expected over-quota transfer to be rejected, got nil")
	}
	var ce *contract.CapError
	if !errors.As(err, &ce) || ce.Code != contract.ErrQuotaExceeded {
		t.Fatalf("expected QUOTA_EXCEEDED, got %v", err)
	}
	if got := sink.bytes(); len(got) != 0 {
		t.Fatalf("over-quota blob must not be delivered to the sink, got %d bytes", len(got))
	}
}

// TestOverQuotaUnderDeclaredRejected proves the server enforces the ceiling even
// when a (hostile) client under-declares Length to slip past the up-front check.
func TestOverQuotaUnderDeclaredRejected(t *testing.T) {
	kernel := stub.NewCapKernel()
	cap, _ := kernel.Mint(contract.ResourceRef{Kind: contract.KindVRAM}, []contract.Right{contract.RightRead}, nil)

	sink := &collectSink{}
	srv, stop := newRunningServer(t, kernel, sink.sink)
	defer stop()

	ep := srv.RegisterGrant(3, cap, contract.Quota{Bytes: 1024})

	// Bypass the client's local guard and honest Length by writing the frame
	// directly: declare 1000 bytes but stream 4096. The server's streaming guard
	// must still reject it and deliver nothing.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := sendRaw(ctx, ep, header{TransferID: 3, Cap: cap, Length: 1000}, bytes.Repeat([]byte("y"), 4096))
	if err == nil {
		t.Fatal("expected under-declared over-quota transfer to be rejected")
	}
	if got := sink.bytes(); len(got) != 0 {
		t.Fatalf("under-declared over-quota blob must not be delivered, got %d bytes", len(got))
	}
}

func TestMissingCapabilityDenied(t *testing.T) {
	kernel := stub.NewCapKernel()
	cap, _ := kernel.Mint(contract.ResourceRef{Kind: contract.KindVRAM}, []contract.Right{contract.RightRead}, nil)

	sink := &collectSink{}
	srv, stop := newRunningServer(t, kernel, sink.sink)
	defer stop()

	ep := srv.RegisterGrant(4, cap, contract.Quota{Bytes: 4096})

	// Present a capability handle that the server never authorized for this
	// transfer (and that the kernel does not know). No bytes must flow.
	ep.Cap = contract.CapHandle(99999)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := NewClient().SendBytes(ctx, ep, []byte("should never be accepted"))
	if err == nil {
		t.Fatal("expected missing/invalid capability to be denied")
	}
	var ce *contract.CapError
	if !errors.As(err, &ce) || ce.Code != contract.ErrDenied {
		t.Fatalf("expected DENIED, got %v", err)
	}
	if got := sink.bytes(); len(got) != 0 {
		t.Fatalf("denied transfer must not deliver bytes, got %d", len(got))
	}
}

// TestRevokedCapabilityDenied proves a revoked (present but invalid) capability is
// rejected by the kernel verify step before any payload is consumed.
func TestRevokedCapabilityDenied(t *testing.T) {
	kernel := stub.NewCapKernel()
	cap, _ := kernel.Mint(contract.ResourceRef{Kind: contract.KindVRAM}, []contract.Right{contract.RightRead}, nil)

	sink := &collectSink{}
	srv, stop := newRunningServer(t, kernel, sink.sink)
	defer stop()

	ep := srv.RegisterGrant(5, cap, contract.Quota{Bytes: 4096})
	if err := kernel.Revoke(cap); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := NewClient().SendBytes(ctx, ep, []byte("revoked grant"))
	if err == nil {
		t.Fatal("expected revoked capability to be denied")
	}
	if got := sink.bytes(); len(got) != 0 {
		t.Fatalf("revoked transfer must not deliver bytes, got %d", len(got))
	}
}

// TestEndpointDescriptorConsistent checks the descriptor the control plane hands
// out carries the transport addr, transfer id and quota (vertical 04 §4 shape).
func TestEndpointDescriptorConsistent(t *testing.T) {
	kernel := stub.NewCapKernel()
	cap, _ := kernel.Mint(contract.ResourceRef{Kind: contract.KindVRAM}, []contract.Right{contract.RightRead}, nil)

	srv := NewServer(kernel, testNow, newTestIdentity(t))
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer srv.Close()

	ep := srv.RegisterGrant(7, cap, contract.Quota{Bytes: 2 << 20})
	if ep.Kind != EndpointQUIC {
		t.Errorf("kind = %q, want %q", ep.Kind, EndpointQUIC)
	}
	if ep.Addr == "" || ep.Addr != srv.Addr() {
		t.Errorf("addr = %q, want server addr %q", ep.Addr, srv.Addr())
	}
	if ep.TransferID != 7 {
		t.Errorf("transfer id = %d, want 7", ep.TransferID)
	}
	if ep.Cap != cap {
		t.Errorf("cap = %d, want %d", ep.Cap, cap)
	}
	if ep.Quota.Bytes != 2<<20 {
		t.Errorf("quota = %d, want %d", ep.Quota.Bytes, 2<<20)
	}
	if ep.ServerPeerID != srv.PeerID() {
		t.Errorf("ServerPeerID = %x, want server's own PeerID %x", ep.ServerPeerID, srv.PeerID())
	}
	if ep.ServerPeerID == (contract.PeerID{}) {
		t.Errorf("ServerPeerID must not be zero when the server has a real identity")
	}
}
