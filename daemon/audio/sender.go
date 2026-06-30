package audio

import (
	"context"
	"errors"
	"fmt"
)

// Sender pulls PCM frames from a Source, packetizes them (sequence number +
// media timestamp + self-describing format header), and writes each packet to
// an injectable Transport. It is the capture-side half of the network audio
// path; on the wire this is the AES67/ROC-style stream a peer's Receiver
// reconstructs.
//
// A Sender is single-goroutine: call Run (blocking) or drive it frame-by-frame
// with SendFrame from one goroutine.
type Sender struct {
	src   Source
	tx    Transport
	seq   uint64 // next sequence number
	stamp uint64 // next media timestamp, in per-channel samples
	fmt   Format
}

// NewSender wires a Source to a Transport. The stream's Format is taken from the
// Source and stamped into every packet.
func NewSender(src Source, tx Transport) *Sender {
	return &Sender{src: src, tx: tx, fmt: src.Format()}
}

// SendFrame reads exactly one frame from the Source and transmits it as one
// packet. It returns ErrSourceDrained at clean end-of-stream (the caller should
// stop), or any transport/source error. The frame's sample count must match the
// stream format; a mismatch is reported rather than silently shipped.
func (s *Sender) SendFrame(ctx context.Context) error {
	frame, err := s.src.ReadFrame(ctx)
	if err != nil {
		return err
	}
	want := s.fmt.SamplesPerPacket()
	if len(frame.Samples) != want {
		return fmt.Errorf("audio: sender: frame has %d samples, want %d for format", len(frame.Samples), want)
	}

	pkt := packet{
		format:    s.fmt,
		sequence:  s.seq,
		timestamp: s.stamp,
		frame:     frame,
	}
	if err := s.tx.Send(ctx, pkt.encode()); err != nil {
		return err
	}

	s.seq++
	s.stamp += uint64(SamplesPerFrame) // timestamp advances per-channel samples
	return nil
}

// Run streams frames from the Source to the Transport until the Source drains
// (ErrSourceDrained -> nil), ctx is cancelled, or an error occurs. It is the
// normal driver for a live capture stream.
func (s *Sender) Run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := s.SendFrame(ctx)
		switch {
		case err == nil:
			continue
		case errors.Is(err, ErrSourceDrained):
			return nil
		default:
			return err
		}
	}
}

// Format reports the stream format the Sender is emitting.
func (s *Sender) Format() Format { return s.fmt }
