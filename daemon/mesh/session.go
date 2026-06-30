package mesh

import (
	"encoding/binary"
	"io"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/libp2p/go-libp2p/core/network"
)

// sessionProto is the libp2p protocol id for point-to-point control sessions.
// The stream runs over QUIC (quic-go), so it is multiplexed and encrypted.
const sessionProto = "/cerberus/session/1.0.0"

// streamSession adapts a libp2p QUIC stream to contract.Session using simple
// length-prefixed framing (4-byte big-endian length + payload).
//
// remote is the peer identity libp2p cryptographically authenticated for this
// stream's connection (from the QUIC/TLS handshake). verified records whether
// that authenticated key is a valid Ed25519 PeerID. Callers can trust remote
// only when verified is true.
type streamSession struct {
	s        network.Stream
	remote   contract.PeerID
	verified bool
}

// newStreamSession wraps a libp2p stream and captures the authenticated remote
// PeerID from the connection's security session (set by the QUIC/TLS handshake).
func newStreamSession(s network.Stream) *streamSession {
	ss := &streamSession{s: s}
	if pub := s.Conn().RemotePublicKey(); pub != nil {
		if id, ok := contractPeerID(pub); ok {
			ss.remote = id
			ss.verified = true
		}
	}
	return ss
}

// RemotePeerID returns the Ed25519 PeerID libp2p authenticated for this session
// and whether it was a valid Ed25519 identity. The identity is bound by the
// QUIC/TLS handshake, so the peer cannot spoof it in-band.
func (ss *streamSession) RemotePeerID() (contract.PeerID, bool) {
	return ss.remote, ss.verified
}

func (ss *streamSession) Send(b []byte) error {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(b)))
	if _, err := ss.s.Write(hdr[:]); err != nil {
		return err
	}
	_, err := ss.s.Write(b)
	return err
}

func (ss *streamSession) Recv() ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(ss.s, hdr[:]); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, contract.Errf(contract.ErrPartitioned, "session closed")
		}
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	buf := make([]byte, n)
	if _, err := io.ReadFull(ss.s, buf); err != nil {
		return nil, contract.Errf(contract.ErrPartitioned, "short read")
	}
	return buf, nil
}

func (ss *streamSession) Close() error { return ss.s.Close() }

var _ contract.Session = (*streamSession)(nil)
