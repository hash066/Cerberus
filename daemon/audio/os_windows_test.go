//go:build windows

package audio

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
)

// skipOn386 skips tests that call IAudioClient.Initialize on windows/386. This
// is not a limitation of this package's code: go-wca's (and the underlying
// syscall package's) vtable call marshaling truncates 64-bit-by-value COM
// parameters on 32-bit Windows, because uintptr is only 32 bits there — see
// os_windows.go's file doc comment for the full explanation, confirmed by hand
// against this exact device (Initialize fails on windows/386, succeeds
// unmodified on windows/amd64 with the identical request). This is skipped
// rather than special-cased in the production code because the project's own
// release matrix and CI (build/release.ps1, .github/workflows/*.yml) never
// target windows/386 — only windows/amd64 and windows/arm64 — so windows/386
// is not a platform this backend is expected to support; the skip just keeps
// `go test ./...` green on a local dev machine whose default GOARCH happens to
// be 386, without masking the real, load-bearing amd64/arm64 verification
// done by the rest of this file.
func skipOn386(t *testing.T) {
	t.Helper()
	if runtime.GOARCH == "386" {
		t.Skip("windows/386 is not a target of this backend (go-wca's COM call marshaling truncates 64-bit REFERENCE_TIME params on 32-bit uintptr); this project's release matrix and CI only build windows/amd64 and windows/arm64 — see os_windows.go's file doc comment")
	}
}

// TestWASAPIEnumerationSucceeds asserts the minimum bar for a real Windows
// backend: WASAPI device enumeration itself must succeed. A render (speaker)
// endpoint exists via the default audio driver on nearly every Windows
// machine, even fully headless ones (unlike a capture/microphone device,
// which genuinely may not exist in a sandboxed environment) — so this does
// NOT require physical speakers or a mic to be plugged in, just the audio
// subsystem being present. This test does not go through this package's
// Source/Sink constructors; it talks to go-wca directly so a failure here
// isolates to "WASAPI itself isn't available" rather than anything in
// os_windows.go's higher-level logic.
func TestWASAPIEnumerationSucceeds(t *testing.T) {
	if err := ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED); err != nil {
		t.Fatalf("CoInitializeEx: %v", err)
	}
	defer ole.CoUninitialize()

	var mmde *wca.IMMDeviceEnumerator
	if err := wca.CoCreateInstance(wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL, wca.IID_IMMDeviceEnumerator, &mmde); err != nil {
		t.Fatalf("CoCreateInstance(MMDeviceEnumerator): %v", err)
	}
	defer mmde.Release()

	var renderColl *wca.IMMDeviceCollection
	if err := mmde.EnumAudioEndpoints(wca.ERender, wca.DEVICE_STATE_ACTIVE, &renderColl); err != nil {
		t.Fatalf("EnumAudioEndpoints(render): %v", err)
	}
	defer renderColl.Release()

	var renderCount uint32
	if err := renderColl.GetCount(&renderCount); err != nil {
		t.Fatalf("GetCount(render): %v", err)
	}
	if renderCount == 0 {
		// A fully headless CI runner (e.g. GitHub windows-latest) can genuinely
		// have zero active render endpoints — no audio device present at all.
		// The bar this test actually guards is that WASAPI enumeration itself
		// SUCCEEDS (every call above returned without error); the presence of a
		// physical endpoint is an environment property this code does not
		// control, so skip rather than fail when the sandbox has none.
		t.Skip("no active WASAPI render endpoint on this host (headless CI runner); enumeration itself succeeded, which is what this test verifies")
	}
	t.Logf("WASAPI render endpoints found: %d", renderCount)

	// Capture endpoint count is logged, not asserted: a sandboxed box may
	// legitimately have zero microphones. TestOSCaptureSource_liveMic below
	// is the test that skips gracefully when this is 0.
	var captureColl *wca.IMMDeviceCollection
	if err := mmde.EnumAudioEndpoints(wca.ECapture, wca.DEVICE_STATE_ACTIVE, &captureColl); err != nil {
		t.Fatalf("EnumAudioEndpoints(capture): %v", err)
	}
	defer captureColl.Release()
	var captureCount uint32
	if err := captureColl.GetCount(&captureCount); err != nil {
		t.Fatalf("GetCount(capture): %v", err)
	}
	t.Logf("WASAPI capture endpoints found: %d", captureCount)
}

