//go:build linux

// This file is the real Linux half of the OS capture/playback boundary
// described in audio.go: a PulseAudio-protocol Source/Sink pair that a live
// Sender/Receiver pipeline can use as a drop-in replacement for
// SineSource/BufferSink, exactly like os_windows.go's WASAPI pair.
//
// # Library choice: PulseAudio's native protocol, in pure Go
//
// github.com/jfreymuth/pulse speaks PulseAudio's native wire protocol directly
// over the server's unix socket. It is plain Go with no dependencies of its own
// and — this is the point — NO cgo. That preserves the property os_windows.go's
// doc comment argues for at length: daemon/audio is a leaf package that builds
// with CGO_ENABLED=0 and needs no C toolchain on any platform. The
// cross-compile-from-anywhere release path (build/release.sh, which pins
// CGO_ENABLED=0) therefore keeps working for linux/amd64 and linux/arm64 with
// full audio support, rather than needing native runners or a cross-C-compiler.
//
// The alternatives, and why not:
//
//   - ALSA (libasound) is the usual "lowest common denominator" answer, but it
//     is the wrong LAYER here and would cost cgo. On any machine with a sound
//     server — i.e. every desktop, which is what a mic/speaker-sharing feature
//     targets — PulseAudio/PipeWire owns the ALSA device. An ALSA client would
//     be fighting the sound server for the hardware, and would bypass the user's
//     per-application routing and volume. Cerberus wants to be a well-behaved
//     application-level audio client, which means talking to the sound server,
//     not around it. libasound also has no pure-Go binding (unlike WASAPI, whose
//     COM vtables go-wca can drive over syscall, ALSA is a C library with no
//     stable wire protocol to reimplement), so it would force cgo.
//   - PipeWire's native API (pw_stream) is the modern default on current
//     distros, but it is a C library, so again cgo — and it is unnecessary,
//     because PipeWire ships pipewire-pulse, a PulseAudio-protocol-compatible
//     server that is enabled by default on every PipeWire distro. A PulseAudio
//     protocol client therefore covers BOTH classic PulseAudio and PipeWire
//     systems with one pure-Go implementation. That is the whole reason this
//     backend targets the protocol rather than either daemon's C API.
//
// A machine with no sound server at all (a headless box with bare ALSA, or one
// where neither daemon is running) gets ErrNoAudioDevice — loudly — rather than
// a fabricated device list or silent fake audio (CLAUDE.md "maturity honesty").
//
// # Sample-rate handling differs from Windows ON PURPOSE
//
// os_windows.go refuses to resample and returns ErrSampleRateMismatch when the
// endpoint's native rate differs from the requested Format's, because honoring
// it would mean this package writing a naive resampler that silently degrades
// pitch/quality. That reasoning does not apply here: sample-rate conversion is a
// first-class, well-implemented feature of the sound server itself (PulseAudio's
// speex/soxr resamplers; PipeWire's own), performed server-side by code whose
// entire job is doing it correctly. Asking the server for 48 kHz on a 44.1 kHz
// device is a normal, supported request, not a silent lie — so this backend
// makes it rather than failing. This was verified live: on a 44.1 kHz endpoint,
// a 48 kHz stream request is honored and reports back as 48 kHz.
//
// What this backend does NOT do is accept a format the server did not actually
// grant: the negotiated rate/channel count is read back off the created stream
// and checked against what was asked for, and a mismatch is a hard error (see
// verifyPulseNegotiation). The server converts, or the caller finds out.
package audio

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jfreymuth/pulse"
	"github.com/jfreymuth/pulse/proto"
)

