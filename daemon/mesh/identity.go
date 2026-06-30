package mesh

// identity.go binds peer sessions to Ed25519 PeerIDs.
//
// Security model (maturity honesty — see ARCHITECTURE.md §01 "mTLS binds
// sessions to PeerIDs"):
//
//   - The wire is libp2p over QUIC (quic-v1). libp2p's QUIC transport performs a
//     TLS 1.3 handshake whose certificate carries the libp2p extension binding
//     the connection to the remote's Ed25519 host key. The remote therefore
//     *proves possession of the private key behind its PeerID* during the
//     handshake; this is the real mTLS-equivalent guarantee, not a stub. The
//     authenticated key is surfaced by libp2p as Conn().RemotePublicKey() /
//     Conn().RemotePeer() and is what we enforce here.
//
//   - This file adds the explicit, verifiable *enforcement* layer that sits on
//     top of that handshake: on Dial we reject a session whose authenticated key
//     does not equal the claimed PeerID, and on accept we record the
//     authenticated identity so callers can bind to it. A presented key that
//     mismatches the claimed PeerID is rejected (ErrDenied).
//
//   - For a transport that does NOT carry the libp2p TLS handshake (e.g. a future
//     raw-TCP/Thunderbolt data plane), Session.Send/Recv cannot rely on the
//     transport to authenticate the peer. proveIdentity / verifyIdentity below
//     implement a signed-nonce handshake at the Session layer for that case.
//     Full custom-certificate X.509 mTLS over such a transport is a documented
//     TODO (see security.go); we do not fake it.

import (
	"crypto/ed25519"

	contract "github.com/hash066/cerberus/contract/go"
	ic "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

// toLibp2pID converts a contract.PeerID (raw Ed25519 public key) into a libp2p
// peer.ID. It fails if the bytes are not a valid Ed25519 public key.
func toLibp2pID(p contract.PeerID) (peer.ID, error) {
	pub, err := ic.UnmarshalEd25519PublicKey(p[:])
	if err != nil {
		return "", contract.Errf(contract.ErrDenied, "invalid PeerID public key: "+err.Error())
	}
	return peer.IDFromPublicKey(pub)
}

// contractPeerID extracts the raw 32-byte Ed25519 public key from a libp2p
// public key as a contract.PeerID. It returns false if the key is not a 32-byte
// Ed25519 key (Cerberus identities are Ed25519 only).
func contractPeerID(pub ic.PubKey) (contract.PeerID, bool) {
	var id contract.PeerID
	if pub == nil || pub.Type() != ic.Ed25519 {
		return id, false
	}
	raw, err := pub.Raw()
	if err != nil || len(raw) != 32 {
		return id, false
	}
	copy(id[:], raw)
	return id, true
}

// verifyAuthenticatedPeer checks that the public key libp2p cryptographically
// authenticated for a connection (authPub, from Conn().RemotePublicKey()) matches
// the claimed contract.PeerID. This is the binding enforcement: a session whose
// presented/authenticated key != claimed PeerID is rejected.
//
// authPub MUST be the key libp2p verified during the QUIC/TLS handshake, not a
// value supplied by the peer in-band — otherwise the check is meaningless.
func verifyAuthenticatedPeer(claimed contract.PeerID, authPub ic.PubKey) error {
	got, ok := contractPeerID(authPub)
	if !ok {
		return contract.Errf(contract.ErrDenied, "remote presented a non-Ed25519 identity")
	}
	if got != claimed {
		return contract.Errf(contract.ErrDenied, "peer key does not match claimed PeerID")
	}
	return nil
}

// --- Session-layer signed-nonce handshake (transport-agnostic fallback) -----
//
// Used only where the underlying transport does not itself authenticate the
// PeerID (i.e. NOT the libp2p/QUIC path, which already does). It proves
// possession of the private key behind a PeerID by signing a verifier-chosen
// nonce. This is a real, verifiable proof — but it authenticates identity only;
// it does not by itself encrypt the channel. Channel encryption for such a
// transport is the remaining TLS work (see security.go TODO).

// handshakeContext domain-separates handshake signatures from any other use of
// the host key, so a signature gathered here can never be replayed elsewhere.
const handshakeContext = "cerberus/session-handshake/v1\x00"

// proveIdentity signs a verifier-supplied nonce with the node's Ed25519 private
// key, proving possession of the key behind its PeerID.
func proveIdentity(priv ed25519.PrivateKey, nonce []byte) []byte {
	return ed25519.Sign(priv, append([]byte(handshakeContext), nonce...))
}

// verifyIdentity checks a proof produced by proveIdentity against the claimed
// PeerID (the Ed25519 public key) and the nonce the verifier issued. It returns
// nil only if the signature is valid for exactly that PeerID and nonce; a key
// that does not match the claimed PeerID therefore cannot produce a valid proof.
func verifyIdentity(claimed contract.PeerID, nonce, proof []byte) error {
	pub := ed25519.PublicKey(claimed[:])
	if len(pub) != ed25519.PublicKeySize {
		return contract.Errf(contract.ErrDenied, "invalid PeerID length")
	}
	if !ed25519.Verify(pub, append([]byte(handshakeContext), nonce...), proof) {
		return contract.Errf(contract.ErrDenied, "identity proof does not match claimed PeerID")
	}
	return nil
}
