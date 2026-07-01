// Package dataplane is Phase F3: the capability-bound QUIC zero-copy data plane.
//
// Position in the architecture (ARCHITECTURE.md §1 principle 2, §4.1; vertical
// 04): the control plane (9P namespace, mesh pub/sub, naming and grants) and the
// data plane (bulk bytes: tensors, VRAM, audio, file content) are physically
// separate. Opening a 9P `.../ctl` file returns a DataEndpoint descriptor — never
// the bytes themselves — and the holder then connects to *this* package's QUIC
// server to actually move the blob. So this is the bulk-byte path a `ctl` open
// hands you, deliberately kept off the control-plane mesh.
//
// What is REAL here:
//   - A quic-go (quic-v1) Server and Client that stream a bulk byte blob over a
//     dedicated QUIC stream, with a 4-byte length-prefixed header frame followed
//     by the raw payload (streamed in fixed-size chunks; the whole blob is never
//     required to be resident on the receive side).
//   - Every transfer is bound to a capability handle, verified through the
//     contract.CapKernel BEFORE any payload byte is read or written. An invalid /
//     revoked / unknown capability is denied up front (no bytes flow).
//   - Every transfer is bound to a byte Quota taken from the grant. The server
//     enforces it: a declared or actual length over Quota.Bytes is rejected with
//     contract.ErrQuotaExceeded, again before/instead of accepting the payload.
//
// What is a STUB / not yet here (not faked):
//   - TLS now uses a self-signed certificate whose subject key IS the node's
//     real Ed25519 mesh identity (see tls.go), and a dialer that names the
//     server's expected PeerID in Endpoint.ServerPeerID gets that key pinned via
//     VerifyPeerCertificate — a MITM presenting a different key is rejected
//     before any payload byte is sent (see client.go). What remains a
//     documented gap: a caller that genuinely does not know the server's PeerID
//     ahead of the dial (ServerPeerID left zero) cannot pin, and the connection
//     is then authenticated only by the in-band signed-capability check —
//     exactly the raw-transport gap daemon/mesh/security.go documents for a
//     non-libp2p path. This is not silently unsafe: it is the same fallback the
//     mesh package already documents, called out at each such call site.
//   - The capability handle travels in-band as an opaque u64 under the demo's
//     shared-kernel model, matching daemon/mesh/compute.go. Cross-kernel signed
//     capability transfer (CBOR cap verified against the issuer key) is the same
//     hardening step noted for the mesh path.
//   - "Zero-copy" is the architectural intent (stream, don't buffer the whole
//     blob); true RDMA / kernel-bypass zero-copy is Frontier (vertical 04 §10).
package dataplane

import contract "github.com/hash066/cerberus/contract/go"

// EndpointKind identifies the data-plane transport. Mirrors ninep.EndpointKind
// (vertical 04 §4) without importing daemon/ninep — the control plane mints these
// descriptors and hands them across; this package only needs its own copy.
type EndpointKind string

const (
	// EndpointQUIC is a QUIC stream this package's Server/Client speak.
	EndpointQUIC EndpointKind = "quic"
	// EndpointRDMA is the Frontier RDMA-over-Thunderbolt path (not implemented).
	EndpointRDMA EndpointKind = "rdma"
)

// Endpoint is the descriptor the control plane (a 9P `.../ctl` open) hands out so
// a holder can connect to the data plane. It is intentionally consistent with
// ninep.DataEndpoint (vertical 04 §4: `{kind, endpoint, stream_id, quota}`) but
// defined locally so the two lanes do not couple.
//
// An Endpoint is single-use and capability-bound (vertical 04 §7): the TransferID
// names one authorized transfer, Cap is the handle that authorizes it, and Quota
// bounds how many bytes may flow. The control plane fills these in when it grants
// the device; the data-plane Client presents them back when it connects.
type Endpoint struct {
	// Kind is the transport family. This package serves EndpointQUIC.
	Kind EndpointKind `json:"kind"`
	// Addr is the QUIC transport address to dial, e.g. "127.0.0.1:51820".
	Addr string `json:"endpoint"`
	// TransferID identifies this one authorized transfer (the QUIC stream the
	// data flows on carries it in its header). Mirrors ninep's stream_id.
	TransferID uint64 `json:"transfer_id"`
	// Cap is the capability handle that authorizes the transfer. The server
	// verifies it against its CapKernel before any payload byte moves.
	Cap contract.CapHandle `json:"cap"`
	// Quota bounds the grant. Quota.Bytes is the hard byte ceiling enforced on
	// the transfer; 0 means "no bytes permitted" (an unbounded grant must say so
	// explicitly via a large ceiling, never by leaving Bytes zero).
	Quota contract.Quota `json:"quota"`
	// SignedCap is the Ed25519-signed capability envelope (daemon/auth.SignedCap)
	// that authorizes this transfer cross-kernel. The control plane fills it in
	// when it grants the endpoint; the Client presents it in the transfer header,
	// and a signed-verifier server checks it before any byte flows. Optional: empty
	// on the legacy handle-only path.
	SignedCap []byte `json:"signed_cap,omitempty"`
	// Issuer names the node that minted SignedCap, so the receiver can resolve the
	// matching (exchanged) public key to Verify under.
	Issuer contract.PeerID `json:"issuer,omitempty"`
	// ServerPeerID is the Ed25519 PeerID the Client EXPECTS the data-plane server
	// at Addr to present in its TLS certificate (see tls.go: the server's cert
	// subject key is its real mesh identity). When set, Client.Send pins the
	// TLS handshake to this exact key via VerifyPeerCertificate and rejects the
	// connection before any payload byte is sent if a different key is
	// presented — this is what makes the transport resistant to a network MITM
	// terminating the QUIC/TLS handshake with its own certificate. Left as the
	// zero PeerID when the caller does not know the server's identity ahead of
	// the dial; see tls.go's clientTLS doc for what that implies.
	ServerPeerID contract.PeerID `json:"server_peer_id,omitempty"`
}

// alpnNextProto is the ALPN protocol id negotiated on the QUIC/TLS handshake. It
// keeps this data-plane transport from being confused with any other QUIC service.
const alpnNextProto = "cerberus-dataplane/1"
