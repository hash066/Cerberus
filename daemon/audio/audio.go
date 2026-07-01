// Package audio implements Phase F5 — the network audio transport for the
// /cer/dev/audio peripheral (ARCHITECTURE §3.5; vertical 04 §3 "Audio").
//
// It is the mic/speaker-sharing path: PCM frames captured on one node are
// packetized, shipped over the mesh data plane, and reconstructed for playback
// on another. The design is ROC/AES67-inspired — sequence-numbered, timestamped
// packets with a small reordering jitter buffer and a delay-locked-loop (DLL)
// buffer-level controller to absorb clock drift between the two clocks without
// the buffer growing or starving unboundedly. Packet loss is concealed by
// gap-fill (silence or last-frame hold), never a panic.
//
// Everything is built around injectable seams so the package builds and
// unit-tests standalone (per CLAUDE.md golden rule 3):
//
//   - Source / Sink   — the OS capture/playback boundary. Real backends
//     (CoreAudio / WASAPI / PipeWire) are NOT verifiable in this environment
//     and ship as a labeled stub (see NewOSCaptureSource / NewOSPlaybackSink).
//     A synthetic SineSource and an in-memory BufferSink drive the tests.
//   - Transport       — the byte pipe between Sender and Receiver. In v0.1 it
//     is an in-memory pipe; later it sits on the QUIC zero-copy data plane
//     (vertical 04 §3). This package never imports the mesh — it only depends
//     on the Transport interface.
//
// This package imports nothing from the rest of Cerberus: it is a leaf with a
// tiny, well-defined surface so it can be wired behind a capability-gated
// /cer/dev/audio open without dragging the control plane in with it.
package audio

import (
	"context"
	"errors"
	"math"
)

// SamplesPerFrame is the fixed number of samples (per channel) carried in one
// audio frame / one network packet. At 48 kHz this is a 10 ms frame, a typical
// AES67 packet time — small enough for low latency, large enough to keep the
// packet rate sane.
const SamplesPerFrame = 480

// ErrSourceDrained is returned by a Source when it has no more audio to give
// (e.g. a finite test generator). The Sender treats it as a clean end-of-stream
// rather than an error.
var ErrSourceDrained = errors.New("audio: source drained")

// Format describes the PCM layout of a stream. It is carried in every packet
// header so the Receiver can reconstruct timing (samples -> wall-clock) and lay
// out interleaved channels correctly, exactly like an AES67/SDP descriptor.
//
// Samples are 16-bit signed little-endian PCM (the wire format in
// transport.go). Channels are interleaved frame-by-frame.
type Format struct {
	SampleRate uint32 // e.g. 48000
	Channels   uint16 // e.g. 1 (mono) or 2 (stereo)
}

// FrameDuration returns the wall-clock duration represented by one frame of
// SamplesPerFrame samples at this format's sample rate, in nanoseconds. Used by
// the Receiver's DLL to relate buffer occupancy to real time.
func (f Format) FrameDuration() int64 {
	if f.SampleRate == 0 {
		return 0
	}
	return int64(SamplesPerFrame) * 1_000_000_000 / int64(f.SampleRate)
}

// SamplesPerPacket is the total number of int16 samples in one frame's PCM
// payload, accounting for interleaved channels.
func (f Format) SamplesPerPacket() int {
	c := int(f.Channels)
	if c == 0 {
		c = 1
	}
	return SamplesPerFrame * c
}

// Frame is one unit of PCM audio: SamplesPerFrame samples per channel,
// channel-interleaved, 16-bit signed. len(Samples) == Format.SamplesPerPacket().
type Frame struct {
	Samples []int16
}

// Source is the injectable capture seam. ReadFrame fills the next frame of
// audio (blocking until data is available or ctx is cancelled). It returns
// ErrSourceDrained at clean end-of-stream. Real implementations wrap a CoreAudio
// / WASAPI / PipeWire capture node; the synthetic SineSource implements it for
// tests.
type Source interface {
	// Format reports the PCM layout this source produces.
	Format() Format
	// ReadFrame returns the next frame. The returned Frame is owned by the
	// caller. Implementations MUST return exactly Format().SamplesPerPacket()
	// samples on success.
	ReadFrame(ctx context.Context) (Frame, error)
}

// Sink is the injectable playback seam. WriteFrame consumes one reconstructed
// frame in stream order (the Receiver calls it once per frame slot, including
// concealed/gap-filled frames). Real implementations wrap an OS playback node;
// the in-memory BufferSink implements it for tests.
type Sink interface {
	// Format reports the PCM layout this sink expects.
	Format() Format
	// WriteFrame plays (or records, for tests) one frame.
	WriteFrame(Frame) error
}

// ---------------------------------------------------------------------------
// Synthetic Source — a sine-wave generator (real, deterministic, test-friendly).
// ---------------------------------------------------------------------------

