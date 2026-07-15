//go:build darwin && cgo && cerberus_coreaudio

package audio

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

// These are the live-hardware tests for the opt-in CoreAudio backend, mirroring
// os_windows_test.go and os_linux_test.go.
//
// LIKE os_darwin.go ITSELF, THEY HAVE NEVER BEEN COMPILED OR RUN — no Mac was
// reachable from the environment they were written in. They exist so that
// whoever does have a Mac has the verification harness ready rather than having
// to invent it: run
//
//	go test -tags cerberus_coreaudio ./daemon/audio/
//
// and expect to fix things. Passing these is the bar for promoting the backend
// out of opt-in (see os_darwin_stub.go for the promotion steps).

// TestCoreAudioEnumerationSucceeds asserts the minimum bar for a real macOS
// backend: device enumeration works and returns well-formed entries. Skips
// rather than fails on a machine with no endpoints, matching the other
// platforms' policy for headless/sandboxed hosts.
func TestCoreAudioEnumerationSucceeds(t *testing.T) {
	endpoints, err := EnumerateEndpoints()
	if err != nil {
		if errors.Is(err, ErrNoAudioDevice) {
			t.Skipf("no CoreAudio endpoints on this machine: %v", err)
		}
		t.Fatalf("EnumerateEndpoints: %v", err)
	}
	if len(endpoints) == 0 {
		t.Fatal("EnumerateEndpoints returned no error but zero endpoints")
	}
	mics, speakers := 0, 0
	for _, e := range endpoints {
		if e.Name == "" {
			t.Errorf("endpoint %+v has an empty Name", e)
		}
		switch e.Kind {
		case EndpointMic:
			mics++
		case EndpointSpeaker:
			speakers++
		default:
			t.Errorf("endpoint %+v has unexpected Kind %q", e, e.Kind)
		}
	}
	t.Logf("EnumerateEndpoints: %d mic(s), %d speaker(s)", mics, speakers)
	for _, e := range endpoints {
		t.Logf("  %-8s %q", e.Kind, e.Name)
	}
}

// TestOSBackendsAreStubs_darwinReal is the CoreAudio counterpart to the other
// platforms' equivalents: with the real backend compiled in, the constructors
// must never return ErrOSAudioUnavailable — that would mean the real backend
// silently degraded to a stub. ErrNoAudioDevice is the honest failure for a
// machine with no device.
func TestOSBackendsAreStubs_darwinReal(t *testing.T) {
	f := testFormat()

	src, err := NewOSCaptureSource(f)
	switch {
	case err == nil:
		src.(*coreAudioCaptureSource).Close()
	case errors.Is(err, ErrOSAudioUnavailable):
		t.Fatalf("CoreAudio backend must not report the generic stub error: %v", err)
	case errors.Is(err, ErrNoAudioDevice):
		t.Logf("no capture device on this machine (honest failure, not a stub): %v", err)
	default:
		t.Fatalf("unexpected NewOSCaptureSource error: %v", err)
	}

	snk, err := NewOSPlaybackSink(f)
	switch {
	case err == nil:
		snk.(*coreAudioPlaybackSink).Close()
	case errors.Is(err, ErrOSAudioUnavailable):
		t.Fatalf("CoreAudio backend must not report the generic stub error: %v", err)
	case errors.Is(err, ErrNoAudioDevice):
		t.Logf("no render device on this machine (honest failure, not a stub): %v", err)
	default:
		t.Fatalf("unexpected NewOSPlaybackSink error: %v", err)
	}
}

// TestOSPlaybackSink_liveSpeaker_darwin writes real sine frames to the real
// default speaker.
func TestOSPlaybackSink_liveSpeaker_darwin(t *testing.T) {
	f := testFormat()
	snk, err := NewOSPlaybackSink(f)
	if err != nil {
		if errors.Is(err, ErrNoAudioDevice) {
			t.Skipf("no render device on this machine: %v", err)
		}
		t.Fatalf("NewOSPlaybackSink: %v", err)
	}
	sink := snk.(*coreAudioPlaybackSink)
	defer sink.Close()

	if got := snk.Format(); got != f {
		t.Fatalf("Format() = %+v, want %+v", got, f)
	}

	src := NewSineSource(f, 440, 0.2, 25)
	for i := 0; i < 25; i++ {
		frame, err := src.ReadFrame(context.Background())
		if err != nil {
			t.Fatalf("sine source frame %d: %v", i, err)
		}
		if err := snk.WriteFrame(frame); err != nil {
			t.Fatalf("WriteFrame %d: %v", i, err)
		}
	}
	t.Logf("wrote 25 frames (250ms of 440Hz) to the real speaker; underruns=%d", sink.Underruns())
}