const (
	// pulseAppName is how this process identifies itself to the sound server.
	// It is what shows up in `pactl list clients` and in pavucontrol's
	// Recording/Playback tabs, so a user can see — and re-route or mute —
	// Cerberus's streams like any other application's. That visibility is a
	// feature: an audio stream a user cannot see is one they cannot revoke.
	pulseAppName = "cerberus"

	// pulseCaptureQueueFrames bounds how many captured frames may sit unread in
	// the Source's hand-off queue before the oldest are dropped. Capture is a
	// live signal with no backpressure available (the microphone does not wait),
	// so a slow consumer must lose audio somewhere; dropping the OLDEST bounds
	// latency at ~320 ms and keeps the freshest audio, which is the right
	// trade for a live voice stream. Dropping is counted, never silent-by-design
	// (see Dropped).
	pulseCaptureQueueFrames = 32

	// pulsePlaybackQueueFrames bounds the Sink's hand-off queue. Unlike capture,
	// playback CAN apply backpressure: WriteFrame blocks when the queue is full,
	// which is exactly the flow control the Receiver wants.
	pulsePlaybackQueueFrames = 8

	// pulsePlaybackLatencyFrames is the target buffer depth requested from the
	// server for the render stream. Five 10 ms frames (50 ms) is enough headroom
	// to ride out scheduling jitter without underrunning, while staying small
	// enough not to add audible delay to a conversation.
	pulsePlaybackLatencyFrames = 5

	// pulseUnderrunWait is how long the render callback waits for the next frame
	// before concealing the gap with silence. See pulsePlaybackSink.read for why
	// waiting forever is not an option.
	pulseUnderrunWait = 10 * time.Millisecond

	// pulseWriteTimeout bounds WriteFrame's block on a full queue. The queue
	// drains in real time (~80 ms when full), so a wait this long means the
	// render stream has stopped consuming — a dead stream must surface as an
	// error rather than hanging the caller forever.
	pulseWriteTimeout = time.Second
)

// frameSeconds is one SamplesPerFrame frame's duration in seconds, the unit the
// pulse library's latency options are expressed in.
func frameSeconds(f Format) float64 {
	return float64(SamplesPerFrame) / float64(f.SampleRate)
}

// pulseChannelMap maps this package's channel COUNT onto the positional channel
// map PulseAudio requires. Mono and stereo are the formats this package's
// Format realistically carries (see audio.go), and they have unambiguous
// standard layouts. For anything wider there is no single right answer — a
// 4-channel stream could be quad, 3.1, or ambisonic — and guessing would put
// samples in the wrong speakers, so this fails cleanly instead of inventing a
// layout.
func pulseChannelMap(channels int) (proto.ChannelMap, error) {
	switch channels {
	case 1:
		return proto.ChannelMap{proto.ChannelMono}, nil
	case 2:
		return proto.ChannelMap{proto.ChannelLeft, proto.ChannelRight}, nil
	default:
		return nil, fmt.Errorf("audio: PulseAudio backend supports mono or stereo, not %d channels (no unambiguous channel map)", channels)
	}
}

// normalizePulseFormat applies the same defaulting rules as the WASAPI backend
// (absent channel count means mono; a zero sample rate is a caller bug, not a
// thing to guess at) and resolves the channel map up front, so a bad format
// fails before a server connection is made.
func normalizePulseFormat(f Format, who string) (Format, proto.ChannelMap, error) {
	if f.Channels == 0 {
		f.Channels = 1
	}
	if f.SampleRate == 0 {
		return f, nil, fmt.Errorf("audio: %s: format has zero SampleRate", who)
	}
	cmap, err := pulseChannelMap(int(f.Channels))
	if err != nil {
		return f, nil, err
	}
	return f, cmap, nil
}

// dialPulse connects to the sound server. An unreachable server is reported as
// ErrNoAudioDevice (wrapping the dial cause): from a caller's point of view
// "there is no sound server here" and "there are no devices here" are the same
// answer — no audio is available on this machine — and it is the answer the
// WASAPI backend gives in the same situation.
func dialPulse(who string) (*pulse.Client, error) {
	client, err := pulse.NewClient(pulse.ClientApplicationName(pulseAppName))
	if err != nil {
		return nil, fmt.Errorf("%w: %s: no reachable PulseAudio/PipeWire sound server: %v", ErrNoAudioDevice, who, err)
	}
	return client, nil
}

// verifyPulseNegotiation asserts the server actually granted the format that
// was asked for. The server is allowed to CONVERT (resample, remap channels) —
// that is the documented, wanted behavior — but it is not allowed to hand back
// a stream in a different format than the one this Source/Sink advertises via
// Format(), because everything downstream (frame sizing, the Receiver's DLL
// timing math) trusts that advertisement.
func verifyPulseNegotiation(who string, gotRate, gotChannels int, want Format) error {
	if gotRate != int(want.SampleRate) || gotChannels != int(want.Channels) {
		return fmt.Errorf("audio: %s: sound server granted %d Hz/%d ch but %d Hz/%d ch was requested",
			who, gotRate, gotChannels, want.SampleRate, want.Channels)
	}
	return nil
}

