package dataplane

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strings"

	contract "github.com/hash066/cerberus/contract/go"
	quic "github.com/quic-go/quic-go"
)

// client.go is the sending side of the data plane. A holder of an Endpoint (which
// it received from the control plane, e.g. a 9P `.../ctl` open) dials the server,
// presents the capability + transfer id in the header, and streams the blob.
//
// Send refuses locally to attempt a transfer larger than the granted quota — a
// well-behaved client does not even open the stream for an over-quota blob — but
// the SERVER is the authority and enforces the ceiling regardless (defense in
// depth: a buggy or hostile client cannot exceed the grant).

// Client dials data-plane servers. Because the server now requires mutual TLS,
// every Client carries a client certificate bound to an Ed25519 identity, which
// it presents on the handshake so the server can authenticate and record the
// dialing node's PeerID.
type Client struct {
	// cert is the Ed25519-bound client-auth certificate presented on the mTLS
	// handshake. NewClientWithIdentity binds it to the node's REAL mesh identity
	// (so the server records the node's true PeerID); NewClient binds it to a
	// fresh ephemeral identity (a real, verifiable key — just not the node's
	// durable PeerID), preserving the pre-mTLS zero-argument call sites.
	cert tls.Certificate

	// tuning is the QUIC transport tuning this client dials with (quicconf.go).
	// Previously the dialer passed a literally empty &quic.Config{} and inherited
	// every quic-go default, including no keep-alive.
	tuning Tuning

	// transport, when non-nil, is a shared quic.Transport every Send dials
	// through, so all transfers reuse ONE UDP socket instead of quic.DialAddr
	// opening a fresh socket per transfer. Nil (the default) keeps the
	// DialAddr-per-transfer behaviour, whose socket quic-go closes with the
	// connection — so the default Client remains safe to construct, use once and
	// discard, exactly as existing call sites do. See SetTransport.
	transport *quic.Transport
}

// NewClient returns a data-plane client whose mTLS certificate is bound to a
// FRESH, ephemeral Ed25519 identity. The client still authenticates itself with a
// real key the server can pin/record, but that key is not the node's durable mesh
// PeerID. Use this only where the caller has no node identity to bind (e.g. tests,
// or a purely outbound context); prefer NewClientWithIdentity so the server can
// record the dialing node's true PeerID. Panics only on a crypto/rand failure,
// which is not a recoverable condition.
func NewClient() *Client {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(fmt.Sprintf("dataplane: generate ephemeral client identity: %v", err))
	}
	c, err := NewClientWithIdentity(priv)
	if err != nil {
		panic(fmt.Sprintf("dataplane: build ephemeral client cert: %v", err))
	}
	return c
}

// NewClientWithIdentity returns a data-plane client whose mTLS certificate is
// bound to the node's REAL Ed25519 mesh identity (the same keypair the node uses
// as its PeerID; see daemon/mesh Fabric.Identity()). The server derives and
// records the dialing node's authenticated PeerID from this certificate, binding
// each transfer to a real peer identity exactly as the mesh path already does.
func NewClientWithIdentity(identity ed25519.PrivateKey) (*Client, error) {
	cert, err := clientCert(identity)
	if err != nil {
		return nil, err
	}
	return &Client{cert: cert, tuning: DefaultTuning()}, nil
}

// SetTuning overrides the QUIC transport tuning this client dials with (see
// quicconf.go). Not safe to call concurrently with Send.
func (c *Client) SetTuning(t Tuning) { c.tuning = t }

// SetTransport makes every subsequent Send dial through tr, so all transfers
// share ONE UDP socket rather than opening a fresh one per transfer
// (quic.DialAddr's behaviour, which is what this client does when tr is nil).
//
// The caller OWNS tr and must Close it; the Client never does. That ownership
// split is why this is opt-in rather than the default: the existing call sites
// construct a Client, Send once and drop it (`dataplane.NewClient().SendBytes(…)`),
// and silently giving each one a socket it never closes would leak a UDP socket
// per transfer. Callers that make many transfers should build one Transport, one
// Client, and reuse both. Not safe to call concurrently with Send.
func (c *Client) SetTransport(tr *quic.Transport) { c.transport = tr }

// dial opens a QUIC connection to addr, through the shared transport when one is
// installed and via quic.DialAddr otherwise. Both paths use the SAME tls.Config
// and quic.Config, so socket reuse cannot change the security posture or the
// tuning — only which socket the packets leave from.
func (c *Client) dial(ctx context.Context, addr string, expect contract.PeerID) (*quic.Conn, error) {
	tlsConf := clientTLS(expect, c.cert)
	conf := c.tuning.clientConfig()
	if c.transport == nil {
		return quic.DialAddr(ctx, addr, tlsConf, conf)
	}
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	return c.transport.Dial(ctx, ua, tlsConf, conf)
}

// Send streams a blob to the server described by ep, authorized by ep.Cap and
// bounded by ep.Quota.Bytes. The payload is read from r (the bulk source); n is
// its length in bytes, which the client declares in the header. The whole blob is
// streamed chunk by chunk and is never required to be resident in one buffer.
//
// If ep.ServerPeerID is set, the QUIC/TLS dial PINS the server's certificate to
// that exact Ed25519 key (see tls.go's VerifyPeerCertificate): a network MITM
// terminating the handshake with its own certificate — even a validly
// self-signed one — is rejected during the dial, before the header or any
// payload byte is written. If ep.ServerPeerID is the zero PeerID (the caller did
// not know which peer it intended to reach ahead of the dial), no pinning
// happens and the transfer is authorized only by the in-band capability checks
// below — a documented gap, not a silent one (see tls.go / endpoint.go).
//
// Send returns nil only if the server acknowledged a complete, in-quota transfer.
// An over-quota or unauthorized transfer returns a contract.CapError.
func (c *Client) Send(ctx context.Context, ep Endpoint, r io.Reader, n uint64) error {
	_, err := c.send(ctx, ep, r, n)
	return err
}