// SineSource is a synthetic Source that generates a continuous sine wave. It is
// a real, deterministic signal generator (not a stub): it lets the transport be
// exercised end-to-end without any OS audio hardware. Every channel gets the
// same tone.
type SineSource struct {
	format    Format
	freq      float64 // tone frequency in Hz
	amplitude float64 // 0..1, scaled to int16 range
	phase     float64 // running phase accumulator (radians)
	remaining int     // frames left to emit; <0 means unbounded
}

// NewSineSource builds a sine generator at the given format and tone frequency.
// amplitude is clamped to [0,1]. maxFrames bounds the output (the source returns
// ErrSourceDrained after that many frames); pass a negative value for an endless
// stream.
func NewSineSource(format Format, freqHz, amplitude float64, maxFrames int) *SineSource {
	if amplitude < 0 {
		amplitude = 0
	}
	if amplitude > 1 {
		amplitude = 1
	}
	if format.Channels == 0 {
		format.Channels = 1
	}
	return &SineSource{
		format:    format,
		freq:      freqHz,
		amplitude: amplitude,
		remaining: maxFrames,
	}
}

// Format implements Source.
func (s *SineSource) Format() Format { return s.format }

// ReadFrame implements Source. It synthesizes the next frame of the sine wave,
// advancing the phase so successive frames are continuous (no clicks at frame
// boundaries).
func (s *SineSource) ReadFrame(_ context.Context) (Frame, error) {
	if s.remaining == 0 {
		return Frame{}, ErrSourceDrained
	}
	if s.remaining > 0 {
		s.remaining--
	}

	ch := int(s.format.Channels)
	step := 2 * math.Pi * s.freq / float64(s.format.SampleRate)
	out := make([]int16, SamplesPerFrame*ch)
	for i := 0; i < SamplesPerFrame; i++ {
		v := int16(s.amplitude * math.Sin(s.phase) * math.MaxInt16)
		for c := 0; c < ch; c++ {
			out[i*ch+c] = v
		}
		s.phase += step
		if s.phase > 2*math.Pi {
			s.phase -= 2 * math.Pi
		}
	}
	return Frame{Samples: out}, nil
}

// ---------------------------------------------------------------------------
// In-memory Sink — captures played frames for inspection in tests.
// ---------------------------------------------------------------------------

// BufferSink is an in-memory Sink that records every frame written to it, in
// order. Tests assert on Frames to verify reconstruction, ordering, and
// gap-fill. It is intentionally not safe for concurrent writers; the Receiver
// drives it from a single goroutine.
type BufferSink struct {
	format Format
	Frames []Frame
}

// NewBufferSink returns an empty BufferSink for the given format.
func NewBufferSink(format Format) *BufferSink {
	return &BufferSink{format: format}
}

// Format implements Sink.
func (b *BufferSink) Format() Format { return b.format }

// WriteFrame implements Sink by appending a copy of the frame.
func (b *BufferSink) WriteFrame(f Frame) error {
	cp := make([]int16, len(f.Samples))
	copy(cp, f.Samples)
	b.Frames = append(b.Frames, Frame{Samples: cp})
	return nil
}

// ---------------------------------------------------------------------------
// OS capture/playback — the OS-integration point in this package.
//
// Real microphone capture and speaker playback are behind the same Source/Sink
// interfaces the synthetic generators implement:
//   - Windows: WASAPI (IAudioClient capture/render) — REAL, see os_windows.go.
//   - macOS:   CoreAudio (AudioUnit / AVAudioEngine tap) — not wired.
//   - Linux:   PipeWire (pw_stream capture/playback node) — not wired.
//
// The Windows backend is real (go-wca, a pure-Go WASAPI/COM binding — no cgo,
// see os_windows.go's doc comment for the rationale). macOS/Linux are not
// verifiable in this environment and remain documented stubs (MATURITY HONESTY
// per CLAUDE.md "Maturity honesty": this is a documented stub, not a faked
// working backend, until someone implements + verifies it on that platform).
// The entry points below (NewOSCaptureSource / NewOSPlaybackSink) are
// implemented per-platform in os_windows.go / os_other.go; on an unsupported
// build they return ErrOSAudioUnavailable so a caller that asks for a real
// device fails loudly instead of silently producing fake audio.
// ---------------------------------------------------------------------------

// ErrOSAudioUnavailable signals that no real OS audio backend is wired into this
// build. Use SineSource / BufferSink for tests and the in-memory path.
var ErrOSAudioUnavailable = errors.New("audio: OS capture/playback backend not implemented in this build (stub)")

// silentFrame returns a zero-filled (silence) frame sized for the format. Used
// by the Receiver to gap-fill a lost packet when no last frame is available.
func silentFrame(f Format) Frame {
	return Frame{Samples: make([]int16, f.SamplesPerPacket())}
}
