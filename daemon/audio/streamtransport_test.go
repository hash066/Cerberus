package audio

import (
	"context"
	"io"
	"testing"
)

// TestStreamTransportRoundTrip runs a real Sender -> length-framed byte stream ->
// Receiver path through an in-memory pipe and asserts every captured frame is
// reconstructed in order. This is the framing the data-plane audio path relies on
// (one byte transfer carrying discrete packets).
func TestStreamTransportRoundTrip(t *testing.T) {
	format := Format{SampleRate: 48000, Channels: 1}
	const frames = 20

	pr, pw := io.Pipe()
	sender := NewSender(NewSineSource(format, 440, 0.5, frames), NewSendTransport(pw))
	sink := NewBufferSink(format)
	rx := NewReceiver(NewRecvTransport(pr), sink, ReceiverConfig{})

	// Drive the sender in the background; close the write end at end-of-stream so
	// the receiver sees a clean EOF and drains its jitter buffer.
	go func() {
		_ = sender.Run(context.Background())
		_ = pw.Close()
	}()

	if err := rx.Run(context.Background()); err != nil {
		t.Fatalf("receiver run: %v", err)
	}
	if len(sink.Frames) != frames {
		t.Fatalf("reconstructed %d frames, want %d", len(sink.Frames), frames)
	}
	for i, f := range sink.Frames {
		if len(f.Samples) != format.SamplesPerPacket() {
			t.Fatalf("frame %d has %d samples, want %d", i, len(f.Samples), format.SamplesPerPacket())
		}
	}
}

// TestRecvTransportRejectsOversizedFrame ensures a corrupt length prefix is
// rejected rather than driving an unbounded allocation.
func TestRecvTransportRejectsOversizedFrame(t *testing.T) {
	// A 4-byte big-endian length of 0xFFFFFFFF, far beyond maxFramedPacket.
	bad := []byte{0xFF, 0xFF, 0xFF, 0xFF}
	rx := NewRecvTransport(io.NopCloser(bytesReader(bad)))
	if _, err := rx.Recv(context.Background()); err != ErrFrameTooLarge {
		t.Fatalf("expected ErrFrameTooLarge, got %v", err)
	}
}

// bytesReader is a tiny io.Reader over a byte slice (avoids importing bytes for a
// single use and keeps the intent obvious).
type bytesReader []byte

func (b bytesReader) Read(p []byte) (int, error) {
	if len(b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, b)
	return n, nil
}
