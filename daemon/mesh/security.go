package mesh

// security.go documents and configures the wire-security posture of the fabric.
//
// WHAT IS REAL TODAY
//   The fabric runs over libp2p's QUIC transport (quic-v1). That transport is
//   *encrypted and peer-authenticated by construction*: every QUIC connection
//   completes a TLS 1.3 handshake whose certificate carries the libp2p extension
//   binding the channel to the endpoint's Ed25519 host key (its PeerID). There is
//   no unauthenticated or plaintext mode on this path — a connection cannot be
//   established without both sides proving possession of the private key behind
//   their PeerID. We additionally enforce, at the application layer, that the
//   authenticated key equals the *claimed* PeerID on Dial (see identity.go and
//   Fabric.Dial); a mismatch is rejected.
//
//   For completeness libp2p can also negotiate the standalone TLS 1.3 security
//   transport (p2p/security/tls) or Noise (p2p/security/noise) on stream-muxed
//   transports such as TCP. We pin to QUIC, so those are not on the active path,
//   but the security guarantee (PeerID-bound, encrypted) is equivalent.
//
// WHAT IS STILL A STUB / TODO (not faked)
//   A future bulk data plane (Vertical 04: QUIC/RDMA-over-Thunderbolt) may use a
//   raw transport that does NOT carry the libp2p TLS handshake. On such a
//   transport the channel is not authenticated by the transport itself. For that
//   case identity.go provides a verifiable signed-nonce handshake
//   (proveIdentity/verifyIdentity) that authenticates the PeerID, but full
//   custom-certificate X.509 mTLS (mutual cert presentation + channel encryption)
//   over a raw transport is NOT yet implemented. Do not assume the raw data plane
//   is encrypted until that TODO lands.

import (
	libp2p "github.com/libp2p/go-libp2p"
)

// securityOptions returns the libp2p options that pin wire security.
//
// On the QUIC path security is intrinsic to the transport, so there is nothing
// extra to add and we return no options. The function exists as the single,
// documented place where security posture is decided: if a TCP/stream-muxed
// transport is ever added, wire libp2ptls (p2p/security/tls) or noise here
// rather than scattering it. We never enable the insecure/plaintext security
// transport (p2p/security/insecure, "/plaintext/2.0.0") — authentication is
// mandatory on every path.
func securityOptions() []libp2p.Option {
	return nil
}
