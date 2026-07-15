//go:build linux

package audio

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/jfreymuth/pulse"
	"github.com/jfreymuth/pulse/proto"
)

// requirePulse skips the calling test unless a PulseAudio/PipeWire server is
// actually reachable. A sound server is a property of the ENVIRONMENT, not of
// this code: a headless CI runner (GitHub's ubuntu-latest, a container, a build
// box) legitimately has none, and a bare-ALSA embedded system never will. Those
// hosts must skip rather than fail — the same policy os_windows_test.go applies
// to a machine with no WASAPI endpoint. Tests that do NOT need hardware (the
// channel-map and stub-contract tests below) deliberately avoid this helper so
// they still run everywhere.
func requirePulse(t *testing.T) *pulse.Client {
	t.Helper()
	client, err := pulse.NewClient(pulse.ClientApplicationName("cerberus-test"))
	if err != nil {
		t.Skipf("no PulseAudio/PipeWire server reachable on this host (headless CI or bare-ALSA box): %v", err)
	}
	return client
}

// TestPulseServerReachable asserts the minimum bar for the real Linux backend:
// the sound server connection itself works. It talks to the pulse library
// directly rather than through this package's constructors, so a failure here
// isolates to "there is no usable sound server" rather than anything in
// os_linux.go's own logic — the same isolation TestWASAPIEnumerationSucceeds
// provides on Windows.
func TestPulseServerReachable(t *testing.T) {
	client := requirePulse(t)
	defer client.Close()

	sources, err := client.ListSources()
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	sinks, err := client.ListSinks()
	if err != nil {
		t.Fatalf("ListSinks: %v", err)
	}
	t.Logf("sound server reachable: %d source(s), %d sink(s)", len(sources), len(sinks))
	for _, s := range sources {
		t.Logf("  source: id=%q name=%q rate=%d", s.ID(), s.Name(), s.SampleRate())
	}
	for _, s := range sinks {
		t.Logf("  sink:   id=%q name=%q rate=%d", s.ID(), s.Name(), s.SampleRate())
	}
}

