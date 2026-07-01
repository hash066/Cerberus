package dataplane

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/contract/go/stub"
)

// tls_pinning_test.go proves the fix for the production-readiness finding: the
// data-plane server's TLS certificate is bound to the node's REAL Ed25519
// identity (not a fresh throwaway key per Listen), and the client PINS its dial
// to the expected PeerID via VerifyPeerCertificate — so a MITM presenting a
// DIFFERENT key (e.g. its own self-signed cert, terminating the handshake
// instead of relaying it) is rejected before any payload byte is accepted.
//
// A real network MITM cannot be spun up in a unit test, but the property that
// makes MITM resistance possible is exactly this: the client's TLS verification
// must reject any certificate whose subject key isn't the one the caller
// expected, regardless of the fact that the cert is otherwise a "valid"
// self-signed cert (there is no CA to fool — the whole point is that identity
// is pinned, not chain-validated). That is what these tests exercise directly:
// (a) pinning the server's true PeerID succeeds and transfers the full blob,
// (b) pinning any OTHER PeerID against the exact same server is rejected
// deterministically, proving the server cannot be impersonated by a peer that
// doesn't hold the pinned private key — which is precisely what defeats a MITM
// that terminates the handshake with its own key.

// knownIdentityServer starts a real dataplane.Server bound to a KNOWN Ed25519
// keypair (so the test can assert the server's PeerID and pin against it, or
// deliberately pin against a different, wrong PeerID).
func knownIdentityServer(t *testing.T, sink Sink) (*Server, ed25519.PublicKey, func()) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen server identity: %v", err)
	}
	kernel := stub.NewCapKernel()
	srv := NewServer(kernel, testNow, priv)
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = srv.Serve(ctx, sink)
		close(done)
	}()
	return srv, pub, func() {
		cancel()
		_ = srv.Close()
		<-done
	}
}

func peerIDFromPub(pub ed25519.PublicKey) contract.PeerID {
	var id contract.PeerID
	copy(id[:], pub)
	return id
}

// TestClientPinsCorrectServerPeerIDSucceeds is property (a): a client that pins
// the server's TRUE PeerID connects and transfers the full blob successfully —
// pinning does not break the legitimate path.
func TestClientPinsCorrectServerPeerIDSucceeds(t *testing.T) {
	sink := &collectSink{}
	srv, pub, stop := knownIdentityServer(t, sink.sink)
	defer stop()

	// Sanity: the server reports its own PeerID as derived from the known key.
	if got := srv.PeerID(); got != peerIDFromPub(pub) {
		t.Fatalf("srv.PeerID() = %x, want %x (derived from the known identity)", got, peerIDFromPub(pub))
	}

	capH, err := stubMintOnServer(t, srv)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	blob := bytes.Repeat([]byte("pin-ok-"), 4096)
	ep := srv.RegisterGrant(1, capH, contract.Quota{Bytes: uint64(len(blob)) + 1024})
	if ep.ServerPeerID != peerIDFromPub(pub) {
		t.Fatalf("Endpoint.ServerPeerID = %x, want the server's real PeerID %x", ep.ServerPeerID, peerIDFromPub(pub))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := NewClient().SendBytes(ctx, ep, blob); err != nil {
		t.Fatalf("send with correct PeerID pin should succeed: %v", err)
	}
	if got := sink.bytes(); !bytes.Equal(got, blob) {
		t.Fatalf("round-trip mismatch under correct pin: got %d bytes, want %d", len(got), len(blob))
	}
}

// TestClientPinsWrongServerPeerIDRejected is property (b), the concrete
// MITM-resistance behavior: a client that pins a DIFFERENT (wrong) expected
// PeerID against the SAME real server is rejected during the TLS handshake —
// before any payload byte is accepted (the sink must receive nothing). This is
// exactly what would happen if a network MITM terminated the handshake and
// presented its OWN certificate instead of the real server's: the presented key
// would not match the pinned PeerID and VerifyPeerCertificate fails closed.
func TestClientPinsWrongServerPeerIDRejected(t *testing.T) {
	sink := &collectSink{}
	srv, pub, stop := knownIdentityServer(t, sink.sink)
	defer stop()

	capH, err := stubMintOnServer(t, srv)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	blob := bytes.Repeat([]byte("pin-mitm-"), 4096)
	ep := srv.RegisterGrant(2, capH, contract.Quota{Bytes: uint64(len(blob)) + 1024})

	// Simulate what a MITM would force the client to see: a different identity
	// than the real server's. Generate an unrelated "attacker" keypair and pin
	// against IT instead of the server's true PeerID (ep.ServerPeerID).
	attackerPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen attacker identity: %v", err)
	}
	wrongPeerID := peerIDFromPub(attackerPub)
	if wrongPeerID == peerIDFromPub(pub) {
		t.Fatal("attacker key accidentally collided with the real server key")
	}
	ep.ServerPeerID = wrongPeerID

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = NewClient().SendBytes(ctx, ep, blob)
	if err == nil {
		t.Fatal("expected the transfer to be rejected when pinning the wrong PeerID (simulated MITM)")
	}
	var ce *contract.CapError
	if !errors.As(err, &ce) || ce.Code != contract.ErrPartitioned {
		t.Fatalf("expected a PARTITIONED (dial/handshake) failure from the rejected TLS pin, got %v", err)
	}
	// The decisive assertion: NO bytes reached the sink. The handshake must fail
	// (VerifyPeerCertificate) before the client even writes the header, so the
	// server's handleStream never runs for this attempt.
	if got := sink.bytes(); len(got) != 0 {
		t.Fatalf("wrong-PeerID pin must reject before any payload is accepted, got %d bytes", len(got))
	}
}

