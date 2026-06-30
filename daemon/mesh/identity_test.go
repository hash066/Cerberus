package mesh

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	contract "github.com/hash066/cerberus/contract/go"
	ic "github.com/libp2p/go-libp2p/core/crypto"
)

// newPeer returns a fresh Ed25519 keypair and its contract.PeerID.
func newPeer(t *testing.T) (ed25519.PrivateKey, contract.PeerID) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	var id contract.PeerID
	copy(id[:], pub)
	return priv, id
}

// libp2pPub wraps a raw Ed25519 public key as a libp2p PubKey (the form
// Conn().RemotePublicKey() returns).
func libp2pPub(t *testing.T, id contract.PeerID) ic.PubKey {
	t.Helper()
	pub, err := ic.UnmarshalEd25519PublicKey(id[:])
	if err != nil {
		t.Fatalf("unmarshal pub: %v", err)
	}
	return pub
}

// TestVerifyAuthenticatedPeer_Match accepts a key that equals the claimed PeerID.
func TestVerifyAuthenticatedPeer_Match(t *testing.T) {
	_, id := newPeer(t)
	if err := verifyAuthenticatedPeer(id, libp2pPub(t, id)); err != nil {
		t.Fatalf("matching key rejected: %v", err)
	}
}

// TestVerifyAuthenticatedPeer_Mismatch is the core binding test: a session whose
// authenticated key differs from the claimed PeerID must be rejected with DENIED.
func TestVerifyAuthenticatedPeer_Mismatch(t *testing.T) {
	_, claimed := newPeer(t)
	_, other := newPeer(t)

	err := verifyAuthenticatedPeer(claimed, libp2pPub(t, other))
	if err == nil {
		t.Fatal("mismatched key was accepted; binding not enforced")
	}
	var ce *contract.CapError
	if e, ok := err.(*contract.CapError); ok {
		ce = e
	}
	if ce == nil || ce.Code != contract.ErrDenied {
		t.Fatalf("want DENIED, got %v", err)
	}
}

// TestVerifyAuthenticatedPeer_NilKey rejects a connection with no/usable key.
func TestVerifyAuthenticatedPeer_NilKey(t *testing.T) {
	_, id := newPeer(t)
	if err := verifyAuthenticatedPeer(id, nil); err == nil {
		t.Fatal("nil authenticated key was accepted")
	}
}

// TestSignedNonceHandshake exercises the transport-agnostic fallback proof: a
// node proves possession of the key behind its PeerID by signing a nonce, and a
// key that does not match the claimed PeerID cannot produce a valid proof.
func TestSignedNonceHandshake(t *testing.T) {
	priv, id := newPeer(t)
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("nonce: %v", err)
	}

	proof := proveIdentity(priv, nonce)
	if err := verifyIdentity(id, nonce, proof); err != nil {
		t.Fatalf("valid proof rejected: %v", err)
	}

	// Wrong claimed identity -> reject.
	_, other := newPeer(t)
	if err := verifyIdentity(other, nonce, proof); err == nil {
		t.Fatal("proof verified against the wrong PeerID")
	}

	// Tampered nonce -> reject (no replay across nonces).
	bad := append([]byte(nil), nonce...)
	bad[0] ^= 0xFF
	if err := verifyIdentity(id, bad, proof); err == nil {
		t.Fatal("proof verified against a different nonce")
	}

	// An impostor signing with the wrong key cannot satisfy the claimed PeerID.
	impostor, _ := newPeer(t)
	forged := proveIdentity(impostor, nonce)
	if err := verifyIdentity(id, nonce, forged); err == nil {
		t.Fatal("forged proof from a non-matching key was accepted")
	}
}

// TestToLibp2pIDRoundTrip confirms PeerID <-> libp2p peer.ID conversion is
// consistent and that contractPeerID recovers the original raw key.
func TestToLibp2pIDRoundTrip(t *testing.T) {
	_, id := newPeer(t)
	if _, err := toLibp2pID(id); err != nil {
		t.Fatalf("toLibp2pID: %v", err)
	}
	got, ok := contractPeerID(libp2pPub(t, id))
	if !ok {
		t.Fatal("contractPeerID rejected a valid Ed25519 key")
	}
	if got != id {
		t.Fatal("round-trip PeerID mismatch")
	}
}
