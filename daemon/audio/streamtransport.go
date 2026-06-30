package audio

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
)

// streamtransport.go adapts the datagram-style audio Transport to a plain byte
// stream by length-framing each packet. This is what lets an audio session ride
// a single byte transfer — e.g. one capability-bound QUIC transfer on the data
// plane (ARCHITECTURE §4.1) — while the rest of the package stays a leaf that
// depends only on io (the actual data-plane binding lives in the composition
// layer, daemon/audiolink, not here).
//
// Wire framing inside the stream: each packet is written as a 4-byte big-endian
// length prefix followed by exactly that many payload bytes (the transport.go
// packet encoding). The audio path is one-way, so a given endpoint uses one
// half: the capture node a SendTransport, the playback node a RecvTransport.

// maxFramedPacket bounds a single framed packet so a corrupt/hostile length
// prefix cannot make the receiver allocate without limit. It matches the largest
// packet transport.go can legitimately produce.
const maxFramedPacket = headerSize + maxPCMValues*2

// ErrFrameTooLarge is returned by RecvTransport.Recv when a length prefix exceeds
// maxFramedPacket — the stream is corrupt or not an audio stream.
var ErrFrameTooLarge = errors.New("audio: framed packet exceeds maximum size")

// SendTransport adapts an io.Writer to the send half of Transport: each Send
// writes one length-prefixed packet. It is send-only.
type SendTransport struct {
	w   io.Writer
	hdr [4]byte
}

// NewSendTransport wraps w (e.g. the write end of a data-plane transfer) as a
// send-only audio Transport.
func NewSendTransport(w io.Writer) *SendTransport { return &SendTransport{w: w} }

// Send writes one packet as [uint32 length][payload]. The two writes are not
// independently flushed; callers that need framing atomicity should wrap w in a
// buffer they flush per packet.
func (t *SendTransport) Send(ctx context.Context, pkt []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	binary.BigEndian.PutUint32(t.hdr[:], uint32(len(pkt)))
	if _, err := t.w.Write(t.hdr[:]); err != nil {
		return err
	}
	_, err := t.w.Write(pkt)
	return err
}

// Recv is unsupported: a SendTransport is one-way (capture side).
func (t *SendTransport) Recv(context.Context) ([]byte, error) {
	return nil, errors.New("audio: SendTransport is send-only")
}

// RecvTransport adapts an io.Reader to the receive half of Transport: each Recv
// reads one length-prefixed packet. It is receive-only.
type RecvTransport struct {
	r *bufio.Reader
}

// NewRecvTransport wraps r (e.g. the read end of a data-plane transfer) as a
// receive-only audio Transport.
func NewRecvTransport(r io.Reader) *RecvTransport {
	return &RecvTransport{r: bufio.NewReader(r)}
}

// Recv reads the next length-prefixed packet. It returns io.EOF when the stream
// ends cleanly on a packet boundary, and io.ErrUnexpectedEOF if it ends mid-packet.
func (t *RecvTransport) Recv(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var hdr [4]byte
	if _, err := io.ReadFull(t.r, hdr[:]); err != nil {
		return nil, err // io.EOF here means a clean end between packets.
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxFramedPacket {
		return nil, ErrFrameTooLarge
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(t.r, buf); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return buf, nil
}

// Send is unsupported: a RecvTransport is one-way (playback side).
func (t *RecvTransport) Send(context.Context, []byte) error {
	return errors.New("audio: RecvTransport is receive-only")
}

var (
	_ Transport = (*SendTransport)(nil)
	_ Transport = (*RecvTransport)(nil)
)