// pulseEndpointName picks the friendliest available label for an endpoint,
// mirroring endpointFriendlyName's policy on Windows: prefer the human
// description the server reports (PulseAudio's `device.description`, e.g.
// "Built-in Audio Analog Stereo" — the same string pavucontrol shows), fall
// back to the internal id, and fall back again to a stable synthetic name
// rather than dropping a real device that the server could not describe.
func pulseEndpointName(description, id string, kind EndpointKind, index int) string {
	if description != "" {
		return description
	}
	if id != "" {
		return id
	}
	return fmt.Sprintf("%s %d", kind, index)
}

// EnumerateEndpoints lists every microphone and speaker the sound server knows
// about. This backs the 9P namespace registration in daemon/system.Compose
// (each endpoint becomes a /cer/dev/audio/<mic|speaker>/<index> device) and the
// tray's device list, and it is ordered mics-then-speakers to match the WASAPI
// backend so the two platforms index devices the same way.
//
// Sink MONITORS are deliberately excluded from the microphone list. PulseAudio
// exposes a ".monitor" source for every sink (a loopback of whatever that
// speaker is playing), and those sources are real and usable — but they are not
// microphones, they are system-audio taps. Listing them as mics would put
// phantom "microphones" in `cerberus devices` that actually capture the
// machine's output, which is both confusing and a privacy trap for anyone who
// grants a capability on one thinking it is a mic. They are identified
// structurally rather than by name-sniffing for a ".monitor" suffix: the
// server reports monitor_of_sink for exactly these sources (verified live: a
// real mic reports proto.Undefined here, a sink monitor reports its sink's
// index).
//
// Returns ErrNoAudioDevice if the server reports no mic AND no speaker, or if
// no server is reachable at all — the same "loud, honest, no fake devices"
// contract the WASAPI backend has.
func EnumerateEndpoints() ([]EndpointInfo, error) {
	client, err := dialPulse("EnumerateEndpoints")
	if err != nil {
		return nil, err
	}
	defer client.Close()

	// The pulse package's own Source/Sink wrappers do not expose monitor_of_sink,
	// so the raw info-list replies are used instead — the same requests
	// ListSources/ListSinks make, just without discarding the field that
	// distinguishes a microphone from a speaker tap.
	var sources proto.GetSourceInfoListReply
	if err := client.RawRequest(&proto.GetSourceInfoList{}, &sources); err != nil {
		return nil, fmt.Errorf("audio: PulseAudio GetSourceInfoList: %w", err)
	}
	var sinks proto.GetSinkInfoListReply
	if err := client.RawRequest(&proto.GetSinkInfoList{}, &sinks); err != nil {
		return nil, fmt.Errorf("audio: PulseAudio GetSinkInfoList: %w", err)
	}

	out := make([]EndpointInfo, 0, len(sources)+len(sinks))
	mics := 0
	for _, s := range sources {
		if s.MonitorSourceIndex != proto.Undefined {
			continue // a sink monitor, not a microphone — see the doc comment.
		}
		out = append(out, EndpointInfo{Name: pulseEndpointName(s.Device, s.SourceName, EndpointMic, mics), Kind: EndpointMic})
		mics++
	}
	for i, s := range sinks {
		out = append(out, EndpointInfo{Name: pulseEndpointName(s.Device, s.SinkName, EndpointSpeaker, i), Kind: EndpointSpeaker})
	}
	if len(out) == 0 {
		return nil, ErrNoAudioDevice
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Capture (microphone) — pulseCaptureSource implements Source.
// ---------------------------------------------------------------------------

// pulseCaptureSource is a real Source backed by the sound server's default
// capture source.
//
// The impedance mismatch this type exists to solve: the pulse library PUSHES
// captured audio into a callback on its own protocol goroutine, in whatever
// chunk sizes the server sends, while the Source interface is PULL-based
// (ReadFrame blocks until exactly one frame is ready). So the callback
// re-slices the server's chunks into exact SamplesPerFrame frames and hands
// them to ReadFrame over a bounded channel.
type pulseCaptureSource struct {
	format Format
	client *pulse.Client
	stream *pulse.RecordStream

	// frames carries whole, exactly-sized frames from the library's callback
	// goroutine to ReadFrame's caller.
	frames chan Frame

	// mu guards pending and dropped, which the callback mutates.
	mu      sync.Mutex
	pending []int16 // leftover samples that did not fill a whole frame
	dropped uint64

	closeCh   chan struct{}
	closeOnce sync.Once
}

// NewOSCaptureSource opens the sound server's default microphone at f's sample
// rate and channel count as 16-bit PCM. The server resamples/remaps as needed
// (see the file doc comment — this is deliberate and unlike the WASAPI path).
//
// It returns ErrNoAudioDevice if no sound server is reachable or there is no
// default capture source, rather than returning a Source that fabricates audio.
func NewOSCaptureSource(f Format) (Source, error) {
	f, cmap, err := normalizePulseFormat(f, "NewOSCaptureSource")
	if err != nil {
		return nil, err
	}

	client, err := dialPulse("NewOSCaptureSource")
	if err != nil {
		return nil, err
	}

	dev, err := client.DefaultSource()
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("%w: no default capture source: %v", ErrNoAudioDevice, err)
	}

	s := &pulseCaptureSource{
		format:  f,
		client:  client,
		frames:  make(chan Frame, pulseCaptureQueueFrames),
		closeCh: make(chan struct{}),
	}

	// Option order matters: RecordLatency derives its fragment size from the
	// rate and channel count set by the options before it, so it must come last
	// (documented on RecordLatency itself). One frame of latency asks the server
	// to hand us chunks the same size as the frames we emit.
	stream, err := client.NewRecord(pulse.Int16Writer(s.write),
		pulse.RecordSource(dev),
		pulse.RecordSampleRate(int(f.SampleRate)),
		pulse.RecordChannels(cmap),
		pulse.RecordLatency(frameSeconds(f)),
		pulse.RecordMediaName("Cerberus microphone capture"),
	)
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("audio: PulseAudio NewRecord: %w", err)
	}
	if err := verifyPulseNegotiation("NewOSCaptureSource", stream.SampleRate(), stream.Channels(), f); err != nil {
		stream.Close()
		client.Close()
		return nil, err
	}

	s.stream = stream
	stream.Start()
	return s, nil
}

