package audio

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"sync"
)

// Transport is the injectable byte pipe the Sender writes packets to and the
// Receiver reads packets from. It is deliberately datagram-oriented: each Send
// is one packet and each Recv yields exactly one packet, mirroring how this will
// later ride a QUIC datagram / stream on the data plane (vertical 04 §3). The
// transport is allowed to drop, reorder, or duplicate packets — concealment and
// reordering are the Receiver's job, not the transport's.
//
// Keeping this an interface is what lets the package unit-test standalone and
// later sit on the real mesh without any change to Sender/Receiver.
type Transport interface {
	// Send transmits one packet's bytes. It may block briefly for flow control
	// but returns ErrTransportClosed once the transport is shut down.
	Send(ctx context.Context, pkt []byte) error
	// Recv returns the next packet's bytes. It blocks until a packet is
	// available, ctx is cancelled, or the transport is closed (io.EOF).
	Recv(ctx context.Context) ([]byte, error)
}

// ErrTransportClosed is returned by Send after the transport has been closed.
var ErrTransportClosed = errors.New("audio: transport closed")

// ---------------------------------------------------------------------------
// Wire format.
//
// One packet = fixed-size header + interleaved int16 little-endian PCM payload.
// The header is ROC/AES67-inspired: a sequence number for reordering/loss
// detection and a media timestamp (in samples) so the Receiver can relate the
// stream's clock to its own. Sample rate + channels travel in every packet so a
// late joiner / re-keyed stream is self-describing (like an inline SDP).
//
//	offset  size  field
//	0       4     magic   "CAU1"  (Cerberus AUdio v1)
//	4       2     channels        (uint16, big-endian)
//	6       4     sampleRate      (uint32, big-endian)
//	10      8     sequence        (uint64, big-endian) — per-packet, +1 each
//	18      8     timestamp       (uint64, big-endian) — sample index of frame[0]
//	26      4     sampleCount     (uint32, big-endian) — int16s that follow
//	30      ...   payload         (sampleCount * int16, little-endian)
// ---------------------------------------------------------------------------

const (
	packetMagic  = "CAU1"
	headerSize   = 30
	maxPCMValues = 1 << 20 // sanity cap on a single packet's sample count
)

// ErrMalformedPacket is returned by decodePacket when a packet cannot be parsed.
var ErrMalformedPacket = errors.New("audio: malformed packet")

// packet is the decoded form of one on-wire audio datagram.
type packet struct {
	format    Format
	sequence  uint64
	timestamp uint64 // sample index (per channel) of the first sample in frame
	frame     Frame
}

// encode serializes the packet to its wire form.
func (p packet) encode() []byte {
	buf := make([]byte, headerSize+len(p.frame.Samples)*2)
	copy(buf[0:4], packetMagic)
	binary.BigEndian.PutUint16(buf[4:6], p.format.Channels)
	binary.BigEndian.PutUint32(buf[6:10], p.format.SampleRate)
	binary.BigEndian.PutUint64(buf[10:18], p.sequence)
	binary.BigEndian.PutUint64(buf[18:26], p.timestamp)
	binary.BigEndian.PutUint32(buf[26:30], uint32(len(p.frame.Samples)))
	off := headerSize
	for _, s := range p.frame.Samples {
		binary.LittleEndian.PutUint16(buf[off:off+2], uint16(s))
		off += 2
	}
	return buf
}

// decodePacket parses a wire packet. It never panics on bad input — a truncated
// or corrupt packet yields ErrMalformedPacket so the Receiver can drop it and
// keep the stream alive.
func decodePacket(buf []byte) (packet, error) {
	if len(buf) < headerSize || string(buf[0:4]) != packetMagic {
		return packet{}, ErrMalformedPacket
	}
	count := binary.BigEndian.Uint32(buf[26:30])
	if count > maxPCMValues {
		return packet{}, ErrMalformedPacket
	}
	if len(buf) < headerSize+int(count)*2 {
		return packet{}, ErrMalformedPacket
	}
	p := packet{
		format: Format{
			Channels:   binary.BigEndian.Uint16(buf[4:6]),
			SampleRate: binary.BigEndian.Uint32(buf[6:10]),
		},
		sequence:  binary.BigEndian.Uint64(buf[10:18]),
		timestamp: binary.BigEndian.Uint64(buf[18:26]),
	}
	samples := make([]int16, count)
	off := headerSize
	for i := range samples {
		samples[i] = int16(binary.LittleEndian.Uint16(buf[off : off+2]))
		off += 2
	}
	p.frame = Frame{Samples: samples}
	return p, nil
}

// ---------------------------------------------------------------------------
// In-memory transport — the test/loopback data plane.
// ---------------------------------------------------------------------------

// MemTransport is an in-memory Transport: Send enqueues a copy of the packet
// onto a buffered channel that Recv drains. It models a lossless, in-order pipe
// by default; tests inject loss/reordering by manipulating the byte stream
// directly (e.g. via Sender.SendFrame against a packet list) or by using the
// channel capacity. It is safe for one sender goroutine and one receiver
// goroutine.
type MemTransport struct {
	ch     chan []byte
	mu     sync.Mutex
	closed bool
}

// NewMemTransport returns an in-memory transport with the given queue depth.
// A depth of a few frames mimics a real socket buffer.
func NewMemTransport(depth int) *MemTransport {
	if depth < 1 {
		depth = 1
	}
	return &MemTransport{ch: make(chan []byte, depth)}
}

// Send implements Transport. It copies the packet so the caller may reuse its
// buffer, then enqueues it. Send blocks if the queue is full (back-pressure).
func (t *MemTransport) Send(ctx context.Context, pkt []byte) error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return ErrTransportClosed
	}
	t.mu.Unlock()

	cp := make([]byte, len(pkt))
	copy(cp, pkt)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case t.ch <- cp:
		return nil
	}
}

// Recv implements Transport, returning the next queued packet. It returns
// io.EOF once the transport is closed and drained.
func (t *MemTransport) Recv(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case pkt, ok := <-t.ch:
		if !ok {
			return nil, io.EOF
		}
		return pkt, nil
	}
}

// Close shuts the transport down. After Close, Send returns ErrTransportClosed
// and Recv drains remaining packets then returns io.EOF.
func (t *MemTransport) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.closed = true
	close(t.ch)
}
