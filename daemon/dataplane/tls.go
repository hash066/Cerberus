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
//   - Both legs of the handshake carry a certificate bound to a node's REAL
//     Ed25519 mesh identity. selfSignedCert takes an identity keypair and issues a
//     self-signed X.509 cert whose SUBJECT PUBLIC KEY is that identity's public
//     half (a contract.PeerID). It is still self-signed (there is no CA — the mesh
//     has no PKI), but the key it attests to is the node's actual, durable PeerID.
//   - This is now MUTUAL TLS: the server (newSelfSignedTLS) requires a client
//     certificate (ClientAuth: RequireAnyClientCert) and, via a VerifyPeerCertificate
//     hook, rejects any client leg that presents no cert or a non-Ed25519 cert
//     before the handshake completes. The client (clientTLS) always presents its
//     own Ed25519-bound certificate. So each side authenticates the other's key.
//   - Neither side relies on Go's default chain/hostname validation (meaningless
//     for a self-signed cert with no CA). Instead they PIN the peer's Ed25519
//     subject key directly:
//       * the dialer pins the SERVER's expected PeerID via verifyPeerCertificate
//         (a mismatch — e.g. a MITM presenting its own key — fails the dial before
//         any payload byte is written);
//       * the server derives the CLIENT's authenticated PeerID from the verified
//         client leaf (see peerIDFromCert / server.go's serveConn) and binds the
//         transfer to it, exactly as the mesh path records the authenticated peer.
//   - This is the same trust model as mesh/identity.go's verifyAuthenticatedPeer:
//     "a session whose presented/authenticated key != claimed PeerID is
//     rejected." The data plane cannot use libp2p's own certificate extension
//     (it is a raw QUIC transport, not libp2p), so it authenticates each peer by
//     pinning / recording the TLS leaf's subject public key directly instead —
//     the equivalent guarantee over crypto/tls's VerifyPeerCertificate hook.
//   - When the DIALER does NOT know which server peer it intends to reach ahead of
//     the dial (expectedPeer is the zero PeerID), server pinning is impossible and
//     the server's identity is authenticated ONLY by the in-band signed-capability
//     check (server.go's SignedCapVerifier), exactly as documented in
//     daemon/mesh/security.go for a raw transport without a peer-identity
//     handshake. mTLS still forces the client to present a real key, and the
//     server still records the client's authenticated PeerID; only the server->
//     client leg is un-pinned in that case. This is called out explicitly rather
//     than silently leaving the channel unauthenticated (CLAUDE.md "maturity
//     honesty").