// TestVerifyPeerCertificateRejectsMismatchedKey is a focused unit test of the
// VerifyPeerCertificate callback itself: given a real server certificate (whose
// subject key is a KNOWN Ed25519 public key), the callback must accept when
// expectedPeer equals that key and reject when it does not.
func TestVerifyPeerCertificateRejectsMismatchedKey(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen identity: %v", err)
	}
	tlsConf, err := newSelfSignedTLS(priv)
	if err != nil {
		t.Fatalf("newSelfSignedTLS: %v", err)
	}
	rawCert := tlsConf.Certificates[0].Certificate

	// Correct PeerID: accepted.
	if err := verifyPeerCertificate(peerIDFromPub(pub))(rawCert, nil); err != nil {
		t.Fatalf("expected correct PeerID to verify, got %v", err)
	}

	// Wrong PeerID: rejected.
	otherPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen other identity: %v", err)
	}
	if err := verifyPeerCertificate(peerIDFromPub(otherPub))(rawCert, nil); err == nil {
		t.Fatal("expected mismatched PeerID to fail verification")
	}

	// Zero PeerID: pinning is skipped entirely (nil callback), matching the
	// documented "caller does not know the expected peer" fallback.
	if cb := verifyPeerCertificate(contract.PeerID{}); cb != nil {
		t.Fatal("expected nil VerifyPeerCertificate callback when expectedPeer is the zero PeerID")
	}
}

// stubMintOnServer mints a read capability on the SAME stub kernel srv was
// built with (knownIdentityServer wires a *stub.CapKernel into NewServer), so
// the minted handle passes the server's own CapKernel.Verify gate exactly like
// every other test in this package that mints against the kernel it registers
// grants under.
func stubMintOnServer(t *testing.T, srv *Server) (contract.CapHandle, error) {
	t.Helper()
	k, ok := srv.kernel.(*stub.CapKernel)
	if !ok {
		t.Fatalf("test server kernel is not *stub.CapKernel (got %T)", srv.kernel)
	}
	return k.Mint(contract.ResourceRef{Kind: contract.KindVRAM}, []contract.Right{contract.RightRead}, nil)
}