// TestEnumerateEndpointsSucceeds_linux proves the public EnumerateEndpoints —
// the function daemon/system.Compose calls to register real devices into the 9P
// namespace — actually talks to the sound server on this machine and returns
// well-formed entries.
func TestEnumerateEndpointsSucceeds_linux(t *testing.T) {
	client := requirePulse(t)
	client.Close()

	endpoints, err := EnumerateEndpoints()
	if err != nil {
		if errors.Is(err, ErrNoAudioDevice) {
			t.Skipf("sound server reachable but reports no endpoints: %v", err)
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

// TestEnumerateEndpointsExcludesMonitors is the real test of this backend's
// monitor-filtering policy (see EnumerateEndpoints' doc comment): PulseAudio
// exposes a ".monitor" source for every sink, and those are speaker taps rather
// than microphones. Listing one as a mic would be both misleading and a privacy
// trap, so this cross-checks the public listing against the server's own raw
// source list: the number of reported mics must equal the number of sources
// that are NOT monitors, and no monitor's description may appear as a mic.
func TestEnumerateEndpointsExcludesMonitors(t *testing.T) {
	client := requirePulse(t)
	defer client.Close()

	var raw proto.GetSourceInfoListReply
	if err := client.RawRequest(&proto.GetSourceInfoList{}, &raw); err != nil {
		t.Fatalf("GetSourceInfoList: %v", err)
	}

	wantMics := 0
	monitorNames := map[string]bool{}
	for _, s := range raw {
		if s.MonitorSourceIndex != proto.Undefined {
			monitorNames[s.Device] = true
			continue
		}
		wantMics++
	}
	if len(monitorNames) == 0 {
		t.Log("note: this host exposes no sink monitors, so the exclusion path is not exercised here")
	}

	endpoints, err := EnumerateEndpoints()
	if err != nil {
		if errors.Is(err, ErrNoAudioDevice) {
			t.Skipf("no endpoints on this host: %v", err)
		}
		t.Fatalf("EnumerateEndpoints: %v", err)
	}

	gotMics := 0
	for _, e := range endpoints {
		if e.Kind != EndpointMic {
			continue
		}
		gotMics++
		if monitorNames[e.Name] {
			t.Errorf("sink monitor %q was reported as a microphone; monitors are speaker taps, not mics", e.Name)
		}
	}
	if gotMics != wantMics {
		t.Errorf("EnumerateEndpoints reported %d mic(s), want %d (non-monitor sources); monitor filtering is wrong", gotMics, wantMics)
	}
	t.Logf("monitor filtering: %d raw source(s) -> %d mic(s), %d monitor(s) excluded", len(raw), gotMics, len(monitorNames))
}

// TestPulseChannelMapRejectsUnsupported pins the honest failure for a channel
// count with no unambiguous layout. This needs no hardware, so it runs
// everywhere — including headless CI.
func TestPulseChannelMapRejectsUnsupported(t *testing.T) {
	if _, err := pulseChannelMap(1); err != nil {
		t.Errorf("mono must be supported: %v", err)
	}
	if _, err := pulseChannelMap(2); err != nil {
		t.Errorf("stereo must be supported: %v", err)
	}
	for _, ch := range []int{0, 3, 6} {
		if _, err := pulseChannelMap(ch); err == nil {
			t.Errorf("pulseChannelMap(%d) must fail rather than invent a channel layout", ch)
		}
	}
}

// TestVerifyPulseNegotiation checks the guard that keeps the sound server
// honest: converting is fine, but silently handing back a different format than
// the one this Source/Sink advertises is not. No hardware needed.
func TestVerifyPulseNegotiation(t *testing.T) {
	f := testFormat()
	if err := verifyPulseNegotiation("t", 48000, 1, f); err != nil {
		t.Errorf("matching negotiation must pass: %v", err)
	}
	if err := verifyPulseNegotiation("t", 44100, 1, f); err == nil {
		t.Error("a granted rate different from the requested one must fail loudly")
	}
	if err := verifyPulseNegotiation("t", 48000, 2, f); err == nil {
		t.Error("a granted channel count different from the requested one must fail loudly")
	}
}

// TestOSBackendsAreStubs_linux is the Linux counterpart to os_other_test.go's
// TestOSBackendsAreStubs: on Linux the backend is REAL, so the constructors must
// never return ErrOSAudioUnavailable — that would mean the real backend silently
// degraded into a fake stub. ErrNoAudioDevice is the honest failure for a box
// with no sound server, and is accepted here.
func TestOSBackendsAreStubs_linux(t *testing.T) {
	f := testFormat()

	src, err := NewOSCaptureSource(f)
	switch {
	case err == nil:
		src.(*pulseCaptureSource).Close()
	case errors.Is(err, ErrOSAudioUnavailable):
		t.Fatalf("Linux backend must not report the generic stub error: %v", err)
	case errors.Is(err, ErrNoAudioDevice):
		t.Logf("no capture device on this host (honest failure, not a stub): %v", err)
	default:
		t.Fatalf("unexpected NewOSCaptureSource error: %v", err)
	}

	snk, err := NewOSPlaybackSink(f)
	switch {
	case err == nil:
		snk.(*pulsePlaybackSink).Close()
	case errors.Is(err, ErrOSAudioUnavailable):
		t.Fatalf("Linux backend must not report the generic stub error: %v", err)
	case errors.Is(err, ErrNoAudioDevice):
		t.Logf("no render device on this host (honest failure, not a stub): %v", err)
	default:
		t.Fatalf("unexpected NewOSPlaybackSink error: %v", err)
	}
}

// TestOSPlaybackSink_liveSpeaker_linux opens the real default speaker and
// writes real frames of a sine tone through it.
func TestOSPlaybackSink_liveSpeaker_linux(t *testing.T) {
	requirePulse(t).Close()

	f := testFormat()
	snk, err := NewOSPlaybackSink(f)
	if err != nil {
		if errors.Is(err, ErrNoAudioDevice) {
			t.Skipf("no render device on this host: %v", err)
		}
		t.Fatalf("NewOSPlaybackSink: %v", err)
	}
	sink := snk.(*pulsePlaybackSink)
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

// TestOSCaptureSource_liveMic_linux opens the real default microphone and reads
// real frames from it, logging the signal's RMS and peak so a human can see that
// genuine audio — not zero-filled silence — came back. Those levels are LOGGED
// rather than asserted on purpose: a silent room (or a muted mic) is a perfectly
// valid environment, and asserting non-silence would make this test flaky for a
// reason that has nothing to do with the code being correct.
func TestOSCaptureSource_liveMic_linux(t *testing.T) {
	requirePulse(t).Close()

	f := testFormat()
	src, err := NewOSCaptureSource(f)
	if err != nil {
		if errors.Is(err, ErrNoAudioDevice) {
			t.Skipf("no capture device on this host: %v", err)
		}
		t.Fatalf("NewOSCaptureSource: %v", err)
	}
	mic := src.(*pulseCaptureSource)
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
		t.Log("note: captured signal is pure digital silence — mic may be muted or the room silent; not a failure")
	}
}

// TestPulseCaptureToPlaybackRoundTrip is the strongest live proof for this
// backend, mirroring TestWASAPICaptureToPlaybackRoundTrip on Windows: it wires
// the real microphone into this package's actual Sender -> Transport ->
// Receiver -> Sink pipeline (the same jitter-buffered, gap-filling path
// production traffic uses) and confirms frames flow end to end, with the
// reconstructed audio going to the real speaker AND to a BufferSink the test can
// assert on. Both halves of the OS boundary are exercised together.
func TestPulseCaptureToPlaybackRoundTrip(t *testing.T) {
	requirePulse(t).Close()

	f := testFormat()
	src, err := NewOSCaptureSource(f)
	if err != nil {
		if errors.Is(err, ErrNoAudioDevice) {
			t.Skipf("no capture device on this host: %v", err)
		}
		t.Fatalf("NewOSCaptureSource: %v", err)
	}
	mic := src.(*pulseCaptureSource)
	defer mic.Close()

	speaker, err := NewOSPlaybackSink(f)
	if err != nil {
		if errors.Is(err, ErrNoAudioDevice) {
			t.Skipf("no render device on this host: %v", err)
		}
		t.Fatalf("NewOSPlaybackSink: %v", err)
	}
	spk := speaker.(*pulsePlaybackSink)
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
	for i, fr := range inspect.Frames {
		if len(fr.Samples) != f.SamplesPerPacket() {
			t.Fatalf("frame %d has %d samples, want %d", i, len(fr.Samples), f.SamplesPerPacket())
		}
	}
	t.Logf("live PulseAudio capture -> Sender -> jitter buffer -> Receiver -> real speaker: %d frames round-tripped (mic drops=%d, speaker underruns=%d)",
		len(inspect.Frames), mic.Dropped(), spk.Underruns())
}