// write is the library's capture callback. It runs on the pulse client's
// protocol goroutine, so it must not block: a stall here stalls the whole
// connection.
func (s *pulseCaptureSource) write(b []int16) (int, error) {
	per := s.format.SamplesPerPacket()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.pending = append(s.pending, b...)
	off := 0
	for len(s.pending)-off >= per {
		frame := Frame{Samples: append([]int16(nil), s.pending[off:off+per]...)}
		off += per
		s.enqueue(frame)
	}
	if off > 0 {
		// Compact in place; re-slicing alone would let the backing array grow
		// without bound across a long-lived stream.
		rem := copy(s.pending, s.pending[off:])
		s.pending = s.pending[:rem]
	}
	return len(b), nil
}

// enqueue hands one frame to ReadFrame, dropping the oldest queued frame if the
// consumer has fallen behind. Caller holds s.mu. It never blocks (see write).
func (s *pulseCaptureSource) enqueue(f Frame) {
	select {
	case s.frames <- f:
		return
	default:
	}
	// Queue full: discard the oldest frame to bound latency, then retry once.
	select {
	case <-s.frames:
		s.dropped++
	default:
	}
	select {
	case s.frames <- f:
	default:
		// Lost the race to a concurrent reader refilling the queue; drop the new
		// frame instead. Either way the drop is counted, never silent.
		s.dropped++
	}
}

// Dropped reports how many captured frames were discarded because the consumer
// did not keep up. It is exposed so an overrun is observable rather than a
// silent quality loss; a healthy live capture reports zero.
func (s *pulseCaptureSource) Dropped() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

// Format implements Source.
func (s *pulseCaptureSource) Format() Format { return s.format }

// ReadFrame implements Source, blocking until the capture callback has produced
// a whole frame, ctx is cancelled, or the stream is closed.
func (s *pulseCaptureSource) ReadFrame(ctx context.Context) (Frame, error) {
	select {
	case f := <-s.frames:
		return f, nil
	case <-s.closeCh:
		return Frame{}, ErrOSAudioUnavailable
	case <-ctx.Done():
		return Frame{}, ctx.Err()
	}
}

// Close stops the capture stream and releases the server connection. Safe to
// call multiple times.
func (s *pulseCaptureSource) Close() error {
	s.closeOnce.Do(func() {
		close(s.closeCh)
		s.stream.Stop()
		s.stream.Close()
		s.client.Close()
	})
	return nil
}

