package dataplane

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
)

// tls.go provides the QUIC/TLS material for the data plane.
//
// SECURITY MODEL (mirrors daemon/mesh/identity.go's pattern for a transport that
// does NOT carry libp2p's own handshake — see daemon/mesh/security.go's TODO):
//
//   - The server's certificate is now bound to the node's REAL Ed25519 mesh
//     identity: newSelfSignedTLS takes the node's actual identity keypair and
//     issues a self-signed X.509 cert whose SUBJECT PUBLIC KEY is that identity's
//     public half (a contract.PeerID). It is still self-signed (there is no CA —
//     the mesh has no PKI), but the cert is no longer a disposable, unrelated
//     ECDSA key generated fresh per Listen() call.
//   - The client does NOT rely on Go's chain/hostname validation (meaningless for
//     a self-signed cert with no CA) but it DOES perform real, manual peer-key
//     pinning via VerifyPeerCertificate: it parses the presented leaf cert,
//     extracts the Ed25519 public key, and compares it against the PeerID the
//     caller expects to reach. A mismatch — e.g. a network MITM presenting its
//     own key on either leg — is rejected before any payload byte is sent
//     (client.go's Send writes the header only after the QUIC/TLS handshake,
//     including VerifyPeerCertificate, has completed).
//   - This is the same trust model as mesh/identity.go's verifyAuthenticatedPeer:
//     "a session whose presented/authenticated key != claimed PeerID is
//     rejected." The data plane cannot use libp2p's own certificate extension
//     (it is a raw QUIC transport, not libp2p), so it authenticates the peer by
//     pinning the TLS leaf's subject public key directly instead — the
//     equivalent guarantee over crypto/tls's VerifyPeerCertificate hook.
//   - When the caller does NOT know which peer it intends to reach ahead of the
//     dial (expectedPeer is the zero PeerID), pinning is impossible and the
//     connection is authenticated ONLY by the in-band signed-capability check
//     (server.go's SignedCapVerifier), exactly as documented in
//     daemon/mesh/security.go for a raw transport without a peer-identity
//     handshake. This is called out explicitly at each such call site rather than
//     silently leaving the channel unauthenticated (CLAUDE.md "maturity honesty").

// newSelfSignedTLS returns a server tls.Config whose certificate's subject
// public key is the node's real Ed25519 mesh identity (priv), not a fresh
// throwaway key. The cert is still self-signed (issuer == subject; there is no
// mesh CA), but the key it attests to is the node's actual, durable PeerID, so a
// client that knows the expected PeerID can pin against it (see clientTLS).
func newSelfSignedTLS(priv ed25519.PrivateKey) (*tls.Config, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("dataplane: identity key must be an Ed25519 private key (got %d bytes)", len(priv))
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("dataplane: identity private key has no Ed25519 public half")
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("dataplane: gen serial: %w", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "cerberus-dataplane"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	// Self-signed: signer == subject (priv signs a cert naming its own pub as the
	// subject key). ed25519.PrivateKey implements crypto.Signer, so it is a valid
	// CreateCertificate signer directly — no intermediate CA key is needed or
	// used.
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, pub, priv)
	if err != nil {
		return nil, fmt.Errorf("dataplane: create cert: %w", err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{alpnNextProto},
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// clientTLS returns the dialer tls.Config. InsecureSkipVerify is set because Go's
// default chain/hostname validation is meaningless here (the data plane has no
// CA and never will — every node is its own root, keyed by its PeerID). That is
// NOT the same as "unauthenticated": VerifyPeerCertificate is ALWAYS installed
// and performs the real check — it parses the presented leaf certificate,
// extracts its Ed25519 subject public key, and compares it against expectedPeer.
//
// If expectedPeer is the zero contract.PeerID, the caller does not know which
// peer it intends to reach ahead of the dial (see the call site comments in
// client.go / daemon/system/fs.go for exactly which paths this applies to
// today). In that case VerifyPeerCertificate cannot pin anything and the
// connection is authenticated only by the in-band signed-capability check
// (server.go's SignedCapVerifier) — this is a documented, not faked, gap (see
// daemon/mesh/security.go's equivalent TODO for a raw, non-libp2p transport).
func clientTLS(expectedPeer contract.PeerID) *tls.Config {
	return &tls.Config{
		InsecureSkipVerify:    true, //nolint:gosec // chain/hostname validation is meaningless for a self-signed, CA-less cert; VerifyPeerCertificate below does the real check.
		NextProtos:            []string{alpnNextProto},
		MinVersion:            tls.VersionTLS13,
		VerifyPeerCertificate: verifyPeerCertificate(expectedPeer),
	}
}

// zeroPeerID is used to detect the "no expected peer" case.
var zeroPeerID contract.PeerID

// verifyPeerCertificate builds a crypto/tls VerifyPeerCertificate callback that
// pins the presented leaf certificate's Ed25519 subject public key against
// expectedPeer. It runs AFTER the TLS handshake has cryptographically verified
// the peer possesses the private key for that certificate (crypto/tls always
// checks the handshake signature before calling this hook), so a match here
// really does prove the remote holds expectedPeer's private key — exactly the
// property daemon/mesh/identity.go's verifyAuthenticatedPeer establishes for the
// libp2p/QUIC path.
//
// If expectedPeer is the zero PeerID, pinning is skipped (nil callback) — see
// clientTLS's doc comment for when that applies and why it is not silently
// unsafe (the in-band signed-capability gate still authorizes the transfer).
func verifyPeerCertificate(expectedPeer contract.PeerID) func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	if expectedPeer == zeroPeerID {
		return nil
	}
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return fmt.Errorf("dataplane: peer presented no certificate")
		}
		leaf, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("dataplane: parse peer certificate: %w", err)
		}
		pub, ok := leaf.PublicKey.(ed25519.PublicKey)
		if !ok {
			return fmt.Errorf("dataplane: peer certificate is not Ed25519 (got %T) — cannot verify PeerID", leaf.PublicKey)
		}
		if len(pub) != len(expectedPeer) {
			return fmt.Errorf("dataplane: peer public key has unexpected length %d", len(pub))
		}
		var got contract.PeerID
		copy(got[:], pub)
		if got != expectedPeer {
			return fmt.Errorf("dataplane: peer certificate key %x does not match expected PeerID %x — possible MITM", got, expectedPeer)
		}
		return nil
	}
}