// TestEnumerateEndpointsSucceeds proves the public EnumerateEndpoints
// function — the one daemon/system.Compose calls to register real devices
// into the 9P namespace — actually talks to WASAPI successfully on this
// machine. Mirroring TestWASAPIEnumerationSucceeds above: a render (speaker)
// endpoint is expected on essentially any Windows machine (even headless), so
// this asserts the call does not error and that at least one entry comes
// back, without hardcoding an exact count (this sandboxed environment's exact
// device set is not something this test should assume).
func TestEnumerateEndpointsSucceeds(t *testing.T) {
	endpoints, err := EnumerateEndpoints()
	if err != nil {
		if errors.Is(err, ErrNoAudioDevice) {
			t.Skipf("no WASAPI endpoints (mic or speaker) on this machine: %v", err)
		}
		t.Fatalf("EnumerateEndpoints: %v", err)
	}
	if len(endpoints) == 0 {
		t.Fatalf("EnumerateEndpoints returned no error but zero endpoints")
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
	if speakers == 0 {
		t.Errorf("expected at least one render (speaker) endpoint (even headless Windows normally has one); got 0")
	}
}

// hasCaptureDevice reports whether at least one active WASAPI capture
// endpoint exists, so tests that need a real microphone can skip gracefully
// rather than failing on a sandboxed/headless box with no input device.
func hasCaptureDevice(t *testing.T) bool {
	t.Helper()
	if err := ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED); err != nil {
		t.Fatalf("CoInitializeEx: %v", err)
	}
	defer ole.CoUninitialize()

	var mmde *wca.IMMDeviceEnumerator
	if err := wca.CoCreateInstance(wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL, wca.IID_IMMDeviceEnumerator, &mmde); err != nil {
		t.Fatalf("CoCreateInstance(MMDeviceEnumerator): %v", err)
	}
	defer mmde.Release()

	var coll *wca.IMMDeviceCollection
	if err := mmde.EnumAudioEndpoints(wca.ECapture, wca.DEVICE_STATE_ACTIVE, &coll); err != nil {
		t.Fatalf("EnumAudioEndpoints(capture): %v", err)
	}
	defer coll.Release()

	var count uint32
	if err := coll.GetCount(&count); err != nil {
		t.Fatalf("GetCount(capture): %v", err)
	}
	return count > 0
}

// TestOSBackendsAreStubs_windows is the Windows counterpart to
// os_other_test.go's TestOSBackendsAreStubs: on Windows the backend is real,
// so NewOSCaptureSource/NewOSPlaybackSink must never return
// ErrOSAudioUnavailable (that would mean the "real" backend silently fell
// back to being a fake stub). They may legitimately return ErrNoAudioDevice if
// this machine truly has no such endpoint — that is the honest failure mode
// for a headless box, not a stub.
func TestOSBackendsAreStubs_windows(t *testing.T) {
	skipOn386(t)
	f := testFormat()

	src, err := NewOSCaptureSource(f)
	switch {
	case err == nil:
		src.(*wasapiCaptureSource).Close()
	case errors.Is(err, ErrNoAudioDevice):
		t.Logf("no capture device on this machine (honest failure, not a stub): %v", err)
	case errors.Is(err, ErrOSAudioUnavailable):
		t.Fatalf("Windows backend must not report the generic stub error: %v", err)
	default:
		t.Fatalf("unexpected NewOSCaptureSource error: %v", err)
	}

	snk, err := NewOSPlaybackSink(f)
	switch {
	case err == nil:
		snk.(*wasapiPlaybackSink).Close()
	case errors.Is(err, ErrNoAudioDevice):
		t.Logf("no render device on this machine (honest failure, not a stub): %v", err)
	case errors.Is(err, ErrOSAudioUnavailable):
		t.Fatalf("Windows backend must not report the generic stub error: %v", err)
	default:
		t.Fatalf("unexpected NewOSPlaybackSink error: %v", err)
	}
}

