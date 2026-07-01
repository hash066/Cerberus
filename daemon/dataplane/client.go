package dataplane

import (
	"bytes"
	"context"
	"fmt"
	"io"
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

// Client dials data-plane servers.
type Client struct{}

// NewClient returns a data-plane client.
func NewClient() *Client { return &Client{} }

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
	if ep.Kind != EndpointQUIC {
		return fmt.Errorf("dataplane: unsupported endpoint kind %q", ep.Kind)
	}
	// Local guard: do not even open a stream for a blob that cannot fit the grant.
	if n > ep.Quota.Bytes {
		return contract.Errf(contract.ErrQuotaExceeded,
			fmt.Sprintf("blob %d bytes exceeds quota %d", n, ep.Quota.Bytes))
	}

	conn, err := quic.DialAddr(ctx, ep.Addr, clientTLS(ep.ServerPeerID), &quic.Config{})
	if err != nil {
		return contract.Errf(contract.ErrPartitioned, err.Error())
	}
	defer conn.CloseWithError(0, "done")

	st, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return contract.Errf(contract.ErrPartitioned, err.Error())
	}
	defer st.CancelRead(0)

	if err := writeHeader(st, header{
		TransferID: ep.TransferID,
		Cap:        ep.Cap,
		Length:     n,
		SignedCap:  ep.SignedCap, // cross-kernel authority; verified before bytes flow
		Issuer:     ep.Issuer,
	}); err != nil {
		return err
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

	ackResult := readAck(st)
	werr := <-writeErr

	// The ack is authoritative: if the server explicitly denied/accepted, honor
	// that. A write error only matters when the server gave us no usable ack.
	if ackResult != nil {
		return ackResult
	}
	if werr != nil {
		return contract.Errf(contract.ErrPartitioned, werr.Error())
	}
	return nil
}

// SendBytes is the convenience form of Send for an in-memory blob.
func (c *Client) SendBytes(ctx context.Context, ep Endpoint, blob []byte) error {
	return c.Send(ctx, ep, bytes.NewReader(blob), uint64(len(blob)))
}

// readAck reads the server's single-byte status (plus optional message) and maps
// it to a contract error.
func readAck(st *quic.Stream) error {
	buf, err := io.ReadAll(st)
	if err != nil {
		return contract.Errf(contract.ErrPartitioned, err.Error())
	}
	if len(buf) == 0 {
		return contract.Errf(contract.ErrPartitioned, "no ack from server")
	}
	if buf[0] == ackOK {
		return nil
	}
	msg := string(buf[1:])
	// Surface the server's category when it is a known code; otherwise denied.
	switch {
	case strings.Contains(msg, string(contract.ErrQuotaExceeded)):
		return contract.Errf(contract.ErrQuotaExceeded, msg)
	case strings.Contains(msg, string(contract.ErrRevoked)):
		return contract.Errf(contract.ErrRevoked, msg)
	default:
		return contract.Errf(contract.ErrDenied, msg)
	}
}