// TestOSCaptureSource_liveMic_darwin reads real frames from the real default
// microphone. Signal level is LOGGED, not asserted: a silent room or a muted mic
// is a valid environment, and asserting non-silence would be flaky for reasons
// unrelated to correctness.
//
// NOTE for whoever runs this: macOS gates microphone access behind TCC. A
// process without the Microphone entitlement/permission gets a silent (all-zero)
// stream rather than an error, so if rms is exactly 0, check
// System Settings > Privacy & Security > Microphone for the terminal/test binary
// before concluding the backend is broken.
func TestOSCaptureSource_liveMic_darwin(t *testing.T) {
	f := testFormat()
	src, err := NewOSCaptureSource(f)
	if err != nil {
		if errors.Is(err, ErrNoAudioDevice) {
			t.Skipf("no capture device on this machine: %v", err)
		}
		t.Fatalf("NewOSCaptureSource: %v", err)
	}
	mic := src.(*coreAudioCaptureSource)
	defer mic.Close()

	if got := src.Format(); got != f {
		t.Fatalf("Format() = %+v, want %+v", got, f)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var sumSq float64
	var peak int16
	var n int
	const frames = 20
	for i := 0; i < frames; i++ {
		frame, err := src.ReadFrame(ctx)
		if err != nil {
			t.Fatalf("ReadFrame %d: %v", i, err)
		}
		if len(frame.Samples) != f.SamplesPerPacket() {
			t.Fatalf("frame %d has %d samples, want %d", i, len(frame.Samples), f.SamplesPerPacket())
		}
		for _, v := range frame.Samples {
			sumSq += float64(v) * float64(v)
			if v > peak {
				peak = v
			}
			n++
		}
	}
	rms := 0.0
	if n > 0 {
		rms = math.Sqrt(sumSq / float64(n))
	}
	t.Logf("live mic: %d frames (%d samples), rms=%.1f peak=%d dropped=%d", frames, n, rms, peak, mic.Dropped())
	if rms == 0 {
		t.Log("note: pure digital silence — check macOS Microphone permission (TCC) for this binary, or the room/mic really is silent")
	}
}

// TestCoreAudioCaptureToPlaybackRoundTrip wires the real mic through this
// package's actual Sender -> Transport -> Receiver -> Sink pipeline to the real
// speaker, the strongest live proof for this backend.
func TestCoreAudioCaptureToPlaybackRoundTrip(t *testing.T) {
	f := testFormat()
	src, err := NewOSCaptureSource(f)
	if err != nil {
		if errors.Is(err, ErrNoAudioDevice) {
			t.Skipf("no capture device on this machine: %v", err)
		}
		t.Fatalf("NewOSCaptureSource: %v", err)
	}
	mic := src.(*coreAudioCaptureSource)
	defer mic.Close()

	speaker, err := NewOSPlaybackSink(f)
	if err != nil {
		if errors.Is(err, ErrNoAudioDevice) {
			t.Skipf("no render device on this machine: %v", err)
		}
		t.Fatalf("NewOSPlaybackSink: %v", err)
	}
	spk := speaker.(*coreAudioPlaybackSink)
	defer spk.Close()

	inspect := NewBufferSink(f)
	fanOut := &fanOutSink{format: f, sinks: []Sink{speaker, inspect}}

	tx := NewMemTransport(64)
	sender := NewSender(src, tx)
	receiver := NewReceiver(tx, fanOut, ReceiverConfig{TargetFrames: 2, MaxFrames: 16})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	const wantFrames = 20
	senderErr := make(chan error, 1)
	go func() {
		for i := 0; i < wantFrames; i++ {
			if err := sender.SendFrame(ctx); err != nil {
				senderErr <- err
				return
			}
		}
		tx.Close()
		senderErr <- nil
	}()

	if err := receiver.Run(ctx); err != nil {
		t.Fatalf("receiver run: %v", err)
	}
	if err := <-senderErr; err != nil {
		t.Fatalf("sender: %v", err)
	}

	if len(inspect.Frames) != wantFrames {
		t.Fatalf("got %d frames through the live capture->sender->receiver->sink pipeline, want %d", len(inspect.Frames), wantFrames)
	}
	t.Logf("live CoreAudio capture -> Sender -> jitter buffer -> Receiver -> real speaker: %d frames round-tripped (mic drops=%d, speaker underruns=%d)",
		len(inspect.Frames), mic.Dropped(), spk.Underruns())
}