// selfSignedCert issues a self-signed X.509 certificate whose subject public key
// is priv's Ed25519 public half. extUsage distinguishes a server-auth cert from a
// client-auth cert; the subject key (and therefore the PeerID a peer derives from
// it) is identical either way. There is no CA — the node is its own root, keyed by
// its PeerID — so the cert is self-signed (issuer == subject). ed25519.PrivateKey
// implements crypto.Signer, so it is a valid CreateCertificate signer directly.
func selfSignedCert(priv ed25519.PrivateKey, extUsage []x509.ExtKeyUsage) (tls.Certificate, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return tls.Certificate{}, fmt.Errorf("dataplane: identity key must be an Ed25519 private key (got %d bytes)", len(priv))
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return tls.Certificate{}, fmt.Errorf("dataplane: identity private key has no Ed25519 public half")
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("dataplane: gen serial: %w", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "cerberus-dataplane"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           extUsage,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, pub, priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("dataplane: create cert: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}, nil
}

// clientCert issues the client-auth certificate a dialer presents on the mTLS
// handshake, bound to the node's real Ed25519 identity so the server can derive
// the dialing node's authenticated PeerID from it.
func clientCert(priv ed25519.PrivateKey) (tls.Certificate, error) {
	return selfSignedCert(priv, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
}

// newSelfSignedTLS returns a server tls.Config whose certificate's subject public
// key is the node's real Ed25519 mesh identity (priv), not a fresh throwaway key.
//
// It enforces MUTUAL TLS: ClientAuth is RequireAnyClientCert, so a client that
// presents no certificate is rejected during the handshake. The default
// VerifyPeerCertificate hook additionally rejects a client whose leaf is missing
// or is not an Ed25519 cert — so every accepted connection has a client leaf from
// which serveConn can derive a real PeerID. (Chain/hostname validation is
// deliberately NOT applied — there is no CA; the client leaf is self-signed and
// its identity is the subject key itself, recorded by the server, not validated
// against a trust store here.)
func newSelfSignedTLS(priv ed25519.PrivateKey) (*tls.Config, error) {
	cert, err := selfSignedCert(priv, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{alpnNextProto},
		MinVersion:   tls.VersionTLS13,
		// mTLS: demand a client certificate. RequireAnyClientCert (not
		// RequireAndVerifyClientCert) because there is no CA to verify a chain
		// against; requireEd25519ClientCert below performs the real check (a
		// well-formed Ed25519 leaf must be present) and serveConn records the
		// authenticated client PeerID derived from it.
		ClientAuth:            tls.RequireAnyClientCert,
		VerifyPeerCertificate: requireEd25519ClientCert,
	}, nil
}

// requireEd25519ClientCert is the server-side VerifyPeerCertificate hook. mTLS has
// already cryptographically verified the client possesses the private key for the
// presented leaf (crypto/tls checks the handshake signature before this runs), so
// this only needs to ensure the leaf exists and is an Ed25519 cert — i.e. that a
// real PeerID can be derived from it. The derivation itself happens once per
// connection in serveConn (from conn.ConnectionState().TLS.PeerCertificates).
func requireEd25519ClientCert(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	if len(rawCerts) == 0 {
		return fmt.Errorf("dataplane: client presented no certificate (mTLS required)")
	}
	leaf, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return fmt.Errorf("dataplane: parse client certificate: %w", err)
	}
	if _, ok := leaf.PublicKey.(ed25519.PublicKey); !ok {
		return fmt.Errorf("dataplane: client certificate is not Ed25519 (got %T) — cannot derive PeerID", leaf.PublicKey)
	}
	return nil
}

// peerIDFromCert derives a peer's contract.PeerID (the Ed25519 public key) from a
// verified leaf certificate. It returns the zero PeerID and false if the leaf is
// nil or does not carry a well-formed Ed25519 public key.
func peerIDFromCert(leaf *x509.Certificate) (contract.PeerID, bool) {
	if leaf == nil {
		return contract.PeerID{}, false
	}
	pub, ok := leaf.PublicKey.(ed25519.PublicKey)
	if !ok || len(pub) != len(contract.PeerID{}) {
		return contract.PeerID{}, false
	}
	var id contract.PeerID
	copy(id[:], pub)
	return id, true
}

// clientTLS returns the dialer tls.Config for mutual TLS. It ALWAYS presents the
// client's own Ed25519-bound certificate (clientCertificate) so the server can
// authenticate and record the dialing node's PeerID.
//
// InsecureSkipVerify is set because Go's default chain/hostname validation is
// meaningless here (the data plane has no CA and never will — every node is its
// own root, keyed by its PeerID). That is NOT the same as "unauthenticated":
// VerifyPeerCertificate is ALWAYS installed and performs the real check — it
// parses the presented server leaf, extracts its Ed25519 subject public key, and
// compares it against expectedPeer.
//
// If expectedPeer is the zero contract.PeerID, the caller does not know which
// server peer it intends to reach ahead of the dial (see the call site comments
// in client.go / daemon/system/fs.go for exactly which paths this applies to
// today). In that case VerifyPeerCertificate cannot pin the server and the
// server->client leg is authenticated only by the in-band signed-capability check
// (server.go's SignedCapVerifier) — a documented, not faked, gap (see
// daemon/mesh/security.go's equivalent TODO for a raw, non-libp2p transport).
// mTLS still forces the client to present a real key regardless.
func clientTLS(expectedPeer contract.PeerID, clientCertificate tls.Certificate) *tls.Config {
	return &tls.Config{
		Certificates:          []tls.Certificate{clientCertificate},
		InsecureSkipVerify:    true, //nolint:gosec // chain/hostname validation is meaningless for a self-signed, CA-less cert; VerifyPeerCertificate below does the real PeerID-pin check.
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