// Request runs a request/response transfer against a server-side Responder
// (registered via Server.RegisterResponder): it sends req over the granted
// session and returns the response blob the responder produced. It is the
// requester side of a data-plane WORK session — e.g. dispatching an f32 kernel to
// a peer's /cer/dev/gpu device after opening its ctl. The request is bounded by
// ep.Quota exactly as Send is; the response is whatever the server sent back in
// the success ack. A denial/failure returns a contract.CapError with the server's
// reason (no fabricated response).
func (c *Client) Request(ctx context.Context, ep Endpoint, req []byte) ([]byte, error) {
	return c.send(ctx, ep, bytes.NewReader(req), uint64(len(req)))
}

// send streams a blob to the server described by ep, authorized by ep.Cap and
// bounded by ep.Quota.Bytes, and returns the server's ack body (empty for a
// plain one-way transfer; the responder's output for a request/response session).
// Send and Request are thin wrappers over it.
//
// If ep.ServerPeerID is set, the QUIC/TLS dial PINS the server's certificate to
// that exact Ed25519 key (see tls.go's VerifyPeerCertificate): a network MITM
// terminating the handshake with its own certificate — even a validly
// self-signed one — is rejected during the dial, before the header or any
// payload byte is written. If ep.ServerPeerID is the zero PeerID (the caller did
// not know which peer it intended to reach ahead of the dial), no pinning
// happens and the transfer is authorized only by the in-band capability checks
// below — a documented gap, not a silent one (see tls.go / endpoint.go).
//
// send returns a nil error only if the server acknowledged a complete, in-quota
// transfer. An over-quota or unauthorized transfer returns a contract.CapError.
func (c *Client) send(ctx context.Context, ep Endpoint, r io.Reader, n uint64) ([]byte, error) {
	if ep.Kind != EndpointQUIC {
		return nil, fmt.Errorf("dataplane: unsupported endpoint kind %q", ep.Kind)
	}
	// Local guard: do not even open a stream for a blob that cannot fit the grant.
	if n > ep.Quota.Bytes {
		return nil, contract.Errf(contract.ErrQuotaExceeded,
			fmt.Sprintf("blob %d bytes exceeds quota %d", n, ep.Quota.Bytes))
	}

	conn, err := c.dial(ctx, ep.Addr, ep.ServerPeerID)
	if err != nil {
		return nil, contract.Errf(contract.ErrPartitioned, err.Error())
	}
	defer func() { _ = conn.CloseWithError(0, "done") }()

	st, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, contract.Errf(contract.ErrPartitioned, err.Error())
	}
	defer st.CancelRead(0)

	if err := writeHeader(st, header{
		TransferID: ep.TransferID,
		Cap:        ep.Cap,
		Length:     n,
		SignedCap:  ep.SignedCap, // cross-kernel authority; verified before bytes flow
		Issuer:     ep.Issuer,
	}); err != nil {
		return nil, err
	}

	// Write the payload and read the ack concurrently. The server may reject a
	// transfer right after the header (unauthorized / over-declared length) by
	// sending STOP_SENDING + an ack without ever reading the payload; reading the
	// ack in parallel surfaces that denial cleanly instead of blocking on a write
	// the server has stopped consuming.
	writeErr := make(chan error, 1)
	go func() {
		// Stream the payload bounded by the grant; the server independently enforces it.
		if _, err := copyQuota(st, r, ep.Quota.Bytes); err != nil {
			_ = st.Close()
			writeErr <- err
			return
		}
		writeErr <- st.Close()
	}()

	body, ackErr := readAckBody(st)
	werr := <-writeErr

	// The ack is authoritative: if the server explicitly denied/accepted, honor
	// that. A write error only matters when the server gave us no usable ack.
	if ackErr != nil {
		return nil, ackErr
	}
	if werr != nil {
		return nil, contract.Errf(contract.ErrPartitioned, werr.Error())
	}
	return body, nil
}

// SendBytes is the convenience form of Send for an in-memory blob.
func (c *Client) SendBytes(ctx context.Context, ep Endpoint, blob []byte) error {
	return c.Send(ctx, ep, bytes.NewReader(blob), uint64(len(blob)))
}

// readAck reports only the server's ack status, discarding any response body. It
// is the error-only form used where the caller does not expect a response (the
// one-way transfer path and the low-level test harnesses).
func readAck(st *quic.Stream) error {
	_, err := readAckBody(st)
	return err
}

// readAckBody reads the server's single-byte status (plus optional body) and maps
// it to a contract error. On success it returns the trailing body bytes (the
// responder's output for a request/response session; empty for a one-way
// transfer); on failure it returns the mapped contract.CapError.
func readAckBody(st *quic.Stream) ([]byte, error) {
	buf, err := io.ReadAll(st)
	if err != nil {
		return nil, contract.Errf(contract.ErrPartitioned, err.Error())
	}
	if len(buf) == 0 {
		return nil, contract.Errf(contract.ErrPartitioned, "no ack from server")
	}
	if buf[0] == ackOK {
		return buf[1:], nil
	}
	msg := string(buf[1:])
	// Surface the server's category when it is a known code; otherwise denied.
	switch {
	case strings.Contains(msg, string(contract.ErrQuotaExceeded)):
		return nil, contract.Errf(contract.ErrQuotaExceeded, msg)
	case strings.Contains(msg, string(contract.ErrRevoked)):
		return nil, contract.Errf(contract.ErrRevoked, msg)
	default:
		return nil, contract.Errf(contract.ErrDenied, msg)
	}
}