// ---------------------------------------------------------------------------
// Playback (speaker) — pulsePlaybackSink implements Sink.
// ---------------------------------------------------------------------------

// pulsePlaybackSink is a real Sink backed by the sound server's default sink.
//
// This is the mirror image of pulseCaptureSource's problem: the pulse library
// PULLS audio from a callback whenever the server asks for more, while the Sink
// interface is PUSH-based (WriteFrame hands over one frame). WriteFrame
// therefore queues frames and the callback drains them.
type pulsePlaybackSink struct {
	format Format
	client *pulse.Client
	stream *pulse.PlaybackStream

	frames chan Frame

	// pending is the tail of a frame the last read could not fit. Touched only
	// by the library's render goroutine, so it needs no lock.
	pending []int16

	closeCh   chan struct{}
	closeOnce sync.Once

	// startOnce uncorks the stream on the first WriteFrame rather than at
	// construction. See WriteFrame for why.
	startOnce sync.Once

	mu        sync.Mutex
	underruns uint64
}

// NewOSPlaybackSink opens the sound server's default speaker at f's sample rate
// and channel count as 16-bit PCM. It returns ErrNoAudioDevice if no sound
// server is reachable or there is no default sink.
func NewOSPlaybackSink(f Format) (Sink, error) {
	f, cmap, err := normalizePulseFormat(f, "NewOSPlaybackSink")
	if err != nil {
		return nil, err
	}

	client, err := dialPulse("NewOSPlaybackSink")
	if err != nil {
		return nil, err
	}

	dev, err := client.DefaultSink()
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("%w: no default playback sink: %v", ErrNoAudioDevice, err)
	}

	s := &pulsePlaybackSink{
		format:  f,
		client:  client,
		frames:  make(chan Frame, pulsePlaybackQueueFrames),
		closeCh: make(chan struct{}),
	}

	// As with capture, PlaybackLatency must follow the rate/channel options.
	stream, err := client.NewPlayback(pulse.Int16Reader(s.read),
		pulse.PlaybackSink(dev),
		pulse.PlaybackSampleRate(int(f.SampleRate)),
		pulse.PlaybackChannels(cmap),
		pulse.PlaybackLatency(pulsePlaybackLatencyFrames*frameSeconds(f)),
		pulse.PlaybackMediaName("Cerberus speaker playback"),
	)
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("audio: PulseAudio NewPlayback: %w", err)
	}
	if err := verifyPulseNegotiation("NewOSPlaybackSink", stream.SampleRate(), stream.Channels(), f); err != nil {
		stream.Close()
		client.Close()
		return nil, err
	}

	// NOTE: the stream is deliberately NOT started here — WriteFrame starts it.
	// The pulse library creates streams corked, and a corked stream is not asked
	// for audio, so leaving it corked until there is actually something to play
	// means the render callback is never called before the first frame exists.
	s.stream = stream
	return s, nil
}

// read is the library's render callback, run on the stream's own goroutine. It
// fills out with queued frames and reports how many samples it wrote.
//
// Two hard constraints shape this, both learned from the library's run loop:
//
//  1. It must NEVER return (0, nil). The loop treats a zero-length, error-free
//     read as "try again immediately" and would spin the CPU forever.
//  2. It must not block indefinitely. The client's protocol goroutine hands
//     this stream its refill requests over an unbuffered channel, so a render
//     callback parked forever would wedge the whole connection.
//
// Together those mean a starved callback has to emit SOMETHING. It waits
// pulseUnderrunWait for real audio first and only then fills the gap with
// silence — the same concealment this package's Receiver already applies to a
// lost packet (see audio.go), and the standard behavior of every real audio
// backend on underrun. That is gap-filling a real stream that is momentarily
// starved, not fabricating a device: an underrun is counted (see Underruns) and
// only ever happens after a genuine attempt to get real samples.
//
// Returned counts are always a whole number of sample-frames, so channel
// interleaving can never drift out of phase.
func (s *pulsePlaybackSink) read(out []int16) (int, error) {
	ch := int(s.format.Channels)
	usable := (len(out) / ch) * ch
	if usable == 0 {
		// The server asked for less than one sample-frame. There is no aligned
		// audio to give; zero it so the caller gets a defined value and cannot
		// spin on a (0, nil) return.
		for i := range out {
			out[i] = 0
		}
		return len(out), nil
	}

	n := 0
	for n < usable {
		if len(s.pending) == 0 {
			// Fast path: a frame is already queued.
			select {
			case f := <-s.frames:
				s.pending = f.Samples
				continue
			default:
			}
			// Nothing queued right now. If we already have some audio, hand it
			// over — a short (but frame-aligned) read is legal, and the server
			// will simply ask again.
			if n > 0 {
				return n, nil
			}
			// Nothing at all to give: wait briefly for the producer.
			timer := time.NewTimer(pulseUnderrunWait)
			select {
			case f := <-s.frames:
				timer.Stop()
				s.pending = f.Samples
				continue
			case <-s.closeCh:
				timer.Stop()
				return 0, pulse.EndOfData
			case <-timer.C:
				s.mu.Lock()
				s.underruns++
				s.mu.Unlock()
				for i := 0; i < usable; i++ {
					out[i] = 0
				}
				return usable, nil
			}
		}
		c := copy(out[n:usable], s.pending)
		s.pending = s.pending[c:]
		n += c
	}
	return n, nil
}