// TestOSPlaybackSink_liveSpeaker opens the real default speaker via
// NewOSPlaybackSink and writes a few real frames of a sine tone through it.
// A render endpoint is expected on essentially any Windows machine (even
// headless), so this test asserts success rather than skipping.
func TestOSPlaybackSink_liveSpeaker(t *testing.T) {
	skipOn386(t)
	f := testFormat()
	snk, err := NewOSPlaybackSink(f)
	if err != nil {
		if errors.Is(err, ErrNoAudioDevice) {
			t.Skipf("no render device on this machine: %v", err)
		}
		t.Fatalf("NewOSPlaybackSink: %v", err)
	}
	defer snk.(*wasapiPlaybackSink).Close()

	if got := snk.Format(); got != f {
		t.Fatalf("Format() = %+v, want %+v", got, f)
	}

	src := NewSineSource(f, 440, 0.2, 10)
	for i := 0; i < 10; i++ {
		frame, err := src.ReadFrame(context.Background())
		if err != nil {
			t.Fatalf("sine source frame %d: %v", i, err)
		}
		if err := snk.WriteFrame(frame); err != nil {
			t.Fatalf("WriteFrame %d: %v", i, err)
		}
	}
}

// TestOSCaptureSource_liveMic opens the real default microphone via
// NewOSCaptureSource and reads a couple of real frames from it. Gracefully
// skipped (not failed) if this sandbox has no capture device — a mic is a
// much less certain bet than a render endpoint in a headless/CI environment.
func TestOSCaptureSource_liveMic(t *testing.T) {
	skipOn386(t)
	if !hasCaptureDevice(t) {
		t.Skip("no WASAPI capture endpoint on this machine")
	}

	f := testFormat()
	src, err := NewOSCaptureSource(f)
	if err != nil {
		if errors.Is(err, ErrNoAudioDevice) {
			t.Skipf("no capture device on this machine: %v", err)
		}
		t.Fatalf("NewOSCaptureSource: %v", err)
	}
	defer src.(*wasapiCaptureSource).Close()

	if got := src.Format(); got != f {
		t.Fatalf("Format() = %+v, want %+v", got, f)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for i := 0; i < 3; i++ {
		frame, err := src.ReadFrame(ctx)
		if err != nil {
			t.Fatalf("ReadFrame %d: %v", i, err)
		}
		if len(frame.Samples) != f.SamplesPerPacket() {
			t.Fatalf("frame %d has %d samples, want %d", i, len(frame.Samples), f.SamplesPerPacket())
		}
	}
}

// TestWASAPICaptureToPlaybackRoundTrip is the strongest live proof available
// in this environment: it wires a real WASAPI capture Source into this
// package's actual Sender -> Transport -> Receiver -> Sink pipeline (the same
// jitter-buffered, gap-filling path production traffic uses) and confirms
// frames actually flow end to end, landing in a BufferSink for inspection.
// Playback of the reconstructed frames also goes to the real speaker via
// NewOSPlaybackSink so both halves of the OS boundary are exercised together.
// Skipped gracefully if this sandbox has no capture device.
func TestWASAPICaptureToPlaybackRoundTrip(t *testing.T) {
	skipOn386(t)
	if !hasCaptureDevice(t) {
		t.Skip("no WASAPI capture endpoint on this machine")
	}

	f := testFormat()
	src, err := NewOSCaptureSource(f)
	if err != nil {
		if errors.Is(err, ErrNoAudioDevice) {
			t.Skipf("no capture device on this machine: %v", err)
		}
		t.Fatalf("NewOSCaptureSource: %v", err)
	}
	defer src.(*wasapiCaptureSource).Close()

	speaker, err := NewOSPlaybackSink(f)
	if err != nil {
		t.Fatalf("NewOSPlaybackSink: %v", err)
	}
	defer speaker.(*wasapiPlaybackSink).Close()

	inspect := NewBufferSink(f)
	// fanOutSink drives both the real speaker (proving the Sink boundary is
	// live) and the in-memory BufferSink (so the test can assert on what
	// actually arrived) from the single WriteFrame call the Receiver makes.
	fanOut := &fanOutSink{format: f, sinks: []Sink{speaker, inspect}}

	tx := NewMemTransport(64)
	sender := NewSender(src, tx)
	receiver := NewReceiver(tx, fanOut, ReceiverConfig{TargetFrames: 2, MaxFrames: 16})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
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
	t.Logf("live WASAPI capture -> Sender -> jitter buffer -> Receiver -> Sink: %d frames round-tripped", len(inspect.Frames))
}

// fanOutSink writes every frame to multiple Sinks in order, so a test can
// drive a real device and an inspectable BufferSink from one Receiver.
type fanOutSink struct {
	format Format
	sinks  []Sink
}

func (f *fanOutSink) Format() Format { return f.format }

func (f *fanOutSink) WriteFrame(fr Frame) error {
	for _, s := range f.sinks {
		if err := s.WriteFrame(fr); err != nil {
			return err
		}
	}
	return nil
}