// Underruns reports how many times the render callback ran out of audio and had
// to conceal the gap with silence. Exposed so starvation is observable instead
// of merely sounding bad.
//
// Expect a SMALL NON-ZERO count (typically 1) on a perfectly healthy stream, and
// do not treat that as a fault. When a stream starts, the server asks for a full
// buffer's worth of audio (pulsePlaybackLatencyFrames) up front, and the pulse
// library's Start blocks the caller until the server acknowledges — so the
// producer cannot possibly have supplied that much yet, and the balance is
// primed with silence. That is standard prebuffering, and is exactly what the
// WASAPI backend does explicitly in renderSilence before its own Start.
//
// What is worth acting on is this count CONTINUING to climb once a stream is
// established: that means the producer is not keeping up in steady state.
func (s *pulsePlaybackSink) Underruns() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.underruns
}

// Format implements Sink.
func (s *pulsePlaybackSink) Format() Format { return s.format }

// WriteFrame implements Sink by queueing the frame for the render callback,
// blocking while the queue is full so the caller inherits the device's natural
// pacing (the flow control the Receiver relies on).
//
// The first call also uncorks the stream, queueing that first frame before doing
// so. Streams are created corked, and starting on demand rather than at
// construction buys two things: a Sink that is opened but not yet fed costs
// nothing (a started stream would have its render callback spinning out silence
// at ~100 Hz for as long as it went unused), and the first audio the server is
// handed is real rather than primed silence. It does NOT eliminate the startup
// prime — the server asks for a whole buffer up front and pulse's Start blocks
// until it acknowledges, so one frame cannot cover it — see Underruns.
func (s *pulsePlaybackSink) WriteFrame(f Frame) error {
	ch := int(s.format.Channels)
	if ch > 0 && len(f.Samples)%ch != 0 {
		return fmt.Errorf("audio: NewOSPlaybackSink: frame of %d samples is not a whole number of %d-channel sample-frames", len(f.Samples), ch)
	}
	if len(f.Samples) == 0 {
		return nil
	}

	// The frame is handed to another goroutine, so it must not alias a buffer
	// the caller may reuse for the next frame.
	cp := append([]int16(nil), f.Samples...)

	// Queue the first frame BEFORE uncorking, so the server's very first refill
	// request already has real audio waiting for it.
	s.startOnce.Do(func() {
		select {
		case s.frames <- Frame{Samples: cp}:
			cp = nil
		default:
		}
		s.stream.Start()
	})
	if cp == nil {
		return nil
	}

	timer := time.NewTimer(pulseWriteTimeout)
	defer timer.Stop()
	select {
	case s.frames <- Frame{Samples: cp}:
		return nil
	case <-s.closeCh:
		return ErrOSAudioUnavailable
	case <-timer.C:
		return fmt.Errorf("audio: PulseAudio playback stream stopped consuming (blocked %s); server or stream is gone", pulseWriteTimeout)
	}
}

// Close stops the playback stream and releases the server connection. Safe to
// call multiple times.
func (s *pulsePlaybackSink) Close() error {
	s.closeOnce.Do(func() {
		close(s.closeCh)
		s.stream.Stop()
		s.stream.Close()
		s.client.Close()
	})
	return nil
}
